# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Open Streamer is a live media server in Go: it ingests over many protocols, normalises to an internal MPEG-TS pipeline, optionally transcodes via libavcodec, and publishes HLS, DASH, RTMP, RTSP and SRT.

## Commands

```bash
make build              # server only → bin/open-streamer (pure Go)
make build-all          # server + open-streamer-transcoder (cgo, needs libav 8)
make run                # go run the server
make test               # go test -race -shuffle=on -count=1 -timeout=5m ./...
make lint               # golangci-lint run ./...
make check              # tidy + vet + lint + test
make fmt                # gofumpt if installed, else gofmt
make swagger            # regenerate api/docs from swag annotations
make hooks-install      # pre-commit hook: regenerates and auto-stages api/docs when Go files are staged

go test -race -count=1 -run TestName ./internal/<pkg>/   # single test
```

**cgo boundary.** Only `internal/transcoder/native` and `cmd/open-streamer-transcoder` need cgo and libav 8 headers; FFmpeg 8 is not in apt, so distro packages usually won't do. Without libav, `go build ./...`, `go vet ./...`, `make check` and `make lint` fail on those two packages. To test everything else:

```bash
go test -race -shuffle=on -count=1 -timeout=5m \
  $(go list ./... | grep -v -e '/transcoder/native$' -e '/cmd/open-streamer-transcoder$')
```

To exercise the native packages, use the builder image, which is what CI does (`WORKDIR` is `/src`). `make builder-image` builds it locally and compiles FFmpeg from source:

```bash
docker run --rm -v "$PWD":/src open-streamer-builder:v1 go test -count=1 ./internal/transcoder/native/...
```

Tooling caveats:

- Use `make swagger`, not `make generate`. The `go:generate` line scans fewer directories, so it omits types from manager, publisher, transcoder, vod, config and pkg/version.
- `make test-integration` passes `-tags integration`, but no file in the repo carries that tag, so it only runs the ordinary `internal/transcoder` tests.
- The `.pb.go` files under `internal/transcoder/native/proto/` are checked in, and there is no make target to regenerate them.
- Coverage gates live in `.testcoverage.yml` (run with `make cover && go run github.com/vladopajic/go-test-coverage/v2 -c .testcoverage.yml`). New exclusions must carry a comment justifying them.

CI runs on PRs into `main` only: `go mod tidy` drift check, vet/build/test in the builder image, and govulncheck. The golangci-lint job is `continue-on-error`. Releases are built from `v*` tags.

Lint rules that affect how code is written: `exhaustive` is set with `default-signifies-exhaustive: false`, so a `switch` on an enum must list every value even with a `default`. `godot` requires top-level declaration comments to end with a period. Formatting is gofumpt.

## Running locally

Configuration is split in two:

- **Bootstrap** (`config.LoadStorage`): only the storage driver and directory, from `OPEN_STREAMER_STORAGE_*` env vars or `config.yaml` in `.` or `/etc/open-streamer`. The default is the JSON driver in `./test_data`.
- **Everything else** (`domain.GlobalConfig`): lives in the store and is edited through `/api/v1/config`. `internal/runtime` diffs old against new and cycles only the affected services.

`.env.example` is mostly stale: apart from the storage vars and `OPEN_STREAMER_API_ADMIN_PASSWORD`, nothing reads those variables, and the `sql`/`mongo` drivers don't exist (only `json` and `yaml`).

Transcoded streams need the `open-streamer-transcoder` binary next to the server binary or on `$PATH`. Passthrough streams don't need it.

## Architecture

`docs/ARCHITECTURE.md` is the detailed reference (subsystems, invariants table) and `docs/APP_FLOW.md` has step-by-step traces. The essentials:

```
Input URL → Manager (failover) → Ingestor → timeline.Normaliser → Buffer Hub → Transcoder / Publisher / DVR
                                                                       ▲              │
                                                                       └── renditions ┘
```

Invariants that must hold:

1. **The Buffer Hub (`internal/buffer`) is the only data source.** Publishers, the transcoder and DVR never read from the network or from each other.
2. **Hub writes never block.** Fan-out is `select { case ch <- pkt: default: }`, and slow consumers drop packets. Any blocking send here can freeze every viewer.
3. **Failover does not restart the transcoder.** The Manager swaps the ingestor goroutine, and the subprocess swaps only its decoder (`SwitchInput`) while encoders stay alive.
4. **One goroutine per ingest, never one process.** The only subprocess is the per-stream transcoder.
5. **Hot-reload goes by diff.** `coordinator.ComputeDiff` routes each change to the smallest set of service calls.
6. **All persistence goes through `internal/store`** repository interfaces.

How the pieces fit:

- **Wiring.** `cmd/server/main.go` registers everything in `samber/do/v2`. Circular dependencies are broken with post-construction setters (`SetRuntimeManager`, `SetMediaAuthorizer`, `SetStreamLookup`).
- **Coordinator (`internal/coordinator`)** is the only place that knows the whole per-stream pipeline. It talks to services through narrow interfaces in `deps.go`, which is also how its tests inject spies. `RunReconciler` restarts any non-disabled stream that should be running, every 10s.
- **Buffer namespaces.** Without a transcoder, one `<code>` buffer. With one, the ingestor writes `$raw$<code>`, the transcoder reads it and writes `$r$<code>$track_N`, and publishers read those.
- **Transcoder.** `internal/transcoder` supervises one `open-streamer-transcoder` per stream over a bidirectional gRPC stream on a Unix socket. It decodes once and encodes N renditions, so there is no per-rendition lifecycle: any transcoder config change goes through `reloadTranscoderFull`, which rebuilds buffers, transcoder, publisher and DVR. `ProfilesDiff` no longer selects individual renditions.
- **Timestamps.** `internal/timeline.Normaliser` anchors each track to local wallclock before the hub write. Session boundaries travel on `buffer.Packet.SessionStart`; consumers dispatch on that, not on per-packet discontinuity flags.
- **Templates.** Resolution happens at read time: `domain.ResolveStream` fills zero-valued stream fields from the template on a copy. The store and `GET /streams` keep the raw record, so never persist or return the merged form.
- **Runtime streams (`internal/autopublish`)** are created by RTMP pushes matching a template prefix, and exist only in memory. They must never reach the store.

When adding a `Stream` field, check each of these or it will silently misbehave:

- `ComputeDiff`/`Coordinator.Update`: unlisted fields are not applied to running streams.
- `domain.ResolveStream` and `Template`, if the field should inherit.
- `make swagger`, if it is exposed over the API.

Conventions from `docs/ARCHITECTURE.md`:

- Wrap errors as `fmt.Errorf("module: operation: %w", err)`, and never both log and return the same error.
- Lock order is `Service.mu` then per-stream `state.mu`, never the reverse.
- Every spawned goroutine needs an explicit cancel path.

## Where the docs and code disagree

- `docs/ARCHITECTURE.md` describes DVR as a native MPEG-TS segmenter. Recording is now the CMAF blob archive in `internal/dvr/blob`; legacy `.ts` recordings are served read-only.
- Its "no sibling-module imports" rule is aspirational: `manager` imports `ingestor`, `publisher` and `transcoder`; `publisher` imports `ingestor/push`; `dvr/blob` imports `publisher/dash`.
- `FFmpeg` in identifiers and comments (e.g. `needsFFmpeg`) usually means the native libavcodec pipeline; no ffmpeg CLI is spawned. The comment in `cmd/server/main.go` about a stubbed `Service.Start` and `ErrNotImplemented` is out of date.
- `recording.*`, `segment.written` and `dvr.segment_pruned` are declared in `internal/domain/event.go` but nothing publishes them. `docs/APP_FLOW.md` still describes them as firing.
