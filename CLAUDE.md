# CLAUDE.md

Open Streamer v2 is a live media server in Go, rebuilt from scratch on the `rebuild` branch. The design and milestones are in `docs/REBUILD_PLAN.md`; read it before adding packages.

## Commands

```bash
make test         # go test -race -shuffle=on -count=1 -timeout=20m ./...
make test-ci-peers # peer packages in osv2-ci-peers:m4a (CI's Ubuntu 24.04: ffmpeg 6.1, VLC 3, GStreamer 1.24); ffmpeg-8 rows via OS_FFMPEG8 or on the host
make ostranscoder  # bundle cmd/ostranscoder (cgo, -tags libav) with the pinned FFmpeg 8.1 libs into dist/ostranscoder-<arch>/, built in osv2-ci-peers:m4a
make test-transcode # go test -race -tags libav $(ARGS) in osv2-ci-peers:m4a; ARGS defaults to the cgo packages (scripts/ffmpeg-dev.sh fetches FFmpeg)
make test-nvenc     # GPU host only: the NVENC proof (scripts/test-nvenc.sh); refuses while NVENC is in use (OS_NVENC_SHARED=1 warns); writes dist/nvenc-report/report.md. NVENC device tests are opt-in (OS_NVENC=1, which it sets): make test never opens a session
make openstreamer # the server, pure Go, at bin/openstreamer (-config FILE; cmd/openstreamer/openstreamer.example.yaml)
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

## The server (M5a, M5b1, M5c1)

`cmd/openstreamer` is the server binary; `cmd/osdev` stays the development harness. `openstreamer -config FILE` runs it, `openstreamer hash-password` reads a password from stdin and prints its hash, `openstreamer -version` prints the version.

All configuration is **one YAML file** (`internal/config`; documented example: `cmd/openstreamer/openstreamer.example.yaml`), at most 4 MiB, mode 0600 on Unix, decoded strictly:

- `version: 1`.
- `server`: `http.listen` (default `:8080`, `/live/`), `api.listen` (default `127.0.0.1:8081`, plain: the API, `/metrics`, pprof), `rtmp.listen`/`app`, `rtsp.listen`/`app`/`udp`, `srt.listen`/`app`/`latency_ms` (empty `listen` turns a server off; `app` defaults to `live`, `latency_ms` to 120), `tls.cert_file`/`key_file` (one PEM pair for every TLS listener) and a `tls_listen` beside the `listen` of `http`, `api`, `rtmp` and `rtsp` (HTTPS, the API over HTTPS, RTMPS, RTSPS interleaved only; no default), `transcoder.binary` (default: `ostranscoder` next to the executable), `assets_dir`, `log.level` (`debug`, `info`, `warn`, `error`) and `log.format` (`json`, `text`). Never applied while running: reported as `restart_required`. The one live `server` change is the **content** of the certificate files: read again every 30 s, on SIGHUP and on `POST /api/v1/reload` (`internal/certs`; a pair that does not load keeps the one in force).
- `users`: `name`, `role` (`viewer` reads; `editor` reads, writes, sees secrets and pprof), `password` (`pbkdf2-sha256$600000$<salt>$<key>`, exactly 600000 iterations). At least one editor, at most 64 users.
- `streams`: `spec.Stream` documents (`name`, `disabled`, `inputs`, `layout`, `transcode`, `thumbnail`, `outputs` with `hls`, `dash`, `httpts`, `rtmp`, `rtsp`, `srt`, `push`). The `yaml` and `json` tags are equal. Over TLS: `rtmps://` (listen input on the server's RTMPS address, pull, push) and `rtsps://` (pull, TCP only) verify the peer against the system roots; `rtmps+insecure://` and `rtsps+insecure://` skip that for one URL.

Anchors, aliases and merge keys are refused, and every number must be a plain decimal integer.

Packages of the control plane:

- `internal/spec`: the stream document; `Validate`, `Redacted`, `ETag` (a keyed tag, new at every start), `Diff`.
- `internal/config`: parse (strict; lenient for streams at start), the node-tree edits `SetStream`/`DeleteStream`, `File` (atomic write, stale detection by SHA-256).
- `internal/auth`: PBKDF2 hashes, `Users` (verification, remembered logins), the failed-login `Limiter`.
- `internal/stream`: `Supervisor` (one goroutine per stream, mailbox, `View`/`Status` behind atomic pointers, rebuild with backoff, no reconciler) and `Manager` (the set, `Check`, listen claims, `copy://` cycles, `Replace`, the output lookups).
- `internal/api`: the REST API on `net/http.ServeMux`, Basic auth and roles, the hand-written `openapi.json` (kept equal to the routes and the spec by tests).
- `internal/metrics` (Prometheus text, `metricstest` for tests), `internal/logging` (slog with a scrubbing handler), `internal/server` (listeners, wiring, the apply path), `internal/certs` (the certificate in force, behind an atomic pointer); test certificates come from `pkg/tlstest`, never from a checked-in key.
- `internal/asset`: the flat asset directory `server.assets_dir` (images `.png`/`.jpg`/`.jpeg`, fonts `.ttf`/`.otf`; content checks, the font probe `ostranscoder -probe font`, quota, temp-file-and-rename writes, SHA-256 ETags), served as `GET /api/v1/assets` and `GET`/`PUT`/`DELETE /api/v1/assets/{name}` (raw `application/octet-stream` body; writes are editor-only). A `PUT` refreshes every running watermark that names the asset (`Manager.RefreshAsset`); a `DELETE` of an asset a stream names is 409 `asset_in_use`.

**One apply path (R-D4).** A whole-config `PUT /api/v1/config` (the client's exact bytes), a per-stream `PUT`/`DELETE /api/v1/streams/{name}` (a node-tree edit of the text in force), and a reload (SIGHUP or `POST /api/v1/reload`, the file as on disk) all end in `server.apply`, under one mutex: parse and validate the whole document, `Manager.Replace` judges the streams, the file is written inside `Replace`'s commit (API edits only), the streams change, and the users are replaced when their section changed. An API write is refused with 409 while the file on disk differs from the text in force; nothing is half applied. A stream the start left stopped (a conflict, a failed `Check`) is tried again by every apply that leaves it unchanged (`Manager.plan`: `started`, or still stopped with the reason refreshed, never failing the edit). A reload of a file that cannot be read is 409 `config_unreadable` (`config.ErrUnreadable`); a write that fails for another reason than a changed file is 500 `config_write_failed` (`config.ErrWrite`).

**Three outcomes of an edit (R-D6, R-E2).** `spec.Diff` gives `ChangeNone` (untouched) or one of: `ChangeOutputs` (applied in place by the supervisor's `reattach`: packagers, output flags via `hub.Refuse`, pushes keyed by URL; nothing else is touched); `ChangeLive` (inputs, the watermark and/or the audio language, maybe outputs: `Pipeline.SetInputs` swaps the inputs between two frames, keeping matched readers, at most one epoch change; `Transcoder.SetWatermark` swaps the picture, the old one stays on failure with `watermark: …` in `last_error`; `Supervisor.Refresh` rebuilds the watermark from the assets; `Packager.SetAudioLanguage` relabels the masters and the MPD); or `ChangeRebuild` (the generation is torn down and rebuilt at once). The API answers `outputs`, `live` or `rebuild`.

## v1 is reference only

The v1 code lives on `main`. Read it with `git show main:<path>`; never copy it wholesale. Milestones bring back what they need from history.

## Invariants

- Module path is `github.com/ntt0601zcoder/open-streamer`. No cgo in M1: `go build ./...`, `go vet ./...` and `go test ./...` work on a plain machine.
- Libraries: only independent ones. The approved set is mp4ff, pion, gosrt, go-astiav (cgo, only for the transcoder), protobuf's `protowire`, testify and `go.yaml.in/yaml/v3` (the config file). HTTP routing is `net/http`'s `ServeMux`, not chi (M5a's R-D7); metrics are our own text exposition, not `client_golang`; password hashing is the standard library's `crypto/pbkdf2`. M1 is limited to the M1 dependency list below. Never `bluenviron/*` (mediacommon, gortsplib, gohlslib) or `q191201771/lal`, and do not copy code from them.
- What no independent library provides goes into our own reusable package under `pkg/`, which never imports `internal/`.
- Module dependencies allowed in M1: the Go standard library, `Eyevinn/mp4ff`, `stretchr/testify`. MPEG-TS is our own `pkg/mpegts`. Tools run with `go run <tool>@<version>` or `go install`, never added to `go.mod`.
- Timestamps on `media.Frame` are `int64` in 90 kHz ticks (`media.ClockRate = 90000`). No `time.Duration` and no milliseconds on the media path.
- `media`, `ts`, `clock`, `conform` and `hub` start no goroutines.
- `hub.Write` never blocks, under any subscriber behaviour.
- Audio frames always have `Key == true` and `RASL == false`.
- Video `Frame.Data` never contains VPS, SPS, PPS or AUD NAL units; they live only in `TrackConfig`.
- `internal/transcode/picture` is pure Go; operator text never enters a libavfilter graph string; an operator's font reaches the child as bytes, never as a path.
- Fixtures are opened from tests as `../../testdata/fixtures/<name>` (or `../../../` from deeper packages).

## Conventions

- Wrap errors as `fmt.Errorf("<pkg>: <operation>: %w", err)`. Never log and return the same error.
- Lint: gofumpt formatting; `godot` (top-level comments end with a period); `exhaustive` with `default-signifies-exhaustive: false`, so a `switch` on an enum lists every value even with a `default`.
