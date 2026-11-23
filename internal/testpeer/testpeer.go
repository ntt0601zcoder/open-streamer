// Package testpeer finds the external programs that tests talk to.
package testpeer

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// EnvRequire names the environment variable that turns a missing peer from a
// skip into a failure. CI sets it to "1".
const EnvRequire = "OS_REQUIRE_PEERS"

// EnvMediaMTX names the environment variable that holds the host:port of a
// MediaMTX RTMP listener for tests, and EnvRequireMediaMTX the one that turns
// its absence from a skip into a failure. CI sets them to "127.0.0.1:19350"
// and "1", keeping port 1935 free for tests of the RTMP default port.
const (
	EnvMediaMTX        = "OS_MEDIAMTX_RTMP"
	EnvRequireMediaMTX = "OS_REQUIRE_MEDIAMTX"
)

// EnvMediaMTXRTSP names the environment variable that holds the host:port of
// a MediaMTX RTSP listener for tests. CI sets it to "127.0.0.1:18554"; that
// MediaMTX runs on the host network, so that UDP transport works, with the
// RTP and RTCP ports 18000 and 18001.
const EnvMediaMTXRTSP = "OS_MEDIAMTX_RTSP"

// EnvMediaMTXPull names the environment variable that, set to "1", says that
// the MediaMTX RTSP listener of OS_MEDIAMTX_RTSP was started with the paths
// m3b1pull and m3b1pullhevc pulling, on demand, from
// rtmp://127.0.0.1:19361/live/m3b1pull and …/live/m3b1pullhevc, and the paths
// m3b2pull and m3b2pullhevc pulling, on demand and over TCP
// (…_RTSPTRANSPORT=tcp), from rtsp://127.0.0.1:19362/live/m3b2pull and
// …/live/m3b2pullhevc. CI sets it.
const EnvMediaMTXPull = "OS_MEDIAMTX_PULL"

// Require returns the path of binary, which must support every listed
// protocol (checked with "-hide_banner -protocols", so it applies to ffmpeg
// and ffprobe). When the binary or a protocol is missing the test is skipped,
// or failed when OS_REQUIRE_PEERS=1.
func Require(t testing.TB, binary string, protocols ...string) string {
	t.Helper()
	path, err := exec.LookPath(binary)
	if err != nil {
		missing(t, "peer %s not found: %v", binary, err)
		return ""
	}
	if len(protocols) == 0 {
		return path
	}
	out, err := exec.CommandContext(t.Context(), path, "-hide_banner", "-protocols").CombinedOutput()
	if err != nil {
		missing(t, "peer %s: listing protocols: %v", binary, err)
		return ""
	}
	have := map[string]bool{}
	for line := range strings.SplitSeq(string(out), "\n") {
		have[strings.TrimSpace(line)] = true
	}
	for _, p := range protocols {
		if !have[p] {
			missing(t, "peer %s lacks protocol %s", binary, p)
			return ""
		}
	}
	return path
}

// MediaMTX returns the host:port of the MediaMTX RTMP listener named by
// OS_MEDIAMTX_RTMP. When it is unset the test is skipped, or failed when
// OS_REQUIRE_MEDIAMTX=1. MediaMTX is a running server, not a binary, so
// OS_REQUIRE_PEERS does not apply to it.
func MediaMTX(t testing.TB) (rtmpAddr string) {
	t.Helper()
	if addr := os.Getenv(EnvMediaMTX); addr != "" {
		return addr
	}
	if os.Getenv(EnvRequireMediaMTX) == "1" {
		t.Fatalf("peer MediaMTX: %s is not set", EnvMediaMTX)
		return ""
	}
	t.Skipf("peer MediaMTX: %s is not set", EnvMediaMTX)
	return ""
}

// MediaMTXRTSP returns the host:port of the MediaMTX RTSP listener named by
// OS_MEDIAMTX_RTSP. When it is unset the test is skipped, or failed when
// OS_REQUIRE_MEDIAMTX=1.
func MediaMTXRTSP(t testing.TB) (rtspAddr string) {
	t.Helper()
	if addr := os.Getenv(EnvMediaMTXRTSP); addr != "" {
		return addr
	}
	if os.Getenv(EnvRequireMediaMTX) == "1" {
		t.Fatalf("peer MediaMTX: %s is not set", EnvMediaMTXRTSP)
		return ""
	}
	t.Skipf("peer MediaMTX: %s is not set", EnvMediaMTXRTSP)
	return ""
}

// MediaMTXPull returns the host:port of the MediaMTX RTSP listener whose
// paths pull our RTMP and RTSP play outputs (EnvMediaMTXPull). When OS_MEDIAMTX_PULL is
// not "1", or OS_MEDIAMTX_RTSP is unset, the test is skipped, or failed when
// OS_REQUIRE_MEDIAMTX=1.
func MediaMTXPull(t testing.TB) (rtspHostPort string) {
	t.Helper()
	if os.Getenv(EnvMediaMTXPull) == "1" {
		return MediaMTXRTSP(t)
	}
	if os.Getenv(EnvRequireMediaMTX) == "1" {
		t.Fatalf("peer MediaMTX: %s is not 1", EnvMediaMTXPull)
		return ""
	}
	t.Skipf("peer MediaMTX: %s is not 1", EnvMediaMTXPull)
	return ""
}

// RequireMuxer checks that binary (ffmpeg) lists the output format muxer
// ("-hide_banner -muxers"). When it does not the test is skipped, or failed
// when OS_REQUIRE_PEERS=1.
func RequireMuxer(t testing.TB, binary, muxer string) {
	t.Helper()
	path, err := exec.LookPath(binary)
	if err != nil {
		missing(t, "peer %s not found: %v", binary, err)
		return
	}
	out, err := exec.CommandContext(t.Context(), path, "-hide_banner", "-muxers").CombinedOutput()
	if err != nil {
		missing(t, "peer %s: listing muxers: %v", binary, err)
		return
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		// " E  rtsp            RTSP output": flags, then the name.
		if f := strings.Fields(line); len(f) > 2 && strings.Contains(f[0], "E") && f[1] == muxer {
			return
		}
	}
	missing(t, "peer %s lacks muxer %s", binary, muxer)
}

func missing(t testing.TB, format string, args ...any) {
	t.Helper()
	if os.Getenv(EnvRequire) == "1" {
		t.Fatalf(format, args...)
		return
	}
	t.Skipf(format, args...)
}

// EnvPeerImage names the environment variable that holds the image of the
// VLC-class peers (testdata/peers/Dockerfile.vlc): when it is set and the
// host lacks VLC, or GStreamer or one of the elements a test needs, the
// peer runs in a throwaway container of that image on the host network.
const EnvPeerImage = "OS_PEER_IMAGE"

// EnvPeerForceImage names the environment variable that, set to "1", makes
// VLC and GStreamer skip the host's programs and use the image that
// OS_PEER_IMAGE names, as CI's packages would run (Ubuntu 24.04's VLC 3 and
// GStreamer 1.24) on a host whose own differ.
const EnvPeerForceImage = "OS_PEER_FORCE_IMAGE"

// forceImage reports whether OS_PEER_FORCE_IMAGE is "1".
func forceImage() bool { return os.Getenv(EnvPeerForceImage) == "1" }

// dockerBin is the docker client; tests replace it.
var dockerBin = "docker"

// containerUser is the user the image runs as: VLC refuses to run as root.
const containerUser = "peer"

// Peer is a program that a test runs: a host binary, or the same program in
// the peer image.
type Peer struct {
	name  string // the program, as named on the host and in the image
	path  string // the host binary; "" in the image
	image string // the image; "" on the host
}

// String names the program and where it runs.
func (p Peer) String() string {
	if p.image != "" {
		return p.name + " in " + p.image
	}
	return p.path
}

// InImage reports whether the peer runs in the image.
func (p Peer) InImage() bool { return p.image != "" }

// Run tunes Peer.Command.
type Run struct {
	// Dirs are host directories the program reads or writes; in the image
	// they are mounted at the same paths.
	Dirs []string
	// Interrupt, when positive, stops the program with SIGINT after that
	// long, and kills it 10 s later if it is still running (coreutils
	// timeout, on the host and in the image).
	Interrupt time.Duration
}

// Command returns a command that runs the peer with args until it exits or
// ctx ends. On the host the program runs in a process group of its own,
// killed whole when ctx ends (timeout and its child included). In the
// image the container is named osv2-vlc-<random>, uses the host network,
// runs as a non-root user (the host's own user and group when those are not
// root, so that it can write to Dirs), is removed when it exits, and is
// killed when ctx ends and at the test's cleanup.
func (p Peer) Command(ctx context.Context, t testing.TB, run Run, args ...string) *exec.Cmd {
	t.Helper()
	prog := p.name
	if p.image == "" {
		prog = p.path
	}
	argv := append([]string{prog}, args...)
	if run.Interrupt > 0 {
		secs := strconv.FormatFloat(run.Interrupt.Seconds(), 'f', 3, 64)
		argv = append([]string{"timeout", "-k", "10", "-s", "INT", secs}, argv...)
	}
	if p.image == "" {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // test peers
		ownProcessGroup(cmd)
		cmd.WaitDelay = 10 * time.Second
		return cmd
	}
	name := "osv2-vlc-" + strings.ToLower(rand.Text()[:12])
	cmd := exec.CommandContext(ctx, dockerBin, dockerArgs(name, p.image, run.Dirs, argv)...) //nolint:gosec // test peers
	kill := func() {
		// ctx may be done by now: the kill runs on regardless.
		kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(kctx, dockerBin, "kill", name).Run() //nolint:gosec // test peers
	}
	cmd.Cancel = func() error {
		kill()
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 10 * time.Second
	t.Cleanup(kill)
	return cmd
}

// dockerArgs is the docker command line that runs argv in image.
func dockerArgs(name, image string, dirs, argv []string) []string {
	args := []string{"run", "--rm", "-i", "--network", "host", "--name", name, "--user", hostUser(), "-e", "HOME=/tmp"}
	for _, d := range dirs {
		args = append(args, "-v", d+":"+d)
	}
	return append(append(args, image), argv...)
}

// hostUser is the host's user and group when they are not root, else the
// image's non-root user.
func hostUser() string {
	if uid, gid := os.Getuid(), os.Getgid(); uid > 0 && gid >= 0 {
		return strconv.Itoa(uid) + ":" + strconv.Itoa(gid)
	}
	return containerUser
}

// Version returns the first line the peer prints for --version.
func (p Peer) Version(t testing.TB) string {
	t.Helper()
	out, err := p.Command(t.Context(), t, Run{}, "--version").CombinedOutput()
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if err != nil {
		return fmt.Sprintf("%s (--version: %v)", line, err)
	}
	return line
}

// VLC returns cvlc from the host when it has every listed VLC module (as
// cvlc --list names them, e.g. access_srt for SRT, adaptive for HLS and
// DASH), or from the peer image (OS_PEER_IMAGE) when the host lacks cvlc or
// a module, or when OS_PEER_FORCE_IMAGE=1, and the image has the modules.
// When neither does the test is skipped, or failed when
// OS_REQUIRE_PEERS=1.
func VLC(t testing.TB, modules ...string) Peer {
	t.Helper()
	const vlc = "cvlc"
	if path, err := exec.LookPath(vlc); err == nil && !forceImage() {
		if lacks := lacking(vlcModules(t, Peer{name: vlc, path: path}), modules); lacks == "" {
			return Peer{name: vlc, path: path}
		}
	}
	if img := peerImage(t); img != "" {
		p := Peer{name: vlc, image: img}
		lacks := lacking(imageVLCModules(t, p), modules)
		if lacks == "" {
			return p
		}
		missing(t, "peer %s lacks VLC module %s", p, lacks)
		return Peer{}
	}
	missing(t, "peer %s with modules %v not found, and %s names no usable image (%s=%q)",
		vlc, modules, EnvPeerImage, EnvPeerForceImage, os.Getenv(EnvPeerForceImage))
	return Peer{}
}

// vlcModules returns the module names that p's cvlc --list prints.
func vlcModules(t testing.TB, p Peer) map[string]bool {
	out, _ := p.Command(t.Context(), t, Run{}, "--list").Output()
	mods := map[string]bool{}
	for line := range strings.SplitSeq(string(out), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			mods[f[0]] = true
		}
	}
	return mods
}

var (
	vlcMu     sync.Mutex
	vlcByPeer = map[string]map[string]bool{}
)

// imageVLCModules is vlcModules for the image, listed once per image.
func imageVLCModules(t testing.TB, p Peer) map[string]bool {
	vlcMu.Lock()
	defer vlcMu.Unlock()
	mods, ok := vlcByPeer[p.image]
	if !ok {
		mods = vlcModules(t, p)
		vlcByPeer[p.image] = mods
	}
	return mods
}

// lacking returns the first of want that is not in have, or "".
func lacking(have map[string]bool, want []string) string {
	for _, w := range want {
		if !have[w] {
			return w
		}
	}
	return ""
}

// GStreamer returns gst-launch-1.0 from the host when the host has every
// listed element (gst-inspect-1.0 --exists) and OS_PEER_FORCE_IMAGE is not
// "1", else from the peer image (OS_PEER_IMAGE) when the image has them.
// When neither does the test is skipped, or failed when OS_REQUIRE_PEERS=1.
func GStreamer(t testing.TB, elements ...string) Peer {
	t.Helper()
	const launch, inspect = "gst-launch-1.0", "gst-inspect-1.0"
	path, err := exec.LookPath(launch)
	if err == nil && !forceImage() {
		if ins, ierr := exec.LookPath(inspect); ierr == nil && hostHas(t, ins, elements) {
			return Peer{name: launch, path: path}
		}
	}
	if img := peerImage(t); img != "" {
		lacks := imageLacks(t, img, elements)
		if lacks == "" {
			return Peer{name: launch, image: img}
		}
		missing(t, "peer %s in %s lacks element %s", launch, img, lacks)
		return Peer{}
	}
	missing(t, "peer %s with elements %v not found, and %s names no usable image", launch, elements, EnvPeerImage)
	return Peer{}
}

// FFmpegSRT returns the host's ffmpeg when it reads srt:// (libsrt), which
// makes it an SRT receiver independent of gosrt. The peer image carries no
// ffmpeg: when the host's has no SRT the test is skipped, or failed when
// OS_REQUIRE_PEERS=1.
func FFmpegSRT(t testing.TB) string {
	t.Helper()
	return Require(t, "ffmpeg", "srt")
}

func hostHas(t testing.TB, inspect string, elements []string) bool {
	for _, e := range elements {
		if exec.CommandContext(t.Context(), inspect, "--exists", e).Run() != nil { //nolint:gosec // test peers
			return false
		}
	}
	return true
}

var (
	imageMu    sync.Mutex
	imageOK    = map[string]bool{}
	imageElems = map[string]bool{}
)

// peerImage returns OS_PEER_IMAGE when docker has that image, else "".
func peerImage(t testing.TB) string {
	img := os.Getenv(EnvPeerImage)
	if img == "" {
		return ""
	}
	imageMu.Lock()
	defer imageMu.Unlock()
	ok, known := imageOK[img]
	if !known {
		ok = exec.CommandContext(t.Context(), dockerBin, "image", "inspect", img).Run() == nil //nolint:gosec // test peers
		imageOK[img] = ok
	}
	if !ok {
		return ""
	}
	return img
}

// imageLacks returns the first of elements that the image lacks, or "".
func imageLacks(t testing.TB, img string, elements []string) string {
	imageMu.Lock()
	defer imageMu.Unlock()
	for _, e := range elements {
		key := img + "\x00" + e
		has, known := imageElems[key]
		if !known {
			name := "osv2-vlc-" + strings.ToLower(rand.Text()[:12])
			has = exec.CommandContext(t.Context(), dockerBin, //nolint:gosec // test peers
				dockerArgs(name, img, nil, []string{"gst-inspect-1.0", "--exists", e})...).Run() == nil
			imageElems[key] = has
		}
		if !has {
			return e
		}
	}
	return ""
}
