package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// errClosed is returned by Read after Close.
var errClosed = errors.New("ingest: file: closed")

// fileReader reads a TS file, once or in a loop. Each pass has its own
// demuxer and timestamp decoder, since a loop jumps back; the config cache
// is shared, so the passes hand out the same Config pointers.
type fileReader struct {
	path string
	loop bool
	spec Spec
	now  func() time.Time
	// open opens the file for a pass; nil means os.Open. Tests inject a
	// reader that blocks.
	open func(name string) (io.ReadCloser, error)

	cache  media.ConfigCache
	frames int // frames of the current pass; Read goroutine only

	mu     sync.Mutex // guards the fields below
	cur    *tsReader
	kinds  []media.Kind // of the latest pass, kept between passes
	closed bool
	done   map[string]uint64 // counters of finished passes
	passes uint64
}

func openFile(ctx context.Context, p parsed, env Env) (Reader, error) {
	r := &fileReader{path: p.path, loop: p.loop, spec: p.spec, now: env.now(), done: map[string]uint64{}}
	if err := r.openPass(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// openPass opens the file for a new pass.
func (r *fileReader) openPass(ctx context.Context) error {
	open := r.open
	if open == nil {
		open = func(name string) (io.ReadCloser, error) { return os.Open(name) }
	}
	f, err := open(r.path)
	if err != nil {
		return fmt.Errorf("ingest: file: open: %w", err)
	}
	cur, err := openTS(ctx, f, r.now, r.spec, &r.cache)
	if err != nil {
		return fmt.Errorf("ingest: file: open %s: %w", r.path, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		_ = cur.Close()
		return errClosed
	}
	r.cur = cur
	r.kinds = cur.Kinds()
	r.frames = 0
	r.passes++
	return nil
}

// Read implements Reader.
func (r *fileReader) Read() (Frame, error) {
	r.mu.Lock()
	cur, closed := r.cur, r.closed
	r.mu.Unlock()
	if closed {
		return Frame{}, errClosed
	}
	if cur == nil {
		if err := r.openPass(context.Background()); err != nil {
			return Frame{}, err
		}
		return r.Read()
	}
	f, err := cur.Read()
	switch {
	case err == nil:
		r.frames++
		return f, nil
	case errors.Is(err, ErrDiscontinuity):
		return Frame{}, err
	case errors.Is(err, io.EOF) && r.loop:
		return Frame{}, r.endPass(cur)
	case errors.Is(err, io.EOF):
		return Frame{}, io.EOF
	}
	r.mu.Lock()
	closed = r.closed
	r.mu.Unlock()
	if closed {
		return Frame{}, errClosed
	}
	return Frame{}, err
}

// endPass retires the finished pass of a looping file: ErrDiscontinuity so
// the next pass starts a new timeline, or an error when the pass held no
// frame, which would otherwise loop without end.
func (r *fileReader) endPass(cur *tsReader) error {
	r.mu.Lock()
	for k, v := range cur.Stats() {
		r.done[k] += v
	}
	r.cur = nil
	r.mu.Unlock()
	_ = cur.Close()
	if r.frames == 0 {
		return fmt.Errorf("ingest: file: loop %s: pass produced no frame", r.path)
	}
	return ErrDiscontinuity
}

// Kinds implements Reader.
func (r *fileReader) Kinds() []media.Kind {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur != nil {
		r.kinds = r.cur.Kinds()
	}
	return slices.Clone(r.kinds)
}

// Spec implements Reader.
func (r *fileReader) Spec() Spec { return r.spec }

// Stats implements Reader.
func (r *fileReader) Stats() map[string]uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]uint64, len(r.done)+1)
	for k, v := range r.done {
		out[k] = v
	}
	if r.cur != nil {
		for k, v := range r.cur.Stats() {
			out[k] += v
		}
	}
	out["passes"] = r.passes
	return out
}

// Close implements Reader.
func (r *fileReader) Close() error {
	r.mu.Lock()
	r.closed = true
	cur := r.cur
	r.mu.Unlock()
	if cur == nil {
		return nil
	}
	return cur.Close()
}
