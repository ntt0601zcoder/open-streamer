# M2a TS-Family Inputs and Failover Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ingest live MPEG-TS from file, UDP (unicast, multicast, RTP-wrapped), SRT (caller and listener), HTTP-TS pull, HLS pull (TS and fMP4 segments, with a separate audio rendition) and `copy://`, with input failover by priority, so that every input fed by ffmpeg yields violation-free frames and a failover is exactly one epoch change.

**Architecture:** `internal/ingest` defines one `Reader` interface (frames in source time with their arrival time, `ErrDiscontinuity` for timeline breaks), parses input URLs into a `Spec`, and opens readers. Every TS-carrying transport feeds the one TS demuxer, which gains PMT-change remapping, broadcast PES handling and H.264 open GOP; fMP4 segments go through a new fMP4 demuxer. `internal/pipeline` becomes the per-stream ingest loop: it runs the highest-priority healthy input, detects start and stall failures with an injectable timer, retries with backoff, probes higher-priority inputs for a held fail-back, maps reader tracks onto the stream's fixed track layout, and turns every switch into one `Clock.Break`. The Clock gains three carried fixes (late-joining tracks, staggered config changes, drift re-anchoring for live sources).

**Tech Stack:** Go (`go 1.26.11` in `go.mod`), `Eyevinn/mp4ff` v0.52.0, `datarhei/gosrt` (latest v0.x, added in Task 11), `net`, `net/http`, `math/bits` from the standard library, `stretchr/testify`. ffmpeg (with the `srt` and `udp` protocols) and ffprobe as test peers.

**Spec:** [docs/REBUILD_PLAN.md](../../REBUILD_PLAN.md) §2 (Inputs row), §3.2, §3.6 (Failover), §3.7, the M2 row of §5 and "Carried from M1". M2 is split like M3 (Task 19 writes this into the spec):

| Part | Contents |
|---|---|
| **M2a (this plan)** | TS-family inputs, HLS pull, `copy://`, failover, TS demuxer completion, fMP4 demuxer, HE-AAC in the TS muxer's ADTS writer, Clock carry-overs, CPU measurement |
| M2b | `pkg/unwrap`, `pkg/rtmp` (handshake, chunk stream, AMF0, server and client sessions, FLV, enhanced RTMP), RTMP push and pull inputs |
| M2c | `pkg/h26x` (DTS from POC, which also fixes the reordered multi-AU PES case ruled in Task 3), `pkg/rtsp` client (TCP interleaved and UDP, RFC 3640 AAC, pion/rtp depacketisers, pion/sdp), RTSP pull input |

M3b (RTMP play and push, RTSP server, SRT output) follows M2b and M2c. M2a's exit criteria are the M2 row's, restricted to M2a's inputs: each fed by ffmpeg produces violation-free frames; failover produces exactly one epoch change; a PMT change is one epoch change; an H.264 open-GOP feed starts video at its first recovery point.

**Out of M2a, with reasons** (each becomes a ledger ruling at dispatch): source-specific multicast (232/8 needs `golang.org/x/net`; revisit on demand); H.264 intra-refresh (GDR) feeds, which never produce a `Key` (needs decoder-side recovery tracking; revisit on demand); encrypted HLS (`EXT-X-KEY` other than `NONE` is refused); packed-audio HLS renditions (`.aac` segments with ID3 timestamps); changing a running pipeline's inputs in place (§3.6; M5's supervisor adds `SetInputs`); live-source epochs placed by the chain lead (R10) stay as ruled, bounded and non-accumulating.

## Global Constraints

- Every Global Constraint of the M1 and M3a plans still holds. Goroutines are also allowed in `internal/ingest/...`; each stops when its context is cancelled or its reader is closed, verified under `-race`. Goroutine-count checks never run in `t.Parallel` tests.
- Module dependencies: the standard library, `Eyevinn/mp4ff`, `stretchr/testify`, and `datarhei/gosrt` from Task 11. Only independent libraries; never `bluenviron/*` or `q191201771/lal`, and never copy their code. `pkg/...` never imports `internal/...`.
- Readers emit frames in **source time**: 90 kHz, unwrapped, possibly negative, per-track DTS non-decreasing within one source timeline, `Epoch` zero. Each frame carries the time the reader received the bytes that completed it. Only the Clock maps frames to stream time.
- A reader reports a timeline break it knows about (HLS `EXT-X-DISCONTINUITY` or sequence gap, file loop, `copy://` loss or epoch change, a passive reader's new publisher) by returning `ingest.ErrDiscontinuity` from `Read` once, then continuing. The pipeline answers it with exactly one `Clock.Break`.
- `Open(ctx, …)`: `ctx` bounds only the open and handshake (up to the first PMT or init segment); cancelling it aborts a blocked open by closing the underlying connection or listener. The returned reader lives until `Close`, independent of that `ctx`. `Env.HTTP` sets `Transport.ResponseHeaderTimeout` and dial timeouts, never `Client.Timeout`.
- `Config` pointers are stable: a reader that rebuilds a `TrackConfig` equal (`TrackConfig.Equal`) to the previous one for that track returns the previous pointer, across segments, demuxer instances and file loops (carried M1 item).
- Stream track layout: fixed for the life of a pipeline run, from `Options.Layout`, or else input 0's `Kinds()` (waiting up to `StartTimeout` for input 0 before taking the first input that opens). Reader tracks map onto it by (kind, ordinal): the k-th video track of the reader is the k-th video track of the layout. Unmapped reader tracks are dropped and counted.
- Defaults: `StartTimeout` 10 s (open until the first frame), `StallTimeout` 4 s (time Run waits on the reader with no frame, excluding pacing sleeps; a reader's `Spec().StallTimeout` raises it), retry backoff 0.5 s doubling to `RetryInterval` 10 s, `FailbackHold` 10 s, UDP socket read buffer 4 MiB (effective size reported), max PES 8 MiB, total buffered PES 32 MiB per demux, max tracks per TS demuxer 16, Clock `MaxDrift` 2 s (above `media.MaxAnchorLead` = 1 s) for live inputs, disabled otherwise.
- Errors are wrapped `fmt.Errorf("pkg: op: %w", err)`; never log and return the same error. URLs in errors and logs have credentials and `passphrase` redacted.
- Peer-dependent tests use `internal/testpeer` (Task 7): they skip when the binary or protocol is missing, unless `OS_REQUIRE_PEERS=1`, in which case they fail. CI sets it from Task 7 on.
- Input URL schemes. `ingest.Parse` returns the URL's `Spec` without I/O; the pipeline uses it only for `Passive` (needed before open). An open reader's `Spec()` is the effective spec (an HTTP URL sniffed as HLS becomes paced; HLS's `StallTimeout` comes from the playlist), and the pipeline takes `Pace`, `Live` and `StallTimeout` from it:

| URL | Input | Spec |
|---|---|---|
| `file:///abs/path.ts?loop=1&pace=0` or a bare path | file; loop optional; paced unless `pace=0` | Pace (default), not live |
| `udp://@:5000`, `udp://239.1.1.1:5000?iface=eth0` | UDP listen; multicast join when the host is a multicast group; RTP-wrapped TS detected and unwrapped | Live, Passive |
| `srt://host:port?mode=caller&streamid=…&passphrase=…&latency=120` | SRT caller (pull); `mode` defaults to `caller` when a host is given; `latency` in milliseconds (ffmpeg's is in microseconds) | Live |
| `srt://:port?mode=listener&streamid=…` | SRT listener; one publisher at a time; the next publisher is accepted after a disconnect | Live, Passive |
| `http(s)://…` that is not HLS | HTTP-TS pull | Live |
| `http(s)://….m3u8`, or `http(s)` answering an HLS content type or a body starting `#EXTM3U` | HLS pull | Pace, Live; `StallTimeout` = 1.5 × target duration, set by `Spec()` after open (`Parse` returns 0) |
| `copy://<stream>` | another stream's hub, in process | Live |

## Review Focus

1. **A flapping primary** (up 3 s, down 3 s, repeatedly) while a backup is healthy. Expected: over 120 s, exactly one switch (to the backup) and no fail-back, because the primary never stays healthy for `FailbackHold`; every switch is exactly one epoch. Test: `TestFailoverFlappingPrimary` (Task 10).
2. **Inputs with different track sets** (primary video+audio, backup video only, backup opening first while the primary is down). Expected: the layout comes from configuration or input 0, never from whichever input opened first; absent tracks stop; extra tracks are dropped and counted in `Stats.Unmapped`; `RuleStartOffset` holds inside each epoch. Tests: `TestPipelineLayoutWaitsForInput0` (Task 8), `TestFailoverTrackMismatch` (Task 9).
3. **A connected input that goes silent** (TCP, HTTP or SRT open, no bytes). Expected: failover within `StallTimeout` + 1 s, the stalled reader closed and its goroutine gone. Test: `TestPipelineStallFailover` (Task 9).
4. **A publisher that reconnects or starts late** (an SRT encoder restarting; a listener-only stream whose encoder connects 30 s after start; a UDP sender pausing 2 s). Expected: the listener never closes; an SRT reconnect is one `ErrDiscontinuity` and one epoch; a UDP pause continues in the same reader and the Clock decides (one epoch if the timestamps jump, none if they continue); no outage beyond the reconnect itself (no 10 s retry wait). Tests: `TestSRTListenerReconnect` (Task 11), `TestPipelinePassiveReconnect`, `TestSinglePassiveStartsLate` (Task 9).
5. **A live source that starts with a burst** (`copy://` replaying the hub's GOP, HTTP-TS from a GOP-caching server, a fail-back probe's buffered frames). Expected: no drift break and no dropped frame from the burst. Tests: `TestDriftBurstStart`, `TestDriftFastAfterBurst` (Task 6), `TestPipelineBurstNoDrift` (Task 8), `TestM2aEveryInput` `copy://` and HTTP-TS rows (Task 19).

---

### Task 1: `pkg/mpegts` recoverable timeouts and bounded PES buffering

**Files:** Modify `pkg/mpegts/reader.go`, `pkg/mpegts/demux.go`; Test `pkg/mpegts/reader_test.go`, `pkg/mpegts/demux_test.go`

**Interfaces — produces:**

```go
type DemuxOptions struct {
    MaxPES   int // one PES; 0 means 8 << 20
    MaxTotal int // all partial PES of this demux; 0 means 32 << 20
}
func NewDemuxWithOptions(r io.Reader, opt DemuxOptions) *Demux
// DemuxStats gains:
LostPES uint64 // PES dropped: continuity gap, oversize, total cap, or bad header
```

- [ ] **Step 1: Write the failing tests**
  - `TestReaderTimeoutIsNotSticky`: a reader whose `Read` returns a `net.Error` with `Timeout() == true` once, then data. `Next` returns that error (`errors.As` to `net.Error`, `Timeout()` true), and the next `Next` returns the following packet. Any other error still ends the Reader for good.
  - `TestDemuxMaxPES`: a length-0 video PES growing past `MaxPES: 1 << 20` is dropped, `LostPES == 1`, and the next PES on the PID is delivered whole.
  - `TestDemuxMaxTotal`: three PIDs each holding a 600 KiB partial PES with `MaxTotal: 1 << 20`; the PES that crosses the cap is dropped and counted; the other two are delivered whole.
  - `TestDemuxLostPESOnCCGap`: a CC gap mid-PES increments `LostPES` and `CCErrors`, and the next PES carries `Discontinuity`.
- [ ] **Step 2: Run** `go test -race ./pkg/mpegts/` — FAIL.
- [ ] **Step 3: Implement.** In `fill`, a timeout error is returned for this call and not stored; the partial buffer is kept. `NewDemux` keeps its signature and uses the defaults. Fix the "any read error ends the Reader" doc sentence.
- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit** `fix(mpegts): recover from read timeouts and bound PES buffering`

### Task 2: `internal/ts` PMT-change remapping and a track cap

**Files:** Modify `internal/ts/demux.go`; Test `internal/ts/pmt_test.go`; Modify `testdata/fixtures/gen.sh`, add `testdata/fixtures/h264_aac_pmtchange.ts`, `testdata/fixtures/h264_aac_audiopid.ts`

**Interfaces — produces:**

```go
type DemuxOptions struct {
    Decoder   *mpegts.TimeDecoder
    FollowPMT bool // unchanged semantics: new PIDs append tracks (conform's reader)
    Remap     bool // ingest: a PMT whose stream set changed re-assigns tracks by (kind, ordinal)
    MaxTracks int  // 0 means 16; streams beyond it are skipped with a reason
}
// DemuxStats gains:
PMTChanges uint64 // PMTs whose (PID, stream_type) set differed from the current one
```

Remap rule: track index = (kind, ordinal) slot fixed at the first PMT, in PMT order. A later PMT whose (PID, stream_type) set differs maps each supported stream to the slot of the same kind and ordinal, appending a slot when the ordinal is new (up to `MaxTracks`). Only slots whose (PID, stream_type) changed start fresh (no parameter sets; a new `Config` when they arrive); a slot whose PID and type are unchanged keeps its state. Slots with no stream in the new PMT produce nothing. `TimeDecoder` is kept. A PMT with an identical set changes nothing. `FollowPMT` and `Remap` together are a constructor error.

- [ ] **Step 1: Fixtures.** In `gen.sh`: `h264_aac_pmtchange.ts` = `h264_aac.ts` followed by a 3 s clip with the same codecs muxed with `-mpegts_start_pid 0x200 -mpegts_pmt_start_pid 0x1100` (all PIDs new) and `-output_ts_offset 6`, so timestamps continue across the change and only the PMT signals it; `h264_aac_audiopid.ts` = `h264_aac.ts` followed by a 3 s clip whose video PID is unchanged and audio PID differs (`-streamid 1:0x120` or equivalent). Regenerate twice and confirm the bytes do not change.
- [ ] **Step 2: Write the failing tests**
  - `TestRemapPMTChange`: the full-change fixture with `Remap: true`; `NumTracks() == 2` throughout; `PMTChanges == 1`; frames after the change carry new `Config` pointers on both tracks; all frames through `clock.New(...)` give exactly 2 epochs and `conform.CheckFrames` reports nothing.
  - `TestRemapAudioPIDOnly`: the audio-PID fixture; video keeps its `Config` pointer and no video frame is dropped across the change; audio restarts.
  - `TestRemapIdenticalPMT`: a PMT repeated with a new version but the same set changes nothing.
  - `TestMaxTracks`: a synthetic PMT with 20 AAC streams gives 16 tracks and 4 `Skipped()` entries naming the cap.
  - `TestRemapFollowPMTExclusive`: both options → error.
- [ ] **Step 3: Run** `go test -race ./internal/ts/` — FAIL.
- [ ] **Step 4: Implement.** Correct the `FollowPMT` doc: PIDs a later PMT removes stop producing frames.
- [ ] **Step 5: Run** — PASS.
- [ ] **Step 6: Commit** `feat(ts): remap tracks on a PMT change and cap the track count`

### Task 3: `internal/ts` broadcast PES layouts and lost-frame counting

**Files:** Modify `internal/ts/demux.go`; Test `internal/ts/broadcast_test.go`

**Interfaces — produces** (`DemuxStats` gains):

```go
LostFrames        uint64 // access units not emitted because PES data for them was lost or dropped (mpegts LostPES or Discontinuity)
SplitAUs          uint64 // access units found after the first one inside one PES and given extrapolated timestamps
SplitAUsReordered uint64 // such access units dropped because the PES had PTS != DTS
Continued         uint64 // video PES without PTS appended to the previous access unit
```

Rules:
- **Video PES without PTS** continues the previous access unit: its NAL units are appended, so a video access unit is emitted only when the next PES with a PTS arrives, on a PMT event, or at EOF. If such a PES contains an access-unit start (AUD, or a first slice of a new picture), the access unit starting there gets DTS = previous DTS + the track's last DTS delta (3600 if none) and PTS = DTS when the track's last PES had PTS == DTS; otherwise it is dropped and counted in `SplitAUsReordered`.
- **Several access units in one video PES** are split on AUD, or, without AUD, on a slice with `first_mb_in_slice == 0` (H.264) or `first_slice_segment_in_pic_flag` (H.265) after a previous slice. The first gets the PES timestamps. Later ones get extrapolated timestamps as above when PES PTS == DTS (`SplitAUs`), and are dropped and counted (`SplitAUsReordered`) otherwise. Ruling at dispatch: M2c's `pkg/h26x` POC derivation can recover them.
- **Audio PES without PTS** continues from the previous frame's PTS plus 1024 samples per frame (sample-count arithmetic).
- **PMT events flush:** before a remap (Task 2), every pending access unit is emitted with its old `Config`.
- **Loss:** a PES that arrives with `Discontinuity` first drops the pending access unit of its PID (counted in `LostFrames`), and a continuation PES (no PTS) after a loss is dropped too. Frames after the lost one pass through (decoders conceal; ruling at dispatch: dropping video to the next keyframe on every loss would freeze lossy feeds).

- [ ] **Step 1: Write the failing tests** (TS built in-test with `pkg/mpegts.Writer`):
  - `TestVideoPESWithoutPTSContinues`: one access unit over 3 PES (only the first with PTS) → one frame with all NAL units, `Continued == 2`.
  - `TestVideoPESManyAUs`: 3 access units in one PES with PTS == DTS, with AUDs → 3 frames, DTS stepping by the delta, `SplitAUs == 2`; the same without AUDs split on first slices; the same with PTS != DTS → 1 frame, `SplitAUsReordered == 2`.
  - `TestAudioPESWithoutPTS`: an ADTS PES without PTS after one with PTS → timestamps continue by 1920 ticks per frame at 48 kHz.
  - `TestLostFramesCounted`: a CC gap inside a video PES → `LostFrames == 1`, the next access unit is delivered.
  - `TestPMTFlushesPending`: a pending access unit before a PMT change is emitted with the old `Config` before any new-PMT frame.
  - The existing fixture tests pass with identical frame counts (at EOF `Read` flushes the pending unit).
- [ ] **Step 2: Run** `go test -race ./internal/ts/` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — PASS, plus `go test -race ./internal/e2e/` unchanged.
- [ ] **Step 5: Commit** `feat(ts): handle broadcast PES layouts and count lost frames`

### Task 4: Shared NAL classification and H.264 open GOP

**Files:** Create `internal/media/nal.go`; Modify `internal/ts/demux.go`; Test `internal/media/nal_test.go`, `internal/ts/opengop_test.go`, `internal/hub/opengop_test.go`; Modify `testdata/fixtures/gen.sh`, `testdata/fixtures/README.md`, add `testdata/fixtures/h264_opengop.ts`, `testdata/fixtures/h264_opengop_midgop.ts`

**Interfaces — produces:**

```go
// NALClassifier tracks one video track's key and RASL state across access units.
type NALClassifier struct{ /* codec, last recovery-key PTS */ }
func NewNALClassifier(c Codec) *NALClassifier
// Classify returns Key and RASL for one access unit (NAL units without start codes) presented at pts.
func (c *NALClassifier) Classify(nalus [][]byte, pts int64) (key, rasl bool)
```

Rules: H.265 keys (IDR, CRA, BLA) and RASL (types 8, 9) as today. H.264 IDR is `Key`. An H.264 access unit whose slices are all I slices (`slice_type` 2 or 7, read after `first_mb_in_slice`) and which carries a recovery point SEI (payload type 6) with `recovery_frame_cnt == 0` is `Key`; until the next `Key`, frames whose PTS is below that key's PTS are `RASL`. Segments that start at an open-GOP I frame therefore carry RASL frames exactly as H.265 CRA segments do today: fMP4 sample flags and `INDEPENDENT-SEGMENTS` behave unchanged. Task 14's fMP4 demuxer uses the same classifier.

- [ ] **Step 1: Fixtures.** `h264_opengop.ts`: 6 s, `-x264-params open-gop=1:keyint=25:min-keyint=25:scenecut=0 -bf 2`, AAC; `h264_opengop_midgop.ts`: cut at 3/8 like `h264_aac_midgop.ts`. Verify with `ffmpeg -i h264_opengop.ts -c copy -bsf:v trace_headers -f null -` that every keyframe after the first is a non-IDR I slice with a recovery point SEI; record the command and the observed recovery-point PTS values in `README.md`.
- [ ] **Step 2: Write the failing tests**
  - `TestClassifyH264OpenGOP` (`internal/media`, synthetic NAL units built in-test): an I slice + recovery SEI is `Key`; a following frame with lower PTS is `RASL`; one with higher PTS is not; an I slice without SEI is not `Key`; `recovery_frame_cnt == 2` is not `Key`.
  - `TestOpenGOPKeys` (`internal/ts`): on `h264_opengop.ts`, 6 `Key` frames at the recorded PTS values, leading B frames after each non-first key are `RASL`.
  - `TestOpenGOPMidStart`: `h264_opengop_midgop.ts` through demux → Clock → `conform.CheckFrames` with no violation; the first released video frame's source PTS equals the first recovery point after the cut (from the README table).
  - `TestOpenGOPHubResume` (`internal/hub`, synthetic H.264 frames with `Key`/`RASL` set as the classifier would): a slow subscriber resumes on the recovery key and its RASL frames are dropped.
- [ ] **Step 3: Run** `go test -race ./internal/media/ ./internal/ts/ ./internal/hub/` — FAIL.
- [ ] **Step 4: Implement** (SEI parsing removes emulation-prevention bytes and walks `payload_type`/`payload_size`; exp-Golomb reader for the slice header). `internal/ts` calls the classifier instead of its own `isKeySlice`/`isRASL`.
- [ ] **Step 5: Run** — PASS.
- [ ] **Step 6: Commit** `feat(ts): start H.264 open-GOP feeds at recovery points`

### Task 5: Clock — late-joining tracks and staggered configuration changes

**Files:** Modify `internal/clock/clock.go`; Test `internal/clock/join_test.go`

**Interfaces — produces:** `func (c *Clock) Break(newInput bool)` (was `Break()`; callers updated), and `Stats` gains `LateJoins, LateTracks uint64`.

Rules:
- **Late join:** a track with no frame in the current epoch whose first frame arrives after the epoch was released opens a new epoch at the next video keyframe (audio-only streams: at once). Tracks already running keep emitting into the released epoch until that keyframe, so no GOP is lost. Each track triggers at most one late-join epoch per pipeline input (the count resets on an input switch, which the pipeline signals with `Break(true)`, not on a loop or HLS discontinuity `Break(false)`); its late-join hold for later epochs is raised to its observed lateness + 200 ms, capped at `VideoWait`, and survives every `Break`. Lateness is in source ticks, from the epoch's keyframe DTS to the late track's first DTS. A track later than the raised hold joins its epoch late without opening another, counted in `LateTracks` (its start offset is accepted).
- **Config-change epochs hold the full window:** an epoch opened by a configuration change does not release early when every track has a frame; it holds `AlignWindow` source ticks from the keyframe. A configuration change on another track inside that window joins the pending epoch, and that track's old-config frames are dropped from it. A change after release opens a new epoch.
- **Old-config audio after the new key:** audio frames still carrying the old config, presented at or after the new epoch's keyframe, are kept when that track's config does not change within the window.
- **Accepted, not changed** (ruling at dispatch): with a constant inter-track skew above `MaxBackJump`, a track joining at start-up still loses one GOP once.

- [ ] **Step 1: Write the failing tests**
  - `TestLateJoinOpensEpoch`: video alone for 3 s, then audio appears → exactly 2 epochs; epoch 2 starts on a keyframe with audio within 40 ms; no video frame is dropped between the audio's arrival and that keyframe; `CheckFrames` with no skipped rule reports nothing; `LateJoins == 1`.
  - `TestLateJoinPersistentSkew`: audio arrives 1.5 s after video (arrival order) for 60 s → at most 2 epochs; `CheckFrames` clean.
  - `TestStaggeredConfigChange`: video config changes at a keyframe, audio config changes 4 frames later → exactly one new epoch, no video frame of that GOP dropped.
  - `TestOldConfigAudioKept`: video config change only; audio with PTS ≥ the key's PTS is in the new epoch.
  - All existing clock and `internal/e2e` tests still pass.
- [ ] **Step 2: Run** `go test -race ./internal/clock/` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — PASS (`./internal/clock/ ./internal/e2e/`).
- [ ] **Step 5: Commit** `fix(clock): start late tracks and staggered changes in one epoch`

### Task 6: Clock — arrival times and drift re-anchoring for live sources

**Files:** Modify `internal/clock/clock.go`, `internal/media/anchor.go` (doc only); Test `internal/clock/drift_test.go`

**Interfaces — produces:**

```go
func (c *Clock) PushAt(f media.Frame, arrival time.Time) []media.Frame // Push is PushAt(f, Now())
func (c *Clock) SetDrift(max time.Duration, positiveOnly bool)         // applies to the pending epoch when one is pending (always at construction and right after Break), else from the next epoch opened; same clamp as MaxDrift
// Options gains:
MaxDrift          time.Duration // re-anchor when drift exceeds it; 0 disables; must exceed media.MaxAnchorLead
DriftPositiveOnly bool          // paced inputs: react only to a source falling behind
// Stats gains:
DriftBreaks, DroppedDrift uint64
```

Definitions, per epoch: `off_i = arrival_i − duration(DTS_i − Start)` for each released frame. `baseline` = the minimum `off` over the epoch's first 10 s of arrivals (bursts and network delay only raise `off` above the true offset, and the chain lead is a constant inside it). `drift` = (minimum `off` over the last 10 s) − `baseline`, evaluated once per second of arrivals after the first 10 s. Rule: when `drift > MaxDrift`, or `drift < −MaxDrift` and not `DriftPositiveOnly`, the Clock opens a new epoch at the next video keyframe (any frame for audio-only streams) (placement below). Let `b = baseline − Wall_{n−1}`, the previous epoch's constant offset (burst, chain lead, first-frame delay). For a drift epoch only, frames with `arrival − b < Wall_{n−1} + Δ` are dropped and counted in `DroppedDrift`, so epochs never overlap; the new epoch's `Wall_n = ceil_ms(arrival_key − b)` and R is set to the same value, so the constant offset is kept and only the drift is removed. A positive-drift break leaves a wall gap between periods of about the drift; document it in the `Epoch` doc. New Clocks reject `0 < MaxDrift ≤ MaxAnchorLead` by clamping to `2 × MaxAnchorLead`.

- [ ] **Step 1: Write the failing tests** (fake arrival times through `PushAt`):
  - `TestDriftSlowEncoder`: stream time advances 0.2 % slower than arrival for 30 min → `DriftBreaks` ≥ 1; after each break `|drift|` restarts near 0; `Wall_n ≥ Wall_{n−1} + Δ` for every epoch.
  - `TestDriftFastEncoder`: 0.2 % faster → breaks occur; `DroppedDrift` per break ≤ frames in (`MaxDrift` + one GOP); no overlap.
  - `TestDriftJitterIgnored`: constant rate with ±800 ms uniform arrival jitter, including +800 ms on the first frame, for 30 min → `DriftBreaks == 0`.
  - `TestDriftBurstStart`: one GOP (2 s) delivered at once, then real time for 30 min → 0 breaks, 0 dropped.
  - `TestDriftFastAfterBurst`: a 2 s burst, then 0.2 % fast → `DroppedDrift` per break ≤ frames in (`MaxDrift` + one GOP).
  - `TestSetDrift`: drift disabled at construction; `SetDrift(2s, false)` called right after construction, and in a second run right after a `Break(true)` (the pipeline's order) → the pending epoch itself re-anchors on a drifting source; `SetDrift(1s, false)` is clamped to 2 s.
  - `TestDriftAfterChainLead`: a second epoch placed with a 1 s chain lead (previous epoch's excess) then real time → 0 breaks.
  - `TestDriftPositiveOnly`: the fast source with `DriftPositiveOnly` → 0 breaks; the slow source → breaks.
  - `TestDriftDisabled`: `MaxDrift == 0` → no breaks (frozen-clock behaviour unchanged).
- [ ] **Step 2: Run** `go test -race ./internal/clock/` — FAIL.
- [ ] **Step 3: Implement**; update the `Epoch` doc to describe the drift reset.
- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit** `feat(clock): re-anchor live sources whose clock drifts`

### Task 7: `internal/ingest` core, TS reader, file input, test peers and CI peers

**Files:** Create `internal/ingest/ingest.go`, `internal/ingest/tsreader.go`, `internal/ingest/file.go`, `internal/media/configcache.go`, `internal/testpeer/testpeer.go`; Modify `.github/workflows/ci.yml`; Test `internal/ingest/ingest_test.go`, `internal/ingest/file_test.go`, `internal/media/configcache_test.go`

**Interfaces — produces:**

```go
// ingest
var ErrDiscontinuity = errors.New("ingest: discontinuity")

type Spec struct {
    Pace         bool          // release at real time against epoch anchors (file, HLS pull)
    Live         bool          // the source runs in real time: Clock drift re-anchoring
    Passive      bool          // the source connects to us (UDP, SRT listener)
    StallTimeout time.Duration // raises the pipeline's stall limit for this input; 0 means none
}

type Frame struct {
    media.Frame
    Arrival time.Time // when the reader received the bytes completing it
}

type Reader interface {
    Read() (Frame, error)     // source time; ErrDiscontinuity once per break; io.EOF at the end
    Kinds() []media.Kind      // reader tracks by index; may grow after a PMT change
    Spec() Spec               // effective spec after open
    Stats() map[string]uint64 // named snake_case counters; safe concurrently with Read
    Close() error             // safe concurrently with Read; unblocks it
}

type Env struct {
    Lookup func(stream string) (*hub.Hub, bool) // copy://; nil disables it
    HTTP   *http.Client                          // nil means 10 s dial and ResponseHeaderTimeout
    Now    func() time.Time                      // arrival stamps; nil means time.Now
}

func Parse(rawURL string) (Spec, error)                                  // no I/O
func Open(ctx context.Context, rawURL string, env Env) (Reader, error)   // dispatch on scheme
func NewTSReader(rc io.ReadCloser, now func() time.Time) (Reader, error) // ts.Demuxer with Remap; blocks until the first PMT; closing rc aborts it
func Redact(rawURL string) string

// media
type ConfigCache struct{ /* last config per track */ }
func (c *ConfigCache) Stable(track int, cfg *TrackConfig) *TrackConfig // previous pointer when Equal

// testpeer
func Require(t testing.TB, binary string, protocols ...string) string // path; skip or fail per OS_REQUIRE_PEERS
```

File input: reopens at EOF when `loop=1`; each pass gets a fresh demuxer and `TimeDecoder` (a loop jumps back) and the reader returns `ErrDiscontinuity` between passes; a pass that yields no frame returns an error instead of looping (the deferred empty-source spin). The TS reader's counters include the demuxer's as `lost_frames`, `pmt_changes`, `split_aus`, `split_aus_reordered`, `continued`, `cc_errors`. The TS reader returns `ErrDiscontinuity` before the first frame after a PMT change that remapped a slot, and resets the `ConfigCache` entries of remapped slots, so a PMT change is one epoch even with continuous timestamps and identical codecs.

- [ ] **Step 1: Write the failing tests**
  - `TestParse`: every row of the URL table → its `Spec`; `pace=0` clears `Pace`; `ftp://x` and malformed URLs → errors; `Redact("srt://u:p@h:1?passphrase=s")` contains neither `p@` nor `s` as values.
  - `TestOpenFile`: `Open` on a bare path and a `file://` URL returns a reader; unknown scheme → error naming the redacted URL. (Later tasks add their own `Open` rows.)
  - `TestFileReader`: `h264_aac.ts` → all frames with non-zero `Arrival`, then `io.EOF`; counts equal `ts.Demuxer`'s.
  - `TestFileLoop`: `loop=1` → after the last frame `ErrDiscontinuity`, then the first frame again with the same `Config` pointers; `Close` during `Read` returns within 100 ms.
  - `TestFileLoopEmptyPass`: a file with a PMT and no PES, `loop=1` → error, not a spin.
  - `TestTSReaderKindsStable`: on `h264_aac_pmtchange.ts`, `Kinds()` stays `[video audio]` and `Stats()["pmt_changes"] == 1`.
  - `TestTSReaderPMTChangeIsOneEpoch`: the same fixture (continuous PTS) → exactly one `ErrDiscontinuity`; through a Clock, exactly 2 epochs.
  - `TestConfigCacheStable`: equal configs → same pointer; different → new.
- [ ] **Step 2: Run** `go test -race ./internal/ingest/ ./internal/media/` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: CI.** In the `test` and `coverage` jobs: `sudo apt-get update && sudo apt-get install -y ffmpeg`, a step asserting `ffmpeg -hide_banner -protocols | grep -qx '  srt'` (or the equivalent trimmed match), and `env: OS_REQUIRE_PEERS: "1"`.
- [ ] **Step 5: Run** — PASS.
- [ ] **Step 6: Commit** `feat(ingest): add the reader interface, TS reader and file input`

### Task 8: Pipeline on ingest readers

**Files:** Rename `internal/pipeline/source.go` → `internal/pipeline/pipeline.go`; Modify callers `cmd/osdev/main.go`, `cmd/osconform/main_test.go`, `internal/e2e/m3a_test.go`, `internal/e2e/drift_test.go`, `internal/conform/container_test.go`, `internal/output/live/packager_test.go`, `internal/pipeline/*_test.go`; Test `internal/pipeline/pipeline_test.go`

**Interfaces — produces** (replaces `Options{Open, Loop, Pace}` and `Run`):

```go
type Options struct {
    Inputs   []string // URLs by priority, first highest; at least one
    Env      ingest.Env
    Open     func(ctx context.Context, url string) (ingest.Reader, error) // nil means ingest.Open with Env
    Parse    func(url string) (ingest.Spec, error)                         // nil means ingest.Parse
    Layout   []media.Kind // nil: input 0's kinds (see Global Constraints)
    EndOnEOF bool         // io.EOF from the active input ends Run with nil
    StartTimeout, StallTimeout, RetryInterval, FailbackHold time.Duration // 0 means the defaults
    Now     func() time.Time
    Sleep   func(ctx context.Context, d time.Duration) error
    After   func(d time.Duration) <-chan time.Time // every timeout and retry wait; nil means time.After
    OnEvent func(Event)                            // optional, called from the Run goroutine
}

type EventKind uint8
const ( EventActive EventKind = iota + 1; EventFailed; EventDiscontinuity )
type Event struct { Kind EventKind; Input int; Err error }

type Stats struct {
    Active   int // index of the active input, -1 when none
    Switches uint64
    Unmapped uint64 // frames of reader tracks outside the layout
    Clock    clock.Stats
    Input    map[string]uint64 // the active reader's Stats()
}

type Pipeline struct{ /* ... */ }
func New(h *hub.Hub, opt Options) *Pipeline
func (p *Pipeline) Run(ctx context.Context) error // ctx.Err() on cancel; nil per EndOnEOF
func (p *Pipeline) Stats() Stats                  // safe from other goroutines
```

This task runs input 0 only (failover is Tasks 9–10): one read goroutine per reader feeds a channel; Run waits on it with `After(StartTimeout)` before the first frame and `After(max(StallTimeout, Spec().StallTimeout))` between frames, never counting its own pacing sleeps. A failure closes the reader, emits `EventFailed`, and retries input 0 with backoff 0.5 s, 1 s, 2 s … up to `RetryInterval`, reset by 10 s of healthy reading. `Parse` gives `Passive` before open; everything else comes from the open reader's `Spec()` (so the stall limit is `max(StallTimeout, Spec().StallTimeout)`). Every time an input becomes active after a different reader, including the same input reopened after a failure, the pipeline calls `Break(true)` then `SetDrift` for it; the very first activation calls only `SetDrift`. Anchors and pacing keep M3a's behaviour when `Spec().Pace`. The Clock gets `Kinds = layout`; at every input start the pipeline calls `Clock.SetDrift(2 s, Spec().Pace)` when `Spec().Live`, else `SetDrift(0, false)`; frames go in with `PushAt(f, f.Arrival)`. `ErrDiscontinuity` → `Clock.Break(false)` + `EventDiscontinuity`. When `Env.Now` is nil the pipeline sets it to `Options.Now`, so arrival stamps and timers share one clock.

Porting recipe for in-memory callers: `Open` returns `ingest.NewTSReader(io.NopCloser(bytes.NewReader(clip)), now)` (its `Spec()` is zero: unpaced, not live), `Parse` returns `ingest.Spec{}`, `EndOnEOF: true`. Looping callers (`TestPacedLoopDoesNotDrift`): write the clip to `t.TempDir()`, use `file://…?loop=1`, and pass the fake clock as `Options.Now`; a loop is an `ErrDiscontinuity` inside one reader, never an EOF retry. osdev: `-stream name=path` becomes `path` (plus `?loop=1` with `-loop`).

- [ ] **Step 1: Write the failing tests** with fake readers (`Open`/`Parse` injected, fake `After`):
  - `TestPipelineLayoutMapping`: reader kinds `[audio video audio]`, layout `[video audio]` → reader track 1 → 0, track 0 → 1, track 2 dropped, `Unmapped` counts it.
  - `TestPipelineLayoutWaitsForInput0`: no `Layout`; input 0 opens after 3 s (within `StartTimeout`) → the layout is input 0's kinds.
  - `TestPipelineDiscontinuity`: `ErrDiscontinuity` mid-stream → exactly one extra epoch.
  - `TestPipelineStartTimeout`, `TestPipelineStallTimeout`: no frame in time → `EventFailed`, reader closed, read goroutine gone (`runtime.NumGoroutine` returns to its baseline within 1 s; not parallel); retries at 0.5 s, 1 s, 2 s … of fake time.
  - `TestPipelinePacingNotStall`: a paced input whose pacing sleeps exceed `StallTimeout` never stalls.
  - `TestPipelineDriftSettings`: live unpaced → `SetDrift(2 s, false)`; live paced → `SetDrift(2 s, true)`; not live → disabled (observed through `Stats().Clock.DriftBreaks` on drifting fakes).
  - `TestPipelineSniffedSpec`: a fake reader opened from an `http://` URL whose `Spec()` reports `Pace` → frames are paced although `Parse` said unpaced.
  - `TestPipelineBurstNoDrift`: a live fake reader that delivers a 2 s GOP at once, then 30 s of real-time frames (fake arrivals) → `DriftBreaks == 0`, no frame dropped.
  - Ported tests pass unchanged in substance, including `TestPacedLoopDoesNotDrift` in `internal/e2e`.
- [ ] **Step 2: Run** `go test -race ./internal/pipeline/` — FAIL.
- [ ] **Step 3: Implement** and port every caller in Files.
- [ ] **Step 4: Run** `go test -race ./internal/... ./cmd/...` — PASS.
- [ ] **Step 5: Commit** `refactor(pipeline): run ingest readers with a fixed track layout`

### Task 9: Failover on failure

**Files:** Modify `internal/pipeline/pipeline.go`; Test `internal/pipeline/failover_test.go`

Rules: inputs are tried in priority order. An input fails on an open error, no frame within `StartTimeout`, a stall, a read error, or `io.EOF` (unless `EndOnEOF`). On a failure of the active input the pipeline switches at once to the next input in priority order that is not waiting out its backoff; when all are waiting, it waits for the earliest (the hub stays open). A switch is counted when the new input delivers its first frame: then the old reader is closed, `Clock.Break(true)` is called once, `SetDrift` is applied for the new input, `EventActive` is emitted and `Switches` counts it; trying several dead inputs in a row is still one switch. **No active input** (start-up, or every input failed): the highest-priority input that delivers a frame becomes active at once, with no hold — passive inputs keep listening, pull inputs reopen per their backoff, all concurrently. A failed passive input keeps listening in the background (it is not closed) and becomes a fail-back candidate (Task 10); a failed active-pull input follows its backoff.

- [ ] **Step 1: Write the failing tests** (fake readers, fake timer):
  - `TestFailoverOneEpoch`: input 0 delivers 2 s then errors; input 1 healthy → exactly one more epoch; frames lost = (input 0 frames read but not written to the hub) + (input 1 frames dropped before its first key), computed from the fakes' logs and the Clock stats, and asserted equal to the observed gap; `CheckFrames` clean.
  - `TestFailoverTrackMismatch` (Review Focus 2): layout `[video audio]`; backup video only → audio stops, one epoch; a backup with two audio tracks → the second counted in `Unmapped`.
  - `TestPipelineStallFailover` (Review Focus 3): input 0 stops sending → switch within `StallTimeout` + 1 s of fake time; input 0's read goroutine gone.
  - `TestAllInputsDown`: both fail → retries follow the backoff (opens at 0, 0.5, 1.5, 3.5, 7.5, 15.5, 25.5, 35.5, 45.5, 55.5 s); over 60 s of fake time at most 10 `Open` calls per input, 20 in total.
  - `TestSinglePassiveStartsLate`: one passive input whose publisher sends its first frame 30 s after Run → it becomes active then, one epoch, no switch counted before it.
  - `TestSinglePassiveAfterStall`: the only passive input stalls 6 s and resumes → active again on its first frame, one epoch.
  - `TestFailoverDriftFollowsInput`: a not-live primary fails over to a live drifting backup → drift breaks occur on the backup.
  - `TestPipelinePassiveReconnect` (Review Focus 4, with a fake passive reader): the publisher pauses 2 s and returns with `ErrDiscontinuity` inside `StallTimeout` → no switch, one epoch.
- [ ] **Step 2: Run** — FAIL. **Step 3: Implement.** **Step 4: Run** `go test -race ./internal/pipeline/` — PASS.
- [ ] **Step 5: Commit** `feat(pipeline): fail over between inputs by priority`

### Task 10: Fail-back with a hold

**Files:** Modify `internal/pipeline/pipeline.go`; Test `internal/pipeline/failback_test.go`

Rules: while input k > 0 is active, a probe for each of inputs 0..k−1 runs concurrently: pull inputs reopen after their backoff; passive inputs keep listening. A probe that reads stall-free for `FailbackHold` (default 10 s) becomes eligible; the pipeline then switches to it at its next video keyframe (any frame when it has no video), with the frames from that keyframe on. While waiting, a probe keeps only the frames from its latest keyframe (bounded by one GOP). A higher-priority eligible probe wins; others are closed. A probe that fails restarts its hold. The fail-back switch is a switch as defined in Task 9 (`Break(true)`, `SetDrift`, `EventActive`, `Switches`).

- [ ] **Step 1: Write the failing tests**:
  - `TestFailbackOnKeyframe`: input 0 down at start, input 1 active; input 0 returns and is healthy for 10 s → one switch to 0 at its next keyframe, which is the first frame of the new epoch; input 1 closed.
  - `TestFailoverFlappingPrimary` (Review Focus 1): primary up 3 s / down 3 s for 120 s, backup healthy → exactly 1 switch, no fail-back; probe memory stays within one GOP (assert the buffered frame count ≤ frames per GOP).
  - `TestFailbackPrefersHighest`: inputs 0 and 1 both recover while 2 is active → switch to 0.
  - `TestProbeGoroutinesExit`: cancel Run with probes alive → all probe goroutines gone within 1 s.
- [ ] **Step 2–4:** red, implement, green.
- [ ] **Step 5: Commit** `feat(pipeline): fail back to higher-priority inputs after a hold`

### Task 11: SRT input

**Files:** Create `internal/ingest/srt.go`; Test `internal/ingest/srt_test.go`; Modify `go.mod`, `go.sum`

Caller: `srt.Dial` with the URL's `streamid`, `passphrase`, `latency` (ms, default 120); a caller disconnect returns an error from `Read` (the pipeline retries). Listener: listens for the reader's whole life; accepts the first publisher whose stream ID matches (any when `streamid` is empty) and rejects others while one is connected; when the publisher disconnects, `Read` waits for the next publisher and returns `ErrDiscontinuity` before its first frame (a fresh TS demuxer per publisher; `Config` pointers through the reader's `ConfigCache`). `Close` closes the connection and the listener.

- [ ] **Step 1:** `go get github.com/datarhei/gosrt@latest`; record the version in the commit message.
- [ ] **Step 2: Write the failing tests**
  - `TestSRTCallerListenerInProcess`: our listener reader + a gosrt caller writing `h264_aac.ts` paced → per-track frame counts equal the file's.
  - `TestSRTListenerRejectsSecond`: a second publisher is rejected while the first is connected.
  - `TestSRTListenerReconnect` (Review Focus 4): the publisher disconnects and a new one connects 1 s later → one `ErrDiscontinuity`, frames continue, the listener port stays bound throughout.
  - `TestSRTWithFFmpeg` (peer via `testpeer.Require(t, "ffmpeg", "srt")`): ffmpeg as caller into our listener, and our caller pulling from ffmpeg as listener (`latency` given to ffmpeg in µs) → frames through Clock pass `CheckFrames`.
  - `TestSRTPassphrase`: a wrong passphrase → open error, no frames.
  - `TestOpenSRT`: `Open` row for both modes.
- [ ] **Step 3: Run** — FAIL. **Step 4: Implement.** **Step 5: Run** `go test -race ./internal/ingest/` — PASS.
- [ ] **Step 6: Commit** `feat(ingest): add SRT caller and listener inputs`

### Task 12: UDP input

**Files:** Create `internal/ingest/udp.go`; Test `internal/ingest/udp_test.go`

Listen with `net.ListenUDP`, or `net.ListenMulticastUDP` on the named interface when the host is a multicast group; `SetReadBuffer(4 << 20)`, then report the effective size (from the socket) as `udp_read_buffer` and warn in the counters when it is lower (`net.core.rmem_max`). Framing per datagram: a 0x47 at offset 0 → raw TS; else an RTP version-2 header whose computed payload offset (12 + 4×CSRC count + extension) holds 0x47 → strip it (any payload type); count RTP sequence gaps (`rtp_gaps`), reorder nothing. A gap in datagrams longer than `StallTimeout` is the pipeline's stall; a sender returning after a pause continues in the same reader (the demuxer resyncs; the Clock judges the jump). Datagrams feed the TS reader through a buffer reader, no goroutine per datagram. `Close` closes the socket.

- [ ] **Step 1: Write the failing tests**
  - `TestUDPRawTS`: an in-process sender writes `h264_aac.ts` in 7×188-byte datagrams → per-track frame counts equal the file's.
  - `TestUDPRTPWrapped`: the same with RTP headers, PT 33 and a dynamic PT 96, one with a CSRC and an extension → same frames; `rtp_gaps == 0`.
  - `TestUDPLossAndReorder`: drop 1 % and swap adjacent datagrams 1 % (seeded) → no frame-check violation, `lost_frames > 0`.
  - `TestUDPMulticast`: join 239.255.0.1 on loopback when supported (skip with the reason otherwise).
  - `TestUDPWithFFmpeg` (peer): `ffmpeg -re -i h264_aac.ts -c copy -f mpegts "udp://127.0.0.1:P?pkt_size=1316"` → clean frames.
  - `TestOpenUDP`: `Open` row; `udp_read_buffer` reported.
- [ ] **Step 2–4:** red, implement, green.
- [ ] **Step 5: Commit** `feat(ingest): add UDP and RTP-wrapped TS inputs`

### Task 13: HTTP-TS pull input

**Files:** Create `internal/ingest/httpts.go`; Test `internal/ingest/httpts_test.go`

GET with `Env.HTTP` (redirects followed); non-2xx → error naming the status. A response whose content type is an HLS type, or whose body starts with `#EXTM3U`, is handed to the HLS reader (Task 15) — until Task 15 lands, it is an error "HLS input not available". The body feeds `NewTSReader`. `Close` cancels the request.

- [ ] **Step 1: Write the failing tests**
  - `TestHTTPTSPull`: an `httptest.Server` streaming `h264_aac.ts` slowly → all frames.
  - `TestHTTPTSPullFromOurOutput`: our M3a `httpts` handler on a hub fed by a file pipeline → frames pass `CheckFrames`.
  - `TestHTTPTSStatus`: 404 → error naming 404; a 302 to a relative path → followed.
  - `TestHTTPTSCloseUnblocks`: headers then a stall; `Close` ends `Read` within 1 s; the open `ctx` ending after `Open` returned does not end the reader.
- [ ] **Step 2–4:** red, implement, green.
- [ ] **Step 5: Commit** `feat(ingest): add HTTP-TS pull input`

### Task 14: fMP4 demuxer and HE-AAC in the TS muxer

**Files:** Create `internal/mux/fmp4/demux.go`; Modify `pkg/mpegts/adts.go`; Test `internal/mux/fmp4/demux_test.go`, `pkg/mpegts/adts_test.go`; Modify `testdata/fixtures/gen.sh`, add `testdata/fixtures/h264_aac_frag.mp4`

**Interfaces — produces:**

```go
type Demuxer struct{ /* tracks from one init segment */ }
func NewDemuxer(init []byte) (*Demuxer, error)              // every H.264/H.265/AAC trak; others skipped with a reason
func (d *Demuxer) Kinds() []media.Kind
func (d *Demuxer) Segment(b []byte) ([]media.Frame, error)  // all tracks merged in DTS order, 90 kHz source time
```

Rules:
- Times: per track, tfdt + cumulative sample durations; composition offsets signed (trun v1). Rescale the cumulative value to 90 kHz with `math/bits` (128-bit product, never per-sample rounding, no overflow for tfdt counted from 1970 at 10 MHz).
- Negative composition offsets: when `elst` gives a media time, subtract it; otherwise shift the track's DTS down by the most negative offset seen in its first segment, constant for the demuxer's life; a later frame that would still have PTS < DTS is dropped and counted.
- Video samples are split from length-prefixed NAL units; parameter sets in samples move into the `Config` (as the TS demuxer does); `Key` and `RASL` come from `media.NALClassifier` (Task 4); `Config` pointers through a `media.ConfigCache`. Audio `Data` is the raw AAC frame. mp4ff panics on hostile input become errors.
- `pkg/mpegts.AppendADTS` accepts HE-AAC ASCs (object type 5 or 29, including explicit hierarchical signalling): it writes an AAC-LC ADTS header (profile 1) at the core sampling frequency and the core channel configuration (implicit SBR/PS signalling). This was deferred to M2b; it moves here because fMP4 HLS sources are the first HE-AAC input.

- [ ] **Step 1: Fixture.** `h264_aac_frag.mp4`: `-f mp4 -movflags frag_keyframe+empty_moov+default_base_moof` with both traks in one init.
- [ ] **Step 2: Write the failing tests**
  - `TestDemuxOurCMAF`: segments from our `fmp4` writer for `h264_aac.ts` frames → identical PTS, DTS, Key, Data after the round trip; output in DTS order across tracks.
  - `TestDemuxMuxedInit`: `h264_aac_frag.mp4` → two tracks; frame counts equal ffprobe's; frames interleaved by DTS.
  - `TestDemuxNegativeCTO`: a hand-built trun v1 with negative offsets → every frame PTS ≥ DTS, none dropped.
  - `TestDemuxTimescale`: 44.1 kHz audio over 10 min → the last PTS equals the exact sample-count value; tfdt of 1.7e16 at timescale 10 000 000 → no overflow.
  - `TestDemuxHostile`: truncated and random boxes → errors, no panic.
  - `TestAppendADTSHEAAC`: ASC object type 5 at 24 kHz core / 48 kHz output, stereo → ADTS profile LC, frequency index for 24 kHz, channels 2; object type 29 → channel configuration 1.
  - `TestHEAACThroughOutputs` (`internal/e2e` or `internal/output/live`): HE-AAC frames at 1024 samples per frame at the core rate through `fmp4.Init`/segments and the TS muxer → durations equal the sample-count values and ffprobe reads both outputs.
- [ ] **Step 3–5:** red, implement, green.
- [ ] **Step 6: Commit** `feat(fmp4): add a fragmented MP4 demuxer; write HE-AAC as ADTS`

### Task 15: HLS pull — media playlists and TS segments

**Files:** Create `internal/ingest/hls.go`; Modify `pkg/hls/media.go`, `internal/ingest/httpts.go` (handoff); Test `internal/ingest/hls_test.go`, `pkg/hls/media_test.go`

`pkg/hls` media playlist parsing gains `EXT-X-KEY` (method, URI) and `EXT-X-BYTERANGE` (length and offset, offset defaulting to the end of the previous range of the same URI) on segments.

Behaviour:
- All URIs resolve against the URL the playlist was finally fetched from (`resp.Request.URL`, after redirects). Byte ranges are fetched with `Range` headers.
- Live playlists: start three segments from the end (RFC 8216 §6.3.3); reload every target duration, or half of it after a reload with no new segment; fetch in order. When the next wanted segment is no longer listed → `ErrDiscontinuity`, resume three from the end, counted in `fell_behind`. `EXT-X-DISCONTINUITY` → `ErrDiscontinuity` before that segment. `EXT-X-ENDLIST` → play to the end, then `io.EOF`. Any `EXT-X-KEY` other than `NONE` → open error "encrypted HLS is not supported".
- TS segments are fed into one TS demuxer in order (PES may span segments); every `ErrDiscontinuity` starts a new demuxer and `TimeDecoder`; `Config` pointers through the reader's `ConfigCache`. A failed segment fetch is retried once, then skipped with `ErrDiscontinuity`.
- Arrival stamps are the time each segment finished downloading. `Spec` per the URL table.

- [ ] **Step 1: Write the failing tests** against `httptest` servers:
  - `TestHLSKeyAndByteRange` (`pkg/hls`): parse both tags; offsets default correctly.
  - `TestHLSPullFromOurOutput`: our M3a packager's HLS-TS output of `h264_aac.ts` looped → frames pass `CheckFrames`.
  - `TestHLSPullConfigChange`: `h264_aac_then_640.ts` → one `ErrDiscontinuity` and new `Config` pointers.
  - `TestHLSPullFFmpegVOD` (peer): `ffmpeg -i h264_aac.ts -c copy -f hls -hls_time 2 -hls_list_size 0` in a temp dir served by `httptest` → frame counts equal the file's, then `io.EOF`.
  - `TestHLSPullFellBehind`: the server drops segments from its window faster than the reader fetches → one `ErrDiscontinuity`, resume three from the end, `fell_behind == 1`.
  - `TestHLSPullRedirectRelative`: the playlist URL redirects to another path; relative segment URIs resolve against the final URL.
  - `TestHLSPullByteRange`: segments as byte ranges of one file → each fetched with `Range`.
  - `TestHLSPullEncryptedRefused`.
  - `TestOpenHLS`: `Open` rows by suffix, by content type, by `#EXTM3U` sniff from the HTTP-TS path.
- [ ] **Step 2–4:** red, implement, green.
- [ ] **Step 5: Commit** `feat(ingest): add HLS pull for TS media playlists`

### Task 16: HLS pull — multivariant playlists, fMP4 segments and a separate audio rendition

**Files:** Modify `internal/ingest/hls.go`; Test `internal/ingest/hls_variant_test.go`

Behaviour:
- A multivariant playlist selects the variant with the highest `BANDWIDTH`, or `?variant=N` (0-based, playlist order).
- fMP4 segments (`EXT-X-MAP`) go through Task 14's demuxer; a new map URI or an `ErrDiscontinuity` creates a new demuxer.
- When the variant's audio is in an `EXT-X-MEDIA TYPE=AUDIO` rendition with a URI (the group's `DEFAULT=YES` entry, else its first), the reader runs that media playlist too, in its own goroutine with its own demuxer, and merges the two frame streams in DTS order (both are on the presentation timeline per RFC 8216 §6.2.4). The audio track gets the next reader track index after the variant's own tracks. A discontinuity or fell-behind on either playlist is one `ErrDiscontinuity` for the reader, after which both resume aligned by discontinuity sequence and timestamps (media sequence numbers of the two playlists need not match). When either playlist stalls, merged output waits up to the stall limit, then the pipeline's stall rule fails the whole input (ruling at dispatch: no degraded video-only mode in M2a).

- [ ] **Step 1: Write the failing tests**:
  - `TestHLSPullOurCMAF`: our M3a HLS-fMP4 output (`cmaf/master.m3u8`, separate audio rendition) of `h264_aac.ts` looped → video and audio, frames in DTS order, `CheckFrames` clean.
  - `TestHLSPullVariantSelection`: highest bandwidth by default; `variant=0` picks the first.
  - `TestHLSPullMuxedFMP4`: one playlist of `h264_aac_frag.mp4` fragments → two tracks.
  - `TestHLSPullRenditionDiscontinuity`: a discontinuity on both playlists at the same sequence → exactly one `ErrDiscontinuity`.
  - `TestHLSPullRenditionGoroutineExit`: `Close` → both playlist goroutines gone within 1 s.
- [ ] **Step 2–4:** red, implement, green.
- [ ] **Step 5: Commit** `feat(ingest): pull multivariant and fMP4 HLS with separate audio`

### Task 17: `copy://` input

**Files:** Create `internal/ingest/copy.go`; Test `internal/ingest/copy_test.go`

Subscribes to `Env.Lookup(name)` with queue 1024 under the subscriber name `copy:<url>`; frames pass on in the source stream's stream time with `Epoch` set to zero and `Arrival` = delivery time; `Delivery.Lost > 0` or a change of the source frame's `Epoch` → `ErrDiscontinuity` before that frame; `Kinds()` from the source's tracks seen up to the first video keyframe (any frame for audio-only), which `Open` waits for within its `ctx`. Hub close → `io.EOF`. Unknown stream → open error.

- [ ] **Step 1: Write the failing tests**: `TestCopyFrames` (frames equal the source hub's, `Epoch == 0`), `TestCopyLostIsDiscontinuity`, `TestCopyEpochIsDiscontinuity`, `TestCopyUnknown`, `TestCopyClose` (the subscriber disappears from the source hub's stats), `TestOpenCopy`.
- [ ] **Step 2–4:** red, implement, green.
- [ ] **Step 5: Commit** `feat(ingest): add copy input`

### Task 18: `osdev` inputs and the ingest CPU measurement

**Files:** Modify `cmd/osdev/main.go`; Create `internal/pipeline/bench_test.go`; Test `cmd/osdev/main_test.go`

- `-stream name=URL` accepts any input URL (a bare path stays a file). Repeating a name adds a lower-priority input. `-layout video,audio` sets the layout for all streams (default `video,audio`). `-loop` adds `loop=1` to file inputs. `copy://` resolves to osdev's own stream hubs. Every 30 s, and at shutdown, osdev logs per stream, only when changed: active input (redacted), switches, unmapped, drift breaks, and the active reader's counters.
- `BenchmarkIngest10Streams`: 10 pipelines reading an in-memory 4 Mbps-equivalent TS clip (built in the benchmark by looping `h264_aac.ts` with `pace=0`, not live) into hubs with one draining subscriber each. Report frames/s and CPU-seconds per media-second per Mbps with `b.ReportMetric`. It measures demux, Clock and hub only; record the numbers in the task report and the PR description against REBUILD_PLAN §6's budget (v1: 4 cores for 10 streams).

- [ ] **Step 1: Write the failing tests**: `TestOsdevFailoverFlags` (two inputs for one name parse in priority order; a malformed URL → exit 2; `-layout audio` accepted), `TestOsdevSRTStream` (a gosrt caller publishes into `-stream a=srt://:P?mode=listener`; `/live/a/hls/index.m3u8` lists segments).
- [ ] **Step 2–4:** red, implement, green; run the benchmark once with `-benchtime 20x` and paste the numbers into the task report.
- [ ] **Step 5: Commit** `feat(osdev): serve any input with failover and measure ingest CPU`

### Task 19: M2a end-to-end scenarios and the spec update

**Files:** Create `internal/e2e/m2a_test.go`; Modify `docs/REBUILD_PLAN.md`, `testdata/fixtures/README.md`

- [ ] **Step 1: Write the scenarios** (each: input → pipeline → hub → frames collected by a subscriber → `conform.CheckFrames` with no skipped rule unless the test names why):
  - `TestM2aEveryInput`: rows for file, UDP raw, UDP RTP, SRT caller and listener (ffmpeg), HTTP-TS from an `httptest` server and from our `httpts` output (GOP burst at connect), HLS-TS, HLS-fMP4 with separate audio, `copy://` (GOP replay at subscribe) — each clean, frame counts within one GOP of the source's. The live rows run 30 s (looped sources) and assert `DriftBreaks == 0`, so the drift evaluation (after 10 s) has 20 s to fire.
  - `TestM2aFailoverSRTtoUDP`: primary SRT from ffmpeg killed mid-stream, backup UDP from ffmpeg → exactly one epoch change; frames lost bounded by `StallTimeout` worth of source frames; an M3a packager on the same hub lists one discontinuity and `conform.ReadHLS` on it reports no violation.
  - `TestM2aPMTChange`: `h264_aac_pmtchange.ts` over UDP → one epoch change.
  - `TestM2aOpenGOP`: `h264_opengop_midgop.ts` over UDP → video starts at the first recovery point.
- [ ] **Step 2: Run** `OS_REQUIRE_PEERS=1 go test -race ./internal/e2e/` locally — PASS.
- [ ] **Step 3: Spec.** In `docs/REBUILD_PLAN.md`: add the M2a/M2b/M2c split under §5 after the M3 parallelism paragraph, linking this plan, with the "Out of M2a" list; change the M2 row's "a PMT change restarts the demuxer" to "a PMT change remaps the demuxer's tracks"; mark the "Carried from M1" M2 items done; add `datarhei/gosrt` as used; in §3.2 add the late-join, config-change hold and drift rules and `PushAt`. In `testdata/fixtures/README.md`, describe every new fixture.
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/... ./pkg/... ./cmd/...`, `go vet`, `gofumpt -l` — clean.
- [ ] **Step 5: Commit** `test(e2e): prove M2a inputs and failover; split M2 in the spec`
