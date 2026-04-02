package publisher

import (
	"bytes"
	"context"
	"os/exec"
	"sync"
	"testing"

	"github.com/ntthuan060102github/open-streamer/config"
	"github.com/ntthuan060102github/open-streamer/internal/buffer"
	"github.com/ntthuan060102github/open-streamer/internal/domain"
	"github.com/ntthuan060102github/open-streamer/internal/ingestor"
)

func requireFFmpegTools(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not in PATH")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not in PATH")
	}
}

// generateTestMPEGTS writes a short H.264+AAC MPEG-TS file (frequent IDR) via ffmpeg.
func generateTestMPEGTS(t *testing.T, path string) {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=25",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", "90",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-g", "25", "-keyint_min", "25",
		"-c:a", "aac", "-ar", "48000", "-ac", "2",
		"-f", "mpegts", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
}

// feedMPEGTSFileLoopIngest starts a goroutine that demuxes a looping file:// MPEG-TS source into AVPackets on the buffer until ctx is done.
func feedMPEGTSFileLoopIngest(ctx context.Context, t *testing.T, wg *sync.WaitGroup, svc *buffer.Service, code domain.StreamCode, tsPath string) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		input := domain.Input{URL: "file://" + tsPath + "?loop=true"}
		pr, rerr := ingestor.NewPacketReader(input, config.IngestorConfig{})
		if rerr != nil {
			t.Error(rerr)
			return
		}
		if rerr := pr.Open(ctx); rerr != nil {
			t.Error(rerr)
			return
		}
		defer pr.Close()
		for ctx.Err() == nil {
			batch, rerr := pr.ReadPackets(ctx)
			if rerr != nil {
				return
			}
			for i := range batch {
				cp := batch[i].Clone()
				if werr := svc.Write(code, buffer.Packet{AV: cp}); werr != nil {
					t.Error(werr)
					return
				}
			}
		}
	}()
}

// firstTSegmentURIInM3U8 returns the first non-comment line containing ".ts" (HLS media URI).
func firstTSegmentURIInM3U8(body []byte) string {
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		if bytes.Contains(line, []byte(".ts")) {
			return string(line)
		}
	}
	return ""
}
