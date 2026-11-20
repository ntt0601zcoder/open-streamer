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

// muxedTags returns the RTMP messages our muxer makes of h264_aac.ts, as
// FLV tags.
func muxedTags(t *testing.T) []rtmp.FLVTag {
	t.Helper()
	m := flv.NewMuxer()
	var out []rtmp.FLVTag
	for _, f := range streamClip(t, "h264_aac.ts") {
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

// editVideo rewrites a legacy AVC video tag.
func editVideo(t *testing.T, tag rtmp.FLVTag, edit func(*rtmp.VideoTag)) rtmp.FLVTag {
	t.Helper()
	v, err := rtmp.ParseVideoTag(tag.Data)
	require.NoError(t, err)
	edit(&v)
	tag.Data, err = rtmp.AppendVideoTag(nil, v, false)
	require.NoError(t, err)
	return tag
}

// dropNAL removes the NAL units of an H.264 type from 4-byte length-prefixed
// data.
func dropNAL(t *testing.T, data []byte, typ byte) []byte {
	t.Helper()
	nalus, err := splitNALUs(data, 4)
	require.NoError(t, err)
	var out []byte
	for _, n := range nalus {
		if n[0]&0x1f != typ {
			out = binary.BigEndian.AppendUint32(out, uint32(len(n)))
			out = append(out, n...)
		}
	}
	return out
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

func TestReadRTMPViolations(t *testing.T) {
	base := muxedTags(t)
	del := func(tags []rtmp.FLVTag, i int) []rtmp.FLVTag { return slices.Delete(tags, i, i+1) }
	for name, tc := range map[string]struct {
		edit func([]rtmp.FLVTag) []rtmp.FLVTag
		want string // "" means clean
	}{
		"clean": {func(tags []rtmp.FLVTag) []rtmp.FLVTag { return tags }, ""},
		"timestamps wrap": {func(tags []rtmp.FLVTag) []rtmp.FLVTag {
			for i := range tags {
				tags[i].Timestamp += 1<<32 - 1000
			}
			return tags
		}, ""},
		"second epoch with headers": {func(tags []rtmp.FLVTag) []rtmp.FLVTag {
			return newEpoch(t, tags, 3)
		}, ""},
		"media before header": {func(tags []rtmp.FLVTag) []rtmp.FLVTag {
			return del(tags, nth(t, tags, tagVideoHeader, 1))
		}, RuleRTMPHeader},
		"second epoch without headers": {func(tags []rtmp.FLVTag) []rtmp.FLVTag {
			k := nth(t, tags, tagVideoKey, 3)
			meta := tags[nth(t, tags, tagMeta, 1)]
			meta.Timestamp = tags[k].Timestamp
			return slices.Insert(tags, k, meta)
		}, RuleRTMPHeader},
		"no metadata": {func(tags []rtmp.FLVTag) []rtmp.FLVTag {
			// Another data message first: the play server then sends no
			// onMetaData of its own.
			i := nth(t, tags, tagMeta, 1)
			b, err := rtmp.EncodeAMF0("onTextData", rtmp.ECMAArray{{Key: "text", Value: "x"}})
			require.NoError(t, err)
			tags[i].Data = b
			return tags
		}, RuleRTMPMetadata},
		"delta first": {func(tags []rtmp.FLVTag) []rtmp.FLVTag {
			return del(tags, nth(t, tags, tagVideoKey, 1))
		}, RuleEpochKey},
		"backward timestamp": {func(tags []rtmp.FLVTag) []rtmp.FLVTag {
			i := nth(t, tags, tagVideoDelta, 10)
			tags[i].Timestamp = tags[nth(t, tags, tagVideoDelta, 8)].Timestamp
			return tags
		}, RuleRTMPTimestamp},
		"missing in-band SPS": {func(tags []rtmp.FLVTag) []rtmp.FLVTag {
			i := nth(t, tags, tagVideoKey, 2)
			tags[i] = editVideo(t, tags[i], func(v *rtmp.VideoTag) { v.Data = dropNAL(t, v.Data, 7) })
			return tags
		}, RuleRTMPInband},
		"negative composition time": {func(tags []rtmp.FLVTag) []rtmp.FLVTag {
			i := nth(t, tags, tagVideoDelta, 5)
			tags[i] = editVideo(t, tags[i], func(v *rtmp.VideoTag) { v.CompTime = -40 })
			return tags
		}, RuleRTMPCompTime},
		"bad NAL length": {func(tags []rtmp.FLVTag) []rtmp.FLVTag {
			i := nth(t, tags, tagVideoDelta, 5)
			tags[i] = editVideo(t, tags[i], func(v *rtmp.VideoTag) { v.Data = append(v.Data, 0, 0, 1, 0) })
			return tags
		}, RuleRTMPNALU},
		"bad tag": {func(tags []rtmp.FLVTag) []rtmp.FLVTag {
			i := nth(t, tags, tagAudio, 5)
			tags[i].Data = []byte{0x2f, 1} // MP3
			return tags
		}, RuleRTMPTag},
	} {
		t.Run(name, func(t *testing.T) {
			tags := make([]rtmp.FLVTag, len(base))
			copy(tags, base)
			c := readTags(t, tc.edit(tags))
			if tc.want == "" {
				assert.Empty(t, c.Violations)
				return
			}
			assert.Contains(t, rules(c.Violations), tc.want, "%v", c.Violations)
		})
	}
}

// The second capture epoch opens at the second onMetaData.
func TestReadRTMPEpochs(t *testing.T) {
	c := readTags(t, newEpoch(t, muxedTags(t), 3))
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
	s := rtmptest.NewPlayServer(t, muxedTags(t), rtmptest.PlayOptions{Hold: true})
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
	s := rtmptest.NewPlayServer(t, muxedTags(t), rtmptest.PlayOptions{Hold: true})
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
	s := rtmptest.NewPlayServer(t, muxedTags(t), rtmptest.PlayOptions{Hold: true})
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
