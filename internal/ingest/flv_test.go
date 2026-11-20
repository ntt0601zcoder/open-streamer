package ingest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
)

// flvClock is a fake arrival clock: each push is stamped with the time set
// before it.
type flvClock struct{ at time.Time }

func (c *flvClock) now() time.Time { return c.at }

var flvBase = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

// tsFrames demuxes a TS fixture.
func tsFrames(t *testing.T, name string) []media.Frame {
	t.Helper()
	data, err := os.ReadFile(fixturePath(name))
	require.NoError(t, err)
	d, err := ts.NewDemuxer(bytes.NewReader(data))
	require.NoError(t, err)
	var frames []media.Frame
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			return frames
		}
		require.NoError(t, err)
		frames = append(frames, f)
	}
}

// videoConfig returns the config of the first video frame of a TS fixture.
func videoConfig(t *testing.T, name string) *media.TrackConfig {
	t.Helper()
	for _, f := range tsFrames(t, name) {
		if f.Kind() == media.KindVideo {
			return f.Config
		}
	}
	t.Fatalf("%s has no video", name)
	return nil
}

func avcRecord(t *testing.T, sps, pps [][]byte) []byte {
	t.Helper()
	rec, err := avc.CreateAVCDecConfRec(sps, pps, true)
	require.NoError(t, err)
	var b bytes.Buffer
	require.NoError(t, rec.Encode(&b))
	return b.Bytes()
}

func lengthPrefixed(nalus ...[]byte) []byte {
	var out []byte
	for _, n := range nalus {
		out = binary.BigEndian.AppendUint32(out, uint32(len(n)))
		out = append(out, n...)
	}
	return out
}

func videoMedia(ms uint32, tag rtmp.VideoTag) rtmp.Media {
	b, err := rtmp.AppendVideoTag(nil, tag, false)
	if err != nil {
		panic(err)
	}
	return rtmp.Media{Type: rtmp.MsgVideo, Timestamp: ms, Payload: b}
}

func avcHeader(t *testing.T, ms uint32, cfg *media.TrackConfig) rtmp.Media {
	t.Helper()
	return videoMedia(ms, rtmp.VideoTag{Codec: rtmp.VideoAVC, Kind: rtmp.VideoSequenceStart, Key: true, Data: avcRecord(t, cfg.SPS, cfg.PPS)})
}

func avcFrame(ms uint32, key bool, cts int32, nalus ...[]byte) rtmp.Media {
	return videoMedia(ms, rtmp.VideoTag{Codec: rtmp.VideoAVC, Kind: rtmp.VideoCodedFrames, Key: key, CompTime: cts, Data: lengthPrefixed(nalus...)})
}

func audioMedia(ms uint32, header bool, data []byte) rtmp.Media {
	return rtmp.Media{Type: rtmp.MsgAudio, Timestamp: ms, Payload: rtmp.AppendAudioTag(nil, rtmp.AudioTag{SequenceHeader: header, Data: data})}
}

func metaMedia(t *testing.T, vals ...any) rtmp.Media {
	t.Helper()
	b, err := rtmp.EncodeAMF0(vals...)
	require.NoError(t, err)
	return rtmp.Media{Type: rtmp.MsgDataAMF0, Payload: b}
}

// Synthetic H.264 NAL units: an IDR slice, and P and B slices whose headers
// read first_mb_in_slice 0 and slice_type 0 (P) or 1 (B).
var (
	nalIDR  = []byte{0x65, 0x88, 0x80, 0x10}
	nalP    = []byte{0x41, 0xC0, 0x20}
	nalB    = []byte{0x01, 0xA0, 0x30}
	testAUD = []byte{0x09, 0xF0}
	// AAC-LC, 48 kHz, stereo.
	asc48k = []byte{0x11, 0x90}
)

// pushAll pushes every message with its arrival at flvBase plus the message
// time, then flushes; it fails on any push error.
func pushAll(t *testing.T, f *flvFrames, clk *flvClock, msgs []rtmp.Media) []Frame {
	t.Helper()
	var out []Frame
	for i, m := range msgs {
		clk.at = flvBase.Add(time.Duration(m.Timestamp) * time.Millisecond)
		got, err := f.push(m)
		require.NoError(t, err, "message %d", i)
		out = append(out, got...)
	}
	return append(out, f.flush()...)
}

// clockFrames runs frames through a Clock with their arrivals, checks the
// frame rules and returns the Clock's stats.
func clockFrames(t *testing.T, kinds []media.Kind, frames []Frame) clock.Stats {
	t.Helper()
	c := clock.New(clock.Options{Kinds: kinds})
	out := make([]media.Frame, 0, len(frames))
	for _, f := range frames {
		out = append(out, c.PushAt(f.Frame, f.Arrival)...)
	}
	out = append(out, c.Flush()...)
	require.NotEmpty(t, out)
	assert.Nil(t, conform.CheckFrames(out, conform.FrameOptions{}))
	return c.Stats()
}

func byTrack(frames []Frame, track int) []Frame {
	var out []Frame
	for _, f := range frames {
		if f.Track == track {
			out = append(out, f)
		}
	}
	return out
}

func msOf(ts int64) uint32 {
	ms := ts / 90
	if ts%90 < 0 {
		ms--
	}
	return uint32(ms)
}

func floorMS(ts int64) int64 {
	ms := ts / 90
	if ts%90 < 0 {
		ms--
	}
	return ms * 90
}

var avKinds = []media.Kind{media.KindVideo, media.KindAudio}

func TestFLVFramesFromTS(t *testing.T) {
	src := tsFrames(t, "h264_aac.ts")
	var (
		msgs       []rtmp.Media
		vcfg, acfg *media.TrackConfig
		srcV, srcA []media.Frame
	)
	for _, f := range src {
		switch f.Kind() {
		case media.KindVideo:
			if f.Config != vcfg {
				vcfg = f.Config
				msgs = append(msgs, avcHeader(t, msOf(f.DTS), vcfg))
			}
			msgs = append(msgs, avcFrame(msOf(f.DTS), f.Key, int32((f.PTS-f.DTS)/90), f.Data...))
			srcV = append(srcV, f)
		case media.KindAudio:
			if f.Config != acfg {
				acfg = f.Config
				msgs = append(msgs, audioMedia(msOf(f.PTS), true, acfg.ASC))
			}
			msgs = append(msgs, audioMedia(msOf(f.PTS), false, f.Data[0]))
			srcA = append(srcA, f)
		}
	}
	require.NotEmpty(t, srcV)
	require.NotEmpty(t, srcA)

	clk := &flvClock{}
	f := newFLVFrames(&media.ConfigCache{}, clk.now)
	out := pushAll(t, f, clk, msgs)

	gotV, gotA := byTrack(out, 0), byTrack(out, 1)
	require.Len(t, gotV, len(srcV), "video frames before any Clock")
	require.Len(t, gotA, len(srcA), "audio frames before any Clock")
	// The fixture starts slightly below zero, so the RTMP stamps start just
	// below 2^32 ms; the unwrapped timeline keeps that lap.
	lap := gotV[0].DTS - floorMS(srcV[0].DTS)
	require.Zero(t, lap%(1<<32*90))
	for i, g := range gotV {
		s := srcV[i]
		require.Equal(t, s.Data, g.Data, "video %d", i)
		require.Equal(t, s.Key, g.Key, "video %d", i)
		require.Equal(t, floorMS(s.DTS)+lap, g.DTS, "video %d", i)
		require.Equal(t, floorMS(s.DTS)+(s.PTS-s.DTS)/90*90+lap, g.PTS, "video %d", i)
		require.Equal(t, media.CodecH264, g.Config.Codec)
	}
	for i, g := range gotA {
		require.Equal(t, srcA[i].Data, g.Data, "audio %d", i)
		require.Equal(t, g.PTS, g.DTS)
		require.True(t, g.Key)
		require.InDelta(t, srcA[i].PTS+lap, g.PTS, 91, "audio %d", i)
	}
	st := f.stats()
	assert.Equal(t, uint64(len(out)), st["frames"])
	assert.Zero(t, st["bad_tags"])
	assert.Zero(t, st["dts_shifts"])

	cs := clockFrames(t, avKinds, out)
	assert.Zero(t, cs.DroppedInvalid)
}

func flvFixture(t *testing.T, name string) []rtmp.Media {
	t.Helper()
	raw, err := os.ReadFile(fixturePath(name))
	require.NoError(t, err)
	tags, err := rtmp.ReadFLV(bytes.NewReader(raw))
	require.NoError(t, err)
	msgs := make([]rtmp.Media, 0, len(tags))
	for _, tg := range tags {
		msgs = append(msgs, rtmp.Media{Type: tg.Type, Timestamp: tg.Timestamp, Payload: tg.Data})
	}
	return msgs
}

func h265IRAP(nalus [][]byte) bool {
	for _, n := range nalus {
		if typ := (n[0] >> 1) & 0x3f; typ >= 16 && typ <= 21 {
			return true
		}
	}
	return false
}

func TestFLVFramesFixtureHEVC(t *testing.T) {
	clk := &flvClock{}
	f := newFLVFrames(&media.ConfigCache{}, clk.now)
	out := pushAll(t, f, clk, flvFixture(t, "h265_aac.flv"))

	v, a := byTrack(out, 0), byTrack(out, 1)
	assert.Len(t, v, 150)
	assert.Len(t, a, 260)
	keys := 0
	for i, fr := range v {
		require.Equal(t, media.CodecH265, fr.Config.Codec)
		require.NotEmpty(t, fr.Config.VPS)
		require.NotEmpty(t, fr.Config.SPS)
		require.NotEmpty(t, fr.Config.PPS)
		require.Equal(t, h265IRAP(fr.Data), fr.Key, "video %d", i)
		require.GreaterOrEqual(t, fr.PTS, fr.DTS)
		if fr.Key {
			keys++
		}
	}
	assert.Positive(t, keys)
	assert.Equal(t, 6, a[0].Config.Channels)
	st := f.stats()
	assert.Equal(t, uint64(2), st["unsupported_tags"], "the two multichannel-config tags")
	assert.Zero(t, st["bad_tags"])
	vOK, aOK := f.announced()
	assert.True(t, vOK)
	assert.True(t, aOK)

	cs := clockFrames(t, avKinds, out)
	assert.Zero(t, cs.DroppedInvalid)
}

func TestFLVFramesFixtureH264(t *testing.T) {
	clk := &flvClock{}
	f := newFLVFrames(&media.ConfigCache{}, clk.now)
	out := pushAll(t, f, clk, flvFixture(t, "h264_aac.flv"))
	assert.Len(t, byTrack(out, 0), 150)
	assert.Len(t, byTrack(out, 1), 283, "the empty AAC sequence header is ignored")
	st := f.stats()
	assert.Zero(t, st["bad_tags"])
	assert.Zero(t, st["unsupported_tags"])
	assert.Zero(t, st["dropped_no_config"])
	clockFrames(t, avKinds, out)
}

// ipbbSession returns n video frames in decode order I P B B … (a key every
// gop frames, gop = 1 + 3m) at 33 ms steps from startMS, with I/P/B
// composition offsets 0/+66/−33 ms, plus AAC frames at 48 kHz over the same
// span, interleaved by time; ctsOf gives each video frame's offset.
func ipbbSession(t *testing.T, cfg *media.TrackConfig, startMS uint32, n, gop int) (msgs []rtmp.Media, ctsOf []int32) {
	t.Helper()
	msgs = append(msgs, avcHeader(t, startMS, cfg), audioMedia(startMS, true, asc48k))
	audioK := int64(0)
	for i := range n {
		dts := int64(i) * 33
		for ; audioK*1024*1000/48000 <= dts; audioK++ {
			msgs = append(msgs, audioMedia(startMS+uint32(audioK*1024*1000/48000), false, []byte{0x21, byte(audioK)}))
		}
		var m rtmp.Media
		var cts int32
		switch j := i % gop; {
		case j == 0:
			m = avcFrame(startMS+uint32(dts), true, 0, nalIDR)
		case (j-1)%3 == 0:
			cts = 66
			m = avcFrame(startMS+uint32(dts), false, cts, nalP)
		default:
			cts = -33
			m = avcFrame(startMS+uint32(dts), false, cts, nalB)
		}
		msgs = append(msgs, m)
		ctsOf = append(ctsOf, cts)
	}
	return msgs, ctsOf
}

func TestFLVFramesTiming(t *testing.T) {
	vcfg := videoConfig(t, "h264_aac.ts")
	cache := &media.ConfigCache{}
	// The first session starts 4096 ms before the 32-bit wrap; the second,
	// after a reconnect, starts again from 0 on a fresh flvFrames.
	for _, start := range []uint32{0xFFFFF000, 0} {
		const n = 300
		msgs, ctsOf := ipbbSession(t, vcfg, start, n, 13)
		clk := &flvClock{}
		f := newFLVFrames(cache, clk.now)
		out := pushAll(t, f, clk, msgs)

		v := byTrack(out, 0)
		require.Len(t, v, n, "start %#x: no video frame dropped", start)
		for i, fr := range v {
			wantPTS := (int64(start) + int64(i)*33 + int64(ctsOf[i])) * 90
			require.Equal(t, wantPTS, fr.PTS, "start %#x frame %d: PTS exact", start, i)
			require.Equal(t, (int64(start)+int64(i)*33-33)*90, fr.DTS, "start %#x frame %d: shifted by 33 ms", start, i)
			require.GreaterOrEqual(t, fr.PTS, fr.DTS)
		}
		for _, track := range []int{0, 1} {
			fr := byTrack(out, track)
			require.NotEmpty(t, fr)
			for i := 1; i < len(fr); i++ {
				require.Greater(t, fr[i].DTS, fr[i-1].DTS, "start %#x track %d frame %d", start, track, i)
			}
		}
		st := f.stats()
		assert.Equal(t, uint64(1), st["dts_shifts"])
		assert.Zero(t, st["dts_shift_late"])
		assert.Zero(t, st["audio_reanchors"])
		assert.Zero(t, st["audio_dropped_ahead"])

		cs := clockFrames(t, avKinds, out)
		assert.Zero(t, cs.DroppedInvalid, "start %#x", start)
	}
}

func TestFLVFramesHold(t *testing.T) {
	vcfg := videoConfig(t, "h264_aac.ts")
	cases := []struct {
		name    string
		gop     int
		release int // index of the frame whose push releases the hold
	}{
		{"second keyframe", 13, 13},
		{"one second", 61, 31}, // 31 × 33 ms = 1023 ms
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clk := &flvClock{}
			f := newFLVFrames(&media.ConfigCache{}, clk.now)
			_, err := f.push(avcHeader(t, 0, vcfg))
			require.NoError(t, err)
			msgs, _ := ipbbSession(t, vcfg, 0, c.release+5, c.gop)
			i := 0
			for _, m := range msgs {
				if m.Type != rtmp.MsgVideo || bytes.Equal(m.Payload[:2], []byte{0x17, 0x00}) {
					continue // headers and audio
				}
				clk.at = flvBase.Add(time.Duration(i) * time.Second)
				got, err := f.push(m)
				require.NoError(t, err)
				switch {
				case i < c.release:
					require.Empty(t, got, "frame %d is held", i)
				case i == c.release:
					require.Len(t, got, c.release+1)
					for k, fr := range got {
						// R-PF3: held frames keep their receipt time.
						require.Equal(t, flvBase.Add(time.Duration(k)*time.Second), fr.Arrival)
					}
				default:
					require.Len(t, got, 1)
				}
				i++
			}
		})
	}
}

func TestFLVFramesFlush(t *testing.T) {
	vcfg := videoConfig(t, "h264_aac.ts")
	clk := &flvClock{}
	f := newFLVFrames(&media.ConfigCache{}, clk.now)
	_, err := f.push(avcHeader(t, 0, vcfg))
	require.NoError(t, err)
	for i, m := range []rtmp.Media{
		avcFrame(0, true, 0, nalIDR),
		avcFrame(33, false, 66, nalP),
		avcFrame(66, false, -33, nalB),
	} {
		clk.at = flvBase.Add(time.Duration(i) * time.Second)
		got, err := f.push(m)
		require.NoError(t, err)
		require.Empty(t, got)
	}
	got := f.flush()
	require.Len(t, got, 3)
	for i, fr := range got {
		assert.Equal(t, flvBase.Add(time.Duration(i)*time.Second), fr.Arrival)
		assert.Equal(t, int64(i*33-33)*90, fr.DTS, "the shift is learned from the flushed frames")
	}
	assert.Empty(t, f.flush())
	assert.Equal(t, uint64(3), f.stats()["frames"])
}

func TestFLVFramesHoldBound(t *testing.T) {
	vcfg := videoConfig(t, "h264_aac.ts")
	// No second keyframe within 1 s: the hold is bounded by frames (with
	// frozen timestamps) and by bytes.
	big := append([]byte{0x41, 0xC0}, make([]byte, maxHeldBytes/4)...)
	cases := []struct {
		name string
		n    int
		nal  []byte
		ms   func(i int) uint32
	}{
		{"frames", maxHeldVideo, nalP, func(int) uint32 { return 0 }},
		{"bytes", 4, big, func(i int) uint32 { return uint32(i * 40) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clk := &flvClock{}
			f := newFLVFrames(&media.ConfigCache{}, clk.now)
			_, err := f.push(avcHeader(t, 0, vcfg))
			require.NoError(t, err)
			for i := range c.n {
				got, err := f.push(avcFrame(c.ms(i), i == 0, 0, c.nal))
				require.NoError(t, err)
				if i < c.n-1 {
					require.Empty(t, got, "frame %d is held", i)
				} else {
					require.Len(t, got, c.n, "the last frame releases the hold")
				}
			}
		})
	}
}

func TestFLVFramesLateShift(t *testing.T) {
	vcfg := videoConfig(t, "h264_aac.ts")
	// No negative offset in the hold (keys at 0 and 400 ms, 40 ms steps),
	// then B frames at negative offsets: each late shift costs its frame.
	// A shift beyond one step also raises the frames after it that would
	// go backward; two increases in a row add up.
	cases := []struct {
		name   string
		neg    map[int]int32 // source frame → composition offset (ms)
		raised int           // frames after the lost ones whose DTS is raised
	}{
		{"one step", map[int]int32{15: -40}, 0},
		{"two and a half steps", map[int]int32{15: -100}, 1},
		{"two increases", map[int]int32{15: -100, 16: -150}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clk := &flvClock{}
			f := newFLVFrames(&media.ConfigCache{}, clk.now)
			msgs := []rtmp.Media{avcHeader(t, 0, vcfg)}
			shift := int64(0)
			for i := range 20 {
				ms := uint32(i * 40)
				cts, neg := c.neg[i]
				switch {
				case i%10 == 0:
					msgs = append(msgs, avcFrame(ms, true, 0, nalIDR))
				case neg:
					msgs = append(msgs, avcFrame(ms, false, cts, nalB))
					shift = max(shift, -int64(cts))
				default:
					msgs = append(msgs, avcFrame(ms, false, 0, nalP))
				}
			}
			out := pushAll(t, f, clk, msgs)
			lost := len(c.neg)
			require.Len(t, out, 20-lost)
			for i, fr := range out {
				require.GreaterOrEqual(t, fr.PTS, fr.DTS, "frame %d", i)
				if i > 0 {
					require.Greater(t, fr.DTS, out[i-1].DTS, "frame %d", i)
				}
			}
			// out[15:] are the source frames after the lost ones.
			for k := 15; k < len(out); k++ {
				src := int64(k+lost) * 40 * 90
				require.Equal(t, src, out[k].PTS, "frame %d: PTS exact", k)
				if k < 15+c.raised {
					require.Equal(t, out[14].DTS+int64(k-14), out[k].DTS, "frame %d raised", k)
				} else {
					require.Equal(t, src-shift*90, out[k].DTS, "frame %d shifted", k)
				}
			}
			st := f.stats()
			assert.Equal(t, uint64(lost), st["dts_shifts"])
			assert.Equal(t, uint64(lost), st["dts_shift_late"])
		})
	}
}

func TestFLVFramesBadConfigRecovery(t *testing.T) {
	cfg := videoConfig(t, "h264_aac.ts")
	badSPS := []byte{0x67} // too short for an ID
	badHeader := videoMedia(0, rtmp.VideoTag{Codec: rtmp.VideoAVC, Kind: rtmp.VideoSequenceStart, Data: []byte{1, 2}})
	cases := []struct {
		name    string
		bad     func(ms uint32) rtmp.Media // invalidates the config
		recover func(ms uint32) []rtmp.Media
	}{
		{"bad in-band set, then good in-band sets", func(ms uint32) rtmp.Media {
			return avcFrame(ms, false, 0, badSPS, nalP)
		}, func(ms uint32) []rtmp.Media {
			return []rtmp.Media{avcFrame(ms, true, 0, cfg.SPS[0], cfg.PPS[0], nalIDR)}
		}},
		{"bad header, then good header", func(ms uint32) rtmp.Media {
			m := badHeader
			m.Timestamp = ms
			return m
		}, func(ms uint32) []rtmp.Media {
			return []rtmp.Media{avcHeader(t, ms, cfg), avcFrame(ms, true, 0, nalIDR)}
		}},
		{"bad header, then good in-band sets", func(ms uint32) rtmp.Media {
			m := badHeader
			m.Timestamp = ms
			return m
		}, func(ms uint32) []rtmp.Media {
			return []rtmp.Media{avcFrame(ms, true, 0, cfg.SPS[0], cfg.PPS[0], nalIDR)}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clk := &flvClock{}
			f := newFLVFrames(&media.ConfigCache{}, clk.now)
			var out []Frame
			push := func(m rtmp.Media, wantErr bool) {
				t.Helper()
				clk.at = flvBase.Add(time.Duration(m.Timestamp) * time.Millisecond)
				got, err := f.push(m)
				if wantErr {
					require.ErrorContains(t, err, "ingest: flv:")
				} else {
					require.NoError(t, err)
				}
				out = append(out, got...)
			}
			push(avcHeader(t, 0, cfg), false)
			push(avcFrame(0, true, 0, nalIDR), false)
			push(avcFrame(40, false, 0, nalP), false)
			push(c.bad(80), true)
			// The gap: frames are dropped while the config is invalid.
			push(avcFrame(120, false, 0, nalP), false)
			push(avcFrame(160, false, 0, nalP), false)
			for _, m := range c.recover(200) {
				push(m, false)
			}
			push(avcFrame(240, false, 0, nalP), false)
			out = append(out, f.flush()...)

			require.Len(t, out, 4)
			wantPTS := []int64{0, 40 * 90, 200 * 90, 240 * 90}
			for i, fr := range out {
				assert.Equal(t, wantPTS[i], fr.PTS, "frame %d", i)
				assert.Same(t, out[0].Config, fr.Config, "frame %d: the rebuilt config keeps the pointer", i)
			}
			assert.True(t, out[2].Key)
			assert.Equal(t, [][]byte{nalIDR}, out[2].Data)
			st := f.stats()
			assert.Equal(t, uint64(2), st["dropped_no_config"])
			assert.Equal(t, uint64(1), st["bad_tags"])
		})
	}
}

func TestFLVFramesStableConfig(t *testing.T) {
	cfg320 := videoConfig(t, "h264_aac.ts")
	cfg640 := videoConfig(t, "h264_640.ts")
	cache := &media.ConfigCache{}
	clk := &flvClock{}

	f1 := newFLVFrames(cache, clk.now)
	out1 := pushAll(t, f1, clk, []rtmp.Media{
		avcHeader(t, 0, cfg320), audioMedia(0, true, asc48k),
		avcFrame(0, true, 0, nalIDR), audioMedia(0, false, []byte{1}),
		avcHeader(t, 40, cfg320), audioMedia(40, true, asc48k),
		avcFrame(40, true, 0, nalIDR), audioMedia(42, false, []byte{2}),
	})
	require.Len(t, out1, 4)
	v1, a1 := byTrack(out1, 0), byTrack(out1, 1)
	assert.Same(t, v1[0].Config, v1[1].Config, "a repeated identical header keeps the pointer")
	assert.Same(t, a1[0].Config, a1[1].Config)

	f2 := newFLVFrames(cache, clk.now)
	out2 := pushAll(t, f2, clk, []rtmp.Media{
		avcHeader(t, 0, cfg320), audioMedia(0, true, asc48k),
		avcFrame(0, true, 0, nalIDR), audioMedia(0, false, []byte{1}),
		avcHeader(t, 40, cfg640), audioMedia(40, true, []byte{0x12, 0x10}),
		avcFrame(40, true, 0, nalIDR), audioMedia(42, false, []byte{2}),
	})
	require.Len(t, out2, 4)
	v2, a2 := byTrack(out2, 0), byTrack(out2, 1)
	assert.Same(t, v1[0].Config, v2[0].Config, "a reconnect sharing the cache keeps the pointer")
	assert.Same(t, a1[0].Config, a2[0].Config)
	assert.NotSame(t, v2[0].Config, v2[1].Config)
	assert.Equal(t, 640, v2[1].Config.Width)
	assert.NotSame(t, a2[0].Config, a2[1].Config)
	assert.Equal(t, 44100, a2[1].Config.SampleRate)
}

func TestFLVFramesVideoData(t *testing.T) {
	cfg := videoConfig(t, "h264_aac.ts")
	pps1 := []byte{0x68, 0x5F, 0x80} // pps_id 1, sps_id 0
	clk := &flvClock{}
	f := newFLVFrames(&media.ConfigCache{}, clk.now)
	out := pushAll(t, f, clk, []rtmp.Media{
		avcHeader(t, 0, cfg),
		// In-band copies of the record's sets and an AUD are removed.
		avcFrame(0, true, 0, testAUD, cfg.SPS[0], cfg.PPS[0], nalIDR),
		// A new in-band PPS joins the config.
		avcFrame(40, false, 0, pps1, nalP),
		avcFrame(80, true, 0, nalIDR),
	})
	require.Len(t, out, 3)
	assert.Equal(t, [][]byte{nalIDR}, out[0].Data)
	assert.Equal(t, [][]byte{nalP}, out[1].Data)
	assert.True(t, out[0].Key)
	assert.False(t, out[1].Key, "Key comes from the NAL units, not the FLV frame type")
	assert.Equal(t, cfg.PPS, out[0].Config.PPS)
	assert.Equal(t, [][]byte{cfg.PPS[0], pps1}, out[1].Config.PPS)
	assert.Same(t, out[1].Config, out[2].Config)
}

func TestFLVFramesLengthSize(t *testing.T) {
	cfg := videoConfig(t, "h264_aac.ts")
	rec := avcRecord(t, cfg.SPS, cfg.PPS)
	rec[4] = 0xFC | 1 // lengthSizeMinusOne 1: two-byte NAL lengths
	clk := &flvClock{}
	f := newFLVFrames(&media.ConfigCache{}, clk.now)
	data := append([]byte{0, byte(len(nalIDR))}, nalIDR...)
	out := pushAll(t, f, clk, []rtmp.Media{
		videoMedia(0, rtmp.VideoTag{Codec: rtmp.VideoAVC, Kind: rtmp.VideoSequenceStart, Key: true, Data: rec}),
		videoMedia(0, rtmp.VideoTag{Codec: rtmp.VideoAVC, Kind: rtmp.VideoCodedFrames, Key: true, Data: data}),
	})
	require.Len(t, out, 1)
	assert.Equal(t, [][]byte{nalIDR}, out[0].Data)
}

func TestFLVFramesAnnounced(t *testing.T) {
	cfg := videoConfig(t, "h264_aac.ts")
	meta := func(props ...rtmp.Property) []any { return []any{"onMetaData", rtmp.ECMAArray(props)} }
	cases := []struct {
		name         string
		msgs         func() []rtmp.Media
		video, audio bool
	}{
		{"metadata first", func() []rtmp.Media {
			return []rtmp.Media{metaMedia(t, meta(rtmp.Property{Key: "videocodecid", Value: 7.0}, rtmp.Property{Key: "audiocodecid", Value: 10.0})...)}
		}, true, true},
		{"metadata hevc and mp3", func() []rtmp.Media {
			return []rtmp.Media{metaMedia(t, meta(rtmp.Property{Key: "videocodecid", Value: 12.0}, rtmp.Property{Key: "audiocodecid", Value: 2.0})...)}
		}, true, false},
		{"fourcc string", func() []rtmp.Media {
			return []rtmp.Media{metaMedia(t, meta(rtmp.Property{Key: "videocodecid", Value: "hvc1"})...)}
		}, true, false},
		{"fourcc number", func() []rtmp.Media {
			return []rtmp.Media{metaMedia(t, meta(rtmp.Property{Key: "videocodecid", Value: float64(0x61766331)})...)}
		}, true, false},
		{"unsupported video", func() []rtmp.Media {
			return []rtmp.Media{metaMedia(t, meta(rtmp.Property{Key: "videocodecid", Value: "av01"}, rtmp.Property{Key: "audiocodecid", Value: 10.0})...)}
		}, false, true},
		{"object and setDataFrame", func() []rtmp.Media {
			return []rtmp.Media{metaMedia(t, "@setDataFrame", "onMetaData", rtmp.Object{{Key: "videocodecid", Value: 7.0}})}
		}, true, false},
		{"other data message", func() []rtmp.Media {
			return []rtmp.Media{metaMedia(t, "onTextData", rtmp.Object{{Key: "videocodecid", Value: 7.0}})}
		}, false, false},
		{"video header only", func() []rtmp.Media { return []rtmp.Media{avcHeader(t, 0, cfg)} }, true, false},
		{"empty audio header", func() []rtmp.Media { return []rtmp.Media{audioMedia(0, true, nil)} }, false, false},
		{"audio header only", func() []rtmp.Media { return []rtmp.Media{audioMedia(0, true, asc48k)} }, false, true},
		{"both headers", func() []rtmp.Media {
			return []rtmp.Media{avcHeader(t, 0, cfg), audioMedia(0, true, asc48k)}
		}, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFLVFrames(&media.ConfigCache{}, time.Now)
			v, a := f.announced()
			require.False(t, v)
			require.False(t, a)
			for _, m := range c.msgs() {
				_, err := f.push(m)
				require.NoError(t, err)
			}
			v, a = f.announced()
			assert.Equal(t, c.video, v, "video")
			assert.Equal(t, c.audio, a, "audio")
		})
	}
}

func TestFLVFramesCounters(t *testing.T) {
	cfg := videoConfig(t, "h264_aac.ts")
	f := newFLVFrames(&media.ConfigCache{}, time.Now)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				_ = f.stats()
				_, _ = f.announced()
			}
		}
	})

	push := func(m rtmp.Media) ([]Frame, error) { return f.push(m) }
	// Before any sequence header.
	_, err := push(avcFrame(0, true, 0, nalIDR))
	require.NoError(t, err)
	_, err = push(audioMedia(0, false, []byte{1}))
	require.NoError(t, err)
	// Unsupported: MP3 audio, Sorenson video.
	_, err = push(rtmp.Media{Type: rtmp.MsgAudio, Payload: []byte{0x2F, 0xFF}})
	require.NoError(t, err)
	_, err = push(rtmp.Media{Type: rtmp.MsgVideo, Payload: []byte{0x22, 0, 0, 0, 0}})
	require.NoError(t, err)
	// Sequence end.
	_, err = push(videoMedia(0, rtmp.VideoTag{Codec: rtmp.VideoAVC, Kind: rtmp.VideoSequenceEnd}))
	require.NoError(t, err)
	// Bad: a truncated NAL length, a truncated tag, an undecodable record.
	_, err = push(avcHeader(t, 0, cfg))
	require.NoError(t, err)
	_, err = push(videoMedia(0, rtmp.VideoTag{Codec: rtmp.VideoAVC, Kind: rtmp.VideoCodedFrames, Key: true, Data: []byte{0, 0, 0, 9, 0x65}}))
	require.ErrorContains(t, err, "ingest: flv:")
	_, err = push(rtmp.Media{Type: rtmp.MsgVideo, Payload: []byte{0x17, 1}})
	require.Error(t, err)
	_, err = push(videoMedia(0, rtmp.VideoTag{Codec: rtmp.VideoAVC, Kind: rtmp.VideoSequenceStart, Data: []byte{1, 2}}))
	require.Error(t, err)
	// The bad record invalidated the config until the next valid header.
	got, err := push(avcFrame(0, true, 0, nalIDR))
	require.NoError(t, err)
	require.Empty(t, got)
	_, err = push(avcHeader(t, 40, cfg))
	require.NoError(t, err)
	got, err = push(avcFrame(40, true, 0, nalIDR))
	require.NoError(t, err)
	got = append(got, f.flush()...)
	require.Len(t, got, 1)
	assert.Equal(t, cfg.SPS, got[0].Config.SPS)

	close(stop)
	wg.Wait()
	assert.Equal(t, map[string]uint64{
		"frames":              1,
		"unsupported_tags":    2,
		"bad_tags":            3,
		"dropped_no_config":   3,
		"dts_shifts":          0,
		"dts_shift_late":      0,
		"audio_reanchors":     0,
		"audio_dropped_ahead": 0,
		"sequence_ends":       1,
	}, f.stats())
}
