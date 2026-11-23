package srtout

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// proxy relays UDP datagrams between one client and a target on 127.0.0.1,
// with optional random loss (both directions), a byte rate limit on the
// target-to-client direction (a FIFO that delays datagrams and drops those
// that would wait longer than maxDelay) and blackholing.
type proxy struct {
	front *net.UDPConn // the client's side, on an ephemeral port
	back  *net.UDPConn // connected to the target

	client    atomic.Pointer[net.UDPAddr]
	loss      atomic.Uint64 // math.Float64bits of the loss probability
	blackhole atomic.Bool
	rate      atomic.Int64 // bytes per second; 0 means unlimited
	maxDelay  atomic.Int64 // nanoseconds

	queue chan timed // throttled datagrams, in departure order
	done  chan struct{}
	once  sync.Once
	wg    sync.WaitGroup

	mu   sync.Mutex
	next time.Time // the throttled link is busy until then
}

// timed is a datagram due at a time.
type timed struct {
	at   time.Time
	data []byte
}

// newProxy relays to target and closes with the test.
func newProxy(t *testing.T, target string) *proxy {
	t.Helper()
	ctx := context.Background()
	fc, err := new(net.ListenConfig).ListenPacket(ctx, "udp", "127.0.0.1:0")
	require.NoError(t, err)
	var d net.Dialer
	bc, err := d.DialContext(ctx, "udp", target)
	require.NoError(t, err)
	p := &proxy{
		front: fc.(*net.UDPConn), back: bc.(*net.UDPConn),
		queue: make(chan timed, 1<<16), done: make(chan struct{}),
	}
	p.wg.Add(3)
	go p.up()
	go p.down()
	go p.drain()
	t.Cleanup(p.Close)
	return p
}

// Addr is the address the client dials.
func (p *proxy) Addr() string { return p.front.LocalAddr().String() }

// SetLoss drops each datagram, in either direction, with probability v.
func (p *proxy) SetLoss(v float64) { p.loss.Store(math.Float64bits(v)) }

// SetBlackhole drops every datagram while on.
func (p *proxy) SetBlackhole(on bool) { p.blackhole.Store(on) }

// Throttle limits the target-to-client direction to rate bytes per second;
// a datagram that would wait longer than maxDelay is dropped.
func (p *proxy) Throttle(rate int64, maxDelay time.Duration) {
	p.maxDelay.Store(int64(maxDelay))
	p.rate.Store(rate)
}

// Close closes both sockets and waits for the relays: from then on nothing
// reaches either side.
func (p *proxy) Close() {
	p.once.Do(func() {
		close(p.done)
		_ = p.front.Close()
		_ = p.back.Close()
	})
	p.wg.Wait()
}

func (p *proxy) drop() bool {
	if p.blackhole.Load() {
		return true
	}
	v := math.Float64frombits(p.loss.Load())
	return v > 0 && rand.Float64() < v //nolint:gosec // test loss, not security
}

// up relays client datagrams to the target.
func (p *proxy) up() {
	defer p.wg.Done()
	buf := make([]byte, 2048)
	for {
		n, addr, err := p.front.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		p.client.Store(addr)
		if p.drop() {
			continue
		}
		_, _ = p.back.Write(buf[:n])
	}
}

// down relays target datagrams to the client, through the throttle when
// one is set.
func (p *proxy) down() {
	defer p.wg.Done()
	buf := make([]byte, 2048)
	for {
		n, err := p.back.Read(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue // ICMP unreachable before the target is up
		}
		if p.drop() {
			continue
		}
		rate := p.rate.Load()
		if rate <= 0 {
			p.send(buf[:n])
			continue
		}
		now := time.Now()
		p.mu.Lock()
		start := p.next
		if start.Before(now) {
			start = now
		}
		if start.Sub(now) > time.Duration(p.maxDelay.Load()) {
			p.mu.Unlock()
			continue // the queue is full: tail drop
		}
		p.next = start.Add(time.Duration(int64(n) * int64(time.Second) / rate))
		p.mu.Unlock()
		select {
		case p.queue <- timed{at: start, data: append([]byte(nil), buf[:n]...)}:
		default:
		}
	}
}

// drain sends throttled datagrams at their departure times.
func (p *proxy) drain() {
	defer p.wg.Done()
	for {
		select {
		case <-p.done:
			return
		case d := <-p.queue:
			if w := time.Until(d.at); w > 0 {
				tm := time.NewTimer(w)
				select {
				case <-p.done:
					tm.Stop()
					return
				case <-tm.C:
				}
			}
			p.send(d.data)
		}
	}
}

func (p *proxy) send(b []byte) {
	if c := p.client.Load(); c != nil {
		_, _ = p.front.WriteToUDP(b, c)
	}
}
