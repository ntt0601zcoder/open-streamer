# M5b1: Live Edits and Assets Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A running stream takes three more kinds of edit without a rebuild (its inputs, its watermark, and an asset it uses being replaced), assets (watermark images and fonts) are uploaded, listed, replaced and deleted over the API, a text watermark can use an operator's font, and an NVENC rung can refuse the libx264 fallback, which is then counted.

**Architecture:**
- **`spec.Diff` gets one more class, `ChangeLive`:** the documents differ only in `inputs`, `transcode.watermark` and `outputs`, and at least in one of the first two. The supervisor applies each differing part in place: `Pipeline.SetInputs` (new), `Transcoder.SetWatermark` (exists since M4c) and `reattach` (M5a). Everything else is still a rebuild, so there are still only "in place" and "rebuild", and no per-field routing.
- **`Pipeline.SetInputs`** hands the new URL list to the `Run` goroutine, which keeps every input whose URL stays (its reader, its listen socket and its retry state), closes the ones that go and adds the new ones. Only when the *active* input goes does the stream change epoch, once, at the activation of its successor: the same Clock break a failover makes. The hub, the transcoder and every output session are untouched.
- **`internal/asset`** is the asset directory behind an API: names by kind, content checked against the name, sizes and a quota, atomic writes. `internal/api` serves it; `internal/server` keeps a delete and a stream edit apart with the apply mutex and tells the running streams that use a replaced asset to load it again (`Manager.RefreshAsset`).
- **Wire version 3**, once, for both transcoder changes: a watermark carries font bytes, which the child writes where it wrote the embedded DejaVu Sans, and a rung carries `no_fallback`; a child that cannot open NVENC for such a rung ends with a typed fatal `Error`, which the host counts.

**Tech Stack:** Go 1.26.11, standard library only for everything new (`net/http`, `os`, `crypto/sha256`, `image/png`, `image/jpeg`, `encoding/binary`, `os/exec`). Existing: the v2 packages `spec`, `stream`, `pipeline`, `transcode` (+ `wire`, `picture`, `native`, `transcodetest`), `api`, `server`, `metrics`, `conform`, `testpeer`; testify; peers ffmpeg/ffprobe.

**Spec:** `docs/REBUILD_PLAN.md` (§3.5 Transcoder, §3.6 Control plane, §3.7 Libraries, §5 row M5b and every line that names M5b: `grep -n "M5b" docs/REBUILD_PLAN.md`, the Backlog items "Operator fonts for the watermark" and "A fallback to libx264 can overload a host sized for NVENC", and "M5a deferred minors"). Predecessor and format exemplar: `docs/superpowers/plans/2026-11-26-m5a-control-plane-core.md` (its rulings R-D1 to R-D13 stay in force). `CLAUDE.md` describes the M5a packages as they are.

## Global Constraints

- **Libraries:** **no new module.** Only the approved independent libraries (mp4ff, pion/rtp, pion/sdp, datarhei/gosrt, testify, go-astiav v0.41.0, protobuf `protowire`, `go.yaml.in/yaml/v3`); never bluenviron or lal; no code copied from another product. The upload is a raw request body read with `net/http` (no multipart). Fonts are checked by our own sfnt table-directory check and by the transcoder child's FreeType (R-E8), not by `golang.org/x/image`. **One file is added that is not ours:** `testdata/fixtures/fonts/DejaVuSansMono.ttf` (DejaVu Sans Mono 2.37, from Ubuntu's `fonts-dejavu-core`, the package and licence of the embedded `internal/transcode/native/font/DejaVuSans.ttf`; its `LICENSE` is copied beside it), a test fixture only (R-E8). After each wave merge, `go mod tidy` changes nothing.
- **cgo:** only `internal/transcode/native` and `cmd/ostranscoder`, behind the tag `libav`. Without the tag, `go build ./...`, `go vet ./...`, `golangci-lint run ./...` and `make test` pass, as do `GOOS=windows go vet ./...` and `GOOS=darwin go vet ./...`. `internal/asset` and `internal/transcode/picture` stay pure Go; Unix-only code (directory `fsync`) sits in `_unix.go`/`_other.go` files.
- **Frozen behaviour:** everything M5a delivered keeps working, and every existing test passes unchanged, except: (1) an input-only or watermark-only edit of a running stream now answers `live` where it answered `rebuild` (the tests that used such an edit *as* a rebuild move to another field, named in Tasks 8 and 12); (2) the wire version is 3, so the tests that pin `2` change, and a version-2 child is refused as a version-1 child was; (3) an exit error of a child that sent a fatal `Error` ends with that message (Task 6); (4) `TestManagerConcurrentReplaces` is rewritten (Task 1). An outputs-only edit still answers `outputs`. A watermark without a font renders exactly as before (DejaVu Sans); a rung without `no_fallback` falls back exactly as before.
- **Time:** 90 kHz everywhere. Nothing here touches a timestamp except the one Clock break of R-E3.
- **Lint:** `exhaustive` with `default-signifies-exhaustive: false` (every `switch` on `spec.Change` lists `ChangeLive`; on `asset.Kind` both kinds); `godot`; gofumpt.
- **Errors:** wrap as `fmt.Errorf("pkg: op: %w", err)`; never both log and return.
- **Goroutines:** every spawned goroutine has an explicit cancel path and an owner who waits for it. `SetInputs` starts none of its own: the readers it opens are the pipeline's attempts, which `Run` waits for.
- **Secrets:** no input URL, stream key or password reaches a log, an error, a status or a metric label. `SetInputs` errors never quote a URL; asset names are not secrets, asset bytes are never logged.
- **Security of the asset API (R-E5, R-E6):** a name must match the asset rule before it is joined to the directory, so it cannot leave it; the content, not the name or the request's `Content-Type`, decides what a file is, and must agree with the name; bodies are bounded per kind before they are read (1 MiB image, 24 MiB font) and the directory by a quota (256 files, 512 MiB); writes and deletes are editor-only and need `X-Requested-By`, as every write; a write is a temp file, `fsync`, rename, directory `fsync`; responses carry `nosniff` and `Content-Disposition: attachment`.
- **Peers:** every test that runs ffmpeg/ffprobe passes on the host (ffmpeg 8) **and** in `make test-ci-peers` (image `osv2-ci-peers:m4a`: ffmpeg 6.1, VLC 3.0.20, GStreamer 1.24) before any push. The transcoder bundle is rebuilt with `make ostranscoder` after each of the merges of waves 1 and 2, before any real-child test runs (a version-2 bundle is refused by a version-3 host). NVENC device tests stay opt-in (`OS_NVENC=1`).
- **Test time:** `make test` and `make cover` use `-timeout=20m` and `internal/e2e` is near it on CI. The M5b1 scenarios are `t.Parallel()` and together take at most 90 s on this host (R-E10; measured in Task 12).
- **Soak safety (absolute):** never stop, restart or reconfigure open-streamer-soak-mediamtx, caddy, grafana, osdev-console-1, vector, alloy, pyroscope, prometheus, node-exporter, nvidia-gpu-exporter, or any other container not named `osv2-*`. Never bind ports 554, 1935 or 8554: every test uses `127.0.0.1:0` and reads the port back. Own containers are `osv2-*`, `--rm`.
- **Commits:** end every message with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Rulings

| ID | Ruling | Cost if wrong |
|---|---|---|
| R-E1 | **M5b is split in two.** **M5b1 (this plan):** inputs edited in place, the live watermark edit, the asset API, operator fonts, `no_fallback`, and M5a's three carry items. Done when the first three clauses of the M5b row hold: replacing a running stream's inputs is exactly one epoch change and ends no output session; an uploaded image and font appear on every rung; a `no_fallback` NVENC rung fails instead of falling back and is counted (Task 12). **M5b2, audio language:** the spec field, the HLS masters' `LANGUAGE`, the MPD's `lang`, `osconform` reading them (the row's fourth clause), and M4b's multi-audio minors. It is cut off because it shares no code with the rest (it is `pkg/hls`, `pkg/dash`, the packager, `internal/conform` and possibly the demuxers, none of which M5b1 touches), and because its design hangs on a question for the user: one declared language for the stream's one audio track, or several audio tracks chosen by language (see the plan's hand-over). Task 13 splits the §5 row | The M5b row closes in two steps; M5b2 can run beside M5c and M6, as M5b could |
| R-E2 | **`spec.ChangeLive`.** `Diff` returns `ChangeNone` (equal), `ChangeOutputs` (differ only in `outputs`, as before), `ChangeLive` (differ in `inputs` and/or `transcode.watermark`, and possibly `outputs`, and nowhere else; both documents have a ladder, or neither), `ChangeRebuild` (anything else: adding or removing the whole `transcode`, a rendition, `no_fallback`, `layout`, `thumbnail`, `disabled`). The enum stays ordered by weight. The API outcome of a running stream is `outputs`, `live` or `rebuild`. `Diff` also treats `layout: [video, audio]` as the empty layout (an M5a deferred minor: that edit rebuilt for nothing). Rejected: one class per field (`inputs`, `watermark`), which needs a set of classes for a mixed edit and brings back v1's per-field routing | A client that switches on the outcome string sees a value it does not know for an edit that used to say `rebuild`; the README and the OpenAPI enum say so |
| R-E3 | **`Pipeline.SetInputs(ctx, urls)`.** Applied by the `Run` goroutine between two frames; it returns when it is applied. Old and new entries are matched by URL text, in order, each old entry at most once. **Kept** (same URL, any position): the entry moves with its attempt, its reader, its listen socket and its retry time; nothing is reopened. **Removed, not active:** its attempt is stopped and its reader closed before `SetInputs` returns; no event. **Added:** opened as `Run` would open an input in that position (a probe above the active input; at once when none is active). **The active input kept:** it stays active at its new index; no Clock break, no epoch change, no switch counted; if inputs now stand above it they are probed and fail-back takes over by the existing rules (that later fail-back is its own epoch change). **The active input removed:** the Clock is flushed and the reader closed as at a failure, without `EventFailed` and without a backoff for anyone; with no active input, every input opens at once and the first frame activates one, which breaks the Clock once: **exactly one epoch change**, counted as one switch. The layout is fixed for the pipeline's life. Errors change nothing: an empty list, a URL `Parse` refuses, `ErrNotRunning` once `Run` has returned, `ctx.Err()`. Invariant 3 (failover never restarts the transcoder) holds by construction: the transcoder is a hub subscriber and sees an epoch change, as at a failover | Between the removal of the active input and the first frame of its successor the hub is silent (a pull input's connect time; nothing when the successor was already probing); players see a discontinuity, HLS one `EXT-X-DISCONTINUITY`. An operator who wants no gap adds the new input first, waits for the fail-back hold, then removes the old one |
| R-E4 | **Live watermark edit.** The supervisor builds the new `picture.Watermark` with the function `Check` and `build` use (it reads the image and the font from the asset directory) and calls `Transcoder.SetWatermark`; adding and removing a watermark are live too. The child takes it in order with its frames, the encoders are not reopened, no epoch changes. If the watermark cannot be built now (the asset went between `Check` and the apply), the old one stays on the picture, the stream's `last_error` says `watermark: …`, and the spec in force is the new one. A live edit made while the stream is between generations only replaces the spec | The picture and the spec can disagree until the next successful refresh, edit or generation; `last_error` is the only sign |
| R-E5 | **Asset model.** An asset is a regular file directly in `server.assets_dir`, named `^[a-z0-9][a-z0-9_-]{0,63}\.(png\|jpg\|jpeg)$` (an image, M4c's rule, unchanged) or `^[a-z0-9][a-z0-9_-]{0,63}\.(ttf\|otf)$` (a font). No subdirectories, no metadata file: the directory is the model, so files an operator copies in by hand are assets like any other, and the API works on a directory M5a already serves from. **Limits:** an image is at most 1 MiB and 2048 px per side (`picture.DecodeImage`, unchanged), a font at most 24 MiB (`picture.MaxFontBytes`; a wire message is at most 32 MiB), the directory at most 256 assets and 512 MiB in all. **Content:** an image must decode in full and be the format its extension names (`.png`: PNG; `.jpg`/`.jpeg`: JPEG); a font must pass `picture.CheckFont` and, when the transcoder is configured, the child's own loader (`ostranscoder -probe font`, R-E8). **Write:** a temp file `.<name>.<8 hex digits>.tmp` in the same directory, mode 0600, `fsync`, rename, directory `fsync`; a leftover temp file is removed when the store opens; a reader sees the whole old file or the whole new one. The entity tag is `"` + the first 16 hex digits of the SHA-256 of the bytes + `"` (assets are not secret, so it is not keyed) | The quota counts only what the store manages: a full disk is a 500 `asset_write_failed`, not a 507. No versions: a replaced asset is gone |
| R-E6 | **Asset API.** `GET /api/v1/assets` (viewer): the list, by name, each `{name, kind, size, modified, etag, used_by}` (`used_by`: the streams whose spec names it, running or not). `GET /api/v1/assets/{name}` (viewer): the bytes through `http.ServeContent`, `Content-Type` by content (`image/png`, `image/jpeg`, `font/ttf`, `font/otf`), `ETag`, `Content-Disposition: attachment`. `PUT /api/v1/assets/{name}` (editor): a raw body, `Content-Type: application/octet-stream` (anything else is 415); creates (201) or replaces (200); optional `If-Match` (a tag or `*`) and `If-None-Match: *` (create only); answers the asset document plus `refreshed`, the running streams that took the new bytes. `DELETE /api/v1/assets/{name}` (editor): 204. **Errors:** a name outside the rule is 404 `not_found` on every method (it names nothing); content the name does not allow is 422 `invalid_asset`; a body over the kind's limit is 413 `too_large`; a full directory is 507 `assets_full`; no `server.assets_dir` is 409 `assets_off`; a delete of an asset a stream names is 409 `asset_in_use` with the stream names; a failed precondition is 412; a write failure is 500 `asset_write_failed`. At most 2 uploads are read at once; a third waits for its turn or its request's end. Rejected: multipart forms (a second parser and file names from the client), and `POST` with a server-chosen name (the spec refers to assets by name) | Raw `PUT` is not what an HTML form sends; a UI uploads with `fetch`. Viewers can download every image and font |
| R-E7 | **Replacement and deletion under running streams.** A successful `PUT` of an asset calls `Manager.RefreshAsset(name)`: every running stream whose watermark names it loads its watermark again and calls `SetWatermark` (R-E4), so the new image or font is on the picture without a stream edit; a stream between generations takes it at its next build, as M5a already reads assets per generation. A `DELETE` is refused while any stream of the config names the asset, enabled or not. The reference check and the removal run under the server's apply mutex, which every stream edit holds, so a delete and an edit that starts using the asset cannot cross. The upload's read, checks and probe run **outside** that mutex | A file removed by hand under a running stream is still M5a's case (Review Focus 5 there): the next generation fails to build with the reason. `RefreshAsset` is not transactional across streams: one that fails to load reports it in its own `last_error` |
| R-E8 | **Operator fonts.** `spec.Watermark.Font` names a font asset, text watermarks only; empty means the embedded DejaVu Sans, as before. The host reads the file, checks it (`picture.CheckFont`: at most 24 MiB; an sfnt header `00 01 00 00`, `OTTO` or `true`; 1 to 256 table records, each inside the file; `cmap`, `head`, `hhea`, `hmtx`, `maxp` and either `glyf` with `loca` or `CFF `/`CFF2` present; collections `ttcf` and WOFF are refused by name) and sends the **bytes** in the watermark (wire field 7). The child writes them to `<its dir>/font.ttf`, the path the graph string already names, so no operator text or path enters a filter graph (the invariant stands) and the child never opens an operator's path. **The real parser is the child's FreeType**, so an upload is also probed with `ostranscoder -probe font FILE` (10 s limit), which draws two glyphs with it on a 256×144 picture; without a configured transcoder there is no probe and no child to hurt. Shaping (Arabic) and glyph coverage are the font's and the FFmpeg build's business (drawtext shapes with HarfBuzz); the plan proves only that the operator's font is the one drawn. Rejected: `golang.org/x/image/font/sfnt` (a new module for a second opinion the child gives anyway); passing a path (the child would read operator-controlled paths, and a replaced file would change under it) | A font FreeType accepts in the probe and fails on later text is fatal in the child (M4c's R-C9: never a picture without its watermark), so the stream respawns until the watermark is edited. Each running stream with a font holds its bytes twice (host and child), up to 48 MiB |
| R-E9 | **Refusing the fallback.** `spec.Rendition.NoFallback` (`no_fallback: true`), valid only with `encoder: nvenc`. In the child, a rung with it whose NVENC open fails is not opened with libx264: the build fails, and since a build opens every rung or none (M4c), **the whole ladder fails**: the child sends `Error{Rung, Code: ErrorEncoderRefused, Fatal: true}` and exits 1; the host counts `Stats.EncoderRefusals` (once per refused build), reports the message in the exit error, and respawns by its existing backoff (250 ms doubling to 5 s). No software frame is ever produced for any rung of that ladder; the stream's source keeps running, its ladder outputs carry nothing until NVENC opens. Metric `openstreamer_transcoder_encoder_refusals_total{stream}`; status `transcoder.encoder_refusals`. Rejected: failing only that rung (a presentation with a rung that never starts is M4b's degraded path, not a state to run in for hours), and stopping the stream (an NVENC session freed by another stream would not bring it back) | One strict rung takes the ladder's software rungs down with it, and a host whose GPU is gone retries every 5 s for ever: a spawn, a decode to the first picture, an exit. The counter and `last_error` are how an operator sees it |
| R-E10 | **Proof, and where it lives.** `TestM5b1ReplaceInputs` (a passthrough stream on every output, M5a's session harness: every session reader holds every hub frame once across the edit; one more epoch on the hub; no generation change) and `TestM5b1ReplaceInputsLadder` (the same edit under a real child: one epoch per rendition, the same PID); `TestM5b1Assets` (upload, live watermark edit, replacement, font, delete in use, roles; every rung checked by picture); `TestM5b1NoFallback`. They stay in `internal/e2e`, because they reuse M5a's harness and M4c's picture helpers, which are test files of that package; they are `t.Parallel()` and bounded at 90 s together. Task 13 reads the package's time from the CI run: above 17 min, the Backlog item "Split `internal/e2e`" is done before the PR merges | The package crosses 20 min on a slow runner and `make test` fails for a reason that is not a defect |
| R-E11 | **Listeners stay fixed at start** (carry item 3). M5b1 does not make `server` hot-reloadable, so the 422 for an output whose listener is off stays, and the README gains the two operator consequences (Task 1) | Adding a listener and its first output stays a two-step change with a restart between |
| R-E12 | **One wire bump.** Fonts and `no_fallback` both change the protocol; `wire.Version` goes from 2 to 3 once, in Task 4, with every field of both. The host, the fake child and the real child share `internal/transcode/wire`, so every merged state builds a consistent set; only a bundle left in `dist/` from before the merge is stale | A bundle built before Task 5 is refused by the host with the version error; `make ostranscoder` fixes it |

## Execution waves (parallel per user instruction)

| Wave | Tasks | Why this order |
|---|---|---|
| 1 | 1 (carry items), 2 (`spec`), 3 (`pipeline.SetInputs`), 4 (`picture`, `wire` v3) | Disjoint files: Task 1 has `internal/stream/manager_test.go` and `helpers_test.go`, `internal/api/errors.go` with its test, and `README.md`; Task 2 `internal/spec`; Task 3 `internal/pipeline`; Task 4 `internal/transcode/picture`, `internal/transcode/wire` and the fixture font |
| 2 | 5 (`native`, `cmd/ostranscoder`), 6 (`transcode` host, `transcodetest`), 7 (`internal/asset`) | 5 and 6 need Task 4's wire and picture; 7 needs Task 4's names and `CheckFont`. 5 has the cgo packages, 6 `internal/transcode/*.go` and `transcodetest`, 7 a new package |
| 3 | 8 (`stream.Supervisor`) | Needs 2, 3, 4 and 6 |
| 4 | 9 (`stream.Manager`) | Same package as 8 |
| 5 | 10 (`api`) | Needs 7's `asset.Info` and errors and 9's outcome |
| 6 | 11 (`server`, the example config, README) | Needs 7, 9 and 10 |
| 7 | 12 (M5b1 scenarios) | Needs 11 and Task 5's bundle |
| 8 | 13 (CI, docs, spec, done) | Needs 12 |

Each parallel task runs in its own worktree; the controller merges.

## Review Focus

1. **An asset name or body from a hostile client:** `../x.png`, `%2e%2e%2fx.png`, `a/b.png`, `.png`, `X.PNG`, a 65-character stem, a name with a NUL or a space; an HTML or SVG file named `.png`, a PNG named `.jpg`, a truncated PNG, a 2049 px image, a 20000×20000 PNG of a few kB, a WOFF or a `.ttc` named `.ttf`, a font whose table directory points past the file, a body one byte over the limit with and without `Content-Length`. Each is refused with its code, nothing is written, no temp file is left, and nothing outside the directory is read or written. Task 7, `TestKindOf`, `TestPutRefusesContent` and `TestPutLeavesNoTemp`; Task 4, `TestCheckFont`; Task 10, `TestAssetNames` and `TestAssetTooLarge`.
2. **An input edit that meets the pipeline in an awkward state:** the active input removed while its successor is in a retry backoff, or during the search after a failure; a kept listen input (its socket must not be rebound); a reorder that puts an input above the active one; the same URL twice in a list; the same edit sent twice; an edit as `Run` ends. No reader leaks, a kept reader is never reopened, and the epoch count is 0 or 1 as R-E3 says. Task 3, `TestSetInputsReplaceActive`, `TestSetInputsKeepsListener`, `TestSetInputsDuringSearch`, `TestSetInputsDuplicates` and `TestSetInputsErrors`.
3. **An asset replaced or gone under running streams:** a replacement while two streams use it and one of them is in backoff; a file removed by hand, then a live watermark edit or a refresh; a delete of an asset a disabled stream names. The running picture keeps a valid watermark, the failure is in `last_error`, the delete is refused. Task 8, `TestSupervisorRefresh` and `TestSupervisorWatermarkAssetGone`; Task 9, `TestManagerAssetUsers`; Task 11, `TestDeleteAssetInUse`.
4. **A `no_fallback` rung on a host where NVENC never opens:** no software frame on any rung, a respawn at most every 5 s, the counter up by one per attempt, `last_error` naming the rung, other streams untouched, and an edit that clears `no_fallback` brings the ladder up on libx264. Task 5, `TestNoFallbackRefuses`; Task 6, `TestRefusalCounted`; Task 12, `TestM5b1NoFallback`.
5. **Edits that mix and race:** one `PUT` that changes inputs, watermark and outputs (all three applied, one answer, `live`); inputs with a rebuild field (a rebuild); a live edit during a backoff; twenty concurrent writers mixing stream edits with asset replacements and deletes. No deadlock between the apply mutex and the store, the final state equals the last write, no goroutine is left. Task 2, `TestDiff`; Task 8, `TestSupervisorLiveEditAllParts` and `TestSupervisorLiveEditInBackoff`; Task 11, `TestConcurrentAssetAndStreamWrites`.

---

### Task 1: Carry items from M5a's final re-review

A hygiene task: a flaky test made exact, a cap on error lists, and two README paragraphs.

**Files:**
- Modify: `internal/stream/manager_test.go` (`TestManagerConcurrentReplaces`, `listenSpec`), `internal/stream/helpers_test.go` (the lease), `internal/api/errors.go`, `README.md`
- Test: `internal/stream/manager_test.go`, `internal/api/streams_test.go`

**Interfaces:** none produced.

- [ ] **Step 1: The cause, in the plan's terms.** `TestManagerConcurrentReplaces` failed once with `udp … bind: address already in use`. Its ports come from `listenSpec`, which binds, reads the port and closes: between two generations of `cam` the port is free, and any process on the host may take it. So the failure has two possible causes the test cannot tell apart: a foreign process, or two generations of `cam` overlapping (the invariant R-D5 forbids). Replace the kernel ports by **held sockets** and assert the invariant directly.
- [ ] **Step 2: Write the lease** in `helpers_test.go`:

```go
// leases hands out exclusive use of sockets the test binds once and holds
// until it ends. An Open for a URL whose previous reader has not returned
// from Close is an overlap: two generations of one stream held one input
// at the same time.
type leases struct{ /* by URL: the held socket, the live holder, the overlaps seen */ }

func newLeases(t testing.TB) *leases
// udp binds 127.0.0.1:0 once, holds it for the test and returns its URL "udp://127.0.0.1:<port>" and the port.
func (l *leases) udp() (url string, port int)
// idle returns a URL whose reader delivers nothing until it is closed (the RTMP listen input's stand-in).
func (l *leases) idle(url string) string
// Open is an Env.Open. For a leased URL it returns ingest.NewTSReader over a reader of the held socket's
// datagrams; its Close waits delay, then releases the lease, and never closes the held socket.
func (l *leases) Open(ctx context.Context, url string) (ingest.Reader, error)
func (l *leases) Overlaps() []string // one line per overlap: the URL and both holders' open numbers
func (l *leases) Opens() int64
func (l *leases) Live() int64
```

A read of a released lease returns `io.EOF` at once; a read deadline of 50 ms on the held socket lets a lease notice its release. `Open` on a URL that is still held records the overlap **and** returns an error whose text is `lease: overlap`, so a real overlap also shows in `last_error`.
- [ ] **Step 3: Rewrite the test on it.** `listenSpec` stays for the tests that need real ports. `TestManagerConcurrentReplaces` builds `cam` with `inputs: [l.idle("rtmp://lease/in/"+vKey+"?mode=listen"), udpURL]`, `Env.Open: l.Open`, and feeds the fixture to the held port with `sendFixture`. Every assertion it has stays (no deadlock within 20 s, the final list, view, layout and `running` state, `stream_up == 1`, more than 2 opens, `Run` returns, no goroutine left, no `vKey` in the log), and it gains `assert.Empty(t, l.Overlaps(), "two generations of cam never hold an input at once")`. `assertNoSocketErrors` stays as it is: with held sockets no bind happens, so any socket error is now a defect.
- [ ] **Step 4: A real rebind stays covered, with the two causes told apart.** Add `TestManagerRebindsAfterRebuild`: `listenSpec`'s real ports, one stream, 10 sequential rebuild edits (`Layout` toggled), each awaited. After each, `Status.LastError` must not contain `address already in use`; if it does, the test probes the port itself (`ListenPacket` on it): when the probe also fails **and** the stream has no generation (`View().Gen == 0`), a foreign process holds the port, and the test takes new ports and starts over, at most 3 times, logging each; any other bind error fails the test as an overlap.
- [ ] **Step 5: Hunt for a real overlap.** Run `go test -race -count=300 -run '^TestManagerConcurrentReplaces$' ./internal/stream/` while `go test -race -count=1 ./internal/e2e/ ./internal/transcode/` runs beside it. Expected: 300 passes, `Overlaps()` empty. **If an overlap is reported, stop the task and follow superpowers:systematic-debugging on the supervisor chain (`NewSupervisor`'s `after`, `Manager.tails`, `teardown`'s wait for `pipeDone`): the fix goes into `internal/stream` with a test that fails without it, in this task, and the plan's later tasks build on it.**
- [ ] **Step 6: Cap the error lists.** Write `TestErrorsFromManagerCapped` in `internal/api/streams_test.go`: a backend whose `PutStream` returns 150 joined reasons that wrap `stream.ErrInvalid` answers 422 with `len(errors) == 101`, `errors[100] == "and 50 more"`, and `errors[0..99]` the first hundred in order; the same with `stream.ErrConflict` answers 409 with the same shape; 100 reasons give 100 entries and no tail. Run it: it fails (150 entries).
- [ ] **Step 7:** in `internal/api/errors.go` add `capped(list []string) []string` (at most `config.MaxErrors` entries, then `fmt.Sprintf("and %d more", n)`), and use it on both `texts(split(err))` results. Run `go test -race -count=1 ./internal/api/`: pass.
- [ ] **Step 8: README.** After the paragraph that ends "turn the output off, or set the listener and restart", add, under a line `Listeners are fixed at start:`, two items in the README's own voice: (1) removing a server listener (`server.rtmp.listen`, `rtsp`, `srt`) and restarting leaves every stream that has that output `stopped` with a load error that names the output, until the output is turned off or the listener is back; (2) adding a listener and turning on its output takes two steps, the listener and a restart, then the output: one whole-config `PUT` that does both is refused with 422, because the output is judged against the listeners the server started with.
- [ ] **Step 9:** `go test -race -count=1 ./internal/stream/ ./internal/api/` and `make lint`: pass.
- [ ] **Step 10: Commit** `test(stream): held sockets and an overlap ledger for the concurrent replaces`, `fix(api): cap an edit's error list at config.MaxErrors` and `docs: what a removed or added listener does to streams`

### Task 2: `internal/spec` — `ChangeLive`, `no_fallback`, `font`

**Files:**
- Modify: `internal/spec/spec.go`, `internal/spec/diff.go`, `internal/spec/validate.go`, `internal/spec/doc.go`
- Test: `internal/spec/diff_test.go`, `internal/spec/validate_test.go`, `internal/spec/spec_test.go`

**Interfaces — Produces:**

```go
const (
	ChangeNone    Change = iota // the documents are equal
	ChangeOutputs               // they differ only in outputs
	ChangeLive                  // they differ in inputs or transcode.watermark, maybe in outputs, and nowhere else
	ChangeRebuild               // they differ elsewhere
)

// In Rendition: NoFallback makes an nvenc rung fail when NVENC cannot be opened, instead of encoding with libx264 (R-E9).
NoFallback bool `json:"no_fallback,omitempty" yaml:"no_fallback,omitempty"`
// In Watermark: Font is a font asset's name (R-E8); text only; "" means the embedded font.
Font string `json:"font,omitempty" yaml:"font,omitempty"`

// Assets returns the asset names the document refers to (the watermark's image and font), sorted, without repeats.
func (s Stream) Assets() []string
```

- [ ] **Step 1: Write the failing tests**

```go
func TestDiff(t *testing.T) // table, each row an edit of one base document with a ladder, a text watermark, 2 inputs, 2 pushes:
// equal → None; nil vs empty slices → None; layout nil vs ["video","audio"] → None; ["audio","video"] → Rebuild;
// outputs.hls → Outputs; a push reordered → Outputs;
// an input URL changed → Live; inputs reordered → Live; an input added → Live; an input removed → Live;
// watermark text → Live; watermark font → Live; watermark removed → Live; watermark added → Live;
// inputs + watermark + outputs together → Live;
// inputs + layout → Rebuild; watermark + a rendition's bitrate → Rebuild; no_fallback → Rebuild;
// transcode removed → Rebuild; transcode added (with a watermark) → Rebuild; thumbnail → Rebuild; disabled → Rebuild.
func TestDiffOrder(t *testing.T) // ChangeNone < ChangeOutputs < ChangeLive < ChangeRebuild
func TestValidateNoFallback(t *testing.T)
// {encoder:"nvenc", no_fallback:true} valid; with encoder "" or "x264": "transcode.renditions[0].no_fallback: needs encoder nvenc";
// on a copy rendition: the existing "a copy rendition sets nothing but its label".
func TestValidateFont(t *testing.T)
// font "noto-sans.ttf", "a_b-c.otf" with text: valid; "Noto.ttf", "x.ttc", "x.woff", "../x.ttf", "x.png", a 65-character stem:
// "transcode.watermark.font: … is not a ttf or otf asset name"; a font with an image: "transcode.watermark.font: needs text".
func TestAssets(t *testing.T) // no ladder → nil; image only → [image]; text+font → [font]
func TestCloneCopiesNewFields(t *testing.T) // Redacted() and ETag() carry NoFallback and Font; ETag differs when either differs
```

- [ ] **Step 2: Run** `go test -race -count=1 ./internal/spec/` — compile failure.
- [ ] **Step 3: Implement.** `Diff`: compare the normal forms; then with `Outputs` cleared; then with `Inputs` set to nil and, when both have a ladder, `Transcode.Watermark` set to nil on copies. `normal` maps a layout equal to `["video","audio"]` to nil. `fontRE` is `^[a-z0-9][a-z0-9_-]{0,63}\.(ttf|otf)$`.
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/spec/` — pass.
- [ ] **Step 5: Keep M5a's behaviour until Tasks 8 and 9.** A new enum value compiles everywhere, but a `switch` without it would leave a live edit unanswered. In `internal/stream/supervisor.go`, make the `ChangeRebuild` arm of both switches (`serve`, `idle`) `case spec.ChangeLive, spec.ChangeRebuild:` with the comment `// ChangeLive: applied in place from Task 8`; in `manager.go`'s `plan`, report `rebuild` for both. So an input or watermark edit still rebuilds and still answers `rebuild`, and every test of `internal/stream` passes unchanged. `make lint` and `go test -race -count=1 ./internal/stream/ ./internal/config/ ./internal/api/` pass, except `TestOpenAPIMatchesSpec`, which now names the two fields it does not find: add them to `internal/api/openapi.json` (`no_fallback`: boolean, "Fail the ladder when NVENC cannot be opened for this rendition, instead of encoding it with libx264; needs encoder nvenc."; `font`: string, "The name of a TTF or OTF file in the assets directory, for a text watermark; the built-in DejaVu Sans when empty.").
- [ ] **Step 6: Commit** `feat(spec): a live change class, no_fallback and the watermark font`

### Task 3: `Pipeline.SetInputs`

**Files:**
- Modify: `internal/pipeline/pipeline.go`
- Create: `internal/pipeline/setinputs.go`
- Test: `internal/pipeline/setinputs_test.go` (on `fake_test.go`'s `fakeReader`, `opener`, `byURL`, `events` and `fakeTime`)

**Interfaces — Produces:**

```go
// ErrNotRunning is SetInputs' error once Run has returned.
var ErrNotRunning = errors.New("pipeline: not running")

// SetInputs replaces the inputs, by priority, while Run runs, and returns
// once Run has applied the change (R-E3). An input whose URL stays keeps
// its reader and its retry state; one that goes is closed before SetInputs
// returns; the hub changes epoch only if the active input goes, once, when
// its successor delivers. Event.Input and Stats.Active index the new list
// from then on. On an error nothing changed. Safe from any goroutine; a
// call made before Run waits for it.
func (p *Pipeline) SetInputs(ctx context.Context, urls []string) error

// Inputs returns the URLs in force, by priority. Safe from any goroutine.
func (p *Pipeline) Inputs() []string
```

The algorithm, run by the `Run` goroutine when it takes the request from its `select` (a new case beside the timer and the items):

```
parse every new URL (Options.Parse); any error → answer it, change nothing
match: for each new URL in order, the first old entry with the same URL that no earlier new URL took
for each old entry not matched:
    if it is the active one: flush the Clock into the hub as fail does, stop its attempt,
        act = nil, search = false, Stats.Active = -1; no EventFailed, no backoff
    else: stop its attempt (stopAttempt waits for its goroutine)
build the new entry list: a matched entry moves (its attempt's index is rewritten),
    an unmatched new URL gets a fresh entry (passive from Parse, no backoff)
if an input is still active at index k: stop the pull attempts now below k and reset the passive ones below k,
    exactly as activate does for the inputs below a newly active one; Stats.Active = k
last (the input active last) follows its entry; if that entry is gone it becomes "none of these",
    so that the next activation counts as a switch
drop removed attempts from the layout wait list; rearm = true; answer nil
```

Nothing else changes: `schedule` then opens what is due under the existing rules, and the first frame after a removed active input goes through `activate`, whose `Break(true)` is the one epoch change.

- [ ] **Step 1: Write the failing tests** (fake time, fake readers; "epochs" are the distinct `Frame.Epoch` values the hub received)

```go
func TestSetInputsReplaceActive(t *testing.T)
// [A] active, its frames in one epoch e. SetInputs([B]): A's reader is closed when it returns; B is opened once;
// after B's first frame the hub's epochs are exactly {e, e+1}; Stats.Active == 0, Switches == 1; no EventFailed was emitted;
// EventActive{Input: 0} was emitted for B.
func TestSetInputsKeepsActive(t *testing.T)
// [A, B] with A active. SetInputs([C, A]): A opened once in all and never closed; no new epoch; Stats.Active == 1; Switches == 0;
// C is opened as a probe; after FailbackHold of C's frames and its next key, C is active: exactly one new epoch, Switches == 1.
func TestSetInputsReorderBelow(t *testing.T)
// [A, B], A failed and in backoff, B active, A probing. SetInputs([B, A]): Stats.Active == 0; A's pull attempt is closed (it is below
// the active input now) and is not reopened while B is active; no new epoch.
func TestSetInputsRemovesIdle(t *testing.T)
// [A, L] with A active and L passive, listening. SetInputs([A]): L's reader is closed before SetInputs returns; no new epoch;
// A opened once.
func TestSetInputsKeepsListener(t *testing.T)
// [A, L], L passive. SetInputs([L, B]): L opened once in all, never closed (the same reader instance delivers after the edit).
func TestSetInputsDuringSearch(t *testing.T)
// [A, B, C]: A failed (retry in 500 ms), the search is trying B. SetInputs([A, D]): B's and C's attempts are closed; D opens at once;
// A is not reopened before its retry time; the first of A and D to deliver is active; exactly one new epoch after the edit.
func TestSetInputsDuplicates(t *testing.T) // [A, A] → [A]: the first A's reader stays, the second is closed; [A] → [A, A]: one more open
func TestSetInputsSameList(t *testing.T) // the list in force again: nil, nothing opened, nothing closed, no event
func TestSetInputsErrors(t *testing.T)
// nil and []string{}: an error, Inputs() unchanged; a URL Parse refuses: an error that does not contain the URL, Inputs() unchanged
// and no reader touched; after Run returned: ErrNotRunning; a context cancelled before Run takes it: context.Canceled.
func TestSetInputsBeforeRun(t *testing.T) // a call made before Run blocks, and returns nil once Run runs with the new list
func TestSetInputsRace(t *testing.T) // -race: four goroutines call SetInputs with permutations for 200 ms of real frames; Run returns
// nil error but ctx's, every reader that was opened is closed, runtime.NumGoroutine is back.
```

- [ ] **Step 2: Run** `go test -race -count=1 -run 'TestSetInputs' ./internal/pipeline/` — compile failure.
- [ ] **Step 3: Implement** in `setinputs.go`: the request is `struct{ urls []string; ans chan error }` on an unbuffered channel made in `New`; `SetInputs` selects on the send, `ctx.Done()` and a `done` channel `Run` closes when it returns (then `ErrNotRunning`), then on the answer and `done`. `Options.Inputs` is replaced by a copy; `Inputs()` reads it under `p.mu`. `Stats.Active` is written under `p.mu`, as `activate` does.
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/pipeline/` (every existing test unchanged) and `go test -race -count=50 -run 'TestSetInputs' ./internal/pipeline/` — pass.
- [ ] **Step 5: Commit** `feat(pipeline): replace the inputs of a running pipeline`

### Task 4: `picture` and `wire` version 3

**Files:**
- Modify: `internal/transcode/picture/watermark.go`, `internal/transcode/wire/wire.go`, `internal/transcode/wire/doc.go`
- Create: `internal/transcode/picture/font.go`, `testdata/fixtures/fonts/DejaVuSansMono.ttf`, `testdata/fixtures/fonts/LICENSE`
- Test: `internal/transcode/picture/font_test.go`, `internal/transcode/picture/watermark_test.go`, `internal/transcode/wire/wire_test.go`

**Interfaces — Produces:**

```go
// package picture
const MaxFontBytes = 24 << 20

// In Watermark: Font is a TrueType or OpenType file; text only; nil means the built-in DejaVu Sans.
Font []byte

// ImageName and FontName report whether name is an image or a font asset's name (R-E5).
func ImageName(name string) bool
func FontName(name string) bool

// CheckFont reports why b is not a font the child may be given (R-E8). It reads the table directory only.
func CheckFont(b []byte) error

// LoadFont reads the font asset name from dir, as LoadAsset reads an image: the name must match the rule,
// the file is at most MaxFontBytes, and it passes CheckFont.
func LoadFont(dir, name string) ([]byte, error)

// package wire
const Version = 3

// ErrorCode says what kind of failure an Error reports.
type ErrorCode uint32

const (
	ErrorOther          ErrorCode = iota
	ErrorEncoderRefused           // a no_fallback rung could not open its encoder (R-E9)
)

// In Rung:  NoFallback bool        (field: the next free number of the rung message, a bool written only when true)
// In Error: Code ErrorCode         (field 4, a varint written only when not zero; an unknown value decodes as ErrorOther)
// Watermark field 7: font (bytes), at most picture.MaxFontBytes on both sides.
```

- [ ] **Step 1: The fixture.** Copy DejaVu Sans Mono 2.37 from `fonts-dejavu-core` (`/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf` on this host, or in `osv2-ci-peers:m4a`) to `testdata/fixtures/fonts/DejaVuSansMono.ttf`, and `internal/transcode/native/font/LICENSE` to `testdata/fixtures/fonts/LICENSE`. Check `sha256sum` against the package's file and write the sum into `testdata/fixtures/README.md` with one line saying what the file is for.
- [ ] **Step 2: Write the failing tests**

```go
func TestCheckFont(t *testing.T)
// the fixture: nil. Then, each an error naming the reason: empty; 11 bytes; over MaxFontBytes; the fixture with magic "ttcf"
// ("a font collection is not accepted: extract one face"); magic "wOFF" and "wOF2" ("WOFF is not accepted"); magic "<svg";
// numTables 0 and 257; a table whose offset+length is past the end; an offset+length that overflows uint32;
// the fixture with its "cmap" tag renamed; with "glyf" renamed (no outlines); a directory cut in the middle of a record.
// And accepted: the fixture with magic "true"; a synthetic OTTO directory with "CFF " and the five required tables.
func TestFontNames(t *testing.T) // FontName: "a.ttf", "a_b-c.otf" true; "A.ttf", "a.ttc", "a.png", ".ttf", "a/b.ttf", "../a.ttf", 65-char stem false; ImageName as LoadAsset's rule
func TestLoadFont(t *testing.T) // a temp dir: the fixture loads; a bad name is refused before any open; a missing file; a file that fails CheckFont
func TestWatermarkValidateFont(t *testing.T) // text+font valid; image+font: "watermark: a font needs text"; text + a bad font: CheckFont's error
func TestWatermarkFontRoundTrip(t *testing.T) // wire: a Configure and a SetWatermark with a 300 kB font round-trip byte for byte; a font of
// MaxFontBytes+1 is refused by the writer and, hand-encoded, by the reader; a watermark without a font has the bytes it had in version 2
func TestRungNoFallbackRoundTrip(t *testing.T) // true round-trips; false writes no field
func TestErrorCodeRoundTrip(t *testing.T) // ErrorEncoderRefused round-trips; code 99 reads as ErrorOther; code 0 writes no field
func TestVersionIs3(t *testing.T)
```

- [ ] **Step 3: Run** `go test -race -count=1 ./internal/transcode/picture/ ./internal/transcode/wire/` — compile failure.
- [ ] **Step 4: Implement.** `CheckFont` uses `encoding/binary` on the 12-byte header and the 16-byte records only. `Watermark.Validate` calls it; `cloneWatermark` callers are Task 6's. Update the tests in the repository that pin `Version` 2 or the watermark's byte layout.
- [ ] **Step 5: Run** the two packages, then `go build ./... && go vet ./...` — pass (the host and the fake child compile against the new fields; they use them in Tasks 5 and 6).
- [ ] **Step 6: Commit** `feat(picture): font assets and their check` and `feat(wire): version 3: a watermark's font, no_fallback, an error code`

### Task 5: The child — the operator's font, the refusal, `-probe font`

Verified with `make test-transcode` (cgo, in `osv2-ci-peers:m4a`), then `make ostranscoder`.

**Files:**
- Modify: `internal/transcode/native/watermark.go`, `internal/transcode/native/encoder.go`, `internal/transcode/native/pipeline.go`, `cmd/ostranscoder/main.go`, `cmd/ostranscoder/doc.go`
- Test: `internal/transcode/native/watermark_test.go`, `internal/transcode/native/encoder_test.go`, `cmd/ostranscoder/main_test.go`

**Interfaces — Produces:**

```go
// package native
// In Rung: NoFallback bool — an NVENC rung that cannot be opened fails the build instead of using libx264.

// RefusedError is the build error of a NoFallback rung whose encoder could not be opened.
type RefusedError struct {
	Rung int
	Err  error // the NVENC failure
}
func (e *RefusedError) Error() string // "rung <n>: nvenc could not be opened and no_fallback is set: <err>"
func (e *RefusedError) Unwrap() error

// ProbeFont draws text with the font on a 256×144 picture and reports why that fails.
func ProbeFont(font []byte) error
```

`ostranscoder -probe font FILE` reads at most `picture.MaxFontBytes+1` bytes of FILE, runs `picture.CheckFont` then `native.ProbeFont`, prints `font: ok` and exits 0, or prints `font: <reason>` and exits `exitProbeFailed`; the usage line becomes `usage: ostranscoder [-version | -probe nvenc | -probe font FILE]`. A fatal build error that is a `*native.RefusedError` is written as `wire.Error{Rung: e.Rung, Code: wire.ErrorEncoderRefused, Fatal: true, Message: …}`; every other fatal keeps `ErrorOther`.

- [ ] **Step 1: Write the failing tests**

```go
func TestWatermarkFont(t *testing.T)
// Text "iiiiiiii", SizePct 20, TopLeft on a 640×360 black picture, drawn twice: with no font, and with the fixture Mono.
// The lit bounding box (luma > 128) of the Mono run is at least 1.5 times as wide as the other's: a monospaced i is as wide as an m.
// The graph string is the same in both runs and holds neither font's bytes nor a path outside the watermarker's directory.
func TestWatermarkFontChange(t *testing.T) // set(no font) → set(Mono) → set(no font) on one watermarker: the box widens and narrows again;
// <dir>/font.ttf holds the current font's bytes after each set
func TestWatermarkFontRestored(t *testing.T) // <dir>/font.ttf removed under a Mono watermark: the next apply writes Mono back, not DejaVu
func TestWatermarkBadFont(t *testing.T) // 4 kB of zeros behind a valid directory: apply fails with an error that says "font"
func TestProbeFont(t *testing.T) // the fixture: nil; the bad font above: an error
func TestNoFallbackRefuses(t *testing.T)
// nvencGPU = 99 (a device that does not exist; restored; not parallel), rungs [nvenc+NoFallback, x264]: openEncoders returns a
// *RefusedError with Rung 0, no rung is left open, Stats.EncoderFallbacks stays 0. With rungs [x264, nvenc+NoFallback]: Rung 1,
// and the x264 rung it had opened is released.
func TestFallbackUnchanged(t *testing.T) // the same without NoFallback: the build succeeds on libx264 and EncoderFallbacks is 1, as before
func TestProbeFontFlag(t *testing.T) // in cmd/ostranscoder: the fixture → "font: ok", exit 0; a text file → "font: …", exitProbeFailed;
// a missing file → exitProbeFailed; `-probe font` without a file → usage, exit 2
func TestChildRefusal(t *testing.T) // in cmd/ostranscoder, with the package's seam for a failing NVENC open: Configure with a NoFallback
// rung, one GOP → the child writes Error{Rung: 0, Code: ErrorEncoderRefused, Fatal: true} and exits 1, and writes no Frame of any rung
```

- [ ] **Step 2: Run** `make test-transcode ARGS='-run "TestWatermarkFont|TestWatermarkBadFont|TestProbeFont|TestNoFallback|TestFallbackUnchanged|TestChildRefusal" ./internal/transcode/native/ ./cmd/ostranscoder/'` — compile failure.
- [ ] **Step 3: Implement.** The watermarker keeps the font bytes it last wrote; `set` writes `font.tmp` and renames it over `font.ttf` when the watermark's font (or the built-in one, for nil) differs from them, then frees the graph; `restoreFiles` writes the current font. `openRung` returns the `*RefusedError` instead of opening libx264 when `r.NoFallback` and NVENC failed. The untagged stub of `cmd/ostranscoder` answers `-probe font` as it answers `-probe nvenc`.
- [ ] **Step 4: Run** `make test-transcode` (all cgo packages) — pass. Then `make ostranscoder`, and on this host `dist/ostranscoder-amd64/ostranscoder -probe font testdata/fixtures/fonts/DejaVuSansMono.ttf` prints `font: ok`.
- [ ] **Step 5: Decide how Task 12 forces a refusal on the GPU host.** Run `CUDA_VISIBLE_DEVICES= dist/ostranscoder-amd64/ostranscoder -probe nvenc; echo $?`. Record the result in the task report: exit `exitProbeFailed` means an empty `CUDA_VISIBLE_DEVICES` in a child's environment makes NVENC unavailable to it, and Task 12 uses it (through `server.Options.TranscoderEnv`, Task 11); exit 0 means it does not, and Task 12's test skips where `/dev/nvidiactl` exists (the refusal on a GPU host is then proved by `TestNoFallbackRefuses` alone, under `make test-nvenc`).
- [ ] **Step 6: Commit** `feat(native): draw text with the watermark's font`, `feat(native): a no_fallback rung fails the build` and `feat(ostranscoder): -probe font, and the refusal's error code`

### Task 6: The host — `Video.NoFallback` and counted refusals

**Files:**
- Modify: `internal/transcode/transcode.go`, `internal/transcode/transcodetest/fake.go`
- Test: `internal/transcode/transcode_test.go` (or the file that holds the child-exit tests), `internal/transcode/transcodetest/fake_test.go`

**Interfaces — Produces:**

```go
// In Video: NoFallback makes an nvenc rendition fail the ladder when NVENC cannot be opened (R-E9).
NoFallback bool
// In Stats: EncoderRefusals counts the children that ended because a NoFallback rendition could not open its encoder.
EncoderRefusals uint64

// package transcodetest
// EnvRefuse makes the fake child answer its first video frame with a fatal Error
// {Rung: <the value, a rung index>, Code: ErrorEncoderRefused} and exit 1.
const EnvRefuse = "OSTX_FAKE_REFUSE"
```

Rules: `New` refuses `NoFallback` on a rendition whose encoder is not `nvenc` ("rendition %q: no_fallback needs encoder nvenc") and on a copy; the flag goes into the rung of every `Configure`. The reader goroutine counts `EncoderRefusals` when it reads an `Error` with `ErrorEncoderRefused`. **Every** fatal `Error`'s message is kept on the child (the last one, cut to 256 bytes), and `exited` appends it to the exit error as `: <message>`, so `Event.Err` and the supervisor's `last_error` name the cause. `cloneWatermark` copies `Font`. The describe line of the fake child's watermark log gains ` font=<n bytes>` when a font is set.

- [ ] **Step 1: Write the failing tests**

```go
func TestNoFallbackOptions(t *testing.T) // New: nvenc+NoFallback accepted; x264+NoFallback and copy+NoFallback refused with the texts above
func TestNoFallbackReachesChild(t *testing.T) // the fake child's frame log / configure echo shows NoFallback on that rung and not on the other
func TestRefusalCounted(t *testing.T)
// fake child with EnvRefuse=0, fake After: after the first spawn's exit Stats.EncoderRefusals == 1, Exits == 1, Child.EncoderFallbacks == 0;
// the EventExited's Err text contains "no_fallback" (the child's message) and the PID; firing the respawn wait gives a second child and
// EncoderRefusals == 2; the waits asked of After are 250 ms then 500 ms (the existing backoff, unchanged); no frame was written to
// any rendition hub or to the presentation hub.
func TestFatalMessageInExitError(t *testing.T) // a fake child that sends Error{Fatal: true, Message: 300 × "x"} and exits: Err's text ends with
// ": " + 256 × "x"; a child that exits without one has the text it had before
func TestSetWatermarkCarriesFont(t *testing.T) // SetWatermark with a font: the fake child's watermark log shows font=<n>; changing the caller's
// slice afterwards does not change what a respawned child is configured with
```

- [ ] **Step 2: Run** `go test -race -count=1 -run 'TestNoFallback|TestRefusalCounted|TestFatalMessage|TestSetWatermarkCarriesFont' ./internal/transcode/...` — compile failure.
- [ ] **Step 3: Implement** as the rules say.
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/transcode/...` — pass; an existing test that compared a whole exit-error text gets its expectation extended, and the task report lists each one.
- [ ] **Step 5: Commit** `feat(transcode): no_fallback renditions, counted refusals, the child's last word in its exit error`

### Task 7: `internal/asset` — the asset directory

**Files:**
- Create: `internal/asset/doc.go`, `internal/asset/asset.go`, `internal/asset/sync_unix.go`, `internal/asset/sync_other.go`
- Test: `internal/asset/asset_test.go`

**Interfaces — Produces:**

```go
// Kind is what an asset is.
type Kind string

const (
	Image Kind = "image"
	Font  Kind = "font"
)

// Limits of the directory (R-E5).
const (
	MaxFiles      = 256
	MaxTotalBytes = 512 << 20
)

var (
	ErrName     = errors.New("asset: not an asset name")
	ErrNotFound = errors.New("asset: not found")
	ErrContent  = errors.New("asset: the content is not what the name says") // wrapped with the reason
	ErrTooLarge = errors.New("asset: too large")
	ErrFull     = errors.New("asset: the directory is full")
	ErrOff      = errors.New("asset: no assets directory")
	ErrWrite    = errors.New("asset: write") // wrapped with the cause
)

// Info describes one asset.
type Info struct {
	Name     string
	Kind     Kind
	Type     string // the media type, by content: image/png, image/jpeg, font/ttf, font/otf
	Size     int64
	Modified time.Time
	ETag     string // `"` + the first 16 hex digits of the SHA-256 of the bytes + `"`
}

// Options configures a Store.
type Options struct {
	// ProbeFont, when set, is the last check of an uploaded font: it gets the path of the temp file.
	ProbeFont func(ctx context.Context, path string) error
}

// KindOf returns the kind an asset name stands for; ok is false for any other name.
func KindOf(name string) (kind Kind, ok bool)
// MaxBytes is the largest body of a kind: picture.MaxImageBytes or picture.MaxFontBytes.
func MaxBytes(k Kind) int64

// Open returns the store of dir and removes the temp files a crash left in it. dir "" gives a store
// whose every method returns ErrOff.
func Open(dir string, opt Options) (*Store, error)
func (s *Store) List() ([]Info, error)                 // by name; only regular files with an asset name
func (s *Store) Stat(name string) (Info, error)
func (s *Store) Read(name string) ([]byte, Info, error)
// Put checks and stores r under name: created reports whether the name was new. match, when not "", is an
// If-Match value (a tag, or "*": the asset exists); noneMatch true is If-None-Match: * (the asset must not exist);
// a failed condition is ErrPrecondition.
func (s *Store) Put(ctx context.Context, name string, r io.Reader, match string, noneMatch bool) (info Info, created bool, err error)
func (s *Store) Delete(name, match string) error
// Totals returns the count and the bytes per kind, for the metrics.
func (s *Store) Totals() map[Kind][2]int64

var ErrPrecondition = errors.New("asset: precondition failed")
```

`Put`'s order, which the tests pin: name (`ErrName`); read at most `MaxBytes(kind)+1` bytes (`ErrTooLarge`); content (an image: `picture.DecodeImage`, and its format must be the extension's; a font: `picture.CheckFont`; else `ErrContent` with the reason); the temp file `.<name>.<8 hex digits>.tmp` with mode 0600 and `fsync`; `ProbeFont` on it for a font (`ErrContent` with the probe's text); then, **under the store's lock:** the conditions, the quota (a new name needs a free slot of `MaxFiles`; the total minus the old size plus the new must be at most `MaxTotalBytes`; else `ErrFull`), the rename and the directory `fsync`. Every failure after the temp file exists removes it. Nothing before the lock holds it, so a slow body or a 10 s probe blocks nobody. Hashes are cached by name, size and modification time. `sync_unix.go`/`sync_other.go` are `internal/config`'s directory `fsync` again (`EINVAL` and `ENOTSUP` ignored), with a comment that names the other copy.

- [ ] **Step 1: Write the failing tests**

```go
func TestKindOf(t *testing.T)
// image: "a.png", "a.jpg", "a.jpeg", "logo_2-b.png"; font: "a.ttf", "a.otf"; not ok: "", ".png", "A.png", "a.PNG", "a.gif", "a.ttc",
// "a.woff", "a", "a.png.tmp", ".a.png.0a1b2c3d.tmp", "a/b.png", "../a.png", "a\x00.png", "a b.png", "..", a 64-character stem + ".png" ok,
// 65 not ok.
func TestPutAndRead(t *testing.T) // a PNG, a JPEG and the fixture font: created true, then false on a replacement; Read returns the bytes;
// Info.Type by content; the ETag is the SHA-256 rule and changes with the bytes; file mode 0600; List is sorted by name
func TestPutRefusesContent(t *testing.T)
// each ErrContent with a reason, the directory unchanged afterwards: a PNG as "a.jpg"; a JPEG as "a.png"; "<svg…" and "<html…" as
// "a.png"; a PNG cut in half; a 2049×10 PNG; a 20000×20000 PNG header with little data; a GIF as "a.png"; a text file as "a.ttf";
// the font with magic "ttcf"; a font whose ProbeFont fails (the probe's text is in the error). And ErrTooLarge: an image of
// MaxImageBytes+1, a font of MaxFontBytes+1 (a reader that counts: at most limit+1 bytes were read).
func TestPutLeavesNoTemp(t *testing.T) // after every refused Put above, and after a ProbeFont that fails, the directory has no dot-file
func TestPutConditions(t *testing.T) // noneMatch on an existing name, match "*" on a missing one, a stale tag: ErrPrecondition, nothing written;
// the current tag: replaced
func TestQuota(t *testing.T) // with the limits lowered through an unexported seam: one file too many → ErrFull; a replacement at the file
// limit succeeds; bytes over the total → ErrFull; a replacement by a smaller file succeeds
func TestDelete(t *testing.T) // removes; ErrNotFound on a second delete; ErrName on a bad name before any file access; a stale match
func TestOpenSweepsTemp(t *testing.T) // ".a.png.0a1b2c3d.tmp" is removed at Open; "notes.txt", ".hidden" and a subdirectory are left and never listed
func TestHandCopiedFiles(t *testing.T) // a PNG written straight into the directory is listed, read and deleted like an uploaded one;
// a directory named "x.png" and a FIFO named "y.png" (Unix) are not listed, and Read of them is ErrNotFound without blocking
func TestOff(t *testing.T) // Open(""): every method returns ErrOff
func TestConcurrentPuts(t *testing.T) // -race: 16 goroutines put two alternating valid images under one name while 4 read it: every Read is
// one of the two, whole; no temp file is left
func TestPutWriteError(t *testing.T) // a read-only directory (Unix, not root): ErrWrite, wrapping the cause
```

- [ ] **Step 2: Run** `go test -race -count=1 ./internal/asset/` — compile failure.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/asset/`, `GOOS=windows go vet ./internal/asset/`, `GOOS=darwin go vet ./internal/asset/` — pass.
- [ ] **Step 5: Commit** `feat(asset): the asset directory: names, content checks, a quota, atomic writes`

### Task 8: `stream.Supervisor` — live edits and refresh

**Files:**
- Modify: `internal/stream/supervisor.go`, `internal/stream/generation.go`, `internal/stream/env.go`, `internal/stream/status.go`
- Create: `internal/stream/live.go`
- Test: `internal/stream/live_test.go`, `internal/stream/supervisor_test.go`, `internal/stream/check_test.go`

**Interfaces:**
- Consumes: `spec.ChangeLive`, `spec.Stream.Assets` (Task 2); `(*pipeline.Pipeline).SetInputs` (Task 3); `picture.LoadFont`, `picture.Watermark.Font` (Task 4); `transcode.Video.NoFallback`, `transcode.Stats.EncoderRefusals`, `transcodetest.EnvRefuse` (Task 6).
- Produces:

```go
// Refresh makes the running generation build its watermark again from the asset directory and hand it to the
// transcoder (R-E7). The channel is closed when that is done, or at once when there is nothing to refresh
// (no generation, no watermark) or the supervisor has stopped. It never blocks.
func (s *Supervisor) Refresh() <-chan struct{}

// watermarkOf returns the transcoder's watermark for sp, read from env.AssetsDir; nil without one.
// ladderOf, and so Check and build, use it; so does the live edit.
func watermarkOf(sp spec.Stream, env Env) (*picture.Watermark, error)

// In the status document's Transcoder: EncoderRefusals uint64 `json:"encoder_refusals"`
```

How a live edit is applied by the `Run` goroutine in `serve` (`live.go`, `applyLive(g, old, new)`), in this order: **(1) the watermark**, when `old` and `new` differ in it and the generation has a transcoder: `watermarkOf(new)` then `g.tr.SetWatermark`; an error is recorded with `s.fail("watermark: ", err)` and the old watermark stays. **(2) the inputs**, when they differ: `g.pipe.SetInputs(g.ctx, inputsOf(new))`; `ErrNotRunning` and a context error are ignored (the generation is ending and the next one is built from the new spec); if the recorded `last_error` starts with `input `, it is cleared (it may name an input that is gone: an M5a deferred minor). **(3) the spec is replaced** (`setSpec`). **(4) the outputs**, when they differ: `reattach`, as for `ChangeOutputs`. Then the edit is answered `spec.ChangeLive`. In `idle` (between generations) a live edit replaces the spec, is answered `ChangeLive`, and ends the wait, as an output edit does there. A refresh is a mailbox entry like an edit, taken in order with them; in `serve` it runs step (1) for the spec in force; in `idle` it is answered at once. `ladderOf` passes `NoFallback` on, and loads a font with `picture.LoadFont` ("watermark: a font needs the assets directory, which is not configured" without one). `Collect` adds `openstreamer_transcoder_encoder_refusals_total` ("Transcoder children that ended because a no_fallback rendition could not open its encoder."), with the labels of `…_encoder_fallbacks_total`. If `internal/api`'s OpenAPI tests compare the status schema with `stream.Status`, add `encoder_refusals` to `internal/api/openapi.json` here; otherwise Task 10 adds it.

- [ ] **Step 1: Write the failing tests** (the package's `rig`, `script` readers and `fakeChild`)

```go
func TestSupervisorLiveInputs(t *testing.T)
// inputs [a] running, a session subscriber "rtsp x" on the served hub. Submit(inputs [b]) answers ChangeLive; the generation number, its
// Since and Rebuilds are unchanged; reader a was closed and b opened once; the subscriber is still open and its frames carry exactly
// one more epoch than before; Status.Inputs is [b] (redacted) and ActiveInput 0; input_switches is 1.
func TestSupervisorLiveInputsKeepsTranscoder(t *testing.T) // the same under a ladder with the fake child: the PID and Spawns are unchanged
func TestSupervisorLiveWatermark(t *testing.T)
// a ladder with watermark text "one": Submit(text "two") answers ChangeLive; the fake child's watermark log gains a line for "two";
// PID, Spawns, generation unchanged; no new epoch on any rendition hub. Then watermark removed (a "none" line), then an image
// watermark added from the assets dir (a line with its byte count): all ChangeLive.
func TestSupervisorLiveEditAllParts(t *testing.T) // one Submit that changes the inputs, the watermark and outputs.rtsp: one answer, ChangeLive;
// each of the three took effect (reader swapped, watermark line, "rtsp " subscribers refused); generation unchanged
func TestSupervisorLiveEditInBackoff(t *testing.T) // during a backoff: answered ChangeLive at once, the wait ends, the next generation is built
// from the new inputs and watermark
func TestSupervisorWatermarkAssetGone(t *testing.T) // the new watermark's image is removed between Check and Submit: answered ChangeLive, the
// child got no new watermark, Status.LastError starts "watermark: " and names the asset, the stream keeps running
func TestSupervisorRefresh(t *testing.T) // an image watermark; the file is replaced by other valid bytes; Refresh's channel closes and the
// child's log has a new line with the new byte count; a Refresh without a watermark, during a backoff, and after Run closes at once
func TestSupervisorRefreshInOrder(t *testing.T) // Submit(watermark A), Refresh, Submit(watermark B) queued while the loop is held: the child sees A, A again, B
func TestSupervisorClearsStaleInputError(t *testing.T) // last_error "input 1: …"; a live edit that removes input 1 clears it; a "transcoder: …" error stays
func TestSupervisorRefusalStatus(t *testing.T) // fakeChild with EnvRefuse=0: Status.Transcoder.EncoderRefusals rises, LastError starts
// "transcoder: " and contains "no_fallback", the metric openstreamer_transcoder_encoder_refusals_total{stream} equals the status
func TestCheckWatermarkFont(t *testing.T) // Check: a spec with a font that is in the assets dir passes; a missing one, a non-font file under a font
// name, and a font without an assets dir each fail with "stream: check: transcode: watermark: …"
```

- [ ] **Step 2: Run** `go test -race -count=1 -run 'TestSupervisorLive|TestSupervisorWatermarkAssetGone|TestSupervisorRefresh|TestSupervisorClearsStale|TestSupervisorRefusalStatus|TestCheckWatermarkFont' ./internal/stream/` — failures (`ChangeLive` still rebuilds; `Refresh` does not exist).
- [ ] **Step 3: Implement.** The mailbox's `edit` gains `refresh bool` and a `done chan struct{}`; `closeMailbox` closes those too. Split Task 2's two `case spec.ChangeLive, spec.ChangeRebuild:` arms: `ChangeLive` gets its own. (`Manager.plan` still reports `rebuild` for it until Task 9; this task's tests talk to the supervisor.)
- [ ] **Step 4: The tests that used an input edit as a rebuild.** `grep -n "Inputs" internal/stream/*_test.go` for edits whose expected answer is `spec.ChangeRebuild` or whose expected outcome is `"rebuild"`: change each to toggle `Layout` between nil and `["video"]` instead, and list them in the task report. No other assertion of those tests changes.
- [ ] **Step 5: Run** `go test -race -count=1 ./internal/stream/` and `go test -race -count=20 -run 'TestSupervisorLive|TestSupervisorRefresh' ./internal/stream/` — pass.
- [ ] **Step 6: Commit** `feat(stream): apply input and watermark edits in place, refresh a watermark's assets, report refusals`

### Task 9: `stream.Manager` — the `live` outcome and asset use

**Files:**
- Modify: `internal/stream/manager.go`; the tests Step 4 names (`internal/e2e/m5a_test.go`, `internal/server/*_test.go`)
- Test: `internal/stream/manager_test.go`

**Interfaces — Produces:**

```go
// In Outcome.Applied, beside the others: "live" — a running stream's inputs or watermark were edited in place.

// AssetUsers returns the names of the streams whose spec refers to the asset, sorted: running, disabled or left stopped.
func (m *Manager) AssetUsers(name string) []string

// RefreshAsset tells every running stream that refers to the asset to load it again, waits for them (or for ctx),
// and returns their names, sorted.
func (m *Manager) RefreshAsset(ctx context.Context, name string) []string
```

`plan` sets `applied` from `spec.Diff`: `outputs`, `live` or `rebuild`. A live input edit is judged exactly as a rebuild is: `Check`, its listen claims against the server's, the holders' and the other touched streams' (its own old claims are not held against it, so a kept listen input does not conflict with itself), and `copy://` cycles.

- [ ] **Step 1: Write the failing tests**

```go
func TestManagerLiveOutcome(t *testing.T) // Replace with one stream's input changed: Outcome "live"; with its watermark changed: "live";
// with outputs only: "outputs"; with a layout change: "rebuild"; the supervisor (pointer) is the same after the live edits
func TestManagerLiveInputClaims(t *testing.T) // stream a edits its inputs to a listen address stream b holds: ErrConflict, nothing changed,
// a's reader not touched; a adds a second listen input and keeps its first: accepted, the first reader was opened once in all;
// a live input edit that makes a copy:// cycle: ErrInvalid
func TestManagerAssetUsers(t *testing.T) // three streams: one with image "logo.png", one disabled with font "f.ttf" and image none, one without
// a ladder: AssetUsers("logo.png") == [first]; ("f.ttf") == [the disabled one]; ("other.png") == nil
func TestManagerRefreshAsset(t *testing.T) // two running streams use "logo.png", one is disabled and names it too: after the file is replaced,
// RefreshAsset returns the two running names and both fake children have a new watermark line; a cancelled ctx returns without waiting
func TestManagerConcurrentLiveEdits(t *testing.T) // Task 1's leases: 20 writers mixing input swaps between two leased inputs, watermark texts,
// output toggles and RefreshAsset for 2 s, then one last Replace: no deadlock, the final spec and view are the last write's,
// l.Overlaps() is empty, the generation number moved only for the rebuild edits among them (none here: it is 1)
```

- [ ] **Step 2: Run** `go test -race -count=1 -run 'TestManagerLive|TestManagerAsset|TestManagerRefreshAsset|TestManagerConcurrentLiveEdits' ./internal/stream/` — failures.
- [ ] **Step 3: Implement.** Both new methods read the immutable set behind the atomic pointer and take no lock.
- [ ] **Step 4: The tests elsewhere that used an input edit as the rebuild.** From this task on an input or watermark edit answers `live`. `grep -rn '"rebuild"\|ChangeRebuild' internal --include='*_test.go'`: every test outside `internal/spec` whose edit changes only `inputs` or the watermark and expects a rebuild (`TestM5aRebuildIsTheOtherRoute` in `internal/e2e/m5a_test.go` is one; `internal/server` has others) switches its edit to `layout: ["video"]` and keeps every assertion. (`["video","audio"]` is no use: Task 2 makes it equal to the empty layout.) List each in the task report.
- [ ] **Step 5: Run** `go test -race -count=1 ./internal/stream/ ./internal/server/ ./internal/api/` and `go test -race -count=1 -run '^TestM5a' ./internal/e2e/` — pass.
- [ ] **Step 6: Commit** `feat(stream): the live outcome, and which streams use an asset`

### Task 10: `internal/api` — the asset routes

**Files:**
- Modify: `internal/api/api.go`, `internal/api/errors.go`, `internal/api/openapi.json`, `internal/api/doc.go`
- Create: `internal/api/assets.go`
- Test: `internal/api/assets_test.go`, `internal/api/openapi_test.go`, `internal/api/auth_test.go` (`TestRoles`, `TestWritesNeedRequestedBy`)

**Interfaces:**
- Consumes: `asset.Info`, `asset.KindOf`, `asset.MaxBytes` and the `asset` errors (Task 7); `stream.Outcome` with `live` (Task 9).
- Produces:

```go
// Asset is the asset document.
type Asset struct {
	Name     string    `json:"name"`
	Kind     string    `json:"kind"` // "image", "font"
	Type     string    `json:"type"` // the media type
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	ETag     string    `json:"etag"`
	UsedBy   []string  `json:"used_by"` // [] when no stream names it
}

// AssetPut is the answer to an upload: the asset, and the running streams that took the new bytes.
type AssetPut struct {
	Asset
	Refreshed []string `json:"refreshed"`
}

// ErrAssetInUse is DeleteAsset's error while a stream names the asset; Streams are their names.
type ErrAssetInUse struct{ Streams []string }

// Added to Backend:
Assets() ([]Asset, error)
Asset(name string) (Asset, []byte, error)
PutAsset(ctx context.Context, name string, body io.Reader, ifMatch string, ifNoneMatch bool) (res AssetPut, created bool, err error)
DeleteAsset(ctx context.Context, name, ifMatch string) error
```

Routes added to the table: `GET /api/v1/assets` (viewer), `GET /api/v1/assets/{name}` (viewer), `PUT /api/v1/assets/{name}` (editor), `DELETE /api/v1/assets/{name}` (editor). The handlers: a `{name}` that `asset.KindOf` refuses is 404 before the backend is asked. `PUT`: `Content-Type` must be `application/octet-stream` (415 `unsupported_media_type`); a `Content-Length` above `asset.MaxBytes(kind)` is 413 at once; the body is wrapped in `http.MaxBytesReader(w, r.Body, limit+1)`; one of 2 upload slots is taken before the body is read and given back after the backend returns (a request whose context ends while it waits is 503 `unavailable`); `If-None-Match` other than `*` is 400. `GET` of one asset: `http.ServeContent` over the bytes with the `ETag`, `Content-Type` from `Asset.Type`, `Content-Disposition: attachment; filename="<name>"`, `Cache-Control: no-cache`. Error rows added to `fail`: `asset.ErrNotFound`, `asset.ErrName` → 404 `not_found`; `asset.ErrContent` → 422 `invalid_asset`, the reason as the message; `asset.ErrTooLarge` → 413 `too_large`; `asset.ErrFull` → 507 `assets_full`; `asset.ErrOff` → 409 `assets_off`, "server.assets_dir is not set"; `*ErrAssetInUse` → 409 `asset_in_use`, `errors` the stream names (through Task 1's `capped`); `asset.ErrPrecondition` → 412 `precondition_failed`; `asset.ErrWrite` → 500 `asset_write_failed`, logged as `config.ErrWrite` is.

- [ ] **Step 1: Write the failing tests** (a fake backend over a real `asset.Store` in a temp dir)

```go
func TestAssetCRUD(t *testing.T)
// PUT logo.png (a PNG) → 201, the document with kind "image", type "image/png", used_by [], refreshed []; GET list → [it];
// GET logo.png → 200, the bytes, Content-Type image/png, the ETag, Content-Disposition attachment, X-Content-Type-Options nosniff;
// GET with If-None-Match: <etag> → 304; PUT again with other bytes → 200 and a new ETag; DELETE → 204; GET → 404; DELETE → 404.
func TestAssetNames(t *testing.T) // every method on "/api/v1/assets/" + each of: "..%2Flogo.png", "%2e%2e%2fx.png", "a%2Fb.png", "X.PNG",
// "a.gif", ".png", "a%00.png", a 65-character stem: 404 not_found, the backend never called; "/api/v1/assets/a/b.png": 404 from the mux
func TestAssetTooLarge(t *testing.T) // an image body of MaxImageBytes+1 with Content-Length: 413 before any byte is read (the body reader
// counts 0); the same chunked: 413 after at most limit+1 bytes; a font of 2 MiB is accepted (the image limit is not the font's)
func TestAssetContent(t *testing.T) // an HTML body as logo.png: 422 invalid_asset, a message that says why; Content-Type text/html on a valid
// PNG: 415; multipart/form-data: 415
func TestAssetConditions(t *testing.T) // If-None-Match: * on an existing name → 412; If-Match stale → 412; If-Match current → 200;
// If-None-Match: "x" → 400
func TestAssetErrors(t *testing.T) // table over the backend's errors: ErrFull → 507 assets_full; ErrOff → 409 assets_off;
// &ErrAssetInUse{[a b]} → 409 asset_in_use with errors [a b]; ErrWrite → 500 asset_write_failed and one error log line
func TestAssetUploadSlots(t *testing.T) // two uploads held in the backend: a third is not passed to the backend until one returns; a third
// whose request is cancelled while it waits answers 503
func TestAssetRoles(t *testing.T) // viewer: GET list and GET one 200, PUT and DELETE 403; no credentials 401; a PUT without X-Requested-By 400
```

Extend `TestRoles` and `TestWritesNeedRequestedBy` with the four routes. `TestOpenAPIMatchesRoutes` fails until `openapi.json` has the four operations, the `Asset` and `AssetPut` schemas, `live` in the outcome's enum with its description ("live: the inputs or the watermark of a running stream were changed in place; no session ended."), and `encoder_refusals` in the transcoder status schema.

- [ ] **Step 2: Run** `go test -race -count=1 ./internal/api/` — compile failure.
- [ ] **Step 3: Implement.** The existing fake backends of the package's tests get the four methods.
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/api/` — pass.
- [ ] **Step 5: Commit** `feat(api): the asset routes`

### Task 11: `internal/server` — wiring, the font probe, the docs

**Files:**
- Modify: `internal/server/server.go`, `internal/server/apply.go`, `internal/server/collect.go`, `internal/server/doc.go`, `cmd/openstreamer/openstreamer.example.yaml`, `README.md`
- Create: `internal/server/assets.go`
- Test: `internal/server/assets_test.go`

**Interfaces:**
- Consumes: `asset.Open`, `asset.Options.ProbeFont` (Task 7); `Manager.AssetUsers`, `Manager.RefreshAsset` (Task 9); the four `api.Backend` methods (Task 10).
- Produces: the server implements them.

Rules. `server.Options` gains `TranscoderEnv []string` ("added to the environment of every transcoder child and of the font probe; tests only, `cmd/openstreamer` never sets it"), passed to `stream.Env.TranscoderEnv`: it is how this package's tests run the fake child (its `TestMain` runs `transcodetest.Main` when `OS_TRANSCODE_FAKE=1`, as `internal/stream`'s does, with `transcoder.binary` set to the test executable) and how Task 12 hides the GPU from one server's children. `server.New` opens the store on `server.assets_dir` (after `config`'s check that it is a directory); with a configured transcoder binary, `ProbeFont` runs `<binary> -probe font <path>` under a 10 s context with stdout and stderr captured to at most 4 KiB, and a non-zero exit is an error with that text. `Assets` and `Asset` fill `UsedBy` from `Manager.AssetUsers`. `PutAsset` calls `store.Put` **without** the apply mutex, then takes it (`s.cfg`) for `Manager.RefreshAsset`, so a refresh and a stream edit are in one order. `DeleteAsset` takes the apply mutex, asks `AssetUsers`, returns `&api.ErrAssetInUse{…}` when any, and deletes otherwise. `collect` adds `openstreamer_assets{kind}` ("Assets in the assets directory, by kind.") and `openstreamer_assets_bytes{kind}` from `Store.Totals`; no series without an assets directory.

- [ ] **Step 1: Write the failing tests** (the package's server rig on ephemeral ports, a temp assets dir, the fake child)

```go
func TestAssetReachesRunningStream(t *testing.T) // a running ladder stream with watermark image "logo.png": PutAsset("logo.png", other bytes)
// returns refreshed [cam]; the fake child's watermark log has a line with the new byte count; the stream's generation is unchanged
func TestDeleteAssetInUse(t *testing.T) // refused with ErrAssetInUse{[cam]} while cam names it, also when cam is disabled; after a stream edit
// that drops the watermark the delete succeeds; a stream PUT that names a deleted asset is refused with 422 (Check)
func TestFontProbe(t *testing.T) // a transcoder stand-in script that exits 1 printing "font: bad" for one file name: PutAsset of that font is
// asset.ErrContent with "font: bad" in its text and no file is left; exit 0: stored. Without a transcoder: stored without a probe
func TestAssetsOff(t *testing.T) // no assets_dir: Assets and PutAsset return asset.ErrOff; no assets series in the scrape
func TestAssetMetrics(t *testing.T) // one image and one font: openstreamer_assets{kind="image"} 1, {kind="font"} 1, the byte gauges their sizes
func TestConcurrentAssetAndStreamWrites(t *testing.T)
// 20 writers for 2 s: PutStream toggling cam's watermark between an image, a text with a font and none; PutAsset alternating two valid
// images under the image's name; DeleteAsset of both names. Every result is nil, ErrAssetInUse, asset.ErrNotFound or a 422 for a
// missing asset, never another error; no deadlock (a 20 s guard); at the end the config file equals the last stream written, every
// asset the final spec names exists, and no goroutine is left.
func TestLiveEditOverTheApplyPath(t *testing.T) // PutStream with new inputs: Outcome "live", the config file holds the new inputs, the
// stream's generation is unchanged; a whole-config PUT with the same kind of edit reports "live" for it and "unchanged" for the others
```

- [ ] **Step 2: Run** `go test -race -count=1 ./internal/server/` — compile failure.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Docs.** `openstreamer.example.yaml`: a commented `font: brand.ttf` under the watermark example and a commented `no_fallback: true` on the NVENC rendition, each with one line of explanation. `README.md`: (a) in the edits section, replace "any other change to a stream restarts that stream" by the three outcomes: `outputs`, `live` (inputs and the watermark: no session ends; replacing the active input is one discontinuity; to switch without a gap, add the new input first and remove the old one after the fail-back hold of 10 s) and `rebuild`; (b) a section "Assets" with the four routes, the names, the limits, a `curl --data-binary @logo.png -H 'Content-Type: application/octet-stream'` upload, what `refreshed` and 409 `asset_in_use` mean, and that files copied into the directory by hand are assets too but reach a running stream only at its next build or edit; (c) fonts: `watermark.font`, TTF or OTF, one face per file, checked by the transcoder at upload; (d) `no_fallback`: what happens when NVENC cannot be opened, the metric and the status field, and that it takes the ladder's other renditions down with it.
- [ ] **Step 5: Run** `go test -race -count=1 ./internal/server/ ./cmd/openstreamer/` and `make lint` — pass.
- [ ] **Step 6: Commit** `feat(server): serve the asset API, probe fonts with the transcoder, refresh running streams` and `docs: live edits, assets, fonts and no_fallback`

### Task 12: The M5b1 scenarios

Needs the bundle of Task 5 (`make ostranscoder`). The tests are in `internal/e2e/m5b1_test.go`, package `e2e`, on M5a's harness (`m5aServer`, `m5aOpen`, `assertUninterrupted`, `tap`, `running`, `metric`) and M4c's picture helpers (`firstPicture`, `marked`, `topLeftBand`, `meanAbsDiff`, `magentaPNG`). Every test is `t.Parallel()` and binds only `127.0.0.1:0`.

**Files:**
- Create: `internal/e2e/m5b1_test.go`
- Modify: `internal/e2e/m5a_test.go` (only a variant of `m5aStart` that takes `server.Options`), `.github/workflows/ci.yml` (`E2E_TESTS`)

**Interfaces:** consumes the API as a client; produces the proof of R-E10.

- [ ] **Step 1: Write the tests**

```go
// TestM5b1ReplaceInputs: clause 1 of the row, on a passthrough stream.
// A server with every output on; stream cam with inputs [A] (the looped fixture, as m5aSource); an m5aOpen row with a session reader
// on HTTP-TS, RTMP, RTSP and SRT, the HLS and DASH watchers, and a tap on the hub. After 3 s: PUT cam with inputs [B] (a second
// source of the same fixture). Asserts: the PUT answers 200 and "live"; the status' generation and since are unchanged and
// rebuilds is 0; within 10 s the tap has frames of exactly one more epoch than before the edit, and no further epoch in the
// next 3 s; assertUninterrupted(row) with no exception: every session reader holds every hub frame once, across the epoch change,
// and no openstreamer_output_sessions_ended_total series moved; the HLS media sequence went on (never back) with exactly one
// EXT-X-DISCONTINUITY more; the config file holds [B]; input_switches is 1.
// Then PUT inputs [B, A] (an input added below the active one): "live", no new epoch within 3 s, sessions still uninterrupted.
func TestM5b1ReplaceInputs(t *testing.T)

// TestM5b1ReplaceInputsLadder: invariant 3 under a real child. A ladder of one x264 rung (height 180) and audio copy, HTTP-TS on.
// PUT with a new input: "live"; the status' transcoder.pid and spawns are unchanged; a tap on cam_<label> sees exactly one more
// epoch; an HTTP-TS reader of cam_<label> opened before the edit is still reading 3 s after it and its capture has no violation
// (conform.CheckFrames with the epoch change allowed).
func TestM5b1ReplaceInputsLadder(t *testing.T)

// TestM5b1Assets: clause 2, and the live watermark edit. One server with an assets dir and the real child; cam with a ladder of two
// x264 rungs (heights 180 and 120) and a text watermark "M5B1"; taps on both rendition hubs. picture(i) decodes the first picture
// of rung i's latest GOP. Steps, each asserting on BOTH rungs, with the transcoder's pid, spawns and the generation unchanged
// throughout, and no new epoch on either tap:
//  1. plain text: the top-left band is recorded as the DejaVu band.
//  2. viewer PUT /assets/logo.png → 403. editor PUT (magentaPNG) → 201, used_by [], refreshed [].
//  3. PUT cam with watermark {image: logo.png, size_pct: 25} → "live"; within 10 s marked(picture(i), TopLeft) is true.
//  4. PUT /assets/logo.png with a green PNG of the same size → 200, refreshed [cam]; within 10 s the mark region is within 40 of
//     (0,255,0) on both rungs. No stream PUT was made.
//  5. DELETE /assets/logo.png → 409 asset_in_use, errors [cam]. GET /assets lists it with used_by [cam].
//  6. PUT /assets/mono.ttf (the fixture font) → 201 (the probe ran: the bundle is configured). PUT cam with watermark
//     {text: "iiiiiiii", font: mono.ttf, size_pct: 20} → "live". Then PUT the same without the font → "live". The lit width of the
//     top-left band with the font is at least 1.5 times the width without it, on both rungs.
//  7. DELETE /assets/logo.png → 204 (no stream names it now); DELETE /assets/mono.ttf → 204.
//  8. PUT /assets/evil.png with an HTML body → 422 invalid_asset; PUT "/assets/..%2Fx.png" → 404; nothing but the server's own files
//     is in the assets dir or its parent.
func TestM5b1Assets(t *testing.T)

// TestM5b1NoFallback: clause 3. Needs NVENC to be unavailable to the child: where /dev/nvidiactl does not exist (CI, the peers
// image) that is so; on a GPU host the test follows Task 5 Step 5's finding (server.Options.TranscoderEnv = ["CUDA_VISIBLE_DEVICES="]
// for the server it starts, never os.Setenv; or it skips with that reason). cam: renditions [{label: hi, height: 180, bitrate_kbps: 400, encoder: nvenc, no_fallback: true},
// {label: lo, height: 120, bitrate_kbps: 200}], HTTP-TS on. Asserts, within 20 s: openstreamer_transcoder_encoder_refusals_total
// {stream="cam"} ≥ 2 and equal to the status' transcoder.encoder_refusals; encoder_fallbacks_total is 0; the status' last_error
// starts "transcoder: " and contains "no_fallback"; taps on cam_hi and cam_lo have no video frame; the source is running (state
// "running", copy://cam delivers); a second, passthrough stream on the same server plays over HTTP-TS without a violation.
// Then PUT cam without no_fallback → "rebuild"; within 20 s both rungs deliver frames and encoder_fallbacks_total is 1.
func TestM5b1NoFallback(t *testing.T)
```

- [ ] **Step 2: Run** `go test -race -count=1 -run '^TestM5b1' ./internal/e2e/` on the host — they pass (every task before this one is merged; a failure here is a defect of an earlier task: fix it there, with a test in its package).
- [ ] **Step 3: The budget.** `time go test -race -count=1 -run '^TestM5b1' ./internal/e2e/`: at most 90 s. Over it: shorten the waits that are sleeps (the "no further epoch in the next 3 s" windows may go to 2 s), never an assertion.
- [ ] **Step 4:** `go test -race -count=10 -run '^TestM5b1' ./internal/e2e/` — 10 of 10. Then `make test-ci-peers ARGS='-run ^TestM5b1 ./internal/e2e/'` — pass with ffmpeg 6.1.
- [ ] **Step 5: M5a still holds.** `go test -race -count=1 -run '^TestM5a' ./internal/e2e/` — pass (Task 9 moved the tests that used an input edit as the rebuild to a layout edit; `TestM5aRebuildIsTheOtherRoute` still proves that a rebuild ends every session).
- [ ] **Step 6: CI.** Add `TestM5b1ReplaceInputsLadder TestM5b1Assets TestM5b1NoFallback` to `E2E_TESTS` of the `transcode-e2e-arm64` job (its listed-tests guard then checks the names).
- [ ] **Step 7:** `make test` (whole repository), `make test-ci-peers`, `make lint`, `make covercheck` — pass.
- [ ] **Step 8: Commit** `test(e2e): the M5b1 scenarios`

### Task 13: CI, docs, spec, done

**Files:**
- Modify: `docs/REBUILD_PLAN.md`, `CLAUDE.md`, `.testcoverage.yml` (only if a new exclusion is needed, with its reason)

- [ ] **Step 1: `docs/REBUILD_PLAN.md`.** (a) §5: replace the M5b row by two rows. **M5b1** (`[plan](superpowers/plans/2026-11-26-m5b1-live-edits-assets.md), done`): contents as R-E1 lists; exit criterion the row's first three clauses; "Proved by" the four `TestM5b1…` tests with the pipeline, spec, asset, transcode, native, stream, api and server suites. **M5b2** (depends on M5a): "Audio language: the spec field, the HLS masters' `LANGUAGE`, the MPD's `lang`, M4b's multi-audio minors"; exit criterion "`osconform` reads the languages". Update every other "M5b" mention to the part it means (the lines `grep -n "M5b" docs/REBUILD_PLAN.md` prints; "Several audio languages" and "Audio language" → M5b2). (b) §3.6 "Edits": the three outcomes and R-E3's rule in four sentences; a new bullet **Assets** (R-E5 to R-E7 in one paragraph). (c) §3.5: wire version 3, the watermark's font, `no_fallback` and `EncoderRefusals`. (d) §4: `internal/asset/` in the layout. (e) An "Out of M5b1" table: audio language (M5b2); hot-reloadable listeners (R-E11); asset versions and metadata (R-E5); multipart uploads (R-E6); a `no_fallback` rung that fails alone (R-E9); font collections and WOFF (R-E8); a replace of the active input without a gap (R-E3: add, wait, remove). (f) Backlog: close "Operator fonts for the watermark" and the `no_fallback` half of "A fallback to libx264 can overload a host sized for NVENC"; add "A replaced active input leaves the hub silent until its successor delivers (R-E3)", "A font that passes the probe and fails later respawns the child until the watermark is edited (R-E8)", "A refused ladder retries every 5 s for ever (R-E9)", "Hand-copied assets reach a running stream only at its next build, edit or API replacement (R-E7)"; in "Split `internal/e2e`" record the package's time from this PR's CI run. (g) "M5a deferred minors": strike the two this plan did (the full layout list; the stale input error) and add an "M5b1 deferred minors" list from the task reviews.
- [ ] **Step 2: `CLAUDE.md`.** In "The server": the asset routes and the `asset` package in one bullet; "Two routes of an edit" becomes the three outcomes with `ChangeLive`, `Pipeline.SetInputs`, `Transcoder.SetWatermark` and `Supervisor.Refresh` named; delete "An input edit is a rebuild until M5b."; under Invariants keep the watermark line and add "an operator's font reaches the child as bytes, never as a path". Keep the file's length within ten lines of what it is.
- [ ] **Step 3: The whole gate.** `make check`, `make covercheck`, `make test-ci-peers`, `make test-transcode`, `GOOS=windows go vet ./...`, `GOOS=darwin go vet ./...`, `go mod tidy` (no change). On this host: `make test-nvenc` when the GPU is free (it must still pass: `no_fallback` is off in M4c's ladders); if the GPU is busy, say so in the PR and do not use `OS_NVENC_SHARED` without the controller's word.
- [ ] **Step 4: Push and read CI.** After the push, read the duration of `internal/e2e` in the `test` job. Above 17 min: stop, and do the Backlog item "Split `internal/e2e`" as its own plan before the merge (R-E10). Otherwise record the figure in the Backlog item.
- [ ] **Step 5: Commit** `docs: M5b1 is done; M5b2 is audio language`

---

## Hand-over: what M5b2 needs decided

M5b2 is not planned here. Its design needs one answer from the user first: **is the audio language one declared value for the stream's single audio track** (a spec field such as `audio_language: vi`, written as `LANGUAGE` on an `EXT-X-MEDIA` audio entry of the HLS masters and as `lang` on the MPD's audio AdaptationSet, with `osconform` reading both), **or several audio tracks chosen and labelled by language** (the layout takes more than one audio, the demuxers report each track's language from the PMT, the HLS rendition list or the SDP, the masters list one `EXT-X-MEDIA` per language and TS variants stop muxing every audio track)? The recommended default is the first: it closes the row's clause with `pkg/hls`, `pkg/dash`, the packager and `internal/conform` alone, and leaves multi-audio as a milestone of its own with M4b's deferred minors.
