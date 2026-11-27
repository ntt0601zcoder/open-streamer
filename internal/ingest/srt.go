package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/datarhei/gosrt"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// defaultSRTLatency is the SRT latency in milliseconds when the URL has none.
const defaultSRTLatency = 120

// srtConfig builds the gosrt config from the URL query.
func srtConfig(p parsed) (srt.Config, error) {
	q := p.u.Query()
	cfg := srt.DefaultConfig()
	lat := defaultSRTLatency
	if v := q.Get("latency"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return cfg, fmt.Errorf("invalid latency=%q", v)
		}
		lat = n
	}
	cfg.Latency = time.Duration(lat) * time.Millisecond
	cfg.StreamId = q.Get("streamid")
	cfg.Passphrase = q.Get("passphrase")
	return cfg, nil
}

func openSRT(ctx context.Context, rawURL string, p parsed, env Env) (Reader, error) {
	cfg, err := srtConfig(p)
	if err != nil {
		return nil, fmt.Errorf("ingest: srt: open %s: %w", Redact(rawURL), err)
	}
	addr := net.JoinHostPort(p.u.Hostname(), p.u.Port())
	if p.spec.Passive {
		return openSRTListener(ctx, rawURL, addr, cfg, p.spec, env)
	}
	return openSRTCaller(ctx, rawURL, addr, cfg, p.spec, env)
}

// srtErr wraps err with the redacted URL.
func srtErr(rawURL, op string, err error) error {
	return fmt.Errorf("ingest: srt: %s %s: %w", op, Redact(rawURL), err)
}

// srtCaller reads one SRT connection that we dialled.
type srtCaller struct {
	ts     *tsReader
	conn   srt.Conn
	rawURL string
}

func openSRTCaller(ctx context.Context, rawURL, addr string, cfg srt.Config, spec Spec, env Env) (Reader, error) {
	conn, err := srt.DialWithContext(ctx, "srt", addr, cfg)
	if err != nil {
		return nil, srtErr(rawURL, "dial", err)
	}
	r, err := openTS(ctx, conn, env.now(), spec, &media.ConfigCache{})
	if err != nil {
		return nil, srtErr(rawURL, "open", err)
	}
	return &srtCaller{ts: r, conn: conn, rawURL: rawURL}, nil
}

// Read implements Reader. A disconnect is an error, not io.EOF: a live pull
// never ends cleanly.
func (c *srtCaller) Read() (Frame, error) {
	f, err := c.ts.Read()
	if err == nil || errors.Is(err, ErrDiscontinuity) {
		return f, err
	}
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return Frame{}, srtErr(c.rawURL, "read", err)
}

// Kinds implements Reader.
func (c *srtCaller) Kinds() []media.Kind { return c.ts.Kinds() }

// Spec implements Reader.
func (c *srtCaller) Spec() Spec { return c.ts.Spec() }

// Stats implements Reader.
func (c *srtCaller) Stats() map[string]uint64 { return c.ts.Stats() }

// Close implements Reader.
func (c *srtCaller) Close() error {
	if err := c.ts.Close(); err != nil {
		return srtErr(c.rawURL, "close", err)
	}
	return nil
}

// srtListener accepts publishers for the reader's whole life, one at a time.
type srtListener struct {
	rawURL   string
	streamID string
	passcode string
	spec     Spec
	now      func() time.Time
	ln       srt.Listener
	cache    media.ConfigCache
	conns    chan srt.Conn // accepted publishers, at most one in flight
	done     chan struct{} // closed by Close
	acceptWG sync.WaitGroup
	closeMu  sync.Mutex // held for the whole of Close; taken before mu

	mu       sync.Mutex // guards the fields below
	busy     bool       // a publisher is accepted or being read
	closed   bool
	pending  srt.Conn // accepted, waiting for its PMT
	cur      *tsReader
	kinds    []media.Kind
	doneStat map[string]uint64 // counters of finished publishers
	disc     bool              // a new publisher took over: report ErrDiscontinuity
}

func openSRTListener(ctx context.Context, rawURL, addr string, cfg srt.Config, spec Spec, env Env) (Reader, error) {
	ln, err := srt.Listen("srt", addr, cfg)
	if err != nil {
		return nil, srtErr(rawURL, "listen", err)
	}
	l := &srtListener{
		rawURL: rawURL, streamID: cfg.StreamId, passcode: cfg.Passphrase, spec: spec, now: env.now(), ln: ln,
		conns: make(chan srt.Conn, 1), done: make(chan struct{}), doneStat: map[string]uint64{},
	}
	l.acceptWG.Add(1)
	go l.acceptLoop()

	// The first publisher and its PMT, bounded by ctx.
	stop := context.AfterFunc(ctx, func() { _ = l.Close() })
	cur, err := l.nextPublisher(ctx)
	if !stop() {
		if err == nil {
			err = ctx.Err()
		}
		_ = l.Close()
	}
	if err != nil {
		_ = l.Close()
		return nil, err
	}
	l.setCur(cur)
	l.mu.Lock()
	l.disc = false
	l.mu.Unlock()
	return l, nil
}

// acceptLoop answers connection requests until the listener closes.
func (l *srtListener) acceptLoop() {
	defer l.acceptWG.Done()
	for {
		req, err := l.ln.Accept2()
		if err != nil {
			return // closed, or the socket failed: Read sees done or waits for Close
		}
		l.mu.Lock()
		ok := !l.closed && !l.busy && (l.streamID == "" || req.StreamId() == l.streamID)
		if ok && l.passcode != "" && !req.IsEncrypted() {
			ok = false
		}
		if ok {
			l.busy = true
		}
		l.mu.Unlock()
		if !ok {
			req.Reject(srt.REJX_CONFLICT)
			continue
		}
		if req.IsEncrypted() {
			if err := req.SetPassphrase(l.passcode); err != nil {
				req.Reject(srt.REJ_BADSECRET)
				l.release()
				continue
			}
		}
		conn, err := req.Accept()
		if err != nil {
			l.release()
			continue
		}
		l.conns <- conn // busy was false, so the buffer is empty
	}
}

func (l *srtListener) release() {
	l.mu.Lock()
	l.busy = false
	l.mu.Unlock()
}

// nextPublisher waits for the next accepted publisher and reads up to its
// PMT. A publisher that fails before its PMT is dropped and the wait goes on.
func (l *srtListener) nextPublisher(ctx context.Context) (*tsReader, error) {
	for {
		var conn srt.Conn
		select {
		case conn = <-l.conns:
		case <-l.done:
			return nil, errSRTClosed
		case <-ctx.Done():
			return nil, srtErr(l.rawURL, "accept", ctx.Err())
		}
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			_ = conn.Close()
			return nil, errSRTClosed
		}
		l.pending = conn
		l.mu.Unlock()
		r, err := openTS(ctx, conn, l.now, l.spec, &l.cache)
		l.mu.Lock()
		l.pending = nil
		closed := l.closed
		l.mu.Unlock()
		if err == nil {
			return r, nil
		}
		l.release()
		if closed {
			return nil, errSRTClosed
		}
		if ctx.Err() != nil {
			return nil, srtErr(l.rawURL, "open", err)
		}
	}
}

// errSRTClosed is returned by Read after Close.
var errSRTClosed = errors.New("ingest: srt: closed")

// setCur makes cur the current publisher, with a discontinuity before its
// first frame. When Close ran since nextPublisher returned it, nobody else
// will close cur: setCur closes it and reports false.
func (l *srtListener) setCur(cur *tsReader) bool {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		_ = cur.Close()
		return false
	}
	l.cur = cur
	l.kinds = cur.Kinds()
	l.disc = true
	l.mu.Unlock()
	return true
}

// Read implements Reader.
func (l *srtListener) Read() (Frame, error) {
	for {
		l.mu.Lock()
		cur, closed, disc := l.cur, l.closed, l.disc
		if disc && cur != nil {
			l.disc = false
		}
		l.mu.Unlock()
		if closed {
			return Frame{}, errSRTClosed
		}
		if cur == nil {
			next, err := l.nextPublisher(context.Background())
			if err != nil {
				return Frame{}, err
			}
			if !l.setCur(next) {
				return Frame{}, errSRTClosed
			}
			continue
		}
		if disc {
			return Frame{}, ErrDiscontinuity
		}
		f, err := cur.Read()
		if err == nil || errors.Is(err, ErrDiscontinuity) {
			return f, err
		}
		l.mu.Lock()
		closed = l.closed
		l.mu.Unlock()
		if closed {
			return Frame{}, errSRTClosed
		}
		// The publisher is gone: retire it and wait for the next.
		l.retire(cur)
	}
}

func (l *srtListener) retire(cur *tsReader) {
	l.mu.Lock()
	for k, v := range cur.Stats() {
		l.doneStat[k] += v
	}
	l.cur = nil
	l.busy = false
	l.mu.Unlock()
	_ = cur.Close()
}

// Kinds implements Reader.
func (l *srtListener) Kinds() []media.Kind {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cur != nil {
		return l.cur.Kinds()
	}
	return slices.Clone(l.kinds)
}

// Spec implements Reader.
func (l *srtListener) Spec() Spec { return l.spec }

// Stats implements Reader.
func (l *srtListener) Stats() map[string]uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]uint64, len(l.doneStat))
	for k, v := range l.doneStat {
		out[k] = v
	}
	if l.cur != nil {
		for k, v := range l.cur.Stats() {
			out[k] += v
		}
	}
	return out
}

// Close implements Reader.
func (l *srtListener) Close() error {
	// A Close during another waits until the socket is closed.
	l.closeMu.Lock()
	defer l.closeMu.Unlock()
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	close(l.done)
	cur, pending := l.cur, l.pending
	l.mu.Unlock()
	l.ln.Close()
	if cur != nil {
		_ = cur.Close()
	}
	if pending != nil {
		_ = pending.Close()
	}
	l.acceptWG.Wait()
	// A publisher accepted but not yet claimed.
	select {
	case c := <-l.conns:
		_ = c.Close()
	default:
	}
	return nil
}
