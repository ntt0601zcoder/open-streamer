# Open Streamer — Soak Test Plan

**Status:** proposed; the tooling in [§10](#10-deliverables) does not exist yet.
Thresholds marked *(initial)* are starting points to adjust after the first run.

This plan answers one question: do the streams and their outputs keep working
when left running for days? A 10-minute check shows that a configuration starts.
It cannot show a slow leak, a timestamp wrap, or an output that degrades on day
four.

Fault injection is deliberately out of scope here. See
[§12](#12-deferred-to-a-later-plan).

## 1. What this plan looks for

| Failure class | Example | How it is exposed |
|---|---|---|
| Output stops working | An output returns errors or stalls after some days | Output probes throughout the run |
| Resource leaks | Go heap, libav memory, goroutines, file descriptors, NVENC sessions | Trends over days |
| Time-dependent bugs | MPEG-TS PTS wraps every 26.5 h; a past wrap took down every transcoded stream | Runs longer than 27 h on every path |
| Accumulating drift | A/V offset, DASH timeline, segment timing | Periodic output probes |
| Unbounded growth | Disk, logs, metric series | Daily trend |

Out of scope: fault injection, S3 input (needs an endpoint), load testing to
saturation, and GPU encoders other than NVENC.

## 2. Test environment

| Item | Value |
|---|---|
| Host | Ryzen 7 3750H (4c/8t), about 15 GB RAM, GTX 1650, NVMe with 380 GB free |
| Server | `open-streamer` as a systemd unit, build `main-dd4e561` |
| NVENC limit | 12 concurrent sessions worked; 16 failed |
| Observability | Prometheus (15 s, 30 d), Pyroscope (30 d), VictoriaLogs (30 d, 10 GiB) |

```
ffmpeg (loops clip, -c copy) → MediaMTX → Open Streamer → probes
```

MediaMTX is an independent source, so the server is not only tested against
itself. It runs in its own compose project with a pinned version, so a homelab
deploy cannot interrupt a run. Its RTMP port moves off 1935, and `hlsVariant`
must be `mpegts` because the HLS reader expects MPEG-TS.

| Port | Service |
|---|---|
| 8080, 1935, 5554, 9999 | Open Streamer HTTP, RTMP, RTSP, SRT |
| 8554, 1936, 8888, 8890 | MediaMTX RTSP, RTMP, HLS, SRT |

## 3. Test media

1080p25 H.264 High, 4 Mbps, GOP 50, 2 B-frames, AAC-LC 44.1 kHz stereo, 291 s,
played in a loop. It is 25 fps because the capacity figures in this plan were
measured at that rate.

## 4. Coverage axes

### Inputs

| ID | Input | Path | Source | URL form |
|---|---|---|---|---|
| I1 | RTMP push | AV | ffmpeg reads MediaMTX and publishes | `publish://` |
| I2 | RTMP pull | AV | MediaMTX | `rtmp://…` |
| I3 | RTSP pull | AV | MediaMTX | `rtsp://…` |
| I4 | `copy://` | AV | Another stream | `copy://<code>` |
| I5 | `mixer://` | AV | Two other streams | `mixer://<video>?audio=<audio>` |
| I6 | SRT pull | Raw TS | MediaMTX | `srt://…?streamid=read:<path>` |
| I7 | UDP | Raw TS | ffmpeg reads MediaMTX, sends MPEG-TS | `udp://…` |
| I8 | HLS pull | Raw TS | MediaMTX | `http://…/index.m3u8` |
| I9 | HTTP-TS | Raw TS | ffmpeg reads MediaMTX, serves HTTP | `http://…` |
| I10 | File | Raw TS | VOD mount | `file://<mount>/<file>?loop=true` |

### Transcode profiles

All video encoding uses the GPU. Audio re-encoding always runs on the CPU.

| ID | Config | NVENC per stream | Sessions |
|---|---|---|---|
| T0 | No transcoder | 0 | 0 |
| T1 | Video copy, audio copy | 0 | 0 |
| T2 | Video copy, AAC re-encode | 0 | 0 |
| T3 | NVENC H.264, 720p | about 12% (derived from T4) | 1 |
| T4 | NVENC H.264, 1080p + 720p + 480p | 43% | 3 |
| T5 | NVENC H.265, 720p | about 3% (likely optimistic) | 1 |

### Outputs

Every stream enables all of them.

| ID | Output | URL |
|---|---|---|
| O1 | HLS | `http://host:8080/<code>/index.m3u8` |
| O2 | DASH | `http://host:8080/<code>/index.mpd` |
| O3 | RTMP | `rtmp://host:1935/live/<code>` |
| O4 | RTSP | `rtsp://host:5554/live/<code>` |
| O5 | SRT | `srt://host:9999?streamid=live/<code>` |
| O6 | HTTP-TS | `http://host:8080/<code>/mpegts` |
| O7 | RTMP push-out | To a local receiver |

## 5. Test suites

| Suite | Purpose | Duration |
|---|---|---|
| CONF | Every planned input, profile and output combination starts and plays | About 25 min |
| SMOKE | Validate the tooling on the long-run stream set | 2 h |
| LONG-RUN | The long-run stream set runs untouched | To 72 h, then to 7 days |

## 6. Suite CONF: conformance matrix

28 streams and 196 output checks, in batches sized to the host. Two batched T4
streams use about 86% of NVENC, so if frames drop, T4 runs one stream per batch.

| Input | T0 | T1 | T2 | T3 | T4 | T5 |
|---|---|---|---|---|---|---|
| I1 RTMP push | B1 | B1 | B1 | B3 | B4 | B3 |
| I2 RTMP pull | B1 | – | – | – | B5 | – |
| I3 RTSP pull | B1 | – | – | – | B5 | – |
| I4 `copy://` | B1 | – | – | – | B6 | – |
| I5 `mixer://` | B1 | – | – | – | B6 | – |
| I6 SRT pull | B2 | B2 | B2 | B3 | B4 | B3 |
| I7 UDP | B2 | – | – | – | B7 | – |
| I8 HLS pull | B2 | – | – | – | B7 | – |
| I9 HTTP-TS | B2 | – | – | – | B8 | – |
| I10 File | B2 | – | – | – | B8 | – |

I1 and I6 cover all profiles because they represent the AV and raw-TS ingest
paths.

**Pass:** status is `active`, `restart_count` is 0, and on every output `ffprobe`
reads the expected codec and resolution for 30 s with video and audio present.
Backwards DTS and buffer drops are recorded as warnings.

## 7. Suite LONG-RUN: stream set

About 58% NVENC at steady state. Every stream enables all outputs. Nothing is
changed or disturbed during the run.

| # | Input | Transcode | Purpose |
|---|---|---|---|
| 1 | I1 RTMP push | T4 | Full ladder on the AV path |
| 2 | I6 SRT pull | T3 | Raw TS with transcoding |
| 3 | I3 RTSP pull | T5 | AV path with HEVC |
| 4 | I8 HLS pull | T0 | Long-running playlist polling |
| 5 | I7 UDP | T0 | Raw TS passthrough |
| 6 | I9 HTTP-TS | T2 | Audio re-encode with video copy |
| 7 | I10 File | T1 | Loop boundaries |
| 8 | I4 `copy://` | T0 | In-process subscription |
| 9 | I5 `mixer://` | T0 | A/V drift |
| 10 | I2 RTMP pull | T0 | AV passthrough |

**Output checks during the run**

| Check | Frequency | What it shows |
|---|---|---|
| Short `ffprobe` on every output of every stream, staggered | Every 5 min | Output still serves the right codec, resolution, video and audio |
| One long-held reader per output protocol, on streams 1 and 2 | Whole run | A connection stays healthy for days |
| Push-out receiver for each stream | Whole run | Push-out keeps delivering |

## 8. Signals and pass criteria

| Signal | Source | Pass *(initial)* |
|---|---|---|
| Stream status | `GET /streams` | `active` for the whole run |
| Restarts and failures | `transcoder_restarts_total`, `transcoder_errors_total`, `ingestor_errors_total` | No increase |
| Output probe | Scheduled `ffprobe` | 100% pass |
| Long-held readers | Reader logs | No unexpected disconnect |
| Server RSS, heap, goroutines, FDs | `process_*`, `go_*` | After 6 h warm-up, growth under 5% per day |
| Transcoder RSS, threads | `transcoder_process_*` | Same |
| NVENC sessions | GPU exporter | Equals the configured rung count |
| Metric series | Prometheus | Stable |
| Disk | Node exporter, `du` | HLS and DASH directories bounded |
| Log volume | VictoriaLogs | No message over 1,000 lines per hour |
| A/V offset | Output probes | Within 500 ms, and not trending |
| Segment cadence | `publisher_segments_total` | Matches the configured duration |
| Buffer drops | `buffer_drops_total` | 0 |
| Frames | `transcoder_frames_total`, `decoded_frames_total` | Output tracks source FPS |

**Severity**

| Level | Meaning |
|---|---|
| S1 | Crash, hang, or a stream or output that stops and needs manual action |
| S2 | Leak, or an output that degrades but keeps running |
| S3 | Output defects such as backwards DTS |
| S4 | Log noise or wrong metrics |

A run passes with no S1 or S2 findings. If MediaMTX or a publisher fails, the
event is recorded as an environment incident and is not counted against Open
Streamer.

## 9. Schedule and preconditions

| Phase | Time | Stop and review if |
|---|---|---|
| CONF | 25 min | Any output fails on most streams |
| SMOKE | 2 h | The tooling misreports |
| LONG-RUN | To 72 h | Any S1 |
| Checkpoint | Hour 72 | Review, then decide whether to continue |
| LONG-RUN | To day 7 | Any S1 |

- [ ] The laptop stays on AC with the lid open. Closing the lid suspends it.
- [ ] No Open Streamer deploy, and no host reboot, during a run.
- [ ] Core dumps enabled with `systemd-coredump`. Apport drops dumps from
      binaries not installed from a package.
- [ ] The tooling runs as its own service and stores data outside `/tmp`.

## 10. Deliverables

| Item | Description |
|---|---|
| `bench/soak/` | Compose file for MediaMTX, publishers, probes and reporting |
| Daily report | Trends against [§8](#8-signals-and-pass-criteria), findings by severity |
| Final report | Verdict, findings with evidence, heap and goroutine profile diffs |

## 11. Known issues to watch

From code review and a one-stream pilot.

| Issue | Status |
|---|---|
| DASH output had 4 backwards video DTS in 30 s, with `dash: video OOO push` warnings, on a source with B-frames | Probably a real defect |
| `dash: video OOO push` logged 92,818 times in 24 h on an earlier run, none with `stream_code` | Log flood |
| `ffprobe` timed out reading HTTP-TS | Cause unknown |
| T5 (H.265) on RTMP, RTSP and SRT outputs and push-out | Untested; no H.265-specific handling found |
| `buffer_drops_total` does not identify the slow consumer | Limits diagnosis |

## 12. Deferred to a later plan

Not tested here, to be planned separately once the streams and outputs are shown
to run stably:

- Source failover and failback
- Source stall, outages and flapping
- Low bitrate and bitrate spikes
- Transcoder restarts and crashes
- Corrupted streams, sparse keyframes and timestamp anomalies
- Network loss, jitter and bandwidth limits
- Push-out destination failures
- Live configuration changes
- Viewer churn and slow readers
- Disk, CPU and GPU pressure
- DVR retention
