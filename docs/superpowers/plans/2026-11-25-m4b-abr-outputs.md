# M4b: ABR Outputs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve a transcoder ladder as one ABR presentation: an HLS multivariant playlist (TS and fMP4) and a DASH MPD with one representation per rung, whose segments align across rungs by construction.

**Architecture:** The transcoder writes one more hub, the *presentation*, with every rendition's video as its own track and the audio once (ruling R-B1). One `live.Packager` in ABR mode packages it, so one segmenter cuts every rung, and M3b3's per-segment entries with `EXT-X-GAP` placeholders keep media sequence and discontinuity numbers equal across variants. The segmenter learns to cut each video track at its own keyframe (R-B3). `osconform` gains two ABR rules: variants aligned, and BANDWIDTH honest.

**Tech Stack:** Go 1.25, existing `internal/segment`, `internal/transcode`, `internal/output/live`, `internal/conform`, `pkg/hls`, `pkg/dash`; peers ffmpeg/ffprobe, VLC, GStreamer as in M3/M4a.

**Spec:** `docs/REBUILD_PLAN.md` (§3.4 Output kit, §3.5 Transcoder, §5 row M4b). M4a plan: `docs/superpowers/plans/2026-11-24-m4a-transcoder-core.md`.

## Global Constraints

- **Libraries:** no new dependency. Only the approved independent libraries; no bluenviron, no lal, no code copied from another product.
- **cgo:** nothing in this plan touches `internal/transcode/native` or `cmd/ostranscoder`. Without the `libav` tag, `go build ./...`, `go vet ./...`, `golangci-lint run ./...` and `make test` keep passing, as do `GOOS=windows go vet ./...` and `GOOS=darwin go vet ./...`.
- **Non-ABR behaviour is frozen:** with `live.Options.ABR` false and `segment.Options.AlignVideo` false, every playlist, MPD, segment byte and counter is what M4a produced. Every existing test passes unchanged.
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
| R-B3 | `segment.Options.AlignVideo`: at a keyframe cut at K, every non-anchor video track is cut at its own first keyframe with PTS ≥ K, and the closed segment is held (with the audio hold, same 1 s timeout) until each such track has delivered it. An aligned track enters each epoch, and resumes after `Lost`, at its first keyframe. MaxDuration and anchor-change cuts do not align video | A rung without a keyframe at K (a source GOP above x264's `keyint=600`) delays the segment's release by up to 1 s and its next part does not start on a keyframe |
| R-B4 | HLS-TS in ABR mode: one TS per segment per video track (that track plus every audio track), muxed with the video at track index 0 so every variant has the same PIDs. `hls/index.m3u8` is the multivariant playlist; `hls/v{track}.m3u8` lists one entry per segment, `{track}-{Seq}.ts` or a `gap-{track}-{Seq}.ts` placeholder, MEDIA-SEQUENCE = first Seq. No combined TS is muxed | TS bytes muxed N times; a TS client sees a variant's PMT name only its own video |
| R-B5 | Variants are listed in rendition order. BANDWIDTH is the window's peak over segments of (variant video part bytes + default audio part bytes) × 8 / duration, as `buildMaster` does today; for TS, the variant TS's bytes. No AVERAGE-BANDWIDTH or FRAME-RATE | A player that starts on the first variant starts on rung 0; ABR heuristics lack the average |
| R-B6 | DASH in ABR mode: video tracks of one codec share an AdaptationSet (`segmentAlignment="true"`), each Representation with its own SegmentTemplate (init, media, startNumber, timeline, presentationTimeOffset). Audio and non-ABR streams keep today's one-set-per-track layout | Two codecs in one ladder (an H.265 copy rung among H.264 rungs) give two video sets, so a player switches only within one |
| R-B7 | The presentation hub's `GOPCacheFrames` is 1500 × (renditions + 1) | Memory: N rungs' GOP in one cache |

## Execution waves (parallel per user instruction)

| Wave | Tasks | Why this order |
|---|---|---|
| 1 | 1 (segmenter), 2 (presentation hub), 3 (conform ABR rules) | Independent packages |
| 2 | 4 (packager ABR: fMP4 master, DASH) | Needs Task 1's `AlignVideo` |
| 3 | 5 (packager ABR: TS variants), 6 (osdev) | Both need Task 4's `Options.ABR`; 6 needs Task 2 |
| 4 | 7 (M4b scenarios) | Needs 2–6 |
| 5 | 8 (players, CI list, spec) | Needs Task 7's helpers |

Each parallel task runs in its own worktree; the controller merges.

## Review Focus

1. **A rung lagging the anchor by a few frames in decode order** (B-frames 3 on the copy rung, 2 on the encoded ones): its pre-K frames arrive after the anchor's keyframe at K and must still close the old segment. Task 1 tests it.
2. **A rung leading the anchor** (its IDR at K arrives before the anchor's): the IDR must open the next segment, not end the old one. Task 1 tests it.
3. **One slow packager subscription** (`Lost`) in a presentation: one discontinuity, at the same media sequence number, in every variant, and every variant resumes on a keyframe. Task 7's slow-consumer row tests it.
4. **A respawned child**: the presentation gets exactly one new epoch, at one media sequence number in every variant. Task 7 tests it.
5. **Switching variants at a segment boundary** decodes cleanly: alternating segments from different TS variants form a stream ffmpeg decodes without error, with continuous PTS. Task 7 tests it.

---

### Task 1: Segmenter aligns video tracks (R-B3)

**Files:**
- Modify: `internal/segment/segmenter.go`
- Test: `internal/segment/segmenter_test.go`

**Interfaces:**
- Produces: `segment.Options.AlignVideo bool`. Off: behaviour unchanged.

Rules (in the package doc as well), all only when `AlignVideo` is set:
- An *aligned track* is a video track of the epoch other than the anchor.
- An aligned track is *synced* from its first keyframe in the epoch; its frames before that are dropped (`Dropped`). A `Lost` delivery unsyncs every aligned track. An aligned keyframe that arrives while no segment is open (it leads the anchor's first keyframe) is kept in the carry, bounded like leading audio by MaxDuration, and joins the segment the anchor opens if its PTS ≥ that segment's Start; otherwise it and its followers are dropped until the next keyframe.
- At a keyframe cut at K: an aligned track's frames in the closing segment from its first keyframe with PTS ≥ K (arrival order) onward move to the carry. The held segment waits for every synced, not timed-out aligned track that has not yet delivered such a keyframe; its frames that arrive meanwhile go into the held segment. Release follows the existing rule: all waited-for tracks arrived, or any track of the epoch reached K + `holdTimeout`, which times out the waiting tracks until their next keyframe.
- MaxDuration and anchor-change cuts move no video.

- [ ] **Step 1: Write the failing tests** (two H.264 tracks 0 and 1 at 25 fps, GOP 1 s, target 2 s, plus AAC track 2 where stated; helpers `frame`, `bframes`, `shifted`, `pushAll` exist)

```go
func TestAlignVideoLaggingTrack(t *testing.T)
// track 1 delivered 2 frames behind track 0 in arrival order, same keyframe PTS.
// Every segment: track 1's first frame is Key with PTS == seg.Start; no track-1 frame has PTS >= next seg.Start.
// Dropped() == 0.

func TestAlignVideoLeadingTrack(t *testing.T)
// track 1 delivered 2 frames ahead of track 0. Same assertions, including the first segment
// (track 1's IDR at 0 arrives before the anchor's).

func TestAlignVideoBFramesBothTracks(t *testing.T)
// both tracks with B-frames (bframes(n)), track 1 lagging by 3 frames: per track, frames inside a
// segment are in DTS order and the PTS set of segment n is exactly [Start_n, Start_{n+1}).

func TestAlignVideoAfterLost(t *testing.T)
// a Lost delivery on a track-0 keyframe while track 1 is mid-GOP: the next segment holds no track-1
// frame before track 1's next keyframe; those frames count in Dropped.

func TestAlignVideoMissingKeyframe(t *testing.T)
// track 1 lacks its keyframe at K=180000: the segment cut at K is released once some track reaches
// K+90000, not before; track 1's frames after the release go to the open segment in arrival order
// until its next keyframe, from which it is aligned again.

func TestAlignVideoWithAudio(t *testing.T)
// tracks 0, 1 and AAC 2 (audio lagging 0.5 s): audio still cut at K (assertAudioInside) and video aligned.

func TestAlignVideoForcedCutMovesNoVideo(t *testing.T)
// GOP 20 s, MaxDuration 10 s: forced segments keep track 1 in arrival order (as with AlignVideo off).
```

- [ ] **Step 2: Run** `go test -race -count=1 -run 'TestAlignVideo' ./internal/segment/` — expect compile failure (`AlignVideo` undefined).
- [ ] **Step 3: Implement** the rules above in `segmenter.go`. Track per aligned track: synced, and per held segment whether its keyframe ≥ k arrived. Reuse `timedOut` for video tracks.
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/segment/` — all pass, existing tests included.
- [ ] **Step 5: Commit** `feat(segment): align every video track's cut on its own keyframe`

### Task 2: The transcoder's presentation hub (R-B1, R-B7)

**Files:**
- Modify: `internal/transcode/transcode.go`, `internal/transcode/merge.go` (only if the merger needs a relabelling hook)
- Test: `internal/transcode/transcode_test.go` (with `transcodetest` fake child)

**Interfaces:**
- Produces: `transcode.Options.Presentation bool`; `func (t *Transcoder) Presentation() *hub.Hub` (nil when off; closed by Run like the rendition hubs); `func PresentationTrack(rendition int) int` returns `rendition`; `func PresentationAudioTrack(renditions, srcTrack int) int` returns `renditions + srcTrack`.

One more merger writes the presentation from the same frames the rendition mergers get, each frame copied with its `Track` relabelled: a rendition's video (child rung or copy path) to `PresentationTrack(i)`, audio once (not once per rendition) to `PresentationAudioTrack`. Anchors come from the shared `anchorStore`, so the presentation's epochs and anchors equal the renditions'. `expectVideo` also announces the relabelled tracks of every encoded rendition to the presentation merger. Hub option `GOPCacheFrames: 1500 * (len(Renditions)+1)`.

- [ ] **Step 1: Write the failing tests**

```go
func TestPresentationOff(t *testing.T)            // Presentation() == nil by default
func TestPresentationTracks(t *testing.T)
// fake child, ladder {enc 240, enc 120, copy}, AAC audio: the presentation's frames are, per track,
// exactly rendition i's video frames (PTS, DTS, Key, Epoch, Data) relabelled to track i, and the
// audio frames once, relabelled to 3+src; written in non-decreasing (epoch, DTS) order per track.
func TestPresentationEpochsMatchRenditions(t *testing.T)
// a fake child exit (respawn): the presentation's epoch sequence and every Anchor equal rendition 0's.
func TestPresentationAudioWaitsForVideo(t *testing.T)
// as the M4a merger test for encoded rungs: no presentation audio before the epoch's first video frame.
```

- [ ] **Step 2: Run** `go test -race -count=1 -run 'TestPresentation' ./internal/transcode/` — compile failure.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/transcode/...` — pass.
- [ ] **Step 5: Commit** `feat(transcode): write the ladder's presentation hub`

### Task 3: osconform ABR rules

**Files:**
- Modify: `internal/conform/read.go`, `internal/conform/container.go` (rule constants)
- Test: `internal/conform/container_test.go` (httptest servers with hand-built playlists, MPDs and segments, as the existing tests do)

**Interfaces:**
- Produces: `RuleVariantAlignment = "variant-aligned"`, `RuleVariantBandwidth = "variant-bandwidth"`.

Rules:
- **variant-aligned, HLS** (only with `AlignedSequences`): for each media sequence number with a real (non-gap) entry in two or more playlists that carry video, the smallest video PTS of those entries' segments is equal (exactly, in 90 kHz).
- **variant-aligned, DASH:** in each period, for an AdaptationSet with `segmentAlignment="true"` and two or more Representations, the resolved timeline start times, rescaled to 90 kHz, are the same set over the span common to all of them.
- **variant-bandwidth, HLS** (only with `AlignedSequences`): for each variant, the measured peak over media sequence numbers of Σ(bytes × 8 / EXTINF) over its own playlist and its audio group's DEFAULT rendition playlist must not exceed 1.1 × the largest BANDWIDTH any master read gave that variant URI.
- Each rule reports once per pair of playlists/representations (as `b.once`).

- [ ] **Step 1: Write the failing tests**

```go
func TestVariantAlignedHLS(t *testing.T)        // two TS variants, equal starts: no violation
func TestVariantMisalignedHLS(t *testing.T)     // variant B's segment 3 starts 1 frame (3600) later: one RuleVariantAlignment
func TestVariantAlignedIgnoresGaps(t *testing.T) // B has a gap at msn 2: no violation
func TestVariantAlignedDASH(t *testing.T)       // set with 2 reps, equal timelines: none; shifted t in rep 2: one violation
func TestVariantBandwidth(t *testing.T)         // BANDWIDTH below 1/1.1 of the measured peak: one RuleVariantBandwidth; at the peak: none
```

- [ ] **Step 2: Run** `go test -race -count=1 -run 'TestVariant' ./internal/conform/` — compile failure.
- [ ] **Step 3: Implement**; record per playlist per msn the segment's byte count and smallest video PTS while reading.
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/conform/ ./cmd/osconform/` — pass.
- [ ] **Step 5: Commit** `feat(conform): check ABR variants for alignment and honest bandwidth`

### Task 4: Packager ABR mode — fMP4 master and DASH (R-B5, R-B6)

**Files:**
- Modify: `internal/output/live/packager.go` (`Options.ABR`, pass `AlignVideo`), `internal/output/live/hls.go` (`buildMaster`), `internal/output/live/dash.go` (`buildPeriod`/`buildSet`)
- Test: `internal/output/live/hls_test.go`, `internal/output/live/dash_test.go`

**Interfaces:**
- Consumes: `segment.Options.AlignVideo` (Task 1).
- Produces: `live.Options.ABR bool` — every video track is a variant of the same content.

`cmaf/master.m3u8` in ABR mode: one `EXT-X-STREAM-INF` per known video track (ascending track = rendition order), URI `v{track}.m3u8`, RESOLUTION from its latest config, CODECS = its codec + the lowest audio track's codec, `AUDIO="aud"` when there is audio, BANDWIDTH per R-B5. The one `EXT-X-MEDIA` audio rendition stays. The MPD per R-B6: Representation `id` = track, `SegmentTemplate` at Representation level with `Initialization` `init-{track}-{epoch}.mp4` and `Media` `{track}-$Number$.m4s`.

- [ ] **Step 1: Write the failing tests** (a synthetic 3-video + 1-audio hub: tracks 0..2 H.264 at 640×360, 426×240, 320×180, AAC track 3, aligned keyframes; helper `synthLadder(t, epoch, start, n)` added to `packager_test.go`)

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

One `ts.Muxer` per video track, kept across segments (continuity counters), fed that track's frames relabelled to track 0 and every audio frame relabelled to 1 + its rank among the epoch's audio tracks (ascending track), so PIDs stay put across segments. A mux failure counts in `Errors()` and leaves that variant's TS out of the segment (its playlist lists a gap). `hls/index.m3u8` in ABR mode: multivariant, version 3, one variant per video track with a TS in the window or known (track order), URI `v{track}.m3u8`, CODECS video + every audio codec, RESOLUTION, BANDWIDTH = peak variant TS bytes × 8 / segment duration. `hls/v{track}.m3u8`: version 3, MEDIA-SEQUENCE = first listed Seq, DISCONTINUITY-SEQUENCE = `EvictedDiscontinuities`, one entry per listed segment with the segment's Discontinuity, EXTINF = segment duration, gap entries for missing TS, PDT on the first entry and every discontinuity, one TARGETDURATION shared by all variants (key `hls-abr`), EXT-X-INDEPENDENT-SEGMENTS per `independent`, ENDLIST per `ended`.

- [ ] **Step 1: Write the failing tests** (`synthLadder` from Task 4)

```go
func TestABRTSMaster(t *testing.T)        // 3 variants, v0..v2.m3u8, CODECS video+aac, BANDWIDTH = peak TS bitrate
func TestABRTSVariants(t *testing.T)
// v0..v2: equal MEDIA-SEQUENCE, entry count, DISCONTINUITY flags, TARGETDURATION; every {t}-{seq}.ts
// answers 200, starts with PAT/PMT, video PID 0x100 in every variant, first video PES is a keyframe
// at the segment's Start.
func TestABRTSMuxFailureLeavesGap(t *testing.T)
// a failing muxer for track 1 on one segment: v1 lists gap-1-{seq}.ts there; v0, v2 real; Errors() == 1.
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

Every stream with `-rung` gets `transcode.Options.Presentation: true`; its presentation is packaged with `live.Options{ABR: true}` and found by `packLookup` as `<name>_abr` only. The RTMP, RTSP, SRT, HTTP-TS lookups and `copy://` resolution do not find it. `parseRung` rejects the label `abr`; a stream named `<name>_abr` that already exists is an error, as for renditions. The usage text and package doc name the presentation.

- [ ] **Step 1: Write the failing tests**

```go
func TestParseRungRejectsABRLabel(t *testing.T)   // -rung cam=abr:360:800 → error naming the label
func TestABRNameTaken(t *testing.T)               // a stream "cam_abr" plus -rung cam=… → error
func TestABRServedOnlyOverHTTP(t *testing.T)
// osdev wiring over a fake child (transcodetest): GET /live/cam_abr/cmaf/master.m3u8 → 200 once
// segments exist; the RTMP/RTSP/SRT/HTTP-TS lookups of "cam_abr" report not found; copy://cam_abr is refused.
```

- [ ] **Step 2: Run** `go test -race -count=1 ./cmd/osdev/` — fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test -race -count=1 ./cmd/osdev/` — pass.
- [ ] **Step 5: Commit** `feat(osdev): serve a ladder's ABR presentation as <name>_abr`

### Task 7: The M4b scenarios

**Files:**
- Create: `internal/e2e/m4b_test.go`
- Modify: `internal/e2e/m4a_test.go` (only to let `transcodeSteps` also record the presentation and `feedScenario`/`packFeed` take `live.Options`)

**Interfaces:**
- Consumes: Tasks 1–5.
- Produces (for Task 8): `m4bRuns(t, bin, fixtures...) map[string]m4bRun` with `type m4bRun struct { presentation []step; renditions [][]step }`, recorded from one transcoder run per fixture (ladder `m4aLadder` with `Presentation: true`), `feedABR(t, steps) (*live.Packager, base string, caps map[string]conform.Capture)`.

Every read uses `conform.ReadOptions{AlignedSequences: true}`, so `RuleVariantAlignment` and `RuleVariantBandwidth` apply.

- [ ] **Step 1: Write the tests**

```go
func TestM4bMatrix(t *testing.T)
// fixtures h264_aac.ts, h264_aac_then_640.ts, h264_aac_nob.ts, h264_pyramid.ts through the 4-rendition
// ladder; per row the presentation is packaged in ABR mode and read as hls-ts (hls/index.m3u8),
// hls-fmp4 (cmaf/master.m3u8) and dash (cmaf/index.mpd):
//   steady-state: no violation; 1 epoch; masters list 4 variants; MPD has one H.264 video set with 4 reps
//   epoch-change: epochs [1 2] in every format, one discontinuity at the same msn in every variant
//   slow-consumer: the packager's subscription queue too small for a 3× paced replay (as TestM3aSlowPackager):
//                  Lost > 0, no violation, every variant's discontinuities at equal msn
//   no-bframes, b-frames: no violation; presentation track i's frames equal rendition i's video frames of the same run
func TestM4bSwitchTS(t *testing.T)
// fetch segment n from variant n%4 of hls/index.m3u8 for every listed n, concatenate, decode with
// `ffmpeg -v error -xerror -i - -f null -`: exit 0, empty stderr; ffprobe frame PTS strictly increase.
func TestM4bRespawn(t *testing.T)
// kill the child once mid-run (as TestM4aRespawn): the presentation has exactly 2 epochs, and in hls-ts,
// hls-fmp4 and dash every variant has its one discontinuity at the same msn / a second period.
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
- [ ] **Step 4: Spec:** in `docs/REBUILD_PLAN.md`: §3.4 a bullet "ABR presentation (M4b)" with R-B1/R-B3/R-B4/R-B6 as rules; §3.5 the presentation hub; §5 row M4b → done with this plan's link and the tests that prove it; an "Out of M4b" table (AVERAGE-BANDWIDTH and FRAME-RATE; I-frame playlists; several audio languages; ABR over RTMP/RTSP/SRT/HTTP-TS; LL-HLS); Backlog: the `keyint=600` misalignment (R-B3's cost) if not fixed.
- [ ] **Step 5: Commit** `docs: M4b done` and `ci: run the M4b scenarios on arm64`
