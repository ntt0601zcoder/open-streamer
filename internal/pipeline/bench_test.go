//go:build unix

package pipeline

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/ingest"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

const (
	benchStreams  = 10
	benchMediaSec = 20 // media seconds each stream reads per iteration
)

// processCPU returns the CPU time (user + system) the process has used.
func processCPU(tb testing.TB) time.Duration {
	tb.Helper()
	var ru syscall.Rusage
	require.NoError(tb, syscall.Getrusage(syscall.RUSAGE_SELF, &ru))
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// clipMbps returns the bitrate of the fixture in Mbit/s, from its video DTS
// span.
func clipMbps(tb testing.TB, b []byte) float64 {
	tb.Helper()
	r, err := ingest.NewTSReader(io.NopCloser(bytes.NewReader(b)), nil)
	require.NoError(tb, err)
	defer r.Close()
	var first, last int64
	n := 0
	for {
		f, err := r.Read()
		if err != nil {
			break
		}
		if r.Kinds()[f.Track] != media.KindVideo {
			continue
		}
		if n == 0 {
			first = f.DTS
		}
		last = f.DTS
		n++
	}
	require.Greater(tb, n, 1)
	sec := media.ToDuration(last-first).Seconds() * float64(n) / float64(n-1)
	return float64(len(b)) * 8 / sec / 1e6
}

// BenchmarkIngest10Streams runs 10 pipelines over a looping, unpaced file of
// h264_aac.ts (not live) into hubs with one draining subscriber each, each
// until it has delivered benchMediaSec media seconds. It measures demux, the
// Clock and the hub only. Besides time per iteration it reports frames/s,
// the cores used (CPU time over wall time) and cpu_s/media_s/Mbps: process CPU
// seconds per media second per Mbps of the clip, over all streams.
func BenchmarkIngest10Streams(b *testing.B) {
	clip := fixture(b, "h264_aac.ts")
	mbps := clipMbps(b, clip)
	u := loopURL(b, clip, "loop=1&pace=0")

	var frames, mediaTicks atomic.Int64
	cpu0 := processCPU(b)
	b.ResetTimer()
	for range b.N {
		var wg sync.WaitGroup
		for range benchStreams {
			h := hub.New(hub.Options{})
			sub := h.Subscribe("bench", 1<<14)
			ctx, cancel := context.WithCancel(context.Background())
			p := New(h, Options{Inputs: []string{u}})
			wg.Add(2)
			go func() {
				defer wg.Done()
				if err := p.Run(ctx); !errors.Is(err, context.Canceled) {
					b.Errorf("Run: %v", err)
				}
				h.Close()
			}()
			go func() {
				defer wg.Done()
				defer cancel()
				var first, last int64
				seen := false
				for d := range sub.C() {
					frames.Add(1)
					if d.Frame.Track != 0 {
						continue // layout track 0 is the video
					}
					if !seen {
						first, seen = d.Frame.DTS, true
					}
					last = d.Frame.DTS
					if last-first >= benchMediaSec*media.ClockRate {
						mediaTicks.Add(last - first)
						return
					}
				}
			}()
		}
		wg.Wait()
	}
	b.StopTimer()
	if got := mediaTicks.Load(); got < int64(b.N*benchStreams*benchMediaSec*media.ClockRate) {
		b.Fatalf("read %d media ticks, want at least %d", got, b.N*benchStreams*benchMediaSec*media.ClockRate)
	}
	cpu := (processCPU(b) - cpu0).Seconds()
	wall := b.Elapsed().Seconds()
	mediaSec := float64(mediaTicks.Load()) / media.ClockRate
	b.ReportMetric(float64(frames.Load())/wall, "frames/s")
	b.ReportMetric(cpu/wall, "cores")
	b.ReportMetric(cpu/mediaSec/mbps, "cpu_s/media_s/Mbps")
	b.ReportMetric(mbps, "clip_Mbps")
}
