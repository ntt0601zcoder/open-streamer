package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"io"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// Parameter sets from internal/media's fixtures.
const (
	h264SPSHex = "674d400deca0a0fd80880000030008000003019078a14cb0"
	h264PPSHex = "68ebe3cb20"
	h265VPSHex = "40010c01ffff01600000030090000003000003003c959009"
	h265SPSHex = "42010101600000030090000003000003003ca00a080f16595924932bc05a020000030002000003003210"
	h265PPSHex = "4401c172b46240"
)

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err)
	return b
}

func h264Config(t testing.TB) *media.TrackConfig {
	t.Helper()
	c, err := media.NewH264Config([][]byte{unhex(t, h264SPSHex)}, [][]byte{unhex(t, h264PPSHex)})
	require.NoError(t, err)
	return c
}

func h265Config(t testing.TB) *media.TrackConfig {
	t.Helper()
	c, err := media.NewH265Config([][]byte{unhex(t, h265VPSHex)}, [][]byte{unhex(t, h265SPSHex)}, [][]byte{unhex(t, h265PPSHex)})
	require.NoError(t, err)
	return c
}

func aacConfig(t testing.TB) *media.TrackConfig {
	t.Helper()
	c, err := media.NewAACConfig([]byte{0x11, 0x90})
	require.NoError(t, err)
	return c
}

func roundTrip(t *testing.T, msgs ...Message) []Message {
	t.Helper()
	var buf bytes.Buffer
	w := NewWriter(&buf)
	for _, m := range msgs {
		require.NoError(t, w.Write(m))
	}
	r := NewReader(&buf)
	out := make([]Message, 0, len(msgs))
	for range msgs {
		m, err := r.Read()
		require.NoError(t, err)
		out = append(out, m)
	}
	_, err := r.Read()
	require.ErrorIs(t, err, io.EOF)
	return out
}

func assertFrameEqual(t *testing.T, want, got Frame) {
	t.Helper()
	assert.Equal(t, want.Rung, got.Rung)
	assert.Equal(t, want.F.Track, got.F.Track)
	assert.Equal(t, want.F.Epoch, got.F.Epoch)
	assert.Equal(t, want.F.PTS, got.F.PTS)
	assert.Equal(t, want.F.DTS, got.F.DTS)
	assert.Equal(t, want.F.Key, got.F.Key)
	assert.Equal(t, want.F.RASL, got.F.RASL)
	require.Len(t, got.F.Data, len(want.F.Data))
	for i := range want.F.Data {
		assert.True(t, bytes.Equal(want.F.Data[i], got.F.Data[i]), "data[%d]", i)
	}
	assert.True(t, want.F.Config.Equal(got.F.Config), "config")
}

func TestRoundTripEveryMessage(t *testing.T) {
	h264, h265, aac := h264Config(t), h265Config(t), aacConfig(t)
	frames := []Frame{
		{Rung: -1, F: media.Frame{
			Track: 0, Config: h264, PTS: 90000, DTS: 86400, Key: true, Epoch: 3,
			Data: [][]byte{{0x65, 1, 2, 3}, {0x41, 9}, {}},
		}},
		{Rung: 2, F: media.Frame{
			Track: 7, Config: h265, PTS: -45000, DTS: -48600, Key: false, RASL: true, Epoch: 4000000000,
			Data: [][]byte{{0x02, 0x01, 0xaa}},
		}},
		{Rung: -1, F: media.Frame{
			Track: 1, Config: aac, PTS: 1 << 40, DTS: 1 << 40, Key: true,
			Data: [][]byte{bytes.Repeat([]byte{0x5a}, 300)},
		}},
		{Rung: 0, F: media.Frame{Track: 2, PTS: 1, DTS: 1}}, // no config, no data
	}
	msgs := make([]Message, 0, 16)
	msgs = append(msgs,
		Hello{Version: Version, PID: 4242},
		Configure{
			Rungs: []Rung{
				{Height: 1080, BitrateKbps: 5000, MaxBitrateKbps: 6000, BFrames: 2, Preset: "veryfast"},
				{Height: 360, BitrateKbps: 600, MaxBitrateKbps: 0, BFrames: -1, Preset: ""},
			},
			Audio: &AudioEncode{BitrateKbps: 128, SampleRate: 48000, Channels: 2},
		},
		Configure{},
		Discontinuity{},
		Stats{In: 1 << 50, Out: []uint64{10, 20, 0}, EncodeNanos: []int64{5, -6, 7000000000}},
		Stats{},
		Stats{
			In: 7, Out: []uint64{3}, EncodeNanos: []int64{9},
			DroppedTracks: 1, Reanchors: 2, AudioDropped: 3 << 40, AudioCorrupt: 4, VideoCorrupt: 5,
		},
		Stats{In: 1, VideoCorrupt: 1},
		Error{Rung: -1, Message: "boom ✓", Fatal: true},
		Error{Rung: 3},
		Stop{},
	)
	for _, f := range frames {
		msgs = append(msgs, f)
	}
	got := roundTrip(t, msgs...)
	require.Len(t, got, len(msgs))
	for i, want := range msgs {
		if wf, ok := want.(Frame); ok {
			gf, ok := got[i].(Frame)
			require.True(t, ok, "message %d is %T", i, got[i])
			assertFrameEqual(t, wf, gf)
			continue
		}
		if wc, ok := want.(Configure); ok && wc.Rungs == nil {
			assert.Equal(t, Configure{}, got[i])
			continue
		}
		if ws, ok := want.(Stats); ok && ws.Out == nil {
			gs, ok := got[i].(Stats)
			require.True(t, ok, "message %d is %T", i, got[i])
			assert.Empty(t, gs.Out, "message %d", i)
			assert.Empty(t, gs.EncodeNanos, "message %d", i)
			gs.Out, gs.EncodeNanos = nil, nil
			assert.Equal(t, ws, gs, "message %d", i)
			continue
		}
		assert.Equal(t, want, got[i], "message %d", i)
	}
}

func TestEnvelopeFieldNumbers(t *testing.T) {
	want := map[string]Message{
		"\x0a": Hello{}, "\x12": Configure{}, "\x1a": Frame{}, "\x22": Discontinuity{},
		"\x2a": Stats{}, "\x32": Error{}, "\x3a": Stop{},
	}
	for tag, m := range want {
		var buf bytes.Buffer
		require.NoError(t, NewWriter(&buf).Write(m))
		assert.Equal(t, tag[0], buf.Bytes()[4], "%T", m)
	}
}

// The media counters (ruling R-T6a) were appended as Stats fields 4 to 8, so
// a reader that predates them skips them, and they are absent when zero.
func TestStatsCounterFieldNumbers(t *testing.T) {
	body := func(s Stats) map[protowire.Number]uint64 {
		var buf bytes.Buffer
		require.NoError(t, NewWriter(&buf).Write(s))
		env := buf.Bytes()[4:]
		num, typ, n := protowire.ConsumeTag(env)
		require.Positive(t, n)
		require.Equal(t, protowire.Number(fStats), num)
		require.Equal(t, protowire.BytesType, typ)
		b, m := protowire.ConsumeBytes(env[n:])
		require.Positive(t, m)
		got := map[protowire.Number]uint64{}
		for len(b) > 0 {
			num, typ, n := protowire.ConsumeTag(b)
			require.Positive(t, n)
			require.Equal(t, protowire.VarintType, typ)
			v, m := protowire.ConsumeVarint(b[n:])
			require.Positive(t, m)
			got[num] = v
			b = b[n+m:]
		}
		return got
	}
	assert.Equal(t, map[protowire.Number]uint64{1: 0}, body(Stats{}))
	assert.Equal(t, map[protowire.Number]uint64{1: 9, 4: 1, 5: 2, 6: 3, 7: 4, 8: 5}, body(Stats{
		In: 9, DroppedTracks: 1, Reanchors: 2, AudioDropped: 3, AudioCorrupt: 4, VideoCorrupt: 5,
	}))
}

// A Stats from a newer child, with fields this reader does not know, still
// decodes: the unknown fields are skipped.
func TestStatsUnknownFieldSkipped(t *testing.T) {
	body := appendUint(nil, 1, 7)
	body = appendUint(body, 9, 42)
	body = appendBytes(body, 10, []byte("future"))
	body = appendUint(body, 5, 3) // a known field after the unknown ones
	env := appendEnvelope(nil, fStats, body)
	msg := binary.BigEndian.AppendUint32(nil, uint32(len(env)))
	msg = append(msg, env...)
	m, err := NewReader(bytes.NewReader(msg)).Read()
	require.NoError(t, err)
	assert.Equal(t, Stats{In: 7, Reanchors: 3}, m)
}

func TestConfigSentOnce(t *testing.T) {
	cfg := h264Config(t)
	f := Frame{Rung: -1, F: media.Frame{Config: cfg, PTS: 1, DTS: 1, Key: true, Data: [][]byte{{0x65, 1}}}}

	var buf bytes.Buffer
	w := NewWriter(&buf)
	require.NoError(t, w.Write(f))
	first := buf.Len()
	require.NoError(t, w.Write(f))
	second := buf.Len() - first

	assert.GreaterOrEqual(t, first-second, len(cfg.SPS[0])+len(cfg.PPS[0]))

	r := NewReader(&buf)
	m1, err := r.Read()
	require.NoError(t, err)
	m2, err := r.Read()
	require.NoError(t, err)
	c1, c2 := m1.(Frame).F.Config, m2.(Frame).F.Config
	require.NotNil(t, c1)
	assert.Same(t, c1, c2)
	assert.True(t, cfg.Equal(c1))
}

func TestConfigChangeNewPointer(t *testing.T) {
	a := h264Config(t)
	// A second, different SPS: same codec, different bytes.
	spsB := bytes.Clone(unhex(t, h264SPSHex))
	spsB[len(spsB)-1] ^= 0x01
	b, err := media.NewH264Config([][]byte{spsB}, [][]byte{unhex(t, h264PPSHex)})
	if err != nil {
		t.Skipf("variant SPS rejected by media: %v", err)
	}
	require.False(t, a.Equal(b))

	var buf bytes.Buffer
	w := NewWriter(&buf)
	for _, c := range []*media.TrackConfig{a, b, a} {
		require.NoError(t, w.Write(Frame{Rung: -1, F: media.Frame{Config: c, Key: true, Data: [][]byte{{1}}}}))
	}
	r := NewReader(&buf)
	var got [3]*media.TrackConfig
	for i := range got {
		m, err := r.Read()
		require.NoError(t, err)
		got[i] = m.(Frame).F.Config
	}
	assert.NotSame(t, got[0], got[1])
	assert.False(t, got[0].Equal(got[1]))
	assert.True(t, b.Equal(got[1]))
	assert.Same(t, got[0], got[2], "an earlier ID keeps resolving to its pointer")
}

func TestEqualButDistinctPointersGetDistinctIDs(t *testing.T) {
	a, b := h264Config(t), h264Config(t)
	require.True(t, a.Equal(b))
	var buf bytes.Buffer
	w := NewWriter(&buf)
	require.NoError(t, w.Write(Frame{F: media.Frame{Config: a, Data: [][]byte{{1}}}}))
	require.NoError(t, w.Write(Frame{F: media.Frame{Config: b, Data: [][]byte{{1}}}}))
	r := NewReader(&buf)
	m1, _ := r.Read()
	m2, _ := r.Read()
	assert.NotSame(t, m1.(Frame).F.Config, m2.(Frame).F.Config)
}

func TestTooLarge(t *testing.T) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], MaxMessage+1)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := NewReader(bytes.NewReader(hdr[:])).Read()
	runtime.ReadMemStats(&after)
	require.ErrorIs(t, err, ErrTooLarge)
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(1<<20), "no large allocation")

	allocs := testing.AllocsPerRun(10, func() {
		_, _ = NewReader(bytes.NewReader(hdr[:])).Read()
	})
	assert.Less(t, allocs, 20.0)

	// A liar: MaxMessage announced, 5 bytes sent. Must not preallocate 32 MiB.
	binary.BigEndian.PutUint32(hdr[:], MaxMessage)
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err = NewReader(io.MultiReader(bytes.NewReader(hdr[:]), bytes.NewReader([]byte{1, 2, 3, 4, 5}))).Read()
	runtime.ReadMemStats(&after)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(8<<20))

	// The Writer refuses an oversized message and writes nothing.
	var buf bytes.Buffer
	err = NewWriter(&buf).Write(Frame{F: media.Frame{Data: [][]byte{make([]byte, MaxMessage)}}})
	require.ErrorIs(t, err, ErrTooLarge)
	assert.Zero(t, buf.Len())
}

func TestTruncated(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, NewWriter(&buf).Write(Hello{Version: Version, PID: 1}))
	full := buf.Bytes()
	for _, n := range []int{1, 3, 4, len(full) - 1} {
		_, err := NewReader(bytes.NewReader(full[:n])).Read()
		require.ErrorIs(t, err, io.ErrUnexpectedEOF, "cut at %d", n)
	}
	_, err := NewReader(bytes.NewReader(nil)).Read()
	require.ErrorIs(t, err, io.EOF)
	assert.NotErrorIs(t, err, io.ErrUnexpectedEOF)
}

func record(body []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(body)))
	return append(out, body...)
}

func TestMalformed(t *testing.T) {
	cases := map[string][]byte{
		"empty envelope":     record(nil),
		"unknown type":       record([]byte{0x42, 0x00}),
		"wrong wire type":    record([]byte{0x08, 0x01}),
		"trailing bytes":     record([]byte{0x22, 0x00, 0x22, 0x00}),
		"bad inner varint":   record([]byte{0x0a, 0x02, 0x08, 0x80}),
		"unknown config id":  record([]byte{0x1a, 0x02, 0x48, 0x05}),
		"config without id":  record([]byte{0x1a, 0x04, 0x52, 0x02, 0x08, 0x03}),
		"bad bool":           record([]byte{0x1a, 0x02, 0x30, 0x02}),
		"config bad payload": record([]byte{0x1a, 0x08, 0x48, 0x01, 0x52, 0x04, 0x08, 0x01, 0x1a, 0x00}),
	}
	for name, b := range cases {
		_, err := NewReader(bytes.NewReader(b)).Read()
		require.ErrorIs(t, err, ErrMalformed, name)
	}
}

func TestWriteUnsupported(t *testing.T) {
	assert.Error(t, NewWriter(io.Discard).Write(nil))
	assert.Error(t, NewWriter(io.Discard).Write(Frame{F: media.Frame{Track: -1}}))
	assert.Error(t, NewWriter(io.Discard).Write(Configure{Rungs: []Rung{{Height: -1}}}))
}

func TestWriteFailureDoesNotRegisterConfig(t *testing.T) {
	cfg := h264Config(t)
	f := Frame{F: media.Frame{Config: cfg, Data: [][]byte{{1}}}}
	w := NewWriter(failWriter{})
	require.Error(t, w.Write(f))
	var buf bytes.Buffer
	w.w = &buf
	require.NoError(t, w.Write(f))
	m, err := NewReader(&buf).Read()
	require.NoError(t, err, "the retry carries the full config")
	assert.True(t, cfg.Equal(m.(Frame).F.Config))
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
