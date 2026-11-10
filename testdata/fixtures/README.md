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
