package tsstream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
)

// fixtureFrames returns a fixture's frames in stream time.
func fixtureFrames(t *testing.T, name string) []media.Frame {
	t.Helper()
	data, err := os.ReadFile("../../../testdata/fixtures/" + name)
	require.NoError(t, err)
	d, err := ts.NewDemuxer(bytes.NewReader(data))
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

// recSink records the order of sink calls.
type recSink struct {
	calls   []string
	failAt  int // fail the Write with this 1-based index; 0 never
	writes  int
	failErr error
	bytes   int
}

func (r *recSink) Write(p []byte) (int, error) {
	r.calls = append(r.calls, "write")
	r.writes++
	if r.failAt > 0 && r.writes == r.failAt {
		return 0, r.failErr
	}
	r.bytes += len(p)
	return len(p), nil
}

func (r *recSink) SetWriteDeadline(time.Time) error {
	r.calls = append(r.calls, "deadline")
	return nil
}

func (r *recSink) Flush() error {
	r.calls = append(r.calls, "flush")
	return nil
}

// run feeds frames to Stream through a hub and returns once Stream ends.
func run(t *testing.T, frames []media.Frame, s Sink) (Stats, error) {
	t.Helper()
	h := hub.New(hub.Options{})
	type res struct {
		st  Stats
		err error
	}
	done := make(chan res, 1)
	go func() {
		st, err := Stream(context.Background(), h, "test", s, Options{})
		done <- res{st, err}
	}()
	require.Eventually(t, func() bool { return len(h.Stats()) > 0 }, 5*time.Second, time.Millisecond)
	for _, f := range frames {
		h.Write(f)
	}
	h.Close()
	select {
	case r := <-done:
		return r.st, r.err
	case <-time.After(5 * time.Second):
		t.Fatal("Stream did not end")
		return Stats{}, nil
	}
}

func TestStreamSink(t *testing.T) {
	t.Run("deadline before and flush after each write", func(t *testing.T) {
		s := &recSink{}
		st, err := run(t, fixtureFrames(t, "h264_aac.ts"), s)
		require.NoError(t, err)
		assert.Equal(t, EndedClosed, st.Ended)
		assert.Positive(t, st.Frames)
		require.NotEmpty(t, s.calls)
		require.Zero(t, len(s.calls)%3)
		for i := 0; i < len(s.calls); i += 3 {
			assert.Equal(t, []string{"deadline", "write", "flush"}, s.calls[i:i+3], "call %d", i/3)
		}
		assert.Equal(t, int(st.Frames), len(s.calls)/3)
	})

	t.Run("write error ends Stream wrapped", func(t *testing.T) {
		boom := errors.New("boom")
		s := &recSink{failAt: 3, failErr: boom}
		st, err := run(t, fixtureFrames(t, "h264_aac.ts"), s)
		require.ErrorIs(t, err, boom)
		assert.Equal(t, EndedError, st.Ended)
		assert.Equal(t, 3, s.writes, "no write after the failing one")
	})
}

const (
	patPID   = 0
	pmtPID   = 4096
	videoPID = 256 // track 0
	audioPID = 257 // track 1
)

// tsPackets parses every packet of data, cloning the payloads.
func tsPackets(t *testing.T, data []byte) []mpegts.Packet {
	t.Helper()
	r := mpegts.NewReader(bytes.NewReader(data))
	var out []mpegts.Packet
	for {
		p, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		require.NoError(t, err)
		p.Payload = bytes.Clone(p.Payload)
		out = append(out, p)
	}
}

// pesPTS returns the PTS of the PES header at the start of b, or -1.
func pesPTS(b []byte) int64 {
	if len(b) < 14 || b[0] != 0 || b[1] != 0 || b[2] != 1 || b[7]&0x80 == 0 {
		return -1
	}
	return int64(b[9]>>1&0x07)<<30 | int64(b[10])<<22 | int64(b[11]>>1)<<15 |
		int64(b[12])<<7 | int64(b[13]>>1)
}

// checkResyncs checks every resume point of a TS byte stream and returns
// how many there are. A resume point is a packet on resumePID carrying the
// discontinuity indicator. At each one: the two packets before it are the
// PAT and the PMT; it starts a PES, with the random access indicator and a
// PCR when video is set (a keyframe); from there on, the first packet of
// every PID in pids carries the indicator and no other packet does; and with
// video set, the first audio PES is not presented before the keyframe.
func checkResyncs(t *testing.T, data []byte, resumePID uint16, video bool, pids []uint16) int {
	t.Helper()
	pkts := tsPackets(t, data)
	groups, start := 0, -1
	seen := map[uint16]bool{}
	var resumePTS int64
	audioDone := true
	for i, p := range pkts {
		if p.PID == resumePID && p.Discontinuity {
			groups++
			start = i
			clear(seen)
			audioDone = !video
			require.GreaterOrEqual(t, i, 2, "resume %d", groups)
			assert.Equal(t, uint16(patPID), pkts[i-2].PID, "resume %d: PAT before", groups)
			assert.Equal(t, uint16(pmtPID), pkts[i-1].PID, "resume %d: PMT before", groups)
			assert.True(t, p.PUSI, "resume %d: starts a PES", groups)
			if video {
				assert.True(t, p.RandomAccess, "resume %d: keyframe", groups)
				assert.GreaterOrEqual(t, p.PCR, int64(0), "resume %d: PCR", groups)
			}
			resumePTS = pesPTS(p.Payload)
			require.GreaterOrEqual(t, resumePTS, int64(0))
		}
		es := slices.Contains(pids, p.PID)
		want := start >= 0 && es && !seen[p.PID]
		if es && start >= 0 {
			seen[p.PID] = true
		}
		assert.Equal(t, want, p.Discontinuity, "packet %d PID %d", i, p.PID)
		if !audioDone && p.PID == audioPID && p.PUSI {
			audioDone = true
			assert.GreaterOrEqual(t, pesPTS(p.Payload), resumePTS, "resume %d: first audio PTS", groups)
		}
	}
	return groups
}

// conformTS reads data back through conform.ReadTS from a local server.
func conformTS(t *testing.T, data []byte) conform.Capture {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		_, _ = w.Write(data)
	}))
	defer srv.Close()
	c, err := conform.ReadTS(context.Background(), srv.URL, conform.ReadOptions{Timeout: 5 * time.Second})
	require.NoError(t, err)
	return c
}

// repeat returns frames n times back to back, each copy shifted past the
// previous one so that timestamps keep increasing on every track.
func repeat(frames []media.Frame, n int) []media.Frame {
	lo, hi := frames[0].DTS, frames[0].DTS
	for _, f := range frames {
		lo, hi = min(lo, f.DTS), max(hi, f.DTS)
	}
	span := hi - lo + 3600
	out := make([]media.Frame, 0, n*len(frames))
	for c := range n {
		for _, f := range frames {
			f.DTS += int64(c) * span
			f.PTS += int64(c) * span
			out = append(out, f)
		}
	}
	return out
}

// slowSink records the bytes; while paused, Write waits for resume.
type slowSink struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	paused chan struct{}
	last   time.Time
}

func (s *slowSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	ch := s.paused
	s.mu.Unlock()
	if ch != nil {
		<-ch
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Write(p)
	s.last = time.Now()
	return len(p), nil
}

func (s *slowSink) SetWriteDeadline(time.Time) error { return nil }
func (s *slowSink) Flush() error                     { return nil }

func (s *slowSink) pause() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paused = make(chan struct{})
}

func (s *slowSink) resume() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.paused)
	s.paused = nil
}

// idle reports whether nothing was written for d.
func (s *slowSink) idle(d time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.last) >= d
}

func (s *slowSink) bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Clone(s.buf.Bytes())
}

// TestStreamResyncAfterLoss: a sink that stalls while frames keep coming
// makes the hub skip; after each gap the stream resumes at a keyframe with
// PAT and PMT before it, the discontinuity indicator on the first packet of
// every PID, and no audio presented before the keyframe. The bytes read back
// clean.
func TestStreamResyncAfterLoss(t *testing.T) {
	frames := repeat(fixtureFrames(t, "h264_aac.ts"), 3)
	h := hub.New(hub.Options{})
	sink := &slowSink{}
	type res struct {
		st  Stats
		err error
	}
	done := make(chan res, 1)
	go func() {
		st, err := Stream(context.Background(), h, "test", sink, Options{Queue: 8})
		done <- res{st, err}
	}()
	require.Eventually(t, func() bool { return len(h.Stats()) > 0 }, 5*time.Second, time.Millisecond)

	next := 0
	waitIdle := func() {
		require.Eventually(t, func() bool { return sink.idle(20 * time.Millisecond) }, 5*time.Second, time.Millisecond)
	}
	// paced writes n frames, letting the stream catch up every 16 frames.
	paced := func(n int) {
		for k := 0; k < n && next < len(frames); k++ {
			h.Write(frames[next])
			next++
			if k%16 == 15 {
				waitIdle()
			}
		}
		waitIdle()
	}
	// burst writes n frames while the sink is stalled: far more than the
	// queue holds.
	burst := func(n int) {
		sink.pause()
		for k := 0; k < n && next < len(frames); k++ {
			h.Write(frames[next])
			next++
		}
		sink.resume()
		waitIdle()
	}
	paced(150)
	burst(150)
	paced(250)
	burst(150)
	paced(len(frames))
	h.Close()

	var r res
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stream did not end")
	}
	require.NoError(t, r.err)
	st := r.st
	assert.Equal(t, EndedClosed, st.Ended)
	data := sink.bytes()

	groups := checkResyncs(t, data, videoPID, true, []uint16{videoPID, audioPID})
	assert.GreaterOrEqual(t, groups, 2)
	assert.Equal(t, uint64(groups), st.Resyncs)
	assert.Positive(t, st.DroppedEarlyAudio)
	// The hub resumes a skipping subscriber at a keyframe, so the stream's
	// own wait drops nothing here.
	assert.Zero(t, st.DroppedBeforeKey)

	c := conformTS(t, data)
	assert.Empty(t, c.Violations)
	assert.Len(t, c.Frames, int(st.Frames))
}

// bufSink collects the bytes; deadlines and flushes are no-ops.
type bufSink struct{ bytes.Buffer }

func (*bufSink) SetWriteDeadline(time.Time) error { return nil }
func (*bufSink) Flush() error                     { return nil }

// runDeliveries runs a session over ds with a one-tick prebuffer and
// returns its stats and output.
func runDeliveries(t *testing.T, ds []hub.Delivery) (Stats, []byte) {
	t.Helper()
	ch := make(chan hub.Delivery, len(ds))
	for _, d := range ds {
		ch <- d
	}
	close(ch)
	out := &bufSink{}
	s := &session{s: out, timeout: time.Second}
	require.NoError(t, s.run(context.Background(), ch, 1))
	return s.stats, out.Bytes()
}

func deliveries(frames []media.Frame) []hub.Delivery {
	ds := make([]hub.Delivery, len(frames))
	for i, f := range frames {
		ds[i] = hub.Delivery{Frame: f}
	}
	return ds
}

// TestStreamResyncRules drives the resync rules with deliveries the hub
// would not produce on its own (a gap reported on a delta frame, a video
// track that ended), counting every dropped frame.
func TestStreamResyncRules(t *testing.T) {
	t.Run("gap on a delta frame waits for the keyframe", func(t *testing.T) {
		frames := fixtureFrames(t, "h264_aac.ts")
		j := 200
		for frames[j].Track != 0 || frames[j].Key {
			j++
		}
		k := j + 1
		for frames[k].Track != 0 || !frames[k].Key {
			k++
		}
		var early uint64
		for _, f := range frames[k+1:] {
			if f.Track == 1 && f.PTS < frames[k].PTS {
				early++
			}
		}
		require.Positive(t, early, "the fixture has audio presented before its keyframes")
		ds := deliveries(frames)
		ds[j].Lost = 7

		st, data := runDeliveries(t, ds)
		assert.Equal(t, uint64(1), st.Resyncs)
		assert.Equal(t, uint64(k-j), st.DroppedBeforeKey)
		assert.Equal(t, early, st.DroppedEarlyAudio)
		assert.Equal(t, uint64(len(frames))-uint64(k-j)-early, st.Frames)
		assert.Equal(t, 1, checkResyncs(t, data, videoPID, true, []uint16{videoPID, audioPID}))
		c := conformTS(t, data)
		assert.Empty(t, c.Violations)
		assert.Len(t, c.Frames, int(st.Frames))
	})

	t.Run("audio-only resumes at the next frame", func(t *testing.T) {
		var frames []media.Frame
		for _, f := range fixtureFrames(t, "h265_aac51.ts") {
			if f.Track == 1 {
				frames = append(frames, f)
			}
		}
		ds := deliveries(frames)
		ds[100].Lost = 3
		ds[200].Lost = 1

		st, data := runDeliveries(t, ds)
		assert.Equal(t, uint64(2), st.Resyncs)
		assert.Zero(t, st.DroppedBeforeKey)
		assert.Zero(t, st.DroppedEarlyAudio)
		assert.Equal(t, uint64(len(frames)), st.Frames)
		assert.Equal(t, 2, checkResyncs(t, data, audioPID, false, []uint16{audioPID}))
		c := conformTS(t, data)
		assert.Empty(t, c.Violations)
	})

	t.Run("video that ended: audio resumes after the live window", func(t *testing.T) {
		all := fixtureFrames(t, "h264_aac.ts")
		var av, video []media.Frame
		var acfg *media.TrackConfig
		var lastDTS int64
		for _, f := range all {
			if f.DTS < 2*media.ClockRate {
				av = append(av, f)
				lastDTS = max(lastDTS, f.DTS)
			}
			if f.Track == 1 && acfg == nil {
				acfg = f.Config
			}
			if f.Track == 0 {
				video = append(video, f)
			}
		}
		// 12 s of audio alone after the A/V start.
		const n = 12 * 48000 / 1024
		audio := make([]media.Frame, 0, n)
		for i := range n {
			ts := lastDTS + 1920 + int64(i)*1920
			audio = append(audio, media.Frame{
				Track: 1, Config: acfg, PTS: ts, DTS: ts, Key: true,
				Data: [][]byte{{0x21, 0x10, 0x04, 0x60, 0x8C, 0x1C}}, Epoch: 1,
			})
		}
		var waitDropped uint64
		for _, f := range audio {
			if f.DTS-audio[0].DTS < hub.LiveWindow {
				waitDropped++
			}
		}
		// Video comes back: a delta frame, then a keyframe and its GOP.
		shift := audio[len(audio)-1].DTS + 3600 - video[1].DTS
		back := make([]media.Frame, 0, 25)
		require.False(t, video[1].Key)
		for _, f := range video[1:26] {
			f.DTS += shift
			f.PTS += shift
			back = append(back, f)
		}
		ds := deliveries(av)
		ds = append(ds, deliveries(audio)...)
		ds[len(av)].Lost = 2
		ds = append(ds, deliveries(back)...)

		var deltas uint64
		for _, f := range back {
			if f.Key {
				break
			}
			deltas++
		}
		require.Positive(t, deltas)

		st, data := runDeliveries(t, ds)
		assert.Equal(t, uint64(1), st.Resyncs)
		assert.Equal(t, waitDropped+deltas, st.DroppedBeforeKey)
		assert.Equal(t, 1, checkResyncs(t, data, audioPID, false, []uint16{videoPID, audioPID}))
		// The returning video starts at its keyframe.
		pkts := tsPackets(t, data)
		var after bool
		for _, p := range pkts {
			if p.PID == audioPID && p.Discontinuity {
				after = true
			}
			if after && p.PID == videoPID && p.PUSI {
				assert.True(t, p.RandomAccess, "first video after the resume is a keyframe")
				break
			}
		}
	})
}

// TestStreamLateTrack: a track that starts after the prebuffer is added with
// fresh tables and counted. No fixture has a track that starts late, so the
// audio of h264_aac.ts is cut for its first two seconds.
func TestStreamLateTrack(t *testing.T) {
	var frames []media.Frame
	for _, f := range fixtureFrames(t, "h264_aac.ts") {
		if f.Track == 1 && f.DTS < 2*media.ClockRate {
			continue
		}
		frames = append(frames, f)
	}
	st, err := run(t, frames, &recSink{})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), st.LateTracks)
	assert.Equal(t, uint64(len(frames)), st.Frames)
}

// recMeter records Meter calls.
type recMeter struct {
	pre    [][2]int64 // bytes, span
	frames int
	bytes  int
	last   int64
}

func (m *recMeter) Prebuffered(bytes int, span int64) {
	m.pre = append(m.pre, [2]int64{int64(bytes), span})
}

func (m *recMeter) Frame(dts int64, bytes int) {
	m.frames++
	m.bytes += bytes
	m.last = dts
}

// The Meter sees the prebuffered payload once, before any write, and then
// every frame's TS bytes, which sum to the bytes written.
func TestStreamMeter(t *testing.T) {
	frames := fixtureFrames(t, "h264_aac.ts")
	h := hub.New(hub.Options{})
	m := &recMeter{}
	s := &recSink{}
	done := make(chan Stats, 1)
	go func() {
		st, _ := Stream(context.Background(), h, "test", s, Options{Meter: m})
		done <- st
	}()
	require.Eventually(t, func() bool { return len(h.Stats()) > 0 }, 5*time.Second, time.Millisecond)
	var payload int
	var first int64 = -1
	cut := -1
	for i, f := range frames {
		h.Write(f)
		if first < 0 {
			first = f.DTS
		}
		if cut < 0 {
			for _, b := range f.Data {
				payload += len(b)
			}
			if f.DTS >= first+media.ClockRate {
				cut = i
			}
		}
	}
	h.Close()
	st := <-done
	require.Len(t, m.pre, 1)
	assert.Equal(t, int64(payload), m.pre[0][0], "payload of the prebuffered frames")
	assert.Equal(t, frames[cut].DTS-first, m.pre[0][1])
	assert.Equal(t, int(st.Frames), m.frames)
	assert.Equal(t, s.bytes, m.bytes)
}
