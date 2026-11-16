package ingest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/datarhei/gosrt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
)

// freeUDPPort returns a UDP port that was free a moment ago.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := new(net.ListenConfig).ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer pc.Close()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

func udpPortBound(port int) bool {
	pc, err := new(net.ListenConfig).ListenPacket(context.Background(), "udp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return true
	}
	_ = pc.Close()
	return false
}

// srtPublish dials the listener and writes the fixture paced over about d,
// then closes. It runs in its own goroutine, so it returns its errors
// instead of failing the test.
func srtPublish(port int, streamID string, d time.Duration) error {
	data, err := os.ReadFile(fixturePath("h264_aac.ts"))
	if err != nil {
		return err
	}
	cfg := srt.DefaultConfig()
	cfg.StreamId = streamID
	conn, err := srt.Dial("srt", "127.0.0.1:"+strconv.Itoa(port), cfg)
	if err != nil {
		return err
	}
	defer conn.Close()
	const chunk = 7 * 188
	n := (len(data) + chunk - 1) / chunk
	tick := time.NewTicker(d / time.Duration(n))
	defer tick.Stop()
	for off := 0; off < len(data); off += chunk {
		<-tick.C
		end := min(off+chunk, len(data))
		if _, err := conn.Write(data[off:end]); err != nil {
			return err
		}
	}
	time.Sleep(300 * time.Millisecond) // let the tail drain before closing
	return nil
}

type srtResult struct {
	counts map[int]int
	frames []Frame
	breaks int
	err    error
}

// drain reads r until it fails, in the background.
func drain(r Reader) (*srtResult, *sync.WaitGroup, *sync.Mutex) {
	res := &srtResult{counts: map[int]int{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			f, err := r.Read()
			mu.Lock()
			switch {
			case err == ErrDiscontinuity:
				res.breaks++
			case err != nil:
				res.err = err
				mu.Unlock()
				return
			default:
				res.counts[f.Track]++
				res.frames = append(res.frames, f)
			}
			mu.Unlock()
		}
	}()
	return res, &wg, &mu
}

func fixtureCounts(t *testing.T, name string) map[int]int {
	t.Helper()
	m := map[int]int{}
	for _, f := range demuxFrames(t, name) {
		m[f.Track]++
	}
	return m
}

func listenerURL(port int, extra string) string {
	return fmt.Sprintf("srt://127.0.0.1:%d?mode=listener%s", port, extra)
}

func TestSRTCallerListenerInProcess(t *testing.T) {
	port := freeUDPPort(t)
	pubErr := make(chan error, 1)
	var r Reader
	opened := make(chan error, 1)
	go func() {
		var err error
		r, err = Open(context.Background(), listenerURL(port, ""), Env{})
		opened <- err
	}()
	time.Sleep(200 * time.Millisecond)
	go func() { pubErr <- srtPublish(port, "", 2*time.Second) }()
	require.NoError(t, <-opened)
	assert.Equal(t, Spec{Live: true, Passive: true}, r.Spec())
	assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, r.Kinds())
	res, wg, mu := drain(r)
	require.NoError(t, <-pubErr)
	want := fixtureCounts(t, "h264_aac.ts")
	assert.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(res.counts) == len(want) && res.counts[0] == want[0] && res.counts[1] == want[1]
	}, 3*time.Second, 20*time.Millisecond)
	require.NoError(t, r.Close())
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, want, res.counts)
	assert.Zero(t, res.breaks)
	for _, f := range res.frames {
		assert.False(t, f.Arrival.IsZero())
	}
}

func TestSRTListenerRejectsSecond(t *testing.T) {
	port := freeUDPPort(t)
	opened := openAsync(t, context.Background(), listenerURL(port, ""), Env{})
	time.Sleep(200 * time.Millisecond)
	first := make(chan error, 1)
	go func() { first <- srtPublish(port, "", 2*time.Second) }()
	o := <-opened
	require.NoError(t, o.err)
	r := o.r
	defer r.Close()
	_, wg, _ := drain(r)
	time.Sleep(500 * time.Millisecond)

	_, err := srt.Dial("srt", "127.0.0.1:"+strconv.Itoa(port), srt.DefaultConfig())
	require.Error(t, err, "a second publisher is rejected while the first is connected")
	require.NoError(t, <-first)
	require.NoError(t, r.Close())
	wg.Wait()
}

func TestSRTListenerStreamID(t *testing.T) {
	port := freeUDPPort(t)
	opened := openAsync(t, context.Background(), listenerURL(port, "&streamid=live"), Env{})
	time.Sleep(200 * time.Millisecond)
	cfg := srt.DefaultConfig()
	cfg.StreamId = "other"
	_, err := srt.Dial("srt", "127.0.0.1:"+strconv.Itoa(port), cfg)
	require.Error(t, err)
	pub := make(chan error, 1)
	go func() { pub <- srtPublish(port, "live", time.Second) }()
	o := <-opened
	require.NoError(t, o.err)
	require.NoError(t, o.r.Close())
	<-pub // fails once the listener is gone; only its end matters
}

func TestSRTListenerReconnect(t *testing.T) {
	port := freeUDPPort(t)
	opened := openAsync(t, context.Background(), listenerURL(port, ""), Env{})
	time.Sleep(200 * time.Millisecond)
	pub := make(chan error, 1)
	go func() {
		if err := srtPublish(port, "", time.Second); err != nil {
			pub <- fmt.Errorf("first publisher: %w", err)
			return
		}
		time.Sleep(time.Second)
		if !udpPortBound(port) {
			pub <- errors.New("the listener port was released between publishers")
			return
		}
		if err := srtPublish(port, "", time.Second); err != nil {
			pub <- fmt.Errorf("second publisher: %w", err)
			return
		}
		pub <- nil
	}()
	o := <-opened
	require.NoError(t, o.err)
	r := o.r
	res, wg, mu := drain(r)
	want := fixtureCounts(t, "h264_aac.ts")
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return res.counts[0] >= 2*want[0] && res.counts[1] >= 2*want[1]
	}, 10*time.Second, 50*time.Millisecond)
	require.NoError(t, <-pub)
	require.NoError(t, r.Close())
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, res.breaks, "one ErrDiscontinuity for the second publisher")
	assert.Equal(t, 2*want[0], res.counts[0])
	assert.Equal(t, 2*want[1], res.counts[1])
	// Equal configs keep their pointer across publishers.
	var firstCfg, lastCfg *media.TrackConfig
	for _, f := range res.frames {
		if f.Track == 0 {
			if firstCfg == nil {
				firstCfg = f.Config
			}
			lastCfg = f.Config
		}
	}
	assert.Same(t, firstCfg, lastCfg)
}

func TestSRTListenerCloseUnblocks(t *testing.T) {
	port := freeUDPPort(t)
	opened := openAsync(t, context.Background(), listenerURL(port, ""), Env{})
	time.Sleep(200 * time.Millisecond)
	pub := make(chan error, 1)
	go func() { pub <- srtPublish(port, "", 500*time.Millisecond) }()
	o := <-opened
	require.NoError(t, o.err)
	r := o.r
	_, wg, _ := drain(r)
	require.NoError(t, <-pub)
	time.Sleep(time.Second) // the publisher is gone; Read waits
	require.NoError(t, r.Close())
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not stop after Close")
	}
	assert.False(t, udpPortBound(port), "Close frees the port")
}

func TestSRTOpenContextAbortsListener(t *testing.T) {
	port := freeUDPPort(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := Open(ctx, listenerURL(port, "&"+kPass+"="+vSecret+vSecret), Env{})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), vSecret+vSecret)
	assert.False(t, udpPortBound(port))
}

func TestSRTPassphrase(t *testing.T) {
	port := freeUDPPort(t)
	ln, err := srt.Listen("srt", "127.0.0.1:"+strconv.Itoa(port), func() srt.Config {
		c := srt.DefaultConfig()
		c.Passphrase = vRight
		return c
	}())
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		for {
			req, err := ln.Accept2()
			if err != nil {
				return
			}
			if req.IsEncrypted() {
				if err := req.SetPassphrase(vRight); err != nil {
					req.Reject(srt.REJ_BADSECRET)
					continue
				}
			}
			conn, err := req.Accept()
			if err != nil {
				continue
			}
			go func() {
				defer conn.Close()
				data, _ := os.ReadFile(fixturePath("h264_aac.ts"))
				for off := 0; off < len(data); off += 1316 {
					if _, err := conn.Write(data[off:min(off+1316, len(data))]); err != nil {
						return
					}
					time.Sleep(time.Millisecond)
				}
			}()
		}
	}()
	url := func(pass string) string {
		return fmt.Sprintf("srt://127.0.0.1:%d?mode=caller&%s=%s", port, kPass, pass)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := Open(ctx, url(vWrong), Env{})
	require.Error(t, err)
	assert.Nil(t, r)
	assert.NotContains(t, err.Error(), vWrong)

	r, err = Open(ctx, url(vRight), Env{})
	require.NoError(t, err)
	defer r.Close()
	f, err := r.Read()
	require.NoError(t, err)
	assert.NotNil(t, f.Config)
}

func TestSRTCallerDisconnectErrors(t *testing.T) {
	port := freeUDPPort(t)
	ln, err := srt.Listen("srt", "127.0.0.1:"+strconv.Itoa(port), srt.DefaultConfig())
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		req, err := ln.Accept2()
		if err != nil {
			return
		}
		conn, err := req.Accept()
		if err != nil {
			return
		}
		data, _ := os.ReadFile(fixturePath("h264_aac.ts"))
		for off := 0; off < len(data)/2; off += 1316 {
			_, _ = conn.Write(data[off:min(off+1316, len(data))])
			time.Sleep(time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
		_ = conn.Close()
	}()
	r, err := Open(context.Background(), fmt.Sprintf("srt://127.0.0.1:%d?mode=caller&latency=50", port), Env{})
	require.NoError(t, err)
	defer r.Close()
	assert.Equal(t, Spec{Live: true}, r.Spec())
	done := make(chan error, 1)
	go func() {
		for {
			if _, err := r.Read(); err != nil && err != ErrDiscontinuity {
				done <- err
				return
			}
		}
	}()
	select {
	case err := <-done:
		assert.Error(t, err)
		assert.NotErrorIs(t, err, ErrDiscontinuity)
	case <-time.After(5 * time.Second):
		t.Fatal("Read did not fail after the peer disconnected")
	}
}

// The caller path delivers every frame of the fixture, then fails when the
// listener hangs up.
func TestSRTCallerFrameCount(t *testing.T) {
	port := freeUDPPort(t)
	ln, err := srt.Listen("srt", "127.0.0.1:"+strconv.Itoa(port), srt.DefaultConfig())
	require.NoError(t, err)
	defer ln.Close()
	data, err := os.ReadFile(fixturePath("h264_aac.ts"))
	require.NoError(t, err)
	served := make(chan error, 1)
	go func() {
		served <- func() error {
			req, err := ln.Accept2()
			if err != nil {
				return err
			}
			conn, err := req.Accept()
			if err != nil {
				return err
			}
			defer conn.Close()
			for off := 0; off < len(data); off += 1316 {
				if _, err := conn.Write(data[off:min(off+1316, len(data))]); err != nil {
					return err
				}
				time.Sleep(time.Millisecond)
			}
			time.Sleep(500 * time.Millisecond) // let the tail drain before closing
			return nil
		}()
	}()
	r, err := Open(context.Background(), fmt.Sprintf("srt://127.0.0.1:%d?mode=caller", port), Env{})
	require.NoError(t, err)
	defer r.Close()
	res, wg, mu := drain(r)
	require.NoError(t, <-served)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Read did not fail after the listener hung up")
	}
	mu.Lock()
	defer mu.Unlock()
	want := map[int]int{}
	for _, f := range demuxFrames(t, "h264_aac.ts") {
		want[f.Track]++
	}
	assert.Equal(t, want, res.counts)
	assert.Zero(t, res.breaks)
	require.Error(t, res.err)
	assert.NotErrorIs(t, res.err, ErrDiscontinuity)
}

func TestOpenSRT(t *testing.T) {
	// Caller row.
	port := freeUDPPort(t)
	ln, err := srt.Listen("srt", "127.0.0.1:"+strconv.Itoa(port), srt.DefaultConfig())
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		req, err := ln.Accept2()
		if err != nil {
			return
		}
		conn, err := req.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		data, _ := os.ReadFile(fixturePath("h264_aac.ts"))
		for off := 0; off < len(data); off += 1316 {
			if _, err := conn.Write(data[off:min(off+1316, len(data))]); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(time.Second)
	}()
	r, err := Open(context.Background(), fmt.Sprintf("srt://127.0.0.1:%d", port), Env{})
	require.NoError(t, err)
	assert.Equal(t, Spec{Live: true}, r.Spec())
	assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, r.Kinds())
	assert.Contains(t, r.Stats(), "cc_errors")
	require.NoError(t, r.Close())

	// Listener row: Spec is Passive, and an open that nobody publishes to
	// fails when its ctx ends.
	lp := freeUDPPort(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err = Open(ctx, listenerURL(lp, ""), Env{})
	require.Error(t, err)

	// Bad latency.
	_, err = Open(context.Background(), fmt.Sprintf("srt://127.0.0.1:%d?latency=x", port), Env{})
	require.Error(t, err)
}

// clockCheck pushes frames through a Clock and checks the frame rules.
func clockCheck(t *testing.T, r Reader, frames []Frame) {
	t.Helper()
	c := clock.New(clock.Options{Kinds: r.Kinds()})
	out := make([]media.Frame, 0, len(frames))
	for _, f := range frames {
		out = append(out, c.PushAt(f.Frame, f.Arrival)...)
	}
	out = append(out, c.Flush()...)
	require.NotEmpty(t, out)
	assert.Nil(t, conform.CheckFrames(out, conform.FrameOptions{}))
}

func TestSRTWithFFmpeg(t *testing.T) {
	ffmpeg := testpeer.Require(t, "ffmpeg", "srt")

	t.Run("ffmpeg caller into our listener", func(t *testing.T) {
		port := freeUDPPort(t)
		ch := openAsync(t, context.Background(), listenerURL(port, "&streamid=cam&latency=200"), Env{})
		time.Sleep(300 * time.Millisecond)
		cmd := exec.CommandContext(t.Context(), ffmpeg, "-hide_banner", "-loglevel", "error", "-re",
			"-i", fixturePath("h264_aac.ts"), "-c", "copy", "-f", "mpegts",
			fmt.Sprintf("srt://127.0.0.1:%d?mode=caller&streamid=cam&latency=200000", port))
		require.NoError(t, cmd.Start())
		defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
		o := <-ch
		require.NoError(t, o.err)
		res, wg, mu := drain(o.r)
		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(res.frames) > 100
		}, 15*time.Second, 50*time.Millisecond)
		require.NoError(t, o.r.Close())
		wg.Wait()
		mu.Lock()
		defer mu.Unlock()
		clockCheck(t, o.r, res.frames)
	})

	t.Run("our caller from ffmpeg listener", func(t *testing.T) {
		port := freeUDPPort(t)
		cmd := exec.CommandContext(t.Context(), ffmpeg, "-hide_banner", "-loglevel", "error", "-re",
			"-stream_loop", "-1", "-i", fixturePath("h264_aac.ts"), "-c", "copy", "-f", "mpegts",
			fmt.Sprintf("srt://127.0.0.1:%d?mode=listener&latency=200000", port))
		require.NoError(t, cmd.Start())
		defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
		var r Reader
		require.Eventually(t, func() bool {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			var err error
			r, err = Open(ctx, fmt.Sprintf("srt://127.0.0.1:%d?mode=caller&latency=200", port), Env{})
			return err == nil
		}, 15*time.Second, 200*time.Millisecond)
		res, wg, mu := drain(r)
		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(res.frames) > 100
		}, 15*time.Second, 50*time.Millisecond)
		require.NoError(t, r.Close())
		wg.Wait()
		mu.Lock()
		defer mu.Unlock()
		clockCheck(t, r, res.frames)
	})
}
