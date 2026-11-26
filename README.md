# Open Streamer v2

This branch is the v2 rebuild of Open Streamer, started from a clean tree.
The design and milestones are in [docs/REBUILD_PLAN.md](docs/REBUILD_PLAN.md).

## Running the server

```sh
make openstreamer        # bin/openstreamer, pure Go
make ostranscoder        # dist/ostranscoder-<arch>/: the transcoder and its libraries (Linux)
bin/openstreamer -config /srv/openstreamer/openstreamer.yaml
```

The server prints the address of each listener it bound (`http 0.0.0.0:8080`,
`api 127.0.0.1:8081`, …) and runs until SIGINT or SIGTERM. Streams with a
ladder or a thumbnail need the transcoder: set `server.transcoder.binary` to
the bundle's `ostranscoder` (keep its `lib/` beside it), or put `ostranscoder`
next to `openstreamer`. Passthrough streams need no transcoder.

### The config file

All configuration is one YAML file. Start from
[cmd/openstreamer/openstreamer.example.yaml](cmd/openstreamer/openstreamer.example.yaml),
which documents every key. Besides `version: 1` it has three sections:

- `server`: the listeners (`http`, `api`, `rtmp`, `rtsp`, `srt`), their apps,
  the transcoder binary, the assets directory and logging. A change here
  needs a restart: until then it is reported as `restart_required`.
- `users`: the API's users, each a `name`, a `role` and a password hash.
- `streams`: one document per stream (inputs by priority, an optional
  transcode ladder and thumbnail, the outputs and the push targets).

**Users and roles.** A `viewer` reads everything except secrets (streams with
their URLs redacted, status, thumbnails, `/metrics`). An `editor` also writes,
reads the config file and stream keys, and reaches pprof. The config needs at
least one editor. The `password` field holds a hash, never the password:

```sh
bin/openstreamer hash-password       # asks twice on a terminal, echo off
bin/openstreamer hash-password < password.txt          # or reads a file or a pipe
```

It prints `pbkdf2-sha256$600000$…`, which goes into the file as the user's
`password`. Passwords are at least 12 bytes and never go on the command line.

**File and directory.** The file holds stream keys and password hashes, so on
Unix it must be mode 0600 or the server does not start. The server rewrites it
when the API edits the config (a temp file in the same directory, then a
rename), so the service user must own the file **and its directory**: keep it
in a directory such as `/srv/openstreamer`, not a root-owned `/etc`. In a
container, mount that directory, not the single file: a bind-mounted file
cannot be renamed over.

**YAML rules.** Anchors, aliases and merge keys (`&a`, `*a`, `<<:`) are not
supported. Numbers are plain decimal integers (`720`, not `0720`, `720.0` or
`7_20`). Text that looks like a number must be quoted: a stream named `007`
is `name: "007"`, a watermark text `24.7` is `text: "24.7"`. Unknown keys are
errors, and every error is reported with its place.

### The API

The API listens on `127.0.0.1:8081` by default and keeps to loopback until
M5c brings HTTPS: Basic credentials are only base64-encoded, so anyone who can
see the traffic can read them. Every request but `/healthz` uses HTTP Basic
auth (`curl -u ops` asks for the password), and every `PUT`, `DELETE` and
`POST` must carry an `X-Requested-By` header (any value). The OpenAPI
document is at `/api/v1/openapi.json`.

Create or replace a stream, served on every output:

```sh
curl -u ops -X PUT http://127.0.0.1:8081/api/v1/streams/cam1 \
  -H 'X-Requested-By: curl' -H 'Content-Type: application/json' \
  -d '{"inputs":[{"url":"rtsp://camera.example/stream1"}],
       "outputs":{"hls":true,"dash":true,"httpts":true,"rtmp":true,"rtsp":true,"srt":true}}'
```

It answers 201 for a new stream and 200 for a replaced one, with the stream
(URLs redacted) and how the edit was applied. `GET /api/v1/streams/cam1/status`
shows its state; `DELETE /api/v1/streams/cam1` removes it.

Edit the whole config in one round trip. `GET` returns the file as it is,
comments included, with an `ETag`; `PUT` takes the whole file back with that
`ETag` in `If-Match`, applies it and writes the bytes exactly as sent:

```sh
curl -u ops -D headers.txt -o openstreamer.yaml http://127.0.0.1:8081/api/v1/config
etag=$(sed -n 's/^[Ee][Tt]ag: *//p' headers.txt | tr -d '\r')
"$EDITOR" openstreamer.yaml
curl -u ops -X PUT http://127.0.0.1:8081/api/v1/config \
  -H 'X-Requested-By: curl' -H 'Content-Type: application/yaml' \
  -H "If-Match: $etag" --data-binary @openstreamer.yaml
```

The answer lists each stream's outcome (`unchanged`, `outputs`, `rebuild`,
`created`, `started`, `stopped`, `deleted`) and any `restart_required` keys. An
invalid document is 422 with every error and changes nothing; a stale
`If-Match` is 412. Changing only a stream's outputs is applied in place, so its
other outputs keep their viewers; any other change to a stream restarts that
stream, and streams that did not change are not touched.

A stream that could not run when the server started (its listen address was
taken by another stream, an asset was missing) is left `stopped`, with the
reason as its `last_error` and in `load_errors` of
`GET /api/v1/config/status`. Every later edit and reload tries it again, even
when its own document did not change: it answers `started` once nothing stands
in its way, and until then it stays stopped with the reason refreshed, which
never fails the edit. So after removing what was in the way, a `PUT` of the
same document or a reload is enough.

**Editing the file by hand.** Save the file, then reload it:

```sh
kill -HUP "$(pidof openstreamer)"
curl -u ops -X POST -H 'X-Requested-By: curl' http://127.0.0.1:8081/api/v1/reload   # the same, also on Windows
```

Until the reload, every API write is refused with 409 `config_changed` ("the
config file changed on disk; reload first"), so neither side overwrites the
other. An invalid file at reload keeps the running config; the error is in the
log and in `GET /api/v1/config/status`.

### Playing a stream

With the example config's listeners, stream `cam1` plays at:

| Output | URL |
|---|---|
| HLS (TS) | `http://host:8080/live/cam1/hls/index.m3u8` |
| HLS (fMP4) | `http://host:8080/live/cam1/cmaf/master.m3u8` |
| DASH | `http://host:8080/live/cam1/cmaf/index.mpd` |
| HTTP-TS | `http://host:8080/live/cam1/stream.ts` |
| RTMP | `rtmp://host:19350/live/cam1` |
| RTSP | `rtsp://host:5554/live/cam1` |
| SRT | `srt://host:9999?streamid=live/cam1` |

A stream with a ladder is served as `cam1` (the ABR presentation over HLS and
DASH, the first rendition elsewhere), and each rendition also as
`cam1_<label>`. The thumbnail is an operator preview behind the API:
`/api/v1/streams/cam1/thumbnail.jpg`. Playback needs no credentials.

### Metrics and logs

`/metrics` on the API listener is in the Prometheus text format and needs a
viewer. Scrape it with `basic_auth`:

```yaml
scrape_configs:
  - job_name: openstreamer
    basic_auth:
      username: grafana
      password_file: /etc/prometheus/openstreamer-password
    static_configs:
      - targets: ["127.0.0.1:8081"]
```

Logs go to stderr as JSON, one object per line with `time`, `level`, `msg`
and attributes such as `stream`, `input` and the error;
`server.log.format: text` gives `key=value` lines for a terminal. The API
logs one line per request (`api: request`: method, route pattern, status,
duration, remote address and user), never a header, a query or a body.
URLs are redacted, and attributes named like a secret are masked.
