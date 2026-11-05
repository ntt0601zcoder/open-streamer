package publisher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/tsmux"
)

// firstPESPTSms returns the PTS (ms) of the first video (0xE0) and first audio
// (0xC0) PES found in a TS segment. -1 means the stream is absent.
func firstPESPTSms(ts []byte) (video, audio int64) {
	video, audio = -1, -1
	for off := 0; off+188 <= len(ts); off += 188 {
		pkt := ts[off : off+188]
		if pkt[0] != 0x47 || pkt[1]&0x40 == 0 {
			continue
		}
		i := 4
		if pkt[3]&0x20 != 0 {
			i += 1 + int(pkt[4])
		}
		if i+14 > len(pkt) || pkt[i] != 0 || pkt[i+1] != 0 || pkt[i+2] != 1 {
			continue
		}
		if pkt[i+7]&0x80 == 0 {
			continue
		}
		b := pkt[i+9 : i+14]
		pts := int64(b[0]>>1&0x07)<<30 | int64(b[1])<<22 | int64(b[2]>>1)<<15 | int64(b[3])<<7 | int64(b[4]>>1)
		switch pkt[i+3] {
		case 0xE0:
			if video < 0 {
				video = pts / 90
			}
		case 0xC0:
			if audio < 0 {
				audio = pts / 90
			}
		}
	}
	return video, audio
}

// Audio that reaches the segmenter ahead of the video it belongs with must
// not end up in the previous segment: a new segment has to start with audio
// and video within one audio frame of each other.
func TestHLSSegmenter_AudioAheadOfVideoAtCut(t *testing.T) {
	t.Parallel()
	for _, leadMs := range []uint64{0, 200, 400, 800} {
		t.Run(fmt.Sprintf("lead_%dms", leadMs), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			p := &hlsSegmenter{
				streamID:     "test",
				streamDir:    dir,
				manifestPath: filepath.Join(dir, "index.m3u8"),
				segSec:       2,
				window:       5,
			}
			type arrival struct {
				key float64
				av  *domain.AVPacket
			}
			var in []arrival
			for pts := uint64(10000); pts <= 14500; pts += 40 {
				v := &domain.AVPacket{
					Codec: domain.AVCodecH264, Data: h264DeltaFrame(),
					PTSms: pts, DTSms: pts,
				}
				if (pts-10000)%2000 == 0 {
					v.Data, v.KeyFrame = h264KeyFrame(0x4d, 0x40, 0x28), true
				}
				in = append(in, arrival{float64(pts), v})
			}
			for pts := uint64(10000); pts <= 14500; pts += 24 {
				a := &domain.AVPacket{
					Codec: domain.AVCodecAAC, PTSms: pts, DTSms: pts,
					Data: []byte{0xFF, 0xF1, 0x4C, 0x80, 0x01, 0x1F, 0xFC, 0x21, 0x00, 0x00},
				}
				in = append(in, arrival{float64(pts) - float64(leadMs) + 0.5, a})
			}
			sort.SliceStable(in, func(i, j int) bool { return in[i].key < in[j].key })

			ctx := context.Background()
			var avMux *tsmux.FromAV
			var tsCarry []byte
			for _, a := range in {
				p.handleIncoming(ctx, buffer.Packet{AV: a.av}, 2*time.Second, &avMux, &tsCarry)
			}

			b, err := os.ReadFile(filepath.Join(dir, "seg_000002.ts"))
			require.NoError(t, err)
			v, a := firstPESPTSms(b)
			require.Equal(t, int64(12000), v)
			require.GreaterOrEqual(t, a, int64(0))
			diff := a - v
			if diff < 0 {
				diff = -diff
			}
			require.Less(t, diff, int64(24), "first audio PTS %d vs first video PTS %d", a, v)
		})
	}
}
