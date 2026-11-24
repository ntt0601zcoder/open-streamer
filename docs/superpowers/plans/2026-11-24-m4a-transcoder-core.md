# M4a: Transcoder Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A per-stream transcoder subprocess turns one source hub into N rendition hubs with a software H.264 ladder, AAC re-encode or copy, and per-track passthrough. It runs on Linux amd64 and arm64, and it respawns as exactly one new epoch.

**Architecture:**
- **Host side (`internal/transcode`):**
  - subscribes once to the source hub;
  - stamps every frame with its rendition epoch;
  - shifts it by the rendition delay D;
  - sends encode tracks to `cmd/ostranscoder` and copy tracks to the renditions directly. The child is reached over a socketpair on fd 3, using a length-prefixed protobuf-wire protocol.
- **Child side (`cmd/ostranscoder`, wrapping `internal/transcode/native`):**
  - built with cgo and go-astiav, behind the build tag `libav`;
  - decodes once, then scales and encodes per rung;
  - passes PTS and epochs through;
  - forces an IDR at the PTS of every source keyframe, so every rung and the source stay segment-aligned.
- **Rendition hubs:** a per-rendition DTS merger writes each rendition hub. The first task makes the segmenter cut audio at the video boundary's PTS (ruling R-F3).

**Tech Stack:**
- Go 1.26.11.
- `asticode/go-astiav` v0.41.0.
- FFmpeg 8.1.3 shared, from a pinned FFmpeg-Builds release (linux64 and linuxarm64, GPL).
- `google.golang.org/protobuf/encoding/protowire` only: no codegen and no protoc.

**Spec:** `docs/REBUILD_PLAN.md`: §3.1 Frame, §3.2 Clock, §3.3 Hub, §3.4 Output kit, §3.5 Transcoder, §3.7 Libraries, the §5 M4 row, and the Backlog item "Cut audio segments and parts at the video boundary's PTS".

**Rulings made by this plan:**
- **R-M4a, the split of M4:**
  - M4a (this plan) is the segmenter fix, the transcoder core, the software ladder, passthrough, respawn and CI on amd64 and arm64. Each rendition is served as its own stream, `<name>_<label>`.
  - M4b is the ABR outputs: HLS multivariant and DASH representations.
  - M4c is NVENC on the GPU host (this machine, a GTX 1650), with a make target whose results are recorded in the PR, plus the watermark and thumbnails.
  - Cost if wrong: "a 3-rung ladder passes M3's scenarios" is proven per rendition in M4a, and as one ABR presentation only in M4b.
- **R-M4b, FFmpeg-Builds instead of v1's 11.6 GB CUDA builder image:** no FFmpeg compile and no image. The cost if wrong is a dependency on FFmpeg-Builds keeping month-end releases, which it does, and no libfdk-aac, so the native `aac` encoder is used.
- **R-M4c, the rendition delay D:** every frame and anchor of every rendition is presented at source time + D, with D = 45000 ticks (0.5 s).
  - D covers the encoder's reorder delay (B-frames ≤ 3 at ≥ 8 fps), so encoder DTS is never negative, including for sources without B-frames.
  - DTS order across epochs is guaranteed separately, by the Clock's gap between epochs and the encoder's constant delay.
  - `New` rejects BFrames > 3. The bound does not hold below about 8 fps with B-frames 3, or about 6 fps with B-frames 2. The doc says so; such sources should use BFrames −1.
  - Copy tracks get the same shift, so all renditions stay aligned with each other.
  - The cost if wrong is renditions playing 0.5 s behind the source stream.
- **R-M4d, platforms (user decision, 2026-11-24):**
  - The server is pure Go and builds for any OS. `internal/transcode` returns `errors.ErrUnsupported` on non-Unix systems.
  - The transcoder runs on Linux amd64 and arm64, with glibc ≥ 2.39 (Ubuntu 24.04+, Debian 13+).
  - Encoding is software (libx264), with NVIDIA NVENC in M4c.
  - macOS, Windows, VAAPI, QSV and VideoToolbox are out of v2.0.

## Global Constraints

- **Libraries:**
  - Only approved, independent libraries.
  - New in M4a: `github.com/asticode/go-astiav v0.41.0` and `google.golang.org/protobuf` (`encoding/protowire` only).
  - No bluenviron, no lal, and no code copied from another product. v1 (our own code) may be read.
- **cgo and the `libav` tag:**
  - Only `internal/transcode/native` and `cmd/ostranscoder` use cgo, and only behind the build tag `libav`.
  - Without the tag, `go build ./...`, `go vet ./...`, `golangci-lint run ./...` and `make test` pass on a machine without libav, and so do `GOOS=windows go vet ./...` and `GOOS=darwin go vet ./...`.
  - Untagged, `cmd/ostranscoder` builds to a stub that prints "ostranscoder: built without -tags libav" and exits 2.
- **FFmpeg:**
  - The source is FFmpeg-Builds release `autobuild-2026-10-31-12-59`:
    - amd64: `ffmpeg-n8.1.3-27-g8dca1586f4-linux64-gpl-shared-8.1.tar.xz`, sha256 `202fe67679a0860b155817a0907ffcc5c1ea82e186acaa21cb87d307f54fd59a`;
    - arm64: `ffmpeg-n8.1.3-27-g8dca1586f4-linuxarm64-gpl-shared-8.1.tar.xz`, sha256 `9d3161962e57f0f621b87c1fe4287ad14b4fd12c74c243436143319e4e14a0a0`.
  - `scripts/ffmpeg-dev.sh <amd64|arm64>` downloads the archive into `${XDG_CACHE_HOME:-$HOME/.cache}/osv2-ffmpeg/`, checks the sha256 before unpacking, and asserts that `pkg-config --modversion libavcodec` starts with `62.`. It prints the prefix.
  - Its `.pc` files are relocatable (`prefix=${pcfiledir}/../..`), so plain `PKG_CONFIG_PATH=<prefix>/lib/pkgconfig` works.
  - The user approved downloading the amd64 archive locally.
- **Bundle:**
  - Built with `go build -tags libav -ldflags '-r $ORIGIN/lib -extldflags=-Wl,--disable-new-dtags'` (written `$$ORIGIN` in make).
  - `dist/ostranscoder-<arch>/` holds `ostranscoder` plus `lib/<soname>` for every library that `LD_LIBRARY_PATH=<prefix>/lib ldd` resolves inside the prefix, copied with `cp -L`. Any "not found" fails the build.
  - It is built on Ubuntu 24.04: in CI on `ubuntu-24.04` and `ubuntu-24.04-arm`; locally inside `osv2-ci-peers:m4a` (the CI-peer image with `pkg-config` added), so only glibc comes from the container; vet and lint run on the host with `PKG_CONFIG_PATH`.
  - CI uploads it as a tar, which keeps the exec bit.
  - `dist/` is git-ignored.
- **Time:**
  - 90 kHz everywhere. Every libav codec context uses time base 1/90000.
  - **D is added once, by the host,** before frames reach the child or a merger. The child sees frames already shifted.
  - Video output PTS = input PTS. Output DTS = encoder DTS.
  - Audio: each encoder input frame gets PTS = anchor + `media.SampleTime`(n × 1024, rate). Output PTS is the packet's.
    - The anchor is the first input audio PTS of the epoch, after decoder and encoder rebuild.
    - **Forward re-anchor:** when an input frame's PTS is more than one frame duration after the predicted PTS, re-anchor to it, and reset the FIFO and count.
    - **Backward deviation:** when the input is more than one frame duration earlier than predicted, never re-anchor backwards. Drop input samples until the input meets the prediction, and count them in `Stats.AudioDropped`. Smaller deviations, such as millisecond-timestamp jitter, are ignored.
    - Only the first packet after each encoder build may be priming. Drop it if its PTS is before the anchor.
- **Frames (§3.1):**
  - Video `Data` carries no VPS, SPS, PPS or AUD, and keeps SEI.
  - `Config` pointers are stable, with one `media.ConfigCache` per rung and per audio track.
  - Audio is one raw AAC frame with no ADTS header.
  - An output frame's `Track` is its source track index.
- **Tracks:**
  - The lowest-numbered source video track is encoded and copied. Other video tracks are dropped, logged once, and counted in `Stats.DroppedTracks`.
  - Every audio track gets its own decoder, resampler and encoder (AAC mode), or is copied (copy mode).
- **Keyframes:**
  - Every rung encodes an IDR at exactly the PTS of every source frame with `Key`. The decoded frame is matched by PTS, because the decoder's own key flag misses H.264 recovery points. The IDR is forced with `pict_type` I, the key flag and `forced-idr=1`; every other frame's `pict_type` is reset to none.
  - libx264 uses `x264-params` = `scenecut=0:keyint=600:min-keyint=1:rc-lookahead=10:b-pyramid=none` (`b-pyramid=none`: ruling R-T7b', VLC 3 otherwise logs "buffer deadlock prevented" on encoded rungs).
  - Defaults are B-frames 2, preset `veryfast`, threads 0 (auto), and MaxBitrate = Bitrate when 0, with bufsize = 2 × maxrate.
- **Rendition epochs (§3.5):**
  - The host's `epochMap` assigns the epoch of every frame before it goes to the child or a merger. Upstream epoch e maps to e + offset.
  - A *bump* opens a new rendition epoch, numbered (largest issued) + 1, and raises the offset. Respawns and source-subscription loss cause bumps.
  - **Where a bump takes effect:** at K. K is the next source video keyframe, or the next frame when the upstream epoch has no live video track (no video track, or none live in the hub's `LiveWindow`).
    - For a loss with live video, K is the first keyframe after the `Lost` delivery (itself the resume keyframe). This costs one GOP, so the pending-audio buffer can collect the audio that leads K. Encode-path frames from the resume keyframe to K are dropped. Copy-path frames stay in the old epoch, which resumes on a keyframe.
    - For a loss without live video, K is the `Lost` delivery itself.
    - In every case K must be a keyframe whose PTS is above the largest PTS already sent in the old epoch on any track. If it is not, the next keyframe that is becomes K.
    - For a respawn, K is the first source keyframe delivered after the new child's `Hello`.
  - **Pending bump:**
    - While a bump is pending, audio is buffered for at most 1 s. This covers copied audio and child-bound audio.
    - At K, buffered audio with PTS < K is dropped and counted, and audio with PTS ≥ K goes into the new epoch.
    - If the 1 s bound is exceeded, the oldest buffered audio goes out in the old epoch.
  - **Frames before K:** "before K" means decode order for video and PTS < K for audio.
    - Encode-path video before K is dropped and counted in `DroppedBeforeKey`, and the child is fed from K.
    - Copy-path video before K stays in the old epoch.
    - From K on, the copy path drops frames marked `RASL` until the first trailing picture, as the hub does on resume.
    - After K takes effect, audio with PTS < K is dropped and counted in `DroppedBeforeKey`, whenever it arrives and on either path.
  - **Coalescing:**
    - A bump requested while one is pending is absorbed.
    - An epoch number is issued only when K takes effect.
    - If an upstream epoch starts exactly at K, it is that same rendition epoch.
  - **One subscription:** the host keeps one source subscription across respawns.
  - The child passes epochs through.
  - One anchor per rendition epoch is set on every rendition hub before that epoch's first frame:
    - for a remapped upstream epoch: Start = upstream Start + D, Wall = upstream Wall + D;
    - for a bump: Start = K + D, Wall = upstream Wall + (K − upstream Start) + D, where "upstream" is the epoch K belongs to.
- **Hub writes never block.** The host never blocks the source hub. A slow child shows up as `Lost` on the host's source subscription.
- **Errors and style:**
  - Wrap errors as `fmt.Errorf("transcode: op: %w", err)`, and never both log and return one.
  - Every goroutine has a cancel path.
  - Lint: `exhaustive` (default is not exhaustive), `godot`, gofumpt.
- **Soak safety:**
  - Never stop, restart or reconfigure the existing containers: open-streamer-soak-mediamtx, caddy, grafana, osdev-console-1, vector, alloy, pyroscope, prometheus, node-exporter, nvidia-gpu-exporter.
  - Own containers are named `osv2-*`, run with `--rm`, and are stopped afterwards.
  - Never bind 554, 1935 or 8554.
  - MediaMTX runs only as the throwaway on 18554/18000/18001 with `MTX_MOQ=no` and every other server off, verified with `ss`.
- **CI versions locally:**
  - Before any push, run the peer-using packages with `make test-ci-peers` (ffmpeg 6.1, VLC 3.0.20, GStreamer 1.24) and `OS_TRANSCODER=<host path to dist/ostranscoder-amd64/ostranscoder>`. The script mounts that path at the same path, as it does for `OS_FFMPEG8`.
  - arm64 is proven on CI only, because this host has no qemu.
- **Commits** end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. After each wave merge, run `go mod tidy`.

## Execution waves (parallel per user instruction)

| Wave | Tasks (parallel within a wave, one worktree each) | Why independent |
|---|---|---|
| 0 | T0 deps and fixtures | Everything else builds on it |
| 1 | T1 segmenter, T2 wire, T3 build and CI, T4 native | Disjoint files; T0 already added go.mod, the scripts and the make targets |
| 2 | T5 supervisor, T6 ostranscoder main | T5 needs T0 and T2; T6 needs T2, T3 and T4. Disjoint files |
| 3 | T7 osdev, real-child test and e2e | Needs T1, T5 and T6 |
| 4 | T8 arm64 CI, then T9 spec | T8 needs T7's tests |

Within a wave, merge in task-number order. After each wave, the controller runs `go mod tidy`, `make test`, vet and lint on the merged tree.

## Review Focus

1. **Respawn or loss on a rendition that mixes copied and encoded tracks.** The new epoch must start on a keyframe, with the 40 ms start-offset rule held, and with no A/V offset. Pinned in T5: `TestSupervisorMixedRespawn`.
2. **A source without B-frames.** HLS fMP4 and DASH must accept every rung (D covers the encoder delay). Pinned in T4 `TestPipelineNoBFrames` and T7 `TestM4aMatrix/*/no-bframes`.
3. **An audio gap inside an epoch.** There must be no lasting A/V drift (re-anchor). Pinned in T4: `TestPipelineAudioGap`.
4. **Two audio tracks and a resolution change mid-stream.** Each track is encoded separately, and a rung's config changes only at an epoch. Pinned in T4: `TestPipelineTwoAudio` and `TestPipelineResolutionChange`.
5. **The bundle fails to run**, through a lost exec bit, missing libs or glibc, or a child that crash-loops, is missing, or speaks the wrong version. Pinned in T3 (container `-version` run and CI tar) and T5: `TestSupervisorCrashLoopBacksOff`, `TestSupervisorMissingBinary`, `TestSupervisorVersionMismatch`.

---

### Task 0: Dependencies, fixtures and the build harness

**Files:**
- `go.mod`, `go.sum`: add `github.com/asticode/go-astiav v0.41.0` and `google.golang.org/protobuf` at the latest 1.36.x. They stay in the graph through blank imports in package-doc files:
  - `internal/transcode/native/doc.go`, tag `libav`: go-astiav;
  - `internal/transcode/wire/doc.go`, untagged: `protowire`.
- `scripts/ffmpeg-dev.sh`: as in the Global Constraints.
- `scripts/bundle-ostranscoder.sh <arch> <outdir>`: as in the Global Constraints.
- `scripts/test-ci-peers.sh`: image tag `osv2-ci-peers:m4a`. It mounts `${XDG_CACHE_HOME:-$HOME/.cache}/osv2-ffmpeg` at the same path, sets `PKG_CONFIG_PATH` to the prefix matching the host's arch when that directory holds an unpacked prefix, and mounts the `OS_TRANSCODER` path as it does for `OS_FFMPEG8`.
- `Makefile`:
  - `ostranscoder`: runs `scripts/bundle-ostranscoder.sh amd64 dist/ostranscoder-amd64` inside `osv2-ci-peers:m4a`, as container `osv2-ostx-build-$$` with `--rm`, using the same mounts as `test-ci-peers`.
  - `test-transcode`: runs `go test -race -tags libav $(ARGS)` in that container, with the same quoting as `test-ci-peers`.
- `.gitignore`: add `dist/`.
- `testdata/peers/Dockerfile.ci`: add `pkg-config`.
- `testdata/fixtures/gen.sh`: add `h264_aac_nob.ts` (h264_aac.ts with `-bf 0`) and its `_dts.txt`. Also add it to the fixtures README.
- `CLAUDE.md`: add `make ostranscoder` and `make test-transcode`.

- [ ] **Step 1:** Make the edits. Run `sh testdata/fixtures/gen.sh` with the host's ffmpeg 8. Keep only `h264_aac_nob.ts` and its dts file, and restore every other fixture with `git checkout testdata/fixtures/`.
- [ ] **Step 2:** Run `p=$(scripts/ffmpeg-dev.sh amd64) && PKG_CONFIG_PATH=$p/lib/pkgconfig pkg-config --modversion libavcodec`. Expected: `62.` followed by the minor version.
- [ ] **Step 3:** Run `make test-transcode ARGS='./internal/transcode/native/'` with only `doc.go` present. Expected: `ok` or "no test files", which proves the container, the mounts and pkg-config work.
- [ ] **Step 4:** Run `go mod tidy && go build ./... && go vet ./... && make test`. Expected: clean, and `go.mod` still lists both modules.
- [ ] **Step 5:** Commit: `build: add the transcoder dependencies, a no-B-frame fixture and the build harness`.

---

### Task 1: Segmenter cuts audio at the video boundary (R-F3)

**Files:**
- Modify: `internal/segment/segmenter.go` and `segmenter_test.go`.
- Modify: `internal/e2e/vlc_test.go` (replace the R-T7e matcher by ruling R-T1c's start-up tolerance).
- Modify: `docs/REBUILD_PLAN.md` (§3.4 and Backlog).

**Interfaces:**
- Produces: the same `Segmenter` API, plus `func (s *Segmenter) Late() uint64`.
- New guarantee: every non-anchor frame lies in the segment whose [Start, Start+Duration) contains its PTS, except frames counted by `Late()` and frames in the last segment emitted by `Flush`.

**Rule.** Moving and holding apply only to the cuts made within an epoch: the keyframe cut, the forced cut at `MaxDuration` (where K is the PTS of the frame that triggers it), and the cut when `promote` changes the anchor. Closes at an epoch change, at `Lost`, or at `Flush` never move frames and never hold.
- When such a cut at anchor PTS K closes segment S:
  - non-anchor frames already in S with PTS ≥ K move to the next segment;
  - S's per-track accumulators are rebuilt from S's remaining frames;
  - S is then *held*.
- A held segment is released when either of these happens:
  - every track in the wait set has delivered a frame with PTS ≥ K;
  - the anchor track has advanced 90000 ticks (1 s) past K.
- The wait set is the tracks other than the anchor that have frames in the epoch.
- A track that makes a hold time out leaves the wait set until it delivers again.
- When a `promote` cut's new anchor frame is not a keyframe, so no next segment opens, the moved frames go to `carry`.
- A non-anchor frame with PTS < K that arrives while S is held goes into S. After release, such a frame goes into the open segment and is counted by `Late()`.
- More than one segment can be held when Target < 1 s. Held segments are always emitted in sequence order.
- An epoch change, `Lost > 0` or `Flush` releases every held segment first, in order, then closes the open one.
- Duration and End are unchanged, because video decides them.

- [ ] **Step 1: Write the tests.**
  - `TestAudioCutAtVideoPTS`:
    - frames of `h264_aac.ts` from the existing fixture helper, reordered so audio leads video by up to 300 ms;
    - for each segment except the last flushed one, every audio frame satisfies `Start ≤ PTS < Start+Duration`;
    - the first audio PTS is in `[Start, Start+1920)`.
  - `TestAudioCutHoldReleasedByAnchor`:
    - audio stops after segment 2;
    - segment 3 is released once the anchor passes K+90000;
    - later segments are not held.
  - `TestAudioLateCounted`: one audio frame below K delivered after release lands in the next segment, and `Late() == 1`.
  - `TestAudioCutReleaseOnEpoch` and `TestAudioCutReleaseOnLost`: held segments come out in order before the new epoch's first segment.
  - `TestAudioCutForcedCut`: a source with no keyframe for 6 s. The forced cut moves audio as well.
  - `TestAudioCutShortTarget`: Target 0.5 s, with two held segments emitted in order.
- [ ] **Step 2:** Run `go test -race -run 'TestAudio' ./internal/segment/`. Expected: FAIL.
- [ ] **Step 3:** Implement the rule.
- [ ] **Step 4:** Run `go test -race ./internal/segment/ ./internal/output/... ./internal/e2e/`. Expected: PASS.
  - Expectations that the rule itself changes may be updated, for example tighter per-track EXTINF. List each one in the report.
- [ ] **Step 5:** Replace the R-T7e matcher by the start-up tolerance of ruling R-T1c (the segmenter rule is amended by R-T1d). Then run `make test-ci-peers ARGS='-race -count=1 -run ^TestM3VLCClass$ ./internal/e2e/'` 3 times. Expected: PASS.
  - If the "no reference clock" pair still appears, stop and report NEEDS_CONTEXT with the log.
- [ ] **Step 6:** Update §3.4 Segmenter with one sentence on the rule. Rewrite the R-F3 Backlog item to the VLC start-up items of R-T1c.
- [ ] **Step 7:** Commit: `fix(segment): cut audio at the video keyframe's PTS`.

---

### Task 2: Wire protocol (`internal/transcode/wire`)

**Files:** Create `internal/transcode/wire/wire.go`, `wire_test.go` and `fuzz_test.go`.

**Interfaces:**
- Consumes: `media.Frame`, `media.TrackConfig`, and `media.NewH264Config`, `NewH265Config` and `NewAACConfig`.
- Produces:
```go
package wire
const Version = 1
const MaxMessage = 32 << 20 // bytes after the 4-byte big-endian length
var ErrTooLarge, ErrMalformed error
type Message interface{ isMessage() }
type Hello struct{ Version uint32; PID int }                 // child → host, first
type Configure struct{ Rungs []Rung; Audio *AudioEncode }    // host → child, once; Audio nil = no audio in the child
type Rung struct{ Height, BitrateKbps, MaxBitrateKbps, BFrames int; Preset string }
type AudioEncode struct{ BitrateKbps, SampleRate, Channels int }
type Frame struct{ Rung int; F media.Frame }                  // host → child: Rung −1; child → host: rung index, or −1 for audio
type Discontinuity struct{}                                   // host → child
type Stats struct{ In uint64; Out []uint64; EncodeNanos []int64 } // child → host every 1 s
type Error struct{ Rung int; Message string; Fatal bool }
type Stop struct{}
func NewWriter(w io.Writer) *Writer; func (w *Writer) Write(m Message) error
func NewReader(r io.Reader) *Reader; func (r *Reader) Read() (Message, error) // io.EOF at a clean end
```

**Encoding:**
- Each message is one protobuf envelope; its field number selects the type (1 Hello … 7 Stop). It is hand-coded with `protowire`.
- A Frame carries track, epoch, pts and dts (sint64), key, rasl, data (repeated bytes) and config_id.
- The full TrackConfig is carried only the first time a Writer sees a given `*TrackConfig` pointer.
- The Reader rebuilds it with the `media` constructors and returns the same pointer for every later frame that carries that ID.

- [ ] **Step 1: Write the tests.**
  - `TestRoundTripEveryMessage`: every type compares equal after Write then Read. Frames are compared field by field, and the config by `Equal`.
  - `TestConfigSentOnce`: the second frame with the same config is at least `len(SPS)+len(PPS)` bytes smaller on the wire, and Read returns the identical pointer.
  - `TestConfigChangeNewPointer`: a new pointer with different SPS gets a new ID, and Read returns a new pointer that is not `Equal` to the old one.
  - `TestTooLarge`: a length of 32 MiB + 1 returns `ErrTooLarge`, with `testing.AllocsPerRun` showing no large allocation.
  - `TestTruncated`: returns `errors.Is(err, io.ErrUnexpectedEOF)`.
  - `FuzzRead`: never panics.
- [ ] **Step 2:** Run `go test -race ./internal/transcode/wire/`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test -race ./internal/transcode/wire/ && go test -run=^$ -fuzz=FuzzRead -fuzztime=30s ./internal/transcode/wire/`. Expected: PASS.
- [ ] **Step 5:** Commit: `feat(transcode): add the wire protocol between host and transcoder`.

---

### Task 3: ostranscoder build, bundle and CI jobs

**Files:**
- Create:
  - `cmd/ostranscoder/main.go`, tag `libav`: `-version` prints `ostranscoder wire <N> libavcodec <ver>` and exits 0. Anything else exits 2 until T6.
  - `cmd/ostranscoder/stub.go`, tag `!libav`.
  - `cmd/ostranscoder/stub_test.go`, tag `!libav`.
- Modify:
  - `.github/workflows/ci.yml`:
    - jobs `transcoder-amd64` (`ubuntu-24.04`) and `transcoder-arm64` (`ubuntu-24.04-arm`), each: apt `pkg-config`, `scripts/ffmpeg-dev.sh`, `go vet -tags libav` and `go test -race -tags libav` on `./internal/transcode/native/... ./cmd/ostranscoder/...`, the bundle, `-version`, then a tar and upload-artifact `ostranscoder-<arch>`;
    - the test and coverage jobs get `needs: transcoder-amd64`, download and untar the bundle, and set `OS_TRANSCODER`.
  - `.testcoverage.yml`: exclude `^cmd/ostranscoder`, `^internal/transcode/native` and `^internal/transcode/transcodetest`, with the comment "cgo behind -tags libav, covered by the transcoder jobs; transcodetest runs only in re-executed children".
  - `internal/testpeer/testpeer.go`: `EnvTranscoder = "OS_TRANSCODER"` and `func OSTranscoder(t testing.TB) string`. If the env var is set, it must run `-version`, otherwise the test fails. Without it, use `LookPath("ostranscoder")`, else `missing`.

**Interfaces:**
- Produces:
  - `dist/ostranscoder-amd64/ostranscoder`;
  - `testpeer.OSTranscoder(t)`;
  - CI artifacts `ostranscoder-amd64` and `ostranscoder-arm64`, each a tar.

- [ ] **Step 1: Write the tests.**
  - `TestOSTranscoderPeer`:
    - env set to `/nonexistent` → test failure;
    - unset with no binary → skip;
    - unset under `OS_REQUIRE_PEERS=1` → failure.
  - `TestStubExits2` (`!libav`): build the package to a temp dir, run it, and check the exit code is 2 and stderr is the message.
- [ ] **Step 2:** Run `go test ./internal/testpeer/ ./cmd/ostranscoder/`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4: Verify locally.**
  - `make ostranscoder && ./dist/ostranscoder-amd64/ostranscoder -version` prints libavcodec 62.
  - `ldd` shows every libav library resolving under `dist/ostranscoder-amd64/lib/`.
  - `docker run --rm --name osv2-ostx-check -v $PWD/dist:/d osv2-ci-peers:m4a /d/ostranscoder-amd64/ostranscoder -version` gives the same output.
  - Untagged `go build ./... && go vet ./... && golangci-lint run ./... && make test` is clean.
  - Validate the YAML with python3 `yaml`.
- [ ] **Step 5:** Commit: `build: build and bundle ostranscoder against a pinned FFmpeg 8.1`.

---

### Task 4: Native media pipeline (`internal/transcode/native`, tag `libav`)

**Files:** Create `pipeline.go`, `video.go`, `audio.go`, `annexb.go` and `pipeline_test.go`, all tagged `libav`. `doc.go` comes from T0.

**Interfaces:**
- Consumes: `media.*`; in tests, `internal/ts` to demux fixtures and `internal/conform.CheckFrames`.
- Produces:
```go
package native
type Rung struct{ Height, BitrateKbps, MaxBitrateKbps, BFrames int; Preset string } // BFrames −1 = none, 0 = 2
type Audio struct{ BitrateKbps, SampleRate, Channels int }                         // zero values = 128, 48000, 2
type Output struct{ Rung int /* −1: audio */; F media.Frame }                      // F.Track = source track
type Stats struct{ DroppedTracks, Reanchors, AudioDropped uint64 }
func New(rungs []Rung, audio *Audio) (*Pipeline, error) // audio nil: audio frames ignored
func (p *Pipeline) Push(f media.Frame) ([]Output, error) // f is already in rendition time, epoch and +D
func (p *Pipeline) Discontinuity() ([]Output, error)     // drain; resume at the next video key (audio-only: next frame)
func (p *Pipeline) Flush() ([]Output, error)
func (p *Pipeline) Stats() Stats
func (p *Pipeline) Close()
func Versions() string
```

**Behaviour:**
- **Decoder input:**
  - Annex B built from `Data`, with every VPS, SPS and PPS of `Config` prepended on `Key` frames.
  - The packet's pts and dts are the frame's.
  - After a CRA start, RASL frames are not sent until the first trailing picture.
- **Rebuild:** a new `Config` pointer or a new `Epoch` drains the decoder and every encoder. Outputs keep the old epoch. Then the decoder, scalers and encoders are rebuilt.
- **Per rung:**
  - Scaling: swscale to W × Height, YUV420P, with W = even(source W × Height / source H).
  - Encoder: libx264 per the Global Constraints, with the framerate estimated from the first 8 video DTS deltas (default 25).
  - Keyframes: IDR forcing by PTS match.
- **Output video:**
  - PTS, DTS and Key come from the packet.
  - `Config` comes from the in-band SPS/PPS of the first keyframe, through the rung's ConfigCache.
  - `Data` is the NALs minus AUD, SPS and PPS.
- **Audio:**
  - Per track: decoder, then swresample to SampleRate × Channels FLTP, then an AudioFifo of 1024, then the `aac` encoder.
  - `Config` comes from the encoder's extradata ASC.
  - Output PTS and re-anchoring follow the Global Constraints.

- [ ] **Step 1: Write the tagged tests.** Fixtures go through the `ts` demuxer and a Clock and are shifted by D; there is a local helper in the test.
  - `TestPipelineLadder`, on `h264_aac.ts` with rungs 240/600, 180/300 and 120/150, and audio 96:
    - `CheckFrames` reports no violations per rung;
    - per rung, the keyframe PTS set equals the source keyframe PTS set + D;
    - dimensions are 320×240, 240×180 and 160×120;
    - video counts are equal to the source's;
    - the audio PTS step is 1920.
  - `TestPipelineResolutionChange`, on `h264_aac_then_640.ts`: epoch 2 on the 180 rung is 320×180, and its config changes only at that epoch's first frame.
  - `TestPipelineHEVCOpenGOP`, on `h265_aac51.ts`: no violations, and the audio is stereo 48 k.
  - `TestPipelineOpenGOPH264`, on `h264_opengop.ts`: an IDR at each source recovery-point PTS + D, from `Key` frames.
  - `TestPipelinePyramid`, on `h264_pyramid.ts`: DTS strictly increasing and PTS ≥ DTS.
  - `TestPipelineNoBFrames`, on `h264_aac_nob.ts` with B-frames 2: the minimum output DTS is ≥ 0, and there are no violations.
  - `TestPipelineVideoOnly`, on `h264_640.ts`: no audio output.
  - `TestPipelineAudioOnly`: audio is encoded, and `Discontinuity` resumes at the next frame.
  - `TestPipelineTwoAudio`: the audio frames are duplicated onto track 2, each track gets its own Config, and both tracks have PTS step 1920.
  - `TestPipelineAudioGap`: 10 audio frames removed mid-epoch. The first frame after the gap has PTS equal to its input PTS ± 1920, and `Stats().Reanchors == 1`. A second case shifts later input 3 frames earlier, which is backward drift: output PTS stays strictly increasing, `Stats().AudioDropped` is ≥ 3 × 1024, and there is no re-anchor. A third case jitters input PTS by ±90 ticks: `AudioDropped == 0` and `Reanchors == 0`.
  - `TestPipelineSecondVideoDropped`: `Stats().DroppedTracks == 1`.
  - `TestPipelineDiscontinuity`: output resumes at the next source key, as a keyframe.
- [ ] **Step 2:** Run `make test-transcode ARGS='-run TestPipeline ./internal/transcode/native/'`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, plus, on the host with `PKG_CONFIG_PATH=$(scripts/ffmpeg-dev.sh amd64)/lib/pkgconfig`, `go vet -tags libav ./internal/transcode/native/` and `golangci-lint run --build-tags libav ./internal/transcode/native/`. Expected: PASS.
- [ ] **Step 5:** Commit: `feat(transcode): decode once and encode a keyframe-aligned software ladder`.

---

### Task 5: Supervisor (`internal/transcode`)

**Files:**
- Create `transcode.go` (Transcoder, Options, Run, Stats, Event).
- Create `spawn_unix.go` (`//go:build unix`: socketpair, `ExtraFiles`, `Setpgid`; `Stop`, then SIGKILL to the group after 2 s).
- Create `spawn_other.go` (`!unix`: `errors.ErrUnsupported`).
- Create `epochs.go`, `merge.go` and `feed_test.go`. `feed_test.go` is a local helper that turns a fixture into frames and anchors through `ts`, a Clock and `hub.SetAnchor`.
- Create `transcodetest/fake.go` with `Main()`, a pure-Go fake child:
  - It sends Hello, then echoes every input video frame to every rung unchanged, and every input audio frame as rung −1.
  - It reads these env variables:
    - `OS_TRANSCODE_FAKE_CRASH_AFTER=N`: exit 2 after N frames.
    - `OS_TRANSCODE_FAKE_CRASH_ONCE=<file>`: crash only if the file is absent, then create it.
    - `OS_TRANSCODE_FAKE_DELAY=d`: sleep d per frame.
    - `OS_TRANSCODE_FAKE_VERSION=v`.
- Create `transcode_test.go`, `epochs_test.go` and `merge_test.go`. `TestMain` re-executes the test binary as the child when `OS_TRANSCODE_FAKE=1`.

**Interfaces:**
- Consumes: `wire` (T2), `hub`, `media`.
- Produces:
```go
package transcode
const Delay = 45000 // D, ruling R-M4c
type Video struct{ Copy bool; Height, BitrateKbps, MaxBitrateKbps, BFrames int; Preset string }
type Audio struct{ Copy bool; BitrateKbps, SampleRate, Channels int }
type Rendition struct{ Name string; Video Video }
type Options struct {
	Binary string; Args, Env []string
	Renditions []Rendition; Audio Audio
	Queue int // source subscription queue; 0 = 1024
	RespawnMin, RespawnMax, HealthyAfter, MergeHold time.Duration // 0 = 250 ms, 5 s, 30 s, 2 s
	Now func() time.Time; After func(time.Duration) <-chan time.Time
	Logger *slog.Logger; OnEvent func(Event)
}
type EventKind uint8 // EventSpawned=1, EventExited, EventEpoch, EventLoss
type Event struct{ Kind EventKind; PID int; Epoch uint32; Err error }
type Stats struct{ Spawns, Exits, Losses, DroppedBeforeKey, MergeDropped uint64; FramesIn uint64; FramesOut []uint64; MaxQueued int; PID int }
func New(src *hub.Hub, opt Options) (*Transcoder, error)
func (t *Transcoder) Hubs() []*hub.Hub
func (t *Transcoder) Run(ctx context.Context) error // until src.Done() or ctx; flushes the mergers and closes the rendition hubs
func (t *Transcoder) Stats() Stats
```

**Behaviour:**
- **Spawning:** a child is spawned only if some rendition encodes video, or audio is not Copy.
- **Handshake:** a `Hello` whose Version is not `wire.Version` is a fatal error. It is not retried, and `Run` returns it.
- **Child input:** one `src.Subscribe("transcode", Queue)` lasts the whole run.
  - Every delivery is mapped by `epochMap`, shifted by D, and sent to the child if it has an encode track, or straight to the mergers if it is a copy track.
  - The child's input channel holds 256 frames, and `MaxQueued` records its high-water mark.
  - `Lost > 0` means `Discontinuity` plus a bump.
- **Child output:** frames go by rung to the rendition mergers. Rung −1 audio goes to every rendition.
- **Child exit:**
  - Any exit not caused by ctx produces an `EventExited`, then a bump.
  - The child respawns after a backoff: `RespawnMin` doubling up to `RespawnMax`, reset after `HealthyAfter` of uptime.
  - After respawning, the child is fed from K.
- **Merger** (per rendition):
  - Expected tracks are those seen in the rendition's current epoch.
  - It writes to the hub in DTS order, but a frame waits only while another expected track has nothing pending, and only for at most `MergeHold`, measured as stream time (largest pending DTS minus the frame's DTS) or wall time, whichever ends first.
  - Frames whose epoch is below the hub's current epoch are dropped and counted in `MergeDropped`.
  - Before an epoch's first frame, the merger calls `SetAnchor` on the rendition hub.
  - `Run`'s end flushes the mergers.

- [ ] **Step 1: Write the tests.**
  - `TestEpochMap`:
    - upstream 1,1,2 map to 1,1,2;
    - after `bump()` takes effect at K, frames of upstream 2 map to 3;
    - upstream 3 maps to 4;
    - a second bump → 5, after which upstream 3 → 5 and upstream 4 → 6;
    - issued epochs are strictly increasing and never reused;
    - two bumps requested before K issue one epoch;
    - an upstream epoch that starts exactly at K maps to the bump's epoch, not to the next one.
  - `TestMergerOrdersByDTS`: interleaved inputs come out in non-decreasing DTS order, and the frame count is conserved.
  - `TestMergerHoldBound`: audio only, with the video track silent. Audio is released at most 2 s of stream time later, and at most 2 s of wall time (fake `After`).
  - `TestSupervisorLadderFake`: 2 encode rungs and 1 copy rendition, fed `h264_aac.ts`.
    - Every hub gets every frame shifted by D.
    - `CheckFrames` reports nothing.
    - The epochs equal the source's.
  - `TestSupervisorRespawnOneEpoch` (CRASH_ONCE after 50 frames):
    - each rendition gets exactly one extra epoch;
    - its first frame is a video keyframe;
    - the anchor is set before that frame;
    - `Stats.Spawns == 2`.
  - `TestSupervisorMixedRespawn`: copy video and child audio, crashing once, run on two feeds. One is the normal fixture. In the other, audio leads video in arrival by 300 ms, built with the reordering helper of T1's `TestAudioCutAtVideoPTS`, copied locally.
    - `CheckFrames` is clean, including track-start-offset ≤ 40 ms and epoch-starts-on-key.
    - `DroppedBeforeKey > 0`.
    - A third run on `h265_aac51.ts` (open GOP, CRA and RASL) has no `no-rasl-after-start` violation on the copy rendition.
  - `TestSupervisorCrashLoopBacksOff`: always crash, with fake `After`.
    - The respawn delays are exactly 250, 500, 1000, 2000, 4000, 5000 and 5000 ms.
    - After 7 delays, `Spawns == 8`.
  - `TestSupervisorMissingBinary`:
    - `Binary` set to `/nonexistent` gives an `EventExited` whose Err wraps `exec.ErrNotFound` or `fs.ErrNotExist`;
    - respawns back off;
    - the rendition hubs stay open until `ctx` is cancelled.
  - `TestSupervisorVersionMismatch`: `Run` returns an error containing "version", and `Spawns == 1`.
  - `TestSupervisorAudioOnly`: an audio-only source with one encode rung. Every rendition hub carries only audio, from the child.
  - `TestSupervisorSlowChildLoss`: a looped clip with `Queue = 64` and a 20 ms delay per frame, on the audio-leading feed.
    - `EventLoss` fires at least once, with one bump per loss.
    - `MaxQueued ≤ 256`.
    - `CheckFrames` is clean.
  - `TestSupervisorStopsChildOnCancel`: after `Run` returns, the child PID has been reaped (signal 0 gives ESRCH), and the goroutine count is back to its baseline.
- [ ] **Step 2:** Run `go test -race ./internal/transcode/...`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test -race -count=3 ./internal/transcode/...`, then vet and lint, then `GOOS=windows go vet ./internal/transcode/...` and `GOOS=darwin go vet ./internal/transcode/...`. Expected: PASS.
- [ ] **Step 5:** Commit: `feat(transcode): supervise the transcoder and map its epochs`.

---

### Task 6: ostranscoder main

**Files:** Modify `cmd/ostranscoder/main.go` (tag `libav`). Create `cmd/ostranscoder/main_test.go` (tag `libav`).

**Interfaces:**
- Consumes: `wire` (T2) and `native` (T4).
- Produces the child side of the protocol on fd 3:
  - It writes `Hello`, then reads `Configure` and builds the native pipeline.
  - For each `Frame` it calls `Push` and writes the outputs as `wire.Frame`.
  - On `Discontinuity` it calls `p.Discontinuity()` and writes the outputs.
  - On `Stop`, or on EOF from the host, it calls `Flush`, writes the outputs and exits 0.
  - It writes `Stats` every second.
  - On a pipeline error it writes `Error{Fatal: true}` and exits 1.
  - libav logs go to stderr at warning level.
  - `-version` is kept.

- [ ] **Step 1: Write the tests.**
  - `TestChildOverSocketpair`: in-process. A `socketpair` connects `run(conn)` on one end and the test, acting as host, on the other.
    - The test sends Configure (2 rungs plus audio) and the frames of `h264_aac.ts`.
    - Outputs per rung pass `CheckFrames`.
    - After `Stop`, `run` returns nil.
  - `TestChildBadConfigure`: zero rungs and no audio. The child writes `Error{Fatal: true}`, and `run` returns an error.
- [ ] **Step 2:** Run `make test-transcode ARGS='./cmd/ostranscoder/'`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command, plus vet and lint with the tag. Expected: PASS.
- [ ] **Step 5:** Commit: `feat(ostranscoder): serve the native pipeline over the wire protocol`.

---

### Task 7: osdev wiring, the real child, and the M4 scenarios

**Files:**
- Modify `cmd/osdev/main.go`. New flags:
  - `-ostranscoder PATH`;
  - `-rung name=label:height:kbps` (repeatable);
  - `-rung name=label:copy`;
  - `-audio name=copy|aac[:kbps]`.

  Each rendition hub is added to the hub lookup as `<name>_<label>`, with its own packager, and it starts after the source.
- Modify `internal/transcode/transcode_test.go`: add `TestSupervisorRealChild`.
- Modify `internal/e2e/m2a_test.go`: add `startFeed`, which returns the same `*m2aRun` handle as `startRun`, with `h`, `col` and `check`, but driven by a `feed` instead of inputs. Its `check` skips the pipeline-stats asserts (`DriftBreaks`, `DroppedDrift`), since a feed has no pipeline.
- Modify `internal/e2e/m3b3_test.go`:
  - `testSRTSteady` and `testHTTPTSSlowConsumer` take a `func(t *testing.T) *m2aRun` starter, and the existing callers pass `startRun` with their current options;
  - `pyramidRef` stays.
- Create `internal/e2e/m4a_test.go`. The arm64 guard: the `srt` and `rtsp` subtests call `t.Skip` when `OS_TRANSCODE_E2E_ARM=1`.

**Interfaces:**
- Consumes: `transcode.New` (T5), `testpeer.OSTranscoder` (T3), and the e2e helpers `pipelineFeed`, `stepsFeed`, `step`, `packFeed`, `serve`, `readAll`, `assertComplete`, `probeVideoTimes`, `serveRTSP` and `servePlay`.
- Produces, for M4b:
```go
// transcodeFeed runs one transcoder over src, records every rendition hub's
// anchors and frames as steps, and returns one replayable feed per rendition.
func transcodeFeed(t *testing.T, src feed, opt transcode.Options) []feed

// pacedFeed replays steps at wall-clock pace: each frame is written at its
// epoch anchor's Wall + (DTS − Start), and a loop clip is repeated n times
// with the epochs and anchors of each pass shifted, as loopClip does for
// inputs. Recorded anchor Walls are rebased to the replay start, and each
// pass is shifted by Δ = the largest per-track (last PTS + duration) − the
// first DTS. A clip with one epoch and one config stays in that epoch on
// every pass. Otherwise pass k shifts epoch numbers by k × (epochs per pass)
// and adds anchors shifted by k × Δ.
func pacedFeed(steps []step, n int) feed
```
- The slow-consumer and SRT rows use `startFeed(pacedFeed(rendition steps, 3))`. The reader queues stay unchanged: `bigQueue`, and the slow consumer's deliberate 32. Only the transcoder's `Options.Queue` is set to 2048, so the transcoder itself does not hit loss.
- `TestM4aRespawn` feeds the transcoder from a paced 2-pass loop of `h264_aac_then_640.ts`, which carries the upstream epoch change.

**The ladder** used by every test is `t240:240:600`, `t180:180:300`, `t120:120:150` and `src:copy`, with audio `aac:96`. Each test starts with `testpeer.OSTranscoder(t)`.

- [ ] **Step 1: Write the tests.**
  - `TestSupervisorRealChild`: 3 rungs plus audio on `h264_aac.ts`. Every hub passes `CheckFrames`, and the keyframe PTS sets are equal across hubs.
  - `TestM4aMatrix/<rendition>/<scenario>`, for each of the 4 renditions:
    - `steady-state`: `readAll` and `assertComplete`.
    - `epoch-change`: `h264_aac_then_640.ts`.
    - `slow-consumer`: `testHTTPTSSlowConsumer` with the rendition feed.
    - `no-bframes`: `h264_aac_nob.ts`, with every reader free of violations.
    - `b-frames`:
      - for `src`, `probeVideoTimes` equals `pyramidRef` shifted by D;
      - for the encoded rungs:
        - the ffprobe PTS set equals the source's PTS set + D;
        - DTS strictly increases;
        - at least one packet has PTS > DTS.
  - Also `t180/srt-steady` (`testSRTSteady` with the feed) and `t180/rtsp-steady` (`serveRTSP` then `ReadRTSP`, with no violations).
  - `TestM4aKeyframesAligned`: the keyframe PTS sets of the four hubs are equal, and so are the boundaries of their HLS-TS segments.
  - `TestM4aRespawn`: SIGKILL the child at `Stats().PID` mid-stream.
    - Each rendition hub gets exactly one new epoch within 5 s.
    - `ReadHLS` of HLS fMP4 shows exactly one extra discontinuity per rendition, and no violation.
    - The feed's later upstream epoch change maps above the respawn epoch.
  - `TestM4aVLC`: `t180` HLS-TS plays in VLC, with the M3 pass rule and no tolerance.
- [ ] **Step 2:** Run `make ostranscoder && OS_TRANSCODER=$PWD/dist/ostranscoder-amd64/ostranscoder go test -race -run 'TestM4a|TestSupervisorRealChild' ./internal/e2e/ ./internal/transcode/`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4: Run** each of these. Expected: PASS everywhere.
  - The same command, 3 times.
  - `OS_TRANSCODER=$PWD/dist/ostranscoder-amd64/ostranscoder make test-ci-peers ARGS='-race -count=1 -run TestM4a ./internal/e2e/'`.
  - `make test`.
- [ ] **Step 5:** Commit: `test(e2e): prove the software ladder under the M3 scenarios`.

---

### Task 8: arm64 proof on CI

**Files:** Modify `.github/workflows/ci.yml`. Add a job `transcode-e2e-arm64` (`ubuntu-24.04-arm`, `needs: transcoder-arm64`). It:
- installs apt `ffmpeg` (for ffprobe);
- untars the arm64 bundle;
- runs `go test -race -run 'TestM4aMatrix|TestM4aKeyframesAligned|TestM4aRespawn|TestSupervisorRealChild' ./internal/e2e/ ./internal/transcode/`, with `OS_TRANSCODER` set and `OS_REQUIRE_PEERS=1`.

The VLC, SRT and RTSP peer rows stay amd64-only: `TestM4aVLC` is not in the arm64 pattern, and the srt and rtsp subtests skip there through T7's `OS_TRANSCODE_E2E_ARM=1` guard, which this job sets.

- [ ] **Step 1:** Add the job. Validate the YAML with python3 `yaml`.
- [ ] **Step 2:** Check that `TestM4aMatrix` makes no wall-clock assertion tighter than 5 s. Run it 3 times locally with `GOMAXPROCS=2` as a proxy for the slower arm runner. Expected: PASS.
- [ ] **Step 3:** Commit: `ci: prove the transcoder on linux/arm64`.

---

### Task 9: Spec update — M4a done

**Files:** Modify `docs/REBUILD_PLAN.md`

- [ ] **Step 1: Update the spec.**
  - **§5:** split the M4 row into M4a (done, with this plan's link, its exit criteria and an "Out of M4a" list), M4b (ABR outputs) and M4c (NVENC on the GPU host, watermark, thumbnails).
  - **§2:** add a "Platforms" row, per R-M4d.
  - **§3.5:** add the wire protocol (an envelope, with each config sent once), IDR forcing at source keyframes, the rendition epoch rule with its bump-at-K point, the delay D (R-M4c), the DTS merger, passthrough without a child, and the FFmpeg-Builds pin (R-M4b).
  - **§3.7:** go-astiav v0.41.0, protobuf through `protowire` only, and FFmpeg 8.1.3 from FFmpeg-Builds.
  - **§4:** add `internal/transcode/wire`, `internal/transcode/native`, `internal/transcode/transcodetest` and `scripts/`.
  - **Backlog:** add the final review's deferred minors.
  - **Carried from M1:** remove "M4, transcoder: rendition epoch numbers never collide".
- [ ] **Step 2: Run** these. Expected: clean.
  - The full suite, vet, gofumpt and lint.
  - `make cover covercheck`.
  - `make test-transcode`.
  - `make test-ci-peers` with `OS_TRANSCODER`.
- [ ] **Step 3:** Commit: `docs: mark M4a done`.
