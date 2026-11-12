package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/output/httpts"
	"github.com/ntt0601zcoder/open-streamer/internal/output/live"
	"github.com/ntt0601zcoder/open-streamer/internal/pipeline"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/dash"
	"github.com/ntt0601zcoder/open-streamer/pkg/hls"
)

// m3aT0 is the fake wallclock every epoch is anchored at.
var m3aT0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	m3aWindow = 100     // holds every segment of every fixture
	bigQueue  = 1 << 16 // holds a whole clip
)

// fakeClock is a settable wallclock safe for concurrent use.
type fakeClock struct{ ns atomic.Int64 }

func newFakeClock(t time.Time) *fakeClock {
	c := &fakeClock{}
	c.Set(t)
	return c
}

func (c *fakeClock) Now() time.Time  { return time.Unix(0, c.ns.Load()).UTC() }
func (c *fakeClock) Set(t time.Time) { c.ns.Store(t.UnixNano()) }

func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", name))
	require.NoError(t, err)
	return b
}

// feed writes one stream into a hub and returns at its end; it does not
// close the hub.
type feed func(ctx context.Context, h *hub.Hub) error

// pipelineFeed runs the unpaced pipeline over data, anchored on now.
func pipelineFeed(data []byte, now func() time.Time) feed {
	return func(ctx context.Context, h *hub.Hub) error {
		return pipeline.Run(ctx, h, pipeline.Options{
			Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
			Now:  now,
		})
	}
}

// step is one frame of Clock output and the anchors to record before it.
type step struct {
	anchors []media.Anchor
	f       media.Frame
}

// clockSteps runs a fixture through demux and a Clock whose wallclock is
// now, recording each epoch's anchor before its first frame, as the
// pipeline does.
func clockSteps(t *testing.T, name string, now func() time.Time) []step {
	t.Helper()
	d, err := ts.NewDemuxer(bytes.NewReader(fixtureBytes(t, name)))
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

// write records the step's anchors, then writes its frame.
func (s step) write(h *hub.Hub) {
	for _, a := range s.anchors {
		h.SetAnchor(a)
	}
	h.Write(s.f)
}

func stepsFeed(steps []step) feed {
	return func(ctx context.Context, h *hub.Hub) error {
		for _, s := range steps {
			if err := ctx.Err(); err != nil {
				return err
			}
			s.write(h)
		}
		return nil
	}
}

// packFeed packages a feed with a whole-clip packager subscriber running
// concurrently with the writes. It returns the packager and every frame
// written to the hub.
func packFeed(t *testing.T, fd feed, now func() time.Time) (*live.Packager, []media.Frame) {
	t.Helper()
	h := hub.New(hub.Options{})
	sub := h.Subscribe("packager", bigQueue)
	tap := h.Subscribe("tap", bigQueue)
	p := live.NewPackager(h, live.Options{Window: m3aWindow, Now: now})
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(context.Background(), sub)
	}()
	require.NoError(t, fd(context.Background(), h))
	h.Close()
	<-done
	var written []media.Frame
	for dl := range tap.C() {
		require.Zero(t, dl.Lost)
		written = append(written, dl.Frame)
	}
	require.Zero(t, p.Errors())
	require.Zero(t, p.FallbackAnchors())
	return p, written
}

// advance moves clk to the wall where the window's last media ends plus
// one second, so every segment is listed.
func advance(t *testing.T, p *live.Packager, clk *fakeClock) time.Duration {
	t.Helper()
	s := p.Snapshot()
	require.NotEmpty(t, s.Segments)
	var end time.Time
	for _, seg := range s.Segments {
		e, ok := s.Epoch(seg.Epoch)
		require.True(t, ok)
		if w := e.WallAt(seg.End); w.After(end) {
			end = w
		}
	}
	content := end.Sub(m3aT0)
	clk.Set(end.Add(time.Second))
	s = p.Snapshot()
	require.Len(t, s.Segments, s.Listed, "every segment is listed")
	return content
}

// serve serves p and, for stream.ts, a fresh hub per request: a goroutine
// waits until the handler has subscribed, writes fd into the hub and closes
// it. It returns the stream's base URL.
func serve(t *testing.T, p *live.Packager, fd feed) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	hubLookup := func(name string) (*hub.Hub, bool) {
		if name != "s" {
			return nil, false
		}
		h := hub.New(hub.Options{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer h.Close()
			for len(h.Stats()) == 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Millisecond):
				}
			}
			if err := fd(ctx, h); err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("http-ts feed: %v", err)
			}
		}()
		return h, true
	}
	mux := http.NewServeMux()
	mux.Handle("GET /live/{stream}/stream.ts", httpts.Handler(hubLookup, 0))
	mux.Handle("/live/", live.Handler(func(name string) (*live.Packager, bool) { return p, name == "s" }))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/live/s/"
}

var formats = []string{"hls-ts", "hls-fmp4", "dash", "http-ts"}

// readAll reads the four outputs back and checks that none has a violation.
func readAll(t *testing.T, base string) map[string]conform.Capture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	once := conform.ReadOptions{Once: true}
	out := map[string]conform.Capture{}
	for _, f := range formats {
		var (
			c   conform.Capture
			err error
		)
		start := time.Now()
		switch f {
		case "hls-ts":
			c, err = conform.ReadHLS(ctx, base+"hls/index.m3u8", once)
		case "hls-fmp4":
			c, err = conform.ReadHLS(ctx, base+"cmaf/master.m3u8", once)
		case "dash":
			c, err = conform.ReadDASH(ctx, base+"cmaf/index.mpd", once)
		case "http-ts":
			c, err = conform.ReadTS(ctx, base+"stream.ts", conform.ReadOptions{})
		}
		require.NoError(t, err, f)
		assert.Empty(t, c.Violations, f)
		assert.NotEmpty(t, c.Frames, f)
		t.Logf("%-8s %4d frames in %v; per epoch/kind %s; violations %v",
			f, len(c.Frames), time.Since(start).Round(time.Millisecond), kindCounts(c.Frames), c.Violations)
		out[f] = c
	}
	return out
}

// epochKind names one kind of track within one epoch.
type epochKind struct {
	epoch uint32
	kind  media.Kind
}

type kindMap map[epochKind]int

func (m kindMap) String() string {
	keys := make([]epochKind, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].epoch != keys[j].epoch {
			return keys[i].epoch < keys[j].epoch
		}
		return keys[i].kind < keys[j].kind
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("e%d/%v=%d", k.epoch, k.kind, m[k]))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// kindCounts counts frames per epoch and kind.
func kindCounts(frames []media.Frame) kindMap {
	out := kindMap{}
	for _, f := range frames {
		out[epochKind{f.Epoch, f.Kind()}]++
	}
	return out
}

// flat counts frames per kind, every epoch as epoch 1.
func flat(m kindMap) kindMap {
	out := kindMap{}
	for k, n := range m {
		out[epochKind{1, k.kind}] += n
	}
	return out
}

// assertComplete checks that each output carries every frame written: HLS
// and DASH per epoch (their epochs follow the packager's), HTTP-TS per kind.
func assertComplete(t *testing.T, caps map[string]conform.Capture, written []media.Frame) {
	t.Helper()
	want := kindCounts(written)
	for _, f := range formats {
		got := kindCounts(caps[f].Frames)
		if f == "http-ts" {
			assert.Equal(t, flat(want), got, f)
		} else {
			assert.Equal(t, want, got, f)
		}
	}
}

// pipelineScenario packages and serves a pipeline run over data, then reads
// every output back.
func pipelineScenario(t *testing.T, data []byte) (*live.Packager, string, map[string]conform.Capture) {
	t.Helper()
	start := time.Now()
	clk := newFakeClock(m3aT0)
	fd := pipelineFeed(data, clk.Now)
	p, written := packFeed(t, fd, clk.Now)
	content := advance(t, p, clk)
	t.Logf("written %d frames %s, content %v, %d segments, %d epochs",
		len(written), kindCounts(written), content, len(p.Snapshot().Segments), len(p.Snapshot().Epochs))
	base := serve(t, p, fd)
	caps := readAll(t, base)
	assertComplete(t, caps, written)
	t.Logf("scenario took %v", time.Since(start).Round(time.Millisecond))
	return p, base, caps
}

func get(t *testing.T, u string) []byte {
	t.Helper()
	resp, err := http.Get(u) //nolint:noctx // test against a local server
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode, u)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return b
}

func mediaPlaylist(t *testing.T, u string) hls.MediaPlaylist {
	t.Helper()
	pl, err := hls.ParseMedia(get(t, u))
	require.NoError(t, err)
	return pl
}

// cmafPlaylists returns the URLs of the fMP4 media playlists the master
// lists: variants first, then renditions.
func cmafPlaylists(t *testing.T, base string) []string {
	t.Helper()
	m, err := hls.ParseMultivariant(get(t, base+"cmaf/master.m3u8"))
	require.NoError(t, err)
	var out []string
	for _, v := range m.Variants {
		out = append(out, base+"cmaf/"+v.URI)
	}
	for _, r := range m.Renditions {
		if r.URI != "" {
			out = append(out, base+"cmaf/"+r.URI)
		}
	}
	require.NotEmpty(t, out)
	return out
}

func discontinuities(pl hls.MediaPlaylist) int {
	n := 0
	for _, s := range pl.Segments {
		if s.Discontinuity {
			n++
		}
	}
	return n
}

func mpd(t *testing.T, base string) dash.MPD {
	t.Helper()
	m, err := dash.Parse(get(t, base+"cmaf/index.mpd"))
	require.NoError(t, err)
	return m
}

func TestM3aSteady(t *testing.T) {
	for _, name := range []string{"h264_aac.ts", "h265_aac51.ts"} {
		t.Run(name, func(t *testing.T) {
			p, base, _ := pipelineScenario(t, fixtureBytes(t, name))
			assert.Len(t, p.Snapshot().Epochs, 1)
			assert.Zero(t, discontinuities(mediaPlaylist(t, base+"hls/index.m3u8")))
			assert.Len(t, mpd(t, base).Periods, 1)
		})
	}
}

func TestM3aLoop(t *testing.T) {
	p, base, caps := pipelineScenario(t, fixtureBytes(t, "h264_aac_loop.ts"))
	assert.Len(t, p.Snapshot().Epochs, 2)
	assert.Equal(t, 1, discontinuities(mediaPlaylist(t, base+"hls/index.m3u8")), "hls-ts")
	for _, u := range cmafPlaylists(t, base) {
		assert.Equal(t, 1, discontinuities(mediaPlaylist(t, u)), u)
	}
	assert.Len(t, mpd(t, base).Periods, 2)
	for _, f := range []string{"hls-ts", "hls-fmp4", "dash"} {
		assert.Equal(t, []uint32{1, 2}, epochs(caps[f].Frames), f)
	}
}

func TestM3aConfigChange(t *testing.T) {
	p, base, caps := pipelineScenario(t, fixtureBytes(t, "h264_aac_then_640.ts"))
	snap := p.Snapshot()
	require.Len(t, snap.Epochs, 2)

	// HLS-fMP4: the discontinuity of the video playlist switches EXT-X-MAP.
	video := mediaPlaylist(t, base+"cmaf/v0.m3u8")
	maps := map[string]bool{}
	for i, s := range video.Segments {
		maps[s.Map] = true
		if s.Discontinuity {
			require.Positive(t, i)
			assert.NotEqual(t, video.Segments[i-1].Map, s.Map, "a new EXT-X-MAP with the discontinuity")
			assert.Equal(t, "init-0-2.mp4", s.Map)
		}
	}
	assert.Equal(t, 1, discontinuities(video))
	assert.Len(t, maps, 2)
	assert.Equal(t, 1, discontinuities(mediaPlaylist(t, base+"hls/index.m3u8")), "hls-ts")

	// DASH: period 2 is 640x360 avc1.4D401E with no audio.
	m := mpd(t, base)
	require.Len(t, m.Periods, 2)
	require.Len(t, m.Periods[1].AdaptationSets, 1)
	set := m.Periods[1].AdaptationSets[0]
	assert.Equal(t, "video", set.ContentType)
	require.Len(t, set.Representations, 1)
	rep := set.Representations[0]
	assert.Equal(t, "avc1.4D401E", rep.Codecs)
	assert.Equal(t, 640, rep.Width)
	assert.Equal(t, 360, rep.Height)

	// The epoch 2 init served and read back: 640x360, avc1.4D401E.
	get(t, base+"cmaf/init-0-2.mp4")
	for _, f := range []string{"hls-ts", "hls-fmp4", "dash"} {
		seen := false
		for _, fr := range caps[f].Frames {
			if fr.Epoch != 2 {
				continue
			}
			require.Equal(t, media.KindVideo, fr.Kind(), "%s: epoch 2 has video only", f)
			if seen {
				continue
			}
			seen = true
			assert.Equal(t, 640, fr.Config.Width, f)
			assert.Equal(t, 360, fr.Config.Height, f)
			cs, err := media.CodecString(fr.Config)
			require.NoError(t, err)
			assert.Equal(t, "avc1.4D401E", cs, f)
		}
		assert.True(t, seen, "%s: epoch 2 read back", f)
	}
}

// slowRun is a packager fed by a slow subscriber.
type slowRun struct {
	steps  []step
	p      *live.Packager
	handed []hub.Delivery
	lost   int
}

func (r slowRun) handedFrames(k media.Kind) []media.Frame {
	var out []media.Frame
	for _, dl := range r.handed {
		if dl.Frame.Kind() == k {
			out = append(out, dl.Frame)
		}
	}
	return out
}

// slowPackage writes the Clock output of h264_aac_loop.ts into a hub with a
// subscriber of the given queue and, every `every` writes, hands up to
// `burst` deliveries to Packager.Handle, so the hub drops frames and the
// segmenter marks discontinuities within an epoch.
func slowPackage(t *testing.T, clk *fakeClock, queue, every, burst int) slowRun {
	t.Helper()
	r := slowRun{steps: clockSteps(t, "h264_aac_loop.ts", clk.Now)}
	h := hub.New(hub.Options{})
	sub := h.Subscribe("slow", queue)
	r.p = live.NewPackager(h, live.Options{Window: m3aWindow, Now: clk.Now})
	take := func(dl hub.Delivery) {
		if dl.Lost > 0 {
			r.lost++
			require.True(t, dl.Frame.Key && dl.Frame.Kind() == media.KindVideo, "delivery after a loss is a video keyframe")
		}
		r.handed = append(r.handed, dl)
		r.p.Handle(dl)
	}
	for i, s := range r.steps {
		s.write(h)
		if (i+1)%every == 0 {
			for range burst {
				select {
				case dl := <-sub.C():
					take(dl)
				default:
				}
			}
		}
	}
	h.Close()
	for dl := range sub.C() {
		take(dl)
	}
	r.p.Flush()
	require.Zero(t, r.p.Errors())
	require.Zero(t, r.p.FallbackAnchors())
	return r
}

// lossSegments returns the indexes of the window's segments that are
// discontinuous without an epoch change, and checks that each starts on a
// video keyframe.
func lossSegments(t *testing.T, snap live.Snapshot) []int {
	t.Helper()
	var out []int
	for i, seg := range snap.Segments {
		if i == 0 || !seg.Discontinuity || seg.Epoch != snap.Segments[i-1].Epoch {
			continue
		}
		out = append(out, i)
		d, err := ts.NewDemuxer(bytes.NewReader(seg.TS))
		require.NoError(t, err)
		var first *media.Frame
		for {
			f, err := d.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			require.NoError(t, err)
			if first == nil && f.Kind() == media.KindVideo {
				first = &f
			}
		}
		require.NotNil(t, first, "segment %d has video", seg.Seq)
		assert.True(t, first.Key, "segment %d after a loss starts on a keyframe", seg.Seq)
		assert.Zero(t, d.Stats().DroppedNoConfig, "segment %d", seg.Seq)
	}
	return out
}

// videoDTS returns the DTS of the video frames, sorted, relative to the
// first one.
func videoDTS(frames []media.Frame) []int64 {
	var out []int64
	for _, f := range frames {
		if f.Kind() == media.KindVideo {
			out = append(out, f.DTS)
		}
	}
	slices.Sort(out)
	for i := len(out) - 1; i >= 0; i-- {
		out[i] -= out[0]
	}
	return out
}

func minDTS(frames []media.Frame, k media.Kind) int64 {
	var lo int64 = 1 << 62
	for _, f := range frames {
		if f.Kind() == k {
			lo = min(lo, f.DTS)
		}
	}
	return lo
}

func epochRange(n uint32) []uint32 {
	out := make([]uint32, 0, n)
	for e := uint32(1); e <= n; e++ {
		out = append(out, e)
	}
	return out
}

// TestM3aSlowPackager hands the packager 8 deliveries every 40 writes from
// a queue-8 subscriber. The fixture muxes audio about 300 ms behind video,
// so each burst after a loss holds the keyframe that opens the next
// segment and audio presented before it, which the segmenter drops; the
// audio that would fall inside the segment is lost in the hub. The outputs
// carry every handed video frame and no audio.
func TestM3aSlowPackager(t *testing.T) {
	start := time.Now()
	clk := newFakeClock(m3aT0)
	r := slowPackage(t, clk, 8, 40, 8)
	content := advance(t, r.p, clk)
	snap := r.p.Snapshot()
	video, audio := r.handedFrames(media.KindVideo), r.handedFrames(media.KindAudio)
	t.Logf("written %d frames, handed %d (%d video, %d audio) in %d losses, content %v, %d segments, %d epochs",
		len(r.steps), len(r.handed), len(video), len(audio), r.lost, content, len(snap.Segments), len(snap.Epochs))

	// Deterministic for this fixture and pattern: 11 losses, one of them
	// across the epoch change, so 10 discontinuities within an epoch.
	assert.Equal(t, 11, r.lost)
	losses := lossSegments(t, snap)
	assert.Len(t, losses, 10)

	// Every handed audio frame follows a loss and is presented before the
	// keyframe delivered with that loss, which opens the next segment.
	var lossKey *media.Frame
	starts := map[int64]bool{}
	for _, seg := range snap.Segments {
		starts[seg.Start] = true
	}
	for _, dl := range r.handed {
		f := dl.Frame
		if dl.Lost > 0 {
			lossKey = &f
			assert.True(t, starts[f.PTS], "the keyframe at PTS %d opens a segment", f.PTS)
			continue
		}
		if f.Kind() == media.KindAudio {
			require.NotNil(t, lossKey, "audio at PTS %d follows a loss", f.PTS)
			assert.Less(t, f.PTS, lossKey.PTS, "audio is presented before the segment opened after the loss")
		}
	}

	base := serve(t, r.p, stepsFeed(r.steps))
	caps := readAll(t, base)
	for _, f := range []string{"hls-ts", "hls-fmp4", "dash"} {
		assert.Equal(t, videoDTS(video), videoDTS(caps[f].Frames),
			"%s: every handed video frame, in every epoch", f)
		assert.Zero(t, kindCounts(caps[f].Frames)[epochKind{1, media.KindAudio}], f)
		assert.Equal(t, len(video), len(caps[f].Frames), "%s: video only", f)
	}
	// HLS epochs count discontinuities: 1 + 11 (10 losses and the epoch
	// change); DASH periods follow the 2 epochs.
	assert.Equal(t, epochRange(12), epochs(caps["hls-ts"].Frames))
	assert.Equal(t, epochRange(12), epochs(caps["hls-fmp4"].Frames))
	assert.Equal(t, epochRange(2), epochs(caps["dash"].Frames))
	written := make([]media.Frame, 0, len(r.steps))
	for _, s := range r.steps {
		written = append(written, s.f)
	}
	assert.Equal(t, flat(kindCounts(written)), kindCounts(caps["http-ts"].Frames), "http-ts")
	t.Logf("scenario took %v", time.Since(start).Round(time.Millisecond))
}

// TestM3aSlowPackagerAudio hands bursts long enough to reach past the
// audio mux lag, so audio survives each loss: its part after a loss is
// discontinuous and every track's part numbers stay hole-free.
func TestM3aSlowPackagerAudio(t *testing.T) {
	start := time.Now()
	clk := newFakeClock(m3aT0)
	r := slowPackage(t, clk, slowAudioBurst, slowAudioEvery, slowAudioBurst)
	content := advance(t, r.p, clk)
	snap := r.p.Snapshot()
	video, audio := r.handedFrames(media.KindVideo), r.handedFrames(media.KindAudio)
	t.Logf("written %d frames, handed %d (%d video, %d audio) in %d losses, content %v, %d segments, %d epochs",
		len(r.steps), len(r.handed), len(video), len(audio), r.lost, content, len(snap.Segments), len(snap.Epochs))
	// Deterministic for this fixture and pattern: 7 losses, one of them
	// across the epoch change.
	assert.Equal(t, 7, r.lost)
	losses := lossSegments(t, snap)
	require.Len(t, losses, 6)

	// Part numbers run from 0 with no holes per track.
	parts := map[int][]live.CMAFPart{}
	for _, seg := range snap.Segments {
		for _, part := range seg.Parts {
			parts[part.Track] = append(parts[part.Track], part)
		}
	}
	require.Len(t, parts, 2)
	for track, ps := range parts {
		for i, part := range ps {
			assert.Equal(t, uint64(i), part.N, "track %d part numbering", track)
		}
	}

	// After each loss, the audio track's next part is discontinuous, and
	// its fMP4 playlist entry carries EXT-X-DISCONTINUITY.
	base := serve(t, r.p, stepsFeed(r.steps))
	audioPL := mediaPlaylist(t, base+"cmaf/a1.m3u8")
	disc := map[string]bool{}
	uris := make([]string, 0, len(audioPL.Segments))
	for _, s := range audioPL.Segments {
		disc[s.URI] = s.Discontinuity
		uris = append(uris, s.URI)
	}
	for i, u := range uris {
		assert.Equal(t, fmt.Sprintf("1-%d.m4s", i), u, "audio playlist numbering")
	}
	for i, seg := range mediaPlaylist(t, base+"cmaf/v0.m3u8").Segments {
		assert.Equal(t, fmt.Sprintf("0-%d.m4s", i), seg.URI, "video playlist numbering")
	}
	for _, li := range losses {
		seg := snap.Segments[li]
		var part *live.CMAFPart
		for _, p := range seg.Parts {
			if p.Track == 1 {
				part = &p
			}
		}
		require.NotNil(t, part, "segment %d after a loss has audio", seg.Seq)
		assert.True(t, part.Discontinuity, "audio part %d after a loss is discontinuous", part.N)
		assert.True(t, disc[fmt.Sprintf("1-%d.m4s", part.N)], "audio part %d has EXT-X-DISCONTINUITY", part.N)
	}

	// Handed frames in the capture timeline: the captures carry every
	// handed video frame, so the first video DTS maps the timelines.
	firstLoss := snap.Segments[losses[0]].Start
	caps := readAll(t, base)
	for _, f := range []string{"hls-fmp4", "dash"} {
		assert.Equal(t, videoDTS(video), videoDTS(caps[f].Frames),
			"%s: every handed video frame", f)
		off := minDTS(caps[f].Frames, media.KindVideo) - minDTS(video, media.KindVideo)
		n := 0
		for _, fr := range caps[f].Frames {
			if fr.Kind() == media.KindAudio && fr.PTS-off >= firstLoss {
				n++
			}
		}
		assert.Positive(t, n, "%s: audio after a loss", f)
		t.Logf("%s: %d audio frames at or after the first loss (PTS %d)", f, n, firstLoss)
	}
	t.Logf("%d losses within an epoch; scenario took %v", len(losses), time.Since(start).Round(time.Millisecond))
}

// garbled returns h264_aac.ts behind 1000 random bytes with no sync byte,
// with 50 bytes cut out at cut (an offset into the fixture) when cut >= 0.
func garbled(t *testing.T, cut int) []byte {
	t.Helper()
	clip := fixtureBytes(t, "h264_aac.ts")
	if cut >= 0 {
		clip = slices.Delete(clip, cut, cut+50)
	}
	out := make([]byte, 1000, 1000+len(clip))
	rand.New(rand.NewSource(1)).Read(out) //nolint:gosec // deterministic test data
	for i, b := range out {
		if b == 0x47 {
			out[i] = 0
		}
	}
	return append(out, clip...)
}

func TestM3aGarbageInput(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cut          int
		video, audio int
	}{
		{"video-loss", 188 * 855, 149, 282},
		{"audio-loss", 188*900 + 100, 150, 266},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _, caps := pipelineScenario(t, garbled(t, tc.cut))
			assert.Len(t, p.Snapshot().Epochs, 1)
			// h264_aac.ts delivers 150 video and 282 audio frames whole;
			// each cut loses frames of one kind only. pipelineScenario has
			// checked every format against the frames written.
			want := kindMap{{1, media.KindVideo}: tc.video, {1, media.KindAudio}: tc.audio}
			for _, f := range formats {
				assert.Equal(t, want, kindCounts(caps[f].Frames), f)
			}
		})
	}
}

// probeStream is the part of ffprobe's JSON output the test checks.
type probeStream struct {
	CodecType  string `json:"codec_type"`
	CodecName  string `json:"codec_name"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Channels   int    `json:"channels"`
	SampleRate string `json:"sample_rate"`
}

// TestM3aFFprobe has ffprobe read every output of h264_aac.ts. The packager
// runs on the real clock with the epoch anchored in the past, so DASH and
// HLS clients see every segment available.
func TestM3aFFprobe(t *testing.T) {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	data := fixtureBytes(t, "h264_aac.ts")
	var last int64
	for _, s := range clockSteps(t, "h264_aac.ts", time.Now) {
		last = max(last, s.f.PTS)
	}
	content := media.ToDuration(last)
	t0 := time.Now().Add(-content - 5*time.Second)
	fd := pipelineFeed(data, func() time.Time { return t0 })
	p, _ := packFeed(t, fd, time.Now)
	snap := p.Snapshot()
	require.Len(t, snap.Segments, snap.Listed, "every segment is listed")
	base := serve(t, p, fd)

	want := []probeStream{
		{CodecType: "audio", CodecName: "aac", Channels: 2, SampleRate: "48000"},
		{CodecType: "video", CodecName: "h264", Width: 320, Height: 240},
	}
	for _, out := range []string{"hls/index.m3u8", "cmaf/master.m3u8", "cmaf/index.mpd", "stream.ts"} {
		t.Run(strings.ReplaceAll(out, "/", "_"), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			start := time.Now()
			cmd := exec.CommandContext(ctx, ffprobe, "-v", "error",
				"-show_entries", "stream=codec_type,codec_name,width,height,channels,sample_rate",
				"-of", "json", base+out)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			b, err := cmd.Output()
			require.NoError(t, err, stderr.String())
			var res struct {
				Streams []probeStream `json:"streams"`
			}
			require.NoError(t, json.Unmarshal(b, &res))
			got := slices.Clone(res.Streams)
			slices.SortFunc(got, func(a, b probeStream) int { return strings.Compare(a.CodecType, b.CodecType) })
			assert.Equal(t, want, got, "streams %+v", res.Streams)
			assert.Empty(t, stderr.String(), "ffprobe stderr")
			t.Logf("%s: %+v in %v; stderr %q", out, res.Streams, time.Since(start).Round(time.Millisecond), stderr.String())
		})
	}
}

// The audio-surviving slow variant: a queue and burst of slowAudioBurst
// every slowAudioEvery writes. 48 every 56 or 96 instead lands a loss
// right after epoch 2's first keyframe, so that epoch's first segment has
// no audio; see the task 19 fix report.
const (
	slowAudioBurst = 48
	slowAudioEvery = 64
)
