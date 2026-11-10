#!/bin/sh
# Generates the MPEG-TS fixtures. Output is bitexact: a re-run changes nothing.
set -eu
cd "$(dirname "$0")"

F="ffmpeg -nostdin -v error -y"
BX="-threads 1 -fflags +bitexact -flags:v +bitexact -flags:a +bitexact -f mpegts"
X264="-c:v libx264 -profile:v main -bf 2 -g 25 -keyint_min 25 -sc_threshold 0"
X265="-c:v libx265 -x265-params keyint=25:min-keyint=25:bframes=2:scenecut=0:pools=1:frame-threads=1:log-level=error"
v() { echo "-f lavfi -i testsrc2=size=$1:rate=25"; }
a() { echo "-f lavfi -i sine=frequency=1000:sample_rate=$1"; }

$F $(v 320x240) $(a 48000) -t 6 $X264 -c:a aac -ac 2 -b:a 64k  $BX h264_aac.ts
$F $(v 320x240) $(a 44100) -t 6 $X265 -c:a aac -ac 6 -b:a 128k $BX h265_aac51.ts
$F $(v 320x240) $(a 44100) -t 6 $X264 -c:a aac -ac 1 -b:a 64k -output_ts_offset 95440 $BX h264_aacmono_wrap.ts
$F $(v 640x360) -t 2 $X264 -an $BX h264_640.ts
$F $(v 320x240) $(a 48000) -t 2 $X264 -c:a mp2 $BX h264_mp2.ts
cat h264_aac.ts h264_aac.ts > h264_aac_loop.ts
cat h264_aac.ts h264_640.ts > h264_aac_then_640.ts
N=$(( $(stat -c%s h264_aac.ts) / 188 ))
tail -c +$(( 188 * (N * 3 / 8) + 1 )) h264_aac.ts > h264_aac_midgop.ts
