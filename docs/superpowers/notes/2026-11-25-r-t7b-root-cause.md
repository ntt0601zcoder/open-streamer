Outcome: (a)

# R-T7b': why VLC 3 logs "buffer deadlock prevented" on B-pyramid rungs

M4c Task 9, ruling R-C11. Measured on 2026-11-25 with VLC 3.0.20
(libavcodec 60.31, 6 decoder threads) in `osv2-ci-peers:m4a`, on rung `t180`
of `h264_aac.ts` (240x180, 25 fps, GOP 2 s) served as live HLS TS.

## Result

The line is a limit of VLC 3, reached by a conformant stream. Keep
`b-pyramid=none` on x264 rungs and `b_ref_mode=disabled` on NVENC rungs.

1. **Where.** Never at the start of a play. It comes 4, 6 or 8 s in, each
   time VLC fetches a new segment at the live edge. VLC then logs
   `ES_OUT_SET_(GROUP_)PCR is called too late`, resets its clock
   (`ES_OUT_RESET_PCR`), flushes its decoders and buffers 1000 ms of PCR time
   again (`Stream buffering done (1002 ms in 1 ms)`).
2. **What the decoder is fed.** After the flush the video decoder is fed from
   the new segment's IDR. Our mux writes the PCR 700 ms behind the DTS
   (`internal/ts/mux.go`, `pcrOffset`), so of the 1000 ms only about 300 ms
   are video of the new segment: **8 or 9 access units** at 25 fps.
3. **What the decoder needs.** When the buffering is done, es_out waits in
   `input_DecoderWait` for the decoder's *second* picture
   (`src/input/decoder.c`: the first only clears `b_first`). libavcodec's
   frame-threaded H.264 decoder returns its first picture after
   threads − 1 + reorder + 1 packets. With VLC's 6 threads the second picture
   needs **8 packets at reorder depth 1 and 9 at depth 2**.
4. **The line.** With 8 fed and 9 needed, the decoder empties its FIFO and
   goes idle one picture short, and `input_DecoderWait` logs the line and
   gives up waiting. Playback goes on.
5. **Why only some runs.** In most resets (57% to 92% per cell) a picture
   decoded before the flush is delivered after it and counts as the first,
   so one more from the new segment is enough. The line needs a reset
   without such a stray picture.

So B-pyramid matters only through the reorder depth it announces (H1). What
frame threads changed (H2) is the phase of that reset, by one frame: see
"Sliced threads" below.

## Protocol

A run is `TestM4aVLC`'s set-up: the ladder of `m4aLadder` transcoded once
per cell, rung `t180` replayed four times at its pace into a live packager,
VLC started when five segments are listed and stopped after 10 s. Each run
starts a display instance and a dump instance (`--sout`), both with `-vvv`,
stderr kept with a microsecond stamp per line. A cell is 16 runs. The line
only ever appeared in display instances, at most once per run.

Two trees:

- **pre-fix**: commit `2d67699` (the parent of `8544a68`, which added
  `b-pyramid=none`). Its host has no audio hold (`72bbac7` came later).
- **HEAD**: `67c4774`.

The child's `x264-params`, the PCR offset, a hold of video outputs in the
child and the merger's audio hold were switched by environment variables in
scratch builds. None of that code is committed. The scratch diffs, scripts
and every log are in `dist/t7b-out/` of the Task 9 worktree
(`scratch/`, `pre-fix/<cell>/`, `<cell>/`); `dist/` is not tracked.

Commands, from the worktree, with
`P=scenecut=0:keyint=600:min-keyint=1:rc-lookahead=10`:

```
# HEAD cells (dist/t7b-out/scratch/cell.sh NAME X264PARAMS [VLCARGS]):
OS_T7B_X264=$2 OS_T7B_VLCARGS=$3 OS_T7B_OUT=/src/dist/t7b-out/$1 \
OS_TRANSCODER=$PWD/dist/t7b/ostranscoder \
  sh scripts/test-ci-peers.sh --name osv2-t7b-$1 --exec \
  go test -count=1 -timeout=40m -run '^TestT7bCell$' ./internal/e2e/
# pre-fix cells: the same from an export of 2d67699 (oldcell.sh with the
# bundles dist/vlcv-base, -nopyr, -sliced of 02:34; oldcell2.sh with a
# scratch child built from that tree).
```

The log analysis is `scratch/an.py` (per buffering episode: slices parsed
from the IDR on, where "Received first picture" falls, whether the line was
logged) and `scratch/table.sh`.

## Step 1: baseline

| Cell | Tree | x264 | Runs with the line | SPS reorder / DPB | First IDR PTS − DTS |
|---|---|---|---|---|---|
| A | pre-fix | `b-pyramid=normal`, frame threads | **8 of 16**; again 3 of 16, 2 of 6, and 4 of 8 as `TestM4aVLC -race -count=8` (17 of 46) | 2 / 4 | 7200 (80 ms) |
| B | pre-fix | `b-pyramid=none` | **0 of 16** | 1 / 2 | 3600 (40 ms) |
| C | pre-fix | `b-pyramid=normal:sliced-threads=1` | **0 of 16**; again 0 of 16, and 0 of 16 as cell F | 2 / 4 | 7200 |
| A | HEAD | `b-pyramid=normal` | **0 of 16**; and 0 of 16 as `TestM4aVLC -race -count=16` | 2 / 4 | 7200 |

A's and C's SPS are the same bytes
(`6764000cacd943c67e7c0440000003004000afc803c50a6580`). DTS of the first 12
frames, A and C alike: 45000 to 84600 in steps of 3600; B: 48600 to 88200.
The x264 SEI confirms the settings: A `threads=6 sliced_threads=0`, C
`threads=3 sliced_threads=1 slices=3`, both `b_pyramid=2`.

VLC, the same in every cell: `Stream buffering done (1010 ms in 0–3 ms)` at
the start (1013 ms in B), then `Received first picture` 21 to 57 ms after
the IDR is parsed (hardware decoder probing), then `Decoder wait done`. No
line at the start in any display log. With `--avcodec-threads=16` the
decoder took at least 18 packets before `Decoder wait done` at the start,
so the start is not short of frames.

The symptom does not reproduce on HEAD with the pyramid on. It needed the
pre-fix tree, which is where every cell below ran unless it says HEAD.

## Step 2: cell C

C has A's SPS and 0 of 48, so on the protocol's reading H1 was refuted and
Step 3 followed. Step 3 then showed that C differs from A in how many frames
the decoder is fed, not in what it needs: H1's mechanism holds, C moves the
other side of the comparison.

## Step 3: what differs between A and C

Per reset in the display logs of the pre-fix tree ("stray" is a picture
delivered after the reset and before the new IDR is parsed):

| Cell | Resets | Buffered at each reset | Without a stray picture | of which with the line |
|---|---|---|---|---|
| A (16) | 48 | 1002, 1045, 1109 ms | 11 | 8: all at 1002 and 1109 ms, 8 packets fed; the 3 at 1045 ms were fed 9 |
| A again (16) | 47 | 1002, 1045, 1109 | 5 | 2 (and 1 with a stray) |
| B | 16 (one per run) | 1002 | 1 | 0: 8 fed, 8 needed |
| C (16) | 48 | **1024**, 1045, 1149 | 4 | 0: 9 fed |
| C again (16) | 46 | 1024, 1045, 1149 | 6 | 0 |
| HEAD A (16) | 30 (two per run) | **1024**, 1045 | 13 | 0: 9 fed |

In the failing resets the log shows the count directly: after the IDR, seven
more slices are parsed, `Received first picture`, then
`buffer deadlock prevented` 4 µs later.

- **Slices per picture: no.** Cell E, frame threads with `slices=4` and the
  pyramid: 1 of 16, resets at 1002 ms as in A. Cell F,
  `sliced-threads=1:slices=1`: x264 kept 3 slices
  (`sliced_threads=1 slices=3`), so F is C: 0 of 16.
- **First-picture decode time: no.** From the IDR being parsed to
  `Received first picture`, at resets without a stray picture: A 3.4 ms
  (2.3–5.6, n = 11), C 3.0 ms (1.9–3.5, n = 6), E 7.9 ms (n = 3). Cell A
  with `--avcodec-threads=1`: 0 of 16, 2.1 ms.
- **Output cadence: yes.** The encoder returns packets at the same cadence
  in A and C (the first 12, in ms after the first: A 0 39 79 119 159 199 264
  278 319 358 398 438; C 0 33 70 110 152 234 238 272 310 351 390 430), but
  its lag differs. Before `72bbac7` the merger wrote the audio that the
  child returned ahead of a rung's first IDR straight to the hub, so each
  pass of the clip starts with audio alone: **49 audio frames (1.05 s)
  before the first video in A, B and E, 33 (0.70 s) in C**. The timestamps
  and the PES order of every other segment are identical in A and C (PCR
  700 ms behind the DTS, first 12 video PES of `2.ts` at bytes 376 … 31396
  in A and 376 … 30456 in C). That audio lead shifts where VLC's reset lands
  by one AAC frame (1002 against 1024 ms), which is one video frame more
  after the IDR.
  - Cell G, C with its video outputs held 350 ms in the child (the lead
    back at 49 frames): **5 of 16**, resets at 1002 ms as in A. This is the
    protocol's cell G run in the direction a hold allows.
  - HEAD A with the merger's audio hold switched off: resets at 1002, 1045,
    1109 ms, three per run, as in pre-fix A, and the line in 1 of 16 (only
    2 resets without a stray picture). With the hold: 0 of 32.
- **Strict decode.** `ffmpeg -strict strict -err_detect explode` (6.1) over
  cell A's segments: exit 0, no message, 450 frames, `has_b_frames=2`. The
  output order is what the SPS allows.

## Step 4: the controlled changes

| Change | Prediction | Result |
|---|---|---|
| A (pyramid on) with `--avcodec-threads=5`: second picture after 8 packets | 0 | **0 of 16** (6 resets without a stray picture) |
| B (pyramid off) with `--avcodec-threads=7`: needs 9 | the line returns | **4 of 16**, in all 4 resets without a stray picture |
| A with the PCR 400 ms behind the DTS: 600 ms of video fed | 0 | **0 of 16** (4 resets without a stray picture) |
| B with the PCR 780 ms behind: 5 or 6 packets fed | the line returns | **16 of 16** |
| HEAD A with `--avcodec-threads=7`: 9 fed, 10 needed | the line returns | **2 of 16**, in 2 of 4 resets without a stray picture |

One change on the decoder's side and one on the stream's each take the line
to 0 of 16 with the pyramid on, and their inverses bring it back with the
pyramid off. The cause is found: frames fed in VLC's re-buffer against the
packets its decoder needs for a second picture.

## What this means for our settings

- `b-pyramid=none` gives depth 1, which needs 8 packets against the 8 or 9
  fed: it fits with **no frame to spare** at 25 fps. HEAD with the pyramid
  fits by one frame only because the audio hold moved the reset.
- The fed count is about (1000 ms − PCR lead) / frame duration + 1. **Not
  measured, predicted:** below about 24 fps fewer than 8 frames are fed, so
  the line would appear with `b-pyramid=none` too. The 15 and 10 fps rows of
  `TestM4bMatrix` have no VLC play.
- Whether VLC resets at all depends on the stream. HEAD with
  `b-pyramid=none` and `--avcodec-threads=8` showed no reset in 16 runs, so
  that cell says nothing about the margin.
- A shorter PCR lead is the lever on our side that widens the margin for
  every encoder (400 ms measured: 0 of 16 with the pyramid). 700 ms is
  conformant, so this is a choice, not a fix. It is a change to
  `internal/ts`, outside this task.
- NVENC's `b_ref_mode` raises the announced depth as the pyramid does, so it
  stays disabled on the same grounds. Not measured here.

## Not determined

- Why VLC finds the PCR late when a segment arrives at the live edge, and why
  the video between the reset point and the next IDR does not reach
  libavcodec (VLC's packetizer waiting for a keyframe after the flush is the
  likely reason). The 40 ms of lead per frame in the two PCR cells is the
  evidence that the lead sets the count.
- Why an audio lead in one segment moves the reset in the next by one AAC
  frame. It was shown by switching it (cell G, HEAD without the hold), not
  explained from VLC's source.
- Cell counts differ run to run with how often a stray picture is delivered
  (3 of 16 to 8 of 16 for cell A). Another agent's stress run shared the
  host during most cells.
