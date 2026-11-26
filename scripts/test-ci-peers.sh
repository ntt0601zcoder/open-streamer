#!/bin/sh
# Runs the packages whose tests use ffmpeg, ffprobe, VLC or GStreamer inside
# osv2-ci-peers:m4a (testdata/peers/Dockerfile.ci), so that they meet the
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
#        scripts/test-ci-peers.sh [--name NAME] --exec CMD [ARGS...]
#   scripts/test-ci-peers.sh                        # every peer package
#   scripts/test-ci-peers.sh -run '^TestSRTFFprobe$' ./internal/output/srtout/
#
# ffmpeg 8 rows (testpeer.FFmpeg8): with OS_FFMPEG8 set (CI's pinned
# static build, say), its directory is mounted read-only at the same path
# and the rows run in the container. Without it the container's apt ffmpeg
# is 6.1, so the container skips them (OS_FFMPEG8_ON_HOST=1) and, when no
# -run or -skip is given, they then run on the host, whose ffmpeg must be 8.
#
# The transcoder (M4a): when scripts/ffmpeg-dev.sh has unpacked the
# pinned FFmpeg 8.1 for the host's arch, its cache directory
# (${OS_FFMPEG_CACHE:-${XDG_CACHE_HOME:-~/.cache}/osv2-ffmpeg}) is mounted
# read-only at the same path, with OS_FFMPEG_CACHE, PKG_CONFIG_PATH and
# LD_LIBRARY_PATH pointing at the prefix, so -tags libav builds and runs in
# the container (FFmpeg 8.1's sonames differ from the apt ffmpeg 6.1's).
# OS_TRANSCODER (a bundled ostranscoder) is mounted read-only at the same
# path with its directory, as OS_FFMPEG8 is.
#
# --name NAME names the container (default osv2-ci-peers-<pid>), and
# --exec CMD... runs CMD in the container, with the same mounts and
# environment, instead of go test: make test-transcode and make
# ostranscoder use it.
#
# Flags given follow the defaults (-race -shuffle=on -count=1
# -timeout=20m), so a later -count or -timeout wins; packages given replace
# the default list. Arguments are split on spaces and not globbed.
set -euf

image=${OS_CI_PEERS_IMAGE:-osv2-ci-peers:m4a}
repo=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo"

name=osv2-ci-peers-$$
execmode=0
while [ $# -gt 0 ]; do
	case $1 in
	--name)
		name=$2
		shift 2
		;;
	--exec)
		execmode=1
		shift
		break
		;;
	*) break ;;
	esac
done
if [ "$execmode" = 1 ] && [ $# -eq 0 ]; then
	echo "test-ci-peers: --exec needs a command" >&2
	exit 2
fi

if ! docker image inspect "$image" >/dev/null 2>&1; then
	echo "building $image" >&2
	docker build -t "$image" -f testdata/peers/Dockerfile.ci testdata/peers
fi

flags="-race -shuffle=on -count=1 -timeout=20m"
pkgs=""
if [ "$execmode" = 0 ]; then
	for a in "$@"; do
		case $a in
		./* | github.com/*) pkgs="$pkgs $a" ;;
		*) flags="$flags $a" ;;
		esac
	done
fi
if [ "$execmode" = 0 ] && [ -z "$pkgs" ]; then
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
	OS_PEER_IMAGE | OS_PEER_FORCE_IMAGE | OS_CI_PEERS_IMAGE | OS_FFMPEG8 | OS_FFMPEG8_ON_HOST | \
		OS_FFMPEG_CACHE | OS_TRANSCODER) ;;
	*) envs="$envs -e $v" ;;
	esac
done

ff8=""
onhost=0
if [ -n "${OS_FFMPEG8:-}" ]; then
	ff8=$(cd "$(dirname "$OS_FFMPEG8")" && pwd)/$(basename "$OS_FFMPEG8")
	envs="$envs -v $(dirname "$ff8"):$(dirname "$ff8"):ro -e OS_FFMPEG8=$ff8"
else
	onhost=1
	envs="$envs -e OS_FFMPEG8_ON_HOST=1"
fi

case $(uname -m) in
x86_64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) arch=$(uname -m) ;;
esac
if ffprefix=$(sh scripts/ffmpeg-dev.sh --cached "$arch" 2>/dev/null); then
	ffcache=$(dirname "$ffprefix")
	envs="$envs -v $ffcache:$ffcache:ro -e OS_FFMPEG_CACHE=$ffcache"
	envs="$envs -e PKG_CONFIG_PATH=$ffprefix/lib/pkgconfig -e LD_LIBRARY_PATH=$ffprefix/lib"
fi

if [ -n "${OS_TRANSCODER:-}" ]; then
	ostx=$(cd "$(dirname "$OS_TRANSCODER")" && pwd)/$(basename "$OS_TRANSCODER")
	envs="$envs -v $(dirname "$ostx"):$(dirname "$ostx"):ro -e OS_TRANSCODER=$ostx"
fi

run() {
	# shellcheck disable=SC2086 # -e/-v options are a word list
	docker run --rm --init --name "$name" --network host \
		--user "$(id -u):$(id -g)" \
		-v "$goroot:/usr/local/go:ro" -v "$modcache:/gomod:ro" -v "$cache/go-build:/gocache" \
		-v "$repo:/src" -w /src \
		-e HOME=/tmp -e PATH=/usr/local/go/bin:/usr/bin:/bin \
		-e GOCACHE=/gocache -e GOMODCACHE=/gomod -e GOFLAGS=-mod=mod -e GOPROXY=off \
		-e GOTOOLCHAIN=local $envs -e OS_REQUIRE_PEERS=1 \
		"$image" "$@"
}

if [ "$execmode" = 1 ]; then
	run "$@"
	exit
fi

status=0
# shellcheck disable=SC2086 # flags and packages are word lists
run go test $flags $pkgs || status=$?

case " $flags " in
*" -run"* | *" -skip"*) onhost=0 ;;
esac
if [ "$onhost" = 1 ]; then
	# The top-level tests that call testpeer.FFmpeg8 directly, per package
	# (a call through a helper would be missed; keep the call in the test).
	for pkg in $pkgs; do
		names=$(find "$pkg" -maxdepth 1 -name '*_test.go' -exec awk '
			FNR == 1 { name = "" }
			match($0, /^func Test[A-Za-z0-9_]+\(/) { name = substr($0, 6, RLENGTH - 6) }
			/testpeer\.FFmpeg8\(/ && name != "" { print name; name = "" }
		' {} + | sort -u | paste -sd '|' -)
		if [ -n "$names" ]; then
			echo "ffmpeg 8 rows on the host ($(ffmpeg -version | head -1)): $pkg $names" >&2
			# shellcheck disable=SC2086 # flags is a word list
			OS_REQUIRE_PEERS=1 go test $flags -run "^($names)\$" "$pkg" || status=$?
		fi
	done
fi
exit "$status"
