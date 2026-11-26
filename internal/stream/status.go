package stream

import (
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics"
	"github.com/ntt0601zcoder/open-streamer/internal/output/live"
	"github.com/ntt0601zcoder/open-streamer/internal/output/rtmppush"
	"github.com/ntt0601zcoder/open-streamer/internal/pipeline"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
)

// View is what one generation serves. It is immutable; the supervisor swaps
// it.
type View struct {
	Gen     uint64                    // the generation, from 1; 0 between generations
	Outputs spec.Outputs              // the outputs that are on
	Single  map[string]*hub.Hub       // served name → the hub of HTTP-TS, RTMP, RTSP, SRT and push
	Packs   map[string]*live.Packager // served name → its packager; empty while neither HLS nor DASH is on
	Source  *hub.Hub                  // the source hub: copy://<name>
}

// The states of a stream.
const (
	stateStopped  = "stopped"  // no supervisor runs it
	stateStarting = "starting" // no input has been active in this generation
	stateRunning  = "running"  // an input is active
	stateNoInput  = "no_input" // an input was active and none is now
	stateBackoff  = "backoff"  // between generations after a failure
)

// Status is a stream's state for the API.
type Status struct {
	Name        string       `json:"name"`
	State       string       `json:"state"` // "stopped", "starting", "running", "no_input", "backoff"
	Generation  uint64       `json:"generation"`
	Since       time.Time    `json:"since"`                   // when the generation started
	Restarts    uint64       `json:"restarts"`                // rebuilds after a failure
	Rebuilds    uint64       `json:"rebuilds"`                // rebuilds for an edit
	LastError   string       `json:"last_error,omitempty"`    // scrubbed; prefixed "input N: ", "inputs: ", "watermark: ", "transcoder: ", "thumbnailer: " or "build: "
	LastErrorAt *time.Time   `json:"last_error_at,omitempty"` // when LastError was recorded
	Served      []string     `json:"served"`
	Outputs     spec.Outputs `json:"outputs"`      // push URLs redacted
	Inputs      []string     `json:"inputs"`       // redacted
	ActiveInput int          `json:"active_input"` // -1 when none
	Switches    uint64       `json:"input_switches"`
	Tracks      []Track      `json:"tracks"` // the source hub's live tracks
	Transcoder  *Transcoder  `json:"transcoder,omitempty"`
	Pushes      []Push       `json:"pushes,omitempty"`
}

// Track is one live track of the source hub.
type Track struct {
	Track int    `json:"track"`
	Kind  string `json:"kind"`
	Codec string `json:"codec"`
}

// Transcoder is the state of a stream's transcoder or thumbnailer.
type Transcoder struct {
	PID              int    `json:"pid"`
	Spawns           uint64 `json:"spawns"`
	Exits            uint64 `json:"exits"`
	EncoderFallbacks uint64 `json:"encoder_fallbacks"`
	EncoderRefusals  uint64 `json:"encoder_refusals"`
	Thumbnails       uint64 `json:"thumbnails"`
}

// Push is the state of one push.
type Push struct {
	ID        string `json:"id"`     // the first 8 hex digits of a keyed tag of the URL (new at every start): the metrics' target label
	Target    string `json:"target"` // redacted
	State     string `json:"state"`
	Connects  uint64 `json:"connects"`
	Failures  uint64 `json:"failures"`
	LastError string `json:"last_error,omitempty"`
}

// snapshot is what Status and Collect read of the supervisor, taken under
// its lock. The generation's own counters are read after it.
type snapshot struct {
	sp                 spec.Stream
	g                  *generation
	pushes             []*pushRun
	backoff, stopped   bool
	restarts, rebuilds uint64
	lastErr            string
	lastErrAt          time.Time
}

func (s *Supervisor) snapshot() snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return snapshot{
		sp: s.spec, g: s.cur, pushes: s.pushes, backoff: s.backoff, stopped: s.stopped,
		restarts: s.restarts, rebuilds: s.rebuilds, lastErr: s.lastErr, lastErrAt: s.lastErrAt,
	}
}

// state derives the stream's state. With a generation it comes from the
// pipeline, not from the hub: Hub.Live stays true once a keyframe was
// written, whatever happens to the input.
func (n snapshot) state(active int) string {
	switch {
	case n.stopped:
		return stateStopped
	case n.g == nil && n.backoff:
		return stateBackoff
	case n.g == nil:
		return stateStarting
	case active >= 0:
		return stateRunning
	case n.g.active.Load():
		return stateNoInput
	default:
		return stateStarting
	}
}

// pipeStats returns the generation's pipeline counters; without a
// generation no input is active.
func (n snapshot) pipeStats() pipeline.Stats {
	if n.g == nil {
		return pipeline.Stats{Active: -1}
	}
	return n.g.pipe.Stats()
}

// state returns the stream's state alone, for the manager's count.
func (s *Supervisor) state() string {
	n := s.snapshot()
	return n.state(n.pipeStats().Active)
}

// Status returns the stream's state. It is safe from any goroutine.
func (s *Supervisor) Status() Status {
	n := s.snapshot()
	ps := n.pipeStats()
	red := n.sp.Redacted()
	st := Status{
		Name:        s.name,
		State:       n.state(ps.Active),
		Restarts:    n.restarts,
		Rebuilds:    n.rebuilds,
		LastError:   n.lastErr,
		Served:      n.sp.Served(),
		Outputs:     red.Outputs,
		Inputs:      inputsOf(red),
		ActiveInput: ps.Active,
		Switches:    ps.Switches,
		Tracks:      []Track{},
	}
	if n.lastErr != "" {
		at := n.lastErrAt
		st.LastErrorAt = &at
	}
	g := n.g
	if g == nil {
		return st
	}
	st.Generation, st.Since = g.n, g.since
	for _, tc := range g.source.TrackConfigs() {
		t := Track{Track: tc.Track, Kind: kindName(tc.Kind), Codec: "unknown"}
		if tc.Config != nil {
			t.Codec = tc.Config.Codec.String()
		}
		st.Tracks = append(st.Tracks, t)
	}
	if tr, ok := g.child(); ok {
		st.Transcoder = &Transcoder{
			PID: tr.PID, Spawns: tr.spawns, Exits: tr.exits,
			EncoderFallbacks: tr.Child.EncoderFallbacks, EncoderRefusals: tr.EncoderRefusals, Thumbnails: tr.thumbnails,
		}
	}
	for _, r := range n.pushes {
		p := r.p.Stats()
		st.Pushes = append(st.Pushes, Push{
			ID: r.id, Target: r.p.String(), State: string(p.State),
			Connects: p.Connects, Failures: p.Failures, LastError: p.LastError,
		})
	}
	return st
}

func kindName(k media.Kind) string {
	switch k {
	case media.KindVideo:
		return "video"
	case media.KindAudio:
		return "audio"
	default:
		return "unknown"
	}
}

// childStats are the counters of a generation's transcoder and thumbnailer
// together: a stream has a transcoder, a thumbnailer, or (a ladder that only
// copies, with a thumbnail) both, and then only the thumbnailer has a child.
type childStats struct {
	transcode.Stats        // the transcoder's; zero without a ladder
	ladder          bool   // the stream has a transcoder
	up              bool   // a child is running
	spawns, exits   uint64 // of both
	thumbnails      uint64 // shots made, by either
	thumbsFailed    uint64 // shots that could not be made, by either
}

// child returns the generation's transcoder and thumbnailer counters; ok is
// false for a stream with neither.
func (g *generation) child() (childStats, bool) {
	var c childStats
	if g.tr == nil && g.nailer == nil {
		return c, false
	}
	if g.tr != nil {
		c.Stats = g.tr.Stats()
		c.ladder = true
		c.up = c.PID != 0
		c.spawns, c.exits = c.Spawns, c.Exits
		c.thumbnails, c.thumbsFailed = c.Child.Thumbnails, c.Child.ThumbnailsFailed
	}
	if g.nailer != nil {
		n := g.nailer.Stats()
		// The thumbnailer reports no PID: its child runs between a spawn and
		// the exit that follows it.
		c.up = c.up || n.Spawns > n.Exits
		c.spawns += n.Spawns
		c.exits += n.Exits
		c.thumbnails += n.Shots
		c.thumbsFailed += n.Failed
	}
	return c, true
}

// Shot returns the stream's latest thumbnail and when it arrived; ok is
// false while there is none, and for a stream without a thumbnail.
func (s *Supervisor) Shot() (shot picture.Shot, at time.Time, ok bool) {
	s.mu.Lock()
	g := s.cur
	s.mu.Unlock()
	if g == nil || g.shot == nil {
		return picture.Shot{}, time.Time{}, false
	}
	return g.shot()
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Collect writes the stream's series, each with stream=<name>. The series
// of the stream itself are always written; those of its hubs, packagers,
// transcoder and pushes only while a generation runs, and they count from
// zero in every generation.
func (s *Supervisor) Collect(w *metrics.Writer) {
	n := s.snapshot()
	ps := n.pipeStats()
	stream := metrics.L("stream", s.name)
	l := func(pairs ...string) []metrics.Label {
		out := []metrics.Label{stream}
		for i := 0; i+1 < len(pairs); i += 2 {
			out = append(out, metrics.L(pairs[i], pairs[i+1]))
		}
		return out
	}
	var gen uint64
	if n.g != nil {
		gen = n.g.n
	}
	w.Gauge("openstreamer_stream_up", "Whether the stream has an active input.", b2f(n.state(ps.Active) == stateRunning), stream)
	w.Gauge("openstreamer_stream_generation", "The generation the stream serves, 0 between generations.", float64(gen), stream)
	const restarts = "Rebuilds of the stream, by reason."
	w.Counter("openstreamer_stream_restarts_total", restarts, float64(n.restarts), l("reason", "failure")...)
	w.Counter("openstreamer_stream_restarts_total", restarts, float64(n.rebuilds), l("reason", "edit")...)
	w.Gauge("openstreamer_stream_active_input", "The index of the active input, -1 when none.", float64(ps.Active), stream)
	w.Counter("openstreamer_stream_input_switches_total", "Switches of the active input.", float64(ps.Switches), stream)
	w.Counter("openstreamer_stream_input_eof_total", "Inputs that ended and were restarted.", float64(s.eofs.Load()), stream)
	w.Counter("openstreamer_stream_unmapped_frames_total", "Frames of input tracks outside the layout.", float64(ps.Unmapped), stream)
	w.Counter("openstreamer_stream_drift_breaks_total", "Timeline breaks made for clock drift.", float64(ps.Clock.DriftBreaks), stream)
	g := n.g
	if g == nil {
		return
	}
	v := s.View()
	if v.Gen != g.n {
		// Between the snapshot and here the generation ended, or it is not
		// published yet: its hubs and packagers are not served.
		v = &View{}
	}
	for _, name := range slices.Sorted(maps.Keys(v.Single)) {
		w.Gauge("openstreamer_hub_subscribers", "Subscribers of a served hub.", float64(len(v.Single[name].Stats())), l("name", name)...)
	}
	const packErrs = "Packager errors, by kind."
	for _, name := range slices.Sorted(maps.Keys(v.Packs)) {
		p := v.Packs[name]
		w.Counter("openstreamer_packager_errors_total", packErrs, float64(p.Errors()), l("name", name, "kind", "mux")...)
		w.Counter("openstreamer_packager_errors_total", packErrs, float64(p.CodecErrors()), l("name", name, "kind", "codec")...)
		w.Counter("openstreamer_packager_errors_total", packErrs, float64(p.FallbackAnchors()), l("name", name, "kind", "anchor")...)
	}
	for _, name := range slices.Sorted(maps.Keys(v.Packs)) {
		rungs := v.Packs[name].Rungs()
		for _, track := range slices.Sorted(maps.Keys(rungs)) {
			w.Counter("openstreamer_packager_rung_gaps_total", "Segments an ABR presentation packaged without a rung.",
				float64(rungs[track].Gaps), l("name", name, "track", strconv.Itoa(track))...)
		}
	}
	if c, ok := g.child(); ok {
		collectChild(w, l, g, c)
	}
	pushes := make([]rtmppush.Stats, len(n.pushes))
	for i, r := range n.pushes {
		pushes[i] = r.p.Stats()
		w.Gauge("openstreamer_push_up", "Whether the push is publishing.", b2f(pushes[i].State == rtmppush.StatePublishing), l("target", r.id)...)
	}
	for _, f := range []struct {
		name, help string
		get        func(rtmppush.Stats) uint64
	}{
		{"openstreamer_push_connects_total", "Publishes the push target accepted.", func(p rtmppush.Stats) uint64 { return p.Connects }},
		{"openstreamer_push_failures_total", "Push connections and attempts that failed.", func(p rtmppush.Stats) uint64 { return p.Failures }},
		{"openstreamer_push_republishes_total", "Push connections published again for a codec change.", func(p rtmppush.Stats) uint64 { return p.Republishes }},
		{"openstreamer_push_messages_total", "RTMP messages written to the push target.", func(p rtmppush.Stats) uint64 { return p.Messages }},
		{"openstreamer_push_resyncs_total", "Deliveries to the push that reported lost frames.", func(p rtmppush.Stats) uint64 { return p.Resyncs }},
	} {
		for i, r := range n.pushes {
			w.Counter(f.name, f.help, float64(f.get(pushes[i])), l("target", r.id)...)
		}
	}
}

// collectChild writes the transcoder families: those of the child for any
// stream with a transcoder or a thumbnailer, the rest for a ladder only.
func collectChild(w *metrics.Writer, l func(...string) []metrics.Label, g *generation, c childStats) {
	w.Gauge("openstreamer_transcoder_up", "Whether the transcoder child is running.", b2f(c.up), l()...)
	w.Counter("openstreamer_transcoder_spawns_total", "Transcoder children started.", float64(c.spawns), l()...)
	w.Counter("openstreamer_transcoder_exits_total", "Transcoder children that exited or failed to start.", float64(c.exits), l()...)
	if c.ladder {
		w.Counter("openstreamer_transcoder_losses_total", "Losses on the transcoder's source subscription.", float64(c.Losses), l()...)
		w.Counter("openstreamer_transcoder_frames_in_total", "Frames the transcoder read from the source.", float64(c.FramesIn), l()...)
		for i, n := range c.FramesOut {
			if i < len(g.labels) {
				w.Counter("openstreamer_transcoder_frames_out_total", "Frames written to a rendition.", float64(n), l("rendition", g.labels[i])...)
			}
		}
		const dropped = "Frames the transcoder dropped, by reason."
		for _, d := range []struct {
			reason string
			n      uint64
		}{
			{"before_key", c.DroppedBeforeKey},
			{"merge", c.MergeDropped},
			{"oversize", c.Oversize},
			{"presentation", c.PresentationDropped},
		} {
			w.Counter("openstreamer_transcoder_dropped_frames_total", dropped, float64(d.n), l("reason", d.reason)...)
		}
		w.Counter("openstreamer_transcoder_encoder_fallbacks_total", "Rungs that fell back to the software encoder.",
			float64(c.Child.EncoderFallbacks), l()...)
		w.Counter("openstreamer_transcoder_encoder_refusals_total",
			"Transcoder children that ended because a no_fallback rendition could not open its encoder.",
			float64(c.EncoderRefusals), l()...)
		const mediaErrs = "Media errors the transcoder child reported, by kind."
		for _, e := range []struct {
			kind string
			n    uint64
		}{
			{"audio_corrupt", c.Child.AudioCorrupt},
			{"video_corrupt", c.Child.VideoCorrupt},
			{"audio_dropped", c.Child.AudioDropped},
			{"reanchors", c.Child.Reanchors},
			{"dropped_tracks", c.Child.DroppedTracks},
		} {
			w.Counter("openstreamer_transcoder_media_errors_total", mediaErrs, float64(e.n), l("kind", e.kind)...)
		}
	}
	const thumbs = "Thumbnails, by result."
	w.Counter("openstreamer_transcoder_thumbnails_total", thumbs, float64(c.thumbnails), l("result", "ok")...)
	w.Counter("openstreamer_transcoder_thumbnails_total", thumbs, float64(c.thumbsFailed), l("result", "failed")...)
}
