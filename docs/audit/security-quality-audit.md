# Open Streamer — Production Security & Stability Audit

**Scope:** Full-codebase audit of the live-video media server (`/Users/thuan.nguyen.trong/Desktop/workspace/thuannt/Open-Streamer`). Findings below were each independently verified against source; line citations are to the files as audited. No verdict in the input set was `false-positive`, so nothing was dropped on that basis. Overlapping symptoms that share one root cause have been merged and cross-referenced.

---

## 1. Executive Summary

**Overall health:** The media pipeline (ingest → buffer hub → transcode → publish) is competently engineered for its *happy path* (UDP/raw-TS sources, H.264, single-config transcode, HLS/DASH out), with strong invariant discipline visible in the timeline normaliser, DASH packager, and NVENC decoder-swap. However, the **control plane is completely unauthenticated**, which converts a cluster of "operator-only" input-handling weaknesses (filtergraph injection, SSRF, arbitrary file read/write) into **remote-unauthenticated** primitives. Separately, several **supported configurations silently fail** — AV-path (RTSP/RTMP) transcode, HEVC, template-inherited inputs, copy/mixer over templated upstreams, and DVR of raw-TS sources all produce dead or corrupt output with misleading "healthy/recording" status.

**Confirmed findings by severity (deduplicated):**

| Severity | Count |
|---|---|
| Critical | 2 |
| High | 19 |
| Medium | 10 |
| Low | 6 |

**Top 5 to fix first:**

1. **Mount authentication on the admin API** (`internal/api/server.go:154-258`). Critical and load-bearing — it is the trust boundary that makes the hook file-write, watermark injection, SSRF, and VOD file-read findings remotely exploitable. Default bind is `:8080` (all interfaces).
2. **Hook file/HTTP sink containment** (`internal/hooks/service.go:337`, `internal/hooks/batcher.go:414`, `internal/api/handler/hook.go:84/128`). Unauthenticated arbitrary file create/append + blind SSRF; `Create`/`Update` persist with zero validation.
3. **Watermark `font_color` lavfi injection** (`internal/transcoder/native/watermark.go:239`). Arbitrary server-side file read into video + transcoder crash-loop, reachable via any stream/template `PUT`.
4. **SSRF egress guard on all server-side fetchers/dials** (`internal/ingestor/pull/hls.go`, `httpts.go`, `internal/manager/service.go`). Content-/redirect-chosen targets reach cloud metadata and internal services.
5. **DVR data-loss cluster** (`internal/dvr/blob/service.go:96-100`, `internal/dvr/blob/writer.go:112-119`). Catalog wiped on every restart (orphaned blobs, retention defeated, unbounded disk growth) **and** raw-TS streams record zero bytes while reporting "recording". Both are triggered by routine operations, no attacker needed.

---

## 2. Confirmed Findings

### 2.1 Security

---

#### S-1 (CRITICAL) — No authentication or authorization on the control-plane HTTP API
**Files:** `internal/api/server.go:154-258` (`buildRouter`), `internal/runtime/manager.go:155/236` (start), `:163` (`middleware.RealIP`)
**Merges:** five reported findings (API server, runtime manager, "mutating admin API", RealIP-spoof escalation, DVR/media-route auth).

**Trigger:** Any client that can reach `cfg.HTTPAddr` calls `POST /config`, `PUT /config/yaml`, `POST/DELETE /streams/*`, `POST /templates`, `POST /hooks`, `POST /vod`, `POST /watermarks`, `DELETE /sessions/{id}`, `GET /metrics`, and all DVR/media playback routes — no credential is checked. The middleware chain is only `RequestID(157)`, `RealIP(163)`, optional CORS, `Recoverer(167)`, access logger, duration metric, `Timeout(170)`. The shipped sample config binds `:8080` (all interfaces — `examples/data/open_streamer.json:7`, `docs/CONFIG.md:69`), and `StartWithConfig(351-356)` uses a bare `http.Server` with no TLS.

**Impact:** Full unauthenticated takeover — replace global config (`config_yaml.go:124` → `rtm.Apply`, rebinds listeners, stop/delete every stream via `removeStreams`), create/modify/delete streams and templates, upload watermark/VOD files, read webhook HMAC secrets (S-9) and push keys (S-10). `middleware.RealIP` additionally rewrites `r.RemoteAddr` from `X-Forwarded-For`/`X-Real-IP` unconditionally, so the only stated control (a "trusted reverse proxy", acknowledged in the `server.go:158-163` comment) is itself spoofable and unenforced in code. This is the **enabling precondition** for S-2, S-3, S-4, S-5, S-6.

**Root cause:** Authn/authz delegated entirely to an out-of-band proxy assumption; no in-process gate, no auth concept in `config.ServerConfig`.

**Fix:** Add a fail-closed auth middleware (constant-time bearer/API-key compare via `subtle.ConstantTimeCompare`, or mTLS) mounted on every admin route group (`/config*`, `/streams*`, `/templates*`, `/hooks*`, `/watermarks*`, `/vod*`, `/sessions*`); allowlist only `/healthz`, `/readyz`, and (per policy) `/metrics` and the media catch-all. Refuse to start (or bind `127.0.0.1` only) when no credential is configured and the bind host is non-loopback. Replace `middleware.RealIP` with a non-mutating client-IP extractor that trusts `X-Forwarded-For`/`X-Real-IP` only from a configured trusted-proxy CIDR (this also closes S-17). For DVR/media playback specifically, gate behind a signed-URL/per-stream token if the media port is exposed, or split admin and media onto separate listeners.

---

#### S-2 (CRITICAL) — Unauthenticated arbitrary host-file create/append (and blind SSRF) via hooks
**Files:** `internal/api/handler/hook.go:84` (Create) / `:128` (Update); file sink `internal/hooks/service.go:337-365` (`deliverFile`, only guard `filepath.IsAbs` at `:342`, `O_APPEND|O_CREATE|O_WRONLY 0o644` at `:356`); HTTP sink `internal/hooks/batcher.go:414-446` (`postOnce`); store `internal/store/json/store.go:328-333`.
**Merges:** "file-type hook arbitrary write", "hook create/update skip validation".

**Trigger:** `POST /hooks {"type":"file","target":"/abs/path","enabled":true,...}` — `Create` decodes straight into `domain.Hook` and calls `hookRepo.Save` with **zero validation** (no `Hook.Validate()` exists; `validateHookTypeTarget` runs only on the YAML path, `config_yaml.go:770`). Then `POST /hooks/{id}/test` (`hook.go:184` → `DeliverTestEvent` `service.go:112-134`) calls `deliverFile` synchronously, or any matching live event (e.g. open an HLS session) fires dispatch (`service.go:228-233`). With nil `stream_codes` + empty `event_types`, `matches()` (`service.go:311-325`) returns true for every event. For `type:"http"`, `postOnce` issues an outbound POST to any URL (e.g. `http://169.254.169.254/...`) on the shared `&http.Client{}` (`service.go:87`) with no `DialContext` restriction.

**Impact:** Create/append files anywhere the service user can write (`0o644`): appending to the JSON store breaks `readAll`'s whole-file unmarshal → total API DoS; unbounded appends fill disk; blind SSRF to internal/cloud-metadata POST-actionable endpoints. The `/test` endpoint's 200-vs-502 response is a reachability oracle.

**Severity nuance (from verification):** `deliverFile` writes `json.Marshal(event)+"\n"` with `O_APPEND` (no truncate); JSON escaping turns raw newlines into `\n`, so injecting a *clean* multi-line crontab/`authorized_keys` entry is not possible — the file primitive is "create-anywhere + append-escaped-JSON" (DoS/integrity), and the genuinely novel capability beyond the already-open admin API is the **SSRF pivot**. One verdict therefore scored this High; given the critical file-write/DoS reachability it is reported here as Critical with the nuance noted.

**Root cause:** REST `Create`/`Update` perform no schema/target validation; the file sink trusts any absolute path with no directory allowlist; `postOnce` has no host/scheme allowlist.

**Fix:** (1) In `deliverFile`, confine `Target` to a configured root: `filepath.Clean` then `pathInside(target, allowedDir)`, rejecting `..` escape and `filepath.EvalSymlinks` targets outside the root. (2) Validate at the REST boundary in `Create`/`Update` (reject unknown types; for file hooks enforce the same containment; for http hooks reject internal hosts). (3) Give the hooks `http.Client` a `Transport.DialContext` `Control` func that rejects loopback/link-local/RFC1918/ULA at connect time (defeats DNS-rebind/redirect) and a `CheckRedirect` that re-applies it; optionally require a non-empty `Secret` for http hooks. (4) Lift `validateHookTypeTarget` (extended with containment) into a shared validator used by both REST and YAML paths.

---

#### S-3 (HIGH) — Lavfi filtergraph injection via unescaped watermark `font_color`
**Files:** `internal/transcoder/native/watermark.go:239` (`buildTextFilter`, `"fontcolor="+fontColor`), `:305` (`escapeLavfiArg`); `internal/domain/watermark.go:152`/`stream.go:674` (`Validate` never checks `FontColor`); `internal/transcoder/supervisor.go:617` (verbatim copy); `internal/transcoder/native/server.go:293` (proto→config).
**Merges:** two reported font_color findings (identical root cause).

**Trigger:** `PUT /streams/{code}` (or a template) with `transcoder.watermark = {enabled:true, type:"text", text:"x", font_color:"white,drawtext=textfile=/etc/passwd:fontcolor=white:x=0:y=100"}`. `Validate()` checks opacity/font_size/position/text/filename but **never `FontColor`**, so the body passes. `FontColor` flows unchanged → `buildTextFilter` joins it unquoted into the colon-separated `drawtext` option list, and a `,` chains a second attacker-controlled `drawtext`/`movie` filter into the graph parsed by `graph.Parse` (`watermark.go:136`).

**Impact:** An injected `drawtext textfile=`/`movie=` reads an arbitrary server-side file and renders its bytes into every transcoded rendition, which any unauthenticated HLS/DASH viewer captures — **arbitrary local file disclosure**. A malformed payload instead fails `graph.Parse` → terminal subprocess error → respawn loop (per-stream DoS). Secondary: `escapeLavfiArg` escapes backslash+colon but **not** single-quote, so a `FontFile`/`AssetPath` containing `'` breaks out of `fontfile='...'`/`movie='...'`.

**Root cause:** `FontColor` concatenated unquoted/unescaped into the lavfi description; `Validate` whitelists nothing; `escapeLavfiArg` omits single-quote escaping. (`text` and `fontfile` *are* single-quoted — the bug is the omitted guard on `fontcolor`/`x`/`y`.)

**Fix:** In `domain.WatermarkConfig.Validate`, reject `FontColor` that is not a strict color literal (named color | `#RRGGBB[AA]` | `name@<float>`). In `buildTextFilter`, single-quote-wrap and escape `fontcolor` exactly like `text`. Extend `escapeLavfiArg` to escape `'`→`'\''` and apply it to `fontcolor`, `fontfile`, and the `movie` asset path. Reject lavfi metacharacters in font paths.

---

#### S-4 (HIGH) — SSRF via unvalidated ingest/pull input URLs (content- and redirect-chosen targets)
**Files:** `internal/api/handler/stream.go:660-679` (`decodeStreamBody` validates code/priority/uniqueness/watermark only — never URL host); `internal/manager/service.go:416/901/1021/1187/1227` (forwards raw `Input.URL` to ingestor on register/failover/probe/switch); pull readers `internal/ingestor/pull/httpts.go:76-85`, `hls.go:417-439/465-484`; `resolveHLSURL hls.go:659-671`; clients at `hls.go:213-214` are bare `&http.Client{}` with **no `CheckRedirect`**.
**Merges:** three reported SSRF findings (HLS playlist-content SSRF, manager-forwarded SSRF, domain-input SSRF).

**Trigger:** `POST /streams/{code}` with input `http://169.254.169.254/latest/meta-data/...` or `http://127.0.0.1:<admin-port>/...`. No save-time host check exists. On start, the HTTP-TS/HLS reader GETs the URL server-side; for HLS the **content of the remote playlist** chooses the next GET (`resolveHLSURL` passes absolute http(s) through unchanged, `:663-665`; variant URIs `:593` and segment URIs `:602` both feed it), and the default client auto-follows up to 10 redirects to any host. `input.Headers` are re-applied on every request and **not stripped on cross-host redirect**, leaking configured upstream credentials. RTSP/RTMP/SRT/UDP readers also dial arbitrary hosts (lower-value, no body channel).

**Impact:** Server-side requests to cloud metadata / internal services / port-scan; credential-header leak to attacker-chosen hosts. **Blind** SSRF in practice — fetched bytes go through the TS demuxer, so non-TS responses (IMDS JSON, `/etc/passwd` text) are dropped and do not surface via playback (this corrects the "exfiltrate response bytes via playback" framing in the original findings; full re-serve works only for TS/HLS-shaped internal targets). `file://` is **not** an arbitrary-read vector — `KindFile` is confined through `VODResolver.Resolve` (`internal/vod/registry.go:171-202`) with `pathInside` (corrects a sub-claim). High stands because of cloud-metadata reachability + credential-header leak + zero egress controls + anonymous reachability (S-1).

**Root cause:** No SSRF egress policy anywhere; save-time validation ignores scheme/host; dial-time fetchers accept any host and follow redirects.

**Fix:** Enforce at **dial time** (redirects/rebinding/playlist-host chains bypass save-time checks). Install a shared `net.Dialer.Control`/`DialContext` guard on the http transports in `httpts.go` and `hls.go` (both `plClient` and `segClient`) that rejects resolved IPs in loopback/link-local-incl-IMDS/RFC1918/ULA; set `CheckRedirect` to cap depth and re-validate. Because legitimate inputs use private IPs (LAN RTSP cameras, internal multicast), make broad RFC1918 blocking an operator opt-*out* (`ingestor.allow_private_targets`, default false→blocked) while blocking loopback/link-local/IMDS unconditionally. Apply the same resolved-IP guard to RTSP/RTMP/SRT/UDP dials and add a fast-feedback scheme/host check in `decodeStreamBody`. Do not text-parse `resolveHLSURL` alone.

---

#### S-5 (HIGH) — Unauthenticated arbitrary host-file read via VOD `/raw`
**Files:** `internal/api/handler/vod.go:265-299` (`Raw`, `http.ServeFile` with no extension policy), `:89` (`Create`), `:482-494` (`ensureMountStorageWritable`); `internal/vod/registry.go:137-152` (`ResolvePath`, lexical `pathInside` only, `:169-170` deliberately no symlink resolution); `internal/domain/vod.go:52-61` (`ValidateStorage` requires only absolute path).
**Merges:** two reported VOD findings.

**Trigger:** `POST /vod {"name":"x","storage":"<writable dir holding secrets>"}` then `GET /vod/x/raw/config.yaml` (or `raw/hooks.json`). `Raw` serves any file by exact path — the `IsVideoFile` allowlist exists only in `ListFiles` (`registry.go:252`) and `UploadFile` (`vod.go:344`), so listing hides non-video files but raw-serve returns them.

**Impact:** Unauthenticated read of JSON store files (containing hook HMAC secrets), `config.yaml`, and any file in a writable subtree (plus symlink targets out of it). Bounded to directories the service user can write (`ensureMountStorageWritable` `MkdirAll`+probe — a genuine constraint; arbitrary system-file read requires running as root). The narrow same-mount disclosure (a secret co-located in a legitimate media mount, hidden by `ListFiles` but served by `Raw`) is fully reachable with no assumptions.

**Root cause:** Inconsistent policy — listing enforces the video allowlist, raw-serve does not; mount storage root is unconstrained.

**Fix:** In `Raw`, after `ResolvePath`/`Stat`, reject `!vod.IsVideoFile(abs)` with 404 (put the check in the handler, not `ResolvePath`, which `DeleteFile` shares). Defense-in-depth: constrain VOD storage roots to a configured allowlist / reject overlap with config/store dirs in `ValidateStorage`; `filepath.EvalSymlinks` both target and root before the `pathInside` check.

---

#### S-6 (MEDIUM) — Blind SSRF via HTTP hook target
Covered as the HTTP-sink half of **S-2**. `batcher.go:414-446` POSTs to any operator-supplied URL with no host allowlist; downgraded from High by verification because delivery is blind (`DeliverTestEvent` returns nil immediately, `service.go:128-132`) and POST-only (weakens IMDS GET credential-theft). Fix: the `DialContext` guard described in S-2.

---

#### S-7 (MEDIUM) — Secrets in logs: SRT passphrase and credentialed pull URLs logged in cleartext
**Files:** `internal/ingestor/pull/srt.go:76-81` (`slog.Info(... "url", r.input.URL)` on every connect) + `:66` (URL in error); `internal/ingestor/worker.go:103-107` (connect) / `:254-260` (`handleOpenFailure`, repeated during backoff); `hls.go:210-211/308/382/387`; `httpts.go:78/87/93` (these are `fmt.Errorf` wraps, not slog — minor locator correction, but the wrapped URL still reaches the log via `handleOpenFailure`'s `"err"` field); also `rtsp.go` at 12 sites (`:120,135,150,204,231,309,333,405,420,449`) and `rtmp.go:164`.

**Trigger:** Any SRT input `srt://host?...&passphrase=secret` or credentialed pull URL (`http://user:pass@host`, `?token=...`, RTSP `user:pass@`) is logged at Info on connect/reconnect/open-failure. A passphrase supplied via `input.Params` (not the URL) is *not* logged — only URL-embedded secrets leak.

**Impact:** Encryption passphrases and upstream credentials written to logs / journald / shippers, re-emitted on every reconnect loop, persisting beyond intended scope.

**Root cause:** Raw URLs logged without redacting userinfo or sensitive query keys. No redaction helper exists anywhere in `internal/`.

**Fix:** Add a `redactURL(raw string)` helper (strip userinfo via `u.Redacted()`, redact `passphrase|token|password|key|secret|auth|apikey` query values) and apply at `worker.go:106/257`, `srt.go:66/77`, `httpts.go:78/87/93`, `hls.go:211/308/382/387`, all `rtsp.go` URL sites, and `rtmp.go:164`. Keep the raw URL for dialing/`UnmarshalURL`.

---

#### S-8 (MEDIUM) — Push ingest has no authentication; auto-publish lets any client materialise streams
**Files:** `internal/ingestor/push/rtmp_server.go:350-402` (`OnNewRtmpPubSession`, target from `rtmpRouteKey` only — no secret read), `:232-257` (`acquireOrAutoPublish`); `internal/ingestor/registry.go:76-89` (`Acquire` checks only registered + not-active); `internal/ingestor/service.go:607-617` (`pushStreamKey` = stream code); `internal/autopublish/service.go:143-204` (`ResolveOrCreate` — prefix match + code-shape validation only).

**Trigger:** Any client that can TCP-connect to the RTMP push port and knows/guesses a registered stream code can publish into an **idle** `publish://` slot (the full path is the only credential). With an `AutoPublishResolver` wired, a push to any path matching a template prefix synthesises a runtime stream and starts a pipeline.

**Impact:** Content injection/takeover of idle `publish://` streams; unauthenticated creation of arbitrary runtime streams (each spinning a coordinator pipeline + observer goroutine) — resource exhaustion, no concurrent-runtime-stream cap and no rate limit. Decisive: `domain.Stream.StreamKey` (`stream.go:70-71`) and `Template.StreamKey` (`template.go:67-70`) are documented as "the shared push-authentication secret" but are **read only in the `ResolveStream` merge** (`template.go:236-237`) — never consulted in `OnNewRtmpPubSession`/`Acquire`/`ResolveOrCreate`. The auth control exists in the data model but is entirely unwired. Default config enables `listeners.rtmp` on `0.0.0.0:1935`.

**Fix:** Wire the existing `StreamKey`: in `OnNewRtmpPubSession`, parse a secret from the stream name/tcUrl query, resolve the configured `StreamKey`, and require a match before `Acquire` (reject when set-but-absent/mismatch). For auto-publish, require `Template.StreamKey` match in `ResolveOrCreate` before `coordinator.Start`, plus a configurable cap on concurrent runtime streams and per-source-IP rate limit. Encourage binding the listener to a trusted interface. Until enforced, the "authenticate push ingest" doc comment is aspirational.

---

#### S-9 (MEDIUM) — Hook HMAC secrets disclosed via GET /hooks and GET /config/yaml
**Files:** `internal/domain/hook.go:55` (`Secret` with `json:"secret" yaml:"secret"`, no redaction); `internal/api/handler/hook.go:65-72` (List), `:106-114` (Get), `:95/:146` (Create/Update echo); `internal/api/handler/config_yaml.go:60-101` (`GetConfigYAML` marshals `Hooks`), `:207/227` (PUT success returns `hooksAfter`). Secret is the HMAC-SHA256 key (`internal/hooks/batcher.go:413-427`, sets `X-OpenStreamer-Signature`).

**Trigger:** `GET /config/yaml` or `GET /hooks` returns every hook's signing secret verbatim. Reachable unauthenticated (S-1); transits plaintext (no TLS).

**Impact:** Anyone with API/log access obtains every webhook HMAC secret and can forge signed deliveries to downstream consumers.

**Design nuance:** `GET /config/yaml` is the round-trip source for `PUT /config/yaml` (full replace; `applyHooks` overwrites every hook from the document). Naive omission would wipe all hook secrets on the next editor round-trip.

**Fix:** Make `Secret` write-only with round-trip preservation: add `hook.Redacted()` masking the secret in List/Get/Create/Update echoes and in `GetConfigYAML`/PUT-response. In write paths, when an incoming secret equals the mask sentinel, carry the stored secret forward. Serve the API over TLS (S-1).

---

#### S-10 (MEDIUM) — Push destination URLs (stream keys/credentials) exposed via logs, /metrics, and RuntimeStatus
**Files:** logs `internal/publisher/push_rtmp.go:166,174,217,254,385,407,415,425,433,438,462` (Info/Warn `"url"` on every lifecycle event, fires on every retry); metrics `internal/metrics/metrics.go:290/371/409` (`dest_url` label) ← `internal/publisher/service.go:678/688`, `internal/publisher/runtime.go:112/126`; API `internal/publisher/runtime.go:214-244` (`PushSnapshot.URL` unredacted) → `internal/api/handler/stream.go:132-133`; event payload `runtime.go:154`.
**Merges:** "push URL logged + RuntimeStatus" (low) and "push URL on /metrics" (medium, two reports).

**Trigger:** Configure `rtmp://host/app/<SECRET-KEY>` (or `rtmps://user:pass@host`) — the stream key is conventionally the final path segment (`internal/domain/push.go:20-21`). The full URL is logged at Info on every connect/retry, used verbatim as the Prometheus `dest_url` label (series created the moment the push worker starts, before any connection), and returned through `RuntimeStatus`/`GET /streams`.

**Impact:** Disclosure of outbound ingest keys / embedded credentials → downstream CDN/ingest hijack. `/metrics` is the worst propagation channel (long-retention TSDB, Grafana label dropdowns, broader audience). Note `PublisherPushBytes` series is **never deleted** (`removePushState` `runtime.go:206-207` deletes only state+reconnect series), so the secret persists for the process lifetime.

**Fix:** Add `redactPushURL(raw)` (strip userinfo, replace final path segment with `***`, mask query) and apply at all slog `"url"` sites, the three metric label sites **and** the matching `DeleteLabelValues` (add one for `PublisherPushBytes`), the event payload, and `PushSnapshot.URL`. Keep the raw URL only in the internal `pushStates` map key and config so diffing/editing is unchanged.

---

#### S-11 (MEDIUM) — Secrets persisted in world-readable store files
**Files:** `internal/store/json/store.go:52` (`MkdirAll 0o755`), `:118` (`WriteFile 0o644`) + `:121` rename; identical `internal/store/yaml/store.go:53/119`. No `os.Chmod`/`0o600` anywhere in `internal/store/`. Secrets serialized: `Hook.Secret`, `Stream.StreamKey`, `Input.Headers`/`Input.Params` (Authorization headers / SRT passphrases / S3 keys).

**Trigger:** Any local user/process that can traverse to the data dir reads `<dataDir>/open_streamer.json` (mode 0644, umask-masked). Default driver `json`, default dir wired in `cmd/server/main.go:153/165`.

**Impact:** Disclosure of all push keys, webhook signing secrets, and embedded source credentials to any local account → ingest hijack / forged webhooks. Downgraded from High by verification: strictly local, single-tenant host, mitigated coincidentally by a hardened umask — but a real CWE-276 hardening defect aggregating every secret in one group/other-readable file.

**Fix:** `MkdirAll(dir, 0o700)` and `WriteFile(tmp, data, 0o600)` in both backends; best-effort `os.Chmod` in `New()` for existing installs (MkdirAll is a no-op on existing dirs). Document plaintext-at-rest in `docs/CONFIG.md`.

---

#### S-12 (LOW) — HLS timeshift master-playlist injection via unvalidated `from/dur/delay/ago`
**Files:** `internal/api/handler/blob_timeshift.go:106-108` (master branch renders **before** the numeric validation at `:110-119`), `:238-246` (`timeshiftParams` rebuilds `k+"="+v` raw); `internal/dvr/blob/hls.go:20-21/43` (`Fprintf` into the manifest).
**Merges:** two reported timeshift-injection findings.

**Trigger:** `GET /<code>/index.m3u8?from=0%0A%23EXT-X-STREAM-INF...%0Ahttp://attacker/evil.m3u8` (no `profile` → master branch) on a stream with a blob DVR catalog. Go decodes `%0A`→newline; the raw value is interpolated verbatim into `EXT-X-MEDIA URI="..."` and the child playlist line; a `%22` breaks out of the `URI="..."` attribute.

**Impact:** Reflected **manifest-body** injection (not HTTP header CRLF — headers are committed first and Go rejects CR/LF in header values; not browser XSS — `Content-Type: application/vnd.apple.mpegurl`). A victim opening a crafted link in an HLS player can be redirected to attacker-controlled variants/media served from the trusted origin. Precondition: the target `<code>` must have an existing blob archive. Downgraded to Low.

**Fix:** Validate `from/dur/delay/ago` numerically on the master branch too (reuse `parseTimeshiftStart`/`parseTimeshiftDuration`) before rendering, and percent-encode in `timeshiftParams` (`url.Values.Encode()` / `url.QueryEscape`) for defense-in-depth.

---

#### S-13 (LOW) — Playback "tokens" recorded but never enforced; SRT `?token=` rejected at connect
**Files:** `internal/publisher/sessions_helper.go:98-114` (`openSRTSession`, token for tagging only), `:157-171` (MPEGTS); `internal/publisher/serve_srt.go:232-243` (`srtStreamCode` never cuts at `?`), `:88-101` (`srtHandleConnect` rejects). `internal/sessions/tracker.go:326-378` uses the token only for `named_by="token"`.

**Trigger:** Operators relying on `?token=` for authorization get none — token-less clients play freely (no validation on any path), and an SRT client with streamid `live/foo?token=x` parses to code `foo?token=x`, misses `mediaBufferFor`, and is **rejected**, while plain `live/foo` is accepted.

**Impact:** Zero access control + a functional SRT bug (tokened SRT streamids unusable, dead tagging path). Verification clarified this is a *documented* known gap (`docs/FEATURES_CHECKLIST.md:394` marks token auth "Not started"; `docs/USER_GUIDE.md:809-811` documents `?token=` as NAT disambiguation only), so the "false sense of security" framing is overstated — the residual is the SRT parsing bug.

**Fix:** In `srtStreamCode`, strip the query before validation (`if i := strings.IndexByte(streamid, '?'); i >= 0 { streamid = streamid[:i] }`); add table tests. Soften the misleading "auth token" comments. Real playback authz remains the tracked backlog item.

---

#### S-14 (LOW) — Subprocess gRPC unix socket is unauthenticated
**Files:** `cmd/open-streamer-transcoder/main.go:64-94` (`lc.Listen("unix", ...)` no chmod, plain `grpc.NewServer()` no peer check); `internal/transcoder/supervisor.go:534-539` (`pickSocketPath` under `os.TempDir()`).
**Verdict:** partially-confirmed.

**Trigger:** A local process (same or permissive umask) that connects to the socket can open a `Run` stream and drive an independent `StreamPipeline` (CPU/GPU/VRAM abuse). It cannot inject into the real stream's output buffers (those go via the supervisor's own client).

**Corrections:** The claimed "crypto/rand failure → zero nonce" is a **false sub-claim** — `crypto/rand.Read` cannot return an error on Go ≥1.24 (process crashes instead), and the nonce is only inode-collision avoidance, not a security control. Cross-user reachability is umask-dependent (default 0755 + connect-needs-write blocks other UIDs).

**Fix:** Create the socket inside a `0700` per-process dir via `os.MkdirTemp` and return `filepath.Join(dir, "t.sock")`; remove the dir on teardown. This enforces same-UID-only without SO_PEERCRED plumbing. Don't bother "fixing" the rand error path for security.

---

#### S-15 (LOW) — Client IP / session fingerprint spoofable via X-Forwarded-For / X-Real-IP
**Files:** `internal/sessions/tracker.go:160-177` (`clientIP` reads XFF/X-Real-IP directly) → `fingerprintID` `:295`, GeoIP `:346`, event payload `:622/646`; `internal/api/server.go:163` (`middleware.RealIP` mutates `r.RemoteAddr`).

**Trigger:** Send `X-Forwarded-For: <arbitrary>` on any HLS/DASH GET. The IP feeds the session fingerprint, GeoIP country, and attribution.

**Impact:** Session-map inflation (bounded by ~30s idle reap), session-id impersonation, mis-attribution. **No IP-based access control / geo-block exists** (searched), so harm is metric-integrity only. Connection-bound protocols (RTMP/SRT/RTSP) use the real socket and are not spoofable here.

**Fix:** Honor forwarded headers only when `r.RemoteAddr` is in a configured trusted-proxy CIDR; otherwise use the socket address. Replace `middleware.RealIP` with a non-mutating extractor (shared with S-1).

---

#### S-16 (LOW) — pprof listener: unauthenticated heap/goroutine dumps + always-on block/mutex profiling
**Files:** `internal/api/server.go:393-436` (`startPprofListener`), `:414-415` (`SetBlockProfileRate(10ms)`/`SetMutexProfileFraction(100)` unconditional, never reset), `:398-407` (mux, no auth), `:418` (addr used verbatim, no loopback enforcement). `config/config.go:39` — no default, opt-in.

**Trigger:** Operator sets `pprof_addr`. Serves `/debug/pprof/*` (heap/goroutine/cmdline/profile/trace) unauthenticated and forces process-wide contention profiling on. A config reload clearing `pprof_addr` stops the listener but leaves the profilers enabled (slightly worse than reported).

**Impact:** Memory/goroutine disclosure if bound to a reachable interface; small steady contention overhead. Default-disabled, opt-in — Low.

**Fix:** Validate the bind host is loopback (refuse non-loopback unless an explicit `pprof_allow_remote` flag); gate `SetBlockProfileRate`/`SetMutexProfileFraction` behind a separate flag and reset to 0 on shutdown.

---

### 2.2 Business Bugs

> **Shared root cause (B-1..B-4):** the gRPC `InputPacket` (`internal/transcoder/native/proto/transcoder.proto:126-130`) carries **no codec and no PTS/DTS**, and the supervisor's `forwardOnePacket` (`supervisor.go:294-318`) forwards `pkt.AV.Data` for AV-path sources with no codec discrimination. The subprocess Annex-B branch (`stream_pipeline.go:670-686`) assumes every packet is an H.264 video AU. This single design gap produces B-1, B-2, and B-4 (and the TS-path HEVC crash B-AV-7 is the codec-hardcoding sibling). Fixing the proto (add `codec`, `pts_ms`, `dts_ms`) addresses all of them.

---

#### B-1 (HIGH) — Transcoding an AV-path source drops all audio and feeds AAC into the H.264 decoder
> ✅ **FIXED** in `fix/av-path-transcode` — `InputPacket` now carries `codec`/`pts_ms`/`dts_ms`; the supervisor forwards them and drops non-AAC audio, and the AV-path `ProcessPacket` routes AAC to the audio path instead of the H.264 decoder. Regression test `TestProcessPacket_AVPathAudioNotFedToDecoder`.

**Files:** `internal/transcoder/native/stream_pipeline.go:670-686` (Annex-B branch, no audio handling); `internal/ingestor/pull/rtsp.go:431-437` (RTSP always emits AAC); `internal/transcoder/supervisor.go:300-303` (forwards all packets); `coordinator.go:1067-1073` (`shouldRunTranscoder` no input-type restriction).

**Trigger:** Transcode a stream whose active input is RTSP or RTMP (any AV-path source reaching the raw-ingest buffer as `domain.AVPacket`). The subprocess probes the first packet as non-TS, so `tsInput` stays nil and ProcessPacket takes the Annex-B branch.

**Impact:** `handleAudio`/`passthroughAudio`/`audioReenc` are reachable only from the TS branch, so all AAC AUs (`0xFF 0xF1…`) are fed to `dec.Decode()` (the H.264 decoder) → **silent transcoded output**; on stricter backends a decode error → terminal error → respawn loop. `mixer://` goes through the separate ABR path so the title's mixer inclusion is unestablished — affected sources are RTSP pull, RTMP pull, RTMP push.

**Fix:** Stopgap: in `forwardOnePacket`, skip AV packets whose codec is audio (`!pkt.AV.Codec.IsVideo()`) and log once that AV-path transcode is video-only. Real fix: add `codec`/`pts_ms`/`dts_ms` to `InputPacket`, regenerate via protoc (never hand-edit the pb.go), populate from `pkt.AV`, and route audio-codec packets through `handleAudio` in the non-TS branch.

---

#### B-2 (HIGH) — AV-path transcode output PTS collapses to 1 ms/frame (~1000 fps)
> ✅ **FIXED** in `fix/av-path-transcode` — the supervisor forwards `pkt.AV.PTSms/DTSms` and `server.dispatch` passes them to `ProcessPacket`/`dec.Decode`, so `encodeOne` sees the real source PTS. Defense-in-depth: the `srcPTS<=0` fallback now advances by `videoFrameDurMs` (nominal frame duration), not a 1 ms frame counter.

**Files:** `internal/transcoder/native/server.go:108` (`ProcessPacket(pkt.GetData(), 0, 0)`); `decoder.go:178-179` (stamps pts=dts=0); `stream_pipeline.go:1165-1167` (srcPTS≤0 fallback → `NextPTS`); `encoder.go:333-337` (bare frame counter), `:131` (fixed 1/1000 timebase).

**Trigger:** Transcode any Annex-B AV-path source. The InputPacket has no PTS field, so the normalised `pkt.AV.PTSms` is dropped; the decoder sees pts=0 for every packet, so `f.Pts()==0` and `encodeOne` falls back to a 0,1,2,… frame counter at 1 ms spacing.

**Impact:** Output frames declare a ~1000 fps timeline → downstream segment-duration math packs ~1000 frames per "1 s" → media runs ~40× **slower** than wallclock at 25 fps (the original "live edge races" direction is inverted, but the timeline-compression substance is the same documented failure class). Raw-TS sources are unaffected (`tsInput` recovers in-band PES timing). Tests mask it (they pass real PTS; production hardcodes 0).

**Fix:** Same proto change as B-1 (add `pts_ms`/`dts_ms`), forward `pkt.AV.PTSms/DTSms`, pass them at `server.go:108`. Defense-in-depth: when the `srcPTS≤0` fallback fires, advance by frame-duration not `+1`. Add a regression test driving ProcessPacket with pts=0 and asserting output spacing equals frame duration.

---

#### B-3 (MEDIUM) — HEVC Annex-B AV source never trips the keyframe gate → silent zero output, reported healthy
**Files:** `internal/transcoder/native/stream_pipeline.go:670-675` (gate on `sawKeyframe`), `:758-784` (`isH264KeyframeAnnexB`, `data[naluStart]&0x1F == 5` — H.264 mask).

**Trigger:** Transcode an H.265 Annex-B AV-path source (HEVC RTSP/RTMP). HEVC base-layer NAL first bytes are always even (IDR_W_RADL 0x26, etc.), so `&0x1F==5` (odd) never matches; the gate never opens; every packet is dropped with nil error.

**Impact:** Stream reports running/healthy with zero rendition output forever (`ProcessPacket` returns nil error → no terminal error → no respawn; `Service` health flips only on error transitions; no output-liveness watchdog). Downgraded to Medium: HEVC transcode is outside the supported envelope (`decoderCodecForBackend` hardcodes h264) and triggering requires an operator to enable transcode on an HEVC AV source.

**Fix:** Make the unsupported input *visible*: after N consecutive gated drops with no H.264 IDR, return a terminal error ("non-H.264 AV input not supported") so the supervisor marks unhealthy; or reject at configure time using the ingest `AVPacket.Codec`. Full HEVC support requires a codec-aware keyframe detector + a decoder-codec proto field + `hevc/hevc_cuvid` selection.

> ✅ **FIXED** by `fix/decoder-codec-aware` (A-3) — rather than just making it visible, HEVC AV-path transcode now WORKS. The AV-path gate is `isVideoKeyframeAnnexB(codec, data)`, which routes HEVC to `gocodec.IsH265IDRFrame` (the H.264 mask no longer blocks HEVC IRAPs), and `ensureVideoDecoder` rebuilds the decoder to `hevc`/`hevc_cuvid` on the first H.265 frame. Codec is detected at runtime from the esFrame, so the proposed decoder-codec proto field was unnecessary. Covered by A-3's `TestIsVideoKeyframeAnnexB` (HEVC IRAP detection + dispatch divergence).

---

#### B-4 (MEDIUM) — DASH ABR: shards anchor their own availabilityStartTime; root MPD publishes one AST
**Files:** `internal/publisher/dash/abr.go:137/177-179` (`combineSnapshots` takes first non-zero AST); `internal/publisher/dash/packager.go:702-708` (per-shard `availStart` at first flush); `internal/publisher/dash/state.go:95-107` (video-only shards wait the full 3 s pairing deadline).
**Verdict:** partially-confirmed, downgraded High→Medium.

**Trigger:** Any ABR DASH stream with ≥2 renditions. The audio-packing shard flushes at ~segDur (default 2 s); non-audio shards never have `audioReady` true and only flush after the 3 s pairing deadline, so per-shard AST skew Δ≈1 s.

**Impact (corrected):** Segment timelines are **media-PTS-anchored from the first IDR**, not wallclock, so the same content gets the same `t` in every rep — the original "rendition switch jumps content by the skew" and "audio offset against video" claims are **false**. Real defects: (a) the single published AST is Δ-wrong for all but the slug-sorted winner; (b) `behindPrevSegEnd` paces each shard against its own later AST so non-audio reps publish every segment Δ later forever (AST-window availability over-promise, edge-switch stall risk); (c) if the audio-packing best rendition isn't `track_1`, the published AST **changes mid-stream**, violating dynamic-MPD AST constancy. On default config Δ≈1 s is absorbed by `suggestedPresentationDelay=6 s`.

**Fix:** Remove the pointless pairing wait for `!PackAudio` shards (`audioReady := videoReady` so all shards cut at the same source IDR — collapses skew to ≤~150 ms). Share one ladder-wide AST via `ABRMaster` set once on first flush. Do **not** rebase per-shard `StartTicks` (timelines are already media-aligned).

---

#### B-5 (HIGH) — Streams that inherit Inputs from a template never auto-start
> ✅ **FIXED** in `fix/template-resolution` — `BootstrapPersistedStreams`, `reconcileOnce`, and the `Put` handler now resolve the template BEFORE the input-eligibility gate (`freshlyCreated` is computed from the resolved inputs). Regression test `TestReconcileOnceStartsTemplateInheritedInputs`.

**Files:** `internal/coordinator/coordinator.go:893-896` (`BootstrapPersistedStreams` skips `len(st.Inputs)==0` **before** `resolveTemplate` at `:897`), `:961-968` (`reconcileOnce` same raw check before resolution); `internal/api/handler/stream.go:400` (`freshlyCreated` tests `len(body.Inputs)>0` on the raw body).
**Merges:** two reported findings (identical root cause).

**Trigger:** Create template `T` with `inputs:[...]`; create `{"code":"s1","template":"T"}` (no own inputs — the documented zero-value=inherit pattern). `POST /streams/s1` returns 201 but never starts. After a restart, bootstrap and every `reconcileOnce` skip it because the input-eligibility check runs on the raw on-disk record before `domain.ResolveStream` fills `Inputs` from the template.

**Impact:** The core template use case (many streams sharing one input/profile) silently produces permanently-stopped streams; the reconciler safety net is defeated by the same raw gate. Only manual `POST /streams/{code}/restart` or a disabled→enabled toggle starts them — and they die again on the next reboot. Bootstrap logs only at Debug.

**Fix:** Resolve the template **before** each eligibility gate (resolveTemplate is a no-op for `Template==nil`, so genuinely input-less streams stay skipped): move resolution above the inputs check in `BootstrapPersistedStreams` and `reconcileOnce`; compute `freshlyCreated` from `resolvedBody` in `Put`. Add a reconciler test for `{code, template}` with template-only inputs.

---

#### B-6 (HIGH) — copy:// / mixer:// upstream-shape lookups never resolve templates → silent blackout + ABR-validation bypass
> ✅ **FIXED** in `fix/template-resolution` — `wireCopyLookup`, the coordinator `upstreamLookup`, and the copy/mixer validation wrappers now resolve the template so copy:// / mixer:// classify upstreams by their inherited Inputs/Transcoder. Regression test `TestValidateCopyConfig_ResolvesTemplateUpstream`.

**Files:** `cmd/server/main.go:295-305` (`wireCopyLookup` = plain `repo.FindByCode`); `internal/coordinator/coordinator.go:106-112` (`upstreamLookup` raw lookup; resolving helper `resolveTemplate` exists at `:136-145` but unused here); `internal/api/handler/stream.go:531-545` (`ValidateCopyShape` lookup); consumers `internal/ingestor/pull/copy.go:86-106/153-157`, `mixer.go:112-156`, `internal/domain/copy_shape.go:181-192`.

**Trigger:** Upstream `A = {code:"a", template:"T"}` where `T` supplies the Transcoder (ABR) and/or raw-TS Inputs. Downstream `B` has input `copy://a` (or `mixer://a,x`). The lookup returns A's raw record (Transcoder=nil, Inputs=[]), so `streamHasRenditions`/`StreamMainBufferIsTS` misclassify A as single-stream direct-AV. But A's main buffer carries TS chunks (or, for ABR, nothing the copy subscribes to), so every packet hits the `pkt.AV==nil → return nil,nil` drop (`copy.go:153-157`).

**Impact:** Downstream copy/mixer of any template-based upstream is **black/silent with no error** (reader runs "healthy"); the coordinator mis-routes ABR mirror paths; and the "ABR-copy must be sole input" rule is silently skipped at the API. Not perfectly silent — the 15 s stall watchdog eventually marks the input Degraded, but reconnects rebuild the identical misclassified reader, so the stream stays black indefinitely with nothing pointing at the cause.

**Fix:** Make all three lookups template-aware: `wireCopyLookup` and `upstreamLookup` return `domain.ResolveStream(s, tpl)`; in `validateCopyConfig`/`validateMixerConfig` resolve each listed stream and the body before shape checks. Add a regression test (template ABR/raw-TS upstream → TS-demux branch / mirror path / `INVALID_COPY_SHAPE`).

---

#### B-7 (HIGH) — `domain.inputSourceIsRawTS` misclassifies HTTP-TS (`.ts` / `/mpegts`) and `.m3u` upstreams
> ✅ **FIXED** in `fix/template-resolution` — `inputSourceIsRawTS` now delegates to `protocol.Detect` (handles `.ts`, `/mpegts`, `.m3u`, uppercase); `looksLikeHLS` deleted. Table test `TestInputSourceIsRawTS`.

**Files:** `internal/domain/copy_shape.go:200-216` (`inputSourceIsRawTS`), `:220-227` (`looksLikeHLS`, case-sensitive `.m3u8` substring scan) vs `pkg/protocol/protocol.go:66-78` (lowercases, classifies `.ts`/`/mpegts` as KindHTTPTS, `.m3u` as HLS).

**Trigger:** Upstream `A` input `http://relay/chan/mpegts` or `http://cdn/live.ts` → `protocol.Detect`=KindHTTPTS → A's main buffer holds raw TS chunks. `B = copy://a`: `StreamMainBufferIsTS(A)` calls `inputSourceIsRawTS`, which for http(s) returns true only when `.m3u8` appears — `.ts`, `/mpegts`, `.m3u`, and uppercase variants return false. CopyReader takes direct-AV mode and drops every TS-only packet.

**Impact:** copy:// / mixer:// of the server's own documented HTTP-TS instance-to-instance relay yields a **permanently silent downstream with no error**. The `inputSourceIsRawTS` "inlined to avoid importing pkg/protocol" rationale is dead — `copy_shape.go` already imports `pkg/protocol` (`:7`) and `IsCopyInput` calls `protocol.Detect`.

**Fix (drift-proof):** Replace the duplicated classifier with delegation — `switch protocol.Detect(rawURL) { case KindUDP, KindHLS, KindHTTPTS, KindSRT, KindFile: return true }` — and delete `looksLikeHLS` (this is exactly the set `reader.go:103-126` wires to `TSPassthroughPacketReader`). Add table tests for `/mpegts`, `.ts`, `.m3u`, uppercase, and negative cases.

---

### 2.3 Data Loss

---

#### D-1 (HIGH) — DVR catalog wiped on every recording restart → orphaned blobs, retention defeated, unbounded disk growth
> ✅ **FIXED** in `fix/dvr-data-loss` — `StartRecording` now `LoadCatalog`s the prior catalog and `mergePriorCoverage` carries its `Hours`/`Available`/`Gaps` (and profiles only in the old catalog) into the fresh one, so `pruneOnce` sees and prunes pre-restart hours (retention is wall/size-anchored → correct regardless of origin). The new run anchors its own media origin so recent timeshift keeps working; playing back **across** a restart boundary needs a per-hour reader anchor (noted follow-up). Test `TestMergePriorCoverage`.

**Files:** `internal/dvr/blob/service.go:96/100` (`newCatalog` + `Save`, `:251-274`); `LoadCatalog` only at `reader.go:48`; retention `retention.go:48-93`; `recovery.go:50-70` (`RepairStream` only seals `.open`-sentinel crash-dirty hours); trigger `coordinator.go:360/745-748`.
**Merges:** two reported findings (identical root cause).

**Trigger:** Any restart of a recording — server restart (`BootstrapPersistedStreams`), stream `Update`/`Restart`, or template hot-reload (`reloadDVR`). `StopRecording` saves the full catalog; the immediate `StartRecording` builds a fresh **empty** catalog and `Save`s it over `catalog.json`. No `LoadCatalog`/merge exists on the write path.

**Impact:** All pre-restart sealed `.cmfv/.cmfa/.ranges` files remain on disk but vanish from the catalog. Retention iterates only `cat.Profiles[].Hours`, so orphans are never age-pruned, never counted toward `MaxSizeGB` → **unbounded disk growth across restarts despite configured retention**. Pre-restart timeshift history becomes unreachable (reader loads the empty catalog). Commit `3886163` ("keep recording across restart") fixed only the O_EXCL current-hour resume — earlier sealed hours are still orphaned.

**Fix:** In `StartRecording`, after `RepairStream`, `LoadCatalog(streamDir)`; if a matching-`Format` catalog exists, seed the new in-memory catalog from it (keep `Profiles[].Hours`/`Available`/`Gaps`, carry `RecordingMediaOrigin*` with `originSet=true`), refresh per-profile metadata + retention from the new cfg, then Save. Caveat: `setOrigin` zeroes the per-run tick origin — preserve the loaded origin across runs or anchor `Reader.Query` per-hour via `HourRecord.WallFromMs/WallToMs`. Defense-in-depth: make `pruneOnce` reconcile against on-disk hour dirs so legacy orphans are reclaimable.

---

#### D-2 (HIGH) — Raw-TS / passthrough streams record ZERO bytes while `recording_status` reports "recording"
> ✅ **FIXED (fail-loud)** in `fix/dvr-data-loss` — `blobProfiles` now refuses a DVR lane for a non-transcoded raw-TS source (`StreamMainBufferIsTS`), so no catalog or Recording row is created and the status no longer lies. Making raw-TS DVR actually record (wire `pkt.TS` demux → AVPacket → `ingestAV`) is a deliberate follow-up. Test `TestBlobProfiles_RefusesRawTSSource`.

**Files:** `internal/dvr/blob/writer.go:102-120` (`Ingest` handles only `pkt.AV`; `pkt.TS` is an explicit unimplemented stub at `:117-118`); `service.go:100-113` (catalog + Recording row saved unconditionally); `coordinator.go:780-784` (`blobProfiles` single `p0` lane on `stream.Code`); status `blob_timeshift.go:45-48`.

**Trigger:** Enable DVR on any non-transcoded stream whose ingest writes raw MPEG-TS into the buffer hub: UDP multicast, HLS-pull, HTTP-TS, SRT, or file. `profileWriter.Ingest` discards `pkt.TS` silently (returns nil), so no blob/fragment is ever written.

**Impact:** The archive is empty, yet `StartRecording` persists a Recording row and catalog, so `RecordingStatusJSON` reports `status="recording"` with empty `dvr_range` indefinitely; every timeshift query 404s. Audio-only sources are also dead (origin set only on video, audio-only cuts dropped at `:216-219`). **Correction:** `copy://` and `mixer://` are *not* affected — `CopyReader` wraps the upstream in `NewTSDemuxPacketReader` which emits real AVPackets (the AV path); affected classes are non-transcoded `udp://`, HLS-pull, `http-ts`, `srt://`, `file://`. UDP multicast is a primary production source, so High stands.

**Fix:** Minimal (fail loudly): in `startDVR`/`blobProfiles`, when `len(rends)==0 && domain.StreamMainBufferIsTS(stream)`, refuse to start recording, log a warning, and surface "unsupported" — so no catalog/Recording row is created and status never lies. Proper: wire `pkt.TS` ingestion by demuxing chunks (as `pull.TSDemuxPacketReader.runDemux`/`buildAVPacket` already does) into AVPackets fed to `ingestAV`, plus a `writer_test.go` regression.

---

### 2.4 Availability

---

#### A-1 (HIGH) — Unauthenticated playback endpoints with no connection cap → resource-exhaustion DoS
**Files:** `internal/publisher/serve_rtsp.go:126-163` (`WriteQueueSize=4096`, ~6 MB/stalled session per the code's own comment); `serve_rtmp.go:44-79/97-232` (per-client 16 MiB `tsBuffer` `tsbuffer.go:33` + producer goroutine + tsdemux goroutine + full remux); `serve_srt.go:88-101`; `serve_mpegts.go:69-120`; `internal/buffer/service.go:82` (`subscribe` appends with no count limit). No `MaxSessions`/semaphore anywhere (grep confirmed).

**Trigger:** An unauthenticated client that knows/guesses one active stream code opens many concurrent connections (RTSP DESCRIBE/SETUP/PLAY, RTMP play, SRT subscribe, `GET /<code>/mpegts`). None of the handlers authenticate; the `token` param is attribution-only.

**Impact:** Each accepted connection allocates attacker-multiplied heavyweight per-session state with no cap — N connections = N demux pipelines / N×6 MB write queues / 2-3N goroutines / N FDs → goroutine/FD/memory exhaustion → OOM, taking down all streams. The 16 MiB/6 MB figures are slow-reader worst-cases (a deliberate slow-reader reaches them); the 2-3 goroutines + 1 FD per connection is unconditional.

**Fix:** Add per-stream and global concurrent-playback caps checked *before* allocating any pipeline: atomic counters on the publisher Service, increment-and-test at the top of `srtHandleConnect` (return REJECT), `HandleRTMPPlay` (error), RTSP `OnSetup`/`OnPlay` (503), `HandleMPEGTS` (503); decrement on close. For RTMP specifically, consider one shared demux per stream fanned out instead of a full 16 MiB pipeline per connection. Combine with the auth from S-1.

> ✅ **FIXED** in `fix/playback-conn-cap` — new `connLimiter` (`conn_limiter.go`) on the publisher `Service` caps concurrent playback connections per-stream AND globally; `acquire` is called BEFORE any per-connection state is allocated and `release` on close. Wiring: `HandleMPEGTS` (acquire→503, `defer release`), `HandleRTMPPlay` (acquire→error, `defer release`), `srtHandleSubscribe` (acquire→close+return, `defer release` — moved to the subscribe path so the slot is leak-proof via defer), and RTSP `OnPlay` (acquire→503; release in `OnSessionClose` via the `rtspSessions` entry, with a repeated-PLAY guard so PAUSE→PLAY can't double-count and a nil-`ps` cap-only entry handled by the touch loop / `pollBytes`). Caps are configurable (`PublisherConfig.MaxPlaybackConnPerStream` / `MaxPlaybackConnTotal`): unset → protective default (256 / 4096), negative → unlimited. HLS/DASH viewers go over stateless HTTP and aren't counted. The acquire/release balance across all 4 handlers was adversarially audited (incl. against gortsplib's serialized-per-session / guaranteed-`OnSessionClose` lifecycle) — no leak or double-release. Tests: `TestConnLimiter_*` (per-stream/global cap, isolation, unlimited, no-underflow, concurrent `-race` balance), `TestResolvePlaybackCap`, `TestHandleMPEGTS_PlaybackCapRejectsAndReleases` (503 over cap + slot frees after close). (Auth, the other half, remains S-1.)

---

#### A-2 (HIGH) — Unbounded DVR timeshift window loads the entire archive into memory per profile
**Files:** `internal/dvr/blob/reader.go:73-119` (`endMs=1<<62-1` when `dur≤0`; reads every hour's `.ranges`, builds a `FragmentRef` per fragment); `blob_timeshift.go:154-161` (`ServeMPD` loops every profile); `parseTimeshiftDuration timeshift_params.go:43-53` returns 0 when `dur` absent.

**Trigger:** Unauthenticated `GET /<code>/index.mpd?from=0` (or `?ago=<huge>`) with no `dur` on a stream with a blob archive. The hour filter admits every hour; `Query` holds no ctx so the 120 s `Timeout` cannot abort it. **Correction:** `?ago=0` does *not* trigger it (yields `fromMs=now`, cheap); the real triggers are `?from=0` and `?ago=<huge>`/`?delay=<huge>`. Magnitude is unbounded only under the default keep-forever retention (`retention.go:20-22`).

**Impact:** O(total fragments × profiles) allocation + reads of every `.ranges` file — hundreds of MB per request for a multi-day/multi-profile archive; a few concurrent requests OOM the server.

**Fix:** Clamp the resolved end to `fromMs + maxWindowMs` (configurable max timeshift depth) when `dur` is absent or oversized; clamp `from` to the catalog's earliest hour; thread `r.Context()` into `Query` and check `ctx.Err()` in the hour loop. Shared `candidateHours` overflow/bound issues (see L-list) belong to the same reader.

> ✅ **FIXED** in `fix/dvr-timeshift-bound` — `Reader.Query` now resolves its window via `resolveWindowBounds`, which clamps the depth to `MaxTimeshiftWindow` (package var, default 24h) when `dur` is absent/≤0/oversized and pulls `from` UP to the archive's earliest hour (`earliestHourMs`), so a `?from=0` / `?ago=<huge>` request can no longer anchor at the epoch and admit every hour. The `1<<62` open-ended sentinel is gone — `endMs`/`endTicks` are always finite. `Query` now takes a `context.Context` and checks `ctx.Err()` at the top of the hour loop, so the server's 120 s timeout (and client disconnect) aborts the scan instead of reading every `.ranges` file. Both handler call sites (`ServeTimeshift`, `ServeMPD`'s per-profile loop) pass `r.Context()`. Tests `TestResolveWindowBounds` (clamp table) + `TestBlobReader_QueryClampsAndCancels` (from-clamp returns data + cancelled ctx aborts).

---

#### A-3 (HIGH) — HEVC MPEG-TS source feeds the H.265 stream into an H.264-only decoder → permanent respawn loop
**Files:** `internal/transcoder/native/ts_input.go:214-217` (`StreamTypeH265` → `esCodecH265`, routed onto the shared `t.frames` video queue); `internal/transcoder/native/server.go:360-365` (`decoderCodecForBackend` unconditionally returns `h264`/`h264_cuvid`, with an in-code admission to wire HEVC later); `decoder.go:161-188`; supervisor respawn `supervisor.go:102-139/439-450`.

**Trigger:** Transcode a raw-TS source (UDP/HLS-pull/SRT/file) whose video is HEVC. The demuxer tags frames `esCodecH265` and feeds them to `dec.Decode`, but the decoder is H.264 → `avcodec_send_packet` returns `AVERROR_INVALIDDATA` → terminal error → respawn → same first frame → endless crash loop, `restart_count` climbing.

**Impact:** Per-stream permanent loss of transcoded output (subprocess isolation holds, so blast radius is one stream — High not Critical). Can fire latently when an upstream mux switches H.264→HEVC on an established stream. On NVENC hosts the cuvid parser may instead silently produce no video (audio-only/black) — either mode is permanent.

**Fix:** Make decoder selection codec-aware: on the first video esFrame whose `codec` ≠ the active decoder's codec, rebuild the decoder via the existing `SwitchInput` swap machinery (`stream_pipeline.go:873-880`) using `esCodecH265 → hevc_cuvid/hevc`; `newDecoderWithFallback` already degrades cuvid→CPU. If HEVC is out of scope, return a single descriptive terminal error instead of an opaque respawn loop.

> ✅ **FIXED** in `fix/decoder-codec-aware` — both video decode paths now call `StreamPipeline.ensureVideoDecoder(codec)` before `dec.Decode`. When the incoming codec's family (`decoderCodecFamily`) differs from the active decoder, it flushes the old decoder through the surviving encoder, rebuilds via `newDecoderWithFallback` with `videoDecoderNameForCodec` (`esCodecH265 → hevc/hevc_cuvid`, preserving the GPU/CPU lane of the current decoder), rebinds the GPU scale graphs to the new CUDA pool, and resets the keyframe gate so decode resumes on the new codec's first IRAP. The encoder is never rebuilt (codec-agnostic) so output stays continuous. Handles HEVC-from-first-frame **and** mid-stream H.264→HEVC. The AV-path keyframe gate is now codec-aware too (`isVideoKeyframeAnnexB` → `IsH265IDRFrame` for HEVC); the raw-TS path already tagged `f.keyframe` per codec at demux. If HEVC decode is genuinely unavailable in the linked libav, `newDecoderWithFallback` surfaces a single descriptive error (`decoder "hevc" not available…`) instead of the opaque `AVERROR_INVALIDDATA` loop. Tests `TestDecoderCodecFamily`, `TestVideoDecoderNameForCodec`, `TestEnsureVideoDecoder_NoRebuildPaths`, `TestIsVideoKeyframeAnnexB`.

---

#### A-4 (HIGH) — tsnorm demux goroutine death permanently disables TS normalisation, silently switches to raw passthrough
> ✅ **FIXED** in `fix/tsnorm-restart` — `runDemux` now rebuilds the astits demuxer on a parse error and resyncs (capped at `maxDemuxRestarts=8`, mirroring `pull.TSDemuxPacketReader`) instead of dying; and `Process` resets `n.started` when it observes `demuxDone`, so a goroutine that does give up is relaunched lazily on the next call. A transient corruption now costs at most one passthrough chunk, not permanent un-normalised output. Test `TestProcess_RecoversFromParseError`.

**Files:** `internal/ingestor/tsnorm/tsnorm.go:391-417` (`runDemux` exits on any non-EOF `NextData` error; `n.started` never reset), `:262-290` (`Process` returns io.EOF forever); `internal/ingestor/worker.go:553-574` (`writeRawTSChunk` falls back to raw passthrough per chunk).

**Trigger:** Any astits parse error on a raw-TS source — e.g. one truncated UDP datagram (UDP default read buffer is exactly 1316 bytes / 188×7; larger datagrams truncate → permanent misalignment) or a corrupted adaptation field on a noisy multicast feed. `runDemux` logs at Debug and returns; every later `Process()` returns io.EOF; `writeRawTSChunk` then writes the **raw upstream chunk** for every chunk, forever.

**Impact:** The buffer hub receives original PIDs and un-normalised PTS instead of the remuxed wallclock-anchored timeline — violating the "all raw-TS goes through the Normaliser" invariant, breaking DASH AST/tfdt math and HLS timing with a mid-stream PID/timestamp-domain change. **No self-heal:** raw bytes keep flowing so the stall watchdog and manager packet-timeout never fire (`stall_watchdog.go:88` calls `buf.SetSession`, never `tsNorm.OnSession`). Not fully silent — a per-chunk Warn at `worker.go:555` floods, but nothing surfaces in stream health/status. The sibling `tsdemux_packet_reader.go:233-279` tolerates this exact error class with `maxDemuxRestarts=8`.

**Fix:** (a) In `runDemux`, on a non-EOF error rebuild the astits demuxer (drop to the next 188-aligned chunk boundary) and continue, capped by a consecutive-error counter (mirror `maxDemuxRestarts=8`). (b) In `Process`, when `demuxDone` is observed closed, reset `n.started=false`, clear `pidStream`, rebuild the muxer, and lazily restart on the next call. (c) Defense-in-depth: count consecutive failures in `writeRawTSChunk` and after N return the error so `readLoop` reconnects and `mintSessionForOpen` emits a proper boundary. Add a misaligned-chunk regression test.

---

#### A-5 (HIGH) — `manager.Register` swallows initial `ingestor.Start` failure → permanent zombie stream reported Active
> ✅ **FIXED** in `fix/lifecycle-self-heal` — `Register`'s synchronous start-failure branch now routes through `ReportInputError`, so the input degrades, error history is recorded, and failover / exhausted-handling engage (multi-input promotes a backup; single-input flips to Degraded + probe loop). `Register` also refuses a duplicate registration (no monitor-goroutine leak). Tests `TestRegister_StartFailureDrivesExhausted`, `TestRegister_RefusesDuplicate`. (The connects-but-no-packets Idle blind spot is a noted follow-up.)

**Files:** `internal/manager/service.go:415-421` (error only logged, `Register` returns nil), `:816` (`collectTimeoutIfNeeded` needs StatusActive), `:846/849` (`collectProbeIfNeeded` needs StatusDegraded); `coordinator.go:189-199` (StreamStatus = Active when registered + no degradation), `:964-966` (reconciler skips IsRunning).

**Trigger:** `mgr.Register` → `s.ingestor.Start` fails synchronously — `NewPacketReader` error for `KindUnknown` schemes (including extension-less http(s) HLS URLs the API never validates), `file://` with an unresolvable VOD mount, or `copy://`/`mixer://` whose upstream vanished before bootstrap. `Register` returns nil; the coordinator records the stream started, `IsRunning==true`.

**Impact:** Every input stays StatusIdle with zero `LastPacketAt`: the timeout collector never fires (needs Active), no probe is scheduled (needs Degraded), `exhausted` is never set, the reconciler skips the stream. **Stream is permanently dead at the source while StreamStatus reports Active.** Same blind spot: a source that connects but never delivers a packet (e.g. dead UDP multicast — `udp.go:109` Open always succeeds) never triggers failover. **Corrections:** there is no push-registration-conflict error (push degrades correctly via `ErrNoPusherConnected`), and the Unregister/Register race occurs after Start returns, so the hole is limited to synchronous failures + zero-packet sources.

**Fix:** In `Register`'s error branch, route through `ReportInputError(stream.Code, best.Input.Priority, err)` so the input degrades, error history is recorded, and `tryFailover`/`handleExhausted` engage (multi-input promotes a backup; single-input sets exhausted → StreamStatus reports Degraded + enables probing). Close the Idle blind spot: stamp `LastPacketAt=now` on successful Start and relax `collectTimeoutIfNeeded` to also degrade an active Idle input with non-zero `LastPacketAt` after the timeout. Optionally validate scheme in `decodeStreamBody`.

---

#### A-6 (HIGH) — `reloadTranscoderFull` partial failure strands the stream with no publisher/DVR while IsRunning stays true
> ✅ **FIXED** in `fix/lifecycle-self-heal` — any error after teardown in `reloadTranscoderFull` now routes through `reloadFailed`, which does a clean full `Stop` (idempotent against the partial state) so `IsRunning` flips false and the reconciler restarts the stream from the persisted config within one tick — a permanent invisible outage becomes a ≤10 s self-heal. Test `TestUpdate_ReloadFailureFullStops`.

**Files:** `internal/coordinator/coordinator.go:585-652` — teardown `:588-591` (stopDVR/pub.Stop/tc.Stop), early returns at `:638-640` (tc.Start fails → publisher never restarted) and `:646-648` (pub.Start fails → DVR never restarted).

**Trigger:** `PUT /streams/{code}` with a transcoder topology change (nil↔non-nil, video.copy flip, watermark change per `diff.go:123-170`) while `tc.Start` fails — realistic: transcoder binary missing after a bad deploy (`transcoder/service.go:312-316` probes the binary at Start), NVENC/GPU error. The reload has already torn down DVR + publisher + old transcoder; the early return leaves ingest running with zero outputs. Manager registration is never dropped, so `IsRunning==true` and `reconcileOnce` skips it forever. Template hot-reload routes every dependent through the same `Update`, so one broken-binary template edit can strand many streams at once.

**Impact:** All HLS/DASH/RTMP viewers and DVR stay dead until a manual `/restart`; the self-healing loop is structurally blind. **Worse than reported:** no degradation flag is set, so StreamStatus reports **Active** for a zero-output stream. A retried identical `PUT` diffs new-vs-new (empty diff) and repairs nothing (handler saves before Update). **Correction:** the secondary `reloadProfiles` claim is imprecise — native `StopProfile` is a no-op and `StartProfile` returns `ErrNotImplemented`, so its mid-loop return doesn't strand outputs (though its Removed-branch buffer deletion is a separate lesser defect).

**Fix:** On any error after teardown, fall back to a clean full stop: `c.Stop(ctx, new.Code); return fmt.Errorf(...)` — `Stop` is idempotent against the partial state, so `IsRunning` goes false and the 10 s reconcile tick restarts the stream from the persisted config, converting a permanent invisible outage into a ≤10 s self-healing one and making a retried PUT meaningful. Set a degradation flag for observability.

---

#### A-7 (HIGH) — HLS segment body read into memory with no size cap (OOM DoS)
**Files:** `internal/ingestor/pull/hls.go:465-484` (`fetchSegmentOnce` → `io.ReadAll(resp.Body)`, no Content-Length check, no `io.LimitReader`); playlist parse `:504-552` (unbounded `segments` slice, only per-line capped at 256 KB). Up to `DefaultHLSMaxSegmentBuffer=8` bodies + the in-flight fetch held simultaneously, per worker.

**Trigger:** A pull HLS source (or any host its segment URI / a 3xx redirect points to) returns a multi-GB segment body. The only bound is the 60 s segClient timeout (~7.5 GB at 1 Gbps). Combines with S-4 (SSRF) — the target is content/redirect-chosen.

**Impact:** A single malicious/compromised/redirected upstream forces unbounded allocation → OOM-kill of the shared process → all streams on the host go down. No config write access needed.

**Fix:** Wrap the body in `io.LimitReader` with a configurable max (e.g. 64 MB) and treat over-limit as a failed/skipped segment; cap the playlist body size and parsed-segment count.

> ✅ **FIXED** in `fix/hls-segment-cap` — `fetchSegmentOnce` now reads via `readCapped(resp.Body, hlsMaxSegmentBytes)` (64 MiB default), which reads at most `cap+1` bytes through an `io.LimitReader` and returns `errBodyTooLarge` if exceeded — so an oversized body is rejected WITHOUT allocating it, and `fetchSegmentWithRetry` treats it as a failed (then skipped) segment rather than killing the stream. `parseM3U8` wraps its body in `io.LimitReader(body, hlsMaxPlaylistBytes)` (8 MiB), which bounds the scanner input and transitively the parsed segment/variant slice. Both caps are package vars so ops can tune and tests can shrink them. Tests `TestReadCapped` (under/at/over cap + proves ≤ cap+1 bytes read via a counting reader), `TestFetchSegmentOnce_RejectsOversizedSegment` (httptest, shrunk cap), `TestParseM3U8_PlaylistBodyCapped`.

---

#### A-8 (HIGH) — DASH output freezes permanently when either track dies mid-session (audio-coupled cut hold has no timeout)
**Files:** `internal/publisher/dash/segmenter.go:294-301` (`buildCutDecision` hold, no deadline), `:141-146` (`Cut` switch); `internal/publisher/dash/packager.go:620-624/692-695` (`haveVideo`/`haveAudio` latched from `videoInit`/`audioInit` which are set once and never cleared, preserved across `onSessionBoundary`).

**Trigger:** Source loses its audio track after `audioInit` is built (encoder fault, failover to a video-only feed — the Manager swaps inputs without restarting the publisher by design). Video IDRs keep arriving so `findIDRCutPoint` always returns idx≥0; `buildCutDecision` computes `targetA>0`, sees `q.AudioLen() < targetA`, and returns `Ok=false` on every 50 ms tick forever. Symmetric: video dies → `VideoLen()==0` makes `Cut`'s first case fail and the second requires `!haveVideo` (false because latched), so `cutAudioOnly` is unreachable.

**Impact:** Live edge frozen permanently for all DASH viewers; FrameQueue saturates `maxQueueSpanMs` and overflow-drops every incoming frame; the MPD stops updating. HLS on the same stream keeps working, masking the failure. No watchdog recovers it — only a manual restart. (Both safety-net call sites also funnel through `buildCutDecision` with `haveAudio=true`, so there is no escape path.)

**Fix:** Un-latch track presence based on arrival liveness rather than init existence: record `lastVideoFrameAt`/`lastAudioFrameAt`; once past `WaitingForPairing`, compute `haveX := initX != nil && (queueLen>0 || now-lastXFrameAt < trackLossTimeout)` with `trackLossTimeout ≈ 6 s` (the existing safety-net deadline). Audio death → `haveAudio=false` → coupling skipped → video cuts resume; video death → `haveVideo=false` → `cutAudioOnly` engages. On a declared-dead track's first resumed frame, re-anchor its next segment tfdt to wallclock so the outage gap isn't baked in as permanent A/V desync.

> ✅ **FIXED** in `fix/dash-track-death` — `tryCut` now derives `haveVideo`/`haveAudio` from a new `liveTrackPresence(now)` instead of the latched `videoInit`/`audioInit` pointers. `handleH264`/`handleAAC` stamp `lastVideoFrameAt`/`lastAudioFrameAt` on every accepted frame; once the stream is `StateLive` (only — `WaitingForPairing`/`SessionBoundary` keep init-presence so the pairing handshake and post-reset stale timestamps aren't disturbed), a track with an empty queue and no arrival within `trackLossTimeout` (6 s, a package var so tests can shrink it) is declared dead. Audio death → coupling skipped (`buildCutDecision` audio block gates on `haveAudio`) → video cuts resume; video death → `cutAudioOnly`. The flag flips back automatically when the track resumes (re-stamp), and the wallclock-anchored tfdt lands the resumed segment at the live edge with no extra re-anchoring needed. Test `TestLiveTrackPresence_TrackDeath` (audio-dead, video-dead, draining-queue, recent-frames, pairing-no-downgrade, single-track cases).

---

### 2.5 Concurrency

---

#### C-1 (HIGH) — No per-stream serialisation of coordinator Start/Stop/Update; manager.Register overwrites state and leaks the monitor goroutine
> ✅ **FIXED** in `fix/lifecycle-self-heal` — a per-stream lifecycle mutex (`lockStream`) now wraps the full body of `Start`/`Stop`/`Update` (via `startLocked`/`stopLocked`/`updateLocked` so re-entrant internal calls don't deadlock); a racing op re-checks `IsRunning` under the lock and no-ops. `manager.Register` refuses a duplicate registration instead of overwriting (no monitor-goroutine/ticker leak). Test `TestLifecycle_ConcurrentStartStopSerialised` (-race).

**Files:** `internal/coordinator/coordinator.go:271` (unlocked `IsRunning` TOCTOU), `:330-340` (loser's `pub.Start`-failure rollback runs `mgr.Unregister` + `buf.Delete` on the **winner**'s resources, keyed only by stream code); `internal/manager/service.go:401-403` (`s.streams[code]=state` overwrites unconditionally, prior cancel unreachable), `:405` (second monitor goroutine spawned), `:698-709` (orphaned monitor exits only on ctx.Done).

**Trigger:** Two operations on the same code race the multi-step wiring — most reachably two concurrent `/restart`, or a `DELETE`/template-reload `Stop` racing a reconciler/handler `Start`. `Coordinator.Stop` can land anywhere inside `Start`'s wiring (including the slow `tc.Start` subprocess spawn). **Corrections:** the "bootstrap vs reconciler" sub-trigger is wrong (`BootstrapPersistedStreams` completes before `RunReconciler` is spawned); the pure double-Start window on the normal path is microseconds — the practically wide trigger is the unguarded Stop-interleaves-Start variant.

**Impact:** The winning pipeline's manager registration, ingest worker, and buffers are destroyed by the loser's rollback (`buffer.Delete` closes all subscriber channels, killing HLS/DASH/DVR/transcoder consumers). **Worse than reported:** the winner's orphaned publisher entry is only removed by an explicit `pub.Stop` the rollback never calls, so every subsequent reconciler Start (10 s) re-runs the destructive rollback — the stream stays down until manual `/restart`. Each overwrite leaks a monitor goroutine + 2 s ticker forever; Unregister racing a fresh Register can `ingestor.Stop` the new worker (registered-but-never-ingesting zombie). The ABR paths already fixed this identical race for themselves (`abr_mixer.go:139-148`), and the `RunReconciler` doc comment (`:926-929`) falsely claims "Start checks IsRunning under its own lock".

**Fix:** Add a per-stream keyed mutex (`map[StreamCode]*sync.Mutex` under a guard lock) acquired for the full body of `Start`, `Stop`, `Update`, and `reloadTranscoderFull`; re-check `IsRunning` after acquiring so the loser is a clean no-op. Make `manager.Register` return an error (or cancel-and-replace the prior `monCtx`) when the code is already registered. Fix the false `RunReconciler` comment.

---

## 3. Stability Matrix

### 3.1 Ingest stability per source type

| Scenario | Expected | Actual | Stability | Key issues |
|---|---|---|---|---|
| **rtsp-pull** (AV) | ctx-aware connect+backoff; in-worker reconnect on blip; IDR carries SPS/PPS; per-track wallclock PTS | Open ignores ctx (`pull/rtsp.go:69-70`); param-set invariant enforced; **any** connection end → channel close → io.EOF → terminal stop (`:446-463`→`worker.go:284-296`), so the 1s→10s reconnect never runs — recovery is the ~10-15 s manager probe cycle | degraded-ok | io.EOF conflation; Open not cancellable; unguarded `uint64(pts/90)`; silent DTSExtractor drops |
| **rtmp-pull** (lal) | reconnect+backoff; AMF tolerance; IDR invariant; monotonic PTS | lal chosen for AMF safety; dial ctx-abortable; IDR invariant enforced; same io.EOF-conflation as RTSP (`rtmp.go:157-172`); stale comment at `:146-148` claims reconnect that no longer exists | degraded-ok | io.EOF terminal; 16384-chan full-drop can discard an IDR; G.711/MP3 audio dropped silently |
| **hls-pull** (raw-TS via tsnorm) | poll, dedup by seq, retry transient, absorb burst, signal discontinuity | retries + fast-fail status codes; **unbounded master→master resolution loop** (`hls.go:392-407`); MEDIA-SEQUENCE rollback stalls ~30-40 s; every reconnect re-emits the full window; `io.ReadAll` no size cap; parsed discontinuity flag is dead state | fragile | A-7; unbounded resolve loop; seq-reset stall; duplicate content on reconnect |
| **udp-multicast** (raw-TS) | IGMP/SSM+iface join, RTP strip, tolerate garbage, never stall, detect silence | strongest reader: ASM/SSM/IPv6+iface, IP_MULTICAST_ALL disabled, SO_RCVBUFFORCE; malformed datagrams dropped; non-EOF socket errors reconnect; silence → 15 s stall marker → 30 s manager timeout | solid | manager probe re-binds same port w/o SO_REUSEADDR (harmless, live-worker recovery covers); burst loss unmetered |
| **srt-pull** (gosrt) | dial+timeout, reconnect on transport error, ctx-cancellable reads | non-EOF errors reconnect with backoff (one of the few that exercises it); ctx-cancel via watcher goroutine; clean close → io.EOF terminal + probe | solid | `srt.Dial` not ctx-aware; clean remote close costs a probe round-trip per cycle |
| **file** (.ts passthrough / MP4 / FLV) | realtime pacing, seamless loop, corrupt files surfaced | MP4/FLV paced + continuous-PTS loop; **.ts passthrough has NO pacing** — pumps at disk speed, `loop=true` default spins a core forever and races PTS ahead of wallclock; non-loop EOF loops via manager probe; corrupt container = infinite head-replay | fragile | unpaced looping .ts spin; loop=false unenforceable; corrupt container never surfaces as permanent failure |
| **http-ts** (chunked) | single long GET, reconnect, status classified, low latency | bounded open; status fast-fail; non-EOF reconnect; clean close → terminal+probe; ctx propagates | solid | 32 KiB per-Read alloc churns GC |
| **copy://** (buffer tap) | behave like a network reader: retry until upstream exists, follow restarts | retries until upstream config exists; mode frozen at construction; **violates Open→Close→Open contract** (`closed` flag → re-Open fails permanently `copy.go:111-113`); single-stream mode silently drops TS packets | degraded-ok | re-Open permanent failure on retriable error; silent TS-drop on upstream shape flip (B-6/B-7) |
| **mixer://** (V+A from 2 upstreams) | interleave clock-independent sources; video death aborts; audio policy-controlled | policy implemented; bursts absorbed via TSDemux+pacing; same Open-after-Close permanent failure (`mixer.go:163-165`); `videoEOF`/`audioEOF` atomics dead | degraded-ok | re-Open failure; dead policy atomics; continue-mode audio loss has no event/metric |
| **rtmp-push** (lal server) | registered keys only; one active pusher; session PTS anchor; disconnect→failover; stopped streams reject | routing+single-pusher enforced; per-session Normaliser+boundary; **lifecycle hole: `Registry.Unregister` has zero production callers** — stopped/deleted streams keep accepting pushes that write to a torn-down buffer at Debug level while the encoder believes it's live | degraded-ok | stale push slots survive Stop/Delete forever; buffer-write failures never tear down session/notify manager; S-8 (no auth) |
| **srt-push** (listener ingest) | SRT encoders connect to our server, TS passthrough | **does not exist** — only the RTMP push server starts; the only SRT listener is the publisher's play server (no PUBLISH branch). A `srt://0.0.0.0:9999` input registers a junk slot, reports `ErrNoPusherConnected`, and sits Degraded forever | broken | documented-but-unimplemented; no config-time rejection distinguishes "no encoder yet" from "no listener exists" |

**Cross-cutting (ingest):**
- **io.EOF conflation defeats in-worker reconnect for AV-path sources** (RTSP/RTMP collapse every session end into io.EOF → terminal; backoff only ever exercised by UDP/SRT/HTTP-TS). Recovery for RTSP/RTMP blips is the ~10-15 s manager probe cycle, not the intended 1-2 s. → root of A-4-adjacent latency; stale `rtmp.go:146-148` comment confirms drift.
- **tsnorm is a single point of permanent per-worker degradation** → **A-4**.
- **tsnorm silently strips every non-{H.264,H.265,AAC} ES** (`tsnorm.go:594-607`): MPEG-1/2 audio (standard for DVB multicast), AC-3, SCTE-35, teletext — a DVB feed with MP2 audio ingests video-only with no error.
- **PacketReader lifecycle contract is asymmetric** — copy/mixer/bufferTSChunk latch a `closed` flag and fail re-Open, unlike UDP/HLS/SRT/File/HTTP-TS.
- **Push registry entries leak across the lifecycle** (`Unregister` no callers) → stopped streams keep accepting blackholed pushes.
- **Error classification by string matching** (`worker.go:612-642` greps `x509:`/`tls:`/`no such host`/`HTTP \d{3}`) — fragile to dependency wrapping changes; duplicated in `hls.go:94-121`.
- **Silence-death detection is slow by default** (~45 s worst case: 15 s stall marker + 30 s manager timeout) even with a healthy backup input.

### 3.2 Failover / manual switch / fallback

| Scenario | Expected | Actual | Stability | Key issues |
|---|---|---|---|---|
| Error-triggered priority failover | degrade active, start backup with no gap, record switch, notify via boundary | pre-connect handoff keeps old worker until new Open; Failover session minted; commitSwitch re-checks under lock | solid | old worker's in-flight batch can consume the one-shot SessionStart latch on an old-source packet (boundary fires one batch early) |
| Timeout-triggered failover | silence detected within packet-timeout, failover fires | 2 s monitor tick + 30 s default timeout; same switch path | solid | ~32 s dead air by design; silent worker only cancelled at backup's first Open |
| **Failover commit fails (ingestor.Start error)** | retry/surface so stream comes up on some input | `tryFailover` logs+returns; state stays pointing at the degraded input; no scheduler can re-trigger → **permanent wedge until manual restart** | fragile | overlaps **A-5**; no SwitchEvent recorded |
| **Failover/switch TO a publish:// backup before encoder connects** | backup slot registered, waits for encoder, recovers on packets | synchronous `ErrNoPusherConnected` inside `tryFailover` → on a 2-input stream the backup is instantly Degraded → recursive failover finds nothing → exhausted; later encoder connect can't clear it (`RecordPacket` recovery requires `exhausted==true`, now false) → **permanently sticky StatusDegraded** | broken | deterministic on the common pull-primary + publish-backup ladder; manual override to a not-yet-connected publish input is auto-cleared |
| Manual switch `POST /switch` | force input; clear error when impossible; override persists | validates+sets override; lands via normal handoff | degraded-ok | returns 200 `{switched}` even when `ingestor.Start` fails/no-ops; switching to already-active no-ops silently; can't switch to a Degraded input until 8 s probe cooldown |
| All inputs die → recover | flag degraded while offline; auto-restart on recovery | exhausted flag; publisher/transcoder/DVR stay up; probe (8 s cooldown) or self-heal bypass | degraded-ok | recovery floor ≈ 8 s+tick+probe; probe opens a 2nd connection (single-session RTSP cameras fail forever); file:// loops EOF→exhausted→probe |
| Failback to recovered higher-priority | switch back respecting cooldowns | sweeper `shouldFailbackNow` closes the probe-inside-cooldown race | solid | — |
| Transcoder decoder-swap on switch | only decoder rebuilt; encoders survive; new segment at IDR | SwitchInput flushes old decoder through live encoders, rebuilds decoder only, forceIDR | degraded-ok | swap rides the lossy fan-out (full chan at boundary → marker dropped → no Switch sent, timeline jumps); `NotifyInputSwitch` is dead code; heterogeneous ladder failover (raw-TS→AV backup) → AAC into H.264 decoder (B-1) |
| SessionStart boundary in buffer hub + publisher | boundary flushes output state, signals discontinuity | HLS flushes+schedules DISCONTINUITY; DASH drops queues; RTMP/SRT/RTSP re-arm | solid | per-consumer marker loss when a subscriber channel is full at the boundary write |
| **DVR across failover/switch** | continue recording with discontinuity; index consistent | per-worker Normaliser restarts PTS at ~0 after a switch; DVR writer never re-anchors `originPTSms` → post-switch fragments compute wallMs ≈ first-worker start time, `ensureHour` rotates **back** to the origin hour and `openHour` **deletes** its blobs | broken | failover/switch/restart on a non-transcoded AV-path DVR stream >1 h destroys origin-hour blobs and mis-indexes; raw-TS DVR records nothing (D-2) |

**Cross-cutting (failover):**
- **Session boundary is a one-shot marker on a lossy channel** (`buffer/service.go:63-78`): every boundary consumer (transcoder swap, HLS discontinuity, DASH queue drop, DVR reset, re-stream writers) can independently and silently miss it under backpressure — exactly when boundaries occur. No re-delivery. **Fix once:** have consumers edge-detect on `pkt.SessionID` change (already stamped on every packet) rather than the `SessionStart` bit. (See lower-confidence items on this.)
- **Two-writer window during pre-connect handoff** violates the single-writer buffer contract.
- **`transcoder.Service.NotifyInputSwitch` is dead code** (zero callers) — the decoder swap is purely reactive to the marker packet, so marker loss = no swap.
- **Documentation drift:** the documented state machine `ACTIVE→DEGRADED→DEAD→SWITCH` doesn't match the actual `Idle/Active/Degraded` (StatusStopped is checked but never assigned).
- **Manager exhausted-flag and coordinator degradation map can permanently diverge** (the publish:// recursion path).
- **Every worker replacement restarts the buffer-hub timeline at ~0** — HLS/DASH/transcoder absorb the backward jump; DVR does not.

### 3.3 Transcoder configuration

| Scenario | Expected | Actual | Stability | Key issues |
|---|---|---|---|---|
| Copy / no transcode | no subprocess; bytes passthrough | matches; `shouldRunTranscoder` requires both copies false | solid | **trap: `video.copy=true` is a real copy only if `audio.copy` is also true** — flipping audio.copy alone crash-loops; stale "mpegts copy passthrough profile" comment |
| Full re-encode, single rendition, raw-TS source | decode/scale/encode; continuous ms PTS; A/V sync; faults isolated | works on TS sources; shared ptsOffset + monotonic clamps + 33-bit wrap guard; audio held when leading >500 ms | degraded-ok | AV-path sources broken (B-1/B-2); no fps resampling; SAR/ResizeMode/Refs/Interlace sent but never read → stretch, no pad/crop/deinterlace; GOP unset → 250-frame default |
| ABR on NVENC GPU | 1 NVDEC → N scale_cuda+NVENC in VRAM; encoders survive switch | implemented as designed; hardware-verified seamless swap | degraded-ok | NVENC open deferred → session exhaustion = respawn loop (no CPU fallback); decoder-only fallback incoherence (CPU frames → CUDA scaler → respawn loop); **runtime ladder edits broken** (see cross-cutting); no NVENC session admission control |
| ABR on CPU (+ "fallback") | libx264 ladder; auto full-CPU fallback when NVENC unavailable | explicit hw=cpu works; **"fallback" only downgrades the decoder name, not the encoder** → hw=nvenc on a GPU-less host = permanent respawn loop | fragile | misleading "CPU pipeline fallback" log; VAAPI/QSV appear unworkable (no hwupload graph for CPU yuv420p → hw encoder) |
| Audio passthrough | AAC unchanged to all renditions, rebased PTS, A/V sync | TS path works; 500 ms lead gate + 5 s valve | degraded-ok | audio gated on first video IDR → up to one GOP dropped at start/switch; **audio-only TS streams totally dead**; AV-path crashes (B-1) |
| **Audio re-encode** | fixed output format; seamless switch; EBU R128 when normalize=true | core path works for TS; **`video.copy=true + audio.copy=false` → `singleOriginCopyProfile{W:0,H:0}` → "dimensions not set" → respawn loop (empirically verified)** | broken | the "fix audio only" config crash-loops; `Normalize` plumbed end-to-end but **no loudnorm filter exists** (silently ignored); zero-value audio.copy=false silently re-encodes audio at defaults; toggling audio.copy live → ErrNotImplemented reload path |
| Watermark ON (text/image, CPU+GPU) | overlay after scale; GPU round-trip works; edits hot-reload | per-rendition graph; GPU folds into scale_cuda with hwdownload/upload (contradicts stale "GPU = no watermark" comment); edit → full subprocess+publisher reload | degraded-ok | GPU pays N× VRAM↔RAM round-trip; bad font/deleted asset → respawn loop; image/movie path through `Filter` untested; every edit interrupts all viewers |
| Watermark OFF | no graph, zero overhead | correct | solid | — |

**Cross-cutting (transcoder):**
- **AV-path sources into ANY transcoder config are broken** (root of **B-1**, **B-2**): no PTS on the wire (1000 fps timeline) + AAC fed to the H.264 decoder (silent audio / respawn loop) + no re-probe on AV→TS failover. Only raw-TS sources are viable transcoder inputs today; nothing validates or documents this.
- **Runtime ladder/global/audio edits on a live transcoded stream are broken**: diff routes non-topology changes to per-profile reload, but `StopProfile` is a no-op and `StartProfile` returns `ErrNotImplemented` (`transcoder/service.go:440-459`). Profile UPDATE → API error, old config persists. Profile REMOVE → rendition buffer deleted while the subprocess still writes → **permanent crash loop**. The package doc claims "a ladder change restarts the whole subprocess" — the routing does not.
- **Subprocess crash recovery resets the output timeline to ~1 ms without a SessionStart** — every respawn (including the crash-isolation path the design relies on) risks a stuck output (DASH `behindPrevSegEnd` wedge / HLS uint64 underflow) until the next ingest-side boundary.
- **H.265 ingest into the transcoder is broken on every backend** → **A-3** (TS) + **B-3** (Annex-B).
- **Negative B-frame-warmup DTS crosses as uint64 two's-complement** (`supervisor.go:389-390`) — correctness depends on every consumer casting back to int64; only the DASH packager guards it.
- **Transcoded output bypasses `timeline.Normaliser`** — the pipeline's wallclock-free rebase substitutes for it, so encoder stalls compress out of the timeline rather than re-anchoring (AST drift over multi-day runs).
- **Multiple stale/misleading comments** (`stream_pipeline.go:110-116`, `coordinator.go:1076-1126`, `service.go:12-13`, `pipeline.go:63-91`) that will misdirect future fixes.

### 3.4 Publish/output type — output quality

| Scenario | Expected | Actual | Stability | Key issues |
|---|---|---|---|---|
| HLS live, TS, single rendition | IDR-aligned segments, accurate EXTINF, DISCONTINUITY on switch, atomic writes | AV cuts at first IDR past segDur; raw-TS splits at PAT-before-IDR; atomic writes; PCR + RAI PSI | solid | cold-start audio-lead PTS underflow → tiny first segment; EXTINF omits last frame's duration (HLS-only; DASH fixed it); TARGETDURATION recomputed mid-stream (spec deviation); no PROGRAM-DATE-TIME; audio-only → 8 s force-flush + discontinuity every segment; **`live_ephemeral=false` default → segments never deleted → unbounded disk** |
| HLS live ABR | accurate master; cross-variant alignment | per-rendition segmenter; debounced master | degraded-ok | no cross-variant alignment (per-shard seq diverges on any force-flush/drop; no PDT → switch content jump); CODECS resolution-guessed not SPS-parsed; master absent until first shard flushes |
| HLS fMP4/CMAF live | (asked) | **no live fMP4/LL-HLS** — TS only; only DVR timeshift uses HLS-fMP4 (VOD) | degraded-ok | HEVC-over-HLS relies on TS-in-HLS (works hls.js/ffmpeg, **not Safari**) |
| DASH live fMP4 single | non-overlapping timeline, SAP=1, V/A coupling, ADTS split | heavily defended; behindPrevSegEnd gate; cut-before-IDR; next-PTS dur; splitADTSBundle | solid | **HE-AAC mis-signalled as AAC-LC**; sustained clock skew → holds cuts until 30 s span cap sheds frames; per-sample even-divided DecodeTime jitters VFR; `ephemeral=false` → unbounded disk; finalFlush bypasses pacing |
| DASH live ABR | shared AST/timeline for seamless switch | **each shard keeps its own AST**; `combineSnapshots` takes the first, ignores divergence | fragile | **B-4** (cross-rep AST offset; audio AdaptationSet inherits packing-shard AST) |
| RTMP play-out | FLV H.264+AAC, seq headers at t=0, monotonic, one timeline | mux→demux→FLV; onMetaData+AVC seq header; bundled ADTS split | degraded-ok | **fixed A/V offset** (separate video/audio bases — audio early up to ~170 ms, no RTCP to correct); **HEVC plays nothing** (video dropped → firstVideo gate drops audio too); relPTS uint32 wrap risk |
| RTSP play-out | valid SDP, monotonic RTP, realtime pacing, rebase | direct AV + raw-TS fallback; backward-jump rebase+clamp; shared pacer | degraded-ok | **RTP 32-bit wrap mishandled** → ~30-60 s frozen RTP time then forward jump once per ~13.25 h (video)/~24.9 h (audio), all viewers together; audio-only → nothing; H.265 dropped; init failure silently never mounts |
| SRT play-out | 188-aligned TS relay, clean reset on switch | passthrough + AV-FromAV; 7×188 batching; SessionStart drops muxer+carry+batch | solid | no TS-level discontinuity beyond fresh PSI/CC; per-client backpressure drop invisible |
| MPEG-TS over HTTP | raw TS relay for MPEGTS-enabled streams | forwards `pkt.TS` only, `continue`s on AV → **all transcoded + AV-path streams return 200 + empty body forever** (transcoder writes `Packet{AV}`) | fragile | silently dead output for transcoded/AV-path streams; stale "transcoder output is Packet.TS" comment; `isClientGone` sentinel never wrapped |
| RTMP push-out | clean handshake, seq headers per session, CTS preserved, reconnect | lal PushSession+TLS; CTS preserved; shared baseDTS (no inter-track offset); SessionStart→reconnect | degraded-ok | **cold-start double connect** (subscriber created before handshake → first SessionStart=true → immediate teardown+reconnect; the "consumed by setup" comment is unimplemented); every failover = full remote reconnect; HEVC/MP2/MP3 dropped |
| DVR record (CMAF) | per-hour blobs, crash-safe index, rotation, discontinuity | reuses live builders (byte-identical fragments); O_EXCL blobs + CRC16 ranges + sentinel | fragile | **D-2** (raw-TS records nothing); mid-hour restart discards the whole partial hour; SessionStart drops queued fragments; audio-only never records; `ProfileDesc.Codec` never populated; `Gaps` never appended |
| DVR HLS timeshift | master + per-track VOD playlists, EXT-X-MAP, PDT, codecs | VERSION:7, MAP, DISCONTINUITY+re-MAP, PDT, ENDLIST; keyframe-snapped window | degraded-ok | **CODECS always `avc1.4d401f`** (catalog never records codec → HEVC DVR breaks in browsers); ReadInit always newest hour's init (codec/res change → corrupt decode); hour pre-filter off-by-one-fragment; empty-video window → TARGETDURATION:0 |
| DVR DASH timeshift | static MPD, shared origin, lossless $Time$, codecs | type=static; shared PTO=T0; exact-tick $Time$ | degraded-ok | **cross-profile tick origins not actually shared** (per-lane anchor → rep offset); same codec defaulting (HEVC declared avc1); HE-AAC LC mis-signalling carried forward |

**Cross-cutting (publish):**
- **Stale architecture comments caused a real output gap**: `serve_mpegts.go:173-176` and `hls.go:7-10` describe transcoder output as `Packet.TS`, but `supervisor.go:386-393` has written `Packet{AV}` since the native-transcoder migration — the MPEG-TS-HTTP handler's AV-skip and the DVR writer's TS-skip are mirror-image blind spots, neither validated/logged.
- **`live_ephemeral=false` default** → both HLS and DASH skip all segment deletion/window trimming → every long-running live stream grows disk + in-memory window state without bound.
- **Codec coverage is asymmetric and fails silently**: H.265 works in HLS/DASH/SRT/DVR-record but is silently dropped by RTMP play, RTSP, RTMP push (and the firstVideo gate then suppresses audio too) → HEVC produces zero output with no error; DVR records HEVC but declares `avc1` in playback manifests.
- **HE-AAC mis-signalled end-to-end** on fMP4 paths (`BuildAACInit` hardcodes AAC-LC + 1024 samples/frame).
- **Segment/fragment disk writes happen under the packager/segmenter mutex on the ingest path** → slow disk back-pressures into the buffer-hub subscriber channel → silent packet drops (output gaps, no error).
- **Only the DASH packager has drop diagnostics** — HLS, RTMP/RTSP/SRT play, and push have no equivalent counters, so quality regressions there are invisible in metrics.
- **DASH ABR AST coherence is asserted in comments but enforced nowhere** (`abr.go:173-177`) → **B-4**.

---

## 4. Lower-Confidence / Needs-Follow-Up

Reported by finders, **not independently re-verified** in this pass. Several restate confirmed root causes (noted).

**Availability / DoS**
- HLS master-playlist resolution loop has no recursion/redirect depth cap (`hls.go:392-407`) — tight no-backoff GET flood on a self/mutually-referential master. *(medium; reported twice; matches §3.1 cross-cutting)*
- UDP `chan_buf` query param sizes a channel with no upper bound (`udp.go:147/463-465`) → eager allocation panic/OOM from a single malformed URL. *(medium)*
- No gRPC `MaxRecvMsgSize` override (`cmd/open-streamer-transcoder/main.go:82`, `supervisor.go:174`) — a >4 MiB IDR/chunk → `ResourceExhausted` → data-driven respawn loop. *(medium; reported twice)*
- DASH `$Time$` fragment fallback scans every `.ranges` file for a non-matching tick (`reader.go:239-282`); `vEquiv` overflow (`:289`) → CPU/IO amplification on an unauthenticated route. *(medium; reported twice; same reader as A-2)*
- Full-replace `PUT /config/yaml` tears down omitted streams/hooks/vod (`config_yaml.go:124/196/443`) — highest single-call blast radius. *(medium; gated by S-1)*
- Unauthenticated disk-fill + arbitrary directory creation via VOD create+upload (`vod.go:322/482`). *(medium; same root as S-5 + S-1)*
- Whole-DB read+rewrite on every store op amplifies into memory/CPU/disk DoS at scale (`json/store.go:80/112/139`, `reconcileOnce` lists every 10 s). *(medium)*
- HLS-pull stalls on upstream MEDIA-SEQUENCE reset (`hls.go:298-301`). *(medium; matches §3.1)*
- Exhausted-recovery dead end if `ingestor.Start` fails after a successful probe (`manager/service.go:901-908/1039`). *(medium; overlaps A-5)*
- `UpdateInputs` removing the active input with no replacement leaves its worker running (`manager/service.go:1148-1156`). *(medium)*
- DVR `StartRecording` failure at stream start logged and never retried (`coordinator.go:752-764`). *(low)*

**Business bugs / correctness**
- FLV file pacing uint32 underflow → ~49.7-day sleep on a 1 ms DTS regression (`file.go:593-614`). *(medium)*
- Raw `.ts` file sources have no pacing; `loop=true` default spins at disk speed (`file.go:161-186`). *(medium; matches §3.1 file cell)*
- RTMP play uses separate V/A timestamp bases → constant ~170 ms offset (`serve_rtmp.go:260-294`). *(medium; matches §3.4)*
- SRT play `?token=` streamid rejected at connect (`serve_srt.go:232-243`). *(medium; = S-13)*
- RTSP play 32-bit RTP wrap mishandled (`serve_rtsp.go:846-851/935-940`). *(medium; matches §3.4)*
- DASH packager doesn't re-arm the IDR-startup gate on session boundary; stale init survives codec change (`packager.go:596-607`). *(medium)*
- HLS keyframe-cut unsigned PTS subtraction underflow on audio-leading start (`hls.go:427`). *(low; matches §3.4)*
- RTMP push retry `Limit` counts successful sessions; terminal "failed" state deleted instantly (`push_rtmp.go:399-411`). *(medium)*
- MPTS program filter trusts any PMT section on the learned PID (`mpts_filter.go:234-276`). *(low)*
- Transcoder health flaps healthy↔unhealthy on every respawn during a crash loop (`supervisor.go:250-253` vs `:121-122`). *(medium)*
- Catalog `Gaps` never populated; buffer-hub drops invisible; status always reports zero gaps (`catalog.go:83`, `writer.go:333-349`). *(medium)*
- Multi-profile DVR per-lane origin anchoring skews the timeshift window between profiles (`writer.go:124-129`). *(medium; matches §3.4 DVR-DASH)*
- Integer overflow / missing bounds on `from`/`dur` timeshift params (`timeshift_params.go`). *(low; same reader as A-2)*
- Unknown `storage.driver` silently selects YAML backend (`cmd/server/main.go:151-176`). *(low)*
- Templates never validated — invalid Inputs/Watermark/mixer shapes enter pipelines via inheritance/hot-reload (`template.go:123-161`). *(medium; same family as B-5/B-6)*

**Data loss**
- JSON/YAML store rename without fsync → crash can corrupt the single config DB; no backup (`json/store.go:112-125`). *(medium; also ties to S-11 mode)*
- `ReadRanges` bounds audio records against `max(.cmfv,.cmfa)` → torn-audio records in live playlists (`reader.go:368-374`). *(low)*
- `wrapADTS` writes frameLen into a 13-bit field with no bounds check (`audio_reencode.go:494-516`). *(low)*
- Negative/wild encoded PTS cast int64→uint64 at the supervisor boundary with no validation (`supervisor.go:389-390`). *(low; latent — pipeline currently guarantees PTS≥1)*
- `watermarks.writeAtomic` claims fsync but doesn't; 8 MiB cap enforced after rename (`watermarks/service.go:316-348`). *(low)*

**Concurrency**
- **SessionStart boundary on a droppable packet — per-consumer marker loss** (`buffer/service.go:63-78`); fix once via `SessionID` edge-detection. *(medium; reported twice; = §3.2 cross-cutting — high-value single fix)*
- Failover handoff race: late writes from the previous worker consume the one-shot SessionStart marker (`worker.go:99-118`, `service.go:487-495`). *(medium; = §3.2)*
- `spawnProtocolLocked` respawn doesn't wait for the old goroutine → old+new HLS/DASH segmenters write the same dir concurrently (`service.go:306-327`). *(medium)*
- `Service.Stop` vs `UpdateProtocols`/`RestartHLSDASH` race: `wg.Add` can race `wg.Wait` (panic) + dir resurrection (`service.go:377-393`). *(low)*
- Data race in `sessions.TrackHTTP` — snapshot copied after lock released (`tracker.go:300-307`). *(medium; `-race` would flag)*
- Auto-publish `ResolveOrCreate` holds the service lock across full `coordinator.Start` (`autopublish/service.go:174-196`). *(medium)*
- Stream/template `Put` non-atomic read-modify-write → lost updates (`stream.go:344-419`). *(medium)*
- Auto-publish liveness subscribe failure orphans a running pipeline the reaper can't stop (`autopublish/service.go:233-263`). *(low)*
- `Start`'s `clearDegradation` runs after `mgr.Register` → startup-window exhaustion wiped, stream reports Active while offline (`coordinator.go:368` vs `:319`). *(medium; same family as A-5)*
- `tryFailover` commit race: stale `commitSwitch` aborts after the worker swapped → active pointer diverges from the live worker (`manager/service.go:887-956`). *(medium)*
- Reconciler can resurrect a just-deleted stream as a permanent ghost pipeline (`coordinator.go:954-972` vs `stream.go:707-708`). *(medium; same family as C-1)*
- `buffer.Subscribe` races `buffer.Delete` → subscriber lands on an orphaned ring, `Recv` blocks forever (`buffer/service.go:242-287`). *(medium)*
- Event bus worker pool (default 4) doesn't preserve publish order → correlated lifecycle events reach hooks inverted (`events/bus.go:54-62`). *(medium)*
- `Event.ID`/`OccurredAt` unset at almost every publish site → hook retries deliver non-dedupable duplicates with zero timestamps (`events/bus.go`). *(medium)*

**Performance / resource leaks**
- Per-stream counter label series never deleted; auto-publish runtime codes (client-chosen paths) mint permanent series → unbounded registry growth (`metrics.go:221-428`). *(medium)*
- RTMP play spawns a full mux→16 MiB tsBuffer→demux per client, no cap (`serve_rtmp.go:97-232`). *(medium; = the per-connection cost behind A-1)*
- Hooks dispatcher reads+unmarshals the entire DB on every delivered event (`hooks/service.go:215`). *(medium)*
- Sessions tracker takes a global write lock on every manifest/segment GET (`tracker.go:297`). *(medium)*
- RTSP serve `pendingAudio` unbounded when SPS/PPS never appear in-band (`serve_rtsp.go:639-672`). *(medium; reported twice)*
- Buffer-hub fan-out deep-copies every packet per subscriber under the ring lock, including dropped packets (`buffer/service.go:70-79`). *(low)*
- DASH ABRMaster debounce flush can recreate the stream dir after Stop+cleanup (`abr.go:95-216`). *(low)*
- Disabling HLS/DASH via `UpdateProtocols` leaves stale manifests served as a frozen live stream (`service.go:439-456`). *(low)*

---

## 5. Prioritized Remediation Plan

### P0 — Do first (security trust boundary + silent data loss)

| Item | Findings | Rough effort |
|---|---|---|
| **Authenticate the admin API** (fail-closed middleware on all admin/mutating routes; loopback-default or refuse non-loopback bind without auth; replace mutating `RealIP`) | S-1, S-15; unblocks/contains S-2/S-3/S-4/S-5/S-6/S-8/S-9/S-10/S-13/A-1/A-2, config-replace DoS | M (1-2 days; new config block + middleware + tests) |
| **Hook sink containment + validation** (file `pathInside` allowlist; http `DialContext` SSRF guard + `CheckRedirect`; validate in Create/Update) | S-2/S-6 | M (1-2 days) |
| **Watermark `font_color` allowlist + lavfi escaping** (Validate color literal; single-quote+escape `fontcolor`; fix `escapeLavfiArg` single-quote) | S-3 | S (half day) |
| **SSRF egress guard at dial time** (shared `DialContext` `Control` for hls/httpts transports + RTSP/RTMP/SRT/UDP dials; opt-out for RFC1918) | S-4; bounds A-7 | M (1-2 days) |
| **DVR: stop lying about recording + stop wiping the catalog** (refuse/flag raw-TS DVR; `LoadCatalog`+merge on `StartRecording`; retention reconcile against disk) | D-1, D-2 | M (2-3 days incl. origin reconciliation + tests) |

### P1 — High-impact correctness/availability (no attacker required)

| Item | Findings | Rough effort |
|---|---|---|
| **Fix AV-path transcode** (add `codec`+`pts_ms`/`dts_ms` to InputPacket proto, regenerate via protoc, forward+route audio; advance fallback PTS by frame-duration) | B-1, B-2 (and unblocks heterogeneous-ladder failover) | M-L (proto + supervisor + pipeline + tests; 2-4 days) |
| **Codec-aware decoder selection / reject HEVC loudly** (rebuild decoder on codec change, or terminal error instead of respawn loop) | A-3, B-3 | M (1-2 days) |
| **Per-stream lifecycle mutex** in coordinator Start/Stop/Update/reloadTranscoderFull; manager.Register rejects/replaces duplicate registration | C-1 (+ ghost-stream, clearDegradation, commit-race family) | M (1-2 days) |
| **Self-heal partial reload + zombie streams** (`reloadTranscoderFull` falls back to full Stop; manager.Register routes Start failure through ReportInputError + Idle-too-long degrade) | A-5, A-6 | S-M (1-2 days) |
| **tsnorm restart-on-error + recoverable Process** | A-4 | S-M (1 day) |
| **DASH track-death liveness un-latch** (arrival-based haveVideo/haveAudio) | A-8 | M (1-2 days) |
| **Template-aware auto-start + copy/mixer + classifier** (resolve before eligibility gates; template-aware upstream lookups; delegate `inputSourceIsRawTS` to `protocol.Detect`) | B-5, B-6, B-7 | M (shared theme: template resolution at read sites; 1-2 days) |
| **Playback connection caps** (per-stream + global, before pipeline alloc) + bounded HLS segment `io.LimitReader` | A-1, A-7 | M (1-2 days) |
| **DVR window clamp + ctx cancellation** | A-2 (+ `$Time$` fallback bound, param overflow) | S-M (1 day) |
| **SessionID-based boundary detection** (consumers edge-detect on `pkt.SessionID` instead of the droppable one-shot bit) | §3.2 cross-cutting + buffer/handoff race items | M (touches all publishers + transcoder + DVR; 2-3 days) |

### P2 — Hardening, hygiene, and known-config breakage

| Item | Findings | Rough effort |
|---|---|---|
| Redact secrets in logs/metrics/RuntimeStatus/API; store-file `0o600`/`0o700` + fsync | S-7, S-10, S-11, S-9, store-fsync | S-M |
| Wire push-ingest StreamKey + runtime-stream caps/rate-limit; call `Registry.Unregister` on Stop | S-8, push-slot leak | M |
| RTSP exact-mount lookup (drop prefix scan) | S-9 (RTSP wrong-stream) | S |
| Timeshift master-playlist param validation/encoding; SRT `?token=` query strip | S-12, S-13 | S |
| gRPC `MaxRecvMsgSize`; UDP `chan_buf` clamp; `.ts`/MP4-corrupt/file-loop pacing | lower-conf availability | M |
| RTP 32-bit wrap (RTSP), separate-base A/V offset (RTMP play), FLV uint32 underflow, HE-AAC signalling, EXTINF last-frame, runtime ladder-edit routing | publish/transcoder correctness | M-L |
| `live_ephemeral` default + window trimming; metrics label pruning; hooks-dispatch DB-read caching; sessions sharding | perf/resource leaks | M |
| pprof loopback enforcement + on-demand profiling; gRPC socket `0700` dir; storage-driver fail-fast; event ID/OccurredAt+ordering | hygiene | S-M |
| Stale-comment cleanup across transcoder/publisher/coordinator | doc drift (misdirects future fixes) | S |

**Effort key:** S ≈ ≤0.5 day, M ≈ 1-3 days, L ≈ >3 days. Several P1 items collapse into shared work: the **InputPacket proto change** (B-1+B-2+heterogeneous failover), the **read-time template resolution** sweep (B-5+B-6+B-7+template validation), and the **SessionID boundary** refactor (transcoder swap reliability + all publisher boundary handling + the handoff/drop races) each fix multiple listed symptoms at one site.