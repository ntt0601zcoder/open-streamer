package mpegts

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testProgram() PMT {
	return PMT{
		PCRPID: tVideo,
		Streams: []Stream{
			{PID: tVideo, Type: tH264},
			{PID: tAudio, Type: tAAC},
		},
	}
}

func newTestWriter(t *testing.T, out io.Writer) *Writer {
	t.Helper()
	w := NewWriter(out)
	require.NoError(t, w.SetProgram(testProgram()))
	return w
}

// wDemux returns every PES and PMT of a stream with the final stats.
func wDemux(t *testing.T, b []byte) (pes []*PES, pmts []*PMT, st DemuxStats) {
	t.Helper()
	d := NewDemux(bytes.NewReader(b))
	for {
		ev, err := d.Next()
		if errors.Is(err, io.EOF) {
			return pes, pmts, d.Stats()
		}
		require.NoError(t, err)
		if ev.PES != nil {
			pes = append(pes, ev.PES)
		}
		if ev.PMT != nil {
			pmts = append(pmts, ev.PMT)
		}
	}
}

func TestWriterRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := newTestWriter(t, &buf)
	require.NoError(t, w.WriteTables())

	vdata := bytes.Repeat([]byte{0xAB, 0xCD, 0x01}, 200)
	adata := []byte{1, 2, 3, 4, 5}
	require.NoError(t, w.WritePES(tVideo, vdata, PESOptions{
		PTS: 1 << 32, DTS: 12345, HasDTS: true, RandomAccess: true, PCR: 999, Video: true,
	}))
	require.NoError(t, w.WritePES(tAudio, adata, PESOptions{PTS: 777, PCR: -1}))
	require.NoError(t, w.WritePES(tAudio, nil, PESOptions{PTS: 778, PCR: -1}))
	require.Zero(t, buf.Len()%PacketSize)

	pes, pmts, st := wDemux(t, buf.Bytes())
	require.Len(t, pmts, 1)
	assert.Equal(t, uint16(1), pmts[0].Program)
	assert.Equal(t, uint16(4096), pmts[0].PID)
	assert.Equal(t, uint16(tVideo), pmts[0].PCRPID)
	assert.Equal(t, testProgram().Streams, pmts[0].Streams)
	require.Len(t, pes, 3)
	assert.Equal(t, vdata, pes[0].Data)
	assert.Equal(t, int64(1<<32), pes[0].PTS)
	assert.Equal(t, int64(12345), pes[0].DTS)
	assert.True(t, pes[0].RandomAccess)
	assert.Equal(t, adata, pes[1].Data)
	assert.Equal(t, int64(777), pes[1].PTS)
	assert.Equal(t, int64(777), pes[1].DTS)
	assert.False(t, pes[1].RandomAccess)
	assert.Empty(t, pes[2].Data)
	assert.Zero(t, st.CCErrors+st.PSIErrors+st.PESErrors)

	// The PCR rides on the first packet of the video PES.
	r := NewReader(bytes.NewReader(buf.Bytes()))
	var pcrs []int64
	for {
		p, err := r.Next()
		if err != nil {
			break
		}
		if p.PCR >= 0 {
			assert.Equal(t, uint16(tVideo), p.PID)
			assert.True(t, p.PUSI)
			assert.True(t, p.RandomAccess)
			pcrs = append(pcrs, p.PCR)
		}
	}
	assert.Equal(t, []int64{999}, pcrs)
}

func TestWriterTimestampWrap(t *testing.T) {
	var buf bytes.Buffer
	w := newTestWriter(t, &buf)
	require.NoError(t, w.WritePES(tVideo, []byte{1}, PESOptions{
		PTS: 1<<33 + 5, DTS: 1<<33 + 4, HasDTS: true, PCR: 1<<33 + 3, Video: true,
	}))
	pes, _, _ := wDemux(t, buf.Bytes())
	// Tables were not written, so Demux sees no PES; write them and retry.
	require.Empty(t, pes)
	buf.Reset()
	require.NoError(t, w.WriteTables())
	require.NoError(t, w.WritePES(tVideo, []byte{1}, PESOptions{
		PTS: 1<<33 + 5, DTS: 1<<33 + 4, HasDTS: true, PCR: 1<<33 + 3, Video: true,
	}))
	pes, _, _ = wDemux(t, buf.Bytes())
	require.Len(t, pes, 1)
	assert.Equal(t, int64(5), pes[0].PTS)
	assert.Equal(t, int64(4), pes[0].DTS)
	r := NewReader(bytes.NewReader(buf.Bytes()))
	for {
		p, err := r.Next()
		require.NoError(t, err)
		if p.PCR >= 0 {
			assert.Equal(t, int64(3), p.PCR)
			break
		}
	}
}

// TestWriterPacketing covers every payload size around the packet boundaries,
// with and without an adaptation field on the first packet.
func TestWriterPacketing(t *testing.T) {
	for _, withAF := range []bool{false, true} {
		for n := 0; n <= 600; n++ {
			var buf bytes.Buffer
			w := newTestWriter(t, &buf)
			require.NoError(t, w.WriteTables())
			data := bytes.Repeat([]byte{0x5A}, n)
			opt := PESOptions{PTS: 90, PCR: -1}
			if withAF {
				opt.RandomAccess, opt.PCR = true, 5
			}
			require.NoError(t, w.WritePES(tAudio, data, opt))
			require.Zero(t, buf.Len()%PacketSize)
			pes, _, st := wDemux(t, buf.Bytes())
			require.Len(t, pes, 1, "n=%d af=%v", n, withAF)
			require.Equal(t, data, append([]byte{}, pes[0].Data...), "n=%d af=%v", n, withAF)
			require.Zero(t, st.CCErrors+st.PESErrors+st.Reader.Resyncs, "n=%d", n)
		}
	}
}

func TestWriterLargePES(t *testing.T) {
	var buf bytes.Buffer
	w := newTestWriter(t, &buf)
	require.NoError(t, w.WriteTables())
	data := make([]byte, 100_000)
	for i := range data {
		data[i] = byte(i * 7)
	}
	require.NoError(t, w.WritePES(tVideo, data, PESOptions{PTS: 9, Video: true, PCR: -1}))
	require.NoError(t, w.WritePES(tVideo, data[:10], PESOptions{PTS: 10, Video: true, PCR: -1}))

	// PES_packet_length is 0 on the first one.
	r := NewReader(bytes.NewReader(buf.Bytes()))
	for {
		p, err := r.Next()
		require.NoError(t, err)
		if p.PID == tVideo {
			require.True(t, p.PUSI)
			assert.Equal(t, []byte{0, 0, 1, 0xE0, 0, 0}, p.Payload[:6])
			break
		}
	}
	pes, _, st := wDemux(t, buf.Bytes())
	require.Len(t, pes, 2)
	assert.Equal(t, data, pes[0].Data)
	assert.Equal(t, data[:10], pes[1].Data)
	assert.Zero(t, st.CCErrors+st.PESErrors)

	// Audio cannot use length 0.
	err := w.WritePES(tAudio, data, PESOptions{PTS: 1, PCR: -1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mpegts:")
}

func TestWriterSmallPESLength(t *testing.T) {
	var buf bytes.Buffer
	w := newTestWriter(t, &buf)
	data := make([]byte, 65535-8) // header data 5 + 3 fixed bytes => exactly 65535
	require.NoError(t, w.WritePES(tAudio, data, PESOptions{PTS: 1, PCR: -1}))
	r := NewReader(bytes.NewReader(buf.Bytes()))
	p, err := r.Next()
	require.NoError(t, err)
	assert.Equal(t, []byte{0, 0, 1, 0xC0, 0xFF, 0xFF}, p.Payload[:6])
}

func TestWriterVersion(t *testing.T) {
	var buf bytes.Buffer
	w := newTestWriter(t, &buf)
	version := func() uint8 {
		buf.Reset()
		require.NoError(t, w.WriteTables())
		_, pmts, st := wDemux(t, buf.Bytes())
		require.Len(t, pmts, 1)
		require.Zero(t, st.PSIErrors)
		return pmts[0].Version
	}
	assert.Equal(t, uint8(0), version())
	require.NoError(t, w.SetProgram(testProgram()))
	assert.Equal(t, uint8(0), version(), "identical program keeps the version")

	p := testProgram()
	p.Streams = p.Streams[:1]
	require.NoError(t, w.SetProgram(p))
	assert.Equal(t, uint8(1), version())

	p.PCRPID = 0x102
	p.Streams = append(p.Streams, Stream{PID: 0x102, Type: tAAC})
	require.NoError(t, w.SetProgram(p))
	assert.Equal(t, uint8(2), version())

	p.PCRPID = 0x100
	require.NoError(t, w.SetProgram(p))
	assert.Equal(t, uint8(3), version(), "PCR PID change")

	for i := 4; i <= 33; i++ {
		p.Streams[0].Type = uint8(0x20 + i)
		require.NoError(t, w.SetProgram(p))
	}
	assert.Equal(t, uint8(33%32), version(), "wraps mod 32")
}

func TestWriterSetProgramErrors(t *testing.T) {
	w := NewWriter(io.Discard)
	for name, p := range map[string]PMT{
		"duplicate":  {PCRPID: 0x100, Streams: []Stream{{PID: 0x100}, {PID: 0x100}}},
		"pat pid":    {PCRPID: 0x100, Streams: []Stream{{PID: 0x100}, {PID: 0}}},
		"null pid":   {PCRPID: 0x100, Streams: []Stream{{PID: 0x100}, {PID: 0x1FFF}}},
		"pmt pid":    {PCRPID: 0x100, Streams: []Stream{{PID: 0x100}, {PID: 4096}}},
		"big pid":    {PCRPID: 0x100, Streams: []Stream{{PID: 0x100}, {PID: 0x2000}}},
		"pcr absent": {PCRPID: 0x200, Streams: []Stream{{PID: 0x100}}},
		"no streams": {PCRPID: 0x100},
		"bad pmt":    {PID: 0x1FFF, PCRPID: 0x100, Streams: []Stream{{PID: 0x100}}},
		"low pmt":    {PID: 5, PCRPID: 0x100, Streams: []Stream{{PID: 0x100}}},
	} {
		err := w.SetProgram(p)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "mpegts:", name)
	}
	require.Error(t, w.WriteTables(), "no program set")
	require.Error(t, w.WritePES(0x100, nil, PESOptions{PCR: -1}))
}

func TestWriterCCAcrossSetProgram(t *testing.T) {
	var buf bytes.Buffer
	w := newTestWriter(t, &buf)
	write := func() {
		require.NoError(t, w.WriteTables())
		for _, s := range w.program.Streams {
			require.NoError(t, w.WritePES(s.PID, bytes.Repeat([]byte{1}, 500), PESOptions{PTS: 1, PCR: -1}))
		}
	}
	write()
	write()
	// Remove the audio PID, then add it back; the PMT PID changes too.
	p := testProgram()
	p.Streams = p.Streams[:1]
	p.PID = 0x200
	require.NoError(t, w.SetProgram(p))
	write()
	require.Error(t, w.WritePES(tAudio, nil, PESOptions{PCR: -1}))
	write()
	p = testProgram()
	p.Program = 7
	require.NoError(t, w.SetProgram(p))
	write()
	write()

	pes, pmts, st := wDemux(t, buf.Bytes())
	assert.Zero(t, st.CCErrors, "continuity errors")
	assert.Zero(t, st.PSIErrors+st.PESErrors)
	assert.Len(t, pmts, 3)
	assert.Len(t, pes, 10)
}

func TestWriterSetOutput(t *testing.T) {
	var a, b bytes.Buffer
	w := newTestWriter(t, &a)
	require.NoError(t, w.WriteTables())
	require.NoError(t, w.WritePES(tVideo, bytes.Repeat([]byte{9}, 700), PESOptions{PTS: 1, PCR: 1, Video: true}))
	w.SetOutput(&b)
	require.NoError(t, w.WriteTables())
	require.NoError(t, w.WritePES(tVideo, bytes.Repeat([]byte{9}, 700), PESOptions{PTS: 2, PCR: 2, Video: true}))
	require.NotZero(t, a.Len())
	require.NotZero(t, b.Len())

	pes, _, st := wDemux(t, append(a.Bytes(), b.Bytes()...))
	assert.Len(t, pes, 2)
	assert.Zero(t, st.CCErrors)
}

func TestWriterUnknownPID(t *testing.T) {
	w := newTestWriter(t, io.Discard)
	err := w.WritePES(0x300, []byte{1}, PESOptions{PCR: -1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mpegts:")
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("boom") }

func TestWriterWriteError(t *testing.T) {
	w := newTestWriter(t, failWriter{})
	require.Error(t, w.WriteTables())
	require.Error(t, w.WritePES(tVideo, []byte{1}, PESOptions{PCR: -1}))
}

func fixturePES(t *testing.T) (video, audio []*PES) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "h264_aac.ts"))
	require.NoError(t, err)
	pes, pmts, _ := wDemux(t, raw)
	require.NotEmpty(t, pmts)
	for _, s := range pmts[0].Streams {
		for _, p := range pes {
			if p.PID != s.PID {
				continue
			}
			switch s.Type {
			case tH264:
				video = append(video, p)
			case tAAC:
				audio = append(audio, p)
			}
		}
	}
	require.NotEmpty(t, video)
	require.NotEmpty(t, audio)
	return video, audio
}

func TestWriterFFprobe(t *testing.T) {
	probe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	video, audio := fixturePES(t)
	var buf bytes.Buffer
	w := newTestWriter(t, &buf)
	require.NoError(t, w.WriteTables())
	vi, ai := 0, 0
	for vi < len(video) || ai < len(audio) {
		if vi < len(video) {
			p := video[vi]
			vi++
			require.NoError(t, w.WritePES(tVideo, p.Data, PESOptions{
				PTS: p.PTS, DTS: p.DTS, HasDTS: p.DTS != p.PTS, RandomAccess: p.RandomAccess,
				PCR: p.DTS, Video: true,
			}))
		}
		for ai < len(audio) && (vi >= len(video) || audio[ai].PTS <= video[vi-1].PTS) {
			p := audio[ai]
			ai++
			require.NoError(t, w.WritePES(tAudio, p.Data, PESOptions{PTS: p.PTS, PCR: -1}))
		}
	}
	file := filepath.Join(t.TempDir(), "out.ts")
	require.NoError(t, os.WriteFile(file, buf.Bytes(), 0o600))

	got := probeStreams(t, probe, file)
	want := probeStreams(t, probe, filepath.Join("..", "..", "testdata", "fixtures", "h264_aac.ts"))
	assert.Contains(t, got, "video:h264")
	assert.Contains(t, got, "audio:aac")
	assert.Equal(t, want, got, "codecs and packet counts match the source")
}

// probeStreams maps "type:codec" to the packet count ffprobe reads.
func probeStreams(t *testing.T, probe, file string) map[string]string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), probe, "-v", "error", "-count_packets", "-show_entries",
		"stream=codec_name,codec_type,nb_read_packets", "-of", "json", file).Output()
	require.NoError(t, err)
	var res struct {
		Streams []struct {
			Name    string `json:"codec_name"`
			Type    string `json:"codec_type"`
			Packets string `json:"nb_read_packets"`
		} `json:"streams"`
	}
	require.NoError(t, json.Unmarshal(out, &res))
	got := map[string]string{}
	for _, s := range res.Streams {
		got[s.Type+":"+s.Name] = s.Packets
	}
	return got
}

// patVersions returns the PAT version of every PAT packet in b.
func patVersions(t *testing.T, b []byte) []uint8 {
	t.Helper()
	var vs []uint8
	r := NewReader(bytes.NewReader(b))
	for {
		p, err := r.Next()
		if err != nil {
			return vs
		}
		if p.PID == 0 && p.PUSI {
			vs = append(vs, p.Payload[1+5]>>1&0x1F)
		}
	}
}

func TestWriterPATVersion(t *testing.T) {
	var buf bytes.Buffer
	w := newTestWriter(t, &buf)
	tables := func() {
		require.NoError(t, w.WriteTables())
		require.NoError(t, w.WritePES(tVideo, []byte{1}, PESOptions{PTS: 1, PCR: -1}))
	}
	tables()
	require.NoError(t, w.SetProgram(testProgram()))
	tables()
	p := testProgram()
	p.Streams = p.Streams[:1] // PMT changes, PAT does not
	require.NoError(t, w.SetProgram(p))
	tables()
	p.PID = 0x200
	require.NoError(t, w.SetProgram(p))
	tables()
	p.Program = 9
	require.NoError(t, w.SetProgram(p))
	tables()
	require.NoError(t, w.SetProgram(p))
	tables()
	assert.Equal(t, []uint8{0, 0, 0, 1, 2, 2}, patVersions(t, buf.Bytes()))

	pes, pmts, st := wDemux(t, buf.Bytes())
	assert.Zero(t, st.CCErrors+st.PSIErrors)
	assert.Len(t, pes, 6)
	require.NotEmpty(t, pmts)
	last := pmts[len(pmts)-1]
	assert.Equal(t, uint16(9), last.Program)
	assert.Equal(t, uint16(0x200), last.PID)
}

func TestWriterPMTTooLarge(t *testing.T) {
	prog := func(n int) PMT {
		p := PMT{PCRPID: 0x100}
		for i := range n {
			p.Streams = append(p.Streams, Stream{PID: 0x100 + uint16(i), Type: tAAC})
		}
		return p
	}
	var buf bytes.Buffer
	w := NewWriter(&buf)
	require.NoError(t, w.SetProgram(prog(33)))
	require.NoError(t, w.WriteTables())
	assert.Zero(t, buf.Len()%PacketSize)
	_, pmts, st := wDemux(t, buf.Bytes())
	require.Len(t, pmts, 1)
	assert.Len(t, pmts[0].Streams, 33)
	assert.Zero(t, st.PSIErrors)

	err := w.SetProgram(prog(34))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mpegts:")
}
