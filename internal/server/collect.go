package server

import (
	"github.com/ntt0601zcoder/open-streamer/internal/metrics"
)

// reason is one "why it ended" or "why it was refused" counter.
type reason struct {
	name string
	n    uint64
}

// collect writes the config's and the output servers' series.
func (s *Server) collect(w *metrics.Writer) {
	st := s.state.Load().status
	w.Gauge("openstreamer_config_load_errors", "Entries and streams the start skipped or left stopped.", float64(len(st.LoadErrors)))
	const reloads = "openstreamer_config_reloads_total"
	const reloadsHelp = "Reloads of the config file, by result."
	w.Counter(reloads, reloadsHelp, float64(s.reloadsOK.Load()), metrics.L("result", "ok"))
	w.Counter(reloads, reloadsHelp, float64(s.reloadsFailed.Load()), metrics.L("result", "error"))
	restart := 0.0
	if len(st.RestartRequired) > 0 {
		restart = 1
	}
	w.Gauge("openstreamer_config_restart_required", "1 while the file's server section differs from the running one.", restart)

	// Family by family, as the exposition groups them; an output whose
	// listener is off has no series.
	type output struct {
		name            string
		sessions        float64
		started         *uint64 // nil for SRT, which does not count them
		resyncs         uint64
		ended, rejected []reason
	}
	var outs []output
	if s.rtmp != nil {
		x := s.rtmp.Stats()
		outs = append(outs, output{
			name: "rtmp", sessions: float64(x.Players), started: &x.PlayersStarted, resyncs: x.Resyncs,
			ended: []reason{
				{"deadline", x.EndedDeadline},
				{"write_error", x.EndedWriteError},
				{"codec_change", x.EndedCodecChange},
				{"hub_closed", x.EndedHubClosed},
				{"left", x.EndedLeft},
				{"shutdown", x.EndedShutdown},
			},
		})
	}
	if s.rtsp != nil {
		x := s.rtsp.Stats()
		outs = append(outs, output{
			name: "rtsp", sessions: float64(x.Players), started: &x.PlayersStarted, resyncs: x.Resyncs,
			ended: []reason{
				{"deadline", x.EndedDeadline},
				{"write_error", x.EndedWriteError},
				{"codec_change", x.EndedCodecChange},
				{"frame_error", x.EndedFrameError},
				{"hub_closed", x.EndedHubClosed},
				{"tracks_gone", x.EndedTracksGone},
				{"teardown", x.EndedTeardown},
				{"timeout", x.EndedTimeout},
				{"left", x.EndedLeft},
				{"shutdown", x.EndedShutdown},
			},
		})
	}
	if s.srt != nil {
		x := s.srt.Stats()
		outs = append(outs, output{
			name: "srt", sessions: float64(x.Conns), resyncs: x.Resyncs,
			ended: []reason{
				{"stalled", x.EndedStalled},
				{"gone", x.EndedGone},
				{"codec_change", x.EndedCodecChange},
				{"hub_closed", x.EndedHubClosed},
				{"shutdown", x.EndedShutdown},
				{"error", x.EndedError},
			},
			rejected: []reason{
				{"unknown", x.RejectedUnknown},
				{"mode", x.RejectedMode},
				{"caps", x.RejectedCaps},
				{"mss", x.RejectedMSS},
			},
		})
	}
	for _, o := range outs {
		w.Gauge("openstreamer_output_sessions", "Players and receivers being served, by output.", o.sessions, metrics.L("output", o.name))
	}
	for _, o := range outs {
		if o.started != nil {
			w.Counter("openstreamer_output_sessions_started_total", "Players served, by output.", float64(*o.started), metrics.L("output", o.name))
		}
	}
	for _, o := range outs {
		for _, r := range o.ended {
			w.Counter("openstreamer_output_sessions_ended_total", "Players and receivers that ended, by output and reason.",
				float64(r.n), metrics.L("output", o.name), metrics.L("reason", r.name))
		}
	}
	for _, o := range outs {
		w.Counter("openstreamer_output_resyncs_total", "Deliveries that reported lost frames and were resynced, by output.",
			float64(o.resyncs), metrics.L("output", o.name))
	}
	for _, o := range outs {
		for _, r := range o.rejected {
			w.Counter("openstreamer_output_rejected_total", "Connection requests refused, by output and reason.",
				float64(r.n), metrics.L("output", o.name), metrics.L("reason", r.name))
		}
	}
}
