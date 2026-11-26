package server

import (
	"github.com/ntt0601zcoder/open-streamer/internal/asset"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics"
	"github.com/ntt0601zcoder/open-streamer/internal/rtmpsrv"
)

// reason is one "why it ended" or "why it was refused" counter.
type reason struct {
	name string
	n    uint64
}

// collect writes the config's, the assets' and the output servers' series.
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

	// No series without server.tls; the certificate is asked of its store
	// at each scrape, as a poll changes it outside cfg.
	if s.store != nil {
		s.collectTLS(w)
	}

	// No series without an assets directory, nor while it cannot be read.
	if t := s.assets.Totals(); t != nil {
		kinds := []asset.Kind{asset.Image, asset.Font}
		for _, k := range kinds {
			w.Gauge("openstreamer_assets", "Assets in the assets directory, by kind.", float64(t[k][0]), metrics.L("kind", string(k)))
		}
		for _, k := range kinds {
			w.Gauge("openstreamer_assets_bytes", "Bytes of the assets in the assets directory, by kind.", float64(t[k][1]), metrics.L("kind", string(k)))
		}
	}

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

// collectTLS writes the certificate's series and the failed handshakes of
// each TLS listener that is on.
func (s *Server) collectTLS(w *metrics.Writer) {
	info := s.store.Info()
	w.Gauge("openstreamer_tls_certificate_not_after_seconds", "When the certificate in force expires, in Unix seconds.",
		float64(info.NotAfter.Unix()))
	// The result values are those of openstreamer_config_reloads_total, and
	// unchanged: a read that found the same leaf.
	const reloads = "openstreamer_tls_certificate_reloads_total"
	const reloadsHelp = "Reads of the certificate pair, the start's included, by result."
	w.Counter(reloads, reloadsHelp, float64(info.Changed), metrics.L("result", "ok"))
	w.Counter(reloads, reloadsHelp, float64(info.Unchanged), metrics.L("result", "unchanged"))
	w.Counter(reloads, reloadsHelp, float64(info.Failed), metrics.L("result", "error"))
	const failures = "openstreamer_tls_handshake_failures_total"
	const failuresHelp = "TLS handshakes that failed, by listener."
	if s.mediaTLS != nil {
		w.Counter(failures, failuresHelp, float64(s.httpsFailures.Load()), metrics.L("listener", "https"))
	}
	if s.apiTLS != nil {
		w.Counter(failures, failuresHelp, float64(s.apiFailures.Load()), metrics.L("listener", "api"))
	}
	if s.addrs.RTMPS != "" {
		n := rtmpsrv.Default.Stats(s.addrs.RTMPS).TLSHandshakeFailures
		w.Counter(failures, failuresHelp, float64(n), metrics.L("listener", "rtmps"))
	}
	if s.addrs.RTSPS != "" {
		n := s.rtsp.Stats().Server.TLSHandshakeFailures
		w.Counter(failures, failuresHelp, float64(n), metrics.L("listener", "rtsps"))
	}
}
