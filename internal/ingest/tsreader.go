package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
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
	pmtChanges uint64
	pids       []uint16
	pending    *Frame // the frame that followed a discontinuity

	closeOnce sync.Once
	closeErr  error

	mu    sync.Mutex // guards the snapshots read by Stats and Kinds
	stats map[string]uint64
	kinds []media.Kind
}

// NewTSReader reads rc up to the first PMT and returns a Reader of its
// frames, which are in source time. It blocks until the first PMT; closing rc
// aborts it. The reader owns rc: Close closes it, and so does a failed
// NewTSReader. now stamps arrivals; nil means time.Now.
func NewTSReader(rc io.ReadCloser, now func() time.Time) (Reader, error) {
	r, err := newTSReader(rc, now, Spec{}, &media.ConfigCache{})
	if err != nil {
		return nil, err
	}
	return r, nil
}

func newTSReader(rc io.ReadCloser, now func() time.Time, spec Spec, cache *media.ConfigCache) (*tsReader, error) {
	if now == nil {
		now = time.Now
	}
	src := &stampReader{rc: rc, now: now, arrival: now()}
	dmx, err := ts.NewDemuxerWithOptions(src, ts.DemuxOptions{Remap: true})
	if err != nil {
		_ = rc.Close()
		return nil, fmt.Errorf("ingest: ts reader: %w", err)
	}
	r := &tsReader{rc: rc, src: src, dmx: dmx, spec: spec, cache: cache, pids: dmx.TrackPIDs()}
	r.snapshot()
	return r, nil
}

// openTS is newTSReader with ctx bounding the wait for the first PMT: when
// ctx ends first, rc is closed, which fails the open.
func openTS(ctx context.Context, rc io.ReadCloser, now func() time.Time, spec Spec, cache *media.ConfigCache) (*tsReader, error) {
	stop := context.AfterFunc(ctx, func() { _ = rc.Close() })
	r, err := newTSReader(rc, now, spec, cache)
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

// snapshot publishes the demuxer's counters and tracks for Stats and Kinds.
func (r *tsReader) snapshot() {
	s := r.dmx.Stats()
	m := map[string]uint64{
		"lost_frames":         s.LostFrames,
		"pmt_changes":         s.PMTChanges,
		"split_aus":           s.SplitAUs,
		"split_aus_reordered": s.SplitAUsReordered,
		"split_aus_no_room":   s.SplitAUsNoRoom,
		"continued":           s.Continued,
		"cc_errors":           r.dmx.CCErrors(),
		"decode_errors":       s.DecodeErrors,
		"dropped_no_config":   s.DroppedNoConfig,
	}
	kinds := r.dmx.TrackKinds()
	r.mu.Lock()
	r.stats = m
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
	mf, err := r.dmx.Read()
	r.snapshot()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return Frame{}, io.EOF
		}
		return Frame{}, fmt.Errorf("ingest: ts reader: read: %w", err)
	}
	changed := false
	if n := r.dmx.Stats().PMTChanges; n != r.pmtChanges {
		r.pmtChanges = n
		changed = r.remapped()
	}
	mf.Config = r.cache.Stable(mf.Track, mf.Config)
	f := Frame{Frame: mf, Arrival: r.src.arrival}
	if changed {
		r.pending = &f
		return Frame{}, ErrDiscontinuity
	}
	return f, nil
}

// remapped reports whether the last PMT change moved a track to another PID,
// and forgets the configs of the tracks it moved. A change that left every
// track in place is not a timeline break.
func (r *tsReader) remapped() bool {
	pids := r.dmx.TrackPIDs()
	moved := false
	for i, pid := range pids {
		if i >= len(r.pids) || r.pids[i] != pid {
			r.cache.Reset(i)
			moved = true
		}
	}
	r.pids = slices.Clone(pids)
	return moved
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
	return cloneStats(r.stats)
}

// Close implements Reader.
func (r *tsReader) Close() error {
	r.closeOnce.Do(func() { r.closeErr = r.rc.Close() })
	if r.closeErr != nil {
		return fmt.Errorf("ingest: ts reader: close: %w", r.closeErr)
	}
	return nil
}

func cloneStats(m map[string]uint64) map[string]uint64 {
	return maps.Clone(m)
}
