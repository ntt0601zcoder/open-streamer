#!/bin/sh
# Fetches the pinned FFmpeg 8.1 development tree (shared libraries, headers
# and pkg-config files) that cmd/ostranscoder and internal/transcode/native
# build against, and prints its prefix on stdout:
#
#   p=$(scripts/ffmpeg-dev.sh amd64)
#   PKG_CONFIG_PATH=$p/lib/pkgconfig go vet -tags libav ./internal/transcode/native/
#
# The source is FFmpeg-Builds release autobuild-2026-10-31-12-59 (FFmpeg
# n8.1.3, GPL, shared). The archive is downloaded once into
# ${OS_FFMPEG_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/osv2-ffmpeg}, its sha256
# is checked before it is unpacked, and the prefix is unpacked next to it.
# Later runs reuse the prefix without network. Every run asserts that
# pkg-config sees libavcodec 62.x under the prefix. The .pc files are
# relocatable (prefix=${pcfiledir}/../..), so the prefix can be mounted
# anywhere, as scripts/test-ci-peers.sh does.
#
# Usage: scripts/ffmpeg-dev.sh [--cached] <amd64|arm64>
#   --cached  print the prefix only if it is already unpacked; never
#             download (exit 1 otherwise, silently)
set -eu

die() {
	echo "ffmpeg-dev: $*" >&2
	exit 1
}

cached=0
if [ "${1:-}" = --cached ]; then
	cached=1
	shift
fi
[ $# -eq 1 ] || die "usage: $0 [--cached] <amd64|arm64>"

release=autobuild-2026-10-31-12-59
case $1 in
amd64)
	asset=ffmpeg-n8.1.3-27-g8dca1586f4-linux64-gpl-shared-8.1.tar.xz
	sum=202fe67679a0860b155817a0907ffcc5c1ea82e186acaa21cb87d307f54fd59a
	;;
arm64)
	asset=ffmpeg-n8.1.3-27-g8dca1586f4-linuxarm64-gpl-shared-8.1.tar.xz
	sum=9d3161962e57f0f621b87c1fe4287ad14b4fd12c74c243436143319e4e14a0a0
	;;
*) die "unknown arch '$1' (want amd64 or arm64)" ;;
esac
url=https://github.com/FFmpeg-Builds/FFmpeg-Builds/releases/download/$release/$asset

cache=${OS_FFMPEG_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/osv2-ffmpeg}
prefix=$cache/${asset%.tar.xz}
# The marker is written last, after a verified unpack, so a prefix without
# it is incomplete.
marker=$prefix/.osv2-verified

if [ ! -f "$marker" ]; then
	[ "$cached" = 0 ] || exit 1
	mkdir -p "$cache"
	# Serialise concurrent runs (parallel worktrees): only one downloads and
	# unpacks; the others wait and then find the marker.
	exec 9>"$cache/.lock"
	flock 9
fi
if [ ! -f "$marker" ]; then
	archive=$cache/$asset
	if [ ! -f "$archive" ] || ! echo "$sum  $archive" | sha256sum -c --status; then
		echo "ffmpeg-dev: downloading $url" >&2
		part=$(mktemp "$cache/download.XXXXXX")
		trap 'rm -f "$part"' EXIT
		curl -fsSL --retry 3 --proto '=https' -o "$part" "$url" || die "download failed: $url"
		echo "$sum  $part" | sha256sum -c --status ||
			die "sha256 mismatch for $asset: got $(sha256sum "$part" | cut -d' ' -f1), want $sum"
		mv "$part" "$archive"
		trap - EXIT
	fi
	tmp=$(mktemp -d "$cache/unpack.XXXXXX")
	trap 'rm -rf "$tmp"' EXIT
	tar -xJf "$archive" -C "$tmp"
	top=$tmp/${asset%.tar.xz}
	[ -d "$top/lib/pkgconfig" ] || die "$asset does not unpack to ${asset%.tar.xz}/lib/pkgconfig"
	touch "$top/.osv2-verified"
	rm -rf "$prefix"
	mv "$top" "$prefix"
	rm -rf "$tmp"
	trap - EXIT
fi

ver=$(PKG_CONFIG_PATH=$prefix/lib/pkgconfig pkg-config --modversion libavcodec) ||
	die "pkg-config cannot find libavcodec under $prefix"
case $ver in
62.*) ;;
*) die "libavcodec under $prefix is $ver, want 62.x" ;;
esac
echo "$prefix"
