# M4b: ABR Outputs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve a transcoder ladder as one ABR presentation: an HLS multivariant playlist (TS and fMP4) and a DASH MPD with one representation per rung, whose segments align across rungs by construction.

**Architecture:** The transcoder writes one more hub, the *presentation*, with every rendition's video as its own track and the audio once (ruling R-B1). Its merger orders frames with a *keyframe barrier* (R-B3): at every source keyframe K, every rung's frames before its IDR at K come out before any rung's IDR at K, and those IDRs come out in track order. So the unchanged segmenter, which cuts on the lowest video track, cuts every rung at the same frame. One `live.Packager` in ABR mode packages the presentation; M3b3's per-segment entries with `EXT-X-GAP` placeholders keep media sequence and discontinuity numbers equal across variants. A small segmenter option (R-B8) leaves a rung out of a segment it would not start on a keyframe at the segment's start, so every listed part is independent. `osconform` gains two ABR rules: variants aligned, and BANDWIDTH honest.

**Tech Stack:** Go 1.25, existing `internal/segment`, `internal/transcode`, `internal/output/live`, `internal/conform`, `pkg/hls`, `pkg/dash`; peers ffmpeg/ffprobe, VLC, GStreamer as in M3/M4a.

**Spec:** `docs/REBUILD_PLAN.md` (§3.4 Output kit, §3.5 Transcoder, §5 row M4b). M4a plan: `docs/superpowers/plans/2026-11-24-m4a-transcoder-core.md`.

## Global Constraints

- **Libraries:** no new dependency. Only the approved independent libraries; no bluenviron, no lal, no code copied from another product.
- **cgo:** nothing in this plan touches `internal/transcode/native` or `cmd/ostranscoder`. Without the `libav` tag, `go build ./...`, `go vet ./...`, `golangci-lint run ./...` and `make test` keep passing, as do `GOOS=windows go vet ./...` and `GOOS=darwin go vet ./...`.
- **Non-ABR behaviour is frozen:** with `live.Options.ABR` false, `segment.Options.AlignVideo` false and `transcode.Options.Presentation` false, every hub write, playlist, MPD, segment byte and counter is what M4a produced. The rendition mergers do not use the barrier. Every existing test passes unchanged.
- **Time:** 90 kHz everywhere. Rendition delay D = 45000 (`transcode.Delay`) is unchanged; the presentation carries the same PTS/DTS as the rendition hubs.
- **Lint:** `exhaustive` with `default-signifies-exhaustive: false`; `godot`; gofumpt.
- **Errors:** wrap as `fmt.Errorf("pkg: op: %w", err)`; never both log and return.
- **Goroutines:** every spawned goroutine has an explicit cancel path.
- **Peers:** every test that runs ffmpeg/ffprobe, VLC or GStreamer passes on the host (ffmpeg 8, GStreamer 1.28) and in `make test-ci-peers` (image `osv2-ci-peers:m4a`: ffmpeg 6.1, VLC 3.0.20, GStreamer 1.24) before any push.
- **Soak safety:** never stop, restart or reconfigure the v1 soak containers; never bind ports 554, 1935 or 8554; own containers are `osv2-*`, `--rm`, stopped afterwards.
- **Commits:** end every message with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Rulings

| ID | Ruling | Cost if wrong |
|---|---|---|
| R-B1 | The ABR presentation is one hub written by the transcoder: rendition i's video is track i (in `Options.Renditions` order), each source audio track a is track len(Renditions) + a. One packager packages it. Rejected: a manifest layer over the N rendition packagers, whose independent segmenters and losses can disagree on segment numbers and discontinuities (RFC 8216 §6.2.4) | One more merger per transcoder, and every rendition's bytes held a second time in the presentation's GOP cache and packager window |
| R-B2 | osdev serves the presentation of stream `<name>` as `<name>_abr`, on HLS (TS and fMP4) and DASH only; RTMP, RTSP, SRT, HTTP-TS and `copy://` do not find it. The label `abr` is reserved. Product naming is M5's | M5 renames it; the M4b tests use the osdev name only through `packLookup` |
| R-B3 | Alignment is made by the presentation's merger, not the segmenter: a keyframe barrier at every source main-video keyframe PTS K (+D), announced by the router before the keyframe goes to the child or the copy path. Guaranteed for sources whose video keyframes are IDRs (closed GOP); an open-GOP copy rung's leading pictures land after the barrier, so its segments there do not decode on their own. Rejected: a segmenter that cuts each video track at its own keyframe (review round 1: the first-arriving rung becomes the epoch's anchor, the hub's cache and loss resume start at the anchor's keyframe, and several held segments made the routing ambiguous) | The barrier waits at most MergeHold (2 s) for a late rung; past that, or for a rung without an IDR at K, that rung is left out of the segment (R-B8) and lists a gap |
| R-B4 | HLS-TS in ABR mode: one TS per segment per video track that has frames in it (that track plus every audio track), muxed with the video at track index 0 and audio at 1 + its rank among the window's known audio tracks, so every variant has the same PIDs. `hls/index.m3u8` is the multivariant playlist; `hls/v{track}.m3u8` lists one entry per segment, `{track}-{Seq}.ts`, or a `gap-{track}-{Seq}.ts` placeholder when the variant's video has no frames in the segment or its mux failed. MEDIA-SEQUENCE = first Seq. No combined TS is muxed. Continuity counters are per variant | TS bytes muxed N times; a client splicing variants sees a CC jump at each switch, which HLS clients reset at segment boundaries |
| R-B5 | Variants are listed in rendition order. BANDWIDTH is the window's peak over segments of (variant video part bytes + default audio part bytes) × 8 / duration, as `buildMaster` does today; for TS, the variant TS's bytes. RESOLUTION is the largest over the window, as today. No AVERAGE-BANDWIDTH or FRAME-RATE | A player that starts on the first variant starts on rung 0; ABR heuristics lack the average |
| R-B6 | DASH in ABR mode: video tracks with the same `media.Codec` share an AdaptationSet, each Representation with its own SegmentTemplate (init, media, startNumber, timeline, presentationTimeOffset). The set says `segmentAlignment="true"` in a period only when every Representation's timeline there is identical (`t`, `d`). Part numbers stay per track (a gap cannot be expressed in a `$Number$` timeline), so after a gap the Representations' numbers differ by a constant. Audio and non-ABR streams keep today's one-set-per-track layout | ISO 23009-1 phrases alignment by segment number; a player that aligns by number rather than time (dash.js and Shaka align by time) would switch to the wrong segment after a gap. An H.265 copy rung among H.264 rungs gives two video sets |
| R-B7 | The presentation hub's `GOPCacheFrames` is 1500 × (renditions + 1). The presentation's merger counts into `Stats.PresentationOut` and `Stats.PresentationDropped`, not `FramesOut` or `MergeDropped` | Memory: N rungs' GOP in one cache |
| R-B8 | `segment.Options.AlignVideo`, two rules. (1) *Leading video:* a segment opened by a video anchor before any segment of its epoch closed is leading, like the audio-anchored one today: when a lower video track arrives, the segment is taken back (`uncut`, its frames to the carry, where `openAt` keeps those with PTS ≥ the new Start) instead of cut. (2) *Late rung:* in a segment that is not Forced, a video track other than the epoch's anchor (`s.anchor`) enters the segment only at a keyframe with PTS equal to the segment's Start. Until it delivers a keyframe, its frames are dropped (`Dropped`); if its first keyframe in the segment has another PTS, the track is left out of the segment. A frame counts once actually added, after `place`'s post-loss drop and the carry filter | A rung that misses the barrier (beyond MergeHold, swallowed by a slow subscriber, or with no IDR at K) shows a gap entry for one segment, or a previous part cut short by the frames dropped, instead of a part that would not decode from its start |

**Respawn and the anchor rung (known cost):** while the child respawns, the copy rung goes on in the old epoch but the anchor rung (rendition 0, encoded) is silent, so the presentation's segment closes only at the epoch change or MaxDuration. That segment can exceed the target and raise the shared TARGETDURATION, which never shrinks. Recorded in the Backlog by Task 8.

## Execution waves (parallel per user instruction)

| Wave | Tasks | Why this order |
|---|---|---|
| 1 | 1 (segmenter R-B8), 2 (presentation hub and barrier), 3 (conform ABR rules) | Independent packages |
| 2 | 4 (packager ABR: fMP4 master, DASH) | Needs Task 1's `AlignVideo` |
| 3 | 5 (packager ABR: TS variants), 6 (osdev) | Both need Task 4's `Options.ABR`; 6 needs Task 2 |
| 4 | 7 (M4b scenarios) | Needs 2–6 |
| 5 | 8 (players, CI list, spec) | Needs Task 7's helpers |

Each parallel task runs in its own worktree; the controller merges.

## Review Focus

1. **A rung lagging or leading the anchor in decode order** (B-frames 3 on a copy rung, 2 on the encoded ones, or none): the presentation still delivers every pre-K frame before any IDR at K, and rung 0's IDR first. Task 2 tests both directions; Task 7's b-frames row proves it end to end.
2. **One slow packager subscription** (`Lost`) in a presentation: one discontinuity, at the same media sequence number, in every variant; a rung whose IDR the loss swallowed lists a gap rather than a part that starts mid-GOP. Task 1 and Task 7's slow-consumer row test it. Known: after a resume the hub passes audio only once every live video track has delivered a keyframe, so a rung with no IDR at the resume point costs that subscriber up to a GOP of audio, without a `Lost`.
3. **A respawned child**: the presentation gets exactly one new epoch, at one media sequence number in every variant. Task 7 tests it.
4. **Switching variants at a segment boundary** decodes cleanly. Task 7 tests it with segments from alternating TS variants.
5. **A late subscriber** to the presentation (the hub's GOP cache) gets every rung's IDR of the cached GOP, since rung 0's IDR, where the cache starts, comes first. Task 2 tests it.

---

### Task 1: Segmenter leaves out a rung that misses the segment start (R-B8)

**Files:**
- Modify: `internal/segment/segmenter.go` (doc and `place`)
- Test: `internal/segment/segmenter_test.go`

**Interfaces:**
- Produces: `segment.Options.AlignVideo bool`. Off: behaviour unchanged.

- [ ] **Step 1: Write the failing tests** (a new helper `ladder(s, tracks, start, n, gopFrames, order func(track, i int) int)` pushes several 25 fps H.264 tracks with keyframes at the same PTS, in a given arrival order; tracks 0..2 video, AAC track 3 where stated)

```go
func TestAlignVideoBarrierOrder(t *testing.T)
// AlignVideo, tracks 0..2, GOP 1 s, target 2 s, arrival as the barrier delivers it (all pre-K frames,
// then IDR_0, IDR_1, IDR_2, then the rest by DTS): every segment holds, per track, a run starting with
// its keyframe at seg.Start; Dropped() == 0. The same frames with AlignVideo off give identical segments.
func TestAlignVideoLateTrackAtOneCut(t *testing.T)
// at one K only, track 2's last 2 pre-K frames arrive after IDR_0(K): they are dropped (Dropped() == 2),
// track 2 enters the new segment at its IDR at Start; every other segment is untouched.
func TestAlignVideoLeadingRungTakenBack(t *testing.T)
// a new epoch whose first video frame is IDR_2(K) then IDR_0(K), IDR_1(K): one segment opens at K with
// anchor 0 and holds all three tracks from their IDRs; Seq has no hole; no one-frame segment.
func TestAlignVideoResumeOnHigherTrack(t *testing.T)
// a Lost delivery on IDR_2(K) (the hub resumes track 2 first): nothing opens until IDR_0's next keyframe
// K2; the segment at K2 holds every track from its keyframe at K2.
func TestAlignVideoTrackResumesLate(t *testing.T)
// after a Lost, track 1's first keyframe in the segment opened at K is at K+GOP (< K+Target): track 1 is
// left out of that segment and present in the next.
func TestAlignVideoMissingKeyframe(t *testing.T)
// track 1 has no keyframe at K=180000 (its next is K+GOP, inside the segment): that segment has no track-1
// frame; the following one has.
func TestAlignVideoForcedSegmentKeepsAll(t *testing.T)
// GOP 20 s, MaxDuration 10 s: Forced segments keep every track's frames, as with AlignVideo off.
func TestAlignVideoAudioUntouched(t *testing.T)
// with AAC track 3 lagging 0.5 s: audio placement equals AlignVideo off (assertAudioInside).
```

- [ ] **Step 2: Run** `go test -race -count=1 -run 'TestAlignVideo' ./internal/segment/` — compile failure.
- [ ] **Step 3: Implement** R-B8: the leading-video uncut, and per open segment each non-anchor video track's state (waiting, in, out).
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/segment/` — all pass.
- [ ] **Step 5: Commit** `feat(segment): leave out a video track that misses the segment's start`

### Task 2: The presentation hub and its keyframe barrier (R-B1, R-B3, R-B7)

**Files:**
- Modify: `internal/transcode/transcode.go`, `internal/transcode/merge.go`, `internal/transcode/epochs.go` (router: announce barriers)
- Test: `internal/transcode/merge_test.go`, `internal/transcode/transcode_test.go` (with the `transcodetest` fake child)

**Interfaces:**
- Produces: `transcode.Options.Presentation bool`; `func (t *Transcoder) Presentation() *hub.Hub` (nil when off; closed by Run like the rendition hubs); `func PresentationTrack(rendition int) int` = `rendition`; `func PresentationAudioTrack(renditions, srcTrack int) int` = `renditions + srcTrack`; `Stats.PresentationOut`, `Stats.PresentationDropped uint64`.

The presentation merger gets the same frames as the rendition mergers, each copied with `Track` relabelled: a rendition's video (child rung or copy path) to `PresentationTrack(i)`, audio once to `PresentationAudioTrack`. Anchors come from the shared `anchorStore`. `expectVideo` also announces the relabelled tracks of every encoded rendition. Hub option `GOPCacheFrames: 1500 * (len(Renditions)+1)`.

**Barrier.** The router calls `barrier(epoch, K)` for every source main-video keyframe it routes (K after the D shift), before the frame goes to the child or the copy path; only the presentation merger keeps barriers. Barriers get absolute sequence numbers per epoch (1, 2, …, kept with a base offset when old ones are pruned). Each frame gets an order key at push:
- video: `b` = the largest barrier sequence its track has passed. A keyframe whose PTS equals an unpassed barrier passes it and every lower one (phase 0, ordered by track). A non-keyframe whose PTS is above unpassed barriers passes the highest of them below its PTS (a rung without an IDR at K: phase 1), so a track never falls behind for the rest of the epoch;
- every other video frame: phase 1, ordered by DTS;
- audio: `b` = the largest barrier sequence at or below its PTS when pushed (never below the track's previous `b`), phase 1, ordered by DTS.
Frames are released in (epoch, b, phase, track for phase 0 | DTS for phase 1) order, under the merger's existing wait-for-expected-tracks rule and MergeHold. Per track the key never decreases, so the existing rule stays correct.

**Start and new epochs.** Two changes make rung 0's IDR the first video frame of every presentation epoch: (a) `router.send` announces the encoded tracks (`expectChildVideo`) before it pushes the copy video, for every merger (rendition mergers are unaffected: copy video never reaches an encoded rendition's merger); (b) in the presentation merger only, when the candidate frame is in an epoch above the hub's current one, `missing()` waits for that epoch's expected tracks (`seen[candidate.Epoch]`), within MergeHold. The segmenter's leading-video rule (R-B8 (1)) covers what MergeHold lets through.

- [ ] **Step 1: Write the failing tests**

```go
func TestMergerBarrierLaggingTrack(t *testing.T)
// presentation merger, 3 video tracks, track 2 lags 2 frames in DTS (more B-frames): hub order has every
// frame with PTS < K of every track before IDR_0(K), then IDR_0, IDR_1, IDR_2.
func TestMergerBarrierLeadingTrack(t *testing.T)   // track 2 leads by 2 frames: same order
func TestMergerBarrierMissingIDR(t *testing.T)
// track 1 has no IDR at K: tracks 0 and 2 are not held past their own frames; nothing waits for MergeHold.
func TestMergerBarrierPruneKeepsOrder(t *testing.T)
// 50 barriers with pruning: audio and video keys never decrease per track; a track that skipped barrier 3
// (no IDR) is ordered with the others again from barrier 4.
func TestPresentationStartsOnRungZero(t *testing.T)
// run start and a fake-child respawn, copy rung present: the presentation's first video frame of each epoch
// is track 0's IDR, followed by the other tracks' IDRs at the same PTS.
func TestMergerBarrierHoldRunsOut(t *testing.T)
// track 1 silent: after MergeHold the others are released (fake clock), as the M4a hold tests do.
func TestPresentationOff(t *testing.T)            // Presentation() == nil; the rendition mergers keep no barrier
func TestPresentationTracks(t *testing.T)
// fake child, ladder {enc 240, enc 120, copy}, AAC audio: per track, the presentation's frames are exactly
// rendition i's video (PTS, DTS, Key, Epoch, Data) relabelled to i, and the audio once, relabelled to 3+src.
func TestPresentationEpochsMatchRenditions(t *testing.T)
// a fake child exit (respawn): the presentation's epochs and every Anchor equal rendition 0's.
func TestPresentationLateSubscriber(t *testing.T)
// SubscribeCached after 3 GOPs: the replay starts with IDR_0 and holds every track's IDR of that GOP.
func TestPresentationStats(t *testing.T)           // PresentationOut counts its writes; FramesOut unchanged
```

- [ ] **Step 2: Run** `go test -race -count=1 -run 'TestMergerBarrier|TestPresentation' ./internal/transcode/` — compile failure.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/transcode/...` — pass.
- [ ] **Step 5: Commit** `feat(transcode): write the ladder's presentation hub behind a keyframe barrier`

### Task 3: osconform ABR rules

**Files:**
- Modify: `internal/conform/read.go`, `internal/conform/container.go` (rule constants)
- Test: `internal/conform/container_test.go` (httptest servers with hand-built playlists, MPDs and segments, as the existing tests do)

**Interfaces:**
- Produces: `RuleVariantAlignment = "variant-aligned"`, `RuleVariantBandwidth = "variant-bandwidth"`.

Rules:
- **variant-aligned, HLS** (only with `AlignedSequences`): for each media sequence number with a real (non-gap) entry in two or more playlists that carry video, the PTS of each entry's first video frame in decode order is equal, exactly, in 90 kHz; an msn is compared only when every compared entry starts with a keyframe (Forced segments do not).
- **variant-aligned, DASH:** in each period, for an AdaptationSet with `segmentAlignment="true"` and two or more Representations, the Representations list the same segments: equal resolved timelines (`t`, `d`, in 90 kHz) over the period. `$Number$` may differ by a constant (R-B6).
- **variant-bandwidth, HLS** (only with `AlignedSequences`): for each variant, the measured peak over media sequence numbers of Σ(bytes × 8 / EXTINF) over its own playlist and its audio group's DEFAULT rendition playlist does not exceed 1.1 × the largest BANDWIDTH any master read gave that variant URI.
- Each rule reports once per pair of playlists or Representations (as `b.once`).

- [ ] **Step 1: Write the failing tests**

```go
func TestVariantAlignedHLS(t *testing.T)         // two TS variants, equal starts: no violation
func TestVariantMisalignedHLS(t *testing.T)      // variant B's segment 3 starts 3600 later: one RuleVariantAlignment
func TestVariantAlignedIgnoresGaps(t *testing.T)  // B has a gap at msn 2: no violation
func TestVariantAlignedOpenGOP(t *testing.T)      // B's segment carries leading pictures below its keyframe's PTS: no violation
func TestVariantAlignedSkipsForced(t *testing.T)   // entries not starting with a keyframe are not compared
func TestVariantAlignedDASH(t *testing.T)         // 2 reps, equal timelines (startNumbers differing): none; rep 2 missing one segment: one; same with segmentAlignment absent: none; rep 2 shifted 3600: one
func TestVariantBandwidth(t *testing.T)           // BANDWIDTH below 1/1.1 of the measured peak: one RuleVariantBandwidth; at the peak: none
```

- [ ] **Step 2: Run** `go test -race -count=1 -run 'TestVariant' ./internal/conform/` — compile failure.
- [ ] **Step 3: Implement**; record per playlist per msn the segment's byte count and first video PTS while reading.
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/conform/ ./cmd/osconform/` — pass.
- [ ] **Step 5: Commit** `feat(conform): check ABR variants for alignment and honest bandwidth`

### Task 4: Packager ABR mode — fMP4 master and DASH (R-B5, R-B6)

**Files:**
- Modify: `internal/output/live/packager.go` (`Options.ABR`, pass `AlignVideo`), `internal/output/live/hls.go` (`buildMaster`), `internal/output/live/dash.go` (`buildPeriod`/`buildSet`)
- Test: `internal/output/live/hls_test.go`, `internal/output/live/dash_test.go`

**Interfaces:**
- Consumes: `segment.Options.AlignVideo` (Task 1).
- Produces: `live.Options.ABR bool` — every video track is a variant of the same content.

`cmaf/master.m3u8` in ABR mode: one `EXT-X-STREAM-INF` per known video track (ascending track = rendition order), URI `v{track}.m3u8`, RESOLUTION the largest over the window (as today), CODECS = its codec strings over the window + the lowest audio track's codec, `AUDIO="aud"` when there is audio, BANDWIDTH per R-B5. The one `EXT-X-MEDIA` audio rendition stays. The MPD per R-B6: Representation `id` = track, `SegmentTemplate` at Representation level with `Initialization` `init-{track}-{epoch}.mp4` and `Media` `{track}-$Number$.m4s`.

- [ ] **Step 1: Write the failing tests** (a synthetic 3-video + 1-audio hub: tracks 0..2 H.264 at 640×360, 426×240, 320×180, AAC track 3, keyframes at the same PTS, written in barrier order; helper `synthLadder(t, epoch, start, n)` added to `packager_test.go`)

```go
func TestABRMaster(t *testing.T)
// 3 variants in track order, URIs v0/v1/v2.m3u8, RESOLUTION as configured, CODECS "avc1.…,mp4a.40.2",
// AUDIO "aud", BANDWIDTH == peak of (video part bytes + audio part bytes)*8/duration per segment.
func TestABRTrackPlaylistsAligned(t *testing.T)
// v0, v1, v2 and a3 share MEDIA-SEQUENCE, entry count and every entry's DISCONTINUITY.
func TestABRMasterNonABRUnchanged(t *testing.T)
// the same hub with ABR off: master byte-identical to buildMaster's output before this task (golden).
func TestABRMPD(t *testing.T)
// one video AdaptationSet with 3 Representations (ids "0","1","2", width/height, codecs), segmentAlignment,
// per-Representation SegmentTemplate whose timelines are equal; one audio set as before.
func TestABRMPDGapDropsAlignment(t *testing.T)
// track 1 left out of one segment: in that period the set has no segmentAlignment; rep 1's timeline lacks that
// segment and its media numbers still resolve to served parts.
func TestABRMPDTwoCodecs(t *testing.T)
// tracks 0,1 H.264 and 2 H.265: two video sets.
func TestABRMPDFFprobe(t *testing.T)
// ffprobe reads the ABR MPD and finds 3 video streams and 1 audio stream (skips without ffprobe).
```

- [ ] **Step 2: Run** `go test -race -count=1 -run 'TestABR' ./internal/output/live/` — compile failure.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/output/live/` — pass, existing tests unchanged.
- [ ] **Step 5: Commit** `feat(live): serve an ABR ladder's fMP4 master and DASH representations`

### Task 5: Packager ABR mode — TS variants (R-B4)

**Files:**
- Modify: `internal/output/live/packager.go` (per-variant TS muxers), `internal/output/live/store.go` (keep variant TS bytes), `internal/output/live/http.go` (serve `{track}-{seq}.ts`), `internal/output/live/hls.go` (TS master and variant playlists)
- Test: `internal/output/live/hls_test.go`, `internal/output/live/packager_test.go`

**Interfaces:**
- Consumes: `live.Options.ABR` (Task 4).
- Produces: `SegmentInfo.VariantTS []VariantTS` with `type VariantTS struct { Track int; Data []byte }` (ascending Track; nil when not ABR). In ABR mode `SegmentInfo.TS` is nil.

One `ts.Muxer` per video track, kept across segments (continuity counters), fed that track's frames relabelled to track 0 and every audio frame relabelled to 1 + its rank among the window's known audio tracks (`Snapshot.Tracks`, ascending), so PIDs stay put across segments. A variant whose video has no frame in the segment gets no TS there. A mux failure counts in `Errors()` and leaves that variant's TS out of the segment (its playlist lists a gap). `hls/index.m3u8` in ABR mode: multivariant, version 3, one variant per video track with a TS in the window or known (track order), URI `v{track}.m3u8`, CODECS video + every audio codec, RESOLUTION, BANDWIDTH = peak variant TS bytes × 8 / segment duration. `hls/v{track}.m3u8`: version 3, MEDIA-SEQUENCE = first listed Seq, DISCONTINUITY-SEQUENCE = `EvictedDiscontinuities`, one entry per listed segment with the segment's Discontinuity, EXTINF = segment duration, gap entries for missing TS, PDT on the first entry and every discontinuity, one TARGETDURATION shared by all variants (key `hls-abr`), EXT-X-INDEPENDENT-SEGMENTS per `independent`, ENDLIST per `ended`.

- [ ] **Step 1: Write the failing tests** (`synthLadder` from Task 4)

```go
func TestABRTSMaster(t *testing.T)        // 3 variants, v0..v2.m3u8, CODECS video+aac, BANDWIDTH = peak TS bitrate
func TestABRTSVariants(t *testing.T)
// v0..v2: equal MEDIA-SEQUENCE, entry count, DISCONTINUITY flags, TARGETDURATION; every {t}-{seq}.ts
// answers 200, starts with PAT/PMT, video PID 0x100 in every variant, first video PES is a keyframe
// at the segment's Start.
func TestABRTSMuxFailureLeavesGap(t *testing.T)
// one track-1 video frame with empty Data in one segment (the TS muxer rejects it, mux.go "video frame has no
// data"): v1 lists gap-1-{seq}.ts there; v0, v2 real; Errors() counts the TS failure (and CMAF's, asserted separately).
func TestABRTSNoCombinedTS(t *testing.T)  // SegmentInfo.TS nil, {seq}.ts answers 404 in ABR mode
func TestABRTSConform(t *testing.T)
// conform.ReadHLS(hls/index.m3u8, AlignedSequences, Once) over a served ABR packager: no violation,
// 3 video tracks and 3 audio captures (one per variant).
```

- [ ] **Step 2: Run** `go test -race -count=1 -run 'TestABRTS' ./internal/output/live/` — fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/output/live/` — pass.
- [ ] **Step 5: Commit** `feat(live): serve an ABR ladder as HLS-TS variants`

### Task 6: osdev serves `<name>_abr` (R-B2)

**Files:**
- Modify: `cmd/osdev/main.go`
- Test: `cmd/osdev/main_test.go`

**Interfaces:**
- Consumes: `transcode.Options.Presentation`, `Transcoder.Presentation()` (Task 2); `live.Options.ABR` (Task 4).

Every stream with `-rung` gets `transcode.Options.Presentation: true`; its presentation is packaged with `live.Options{ABR: true}` through a subscription of `subQueue × (renditions + 1)` and found by `packLookup` as `<name>_abr` only. The RTMP, RTSP, SRT, HTTP-TS lookups and `copy://` resolution do not find it. `parseRung` rejects the label `abr`; a stream named `<name>_abr` that already exists is an error, as for renditions. The usage text and package doc name the presentation.

- [ ] **Step 1: Write the failing tests**

```go
func TestParseRungRejectsABRLabel(t *testing.T)   // -rung cam=abr:360:800 → error naming the label
func TestABRNameTaken(t *testing.T)               // a stream "cam_abr" plus -rung cam=… → error
func TestABRServedOnlyOverHTTP(t *testing.T)
// a copy-only ladder needs no child (-rung v=src:copy -audio v=copy, as TestOsdevCopyLadder): GET
// /live/v_abr/cmaf/master.m3u8 and /live/v_abr/hls/index.m3u8 → 200 once segments exist; /live/v_abr/stream.ts → 404;
// an RTMP play and an RTSP DESCRIBE of v_abr are refused; -input w=copy://v_abr fails at start-up.
```

- [ ] **Step 2: Run** `go test -race -count=1 ./cmd/osdev/` — fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test -race -count=1 ./cmd/osdev/` — pass.
- [ ] **Step 5: Commit** `feat(osdev): serve a ladder's ABR presentation as <name>_abr`

### Task 7: The M4b scenarios

**Files:**
- Create: `internal/e2e/m4b_test.go`
- Modify: `internal/e2e/m4a_test.go` and `m3a_test.go` only by adding functions: existing helpers keep their signatures (`transcodeSteps` becomes a wrapper over the new recorder)

**Interfaces:**
- Consumes: Tasks 1–5.
- Produces (for Task 8):
  - `m4bRuns(t, bin, fixtures...) map[string]m4bRun` with `type m4bRun struct { presentation []step; renditions [][]step }`, recorded from one transcoder run per fixture (ladder `m4aLadder` with `Presentation: true`); the run must be clean as `transcodeSteps` requires, plus `PresentationDropped == 0`.
  - `feedABR(t, steps, queue int) (*live.Packager, base string, caps map[string]conform.Capture)`: packages the steps with `live.Options{ABR: true}` through a subscription of `queue` (0 = `bigQueue`), serves them, and reads hls-ts (`hls/index.m3u8`), hls-fmp4 (`cmaf/master.m3u8`) and dash (`cmaf/index.mpd`); no http-ts.
  - `assertCompleteABR(t, caps, written)`: per format, every written video frame of every track is captured once, and the audio once per hls-ts variant, once in hls-fmp4 and dash.

Every read uses `conform.ReadOptions{AlignedSequences: true}`, so `RuleVariantAlignment` and `RuleVariantBandwidth` apply.

- [ ] **Step 1: Write the tests**

```go
func TestM4bMatrix(t *testing.T)
// fixtures h264_aac.ts, h264_aac_then_640.ts, h264_aac_nob.ts, h264_pyramid.ts through the 4-rendition
// ladder; per row the presentation is packaged in ABR mode and read as hls-ts (hls/index.m3u8),
// hls-fmp4 (cmaf/master.m3u8) and dash (cmaf/index.mpd):
//   steady-state: no violation; 1 epoch; masters list 4 variants; MPD has one H.264 video set with 4 reps
//   epoch-change: epochs [1 2] in every format, one discontinuity at the same msn in every variant
//   slow-consumer: feedABR with a queue too small for a 3× paced replay (as TestM3aSlowPackager):
//                  Lost > 0, no violation, every variant's discontinuities at equal msn (completeness not asserted)
//   no-bframes, b-frames: no violation; presentation track i's frames equal rendition i's video frames of the same run
func TestM4bSwitchTS(t *testing.T)
// fetch segment n from variant n%4 of hls/index.m3u8 (the next variant with a real entry when that one is a
// gap; at least 3 distinct variants used) for every listed n into files, list them for
// ffmpeg's concat demuxer and decode with `ffmpeg -v error -f concat -safe 0 -i list -f null -`: exit 0,
// empty stderr; ffprobe on the same list: video frame PTS strictly increase.
func TestM4bRespawn(t *testing.T)
// h264_aac.ts fed once at its pace, the child killed once after its 3rd output keyframe (TestM4aRespawn's
// kill helper): the presentation's epochs are [1 2]; in hls-ts and hls-fmp4 every variant has exactly one
// discontinuity, at the same msn; the MPD has 2 periods, each with the 4 video Representations.
```

- [ ] **Step 2: Run** `make test-transcode` scoped: `OS_TRANSCODER=$PWD/dist/ostranscoder-amd64/ostranscoder go test -race -count=1 -run 'TestM4b' ./internal/e2e/` — pass.
- [ ] **Step 3: Run** the same in `make test-ci-peers` (`--exec` with the e2e binary) — pass.
- [ ] **Step 4: Commit** `test(e2e): the M4b ABR scenarios`

### Task 8: Players, CI list and spec — M4b done

**Files:**
- Modify: `internal/e2e/m4b_test.go`, `.github/workflows/ci.yml` (`E2E_TESTS`), `docs/REBUILD_PLAN.md`, `CLAUDE.md` (only if a make target changes)

- [ ] **Step 1: Write the test**

```go
func TestM4bPlayers(t *testing.T)
// the h264_aac.ts presentation, served live (paced ×3):
//   ffprobe on hls/index.m3u8, cmaf/master.m3u8 and cmaf/index.mpd: 4 video streams (heights 240,180,120,source) and audio
//   VLC (as TestM4aVLC, with its `tolerate` field) plays each of the three for 6 s without other errors
//   GStreamer playbin (as the M3 GStreamer class check) plays hls/index.m3u8 and cmaf/index.mpd to EOS or 6 s without error
```

- [ ] **Step 2: Run** on the host and in `make test-ci-peers` — pass.
- [ ] **Step 3: CI:** add `TestM4bMatrix TestM4bRespawn` to `E2E_TESTS` of `transcode-e2e-arm64` (the switch and players tests need ffmpeg/VLC peers that job lacks; they run in `test`).
- [ ] **Step 4: Spec:** in `docs/REBUILD_PLAN.md`: §3.4 a bullet "ABR presentation (M4b)" with R-B1, R-B3, R-B4, R-B6 and R-B8 as rules; §3.5 the presentation hub; §5 row M4b → done with this plan's link and the tests that prove it; an "Out of M4b" table (AVERAGE-BANDWIDTH and FRAME-RATE; I-frame playlists; several audio languages; ABR over RTMP/RTSP/SRT/HTTP-TS; LL-HLS); Backlog: the respawn segment that can raise TARGETDURATION (see Rulings), and rung 0's extra x264 IDR at `keyint=600` (a source GOP above 600 frames), which cuts the presentation where the other rungs have no keyframe, so R-B8 leaves them out of that segment.
- [ ] **Step 5: Commit** `docs: M4b done` and `ci: run the M4b scenarios on arm64`
