package publisher

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/grafov/m3u8"
	"github.com/ntthuan060102github/open-streamer/internal/buffer"
	"github.com/ntthuan060102github/open-streamer/internal/domain"
	"github.com/ntthuan060102github/open-streamer/internal/tsmux"
)

// segmenterOpts configures native MPEG-TS segment files + optional HLS playlist.
type segmenterOpts struct {
	streamDir   string
	hlsPlaylist string // empty: skip m3u8
	segmentSec  int
	window      int
	history     int
	ephemeral   bool
	// currentFailoverGen returns the stream's input-failover generation (see publisher.Service).
	// When it increases, the next flushed segment gets EXT-X-DISCONTINUITY (per variant, ABR-safe).
	currentFailoverGen func() uint64
}

func runFilesystemSegmenter(ctx context.Context, streamID domain.StreamCode, sub *buffer.Subscriber, o segmenterOpts) {
	segSec := o.segmentSec
	if segSec <= 0 {
		segSec = 2
	}
	win := o.window
	if win <= 0 {
		win = 12
	}
	hist := o.history
	if hist < 0 {
		hist = 0
	}
	capacity := uint(win + hist + 8)
	wsize := uint(win)
	var playlist *m3u8.MediaPlaylist
	if o.hlsPlaylist != "" {
		var perr error
		playlist, perr = m3u8.NewMediaPlaylist(wsize, capacity)
		if perr != nil {
			slog.Error("publisher: m3u8 playlist init failed", "stream_code", streamID, "err", perr)
			return
		}
		// v6: #EXT-X-PROGRAM-DATE-TIME may include fractional seconds (RFC3339).
		playlist.SetVersion(6)
	}

	var segBuf bytes.Buffer
	var tsRemainder []byte
	var avMux *tsmux.FromAV
	segIndex := 1
	segmentStart := time.Now()
	onDisk := make([]string, 0, win+hist+4)
	segDur := float64(segSec)

	// IDR gate: discard packets until the first video IDR (keyframe) arrives so that
	// every HLS segment begins at a clean access-unit boundary. Without this gate,
	// RTMP streams that connect mid-GOP produce a first segment with no IDR in it;
	// hlsMpegTSSegmentCut then returns 0 and the segmenter stalls for up to
	// forceAfterSec (10–30 s) before making a forced 188-byte-aligned cut.
	// Raw-TS packets (push / transcoder path) bypass the gate because IDR detection
	// requires full TS parsing — those sources typically start at a keyframe anyway.
	// The deadline prevents audio-only or IDR-marker-less streams from freezing.
	var gotFirstIDR bool
	gateDeadline := time.Now().Add(time.Duration(segSec*4) * time.Second)

	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	feedChunk := func(chunk []byte) {
		if len(chunk) == 0 {
			return
		}
		data := chunk
		if len(tsRemainder) > 0 {
			merged := make([]byte, 0, len(tsRemainder)+len(chunk))
			merged = append(merged, tsRemainder...)
			merged = append(merged, chunk...)
			data = merged
			tsRemainder = tsRemainder[:0]
		}
		fullLen := (len(data) / 188) * 188
		if fullLen > 0 {
			_, _ = segBuf.Write(data[:fullLen])
		}
		if fullLen < len(data) {
			tsRemainder = append(tsRemainder[:0], data[fullLen:]...)
		}
	}

	var lastAppliedFailoverGen uint64
	// Wall-clock for the first segment line in the playlist (one PDT at list head, not per EXTINF).
	var firstSegPDT time.Time
	var haveFirstSegPDT bool
	segStep := time.Duration(segSec) * time.Second

	// flush writes one HLS segment. When force is false, we cut on a video boundary (IDR+RA if
	// present, else min-byte span between video PUSI; see hlsMpegTSSegmentCut) after live_segment_sec.
	// If still not possible after a long wall-clock wait (see forceAfterSec), we use a 188-byte-aligned
	// cut. force=true uses that last resort immediately for shutdown / subscriber close.
	flush := func(force bool) error {
		buf := segBuf.Bytes()
		if len(buf) == 0 {
			segmentStart = time.Now()
			return nil
		}

		cut := hlsMpegTSSegmentCut(buf)
		if cut <= 0 {
			// Wait longer than 2*segSec so long-GOP sources get a second IDR/NAL boundary before we
			// fall back to an arbitrary 188-byte edge (causes macroblocking / "blurry" playback).
			forceAfterSec := 4 * segSec
			if forceAfterSec < 10 {
				forceAfterSec = 10
			}
			if forceAfterSec > 30 {
				forceAfterSec = 30
			}
			switch {
			case !force && time.Since(segmentStart) >= time.Duration(forceAfterSec)*time.Second:
				cut = hlsMpegTSForceCutLen(buf)
				if cut > 0 {
					hlsLogForcedSegmentCut(streamID, segIndex, "no PES boundary in window (timed fallback)")
				}
			case force:
				cut = hlsMpegTSForceCutLen(buf)
				if cut > 0 {
					hlsLogForcedSegmentCut(streamID, segIndex, "shutdown or subscriber closed")
				}
			}
		}
		if cut <= 0 {
			return nil
		}

		toWrite := buf[:cut]
		name := fmt.Sprintf("seg_%06d.ts", segIndex)
		path := filepath.Join(o.streamDir, name)
		if err := writeFileAtomic(path, toWrite); err != nil {
			return err
		}
		segIndex++
		segBuf.Reset()
		_, _ = segBuf.Write(buf[cut:])
		segmentStart = time.Now()

		onDisk = append(onDisk, name)
		maxKeep := win + hist
		if maxKeep < win {
			maxKeep = win
		}
		for o.ephemeral && len(onDisk) > maxKeep {
			old := onDisk[0]
			onDisk = onDisk[1:]
			_ = os.Remove(filepath.Join(o.streamDir, old))
		}

		if o.hlsPlaylist != "" && playlist != nil {
			wasFull := playlist.Count() >= wsize
			playlist.Slide(name, segDur, "")
			if o.currentFailoverGen != nil {
				if g := o.currentFailoverGen(); g > lastAppliedFailoverGen {
					lastAppliedFailoverGen = g
					if err := playlist.SetDiscontinuity(); err != nil {
						slog.Warn("publisher: HLS EXT-X-DISCONTINUITY failed", "stream_code", streamID, "err", err)
					} else {
						slog.Info("publisher: HLS EXT-X-DISCONTINUITY after input failover",
							"stream_code", streamID, "segment", name, "failover_gen", g)
					}
				}
			}
			switch {
			case !haveFirstSegPDT:
				firstSegPDT = time.Now().UTC()
				haveFirstSegPDT = true
			case wasFull:
				firstSegPDT = firstSegPDT.Add(segStep)
			}
			setProgramDateTimeOnFirstSegmentOnly(playlist, firstSegPDT)
			if err := writeFileAtomic(o.hlsPlaylist, playlist.Encode().Bytes()); err != nil {
				return err
			}
		}
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			_ = flush(true)
			return
		case <-tick.C:
			if !gotFirstIDR && time.Now().After(gateDeadline) {
				slog.Warn("publisher: HLS IDR gate deadline exceeded — opening segmenter without keyframe",
					"stream_code", streamID,
					"hint", "audio-only stream, or encoder not sending IDR markers; segments may not seek cleanly")
				gotFirstIDR = true
				segmentStart = time.Now()
			}
			if gotFirstIDR && time.Since(segmentStart) >= time.Duration(segSec)*time.Second {
				if err := flush(false); err != nil {
					slog.Warn("publisher: segment flush failed", "stream_code", streamID, "err", err)
				}
			}
		case pkt, ok := <-sub.Recv():
			if !ok {
				_ = flush(true)
				return
			}
			if !gotFirstIDR {
				switch {
				case len(pkt.TS) > 0:
					// Raw-TS path (push mode or transcoder): let through immediately;
					// hlsMpegTSSegmentCut will locate the first IDR in the TS bytes.
					gotFirstIDR = true
					segmentStart = time.Now()
				case pkt.AV != nil:
					isVideoIDR := pkt.AV.KeyFrame &&
						(pkt.AV.Codec == domain.AVCodecH264 || pkt.AV.Codec == domain.AVCodecH265)
					if !isVideoIDR {
						continue // discard pre-IDR audio and non-IDR video
					}
					gotFirstIDR = true
					segmentStart = time.Now()
					slog.Debug("publisher: HLS IDR gate opened — first keyframe received",
						"stream_code", streamID)
				default:
					continue
				}
			}
			tsmux.FeedWirePacket(pkt.TS, pkt.AV, &avMux, feedChunk)
			// Do not rely on tick alone: when the subscriber channel stays ready, select can
			// favor Recv repeatedly and flush deadlines slip — HLS EXTINF stays fixed while media
			// piles up and players stutter badly.
			if time.Since(segmentStart) >= time.Duration(segSec)*time.Second {
				if err := flush(false); err != nil {
					slog.Warn("publisher: segment flush failed", "stream_code", streamID, "err", err)
				}
			}
		}
	}
}

// setProgramDateTimeOnFirstSegmentOnly assigns EXT-X-PROGRAM-DATE-TIME only to the first segment
// in playlist order (appears once after the header tags, before the first EXTINF).
func setProgramDateTimeOnFirstSegmentOnly(playlist *m3u8.MediaPlaylist, t time.Time) {
	segs := playlist.GetAllSegments()
	for _, seg := range segs {
		if seg == nil {
			continue
		}
		seg.ProgramDateTime = time.Time{}
	}
	if len(segs) > 0 && segs[0] != nil {
		segs[0].ProgramDateTime = t
	}
	playlist.ResetCache()
}

func windowTail(names []string, n int) []string {
	if n <= 0 || len(names) == 0 {
		return nil
	}
	start := len(names) - n
	if start < 0 {
		start = 0
	}
	return append([]string(nil), names[start:]...)
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".seg-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}
