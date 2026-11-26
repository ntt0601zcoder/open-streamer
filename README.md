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
`api 127.0.0.1:8081`, …) and runs until SIGINT or SIGTERM, which stop it in
order; a second one ends it at once. Streams with a
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
cannot be renamed over. When the file cannot be written, an API edit answers 500
`config_write_failed` with the reason, and nothing is applied.

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

Failed logins are limited per client address and user name: after 5 failures
that pair waits (1 s, doubling to 5 min) and gets 429 with `Retry-After`.
Behind a reverse proxy every client has the proxy's address, so the limit is
shared by all of them and one client's failures can lock a user name out for
everyone; a trusted-proxy setting that reads the forwarded address comes with
M5c. Until then keep the API on loopback or on a network you trust. The
server logs a warning at start when `server.api.listen` is not a loopback
address.

Create or replace a stream, served on every output:

```sh
curl -u ops -X PUT http://127.0.0.1:8081/api/v1/streams/cam1 \
  -H 'X-Requested-By: curl' -H 'Content-Type: application/json' \
  -d '{"inputs":[{"url":"rtsp://camera.example/stream1"}],
       "outputs":{"hls":true,"dash":true,"httpts":true,"rtmp":true,"rtsp":true,"srt":true}}'
```

It answers 201 for a new stream and 200 for a replaced one, with the stream
(URLs redacted) and how the edit was applied. A `PUT` of the document the
stream already has is `unchanged` and does not rewrite the file. An output
whose listener is off (`rtmp`, `rtsp` or `srt` while that `server.*.listen`
is empty) is refused with 422 `invalid_spec`: turn the output off, or set the
listener and restart. `GET /api/v1/streams/cam1/status` shows its state;
`DELETE /api/v1/streams/cam1` removes it.

Listeners are fixed at start:

- Removing a server listener (`server.rtmp.listen`, `rtsp` or `srt`) and
  restarting leaves every stream that has that output `stopped`, with a load
  error that names the output, until the output is turned off or the listener
  is back.
- Adding a listener and turning on its output takes two steps: set the
  listener and restart, then turn the output on. One whole-config `PUT` that
  does both is refused with 422, because the output is judged against the
  listeners the server started with.

`If-Match` makes a write conditional. It takes one tag exactly as an `ETag`
header gave it (or `*`, for "the stream exists"): a list of tags and weak
tags (`W/"…"`) do not match. It is optional on a stream's `PUT` and `DELETE`
and required on the config's `PUT`. A `DELETE` with `If-Match` on a name
that does not exist is 412, not 404: the condition is checked first.

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

The answer lists each stream's outcome (`unchanged`, `outputs`, `live`,
`rebuild`, `created`, `started`, `stopped`, `deleted`) and any
`restart_required` keys. An invalid document is 422 with every error and
changes nothing; a stale `If-Match` is 412. Streams that did not change are not
touched. A running stream that changed is edited in one of three ways:

- `outputs`: only its outputs changed. They are edited in place, and the
  other outputs keep their viewers.
- `live`: its inputs, its watermark or its `audio_language` changed (and
  maybe its outputs, and nothing else). They are applied while it runs: no
  output session ends and the transcoder is not restarted. Replacing the
  input in use is one discontinuity for the players (HLS shows one
  `EXT-X-DISCONTINUITY`). To switch inputs without a gap, add the new input
  first, above the old one, wait for the fail-back hold of 10 s so that it
  takes over, then remove the old one.
- `rebuild`: anything else (the ladder's renditions, `no_fallback`, adding or
  removing the whole `transcode`, `layout`, `thumbnail`): the stream is
  rebuilt, and its viewers reconnect.

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
log and in `GET /api/v1/config/status`. A file that cannot be read at reload,
or whose mode is no longer 0600, is 409 `config_unreadable` with the reason.

### Assets

Watermark images and fonts are assets: regular files directly in
`server.assets_dir`, which the API manages. A watermark names them
(`watermark.image`, `watermark.font`).

| Route | Role | |
|---|---|---|
| `GET /api/v1/assets` | viewer | every asset: `name`, `kind`, `type`, `size`, `modified`, `etag`, and `used_by`, the streams that name it |
| `GET /api/v1/assets/{name}` | viewer | the bytes, with an `ETag` |
| `PUT /api/v1/assets/{name}` | editor | creates (201) or replaces (200) it |
| `DELETE /api/v1/assets/{name}` | editor | removes it (204) |

A name is lower case: a letter or digit, then up to 63 letters, digits, `_` or
`-`, then `.png`, `.jpg` or `.jpeg` for an image, `.ttf` or `.otf` for a font.
Any other name is 404. An image is at most 1 MiB and 2048 pixels a side and
must be what its extension says; a font is at most 24 MiB. The directory holds
at most 256 assets and 512 MiB in all (507 `assets_full` beyond). Without
`server.assets_dir` every route answers 409 `assets_off`.

Upload the raw bytes:

```sh
curl -u ops -X PUT http://127.0.0.1:8081/api/v1/assets/logo.png \
  -H 'X-Requested-By: curl' -H 'Content-Type: application/octet-stream' \
  --data-binary @logo.png
```

`If-Match` (a tag, or `*`) makes it a replacement of that version, and
`If-None-Match: *` a creation only; a failed condition is 412. Content the
name does not allow is 422 `invalid_asset` with the reason, and a body over
the limit is 413; a refused upload stores nothing. The answer is the asset
with two lists:

- `refreshed`: one entry per running stream that names the asset, with `ok`
  when its picture took the new bytes (and `error` when not). The upload
  reaches those streams without an edit or a restart.
- `retried`: the streams that were left stopped because the asset was missing
  are tried again, and start with the upload (`started`; `unchanged` when
  something else still stands in their way).
  Nothing is tried again while the config holds an entry that was skipped at
  start: fix or remove that entry, and the edit tries them.

A `DELETE` of an asset that a stream names, running or disabled, is 409
`asset_in_use` with the stream names: take it out of their watermarks first.
While the config holds an entry that was skipped at start it is 409
`config_invalid_entries`, since nobody can tell what that entry names.

An upload has 30 s plus 1 s per 256 KiB of its kind's limit to arrive (34 s
for an image, 126 s for a font), so a link slower than about 2 Mbit/s cannot
upload the largest font. At most two uploads are read at once; a third waits
up to 30 s, then gets 503.

Files copied into the directory by hand are assets too, but a running stream
takes them only at its next rebuild or edit; upload through the API to reach
it at once. An asset must be a regular file: a symbolic link is not one, so a
Kubernetes ConfigMap or Secret mount, which is made of links, does not work as
an assets directory; copy the files in instead. A hand edit is outside the
API's contract: a file replaced by hand with one of the same size within the
same timestamp tick may keep its old `ETag`, so an `If-Match` taken before the
hand edit can still pass.

**Fonts.** `watermark.font` names a font asset for a text watermark; without
it the text is drawn in DejaVu Sans. A font is a TTF or OTF file with one face:
collections (`.ttc`) and WOFF are refused, so extract the face you need. When
the server has a transcoder, an upload is also checked by it
(`ostranscoder -probe font`, at most 10 s), which loads the font and draws
with it: a font it cannot use is 422 `invalid_asset` with its reason. A check
that cannot run (an old transcoder bundle, a timeout) says nothing about the
font: it is 503 `probe_failed`, logged, and nothing is stored.

**`no_fallback`.** A rendition with `encoder: nvenc` falls back to the software
encoder (libx264) when NVENC cannot be opened, and counts it in
`openstreamer_transcoder_encoder_fallbacks_total`. With `no_fallback: true` it
does not: the transcoder fails instead, and since a ladder opens all its
renditions or none, **the stream's other renditions go down with it**, the
software ones included. The stream keeps reading its source, its ladder
outputs carry nothing, and the transcoder is retried (250 ms, doubling to
5 s) until NVENC opens. Each refusal counts in
`openstreamer_transcoder_encoder_refusals_total{stream}` and in
`transcoder.encoder_refusals` of the stream's status, with the reason in its
`last_error`.

### Running it as a service

Run `openstreamer` under a service manager that restarts it (systemd with
`Restart=always`, a container with a restart policy). There is no per-stream
panic recovery: a panic in one stream's code ends the whole process, and the
restart brings every stream back from the config file. SIGTERM stops it in
order; a second SIGTERM, or the manager's kill after its timeout, ends it at
once.

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

`audio_language` declares the language of a stream's audio. It is a BCP 47
tag such as `vi` or `en-US` (a primary subtag of 2 or 3 letters, then
subtags of 1 to 8 letters or digits, 35 bytes at most), checked for its
form only and written as given. It appears as `LANGUAGE` in the HLS masters
(`cmaf/master.m3u8`, and the `<name>/hls/index.m3u8` of a stream with a
ladder, whose per-rendition `<name>_<label>/hls/index.m3u8` are media
playlists and carry none, though their CMAF master and MPD do) and as
`lang` in the DASH MPD. HLS (TS) of a stream without a ladder is a single
media playlist with no master, so it carries no language. A stream whose
`layout` has no audio refuses the field. Changing it is a `live` edit: the
next master or MPD a player fetches has the new value.

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
