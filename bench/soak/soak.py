#!/usr/bin/env python3
"""Soak test driver for a local Open Streamer. See docs/SOAK_TEST_PLAN.md.

    soak.py conf [--batch B1 B4]     conformance matrix
    soak.py long [--hours 168] [--no-checks]   long-run stream set, left undisturbed
    soak.py report RUN_DIR           rebuild report.md for a run

Python standard library plus ffmpeg, ffprobe, curl, docker and nvidia-smi.
Every stream it creates is removed on exit.
"""
import argparse
import concurrent.futures as cf
import json
import os
import re
import signal
import statistics
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from collections import Counter, defaultdict

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_DATA = os.path.expanduser("~/open-streamer-soak")

API = "http://127.0.0.1:8080"
MTX_API = "http://127.0.0.1:9997"
OS_RTMP, OS_RTSP, OS_SRT = 1935, 5554, 9999
# Not loopback: Open Streamer treats rtmp:// and srt:// URLs on a loopback
# address as "listen for a push" instead of pulling from them.
MTX_HOST = "172.17.0.1"
MTX_RTSP, MTX_RTMP, MTX_HLS, MTX_SRT = 8554, 1936, 8888, 8890
SOURCE = f"rtsp://{MTX_HOST}:{MTX_RTSP}/live/main"
VOD = "soak-vod"
WARMUP_HOURS = 6

AUDIO_REENC = {"copy": False, "codec": "aac", "bitrate": 128, "sample_rate": 48000, "channels": 2}
NVENC = {"global": {"hw": "nvenc", "gop": 50}, "decoder": {"name": "h264_cuvid"}}


def rung(w, h, kbps, codec="h264"):
    return {"width": w, "height": h, "bitrate": kbps, "framerate": 25, "codec": codec, "preset": "p4"}


PROFILES = {
    0: None,
    1: {"video": {"copy": True}, "audio": {"copy": True}},
    2: {"video": {"copy": True}, "audio": AUDIO_REENC},
    3: {**NVENC, "video": {"copy": False, "profiles": [rung(1280, 720, 2000)]}, "audio": AUDIO_REENC},
    4: {**NVENC, "video": {"copy": False, "profiles": [
        rung(1920, 1080, 4500), rung(1280, 720, 2500), rung(854, 480, 1000)]}, "audio": AUDIO_REENC},
    5: {**NVENC, "video": {"copy": False, "profiles": [rung(1280, 720, 2000, "h265")]}, "audio": AUDIO_REENC},
}
ALL_PROTOCOLS = {"hls": True, "dash": True, "rtsp": True, "rtmp": True, "srt": True, "mpegts": True}
PROBED_OUTPUTS = ["hls", "dash", "rtmp", "rtsp", "srt", "httpts"]
OUTPUTS = PROBED_OUTPUTS + ["push"]

CONF_BATCHES = {
    "B1": [("rtmppush", 0), ("rtmppull", 0), ("rtsp", 0), ("copy", 0), ("mixer", 0), ("rtmppush", 1), ("rtmppush", 2)],
    "B2": [("srt", 0), ("udp", 0), ("hls", 0), ("httpts", 0), ("file", 0), ("srt", 1), ("srt", 2)],
    "B3": [("rtmppush", 3), ("rtmppush", 5), ("srt", 3), ("srt", 5)],
    "B4": [("rtmppush", 4), ("srt", 4)],
    "B5": [("rtmppull", 4), ("rtsp", 4)],
    "B6": [("copy", 4), ("mixer", 4)],
    "B7": [("udp", 4), ("hls", 4)],
    "B8": [("httpts", 4), ("file", 4)],
}
# Order matters: streams that other streams read from come first.
LONG_RUN = [("rtmppull", 0), ("udp", 0), ("rtmppush", 4), ("srt", 3), ("rtsp", 5), ("hls", 0), ("httpts", 1),
            ("file", 1), ("copy", 0), ("mixer", 0)]


def log(msg):
    print(time.strftime("%Y-%m-%d %H:%M:%S"), msg, flush=True)


def http(method, url, body=None, timeout=20):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw, status = r.read(), r.status
    except urllib.error.HTTPError as e:
        raw, status = e.read(), e.code
    except OSError as e:
        return 0, str(e)
    try:
        return status, json.loads(raw)
    except ValueError:
        return status, raw.decode(errors="replace")


def api(method, path, body=None):
    return http(method, API + path, body)


class Run:
    """Files and child processes of one test run."""

    def __init__(self, data, name):
        self.dir = os.path.join(data, "runs", name)
        self.logs = os.path.join(self.dir, "logs")
        os.makedirs(self.logs, exist_ok=True)
        self.lock = threading.Lock()
        self.managed = []
        self.halt = threading.Event()

    def record(self, name, doc):
        doc = {"ts": round(time.time(), 1), **doc}
        with self.lock, open(os.path.join(self.dir, name + ".jsonl"), "a", encoding="utf-8") as f:
            f.write(json.dumps(doc) + "\n")

    def event(self, kind, **fields):
        self.record("events", {"kind": kind, **fields})
        log(f"event {kind} {fields}")

    def spawn(self, cmd, name):
        f = open(os.path.join(self.logs, name + ".log"), "ab")
        return subprocess.Popen(cmd, stdin=subprocess.DEVNULL, stdout=f, stderr=f)

    def manage(self, cmd, name):
        """Starts a process that the supervise thread keeps alive."""
        self.managed.append({"cmd": cmd, "name": name, "proc": self.spawn(cmd, name), "restarts": 0})

    def supervise(self):
        while not self.halt.wait(2):
            for m in self.managed:
                code = m["proc"].poll()
                if code is not None and not self.halt.is_set():
                    m["restarts"] += 1
                    self.event("process_exit", name=m["name"], code=code, restarts=m["restarts"])
                    m["proc"] = self.spawn(m["cmd"], m["name"])

    def stop_all(self):
        self.halt.set()
        procs = [m["proc"] for m in self.managed]
        for p in procs:
            if p.poll() is None:
                p.terminate()
        for p in procs:
            try:
                p.wait(5)
            except subprocess.TimeoutExpired:
                p.kill()
        self.managed.clear()


FFMPEG = ["ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "warning"]


def bridge(output):
    return FFMPEG + ["-rtsp_transport", "tcp", "-i", SOURCE, "-c", "copy"] + output


def input_url(inp, port, refs):
    if inp == "rtmppush":
        return "publish://"
    if inp == "rtmppull":
        return f"rtmp://{MTX_HOST}:{MTX_RTMP}/live/main"
    if inp == "rtsp":
        return f"rtsp://{MTX_HOST}:{MTX_RTSP}/live/main"
    if inp == "srt":
        return f"srt://{MTX_HOST}:{MTX_SRT}?streamid=read:live/main"
    if inp == "hls":
        return f"http://{MTX_HOST}:{MTX_HLS}/live/main/index.m3u8"
    if inp == "udp":
        return f"udp://127.0.0.1:{port}"
    if inp == "httpts":
        return f"http://127.0.0.1:{port}/stream.ts"
    if inp == "file":
        return f"file://{VOD}/clip.ts?loop=true"
    if inp == "copy":
        return f"copy://{refs[0]}"
    if inp == "mixer":
        return f"mixer://{refs[0]},{refs[1]}"
    raise ValueError(inp)


def start_feeder(run, inp, code, port):
    """Starts whatever must push into the stream. Pull inputs need nothing."""
    if inp == "rtmppush":
        run.manage(bridge(["-f", "flv", f"rtmp://127.0.0.1:{OS_RTMP}/live/{code}"]), "feed-" + code)
    elif inp == "udp":
        run.manage(bridge(["-f", "mpegts", f"udp://127.0.0.1:{port}?pkt_size=1316"]), "feed-" + code)
    elif inp == "httpts":
        run.manage(bridge(["-f", "mpegts", "-listen", "1", f"http://127.0.0.1:{port}/stream.ts"]), "feed-" + code)


def stream_body(inp, t, code, port, refs):
    body = {"inputs": [{"url": input_url(inp, port, refs), "priority": 0}], "protocols": ALL_PROTOCOLS,
            "push": [{"url": f"rtmp://{MTX_HOST}:{MTX_RTMP}/sink/{code}", "enabled": True,
                      "timeout_sec": 10, "retry_timeout_sec": 3}]}
    if PROFILES[t] is not None:
        body["transcoder"] = PROFILES[t]
    return body


def runtime(code):
    status, body = api("GET", f"/streams/{code}")
    if status != 200 or not isinstance(body, dict):
        return {}
    return (body.get("data") or {}).get("runtime") or {}


def wait_active(code, limit):
    end = time.time() + limit
    while time.time() < end:
        if runtime(code).get("status") == "active" and api("GET", f"/{code}/index.m3u8")[0] == 200:
            return True
        time.sleep(1)
    return False


def start_stack(run, clip):
    if not os.path.isfile(clip):
        sys.exit(f"clip not found: {clip}")
    subprocess.run(["docker", "compose", "-f", os.path.join(HERE, "compose.yml"), "up", "-d", "--quiet-pull"], check=True,
                   capture_output=True)
    # RTMP, because ffmpeg's RTSP muxer rejects ADTS AAC from an MPEG-TS clip.
    run.manage(FFMPEG + ["-re", "-stream_loop", "-1", "-i", clip, "-c", "copy", "-f", "flv",
                         f"rtmp://{MTX_HOST}:{MTX_RTMP}/live/main"], "publisher")
    end = time.time() + 30
    while time.time() < end:
        st, body = http("GET", f"{MTX_API}/v3/paths/get/live/main")
        if st == 200 and isinstance(body, dict) and body.get("ready"):
            time.sleep(8)  # let MediaMTX fill its HLS window before anything pulls it
            return
        time.sleep(1)
    sys.exit("MediaMTX source never became ready")


def upload_clip(clip):
    st, body = api("POST", "/vod", {"name": VOD, "storage": f"/var/lib/open-streamer/{VOD}"})
    if st not in (200, 201, 409):
        sys.exit(f"vod mount: {st} {body}")
    out = subprocess.run(["curl", "-sS", "-m", "300", "-o", "/dev/null", "-w", "%{http_code}", "-F", f"file=@{clip}",
                          f"{API}/vod/{VOD}/files"], capture_output=True, text=True, check=False)
    if out.stdout.strip() not in ("200", "201"):
        sys.exit(f"vod upload: {out.stdout} {out.stderr}")


def cleanup(run, prefix):
    run.stop_all()
    st, body = api("GET", "/streams")
    if st == 200:
        for s in body.get("data") or []:
            if str(s.get("code", "")).startswith(prefix):
                api("DELETE", f"/streams/{s['code']}")
    api("DELETE", f"/vod/{VOD}/files/clip.ts")
    api("DELETE", f"/vod/{VOD}")
    log("cleaned up")


def expected(t):
    codec = "hevc" if t == 5 else "h264"
    if t == 4:
        return codec, {1080, 720, 480}
    return codec, {720} if t in (3, 5) else {1080}


def output_url(kind, code):
    return {
        "hls": f"http://127.0.0.1:8080/{code}/index.m3u8",
        "dash": f"http://127.0.0.1:8080/{code}/index.mpd",
        "rtmp": f"rtmp://127.0.0.1:{OS_RTMP}/live/{code}",
        "rtsp": f"rtsp://127.0.0.1:{OS_RTSP}/live/{code}",
        "srt": f"srt://127.0.0.1:{OS_SRT}?streamid=live/{code}",
        "httpts": f"http://127.0.0.1:8080/{code}/mpegts",
    }[kind]


def analyze(kind, t, doc, min_span):
    """Returns the verdict for one output. Backwards DTS is a warning, not a failure."""
    why, warn = [], []
    videos = [s for s in doc.get("streams", []) if s.get("codec_type") == "video"]
    audios = [s for s in doc.get("streams", []) if s.get("codec_type") == "audio"]
    want_codec, want_heights = expected(t)
    heights = sorted({s.get("height") for s in videos if s.get("height")})
    codecs = sorted({s.get("codec_name") for s in videos})
    if not videos:
        why.append("no video")
    else:
        if want_codec not in codecs:
            why.append(f"video codec {codecs}, want {want_codec}")
        if t == 4 and kind in ("hls", "dash"):
            if want_heights - set(heights):
                why.append(f"renditions {heights}, want {sorted(want_heights)}")
        elif not want_heights & set(heights):
            why.append(f"height {heights}, want one of {sorted(want_heights)}")
    if not audios:
        why.append("no audio")
    elif "aac" not in {s.get("codec_name") for s in audios}:
        why.append(f"audio {sorted({s.get('codec_name') for s in audios})}, want aac")

    dts = defaultdict(list)
    for p in doc.get("packets", []):
        if p.get("dts_time") not in (None, "N/A"):
            dts[p.get("stream_index")].append(float(p["dts_time"]))
    v_idx = {s.get("index") for s in videos}
    a_idx = {s.get("index") for s in audios}
    span, backwards = 0.0, 0
    for idx, values in dts.items():
        if idx in v_idx and len(values) > 1:
            span = max(span, max(values) - min(values))
            backwards += sum(1 for a, b in zip(values, values[1:]) if b < a)
    if videos and span < min_span:
        why.append(f"video span {span:.1f}s, want at least {min_span}s")
    if audios and not any(dts.get(i) for i in a_idx):
        why.append("no audio packets")
    if backwards:
        warn.append(f"{backwards} backwards video DTS")
    first_v = [dts[i][0] for i in v_idx if dts.get(i)]
    first_a = [dts[i][0] for i in a_idx if dts.get(i)]
    offset = round(first_a[0] - first_v[0], 3) if first_a and first_v else None
    return {"ok": not why, "why": why, "warn": warn, "codecs": codecs, "heights": heights, "span": round(span, 1),
            "av_start_offset": offset}


def probe(kind, code, t, seconds):
    entries = "stream=index,codec_type,codec_name,height:packet=stream_index,dts_time"
    base = ["ffprobe", "-v", "error", "-show_entries", entries, "-of", "json"]
    limit = str(seconds + 45)
    if kind == "httpts":
        # An endless HTTP body: curl ends the input after the window.
        url = output_url(kind, code)
        cmd = ["timeout", limit, "bash", "-c", f"curl -sN --max-time {seconds + 3} {url} | {' '.join(base)} -i pipe:0"]
    else:
        cmd = ["timeout", limit] + base + (["-rtsp_transport", "tcp"] if kind == "rtsp" else [])
        cmd += ["-read_intervals", f"%+{seconds}", output_url(kind, code)]
    run = subprocess.run(cmd, capture_output=True, text=True, check=False)
    try:
        doc = json.loads(run.stdout)
    except ValueError:
        return {"ok": False, "why": [f"ffprobe rc={run.returncode}: {run.stderr.strip()[:160]}"], "warn": []}
    res = analyze(kind, t, doc, seconds * 2 / 3)
    if run.returncode != 0:
        res["ok"] = False
        res["why"].append(f"ffprobe rc={run.returncode}")
    return res


def check_push(code, t, before):
    """Compares what the MediaMTX sink received since `before` bytes."""
    st, body = http("GET", f"{MTX_API}/v3/paths/get/sink/{code}")
    if st != 200 or not isinstance(body, dict):
        return {"ok": False, "why": [f"sink not found ({st})"], "warn": [], "bytes": 0}
    received = body.get("bytesReceived") or 0
    tracks = body.get("tracks") or []
    why = []
    if not body.get("ready"):
        why.append("sink not ready")
    if received <= before:
        why.append("sink received no new bytes")
    want = "H265" if t == 5 else "H264"
    if want not in tracks:
        why.append(f"sink tracks {tracks}, want {want}")
    return {"ok": not why, "why": why, "warn": [], "tracks": tracks, "bytes": received}


def sink_bytes(code):
    st, body = http("GET", f"{MTX_API}/v3/paths/get/sink/{code}")
    return (body.get("bytesReceived") or 0) if st == 200 and isinstance(body, dict) else 0


def parse_metrics():
    status, text = api("GET", "/metrics")
    rows = []
    if status != 200:
        return rows
    for line in text.splitlines():
        m = re.match(r"^([a-zA-Z_:][\w:]*)(?:\{([^}]*)\})?\s+(\S+)$", line)
        if m:
            labels = dict(re.findall(r'(\w+)="((?:[^"\\]|\\.)*)"', m.group(2) or ""))
            rows.append((m.group(1), labels, float(m.group(3))))
    return rows


def belongs(buffer_id, code):
    return buffer_id == code or buffer_id == "$raw$" + code or buffer_id.startswith("$r$" + code + "$")


def stream_counters(rows, code):
    total = Counter()
    for name, labels, value in rows:
        if not name.startswith("open_streamer_"):
            continue
        short = name[len("open_streamer_"):]
        sc = labels.get("stream_code", "")
        if short.startswith("buffer_") and belongs(sc, code) and short == "buffer_drops_total":
            total["drops"] += value
        elif sc == code:
            if short == "transcoder_restarts_total":
                total["restarts"] += value
            elif short == "transcoder_errors_total":
                total["tc_errors"] += value
            elif short == "ingestor_errors_total":
                total["ingest_errors"] += value
            elif short == "manager_failovers_total":
                total["failovers"] += value
            elif short == "transcoder_process_resident_memory_bytes":
                total["tc_rss"] += value
            elif short == "transcoder_process_cpu_seconds_total":
                total["tc_cpu"] += value
            elif short == "transcoder_decoded_frames_total":
                total["decoded"] += value
            elif short == "transcoder_frames_total" and labels.get("kind") == "video":
                total["encoded"] += value
            elif short == "publisher_segments_total":
                total["segments"] += value
    return dict(total)


def gpu():
    out = subprocess.run(["nvidia-smi", "--query-gpu=utilization.encoder,utilization.decoder,memory.used,"
                          "encoder.stats.sessionCount,temperature.gpu", "--format=csv,noheader,nounits"],
                         capture_output=True, text=True, check=False).stdout
    try:
        enc, dec, mem, sessions, temp = (float(x) for x in out.split(","))
    except ValueError:
        return {}
    return {"nvenc": enc, "nvdec": dec, "vram_mb": mem, "sessions": sessions, "temp": temp}


def journal_top(since):
    out = subprocess.run(["journalctl", "-u", "open-streamer", "--since", f"@{int(since)}", "--no-pager", "-o", "cat"],
                         capture_output=True, text=True, check=False).stdout
    seen = Counter()
    for line in out.splitlines():
        try:
            d = json.loads(line)
        except ValueError:
            continue
        if d.get("level") in ("WARN", "ERROR"):
            seen[f'{d["level"]} {d.get("msg", "")[:70]}'] += 1
    return seen.most_common(10)


def create_streams(run, specs, code_of, refs_of):
    """Creates streams in order and returns their descriptors."""
    streams = []
    for i, (inp, t) in enumerate(specs):
        code = code_of(i, inp, t)
        port = 17000 + i
        # An HTTP or UDP source must exist before the stream pulls it; an RTMP
        # push can only start once the stream is registered.
        if inp != "rtmppush":
            start_feeder(run, inp, code, port)
            if inp == "httpts":
                time.sleep(3)
        body = stream_body(inp, t, code, port, refs_of(i, inp))
        st, resp = api("POST", f"/streams/{code}", body)
        s = {"code": code, "inp": inp, "t": t, "created": st in (200, 201)}
        streams.append(s)
        if not s["created"]:
            s["error"] = f"{st} {str(resp)[:240]}"
            run.event("create_failed", code=code, error=s["error"])
            continue
        if inp == "rtmppush":
            start_feeder(run, inp, code, port)
    return streams


def conf_batch(run, name, seeds, results):
    specs = CONF_BATCHES[name]
    log(f"batch {name}: {specs}")
    t0 = time.time()
    code_of = lambda _i, inp, t: f"soak-conf-{inp}-t{t}"
    streams = create_streams(run, specs, code_of, lambda _i, _inp: seeds)
    with cf.ThreadPoolExecutor(len(streams)) as ex:
        ready = list(ex.map(lambda s: s["created"] and wait_active(s["code"], 60), streams))
    time.sleep(12)
    live = [s for s in streams if s["created"]]
    before = {s["code"]: stream_counters_snapshot(s["code"]) for s in live}
    sinks = {s["code"]: sink_bytes(s["code"]) for s in live}
    peak = []
    stop = threading.Event()
    sampler = threading.Thread(target=lambda: [peak.append(gpu()) or stop.wait(2) for _ in iter(lambda: stop.is_set(), True)],
                               daemon=True)
    sampler.start()
    with cf.ThreadPoolExecutor(max(1, len(live) * 6)) as ex:
        jobs = {(s["code"], k): ex.submit(probe, k, s["code"], s["t"], 30) for s in live for k in PROBED_OUTPUTS}
        probed = {key: f.result() for key, f in jobs.items()}
    stop.set()
    sampler.join(5)

    for s, is_ready in zip(streams, ready):
        row = {"batch": name, **s, "ready": bool(is_ready), "outputs": {}}
        if s["created"]:
            code = s["code"]
            rt = runtime(code)
            row["status"] = rt.get("status")
            row["restarts"] = (rt.get("transcoder") or {}).get("restart_count", 0)
            row["outputs"] = {k: probed[(code, k)] for k in PROBED_OUTPUTS}
            row["outputs"]["push"] = check_push(code, s["t"], sinks[code])
            after = stream_counters_snapshot(code)
            row["delta"] = {k: round(after.get(k, 0) - before[code].get(k, 0), 2) for k in ("drops", "ingest_errors", "tc_errors")}
        row["pass"] = bool(s["created"] and row["ready"] and row.get("status") == "active" and not row.get("restarts")
                           and row["outputs"] and all(o["ok"] for o in row["outputs"].values()))
        results.append(row)
        run.record("conf", row)
        bad = {k: o["why"] for k, o in row["outputs"].items() if not o["ok"]}
        warn = {k: o["warn"] for k, o in row["outputs"].items() if o.get("warn")}
        log(f"{'PASS' if row['pass'] else 'FAIL'} {s['code']} status={row.get('status')} restarts={row.get('restarts')} "
            f"{row.get('delta', '')} {bad or ''} {('warn ' + str(warn)) if warn else ''} {s.get('error', '')}")

    columns = [p for p in peak if p]
    machine = {k: max(p[k] for p in columns) for k in ("nvenc", "nvdec", "vram_mb", "sessions", "temp")} if columns else {}
    run.record("batches", {"batch": name, "peak": machine, "journal": journal_top(t0), "seconds": round(time.time() - t0)})
    log(f"peak {machine}")

    for m in [m for m in run.managed if m["name"].startswith("feed-soak-conf-")]:
        m["proc"].terminate()
    run.managed[:] = [m for m in run.managed if not m["name"].startswith("feed-soak-conf-")]
    for s in live:
        api("DELETE", f"/streams/{s['code']}")
    time.sleep(4)


def stream_counters_snapshot(code):
    return stream_counters(parse_metrics(), code)


def cmd_conf(args):
    name = time.strftime("conf-%Y%m%d-%H%M%S")
    run = Run(args.data, name)
    prefix = "soak-conf-"
    install_signals(run, prefix)
    results = []
    try:
        threading.Thread(target=run.supervise, daemon=True).start()
        start_stack(run, args.clip)
        seeds = [f"{prefix}seed-a", f"{prefix}seed-b"]
        for code in seeds:
            api("POST", f"/streams/{code}", {"inputs": [{"url": f"rtsp://{MTX_HOST}:{MTX_RTSP}/live/main", "priority": 0}],
                                             "protocols": ALL_PROTOCOLS})
            if not wait_active(code, 60):
                sys.exit(f"seed {code} never became active")
        batches = args.batch or list(CONF_BATCHES)
        if any(i == "file" for b in batches for i, _ in CONF_BATCHES[b]):
            upload_clip(args.clip)
        for b in batches:
            conf_batch(run, b, seeds, results)
    finally:
        cleanup(run, prefix)
    print("\n" + "stream".ljust(26) + " ".join(k.ljust(7) for k in OUTPUTS) + "result")
    for r in results:
        cells = " ".join(cell(r["outputs"].get(k)).ljust(7) for k in OUTPUTS)
        print(r["code"].replace(prefix, "").ljust(26) + cells + ("PASS" if r["pass"] else "FAIL"))
    print(f"\n{sum(r['pass'] for r in results)}/{len(results)} streams passed; data in {run.dir}", flush=True)


def cell(o):
    if not o:
        return "-"
    if not o["ok"]:
        return "FAIL"
    return "warn" if o.get("warn") else "ok"


def install_signals(run, prefix):
    def handler(*_):
        cleanup(run, prefix)
        sys.exit(1)
    for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(sig, handler)


# ---- LONG-RUN ---------------------------------------------------------------


def long_code(i, inp, t):
    return f"soak-{i + 1:02d}-{inp}-t{t}"


def long_refs(_i, inp):
    if inp == "copy":
        return [long_code(0, *LONG_RUN[0])]
    if inp == "mixer":
        return [long_code(0, *LONG_RUN[0]), long_code(1, *LONG_RUN[1])]
    return []


def sampler_loop(run, streams, every):
    while not run.halt.wait(every):
        rows = parse_metrics()
        global_values = {n: v for n, l, v in rows if not l and n in (
            "process_resident_memory_bytes", "go_memstats_heap_inuse_bytes", "go_goroutines", "process_open_fds",
            "process_cpu_seconds_total")}
        st, listing = api("GET", "/streams")
        status = {}
        if st == 200:
            for s in listing.get("data") or []:
                status[s.get("code")] = ((s.get("runtime") or {}).get("status"))
        disk = subprocess.run(["du", "-sb", "/var/lib/open-streamer"], capture_output=True, text=True, check=False).stdout.split()
        run.record("samples", {
            "server": global_values,
            "series": len(rows),
            "gpu": gpu(),
            "disk_bytes": int(disk[0]) if disk else None,
            "streams": {s["code"]: {"status": status.get(s["code"]), **stream_counters(rows, s["code"])} for s in streams},
        })


def prober_loop(run, streams, period, seconds):
    """Spreads the probes evenly over each period."""
    work = [(s, k) for s in streams for k in PROBED_OUTPUTS]
    gap = period / max(1, len(work))
    with cf.ThreadPoolExecutor(max_workers=int(seconds / gap) + 4) as ex:
        while not run.halt.is_set():
            for s, kind in work:
                if run.halt.is_set():
                    return
                ex.submit(probe_once, run, s, kind, seconds)
                run.halt.wait(gap)


def probe_once(run, s, kind, seconds):
    res = probe(kind, s["code"], s["t"], seconds)
    run.record("probes", {"code": s["code"], "output": kind, **res})
    if not res["ok"]:
        run.event("probe_failed", code=s["code"], output=kind, why=res["why"])


def push_loop(run, streams, every):
    last = {s["code"]: sink_bytes(s["code"]) for s in streams}
    while not run.halt.wait(every):
        for s in streams:
            res = check_push(s["code"], s["t"], last[s["code"]])
            last[s["code"]] = res.get("bytes", last[s["code"]])
            run.record("probes", {"code": s["code"], "output": "push", **res})
            if not res["ok"]:
                run.event("probe_failed", code=s["code"], output="push", why=res["why"])


def start_readers(run, streams):
    """One long-held reader per output on the first two transcoded streams."""
    held = [s for s in streams if s["t"] in (3, 4)]
    for s in held:
        for kind in PROBED_OUTPUTS:
            name = f"reader-{s['code']}-{kind}"
            cmd = ["ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error"]
            cmd += ["-rtsp_transport", "tcp"] if kind == "rtsp" else []
            cmd += ["-i", output_url(kind, s["code"]), "-map", "0", "-c", "copy", "-f", "null", "-"]
            run.manage(cmd, name)


def cmd_long(args):
    name = time.strftime("long-%Y%m%d-%H%M%S")
    run = Run(args.data, name)
    install_signals(run, "soak-")
    end = time.time() + args.hours * 3600 if args.hours else float("inf")
    with open(os.path.join(run.dir, "meta.json"), "w", encoding="utf-8") as f:
        json.dump({"start": time.time(), "hours": args.hours, "stream_set": LONG_RUN, "version": api("GET", "/config")[1].get("version")}, f, indent=1)
    try:
        threading.Thread(target=run.supervise, daemon=True).start()
        start_stack(run, args.clip)
        upload_clip(args.clip)
        streams = create_streams(run, LONG_RUN, long_code, long_refs)
        live = [s for s in streams if s["created"]]
        if len(live) != len(streams):
            sys.exit("not every stream could be created; see events.jsonl")
        for s in live:
            if not wait_active(s["code"], 90):
                run.event("not_active_at_start", code=s["code"])
        time.sleep(20)
        loops = [(sampler_loop, (run, live, 60))]
        if not args.no_checks:
            start_readers(run, live)
            loops += [(prober_loop, (run, live, args.probe_period, 15)), (push_loop, (run, live, 60))]
        for target, loop_args in loops:
            threading.Thread(target=target, args=loop_args, daemon=True).start()
        log(f"long run started: {len(live)} streams, {args.hours or 'until stopped'} h, data in {run.dir}")
        next_report = time.time() + args.report_every * 3600
        while time.time() < end and not run.halt.is_set():
            time.sleep(5)
            if time.time() >= next_report:
                write_report(run.dir)
                next_report += args.report_every * 3600
        log("long run finished")
    finally:
        cleanup(run, "soak-")
        write_report(run.dir)


def read_jsonl(path):
    if not os.path.exists(path):
        return
    with open(path, encoding="utf-8") as f:
        for line in f:
            try:
                yield json.loads(line)
            except ValueError:
                continue


def growth_per_day(points):
    """Percent per day from a least-squares fit after the warm-up."""
    if len(points) < 10:
        return None
    t0 = points[0][0]
    xs = [(t - t0) / 86400 for t, _ in points]
    ys = [v for _, v in points]
    mean_x, mean_y = statistics.fmean(xs), statistics.fmean(ys)
    denom = sum((x - mean_x) ** 2 for x in xs)
    if not denom or not mean_y:
        return None
    slope = sum((x - mean_x) * (y - mean_y) for x, y in zip(xs, ys)) / denom
    return round(100 * slope / mean_y, 2)


def write_report(run_dir):
    samples = list(read_jsonl(os.path.join(run_dir, "samples.jsonl")))
    probes = list(read_jsonl(os.path.join(run_dir, "probes.jsonl")))
    events = list(read_jsonl(os.path.join(run_dir, "events.jsonl")))
    with open(os.path.join(run_dir, "meta.json"), encoding="utf-8") as f:
        meta = json.load(f)
    start = meta["start"]
    hours = (samples[-1]["ts"] - start) / 3600 if samples else 0
    lines = [f"# Soak report: {os.path.basename(run_dir)}", "", f"Version: `{meta.get('version')}`  ",
             f"Elapsed: {hours:.1f} h of {meta['hours']} h  ", f"Generated: {time.strftime('%Y-%m-%d %H:%M:%S')}", ""]

    lines += ["## Outputs", "", "Pass rate per output, with failed checks in brackets.", "",
              "| Stream | " + " | ".join(OUTPUTS) + " |", "|---|" + "---|" * len(OUTPUTS)]
    table = defaultdict(lambda: defaultdict(lambda: [0, 0]))
    for p in probes:
        table[p["code"]][p["output"]][0] += 1
        table[p["code"]][p["output"]][1] += 0 if p["ok"] else 1
    for code in sorted(table):
        cells = []
        for k in OUTPUTS:
            total, bad = table[code][k]
            cells.append("-" if not total else f"{100 * (total - bad) / total:.2f}% ({bad})")
        lines.append(f"| {code} | " + " | ".join(cells) + " |")

    warned = Counter((p["code"], p["output"]) for p in probes if p.get("warn"))
    if warned:
        lines += ["", "Backwards DTS seen in these outputs (number of probes):", ""]
        lines += [f"- {c} {o}: {n}" for (c, o), n in sorted(warned.items())]

    if samples:
        lines += ["", "## Streams", "", "Change from the first to the last sample.", "",
                  "| Stream | not active samples | restarts | transcoder errors | ingest errors | failovers | buffer drops |",
                  "|---|---|---|---|---|---|---|"]
        for code in sorted(samples[-1]["streams"]):
            first, last = samples[0]["streams"].get(code, {}), samples[-1]["streams"].get(code, {})
            down = sum(1 for s in samples if s["streams"].get(code, {}).get("status") != "active")
            diff = lambda k: round(last.get(k, 0) - first.get(k, 0))
            lines.append(f"| {code} | {down} | {diff('restarts')} | {diff('tc_errors')} | {diff('ingest_errors')} | "
                         f"{diff('failovers')} | {diff('drops')} |")

        settled = [s for s in samples if s["ts"] - start >= WARMUP_HOURS * 3600]
        lines += ["", f"## Trends after {WARMUP_HOURS} h warm-up", ""]
        if not settled:
            lines.append("Not enough data yet.")
        else:
            lines += ["| Metric | First | Last | Growth %/day |", "|---|---|---|---|"]
            def add(label, get):
                pts = [(s["ts"], get(s)) for s in settled if get(s) is not None]
                if pts:
                    lines.append(f"| {label} | {pts[0][1]:.0f} | {pts[-1][1]:.0f} | {growth_per_day(pts)} |")
            for key in ("process_resident_memory_bytes", "go_memstats_heap_inuse_bytes", "go_goroutines", "process_open_fds"):
                add("server " + key, lambda s, k=key: s["server"].get(k))
            add("metric series", lambda s: s.get("series"))
            add("disk bytes", lambda s: s.get("disk_bytes"))
            for code in sorted(samples[-1]["streams"]):
                if any(s["streams"].get(code, {}).get("tc_rss") for s in settled):
                    add(f"{code} transcoder RSS", lambda s, c=code: s["streams"].get(c, {}).get("tc_rss"))
        gpus = [s["gpu"] for s in samples if s.get("gpu")]
        if gpus:
            lines += ["", "## GPU", "", f"NVENC peak {max(g['nvenc'] for g in gpus):.0f}%, average "
                      f"{statistics.fmean(g['nvenc'] for g in gpus):.0f}%. Sessions min {min(g['sessions'] for g in gpus):.0f}, "
                      f"max {max(g['sessions'] for g in gpus):.0f}. Peak temperature {max(g['temp'] for g in gpus):.0f}°C."]

    kinds = Counter(e["kind"] for e in events)
    lines += ["", "## Events", "", *(f"- {k}: {n}" for k, n in kinds.most_common())]
    lines += ["", "Environment incidents are `process_exit` events from the publisher and bridges; they do not count "
              "against Open Streamer.", "", "Last 20 events:", "", "```"]
    lines += [f"{time.strftime('%m-%d %H:%M:%S', time.localtime(e['ts']))} {e['kind']} "
              f"{json.dumps({k: v for k, v in e.items() if k not in ('ts', 'kind')})[:200]}" for e in events[-20:]]
    lines += ["```", "", "## Top warnings", "", *(f"- {n} × {m}" for m, n in journal_top(start))]
    with open(os.path.join(run_dir, "report.md"), "w", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")
    log(f"report written to {run_dir}/report.md")


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--data", default=DEFAULT_DATA, help="data directory (default: %(default)s)")
    parser.add_argument("--clip", default=os.path.join(DEFAULT_DATA, "media", "clip.ts"), help="source clip")
    sub = parser.add_subparsers(dest="cmd", required=True)
    conf = sub.add_parser("conf")
    conf.add_argument("--batch", nargs="+", choices=list(CONF_BATCHES))
    conf.set_defaults(func=cmd_conf)
    long_run = sub.add_parser("long")
    long_run.add_argument("--hours", type=float, help="stop after this many hours (default: run until stopped)")
    long_run.add_argument("--no-checks", action="store_true", help="only keep the streams running; no probes or readers")
    long_run.add_argument("--probe-period", type=int, default=300, help="seconds per round of output probes")
    long_run.add_argument("--report-every", type=float, default=24, help="hours between reports")
    long_run.set_defaults(func=cmd_long)
    report = sub.add_parser("report")
    report.add_argument("run_dir")
    report.set_defaults(func=lambda a: write_report(a.run_dir))
    args = parser.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
