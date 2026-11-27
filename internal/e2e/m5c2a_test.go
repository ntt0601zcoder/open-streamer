package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	srt "github.com/datarhei/gosrt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/api"
	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
	"github.com/ntt0601zcoder/open-streamer/internal/stream"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
)

// The M5c2a scenarios prove the milestone's exit criterion over the whole
// server (M5a's harness): ffmpeg 6.1 and 8 and our own client play an SRT
// output that requires a passphrase, and a caller without it or with a
// wrong one is refused; no secret appears in any response to any role, in
// a log or in a metric; an editor's read, change and write of a stream and
// of the whole config keeps every stored secret without ever being shown
// one. The scenarios are passthrough.

const (
	// m5c2aPass is the SRT passphrase of the scenarios' locked streams, and
	// m5c2aNewPass the one TestM5c2aSRTFFmpeg changes it to.
	m5c2aPass    = "outpass-0123456789"
	m5c2aNewPass = "outpass-9876543210"
	// m5c2aWrongPass is a passphrase no stream has.
	m5c2aWrongPass = "wrongpass-0123456789"
	// m5c2aPushKey is the stream key of the scenarios' pushes, and
	// m5c2aRTSPPW the password of their RTSP inputs.
	m5c2aPushKey = "pushkey-0123456789"
	m5c2aRTSPPW  = "hunter2-pw"
	// m5c2aSpan is the media an ffmpeg player of TestM5c2aSRTFFmpeg
	// decodes.
	m5c2aSpan = 5 * time.Second
)

// m5c2aMaskText is what a 422 says of a mask that could not be resolved
// (spec.MaskRefusal).
const m5c2aMaskText = "holds the redaction mask"

// m5c2aDebugServer is m5aServer without streams, logging at debug level:
// a leak scan must read every line the server can write.
func m5c2aDebugServer(t *testing.T) *m5a {
	t.Helper()
	text := m5aConfig("", "")
	require.Contains(t, text, "log: {level: info,")
	path := filepath.Join(t.TempDir(), "openstreamer.yaml")
	require.NoError(t, os.WriteFile(path, []byte(strings.Replace(text, "log: {level: info,", "log: {level: debug,", 1)), 0o600))
	return m5aStart(t, path)
}

// m5c2aPut writes sp over the API and returns the whole answer.
func (m *m5a) m5c2aPut(t *testing.T, sp spec.Stream) m5aResp {
	t.Helper()
	body, err := json.Marshal(sp)
	require.NoError(t, err)
	return m.do(t, http.MethodPut, "/api/v1/streams/"+sp.Name, body, "Content-Type", "application/json")
}

// m5c2aSecrets are every literal the scans look for: the M5a stream
// secrets, the SRT output's passphrase, and the credentials embedded in a
// path, a query and a stream ID (R-T2).
var m5c2aSecrets = []string{
	"hunter2-pw", "pushkey-0123456789", "listenkey-0123456789", "srtpass-0123456789", "outpass-0123456789",
	"tlJwpbo6", "hunter2-q", "abc123tok", "pw12345",
}

// m5c2aBudget fails the test when it took longer than d.
func m5c2aBudget(t *testing.T, start time.Time, d time.Duration) {
	t.Helper()
	took := time.Since(start)
	t.Logf("wall time %v of a %v budget", took.Round(time.Millisecond), d)
	assert.LessOrEqual(t, took, d, "the scenario's budget")
}

// m5c2aRun is one ffmpeg or ffprobe run that has ended.
type m5c2aRun struct {
	stdout, stderr string
	err            error
	took           time.Duration
}

// m5c2aExec runs bin with args in the background until it exits or ctx
// ends.
func m5c2aExec(ctx context.Context, bin string, args ...string) <-chan m5c2aRun {
	ch := make(chan m5c2aRun, 1)
	go func() {
		start := time.Now()
		cmd := exec.CommandContext(ctx, bin, args...)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		ch <- m5c2aRun{out.String(), errb.String(), err, time.Since(start)}
	}()
	return ch
}

// m5c2aWait waits for a run, for at most d.
func m5c2aWait(t *testing.T, what string, ch <-chan m5c2aRun, d time.Duration) m5c2aRun {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(d):
		require.Failf(t, "a peer did not exit", "%s within %v", what, d)
		return m5c2aRun{}
	}
}

// m5c2aFFmpeg is ffmpeg's argument list that reads in, with the input
// options before it, writes -progress to stdout and logs at info, so that
// its closing line counts what each kind of output received.
func m5c2aFFmpeg(in string, inOpts []string, out ...string) []string {
	args := []string{"-hide_banner", "-nostdin", "-v", "info", "-progress", "pipe:1"}
	args = append(args, inOpts...)
	return append(append(args, "-i", in), out...)
}

// m5c2aProgress is the last value of key in ffmpeg's -progress output, and
// whether there was one.
func m5c2aProgress(out, key string) (int64, bool) {
	var n int64
	ok := false
	for line := range strings.Lines(out) {
		if v, found := strings.CutPrefix(strings.TrimSpace(line), key+"="); found {
			if k, err := strconv.ParseInt(v, 10, 64); err == nil {
				n, ok = k, true
			}
		}
	}
	return n, ok
}

// m5c2aSizes matches ffmpeg's closing line of an output, which says how
// much video and audio it received ("video:123KiB audio:45KiB", or kB in
// ffmpeg 6.1).
var m5c2aSizes = regexp.MustCompile(`video:\s*(\d+)\S*\s+audio:\s*(\d+)`)

// m5c2aDecoded requires a run of ffmpeg ... -t <m5c2aSpan> -f null - that
// decoded m5c2aSpan of video and audio: it exited cleanly, its progress
// reports that much output and at least 80 % of its video frames (the
// fixture is 25 fps), its closing line counts video and audio, and its log
// has no "corrupt".
func m5c2aDecoded(t *testing.T, what string, r m5c2aRun) {
	t.Helper()
	const span = m5c2aSpan
	require.NoError(t, r.err, "%s: stderr %s", what, r.stderr)
	frames, ok := m5c2aProgress(r.stdout, "frame")
	require.True(t, ok, "%s: progress %q", what, r.stdout)
	assert.GreaterOrEqual(t, float64(frames), 0.8*25*span.Seconds(), "%s: video frames decoded", what)
	us, _ := m5c2aProgress(r.stdout, "out_time_us")
	assert.GreaterOrEqual(t, time.Duration(us)*time.Microsecond, span-500*time.Millisecond, "%s: output time", what)
	m := m5c2aSizes.FindStringSubmatch(r.stderr)
	if assert.NotNil(t, m, "%s: the closing line; stderr %s", what, r.stderr) {
		assert.NotEqual(t, "0", m[1], "%s: video received", what)
		assert.NotEqual(t, "0", m[2], "%s: audio received", what)
	}
	assert.NotContains(t, strings.ToLower(r.stderr), "corrupt", what)
	t.Logf("%s: %d frames, %v of output, %s, in %v", what, frames, time.Duration(us)*time.Microsecond, m, r.took.Round(time.Millisecond))
}

// m5c2aRefusedRun requires a run of ffmpeg that was refused before it read
// a frame: it failed, its log holds one of marks, its progress counts no
// frame, and the output file it was given does not exist or is empty.
func m5c2aRefusedRun(t *testing.T, what string, r m5c2aRun, file string, marks ...string) {
	t.Helper()
	require.Error(t, r.err, "%s: ffmpeg is refused", what)
	assert.True(t, slices.ContainsFunc(marks, func(m string) bool { return strings.Contains(r.stderr, m) }),
		"%s: the log names the refusal (%q); stderr %s", what, marks, r.stderr)
	frames, _ := m5c2aProgress(r.stdout, "frame")
	assert.Zero(t, frames, "%s: frames decoded", what)
	if fi, err := os.Stat(file); err == nil {
		assert.Zero(t, fi.Size(), "%s: ffmpeg wrote output", what)
	} else {
		assert.ErrorIs(t, err, os.ErrNotExist, what)
	}
	t.Logf("%s: refused in %v: %v; %s", what, r.took.Round(time.Millisecond), r.err, m5c2aLine(r.stderr, marks...))
}

// m5c2aLine is the first line of a log that holds one of marks.
func m5c2aLine(log string, marks ...string) string {
	for line := range strings.Lines(log) {
		if slices.ContainsFunc(marks, func(m string) bool { return strings.Contains(line, m) }) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// m5c2aLast is the last n lines of a log.
func m5c2aLast(log string, n int) string {
	lines := strings.Split(strings.TrimSpace(log), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], " | ")
}

// m5c2aSRTURL is the SRT URL of a served name with a passphrase, "" for
// none.
func (m *m5a) m5c2aSRTURL(served, pass string) string {
	u := m.url(stream.OutSRT, served)
	if pass != "" {
		u += "&passphrase=" + pass
	}
	return u
}

// m5c2aRejected is the SRT output's refusals of a reason.
func (m *m5a) m5c2aRejected(t *testing.T, reason string) float64 {
	t.Helper()
	return m.metric(t, "openstreamer_output_rejected_total", "output", "srt", "reason", reason)
}

// m5c2aCreate is a stream PUT that must create the stream.
func (m *m5a) m5c2aCreate(t *testing.T, sp spec.Stream) {
	t.Helper()
	code, applied := m.put(t, sp)
	require.Equal(t, http.StatusCreated, code, "PUT %s", sp.Name)
	require.Equal(t, "created", applied, "PUT %s", sp.Name)
}

// TestM5c2aSRTFFmpeg is the exit criterion's SRT half with ffmpeg alone
// (it runs in the arm64 job too). cam is a passthrough stream with SRT and
// HTTP-TS on and srt_passphrase m5c2aPass; open has SRT on and none.
//   - ffprobe with the passphrase lists h264 and aac; ffmpeg -f null
//     decodes 5 s of both without "corrupt" in its log, and so it does with
//     -pbkeylen 32; conform.ReadTS (our own client) reads it without a
//     violation; open plays without a passphrase, and ffmpeg with one is
//     refused, as at HEAD.
//   - Without the passphrase ffmpeg fails with UNSECURE ("Password
//     required"), with a wrong one with BADSECRET ("Incorrect passphrase");
//     neither decodes a frame or writes output, and the secret refusals
//     move by at least 2 (a caller that retransmits its request is counted
//     again). The sessions gauge is not asserted: other rows hold
//     sessions.
//   - While ffmpeg plays cam, a PUT changes the passphrase: that ffmpeg
//     ends within 5 s and a new one with the new passphrase plays; cam's
//     rebuilds did not move and its HTTP-TS reader was not interrupted.
//
// Budget 90 s.
func TestM5c2aSRTFFmpeg(t *testing.T) {
	t.Parallel()
	start := time.Now()
	defer m5c2aBudget(t, start, 90*time.Second)
	ffmpeg := testpeer.Require(t, "ffmpeg", "srt")
	ffprobe := testpeer.Require(t, "ffprobe", "srt")
	if major, ok := testpeer.FFmpegMajor(t, ffmpeg); ok {
		t.Logf("ffmpeg %d", major)
	}
	m := m5aServer(t, "")
	src := m5aSource(t)
	cam := spec.Stream{
		Name:    "cam",
		Inputs:  []spec.Input{{URL: src}},
		Outputs: spec.Outputs{HTTPTS: true, SRT: true, SRTPassphrase: m5c2aPass},
	}
	m.m5c2aCreate(t, cam)
	m.m5c2aCreate(t, spec.Stream{Name: "open", Inputs: []spec.Input{{URL: src}}, Outputs: spec.Outputs{SRT: true}})
	m.running(t, "cam")
	m.running(t, "open")
	dir := t.TempDir()
	// Every peer of the test runs under ctx, which outlives the budget, so
	// that a slow run fails on its budget and not on a killed peer.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	locked := m.m5c2aSRTURL("cam", m5c2aPass)

	// What plays, side by side.
	probe := func(u string) <-chan m5c2aRun {
		return m5c2aExec(ctx, ffprobe, "-v", "error", "-show_entries", "stream=codec_type,codec_name", "-of", "json", u)
	}
	decode := func(u string, inOpts ...string) <-chan m5c2aRun {
		return m5c2aExec(ctx, ffmpeg, m5c2aFFmpeg(u, inOpts, "-t", strconv.Itoa(int(m5c2aSpan.Seconds())), "-f", "null", "-")...)
	}
	refusedAt := func(u, file string) <-chan m5c2aRun {
		return m5c2aExec(ctx, ffmpeg, m5c2aFFmpeg(u, nil, "-t", "5", "-c", "copy", "-f", "mpegts", file)...)
	}
	probeCam, probeOpen := probe(locked), probe(m.m5c2aSRTURL("open", ""))
	decCam, decCam32 := decode(locked), decode(locked, "-pbkeylen", "32")
	decOpen := decode(m.m5c2aSRTURL("open", ""))
	modeBefore := m.m5c2aRejected(t, "mode")
	openLocked := refusedAt(m.m5c2aSRTURL("open", m5c2aPass), filepath.Join(dir, "open-pass.ts"))
	own := make(chan capResult, 1)
	go func() {
		c, err := conform.ReadTS(ctx, locked, conform.ReadOptions{Duration: 3 * time.Second, Timeout: m5aRead})
		own <- capResult{c, err}
	}()
	for _, p := range []struct {
		what string
		ch   <-chan m5c2aRun
	}{{"ffprobe cam with the passphrase", probeCam}, {"ffprobe open", probeOpen}} {
		r := m5c2aWait(t, p.what, p.ch, time.Minute)
		require.NoError(t, r.err, "%s: %s", p.what, r.stderr)
		var v struct {
			Streams []struct {
				Type  string `json:"codec_type"`
				Codec string `json:"codec_name"`
			} `json:"streams"`
		}
		require.NoError(t, json.Unmarshal([]byte(r.stdout), &v), p.what)
		got := make([]string, 0, len(v.Streams))
		for _, s := range v.Streams {
			got = append(got, s.Type+":"+s.Codec)
		}
		assert.ElementsMatch(t, []string{"video:h264", "audio:aac"}, got, p.what)
	}
	m5c2aDecoded(t, "ffmpeg cam with the passphrase", m5c2aWait(t, "ffmpeg cam", decCam, time.Minute))
	m5c2aDecoded(t, "ffmpeg cam with -pbkeylen 32", m5c2aWait(t, "ffmpeg cam -pbkeylen 32", decCam32, time.Minute))
	m5c2aDecoded(t, "ffmpeg open without a passphrase", m5c2aWait(t, "ffmpeg open", decOpen, time.Minute))
	r := m5c2aWait(t, "ffmpeg open with a passphrase", openLocked, time.Minute)
	require.Error(t, r.err, "ffmpeg with a passphrase is refused by open")
	assert.NoFileExists(t, filepath.Join(dir, "open-pass.ts"))
	assert.GreaterOrEqual(t, m.m5c2aRejected(t, "mode")-modeBefore, 1.0, "an encrypted request to open is refused by mode, as at HEAD")
	t.Logf("open with a passphrase: %v; %s", r.err, m5c2aLast(r.stderr, 3))
	c := m5aWaitCap(t, "conform.ReadTS of cam", own)
	m5aAV(t, "conform.ReadTS of cam with the passphrase", c.c, c.err)

	// The refusals.
	before := m.m5c2aRejected(t, "secret")
	noPass, wrong := filepath.Join(dir, "no-pass.ts"), filepath.Join(dir, "wrong.ts")
	noPassRun := refusedAt(m.m5c2aSRTURL("cam", ""), noPass)
	wrongRun := refusedAt(m.m5c2aSRTURL("cam", m5c2aWrongPass), wrong)
	m5c2aRefusedRun(t, "no passphrase", m5c2aWait(t, "ffmpeg without a passphrase", noPassRun, time.Minute), noPass,
		"UNSECURE", "Password required")
	m5c2aRefusedRun(t, "a wrong passphrase", m5c2aWait(t, "ffmpeg with a wrong passphrase", wrongRun, time.Minute), wrong,
		"BADSECRET", "Incorrect passphrase")
	after := m.m5c2aRejected(t, "secret")
	assert.GreaterOrEqual(t, after-before, 2.0, "the secret refusals")
	t.Logf("secret refusals %v -> %v, limited %v", before, after, m.m5c2aRejected(t, "limited"))

	// The passphrase changed under a player.
	row := m5aOpen(t, m, "cam", m5aRowOpt{sessions: []stream.Output{stream.OutHTTPTS}, segmented: []string{}})
	playing := m5c2aExec(ctx, ffmpeg, m5c2aFFmpeg(locked, nil, "-f", "null", "-")...)
	m5aUntil(t, m5aWait, func() bool { return m.metric(t, "openstreamer_output_sessions", "output", "srt") == 1 },
		"ffmpeg plays cam")
	time.Sleep(time.Second)
	edited := cam
	edited.Outputs.SRTPassphrase = m5c2aNewPass
	answer := m.m5c2aPut(t, edited)
	row.edited()
	require.Equal(t, http.StatusOK, answer.code, "%s", answer.body)
	var outcome stream.Outcome
	require.NoError(t, json.Unmarshal(answer.body, &outcome), "%s", answer.body)
	assert.Equal(t, "outputs", outcome.Applied)
	// The answer to the write that set it does not show it.
	assert.NotContains(t, string(answer.body), m5c2aNewPass)
	assert.NotContains(t, string(answer.body), m5c2aPass)
	r = m5c2aWait(t, "the player admitted under the old passphrase", playing, 5*time.Second)
	t.Logf("the old player ended %v after the edit: %v; %s", time.Since(row.edits[0]).Round(time.Millisecond), r.err,
		m5c2aLast(r.stderr, 2))
	frames, _ := m5c2aProgress(r.stdout, "frame")
	assert.Positive(t, frames, "the old player decoded before the edit")
	m5c2aDecoded(t, "ffmpeg cam with the new passphrase",
		m5c2aWait(t, "ffmpeg with the new passphrase", decode(m.m5c2aSRTURL("cam", m5c2aNewPass)), time.Minute))
	row.settled(t)
	row.end(t)
	assertUninterrupted(t, row)
	assert.Zero(t, row.last["rebuilds"], "cam's rebuilds")

	// No passphrase of this scenario is in a log line: the two the stream
	// had, and the wrong one a player sent.
	logs := m.logs.String()
	require.NotEmpty(t, logs)
	for _, p := range []string{m5c2aPass, m5c2aNewPass, m5c2aWrongPass} {
		assert.NotContains(t, logs, p, "the log")
	}
}

// m5c2aDial is one gosrt caller of a stream ID with a passphrase ("" for
// none); it returns how its request was answered.
func m5c2aDial(addr, streamID, pass string) error {
	cfg := srt.DefaultConfig()
	cfg.StreamId = streamID
	cfg.Passphrase = pass
	cfg.ConnectionTimeout = 5 * time.Second
	c, err := srt.Dial("srt", addr, cfg)
	if err != nil {
		return err //nolint:wrapcheck // the caller reports it
	}
	return c.Close() //nolint:wrapcheck // the caller reports it
}

// TestM5c2aWriteOnly is Review Focus 1 and 2 over the real API, as the
// editor. cam has an RTSP input with a password (nothing listens), the M5a
// source as its second input, an RTMP push with a key (to sink, on the
// server's own RTMP listener) and the SRT passphrase.
//   - GET shows the masks to the editor and the viewer alike; GET
//     ?secrets=1 and ?secrets=1&x=1 are 400 for both.
//   - The editor GETs cam, sets audio_language and PUTs the body with its
//     ETag: 200 "live"; the file holds the real secrets; cam's active
//     input, generation and switches are what they were, and its HTTP-TS
//     reader is not interrupted.
//   - The editor GETs /api/v1/config (no secret in it), appends a stream
//     to the text and PUTs it: the file is the old file plus the entry,
//     byte for byte.
//   - A PUT of cam with the masked input's host changed to a listener of
//     the test is 422, and the listener sees no connection. A PUT of the
//     masked body under a new name, the body's name changed to it, is 422.
//   - The config ETag the viewer reads in /api/v1/config/status is not the
//     SHA-256 of the file.
//   - Two streams pushing to the same URL show different push ids.
//
// Budget 60 s.
func TestM5c2aWriteOnly(t *testing.T) {
	t.Parallel()
	start := time.Now()
	defer m5c2aBudget(t, start, 60*time.Second)
	m := m5aServer(t, "")
	eye := m.as(m5aViewer, m5aViewerPW)
	src := m5aSource(t)
	rtspIn := "rtsp://admin:" + m5c2aRTSPPW + "@127.0.0.1:1/x"
	push := "rtmp://" + m.addrs.RTMP + "/in/" + m5c2aPushKey
	m.m5c2aCreate(t, spec.Stream{Name: "sink", Inputs: []spec.Input{{URL: push + "?mode=listen"}}, Outputs: spec.Outputs{HTTPTS: true}})
	cam := spec.Stream{
		Name:   "cam",
		Inputs: []spec.Input{{URL: rtspIn}, {URL: src}},
		Outputs: spec.Outputs{
			HTTPTS: true, SRT: true, SRTPassphrase: m5c2aPass,
			Push: []spec.Push{{URL: push}},
		},
	}
	m.m5c2aCreate(t, cam)
	m5aUntil(t, m5aWait, func() bool { return m.status(t, "cam").ActiveInput == 1 }, "cam runs on its second input")
	secrets := []string{m5c2aRTSPPW, m5c2aPushKey, m5c2aPass}
	clean := func(what, text string) {
		t.Helper()
		for _, s := range secrets {
			assert.NotContains(t, text, s, what)
		}
	}

	// What both roles are shown.
	for _, who := range []*m5a{m, eye} {
		code, body := who.get(t, "/api/v1/streams/cam")
		require.Equal(t, http.StatusOK, code, "%s", body)
		clean("GET cam as "+who.user, string(body))
		var shown spec.Stream
		require.NoError(t, json.Unmarshal(body, &shown))
		assert.Equal(t, "rtsp://admin:xxxxx@127.0.0.1:1/x", shown.Inputs[0].URL, "as %s", who.user)
		assert.Equal(t, src, shown.Inputs[1].URL, "as %s", who.user)
		assert.Equal(t, "rtmp://"+m.addrs.RTMP+"/in/puxxxxx", shown.Outputs.Push[0].URL, "as %s", who.user)
		assert.Equal(t, "xxxxx", shown.Outputs.SRTPassphrase, "as %s", who.user)
		code, body = who.get(t, "/api/v1/streams")
		require.Equal(t, http.StatusOK, code)
		clean("GET streams as "+who.user, string(body))
		for _, q := range []string{"?secrets=1", "?secrets=1&x=1"} {
			code, body = who.get(t, "/api/v1/streams/cam"+q)
			assert.Equal(t, http.StatusBadRequest, code, "GET cam%s as %s: %s", q, who.user, body)
			clean("GET cam"+q+" as "+who.user, string(body))
		}
	}

	// The editor's read, change and write of cam.
	row := m5aOpen(t, m, "cam", m5aRowOpt{sessions: []stream.Output{stream.OutHTTPTS}, segmented: []string{}})
	before := m.status(t, "cam")
	got := m.do(t, http.MethodGet, "/api/v1/streams/cam", nil)
	require.Equal(t, http.StatusOK, got.code)
	etag := got.header.Get("ETag")
	require.NotEmpty(t, etag)
	var shown spec.Stream
	require.NoError(t, json.Unmarshal(got.body, &shown))
	shown.AudioLanguage = "en"
	body, err := json.Marshal(shown)
	require.NoError(t, err)
	resp := m.do(t, http.MethodPut, "/api/v1/streams/cam", body, "Content-Type", "application/json", "If-Match", etag)
	row.edited()
	require.Equal(t, http.StatusOK, resp.code, "%s", resp.body)
	var put struct {
		Stream  spec.Stream `json:"stream"`
		Applied string      `json:"applied"`
	}
	require.NoError(t, json.Unmarshal(resp.body, &put))
	assert.Equal(t, "live", put.Applied)
	clean("the PUT's answer", string(resp.body))
	assert.Equal(t, "xxxxx", put.Stream.Outputs.SRTPassphrase, "the answer shows the mask")
	assert.NotEqual(t, etag, resp.header.Get("ETag"), "a new ETag")
	file := m.fileText(t)
	for _, s := range []string{rtspIn, push, "srt_passphrase: " + m5c2aPass, "audio_language: en"} {
		assert.Contains(t, file, s, "the file holds the real values")
	}
	assert.NotContains(t, file, "xxxxx", "the file holds no mask")
	after := m.status(t, "cam")
	assert.Equal(t, before.ActiveInput, after.ActiveInput, "cam's active input")
	assert.Equal(t, before.Generation, after.Generation, "cam's generation")
	assert.Equal(t, before.Switches, after.Switches, "cam's input switches")

	// The config editor: read, append a stream, write.
	text, cfgTag := m.configText(t)
	clean("GET /api/v1/config", text)
	assert.Contains(t, text, "xxxxx")
	oldFile := m.fileText(t)
	require.True(t, strings.HasSuffix(oldFile, "\n"))
	entry := "  # extra: appended through the config editor.\n" +
		"  - name: extra\n" +
		"    inputs:\n" +
		"      - url: \"" + src + "\"\n" +
		"    outputs: {httpts: true}\n"
	code, res := m.putConfig(t, text+entry, cfgTag)
	row.edited()
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, res.Streams, stream.Outcome{Name: "cam", Applied: "unchanged"})
	assert.Contains(t, res.Streams, stream.Outcome{Name: "extra", Applied: "created"})
	assert.Equal(t, oldFile+entry, m.fileText(t), "the file is the old file plus the entry, byte for byte")

	// A mask pointed somewhere else.
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	var dialed atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			dialed.Add(1)
			_ = c.Close()
		}
	}()
	code, body = m.get(t, "/api/v1/streams/cam")
	require.Equal(t, http.StatusOK, code)
	var masked spec.Stream
	require.NoError(t, json.Unmarshal(body, &masked))
	moved := masked
	moved.Inputs = slices.Clone(masked.Inputs)
	moved.Inputs[0].URL = strings.Replace(moved.Inputs[0].URL, "127.0.0.1:1", ln.Addr().String(), 1)
	require.Contains(t, moved.Inputs[0].URL, "xxxxx")
	stored := m.fileText(t)
	refused := m.m5c2aPut(t, moved)
	assert.Equal(t, http.StatusUnprocessableEntity, refused.code, "the mask with another host")
	assert.Contains(t, string(refused.body), m5c2aMaskText, "the mask with another host")
	renamed := masked
	renamed.Name = "cam2"
	refused = m.m5c2aPut(t, renamed)
	assert.Equal(t, http.StatusUnprocessableEntity, refused.code, "the masked body under a new name")
	assert.Contains(t, string(refused.body), m5c2aMaskText, "the masked body under a new name")
	assert.Equal(t, stored, m.fileText(t), "nothing was stored")
	time.Sleep(time.Second)
	assert.Zero(t, dialed.Load(), "the listener saw no connection")

	// The config's ETag is keyed (R-T12).
	sum := sha256.Sum256([]byte(m.fileText(t)))
	hexSum := hex.EncodeToString(sum[:])
	cs := eye.configStatus(t)
	assert.NotEmpty(t, cs.ETag)
	assert.NotContains(t, cs.ETag, hexSum[:16], "the config ETag is not the SHA-256 of the file")
	_, nowTag := m.configText(t)
	assert.Equal(t, nowTag, cs.ETag)

	row.settled(t)
	row.end(t)
	assertUninterrupted(t, row)

	// Two streams pushing to the same URL: a push's id is tagged over the
	// stream's name too.
	same := "rtmp://127.0.0.1:1/live/" + m5c2aPushKey
	for _, name := range []string{"p1", "p2"} {
		m.m5c2aCreate(t, spec.Stream{Name: name, Inputs: []spec.Input{{URL: src}}, Outputs: spec.Outputs{Push: []spec.Push{{URL: same}}}})
	}
	var ids []string
	m5aUntil(t, m5aWait, func() bool {
		ids = ids[:0]
		for _, name := range []string{"p1", "p2"} {
			if p := m.status(t, name).Pushes; len(p) == 1 && p[0].ID != "" {
				ids = append(ids, p[0].ID)
			}
		}
		return len(ids) == 2
	}, "both pushes have an id")
	assert.NotEqual(t, ids[0], ids[1], "the push ids of two streams pushing to the same URL")
}

// TestM5c2aNoSecretsLeak is TestM5c1NoSecretsLeak's scan, extended to
// R-T2's embedded credentials and the SRT passphrase. Each stream's first
// input fails, so that its URL reaches errors and logs, and the M5a source
// is its second: an XMeye path (user=admin_password=…), a query
// (?user=&password=), a path token (/token=…/), a MediaMTX stream ID
// (publish:<path>:<user>:<pass>), and M5a's four forms (a URL password, a
// push key, an SRT input passphrase, an RTMP listen key). One stream has
// srt_passphrase m5c2aPass, and a caller with a wrong passphrase and one
// without are refused.
//
// Scanned for every literal of m5c2aSecrets: the log; /metrics; every GET
// route as the viewer and as the editor, GET /api/v1/config included, for
// every stream; and the answers to a PUT of a taken listen input, of a URL
// that cannot be an input and of an unresolvable mask. The editor's GET
// /api/v1/config holds no user password either, and does hold the users'
// hashes (R-T8: recorded, not hidden).
func TestM5c2aNoSecretsLeak(t *testing.T) {
	t.Parallel()
	start := time.Now()
	defer m5c2aBudget(t, start, 60*time.Second)
	m := m5c2aDebugServer(t)
	src := m5aSource(t)
	listenInput := "rtmp://" + m.addrs.RTMP + "/in/listenkey-0123456789?mode=listen"
	failing := []struct{ name, url string }{
		{"xmeye", "rtsp://127.0.0.1:1/user=admin_password=tlJwpbo6_channel=1_stream=0.sdp?real_stream"},
		{"query", "rtsp://127.0.0.1:1/x?user=admin&password=hunter2-q"},
		{"token", "http://127.0.0.1:1/live/token=abc123tok/index.m3u8"},
		{"mtx", "srt://127.0.0.1:1?streamid=publish:cam:user:pw12345"},
		{"userinfo", "rtsp://user:hunter2-pw@127.0.0.1:1/x"},
		{"srtin", "srt://127.0.0.1:1?passphrase=srtpass-0123456789"},
	}
	names := make([]string, 0, len(failing)+1)
	for _, f := range failing {
		sp := spec.Stream{Name: f.name, Inputs: []spec.Input{{URL: f.url}, {URL: src}}, Outputs: spec.Outputs{HTTPTS: true}}
		if f.name == "userinfo" {
			sp.Outputs.Push = []spec.Push{{URL: "rtmp://127.0.0.1:1/live/pushkey-0123456789"}}
			sp.Outputs.SRT, sp.Outputs.SRTPassphrase = true, m5c2aPass
		}
		m.m5c2aCreate(t, sp)
		names = append(names, f.name)
	}
	m.m5c2aCreate(t, spec.Stream{Name: "listen", Inputs: []spec.Input{{URL: listenInput}, {URL: src}}, Outputs: spec.Outputs{HTTPTS: true}})
	names = append(names, "listen")
	m5aUntil(t, m5aWait, func() bool {
		for _, f := range failing {
			st := m.status(t, f.name)
			if st.LastError == "" || st.ActiveInput != 1 {
				return false
			}
		}
		p := m.status(t, "userinfo").Pushes
		return len(p) == 1 && p[0].LastError != ""
	}, "every failing stream runs on its second input, with its failure as last error")
	// Each failure quotes its URL, redacted: the scans below look at texts
	// that carried the secrets.
	for _, f := range failing {
		e := m.status(t, f.name).LastError
		assert.Contains(t, e, "xxxxx", "%s's last error quotes its URL redacted", f.name)
		t.Logf("%s: %s", f.name, e)
	}

	// The SRT passphrase's refusals.
	before := m.m5c2aRejected(t, "secret")
	require.Error(t, m5c2aDial(m.addrs.SRT, "live/userinfo", ""), "a caller without the passphrase")
	require.Error(t, m5c2aDial(m.addrs.SRT, "live/userinfo", m5c2aWrongPass), "a caller with a wrong passphrase")
	assert.GreaterOrEqual(t, m.m5c2aRejected(t, "secret")-before, 2.0, "the secret refusals")
	require.NoError(t, m5c2aDial(m.addrs.SRT, "live/userinfo", m5c2aPass), "a caller with the passphrase")

	users := []string{m5aEditorPW, m5aViewerPW}
	hashes := []string{m5aHash(m5aEditorPW), m5aHash(m5aViewerPW)}
	all := slices.Concat(m5c2aSecrets, users, hashes)
	clean := func(what, text string, secrets []string) {
		t.Helper()
		for _, s := range secrets {
			assert.NotContains(t, text, s, what)
		}
	}
	dup, err := json.Marshal(spec.Stream{Name: "dup", Inputs: []spec.Input{{URL: listenInput}}, Outputs: spec.Outputs{HTTPTS: true}})
	require.NoError(t, err)
	bad, err := json.Marshal(spec.Stream{Name: "bad", Inputs: []spec.Input{{URL: "gopher://user:hunter2-pw@127.0.0.1:1/x?password=hunter2-q"}}})
	require.NoError(t, err)
	ghost, err := json.Marshal(spec.Stream{Name: "ghost", Inputs: []spec.Input{{URL: "rtsp://user:xxxxx@127.0.0.1:1/x"}}})
	require.NoError(t, err)
	eye := m.as(m5aViewer, m5aViewerPW)
	for _, who := range []*m5a{eye, m} {
		for _, rt := range api.Routes() {
			if rt.Method != http.MethodGet {
				continue
			}
			var paths []string
			switch {
			case strings.Contains(rt.Path, "/streams/{name}"):
				for _, name := range names {
					paths = append(paths, strings.ReplaceAll(rt.Path, "{name}", name))
				}
			case strings.Contains(rt.Path, "{name}"):
				paths = append(paths, strings.ReplaceAll(rt.Path, "{name}", "none.png"))
			case strings.HasSuffix(rt.Path, "/profile"), strings.HasSuffix(rt.Path, "/trace"):
				paths = append(paths, rt.Path+"?seconds=1")
			default:
				paths = append(paths, rt.Path)
			}
			for _, path := range paths {
				r := who.do(t, http.MethodGet, path, nil)
				if !strings.HasPrefix(path, "/debug/pprof/") {
					assert.Less(t, r.code, 500, "GET %s as %s: %s", path, who.user, r.body)
				}
				what := "GET " + path + " as " + who.user
				if path == "/api/v1/config" && who == m {
					clean(what, string(r.body), slices.Concat(m5c2aSecrets, users))
					for _, h := range hashes {
						assert.Contains(t, string(r.body), h, "the editor reads the users' hashes (R-T8)")
					}
					continue
				}
				clean(what, string(r.body), all)
			}
		}
		for _, c := range []struct {
			name string
			body []byte
			want int
		}{
			{"dup", dup, http.StatusConflict},
			{"bad", bad, http.StatusUnprocessableEntity},
			{"ghost", ghost, http.StatusUnprocessableEntity},
		} {
			r := who.do(t, http.MethodPut, "/api/v1/streams/"+c.name, c.body, "Content-Type", "application/json")
			want := c.want
			if who == eye {
				want = http.StatusForbidden
			}
			assert.Equal(t, want, r.code, "the PUT of %s as %s: %s", c.name, who.user, r.body)
			clean("the answer to the PUT of "+c.name+" as "+who.user, string(r.body), all)
			if c.name == "ghost" && who == m {
				assert.Contains(t, string(r.body), m5c2aMaskText, "the 422 says why")
			}
		}
	}

	logs := m.logs.String()
	require.NotEmpty(t, logs)
	clean("the log", logs, slices.Concat(all, []string{m5c2aWrongPass}))
	// The failures are in the log, redacted: the scan above looked at
	// lines that were written.
	assert.Contains(t, logs, "password=xxxxx")
	// The file holds every secret: it is what a restart reads (R-T7).
	file := m.fileText(t)
	for _, f := range failing {
		assert.Contains(t, file, f.url)
	}
	assert.Contains(t, file, m5c2aPass)
}

// TestM5c2aPlayers plays cam's SRT output, which requires m5c2aPass, with
// the VLC-class clients (the amd64 test job only: VLC and GStreamer). Both
// rows are † (R-T11): a peer that provably cannot do one is dropped and
// recorded as a known limit.
//   - GStreamer: srtsrc uri=<the URL without the passphrase>
//     passphrase=m5c2aPass ! tsdemux, by gstPlays' rule.
//   - VLC: the passphrase as an option and the stream ID in the URL, as the
//     M3b3 VLC row passes it, by vlcPlays' rule. VLC 3.0.20's access_srt
//     names its options --passphrase and --streamid ("vlc -p access_srt");
//     it refuses --srt-passphrase as an unknown option and exits. Playing
//     at all proves the passphrase was used: the output refuses an
//     unencrypted request.
func TestM5c2aPlayers(t *testing.T) {
	t.Parallel()
	m := m5aServer(t, "")
	m.m5c2aCreate(t, spec.Stream{
		Name: "cam", Inputs: []spec.Input{{URL: m5aSource(t)}},
		Outputs: spec.Outputs{SRT: true, SRTPassphrase: m5c2aPass},
	})
	m.running(t, "cam")
	u := m.m5c2aSRTURL("cam", "")
	t.Run("gstreamer", func(t *testing.T) {
		t.Parallel()
		gst := testpeer.GStreamer(t, "srtsrc", "tsdemux", "h264parse", "aacparse", "avdec_h264", "avdec_aac", "queue", "fakesink")
		gstPlays(t, gst, []string{
			"srtsrc", "uri=" + u, "passphrase=" + m5c2aPass,
			"!", "tsdemux", "name=d",
			"d.", "!", "video/x-h264", "!", "h264parse", "!", "avdec_h264", "!", "queue", "!", "fakesink", "name=v", "silent=false",
			"d.", "!", "audio/mpeg", "!", "aacparse", "!", "avdec_aac", "!", "queue", "!", "fakesink", "name=a", "silent=false",
		})
	})
	t.Run("vlc", func(t *testing.T) {
		t.Parallel()
		vlc := testpeer.VLC(t, "access_srt", "ts", "avcodec")
		ffprobe := testpeer.Require(t, "ffprobe")
		vlcPlaysWith(t, vlc, ffprobe, nil, []string{"--passphrase=" + m5c2aPass}, u, false)
	})
}
