package publisher

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ntthuan060102github/open-streamer/internal/buffer"
	"github.com/ntthuan060102github/open-streamer/internal/domain"
	"github.com/ntthuan060102github/open-streamer/internal/mediaserve"
	"github.com/stretchr/testify/require"
)

// TestDASHFMP4FromMPEGTSFile serves output through the same HTTP routes as the API (/{code}/index.mpd and /{code}/*).
func TestDASHFMP4FromMPEGTSFile(t *testing.T) {
	requireFFmpegTools(t)

	tsPath := filepath.Join(t.TempDir(), "live.ts")
	generateTestMPEGTS(t, tsPath)

	hlsRoot := t.TempDir()
	dashRoot := t.TempDir()
	streamDir := filepath.Join(dashRoot, "test")
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
		runDASHFMP4Packager(ctx, code, sub, streamDir, filepath.Join(streamDir, "index.mpd"),
			2, 8, 0, true, nil)
	}()

	ts := httptest.NewServer(mediaserve.NewHandler(hlsRoot, dashRoot))
	defer ts.Close()

	client := &http.Client{Timeout: 30 * time.Second}
	base := ts.URL + "/test"

	var mpdBody []byte
	require.Eventually(t, func() bool {
		resp, err := client.Get(base + "/index.mpd")
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
		if !strings.Contains(ct, "dash") && !strings.Contains(ct, "xml") {
			return false
		}
		if !bytes.Contains(b, []byte("<MPD")) || !bytes.Contains(b, []byte("SegmentTemplate")) {
			return false
		}
		mpdBody = b
		return true
	}, 45*time.Second, 150*time.Millisecond, "GET /{code}/index.mpd should return a live MPD")

	initResp, err := client.Get(base + "/init_v.mp4")
	require.NoError(t, err)
	defer initResp.Body.Close()
	require.Equal(t, http.StatusOK, initResp.StatusCode)
	initBody, err := io.ReadAll(initResp.Body)
	require.NoError(t, err)
	require.Greater(t, len(initBody), 100, "init_v.mp4 over HTTP should have payload")

	startNum := dashVideoStartNumberFromMPD(mpdBody)
	require.Greater(t, startNum, 0, "MPD should carry a video startNumber")

	segPath := base + "/seg_v_" + formatSeg5(startNum) + ".m4s"
	require.Eventually(t, func() bool {
		resp, gerr := client.Get(segPath)
		if gerr != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				_ = resp.Body.Close()
			}
			return false
		}
		b, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return rerr == nil && len(b) > 100
	}, 20*time.Second, 150*time.Millisecond, "GET /{code}/seg_v_*.m4s should succeed (same route as DASH clients)")

	cancel()
	wg.Wait()
}

func dashVideoStartNumberFromMPD(mpd []byte) int {
	// First startNumber after video/mp4 (video adaptation set comes before audio in our MPD).
	idx := bytes.Index(mpd, []byte("video/mp4"))
	if idx < 0 {
		return 0
	}
	tail := mpd[idx:]
	const prefix = `startNumber="`
	p := bytes.Index(tail, []byte(prefix))
	if p < 0 {
		return 0
	}
	p += len(prefix)
	end := bytes.IndexByte(tail[p:], '"')
	if end < 0 {
		return 0
	}
	n, err := strconv.Atoi(string(tail[p : p+end]))
	if err != nil {
		return 0
	}
	return n
}

func formatSeg5(n int) string { return fmt.Sprintf("%05d", n) }
