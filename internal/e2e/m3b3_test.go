package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	srt "github.com/datarhei/gosrt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/ingest"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/output/httpts"
	"github.com/ntt0601zcoder/open-streamer/internal/output/live"
	"github.com/ntt0601zcoder/open-streamer/internal/output/rtmppush"
	"github.com/ntt0601zcoder/open-streamer/internal/output/srtout"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/hls"
	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
)

// The M3b3 scenarios close the M3 matrix. TestM3Matrix fills the cells that
// no earlier scenario covers, each read back with conform and with ffprobe
// or ffmpeg: B-frames on the HTTP outputs, the slow consumer on HTTP-TS,
// and all four scenarios on SRT. TestHLSFFmpegAcrossGap and
// TestM3b3HLSAlignment run the live packager across a failover to a
// video-only source and back; TestM3b3MediaMTXPull has MediaMTX pull our
// HLS and SRT outputs; TestM3b3SRTFFmpeg has libsrt's ffmpeg decode our SRT
// output. The VLC-class clients are in vlc_test.go.

const (
	// m3b3SRTAddr and m3b3HTTPAddr are the fixed SRT and HTTP addresses
	// that the RTSP MediaMTX pulls its paths m3b3hls, m3b3hlsts and
	// m3b3srt from (CI's step and the local container set
	// MTX_PATHS_M3B3*_SOURCE to them).
	m3b3SRTAddr  = "127.0.0.1:19363"
	m3b3HTTPAddr = "127.0.0.1:19364"
	// m3b3App is the app of every SRT stream ID: live/<name>.
	m3b3App = "live"
	// m3b3Read bounds every read's set-up and every wait for the next data.
	m3b3Read = 15 * time.Second
	// The TS PIDs of tsstream's tracks 0 (video) and 1 (audio).
	videoPID = 256
	audioPID = 257
)

// chainAtEOF is ingest.Open for inputs that play one after the other, by
// URL: each input but the last opens once, only after the one before it
// has ended, and its end is a failure, so that the pipeline fails over to
// the next one; the last opens once the one before it has ended and ends
// the run at its end.
func chainAtEOF(urls ...string) func(ctx context.Context, u string) (ingest.Reader, error) {
	ended := make([]chan struct{}, len(urls))
	once := make([]sync.Once, len(urls))
	opened := make([]atomic.Bool, len(urls))
	for i := range ended {
		ended[i] = make(chan struct{})
	}
	return func(ctx context.Context, u string) (ingest.Reader, error) {
		i := slices.Index(urls, u)
		if i < 0 {
			return nil, fmt.Errorf("e2e: unknown input %s", u)
		}
		if i > 0 {
			select {
			case <-ended[i-1]:
			case <-ctx.Done():
				return nil, ctx.Err() //nolint:wrapcheck // the pipeline wraps it
			}
		}
		if opened[i].Swap(true) {
			return nil, errPrimaryEnded
		}
		r, err := ingest.Open(ctx, u, ingest.Env{})
		if err != nil || i == len(urls)-1 {
			return r, err //nolint:wrapcheck // the pipeline wraps it
		}
		return eofFails{Reader: r, ended: func() { once[i].Do(func() { close(ended[i]) }) }}, nil
	}
}

// audioOnlyClip writes the audio of h264_aac.ts, looped passes times, into
// a temporary TS file and returns its path.
func audioOnlyClip(t *testing.T, ffmpeg string, passes int) string {
	t.Helper()
	clip := loopClip(t, ffmpeg, "h264_aac.ts", passes, 0)
	out := strings.TrimSuffix(clip, ".ts") + "-audio.ts"
	b, err := exec.CommandContext(t.Context(), ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin",
		"-i", clip, "-map", "0:a", "-c", "copy", "-f", "mpegts", out).CombinedOutput()
	require.NoError(t, err, string(b))
	return out
}

// liveRun starts a run over inputs (open as in runOptions) with a live
// packager on the real clock, subscribed before the first write.
func liveRun(t *testing.T, inputs []string, open func(ctx context.Context, u string) (ingest.Reader, error)) (*m2aRun, *live.Packager) {
	t.Helper()
	var p *live.Packager
	done := make(chan struct{})
	r := startRun(t, runOptions{inputs: inputs, open: open, prepare: func(h *hub.Hub) {
		p = live.NewPackager(h, live.Options{})
		sub := h.Subscribe("packager", bigQueue)
		go func() {
			defer close(done)
			p.Run(context.Background(), sub)
		}()
	}})
	t.Cleanup(func() {
		r.cancel()
		r.wait()
		<-done
	})
	return r, p
}

// waitListed waits until the packager lists n segments.
func waitListed(t *testing.T, p *live.Packager, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return p.Snapshot().Listed >= n }, 30*time.Second, 20*time.Millisecond,
		"%d segments listed", n)
}

// hookListener passes every accepted connection to hook.
type hookListener struct {
	net.Listener
	hook func(net.Conn)
}

func (l hookListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.hook(c)
	}
	return c, err //nolint:wrapcheck // net.Listener contract
}

// serveHTTP serves the live packagers and HTTP-TS of the named streams on
// addr (a free port when empty) until the test ends, and returns the
// origin, http://host:port. queue is HTTP-TS's subscription queue (0 for
// the default); hook, when set, sees every accepted connection.
func serveHTTP(t *testing.T, addr string, hubs map[string]*hub.Hub, packs map[string]*live.Packager, queue int, hook func(net.Conn)) string {
	t.Helper()
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	l, err := new(net.ListenConfig).Listen(context.Background(), "tcp", addr)
	require.NoError(t, err)
	if hook != nil {
		l = hookListener{Listener: l, hook: hook}
	}
	mux := http.NewServeMux()
	mux.Handle("GET /live/{stream}/stream.ts", httpts.Handler(func(name string) (*hub.Hub, bool) {
		h, ok := hubs[name]
		return h, ok
	}, queue))
	mux.Handle("/live/", live.Handler(func(name string) (*live.Packager, bool) {
		p, ok := packs[name]
		return p, ok
	}))
	srv := &httptest.Server{Listener: l, Config: &http.Server{Handler: mux, ReadHeaderTimeout: m3b3Read}}
	srv.Start()
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	return srv.URL
}

// srtOut is an SRT output on its own listener.
type srtOut struct {
	srv  *srtout.Server
	addr string
}

// serveSRT serves hubs by stream name on addr (a free port when empty)
// until the test ends.
func serveSRT(t *testing.T, addr string, opt srtout.Options, hubs map[string]*hub.Hub) *srtOut {
	t.Helper()
	if addr == "" {
		addr = fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))
	}
	srv := srtout.New(func(name string) (*hub.Hub, bool) {
		h, ok := hubs[name]
		return h, ok
	}, opt)
	require.NoError(t, srv.Listen(addr))
	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()
	t.Cleanup(func() {
		assert.NoError(t, srv.Close())
		select {
		case err := <-served:
			assert.ErrorIs(t, err, srtout.ErrServerClosed)
		case <-time.After(m3b3Read):
			t.Error("Serve did not return after Close")
		}
	})
	return &srtOut{srv: srv, addr: srv.Addr().String()}
}

// url is the SRT URL of the stream s, with the plain stream ID.
func (o *srtOut) url() string {
	return "srt://" + o.addr + "?streamid=" + m3b3App + "/s"
}

// readTS reads u with conform.ReadTS until the end of the stream, in the
// background.
func readTS(u string) <-chan capResult {
	ch := make(chan capResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), runLimit)
		defer cancel()
		c, err := conform.ReadTS(ctx, u, conform.ReadOptions{Timeout: m3b3Read})
		ch <- capResult{c, err}
	}()
	return ch
}

// rawResult is the bytes one raw read received, and how it ended.
type rawResult struct {
	b   []byte
	err error
}

// rawSRT reads the bytes an SRT receiver of streamID at addr gets until the
// stream ends, in the background.
func rawSRT(addr, streamID string) <-chan rawResult {
	ch := make(chan rawResult, 1)
	go func() {
		cfg := srt.DefaultConfig()
		cfg.StreamId = streamID
		cfg.ConnectionTimeout = m3b3Read
		c, err := srt.Dial("srt", addr, cfg)
		if err != nil {
			ch <- rawResult{err: err}
			return
		}
		stop := time.AfterFunc(runLimit, func() { _ = c.Close() })
		defer stop.Stop()
		defer func() { _ = c.Close() }()
		var out []byte
		buf := make([]byte, 1500)
		for {
			n, err := c.Read(buf)
			out = append(out, buf[:n]...)
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				ch <- rawResult{b: out, err: err}
				return
			}
		}
	}()
	return ch
}

// rawHTTP reads the body of u until it ends, in the background.
func rawHTTP(u string) <-chan rawResult { return rawHTTPWith(http.DefaultClient, u) }

// rawHTTPWith is rawHTTP through the client c, which must have no Timeout.
func rawHTTPWith(c *http.Client, u string) <-chan rawResult {
	ch := make(chan rawResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), runLimit)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			ch <- rawResult{err: err}
			return
		}
		resp, err := c.Do(req)
		if err != nil {
			ch <- rawResult{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		ch <- rawResult{b: b, err: err}
	}()
	return ch
}

func waitRawResult(t *testing.T, ch <-chan rawResult) []byte {
	t.Helper()
	select {
	case r := <-ch:
		require.NoError(t, r.err)
		require.NotEmpty(t, r.b)
		return r.b
	case <-time.After(runLimit + m3b3Read):
		t.Fatal("the raw read did not end")
		return nil
	}
}

// conformBytes has conform.ReadTS read b, served over HTTP.
func conformBytes(t *testing.T, b []byte) conform.Capture {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), m3b3Read)
	defer cancel()
	c, err := conform.ReadTS(ctx, srv.URL+"/s.ts", conform.ReadOptions{Timeout: m3b3Read})
	require.NoError(t, err)
	return c
}

// decodeBytes has ffmpeg decode b with -xerror and returns its stderr.
func decodeBytes(t *testing.T, ffmpeg string, b []byte) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "capture.ts")
	require.NoError(t, os.WriteFile(f, b, 0o600))
	cmd := exec.CommandContext(t.Context(), ffmpeg, "-hide_banner", "-nostdin", "-v", "error", "-xerror",
		"-i", f, "-f", "null", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Run(), stderr.String())
	return stderr.String()
}

// tsFrames demuxes b with the ingest TS demuxer.
func tsFrames(t *testing.T, b []byte) []media.Frame {
	t.Helper()
	d, err := ts.NewDemuxer(bytes.NewReader(b))
	require.NoError(t, err)
	var out []media.Frame
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			return out
		}
		require.NoError(t, err)
		out = append(out, f)
	}
}

// patPMT returns the PMT PID of the first program a PAT payload lists, or
// -1.
func patPMT(payload []byte) int {
	if len(payload) < 1 {
		return -1
	}
	p := int(payload[0])
	if len(payload) < 1+p+12 {
		return -1
	}
	sec := payload[1+p:]
	end := min(3+(int(sec[1]&0x0F)<<8|int(sec[2]))-4, len(sec))
	for j := 8; j+4 <= end; j += 4 {
		if prog := int(sec[j])<<8 | int(sec[j+1]); prog != 0 {
			return int(sec[j+2]&0x1F)<<8 | int(sec[j+3])
		}
	}
	return -1
}

// pesHead returns the PTS of the PES that a payload starts (-1 when it has
// none) and the H.264 NAL unit types that start in the payload's data.
func pesHead(b []byte) (pts int64, nals []int) {
	if len(b) < 9 || b[0] != 0 || b[1] != 0 || b[2] != 1 {
		return -1, nil
	}
	pts = -1
	if b[7]&0x80 != 0 && len(b) >= 14 {
		pts = int64(b[9]>>1&7)<<30 | int64(b[10])<<22 | int64(b[11]>>1)<<15 | int64(b[12])<<7 | int64(b[13]>>1)
	}
	es := b[min(9+int(b[8]), len(b)):]
	for i := 0; i+3 < len(es); i++ {
		if es[i] == 0 && es[i+1] == 0 && es[i+2] == 1 {
			nals = append(nals, int(es[i+3]&0x1F))
		}
	}
	return pts, nals
}

// tsGaps checks, packet by packet, every resume of a TS stream after hub
// loss (a video packet with the discontinuity_indicator): a PAT and a PMT
// since the previous video packet, the indicator on a packet that starts a
// PES of an H.264 keyframe (SPS or IDR slice in its first packet), and the
// first audio packet after it carrying the indicator and starting a PES
// presented at or after the keyframe. It returns the number of resumes.
func tsGaps(t *testing.T, b []byte) (gaps int) {
	t.Helper()
	r := mpegts.NewReader(bytes.NewReader(b))
	pmtPID := -1
	var pat, pmt, awaitAudio bool
	var resume int64
	for {
		p, err := r.Next()
		if errors.Is(err, io.EOF) {
			return gaps
		}
		require.NoError(t, err)
		switch {
		case p.PID == 0:
			pat = true
			if pmtPID < 0 {
				pmtPID = patPMT(p.Payload)
			}
		case int(p.PID) == pmtPID:
			pmt = pat
		case p.PID == videoPID:
			if p.Discontinuity {
				gaps++
				require.True(t, pat && pmt, "resume %d: a PAT and a PMT before the keyframe", gaps)
				require.True(t, p.PUSI, "resume %d: the indicator on a PES start", gaps)
				pts, nals := pesHead(p.Payload)
				require.True(t, slices.Contains(nals, 5) || slices.Contains(nals, 7),
					"resume %d: a keyframe first (NAL types %v)", gaps, nals)
				resume, awaitAudio = pts, true
			}
			pat, pmt = false, false
		case p.PID == audioPID:
			if awaitAudio {
				require.True(t, p.Discontinuity, "resume %d: the indicator on the first audio packet", gaps)
				require.True(t, p.PUSI, "resume %d: audio resumes on a PES start", gaps)
				pts, _ := pesHead(p.Payload)
				require.GreaterOrEqual(t, pts, resume, "resume %d: audio presented before the keyframe is dropped", gaps)
				awaitAudio = false
			} else {
				require.False(t, p.Discontinuity, "an audio discontinuity only after a video resume")
			}
		}
	}
}

// assertTSMatchesHub checks that a TS reader that lost nothing read the
// hub's frames from a video keyframe to the end of the stream: per kind, the
// last frames received are the hub's last, with one timestamp offset over
// every frame (TS carries the hub's timestamps; a demuxer may rebase them),
// and the key flags.
func assertTSMatchesHub(t *testing.T, hubFrames, got []media.Frame) {
	t.Helper()
	hv, _ := byKind(hubFrames, media.KindVideo)
	gv, _ := byKind(got, media.KindVideo)
	require.NotEmpty(t, gv, "video received")
	require.LessOrEqual(t, len(gv), len(hv))
	k := len(hv) - len(gv)
	require.True(t, hv[k].Key, "the reader starts at hub video frame %d, a keyframe", k)
	off := hv[len(hv)-1].DTS - gv[len(gv)-1].DTS
	for i, f := range gv {
		w := hv[k+i]
		require.Equal(t, w.DTS-off, f.DTS, "video %d DTS", i)
		require.Equal(t, w.PTS-off, f.PTS, "video %d PTS", i)
		require.Equal(t, w.Key, f.Key, "video %d key", i)
	}
	ha, _ := byKind(hubFrames, media.KindAudio)
	ga, _ := byKind(got, media.KindAudio)
	require.LessOrEqual(t, len(ga), len(ha))
	ka := len(ha) - len(ga)
	for i, f := range ga {
		require.Equal(t, ha[ka+i].PTS-off, f.PTS, "audio %d PTS", i)
	}
	if len(ga) > 0 {
		t.Logf("joined at hub video %d (PTS %d), first audio at PTS %d", k, hv[k].PTS, ha[ka].PTS)
	}
}

// tsResumes checks the frames of a TS reader that read to the hub's end but
// lost frames on the way: every frame is a hub frame (by PTS, one offset
// over the stream, tsOffset), with the hub frame's DTS and key flag; the
// first video frame and the first after every gap are keyframes. The
// reader's last video frame lies in the hub's last GOP or the one before
// it: a loss there leaves no keyframe to resume at, so the tail can be
// missing, but a reader cut off earlier fails. It returns the number of
// gaps.
func tsResumes(t *testing.T, hubFrames, got []media.Frame) (gaps int) {
	t.Helper()
	hv, _ := byKind(hubFrames, media.KindVideo)
	gv, _ := byKind(got, media.KindVideo)
	require.NotEmpty(t, gv, "video received")
	at := map[int64]int{}
	for i, f := range hv {
		at[f.PTS] = i
	}
	off := tsOffset(t, hv, gv, at)
	prev := -1
	for i, f := range gv {
		j, ok := at[f.PTS+off]
		require.True(t, ok, "video %d (PTS %d) is a hub frame", i, f.PTS+off)
		require.Equal(t, hv[j].DTS-off, f.DTS, "video %d DTS", i)
		require.Equal(t, hv[j].Key, f.Key, "video %d key", i)
		switch {
		case i == 0:
			require.True(t, hv[j].Key, "the first frame is a keyframe")
		case j != prev+1:
			require.Greater(t, j, prev, "video %d in decode order", i)
			require.True(t, hv[j].Key, "the first frame after a gap is a keyframe (hub video %d after %d)", j, prev)
			gaps++
		}
		prev = j
	}
	var keys []int
	for i, f := range hv {
		if f.Key {
			keys = append(keys, i)
		}
	}
	tail := keys[max(0, len(keys)-2)]
	require.GreaterOrEqual(t, prev, tail,
		"the reader's last video frame (hub video %d of %d) is in the hub's last two GOPs (from hub video %d)",
		prev, len(hv), tail)
	ha, _ := byKind(hubFrames, media.KindAudio)
	pts := map[int64]bool{}
	for _, f := range ha {
		pts[f.PTS] = true
	}
	ga, _ := byKind(got, media.KindAudio)
	for i, f := range ga {
		require.True(t, pts[f.PTS+off], "audio %d (PTS %d) is a hub frame", i, f.PTS+off)
	}
	return gaps
}

// tsOffset returns the offset from a lossy TS reader's video timestamps to
// the hub's: the one under which every frame the reader got, from its first
// (a keyframe), is a hub frame with the same DTS and key flag. The reader's
// last frame is not aligned with the hub's, because a loss in the hub's
// last GOP leaves no keyframe to resume at and the reader's tail is
// missing; tsResumes bounds it. at maps each hub video PTS to its index in
// hv.
//
// It assumes that no run of the hub's GOPs repeats the reader's frames
// under another offset. A stream that loops a clip comes close: two
// offsets fit only when the reader got nothing from the last loop's span,
// and then the test fails. The assumption can cause a false failure, never
// a false pass.
func tsOffset(t *testing.T, hv, gv []media.Frame, at map[int64]int) int64 {
	t.Helper()
	var offs []int64
	for _, k := range hv {
		if !k.Key {
			continue
		}
		off := k.PTS - gv[0].PTS
		ok := true
		for _, f := range gv {
			j, found := at[f.PTS+off]
			if !found || hv[j].DTS-off != f.DTS || hv[j].Key != f.Key {
				ok = false
				break
			}
		}
		if ok {
			offs = append(offs, off)
		}
	}
	require.Len(t, offs, 1, "one hub keyframe aligns every video frame the reader got; offsets %v", offs)
	return offs[0]
}

// clockStepsData is clockSteps over the TS bytes data.
func clockStepsData(t *testing.T, data []byte, now func() time.Time) []step {
	t.Helper()
	d, err := ts.NewDemuxer(bytes.NewReader(data))
	require.NoError(t, err)
	c := clock.New(clock.Options{Kinds: d.TrackKinds(), Now: now})
	var out []step
	var anchored uint32
	add := func(frames []media.Frame) {
		for _, f := range frames {
			s := step{f: f}
			if f.Epoch > anchored {
				for _, e := range c.Epochs() {
					if e.ID > anchored {
						s.anchors = append(s.anchors, media.Anchor{Epoch: e.ID, Start: e.Start, Wall: e.Wall})
						anchored = e.ID
					}
				}
			}
			out = append(out, s)
		}
	}
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		add(c.Push(f))
	}
	add(c.Flush())
	return out
}

// burstFeed writes steps into h at their pace from now, except that once
// the stream time at has been written it waits for ready, writes the next
// burst of stream time at once and paces the rest from there. It closes h
// at the end; the channel it returns is closed then.
func burstFeed(ctx context.Context, h *hub.Hub, steps []step, at, burst time.Duration, ready <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer h.Close()
		first := steps[0].f.DTS
		base := time.Now()
		waited := false
		for _, s := range steps {
			pos := media.ToDuration(s.f.DTS - first)
			var due time.Time
			switch {
			case pos < at:
				due = base.Add(pos)
			case pos < at+burst:
				if !waited {
					waited = true
					select {
					case <-ready:
					case <-ctx.Done():
						return
					}
					base = time.Now()
				}
			default:
				due = base.Add(pos - at - burst)
			}
			if d := time.Until(due); d > 0 {
				select {
				case <-time.After(d):
				case <-ctx.Done():
					return
				}
			}
			s.write(h)
		}
	}()
	return done
}

// bgResult is what a function run by inBackground returned.
type bgResult[T any] struct {
	v   T
	err error
}

// inBackground runs f with a context bounded by runLimit in a goroutine and
// delivers its result on the channel it returns.
func inBackground[T any](f func(ctx context.Context) (T, error)) <-chan bgResult[T] {
	ch := make(chan bgResult[T], 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), runLimit)
		defer cancel()
		v, err := f(ctx)
		ch <- bgResult[T]{v, err}
	}()
	return ch
}

// probeTime is one packet as ffprobe reports it, in seconds.
type probeTime struct {
	pts, dts float64
	hasDTS   bool
	key      bool
}

// probeVideoTimes runs ffprobe on u, with extra options before it, until
// the stream ends and returns its video packets.
func probeVideoTimes(ctx context.Context, ffprobe, u string, extra ...string) ([]probeTime, error) {
	args := append([]string{
		"-v", "error", "-select_streams", "v",
		"-show_entries", "packet=pts_time,dts_time,flags", "-of", "json",
	}, extra...)
	out, err := exec.CommandContext(ctx, ffprobe, append(args, u)...).Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	var v struct {
		Packets []struct {
			PTS   string `json:"pts_time"`
			DTS   string `json:"dts_time"`
			Flags string `json:"flags"`
		} `json:"packets"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	pkts := make([]probeTime, 0, len(v.Packets))
	for _, p := range v.Packets {
		pts, err := strconv.ParseFloat(p.PTS, 64)
		if err != nil {
			return nil, fmt.Errorf("ffprobe: pts_time %q: %w", p.PTS, err)
		}
		dts, derr := strconv.ParseFloat(p.DTS, 64)
		pkts = append(pkts, probeTime{pts: pts, dts: dts, hasDTS: derr == nil, key: strings.HasPrefix(p.Flags, "K")})
	}
	return pkts, nil
}

// pyramidRef returns the video packets of h264_pyramid.ts from its
// reference file, in seconds.
func pyramidRef(t *testing.T) []probeTime {
	t.Helper()
	var out []probeTime
	sc := bufio.NewScanner(bytes.NewReader(fixtureBytes(t, "h264_pyramid_dts.txt")))
	for sc.Scan() {
		f := strings.Split(strings.TrimSpace(sc.Text()), ",")
		if len(f) < 3 {
			continue
		}
		pts, err := strconv.ParseInt(f[0], 10, 64)
		require.NoError(t, err)
		dts, err := strconv.ParseInt(f[1], 10, 64)
		require.NoError(t, err)
		out = append(out, probeTime{pts: float64(pts) / 90000, dts: float64(dts) / 90000, hasDTS: true, key: strings.HasPrefix(f[2], "K")})
	}
	require.NoError(t, sc.Err())
	require.NotEmpty(t, out)
	return out
}

// assertReorder checks ffprobe's video packets against want, aligned at
// their first packets when prefix is set, else at their last: per packet
// the key flag and the composition offset pts−dts within 1.5 ms, and the
// dts increasing. It returns how many packets have a composition offset.
func assertReorder(t *testing.T, want, got []probeTime, prefix bool) (reordered int) {
	t.Helper()
	require.LessOrEqual(t, len(got), len(want))
	w := want[len(want)-len(got):]
	if prefix {
		w = want[:len(got)]
	}
	prev := math.Inf(-1)
	for i, p := range got {
		require.True(t, p.hasDTS, "packet %d has a dts", i)
		require.Equal(t, w[i].key, p.key, "packet %d key", i)
		require.InDelta(t, w[i].pts-w[i].dts, p.pts-p.dts, 0.0015, "packet %d pts−dts", i)
		require.Greater(t, p.dts, prev, "packet %d dts increasing", i)
		prev = p.dts
		if p.pts-p.dts > 0.0005 {
			reordered++
		}
	}
	return reordered
}

// hubTimes returns the hub's video frames as ffprobe would report them, in
// seconds.
func hubTimes(frames []media.Frame) []probeTime {
	hv, _ := byKind(frames, media.KindVideo)
	out := make([]probeTime, 0, len(hv))
	for _, f := range hv {
		out = append(out, probeTime{pts: float64(f.PTS) / 90000, dts: float64(f.DTS) / 90000, hasDTS: true, key: f.Key})
	}
	return out
}

// probeBase packages and serves the fixture name as TestM3aFFprobe does,
// for ffprobe, until the test ends, and returns the stream's base URL.
func probeBase(t *testing.T, name string) string {
	t.Helper()
	data := fixtureBytes(t, name)
	var last int64
	for _, s := range clockSteps(t, name, time.Now) {
		last = max(last, s.f.PTS)
	}
	t0 := time.Now().Add(-media.ToDuration(last) - 5*time.Second)
	fd := pipelineFeed(data, func() time.Time { return t0 })
	p, _ := packFeed(t, fd, time.Now)
	return serve(t, p, fd)
}

// TestM3Matrix fills the M3 matrix cells that no earlier scenario covers
// (the audit is in the M3b3 report): each output read back with conform,
// and with ffprobe or ffmpeg.
//
//   - <http output>/b-frames: h264_pyramid.ts (a B-pyramid, decode delay
//     3) packaged and read back by conform from HLS fMP4, HLS TS, DASH and
//     HTTP-TS with every frame written (pipelineScenario), and ffprobe's
//     video packets of that output carrying the fixture's composition
//     offsets and key flags. Our MPD stays dynamic after the stream ends,
//     so ffmpeg's DASH demuxer, which starts at the first segment, would
//     wait for more: ffprobe reads the first 5 s of DASH.
//
//   - http-ts/slow-consumer: a client behind slowProxy stops reading for
//     4 s; it resumes after hub loss at a keyframe with the PAT and PMT,
//     the discontinuity_indicator on every PID, no delta after a gap and
//     no audio presented before the keyframe (tsGaps, tsResumes); conform
//     reads its bytes clean and ffmpeg decodes them with -xerror. A normal
//     client gets exactly the hub's frames.
//
//   - srt/<scenario>: steady state (H.264 and H.265), epoch change (to a
//     new resolution, and to a video-only source), slow consumer (a burst
//     of 6 s of stream written at once into the hub, past the queue of the
//     SRT viewers) and B-frames, read by conform.ReadTS with the SRT-only
//     strict rules, and by ffprobe or ffmpeg with libsrt.
//
//   - push-out/ffprobe: h264_pyramid.ts, looped twice, pushed to MediaMTX
//     and read back from it by ffprobe over RTMP: H.264 and AAC, video
//     packets with composition offsets. (The push-out's four scenarios
//     are read back with our RTMP pull and conform's frame rules in the
//     M3b1 scenarios.)
//
// The SRT viewers of the slow-consumer cell lose frames because the hub
// outruns their paced writers (srtout's token bucket), not because of the
// network: an SRT link slower than the stream ends the viewer as stalled
// instead (TestSRTStalledReceiverEnded).
func TestM3Matrix(t *testing.T) {
	t.Parallel()
	// One conform read of the four HTTP outputs and one packaging for
	// ffprobe serve the four b-frames cells.
	_, _, caps := pipelineScenario(t, fixtureBytes(t, "h264_pyramid.ts"))
	base := probeBase(t, "h264_pyramid.ts")
	for _, out := range []struct {
		name, path string
		extra      []string
	}{
		{"hls-fmp4", "cmaf/master.m3u8", nil},
		{"hls-ts", "hls/index.m3u8", nil},
		{"dash", "cmaf/index.mpd", []string{"-read_intervals", "%+5"}},
		{"http-ts", "stream.ts", nil},
	} {
		t.Run(out.name+"/b-frames", func(t *testing.T) {
			t.Parallel()
			ffprobe := testpeer.Require(t, "ffprobe")
			assert.Empty(t, caps[out.name].Violations)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			pkts, err := probeVideoTimes(ctx, ffprobe, base+out.path, out.extra...)
			require.NoError(t, err)
			want := pyramidRef(t)
			require.GreaterOrEqual(t, len(pkts), len(want)/2, "ffprobe read most of the clip")
			reordered := assertReorder(t, want, pkts, true)
			assert.Positive(t, reordered, "packets with a composition offset")
			t.Logf("%s: conform %s; ffprobe %d of %d video packets, %d reordered", out.name,
				kindCounts(caps[out.name].Frames), len(pkts), len(want), reordered)
		})
	}
	t.Run("http-ts/slow-consumer", func(t *testing.T) {
		testHTTPTSSlowConsumer(t, func(t *testing.T) *m2aRun {
			t.Helper()
			clip := loopClip(t, testpeer.Require(t, "ffmpeg"), "h264_aac.ts", 3, 0)
			return startRun(t, runOptions{inputs: []string{fileURL(clip, "")}})
		})
	})
	t.Run("srt/steady-state", func(t *testing.T) {
		t.Parallel()
		for _, tc := range srtSteadyClips {
			t.Run(tc.name, func(t *testing.T) {
				testSRTSteady(t, func(t *testing.T) *m2aRun {
					t.Helper()
					clip := loopClip(t, testpeer.Require(t, "ffmpeg"), tc.name, 2, 0)
					return startRun(t, runOptions{inputs: []string{fileURL(clip, "")}})
				}, tc.video, tc.probe)
			})
		}
	})
	t.Run("srt/epoch-change", testSRTEpochChange)
	t.Run("srt/slow-consumer", testSRTSlowConsumer)
	t.Run("srt/b-frames", testSRTBFrames)
	t.Run("push-out/ffprobe", testPushOutFFprobe)
}

// testHTTPTSSlowConsumer runs the http-ts/slow-consumer cell on the run
// that start starts: a single-epoch stream of about 18 s.
func testHTTPTSSlowConsumer(t *testing.T, start func(t *testing.T) *m2aRun) {
	t.Parallel()
	ffmpeg := testpeer.Require(t, "ffmpeg")
	r := start(t)
	origin := serveHTTP(t, "", map[string]*hub.Hub{"s": r.h}, nil, 32, shrinkSend)
	waitAV(t, r.h)
	normalCh := rawHTTP(origin + "/live/s/stream.ts")
	proxy := slowProxy(t, strings.TrimPrefix(origin, "http://"), 100<<10, 4*time.Second)
	slowCh := rawHTTP("http://" + proxy + "/live/s/stream.ts")

	r.check(t, []uint32{1})
	normal := waitRawResult(t, normalCh)
	slow := waitRawResult(t, slowCh)
	assert.Zero(t, tsGaps(t, normal))
	assertTSMatchesHub(t, r.col.frames, tsFrames(t, normal))
	resumes := tsGaps(t, slow)
	require.Positive(t, resumes, "the throttled client lost frames and resumed")
	sf := tsFrames(t, slow)
	gaps := tsResumes(t, r.col.frames, sf)
	assert.Equal(t, resumes, gaps, "a gap in the frames at every resume")
	for name, b := range map[string][]byte{"normal": normal, "throttled": slow} {
		c := conformBytes(t, b)
		assert.Empty(t, c.Violations, name)
		assert.Empty(t, decodeBytes(t, ffmpeg, b), name)
	}
	t.Logf("hub %s; normal %d bytes; throttled %d bytes, %s, %d resumes", kindCounts(r.col.frames),
		len(normal), len(slow), kindCounts(sf), resumes)
}

// srtSteadyClips are the srt/steady-state rows: the fixture looped twice,
// its video codec, and what ffprobe lists.
var srtSteadyClips = []struct {
	name  string
	video media.Codec
	probe []string
}{
	{"h264_aac.ts", media.CodecH264, []string{"video:h264", "audio:aac"}},
	{"h265_aac51.ts", media.CodecH265, []string{"video:hevc", "audio:aac"}},
}

// testSRTSteady runs the srt/steady-state cell on the run that start
// starts, a single-epoch stream with AAC audio and video in codec video,
// which ffprobe lists as probe.
func testSRTSteady(t *testing.T, start func(t *testing.T) *m2aRun, video media.Codec, probe []string) {
	t.Parallel()
	ffprobe := testpeer.Require(t, "ffprobe", "srt")
	r := start(t)
	o := serveSRT(t, "", srtout.Options{}, map[string]*hub.Hub{"s": r.h})
	waitAV(t, r.h)
	capCh := readTS(o.url())
	probeCh := inBackground(func(ctx context.Context) ([]string, error) {
		return probeStreams(ctx, ffprobe, o.url())
	})

	r.check(t, []uint32{1})
	c := waitCapture(t, capCh)
	assert.Empty(t, c.Violations)
	assertCodecs(t, c.Frames, video, media.CodecAAC)
	assertTSMatchesHub(t, r.col.frames, c.Frames)
	pr := <-probeCh
	require.NoError(t, pr.err)
	assert.ElementsMatch(t, probe, pr.v)
	require.Eventually(t, func() bool { return o.srv.Stats().Conns == 0 }, m3b3Read, 10*time.Millisecond)
	st := o.srv.Stats()
	assert.Zero(t, st.Resyncs)
	assert.Zero(t, st.EndedStalled)
	assert.Zero(t, st.EndedError)
	t.Logf("hub %s; read %s; ffprobe %v; srt %+v", kindCounts(r.col.frames), kindCounts(c.Frames), pr.v, st)
}

// widths returns the distinct video widths of frames, in order.
func widths(frames []media.Frame) []int {
	var out []int
	for _, f := range frames {
		if f.Kind() != media.KindVideo || f.Config == nil {
			continue
		}
		if len(out) == 0 || out[len(out)-1] != f.Config.Width {
			out = append(out, f.Config.Width)
		}
	}
	return out
}

func testSRTEpochChange(t *testing.T) {
	t.Parallel()
	ffmpeg := testpeer.FFmpegSRT(t)
	// ffmpeg decodes 15 s of stream: the 12 s primary and 3 s of the 6 s
	// backup. libsrt reports a sender's end of stream as an I/O error,
	// which -xerror makes fatal (TestM3b3SRTFFmpeg). An output's -t waits
	// for every stream of the output to reach it, so the audio of the
	// video-only row has an output of its own, which ends before the
	// failover.
	for _, tc := range []struct {
		backup string
		passes int
		out    []string
	}{
		{"h264_aac_640.ts", 1, []string{"-t", "15", "-f", "null", "-"}},
		{"h264_640.ts", 3, []string{"-map", "0:v", "-t", "15", "-f", "null", "-", "-map", "0:a", "-t", "9", "-f", "null", "-"}},
	} {
		t.Run(tc.backup, func(t *testing.T) {
			t.Parallel()
			primary := fileURL(loopClip(t, ffmpeg, "h264_aac.ts", 2, 0), "")
			r := startRun(t, runOptions{
				inputs: []string{primary, fileURL(loopClip(t, ffmpeg, tc.backup, tc.passes, 0), "")},
				open:   failAtEOF(primary),
			})
			o := serveSRT(t, "", srtout.Options{}, map[string]*hub.Hub{"s": r.h})
			waitAV(t, r.h)
			capCh := readTS(o.url())
			f := startFFmpeg(t, ffmpeg, append([]string{"-xerror", "-i", o.url()}, tc.out...)...)

			r.check(t, []uint32{1, 2})
			c := waitCapture(t, capCh)
			assert.Empty(t, c.Violations)
			assertTSMatchesHub(t, r.col.frames, c.Frames)
			assert.Equal(t, []int{320, 640}, widths(c.Frames))
			f.requireExited(t)
			assert.Empty(t, f.stderr.String(), "ffmpeg stderr")
			require.Eventually(t, func() bool { return o.srv.Stats().Conns == 0 }, m3b3Read, 10*time.Millisecond)
			st := o.srv.Stats()
			assert.Zero(t, st.EndedCodecChange)
			assert.Zero(t, st.Resyncs)
			assert.Equal(t, uint64(1), st.EndedHubClosed, "conform read to the end")
			t.Logf("hub %s; read %s; srt %+v", kindCounts(r.col.frames), kindCounts(c.Frames), st)
		})
	}
}

func testSRTSlowConsumer(t *testing.T) {
	t.Parallel()
	ffmpeg := testpeer.Require(t, "ffmpeg")
	data, err := os.ReadFile(loopClip(t, ffmpeg, "h264_aac.ts", 3, 0))
	require.NoError(t, err)
	steps := clockStepsData(t, data, time.Now)
	h := hub.New(hub.Options{})
	tap := collect(h, "tap")
	o := serveSRT(t, "", srtout.Options{Queue: 32}, map[string]*hub.Hub{"s": h})
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	fed := burstFeed(ctx, h, steps, 3*time.Second, 6*time.Second, ready)
	t.Cleanup(func() {
		cancel()
		<-fed
	})
	waitAV(t, h)
	capCh := readTS(o.url())
	rawCh := rawSRT(o.addr, m3b3App+"/s")
	require.Eventually(t, func() bool { return o.srv.Stats().Conns == 2 }, m3b3Read, 5*time.Millisecond, "two receivers")
	close(ready)

	select {
	case <-fed:
	case <-time.After(runLimit):
		t.Fatal("the feed did not end")
	}
	<-tap.done
	c := waitCapture(t, capCh)
	raw := waitRawResult(t, rawCh)
	require.Eventually(t, func() bool { return o.srv.Stats().Conns == 0 }, m3b3Read, 10*time.Millisecond)
	st := o.srv.Stats()
	assert.Empty(t, c.Violations)
	require.Positive(t, st.Resyncs, "the receivers lost frames")
	assert.Equal(t, uint64(2), st.EndedHubClosed, "both receivers read to the end")
	gapsConform := tsResumes(t, tap.frames, c.Frames)
	assert.Positive(t, gapsConform)
	resumes := tsGaps(t, raw)
	require.Positive(t, resumes, "the raw receiver resumed after hub loss")
	rf := tsFrames(t, raw)
	assert.Equal(t, resumes, tsResumes(t, tap.frames, rf), "a gap in the frames at every resume")
	assert.Empty(t, decodeBytes(t, ffmpeg, raw))
	assert.LessOrEqual(t, uint64(resumes+gapsConform), st.Resyncs)
	t.Logf("hub %s; conform %s with %d gaps; raw %s with %d resumes; srt %+v",
		kindCounts(tap.frames), kindCounts(c.Frames), gapsConform, kindCounts(rf), resumes, st)
}

func testSRTBFrames(t *testing.T) {
	t.Parallel()
	ffprobe := testpeer.Require(t, "ffprobe", "srt")
	r := startRun(t, runOptions{inputs: []string{fixtureFile(t, "h264_pyramid.ts")}})
	o := serveSRT(t, "", srtout.Options{}, map[string]*hub.Hub{"s": r.h})
	waitAV(t, r.h)
	capCh := readTS(o.url())
	probeCh := inBackground(func(ctx context.Context) ([]probeTime, error) {
		return probeVideoTimes(ctx, ffprobe, o.url())
	})

	r.check(t, []uint32{1})
	c := waitCapture(t, capCh)
	assert.Empty(t, c.Violations)
	assertTSMatchesHub(t, r.col.frames, c.Frames)
	pr := <-probeCh
	require.NoError(t, pr.err)
	require.NotEmpty(t, pr.v)
	reordered := assertReorder(t, hubTimes(r.col.frames), pr.v, false)
	assert.Positive(t, reordered, "packets with a composition offset")
	t.Logf("hub %s; read %s; ffprobe %d video packets, %d reordered", kindCounts(r.col.frames),
		kindCounts(c.Frames), len(pr.v), reordered)
}

func testPushOutFFprobe(t *testing.T) {
	t.Parallel()
	mtx := testpeer.MediaMTX(t)
	ffmpeg := testpeer.Require(t, "ffmpeg")
	ffprobe := testpeer.Require(t, "ffprobe", "rtmp")
	src := startRun(t, runOptions{inputs: []string{fileURL(loopClip(t, ffmpeg, "h264_pyramid.ts", 2, 0), "")}})
	u := "rtmp://" + mtx + "/" + mtxPath("m3b3-probe")
	p, err := rtmppush.New(src.h, u, rtmppush.Options{})
	require.NoError(t, err)
	done, perr := runPusher(t, p)
	go src.wait() // closes the hub at the source's end, which ends the push
	var streams []string
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s, err := probeStreams(ctx, ffprobe, u)
		streams = s
		return err == nil && len(s) == 2
	}, m3b1MTXReady, 100*time.Millisecond, "ffprobe reads the push target")
	assert.ElementsMatch(t, []string{"video:h264", "audio:aac"}, streams)
	ctx, cancel := context.WithTimeout(context.Background(), runLimit)
	defer cancel()
	pkts, err := probeVideoTimes(ctx, ffprobe, u)
	require.NoError(t, err)
	reordered, keys := 0, 0
	for _, pk := range pkts {
		if pk.pts-pk.dts > 0.0005 {
			reordered++
		}
		if pk.key {
			keys++
		}
	}
	assert.Positive(t, reordered, "packets with a composition offset")
	assert.Positive(t, keys, "keyframes")
	<-done
	require.NoError(t, *perr)
	src.check(t, []uint32{1})
	t.Logf("ffprobe %v; %d video packets, %d reordered, %d keys; push %+v", streams, len(pkts), reordered, keys, p.Stats())
}

// gapSource runs, through a live packager on the real clock, h264_aac.ts
// looped twice (12 s), then h264_640.ts looped three times (6 s of
// video-only 640x360), then h264_aac.ts looped av times, one input after
// the other (two failovers, three epochs), and serves the HTTP outputs as
// stream s. It returns the origin.
func gapSource(t *testing.T, ffmpeg string, av int) (*m2aRun, *live.Packager, string) {
	t.Helper()
	return failoverSource(t, ffmpeg, "h264_640.ts", 3, av)
}

// failoverSource is gapSource with mid, looped midPasses times, between
// the two runs of h264_aac.ts.
func failoverSource(t *testing.T, ffmpeg, mid string, midPasses, av int) (*m2aRun, *live.Packager, string) {
	t.Helper()
	inputs := []string{
		fileURL(loopClip(t, ffmpeg, "h264_aac.ts", 2, 0), ""),
		fileURL(loopClip(t, ffmpeg, mid, midPasses, 0), ""),
		fileURL(loopClip(t, ffmpeg, "h264_aac.ts", av, 0), ""),
	}
	r, p := liveRun(t, inputs, chainAtEOF(inputs...))
	origin := serveHTTP(t, "", map[string]*hub.Hub{"s": r.h}, map[string]*live.Packager{"s": p}, 0, nil)
	return r, p, origin
}

// framecrcLine is one frame of ffmpeg's framecrc output, in seconds.
type framecrcLine struct {
	stream int
	at     float64
}

// parseFramecrc parses ffmpeg's framecrc output: "#tb N: num/den" headers,
// then "stream, dts, pts, duration, size, crc" per frame.
func parseFramecrc(t *testing.T, out []byte) []framecrcLine {
	t.Helper()
	tb := map[int]float64{}
	var lines []framecrcLine
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if rest, ok := strings.CutPrefix(line, "#tb "); ok {
			idx, frac, _ := strings.Cut(rest, ":")
			num, den, _ := strings.Cut(strings.TrimSpace(frac), "/")
			i, err1 := strconv.Atoi(idx)
			n, err2 := strconv.ParseFloat(num, 64)
			d, err3 := strconv.ParseFloat(den, 64)
			require.NoError(t, errors.Join(err1, err2, err3), line)
			tb[i] = n / d
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ",")
		require.GreaterOrEqual(t, len(f), 3, line)
		s, err := strconv.Atoi(strings.TrimSpace(f[0]))
		require.NoError(t, err, line)
		pts, err := strconv.ParseInt(strings.TrimSpace(f[2]), 10, 64)
		require.NoError(t, err, line)
		lines = append(lines, framecrcLine{stream: s, at: float64(pts) * tb[s]})
	}
	require.NoError(t, sc.Err())
	return lines
}

// largestGap returns the largest step between consecutive times of stream
// s (sorted), with the times around it.
func largestGap(lines []framecrcLine, s int) (gap, from, to float64, n int) {
	var at []float64
	for _, l := range lines {
		if l.stream == s {
			at = append(at, l.at)
		}
	}
	slices.Sort(at)
	for i := 1; i < len(at); i++ {
		if d := at[i] - at[i-1]; d > gap {
			gap, from, to = d, at[i-1], at[i]
		}
	}
	return gap, from, to, len(at)
}

// TestHLSFFmpegAcrossGap has ffmpeg read the HLS fMP4 master of a live
// stream across a video-only stretch (gapSource: 12 s of A/V, 6 s of video
// only, 12 s of A/V), from its first listed segment until EXT-X-ENDLIST,
// decoding both streams to framecrc: ffmpeg exits 0, audio is decoded
// before the stretch and after it, with the stretch as its only hole, and
// video throughout. Its warnings (the gap entries it may try to load) are
// recorded.
//
// The reader is ffmpeg 8 or later (testpeer.FFmpeg8; CI's is a pinned
// static build), not the distribution's ffmpeg: ffmpeg 6.1 (Ubuntu 24.04's)
// loads gap entries' URIs, which fail, and its HLS demuxer then reads no
// more of that rendition, so the audio ends at the stretch. It does the same
// with a VOD playlist and segments written by its own hls muxer, three audio
// segments missing and no EXT-X-GAP or EXT-X-DISCONTINUITY (259 of 863
// audio frames, none after the first missing one), where ffmpeg 8 resumes
// after the hole. 7 is untried.
func TestHLSFFmpegAcrossGap(t *testing.T) {
	t.Parallel()
	ffmpeg := testpeer.Require(t, "ffmpeg")
	ffmpeg8 := testpeer.FFmpeg8(t)
	r, p, origin := gapSource(t, ffmpeg, 2)
	waitListed(t, p, 2)
	ctx, cancel := context.WithTimeout(context.Background(), runLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg8, "-hide_banner", "-nostdin", "-v", "warning",
		"-live_start_index", "0", "-i", origin+"/live/s/cmaf/master.m3u8",
		"-map", "0:v", "-map", "0:a", "-f", "framecrc", "-")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	r.check(t, []uint32{1, 2, 3})
	var err error
	select {
	case err = <-done:
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("ffmpeg did not exit after the stream ended; stderr %s", stderr.String())
	}
	require.NoError(t, err, stderr.String())
	lines := parseFramecrc(t, stdout.Bytes())
	agap, afrom, ato, an := largestGap(lines, 1)
	vgap, _, _, vn := largestGap(lines, 0)
	var before, after, during int
	for _, l := range lines {
		switch {
		case l.stream == 1 && l.at <= afrom:
			before++
		case l.stream == 1 && l.at >= ato:
			after++
		case l.stream == 0 && l.at > afrom && l.at < ato:
			during++
		}
	}
	t.Logf("ffmpeg: %d video frames (largest step %.3f s), %d audio frames: %d before %.3f s, %d from %.3f s (hole %.3f s); %d video frames in the hole; stderr:\n%s",
		vn, vgap, an, before, afrom, after, ato, agap, during, stderr.String())
	assert.InDelta(t, 6, agap, 1.5, "the audio hole is the video-only stretch")
	assert.Greater(t, before, 2*gopAudio, "audio decoded before the stretch")
	assert.Greater(t, after, 2*gopAudio, "audio decoded after the stretch")
	assert.Greater(t, during, 4*gopVideo, "video decoded through the stretch")
	assert.Less(t, vgap, 1.0, "no hole in the video")
}

// hlsEntry is what one playlist lists for one media sequence number.
type hlsEntry struct {
	uri, mapURI string
	dur         time.Duration
	disc, gap   bool
}

// TestM3b3HLSAlignment runs gapSource (12 s of A/V, 6 s of video only, 18 s
// of A/V; a 6-segment window, so the gaps slide out of it) and refreshes
// the HLS fMP4 playlists every 200 ms until EXT-X-ENDLIST. At every refresh
// taken while the packager's window did not change, the renditions have
// equal MEDIA-SEQUENCE, DISCONTINUITY-SEQUENCE, entry counts and
// discontinuity flags; no listed entry ever changes between refreshes (RFC
// 8216 section 6.2.1); the audio rendition lists gap entries during the
// stretch, which answer 404, and none once the window has slid past them.
// conform.ReadHLS, polling with AlignedSequences, reads the stream clean.
func TestM3b3HLSAlignment(t *testing.T) {
	t.Parallel()
	ffmpeg := testpeer.Require(t, "ffmpeg")
	r, p, origin := gapSource(t, ffmpeg, 3)
	base := origin + "/live/s/cmaf/"
	waitListed(t, p, 1)
	capCh := make(chan capResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), runLimit)
		defer cancel()
		// A polling read stops at EXT-X-ENDLIST or once Duration is
		// captured, so Duration is longer than the stream.
		c, err := conform.ReadHLS(ctx, base+"master.m3u8",
			conform.ReadOptions{AlignedSequences: true, Timeout: m3b3Read, Duration: runLimit})
		capCh <- capResult{c, err}
	}()
	go r.wait() // closes the hub at the source's end, which ends the playlists

	seen := map[string]map[uint64]hlsEntry{}
	type window struct {
		first  uint64
		listed int
		ended  bool
	}
	key := func() window {
		s := p.Snapshot()
		w := window{listed: s.Listed, ended: s.Ended}
		if len(s.Segments) > 0 {
			w.first = s.Segments[0].Seq
		}
		return w
	}
	var refreshes, skipped, maxGaps, lastGaps int
	gap404 := false
	deadline := time.Now().Add(runLimit)
	for time.Now().Before(deadline) {
		before := key()
		m, err := hls.ParseMultivariant(get(t, base+"master.m3u8"))
		require.NoError(t, err)
		pls := map[string]hls.MediaPlaylist{}
		var names []string
		for _, v := range m.Variants {
			names = append(names, v.URI)
		}
		for _, rd := range m.Renditions {
			if rd.URI != "" {
				names = append(names, rd.URI)
			}
		}
		for _, n := range names {
			pls[n] = mediaPlaylist(t, base+n)
		}
		if key() != before {
			skipped++
			continue
		}
		refreshes++
		ended := true
		gaps := 0
		ref := pls[names[0]]
		for _, n := range names {
			pl := pls[n]
			ended = ended && pl.Ended
			require.Equal(t, ref.MediaSequence, pl.MediaSequence, "refresh %d: %s MEDIA-SEQUENCE", refreshes, n)
			require.Equal(t, ref.DiscontinuitySequence, pl.DiscontinuitySequence, "refresh %d: %s DISCONTINUITY-SEQUENCE", refreshes, n)
			require.Len(t, pl.Segments, len(ref.Segments), "refresh %d: %s entries", refreshes, n)
			if seen[n] == nil {
				seen[n] = map[uint64]hlsEntry{}
			}
			for i, s := range pl.Segments {
				require.Equal(t, ref.Segments[i].Discontinuity, s.Discontinuity, "refresh %d: %s entry %d discontinuity", refreshes, n, i)
				msn := pl.MediaSequence + uint64(i)
				e := hlsEntry{uri: s.URI, mapURI: s.Map, dur: s.Duration, disc: s.Discontinuity, gap: s.Gap}
				if old, ok := seen[n][msn]; ok {
					require.Equal(t, old, e, "refresh %d: %s entry %d changed", refreshes, n, msn)
				}
				seen[n][msn] = e
				if s.Gap {
					gaps++
					if !gap404 {
						resp, err := http.Get(base + s.URI) //nolint:noctx // test against a local server
						require.NoError(t, err)
						_ = resp.Body.Close()
						assert.Equal(t, http.StatusNotFound, resp.StatusCode, s.URI)
						gap404 = true
					}
				}
			}
		}
		maxGaps, lastGaps = max(maxGaps, gaps), gaps
		if ended {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	r.check(t, []uint32{1, 2, 3})
	t.Logf("%d refreshes (%d skipped while the window moved); at most %d gap entries listed, %d at the end; playlists %v",
		refreshes, skipped, maxGaps, lastGaps, slices.Sorted(maps.Keys(seen)))
	assert.Greater(t, refreshes, 50)
	assert.Positive(t, maxGaps, "gap entries during the video-only stretch")
	assert.True(t, gap404, "a gap URI answered")
	assert.Zero(t, lastGaps, "the window slid past the gaps")
	select {
	case res := <-capCh:
		require.NoError(t, res.err)
		assert.Empty(t, res.c.Violations)
		assert.Positive(t, res.c.HLSGaps)
		t.Logf("conform: %d frames %s, %d gap entries", len(res.c.Frames), kindCounts(res.c.Frames), res.c.HLSGaps)
	case <-time.After(m3b3Read):
		t.Fatal("conform.ReadHLS did not end")
	}
}

// TestM3b3MediaMTXPull has MediaMTX pull, on demand, our HLS fMP4 (path
// m3b3hls), HLS TS (m3b3hlsts) and SRT (m3b3srt) outputs of h264_aac.ts
// looped four times, from the fixed ports 19364 (HTTP) and 19363 (SRT), and
// our RTSP pull read each path over TCP: one epoch of violation-free frames
// (the tracks' start offset within mtxPullStartOffset) with no offset jump.
// DASH and HTTP-TS are not MediaMTX sources (ruling R-PF2). Each path is
// warmed with a throwaway reader and joined 1 s later (settleRTSP); an HLS
// path is warmed 1 s after a segment is listed, and an HLS row whose read
// MediaMTX still ends runs once more (the retry is logged).
func TestM3b3MediaMTXPull(t *testing.T) {
	t.Parallel()
	mtx := testpeer.MediaMTXPull(t)
	ffmpeg := testpeer.Require(t, "ffmpeg")
	// The rows run one after the other on the fixed ports.
	for _, path := range []string{"m3b3hls", "m3b3hlsts", "m3b3srt"} {
		t.Run(path, func(t *testing.T) {
			// An HLS row that hits gohlslib's reload phase (below) despite
			// the offset, seen as MediaMTX ending our read, runs once more;
			// each attempt is a subtest, so that its ports are free after it.
			hls := path != "m3b3srt"
			for n := 1; ; n++ {
				retry := false
				t.Run(fmt.Sprintf("attempt-%d", n), func(t *testing.T) {
					retry = mediaMTXPullRow(t, mtx, ffmpeg, path, hls && n == 1)
				})
				if !retry {
					return
				}
				t.Logf("%s: MediaMTX's HLS source stopped during attempt %d (gohlslib's reload phase): retrying once", path, n)
			}
		})
	}
}

// mtxPullStartOffset is the track-start-offset bound of the MediaMTX pull
// rows: the default 40 ms plus 160 ms, h264_aac.ts's largest video PTS −
// DTS (14400 ticks). MediaMTX's RTSP timing of these sources places audio
// that much late against video: with the hub's frames matched by payload to
// what our reader got, every frame of a row reads with audio 155 to 160 ms
// later relative to video than in the hub, on the HLS fMP4, HLS TS and SRT
// sources alike, before and after the segmenter's audio cut (ruling R-F3).
// The PLAY response's RTP-Info and the sender reports both carry it, so a
// receiver that follows them, as RFC 3550 asks, keeps it; that matches
// MediaMTX timing video by decode time while its RTP timestamps carry the
// PTS. A reader that joins up to 160 ms before a keyframe, and so keeps the
// audio just ahead of it, sees its audio start up to 160 ms after its
// video. The other rules, the single epoch and the absence of offset jumps
// still hold exactly.
const mtxPullStartOffset = 3600 + 14400

// mediaMTXPullRow runs one row of TestM3b3MediaMTXPull. When mayRetry is
// set and MediaMTX ended our read (unexpected EOF, its source stopped), it
// returns true without checking.
func mediaMTXPullRow(t *testing.T, mtx, ffmpeg, path string, mayRetry bool) (retry bool) {
	t.Helper()
	clip := loopClip(t, ffmpeg, "h264_aac.ts", 4, 0)
	src, p := liveRun(t, []string{fileURL(clip, "")}, nil)
	hubs := map[string]*hub.Hub{"m3b3": src.h, "m3b3srt": src.h}
	serveHTTP(t, m3b3HTTPAddr, hubs, map[string]*live.Packager{"m3b3": p}, 0, nil)
	o := serveSRT(t, m3b3SRTAddr, srtout.Options{}, hubs)
	waitAV(t, src.h)
	if path != "m3b3srt" {
		// MediaMTX's HLS client (gohlslib) treats a reload that finds no
		// new segment as an error, instead of retrying after half the
		// target duration (RFC 8216 section 6.3.4), and reloads one target
		// duration after its first load. A segment is listed every 2 s:
		// starting the client 1 s after a listing keeps its reloads off
		// the listing instants.
		waitListed(t, p, 3)
		time.Sleep(time.Second)
	}
	u := "rtsp://" + mtx + "/" + path + "?transport=tcp"
	settleRTSP(t, u)
	var rl readerLog
	r := startRun(t, runOptions{inputs: []string{u}, open: rl.wrap(nil)})
	r.waitActive(t, 0, rtspReady)
	time.Sleep(10 * time.Second)
	r.cancel()
	r.wait()
	if mayRetry && slices.ContainsFunc(r.failures(), func(f string) bool { return strings.Contains(f, "unexpected EOF") }) {
		t.Logf("read %s; failures %v", r.col.counts(), r.failures())
		return true
	}
	r.checkWith(t, []uint32{1}, conform.FrameOptions{MaxStartOffset: mtxPullStartOffset})
	assertCodecs(t, r.col.frames, media.CodecH264, media.CodecAAC)
	st := rl.stats()
	assert.Zero(t, st["sync_jumps"], "offset jumps")
	t.Logf("read %s over %v; clock %+v; reader %v; srt %+v; failures %v", r.col.counts(),
		r.col.span().Round(time.Millisecond), r.stats.Clock, st, o.srv.Stats(), r.failures())
	return false
}

// srtEOFLine is the line ffmpeg with libsrt writes, with -v error, when the
// SRT sender ends the stream: libsrt reports the sender's shutdown as an I/O
// error, for any sender (ffmpeg's own libsrt listener included). ffmpeg 6.1
// follows it with srtEOFLine61, again for any sender; ffmpeg 8 writes
// srtEOFLine alone.
const (
	srtEOFLine   = "Error during demuxing: Input/output error"
	srtEOFLine61 = "Error retrieving a packet from demuxer: Input/output error"
)

// TestM3b3SRTFFmpeg has ffmpeg with libsrt, an SRT receiver independent of
// gosrt, read our SRT output of h264_aac.ts looped twice (12 s) with the
// plain stream ID, twice at once: with -xerror -v error for 10 s of stream,
// it exits 0 and writes nothing to stderr; with -v error until the stream
// ends, it exits 0 and its stderr is the end-of-stream line alone
// (srtEOFLine, followed by srtEOFLine61 on ffmpeg 6.1), which -xerror would
// make fatal.
func TestM3b3SRTFFmpeg(t *testing.T) {
	t.Parallel()
	ffmpeg := testpeer.FFmpegSRT(t)
	clip := loopClip(t, ffmpeg, "h264_aac.ts", 2, 0)
	r := startRun(t, runOptions{inputs: []string{fileURL(clip, "")}})
	o := serveSRT(t, "", srtout.Options{}, map[string]*hub.Hub{"s": r.h})
	waitAV(t, r.h)
	strict := startFFmpeg(t, ffmpeg, "-xerror", "-t", "10", "-i", o.url(), "-f", "null", "-")
	toEnd := startFFmpeg(t, ffmpeg, "-i", o.url(), "-f", "null", "-")
	r.check(t, []uint32{1})
	strict.requireExited(t)
	toEnd.requireExited(t)
	assert.Empty(t, strict.stderr.String(), "ffmpeg -xerror stderr")
	var lines []string
	for l := range strings.SplitSeq(strings.TrimSpace(toEnd.stderr.String()), "\n") {
		lines = append(lines, l)
	}
	require.True(t, len(lines) == 1 || len(lines) == 2, "ffmpeg stderr %q", toEnd.stderr.String())
	assert.Contains(t, lines[0], srtEOFLine)
	if len(lines) == 2 {
		assert.Contains(t, lines[1], srtEOFLine61)
	}
	require.Eventually(t, func() bool { return o.srv.Stats().Conns == 0 }, m3b3Read, 10*time.Millisecond)
	st := o.srv.Stats()
	assert.Equal(t, uint64(1), st.EndedHubClosed, "the second receiver read to the end")
	assert.Zero(t, st.Resyncs)
	t.Logf("hub %s; srt %+v; stderr to the end %q", kindCounts(r.col.frames), st, toEnd.stderr.String())
}
