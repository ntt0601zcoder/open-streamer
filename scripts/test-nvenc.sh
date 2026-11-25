#!/bin/sh
# The GPU proof of M4c (ruling R-C12), run by make test-nvenc on a host
# with an NVIDIA GPU and its driver. Docker here has no GPU runtime, so
# the NVENC tests run on the host, against the bundle that make
# ostranscoder builds in osv2-ci-peers:m4a; only the VLC row, which needs
# no GPU, runs in that image.
#
#  1. Refuses to start (exit 1, starting nothing and leaving
#     dist/nvenc-report alone) when nvidia-smi reports an NVENC session or
#     a compute process: another process uses the GPU and is not
#     disturbed. The check fails closed: nvidia-smi failing, or a session
#     count that is not an integer ("[N/A]", say), is a refusal too.
#     OS_NVENC_SHARED=1 turns a busy GPU into a warning;
#     TestM4cNVENCSessions then asserts a rise by 3 over the count found.
#  2. Fetches the pinned FFmpeg 8.1 (scripts/ffmpeg-dev.sh), which must
#     list h264_nvenc.
#  3. make ostranscoder; the bundle's -probe nvenc must print "nvenc: ok".
#  4. Checks the GPU again as in step 1 (steps 2 and 3 take minutes), then
#     on the host, with OS_NVENC=1 (the NVENC device tests are opt-in, so
#     that a plain go test on this host never opens a session),
#     OS_REQUIRE_NVENC=1 and OS_REQUIRE_PEERS=1: the
#     TestM4cNVENC* scenarios of internal/e2e (TestM4cNVENCKeep records the
#     B-frame presentation into dist/nvenc-report/media; TestM4cNVENCLongGOP
#     makes its clip with the pinned FFmpeg, OS_NVENC_FFMPEG) and the native
#     NVENC device tests (Task 4).
#  5. In osv2-ci-peers:m4a (VLC 3.0.20): TestM4cNVENCRecordedVLC plays that
#     recording.
#  6. The session count must be back to step 4's.
#  7. Writes dist/nvenc-report/report.md.
# Any failure, and any skipped test in steps 4 and 5, fails the script.
# On every exit the session count is read again, printed and, once the run
# has started (after step 1), recorded in the log; a run that fails after
# step 1 still writes a short report.md saying so, with that count. The log
# of steps 4 and 5 is dist/nvenc-report/test.log. The only containers
# started are make ostranscoder's and step 5's, both osv2-* and --rm; the
# tests bind only ephemeral ports on 127.0.0.1.
#
# Usage: scripts/test-nvenc.sh   (from make test-nvenc)
set -eu

repo=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo"

out=dist/nvenc-report
log=$out/test.log
media=$out/media
started=0  # 1 once step 1 has passed and $out is ours
failure="" # what failed, for the failure report
before=unknown

die() {
	failure=$*
	echo "test-nvenc: $*" >&2
	exit 1
}

command -v nvidia-smi >/dev/null 2>&1 || die "nvidia-smi not found: make test-nvenc runs on the GPU host only"

# sessions prints the NVENC session count summed over the GPUs, or fails
# (status 1, the reason on stderr) when nvidia-smi fails or prints anything
# but one integer per GPU.
sessions() {
	if ! counts=$(nvidia-smi --query-gpu=encoder.stats.sessionCount --format=csv,noheader,nounits); then
		echo "test-nvenc: nvidia-smi failed reading encoder.stats.sessionCount" >&2
		return 1
	fi
	[ -n "$counts" ] || {
		echo "test-nvenc: nvidia-smi printed no session count" >&2
		return 1
	}
	n=0
	set -f # "[N/A]" is not a glob here
	for c in $counts; do
		case $c in
		'' | *[!0-9]*)
			set +f
			echo "test-nvenc: nvidia-smi session count '$c' is not an integer" >&2
			return 1
			;;
		esac
		n=$((n + c))
	done
	set +f
	echo "$n"
}

# gpu_free LABEL: refuses (die) unless the GPU has no NVENC session and no
# compute process, or OS_NVENC_SHARED=1, and sets $before to the count.
gpu_free() {
	n=$(sessions) || die "$1: cannot read the NVENC session count; not running"
	apps=$(nvidia-smi --query-compute-apps=pid,process_name --format=csv,noheader) ||
		die "$1: nvidia-smi failed listing compute processes; not running"
	if [ "$n" != 0 ] || [ -n "$apps" ]; then
		echo "test-nvenc: $1: NVENC sessions: $n" >&2
		echo "test-nvenc: $1: compute processes: ${apps:-none}" >&2
		if [ "${OS_NVENC_SHARED:-}" != 1 ]; then
			die "$1: NVENC is in use by another process; not disturbing it (OS_NVENC_SHARED=1 shares the GPU)"
		fi
		echo "test-nvenc: WARNING: OS_NVENC_SHARED=1: sharing the GPU; the session checks expect a rise by 3" >&2
	fi
	before=$n
	echo "test-nvenc: $1: NVENC sessions: $before" >&2
}

# on_exit STATUS: keeps an interrupted step's output in the log, removes
# the step files, reads and prints the session count (waiting up to 10 s
# for it to fall back to the count before the run, as children stopped by
# a failure or an interrupt close their sessions), and once the run has
# started records it in the log and, when the run failed, writes the
# failure report.
on_exit() {
	status=$1
	trap - EXIT
	if [ "$started" = 1 ] && [ -f "$out/.step.log" ]; then
		cat "$out/.step.log" >>"$log"
	fi
	rm -f "$out/.step.log" "$out/.step.status"
	tries=0
	while :; do
		final=$(sessions 2>/dev/null) || final="unreadable"
		case $final:$before in
		*[!0-9:]* | "$before:$before") break ;;
		esac
		[ "$final" -gt "$before" ] && [ "$tries" -lt 20 ] || break
		tries=$((tries + 1))
		sleep 0.5
	done
	echo "test-nvenc: exit status $status; NVENC sessions after: $final" >&2
	[ "$started" = 1 ] || exit "$status"
	echo "test-nvenc: exit status $status; NVENC sessions after: $final" >>"$log"
	if [ "$status" != 0 ]; then
		{
			echo "# NVENC proof (make test-nvenc): FAILED"
			echo
			echo "- Date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
			echo "- Commit: $(git rev-parse HEAD)$(git diff --quiet HEAD -- || echo ' (with uncommitted changes)')"
			echo "- Exit status: $status"
			echo "- Failure: ${failure:-see test.log}"
			echo "- NVENC sessions: before $before; after $final"
			echo "- Log: $log"
		} >"$out/report.md"
		echo "test-nvenc: wrote the failure report $out/report.md" >&2
	fi
	exit "$status"
}
trap 'on_exit $?' EXIT
trap 'failure="interrupted"; exit 130' INT
trap 'failure="terminated"; exit 143' TERM

# 1. Nothing else may be using the GPU.
gpu_free "before the run"
rm -rf "$out"
mkdir -p "$out"
: >"$log"
started=1

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

# 4. The GPU again, then the NVENC scenarios and the native device tests,
# on the host. The host has no VLC: TestM4cNVENCRecordedVLC runs in step 5.
gpu_free "before the GPU tests"
step "e2e scenarios on the host" env OS_TRANSCODER="$ostx" OS_NVENC=1 OS_REQUIRE_NVENC=1 OS_REQUIRE_PEERS=1 \
	OS_NVENC_MEDIA="$media" OS_NVENC_FFMPEG="$p/bin/ffmpeg" go test -race -count=1 -timeout=15m -v \
	-run '^TestM4cNVENC' -skip '^TestM4cNVENCRecordedVLC$' ./internal/e2e/
step "native device tests on the host" env PKG_CONFIG_PATH="$p/lib/pkgconfig" LD_LIBRARY_PATH="$p/lib" \
	OS_NVENC=1 OS_REQUIRE_NVENC=1 go test -race -count=1 -v -tags libav -run 'NVENC' ./internal/transcode/native/

# 5. VLC 3.0.20 on the recorded presentation, in the peers image.
step "recorded presentation in VLC (osv2-ci-peers:m4a)" env OS_NVENC_MEDIA="$media" \
	sh scripts/test-ci-peers.sh --name "osv2-nvenc-vlc-$$" --exec \
	go test -count=1 -v -run '^TestM4cNVENCRecordedVLC$' ./internal/e2e/

# 6. Every session closed.
after=$(sessions) || die "cannot read the NVENC session count after the run"
echo "test-nvenc: NVENC sessions after: $after" >&2
if [ "$after" != "$before" ]; then
	if [ "${OS_NVENC_SHARED:-}" != 1 ]; then
		die "NVENC sessions: $before before, $after after"
	fi
	echo "test-nvenc: WARNING: NVENC sessions: $before before, $after after (OS_NVENC_SHARED=1)" >&2
fi

# 7. The report: the top-level results first, the subtests folded.
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
	echo "- The two-period DASH cell of h264_aac_then_640.ts (TestM4cNVENCMatrix/noB no-reorder) is read with" \
		"conform instead of ffprobe: ffprobe 8 does not finish reading that MPD."
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
	grep -E -- '^--- (PASS|FAIL|SKIP)' "$log" || true
	echo '```'
	echo
	echo "<details><summary>Subtests</summary>"
	echo
	echo '```'
	grep -E -- '^[[:space:]]+--- (PASS|FAIL|SKIP)' "$log" || true
	echo '```'
	echo
	echo "</details>"
} >"$out/report.md"
echo "test-nvenc: wrote $out/report.md" >&2
