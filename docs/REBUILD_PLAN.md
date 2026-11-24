# Rebuild Plan: Open Streamer v2

**Status:** approved direction, design proposed. Each milestone gets its own
task-level plan under `docs/superpowers/plans/` when it starts. M1 is written:
[2026-11-10-m1-media-core.md](superpowers/plans/2026-11-10-m1-media-core.md).

**Decision:** v2 is a new codebase. No compatibility with v1's API, config,
storage layout or internals is kept. v1 stays on `main` and keeps running until
v2 passes the cutover soak (M7); then v2 replaces it.

**Why:** the playback defects in v1 (DASH not playing, A/V drift, corrupted
pictures, failures after failover) come from five properties of its pipeline,
not from isolated bugs. See [bug-review/README.md](bug-review/README.md) for the
175 suspected defects and §1 below for the causes. v2 is designed so that none
of the five can exist.

## 1. What v1 got wrong, and the v2 rule that replaces it

| # | v1 property | Defect family | v2 rule |
|---|---|---|---|
| R1 | The hub carries raw TS chunks and AV packets, and every consumer branches on them | Empty or black outputs | The hub carries one type: `media.Frame` |
| R2 | Timestamps are rewritten in five or six places, at millisecond precision | A/V offset, drift, wrong durations | One `Clock` per stream, at ingest, in 90 kHz `int64`; nothing downstream rewrites time |
| R3 | A new session is a one-shot flag in a lossy channel | Consumers miss failovers | Every frame carries its `Epoch`; a consumer that lost frames still sees the change |
| R4 | A slow subscriber loses single packets | GOPs broken mid-way | A lagging subscriber skips to the next keyframe and is told how much it lost |
| R5 | Eight places demux MPEG-TS, each assuming PTS order | B-frame and codec handling differs per output | One TS demuxer and one TS muxer; containers are written by shared building blocks |

## 2. Scope

**In v2.0 (M1 to M6):**

| Area | Included |
|---|---|
| Inputs | RTMP push and pull, RTSP pull, SRT pull and listen, UDP (unicast and multicast), HLS pull, HTTP-TS, file loop, `copy://` |
| Codecs | H.264, H.265, AAC. Any other track is skipped at ingest with a logged reason |
| Transcoding | Per-stream subprocess on libav: decode once, encode an N-rung ladder, NVENC and software, audio AAC re-encode, passthrough per track |
| Picture features (M4) | Watermark (text with time directives, image from an asset library), applied before the ladder so every rung carries it. Thumbnails: a JPEG every N seconds from the decoder, also for passthrough streams by decoding keyframes only |
| Outputs | HLS (fMP4 and TS), DASH (fMP4), HTTP-TS, SRT, RTMP play, RTSP play, RTMP push-out |
| Control | Declarative stream specs in a file store, REST API, per-stream supervisor, input failover by priority |
| Operations | Prometheus metrics, `slog` JSON logs, pprof, conformance CLI |
| Later in v2.0 (M6) | DVR and timeshift, RTMP auto-publish by template, templates, playback sessions and tokens, webhooks |

**Dropped:** `mixer://` (audio from another stream; revisit as a transcoder
feature), S3 input, TS data PIDs (SCTE-35, teletext, KLV), v1's swagger docs.

## 3. Architecture

```
          ┌────────────── stream supervisor (one goroutine per stream) ──────────────┐
input ──► reader ──► demux ──► Clock ──► source hub ──► transcoder ──► rendition hubs ──► outputs
(RTMP, RTSP,      (TS, FLV,   (epochs,      │            (subprocess,        │
 SRT, UDP, HLS,    RTP)        90 kHz)      └──────── passthrough ───────────┘
 HTTP-TS, file)
```

### 3.1 Frame

Package `internal/media`. The only type that crosses a package boundary on the
media path.

| Field | Type | Meaning |
|---|---|---|
| `Track` | `int` | Index within the stream; outputs never assume one video and one audio |
| `Config` | `*TrackConfig` | Codec and parameters: dimensions; every VPS, SPS and PPS in use, keyed by ID; sample rate, channels, AudioSpecificConfig. Immutable; a new pointer means a configuration change |
| `PTS`, `DTS` | `int64` | Stream time in 90 kHz ticks. Never wraps. `PTS >= DTS`. DTS strictly increases per track |
| `Key` | `bool` | Random access point (H.264 IDR, H.265 IDR, CRA or BLA). Always true for audio |
| `RASL` | `bool` | H.265 leading picture that cannot be decoded when decoding starts at the preceding CRA. Anything that starts on a CRA drops these |
| `Data` | `[][]byte` | Video: NAL units without start codes, with no VPS, SPS, PPS or AUD. Audio: one raw AAC frame, no ADTS header |
| `Epoch` | `uint32` | Increases at every timeline break: source restart, failover, transcoder respawn, codec configuration change |

Parameter sets live only in `Config`. Every output whose container carries
them in-band (TS, RTP, FLV for H.265 resends) re-inserts all of them before
each keyframe; fMP4 puts them in the init segment.

Audio timestamps inside one source packet are derived from the sample count
(1024 samples per AAC frame), not by adding a rounded per-frame duration, so
rounding never accumulates. Outputs that need a sample-rate timescale (fMP4
audio) compute durations from sample counts too.

### 3.2 Clock

One per stream, between demux and the source hub. Readers hand it source
timestamps that are already unwrapped (33-bit TS PTS, 32-bit RTMP, RTP), each
frame with its arrival time: when the reader received the bytes that completed
it. `PushAt(frame, arrival)` takes both; `Push` stamps the current time.

- **Within an epoch,** stream time = source time − epoch base + epoch start, so
  deltas between frames and between tracks are kept exactly.
- **At an epoch start,** it drops video until each video track's first
  keyframe (and H.265 RASL frames after it), drops audio that would be
  presented before that keyframe, then holds frames until every known track
  has a usable frame, or for up to 1 s of source time counted from the
  keyframe. If video is expected but no keyframe arrives within 5 s, it
  releases audio alone; the first keyframe after that opens a new epoch, so
  audio and video always start together within an epoch. The epoch base is the
  smallest DTS held. The new epoch starts after the latest *presentation* end
  of the previous one (largest PTS plus frame duration over all tracks), so
  neither decode nor presentation time overlaps across epochs.
- **A jump** opens a new epoch. Jumps are judged against a stream-wide source
  high-water mark, not per track: one track going quiet while others continue
  is a gap, not a jump. A frame that continues its own track's timeline in the
  current epoch is never a jump, so a constant skew between tracks (above the
  2 s backward-jump limit) stays in one epoch; a real loop or jump breaks the
  track's own continuity too. A small backwards step on one track drops that
  frame and counts it.
- **A configuration change** (new `TrackConfig` pointer) opens a new epoch, so
  outputs get one signal for "new init segment or discontinuity". When several
  tracks change together (failover, PMT change), it is one epoch change and the
  new input's first GOP is kept.
- **A configuration-change epoch holds its whole 1 s window,** even when every
  track already has a frame. A track that changes later, inside the window,
  joins the same epoch, so staggered changes are one epoch change.
- **A late-joining track** (one that was not in an epoch when it was released
  and shows up later) opens the next epoch at the next video keyframe. Its
  hold is raised to its lateness plus 200 ms, capped at 5 s, so it starts
  with the others. Each track does this once per input; after that it joins
  the current epoch late, and is counted. An input switch resets it.
- **Drift (live sources).** Per epoch, a frame's offset is its arrival minus
  its stream time since the epoch start. The window is 10 s, or four
  arrival steps when the reader delivers in chunks (`Spec.ArrivalGranularity`:
  an HLS segment arrives whole, so 6 s segments give a 24 s window); four,
  so that it strictly holds three arrivals even when a source falling behind
  spaces them by more than a step. The
  baseline is the smallest offset over the epoch's first window of arrivals;
  bursts and network delay only raise offsets. Once per second after that,
  the drift is the smallest offset over the last window minus the baseline,
  judged only when the window holds three distinct arrival instants, so one
  slow segment fetch is no drift. Above `MaxDrift` (2 s for live inputs;
  paced inputs react only to a source falling behind), the next video keyframe
  opens a drift epoch. It keeps the epoch's constant offset and removes only
  the drift; frames that would overlap the previous epoch are dropped and
  counted.
- Each epoch records a wallclock anchor: the arrival time of its first frame.
  DASH and DVR use it to map stream time to wallclock, never letting a period
  start before the previous one ends, which matters for bursty sources such as
  HLS pull and file loop. Outputs never use wallclock for media timing.

### 3.3 Hub

One per (stream, rendition). Writes never block.

- Each subscriber has a bounded queue. When it is full, the subscriber enters
  skip mode: each video track resumes at its own next keyframe, and audio
  resumes once every live video track has (any frame for audio-only
  streams), so it always resumes on whole GOPs.
  RASL frames after a resume CRA are dropped too, up to its first trailing
  picture.
- Each delivery carries `Lost`, the number of frames skipped before it. Outputs
  treat `Lost > 0` like an epoch change.
- The hub keeps the current GOP. A new subscriber starts with it, so it starts
  on a keyframe and gets the current `TrackConfig` with the first frame.
- Drops are counted per subscriber, under the subscriber's name.

### 3.4 Output kit

Shared by every output, in `internal/ts` (TS muxer), `internal/mux` (fMP4 writer), `internal/segment`, and the protocol libraries `pkg/rtmp` and `pkg/rtsp`:

- **Segmenter:** cuts the frame sequence into segments and parts on video
  keyframes, from stream time only. It decides timing once; HLS-TS, HLS-fMP4,
  DASH and DVR consume the same segments. Audio is cut at the cut's PTS K,
  not by arrival order (rulings R-F3 and R-T1d, M4a): at a keyframe,
  MaxDuration or anchor-change cut, audio presented at or after K moves to
  the next segment, and the closed segment is held until every other audio
  track of the epoch reaches K or any track is 1 s past it (later audio
  below K is counted by `Late()`). Video tracks other than the anchor stay
  in arrival order, since their decode order differs from PTS order. Closes
  at an epoch change, `Lost` or `Flush` move nothing, so a segment closed by
  an epoch change or `Lost` may hold audio past its end.
- **TS muxer** (our `pkg/mpegts` writer), **fMP4 writer** (mp4ff `mp4`), **FLV tag
  codec** with enhanced RTMP for H.265 (`pkg/rtmp`), **RTP** packetisers, all
  our own in `pkg/rtsp` (M3b2): H.264 (RFC 6184, single NAL unit and FU-A),
  H.265 (RFC 7798, single NAL unit and FU) and RFC 3640 AAC; `pion/rtp` only
  marshals the packets. pion's payloaders take Annex B input and keep their
  own parameter-set cache, while ours take the frame's NAL units and send the
  parameter sets of `Config`, with one meaning of MTU (the RTP payload size)
  across codecs.
- **Epoch handling:** what an output does at an epoch change or `Lost > 0`
  (discontinuity tag, new period or init segment, sequence headers resent)
  lives in each muxer rather than in one shared helper, since the actions
  differ per container. The FLV muxer (`internal/mux/flv`, M3b1) sends one
  `onMetaData` and each carried track's sequence header per epoch and after
  every resync, and resumes after a loss at the next video keyframe. The RTP
  muxer (`internal/mux/rtp`, M3b2) needs nothing at an epoch change beyond the
  new parameter sets, which go in band before every keyframe anyway: RTP
  timestamps follow hub stream time, which is continuous across epochs, so
  they stay continuous. After a loss it resumes at the next video keyframe,
  with its parameter sets, and drops the audio presented before it. A codec
  change on a carried track, a new AAC config, or a frame of a kind the
  session's SDP lacks ends the session with a BYE per track (the SDP cannot
  follow; players reconnect and DESCRIBE again).
- **Parameter sets:** TS, RTP and FLV writers re-insert every parameter set
  from `Config` before each keyframe. No muxer or packetiser library does this
  for us.
- **HLS renditions stay aligned** (M3b3, closing the M3a rulings R4, R9 and R7,
  which are not §1's rules of the same number): every CMAF per-track playlist
  lists one entry per segment in the window, the track's part or an
  `EXT-X-GAP` placeholder (`gap-{track}-{seq}.m4s`, answered 404 and never
  loaded). Media sequence numbers and discontinuity counts are therefore equal
  across renditions by construction, also across a stretch where a track is
  absent, across window eviction, and when loss merges discontinuities in one
  rendition. A gap entry's `EXT-X-MAP` is that of the track's latest real part
  so far, so no listed entry changes between refreshes (RFC 8216 §6.2.1).
  DASH needs no placeholders: periods carry their adaptation sets.
- **TS streaming** (M3b3): HTTP-TS and SRT share one loop, `internal/output/tsstream`.
  After hub loss (`Lost > 0`) it drops frames until the next video keyframe
  (audio-only: the next frame), writes PAT and PMT, sets the
  `discontinuity_indicator` on the first packet of every track PID from there
  on, and drops audio presented before the keyframe. HTTP-TS starts from the
  hub's cached GOP (`SubscribeCached`), like the other streaming outputs.
- **SRT output** (M3b3): one gosrt listener; the stream ID selects the stream
  (request mode only, every refusal identical). Writes are cut to 1316-byte
  payloads (fewer whole TS packets when a caller's MSS is lower, so each write
  stays one SRT packet; below 4 packets the connection is closed at once and
  counted as refused) and
  retried when gosrt's send queue is full, and paced by a token
  bucket per connection (2.5 times the stream's bitrate, between 2 Mbit/s and
  256 packets per 5 ms), so a GOP replay at join does not arrive as one burst
  that is too late at 120 ms latency. Vanished receivers are ended by the
  reader goroutine, stalled ones from `conn.Stats`. Stream IDs are taken
  literally, never percent-decoded (ruling R-CI2): ffmpeg 6.1 does not
  URL-decode a URL's `streamid` (ffmpeg 8 does), so with ffmpeg pass the ID
  raw (`?streamid=#!::r=live/name`) or with `-streamid` (clients whose URI
  parser treats `#` as a fragment need their own stream ID setting).
- DASH maps stream time to wallclock through the epoch anchors, with one fixed
  `availabilityStartTime` for the life of the stream. A period starts at
  `max(anchor, previous period end)`.
- **Epoch anchors reach outputs through the hub** (M3 design note): the
  Clock's `Epoch` (ID, Start, Wall) is registered on the stream's hub before
  that epoch's first frame is written, and outputs look anchors up by epoch
  ID. DASH uses them as above.

### 3.5 Transcoder

- One subprocess per stream (`cmd/ostranscoder`, cgo, go-astiav), so a libav
  crash cannot take down the server.
- Frames go in and come out over a Unix socketpair passed as fd 3, as
  length-prefixed protobuf messages. There is no gRPC server, no socket path and
  no TS re-mux.
- Output PTS are the input frame's PTS, carried through the encoder. Output
  DTS come from the encoder (which reorders when B-frames are on) and are
  checked against the same conformance rules. Epochs pass through unchanged.
- A respawn opens a new epoch on every rendition hub. Rendition epoch numbers
  must never collide (M4): for example, a respawn epoch is the largest epoch
  seen + 1, and later upstream epochs are remapped above it.

### 3.6 Control plane

- **Stream spec:** a declarative document (inputs by priority, transcode
  ladder, outputs). The file store writes each spec atomically (write and
  rename).
- **Supervisor:** one goroutine per stream owns all of that stream's state and
  receives changes as messages, so there are no cross-stream locks. Changing
  inputs or the output set is applied in place; any other change rebuilds the
  stream's pipeline. This replaces v1's diff routing, which caused most
  lifecycle and config findings.
- **Failover:** the supervisor switches to the next healthy input by priority.
  The switch is an epoch change and nothing else; the transcoder keeps running.
- **API:** REST with chi, documented by an OpenAPI file written by hand and
  checked in CI.

### 3.7 Libraries

**Rule:** only independent libraries. Nothing that is the core of another
streaming product, so no bluenviron (mediacommon, gortsplib, gohlslib: the
core of MediaMTX) and no lal. Where no independent library exists, v2 builds
its own as a reusable library under `pkg/`, which imports nothing from
`internal/`. External servers (MediaMTX, ffmpeg) are still used as test peers,
never as dependencies.

| Concern | Library |
|---|---|
| H.264, H.265, AAC parsing (SPS, PPS, VPS, ASC, ADTS); fMP4 boxes | `Eyevinn/mp4ff` |
| RTP packets; SDP | `pion/rtp`, `pion/sdp/v3`; used from M2c by `pkg/rtsp` (`pion/rtp` parses and marshals RTP everywhere; its payloaders only in tests and `rtsptest`: the H.264, H.265 and AAC packetisers of the RTSP output are our own in `pkg/rtsp`, M3b2) |
| SRT | `datarhei/gosrt` (approved: a standalone library repository); used from M2a by the SRT caller and listener inputs |
| Transcoding | `asticode/go-astiav` |
| Transcoder protocol messages | `google.golang.org/protobuf` (M4) |
| HTTP | `go-chi/chi/v5` |

Our own libraries:

| Package | Contents | Milestone |
|---|---|---|
| `pkg/mpegts` | 33-bit timestamp unwrapping and ADTS parsing (M1); TS packet reader that resynchronises after loss or garbage, PAT/PMT with CRC and versions, PES assembly, and a TS writer (M3a). It replaces go-astits, whose behaviour on lossy and non-seekable input needed a workaround per case | M1, M3a |
| `pkg/unwrap` | Timestamp unwrapping for any bit width: 32-bit RTMP milliseconds, 32-bit RTP | M2 (delivered in M2b) |
| `pkg/h26x` | Bounded bit reader and our own H.264/H.265 parameter-set and slice-header parsing (it does not use `mp4ff` for them); picture order counts; DTS from PTS and the reorder depth, for RTP sources, which carry PTS only (POC times the reordered access units packed in one PES in the TS demuxer) | M2 (delivered in M2c) |
| `pkg/rtmp` | Handshake, chunk stream, client and server sessions, FLV tags, enhanced RTMP | M2 (ingest, delivered in M2b), M3 (play, push) |
| `pkg/rtsp` | RTSP client and server, TCP interleaved and UDP, Basic and Digest authentication, RTCP (sender reports, BYE), SDP, our own H.264, H.265 and RFC 3640 AAC packetisers, and `rtsptest`, a scripted server for tests | M2 (client, delivered in M2c), M3 (server, delivered in M3b2) |
| `pkg/hls` | Media and multivariant playlist parsing and writing | M3 (first), used by M2's HLS pull |
| `pkg/dash` | MPD writing (live, multi-period, SegmentTimeline) and parsing for `osconform` | M3 |

Container conformance checks read output with ffprobe as the independent
oracle, plus our own playlist parser for HLS and DASH timing, so that a writer
is never checked only by the library that wrote it.

Dropped from v1: `bluenviron/*`, `q191201771/lal`, `yapingcat/gomedia`,
`samber/do` (plain constructors in `main`), and the in-house `tsdemux` and
`tsmux`.

## 4. Codebase layout

```
cmd/openstreamer/      server
cmd/ostranscoder/      transcoder subprocess (cgo)
cmd/osconform/         conformance CLI: reads an output URL and runs the checks
internal/media/        Frame, TrackConfig, codecs, timebase
internal/clock/        Clock
internal/hub/          Hub and subscribers
internal/ts/           the only MPEG-TS demuxer and muxer, on pkg/mpegts
internal/e2e/          end-to-end tests across packages (tests only)
internal/conform/      property checks and container readers for tests and the CLI
internal/ingest/...    readers, one package per protocol
internal/rtmpsrv/      the RTMP listener shared by publish inputs and play outputs
internal/redact/       credential and stream-key redaction for URLs and errors
internal/segment/      segmenter
internal/mux/...       fMP4 writer on mp4ff; internal/mux/flv: hub frames to RTMP messages;
                       internal/mux/rtp: hub frames to RTP packets and sender reports
internal/output/...    one package per output protocol (rtmpplay: RTMP players;
                       rtmppush: RTMP push-out; rtspplay: RTSP players;
                       srtout: SRT receivers; tsstream: the MPEG-TS streaming
                       loop shared by httpts and srtout)
internal/transcode/    subprocess supervisor and protocol
internal/stream/       supervisor and failover
internal/store/        spec storage
internal/api/          REST API
pkg/...                our own reusable libraries (§3.7); no imports from internal/
testdata/fixtures/     generated clips and the script that makes them
```

## 5. Milestones

Every milestone ends with green tests in CI. Conformance checks are written
before the code they check.

| | Milestone | Depends on | Exit criteria |
|---|---|---|---|
| M1 | Media core: Frame, TS demuxer, Clock, hub, frame checks, fixtures | – | Every fixture goes demux → Clock → hub with no check violation, including the PTS-wrap and looped-source fixtures |
| M2 (done) | Ingest: `pkg/h26x`, `pkg/rtmp` server and client, `pkg/rtsp` client; every input protocol to Frames; input failover; H.264 open GOP in the TS demuxer | M1 | Our RTMP and RTSP libraries interoperate with ffmpeg and MediaMTX as peers; each input, fed by MediaMTX or ffmpeg in tests, produces violation-free frames; failover produces exactly one epoch change; a PMT change remaps the demuxer's tracks as one epoch change; an H.264 open-GOP broadcast feed starts video at its first recovery point |
| M3 (done) | Outputs: `pkg/hls`, `pkg/dash`, segmenter, muxers, `pkg/rtsp` server, RTMP play and push, every output protocol, `osconform` | M1 (M2's `pkg/rtmp` and `pkg/rtsp` for the RTMP and RTSP outputs) | ffmpeg and VLC-class clients play every output, and MediaMTX pulls every output it can take as a source (DASH and HTTP-TS are not MediaMTX sources; see Out of M3b3); for every output, `osconform` passes the steady-state, epoch-change, slow-consumer and B-frame scenarios, and ffprobe reads it. Proved by `TestM3Matrix` (every output under the four scenarios), `TestM3VLCClass` and `TestM3GStreamerClass` (VLC-class clients), `TestM3b3MediaMTXPull`, `TestM3b3SRTFFmpeg`, `TestHLSFFmpegAcrossGap` and `TestM3b3HLSAlignment`, with the M3a, M3b1 and M3b2 scenarios for the cells they cover |
| M4 | Transcoder, watermark, thumbnails | M1, M3 | In CI (no GPU, builder image restored): a 3-rung software ladder and passthrough pass M3's scenarios. On the GPU host: the same with 3 NVENC rungs, run by a make target and recorded in the PR; a killed subprocess comes back as one new epoch; the watermark appears on every rung; thumbnails refresh at the configured interval on transcoded and passthrough streams |
| M5 | Control plane: store, API, supervisor, metrics, logs | M2, M3 | A spec created over the API serves every output; editing outputs does not interrupt other outputs |
| M6 | DVR and timeshift, auto-publish, templates, sessions, webhooks | M5 | Feature tests; DVR playback passes `osconform` |
| M7 | Cutover | M1–M6 | The [soak plan](SOAK_TEST_PLAN.md) runs for 7 days with no S1 or S2 finding; then v2 is merged to `main` |

M3 starts after M1, in parallel with M2, with `pkg/hls` (parser and writer,
which both `osconform` and M2's HLS pull need) and `pkg/dash`, then the
outputs that need no other protocol library of ours (segmenter, HLS, DASH,
HTTP-TS, SRT). M2's HLS pull input comes after `pkg/hls`. M3's RTMP and RTSP
outputs follow M2's `pkg/rtmp` and `pkg/rtsp`.

M3 runs in two parts: M3a ([plan](superpowers/plans/2026-11-11-m3a-hls-dash.md),
done) serves HLS, DASH and HTTP-TS; M3b (RTMP play and push, RTSP server, SRT
output) follows M2b and M2c. M3b is split in three parts:

| Part | Contents |
|---|---|
| M3b1 ([plan](superpowers/plans/2026-11-17-m3b1-rtmp-out.md), done) | `pkg/rtmp` server play and client publish, an RTMP listener shared by publish inputs and play outputs (players address a stream by its name, never by a publish key), the FLV muxer, RTMP play and RTMP push-out outputs, `osconform` for `rtmp://`, M2b's items carried to M3b |
| M3b2 ([plan](superpowers/plans/2026-11-22-m3b2-rtsp-out.md), done) | `pkg/rtsp` server (TCP interleaved and UDP), our own RTP packetisers with parameter sets before keyframes, RTCP sender reports, the RTP muxer, the RTSP play output, `osconform` for `rtsp://` |
| M3b3 ([plan](superpowers/plans/2026-11-23-m3b3-srt-out-m3-done.md), done) | The shared TS streaming loop with resync after loss, the SRT output, per-segment HLS entries with `EXT-X-GAP` placeholders for the R4/R9 rendition alignment, `osconform` for `srt://`, the VLC-class client check and the M3 row's remaining scenarios for every output; M3 done |

M3b1's exit criteria are the M3 row's, for RTMP (VLC-class clients are
checked in M3b3 for every output): ffmpeg/ffprobe and MediaMTX (as a puller)
play our RTMP output for H.264+AAC and H.265+AAC; our push-out is accepted by
ffmpeg's RTMP listener and by MediaMTX; for the RTMP play output and the
push-out, `osconform`/`ReadRTMP` passes the steady-state, epoch-change,
slow-consumer and B-frame scenarios and ffprobe reads it; publishers and
players share one port without exposing publish keys. The `TestM3b1`
scenarios in `internal/e2e` prove them, with `TestServerPlayFFmpeg` and
`TestPublishFFmpegListener`/`TestPublishMediaMTX` (`pkg/rtmp`) and
`TestPushFFmpegListener` (`internal/output/rtmppush`) for the exchanges with
each peer. `ReadRTMP` reads only our own output; what passed through MediaMTX
is read back with our RTMP and RTSP inputs and checked as frames (MediaMTX's
RTMP output may resend headers or relay H.265 in the legacy form, which the
strict RTMP rules would flag). CI's host-network RTSP MediaMTX also pulls our
play output on demand from port 19361.

Out of M3b1:

| Item | Reason |
|---|---|
| RTMPS (TLS) | Needs certificates and their configuration: M5 |
| Play authentication and tokens | M6 sessions and tokens |
| Seek, pause, `receiveAudio` and `receiveVideo` | Live only: acknowledged and ignored |
| AMF3 | Clients that ask for `objectEncoding` 3 are refused, as in M2b |
| Legacy H.265 with FLV codec ID 12 | Non-standard; enhanced RTMP only |
| More than one target per push-out | Configure several push-outs |
| A mid-session video or audio codec change | Players and push targets cannot follow one: the play session ends with end of stream (players reconnect) and the push republishes |
| VLC-class client checks | M3b3, for every output |

M3b2's exit criteria are the M3 row's, for RTSP (VLC-class clients are
checked in M3b3 for every output): ffmpeg/ffprobe (over TCP and UDP) and
MediaMTX (as a puller) play our RTSP output for H.264+AAC and H.265+AAC; for
the RTSP play output, `osconform`/`ReadRTSP` passes the steady-state,
epoch-change, slow-consumer and B-frame scenarios and ffprobe reads it. The
`TestM3b2` scenarios in `internal/e2e` prove them (our RTSP pull input reads
the output with no A/V correction and no offset jump; MediaMTX pulls it on
demand, over TCP, from port 19362 in CI's host-network RTSP MediaMTX), with
`TestServerFFprobeTCP`/`TestServerFFprobeUDP` (`pkg/rtsp`) and
`TestPlayFFmpegDecodes` (`internal/output/rtspplay`) for the exchanges with
each peer. Over TCP a player that falls behind loses hub frames, not RTP
packets (dropped frames consume no sequence numbers), so `TestM3b2SlowConsumer`
maps the frames read back to the hub's to prove each resume is a keyframe.
libavformat outputs the first access unit of an RTSP stream without a pts
(its packets carry no byte position), on our output and on MediaMTX's alike,
so `TestM3b2BFrames` aligns ffprobe's pts at the first packet that has one.

Out of M3b2:

| Item | Reason |
|---|---|
| RTSPS and SRTP | Need TLS and its configuration: M5 |
| Play authentication and tokens | M6 sessions and tokens, as for RTMP play |
| Multicast; RTSP over HTTP tunnelling | Rare for players; added on demand |
| PAUSE, seek, `Scale` and `Speed` | Live only: PAUSE is answered 455 |
| RECORD and ANNOUNCE | Publishing into us over RTSP is not a v2 input |
| RTSP 2.0 | Players use RTSP 1.0 |
| A mid-session codec change, a new AAC config or a kind appearing | The SDP cannot follow: the session ends with a BYE per track and players reconnect |
| VLC-class client checks | M3b3, for every output |

M3b3's exit criteria are the M3 row's. ffmpeg and a VLC-class client play
every output: HLS fMP4, HLS TS, DASH, HTTP-TS, SRT and RTMP play with VLC 3
(two instances per row, since its `rc` stats count nothing while a `--sout` is
attached: one supplies the decoded counts, one the dump), RTSP play and the
RTMP push-out (read back over MediaMTX's RTSP) with GStreamer, as Ubuntu's VLC
has no RTSP module. MediaMTX pulls every output it can take as a source (HLS
fMP4, HLS TS and SRT; RTMP and RTSP by M3b1 and M3b2), and our RTSP input reads
back violation-free frames in one epoch; ffmpeg with libsrt, independent of
gosrt, decodes the SRT output with `-xerror`. Every output passes `osconform`
and ffprobe under the steady-state, epoch-change, slow-consumer (defined per
output: a slow packager subscriber for segment outputs, hub loss from a slow
writer for streaming ones) and B-frame scenarios. CMAF HLS renditions keep
equal media sequence numbers and discontinuity counts at every refresh, across
a video-only stretch and across window eviction. The `TestM3b3` scenarios and
`TestM3Matrix` in `internal/e2e` prove them (`TestM3b3MediaMTXPull` pulls from
ports 19363 for SRT and 19364 for HTTP in CI's host-network MediaMTX), with
`TestStreamResyncAfterLoss` (`internal/output/tsstream`),
`TestHLSPerSegmentEntries` and `TestHLSGapEviction` (`internal/output/live`),
and the `internal/output/srtout` tests (`TestSRTStalledReceiverEnded`,
`TestSRTLossyLink`, `TestSRTBurstJoin`) for the parts that need no peer.
Across a video-only stretch only ffmpeg plays on (`TestHLSFFmpegAcrossGap`);
the VLC and GStreamer limitations are in Backlog.

Out of M3b3:

| Item | Reason |
|---|---|
| SRT caller push-out | Add on demand; the RTMP push-out covers pushing today |
| SRT encryption | Needs credentials: M5; encrypted requests are refused |
| Publishing into the SRT output listener | The SRT inputs own publishing |
| DASH placeholders | Periods carry their adaptation sets; there is nothing to align |
| Silent AAC placeholder parts instead of gaps | Backlog: silent AAC placeholder parts |
| LL-HLS changes | Not part of the M3 row |
| Play authentication and tokens | M6 sessions and tokens, as for the other outputs |
| VLC-class playback across a video-only stretch | VLC 3 and GStreamer fail on it; see the Backlog's silent AAC placeholder and VLC 3 discontinuity items |
| MediaMTX pulls of DASH and HTTP-TS | They are not MediaMTX sources; ffmpeg and the VLC-class clients cover them |

M2 is split in three parts:

| Part | Contents |
|---|---|
| M2a ([plan](superpowers/plans/2026-11-12-m2a-ts-inputs-failover.md), done) | TS-family inputs (file, UDP and RTP-wrapped UDP, SRT caller and listener, HTTP-TS pull), HLS pull (TS and fMP4 segments, separate audio rendition), `copy://`, failover and fail-back, TS demuxer completion, fMP4 demuxer, HE-AAC in the TS muxer's ADTS writer, the Clock's carried fixes, ingest CPU measurement |
| M2b ([plan](superpowers/plans/2026-11-16-m2b-rtmp.md), done) | `pkg/unwrap`, `pkg/rtmp` (handshake, chunk stream, AMF0, server and client sessions, FLV, enhanced RTMP), RTMP push and pull inputs |
| M2c ([plan](superpowers/plans/2026-11-17-m2c-rtsp.md), done) | `pkg/h26x` (DTS from PTS and the reorder depth; POC also recovers reordered access units packed in one PES), `pkg/rtsp` client (TCP interleaved and UDP, Basic and Digest, RTCP, RFC 3640 AAC, `pion/rtp` depacketisers, `pion/sdp`), RTSP pull input |

M2a's exit criteria are the M2 row's, for its inputs: each, fed by ffmpeg,
produces violation-free frames; failover is exactly one epoch change; a PMT
change is one epoch change; an H.264 open-GOP feed starts video at its first
recovery point. The `TestM2a` scenarios in `internal/e2e` prove them.

HE-AAC in the TS muxer's ADTS writer was deferred from M3a to M2b, then
done in M2a, whose fMP4 HLS pull is the first HE-AAC input.

Out of M2a:

| Item | Reason |
|---|---|
| Source-specific multicast (232/8) | Needs `golang.org/x/net`; revisit on demand |
| H.264 intra-refresh (GDR) feeds | They never produce a `Key`; needs decoder-side recovery tracking; revisit on demand |
| Encrypted HLS | `EXT-X-KEY` other than `NONE` is refused |
| Packed-audio HLS renditions | `.aac` segments with ID3 timestamps |
| Single-file fMP4 HLS | `EXT-X-MAP` with `BYTERANGE` |
| A video-only mode when an HLS audio rendition stalls | The stall fails the whole input |
| Changing a running pipeline's inputs in place | §3.6; M5's supervisor adds it |
| Reordered access units packed in one PES | Dropped until M2c's `pkg/h26x` gives their picture order counts (done in M2c) |
| Dropping video to the next keyframe after a TS loss | Frames after the lost one pass through: decoders conceal, and a lossy feed would otherwise freeze |
| A late join with an inter-track skew above the 2 s backward-jump limit | Loses one GOP once, at start-up |
| Live-source epochs placed by the chain lead (R10) | Stay as ruled: bounded, and they do not accumulate |

M2b's exit criteria are the M2 row's, for RTMP: our RTMP library
interoperates with ffmpeg and MediaMTX as peers (we accept ffmpeg's publish;
we pull from ffmpeg and from MediaMTX); each RTMP input fed by them produces
violation-free frames; failover between an RTMP input and an M2a input is
exactly one epoch change; H.265 over enhanced RTMP works end to end with an
independent writer (an ffmpeg-made enhanced-FLV fixture, and MediaMTX). The
`TestM2b` and `TestRTMPMediaMTX` scenarios in `internal/e2e` prove them, with
`TestRTMPFFmpegServerPull` (`internal/ingest`) and `TestClientFFmpegServer`
(`pkg/rtmp`) for the pull from ffmpeg; CI runs MediaMTX as a service
container.

Out of M2b:

| Item | Reason |
|---|---|
| RTMPS (RTMP over TLS) | Needs certificates and their configuration: M5 |
| RTMPT and RTMPE | Obsolete |
| Enhanced RTMP v2 multitrack, and codecs other than H.264, H.265 and AAC (AV1, VP9, Opus, MP3) | Outside §2's codec scope |
| AMF3 command encoding | Clients that ask for `objectEncoding` 3 are refused |
| Publish authentication beyond an exact path match | M6 sessions and tokens |
| Server play and client publish | M3b, on the same session core |

M2c's exit criteria are the M2 row's, for RTSP: our RTSP client
interoperates with MediaMTX over TCP and UDP, fed by ffmpeg; RTSP inputs
produce violation-free frames for H.264 with B-pyramids, H.265 open GOP and
AAC; failover between an RTSP input and an RTMP input is exactly one epoch
change; `pkg/h26x`'s DTS equals ffmpeg's minus one constant on the fixtures;
the TS demuxer recovers reordered access units packed in one PES. The
`TestM2c` scenarios in `internal/e2e` prove them (CI runs a second MediaMTX
on the host network for RTSP, so that UDP works), with `pkg/h26x`'s DTS
tests against ffmpeg for the DTS criterion. MediaMTX's RTP-Info carries
`rtptime` over UDP but only URLs over TCP, it
builds its sender reports from packet arrival, and video with B-frames has RTP
timestamps that lead their arrival by the reorder depth. The RTSP input
therefore places video by DTS on arrival, evaluates sender-report offsets only
on fresh pairs (each track's SR newer than the last pair), and moves an offset
only when two consecutive fresh pairs confirm it, against 50 ms for the first
correction and 100 ms for later moves, each plus the video reorder lead;
unconfirmed candidates are counted (`sync_unconfirmed`). With that the tracks
meet the 40 ms start-offset rule against MediaMTX, in one epoch, once the
test publisher stops delaying audio (ffmpeg's RTP muxer aggregates AAC frames
for up to its `max_delay`, 0.7 s for RTSP, which put audio 120 to 200 ms
behind in the relay's arrival-based sender reports; the scenarios publish with
`-max_delay 0`).

The M2 row's DTS ruling changed in M2c: DTS is derived from the PTS and the
reorder depth, not from the picture order count (RTP carries the PTS, and the extractor
follows the reorder depth); POC is used for the TS case of several access
units in one PES, which has no per-unit timestamps.

Out of M2c:

| Item | Reason |
|---|---|
| RTSP 2.0; RTSPS and SRTP | TLS is M5, like RTMPS; `rtsps://` is rejected with a clear error |
| UDP multicast transport; RTSP over HTTP tunnelling | Rare for pull |
| MP4A-LATM, G.711, Opus, PCM | Outside §2's codec scope; such tracks are skipped with a reason |
| H.264 packetization-mode 2; H.265 with `sprop-max-don-diff` above 0 | Interleaved, DON reordering; skipped with a reason |
| Credentials in the URL path or query (`?user=&password=`) | Noted for M5's redaction work |
| The RTSP server | M3b |
| ONVIF | Outside M2c |

### Backlog

Items accepted for later, not yet placed in a milestone:

- **Per-input RTSP sync mode** (`?sync=sr|initial`): `sr` (the default) keeps
  M2c's confirmed sender-report rule; `initial` uses sender reports only for
  the first alignment and then trusts the RTP clocks, for relays whose
  arrival-based sender reports move with their publisher's pacing (a client
  that joins MediaMTX while its publisher starts gets one discontinuity; the
  e2e scenarios that pull from MediaMTX join after it settles, see
  `TestM2cRTSPMediaMTXH264`, M3b1's `TestM3b1MediaMTXPull` and M3b2's
  `TestM3b2MediaMTXPull`).

- **Silent AAC placeholder parts instead of `EXT-X-GAP` entries.** GStreamer's
  hlsdemux2 (1.24 and 1.28) loads consecutive gap URIs, gets 404 and stops
  (`TestM3GStreamerClass/hls-fmp4-across-gap` is skipped with this evidence;
  `OS_PEER_LIMITS=1` probes the limitation). It would very likely make
  GStreamer play across an audio-less stretch, and also help hls.js if stalls
  are reported there.
- **VLC 3's start-up "no reference clock" pair on HLS fMP4 and DASH.** On
  HLS fMP4 it appears in about 20 % of runs whether or not segments are
  aligned (30-run R-F3 A/B: 7/30 before, 6/30 after). DASH was not part of
  that A/B; with R-F3 it appeared in 2 of 3 runs. `TestM3VLCClass` tolerates
  it on both rows (ruling R-T1c). Next: investigate VLC's fMP4/DASH clock
  start, and measure DASH as HLS fMP4 was. Normal priority.
- **VLC 3 HLS-TS: R-F3 adds one deterministic clock reset at live start** (in
  `TestM3VLCClass` it lands on the loop seam of the 4× concatenated source,
  `hls/6.ts`; the 'no reference clock' pair follows in 1 of 30 runs).
  Measure with a seamless source. If the reset is real, start each TS
  segment's audio at or before the keyframe PTS (a TS-only layout; CMAF keeps
  R-F3's cut). Normal priority.
- **DASH MPD stays `type="dynamic"` after the stream ends** (pre-existing;
  M3a). Players keep polling an MPD that will not change.
- **VLC 3 stops at any fMP4 HLS discontinuity**, not only at gaps: every
  failover ends playback (`Failed to create demuxer (nil) MP4`, then end of
  input). Reproduced with an ffmpeg-made playlist carrying one
  `EXT-X-DISCONTINUITY` (VLC 3.0.20), so it is a VLC defect, not ours. The
  docs should recommend HLS TS for VLC 3 users.
- **PCR during audio-only stretches (`internal/ts/mux.go`). High priority for
  SRT, before the M7 soak** (ruling R-F2). The PCR PID is the first video
  track and the muxer sends PCR only on that PID's frames, so while video is
  absent (an A/V to audio-only failover) the TS carries no PCR, where
  ISO/IEC 13818-1 wants one at least every 100 ms. VLC plays through it
  (M3b3 Task 7 audio-only-stretch rows); broadcast IRDs may lose lock. Fix: move the PCR
  PID to a live track, or send PCR-only adaptation-field packets on the PCR
  PID while its track is silent; add a conform PCR-interval rule.

- **In-band SPS/PPS in the fMP4 video track** (`avc3`/`hev1`, or the
  parameter sets repeated in keyframe samples), so that decoders which ignore
  a new init segment decode resolution changes cleanly. ffmpeg's HLS demuxer
  (6.1 and 8) feeds every `EXT-X-MAP` into one mov demuxer, which skips the
  new `moov` ("Found duplicated MOOV Atom"), and decodes the 640x360 stretch
  of `TestHLSFFmpegAcrossGap` with the 320x240 parameter sets ("Reference N
  >= M", "corrupt decoded frame").
- **Timing-sensitive tests to watch in CI**: `TestSRTOpenContextAbortsListener`,
  `TestSRTLossyLink` (299 of 300 video frames) and `TestM3b2SteadyState`'s UDP
  player (273 of 275 video frames, 0 RTP lost) each failed once locally, only
  under soak load, and passed alone and in CI.
- **`TestFailoverSearchSkipsRunningProbe` flake** (internal/pipeline, from
  M2a): about 1–2 % under parallel race load (7 of 320), before and after the
  arm fix; `stepTo` (failback_test.go) syncs with `Run` only by waiting for
  the readers and a 50 µs sleep, so `Run` can fall behind the fake clock. Fix:
  a handshake that `Run` is idle after each step (13 call sites); raise if CI
  hits it.

#### M3b3 deferred minors

- `tsstream.Stream`'s doc comment has a stray line break.
- After hub loss while video has ended, up to 10 s of audio is dropped
  waiting for a keyframe (`maxKeyWait` is `hub.LiveWindow`).
- SRT pacing keeps the join-time rate when resets come more often than the
  2 s bitrate window covers, so it may go stale.
- HLS adds `EXT-X-PROGRAM-DATE-TIME` to the new first entry after eviction
  (pre-existing, HLS TS too); fix by writing it on every entry.
- conform drops a leftover pending rendition disagreement when polling stops.
- `TestReadHLSRenditionLag` does not exercise the spacing (its
  `TargetDuration` is 0).
- conform flags a recovery-point SEI that follows another SEI message (seen
  only on third-party streams).
- Strict TS rules (continuity, start) for HTTP-TS as an opt-in
  `ReadOptions` field.
- testpeer's host peers survive Ctrl-C; set `Pdeathsig` on Linux.
- procgroup's `Cancel` should map `ESRCH` to `os.ErrProcessDone`.
- `TestM3VLCClass` should assert VLC's `lost` stats delta.
- `store.learn` keeps a nil `Init` when a track's first part in an epoch had
  none, so the gap's `EXT-X-MAP` gets 404 (only if `fmp4.Init` fails).

### Carried from M1

The M2 items are done in M2a.

- **M2, H.264 open GOP (done):** an I-slice carrying a recovery point SEI is
  `Key`, and the frames after it presented before it (PTS below its PTS) are
  marked RASL-equivalent, so a broadcast feed starts video at its first
  recovery point (`TestM2aOpenGOP`).
- **M2, readers (done):** pass `Kinds` to the Clock, and keep `Config`
  pointers stable across repeated identical sequence headers, segments,
  demuxer instances and file loops.
- **M2, tests (done):** failover tests assert both the epoch count and the
  frames lost (`TestM2aFailoverSRTtoUDP`); frames lost to continuity-counter
  discontinuities are counted (`pkg/mpegts` reports them); the
  one-PES-per-access-unit and PES-without-PTS assumptions have broadcast-style
  tests.
- **M2, Clock (done):** a track that joins an epoch after its `AlignWindow`
  release opens a late-join epoch (§3.2).
- **M4, transcoder:** rendition epoch numbers never collide (§3.5).

### Conformance checks

Written in M1 (frame level) and M3 (container level), used in every later
milestone and by `osconform`:

- DTS strictly increasing per track; PTS ≥ DTS.
- The first frame of each epoch per video track is a keyframe.
- Track start offset within an epoch, measured on first PTS, under 40 ms *(initial)*.
- No RASL frame directly after the first keyframe of an epoch. (After a hub gap the same rule is checked by the hub's own tests, because frame checks cannot see `Lost`.)
- Audio frames are keyframes and never RASL; video data carries no VPS, SPS,
  PPS or AUD; a track's `Config` changes only at its first frame of an epoch.
- Every segment starts on a keyframe; EXTINF and MPD durations match media
  duration within one frame.
- Codec strings match the content.
- A discontinuity, period or init change appears at every epoch change, and
  nowhere else.

## 6. Risks

| Risk | Mitigation |
|---|---|
| Two systems in parallel for months | v1 receives only critical fixes the user asks for; v2 has a fixed milestone order |
| Demuxing every TS input costs CPU | Measured in M2 against v1's 4 cores for 10 streams; demuxing is the same work v1's tsnorm already does |
| Our own RTMP, RTSP and HLS libraries take time and miss real-world quirks | Each is test-first against recorded sessions and live peers (ffmpeg, OBS, MediaMTX, VLC); M2 and M3 are sized for them; the protocol RFCs and specs are the reference, not another product's code |
| `pkg/h26x` DTS-from-POC is subtle (B-pyramids, H.265 open GOP) | Checked against the DTS that ffmpeg writes for the same fixtures |
| Scope creep | §2 is the scope; anything else is a new milestone |
