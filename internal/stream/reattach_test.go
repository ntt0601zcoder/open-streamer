package stream

import (
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/mux/fmp4"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp/rtmptest"
)

// reattach handles exactly the outputs spec.Outputs has. An output it does
// not know would be accepted by the API, stored, and silently never served
// or never stopped.
func TestReattachCoversEveryOutput(t *testing.T) {
	known := []string{"HLS", "DASH", "HTTPTS", "RTMP", "RTSP", "SRT", "SRTPassphrase", "Push"}
	typ := reflect.TypeFor[spec.Outputs]()
	got := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		got = append(got, name)
		assert.True(t, slices.Contains(known, name),
			"spec.Outputs has a new field %s: teach reattach (generation.go) and off() about it, then add it here", name)
	}
	for _, name := range known {
		assert.True(t, slices.Contains(got, name),
			"spec.Outputs lost its field %s: remove it from reattach (generation.go) and off(), then from here", name)
	}
}

// Every prefix off returns is non-empty and ends in a space: hub.Refuse("")
// would end every subscription of the hub, and a prefix without the space
// would match another output's subscribers ("rtmp" matches "rtmppush …").
func TestOffPrefixesNonEmpty(t *testing.T) {
	for mask := range 16 {
		o := spec.Outputs{HTTPTS: mask&1 != 0, RTMP: mask&2 != 0, RTSP: mask&4 != 0, SRT: mask&8 != 0}
		prefixes := off(o)
		want := 0
		for _, on := range []bool{o.HTTPTS, o.RTMP, o.RTSP, o.SRT} {
			if !on {
				want++
			}
		}
		assert.Len(t, prefixes, want, "%+v", o)
		for _, p := range prefixes {
			assert.NotEmpty(t, p, "%+v", o)
			assert.True(t, strings.HasSuffix(p, " "), "%+v: prefix %q", o, p)
			assert.False(t, strings.HasPrefix("rtmppush rtmp://h/app/ke…", p), "%+v: prefix %q refuses pushes", o, p)
			assert.False(t, strings.HasPrefix("packager cam", p), "%+v: prefix %q refuses packagers", o, p)
			assert.False(t, strings.HasPrefix("transcode", p), "%+v: prefix %q refuses the transcoder", o, p)
		}
		// HLS, DASH and pushes are not session outputs: they change nothing.
		o.HLS, o.DASH, o.Push = true, true, []spec.Push{{URL: "rtmp://h/a/k"}}
		assert.Equal(t, prefixes, off(o))
	}
	assert.Equal(t, []string{"httpts ", "rtmp ", "rtsp ", "srt "}, off(spec.Outputs{}))
	assert.Empty(t, off(spec.Outputs{HTTPTS: true, RTMP: true, RTSP: true, SRT: true}))
	assert.Equal(t, []string{"rtmp "}, both([]string{"httpts ", "rtmp "}, []string{"rtmp ", "srt "}))
	assert.Empty(t, both(off(spec.Outputs{}), nil))
}

func subscribers(h *hub.Hub) []string {
	stats := h.Stats()
	out := make([]string, 0, len(stats))
	for _, st := range stats {
		out = append(out, st.Name)
	}
	return out
}

// The packagers run while HLS or DASH is on, are kept across an edit that
// keeps either, and stop when both are off, without touching the sessions.
func TestSupervisorPackagerEdit(t *testing.T) {
	r := newRig(t)
	sp := camSpec()
	s := r.start(sp)
	waitRunning(t, s)
	v := s.View()
	h := v.Single["cam"]
	session := tap(t, h, "rtmp cam")
	pack := v.Packs["cam"]
	require.NotNil(t, pack)
	segments := func() uint64 {
		segs := pack.Snapshot().Segments
		if len(segs) == 0 {
			return 0
		}
		return segs[len(segs)-1].Seq + 1
	}
	require.Eventually(t, func() bool { return segments() > 0 }, waitFor, 5*time.Millisecond)

	dashOnly := outputEdit(sp, OutHLS, false)
	require.Equal(t, spec.ChangeOutputs, answer(t, s.Submit(dashOnly)))
	now := s.View()
	assert.Same(t, pack, now.Packs["cam"], "the packager was replaced though DASH stayed on")
	assert.False(t, now.Outputs.HLS)
	assert.True(t, now.Outputs.DASH)
	had := segments()
	require.Eventually(t, func() bool { return segments() > had }, waitFor, 5*time.Millisecond, "the packager stopped cutting segments")

	neither := outputEdit(dashOnly, OutDASH, false)
	require.Equal(t, spec.ChangeOutputs, answer(t, s.Submit(neither)))
	assert.Empty(t, s.View().Packs)
	assert.NotContains(t, subscribers(h), "packager cam")
	assert.Contains(t, subscribers(h), "rtmp cam")

	hlsOnly := outputEdit(neither, OutHLS, true)
	require.Equal(t, spec.ChangeOutputs, answer(t, s.Submit(hlsOnly)))
	fresh := s.View().Packs["cam"]
	require.NotNil(t, fresh)
	assert.NotSame(t, pack, fresh)
	assert.Contains(t, subscribers(h), "packager cam")
	require.Eventually(t, func() bool { return len(fresh.Snapshot().Segments) > 0 }, waitFor, 5*time.Millisecond)
	// It joined a live hub: its first segment starts every track at the
	// keyframe, not with the audio the cached GOP holds from before it.
	snap := fresh.Snapshot()
	require.Len(t, snap.Segments[0].Parts, 2)
	var starts [2]float64
	for i, part := range snap.Segments[0].Parts {
		tr, ok := snap.Track(part.Track)
		require.True(t, ok)
		starts[i] = float64(part.Timing.Earliest) / float64(fmp4.Timescale(tr.Config))
	}
	assert.InDelta(t, starts[0], starts[1], 0.03, "a packager started mid-stream opens with audio only")

	session.more(t, 20)
	assert.Zero(t, session.lost.Load(), "the session lost frames over the packager edits")
	assert.False(t, session.closed())
	assert.Equal(t, uint64(1), s.View().Gen)
	assert.Same(t, h, s.View().Single["cam"])
}

// Pushes are keyed by URL: an edit stops the ones removed, starts the ones
// added and leaves the rest connected.
func TestSupervisorPushEdit(t *testing.T) {
	r := newRig(t)
	sinks := map[string]*rtmptest.PublishServer{}
	urls := map[string]string{}
	for _, n := range []string{"a", "b", "c"} {
		sinks[n] = rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{})
		urls[n] = sinks[n].URL("live/" + vKey + n)
	}
	sp := camSpec()
	sp.Outputs.Push = []spec.Push{{URL: urls["a"]}, {URL: urls["b"]}}
	s := r.start(sp)
	waitRunning(t, s)
	require.Eventually(t, func() bool {
		return len(sinks["a"].Requests()) == 1 && len(sinks["b"].Requests()) == 1
	}, waitFor, time.Millisecond)
	h := s.View().Single["cam"]

	next := sp
	next.Outputs.Push = []spec.Push{{URL: urls["b"]}, {URL: urls["c"]}}
	require.Equal(t, spec.ChangeOutputs, answer(t, s.Submit(next)))
	require.Eventually(t, func() bool { return len(sinks["a"].Ends()) == 1 }, waitFor, time.Millisecond, "A's connection did not end")
	require.Eventually(t, func() bool { return len(sinks["c"].Requests()) == 1 }, waitFor, time.Millisecond, "C got no publish")
	st := s.Status()
	require.Len(t, st.Pushes, 2)
	assert.Equal(t, []string{pushID("cam", urls["b"]), pushID("cam", urls["c"])}, []string{st.Pushes[0].ID, st.Pushes[1].ID})
	require.Eventually(t, func() bool { return pushStats(t, s, urls["c"]).State == "publishing" }, waitFor, time.Millisecond)

	// A reordering is an outputs edit that touches no push.
	reordered := next
	reordered.Outputs.Push = []spec.Push{{URL: urls["c"]}, {URL: urls["b"]}}
	require.Equal(t, spec.ChangeOutputs, answer(t, s.Submit(reordered)))
	assert.Equal(t, pushID("cam", urls["c"]), s.Status().Pushes[0].ID)

	time.Sleep(50 * time.Millisecond)
	assert.Len(t, sinks["b"].Requests(), 1, "B was reconnected")
	assert.Empty(t, sinks["b"].Ends(), "B's connection ended")
	assert.Equal(t, uint64(1), pushStats(t, s, urls["b"]).Connects)
	assert.Len(t, sinks["c"].Requests(), 1)
	assert.Len(t, sinks["a"].Requests(), 1)
	var pushes int
	for _, name := range subscribers(h) {
		if strings.HasPrefix(name, "rtmppush ") {
			pushes++
		}
	}
	assert.Equal(t, 2, pushes)
	assert.Equal(t, uint64(1), s.View().Gen)
}

// A changed SRT passphrase (set, changed or removed) ends the stream's SRT
// sessions, which were admitted under the old one, and no other session: the
// View that holds the new passphrase is published first, so a player that
// subscribes after the edit is asked for the new one. An edit that keeps the
// passphrase ends none.
func TestReattachPassphrase(t *testing.T) {
	r := newRig(t)
	// At each publish: the passphrase the View holds, and the SRT sessions
	// still subscribed at that moment.
	type seen struct {
		passphrase string
		srt        []string
	}
	var (
		mu  sync.Mutex
		pub []seen
		hb  atomic.Pointer[hub.Hub]
	)
	r.published = func(v *View) {
		var srt []string
		if h := hb.Load(); h != nil {
			for _, name := range subscribers(h) {
				if strings.HasPrefix(name, "srt ") {
					srt = append(srt, name)
				}
			}
		}
		mu.Lock()
		pub = append(pub, seen{v.Outputs.SRTPassphrase, srt})
		mu.Unlock()
	}
	lastPublished := func() seen {
		mu.Lock()
		defer mu.Unlock()
		require.NotEmpty(t, pub)
		return pub[len(pub)-1]
	}
	sink := rtmptest.NewPublishServer(t, rtmptest.PublishServerOptions{})
	sp := camSpec()
	s := r.start(sp)
	waitRunning(t, s)
	h := s.View().Single["cam"]
	hb.Store(h)
	others := []*tapper{tap(t, h, "rtsp c"), tap(t, h, "httpts d")}
	srts := []*tapper{tap(t, h, "srt a"), tap(t, h, "srt b")}

	for _, passphrase := range []string{"p1-0123456789", "p2-0123456789", ""} {
		next := sp
		next.Outputs.SRTPassphrase = passphrase
		require.Equal(t, spec.ChangeOutputs, answer(t, s.Submit(next)), "to %q", passphrase)
		at := lastPublished()
		assert.Equal(t, passphrase, at.passphrase)
		assert.Len(t, at.srt, len(srts), "to %q: an SRT session ended before the View was published", passphrase)
		assert.Equal(t, passphrase, s.View().Outputs.SRTPassphrase)
		for _, tp := range srts {
			require.Eventually(t, tp.closed, waitFor, time.Millisecond, "to %q: an SRT session survived", passphrase)
		}
		for _, tp := range others {
			tp.more(t, 20)
			assert.False(t, tp.closed(), "to %q: another output's session ended", passphrase)
		}
		e := tap(t, h, "srt e")
		e.more(t, 10)
		assert.False(t, e.closed(), "to %q: a new SRT session was refused", passphrase)
		srts = []*tapper{e, tap(t, h, "srt a"), tap(t, h, "srt b")}
		sp = next
	}

	// The passphrase is kept and only a push changes: no session ends.
	keep := sp
	keep.Outputs.SRTPassphrase = "p3-0123456789"
	require.Equal(t, spec.ChangeOutputs, answer(t, s.Submit(keep)))
	for _, tp := range srts {
		require.Eventually(t, tp.closed, waitFor, time.Millisecond)
	}
	srts = []*tapper{tap(t, h, "srt a"), tap(t, h, "srt b")}
	push := keep
	push.Outputs.Push = []spec.Push{{URL: sink.URL("live/" + vKey + "p")}}
	require.Equal(t, spec.ChangeOutputs, answer(t, s.Submit(push)))
	require.Eventually(t, func() bool { return len(sink.Requests()) == 1 }, waitFor, time.Millisecond, "the push did not start")
	for _, tp := range append(slices.Clone(srts), others...) {
		tp.more(t, 20)
		assert.False(t, tp.closed(), "a session ended over a push edit")
	}

	// SRT turned off with the passphrase removed: off() ends the SRT
	// sessions, and a new one is refused.
	srtOff := push
	srtOff.Outputs.SRT, srtOff.Outputs.SRTPassphrase = false, ""
	require.Equal(t, spec.ChangeOutputs, answer(t, s.Submit(srtOff)))
	for _, tp := range srts {
		require.Eventually(t, tp.closed, waitFor, time.Millisecond, "an SRT session survived srt off")
	}
	refused := h.Subscribe("srt e", 8)
	_, open := <-refused.C()
	assert.False(t, open, "a new SRT session was accepted while srt is off")
	for _, tp := range others {
		tp.more(t, 20)
		assert.False(t, tp.closed())
	}
	assert.Equal(t, uint64(1), s.View().Gen)
	assert.Zero(t, s.Status().Rebuilds)

	// A ladder: the SRT sessions of every rung's hub end too, and only those.
	lr := newRig(t)
	fakeChild(t, &lr.env)
	lsp := ladderSpec("720", "360")
	ls := lr.start(lsp)
	waitRunning(t, ls)
	lv := ls.View()
	rungs := []string{"cam_720", "cam_360"}
	rungSRT := map[string]*tapper{}
	rungOther := map[string]*tapper{}
	for _, name := range rungs {
		rh := lv.Single[name]
		require.NotNil(t, rh, name)
		rungSRT[name] = tap(t, rh, "srt "+name)
		rungOther[name] = tap(t, rh, "rtsp "+name)
		rungSRT[name].more(t, 5)
	}
	next := lsp
	next.Outputs.SRTPassphrase = "p4-0123456789"
	require.Equal(t, spec.ChangeOutputs, answer(t, ls.Submit(next)))
	for _, name := range rungs {
		require.Eventually(t, rungSRT[name].closed, waitFor, time.Millisecond, "%s: an SRT session survived the passphrase change", name)
	}
	for _, name := range rungs {
		rungOther[name].more(t, 10)
		assert.False(t, rungOther[name].closed(), "%s: another output's session ended", name)
		fresh := tap(t, lv.Single[name], "srt "+name+" new")
		fresh.more(t, 5)
		assert.False(t, fresh.closed(), "%s: a new SRT session was refused after the relax", name)
	}
	assert.Equal(t, lv.Gen, ls.View().Gen)
	assert.Zero(t, ls.Status().Rebuilds)
}
