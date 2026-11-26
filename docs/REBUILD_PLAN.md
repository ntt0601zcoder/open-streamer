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
| Picture features (M4c) | Watermark (text with time directives, image from an asset library), applied before the ladder so every encoded rung carries it (a ladder with a copy-video rendition rejects it). Thumbnails: a JPEG every N seconds from the decoder, also for passthrough streams by decoding keyframes only |
| Outputs | HLS (fMP4 and TS), DASH (fMP4), HTTP-TS, SRT, RTMP play, RTSP play, RTMP push-out |
| Control | Declarative stream specs in one YAML config file with the server settings and the users, REST API with Basic auth (viewer and editor), a whole-config editor, reload on SIGHUP, per-stream supervisor, input failover by priority |
| Operations | Prometheus metrics, `slog` JSON logs, pprof, conformance CLI |
| Platforms | The server is pure Go and builds for any OS. The transcoder runs on Linux amd64 and arm64 with glibc 2.39 or later (Ubuntu 24.04+, Debian 13+); `internal/transcode` returns `errors.ErrUnsupported` on non-Unix systems. Encoding is software (libx264) and NVIDIA NVENC (`h264_nvenc`, per rung). macOS, Windows, VAAPI, QSV and VideoToolbox are out of v2.0 (ruling R-M4d) |
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
- **ABR presentation (M4b).** A transcoder ladder is served as one ABR
  presentation, whose segments align across rungs by construction:
  - *One hub, one packager* (ruling R-B1). The transcoder writes the
    *presentation* hub (§3.5): rendition i's video is track i, in ladder
    order, and each source audio track a is track (number of renditions) +
    a. One `live.Packager` in ABR mode packages it, so media sequence
    numbers and discontinuities are the same in every variant by
    construction. A manifest layer over the per-rendition packagers was
    rejected: their independent segmenters and losses can disagree on
    segment numbers and discontinuities (RFC 8216 §6.2.4).
  - *Alignment comes from the merger* (R-B3). The presentation's merger
    holds a keyframe barrier at every source main-video keyframe K (+D), so
    every rung's frames before its IDR at K come out before any rung's IDR
    at K, rung 0's IDR first, and the unchanged segmenter, cutting on the
    lowest video track, cuts every rung at the same frame. This holds for
    sources whose keyframes are IDRs (closed GOP); an open-GOP copy rung's
    leading pictures land after the barrier, so its segments there do not
    decode on their own. The barrier waits for a late rung, and what it
    waits follows the encoder's delay (M4b final review, finding C1): an
    encoder trails the copy rung by a number of frames (about 30 with
    x264), which is 1.2 s of stream time at 25 fps, 2 s at 15 fps and 3 s
    at 10 fps, so a fixed 2 s hold released the copy rung ahead of the
    barrier at every keyframe from 15 fps down, and the encoded rungs were
    out of every segment. The merger measures each track's lag behind the
    furthest frame pushed and holds 1.5 × the largest, at least `MergeHold`
    (2 s; 25 fps is unchanged) and at most 4 × `MergeHold` (8 s); a new
    epoch's first frame waits, in wall time, 1.5 × the largest lag learnt
    in an earlier epoch for the rungs announced for it, or that cap when
    none is known (the first epoch). A track's lag is forgotten once it has
    been silent for more than the cap of stream time. The costs: the
    presentation runs as far behind its source as its slowest rung (it did
    already, for every rung but the copy); a rung that falls
    silent, or a respawned child that never delivers, stalls it for up to
    the hold once (2 s at 25 fps, at most 8 s) before the rung is given up
    until it delivers again; and the copy rung's frames wait in the merger
    for that long. An encoder more than 8 s of stream time behind (below
    about 4 fps with x264's defaults) is beyond the cap: the copy rung is
    then released ahead of *every* barrier, and the rungs that miss it are
    out of every segment, not one, for as long as the lag lasts; the copy
    rung can also open the epoch and keep the anchor. That is accepted,
    and visible: `live.Packager.Rungs` counts each rung's gaps and frames
    left out, and the packager warns about them (below).
  - *A rung that misses the segment start is left out* (R-B8,
    `segment.Options.AlignVideo`). (1) A lower video track takes the anchor
    only at a keyframe at or above the open segment's Start (strictly above
    the newest closed segment's Start when none is open; any keyframe before
    the epoch's first segment), and its earlier frames are dropped; at
    Start the open segment is taken back and reopened by the new anchor. No
    segment ever starts at or below its predecessor's Start. (2) In a
    segment that is not Forced, a video track other than the epoch's anchor
    enters only at a keyframe whose PTS equals the segment's Start; until
    then its frames are dropped, and a track whose first keyframe in the
    segment has another PTS is left out of it. (3) A rung that leads the
    anchor (its IDR at K reached the segmenter before rung 0's): at a cut
    at K, its frames from its keyframe at K on move to the next segment,
    as audio does, so its part of the closing segment ends at K and the
    next starts on its keyframe. A rung that missed the barrier at one K
    (past the merger's hold, swallowed by a slow subscriber, or with no IDR
    at K) therefore costs, at that K: when it trails the anchor, its frames
    below K that came after rung 0's IDR (its previous part is cut short by
    them), or a gap entry for that one segment when its IDR at K is missing
    or lost; when it leads the anchor, nothing. Never a part that does not
    decode from its start, and never a part running past its segment,
    except a leading rung with no keyframe at K, whose frames are not moved
    (Backlog). A rung that misses every barrier (an encoder beyond the
    merger's cap, above) pays that at every segment. The packager counts
    it per rung (`live.Packager.Rungs`: segments without the rung, and
    frames left out, from `segment.Segmenter.LeftOut`) and logs a warning
    when the counts rise, at most once every 10 s; osdev reports them with
    its packager counters. Under AlignVideo a segment closed
    without a cut records each video track's own span where it differs from
    the anchor's (`VideoDurations`).
  - *HLS TS* (R-B4). One TS per segment per video track that has frames in
    it, holding that track and every audio track, muxed with the video at
    track index 0 and audio at 1 + its rank among the window's known audio
    tracks, so every variant has the same PIDs; continuity counters are per
    variant, and no combined TS is muxed. `hls/index.m3u8` is the
    multivariant playlist; `hls/v{track}.m3u8` lists one entry per segment,
    `{track}-{Seq}.ts`, or a `gap-{track}-{Seq}.ts` placeholder when the
    variant's video has no frames in the segment or its mux failed.
    MEDIA-SEQUENCE is the first Seq. A real entry's EXTINF is its variant's
    own duration (`VariantTS.Duration`): the segment's, except in a segment
    closed without a cut, where each rung's own span is used (the anchor
    rung can stop early, as at a respawn, while the copy rung runs on); the
    same duration divides that variant's bytes for BANDWIDTH. All TS
    variants share one TARGETDURATION, the largest over every listed segment
    and variant duration, whichever playlist is fetched first.
  - *HLS fMP4 and the masters* (R-B5). `cmaf/master.m3u8` lists the
    variants `v{track}.m3u8` in rendition order, with the audio as a
    rendition. BANDWIDTH is the window's peak over segments of (variant
    video part bytes + default audio part bytes) × 8 / duration, the
    variant TS's bytes for HLS TS; RESOLUTION is the window's largest. No
    AVERAGE-BANDWIDTH or FRAME-RATE.
  - *DASH* (R-B6). Video tracks of the same codec share one AdaptationSet,
    each Representation with its own SegmentTemplate (init, media,
    startNumber, timeline, presentationTimeOffset). The set carries no
    `segmentAlignment` attribute: part numbers stay per track, since a gap
    cannot be expressed in a `$Number$` timeline, so after a gap the
    Representations' numbers differ by a constant. Alignment holds in time:
    no segment of one Representation straddles another's segment start.
    Audio, and every non-ABR stream, keep one set per track.
  - With ABR off (`live.Options.ABR`, `segment.Options.AlignVideo` and
    `transcode.Options.Presentation` false) every output is byte for byte
    what M4a produced.
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

- One subprocess per stream (the transcoding child; a stream with no
  transcoding child and thumbnails on runs one thumbnail-only child instead,
  R-C10a): `cmd/ostranscoder`, cgo, go-astiav, so a libav crash cannot take
  down the server. cgo and libav are needed only by
  `internal/transcode/native` and `cmd/ostranscoder`, behind the build tag
  `libav`; without the tag everything else builds, vets and tests with no
  libav, and `ostranscoder` is a stub that exits 2.
- Frames go in and come out over a Unix socketpair passed as fd 3, as
  length-prefixed protobuf messages (`internal/transcode/wire`, hand-coded with
  `protowire`: no code generation, no gRPC server, no socket path, no TS
  re-mux). Each record is a 4-byte length and one envelope that holds exactly
  one message: Hello, Configure, Frame, Discontinuity, Stats, Error, Stop,
  SetWatermark (host to child) or Shot (child to host). Configure, sent once,
  carries the ladder (each rung with its encoder), the audio encode, the
  watermark and the thumbnail settings; one with no rungs and no audio but a
  thumbnail asks for shots only. SetWatermark replaces or removes the
  watermark of a running child, and Shot carries one thumbnail. Each
  `TrackConfig` travels once, by ID, in the first Frame that uses it, and
  later Frames refer to the ID. `Stats` carries the counters `In`, `Out`,
  `EncodeNanos`, `DroppedTracks`, `Reanchors`, `AudioDropped`, `AudioCorrupt`
  and `VideoCorrupt` (ruling R-T6a), and `EncoderFallbacks`, `Thumbnails` and
  `ThumbnailsFailed`. The wire is version 3 (R-C13; M5b1's R-E12): version
  2 added the watermark and the thumbnails, version 3 a rung's `no_fallback`,
  the watermark's font bytes and an Error's code. Every field added since
  version 1 is absent when empty, so a Configure of x264 rungs with no
  watermark and no thumbnail has version 1's bytes. The host rejects a child
  whose Hello version differs, so server and bundle are deployed together.
- **Time.** The host adds the rendition delay D = 45000 ticks (0.5 s, ruling
  R-M4c) to every frame and anchor of every rendition, copy tracks included,
  so renditions stay aligned with each other. D covers the encoder's reorder
  delay (B-frames up to 3 at 8 fps or more), so encoder DTS is never negative,
  also for sources without B-frames; `New` rejects more than 3 B-frames, and a
  source below about 8 fps should use none. Output PTS are the input frame's
  PTS, carried through the encoder. Output DTS come from the encoder and are
  checked against the same conformance rules. Audio is re-anchored to the
  input's PTS when it jumps forward by more than one frame, and input that
  runs behind the prediction is dropped and counted, never re-anchored
  backwards.
- **IDRs at source keyframes.** Every rung encodes an IDR at exactly the PTS of
  every source keyframe (matched by PTS, since the decoder's key flag misses
  H.264 recovery points), so all rungs and the source segment identically.
  libx264 runs with `scenecut=0:keyint=600:min-keyint=1:rc-lookahead=10:b-pyramid=none`;
  `b-pyramid=none` is ruling R-T7b', whose cause M4c found
  ([notes](superpowers/notes/2026-11-25-r-t7b-root-cause.md)). VLC 3 logs
  "buffer deadlock prevented" at a clock reset at the live edge, never at the
  start of a play, when its re-buffer feeds the video decoder fewer packets
  than its frame-threaded H.264 decoder needs for a second picture: with
  VLC's 6 threads, 8 at reorder depth 1 and 9 at depth 2. Our PCR runs 700 ms
  behind the written DTS (`timeOffset` − `pcrOffset`), so of the 1000 ms
  re-buffered only about 300 ms are video of the new segment: 8 access units
  at 25 fps. It is a limit of VLC 3
  reached by a conformant stream, and the line does not stop playback. With
  the pyramid (depth 2) it was logged in 17 of 46 plays on the tree before
  the setting, and in 0 of 16 without it. Depth 1 fits at 25 fps with no
  frame to spare; a lower frame rate feeds fewer frames, and at 15 fps the
  line follows VLC's one reset in every play (Backlog: low frame rates in
  VLC 3). Every encoded rung's SPS announces a reorder depth of at most 1,
  pinned by a test (R-C11).
- **Rendition epochs.** Rendition epoch numbers never collide. The host maps
  each upstream epoch e to e + offset, and a *bump* opens a new rendition
  epoch numbered (largest issued) + 1 and raises the offset. A respawn and the
  loss of the source subscription (`Lost`) cause bumps. A bump takes effect at
  K: the next source video keyframe, or the next frame when the upstream epoch
  has no live video; for a respawn, the first keyframe after the new child's
  Hello. K must lie above every PTS already sent in the old epoch, otherwise
  the next keyframe that does is K. While a bump is pending, audio is held for
  at most 1 s; at K, held audio below K is dropped and counted, and the rest
  joins the new epoch. Bumps requested while one is pending coalesce, and one
  anchor per rendition epoch is registered on every rendition hub before its
  first frame. The host keeps one source subscription across respawns, and the
  child passes epochs through. Child-bound audio is sent only when the child
  queue has room, otherwise it is dropped and counted (ruling R-T5a).
- **DTS merger.** Each rendition hub is written by a merger that merges the
  child's output with the copied tracks of the same rendition into one
  non-decreasing DTS order, holding up to `MergeHold` (2 s) of stream time for
  a quiet track, so a rendition mixing copied video with encoded audio stays
  aligned.
- **Presentation hub (M4b).** With `Options.Presentation` the transcoder also
  writes the ladder's ABR presentation (§3.4) to one more hub, through its
  own merger, which carries the same PTS and DTS as the rendition hubs
  (ruling R-B1). The router announces a barrier at every source main-video
  keyframe PTS K (+D) before the keyframe goes to the child or the copy
  path; the merger releases every rung's frames before K first, then the
  rungs' IDRs at K in track order, waiting for a late rung 1.5 × the lag
  it measures between the tracks, between `MergeHold` and 4 × `MergeHold`
  (R-B3; §3.4 has the reason and the costs). A new epoch's first frame
  waits, in wall time only, for the rungs announced for it: 1.5 × the
  largest lag recorded in an earlier epoch, within the same bounds, or 4 ×
  `MergeHold` when there is none (the first epoch of a run, or every
  earlier track's lag forgotten). Stale lag is pruned: a track's lag is
  forgotten once, in the latest epoch, the furthest frame has moved more
  than 4 × `MergeHold` of stream time past the track's last push, or,
  when the track last pushed in an older epoch, once the latest epoch has
  run that long; so the previous epoch's lag carries into the new epoch's
  start, and a rung that falls silent stops raising the hold. The
  rendition mergers do not use the barrier and keep the fixed `MergeHold`.
  The hub's `GOPCacheFrames` is 1500 × (renditions + 1), so a late
  subscriber gets every rung's IDR of the cached GOP (rung 0's IDR, where
  the cache starts, comes first), and the merger counts into
  `Stats.PresentationOut` and `Stats.PresentationDropped`, not `FramesOut`
  or `MergeDropped` (R-B7).
  The product names it (M5a's R-D3): a stream with a ladder is served as
  `<name>`, which over HLS (TS and fMP4) and DASH is the presentation and
  over HTTP-TS, RTMP, RTSP, SRT and RTMP push is rendition 0, the first in
  the spec; each rendition is also its own stream `<name>_<label>` on every
  enabled output; the source of a laddered stream is not served to players
  (`copy://<name>` reads it), there is no `<name>_abr`, and `abr` is an
  ordinary label. osdev keeps its development names: the presentation is
  `<name>_abr`, on HLS and DASH only, and there the label `abr` is reserved
  (R-B2).
- **Passthrough** (`name=label:copy`) needs no child: copy tracks go from the
  host's subscription to the rendition's merger directly, and a stream whose
  renditions all copy never starts a transcoding child; with thumbnails on it
  runs a thumbnail-only child.
- **Child supervision.** The host feeds a child that has not said Hello yet
  from a backlog of up to 1500 frames, the hub's GOP cache (ruling R-T5c), so
  joining a live hub with a long GOP costs no epoch; only an overflow bumps.
  `HelloTimeout` (10 s) kills a child that never says Hello. A send to a live
  child that blocks for `InputStallTimeout` (10 s) kills it with the cause
  "input stalled", where progress is the child's own report (`Stats.In`
  rising, or any Frame output), not completed host writes (rulings R-T5d and
  R-T5e). A dead child, and a child that cannot be started (a missing
  binary included, so that a binary installed later is picked up), is
  respawned with backoff (250 ms doubling to 5 s), each failure logged and
  counted as an exit; a version mismatch ends `Run` with an error. `Run`
  returns nil when the source hub closes and `ctx.Err()` when ctx ends.
- **Decoder errors.** Corrupt input frames, audio and video, are skipped and
  counted (`AudioCorrupt`, `VideoCorrupt`); a decoder fails the pipeline only
  after 50 consecutive corrupt frames (ruling R-T4b). Encoder errors stay
  fatal.
- **Only the lowest-numbered video track** is encoded and copied; other video
  tracks are dropped, logged once and counted. Every audio track is encoded
  (AAC, native `aac` encoder) or copied.
- **NVENC (M4c).** The encoder is chosen per rung (R-C1):
  `transcode.Video.Encoder` is `""` or `"x264"` (libx264) or `"nvenc"`
  (`h264_nvenc`), and a ladder may mix them; each rung is its own encoder
  context, and the presentation's barrier already orders rungs with
  different decode delays. Frames stay on the CPU (R-C2): the child decodes
  in software, applies the watermark and scales with swscale as for x264,
  and gives `yuv420p` system-memory frames to `h264_nvenc`, which uploads
  them itself; there is no `hwupload`, cuvid decode or `scale_cuda`. The
  options are fixed (R-C3): `preset` is `Video.Preset`, `""` (→ `p4`) or
  `p1`…`p7`, and `New` rejects an x264 preset name on an nvenc rung;
  `tune=hq`; `rc=vbr` with bitrate, maxrate (0 → bitrate) and bufsize
  2 × maxrate, as x264; `profile=high`; GOP 600, as x264's `keyint=600`;
  `forced-idr=1` with an IDR at each source keyframe PTS, as on x264 rungs;
  `no-scenecut=1`, `rc-lookahead=0`, `strict_gop=1`, `delay=0`, `aud=0`.
  B-frames follow the x264 rungs' rule (`BFrames` 0 means 2, −1 none, at
  most 3, under the same D), and `b_ref_mode=disabled` always, NVENC's
  analogue of `b-pyramid=none` under R-T7b'. A failed NVENC open falls back
  to libx264 for that rung and that build (R-C4): same height, rates and
  B-frames, preset `veryfast`. `Stats.Child.EncoderFallbacks` rises by 1 for
  each rung that fell back, the child logs one warning per build naming
  those rungs, and the next build (a source config change, a discontinuity,
  a respawn) tries NVENC again. The driver's session limit, sessions held by
  another process, a resolution below NVENC's minimum, a card without
  B-frames, a missing driver and a build without the encoder all end in this
  fallback; a rung or a stream is never failed for them, unless the rung
  sets `no_fallback` (M5b1's R-E9, valid only with `encoder: nvenc`). Then a
  failed NVENC open fails the build, and since a build opens every rung or
  none, the whole ladder: the child sends `Error{Rung, Code:
  ErrorEncoderRefused, Fatal: true}` and exits 1, and the host counts
  `Stats.EncoderRefusals` (once per refused build; the metric
  `openstreamer_transcoder_encoder_refusals_total{stream}` and the status's
  `transcoder.encoder_refusals`), ends the exit error with the child's
  message and respawns by its backoff. No software frame is produced for any
  rung of that ladder, whose outputs carry nothing until NVENC opens.
  `ostranscoder -probe nvenc` opens and closes one NVENC encoder and exits 0
  (`nvenc: ok`) or 3 (`nvenc: <error>`).
- **Watermark (M4c).** It is drawn once, on the decoded picture before the
  ladder (R-C5), so every encoded rung carries it, scaled with the picture.
  A copy rung cannot carry it, so `New` and `SetWatermark` reject a
  watermark on a ladder with a copy-video rendition. The spec,
  `picture.Watermark` (R-C6), is `Text` or `Image`, never both. Text is at
  most 256 bytes of valid UTF-8 on one line, with the directives
  `%Y %m %d %H %M %S` and `%%` (any other `%x` is rejected), formatted in
  `TimeZone` (an IANA name, `""` = UTC; both binaries embed `time/tzdata`)
  from the child's wall clock and re-rendered when the formatted string
  changes; it is white with a black border of max(1, size/16) px. An image
  is PNG (alpha kept) or JPEG file bytes, at most 1 MiB and 2048 px per side.
  `Position` is top-left (the zero value), top-right, bottom-left,
  bottom-right or centre, with a margin of 2 % of the picture height;
  `OpacityPct` is 1…100 (0 → 100); `SizePct` is the text's glyph height in %
  of the picture height (0 → 5, 1…50) or the image's width in % of the
  picture width (0 → 15, 1…100), its height following the aspect ratio. Text
  is rendered by libavfilter `drawtext` with the embedded DejaVu Sans
  (R-C7), written to a private temp directory (`ostx-wm-<pid>-<random>`,
  removed when the watermark closes; a child killed or crashed cannot, so
  building a text watermark first removes this user's `ostx-wm-<pid>-*`
  directories whose PID no process has), with `expansion=none` and the
  text read from a `textfile` that the child rewrites atomically (`reload=1`
  when the text has a time directive). Since M5b1 the font may be an
  operator's (R-E8): the spec's `font` names a font asset, for text only; the host
  checks it with `picture.CheckFont` and sends its bytes in the watermark
  (wire field 7), never a path, and the child writes them to `font.ttf` in
  that directory, the path the graph already names. The child sets
  `FONTCONFIG_FILE` to a directory, so drawtext never falls back to a system
  font. A font's bytes are immutable and shared, never copied by
  `cloneWatermark` or the supervisor. Operator text never enters a
  filter-graph string: the graph is built only from numbers and the paths
  the child makes, and a temp path with a character outside
  `[A-Za-z0-9_./-]` is refused. Images come from an asset library, a
  directory (R-C8): an asset name matches
  `^[a-z0-9][a-z0-9_-]{0,63}\.(png|jpg|jpeg)$` and the file is read by
  `picture.LoadAsset` at each build, watermark edit or refresh;
  `transcode.Options` and the wire carry the bytes, so the transcoder never
  sees a path. A file replaced through the asset API is reloaded at once
  (R-E7); one changed by hand only at the stream's next build, watermark
  edit (an edit that changes the watermark) or API replacement. Live update
  (R-C9): `Transcoder.SetWatermark` sends one
  SetWatermark message and the child swaps its filter graph at the next
  decoded picture, with no drain, no IDR and no epoch. The host keeps the
  latest watermark and sends it in the next child's Configure, so a
  respawned child starts with it. A watermark the child cannot build is a
  fatal child error: a stream configured with a watermark never goes out
  without it. `internal/transcode/picture` is pure Go.
- **Thumbnails (M4c).** `picture.Thumbnail{IntervalSec, Height, Quality}`
  (R-C10): 0 means 10 s, 180 px and 80; the interval is 1…3600, the height
  even, 2…720 and capped at the source's, the quality 1…100. A shot is taken
  from the decoded picture before the watermark, scaled by swscale to
  full-range YUV 4:2:0 at the display aspect ratio, and encoded with Go's
  `image/jpeg`. The schedule runs in stream time: a shot is due at the first
  picture, at a new epoch, when PTS ≥ last shot + interval, and when PTS is
  below the last shot's (the stream time went back). Only the
  latest shot is kept, in memory; osdev serves it at
  `GET /live/{stream}/thumbnail.jpg` under the source stream's name. A
  stream still runs one subprocess (R-C10a). Whenever it has a transcoding
  child, that child takes the thumbnails: from every decoded picture when it
  encodes video; when it encodes only audio (copy video), the host also
  sends it the main video track's keyframe whenever a shot is due, and the
  child decodes that keyframe alone (*key-only*). On every key-only path the
  host's `picture.Schedule` is the only schedule and the child shoots every
  keyframe it is sent, so one keyframe per interval crosses the socket and
  the interval is N rounded up to the next keyframe. A full child queue
  skips the keyframe (the next one is tried), never the audio, and a
  keyframe that does not decode alone costs a thumbnail, never the child.
  The child reports such failures in `Stats`, and the host retries by one
  rule: the first failure after a success (or after the child's start)
  resets the schedule, so the next keyframe is tried at once; while failures
  stay consecutive the schedule stands, so a source whose keyframes never
  decode alone costs one attempt per interval. The schedule is also reset
  when the child exits. Only a stream with no transcoding child (no ladder,
  or a ladder that copies everything) gets a child of its own, in
  thumbnail-only mode, run by `transcode.Thumbnailer` on the source hub and
  fed keyframes only; `New` rejects thumbnails on a ladder that needs no
  child. The Thumbnailer supervises its child: a
  child without Hello after `HelloTimeout` (10 s), or silent after Hello
  for `SilenceTimeout` (10 s; it sends `Stats` every second), is killed; a
  child that exits, or cannot be started (a missing binary included), is
  respawned with backoff (250 ms doubling to 5 s), as the Transcoder's is,
  and the schedule is reset; a wire-version mismatch ends `Run` with an
  error. Like `Transcoder.Run`, `Run` returns nil when the source hub closes
  and `ctx.Err()` when ctx ends.
- **Build.** FFmpeg 8.1.3 shared, GPL, comes from a pinned FFmpeg-Builds
  release instead of a compiled builder image (ruling R-M4b): release
  `autobuild-2026-10-31-12-59`, archives
  `ffmpeg-n8.1.3-27-g8dca1586f4-linux64-gpl-shared-8.1.tar.xz` (sha256
  `202fe67679a0860b155817a0907ffcc5c1ea82e186acaa21cb87d307f54fd59a`) and
  `ffmpeg-n8.1.3-27-g8dca1586f4-linuxarm64-gpl-shared-8.1.tar.xz` (sha256
  `9d3161962e57f0f621b87c1fe4287ad14b4fd12c74c243436143319e4e14a0a0`).
  `scripts/ffmpeg-dev.sh` verifies the sha256 before unpacking and checks that
  libavcodec is 62.x. The release ships no libfdk-aac, so audio uses the
  native encoder. `scripts/bundle-ostranscoder.sh` builds `ostranscoder` with
  an `$ORIGIN/lib` rpath into `dist/ostranscoder-<arch>/`, with the libav
  libraries and FFmpeg's license files.

### 3.6 Control plane

- **Stream spec and the config file** (M5a's R-D2, R-D4): a stream is a
  declarative document (`internal/spec`: inputs by priority, layout,
  transcode ladder, thumbnail, outputs and pushes), with a structural
  `Validate`, a redacted copy and an `ETag`. All configuration is one YAML
  file (`internal/config`, `-config path`, at most 4 MiB, mode 0600 on
  Unix): `version: 1` and three sections, `server` (listeners, apps, the
  transcoder binary, the assets directory, logging), `users` and `streams`.
  It is decoded strictly (unknown and duplicate keys are errors), anchors,
  aliases and merge keys are refused, and every scalar that resolves as a
  number must be a plain decimal integer, so text that looks like a number
  is quoted. Every API edit writes the whole file atomically: a temp file
  `.<base>.<8 hex digits>.tmp` in the same directory, mode 0600, `fsync`,
  rename, `fsync` of the directory (Unix; `EINVAL` and `ENOTSUP`, from a
  file system that cannot `fsync` a directory, are ignored); a leftover
  temp file is removed at the next start. A whole-config `PUT` writes the
  client's exact bytes; a per-stream edit goes through the YAML node tree,
  which keeps the head comments and re-emits the rest of the layout. A
  hand edit is applied by a reload, on SIGHUP or `POST /api/v1/reload`.
  The server remembers the SHA-256 of the text it last loaded, wrote or
  reloaded: an API write that finds the file different is refused with 409
  until a reload, so neither side overwrites the other. A reload and every
  write are all or nothing on the whole document, with every error
  reported. Only the start is lenient: a stream entry that does not decode,
  fails `Validate` or repeats a name is skipped and reported, and blocks
  later writes and reloads until it is fixed; an error in `version`,
  `server` or `users` stops the start. A change in `server` is not applied
  while running and is reported as `restart_required`.
- **Users** (R-D8, `internal/auth`): HTTP Basic auth against the file's
  `users`, each a name, a role and a password hash. `viewer` reads; `editor`
  also writes and gets what holds stored secrets or process internals
  (`GET /api/v1/config`, `?secrets=1`, pprof). A hash is
  `pbkdf2-sha256$600000$<salt>$<key>` (PBKDF2-HMAC-SHA-256, a salt of at
  least 16 bytes, a 32-byte key, unpadded standard base64, in canonical
  form), made by `openstreamer hash-password`; any other iteration count, a
  plaintext, an unknown role, more than 64 users or no editor is refused.
  Unknown names are verified against a dummy hash, successes are remembered
  under a per-process HMAC key, at most 2 verifications run at once, and a
  per-address-and-name limiter blocks after 5 failures for 1 s, doubling up
  to 5 min; a remembered login touches no limiter state. Writes need an
  `X-Requested-By` header. The API listens on `127.0.0.1:8081` by default,
  apart from the media listener, because Basic credentials are only base64
  until M5c's HTTPS.
- **Supervisor** (R-D5, `internal/stream`): one goroutine per enabled
  stream owns all of that stream's state (its hubs, pipeline, transcoder or
  thumbnailer, packagers and pushers) and takes edits from a mailbox, in
  order; readers (the output servers' lookups, the status, the metrics) see
  only an immutable `View` and `Status` behind atomic pointers, so there is
  no cross-stream lock. There is no periodic reconciler: a `Manager` runs a
  supervisor for exactly the enabled specs, judges an edit of the set as a
  whole (`stream.Check`, listen claims, `copy://` cycles) before the file
  is written, and never lets two supervisors of one name overlap. Inside a
  generation the pipeline and the transcoder retry on their own; when a
  long-lived part returns, or building fails, the supervisor rebuilds the
  generation after 1 s, doubling up to 30 s, back to 1 s once a generation
  has lived 60 s. A panic is not recovered.
- **Edits** (R-D6; M5b1's R-E2 and R-E3): `spec.Diff` gives a running stream
  one of three outcomes, with no field-by-field routing (v1's diff routing
  caused most lifecycle and config findings): `outputs` when only the
  output set changed (applied in place), `live` when the inputs or the
  watermark changed and maybe the outputs, nothing else (the supervisor
  hands the inputs to `Pipeline.SetInputs` and the watermark to
  `Transcoder.SetWatermark`, and reattaches the outputs), and `rebuild` for
  anything else (the stream's pipeline is rebuilt). `SetInputs` is applied
  between two frames and matches old and new entries by URL text, in order:
  a kept entry keeps its reader, its listen socket and its retry time and
  is never reopened, a removed one is closed, and an added one is opened as
  `Run` would open an input in that position. A kept active input stays
  active at its new index with no epoch change, while a removed active input
  is closed as at a failure, without `EventFailed`, and the first frame of
  the input that takes over is exactly one epoch change, counted as one
  switch. Between the two the hub is silent (a new pull input's connect
  time, up to the 10 s retry interval when only inputs in backoff are
  left), so an operator who wants no gap adds the new input first, waits
  for the fail-back, then removes the old one.
- **Assets** (M5b1's R-E5 to R-E7, `internal/asset`): an asset is a regular
  file directly in `server.assets_dir`, either an image (`.png`, `.jpg`,
  `.jpeg`: at most 1 MiB and 2048 px per side, decoding in full as the
  format its extension names) or a font (`.ttf`, `.otf`: at most 24 MiB,
  passing `picture.CheckFont` and, with a configured transcoder,
  `ostranscoder -probe font`, whose own verdict alone refuses a font); there
  is no metadata file and no subdirectory, so files copied in by hand are
  assets too, and the directory holds at most 256 assets and 512 MiB. A
  write is a temp file, `fsync`, rename and directory `fsync`, and the
  entity tag is the first 16 hex digits of the bytes' SHA-256.
  `GET /api/v1/assets` (viewer) lists them, each with the streams that name
  it; `GET`, `PUT` and `DELETE /api/v1/assets/{name}` read one (viewer),
  create or replace it from a raw `application/octet-stream` body with
  optional `If-Match` or `If-None-Match: *` (editor), and remove it
  (editor). A name outside the rule is 404, content the name does not allow
  422, a body over its kind's limit 413, a full directory 507, a probe that
  could not run 503; at most two uploads are read at once, each under its
  own read deadline of 30 s plus 1 s per 256 KiB of its kind's limit. A
  replacement makes every running stream that names the asset build its
  watermark again (`Manager.RefreshAsset`, through `Supervisor.Refresh`) and
  retries the streams left stopped that name it, and the answer reports
  both; a delete is refused with 409 `asset_in_use` while any stream of the
  config names the asset, and the check and the removal run under the apply
  mutex, while an upload's read, checks and probe run outside it.
- **Failover:** the supervisor switches to the next healthy input by priority.
  The switch is an epoch change and nothing else; the transcoder keeps running.
- **API** (R-D7, `internal/api`): REST under `/api/v1` on `net/http.ServeMux`
  method and wildcard patterns, with no router library; auth, the body
  limits and the request counter are hand-written wrappers. The config
  editor is `GET` and `PUT /api/v1/config` (YAML, `If-Match` required);
  `PUT` and `DELETE /api/v1/streams/{name}` (JSON) are thin wrappers over
  the same apply path. Stream URLs are redacted in every response unless an
  editor asks with `?secrets=1`, and a stream `PUT` that carries the mask is
  refused. A stream's `ETag` and a push's ID are HMAC-SHA-256 tags under a
  per-process random key, so a viewer cannot test guesses of a secret
  against them; they change when the server restarts. The OpenAPI 3.1
  document is written by hand as JSON, embedded and served at
  `/api/v1/openapi.json`; `TestOpenAPIMatchesRoutes` and
  `TestOpenAPIMatchesSpec` keep it equal to the registered routes and the
  spec's fields.

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
| Transcoding | `asticode/go-astiav` v0.41.0 on FFmpeg 8.1.3 shared (libavcodec 62), from FFmpeg-Builds (§3.5); cgo, behind the build tag `libav` (M4a) |
| Transcoder protocol messages | `google.golang.org/protobuf`, `encoding/protowire` only: no codegen and no protoc (M4a) |
| HTTP | `net/http` (`ServeMux` patterns); no router library (R-D7) |
| Metrics | our own text exposition, `internal/metrics` (R-D9) |
| Config file | `go.yaml.in/yaml/v3` (R-D2) |
| Password hashing | `crypto/pbkdf2`, standard library (R-D8) |

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
cmd/ostranscoder/      transcoder subprocess (cgo, tag libav)
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
internal/transcode/    host side of the transcoder: supervisor, epoch map, DTS merger
  wire/                length-prefixed protobuf protocol, shared with the child
  native/              the libav pipeline (cgo, tag libav): decode once, encode N rungs
  transcodetest/       fake child and fixtures for host tests
internal/spec/         the stream document: validation, redaction, ETag, Diff
internal/config/       the one YAML config file: parse, node edits, atomic writes
internal/auth/         users, PBKDF2 password hashes, the failed-login limiter
internal/asset/        the asset directory: images and fonts, checks, quota, atomic writes
internal/stream/       supervisor and manager
internal/api/          REST API and its OpenAPI document
internal/server/       the openstreamer process: listeners, wiring, the apply path
internal/metrics/      Prometheus text exposition
internal/logging/      slog with a scrubbing handler
pkg/...                our own reusable libraries (§3.7); no imports from internal/
scripts/               FFmpeg-Builds fetch, transcoder bundle, CI-peer test runner
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
| M4a ([plan](superpowers/plans/2026-11-24-m4a-transcoder-core.md), done) | Transcoder core: the segmenter fix that cuts audio at the video keyframe's PTS, `internal/transcode` (host, wire protocol, native libav pipeline, `cmd/ostranscoder`), a software H.264 ladder with AAC re-encode or copy, per-track passthrough, respawn, CI on Linux amd64 and arm64. Each rendition is served as its own stream, `<name>_<label>` | M1, M3 | A 3-rung software ladder plus a copied rendition passes M3's scenarios per rendition; the rungs' keyframes and segments align with the source's; a killed subprocess comes back as exactly one new epoch per rendition; the same on arm64 in CI. Proved by `TestM4aMatrix` (every rendition under steady-state, epoch-change, slow-consumer, no-B-frames and B-frames, plus SRT and RTSP on `t180`), `TestM4aKeyframesAligned`, `TestM4aRespawn`, `TestM4aVLC`, `TestSupervisorRealChild`, the native and supervisor suites (`internal/transcode/...`), and the `transcode-e2e-arm64` CI job |
| M4b ([plan](superpowers/plans/2026-11-25-m4b-abr-outputs.md), done) | ABR outputs: the transcoder's presentation hub with its keyframe barrier, the segmenter's `AlignVideo` (a rung that misses a segment start lists a gap), the packager's ABR mode (HLS TS variants under a multivariant playlist, the HLS fMP4 master, DASH Representations in one video set), `osconform`'s ABR rules (variants aligned, BANDWIDTH honest), osdev's `<name>_abr` | M4a | A ladder is served as one ABR presentation that passes M3's scenarios. Proved by `TestM4bMatrix` (steady-state, epoch-change, slow-consumer, no-B-frames, B-frames, and 15 fps and 10 fps sources, whose encoders trail by 2 s and 3 s of stream time, each read back over HLS TS, HLS fMP4 and DASH with the ABR rules, every frame once, discontinuities at the same media sequence number in every playlist), `TestM4bSwitchTS` (segments from alternating TS variants decode as one stream), `TestM4bRespawn` (the clip fed twice; a killed child is exactly one new epoch, at one media sequence number in every variant, opened by rung 0's IDR), `TestM4bRungGap` (a rung whose IDR a loss swallowed lists a gap, end to end), `TestM4bPlayers` (ffprobe, VLC and GStreamer; GStreamer DASH on 1.28 only, ruling R-B9), the segmenter, transcoder, packager, conform and osdev suites, and the `transcode-e2e-arm64` CI job (`TestM4bMatrix`, `TestM4bRespawn`, `TestM4bSwitchTS`, `TestM4bRungGap`) |
| M4c ([plan](superpowers/plans/2026-11-25-m4c-picture-features.md), done) | Picture features: NVENC rungs (`h264_nvenc`, chosen per rung, with a per-rung fallback to libx264), the watermark (text with time directives, or an image from the asset library) drawn before the ladder and updated live, thumbnails from the decoder for transcoded, copy-video and passthrough streams, wire version 2, osdev's `-watermark`, `-thumbnail` and `/live/<name>/thumbnail.jpg`, and the root cause of R-T7b' | M4a | On the GPU host (this machine, a GTX 1650): the same ladder with 3 NVENC rungs, run by a make target and recorded in the PR. The watermark appears on every rung; thumbnails refresh at the configured interval on transcoded and passthrough streams. Proved by `TestM4cNVENCMatrix`, `TestM4cNVENCProbe` and `TestM4cNVENCSessions` (run by `make test-nvenc`, evidence below), `TestM4cWatermarkEveryRung`, `TestM4cWatermarkABR`, `TestM4cThumbnailsTranscoded`, `TestM4cThumbnailsPassthrough` and `TestM4cThumbnailsCopyVideo`, with `TestM4cWatermarkLiveUpdate`, `TestM4cWatermarkRespawn`, `TestM4cFrozen` (no watermark, no thumbnails and x264 rungs give M4b's output), the picture, native, transcode and osdev suites, and the `transcode-e2e-arm64` CI job |
| M5a ([plan](superpowers/plans/2026-11-26-m5a-control-plane-core.md), done) | Control-plane core: the stream document (`internal/spec`), one YAML config file for server settings, users and streams (`internal/config`) with atomic rewrites and reload on SIGHUP, the per-stream supervisor with in-place output edits, the manager, the REST API with Basic auth and two roles (`internal/auth`), its whole-config editor and its OpenAPI document, Prometheus metrics (with `EncoderFallbacks`), redacted JSON logs, `cmd/openstreamer` and its config, and the product names of a ladder (R-D3) | M2, M3, M4 | A spec created over the API serves every output; editing outputs does not interrupt other outputs; a restart restores every stream. Proved by `TestM5aEveryOutput`, `TestM5aEditOutputs` (each output switched off and on while a reader of every other output must hold every hub frame once), `TestM5aConfigPut` (the same through the whole-config editor, comments kept), `TestM5aReload`, `TestM5aHandEditThenWrite`, `TestM5aRebuildIsTheOtherRoute`, `TestM5aRestartRestores`, `TestM5aLadder`, `TestM5aPeers`, `TestM5aNoSecretsLeak` and `TestM5aRoles`, with the spec, config, auth, stream, api, metrics, logging and server suites |
| M5b1 ([plan](superpowers/plans/2026-11-26-m5b1-live-edits-assets.md), done) | Live edits and assets: a running pipeline's inputs edited in place, the live watermark edit, the asset API (upload, listing, replacement; R-C8) with operator fonts (R-C7), `no_fallback` on an NVENC rung (wire version 3), and M5a's three carry items (`TestManagerConcurrentReplaces` on held sockets, error lists capped, the listener notes in the README) | M5a | Replacing a running stream's inputs is exactly one epoch change and ends no output session; an uploaded image and font appear on every rung; a strict NVENC rung fails instead of falling back and is counted. Proved by `TestM5b1ReplaceInputs`, `TestM5b1ReplaceInputsLadder`, `TestM5b1Assets` and `TestM5b1NoFallback`, with the pipeline, spec, asset, transcode, native, stream, api and server suites |
| M5b2 | Audio language: the spec field, the HLS masters' `LANGUAGE`, the MPD's `lang`, M4b's multi-audio minors | M5a | `osconform` reads the languages |
| M5c | Transport security: HTTPS for the API and the HTTP outputs, RTMPS (listen, pull, push), RTSPS and SRTP, SRT encryption (input and output), credentials in URL paths and queries redacted, secrets as write-only spec fields | M5a | ffmpeg publishes and plays over each secured protocol against our server; no secret appears in a response, a log or a metric |
| M6 | DVR and timeshift, auto-publish, templates, sessions, webhooks | M5a | Feature tests; DVR playback passes `osconform` |
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
| RTMPS (TLS) | Needs certificates and their configuration: M5c |
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
| RTSPS and SRTP | Need TLS and its configuration: M5c |
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
| SRT encryption | Needs credentials: M5c; encrypted requests are refused |
| Publishing into the SRT output listener | The SRT inputs own publishing |
| DASH placeholders | Periods carry their adaptation sets; there is nothing to align |
| Silent AAC placeholder parts instead of gaps | Backlog: silent AAC placeholder parts |
| LL-HLS changes | Not part of the M3 row |
| Play authentication and tokens | M6 sessions and tokens, as for the other outputs |
| VLC-class playback across a video-only stretch | VLC 3 and GStreamer fail on it; see the Backlog's silent AAC placeholder and VLC 3 discontinuity items |
| MediaMTX pulls of DASH and HTTP-TS | They are not MediaMTX sources; ffmpeg and the VLC-class clients cover them |

M4a's rulings are in its plan (R-M4a to R-M4d, and R-T1c to R-T7b' made while
it ran); the design is §3.5. The VLC-class check runs on `t180`'s HLS TS
(`TestM4aVLC`, with ruling R-T1c's start-up tolerance). The arm64 job runs
only ffmpeg and ffprobe as peers, so the VLC, GStreamer, SRT and RTSP rows
are amd64-only.

Out of M4a:

| Item | Reason |
|---|---|
| HLS multivariant and DASH representations | M4b (done); M4a proves each rendition as its own stream |
| NVENC, watermark, thumbnails | M4c (done) |
| macOS, Windows, VAAPI, QSV, VideoToolbox | Out of v2.0 (R-M4d) |
| libfdk-aac | FFmpeg-Builds ships none (R-M4b); the native `aac` encoder is used |
| Video encoders other than libx264 | Rungs are H.264 software; NVENC is M4c (done) |
| More than one encoded video track | Only the lowest-numbered video track is encoded or copied; the others are dropped and counted |
| More than 3 B-frames, and B-frames below about 8 fps | Beyond what D covers (R-M4c); use none |
| Merger audio on a long copy-video burst | Backlog |

M4b's rulings are in its plan (R-B1 to R-B8, and R-B9 made while it ran);
the design is §3.4's ABR
presentation and §3.5's presentation hub. Every M4b scenario records one
transcoder run per fixture through the M4a ladder with the presentation on,
requires a clean run (no loss, nothing dropped by the presentation's merger,
every epoch opened by rung 0's IDR followed by the other rungs' IDRs at its
PTS), and packages the presentation in ABR mode. Two conform rules changed
meaning while it ran, both removing false positives with a unit test that
keeps the true positive: `variant-aligned` ends a DASH segment at the earlier
of its own end (S@t + S@d) and the next S@t, since a Representation's
timeline may overlap itself (Backlog: DASH timeline overlapping itself); and
an HLS playlist whose first entry in an epoch is a gap may start late in
that epoch, since R-B8 starts that rung late by design: by the summed
EXTINF of its gaps before its first real entry, plus the usual offset
(bounded by the final review; a track starting later than its gaps declare
is flagged). `TestM4bRespawn` feeds h264_aac.ts twice at its pace, so that seven
source keyframes remain after the kill for the respawn's backoff and
start-up. The players
(`TestM4bPlayers`) run on the h264_aac.ts presentation replayed four times at
its pace into a live packager per row: ffprobe lists the four rungs and the
audio on every output; VLC 3.0.20 plays all three with M3's pass rule and
ruling R-T1c's start-up tolerance; GStreamer plays HLS TS, and DASH on 1.28.
**Ruling R-B9:** GStreamer 1.24's dashdemux2 cannot play a live DASH
window that slides, so on GStreamers before 1.28 `TestM4bPlayers`'
gstreamer/dash row skips naming the limitation, and with `OS_PEER_LIMITS=1`
probes it (a 60 s replay, a 30 s run; it fails if GStreamer still gets video
buffers in the last 10 s, or logs an ERROR). Evidence:

| Live DASH, sliding window | GStreamer 1.24.2 | GStreamer 1.28.2 | VLC 3.0.20 |
|---|---|---|---|
| Ours (the ABR presentation; also plain M3 DASH and a seamless video-only loop) | No buffer, or 225 to 308 video buffers and none after 8.2 s, in 30 s (6 probe runs); asks for fragments ahead of the live edge after each MPD update that evicts segments ("Seek to after last segment") | Passes the 12 s rule, first buffer after 4 to 6 s; over 30 to 40 s, 300 to 390 video buffers and once a 17 s jump forward | Plays (`TestM4bPlayers/vlc/dash`) |
| Reference: ffmpeg 6.1.1's dash muxer (`-window_size 5 -extra_window_size 0 -seg_duration 2 -use_template 1 -use_timeline 1`, two libx264 Representations, AAC) | No buffer in any of 4 sessions (12 s, 30 s and 40 s runs); fetched one segment per stream and stopped | No buffer in 12 s, 30 s and 40 s runs (3 sessions); "Download waiting for 28 s", then "Seek to after last segment" 14 times | Plays segments 3 to 17 in order in 25 s, no decoder error |

The reference stalls both GStreamers, while VLC plays it, so the limit is
GStreamer's live sliding-window handling, not our MPD. 1.28's long-run
behaviour on our output is in the Backlog, as a gate before M7 if the soak
uses GStreamer. The arm64 job runs `TestM4bMatrix`, `TestM4bRespawn`,
`TestM4bSwitchTS` and `TestM4bRungGap`, whose only peers are ffmpeg and
ffprobe; `TestM4bPlayers` runs in the amd64 `test` job.

Out of M4b:

| Item | Reason |
|---|---|
| AVERAGE-BANDWIDTH and FRAME-RATE in the masters | R-B5: BANDWIDTH is the window's peak; add on demand |
| I-frame playlists (`EXT-X-I-FRAME-STREAM-INF`) | Trick play is not a v2.0 goal |
| Several audio languages | Neither the masters nor the MPD carry a language (no `LANGUAGE`, no `lang`), and every TS variant muxes every audio track; language selection comes with M5b2's audio language in the stream spec |
| ABR over RTMP, RTSP, SRT and HTTP-TS | Single-rendition protocols: each rendition stays its own stream, `<name>_<label>`, and the product serves rendition 0 as `<name>` there (M5a's R-D3); R-B2 |
| LL-HLS | Not part of the M4b row, as for M3 |
| Product naming of the presentation | Done in M5a (R-D3): the product serves the presentation as `<name>` on HLS and DASH and has no `<name>_abr`; osdev keeps `<name>_abr` (R-B2) |

M4c's rulings are in its plan (R-C1 to R-C13 and R-C10a); the design is
§3.5's NVENC, watermark and thumbnail bullets, and §3.5's keyframes bullet
states the cause found for R-T7b'. The software scenarios run in CI: the
`test` job runs them all, and `transcode-e2e-arm64` runs
`TestM4cWatermarkEveryRung`, `TestM4cWatermarkABR`,
`TestM4cThumbnailsTranscoded`, `TestM4cThumbnailsPassthrough`,
`TestM4cThumbnailsCopyVideo` and `TestM4cFrozen`. libx264 does not repeat
its bytes between runs of the same input (Backlog), so `TestM4cFrozen`
compares an encoded rung by its PTS, DTS, key flags, epochs, configs and
x264 options SEI exactly and by its decoded pictures within a mean
difference of 4 of 255, and the audio and copied video by their bytes. The
runners have no NVIDIA device: the NVENC scenarios skip there with their
reason, and both transcoder jobs require the bundle's `ostranscoder -probe
nvenc` to exit 3 with an `nvenc: ` line. The GPU proof is `make test-nvenc`
on this host (R-C12): it refuses to start unless `nvidia-smi` reports no
encoder session and no compute process, runs the NVENC scenarios one after
another with `OS_REQUIRE_NVENC=1` (at most 3 sessions open), has VLC 3.0.20
in the peers image play the recorded NVENC presentation with B-frames on
(`TestM4cNVENCRecordedVLC`), and writes `dist/nvenc-report/report.md`.
ffprobe 8 does not finish reading the two-period live DASH of
h264_aac_then_640.ts (ffmpeg's DASH demuxer), so that one cell of
`TestM4cNVENCMatrix` reads DASH with `conform.ReadDASH` instead (no
violation, every frame in order with its key flag, DTS equal to PTS); every
other cell uses ffprobe. The recorded evidence is the body of
`dist/nvenc-report/report.md` from the clean-tree run at 9f13022 (Task 11's
last commit; the same tree was cherry-picked as 219414a), below, with its
level-two headings set in bold. The same target passed again, 16 of 16 with
sessions 0, 3 and 0, at a629ae8 when M4c closed.

- Date: 2026-11-25T14:55:56Z
- GPU: NVIDIA GeForce GTX 1650, 595.99.02, 4096 MiB
- FFmpeg: ffmpeg-n8.1.3-27-g8dca1586f4-linux64-gpl-shared-8.1.tar.xz
- ostranscoder: ostranscoder wire 2 libavcodec 62.28.103
- Commit: 9f13022ac32acd43bc7890c77269a2e200548acd
- NVENC sessions: before 0; during (TestM4cNVENCSessions) base=0 peak=3 after=0 (51 polls, 49 before the feed ended at 2026-11-25T14:53:52Z); after 0
- The two-period DASH cell of h264_aac_then_640.ts (TestM4cNVENCMatrix/noB no-reorder) is read with conform instead of ffprobe: ffprobe 8 does not finish reading that MPD.

**NVENC rungs (TestM4cNVENCProbe, h264_aac.ts)**

```
nvenc-rung noB t240 h264 High 320x240 has_b_frames=0 kbps=594
nvenc-rung noB t180 h264 High 240x180 has_b_frames=0 kbps=297
nvenc-rung noB t120 h264 High 160x120 has_b_frames=0 kbps=145
nvenc-rung B t240 h264 High 320x240 has_b_frames=1 kbps=601
nvenc-rung B t180 h264 High 240x180 has_b_frames=1 kbps=299
nvenc-rung B t120 h264 High 160x120 has_b_frames=1 kbps=146
```

**Results**

```
--- PASS: TestM4cNVENCMatrix (44.72s)
--- PASS: TestM4cNVENCProbe (14.39s)
--- PASS: TestM4cNVENCSessions (13.12s)
--- PASS: TestM4cNVENCMixed (7.29s)
--- PASS: TestM4cNVENCLongGOP (31.91s)
--- PASS: TestM4cNVENCWatermarkThumbnails (25.58s)
--- PASS: TestM4cNVENCKeep (27.76s)
--- PASS: TestNVENCOptions (0.00s)
--- PASS: TestNVENCFallback (0.35s)
--- PASS: TestNVENCMissingEncoder (0.06s)
--- PASS: TestNVENCRung (0.86s)
--- PASS: TestNVENCBFrames (0.84s)
--- PASS: TestNVENCMixedLadder (1.06s)
--- PASS: TestNVENCAcrossConfigChange (0.95s)
--- PASS: TestProbeNVENC (0.22s)
--- PASS: TestM4cNVENCRecordedVLC (20.34s)
```

<details><summary>Subtests</summary>

```
    --- PASS: TestM4cNVENCMatrix/noB (28.12s)
        --- PASS: TestM4cNVENCMatrix/noB/transcode (28.11s)
            --- PASS: TestM4cNVENCMatrix/noB/transcode/h264_aac.ts (6.54s)
            --- PASS: TestM4cNVENCMatrix/noB/transcode/h264_aac_then_640.ts (8.60s)
            --- PASS: TestM4cNVENCMatrix/noB/transcode/h264_aac_nob.ts (6.47s)
            --- PASS: TestM4cNVENCMatrix/noB/transcode/h264_pyramid.ts (6.50s)
        --- PASS: TestM4cNVENCMatrix/noB/ladder/h264_pyramid.ts (0.61s)
        --- PASS: TestM4cNVENCMatrix/noB/ladder/h264_aac.ts (0.66s)
        --- PASS: TestM4cNVENCMatrix/noB/ladder/h264_aac_nob.ts (0.72s)
        --- PASS: TestM4cNVENCMatrix/noB/ladder/h264_aac_then_640.ts (0.82s)
        --- PASS: TestM4cNVENCMatrix/noB/t180 (0.02s)
            --- PASS: TestM4cNVENCMatrix/noB/t180/steady-state (0.21s)
            --- PASS: TestM4cNVENCMatrix/noB/t180/no-reorder/h264_aac_then_640.ts (0.55s)
            --- PASS: TestM4cNVENCMatrix/noB/t180/no-bframes (0.22s)
            --- PASS: TestM4cNVENCMatrix/noB/t180/no-reorder/h264_pyramid.ts (0.60s)
            --- PASS: TestM4cNVENCMatrix/noB/t180/epoch-change (0.26s)
            --- PASS: TestM4cNVENCMatrix/noB/t180/no-reorder/h264_aac_nob.ts (0.63s)
            --- PASS: TestM4cNVENCMatrix/noB/t180/no-reorder/h264_aac.ts (0.58s)
        --- PASS: TestM4cNVENCMatrix/noB/src (0.01s)
            --- PASS: TestM4cNVENCMatrix/noB/src/no-bframes (0.23s)
            --- PASS: TestM4cNVENCMatrix/noB/src/steady-state (0.26s)
            --- PASS: TestM4cNVENCMatrix/noB/src/epoch-change (0.33s)
            --- PASS: TestM4cNVENCMatrix/noB/src/b-frames (1.39s)
        --- PASS: TestM4cNVENCMatrix/noB/t240 (0.02s)
            --- PASS: TestM4cNVENCMatrix/noB/t240/steady-state (0.27s)
            --- PASS: TestM4cNVENCMatrix/noB/t240/no-bframes (0.27s)
            --- PASS: TestM4cNVENCMatrix/noB/t240/no-reorder/h264_pyramid.ts (0.59s)
            --- PASS: TestM4cNVENCMatrix/noB/t240/no-reorder/h264_aac_then_640.ts (0.57s)
            --- PASS: TestM4cNVENCMatrix/noB/t240/epoch-change (0.33s)
            --- PASS: TestM4cNVENCMatrix/noB/t240/no-reorder/h264_aac.ts (0.60s)
            --- PASS: TestM4cNVENCMatrix/noB/t240/no-reorder/h264_aac_nob.ts (0.61s)
        --- PASS: TestM4cNVENCMatrix/noB/t120 (0.02s)
            --- PASS: TestM4cNVENCMatrix/noB/t120/no-bframes (0.13s)
            --- PASS: TestM4cNVENCMatrix/noB/t120/steady-state (0.13s)
            --- PASS: TestM4cNVENCMatrix/noB/t120/epoch-change (0.12s)
            --- PASS: TestM4cNVENCMatrix/noB/t120/no-reorder/h264_aac.ts (0.48s)
            --- PASS: TestM4cNVENCMatrix/noB/t120/no-reorder/h264_aac_then_640.ts (0.39s)
            --- PASS: TestM4cNVENCMatrix/noB/t120/no-reorder/h264_pyramid.ts (0.45s)
            --- PASS: TestM4cNVENCMatrix/noB/t120/no-reorder/h264_aac_nob.ts (0.46s)
    --- PASS: TestM4cNVENCMatrix/B (13.05s)
        --- PASS: TestM4cNVENCMatrix/B/transcode (13.04s)
            --- PASS: TestM4cNVENCMatrix/B/transcode/h264_pyramid.ts (6.52s)
            --- PASS: TestM4cNVENCMatrix/B/transcode/h264_aac.ts (6.52s)
        --- PASS: TestM4cNVENCMatrix/B/ladder/h264_aac.ts (0.57s)
        --- PASS: TestM4cNVENCMatrix/B/ladder/h264_pyramid.ts (0.57s)
        --- PASS: TestM4cNVENCMatrix/B/t120 (0.01s)
            --- PASS: TestM4cNVENCMatrix/B/t120/steady-state (0.20s)
            --- PASS: TestM4cNVENCMatrix/B/t120/b-frames (1.12s)
        --- PASS: TestM4cNVENCMatrix/B/t180 (0.01s)
            --- PASS: TestM4cNVENCMatrix/B/t180/steady-state (0.20s)
            --- PASS: TestM4cNVENCMatrix/B/t180/b-frames (1.18s)
        --- PASS: TestM4cNVENCMatrix/B/src (0.00s)
            --- PASS: TestM4cNVENCMatrix/B/src/b-frames (1.22s)
        --- PASS: TestM4cNVENCMatrix/B/t240 (0.01s)
            --- PASS: TestM4cNVENCMatrix/B/t240/steady-state (0.38s)
            --- PASS: TestM4cNVENCMatrix/B/t240/b-frames (1.30s)
    --- PASS: TestM4cNVENCProbe/noB (7.00s)
        --- PASS: TestM4cNVENCProbe/noB/transcode (6.52s)
            --- PASS: TestM4cNVENCProbe/noB/transcode/h264_aac.ts (6.52s)
    --- PASS: TestM4cNVENCProbe/B (6.96s)
        --- PASS: TestM4cNVENCProbe/B/transcode (6.52s)
            --- PASS: TestM4cNVENCProbe/B/transcode/h264_aac.ts (6.52s)
    --- PASS: TestM4cNVENCMixed/transcode (6.44s)
        --- PASS: TestM4cNVENCMixed/transcode/h264_pyramid.ts (6.44s)
    --- PASS: TestM4cNVENCKeep/transcode (24.82s)
        --- PASS: TestM4cNVENCKeep/transcode/h264_aac.ts*4 (24.82s)
    --- PASS: TestNVENCFallback/one_rung (0.20s)
    --- PASS: TestNVENCFallback/two_rungs (0.14s)
    --- PASS: TestNVENCBFrames/default_2 (0.31s)
    --- PASS: TestNVENCBFrames/3 (0.29s)
    --- PASS: TestM4cNVENCRecordedVLC/index.m3u8 (10.17s)
    --- PASS: TestM4cNVENCRecordedVLC/v0.m3u8 (10.13s)
```

</details>

Out of M4c:

| Item | Reason |
|---|---|
| NVENC reference B-frames (`b_ref_mode`) | R-C3: disabled always, as `b-pyramid=none` on x264 (R-T7b'); Backlog |
| The full-GPU path (cuvid, `scale_cuda`, `overlay_cuda`) | R-C2: frames stay on the CPU; needs a watermark that renders on the GPU; Backlog |
| HEVC and AV1 NVENC | Rungs encode H.264 only; add on demand |
| An operator font, and non-Latin text | R-C7: the embedded DejaVu Sans only, so CJK and Arabic shaping render as empty boxes; operator fonts done in M5b1 (R-E8) |
| Several watermarks, colours, rotation, tiling | R-C6: one watermark, white text with a black border or an image |
| A watermark on a copy rung | R-C5: `New` rejects it; a branded source resolution needs one more encoded rung |
| Thumbnail history, sprite sheets, files | R-C10: the latest shot only, in memory |
| The asset API (upload, listing, replacement) | Done in M5b1, in front of the same directory (R-C8; R-E5 to R-E7) |
| Metrics for `EncoderFallbacks` | Done in M5a: `openstreamer_transcoder_encoder_fallbacks_total{stream}` (R-D9); refusing the fallback is done in M5b1 (`no_fallback`, R-E9) |
| NVENC in CI | The runners have no NVIDIA device; `make test-nvenc` is run by hand on this host (R-C12) |

M5 runs in three parts (ruling R-D1 of the M5a plan), and M5b in two
(ruling R-E1 of the M5b1 plan). M5a is the
control-plane core and delivers the old M5 row's exit criterion: the spec,
the one config file with its users, the supervisor, the manager, the API
with its config editor, metrics (with `EncoderFallbacks`), logs,
`cmd/openstreamer`, and the product names of a ladder. M5b1, live edits and
assets: a running pipeline's inputs changed in place (`Pipeline.SetInputs`,
§3.6), the watermark edited in place over the API (`Transcoder.SetWatermark`),
the asset API (R-C8) with operator fonts (R-C7), and a rung that refuses
the x264 fallback (wire version 3). M5b2, audio language in the spec, the
HLS masters and the MPD. M5c, transport security: HTTPS for the API and the HTTP
outputs, RTMPS (listen, pull, push), RTSPS with SRTP by its own ruling, SRT
encryption (input and output), redaction of credentials in URL paths and
queries (the M2c note), and secrets as write-only spec fields. M6 and the M7
soak need only M5a; M5b2 and M5c may run beside M6. Until M5b1 an input
edit rebuilt the stream (R-D6), and assets were files the operator copied
into a directory; until M5c the Basic credentials and every stream key cross the
network in clear, so the API listens on loopback by default (R-D8).

M5a's rulings are in its plan: R-D2 (one binary, one config file), R-D3
(product names), R-D4 (how the file is loaded, written and reloaded), R-D5
(the supervisor), R-D6 (how an edit is applied), R-D7 (the API), R-D8
(auth), R-D9 (metrics), R-D10 (logs), R-D11 (validation and listen claims),
R-D12 (the proof) and R-D13 (RTMP on an ephemeral port); the design is
§3.6. Facts the code settled while it ran, beyond §3.6: a stream's `ETag`
and a push's ID (the metrics' `target` label) are HMAC-SHA-256 tags under a
per-process key (`redact.Tag`), so they change at a restart and a client
holding an old `ETag` gets one 412 and reads again. The config's `ETag`
is the plain SHA-256 of its text, which viewers see in the config status;
the text includes every user's randomly salted hash, so it cannot be
guessed. Only a real verification that succeeds clears a limiter entry,
and room among the 4096 tracked pairs is made only when a new pair is
inserted: pairs under 5 failures first, oldest first, a blocked pair
only when nothing else can go, never one with a login in flight. The API
itself refuses a stream document that carries the redaction mask; a
viewer's `?secrets=1` is 403 on every route; and a handler's panic after a
part of the answer is written aborts the connection instead of ending it as
a whole answer. `server.New` binds every listener and starts the streams
through `Manager.Load`, so a server it returns must be run. The e2e
scenarios run in `make test` and the amd64 `test` job, inside a 240 s
budget on the package's time (`make test` and `make cover` allow 20 min per
package: CI's 2-core runners are slower than a local machine and the
transcode scenarios contend for CPU, which overran the earlier 10 min); `transcode-e2e-arm64` runs `TestM5aLadder` against the arm64
bundle; the `test` job builds `cmd/openstreamer` for Linux, Windows and
macOS.

Out of M5a:

| Item | Reason |
|---|---|
| Inputs edited in place, the live watermark edit, the asset API, operator fonts, refusing the x264 fallback | Done in M5b1; until then an input edit rebuilt the stream (R-D6) and assets were files in `server.assets_dir` |
| Audio language | M5b2 |
| HTTPS, RTMPS, RTSPS and SRTP, SRT encryption, credentials in URL paths or queries, write-only secrets | M5c; until then the API listens on loopback by default (R-D8) |
| `PATCH` and per-field edits | R-D7: a stream is replaced whole by `PUT`, or the whole config |
| Histograms and per-stream session counts | R-D9; per-stream sessions are M6's |
| Applying a `server` change without a restart | R-D4: reported as `restart_required` |
| A user API, more roles, sessions or tokens instead of Basic auth | R-D8: users change only with the whole config |
| Config history | R-D4: an overwritten config is gone |

M5b1's rulings are in its plan: R-E1 (the split of M5b), R-E2 (`ChangeLive`),
R-E3 (`Pipeline.SetInputs`), R-E4 (the live watermark edit), R-E5 to R-E7
(the asset model, its API, replacement and deletion under running streams),
R-E8 (operator fonts), R-E9 (`no_fallback`), R-E10 (the proof), R-E11
(listeners fixed at start) and R-E12 (one wire bump); the design is §3.5 and
§3.6.

Out of M5b1:

| Item | Reason |
|---|---|
| Audio language | M5b2 |
| Hot-reloadable listeners | R-E11: a `server` change still needs a restart, so adding a listener and its first output takes two steps |
| Asset versions and metadata | R-E5: the directory is the model; a replaced asset is gone |
| Multipart uploads | R-E6: a raw `PUT` body; a UI uploads with `fetch` |
| A `no_fallback` rung that fails alone | R-E9: a build opens every rung or none, so one strict rung fails the whole ladder |
| Font collections and WOFF | R-E8: `.ttc` and WOFF are refused; a CJK collection needs a single-face file |
| A replace of the active input without a gap | R-E3: add the new input, wait for the fail-back, then remove the old one |

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
| Changing a running pipeline's inputs in place | §3.6; done in M5b1 (`Pipeline.SetInputs`, R-E3; in M5a an input edit rebuilt the stream) |
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
| RTMPS (RTMP over TLS) | Needs certificates and their configuration: M5c |
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
| RTSP 2.0; RTSPS and SRTP | TLS is M5c, like RTMPS; `rtsps://` is rejected with a clear error |
| UDP multicast transport; RTSP over HTTP tunnelling | Rare for pull |
| MP4A-LATM, G.711, Opus, PCM | Outside §2's codec scope; such tracks are skipped with a reason |
| H.264 packetization-mode 2; H.265 with `sprop-max-don-diff` above 0 | Interleaved, DON reordering; skipped with a reason |
| Credentials in the URL path or query (`?user=&password=`) | M5c's redaction work |
| The RTSP server | M3b |
| ONVIF | Outside M2c |

### Backlog

Items accepted for later, not yet placed in a milestone:

- **Split `internal/e2e` by milestone, or run the transcode scenarios in their
  own job.** The package alone took over 10 min on the 2-core runners (CI run
  45755846031), so `make test` and `make cover` now allow 20 min; a split
  would bring the per-package time back down. With M5b1's four scenarios
  the package took 342 s on this 8-core host, in four runs of 340 to
  342 s (local; CI figure recorded at push); R-E10 splits it before a merge
  when CI reads above 17 min.

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
  it on the `hls-fmp4`, `hls-ts` and `dash` rows, and `TestM4aVLC` on HLS TS
  (rulings R-T1c and R-T7a'). Next: investigate VLC's fMP4/DASH clock
  start, and measure DASH as HLS fMP4 was. Normal priority.
- **VLC 3 HLS-TS: R-F3 adds one deterministic clock reset at live start** (in
  `TestM3VLCClass` it lands on the loop seam of the 4× concatenated source,
  `hls/6.ts`; the 'no reference clock' pair follows in 1 of 30 runs).
  Measure with a seamless source. If the reset is real, start each TS
  segment's audio at or before the keyframe PTS (a TS-only layout; CMAF keeps
  R-F3's cut). Normal priority; before M7 if the soak uses a looping source
  with VLC.
- **Transcoder merger drops a child track it has not seen yet in an epoch**
  (`internal/transcode/merge.go`). The merger waits only for tracks already
  seen in the hub's current epoch. Since the M4a final fix wave, a burst of
  new-epoch copy frames no longer counts as stream time for the old epoch's
  hold, so late child output of an epoch the child has already delivered to
  is kept within `MergeHold` wall time (`TestMergerHoldAcrossEpochs`). What
  remains: when the copy path runs through a whole epoch, and into the next,
  before the child's first output of that epoch arrives, nothing makes the
  merger wait, and that output is dropped (`MergeDropped`). Seen only with
  unpaced input: unpaced `h264_aac_then_640.ts` with copy video and a child
  1 ms per frame drops all 282 epoch-1 audio frames. Live paced sources are
  not affected. Fix: seed each epoch's expected set with the child's audio
  tracks too, as the router already does with an encoded rung's video.
  Related (found reviewing the CI fix, not reproduced): with copy audio and
  a child respawn, the new epoch's first copied audio frame can still reach
  the hub before the child's IDR, because `merger.missing` checks only the
  hub's current epoch's expected set until the new epoch's first frame is
  written. Fix: when a candidate's epoch is above the hub's, check the union
  of both epochs' expected sets; add a copy-audio respawn subtest.
- **Root cause of VLC 3's "buffer deadlock prevented" on B-pyramid rungs
  (closed in M4c**, ruling R-T7b', Task 9:
  [notes](superpowers/notes/2026-11-25-r-t7b-root-cause.md), `Outcome: (a)`).
  It is a limit of VLC 3 reached by a conformant stream, not a defect in our
  output. At a clock reset at the live edge VLC flushes its decoders and
  re-buffers 1000 ms of PCR time; with our PCR 700 ms behind the written DTS
  (`timeOffset` − `pcrOffset` in `internal/ts/mux.go`) that feeds the decoder about 300 ms of the new segment's video, 8 access units
  at 25 fps, and with 6 frame threads libavcodec needs 8 packets for the
  second picture at reorder depth 1 and 9 at depth 2. With 8 fed and 9
  needed the line is logged; playback goes on. Proved both ways on the tree
  before the setting: on the pyramid with 5 decoder threads, or with the PCR
  400 ms behind the DTS, 0 of 16; without the pyramid with 7 threads 4 of
  16, or with the PCR 780 ms behind, 16 of 16. The earlier reading is superseded: the copied
  source has depth 2 too and passes because, with no encoder lag, its passes
  start without an audio lead, which moves VLC's reset one frame later;
  sliced threads only shortened that lead. `b-pyramid=none` (x264) and
  `b_ref_mode=disabled` (NVENC) stay. Why VLC resets at the live edge, and
  what a viewer sees at a reset, are VLC's side and stay under "Not
  determined" in the notes. Follow-ups: the PCR lead, low frame rates in
  VLC 3 and NVENC reference B-frames, below.
- **PCR lead: proposal to shorten it** (R-T7b'). In `internal/ts/mux.go`
  the written DTS is the stream DTS + `timeOffset` (126000) and the PCR is
  the stream DTS + `pcrOffset` (63000), so the lead = `timeOffset` −
  `pcrOffset`, 700 ms today; shortening it raises `pcrOffset`. A shorter
  lead feeds VLC 3 more video after a live-edge reset: one cell at 400 ms
  (`pcrOffset` 90000) gave 0 of 16 plays with the line on pyramid rungs. Not changed in M4c (controller ruling): it changes every TS
  output and narrows the A/V skew the mux tolerates, and one 16-play cell is
  its only evidence. Weigh those costs, and measure more cells, before
  taking it.
- **Low frame rates in VLC 3** (R-T7b'). At 15 fps with shipping settings
  (`b-pyramid=none`, the audio hold on) every VLC 3 play resets its clock
  once, at about 6 s, and logs "buffer deadlock prevented" (16 of 16 plays),
  since only 4 to 6 packets are fed against the 8 needed; at that reset the
  dump instance also logs about 5 pictures too late to be displayed, 544 to
  833 ms late. What a viewer sees there was not observed (no display
  capture). `TestM4bMatrix`'s 15 and 10 fps rows have no VLC play, and a VLC
  row there would fail on the line today. The lever on our side is the PCR
  lead above.
- **NVENC reference B-frames (`b_ref_mode`), once R-T7b' is understood.**
  NVENC rungs run `b_ref_mode=disabled` always (R-C3), NVENC's analogue of
  `b-pyramid=none`: reference B-frames raise the announced reorder depth as
  the pyramid does, and depth 2 needs one packet more than VLC 3's
  live-edge re-buffer feeds at 25 fps. R-T7b''s cause is known; what is not
  is a margin that would admit depth 2 (the PCR lead and low frame rates,
  above), and `b_ref_mode` itself was not measured. Turning it on gives back
  NVENC's efficiency on cards that have it, and needs the VLC rows
  (`TestM4aVLC`, `TestM4bPlayers`, `TestM4cNVENCRecordedVLC`) and a ruling
  on R-C11's depth limit.
- **Full-GPU transcode path** (R-C2). cuvid decode → `scale_cuda` →
  `h264_nvenc`, with `overlay_cuda`, so the decode, the scalers and the
  per-rung upload leave the CPU and the bus. It needs a watermark that
  renders on the GPU, and thumbnails taken from GPU frames.
- **Operator fonts for the watermark (closed in M5b1**, R-E8). The
  watermark's `font` names a font asset, uploaded through the asset API and
  probed by the child's FreeType; shaping and glyph coverage are the font's
  and the FFmpeg build's.
- **A fallback to libx264 can overload a host sized for NVENC** (R-C4):
  the overflow silently pays CPU for every rung that fell back. The count is
  exported since M5a (`openstreamer_transcoder_encoder_fallbacks_total`,
  beside `Stats.Child.EncoderFallbacks` and the child's warning). Letting a
  stream refuse the fallback is closed in M5b1 (`no_fallback`, R-E9); what
  remains is the overload on a host whose rungs allow the fallback.
- **A replaced active input leaves the hub silent until its successor
  delivers (R-E3).** A new pull input's connect time, nothing when the
  successor was already probing, up to `RetryInterval` (10 s) when only kept
  inputs in backoff are left; players see a discontinuity. The workaround is
  to add the new input, wait for the fail-back hold, then remove the old one.
- **A font that passes the probe and fails later respawns the child until
  the watermark is edited (R-E8).** A watermark the child cannot build is
  fatal (R-C9), so FreeType failing on later text crash-loops the ladder.
- **A refused ladder retries every 5 s for ever (R-E9).** On a host whose
  GPU is gone, a `no_fallback` ladder spawns, decodes to its first picture
  and exits every 5 s; `encoder_refusals` and `last_error` are the signs.
- **Hand-copied assets reach a running stream only at its next build, edit
  or API replacement (R-E7).** Nothing watches the directory.
- **HLS `MEDIA-SEQUENCE` and the DASH timeline go backwards on the same
  URL** (M5a's R-D5, R-D6). A rebuild, for an edit or a failure, and a
  process restart start a new packager, so a player that stayed on the URL
  must reload. Carry the sequence numbers and the availability start across
  generations.
- **A switch that stops serving a ladder's renditions as their own streams**
  (R-D3). Each rung is also `<name>_<label>`, which costs N packagers when
  HLS or DASH is on; a stream that serves only the presentation saves them.
- **HTTP-TS has no session counters**, so `openstreamer_output_sessions`
  lacks it.
- **A panic in one stream's goroutines takes the process down** (R-D5). The
  service manager restarts it and the config file brings every stream back;
  decide on per-stream recovery after the soak.
- **A per-stream API edit goes through a YAML node round trip** (R-D4): it
  drops blank lines, re-indents sequences, normalises the spacing of line
  comments, and loses the comments inside the edited entry and a line
  comment on its `name:`. A text-level splice of the one entry would keep
  the rest byte for byte. (A `PUT` of the document the stream already has
  writes nothing, since the M5a final fix wave.)
- **The login limiter's key includes the presented user name** (R-D8), so
  invented names can keep both verification slots busy and delay a real
  user's first (not yet remembered) login. A per-address secondary cap
  would bound it. **Raised priority: before the API is exposed** beyond
  loopback (M5c's HTTPS), not after.
- **Writers queue behind a slow apply and cannot be cancelled** (M5a final
  review). `server.apply` runs under one mutex, and a writer waiting for it
  does not watch its request's context. A teardown that hangs during a
  SIGHUP reload therefore blocks every API write until the process exits.
  Make the wait for the mutex cancellable, and bound the apply's wait for
  the supervisors.
- **`stopPackagers` runs before the View is published** (M5a final review).
  When an edit turns HLS and DASH off, `reattach` stops the packagers first
  and publishes the View after, so for some milliseconds a request still
  finds the stopped packager and gets its last playlist. Publish first.
- **Behind a reverse proxy every API client shares one IP for the login
  limiter** (R-D8): a trusted-proxy setting.
- **A listen address handed from one stream to another in one edit** (R-D11):
  delete `a` and give `b` its URLs, or rebuild `a` off a port that `b`
  takes; the new owner may bind before the old one has let go, and gets one
  transient bind error that the supervisor's retry heals.
- **The listen-claim rule compares hosts as text** (R-D11): `localhost` and
  `127.0.0.1` are different hosts to it, host names are case-sensitive, and
  the multicast exemption needs an IP literal.
- **Per-generation counters hide crash loops.** A stream's `input_switches`,
  unmapped-track and clock-drift counters, and the transcoder's series,
  start again at 0 with each generation, which Prometheus reads as a reset;
  cumulative counters per stream would show a crash loop. **Revisit before
  M7's dashboards**: a panel built on the per-generation series would have
  to be rebuilt.
- **RTMP: ffprobe sees an extra `subtitle:text` stream** when an
  `onMetaData` arrives mid-stream at an epoch change (M3 behaviour, seen by
  `TestM5aPeers`).
- **JSON stream bodies decode looser than YAML**: `encoding/json` matches
  keys case-insensitively and accepts duplicate keys, while the config file
  refuses both.
- **Transcoder: drop and count a persistently failing audio track instead of
  failing the pipeline.** An audio track the child cannot decode (or encode)
  makes the child exit; it is respawned and fails again, so one bad track
  crash-loops the whole ladder, video included. Drop that track after
  repeated failures, count it, and keep the video rungs running (M4a final
  review, M3).
- **`TestM4aRespawn`: add an HTTP-TS read** of the respawned rendition.
  The merger now waits for an encoded rung's video in every new epoch (the
  router announces it when it sends the epoch's first video frame to the
  child), so a direct output no longer gets the new child's first second
  of audio ahead of the IDR; an HTTP-TS read would pin that end to end.
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
  under soak load, and passed alone and in CI. `TestShotsFromAudioOnlyChild`'s
  respawn row (M4c, `internal/transcode`) compares two paced runs, so it is
  a flake risk under load; it has not failed yet.
- **`TestFailoverSearchSkipsRunningProbe` flake** (internal/pipeline, from
  M2a): about 1–2 % under parallel race load (7 of 320), before and after the
  arm fix; `stepTo` (failback_test.go) syncs with `Run` only by waiting for
  the readers and a 50 µs sleep, so `Run` can fall behind the fake clock. Fix:
  a handshake that `Run` is idle after each step (13 call sites); raise if CI
  hits it. M4a runs added three that failed once under full parallel load and
  passed alone: `TestM2bFailoverRTMPtoSRT`, `TestM3b1SharedPort` ("connection
  starts at hub video frame 274") and rtspplay's tests.

- **ABR: the respawn segment can raise TARGETDURATION** (M4b). While the
  child respawns, the copy rung goes on in the old epoch but rung 0, the
  encoded anchor, is silent, so the presentation's last old-epoch segment
  closes only at the epoch change or `MaxDuration`, without a cut. Each TS
  variant lists it with its own span (R-B4; in `TestM4bRespawn` rung 0's
  part held one frame and the copy's 2 s), so a variant's EXTINF and
  BANDWIDTH are honest, but when the copy rung runs past the target the
  shared TARGETDURATION of the TS variants (and the CMAF one) rises, and it
  never shrinks. Before M4b Task 7's fix the segment carried the anchor's
  short duration for every variant, which was the real defect. Fix if it
  matters: close the old epoch's segment when the anchor rung's child
  exits, or cut it at the target for the rungs still running.
- **ABR: rung 0's extra x264 IDR at `keyint=600`** (M4b). x264 places an
  IDR on its own 600 frames after the last one. A source GOP above 600
  frames therefore gives rung 0 an IDR where the source, and so the copy
  rung, has no keyframe; the segmenter cuts the presentation there, and
  R-B8 leaves every rung without a keyframe at that PTS out of the segment,
  so it lists a gap for one segment. Fix: raise `keyint` above any accepted
  source GOP, or let only the forced IDRs at source keyframes cut
  (`keyint=infinite`).
- **ABR: an encoder beyond the presentation merger's cap** (M4b final
  review, C1; ruled). The presentation merger's hold follows the encoded
  rungs' lag up to 4 × `MergeHold` (8 s). An encoder further behind in
  stream time (x264's default delay of about 30 frames below about 4 fps,
  or a longer lookahead) misses every barrier: the rungs that miss are out
  of every segment while it lasts, and when the copy rung opens the epoch
  it keeps the anchor (R-B8 (1) lets rung 0 in only at a keyframe at or
  above the open segment's Start, which a rung more than a segment late
  never has). Visible in `live.Packager.Rungs` and its warning. Fix if it
  matters: have the child report its encoder delay in Hello and size the
  hold from it, bound the encoder's delay at low frame rates
  (`rc-lookahead`, `sync-lookahead`), or delay the copy rung by the
  measured lag instead of holding it.
- **ABR: the presentation waits up to 8 s for the first epoch's rungs**
  (M4b final review, C1; ruled; narrowed in M4c). Since M4c a new epoch's
  first frame waits 1.5 × the lag learned in an earlier epoch (§3.5), so
  only an epoch with no lag learned before it waits the full 4 × `MergeHold`
  of wall time for the encoded rungs announced for it: the first epoch of a
  run, or one after every earlier track's lag was pruned, since a 10 fps
  encoder needs 3 s. A child that says Hello and is fed but never delivers
  in that epoch stalls the presentation for 8 s before the copy rung opens
  it. Fix if it matters: have the child report its encoder delay in Hello
  and size the first hold from it.
- **Low-fps A/V interleave in the rendition hubs** (M4b final review). The
  rendition mergers keep the fixed `MergeHold` (M4a is frozen). Where an
  encoded rung trails its audio by 2 s or more of stream time (15 fps and
  below), the rendition hub's audio is written ahead of its video, up to
  the encoder's delay, and can open the epoch (`requireVideoFirst` is off
  for the M4b low-fps fixtures' rendition hubs). Cost: outputs that
  interleave by arrival (HTTP-TS, RTMP, SRT) carry audio that far ahead of
  video; the packagers place audio by PTS. Fix: give the rendition mergers
  the presentation merger's lag-following hold.
- **ABR segmenter and packager, degraded paths** (M4b final review, ruled
  Backlog):
  - the take-back (`uncut` then `openAt`) drops audio below the new Start
    that the taken-back segment had kept as Late. Cost: an audio blip
    after a take-back;
  - the promote-cut path closes the segment with `duration` −1, so
    `finish` fills `VideoDurations` there although the doc says a cut
    never does. Cost: a variant EXTINF from the rung's own span at that
    one segment;
  - `buildTSMaster`'s BANDWIDTH is inflated by a one-frame variant part (a
    part's bytes over a one-frame duration). Cost: a briefly over-stated
    BANDWIDTH after a respawn;
  - a rung that leads the anchor and has no keyframe at the cut K keeps
    its frames past K in the closing segment (R-B8 (3) moves only from a
    keyframe at K) and is left out of the next. Cost: one part running
    past its segment, flagged by conform (`extinf-matches`,
    `variant-aligned`); needs a merger miss and a missing IDR at once;
  - when copy audio opens an epoch the barrier's rung-0-first step is
    skipped, so a copy IDR can precede rung 0's (R-B8 (1) and (3) catch
    it, degraded).
- **osdev tests: `free()` has a port race** (M4b final review). The helper
  closes a listener to learn a free port, which another test can take
  before osdev binds it. Cost: a rare osdev test flake. Fix: let osdev
  bind `:0` and report the port.
- **HLS fMP4 gap EXTINF after heavy loss** (pre-existing M3b3 behaviour,
  ruled outside M4b's frozen non-ABR scope). After a slow consumer on a tiny
  queue loses every audio frame of a segment, the audio playlist's gap
  entry gets EXTINF = the segment's duration, a PTS span (14400 ticks),
  while the encoded rung's part, a B-frame part cut short by the loss, has a
  DTS-sum duration (7200): conform's `gap-duration` fires. Needs B-frames, a
  segment truncated by a loss and an audio gap. Repro (M4b Task 7):
  `packABR(t, loopSteps(pres, 3), 8, &handBursts{every: 10, n: 8})`. Cost
  if left: strict validators flag a gap EXTINF off by up to a part.
- **DASH timeline overlapping itself across an in-epoch PTS jump** (ruling
  in M4b, accepted; non-ABR too). S@t is a segment's earliest presentation
  time and S@d its media's decode duration (`timeline-matches-media`
  requires that). Where an encoder reorders across a source PTS jump inside
  an epoch, the decode duration is short in the segment before the jump and
  long in the one after, so a Representation's S@t + S@d passes the next
  S@t, by up to the jump (x264 at `TestM4bRespawn`'s loop seam: 8640 ticks,
  96 ms). conform's `variant-aligned` ends a segment at the earlier of S@t +
  S@d and the next S@t. Cost if wrong: a player that does not trim at the
  next S@t could render up to the jump twice. Making S@d the presentation
  span instead would break `timeline-matches-media`; a ruling for DASH as a
  whole.
- **GStreamer on a sliding live DASH window** (ruling R-B9; found by
  `TestM4bPlayers`, not ABR-specific, and M3 had no GStreamer DASH row).
  GStreamer 1.24.2's dashdemux2 skips segments after each MPD update that
  evicts some, asks for fragments ahead of the live edge and stalls, on our
  output and on ffmpeg's dash muxer alike (`TestM4bPlayers/gstreamer/dash`
  skips on GStreamers before 1.28; `OS_PEER_LIMITS=1` probes it). With a
  window that does not slide it plays ours, but its first buffer comes 8.5
  to over 12 s in (it finds new segments only at each 2 s MPD refresh while
  joining 6 s behind the edge with a 10 s buffer). 1.28.2 passes the 12 s
  pass rule on ours (first buffer after 4 to 6 s), but over 30 to 40 s runs
  it decoded 300 to 390 video buffers and once jumped forward 17 s (PTS
  14.4 to 31.8), and it stalled on ffmpeg's reference altogether. Our MPDs
  were checked across refreshes (fixed `availabilityStartTime`,
  `startNumber` advancing with each evicted S, every S@t unchanged,
  segments listed after their availability time). **Gate before M7 if the
  soak uses GStreamer:** settle 1.28's long-run behaviour on our live DASH
  (check whether the replay's loop-seam gaps, S@t above the previous S@t +
  S@d by 96 ms for video and 80 ms for audio, explain the jump, and run it
  against a seamless source); otherwise the soak's GStreamer clients use
  HLS. VLC 3.0.20 and ffprobe play the same output.
- **libx264 output varies between runs of the same input** (M4c Task 10).
  Two transcodes of the same clip gave byte-identical t180 frames in
  150 of 150 in some runs and 5 to 60 of 150 in others, with every PTS, DTS,
  key flag and epoch equal and the decoded pictures within a mean difference
  under 1 (at most 0.99). libx264 is meant to repeat its output for the same input and
  settings with threads (its `non-deterministic` option is off), so the
  encoder's input may vary between runs (the decoder, the scaler or side
  data). `TestM4cFrozen` and the native rung tests compare encoded rungs by
  timing exactly and by decoded picture within 4 of 255 for that reason.
  Find what varies; a deterministic path would let them compare bytes.
- **Thumbnails read BT.709 sources with the BT.601 matrix** (M4c Task 6).
  The shot scaler converts the range to full but keeps the source's YUV
  samples, and a JPEG (JFIF) is decoded with the BT.601 matrix, so an HD
  (BT.709) source's thumbnail has a slight hue shift. Fix: convert the
  matrix in the scaler (`sws_setColorspaceDetails` with the frame's colour
  space as source and BT.601 as destination).
- **`make test-nvenc`: shared mode and the refusal guard** (M4c Task 11).
  `OS_NVENC_SHARED=1` (warn instead of refusing when other NVENC sessions
  exist; session assertions become "rose by 3") is untested. The opt-in
  gate is done (M4c final review): the NVENC device tests (e2e
  `testpeer.NVENC`, native `nvencOrSkip`, and `cmd/ostranscoder`'s
  `TestProbeFlag/device` and `TestChildEncoderFallbacks`, which skip only
  where `/dev/nvidiactl` exists) skip without opening a session unless
  `OS_NVENC=1` or `OS_REQUIRE_NVENC=1`, which only the script sets after
  its refusal check, so a plain `go test` or `make test` on the GPU host no
  longer bypasses the refusal.

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

#### M4a deferred minors

- wire: an empty-body Discontinuity or Stop is not rejected; unknown fixed32
  and fixed64 fields count as malformed rather than skipped; the child exits
  on an output frame the Writer refuses (`wire.ErrLimit`; not seen
  from libav's encoders).
- ostranscoder main: a dead nil branch after `os.NewFile(3)`;
  `TestMainOverFD3` waits on the child without a bound (kill it after
  `childTimeout`).
- Build: add `xz-utils` and `CGO_ENABLED` to the arm64 job's environment; the
  stub test's `.exe` suffix matters on Windows only;
  `testpeer.OSTranscoder` should use `Output()`, not `CombinedOutput()`.
- native: one skipped AAC frame leaves audio about 21 ms early until the next
  re-anchor (the threshold is more than one frame; pin it with an assertion
  or use at least one); `isCorruptData` could exclude `EINVAL` and
  `AVERROR_BUG`.
- supervisor: `New` should reject an `InputStallTimeout` below 2 s; add a
  `SlowChildNotKilled` subtest with the child's output muted, to isolate the
  Stats path; start-up catch-up can exceed `MergeHold` (consistent with the
  rule).
- segmenter: no tests pin the audio-only restriction of `toHeld`, `waitsFor`
  and `place` (a lagging second video track: no hold, `Late()` 0), nor the
  promote cut with nothing moved (the `carry` case); a non-anchor video track
  is now carried at a non-key promote (harmless).

#### M4b deferred minors

- segmenter (R-B8): no test for `belowAnchor`'s no-segment-yet branch, for
  AlignVideo's take-back with a sub-Start carry or a lost restore, nor for a
  Forced opener by a non-anchor video track; comment that the take-back
  drops Forced on purpose.
- presentation hub: the copy-audio path into the presentation is untested;
  when copy audio opens an epoch the barrier's rung-0-first step is skipped,
  so a copy IDR may precede rung 0's (R-B8 (1) catches it, degraded);
  audio before the barrier sits ahead of rung 0's IDR, where the GOP cache
  misses it (re-keying in `barrier()` needs a ruling); no end-to-end
  barrier test with per-rung timestamps in the fake child (`TestM4bMatrix`
  covers it with the real child).
- conform: in once mode an extra master read feeds the CODECS union
  (benign); the open-GOP DASH test could say why mutating frame 2 works;
  the `track-start-offset` exemption could still compare the exempt
  tracks among themselves.
- packager: an ABR variant for a track with no codec string; an all-gap rung's BANDWIDTH is the audio's
  only (fMP4) or 1 (TS); the TS variants' audio PID can shift when a lower
  audio track appears later (R-B4's rank rule); variant muxers are never
  pruned; the TS TARGETDURATION includes the segment's Duration even when
  no variant lists a gap there (it can only raise it).
- osdev: the `copy://` lookup-exclusion test lacks a positive control;
  optionally reject `copy://<ladder>_abr` at config time; the presentation
  URL list is hard-coded separately; no SRT lookup-exclusion test.
- e2e: `assertCompleteABR` joins all audio tracks (fixtures with several
  audio tracks would need per-track checks); `TestM4bSwitchTS` reruns a
  transcode `TestM4bMatrix` already does; `TestM4bRungGap` checks hls-ts
  audio by counts only.

#### M4c deferred minors

- picture: `DecodeImage`'s errors are prefixed `image:`, not `picture:`;
  `LoadAsset` does not check that the asset is a regular file;
  `NewSchedule` with an interval of 0 or less is always due (document it);
  no test for `EncodeJPEG` over `MaxShotBytes` or `ShotSize`'s zero guard;
  `TimeZone` is validated for image watermarks too, where it is unused.
- wire: the field numbers of Watermark (5, 6), Thumbnail (1, 3) and Shot
  (1, 4) are pinned only by round trips (add hex goldens); a watermark text
  with a newline would split the fake child's log line (the e2e tests use
  no multi-line text); the fake child's tests duplicate fixtures.
- merger and rtspplay tests (M4c Task 1): rtspplay's no-H.265 tag check
  misses a trailing access unit in the drop branch; one `require.Less` is
  vacuous; the drop rule's audio-only edge is a flake risk; a poller may
  outlive its test after `Fatal`; `merge.go`'s doc at line 72 is imprecise.
- native NVENC: no check for options an NVENC open leaves unused;
  `TestChildEncoderFallbacks` probes, then opens, so a session freed or
  taken between the two can flip it; the two-rungs subtest of
  `TestNVENCFallback` would panic on an `IndexFunc` of −1 instead of
  failing; High profile is not asserted with B-frames on.
- native thumbnails: in key-only mode, extra video tracks count into
  `DroppedTracks`.
- transcode host: `TestShotsFromAudioOnlyChild` proves a negative with a
  20 ms sleep; an oversize key for the Thumbnailer's child is lost silently
  after `Took`; a SetWatermark sent while the pre-Hello backlog is full
  adds 1 to `DroppedBeforeKey`; `KeysSkipped` and `ShotKeysSkipped` count
  differently (document it); `helloFailed` is missing from the plan's list
  of shared helpers.
- osdev: `If-Modified-Since` has 1 s resolution, so a newer shot in the same
  second can get 304; the thumbnail counters are printed only when not zero;
  the usage text says opacity is 1-100 while 0 means the default; "until the
  stream ends" in the doc, while the last shot stays served; the 404 before
  the first shot of `TestThumbnailHTTP` is proved only by a unit test;
  `TestThumbnailFromChild` waits 300 ms for a Stats (flake risk).
- e2e: the watermark's text band check scales with the picture width.

#### M5a deferred minors

- metrics: the doc should say that label values come from configuration
  only, and that collectors may run concurrently; `metricstest.Parse` has
  no tests of its own (negatives) and its scope (our own output only) is
  undocumented; the `__` prefix and duplicate-label checks are untested;
  `process_open_fds` counts the directory descriptor that reads it (one
  too many).
- logging: scrubbing is tested in the JSON format only; a URL password
  with a literal space leaves its tail unscrubbed (such a URL is invalid
  anyway); the doc should say that struct fields are not scrubbed by name
  (a `Password` field in a URL-less struct passes).
- spec: `redact.Masked` refuses any RTMP stream key that ends in the mask
  (a false positive); ~~a full
  `layout` list (`video` and `audio`) is not normalised to the empty one, so
  that edit rebuilds needlessly~~ (done in M5b1, R-E2).
- hub: `Refuse("")` refuses everything and is unguarded (no caller passes
  it); `TestRefuseRace` asserts only that nothing panics.
- auth: `TestLimiterRefusedReserveEvictsNothing` passes trivially.
- config: the branch of a real directory `fsync` failure is untested; the
  same fault is a
  `SyntaxError` (400) under `server` or `users` but `Errors` (422) inside a
  stream; a FIFO at the
  config path hangs `Open`; the `Unwrap` of its errors relies on how
  `spec.Validate` wraps.
- stream: a thumbnail-only child reports PID 0; an 8-hex push ID collision
  would make the scrape fail with 500; ~~a stale input error may survive an
  edit that clears it~~ (done in M5b1); `serve` drains the whole mailbox before it checks
  its context; the manager takes two
  snapshots per scrape; the claim normalisation lacks tests for a reserved
  `::` and for `localhost`.
- api: `openapi.json` was generated by a script that is not committed (the
  tests pin the result); a `Verify` that ends with the request's context
  counts as a 503 and inflates that series.

**Note for the user:** make the `transcoder-amd64`, `transcoder-arm64` and
`transcode-e2e-arm64` CI jobs required checks on the `rebuild` and `main`
branches. That is a repository setting, which no commit can change.

#### M5b1 deferred minors

- spec:
  - an image error is reported before a font's "needs text".
- pipeline:
  - other `stepTo` tests may share the fake-clock timer race (unaudited);
  - an edit during the layout wait (nil `Layout` only) parks a moved
    attempt until `layoutAt`;
  - `SetInputs` before `Run` opens the original inputs first;
  - test gap: removing an active passive input (verified correct by
    overlay in the final review);
  - test gaps: duplicates with one copy active, and `SetInputs` under a
    paced active input;
  - `SetInputs(nil)` after `Run` returns "no input" rather than
    `ErrNotRunning`.
- picture:
  - seed `FuzzCheckFont` with the fixture, an `OTTO` and a 12-byte header;
  - `CheckFont` accepts overlapping or duplicate tables (directory only).
- native and cmd/ostranscoder:
  - the NVENC opt-in gate exists in four copies;
  - the child's own `transcode: set watermark: watermark: …` doubles the word, and reaches `last_error` on a child temp-dir or font-write failure.
- transcode host:
  - a test note on the wire carriage of `NoFallback`;
  - control characters in a child's fatal message are not stripped;
  - `TestRefusalCounted` takes zero `FramesOut` as "no software frame".
- asset:
  - `Delete`'s doc should say an `ErrWrite` can follow the remove;
  - Windows reserved names (`con.png`) are allowed.
- stream, live edits:
  - comment the window of the `input ` prefix clear (in `applyLive`);
  - a successful `SetInputs` does not clear an `inputs: ` error;
  - `applyWatermark` logs and returns;
  - no test of a watermark failure and an inputs success in one edit;
  - `plan` calls `spec.Diff` twice.
- stream, assets:
  - `Refreshed.OK`'s comment: the transcoder took the bytes, not the child;
  - the concurrent test should count outcomes per kind;
  - no scrub test, no stopped-at-load `AssetUsers` row;
  - the watermark log is not per child;
  - `loaded_at` moves on a retry (consistent with `PutStream`);
  - a refresh answers ok when the spec has no watermark (harmless).
- stream, test helpers (`leases`):
  - the `held()` comment overstates;
  - the initial create is not routed through the `inUse` watch;
  - the port-prefix heuristic;
  - `Live()` lags the holder briefly.
- api:
  - a duplicate `capped` helper (config has one);
  - no slot-wait timeout test, no guard that `WriteTimeout` exceeds 30 s;
  - a waiter on a real connection misses its client's hang-up;
  - the deadline tests' margins (base 2 s);
  - release upload slots in `t.Cleanup`;
  - `ErrTooLarge` on `GET` and `DELETE` (404 or 409?);
  - a `MaxBytesReader` comment;
  - no test that a panic releases the slot.
- testpeer:
  - filter the peer list by an untagged `go list`, not a guard test.

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
- ABR presentations (M4b). `variant-aligned`: in HLS, with
  `ReadOptions.AlignedSequences` (`osconform -aligned`), the first video
  frame of a media sequence number has the same PTS in every playlist that
  carries video there; a media sequence number is compared only when every
  one of its video entries starts with a keyframe, and skipped otherwise
  (a Forced segment). In DASH, always (the rule is not gated by
  `AlignedSequences`): no segment of a video Representation contains
  another's segment start strictly inside it, a segment ending at the
  earlier of S@t + S@d and the next S@t (with `segmentAlignment` the
  timelines must be equal). `variant-bandwidth` (HLS, with
  `AlignedSequences`): a variant's measured peak bitrate, with its default
  audio rendition, is at most 1.1 times its BANDWIDTH, checked against the
  final master. A playlist whose first entry in an epoch is a gap may
  start late in that epoch by the summed EXTINF of its leading gaps there,
  plus the usual track start offset; the other tracks are not measured
  from it.

## 6. Risks

| Risk | Mitigation |
|---|---|
| Two systems in parallel for months | v1 receives only critical fixes the user asks for; v2 has a fixed milestone order |
| Demuxing every TS input costs CPU | Measured in M2 against v1's 4 cores for 10 streams; demuxing is the same work v1's tsnorm already does |
| Our own RTMP, RTSP and HLS libraries take time and miss real-world quirks | Each is test-first against recorded sessions and live peers (ffmpeg, OBS, MediaMTX, VLC); M2 and M3 are sized for them; the protocol RFCs and specs are the reference, not another product's code |
| `pkg/h26x` DTS-from-POC is subtle (B-pyramids, H.265 open GOP) | Checked against the DTS that ffmpeg writes for the same fixtures |
| Scope creep | §2 is the scope; anything else is a new milestone |
