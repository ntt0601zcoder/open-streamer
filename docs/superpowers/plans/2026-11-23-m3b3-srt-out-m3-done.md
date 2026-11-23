# M3b3 SRT Output, Rendition Alignment and M3 Completion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve every stream as MPEG-TS over SRT, keep HLS renditions aligned when a track is absent for a stretch (R4/R9), prove every output with a VLC-class client, MediaMTX where it can pull, and the M3 row's four scenarios, and close M3.

**Architecture:** The per-subscriber MPEG-TS streaming loop of `internal/output/httpts` moves unchanged to a shared `internal/output/tsstream`, which then gains resync after hub loss (a discontinuity indicator and a restart at the next keyframe) for both users. `internal/output/srtout` serves it over gosrt: the SRT stream ID selects the stream (request mode only), a reader goroutine per connection notices a closed or idle peer, and a write wrapper turns gosrt's non-blocking full-queue `io.EOF` into bounded, paced retries so bursts pass, and a stats poller ends stalled receivers. The CMAF HLS per-track playlists list one entry per segment for every track — the real part or an `EXT-X-GAP` placeholder — so sequence numbers and discontinuity counts are equal across renditions by construction. `internal/conform` reads `srt://`. An e2e matrix checks every output under the four scenarios; VLC 3 or GStreamer (chosen per output by a probe) and MediaMTX pull our outputs.

**Tech Stack:** Go (`go 1.26.11`); `datarhei/gosrt` v0.13.0 (approved, already used by the SRT inputs); our `pkg/hls`, `pkg/mpegts`, `internal/ts`; standard library; `stretchr/testify`. Test peers: ffmpeg/ffprobe with libsrt (CI apt 6.1), VLC 3 and GStreamer 1.24 (CI apt; on the dev host when the user has installed them, else in a throwaway container built once with a non-root user), MediaMTX 1.21.3 (the host-network RTSP container, which can pull HLS and SRT sources).

**Spec:** [docs/REBUILD_PLAN.md](../../REBUILD_PLAN.md) §2 (Outputs), §3.3, §3.4, §3.7 (gosrt), the M3 row and the M3b3 row of §5; M3a ledger rulings R4 and R9 (a track absent for a stretch makes per-rendition discontinuity counts diverge; fix with EXT-X-GAP or per-track placeholders before M3 is called done) and R7 (loss-merged discontinuities). References: draft-sharabayko-srt (SRT, access-control stream ID), RFC 8216 and 8216bis (HLS, `EXT-X-GAP` §4.4.4.7, playlist updates §6.2.1), ISO/IEC 13818-1 — never another product's code.

**M3b3 exit criteria (and M3 done):** the M3 row: ffmpeg and VLC-class clients play every output (HLS fMP4, HLS TS, DASH, HTTP-TS, SRT, RTMP play, RTSP play; the RTMP push-out is played back from its target); MediaMTX pulls every output it can take as a source (HLS fMP4, HLS TS, SRT, RTMP, RTSP — DASH and HTTP-TS are not MediaMTX sources, ruled at dispatch); for every output `osconform` passes the steady-state, epoch-change, slow-consumer and B-frame scenarios and ffprobe reads it; CMAF HLS renditions keep equal media sequence numbers and discontinuity counts at every refresh, including across a stretch where a track is absent and across window eviction.

**Out of M3b3, with reasons** (ledger rulings at dispatch): SRT caller push-out (add on demand; RTMP covers push today); SRT encryption (credentials are M5; encrypted requests are rejected); SRT publish into the output listener (the SRT inputs own publishing); DASH placeholders (periods carry their adaptation sets; nothing to align); synthesised placeholder media (silent AAC) instead of gaps (Backlog if hls.js stalls are reported); LL-HLS changes.

## Global Constraints

- Every Global Constraint of the M1, M3a, M2a, M2b, M2c, M3b1 and M3b2 plans still holds (independent libraries; `pkg/...` never imports `internal/...`; errors `pkg: op: %w`; never log and return the same error; every goroutine has a cancel path; hub writes never block — outputs read through `hub.Sub` only; parameter sets only in `Config`; peer tests via `internal/testpeer`; tests and CI never bind 554/1935/8554 nor touch other containers — a v1 soak runs on the dev machine).
- **Shared TS streaming (`internal/output/tsstream`):** first a byte-identical extraction of HTTP-TS's current loop (its subscription call, prebuffer of one second of stream time, tracks that start together, late tracks with fresh PAT/PMT, codec change of a configured track ends, write deadline before each write, flush after each write, track `i` on PID `256 + i`); then one new behaviour for both users: when a delivery has `Lost > 0`, the muxer drops frames until the next video keyframe (audio-only: next frame), writes PAT/PMT, sets the `discontinuity_indicator` in the adaptation field of the first packet of every PID after the gap, and drops audio whose PTS is below the resume keyframe's (counted); HTTP-TS subscribes with `hub.SubscribeCached(name, 1024)` like the other streaming outputs (a recorded behaviour change only for caches longer than the queue, which `Subscribe` would not replay).
- **SRT output (`internal/output/srtout`):** one gosrt listener per address (osdev `-srt-play <addr>`, refused when it equals an SRT listen input's address after normalising host and port, so `:9000` equals `0.0.0.0:9000`); `Accept2` decides from the connection request: the stream ID is either `#!::r=<app>/<name>[,m=request][,t=stream]` (access-control form; no `m` means request; `t` other than `stream` rejected; unknown keys ignored) or the plain `<app>/<name>` (app from `-srt-play-app`, default `live`); empty stream ID (version-4 handshake), `m=publish` or any mode other than request, encrypted requests, unknown names, streams that are not live and every cap are rejected with the same reason (`srt.REJ_RESOURCE` via gosrt's `Reject`) and no detail (a caller that retransmits its request after a reject may be counted twice; documented); latency `120 ms` default (`-srt-latency`); `PeerIdleTimeout` from gosrt's default (5 s; shortened in tests); each accepted connection has a reader goroutine whose `Read` returning (peer shutdown, idle timeout, close) cancels the connection's context (it discards any payload a client sends); the listener config sets `PayloadSize = 1316` (7 TS packets) and the write wrapper calls gosrt's `Write` with at most 1316 bytes at a time (never an empty slice), so a chunk is queued whole or not at all; `io.EOF` from `Write` while the connection's context is not done (gosrt has no closed flag; its `io.EOF` also means a full send queue) means the queue is full — the same chunk is retried every 5 ms (burst handling, counted `send_queue_full`), and a retry loop longer than `StallTimeout` ends the connection; the wrapper also paces writes to at most 256 packets per 5 ms so a GOP replay or a large keyframe does not arrive as one TSBPD burst that overflows a gosrt receiver's 1024-packet read queue (the pause only slows the loop; hub skips are allowed); **stalled** is decided from `conn.Stats`, polled every 500 ms: over the last `StallTimeout` (default 5 s), sender drops (`Accumulated.PktSendDrop`) above 5 % of the packets sent (gosrt's accumulated sent-packet counter over the same window), or `Accumulated.PktRecvACK` not advancing while data is sent → end as `EndedStalled` (a receiver whose application stops reading keeps ACKing and its own gosrt drops the data: undetectable here and harmless to the server; documented); bounds: connections ≤ 256 per stream, 1024 per listener, 64 per IP (IPv6 by /64); queue `hub.SubscribeCached(name, 1024)`.
- **HLS rendition alignment (R4/R9, R7):** every CMAF per-track media playlist (`cmaf/v{track}.m3u8`, `cmaf/a{track}.m3u8`) lists one entry per segment in the window (the segmenter's `Seq`), in order: the track's part for that segment, or a gap entry (`#EXT-X-GAP`, `EXTINF` = that segment's duration, the segment's `Discontinuity` flag, URI `gap-{track}-{seq}.m4s` which the server already answers 404, the previous entry's `EXT-X-MAP` kept so no map points at a missing init); `EXT-X-MEDIA-SEQUENCE` = the first listed segment's `Seq`; discontinuity flags follow the segmenter's per-segment flags (so `EvictedTrackDiscontinuities` becomes one `EvictedDiscontinuities` shared by all tracks); `EXT-X-VERSION` stays 7 (no version is required for `EXT-X-GAP`); the packager keeps a known-tracks set (every track that has had a part since the stream started, kept until the stream ends) with each track's last `EpochTrack` (codec string, config, init bytes); both the master and the track playlists use it, so a rendition whose window holds only gaps stays in the master — with its retained codec string in CODECS (and RESOLUTION for video) when the window has no part of it — and its playlist is served (never 404); leading gap entries (before a track's first real part in the window) use the first real entry's `EXT-X-MAP`; a playlist with no real entry in the window writes no `EXT-X-MAP` (gap entries are never loaded), so no map ever points at a missing init; gap entries carry `EXT-X-PROGRAM-DATE-TIME` from the segment's start on the first entry and at discontinuities, like real ones; no listed entry ever changes between refreshes except removal from the front (RFC 8216 §6.2.1); `EXT-X-TARGETDURATION` is computed the same way as the HLS-TS playlist's. The muxed HLS-TS playlist and DASH are unchanged.
- **Slow consumer, per output:** segment outputs (HLS fMP4, HLS TS, DASH) — a slow packager subscriber (`TestM3aSlowPackager*` pattern); streaming outputs (HTTP-TS, SRT, RTMP play, RTSP play, RTMP push-out) — hub loss from a slow writer: resume at a keyframe with headers, no delta after a gap.
- **VLC-class client:** the client is chosen per output by Task 7's probe: VLC 3 (`cvlc` as a non-root user, `-I rc --rc-fake-tty --stats --vout dummy --aout dummy`) where it decodes both kinds of that output, else GStreamer 1.24 (`gst-launch-1.0 -e`, decoders from `gstreamer1.0-libav`, stopped with `timeout -s INT`). RTSP (and the push-out played back over RTSP) uses GStreamer `rtspsrc` (Ubuntu's VLC has no live555 module). Pass rule: VLC — with those options and `--sout '#duplicate{dst=display,dst=std{access=file,mux=ts,dst=<f>}}'`; after 6 s write `stats` to its stdin and parse the decoded video and audio block counts (both > 0), then `quit`; the log shows the input opened and no `decoder error`/`cannot` lines; the dump has a video and an audio stream of at least 4 s (ffprobe); if the probe shows `rc` stats unusable on a VLC build, the probe records the replacement method as a ruling. GStreamer — `fakesink name=v silent=false` / `name=a silent=false` with `gst-launch-1.0 -v -e`, count `last-message` lines per sink (both > 0 within 10 s), no `ERROR` message. The exit code alone is never a pass.
- **Local peers:** VLC/GStreamer run on the host when present (the user may install them), else in a throwaway image built once from a tiny Dockerfile in `testdata/peers/` (`ubuntu:24.04`, the apt packages, a non-root user) and run with `docker run --rm --network host --name osv2-vlc-…`; MediaMTX only as the M2c host-network throwaway (18554/18000/18001, `MTX_MOQ=no`, all other servers off, verified with `ss`).

## Review Focus

1. **SRT viewers that vanish, stall, lose packets or join on a burst** (receiver closes its socket without SRT shutdown during a source outage; a link below the stream bitrate; 2 % loss through a lossy UDP proxy; a join on a GOP replay with a > 2 MB keyframe). Expected: vanished viewers are ended within the idle timeout even while the hub is quiet and their cap slots freed; stalled viewers ended after `StallTimeout` without slowing others or the hub; loss repaired by SRT retransmission (clean TS); bursts delivered without ending the viewer. Tests: `TestSRTViewerGoneDuringOutage`, `TestSRTStalledReceiverEnded`, `TestSRTLossyLink`, `TestSRTBurstJoin`, `TestSRTWriterRetryExact` (Task 4).
2. **Stream IDs and access** (both forms, `m=publish`, `t=file`, empty ID, encrypted request, unknown names, caps). Expected: only request mode, identical rejects. Tests: `TestSRTStreamID`, `TestSRTRejects`, `TestSRTCaps` (Task 4).
3. **HLS renditions across an audio-less stretch and eviction** (failover A/V → video-only → A/V; the window sliding past the gaps; playlists refreshed throughout; ffmpeg and the HLS fMP4 VLC-class client reading across the gap). Expected: equal media sequence and discontinuity counts in every rendition at every refresh, no listed entry ever changes, gap URIs 404, players continue across the gap. Tests: `TestHLSPerSegmentEntries`, `TestHLSGapEviction`, `TestHLSGapOnlyPlaylistServed`, `TestHLSFFmpegAcrossGap`, `TestM3VLCClass/hls-fmp4-across-gap`, `TestM3b3HLSAlignment` (Tasks 5, 7).
4. **Every output under every scenario** with `osconform` and ffprobe, slow consumer defined per output. Test: `TestM3Matrix` (Task 7).
5. **Hub loss on the TS streaming outputs** (HTTP-TS and SRT behind a slow writer). Expected: discontinuity indicator, PAT/PMT, keyframe first, early audio dropped. Tests: `TestStreamResyncAfterLoss` (Task 3), the HTTP-TS slow-consumer matrix cell (Task 7).

---

### Task 1: M3b2 carry-overs in `pkg/rtsp` and conform

**Files:** Modify `pkg/rtsp/server.go`, `pkg/rtsp/server_session.go`, `pkg/rtsp/message.go`, `internal/conform/rtsp.go`, `internal/conform/read.go`; Test the matching `_test.go` files

(a) the UDP BYE uses the same `now + WriteTimeout` deadline as every other UDP write, not a short one on the shared RTCP socket; (b) once a connection plays, its request reader uses a 4 KiB buffer for request header bytes (interleaved frame lengths are bounded separately as today); the 64 KiB header limit stays before PLAY; (c) `ParseTransports` skips a malformed offer when at least one offer parses (461 only when none parses); (d) `conform.Capture` gains `ByeTracks int` (tracks that sent an RTCP BYE).

- [ ] **Step 1: Write the failing tests:** `TestServerUDPByeDeadline`, `TestServerPlayingReaderSmall`, `TestParseTransportsSkipsMalformed`, `TestReadRTSPCountsBye`.
- [ ] **Step 2–4:** red, implement, green (`go test -race ./pkg/rtsp/ ./internal/conform/`).
- [ ] **Step 5: Commit** `fix(rtsp,conform): settle the items carried from M3b2`

### Task 2: Extract the TS streaming loop (`internal/output/tsstream`)

**Files:** Create `internal/output/tsstream/tsstream.go`, `internal/output/httpts/testdata/golden_*.ts`; Modify `internal/output/httpts/httpts.go`; Test `internal/output/tsstream/tsstream_test.go` (existing `internal/output/httpts` tests unchanged and passing)

**Interfaces — produces:**

```go
type Sink interface {
    io.Writer
    SetWriteDeadline(time.Time) error // may be a no-op
    Flush() error                     // may be a no-op; http.ErrNotSupported is ignored by HTTP-TS's adapter
}
type Options struct{ Queue int; Prebuffer int64 /* 90 kHz, default 90000 */; WriteTimeout time.Duration }
type Stats struct{ Frames, LateTracks uint64; Ended string }
// Stream subscribes to h and writes MPEG-TS to s until ctx ends, the hub closes, a configured track's codec
// changes, or a write fails.
func Stream(ctx context.Context, h *hub.Hub, name string, s Sink, opt Options) (Stats, error)
```

- [ ] **Step 1: Capture golden output before touching code:** run the current HTTP-TS handler on `h264_aac.ts`, `h265_aac51.ts` and `h264_aac_then_640.ts` (deterministic fake clock) and commit the bytes as `golden_*.ts`; `TestHTTPTSGolden` asserts the handler reproduces them.
- [ ] **Step 2:** move the loop into `tsstream.Stream` with no behaviour change; HTTP-TS calls it through an adapter (response writer, `ResponseController` deadline and flush); `TestHTTPTSGolden` still passes byte for byte; `TestStreamSink` covers deadlines and flush calls.
- [ ] **Step 3: Commit** `refactor(output): extract the TS streaming loop for HTTP-TS and SRT`

### Task 3: Resync after hub loss in the TS streaming loop

**Files:** Modify `internal/output/tsstream/tsstream.go`, `internal/ts/mux.go` (discontinuity indicator on the next packet of each PID), `pkg/mpegts/writer.go` (adaptation-field `discontinuity_indicator` option), `internal/output/httpts/httpts.go` (`SubscribeCached`); Test `internal/output/tsstream/tsstream_test.go`, `pkg/mpegts/writer_test.go`, `internal/ts/mux_test.go`, `internal/output/httpts/httpts_test.go`; regenerate HTTP-TS golden files only if the cached-GOP start changes them (state why in the commit)

Rules per the Global Constraints (resync behaviour; `SubscribeCached`). Stats gain `Resyncs`, `DroppedBeforeKey`, `DroppedEarlyAudio`.

- [ ] **Step 1: Write the failing tests:** `TestStreamResyncAfterLoss` (an injected slow `Sink` and a small queue → after each gap: discontinuity indicator on the first packet of every PID, PAT/PMT, a keyframe first, early audio dropped; `conform.ReadTS`-style checks on the bytes clean), `TestWriterDiscontinuityIndicator`, `TestMuxDiscontinuityNextPacket`, `TestHTTPTSJoinsCachedGOP`.
- [ ] **Step 2–4:** red, implement, green; `go test -race ./internal/output/... ./internal/ts/ ./pkg/mpegts/ ./internal/e2e/`.
- [ ] **Step 5: Commit** `feat(output): resume TS streams at a keyframe after hub loss`

### Task 4: SRT output (`internal/output/srtout`)

**Files:** Create `internal/output/srtout/srtout.go`; Modify `cmd/osdev/main.go` (flags `-srt-play <addr>`, `-srt-play-app <app>` default `live`, `-srt-latency <dur>` default 120 ms; the address-collision check; usage text); Test `internal/output/srtout/srtout_test.go`, `internal/output/srtout/proxy_test.go` (a UDP proxy for loss, blackholing and throttling)

**Interfaces — produces:**

```go
type Options struct{ App string; Latency, StallTimeout, PeerIdleTimeout time.Duration; Queue int; MaxPerStream, MaxPerListener, MaxPerIP int }
type Stats struct{ Conns, Rejected, RejectedUnknown, RejectedMode, RejectedCaps, Frames, Resyncs, SendQueueFull, EndedStalled, EndedGone, EndedCodecChange, EndedHubClosed, EndedShutdown uint64 }
type Server struct{ /* lookup, gosrt listener, conns, stats */ }
func New(lookup func(name string) (*hub.Hub, bool), opt Options) *Server
func (s *Server) Listen(addr string) error // binds the gosrt listener (Addr valid after it returns)
func (s *Server) Addr() net.Addr
func (s *Server) Serve() error             // accepts until Close
func (s *Server) Close() error             // cancels every connection, waits
func (s *Server) Stats() Stats
```

Rules per the Global Constraints. Each connection: `tsstream.Stream` with a `Sink` that wraps the gosrt connection (no-op deadline and flush; the chunked, paced, retrying write); the reader goroutine; the stats poller; ctx cancelled on gone, stalled, `Close`. Stats gain `SendQueueFull`.

- [ ] **Step 1: Write the failing tests** (hub fed by a looped fixture through `clock.New`; receivers: a gosrt caller in the test, ffmpeg/ffprobe with libsrt via testpeer — the access-control ID percent-encoded in their URLs): `TestSRTPlay` (TS read back clean), `TestSRTStreamID`, `TestSRTRejects` (publish, `t=file`, empty, encrypted, unknown, not live → same reject seen by `Dial` as "connection rejected"), `TestSRTCaps`, `TestSRTJoinsAtKeyframe`, `TestSRTViewerGoneDuringOutage` (hub quiet; receiver socket closed without SRT shutdown → ended within the shortened idle timeout; cap slot freed), `TestSRTStalledReceiverEnded` (a throttling proxy holds the link below the stream bitrate → sender drops rise → ended as stalled after a shortened `StallTimeout`; a second receiver unaffected; hub never blocked), `TestSRTWriterRetryExact` (a fake connection copying gosrt's behaviour — the failing `Write` consumes the chunk and discards it — failing every Nth chunk with `io.EOF` → bytes received equal bytes sent, no duplicate or missing TS packet; the wrapper never passes more than 1316 bytes or an empty slice), `TestSRTLossyLink` (2 % loss proxy → TS clean, retransmissions counted), `TestSRTBurstJoin` (synthetic GOP with a > 2 MB keyframe replayed at join → delivered, viewer not ended, a gosrt receiver sees no continuity-counter error), `TestSRTCodecChangeEnds`, `TestSRTFFprobe` (H.264+AAC and H.265+AAC).
- [ ] **Step 2–4:** red, implement, green.
- [ ] **Step 5: Commit** `feat(output): serve streams over SRT`

### Task 5: HLS per-segment entries with gap placeholders (R4/R9, R7)

**Files:** Modify `pkg/hls/media.go` (`Segment.Gap bool` ↔ `#EXT-X-GAP`; parse and write), `internal/output/live/hls.go`, `internal/output/live/store.go` and `packager.go` (one `EvictedDiscontinuities`), `internal/conform/read.go` (`ReadHLS` accepts gap entries: never fetched, counted, duration equal to the segment's in the other renditions; equal discontinuity counts across renditions stay a rule (RFC MUST); equal media sequence numbers across renditions is checked only with `ReadOptions.AlignedSequences`, set for our outputs, since RFC 8216bis lets other servers number independently); Test `pkg/hls/media_test.go`, `internal/output/live/hls_test.go`, `internal/output/live/packager_test.go`, the HLS conform tests

Rules per the Global Constraints. Replace the "Known limitation (M3a)" comment with the new behaviour.

- [ ] **Step 1: Write the failing tests:** `TestHLSPerSegmentEntries` (epoch 1 A/V, epoch 2 video-only, epoch 3 A/V → `a{n}.m3u8` lists a gap entry for each epoch-2 segment with that segment's duration and flags; every rendition has the same MEDIA-SEQUENCE and discontinuity count; each URI keeps its sequence number on every refresh; no listed entry changes), `TestHLSGapEviction` (window slides past the gaps → counts and sequences equal at every step; DISCONTINUITY-SEQUENCE never decreases), `TestHLSLossMergedDiscontinuities` (R7: two discontinuities merged by loss in one rendition → counts still equal), `TestHLSGapOnlyPlaylistServed` (the master still lists the rendition with the audio codec in CODECS; its playlist is served; it has no `EXT-X-MAP`; every `EXT-X-MAP` URI in any playlist answers 200), `TestHLSGapKeepsMap` (incl. leading gaps), `TestHLSGapProgramDateTime`, `TestHLSTargetDurationLikeTS`, `TestParseGap`, `TestReadHLSGap`.
- [ ] **Step 2–4:** red, implement, green.
- [ ] **Step 5: Commit** `feat(hls): list every segment in every rendition with gap placeholders`

### Task 6: `osconform` for SRT

**Files:** Modify `internal/conform/read.go` (`ReadTS` accepts `srt://`: a gosrt caller, stream ID from the URL's `streamid` query; a reader goroutine copies the connection into a bounded buffer so demuxing never stalls SRT delivery; a full buffer is reported as a "reader overflow" error, never a silent block), `cmd/osconform/main.go`; Test `internal/conform/srt_test.go`, `cmd/osconform/main_test.go`

- [ ] **Step 1: Write the failing tests:** `TestReadSRTClean` (the SRT output of `h264_aac.ts` and `h265_aac51.ts` → no violations), `TestReadSRTBurst` (the burst-join stream from Task 4 → no false CC errors), `TestReadSRTViolations` (a scripted gosrt listener: missing PAT, backward PTS, delta first → each reported), `TestReadSRTReject` (rejected stream ID → error naming the rejection, no hang), `TestOsconformSRT`.
- [ ] **Step 2–4:** red, implement, green.
- [ ] **Step 5: Commit** `feat(conform): read and check SRT outputs`

### Task 7: The M3 scenario matrix, VLC-class clients and MediaMTX pulls

**Files:** Create `internal/e2e/m3b3_test.go`, `internal/e2e/vlc_test.go`, `testdata/peers/Dockerfile.vlc`; Modify `internal/testpeer/testpeer.go` (`VLC(t)`, `GStreamer(t)`, `FFmpegSRT(t)` helpers: host binary or the throwaway image when `OS_PEER_IMAGE` is set; skip or fail under `OS_REQUIRE_PEERS=1`), `internal/e2e/m3a_test.go` (only if `TestM3aFFprobe` must tolerate gap 404 warnings — ruled with evidence), `.github/workflows/ci.yml` (apt packages for VLC, GStreamer incl. `gstreamer1.0-libav`, in both jobs; MediaMTX path envs for HLS and SRT sources in both jobs)

- [ ] **Step 1: Probe and audit.** (a) In the throwaway image and on CI's `ubuntu-latest` (record versions): for each output, whether VLC 3 decodes both kinds, and whether its `rc` `stats` command gives decoded counts; whether MediaMTX pulls an HLS source with `MTX_HLS=no`; the ledger ruling names the client per output (VLC, or GStreamer; RTSP is GStreamer). (b) For every output and scenario, the existing test that proves it with `osconform` and ffprobe, or "missing"; the table goes in the report; slow consumer per the Global Constraints.
- [ ] **Step 2: Write the scenarios:**
  - `TestM3Matrix/<output>/<scenario>` for every missing cell (at least B-frames with `h264_pyramid.ts` for HLS fMP4, HLS TS, DASH and HTTP-TS; slow consumer for HTTP-TS; all four for SRT).
  - `TestHLSFFmpegAcrossGap` (ffmpeg reads `cmaf/master.m3u8` across a video-only stretch: exits 0, both streams decoded before and after; warnings recorded).
  - `TestM3b3HLSAlignment` (failover A/V → `h264_640.ts` → A/V through the live packager; every refresh has equal sequences and counts across renditions; `conform.ReadHLS` clean).
  - `TestM3b3MediaMTXPull`: MediaMTX pulls our HLS fMP4 (`MTX_PATHS_M3B3HLS_SOURCE=http://127.0.0.1:19364/live/m3b3/cmaf/master.m3u8`), HLS TS (`…M3B3HLSTS_SOURCE=…/hls/index.m3u8`) and SRT (`…M3B3SRT_SOURCE=srt://127.0.0.1:19363?streamid=live/m3b3srt`), all `…_SOURCEONDEMAND=yes`; our RTSP pull of each MediaMTX path → violation-free frames in one epoch. Fixed private ports 19363 (SRT) and 19364 (HTTP). Ruling recorded: DASH and HTTP-TS are not MediaMTX sources.
  - `TestM3b3SRTFFmpeg`: ffmpeg with libsrt (independent of gosrt) reads our SRT output and decodes with `-xerror -v error` (exit 0; any stderr line allowed is recorded as a ruling with the run's evidence).
  - `TestM3VLCClass`: one subtest per output (HLS fMP4, HLS TS, DASH, HTTP-TS, SRT with the plain stream ID, RTMP play, RTSP play over TCP; the push-out read back from MediaMTX's RTSP) plus `hls-fmp4-across-gap` (a video-only stretch in the middle; `stats` sampled before the gap and after it, the audio decoded count must grow after the gap), with the client the probe chose and the pass rule from the Global Constraints.
- [ ] **Step 3: Run** locally (peers on the host or in the throwaway image; the MediaMTX throwaway per the Global Constraints; stop every container afterwards).
- [ ] **Step 4: Commit** `test(e2e): cover every output under the M3 scenarios and with VLC-class clients`

### Task 8: Spec update — M3b3 and M3 done

**Files:** Modify `docs/REBUILD_PLAN.md`

- [ ] **Step 1:** M3b3 done in §5's M3b table with this plan's link, its exit criteria and "Out of M3b3"; the M3 row marked done with the evidence (the matrix test, the VLC-class test, the MediaMTX pulls); §4 layout gains `internal/output/tsstream` and `internal/output/srtout`; §3.4 notes per-segment HLS entries with gap placeholders (R4/R9/R7 closed), the shared TS streaming core and its resync; the HTTP-TS cached-GOP start; the Backlog gains "silent AAC placeholder parts instead of gaps, if hls.js stalls are reported".
- [ ] **Step 2: Run** the full suite, `go vet`, gofumpt, golangci-lint, `make cover covercheck` — clean.
- [ ] **Step 3: Commit** `docs: mark M3b3 and M3 done`
