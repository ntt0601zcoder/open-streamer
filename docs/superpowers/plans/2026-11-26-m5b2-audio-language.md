# M5b2: Audio Language Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A stream declares the language of its audio (`audio_language: vi`). The HLS masters write it as `LANGUAGE` on an `EXT-X-MEDIA` audio entry, the MPD writes it as `lang` on the audio AdaptationSet, `osconform` reads and checks both, and an edit to it is applied to the running stream without ending a session.

**Architecture:** The tag's syntax checks live in `internal/media`. The spec uses a strict check and conform uses RFC 5646's looser syntax. `pkg/hls` and `pkg/dash` carry the attribute. The packager takes the language as an option that can be changed while it runs (`SetAudioLanguage`). Each `Snapshot` carries the language, so every playlist built from that snapshot agrees with the others. The spec classes the field as a live change, and `applyLive` hands it to the packagers.

**Tech Stack:** Go 1.26, stdlib only. Peers: ffmpeg 8 on the host, and ffmpeg 6.1, VLC 3.0.20 and GStreamer 1.24 in `osv2-ci-peers:m4a`.

**Spec:**
- `docs/REBUILD_PLAN.md`, the M5b2 row in §5.
- The ruling in `docs/superpowers/plans/2026-11-26-m5b1-live-edits-assets.md`, section "Hand-over: M5b2". It says: "the audio language is one declared value for the stream's single audio track … written as `LANGUAGE` on an `EXT-X-MEDIA` audio entry of the HLS masters and as `lang` on the MPD's audio AdaptationSet, with `osconform` reading both … Several audio tracks chosen and labelled by language … are a milestone of their own, with M4b's multi-audio deferred minors."

## Rules (R-L)

- **R-L1 (scope).** M5b2 is the single declared language. The §5 row's clause "M4b's multi-audio minors" conflicts with the hand-over ruling, which sends those minors to the multi-audio milestone. The hand-over is the later and more specific decision, so it wins. Task 6 rewrites the row to match. The out-of-scope items are:
  - several audio tracks, chosen or labelled by language;
  - languages from the source (the PMT, the HLS rendition list, the SDP);
  - an `ISO_639_language_descriptor` in TS (HLS-TS, HTTP-TS, SRT, push);
  - RTMP and RTSP metadata;
  - an `osdev` flag.
- **R-L2 (the value).**
  - `audio_language` is a BCP 47 tag, checked for syntax only, by `media.ValidLanguage`:
    - one primary subtag of 2–3 ASCII letters;
    - then zero or more `-` subtags of 1–8 ASCII letters or digits;
    - 35 bytes at most.
  - The registry is not consulted. The value is stored and written exactly as given, with no case folding.
  - Empty means "not declared". Nothing is then written anywhere, and every playlist is byte-for-byte what M5b1 writes (frozen behaviour).
  - A stream whose layout has no audio refuses a declared language: `audio_language: the layout has no audio`.
  - The packager ignores a value that fails `media.ValidLanguage`, which can arrive from `cmd/osdev` or a test. It writes nothing for it.
- **R-L3 (which track).**
  - The language labels the stream's lowest audio track, the one `cmaf/master.m3u8` lists. That is R-B5: the audio track of `lowestTracks(s)`.
  - In the MPD, only the AdaptationSets of that same track number get `lang`, in every period that has the track.
  - The TS master's audio entry describes the audio muxed into its variants. A source with a second audio track carries no language for it (R-L1).
- **R-L4 (where it is written).** These rules apply only when a language is declared and an audio track is known.
  - **`cmaf/master.m3u8` with video:** the existing `TYPE=AUDIO` rendition gains `LANGUAGE="<tag>"`.
  - **`cmaf/master.m3u8` audio-only, and `hls/index.m3u8` in ABR mode:** the audio is inside the variant, so the master gains exactly one entry, `#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="audio",LANGUAGE="<tag>",DEFAULT=YES,AUTOSELECT=YES`, with no `URI`, and every variant gains `AUDIO="aud"`.
    - RFC 8216 §4.3.4.2.1: with no URI, the rendition's media is in the variant.
    - §7 requires no version change.
    - Plan review round 1 played this form with ffmpeg 6.1 and 8 (both report `language=vi`), GStreamer 1.24 and VLC 3.0.20, on static playlists.
  - **`NAME`** stays `"audio"`.
  - **HLS-TS without ABR** has no master (its `index.m3u8` is a media playlist), so it carries no language. The MPD and the CMAF master still do.
- **R-L5 (edits).**
  - A change to `audio_language` alone, or one combined only with the inputs, the watermark or the outputs, is `ChangeLive`.
  - `applyLive` hands the new value to every running packager. No packager restarts, no session ends, and segment numbering goes on.
  - The next master or MPD fetched after the answer carries the new value.
  - Packagers started later start with the value in force: `startPackagers` runs after `setSpec` on every path.
- **R-L6 (conform).**
  - `conform.Capture.AudioLanguages` lists the distinct languages read, in the order first seen. Sources are the `TYPE=AUDIO` renditions of every master version read, and the `lang` of every audio AdaptationSet of every MPD version read.
  - An audio set is one whose `contentType` is `audio`, or whose `mimeType` starts with `audio/`. The `mimeType` is taken from the set, or else from its first representation.
  - Rule `audio-language` is a violation in four cases:
    - **(a)** a value fails `media.ValidLanguageSyntax`, the RFC 5646 syntax (conform reads other packagers' output);
    - **(b)** `ReadOptions.AudioLanguage` is set and an audio rendition or audio set carries a different value;
    - **(c)** `ReadOptions.AudioLanguage` is set and a master or MPD version that has audio carries the expected value on none of its audio renditions or sets. "Has audio" means an audio rendition, an `AUDIO=` group, an audio entry in CODECS (`mp4a`, `ac-3`, `ec-3`, `opus`), or an audio set;
    - **(d)** `ReadOptions.AudioLanguage` is set and the URL read is a media playlist ("no master to carry a language").
  - An unlabelled extra audio set is not a violation (R-L3). Each violation is reported once per (URL, value) pair.
  - `osconform -audio-language <tag>` sets the option. A value failing `media.ValidLanguage` exits 2. When `AudioLanguages` is non-empty, osconform prints `osconform: audio languages: <a>, <b>` on stderr.

## Global Constraints

- **Libraries:** no new module, stdlib only. Never use bluenviron or lal, and never copy another product's code.
- **Frozen behaviour:** with `audio_language` empty, every master, media playlist and MPD is byte-identical to before. Each task that touches a writer keeps or adds a golden test that pins this.
- **Lint:**
  - `exhaustive` (a switch on an enum lists every value);
  - `godot` (top-level comments end with a period);
  - gofumpt formatting;
  - errors wrapped as `module: op: %w`, never both logged and returned.
- **Tests:**
  - Use `127.0.0.1:0` only. Never bind 554, 1935 or 8554.
  - Every test that runs a peer passes on the host (ffmpeg 8) and in `make test-ci-peers` before any push.
  - Real-child tests use `OS_TRANSCODER=<repo>/dist/ostranscoder-amd64/ostranscoder`.
  - NVENC stays opt-in.
  - The arm64 job has only ffmpeg and sets `OS_REQUIRE_PEERS=1`, so any test listed there must not need VLC or GStreamer.
- **Environment:**
  - The v1 soak containers are never touched.
  - Own containers are named `osv2-*` and run with `--rm`.

## Review Focus

1. **Players and the no-URI audio entry, over a live sliding window.** ffmpeg 6.1/8, GStreamer 1.24 and VLC 3.0.20 are given the ABR TS master with its `URI`-less `TYPE=AUDIO` entry and the variants' `AUDIO="aud"`. They must play it with sound, as before. *Pinned by Task 6's `TestM5b2Players`.*
2. **Our own HLS pull of that master.** It must still read the muxed audio. *Pinned by Task 1's ingest row and Task 6's pull row.*
3. **A hostile or odd value.** `vi",URI="x`, a newline, `EN-us`, `zh-Hant-TW`, a 36-byte tag, `e`, `english`, `x-klingon`:
   - quotes and controls are refused by the spec and dropped by the packager, so `Encode` can never be made to write a second attribute;
   - case is kept;
   - the spec refuses private-use-first and over-long tags, which conform still reads as syntax-valid.

   *Pinned by the tables in Tasks 1, 2, 3 and 5.*
4. **A declared language with a layout of `["video"]`.** `spec.Validate` refuses it with R-L2's text, and so does every config and API write. It is not silently ignored. *Pinned by Task 2.*
5. **A live edit while a player polls.** The language changes from `vi` to `en` and then is removed:
   - each answer is `live`;
   - the generation and the packager pointers are unchanged;
   - no `openstreamer_output_sessions_ended_total` series moves;
   - the media sequence goes on;
   - the next master and MPD show the change;
   - after the removal they carry no language.

   *Pinned by Task 4 and Task 6.*

---

## Execution waves

| Wave | Tasks | Why |
|---|---|---|
| 1 | 1 (`media`, `pkg/hls`, `pkg/dash`, an ingest row) | Everything consumes it |
| 2 | 2 (`spec`, openapi), 3 (`live` packager) | Disjoint packages; each needs only Task 1 |
| 3 | 4 (`stream`), 5 (`conform`, `osconform`) | 4 needs 2 and 3. 5 follows 3 because `cmd/osconform`'s tests import `internal/output/live` and `live`'s tests import `conform` |
| 4 | 6 (e2e, docs, CI) | Needs all |

Each task runs in its own worktree.

---

### Task 1: The tag checks, the attributes in `pkg/hls` and `pkg/dash`, and the pull

**Files:**
- Create: `internal/media/language.go`, `internal/media/language_test.go`
- Modify: `pkg/hls/multivariant.go`, `pkg/dash/mpd.go`
- Test: `pkg/hls/hls_test.go` (beside `TestMultivariantEncodeGolden`), `pkg/dash/mpd_test.go`, `internal/ingest/hls_test.go`

**Interfaces:**
- Produces:

```go
// ValidLanguage reports whether s is a language tag the server accepts (R-L2): a primary subtag of 2–3 ASCII letters,
// then zero or more "-"-separated subtags of 1–8 ASCII letters or digits, 35 bytes at most.
func ValidLanguage(s string) bool // internal/media
// ValidLanguageSyntax reports whether s is a BCP 47 tag by RFC 5646's syntax: subtags of 1–8 ASCII letters or digits separated
// by "-", the first of 2–8 letters, or the private-use "x" or grandfathered "i" singleton followed by at least one subtag; no length cap.
func ValidLanguageSyntax(s string) bool // internal/media

// In hls.Rendition: Language string // LANGUAGE; written after NAME when not empty, read by ParseMultivariant.
// In dash.AdaptationSet: Lang string `xml:"lang,attr,omitempty"` // after mimeType in field order.
```

- [ ] **Step 1: Write the failing tests.**

```go
func TestValidLanguage(t *testing.T) // true: "vi", "en", "en-US", "EN-us", "zh-Hant-TW", "yue", "sr-Latn-RS-1994"; false: "", "e",
// "english", "x-klingon", "en_US", "en-", "-en", "en--US", "vi\"", "vi\n", "vi ", "en-abcdefghi", a 36-byte valid-shaped tag
func TestValidLanguageSyntax(t *testing.T) // true: everything TestValidLanguage accepts, "x-klingon", "i-klingon", "english", a 40-byte
// tag of valid subtags; false: "", "e", "x", "x-", "en_US", "en--US", "vi\"", "vi\n", "en-abcdefghi"
func TestEncodeRenditionLanguage(t *testing.T) // Rendition{Type:"AUDIO",GroupID:"aud",Name:"audio",Language:"vi",Default:true} encodes exactly
// `#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="audio",LANGUAGE="vi",DEFAULT=YES,AUTOSELECT=YES`; with URI the URI follows AUTOSELECT;
// Language "" writes no LANGUAGE (TestMultivariantEncodeGolden stays byte-identical)
func TestParseRenditionLanguage(t *testing.T) // round trip of the above; a master from another packager with LANGUAGE="en-US" and no URI
func TestMPDAdaptationSetLang(t *testing.T) // AdaptationSet{ContentType:"audio",MimeType:"audio/mp4",Lang:"vi"} marshals with lang="vi";
// Lang "" writes no lang attribute; unmarshal reads it back
func TestHLSPullAudioInVariant(t *testing.T) // internal/ingest: an httptest master with R-L4's URI-less line and AUDIO="aud" on two TS
// variants: the pull fetches only the chosen variant's playlist (never a rendition playlist) and its reader yields audio and video frames
```

- [ ] **Step 2: Run** `go test -count=1 ./internal/media/ ./pkg/hls/ ./pkg/dash/ ./internal/ingest/`. Expect a compile failure.
- [ ] **Step 3: Implement.** Both checks are hand-written scans. Do not use `regexp`.
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/media/ ./pkg/... ./internal/ingest/`. Expect a pass.
- [ ] **Step 5: Commit** `feat(hls, dash): a language on an audio rendition and an adaptation set`

---

### Task 2: `spec.Stream.AudioLanguage`

**Files:**
- Modify:
  - `internal/spec/spec.go`, `internal/spec/validate.go`, `internal/spec/diff.go`
  - `internal/stream/manager.go:35`: the `appliedLive` comment only, which gains "or the audio language"
  - `internal/api/openapi.json`: the `Stream` schema's new property, and the `Outcome` description, which becomes "live: the inputs, the watermark or the audio language of a running stream were changed in place; no session ended."
- Test: `internal/spec/validate_test.go`, `internal/spec/diff_test.go`

**Interfaces:**
- Consumes: `media.ValidLanguage` (Task 1).
- Produces: `spec.Stream.AudioLanguage string` with tags `json:"audio_language,omitempty" yaml:"audio_language,omitempty"`. The field goes after `Layout`. `Diff` classes a change to it as `ChangeLive` (R-L5).

- [ ] **Step 1: Write the failing tests.**

```go
func TestValidateAudioLanguage(t *testing.T)
// accepted: "vi", "EN-us" (kept as given), "zh-Hant-TW"; refused with "spec: validate: audio_language: …":
// `vi",URI="x` and "vi\n" and "english" and "x-klingon" and a 36-byte tag → "audio_language: <Quote(v)> is not a language tag (BCP 47), such as vi or en-US";
// layout ["video"] with "vi" → "audio_language: the layout has no audio"; layout ["video"] with "" → accepted
func TestDiffAudioLanguage(t *testing.T) // "" → "vi": ChangeLive; "vi" → "en": ChangeLive; with an input change too: ChangeLive;
// with outputs too: ChangeLive; with a layout change too: ChangeRebuild; equal: ChangeNone
```

- [ ] **Step 2: Run** `go test -count=1 ./internal/spec/`. Expect it to fail.
- [ ] **Step 3: Implement.**
  - The check sits after the layout check in `validate`.
  - In `Diff`, clear `AudioLanguage` on both copies beside `Inputs` before the live comparison. Update `Diff`'s doc comment and the `ChangeLive` comment.
  - In `openapi.json` add `audio_language`: string, `maxLength` 35, description "BCP 47 tag of the stream's audio, written to the HLS masters and the MPD; empty: not declared."
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/spec/ ./internal/api/ ./internal/stream/`. Expect a pass.
- [ ] **Step 5: Commit** `feat(spec): audio_language, checked, and a live change`

---

### Task 3: The packager writes the language

**Files:**
- Modify:
  - `internal/output/live/packager.go` (`Options`, `Packager`)
  - `internal/output/live/store.go` (`Snapshot`)
  - `internal/output/live/hls.go` (`buildMaster`, `buildTSMaster`)
  - `internal/output/live/dash.go` (`buildSet`, and `buildPeriod` if it must pass the track)
- Test: `internal/output/live/hls_test.go`, `internal/output/live/dash_test.go`, `internal/output/live/packager_test.go`

**Interfaces:**
- Consumes: `media.ValidLanguage`, `hls.Rendition.Language`, `dash.AdaptationSet.Lang` (Task 1).
- Produces:

```go
// In live.Options: AudioLanguage string // the declared language of the audio (R-L2); "" or an invalid tag writes none.
// SetAudioLanguage replaces the declared language; the next Snapshot, and so every playlist built after the call returns, carries it.
// A value failing media.ValidLanguage is stored as "".
func (p *Packager) SetAudioLanguage(lang string)
// In live.Snapshot: AudioLanguage string // the packager's language when the snapshot was taken
```

- [ ] **Step 1: Write the failing tests.**

```go
func TestMasterAudioLanguage(t *testing.T)
// a snapshot with video track 0 and audio track 1, AudioLanguage "vi": cmaf/master.m3u8 has the AUDIO rendition with LANGUAGE="vi" and
// its URI a1.m3u8; with "" the body equals the existing golden byte for byte
func TestMasterAudioOnlyLanguage(t *testing.T) // audio only, "vi": one URI-less AUDIO rendition (R-L4's exact line) and the variant has
// AUDIO="aud"; with "": no EXT-X-MEDIA and no AUDIO= (golden)
func TestTSMasterAudioLanguage(t *testing.T) // ABR, two video tracks and one audio track, "vi": the URI-less rendition, both variants
// AUDIO="aud", CODECS and EXT-X-VERSION:3 unchanged; with "" the golden; ABR without any audio track and "vi": no rendition, no AUDIO=
func TestMPDAudioLanguage(t *testing.T) // two audio tracks (1 and 2) and "vi": track 1's AdaptationSet has lang="vi" in every period
// that has it, track 2's has none, video sets have none; with "": no lang anywhere (golden)
func TestSetAudioLanguage(t *testing.T) // a running packager: SetAudioLanguage("en") then Snapshot().AudioLanguage == "en" and the next
// master has LANGUAGE="en"; SetAudioLanguage(`vi",URI="x`) and Options.AudioLanguage "english" store ""; concurrent SetAudioLanguage
// and playlist requests under -race; Options.AudioLanguage seeds the first value
```

- [ ] **Step 2: Run** `go test -count=1 ./internal/output/live/`. Expect a compile failure.
- [ ] **Step 3: Implement.**
  - The language lives in an `atomic.Pointer[string]` on `Packager`. `Snapshot()` copies it into `Snapshot.AudioLanguage`.
  - Bodies are built per request from a fresh snapshot (`http.go`), so no cache needs keying.
  - `buildSet` sets `Lang` when the set is audio and `et.Track` equals `lowestTracks(s)`'s audio track.
  - Update the comments of `buildTSMaster` ("no audio rendition") and `buildMaster` to R-L4.
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/output/live/`. Expect a pass.
- [ ] **Step 5: Commit** `feat(live): the declared audio language in the masters and the MPD`

---

### Task 4: `stream` applies the language

**Files:**
- Modify: `internal/stream/generation.go` (`startPackagers`), `internal/stream/live.go` (`applyLive`, a step 5, and its doc comment at ~93–97)
- Test: `internal/stream/live_test.go`, `internal/stream/manager_test.go`

**Interfaces:**
- Consumes: `spec.Stream.AudioLanguage` and its `ChangeLive` class (Task 2); `live.Options.AudioLanguage` and `(*live.Packager).SetAudioLanguage` (Task 3).
- Produces: nothing new to later tasks.

- [ ] **Step 1: Write the failing tests.**

```go
func TestSupervisorLiveAudioLanguage(t *testing.T)
// HLS on, audio_language "vi": every packager of the view has Snapshot().AudioLanguage "vi". Submit with "en": ChangeLive; generation,
// Since and the *live.Packager pointers in View().Packs are unchanged; every packager now reports "en". Submit with "": ChangeLive, "".
// Then HLS and DASH off, Submit with "fr" (ChangeLive, no packager), then HLS on (ChangeOutputs): the new packagers start with "fr".
func TestManagerAudioLanguageOutcome(t *testing.T) // Replace with only audio_language changed: Outcome "live"; with it and layout
// ["audio","video"]: "rebuild"
```

- [ ] **Step 2: Run** `go test -count=1 -run 'TestSupervisorLiveAudioLanguage|TestManagerAudioLanguageOutcome' ./internal/stream/`. Expect it to fail.
- [ ] **Step 3: Implement.**
  - `startPackagers` sets `opt.AudioLanguage = s.spec.AudioLanguage`. Read it directly, as `build` does: the Run goroutine is the only writer.
  - `applyLive` gains a step 5, after the outputs: when `old.AudioLanguage != new.AudioLanguage`, call `SetAudioLanguage(new.AudioLanguage)` on every `g.packs` entry.
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/stream/` and `go test -race -count=10 -run 'TestSupervisorLiveAudioLanguage' ./internal/stream/`. Expect both to pass.
- [ ] **Step 5: Commit** `feat(stream): an audio language edit reaches the running packagers`

---

### Task 5: `conform` and `osconform` read the languages

**Files:**
- Modify:
  - `internal/conform/read.go` (`Capture`, `ReadOptions`, `readMaster`, the media-playlist path of `ReadHLS`, the DASH period reader)
  - `internal/conform/container.go` (the rule constant)
  - `cmd/osconform/main.go`: `run` gains `stderr io.Writer`, and `main` passes `os.Stderr`
- Test: `internal/conform/container_test.go`, `cmd/osconform/main_test.go` (`runConform` returns stdout and stderr)

**Interfaces:**
- Consumes: `media.ValidLanguage`, `media.ValidLanguageSyntax`, `hls.Rendition.Language`, `dash.AdaptationSet.Lang` (Task 1).
- Produces:

```go
// In conform.Capture: AudioLanguages []string // distinct, first-seen order (R-L6)
// In conform.ReadOptions: AudioLanguage string // when set, R-L6 (b)–(d) apply
const RuleAudioLanguage = "audio-language"
```

- [ ] **Step 1: Write the failing tests.** Use the package's existing `httptest` fixtures for HLS and DASH.

```go
func TestReadHLSAudioLanguages(t *testing.T) // a master with an AUDIO rendition LANGUAGE="vi" (URI) → AudioLanguages ["vi"], no violation;
// a URI-less AUDIO rendition LANGUAGE="en-US" → ["en-US"]; LANGUAGE="x-klingon" → ["x-klingon"], no violation; LANGUAGE="e" → one
// audio-language violation; with AudioLanguage "vi": a rendition saying "en" → a violation naming both; a master with variants and
// an mp4a CODECS entry but no AUDIO rendition → violation (c); a media playlist URL → violation (d); a master without audio → none
func TestReadDASHAudioLanguages(t *testing.T) // an audio set lang="vi" → ["vi"]; video sets are ignored; a set with mimeType audio/mp4
// and no contentType counts as audio; AudioLanguage "vi": two audio sets, one lang="vi" and one unlabelled → no violation; one audio
// set without lang → violation (c); polling an MPD that changes vi → en gives ["vi","en"], each violation once
func TestOsconformAudioLanguage(t *testing.T) // -audio-language vi against a fixture with vi: exit 0, stdout empty and stderr has
// "osconform: audio languages: vi"; against en: exit 1 and stdout has the audio-language violation; -audio-language "bad tag": exit 2
```

- [ ] **Step 2: Run** `go test -count=1 ./internal/conform/ ./cmd/osconform/`. Expect it to fail.
- [ ] **Step 3: Implement.** `readMaster` records the languages for every version it reads, the final read in once mode included. The DASH reader does the same on each MPD it polls.
- [ ] **Step 4: Run** `go test -race -count=1 ./internal/conform/ ./cmd/osconform/`. Expect a pass.
- [ ] **Step 5: Commit** `feat(conform): read and check the audio language`

---

### Task 6: The M5b2 scenarios, docs and CI

**Files:**
- Create: `internal/e2e/m5b2_test.go`
- Modify: `.github/workflows/ci.yml` (`E2E_TESTS`), `docs/REBUILD_PLAN.md`, `README.md`, `cmd/openstreamer/openstreamer.example.yaml`, `CLAUDE.md` (one clause)

**Interfaces:** acts as an API client and as a peer driver. It proves the M5b2 exit criterion: "`osconform` reads the languages".

- [ ] **Step 1: Write the tests.** Build them on M5a's harness (`m5aServer`, `m5aOpen`, `assertUninterrupted`, `metric`), M5b1's `m5b1Clip` and M4b's replay (`liveABR`, `vlcPlays`, `gstPlays`). Each test is `t.Parallel()`.

```go
// TestM5b2AudioLanguage: a passthrough stream cam (m5b1Clip; HLS, DASH and HTTP-TS on; audio_language "vi").
// conform.ReadHLS(cmaf/master.m3u8, Once, AudioLanguage "vi") and conform.ReadDASH(cmaf/index.mpd, Once, AudioLanguage "vi"): no
// violation, AudioLanguages ["vi"]. ffprobe (host, and in the peers image) of the CMAF master and of the MPD reports the audio stream's
// language tag "vi". An m5aOpen row with a session reader on HTTP-TS; PUT cam with "en": "live", generation unchanged; within 5 s both
// reads report ["en"]; PUT with "": "live"; the master and the MPD carry no LANGUAGE and no lang; no
// openstreamer_output_sessions_ended_total series moved, assertUninterrupted(row). Budget 40 s.
func TestM5b2AudioLanguage(t *testing.T)

// TestM5b2AudioLanguageLadder: the real child; a ladder of two x264 rungs (180, 120) and audio copy; HLS and DASH on; "vi".
// hls/index.m3u8 (the ABR TS master) has the URI-less AUDIO entry with LANGUAGE="vi" and AUDIO="aud" on both variants;
// conform.ReadHLS of it with AudioLanguage "vi" and AlignedSequences: no violation and AudioLanguages ["vi"]; ffprobe (host) of it
// reports language "vi" on the audio stream. A second stream pull reads http://…/cam/hls/index.m3u8 as its input: its hub gets audio
// and video frames within 15 s. ffmpeg only, so it runs in the arm64 job too. Budget 45 s.
func TestM5b2AudioLanguageLadder(t *testing.T)

// TestM5b2Players: M4b's replay (liveABR with live.Options{ABR: true, AudioLanguage: "vi"}) serves the ABR TS master over a live
// sliding window; ffprobe 6.1 and 8 report language "vi"; vlcPlays and gstPlays, unchanged, pass on hls/index.m3u8 and on
// cmaf/master.m3u8 with audio decoded. Peers only: amd64 test job, not the arm64 list.
func TestM5b2Players(t *testing.T)
```

- [ ] **Step 2: Run** `go test -race -count=1 -run '^TestM5b2' ./internal/e2e/` on the host, with `OS_TRANSCODER` set. Expect a pass within each test's budget.
  - Then `-count=5` must pass 5 of 5.
  - Then `make test-ci-peers ARGS='-run ^TestM5b2 ./internal/e2e/'` must pass.
  - If anything fails, use superpowers:systematic-debugging. A code fix gets a test in the package that owns the code.
- [ ] **Step 3: CI.** Add `TestM5b2AudioLanguageLadder` to `E2E_TESTS` of `transcode-e2e-arm64`. Its listed-tests guard checks the name. `TestM5b2Players` stays out.
- [ ] **Step 4: Docs.**
  - **`docs/REBUILD_PLAN.md`:**
    - §5: the M5b2 row reads "Audio language: the spec field, the HLS masters' `LANGUAGE`, the MPD's `lang`" (R-L1), with plan link, "done", and "Proved by" the three tests plus the media, hls, dash, ingest, spec, live, stream and conform suites.
    - Add a §5 row **Multi-audio**: "several audio tracks chosen and labelled by language, languages from the source, the TS language descriptor, M4b's multi-audio minors", depending on M5b2, not yet planned.
    - Create an "Out of M5b2" table holding R-L1's items, pointing at that row.
    - Rewrite the "Several audio languages" known-limit row (near line 999): one declared language is written; selection among several waits for Multi-audio.
    - §3.6 Edits: the live class includes the audio language.
    - Add an "M5b2 deferred minors" list built from the task reviews.
  - **README:** `audio_language`, its form, where it appears, and that HLS-TS without a ladder has no master to carry it.
  - **Example config:** a commented `audio_language: vi` with one line of explanation.
  - **CLAUDE.md:** in the edit outcomes, `live` names the audio language. Keep the length within three lines of the current one.
- [ ] **Step 5: The gate.**
  - `make check`, `make covercheck` on a fresh `make cover`, `make test-ci-peers`, `make test-transcode`.
  - `GOOS=windows go vet` and `GOOS=darwin go vet` (untagged packages).
  - `go mod tidy`: no change.
- [ ] **Step 6: Commit** `test(e2e): the M5b2 scenarios` and `docs: M5b2 is done`.
