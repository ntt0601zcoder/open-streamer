#!/bin/sh
# Runs the packages whose tests use ffmpeg, ffprobe, VLC or GStreamer inside
# osv2-ci-peers:m3b3 (testdata/peers/Dockerfile.ci), so that they meet the
# peer versions CI has (Ubuntu 24.04: ffmpeg 6.1, VLC 3, GStreamer 1.24)
# rather than the host's. The container is throwaway (--rm), named
# osv2-ci-peers-<pid>, on the host network, and runs as the host's user;
# the repository is mounted at /src, the host's Go toolchain and module
# cache read-only, and the build cache lives in
# ${XDG_CACHE_HOME:-~/.cache}/osv2-ci-peers. OS_REQUIRE_PEERS=1, as in CI,
# makes a missing peer a failure; other OS_* variables set in the caller's
# environment (OS_MEDIAMTX_RTSP, ...) are passed on, except the peer-image
# ones. OS_CI_PEERS_IMAGE overrides the image. MediaMTX is not
# started: tests that need it skip unless its variables are set.
#
# Usage: scripts/test-ci-peers.sh [go test flags] [packages]
#   scripts/test-ci-peers.sh                        # every peer package
#   scripts/test-ci-peers.sh -run '^TestSRTFFprobe$' ./internal/output/srtout/
#
# Flags given follow the defaults (-race -shuffle=on -count=1
# -timeout=10m), so a later -count or -timeout wins; packages given replace
# the default list. Arguments are split on spaces and not globbed.
set -euf

image=${OS_CI_PEERS_IMAGE:-osv2-ci-peers:m3b3}
repo=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo"

if ! docker image inspect "$image" >/dev/null 2>&1; then
	echo "building $image" >&2
	docker build -t "$image" -f testdata/peers/Dockerfile.ci testdata/peers
fi

flags="-race -shuffle=on -count=1 -timeout=10m"
pkgs=""
for a in "$@"; do
	case $a in
	./* | github.com/*) pkgs="$pkgs $a" ;;
	*) flags="$flags $a" ;;
	esac
done
if [ -z "$pkgs" ]; then
	# Every package whose tests name a peer program or testpeer.
	pkgs=$(grep -rlE '\b(ffmpeg|ffprobe|cvlc|gst-launch-1\.0|testpeer)\b' --include='*_test.go' cmd internal pkg |
		xargs -n1 dirname | sort -u | sed 's|^|./|' | tr '\n' ' ')
fi

goroot=$(go env GOROOT)
modcache=$(go env GOMODCACHE)
cache=${XDG_CACHE_HOME:-$HOME/.cache}/osv2-ci-peers
mkdir -p "$cache/go-build"
go mod download

# The peer-image variables are not passed on: the container has the peers
# itself and no docker.
envs=""
for v in $(env | sed -n 's/^\(OS_[A-Z0-9_]*\)=.*/\1/p'); do
	case $v in
	OS_PEER_IMAGE | OS_PEER_FORCE_IMAGE | OS_CI_PEERS_IMAGE) ;;
	*) envs="$envs -e $v" ;;
	esac
done

# shellcheck disable=SC2086 # flags, packages and -e options are word lists
exec docker run --rm --init --name "osv2-ci-peers-$$" --network host \
	--user "$(id -u):$(id -g)" \
	-v "$goroot:/usr/local/go:ro" -v "$modcache:/gomod:ro" -v "$cache/go-build:/gocache" \
	-v "$repo:/src" -w /src \
	-e HOME=/tmp -e PATH=/usr/local/go/bin:/usr/bin:/bin \
	-e GOCACHE=/gocache -e GOMODCACHE=/gomod -e GOFLAGS=-mod=mod -e GOPROXY=off \
	-e GOTOOLCHAIN=local -e OS_REQUIRE_PEERS=1 $envs \
	"$image" go test $flags $pkgs
