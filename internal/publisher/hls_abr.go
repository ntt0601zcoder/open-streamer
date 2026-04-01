package publisher

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/ntthuan060102github/open-streamer/internal/buffer"
	"github.com/ntthuan060102github/open-streamer/internal/domain"
)

func (s *Service) serveHLSAdaptive(ctx context.Context, stream *domain.Stream) {
	code := stream.Code
	rends := buffer.RenditionsForTranscoder(code, stream.Transcoder)
	if len(rends) == 0 {
		s.serveHLS(ctx, code)
		return
	}

	streamDir := filepath.Join(s.cfg.HLS.Dir, string(code))
	if err := resetOutputDir(streamDir); err != nil {
		slog.Error("publisher: HLS ABR setup dir failed", "stream_code", code, "dir", streamDir, "err", err)
		return
	}

	masterPath := filepath.Join(streamDir, "index.m3u8")
	if err := writeHLSMasterPlaylist(masterPath, rends); err != nil {
		slog.Error("publisher: HLS ABR master playlist failed", "stream_code", code, "err", err)
	}

	var wg sync.WaitGroup
	for _, r := range rends {
		wg.Add(1)
		go func(r buffer.RenditionPlayout) {
			defer wg.Done()
			sub, err := s.buf.Subscribe(r.BufferID)
			if err != nil {
				slog.Error("publisher: HLS ABR subscribe failed", "stream_code", code, "slug", r.Slug, "err", err)
				return
			}
			defer s.buf.Unsubscribe(r.BufferID, sub)

			rd := filepath.Join(streamDir, r.Slug)
			if err := os.MkdirAll(rd, 0o755); err != nil {
				slog.Error("publisher: HLS ABR mkdir failed", "stream_code", code, "slug", r.Slug, "err", err)
				return
			}
			playlist := filepath.Join(rd, "index.m3u8")
			runFilesystemSegmenter(ctx, code, sub, segmenterOpts{
				streamDir:   rd,
				hlsPlaylist: playlist,
				segmentSec:  s.cfg.HLS.LiveSegmentSec,
				window:      s.cfg.HLS.LiveWindow,
				history:     s.cfg.HLS.LiveHistory,
				ephemeral:   s.cfg.HLS.LiveEphemeral,
			})
		}(r)
	}
	wg.Wait()
}

func writeHLSMasterPlaylist(path string, rends []buffer.RenditionPlayout) error {
	if len(rends) == 0 {
		return nil
	}
	ordered := append([]buffer.RenditionPlayout(nil), rends...)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].BandwidthBps() > ordered[j].BandwidthBps()
	})

	var b []byte
	b = append(b, "#EXTM3U\n#EXT-X-VERSION:3\n"...)
	for _, r := range ordered {
		res := ""
		if r.Width > 0 && r.Height > 0 {
			res = fmt.Sprintf(",RESOLUTION=%dx%d", r.Width, r.Height)
		}
		b = append(b, fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d%s\n%s/index.m3u8\n",
			r.BandwidthBps(), res, r.Slug)...)
	}
	return writeFileAtomic(path, b)
}
