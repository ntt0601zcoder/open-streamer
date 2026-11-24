.PHONY: test lint tools fmt tidy vet check cover covercheck fixtures test-ci-peers

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
