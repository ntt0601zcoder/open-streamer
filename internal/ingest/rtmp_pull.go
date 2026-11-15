package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
)

// rtmpPull is an RTMP pull input: it plays one stream from a server, one
// session for the reader's life. A live pull never ends cleanly: when the
// server ends the stream (Play.Stop, UnpublishNotify, StreamEOF or a
// disconnect), Read returns the held video frames (flvFrames.flush), then
// io.ErrUnexpectedEOF, and the pipeline retries or fails over.
//
// Only the goroutine calling Read (Open's before it returns) reads the
// connection and pushes into flvFrames; Kinds, Stats and Close are safe
// concurrently with it.
type rtmpPull struct {
	rawURL string
	spec   Spec
	conn   *rtmp.ClientConn
	flv    *flvFrames

	// The reading goroutine's own state.
	queue frameQueue // ready for Read
	err   error      // the end of the stream, returned once the queue is empty

	closed atomic.Bool
}

func openRTMPPull(ctx context.Context, rawURL string, p parsed, env Env) (Reader, error) {
	// The client strips our mode parameter from the query it sends.
	conn, err := rtmp.Play(ctx, rawURL)
	if err != nil {
		return nil, fmt.Errorf("ingest: rtmp: open %s: %w", Redact(rawURL), err)
	}
	r := &rtmpPull{rawURL: rawURL, spec: p.spec, conn: conn, flv: newFLVFrames(&media.ConfigCache{}, env.now())}

	// The tracks, bounded by ctx; the frames read meanwhile stay queued.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	for r.err == nil && !tracksKnown(r.flv, &r.queue) {
		r.step()
	}
	err = r.err
	if err != nil {
		err = fmt.Errorf("the stream ended before its tracks were known: %w", err)
	}
	if !stop() {
		err = ctx.Err()
	}
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ingest: rtmp: open %s: %w", Redact(rawURL), err)
	}
	return r, nil
}

// step reads one message and queues the frames it completes. At the end of
// the stream it queues the held video frames and records the end: a clean
// end becomes io.ErrUnexpectedEOF.
func (r *rtmpPull) step() {
	m, err := r.conn.ReadMedia()
	if err != nil {
		r.queue.push(r.flv.flush())
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		r.err = err
		return
	}
	// A bad tag is counted by flvFrames and skipped; the session goes on.
	frames, _ := r.flv.push(m)
	r.queue.push(frames)
}

// Read implements Reader.
func (r *rtmpPull) Read() (Frame, error) {
	for {
		if r.closed.Load() {
			return Frame{}, errRTMPClosed
		}
		if f, ok := r.queue.pop(); ok {
			return f, nil
		}
		if r.err != nil {
			return Frame{}, fmt.Errorf("ingest: rtmp: read %s: %w", Redact(r.rawURL), r.err)
		}
		r.step()
	}
}

// Kinds implements Reader: video on track 0, audio on track 1, for the
// kinds the stream announced (audio only is [0, audio]). It grows when a
// kind is announced after Open, but the pipeline fixes its track layout
// from Kinds when the input opens: frames of such a kind are dropped there.
func (r *rtmpPull) Kinds() []media.Kind {
	return rtmpKinds(r.flv.announced())
}

// Spec implements Reader.
func (r *rtmpPull) Spec() Spec { return r.spec }

// Stats implements Reader: the flvFrames counters and the server commands
// that did not decode (bad_commands).
func (r *rtmpPull) Stats() map[string]uint64 {
	out := r.flv.stats()
	out["bad_commands"] = r.conn.BadCommands()
	return out
}

// Close implements Reader. It closes the connection, unblocking a pending
// Read.
func (r *rtmpPull) Close() error {
	if r.closed.Swap(true) {
		return nil
	}
	_ = r.conn.Close()
	return nil
}
