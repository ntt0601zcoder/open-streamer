# M5a: Control-Plane Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A real server binary, `openstreamer`, configured by **one YAML file** that holds the server settings, the users and the streams; the streams are declarative specs edited over a REST API behind HTTP Basic auth with two roles (as one whole-config document or one stream at a time) or by hand and a reload; each stream is run by its own supervisor, which applies an edit of the output set in place, so the other outputs of the stream keep their sessions; the server exports Prometheus metrics and writes redacted JSON logs.

**Architecture:**
- **`internal/spec`** is the stream document: a pure data package with structural validation, a redacted copy, an ETag and `Diff`, which classifies an edit as *none*, *outputs* or *rebuild*.
- **`internal/config`** is the config file: the YAML document (`server`, `users`, `streams`), its strict parser, the two node-tree edits that change one stream and keep the comments, and the file itself, written by temp file, fsync and rename, with a content hash that detects a hand edit. **`internal/auth`** is the users: PBKDF2 password hashes, the two roles, and the failed-login limiter.
- **`internal/stream`** has the `Supervisor` (one goroutine per stream; it owns the stream's hubs, pipeline, transcoder, packagers and pushers, and takes edits as messages) and the `Manager` (the set of supervisors, the cross-stream checks, `Replace`, which turns a whole new stream set into the smallest per-stream edits, and the lookups the output servers use).
- **Output edits are in place.** The session outputs (HTTP-TS, RTMP, RTSP, SRT) are process-wide servers that find a stream through a lookup. A supervisor publishes an immutable `View` (which names are served by which output) through an atomic pointer; disabling an output swaps the view and tells the stream's hubs to refuse that output's subscribers (`hub.Refuse`), which ends exactly its sessions. Packagers and pushers are per stream and are started or stopped one by one. Nothing else is touched, so nothing else is interrupted.
- **One apply path.** A whole-config `PUT`, a per-stream `PUT` or `DELETE`, and a reload after a hand edit (SIGHUP) all end in the same function of `internal/server`: parse and validate the whole document, write the file when the change came over the API, `Manager.Replace`, swap the users. An unchanged stream is never touched.
- **`internal/api`**, **`internal/metrics`**, **`internal/logging`** and **`internal/server`** are the REST handler, a small Prometheus text writer, the `slog` set-up with a scrubbing handler, and the wiring that `cmd/openstreamer` runs.

**Tech Stack:** Go 1.26.11. Standard library for everything new but YAML: `net/http` (`ServeMux` method and wildcard patterns), `encoding/json`, `log/slog`, `crypto/pbkdf2` (in the standard library since Go 1.24), `crypto/sha256`, `crypto/hmac`, `crypto/subtle`, `net/http/pprof`, `runtime/metrics`. YAML: `go.yaml.in/yaml/v3` v3.0.5. Existing: the v2 packages `hub`, `pipeline`, `ingest`, `transcode`, `output/*`, `rtmpsrv`, `redact`, `conform`, `testpeer`; testify; peers ffmpeg/ffprobe.

**Spec:** `docs/REBUILD_PLAN.md` (§2 rows "Control" and "Operations", §3.6 Control plane, §3.7 Libraries, §4 layout, §5 row M5, the "M5" lines of the Out-of tables, and the Backlog items naming M5). Format exemplars: `docs/superpowers/plans/2026-11-25-m4c-picture-features.md`, `docs/superpowers/plans/2026-11-25-m4b-abr-outputs.md`. `cmd/osdev/main.go` is the reference for how the v2 packages are wired and shut down; it stays as it is.

## Global Constraints

- **Libraries:** **one module becomes a direct dependency, and nothing is downloaded:** `go.yaml.in/yaml/v3` v3.0.5 (MIT and Apache-2.0), which `go.mod` already lists as indirect (testify needs it); Task 6 moves it to the direct `require` block, at that version (controller ruling, user decision 4). Nothing else is added: only the approved independent libraries (mp4ff, pion/rtp, pion/sdp, datarhei/gosrt, testify, go-astiav v0.41.0, protobuf `protowire`); no bluenviron, no lal, no code copied from another product. v1's code on `main` (`internal/store`, `internal/api`, `internal/coordinator`, `internal/runtime`) may be read for ideas and is never copied. The router is `net/http`'s `ServeMux`, not chi (R-D7); metrics are a hand-written text exposition, not `prometheus/client_golang` (R-D9); password hashing is the standard library's `crypto/pbkdf2` (R-D8); the terminal handling of `hash-password` uses `syscall`, not `golang.org/x/term` (R-D8). A task that finds it needs another module stops and reports.
- **cgo:** only `internal/transcode/native` and `cmd/ostranscoder` use cgo, behind the tag `libav`. Without the tag, `go build ./...`, `go vet ./...`, `golangci-lint run ./...` and `make test` pass, as do `GOOS=windows go vet ./...` and `GOOS=darwin go vet ./...`. Every new package is pure Go; anything Unix-only (directory fsync, `/proc`) sits behind `_unix.go`/`_linux.go` and `_other.go` files.
- **Frozen behaviour:** every existing pipeline, output, transcoder and osdev behaviour is what M4c left, and every existing test passes unchanged except the three Task 1 names. The only edits to existing non-test code are additive: `hub.Refuse` and `transcode.ThumbnailerOptions.OnEvent` (Task 5), `redact.Masked` (Task 2) and the watermark sweep's regex (Task 1). `rtmpsrv` is not changed: `AddrKey` rejects port 0 and osdev and ingest depend on that. A hub on which `Refuse` was never called behaves exactly as before.
- **Time:** 90 kHz everywhere. The control plane never touches a timestamp.
- **Lint:** `exhaustive` with `default-signifies-exhaustive: false` (a `switch` on `spec.Change`, `stream.Output` or a state lists every value); `godot`; gofumpt.
- **Errors:** wrap as `fmt.Errorf("pkg: op: %w", err)`; never both log and return.
- **Goroutines:** every spawned goroutine has an explicit cancel path, and the function that spawns it waits for it before returning.
- **Security:** every `/api/`, `/metrics` and `/debug/pprof/` request is authenticated with HTTP Basic auth against the users of the config file and authorised by role (R-D8); passwords exist in the file only as PBKDF2 hashes and are never logged, echoed or taken from argv. Request bodies are bounded (1 MiB for a stream, 4 MiB for a config) and decoded strictly. The config file must be mode 0600 on Unix (it holds stream keys and password hashes). A stream name must pass `spec.ValidName` (`^[a-z0-9][a-z0-9-]{0,62}$`, and not a Windows device name). No secret (URL password, secret query value, RTMP stream key, a user's password or its hash) reaches a log line, a metric, a status document or an error message: URLs pass through `redact.URL` before they leave the process, and the log handler scrubs what slips through (R-D10). The two deliberate exceptions return stored secrets to an **editor** only: `GET /api/v1/config` and `?secrets=1`.
- **Peers:** every test that runs ffmpeg/ffprobe passes on the host (ffmpeg 8) and in `make test-ci-peers` (image `osv2-ci-peers:m4a`: ffmpeg 6.1, VLC 3.0.20, GStreamer 1.24) before any push. The transcoder bundle is built with `make ostranscoder`; NVENC device tests stay opt-in (`OS_NVENC=1`).
- **Soak safety (absolute):** never stop, restart or reconfigure open-streamer-soak-mediamtx, caddy, grafana, osdev-console-1, vector, alloy, pyroscope, prometheus, node-exporter, nvidia-gpu-exporter, or any other container not named `osv2-*`. Never bind ports 554, 1935 or 8554: every test uses ephemeral ports and reads them back from `server.Addrs` (`127.0.0.1:0` for every listener; `server.New` turns a port-0 RTMP address into a concrete free port itself, because `rtmpsrv` cannot bind port 0, R-D13), and no test or step runs the server with the example config. Own containers are `osv2-*`, `--rm`, stopped afterwards.
- **Commits:** end every message with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. After each wave merge, run `go mod tidy` and check that it changes nothing (after Task 6: nothing but `go.yaml.in/yaml/v3` having moved to the direct block).

## Rulings

| ID | Ruling | Cost if wrong |
|---|---|---|
| R-D1 | **M5 is split in three.** **M5a (this plan)** is the control-plane core and delivers the M5 row's exit criterion: spec, the one config file with its users, supervisor, manager, API with its config editor, metrics (with `EncoderFallbacks`), logs, `cmd/openstreamer`, and the product names of a ladder. Done when a spec created over the API serves every output, an output edit interrupts no other output, and a restart restores every stream (Task 11's tests). **M5b, live edits and assets:** a running pipeline's inputs changed in place (`Pipeline.SetInputs`, §3.6), the watermark edited in place over the API (`Transcoder.SetWatermark` exists), the asset API (R-C8) with operator fonts (R-C7), a rung that refuses the x264 fallback (wire version 3), and audio language in the spec, the HLS masters and the MPD. Done when replacing a running stream's inputs is exactly one epoch change and ends no output session, an uploaded image and font appear on every rung, a strict NVENC rung fails instead of falling back and is counted, and `osconform` reads the languages. **M5c, transport security:** HTTPS for the API and the HTTP outputs, RTMPS (listen, pull, push), RTSPS with SRTP by its own ruling, SRT encryption (input and output), redaction of credentials in URL paths and queries (the M2c note), and secrets as write-only spec fields. Done when ffmpeg publishes and plays over each secured protocol against our server, and no secret appears in a response, log or metric. M6 and the M7 soak need only M5a; M5b and M5c may run beside M6 | Three plans and three reviews instead of one. Until M5b an input edit rebuilds the stream (R-D6), and assets are files the operator copies into a directory. Until M5c the Basic credentials (base64, not encrypted) and every stream key cross the network in clear, so the API listens on loopback by default (R-D8) |
| R-D2 | **One binary, one config file** (user decisions 3 and 4). The binary is `cmd/openstreamer` (§4); `cmd/osdev` stays the development harness. **All configuration is one YAML file**, `-config path`, with a `version` and three sections: `server` (the listen addresses and apps of the shared servers, the transcoder binary, the assets directory, logging; keys and defaults in Task 6), `users` (R-D8) and `streams` (a list of stream documents, R-D3). There is no data directory and no separate store. The file is decoded strictly (`KnownFields`: an unknown key is an error), is at most 4 MiB, and on Unix must be mode 0600, checked at start and at every reload. **The rule for formats:** the config document is YAML, in the file and on `GET`/`PUT /api/v1/config`; every other request and response body is JSON, and a stream has the same keys in both (the `yaml` and `json` tags of `internal/spec` are equal, by a test). The library is `go.yaml.in/yaml/v3`, already in `go.mod` as an indirect dependency. Rejected: JSON for the file (no comments, which decision 4 requires to survive) | The service user must be able to write the file **and its directory** (the rename), so the file lives in a directory the service owns, not a root-owned `/etc`. A change in `server` needs a restart (R-D4). YAML's looser typing (an unquoted `no`, a leading-zero number) is the operator's to quote; strict decoding into typed fields catches most of it |
| R-D3 | **Product names (closes R-B2).** A stream name and a rendition label each match `^[a-z0-9][a-z0-9-]{0,62}$` and are none of the Windows device names (`con`, `prn`, `aux`, `nul`, `com1`…`com9`, `lpt1`…`lpt9`), so `_` separates them without ambiguity and a name is a safe file name on every system the server builds for. A stream **without** a ladder is served as `<name>` on every output. A stream **with** a ladder is served as `<name>`: over HLS and DASH that is the ABR presentation, over HTTP-TS, RTMP, RTSP, SRT and RTMP push it is rendition 0, the first in the spec; and each rendition is also its own stream `<name>_<label>` on every enabled output. **The source of a laddered stream is not served to players** (with a watermark it is the unbranded picture, R-C5); `copy://<name>` reads it, and `copy://<name>_<label>` reads a rendition. There is no `<name>_abr` in the product and `abr` is an ordinary label; osdev keeps its names. The thumbnail is an operator preview of the source, so it is served by the API, behind its authentication, not under `/live/` | An operator who wants the raw source out next to a ladder adds a `copy` rendition (not possible with a watermark, by R-C5). A ladder of N rungs runs N + 1 packagers when HLS or DASH is on, as osdev does; a switch that stops serving the renditions on their own is a Backlog item |
| R-D4 | **How the file is loaded, written and reloaded** (user decisions 3 and 4). **Write:** every API edit writes the whole file: a temp file `.<base>.<random>.tmp` in the same directory with mode 0600, write, `fsync`, rename over the target, `fsync` the directory (Unix); a crash leaves the old file or the new one, never a partial one, and a leftover temp file is removed at the next start. A whole-config `PUT` writes **the exact bytes the client sent**, so comments and formatting survive; a per-stream `PUT` or `DELETE` edits the YAML node tree of the current text and writes the result: the comment lines above keys and entries survive, but the library re-emits the document, so blank lines are dropped, sequences re-indented to 2 spaces, the spacing of line comments normalised, and the comments inside the edited stream (and a line comment on its `name:`) lost. An operator who cares about the file's layout edits it whole. **Hand edits** are applied by a reload: SIGHUP, or `POST /api/v1/reload` (the same function; Windows has no SIGHUP). **Stale detection:** the server remembers the SHA-256 of the text it last loaded, reloaded or wrote; an API write finds the file's hash different and is refused with 409 until a reload, so neither side silently overwrites the other. **Validity.** A reload and every API write are all or nothing on the *document*: YAML syntax, the `version`, `server`, `users`, every stream's `Validate`, and duplicate names; any error is reported (all of them, not the first), nothing is applied and nothing is written. An invalid file at reload keeps the running state and is reported in the log, in `GET /api/v1/config/status` and in `openstreamer_config_reloads_total{result="error"}`. **Only the start is lenient about streams**, so one bad stream cannot keep every other stream offline after a reboot: a stream entry that does not decode, fails `Validate` or repeats a name is skipped and reported; errors in `version`, `server` or `users` stop the start. While the file holds such an entry, API writes and reloads are refused with those errors until it is fixed: by hand, with a `PUT /api/v1/config`, or with a per-stream `PUT` or `DELETE` of that entry's own name, which replaces or removes it. Anchors, aliases and merge keys are refused everywhere, and numbers must be plain decimal integers (Task 6). **`server` changes are not applied while running:** a reload or `PUT` whose `server` section differs from the one the process started with applies users and streams and lists the changed keys as `restart_required` (in the response, the log and the status). Migrations: `version` is the only switch; version 1 is the only one accepted. Rejected: a store beside the config (two sources of truth), and merging hand edits with API edits (no safe merge of two texts) | Every API edit rewrites the whole file, which is small. The window between the stale check and the rename is not closed: a hand edit saved in those microseconds is lost. If the directory `fsync` fails after the rename, the new text is on disk but was not applied: the API answers 409 until a reload. The file's symlinks are resolved once at start, and the rename needs the directory: a container must mount the directory, not the single file (a bind-mounted file cannot be renamed over: `EBUSY`). A stream that is skipped at start is absent until someone reads the log, the status or `openstreamer_config_load_errors`. No history: an overwritten config is gone |
| R-D5 | **Supervisor (§3.6).** One goroutine per enabled stream owns everything of that stream and takes edits from a mailbox (`Submit`), in order; readers (the output servers' lookups, the API's status, the metrics) see only an immutable `View` and a `Status` behind atomic pointers. There is no lock shared between streams, and **no periodic reconciler**: a supervisor exists for exactly the enabled specs in the manager's map, by construction, and its loop ends only with its context. **Restart policy:** inside a generation, a failed input is the pipeline's business (retry and failover, M2a) and a dead child the transcoder's (respawn, M4a). The supervisor rebuilds the whole generation only when one of its long-lived parts returns before the context ends (the pipeline's `Run`, the transcoder's `Run`, for example on a child of another wire version, or the thumbnailer's `Run`), or when building it fails (an asset that was there at validation and is gone now): it tears the generation down, waits 1 s, doubling up to 30 s, back to 1 s once a generation has lived 60 s, and builds again, forever. An edit during the wait is applied at once. **Two supervisors of one name never overlap:** a supervisor started for a name waits, before it builds, for the previous supervisor of that name to have stopped completely (its pipeline's `Run` returned, so its listen sockets and RTMP publish routes are released). A stream never ends: `EndOnEOF` is false, so a file input without `loop` restarts at its end as a new epoch. A panic is not recovered | A generation that fails at once is rebuilt every 30 s for as long as the spec stands; `state: backoff`, `last_error` and `openstreamer_stream_restarts_total{reason="failure"}` are the signals. A rebuild, for an edit or a failure, starts a new packager, so the HLS `MEDIA-SEQUENCE` and the DASH timeline of the same URL start again (Backlog). A bug that panics in one stream takes the process, and every stream, down; the service manager restarts it and the config file brings every stream back |
| R-D6 | **How an edit is applied.** `spec.Diff(old, new)` is `ChangeNone` when the documents are equal, `ChangeOutputs` when they differ only in `outputs`, and `ChangeRebuild` otherwise. **Outputs, in place** (Task 7's `reattach`): packagers are started when the first of HLS and DASH is enabled and stopped when the last is disabled, and left alone otherwise; each of HTTP-TS, RTMP, RTSP and SRT is a flag in the view, and disabling one makes each served hub refuse that output's subscribers, which ends its sessions and no others; pushes are keyed by URL, so an added target starts, a removed one stops, and an unchanged one keeps its connection. The source hub, the pipeline, the transcoder and its child are not touched. **Rebuild:** the generation is torn down and built from the new spec at once (no backoff); every session of the stream ends and HLS media sequence numbers start again. **In M5a an input edit is a rebuild**; §3.6's "changing inputs … is applied in place" is M5b's. `disabled` stops and removes the supervisor, or starts one. Rejected: v1's field-by-field diff routing (the source of most of v1's lifecycle findings); here there are two routes | Any edit outside `outputs` (a rung's bitrate, a thumbnail interval, one more failover input) interrupts every viewer of that stream for a few seconds. Disabling HLS while DASH stays on stops serving the HLS paths but keeps the packager's work |
| R-D7 | **API** (user decision 4 for the config editor). Base path `/api/v1`. **The config editor:** `GET /api/v1/config` returns the file text as `application/yaml`, verbatim (stream secrets and password hashes included), with an `ETag` that is the content hash; editors only. `PUT /api/v1/config` (editors, `Content-Type: application/yaml`, **`If-Match` required**) validates the whole document, applies it through the one apply path (R-D4) and writes the exact bytes; it answers with a per-stream outcome (`unchanged`, `outputs`, `rebuild`, `created`, `started`, `stopped`, `deleted`) and `restart_required`. A stale `If-Match` is 412, a missing one 428, a file changed on disk 409, an unparsable body 400, an invalid document 422 with every error. **Per-stream endpoints are thin wrappers** over the same path: `PUT /streams/{name}` (a JSON stream document, idempotent, creates or replaces; no PATCH, no POST) and `DELETE /streams/{name}` edit the YAML node tree and go the same way; they honour an optional `If-Match` on the stream's own `ETag`. Reads: `GET /streams`, `GET /streams/{name}`, `GET /streams/{name}/status`, `GET /streams/{name}/thumbnail.jpg`, `GET /config/status`, `GET /version`, `GET /openapi.json`; `POST /reload`; outside the base path `/healthz`, `/metrics` and `/debug/pprof/`. **Stream URLs are redacted in every JSON response** unless an editor asks with `?secrets=1` (for an edit round trip); a per-stream `PUT` whose URL carries the redaction mask where a secret would be is refused, so a redacted document cannot be written back by mistake. Users have no endpoints of their own: they change only as part of the whole config (`PUT /api/v1/config`, or a hand edit and reload), and a document without an editor is refused. The router is `net/http.ServeMux`, not chi. The OpenAPI 3.1 document is hand-written as JSON, embedded and served, and a test keeps it equal to the registered routes and the spec's fields. Task 12 edits §3.6 and §3.7 to say so | Two ways to edit, which must agree: one apply path is what keeps them from diverging. A per-stream edit loses that stream's comments. An editor can lock every other editor out, or change a password hash, with one `PUT`; that is the role. No middleware library: auth, the body limit and the request counter are hand-written wrappers |
| R-D8 | **Auth** (user decisions 1 and 2). HTTP Basic auth against the `users` of the config file, each `{name, role, password}`. **Two roles:** `viewer` may do every read (streams with URLs redacted, status, thumbnail, config status, version, the OpenAPI document, `/metrics`); `editor` may do everything a viewer can, plus every write (`PUT`, `DELETE`, `POST /reload`), plus what returns stored secrets or process internals (`GET /api/v1/config`, `?secrets=1`, `/debug/pprof/`). `/healthz` is open. **The password field is a hash, never plaintext:** `pbkdf2-sha256$<iterations>$<salt>$<key>`, salt and key in unpadded standard base64, PBKDF2-HMAC-SHA-256 from `crypto/pbkdf2`, a random 16-byte salt per user, a 32-byte key, **600000 iterations** (OWASP's 2023 figure; about 0.2 s of one core per verification); the prefix versions the format. The config loader refuses a password that is not in that format (so plaintext cannot be configured), fewer than 600000 or more than 10000000 iterations, an unknown role, a duplicate or malformed name, more than 64 users, and a config with no editor; its errors never quote the field. `openstreamer hash-password` makes the hash: it reads the password from stdin, never from argv; on a terminal it turns echo off and asks twice; it refuses fewer than 12 bytes. **Verification:** in constant time; a request for an unknown user is verified against a dummy hash (made once per process, at 600000 iterations), so timing does not reveal names. Because Basic auth sends the password on every request, a successful verification is remembered in the user set: HMAC-SHA-256, under a per-process random key, of name and password, mapped to the role, at most one entry per user, kept across config edits that leave the users section unchanged and dropped when it changes; a failed one is never remembered. At most 2 verifications run at once and at most 16 wait (beyond that: 503 with `Retry-After`); a request that goes away gives up its place. **Failed-login backoff, per client address and presented user name** (controller ruling; the connection's remote address, an IPv6 address by its /64, no forwarded header trusted; the pair is kept only as an HMAC): a login takes one of the pair's 5 places before it is verified, so 200 simultaneous wrong logins run at most 5 verifications; after 5 failures the pair is refused with 429 and `Retry-After`, before anything is compared (not even against a remembered login), for 1 s, doubling with each further failure up to 5 min; a success clears it; an entry idle for 15 min is dropped; at most 4096 pairs are tracked and the one that failed longest ago is dropped first. **Every `PUT`, `DELETE` and `POST` must carry an `X-Requested-By` header** (any value): a browser that has cached the Basic credentials would otherwise send them with a cross-site form or a bodyless `POST /api/v1/reload`, which need no preflight; a custom header does. A 401 carries `WWW-Authenticate: Basic realm="openstreamer"`; an authenticated user without the role gets 403. The API has its own listener, default `127.0.0.1:8081`, apart from the media listener (`/live/`, default `:8080`, no auth: playback tokens are M6). **Basic credentials are base64, not encrypted: until M5c's HTTPS the API listener stays on loopback by default.** The soak's Prometheus scrapes `/metrics` with `basic_auth` as a viewer | The first request of each user after a start, or after an edit that changes the users, costs 0.2 s of CPU, and every wrong password costs that again, which the limiter and the cap of 2 bound. Because the key includes the presented name, a client can try 5 passwords for each name it invents, each costing a dummy verification: that cannot find a password of a real user faster, but it can keep both verification slots busy, and then a real user's *first* login waits or gets 503 (a remembered login is not affected). Wrong passwords for one user from one address lock out that pair, the right password included, for up to 5 min; behind a reverse proxy, or on loopback, every client of that user shares the address. With 4096 pairs tracked, an attacker with more pairs resets its own counters. A remembered login clears its pair's failures, so on a shared address (loopback, a proxy) an attacker guessing a user's password gets 5 fresh guesses each time the real client of that user makes a request: against a 12-byte password that changes nothing. `curl` users must add `-H 'X-Requested-By: curl'` to writes. An editor is a full operator: it can point a stream at any URL or local media file the server can read. No rotation other than editing the file |
| R-D9 | **Metrics.** `internal/metrics` writes the Prometheus text format 0.0.4 itself: counters and gauges, collected at scrape time from the `Stats` the packages already keep. No histograms. Every name starts `openstreamer_`, except the conventional `go_*` and `process_*` (so stock dashboards and the soak's leak panels work). **Labels come only from configuration:** `stream` (a spec name), `name` (a served name), `rendition` (a label), `output`, `reason`, `state`, `kind`, `result`, `target` (the first 8 hex digits of the SHA-256 of a push's URL: stable when another push is removed, and never the URL), and for the API `method` (one of `GET`, `PUT`, `DELETE`, `POST`, `HEAD`, `OPTIONS`, or `other` for anything else a client sends), `route` (the mux pattern, never the path; `unmatched` when none matches) and `code`. Nothing a client sends becomes a label value. With S streams of at most 8 rungs and 8 pushes the stream series are bounded by about 60 × S. Counters that come from a generation's objects start again at 0 when the stream is rebuilt, which Prometheus treats as a counter reset. `EncoderFallbacks` is `openstreamer_transcoder_encoder_fallbacks_total{stream}` (closes the Backlog's export; refusing the fallback is M5b). The catalogue is in Task 7 and Task 10. Rejected: `prometheus/client_golang` (Apache-2.0): about ten transitive modules for a job that is two hundred lines when every value is already a snapshot | We own the exposition code and its escaping. No latency histograms; if they are wanted, switching to `client_golang` is a contained change behind `metrics.Writer`. Per-stream session counts are not exported (the output servers count per server, not per stream): M6's sessions |
| R-D10 | **Logs.** `log/slog`, JSON on stderr by default (`text` for a terminal), level `info` by default. Attributes: `stream`, `component`, `input`, `err`. The API logs one line per request at `info`: method, route pattern, status, duration, remote address; never a header, a query or a body. Media requests are not logged per request. **Redaction is two layers:** code that logs a URL passes it through `redact.URL`, and the handler from `logging.New` scrubs every message, string attribute and error text: each URL-shaped substring (`scheme://…`) goes through `redact.URL`, and an attribute named `token`, `password`, `passphrase`, `secret` or `authorization` (any case) is replaced by the mask | A regular expression runs over every logged string, which is fine at log rates and would not be on a per-frame path (nothing logs per frame). A secret that is not URL-shaped and not under one of those keys is not caught by the second layer |
| R-D11 | **Validation happens before anything is stored or started,** in two steps: `spec.Stream.Validate` (structure, names, limits; no I/O) and `stream.Check` (what needs the v2 packages or the server's config: `ingest.Parse` on every input, a dry `transcode.New` and `rtmppush.New` on scratch hubs, the asset read, the transcoder binary being configured). The manager then checks `copy://` cycles and **listen claims**, for the streams an edit changes or adds, against the server's listeners and the streams that hold their claims (a stream holds its claims while it has a supervisor: a disabled stream and one left stopped at load hold none, and are judged when they are next edited), under one rule for TCP and UDP: two claims overlap when they have the same protocol family and port and the same host, or a wildcard host on either side. The rule is seeded with the server's own bound listeners (HTTP, API, RTMP, RTSP on TCP; SRT on UDP), runs between the inputs of one spec as well as between streams, and runs over the file's streams in name order at load, where the stream that comes second is left stopped with the reason. Two exceptions: RTMP listen inputs on the *same* address share one listener (and the server's RTMP play listener) and collide only on an equal publish path; and a UDP input on a multicast group claims nothing (several streams may read one group). After that the config file is written, and only then is the supervisor told; a supervisor is never given a spec that failed a check, so building a generation cannot fail on the spec. An unreachable source is **not** a validation error: the stream is created and waits | A check the dry run misses surfaces as a stream in `backoff` rather than a 422. Listen overlaps the claims do not see, each of which ends as a bind error the pipeline retries and shows in `last_error`: a multicast input against a unicast or wildcard socket on the same port; host names (`localhost` and `127.0.0.1` are different hosts to the rule); the RTSP server's UDP port pair, chosen at start; and the moment an address passes from one stream to another in one edit, when the new owner may try to bind before the old one has let go. Two multicast inputs that differ only in a source filter are not told apart. `copy://` of a stream that does not exist yet is accepted, and the copying stream waits for it |
| R-D12 | **Proof.** In-process end-to-end tests in `internal/e2e` start `server.New` on ephemeral ports, from a config file in a temp directory, and drive it through HTTP as a client would: `TestM5aEveryOutput`, `TestM5aEditOutputs` (per-stream `PUT`s), `TestM5aConfigPut` (the same proof through `PUT /api/v1/config`, with the comments surviving), `TestM5aReload` (a hand edit and a reload), `TestM5aHandEditThenWrite`, `TestM5aRestartRestores`, `TestM5aLadder`, `TestM5aPeers`, `TestM5aNoSecretsLeak`, `TestM5aRoles` (Task 11). "Not interrupted" is measured, not inferred, by the rules of Task 11: a session output's reader (RTMP, RTSP, HTTP-TS, SRT) runs across the edit until the stream is deleted, so its capture and a lossless tap on the same hub end on the same frame and M3's own hub-matching helpers apply unchanged, frame for frame; a segmented output (HLS, DASH) has no session, so its rule is weaker and stated: no conform violation, no timestamp step inside an epoch other than the tap's, whole epochs complete, the same packager and one unbroken run of segment sequence numbers. The counters taken after the readers connected (`PlayersStarted`, SRT connections, push `Connects`, the generation, the transcoder's `Spawns`) must not move | The tests run a looped file input, so they prove the control plane and not ingest, which M2's and M3's matrices already cover. For HLS and DASH a lost frame in the first or last epoch of the capture is caught only as a timestamp step, not by a count |
| R-D13 | **RTMP on an ephemeral port** (controller ruling, review round 1). `rtmpsrv.AddrKey` rejects port 0 and listeners are keyed by the requested address, so `server.New` resolves an `rtmp.listen` with port 0 itself: it binds a TCP listener on that host with port 0, reads the port, closes it, and registers the play handler on the concrete address, retrying up to 5 times when the port was taken in between. `Addrs().RTMP` and `Env.RTMPPlay` carry that concrete address. The other listeners (HTTP, API, RTSP, SRT) are bound directly and report their real ports. Rejected: changing `AddrKey` (not additive: osdev and `ingest/rtmp_listen.go` use it) | A port-0 RTMP listener has the close-and-rebind race the e2e's `freeTCPPort` has; it matters only to tests, since a deployment names its port |

## Execution waves (parallel per user instruction)

| Wave | Tasks | Why this order |
|---|---|---|
| 1 | 1 (carry items), 2 (`spec`, `redact.Masked`), 3 (`metrics`), 4 (`logging`), 5 (`hub.Refuse`, the Thumbnailer's `OnEvent`), 13 (`auth`) | Disjoint files: Task 1 has `native/watermark*`, `cmd/ostranscoder/main_test.go`, `rtspplay_test.go` and one Backlog line; Tasks 2, 3, 4 and 13 are new packages (Task 2 also `internal/redact`); Task 5 has `internal/hub` and `internal/transcode/thumbnailer*.go`. Task 13 has its number because it was added in review round 2; it stands after Task 5 in this document |
| 2 | 6 (`config`), 7 (`stream.Supervisor`) | 6 needs Task 2's types and Task 13's hashes, and makes yaml a direct dependency (`go.mod`: only Task 6 touches it); 7 needs Tasks 2, 3, 4 (`logging.Scrub`) and 5. New, separate packages |
| 3 | 8 (`stream.Manager`) | Same package as 7 |
| 4 | 9 (`api`) | Needs 8's `Outcome` and `Status`, 6's error types (`config.SyntaxError`, `Errors`, `ErrStale`, `ErrNoStream`, `MaxBytes`), 13's `auth.Users` and `Limiter`, 3's `metrics.Writer` and 4's `logging.Scrub` |
| 5 | 10 (`server`, `cmd/openstreamer`) | Needs 3, 4, 6, 8, 9 and 13 |
| 6 | 11 (M5a scenarios) | Needs 10 |
| 7 | 12 (CI, docs, spec, done) | Needs 11 |

Each parallel task runs in its own worktree; the controller merges.

## Review Focus

1. **A listen address claimed twice** (the same RTMP publish path; RTMP on `:P` and on `127.0.0.1:P`; the same UDP or SRT port, or a UDP input on the SRT play port; a listen input on one of the server's own ports; two inputs of one stream on one address): the `PUT` is refused with 409, nothing is stored and the stream that holds the address keeps running untouched; at load the later stream stays stopped with the reason, and that does not block later edits of other streams. Two streams on one multicast group are accepted. Task 8, `TestManagerListenClaim` and `TestManagerLoad`.
2. **A config file that is damaged or changed behind the server's back** (a crash in the middle of a write; one stream entry with a typo at start; a hand edit that is invalid YAML or names a stream twice at reload; a hand edit followed by an API write): the file on disk is always a whole old or new version; at start every good stream runs and the bad entry is reported; an invalid reload changes nothing and is reported; and the API write is refused with 409 until a reload. Task 6, `TestWriteCrashPoints` and `TestParseLenientSkipsBadStreams`; Task 10, `TestReloadInvalidKeepsState`; Task 11, `TestM5aHandEditThenWrite`.
3. **Secrets on their way out, and a redacted document written back:** no URL password, stream key, user password or password hash appears in a JSON response to a viewer, a status, a metric or a log line, including the error of an input that fails to connect; an editor gets them only from `GET /api/v1/config` and `?secrets=1`; a redacted stream `PUT` back is refused and the stored secret survives; the file on disk keeps them verbatim. Task 9, `TestPutRejectsMaskedSecrets` and `TestRoles`; Task 11, `TestM5aNoSecretsLeak`.
4. **Edits that race:** twenty concurrent per-stream `PUT`s of one stream mixing output edits and rebuilds, with `DELETE`s and re-creations of it and a whole-config `PUT` among them: no deadlock, the runtime ends equal to the file's last version, the file is never a mix of two writers, two generations of one name never overlap, and no goroutine or subscriber is left. Task 8, `TestManagerConcurrentReplaces` and `TestManagerChainSurvivesCancelledWaiter`; Task 10, `TestConcurrentWriters`.
5. **A stream that cannot start, or stops working** (a source that is down or goes away, a wrong password, a transcoder binary that disappears after start-up or speaks another wire version, an asset deleted under a running stream): the `PUT` succeeds; the stream reports `starting`, `no_input` or `backoff` with a redacted `last_error` that names the failing part (the input, the transcoder's or the thumbnailer's spawn, the asset); other streams and the API are unaffected; it runs when the cause is gone; and deleting it during a backoff returns at once. A binary that is missing when the server starts is a config error. Task 7, `TestSupervisorDeadInput`, `TestSupervisorInputLoss`, `TestSupervisorSpawnFails`, `TestSupervisorAssetGone` and `TestSupervisorStopDuringBackoff`; Task 6, `TestParseServer`.

---

### Task 1: Carry items from M4c's final review

A hygiene task: three items the controller carried out of M4c. Items 1 and 2 touch the cgo packages and are verified with `make test-transcode`.

**Files:**
- Modify: `internal/transcode/native/watermark.go` (`wmDirName`), `internal/transcode/native/watermark_test.go` (`TestWatermarkSweepsStaleDirs`), `cmd/ostranscoder/main_test.go` (`TestProbeFlag`, `TestChildEncoderFallbacks`), `internal/output/rtspplay/rtspplay_test.go` (`TestPlayManyPlayers`), `docs/REBUILD_PLAN.md` (the Backlog item "`make test-nvenc`: shared mode and the refusal guard")

**Interfaces:** none produced.

- [ ] **Step 1: The sweep's PID.** `syscall.Kill` takes a 32-bit pid, so `ostx-wm-2147483648-1` reaches the kernel as a negative number (a process group), which answers `ESRCH`, and the sweep removes a directory it has no reason to think is dead. In `TestWatermarkSweepsStaleDirs`, add to the kept directories `mkdir("ostx-wm-2147483648-1")`, `mkdir("ostx-wm-4294967297-1")` (as a 32-bit pid it wraps to 1) and `mkdir("ostx-wm-99999999-1")` (8 digits). Run `make test-transcode ARGS='-run ^TestWatermarkSweepsStaleDirs$ ./internal/transcode/native/'`: it fails on the first and the third, which the sweep removes (`kill` answers `ESRCH` for the group −2147483648 and for a pid above `pid_max`). The second is already kept before the fix, because pid 1 exists; it stays as a row that pins the wrap.
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

Pure data: imports only the standard library and `internal/redact`. No goroutines. **Every field carries a `yaml` tag equal to its `json` tag** (`json:"bitrate_kbps,omitempty" yaml:"bitrate_kbps,omitempty"`), so a stream reads the same in the config file and in the API; the listing below shows the `json` tags only. The package does not import the YAML library: the tags are plain strings, and Task 6 decodes with them.

**Interfaces — Produces:**

```go
package spec

const (
	MaxInputs, MaxRenditions, MaxPushes = 8, 8, 8
	MaxURLBytes   = 2048
	MaxSpecBytes  = 1 << 20
	MaxStreams    = 1000 // streams in one config
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

func ValidName(s string) bool                 // ^[a-z0-9][a-z0-9-]{0,62}$ and not con, prn, aux, nul, com1…com9, lpt1…lpt9
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
// ok: "cam", "a", "cam-1", "con1", "null", 63 × "a". Not ok: "", "Cam", "cam_1", "-cam", "cam.1", "..", "a/b", "a%2f", "cam ", 64 × "a",
// and the device names "con", "prn", "aux", "nul", "com1", "com9", "lpt1", "lpt9".
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
func TestYAMLTagsEqualJSONTags(t *testing.T) // by reflection over Stream and every nested type: each field has both tags and they are equal, options included
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

The handler wraps the JSON or text handler and rewrites every record before it: the message through `Scrub`; a string attribute through `Scrub`; an `error` attribute to the `Scrub` of its `Error()`; groups recursively; `slog.LogValuer`s resolved first; an attribute whose key, lower-cased, is `token`, `password`, `passphrase`, `secret` or `authorization` becomes `xxxxx`. `WithAttrs` scrubs the attributes it is given; `WithGroup` and `Enabled` delegate. A URL-shaped substring matches `[A-Za-z][A-Za-z0-9+.-]*://\S+`: it runs to the next white space, because Go's `net/url` accepts `'`, `"` and most punctuation in the userinfo, so stopping earlier would cut a password in two and leave its tail in the log. Text glued to a URL's end without a space is therefore handed to `redact.URL` with it, which can only hide more.

- [ ] **Step 1: Write the failing tests**

```go
func TestNewLevelsAndFormats(t *testing.T) // "" → info JSON: Debug is dropped, Info is one JSON object per line with time, level, msg; "text" is not JSON; "loud" and "xml" are errors
func TestScrub(t *testing.T)
// `open rtsp://u:hunter2@cam/1 failed, then srt://h:9?passphrase=topsecret&x=1` loses "hunter2" and "topsecret" and keeps
// "cam/1" and "x=1"; `rtmp://h/live/secretkey` loses "secretkey"; `rtsp://u:pa'ss"w<d@cam/1 down` loses "pa'ss", "w<d" and
// every other piece of the password and keeps "down"; a string without "://" is returned as is.
func TestHandlerScrubs(t *testing.T)
// log.With("url", u1).Error(msgWith(u2), "err", fmt.Errorf("dial %s: refused", u3), slog.Group("g", "url", u4), "Token", "abc123", "password", "pw"):
// the output line contains none of the four passwords, nor "abc123", nor `"pw"`; it contains the hosts.
func TestHandlerKeepsStructure(t *testing.T) // ints, bools, durations and times pass through unchanged; a group keeps its name
```

- [ ] **Step 2: Run** `go test -race -count=1 ./internal/logging/` — compile failure.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the same command — pass.
- [ ] **Step 5: Commit** `feat(logging): slog set-up with a handler that scrubs URLs and secrets`

### Task 5: `hub.Refuse` and the Thumbnailer's `OnEvent`

Two additive hooks the supervisor needs. Nothing changes for a caller that does not use them.

**Files:**
- Modify: `internal/hub/hub.go`, `internal/transcode/thumbnailer.go`
- Test: `internal/hub/hub_test.go`, `internal/transcode/thumbnailer_test.go`

**Interfaces — Produces:**

```go
// Refuse replaces the set of refused subscriber-name prefixes. Every open
// subscriber whose name starts with one of them is closed, as by Sub.Close,
// and a later Subscribe, SubscribeLive or SubscribeCached under such a name
// returns a closed Sub, as on a closed hub. Refuse() refuses nothing. It
// returns the number of subscribers it closed.
func (h *Hub) Refuse(prefixes ...string) int

// In ThumbnailerOptions: OnEvent is called from the Run goroutine with
// EventSpawned (PID set) when a child starts and EventExited (Err set) when
// one exits, is killed, or cannot be started, as the Transcoder's is. Optional.
OnEvent func(Event)
```

The Thumbnailer emits `EventSpawned` after a successful start and `EventExited` in exactly two places: where a spawn fails, and in `exited()`, where every child's end arrives, killed ones included. It does **not** emit where it kills a silent child ("child sends nothing"): that child's end reaches `exited()` and would be reported twice. Its logs, counters and retries are unchanged.

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
func TestThumbnailerEvents(t *testing.T)
// a binary that does not exist: EventExited with a non-nil Err at every attempt (the fake After fires twice: two events), Stats
// as before; the fake child: EventSpawned with its PID, then exactly one EventExited when it is killed, and exactly one when it is killed
// for silence (SilenceTimeout on the fake clock). With OnEvent nil, nothing changes.
```

- [ ] **Step 2: Run** `go test -race -count=1 -run 'TestRefuse|TestThumbnailerEvents' ./internal/hub/ ./internal/transcode/` — compile failure.
- [ ] **Step 3: Implement.** `Refuse` takes `h.mu`, stores the prefixes, and closes matching subscribers with the same steps as `Sub.Close`; `subscribe` returns a closed `Sub` for a refused name before it replays the cache.
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/hub/ ./internal/transcode/` — pass, every existing test unchanged.
- [ ] **Step 5: Commit** `feat(hub): refuse subscribers by name prefix` and `feat(transcode): report the thumbnailer's child events`

### Task 13: `internal/auth` — users, password hashes and the login limiter

(Wave 1. Numbered 13 because it was added in review round 2.)

**Files:**
- Create: `internal/auth/doc.go`, `hash.go`, `users.go`, `limiter.go`
- Test: `internal/auth/hash_test.go`, `users_test.go`, `limiter_test.go`

Standard library only (`crypto/pbkdf2`, `crypto/sha256`, `crypto/hmac`, `crypto/rand`, `crypto/subtle`, `encoding/base64`, `net/netip`). No goroutines: the limiter prunes when it is called.

**Interfaces — Produces:**

```go
package auth

type Role string
const ( Viewer Role = "viewer"; Editor Role = "editor" )
func (r Role) Allows(need Role) bool // Editor allows both; Viewer allows Viewer

const (
	Prefix           = "pbkdf2-sha256"
	Iterations       = 600000 // what HashPassword uses
	MinIterations    = 600000 // what a config file must have
	MaxIterations    = 10000000
	SaltBytes        = 16
	KeyBytes         = 32
	MinPasswordBytes = 12
	MaxUsers         = 64
)

type Hash struct { Iterations int; Salt, Key []byte }
func HashPassword(password string) (string, error)   // random salt, Iterations; an error under MinPasswordBytes
func NewHash(password string, salt []byte, iterations int) Hash
func (h Hash) String() string                         // pbkdf2-sha256$<iterations>$<salt>$<key>, base64.RawStdEncoding
// ParseHash refuses anything but that format, a salt under 8 bytes, a key that is not KeyBytes long, and an iteration count
// outside [minIterations, MaxIterations]. Its errors say what is wrong and never contain s.
func ParseHash(s string, minIterations int) (Hash, error)
func (h Hash) Verify(password string) bool            // PBKDF2, then subtle.ConstantTimeCompare

type User struct { Name string; Role Role; Hash Hash }
type Users struct{ /* unexported */ }
// NewUsers refuses: a name not matching ^[a-z0-9][a-z0-9._-]{0,31}$, a duplicate name, a role other than viewer or editor,
// more than MaxUsers, and a list without an editor.
func NewUsers(list []User) (*Users, error)
func (u *Users) Len() int
func (u *Users) Equal(list []User) bool // the same names with the same role and the same hash string, in any order
// Remembered reports a login this set has already verified: no PBKDF2, constant time. It is never true for a wrong password.
func (u *Users) Remembered(name, password string) (Role, bool)
// Verify runs PBKDF2 (against the dummy hash for an unknown name) and remembers a success. At most 2 run at once and at
// most 16 wait, **in the whole process**: the slots are package-level, not per Users, so replacing the user set on a
// reload neither resets the queue nor doubles the CPU. ErrBusy beyond that. A waiter whose ctx ends leaves the queue and gets ctx.Err().
func (u *Users) Verify(ctx context.Context, name, password string) (Role, error)
var (
	ErrBadCredentials = errors.New("auth: bad credentials")
	ErrBusy           = errors.New("auth: too many logins waiting")
)

type Limiter struct{ /* unexported */ }
func NewLimiter(now func() time.Time) *Limiter // nil means time.Now
// Key is the limiter's key for a client and the user name it presents: hex(HMAC-SHA-256(process key, ClientKey ‖ 0x00 ‖
// user)). Hashed, so the table holds no names and a long name costs no memory.
func (l *Limiter) Key(remoteAddr, user string) string
func (l *Limiter) Blocked(key string) (retryAfter time.Duration, blocked bool) // in a backoff now
// Reserve takes one in-flight attempt for key, unless its counted failures plus its attempts in flight are already 5.
func (l *Limiter) Reserve(key string) (*Attempt, bool)
func (l *Limiter) Succeed(key string) // a remembered login: zeroes the key's failures; its attempts in flight stay counted
type Attempt struct{ /* unexported */ }
func (a *Attempt) Succeed() // gives the place back and zeroes the key's failures
func (a *Attempt) Fail()    // counts one failure and starts or lengthens the backoff
func (a *Attempt) Abandon() // neither: the request went away, or the server was busy
func ClientKey(remoteAddr string) string // the IP of a host:port, Unmap()ped first, the zone dropped; an IPv6 address masked to its /64; the input itself when it does not parse
```

**Remembered logins.** `tag = HMAC-SHA-256(processKey, name ‖ 0x00 ‖ password)`. `Remembered` is true when the user has a remembered tag and `hmac.Equal` says it is this one. `Verify` takes one of 2 slots (or waits, at most 16 deep), verifies against the user's hash, or against the dummy hash when the name is unknown, and on success remembers the tag for that user, replacing an older one. `processKey` (32 random bytes) and **the dummy hash are made once per process**, at first use (`sync.Once`), the dummy at `auth.Iterations`: they are not remade when a `Users` is built, so building one costs no PBKDF2. A `Users` is immutable apart from its remembered tags; the server keeps the same `*Users` across config edits that leave the users section equal (`Equal`, Task 10), so an editor's own writes do not forget its login.

**The limiter.** A key is a client address **and the user name presented** (controller ruling): on the loopback default every local client has the same address, and one scraper with a stale password must not lock out the others. Per key: `failures` (counted), `inFlight` (reserved, not yet resolved), the time of the last failure. `Blocked` is true for `min(1 s × 2^(failures−5), 5 min)` after the last failure once `failures ≥ 5`. `Reserve` refuses when `failures + inFlight ≥ 5`: so of N simultaneous wrong logins for one key at most 5 are verified, however they are timed, because each takes its place before it is verified, not after. `Fail` gives the place back and adds a failure; `Succeed` (the limiter's and the attempt's) **sets `failures` to 0 and never deletes the key or touches the other attempts in flight**: deleting it would let those attempts give back places that are no longer counted, and `inFlight` would go negative; `Abandon` only gives the place back. A key idle for 15 min (and with nothing in flight) is deleted; with 4096 keys the one whose last failure is oldest and that has nothing in flight is deleted to make room.

**The order a caller follows** (Task 9 does; it is stated here because the pieces only make sense together): (1) `Blocked(key)` → refuse, **before anything is compared**: a blocked client must not learn whether its guess equals a remembered password; (2) `Remembered` → success, `Limiter.Succeed(key)`, no reservation (so many parallel requests of a logged-in client never meet the limit of 5); (3) `Reserve(key)` → refuse when false; (4) `Verify`, then `Succeed`, `Fail` (on `ErrBadCredentials`) or `Abandon` (on `ErrBusy` and on a ctx error).

- [ ] **Step 1: Write the failing tests**

```go
func TestHashFormat(t *testing.T)
// HashPassword("correct horse battery") matches ^pbkdf2-sha256\$600000\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$; two calls differ
// (the salt); ParseHash of it round-trips through String; Verify is true for the password and false for one byte changed.
// HashPassword of 11 bytes is an error.
func TestHashKnownAnswer(t *testing.T)
// NewHash("password", []byte("salt"), 1).Key is 120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b
// (PBKDF2-HMAC-SHA-256, the published test vector for c = 1, dkLen = 32).
func TestParseHashRefuses(t *testing.T)
// "hunter2-plaintext", "", "$2b$12$…" (bcrypt), "pbkdf2-sha1$600000$a$b", three fields, five fields, iterations "abc", 599999
// with min 600000, 10000001, padded base64, a 7-byte salt, a 31-byte key. Every error is non-empty and none contains the
// input or any 6-byte piece of it.
func TestNewUsers(t *testing.T)
// refuses: names "", "Bob", "a b", 33 bytes; two "ops"; role "admin" and ""; 65 users; viewers only. Accepts one editor.
func TestVerifyAndRemember(t *testing.T)
// (hashes at 1000 iterations) Verify of the editor with its password → Editor; a wrong password, an unknown name and an
// empty name → ErrBadCredentials; the viewer → Viewer. Remembered is false before the first Verify, true after it for that
// password only, and never true after a failed Verify. A counter on the PBKDF2 call (an unexported hook) shows one call per
// Verify and none per Remembered.
func TestVerifyBounded(t *testing.T)
// the hook blocks. 30 goroutines call Verify with wrong passwords, half on one Users and half on another: at most 2 are inside PBKDF2, 16 wait, and the other 12
// get ErrBusy at once. A waiter whose ctx is cancelled returns ctx.Err() and its place is taken by a new caller; when the
// hook is released, every waiter finishes.
func TestDummyOncePerProcess(t *testing.T)
// NewUsers called 50 times runs no PBKDF2 (the hook); the first Verify of an unknown name anywhere in the process makes the
// dummy (one extra call, at auth.Iterations), later ones do not. An unknown name and a known name with a wrong password
// each cost exactly one verification.
func TestUsersEqual(t *testing.T) // equal for the same list reordered; not equal when a hash string, a role or a name differs, or a user is added
func TestLimiter(t *testing.T)
// fake clock, one key, each attempt Reserve then Fail: 4 failures → not Blocked; the 5th → Blocked for 1 s; at +1 s not
// blocked; a 6th → 2 s; … the 14th and later → 5 min, never more; Succeed → clear at once and the next failure counts from
// 1; an idle key is gone after 15 min; Abandon changes nothing. With 3 attempts in flight, Limiter.Succeed and an
// Attempt.Succeed leave the other attempts counted: only 2 more can be reserved, and after all of them resolve inFlight is
// exactly 0 (never negative: a further 5 can then be reserved, not 6 or more).
func TestLimiterReservesBeforeVerifying(t *testing.T)
// 200 goroutines Reserve the same key at once and hold their attempt: exactly 5 get one. After those 5 Fail, the key is
// Blocked and Reserve is false. With 3 counted failures, only 2 more can be in flight. An Abandon gives a place back.
func TestLimiterKeyIsAddressAndUser(t *testing.T)
// Key("127.0.0.1:1", "scraper") and Key("127.0.0.1:2", "ops") differ; 5 failures on the first leave the second neither
// Blocked nor short of places. Key of the same address and user from two ports is equal. A key is 64 hex digits whatever the
// user name's length (1 MiB of name included), and holds neither the address nor the name.
func TestLimiterBounded(t *testing.T) // 5000 keys failing once each: at most 4096 tracked, the survivors are the latest, and a key with an attempt in flight is never the one dropped
func TestClientKey(t *testing.T)
// "1.2.3.4:5" → "1.2.3.4"; "[::ffff:1.2.3.4]:5" → "1.2.3.4" (unmapped first); "[2001:db8:1:2:3:4:5:6]:7" and
// "[2001:db8:1:2::9]:7" → the same key; "[fe80::1%eth0]:5" and "[fe80::2%eth1]:5" → the same key, without a zone;
// "garbage" → "garbage"
```

- [ ] **Step 2: Run** `go test -race -count=1 ./internal/auth/` — compile failure.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the same command — pass.
- [ ] **Step 5: Commit** `feat(auth): PBKDF2 password hashes, two roles and a failed-login limiter`

### Task 6: `internal/config` — the one config file

**Files:**
- Create: `internal/config/doc.go`, `document.go` (types, `Parse`, `ParseLenient`), `server.go` (the `server` section: defaults, checks, `Diff`), `edit.go` (`SetStream`, `DeleteStream`), `file.go` (`File`), `sync_unix.go`, `sync_other.go`, `exec_unix.go`, `exec_other.go`
- Modify: `go.mod` (`go.yaml.in/yaml/v3 v3.0.5` moves from the indirect to the direct `require` block; `go mod tidy` does it once the import exists; `go.sum` does not change)
- Test: `internal/config/document_test.go`, `server_test.go`, `edit_test.go`, `file_test.go`

**Interfaces:**
- Consumes: Task 2 (`spec.Stream`, `Validate`, `spec.MaxStreams`), Task 13 (`auth.ParseHash`, `auth.NewUsers`).
- Produces:

```go
package config

const ( Version = 1; MaxBytes = 4 << 20 )

type Document struct {
	Version int           `yaml:"version"`
	Server  Server        `yaml:"server"`
	Users   []User        `yaml:"users"`
	Streams []spec.Stream `yaml:"streams"`
}
type User struct {
	Name     string `yaml:"name"`
	Role     string `yaml:"role"`     // "viewer" or "editor"
	Password string `yaml:"password"` // a hash made by openstreamer hash-password
}
type Server struct {
	HTTP struct{ Listen string `yaml:"listen"` } `yaml:"http"` // the media listener, /live/; default ":8080"
	API  struct{ Listen string `yaml:"listen"` } `yaml:"api"`  // default "127.0.0.1:8081"
	RTMP struct { Listen string `yaml:"listen"`; App string `yaml:"app"` } `yaml:"rtmp"` // "" listen: off; app default "live"
	RTSP struct { Listen string `yaml:"listen"`; App string `yaml:"app"`; UDP bool `yaml:"udp"` } `yaml:"rtsp"`
	SRT  struct { Listen string `yaml:"listen"`; App string `yaml:"app"`; LatencyMS int `yaml:"latency_ms"` } `yaml:"srt"` // default 120, 1…65535
	Transcoder struct{ Binary string `yaml:"binary"` } `yaml:"transcoder"` // "": server.New looks once for ostranscoder next to the executable; Parse and Resolved leave "" as it is
	AssetsDir  string `yaml:"assets_dir"`                                   // "": no assets
	Log struct { Level string `yaml:"level"`; Format string `yaml:"format"` } `yaml:"log"`
}
func (s Server) Resolved() Server      // the defaults filled in, from constants only: it never looks at the file system, and leaves transcoder.binary "" as ""
func (s Server) Diff(o Server) []string // the dotted keys whose resolved values differ, sorted: "server.rtmp.listen"
// CheckFiles looks at the file system, which Parse never does: a transcoder.binary that is given must be a regular file (and
// on Unix have an execute bit), and assets_dir, when given, a directory. The server calls it once, at start.
func (s Server) CheckFiles() error

type Options struct{ MinIterations int } // 0 means auth.MinIterations. Tests pass less; cmd/openstreamer never does.

type Parsed struct { Doc Document; Users *auth.Users; UserList []auth.User } // UserList: the parsed users, for auth.Users.Equal

// SyntaxError: the text is not one YAML document of the expected shape. Its Error() hides the values the library quotes:
// yaml puts the offending scalar between backquotes ("cannot unmarshal !!str `hunter2` into int"), and it may be a secret
// typed on the wrong line. On each line of the message, everything from the first backquote to the last is replaced by
// `…` (not pair by pair: a value that itself contains a backquote would leak its tail).
type SyntaxError struct{ Err error }
type Errors []error                   // every validation error of a document; Error() joins them with "; "

// Parse is strict (R-D4): one document, known keys only, at most MaxBytes; then every check, collecting every error.
// It returns *SyntaxError or Errors.
func Parse(text []byte, opt Options) (Parsed, error)
// ParseLenient is Parse for the start: a stream entry that does not decode, fails Validate or repeats a name is left out of
// Doc.Streams and reported in skipped ("streams[3] (cam): …"); every other error is err, as from Parse.
func ParseLenient(text []byte, opt Options) (p Parsed, skipped []error, err error)

func Hash(text []byte) string // `"` + hex(sha256(text)) + `"`: the config's ETag

// SetStream returns text with the stream of sp's name replaced, or sp appended to streams. DeleteStream removes it
// (ErrNoStream when absent). Both edit the YAML node tree, so the comments and the order of everything else stay.
func SetStream(text []byte, sp spec.Stream) ([]byte, error)
func DeleteStream(text []byte, name string) ([]byte, error)
var ErrNoStream = errors.New("config: no such stream")

var ErrStale = errors.New("config: the file changed on disk")
type File struct{ /* unexported */ }
// Open resolves path's symlinks once (filepath.EvalSymlinks: the rename must replace the real file, not the link), reads the
// file (at most MaxBytes), removes the temp files a crashed write of this file left, and remembers the text's hash. On Unix
// it refuses a file whose mode has any group or other bit ("config: <path> must be mode 0600").
func Open(path string) (*File, []byte, error)
func (f *File) Read() ([]byte, error)   // the text on disk now; the same mode check as Open, every time (a reload reads through it)
func (f *File) Stale() (bool, error)    // the text on disk has another hash than the remembered one. Bytes only: no mode check. An unreadable file is (true, err)
func (f *File) Write(text []byte) error // ErrStale when stale; else R-D4's atomic write, and the hash is remembered
func (f *File) Adopt(text []byte)       // remember text's hash: after a reload took the file as it is
```

**Checks** (`Parse`; each error names its place, as `server.srt.latency_ms: …` or `users[1].role: …`, and never quotes a password field): `version` is 1; each listen address is empty or `host:port` with a port of 0 to 65535, `http.listen` and `api.listen` not empty after defaults; apps not empty and without `/` at their ends; `rtsp.udp` needs `rtsp.listen`; `srt.latency_ms` 1…65535; `log.level` one of `debug`, `info`, `warn`, `error` and `log.format` one of `json`, `text`; each user's `password` passes `auth.ParseHash(…, MinIterations)` and the list passes `auth.NewUsers`; at most `spec.MaxStreams` streams, each passing `Validate`, with distinct names. `Parse` does no `stream.Check` and no claims: those need the running server and are `Manager.Replace`'s. **`Parse` never touches the file system**: whether `transcoder.binary` and `assets_dir` exist is `Server.CheckFiles`' question (on Unix, `exec_unix.go`, an execute bit; elsewhere, `exec_other.go`, a regular file is the whole check: Windows has no such bit), asked once at start. So a binary that disappears while the server runs does not make every later edit of the config fail; the stream that needs it reports it (Review Focus 5).

**Two checks on the node tree**, made by walking the decoded `yaml.Node` before anything else, each an entry of `Errors` with its line. **In `ParseLenient` they follow the same split as every other check:** a violation inside `streams[i]` skips that entry and is reported as `streams[i] (name): line N: …` (the walk is run per entry node), so one stream written with an anchor or a `0720` does not stop the start; a violation in `version`, `server` or `users` is `err` and stops it. An alias in one entry to an anchor in another skips both entries (the one with the anchor and the one with the alias).
- **No anchors, aliases or merge keys** (`&a`, `*a`, `<<:`): a node with an `Anchor`, an `AliasNode`, or a mapping key tagged `!!merge` is refused with `line N: anchors, aliases and merge keys are not supported`. They would make a stream entry's text depend on another place in the file, which the per-entry lenient decoding and the node edits of `SetStream` and `DeleteStream` cannot keep whole (an edit could leave an alias dangling).
- **Numbers are plain decimal integers.** The config has no fractional field, and YAML converts silently: `0720` is 464, `1.5` and `1.0` become 1, `1_000` is 1000. Every scalar the library resolved as `!!int` or `!!float` must match `^-?(0|[1-9][0-9]*)$`, or it is refused with `line N: write the number as a plain decimal integer, or quote it if it is text`. A quoted scalar is a string and is not affected. **The rule is on the scalar, not on the field**, so it also catches unquoted text that looks like a number: a stream `name: 007`, a rendition `label: 007`, a watermark `text: 24.7` or `text: 1e3` are refused with that message, where the library would otherwise hand the string field a value the operator did not write (`7`, or a reformatted float). Quoting them (`"007"`) is the fix the message names.

**Strict decoding.** `yaml.NewDecoder` with `KnownFields(true)`; a second document in the text is a `*SyntaxError`; duplicate keys are refused by the library and surface as `*SyntaxError`. `ParseLenient` decodes `streams` as `[]yaml.Node` first, and each entry by marshalling the node back to text and decoding that with a strict decoder (a `Node` cannot be decoded strictly by itself).

**`SetStream`.** Decode `text` into a `yaml.Node`; find the top-level `streams` sequence (append the key when it is missing); find the entry whose `name` scalar is `sp.Name`. Replace: encode `sp` to a node and put its `Content` in place of the entry's, keeping the entry node's own head and foot comments; when several entries have that name (a duplicate the start skipped), the first is replaced and the others removed. Append otherwise. `DeleteStream` removes every entry of the name. Encode with `SetIndent(2)`.

**What a node round trip keeps and loses** (measured in review; R-D4): the library re-emits the whole document, so head comments (the lines above a key or an entry) and the order of keys and entries survive, while **blank lines are dropped, sequences are re-indented, the spacing before a line comment is normalised, comments inside the replaced entry are lost, and so is a line comment on the replaced entry's `name:`**. Only a text that is already in the library's own formatting comes back byte-equal outside the edited entry. A whole-config `PUT` is not affected: it never goes through a node.

**Repairing a skipped entry.** Because `SetStream` and `DeleteStream` find an entry by its `name` scalar alone, a per-stream `PUT` or `DELETE` of the name of an entry the start skipped replaces or removes that entry, and if it was the only fault the document then parses and the edit goes through. An entry whose `name` cannot be read is repaired with a whole-config `PUT` or by hand.

**`File.Write`.** Steps, each followed by a call of an unexported `hook func(step string) error` that tests set (nil in production): create `.<base>.<8 random hex>.tmp` in the file's directory with `O_WRONLY|O_CREATE|O_EXCL`, 0600 (`created`); write (`written`); `fsync` (`synced`); rename over the file (`renamed`); `fsync` the directory (`dirsynced`). A real error before the rename removes the temp file; a hook error returns at once and removes nothing, which is what a crash leaves. **An error after the rename** (the directory `fsync` failing) is returned, and the hash is **not** remembered: the new text is on disk, the caller's edit was not applied (`commit` failed), so the file is stale and API writes answer 409 until a reload takes the file as it is. Calls are serialised by a mutex in `File`.

- [ ] **Step 1: Write the failing tests** (each in `t.TempDir()`; hashes at 1000 iterations with `Options{MinIterations: 1000}` unless the test is about the floor)

```go
func TestParseMinimal(t *testing.T)
// version 1, one editor, no server section, no streams: Parse succeeds; Server.Resolved() has ":8080", "127.0.0.1:8081",
// apps "live", latency 120, level "info", format "json"; Users.Len() is 1.
func TestParseStrict(t *testing.T)
// an unknown top-level key, an unknown key inside server.rtmp, inside a user and inside a stream's rendition; a duplicate
// key; two documents; a tab-indented line; 4 MiB + 1 byte: each is a *SyntaxError or Errors naming the key or line.
// Anchors, aliases and merge keys, each refused with its line: a stream written as `- &cam {…}` with a later `- *cam`; a
// `<<: *defaults` inside a stream; an anchor that is never used. Numbers, each refused with its line: height 0720, 0x2d0,
// 1_080, 720.0, 720.5 and 7.2e2; bitrate_kbps "800" (a string where an int is wanted is the library's type error). Height
// 720 and latency_ms 120 pass. Text that looks like a number, each refused with "…or quote it if it is text": name: 007,
// label: 007, watermark text: 24.7 and text: 1e3; the same four quoted pass and keep their characters ("007", "24.7").
// A comment and a quoted hash are accepted.
func TestSyntaxErrorHidesValues(t *testing.T)
// `latency_ms: hunter2` and a password written under the wrong key as a bare scalar: the error names the line and the
// types and contains neither "hunter2" nor the scalar; every backquoted piece of the library's message reads `…`. A value
// with a backquote inside it, latency_ms: hun`ter2`tail, leaves neither "hun", "ter2" nor "tail" in the message.
func TestParseServer(t *testing.T)
// one row per server check above. Parse does not look at the file system: a transcoder.binary and an assets_dir that do
// not exist pass Parse. Server.CheckFiles refuses a binary that does not exist, is a directory, and on Unix a regular file
// without an execute bit; a regular executable file passes (on Windows: a regular file passes); assets_dir must be a directory.
func TestParseUsers(t *testing.T)
// a plaintext password ("hunter2-plaintext"), a bcrypt hash, 599999 iterations (with the default Options), role "admin", two
// users "ops", no editor, 65 users: each is in Errors under its users[i] path, and no error text contains "hunter2" or any
// hash. With the default Options a 600000-iteration hash passes.
func TestParseCollectsEveryError(t *testing.T) // a document with a bad listen address, a bad role and two invalid streams returns Errors of length 4
func TestParseStreams(t *testing.T) // a stream that fails Validate, two streams "cam", and 1001 streams are errors; the YAML of a full stream (ladder, watermark, thumbnail, pushes) equals the JSON form field for field
func TestParseLenientSkipsBadStreams(t *testing.T)
// streams: good cam; an entry with an unknown key; an entry failing Validate; a second "cam"; good other. ParseLenient
// returns [cam, other] and three skipped errors naming index and name; a bad server section or user is err and nothing is
// returned. Parse of the same text returns Errors with those three. The node-tree checks follow the same split: an entry
// with `height: 0720`, an entry written `- &x {…}` and an entry `- *x` are each skipped as `streams[i] (name): line N: …`
// and the good streams are returned; `latency_ms: 0120` in server, and an anchor on a user, are err.
func TestHash(t *testing.T) // quoted 64 hex digits; one changed byte (a comment) changes it
func TestSetStreamKeepsComments(t *testing.T)
// fixture 1, already in the library's formatting (made by encoding a node once: 2-space indent, no blank lines, one space
// before a line comment), with a header comment, a comment above server, a line comment on a user, comments above streams
// "a" and "b" and inside "a": after SetStream(b changed) the text is byte-for-byte equal outside b's entry, and b's head
// comment is kept. Fixture 2, hand-formatted (blank lines between sections, 4-space sequences, aligned line comments, a
// line comment on b's `name:`): every head comment is still there and the order of keys and streams is kept, and the test
// pins the losses so that a library upgrade that changes them is noticed: the blank lines are gone, the indentation is 2,
// the comments inside b and on b's `name:` are gone. In both, Parse of the result has the new b. SetStream(c, new) appends
// c after b; SetStream on a text without a streams key adds one; SetStream of a name that two entries have leaves one.
func TestSetStreamRepairsSkippedEntry(t *testing.T)
// a text whose entry "cam" has an unknown key (ParseLenient skips it): SetStream(cam, valid) gives a text that Parse
// accepts; DeleteStream("cam") too. An entry without a name is not found by either.
func TestDeleteStream(t *testing.T) // removes b and its comments only (fixture 1: byte-equal otherwise); ErrNoStream for an unknown name; the last stream leaves "streams: []"
func TestOpenMode(t *testing.T)    // Unix only: 0644 and 0640 are refused with the path and "0600"; 0600 and 0400 pass; a file chmodded to 0644 after Open makes the next Read fail the same way
func TestOpenSymlink(t *testing.T) // Unix: Open of a symlink to the file; after Write the link is still a link, the real file has the new text, and the temp file was made next to the real file
func TestWriteAtomic(t *testing.T) // Write replaces the text; the file is still 0600; no temp file is left; Stale is false after; with the directory read-only (skip as root) Write fails and the old text is intact
func TestWriteCrashPoints(t *testing.T)
// for each hook step (created, written, synced, renamed, dirsynced) with the hook returning an error: the file on disk reads
// back byte-equal to the old text (before "renamed") or to the new one (from "renamed" on), never anything else, and Parse
// accepts it; a temp file may be left; a new Open of the path removes it and loads the surviving text. At "renamed" and
// "dirsynced" Write returned an error although the new text is on disk: Stale is then true, a second Write is ErrStale, and
// Adopt of the text on disk clears it.
func TestStale(t *testing.T)
// after Open: Stale false. The test rewrites the file by hand: Stale true and Write returns ErrStale, the hand edit intact.
// Adopt(the hand-edited text): Stale false and Write works. A hand edit that restores the same bytes is not stale.
func TestConcurrentWrites(t *testing.T) // 8 goroutines × 30 Writes of different valid texts under -race: the final file is exactly one of them, no temp files
```

- [ ] **Step 2: Run** `go test -race -count=1 ./internal/config/` — compile failure.
- [ ] **Step 3: Implement.** Then `go mod tidy`, and check with `git diff go.mod go.sum` that the only change is `go.yaml.in/yaml/v3 v3.0.5` moving between the two `require` blocks.
- [ ] **Step 4: Run** the same command, `GOOS=windows go vet ./internal/config/` and `GOOS=darwin go vet ./internal/config/` — pass.
- [ ] **Step 5: Commit** `feat(config): one YAML config file, strict parsing, comment-keeping edits and atomic writes`

### Task 7: `internal/stream` — the per-stream supervisor

**Files:**
- Create: `internal/stream/doc.go`, `env.go` (`Env`, `Output`, `Check`, the spec-to-options functions), `supervisor.go` (the loop, `Submit`), `generation.go` (build, `reattach`, teardown), `status.go` (`View`, `Status`, `Collect`)
- Test: `internal/stream/check_test.go`, `supervisor_test.go`, `reattach_test.go`, `helpers_test.go`

**Interfaces:**
- Consumes: Task 2 (`spec`), Task 3 (`metrics.Writer`, `metrics.L`), Task 4 (`logging.Scrub`), Task 5 (`hub.Refuse`, `ThumbnailerOptions.OnEvent`); `pipeline.New`, `transcode.New`, `transcode.NewThumbnailer`, `live.NewPackager`, `rtmppush.New`, `picture.LoadAsset`, `ingest.Parse`, `redact.URL`; in tests `transcodetest`'s fake child.
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
	// The server's RTMP play address, as bound, and its app, for Check's publish-path rule; "" means off.
	RTMPPlay, RTMPApp string
	// Reserved are the server's own listeners as bound (HTTP, API, RTMP, RTSP, SRT); the Manager's claims start from them.
	Reserved []Claim
	Logger *slog.Logger                           // nil discards
	Now    func() time.Time                       // nil means time.Now
	After  func(time.Duration) <-chan time.Time   // nil means time.After; the restart backoff
	Open   func(ctx context.Context, url string) (ingest.Reader, error) // tests; nil means ingest.Open
	TranscoderArgs, TranscoderEnv []string        // tests: the fake child
}

// Claim is one listen address: a server listener's, or a stream's listen input's (R-D11).
type Claim struct {
	UDP   bool   // the family: UDP (UDP and SRT listen inputs, the SRT server) or TCP
	Host  string // "" is the wildcard (0.0.0.0, ::, or no host)
	Port  int
	RTMP  bool   // an RTMP listener: RTMP claims on the same Host and Port share it
	Path  string // an RTMP publish path; "" for a play listener and for every other claim
	Owner string // a stream name, or "server"
}

// Check reports why sp cannot run under env (R-D11). sp has passed Validate.
func Check(sp spec.Stream, env Env) error

type Supervisor struct{ /* unexported */ }
// NewSupervisor: sp has passed Check and is not disabled. after, when not nil, is the Done of the previous supervisor of
// the same name. Run waits for it before it builds anything, and Done is never closed before after is: a supervisor that is
// cancelled while it waits still waits for after before it closes Done. So the chain is transitive: whatever was started and
// cancelled in between, a supervisor builds only when every earlier one of its name has let go of its sockets.
func NewSupervisor(sp spec.Stream, env Env, after <-chan struct{}) *Supervisor
func (s *Supervisor) Run(ctx context.Context) // returns when ctx ends, with everything stopped and waited for
func (s *Supervisor) Done() <-chan struct{}   // closed when Run has returned
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
	State       string       `json:"state"` // "stopped", "starting", "running", "no_input", "backoff"
	Generation  uint64       `json:"generation"`
	Since       time.Time    `json:"since"`          // when the generation started
	Restarts    uint64       `json:"restarts"`       // rebuilds after a failure
	Rebuilds    uint64       `json:"rebuilds"`       // rebuilds for an edit
	LastError   string       `json:"last_error,omitempty"`    // scrubbed; prefixed "input N: ", "transcoder: ", "thumbnailer: " or "build: "
	LastErrorAt *time.Time   `json:"last_error_at,omitempty"`
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
	ID     string `json:"id"`     // the first 8 hex digits of sha256(URL): the metrics' target label
	Target string `json:"target"` // redacted
	State  string `json:"state"`; Connects uint64 `json:"connects"`; Failures uint64 `json:"failures"`
	LastError string `json:"last_error,omitempty"`
}
```

**`Check`** (each error is prefixed `stream: check:`): every input passes `ingest.Parse` (or `env.Open` is set); `layout` maps to `[]media.Kind` (empty: video, audio); with `transcode`, the options are built as osdev's `parseLadders` does (rendition `Name` = `<name>_<label>`, `Presentation: true`, `Binary: env.Transcoder`), an image watermark is read with `picture.LoadAsset(env.AssetsDir, image)` (an empty `AssetsDir` is an error), anything that encodes needs `env.Transcoder != ""`, and `transcode.New` on a scratch `hub.New(hub.Options{})` must succeed; a thumbnail needs `env.Transcoder != ""` and `picture.Thumbnail.Validate`; every push passes `rtmppush.New` on a scratch hub; an RTMP listen input on `env.RTMPPlay`'s address must not publish under the play app at all: its path's app (the path without its last segment) must differ from `RTMPApp`. A publish path is never playable, so a publisher at `<RTMPApp>/<anything>` would shadow the play path of whichever stream has, or later gets, that name; refusing the whole app is the rule that does not depend on other streams. Listen-address overlaps are not `Check`'s: they are the Manager's claims (Task 8), which see every stream and the server's listeners. `Check` and the generation's build share the functions that turn a spec into options, so they cannot disagree on the spec; what can still differ is the world (an asset deleted since), which is why build has an error path.

**A generation** (unexported `generation`): the source hub; the `pipeline.Pipeline` (`Inputs` from the spec, `Env: ingest.Env{Lookup: env.Lookup}`, `Layout`, `EndOnEOF: false`, and `OnEvent`, see **Last error** below); with a ladder, the `transcode.Transcoder` and its rendition and presentation hubs; a `transcode.Thumbnailer` when the spec has a thumbnail and no child encodes (as osdev's `parseThumbnailers`); and the attached outputs. The served hubs follow R-D3: without a ladder `Single[name]` is the source hub and its packager is non-ABR; with one, `Single[name]` and `Single[name_label0]` are rendition 0's hub, `Single[name_labelI]` rendition I's, `Packs[name]` is an ABR packager (`live.Options{ABR: true, Logger: …}`) on the presentation hub with a queue of 1024 × (renditions + 1), and `Packs[name_label]` a non-ABR packager on each rendition hub with a queue of 1024. Packagers subscribe under `"packager "+servedName`.

**The loop** (this is the algorithm the tests pin):

```go
func (s *Supervisor) Run(ctx context.Context) {
	defer close(s.done)                        // runs last: after the wait for s.after below, on every path
	defer s.closeMailbox()                     // every pending and later Submit gets a closed channel
	if s.after != nil {
		select {
		case <-s.after:                        // the previous supervisor of this name has stopped
		case <-ctx.Done():
			<-s.after                          // cancelled while waiting: still wait, so Done stays behind after.
			return                             // Bounded: the manager cancels a predecessor before it starts a successor.
		}
	}
	wait := time.Second
	for ctx.Err() == nil {
		reason, lived := stopFailed, time.Duration(0)
		g, err := s.build(ctx)                 // hubs, pipeline, transcoder, thumbnailer; on an error nothing is left running
		if err != nil {
			s.fail("build: ", err)             // LastError; state "backoff" below
		} else {
			s.reattach(g, spec.Outputs{}, s.spec.Outputs) // starts the outputs and publishes the View
			s.answerRebuilds(g)                // edits that asked for this generation get their answer now
			reason = s.serve(ctx, g)           // returns on ctx, on a rebuild edit, or when a part of g ended
			lived = s.now().Sub(g.started)
			s.teardown(g)                      // publishes the empty View first, then stops and waits (see teardown)
		}
		switch reason {
		case stopCtx:
			return
		case stopEdit:                         // s.spec is already the new one; no wait
			s.rebuilds++
		case stopFailed:
			s.restarts++
			if lived >= 60*time.Second { wait = time.Second }
			if !s.idle(ctx, wait) { return }   // state "backoff"; an edit arriving here replaces s.spec and ends the wait
			wait = min(2*wait, 30*time.Second)
		}
	}
}
```

`build` returns an error when something the spec names is no longer there: today that is the watermark's asset (`picture.LoadAsset` is read again for every generation), and any error of `transcode.New`, `transcode.NewThumbnailer` or `rtmppush.New` is handled the same way rather than assumed away. A failed build is `stopFailed`: `LastError` is `build: …`, the state is `backoff`, and the next attempt follows the backoff. An edit that asked for a rebuild whose build fails is answered `ChangeRebuild` when the failure is recorded, not held until a build succeeds.

`serve` selects on `ctx.Done()`, the mailbox signal and `g.failed`, which is closed when the pipeline's `Run`, the transcoder's `Run` **or the thumbnailer's `Run`** returns while `g`'s context is live (for any reason: each of them is meant to run for the generation's life); its error becomes `LastError`. For each queued edit, in order: `spec.Diff(s.spec, next)`; `ChangeNone` answers at once; `ChangeOutputs` calls `reattach(g, s.spec.Outputs, next.Outputs)`, sets `s.spec`, answers; `ChangeRebuild` sets `s.spec`, returns `stopEdit`, and the answer is sent after the next generation's `reattach` (or its failed build). Edits queued behind a rebuild are applied to the new generation the same way.

**Last error.** `s.fail(prefix, err)` stores `prefix + logging.Scrub(err.Error())` and the time, behind the status pointer. It is fed by: the pipeline's `EventFailed` (`input N: …`), **except an error that is `io.EOF`** (`errors.Is`): with `EndOnEOF` false a file input without `loop` reaches its end through the pipeline's failure path and restarts, which is the stream working as ruled (R-D5), so it is counted in `openstreamer_stream_input_eof_total` and not shown as an error; the transcoder's `EventExited` (`transcoder: …`: a child that exited, was killed, or could not be started, which the transcoder retries by itself without ending the generation); the thumbnailer's `EventExited` (`thumbnailer: …`, Task 5); a failed build; and the error that closed `g.failed`. It is cleared by a rebuild for an edit, and kept otherwise, with its time, so an operator sees the last thing that went wrong even after recovery.

**State** comes from the pipeline, not from the hub (`Hub.Live()` stays true once a keyframe was written, whatever happens to the input): `starting` while no input has been active in this generation, `running` while `pipeline.Stats().Active >= 0`, `no_input` when an input was active and none is now, `backoff` between generations after a failure, `stopped` without a supervisor. `openstreamer_stream_up` is 1 exactly in `running`.

**`reattach(g, old, new spec.Outputs)`** makes the smallest set of calls (R-D6), in this order. Let `off(o)` be the prefixes of the session outputs disabled in `o`: `"httpts "` for `!o.HTTPTS`, `"rtmp "` for `!o.RTMP`, `"rtsp "` for `!o.RTSP`, `"srt "` for `!o.SRT` (`"rtmp "` does not match `"rtmppush …"`).
1. Packagers: with `want := new.HLS || new.DASH` and `have := old.HLS || old.DASH`, start them all on `want && !have`, stop them all (cancel, close their subscriptions, wait) on `!want && have`, and touch none otherwise.
2. Pushes, keyed by URL: stop and wait for each one in `old` and not in `new`; start one for each in `new` and not in `old`, on `Single[s.spec.Name]`; leave the rest.
3. On every distinct hub in `Single`, `Refuse(off(old) ∩ off(new)…)`: this only relaxes. An output being re-enabled must be subscribable **before** the view announces it, or a player that connects at once would be handed a closed subscription.
4. Publish the new `View`: sessions of a re-enabled output are now found, and new sessions of a disabled output find nothing.
5. On the same hubs, `Refuse(off(new)…)`: this ends the sessions of the outputs just disabled, and only those.

At the first attach `old` is the zero `Outputs`, whose `off` is all four prefixes, so step 3 refuses exactly `off(new)` before anything is announced.

`teardown` publishes the empty `View`, cancels `g`'s context, closes the source hub, and **waits, in this order, for: the pipeline's `Run`** (its return is what closes the input readers, so the listen sockets and RTMP publish routes of this generation are free before the next one, or the next supervisor of this name, builds); the transcoder and the thumbnailer (which close the rendition and presentation hubs); the packagers and the pushers. This is osdev's shutdown order with the pipeline's wait made explicit.

**`Collect`** writes, with `stream=<name>` on every series (help texts are the implementer's, one sentence each):

| Family | Type | Extra labels | Source |
|---|---|---|---|
| `openstreamer_stream_up` | gauge | | 1 in state `running` (`pipeline.Stats().Active >= 0`) |
| `openstreamer_stream_generation` | gauge | | `View.Gen` |
| `openstreamer_stream_restarts_total` | counter | `reason` = `failure`, `edit` | `Restarts`, `Rebuilds` |
| `openstreamer_stream_active_input` | gauge | | `pipeline.Stats.Active` |
| `openstreamer_stream_input_switches_total` | counter | | `Switches` |
| `openstreamer_stream_input_eof_total` | counter | | inputs that ended with `io.EOF` and were restarted |
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
| `openstreamer_push_up` | gauge | `target` (8 hex digits of the URL's SHA-256) | 1 when the pusher's state is `rtmppush.StatePublishing` |
| `openstreamer_push_connects_total`, `_failures_total`, `_republishes_total`, `_messages_total` | counter | `target` | the pusher's `Stats` |

Transcoder families are written only for a stream with a ladder or a thumbnailer.

- [ ] **Step 1: Write the failing tests.** `helpers_test.go` has `scriptedOpen`, an `Env.Open` whose readers emit frames of `testdata/fixtures/h264_aac.ts` in a loop under test control (and can be told to fail), a fake `After`, and `tap(h)`, a subscriber that records frames. Tests with a ladder use `transcodetest`'s fake child: `Env.Transcoder` is the test binary (`os.Args[0]`, whose `TestMain` calls `transcodetest.Main`), with `TranscoderArgs`/`TranscoderEnv` as `internal/transcode`'s own tests set them.

```go
func TestCheck(t *testing.T)
// one row per Check rule: a URL ingest.Parse refuses; an encoded rung with Transcoder ""; an image watermark with AssetsDir "";
// an asset that does not exist; a watermark next to a copy rung; a thumbnail with Transcoder ""; a push that is not rtmp://;
// an RTMP listen input on RTMPPlay with RTMPApp "live" publishing at live/cam, at live/other and at live/a/b (app "live/a" is
// accepted; "in/key" is accepted; the same paths on another address are accepted).
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
// The re-enable order is pinned: a goroutine that subscribes "rtsp cam" the moment View().Outputs.RTSP turns true (polled in a
// tight loop, 200 rounds of off and on) never gets a closed subscription.
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
// pipeline retries), state "starting", stream_up 0, LastError "input 0: …" without "hunter2" and LastErrorAt set; when Open
// starts succeeding the state becomes "running" and stream_up 1, and LastError is still there.
func TestSupervisorInputLoss(t *testing.T)
// running, then the only input's reader fails and every reopen fails: within the stall limit the state is "no_input" and
// openstreamer_stream_up is 0, although View().Source.Live() is still true; Gen is unchanged (no rebuild). The reader comes
// back: "running" and 1 again.
func TestSupervisorEOFIsNotAnError(t *testing.T)
// an input whose reader returns io.EOF after 50 frames and reopens fine: after three rounds LastError is empty,
// openstreamer_stream_input_eof_total is 3, and Restarts is 0.
func TestSupervisorSpawnFails(t *testing.T)
// (a) a ladder with Env.Transcoder naming a file that does not exist (Check passes: the path is only configured): one
// generation, Restarts 0, LastError starts "transcoder: " and names the spawn failure, the transcoder's Spawns rises with the
// fake After; (b) the same for a thumbnail-only stream: LastError starts "thumbnailer: ".
func TestGenerationRunnerFailure(t *testing.T)
// every long-lived part of a generation (pipeline, transcoder, thumbnailer) is started through one unexported helper,
// g.run(name string, f func(context.Context) error), which closes g.failed with "<name>: <err>" when f returns while g's
// context is live, whatever f returns, and does nothing when it returns after the cancel. Called with a function that
// returns an error, for each of the three names: the supervisor goes to "backoff", Restarts is 1, LastError has the name.
// And end to end: a thumbnail-only stream whose fake child announces another wire version (the Thumbnailer's Run returns
// that error): "backoff", Restarts 1, LastError starts "thumbnailer: ".
func TestSupervisorAssetGone(t *testing.T)
// an image watermark from a temp AssetsDir; the stream runs (Gen 1). The asset file is deleted, then the generation is made
// to fail (the fake child of another wire version on respawn): the next build fails, state "backoff", LastError starts
// "build: " and names the asset, View() is empty, nothing is subscribed anywhere, and the fake After keeps doubling. The file
// is put back: the next attempt runs (a new Gen). A Submit asking for a rebuild while the asset is gone is answered ChangeRebuild.
func TestSupervisorWaitsForPredecessor(t *testing.T)
// NewSupervisor(sp, env, after) with after open: Run builds nothing (Open is not called) until after is closed. Cancelling
// ctx first does not close Done: Done closes only once after is closed too, Open was never called, and a Submit made while it
// waited gets a closed channel. A chain of three: S1 with a slow teardown (its reader's Close blocks until released), S2 on
// S1.Done, cancelled at once, S3 on S2.Done: S3 does not call Open until S1's Close is released. Two supervisors of one spec with an RTMP listen input, the second chained on the first's Done: the
// first is cancelled and the second started at once; the second's LastError never holds "already registered" or
// "address already in use", and it becomes "running" when a publisher connects.
func TestSupervisorStopDuringBackoff(t *testing.T)
// in "backoff" with a 30 s wait pending: cancelling ctx makes Run return within 1 s; and, separately, a Submit during the
// wait is applied at once (Gen advances without the fake After firing).
func TestSupervisorSubmitOrder(t *testing.T)
// 50 Submits alternating two output sets, sent without waiting: every channel answers, in order, and the final View.Outputs
// is the last spec's. A Submit after Run returned gets a closed channel.
func TestSupervisorNoLeaks(t *testing.T) // not parallel, like every test of this package (none calls t.Parallel, so the count is this test's): start, 3 output edits, 1 rebuild, stop: runtime.NumGoroutine returns to its starting value within 2 s
func TestCollect(t *testing.T)
// a laddered stream with two pushes; removing the first leaves the second's target label and counters as they were. Then: metricstest.Parse accepts the output; the families of the table are present with
// stream="cam"; openstreamer_transcoder_encoder_fallbacks_total{stream="cam"} is the fake child's count; no label value contains "://".
func TestStatusRedacts(t *testing.T) // inputs, push targets and every LastError in Status hold no password or stream key; json.Marshal(Status) neither; a push's ID is 8 hex digits
```

- [ ] **Step 2: Run** `go test -race -count=1 ./internal/stream/` — compile failure.
- [ ] **Step 3: Implement** `env.go` and `Check`; run `-run TestCheck` — pass.
- [ ] **Step 4: Implement** the generation, the loop, `reattach` and `teardown`; run `-run 'TestSupervisor'` — pass.
- [ ] **Step 5: Implement** `Status`, `Shot` and `Collect`; run `go test -race -count=1 ./internal/stream/` — pass; then `-count=20 -run 'TestSupervisor(OutputEditInPlace|SubmitOrder|NoLeaks|WaitsForPredecessor)'` — pass.
- [ ] **Step 6: Commit** `feat(stream): the per-stream supervisor with in-place output edits`

### Task 8: `stream.Manager`

**Files:**
- Create: `internal/stream/manager.go`, `internal/stream/claims.go` (listen claims, `copy://` cycles, served-name lookup)
- Test: `internal/stream/manager_test.go`, `claims_test.go`

**Interfaces:**
- Consumes: Task 7.
- Produces:

```go
// Outcome is what an edit did to one stream.
type Outcome struct {
	Name    string `json:"name"`
	Applied string `json:"applied"` // "created", "started", "stopped", "unchanged", "outputs", "rebuild", "deleted"
}

var (
	ErrInvalid  = errors.New("stream: invalid spec") // wraps Check's errors
	ErrConflict = errors.New("stream: conflict")     // a listen claim or a copy:// cycle
	ErrClosed   = errors.New("stream: manager closed")
)

type Manager struct{ /* unexported */ }
func NewManager(env Env) *Manager // env.Lookup is replaced by the manager's CopyHub
// Run returns when ctx ends and every supervisor has stopped. Load and Replace may be called before or while it runs.
func (m *Manager) Run(ctx context.Context)
// Load installs the start-up set, leniently (R-D4): in name order, a stream that fails Check or whose claims conflict with
// the server's or an earlier stream's is kept in the set without a supervisor, state "stopped", the reason as its LastError
// and in the returned errors. It is called once, on an empty manager.
func (m *Manager) Load(streams []spec.Stream) []error
// Replace makes streams the whole set, all or nothing. It judges the streams that are new or changed (below); on any error
// it returns every one of them, joined, and changes nothing. Otherwise it calls commit, when not nil; if commit fails,
// its error is returned and nothing changed. Then it applies the difference and returns one Outcome per name of the old and
// the new set, sorted by name. **Once commit has succeeded, Replace returns a nil error, whatever happens to ctx**: the edit
// is in the file and in force, and a caller that saw an error would keep stale bookkeeping. The streams have passed Validate
// and have distinct names (config.Parse).
func (m *Manager) Replace(ctx context.Context, streams []spec.Stream, commit func() error) ([]Outcome, error)
func (m *Manager) Get(name string) (spec.Stream, bool)
func (m *Manager) List() []spec.Stream                    // sorted by name
func (m *Manager) Status(name string) (Status, bool)      // a stream without a supervisor reports state "stopped"
func (m *Manager) Shot(name string) (picture.Shot, time.Time, bool)
func (m *Manager) Hub(out Output, served string) (*hub.Hub, bool)            // out is httpts, rtmp, rtsp or srt
func (m *Manager) Packager(out Output, served string) (*live.Packager, bool) // out is hls or dash
func (m *Manager) CopyHub(name string) (*hub.Hub, bool)   // <stream>: its source hub; <stream>_<label>: that rendition's hub
func (m *Manager) Collect(w *metrics.Writer)              // every supervisor's Collect, by name, and openstreamer_streams{state}
```

The manager does not know the file: `commit` is how the caller (Task 10) writes it at the one moment that is safe, after everything was judged and before anything is applied.

**Locks.** Edits (`Load`, `Replace`, and the start of supervisors in both) are serialised by one mutex, `edit`. Readers never take it: the streams live in an immutable `map[string]*entry` behind an `atomic.Pointer`, which an edit replaces with a changed copy as its **last** step (`entry` holds the spec, the supervisor or nil, and the load-time error or ""). So a lookup from an output server, a status request and a scrape never wait for the file's `fsync` inside `commit` or for another stream's edit. When `Run`'s context ends, it takes `edit`, sets the closed flag (a later `Replace` is `ErrClosed`), releases it, and only then waits for the supervisors, so no supervisor is started after the wait began.

**`Replace`**, holding `edit`:
1. Sort every name of the old and new sets into *deleted*, *created*, *changed* (the spec differs) and *unchanged*.
2. **Judge only what the edit touches.** For each created or changed stream that is enabled: `Check`; its claims against `env.Reserved`, against each other inside its own spec, against the other created or changed enabled streams of this call, and against the **holders**: the streams that have a supervisor now and are neither deleted nor changed by this call. Then the `copy://` cycles of the new set. An unchanged stream is not judged again, whatever its state. **One rule for streams without a supervisor:** a disabled stream and a stream left stopped at load hold no claims and are not judged while they stay as they are; they are judged like any other when an edit changes them, and an unchanged one stays stopped (its `LastError` kept). So a conflict that the start left in the file does not make every later edit fail; it only keeps its own stream down until that stream is edited or the server restarts.
3. `commit()`.
4. Apply: cancel the supervisors of deleted and newly disabled streams (no wait); create a supervisor for each created or newly enabled stream, and for a changed stream that had none, chained on its name's tail; `Submit` the changed spec to a running one; swap the map.
5. Release `edit`; wait for the `Submit` answers, or for `ctx`. `ctx` ending here (the client hung up during a rebuild) is **not an error**: the edits are applied and committed, the outcomes do not depend on the answers (they come from the classification of step 1 and from `spec.Diff`), and the supervisors finish on their own. Return the outcomes with a nil error: `deleted`; `created`; `started` (a stream that existed without a supervisor and has one now); `stopped` (it had one and is now disabled); `unchanged`; `outputs` or `rebuild` as `spec.Diff` says.

**One supervisor per name at a time (R-D5).** The manager keeps, per name, the **tail**: the `Done()` of the last supervisor it created for that name, also after that stream was stopped or deleted. A new supervisor of the name gets the tail as `after`, and becomes the tail. Because a supervisor's `Done` never closes before its own `after` (Task 7), the tail is closed only when every earlier supervisor of the name has stopped, however many were created and cancelled in between. Stopping and deleting only cancel; nothing in `Replace` waits for a teardown. A tail that is closed and whose name has no stream any more is dropped from the map at the next edit, so the map does not grow with deleted names.

The lookups split a served name at its first `_` (stream names have none), find the entry in the current map, read the supervisor's `View` and test the output's flag: `Hub(OutRTSP, "cam_720")` is `View.Single["cam_720"]` when `View.Outputs.RTSP`.

**Claims** (`claims.go`). `claimsOf(sp spec.Stream) []Claim` gives one claim per listen input: an RTMP listen input (`mode=listen`, default port 1935) is `{RTMP: true, Host, Port, Path}` with the host normalised as `rtmpsrv.AddrKey` does and the path by `rtmp.NormalizePath`; a UDP or SRT listen input (`ingest.Parse(url).Passive`) is `{UDP: true, Host, Port}`; **a UDP input whose host is a multicast address gives no claim**. `conflict(a, b Claim) bool`:

```go
func conflict(a, b Claim) bool {
	if a.UDP != b.UDP || a.Port != b.Port { return false }
	if a.RTMP && b.RTMP && a.Host == b.Host {    // one shared RTMP listener
		return a.Path != "" && a.Path == b.Path  // two publishers of one path; a play listener ("" path) shares with all
	}
	return a.Host == b.Host || a.Host == "" || b.Host == ""
}
```

So RTMP on `:P` and on `127.0.0.1:P` conflict whatever their paths (two listeners cannot both bind), a UDP input on the SRT server's port conflicts, and a listen input on the HTTP, API or RTSP port conflicts. The error names both owners and the address, never a path's key. `Load` runs the same rule in name order from `env.Reserved`.

`copy://` cycles: a depth-first walk over "stream reads stream" edges (a `copy://x_label` edge goes to `x`), reporting the path as `a -> b -> a`.

- [ ] **Step 1: Write the failing tests** (`scriptedOpen` from Task 7; `commit` is a closure that records its calls and can fail or block; `set(specs…)` builds the slice)

```go
func TestManagerReplaceCreatesAndEdits(t *testing.T)
// Replace([cam]) → [{cam created}], commit called once, Status running. The same set again → [{cam unchanged}], and commit is
// still called (the caller decides whether to write). cam with rtsp off → outputs, same generation. cam with a new input →
// rebuild, generation 2. cam disabled → stopped, Status "stopped", Hub(OutRTMP,"cam") not found. Enabled again → started.
// Replace([]) → [{cam deleted}], lookups fail.
func TestManagerReplaceTouchesOnlyWhatChanged(t *testing.T)
// a, b, c running. Replace with b's outputs edited, c removed, d added: outcomes a unchanged, b outputs, c deleted, d created;
// a's and b's generations and hub pointers are unchanged and a tap on a's hub saw no Lost.
func TestManagerReplaceAllOrNothing(t *testing.T)
// a set with one good new stream and two bad ones (an encoded rung with Transcoder "", a UDP claim on a Reserved port):
// the error wraps ErrInvalid and ErrConflict and names both streams; commit was not called; the good one was not created.
func TestManagerCommitFails(t *testing.T) // commit returning an error: Replace returns it; Get still returns the old spec, no generation moved, nothing was created
func TestManagerReplaceCtxEndsAfterCommit(t *testing.T)
// a rebuild edit whose teardown is held (a blocking reader Close), with a ctx the test cancels once commit has been called:
// Replace returns within 1 s with a nil error and [{cam rebuild}]; Get returns the new spec at once; when the teardown is
// released the stream reaches generation 2 with the new spec. The same with a ctx already cancelled before the call: commit
// is still called, the error is nil, and the edit is applied (Replace does not look at ctx before the wait).
func TestConflict(t *testing.T)
// the rule alone, as a table of claim pairs. Conflict: UDP :P / UDP :P; UDP 0.0.0.0:P / UDP 127.0.0.1:P; UDP 10.0.0.1:P twice;
// RTMP 127.0.0.1:P path a twice; RTMP :P path a / RTMP 127.0.0.1:P path b; RTMP :P path a / TCP :P (the HTTP listener);
// RTMP 127.0.0.1:P path a / RTMP play on :P. No conflict: different ports; TCP :P / UDP :P; UDP 10.0.0.1:P / UDP 10.0.0.2:P;
// RTMP 127.0.0.1:P paths a and b; RTMP 127.0.0.1:P path a / RTMP play on 127.0.0.1:P.
func TestClaimsOf(t *testing.T)
// rtmp://:1935/in/k?mode=listen → RTMP, host "", port 1935, path in/k; rtmp://h/in/k (a pull) → none; udp://:5000 and
// srt://0.0.0.0:5000?mode=listener → UDP, host "", 5000; udp://239.1.1.1:5000 and udp://[ff02::1]:5000 → none; an RTSP pull → none.
func TestManagerListenClaim(t *testing.T)
// Reserved: HTTP 127.0.0.1:H, API 127.0.0.1:A, RTMP play 127.0.0.1:R, RTSP 127.0.0.1:S (TCP), SRT 127.0.0.1:U (UDP).
// Stream a holds rtmp://127.0.0.1:R/in/key?mode=listen and udp://:P. Each of these Replaces (a plus a new b) is ErrConflict,
// calls no commit and leaves a's generation unchanged: b with a's RTMP URL; b with rtmp://:R/in/other?mode=listen; b with
// udp://127.0.0.1:P; b with srt://127.0.0.1:P?mode=listener; b with udp://:U (the SRT server's port); b with
// rtmp://127.0.0.1:H/x/y?mode=listen, and the same on A and on S; b with two inputs udp://:Q and srt://:Q?mode=listener
// (inside one spec); and b and c, both new, both with udp://:Q. Each error names the address and both owners and holds no
// "key". Accepted: b with rtmp://127.0.0.1:R/in/other?mode=listen (the shared listener, another path); b with udp://:Q;
// b and c both with udp://239.1.1.1:P2; a itself keeping its inputs; a deleted and b taking a's URLs in one Replace; and a
// disabled b with a's URLs (it holds nothing), which becomes ErrConflict only when a later Replace enables it.
func TestManagerCopyCycle(t *testing.T) // a copies b, b copying a → ErrConflict naming "b -> a -> b"; a copying a_720 → conflict; a copying an unknown stream is accepted
func TestManagerLookups(t *testing.T)
// laddered cam with rtsp off: Hub(OutRTMP,"cam_360") found, Hub(OutRTSP,"cam") not, Hub(OutRTMP,"cam_x"), "nope", "" and "_" not;
// Packager(OutHLS,"cam") is the ABR packager; with hls off and dash on, Packager(OutHLS,…) not found and Packager(OutDASH,…) found;
// CopyHub("cam") is the source, CopyHub("cam_360") the rendition.
func TestManagerLoad(t *testing.T)
// Load of: two enabled, one disabled, one whose Check fails (an encoded rung, Transcoder ""), "y" and "z" with the same
// udp://:P input, and "x" with a listen input on a Reserved port. Two run, y runs; the disabled one, the failing one, z and x
// are "stopped", the last three with a LastError naming the reason (for z: the address and "y"); Load returns three errors;
// List returns all seven. **The conflict left in the set does not block edits:** Replace with only y's outputs changed →
// y outputs, everything else unchanged, z and x still stopped with their LastError; Replace adding an unrelated stream w →
// w created; Replace with z on another port → z started; Replace with x unchanged but y deleted → x and z stay as they are.
// Cancelling ctx stops everything; a later Replace is ErrClosed.
func TestManagerChainSurvivesCancelledWaiter(t *testing.T)
// a stream with an RTMP listen input and a udp:// listen input on real loopback ports (Env.Open nil), whose first supervisor
// has a slow teardown (the test holds its pipeline's Run through a blocking reader Close). Replace: create (S1) → delete →
// create (S2, waits for S1) → delete (S2 is cancelled while it waits) → create (S3). While S1's teardown is held, S3's Status
// stays "starting" with no LastError and Open is not called for it; when it is released, S3 runs. At no time does a Status
// or a log line hold "already registered", "address already in use" or "bind:". The tails map has one entry afterwards, and
// none after the stream is deleted, S3 has stopped and one more edit has run.
func TestManagerConcurrentReplaces(t *testing.T)
// not parallel. The same stream with real listen inputs. 20 goroutines each read List, change that stream (alternating an
// output edit and a rebuild edit) and Replace, while one goroutine deletes and re-creates it three times; then one more
// Replace. No deadlock within 20 s; the final List equals the set of the last Replace that returned; View().Outputs equals
// it; no Status sampled every 5 ms and no captured log line holds "already registered", "address already in use" or
// "bind:"; no goroutine is left after Run ends.
func TestManagerReadersDoNotWaitForEdits(t *testing.T)
// commit blocks until released: meanwhile Hub, Packager, CopyHub, Get, List, Status and Collect for this and another stream
// all return within 100 ms and show the state before the edit; after the release they show the new one.
func TestManagerCollect(t *testing.T)  // openstreamer_streams{state="running"} 2, {state="stopped"} 1; every per-stream family carries a stream label of a known name
```

- [ ] **Step 2: Run** `go test -race -count=1 -run 'TestManager|TestConflict|TestClaimsOf' ./internal/stream/` — compile failure.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/stream/` and `-count=20 -run 'TestManager(ConcurrentReplaces|ChainSurvivesCancelledWaiter|ReadersDoNotWaitForEdits)'` — pass.
- [ ] **Step 5: Commit** `feat(stream): the manager: Replace, cross-stream checks and output lookups`

### Task 9: `internal/api` — the REST API, Basic auth and the OpenAPI document

**Files:**
- Create: `internal/api/doc.go`, `api.go` (routes, the outer wrapper), `auth.go` (Basic auth, roles, the limiter), `streams.go`, `config.go` (the config editor, reload, status), `errors.go`, `internal/api/openapi.json` (embedded with `go:embed`)
- Test: `internal/api/api_test.go`, `auth_test.go`, `streams_test.go`, `config_test.go`, `openapi_test.go`

**Interfaces:**
- Consumes: Task 8 (`stream.Outcome`, `stream.Status`, `stream.ErrInvalid`, `ErrConflict`, `ErrClosed`), Task 6 (`config.SyntaxError`, `config.Errors`, `config.ErrStale`, `config.ErrNoStream`, `config.MaxBytes`), Task 13 (`auth.Users` with `Remembered` and `Verify`, `auth.Limiter` with `Key`, `Blocked`, `Reserve` and `Attempt`, `auth.ErrBadCredentials`, `auth.ErrBusy`, the roles), Task 2 (`spec`), Task 3 (`metrics.Writer`), Task 4 (`logging.Scrub`, for error messages).
- Produces:

```go
package api

// Backend is the server behind the API (Task 10 implements it). Every write goes through its one apply path.
type Backend interface {
	Get(name string) (spec.Stream, bool)
	List() []spec.Stream
	Status(name string) (stream.Status, bool)
	Shot(name string) (picture.Shot, time.Time, bool)
	ConfigText() (text []byte, etag string) // the text last loaded, reloaded or written, and config.Hash of it
	ConfigStatus() ConfigStatus
	PutConfig(ctx context.Context, text []byte, ifMatch string) (ApplyResult, error)
	PutStream(ctx context.Context, sp spec.Stream, ifMatch string) (out stream.Outcome, err error)
	DeleteStream(ctx context.Context, name, ifMatch string) error
	Reload(ctx context.Context) (ApplyResult, error)
}

type ApplyResult struct {
	Streams         []stream.Outcome `json:"streams"`          // one per stream of the old and the new config, by name
	RestartRequired []string         `json:"restart_required"` // the server keys that differ from the running ones; [] when none
	ETag            string           `json:"etag"`             // of the config now in force
}
type ConfigStatus struct {
	ETag            string     `json:"etag"`
	LoadedAt        time.Time  `json:"loaded_at"`       // when the config in force was loaded, reloaded or written
	Stale           bool       `json:"stale"`           // the file on disk differs: a reload is pending, API writes are refused. Read from the file at every call, not kept
	LoadErrors      []string   `json:"load_errors"`     // what the start skipped or left stopped
	LastReloadAt    *time.Time `json:"last_reload_at,omitempty"`
	LastReloadError string     `json:"last_reload_error,omitempty"`
	RestartRequired []string   `json:"restart_required"`
}

var (
	ErrNotFound             = errors.New("api: not found")
	ErrPrecondition         = errors.New("api: precondition failed")   // an If-Match that does not match
	ErrPreconditionRequired = errors.New("api: precondition required") // PUT /config without If-Match
)

type Options struct {
	Backend Backend
	Users   func() *auth.Users // the users in force; called per request, so a reload takes effect at once
	Limiter *auth.Limiter
	Metrics http.Handler       // served at /metrics
	Version string
	Logger  *slog.Logger       // nil discards
	Now     func() time.Time
}

type API struct{ /* unexported */ }
func New(opt Options) (*API, error)       // an error for a nil Backend, Users or Limiter
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request)
func (a *API) Collect(w *metrics.Writer)  // openstreamer_api_requests_total{method,route,code}, openstreamer_api_auth_failures_total, openstreamer_api_auth_blocked_total
func Routes() []Route                     // every route, sorted by path and method
type Route struct{ Method, Path string; Role auth.Role } // Role "" means open
```

Routes, roles and answers (JSON unless said otherwise):

| Route | Role | Success | Notes |
|---|---|---|---|
| `GET /healthz` | open | 200 `{"status":"ok"}` | |
| `GET /api/v1/version` | viewer | 200 `{"version":"…"}` | |
| `GET /api/v1/openapi.json` | viewer | 200, the embedded document | |
| `GET /api/v1/streams` | viewer | 200 `{"streams":[spec…]}` | sorted by name; `?secrets=1` needs an editor |
| `GET /api/v1/streams/{name}` | viewer | 200 spec, `ETag` | `?secrets=1` needs an editor |
| `PUT /api/v1/streams/{name}` | editor | 201 (`created`) or 200 `{"stream":spec,"applied":"…"}`, `ETag` | optional `If-Match` on the stream's `ETag`; the body's `name` may be empty and must otherwise equal the path's |
| `DELETE /api/v1/streams/{name}` | editor | 204 | optional `If-Match` |
| `GET /api/v1/streams/{name}/status` | viewer | 200 `stream.Status` | |
| `GET /api/v1/streams/{name}/thumbnail.jpg` | viewer | 200 `image/jpeg` | `ETag` `"<epoch>-<pts>"`, `Cache-Control: no-cache`, served with `http.ServeContent` as osdev's `thumbHandler`; 404 until the first shot |
| `GET /api/v1/config` | editor | 200 `application/yaml`: `ConfigText`'s bytes, verbatim; `ETag` | secrets and password hashes included, which is why a viewer gets 403 |
| `PUT /api/v1/config` | editor | 200 `ApplyResult`, `ETag` | `Content-Type: application/yaml`; `If-Match` required; body at most `config.MaxBytes`; the bytes are written as sent |
| `GET /api/v1/config/status` | viewer | 200 `ConfigStatus` | |
| `POST /api/v1/reload` | editor | 200 `ApplyResult` | what SIGHUP does; no body |
| `GET /metrics` | viewer | the `Metrics` handler | |
| `GET /debug/pprof/…` | editor | `net/http/pprof`'s `Index`, `Cmdline`, `Profile`, `Symbol`, `Trace` | |

**The order of one request** (the outer wrapper; this order is what the tests pin):
1. **Route first, from the pattern only.** `_, pattern := mux.Handler(r)` is called for the pattern and nothing else (the handler it returns has no path values and is never called). The pattern is `"GET /api/v1/streams/{name}"`: the wrapper cuts the method off and looks `(method, path)` up in the table behind `Routes()`. Found: that path is the `route` of the access log and the counter, and its role is the one required. **A path the mux redirects resolves to its target**: for a path it cleans (a doubled slash, a `..`), `mux.Handler` returns the *target's* pattern with its method (`GET /api/v1//streams` gives `"GET /api/v1/streams"` and a 307 handler; measured on go1.26.11), so such a request is treated as a request for that route: authenticated, authorised and counted under it, and then answered by the mux's redirect in step 7. **The lookup requires the `METHOD ` prefix**: the mux's trailing-slash redirect returns a bare path as its pattern, which must not be mistaken for a route. Anything else is `unmatched`: no pattern (an unknown path), a bare-path pattern (the trailing-slash redirect), and **a known path with another method** (the mux then returns an empty pattern and its 405 handler).
2. An `unmatched` request is handed to `mux.ServeHTTP` and gets the mux's own answer, **without authentication**: 404, 405 with `Allow`, or the trailing-slash redirect, as plain text. It is counted under `route="unmatched"` with that code. (It tells a stranger which paths exist, which the OpenAPI document does anyway; it never reaches a handler of ours.) An open route (`/healthz`) is dispatched the same way.
3. A `PUT`, `DELETE` or `POST` without an `X-Requested-By` header is 400 `bad_request` ("the X-Requested-By header is required"), before any credential is looked at (R-D8).
4. Credentials: `r.BasicAuth()`. Missing or malformed is 401; it is not a failed login and counts against nobody.
5. The limiter and the users, in the order Task 13 fixes, with `key := Limiter.Key(r.RemoteAddr, name)`: `Blocked(key)` → 429 with `Retry-After` (whole seconds, rounded up), nothing compared; `Users().Remembered` → authenticated, `Limiter.Succeed(key)`; else `Reserve(key)` false → 429 with `Retry-After: 1`; else `Users().Verify(r.Context(), …)`: a role → `Succeed`; `auth.ErrBadCredentials` → `Fail`, 401; `auth.ErrBusy` → `Abandon`, 503 `unavailable` with `Retry-After: 1`; a context error → `Abandon` (the client is gone).
6. The route's role, and an editor for `?secrets=1`: 403 otherwise.
7. `mux.ServeHTTP(w, r)`: the real dispatch, which is what fills `r.PathValue`.

The access log line (R-D10) has the method, route, status, duration, remote address and, once authenticated, the user's name; never a header, the query or a body. The counter's `method` is the request's when it is `GET`, `PUT`, `DELETE`, `POST`, `HEAD` or `OPTIONS` and `other` for anything else. `HEAD` matches the `GET` patterns, as in the mux. A failed login is logged at `warn` at most once every 10 s (`Options.Now`), with the remote address and the presented name passed through `logging.Scrub` and cut to 32 bytes, never the password.

Errors are `{"error":{"code":"…","message":"…","errors":["…"]}}` (`errors` only where the table says), `Content-Type: application/json`, every text passed through `logging.Scrub`:

| Status | `code` | When |
|---|---|---|
| 400 | `bad_request` | a stream body that is not one JSON document or has an unknown field or a `name` other than the path's; a config body that is not one YAML document of the expected shape (`*config.SyntaxError`, its message with the values hidden); a `PUT`, `DELETE` or `POST` without `X-Requested-By` |
| 401 | `unauthorized` | no, malformed or wrong credentials; with `WWW-Authenticate: Basic realm="openstreamer"` |
| 403 | `forbidden` | a viewer on an editor's route, or with `?secrets=1` |
| 404 | `not_found` | `ErrNotFound`, `config.ErrNoStream`, an unknown stream, an invalid name in the path, no thumbnail yet |
| 404, 405, 3xx | (the mux's plain text) | an `unmatched` request: an unknown path (404), a known path with another method (405 with `Allow`), a trailing-slash redirect. Not authenticated, not JSON |
| 307 | (the mux's redirect) | a path the mux cleans to a registered route (`//`, `..`): authenticated and authorised as that route first, so 401, 403 or 429 without the credentials for it; counted under that route |
| 409 | `conflict` | `stream.ErrConflict` (a listen claim, a `copy://` cycle); `errors` lists each |
| 409 | `config_changed` | `config.ErrStale`: the file was edited by hand; reload first |
| 412 | `precondition_failed` | `ErrPrecondition` |
| 413 | `too_large` | a stream body over `spec.MaxSpecBytes`, a config body over `config.MaxBytes` |
| 415 | `unsupported_media_type` | a stream `PUT` that is not `application/json`, a config `PUT` that is not `application/yaml` |
| 422 | `invalid_config` | `config.Errors` (the whole document is judged, also for a per-stream edit); `errors` lists every one |
| 422 | `invalid_spec` | `stream.ErrInvalid` (`Check`); `errors` lists every one |
| 428 | `precondition_required` | `ErrPreconditionRequired` |
| 429 | `too_many_attempts` | the limiter: the pair of address and user is in a backoff, or already has 5 logins counted or in flight; with `Retry-After` |
| 500 | `internal` | anything else (a failed file write); the cause is logged, the message is "internal error" |
| 503 | `unavailable` | `stream.ErrClosed`; `auth.ErrBusy` (with `Retry-After: 1`) |

Every response sets `Cache-Control: no-store` (except the thumbnail) and `X-Content-Type-Options: nosniff`. JSON stream responses redact (`spec.Stream.Redacted`) unless the query is exactly `secrets=1` from an editor.

`internal/api/openapi.json` is OpenAPI 3.1: every route above with its parameters and bodies (the config as `application/yaml`, type string), the `If-Match` and `ETag` headers, the `secrets` query parameter, a `basicAuth` security scheme (`type: http`, `scheme: basic`) on all but `/healthz`, an `x-role` of `viewer` or `editor` on each operation, and `components.schemas` for `Stream`, `Input`, `Transcode`, `Rendition`, `Audio`, `Watermark`, `Thumbnail`, `Outputs`, `Push`, `Status` and its nested types, `Outcome`, `ApplyResult`, `ConfigStatus` and `Error`, each property with a one-line description and the limits of Task 2's `Validate`.

- [ ] **Step 1: Write the failing tests** (a fake `Backend`; `httptest`; users `ops` (editor) and `eye` (viewer) with hashes at 1000 iterations; a `Limiter` on a fake clock)

```go
func TestAuth(t *testing.T)
// every route but /healthz: no header, "Bearer x", a Basic header that is not base64, an unknown user and a wrong password
// → 401 with WWW-Authenticate exactly `Basic realm="openstreamer"` and the error body; /healthz is 200 without a header; an
// unmatched path is the mux's 404 without a header. New with a nil Users is an error. 100 failed logins, from 100 addresses within 10 s, log one warning, which holds
// the name and not the password.
func TestRoles(t *testing.T)
// from Routes(): the viewer gets 200 on every viewer route and 403 on every editor route (PUT and DELETE of a stream,
// GET and PUT of the config, POST reload, pprof), and 403 for GET streams?secrets=1; the editor gets no 403 anywhere.
// Routes() has exactly the table's roles.
func TestUsersReload(t *testing.T) // Options.Users returning a new set: the old password is 401 on the next request and the new one 200; returning the same set again: the next request of a remembered login runs no PBKDF2
func TestLoginBackoff(t *testing.T)
// 5 wrong passwords for "ops" from 10.0.0.1 → 401 each; the 6th request, with the right password, and even though "ops" has
// a remembered login from 10.0.0.2, is 429 with Retry-After "1" and nothing was compared (a counter on PBKDF2 and one on
// Remembered); after 1 s the right password is 200 and clears it. Never affected: "ops" from 10.0.0.2, and "eye" from
// 10.0.0.1 (the same address, another user: the scraper with a stale password does not block its neighbour). Requests
// without credentials never count. The 429s are in openstreamer_api_auth_blocked_total, the 401s in
// openstreamer_api_auth_failures_total.
func TestLoginBurst(t *testing.T)
// 200 simultaneous requests with wrong passwords for "ops" from one address: the PBKDF2 counter ends at 5 or less; the rest
// are 429. 100 simultaneous requests of a remembered login: all 200, no 429, no PBKDF2 (a remembered login reserves nothing).
func TestLoginBusy(t *testing.T)
// PBKDF2 held by the hook, 30 simultaneous first logins of 30 different unknown names: 2 inside, 16 waiting, 12 answered
// 503 unavailable with Retry-After "1" and counted against nobody; a waiting request whose client hangs up frees its place.
func TestWritesNeedRequestedBy(t *testing.T)
// every PUT, DELETE and POST route from Routes(), as the editor, without X-Requested-By → 400 bad_request, the Backend is
// not called and no PBKDF2 ran; with the header (any value) it works; GET routes never need it. A bodyless
// POST /api/v1/reload with only an Authorization header, which a cross-site form could send, is among the refused.
func TestStreamCRUD(t *testing.T)
// PUT → 201, applied "created", ETag equal to the spec's; GET returns it with the same ETag; PUT again → 200 "unchanged";
// list holds it; DELETE → 204; GET → 404. PUT with a body name "other" → 400; with no name → the path's. The Backend got
// the If-Match value verbatim.
func TestErrors(t *testing.T)
// one row per line of the error table, by the Backend's fake returning that error or by the request itself: the status, the
// code, and for the rows with `errors` the full list (a config.Errors of three gives three). A 500 says "internal error" and
// the cause is in the log. A 412 has no ETag header.
func TestPathNames(t *testing.T)
// PUT and GET of "%2e%2e", "a%2Fb", "A", "x_y", "con" and a 64-byte name → 404 not_found, and the Backend was not called. A
// literal "/api/v1/streams/../x" is cleaned by ServeMux into a redirect before any handler: assert only that the answer is
// not 2xx, with or without credentials, and the Backend was not called.
func TestResponsesRedact(t *testing.T)
// a stored spec with rtsp://u:hunter2@h/a and push rtmp://h/live/secretkey: GET, the list and the PUT response hold neither
// "hunter2" nor "secretkey", for the viewer and the editor; the editor with ?secrets=1 gets both; ?secrets=0 and
// ?secrets=1&x=1 redact.
func TestPutRejectsMaskedSecrets(t *testing.T)
// with a Backend whose PutStream runs spec.Validate as the real path does: PUT a spec with a password; GET it (redacted);
// PUT that body back → 422; GET ?secrets=1 still returns the password.
func TestConfigGet(t *testing.T)
// the editor gets ConfigText's bytes exactly (a text with comments, a stream key and a password hash), Content-Type
// application/yaml, ETag the Backend's; If-None-Match with it → 304; the viewer gets 403 and no byte of the text.
func TestConfigPut(t *testing.T)
// the body reaches Backend.PutConfig byte for byte (comments, trailing newline, CRLF) with the If-Match value; the answer
// is the ApplyResult with its ETag header. Without If-Match → 428 and the Backend is not called; text/plain and
// application/json → 415; MaxBytes + 1 → 413; the viewer → 403.
func TestReloadAndStatus(t *testing.T) // POST reload returns the Backend's ApplyResult (viewer: 403); GET config/status is the Backend's document for both roles
func TestStatusAndThumbnail(t *testing.T)
// status is the fake's document; the thumbnail is the fake's JPEG bytes with image/jpeg and the ETag "3-90000"; If-None-Match
// with that ETag → 304; no shot → 404; an unknown stream → 404.
func TestMetricsAndPprof(t *testing.T) // /metrics is the given handler for the viewer; /debug/pprof/ is 200 for the editor, 403 for the viewer, 401 without credentials
func TestRequestCounter(t *testing.T)
// after 2 GETs of two different streams and one 404 path: Collect has openstreamer_api_requests_total{method="GET",
// route="/api/v1/streams/{name}",code="200"} 2 and a route="unmatched" series; no series holds a stream name or a user name.
// Requests with the methods "BREW", "get" and a 40-byte random token all land in method="other". A request without
// credentials to /api/v1/streams/x is counted under route="/api/v1/streams/{name}",code="401": the route is resolved
// before authentication, and the label has no method in it. Unmatched, each without credentials and each counted under
// route="unmatched": POST /api/v1/streams/x → 405 with an Allow header naming GET, PUT and DELETE; GET /api/v1/nope → 404;
// GET /api/v1/streams/x/ (a trailing slash: the mux's bare-path pattern) → the mux's own answer, never a handler of ours.
// A cleaned path resolves to its target's route: GET /api/v1//streams is 401 without credentials and 307 with them, both
// counted under route="/api/v1/streams", and never reaches a handler of ours; a pattern without a "METHOD " prefix is
// never looked up as a route (a unit row on the lookup function: "/api/v1/streams" → unmatched, "GET /api/v1/streams" → found).
func TestPathValuesReachHandlers(t *testing.T)
// GET, PUT and DELETE of /api/v1/streams/cam and GET of its status and thumbnail: the Backend is called with "cam", not ""
// (the dispatch goes through mux.ServeHTTP, not through the handler mux.Handler returned). HEAD /api/v1/streams/cam is
// authenticated and counted under the GET route.
func TestAccessLog(t *testing.T) // one info line per request with method, route, status, duration and the user; a request with Authorization and ?secrets=1 logs neither the header, the password nor the query
func TestOpenAPIMatchesRoutes(t *testing.T)
// the embedded document parses; its set of method and path equals Routes() minus the pprof routes; every operation except
// /healthz requires basicAuth and its x-role equals the route's Role; GET /api/v1/openapi.json serves the same bytes.
func TestOpenAPIMatchesSpec(t *testing.T)
// by reflection: for each schema named above, its property names equal the JSON field names of the Go type, and each
// property has a description.
```

- [ ] **Step 2: Run** `go test -race -count=1 ./internal/api/` — compile failure.
- [ ] **Step 3: Implement** the handler; write `openapi.json`.
- [ ] **Step 4: Run** the same command — pass.
- [ ] **Step 5: Commit** `feat(api): the REST API with Basic auth and roles, the config editor and the OpenAPI document`

### Task 10: `internal/server` and `cmd/openstreamer`

**Files:**
- Create: `internal/server/doc.go`, `server.go` (listeners, wiring, `Run`), `apply.go` (the one apply path: the `api.Backend`), `media.go` (the `/live/` handler and its output gate), `collect.go` (the output servers' and the config's metrics), `cmd/openstreamer/main.go`, `hashpw.go`, `tty_linux.go`, `tty_darwin.go`, `tty_other.go`, `openstreamer.example.yaml`
- Modify: `Makefile` (target `openstreamer`: `go build -ldflags "-X main.version=$(shell git describe --tags --always --dirty)" -o bin/openstreamer ./cmd/openstreamer`, added to `.PHONY`)
- Test: `internal/server/server_test.go`, `apply_test.go`, `media_test.go`, `cmd/openstreamer/main_test.go`, `hashpw_test.go`, `sighup_unix_test.go`, `tty_linux_test.go`

**Interfaces:**
- Consumes: Tasks 3, 4, 6, 7 (`Claim`, `Env`), 8, 9, 13; `rtmpplay.New`, `rtspplay.New`, `srtout.New`, `httpts.Handler`, `live.Handler`, `rtmpsrv.Default.RegisterPlay`.
- Produces:

```go
package server

type Options struct {
	Version       string
	LogOut        io.Writer // nil means os.Stderr
	MinIterations int       // passed to config.Options; 0 means auth.MinIterations. Tests only.
}
type Addrs struct{ HTTP, API, RTMP, RTSP, SRT string } // host:port as bound; "" when off

type Server struct{ /* unexported */ }
// New opens and parses the config file (leniently for streams, R-D4), binds every listener (so Addrs is final when it
// returns), and loads the streams into a Manager whose Env carries the bound addresses. It starts no stream.
func New(path string, opt Options) (*Server, error)
func (s *Server) Addrs() Addrs
func (s *Server) Manager() *stream.Manager
// Run serves until ctx ends, then stops in order and returns nil, or the first listener error.
func (s *Server) Run(ctx context.Context) error
// *Server implements api.Backend: ConfigText, ConfigStatus, PutConfig, PutStream, DeleteStream, Reload, and Get, List,
// Status and Shot from the Manager. Reload is also what SIGHUP calls.

// LiveOutputs returns the outputs a /live/<name>/<dir>/<file> path is part of: hls/* and cmaf/*.m3u8 → [hls];
// cmaf/*.mpd → [dash]; any other cmaf file → [hls, dash]; anything else → nil.
func LiveOutputs(dir, file string) []stream.Output
```

**The one apply path** (`apply.go`). The state it guards, under one mutex `cfg`: the `config.File`; `applied`, the text in force (also behind an atomic pointer, for `ConfigText`); `started`, the resolved `server` section the process started with; the users (an `atomic.Pointer[auth.Users]`, which `api.Options.Users` reads); the status fields, published after every change as one immutable snapshot behind an atomic pointer, so `ConfigStatus()` and `ConfigText()` never take `cfg` and a viewer's status request does not wait for an apply that is waiting for a rebuild. **`Stale` is not in the snapshot**: a hand edit is not something the server sees happen, so no publish would ever set it. `ConfigStatus()` returns the snapshot with `Stale` filled from a live `s.file.Stale()` at each call, which takes the `File`'s own mutex (held only for one read or one write of the file) and never `cfg`. `File.Stale` compares bytes only: it reads the file without the mode check, so a file that was only chmodded is **not** stale (its bytes are the applied ones; the next reload fails on the mode and says so in `last_reload_error`); a file that cannot be read at all (removed, no permission) is reported as stale, with the error logged at most once every 10 s.

```go
// apply makes text the config in force. write says whether the text came over the API (and must be written) or from the
// file (a reload). The caller holds s.cfg.
func (s *Server) apply(ctx context.Context, text []byte, write bool) (api.ApplyResult, error) {
	p, err := config.Parse(text, s.parseOpt)          // strict: the whole document, every error
	if err != nil { return api.ApplyResult{}, err }
	commit := func() error {                          // runs inside Replace, after every stream was judged
		if !write || bytes.Equal(text, s.applied) { return nil }
		return s.file.Write(text)                     // ErrStale if the file changed since the stale check
	}
	out, err := s.manager.Replace(ctx, p.Doc.Streams, commit)
	if err != nil { return api.ApplyResult{}, err }   // nothing applied, nothing written. After a successful commit Replace
	                                                  // never fails (Task 8), so from here on the bookkeeping always runs,
	                                                  // also when ctx has ended: the client hanging up changes nothing.
	if !write { s.file.Adopt(text) }
	if !s.users.Load().Equal(p.UserList) {            // only a changed users section replaces the set: an unchanged one
		s.users.Store(p.Users)                        // keeps its remembered logins, so an edit costs its author no PBKDF2
	}
	s.setApplied(text)                                // applied, its hash, LoadedAt; publishes the status snapshot
	s.restart = s.started.Diff(p.Doc.Server.Resolved())
	return api.ApplyResult{Streams: out, RestartRequired: s.restart, ETag: config.Hash(text)}, nil
}
```

The four entries, each taking `s.cfg` for its whole length, so two writers are never interleaved:
- **`PutConfig(text, ifMatch)`:** an empty `ifMatch` is `api.ErrPreconditionRequired`; `s.file.Stale()` true is `config.ErrStale` (checked **before** `If-Match`: a client whose `ETag` is current still must not overwrite a hand edit); `ifMatch` other than `config.Hash(applied)` is `api.ErrPrecondition`; then `apply(text, true)`. The file ends up byte-equal to `text`.
- **`PutStream(sp, ifMatch)`:** stale → `config.ErrStale`; `ifMatch`, when not empty, must be `*` or the current stream's `ETag()`, and a missing stream fails it; `text := config.SetStream(applied, sp)`; `apply(text, true)`; the outcome is the one of `sp.Name`.
- **`DeleteStream(name, ifMatch)`:** the same with `config.DeleteStream`; a name that neither the manager nor the text has is `api.ErrNotFound` (`config.ErrNoStream`).
- A per-stream `PUT` or `DELETE` of **the name of an entry the start skipped** is allowed and is how that entry is repaired one stream at a time (Task 6): the edit replaces or removes the bad entry, and if nothing else is wrong the document parses and is applied; the outcome is `created` for a `PUT` (the manager did not have it), and a `DELETE` answers 204.
- **`Reload()`:** `text := s.file.Read()`; `apply(text, false)`. On an error nothing changed: it is logged at `error`, kept as `LastReloadError` with its time, and counted; on success `LastReloadError` is cleared, and a non-empty `restart_required` is logged at `warn` as `restart required: server.rtmp.listen, …`. A reload of a file equal to `applied` answers with every stream `unchanged`.

A change in `server` is therefore never applied while running: `started` does not move, the listeners stay, and `restart_required` says so until the process restarts with the new file. `New` uses `config.ParseLenient`, then resolves an empty `transcoder.binary` **once**: `ostranscoder` next to the executable when it is there, else none; the result goes into the manager's `Env` and nowhere else, so `started`, `Resolved()` and `Diff` only ever compare what the file says (`""` with `""`), and a binary that vanishes later cannot show up as a false `restart_required`. Then `Server.CheckFiles` on the resolved section (a transcoder binary or assets directory that is missing at start is a start-up error; `apply` never asks again, so one that disappears later fails no edit), and `Manager.Load`; what they skipped or left stopped is `LoadErrors` (each also logged at `error`). While the file holds an entry the start skipped, `apply` fails with those errors for every entry point (R-D4); `PUT /api/v1/config` with a fixed document is how an editor repairs it without a shell.

Wiring, following osdev's `run`: the media mux has `GET /live/{stream}/stream.ts` → `httpts.Handler(func(n) { return m.Hub(OutHTTPTS, n) }, 0)` and `/live/` → a gate in front of `live.Handler`: for `/live/{stream}/{dir}/{file}`, 404 unless `m.Packager(out, stream)` finds a packager for one `out` of `LiveOutputs(dir, file)`; the `live.Handler` behind it looks the packager up with either output. RTMP play registers `rtmpplay.New(lookup(OutRTMP), …)` on `rtmpsrv.Default` (the registry ingest's listen inputs share), on a concrete address: `resolveRTMP(addr string) (string, error)` returns `addr` unchanged when its port is not 0 and otherwise does R-D13's bind, read, close; **when the configured port is 0, `New` retries the pair resolve-and-register up to 5 times on any error of `RegisterPlay`**, not only "address in use": two servers of one process share `rtmpsrv.Default`, and the second to be given the same free port gets "already registered". With a configured port there is no retry. RTSP and SRT start as osdev's `startRTSP` and `startSRT`, on listeners `New` binds itself. `New` then fills the manager's `Env` from what was bound: `RTMPPlay` (the concrete address) and `RTMPApp`, `Transcoder` and `AssetsDir` from the resolved config, and `Reserved` with one `stream.Claim` per listener, owner `server`: HTTP, API and RTSP as TCP claims, RTMP as `{RTMP: true}` with an empty path, SRT as a UDP claim, each with the host normalised (a wildcard is `""`) and the real port. The media server has osdev's `newServer` limits (no read or write timeout: HTTP-TS responses are long-lived); the API server has `ReadHeaderTimeout` 10 s, `ReadTimeout` 30 s, `WriteTimeout` 60 s, `IdleTimeout` 2 min, `MaxHeaderBytes` 64 KiB. The metrics registry gets `metrics.Process`, `Manager.Collect`, `API.Collect`, `openstreamer_build_info{version,goversion} 1`, and from `collect.go`:

| Family | Type | Labels | Source |
|---|---|---|---|
| `openstreamer_config_load_errors` | gauge | | the number of `LoadErrors` |
| `openstreamer_config_reloads_total` | counter | `result` = `ok`, `error` | `Reload` |
| `openstreamer_config_restart_required` | gauge | | 1 while `restart_required` is not empty |
| `openstreamer_output_sessions` | gauge | `output` = `rtmp`, `rtsp`, `srt` | `Players`, `Players`, `Conns` |
| `openstreamer_output_sessions_started_total` | counter | `output` = `rtmp`, `rtsp` | `PlayersStarted` |
| `openstreamer_output_sessions_ended_total` | counter | `output`, `reason` | each `Ended*` field, the reason being the field name after `Ended` in snake case (`hub_closed`, `write_error`, …) |
| `openstreamer_output_resyncs_total` | counter | `output` | `Resyncs` |
| `openstreamer_output_rejected_total` | counter | `output` = `srt`, `reason` = `unknown`, `mode`, `caps`, `mss` | `Rejected*` |

Shutdown order when `ctx` ends: stop accepting API requests (`Shutdown`, 2 s, then `Close`); cancel the manager's context and wait for `Manager.Run` (every hub closes); unregister RTMP play, stop RTSP, stop SRT (each waits for its sessions, which ended with their hubs); shut the media server down (2 s, then `Close`). A listener that fails after start-up ends `Run` with that error, after the same stop.

**`cmd/openstreamer`.** `func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, hup <-chan os.Signal) int`. `main` passes `os.Stdin`, `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)` and, on Unix, `hup := make(chan os.Signal, 1)` with `signal.Notify(hup, syscall.SIGHUP)` (`nil` elsewhere: use `POST /api/v1/reload`). **SIGHUP rules:** the channel has a buffer of exactly 1, so signals that arrive during a reload coalesce into one pending reload and none is lost (with no buffer the runtime would drop a signal that arrives while the loop is inside `Reload`; the default action of an unhandled SIGHUP would end the process); `signal.Notify` is called **before** `server.New`, so a HUP during start-up is kept, not fatal; `signal.Stop` is never called (between it and the exit a HUP would kill a process that is shutting down cleanly); and the loop that reads `hup` selects on `ctx.Done()` as well and stops reading once it is closed; **after a receive from `hup` it checks `ctx.Err()` before it calls `Reload`**, because `select` picks at random when both cases are ready, so a HUP that was pending at shutdown could otherwise still start a reload.
- `openstreamer -config path` prints one line per bound listener to stdout (`http <addr>`, `api <addr>`, `rtmp <addr>`, `rtsp <addr>`, `srt <addr>`), then runs; each value from `hup` calls `Reload` (one at a time, in the loop's goroutine) and logs its outcome. Exit codes: 0 after a clean stop, 1 when `Run` returns an error, 2 for a usage, config or start-up error (printed to stderr, scrubbed).
- `openstreamer -version` prints the version.
- `openstreamer hash-password` takes **no argument** (any is a usage error, exit 2, and is not echoed back: it may be a password). When stdin is a terminal it writes `Password: ` to stderr, reads a line with echo off, writes `Again: `, reads again, and fails on a mismatch; echo is restored on every path: a normal return, an error, and SIGINT or SIGTERM while it waits (it catches them for the length of the read, restores the terminal, and exits 130 or 143). When stdin is not a terminal it reads at most 1024 bytes and strips one trailing `\n` or `\r\n`. A password under `auth.MinPasswordBytes` is refused. It prints the hash and a newline to stdout, exit 0. `tty_linux.go` and `tty_darwin.go` implement `isTerminal(fd)` and `readNoEcho(fd)` with `syscall` termios ioctls (`TCGETS`/`TCSETS`; `TIOCGETA`/`TIOCSETA`); `tty_other.go` reports a terminal by `os.ModeCharDevice` and refuses it with `hash-password: echo cannot be turned off on this system; pipe the password on stdin`.

`openstreamer.example.yaml` is a commented example: `version: 1`; `server` with `http.listen` `:8080`, `api.listen` `127.0.0.1:8081`, `rtmp.listen` `:19350`, `rtsp.listen` `:5554`, `srt.listen` `:9999`; one editor and one viewer whose `password` is the placeholder text `REPLACE-WITH-OUTPUT-OF-openstreamer-hash-password` (so the example does not load until it is replaced, and ships no working credential); one stream. **It never names 1935, 554 or 8554**, and no test starts a server from it.

- [ ] **Step 1: Write the failing tests.** `testConfig(t, mutate)` writes a config file (mode 0600) into `t.TempDir()` with every listener on `127.0.0.1:0`, users `ops` (editor, password `editor-password-1`) and `eye` (viewer, `viewer-password-1`) hashed at 1000 iterations, and `Options{MinIterations: 1000}`; it returns the path and the started server. No test of `internal/server` calls `t.Parallel`.

```go
func TestLiveOutputs(t *testing.T) // hls/index.m3u8, hls/3.ts, hls/v1.m3u8 → [hls]; cmaf/master.m3u8, cmaf/a1.m3u8 → [hls]; cmaf/index.mpd → [dash]; cmaf/init-0-1.mp4, cmaf/0-7.m4s → [hls, dash]; x/y → nil
func TestServerStartsAndStops(t *testing.T)
// New binds five listeners with five different ports above 0 (rtmp.listen 127.0.0.1:0 included), and a TCP dial to
// Addrs().RTMP connects; /healthz on the API address is 200; the media address answers 404 for an unknown stream;
// cancelling ctx makes Run return nil within 5 s. Afterwards every address can be bound again by the test: a TCP listen on
// the HTTP, API, RTMP and RTSP addresses and a UDP listen on the SRT address all succeed (a refused connection would prove
// nothing for UDP); and runtime.NumGoroutine is back within 2 s.
func TestResolveRTMP(t *testing.T) // "127.0.0.1:0" → a 127.0.0.1 address with a port above 0 that can be bound; "127.0.0.1:19351" and ":19351" come back unchanged; "nope" is an error
func TestTwoServersOneProcess(t *testing.T) // two servers started together, both with rtmp.listen 127.0.0.1:0, 10 times: both start, with different RTMP ports
func TestServerListenersOff(t *testing.T) // rtmp, rtsp and srt listen "" → Addrs has "" for them and a stream with those outputs is still accepted and served over HLS
func TestServerLiveGate(t *testing.T)
// a stream from a looped fixture with hls on and dash off: hls/index.m3u8 and cmaf/master.m3u8 are 200, cmaf/index.mpd is 404,
// the init and a segment are 200; with only dash: the reverse, segments still 200; with neither: all 404; stream.ts is 404
// while httpts is off and streams while it is on.
func TestServerReserved(t *testing.T)
// through the API: a stream with a udp:// listen input on the SRT port, and ones with an RTMP listen input on the HTTP, the
// API and the RTSP port, are each refused with 409; one with rtmp://<Addrs().RTMP>/in/key?mode=listen is accepted (it shares
// the play listener) and one publishing at live/anything there is refused with 422.
func TestStartIsLenient(t *testing.T)
// a file with a good stream, a stream "bad" with an unknown key, a stream "oct" with `height: 0720`, a stream "ali" that is
// an alias of an anchored one, and two streams on one UDP port: New succeeds; the good stream and the first of the pair
// run; config/status has load_errors for bad, oct, ali and its anchored source (each `streams[i] (name): line N: …`) and
// for the second of the pair, left stopped, and openstreamer_config_load_errors is 5. A file with `latency_ms: 0120` in
// server does not start. While "bad" is in the file, a per-stream PUT of another stream is
// refused with 422 naming the bad entry. Three repairs, each on a fresh server: PUT /api/v1/streams/bad with a valid
// document → 201 "created" and the file parses; DELETE /api/v1/streams/bad → 204; a PUT /api/v1/config with the entry
// fixed. After each, the other per-stream PUT succeeds. A file whose users have no editor, whose server section is bad, or
// whose transcoder.binary does not exist, makes New fail, without printing a hash.
func TestBinaryGoneLaterBlocksNoEdit(t *testing.T)
// started with a transcoder.binary that exists; the test deletes it; a per-stream PUT of a passthrough stream, a
// PUT /api/v1/config and a Reload all still succeed (apply does not look at the file system).
func TestApplyPathsAgree(t *testing.T)
// the same edit (cam's rtsp output off) made three ways on three servers started from the same file: PUT /api/v1/config
// with the edited text, PUT /api/v1/streams/cam, and a hand edit of the file followed by Reload. In all three: the outcome
// of cam is "outputs" and of the other stream "unchanged", cam's generation is unchanged, and config.Parse of the file on
// disk gives the same Document.
func TestPutConfigWritesExactBytes(t *testing.T)
// a text with comments, blank lines and an unusual indentation: after PutConfig the file is byte-equal to it and
// ConfigText returns it. A second PutConfig of the same bytes does not rewrite the file (its mtime is unchanged) and
// answers every stream "unchanged".
func TestPutStreamKeepsComments(t *testing.T) // after PutStream(cam): the header comment, the comment on a user and the comment above the other stream are still in the file
func TestPutConfigPreconditions(t *testing.T)
// no If-Match → ErrPreconditionRequired; an old ETag → ErrPrecondition; after a hand edit of the file, a current ETag →
// config.ErrStale (the stale check comes first), and with an old ETag as well → still config.ErrStale. In each case the
// file and the running streams are untouched.
func TestEditKeepsRememberedLogin(t *testing.T)
// the editor logs in (one PBKDF2, by auth's hook), then does a per-stream PUT, a PUT /api/v1/config that changes a stream
// only, and a Reload of an unchanged users section: no further PBKDF2 for any of its requests, and the *auth.Users pointer
// is the same. A PUT /api/v1/config that changes the viewer's hash replaces the set: the editor's next request costs one.
func TestClientGoneDuringRebuild(t *testing.T)
// a per-stream PUT that rebuilds a stream whose teardown the test holds; the test cancels the request's context after the
// file was written. Then: ConfigText() and its ETag equal the file on disk; config.Parse(file) holds the new spec and so
// does Manager.Get; a following PutStream of another stream keeps the first edit (nothing is reverted); when the teardown
// is released the stream runs the new spec.
func TestStatusDoesNotWaitForApply(t *testing.T) // while an apply waits for a held rebuild: ConfigStatus() and ConfigText() return within 100 ms
func TestStaleIsLive(t *testing.T)
// no apply, no reload, nothing published in between: ConfigStatus().Stale is false; the test rewrites the file by hand and the
// very next call says true; writing the old bytes back says false again; removing the file says true. A chmod alone says false.
func TestDirSyncFails(t *testing.T)
// the directory fsync fails after the rename (config's hook): PutStream returns an error (a 500 to a client), no stream
// changed, config/status says stale, the next write is config.ErrStale, and Reload then applies the text that is on disk.
func TestHandEditThenWrite(t *testing.T)
// the test rewrites the file by hand (a comment added): PutStream, DeleteStream and PutConfig are each config.ErrStale and
// the hand edit is byte-for-byte intact; config/status says stale; after Reload they work, and the comment is still there
// after a PutStream.
func TestReloadAppliesDiff(t *testing.T)
// a, b, c running, a tap on a's hub. The file is edited by hand: b's outputs changed, c removed, d added, a user's hash
// replaced. Reload: outcomes a unchanged, b outputs, c deleted, d created; a's generation and hub pointer are unchanged and
// the tap saw no Lost; the old password is refused and the new one accepted.
func TestReloadInvalidKeepsState(t *testing.T)
// one row per kind of bad file: not YAML; an unknown key; version 2; no editor; a plaintext password; a stream failing
// Validate; two streams of one name; a stream whose Check fails; a claim on a Reserved port; a `- *cam` alias; height 0720;
// `latency_ms: hunter2` (the error and the status do not contain "hunter2"); the file chmodded to 0644. Reload returns the error; every
// stream keeps its generation; the old users still log in; config/status has last_reload_error (without any hash or
// password); stale is true for every row that changed the file's bytes and false for the chmod row (the bytes are the
// applied ones; only the reload fails); openstreamer_config_reloads_total{result="error"} rose by 1. Restoring the old file and
// reloading clears the error.
func TestRestartRequired(t *testing.T)
// a hand edit that changes server.rtmp.listen and server.log.level, and one stream's outputs: Reload applies the stream
// edit, answers restart_required ["server.log.level", "server.rtmp.listen"], logs "restart required" once, and Addrs() is
// unchanged; the gauge is 1. The same through PutConfig. Putting the server section back clears it.
func TestConcurrentWriters(t *testing.T)
// 8 goroutines of PutStream on different streams, 2 of PutConfig (each reading ConfigText and its ETag first, retrying on
// ErrPrecondition) and 1 of Reload, for 2 s under -race: no deadlock; the file always parses when read between writes; at
// the end config.Parse(file).Streams equals Manager.List(), and no temp file is left.
func TestServerMetricsParse(t *testing.T)
// with one running stream and one RTMP player (conform.ReadRTMP for 1 s): metricstest.Parse accepts /metrics (as the
// viewer); it has openstreamer_build_info, go_goroutines, openstreamer_stream_up{stream="cam"} 1,
// openstreamer_output_sessions_started_total{output="rtmp"} 1 and openstreamer_config_load_errors 0.
func TestServerBindFailure(t *testing.T) // http.listen on a port a test listener holds: New returns an error naming the address and leaves no other listener bound
func TestRun(t *testing.T)
// in cmd/openstreamer: -version prints the version and returns 0; no -config, a missing file, a 0644 file (Unix) and a bad
// config return 2 with a message on stderr that holds no hash; with a good config the five address lines appear on stdout,
// /healthz answers, a value sent on hup reloads (a hand-edited stream appears), and cancelling ctx returns 0.
func TestRealIterations(t *testing.T)
// the product path once, without MinIterations: a config whose editor's hash was made by HashPassword (600000 iterations)
// loads through run, and that user gets 200; a 599999-iteration hash makes run return 2.
func TestHashPassword(t *testing.T)
// stdin "correct horse battery\n" → exit 0 and stdout is one line that auth.ParseHash accepts and that verifies the
// password without the newline; "\r\n" is stripped too; "short\n" → exit 2; an argument (`hash-password hunter2-secret`) →
// exit 2 and neither stdout nor stderr contains "hunter2-secret"; empty stdin → exit 2.
func TestSIGHUP(t *testing.T)
// Unix (sighup_unix_test.go), through main's own set-up (a helper shared with main that makes the buffered channel and
// registers it): syscall.Kill(os.Getpid(), SIGHUP) after a hand edit makes the edited stream appear within 5 s. Coalescing:
// with a reload made slow (a rebuild whose teardown the test holds), the test hand-edits the file again and sends 3 HUPs
// while that reload is still running; the process survives, and after the release at least one further reload runs, the
// final file is the config in force (ConfigText equals it) and config/status is not stale. A HUP sent before server.New has
// returned (the helper registers first) does not kill the process. After ctx is cancelled a HUP starts no reload
// (openstreamer_config_reloads_total does not move) and run returns 0.
func TestHashPasswordTTY(t *testing.T)
// Linux (tty_linux_test.go): a pty pair opened through /dev/ptmx with syscall; the slave is hash-password's stdin. While it
// waits for the line the slave's termios has ECHO off; after two equal lines the hash is printed and ECHO is on again; two
// different lines → exit 2 and ECHO is on again. SIGINT while it waits for the first line (sent to the test process, which
// the command's handler catches for the length of the read): ECHO is on again and the exit code is 130.
```

- [ ] **Step 2: Run** `go test -race -count=1 ./internal/server/ ./cmd/openstreamer/` — compile failure.
- [ ] **Step 3: Implement** the server and the apply path, then `main.go` with `hash-password`, the example config and the make target.
- [ ] **Step 4: Run** the same command, `make openstreamer` (a binary at `bin/openstreamer`; `bin/openstreamer -version` prints a version; `printf %s 'a long test password' | bin/openstreamer hash-password` prints a hash), `GOOS=windows go vet ./...` and `GOOS=darwin go vet ./...` — pass.
- [ ] **Step 5: Commit** `feat(server): the openstreamer binary: one config file, one apply path, reload and hash-password`

### Task 11: The M5a scenarios

**Files:**
- Create: `internal/e2e/m5a_test.go`

**Interfaces:**
- Consumes: `server.New`, `server.Options`, `Server.Addrs`, `Server.Manager` (in-process only for the tap and the packager identity: `Manager().CopyHub`, `Manager().Packager`), `Server.Reload`, `config.Parse`, `metricstest`, `conform.ReadHLS`/`ReadDASH`/`ReadTS`/`ReadRTMP`/`ReadRTSP`, `testpeer`, and the package's existing helpers, used as they are: `recordHub` (m4a), `assertMatchesHub` (m3b1), `assertRTSPMatchesHub` (m3b2), `assertTSMatchesHub`, `tsFrames`, `tsGaps`, `conformBytes`, `rawHTTP`, `rawSRT` (m3b3), `kindCounts` (m3a).
- Produces, for later milestones' tests: `m5aServer(t, streams string) *m5a` (it writes a config file, mode 0600, into `t.TempDir()`: a commented template with every listener on `127.0.0.1:0`, the users `ops` (editor) and `eye` (viewer) hashed at 1000 iterations, and the given `streams:` YAML; starts `server.New(path, server.Options{MinIterations: 1000})` with a captured log buffer; stops it by `t.Cleanup`), `(*m5a).put(t, sp spec.Stream) (code int, applied string)` (the per-stream `PUT`), `(*m5a).del(t, name string)`, `(*m5a).get(t, path string) (int, []byte)`, `(*m5a).configText(t) (text, etag string)`, `(*m5a).putConfig(t, text, etag string) (code int, res api.ApplyResult)`, `(*m5a).handEdit(t, edit func(string) string)` (rewrites the file on disk, as an operator's editor would), `(*m5a).reload(t) api.ApplyResult` (`POST /api/v1/reload`), `(*m5a).as(user, password string) *m5a` (the same server with other credentials; the default is the editor; every helper sends `X-Requested-By: e2e` on writes), `(*m5a).metric(t, name string, labels ...string) float64` (a scrape through `metricstest.Find`), `(*m5a).url(out stream.Output, served string) string` (the play URL of an output: `http://…/live/<n>/hls/index.m3u8`, `…/cmaf/index.mpd`, `…/stream.ts`, `rtmp://…/live/<n>`, `rtsp://…/live/<n>`, `srt://…?streamid=live/<n>`), `(*m5a).tap(t, served string) *hubRecorder` (`recordHub` on `Manager().CopyHub(served)`, **polled every 5 ms for up to 5 s**: a `created` answer comes back before the supervisor has published its first View, so the hub may not be there yet), and `m5aSpec(name, input string) spec.Stream` (every output on, no push).

The source is `file://<abs testdata/fixtures/h264_aac.ts>?loop=1`: a file input is paced in real time and each 6 s loop is one epoch change, which every reader below must also survive. The tests drive the server over HTTP with Basic credentials; the in-process access is the tap and the packager's identity, and a hand edit is a write to the file.

**Measuring "not interrupted" (R-D12).** A row creates the stream, opens the tap (before any reader, so every reader joins at or after the tap's first keyframe), starts one reader per output, waits until they are connected, takes the counter baseline, makes its two edits (recording the wall time at which each `PUT` answered), waits for the segmented readers to return, checks that nothing ended by itself, and then **ends the stream with `DELETE`**. That last step is what makes a frame-exact comparison possible: the hub closes, each session reader gets its end of stream after the queued frames, and the tap ends on the same frame, so the capture and the tap are aligned at their ends, which is the contract of M3's helpers (`joinKey`: "received n video frames up to the hub's end"). No reader has to be stopped from outside, so `rawHTTP` and `rawSRT` are used as they are. The tap must be whole: `hubRecorder.lost == 0`, and no epoch without an anchor.

*Connected* means: `openstreamer_output_sessions` is 1 for `rtmp`, `rtsp` and `srt`, and `openstreamer_hub_subscribers{name="cam"}` equals the tap, the four session readers, the packager and the pusher, when present. **The baseline is read after that**, so the readers' own connections are in it: `openstreamer_output_sessions_started_total` for `rtmp` and `rtsp`; `openstreamer_output_sessions` for `rtmp`, `rtsp` and `srt`; `openstreamer_output_resyncs_total` for `rtmp`, `rtsp` and `srt`; `openstreamer_hub_subscribers{name="cam"}`; the status's `generation`, `input_switches`, `restarts`, `rebuilds` and `pushes[0].connects`; and with a ladder `transcoder.pid` and `transcoder.spawns`.

**Timing of the segmented readers.** `ReadOptions.Duration` is media time, not wall time: `ReadHLS` and `ReadDASH` return once one playlist holds that much, and at the reader's start the window may already list up to 6 segments of 2 s. With the edits at 5 s and 10 s after the readers start, `Duration` is **26 s** (10 s to the second edit, 12 s the window can already hold, 4 s of margin), so a segmented reader returns between 14 s and 26 s after its start: always after the second edit, whatever the window held. The row does not rely on that arithmetic: it records the wall time of each reader's return and **requires it to be later than the second `PUT`'s answer**, and it sends the `DELETE` only after every segmented reader has returned (the session readers keep running until then).

**Nothing ended by itself.** Ending the stream would hide a reader that had ended earlier. So immediately before the `DELETE`, the row asserts that the result channel of every session reader it did not touch is still empty (a non-blocking receive), and reads the counters for the comparison below.

| Output | Reader | Capture → tap, and the tolerance | Rule |
|---|---|---|---|
| RTMP | `conform.ReadRTMP`, to the end of the stream | FLV timestamps restart at each epoch and are milliseconds: `assertMatchesHub`'s mapping, DTS and PTS `floor((t − b)/90)` ms from the join keyframe's DTS `b`, exact, with the capture's epochs numbered from the join keyframe's hub epoch | `assertMatchesHub(tap, got)`: every tap video frame from the join keyframe to the end, once, with its key flag, and every audio frame presented from there; no conform violation |
| RTSP (TCP) | `conform.ReadRTSP`, until the BYE | RTP timestamps are re-anchored per epoch: `assertRTSPMatchesHub`'s mapping, video PTS `t − b` exact with the parameter sets, audio PTS within 1 tick | `assertRTSPMatchesHub(tap, got)`; `RTPLost == 0`; `ByeTracks == 2`; no conform violation |
| HTTP-TS | `rawHTTP`, then `tsFrames` | TS carries the hub's timestamps: one constant offset over the whole capture, exact (`assertTSMatchesHub`) | `assertTSMatchesHub(tap, got)`; `tsGaps(bytes) == 0`; `conformBytes` without a violation |
| SRT | `rawSRT`, then `tsFrames` | as HTTP-TS | as HTTP-TS |
| HLS-TS, HLS fMP4, DASH | `conform.ReadHLS` or `ReadDASH`, polling with `Duration` 26 s of media (see the timing above) | **A weaker rule, on purpose.** A segmented output has no session to end with the stream; its poll starts on a window that holds segments from before the tap, stops in the middle of an epoch, and a `DELETE` removes the playlist instead of ending it. So the capture and the tap share neither a start nor an end, and its timeline is the packager's per epoch, not the hub's | (a) no conform violation over the poll, which spans both edits by the recorded times (segment continuity, durations, a discontinuity at every epoch change and nowhere else); (b) inside every capture epoch, every video and audio DTS step is the tap's step for that kind (constant in this fixture; read from the tap): no hole of a frame or more; (c) every capture epoch that is neither its first nor its last holds exactly the per-kind frame counts (`kindCounts`) of a whole tap epoch, and there is at least one such epoch (else the row was too short and fails); (d) in-process, sampled every 200 ms across the edits: `Manager().Packager(…, "cam")` is the same pointer, and the `Seq` of its newest segment never goes back and the set of `Seq` seen is one run increasing by one |

A lost frame in a segmented output's first or last capture epoch is therefore caught by (a) and (b) as a step, not by a count; that is the stated cost in R-D12.

`assertUninterrupted(t, row, except ...stream.Output)` applies the table to every reader not in `except`, then compares the counters read just before the `DELETE` with the baseline. **All are equal, with these exceptions, which belong to the row's own output only** (its long-lived reader ended at the first edit, and the fresh 2 s reader that proved the output works again has come and gone):

| Row | What differs from the baseline |
|---|---|
| `rtmp`, `rtsp` | that output's `sessions_started_total` + 1; its `output_sessions` − 1; `hub_subscribers` − 1 |
| `srt` | `output_sessions{output="srt"}` − 1 (1 in the baseline, 0 now); `hub_subscribers` − 1 |
| `httpts` | `hub_subscribers` − 1 |
| `hls`, `dash` | nothing (the packager stayed) |
| `hls+dash` | nothing (a new packager took the old one's place: the same count) |
| `push` | nothing in the counts (a new pusher took the old one's place); `pushes[0].connects` is 1 for the new pusher |

`output_resyncs_total` is equal in every row: no reader of any output lost a frame.

- [ ] **Step 1: Write the tests** (they fail until they are written right; the code they test exists)

```go
func TestM5aEveryOutput(t *testing.T)
// an empty streams list at start. PUT sink (input rtmp://<Addrs().RTMP>/in/k0123456789?mode=listen, outputs hls) and PUT cam
// (m5aSpec with a push to that URL): both 201 "created", and both are in the config file afterwards. For cam, 8 s of each of hls/index.m3u8, cmaf/master.m3u8, cmaf/index.mpd, stream.ts, RTMP, RTSP and
// SRT: no violation, video and audio frames present, and at least one epoch change survived (the file looped). sink's HLS
// has frames too (the push arrived and the listen input took it). GET cam/status: state "running", active_input 0, two
// tracks, pushes[0].state "publishing" with connects 1. GET /api/v1/streams lists both, redacted (no "k0123456789").
func TestM5aEditOutputs(t *testing.T)
// eight rows, each a parallel subtest with its own m5aServer, its own cam (every output on, a push to its own sink stream)
// and the procedure above: tap; readers; connected; baseline; after 5 s PUT cam with the row's output off → 200 "outputs";
// after 5 s PUT it back on → 200 "outputs" and a fresh 2 s reader of that output gets frames; when every segmented reader has
// returned (each later than the second PUT), the untouched session readers are still running, the counters are read, and cam
// is DELETEd; then assertUninterrupted(t, row, the row's outputs). The edits are per-stream PUTs. Rows and what "off" is:
//   rtsp, rtmp, srt, httpts: that flag false. The row's own reader ends within 2 s of the first edit, and a new session of it
//     is refused until the second (RTSP DESCRIBE 404, RTMP play refused, SRT rejected, GET stream.ts 404).
//   hls: hls false, dash stays on. hls/* and cmaf/*.m3u8 answer 404 at once; the DASH reader is held to the table, including
//     (d): the packager is the same one. dash: the mirror image.
//   hls+dash: both false, then both true. The four session readers are held to the table; the HLS and DASH readers are the
//     row's own. A new packager serves afterwards.
//   push: the push list emptied, then restored. sink's input goes quiet within 5 s and comes back; all seven readers of cam
//     are held to the table; pushes[0].connects is 1 before and 1 after for the new pusher.
// In every row but push, cam's pushes[0].connects is 1 at the end: the pusher kept its one connection for the whole row.
func TestM5aRebuildIsTheOtherRoute(t *testing.T)
// the contrast that keeps the test above honest: with an RTMP reader and the tap running, PUT cam with a second input → 200
// "rebuild"; status.generation is 2; the reader ended and the tap ended (the old hub closed), and assertMatchesHub(tap, got)
// still holds for what it got up to the rebuild. Afterwards every output plays again from the new generation.
func TestM5aConfigPut(t *testing.T)
// the done-criterion through the config editor. The file has comments (a header, one above each stream, one inside cam) and
// two streams, cam (every output, a push to sink) and sink, plus a third stream other. With the tap, the reader set and the
// baseline of cam as in TestM5aEditOutputs, and an RTMP reader and a tap on other: GET /api/v1/config (the editor) returns
// the file's bytes; the test changes one line of that text (cam's rtsp: true → false) and PUTs it with the ETag → 200 with
// streams [{cam outputs}, {other unchanged}, {sink unchanged}], restart_required [] and a new ETag. Then, after the segmented
// readers return: assertUninterrupted for cam with rtsp excepted; other's RTMP reader passes assertMatchesHub against its
// tap after the final DELETEs and other's generation never moved; the file on disk is byte-equal to the text sent, every
// comment included. A second PUT with the old ETag → 412; one without If-Match → 428; a text with an unknown key → 422
// listing it, and the file and the streams are untouched.
func TestM5aReload(t *testing.T)
// a hand edit applied by reload. cam and other running, readers and taps as above. handEdit: cam's srt: true → false, a new
// stream extra added, a comment added. Before the reload nothing changed (cam still serves SRT; config/status says stale).
// reload → streams [{cam outputs}, {extra created}, {other unchanged}]; cam's SRT reader ended and its others pass
// assertUninterrupted; other is untouched; extra plays over HLS. Then handEdit to invalid YAML and reload → the error;
// every stream keeps its generation; config/status has last_reload_error; restoring the file and reloading clears it.
// Then handEdit of server.rtsp.listen and reload → restart_required ["server.rtsp.listen"], and RTSP still answers on the
// old address.
func TestM5aHandEditThenWrite(t *testing.T)
// handEdit adds a comment to the file. PUT of a stream, DELETE of a stream and PUT /api/v1/config (with the current ETag)
// are each 409 config_changed; the file still holds the hand edit byte for byte and no stream moved. After reload the same
// three requests succeed, and the hand-made comment is still in the file.
func TestM5aRestartRestores(t *testing.T)
// PUT cam, a disabled stream off, and a third stream; stop the server (ctx); start a second server on the same file:
// GET /api/v1/streams?secrets=1 equals the list before the stop; cam plays over HLS; off is "stopped". Then stop it, add by
// hand a stream entry with an unknown key, and start a third server: the three good streams run, config/status lists the
// bad entry in load_errors, openstreamer_config_load_errors is 1, and the bad entry is still in the file, untouched.
func TestM5aLadder(t *testing.T)
// needs testpeer.OSTranscoder. cam with rungs 360 (x264, 360 p, 800 kbps) and 180 (180 p, 300 kbps), AAC audio, a thumbnail
// every 2 s, every output. Taps on the rendition hubs, not the source: tap360 = tap("cam_360"), tap180 = tap("cam_180").
// Names: /live/cam/hls/index.m3u8, cmaf/master.m3u8 and cmaf/index.mpd pass conform with AlignedSequences and list 2 video
// variants; /live/cam_360/… and /live/cam_180/… are single-rendition streams on HLS and RTSP; /live/cam_abr/… is 404; the
// API's thumbnail.jpg is a JPEG and /live/cam/thumbnail.jpg is 404; /metrics has
// openstreamer_transcoder_encoder_fallbacks_total{stream="cam"} 0 and openstreamer_transcoder_up{stream="cam"} 1.
// Edit: with an RTMP reader on live/cam, an HLS-TS reader on /live/cam/hls/index.m3u8 and the baseline taken, PUT with dash
// off → "outputs" (the HLS reader's Duration is 26 s; the DELETE follows its return, which must be later than the PUT). Then: assertMatchesHub(tap360, rtmp) (live/cam is rendition 0, frame for frame); the
// ABR capture meets the segmented rule per variant, variant i against rendition i's tap (the steps and whole-epoch counts of
// tap360 for the first video track, tap180 for the second, as assertPresentationIsRenditions pairs track i with rendition
// i), and (d) for Packager(OutHLS, "cam"); transcoder.pid and transcoder.spawns equal the baseline: the child was not touched.
func TestM5aPeers(t *testing.T)
// needs ffprobe with the protocols (testpeer.Require): ffprobe -show_streams on each of cam's seven URLs reports one h264 and
// one aac stream. Runs on the host and in make test-ci-peers.
func TestM5aNoSecretsLeak(t *testing.T)
// cam2 with inputs rtsp://user:hunter2-pw@127.0.0.1:1/x (refused at once) then the file, and a push to
// rtmp://127.0.0.1:1/live/pushkey-0123456789 (refused). After 5 s: none of "hunter2-pw", "pushkey-0123456789" and the API
// users' passwords and password hashes appears in the log buffer, in /metrics, in GET cam2, in the list, in cam2/status
// (whose last_error and pushes[0].last_error are not empty), in config/status, or in a 422 answer to a PUT with a bad URL
// carrying the password, for the viewer and the editor alike. Every log line is one JSON object with time, level and msg.
// The deliberate exceptions, for the editor only: GET cam2?secrets=1 returns both stream secrets, and GET /api/v1/config
// returns them and the hashes. On disk the file holds both stream secrets verbatim (config.Parse of it gives the URLs as
// they were PUT), which is what makes a restart possible.
func TestM5aRoles(t *testing.T)
// without credentials every /api/, /metrics and /debug/pprof/ URL is 401 with WWW-Authenticate `Basic realm="openstreamer"`
// and /healthz is 200. The viewer reads streams, status, config/status and /metrics (as the soak's Prometheus will, with
// basic_auth) and gets 403 for a stream PUT and DELETE, GET and PUT of the config, reload, ?secrets=1 and pprof; the editor
// does all of them. A write without X-Requested-By is 400. Six wrong passwords for "eye" give 429 with Retry-After on the
// sixth, while "ops", from the same address, is still served. A PUT of the config
// that replaces the editor's hash makes the old password 401 and the new one 200 on the next request, and one that leaves
// no editor is 422. The media listener serves /live/ without credentials and has no /api/ route (404).
```

- [ ] **Step 2: Run** `go test -race -count=1 -timeout=10m -run '^TestM5a' ./internal/e2e/` on the host — pass (`TestM5aLadder` needs `OS_TRANSCODER`: run `make ostranscoder` first and set it to `dist/ostranscoder-amd64/ostranscoder`). If a table rule fails on a reader that the edit did not touch, the finding is a defect in `reattach` or in the output, not a reason to loosen the rule: stop and report it.
- [ ] **Step 3: Time budget.** `make test` runs every package under one `-timeout=10m` per package. The M5a scenarios may add at most **240 s** to `./internal/e2e/` (`TestM5aEditOutputs`' eight rows run in parallel, about 35 s of wall time together, the segmented readers' 26 s of media setting the pace; the others run one after another: about 15 s for `EveryOutput`, 35 for `ConfigPut`, 30 for `Reload`, 5 for `HandEditThenWrite`, 12 for `RebuildIsTheOtherRoute`, 15 for `RestartRestores`, 35 for `Ladder`, 15 for `Peers`, 8 for `NoSecretsLeak` and 3 for `Roles`). Measure it: `go test -race -count=1 ./internal/e2e/` before and after, and record both wall times in the commit message. If the package then takes more than 8 min under `-race`, mark the top-level `TestM5a*` tests parallel as well (each has its own server and ports) and measure again.
- [ ] **Step 4: Run** `go test -race -count=5 -timeout=20m -run '^TestM5a(EditOutputs|EveryOutput|ConfigPut|Reload)$' ./internal/e2e/` — 5 of 5 (about 10 min); then once more while `make test` runs beside it, to see the scenarios under load.
- [ ] **Step 5: Run** `make test-ci-peers ARGS='-timeout=15m -run ^TestM5a ./internal/e2e/'` — pass in `osv2-ci-peers:m4a`.
- [ ] **Step 6: Commit** `test(e2e): the M5a scenarios`

### Task 12: CI, docs, spec, done

**Files:**
- Modify: `.github/workflows/ci.yml`, `.testcoverage.yml`, `README.md`, `CLAUDE.md`, `docs/REBUILD_PLAN.md`

- [ ] **Step 1: CI.** Add `TestM5aLadder` to `E2E_TESTS` of the `transcode-e2e-arm64` job (its "Check the tests exist" step then guards the name). The amd64 peers job runs `./internal/e2e/` in full already; check that its run lists `TestM5aEveryOutput`, `TestM5aEditOutputs`, `TestM5aPeers` and `TestM5aLadder` as run, not skipped. Add a step to the untagged test job: `go build -o /dev/null ./cmd/openstreamer` on `GOOS=linux`, `windows` and `darwin`.
- [ ] **Step 2: Coverage.** Run `make cover && make covercheck`. `cmd/openstreamer`'s `tty_*.go` terminal paths are covered on Linux by `TestHashPasswordTTY`; if `tty_darwin.go` and `tty_other.go` fall under the file threshold on CI's Linux, exclude exactly those two files with the comment "platform files: not compiled on the CI's Linux". Exclude `^internal/metrics/metricstest` with the comment "test-helper package: it is exercised by the internal/metrics, internal/stream, internal/api and internal/e2e tests, which go test does not count towards it without -coverpkg". No other exclusion: `cmd/openstreamer` is covered through `run`.
- [ ] **Step 3: README and CLAUDE.md.** README: a section "Running the server" (`make openstreamer`; the example config and its three sections; `openstreamer hash-password` and the two roles; the file's mode 0600 and that the service must own its directory, and in a container mount that directory, not the single file (a bind-mounted file cannot be renamed over); that anchors, aliases and merge keys are not supported, that numbers are plain decimals, and that text which looks like a number (a stream named `007`, a watermark text `24.7`) must be quoted; `X-Requested-By` on writes; one `curl -u` `PUT` creating a stream with every output, and the whole-config `GET`/`PUT` round trip with `If-Match`; a hand edit and `kill -HUP`, and the 409 that asks for it; the play URLs by output; `/metrics` scraped with `basic_auth` as a viewer; the log format; and that the API stays on loopback until HTTPS because Basic credentials are only base64). CLAUDE.md (the rebuild branch's): the binary, the config file's sections and keys, the package list of this plan, the one apply path (R-D4) and the two routes of an edit (R-D6).
- [ ] **Step 4: `docs/REBUILD_PLAN.md`.**
  - §5's table: replace the M5 row with the three rows below, and change M6's "Depends on" to `M5a`.

    | | Milestone | Depends on | Exit criteria |
    |---|---|---|---|
    | M5a ([plan](superpowers/plans/2026-11-26-m5a-control-plane-core.md), done) | Control-plane core: the stream document (`internal/spec`), one YAML config file for server settings, users and streams (`internal/config`) with atomic rewrites and reload on SIGHUP, the per-stream supervisor with in-place output edits, the manager, the REST API with Basic auth and two roles (`internal/auth`), its whole-config editor and its OpenAPI document, Prometheus metrics (with `EncoderFallbacks`), redacted JSON logs, `cmd/openstreamer` and its config, and the product names of a ladder (R-D3) | M2, M3, M4 | A spec created over the API serves every output; editing outputs does not interrupt other outputs; a restart restores every stream. Proved by `TestM5aEveryOutput`, `TestM5aEditOutputs` (each output switched off and on while a reader of every other output must hold every hub frame once), `TestM5aConfigPut` (the same through the whole-config editor, comments kept), `TestM5aReload`, `TestM5aHandEditThenWrite`, `TestM5aRebuildIsTheOtherRoute`, `TestM5aRestartRestores`, `TestM5aLadder`, `TestM5aPeers`, `TestM5aNoSecretsLeak` and `TestM5aRoles`, with the spec, config, auth, stream, api, metrics, logging and server suites |
    | M5b | Live edits and assets: a running pipeline's inputs changed in place, the watermark edited in place over the API, the asset API (upload, listing, replacement; R-C8) with operator fonts (R-C7), a rung that refuses the x264 fallback (wire version 3), audio language in the spec, the HLS masters and the MPD | M5a | Replacing a running stream's inputs is exactly one epoch change and ends no output session; an uploaded image and font appear on every rung; a strict NVENC rung fails instead of falling back and is counted; `osconform` reads the languages |
    | M5c | Transport security: HTTPS for the API and the HTTP outputs, RTMPS (listen, pull, push), RTSPS and SRTP, SRT encryption (input and output), credentials in URL paths and queries redacted, secrets as write-only spec fields | M5a | ffmpeg publishes and plays over each secured protocol against our server; no secret appears in a response, a log or a metric |

  - After the M4 paragraphs of §5, add "M5 runs in three parts" with R-D1's text, the rulings R-D2 to R-D13 by reference to this plan, and an "Out of M5a" table: inputs in place, the live watermark edit, the asset API, operator fonts and refusing the fallback (M5b); audio language (M5b); every TLS and encryption item and URL path or query credentials (M5c); PATCH and per-field edits (R-D7); histograms and per-stream session counts (R-D9; M6's sessions); applying a `server` change without a restart (R-D4); a user API, more roles, and sessions or tokens instead of Basic auth (R-D8); config history (R-D4).
  - §2: the "Control" row becomes "Declarative stream specs in one YAML config file with the server settings and the users, REST API with Basic auth (viewer and editor), a whole-config editor, reload on SIGHUP, per-stream supervisor, input failover by priority".
  - §3.6: rewrite the four bullets to what was built: the spec and the one config file, its atomic write, reload, stale detection and lenient start (R-D2, R-D4), replacing "The file store writes each spec atomically"; users, roles and password hashes (R-D8); the supervisor's mailbox, `View`, restart policy and the absence of a reconciler (R-D5); "Changing the output set is applied in place; changing inputs is applied in place from M5b; any other change rebuilds the stream's pipeline" (R-D6); the API on `net/http.ServeMux` with the OpenAPI document as JSON, checked by `TestOpenAPIMatchesRoutes` and `TestOpenAPIMatchesSpec` (R-D7).
  - §3.7: in the library table replace the row `HTTP | go-chi/chi/v5` with `HTTP | net/http (ServeMux patterns); no router library (R-D7)` and add `Metrics | our own text exposition, internal/metrics (R-D9)`, `Config file | go.yaml.in/yaml/v3 (R-D2)` and `Password hashing | crypto/pbkdf2, standard library (R-D8)`.
  - §4: add `internal/spec/`, `internal/config/`, `internal/auth/`, `internal/server/`, `internal/metrics/` and `internal/logging/`; remove `internal/store/` (the config file took its place); change `internal/stream/` to "supervisor and manager" and `internal/api/` to "REST API and its OpenAPI document".
  - §3.5's presentation bullet and the two "Out of" rows that say "product naming is M5's" (R-B2): state R-D3's names and that osdev keeps `<name>_abr`.
  - The "Out of" rows that name M5: `Metrics for EncoderFallbacks` becomes done (M5a); the asset API, operator fonts, inputs in place and "several audio languages" name M5b; RTMPS, RTSPS and SRTP, SRT encryption and "credentials in the URL path or query" name M5c.
  - Backlog: in "A fallback to libx264 can overload a host sized for NVENC", record that the count is exported (M5a) and refusing is M5b. Add: "A rebuild or a failure restart starts a new packager, so HLS `MEDIA-SEQUENCE` and the DASH timeline go backwards on the same URL and a player that stayed must reload (R-D5, R-D6); carry the sequence numbers and the availability start across generations"; "A switch that stops serving a ladder's renditions as their own streams (R-D3; saves N packagers)"; "HTTP-TS has no session counters, so `openstreamer_output_sessions` lacks it"; "A panic in one stream's goroutines takes the process down (R-D5): decide on per-stream recovery after the soak"; "A per-stream API edit goes through a YAML node round trip: it drops blank lines, re-indents sequences, normalises the spacing of line comments, and loses the comments inside the edited entry and a line comment on its `name:` (R-D4); a text-level splice of the one entry would keep the rest byte for byte"; "The login limiter's key includes the presented user name, so invented names can keep both verification slots busy and delay a real user's first login (R-D8)"; "Behind a reverse proxy every API client shares one IP for the login limiter (R-D8): a trusted-proxy setting". Add an "M5a deferred minors" heading for the review's leftovers.
- [ ] **Step 5: Run** the gates: `go mod tidy` (no change since Task 6), `go vet ./...`, `golangci-lint run ./...`, `make test`, `GOOS=windows go vet ./...`, `GOOS=darwin go vet ./...`, `make test-transcode`, `make test-ci-peers` — all pass.
- [ ] **Step 6: Commit** `ci: run the M5a ladder scenario on arm64 and build openstreamer for three systems` and `docs: M5a is done; M5 is split into M5a, M5b and M5c`
