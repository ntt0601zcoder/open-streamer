package conform

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/datarhei/gosrt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/output/srtout"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
)

const srtWait = 20 * time.Second

type srtResult struct {
	c   Capture
	err error
}

// readSRTAsync runs ReadTS in a goroutine.
func readSRTAsync(u string, opt ReadOptions) <-chan srtResult {
	ch := make(chan srtResult, 1)
	go func() {
		c, err := ReadTS(context.Background(), u, opt)
		ch <- srtResult{c, err}
	}()
	return ch
}

func awaitSRT(t *testing.T, ch <-chan srtResult) srtResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(srtWait):
		t.Fatal("ReadTS did not finish")
		return srtResult{}
	}
}

// srtServer is an srtout server for streams "s" (h) on 127.0.0.1.
type srtServer struct {
	h    *hub.Hub
	srv  *srtout.Server
	addr string
}

func startSRT(t *testing.T) *srtServer {
	t.Helper()
	h := hub.New(hub.Options{})
	srv := srtout.New(func(name string) (*hub.Hub, bool) { return h, name == "s" }, srtout.Options{})
	require.NoError(t, srv.Listen("127.0.0.1:0"))
	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()
	t.Cleanup(func() {
		require.NoError(t, srv.Close())
		select {
		case <-served:
		case <-time.After(srtWait):
			t.Error("Serve did not return after Close")
		}
		h.Close()
	})
	return &srtServer{h: h, srv: srv, addr: srv.Addr().String()}
}

// feedSRT writes frames to h paced at speed times real time by DTS, then
// closes the hub.
func feedSRT(h *hub.Hub, frames []media.Frame, speed float64) {
	start, base := time.Now(), frames[0].DTS
	for _, f := range frames {
		due := start.Add(time.Duration(float64(f.DTS-base) / media.ClockRate / speed * float64(time.Second)))
		time.Sleep(time.Until(due))
		h.Write(f)
	}
	h.Close()
}

// splitAtKey returns the frames before the second video keyframe and the
// rest.
func splitAtKey(frames []media.Frame) (head, rest []media.Frame) {
	keys := 0
	for i, f := range frames {
		if f.Kind() == media.KindVideo && f.Key {
			if keys++; keys == 2 {
				return frames[:i], frames[i:]
			}
		}
	}
	return frames, nil
}

// connectSRT writes pre to the hub (which makes it live), starts ReadTS on
// the server and waits for its subscription.
func connectSRT(t *testing.T, e *srtServer, pre []media.Frame, query string, opt ReadOptions) <-chan srtResult {
	t.Helper()
	for _, f := range pre {
		e.h.Write(f)
	}
	if opt.Timeout == 0 {
		opt.Timeout = srtWait
	}
	ch := readSRTAsync("srt://"+e.addr+query, opt)
	require.Eventually(t, func() bool { return len(e.h.Stats()) > 0 }, srtWait, time.Millisecond, "subscribed")
	return ch
}

// readSRTOutput reads frames back from the SRT output of a hub fed with
// them: the first GOP before the connection, the rest paced after it.
func readSRTOutput(t *testing.T, frames []media.Frame, query string) srtResult {
	t.Helper()
	e := startSRT(t)
	pre, rest := splitAtKey(frames)
	ch := connectSRT(t, e, pre, query, ReadOptions{})
	feedSRT(e.h, rest, 4)
	return awaitSRT(t, ch)
}

func TestReadSRTClean(t *testing.T) {
	for _, name := range []string{"h264_aac.ts", "h265_aac51.ts"} {
		t.Run(name, func(t *testing.T) {
			frames := streamClip(t, name)
			r := readSRTOutput(t, frames, "?streamid=live/s&latency=500")
			require.NoError(t, r.err)
			assert.Empty(t, r.c.Violations)
			assert.Len(t, r.c.Frames, len(frames))
		})
	}
}

func TestReadSRTAccessControlID(t *testing.T) {
	r := readSRTOutput(t, streamClip(t, "h264_aac.ts"), "?streamid=%23%21%3A%3Ar%3Dlive%2Fs")
	require.NoError(t, r.err)
	assert.Empty(t, r.c.Violations)
}

// A join on a GOP whose keyframe is over 2 MB: no false violations.
func TestReadSRTBurst(t *testing.T) {
	var h264 *media.TrackConfig
	for _, f := range streamClip(t, "h264_aac.ts") {
		if f.Config != nil && f.Config.Codec == media.CodecH264 {
			h264 = f.Config
			break
		}
	}
	require.NotNil(t, h264)
	frame := func(i, size int, key bool) media.Frame {
		data := make([]byte, size)
		data[0] = 0x41
		if key {
			data[0] = 0x65
		}
		for j := 1; j < size; j++ {
			data[j] = byte(j*7 + i)
		}
		dts := int64(i) * 3600
		return media.Frame{Track: 0, Config: h264, PTS: dts, DTS: dts, Key: key, Data: [][]byte{data}, Epoch: 1}
	}
	const big = 2_300_000
	e := startSRT(t)
	for i := range 25 {
		size := 2000
		if i == 0 {
			size = big
		}
		e.h.Write(frame(i, size, i == 0))
	}
	var rest []media.Frame
	for i := 25; i < 100; i++ {
		rest = append(rest, frame(i, 2000, i%25 == 0))
	}
	ch := readSRTAsync("srt://"+e.addr+"?streamid=live/s&latency=1000", ReadOptions{Timeout: srtWait})
	require.Eventually(t, func() bool { return len(e.h.Stats()) > 0 }, srtWait, time.Millisecond, "subscribed")
	feedSRT(e.h, rest, 2)
	r := awaitSRT(t, ch)
	require.NoError(t, r.err)
	assert.Empty(t, r.c.Violations)
	assert.Len(t, r.c.Frames, 100)
}

func TestReadSRTReject(t *testing.T) {
	e := startSRT(t)
	for _, q := range []string{"?streamid=live/nope", "", "?streamid=%23%21%3A%3Ar%3Dlive%2Fs%2Cm%3Dpublish"} {
		start := time.Now()
		_, err := ReadTS(context.Background(), "srt://"+e.addr+q, ReadOptions{Timeout: srtWait})
		require.Error(t, err, q)
		assert.Contains(t, err.Error(), "rejected", q)
		assert.Less(t, time.Since(start), 5*time.Second, q)
	}
}

func TestReadSRTBadURL(t *testing.T) {
	for _, u := range []string{"srt://127.0.0.1", "srt://127.0.0.1:9?latency=soon", "srt://127.0.0.1:9?latency=-1", "srt://:9"} {
		_, err := ReadTS(context.Background(), u, ReadOptions{Timeout: time.Second})
		assert.Error(t, err, u)
	}
}

// scriptedSRT serves data to the first caller, then closes.
func scriptedSRT(t *testing.T, data []byte) string {
	t.Helper()
	return scriptedSRTLoop(t, data, false)
}

// scriptedSRTLoop is scriptedSRT; with loop, data repeats until the caller
// goes away, and the connection never closes by itself.
func scriptedSRTLoop(t *testing.T, data []byte, loop bool) string {
	t.Helper()
	all := data
	cfg := srt.DefaultConfig()
	ln, err := srt.Listen("srt", "127.0.0.1:0", cfg)
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, err := ln.Accept2()
		if err != nil {
			return
		}
		conn, err := req.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			for len(data) > 0 {
				n := min(len(data), 1316)
				if _, err := conn.Write(data[:n]); err != nil {
					return
				}
				data = data[n:]
				time.Sleep(time.Millisecond)
			}
			if !loop {
				break
			}
			data = all
		}
		time.Sleep(500 * time.Millisecond)
	}()
	t.Cleanup(func() {
		ln.Close()
		<-done
	})
	return ln.Addr().String()
}

// muxTS returns frames as TS bytes through our muxer.
func muxTS(t *testing.T, frames []media.Frame) []byte {
	t.Helper()
	var tracks []*media.TrackConfig
	for _, f := range frames {
		for len(tracks) <= f.Track {
			tracks = append(tracks, nil)
		}
		if f.Config != nil && tracks[f.Track] == nil {
			tracks[f.Track] = f.Config
		}
	}
	var buf bytes.Buffer
	m, err := ts.NewMuxer(&buf, tracks)
	require.NoError(t, err)
	for _, f := range frames {
		require.NoError(t, m.WriteFrame(f))
	}
	return buf.Bytes()
}

// dropPID removes every packet of pid.
func dropPID(t *testing.T, data []byte, pid uint16) []byte {
	t.Helper()
	r := mpegts.NewReader(bytes.NewReader(data))
	var out []byte
	for i := 0; ; i++ {
		p, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		require.NoError(t, err)
		if p.PID != pid {
			out = append(out, data[i*mpegts.PacketSize:(i+1)*mpegts.PacketSize]...)
		}
	}
}

// dropVideoPackets removes a run of payload packets of the video PID, from
// the middle of the stream.
func dropVideoPackets(t *testing.T, data []byte) []byte {
	t.Helper()
	const pid = 256 // the first elementary PID of the muxer
	r := mpegts.NewReader(bytes.NewReader(data))
	var out []byte
	seen, dropped := 0, 0
	for i := 0; ; i++ {
		p, err := r.Next()
		if errors.Is(err, io.EOF) {
			require.Equal(t, 3, dropped)
			return out
		}
		require.NoError(t, err)
		if p.PID == pid && p.HasPayload && !p.PUSI {
			if seen++; seen > 20 && dropped < 3 {
				dropped++
				continue
			}
		}
		out = append(out, data[i*mpegts.PacketSize:(i+1)*mpegts.PacketSize]...)
	}
}

func TestReadSRTViolations(t *testing.T) {
	clip := streamClip(t, "h264_aac.ts")
	cases := map[string]struct {
		data []byte
		want string // a violation rule, or "error:" and text of the error
	}{
		"missing PAT":    {dropPID(t, muxTS(t, clip), 0), RuleTSPSI},
		"continuity gap": {dropVideoPackets(t, muxTS(t, clip)), RuleTSContinuity},
		// The timeline restarts at the beginning: DTS repeats.
		"backward PTS": {muxTS(t, append(slices.Clone(clip), clip...)), RuleDTS},
		// The first keyframe's IDR slice is made a plain slice, without SEI.
		"delta first": {muxTS(t, func() []media.Frame {
			out := slices.Clone(clip)
			nals := make([][]byte, 0, len(out[0].Data))
			for _, n := range out[0].Data {
				n = slices.Clone(n)
				switch n[0] & 0x1f {
				case 5:
					n[0] = n[0]&0xe0 | 1
				case 6:
					continue // a recovery point SEI would make it a key
				}
				nals = append(nals, n)
			}
			out[0].Data, out[0].Key = nals, false
			return out
		}()), RuleTSStart},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			addr := scriptedSRT(t, tc.data)
			r := awaitSRT(t, readSRTAsync("srt://"+addr+"?streamid=live/s", ReadOptions{Timeout: srtWait}))
			if text, ok := cutPrefix(tc.want, "error:"); ok {
				require.Error(t, r.err)
				assert.Contains(t, r.err.Error(), text)
				return
			}
			require.NoError(t, r.err)
			assert.Contains(t, rules(r.c.Violations), tc.want, "%v", r.c.Violations)
		})
	}
}

func cutPrefix(s, p string) (string, bool) {
	if len(s) >= len(p) && s[:len(p)] == p {
		return s[len(p):], true
	}
	return s, false
}

// pump reports a full buffer as an overflow instead of blocking.
func TestSRTPumpOverflow(t *testing.T) {
	src := &endlessReader{}
	p := startPump(src, 1<<16)
	defer p.stop()
	require.Eventually(t, func() bool { return src.closed.Load() }, srtWait, time.Millisecond, "source closed")
	buf := make([]byte, 1<<20)
	var err error
	for err == nil {
		_, err = p.Read(buf)
	}
	assert.ErrorIs(t, err, errReaderOverflow)
	assert.Contains(t, err.Error(), "reader overflow")
}

func TestReadSRTCancel(t *testing.T) {
	e := startSRT(t)
	pre, _ := splitAtKey(streamClip(t, "h264_aac.ts"))
	for _, f := range pre {
		e.h.Write(f)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan srtResult, 1)
	go func() {
		_, err := ReadTS(ctx, "srt://"+e.addr+"?streamid=live/s", ReadOptions{Timeout: srtWait})
		ch <- srtResult{err: err}
	}()
	require.Eventually(t, func() bool { return len(e.h.Stats()) > 0 }, srtWait, time.Millisecond, "subscribed")
	cancel()
	r := awaitSRT(t, ch)
	require.Error(t, r.err)
	assert.ErrorIs(t, r.err, context.Canceled)
}

// A stream that goes quiet is reported as stalled.
func TestReadSRTStall(t *testing.T) {
	e := startSRT(t)
	pre, _ := splitAtKey(streamClip(t, "h264_aac.ts"))
	start := time.Now()
	r := awaitSRT(t, connectSRT(t, e, pre, "?streamid=live/s", ReadOptions{Timeout: 700 * time.Millisecond}))
	require.Error(t, r.err)
	assert.Contains(t, r.err.Error(), "stalled")
	assert.Less(t, time.Since(start), 10*time.Second)
}

func TestReadSRTDuration(t *testing.T) {
	frames := streamClip(t, "h264_aac.ts")
	e := startSRT(t)
	pre, rest := splitAtKey(frames)
	ch := connectSRT(t, e, pre, "?streamid=live/s&latency=500", ReadOptions{Duration: 1500 * time.Millisecond})
	feedSRT(e.h, rest, 4)
	r := awaitSRT(t, ch)
	require.NoError(t, r.err)
	assert.NotEmpty(t, r.c.Frames)
	assert.Less(t, len(r.c.Frames), len(frames))
}

// endlessReader yields 1316-byte chunks without end; closed is set by Close.
type endlessReader struct{ closed atomic.Bool }

func (e *endlessReader) Read(p []byte) (int, error) {
	if e.closed.Load() {
		return 0, io.EOF
	}
	return min(len(p), 1316), nil
}

func (e *endlessReader) Close() error { e.closed.Store(true); return nil }

// A stream that keeps sending but carries no PAT is reported at the read
// timeout, whatever the transport.
func TestReadTSNoPSIEndless(t *testing.T) {
	data := dropPID(t, muxTS(t, streamClip(t, "h264_aac.ts")), 0)
	check := func(t *testing.T, u string) {
		t.Helper()
		start := time.Now()
		r := awaitSRT(t, readSRTAsync(u, ReadOptions{Timeout: time.Second}))
		require.NoError(t, r.err)
		assert.Equal(t, []string{RuleTSPSI}, rules(r.c.Violations))
		assert.Less(t, time.Since(start), 10*time.Second)
	}
	t.Run("srt", func(t *testing.T) {
		check(t, "srt://"+scriptedSRTLoop(t, data, true)+"?streamid=x")
	})
	t.Run("http", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for r.Context().Err() == nil {
				if _, err := w.Write(data); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				time.Sleep(time.Millisecond)
			}
		}))
		defer srv.Close()
		check(t, srv.URL+"/x.ts")
	})
}

// pesPacket returns one TS packet that starts a video PES with payload.
func pesPacket(pid uint16, payload []byte) []byte {
	p := make([]byte, mpegts.PacketSize)
	p[0], p[1], p[2], p[3] = 0x47, 0x40|byte(pid>>8), byte(pid), 0x10
	pes := append([]byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0, 0}, payload...)
	n := copy(p[4:], pes)
	for i := 4 + n; i < len(p); i++ {
		p[i] = 0xff
	}
	return p
}

func TestVideoStartSEI(t *testing.T) {
	sc := []byte{0, 0, 0, 1}
	nal := func(parts ...[]byte) []byte {
		var out []byte
		for _, p := range parts {
			out = append(append(out, sc...), p...)
		}
		return out
	}
	aud, slice, idr := []byte{0x09, 0xf0}, []byte{0x41, 0x9a, 0x00}, []byte{0x65, 0x88, 0x00}
	plain := []byte{0x06, 0x01, 0x01, 0x00, 0x80} // payload type 1
	recovery := []byte{0x06, 0x06, 0x01, 0x00, 0x80}
	for name, tc := range map[string]struct {
		data      []byte
		ok, known bool
	}{
		"sei then delta":      {nal(aud, plain, slice), false, true},
		"recovery then delta": {nal(aud, recovery, slice), true, true},
		"sei then idr":        {nal(aud, plain, idr), true, true},
		"delta":               {nal(aud, slice), false, true},
		"only aud":            {nal(aud), false, false},
	} {
		t.Run(name, func(t *testing.T) {
			ok, known := videoStart(pesPacket(256, tc.data), 256, media.CodecH264)
			assert.Equal(t, tc.known, known)
			assert.Equal(t, tc.ok, ok)
		})
	}
}

// A stream that ends before its first byte is a read error, whatever the
// transport, not a missing-PSI violation.
func TestReadTSEmpty(t *testing.T) {
	t.Run("http", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		c, err := ReadTS(context.Background(), srv.URL+"/x.ts", ReadOptions{Timeout: 5 * time.Second})
		require.ErrorContains(t, err, "ended before any byte")
		assert.Empty(t, c.Violations)
	})
	t.Run("srt", func(t *testing.T) {
		r := awaitSRT(t, readSRTAsync("srt://"+scriptedSRT(t, nil)+"?streamid=x", ReadOptions{Timeout: 5 * time.Second}))
		require.ErrorContains(t, r.err, "ended before any byte")
		assert.Empty(t, r.c.Violations)
	})
}

// PSI that first comes after more than 4 MiB of other packets (a third-party
// stream at a high bitrate that repeats its tables rarely) is found.
func TestReadTSLatePSI(t *testing.T) {
	null := make([]byte, mpegts.PacketSize)
	null[0], null[1], null[2], null[3] = 0x47, 0x1F, 0xFF, 0x10
	body := append(bytes.Repeat(null, (5<<20)/mpegts.PacketSize), muxTS(t, streamClip(t, "h264_aac.ts"))...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()
	c, err := ReadTS(context.Background(), srv.URL+"/x.ts", ReadOptions{Timeout: 10 * time.Second})
	require.NoError(t, err)
	assert.Empty(t, c.Violations)
	assert.NotEmpty(t, c.Frames)
}
