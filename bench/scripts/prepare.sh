#!/usr/bin/env bash
# One-shot preparation tasks for benchmark runs.
#
# Subcommands:
#   bench/scripts/prepare.sh check     # verify required tools exist
#   bench/scripts/prepare.sh sample    # generate assets/sample-1080p.ts
#   bench/scripts/prepare.sh sysinfo   # snapshot host/GPU info → results/sysinfo.txt
#   bench/scripts/prepare.sh all       # do all three
#
set -euo pipefail

BENCH_ROOT=$(cd "$(dirname "$0")"/.. && pwd)
ASSETS=$BENCH_ROOT/assets
RESULTS=$BENCH_ROOT/results

cmd_check() {
  local missing=0
  for bin in ffmpeg ffprobe curl awk pgrep ps ip; do
    if ! command -v "$bin" >/dev/null; then
      echo "  MISSING: $bin"
      missing=$((missing + 1))
    else
      echo "  ok      $bin → $(command -v "$bin")"
    fi
  done
  for bin in nvidia-smi pidstat ifstat iostat dmidecode jq; do
    if ! command -v "$bin" >/dev/null; then
      echo "  optional: $bin (recommended)"
    else
      echo "  ok      $bin"
    fi
  done
  [[ $missing -eq 0 ]] || { echo "[prepare] $missing required tool(s) missing"; exit 1; }
}

cmd_sample() {
  mkdir -p "$ASSETS"
  local out=$ASSETS/sample-1080p.ts
  if [[ -f "$out" ]]; then
    echo "[prepare] $out already exists ($(du -h "$out" | cut -f1))"
    return 0
  fi
  echo "[prepare] generating 60s 1080p30 6Mbps test pattern → $out"
  ffmpeg -hide_banner -loglevel warning -y \
    -f lavfi -i "testsrc2=size=1920x1080:rate=30" \
    -f lavfi -i "sine=frequency=1000" \
    -c:v libx264 -preset fast -b:v 6M -g 60 \
    -c:a aac -b:a 128k -t 60 \
    -f mpegts "$out"
  echo "[prepare] done: $(du -h "$out" | cut -f1)"
}

cmd_sysinfo() {
  mkdir -p "$RESULTS"
  local out=$RESULTS/sysinfo-$(date +%Y%m%d-%H%M%S).txt
  echo "[prepare] writing $out"
  {
    echo "===== date ====="
    date -Iseconds
    echo
    echo "===== os ====="
    lsb_release -a 2>/dev/null || cat /etc/os-release
    uname -a
    echo
    echo "===== cpu ====="
    lscpu 2>/dev/null | grep -E "Model name|Socket|Core|Thread|MHz|Cache|Vendor|Hypervisor"
    echo
    echo "===== memory ====="
    free -h
    sudo dmidecode -t memory 2>/dev/null | grep -E "Size|Speed|Type:|Manufacturer" | head -40 || echo "(dmidecode unavailable)"
    echo
    echo "===== disk ====="
    lsblk -o NAME,SIZE,TYPE,ROTA,MODEL
    df -hT
    echo
    echo "===== network ====="
    ip -br addr
    nic=$(ip route get 1 2>/dev/null | awk '{print $5; exit}')
    [[ -n "$nic" ]] && ethtool "$nic" 2>/dev/null | grep -E "Speed|Duplex" || echo "(ethtool unavailable for $nic)"
    echo
    echo "===== gpu ====="
    if command -v nvidia-smi >/dev/null; then
      nvidia-smi
      nvidia-smi --query-gpu=name,driver_version,memory.total,compute_cap --format=csv
    else
      echo "(nvidia-smi unavailable)"
    fi
    echo
    echo "===== ffmpeg ====="
    ffmpeg -version 2>&1 | head -3
    echo "--- encoders ---"
    ffmpeg -hide_banner -encoders 2>/dev/null | grep -E "nvenc|h264_|hevc_"
    echo "--- hwaccels ---"
    ffmpeg -hide_banner -hwaccels 2>/dev/null
  } >"$out" 2>&1
  echo "[prepare] sysinfo captured: $out"
}

case ${1:-all} in
  check)   cmd_check ;;
  sample)  cmd_sample ;;
  sysinfo) cmd_sysinfo ;;
  all)     cmd_check; cmd_sample; cmd_sysinfo ;;
  *)
    echo "Usage: $0 [check|sample|sysinfo|all]"
    exit 1
    ;;
esac
