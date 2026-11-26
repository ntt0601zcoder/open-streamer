package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/conform"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/server"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
	"github.com/ntt0601zcoder/open-streamer/internal/stream"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
)

// The M5b2 scenarios prove the audio language on the whole server: the
// declared language is what conform and ffprobe read from the masters and
// the MPD, an edit to it is applied under the readers without ending
// anything (R-L5), the ABR TS master's URI-less audio entry (R-L4) is read
// by our own HLS pull, and real players play that master over a live
// sliding window as they did without the entry.

// m5b2Probe runs ffprobe on u and returns the language tag of every audio
// stream it lists ("" for a stream without one), in its order, and its
// stderr.
func m5b2Probe(t *testing.T, ffprobe, u string) (langs []string, stderr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffprobe, "-v", "error", "-show_entries", "stream=codec_type:stream_tags=language",
		"-of", "json", u)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	out, err := cmd.Output()
	require.NoError(t, err, "ffprobe %s: %s", u, errb.String())
	var v struct {
		Streams []struct {
			Type string `json:"codec_type"`
			Tags struct {
				Language string `json:"language"`
			} `json:"tags"`
		} `json:"streams"`
	}
	require.NoError(t, json.Unmarshal(out, &v), "ffprobe %s: %s", u, out)
	for _, s := range v.Streams {
		if s.Type == "audio" {
			langs = append(langs, s.Tags.Language)
		}
	}
	return langs, errb.String()
}

// m5b2ProbeIs requires that ffprobe lists audio on u and tags every audio
// stream with lang.
func m5b2ProbeIs(t *testing.T, ffprobe, u, lang string) {
	t.Helper()
	langs, stderr := m5b2Probe(t, ffprobe, u)
	t.Logf("ffprobe %s: audio languages %q; stderr %q", u, langs, stderr)
	require.NotEmpty(t, langs, "ffprobe %s: audio streams", u)
	for i, l := range langs {
		assert.Equal(t, lang, l, "ffprobe %s: the language tag of audio stream %d of %d", u, i, len(langs))
	}
}

// m5b2Read reads the window of a master or an MPD once, expecting lang on
// its audio ("" expects nothing), and returns the capture.
func m5b2Read(ctx context.Context, u, lang string) (conform.Capture, error) {
	opt := conform.ReadOptions{Once: true, Timeout: m5aRead, AlignedSequences: true, AudioLanguage: lang}
	if len(u) > 4 && u[len(u)-4:] == ".mpd" {
		return conform.ReadDASH(ctx, u, opt)
	}
	return conform.ReadHLS(ctx, u, opt)
}

// m5b2Reads requires that a read of u expecting lang has video and audio,
// no violation, and exactly the languages want.
func m5b2Reads(t *testing.T, u, lang string, want []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runLimit)
	defer cancel()
	c, err := m5b2Read(ctx, u, lang)
	m5aAV(t, u, c, err)
	assert.Equal(t, want, c.AudioLanguages, "%s: the audio languages read", u)
}

// TestM5b2AudioLanguage is the M5b2 exit criterion on a passthrough stream:
// cam, with HLS, DASH and HTTP-TS on and audio_language "vi". conform reads
// "vi" from the CMAF master and from the MPD without a violation, and
// ffprobe tags the audio stream of each with "vi". Then, under an HTTP-TS
// reader, a playlist poller and the samplers of both packagers, the
// language is edited to "en" and then removed: each answer is "live" with
// the generation unchanged, the first master and MPD fetched after each
// answer carry the new value (after the removal, no LANGUAGE and no lang),
// no session ended, and the reader holds every hub frame once.
func TestM5b2AudioLanguage(t *testing.T) {
	t.Parallel()
	ffmpeg := testpeer.Require(t, "ffmpeg")
	ffprobe := testpeer.Require(t, "ffprobe")
	m := m5aServer(t, "")
	cam := spec.Stream{
		Name:          "cam",
		Inputs:        []spec.Input{{URL: m5b1Clip(t, ffmpeg)}},
		Outputs:       spec.Outputs{HLS: true, DASH: true, HTTPTS: true},
		AudioLanguage: "vi",
	}
	m5b1Create(t, m, cam)
	m.running(t, "cam")
	m.listed(t, "cam", 2)
	master, mpd := m.live("cam")+"cmaf/master.m3u8", m.url(stream.OutDASH, "cam")

	for _, u := range []string{master, mpd} {
		m5b2Reads(t, u, "vi", []string{"vi"})
		m5b2ProbeIs(t, ffprobe, u, "vi")
	}
	// HLS-TS without a ladder is a media playlist: no master, no language
	// (R-L4), and conform says so when one is expected (R-L6 (d)).
	assert.NotContains(t, string(m.mediaGet(t, m.url(stream.OutHLS, "cam")).body), "LANGUAGE", "hls/index.m3u8, a media playlist")

	// No segmented reader, as in TestM5b1ReplaceInputs: the source is one
	// pass of a clip, not a loop. The playlist poller and the two samplers
	// hold HLS and DASH instead.
	r := m5aOpen(t, m, "cam", m5aRowOpt{sessions: []stream.Output{stream.OutHTTPTS}, segmented: []string{}, watch: stream.OutHLS})
	dash := m.watchPackager(stream.OutDASH, "cam")
	t.Cleanup(dash.end)
	playlist := m5b1WatchPlaylist(m.url(stream.OutHLS, "cam"))
	t.Cleanup(playlist.end)
	before := m.status(t, "cam")
	endedBefore := m5b1Ended(t, m)
	unchanged := func(what string) {
		t.Helper()
		st := m.status(t, "cam")
		assert.Equal(t, before.Generation, st.Generation, "%s: the generation", what)
		assert.True(t, before.Since.Equal(st.Since), "%s: since: %v, then %v", what, before.Since, st.Since)
		assert.Zero(t, st.Rebuilds+st.Restarts, "%s: rebuilds and restarts", what)
		assert.Zero(t, st.Switches, "%s: input_switches", what)
	}
	bodies := func() (string, string) {
		t.Helper()
		a, b := m.mediaGet(t, master), m.mediaGet(t, mpd)
		require.Equal(t, 200, a.code, master)
		require.Equal(t, 200, b.code, mpd)
		return string(a.body), string(b.body)
	}
	time.Sleep(2 * time.Second)

	// "vi" to "en": the first master and MPD fetched after the answer carry
	// it (R-L5), and within 5 s both reads report it alone.
	cam.AudioLanguage = "en"
	m5b1Put(t, m, cam, "live")
	r.edited()
	edited := time.Now()
	hlsText, dashText := bodies()
	assert.Contains(t, hlsText, `LANGUAGE="en"`, "the master fetched after the answer")
	assert.NotContains(t, hlsText, `LANGUAGE="vi"`, "the master fetched after the answer")
	assert.Contains(t, dashText, `lang="en"`, "the MPD fetched after the answer")
	assert.NotContains(t, dashText, `lang="vi"`, "the MPD fetched after the answer")
	unchanged("after \"en\"")
	for _, u := range []string{master, mpd} {
		m5b2Reads(t, u, "en", []string{"en"})
	}
	assert.Less(t, time.Since(edited), 5*time.Second, "both reads report \"en\" within 5 s of the answer")
	assert.Contains(t, m.fileText(t), "audio_language: en", "the config file holds the language")
	time.Sleep(2 * time.Second)

	// "en" to none.
	cam.AudioLanguage = ""
	m5b1Put(t, m, cam, "live")
	r.edited()
	hlsText, dashText = bodies()
	assert.NotContains(t, hlsText, "LANGUAGE", "the master fetched after the removal")
	assert.NotContains(t, dashText, "lang=", "the MPD fetched after the removal")
	unchanged("after the removal")
	for _, u := range []string{master, mpd} {
		m5b2Reads(t, u, "", nil)
	}
	assert.NotContains(t, m.fileText(t), "audio_language", "the config file holds no language")

	// The media sequence goes on once the window is full.
	m5aUntil(t, m5aWait, playlist.slid, "the HLS window slides")
	playlist.end()
	dash.end()
	// Read last, just before the row ends the stream: it covers the whole
	// run.
	assert.Equal(t, endedBefore, m5b1Ended(t, m), "no openstreamer_output_sessions_ended_total series moved")
	r.end(t)
	assertUninterrupted(t, r)
	t.Run("dash packager", dash.check)
	playlist.check(t, 0)
	assert.Len(t, epochs(r.hubs[0]), 1, "the hub's epochs from the tap's start to the stream's end: no edit changed the source")
}

// TestM5b2AudioLanguageLadder is R-L4's new form under the real child: cam
// reads a clip through a ladder of two x264 rungs with the audio copied,
// HLS and DASH on, audio_language "vi". hls/index.m3u8, the ABR TS master,
// has exactly one audio entry, without a URI, with LANGUAGE="vi", and
// AUDIO="aud" on both variants; conform reads it with "vi" expected and
// without a violation; ffprobe tags the audio of each variant with "vi";
// and a second stream of the same server, pull, takes that master as its
// input and has video and audio on its hub within 15 s.
//
// It runs in the arm64 job too: its only peers are ffmpeg and ffprobe.
func TestM5b2AudioLanguageLadder(t *testing.T) {
	t.Parallel()
	bin := testpeer.OSTranscoder(t)
	ffmpeg := testpeer.Require(t, "ffmpeg")
	ffprobe := testpeer.Require(t, "ffprobe")
	m, _ := m5b1Server(t, bin, false, server.Options{})
	cam := m5b1Ladder(m5b1Clip(t, ffmpeg), 180, 120)
	cam.Outputs = spec.Outputs{HLS: true, DASH: true}
	cam.AudioLanguage = "vi"
	m5b1Create(t, m, cam)
	m.running(t, "cam")
	m.listed(t, "cam", 2)
	master := m.url(stream.OutHLS, "cam")

	text := string(m.mediaGet(t, master).body)
	pl := multivariant(t, master)
	require.Len(t, pl.Renditions, 1, "the renditions of the ABR TS master:\n%s", text)
	audio := pl.Renditions[0]
	assert.Equal(t, "AUDIO", audio.Type)
	assert.Equal(t, "aud", audio.GroupID)
	assert.Equal(t, "audio", audio.Name)
	assert.Equal(t, "vi", audio.Language)
	assert.Empty(t, audio.URI, "the audio is muxed into the variants: no URI")
	assert.NotContains(t, text, "URI=", "the ABR TS master:\n%s", text)
	require.Len(t, pl.Variants, 2, "the variants of the ABR TS master:\n%s", text)
	for _, v := range pl.Variants {
		assert.Equal(t, "aud", v.Audio, "AUDIO of the variant %s", v.URI)
	}

	m5b2Reads(t, master, "vi", []string{"vi"})
	langs, stderr := m5b2Probe(t, ffprobe, master)
	t.Logf("ffprobe %s: audio languages %q; stderr %q", master, langs, stderr)
	assert.Equal(t, []string{"vi", "vi"}, langs, "ffprobe: the audio of each of the two variants")
	// The other two carry it as well (R-L4).
	m5b2Reads(t, m.live("cam")+"cmaf/master.m3u8", "vi", []string{"vi"})
	m5b2Reads(t, m.url(stream.OutDASH, "cam"), "vi", []string{"vi"})

	// Our own pull of that master (Review Focus 2).
	m5b1Create(t, m, spec.Stream{Name: "pull", Inputs: []spec.Input{{URL: master}}, Outputs: spec.Outputs{HTTPTS: true}})
	tap := m.tap(t, "pull")
	var video, sound int
	m5aUntil(t, 15*time.Second, func() bool {
		frames := m5b1Frames(tap)
		v, _ := byKind(frames, media.KindVideo)
		a, _ := byKind(frames, media.KindAudio)
		video, sound = len(v), len(a)
		return video > 0 && sound > 0
	}, "pull's hub has video and audio; it has %d video and %d audio frames", &video, &sound)
	t.Logf("pull: %d video and %d audio frames; status %+v", video, sound, m.status(t, "pull").Inputs)
	m.del(t, "pull")
	m.del(t, "cam")
}

// TestM5b2Players is Review Focus 1: the ABR presentation of h264_aac.ts
// (TestM4bPlayers' replay), with the language "vi" set on the packager
// before any playlist is asked for, is served over a live sliding window,
// each row on its own replay:
//
//   - ffprobe on hls/index.m3u8, whose audio entry has no URI, tags the
//     audio of every variant with "vi", with nothing on stderr;
//   - GStreamer plays hls/index.m3u8 with the GStreamer pass rule;
//   - VLC plays hls/index.m3u8 and cmaf/master.m3u8 with the VLC pass rule,
//     video and audio decoded, one after the other.
//
// It needs the transcoder bundle and the players, so it is not in the
// arm64 job's list.
func TestM5b2Players(t *testing.T) {
	t.Parallel()
	bin := testpeer.OSTranscoder(t)
	ffprobe := testpeer.Require(t, "ffprobe")
	steps := m4bRuns(t, bin, "h264_aac.ts")["h264_aac.ts"].presentation
	// replay starts a replay with the language set and returns its base
	// URL once n segments are listed. It checks what the players are given.
	replay := func(t *testing.T, n int) string {
		t.Helper()
		p, base := liveABR(t, steps, m4bPlayerPasses)
		p.SetAudioLanguage("vi")
		waitListed(t, p, n)
		text := string(get(t, base+"hls/index.m3u8"))
		require.Contains(t, text, `#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="audio",LANGUAGE="vi",DEFAULT=YES,AUTOSELECT=YES`+"\n",
			"hls/index.m3u8")
		require.NotContains(t, text, "URI=", "hls/index.m3u8")
		require.Contains(t, string(get(t, base+"cmaf/master.m3u8")), `LANGUAGE="vi"`, "cmaf/master.m3u8")
		return base
	}

	t.Run("ffprobe/hls-ts", func(t *testing.T) {
		t.Parallel()
		base := replay(t, 3)
		langs, stderr := m5b2Probe(t, ffprobe, base+"hls/index.m3u8")
		t.Logf("hls/index.m3u8: audio languages %q; stderr %q", langs, stderr)
		require.NotEmpty(t, langs, "audio streams")
		for i, l := range langs {
			assert.Equal(t, "vi", l, "the language tag of audio stream %d of %d", i, len(langs))
		}
		assert.Empty(t, stderr, "ffprobe stderr")
	})
	t.Run("gstreamer/hls-ts", func(t *testing.T) {
		t.Parallel()
		gst := testpeer.GStreamer(t, "uridecodebin3", "hlsdemux2", "tsdemux", "avdec_h264", "avdec_aac", "queue", "fakesink")
		// Five segments, as TestM4bPlayers' GStreamer rows.
		base := replay(t, 5)
		gstPlays(t, gst, hlsPipeline(base+"hls/index.m3u8"))
	})
	t.Run("vlc", func(t *testing.T) {
		t.Parallel()
		for _, row := range []struct {
			name, path string
			modules    []string
		}{
			{"hls-ts", "hls/index.m3u8", []string{"adaptive", "ts", "avcodec"}},
			{"hls-fmp4", "cmaf/master.m3u8", []string{"adaptive", "mp4", "avcodec"}},
		} {
			t.Run(row.name, func(t *testing.T) {
				vlc := testpeer.VLC(t, row.modules...)
				// A window of five segments, as TestM4bPlayers' VLC rows.
				base := replay(t, 5)
				vlcPlays(t, vlc, ffprobe, base+row.path, true)
			})
		}
	})
}
