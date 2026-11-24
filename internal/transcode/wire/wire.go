package wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// Version is the protocol version a Hello announces.
const Version = 1

// MaxMessage is the largest envelope, in bytes after the 4-byte length.
const MaxMessage = 32 << 20

// MaxRungs is the most rungs a Configure carries.
const MaxRungs = 64

// Errors returned by the Reader and Writer.
var (
	// ErrTooLarge means a message exceeds MaxMessage.
	ErrTooLarge = errors.New("wire: message too large")
	// ErrMalformed means the bytes are not a valid message.
	ErrMalformed = errors.New("wire: malformed message")
	// ErrLimit means the Writer refused a message that the Reader would
	// reject for one of its limits: too many NAL units, parameter sets,
	// rungs or stats entries, an integer outside the field's range, or more
	// than MaxMessage bytes (then also ErrTooLarge). Nothing was written;
	// the stream is still usable.
	ErrLimit = errors.New("wire: message exceeds a protocol limit")
	// ErrConfigTable means the Writer has sent as many distinct configs as
	// a Reader tracks, and refused a frame with a new one. Nothing was
	// written, but no frame with a new config can follow on this stream:
	// the peer must start over with a fresh Writer and Reader. It does not
	// wrap ErrLimit.
	ErrConfigTable = errors.New("wire: config table full")
)

// Limits that keep a hostile peer from forcing large allocations.
const (
	maxConfigs   = 1 << 16 // distinct configs one Writer or Reader tracks
	maxNALs      = 1 << 16
	maxParamSets = 256
	maxStatsLen  = 64
	maxInt       = math.MaxInt32
	readChunk    = 1 << 20 // body buffer pre-allocation cap before data arrives
)

// Envelope field numbers, one per message type.
const (
	fHello         = 1
	fConfigure     = 2
	fFrame         = 3
	fDiscontinuity = 4
	fStats         = 5
	fError         = 6
	fStop          = 7
)

// Message is one wire message: Hello, Configure, Frame, Discontinuity, Stats,
// Error or Stop. Read returns them by value.
type Message interface{ isMessage() }

// Hello is the first message, child to host.
type Hello struct {
	Version uint32
	PID     int
}

// Configure is sent once, host to child. A nil Audio means the child has no
// audio to encode.
type Configure struct {
	Rungs []Rung
	Audio *AudioEncode
}

// Rung is one video rendition to encode. BFrames -1 means none, 0 means the
// encoder default.
type Rung struct {
	Height, BitrateKbps, MaxBitrateKbps, BFrames int
	Preset                                       string
}

// AudioEncode asks for AAC re-encoding.
type AudioEncode struct{ BitrateKbps, SampleRate, Channels int }

// Frame carries one access unit. Host to child, Rung is -1. Child to host,
// Rung is the rung index, or -1 for audio.
type Frame struct {
	Rung int
	F    media.Frame
}

// Discontinuity tells the child to drain and resume at the next key frame,
// host to child.
type Discontinuity struct{}

// Stats is sent by the child every second. Every counter is cumulative
// over the child's life. Out has one entry per rung: the video frames
// written. EncodeNanos has one entry per rung: the wall time spent in the
// pipeline's calls, which encode the rungs one after another after a shared
// decode, so every rung reports the same total. The media counters mirror
// the native pipeline's: DroppedTracks (video tracks other than the first),
// Reanchors (forward audio re-anchors), AudioDropped (input audio samples
// dropped for backward drift), AudioCorrupt and VideoCorrupt (frames the
// decoders rejected and skipped).
type Stats struct {
	In          uint64
	Out         []uint64
	EncodeNanos []int64

	DroppedTracks, Reanchors, AudioDropped uint64
	AudioCorrupt, VideoCorrupt             uint64
}

// Error reports a child failure.
type Error struct {
	Rung    int
	Message string
	Fatal   bool
}

// Stop asks the child to flush and exit, host to child.
type Stop struct{}

func (Hello) isMessage()         {}
func (Configure) isMessage()     {}
func (Frame) isMessage()         {}
func (Discontinuity) isMessage() {}
func (Stats) isMessage()         {}
func (Error) isMessage()         {}
func (Stop) isMessage()          {}

// Writer encodes messages. It is safe for concurrent use; each Write is one
// io.Writer call.
type Writer struct {
	mu        sync.Mutex
	w         io.Writer
	configs   map[*media.TrackConfig]uint32
	configCap int // distinct configs this Writer may send; tests lower it
	buf       []byte
}

// NewWriter returns a Writer on w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w, configs: make(map[*media.TrackConfig]uint32), configCap: maxConfigs}
}

// Write encodes m. A Frame's config is included the first time this Writer
// sees its pointer, and referenced by ID afterwards. A message the Reader
// would reject for one of its limits is refused with an error wrapping
// ErrLimit, before anything is written.
func (w *Writer) Write(m Message) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	body := append(w.buf[:0], 0, 0, 0, 0) // reserve the length prefix
	var (
		newCfg *media.TrackConfig
		newID  uint32
		err    error
	)
	switch v := m.(type) {
	case Hello:
		if err = int32s("hello pid", v.PID); err == nil {
			body = appendEnvelope(body, fHello, appendHello(nil, v))
		}
	case Configure:
		var b []byte
		if b, err = appendConfigure(nil, v); err == nil {
			body = appendEnvelope(body, fConfigure, b)
		}
	case Frame:
		var b []byte
		b, newCfg, newID, err = w.appendFrame(nil, v)
		if err == nil {
			body = appendEnvelope(body, fFrame, b)
		}
	case Discontinuity:
		body = appendEnvelope(body, fDiscontinuity, nil)
	case Stats:
		if len(v.Out) > maxStatsLen || len(v.EncodeNanos) > maxStatsLen {
			err = limit("stats: %d out, %d encode_nanos entries, at most %d", len(v.Out), len(v.EncodeNanos), maxStatsLen)
		} else {
			body = appendEnvelope(body, fStats, appendStats(nil, v))
		}
	case Error:
		if err = int32s("error rung", v.Rung); err == nil {
			body = appendEnvelope(body, fError, appendError(nil, v))
		}
	case Stop:
		body = appendEnvelope(body, fStop, nil)
	default:
		err = fmt.Errorf("wire: write: unsupported message %T", m)
	}
	if err != nil {
		return err
	}
	n := len(body) - 4
	if n > MaxMessage {
		return fmt.Errorf("wire: write: %w: %w (%d bytes)", ErrLimit, ErrTooLarge, n)
	}
	binary.BigEndian.PutUint32(body, uint32(n))
	if cap(body) <= readChunk {
		w.buf = body[:0] // keep small buffers; let a huge frame's be collected
	}
	if _, err := w.w.Write(body); err != nil {
		return fmt.Errorf("wire: write: %w", err)
	}
	if newCfg != nil {
		w.configs[newCfg] = newID
	}
	return nil
}

func appendEnvelope(dst []byte, field protowire.Number, body []byte) []byte {
	dst = protowire.AppendTag(dst, field, protowire.BytesType)
	return protowire.AppendBytes(dst, body)
}

func appendUint(dst []byte, field protowire.Number, v uint64) []byte {
	dst = protowire.AppendTag(dst, field, protowire.VarintType)
	return protowire.AppendVarint(dst, v)
}

func appendSint(dst []byte, field protowire.Number, v int64) []byte {
	dst = protowire.AppendTag(dst, field, protowire.VarintType)
	return protowire.AppendVarint(dst, protowire.EncodeZigZag(v))
}

func appendBool(dst []byte, field protowire.Number, v bool) []byte {
	if !v {
		return dst
	}
	return appendUint(dst, field, 1)
}

func appendBytes(dst []byte, field protowire.Number, v []byte) []byte {
	dst = protowire.AppendTag(dst, field, protowire.BytesType)
	return protowire.AppendBytes(dst, v)
}

// Hello: 1 version (varint), 2 pid (sint64).
func appendHello(dst []byte, h Hello) []byte {
	dst = appendUint(dst, 1, uint64(h.Version))
	return appendSint(dst, 2, int64(h.PID))
}

// limit returns an error wrapping ErrLimit.
func limit(format string, args ...any) error {
	return fmt.Errorf("wire: write: %w: "+format, append([]any{ErrLimit}, args...)...)
}

// nonNeg checks values the Reader reads as non-negative int32s.
func nonNeg(name string, vs ...int) error {
	for _, v := range vs {
		if v < 0 || v > maxInt {
			return limit("%s %d out of range", name, v)
		}
	}
	return nil
}

// int32s checks values the Reader reads as int32s.
func int32s(name string, vs ...int) error {
	for _, v := range vs {
		if v < math.MinInt32 || v > maxInt {
			return limit("%s %d out of range", name, v)
		}
	}
	return nil
}

// Configure: 1 rung (repeated bytes), 2 audio (bytes, absent = no audio).
// Rung: 1 height, 2 bitrate_kbps, 3 max_bitrate_kbps (varint),
// 4 bframes (sint64), 5 preset (string).
// AudioEncode: 1 bitrate_kbps, 2 sample_rate, 3 channels (varint).
func appendConfigure(dst []byte, c Configure) ([]byte, error) {
	if len(c.Rungs) > MaxRungs {
		return nil, limit("%d rungs, at most %d", len(c.Rungs), MaxRungs)
	}
	for _, r := range c.Rungs {
		if err := nonNeg("rung", r.Height, r.BitrateKbps, r.MaxBitrateKbps); err != nil {
			return nil, err
		}
		if err := int32s("rung bframes", r.BFrames); err != nil {
			return nil, err
		}
		var b []byte
		b = appendUint(b, 1, uint64(r.Height))
		b = appendUint(b, 2, uint64(r.BitrateKbps))
		b = appendUint(b, 3, uint64(r.MaxBitrateKbps))
		b = appendSint(b, 4, int64(r.BFrames))
		b = appendBytes(b, 5, []byte(r.Preset))
		dst = appendBytes(dst, 1, b)
	}
	if a := c.Audio; a != nil {
		if err := nonNeg("audio", a.BitrateKbps, a.SampleRate, a.Channels); err != nil {
			return nil, err
		}
		var b []byte
		b = appendUint(b, 1, uint64(a.BitrateKbps))
		b = appendUint(b, 2, uint64(a.SampleRate))
		b = appendUint(b, 3, uint64(a.Channels))
		dst = appendBytes(dst, 2, b)
	}
	return dst, nil
}

// Frame: 1 rung (sint64), 2 track (varint), 3 epoch (varint), 4 pts (sint64),
// 5 dts (sint64), 6 key (bool), 7 rasl (bool), 8 data (repeated bytes),
// 9 config_id (varint, 0 = no config), 10 config (bytes, first use of an ID).
// Config: 1 codec (varint), 2 vps, 3 sps, 4 pps (repeated bytes), 5 asc (bytes).
func (w *Writer) appendFrame(dst []byte, v Frame) (b []byte, newCfg *media.TrackConfig, newID uint32, err error) {
	if err = nonNeg("track", v.F.Track); err != nil {
		return nil, nil, 0, err
	}
	if err = int32s("frame rung", v.Rung); err != nil {
		return nil, nil, 0, err
	}
	if len(v.F.Data) > maxNALs {
		return nil, nil, 0, limit("%d NAL units, at most %d", len(v.F.Data), maxNALs)
	}
	dst = appendSint(dst, 1, int64(v.Rung))
	dst = appendUint(dst, 2, uint64(v.F.Track))
	dst = appendUint(dst, 3, uint64(v.F.Epoch))
	dst = appendSint(dst, 4, v.F.PTS)
	dst = appendSint(dst, 5, v.F.DTS)
	dst = appendBool(dst, 6, v.F.Key)
	dst = appendBool(dst, 7, v.F.RASL)
	for _, d := range v.F.Data {
		dst = appendBytes(dst, 8, d)
	}
	if c := v.F.Config; c != nil {
		id, seen := w.configs[c]
		if !seen {
			if len(w.configs) >= w.configCap {
				return nil, nil, 0, fmt.Errorf("wire: write: %w: %d distinct configs sent", ErrConfigTable, len(w.configs))
			}
			if c.Codec.Kind() == 0 {
				return nil, nil, 0, fmt.Errorf("wire: write: config has unknown codec %d", c.Codec)
			}
			if n := max(len(c.VPS), len(c.SPS), len(c.PPS)); n > maxParamSets {
				return nil, nil, 0, limit("config: %d parameter sets of one kind, at most %d", n, maxParamSets)
			}
			id = uint32(len(w.configs)) + 1
			newCfg, newID = c, id
		}
		dst = appendUint(dst, 9, uint64(id))
		if !seen {
			dst = appendBytes(dst, 10, appendConfig(nil, c))
		}
	}
	return dst, newCfg, newID, nil
}

func appendConfig(dst []byte, c *media.TrackConfig) []byte {
	dst = appendUint(dst, 1, uint64(c.Codec))
	for _, s := range c.VPS {
		dst = appendBytes(dst, 2, s)
	}
	for _, s := range c.SPS {
		dst = appendBytes(dst, 3, s)
	}
	for _, s := range c.PPS {
		dst = appendBytes(dst, 4, s)
	}
	if len(c.ASC) > 0 {
		dst = appendBytes(dst, 5, c.ASC)
	}
	return dst
}

// Stats: 1 in (varint), 2 out (repeated varint, one per rung),
// 3 encode_nanos (repeated sint64, one per rung), then the media counters,
// each a varint written only when non-zero: 4 dropped_tracks, 5 reanchors,
// 6 audio_dropped, 7 audio_corrupt, 8 video_corrupt.
func appendStats(dst []byte, s Stats) []byte {
	dst = appendUint(dst, 1, s.In)
	for _, o := range s.Out {
		dst = appendUint(dst, 2, o)
	}
	for _, n := range s.EncodeNanos {
		dst = appendSint(dst, 3, n)
	}
	for i, v := range []uint64{s.DroppedTracks, s.Reanchors, s.AudioDropped, s.AudioCorrupt, s.VideoCorrupt} {
		if v != 0 {
			dst = appendUint(dst, protowire.Number(4+i), v)
		}
	}
	return dst
}

// Error: 1 rung (sint64), 2 message (string), 3 fatal (bool).
func appendError(dst []byte, e Error) []byte {
	dst = appendSint(dst, 1, int64(e.Rung))
	dst = appendBytes(dst, 2, []byte(e.Message))
	return appendBool(dst, 3, e.Fatal)
}

// Reader decodes messages. It is not safe for concurrent use. A Frame's Data
// shares one buffer per message and is never reused by the Reader, so the
// caller may keep it.
type Reader struct {
	r       io.Reader
	configs map[uint32]*media.TrackConfig
}

// NewReader returns a Reader on r.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: r, configs: make(map[uint32]*media.TrackConfig)}
}

// Read returns the next message, or io.EOF at a clean end. A stream cut inside
// a record returns an error wrapping io.ErrUnexpectedEOF.
func (r *Reader) Read() (Message, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r.r, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("wire: read: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxMessage {
		return nil, fmt.Errorf("wire: read: %w (%d bytes)", ErrTooLarge, n)
	}
	if n == 0 {
		return nil, fmt.Errorf("wire: read: %w: empty envelope", ErrMalformed)
	}
	// Grow with the data that actually arrives instead of trusting the length.
	var buf bytes.Buffer
	buf.Grow(int(min(n, readChunk)))
	if _, err := io.CopyN(&buf, r.r, int64(n)); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("wire: read: %w", err)
	}
	m, err := r.decode(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("wire: read: %w", err)
	}
	return m, nil
}

func bad(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrMalformed}, args...)...)
}

func (r *Reader) decode(b []byte) (Message, error) {
	num, typ, n := protowire.ConsumeTag(b)
	if n < 0 {
		return nil, bad("envelope tag")
	}
	if typ != protowire.BytesType {
		return nil, bad("envelope wire type %d", typ)
	}
	body, m := protowire.ConsumeBytes(b[n:])
	if m < 0 {
		return nil, bad("envelope body")
	}
	if n+m != len(b) {
		return nil, bad("trailing bytes after envelope")
	}
	switch int32(num) {
	case fHello:
		return decodeHello(body)
	case fConfigure:
		return decodeConfigure(body)
	case fFrame:
		return r.decodeFrame(body)
	case fDiscontinuity:
		return Discontinuity{}, nil
	case fStats:
		return decodeStats(body)
	case fError:
		return decodeError(body)
	case fStop:
		return Stop{}, nil
	default:
		return nil, bad("unknown message type %d", num)
	}
}

// field is one decoded field: its number and either a varint or bytes value.
type field struct {
	num int32
	v   uint64
	b   []byte
	isB bool
}

// fields calls fn for every varint or bytes field of b. Fixed-width and group
// fields are malformed.
func fields(b []byte, fn func(field) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return bad("field tag")
		}
		b = b[n:]
		switch int8(typ) {
		case int8(protowire.VarintType):
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return bad("varint")
			}
			b = b[n:]
			if err := fn(field{num: int32(num), v: v}); err != nil {
				return err
			}
		case int8(protowire.BytesType):
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return bad("bytes")
			}
			b = b[n:]
			if err := fn(field{num: int32(num), b: v, isB: true}); err != nil {
				return err
			}
		default:
			return bad("field %d wire type %d", num, typ)
		}
	}
	return nil
}

func (f field) uint() (uint64, error) {
	if f.isB {
		return 0, bad("field %d: want varint", f.num)
	}
	return f.v, nil
}

func (f field) sint() (int64, error) {
	v, err := f.uint()
	return protowire.DecodeZigZag(v), err
}

func (f field) bytes() ([]byte, error) {
	if !f.isB {
		return nil, bad("field %d: want bytes", f.num)
	}
	return f.b, nil
}

func (f field) boolean() (bool, error) {
	v, err := f.uint()
	if err == nil && v > 1 {
		err = bad("field %d: bool %d", f.num, v)
	}
	return v == 1, err
}

// small reads a varint that must fit a non-negative int32.
func (f field) small() (int, error) {
	v, err := f.uint()
	if err == nil && v > maxInt {
		err = bad("field %d: %d out of range", f.num, v)
	}
	return int(v), err
}

// smallSint reads a sint64 that must fit an int32.
func (f field) smallSint() (int, error) {
	v, err := f.sint()
	if err == nil && (v < math.MinInt32 || v > maxInt) {
		err = bad("field %d: %d out of range", f.num, v)
	}
	return int(v), err
}

func decodeHello(b []byte) (Message, error) {
	var h Hello
	err := fields(b, func(f field) error {
		var err error
		switch f.num {
		case 1:
			var v uint64
			if v, err = f.uint(); err == nil && v > math.MaxUint32 {
				err = bad("hello version %d", v)
			}
			h.Version = uint32(v)
		case 2:
			h.PID, err = f.smallSint()
		}
		return err
	})
	return h, err
}

func decodeConfigure(b []byte) (Message, error) {
	var c Configure
	err := fields(b, func(f field) error {
		switch f.num {
		case 1:
			if len(c.Rungs) >= MaxRungs {
				return bad("more than %d rungs", MaxRungs)
			}
			raw, err := f.bytes()
			if err != nil {
				return err
			}
			r, err := decodeRung(raw)
			c.Rungs = append(c.Rungs, r)
			return err
		case 2:
			raw, err := f.bytes()
			if err != nil {
				return err
			}
			a, err := decodeAudio(raw)
			c.Audio = &a
			return err
		}
		return nil
	})
	return c, err
}

func decodeRung(b []byte) (Rung, error) {
	var r Rung
	err := fields(b, func(f field) error {
		var err error
		switch f.num {
		case 1:
			r.Height, err = f.small()
		case 2:
			r.BitrateKbps, err = f.small()
		case 3:
			r.MaxBitrateKbps, err = f.small()
		case 4:
			r.BFrames, err = f.smallSint()
		case 5:
			var p []byte
			p, err = f.bytes()
			r.Preset = string(p)
		}
		return err
	})
	return r, err
}

func decodeAudio(b []byte) (AudioEncode, error) {
	var a AudioEncode
	err := fields(b, func(f field) error {
		var err error
		switch f.num {
		case 1:
			a.BitrateKbps, err = f.small()
		case 2:
			a.SampleRate, err = f.small()
		case 3:
			a.Channels, err = f.small()
		}
		return err
	})
	return a, err
}

func (r *Reader) decodeFrame(b []byte) (Message, error) {
	var (
		fr     Frame
		id     uint64
		cfgRaw []byte
		hasCfg bool
	)
	err := fields(b, func(f field) error {
		var err error
		switch f.num {
		case 1:
			fr.Rung, err = f.smallSint()
		case 2:
			fr.F.Track, err = f.small()
		case 3:
			var v uint64
			if v, err = f.uint(); err == nil && v > math.MaxUint32 {
				err = bad("epoch %d", v)
			}
			fr.F.Epoch = uint32(v)
		case 4:
			fr.F.PTS, err = f.sint()
		case 5:
			fr.F.DTS, err = f.sint()
		case 6:
			fr.F.Key, err = f.boolean()
		case 7:
			fr.F.RASL, err = f.boolean()
		case 8:
			if len(fr.F.Data) >= maxNALs {
				return bad("more than %d NAL units", maxNALs)
			}
			var d []byte
			if d, err = f.bytes(); err == nil {
				fr.F.Data = append(fr.F.Data, d)
			}
		case 9:
			id, err = f.uint()
		case 10:
			cfgRaw, err = f.bytes()
			hasCfg = err == nil
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	switch {
	case id == 0 && hasCfg:
		return nil, bad("config without id")
	case id == 0:
	case hasCfg:
		if id > maxConfigs || len(r.configs) >= maxConfigs {
			return nil, bad("config id %d", id)
		}
		if _, dup := r.configs[uint32(id)]; dup {
			return nil, bad("config id %d defined twice", id)
		}
		cfg, err := decodeConfig(cfgRaw)
		if err != nil {
			return nil, err
		}
		r.configs[uint32(id)] = cfg
		fr.F.Config = cfg
	default:
		cfg, ok := r.configs[uint32(id)]
		if !ok || id > maxConfigs {
			return nil, bad("unknown config id %d", id)
		}
		fr.F.Config = cfg
	}
	return fr, nil
}

func decodeConfig(b []byte) (*media.TrackConfig, error) {
	var (
		codec         uint64
		vps, sps, pps [][]byte
		asc           []byte
	)
	add := func(dst *[][]byte, f field) error {
		if len(*dst) >= maxParamSets {
			return bad("more than %d parameter sets", maxParamSets)
		}
		v, err := f.bytes()
		*dst = append(*dst, v)
		return err
	}
	err := fields(b, func(f field) error {
		var err error
		switch f.num {
		case 1:
			codec, err = f.uint()
		case 2:
			err = add(&vps, f)
		case 3:
			err = add(&sps, f)
		case 4:
			err = add(&pps, f)
		case 5:
			asc, err = f.bytes()
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	var cfg *media.TrackConfig
	switch {
	case codec > math.MaxUint8:
		err = errors.New("unknown codec")
	case media.Codec(codec) == media.CodecH264:
		cfg, err = media.NewH264Config(sps, pps)
	case media.Codec(codec) == media.CodecH265:
		cfg, err = media.NewH265Config(vps, sps, pps)
	case media.Codec(codec) == media.CodecAAC:
		cfg, err = media.NewAACConfig(asc)
	default:
		err = errors.New("unknown codec")
	}
	if err != nil {
		return nil, bad("config: %v", err)
	}
	return cfg, nil
}

func decodeStats(b []byte) (Message, error) {
	var s Stats
	err := fields(b, func(f field) error {
		var err error
		switch f.num {
		case 1:
			s.In, err = f.uint()
		case 2:
			if len(s.Out) >= maxStatsLen {
				return bad("more than %d stats", maxStatsLen)
			}
			var v uint64
			v, err = f.uint()
			s.Out = append(s.Out, v)
		case 3:
			if len(s.EncodeNanos) >= maxStatsLen {
				return bad("more than %d stats", maxStatsLen)
			}
			var v int64
			v, err = f.sint()
			s.EncodeNanos = append(s.EncodeNanos, v)
		case 4:
			s.DroppedTracks, err = f.uint()
		case 5:
			s.Reanchors, err = f.uint()
		case 6:
			s.AudioDropped, err = f.uint()
		case 7:
			s.AudioCorrupt, err = f.uint()
		case 8:
			s.VideoCorrupt, err = f.uint()
		}
		return err
	})
	return s, err
}

func decodeError(b []byte) (Message, error) {
	var e Error
	err := fields(b, func(f field) error {
		var err error
		switch f.num {
		case 1:
			e.Rung, err = f.smallSint()
		case 2:
			var p []byte
			p, err = f.bytes()
			e.Message = string(p)
		case 3:
			e.Fatal, err = f.boolean()
		}
		return err
	})
	return e, err
}
