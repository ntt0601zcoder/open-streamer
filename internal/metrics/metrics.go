// Package metrics registers all Prometheus collectors for Open Streamer.
// Naming convention: open_streamer_<module>_<metric>_<unit>
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/samber/do/v2"
)

// Metrics holds all registered Prometheus collectors.
type Metrics struct {
	// ── Ingestor ─────────────────────────────────────────────────────────────
	// IngestorBytesTotal counts raw bytes received from each input source.
	IngestorBytesTotal *prometheus.CounterVec
	// IngestorPacketsTotal counts MPEG-TS packets written to the buffer.
	IngestorPacketsTotal *prometheus.CounterVec
	// IngestorErrorsTotal counts transient read/reconnect errors per stream.
	IngestorErrorsTotal *prometheus.CounterVec

	// ── Stream Manager ────────────────────────────────────────────────────────
	// ManagerFailoversTotal counts input source switches (primary → backup).
	ManagerFailoversTotal *prometheus.CounterVec
	// ManagerInputHealth is 1 when an input is delivering packets, 0 when degraded.
	ManagerInputHealth *prometheus.GaugeVec

	// ── Transcoder ────────────────────────────────────────────────────────────
	// TranscoderWorkersActive is the number of active FFmpeg processes per stream.
	TranscoderWorkersActive *prometheus.GaugeVec
	// TranscoderRestartsTotal counts FFmpeg crash-restarts per stream.
	TranscoderRestartsTotal *prometheus.CounterVec
	// TranscoderQualitiesActive is the number of active ABR renditions per stream.
	TranscoderQualitiesActive *prometheus.GaugeVec

	// ── Buffer ────────────────────────────────────────────────────────────────
	// BufferPackets is the current occupancy (packets) of the ring buffer.
	BufferPackets *prometheus.GaugeVec

	// ── Publisher ─────────────────────────────────────────────────────────────
	// PublisherClientsActive is the number of active viewer connections.
	PublisherClientsActive *prometheus.GaugeVec
	// PublisherSegmentsTotal counts HLS/DASH segments packaged per stream.
	PublisherSegmentsTotal *prometheus.CounterVec

	// ── DVR ───────────────────────────────────────────────────────────────────
	// DVRSegmentsWrittenTotal counts TS segments flushed to disk per stream.
	DVRSegmentsWrittenTotal *prometheus.CounterVec
	// DVRBytesWrittenTotal counts bytes written to disk per stream.
	DVRBytesWrittenTotal *prometheus.CounterVec

	// ── Stream lifecycle ──────────────────────────────────────────────────────
	// StreamStartTimeSeconds is the Unix timestamp when a stream pipeline was
	// last started. 0 / absent means the stream is not currently running.
	// Uptime in Grafana: time() - open_streamer_stream_start_time_seconds
	StreamStartTimeSeconds *prometheus.GaugeVec

	// ── Hooks ─────────────────────────────────────────────────────────────────
	// HooksDeliveryFailedTotal counts hook deliveries that exhausted all retries.
	HooksDeliveryFailedTotal *prometheus.CounterVec
}

// New registers all metrics and returns a Metrics instance.
func New(i do.Injector) (*Metrics, error) {
	_ = i
	m := &Metrics{
		IngestorBytesTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_ingestor_bytes_total",
			Help: "Total bytes ingested from pull/push sources per stream and protocol.",
		}, []string{"stream_code", "protocol"}),

		IngestorPacketsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_ingestor_packets_total",
			Help: "Total MPEG-TS packets written to the buffer per stream and protocol.",
		}, []string{"stream_code", "protocol"}),

		IngestorErrorsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_ingestor_errors_total",
			Help: "Total ingestion errors per stream, categorised by reason (reconnect, failover).",
		}, []string{"stream_code", "reason"}),

		ManagerFailoversTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_manager_failovers_total",
			Help: "Total input source switches (primary degraded → backup activated) per stream.",
		}, []string{"stream_code"}),

		ManagerInputHealth: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "open_streamer_manager_input_health",
			Help: "Input health per stream and priority: 1 = delivering packets, 0 = degraded.",
		}, []string{"stream_code", "input_priority"}),

		TranscoderWorkersActive: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "open_streamer_transcoder_workers_active",
			Help: "Number of FFmpeg encoder processes currently running per stream.",
		}, []string{"stream_code"}),

		TranscoderRestartsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_transcoder_restarts_total",
			Help: "Total FFmpeg process crash-restarts per stream.",
		}, []string{"stream_code"}),

		TranscoderQualitiesActive: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "open_streamer_transcoder_qualities_active",
			Help: "Number of active ABR rendition profiles per stream.",
		}, []string{"stream_code"}),

		BufferPackets: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "open_streamer_buffer_packets",
			Help: "Current number of packets in the ring buffer per stream.",
		}, []string{"stream_code"}),

		PublisherClientsActive: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "open_streamer_publisher_clients_active",
			Help: "Number of active viewer connections per stream and protocol.",
		}, []string{"stream_code", "protocol"}),

		PublisherSegmentsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_publisher_segments_total",
			Help: "Total HLS/DASH segments packaged per stream and profile.",
		}, []string{"stream_code", "profile"}),

		DVRSegmentsWrittenTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_dvr_segments_written_total",
			Help: "Total TS segments successfully written to disk per stream.",
		}, []string{"stream_code"}),

		DVRBytesWrittenTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_dvr_bytes_written_total",
			Help: "Total bytes written to disk by the DVR per stream.",
		}, []string{"stream_code"}),

		StreamStartTimeSeconds: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "open_streamer_stream_start_time_seconds",
			Help: "Unix timestamp when the stream pipeline was last started. Absent when stopped. Uptime = time() - this value.",
		}, []string{"stream_code"}),

		HooksDeliveryFailedTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "open_streamer_hooks_delivery_failed_total",
			Help: "Total hook deliveries that exhausted all retries per hook.",
		}, []string{"hook_id", "hook_type"}),
	}

	return m, nil
}
