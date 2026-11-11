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
timestamps that are already unwrapped (33-bit TS PTS, 32-bit RTMP, RTP).

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
  DASH and DVR consume the same segments.
- **TS muxer** (our `pkg/mpegts` writer), **fMP4 writer** (mp4ff `mp4`), **FLV tag
  codec** with enhanced RTMP for H.265 (`pkg/rtmp`), **RTP** packetisers
  (pion/rtp for H.264 and H.265, our own RFC 3640 for AAC in `pkg/rtsp`).
- **Epoch helper:** one function that tells an output what to do at an epoch
  change or `Lost > 0`: discontinuity tag, new period or init segment, sequence
  headers resent.
- **Parameter sets:** TS, RTP and FLV writers re-insert every parameter set
  from `Config` before each keyframe. No muxer or packetiser library does this
  for us.
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
| RTP packets and H.264/H.265 packetisers; SDP | `pion/rtp`, `pion/sdp` |
| SRT | `datarhei/gosrt` (approved: a standalone library repository) |
| Transcoding | `asticode/go-astiav` |
| Transcoder protocol messages | `google.golang.org/protobuf` (M4) |
| HTTP | `go-chi/chi/v5` |

Our own libraries:

| Package | Contents | Milestone |
|---|---|---|
| `pkg/mpegts` | 33-bit timestamp unwrapping and ADTS parsing (M1); TS packet reader that resynchronises after loss or garbage, PAT/PMT with CRC and versions, PES assembly, and a TS writer (M3a). It replaces go-astits, whose behaviour on lossy and non-seekable input needed a workaround per case | M1, M3a |
| `pkg/unwrap` | Timestamp unwrapping for any bit width: 32-bit RTMP milliseconds, 32-bit RTP | M2 |
| `pkg/h26x` | DTS from picture order count, for RTP sources, which carry PTS only | M2 |
| `pkg/rtmp` | Handshake, chunk stream, client and server sessions, FLV tags, enhanced RTMP | M2 (ingest), M3 (play, push) |
| `pkg/rtsp` | RTSP client and server, TCP interleaved and UDP, RFC 3640 AAC | M2 (client), M3 (server) |
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
internal/segment/      segmenter
internal/mux/...       fMP4 writer on mp4ff
internal/output/...    one package per output protocol
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
| M2 | Ingest: `pkg/h26x`, `pkg/rtmp` server and client, `pkg/rtsp` client; every input protocol to Frames; input failover; H.264 open GOP in the TS demuxer | M1 | Our RTMP and RTSP libraries interoperate with ffmpeg and MediaMTX as peers; each input, fed by MediaMTX or ffmpeg in tests, produces violation-free frames; failover produces exactly one epoch change; a PMT change restarts the demuxer as one epoch change; an H.264 open-GOP broadcast feed starts video at its first recovery point |
| M3 | Outputs: `pkg/hls`, `pkg/dash`, segmenter, muxers, `pkg/rtsp` server, RTMP play and push, every output protocol, `osconform` | M1 (M2's `pkg/rtmp` and `pkg/rtsp` for the RTMP and RTSP outputs) | ffmpeg, VLC-class clients and MediaMTX play every output; for every output, `osconform` passes the steady-state, epoch-change, slow-consumer and B-frame scenarios, and ffprobe reads it |
| M4 | Transcoder, watermark, thumbnails | M1, M3 | In CI (no GPU, builder image restored): a 3-rung software ladder and passthrough pass M3's scenarios. On the GPU host: the same with 3 NVENC rungs, run by a make target and recorded in the PR; a killed subprocess comes back as one new epoch; the watermark appears on every rung; thumbnails refresh at the configured interval on transcoded and passthrough streams |
| M5 | Control plane: store, API, supervisor, metrics, logs | M2, M3 | A spec created over the API serves every output; editing outputs does not interrupt other outputs |
| M6 | DVR and timeshift, auto-publish, templates, sessions, webhooks | M5 | Feature tests; DVR playback passes `osconform` |
| M7 | Cutover | M1–M6 | The [soak plan](SOAK_TEST_PLAN.md) runs for 7 days with no S1 or S2 finding; then v2 is merged to `main` |

M3 starts after M1, in parallel with M2, with `pkg/hls` (parser and writer,
which both `osconform` and M2's HLS pull need) and `pkg/dash`, then the
outputs that need no other protocol library of ours (segmenter, HLS, DASH,
HTTP-TS, SRT). M2's HLS pull input comes after `pkg/hls`. M3's RTMP and RTSP
outputs follow M2's `pkg/rtmp` and `pkg/rtsp`.

### Carried from M1

- **M2, H.264 open GOP:** an I-slice carrying a recovery point SEI is `Key`,
  and the frames after it presented before it (PTS below its PTS) are marked
  RASL-equivalent. Until then the M1 demuxer cannot start video on such
  broadcast feeds.
- **M2, readers:** pass `Kinds` to the Clock, and keep `Config` pointers
  stable across repeated identical sequence headers.
- **M2, tests:** failover tests assert both the epoch count and the frames
  lost; frames lost to continuity-counter discontinuities are
  counted (`pkg/mpegts` reports them); the one-PES-per-access-unit and PES-without-PTS assumptions get a
  broadcast-style test.
- **M2, Clock:** a track that joins an epoch after its `AlignWindow` release
  can start later than the track-start offset allows; M2 closes this gap.
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
