# M4c: NVENC, Watermark and Thumbnails Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A rung can be encoded by NVIDIA NVENC, a stream can carry a watermark (text with time directives, or an image) on every rung, and every stream, transcoded or passthrough, can publish a JPEG thumbnail every N seconds.

**Architecture:**
- **NVENC** is a per-rung encoder choice inside the existing child. The child still decodes and scales on the CPU and hands `yuv420p` frames to `h264_nvenc`, with the same forced IDR at every source keyframe PTS, so the M4a/M4b alignment is untouched. A rung whose NVENC encoder cannot open falls back to libx264 and is counted.
- **Watermark** is one libavfilter graph (`drawtext` or `overlay`) on the decoded picture, before the per-rung scalers, so one render reaches every encoded rung. It can be replaced on a running child with one wire message, with no epoch.
- **Thumbnails** are taken from the decoded picture in the child (before the watermark), scaled by swscale and encoded by Go's `image/jpeg`. A transcoded stream's child takes them while it transcodes; a passthrough stream gets a child in thumbnail-only mode, fed keyframes only by a small supervisor, `transcode.Thumbnailer`. The latest thumbnail is kept in memory and served by osdev.
- The specs shared by host, wire and child live in one pure-Go package, `internal/transcode/picture`, which CI tests without libav.

**Tech Stack:** Go 1.26.11; go-astiav v0.41.0 (now also its `FilterGraph`); the pinned FFmpeg-Builds 8.1.3 GPL shared build (`h264_nvenc`, libavfilter with `drawtext`/libfreetype and `overlay`); Go stdlib `image/png`, `image/jpeg`, `time/tzdata`; peers ffmpeg/ffprobe, VLC, `nvidia-smi`.

**Spec:** `docs/REBUILD_PLAN.md` (§2 rows "Picture features (M4c)" and "Platforms", §3.5 Transcoder, §5 row M4c, the Backlog item on R-T7b'). Earlier plans: `docs/superpowers/plans/2026-11-24-m4a-transcoder-core.md`, `docs/superpowers/plans/2026-11-25-m4b-abr-outputs.md`.

## Global Constraints

- **Libraries:** no new Go module. Only the approved independent libraries (mp4ff, pion/rtp, pion/sdp, datarhei/gosrt, testify, go-astiav v0.41.0, protobuf `protowire`); no bluenviron, no lal, no code copied from another product. New link dependency: `libavfilter` from the pinned FFmpeg prefix already in `~/.cache/osv2-ffmpeg` (no download). New file in the repo: `internal/transcode/native/font/DejaVuSans.ttf`, copied from this host's `/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf` (fonts-dejavu-core 2.37, 759720 bytes, sha256 `b4c632e3cdf9acc7f28758fb5a323c8524d7fc6660d46904d9b6cbe2809c419c`), with its licence (`/usr/share/doc/fonts-dejavu-core/copyright`: Bitstream Vera and Arev licences, free to redistribute and embed) as `internal/transcode/native/font/LICENSE`. Nothing else is downloaded or installed; in particular the NVIDIA container toolkit is **not** installed (it would reconfigure and restart the Docker daemon).
- **cgo:** only `internal/transcode/native` and `cmd/ostranscoder` use cgo, behind the tag `libav`. Without the tag, `go build ./...`, `go vet ./...`, `golangci-lint run ./...` and `make test` keep passing, as do `GOOS=windows go vet ./...` and `GOOS=darwin go vet ./...`. Native changes are verified with `make test-transcode` and `make ostranscoder` (image `osv2-ci-peers:m4a`); `scripts/bundle-ostranscoder.sh` bundles `libavfilter.so.11` by itself, since it copies every library `ldd` resolves inside the prefix.
- **Frozen behaviour:** with `Options.Watermark` nil, `Options.Thumbnail` nil and every rung's `Encoder` empty or `x264`, every hub write, counter, playlist and segment byte is what M4b produced, and every existing test passes unchanged, except the one wire-version assertion and the one test Task 1 names.
- **GPU facts (this host):** GeForce GTX 1650 (TU117), driver 595.99.02, 4 GiB; its NVENC has no B-frames. Docker here has only the `runc` runtime, so no container sees the GPU: every NVENC test runs **on the host**, against the bundle that `make ostranscoder` builds in the container. The host has ffmpeg 8 and ffprobe, and no VLC.
- **CI has no GPU.** NVENC code builds and its device-free logic is unit-tested in CI; a test that needs a device skips with the probe's reason (`no NVENC device: <error>`), or fails when `OS_REQUIRE_NVENC=1`, which only `make test-nvenc` sets.
- **Peers:** every test that runs ffmpeg/ffprobe, VLC or GStreamer passes on the host (ffmpeg 8, GStreamer 1.28) and in `make test-ci-peers` (image `osv2-ci-peers:m4a`: ffmpeg 6.1, VLC 3.0.20, GStreamer 1.24) before any push.
- **Time:** 90 kHz everywhere. `transcode.Delay` (45000) is unchanged. A watermark never changes a frame's PTS and never holds a frame back.
- **Lint:** `exhaustive` with `default-signifies-exhaustive: false`; `godot`; gofumpt.
- **Errors:** wrap as `fmt.Errorf("pkg: op: %w", err)`; never both log and return.
- **Goroutines:** every spawned goroutine has an explicit cancel path.
- **Soak safety (absolute):** never stop, restart or reconfigure open-streamer-soak-mediamtx, caddy, grafana, osdev-console-1, vector, alloy, pyroscope, prometheus, node-exporter, nvidia-gpu-exporter, or any other container not named `osv2-*`. Never bind ports 554, 1935 or 8554. Own containers are `osv2-*`, `--rm`, stopped afterwards. `make test-nvenc` refuses to run when NVENC sessions or GPU compute processes of another process exist (R-C12).
- **Commits:** end every message with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. After each wave merge, run `go mod tidy`.

## Rulings

| ID | Ruling | Cost if wrong |
|---|---|---|
| R-C1 | **The encoder is chosen per rung:** `transcode.Video.Encoder` is `""`/`"x264"` (libx264, as today) or `"nvenc"` (`h264_nvenc`). Mixed ladders are allowed; each rung is its own encoder context and the presentation's barrier already orders rungs with different decode delays. Rejected: per ladder (a 4 GiB card with a session limit must be able to keep the cheap rungs on the CPU) | One more field through `Video`, wire and native; a mixed ladder's rungs differ in B-frames and rate-control character |
| R-C2 | **CPU frames.** The child decodes in software, applies the watermark and scales with swscale as today, and gives `yuv420p` system-memory frames to `h264_nvenc`, which uploads them itself. No `hwupload`, no cuvid decode, no `scale_cuda` | The decode, the scalers and one upload per rung stay on the CPU and the bus, so NVENC saves only the encode. A full-GPU path (cuvid → `scale_cuda` → nvenc, with `overlay_cuda`) is a Backlog item; it needs a watermark that renders on the GPU |
| R-C3 | **NVENC options:** `preset` = `Video.Preset`, which for an nvenc rung is `""` (→ `p4`) or `p1`…`p7` (`New` rejects x264 preset names: nothing is mapped silently); `tune=hq`; `rc=vbr` with bitrate, maxrate (0 → bitrate) and bufsize = 2 × maxrate, as x264; `profile=high`; GOP 600 (as x264's `keyint=600`); `forced-idr=1` with `pict_type` I and the key flag on the frame at each source keyframe PTS, exactly as the x264 rung; `no-scenecut=1`, `rc-lookahead=0`, `strict_gop=1`, `delay=0`, `aud=0`, `b_ref_mode=disabled`. **No B-frames on NVENC in M4c:** an nvenc rung's `BFrames` must be 0 or −1 and both mean none; `New` rejects 1…3 | Flipping a rung to nvenc with an x264 preset is a config error. NVENC rungs on cards that have B-frames give up their gain (about 5–10 % bitrate) until a later ruling; this host cannot test them, and R-T7b' (R-C11) wants reference B-frames kept off |
| R-C4 | **A failed NVENC open falls back to libx264 for that rung, for that build:** same height and rates, preset `veryfast`, no B-frames (what the NVENC rung would have had). It is counted (`Stats.Child.EncoderFallbacks`) and logged by the child once per build; the next build (source config change, discontinuity, respawn) tries NVENC again. This is also the session-limit handling: the driver's concurrent-session limit, another process holding sessions, a resolution below NVENC's minimum and a missing driver all end here. Rejected: failing the rung (the presentation would wait for it at every barrier) and failing the stream (the child would crash-loop every 5 s while the limit lasts) | A host sized for NVENC silently pays CPU for the overflow, and may not have it. `EncoderFallbacks` and the log are the only signal until M5 exports metrics; `make test-nvenc` requires 0 |
| R-C5 | **The watermark is drawn once, on the decoded picture before the ladder,** so every *encoded* rung carries it, scaled with the picture. **A copy rung cannot carry it, so `New` (and `SetWatermark`) reject a watermark on a ladder with a copy-video rendition.** Rejected: silently exempting the copy rung (the full-quality rendition would go out unbranded, which is what a watermark is there to prevent) and silently re-encoding it (it has no bitrate; it would stop being a copy) | An operator who wants the source resolution watermarked pays for one more encoded rung at the source height; one who wants an unbranded copy next to branded rungs needs a second stream |
| R-C6 | **Watermark spec** (`picture.Watermark`): either `Text` or `Image`, never both. Text: at most 256 bytes of valid UTF-8, one line, with the directives `%Y %m %d %H %M %S` and `%%` (any other `%x` is rejected), formatted in `TimeZone` (an IANA name, `""` = UTC; both binaries embed `time/tzdata`) from the child's wall clock, re-rendered when the formatted string changes. Image: PNG (alpha kept) or JPEG file bytes, at most 1 MiB and 2048 px per side. `Position`: top-left (zero value), top-right, bottom-left, bottom-right, centre, with a margin of 2 % of the picture height. `OpacityPct` 1…100 (0 → 100). `SizePct`: text glyph height in % of the picture height (0 → 5, 1…50); image width in % of the picture width (0 → 15, 1…100), height by its aspect ratio. Text is white with a black border of max(1, size/16) px | No colour, font, rotation or tiling choice. The clock is the child's wall clock at encode time, up to the pipeline's latency away from the picture's capture time, and meaningless for unpaced input |
| R-C7 | **Text is rendered by libavfilter `drawtext` with the embedded DejaVu Sans** (written to a private temp directory at start), `expansion=none`, and the text read from a `textfile` the child rewrites atomically (`reload=1` when the text has a time directive). Operator text never enters a filter-graph string: the graph string is built only from numbers and paths the child makes, and a temp path with a character outside `[A-Za-z0-9_./-]` is refused. Rejected: rendering text in Go (needs a font rasteriser, a new dependency) and fontconfig lookup (host-dependent; the peers image has no guaranteed font) | Glyphs outside DejaVu Sans (CJK, Arabic shaping) render as empty boxes; an operator font from the asset library is a Backlog item. One small file read per frame while the text has a directive |
| R-C8 | **The asset library is a directory** (osdev `-assets DIR`); an asset is named by a file name matching `^[a-z0-9][a-z0-9_-]{0,63}\.(png|jpg|jpeg)$`, read once at stream start by `picture.LoadAsset`. `transcode.Options` and the wire carry the image *bytes*, so the transcoder never sees a path. Upload, listing and replacement over the API are M5's | M5 puts an API in front of the same directory; assets are not reloaded when the file changes |
| R-C9 | **Live update:** `Transcoder.SetWatermark` sends one wire message; the child swaps its filter graph at the next decoded picture. No drain, no IDR, no epoch: the encoders never see a size change. The host keeps the latest watermark and sends it in the next child's `Configure`, so a respawned child starts with it. A watermark the child cannot build is a fatal child error: a stream configured with a watermark never goes out without it. Rejected: a full reload (an epoch, a discontinuity in every output, for a text edit) | A wire message and one piece of state to keep in step across respawns; a broken watermark stops the stream's encoded rungs instead of degrading |
| R-C10 | **Thumbnails:** `picture.Thumbnail{IntervalSec, Height, Quality}` (0 → 10 s, 180 px, 80; interval 1…3600, height even 2…720 and never above the source's, quality 1…100). Taken from the decoded picture **before the watermark** (an operator preview of the source, the same on transcoded and passthrough streams), scaled by swscale to full-range `yuvj420p` at the display aspect ratio, encoded with Go's `image/jpeg` (no second libav encoder and no qscale mapping for one small picture every few seconds). The schedule runs in stream time: a shot is due at the first picture, at a new epoch, and when PTS ≥ last shot + interval. **Transcoded streams** (a ladder with an encoded rung): the transcoding child takes them from every decoded picture. **Passthrough streams** (no ladder, a copy-only ladder, or copy video with encoded audio): `transcode.Thumbnailer` runs one child in thumbnail-only mode and forwards only the main video track's keyframes; the child decodes a keyframe alone when a shot is due, so the interval there is N rounded up to the next keyframe. The latest shot is kept **in memory** (no files); osdev serves it at `GET /live/{stream}/thumbnail.jpg` for the source stream name. Rejected: decoding in the server (it is pure Go) and libav's `mjpeg` encoder | A passthrough stream with thumbnails costs one idle child process. No history, no sprite sheets, nothing survives a restart; a public poster image would show the unbranded source |
| R-C11 | **R-T7b' (VLC 3 "buffer deadlock prevented") is settled by Task 9 before M4c is done.** Until Task 9 proves a defect in our output and fixes it, `b-pyramid=none` stays on x264 rungs, and the NVENC rule is R-C3's: no B-frames and `b_ref_mode=disabled`. Every encoded rung's SPS must announce a reorder depth of at most 1 (x264 with B-frames) or 0 (no B-frames, NVENC), pinned by a test | If Task 9 cannot decide inside its protocol, M4c cannot be marked done; the evidence goes to the user (see Task 9, Step 5) |
| R-C12 | **GPU proof:** `make test-nvenc` (script `scripts/test-nvenc.sh`), run on this host only. It refuses to start unless `nvidia-smi` reports 0 encoder sessions and no compute process (`OS_NVENC_SHARED=1` turns the refusal into a warning and the session assertions into "rose by 3"), runs the NVENC scenarios with `OS_REQUIRE_NVENC=1`, and writes `dist/nvenc-report/report.md`, which the controller posts on PR #53 and Task 12 copies into the spec. The NVENC fixtures run one after another, never in parallel, so at most 3 sessions are open | The proof is manual and local; a later change can break NVENC without CI noticing until the target is run again |
| R-C13 | **Wire version 2.** `Rung.Encoder`, `Configure.Watermark`, `Configure.Thumbnail`, the messages `SetWatermark` (host to child) and `Shot` (child to host), and `Stats.EncoderFallbacks` and `Stats.Thumbnails` are added; the host still rejects a child with another version | Server and bundle must be deployed together, as before |

## Execution waves (parallel per user instruction)

| Wave | Tasks | Why this order |
|---|---|---|
| 1 | 1 (carry items), 2 (`picture`), 9 (R-T7b' root cause) | Disjoint: `merge.go` and `rtspplay`; a new package; experiments plus one native test file |
| 2 | 3 (wire v2) | Needs Task 2's types |
| 3 | 4 (native NVENC), 7 (host: options, `SetWatermark`, `Thumbnailer`) | 4 touches native and `cmd/ostranscoder`; 7 touches `internal/transcode` with the fake child |
| 4 | 5 (native watermark), 8 (osdev) | 5 needs 4's `native.Config` (same files); 8 needs 7 |
| 5 | 6 (native thumbnails) | Same files as 5 |
| 6 | 10 (M4c scenarios, x264), 11 (GPU target and NVENC scenarios) | Both need 4–8; 10 is `internal/e2e/m4c_test.go`, 11 is `internal/e2e/m4c_nvenc_test.go` and scripts |
| 7 | 12 (CI, spec, done) | Needs 9, 10, 11 |

Each parallel task runs in its own worktree; the controller merges.

## Review Focus

1. **The source's resolution changes while a watermark is on** (`h264_aac_then_640.ts`): the graph is rebuilt for the new size, the mark keeps its relative place and size, and no picture goes out without it. Task 5, `TestWatermarkAcrossConfigChange`.
2. **NVENC sessions run out** (the driver's limit, another process, a rung below NVENC's minimum size): the rung is encoded by libx264 instead, the stream stays up and aligned, and the fallback is counted. Task 4, `TestNVENCFallback`; Task 11 asserts the count is 0 on the proof run.
3. **Thumbnails of an open-GOP H.264 source and of H.265 on a passthrough stream** (keyframes that are recovery points or CRAs, decoded alone): a shot still appears at every interval. Task 6, `TestThumbnailKeyOnlyOpenGOP` and `TestThumbnailKeyOnlyHEVC`; Task 10's passthrough rows.
4. **Watermark text that looks like filter syntax or is outside the font** (`:`, `'`, `\`, `%{localtime}`, `,`, `[in]`, CJK, 256 bytes): drawn literally or as boxes, never interpreted, never a crash. Task 2, `TestWatermarkValidate`; Task 5, `TestWatermarkTextIsLiteral`.
5. **A child that dies while a watermark is on, or a `SetWatermark` that races a respawn:** the new child's first picture already carries the latest watermark. Task 7, `TestWatermarkSurvivesRespawn`; Task 10, `TestM4cWatermarkRespawn`.

---

### Task 1: Carry items from M4b's final review

**Files:**
- Modify: `internal/transcode/merge.go` (`holds`, `noteLag`, the merger doc), `internal/output/rtspplay/rtspplay_test.go` (`TestPlayCodecChangeEndsSession`), `docs/REBUILD_PLAN.md` (§3.4)
- Test: `internal/transcode/merge_test.go`

**Interfaces:** none produced. Only the presentation merger changes; rendition mergers keep the fixed `MergeHold`.

**Lag envelopes.** `m.lag` becomes `map[int]lagEntry` with `type lagEntry struct { env int64; epoch uint32; lead int64 }`: the envelope, and the epoch and that epoch's lead (`m.lead[epoch]`) when the track last pushed. `m.first map[uint32]int64` records each epoch's first pushed DTS. Let E be the largest epoch any track has pushed. An entry is **stale**, and is deleted by `holds` before it reads the map, when
- its epoch is E and `m.lead[E] − entry.lead > maxHoldTicks` (the track has been silent for more than the cap of stream time), or
- its epoch is below E and `m.lead[E] − m.first[E] > maxHoldTicks` (the new epoch has run that long without the track).

So an entry of the previous epoch survives into the new epoch's start. `m.first` is pruned where `m.lead` is.

**New-epoch opener.** In `holds`, `case newEpoch`: when `m.lag` has an entry after the prune, return the learned hold (1.5 × the largest envelope, clamped to [`hold`, `maxHold`]), wall time only as today; when it has none, `maxHold` as today.

- [ ] **Step 1: Write the failing tests** (fake clock, as `TestMergerBarrierHoldRunsOut`)

```go
func TestMergerNewEpochUsesLearnedHold(t *testing.T)
// presentation merger, MergeHold 2 s. Epoch 1: track 1 trails track 0 by 225000 ticks (2.5 s) for 20 frames.
// Epoch 2 is expected on tracks 0 and 1; only track 0's IDR arrives. It is not written at 3.7 s of wall
// time and is written at 3.8 s (1.5 × 2.5 s = 3.75 s), not at 8 s.
func TestMergerNewEpochBeforeAnyLag(t *testing.T)
// a fresh merger: the first epoch's opener with track 1 missing is held until 8 s, as today.
func TestMergerStaleLagPruned(t *testing.T)
// track 1 pushes one frame 450000 ticks (5 s) behind, then nothing; tracks 0 and 2 push 9 s of stream time
// (more than the 8 s cap). Then a frame waiting on a missing track 2 is released after 2 s of wall time,
// not 7.5 s; before the 8 s have passed it still waits 7.5 s.
func TestMergerLagSurvivesEpochChange(t *testing.T)
// lag learned in epoch 1; epoch 2 opens and runs 1 s: holds(false) still returns the learned hold; after
// epoch 2 has run more than 8 s without track 1 pushing, track 1's entry is gone.
```

- [ ] **Step 2: Run** `go test -race -count=1 -run 'TestMergerNewEpoch|TestMergerStaleLag|TestMergerLagSurvives' ./internal/transcode/` — fail.
- [ ] **Step 3: Implement** the entry type, the prune and the opener rule; update the merger's doc comment (the "new epoch's first frame … waits presHoldFactor × hold" sentence, and the cost sentence about a silent rung).
- [ ] **Step 4: `TestPlayCodecChangeEndsSession`.** Keep every assertion but the count: no H.265 access unit after the change, two BYEs, the BYEs last, `EndedCodecChange == 1`, unsubscribed, 0 players. Replace `assert.Equal(t, 150, sum.videoAUs)` with: while the session lives, a goroutine polls `e.h.Stats()` every millisecond and keeps the last non-empty entry's `Dropped`; then `dropped == 0` requires `sum.videoAUs == 150`, and `dropped > 0` requires `25 <= sum.videoAUs && sum.videoAUs < 150` and logs both numbers. The goroutine ends when the stats empty or the test's context is cancelled.
- [ ] **Step 5: Spec.** In `docs/REBUILD_PLAN.md` §3.4, change "Never a part that does not decode from its start, and never a part running past its segment." to end "…and never a part running past its segment, except a leading rung with no keyframe at K, whose frames are not moved (Backlog)."; in §3.5's presentation bullet, state the learned hold for a new epoch's opener and the stale-lag prune.
- [ ] **Step 6: Run** `go test -race -count=1 ./internal/transcode/ ./internal/output/rtspplay/` — pass; then `go test -race -count=20 -run TestPlayCodecChangeEndsSession ./internal/output/rtspplay/` — pass.
- [ ] **Step 7: Commit** `fix(transcode): learned hold for a new epoch's opener; prune stale lag` and `test(rtspplay): tolerate hub drops in the codec-change test`

### Task 2: `internal/transcode/picture` — the shared specs

**Files:**
- Create: `internal/transcode/picture/doc.go`, `watermark.go`, `thumbnail.go`
- Test: `internal/transcode/picture/watermark_test.go`, `thumbnail_test.go`

Pure Go, no goroutines, imports only the standard library (including `_ "time/tzdata"`).

**Interfaces — Produces:**

```go
type Position uint8
const ( TopLeft Position = iota; TopRight; BottomLeft; BottomRight; Center )

type Watermark struct {
	Text       string   // exclusive with Image
	Image      []byte   // PNG or JPEG file bytes
	Position   Position
	OpacityPct int      // 1..100; 0 means 100
	SizePct    int      // text: 1..50, 0 means 5; image: 1..100, 0 means 15
	TimeZone   string   // IANA name; "" means UTC
}
const ( MaxTextBytes = 256; MaxImageBytes = 1 << 20; MaxImageSide = 2048 )

func (w Watermark) Validate() error                 // R-C6; an image is checked with image.DecodeConfig only
func (w Watermark) Resolved() Watermark             // defaults filled in; call after Validate
func (w Watermark) Location() (*time.Location, error)
func HasTime(text string) bool                      // any directive other than %%
func FormatText(text string, t time.Time) string    // t is already in the zone; %% → %
func DecodeImage(b []byte) (image.Image, error)     // PNG or JPEG within the limits
func PositionExpr(p Position, W, H, w, h string, margin int) (x, y string)
// e.g. PositionExpr(BottomRight, "W", "H", "w", "h", 9) = "W-w-9", "H-h-9"; Center = "(W-w)/2", "(H-h)/2"
func Margin(picH int) int                           // round(0.02 × picH), at least 1
func LoadAsset(dir, name string) ([]byte, error)    // R-C8's name rule, then MaxImageBytes

type Thumbnail struct{ IntervalSec, Height, Quality int }
func (t Thumbnail) Validate() error                 // R-C10's ranges, zero values allowed
func (t Thumbnail) Resolved() Thumbnail             // 10, 180, 80
const MaxShotBytes = 4 << 20

type Shot struct {
	PTS           int64
	Epoch         uint32
	Width, Height int
	JPEG          []byte
}

type Schedule struct{ /* interval in ticks, last epoch and PTS */ }
func NewSchedule(intervalSec int) *Schedule
func (s *Schedule) Due(epoch uint32, pts int64) bool // no shot yet; another epoch; pts ≥ last+interval; pts < last
func (s *Schedule) Took(epoch uint32, pts int64)
func ShotSize(srcW, srcH, sarNum, sarDen, height int) (w, h int)
// h = min(height, srcH) rounded down to even, at least 2; w keeps the display aspect ratio
// (srcW × sarNum : srcH × sarDen; a SAR with a zero is 1:1), rounded to the nearest even, at least 2.
func EncodeJPEG(img image.Image, quality int) ([]byte, error)
```

- [ ] **Step 1: Write the failing tests**

```go
func TestWatermarkValidate(t *testing.T)
// table: neither text nor image → error; both → error; text of 257 bytes → error; "a\nb", "a\x00" and
// invalid UTF-8 → error; "%Q" and a trailing "%" → error; "%Y-%m-%d %H:%M:%S %%" → ok;
// ":'\\%%{localtime},[in]" → ok (it is literal text); OpacityPct 101 and −1 → error; SizePct 51 for text,
// 101 for an image → error; Position 5 → error; TimeZone "Mars/Phobos" → error, "Asia/Ho_Chi_Minh" → ok;
// an image that is a GIF, a 2049×1 PNG, or 1 MiB + 1 byte → error; a 64×64 PNG and JPEG → ok.
func TestWatermarkResolved(t *testing.T)   // text: 100, 5; image: 100, 15
func TestFormatText(t *testing.T)
// 2026-03-09 07:05:03 in Asia/Ho_Chi_Minh: "%Y-%m-%d %H:%M:%S" → "2026-03-09 07:05:03"; "100%% %H" → "100% 07";
// "plain" → "plain".
func TestHasTime(t *testing.T)             // "%%" false; "at %H" true
func TestPositionExpr(t *testing.T)        // all five positions, margin 9, exact strings
func TestLoadAsset(t *testing.T)
// "logo.png" ok; "../logo.png", "Logo.png", "logo.gif", "", "a/b.png" → error without touching the disk;
// a file over 1 MiB → error.
func TestThumbnailValidate(t *testing.T)   // IntervalSec 3601, Height 181 and 722, Quality 101 → error; zeros ok
func TestSchedule(t *testing.T)
// interval 2: Due(1, 0) true; Took(1, 0); Due(1, 179999) false; Due(1, 180000) true; Took(1, 180000);
// Due(2, 0) true (new epoch); after Took(2, 900000): Due(2, 450000) true (PTS went back).
func TestShotSize(t *testing.T)
// (320,240,1,1,180) → 240×180; (320,240,1,1,720) → 320×240; (720,576,16,15,180) → 240×180;
// (1920,1080,0,0,180) → 320×180 (a SAR with a zero is 1:1); (320,241,1,1,720) → 320×240.
func TestEncodeJPEG(t *testing.T)          // a 16×16 image round-trips through image/jpeg at the same size
```

- [ ] **Step 2: Run** `go test -race -count=1 ./internal/transcode/picture/` — compile failure.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the package's tests, `golangci-lint run ./internal/transcode/picture/`, `GOOS=windows go vet ./...` — pass.
- [ ] **Step 5: Commit** `feat(picture): watermark and thumbnail specs shared by host and child`

### Task 3: Wire version 2 (R-C13)

**Files:**
- Modify: `internal/transcode/wire/wire.go`, `internal/transcode/wire/doc.go`, `internal/transcode/transcodetest/fake.go`
- Test: `internal/transcode/wire/wire_test.go`, `internal/transcode/wire/fuzz_test.go`

**Interfaces:**
- Consumes: `picture.Watermark`, `picture.Thumbnail`, `picture.Shot`, `picture.MaxShotBytes`, `picture.MaxImageBytes`, `picture.MaxTextBytes` (Task 2).
- Produces:

```go
const Version = 2

type Rung struct {
	Height, BitrateKbps, MaxBitrateKbps, BFrames int
	Preset, Encoder                              string // Encoder: "", "x264" or "nvenc"
}
type Configure struct {
	Rungs     []Rung
	Audio     *AudioEncode
	Watermark *picture.Watermark // nil: none
	Thumbnail *picture.Thumbnail // nil: none
}
type SetWatermark struct{ W *picture.Watermark } // host to child; nil removes it
type Shot struct{ S picture.Shot }               // child to host
// Stats gains: EncoderFallbacks, Thumbnails uint64
```

Field numbers: Rung 6 `encoder` (bytes). Configure 3 `watermark`, 4 `thumbnail`. Watermark: 1 text, 2 image, 3 position, 4 opacity, 5 size, 6 time zone. Thumbnail: 1 interval, 2 height, 3 quality. Envelope 8 `SetWatermark` (1 watermark, absent = remove), 9 `Shot` (1 pts sint64, 2 epoch, 3 width, 4 height, 5 jpeg). `Stats`: the next two free field numbers. The Writer refuses (`ErrLimit`) a text above `MaxTextBytes`, an image above `MaxImageBytes`, a JPEG above `MaxShotBytes` and an encoder name above 16 bytes; the Reader rejects the same as `ErrMalformed`. The wire does not validate a watermark further: the host and the child call `Validate`.

The fake child (`transcodetest`) announces version 2 and learns three behaviours, each behind an environment variable in the style of `EnvVersion`: `EnvShots` (`OS_TRANSCODE_FAKE_SHOTS=n`: send a `Shot` with a 1×1 JPEG for every n-th video keyframe it receives), `EnvWatermarkLog` (a file path: append one line per `Configure` and per `SetWatermark`, `configure <text|image:<len>|none>` and `set <…>`), and a thumbnail-only `Configure` (no rungs, no audio) is accepted.

- [ ] **Step 1: Write the failing tests**

```go
func TestConfigureV2RoundTrip(t *testing.T)
// a Configure with rungs {Encoder "nvenc", Preset "p4"} and {Encoder ""}, a text watermark and a thumbnail
// spec round-trips; one with an image watermark of MaxImageBytes round-trips; nil Watermark and Thumbnail
// stay nil.
func TestSetWatermarkRoundTrip(t *testing.T)  // a text watermark; nil
func TestShotRoundTrip(t *testing.T)          // PTS −1, epoch 7, 240×180, 3 bytes
func TestStatsV2RoundTrip(t *testing.T)       // EncoderFallbacks 2, Thumbnails 9
func TestV2Limits(t *testing.T)
// Writer: text of 257 bytes, image of MaxImageBytes+1, JPEG of MaxShotBytes+1 → ErrLimit, nothing written.
// Reader: the same bytes hand-encoded → ErrMalformed.
func TestVersionIsTwo(t *testing.T)           // Hello{Version: Version} carries 2
```

Extend `FuzzRead`'s seed corpus with one `SetWatermark` and one `Shot`.

- [ ] **Step 2: Run** `go test -race -count=1 ./internal/transcode/wire/` — fail.
- [ ] **Step 3: Implement**; update the one existing assertion of version 1.
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/transcode/...` — pass (the host ignores the new messages until Task 7).
- [ ] **Step 5: Commit** `feat(wire): version 2 with encoder, watermark and thumbnail messages`

### Task 4: Native NVENC rungs (R-C1 to R-C4)

**Files:**
- Create: `internal/transcode/native/encoder.go` (the encoder choice, NVENC options, the fallback, the probe)
- Modify: `internal/transcode/native/video.go` (`openRung` moves to `encoder.go` and dispatches), `internal/transcode/native/pipeline.go` (`Rung.Encoder`, `Config`, `Stats.EncoderFallbacks`, `resolveRung`), `cmd/ostranscoder/main.go` (`-probe nvenc`, `Encoder` in `configure`, the new counter in `stats`), `cmd/ostranscoder/doc.go`
- Test: `internal/transcode/native/encoder_test.go`, `cmd/ostranscoder/main_test.go`

**Interfaces — Produces:**

```go
// native
const ( EncoderX264 = "x264"; EncoderNVENC = "nvenc" )
type Rung struct { Height, BitrateKbps, MaxBitrateKbps, BFrames int; Preset, Encoder string }
type Config struct { Rungs []Rung; Audio *Audio }          // Tasks 5 and 6 add fields
func NewWith(c Config) (*Pipeline, error)                  // New(rungs, audio) becomes NewWith(Config{…})
func ProbeNVENC() error                                    // opens and closes h264_nvenc at 256×144, 25 fps, p4
// Stats gains EncoderFallbacks uint64: rung encoder opens that fell back to libx264.
func nvencOptions(r Rung) [][2]string                      // unexported, the R-C3 list in a fixed order
var nvencGPU = -1                                          // the `gpu` option; tests set 63 to force a failed open
```

`ostranscoder -probe nvenc` prints `nvenc: ok` and exits 0, or prints `nvenc: <error>` and exits 3. The untagged stub is unchanged (exit 2).

`resolveRung`: `Encoder` `""` → `x264`; any other name than the two → error. For nvenc: `Preset` `""` → `p4`, `p1`…`p7` ok, anything else → error naming the allowed values; `BFrames` 0 and −1 → 0, 1…3 → error "nvenc rungs have no B-frames (R-C3)". For x264 nothing changes.

`openNVENC`: `h264_nvenc`, `yuv420p`, time base 1/90000, the framerate, SAR, bitrate, maxrate, buffer size 2 × maxrate, `SetGopSize(600)`, `SetMaxBFrames(0)`, then `nvencOptions` and `gpu` = `nvencGPU` when it is not −1. The frame path (`rungEncoder.encode`, `frame`) is shared with x264 unchanged: NVENC repeats SPS and PPS in band on every IDR.

Fallback (R-C4): in `openRung`, when `openNVENC` fails, log one `slog.Warn` ("nvenc open failed, using libx264", rung, error), add 1 to `Stats.EncoderFallbacks`, and open libx264 with the rung's height and rates, preset `veryfast` and no B-frames. A failure of that x264 open is the error returned.

- [ ] **Step 1: Write the failing tests** (`-tags libav`; `nvencOrSkip(t)` calls `ProbeNVENC` and skips with `no NVENC device: <error>`, or fails when `OS_REQUIRE_NVENC=1`)

```go
func TestResolveRungEncoder(t *testing.T)
// table: "" → x264; "nvenc" + "" → p4; "nvenc" + "p7" ok; "nvenc" + "veryfast" → error; "nvenc" + BFrames 2 →
// error; "nvenc" + BFrames −1 → 0; "qsv" → error; x264 rows resolve as before this task.
func TestNVENCOptions(t *testing.T)
// nvencOptions(Rung{Preset: "p4"}) == {preset p4, tune hq, rc vbr, profile high, forced-idr 1, no-scenecut 1,
// rc-lookahead 0, strict_gop 1, delay 0, aud 0, b_ref_mode disabled}, in that order.
func TestNVENCFallback(t *testing.T)
// nvencGPU = 63 (restored by t.Cleanup). h264_aac.ts through rungs {240 nvenc, 180 x264}: both rungs produce
// video; Stats.EncoderFallbacks == 1; rung 0's first keyframe carries x264's SEI (a NAL of type 6 containing
// "x264"); both rungs have a keyframe at exactly every source keyframe PTS; rung 0's frames have DTS == PTS.
// Runs with and without a device.
func TestNVENCRung(t *testing.T)            // nvencOrSkip
// h264_aac.ts through rungs {240 nvenc, 120 nvenc}: EncoderFallbacks == 0; no frame has a NAL of type 6
// containing "x264"; a keyframe at exactly every source keyframe PTS and no other; DTS == PTS on every frame;
// PTS equal the input's; the SPS is High profile and h26x.ReorderOf(h264, sps) == 0.
func TestNVENCMixedLadder(t *testing.T)     // nvencOrSkip
// {240 nvenc, 180 x264 BFrames 2, 120 nvenc} on h264_pyramid.ts: every rung has the same keyframe PTS set.
func TestNVENCAcrossConfigChange(t *testing.T) // nvencOrSkip
// h264_aac_then_640.ts: the rung's Config pointer changes once, at the second build; EncoderFallbacks == 0.
func TestProbeNVENC(t *testing.T)
// ProbeNVENC() returns nil or an error and never panics; with nvencGPU = 63 it returns an error.
// cmd/ostranscoder:
func TestProbeFlag(t *testing.T)
// `ostranscoder -probe nvenc` exits 0 printing "nvenc: ok" or 3 printing "nvenc: …"; `-probe x` exits 2.
```

- [ ] **Step 2: Run** `make test-transcode` — the new tests fail to compile.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make test-transcode` (container: the device tests skip, naming the reason) — pass. Then on the host, where the GPU is (check first that `nvidia-smi --query-gpu=encoder.stats.sessionCount --format=csv,noheader` prints 0): `p=$(sh scripts/ffmpeg-dev.sh amd64); PKG_CONFIG_PATH=$p/lib/pkgconfig LD_LIBRARY_PATH=$p/lib OS_REQUIRE_NVENC=1 go test -race -count=1 -tags libav -run 'NVENC' ./internal/transcode/native/` — pass, nothing skipped. Then `go build ./... && go vet ./... && GOOS=darwin go vet ./...` — pass.
- [ ] **Step 5: Commit** `feat(native): NVENC rungs with a libx264 fallback`

### Task 5: Native watermark (R-C5 to R-C7, R-C9)

**Files:**
- Create: `internal/transcode/native/watermark.go`, `internal/transcode/native/font.go` (`//go:embed font/DejaVuSans.ttf`), `internal/transcode/native/font/DejaVuSans.ttf`, `internal/transcode/native/font/LICENSE`
- Modify: `internal/transcode/native/pipeline.go` (`Config.Watermark`, `Config.Now`, `SetWatermark`; the cgo `pkg-config` line gains `libavfilter`), `internal/transcode/native/video.go` (`encode`: filter the picture before the rungs), `cmd/ostranscoder/main.go` (`Configure.Watermark`, the `SetWatermark` message), `scripts/bundle-ostranscoder.sh` (copy the font licence to `<outdir>/LICENSE-dejavu`; fail when `lib/libavfilter.so.*` is missing)
- Test: `internal/transcode/native/watermark_test.go`, `cmd/ostranscoder/main_test.go`

**Interfaces:**
- Consumes: `picture.Watermark` and its helpers (Task 2); `native.Config`, `NewWith` (Task 4); `wire.SetWatermark` (Task 3).
- Produces: `Config.Watermark *picture.Watermark`; `Config.Now func() time.Time` (nil → `time.Now`); `func (p *Pipeline) SetWatermark(w *picture.Watermark) error` (validates; takes effect at the next decoded picture; nil removes it; an error leaves the pipeline as it was). `NewWith` rejects a watermark with no rungs.

**The stage.** `type watermarker` owns one filter graph for one (watermark, picture width, height, pixel format, SAR) and is rebuilt when any of them changes; a rebuild failure is a pipeline error (fatal, R-C9). `videoPath.encode` passes each decoded picture through it before the rung loop; with no watermark the picture goes to the rungs untouched, by the same code path as today. The stage must return exactly one frame per frame put in, with the same PTS, before the next is put in; if the sink has no frame after an add, that is an error ("watermark: graph held a frame").

- *Text:* the graph is `buffer → drawtext → format=yuv420p → buffersink`, parsed from a string that holds only `fontfile=<dir>/font.ttf`, `textfile=<dir>/text`, `expansion=none`, `reload=1` (only when `picture.HasTime`), `fontsize=<px>` (SizePct % of the picture height, at least 8), `fontcolor=white@<a>`, `borderw=<max(1, px/16)>`, `bordercolor=black@<a>`, and `x=`/`y=` from `picture.PositionExpr(pos, "w", "h", "tw", "th", picture.Margin(h))`, where `<a>` is OpacityPct/100 with two decimals. `<dir>` is one `os.MkdirTemp("", "ostx-wm-")` per pipeline, removed by `Close`; a `<dir>` with a character outside `[A-Za-z0-9_./-]` is an error. Before each picture the stage formats the text with `picture.FormatText(text, now().In(loc))` and, when the string differs from the last written, writes it to `<dir>/text.tmp` and renames it over `<dir>/text`.
- *Image:* the graph is `[in]` and `[wm]` buffers → `[wm]scale=<w>:-1,format=yuva420p,colorchannelmixer=aa=<a>[o];[in][o]overlay=<x>:<y>:format=auto,format=yuv420p` → sink, with `<w>` = SizePct % of the picture width (at least 2) and x, y from `PositionExpr(pos, "W", "H", "w", "h", margin)`. The image is decoded with `picture.DecodeImage`, converted to an RGBA frame (`FrameData.FromImage`), added once to `[wm]` with PTS 0 and followed by end of stream, so `overlay` repeats it.

- [ ] **Step 1: Write the failing tests** (`-tags libav`; a helper `decodeRung(t, outs, rung) []image.Image` decodes a rung's output frames with libavcodec; `region(img, pos, wPct, hPct)` returns the mean RGB of the rectangle a mark of that size would occupy at `pos`; `magenta64` is a 64×64 opaque PNG of RGB 255,0,255)

```go
func TestWatermarkImageOnEveryRung(t *testing.T)
// h264_aac.ts, rungs 240/180/120 x264, image magenta64, SizePct 25, TopLeft: on every rung's first and last
// decoded picture, the mean RGB of the inner half of the mark's rectangle is within 40 of (255,0,255) per
// channel, and the mirrored rectangle at BottomRight is more than 80 away on some channel.
func TestWatermarkPositions(t *testing.T)      // one rung at 240, all five positions: the mark is where PositionExpr says
func TestWatermarkOpacity(t *testing.T)        // OpacityPct 50: the region lies strictly between the unmarked picture's and magenta
func TestWatermarkKeepsTiming(t *testing.T)
// the same input with and without a text watermark: per rung, the same number of frames, the same PTS, DTS
// and Key on every frame, and a keyframe at exactly every source keyframe PTS.
func TestWatermarkTextTicks(t *testing.T)
// Text "%H:%M:%S", Now a fake clock advanced 1 s every 25 pictures: the top-left band of pictures 0 and 24
// are equal within noise (mean absolute difference below 2), pictures 24 and 25 differ (above 2) there, and
// the band differs from an unmarked run's on every picture.
func TestWatermarkTextIsLiteral(t *testing.T)
// Text ":'\\%%{localtime},[in]textfile=/etc/passwd" and a 256-byte text of CJK: the pipeline runs to the end,
// the band differs from an unmarked run, and nothing but <dir>/font.ttf and <dir>/text is opened by the graph
// (assert the graph string, exposed to tests, contains none of the text's bytes).
func TestWatermarkAcrossConfigChange(t *testing.T)
// h264_aac_then_640.ts, image mark, SizePct 25, TopRight: the mark is in place on the last picture before and
// the first after the change, at 25 % of each picture's width.
func TestSetWatermark(t *testing.T)
// no watermark for 50 pictures, SetWatermark(image) then 50 more, SetWatermark(nil) then 50: the mark is
// absent, present, absent; frame counts, PTS and keyframes equal a run that never had one; no error.
func TestSetWatermarkInvalid(t *testing.T)     // an invalid watermark → error; the previous one stays
func TestWatermarkNeedsRungs(t *testing.T)     // NewWith with a watermark, audio only → error
func TestWatermarkTempDirRemoved(t *testing.T) // after Close, the ostx-wm-* directory is gone
// cmd/ostranscoder:
func TestServeSetWatermark(t *testing.T)
// Configure with a watermark, frames, then SetWatermark{nil}, frames, Stop: the child exits 0 and writes as
// many frames as without it.
```

- [ ] **Step 2: Run** `make test-transcode` — fail.
- [ ] **Step 3: Implement**; copy the font and its licence as the Global Constraints say and check the sha256.
- [ ] **Step 4: Run** `make test-transcode` — pass; `make ostranscoder` — the bundle has `lib/libavfilter.so.11` and `LICENSE-dejavu`, and `dist/ostranscoder-amd64/ostranscoder -version` runs with no `LD_LIBRARY_PATH`.
- [ ] **Step 5: Commit** `feat(native): watermark the decoded picture before the ladder`

### Task 6: Native thumbnails (R-C10)

**Files:**
- Create: `internal/transcode/native/thumbnail.go`
- Modify: `internal/transcode/native/pipeline.go` (`Config.Thumbnail`, `Output.Shot`, `Stats.Thumbnails`; `NewWith` accepts a thumbnail with no rungs and no audio), `internal/transcode/native/video.go` (`encode`: take a shot before the watermark), `cmd/ostranscoder/main.go` (`Configure.Thumbnail`; write `wire.Shot` for an output with a shot; the counter in `stats`)
- Test: `internal/transcode/native/thumbnail_test.go`, `cmd/ostranscoder/main_test.go`

**Interfaces:**
- Consumes: `picture.Thumbnail`, `picture.Shot`, `picture.Schedule`, `picture.ShotSize`, `picture.EncodeJPEG` (Task 2); `wire.Shot` (Task 3).
- Produces: `Config.Thumbnail *picture.Thumbnail`; `const RungShot = -2`; `Output.Shot *picture.Shot` (non-nil exactly when `Rung == RungShot`; `F` is then zero); `Stats.Thumbnails uint64`.

**Shot:** when `Schedule.Due(epoch, pts)`, scale the decoded picture with swscale (bicubic) to `picture.ShotSize(w, h, sar, Height)` in `yuvj420p`, read it as an `image.YCbCr` (`FrameData.ToImage`), `picture.EncodeJPEG` at Quality, emit the shot with the picture's PTS and the pipeline's epoch, and call `Took`. A JPEG above `picture.MaxShotBytes` is dropped with one warning.

**Two modes.**
- *With rungs:* `videoPath.encode` takes the shot from each decoded picture before the watermark stage.
- *Key-only (no rungs):* the pipeline drops audio and non-key video without counting them. For a key frame of the lowest-numbered video track with `Due(epoch, f.PTS)`: open a decoder for `f.Config` (thread count 1, `AV_CODEC_FLAG2_SHOW_ALL` and `AV_CODEC_FLAG_OUTPUT_CORRUPT` set, so a recovery-point or CRA picture decoded alone is output), send the frame, send end of stream, take the first picture as the shot, free the decoder. A decode failure counts in `VideoCorrupt` (the 50-in-a-row rule applies) and gives no shot.

- [ ] **Step 1: Write the failing tests**

```go
func TestThumbnailWithRungs(t *testing.T)
// h264_aac.ts (6 s, 25 fps) with one rung at 120 and Thumbnail{IntervalSec: 2, Height: 120, Quality: 80}:
// 3 shots, at the first picture's PTS and at the first pictures 180000 and 360000 after it; each JPEG decodes
// to 160×120; the first and last shots differ; Stats.Thumbnails == 3; the rung's frames equal a run without
// thumbnails.
func TestThumbnailBeforeWatermark(t *testing.T) // with an image watermark: the shot's mark rectangle is not magenta
func TestThumbnailNewEpoch(t *testing.T)        // h264_aac_then_640.ts with interval 3600: one shot per epoch
func TestThumbnailNeverUpscales(t *testing.T)   // Height 720 on a 320×240 source: 320×240
func TestThumbnailKeyOnly(t *testing.T)
// no rungs, no audio, interval 2, fed only h264_aac.ts's video keyframes (one a second): shots at keyframes
// 0, 2 and 4; Stats.Thumbnails == 3; no other output.
func TestThumbnailKeyOnlyIgnoresRest(t *testing.T) // fed every frame of h264_aac.ts: the same 3 shots
func TestThumbnailKeyOnlyOpenGOP(t *testing.T)     // h264_opengop.ts keyframes, interval 1: a shot per keyframe
func TestThumbnailKeyOnlyHEVC(t *testing.T)        // h265_aac51.ts keyframes, interval 1: a shot per keyframe
func TestThumbnailKeyOnlyCorruptKey(t *testing.T)  // a keyframe with zeroed slice data: no shot, VideoCorrupt 1, no error; the next keyframe gives a shot
// cmd/ostranscoder:
func TestServeShots(t *testing.T)
// Configure{Thumbnail} alone, the keyframes of h264_aac.ts, Stop: the child writes 3 wire.Shot and a final
// Stats with Thumbnails 3, and exits 0.
```

- [ ] **Step 2: Run** `make test-transcode` — fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make test-transcode` and `make ostranscoder` — pass.
- [ ] **Step 5: Commit** `feat(native): JPEG thumbnails from the decoder, also from keyframes alone`

### Task 7: Host — encoder, watermark, thumbnails and the `Thumbnailer`

**Files:**
- Create: `internal/transcode/thumbnailer.go`
- Modify: `internal/transcode/transcode.go` (`Video.Encoder`, `Options.Watermark`, `Options.Thumbnail`, `Options.OnShot`, `New`'s validation, `Configure`, `readChild`, `ChildStats`, `SetWatermark`, `Shot`, the package doc)
- Test: `internal/transcode/transcode_test.go`, `internal/transcode/thumbnailer_test.go` (both with the `transcodetest` fake child)

**Interfaces:**
- Consumes: wire v2 and the fake child's new behaviours (Task 3); `picture` (Task 2).
- Produces:

```go
const ( EncoderX264 = "x264"; EncoderNVENC = "nvenc" )
// Video gains Encoder string. Options gains:
//   Watermark *picture.Watermark   // nil: none
//   Thumbnail *picture.Thumbnail   // nil: none; needs an encoded video rendition
//   OnShot    func(picture.Shot)   // optional; called from the child's reader goroutine
// ChildStats gains EncoderFallbacks, Thumbnails uint64 (summed like the others).

func (t *Transcoder) SetWatermark(w *picture.Watermark) error
func (t *Transcoder) Shot() (s picture.Shot, at time.Time, ok bool) // the latest, and when it arrived (Options.Now)

type ThumbnailerOptions struct {
	Binary                 string
	Args, Env              []string
	Thumbnail              picture.Thumbnail
	RespawnMin, RespawnMax time.Duration // 0: 250 ms, 5 s
	HelloTimeout           time.Duration // 0: 10 s
	Now                    func() time.Time
	After                  func(time.Duration) <-chan time.Time
	Logger                 *slog.Logger
	OnShot                 func(picture.Shot)
}
type ThumbnailerStats struct{ Spawns, Exits, KeysSent, KeysSkipped, Shots uint64 }
func NewThumbnailer(src *hub.Hub, opt ThumbnailerOptions) (*Thumbnailer, error)
func (t *Thumbnailer) Run(ctx context.Context) error
func (t *Thumbnailer) Shot() (picture.Shot, time.Time, bool)
func (t *Thumbnailer) Stats() ThumbnailerStats
```

**`New`.** `Video.Encoder`: `""`, `x264` or `nvenc`, else an error; for nvenc, R-C3's preset and B-frame rules (the same messages as native's `resolveRung`). `Watermark`: `Validate`, then an error when any rendition copies video ("rendition %q copies video and cannot carry the watermark", R-C5). `Thumbnail`: `Validate`, then an error when no rendition encodes video ("thumbnails of a passthrough stream need a Thumbnailer"). A shot's PTS is in rendition time (source + `Delay`).

**`SetWatermark`** validates as `New` does (an error changes nothing), stores the watermark as the current one, and hands it to the Run goroutine, which sends `wire.SetWatermark` to a child that has said Hello, in order with the frames; several pending calls collapse to the latest. Every child's `Configure` carries the current watermark. Safe from any goroutine, also before `Run` and after it ends.

**`Thumbnailer`.** `Run` subscribes with `src.SubscribeLive("thumbnailer", 64)` and ends with nil when the context ends or the hub closes. The main video track is the lowest-numbered video track seen so far. Each of its key frames is offered, unshifted, to the child through a one-slot channel: taken when the child has said Hello and the slot is free (`KeysSent`), otherwise skipped (`KeysSkipped`); nothing else is sent and `Lost` deliveries are ignored. The child gets `Configure{Thumbnail: &opt.Thumbnail}` and decides which keys become shots. A child that exits is respawned with the backoff of `Transcoder` (`RespawnMin` doubling to `RespawnMax`); no hub is touched. A missing binary and a wire-version mismatch end `Run` with an error; on non-Unix systems `NewThumbnailer` returns `errors.ErrUnsupported`. Stopping sends `Stop`, waits 2 s and kills the process group, as `Transcoder` does (reuse `startProc` and `killGroup`).

- [ ] **Step 1: Write the failing tests**

```go
func TestNewEncoderValidation(t *testing.T)
// Encoder "qsv" → error; nvenc + Preset "veryfast" → error; nvenc + BFrames 2 → error; nvenc + "p5" → the
// child's Configure carries Encoder "nvenc", Preset "p5"; x264 rungs' Configure is byte-identical to before.
func TestWatermarkRejectsCopyRung(t *testing.T)   // New and SetWatermark, ladder {enc, copy}: the R-C5 error
func TestWatermarkInConfigure(t *testing.T)       // the fake's log: "configure text"
func TestSetWatermarkReachesChild(t *testing.T)   // log: "configure none", then "set text", then "set none" after SetWatermark(nil)
func TestSetWatermarkLatestWins(t *testing.T)     // three calls before Hello: the Configure carries the third; no "set" line
func TestWatermarkSurvivesRespawn(t *testing.T)
// SetWatermark(text), then the fake child exits: the next child's log line is "configure text".
func TestSetWatermarkBeforeAndAfterRun(t *testing.T) // no panic, no block; the value is used by a later Run
func TestThumbnailNeedsEncodedRung(t *testing.T)  // Options.Thumbnail with a copy-only ladder → error
func TestShotsFromChild(t *testing.T)
// EnvShots=1: Shot() returns the latest shot with `at` from the fake clock; OnShot saw every one;
// Stats().Child.Thumbnails follows the child's Stats; FramesOut and the hubs equal a run without thumbnails.
func TestThumbnailerForwardsKeysOnly(t *testing.T)
// a hub fed h264_aac.ts, EnvShots=1: KeysSent equals the keyframes delivered, the fake child received no
// audio and no non-key frame, Shots == KeysSent, Shot() is the last.
func TestThumbnailerRespawns(t *testing.T)        // the fake exits: Spawns 2, Exits 1, shots continue, Run still running
func TestThumbnailerSkipsWhenChildBusy(t *testing.T) // a fake that never reads: KeysSkipped rises, Run does not block, the hub's writes do not block
func TestThumbnailerEndsWithHub(t *testing.T)     // hub.Close → Run returns nil and the child is stopped
func TestThumbnailerVersionMismatch(t *testing.T) // EnvVersion=1 → Run returns an error naming the versions
```

- [ ] **Step 2: Run** `go test -race -count=1 ./internal/transcode/` — fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/transcode/...`, `golangci-lint run ./internal/transcode/...`, `GOOS=windows go vet ./...` — pass.
- [ ] **Step 5: Commit** `feat(transcode): per-rung encoder, live watermark, thumbnails and a thumbnailer for passthrough streams`

### Task 8: osdev — flags and `thumbnail.jpg`

**Files:**
- Modify: `cmd/osdev/main.go`
- Test: `cmd/osdev/main_test.go`

**Interfaces — Consumes:** Task 7's API; `picture.LoadAsset`.

- `-rung name=label:height:kbps[:nvenc|x264]`: the fourth field sets `Video.Encoder`.
- `-assets DIR`: the asset library (R-C8).
- `-watermark name=KEY=VALUE[,KEY=VALUE…]`, once per stream, keys `pos` (`tl`, `tr`, `bl`, `br`, `c`), `opacity`, `size`, `tz`, `image` (an asset name; needs `-assets`) and `text`; `text` must be the last key and takes the rest of the value verbatim, commas included. Needs a ladder; the transcoder's R-C5 error is reported at start-up.
- `-thumbnail name=SECONDS[:HEIGHT[:QUALITY]]`, once per stream. A stream with an encoded rung sets `Options.Thumbnail`; any other stream gets a `transcode.Thumbnailer` on its source hub, run and stopped with the stream, and needs `-ostranscoder`.
- `GET /live/{stream}/thumbnail.jpg`, registered next to `stream.ts`: for the source stream's name only. 200 with `Content-Type: image/jpeg`, `Cache-Control: no-cache`, `Last-Modified` (the arrival time) and `ETag: "<epoch>-<pts>"`; 304 on a matching `If-None-Match`; 404 for an unknown stream, a stream without `-thumbnail`, a rendition or `_abr` name, and until the first shot.
- The 30 s report adds, when they changed: a transcoder's `EncoderFallbacks` and `Thumbnails`, and a thumbnailer's counters. The package doc and usage text describe the three flags and the fourth rung field.

- [ ] **Step 1: Write the failing tests**

```go
func TestParseRungEncoder(t *testing.T)   // cam=a:360:800:nvenc → Encoder "nvenc"; :qsv → error; three fields → ""
func TestParseWatermark(t *testing.T)
// "cam=pos=br,opacity=80,size=6,tz=Asia/Ho_Chi_Minh,text=%H:%M:%S, live" → those fields and Text "%H:%M:%S, live";
// "cam=image=logo.png" without -assets → error; with it, Image holds the file; "cam=text=a,pos=br" → Text
// "a,pos=br"; an unknown key, both text and image, a stream without -rung → error.
func TestParseThumbnail(t *testing.T)     // cam=10 → {10,0,0}; cam=5:240:70; cam=0, cam=x, a repeat → error
func TestThumbnailHTTP(t *testing.T)
// a file stream with -thumbnail v=1 and the fake transcoder child (EnvShots=1) as -ostranscoder: GET
// /live/v/thumbnail.jpg is 404, then 200 image/jpeg with an ETag; the same ETag in If-None-Match → 304;
// /live/w/thumbnail.jpg (no -thumbnail) and /live/nope/thumbnail.jpg → 404.
func TestWatermarkCopyRungRefused(t *testing.T) // -rung v=src:copy -rung v=a:120:150 -watermark v=text=x → run returns non-zero and stderr names the rendition
```

- [ ] **Step 2: Run** `go test -race -count=1 ./cmd/osdev/` — fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test -race -count=1 ./cmd/osdev/` — pass.
- [ ] **Step 5: Commit** `feat(osdev): nvenc rungs, -watermark, -thumbnail and thumbnail.jpg`

### Task 9: The root cause of R-T7b' (R-C11)

**Files:**
- Create: `docs/superpowers/notes/2026-11-25-r-t7b-root-cause.md` (the measurements)
- Modify: `internal/transcode/native/video.go` (only the `x264Params` comment, or the params if the outcome is (b)), `docs/REBUILD_PLAN.md` (§3.5's `b-pyramid` sentence, the Backlog item)
- Test: `internal/transcode/native/pipeline_test.go` (`TestRungReorderSignalled`)

Experiments use scratch builds of the child with `x264Params` edited by hand; **no experiment code is committed**. Every VLC run is VLC 3.0.20 in `osv2-ci-peers:m4a` (`scripts/test-ci-peers.sh --name osv2-t7b-<n> --exec …`), against rung `t180` of `h264_aac.ts` served as HLS TS by a Go test server in the same container, 16 runs per cell, with `-vvv` logs kept. The symptom is the log line "buffer deadlock prevented".

**What is known.** VLC 3 prints the line from `input_DecoderWait` (`src/input/decoder.c`): while the input is still pre-buffering, a decoder that has emptied its FIFO and gone idle without having produced its first picture. So the trigger is the decoder's first-picture latency against what the pre-buffer holds, and the leading hypothesis is **H1:** with B-pyramid, x264 announces `max_num_reorder_frames` 2 and a DPB of 4 (against 1 and 2 without), so libavcodec in VLC holds two more pictures before its first output, on top of its frame threads; on a small rung that crosses what VLC has buffered at start. H1 predicts a VLC start-up limitation, not a defect in our stream.

- [ ] **Step 1: Baseline.** Cells A (`b-pyramid=normal`, frame threads), B (`b-pyramid=none`, today), C (`b-pyramid=normal`, `sliced-threads=1`). Record per cell: occurrences in 16 runs; the SPS's `max_num_reorder_frames` and `max_dec_frame_buffering`; PTS − DTS of the first IDR; the DTS of the first 12 frames; and from the VLC log, the "Stream buffering done (… ms in … ms)" line and the time of "Received first picture" relative to the deadlock line.
- [ ] **Step 2: Test H1 in both directions.** (i) Cell A played with `--avcodec-threads=1`, and cell A played with `--network-caching=3000`: H1 predicts 0 of 16 in at least one. (ii) Cell B (pyramid off) played with a larger decoder delay and a smaller pre-buffer: `--avcodec-threads` raised stepwise (2, 4, 8, 16) with `--network-caching=300`: H1 predicts the symptom appears at some step. (iii) Cell D, `b-pyramid=strict` (a simpler pyramid that still announces reorder 2): H1 predicts cell A's rate.
- [ ] **Step 3: If H1 fails, test H2 and H3.** H2, DTS spacing: compare the first 12 DTS deltas of cells A and C; if A's are irregular, replay A through a re-stamp that makes them uniform (a scratch change in the child's `frame()`) and count. H3, announced against real reorder: decode cell A's rung with `ffmpeg -strict strict -err_detect explode` and with a DPB-conformance check (`ffmpeg -debug mmco`), looking for a picture output later than the SPS allows.
- [ ] **Step 4: Decide, and record.** The cause is *found* when one controlled change flips the symptom to 0 of 16 with the pyramid still on, and its inverse brings it back with the pyramid off.
  - **Outcome (a), a VLC start-up limitation (H1 holds):** keep `b-pyramid=none`. Rewrite the `x264Params` comment and §3.5's sentence to name the cause and the numbers; move the Backlog item to the "VLC limitations" notes as closed. The NVENC rule stands as R-C3.
  - **Outcome (b), a defect in our output:** fix it in the child, add the check that catches it (a conform rule, or a native test over every encoder) and keep `b-pyramid=none` in this milestone; restoring the pyramid is a new ruling for the user.
  - Either way, write the cells and counts into the notes file.
- [ ] **Step 5: If neither outcome is reached** after Steps 1–3: stop, write the evidence into the notes file, and report to the controller. M4c is not marked done on this item without the user's decision (see the plan's open question).
- [ ] **Step 6: Pin the rule.**

```go
func TestRungReorderSignalled(t *testing.T)
// x264 rungs with BFrames 0 (→ 2), 3 and −1 on h264_aac.ts: the SPS of each rung's Config gives
// h26x.ReorderOf(h264, sps) == 1, 1 and 0, with assumed == false. (Task 4's TestNVENCRung pins 0 for NVENC.)
```

- [ ] **Step 7: Run** `make test-transcode ARGS='-run ^TestRungReorderSignalled$ ./internal/transcode/native/'` — pass.
- [ ] **Step 8: Commit** `docs: the root cause of VLC 3's buffer deadlock on pyramid rungs (R-T7b')` and `test(native): pin the reorder depth every rung announces`

### Task 10: The M4c scenarios (software encoders; run in CI)

**Files:**
- Create: `internal/e2e/m4c_test.go`
- Modify: `internal/e2e/m4a_test.go` only by adding functions

**Interfaces:**
- Consumes: Tasks 4–8; `transcodeSteps`, `m4aSource`, `feedScenario` and the keyframe-alignment helper of `TestM4aKeyframesAligned`.
- Produces (for Task 11): `m4cLadder(binary, encoder string) transcode.Options` (three encoded rungs `t240`, `t180`, `t120` at 600, 300 and 150 kbps with `Encoder` set, audio AAC 96 kbps, `Queue` 2048, **no** copy rung); `firstPicture(t, ffmpeg, frames []media.Frame) image.Image` (muxes a rendition's first GOP to a TS file with `internal/ts` and decodes its first picture with `ffmpeg -frames:v 1 -f rawvideo -pix_fmt rgb24`); `markRegion(img, pos, sizePct) (r, g, b float64)`; `magentaPNG() []byte`.

- [ ] **Step 1: Write the tests** (each needs the real child through `testpeer`'s `OS_TRANSCODER` helper, and ffmpeg)

```go
func TestM4cWatermarkEveryRung(t *testing.T)
// h264_aac.ts through m4cLadder(bin, "") with the image mark (magentaPNG, SizePct 25, TopLeft): on every
// rendition, the first picture's mark region is within 40 of (255,0,255) and the BottomRight region is not;
// every rendition passes feedScenario's conform read with no violation; the three rungs' keyframe PTS sets
// are equal to the source's + Delay (as TestM4aKeyframesAligned). A second run with Text "%Y-%m-%d %H:%M:%S",
// TimeZone "Asia/Ho_Chi_Minh": on every rendition the top-left band differs from the unmarked run's first
// picture by a mean absolute difference above 2.
func TestM4cWatermarkABR(t *testing.T)
// the same ladder with Presentation: the presentation packaged in ABR mode (feedABR) has no violation and
// every variant's first picture carries the mark.
func TestM4cWatermarkLiveUpdate(t *testing.T)
// h264_aac.ts fed twice at its pace, no watermark at start; after 4 s SetWatermark(image): every rendition has
// exactly one epoch, Stats.Spawns == 1, the first picture of the run has no mark and the first picture of the
// last GOP has it.
func TestM4cWatermarkRespawn(t *testing.T)
// the image mark on; the child is killed after its 3rd output keyframe (TestM4aRespawn's helper): the first
// picture of epoch 2 on every rendition carries the mark.
func TestM4cThumbnailsTranscoded(t *testing.T)
// h264_aac.ts fed four times as one timeline (24 s) through m4cLadder with Thumbnail{IntervalSec: 2,
// Height: 120}, shots collected by OnShot: at least 11; within an epoch, consecutive PTS differ by at least
// 180000 and at most 180000 + 3600; every JPEG decodes to 160×120; the first and the last differ;
// Shot() returns the last one.
func TestM4cThumbnailsPassthrough(t *testing.T)
// the same feed into a hub with only a Thumbnailer (interval 2, height 120): at least 11 shots; consecutive
// PTS differ by at least 180000 and less than 270000 (the fixture's keyframes are 1 s apart); 160×120.
// Rows for h264_opengop.ts and h265_aac51.ts (fed twice): at least 4 shots each, all decodable.
func TestM4cFrozen(t *testing.T)
// h264_aac.ts through m4aLadder(bin) twice, once as M4b ran it and once with Watermark nil, Thumbnail nil
// and Encoder "x264" spelled out: per rendition, the same frames (PTS, DTS, Key, Epoch, Data).
```

- [ ] **Step 2: Run** on the host: `make ostranscoder && OS_TRANSCODER=$PWD/dist/ostranscoder-amd64/ostranscoder go test -race -count=1 -run 'TestM4c(Watermark|Thumbnails|Frozen)' ./internal/e2e/` — pass.
- [ ] **Step 3: Run** the same in `make test-ci-peers` (ffmpeg 6.1) with `OS_TRANSCODER` set — pass.
- [ ] **Step 4: Commit** `test(e2e): the M4c watermark and thumbnail scenarios`

### Task 11: `make test-nvenc` and the NVENC scenarios (R-C12)

**Files:**
- Create: `scripts/test-nvenc.sh`, `internal/e2e/m4c_nvenc_test.go`
- Modify: `Makefile` (`test-nvenc`, `.PHONY`), `internal/testpeer/testpeer.go` (`NVENC`), `internal/testpeer/testpeer_test.go`, `CLAUDE.md` (the Commands block)

**Interfaces:**
- Consumes: `ostranscoder -probe nvenc` (Task 4); `m4cLadder`, `firstPicture` (Task 10); `m4aLadder`, `m4aRuns`' recorder, `m4bRuns`, `feedABR`, `feedScenario`.
- Produces: `const EnvRequireNVENC = "OS_REQUIRE_NVENC"`; `func NVENC(t testing.TB, transcoder string)`: runs `<transcoder> -probe nvenc`; on exit 0 returns; otherwise `t.Skipf("no NVENC device: %s", line)`, or `t.Fatalf` when `OS_REQUIRE_NVENC=1`.

**The scenarios** (`m4c_nvenc_test.go`; every test starts with `testpeer.NVENC`, so in CI and in the peers container each skips, naming the probe's reason). The NVENC ladder is `m4aLadder` with `Encoder: "nvenc"` on `t240`, `t180` and `t120`, the copy rung kept, `Presentation` on. Fixtures are transcoded **one after another** (no `t.Parallel` around a transcoder run), so at most 3 sessions are open.

```go
func TestM4cNVENCMatrix(t *testing.T)
// h264_aac.ts, h264_aac_then_640.ts, h264_aac_nob.ts, h264_pyramid.ts through the NVENC ladder. Per fixture:
// Stats.Child.EncoderFallbacks == 0 and the run is clean as m4bRuns requires. Per rendition, TestM4aMatrix's
// rows steady-state, epoch-change, no-bframes and b-frames with their assertions; the three rungs' keyframe
// PTS equal the source's + Delay; no NVENC rung frame has a NAL of type 6 containing "x264"; DTS == PTS on
// the NVENC rungs. The presentation, packaged in ABR mode and read as hls-ts, hls-fmp4 and dash with
// AlignedSequences: no violation, assertCompleteABR.
func TestM4cNVENCProbe(t *testing.T)
// ffprobe on each NVENC rung's HLS TS of h264_aac.ts: codec_name h264, profile High, has_b_frames 0, heights
// 240, 180, 120, width 320, 240, 160; the copy rung is the source's. The values are logged as one line per
// rung ("nvenc-rung t240 h264 High 320x240 has_b_frames=0 kbps=…"), which the script collects.
func TestM4cNVENCSessions(t *testing.T)
// h264_aac.ts fed at its pace; 3 s in, `nvidia-smi --query-gpu=encoder.stats.sessionCount
// --format=csv,noheader,nounits` reads base + 3, where base is the count before the run (0 unless
// OS_NVENC_SHARED=1); 2 s after the transcoder stops it reads base again. Skips the count when nvidia-smi
// is not on PATH.
func TestM4cNVENCMixed(t *testing.T)
// {t240 nvenc, t180 x264, t120 nvenc, src copy}, h264_pyramid.ts, Presentation: clean run, ABR read with no
// violation, EncoderFallbacks == 0.
func TestM4cNVENCWatermarkThumbnails(t *testing.T)
// m4cLadder(bin, "nvenc") with the image mark and Thumbnail{2, 120, 80} on h264_aac.ts fed four times: the
// mark on every rung's first picture (TestM4cWatermarkEveryRung's rule), at least 11 shots 2 s apart.
func TestM4cNVENCKeep(t *testing.T)
// when OS_NVENC_MEDIA names a directory (relative to the repository root), writes the NVENC presentation of
// h264_aac.ts as a static, ended HLS-TS tree (index.m3u8, v{track}.m3u8, segments) into it; otherwise skips.
func TestM4cNVENCRecordedVLC(t *testing.T)
// no device needed: when OS_NVENC_MEDIA names a directory with index.m3u8, serves it from an
// http.FileServer and plays variant v0 with VLC (vlcPlays, tolerate false: "buffer deadlock prevented" is a
// failure here); otherwise skips naming OS_NVENC_MEDIA.
```

**`scripts/test-nvenc.sh`** (POSIX sh, `set -eu`), run by `make test-nvenc`:
1. Require `nvidia-smi`; read `encoder.stats.sessionCount` and `--query-compute-apps=pid`. When the count is not 0 or a compute process exists: print them and exit 1 ("NVENC is in use by another process; not disturbing it"), or with `OS_NVENC_SHARED=1` print a warning and go on. Start no container but the two below, both `osv2-*` and `--rm`; bind no port outside the tests' ephemeral ones.
2. `p=$(sh scripts/ffmpeg-dev.sh amd64)`; assert the pinned FFmpeg lists the encoder: `LD_LIBRARY_PATH=$p/lib $p/bin/ffmpeg -hide_banner -encoders | grep -qw h264_nvenc`.
3. `make ostranscoder`; then `dist/ostranscoder-amd64/ostranscoder -probe nvenc` must print `nvenc: ok`.
4. On the host: `OS_TRANSCODER=$PWD/dist/ostranscoder-amd64/ostranscoder OS_REQUIRE_NVENC=1 OS_REQUIRE_PEERS=1 OS_NVENC_MEDIA=dist/nvenc-report/media go test -race -count=1 -timeout=15m -v -run '^TestM4cNVENC' ./internal/e2e/`, and the native device tests of Task 4, Step 4, both teed to `dist/nvenc-report/test.log`; any failure or skip of a `TestM4cNVENC*` test other than `TestM4cNVENCRecordedVLC` fails the script.
5. In the peers image (VLC 3.0.20): `OS_NVENC_MEDIA=dist/nvenc-report/media sh scripts/test-ci-peers.sh --name osv2-nvenc-vlc-$$ --exec go test -count=1 -v -run '^TestM4cNVENCRecordedVLC$' ./internal/e2e/`, appended to the log; a skip fails the script.
6. Read the session count again: it must equal step 1's.
7. Write `dist/nvenc-report/report.md`: the date; `nvidia-smi --query-gpu=name,driver_version,memory.total --format=csv,noheader`; the FFmpeg archive name; `ostranscoder -version`; the commit (`git rev-parse HEAD`); the `--- PASS`/`--- FAIL`/`--- SKIP` lines of the log; the `nvenc-rung …` lines; the session counts before, during and after.

- [ ] **Step 1: Write** `testpeer.NVENC` with its test (`TestNVENCProbeSkipsOrFails`: a fake transcoder script exiting 3 → skipped; with `OS_REQUIRE_NVENC=1` → failed; exiting 0 → neither), then the scenarios.
- [ ] **Step 2: Run** `go test -race -count=1 -run 'TestM4cNVENC' ./internal/e2e/` in `make test-ci-peers` with `OS_TRANSCODER` set — every test skips with `no NVENC device: …` or the `OS_NVENC_MEDIA` reason; none fails.
- [ ] **Step 3: Write** the script and the make target; add `make test-nvenc` to `CLAUDE.md`'s Commands with one line saying it runs on the GPU host only.
- [ ] **Step 4: Run** `make test-nvenc` on this host — exit 0; `dist/nvenc-report/report.md` lists every `TestM4cNVENC*` as PASS, three `nvenc-rung` lines, and sessions 0 / 3 / 0. Then `docker ps --format '{{.Names}}'` shows no `osv2-*` container and the soak containers still running.
- [ ] **Step 5: Run** `sh scripts/test-nvenc.sh` once while a second terminal holds an NVENC session (`ffmpeg -f lavfi -i testsrc2 -c:v h264_nvenc -f null -` from the pinned build, stopped afterwards) — it exits 1 with the refusal and starts nothing.
- [ ] **Step 6: Commit** `test(e2e): the NVENC scenarios and make test-nvenc`

### Task 12: CI, spec — M4c done

**Files:**
- Modify: `.github/workflows/ci.yml`, `docs/REBUILD_PLAN.md`, `CLAUDE.md`

- [ ] **Step 1: CI.** Add `TestM4cWatermarkEveryRung TestM4cThumbnailsTranscoded TestM4cThumbnailsPassthrough TestM4cFrozen` to `E2E_TESTS` of `transcode-e2e-arm64` (ffmpeg is installed there; the live-update and respawn tests run in `test`). In both transcoder jobs, after "Run the bundle", add a step that runs `dist/ostranscoder-$OSTX_ARCH/ostranscoder -probe nvenc` and requires exit 3 with a line starting `nvenc: ` (the code is built in and reports the missing device cleanly). The device tests already skip there with their reason.
- [ ] **Step 2: Run** the whole gate locally: `make check`; `make test-transcode`; `make ostranscoder`; `OS_TRANSCODER=… make test` on the host; `OS_TRANSCODER=… make test-ci-peers`; `GOOS=windows go vet ./... && GOOS=darwin go vet ./...`; `make test-nvenc`.
- [ ] **Step 3: Spec** (`docs/REBUILD_PLAN.md`):
  - §3.5: a bullet "NVENC (M4c)" with R-C1 to R-C4 as rules; a bullet "Watermark (M4c)" with R-C5 to R-C9; a bullet "Thumbnails (M4c)" with R-C10; wire version 2 and the new messages in the wire bullet; the `b-pyramid` sentence as Task 9 left it.
  - §5: row M4c → done, with this plan's link and the tests that prove it (`TestM4cNVENCMatrix`, `TestM4cNVENCProbe`, `TestM4cNVENCSessions`, `TestM4cWatermarkEveryRung`, `TestM4cWatermarkABR`, `TestM4cThumbnailsTranscoded`, `TestM4cThumbnailsPassthrough`, `make test-nvenc`), and the body of `dist/nvenc-report/report.md` as the recorded evidence.
  - An "Out of M4c" table: NVENC B-frames and `b_ref_mode` (R-C3); the full-GPU path (R-C2); HEVC and AV1 NVENC; an operator font and non-Latin text (R-C7); several watermarks, colours, rotation; a watermark on a copy rung (R-C5); thumbnail history, sprites and files (R-C10); the asset API (M5); metrics for `EncoderFallbacks` (M5); NVENC in CI.
  - §2's "Platforms" row: "with NVIDIA NVENC in M4c" → "and NVIDIA NVENC (`h264_nvenc`, per rung)". The M4a "Out of" rows that say "M4c" → "M4c (done)".
  - Backlog: close the R-T7b' item with Task 9's finding; add "Full-GPU transcode path", "NVENC B-frames on capable cards", "Operator fonts for the watermark", and "A fallback to libx264 can overload a host sized for NVENC: export `EncoderFallbacks` and let a stream refuse the fallback (M5)".
- [ ] **Step 4: CLAUDE.md:** the Commands block (`make test-nvenc`), and one Invariants line: "`internal/transcode/picture` is pure Go; operator text never enters a libavfilter graph string."
- [ ] **Step 5: Commit** `ci: run the M4c scenarios and the NVENC probe` and `docs: M4c done`. The controller posts `dist/nvenc-report/report.md` on PR #53.
