# Open-Streamer Benchmark Report

> Copy this file into `bench/results/<run-id>/report.md` and fill the `<...>` placeholders.
> Every run must include the sample CSV in the same directory so numbers are reproducible.

---

## 0. Metadata

| Field | Value |
|---|---|
| Report ID | `<YYYYMMDD-NN>` |
| Tester | `<name>` |
| Date | `<YYYY-MM-DD>` |
| Open-Streamer version / commit | `<v0.0.x / git sha>` |
| Build flags | `<go build flags>` |
| Config file used | `<path / inline link>` |
| Storage backend | `<json | yaml>` |
| DVR enabled | `<yes / no>` |
| Push-out enabled | `<yes / no>` |
| Multi-output mode | `<yes / no>` |
| Goal of this run | `<e.g. find ABR NVENC ceiling on T4>` |

---

## 1. Hardware & Environment

### 1.1 Host

| Component | Spec |
|---|---|
| Host type | `<bare-metal / KVM-VM / Docker>` |
| OS | `<Ubuntu 24.04.2 LTS>` |
| Kernel | `<6.8.0-56-generic>` |
| CPU model | `<Intel Xeon Platinum 8562Y+>` |
| vCPU count | `<4>` |
| CPU base / turbo | `<2.0 GHz / —>` |
| RAM total | `<16 GB QEMU>` |
| RAM speed | `<Unknown / DDRx>` |
| Swap | `<0 / N GB>` |

### 1.2 Disk

| Mount | Device | FS | Size | Free | Rotational? | Notes |
|---|---|---|---|---|---|---|
| `/` | vda1 | ext4 | 600G | 437G | virtio | DVR also writes here? |

### 1.3 Network

| Interface | Driver | Speed | Tested via `iperf3` |
|---|---|---|---|
| `<eth0>` | `<virtio_net>` | `<unknown>` | `<x Mbps to peer>` |

### 1.4 GPU

| Field | Value |
|---|---|
| Model | `<Tesla T4>` |
| VRAM | `<16384 MiB>` |
| Driver | `<535.288.01>` |
| CUDA version | `<12.2>` |
| Compute capability | `<7.5>` |
| Encoders supported | `<h264_nvenc, hevc_nvenc>` |
| Hwaccels available | `<cuda, vaapi, qsv>` |
| NVENC chip count | `<1>` |
| NVDEC chip count | `<1>` |

### 1.5 FFmpeg

```
<paste: ffmpeg -version | head -3>
<paste: ffmpeg -encoders | grep -E "nvenc|h264_|hevc_">
```

---

## 2. Open-Streamer Configuration

### 2.1 Effective config snapshot

```yaml
# paste: GET /api/v1/config (redact secrets)
```

### 2.2 Stream payloads used

| Profile name | Payload file | Ladder | Mode | Note |
|---|---|---|---|---|
| `passthrough` | `bench/payloads/passthrough.json` | — | copy | HLS only |
| `abr3-legacy` | `bench/payloads/abr3-legacy.json` | 1080p+720p+480p NVENC | legacy | 3 ffmpeg / stream |
| `abr3-multi` | `bench/payloads/abr3-multi.json` | 1080p+720p+480p NVENC | multi-output | 1 ffmpeg / stream |

---

## 3. Methodology

- **Warm-up**: discard the first 60s of every run, do not count toward metrics.
- **Steady window**: measure for 5 minutes (300s).
- **Sample interval**: 2s (`bench/scripts/sample.sh`).
- **Source**: `ffmpeg -re -stream_loop -1 -i bench/assets/sample-1080p.ts ...` (see `bench/scripts/source.sh`).
- **Stop condition (mark run as FAIL)** if any of:
  - Host CPU > 90% sustained for 30s
  - Packet drop appears in log (`buffer hub: dropped`)
  - HLS segment duration jitter > 15% vs `segment_duration_sec`
  - FFmpeg restart counter increases inside the steady window
  - Stream status flips to `Degraded` not driven by test design
- **Repeat each run 3 times**, report the median.

### Tools

| Tool | Purpose |
|---|---|
| `nvidia-smi dmon -s pucvmet -d 2` | GPU/enc/dec util, VRAM, power |
| `pidstat -p <pids> 2` | per-process CPU/mem |
| `ifstat -i <nic> 2` | network throughput |
| `iostat -x 2` | disk util (phase E only) |
| `ffprobe -i <hls>/index.m3u8` | output validation, segment duration |
| `curl /api/v1/runtime` | switch count, restart count |
| `curl /metrics` | Prometheus snapshot |

---

## 4. Results

### 4.1 Phase A — Passthrough

| Run | N stream | Bitrate/stream | Protocols | CPU% (med) | RAM (MB) | Net Tx (Mbps) | Drop | Verdict |
|---|---|---|---|---|---|---|---|---|
| A1 | 1 | 6 Mbps | HLS | | | | | |
| A2 | 10 | 6 Mbps | HLS | | | | | |
| A3 | 25 | 6 Mbps | HLS | | | | | |
| A4 | 50 | 6 Mbps | HLS | | | | | |
| A5 | 25 | 6 Mbps | HLS+DASH+RTMP | | | | | |

**Measured ceiling:** `<N>` passthrough streams before hitting `<bottleneck>`.

**Bottleneck:** `<CPU / NIC / disk / packet drop>`.

### 4.2 Phase B — Legacy ABR transcoding (NVENC)

| Run | N stream | Ladder | CPU% | RAM | GPU% | Enc% | Dec% | VRAM | Restart | Verdict |
|---|---|---|---|---|---|---|---|---|---|---|
| B1 | 1 | 1080p (recode) | | | | | | | | |
| B2 | 1 | 1080p+720p+480p | | | | | | | | |
| B3 | 4 | 1080p+720p+480p | | | | | | | | |
| B4 | 8 | 720p+480p | | | | | | | | |
| B5 | `<N>` | `<ladder>` | | | | | | | | actual ceiling |

**Legacy ABR ceiling:** `<N>` streams × `<ladder>` before `<encoder/decoder/CPU>` hits 95%.

### 4.3 Phase C — Multi-output (same load as B)

| Run | N stream | Ladder | CPU% | RAM | GPU% | Enc% | Dec% | VRAM | vs Legacy |
|---|---|---|---|---|---|---|---|---|---|
| C2 | 1 | 1080p+720p+480p | | | | | | | VRAM `−<X>%`, Dec `−<Y>%` |
| C3 | 4 | 1080p+720p+480p | | | | | | | |
| C4 | 8 | 720p+480p | | | | | | | |
| C5 | `<N>` | `<ladder>` | | | | | | | actual ceiling |

**Capacity gain from multi-output:** `<+X streams>` (`<+Y%>`) vs the same ladder in Phase B.

### 4.4 Phase D — Failover & resilience

| Case | Description | Measure | Result | Verdict |
|---|---|---|---|---|
| D1 | 2 inputs, kill primary | switch latency | `<XXX ms>` | `<PASS / FAIL — target ~150 ms>` |
| D2 | 1 transcode, `kill -9 ffmpeg` | restart latency, viewer downtime | `<XXX s / Y s>` | |
| D3 | Hot-add push destination | HLS viewer continuity | `<no drop / drop>` | |
| D4 | Push to dead sink | `pushes[]` state | `<Reconnecting after N s, max attempts ...>` | |

### 4.5 Phase E — DVR I/O (optional)

| Run | N stream | Bitrate | Retention | Disk %util | Disk wMB/s | Verdict |
|---|---|---|---|---|---|---|
| E1 | 1 | 6 Mbps | 1h | | | |
| E2 | 5 | 6 Mbps | 1h | | | |
| E3 | 10 | 6 Mbps | 1h | | | |

---

## 5. Highlights & Bottlenecks

- **Practical ceilings for this deployment:**
  - Passthrough: `<N>` streams
  - ABR 3-rung legacy: `<N>` streams
  - ABR 3-rung multi-output: `<N>` streams
- **First bottleneck hit:** `<CPU vCPU=4 / NVENC throughput / NVDEC throughput / NIC / disk>`
- **Surprises / notable observations:**
  - `<e.g. multi-output cut VRAM by 38% but Dec util only by 30% due to post-decode colorspace conversion>`
  - `<e.g. enabling HLS+DASH together added 8% CPU per stream>`
  - `<e.g. GPU ECC reported 0 errors throughout the benchmark>`

---

## 6. Issues & Failures

| Severity | Phase / Run | Description | Log evidence | Issue filed? |
|---|---|---|---|---|
| `<P0/P1/P2>` | `<e.g. B4>` | `<e.g. ffmpeg restart loop after 4 minutes>` | `<bench/results/.../streamer.log:1234>` | `<#issue>` |

---

## 7. Recommendations

### 7.1 For production deployments on similar hardware

- Sizing: up to `<N>` ABR streams per VM with this spec.
- Enable `multi_output` when `<condition>`.
- Suggested profile / preset: `<p4 medium / p2 hp>` for the quality/throughput sweet spot.
- DVR config: `<retention <= X hours>` so disk does not become the bottleneck.

### 7.2 For the codebase

- `<e.g. consider raising default subscriber channel size from 1024 to 2048 for HLS pull bursts>`
- `<e.g. add a transcoder_decoder_busy_ratio Prometheus metric to alert on NVDEC saturation>`

---

## 8. Attachments

| File | Description |
|---|---|
| `sysinfo.txt` | output of the 6 hardware-check commands |
| `config.json` | server config snapshot at run start |
| `streams.json` | `GET /api/v1/streams` |
| `runtime-{t0,warmup,mid,end}.json` | `GET /api/v1/runtime` snapshots |
| `sample.csv` | per-2s metrics from `bench/scripts/sample.sh` |
| `nvidia-dmon.log` | raw `nvidia-smi dmon` output |
| `pidstat.log` | per-process CPU/RAM |
| `ifstat.log` | network throughput |
| `iostat.log` | disk util (phase E) |
| `streamer.log` | filtered warn/error from open-streamer |
| `ffprobe-validate.txt` | HLS playlist validation |
| `screenshots/` | nvidia-smi, htop, etc. at key moments |

---

## 9. Sign-off

| Reviewer | Date | Verdict |
|---|---|---|
| `<name>` | `<YYYY-MM-DD>` | `<APPROVE / NEEDS-RERUN / BLOCK>` |
