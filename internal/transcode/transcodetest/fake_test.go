//go:build unix

package transcodetest_test

import (
	"bytes"
	"encoding/hex"
	"image/jpeg"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/transcodetest"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/wire"
)

func TestMain(m *testing.M) {
	if os.Getenv("OS_TRANSCODE_FAKE") == "1" {
		transcodetest.Main()
	}
	os.Exit(m.Run())
}

// fake is a running fake child and the host's end of its socket.
type fake struct {
	cmd  *exec.Cmd
	conn net.Conn
	w    *wire.Writer
	r    *wire.Reader
}

// startFake runs the test binary as the fake child with env and reads its
// Hello.
func startFake(t *testing.T, env ...string) *fake {
	t.Helper()
	f, _ := startFakeHello(t, env...)
	return f
}

// startFakeHello runs the test binary as the fake child with env and returns
// its Hello.
func startFakeHello(t *testing.T, env ...string) (*fake, wire.Hello) {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	require.NoError(t, err)
	host := os.NewFile(uintptr(fds[0]), "host")
	peer := os.NewFile(uintptr(fds[1]), "child")
	conn, err := net.FileConn(host)
	require.NoError(t, err)
	_ = host.Close()

	cmd := exec.Command(exe) //nolint:noctx // killed in Cleanup
	cmd.Env = append([]string{"OS_TRANSCODE_FAKE=1", "GORACE=atexit_sleep_ms=0"}, env...)
	cmd.ExtraFiles = []*os.File{peer}
	require.NoError(t, cmd.Start())
	_ = peer.Close()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = conn.Close()
	})
	require.NoError(t, conn.SetDeadline(time.Now().Add(20*time.Second)))
	f := &fake{cmd: cmd, conn: conn, w: wire.NewWriter(conn), r: wire.NewReader(conn)}
	m, err := f.r.Read()
	require.NoError(t, err)
	h, ok := m.(wire.Hello)
	require.True(t, ok, "first message is %T", m)
	return f, h
}

func (f *fake) send(t *testing.T, msgs ...wire.Message) {
	t.Helper()
	for _, m := range msgs {
		require.NoError(t, f.w.Write(m))
	}
}

// stop sends Stop and waits for a clean exit.
func (f *fake) stop(t *testing.T) {
	t.Helper()
	f.send(t, wire.Stop{})
	require.Equal(t, 0, f.wait(t))
}

// readUntil reads until a Stats satisfies done, and returns the Shots seen.
func (f *fake) readUntil(t *testing.T, done func(wire.Stats) bool) []wire.Shot {
	t.Helper()
	var shots []wire.Shot
	for {
		m, err := f.r.Read()
		require.NoError(t, err)
		switch v := m.(type) {
		case wire.Shot:
			shots = append(shots, v)
		case wire.Stats:
			if done(v) {
				return shots
			}
		}
	}
}

func h264(t *testing.T) *media.TrackConfig {
	t.Helper()
	sps, _ := hex.DecodeString("674d400deca0a0fd80880000030008000003019078a14cb0")
	pps, _ := hex.DecodeString("68ebe3cb20")
	c, err := media.NewH264Config([][]byte{sps}, [][]byte{pps})
	require.NoError(t, err)
	return c
}

func aac(t *testing.T) *media.TrackConfig {
	t.Helper()
	c, err := media.NewAACConfig([]byte{0x11, 0x90})
	require.NoError(t, err)
	return c
}

func video(cfg *media.TrackConfig, pts int64, epoch uint32, key bool) wire.Frame {
	return wire.Frame{Rung: -1, F: media.Frame{Config: cfg, PTS: pts, DTS: pts, Epoch: epoch, Key: key, Data: [][]byte{{0x65}}}}
}

func TestFakeHelloIsVersion2(t *testing.T) {
	f, h := startFakeHello(t)
	assert.Equal(t, uint32(2), h.Version)
	f.stop(t)
}

// A child with EnvExitAfterHello exits 2 right after its Hello, before it
// reads anything.
func TestFakeExitAfterHello(t *testing.T) {
	f := startFake(t, transcodetest.EnvExitAfterHello+"=1")
	assertExit2(t, f)
	_, err := f.r.Read()
	assert.ErrorIs(t, err, io.EOF)
}

func assertExit2(t *testing.T, f *fake) {
	t.Helper()
	assert.Equal(t, 2, f.wait(t))
}

// wait waits up to 10 s for the child to exit on its own and returns its
// exit code.
func (f *fake) wait(t *testing.T) int {
	t.Helper()
	done := make(chan struct{})
	go func() {
		_ = f.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
		return f.cmd.ProcessState.ExitCode()
	case <-time.After(10 * time.Second):
		_ = f.cmd.Process.Kill()
		<-done
		t.Fatal("the child did not exit")
		return -1
	}
}

// With EnvShots=2, every second video keyframe yields a 1×1 JPEG Shot with
// the key's PTS and epoch, counted in Stats.Thumbnails. A thumbnail-only
// Configure is accepted.
func TestFakeShots(t *testing.T) {
	f := startFake(t, transcodetest.EnvShots+"=2", transcodetest.EnvStatsInterval+"=10ms")
	v, a := h264(t), aac(t)
	f.send(t,
		wire.Configure{Thumbnail: &picture.Thumbnail{}},
		video(v, 0, 1, true),
		video(v, 1500, 1, false),
		video(v, 3000, 1, true),
		wire.Frame{Rung: -1, F: media.Frame{Config: a, PTS: 3000, DTS: 3000, Key: true, Data: [][]byte{{1}}}},
		video(v, 6000, 2, true),
		video(v, 9000, 2, true),
	)
	shots := f.readUntil(t, func(s wire.Stats) bool { return s.Thumbnails == 2 })
	require.Len(t, shots, 2)
	assert.Equal(t, int64(3000), shots[0].S.PTS)
	assert.Equal(t, uint32(1), shots[0].S.Epoch)
	assert.Equal(t, int64(9000), shots[1].S.PTS)
	assert.Equal(t, uint32(2), shots[1].S.Epoch)
	for _, s := range shots {
		assert.Equal(t, 1, s.S.Width)
		assert.Equal(t, 1, s.S.Height)
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(s.S.JPEG))
		require.NoError(t, err)
		assert.Equal(t, 1, cfg.Width)
		assert.Equal(t, 1, cfg.Height)
	}
	f.stop(t)
}

// With EnvShotFail, no Shot is sent and every video keyframe counts in
// Stats.ThumbnailsFailed.
func TestFakeShotFail(t *testing.T) {
	f := startFake(t, transcodetest.EnvShots+"=1", transcodetest.EnvShotFail+"=1",
		transcodetest.EnvStatsInterval+"=10ms")
	v := h264(t)
	f.send(t,
		wire.Configure{Rungs: []wire.Rung{{Height: 240}}},
		video(v, 0, 0, true), video(v, 1, 0, false), video(v, 2, 0, true), video(v, 3, 0, true),
	)
	shots := f.readUntil(t, func(s wire.Stats) bool { return s.ThumbnailsFailed == 3 })
	assert.Empty(t, shots)
	f.stop(t)
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// EnvWatermarkLog gets one line per Configure and per SetWatermark, appended
// across children.
func TestFakeWatermarkLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wm.log")
	env := transcodetest.EnvWatermarkLog + "=" + path

	f := startFake(t, env)
	f.send(t,
		wire.Configure{Watermark: &picture.Watermark{Text: "LIVE %H:%M ✓", Position: picture.Center}},
		wire.SetWatermark{W: &picture.Watermark{Image: []byte{1, 2, 3, 4, 5}}},
		wire.SetWatermark{},
		wire.SetWatermark{W: &picture.Watermark{Text: "x"}},
	)
	f.stop(t)

	f = startFake(t, env)
	f.send(t, wire.Configure{})
	f.stop(t)

	f = startFake(t, env)
	f.send(t, wire.Configure{Watermark: &picture.Watermark{Image: make([]byte, 1234)}})
	f.stop(t)

	assert.Equal(t, []string{
		"configure text:LIVE %H:%M ✓",
		"set image:5",
		"set none",
		"set text:x",
		"configure none",
		"configure image:1234",
	}, readLines(t, path))
}

// EnvFrameLog gets one line per received frame, written as it arrives.
func TestFakeFrameLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frames.log")
	f := startFake(t, transcodetest.EnvFrameLog+"="+path)
	v, a := h264(t), aac(t)
	f.send(t,
		wire.Configure{Rungs: []wire.Rung{{Height: 240}}},
		video(v, 90000, 3, true),
		wire.Frame{Rung: -1, F: media.Frame{Track: 1, Config: a, PTS: -5, DTS: -5, Epoch: 3, Data: [][]byte{{1}}}},
		video(v, 93000, 4, false),
	)
	// The frames are echoed after they are logged: once the last echo is
	// back, every line is in the file while the child still runs.
	for range 3 {
		for {
			m, err := f.r.Read()
			require.NoError(t, err)
			if _, ok := m.(wire.Frame); ok {
				break
			}
		}
	}
	assert.Equal(t, []string{
		"video 0 90000 3 key",
		"audio 1 -5 3 -",
		"video 0 93000 4 -",
	}, readLines(t, path))
	f.stop(t)
}

// A wrong-version child is never sent Configure, so only EnvExitAfterHello
// can make it exit: the fake's other exits fire after Configure.
func TestFakeVersionOverride(t *testing.T) {
	f, h := startFakeHello(t, transcodetest.EnvVersion+"=1", transcodetest.EnvExitAfterHello+"=1")
	assert.Equal(t, uint32(1), h.Version)
	assertExit2(t, f)
}
