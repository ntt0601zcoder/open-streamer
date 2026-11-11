package ts

import (
	"errors"
	"fmt"
	"io"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
)

const (
	// firstPID is the PID of track 0; track i is on firstPID + i.
	firstPID = 256
	// timeOffset is added to every stream time before it is written (1.4 s),
	// so that a decoder never sees a negative DTS at a stream start.
	timeOffset = 126000
	// pcrOffset is added to the DTS to get the PCR (0.7 s of decoder buffer).
	pcrOffset = 63000
	// tableInterval is the longest span without tables when the PCR PID is
	// audio.
	tableInterval = 9000
	ts33Mask      = 1<<33 - 1
)

var (
	// startCode precedes every NAL unit.
	startCode = []byte{0, 0, 0, 1}
	// audH264 and audH265 are access unit delimiters allowing any slice type.
	audH264 = []byte{0x09, 0xF0}
	audH265 = []byte{0x46, 0x01, 0x50}
)

// Muxer writes media frames as an MPEG-TS stream with one program. It is not
// safe for concurrent use.
type Muxer struct {
	w      *mpegts.Writer
	tracks []*media.TrackConfig
	pcrPID uint16

	// haveTables is set once tables were written for the current track set;
	// lastTables is the DTS of the PCR PID frame they preceded.
	haveTables bool
	lastTables int64
	// explicit is set by WriteTables: the next PCR PID frame gets no
	// automatic tables and its DTS becomes lastTables.
	explicit bool
	buf      []byte
}

// NewMuxer returns a Muxer writing to w. tracks is indexed by Frame.Track; a
// nil entry is a track that is not in the output.
func NewMuxer(w io.Writer, tracks []*media.TrackConfig) (*Muxer, error) {
	if w == nil {
		return nil, errors.New("ts: mux: nil writer")
	}
	m := &Muxer{w: mpegts.NewWriter(w)}
	if err := m.SetTracks(tracks); err != nil {
		return nil, err
	}
	return m, nil
}

// SetOutput redirects later output to w. Continuity counters continue.
func (m *Muxer) SetOutput(w io.Writer) { m.w.SetOutput(w) }

// SetTracks replaces the track list. Indexes present before and after keep
// their PID and continuity counter; a kept index whose codec changed gets the
// new stream type. The PCR PID is chosen again. Nothing changes on error.
func (m *Muxer) SetTracks(tracks []*media.TrackConfig) error {
	pmt, err := buildProgram(tracks)
	if err != nil {
		return err
	}
	if err := m.w.SetProgram(pmt); err != nil {
		return fmt.Errorf("ts: mux: %w", err)
	}
	m.tracks = append([]*media.TrackConfig(nil), tracks...)
	m.pcrPID = pmt.PCRPID
	m.haveTables, m.explicit = false, false
	return nil
}

func streamType(c media.Codec) (uint8, bool) {
	switch c {
	case media.CodecH264:
		return typeH264, true
	case media.CodecH265:
		return typeH265, true
	case media.CodecAAC:
		return typeAAC, true
	default:
		return 0, false
	}
}

func buildProgram(tracks []*media.TrackConfig) (mpegts.PMT, error) {
	var pmt mpegts.PMT
	for i, c := range tracks {
		if c == nil {
			continue
		}
		typ, ok := streamType(c.Codec)
		if !ok {
			return pmt, fmt.Errorf("ts: mux: track %d: unsupported codec %d", i, c.Codec)
		}
		pid := uint16(firstPID + i)
		pmt.Streams = append(pmt.Streams, mpegts.Stream{PID: pid, Type: typ})
		if pmt.PCRPID == 0 && c.Codec.Kind() == media.KindVideo {
			pmt.PCRPID = pid
		}
	}
	if len(pmt.Streams) == 0 {
		return pmt, errors.New("ts: mux: no tracks")
	}
	if pmt.PCRPID == 0 {
		pmt.PCRPID = pmt.Streams[0].PID
	}
	return pmt, nil
}

// WriteTables writes the PAT and PMT now. The next frame on the PCR PID gets
// no automatic tables.
func (m *Muxer) WriteTables() error {
	if err := m.w.WriteTables(); err != nil {
		return fmt.Errorf("ts: mux: %w", err)
	}
	m.explicit = true
	return nil
}

// WriteFrame writes f as one PES packet, preceded by tables where the PCR PID
// rules call for them. f.PTS and f.DTS must be stream times; the muxer adds
// the 1.4 s offset.
func (m *Muxer) WriteFrame(f media.Frame) error {
	if f.Track < 0 || f.Track >= len(m.tracks) || m.tracks[f.Track] == nil {
		return fmt.Errorf("ts: mux: track %d is not configured", f.Track)
	}
	tc := m.tracks[f.Track]
	if f.Config != nil && f.Config.Codec != tc.Codec {
		return fmt.Errorf("ts: mux: track %d is %s, frame is %s", f.Track, tc.Codec, f.Config.Codec)
	}
	var err error
	if tc.Codec.Kind() == media.KindVideo {
		err = m.buildVideo(tc.Codec, f)
	} else {
		err = m.buildAudio(tc, f)
	}
	if err != nil {
		return err
	}

	pid := uint16(firstPID + f.Track)
	opt := mpegts.PESOptions{
		PTS:          (f.PTS + timeOffset) & ts33Mask,
		DTS:          (f.DTS + timeOffset) & ts33Mask,
		HasDTS:       f.DTS != f.PTS,
		RandomAccess: f.Key && tc.Codec.Kind() == media.KindVideo,
		PCR:          -1,
		Video:        tc.Codec.Kind() == media.KindVideo,
	}
	if pid == m.pcrPID {
		opt.PCR = (f.DTS + pcrOffset) & ts33Mask
		if m.needTables(f, opt.Video) {
			if err := m.w.WriteTables(); err != nil {
				return fmt.Errorf("ts: mux: %w", err)
			}
			m.haveTables, m.lastTables = true, f.DTS
		}
	}
	if err := m.w.WritePES(pid, m.buf, opt); err != nil {
		return fmt.Errorf("ts: mux: %w", err)
	}
	return nil
}

// needTables applies the table rules to a frame on the PCR PID.
func (m *Muxer) needTables(f media.Frame, video bool) bool {
	if m.explicit {
		m.explicit, m.haveTables, m.lastTables = false, true, f.DTS
		return false
	}
	if video {
		return f.Key
	}
	return !m.haveTables || f.DTS-m.lastTables >= tableInterval
}

// buildVideo fills m.buf with the Annex-B access unit of f.
func (m *Muxer) buildVideo(codec media.Codec, f media.Frame) error {
	if len(f.Data) == 0 {
		return errors.New("ts: mux: video frame has no data")
	}
	b := append(m.buf[:0], startCode...)
	if codec == media.CodecH265 {
		b = append(b, audH265...)
	} else {
		b = append(b, audH264...)
	}
	if f.Key {
		if f.Config == nil {
			return errors.New("ts: mux: keyframe without config")
		}
		for _, sets := range [][][]byte{f.Config.VPS, f.Config.SPS, f.Config.PPS} {
			for _, n := range sets {
				b = append(append(b, startCode...), n...)
			}
		}
	}
	for _, n := range f.Data {
		b = append(append(b, startCode...), n...)
	}
	m.buf = b
	return nil
}

// buildAudio fills m.buf with one ADTS frame.
func (m *Muxer) buildAudio(tc *media.TrackConfig, f media.Frame) error {
	if len(f.Data) != 1 {
		return fmt.Errorf("ts: mux: audio frame has %d data elements, want 1", len(f.Data))
	}
	cfg := tc
	if f.Config != nil {
		cfg = f.Config
	}
	b, err := mpegts.AppendADTS(m.buf[:0], cfg.ASC, f.Data[0])
	if err != nil {
		return fmt.Errorf("ts: mux: %w", err)
	}
	m.buf = b
	return nil
}
