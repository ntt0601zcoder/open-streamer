#!/bin/sh
# Generates the MPEG-TS and fragmented MP4 fixtures. Output is bitexact: a re-run changes nothing.
set -eu
cd "$(dirname "$0")"

F="ffmpeg -nostdin -v error -y"
BX="-threads 1 -fflags +bitexact -flags:v +bitexact -flags:a +bitexact -f mpegts"
X264="-c:v libx264 -profile:v main -bf 2 -g 25 -keyint_min 25 -sc_threshold 0"
X265="-c:v libx265 -x265-params keyint=25:min-keyint=25:bframes=2:scenecut=0:pools=1:frame-threads=1:log-level=error"
v() { echo "-f lavfi -i testsrc2=size=$1:rate=25"; }
a() { echo "-f lavfi -i sine=frequency=1000:sample_rate=$1"; }
# Reference timestamps: video packets in decode order as pts,dts,flags.
dts() { ffprobe -v error -select_streams v -show_entries packet=pts,dts,flags -of csv=p=0 "$1" > "$2"; }

$F $(v 320x240) $(a 48000) -t 6 $X264 -c:a aac -ac 2 -b:a 64k  $BX h264_aac.ts
dts h264_aac.ts h264_aac_dts.txt
# h264_aac.ts without B-frames: decode order is presentation order (DTS = PTS).
$F $(v 320x240) $(a 48000) -t 6 $X264 -bf 0 -c:a aac -ac 2 -b:a 64k $BX h264_aac_nob.ts
dts h264_aac_nob.ts h264_aac_nob_dts.txt
$F $(v 320x240) $(a 44100) -t 6 $X265 -c:a aac -ac 6 -b:a 128k $BX h265_aac51.ts
dts h265_aac51.ts h265_aac51_dts.txt
$F $(v 320x240) $(a 44100) -t 6 $X264 -c:a aac -ac 1 -b:a 64k -output_ts_offset 95440 $BX h264_aacmono_wrap.ts
$F $(v 640x360) -t 2 $X264 -an $BX h264_640.ts
$F $(v 640x360) $(a 48000) -t 6 $X264 -c:a aac -ac 2 -b:a 64k $BX h264_aac_640.ts
$F $(v 320x240) $(a 48000) -t 2 $X264 -c:a mp2 $BX h264_mp2.ts
cat h264_aac.ts h264_aac.ts > h264_aac_loop.ts
cat h264_aac.ts h264_640.ts > h264_aac_then_640.ts
N=$(( $(stat -c%s h264_aac.ts) / 188 ))
tail -c +$(( 188 * (N * 3 / 8) + 1 )) h264_aac.ts > h264_aac_midgop.ts
$F $(v 320x240) $(a 48000) -t 6 -c:v libx264 -profile:v main -bf 2 -g 25 -keyint_min 25 -sc_threshold 0 -x264-params open-gop=1:keyint=25:min-keyint=25:scenecut=0 -c:a aac -ac 2 -b:a 64k $BX h264_opengop.ts
dts h264_opengop.ts h264_opengop_dts.txt
# B-pyramid with up to 3 B frames: decode delay larger than the other clips.
$F $(v 320x240) $(a 48000) -t 6 -c:v libx264 -profile:v main -g 25 -keyint_min 25 -sc_threshold 0 -x264-params b-pyramid=normal:bframes=3:keyint=25:min-keyint=25:scenecut=0 -c:a aac -ac 2 -b:a 64k $BX h264_pyramid.ts
dts h264_pyramid.ts h264_pyramid_dts.txt
N=$(( $(stat -c%s h264_opengop.ts) / 188 ))
tail -c +$(( 188 * (N * 3 / 8) + 1 )) h264_opengop.ts > h264_opengop_midgop.ts
# Low frame rates, 2 s GOPs: an x264 encoder's delay of about 30 frames is 2 s
# of stream time at 15 fps and 3 s at 10 fps.
vr() { echo "-f lavfi -i testsrc2=size=320x240:rate=$1"; }
$F $(vr 15) $(a 48000) -t 8 -c:v libx264 -profile:v main -bf 2 -g 30 -keyint_min 30 -sc_threshold 0 -c:a aac -ac 2 -b:a 64k $BX h264_aac_15fps.ts
$F $(vr 10) $(a 48000) -t 8 -c:v libx264 -profile:v main -bf 2 -g 20 -keyint_min 20 -sc_threshold 0 -c:a aac -ac 2 -b:a 64k $BX h264_aac_10fps.ts
# PMT-change fixtures: the appended 3 s clip continues the timeline
# (-output_ts_offset 6), so only the PMT signals the change.
$F $(v 320x240) $(a 48000) -t 3 $X264 -c:a aac -ac 2 -b:a 64k -output_ts_offset 6 -mpegts_start_pid 0x200 -mpegts_pmt_start_pid 0x1100 $BX tmp_pmtchange.ts
cat h264_aac.ts tmp_pmtchange.ts > h264_aac_pmtchange.ts
$F $(v 320x240) $(a 48000) -t 3 $X264 -c:a aac -ac 2 -b:a 64k -output_ts_offset 6 -streamid 1:0x120 $BX tmp_audiopid.ts
cat h264_aac.ts tmp_audiopid.ts > h264_aac_audiopid.ts
rm -f tmp_pmtchange.ts tmp_audiopid.ts
# Fragmented MP4: both traks in one init, a fragment per video keyframe.
$F $(v 320x240) $(a 48000) -t 6 $X264 -c:a aac -ac 2 -b:a 64k -threads 1 -fflags +bitexact -flags:v +bitexact -flags:a +bitexact -f mp4 -movflags frag_keyframe+empty_moov+default_base_moof h264_aac_frag.mp4
# FLV: remuxed from the TS fixtures (ffmpeg 8.x muxes HEVC as enhanced RTMP).
FLV="-c copy -threads 1 -fflags +bitexact -flags:v +bitexact -flags:a +bitexact -f flv"
$F -i h264_aac.ts $FLV h264_aac.flv
$F -i h265_aac51.ts $FLV h265_aac.flv
