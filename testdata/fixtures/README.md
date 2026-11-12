# Media fixtures

Generated MPEG-TS clips used by the media, TS demuxer, Clock and end-to-end tests.
Output is bitexact, so regenerating changes no file.

Regenerate (needs ffmpeg with libx264 and libx265):

```
make fixtures
```

| File | Contents | Exercises |
|------|----------|-----------|
| `h264_aac.ts` | 6 s, H.264 Main 320x240 25 fps, AAC-LC stereo 48 kHz | B-frames (`-bf 2`) and closed GOP (25 frames, no scenecut); baseline demux |
| `h265_aac51.ts` | 6 s, HEVC 320x240 25 fps, AAC 5.1 44.1 kHz | Open GOP (CRA plus RASL pictures); HEVC parameter sets; 6-channel AAC |
| `h264_aacmono_wrap.ts` | 6 s, H.264 320x240, AAC mono 44.1 kHz, timestamps offset by 95440 s | PTS wrap about 2.4 s into the clip (33-bit 90 kHz rollover) |
| `h264_640.ts` | 2 s, H.264 640x360, no audio | Video-only stream; second resolution |
| `h264_mp2.ts` | 2 s, H.264 320x240, MP2 audio 48 kHz | Unsupported audio codec |
| `h264_aac_loop.ts` | `h264_aac.ts` twice back to back | One backwards timestamp jump |
| `h264_aac_then_640.ts` | `h264_aac.ts` then `h264_640.ts` | One resolution change plus a timestamp jump |
| `h264_aac_midgop.ts` | `h264_aac.ts` cut at 3/8 of its packets | Mid-GOP start: begins without a keyframe or parameter sets |
| `h264_aac_pmtchange.ts` | `h264_aac.ts` then a 3 s clip with the same codecs on all-new PIDs (0x200 and up, PMT on 0x1100), timeline continuing | PMT change that remaps every track, timestamps unbroken |
| `h264_aac_audiopid.ts` | `h264_aac.ts` then a 3 s clip whose video PID is unchanged and whose audio PID is 0x120, timeline continuing | PMT change that remaps only the audio track |
| `h264_opengop.ts` | 6 s, H.264 Main 320x240 25 fps, `-x264-params open-gop=1:keyint=25:min-keyint=25:scenecut=0 -bf 2`, AAC-LC stereo 48 kHz | H.264 open GOP: IDR first, then non-IDR I slices with a recovery point SEI |
| `h264_opengop_midgop.ts` | `h264_opengop.ts` cut at 3/8 of its packets | Open-GOP feed that starts mid-GOP: the first usable key is a recovery point |

## H.264 open-GOP fixtures

Check (every key after the first is a non-IDR I slice, `slice_type` 7, preceded by an SEI with `recovery_frame_cnt` 0):

```
ffmpeg -i h264_opengop.ts -map 0:v -c copy -bsf:v trace_headers -f null - -v trace
```

Key PTS in `h264_opengop.ts` as the demuxer reports them (90 kHz, first frame at 0; `ffprobe` shows these plus the container start of 133200):
`0` (IDR), `90000`, `180000`, `270000`, `360000`, `450000` (recovery points).
Leading B frames (RASL) in decode order, PTS: `86400`, `172800`, `176400`, `262800`, `266400`.

`h264_opengop_midgop.ts`: the first recovery point after the cut is at PTS `57600` (demuxer time, whose zero is the first PTS in this file); its leading B frames are at `50400` and `54000`. The next keys are at `147600` and `237600`.
