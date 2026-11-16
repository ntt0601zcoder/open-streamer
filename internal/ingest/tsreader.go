package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
)

// stampReader records when its last read returned data: the arrival of the
// bytes completing the frames that follow. It is used from the Read
// goroutine only.
type stampReader struct {
	rc      io.ReadCloser
	now     func() time.Time
	arrival time.Time
}

func (s *stampReader) Read(p []byte) (int, error) {
	n, err := s.rc.Read(p)
	if n > 0 {
		s.arrival = s.now()
	}
	return n, err //nolint:wrapcheck // io.Reader contract: errors pass through unwrapped.
}

// tsReader is a Reader over an MPEG-TS byte stream.
type tsReader struct {
	rc    io.ReadCloser
	src   *stampReader
	dmx   *ts.Demuxer
	spec  Spec
	cache *media.ConfigCache

	// Read goroutine only.
	gen     uint32 // PMT generation of the frames returned so far
	pending *Frame // the frame that followed a discontinuity

	closeOnce sync.Once
	closeErr  error

	mu    sync.Mutex // guards the snapshots read by Stats and Kinds
	stats ts.DemuxStats
	cc    uint64
	kinds []media.Kind
}

// NewTSReader reads rc up to the first PMT and returns a Reader of its
// frames, which are in source time. It blocks until the first PMT; closing rc
// aborts it. The reader owns rc: Close closes it, and so does a failed
// NewTSReader. now stamps arrivals; nil means time.Now.
func NewTSReader(rc io.ReadCloser, now func() time.Time) (Reader, error) {
	r, err := newTSReader(rc, now, Spec{}, &media.ConfigCache{}, nil)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// newTSReader is NewTSReader with a spec, a config cache shared across
// readers, and the timestamp decoder (nil: a private one, whose timeline
// starts at 0).
func newTSReader(rc io.ReadCloser, now func() time.Time, spec Spec, cache *media.ConfigCache, dec *mpegts.TimeDecoder) (*tsReader, error) {
	if now == nil {
		now = time.Now
	}
	src := &stampReader{rc: rc, now: now, arrival: now()}
	dmx, err := ts.NewDemuxerWithOptions(src, ts.DemuxOptions{Remap: true, Decoder: dec})
	if err != nil {
		_ = rc.Close()
		return nil, fmt.Errorf("ingest: ts reader: %w", err)
	}
	r := &tsReader{rc: rc, src: src, dmx: dmx, spec: spec, cache: cache}
	r.snapshot()
	r.setKinds()
	return r, nil
}

// openTS is newTSReader with ctx bounding the wait for the first PMT: when
// ctx ends first, rc is closed, which fails the open.
func openTS(ctx context.Context, rc io.ReadCloser, now func() time.Time, spec Spec, cache *media.ConfigCache) (*tsReader, error) {
	stop := context.AfterFunc(ctx, func() { _ = rc.Close() })
	r, err := newTSReader(rc, now, spec, cache, nil)
	if !stop() && err == nil {
		// ctx ended after the PMT but before stop: rc is closed.
		_ = rc.Close()
		return nil, fmt.Errorf("ingest: ts reader: %w", ctx.Err())
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %w", err, ctx.Err())
		}
		return nil, err
	}
	return r, nil
}

// snapshot publishes the demuxer's counters for Stats; it copies values and
// allocates nothing.
func (r *tsReader) snapshot() {
	s, cc := r.dmx.Stats(), r.dmx.CCErrors()
	r.mu.Lock()
	r.stats, r.cc = s, cc
	r.mu.Unlock()
}

// setKinds publishes the tracks for Kinds.
func (r *tsReader) setKinds() {
	kinds := r.dmx.TrackKinds()
	r.mu.Lock()
	r.kinds = kinds
	r.mu.Unlock()
}

// Read implements Reader.
func (r *tsReader) Read() (Frame, error) {
	if r.pending != nil {
		f := *r.pending
		r.pending = nil
		return f, nil
	}
	mf, gen, err := r.dmx.ReadGen()
	r.snapshot()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return Frame{}, io.EOF
		}
		return Frame{}, fmt.Errorf("ingest: ts reader: read: %w", err)
	}
	broke := false
	for ; r.gen < gen; r.gen++ {
		r.setKinds()
		if slots := r.dmx.TakeRemapped(r.gen + 1); len(slots) > 0 {
			broke = true
			for _, i := range slots {
				r.cache.Reset(i)
			}
		}
	}
	mf.Config = r.cache.Stable(mf.Track, mf.Config)
	f := Frame{Frame: mf, Arrival: r.src.arrival}
	if broke {
		r.pending = &f
		return Frame{}, ErrDiscontinuity
	}
	return f, nil
}

// Kinds implements Reader.
func (r *tsReader) Kinds() []media.Kind {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.kinds)
}

// Spec implements Reader.
func (r *tsReader) Spec() Spec { return r.spec }

// Stats implements Reader.
func (r *tsReader) Stats() map[string]uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stats
	return map[string]uint64{
		"lost_frames":         s.LostFrames,
		"pmt_changes":         s.PMTChanges,
		"split_aus":           s.SplitAUs,
		"split_aus_poc":       s.SplitAUsPOC,
		"split_aus_reordered": s.SplitAUsReordered,
		"split_aus_no_room":   s.SplitAUsNoRoom,
		"continued":           s.Continued,
		"cc_errors":           r.cc,
		"decode_errors":       s.DecodeErrors,
		"dropped_no_config":   s.DroppedNoConfig,
	}
}

// Close implements Reader.
func (r *tsReader) Close() error {
	r.closeOnce.Do(func() { r.closeErr = r.rc.Close() })
	if r.closeErr != nil {
		return fmt.Errorf("ingest: ts reader: close: %w", r.closeErr)
	}
	return nil
}
