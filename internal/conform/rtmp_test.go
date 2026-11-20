package conform

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/mux/flv"
	"github.com/ntt0601zcoder/open-streamer/internal/output/rtmpplay"
	"github.com/ntt0601zcoder/open-streamer/internal/rtmpsrv"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp/rtmptest"
)

const rtmpWait = 10 * time.Second

// streamClip returns a fixture's frames in stream time, one epoch.
func streamClip(t *testing.T, name string) []media.Frame {
	t.Helper()
	d, err := ts.NewDemuxer(bytes.NewReader(fixture(t, name)))
	require.NoError(t, err)
	c := clock.New(clock.Options{Kinds: d.TrackKinds()})
	var out []media.Frame
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		out = append(out, c.Push(f)...)
	}
	return append(out, c.Flush()...)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	return "127.0.0.1:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

type rtmpResult struct {
	c   Capture
	err error
}

func readRTMPAsync(u string, opt ReadOptions) <-chan rtmpResult {
	ch := make(chan rtmpResult, 1)
	go func() {
		c, err := ReadRTMP(context.Background(), u, opt)
		ch <- rtmpResult{c, err}
	}()
	return ch
}

func awaitRTMP(t *testing.T, ch <-chan rtmpResult) rtmpResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(3 * rtmpWait):
		t.Fatal("ReadRTMP did not finish")
		return rtmpResult{}
	}
}

// readPlayOutput plays frames through our RTMP play output (rtmpplay) and
// reads them back to the end of the stream: the player joins once the hub
// is live, and the hub closes after the last frame.
func readPlayOutput(t *testing.T, frames []media.Frame) Capture {
	t.Helper()
	h := hub.New(hub.Options{})
	t.Cleanup(h.Close)
	srv := rtmpplay.New(func(name string) (*hub.Hub, bool) { return h, name == "s" }, rtmpplay.Options{Queue: 1 << 16})
	reg := &rtmpsrv.Registry{}
	addr := freeAddr(t)
	unregister, err := reg.RegisterPlay(addr, "live", srv, rtmp.Config{}, rtmpsrv.Policy{})
	require.NoError(t, err)
	t.Cleanup(unregister)

	i := 0
	for ; i < len(frames) && !h.Live(); i++ {
		h.Write(frames[i])
	}
	require.True(t, h.Live())
	ch := readRTMPAsync("rtmp://"+addr+"/live/s", ReadOptions{})
	require.Eventually(t, func() bool { return srv.Stats().Players == 1 }, rtmpWait, time.Millisecond)
	for _, f := range frames[i:] {
		h.Write(f)
	}
	h.Close()
	r := awaitRTMP(t, ch)
	require.NoError(t, r.err)
	return r.c
}

func TestReadRTMPClean(t *testing.T) {
	for _, name := range []string{"h264_aac.ts", "h265_aac51.ts"} {
		t.Run(name, func(t *testing.T) {
			src := streamClip(t, name)
			c := readPlayOutput(t, src)
			assert.Empty(t, c.Violations)

			var srcVideo, gotVideo []media.Frame
			for _, f := range src {
				if f.Kind() == media.KindVideo {
					srcVideo = append(srcVideo, f)
				}
			}
			for _, f := range c.Frames {
				if f.Kind() == media.KindVideo {
					gotVideo = append(gotVideo, f)
				}
				assert.Equal(t, uint32(1), f.Epoch)
			}
			assert.Positive(t, countKind(c.Frames, media.KindAudio))
			require.Len(t, gotVideo, len(srcVideo), "the player starts at the first keyframe")
			b := srcVideo[0].DTS
			for i, f := range gotVideo {
				s := srcVideo[i]
				assert.True(t, s.Config.Equal(f.Config), "frame %d config", i)
				assert.Equal(t, s.Data, f.Data, "frame %d data: parameter sets moved out", i)
				assert.Equal(t, s.Key, f.Key, "frame %d key", i)
				assert.Equal(t, s.RASL, f.RASL, "frame %d RASL", i)
				assert.Equal(t, (s.DTS-b)/90*90, f.DTS, "frame %d DTS", i)
				assert.Equal(t, (s.PTS-b)/90*90, f.PTS, "frame %d PTS", i)
			}
		})
	}
}

// muxedTags returns the RTMP messages our muxer makes of a fixture, as FLV
// tags.
func muxedTags(t *testing.T, name string) []rtmp.FLVTag {
	t.Helper()
	m := flv.NewMuxer()
	var out []rtmp.FLVTag
	for _, f := range streamClip(t, name) {
		msgs, err := m.Frame(f)
		require.NoError(t, err)
		for _, msg := range msgs {
			out = append(out, rtmp.FLVTag{Type: msg.Type, Timestamp: msg.Timestamp, Data: msg.Payload})
		}
	}
	return out
}

// tagKind classifies a tag for the test edits.
type tagKind int

const (
	tagOther tagKind = iota
	tagMeta
	tagVideoHeader
	tagVideoKey
	tagVideoDelta
	tagAudioHeader
	tagAudio
)

func kindOf(t *testing.T, tag rtmp.FLVTag) tagKind {
	t.Helper()
	switch tag.Type {
	case rtmp.FLVScript:
		return tagMeta
	case rtmp.FLVVideo:
		v, err := rtmp.ParseVideoTag(tag.Data)
		require.NoError(t, err)
		switch {
		case v.Kind == rtmp.VideoSequenceStart:
			return tagVideoHeader
		case v.Key:
			return tagVideoKey
		default:
			return tagVideoDelta
		}
	case rtmp.FLVAudio:
		a, err := rtmp.ParseAudioTag(tag.Data)
		require.NoError(t, err)
		if a.SequenceHeader {
			return tagAudioHeader
		}
		return tagAudio
	}
	return tagOther
}

// nth returns the index of the n-th (from 1) tag of kind k.
func nth(t *testing.T, tags []rtmp.FLVTag, k tagKind, n int) int {
	t.Helper()
	for i, tag := range tags {
		if kindOf(t, tag) == k {
			if n--; n == 0 {
				return i
			}
		}
	}
	t.Fatalf("no tag %d of kind %d", n, k)
	return -1
}

// editVideo rewrites a video tag in the layout the muxer uses for its codec:
// legacy for H.264, enhanced for H.265.
func editVideo(t *testing.T, tag rtmp.FLVTag, edit func(*rtmp.VideoTag)) rtmp.FLVTag {
	t.Helper()
	v, err := rtmp.ParseVideoTag(tag.Data)
	require.NoError(t, err)
	edit(&v)
	return videoTag(t, tag, v, v.Codec == rtmp.VideoHEVC)
}

// videoTag returns tag with the body of v in the given layout.
func videoTag(t *testing.T, tag rtmp.FLVTag, v rtmp.VideoTag, enhanced bool) rtmp.FLVTag {
	t.Helper()
	var err error
	tag.Data, err = rtmp.AppendVideoTag(nil, v, enhanced)
	require.NoError(t, err)
	return tag
}

// nalus splits 4-byte length-prefixed data.
func nalus(t *testing.T, data []byte) [][]byte {
	t.Helper()
	out, err := splitNALUs(data, 4)
	require.NoError(t, err)
	return out
}

// joinNALUs length-prefixes NAL units with 4-byte lengths.
func joinNALUs(ns [][]byte) []byte {
	var out []byte
	for _, n := range ns {
		out = binary.BigEndian.AppendUint32(out, uint32(len(n)))
		out = append(out, n...)
	}
	return out
}

// dropNAL removes the NAL units that drop reports from 4-byte
// length-prefixed data.
func dropNAL(t *testing.T, data []byte, drop func(n []byte) bool) []byte {
	t.Helper()
	return joinNALUs(slices.DeleteFunc(nalus(t, data), drop))
}

func h264Type(typ byte) func([]byte) bool { return func(n []byte) bool { return n[0]&0x1f == typ } }

func h265Type(typ byte) func([]byte) bool {
	return func(n []byte) bool { return (n[0]>>1)&0x3f == typ }
}

// everyVideo rewrites every video header and coded frame in the given
// layout.
func everyVideo(t *testing.T, tags []rtmp.FLVTag, enhanced bool) []rtmp.FLVTag {
	t.Helper()
	for i, tag := range tags {
		if tag.Type != rtmp.FLVVideo {
			continue
		}
		v, err := rtmp.ParseVideoTag(tag.Data)
		require.NoError(t, err)
		tags[i] = videoTag(t, tag, v, enhanced)
	}
	return tags
}

// newEpoch starts a new epoch at the n-th video keyframe as the muxer does
// on a resync: onMetaData and both headers before it, and no audio after it
// presented before it.
func newEpoch(t *testing.T, tags []rtmp.FLVTag, n int) []rtmp.FLVTag {
	t.Helper()
	k := nth(t, tags, tagVideoKey, n)
	v, err := rtmp.ParseVideoTag(tags[k].Data)
	require.NoError(t, err)
	keyPTS := int64(tags[k].Timestamp) + int64(v.CompTime)
	out := slices.Clone(tags[:k])
	for _, i := range []int{nth(t, tags, tagMeta, 1), nth(t, tags, tagVideoHeader, 1), nth(t, tags, tagAudioHeader, 1)} {
		h := tags[i]
		h.Timestamp = tags[k].Timestamp
		out = append(out, h)
	}
	for _, tag := range tags[k:] {
		if kindOf(t, tag) != tagAudio || int64(tag.Timestamp) >= keyPTS {
			out = append(out, tag)
		}
	}
	return out
}

func readTags(t *testing.T, tags []rtmp.FLVTag) Capture {
	t.Helper()
	s := rtmptest.NewPlayServer(t, tags, rtmptest.PlayOptions{})
	r := awaitRTMP(t, readRTMPAsync(s.URL("live/s"), ReadOptions{}))
	require.NoError(t, r.err)
	return r.c
}

type rtmpEdit func(t *testing.T, tags []rtmp.FLVTag) []rtmp.FLVTag

func TestReadRTMPViolations(t *testing.T) {
	const h264, h265 = "h264_aac.ts", "h265_aac51.ts"
	bases := map[string][]rtmp.FLVTag{h264: muxedTags(t, h264), h265: muxedTags(t, h265)}
	other := muxedTags(t, "h264_640.ts")
	del := func(tags []rtmp.FLVTag, i int) []rtmp.FLVTag { return slices.Delete(tags, i, i+1) }
	same := func(_ *testing.T, tags []rtmp.FLVTag) []rtmp.FLVTag { return tags }
	for name, tc := range map[string]struct {
		clip string // "" means h264_aac.ts
		edit rtmpEdit
		want string // "" means clean
	}{
		"clean":      {"", same, ""},
		"h265 clean": {h265, same, ""},
		"timestamps wrap": {"", func(_ *testing.T, tags []rtmp.FLVTag) []rtmp.FLVTag {
			for i := range tags {
				tags[i].Timestamp += 1<<32 - 1000
			}
			return tags
		}, ""},
		"second epoch with headers": {"", func(t *testing.T, tags []rtmp.FLVTag) []rtmp.FLVTag {
			return newEpoch(t, tags, 3)
		}, ""},
		"SEI before the parameter sets": {"", func(t *testing.T, tags []rtmp.FLVTag) []rtmp.FLVTag {
			i := nth(t, tags, tagVideoKey, 2)
			tags[i] = editVideo(t, tags[i], func(v *rtmp.VideoTag) {
				v.Data = joinNALUs(append([][]byte{{0x06, 0x05, 0x01, 0x00, 0x80}}, nalus(t, v.Data)...))
			})
			return tags
		}, ""},
		"media before header": {"", func(t *testing.T, tags []rtmp.FLVTag) []rtmp.FLVTag {
			return del(tags, nth(t, tags, tagVideoHeader, 1))
		}, RuleRTMPHeader},
		"second epoch without headers": {"", func(t *testing.T, tags []rtmp.FLVTag) []rtmp.FLVTag {
			k := nth(t, tags, tagVideoKey, 3)
			meta := tags[nth(t, tags, tagMeta, 1)]
			meta.Timestamp = tags[k].Timestamp
			return slices.Insert(tags, k, meta)
		}, RuleRTMPHeader},
		"header resent within epoch": {"", func(t *testing.T, tags []rtmp.FLVTag) []rtmp.FLVTag {
			k := nth(t, tags, tagVideoKey, 3)
			hdr := tags[nth(t, tags, tagVideoHeader, 1)]
			hdr.Timestamp = tags[k].Timestamp
			return slices.Insert(tags, k, hdr)
		}, RuleRTMPHeader},
		"mid-epoch header changes the config": {"", func(t *testing.T, tags []rtmp.FLVTag) []rtmp.FLVTag {
			k := nth(t, tags, tagVideoKey, 3)
			hdr := other[nth(t, other, tagVideoHeader, 1)]
			hdr.Timestamp = tags[k].Timestamp
			return slices.Insert(tags, k, hdr)
		}, RuleConfigSignalled},
		"codec mismatch": {"", func(t *testing.T, tags []rtmp.FLVTag) []rtmp.FLVTag {
			i := nth(t, tags, tagVideoDelta, 5)
			tags[i] = editVideo(t, tags[i], func(v *rtmp.VideoTag) { v.Codec = rtmp.VideoHEVC })
			return tags
		}, RuleRTMPHeader},
		"no metadata": {"", func(t *testing.T, tags []rtmp.FLVTag) []rtmp.FLVTag {
			// Another data message first: the play server then sends no
			// onMetaData of its own.
			i := nth(t, tags, tagMeta, 1)
			b, err := rtmp.EncodeAMF0("onTextData", rtmp.ECMAArray{{Key: "text", Value: "x"}})
			require.NoError(t, err)
			tags[i].Data = b
			return tags
		}, RuleRTMPMetadata},
		"delta first": {"", func(t *testing.T, tags []rtmp.FLVTag) []rtmp.FLVTag {
			return del(tags, nth(t, tags, tagVideoKey, 1))
		}, RuleEpochKey},
		"backward timestamp": {"", func(t *testing.T, tags []rtmp.FLVTag) []rtmp.FLVTag {
			i := nth(t, tags, tagVideoDelta, 10)
			tags[i].Timestamp = tags[nth(t, tags, tagVideoDelta, 8)].Timestamp
			return tags
		}, RuleRTMPTimestamp},
		"missing in-band SPS": {"", func(t *testing.T, tags []rtmp.FLVTag) []rtmp.FLVTag {
			i := nth(t, tags, tagVideoKey, 2)
			tags[i] = editVideo(t, tags[i], func(v *rtmp.VideoTag) { v.Data = dropNAL(t, v.Data, h264Type(7)) })
			return tags
		}, RuleRTMPInband},
		"h265 missing in-band VPS": {h265, func(t *testing.T, tags []rtmp.FLVTag) []rtmp.FLVTag {
			i := nth(t, tags, tagVideoKey, 2)
			tags[i] = editVideo(t, tags[i], func(v *rtmp.VideoTag) { v.Data = dropNAL(t, v.Data, h265Type(32)) })
			return tags
		}, RuleRTMPInband},
		"in-band set not in the header": {"", func(t *testing.T, tags []rtmp.FLVTag) []rtmp.FLVTag {
			key, err := rtmp.ParseVideoTag(tags[nth(t, tags, tagVideoKey, 2)].Data)
			require.NoError(t, err)
			i := slices.IndexFunc(nalus(t, key.Data), h264Type(8))
			require.GreaterOrEqual(t, i, 0)
			pps := append(slices.Clone(nalus(t, key.Data)[i]), 0x80)
			d := nth(t, tags, tagVideoDelta, 5)
			tags[d] = editVideo(t, tags[d], func(v *rtmp.VideoTag) {
				v.Data = joinNALUs(append([][]byte{pps}, nalus(t, v.Data)...))
			})
			return tags
		}, RuleRTMPInband},
		"negative composition time": {"", func(t *testing.T, tags []rtmp.FLVTag) []rtmp.FLVTag {
			i := nth(t, tags, tagVideoDelta, 5)
			tags[i] = editVideo(t, tags[i], func(v *rtmp.VideoTag) { v.CompTime = -40 })
			return tags
		}, RuleRTMPCompTime},
		"bad NAL length": {"", func(t *testing.T, tags []rtmp.FLVTag) []rtmp.FLVTag {
			i := nth(t, tags, tagVideoDelta, 5)
			tags[i] = editVideo(t, tags[i], func(v *rtmp.VideoTag) { v.Data = append(v.Data, 0, 0, 1, 0) })
			return tags
		}, RuleRTMPNALU},
		"header declares 2-byte lengths": {"", func(t *testing.T, tags []rtmp.FLVTag) []rtmp.FLVTag {
			i := nth(t, tags, tagVideoHeader, 1)
			tags[i] = editVideo(t, tags[i], func(v *rtmp.VideoTag) {
				v.Data = slices.Clone(v.Data)
				v.Data[4] = v.Data[4]&^3 | 1
			})
			return tags
		}, RuleRTMPNALU},
		"bad tag": {"", func(t *testing.T, tags []rtmp.FLVTag) []rtmp.FLVTag {
			i := nth(t, tags, tagAudio, 5)
			tags[i].Data = []byte{0x2f, 1} // MP3
			return tags
		}, RuleRTMPTag},
		"enhanced H.264": {"", func(t *testing.T, tags []rtmp.FLVTag) []rtmp.FLVTag {
			return everyVideo(t, tags, true)
		}, RuleRTMPForm},
		"legacy H.265": {h265, func(t *testing.T, tags []rtmp.FLVTag) []rtmp.FLVTag {
			return everyVideo(t, tags, false)
		}, RuleRTMPForm},
		"header not key": {"", func(t *testing.T, tags []rtmp.FLVTag) []rtmp.FLVTag {
			i := nth(t, tags, tagVideoHeader, 1)
			tags[i] = editVideo(t, tags[i], func(v *rtmp.VideoTag) { v.Key = false })
			return tags
		}, RuleRTMPForm},
	} {
		t.Run(name, func(t *testing.T) {
			clip := tc.clip
			if clip == "" {
				clip = h264
			}
			tags := slices.Clone(bases[clip])
			c := readTags(t, tc.edit(t, tags))
			if tc.want == "" {
				assert.Empty(t, c.Violations)
				return
			}
			assert.Contains(t, rules(c.Violations), tc.want, "%v", c.Violations)
		})
	}
}

// H.265 coded frames with composition time 0 use CodedFramesX, which has
// no composition time field.
func TestReadRTMPCodedFramesX(t *testing.T) {
	tags := muxedTags(t, "h265_aac51.ts")
	x := 0
	for _, tag := range tags {
		if tag.Type == rtmp.FLVVideo && tag.Data[0]&0x80 != 0 && tag.Data[0]&0x0f == 3 {
			x++
		}
	}
	require.Positive(t, x, "the clip has CodedFramesX tags")
	c := readTags(t, tags)
	assert.Empty(t, c.Violations)
	assert.Equal(t, 150, countKind(c.Frames, media.KindVideo))
}

// A read whose frames are all rejected still ends after Duration.
func TestReadRTMPDurationWithoutFrames(t *testing.T) {
	tags := slices.DeleteFunc(muxedTags(t, "h264_aac.ts"), func(tag rtmp.FLVTag) bool {
		k := kindOf(t, tag)
		return k == tagVideoHeader || k == tagAudioHeader
	})
	s := rtmptest.NewPlayServer(t, tags, rtmptest.PlayOptions{Hold: true})
	start := time.Now()
	r := awaitRTMP(t, readRTMPAsync(s.URL("live/s"), ReadOptions{Duration: time.Second, Timeout: rtmpWait}))
	require.NoError(t, r.err)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Empty(t, r.c.Frames)
	assert.Contains(t, rules(r.c.Violations), RuleRTMPHeader)
}

// steadyServer serves every player onMetaData, the headers and then the
// same coded frame stamped 0 until the player leaves: a stream whose time
// never advances.
func steadyServer(t *testing.T, tags []rtmp.FLVTag) string {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		conns []net.Conn
	)
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	head := make([]rtmp.Media, 0, 3)
	for _, k := range []tagKind{tagMeta, tagVideoHeader, tagAudioHeader} {
		tag := tags[nth(t, tags, k, 1)]
		head = append(head, rtmp.Media{Type: tag.Type, Payload: tag.Data})
	}
	frame := tags[nth(t, tags, tagVideoKey, 1)]
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				sc, err := rtmp.Accept(context.Background(), c, rtmp.Config{}, nil)
				if err != nil {
					return
				}
				defer sc.Close()
				for _, m := range head {
					if sc.WriteMedia(m) != nil {
						return
					}
				}
				for {
					if sc.WriteMedia(rtmp.Media{Type: rtmp.MsgVideo, Payload: frame.Data}) != nil {
						return
					}
					select {
					case <-sc.Done():
						return
					case <-time.After(5 * time.Millisecond):
					}
				}
			}()
		}
	}()
	return "rtmp://" + ln.Addr().String() + "/live/s"
}

// A stream whose timestamps never advance ends cleanly after Duration plus
// Timeout of wall-clock time.
func TestReadRTMPBackstop(t *testing.T) {
	u := steadyServer(t, muxedTags(t, "h264_aac.ts"))
	start := time.Now()
	r := awaitRTMP(t, readRTMPAsync(u, ReadOptions{Duration: 500 * time.Millisecond, Timeout: time.Second}))
	require.NoError(t, r.err)
	assert.GreaterOrEqual(t, time.Since(start), 1400*time.Millisecond)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Positive(t, countKind(r.c.Frames, media.KindVideo))
}

// The second capture epoch opens at the second onMetaData.
func TestReadRTMPEpochs(t *testing.T) {
	c := readTags(t, newEpoch(t, muxedTags(t, "h264_aac.ts"), 3))
	require.Empty(t, c.Violations)
	epochs := map[uint32]int{}
	for _, f := range c.Frames {
		epochs[f.Epoch]++
	}
	assert.Len(t, epochs, 2)
	assert.Positive(t, epochs[1])
	assert.Positive(t, epochs[2])
}

// A stream with no end is read for Duration.
func TestReadRTMPDuration(t *testing.T) {
	s := rtmptest.NewPlayServer(t, muxedTags(t, "h264_aac.ts"), rtmptest.PlayOptions{Hold: true})
	r := awaitRTMP(t, readRTMPAsync(s.URL("live/s"), ReadOptions{Duration: 2 * time.Second, Timeout: rtmpWait}))
	require.NoError(t, r.err)
	assert.Empty(t, r.c.Violations)
	lo, hi := r.c.Frames[0].DTS, r.c.Frames[0].DTS
	for _, f := range r.c.Frames {
		lo, hi = min(lo, f.DTS), max(hi, f.DTS)
	}
	assert.GreaterOrEqual(t, hi-lo, int64(2*media.ClockRate))
	assert.Less(t, hi-lo, int64(3*media.ClockRate))
}

// A server that goes silent is a read error after Timeout, and so is one
// that never answers the handshake.
func TestReadRTMPStalls(t *testing.T) {
	s := rtmptest.NewPlayServer(t, muxedTags(t, "h264_aac.ts"), rtmptest.PlayOptions{Hold: true})
	start := time.Now()
	r := awaitRTMP(t, readRTMPAsync(s.URL("live/s"), ReadOptions{Timeout: 300 * time.Millisecond}))
	require.Error(t, r.err)
	assert.Contains(t, r.err.Error(), "stalled")
	assert.Less(t, time.Since(start), 5*time.Second)

	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	start = time.Now()
	r = awaitRTMP(t, readRTMPAsync("rtmp://"+ln.Addr().String()+"/live/s", ReadOptions{Timeout: 300 * time.Millisecond}))
	require.Error(t, r.err)
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestReadRTMPCancel(t *testing.T) {
	s := rtmptest.NewPlayServer(t, muxedTags(t, "h264_aac.ts"), rtmptest.PlayOptions{Hold: true})
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() {
		_, err := ReadRTMP(ctx, s.URL("live/s"), ReadOptions{})
		ch <- err
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-ch:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(rtmpWait):
		t.Fatal("ReadRTMP did not stop on cancel")
	}
}

func TestReadRTMPNotFound(t *testing.T) {
	s := rtmptest.NewPlayServer(t, nil, rtmptest.PlayOptions{NotFound: true})
	_, err := ReadRTMP(context.Background(), s.URL("live/s"), ReadOptions{Timeout: rtmpWait})
	var st *rtmp.StatusError
	require.ErrorAs(t, err, &st)
}
