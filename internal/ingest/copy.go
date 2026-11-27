package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// copyQueue is the subscriber queue of a copy input.
const copyQueue = 1024

var errCopyClosed = errors.New("reader closed")

// copyReader reads another stream's hub in process. Frames keep the source
// stream's time; a loss on the subscription or a change of the source's epoch
// is a discontinuity. Read is called from one goroutine; Kinds, Stats and
// Close are safe concurrently.
type copyReader struct {
	sub    *hub.Sub
	rawURL string
	now    func() time.Time
	done   chan struct{}
	once   sync.Once

	// Read goroutine only.
	buffered  []hub.Delivery // deliveries taken while opening
	held      *Frame         // the frame a discontinuity was reported before
	haveEpoch bool
	epoch     uint32

	mu    sync.Mutex // guards kinds
	kinds []media.Kind

	frames        atomic.Uint64
	discontinuity atomic.Uint64
	lostFrames    atomic.Uint64
}

func copyErr(rawURL, op string, err error) error {
	return fmt.Errorf("ingest: copy: %s %s: %w", op, Redact(rawURL), err)
}

func openCopy(ctx context.Context, rawURL string, p parsed, env Env) (Reader, error) {
	if env.Lookup == nil {
		return nil, copyErr(rawURL, "lookup", errors.New("copy inputs are not available"))
	}
	name := p.u.Host
	h, ok := env.Lookup(name)
	if !ok || h == nil {
		return nil, copyErr(rawURL, "lookup", fmt.Errorf("unknown stream %q", name))
	}
	r := &copyReader{
		sub:    h.Subscribe("copy:"+rawURL, copyQueue),
		rawURL: rawURL,
		now:    env.now(),
		done:   make(chan struct{}),
	}
	if err := r.await(ctx); err != nil {
		_ = r.Close()
		return nil, copyErr(rawURL, "open", err)
	}
	return r, nil
}

// await takes deliveries until the first video keyframe (or the first frame
// of a stream without video, which the hub delivers first), then the rest of
// the replay burst already queued, so Kinds covers the tracks of the burst.
func (r *copyReader) await(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for a keyframe: %w", ctx.Err())
		case d, ok := <-r.sub.C():
			if !ok {
				return fmt.Errorf("waiting for a keyframe: %w", io.ErrUnexpectedEOF)
			}
			r.take(d)
			if d.Frame.Kind() != media.KindVideo || d.Frame.Key {
				r.drain()
				return nil
			}
		}
	}
}

// drain buffers the deliveries already queued, without blocking.
func (r *copyReader) drain() {
	for {
		select {
		case d, ok := <-r.sub.C():
			if !ok {
				return
			}
			r.take(d)
		default:
			return
		}
	}
}

// take buffers d and records its track.
func (r *copyReader) take(d hub.Delivery) {
	r.buffered = append(r.buffered, d)
	r.noteTrack(d.Frame)
}

func (r *copyReader) noteTrack(f media.Frame) {
	if f.Track < 0 || f.Config == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.kinds) <= f.Track {
		r.kinds = append(r.kinds, 0)
	}
	r.kinds[f.Track] = f.Kind()
}

// next returns the next delivery, buffered ones first.
func (r *copyReader) next() (hub.Delivery, error) {
	if len(r.buffered) > 0 {
		d := r.buffered[0]
		r.buffered[0] = hub.Delivery{}
		r.buffered = r.buffered[1:]
		return d, nil
	}
	select {
	case <-r.done:
		return hub.Delivery{}, copyErr(r.rawURL, "read", errCopyClosed)
	case d, ok := <-r.sub.C():
		if !ok {
			select {
			case <-r.done:
				return hub.Delivery{}, copyErr(r.rawURL, "read", errCopyClosed)
			default:
			}
			return hub.Delivery{}, io.EOF
		}
		return d, nil
	}
}

// Read implements Reader.
func (r *copyReader) Read() (Frame, error) {
	if f := r.held; f != nil {
		r.held = nil
		return *f, nil
	}
	d, err := r.next()
	if err != nil {
		return Frame{}, err
	}
	r.noteTrack(d.Frame)
	broken := d.Lost > 0 || (r.haveEpoch && d.Frame.Epoch != r.epoch)
	r.haveEpoch, r.epoch = true, d.Frame.Epoch
	r.lostFrames.Add(d.Lost)
	d.Frame.Epoch = 0
	f := Frame{Frame: d.Frame, Arrival: r.now()}
	r.frames.Add(1)
	if broken {
		r.discontinuity.Add(1)
		r.held = &f
		return Frame{}, ErrDiscontinuity
	}
	return f, nil
}

// Kinds implements Reader.
func (r *copyReader) Kinds() []media.Kind {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]media.Kind(nil), r.kinds...)
}

// Spec implements Reader.
func (r *copyReader) Spec() Spec { return Spec{Live: true} }

// Stats implements Reader.
func (r *copyReader) Stats() map[string]uint64 {
	return map[string]uint64{
		"frames":        r.frames.Load(),
		"discontinuity": r.discontinuity.Load(),
		"lost_frames":   r.lostFrames.Load(),
	}
}

// Close implements Reader.
func (r *copyReader) Close() error {
	r.once.Do(func() {
		close(r.done)
		r.sub.Close()
	})
	return nil
}
