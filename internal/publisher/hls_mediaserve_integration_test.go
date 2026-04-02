package publisher

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ntthuan060102github/open-streamer/internal/buffer"
	"github.com/ntthuan060102github/open-streamer/internal/domain"
	"github.com/ntthuan060102github/open-streamer/internal/mediaserve"
	"github.com/stretchr/testify/require"
)

// TestHLSNativeSegmentsFromMPEGTSFile serves output through the same HTTP routes as the API (/{code}/index.m3u8 and /{code}/*).
func TestHLSNativeSegmentsFromMPEGTSFile(t *testing.T) {
	requireFFmpegTools(t)

	tsPath := filepath.Join(t.TempDir(), "live.ts")
	generateTestMPEGTS(t, tsPath)

	hlsRoot := t.TempDir()
	dashRoot := t.TempDir()
	streamDir := filepath.Join(hlsRoot, "test")
	require.NoError(t, os.MkdirAll(streamDir, 0o755))

	svc := buffer.NewServiceForTesting(16_000)
	const code = domain.StreamCode("test")
	svc.Create(code)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	sub, err := svc.Subscribe(code)
	require.NoError(t, err)
	defer svc.Unsubscribe(code, sub)

	var wg sync.WaitGroup
	feedMPEGTSFileLoopIngest(ctx, t, &wg, svc, code, tsPath)

	wg.Add(1)
	go func() {
		defer wg.Done()
		runFilesystemSegmenter(ctx, code, sub, segmenterOpts{
			streamDir:   streamDir,
			hlsPlaylist: filepath.Join(streamDir, "index.m3u8"),
			segmentSec:  2,
			window:      8,
			history:     0,
			ephemeral:   true,
		})
	}()

	ts := httptest.NewServer(mediaserve.NewHandler(hlsRoot, dashRoot))
	defer ts.Close()

	client := &http.Client{Timeout: 30 * time.Second}
	base := ts.URL + "/test"

	var m3u8Body []byte
	require.Eventually(t, func() bool {
		resp, err := client.Get(base + "/index.m3u8")
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				_ = resp.Body.Close()
			}
			return false
		}
		b, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if rerr != nil {
			return false
		}
		ct := resp.Header.Get("Content-Type")
		if !strings.Contains(ct, "mpegurl") && !strings.Contains(ct, "m3u8") {
			return false
		}
		if !bytes.Contains(b, []byte("#EXTINF")) || !bytes.Contains(b, []byte(".ts")) {
			return false
		}
		m3u8Body = b
		return true
	}, 45*time.Second, 150*time.Millisecond, "GET /{code}/index.m3u8 should return a live media playlist")

	segName := firstTSegmentURIInM3U8(m3u8Body)
	require.NotEmpty(t, segName, "m3u8 should list a .ts segment URI")

	segURL := base + "/" + segName
	var segData []byte
	require.Eventually(t, func() bool {
		resp, err := client.Get(segURL)
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				_ = resp.Body.Close()
			}
			return false
		}
		b, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if rerr != nil || len(b) < 188 {
			return false
		}
		segData = b
		return true
	}, 15*time.Second, 100*time.Millisecond, "GET /{code}/<segment>.ts should succeed (same route as players)")

	tmpTS := filepath.Join(t.TempDir(), "seg.ts")
	require.NoError(t, os.WriteFile(tmpTS, segData, 0o644))
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type",
		"-of", "csv=p=0", tmpTS).Output()
	require.NoError(t, err)
	require.Contains(t, string(out), "video")

	cancel()
	wg.Wait()
}
