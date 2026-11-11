package live

import (
	"maps"
	"slices"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/mux/fmp4"
)

// Snapshot is a consistent, read-only view of a packager's window, taken by
// Packager.Snapshot. It is the contract between the packager and the HLS and
// DASH builders:
//
//   - Segments is the window, oldest first, at most Window entries. Seq
//     increases by one between neighbours.
//   - Segments[:Listed] are listed: Listed is the longest prefix whose every
//     segment satisfies IsListed at Now. Playlists and the MPD list exactly
//     these and nothing after them; segments past Listed are served by URL
//     but not advertised yet.
//   - Epochs holds every epoch that has a segment in the window, ascending.
//     Each epoch's Start and Wall were computed once, when its first segment
//     was packaged, and never change.
//   - Three discontinuity views, each with an eviction counter for the
//     playlist's DISCONTINUITY-SEQUENCE. SegmentInfo.Discontinuity is the
//     segmenter's flag (epoch change or lost frames); EvictedDiscontinuities
//     counts it on segments that left the window. SegmentInfo.TSDiscontinuity
//     is the HLS-TS flag: set on a segment with TS bytes, other than the
//     first one ever, when it is Discontinuity or a TS segment was dropped
//     (TS == nil) since the previous segment with TS bytes; it is always
//     false when TS is nil, and EvictedTSDiscontinuities counts it on
//     segments that left the window. CMAFPart.Discontinuity is the
//     per-track flag (see CMAFPart); EvictedTrackDiscontinuities counts it
//     per track (nil when none).
//   - HLS-TS numbering: a segment with TS == nil has no TS entry. Segments
//     with TS bytes carry TSSeq, which counts them from 0 with no holes, so
//     the TS playlist's MEDIA-SEQUENCE is the TSSeq of its first entry.
//
// Byte slices (SegmentInfo.TS, CMAFPart.Data, EpochTrack.Init) and configs
// are shared with the packager and must never be modified. Segments, Epochs
// and the map are the snapshot's own; SegmentInfo.Parts and
// EpochInfo.Tracks share immutable backing arrays clipped to their length,
// so appending to them never touches the packager's data.
type Snapshot struct {
	Target                      int64     // segment target, 90 kHz ticks
	Window                      int       // window length in segments
	Now                         time.Time // the packager's clock when the snapshot was taken
	AST                         time.Time // wall of the first anchor ever packaged; zero before the first segment
	Segments                    []SegmentInfo
	Listed                      int
	Epochs                      []EpochInfo
	EvictedDiscontinuities      uint64
	EvictedTSDiscontinuities    uint64
	EvictedTrackDiscontinuities map[int]uint64
}

// SegmentInfo is one packaged segment. The timing fields are the segmenter's
// (90 kHz stream time).
type SegmentInfo struct {
	Seq           uint64
	Epoch         uint32
	Discontinuity bool  // first segment after an epoch change or lost frames
	Start         int64 // PTS of the anchor track's first frame
	Duration      int64 // anchor track span, ticks
	End           int64 // end of the latest-ending frame over all tracks
	// TS is the muxed TS segment, served as hls/{Seq}.ts; nil when muxing
	// failed, and then the segment has no HLS-TS entry.
	TS []byte
	// TSSeq numbers the segments with TS bytes from 0, consecutively and
	// never reused: the HLS-TS media sequence number. Valid only when TS is
	// non-nil.
	TSSeq uint64
	// TSDiscontinuity is the HLS-TS discontinuity flag: TS is non-nil, this
	// is not the first segment with TS bytes, and the segment is
	// Discontinuity or follows dropped TS segments.
	TSDiscontinuity bool
	Parts           []CMAFPart // one per track that has frames and muxed cleanly, ascending Track
}

// CMAFPart is one track's CMAF media segment, served as cmaf/{Track}-{N}.m4s.
type CMAFPart struct {
	Track int
	// N numbers the track's parts from 0 with no holes, across epochs.
	N uint64
	// Discontinuity is set when the track has a previous part and, since
	// that part, any segment (inclusive of this one) was discontinuous or a
	// part of the track failed to mux. A track's first part never has it.
	Discontinuity bool
	Timing        fmp4.Timing // in the track's timescale (fmp4.Timescale)
	Data          []byte
}

// EpochInfo is one epoch's placement on the wallclock and its tracks.
type EpochInfo struct {
	Epoch  uint32
	Anchor media.Anchor  // the hub's anchor when the epoch's first segment was packaged
	Start  time.Duration // start_n: period start relative to AST, a whole number of milliseconds
	Wall   time.Time     // W_n = AST + Start, the effective epoch wall
	Tracks []EpochTrack  // tracks that had frames with a config in the epoch, ascending Track
}

// EpochTrack is one track's codec config and init segment in one epoch.
type EpochTrack struct {
	Track  int
	Config *media.TrackConfig // the track's first config in the epoch
	Init   []byte             // served as cmaf/init-{Track}-{Epoch}.mp4; nil until fmp4.Init succeeds
}

// WallAt returns the wallclock time of stream time pts in the epoch:
// W_n + (pts − Anchor.Start)/90000 s.
func (e EpochInfo) WallAt(pts int64) time.Time {
	return e.Wall.Add(media.ToDuration(pts - e.Anchor.Start))
}

// Track returns the epoch's entry for a track.
func (e EpochInfo) Track(track int) (EpochTrack, bool) {
	for _, t := range e.Tracks {
		if t.Track == track {
			return t, true
		}
	}
	return EpochTrack{}, false
}

// Epoch returns the snapshot's entry for an epoch.
func (s Snapshot) Epoch(id uint32) (EpochInfo, bool) {
	for _, e := range s.Epochs {
		if e.Epoch == id {
			return e, true
		}
	}
	return EpochInfo{}, false
}

// IsListed applies the single listing rule: a segment is listed once
// W_n + (End − anchor_n.Start)/90000 s ≤ Now, so no listed media of any
// track lies in the future.
func (s Snapshot) IsListed(seg SegmentInfo) bool {
	e, ok := s.Epoch(seg.Epoch)
	return ok && !e.WallAt(seg.End).After(s.Now)
}

// store is the shared state of a packager. The packager's writer mutates it
// under Packager.mu; readers copy it under the same lock. Values it hands
// out are never modified afterwards: a changed epoch is replaced whole.
type store struct {
	ast         time.Time
	segs        []SegmentInfo
	epochs      []EpochInfo // ascending Epoch, only epochs with a segment in segs
	evicted     uint64
	evictedTS   uint64
	evictedPart map[int]uint64
}

// add appends a segment and the current state of its epoch, then evicts
// segments beyond window and epochs left without a segment.
func (s *store) add(seg SegmentInfo, ep EpochInfo, window int) {
	s.segs = append(s.segs, seg)
	i, found := slices.BinarySearchFunc(s.epochs, ep.Epoch, func(e EpochInfo, id uint32) int {
		switch {
		case e.Epoch < id:
			return -1
		case e.Epoch > id:
			return 1
		default:
			return 0
		}
	})
	if found {
		s.epochs[i] = ep
	} else {
		s.epochs = slices.Insert(s.epochs, i, ep)
	}

	n := len(s.segs) - window
	if n <= 0 {
		return
	}
	for _, old := range s.segs[:n] {
		if old.Discontinuity {
			s.evicted++
		}
		if old.TSDiscontinuity {
			s.evictedTS++
		}
		for _, part := range old.Parts {
			if part.Discontinuity {
				if s.evictedPart == nil {
					s.evictedPart = map[int]uint64{}
				}
				s.evictedPart[part.Track]++
			}
		}
	}
	m := copy(s.segs, s.segs[n:])
	clear(s.segs[m:])
	s.segs = s.segs[:m]
	s.epochs = slices.DeleteFunc(s.epochs, func(e EpochInfo) bool {
		return !slices.ContainsFunc(s.segs, func(seg SegmentInfo) bool { return seg.Epoch == e.Epoch })
	})
}

// snapshot copies the store. Listed is left for the caller.
func (s *store) snapshot() Snapshot {
	return Snapshot{
		AST:                         s.ast,
		Segments:                    slices.Clone(s.segs),
		Epochs:                      slices.Clone(s.epochs),
		EvictedDiscontinuities:      s.evicted,
		EvictedTSDiscontinuities:    s.evictedTS,
		EvictedTrackDiscontinuities: maps.Clone(s.evictedPart),
	}
}

// segmentTS returns the TS bytes of a segment in the window.
func (s *store) segmentTS(seq uint64) []byte {
	for _, seg := range s.segs {
		if seg.Seq == seq {
			return seg.TS
		}
	}
	return nil
}

// part returns the bytes of a track's CMAF part in the window.
func (s *store) part(track int, n uint64) []byte {
	for _, seg := range s.segs {
		for _, p := range seg.Parts {
			if p.Track == track && p.N == n {
				return p.Data
			}
		}
	}
	return nil
}

// init returns the init segment of a track in an epoch in the window.
func (s *store) init(track int, epoch uint32) []byte {
	for _, e := range s.epochs {
		if e.Epoch == epoch {
			t, _ := e.Track(track)
			return t.Init
		}
	}
	return nil
}
