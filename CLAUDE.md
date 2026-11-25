# CLAUDE.md

Open Streamer v2 is a live media server in Go, rebuilt from scratch on the `rebuild` branch. The design and milestones are in `docs/REBUILD_PLAN.md`; read it before adding packages.

## Commands

```bash
make test         # go test -race -shuffle=on -count=1 -timeout=10m ./...
make test-ci-peers # peer packages in osv2-ci-peers:m4a (CI's Ubuntu 24.04: ffmpeg 6.1, VLC 3, GStreamer 1.24); ffmpeg-8 rows via OS_FFMPEG8 or on the host
make ostranscoder  # bundle cmd/ostranscoder (cgo, -tags libav) with the pinned FFmpeg 8.1 libs into dist/ostranscoder-<arch>/, built in osv2-ci-peers:m4a
make test-transcode # go test -race -tags libav $(ARGS) in osv2-ci-peers:m4a; ARGS defaults to the cgo packages (scripts/ffmpeg-dev.sh fetches FFmpeg)
make test-nvenc     # GPU host only: the NVENC proof (scripts/test-nvenc.sh); refuses while NVENC is in use (OS_NVENC_SHARED=1 warns); writes dist/nvenc-report/report.md
make lint         # golangci-lint run ./...
make fmt          # gofumpt if installed, else gofmt
make tidy         # go mod tidy
make vet          # go vet ./...
make check        # tidy + vet + lint + test
make cover        # writes coverage.out
make covercheck   # enforce .testcoverage.yml (file 80, package 85, total 90)
make fixtures     # sh testdata/fixtures/gen.sh
make tools        # install golangci-lint v2.14.0
```

## v1 is reference only

The v1 code lives on `main`. Read it with `git show main:<path>`; never copy it wholesale. Milestones bring back what they need from history.

## Invariants

- Module path is `github.com/ntt0601zcoder/open-streamer`. No cgo in M1: `go build ./...`, `go vet ./...` and `go test ./...` work on a plain machine.
- Libraries: only independent ones. The approved set for later milestones is mp4ff, pion, gosrt, go-astiav and chi (go-astiav is cgo and only for the transcoder in M4). M1 is limited to the M1 dependency list below. Never `bluenviron/*` (mediacommon, gortsplib, gohlslib) or `q191201771/lal`, and do not copy code from them.
- What no independent library provides goes into our own reusable package under `pkg/`, which never imports `internal/`.
- Module dependencies allowed in M1: the Go standard library, `Eyevinn/mp4ff`, `stretchr/testify`. MPEG-TS is our own `pkg/mpegts`. Tools run with `go run <tool>@<version>` or `go install`, never added to `go.mod`.
- Timestamps on `media.Frame` are `int64` in 90 kHz ticks (`media.ClockRate = 90000`). No `time.Duration` and no milliseconds on the media path.
- `media`, `ts`, `clock`, `conform` and `hub` start no goroutines.
- `hub.Write` never blocks, under any subscriber behaviour.
- Audio frames always have `Key == true` and `RASL == false`.
- Video `Frame.Data` never contains VPS, SPS, PPS or AUD NAL units; they live only in `TrackConfig`.
- Fixtures are opened from tests as `../../testdata/fixtures/<name>` (or `../../../` from deeper packages).

## Conventions

- Wrap errors as `fmt.Errorf("<pkg>: <operation>: %w", err)`. Never log and return the same error.
- Lint: gofumpt formatting; `godot` (top-level comments end with a period); `exhaustive` with `default-signifies-exhaustive: false`, so a `switch` on an enum lists every value even with a `default`.
