package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/output/live"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/dash"
	"github.com/ntt0601zcoder/open-streamer/pkg/hls"
)

// The M4b scenarios run the M4a ladder with the presentation hub on
// (ruling R-B1): one transcoder run per fixture is recorded, its
// presentation is packaged in ABR mode and read back as one aligned ABR
// presentation over HLS-TS, HLS fMP4 and DASH, with every read checking
// the ABR rules (variant-aligned, variant-bandwidth).

// m4bLadder is m4aLadder with the presentation hub.
func m4bLadder(binary string) transcode.Options {
	opt := m4aLadder(binary)
	opt.Presentation = true
	return opt
}

// m4bRun is one transcoder run over a fixture with the presentation on.
type m4bRun struct {
	presentation []step
	renditions   [][]step // per rendition, in m4bLadder order
}

// m4bRuns transcodes each fixture once through m4bLadder, in parallel.
// Each run must be clean as transcodeSteps requires, with nothing dropped
// by the presentation's merger, and every epoch of the presentation must
// start with rung 0's IDR, followed by the other rungs' IDRs at its PTS.
func m4bRuns(t *testing.T, bin string, names ...string) map[string]m4bRun {
	t.Helper()
	out := map[string]m4bRun{}
	var mu sync.Mutex
	t.Run("transcode", func(t *testing.T) {
		for _, name := range names {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				start := time.Now()
				opt := m4bLadder(bin)
				run := recordTranscode(t, m4aSource(t, name), opt, nil)
				for i, s := range run.renditions {
					requireVideoFirst(t, stepFrames(s), opt.Renditions[i].Name)
					t.Logf("%s: %s", opt.Renditions[i].Name, kindCounts(stepFrames(s)))
				}
				requireCleanRun(t, run.stats)
				require.Zero(t, run.stats.PresentationDropped, "%+v", run.stats)
				pres := stepFrames(run.presentation)
				requireVideoFirst(t, pres, "presentation")
				requireRungZeroFirst(t, pres, len(opt.Renditions))
				t.Logf("presentation: %s", trackCounts(pres))
				t.Logf("transcoded in %v; stats %+v", time.Since(start).Round(time.Millisecond), run.stats)
				mu.Lock()
				out[name] = m4bRun{presentation: run.presentation, renditions: run.renditions}
				mu.Unlock()
			})
		}
	})
	if t.Failed() {
		t.FailNow()
	}
	return out
}

// requireRungZeroFirst checks that each epoch of a presentation of n
// rungs starts, in hub order, with the IDRs of rungs 0 to n-1 at one PTS,
// rung 0's first (Task 2: a late subscriber's cache, anchored on track 0,
// then holds every rung's IDR).
func requireRungZeroFirst(t *testing.T, frames []media.Frame, n int) {
	t.Helper()
	first := map[uint32]int{} // epoch → index of its first frame
	for i, f := range frames {
		if _, ok := first[f.Epoch]; !ok {
			first[f.Epoch] = i
		}
	}
	for e, i := range first {
		require.GreaterOrEqual(t, len(frames)-i, n, "epoch %d", e)
		head := frames[i : i+n]
		for r, f := range head {
			require.True(t, f.Kind() == media.KindVideo && f.Key && f.Track == transcode.PresentationTrack(r) &&
				f.PTS == head[0].PTS,
				"epoch %d: frame %d of the epoch is track %d, kind %d, key %v, PTS %d; want rung %d's IDR at PTS %d",
				e, r, f.Track, f.Kind(), f.Key, f.PTS, r, head[0].PTS)
		}
	}
}

// epochTrackKey names one track of one epoch.
type epochTrackKey struct {
	epoch uint32
	track int
}

// trackCounts counts frames per epoch and track, for logs.
func trackCounts(frames []media.Frame) string {
	m := map[epochTrackKey]int{}
	for _, f := range frames {
		m[epochTrackKey{f.Epoch, f.Track}]++
	}
	keys := make([]epochTrackKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b epochTrackKey) int {
		if a.epoch != b.epoch {
			return int(a.epoch) - int(b.epoch)
		}
		return a.track - b.track
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("e%d/t%d=%d", k.epoch, k.track, m[k]))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// abrFormats are the outputs of an ABR presentation (ruling R-B2: no
// HTTP-TS).
var abrFormats = []struct{ name, path string }{
	{"hls-ts", "hls/index.m3u8"},
	{"hls-fmp4", "cmaf/master.m3u8"},
	{"dash", "cmaf/index.mpd"},
}

// abrScenario is one packaged and served ABR presentation.
type abrScenario struct {
	p       *live.Packager
	base    string
	caps    map[string]conform.Capture
	written []media.Frame // every frame written to the hub
	lost    uint64        // the packager subscription's Lost, summed
	losses  int           // deliveries with Lost > 0
}

// handBursts makes packABR's packager a slow consumer, as slowPackage
// does: every `every` writes, it is handed up to n deliveries.
type handBursts struct{ every, n int }

// packABR packages steps on a fake clock with live.Options{ABR: true}
// through a packager subscription of queue (0 = bigQueue), then serves the
// packager and reads every ABR output back once, with AlignedSequences.
// Without slow the packager runs concurrently with the unpaced writes and,
// with bigQueue, must lose nothing; with slow it is handed bursts on the
// writer's goroutine. Either way the packager must have no error and every
// segment Start must exceed the one before (ruling R-B8).
func packABR(t *testing.T, steps []step, queue int, slow *handBursts) abrScenario {
	t.Helper()
	if queue == 0 {
		queue = bigQueue
	}
	clk := newFakeClock(m3aT0)
	fd := stepsFeed(steps)
	h := hub.New(hub.Options{})
	sub := h.Subscribe("packager", queue)
	tap := h.Subscribe("tap", bigQueue)
	p := live.NewPackager(h, live.Options{Window: m3aWindow, Now: clk.Now, ABR: true})
	r := abrScenario{p: p}
	take := func(dl hub.Delivery) {
		if dl.Lost > 0 {
			r.lost += dl.Lost
			r.losses++
		}
		p.Handle(dl)
	}
	if slow != nil {
		for i, s := range steps {
			s.write(h)
			if (i+1)%slow.every != 0 {
				continue
			}
			for range slow.n {
				select {
				case dl := <-sub.C():
					take(dl)
				default:
				}
			}
		}
		h.Close()
		for dl := range sub.C() {
			take(dl)
		}
		p.End()
	} else {
		done := make(chan struct{})
		go func() {
			defer close(done)
			for dl := range sub.C() {
				take(dl)
			}
			p.End()
		}()
		require.NoError(t, fd(context.Background(), h))
		h.Close()
		<-done
	}
	for dl := range tap.C() {
		require.Zero(t, dl.Lost)
		r.written = append(r.written, dl.Frame)
	}
	require.Len(t, r.written, len(steps), "the hub delivered every frame from the start")
	require.Zero(t, p.Errors())
	require.Zero(t, p.FallbackAnchors())
	if queue == bigQueue {
		require.Zero(t, r.lost)
	}
	r.base, r.caps = serveABR(t, p, clk, fd, fmt.Sprintf("written %d frames %s; packager lost %d frames in %d losses",
		len(r.written), trackCounts(r.written), r.lost, r.losses))
	return r
}

// serveABR lists every segment of the ended ABR packager p (advancing clk),
// checks that every segment Start exceeds the one before (ruling R-B8),
// serves p (fd feeds stream.ts, which ABR outputs do not use) and reads
// every ABR output back with readABR. note heads the log line.
func serveABR(t *testing.T, p *live.Packager, clk *fakeClock, fd feed, note string) (string, map[string]conform.Capture) {
	t.Helper()
	content := advance(t, p, clk)
	snap := p.Snapshot()
	gaps := 0
	for i, seg := range snap.Segments {
		if i > 0 {
			prev := snap.Segments[i-1]
			require.Greater(t, seg.Start, prev.Start, "segment %d starts after segment %d", seg.Seq, prev.Seq)
		}
		gaps += len(snap.Tracks) - len(seg.Parts)
	}
	t.Logf("%s; content %v, %d segments, %d epochs, %d missing parts", note, content, len(snap.Segments),
		len(snap.Epochs), gaps)
	base := serve(t, p, fd)
	return base, readABR(t, base)
}

// feedABR packages steps with live.Options{ABR: true} through a packager
// subscription of queue (0 = bigQueue), serves them, and reads hls-ts
// (hls/index.m3u8), hls-fmp4 (cmaf/master.m3u8) and dash (cmaf/index.mpd)
// back with AlignedSequences, each free of violations.
//
//nolint:unparam // queue is part of the helper's contract for the player scenarios (Task 8)
func feedABR(t *testing.T, steps []step, queue int) (*live.Packager, string, map[string]conform.Capture) {
	t.Helper()
	r := packABR(t, steps, queue, nil)
	return r.p, r.base, r.caps
}

// readABR reads the ABR outputs back once with AlignedSequences, so the
// variant-aligned and variant-bandwidth rules apply, and checks that none
// has a violation.
func readABR(t *testing.T, base string) map[string]conform.Capture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	opt := conform.ReadOptions{Once: true, AlignedSequences: true}
	out := map[string]conform.Capture{}
	for _, f := range abrFormats {
		var (
			c   conform.Capture
			err error
		)
		start := time.Now()
		if f.name == "dash" {
			c, err = conform.ReadDASH(ctx, base+f.path, opt)
		} else {
			c, err = conform.ReadHLS(ctx, base+f.path, opt)
		}
		require.NoError(t, err, f.name)
		assert.Empty(t, c.Violations, f.name)
		assert.NotEmpty(t, c.Frames, f.name)
		t.Logf("%-8s %4d frames in %v; per epoch/track %s; %d gaps; violations %v",
			f.name, len(c.Frames), time.Since(start).Round(time.Millisecond), trackCounts(c.Frames), c.HLSGaps, c.Violations)
		out[f.name] = c
	}
	return out
}

// byTrack splits frames by kind and track.
func byTrack(frames []media.Frame) (video, audio map[int][]media.Frame) {
	video, audio = map[int][]media.Frame{}, map[int][]media.Frame{}
	for _, f := range frames {
		switch f.Kind() {
		case media.KindVideo:
			video[f.Track] = append(video[f.Track], f)
		case media.KindAudio:
			audio[f.Track] = append(audio[f.Track], f)
		default:
		}
	}
	return video, audio
}

func sortedKeys(m map[int][]media.Frame) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// epochShape is what assertCompleteABR compares per track: per epoch, the
// frame count, the DTS relative to the epoch's first, and the first
// frame's picture size.
type epochShape struct {
	count         int
	dts           []int64
	width, height int
}

func shapes(frames []media.Frame) map[uint32]epochShape {
	byEpoch := map[uint32][]media.Frame{}
	for _, f := range frames {
		byEpoch[f.Epoch] = append(byEpoch[f.Epoch], f)
	}
	out := map[uint32]epochShape{}
	for e, fs := range byEpoch {
		dts := make([]int64, len(fs))
		for i, f := range fs {
			dts[i] = f.DTS
		}
		slices.Sort(dts)
		for i := len(dts) - 1; i >= 0; i-- {
			dts[i] -= dts[0]
		}
		s := epochShape{count: len(fs), dts: dts}
		if c := fs[0].Config; c != nil {
			s.width, s.height = c.Width, c.Height
		}
		out[e] = s
	}
	return out
}

// assertCompleteABR checks, per ABR format, that every written video frame
// of every track is captured once (capture video tracks, in order, are the
// presentation's video tracks, in order: per epoch the same count, DTS
// steps and picture size), and the audio track (the presentations have
// one) once per hls-ts variant, once in hls-fmp4 and in dash, with the
// same count and DTS steps per epoch. Formats missing from caps are not
// checked.
func assertCompleteABR(t *testing.T, caps map[string]conform.Capture, written []media.Frame) {
	t.Helper()
	wv, wa := byTrack(written)
	vtracks := sortedKeys(wv)
	require.LessOrEqual(t, len(wa), 1, "one audio track at most")
	var audio []media.Frame
	for _, tr := range sortedKeys(wa) {
		audio = wa[tr]
	}
	for _, f := range abrFormats {
		c, ok := caps[f.name]
		if !ok {
			continue
		}
		cv, ca := byTrack(c.Frames)
		ctracks := sortedKeys(cv)
		if !assert.Len(t, ctracks, len(vtracks), "%s: video tracks", f.name) {
			continue
		}
		for i, tr := range vtracks {
			assert.Equal(t, shapes(wv[tr]), shapes(cv[ctracks[i]]),
				"%s: video track %d (capture track %d)", f.name, tr, ctracks[i])
		}
		n := 1
		if f.name == "hls-ts" {
			n = len(vtracks)
		}
		if len(audio) == 0 {
			assert.Empty(t, ca, "%s: no audio", f.name)
			continue
		}
		if !assert.Len(t, ca, n, "%s: audio captures", f.name) {
			continue
		}
		want := shapes(audio)
		for tr, fs := range ca {
			assert.Equal(t, want, shapes(fs), "%s: audio capture track %d", f.name, tr)
		}
	}
}

// multivariant fetches and parses a multivariant playlist.
func multivariant(t *testing.T, u string) hls.MultivariantPlaylist {
	t.Helper()
	m, err := hls.ParseMultivariant(get(t, u))
	require.NoError(t, err)
	return m
}

// abrPlaylists returns the media playlist URLs of the ABR presentation at
// base: hls-ts its variants, hls-fmp4 its variants then its renditions.
func abrPlaylists(t *testing.T, base string) map[string][]string {
	t.Helper()
	variants := multivariant(t, base+"hls/index.m3u8").Variants
	ts := make([]string, 0, len(variants))
	for _, v := range variants {
		ts = append(ts, base+"hls/"+v.URI)
	}
	return map[string][]string{"hls-ts": ts, "hls-fmp4": cmafPlaylists(t, base)}
}

// discMSNs returns the media sequence numbers of pl's entries that carry
// EXT-X-DISCONTINUITY.
func discMSNs(pl hls.MediaPlaylist) []uint64 {
	var out []uint64
	for i, s := range pl.Segments {
		if s.Discontinuity {
			out = append(out, pl.MediaSequence+uint64(i))
		}
	}
	return out
}

// assertDiscontinuitiesAligned checks that every playlist of both HLS
// formats has its discontinuities at the same media sequence numbers, and
// returns them.
func assertDiscontinuitiesAligned(t *testing.T, base string) []uint64 {
	t.Helper()
	var ref []uint64
	refURL := ""
	for _, f := range []string{"hls-ts", "hls-fmp4"} {
		for _, u := range abrPlaylists(t, base)[f] {
			got := discMSNs(mediaPlaylist(t, u))
			if refURL == "" {
				ref, refURL = got, u
				continue
			}
			assert.Equal(t, ref, got, "discontinuities of %s against %s", u, refURL)
		}
	}
	return ref
}

// assertABRManifests checks that both masters list the ladder's variants
// in rendition order (v0..v{n-1}.m3u8), and that every period of the MPD
// has exactly one video AdaptationSet, H.264, with n Representations.
func assertABRManifests(t *testing.T, base string, n, periods int) {
	t.Helper()
	want := make([]string, 0, n)
	for i := range n {
		want = append(want, fmt.Sprintf("v%d.m3u8", transcode.PresentationTrack(i)))
	}
	for _, u := range []string{base + "hls/index.m3u8", base + "cmaf/master.m3u8"} {
		var got []string
		for _, v := range multivariant(t, u).Variants {
			got = append(got, v.URI)
		}
		assert.Equal(t, want, got, "%s: variants", u)
	}
	m := mpd(t, base)
	require.Len(t, m.Periods, periods)
	for pi, p := range m.Periods {
		var sets []dash.AdaptationSet
		for _, s := range p.AdaptationSets {
			if s.ContentType == "video" {
				sets = append(sets, s)
			}
		}
		require.Len(t, sets, 1, "period %d: one video set", pi)
		reps := sets[0].Representations
		require.Len(t, reps, n, "period %d: one Representation per rung", pi)
		for _, r := range reps {
			assert.True(t, strings.HasPrefix(r.Codecs, "avc1."), "period %d: rep %s codecs %q", pi, r.ID, r.Codecs)
		}
	}
}

// presentationTrackFrames returns the frames of presentation track tr.
func presentationTrackFrames(frames []media.Frame, tr int) []media.Frame {
	var out []media.Frame
	for _, f := range frames {
		if f.Track == tr {
			out = append(out, f)
		}
	}
	return out
}

// assertPresentationIsRenditions checks that presentation track i holds
// exactly rendition i's video frames (PTS, DTS, Key, Epoch, Data), in
// order, and the audio track once.
func assertPresentationIsRenditions(t *testing.T, run m4bRun) {
	t.Helper()
	pres := stepFrames(run.presentation)
	type frameID struct {
		pts, dts int64
		key      bool
		epoch    uint32
		data     string
	}
	id := func(f media.Frame) frameID {
		return frameID{f.PTS, f.DTS, f.Key, f.Epoch, string(bytes.Join(f.Data, nil))}
	}
	ids := func(frames []media.Frame) []frameID {
		out := make([]frameID, len(frames))
		for i, f := range frames {
			out[i] = id(f)
		}
		return out
	}
	n := len(run.renditions)
	for i, steps := range run.renditions {
		want := videoFrames(stepFrames(steps))
		got := presentationTrackFrames(pres, transcode.PresentationTrack(i))
		require.NotEmpty(t, want, "rendition %d", i)
		assert.Equal(t, ids(want), ids(got), "presentation track %d is rendition %d's video", transcode.PresentationTrack(i), i)
	}
	// The audio once, as rendition 0 has it.
	var want []media.Frame
	for _, f := range stepFrames(run.renditions[0]) {
		if f.Kind() == media.KindAudio {
			want = append(want, f)
		}
	}
	var got []media.Frame
	for _, f := range pres {
		if f.Kind() == media.KindAudio {
			got = append(got, f)
			assert.Equal(t, transcode.PresentationAudioTrack(n, want[0].Track), f.Track, "audio track")
		}
	}
	assert.Equal(t, ids(want), ids(got), "the presentation's audio is the renditions'")
}

// loopSteps returns steps looped n times as pacedFeed loops them, without
// pacing: pass k shifted by k × passDelta, and, for a clip with more than
// one epoch or config, its epochs shifted by k × (epochs per pass) with
// anchors to match.
func loopSteps(steps []step, n int) []step {
	frames := stepFrames(steps)
	delta := passDelta(frames)
	minE, maxE := frames[0].Epoch, frames[0].Epoch
	configs := map[int]*media.TrackConfig{}
	oneConfig := true
	for _, f := range frames {
		minE, maxE = min(minE, f.Epoch), max(maxE, f.Epoch)
		if c, ok := configs[f.Track]; ok && c != f.Config {
			oneConfig = false
		}
		configs[f.Track] = f.Config
	}
	var epochShift uint32
	if minE != maxE || !oneConfig {
		epochShift = maxE - minE + 1
	}
	out := make([]step, 0, n*len(steps))
	for k := range n {
		shift := int64(k) * delta
		for _, s := range steps {
			var anchors []media.Anchor
			if k == 0 || epochShift > 0 {
				for _, a := range s.anchors {
					a.Epoch += uint32(k) * epochShift
					a.Start += shift
					a.Wall = a.Wall.Add(media.ToDuration(shift))
					anchors = append(anchors, a)
				}
			}
			f := s.f
			f.Epoch += uint32(k) * epochShift
			f.PTS += shift
			f.DTS += shift
			out = append(out, step{anchors: anchors, f: f})
		}
	}
	return out
}

// The slow-consumer row's packager: a subscription of slowABRQueue handed
// slowABRBurst deliveries every slowABREvery writes of a 3× replay, so the
// queue gains 8 frames a burst and the hub drops frames for it every one or
// two GOPs (12 losses in the replay's 18.6 s, for the recordings seen).
const (
	slowABRQueue = 64
	slowABREvery = 64
	slowABRBurst = 56
)

// TestM4bMatrix runs the presentation of the M4a ladder (t240, t180, t120,
// src) through the M3 scenarios, packaged in ABR mode and read back as
// hls-ts, hls-fmp4 and dash with the ABR rules:
//
//   - steady-state: h264_aac.ts; no violation, every frame once, 1 epoch;
//     both masters list the 4 variants; the MPD has one H.264 video set
//     with 4 Representations.
//   - epoch-change: h264_aac_then_640.ts; epochs [1 2] in every format, and
//     one discontinuity, at the same msn, in every playlist.
//   - slow-consumer: h264_aac.ts's presentation replayed 3× into a packager
//     subscription of slowABRQueue handed bursts, as TestM3aSlowPackager
//     does: Lost > 0, no violation, every playlist's discontinuities at the
//     same msn (completeness is not asserted: the hub dropped frames).
//   - no-bframes (h264_aac_nob.ts), b-frames (h264_pyramid.ts): no
//     violation, every frame once, and presentation track i holds exactly
//     rendition i's video frames of the same run.
func TestM4bMatrix(t *testing.T) {
	t.Parallel()
	bin := testpeer.OSTranscoder(t)
	runs := m4bRuns(t, bin, "h264_aac.ts", "h264_aac_then_640.ts", "h264_aac_nob.ts", "h264_pyramid.ts")
	n := len(m4bLadder(bin).Renditions)
	t.Run("steady-state", func(t *testing.T) {
		t.Parallel()
		steps := runs["h264_aac.ts"].presentation
		p, base, caps := feedABR(t, steps, 0)
		assertCompleteABR(t, caps, stepFrames(steps))
		assert.Len(t, p.Snapshot().Epochs, 1)
		for _, f := range abrFormats {
			assert.Equal(t, []uint32{1}, epochs(caps[f.name].Frames), f.name)
		}
		assert.Empty(t, assertDiscontinuitiesAligned(t, base))
		assertABRManifests(t, base, n, 1)
	})
	t.Run("epoch-change", func(t *testing.T) {
		t.Parallel()
		steps := runs["h264_aac_then_640.ts"].presentation
		p, base, caps := feedABR(t, steps, 0)
		assertCompleteABR(t, caps, stepFrames(steps))
		assert.Len(t, p.Snapshot().Epochs, 2)
		for _, f := range abrFormats {
			assert.Equal(t, []uint32{1, 2}, epochs(caps[f.name].Frames), f.name)
		}
		assert.Len(t, assertDiscontinuitiesAligned(t, base), 1)
		assertABRManifests(t, base, n, 2)
	})
	t.Run("slow-consumer", func(t *testing.T) {
		t.Parallel()
		r := packABR(t, loopSteps(runs["h264_aac.ts"].presentation, 3), slowABRQueue,
			&handBursts{every: slowABREvery, n: slowABRBurst})
		assert.Positive(t, r.lost, "the packager subscription lost frames")
		disc := assertDiscontinuitiesAligned(t, r.base)
		assert.NotEmpty(t, disc, "a discontinuity after a loss")
		t.Logf("discontinuities at msn %v", disc)
	})
	for _, row := range []struct{ name, fixture string }{
		{"no-bframes", "h264_aac_nob.ts"},
		{"b-frames", "h264_pyramid.ts"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			run := runs[row.fixture]
			assertPresentationIsRenditions(t, run)
			_, base, caps := feedABR(t, run.presentation, 0)
			assertCompleteABR(t, caps, stepFrames(run.presentation))
			assert.Empty(t, assertDiscontinuitiesAligned(t, base))
		})
	}
}

// TestM4bSwitchTS switches variants at every segment boundary: segment n
// of the h264_aac.ts presentation, looped twice (loopSteps) so that its six
// segments reach every variant, the copy included, is fetched from variant
// n%4 of hls/index.m3u8 (the next variant with a real entry when that one
// lists a gap), and the files, listed for ffmpeg's concat demuxer, decode
// with no error, every video frame once, their PTS strictly increasing.
func TestM4bSwitchTS(t *testing.T) {
	t.Parallel()
	bin := testpeer.OSTranscoder(t)
	ffmpeg := testpeer.Require(t, "ffmpeg")
	ffprobe := testpeer.Require(t, "ffprobe")
	steps := loopSteps(m4bRuns(t, bin, "h264_aac.ts")["h264_aac.ts"].presentation, 2)
	_, base, _ := feedABR(t, steps, 0)

	variants := multivariant(t, base+"hls/index.m3u8").Variants
	lists := make([]hls.MediaPlaylist, 0, len(variants))
	uris := make([]string, 0, len(variants))
	for _, v := range variants {
		uris = append(uris, v.URI)
		lists = append(lists, mediaPlaylist(t, base+"hls/"+v.URI))
	}
	require.Len(t, lists, len(m4bLadder(bin).Renditions))
	for i, pl := range lists[1:] {
		require.Equal(t, lists[0].MediaSequence, pl.MediaSequence, uris[i+1])
		require.Len(t, pl.Segments, len(lists[0].Segments), uris[i+1])
	}
	dir := t.TempDir()
	var list strings.Builder
	used := map[int]bool{}
	picks := make([]string, 0, len(lists[0].Segments))
	for j := range lists[0].Segments {
		msn := lists[0].MediaSequence + uint64(j)
		v := -1
		for k := range lists {
			c := (int(msn%uint64(len(lists))) + k) % len(lists)
			if !lists[c].Segments[j].Gap {
				v = c
				break
			}
		}
		require.GreaterOrEqual(t, v, 0, "msn %d: a variant with a real entry", msn)
		used[v] = true
		path := filepath.Join(dir, fmt.Sprintf("seg-%03d.ts", j))
		require.NoError(t, os.WriteFile(path, get(t, base+"hls/"+lists[v].Segments[j].URI), 0o600))
		fmt.Fprintf(&list, "file '%s'\n", path)
		picks = append(picks, fmt.Sprintf("%d:%s", msn, lists[v].Segments[j].URI))
	}
	require.GreaterOrEqual(t, len(used), 3, "at least 3 distinct variants used: %v", picks)
	listPath := filepath.Join(dir, "list.txt")
	require.NoError(t, os.WriteFile(listPath, []byte(list.String()), 0o600))
	t.Logf("segments %v", picks)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-f", "concat", "-safe", "0", "-i", listPath, "-f", "null", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Run(), stderr.String())
	assert.Empty(t, stderr.String(), "ffmpeg stderr")

	cmd = exec.CommandContext(ctx, ffprobe, "-v", "error", "-f", "concat", "-safe", "0", "-i", listPath,
		"-select_streams", "v", "-show_entries", "frame=pts", "-of", "csv=p=0")
	stderr.Reset()
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, stderr.String())
	assert.Empty(t, stderr.String(), "ffprobe stderr")
	lines := strings.Fields(string(out))
	pts := make([]int64, 0, len(lines))
	for _, line := range lines {
		v, err := strconv.ParseInt(strings.TrimSuffix(line, ","), 10, 64)
		require.NoError(t, err, "frame pts %q", line)
		pts = append(pts, v)
	}
	// Every rung has the same frames per segment (they key at the same PTS).
	assert.Len(t, pts, len(presentationTrackFrames(stepFrames(steps), transcode.PresentationTrack(0))),
		"ffprobe decoded every video frame")
	for i := 1; i < len(pts); i++ {
		require.Greater(t, pts[i], pts[i-1], "video frame %d PTS increases", i)
	}
	t.Logf("%d segments from %d variants decoded; %d video frames, PTS %d..%d", len(picks), len(used), len(pts),
		pts[0], pts[len(pts)-1])
}

// pacedSteps writes steps at their own pace, as m4aSource writes a
// fixture: each frame at its DTS after the first.
func pacedSteps(steps []step) feed {
	return func(ctx context.Context, h *hub.Hub) error {
		start := time.Now()
		first := steps[0].f.DTS
		for _, s := range steps {
			if d := time.Until(start.Add(media.ToDuration(s.f.DTS - first))); d > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(d):
				}
			}
			s.write(h)
		}
		return nil
	}
}

// killAfterKeys kills tr's child once, after the n-th video keyframe has
// reached h, watching from a subscriber taken now. The returned channel
// yields the kill time and is closed once the watcher is done (also when h
// closes first, without a kill).
func killAfterKeys(t *testing.T, tr *transcode.Transcoder, h *hub.Hub, n int) <-chan time.Time {
	t.Helper()
	sub := h.Subscribe("killer", bigQueue)
	out := make(chan time.Time, 1)
	go func() {
		defer close(out)
		defer sub.Close()
		keys := 0
		for dl := range sub.C() {
			if dl.Frame.Kind() != media.KindVideo || !dl.Frame.Key {
				continue
			}
			if keys++; keys < n {
				continue
			}
			pid := tr.Stats().PID
			if pid <= 0 {
				t.Errorf("killAfterKeys: no child running at keyframe %d", keys)
				return
			}
			proc, err := os.FindProcess(pid)
			if err == nil {
				err = proc.Kill() // SIGKILL on Unix
			}
			if err != nil {
				t.Errorf("killAfterKeys: kill %d: %v", pid, err)
				return
			}
			out <- time.Now()
			return
		}
	}()
	return out
}

// TestM4bRespawn kills the child with SIGKILL once, after its 3rd output
// keyframe on t240, while h264_aac.ts is fed twice at its pace
// (loopSteps, pacedSteps: one upstream epoch, the second pass shifted). The presentation gets
// exactly one new epoch, which starts with rung 0's IDR: its epochs are
// [1 2]; read back, every frame is there once; in hls-ts and hls-fmp4
// every playlist has exactly one discontinuity, at the same msn; the MPD
// has 2 periods, each with the 4 video Representations.
//
// Timing budget: the source keys every second from 1.48 s. t240's 3rd
// output keyframe is source key 3.48 s, reached about 2 s after the feed
// starts plus the encoder's latency; the 2× feed lasts about 10 s, so 7
// source keyframes (and about 7.5 s of wall time) remain after the kill
// for the backoff (RespawnMin 250 ms), the new child's start-up and Hello,
// and its first IDR. A single pass would leave one keyframe, at 5.48 s,
// which a loaded runner (arm64, -race, parallel children) can miss.
func TestM4bRespawn(t *testing.T) {
	t.Parallel()
	bin := testpeer.OSTranscoder(t)
	opt := m4bLadder(bin)
	var killed <-chan time.Time
	src := pacedSteps(loopSteps(clockSteps(t, "h264_aac.ts", newFakeClock(m3aT0).Now), 2))
	run := recordTranscode(t, src, opt, func(tr *transcode.Transcoder) {
		killed = killAfterKeys(t, tr, tr.Hubs()[0], 3)
	})
	at, ok := <-killed
	require.True(t, ok, "the child was killed")
	st := run.stats
	t.Logf("killed at %v; stats %+v", at.Format(time.TimeOnly), st)
	assert.EqualValues(t, 2, st.Spawns)
	assert.EqualValues(t, 1, st.Exits)
	assert.Zero(t, st.Losses)

	pres := stepFrames(run.presentation)
	t.Logf("presentation: %s", trackCounts(pres))
	require.Equal(t, []uint32{1, 2}, epochs(pres), "one new epoch, after the respawn")
	requireRungZeroFirst(t, pres, len(opt.Renditions))
	for i, steps := range run.renditions {
		assert.Equal(t, []uint32{1, 2}, epochs(stepFrames(steps)), opt.Renditions[i].Name)
	}

	p, base, caps := feedABR(t, run.presentation, 0)
	assertCompleteABR(t, caps, pres)
	assert.Len(t, p.Snapshot().Epochs, 2)
	for _, f := range abrFormats {
		assert.Equal(t, []uint32{1, 2}, epochs(caps[f.name].Frames), f.name)
	}
	for f, urls := range abrPlaylists(t, base) {
		for _, u := range urls {
			assert.Len(t, discMSNs(mediaPlaylist(t, u)), 1, "%s: %s has one discontinuity", f, u)
		}
	}
	disc := assertDiscontinuitiesAligned(t, base)
	assertABRManifests(t, base, len(opt.Renditions), 2)
	t.Logf("discontinuity at msn %v", disc)
}

// tsAudioFrames counts the audio frames of segment seg's TS variant of
// video track tr.
func tsAudioFrames(t *testing.T, seg live.SegmentInfo, tr int) int {
	t.Helper()
	i := slices.IndexFunc(seg.VariantTS, func(v live.VariantTS) bool { return v.Track == tr })
	require.GreaterOrEqual(t, i, 0, "segment %d has variant %d", seg.Seq, tr)
	d, err := ts.NewDemuxer(bytes.NewReader(seg.VariantTS[i].Data))
	require.NoError(t, err)
	n := 0
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			return n
		}
		require.NoError(t, err)
		if f.Kind() == media.KindAudio {
			n++
		}
	}
}

// gapRung is the rung TestM4bRungGap leaves out: t120.
const gapRung = 2

// TestM4bRungGap shows ruling R-B8's other half end to end (Review Focus
// 2): a rung whose IDR a loss swallowed lists a gap rather than a part
// that starts mid-GOP. The h264_aac.ts presentation is handed to an ABR
// packager directly, with rung 2's frames dropped from its IDR at the
// second segment's start K up to its next keyframe (1 s later), and the
// delivery of rung 0's IDR at K, the first at that barrier, carrying Lost,
// as a hub loss just before K signals it. The segment at K is
// discontinuous and has no rung-2 part: hls/v2.m3u8 lists gap-2-{Seq}.ts
// and cmaf/v2.m3u8 a gap entry there, every other variant a real entry;
// every format reads back with no violation, rung 2's frames from its next
// keyframe on and every other frame once; and ffmpeg decodes hls/v2.m3u8
// with no error.
func TestM4bRungGap(t *testing.T) {
	t.Parallel()
	bin := testpeer.OSTranscoder(t)
	steps := m4bRuns(t, bin, "h264_aac.ts")["h264_aac.ts"].presentation
	rung := transcode.PresentationTrack(gapRung)

	// K: the second segment's start, as the barrier makes it, two
	// keyframes after the first.
	keys := keySet(presentationTrackFrames(stepFrames(steps), transcode.PresentationTrack(0)))
	require.GreaterOrEqual(t, len(keys), 4)
	k := keys[2]
	var kept []hub.Delivery
	dropping, lostAt := false, -1
	for _, s := range steps {
		f := s.f
		if f.Track == rung && f.Kind() == media.KindVideo && f.Key {
			dropping = f.PTS == k
		}
		if dropping && f.Track == rung {
			continue
		}
		if lostAt < 0 && f.Track == transcode.PresentationTrack(0) && f.Key && f.PTS == k {
			lostAt = len(kept)
		}
		kept = append(kept, hub.Delivery{Frame: f})
	}
	require.GreaterOrEqual(t, lostAt, 0)
	dropped := len(steps) - len(kept)
	require.Positive(t, dropped)
	kept[lostAt].Lost = uint64(dropped)

	clk := newFakeClock(m3aT0)
	h := hub.New(hub.Options{})
	for _, s := range steps {
		for _, a := range s.anchors {
			h.SetAnchor(a)
		}
	}
	p := live.NewPackager(h, live.Options{Window: m3aWindow, Now: clk.Now, ABR: true})
	for _, d := range kept {
		p.Handle(d)
	}
	p.End()
	require.Zero(t, p.Errors())
	require.Zero(t, p.FallbackAnchors())
	base, caps := serveABR(t, p, clk, stepsFeed(steps),
		fmt.Sprintf("rung %d: %d frames dropped from K = %d", rung, dropped, k))

	snap := p.Snapshot()
	at := slices.IndexFunc(snap.Segments, func(seg live.SegmentInfo) bool { return seg.Start == k })
	require.GreaterOrEqual(t, at, 0, "a segment starts at K")
	seg := snap.Segments[at]
	assert.True(t, seg.Discontinuity, "the segment at K follows the loss")
	msn := seg.Seq
	for track := range len(m4bLadder(bin).Renditions) {
		for _, f := range []struct{ dir, ext string }{{"hls", "ts"}, {"cmaf", "m4s"}} {
			pl := mediaPlaylist(t, fmt.Sprintf("%s%s/v%d.m3u8", base, f.dir, track))
			i := int(msn - pl.MediaSequence)
			require.Less(t, i, len(pl.Segments))
			e := pl.Segments[i]
			if track == gapRung {
				assert.True(t, e.Gap, "%s v%d: a gap at msn %d", f.dir, track, msn)
				assert.Equal(t, fmt.Sprintf("gap-%d-%d.%s", track, msn, f.ext), e.URI, "%s v%d", f.dir, track)
			} else {
				assert.False(t, e.Gap, "%s v%d: a real entry at msn %d", f.dir, track, msn)
			}
		}
	}
	// The other segments list rung 2 for real.
	for _, other := range snap.Segments {
		if other.Seq != msn {
			assert.True(t, slices.ContainsFunc(other.VariantTS, func(v live.VariantTS) bool { return v.Track == rung }),
				"segment %d has rung %d", other.Seq, rung)
		}
	}

	// Read back: rung 2 without the GOP dropped and without the rest of
	// the segment at K (its first keyframe in it was not at K), everything
	// else once. HLS epochs count discontinuities, so the loss splits them
	// at K; compared over the stream, as the one DASH period.
	flat := func(frames []media.Frame) []media.Frame {
		out := slices.Clone(frames)
		for i := range out {
			out[i].Epoch = 1
		}
		return out
	}
	var want []media.Frame
	for _, d := range kept {
		f := d.Frame
		if f.Track == rung && f.PTS >= seg.Start && f.PTS < seg.Start+seg.Duration {
			continue
		}
		want = append(want, f)
	}
	// hls-fmp4 and dash: everything once, the audio in its own rendition.
	// hls-ts: the video likewise, and the audio in every variant but v2,
	// whose TS the gap left out at K.
	checked := map[string]conform.Capture{}
	for name, c := range caps {
		c.Frames = flat(c.Frames)
		if name == "hls-ts" {
			c.Frames = slices.DeleteFunc(c.Frames, func(f media.Frame) bool { return f.Kind() == media.KindAudio })
		}
		checked[name] = c
	}
	videoOnly := slices.DeleteFunc(flat(want), func(f media.Frame) bool {
		return f.Kind() == media.KindAudio
	})
	for _, f := range abrFormats {
		w := flat(want)
		if f.name == "hls-ts" {
			w = videoOnly
		}
		assertCompleteABR(t, map[string]conform.Capture{f.name: checked[f.name]}, w)
	}
	audio := len(want) - len(videoOnly)
	inK := tsAudioFrames(t, seg, transcode.PresentationTrack(0))
	require.Positive(t, inK)
	_, ca := byTrack(caps["hls-ts"].Frames)
	counts := make([]int, 0, len(ca))
	for _, tr := range sortedKeys(ca) {
		counts = append(counts, len(ca[tr]))
	}
	want4 := []int{audio, audio, audio - inK, audio} // per variant, in order
	assert.Equal(t, want4, counts, "hls-ts audio per variant: v2 lacks the %d frames of the segment at K", inK)
	es := map[uint32]bool{}
	for _, f := range caps["hls-ts"].Frames {
		es[f.Epoch] = true
	}
	assert.Equal(t, map[uint32]bool{1: true, 2: true}, es, "hls-ts: the loss is a discontinuity")

	ffmpeg := testpeer.Require(t, "ffmpeg")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-i", fmt.Sprintf("%shls/v%d.m3u8", base, gapRung), "-f", "null", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Run(), stderr.String())
	assert.Empty(t, stderr.String(), "ffmpeg stderr")
}
