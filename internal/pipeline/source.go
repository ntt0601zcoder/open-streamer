// Package pipeline wires a media source into the media core.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
)

// Options configures Run.
type Options struct {
	Open  func() (io.ReadCloser, error)                    // opened again for each loop
	Loop  bool                                             // reopen the source at EOF
	Pace  bool                                             // release frames at real time
	Now   func() time.Time                                 // nil means time.Now; also passed to the Clock
	Sleep func(ctx context.Context, d time.Duration) error // nil means a timer that honours ctx
}

// writer feeds clock output to the hub, recording each epoch's anchor before
// its first frame and pacing frames against that anchor. The anchor's Wall is
// the Clock's effective wall, the same wall the packager advertises, so
// pacing waits out each epoch's presentation-end excess and the walls never
// run ahead of the frames (see clock.Epoch).
type writer struct {
	h        *hub.Hub
	c        *clock.Clock
	opt      Options
	anchored uint32 // largest epoch whose anchor was set
	anchors  map[uint32]media.Anchor
}

func (w *writer) write(ctx context.Context, frames []media.Frame) error {
	for _, f := range frames {
		if f.Epoch > w.anchored {
			for _, e := range w.c.Epochs() {
				if e.ID > w.anchored {
					a := media.Anchor{Epoch: e.ID, Start: e.Start, Wall: e.Wall}
					w.h.SetAnchor(a)
					w.anchors[e.ID] = a
					w.anchored = e.ID
				}
			}
		}
		for id := range w.anchors {
			if id < f.Epoch {
				delete(w.anchors, id) // pacing needs only the current epoch
			}
		}
		if w.opt.Pace {
			if a, ok := w.anchors[f.Epoch]; ok {
				due := a.Wall.Add(media.ToDuration(f.DTS - a.Start))
				if d := due.Sub(w.opt.Now()); d > 0 {
					if err := w.opt.Sleep(ctx, d); err != nil {
						return err
					}
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		w.h.Write(f)
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run reads the source into h until EOF (nil), an error, or ctx is done
// (ctx.Err()). It runs in the caller's goroutine.
func Run(ctx context.Context, h *hub.Hub, opt Options) error {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Sleep == nil {
		opt.Sleep = sleep
	}
	w := &writer{h: h, opt: opt, anchors: map[uint32]media.Anchor{}}
	for first := true; ; first = false {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !first {
			w.c.Break()
		}
		done, err := w.runOnce(ctx)
		if err != nil || done {
			return err
		}
	}
}

// runOnce plays one pass over the source. It reports done at EOF without Loop.
func (w *writer) runOnce(ctx context.Context) (done bool, err error) {
	rc, err := w.opt.Open()
	if err != nil {
		return true, w.finish(ctx, fmt.Errorf("pipeline: open: %w", err))
	}
	defer func() { _ = rc.Close() }()
	// A blocked Read never sees ctx, so closing the source on cancel unblocks it.
	stop := context.AfterFunc(ctx, func() { _ = rc.Close() })
	defer stop()
	d, err := ts.NewDemuxer(rc)
	if err != nil {
		return true, w.finish(ctx, fmt.Errorf("pipeline: demux: %w", err))
	}
	if w.c == nil {
		w.c = clock.New(clock.Options{Kinds: d.TrackKinds(), Now: w.opt.Now})
	}
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return true, w.finish(ctx, fmt.Errorf("pipeline: read: %w", err))
		}
		if err := w.write(ctx, w.c.Push(f)); err != nil {
			return true, err
		}
	}
	if err := w.write(ctx, w.c.Flush()); err != nil {
		return true, err
	}
	return !w.opt.Loop, nil
}

// finish flushes what the clock holds, then returns cause. A cancelled
// context wins over cause. Before the first source opened there is no clock.
func (w *writer) finish(ctx context.Context, cause error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.c != nil {
		if err := w.write(ctx, w.c.Flush()); err != nil {
			return err
		}
	}
	return cause
}
