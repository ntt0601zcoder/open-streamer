package conform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/datarhei/gosrt"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
)

const (
	// RuleTSStart names the rule that a TS stream read over SRT starts with
	// parameter sets or an IDR slice, not a delta frame.
	RuleTSStart = "ts-starts-on-key"
	// RuleTSContinuity names the rule that a TS stream read over SRT has no
	// continuity counter gap on a tracked PID.
	RuleTSContinuity = "ts-cc-continuous"
)

// srtBufferBytes bounds the received bytes waiting for the demuxer.
var srtBufferBytes = 16 << 20

// srtReadSize is the largest datagram payload read at once: gosrt returns one
// payload per Read, at most 1316 bytes by default.
const srtReadSize = 1500

var (
	// errReaderOverflow reports a demuxer that fell srtBufferBytes behind the
	// connection.
	errReaderOverflow = errors.New("reader overflow: the checker fell behind the stream")
	// errPumpStalled reports no data for the stall timeout.
	errPumpStalled = errors.New("no data")
)

// readSRT reads a TS stream from an SRT listener as a caller: the stream ID
// is the URL's streamid query, latency (milliseconds) an optional query. A
// goroutine copies the connection into a bounded buffer, so that demuxing
// never stalls SRT delivery.
func readSRT(ctx context.Context, pu *url.URL, opt ReadOptions) (Capture, error) {
	r := newReader(opt)
	u := pu.String()
	cfg := srt.DefaultConfig()
	q := pu.Query()
	cfg.StreamId = q.Get("streamid")
	if v := q.Get("latency"); v != "" {
		ms, err := strconv.Atoi(v)
		if err != nil || ms < 0 {
			return Capture{}, fmt.Errorf("conform: read: %s: latency %q is not a non-negative number of milliseconds", u, v)
		}
		cfg.Latency = time.Duration(ms) * time.Millisecond
	}
	if pu.Port() == "" || pu.Hostname() == "" {
		return Capture{}, fmt.Errorf("conform: read: %s: need host:port", u)
	}
	cfg.ConnectionTimeout = r.timeout
	conn, err := dialSRT(ctx, net.JoinHostPort(pu.Hostname(), pu.Port()), cfg)
	if err != nil {
		if ctx.Err() != nil {
			return Capture{}, fmt.Errorf("conform: read: %w", ctx.Err())
		}
		return Capture{}, fmt.Errorf("conform: read: %s: %w", u, err)
	}
	p := startPump(conn, srtBufferBytes)
	p.stall = r.timeout
	defer p.stop()
	stop := context.AfterFunc(ctx, p.stop)
	defer stop()
	fail := func(err error) error {
		if errors.Is(err, errPumpStalled) {
			return fmt.Errorf("conform: read: %s: stalled for %v: %w", u, r.timeout, err)
		}
		return fmt.Errorf("conform: read: %s: %w", u, err)
	}
	return checkTS(ctx, p, opt, true, fail)
}

// dialSRT connects to addr, returning when ctx ends if the handshake is
// still running; the connection it then makes is closed.
func dialSRT(ctx context.Context, addr string, cfg srt.Config) (srt.Conn, error) {
	type result struct {
		c   srt.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := srt.Dial("srt", addr, cfg)
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.err == nil {
				_ = r.c.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// pump copies src into a bounded queue in a goroutine, and is the io.Reader
// of what it received. A full queue ends the pump with errReaderOverflow:
// the copy never blocks, because a connection that is not drained loses
// packets.
type pump struct {
	src   io.Closer
	max   int
	stall time.Duration // 0: no stall timeout

	mu    sync.Mutex
	q     [][]byte
	bytes int
	err   error // set with done, final
	done  bool
	wake  chan struct{}
	quit  chan struct{}
	once  sync.Once
	exit  chan struct{}
}

func startPump(src interface {
	io.Reader
	io.Closer
}, maxBytes int,
) *pump {
	p := &pump{
		src: src, max: maxBytes,
		wake: make(chan struct{}, 1), quit: make(chan struct{}), exit: make(chan struct{}),
	}
	go p.run(src)
	return p
}

func (p *pump) run(src io.Reader) {
	defer close(p.exit)
	buf := make([]byte, srtReadSize)
	for {
		n, err := src.Read(buf)
		p.mu.Lock()
		if n > 0 {
			if p.bytes+n > p.max {
				p.finish(errReaderOverflow)
				p.mu.Unlock()
				_ = p.src.Close()
				return
			}
			p.q = append(p.q, append([]byte(nil), buf[:n]...))
			p.bytes += n
		}
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				err = io.EOF
			}
			p.finish(err)
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()
		p.signal()
	}
}

// finish ends the stream with err; mu is held.
func (p *pump) finish(err error) {
	if !p.done {
		p.done, p.err = true, err
	}
	p.signal()
}

func (p *pump) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// stop closes the source, ends Read and waits for the copy goroutine.
func (p *pump) stop() {
	p.once.Do(func() {
		close(p.quit)
		_ = p.src.Close()
	})
	<-p.exit
}

// Read returns queued bytes; once the source ended and the queue is empty,
// the source's error (io.EOF for a close). An overflow is returned at once,
// whatever is queued.
func (p *pump) Read(b []byte) (int, error) {
	var timer *time.Timer
	var timeout <-chan time.Time
	if p.stall > 0 {
		timer = time.NewTimer(p.stall)
		defer timer.Stop()
		timeout = timer.C
	}
	for {
		p.mu.Lock()
		switch {
		case p.done && errors.Is(p.err, errReaderOverflow):
			err := p.err
			p.mu.Unlock()
			return 0, err
		case len(p.q) > 0:
			n := copy(b, p.q[0])
			if n < len(p.q[0]) {
				p.q[0] = p.q[0][n:]
			} else {
				p.q = p.q[1:]
			}
			p.bytes -= n
			p.mu.Unlock()
			return n, nil
		case p.done:
			err := p.err
			p.mu.Unlock()
			return 0, err
		}
		p.mu.Unlock()
		select {
		case <-p.wake:
		case <-p.quit:
			return 0, net.ErrClosed
		case <-timeout:
			return 0, errPumpStalled
		}
	}
}

// sniffMax is how much of the start of a stream a sniffReader keeps: the
// first video PES packet is looked for in this window only, and a stream
// whose first video packet comes later is not checked for RuleTSStart.
const sniffMax = 1 << 20

// sniffReader keeps the first sniffMax bytes read through it.
type sniffReader struct {
	r   io.Reader
	buf []byte
}

func (s *sniffReader) Read(b []byte) (int, error) {
	n, err := s.r.Read(b)
	if room := sniffMax - len(s.buf); room > 0 {
		s.buf = append(s.buf, b[:min(n, room)]...)
	}
	return n, err
}

// firstVideoPID returns the PID of the first video track the demuxer found
// in the PMT.
func firstVideoPID(d *ts.Demuxer) (uint16, bool) {
	pids := d.TrackPIDs()
	for i, k := range d.TrackKinds() {
		if k == media.KindVideo && i < len(pids) {
			return pids[i], true
		}
	}
	return 0, false
}

// strictTS runs the checks of a stream that the demuxer, which recovers from
// them, does not report: continuity counter gaps, and a first video access
// unit that starts with a non-IDR slice (RuleTSStart). pid is the video PID
// of the first PMT.
func strictTS(d *ts.Demuxer, sniff *sniffReader, pid uint16, haveVideo bool, frames []media.Frame) []Violation {
	var out []Violation
	if cc := d.CCErrors(); cc > 0 {
		out = append(out, Violation{
			Rule: RuleTSContinuity, Index: -1,
			Detail: fmt.Sprintf("%d continuity counter gaps", cc),
		})
	}
	var codec media.Codec
	for _, f := range frames {
		if f.Kind() == media.KindVideo && f.Config != nil {
			codec = f.Config.Codec
			break
		}
	}
	if !haveVideo || codec == 0 {
		return out
	}
	if ok, known := videoStart(sniff.buf, pid, codec); known && !ok {
		out = append(out, Violation{
			Rule: RuleTSStart, Index: -1,
			Detail: "the first video access unit starts with a non-IDR slice, with no parameter sets or recovery point before it",
		})
	}
	return out
}

// videoStart reports whether the first video PES packet on pid found in data
// (TS packets from its start) begins at a random access point: its first
// NAL unit that is not a delimiter or SEI is a parameter set or an IDR slice,
// or a recovery point SEI comes first. known is false when the first TS
// packet of the PES does not reach such a NAL unit.
func videoStart(data []byte, pid uint16, codec media.Codec) (ok, known bool) {
	start := 0
	for start+mpegts.PacketSize < len(data) && data[start] != 0x47 {
		start++
	}
	for off := start; off+mpegts.PacketSize <= len(data); off += mpegts.PacketSize {
		p := data[off : off+mpegts.PacketSize]
		if p[0] != 0x47 || p[1]&0x40 == 0 || uint16(p[1]&0x1f)<<8|uint16(p[2]) != pid || p[3]&0x10 == 0 {
			continue
		}
		pl := p[4:]
		if p[3]&0x20 != 0 {
			if len(pl) == 0 || int(pl[0])+1 > len(pl) {
				return false, false
			}
			pl = pl[int(pl[0])+1:]
		}
		if len(pl) < 9 || pl[0] != 0 || pl[1] != 0 || pl[2] != 1 || len(pl) < 9+int(pl[8]) {
			return false, false
		}
		return nalsStartAtSync(pl[9+int(pl[8]):], codec)
	}
	return false, false
}

// nalsStartAtSync walks the Annex B NAL units of pl up to the first one that
// decides whether the access unit is a random access point.
func nalsStartAtSync(pl []byte, codec media.Codec) (ok, known bool) {
	h265 := codec == media.CodecH265
	for i := 0; i+3 < len(pl); i++ {
		if pl[i] != 0 || pl[i+1] != 0 || pl[i+2] != 1 {
			continue
		}
		hdr := pl[i+3:]
		typ, body := hdr[0]&0x1f, 1
		if h265 {
			typ, body = hdr[0]>>1&0x3f, 2
		}
		switch {
		case !h265 && typ == 9, h265 && typ == 35:
			// An access unit delimiter.
		case !h265 && typ == 6, h265 && (typ == 39 || typ == 40):
			// SEI: a recovery point makes the unit a random access point;
			// any other SEI says nothing about the slices after it.
			if len(hdr) > body && hdr[body] == 6 {
				return true, true
			}
		case !h265 && (typ == 5 || typ == 7 || typ == 8),
			h265 && (typ >= 16 && typ <= 21 || typ >= 32 && typ <= 34):
			return true, true
		default:
			return false, true
		}
		i += 2
	}
	return false, false
}
