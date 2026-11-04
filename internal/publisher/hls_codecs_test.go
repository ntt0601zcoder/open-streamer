package publisher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/tsmux"
)

// h264KeyFrame builds an Annex-B access unit: SPS (profile/compat/level given),
// PPS and an IDR slice.
func h264KeyFrame(profile, compat, level byte) []byte {
	return []byte{
		0, 0, 0, 1, 0x67, profile, compat, level, 0xac, 0xd9, 0x40,
		0, 0, 0, 1, 0x68, 0xee, 0x3c, 0x80,
		0, 0, 0, 1, 0x65, 0x88, 0x84, 0x00, 0x20,
	}
}

func h264DeltaFrame() []byte {
	return []byte{0, 0, 0, 1, 0x41, 0x9a, 0x00, 0x10, 0x20}
}

// masterCodecsFor feeds a rendition's video through a segmenter wired to an
// ABR master, flushes one segment and returns the master playlist text.
func masterCodecsFor(t *testing.T, width, height int, keyframe []byte, viaTS bool) string {
	t.Helper()
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "index.m3u8")
	master := newHLSABRMaster(rootPath, "test")

	p := &hlsSegmenter{
		streamID:  "test",
		streamDir: dir,
		segSec:    4,
		window:    6,
		abrMaster: master,
		abrSlug:   "track_1",
		bwBps:     1_000_000,
		width:     width,
		height:    height,
	}

	ctx := context.Background()
	var avMux *tsmux.FromAV
	var tsCarry []byte
	// One muxer for the whole stream so PAT/PMT precede the video (TS path).
	mux := tsmux.NewFromAV(ctx)
	feed := func(av *domain.AVPacket) {
		if !viaTS {
			p.handleIncoming(ctx, buffer.Packet{AV: av}, 4*time.Second, &avMux, &tsCarry)
			return
		}
		var ts []byte
		mux.Write(av, func(b []byte) { ts = append(ts, b...) })
		p.handleIncoming(ctx, buffer.Packet{TS: ts}, 4*time.Second, &avMux, &tsCarry)
	}
	feed(&domain.AVPacket{Codec: domain.AVCodecH264, Data: keyframe, KeyFrame: true})
	feed(&domain.AVPacket{Codec: domain.AVCodecH264, Data: h264DeltaFrame(), PTSms: 40, DTSms: 40})
	feed(&domain.AVPacket{Codec: domain.AVCodecH264, Data: h264DeltaFrame(), PTSms: 80, DTSms: 80})

	p.mu.Lock()
	p.flushLocked()
	p.mu.Unlock()

	master.mu.Lock()
	if master.debounce != nil {
		master.debounce.Stop()
	}
	master.mu.Unlock()
	master.flushRoot()

	b, err := os.ReadFile(rootPath)
	require.NoError(t, err)
	return string(b)
}

// The master playlist CODECS must reflect the SPS the encoder really emitted,
// not a guess from the rendition size.
func TestHLSMasterCodecsFromSPS(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		w, h          int
		profile, comp byte
		level         byte
		want          string
	}{
		{"main_1080p", 1920, 1080, 0x4d, 0x40, 0x28, `CODECS="avc1.4d4028"`},
		{"main_480p", 854, 480, 0x4d, 0x40, 0x1e, `CODECS="avc1.4d401e"`},
		{"main_720p", 1280, 720, 0x4d, 0x40, 0x1f, `CODECS="avc1.4d401f"`},
		{"high_1080p", 1920, 1080, 0x64, 0x00, 0x28, `CODECS="avc1.640028"`},
		{"baseline_1080p", 1920, 1080, 0x42, 0xc0, 0x1f, `CODECS="avc1.42c01f"`},
		{"high_480p", 854, 480, 0x64, 0x00, 0x1f, `CODECS="avc1.64001f"`},
	}
	for _, tc := range cases {
		for _, viaTS := range []bool{false, true} {
			name := tc.name + "_av"
			if viaTS {
				name = tc.name + "_ts"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				got := masterCodecsFor(t, tc.w, tc.h, h264KeyFrame(tc.profile, tc.comp, tc.level), viaTS)
				require.Contains(t, got, tc.want)
			})
		}
	}
}
