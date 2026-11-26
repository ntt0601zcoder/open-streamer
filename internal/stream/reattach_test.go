package stream

import (
	"reflect"
	"slices"
	"strings"
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
	known := []string{"HLS", "DASH", "HTTPTS", "RTMP", "RTSP", "SRT", "Push"}
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
	var starts []float64
	for _, part := range snap.Segments[0].Parts {
		tr, ok := snap.Track(part.Track)
		require.True(t, ok)
		starts = append(starts, float64(part.Timing.Earliest)/float64(fmp4.Timescale(tr.Config)))
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
	waitState(t, s, "running")
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
	assert.Equal(t, []string{pushID(urls["b"]), pushID(urls["c"])}, []string{st.Pushes[0].ID, st.Pushes[1].ID})
	require.Eventually(t, func() bool { return pushStats(t, s, urls["c"]).State == "publishing" }, waitFor, time.Millisecond)

	// A reordering is an outputs edit that touches no push.
	reordered := next
	reordered.Outputs.Push = []spec.Push{{URL: urls["c"]}, {URL: urls["b"]}}
	require.Equal(t, spec.ChangeOutputs, answer(t, s.Submit(reordered)))
	assert.Equal(t, pushID(urls["c"]), s.Status().Pushes[0].ID)

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
