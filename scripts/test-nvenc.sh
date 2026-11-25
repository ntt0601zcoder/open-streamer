#!/bin/sh
# The GPU proof of M4c (ruling R-C12), run by make test-nvenc on a host
# with an NVIDIA GPU and its driver. Docker here has no GPU runtime, so
# the NVENC tests run on the host, against the bundle that make
# ostranscoder builds in osv2-ci-peers:m4a; only the VLC row, which needs
# no GPU, runs in that image.
#
#  1. Refuses to start (exit 1, starting nothing) when nvidia-smi reports
#     an NVENC session or a compute process: another process uses the GPU
#     and is not disturbed. OS_NVENC_SHARED=1 turns that into a warning;
#     TestM4cNVENCSessions then asserts a rise by 3 over the count found.
#  2. Fetches the pinned FFmpeg 8.1 (scripts/ffmpeg-dev.sh), which must
#     list h264_nvenc.
#  3. make ostranscoder; the bundle's -probe nvenc must print "nvenc: ok".
#  4. On the host, with OS_REQUIRE_NVENC=1 and OS_REQUIRE_PEERS=1: the
#     TestM4cNVENC* scenarios of internal/e2e (TestM4cNVENCKeep records the
#     B-frame presentation into dist/nvenc-report/media) and the native
#     NVENC device tests (Task 4).
#  5. In osv2-ci-peers:m4a (VLC 3.0.20): TestM4cNVENCRecordedVLC plays that
#     recording.
#  6. The session count must be back to step 1's.
#  7. Writes dist/nvenc-report/report.md.
# Any failure, and any skipped test in steps 4 and 5, fails the script.
# The log of steps 4 and 5 is dist/nvenc-report/test.log. The only
# containers started are make ostranscoder's and step 5's, both osv2-*
# and --rm; the tests bind only ephemeral ports on 127.0.0.1.
#
# Usage: scripts/test-nvenc.sh   (from make test-nvenc)
set -eu

repo=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo"

die() {
	echo "test-nvenc: $*" >&2
	exit 1
}

command -v nvidia-smi >/dev/null 2>&1 || die "nvidia-smi not found: make test-nvenc runs on the GPU host only"

# sessions prints the NVENC session count summed over the GPUs.
sessions() {
	nvidia-smi --query-gpu=encoder.stats.sessionCount --format=csv,noheader,nounits |
		awk '{ n += $1 } END { print n + 0 }'
}

# 1. Nothing else may be using the GPU.
before=$(sessions)
apps=$(nvidia-smi --query-compute-apps=pid,process_name --format=csv,noheader)
if [ "$before" != 0 ] || [ -n "$apps" ]; then
	echo "test-nvenc: NVENC sessions: $before" >&2
	echo "test-nvenc: compute processes: ${apps:-none}" >&2
	if [ "${OS_NVENC_SHARED:-}" != 1 ]; then
		die "NVENC is in use by another process; not disturbing it (OS_NVENC_SHARED=1 shares the GPU)"
	fi
	echo "test-nvenc: WARNING: OS_NVENC_SHARED=1: sharing the GPU; the session checks expect a rise by 3" >&2
fi
echo "test-nvenc: NVENC sessions before: $before" >&2

out=dist/nvenc-report
log=$out/test.log
media=$out/media
rm -rf "$out"
mkdir -p "$out"
: >"$log"

# 2. The pinned FFmpeg, with h264_nvenc.
p=$(sh scripts/ffmpeg-dev.sh amd64)
LD_LIBRARY_PATH=$p/lib "$p/bin/ffmpeg" -hide_banner -encoders 2>/dev/null | grep -qw h264_nvenc ||
	die "the pinned FFmpeg ($p) does not list h264_nvenc"

# 3. The bundle, and a device it can open.
make ostranscoder
ostx=$repo/dist/ostranscoder-amd64/ostranscoder
probe=$("$ostx" -probe nvenc 2>&1) || true
[ "$probe" = "nvenc: ok" ] || die "$ostx -probe nvenc: $probe"

# step LABEL CMD...: runs CMD with its output shown and kept in the log,
# and fails the script when CMD fails or any test skipped. (POSIX sh has no
# pipefail: CMD's status goes through a file.)
step() {
	label=$1
	shift
	part=$out/.step.log
	status=$out/.step.status
	echo "=== test-nvenc: $label: $*" | tee -a "$log"
	{
		rc=0
		"$@" || rc=$?
		echo "$rc" >"$status"
	} 2>&1 | tee "$part"
	cat "$part" >>"$log"
	rc=$(cat "$status")
	skips=$(grep -- '--- SKIP' "$part" || true)
	rm -f "$part" "$status"
	[ "$rc" = 0 ] || die "$label failed (exit $rc); see $log"
	[ -z "$skips" ] || die "$label skipped tests; see $log:
$skips"
}

# 4. The NVENC scenarios and the native device tests, on the host. The host
# has no VLC: TestM4cNVENCRecordedVLC runs in step 5.
step "e2e scenarios on the host" env OS_TRANSCODER="$ostx" OS_REQUIRE_NVENC=1 OS_REQUIRE_PEERS=1 \
	OS_NVENC_MEDIA="$media" go test -race -count=1 -timeout=15m -v -run '^TestM4cNVENC' \
	-skip '^TestM4cNVENCRecordedVLC$' ./internal/e2e/
step "native device tests on the host" env PKG_CONFIG_PATH="$p/lib/pkgconfig" LD_LIBRARY_PATH="$p/lib" \
	OS_REQUIRE_NVENC=1 go test -race -count=1 -v -tags libav -run 'NVENC' ./internal/transcode/native/

# 5. VLC 3.0.20 on the recorded presentation, in the peers image.
step "recorded presentation in VLC (osv2-ci-peers:m4a)" env OS_NVENC_MEDIA="$media" \
	sh scripts/test-ci-peers.sh --name "osv2-nvenc-vlc-$$" --exec \
	go test -count=1 -v -run '^TestM4cNVENCRecordedVLC$' ./internal/e2e/

# 6. Every session closed.
after=$(sessions)
echo "test-nvenc: NVENC sessions after: $after" >&2
if [ "$after" != "$before" ]; then
	if [ "${OS_NVENC_SHARED:-}" != 1 ]; then
		die "NVENC sessions: $before before, $after after"
	fi
	echo "test-nvenc: WARNING: NVENC sessions: $before before, $after after (OS_NVENC_SHARED=1)" >&2
fi

# 7. The report.
during=$(sed -n 's/.*nvenc-sessions \(.*\)/\1/p' "$log")
{
	echo "# NVENC proof (make test-nvenc)"
	echo
	echo "- Date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
	echo "- GPU: $(nvidia-smi --query-gpu=name,driver_version,memory.total --format=csv,noheader)"
	echo "- FFmpeg: $(basename "$p").tar.xz"
	echo "- ostranscoder: $("$ostx" -version 2>&1 | head -1)"
	echo "- Commit: $(git rev-parse HEAD)$(git diff --quiet HEAD -- || echo ' (with uncommitted changes)')"
	echo "- NVENC sessions: before $before; during (TestM4cNVENCSessions) ${during:-not logged}; after $after"
	if [ "${OS_NVENC_SHARED:-}" = 1 ]; then
		echo "- OS_NVENC_SHARED=1: the GPU was shared with other processes"
	fi
	echo
	echo "## NVENC rungs (TestM4cNVENCProbe, h264_aac.ts)"
	echo
	echo '```'
	sed -n 's/.*\(nvenc-rung .*\)/\1/p' "$log"
	echo '```'
	echo
	echo "## Results"
	echo
	echo '```'
	grep -E -- '--- (PASS|FAIL|SKIP)' "$log" || true
	echo '```'
} >"$out/report.md"
echo "test-nvenc: wrote $out/report.md" >&2
