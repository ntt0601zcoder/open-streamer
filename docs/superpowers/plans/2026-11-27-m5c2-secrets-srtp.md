# M5c2a: Secrets and SRT Output Encryption Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** No stored secret leaves the server again: credentials embedded in URL paths and queries are redacted everywhere, every API response is redacted for every role while a write that sends the mask back keeps the stored secret, and an SRT output can require a per-stream passphrase.

**Architecture:**
- **The URL stays the carrier of an input's and a push's secrets.** Nothing in the data plane changes: the pipeline, the pushers and the listen claims still identify an entry by its URL text. What changes is the API boundary: a response always shows `redact.URL`'s form, and a write that carries that form is resolved against the stored document before it is validated (R-T3, R-T4).
- **One secret has no URL, the SRT output's passphrase,** so it is the one new spec field, `outputs.srt_passphrase`, shown as the mask and kept by the mask (R-T5). `srtout` asks for it per connection request and `stream`'s `reattach` ends the stream's SRT sessions when it changes.
- **The config editor serves and takes the same masks** on the YAML text: `config.Mask` on the way out, `config.Unmask` on the way in, both on bytes, so a write still stores the client's exact bytes outside the scalars it resolved (R-T8). The file on disk holds the real values, as it does today (R-T7).
- **The hub is untouched.** SRT encryption is gosrt's, at the edge; a passphrase check runs in the accept loop of the SRT listener and never near a hub write.

**Tech Stack:** Go 1.26, stdlib, `datarhei/gosrt` v0.13.0 (already a dependency), `go.yaml.in/yaml/v3`. Peers: ffmpeg 8 on the host; ffmpeg 6.1, VLC 3.0.20 and GStreamer 1.24 in `osv2-ci-peers:m4a`.

**Spec:**
- `docs/REBUILD_PLAN.md` §5, the M5c row: "… SRT encryption (input and output), credentials in URL paths and queries redacted, secrets as write-only spec fields". Exit: "ffmpeg publishes and plays over each secured protocol against our server; no secret appears in a response, a log or a metric".
- The M5c2 row of §5 (line 829) and the "Out of M5c1" table (lines 1362–1376), with the facts under it (1378–1384).
- The M2c note (line 1476): "Credentials in the URL path or query (`?user=&password=`) | M5c2's redaction work".
- M3b3's "SRT encryption | Needs credentials: M5c2; encrypted requests are refused" (line 936), and M2a's SRT input passphrase (`internal/ingest/srt.go:36`, `:184`).
- §3.6's API and Users bullets (lines 633–647, 710–725): `?secrets=1`, the keyed ETag, the config editor.
- The controller's scope and binding constraints for M5c2, recorded below in the rulings and the Global Constraints.

## Rulings (R-T)

Each ruling ends with what it costs if it is wrong.

- **R-T1 (the split).** M5c2 is two plans, as M5b and M5c were. Its four items are two independent subsystems with no shared file but the docs.
  - **M5c2a (this plan), secrets:** credentials in URL paths and queries redacted; secrets write-only over the API; SRT output encryption, which needs the first write-only field.
  - **M5c2b (a later plan), SRTP:** `pkg/srtp` (SRTP and SRTCP on the standard library, RFC 3711 Appendix B vectors), `RTP/SAVP` with SDES keys inside RTSPS for the RTSP server and the pull client. See "Out of M5c2a" and "Hand-over: M5c2b", which records what the peers were seen to do.
  - **M5c2a's exit criterion:** ffmpeg 6.1 and 8 and our own client play an SRT output that requires a passphrase, and a caller without it or with a wrong one is refused; no secret, the credentials embedded in a URL path or query and the write-only field included, appears in any response to any role, in a log or in a metric; an editor's read, change and write of a stream and of the whole config keeps every stored secret without ever being shown one.
  - **M5c2b's exit criterion (proposed):** ffmpeg 6.1 and 8 play `rtsps://` with SRTP over UDP and over the interleaved channel against our server, and our client pulls the same from our server.
  - *If wrong:* SRTP waits one plan longer. Nothing here closes a door for it: this plan does not touch `pkg/rtsp`, `rtspplay` or `internal/ingest/rtsp.go`.
- **R-T2 (what a credential in a URL is).** `redact.URL` hides, and `redact.Masked` recognises, exactly these, on every scheme. A *secret name* is a name that, percent-decoded and lower-cased, contains `pass`, `pwd`, `psw`, `secret`, `token`, `signature` or `credential`, or is `key`, `pw`, `sig`, `auth`, `hdnts`, `hdnea` or `wmsauthsign`, or ends in `key` (`redact.SecretName`).
  1. **The userinfo:** the password, or a lone user (as today).
  2. **A query pair** whose key is a secret name: its value, also when empty (as today, with the wider name rule).
  3. **A pair inside a path segment, and inside the fragment.** A pair starts at the start of the segment or after one of `&`, `;`, `,` or `_`, with a name matching `[A-Za-z][A-Za-z0-9-]*` followed by `=`; its value runs to the separator before the next pair's start, or to the end of the segment. The value of a pair with a secret name is hidden. So `user=admin_password=a_b_channel=1` hides `a_b`, and `user=admin&password=x&channel=1` hides `x`. The path is taken percent-decoded, so `pass%77ord=x` is found.
  4. **The value of a `streamid` query pair:** after an optional `#!::`, its comma-separated pairs by rule 3; and the fourth field of `read:<path>:<user>:<pass>` or `publish:<path>:<user>:<pass>` (MediaMTX's form).
  5. **The stream key of the RTMP family:** the last path segment cut to 2 characters (as today).
  - A user name is never hidden: `user=admin` stays, as the userinfo's user does.
  - `logging.Scrub` needs no new rule: it passes every URL-shaped token through `redact.URL`, and `pkg/rtsp`'s errors never quote a URL or a request path (`StatusError`, `pkg/rtsp/client.go:141-150`). A slog attribute whose key is a secret name is masked, beside the keys `logging` lists today.
  - **The rule over-masks on purpose.** `?passthrough=1`, `?bypass=1` and a file path `/media/pass=1/x.ts` show `xxxxx`. Only what is shown changes: the stored URL is used as written.
  - *If wrong:* a camera whose password holds `_name=` or `&name=` leaks the tail after it, exactly as that camera's own parser would cut it; a credential under a name the rule does not know (`?p=`, `?k=`) is shown until the name is added to `redact.SecretName`, which is one line and one table row.
- **R-T3 (write-only, and where a secret lives).** A stream's secrets stay inside its input and push URLs; there are no separate `password`, `key` or `passphrase` fields for them. Write-only is a property of the API, not of the document's shape:
  - **Every response is redacted for every role.** `?secrets=1` is gone (R-T6). The mask `xxxxx` in a response is the marker that a secret is stored.
  - **A write that sends the mask keeps the stored secret** (R-T4). A write that sends a real value sets it. A write that leaves the secret out (a URL without it, no `srt_passphrase`) removes it: a stream is replaced whole by `PUT` (R-D7), so absence means absent.
  - **Why not separate fields.** (a) The data plane identifies an input and a push by its URL text (`Pipeline.SetInputs`, the pushers' map at `internal/stream/generation.go:227-231`, `pushID`): a secret outside the URL would need a second identity everywhere. (b) Credentials in a path (`/user=admin_password=x_channel=1`) cannot be lifted into a field at all, so URL redaction stays load-bearing whatever else is built, and separate fields would be a second scheme beside it. (c) A separate field lets an editor point the URL at another host and keep the password, which sends a secret nobody showed him to a server of his choice. Matching by the redacted form (R-T4) refuses that.
  - *If wrong:* a UI cannot offer one "password" box per input: it shows the URL with `xxxxx` in it and the operator retypes the whole URL to change any secret part. Adding fields later is additive (a field that composes into the URL before the pipeline sees it); removing them would not be.
- **R-T4 (how the mask is resolved).** One rule, for a stream `PUT` and for every stream entry of a config `PUT`:
  - A sent input URL that `redact.Masked` is replaced by the stored input URL of **the stream of the same name** whose `redact.URL` form **equals the sent text**. A push URL likewise among the stored pushes. A sent `srt_passphrase` equal to the mask is replaced by the stored passphrase.
  - **Exactly one** distinct stored value must match. None (a new stream, a renamed stream, a changed host, user, path or query, a URL where one secret was retyped and another left masked, no stored passphrase) and more than one (two stored keys that redact alike) are refused with 422 `invalid_config`, one text per field, naming the field and never a value: `inputs[0].url: holds the redaction mask and the stored stream has no one secret it stands for; send the real value`.
  - Matching is by value, not by index: reordering inputs or pushes keeps their secrets.
  - The resolution runs under the config lock, against the config in force, immediately before validation. `spec.Stream.Validate` still refuses the mask (`checkURL`), so a mask can never be stored whichever path forgot to resolve it.
  - *If wrong:* an editor who changes a camera's port must retype its password. That is the price of (c) above and it is the intended behaviour, not a gap.
- **R-T5 (the one new field: `outputs.srt_passphrase`).** Per stream, not per server.
  - A server-wide passphrase would be a `server` key, so changing it would need a restart (R-D4), and every stream would share one secret. gosrt decides per connection request (`ConnRequest.SetPassphrase`), and the stream ID names the stream before the passphrase is needed, so per stream costs nothing.
  - It covers every served name of the stream, its rungs included.
  - **Valid:** needs `outputs.srt: true`; 10 to 79 bytes (gosrt takes 10 to 80; libsrt before 1.5 stops at 79), each a printable ASCII character `0x21`–`0x7E`, so every player's URL and command line can carry it; never the mask. Errors name the field and the length, never the value.
  - A change of it is an `outputs` edit (`spec.Diff` needs no change): applied in place, no rebuild.
  - **The key length is the caller's** (16, 24 or 32 bytes): gosrt takes what the caller's key material says. No config key.
  - *If wrong:* an operator who wants one passphrase for all streams writes it once per stream.
- **R-T6 (the API).**
  - `GET /api/v1/streams`, `GET /api/v1/streams/{name}`, the `PUT` answer and the status documents are `Redacted()` for viewers and editors alike.
  - **`?secrets=1` is answered 400 `bad_request` for every role**, after authentication, with a text that says secrets are write-only and that the mask keeps a stored one. It is not silently ignored: a script that relied on it must fail loudly, not start storing `xxxxx` somewhere else. The viewer's 403 for it goes away with the parameter.
  - **ETag and If-Match are those of the stored document** (the keyed tag of `spec.Stream.ETag`, unchanged). A `PUT` answers with the stored document redacted and its ETag, so a client can chain edits without ever holding a secret.
  - **Diff is taken after resolution.** A `PUT` of exactly what `GET` returned is `unchanged`: nothing is written and nothing is reopened.
  - `Backend.PutStream` takes the document as sent and returns the document as stored; the API no longer refuses masks itself (`maskedURLs`, `internal/api/streams.go:72-85`, is deleted).
  - *If wrong:* a tool that used `?secrets=1` to copy a stream to another server must read the secrets from the file, with shell access. That is the point of the ruling.
- **R-T7 (the file).** The config file holds every secret in clear, as it already holds stream keys and URL passwords.
  - It cannot hold them hashed: a stream secret must be usable, a login password only verifiable. Encrypting them needs a key that would sit beside the file.
  - The file's protection is the one it has: `config.File` refuses it on Unix when group or others have any mode bit (`internal/config/file.go:145-172`), and writes it 0600.
  - **A reload reads real values.** A mask in the file is a validation error for that stream, as today.
  - `SetStream` writes the resolved document, so the file never holds a mask.
  - *If wrong:* whoever can read the file reads the secrets. Encryption at rest with an external key is a backlog item.
- **R-T8 (the config editor).** `GET /api/v1/config` returns the text **masked**; `PUT /api/v1/config` takes masks and resolves them (R-T4), then stores the resolved bytes. The ETag stays `config.Hash` of the stored text, so `If-Match` works as today and two stored texts that differ only in a secret have different ETags and the same body.
  - **What is masked (`config.Mask`):** under `streams`, every scalar value that holds `://` and whose `redact.URL` form carries a mask is replaced by that form, wherever it sits (a mistyped key included); every value of a key `srt_passphrase` is replaced by the mask; and in every comment of the document each URL-shaped token goes through `redact.URL`, as a log line does. A URL that holds no secret keeps its bytes, also when `redact.URL` would normalise it.
  - **What is resolved (`config.Unmask`):** in each stream entry, `inputs[i].url`, `outputs.push[j].url` and `outputs.srt_passphrase` that hold the mask, against the stored text's entries of the same name. Nothing else. A comment is stored as sent.
  - **Exact bytes.** Both work on the bytes: the scalar's own text is replaced in place and everything else is copied. So `Unmask(Mask(t), t)` is `t` byte for byte, and a `PUT` without any mask stores the client's exact bytes, as today. Each checks its own result by decoding it; when a scalar's text cannot be replaced in place (a double-quoted scalar with escapes, a folded or multi-line scalar), the whole document is re-encoded through the node tree instead, which loses layout exactly as `SetStream` does. Neither ever returns a text that still holds what it had to hide.
  - **Users' password hashes are not masked.** They are one-way (PBKDF2, 600 000 iterations), the editor replaces the `users` section through this route, and this plan's spec is "spec fields".
  - *If wrong:* (a) a commented-out input line comes back with its key masked, and a `PUT` of that text stores the masked comment: the secret in the comment is gone from the file. The alternative is that every editor reads every commented-out key. (b) A URL with a secret inside a watermark text is masked on `GET` and stays masked after a round trip. (c) An editor still reads the users' hashes and can attack them offline; masking them is a backlog item with this plan's mechanism.
- **R-T9 (SRT output encryption).** In `srtout`, for a request that names a live stream:
  - **The stream has no passphrase:** exactly as today. An encrypted request is refused with `REJ_RESOURCE` and counted as `RejectedMode`, whether or not the name is known.
  - **The stream has a passphrase:** an unencrypted request is refused with `REJ_UNSECURE`; an encrypted one is checked with `SetPassphrase`, and a wrong passphrase is refused with `REJ_BADSECRET`. Both are counted in `RejectedSecret`. libsrt then tells the player "Password required or unexpected" or "Incorrect passphrase" (seen with ffmpeg 8), which is what a customer with a typo needs.
  - **A flood of wrong passphrases.** Each check costs one PBKDF2 in the accept loop. A peer key (the IP; an IPv6 address by its /64, `peerKey`) with 5 wrong passphrases inside 10 s has its encrypted requests refused unchecked, with `REJ_RESOURCE`, counted in `RejectedLimited`, until 10 s after its first counted failure. The source address is real: gosrt yields a request only after its cookie exchange. At most 4096 keys are tracked; when the table is full, expired entries are dropped, and a failure that still finds no room is not tracked.
  - **A changed passphrase ends the stream's SRT sessions** (set, changed or removed): they were admitted under the old rule. `reattach` refuses the hubs' `"srt "` subscribers once and relaxes again, the mechanism that ends a disabled output's sessions; the sessions end as `hub_closed`.
  - **Key refresh is gosrt's default** (every 2^24 packets, announced 2^12 before). `srtout.Options.KMRefresh` exists so a test can cross a refresh in seconds.
  - *If wrong:* the two distinct refusals tell someone without the passphrase that a name is live. Stream names are not secrets, and `REJ_RESOURCE` for everything would send every customer with a typo to support.
- **R-T10 (migration).** No file changes: a config that works at HEAD loads unchanged, and no key is renamed. What changes for clients is the API: `?secrets=1` is 400, `GET /api/v1/config` is masked, and a stream `PUT` with a mask is resolved instead of refused. The exceptions are listed under Global Constraints.
- **R-T11 (peers).** `srt-live-transmit` is on neither the host nor `osv2-ci-peers:m4a` (`which srt-live-transmit` finds nothing in both). It is libsrt's own tool and ffmpeg links the same libsrt, so ffmpeg's rows stand for it; no image is rebuilt for it. ffmpeg's rows alone carry the exit criterion; the VLC and GStreamer rows are marked † and follow M5c1's rule for a peer that provably cannot do a row.

## Peers: what was checked when this plan was written

Run on 2026-11-27 against the host's ffmpeg 8.0.1 and `osv2-ci-peers:m4a` (ffmpeg 6.1, GStreamer 1.24.2), with a scratch gosrt listener that requires a passphrase and sends a 40 s TS clip of 1000 frames.

| Peer | Checked | Result |
|---|---|---|
| ffmpeg 8 | `srt://…?passphrase=<13 bytes>` | 1000 of 1000 frames |
| ffmpeg 8 and 6.1 | the same with the listener's `KMRefreshRate` 1024 (five refreshes in 5123 packets) | 1000 of 1000 frames |
| ffmpeg 8 | `-pbkeylen 32` | 1000 frames: the caller's key length is taken |
| ffmpeg 8 | a wrong passphrase, with `REJ_BADSECRET` | "ERROR:BADSECRET … Incorrect passphrase", exit 251 |
| ffmpeg 8 | no passphrase, with `REJ_UNSECURE` | "ERROR:UNSECURE … Password required or unexpected", exit 251 |
| ffmpeg 8 | passphrases of 79 and 80 bytes | both play |
| GStreamer 1.24 | `srtsrc uri=… passphrase=…` | Connects encrypted and takes every packet; decoding was not asserted (†) |
| VLC 3.0.20 | `--srt-passphrase` | Not run (†) |
| host and image | `which srt-live-transmit` | Absent in both (R-T11) |

## Out of M5c2a

| Item | Where |
|---|---|
| SRTP for RTSP over UDP and interleaved (`pkg/srtp`, SDES `a=crypto`, `AES_CM_128_HMAC_SHA1_80`), server and pull client | M5c2b (R-T1) |
| MIKEY key management for SRTP (what GStreamer and MediaMTX speak) | Backlog, judged in M5c2b |
| Separate secret fields for inputs and pushes (`password`, `key`) | Not planned (R-T3) |
| Masking the users' password hashes in `GET /api/v1/config` | Backlog (R-T8) |
| Secrets encrypted at rest; a secrets file or environment references | Backlog (R-T7) |
| A server-wide SRT passphrase; a configured key length | Not planned (R-T5) |
| An SRT input passphrase outside the URL | Not planned (R-T3): it stays `?passphrase=`, redacted and kept by the mask like every URL secret |
| `srt-live-transmit` as a peer | Not planned (R-T11) |
| Scrubbing bare `name=value` text outside URLs in logs | Backlog: nothing logs a bare request path today (R-T2) |

## Global Constraints

- **Libraries:** stdlib, plus the approved dependencies already in `go.mod`: mp4ff, pion/rtp, pion/sdp, datarhei/gosrt, testify, go-astiav, protowire, go.yaml.in/yaml/v3. No new module; `go mod tidy` changes nothing. pion/srtp is not approved. Never bluenviron or lal, and never another product's code.
- **Frozen behaviour:** with no `srt_passphrase` and no credential of R-T2's new kinds in any URL, the server binds, serves, logs and reports exactly what it does at HEAD 6e8edb1, and plain RTSP and SRT are byte-identical on the wire. `pkg/rtsp`, `rtspplay` and `internal/ingest/rtsp.go` are not modified. Every existing test passes unchanged. The intended exceptions:
  1. `?secrets=1` is 400 for every role (it was the unredacted document for an editor and 403 for a viewer), and the rows that used it;
  2. every stream response is redacted for an editor too;
  3. `GET /api/v1/config` returns masked text when the config holds a secret (byte-identical when it holds none), and the rows that compared it with the file;
  4. a stream or config `PUT` that carries a mask is resolved when R-T4 allows, and the 422 text of one that is not;
  5. `redact.URL`'s and `redact.Masked`'s results for URLs with R-T2's new credentials, R-T2's over-masked names included, wherever they are shown (statuses, logs, errors);
  6. `/metrics` gains `openstreamer_output_rejected_total{output="srt",reason="secret"}` and `reason="limited"`, at 0;
  7. `TestReattachCoversEveryOutput`'s list, the OpenAPI document and the tests that hold it equal to the spec;
  8. `conform`'s SRT read errors quote the redacted URL (they quoted the URL).
- **House rules:**
  - the hub is the only data source and hub writes never block;
  - hot reload goes by diff: a passphrase change is an `outputs` edit, and no `server` key is added;
  - errors are `module: op: %w`, never both logged and returned;
  - every goroutine has a cancel path; lock order is `Service` mu, then per-stream mu;
  - all configuration is the one YAML file, rewritten atomically by API writes; the API is Basic auth with viewer and editor roles;
  - no secret in a log, a metric, an error or a response, to any role.
- **Lint:** `exhaustive`, `godot`, gofumpt.
- **Tests:**
  - bind `127.0.0.1:0` only; never 554, 1935 or 8554;
  - keys and certificates are generated in-process (`pkg/tlstest`); passphrases and credentials in tests are literals that appear nowhere else, so a leak scan can search for them;
  - every peer test passes on the host (ffmpeg 8) and in `make test-ci-peers` (ffmpeg 6.1, VLC 3.0.20, GStreamer 1.24) before any push;
  - the arm64 job has only ffmpeg and sets `OS_REQUIRE_PEERS=1`;
  - `make test-pinned` (go.mod's Go 1.26.11; the host has 1.27.2) passes before any push.
- **Environment:** the v1 soak containers (open-streamer-soak-mediamtx, caddy, grafana, osdev-console-1, vector, alloy, pyroscope, prometheus, node-exporter, nvidia-gpu-exporter) are never stopped, restarted or reconfigured. Own containers are named `osv2-*` and run with `--rm`. No GPU work.

## Review Focus

1. **An editor's read, change and write.** `GET` a stream whose input has a password, whose push has a key and whose SRT output has a passphrase; change the audio language; `PUT` what came back. The stored secrets are kept, the file holds the real values, the input is not reopened, and the same holds for the whole config through `GET` and `PUT /api/v1/config`, byte for byte outside the edit. *Pinned by Task 2's `TestUnmask`, Task 4's `TestMaskUnmaskRoundTrip` and Task 6's `TestPutStreamKeepsSecrets` and `TestPutConfigKeepsSecrets`.*
2. **A mask pointed somewhere else.** An editor keeps `xxxxx` and changes the host, the port, the user or the path; copies a masked URL into another stream or a new one; retypes one of a URL's two secrets; or two stored keys redact alike. Each is 422 naming the field, nothing is stored, and no connection is made to the new host. *Pinned by Task 2's `TestUnmaskRefuses`, Task 4's `TestUnmaskErrors` and Task 6's `TestPutStreamMaskMoved`.*
3. **Camera URLs that do not look like the table.** A password that holds `_`, `&` or a percent-escape; an upper-case `PASSWORD=`; an empty password; a credential in a path segment that is not the last; the same in an input's failure text, a status and a log line. *Pinned by Task 1's `TestURLEmbeddedCredentials`, `TestMaskedEmbeddedCredentials` and `TestOpenErrorsHideEmbeddedCredentials`, and Task 8's `TestM5c2aNoSecretsLeak`.*
4. **YAML that is not one URL per plain line.** A secret URL in single or double quotes, in a flow sequence with another URL on the same line, in a double-quoted scalar with an escape, in a folded scalar, in a comment, under a mistyped key, and in an entry the lenient start skipped: `GET /api/v1/config` never shows the secret, and a round trip stores it back. *Pinned by Task 4's `TestMaskOddYAML` and `TestMaskNeverLeaks`.*
5. **An SRT player with the wrong passphrase, a thousand of them, and a passphrase changed under a player.** A wrong or missing passphrase is refused with its own reason and never served a byte; 1000 wrong requests from one address cost at most 5 checks per 10 s and a player from another address still connects at once; a changed passphrase ends the connected players within a second and the new one admits them; a session that crosses a key refresh loses no frame. *Pinned by Task 3's `TestPassphraseRefusals`, `TestPassphraseLimiter` and `TestKeyRefreshFFmpeg`, Task 5's `TestReattachPassphrase` and Task 8's `TestM5c2aSRTFFmpeg`.*

---

## Execution waves

| Wave | Tasks | Why |
|---|---|---|
| 1 | 1 (`redact`, `logging`), 2 (`spec`), 3 (`srtout`, `conform`) | Disjoint files. Task 2 uses only what `redact` exports at HEAD and keeps a private copy of the mask, which Task 4 replaces by `redact.Mask` |
| 2 | 4 (`config`), 5 (`stream`) | Both need Task 2's field and `KeepSecret`; 4 needs Task 1's rule for its rows. Disjoint files |
| 3 | 6 (`api`, `server/apply.go`), 7 (`server/server.go`, `server/collect.go`) | 6 needs 2 and 4; 7 needs 3 and 5. Disjoint files in `internal/server`, tests included (`apply_test.go` against `server_test.go`) |
| 4 | 8 (e2e, docs, CI, the gate) | Needs all |

Each task runs in its own worktree, branched from `rebuild` after the wave before it has merged. A fix to a merged task is a new commit, never an amend.

---

### Task 1: `redact`, credentials in paths, queries and stream IDs

**Files:**
- Modify: `internal/redact/redact.go` (`mask` at 40, `secretKeys` at 44, `URL` at 54–98, `Masked` at 133–170, the package comment)
- Modify: `internal/logging/logging.go` (`mask` at 17, `secretKeys` at 25–28, `scrubAttr` at 106–135)
- Test: `internal/redact/redact_test.go`, `internal/logging/logging_test.go`, `internal/ingest/secrets_test.go`

**Interfaces:**
- Produces:

```go
package redact

// Mask replaces a hidden value in what URL returns, and is what a write sends to keep a stored secret (R-T3).
const Mask = "xxxxx"

// SecretName reports whether name, percent-decoded and lower-cased by the caller or not, names a credential (R-T2):
// it contains pass, pwd, psw, secret, token, signature or credential; or is key, pw, sig, auth, hdnts, hdnea or
// wmsauthsign; or ends in key.
func SecretName(name string) bool

// URL and Masked keep their signatures. URL hides R-T2's five places; Masked reports whether rawURL carries the
// mask in any place URL writes it.
```
  - `logging` uses `redact.Mask`, and `scrubAttr` masks an attribute whose key is in its own list or is a `redact.SecretName`.
  - For every URL `u` that `Masked` does not report: `Masked(URL(u))` is true exactly when `URL` hid something, and `URL(URL(u)) == URL(u)`.

- [ ] **Step 1: Write the failing tests.**

```go
func TestSecretName(t *testing.T)
// true: password, Password, PASSWD, pwd, psw, pw, pass, passphrase, secret, client_secret, token, access_token, hdnts, hdnea,
//   wmsauthsign, sig, signature, X-Amz-Signature, X-Amz-Credential, X-Amz-Security-Token, auth, key, apikey, api_key, authkey,
//   streamkey, stream_key, pass%77ord after the caller's unescape
// false: user, username, usr, channel, stream, subtype, mode, transport, latency, streamid, iface, loop, pace, pkt_size,
//   Policy, Key-Pair-Id, author, keyframe, design

func TestURLEmbeddedCredentials(t *testing.T) // redact.URL, input → output; the camera table of R-T2
//   rtsp://192.0.2.1:554/user=admin&password=s3cret&channel=1&stream=0.sdp?real_stream
//     → rtsp://192.0.2.1:554/user=admin&password=xxxxx&channel=1&stream=0.sdp?real_stream            (Xiongmai, & form)
//   rtsp://192.0.2.1/user=admin_password=tlJwpbo6_channel=1_stream=0.sdp?real_stream
//     → rtsp://192.0.2.1/user=admin_password=xxxxx_channel=1_stream=0.sdp?real_stream                (Xiongmai, _ form)
//   rtsp://192.0.2.1/user=admin_password=a_b_channel=1        → …/user=admin_password=xxxxx_channel=1 (a password with _)
//   rtsp://192.0.2.1/user=admin&password=&channel=1           → …/user=admin&password=xxxxx&channel=1 (empty)
//   rtsp://192.0.2.1/USER=admin&PASSWORD=x&channel=1          → …/USER=admin&PASSWORD=xxxxx&channel=1
//   rtsp://192.0.2.1/user=admin&pass%77ord=x%26y&channel=1    → the value x&y is hidden whole; no "x" and no "y" pair is left
//   rtsp://192.0.2.1/live/user=admin&password=x/ch1           → …/live/user=admin&password=xxxxx/ch1  (not the last segment)
//   rtsp://cam/av_stream?user=admin&password=x                → …?user=admin&password=xxxxx           (the M2c note)
//   rtsp://cam/h264/ch1/main/av_stream?username=admin&passwd=x → passwd hidden, username kept
//   http://cam/videostream.cgi?user=admin&pwd=x               → pwd hidden                            (Foscam)
//   rtsp://cam/videoMain?usr=admin&pwd=x                      → pwd hidden
//   http://cam/axis-cgi/mjpg/video.cgi?auth=YWRtaW46YWRtaW4=  → auth hidden
//   https://cdn/live.m3u8?hdnts=exp=1~acl=/*~hmac=abc         → hdnts hidden whole
//   https://cdn/live.m3u8?Policy=p&Signature=s&Key-Pair-Id=K1 → Signature hidden, the other two kept
//   https://s3/x.ts?X-Amz-Credential=c&X-Amz-Signature=s&X-Amz-Security-Token=t&X-Amz-Date=d → the first three hidden
//   http://h/live/token=abc123/index.m3u8                     → http://h/live/token=xxxxx/index.m3u8
//   srt://h:9000?streamid=publish:cam:user:pw12345            → …?streamid=publish:cam:user:xxxxx     (MediaMTX)
//   srt://h:9000?streamid=%23!::r=live/cam,token=abc          → the token's value hidden, r kept
//   srt://h:9000?streamid=live/cam&passphrase=0123456789      → passphrase hidden, streamid kept
//   rtsp://h/x#password=abc                                    → the fragment's value hidden
// unchanged, byte for byte: rtsp://cam/Streaming/Channels/101; rtsp://cam/cam/realmonitor?channel=1&subtype=0;
//   http://h/a=b/c; udp://239.0.0.1:1234?pkt_size=1316; srt://h:9000?streamid=live/cam&latency=200&mode=listen;
//   rtsp://h/user=admin_channel=1
// kept from today: every row of the existing table, rtsp://u:pw@cam/s and the three RTMP schemes included
// over-masked, by R-T2: http://h/x?passthrough=1 → …?passthrough=xxxxx; file:///media/pass=1/x.ts → file:///media/pass=xxxxx/x.ts

func TestMaskedEmbeddedCredentials(t *testing.T) // for every row above: Masked(input) is false; Masked(output) is true when
//   the row hid something and false for the unchanged rows; URL(output) == output

func TestScrubEmbeddedCredentials(t *testing.T) // logging: a message "open rtsp://192.0.2.1/user=admin_password=tlJwpbo6_channel=1: refused"
//   comes out without "tlJwpbo6"; attributes named pwd, srt_passphrase, X-Amz-Signature and api_key come out as "xxxxx";
//   attributes named user, stream and input keep their values

func TestOpenErrorsHideEmbeddedCredentials(t *testing.T) // ingest: Parse and Open of each of
//   rtsp://127.0.0.1:1/user=admin_password=tlJwpbo6_channel=1, rtsp://127.0.0.1:1/x?user=admin&password=hunter2-q,
//   http://127.0.0.1:1/live/token=abc123tok/index.m3u8 and srt://127.0.0.1:1?streamid=publish:cam:user:pw12345
//   fail (nothing listens, 2 s context) with an error text holding none of tlJwpbo6, hunter2-q, abc123tok, pw12345
```

- [ ] **Step 2: Run** `go test -count=1 ./internal/redact/ ./internal/logging/ ./internal/ingest/ -run 'SecretName|Embedded'`. Expect failures.
- [ ] **Step 3: Implement.** One unexported function hides the pairs of a string by R-T2's rule 3 and is used for each path segment, the fragment and a `streamid` value; `Masked` uses the same walk to look for the mask. The path is rewritten through `u.Path` with `RawPath` cleared, as the RTMP branch does today.
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/redact/ ./internal/logging/ ./internal/ingest/ ./internal/spec/ ./internal/stream/ ./internal/output/rtmppush/`. Expect a pass: no existing row changes.
- [ ] **Step 5: Commit** `feat(redact): credentials in URL paths, queries and stream IDs are hidden`

---

### Task 2: `spec`, the passphrase field and the mask that keeps a secret

**Files:**
- Modify: `internal/spec/spec.go` (`Outputs` at 94–103, `Redacted` at 149–158), `internal/spec/validate.go` (`Outputs.validate` at 141–157, `checkURL` at 175–185), `internal/spec/doc.go`
- Create: `internal/spec/unmask.go`
- Modify, so that `rebuild` stays green between waves: `internal/api/openapi.json` (the `Outputs` schema gains `srt_passphrase` with its final description: "Required of SRT players of this stream, 10 to 79 printable ASCII bytes. Write-only: a response shows xxxxx when one is stored, and xxxxx written back keeps it"), `internal/stream/reattach_test.go` (the known list at line 23 gains `"SRTPassphrase"` with the comment `// its sessions end on a change: Task 5`)
- Test: `internal/spec/spec_test.go`, `internal/spec/validate_test.go`, `internal/spec/diff_test.go`, `internal/spec/unmask_test.go`

**Interfaces:**
- Consumes: `redact.URL`, `redact.Masked` as they are at HEAD. `redact.Mask` is Task 1's, in the same wave: this task declares `const mask = "xxxxx"` in `unmask.go`, pinned by `TestMaskIsRedacts`, and Task 4 replaces it by `redact.Mask`.
- Produces:

```go
// In Outputs, after SRT:
//   SRTPassphrase string `json:"srt_passphrase,omitempty" yaml:"srt_passphrase,omitempty"` // required of SRT players; write-only (R-T5)

// Redacted also replaces a non-empty SRTPassphrase by the mask.

// ErrNoSecret and ErrAmbiguousSecret are KeepSecret's errors.
var ErrNoSecret, ErrAmbiguousSecret error

// KeepSecret returns the stored URL that the masked URL sent stands for (R-T4): the one distinct element of stored
// whose redact.URL form equals sent. ErrNoSecret when none does, ErrAmbiguousSecret when several distinct ones do.
func KeepSecret(sent string, stored []string) (string, error)

// Unmask returns s with every input URL and push URL that redact.Masked reports, and an SRTPassphrase equal to the
// mask, replaced by stored's (R-T4); stored is nil when no stream of s's name is stored. Fields that hold no mask are
// taken as sent. The error is an UnmaskError naming every field that could not be replaced; s is returned unchanged with it.
func (s Stream) Unmask(stored *Stream) (Stream, error)

// UnmaskError lists the fields a mask could not be replaced in, each as
// "inputs[0].url: holds the redaction mask and the stored stream has no one secret it stands for; send the real value".
type UnmaskError []string
func (e UnmaskError) Error() string // the texts joined with "; "
```
  - `Validate` adds, in `Outputs.validate`: `outputs.srt_passphrase: needs outputs.srt`; `outputs.srt_passphrase: holds the redaction mask, not the secret; send the real value`; `outputs.srt_passphrase: %d bytes, want 10 to 79`; `outputs.srt_passphrase: holds a character that is not printable ASCII (0x21 to 0x7e)`. None quotes the value.
  - `ETag`, `Diff`, `clone` and `normal` need no change: the field is a string inside `Outputs`.

- [ ] **Step 1: Write the failing tests.**

```go
func TestMaskIsRedacts(t *testing.T) // redact.URL("rtsp://u:p@h/s") is "rtsp://u:" + mask + "@h/s"

func TestSRTPassphraseValidate(t *testing.T)
// accepted: srt true with "0123456789" (10) and with 79 bytes of '!'..'~'; srt true and no passphrase; srt false and none
// refused, text as above and never holding the value: a passphrase with srt false; 9 bytes; 80 bytes; "xxxxx";
//   one with a space; one with a tab; one with "é"

func TestRedactedPassphrase(t *testing.T) // Redacted().Outputs.SRTPassphrase is "xxxxx" for a set passphrase and "" for none;
//   the receiver's own field is untouched; the JSON of Redacted() has "srt_passphrase":"xxxxx" and no byte run of the passphrase

func TestDiffPassphrase(t *testing.T) // set, changed and removed are each ChangeOutputs; equal documents are ChangeNone;
//   the ETags of two documents that differ only in the passphrase differ

func TestKeepSecret(t *testing.T)
// stored {"rtsp://u:pw@cam/s", "rtmp://h/live/secretkey"}: "rtsp://u:xxxxx@cam/s" → the first; "rtmp://h/live/sexxxxx" → the second
// stored holding the same URL twice: one distinct value, returned
// ErrNoSecret: "rtsp://u:xxxxx@other/s"; "rtsp://v:xxxxx@cam/s"; "rtsp://u:xxxxx@cam:8554/s"; empty stored
// ErrAmbiguousSecret: stored {"rtmp://h/live/secretkey", "rtmp://h/live/sesame"} with "rtmp://h/live/sexxxxx"

func TestUnmask(t *testing.T) // the round trip of Review Focus 1
// stored: inputs rtsp://u:pw@cam/s and srt://h:9000?passphrase=0123456789, push rtmp://h/live/secretkey, srt on with "srtpass-0123"
// sent := stored.Redacted() with AudioLanguage changed: Unmask(&stored) equals stored but for the language;
//   Diff(stored, result) is ChangeLive; result.Validate() is nil
// sent := stored.Redacted(): Diff(stored, result) is ChangeNone and the ETags are equal
// the two inputs swapped in sent: both resolved, by value
// a real URL beside a masked one: the real one is taken as sent
// sent without srt_passphrase: the result has none (removed, R-T3); sent with a new real passphrase: taken

func TestUnmaskRefuses(t *testing.T) // Review Focus 2; each returns an UnmaskError naming exactly the listed fields, and s unchanged
// stored nil with a masked input → "inputs[0].url: …"
// the host, the port, the user, the path or a query value changed around the mask → that field
// a URL with a password and a ?token=: the token retyped, the password left masked → that field
// two stored pushes that redact alike → "outputs.push[0].url: …"
// srt_passphrase "xxxxx" with no stored passphrase → "outputs.srt_passphrase: …"
// a masked input and a masked push both unresolvable → two texts, inputs first
```

- [ ] **Step 2: Run** `go test -count=1 ./internal/spec/`. Expect a compile failure.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/spec/ ./internal/config/ ./internal/api/ ./internal/stream/`. Expect a pass: `TestOpenAPIMatchesSpec` and `TestReattachCoversEveryOutput` pass because of the two lines under Files. Until Task 5 a changed passphrase does not end sessions, and until Task 7 nothing asks for it: the field is stored and inert.
- [ ] **Step 5: Commit** `feat(spec): outputs.srt_passphrase, and the mask keeps a stored secret`

---

### Task 3: `srtout`, a passphrase per stream; `conform` reads an encrypted output

**Files:**
- Modify: `internal/output/srtout/srtout.go` (the package comment at 1–60, `Options` at 153–172, `Stats` at 175–197, `Server` at 201–220, `Listen` at 258–277, `refusal` at 343–350, `stream` at 412–433, `countRefusal` at 436–448)
- Create: `internal/output/srtout/secret.go` (the failure table)
- Modify: `internal/conform/srt.go` (`readSRT` at 49–80)
- Test: `internal/output/srtout/srtout_test.go`, `internal/output/srtout/secret_test.go`, `internal/conform/srt_test.go`

**Interfaces:**
- Produces:

```go
// In srtout.Options:
// Passphrase, when set, returns the passphrase SRT callers of the stream served as name must present, or "" for none
// (R-T9). It is called in the accept loop for each request that names a live stream, and must not block.
Passphrase func(name string) string
// KMRefresh is the number of packets after which a connection's key is replaced; 0 means gosrt's default (1<<24).
// The new key is announced KMRefresh/4 packets before. Tests lower it.
KMRefresh uint64

// In srtout.Stats, beside the other Rejected counters (Rejected sums them all):
// RejectedSecret counts requests refused for a missing or wrong passphrase; RejectedLimited those refused unchecked
// because their peer key had 5 wrong passphrases in the last 10 s.
RejectedSecret, RejectedLimited uint64
```
  - The order in `stream` (R-T9), which keeps today's counts when no passphrase is configured: the stream ID is parsed (a bad one is `refuseMode`); the name is looked up and, when it names a live stream, `Passphrase` is asked; an encrypted request with no passphrase to check against is `refuseMode`; an unknown or not-live name is `refuseUnknown`; then, with a passphrase: unencrypted is `REJ_UNSECURE`, a limited peer key is `REJ_RESOURCE`, a failed `SetPassphrase` is `REJ_BADSECRET` and one failure in the table. The caps of `answer` come after.
  - The table: peer key → count and the time of its first counted failure; an entry expires 10 s after that time; at most 4096 entries (R-T9). A correct passphrase clears nothing.
  - `conform.readSRT` takes `passphrase` from the URL's query into `srt.Config.Passphrase`, and every error it returns quotes `redact.URL` of the URL instead of the URL.

- [ ] **Step 1: Write the failing tests.** gosrt callers on `127.0.0.1:0`; passphrases are literals such as `"srtpass-0123456789"`.

```go
func TestPassphraseAccepted(t *testing.T) // hubs "open" (no passphrase) and "locked" ("srtpass-0123456789"): a caller with the
//   passphrase reads TS from "locked" whose frames equal what a plain caller reads from "open" fed the same frames; callers with
//   PBKeylen 16, 24 and 32 all read; the served name of a rung, "locked_720", needs the same passphrase when Passphrase says so

func TestPassphraseRefusals(t *testing.T) // Review Focus 5
//   The reason given to ConnRequest.Reject is recorded through a seam of the test (a wrapped request), since a caller's
//   error need not carry it.
//   "locked" without a passphrase: Dial fails, the reason is REJ_UNSECURE, RejectedSecret == 1, no byte was read
//   "locked" with "wrongwrong-0123": REJ_BADSECRET, RejectedSecret == 2
//   "open" with a passphrase: REJ_RESOURCE, RejectedMode moves, RejectedSecret does not (frozen)
//   an unknown name with a passphrase: REJ_RESOURCE and RejectedMode, as at HEAD; without: RejectedUnknown
//   Passphrase nil: every row of the existing refusal table passes unchanged
//   Stats.Rejected is the sum of the six counters after every row

func TestPassphraseLimiter(t *testing.T) // Review Focus 5; the table's clock is injected
//   5 wrong passphrases from one key: RejectedSecret == 5; the next 1000 encrypted requests from it, right or wrong:
//   RejectedLimited == 1000 and RejectedSecret still 5: none of them was checked
//   an unencrypted request from that key to "open" still plays; a caller under another key (the table's key function is
//   a seam) with the right passphrase connects at once
//   the clock moved 10 s past the first failure: the first key with the right passphrase connects
//   4097 keys each failing once: the table holds at most 4096; after 10 s it is empty on the next failure

func TestPassphraseChangedMidLookup(t *testing.T) // -race: Passphrase flips between "" and a value while 50 callers dial:
//   every accepted connection read TS, every refusal is one of the three reasons, no panic, Conns returns to 0

func TestKeyRefreshFFmpeg(t *testing.T) // peer: testpeer.Require(t, "ffmpeg", "srt"). KMRefresh 1024, a hub fed 25 fps video and
//   audio for 12 s of stream time at a rate that sends more than 4096 packets; ffmpeg -i srt://…?streamid=live/locked&passphrase=…
//   -f null decodes every video frame the hub was given after its first keyframe (count within 1 GOP), and its log holds no
//   "corrupt" and no "decrypt"; the same with a gosrt caller: conform reports no violation

func TestReadSRTPassphrase(t *testing.T) // conform: ReadTS of srt://<srtout>?streamid=live/locked&passphrase=srtpass-0123456789
//   captures frames; with a wrong passphrase the error holds neither passphrase and names the redacted URL
```

- [ ] **Step 2: Run** `go test -count=1 -run 'Passphrase|KeyRefresh|ReadSRT' ./internal/output/srtout/ ./internal/conform/`. Expect failures.
- [ ] **Step 3: Implement.** `Listen` sets `cfg.KMRefreshRate` and `cfg.KMPreAnnounce` only when `KMRefresh` is not 0. The table is guarded by `Server.mu`, which `stream` already takes to count a refusal.
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/output/srtout/ ./internal/conform/`, then `-count=10 -run 'Passphrase' ./internal/output/srtout/`, then `make test-ci-peers ARGS='-run TestKeyRefreshFFmpeg ./internal/output/srtout/'`. Expect all to pass.
- [ ] **Step 5: Commit** `feat(srtout): a stream may require a passphrase` and `feat(conform): an SRT read takes a passphrase`

---

### Task 4: `config`, the text masked and resolved

**Files:**
- Create: `internal/config/mask.go`, `internal/config/mask_test.go`
- Modify: `internal/config/doc.go` (a "Secrets" section after "Editing")
- Modify: `internal/spec/unmask.go` (the private `mask` becomes `redact.Mask`; `TestMaskIsRedacts` goes)

**Interfaces:**
- Consumes: `spec.KeepSecret`, `spec.ErrNoSecret`, `spec.ErrAmbiguousSecret`, `Outputs.SRTPassphrase` (Task 2); `redact.URL`, `redact.Masked`, `redact.Mask` (Task 1); `decodeNode`, `streamsNode`, `entryName`, `entryRef`, `encode`, `Errors` of this package.
- Produces:

```go
// Mask returns text as an API response shows it (R-T8): under streams every URL-shaped scalar value whose redact.URL
// form carries a mask is replaced by that form, every srt_passphrase value by redact.Mask, and in every comment each
// URL-shaped token goes through redact.URL. Everything else is copied byte for byte; a text that needs nothing hidden
// is returned as it is. When a scalar's text cannot be replaced in place the document is re-encoded through the node
// tree, as SetStream does. The result never holds a value that had to be hidden. A text that is not YAML is an error.
func Mask(text []byte) ([]byte, error)

// Unmask returns text with every inputs[i].url, outputs.push[j].url and outputs.srt_passphrase of a stream entry that
// holds the mask replaced by the value the entries of the same name in stored hold for it (R-T4, spec.KeepSecret).
// A text without a mask is returned as it is, the same slice. A text that is not YAML is returned as it is: Parse
// reports it. A mask that cannot be replaced is an Errors, one per place, as
// "streams[3] (cam): inputs[0].url: holds the redaction mask and the stored stream has no one secret it stands for; send the real value".
func Unmask(text, stored []byte) ([]byte, error)
```
  - For every text `t` that `Parse` accepts and that holds no secret in a comment: `Unmask(Mask(t), t)` equals `t` byte for byte.

- [ ] **Step 1: Write the failing tests.** `secretText` is a config with three streams: `cam` (input `rtsp://admin:hunter2-pw@192.0.2.1/s`, push `rtmp://h/live/pushkey-0123456789`, `srt_passphrase: srtpass-0123456789`), `dvr` (input `rtsp://192.0.2.2/user=admin_password=tlJwpbo6_channel=1`) and `plain` (input `udp://239.0.0.1:1234`), with comments above entries, blank lines and a 4-space indent in one entry.

```go
func TestMask(t *testing.T) // Mask(secretText): none of hunter2-pw, pushkey-0123456789, srtpass-0123456789, tlJwpbo6 is in it;
//   it differs from secretText only inside those four scalars (a line diff shows 4 changed lines, the same count of lines);
//   it decodes to the same document but for those values, which are the redact.URL forms and "xxxxx"; the users section,
//   hashes included, is untouched; Mask of a text with no secret returns the same slice

func TestMaskUnmaskRoundTrip(t *testing.T) // Review Focus 1: Unmask(Mask(secretText), secretText) == secretText, bytes.Equal;
//   the same after an edit of the masked text that adds a fourth stream and changes plain's input: the result is secretText
//   with exactly that edit; a text without masks comes back as the same slice

func TestMaskOddYAML(t *testing.T) // Review Focus 4; for each form the masked text holds no secret and the round trip restores
//   a document equal to the original (spec.Diff ChangeNone per stream); "in place" rows also restore the bytes
//   in place: url: 'rtsp://u:pw@h/s'; url: "rtsp://u:pw@h/s"; inputs: [{url: rtsp://u:pw@h/a}, {url: rtsp://u:pw2@h/b}] on one
//     line; a URL followed by a line comment; CRLF line ends; a non-ASCII watermark text on the line before
//   re-encoded: url: "rtsp://u:pw@h/s"; url: >- folded over two lines; a URL split by a double-quoted line continuation
//   a mistyped key (inptus: [{url: rtsp://u:pw@h/s}]): masked on the way out, not resolved on the way in

func TestMaskComments(t *testing.T) // a head comment "# old: rtmp://h/live/oldkey-0123456789", a line comment and a foot comment
//   each holding a URL with a secret: masked in Mask's output, the rest of each comment kept; Unmask leaves them as sent

func TestMaskNeverLeaks(t *testing.T) // Review Focus 4: for every text of the package's existing Parse and edit test tables
//   with a secret URL substituted in turn into each scalar under streams, and for 2000 texts made by a fixed-seed generator
//   that picks a quoting, a flow or block style and an indentation for a stream with secrets: the secret is not in Mask's
//   output, and Mask(Mask(t)) == Mask(t)

func TestUnmaskErrors(t *testing.T) // Review Focus 2; each is an Errors with exactly the listed texts, and nothing is returned
//   a masked URL in a stream stored has no entry for → "streams[0] (new): inputs[0].url: …"
//   cam's masked input with its host changed; cam's masked push copied into dvr; srt_passphrase: xxxxx in plain
//   two masks unresolvable in two streams → two texts, in the file's order
//   an entry the lenient start skipped in stored (a bad layout value) whose masked URL is sent back unchanged: resolved,
//     because the match is on stored's nodes, not on a decoded stream
//   stored holding two entries named cam: the candidates are both entries'
```

- [ ] **Step 2: Run** `go test -count=1 -run 'Mask|Unmask' ./internal/config/`. Expect a compile failure.
- [ ] **Step 3: Implement.** Both functions decode to nodes, compute the wanted tree (the nodes with their values or comments replaced), try the replacement on bytes at each scalar's `Line` and `Column`, decode the result and compare it with the wanted tree, and fall back to `encode` of the wanted tree when they differ. The comparison, not the replacement, is what the "never" in both doc comments rests on.
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/config/`. Expect a pass, the existing tables untouched.
- [ ] **Step 5: Commit** `feat(config): the config text masked for a response and resolved on a write`

---

### Task 5: `stream`, the passphrase reaches the SRT server and a change ends its sessions

**Files:**
- Modify: `internal/stream/generation.go` (`reattach` at 330–375 and its comment at 325–329), `internal/stream/manager.go` (beside `Hub` at 605–626)
- Test: `internal/stream/reattach_test.go` (`TestReattachCoversEveryOutput` at 22–36), `internal/stream/manager_test.go`, `internal/stream/supervisor_test.go`

**Interfaces:**
- Consumes: `Outputs.SRTPassphrase`, `Stream.Redacted` (Task 2).
- Produces:

```go
// SRTPassphrase returns the passphrase SRT callers of a served name must present: the stream's outputs.srt_passphrase,
// or "" when it has none, when its SRT output is off or when nothing is served under that name. It reads the View, as
// Hub does, and never waits for an edit.
func (m *Manager) SRTPassphrase(served string) string
```
  - `reattach`: when `old.SRT && new.SRT && old.SRTPassphrase != new.SRTPassphrase`, after `publish` (step 4) every hub of the generation refuses `"srt "` once, on top of `off(new)`, and then relaxes to `off(new)` again: the sessions admitted under the old passphrase end, and a caller that arrives after the View was published is asked for the new one. No other output's subscriber is touched.
  - `Status.Outputs` is taken from `Redacted()` (`status.go:143-155`) and so shows `xxxxx`; `View.Outputs` holds the real value.

- [ ] **Step 1: Write the failing tests.**

```go
func TestReattachCoversEveryOutput(t *testing.T) // unchanged but for Task 2's comment on "SRTPassphrase", which goes: reattach now handles it

func TestReattachPassphrase(t *testing.T) // Review Focus 5. A running stream with srt on and subscribers named "srt a", "srt b",
//   "rtsp c" and "httpts d" on its hub (the seam of the existing reattach tests):
//   passphrase "" → "p1-0123456789": "srt a" and "srt b" are closed, the other two are not, and a Subscribe under "srt e"
//     after the edit is open
//   "p1-0123456789" → "p2-0123456789" and → "": the same
//   an edit that changes only a push, the passphrase equal: no subscriber is closed
//   srt turned off with the passphrase removed in one edit: the srt subscribers end once, by off(), and "srt e" is refused
//   the View published by the edit holds the new passphrase before any subscriber is closed (the published seam)

func TestManagerSRTPassphrase(t *testing.T) // "" for an unknown name, for a stream without one, for srt off; the value for the
//   stream's name and for each rung's served name; after an outputs edit the new value, with no rebuild (Rebuilds unchanged)

func TestStatusHidesPassphrase(t *testing.T) // Supervisor.Status().Outputs.SRTPassphrase is "xxxxx"; the status JSON holds no
//   byte run of the passphrase; Manager.Get returns the real value
```

- [ ] **Step 2: Run** `go test -count=1 -run 'Reattach|SRTPassphrase|HidesPassphrase' ./internal/stream/`. Expect failures.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/stream/`, then `-count=20 -run TestReattachPassphrase ./internal/stream/`. Expect a pass.
- [ ] **Step 5: Commit** `feat(stream): a changed SRT passphrase ends the stream's SRT sessions`

---

### Task 6: `api` and `server`, secrets are write-only

**Files:**
- Modify: `internal/api/api.go` (`Backend` at 30–48, step 6 of the wrapper at 445–449, `asksSecrets` and `showSecrets` at 461–470), `internal/api/streams.go` (`view` at 20–25, `listStreams` at 45, `getStream` at 55, `maskedURLs` at 72–85, `putStream` at 94–157), `internal/api/config.go` (`getConfig` at 39–46), `internal/api/doc.go` (lines 29–36), `internal/api/openapi.json` (the `secrets` parameter at 1064–1068 and its three uses at 136, 195 and 255, the description at line 6, the 403's description at 1175, the URL description at 1388)
- Modify: `internal/server/apply.go` (`state` at 38–41, `publish` at 45–58, the comment of `apply` at 60–68, `PutConfig` at 143–155, `PutStream` at 164–189, `ConfigText` at 249–252)
- Test: `internal/api/streams_test.go`, `internal/api/config_test.go`, `internal/api/auth_test.go`, `internal/api/api_test.go`, `internal/api/openapi_test.go`, `internal/server/apply_test.go`

**Interfaces:**
- Consumes: `Stream.Unmask`, `UnmaskError`, `Stream.Redacted` (Task 2); `config.Mask`, `config.Unmask`, `config.Errors` (Task 4).
- Produces:

```go
// In api.Backend:
// ConfigText is the text last loaded, reloaded or written as a response shows it, masked by config.Mask, and
// config.Hash of the text itself. The text is nil when it could not be masked.
ConfigText() (masked []byte, etag string)
// PutStream creates or replaces one stream. sp is the document as sent: a mask in it stands for the stored secret
// (R-T4). stored is the document as it is now stored.
PutStream(ctx context.Context, sp spec.Stream, ifMatch string) (out stream.Outcome, stored spec.Stream, err error)
```
  - `api`: every stream document in a response is `Redacted()`. `asksSecrets` stays as the test for the exact query; step 6 answers it 400 `bad_request` for every role with "?secrets=1 is gone: secrets are write-only. A response shows xxxxx where a secret is stored, and a write that sends xxxxx back keeps it". `putStream` sets the `ETag` header from `stored.ETag()` and answers `stored.Redacted()`. A nil text from `ConfigText` is 500 `internal`.
  - `server.PutStream`, under `s.cfg`, after the stale and If-Match checks: `sp.Unmask` against `Manager().Get(sp.Name)`; an `UnmaskError` becomes `config.Errors`, each text prefixed `spec: validate: `, which the API already answers 422 `invalid_config`. Everything after works on the resolved document.
  - `server.PutConfig`, after the If-Match check: `config.Unmask(text, s.applied)`, then `apply` as today. A text without masks reaches `apply` as the same bytes.
  - `publish` computes `config.Mask(s.applied)` once per applied text and keeps it in `state`; a failure is logged at error (scrubbed) and leaves it nil.

- [ ] **Step 1: Write the failing tests.**

```go
// internal/api, against the fake Backend of the existing tests
func TestStreamsAlwaysRedacted(t *testing.T) // GET /streams, GET /streams/{name} and the PUT answer, as viewer and as editor:
//   the URLs are redact.URL forms and srt_passphrase is "xxxxx"; none of the fake's secrets is in any body
func TestSecretsQueryGone(t *testing.T) // ?secrets=1 on every authenticated GET route: 400 bad_request for the viewer and the
//   editor, 401 without credentials; ?secrets=0 and ?x=1 are ignored as before
func TestPutStreamPassesMasks(t *testing.T) // a PUT body holding masks reaches Backend.PutStream as sent; the ETag header is the
//   returned stored document's and differs from the sent one's; a Backend error config.Errors is 422 invalid_config with its texts
func TestGetConfigMasked(t *testing.T) // the body is Backend.ConfigText's masked text and the ETag its etag; If-None-Match with
//   that etag is 304; a nil text is 500 internal and the log holds no secret
func TestOpenAPIMatchesSpec(t *testing.T) // the existing test, plus: no operation refers to a secrets parameter, no components
//   parameter is named secrets, and the document's text holds "secrets=1" only in the description of the 400

// internal/server, a real server on a temp file
func TestPutStreamKeepsSecrets(t *testing.T) // Review Focus 1. cam is created with rtsp://u:hunter2-pw@127.0.0.1:1/s, a second
//   input, a push rtmp://127.0.0.1:1/live/pushkey-0123456789 and srt_passphrase srtpass-0123456789.
//   GET (through api.New over the server) → body b, ETag e. PUT b with If-Match e: 200 "unchanged", the file's bytes and
//   mtime unchanged. PUT b with audio_language set: 200 "live"; the file holds the three real secrets and no "xxxxx";
//   Status.Rebuilds is unchanged; the answer's ETag equals a following GET's and works as the next If-Match
func TestPutStreamMaskMoved(t *testing.T) // Review Focus 2. PUT of b with the input's host changed to a listener the test owns:
//   422 invalid_config naming inputs[0].url; the file is unchanged; the listener accepted no connection within 500 ms.
//   PUT of b under another stream name: 422. PUT of b after the stored push was replaced by one that redacts alike plus the
//   original: 422 naming outputs.push
func TestPutStreamMaskRace(t *testing.T) // -race: 20 goroutines PUT the masked document while 20 PUT a new real password in
//   turn: every answer is 200 or 412, and after each the file's password is one of the real ones, never the mask
func TestPutConfigKeepsSecrets(t *testing.T) // Review Focus 1. GET /api/v1/config: no secret in it. PUT it back with If-Match:
//   200 with every stream "unchanged" and the file's bytes unchanged. PUT it with a stream added: the file is the old text plus
//   the entry, secrets real. PUT it with cam's masked URL moved to another host: 422 and the file unchanged
func TestPutConfigExactBytes(t *testing.T) // a PUT without any mask stores the client's exact bytes (the existing row, kept),
//   with odd indentation and blank lines
func TestReloadWithMask(t *testing.T) // the file edited by hand to hold "xxxxx" as cam's password: the reload fails with cam's
//   validation error, nothing changes, and the error text holds no stored secret
```

- [ ] **Step 2: Run** `go test -count=1 ./internal/api/ ./internal/server/ -run 'Redacted|Secrets|Masks|Masked|OpenAPI|KeepsSecrets|MaskMoved|MaskRace|ExactBytes|ReloadWithMask'`. Expect failures.
- [ ] **Step 3: Implement.** Rewrite the rows that used `?secrets=1` (`internal/api/streams_test.go`, `auth_test.go`, `api_test.go`, `config_test.go`) to the new contract; each rewritten row is listed in the task report with its old and new assertion.
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/api/ ./internal/server/ ./internal/config/ ./internal/spec/`. Expect a pass.
- [ ] **Step 5: Commit** `feat(api): secrets are write-only: every response is masked and the mask keeps the stored secret`

---

### Task 7: `server`, the SRT passphrase wired and counted

**Files:**
- Modify: `internal/server/server.go` (`startSRT` at 705–722), `internal/server/collect.go` (the srt output's `rejected` list at 100–105), `internal/server/doc.go`
- Test: `internal/server/server_test.go`

**Interfaces:**
- Consumes: `srtout.Options.Passphrase`, `Stats.RejectedSecret`, `Stats.RejectedLimited` (Task 3); `Manager.SRTPassphrase` (Task 5).
- Produces: `startSRT` passes a `Passphrase` that asks the manager in force, returning "" while there is none, as `lookup` does (`server.go:383-391`). `/metrics` gains `openstreamer_output_rejected_total{output="srt",reason="secret"}` and `reason="limited"`.

- [ ] **Step 1: Write the failing tests.**

```go
func TestSRTPassphrase(t *testing.T) // a server with srt.listen 127.0.0.1:0 and two running streams, open and locked
//   (srt_passphrase srtpass-0123456789), fed by the package's test source:
//   a gosrt caller with the passphrase reads TS from live/locked; without it and with a wrong one it is refused, and
//   /metrics shows reason="secret" at 2; live/open plays without one and refuses an encrypted caller as before
//   PutStream changes locked's passphrase while a caller reads: its Read ends within 2 s, srt's ended{reason="hub_closed"}
//   moved by 1, a caller with the new passphrase reads and one with the old is refused; Rebuilds did not move
//   PutStream removes the passphrase: a plain caller reads
//   no log line and no /metrics line holds either passphrase
```

- [ ] **Step 2: Run** `go test -count=1 -run TestSRTPassphrase ./internal/server/`. Expect a failure.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/server/`. Expect a pass.
- [ ] **Step 5: Commit** `feat(server): an SRT output's passphrase is asked of its stream`

---

### Task 8: The M5c2a scenarios, docs, CI and the gate

**Files:**
- Create: `internal/e2e/m5c2a_test.go`
- Modify: `internal/e2e/m5a_test.go`, `internal/e2e/m5c1_test.go` (the rows that used `?secrets=1` or compared `GET /api/v1/config` with the file)
- Modify: `internal/testpeer/testpeer.go` (the comment of `FFmpegSRT` at 376–379, which says the image carries no ffmpeg)
- Modify: `.github/workflows/ci.yml` (`E2E_TESTS` at 231), `docs/REBUILD_PLAN.md`, `README.md`, `SECURITY.md`, `cmd/openstreamer/openstreamer.example.yaml`, `CLAUDE.md`

**Interfaces:**
- Consumes: everything above, as an API client and a peer driver.

- [ ] **Step 1: Write the tests.** Build on M5a's harness (`m5aServer`, `m5aSpec`, `m5a.put`, `m5a.get`, `m5aSource`). Each test is `t.Parallel()`. `m5c2aSecrets` lists every literal the scans look for: `hunter2-pw`, `pushkey-0123456789`, `listenkey-0123456789`, `srtpass-0123456789`, `outpass-0123456789`, `tlJwpbo6`, `hunter2-q`, `abc123tok`, `pw12345`.

```go
// TestM5c2aSRTFFmpeg: the exit criterion's SRT half, ffmpeg only (it runs in the arm64 job). A passthrough stream cam with
// srt on and srt_passphrase outpass-0123456789, and a stream open with srt on and none.
//   ffprobe of srt://<SRT>?streamid=live/cam&passphrase=outpass-0123456789 lists h264 and aac; ffmpeg -f null decodes 5 s of both
//   with no "corrupt" in its log; the same with -pbkeylen 32.
//   Without the passphrase ffmpeg fails, and its log holds "UNSECURE" or "Password required"; with a wrong one, "BADSECRET" or
//   "Incorrect passphrase". Neither read a packet (srt's sessions gauge did not move), and reason="secret" moved by 2.
//   conform.ReadTS of the same URL with the passphrase: no violation (our own client).
//   open plays without a passphrase, and ffmpeg with one is refused, as at HEAD.
//   While ffmpeg plays cam, a PUT changes the passphrase: ffmpeg ends within 5 s; a new ffmpeg with the new passphrase plays;
//   cam's rebuilds did not move and its HTTP-TS reader was not interrupted (assertUninterrupted).
// Budget 90 s.
func TestM5c2aSRTFFmpeg(t *testing.T)

// TestM5c2aWriteOnly: Review Focus 1 and 2 over the real API, as the editor.
//   cam is created with an RTSP input with a password (nothing listens), the M5a source as its second input, an RTMP push with a
//   key and the SRT passphrase. GET shows masks for the editor and the viewer alike; GET ?secrets=1 is 400 for both.
//   The editor GETs cam, sets audio_language and PUTs the body with the ETag: 200 "live"; the config file holds the real
//   secrets; cam's active input, generation and switches are what they were; the HTTP-TS reader was not interrupted.
//   The editor GETs /api/v1/config (no secret in it), appends a stream to the text and PUTs it: the file is the old file plus
//   the entry, byte for byte.
//   A PUT of cam with the masked input's host changed to a listener of the test: 422, and the listener saw no connection.
//   A PUT of the masked body under a new name: 422.
// Budget 60 s.
func TestM5c2aWriteOnly(t *testing.T)

// TestM5c2aNoSecretsLeak: TestM5c1NoSecretsLeak's scan, extended. Streams whose first inputs fail and so reach errors and logs:
//   rtsp://127.0.0.1:1/user=admin_password=tlJwpbo6_channel=1_stream=0.sdp?real_stream, rtsp://127.0.0.1:1/x?user=admin&password=hunter2-q,
//   http://127.0.0.1:1/live/token=abc123tok/index.m3u8, srt://127.0.0.1:1?streamid=publish:cam:user:pw12345, and M5a's four
//   forms; each has the M5a source second; one has srt_passphrase outpass-0123456789, and a wrong-passphrase caller and a
//   caller without one were refused.
//   Scanned for every literal of m5c2aSecrets: the log; /metrics; as the viewer AND as the editor every GET route, GET /api/v1/config
//   included, every stream and status document, the config status; the answers to a PUT of a taken listen input, of a URL that
//   cannot be an input, and of an unresolvable mask. The editor's GET /api/v1/config also holds no user password, and holds the
//   users' hashes (R-T8: recorded, not hidden).
func TestM5c2aNoSecretsLeak(t *testing.T)

// TestM5c2aPlayers (amd64 test job only: VLC and GStreamer).
//   †GStreamer plays srt://… with srtsrc uri=<url without the passphrase> passphrase=outpass-0123456789 ! tsdemux, by gstPlays' rule.
//   †VLC plays the same with --srt-passphrase=outpass-0123456789 (and --srt-streamid, as the M3b3 VLC row passes the stream ID),
//   by vlcPlays' rule.
func TestM5c2aPlayers(t *testing.T)
```

- [ ] **Step 2: Run** `go test -race -count=1 -run '^TestM5c2a' ./internal/e2e/` on the host. Expect a pass within each budget.
  - Then `-count=5` must pass 5 of 5.
  - Then `make test-ci-peers ARGS='-run ^TestM5c2a ./internal/e2e/'` must pass.
  - If anything fails, use superpowers:systematic-debugging. A code fix gets a test in the package that owns the code. A † row that a peer provably cannot do is dropped and recorded as a known limit, with the peer's own error or missing option; never weaken a row the peer can do.
- [ ] **Step 3: CI.** Add `TestM5c2aSRTFFmpeg` to `E2E_TESTS` of `transcode-e2e-arm64`: that job installs Ubuntu 24.04's ffmpeg, which is the build the peers image carries and has libsrt. If its first run fails in `testpeer.Require(t, "ffmpeg", "srt")`, take the name out of the arm64 list again and record the runner's `ffmpeg -protocols` line as a known limit. The other M5c2a tests run in the amd64 `test` job with the rest of `./internal/e2e/`.
- [ ] **Step 4: Docs.**
  - **`docs/REBUILD_PLAN.md`:**
    - §5: replace the M5c2 row with two. **M5c2a** "Secrets: credentials in URL paths, queries and stream IDs redacted; secrets write-only over the API (every response masked, the mask keeps the stored secret, the config editor included); SRT output encryption with a per-stream passphrase", depends on M5c1, with the plan link, "done", R-T1's exit criterion and "Proved by" the four tests plus the redact, logging, spec, config, srtout, conform, stream, api and server suites. **M5c2b** "SRTP for RTSP: `pkg/srtp`, `RTP/SAVP` with SDES keys inside RTSPS, server and pull client", depends on M5c1, not yet planned, with R-T1's proposed exit criterion.
    - After the M5c1 paragraphs (near 1293–1384): R-T1's text, the rulings R-T2 to R-T11 by reference to this plan, the "Out of M5c2a" table, the peers table, and this plan's "Hand-over: M5c2b" facts in place of the paragraph at 1378–1384.
    - §3.6: the Users bullet (633–647) loses `?secrets=1` from what an editor gets; the API bullet (710–725) says every response is redacted for every role, that a write carrying the mask keeps the stored secret by R-T4's rule, and that `GET /api/v1/config` is masked while `PUT` resolves masks and stores the resolved bytes. The SRT output bullet (near 314) gains the passphrase and its three refusals.
    - The "Out of" rows near 936 (SRT encryption), 1234, 1367–1369 and 1476: done in M5c2a. The SRTP rows near 899, 1366 and 1472 now say M5c2b.
    - Backlog: the backlog rows of "Out of M5c2a"; the cost lines of R-T2 (names the rule does not know), R-T8 (a secret in a comment is masked for good by a round trip; hashes are readable by editors) and R-T9 (the refusals tell that a name is live).
    - Add an "M5c2a deferred minors" list built from the task reviews, and any dropped † row under known limits of the peers.
  - **README:** a "Secrets" section after "TLS": what is hidden in a URL (R-T2, with the two Xiongmai forms as examples and the note that the rule over-masks); that no API response shows a secret to anyone, `?secrets=1` is gone, and `xxxxx` sent back keeps the stored value while any change around it needs the secret again; that the config file holds the secrets in clear and is refused unless private; that a secret in a YAML comment is masked by the editor's round trip. In the SRT output's section: `outputs.srt_passphrase` (10 to 79 printable ASCII bytes), the player URLs for ffmpeg, GStreamer and VLC, the two refusal messages a player sees, and that changing it disconnects the stream's SRT players.
  - **SECURITY.md:** one paragraph on where secrets are stored and who can read them (R-T7, R-T8's hashes).
  - **Example config:** a commented `srt_passphrase` under an output, with one line of explanation.
  - **CLAUDE.md:** the API note gains "every response is masked; the mask written back keeps the stored secret (`spec.Stream.Unmask`, `config.Unmask`)", and the note on adding a spec field gains "a secret field must be masked in `Redacted` and resolved in `Unmask` and `config.Unmask`". Keep the length within five lines of the current one.
- [ ] **Step 5: The gate.**
  - `make check`, `make covercheck` on a fresh `make cover`, `make test-pinned`, `make test-ci-peers`, `make test-transcode`.
  - `GOOS=windows go vet`, `GOOS=darwin go vet` and `GOOS=linux go vet` (untagged packages).
  - `go mod tidy`: no change.
  - `git diff 6e8edb1 --stat -- pkg/rtsp internal/output/rtspplay internal/ingest/rtsp.go` is empty (the frozen RTSP side).
- [ ] **Step 6: Commit** `test(e2e): the M5c2a scenarios` and `docs: M5c2a is done`.

---

## Hand-over: M5c2b

M5c2b starts from these facts. The first group was measured on 2026-11-27 with a scripted RTSP server that answers `DESCRIBE` with an SDP carrying `a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:<key>` and relays SRTP made by `ffmpeg -f rtp -srtp_out_suite AES_CM_128_HMAC_SHA1_80 -srtp_out_params <key> srtp://…`.

| Peer | Checked | Result |
|---|---|---|
| ffmpeg 8 and 6.1 | `m=video … RTP/SAVP` with `a=crypto`, `-rtsp_transport udp` | Plays (51 frames in 2 s). Its `SETUP` still offers `RTP/AVP/UDP;unicast;client_port=…`, never `RTP/SAVP` |
| ffmpeg 8 and 6.1 | the same, `-rtsp_transport tcp` | Plays: it decrypts SRTP on the interleaved channel too, offering `RTP/AVP/TCP` |
| ffmpeg 8 | `m=video … RTP/AVP` with `a=crypto` | Plays: the `a=crypto` line alone turns decryption on. ffmpeg's own SDP writer puts `a=crypto` under `RTP/AVP` |
| ffmpeg 8 | `RTP/SAVP` without `a=crypto`, the packets still SRTP | No frame decodes: the rows above are real decryption |
| ffmpeg 6.1 and 8 | `-h protocol=srtp` | `srtp_in_suite`, `srtp_in_params`, `srtp_out_suite`, `srtp_out_params`: ffmpeg is an SRTP packet source and sink for `pkg/srtp`'s own interop rows, without RTSP |
| GStreamer 1.24 | `RTP/SAVP` with `a=crypto` | Sends `SETUP` with `Transport: RTP/SAVP;…` and a `KeyMgmt: prot=mikey;…` header, ignores the SDES key, and never delivers a buffer. It speaks MIKEY only |

What follows from them, as proposals for M5c2b's own rulings:
- **The server must accept `RTP/AVP` and `RTP/AVP/TCP` in `SETUP` for a session whose SDP said `RTP/SAVP`**, or ffmpeg cannot play. The profile in `SETUP` cannot be what selects SRTP; the SDP the session was described with is.
- **An SDP with `a=crypto` makes ffmpeg decrypt on TCP too**, so one RTSPS listener cannot serve SRTP to UDP players and plain RTP to interleaved ones from the same SDP. Either every session of an SRTP-enabled RTSPS listener carries SRTP (interleaved included), or SRTP is chosen per request.
- **GStreamer cannot play an SDES SDP at all**, so turning SRTP on for the RTSPS listener takes M5c1's GStreamer RTSPS row away. That argues for an opt-in (`server.rtsp.srtp`, off by default, so M5c1's behaviour stays frozen) and for MIKEY as a named backlog item.
- **No peer serves SDES over RTSP** (ffmpeg has no RTSP server with it; MediaMTX and GStreamer use MIKEY), so the pull client is proved against our own server, `rtsptest`, and ffmpeg's `srtp://` output as a packet source.
- ffmpeg's receiver reports were not inspected. Whether an SRTP session must accept plain RTCP from its player is M5c2b's to measure; RTSP keep-alives hold an ffmpeg session open either way.
- From M5c1, unchanged: an RTSPS session refuses UDP with 461 (`pkg/rtsp/server.go:922-930`), which is the branch SRTP opens; `rtsps://` with `transport=udp` is refused at parse in the pull client.
