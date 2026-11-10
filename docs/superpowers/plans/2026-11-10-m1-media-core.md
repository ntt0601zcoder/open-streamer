# M1 Media Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build v2's media core: the `Frame` type, the only MPEG-TS demuxer, the
`Clock`, the GOP-aware hub, frame-level conformance checks and the fixtures
that prove them, on a fresh `rebuild` branch with v1's code removed.

**Architecture:** Pure, caller-driven packages with no goroutines: `ts` turns
bytes into source-time frames, `clock` turns source time into stream time with
epochs, `hub` fans frames out without blocking, `conform` checks frame
sequences. An end-to-end test runs every fixture through all four.

**Tech Stack:** Go (version from `go.mod`, currently `go 1.26.10`),
`asticode/go-astits` v1.15.0, `Eyevinn/mp4ff` v0.52.0, `stretchr/testify`
v1.11.1, ffmpeg for generating fixtures only. Tools: `golangci-lint` v2.11.3,
`go-test-coverage` v2.22.0.

**Spec:** [docs/REBUILD_PLAN.md](../../REBUILD_PLAN.md), sections 3.1 to 3.3,
4 and the M1 row of §5.

## Global Constraints

- Module path stays `github.com/ntt0601zcoder/open-streamer`; the `go` directive stays as in `go.mod`.
- No cgo anywhere in M1. `go build ./...`, `go vet ./...` and `go test ./...` work on a plain machine.
- Module dependencies allowed in M1: the Go standard library, `asticode/go-astits`, `Eyevinn/mp4ff`, `stretchr/testify`. Tools run with `go run <tool>@<version>` or `go install`, never added to `go.mod`.
- Only independent libraries: never `bluenviron/*` (mediacommon, gortsplib, gohlslib) or `q191201771/lal`, and do not copy code from them. What no independent library provides goes into our own package under `pkg/`, which never imports `internal/`.
- Timestamps on `media.Frame` are `int64` in 90 kHz ticks (`media.ClockRate = 90000`). No `time.Duration` and no milliseconds on the media path.
- `media`, `ts`, `clock`, `conform` and `hub` start no goroutines.
- `hub.Write` never blocks, under any subscriber behaviour.
- Errors are wrapped as `fmt.Errorf("<pkg>: <operation>: %w", err)`. Never log and return the same error.
- Lint rules: gofumpt formatting, `godot` (top-level comments end with a period), `exhaustive` with `default-signifies-exhaustive: false`.
- Audio frames always have `Key == true` and `RASL == false`.
- Video `Frame.Data` never contains VPS, SPS, PPS or AUD NAL units; they live only in `TrackConfig`.
- Fixture files are opened from tests as `../../testdata/fixtures/<name>` (or `../../../` from deeper packages).

## Review Focus

Each line below has a test in the owning task.

1. **A source joined mid-GOP** (UDP, SRT, HTTP-TS). Expected: no video before the first keyframe, no audio presented before it, and a start offset under 40 ms. Tests: `TestDemuxMidGOP` (Task 6), `TestClockMidGOPStart` (Task 7), `TestM1Fixtures` on `h264_aac_midgop.ts` (Task 9).
2. **H.265 open GOP.** Expected: RASL frames after a CRA that starts an epoch or a subscriber are dropped. Tests: `TestDemuxH265RASL`, `TestClockDropsRASLAtEpochStart`, `TestHubResumeDropsRASL`.
3. **One track goes quiet for more than 5 s while the other continues.** Expected: a gap on that track, no new epoch, nothing dropped. Test: `TestClockSingleTrackGap`.
4. **Failover from a B-frame source to one without B-frames.** Expected: the new epoch's first PTS is after the old epoch's last presentation end. Test: `TestClockEpochStartAfterPresentationEnd`.
5. **Unsubscribe during writes.** Expected: no panic and no send on a closed channel. Test: `TestHubConcurrentChurn` under `-race`.

---

### Task 1: `rebuild` branch and skeleton

**Files:**
- Delete: every tracked file except the keep list below
- Keep: `LICENSE`, `SECURITY.md`, `.gitignore`, `.golangci.yml`, `docs/REBUILD_PLAN.md`, `docs/SOAK_TEST_PLAN.md`, `docs/bug-review/`, `docs/superpowers/`, `bench/soak/`
- Rewrite: `README.md`, `CLAUDE.md`, `Makefile`, `.testcoverage.yml`, `.github/workflows/ci.yml`, `go.mod`, `go.sum`

**Interfaces:**
- Produces: an empty module that later tasks add packages to; make targets `test`, `lint`, `fmt`, `tidy`, `vet`, `check`, `cover`, `covercheck`, `fixtures`, `tools`.

- [ ] **Step 1: Commit the planning docs on the current branch.** On `docs/soak-test-plan`, commit `docs/REBUILD_PLAN.md`, `docs/bug-review/` and `docs/superpowers/` with message `docs: add the v2 rebuild plan and the v1 bug review`.

- [ ] **Step 2: Create the branch.** `git switch -c rebuild`

- [ ] **Step 3: Remove v1.** `git rm -r` everything not on the keep list. That includes `.github/workflows/builder-image.yml` and `release.yml`, the Dockerfiles, `api/`, `build/`, `config/`, `examples/`, `scripts/` and `.env.example`. M4 and M7 bring back what they need from history.

- [ ] **Step 4: Write the new top-level files.**
  - `README.md`: three lines saying this is the v2 rebuild, linking `docs/REBUILD_PLAN.md`.
  - `CLAUDE.md`: the commands below; a pointer to `docs/REBUILD_PLAN.md`; the invariants from Global Constraints; the rule that v1 is reference only, read with `git show main:<path>`.
  - `Makefile`:
    - `test`: `go test -race -shuffle=on -count=1 -timeout=5m ./...`
    - `lint`: `golangci-lint run ./...`
    - `tools`: `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.3`
    - `fmt`: gofumpt if installed, else gofmt
    - `tidy`, `vet`
    - `check`: tidy, vet, lint, test
    - `cover`: writes `coverage.out`
    - `covercheck`: `go run github.com/vladopajic/go-test-coverage/v2@v2.22.0 -c .testcoverage.yml`
    - `fixtures`: `sh testdata/fixtures/gen.sh`
  - `.testcoverage.yml`: thresholds file 80, package 85, total 90. One exclusion, `internal/e2e`, with the comment "test-only package; it has no production code".
  - `.github/workflows/ci.yml`: on `pull_request` into `main` and `rebuild`, all jobs on `ubuntu-latest` without the builder image, all blocking:
    - a `go mod tidy` drift check
    - `go vet ./...`
    - `make test`
    - `make cover covercheck`
    - `govulncheck ./...` pinned to `golang.org/x/vuln/cmd/govulncheck@v1.1.4`
    - `golangci-lint` v2.11.3

- [ ] **Step 5: Verify.**

Run: `git ls-files | grep -v -e '^docs/' -e '^bench/soak/'`
Expected: only the kept and rewritten top-level files.

Run: `go mod tidy && git diff --stat go.mod`
Expected: tidy succeeds and `go.mod` has no `require` block. Don't run `go vet ./...`, `go test ./...` or `govulncheck` here: on a module with no packages they exit non-zero.

- [ ] **Step 6: Commit.** Message: `chore: start v2 on a clean tree`.

### Task 2: Fixtures

**Files:**
- Create: `testdata/fixtures/gen.sh`, `testdata/fixtures/README.md`, and the generated `.ts` files below

**Interfaces:**
- Produces: the fixture files used by Tasks 3, 5, 6 and 9.

- [ ] **Step 1: Write `gen.sh`** with `set -eu`, `cd` to the script's directory, and exactly these commands:

```sh
F="ffmpeg -nostdin -v error -y"
BX="-threads 1 -fflags +bitexact -flags:v +bitexact -flags:a +bitexact -f mpegts"
X264="-c:v libx264 -profile:v main -bf 2 -g 25 -keyint_min 25 -sc_threshold 0"
X265="-c:v libx265 -x265-params keyint=25:min-keyint=25:bframes=2:scenecut=0:pools=1:frame-threads=1:log-level=error"
v() { echo "-f lavfi -i testsrc2=size=$1:rate=25"; }
a() { echo "-f lavfi -i sine=frequency=1000:sample_rate=$1"; }

$F $(v 320x240) $(a 48000) -t 6 $X264 -c:a aac -ac 2 -b:a 64k  $BX h264_aac.ts
$F $(v 320x240) $(a 44100) -t 6 $X265 -c:a aac -ac 6 -b:a 128k $BX h265_aac51.ts
$F $(v 320x240) $(a 44100) -t 6 $X264 -c:a aac -ac 1 -b:a 64k -output_ts_offset 95440 $BX h264_aacmono_wrap.ts
$F $(v 640x360) -t 2 $X264 -an $BX h264_640.ts
$F $(v 320x240) $(a 48000) -t 2 $X264 -c:a mp2 $BX h264_mp2.ts
cat h264_aac.ts h264_aac.ts > h264_aac_loop.ts
cat h264_aac.ts h264_640.ts > h264_aac_then_640.ts
N=$(( $(stat -c%s h264_aac.ts) / 188 ))
tail -c +$(( 188 * (N * 3 / 8) + 1 )) h264_aac.ts > h264_aac_midgop.ts
```

- [ ] **Step 2: Run it and check the result.**

Run: `make fixtures && ls -l testdata/fixtures/*.ts && ffprobe -v error -show_entries stream=codec_name,width,height,sample_rate,channels -of csv=p=0 testdata/fixtures/h265_aac51.ts`
Expected: eight files, about 2.7 MB in total, none over 700 KB; `hevc,320,240` and `aac,44100,6`.

Run: `make fixtures && git status --short testdata/`
Expected: a second run changes no file (output is bitexact).

- [ ] **Step 3: Write `README.md`.** One table row per file: what it contains and what it exercises. That covers B-frames and closed GOP for x264; open GOP for x265 (CRA plus RASL pictures); PTS wrap about 2.4 s in; one backwards jump; one resolution change plus a jump; an unsupported codec; and a mid-GOP start. Include the regenerate command.

- [ ] **Step 4: Commit.** Message: `test: add generated media fixtures`.

### Task 3: `media` package

**Files:**
- Create: `internal/media/codec.go`, `config.go`, `frame.go`, `timebase.go`
- Test: `internal/media/codec_test.go`, `config_test.go`, `timebase_test.go`

**Interfaces:**
- Produces:

```go
const ClockRate = 90000

type Kind uint8   // KindVideo = 1, KindAudio = 2
type Codec uint8  // CodecH264 = 1, CodecH265 = 2, CodecAAC = 3
func (c Codec) Kind() Kind
func (c Codec) String() string // "h264", "h265", "aac"

type TrackConfig struct {
    Codec                Codec
    Width, Height        int      // from the SPS with the lowest ID
    VPS, SPS, PPS        [][]byte // video: every set in use, sorted by ID, one per ID
    SampleRate, Channels int      // audio
    ASC                  []byte   // AAC AudioSpecificConfig
}
func NewH264Config(sps, pps [][]byte) (*TrackConfig, error)
func NewH265Config(vps, sps, pps [][]byte) (*TrackConfig, error)
func NewAACConfig(asc []byte) (*TrackConfig, error)
func (c *TrackConfig) Equal(o *TrackConfig) bool
func ParamSetID(codec Codec, nalu []byte) (uint32, error) // ID of a VPS, SPS or PPS NAL unit

type Frame struct {
    Track    int
    Config   *TrackConfig
    PTS, DTS int64
    Key      bool
    RASL     bool
    Data     [][]byte
    Epoch    uint32
}
func (f Frame) Kind() Kind

func Rescale(ts int64, rate int) int64               // 90 kHz → rate, rounded half away from zero
func SampleTime(samples int64, sampleRate int) int64 // samples → 90 kHz, rounded half away from zero
func ToDuration(ts int64) time.Duration
func FromDuration(d time.Duration) int64             // rounded half away from zero
```

- [ ] **Step 1: Extract the test vectors.** Dump the first access unit with `ffmpeg -v error -i testdata/fixtures/h264_aac.ts -map 0:v -c copy -frames:v 1 -f h264 <scratch>/au.264` (and `-f hevc` for `h265_aac51.ts`). Split it on start codes, take the SPS and PPS (plus the VPS for H.265), and paste them into the tests as hex constants.

- [ ] **Step 2: Write the failing tests.**
  - `TestCodecKind`: H264 and H265 are `KindVideo`, AAC is `KindAudio`; the `String()` values are as above.
  - `TestNewAACConfig`:
    - `{0x11, 0x90}` gives 48000 Hz, 2 channels; `{0x12, 0x10}` gives 44100 Hz, 2 channels; `{0x12, 0x08}` gives 44100 Hz, 1 channel.
    - `ASC` is a copy of the input.
    - `{0xff}` returns an error containing `media: aac config`.
  - `TestNewH264Config`:
    - The extracted SPS and PPS give 320x240.
    - An empty SPS list returns an error containing `media: h264 config`.
    - Two PPS with IDs 1 and 0 come out sorted as 0, 1.
    - Two PPS with the same ID return an error.
  - `TestNewH265Config`: the same checks for the H.265 vectors, 320x240.
  - `TestParamSetID`: the extracted H.264 SPS and PPS have ID 0. A PPS whose first `ue(v)` encodes 3 (RBSP bits `00100`) has ID 3. The H.265 VPS has ID 0, read from the 4 bits after the 2-byte header.
  - `TestTrackConfigEqual`: equal when the codec and every parameter set match; not equal when one PPS byte differs; `nil` equals only `nil`.
  - `TestRescale`: `Rescale(90000, 48000) == 48000`, `Rescale(1, 48000) == 1`, `Rescale(-90000, 48000) == -48000`.
  - `TestSampleTime`: `SampleTime(1024, 48000) == 1920`, `SampleTime(1024, 44100) == 2090`, `SampleTime(1024*1000, 44100) == 2089796`, `SampleTime(44100, 44100) == 90000`.
  - `TestDurationConversions`: `ToDuration(90000) == time.Second`, `FromDuration(time.Second/3) == 30000`, `FromDuration(-time.Second/3) == -30000`.

- [ ] **Step 3: Run them to see them fail.** `go test ./internal/media/`: FAIL with undefined identifiers.

- [ ] **Step 4: Implement.**
  - First, `go get github.com/Eyevinn/mp4ff@v0.52.0 github.com/stretchr/testify@v1.11.1`.
  - **H.264 SPS:** dimensions and ID from mp4ff `avc.ParseSPSNALUnit(nalu, false)` (`ParameterID`, `Width`, `Height`).
  - **H.265:** VPS ID from mp4ff `hevc.ParseVPSNALUnit` (`VpsID`); SPS from `hevc.ParseSPSNALUnit` (`SpsID`, `ImageSize()`).
  - **PPS IDs** for both codecs are the first `ue(v)` after the NAL header (1 byte for H.264, 2 for H.265). Read them with mp4ff's `bits.NewEBSPReader`, which removes emulation prevention, because mp4ff's PPS parsers need an SPS map.
  - **AAC:** parse the ASC with mp4ff `aac.DecodeAudioSpecificConfig`. Channel configuration 1 to 6 means that many channels, 7 means 8; anything else returns an error.
  - Constructors copy every byte slice.

- [ ] **Step 5: Run the tests.** `go test -race ./internal/media/`: PASS.

- [ ] **Step 6: Commit.** Message: `feat(media): add Frame, TrackConfig and timebase helpers`.

### Task 4: Frame conformance checks

**Files:**
- Create: `internal/conform/frames.go`
- Test: `internal/conform/frames_test.go`

**Interfaces:**
- Consumes: `media.Frame`, `media.ClockRate`.
- Produces:

```go
const (
    RuleConfig      = "config-present"
    RuleDTS         = "dts-monotonic"       // strictly increasing per track, across epochs too
    RulePTS         = "pts-ge-dts"
    RuleEpochOrder  = "epoch-monotonic"     // per track, never decreases
    RuleEpochKey    = "epoch-starts-on-key" // first video frame of each epoch per track
    RuleRASL        = "no-rasl-after-start" // no RASL frame right after an epoch's first keyframe
    RuleStartOffset = "track-start-offset"  // per epoch: each track's first PTS vs the earliest first PTS
    RuleOverlap     = "epoch-no-overlap"    // an epoch's smallest PTS is above the previous epoch's largest PTS
)
type Violation struct { Rule string; Track, Index int; Detail string }
func (v Violation) String() string
type FrameOptions struct { MaxStartOffset int64 } // 0 means 3600 (40 ms)
func CheckFrames(frames []media.Frame, opt FrameOptions) []Violation
```

- [ ] **Step 1: Write the failing tests.** Each uses a small synthetic sequence and asserts that exactly one violation, with the named rule, track and index, is returned.
  - `TestCheckFramesClean`: a valid two-track, two-epoch sequence with B-frame-style DTS < PTS returns `nil`.
  - `TestCheckFramesMissingConfig`, `TestCheckFramesDTSRepeat`, `TestCheckFramesDTSBackwards`, `TestCheckFramesPTSBeforeDTS`, `TestCheckFramesEpochDecrease`, `TestCheckFramesEpochStartsOnNonKey`.
  - `TestCheckFramesRASLAfterStart`: an epoch that starts on a key followed by a RASL frame is reported.
  - `TestCheckFramesStartOffset`: video first PTS 7200 with first DTS 0. Audio first PTS 10801 is reported; audio first PTS 10800 is not.
  - `TestCheckFramesEpochOverlap`: epoch 2's smallest PTS equal to epoch 1's largest PTS is reported.

- [ ] **Step 2: Run them to see them fail.** `go test ./internal/conform/`: FAIL.

- [ ] **Step 3: Implement `CheckFrames`.** One pass with per-track and per-epoch state. Violations come out in input order, and `Detail` names the two values compared.

- [ ] **Step 4: Run the tests.** `go test -race ./internal/conform/`: PASS.

- [ ] **Step 5: Commit.** Message: `feat(conform): add frame-level property checks`.

### Task 5: `pkg/mpegts` library

Our own library for what go-astits does not do. It imports nothing from
`internal/` and has its own package documentation.

**Files:**
- Create: `pkg/mpegts/doc.go`, `pkg/mpegts/timedecoder.go`, `pkg/mpegts/adts.go`
- Test: `pkg/mpegts/timedecoder_test.go`, `pkg/mpegts/adts_test.go`

**Interfaces:**
- Produces:

```go
// TimeDecoder unwraps 33-bit MPEG-TS timestamps into a continuous int64 timeline that starts at 0.
type TimeDecoder struct { /* unexported */ }
func (d *TimeDecoder) Decode(ts int64) int64

// ADTS is one parsed ADTS frame.
type ADTS struct { ObjectType, SampleRateIndex, ChannelConfig uint8; SampleRate int; Payload []byte }
func ParseADTS(buf []byte) ([]ADTS, error) // every ADTS frame in a PES payload
func (a ADTS) AudioSpecificConfig() []byte  // the 2-byte ASC
```

Behaviour the tests pin:

- **`Decode`.**
  - The first value returns 0.
  - Each later value returns the previous output plus the signed difference to the previous input, taken modulo 2^33 into the range [−2^32, 2^32).
  - It is shared across tracks and fed both PTS and DTS, so negative differences (B-frames, audio slightly behind video) must work.
- **`ParseADTS`.**
  - Handles 7- and 9-byte headers (CRC) and several frames in one buffer.
  - A truncated final frame returns the frames before it and an error wrapping `mpegts: adts:`.
  - It uses mp4ff `aac.DecodeADTSHeader` for the header fields.

- [ ] **Step 1: Write the failing tests.**
  - `TestTimeDecoderStartsAtZero`: `Decode(5000) == 0`, then `Decode(8600) == 3600`.
  - `TestTimeDecoderWrap`: `Decode(1<<33 - 1800)` then `Decode(1800)` returns 3600.
  - `TestTimeDecoderNegativeDiff`: `Decode(10000)`, `Decode(6400)`, `Decode(13600)` return 0, −3600, 3600.
  - `TestTimeDecoderNegativeAcrossWrap`: `Decode(1800)` then `Decode(1<<33 - 1800)` returns −3600.
  - `TestParseADTS`: the first audio PES payload of `h264_aac.ts` gives frames whose ASC is `{0x11, 0x90}`, with `SampleRate == 48000` and payloads without the header. The first audio PES payload of `h265_aac51.ts` gives an ASC with channel configuration 6.
  - `TestParseADTSTruncated`: a payload cut in the middle of its last frame returns the earlier frames and an error.

- [ ] **Step 2: Run them to see them fail.** `go test ./pkg/mpegts/`: FAIL.

- [ ] **Step 3: Implement.** First, `go get github.com/asticode/go-astits@v1.15.0`. To take PES payloads from the fixtures in the tests, use go-astits `NewDemuxer(ctx, r).NextData()`.

- [ ] **Step 4: Run the tests.** `go test -race ./pkg/mpegts/`: PASS.

- [ ] **Step 5: Commit.** Message: `feat(mpegts): add timestamp unwrapping and ADTS parsing`.

### Task 6: TS demuxer

**Files:**
- Create: `internal/ts/demux.go`
- Test: `internal/ts/demux_test.go`

**Interfaces:**
- Consumes: Task 3 constructors, `media.ParamSetID`, `media.SampleTime`; Task 5 `mpegts.TimeDecoder`, `mpegts.ParseADTS`; the Task 2 fixtures.
- Produces:

```go
type Demuxer struct { /* unexported */ }
func NewDemuxer(r io.Reader) (*Demuxer, error)
func (d *Demuxer) NumTracks() int
func (d *Demuxer) TrackKinds() []media.Kind    // by track index, known from the PMT
func (d *Demuxer) Skipped() []string           // e.g. "pid 257: mpeg audio (0x03)"
func (d *Demuxer) Read() (media.Frame, error)  // io.EOF at the end
func (d *Demuxer) Stats() DemuxStats
type DemuxStats struct { DroppedNoConfig, DecodeErrors, PTSOnly uint64 }
```

Behaviour the tests pin:

- **Tracks.** Tracks are the PMT entries with stream type 0x1B (H.264), 0x24 (H.265) or 0x0F (AAC ADTS), numbered from 0 in PMT order.
  - Others go to `Skipped()` as `pid <n>: <name> (0x<type>)`, where 0x03 and 0x04 are named "mpeg audio" and any other type "unsupported".
  - `NewDemuxer` reads with go-astits `NextData` until the first PMT. With no supported track it returns an error containing `ts: demux: no supported track`, and the same if no PMT arrives before EOF.
- **Timestamps.** One `mpegts.TimeDecoder` is shared by all tracks for PTS and DTS, fed the 90 kHz `Base` of go-astits' clock references. Its output starts at 0 and never wraps. `Epoch` is 0.
- **PTS-only video.** A video PES without DTS gets `DTS = PTS` and increments `Stats().PTSOnly`. Correct DTS for such sources comes in M2 from `pkg/h26x`.
- **Access units.** PES payloads are split into NAL units with mp4ff `avc.ExtractNalusFromByteStream`; the split is the same for H.265.
- **Parameter sets.** VPS, SPS, PPS and AUD are removed from `Data` and feed `Config`, keyed by `ParamSetID`. A new `*TrackConfig` is made only when a set with new bytes arrives. Frames before the first complete set (SPS and PPS, plus VPS for H.265) are dropped and counted.
- **Keyframes.** `Key` is true when the access unit holds an H.264 IDR slice (type 5), or an H.265 BLA, IDR or CRA slice (types 16 to 21). `RASL` is true for H.265 types 8 and 9.
- **AAC.** Frames come from `mpegts.ParseADTS`. The config comes from the ADTS header and is rebuilt only when it changes. Frame *i* of a PES gets `PTS = DTS = pesPTS + SampleTime(i*1024, sampleRate)`.

- [ ] **Step 1: Write the failing tests.**
  - `TestDemuxH264AAC`, on `h264_aac.ts`:
    - 2 tracks, `TrackKinds() == [video, audio]`.
    - 150 video frames and 283 audio frames.
    - The first video frame is `Key`.
    - Video Config is 320x240; audio Config is 48000 Hz, 2 channels.
    - No video NAL unit of type 7, 8 or 9 appears in any `Data`.
    - Every audio DTS delta is 1920.
    - `conform.CheckFrames(frames, conform.FrameOptions{})` returns `nil`.
  - `TestDemuxH265AAC51`, on `h265_aac51.ts`:
    - The codec is H265 with one VPS.
    - Audio is 44100 Hz, 6 channels, 260 frames.
    - Every audio DTS delta is within 1 of `SampleTime(1024, 44100)`.
  - `TestDemuxH265RASL`: at least one frame has `RASL == true`, and every RASL frame follows a CRA keyframe in decode order with no intervening keyframe.
  - `TestDemuxWrap`, on `h264_aacmono_wrap.ts`:
    - DTS strictly increases per track.
    - The span from first to last video DTS is between 5.8 s and 6 s in ticks.
    - Separately, with go-astits directly in the test, the first video PES PTS `Base` is above `1<<33 - 10*90000`, which proves the fixture wraps.
  - `TestDemuxLoop`: on `h264_aac_loop.ts`, each track has exactly one backwards DTS step.
  - `TestDemuxConfigChange`: on `h264_aac_then_640.ts`, the video track's `Config` pointer changes exactly once, from 320x240 to 640x360.
  - `TestDemuxMidGOP`: on `h264_aac_midgop.ts`, the first video frame returned has `Key == false`. The demuxer passes it through; dropping is the Clock's job.
  - `TestDemuxSkipsUnsupported`: on `h264_mp2.ts`, `NumTracks() == 1` and `Skipped()` has one entry containing `mpeg audio`.
  - `TestDemuxNoPTSOnly`: on `h264_aac.ts`, `Stats().PTSOnly == 0`.
  - `TestDemuxTruncated`: the first 100,077 bytes of `h264_aac.ts` give at least 40 video frames and 70 audio frames, then `io.EOF`. No frame has empty `Data`, and `CheckFrames` returns `nil`.
  - `TestDemuxGarbage`: 64 KiB from `math/rand` with seed 1 makes `NewDemuxer` return an error, with no panic.

- [ ] **Step 2: Run them to see them fail.** `go test ./internal/ts/`: FAIL.

- [ ] **Step 3: Implement** on go-astits `Demuxer.NextData`. Each PES becomes one video frame or several audio frames, queued; `Read` drains the queue and calls `NextData` when it is empty. go-astits' "no more packets" error becomes `io.EOF`, and other per-packet errors are counted in `DecodeErrors` and skipped.

- [ ] **Step 4: Run the tests.** `go test -race ./internal/ts/`: PASS. If a frame count differs from this plan, check the fixture with `ffprobe -count_packets` before changing the assertion, and record the reason in the commit.

- [ ] **Step 5: Commit.** Message: `feat(ts): add the MPEG-TS demuxer`.

### Task 7: Clock

**Files:**
- Create: `internal/clock/clock.go`
- Test: `internal/clock/clock_test.go`

**Interfaces:**
- Consumes: `media.Frame`, `media.Kind`, `media.ClockRate`.
- Produces:

```go
type Options struct {
    Kinds       []media.Kind     // kind of each expected track, by index (from TrackKinds)
    AlignWindow int64            // source ticks held at an epoch start; 0 means 90000
    MaxForward  int64            // forward jump past the high-water mark that opens an epoch; 0 means 5*90000
    MaxBackStep int64            // backwards distance below which a frame is dropped, not an epoch; 0 means 45000
    Now         func() time.Time // nil means time.Now
}
type Epoch struct { ID uint32; Start int64; Wall time.Time }
type Stats struct { DroppedBackStep, DroppedBeforeKey, DroppedRASL, DroppedEarlyAudio, DroppedStale, DroppedLate uint64 }
func New(opt Options) *Clock
func (c *Clock) Push(f media.Frame) []media.Frame // source time in, stream time out, in arrival order
func (c *Clock) Break()                           // the next frames open a new epoch
func (c *Clock) Flush() []media.Frame             // release held frames
func (c *Clock) Epochs() []Epoch                  // the last 64 anchors
func (c *Clock) Stats() Stats
```

Algorithm. The tests pin each rule.

1. **Numbering.** Epochs number from 1. Epoch 1 starts at stream time 0.
2. **Jumps.** `hw` is the largest source DTS accepted in the current epoch, across all tracks. A frame opens a new epoch when `DTS > hw + MaxForward` or `DTS < hw − MaxBackStep`, unless rule 3 classifies it as continuing the old epoch. One track's own gap never opens an epoch while `hw` keeps advancing.
3. **Old or new timeline.** While a new epoch is pending, a frame of a track that has not joined it continues the old epoch if `−MaxBackStep < DTS − lastSourceDTS(track) ≤ MaxForward`. It is then mapped with the old epoch and emitted at once. Otherwise it joins the pending epoch. After the pending epoch is released, old-timeline frames are dropped (`DroppedStale`).
4. **Small steps back.** A frame continuing its epoch with `−MaxBackStep < DTS − lastSourceDTS(track) ≤ 0` is dropped (`DroppedBackStep`).
5. **Gating a pending epoch,** before anything is held:
   - Video frames of a track are dropped until that track's first keyframe (`DroppedBeforeKey`).
   - RASL frames that directly follow that keyframe are dropped (`DroppedRASL`).
   - At release, held audio whose PTS is below the smallest first-keyframe PTS of the video tracks is dropped (`DroppedEarlyAudio`).
6. **Release.** A pending epoch is released when each expected track has a gated frame, or when the held source DTS span reaches `AlignWindow`. Then:
   - `base` is the smallest held DTS.
   - `Start` is computed at release: 0 for epoch 1, otherwise the largest `lastStreamPTS + lastDuration` over all tracks. `lastDuration` is the track's last DTS delta, or 3600 if unknown.
   - `Wall` is the arrival time (`Now()`) of the first frame held for the epoch.
   - The held frames are released in arrival order.
7. **Mapping.** Inside an epoch, `stream = source − base + Start` for PTS and DTS. A frame whose source DTS is below `base` after release is dropped (`DroppedLate`).
8. **Forced breaks.**
   - `Break()` opens a pending epoch whose frames all join it. Calling it while an epoch is pending does nothing.
   - A frame whose `Config` pointer differs from its track's previous frame does the same as `Break()`, and that frame is the first frame of the new epoch.
   - A jump while an epoch is pending restarts the pending epoch. Frames already held are dropped (`DroppedStale`).

- [ ] **Step 1: Write the failing tests.**
  - `TestClockFirstEpoch`: video from source DTS 1,000,000 and audio from 999,000 come out with audio DTS 0 and video DTS 1000, all in epoch 1, with source deltas preserved.
  - `TestClockHoldsUntilAllTracks`: with two expected tracks, three video frames return nothing, and the first audio frame returns four frames.
  - `TestClockAlignWindow`: with two expected tracks and only video, nothing is released until the held span reaches 90000.
  - `TestClockBackwardJumpOpensEpoch`: a 6 s jump back on both tracks, interleaved as in a looped file, gives epoch 2. `Start` matches rule 6, and DTS still strictly increases per track.
  - `TestClockForwardJump`: a 4 s forward gap on all tracks stays in the epoch with a 360000-tick gap; a 6 s gap opens epoch 2.
  - `TestClockSingleTrackGap`: video pauses 8 s while audio continues, then video resumes. There is no new epoch, every frame is delivered, and the video stream gap is 8 s.
  - `TestClockSmallBackStep`: a 1000-tick step back is dropped, `DroppedBackStep == 1`, and the epoch is unchanged.
  - `TestClockBreak`: after `Break()`, continuous source frames are in epoch 2. A second `Break()` before release changes nothing.
  - `TestClockConfigChangeOpensEpoch`: a new `Config` pointer on video with continuous timestamps puts that frame and all later frames in epoch 2.
  - `TestClockStaleDropped`: audio frames still on the old timeline after epoch 2's release are dropped and counted.
  - `TestClockLateTrackDropped`: a track whose first frame is below `base` after release is dropped and counted.
  - `TestClockDropsUntilKey`: three non-key video frames before the first keyframe are dropped, `DroppedBeforeKey == 3`, and the first released video frame is `Key`.
  - `TestClockMidGOPStart`: audio arrives before the first video keyframe and is dropped (`DroppedEarlyAudio > 0`). In the released frames, the first audio PTS minus the first video PTS is between 0 and one audio frame (1920 ticks).
  - `TestClockDropsRASLAtEpochStart`: a CRA followed by two RASL frames and then a normal frame at an epoch start releases the CRA and the normal frame, with `DroppedRASL == 2`.
  - `TestClockEpochStartAfterPresentationEnd`: epoch 1 has B-frame timing (PTS − DTS = 7200), epoch 2 has none, and epoch 2's first PTS is above epoch 1's largest PTS. `conform.CheckFrames` on the output returns `nil`.
  - `TestClockJumpWhilePending`: a second jump before release drops the held frames (`DroppedStale`) and releases only the latest timeline.
  - `TestClockWallAnchor`: with a `Now` that advances 1 s per call, `Epochs()[0].Wall` equals the time of the first frame's push.
  - `TestClockFlush`: frames held at an epoch start come out of `Flush()` in arrival order.

- [ ] **Step 2: Run them to see them fail.** `go test ./internal/clock/`: FAIL.

- [ ] **Step 3: Implement** the algorithm above.

- [ ] **Step 4: Run the tests.** `go test -race ./internal/clock/`: PASS.

- [ ] **Step 5: Commit.** Message: `feat(clock): add the per-stream clock with epochs`.

### Task 8: Hub

**Files:**
- Create: `internal/hub/hub.go`
- Test: `internal/hub/hub_test.go`

**Interfaces:**
- Consumes: `media.Frame`.
- Produces:

```go
type Options struct {
    GOPCacheFrames   int   // 0 means 1500
    AudioCacheTicks  int64 // audio-only streams: cache span; 0 means 90000 (1 s)
}
type Delivery struct { Frame media.Frame; Lost uint64 }
type SubStats struct { Name string; Delivered, Dropped uint64 }
func New(opt Options) *Hub
func (h *Hub) Write(f media.Frame)
func (h *Hub) Subscribe(name string, queue int) *Sub // queue 0 means 1024
func (h *Hub) Stats() []SubStats
func (h *Hub) Close()
func (s *Sub) C() <-chan Delivery
func (s *Sub) Close()
```

Behaviour the tests pin:

- **Sending.** `Write` sends with `select`/`default` under the hub mutex. A failed send puts that subscriber in skip mode.
- **Resume point.** The hub tracks the set of video tracks it has seen; a track counts as ended when it has sent nothing for 10 s of stream time.
  - In skip mode, the subscriber resumes per video track: frames of a video track are dropped until that track's next keyframe. RASL frames directly after that keyframe are dropped too.
  - Audio is dropped until every live video track has resumed. With no live video track, any frame resumes.
  - The first frame delivered after a skip carries `Lost`, equal to the frames dropped.
- **Cache.**
  - With video, the hub caches frames from the last keyframe of the first video track, capped at `GOPCacheFrames`. An over-long GOP empties the cache until the next keyframe.
  - On audio-only streams, the cache holds the last `AudioCacheTicks` of frames.
  - `Subscribe` replays the cache into the new queue when it fits, dropping RASL frames right after the cached keyframe. Otherwise the new subscriber starts in skip mode.
- **Closing.** `Sub.Close` and `Hub.Close` close channels under the mutex. `Write` after `Close` does nothing.

- [ ] **Step 1: Write the failing tests.**
  - `TestHubDeliversInOrder`.
  - `TestHubSlowSubscriberSkipsToKeyframe`: queue 4.
    1. Write one 10-frame GOP and read nothing: the first 4 frames are queued and 6 are dropped.
    2. Drain the 4.
    3. Write GOPs 2 and 3: the next delivery is GOP 2's keyframe with `Lost == 6`, followed by the rest of GOP 2.
  - `TestHubResumeDropsRASL`: a resume on a CRA followed by two RASL frames delivers the CRA and then the first non-RASL frame.
  - `TestHubTwoVideoTracks`: in skip mode, track 1's frames are delivered only after track 1's own keyframe, even when track 0 has already resumed.
  - `TestHubVideoTrackEnds`: when video stops for more than 10 s of stream time and audio continues, a skipping subscriber resumes on audio.
  - `TestHubWriteNeverBlocks`: with a subscriber that never reads, 100,000 writes return within the test timeout.
  - `TestHubLateJoinStartsOnKey`: after a keyframe and 5 frames, a new subscriber's first delivery is that keyframe with `Lost == 0`, followed by the 5 frames.
  - `TestHubLateJoinCacheTooLarge`: with queue 2 and a 5-frame cache, the subscriber's first delivery is the next keyframe.
  - `TestHubAudioOnly`: the cache holds at most 1 s of frames, and skip mode ends at the next frame.
  - `TestHubStats`: `Dropped` and `Delivered` are counted per subscriber name.
  - `TestHubClose`: channels close, and `Write` after `Close` does not panic.
  - `TestHubConcurrentChurn`: under `-race`, run one writer, four readers and a goroutine that subscribes and closes in a loop for 10,000 iterations. Count iterations, not wallclock.

- [ ] **Step 2: Run them to see them fail.** `go test ./internal/hub/`: FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run the tests.** `go test -race ./internal/hub/`: PASS.

- [ ] **Step 5: Commit.** Message: `feat(hub): add the GOP-aware non-blocking hub`.

### Task 9: End-to-end media core test

**Files:**
- Create: `internal/e2e/doc.go` (package comment only), `internal/e2e/m1_test.go`

**Interfaces:**
- Consumes: `ts.NewDemuxer`, `clock.New`, `hub.New`, `conform.CheckFrames`, the fixtures.

- [ ] **Step 1: Write the tests.** A helper runs a fixture through demux, a Clock with `Kinds: d.TrackKinds()`, and a hub with one subscriber whose queue holds the whole clip. It calls `Flush` at EOF and returns the delivered frames.
  - `TestM1Fixtures`: `CheckFrames` returns `nil` on each of these fixtures:
    - `h264_aac.ts`
    - `h265_aac51.ts`
    - `h264_aacmono_wrap.ts`
    - `h264_aac_loop.ts`
    - `h264_aac_then_640.ts`
    - `h264_aac_midgop.ts`
  - `TestM1LoopEpochs`: `h264_aac_loop.ts` delivers epochs 1 and 2 only, and the last DTS is below 13 s in ticks.
  - `TestM1ConfigChangeEpochs`: `h264_aac_then_640.ts` delivers epochs 1 and 2 only. That is one epoch for the jump and the config change together, because they arrive on the same frame.
  - `TestM1WrapStartsAtZero`: `h264_aacmono_wrap.ts` starts at stream DTS 0 and stays below 7 s in ticks.
  - `TestM1SlowConsumer`: no sleeps. Write all frames of `h264_aac_loop.ts` into a hub with a queue-8 subscriber, and drain 8 deliveries after every 40 writes.
    - At least one delivery has `Lost > 0`.
    - Every such delivery is a video keyframe.
    - `CheckFrames` on the delivered frames returns `nil`.

- [ ] **Step 2: Run them.**

Run: `go test -race ./internal/e2e/`
Expected: PASS. A failure here is a defect in Tasks 5 to 8. Fix it there, with a unit test that reproduces it.

- [ ] **Step 3: Run the full check.**

Run: `make tools && make check && make cover covercheck`
Expected: everything passes and the coverage gates are met.

- [ ] **Step 4: Commit.** Message: `test(e2e): run every fixture through the media core`.

- [ ] **Step 5: Ask the user before pushing.** With their yes, push `rebuild` and open a draft PR into `main` titled `v2 rebuild`, so CI runs on every push. The PR stays a draft until M7.
