# Open Streamer — Domain Events Reference

All events flow through the in-process event bus (`internal/events`).
External consumers receive them via registered **Hooks** (HTTP webhook or Kafka).

## Event envelope

Every event shares the same JSON structure:

```json
{
  "id":          "test-1712345678901234567",
  "type":        "stream.created",
  "stream_code": "my-stream",
  "occurred_at": "2026-04-07T12:00:00Z",
  "payload":     { ... }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Unique event ID (nano-timestamp prefix). Used for idempotent delivery. |
| `type` | string | Event type — see table below. |
| `stream_code` | string | Stream this event belongs to. |
| `occurred_at` | RFC 3339 | Wall time the event was emitted. |
| `payload` | object | Event-specific fields. May be absent when there are no extra fields. |

---

## Event types

### Stream lifecycle

Published by: **API handler** (`internal/api/handler/stream.go`) and **Coordinator** (`internal/coordinator/coordinator.go`).

| Type | When |
|------|------|
| `stream.created` | A new stream is persisted for the first time (PUT /streams/{code} creates a new stream). |
| `stream.started` | The full ingest + publish pipeline has been started successfully (coordinator.Start completed). |
| `stream.stopped` | The pipeline has been torn down (coordinator.Stop completed). Fired on manual stop, server shutdown, or pipeline error. |
| `stream.deleted` | The stream has been removed from the store (DELETE /streams/{code}). |

`stream.created` and `stream.deleted` carry no extra payload fields.
`stream.started` and `stream.stopped` carry no extra payload fields.

---

### Input health

Published by: **Ingestor** (`internal/ingestor/service.go`, `internal/ingestor/worker.go`).

| Type | When | Payload fields |
|------|------|----------------|
| `input.connected` | A pull input opened successfully and the first read loop started. | `input_priority` (int), `url` (string) |
| `input.reconnecting` | A transient read error occurred; the worker is about to sleep before the next reconnect attempt. | `input_priority` (int), `error` (string) |
| `input.degraded` | The stream manager detected that the active input has not delivered a packet within the configured timeout. | — |
| `input.failed` | A pull worker exited (EOF, non-retriable error, or context cancel). | — |
| `input.failover` | The stream manager switched to a lower-priority input after the primary was marked failed. | — |

**Reconnect sequence example:**

```
input.connected       ← source OK
input.reconnecting    ← transient read error, sleeping 1 s
input.connected       ← reconnected
input.failed          ← context cancelled (stream stopped)
```

---

### DVR / Recording

Published by: **DVR service** (`internal/dvr/service.go`).

| Type | When | Payload fields |
|------|------|----------------|
| `recording.started` | `StartRecording` completed; first segment will follow. Fires on fresh start and on resume after restart. | `recording_id` (string) |
| `recording.stopped` | `StopRecording` completed; playlist written as VOD. | `recording_id` (string) |
| `recording.failed` | `os.WriteFile` for a segment failed (e.g. disk full, permission denied). Recording loop continues. | `recording_id` (string), `segment` (filename), `error` (string) |
| `segment.written` | A TS segment was successfully flushed to disk. High-frequency — one event per segment cut. | `recording_id` (string), `segment` (filename), `duration_sec` (float), `size_bytes` (int64), `wall_time` (RFC 3339), `discontinuity` (bool) |

`segment.written` fires at segment cadence (default 4 s). Hook subscribers that want to avoid high
volume should filter by event type when registering a hook (`event_types` field).

---

### Transcoder

Published by: **Transcoder service** (`internal/transcoder/service.go`, `internal/transcoder/worker_run.go`).

| Type | When | Payload fields |
|------|------|----------------|
| `transcoder.started` | FFmpeg worker pool has started for a stream (all profile goroutines launched). | `profiles` (int), `raw_ingest_id` (string) |
| `transcoder.stopped` | All FFmpeg processes for a stream have exited (normal stop or context cancel). | `profiles` (int) |
| `transcoder.error` | A single FFmpeg process exited with a non-zero exit code outside of a controlled shutdown. | `profile` (string, e.g. `track_1`), `error` (string) |

`transcoder.error` does **not** stop the stream — other profile encoders continue running.
Re-encoding the failed rendition requires a full stop + start via the API.

---

## Hook delivery

Events are delivered to all enabled hooks whose `event_types` filter matches the event type.
An empty `event_types` list means "receive all events".

### HTTP hooks

- Method: `POST`
- Body: JSON event envelope (see above)
- HMAC: `X-OpenStreamer-Signature: sha256=<hex>` when `secret` is set on the hook
- Retries: up to `max_retries` times with 1 s / 5 s / 30 s backoff
- Timeout: per-hook `timeout_sec` (default 10 s)

### Kafka hooks

- Topic: the hook's `target` field
- Message key: `stream_code`
- Message value: JSON event envelope
- Brokers: `hooks.kafka_brokers` in config (shared by all Kafka hooks)
- Writer: lazy-initialized per topic, reused across deliveries

---

## Filtering events on a hook

When registering a hook via the API, set `event_types` to limit delivery:

```json
{
  "type": "http",
  "target": "https://example.com/webhook",
  "event_types": ["stream.started", "stream.stopped", "input.failover"],
  "enabled": true
}
```

Leave `event_types` empty to receive every event type.

---

## Volume guide

| Event | Typical frequency |
|-------|-------------------|
| `stream.created` / `stream.deleted` | Rare — operator action |
| `stream.started` / `stream.stopped` | Rare — operator action or failover |
| `input.connected` / `input.failed` | Low — per reconnect cycle |
| `input.reconnecting` / `input.degraded` / `input.failover` | Low — only on signal problems |
| `recording.started` / `recording.stopped` / `recording.failed` | Low |
| `transcoder.started` / `transcoder.stopped` / `transcoder.error` | Low |
| `segment.written` | **High** — one per segment cut (default ~4 s) |

*Updated against codebase state 2026-04-07.*
