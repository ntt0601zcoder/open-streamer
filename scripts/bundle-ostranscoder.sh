#!/bin/sh
# Builds cmd/ostranscoder against the pinned FFmpeg 8.1 (scripts/
# ffmpeg-dev.sh) and bundles it with the FFmpeg shared libraries it needs:
#
#   <outdir>/ostranscoder
#   <outdir>/lib/<soname>   one per library ldd resolves inside the prefix
#   <outdir>/LICENSE-ffmpeg/ FFmpeg's LICENSE* and COPYING* files from the
#                            prefix (the libraries are the GPL build)
#   <outdir>/LICENSE-dejavu  the licence of DejaVu Sans, which the binary
#                            embeds for text watermarks
#
# The binary carries DT_RPATH $ORIGIN/lib (-r with --disable-new-dtags), so
# the bundle runs from any directory with no LD_LIBRARY_PATH; only glibc
# comes from the system. Build it on Ubuntu 24.04 (glibc 2.39), as CI does
# and as `make ostranscoder` does inside osv2-ci-peers:m4a, so that it runs
# on every system with glibc >= 2.39. Any library ldd cannot find fails the
# build. The arch must be the host's: there is no cross build.
#
# Usage: scripts/bundle-ostranscoder.sh <amd64|arm64> <outdir>
set -eu

die() {
	echo "bundle-ostranscoder: $*" >&2
	exit 1
}

[ $# -eq 2 ] || die "usage: $0 <amd64|arm64> <outdir>"
arch=$1
out=$2
repo=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo"

[ -d cmd/ostranscoder ] ||
	die "./cmd/ostranscoder does not exist yet (M4a Task 3 adds it); nothing to bundle"

case $(uname -m) in
x86_64) host=amd64 ;;
aarch64 | arm64) host=arm64 ;;
*) host=$(uname -m) ;;
esac
[ "$arch" = "$host" ] || die "cannot build $arch on a $host host"

prefix=$(sh scripts/ffmpeg-dev.sh "$arch")
export PKG_CONFIG_PATH="$prefix/lib/pkgconfig" CGO_ENABLED=1

# lib/ must be empty while ldd runs below, so that DT_RPATH finds nothing
# there and LD_LIBRARY_PATH resolves every library inside the prefix.
rm -rf "$out"
mkdir -p "$out/lib"
# shellcheck disable=SC2016 # $ORIGIN is for the dynamic linker
go build -tags libav -trimpath -ldflags '-r $ORIGIN/lib -extldflags=-Wl,--disable-new-dtags' \
	-o "$out/ostranscoder" ./cmd/ostranscoder

deps=$(LD_LIBRARY_PATH="$prefix/lib" ldd "$out/ostranscoder")
if echo "$deps" | grep -q 'not found'; then
	echo "$deps" >&2
	die "unresolved libraries"
fi
echo "$deps" | awk -v lib="$prefix/lib/" '$2 == "=>" && index($3, lib) == 1 { print $1, $3 }' |
	while read -r soname path; do
		cp -L "$path" "$out/lib/$soname"
	done
[ -n "$(ls "$out/lib")" ] || die "no library resolved inside $prefix"

# The libraries are FFmpeg-Builds' GPL build: their licence goes with them.
mkdir -p "$out/LICENSE-ffmpeg"
for f in "$prefix"/LICENSE* "$prefix"/COPYING*; do
	if [ -f "$f" ]; then
		cp -L "$f" "$out/LICENSE-ffmpeg/"
	fi
done
[ -n "$(ls "$out/LICENSE-ffmpeg")" ] || die "no LICENSE* or COPYING* file in $prefix"

# The binary embeds DejaVu Sans (internal/transcode/native/font): its
# licence goes with it.
cp internal/transcode/native/font/LICENSE "$out/LICENSE-dejavu"

# The bundle alone, with no LD_LIBRARY_PATH: every library resolves, and
# every bundled one from lib/.
deps=$(env -u LD_LIBRARY_PATH ldd "$out/ostranscoder")
if echo "$deps" | grep -q 'not found'; then
	echo "$deps" >&2
	die "the bundle does not resolve its libraries"
fi
for f in "$out"/lib/*; do
	soname=$(basename "$f")
	got=$(echo "$deps" | awk -v so="$soname" '$1 == so && $2 == "=>" { print $3 }')
	[ -n "$got" ] && [ "$(realpath "$got")" = "$(realpath "$f")" ] ||
		die "$soname resolves to '${got:-nothing}', not $f"
done
echo "$out/ostranscoder"
