Outcome: (a)

# R-T7b': why VLC 3 logs "buffer deadlock prevented" on B-pyramid rungs

M4c Task 9, ruling R-C11. Measured on 2026-11-25 with VLC 3.0.20
(libavcodec 60.31, 6 decoder threads) in `osv2-ci-peers:m4a`, on rung `t180`
of `h264_aac.ts` (240x180, 25 fps, GOP 2 s) served as live HLS TS.

## Result

The line is a limit of VLC 3, reached by a conformant stream. Keep
`b-pyramid=none` on x264 rungs and `b_ref_mode=disabled` on NVENC rungs.

**The upstream cause is VLC's clock reset, not the line.** Each reset
flushes the decoders and buffers 1 s again, which a viewer notices. The
line is only logged on top of some resets and does not itself stop
playback: `input_DecoderWait` gives up waiting and the play goes on. Our
encoder settings decide whether the line is logged, not whether VLC resets.
Why VLC resets is under "Not determined".

1. **Where.** Never at the start of a play. It comes 4, 6 or 8 s in, each
   time VLC fetches a new segment at the live edge. VLC then logs
   `ES_OUT_SET_(GROUP_)PCR is called too late`, resets its clock
   (`ES_OUT_RESET_PCR`), flushes its decoders and buffers 1000 ms of PCR time
   again (`Stream buffering done (1002 ms in 1 ms)`).
2. **What the decoder is fed.** After the flush the video decoder is fed from
   the new segment's IDR. Our mux writes the PCR 700 ms behind the DTS
   (`internal/ts/mux.go`, `pcrOffset`), so of the 1000 ms only about 300 ms
   are video of the new segment. At 25 fps that was **8 access units** in
   every reset that logged the line, and at least 9 in the others (the
   exact count is only known where the line was logged, see Step 3).
3. **What the decoder needs.** When the buffering is done, es_out waits in
   `input_DecoderWait` for the decoder's *second* picture
   (`src/input/decoder.c`: the first only clears `b_first`). libavcodec's
   frame-threaded H.264 decoder returns its first picture after
   threads − 1 + reorder + 1 packets. With VLC's 6 threads the second picture
   needs **8 packets at reorder depth 1 and 9 at depth 2**.
4. **The line.** With 8 fed and 9 needed, the decoder empties its FIFO and
   goes idle one picture short, and `input_DecoderWait` logs the line and
   gives up waiting. Playback goes on.
5. **Why only some runs.** In most resets at 25 fps (57% to 92% per cell) a picture
   decoded before the flush is delivered after it and counts as the first,
   so one more from the new segment is enough. The line needs a reset
   without such a stray picture.

So B-pyramid matters only through the reorder depth it announces (H1). What
frame threads changed (H2) is the phase of that reset, by one frame: see
"Output cadence" under Step 3.

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
| A (16) | 48 | 1002, 1045, 1109 ms | 11 | 8: all at 1002 and 1109 ms, 8 packets fed; the 3 at 1045 ms were fed at least 9 |
| A again (16) | 47 | 1002, 1045, 1109 | 5 | 2 (and 1 with a stray) |
| B | 16 (one per run) | 1002 | 1 | 0: at least 8 fed, 8 needed |
| C (16) | 48 | **1024**, 1045, 1149 | 4 | 0: at least 9 fed |
| C again (16) | 46 | 1024, 1045, 1149 | 6 | 0 |
| HEAD A (16) | 30 (two per run) | **1024**, 1045 | 13 | 0: at least 9 fed |

In the failing resets the log shows the count directly: after the IDR, seven
more slices are parsed, `Received first picture`, then
`buffer deadlock prevented` 4 µs later. The first picture needs 8 packets at
depth 2 and the second 9, so exactly 8 were fed.

**The fed count is exact only where the line was logged.** `an.py` counts
slices up to `Decoder wait done`, and a decoder that gets its second picture
stops being waited for, whatever is still queued. Elsewhere the count is a
lower bound: "at least" what the decoder needed. The buffered value does not
give the count either: 1002 ms and 1109 ms both fed 8, and 1045 ms at least
9.

One reset is outside the model: in the second run of cell A (run 06), a
reset buffered 1069 ms, a stray picture was delivered, and the line was
logged all the same. That is 1 of about 700 resets in the display logs.

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
| A with the PCR 400 ms behind the DTS: more video fed, at least 9 packets | 0 | **0 of 16** (4 resets without a stray picture) |
| B with the PCR 780 ms behind: 5 or 6 slices parsed before the line | the line returns | **16 of 16** |
| HEAD A with `--avcodec-threads=7`: 10 needed | the line returns | **2 of 16**, in 2 of 4 resets without a stray picture |

One change on the decoder's side and one on the stream's each take the line
to 0 of 16 with the pyramid on, and their inverses bring it back with the
pyramid off. The cause is found: frames fed in VLC's re-buffer against the
packets its decoder needs for a second picture.

## Step 5: two cells outside the sample (fix round 1)

**15 fps, HEAD, shipping settings.** `h264_aac_15fps.ts`, rung `t180`,
`b-pyramid=none` (SPS reorder 1, DPB 2), the audio hold on, instrumentation
only:
`OS_T7B_FIXTURE=h264_aac_15fps.ts cell.sh h15 ""`.

| | Predicted | Measured |
|---|---|---|
| Resets | not predicted | **one per run in 16 of 16**, at 6.05 s, when `6.ts` arrives; buffered 1002 ms |
| Packets needed for the second picture | 8 | no picture at all was returned before the line, so fewer than 7 were fed |
| Packets fed | 5 or 6 | 4 slices parsed before the line in 7 resets, 5 in 9 |
| Stray picture | not predicted | none in 16 resets |
| The line | in every reset without a stray picture | **16 of 16 runs** |

So VLC does reset at HEAD with shipping settings, and at 15 fps the line
follows every time. The second cell (audio hold off) was not needed.

**The copied source, pre-fix tree.** Rendition `src` (the 320x240 source
video copied, the audio re-encoded):
`OS_T7B_RUNG=src oldcell.sh oSrc vlcv-base`. **0 of 16**, as the spec's 0 of
8. Its SPS announces reorder 2 and DPB 4 (Main profile), the same depth as
the pyramid, so it needs 9 packets. It has two resets per run, buffered
1024 and 1045 ms, 11 of 32 without a stray picture, none with the line: at
least 9 fed each time. 1024 and 1045 ms are cell C's and HEAD A's values. A
copy has no encoder lag, so its passes start without the audio lead, and
the reset lands one frame later, as in C.

## What this means for our settings

- `b-pyramid=none` gives depth 1, which needs 8 packets against the 8 fed in
  the pre-fix resets: it fits with **no frame to spare** at 25 fps. HEAD
  with the pyramid fits by one frame only because the audio hold moved the
  reset.
- **Low frame rates are not protected.** At 15 fps HEAD with shipping
  settings shows the line in 16 of 16 plays (Step 5). A fit on the two
  leads measured (700 ms: 8 fed; 780 ms: 5 or 6 parsed) puts the fed count
  near (1000 ms − PCR lead) / frame duration + 1, which gave 5 or 6 for
  15 fps against 4 or 5 parsed. It is a fit, not a formula: the same lead
  fed 8 at two buffered values and at least 9 at a third.
- Whether VLC resets depends on the stream. At 25 fps HEAD with
  `b-pyramid=none` and `--avcodec-threads=8` showed no reset in 16 runs, so
  that cell says nothing about the margin; at 15 fps the same settings reset
  once in every run.
- A shorter PCR lead is a lever on our side (400 ms measured: 0 of 16 with
  the pyramid, one cell). **Ruled out for M4c by the controller:** it
  changes every TS output, it narrows the A/V skew the mux tolerates, and
  one cell is its only evidence. It goes to the Backlog as a proposal.
- NVENC's `b_ref_mode` raises the announced depth as the pyramid does, so it
  stays disabled on the same grounds. Not measured here.

## Spec edits for Task 12

- `docs/REBUILD_PLAN.md:365`, the keyframes line: replace "0 of 8 runs
  after, against 3 of 8" by the cause and the counts here (17 of 46 plays
  with the pyramid on the tree before the setting, 0 of 16 without), and say
  the line is logged at a clock reset at the live edge, not at the start.
  Do not write "stall": the line does not stop playback.
- `docs/REBUILD_PLAN.md:867–874`, the Backlog entry "Root cause of VLC 3's
  'buffer deadlock prevented'": close it with Outcome (a), or rewrite it to
  what stays open (why VLC resets at the live edge). Its claims that the
  trigger is "specific to our encoded output under x264's frame threads" and
  that sliced threads "cured it" are superseded: the copy has depth 2 and
  passes because it has no audio lead, and sliced threads only shorten that
  lead.
- New Backlog entry, **PCR lead**: proposal to shorten `pcrOffset`
  (`internal/ts/mux.go`, 700 ms today). Evidence: one cell, 400 ms, 0 of 16
  with the pyramid. Costs to weigh: every TS output changes and the A/V
  skew tolerance narrows.
- New Backlog entry, **low frame rates in VLC 3**: at 15 fps with shipping
  settings VLC resets once per play and logs the line in 16 of 16 plays;
  `TestM4bMatrix`'s 15 and 10 fps rows have no VLC play, and a VLC row there
  would fail on the line today.
- `internal/transcode/native/pipeline_test.go:168` said VLC 3 "failed to
  decode"; fixed in this task's fix round, nothing left for Task 12.

## Not determined

- Why VLC finds the PCR late when a segment arrives at the live edge, which
  is the upstream cause, and why the video between the reset point and the
  next IDR does not reach libavcodec (VLC's packetizer waiting for a
  keyframe after the flush is the likely reason). That the lead sets the
  count rests on two cells, 400 and 780 ms.
- Why an audio lead in one segment moves the reset in the next by one AAC
  frame. It was shown by switching it (cell G, HEAD without the hold, and
  the copied source), not explained from VLC's source.
- The one line logged with a stray picture at a 1069 ms buffer.
- Cell counts differ run to run with how often a stray picture is delivered
  (3 of 16 to 8 of 16 for cell A). Another agent's stress run shared the
  host during most cells.
