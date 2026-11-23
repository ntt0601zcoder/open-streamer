package testpeer

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDocker is a docker client that appends its arguments to $FAKE_LOG,
// has every image but missing-image, fails a run whose last argument is
// "lacking" (a gst-inspect-1.0 --exists of an element the image lacks), and
// answers a run of --list with two VLC modules.
const fakeDocker = `#!/bin/sh
echo "$@" >> "$FAKE_LOG"
case "$1" in
image) [ "$3" != missing-image ] && exit 0; exit 1 ;;
kill) exit 0 ;;
run)
	for a; do last=$a; done
	[ "$last" = lacking ] && exit 1
	if [ "$last" = --list ]; then
		echo "  access_srt             SRT input"
		echo "  adaptive               Unified adaptive streaming for DASH/HLS"
		exit 0
	fi
	echo "fake 1.0"
	echo more
	exit 0 ;;
esac
exit 2
`

// script writes an executable shell script named name into dir.
func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(body), 0o755)) //nolint:gosec // an executable test script
	return p
}

// useFakeDocker points dockerBin at fakeDocker for the test and returns its
// log file.
func useFakeDocker(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	t.Setenv("FAKE_LOG", log)
	old := dockerBin
	dockerBin = script(t, dir, "docker", fakeDocker)
	t.Cleanup(func() { dockerBin = old })
	return log
}

func readLog(t *testing.T, log string) string {
	t.Helper()
	b, err := os.ReadFile(log)
	require.NoError(t, err)
	return string(b)
}

func TestPeerCommandHost(t *testing.T) {
	sh, err := filepath.Abs("/bin/sh")
	require.NoError(t, err)
	p := Peer{name: "sh", path: sh}
	assert.False(t, p.InImage())
	assert.Equal(t, sh, p.String())
	out, err := p.Command(t.Context(), t, Run{}, "-c", "echo hi").Output()
	require.NoError(t, err)
	assert.Equal(t, "hi\n", string(out))

	// Interrupt wraps the program in timeout -s INT, which stops it.
	start := time.Now()
	err = p.Command(t.Context(), t, Run{Interrupt: 200 * time.Millisecond}, "-c", "sleep 5").Run()
	require.Error(t, err)
	assert.Less(t, time.Since(start), 4*time.Second)
}

func TestPeerCommandImage(t *testing.T) {
	log := useFakeDocker(t)
	p := Peer{name: "cvlc", image: "img"}
	assert.True(t, p.InImage())
	assert.Equal(t, "cvlc in img", p.String())
	dir := t.TempDir()
	t.Run("run", func(t *testing.T) {
		out, err := p.Command(t.Context(), t, Run{Dirs: []string{dir}, Interrupt: 2 * time.Second}, "--version").Output()
		require.NoError(t, err)
		assert.Equal(t, "fake 1.0\nmore\n", string(out))
	})
	lines := strings.Split(strings.TrimSpace(readLog(t, log)), "\n")
	require.Len(t, lines, 2, "the run, then the kill at cleanup")
	run := strings.Fields(lines[0])
	require.GreaterOrEqual(t, len(run), 8)
	assert.Equal(t, []string{"run", "--rm", "-i", "--network", "host", "--name"}, run[:6])
	name := run[6]
	assert.True(t, strings.HasPrefix(name, "osv2-vlc-"), name)
	assert.Contains(t, lines[0], "--user "+hostUser()+" -e HOME=/tmp -v "+dir+":"+dir+" img timeout -k 10 -s INT 2.000 cvlc --version")
	assert.Equal(t, "kill "+name, lines[1])
}

func TestPeerCommandImageCancel(t *testing.T) {
	log := useFakeDocker(t)
	p := Peer{name: "cvlc", image: "img"}
	ctx, cancel := context.WithCancel(t.Context())
	cmd := p.Command(ctx, t, Run{}, "x")
	require.NoError(t, cmd.Start())
	cancel()
	_ = cmd.Wait()
	assert.Contains(t, readLog(t, log), "run --rm")
}

func TestPeerVersion(t *testing.T) {
	dir := t.TempDir()
	ok := script(t, dir, "ok", "#!/bin/sh\necho 'prog 1.2'\necho second\n")
	assert.Equal(t, "prog 1.2", Peer{name: "ok", path: ok}.Version(t))
	bad := script(t, dir, "bad", "#!/bin/sh\necho 'prog 0'\nexit 3\n")
	v := Peer{name: "bad", path: bad}.Version(t)
	assert.True(t, strings.HasPrefix(v, "prog 0 (--version: "), v)
}

func TestHostUser(t *testing.T) {
	if os.Getuid() == 0 {
		assert.Equal(t, containerUser, hostUser())
		return
	}
	assert.Equal(t, strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid()), hostUser())
}

func TestVLC(t *testing.T) {
	useFakeDocker(t)
	empty := t.TempDir()
	withVLC := t.TempDir()
	script(t, withVLC, "cvlc", "#!/bin/sh\n[ \"$1\" = --list ] && echo '  adaptive               Unified adaptive streaming'\nexit 0\n")
	for _, tt := range []struct {
		name, path, image, force, require string
		modules                           []string
		wantImage, wantHost               bool
		wantSkip, wantFail                bool
	}{
		{"host", withVLC, "", "", "", nil, false, true, false, false},
		{"host has the module", withVLC, "", "", "", []string{"adaptive"}, false, true, false, false},
		{"host lacks the module, image has it", withVLC, "vlc-image", "", "", []string{"access_srt"}, true, false, false, false},
		{"host lacks the module, no image", withVLC, "", "", "", []string{"access_srt"}, false, false, true, false},
		{"image lacks the module", empty, "vlc-image", "", "", []string{"live555"}, false, false, true, false},
		{"forced image", withVLC, "vlc-image", "1", "", nil, true, false, false, false},
		{"forced without an image", withVLC, "", "1", "1", nil, false, false, false, true},
		{"image", empty, "vlc-image", "", "", nil, true, false, false, false},
		{"missing image", empty, "missing-image", "", "", nil, false, false, true, false},
		{"none", empty, "", "", "", nil, false, false, true, false},
		{"none required", empty, "", "", "1", nil, false, false, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PATH", tt.path)
			t.Setenv(EnvPeerImage, tt.image)
			t.Setenv(EnvPeerForceImage, tt.force)
			t.Setenv(EnvRequire, tt.require)
			rec := &recorder{TB: t}
			p := VLC(rec, tt.modules...)
			assert.Equal(t, tt.wantSkip, rec.skipped)
			assert.Equal(t, tt.wantFail, rec.failed)
			assert.Equal(t, tt.wantImage, p.InImage())
			assert.Equal(t, tt.wantHost, p.path != "")
		})
	}
}

func TestGStreamer(t *testing.T) {
	useFakeDocker(t)
	empty := t.TempDir()
	withGst := t.TempDir()
	script(t, withGst, "gst-launch-1.0", "#!/bin/sh\n")
	script(t, withGst, "gst-inspect-1.0", "#!/bin/sh\n[ \"$2\" = good ]\n")
	for _, tt := range []struct {
		name, path, image, require string
		elements                   []string
		wantImage, wantHost        bool
		wantSkip, wantFail         bool
	}{
		{"host", withGst, "", "", []string{"good"}, false, true, false, false},
		{"host lacks, image has", withGst, "gst-image", "", []string{"good", "other"}, true, false, false, false},
		{"image has", empty, "gst-image", "", []string{"good"}, true, false, false, false},
		{"image lacks", empty, "gst-image", "", []string{"good", "lacking"}, false, false, true, false},
		{"image lacks required", empty, "gst-image", "1", []string{"lacking"}, false, false, false, true},
		{"none", withGst, "", "", []string{"other"}, false, false, true, false},
		{"none required", empty, "", "1", nil, false, false, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PATH", tt.path)
			t.Setenv(EnvPeerImage, tt.image)
			t.Setenv(EnvPeerForceImage, "")
			t.Setenv(EnvRequire, tt.require)
			rec := &recorder{TB: t}
			p := GStreamer(rec, tt.elements...)
			assert.Equal(t, tt.wantSkip, rec.skipped)
			assert.Equal(t, tt.wantFail, rec.failed)
			assert.Equal(t, tt.wantImage, p.InImage())
			assert.Equal(t, tt.wantHost, p.path != "")
		})
	}
}

func TestGStreamerForcedImage(t *testing.T) {
	useFakeDocker(t)
	withGst := t.TempDir()
	script(t, withGst, "gst-launch-1.0", "#!/bin/sh\n")
	script(t, withGst, "gst-inspect-1.0", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", withGst)
	t.Setenv(EnvPeerImage, "gst-forced")
	t.Setenv(EnvPeerForceImage, "1")
	rec := &recorder{TB: t}
	p := GStreamer(rec, "good")
	assert.False(t, rec.skipped || rec.failed)
	assert.True(t, p.InImage(), "the host has every element, but the image is forced")
}

func TestPeerCommandHostCancelKillsGroup(t *testing.T) {
	sh, err := filepath.Abs("/bin/sh")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	// timeout's child outlives a kill of timeout alone; the group kill ends
	// both, so Wait returns at once instead of after WaitDelay.
	cmd := Peer{name: "sh", path: sh}.Command(ctx, t, Run{Interrupt: time.Minute}, "-c", "sleep 60")
	require.NoError(t, cmd.Start())
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	cancel()
	_ = cmd.Wait()
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestFFmpegSRT(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv(EnvRequire, "")
	rec := &recorder{TB: t}
	assert.Empty(t, FFmpegSRT(rec))
	assert.True(t, rec.skipped)
}
