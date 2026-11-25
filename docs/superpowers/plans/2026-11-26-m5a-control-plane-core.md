# M5a: Control-Plane Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A real server binary, `openstreamer`, whose streams are declarative specs kept in a file store and edited over an authenticated REST API; each stream is run by its own supervisor, which applies an edit of the output set in place, so the other outputs of the stream keep their sessions; the server exports Prometheus metrics and writes redacted JSON logs.

**Architecture:**
- **`internal/spec`** is the stream document: a pure data package with structural validation, a redacted copy, an ETag and `Diff`, which classifies an edit as *none*, *outputs* or *rebuild*.
- **`internal/store`** keeps one JSON file per stream, written by temp file, fsync and rename.
- **`internal/stream`** has the `Supervisor` (one goroutine per stream; it owns the stream's hubs, pipeline, transcoder, packagers and pushers, and takes edits as messages) and the `Manager` (the set of supervisors, the cross-stream checks, the store writes, and the lookups the output servers use).
- **Output edits are in place.** The session outputs (HTTP-TS, RTMP, RTSP, SRT) are process-wide servers that find a stream through a lookup. A supervisor publishes an immutable `View` (which names are served by which output) through an atomic pointer; disabling an output swaps the view and tells the stream's hubs to refuse that output's subscribers (`hub.Refuse`), which ends exactly its sessions. Packagers and pushers are per stream and are started or stopped one by one. Nothing else is touched, so nothing else is interrupted.
- **`internal/api`**, **`internal/metrics`**, **`internal/logging`** and **`internal/server`** are the REST handler, a small Prometheus text writer, the `slog` set-up with a scrubbing handler, and the wiring that `cmd/openstreamer` runs.

**Tech Stack:** Go 1.26.11, standard library only for everything new: `net/http` (`ServeMux` method and wildcard patterns), `encoding/json`, `log/slog`, `crypto/sha256`, `crypto/subtle`, `net/http/pprof`, `runtime/metrics`. Existing: the v2 packages `hub`, `pipeline`, `ingest`, `transcode`, `output/*`, `rtmpsrv`, `redact`, `conform`, `testpeer`; testify; peers ffmpeg/ffprobe.

**Spec:** `docs/REBUILD_PLAN.md` (§2 rows "Control" and "Operations", §3.6 Control plane, §3.7 Libraries, §4 layout, §5 row M5, the "M5" lines of the Out-of tables, and the Backlog items naming M5). Format exemplars: `docs/superpowers/plans/2026-11-25-m4c-picture-features.md`, `docs/superpowers/plans/2026-11-25-m4b-abr-outputs.md`. `cmd/osdev/main.go` is the reference for how the v2 packages are wired and shut down; it stays as it is.

## Global Constraints

- **Libraries:** **no new Go module.** Only the approved independent libraries (mp4ff, pion/rtp, pion/sdp, datarhei/gosrt, testify, go-astiav v0.41.0, protobuf `protowire`); no bluenviron, no lal, no code copied from another product. v1's code on `main` (`internal/store`, `internal/api`, `internal/coordinator`, `internal/runtime`) may be read for ideas and is never copied. The router is `net/http`'s `ServeMux`, not chi (R-D7); metrics are a hand-written text exposition, not `prometheus/client_golang` (R-D9); the config and the store are JSON, so no YAML library (R-D2, R-D4). `go.mod` and `go.sum` do not change in this plan; a task that finds it needs a module stops and reports.
- **cgo:** only `internal/transcode/native` and `cmd/ostranscoder` use cgo, behind the tag `libav`. Without the tag, `go build ./...`, `go vet ./...`, `golangci-lint run ./...` and `make test` pass, as do `GOOS=windows go vet ./...` and `GOOS=darwin go vet ./...`. Every new package is pure Go; anything Unix-only (directory fsync, `/proc`) sits behind `_unix.go`/`_linux.go` and `_other.go` files.
- **Frozen behaviour:** every existing pipeline, output, transcoder and osdev behaviour is what M4c left, and every existing test passes unchanged except the three Task 1 names. The only edits to existing non-test code are additive: `hub.Refuse` and `rtmpsrv.Registry.Addr` (Task 5), `redact.Masked` (Task 2) and the watermark sweep's regex (Task 1). A hub on which `Refuse` was never called behaves exactly as before.
- **Time:** 90 kHz everywhere. The control plane never touches a timestamp.
- **Lint:** `exhaustive` with `default-signifies-exhaustive: false` (a `switch` on `spec.Change`, `stream.Output` or a state lists every value); `godot`; gofumpt.
- **Errors:** wrap as `fmt.Errorf("pkg: op: %w", err)`; never both log and return.
- **Goroutines:** every spawned goroutine has an explicit cancel path, and the function that spawns it waits for it before returning.
- **Security:** every `/api/`, `/metrics` and `/debug/pprof/` request needs the bearer token (R-D8). Request bodies are bounded (1 MiB) and decoded strictly. A stream name is the only caller-supplied path component of the store and must match `^[a-z0-9][a-z0-9-]{0,62}$`. No secret (URL password, secret query value, RTMP stream key, API token) reaches a log line, a metric, a status document or an error message: URLs pass through `redact.URL` before they leave the process, and the log handler scrubs what slips through (R-D10).
- **Peers:** every test that runs ffmpeg/ffprobe passes on the host (ffmpeg 8) and in `make test-ci-peers` (image `osv2-ci-peers:m4a`: ffmpeg 6.1, VLC 3.0.20, GStreamer 1.24) before any push. The transcoder bundle is built with `make ostranscoder`; NVENC device tests stay opt-in (`OS_NVENC=1`).
- **Soak safety (absolute):** never stop, restart or reconfigure open-streamer-soak-mediamtx, caddy, grafana, osdev-console-1, vector, alloy, pyroscope, prometheus, node-exporter, nvidia-gpu-exporter, or any other container not named `osv2-*`. Never bind ports 554, 1935 or 8554: every test listens on `127.0.0.1:0` and reads the port back (`server.Addrs`), and no test or step runs the server with the example config. Own containers are `osv2-*`, `--rm`, stopped afterwards.
- **Commits:** end every message with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. After each wave merge, run `go mod tidy` and check that it changes nothing.

## Rulings

| ID | Ruling | Cost if wrong |
|---|---|---|
| R-D1 | **M5 is split in three.** **M5a (this plan)** is the control-plane core and delivers the M5 row's exit criterion: spec, store, supervisor, manager, API, metrics (with `EncoderFallbacks`), logs, `cmd/openstreamer`, and the product names of a ladder. Done when a spec created over the API serves every output, an output edit interrupts no other output, and a restart restores every stream (Task 11's tests). **M5b, live edits and assets:** a running pipeline's inputs changed in place (`Pipeline.SetInputs`, §3.6), the watermark edited in place over the API (`Transcoder.SetWatermark` exists), the asset API (R-C8) with operator fonts (R-C7), a rung that refuses the x264 fallback (wire version 3), and audio language in the spec, the HLS masters and the MPD. Done when replacing a running stream's inputs is exactly one epoch change and ends no output session, an uploaded image and font appear on every rung, a strict NVENC rung fails instead of falling back and is counted, and `osconform` reads the languages. **M5c, transport security:** HTTPS for the API and the HTTP outputs, RTMPS (listen, pull, push), RTSPS with SRTP by its own ruling, SRT encryption (input and output), redaction of credentials in URL paths and queries (the M2c note), and secrets as write-only spec fields. Done when ffmpeg publishes and plays over each secured protocol against our server, and no secret appears in a response, log or metric. M6 and the M7 soak need only M5a; M5b and M5c may run beside M6 | Three plans and three reviews instead of one. Until M5b an input edit rebuilds the stream (R-D6), and assets are files the operator copies into a directory. Until M5c the API token and every stream key cross the network in clear, so the API listens on loopback by default (R-D8) |
| R-D2 | **The binary is `cmd/openstreamer`** (§4), configured by one JSON file, `-config path`, decoded strictly (an unknown key is an error). Keys and defaults are in Task 10. The file holds only what cannot be a stream spec: the data directory, the listen addresses and apps of the shared servers, the transcoder binary, the assets directory, logging. The API token may also come from `OPENSTREAMER_API_TOKEN`, which wins, so the file need not hold a secret. The config is read once: changing it needs a restart. `cmd/osdev` stays the development harness. Rejected: YAML (a new direct dependency for one small file) and flags for everything (the soak unit would carry twenty flags) | No comments in the config file; an operator used to v1's `config.yaml` rewrites it. A listen-address change costs a restart of every stream |
| R-D3 | **Product names (closes R-B2).** A stream name and a rendition label each match `^[a-z0-9][a-z0-9-]{0,62}$`, so `_` separates them without ambiguity and names are safe file names on case-insensitive file systems. A stream **without** a ladder is served as `<name>` on every output. A stream **with** a ladder is served as `<name>`: over HLS and DASH that is the ABR presentation, over HTTP-TS, RTMP, RTSP, SRT and RTMP push it is rendition 0, the first in the spec; and each rendition is also its own stream `<name>_<label>` on every enabled output. **The source of a laddered stream is not served to players** (with a watermark it is the unbranded picture, R-C5); `copy://<name>` reads it, and `copy://<name>_<label>` reads a rendition. There is no `<name>_abr` in the product and `abr` is an ordinary label; osdev keeps its names. The thumbnail is an operator preview of the source, so it is served by the API, behind the token, not under `/live/` | An operator who wants the raw source out next to a ladder adds a `copy` rendition (not possible with a watermark, by R-C5). A ladder of N rungs runs N + 1 packagers when HLS or DASH is on, as osdev does; a switch that stops serving the renditions on their own is a Backlog item |
| R-D4 | **The store is `<data_dir>/streams/<name>.json`**, one file per stream: `{"version":1,"stream":{…}}`. A write is: create `.<name>.json.<random>.tmp` in the same directory with mode 0600, write, `fsync`, rename over the target, `fsync` the directory (Unix). The directory is 0700: specs hold stream keys and passwords. `Load` reads every `*.json`, removes `*.tmp` leftovers, and **skips and reports** a file that does not decode, fails validation, has a `version` above 1, or whose name is not its spec's name; such a file is never changed or deleted, and a later `Put` of that name replaces it. Migrations: `version` is the only switch; a future version N adds a function that lifts N−1 documents in memory at load, and files are rewritten only by `Put`. Rejected: one file for all streams (an edit rewrites every spec, and one bad byte loses them all) and refusing to start on a bad file (one corrupt spec would keep every other stream offline) | A stream whose file is corrupt is silently absent until someone reads the log or the `openstreamer_store_load_errors` gauge. Two servers on one data directory are not prevented (they would also fight for the ports). No history: an overwritten spec is gone |
| R-D5 | **Supervisor (§3.6).** One goroutine per enabled stream owns everything of that stream and takes edits from a mailbox (`Submit`), in order; readers (the output servers' lookups, the API's status, the metrics) see only an immutable `View` and a `Status` behind atomic pointers. There is no lock shared between streams, and **no periodic reconciler**: a supervisor exists for exactly the enabled specs in the manager's map, by construction, and its loop ends only with its context. **Restart policy:** inside a generation, a failed input is the pipeline's business (retry and failover, M2a) and a dead child the transcoder's (respawn, M4a). The supervisor rebuilds the whole generation only when one of its parts returns an error it cannot recover from (the pipeline's `Run` or the transcoder's `Run` returning before the context ends, for example a child of another wire version): it tears the generation down, waits 1 s, doubling up to 30 s, back to 1 s once a generation has lived 60 s, and builds again, forever. An edit during the wait is applied at once. A stream never ends: `EndOnEOF` is false, so a file input without `loop` restarts at its end as a new epoch. A panic is not recovered | A generation that fails at once is rebuilt every 30 s for as long as the spec stands; `state: backoff`, `last_error` and `openstreamer_stream_restarts_total{reason="failure"}` are the signals. A bug that panics in one stream takes the process, and every stream, down; the service manager restarts it and the store brings every stream back |
| R-D6 | **How an edit is applied.** `spec.Diff(old, new)` is `ChangeNone` when the documents are equal, `ChangeOutputs` when they differ only in `outputs`, and `ChangeRebuild` otherwise. **Outputs, in place** (Task 7's `reattach`): packagers are started when the first of HLS and DASH is enabled and stopped when the last is disabled, and left alone otherwise; each of HTTP-TS, RTMP, RTSP and SRT is a flag in the view, and disabling one makes each served hub refuse that output's subscribers, which ends its sessions and no others; pushes are keyed by URL, so an added target starts, a removed one stops, and an unchanged one keeps its connection. The source hub, the pipeline, the transcoder and its child are not touched. **Rebuild:** the generation is torn down and built from the new spec at once (no backoff); every session of the stream ends and HLS media sequence numbers start again. **In M5a an input edit is a rebuild**; §3.6's "changing inputs … is applied in place" is M5b's. `disabled` stops and removes the supervisor, or starts one. Rejected: v1's field-by-field diff routing (the source of most of v1's lifecycle findings); here there are two routes | Any edit outside `outputs` (a rung's bitrate, a thumbnail interval, one more failover input) interrupts every viewer of that stream for a few seconds. Disabling HLS while DASH stays on stops serving the HLS paths but keeps the packager's work |
| R-D7 | **API.** Base path `/api/v1`, JSON. The resource is the whole stream document: `PUT /streams/{name}` creates or replaces it and is idempotent; there is no PATCH and no POST. `GET /streams`, `GET /streams/{name}`, `DELETE /streams/{name}`, `GET /streams/{name}/status`, `GET /streams/{name}/thumbnail.jpg`, `GET /version`; outside the base path `/healthz`, `/metrics` and `/debug/pprof/`. Every `GET` of a spec carries an `ETag`; `PUT` and `DELETE` honour `If-Match` (412 on a mismatch). A `PUT` answers with the stored document and `applied`: `created`, `unchanged`, `outputs`, `rebuild`, `started` or `stopped`. **URLs are redacted in every response** unless the request has `?secrets=1`, which returns the stored document as is, for an edit round trip; a `PUT` whose URL carries the redaction mask where a secret would be is refused, so a redacted document cannot be written back by mistake. The router is `net/http.ServeMux` (method and wildcard patterns; osdev, `live` and `httpts` use it already), not chi. The OpenAPI 3.1 document is hand-written **as JSON**, `api/openapi.json`, and a test keeps it equal to the registered routes and the spec's fields. Task 12 edits §3.6 and §3.7 to say so | Clients send the whole document for a one-field edit, and must use `If-Match` to avoid losing a concurrent edit. No middleware library: auth, the body limit and the request counter are three hand-written wrappers. JSON OpenAPI is less pleasant to hand-edit than YAML |
| R-D8 | **Auth.** One bearer token, at least 16 bytes, compared in constant time (`subtle.ConstantTimeCompare` over SHA-256 sums). The server refuses to start without one. It guards `/api/`, `/metrics` and `/debug/pprof/`; `/healthz` is open. The API has its own listener, default `127.0.0.1:8081`, separate from the media listener (`/live/`, default `:8080`), which has no auth (playback tokens are M6). The token holder is a full operator: it can point a stream at any URL or local media file the server can read. A failed attempt is logged at most once every 10 s, without the presented value. Rejected: no auth on loopback (a reverse proxy on the same host makes every request loopback) | A Prometheus scrape needs `authorization: credentials`. One shared secret, no roles, no rotation without a restart. Over plain HTTP until M5c, so a non-loopback API listener exposes the token to the network |
| R-D9 | **Metrics.** `internal/metrics` writes the Prometheus text format 0.0.4 itself: counters and gauges, collected at scrape time from the `Stats` the packages already keep. No histograms. Every name starts `openstreamer_`, except the conventional `go_*` and `process_*` (so stock dashboards and the soak's leak panels work). **Labels come only from configuration:** `stream` (a spec name), `name` (a served name), `rendition` (a label), `output`, `reason`, `state`, `kind`, `result`, `target` (a push's index, never its URL), and for the API `method`, `route` (the mux pattern, never the path) and `code`. Nothing a client sends becomes a label value. With S streams of at most 8 rungs and 8 pushes the stream series are bounded by about 60 × S. Counters that come from a generation's objects start again at 0 when the stream is rebuilt, which Prometheus treats as a counter reset. `EncoderFallbacks` is `openstreamer_transcoder_encoder_fallbacks_total{stream}` (closes the Backlog's export; refusing the fallback is M5b). The catalogue is in Task 7 and Task 10. Rejected: `prometheus/client_golang` (Apache-2.0): about ten transitive modules for a job that is two hundred lines when every value is already a snapshot | We own the exposition code and its escaping. No latency histograms; if they are wanted, switching to `client_golang` is a contained change behind `metrics.Writer`. Per-stream session counts are not exported (the output servers count per server, not per stream): M6's sessions |
| R-D10 | **Logs.** `log/slog`, JSON on stderr by default (`text` for a terminal), level `info` by default. Attributes: `stream`, `component`, `input`, `err`. The API logs one line per request at `info`: method, route pattern, status, duration, remote address; never a header, a query or a body. Media requests are not logged per request. **Redaction is two layers:** code that logs a URL passes it through `redact.URL`, and the handler from `logging.New` scrubs every message, string attribute and error text: each URL-shaped substring (`scheme://…`) goes through `redact.URL`, and an attribute named `token`, `password`, `passphrase`, `secret` or `authorization` (any case) is replaced by the mask | A regular expression runs over every logged string, which is fine at log rates and would not be on a per-frame path (nothing logs per frame). A secret that is not URL-shaped and not under one of those keys is not caught by the second layer |
| R-D11 | **Validation happens before anything is stored or started,** in two steps: `spec.Stream.Validate` (structure, names, limits; no I/O) and `stream.Check` (what needs the v2 packages or the server's config: `ingest.Parse` on every input, a dry `transcode.New` and `rtmppush.New` on scratch hubs, the asset read, the transcoder binary being configured). The manager then checks across streams: `copy://` cycles, and two streams claiming one listen address (an RTMP publish path on one address, a UDP or SRT listen port). After that the store is written, and only then is the supervisor told; a supervisor is never given a spec that failed a check, so building a generation cannot fail on the spec. An unreachable source is **not** a validation error: the stream is created and waits | A check the dry run misses surfaces as a stream in `backoff` rather than a 422. `copy://` of a stream that does not exist yet is accepted, and the copying stream waits for it |
| R-D12 | **Proof.** In-process end-to-end tests in `internal/e2e` start `server.New` on ephemeral ports and drive it only through HTTP, as a client would: `TestM5aEveryOutput`, `TestM5aEditOutputs`, `TestM5aRestartRestores`, `TestM5aLadder`, `TestM5aPeers`, `TestM5aNoSecretsLeak` (Task 11). "Not interrupted" is measured, not inferred: one long-lived reader per output runs across the edit and its capture must hold every frame a tap subscriber saw on the hub in that span, once, in one epoch; and the session counters (`PlayersStarted`, SRT connections, push `Connects`, the generation number, the transcoder's `Spawns`) must not move | The tests run a looped file input, so they prove the control plane and not ingest, which M2's and M3's matrices already cover. An interruption shorter than one frame is invisible to them |

## Execution waves (parallel per user instruction)

| Wave | Tasks | Why this order |
|---|---|---|
| 1 | 1 (carry items), 2 (`spec`, `redact.Masked`), 3 (`metrics`), 4 (`logging`), 5 (`hub.Refuse`, `rtmpsrv.Registry.Addr`) | Disjoint files: Task 1 has `native/watermark*`, `cmd/ostranscoder/main_test.go`, `rtspplay_test.go` and one Backlog line; Tasks 2, 3 and 4 are new packages (Task 2 also `internal/redact`); Task 5 has `internal/hub` and `internal/rtmpsrv` |
| 2 | 6 (`store`), 7 (`stream.Supervisor`) | 6 needs Task 2's types; 7 needs Tasks 2, 3 and 5. New, separate packages |
| 3 | 8 (`stream.Manager`) | Same package as 7; needs 6's `Store` |
| 4 | 9 (`api`) | Needs 8's `Manager` and `PutResult` |
| 5 | 10 (`server`, `cmd/openstreamer`) | Needs 4, 8 and 9 |
| 6 | 11 (M5a scenarios) | Needs 10 |
| 7 | 12 (CI, docs, spec, done) | Needs 11 |

Each parallel task runs in its own worktree; the controller merges.

## Review Focus

1. **Two streams claim one listen address** (the same RTMP publish path on one address, the same UDP or SRT listen port): the second `PUT` is refused with 409 and the first stream keeps running untouched; nothing is stored. Task 8, `TestManagerListenClaim`.
2. **A damaged store directory** (a truncated file after a crash, a leftover temp file, a file renamed by hand, a file from a newer version): the server starts with every good stream, reports each bad file once, and never rewrites or deletes it. Task 6, `TestLoadSkipsBadFiles`; Task 11, `TestM5aRestartRestores`.
3. **A redacted document written back, and secrets on their way out:** a `GET` without `?secrets=1` followed by a `PUT` of its body is refused and the stored secret survives; no password, stream key or token appears in a response, a status, a metric or a log line, including the error of an input that fails to connect. Task 9, `TestPutRejectsMaskedSecrets`; Task 11, `TestM5aNoSecretsLeak`.
4. **Edits that race:** twenty concurrent `PUT`s of one stream mixing output edits and rebuilds, with a `DELETE` among them: no deadlock, the runtime ends equal to the stored spec (or gone), and no goroutine or subscriber is left. Task 8, `TestManagerConcurrentPuts`.
5. **A stream that cannot start** (a source that is down, a wrong password, a transcoder binary that is missing or speaks another wire version): the `PUT` succeeds, the stream reports `starting` or `backoff` with a redacted `last_error`, other streams and the API are unaffected, it runs when the source appears, and deleting it during a backoff returns at once. Task 7, `TestSupervisorDeadInput` and `TestSupervisorStopDuringBackoff`.

---

### Task 1: Carry items from M4c's final review

A hygiene task: three items the controller carried out of M4c. Items 1 and 2 touch the cgo packages and are verified with `make test-transcode`.

**Files:**
- Modify: `internal/transcode/native/watermark.go` (`wmDirName`), `internal/transcode/native/watermark_test.go` (`TestWatermarkSweepsStaleDirs`), `cmd/ostranscoder/main_test.go` (`TestProbeFlag`, `TestChildEncoderFallbacks`), `internal/output/rtspplay/rtspplay_test.go` (`TestPlayManyPlayers`), `docs/REBUILD_PLAN.md` (the Backlog item "`make test-nvenc`: shared mode and the refusal guard")

**Interfaces:** none produced.

- [ ] **Step 1: The sweep's PID.** `syscall.Kill` takes a 32-bit pid, so `ostx-wm-2147483648-1` reaches the kernel as a negative number (a process group), which answers `ESRCH`, and the sweep removes a directory it has no reason to think is dead. In `TestWatermarkSweepsStaleDirs`, add to the kept directories `mkdir("ostx-wm-2147483648-1")`, `mkdir("ostx-wm-4294967297-1")` (it wraps to pid 1) and `mkdir("ostx-wm-99999999-1")` (8 digits). Run `make test-transcode ARGS='-run ^TestWatermarkSweepsStaleDirs$ ./internal/transcode/native/'`: it fails on the first two.
- [ ] **Step 2:** change the capture group of `wmDirName` to `([1-9][0-9]{0,6})` (7 digits: Linux's `pid_max` is at most 4194304) and say so in its comment. Run the same command: pass.
- [ ] **Step 3: The ostranscoder probes.** Add to `cmd/ostranscoder/main_test.go`:

```go
// nvencGate reports whether a test that opens an NVENC session may run:
// always without a device, and with one only when opted in.
func nvencGate(device, optedIn bool) bool { return !device || optedIn }

func TestNVENCGate(t *testing.T) // table: (false,false)→true, (false,true)→true, (true,false)→false, (true,true)→true
```

and `nvencProbeOrSkip(t)`: `device` is `os.Stat("/dev/nvidiactl") == nil`, `optedIn` is `OS_NVENC` or `OS_REQUIRE_NVENC` equal to `"1"`; when the gate is false it skips with the text `NVENC tests are opt-in: set OS_NVENC=1 (make test-nvenc does)` (the package keeps its own copy, as `native` does: importing `internal/testpeer` would pull the package into the untagged peer run). In `TestProbeFlag`, move the `-probe nvenc` block into a subtest `device` that calls `nvencProbeOrSkip` first; the three usage rows stay outside it and always run. `TestChildEncoderFallbacks` calls it first. CI has no device, so both still run there in full.
- [ ] **Step 4:** run `make test-transcode ARGS='-run ^(TestNVENCGate|TestProbeFlag|TestChildEncoderFallbacks)$ ./cmd/ostranscoder/'` (pass, nothing skipped: the container has no device). On this host, `go test -count=1 ./cmd/ostranscoder/` still passes untagged (the stub tests).
- [ ] **Step 5: Backlog line.** In that Backlog item, replace "the NVENC device tests (e2e `testpeer.NVENC`, native `nvencOrSkip`) skip" with "the NVENC device tests (e2e `testpeer.NVENC`, native `nvencOrSkip`, and `cmd/ostranscoder`'s `TestProbeFlag/device` and `TestChildEncoderFallbacks`, which skip only where `/dev/nvidiactl` exists) skip".
- [ ] **Step 6: `TestPlayManyPlayers`, the cause first.** Reproduce under load: run `go test -race -count=30 -run '^TestPlayManyPlayers$' ./internal/output/rtspplay/` while `go test -race -count=1 ./internal/e2e/ ./internal/transcode/` runs beside it. The expected cause is the late-subscription class of `TestPlayCodecChangeEndsSession`: a session subscribes after its PLAY response, so a player whose subscription lands after the feed has written the next keyframe starts from a later cached GOP (the failure seen was player 49 at ts 453600 for 543600, exactly 1 s, one GOP). Confirm it by logging, in the failing run, `len(e.h.Stats())` just before `feed`: fewer than 50 confirms it. **If all 50 are subscribed and a player still differs, stop and report: that is a defect, not a test race.**
- [ ] **Step 7:** after the `play` loop and before `feed`, add `require.Eventually(t, func() bool { return len(e.h.Stats()) == n }, waitFor, time.Millisecond, "every player subscribed")`. Every assertion stays: 150 video access units, equal normalised packets for all 50 players, 100 SSRCs, `PlayersStarted == 50`, `Resyncs == 0`, `EndedHubClosed == 50`.
- [ ] **Step 8:** run Step 6's two commands again: 30 of 30 pass under load. Then `go test -race -count=1 ./internal/output/rtspplay/`.
- [ ] **Step 9: Commit** `fix(transcode): cap the swept watermark dir's PID at 7 digits`, `test(ostranscoder): gate the NVENC probes on the opt-in where a device exists` and `test(rtspplay): wait for every player's subscription before the feed`

### Task 2: `internal/spec` — the stream document

**Files:**
- Create: `internal/spec/doc.go`, `spec.go`, `validate.go`, `diff.go`
- Modify: `internal/redact/redact.go` (`Masked`)
- Test: `internal/spec/spec_test.go`, `validate_test.go`, `diff_test.go`; `internal/redact/redact_test.go`

Pure data: imports only the standard library and `internal/redact`. No goroutines.

**Interfaces — Produces:**

```go
package spec

const (
	MaxInputs, MaxRenditions, MaxPushes = 8, 8, 8
	MaxURLBytes   = 2048
	MaxSpecBytes  = 1 << 20
)

type Stream struct {
	Name      string     `json:"name"`
	Disabled  bool       `json:"disabled,omitempty"`
	Inputs    []Input    `json:"inputs"`              // by priority, first highest
	Layout    []string   `json:"layout,omitempty"`    // "video", "audio"; empty means both
	Transcode *Transcode `json:"transcode,omitempty"`
	Thumbnail *Thumbnail `json:"thumbnail,omitempty"`
	Outputs   Outputs    `json:"outputs"`
}
type Input struct{ URL string `json:"url"` }
type Transcode struct {
	Renditions []Rendition `json:"renditions"`
	Audio      Audio       `json:"audio"`
	Watermark  *Watermark  `json:"watermark,omitempty"`
}
type Rendition struct {
	Label          string `json:"label"`
	Copy           bool   `json:"copy,omitempty"`
	Height         int    `json:"height,omitempty"`
	BitrateKbps    int    `json:"bitrate_kbps,omitempty"`
	MaxBitrateKbps int    `json:"max_bitrate_kbps,omitempty"`
	BFrames        int    `json:"b_frames,omitempty"`
	Preset         string `json:"preset,omitempty"`
	Encoder        string `json:"encoder,omitempty"` // "", "x264", "nvenc"
}
type Audio struct {
	Copy        bool `json:"copy,omitempty"`
	BitrateKbps int  `json:"bitrate_kbps,omitempty"`
	SampleRate  int  `json:"sample_rate,omitempty"`
	Channels    int  `json:"channels,omitempty"`
}
type Watermark struct {
	Text       string `json:"text,omitempty"`
	Image      string `json:"image,omitempty"`    // an asset name (R-C8)
	Position   string `json:"position,omitempty"` // "tl" (default), "tr", "bl", "br", "c"
	OpacityPct int    `json:"opacity_pct,omitempty"`
	SizePct    int    `json:"size_pct,omitempty"`
	TimeZone   string `json:"time_zone,omitempty"`
}
type Thumbnail struct {
	IntervalSec int `json:"interval_sec,omitempty"`
	Height      int `json:"height,omitempty"`
	Quality     int `json:"quality,omitempty"`
}
type Outputs struct {
	HLS    bool   `json:"hls,omitempty"`    // hls/* and cmaf/*.m3u8
	DASH   bool   `json:"dash,omitempty"`   // cmaf/*.mpd
	HTTPTS bool   `json:"httpts,omitempty"`
	RTMP   bool   `json:"rtmp,omitempty"`
	RTSP   bool   `json:"rtsp,omitempty"`
	SRT    bool   `json:"srt,omitempty"`
	Push   []Push `json:"push,omitempty"`
}
type Push struct{ URL string `json:"url"` }

func ValidName(s string) bool                 // ^[a-z0-9][a-z0-9-]{0,62}$
func Decode(r io.Reader) (Stream, error)      // one JSON document of at most MaxSpecBytes; unknown fields and trailing data are errors
func (s Stream) Validate() error
func (s Stream) Redacted() Stream             // a deep copy with every input and push URL through redact.URL
func (s Stream) ETag() string                 // `"` + the first 16 hex digits of sha256(json.Marshal(s)) + `"`
func (s Stream) Served() []string             // the names it is served under (R-D3): [Name], or [Name, Name_label0, Name_label1, …]

type Change uint8
const ( ChangeNone Change = iota; ChangeOutputs; ChangeRebuild )
func Diff(old, new Stream) Change             // R-D6; nil and empty slices are equal

package redact
func Masked(rawURL string) bool // the mask stands where URL writes it: the password, a lone user, a secret query value, or an rtmp stream key ending in the mask
```

`Validate` rules (each failure names the field, as `inputs[1].url: …`): the name is valid; 1 to 8 inputs, each URL non-empty, at most 2048 bytes, not `redact.Masked`; `layout` holds only `video` and `audio`, each at most once; with `transcode`, 1 to 8 renditions, labels valid and distinct, a `copy` rendition sets nothing else but its label, an encoded one has an even `height` in 2…4320 and `bitrate_kbps` > 0 and an `encoder` of `""`, `x264` or `nvenc`, a watermark has exactly one of `text` and `image`, an `image` matching `^[a-z0-9][a-z0-9_-]{0,63}\.(png|jpg|jpeg)$`, and a `position` of `""`, `tl`, `tr`, `bl`, `br` or `c`; at most 8 pushes, URLs non-empty, at most 2048 bytes, distinct, not `Masked`. Ranges the transcoder owns (presets, B-frames, sample rates, opacity, the thumbnail's fields) are left to `stream.Check`.

- [ ] **Step 1: Write the failing tests**

```go
func TestValidName(t *testing.T)
// ok: "cam", "a", "cam-1", 63 × "a". Not ok: "", "Cam", "cam_1", "-cam", "cam.1", "..", "a/b", "a%2f", "cam ", 64 × "a".
func TestDecodeStrict(t *testing.T)
// an unknown key, a second document after the first, and a body of MaxSpecBytes+1 are errors; the minimal
// document {"name":"cam","inputs":[{"url":"udp://:5000"}],"outputs":{"hls":true}} decodes.
func TestValidate(t *testing.T)
// a table with one row per rule above; each error contains the field path. A full valid document (3 rungs,
// one copy, text watermark, thumbnail, every output, 2 pushes) passes.
func TestValidateRejectsMaskedURL(t *testing.T)
// inputs[0].url "rtsp://user:xxxxx@host/a" and push "rtmp://h/app/abxxxxx" are refused; "rtsp://user:pw@host/a" passes.
func TestRedacted(t *testing.T)
// "rtsp://u:hunter2@h/a" and push "rtmp://h/live/secretkey" come back without "hunter2" and "secretkey"; the receiver is unchanged.
func TestETag(t *testing.T) // equal documents, one with a nil and one with an empty Push slice, have equal ETags; any field change changes it
func TestServed(t *testing.T) // no ladder: ["cam"]; labels 720, 360: ["cam", "cam_720", "cam_360"]
func TestDiff(t *testing.T)
// equal → None; only outputs.rtsp, only one push added, only pushes reordered → Outputs; an input added, a rung's
// bitrate, layout, thumbnail, watermark text, disabled, and outputs together with an input → Rebuild.
func TestMasked(t *testing.T) // in redact: for each form URL masks, Masked(URL(x)) is true and Masked(x) is false; a URL without secrets is false both ways
```

- [ ] **Step 2: Run** `go test -race -count=1 ./internal/spec/ ./internal/redact/` — compile failure.
- [ ] **Step 3: Implement.** `Diff` compares copies with `Outputs` zeroed, then `Outputs`, after normalising empty slices to nil. Pushes are a set for `Diff` only in the sense that a reorder is still `ChangeOutputs` (the supervisor keys them by URL).
- [ ] **Step 4: Run** the same command — pass.
- [ ] **Step 5: Commit** `feat(spec): the stream document, its validation and its diff`

### Task 3: `internal/metrics` — Prometheus text exposition

**Files:**
- Create: `internal/metrics/doc.go`, `metrics.go`, `process.go`, `process_linux.go`, `process_other.go`, `internal/metrics/metricstest/parse.go`
- Test: `internal/metrics/metrics_test.go`, `process_test.go`

**Interfaces — Produces:**

```go
package metrics

type Label struct{ Name, Value string }
func L(name, value string) Label

// Writer collects the samples of one scrape.
type Writer struct{ /* unexported */ }
func (w *Writer) Counter(name, help string, v float64, labels ...Label)
func (w *Writer) Gauge(name, help string, v float64, labels ...Label)

type Registry struct{ /* unexported; the zero value is ready */ }
func (r *Registry) Register(collect func(*Writer)) // collectors run in registration order at every scrape
func (r *Registry) Handler() http.Handler          // GET; Content-Type "text/plain; version=0.0.4; charset=utf-8"

// Process returns the collector of the go_* and process_* families.
func Process(start time.Time) func(*Writer)

package metricstest
type Sample struct { Name string; Labels map[string]string; Value float64; Type, Help string }
func Parse(r io.Reader) ([]Sample, error) // strict: every sample's family has HELP and TYPE before it, families are contiguous, names and label names are legal, no duplicate series
func Find(s []Sample, name string, labels ...string) (float64, bool) // labels as name, value pairs
```

Format rules the Writer owns: samples are grouped by family in first-seen order, with `# HELP` and `# TYPE` once per family; labels are written in the order given; a label value escapes `\`, `"` and newline, a help text `\` and newline; a counter's name ends in `_total`; a value is written with `strconv.FormatFloat(v, 'g', -1, 64)`. A family used with two types or two help texts, an illegal metric or label name, or the same series written twice makes the scrape fail with status 500 and a body naming the family, and nothing else: a broken exposition must not look like a valid one.

`Process` writes `go_goroutines`, `go_memstats_heap_alloc_bytes`, `go_memstats_heap_inuse_bytes`, `go_memstats_sys_bytes`, `go_gc_cycles_total` (from `runtime/metrics` and `runtime.NumGoroutine`), `process_start_time_seconds`, and on Linux `process_open_fds` (entries of `/proc/self/fd`), `process_resident_memory_bytes` and `process_cpu_seconds_total` (from `/proc/self/stat`, with `os.Getpagesize()` and 100 ticks per second). On other systems the three are absent.

- [ ] **Step 1: Write the failing tests**

```go
func TestWriterGolden(t *testing.T)
// two counters of one family written apart, a gauge between them, a label value `a"b\c` + newline: the body is
// byte-equal to a golden string with the family contiguous and the value escaped as a\"b\\c\n.
func TestWriterRejects(t *testing.T)
// a family as counter then gauge; two help texts; a counter not ending _total; a name "bad-name"; a label "0x"; the
// same series twice: each gives 500 and a body with the family name.
func TestHandlerParses(t *testing.T) // a registry with three collectors: metricstest.Parse accepts the body; a HEAD or POST is 405
func TestParseStrict(t *testing.T)   // metricstest.Parse refuses a sample without TYPE, a split family, a duplicate series
func TestProcess(t *testing.T)
// go_goroutines ≥ 1; process_start_time_seconds equals start; on Linux process_open_fds ≥ 3 and rises by 1 after os.Open.
```

- [ ] **Step 2: Run** `go test -race -count=1 ./internal/metrics/...` — compile failure.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the same command, then `GOOS=windows go vet ./internal/metrics/...` and `GOOS=darwin go vet ./internal/metrics/...` — pass.
- [ ] **Step 5: Commit** `feat(metrics): a Prometheus text writer and the process collector`

### Task 4: `internal/logging` — slog with a scrubbing handler

**Files:**
- Create: `internal/logging/doc.go`, `logging.go`
- Test: `internal/logging/logging_test.go`

**Interfaces — Produces:**

```go
package logging

// New returns a logger writing to w. level is "debug", "info", "warn" or "error" ("" means info);
// format is "json" or "text" ("" means json). Anything else is an error.
func New(w io.Writer, level, format string) (*slog.Logger, error)

// Scrub returns s with every URL-shaped substring passed through redact.URL.
func Scrub(s string) string
```

The handler wraps the JSON or text handler and rewrites every record before it: the message through `Scrub`; a string attribute through `Scrub`; an `error` attribute to the `Scrub` of its `Error()`; groups recursively; `slog.LogValuer`s resolved first; an attribute whose key, lower-cased, is `token`, `password`, `passphrase`, `secret` or `authorization` becomes `xxxxx`. `WithAttrs` scrubs the attributes it is given; `WithGroup` and `Enabled` delegate. A URL-shaped substring matches `[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>\\]+`.

- [ ] **Step 1: Write the failing tests**

```go
func TestNewLevelsAndFormats(t *testing.T) // "" → info JSON: Debug is dropped, Info is one JSON object per line with time, level, msg; "text" is not JSON; "loud" and "xml" are errors
func TestScrub(t *testing.T)
// `open rtsp://u:hunter2@cam/1 failed, then srt://h:9?passphrase=topsecret&x=1` loses "hunter2" and "topsecret" and keeps
// "cam/1" and "x=1"; `rtmp://h/live/secretkey` loses "secretkey"; a string without "://" is returned as is.
func TestHandlerScrubs(t *testing.T)
// log.With("url", u1).Error(msgWith(u2), "err", fmt.Errorf("dial %s: refused", u3), slog.Group("g", "url", u4), "Token", "abc123", "password", "pw"):
// the output line contains none of the four passwords, nor "abc123", nor `"pw"`; it contains the hosts.
func TestHandlerKeepsStructure(t *testing.T) // ints, bools, durations and times pass through unchanged; a group keeps its name
```

- [ ] **Step 2: Run** `go test -race -count=1 ./internal/logging/` — compile failure.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the same command — pass.
- [ ] **Step 5: Commit** `feat(logging): slog set-up with a handler that scrubs URLs and secrets`

### Task 5: `hub.Refuse` and `rtmpsrv.Registry.Addr`

Two additive methods the supervisor and the server need. Nothing changes for a caller that does not use them.

**Files:**
- Modify: `internal/hub/hub.go`, `internal/rtmpsrv/registry.go`
- Test: `internal/hub/hub_test.go`, `internal/rtmpsrv/registry_test.go` (the package's existing test file for the registry)

**Interfaces — Produces:**

```go
// Refuse replaces the set of refused subscriber-name prefixes. Every open
// subscriber whose name starts with one of them is closed, as by Sub.Close,
// and a later Subscribe, SubscribeLive or SubscribeCached under such a name
// returns a closed Sub, as on a closed hub. Refuse() refuses nothing. It
// returns the number of subscribers it closed.
func (h *Hub) Refuse(prefixes ...string) int

// Addr returns the address of the listener that serves addr (as given to
// RegisterPlay or RegisterPublish), or nil when nothing listens there. With
// port 0 it is how a caller learns the port.
func (r *Registry) Addr(addr string) net.Addr
```

The subscriber names in use, which Task 7 relies on: `"httpts "+remote`, `"rtmp "+name`, `"rtsp "+name`, `"srt "+remote`, `"rtmppush "+target`, `"copy:"+url`, `"transcode"`, `"thumbnailer"`, and a packager's (the caller's choice).

- [ ] **Step 1: Write the failing tests**

```go
func TestRefuseClosesMatching(t *testing.T)
// subs "rtsp a", "rtsp b", "rtmp a", "rtmppush x": Refuse("rtsp ") returns 2; the two channels are closed; the others
// still get the next frame; Stats() lists 2 subscribers.
func TestRefuseBlocksLaterSubscribes(t *testing.T)
// after Refuse("srt "): Subscribe, SubscribeLive and SubscribeCached under "srt 1.2.3.4:5" each return a Sub whose
// channel is already closed and which is not in Stats(); "rtmp a" subscribes normally.
func TestRefuseReplaces(t *testing.T) // Refuse("rtsp ") then Refuse(): "rtsp a" subscribes again; then Refuse("rtmp ", "srt ") refuses both
func TestRefuseNeverCalled(t *testing.T) // a control: a hub without Refuse delivers the cached GOP to a late subscriber exactly as before
func TestRefuseRace(t *testing.T) // under -race: writes, subscribes and Refuse from three goroutines for 200 ms; no panic, no send on a closed channel
func TestRegistryAddr(t *testing.T) // RegisterPlay("127.0.0.1:0", …): Addr("127.0.0.1:0") has a port above 0 that a TCP dial reaches; after unregister it is nil; an unknown addr is nil
```

- [ ] **Step 2: Run** `go test -race -count=1 -run 'TestRefuse|TestRegistryAddr' ./internal/hub/ ./internal/rtmpsrv/` — compile failure.
- [ ] **Step 3: Implement.** `Refuse` takes `h.mu`, stores the prefixes, and closes matching subscribers with the same steps as `Sub.Close`; `subscribe` returns a closed `Sub` for a refused name before it replays the cache.
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/hub/ ./internal/rtmpsrv/` — pass, every existing test unchanged.
- [ ] **Step 5: Commit** `feat(hub): refuse subscribers by name prefix` and `feat(rtmpsrv): report a listener's address`

### Task 6: `internal/store` — the file store

**Files:**
- Create: `internal/store/doc.go`, `store.go`, `sync_unix.go`, `sync_other.go`
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: `spec.Stream`, `spec.Decode`, `spec.ValidName`, `(spec.Stream).Validate`.
- Produces:

```go
package store

const Version = 1

type FS struct{ /* unexported */ }

// Open creates dir/streams (0700) when missing and returns the store.
func Open(dir string) (*FS, error)

// Load returns every stream it could read, sorted by name, and one error per file it skipped (R-D4).
func (s *FS) Load() ([]spec.Stream, []error)
func (s *FS) Put(sp spec.Stream) error  // sp.Name must be valid; atomic (R-D4)
func (s *FS) Delete(name string) error  // a missing file is not an error; fsyncs the directory
```

`Put` and `Delete` refuse a name that fails `spec.ValidName` before they build a path (`store: put: invalid name`), whatever the caller validated. `syncDir(path string) error` opens the directory and fsyncs it on Unix and does nothing elsewhere. Calls are serialised by a mutex in `FS`.

- [ ] **Step 1: Write the failing tests** (each in `t.TempDir()`)

```go
func TestPutLoadRoundTrip(t *testing.T) // three streams put, loaded sorted and equal; the files are 0600, the directory 0700, each file is {"version":1,"stream":{…}}
func TestPutReplacesAtomically(t *testing.T)
// Put twice under one name: one file, the second content, no *.tmp left. With the directory made read-only (skip as root),
// Put fails and the first content is intact.
func TestPutRejectsUnsafeNames(t *testing.T)
// "../x", "a/b", "", "A", "x.json", "..": Put and Delete return an error and nothing exists outside dir/streams (walk the temp root).
func TestLoadSkipsBadFiles(t *testing.T)
// next to a good cam.json: a truncated bad.json, future.json with "version":2, renamed.json holding the spec "other",
// invalid.json whose spec fails Validate, a directory x.json, notes.txt, and .cam.json.123.tmp. Load returns [cam] and four
// errors, each naming its file; the tmp file is gone; the four bad files, the directory and notes.txt are byte-for-byte untouched.
func TestDelete(t *testing.T) // Delete removes the file; a second Delete is nil; Load is then empty
func TestConcurrentPuts(t *testing.T) // 8 goroutines × 50 Puts over 4 names under -race: Load returns 4 valid streams, no tmp files
```

- [ ] **Step 2: Run** `go test -race -count=1 ./internal/store/` — compile failure.
- [ ] **Step 3: Implement.** The temp name is `"." + name + ".json." + <8 random hex> + ".tmp"`, created with `os.OpenFile(…, O_WRONLY|O_CREATE|O_EXCL, 0o600)`; on any failure after creation the temp file is removed.
- [ ] **Step 4: Run** the same command and `GOOS=windows go vet ./internal/store/` — pass.
- [ ] **Step 5: Commit** `feat(store): one JSON file per stream, written by rename`

### Task 7: `internal/stream` — the per-stream supervisor

**Files:**
- Create: `internal/stream/doc.go`, `env.go` (`Env`, `Output`, `Check`, the spec-to-options functions), `supervisor.go` (the loop, `Submit`), `generation.go` (build, `reattach`, teardown), `status.go` (`View`, `Status`, `Collect`)
- Test: `internal/stream/check_test.go`, `supervisor_test.go`, `reattach_test.go`, `helpers_test.go`

**Interfaces:**
- Consumes: Task 2 (`spec`), Task 3 (`metrics.Writer`, `metrics.L`), Task 5 (`hub.Refuse`); `pipeline.New`, `transcode.New`, `transcode.NewThumbnailer`, `live.NewPackager`, `rtmppush.New`, `picture.LoadAsset`, `ingest.Parse`, `redact.URL`; in tests `transcodetest`'s fake child.
- Produces:

```go
package stream

type Output string
const (
	OutHLS Output = "hls"; OutDASH Output = "dash"; OutHTTPTS Output = "httpts"
	OutRTMP Output = "rtmp"; OutRTSP Output = "rtsp"; OutSRT Output = "srt"
)

// Env is what a supervisor needs from the server.
type Env struct {
	Lookup     func(name string) (*hub.Hub, bool) // copy:// inputs; the Manager sets it
	Transcoder string                             // path of ostranscoder; "" means none
	AssetsDir  string                             // "" means no assets
	// The shared servers' addresses and RTMP play app, for Check's collision rules; "" means off.
	RTMPPlay, RTMPApp, SRTPlay string
	Logger *slog.Logger                           // nil discards
	Now    func() time.Time                       // nil means time.Now
	After  func(time.Duration) <-chan time.Time   // nil means time.After; the restart backoff
	Open   func(ctx context.Context, url string) (ingest.Reader, error) // tests; nil means ingest.Open
	TranscoderArgs, TranscoderEnv []string        // tests: the fake child
}

// Check reports why sp cannot run under env (R-D11). sp has passed Validate.
func Check(sp spec.Stream, env Env) error

type Supervisor struct{ /* unexported */ }
func NewSupervisor(sp spec.Stream, env Env) *Supervisor // sp has passed Check and is not disabled
func (s *Supervisor) Run(ctx context.Context)           // returns when ctx ends, with everything stopped and waited for
// Submit queues an edit and never blocks. The channel gets the Change once it is applied (for a rebuild: once the
// old generation is torn down and the new one's View is published), and is closed without a value if Run ends first.
func (s *Supervisor) Submit(sp spec.Stream) <-chan spec.Change
func (s *Supervisor) View() *View     // never nil; empty between generations
func (s *Supervisor) Status() Status
func (s *Supervisor) Shot() (picture.Shot, time.Time, bool)
func (s *Supervisor) Collect(w *metrics.Writer)

// View is what one generation serves. It is immutable; the supervisor swaps it.
type View struct {
	Gen     uint64                    // the generation, from 1; 0 between generations
	Outputs spec.Outputs
	Single  map[string]*hub.Hub       // served name → the hub of HTTP-TS, RTMP, RTSP, SRT and push
	Packs   map[string]*live.Packager // served name → its packager; empty while neither HLS nor DASH is on
	Source  *hub.Hub                  // the source hub: copy://<name>
}

type Status struct {
	Name        string       `json:"name"`
	State       string       `json:"state"` // "stopped", "starting", "running", "backoff"
	Generation  uint64       `json:"generation"`
	Since       time.Time    `json:"since"`          // when the generation started
	Restarts    uint64       `json:"restarts"`       // rebuilds after a failure
	Rebuilds    uint64       `json:"rebuilds"`       // rebuilds for an edit
	LastError   string       `json:"last_error,omitempty"`
	Served      []string     `json:"served"`
	Outputs     spec.Outputs `json:"outputs"`        // push URLs redacted
	Inputs      []string     `json:"inputs"`         // redacted
	ActiveInput int          `json:"active_input"`   // -1 when none
	Switches    uint64       `json:"input_switches"`
	Tracks      []Track      `json:"tracks"`         // the source hub's live tracks
	Transcoder  *Transcoder  `json:"transcoder,omitempty"`
	Pushes      []Push       `json:"pushes,omitempty"`
}
type Track struct { Track int `json:"track"`; Kind string `json:"kind"`; Codec string `json:"codec"` }
type Transcoder struct {
	PID int `json:"pid"`; Spawns uint64 `json:"spawns"`; Exits uint64 `json:"exits"`
	EncoderFallbacks uint64 `json:"encoder_fallbacks"`; Thumbnails uint64 `json:"thumbnails"`
}
type Push struct {
	Target string `json:"target"` // redacted
	State  string `json:"state"`; Connects uint64 `json:"connects"`; Failures uint64 `json:"failures"`
	LastError string `json:"last_error,omitempty"`
}
```

**`Check`** (each error is prefixed `stream: check:`): every input passes `ingest.Parse` (or `env.Open` is set); `layout` maps to `[]media.Kind` (empty: video, audio); with `transcode`, the options are built as osdev's `parseLadders` does (rendition `Name` = `<name>_<label>`, `Presentation: true`, `Binary: env.Transcoder`), an image watermark is read with `picture.LoadAsset(env.AssetsDir, image)` (an empty `AssetsDir` is an error), anything that encodes needs `env.Transcoder != ""`, and `transcode.New` on a scratch `hub.New(hub.Options{})` must succeed; a thumbnail needs `env.Transcoder != ""` and `picture.Thumbnail.Validate`; every push passes `rtmppush.New` on a scratch hub; an SRT listen input must not overlap `env.SRTPlay` (same port, and the same host or either a wildcard: osdev's `srtAddrsCollide` rule, rewritten here); an RTMP listen input on `env.RTMPPlay`'s address must not publish at `<RTMPApp>/<served name>` for any served name of the stream (a publish path is never playable). `Check` and the generation's build share the functions that turn a spec into options, so they cannot disagree.

**A generation** (unexported `generation`): the source hub; the `pipeline.Pipeline` (`Inputs` from the spec, `Env: ingest.Env{Lookup: env.Lookup}`, `Layout`, `EndOnEOF: false`, `OnEvent` recording the last failure, redacted); with a ladder, the `transcode.Transcoder` and its rendition and presentation hubs; a `transcode.Thumbnailer` when the spec has a thumbnail and no child encodes (as osdev's `parseThumbnailers`); and the attached outputs. The served hubs follow R-D3: without a ladder `Single[name]` is the source hub and its packager is non-ABR; with one, `Single[name]` and `Single[name_label0]` are rendition 0's hub, `Single[name_labelI]` rendition I's, `Packs[name]` is an ABR packager (`live.Options{ABR: true, Logger: …}`) on the presentation hub with a queue of 1024 × (renditions + 1), and `Packs[name_label]` a non-ABR packager on each rendition hub with a queue of 1024. Packagers subscribe under `"packager "+servedName`.

**The loop** (this is the algorithm the tests pin):

```go
func (s *Supervisor) Run(ctx context.Context) {
	wait := time.Second
	for ctx.Err() == nil {
		g := s.build(ctx)                      // hubs, pipeline, transcoder, thumbnailer; starts their goroutines
		s.reattach(g, spec.Outputs{}, s.spec.Outputs) // starts the outputs and publishes the View
		reason := s.serve(ctx, g)              // returns on ctx, on a rebuild edit, or when a part of g ended
		s.teardown(g)                          // publishes the empty View first, then stops and waits, source hub first
		switch reason {
		case stopCtx:
			return
		case stopEdit:                         // s.spec is already the new one; no wait
			s.rebuilds++
		case stopFailed:
			s.restarts++
			if s.now().Sub(g.started) >= 60*time.Second { wait = time.Second }
			if !s.idle(ctx, wait) { return }   // state "backoff"; an edit arriving here replaces s.spec and ends the wait
			wait = min(2*wait, 30*time.Second)
		}
	}
}
```

`serve` selects on `ctx.Done()`, the mailbox signal and `g.failed` (closed when the pipeline's `Run` or the transcoder's `Run` returns while `g`'s context is live; its error, scrubbed, becomes `LastError`). For each queued edit, in order: `spec.Diff(s.spec, next)`; `ChangeNone` answers at once; `ChangeOutputs` calls `reattach(g, s.spec.Outputs, next.Outputs)`, sets `s.spec`, answers; `ChangeRebuild` sets `s.spec`, returns `stopEdit`, and the answer is sent after the next generation's `reattach`. Edits queued behind a rebuild are applied to the new generation the same way.

**`reattach(g, old, new spec.Outputs)`** makes the smallest set of calls (R-D6), in this order:
1. Packagers: with `want := new.HLS || new.DASH` and `have := old.HLS || old.DASH`, start them all on `want && !have`, stop them all (cancel, close their subscriptions, wait) on `!want && have`, and touch none otherwise.
2. Pushes, keyed by URL: stop and wait for each one in `old` and not in `new`; start one for each in `new` and not in `old`, on `Single[s.spec.Name]`; leave the rest.
3. Publish the new `View` (new sessions of a disabled output now find nothing).
4. On every distinct hub in `Single`, call `Refuse` with the prefixes of the disabled session outputs: `"httpts "` for `!new.HTTPTS`, `"rtmp "` for `!new.RTMP`, `"rtsp "` for `!new.RTSP`, `"srt "` for `!new.SRT`. (`"rtmp "` does not match `"rtmppush …"`.)

`teardown` cancels `g`'s context, closes the source hub, waits for the transcoder and thumbnailer (which close the rendition and presentation hubs), then for the packagers and pushers, as osdev's shutdown does. `State` is `starting` until the source hub is `Live()`, then `running`.

**`Collect`** writes, with `stream=<name>` on every series (help texts are the implementer's, one sentence each):

| Family | Type | Extra labels | Source |
|---|---|---|---|
| `openstreamer_stream_up` | gauge | | 1 when the source hub is live |
| `openstreamer_stream_generation` | gauge | | `View.Gen` |
| `openstreamer_stream_restarts_total` | counter | `reason` = `failure`, `edit` | `Restarts`, `Rebuilds` |
| `openstreamer_stream_active_input` | gauge | | `pipeline.Stats.Active` |
| `openstreamer_stream_input_switches_total` | counter | | `Switches` |
| `openstreamer_stream_unmapped_frames_total` | counter | | `Unmapped` |
| `openstreamer_stream_drift_breaks_total` | counter | | `Clock.DriftBreaks` |
| `openstreamer_hub_subscribers` | gauge | `name` | `len(hub.Stats())` per served name |
| `openstreamer_packager_errors_total` | counter | `name`, `kind` = `mux`, `codec`, `anchor` | `Errors`, `CodecErrors`, `FallbackAnchors` |
| `openstreamer_packager_rung_gaps_total` | counter | `name`, `track` | `Rungs()[track].Gaps` |
| `openstreamer_transcoder_up` | gauge | | 1 when `Stats.PID != 0` |
| `openstreamer_transcoder_spawns_total`, `_exits_total`, `_losses_total`, `_frames_in_total` | counter | | `Spawns`, `Exits`, `Losses`, `FramesIn` |
| `openstreamer_transcoder_frames_out_total` | counter | `rendition` | `FramesOut[i]` |
| `openstreamer_transcoder_dropped_frames_total` | counter | `reason` = `before_key`, `merge`, `oversize`, `presentation` | `DroppedBeforeKey`, `MergeDropped`, `Oversize`, `PresentationDropped` |
| `openstreamer_transcoder_encoder_fallbacks_total` | counter | | `Child.EncoderFallbacks` |
| `openstreamer_transcoder_media_errors_total` | counter | `kind` = `audio_corrupt`, `video_corrupt`, `audio_dropped`, `reanchors`, `dropped_tracks` | `Child.*` |
| `openstreamer_transcoder_thumbnails_total` | counter | `result` = `ok`, `failed` | `Child.Thumbnails`, `Child.ThumbnailsFailed` (or the Thumbnailer's) |
| `openstreamer_push_up` | gauge | `target` (index) | 1 when the pusher's state is `rtmppush.StatePublishing` |
| `openstreamer_push_connects_total`, `_failures_total`, `_republishes_total`, `_messages_total` | counter | `target` | the pusher's `Stats` |

Transcoder families are written only for a stream with a ladder or a thumbnailer.

- [ ] **Step 1: Write the failing tests.** `helpers_test.go` has `scriptedOpen`, an `Env.Open` whose readers emit frames of `testdata/fixtures/h264_aac.ts` in a loop under test control (and can be told to fail), a fake `After`, and `tap(h)`, a subscriber that records frames. Tests with a ladder use `transcodetest`'s fake child: `Env.Transcoder` is the test binary (`os.Args[0]`, whose `TestMain` calls `transcodetest.Main`), with `TranscoderArgs`/`TranscoderEnv` as `internal/transcode`'s own tests set them.

```go
func TestCheck(t *testing.T)
// one row per Check rule: a URL ingest.Parse refuses; an encoded rung with Transcoder ""; an image watermark with AssetsDir "";
// an asset that does not exist; a watermark next to a copy rung; a thumbnail with Transcoder ""; a push that is not rtmp://;
// an SRT listen input on SRTPlay's port; an RTMP listen input publishing at live/cam on RTMPPlay with RTMPApp "live".
// Each error starts "stream: check:" and holds no password. A copy-only ladder with copy audio and no binary passes.
func TestSupervisorServes(t *testing.T)
// no ladder, every output on: View.Gen is 1, Single and Packs have exactly "cam"; state goes starting → running; the tap on
// View().Source gets frames; after ctx is cancelled Run returns, the source hub is closed and View() is empty.
func TestSupervisorLadderNames(t *testing.T)
// labels 720, 360 (fake child): Single has cam, cam_720, cam_360 with Single["cam"] == Single["cam_720"] != View().Source;
// Packs has the same three names; a frame on Packs["cam"]'s hub carries the presentation's track numbers.
func TestSupervisorOutputEditInPlace(t *testing.T)
// every output on, 2 pushes to test sinks, subscribers "rtmp cam", "rtsp cam", "srt 1.1.1.1:1", "httpts 1.1.1.1:2" opened by
// the test on Single["cam"]. Submit(rtsp off) answers ChangeOutputs: the "rtsp cam" channel is closed, the other three get the
// next 50 frames with no Lost; View.Gen is still 1; the same *hub.Hub and *live.Packager pointers; both pushers' Connects still 1;
// a new "rtsp cam" subscription comes back closed. Submit(rtsp on): it subscribes again. Repeated for httpts, rtmp and srt.
func TestSupervisorPackagerEdit(t *testing.T)
// hls+dash → dash only: the Packager pointers are unchanged and still listing segments; → neither: Packs is empty and
// the "packager cam" subscriber is gone from hub.Stats(); → hls: new packagers, and "rtmp cam" saw no Lost throughout.
func TestSupervisorPushEdit(t *testing.T)
// pushes [A, B] → [B, C]: A's sink sees its connection end, C's sink gets a publish, B's sink sees one connection for the
// whole test and B's Connects is 1.
func TestSupervisorRebuildOnOtherEdits(t *testing.T)
// Submit(a second input) answers ChangeRebuild: Gen is 2, the old source hub is closed, Rebuilds is 1, Restarts 0, and the
// answer arrives only after View().Gen == 2. No backoff was waited (the fake After was not called).
func TestSupervisorRestartBackoff(t *testing.T)
// a fake child of another wire version (the transcoder's Run returns an error): state "backoff", LastError set, and the fake
// After sees 1 s, 2 s, 4 s … 30 s, 30 s; a generation that lives 60 s (fake Now) resets it to 1 s. Restarts counts each.
func TestSupervisorDeadInput(t *testing.T)
// an input whose Open always fails with an error naming "rtsp://u:hunter2@h/x": the supervisor stays in one generation (the
// pipeline retries), state "starting", LastError without "hunter2"; when Open starts succeeding the state becomes "running".
func TestSupervisorStopDuringBackoff(t *testing.T)
// in "backoff" with a 30 s wait pending: cancelling ctx makes Run return within 1 s; and, separately, a Submit during the
// wait is applied at once (Gen advances without the fake After firing).
func TestSupervisorSubmitOrder(t *testing.T)
// 50 Submits alternating two output sets, sent without waiting: every channel answers, in order, and the final View.Outputs
// is the last spec's. A Submit after Run returned gets a closed channel.
func TestSupervisorNoLeaks(t *testing.T) // start, 3 output edits, 1 rebuild, stop: runtime.NumGoroutine returns to its starting value within 2 s
func TestCollect(t *testing.T)
// a laddered stream with one push: metricstest.Parse accepts the output; the families of the table are present with
// stream="cam"; openstreamer_transcoder_encoder_fallbacks_total{stream="cam"} is the fake child's count; no label value contains "://".
func TestStatusRedacts(t *testing.T) // inputs and push targets in Status hold no password or stream key; json.Marshal(Status) neither
```

- [ ] **Step 2: Run** `go test -race -count=1 ./internal/stream/` — compile failure.
- [ ] **Step 3: Implement** `env.go` and `Check`; run `-run TestCheck` — pass.
- [ ] **Step 4: Implement** the generation, the loop, `reattach` and `teardown`; run `-run 'TestSupervisor'` — pass.
- [ ] **Step 5: Implement** `Status`, `Shot` and `Collect`; run `go test -race -count=1 ./internal/stream/` — pass; then `-count=20 -run 'TestSupervisor(OutputEditInPlace|SubmitOrder|NoLeaks)'` — pass.
- [ ] **Step 6: Commit** `feat(stream): the per-stream supervisor with in-place output edits`

### Task 8: `stream.Manager`

**Files:**
- Create: `internal/stream/manager.go`, `internal/stream/claims.go` (listen claims, `copy://` cycles, served-name lookup)
- Test: `internal/stream/manager_test.go`, `claims_test.go`

**Interfaces:**
- Consumes: Task 7; a `Store`, which Task 6's `*store.FS` satisfies.
- Produces:

```go
const MaxStreams = 1000

type Store interface {
	Put(sp spec.Stream) error
	Delete(name string) error
}

var (
	ErrNotFound     = errors.New("stream: not found")
	ErrInvalid      = errors.New("stream: invalid spec")       // wraps Validate's or Check's error
	ErrConflict     = errors.New("stream: conflict")           // a listen claim or a copy:// cycle
	ErrPrecondition = errors.New("stream: precondition failed") // If-Match
	ErrTooMany      = errors.New("stream: too many streams")
	ErrClosed       = errors.New("stream: manager closed")
)

type PutResult struct {
	Stream  spec.Stream
	Created bool
	Applied string // "created", "unchanged", "outputs", "rebuild", "started", "stopped"
}

type Manager struct{ /* unexported */ }
// NewManager takes the streams the store loaded. env.Lookup is replaced by the manager's CopyHub.
func NewManager(env Env, st Store, initial []spec.Stream) *Manager
// Run starts a supervisor for every enabled initial stream that passes Check (one that fails is kept, stopped, with the
// error as its LastError) and returns when ctx ends and every supervisor has stopped.
func (m *Manager) Run(ctx context.Context)
func (m *Manager) Put(ctx context.Context, sp spec.Stream, ifMatch string) (PutResult, error)
func (m *Manager) Delete(ctx context.Context, name, ifMatch string) error
func (m *Manager) Get(name string) (spec.Stream, bool)
func (m *Manager) List() []spec.Stream                    // sorted by name
func (m *Manager) Status(name string) (Status, bool)      // a stream without a supervisor reports state "stopped"
func (m *Manager) Shot(name string) (picture.Shot, time.Time, bool)
func (m *Manager) Hub(out Output, served string) (*hub.Hub, bool)            // out is httpts, rtmp, rtsp or srt
func (m *Manager) Packager(out Output, served string) (*live.Packager, bool) // out is hls or dash
func (m *Manager) CopyHub(name string) (*hub.Hub, bool)   // <stream>: its source hub; <stream>_<label>: that rendition's hub
func (m *Manager) Collect(w *metrics.Writer)              // every supervisor's Collect, by name, and openstreamer_streams{state}
```

`Put`, under the manager's mutex: `ErrClosed` after `Run` ended; `sp.Validate()` and `Check` (`ErrInvalid`); `ifMatch`, when not empty, must be `*` or the current spec's `ETag()`, and a missing stream fails it (`ErrPrecondition`); a new name with `MaxStreams` streams present is `ErrTooMany`; the claims of the resulting set (`ErrConflict`); an equal document returns `unchanged` without writing; then `st.Put` (its error is returned as is, and nothing changed); then the map; then one of: start a supervisor (`created` or `started`), stop one (`stopped`: cancel, and wait after unlocking), or `Submit`. **The mutex is released before waiting** for a `Submit`'s answer or a stopped supervisor; because `Submit` is queued under the mutex, two `Put`s of one stream reach the supervisor in the order they reached the store. The wait ends early when `ctx` ends, in which case the edit stays applied and `Applied` is what `spec.Diff` said. A `disabled` stream has no supervisor; `Put` of a disabled, never-started stream is `created`.

The lookups split a served name at its first `_` (stream names have none), find the supervisor, read its `View` and test the output's flag: `Hub(OutRTSP, "cam_720")` is `View.Single["cam_720"]` when `View.Outputs.RTSP`. They take a read lock only to find the supervisor.

Claims (`claims.go`): `listenClaim(url string) (key string, ok bool)` gives `rtmp|<addr key>|<normalised path>` for an RTMP listen input (`rtmpsrv.AddrKey`, `rtmp.NormalizePath`, default port 1935) and `udp|<port>|<host or "">` for a UDP or SRT listen input (`ingest.Parse(url).Passive`); two UDP claims collide on equal ports when the hosts are equal or either is a wildcard. `copy://` cycles: a depth-first walk over "stream reads stream" edges (a `copy://x_label` edge goes to `x`), reporting the path as `a -> b -> a`.

- [ ] **Step 1: Write the failing tests** (a fake `Store` that records calls and can fail; `scriptedOpen` from Task 7)

```go
func TestManagerPutCreatesAndEdits(t *testing.T)
// Put → created, store has it, Status running. The same again → unchanged, no second store write. outputs.rtsp off → outputs,
// same generation. A new input → rebuild, generation 2. disabled → stopped, Status "stopped", Hub(OutRTMP,"cam") not found.
// enabled again → started.
func TestManagerPutOrderStoreFirst(t *testing.T) // the store's Put failing returns its error; Get still returns the old spec and the generation did not move
func TestManagerInvalid(t *testing.T)            // Validate and Check failures are ErrInvalid, not stored; 1001st stream is ErrTooMany
func TestManagerIfMatch(t *testing.T)            // a stale ETag → ErrPrecondition on Put and Delete; the current one and "*" pass; If-Match on a missing stream fails
func TestManagerListenClaim(t *testing.T)
// stream a with rtmp://127.0.0.1:P/in/key?mode=listen; Put of b with the same URL → ErrConflict, nothing stored, a's generation
// unchanged. The same for two udp://:P inputs, for udp://0.0.0.0:P against srt://127.0.0.1:P?mode=listener, and not for
// different ports or different RTMP paths. Editing a itself, keeping its input, is not a conflict.
func TestManagerCopyCycle(t *testing.T) // a copies b, Put b copying a → ErrConflict naming "b -> a -> b"; a copying a_720 → conflict; a copying an unknown stream is accepted
func TestManagerLookups(t *testing.T)
// laddered cam with rtsp off: Hub(OutRTMP,"cam_360") found, Hub(OutRTSP,"cam") not, Hub(OutRTMP,"cam_x"), "nope", "" and "_" not;
// Packager(OutHLS,"cam") is the ABR packager; with hls off and dash on, Packager(OutHLS,…) not found and Packager(OutDASH,…) found;
// CopyHub("cam") is the source, CopyHub("cam_360") the rendition.
func TestManagerDelete(t *testing.T)   // Delete stops the supervisor, removes the store file, lookups fail; a second Delete is ErrNotFound
func TestManagerRunRestores(t *testing.T)
// initial: two enabled, one disabled, one whose Check fails (an encoded rung, Transcoder ""): two run, the disabled is "stopped",
// the failing one is "stopped" with LastError; List returns all four. Cancelling ctx stops everything; a later Put is ErrClosed.
func TestManagerConcurrentPuts(t *testing.T)
// 20 goroutines Put one stream, alternating an output edit and a rebuild edit, and one goroutine Deletes it half-way; then
// one more Put. No deadlock within 10 s; the final Get equals the store's last Put; View().Outputs equals it; no goroutine left
// after Run ends.
func TestManagerCollect(t *testing.T)  // openstreamer_streams{state="running"} 2, {state="stopped"} 1; every per-stream family carries a stream label of a known name
```

- [ ] **Step 2: Run** `go test -race -count=1 -run 'TestManager|TestListenClaim|TestCopyCycle' ./internal/stream/` — compile failure.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/stream/` and `-count=20 -run TestManagerConcurrentPuts` — pass.
- [ ] **Step 5: Commit** `feat(stream): the manager, cross-stream checks and output lookups`

### Task 9: `internal/api` — the REST API and its OpenAPI document

**Files:**
- Create: `internal/api/doc.go`, `api.go` (routes, auth, wrappers), `streams.go` (handlers), `errors.go`, `api/openapi.json`
- Test: `internal/api/api_test.go`, `streams_test.go`, `openapi_test.go`

**Interfaces:**
- Consumes: Task 8's `PutResult` and errors, Task 7's `Status`, Task 2, Task 3.
- Produces:

```go
package api

// Streams is what the API needs from stream.Manager.
type Streams interface {
	Put(ctx context.Context, sp spec.Stream, ifMatch string) (stream.PutResult, error)
	Delete(ctx context.Context, name, ifMatch string) error
	Get(name string) (spec.Stream, bool)
	List() []spec.Stream
	Status(name string) (stream.Status, bool)
	Shot(name string) (picture.Shot, time.Time, bool)
}

type Options struct {
	Streams Streams
	Token   string       // at least MinTokenBytes
	Metrics http.Handler // served at /metrics
	Version string
	Logger  *slog.Logger // nil discards
	Now     func() time.Time
}
const MinTokenBytes = 16

type API struct{ /* unexported */ }
func New(opt Options) (*API, error)       // an error for a short token or a nil Streams
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request)
func (a *API) Collect(w *metrics.Writer)  // openstreamer_api_requests_total{method,route,code}
func Routes() []string                    // every route as "METHOD /path/{param}", sorted
```

Routes and answers:

| Route | Success | Notes |
|---|---|---|
| `GET /healthz` | 200 `{"status":"ok"}` | no token |
| `GET /api/v1/version` | 200 `{"version":"…"}` | |
| `GET /api/v1/streams` | 200 `{"streams":[spec…]}` | sorted by name; `?secrets=1` |
| `GET /api/v1/streams/{name}` | 200 spec, `ETag` | `?secrets=1` |
| `PUT /api/v1/streams/{name}` | 201 or 200 `{"stream":spec,"applied":"…"}`, `ETag` | `If-Match`; the body's `name` may be empty and must otherwise equal the path's |
| `DELETE /api/v1/streams/{name}` | 204 | `If-Match` |
| `GET /api/v1/streams/{name}/status` | 200 `stream.Status` | |
| `GET /api/v1/streams/{name}/thumbnail.jpg` | 200 `image/jpeg` | `ETag` `"<epoch>-<pts>"`, `Cache-Control: no-cache`, served with `http.ServeContent` as osdev's `thumbHandler`; 404 until the first shot |
| `GET /metrics` | the `Metrics` handler | |
| `GET /debug/pprof/…` | `net/http/pprof`'s `Index`, `Cmdline`, `Profile`, `Symbol`, `Trace` | |

Errors are `{"error":{"code":"…","message":"…"}}` with `Content-Type: application/json`, the message passed through `logging.Scrub`:

| Status | `code` | When |
|---|---|---|
| 400 | `bad_request` | the body is not one JSON document, has an unknown field, or its `name` differs from the path's |
| 401 | `unauthorized` | no or a wrong bearer token; with `WWW-Authenticate: Bearer` |
| 404 | `not_found` | `stream.ErrNotFound`, an unknown stream, an invalid name in the path, no thumbnail yet |
| 409 | `conflict` | `stream.ErrConflict` |
| 412 | `precondition_failed` | `stream.ErrPrecondition` |
| 413 | `too_large` | a body over `spec.MaxSpecBytes` |
| 415 | `unsupported_media_type` | a `PUT` whose `Content-Type` is not `application/json` |
| 422 | `invalid_spec` | `stream.ErrInvalid`, `stream.ErrTooMany` |
| 500 | `internal` | anything else (a store failure); the cause is logged, the message is "internal error" |
| 503 | `unavailable` | `stream.ErrClosed` |

Every response sets `Cache-Control: no-store` (except the thumbnail) and `X-Content-Type-Options: nosniff`. Responses redact (`spec.Stream.Redacted`) unless the query is exactly `secrets=1`. The access log line (R-D10) and the request counter use `r.Pattern`'s path as `route`; a request no pattern matches counts under `route="unmatched"`. A failed authentication is logged at `warn` at most once every 10 s (`Options.Now`), with the remote address and never the header.

`api/openapi.json` is OpenAPI 3.1: every route above with its parameters, the `If-Match` and `ETag` headers, the `secrets` query parameter, a `bearerAuth` security scheme on all but `/healthz`, and `components.schemas` for `Stream`, `Input`, `Transcode`, `Rendition`, `Audio`, `Watermark`, `Thumbnail`, `Outputs`, `Push`, `Status`, `PutResult` and `Error`, each property with a one-line description and the limits of Task 2's `Validate`.

- [ ] **Step 1: Write the failing tests** (a fake `Streams`; `httptest`)

```go
func TestAuth(t *testing.T)
// every route but /healthz: no header, "Bearer wrong", "Basic …" and "bearer <token>" in lower case → 401 with WWW-Authenticate
// and the error body (the scheme is case-insensitive: lower-case "bearer" with the right token is 200); /healthz is 200 without
// a header. New with a 15-byte token is an error. 100 failed requests log one warning.
func TestStreamCRUD(t *testing.T)
// PUT → 201, applied "created", ETag equal to the spec's; GET returns it with the same ETag; PUT again → 200 "unchanged";
// list holds it; DELETE → 204; GET → 404. PUT with a body name "other" → 400; with no name → the path's.
func TestPutErrors(t *testing.T)
// each row of the error table reachable from PUT: malformed JSON 400, unknown field 400, text/plain 415, 1 MiB + 1 byte 413,
// ErrInvalid 422, ErrConflict 409, ErrPrecondition 412, ErrTooMany 422, a store error 500 with message "internal error" and
// the cause in the log, ErrClosed 503. Each body is the error object with the table's code.
func TestPathNames(t *testing.T) // PUT and GET of "..", "a%2Fb", "A", "x_y" and a 64-byte name → 404, and Streams was not called
func TestResponsesRedact(t *testing.T)
// a stored spec with rtsp://u:hunter2@h/a and push rtmp://h/live/secretkey: GET, the list and the PUT response hold neither
// "hunter2" nor "secretkey"; with ?secrets=1 they hold both; ?secrets=0 and ?secrets=1&x=1 redact.
func TestPutRejectsMaskedSecrets(t *testing.T)
// through a real stream.Manager on a fake store: PUT a spec with a password; GET it (redacted); PUT that body back → 422
// invalid_spec; GET ?secrets=1 still returns the password.
func TestIfMatchHeaders(t *testing.T) // the If-Match value reaches Streams.Put and Delete verbatim; a 412 has no ETag header
func TestStatusAndThumbnail(t *testing.T)
// status is the fake's document; the thumbnail is the fake's JPEG bytes with image/jpeg and the ETag "3-90000"; If-None-Match
// with that ETag → 304; no shot → 404; an unknown stream → 404.
func TestMetricsAndPprof(t *testing.T) // /metrics is the given handler behind the token; /debug/pprof/ is 200 behind it and 401 without
func TestRequestCounter(t *testing.T)
// after 2 GETs of two different streams and one 404 path: Collect has openstreamer_api_requests_total{method="GET",
// route="/api/v1/streams/{name}",code="200"} 2 and a route="unmatched" series; no series holds a stream name.
func TestAccessLog(t *testing.T) // one info line per request with method, route, status, duration; a request with Authorization and ?secrets=1 logs neither the token nor the query
func TestOpenAPIMatchesRoutes(t *testing.T)
// api/openapi.json parses; the set of "METHOD path" in its paths equals Routes() minus the pprof routes; every operation
// except /healthz requires bearerAuth.
func TestOpenAPIMatchesSpec(t *testing.T)
// by reflection: for each schema named above, its property names equal the JSON field names of the Go type (spec.*,
// stream.Status and its nested types, the PutResult response, the error object), and each property has a description.
```

- [ ] **Step 2: Run** `go test -race -count=1 ./internal/api/` — compile failure.
- [ ] **Step 3: Implement** the handler; write `api/openapi.json`.
- [ ] **Step 4: Run** the same command — pass.
- [ ] **Step 5: Commit** `feat(api): the REST API, bearer auth and the OpenAPI document`

### Task 10: `internal/server` and `cmd/openstreamer`

**Files:**
- Create: `internal/server/doc.go`, `config.go`, `server.go`, `media.go` (the `/live/` handler and its output gate), `collect.go` (the output servers' metrics), `cmd/openstreamer/main.go`, `openstreamer.example.json`
- Modify: `Makefile` (target `openstreamer`: `go build -ldflags "-X main.version=$(shell git describe --tags --always --dirty)" -o bin/openstreamer ./cmd/openstreamer`, added to `.PHONY`)
- Test: `internal/server/config_test.go`, `server_test.go`, `media_test.go`, `cmd/openstreamer/main_test.go`

**Interfaces:**
- Consumes: Tasks 3, 4, 5 (`Registry.Addr`), 6, 8, 9; `rtmpplay.New`, `rtspplay.New`, `srtout.New`, `httpts.Handler`, `live.Handler`, `rtmpsrv.Default.RegisterPlay`.
- Produces:

```go
package server

type Config struct {
	DataDir string `json:"data_dir"`   // required; the store is <data_dir>/streams
	HTTP    struct{ Listen string `json:"listen"` } `json:"http"` // media, /live/; default ":8080"
	API     struct {
		Listen string `json:"listen"` // default "127.0.0.1:8081"
		Token  string `json:"token"`  // or OPENSTREAMER_API_TOKEN, which wins
	} `json:"api"`
	RTMP struct{ Listen, App string } `json:"rtmp"`                 // json "listen", "app"; "" listen: off; app default "live"
	RTSP struct{ Listen, App string; UDP bool } `json:"rtsp"`       // json "listen", "app", "udp"
	SRT  struct{ Listen, App string; LatencyMS int } `json:"srt"`   // json "listen", "app", "latency_ms" (default 120, 1…65535)
	Transcoder struct{ Binary string `json:"binary"` } `json:"transcoder"` // "" : ostranscoder next to the executable when it exists, else none
	AssetsDir  string `json:"assets_dir"`                           // default <data_dir>/assets when that directory exists, else none
	Log struct{ Level, Format string } `json:"log"`                 // json "level", "format"
}

// LoadConfig reads and checks the file: strict JSON, defaults filled, addresses well-formed, the token present and long enough.
func LoadConfig(path string, getenv func(string) string) (Config, error)

type Options struct {
	Version string
	LogOut  io.Writer // nil means os.Stderr
}
type Addrs struct{ HTTP, API, RTMP, RTSP, SRT string } // host:port as bound; "" when off

type Server struct{ /* unexported */ }
// New opens the store, loads the streams, and binds every listener, so Addrs is final when it returns. It starts no stream.
func New(cfg Config, opt Options) (*Server, error)
func (s *Server) Addrs() Addrs
func (s *Server) Manager() *stream.Manager
// Run serves until ctx ends, then stops in order and returns nil, or the first listener error.
func (s *Server) Run(ctx context.Context) error

// LiveOutputs returns the outputs a /live/<name>/<dir>/<file> path is part of: hls/* and cmaf/*.m3u8 → [hls];
// cmaf/*.mpd → [dash]; any other cmaf file → [hls, dash]; anything else → nil.
func LiveOutputs(dir, file string) []stream.Output
```

Wiring, following osdev's `run`: the media mux has `GET /live/{stream}/stream.ts` → `httpts.Handler(func(n) { return m.Hub(OutHTTPTS, n) }, 0)` and `/live/` → a gate in front of `live.Handler`: for `/live/{stream}/{dir}/{file}`, 404 unless `m.Packager(out, stream)` finds a packager for one `out` of `LiveOutputs(dir, file)`; the `live.Handler` behind it looks the packager up with either output. RTMP play registers `rtmpplay.New(lookup(OutRTMP), …)` on `rtmpsrv.Default` (the registry ingest's listen inputs share) and reads the port from `Registry.Addr`. RTSP and SRT start as osdev's `startRTSP` and `startSRT`. The media server has osdev's `newServer` limits (no read or write timeout: HTTP-TS responses are long-lived); the API server has `ReadHeaderTimeout` 10 s, `ReadTimeout` 30 s, `WriteTimeout` 60 s, `IdleTimeout` 2 min, `MaxHeaderBytes` 64 KiB. The metrics registry gets `metrics.Process`, `Manager.Collect`, `API.Collect`, `openstreamer_build_info{version,goversion} 1`, `openstreamer_store_load_errors` (the number of files `Load` skipped; each is also logged at `error`), and from `collect.go`:

| Family | Type | Labels | Source |
|---|---|---|---|
| `openstreamer_output_sessions` | gauge | `output` = `rtmp`, `rtsp`, `srt` | `Players`, `Players`, `Conns` |
| `openstreamer_output_sessions_started_total` | counter | `output` = `rtmp`, `rtsp` | `PlayersStarted` |
| `openstreamer_output_sessions_ended_total` | counter | `output`, `reason` | each `Ended*` field, the reason being the field name after `Ended` in snake case (`hub_closed`, `write_error`, …) |
| `openstreamer_output_resyncs_total` | counter | `output` | `Resyncs` |
| `openstreamer_output_rejected_total` | counter | `output` = `srt`, `reason` = `unknown`, `mode`, `caps`, `mss` | `Rejected*` |

Shutdown order when `ctx` ends: stop accepting API requests (`Shutdown`, 2 s, then `Close`); cancel the manager's context and wait for `Manager.Run` (every hub closes); unregister RTMP play, stop RTSP, stop SRT (each waits for its sessions, which ended with their hubs); shut the media server down (2 s, then `Close`). A listener that fails after start-up ends `Run` with that error, after the same stop.

`cmd/openstreamer`: `func run(ctx context.Context, args []string, stdout, stderr io.Writer) int`, flags `-config path` (required) and `-version`; `main` uses `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)`. It prints one line per bound listener to stdout (`http <addr>`, `api <addr>`, `rtmp <addr>`, `rtsp <addr>`, `srt <addr>`), then runs. Exit codes: 0 after a clean stop, 1 when `Run` returns an error, 2 for a usage, config or start-up error (printed to stderr, scrubbed, never the token). `openstreamer.example.json` sets `data_dir` `/var/lib/openstreamer`, `http.listen` `:8080`, `api.listen` `127.0.0.1:8081`, an empty `api.token` (with the README telling the operator to set `OPENSTREAMER_API_TOKEN`), `rtmp.listen` `:19350`, `rtsp.listen` `:5554`, `srt.listen` `:9999`; **it never names 1935, 554 or 8554**, and no test loads it to start a server.

- [ ] **Step 1: Write the failing tests** (every listener `127.0.0.1:0`; `t.TempDir()` as `data_dir`; token `test-token-0123456789`)

```go
func TestLoadConfig(t *testing.T)
// defaults filled from {"data_dir":"/x","api":{"token":"…16+…"}}; the env token wins over the file's; an unknown key, no data_dir,
// no token, a 15-byte token, "listen":"nope", srt latency 0 with a listen address or 70000, log level "loud" are errors,
// and no error text contains the token. openstreamer.example.json decodes strictly (with the token from getenv) and names no forbidden port.
func TestLiveOutputs(t *testing.T) // hls/index.m3u8, hls/3.ts, hls/v1.m3u8 → [hls]; cmaf/master.m3u8, cmaf/a1.m3u8 → [hls]; cmaf/index.mpd → [dash]; cmaf/init-0-1.mp4, cmaf/0-7.m4s → [hls, dash]; x/y → nil
func TestServerStartsAndStops(t *testing.T)
// New binds five listeners with ports above 0; /healthz on the API address is 200; the media address answers 404 for an
// unknown stream; cancelling ctx makes Run return nil within 5 s, after which all five ports refuse connections and
// runtime.NumGoroutine is back within 2 s.
func TestServerListenersOff(t *testing.T) // rtmp, rtsp and srt listen "" → Addrs has "" for them and a stream with those outputs is still accepted and served over HLS
func TestServerLiveGate(t *testing.T)
// a stream from a looped fixture with hls on and dash off: hls/index.m3u8 and cmaf/master.m3u8 are 200, cmaf/index.mpd is 404,
// the init and a segment are 200; with only dash: the reverse, segments still 200; with neither: all 404; stream.ts is 404
// while httpts is off and streams while it is on.
func TestServerLoadErrors(t *testing.T) // a truncated file in <data_dir>/streams: New succeeds, the log has one error line naming the file, /metrics has openstreamer_store_load_errors 1
func TestServerMetricsParse(t *testing.T)
// with one running stream and one RTMP player (conform.ReadRTMP for 1 s): metricstest.Parse accepts /metrics; it has
// openstreamer_build_info, go_goroutines, openstreamer_stream_up{stream="cam"} 1, openstreamer_output_sessions_started_total{output="rtmp"} 1.
func TestServerBindFailure(t *testing.T) // http.listen on a port a test listener holds: New returns an error naming the address and leaves no other listener bound
func TestRun(t *testing.T)
// in cmd/openstreamer: -version prints the version and returns 0; no -config, a missing file and a bad config return 2 with a
// message on stderr; with a good config the five address lines appear on stdout, /healthz answers, and cancelling ctx returns 0.
```

- [ ] **Step 2: Run** `go test -race -count=1 ./internal/server/ ./cmd/openstreamer/` — compile failure.
- [ ] **Step 3: Implement** `config.go`, then the server, then `main.go`, the example config and the make target.
- [ ] **Step 4: Run** the same command, `make openstreamer` (a binary at `bin/openstreamer`; `bin/openstreamer -version` prints a version), `GOOS=windows go vet ./...` and `GOOS=darwin go vet ./...` — pass.
- [ ] **Step 5: Commit** `feat(server): the openstreamer binary, its config and its wiring`

### Task 11: The M5a scenarios

**Files:**
- Create: `internal/e2e/m5a_test.go`

**Interfaces:**
- Consumes: `server.New`, `server.Config`, `Server.Addrs`, `Server.Manager` (only for the tap: `Manager().CopyHub`), `metricstest`, `conform.ReadHLS`/`ReadDASH`/`ReadTS`/`ReadRTMP`/`ReadRTSP`, `testpeer`, and the package's existing helpers (`fixtureBytes`, `tsGaps`, `assertTSMatchesHub`, `rawHTTP`, `rawSRT`).
- Produces, for later milestones' tests: `m5aServer(t, mutate func(*server.Config)) *m5a` (a running server on ephemeral ports with a captured log buffer, stopped by `t.Cleanup`), `(*m5a).put(t, sp spec.Stream) (code int, applied string)`, `(*m5a).get(t, path string) (int, []byte)`, `(*m5a).url(out stream.Output, served string) string` (the play URL of an output: `http://…/live/<n>/hls/index.m3u8`, `…/cmaf/index.mpd`, `…/stream.ts`, `rtmp://…/live/<n>`, `rtsp://…/live/<n>`, `srt://…?streamid=live/<n>`), and `m5aSpec(name, input string) spec.Stream` (every output on, no push).

The source is `file://<abs testdata/fixtures/h264_aac.ts>?loop=1`: a file input is paced in real time and each loop is one epoch change, which every reader below must also survive. The tests talk to the server over HTTP with the token; the only in-process access is the tap.

**The reader set** (`m5aReaders`): one long-lived reader per output of a served name, started together and stopped together: HLS-TS and DASH by `conform.ReadHLS`/`ReadDASH` polling for the span, HTTP-TS and SRT as raw byte captures (`rawHTTP`, `rawSRT`), RTMP by `conform.ReadRTMP`, RTSP by `conform.ReadRTSP` (TCP). `assertUninterrupted(t, readers, tap, except)` requires, for each reader not in `except`: it ended only because the test stopped it; no conform violation; per track, its frames are exactly the tap's frames between its own first and last DTS, each once, with the tap's epochs (TS captures through `assertTSMatchesHub` and `tsGaps == 0`); and the HLS playlists show a discontinuity only where the tap has an epoch change.

- [ ] **Step 1: Write the tests** (they fail until they are written right; the code they test exists)

```go
func TestM5aEveryOutput(t *testing.T)
// PUT sink (input rtmp://127.0.0.1:<rtmp port>/in/k0123456789?mode=listen, outputs hls) and PUT cam (m5aSpec with a push to
// that URL): both 201 "created". For cam, 8 s of each of hls/index.m3u8, cmaf/master.m3u8, cmaf/index.mpd, stream.ts, RTMP,
// RTSP and SRT: no violation, video and audio frames present, and at least one epoch change survived (the file looped).
// sink's HLS has frames too (the push arrived and the listen input took it). GET cam/status: state "running", active_input 0,
// two tracks, pushes[0].state "publishing" with connects 1. GET /api/v1/streams lists both, redacted (no "k0123456789").
func TestM5aEditOutputs(t *testing.T)
// cam with every output and a push to sink. For each row of {rtsp, rtmp, srt, httpts, hls, dash, push}: start the reader set
// and the tap; after 3 s PUT cam with that output off (push: the list emptied) → 200 "outputs"; after 3 s more PUT it back on
// → 200 "outputs"; after 3 s stop the readers. Then:
//   - the row's own reader ended at the first edit (a 404 for hls and dash paths; for push, sink's source goes quiet), and a
//     fresh reader of that output works after the second edit;
//   - assertUninterrupted for every other reader over the whole 9 s;
//   - status.generation is unchanged, input_switches unchanged, and /metrics shows openstreamer_stream_restarts_total{stream="cam"}
//     unchanged for both reasons;
//   - openstreamer_output_sessions_started_total{output="rtmp"} and {output="rtsp"} rose by exactly 1 on their own row (the fresh
//     reader) and by 0 on every other row;
//   - for every row but push, sink saw one connection for the whole test (push connects still 1).
// The hls row keeps dash on and the dash row keeps hls on; a last row turns both off and on again and checks that the four
// session outputs were not interrupted.
func TestM5aRebuildIsTheOtherRoute(t *testing.T)
// the contrast that keeps the test above honest: PUT cam with a second input → 200 "rebuild", status.generation 2, and the
// RTMP reader that ran across it ended. Afterwards every output plays again.
func TestM5aRestartRestores(t *testing.T)
// PUT cam, a disabled stream off, and a third stream; stop the server (ctx); write a truncated junk.json into
// <data_dir>/streams; start a second server on the same data_dir: GET /api/v1/streams?secrets=1 equals the list before the
// stop; cam plays over HLS; off is "stopped"; junk.json is untouched and counted in openstreamer_store_load_errors.
func TestM5aLadder(t *testing.T)
// needs testpeer.OSTranscoder. cam with rungs 360 (x264, 360 p, 800 kbps) and 180 (180 p, 300 kbps), AAC audio, a thumbnail
// every 2 s, every output: /live/cam/hls/index.m3u8, cmaf/master.m3u8 and cmaf/index.mpd pass conform with
// AlignedSequences and list 2 video variants; /live/cam_360/… and /live/cam_180/… are single-rendition streams on HLS and
// RTSP; RTMP live/cam carries 360-line video (rendition 0); /live/cam_abr/… is 404; the API's thumbnail.jpg is a JPEG and
// /live/cam/thumbnail.jpg is 404; /metrics has openstreamer_transcoder_encoder_fallbacks_total{stream="cam"} 0 and
// openstreamer_transcoder_up{stream="cam"} 1. Then PUT with dash off → "outputs": status.transcoder.pid and spawns unchanged,
// and the HLS reader running across it passes assertUninterrupted.
func TestM5aPeers(t *testing.T)
// needs ffprobe with the protocols (testpeer.Require): ffprobe -show_streams on each of cam's seven URLs reports one h264 and
// one aac stream. Runs on the host and in make test-ci-peers.
func TestM5aNoSecretsLeak(t *testing.T)
// cam2 with inputs rtsp://user:hunter2-pw@127.0.0.1:1/x (refused at once) then the file, and a push to
// rtmp://127.0.0.1:1/live/pushkey-0123456789 (refused). After 5 s: none of "hunter2-pw", "pushkey-0123456789" and the API
// token appears in the log buffer, in /metrics, in GET cam2, in the list, in cam2/status (whose last_error and
// pushes[0].last_error are not empty), or in a 422 answer to a PUT with a bad URL carrying the password. Every log line is one
// JSON object with time, level and msg. GET cam2?secrets=1 does return both secrets.
func TestM5aAuth(t *testing.T) // without the token every /api/, /metrics and /debug/pprof/ URL is 401 and /healthz is 200; the media listener serves /live/ without a token and has no /api/ route (404)
```

- [ ] **Step 2: Run** `go test -race -count=1 -timeout=10m -run '^TestM5a' ./internal/e2e/` on the host — pass (`TestM5aLadder` needs `OS_TRANSCODER`: run `make ostranscoder` first and set it to `dist/ostranscoder-amd64/ostranscoder`).
- [ ] **Step 3: Run** `go test -race -count=5 -run '^TestM5a(EditOutputs|EveryOutput)$' ./internal/e2e/` — 5 of 5; then the whole suite, `make test`, beside it once, to see the scenarios under load.
- [ ] **Step 4: Run** `make test-ci-peers ARGS='-run ^TestM5a ./internal/e2e/'` — pass in `osv2-ci-peers:m4a`.
- [ ] **Step 5: Commit** `test(e2e): the M5a scenarios`

### Task 12: CI, docs, spec, done

**Files:**
- Modify: `.github/workflows/ci.yml`, `.testcoverage.yml`, `README.md`, `CLAUDE.md`, `docs/REBUILD_PLAN.md`

- [ ] **Step 1: CI.** Add `TestM5aLadder` to `E2E_TESTS` of the `transcode-e2e-arm64` job (its "Check the tests exist" step then guards the name). The amd64 peers job runs `./internal/e2e/` in full already; check that its run lists `TestM5aEveryOutput`, `TestM5aEditOutputs`, `TestM5aPeers` and `TestM5aLadder` as run, not skipped. Add a step to the untagged test job: `go build -o /dev/null ./cmd/openstreamer` on `GOOS=linux`, `windows` and `darwin`.
- [ ] **Step 2: Coverage.** Run `make cover && make covercheck`. Exclude `^internal/metrics/metricstest` with the comment "test-helper package: it is exercised by the internal/metrics, internal/stream, internal/api and internal/e2e tests, which go test does not count towards it without -coverpkg". No other exclusion: `cmd/openstreamer` is covered through `run`.
- [ ] **Step 3: README and CLAUDE.md.** README: a section "Running the server" (`make openstreamer`, the example config, `OPENSTREAMER_API_TOKEN`, one `curl -X PUT` creating a stream with every output, the play URLs by output, `/metrics` and the log format). CLAUDE.md (the rebuild branch's): the binary, the config keys, the package list of this plan, and the two routes of an edit (R-D6).
- [ ] **Step 4: `docs/REBUILD_PLAN.md`.**
  - §5's table: replace the M5 row with the three rows below, and change M6's "Depends on" to `M5a`.

    | | Milestone | Depends on | Exit criteria |
    |---|---|---|---|
    | M5a ([plan](superpowers/plans/2026-11-26-m5a-control-plane-core.md), done) | Control-plane core: the stream document (`internal/spec`), the file store, the per-stream supervisor with in-place output edits, the manager, the REST API with bearer auth and its OpenAPI document, Prometheus metrics (with `EncoderFallbacks`), redacted JSON logs, `cmd/openstreamer` and its config, and the product names of a ladder (R-D3) | M2, M3, M4 | A spec created over the API serves every output; editing outputs does not interrupt other outputs; a restart restores every stream. Proved by `TestM5aEveryOutput`, `TestM5aEditOutputs` (each output switched off and on while a reader of every other output must hold every hub frame once), `TestM5aRebuildIsTheOtherRoute`, `TestM5aRestartRestores`, `TestM5aLadder`, `TestM5aPeers`, `TestM5aNoSecretsLeak` and `TestM5aAuth`, with the spec, store, stream, api, metrics, logging and server suites |
    | M5b | Live edits and assets: a running pipeline's inputs changed in place, the watermark edited in place over the API, the asset API (upload, listing, replacement; R-C8) with operator fonts (R-C7), a rung that refuses the x264 fallback (wire version 3), audio language in the spec, the HLS masters and the MPD | M5a | Replacing a running stream's inputs is exactly one epoch change and ends no output session; an uploaded image and font appear on every rung; a strict NVENC rung fails instead of falling back and is counted; `osconform` reads the languages |
    | M5c | Transport security: HTTPS for the API and the HTTP outputs, RTMPS (listen, pull, push), RTSPS and SRTP, SRT encryption (input and output), credentials in URL paths and queries redacted, secrets as write-only spec fields | M5a | ffmpeg publishes and plays over each secured protocol against our server; no secret appears in a response, a log or a metric |

  - After the M4 paragraphs of §5, add "M5 runs in three parts" with R-D1's text, the rulings R-D2 to R-D12 by reference to this plan, and an "Out of M5a" table: inputs in place, the live watermark edit, the asset API, operator fonts and refusing the fallback (M5b); audio language (M5b); every TLS and encryption item and URL path or query credentials (M5c); PATCH and per-field edits (R-D7); histograms and per-stream session counts (R-D9; M6's sessions); a config reload without a restart (R-D2); roles and token rotation (R-D8).
  - §3.6: rewrite the four bullets to what was built: the spec and the store's file layout (R-D4); the supervisor's mailbox, `View`, restart policy and the absence of a reconciler (R-D5); "Changing the output set is applied in place; changing inputs is applied in place from M5b; any other change rebuilds the stream's pipeline" (R-D6); the API on `net/http.ServeMux` with the OpenAPI document as JSON, checked by `TestOpenAPIMatchesRoutes` and `TestOpenAPIMatchesSpec` (R-D7).
  - §3.7: in the library table replace the row `HTTP | go-chi/chi/v5` with `HTTP | net/http (ServeMux patterns); no router library (R-D7)` and add `Metrics | our own text exposition, internal/metrics (R-D9)`.
  - §4: add `internal/spec/`, `internal/server/`, `internal/metrics/`, `internal/logging/` and `api/openapi.json`; change `internal/stream/` to "supervisor and manager".
  - §3.5's presentation bullet and the two "Out of" rows that say "product naming is M5's" (R-B2): state R-D3's names and that osdev keeps `<name>_abr`.
  - The "Out of" rows that name M5: `Metrics for EncoderFallbacks` becomes done (M5a); the asset API, operator fonts, inputs in place and "several audio languages" name M5b; RTMPS, RTSPS and SRTP, SRT encryption and "credentials in the URL path or query" name M5c.
  - Backlog: in "A fallback to libx264 can overload a host sized for NVENC", record that the count is exported (M5a) and refusing is M5b. Add: "A switch that stops serving a ladder's renditions as their own streams (R-D3; saves N packagers)"; "HTTP-TS has no session counters, so `openstreamer_output_sessions` lacks it"; "A panic in one stream's goroutines takes the process down (R-D5): decide on per-stream recovery after the soak"; "`openstreamer.example.json` is JSON without comments (R-D2)". Add an "M5a deferred minors" heading for the review's leftovers.
- [ ] **Step 5: Run** the gates: `go mod tidy` (no change), `go vet ./...`, `golangci-lint run ./...`, `make test`, `GOOS=windows go vet ./...`, `GOOS=darwin go vet ./...`, `make test-transcode`, `make test-ci-peers` — all pass.
- [ ] **Step 6: Commit** `ci: run the M5a ladder scenario on arm64 and build openstreamer for three systems` and `docs: M5a is done; M5 is split into M5a, M5b and M5c`
