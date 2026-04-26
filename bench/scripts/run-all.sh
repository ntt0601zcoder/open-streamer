#!/usr/bin/env bash
# Full benchmark sweep — runs Phase A/B/C end-to-end, summarises each run,
# and aggregates everything into one master report.
#
# Phase D (failover) and Phase E (DVR) require scenario-specific manipulation
# and are intentionally skipped here — run them manually after this completes.
#
# Usage:
#   bench/scripts/run-all.sh                # SWEEP = <date>-<gpu>-<git-sha>
#   bench/scripts/run-all.sh stress-v2      # SWEEP = <date>-<gpu>-stress-v2
#   NOTE=baseline bench/scripts/run-all.sh  # same as positional arg
#   SWEEP=manual-name bench/scripts/run-all.sh   # full override
#
# Auto-composed name examples:
#   2026-04-27-T4-2013a9a       (git tracked, no note arg)
#   2026-04-27-T4-baseline      (note arg = "baseline")
#   2026-04-27-RTX-4090-stress  (RTX 4090 host)
#   2026-04-27-nogpu-2013a9a    (no nvidia-smi)
#
# Resume / partial:
#   PLAN="A2 B3 C3" bench/scripts/run-all.sh
#
set -euo pipefail

BENCH_ROOT=$(cd "$(dirname "$0")"/.. && pwd)
SCRIPTS=$BENCH_ROOT/scripts

detect_gpu_tag() {
  command -v nvidia-smi >/dev/null || { echo "nogpu"; return; }
  local name
  name=$(nvidia-smi --query-gpu=name --format=csv,noheader 2>/dev/null | head -1) || true
  [[ -z "$name" ]] && { echo "nogpu"; return; }
  echo "$name" \
    | sed -E 's/^Tesla[[:space:]]+//; s/^NVIDIA[[:space:]]+//; s/^GeForce[[:space:]]+//' \
    | tr -s '[:space:]' '-' \
    | sed -E 's/-+/-/g; s/^-//; s/-$//'
}

auto_note() {
  (cd "$BENCH_ROOT/.." && git rev-parse --short HEAD 2>/dev/null) || echo "manual"
}

# Compose SWEEP if user did not pin it explicitly
if [[ -z "${SWEEP:-}" ]]; then
  DATE=$(date +%Y-%m-%d)
  GPU=$(detect_gpu_tag)
  NOTE=${NOTE:-${1:-$(auto_note)}}
  SWEEP="$DATE-$GPU-$NOTE"
fi

COOLDOWN=${COOLDOWN:-30}
API=${API:-http://127.0.0.1:8080/api/v1}
SKIP_FAILOVER=${SKIP_FAILOVER:-0}
LOGDIR=$BENCH_ROOT/results/$SWEEP
LOG=$LOGDIR/run-all.log

mkdir -p "$LOGDIR"

log()  { echo "[$(date +%H:%M:%S)] $*" | tee -a "$LOG"; }
fail() { log "FATAL: $*"; exit 1; }

api_check() {
  curl -fs --max-time 5 "$API/streams" >/dev/null \
    || fail "open-streamer not responding at $API"
}

set_multi_output() {
  local val=$1
  log "config → multi_output=$val"
  curl -fs -X PUT "$API/config" -H 'Content-Type: application/json' \
    -d "{\"transcoder\":{\"multi_output\":$val}}" >/dev/null \
    || log "WARN: failed to toggle multi_output (continuing)"
}

# Full plan — (id N profile [pre-hook])
declare -a PLAN_ALL=(
  # Phase A — passthrough
  "A1 1  passthrough  noop"
  "A2 10 passthrough  noop"
  "A3 25 passthrough  noop"
  # Phase B — legacy ABR
  "B1 1  abr3-legacy  legacy"
  "B2 1  abr3-legacy  legacy"
  "B3 4  abr3-legacy  legacy"
  "B4 8  abr3-legacy  legacy"
  # Phase C — multi-output
  "C2 1  abr3-multi   multi"
  "C3 4  abr3-multi   multi"
  "C4 8  abr3-multi   multi"
)

# Filter by env PLAN if provided (space-separated run IDs)
declare -a PLAN
if [[ -n "${PLAN:-}" ]]; then
  for line in "${PLAN_ALL[@]}"; do
    id=${line%% *}
    [[ " $PLAN " == *" $id "* ]] && PLAN+=("$line")
  done
else
  PLAN=("${PLAN_ALL[@]}")
fi

run_one() {
  local id=$1 n=$2 profile=$3 hook=$4
  log "=== $id  N=$n  profile=$profile  ==="
  api_check

  case "$hook" in
    legacy) set_multi_output false ;;
    multi)  set_multi_output true ;;
    *) ;;
  esac

  if "$SCRIPTS"/run-bench.sh "$id" "$n" "$profile" >>"$LOG" 2>&1; then
    log "  run completed"
  else
    log "  RUN FAILED — see $LOG (continuing sweep)"
  fi

  if "$SCRIPTS"/summarize.sh "$id" >>"$LOG" 2>&1; then
    log "  summary: results/$id/summary.md"
  else
    log "  WARN: summarize failed for $id"
  fi

  log "  cooldown ${COOLDOWN}s..."
  sleep "$COOLDOWN"
}

# ===== preflight =====
log "=== sweep '$SWEEP' starting (${#PLAN[@]} runs) ==="
log "  log:    $LOG"
log "  api:    $API"
log "  report: $BENCH_ROOT/reports/$SWEEP/report.md (generated at end)"
api_check

if [[ ! -f "$BENCH_ROOT/assets/sample-1080p.ts" ]]; then
  log "missing assets/sample-1080p.ts → running prepare.sh sample"
  "$SCRIPTS"/prepare.sh sample >>"$LOG" 2>&1 || fail "prepare.sh sample failed"
fi

# Capture sysinfo once for the whole sweep
"$SCRIPTS"/prepare.sh sysinfo >>"$LOG" 2>&1
mv "$BENCH_ROOT"/results/sysinfo-*.txt "$LOGDIR/sysinfo.txt" 2>/dev/null || true

# ===== execute plan =====
START=$(date +%s)
for line in "${PLAN[@]}"; do
  read -r id n profile hook <<<"$line"
  run_one "$id" "$n" "$profile" "$hook"
done

# Reset config to baseline
set_multi_output false

# ===== Phase D — failover =====
if [[ "$SKIP_FAILOVER" != "1" ]]; then
  log "=== Phase D — failover scenarios ==="
  if "$SCRIPTS"/run-failover.sh all >>"$LOG" 2>&1; then
    log "  failover scenarios completed"
  else
    log "  WARN: one or more failover cases failed — see $LOG"
  fi
fi

# ===== aggregate =====
log "=== aggregating master report ==="
if "$SCRIPTS"/aggregate.sh "$SWEEP" >>"$LOG" 2>&1; then
  log "report: bench/reports/$SWEEP/report.md (committable)"
else
  log "WARN: aggregate.sh failed — per-run summaries still available"
fi

DUR=$(( $(date +%s) - START ))
log "=== sweep complete in $((DUR / 60))m $((DUR % 60))s ==="
log
log "Committable report:  $BENCH_ROOT/reports/$SWEEP/report.md"
log "Local raw artifacts: $LOGDIR/  (gitignored)"
