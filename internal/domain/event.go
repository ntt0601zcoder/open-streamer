// Package domain defines core types shared across Open Streamer modules.
package domain

import "time"

// EventType identifies the kind of domain event that occurred.
type EventType string

// EventType values are emitted on the event bus for stream lifecycle, inputs, recordings, and segments.
const (
	// Stream lifecycle — published by coordinator and API handler.
	EventStreamCreated EventType = "stream.created"
	EventStreamStarted EventType = "stream.started"
	EventStreamStopped EventType = "stream.stopped"
	EventStreamDeleted EventType = "stream.deleted"

	// Input health — published by ingestor worker and stream manager.
	EventInputConnected    EventType = "input.connected"    // source connected successfully
	EventInputReconnecting EventType = "input.reconnecting" // transient error, retrying
	EventInputDegraded     EventType = "input.degraded"     // error detected by manager
	EventInputFailed       EventType = "input.failed"       // worker exited / non-retriable
	EventInputFailover     EventType = "input.failover"     // switched to lower-priority input

	// DVR recordings — published by dvr.Service.
	EventRecordingStarted EventType = "recording.started"
	EventRecordingStopped EventType = "recording.stopped"
	EventRecordingFailed  EventType = "recording.failed"
	EventSegmentWritten   EventType = "segment.written"

	// Transcoder — published by transcoder.Service.
	EventTranscoderStarted EventType = "transcoder.started"
	EventTranscoderStopped EventType = "transcoder.stopped"
	EventTranscoderError   EventType = "transcoder.error"
)

// Event is an immutable fact describing a domain state change.
type Event struct {
	ID         string         `json:"id"` // UUID for idempotent delivery
	Type       EventType      `json:"type"`
	StreamCode StreamCode     `json:"stream_code"`
	OccurredAt time.Time      `json:"occurred_at"`
	Payload    map[string]any `json:"payload,omitempty"` // event-specific fields
}
