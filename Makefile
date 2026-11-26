.PHONY: test lint tools fmt tidy vet check cover covercheck fixtures test-ci-peers ostranscoder test-transcode test-nvenc openstreamer

test:
	go test -race -shuffle=on -count=1 -timeout=10m ./...

lint:
	golangci-lint run ./...

tools:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0

fmt:
	@if command -v gofumpt >/dev/null 2>&1; then gofumpt -l -w .; else gofmt -l -w .; fi

tidy:
	go mod tidy

vet:
	go vet ./...

check: tidy vet lint test

cover:
	go test -race -shuffle=on -count=1 -timeout=10m -coverprofile=coverage.out ./...

covercheck:
	go run github.com/vladopajic/go-test-coverage/v2@v2.22.0 -c .testcoverage.yml

fixtures:
	sh testdata/fixtures/gen.sh

# The server, pure Go, at bin/openstreamer. The version is the tag, or the
# commit when there is none.
openstreamer:
	go build -ldflags "-X main.version=$(shell git describe --tags --always --dirty)" -o bin/openstreamer ./cmd/openstreamer

# The packages whose tests use ffmpeg, ffprobe, VLC or GStreamer, run in a
# throwaway container of testdata/peers/Dockerfile.ci (CI's Ubuntu 24.04
# peers: ffmpeg 6.1, VLC 3, GStreamer 1.24). See scripts/test-ci-peers.sh;
# ARGS takes go test flags and packages, split on spaces and passed as
# written: $(value ARGS) keeps make from expanding the $ of a pattern (it
# read "$ " as a variable and swallowed the package after it), and the
# shell neither globs nor interprets | or ; in it. Quotes are not removed,
# so write no inner quotes: ARGS='-run ^TestX$ ./pkg/rtsp/'.
test-ci-peers: export TEST_CI_PEERS_ARGS := $(value ARGS)
test-ci-peers:
	@set -f; sh scripts/test-ci-peers.sh $$TEST_CI_PEERS_ARGS

# The transcoder (M4a). Both targets fetch the pinned FFmpeg 8.1 on the host
# first (scripts/ffmpeg-dev.sh, cached in ~/.cache/osv2-ffmpeg), then run in
# osv2-ci-peers:m4a through scripts/test-ci-peers.sh --exec, with its mounts:
# Ubuntu 24.04's glibc, and PKG_CONFIG_PATH and LD_LIBRARY_PATH on the
# prefix. The arch is the host's: there is no cross build.
OSTX_ARCH := $(shell uname -m | sed 's/^x86_64$$/amd64/; s/^aarch64$$/arm64/')

# Builds and bundles cmd/ostranscoder into dist/ostranscoder-<arch>/: the
# binary, with DT_RPATH $$ORIGIN/lib, and lib/ with the FFmpeg libraries it
# links. See scripts/bundle-ostranscoder.sh.
ostranscoder:
	@sh scripts/ffmpeg-dev.sh $(OSTX_ARCH) >/dev/null
	@sh scripts/test-ci-peers.sh --name osv2-ostx-build-$$$$ --exec \
		sh scripts/bundle-ostranscoder.sh $(OSTX_ARCH) dist/ostranscoder-$(OSTX_ARCH)

# go test -race -tags libav $(ARGS) in the same container. ARGS is quoted as
# for test-ci-peers, and defaults to the cgo packages that exist.
TRANSCODE_PKGS := $(patsubst %,./%/...,$(wildcard internal/transcode/native cmd/ostranscoder))
test-transcode: export TEST_TRANSCODE_ARGS := $(value ARGS)
test-transcode:
	@sh scripts/ffmpeg-dev.sh $(OSTX_ARCH) >/dev/null
	@set -f; sh scripts/test-ci-peers.sh --name osv2-ostx-test-$$$$ --exec \
		go test -race -tags libav $${TEST_TRANSCODE_ARGS:-$(TRANSCODE_PKGS)}

# The GPU proof (ruling R-C12), on this host only: refuses to start while
# another process holds NVENC sessions or GPU compute (OS_NVENC_SHARED=1
# warns instead), runs the NVENC scenarios on the host against the bundle
# with OS_NVENC=1 and OS_REQUIRE_NVENC=1 (the device tests are opt-in: a
# plain make test opens no NVENC session), plays the recorded presentation in VLC in
# osv2-ci-peers:m4a, and writes dist/nvenc-report/report.md. See
# scripts/test-nvenc.sh.
test-nvenc:
	@sh scripts/test-nvenc.sh
