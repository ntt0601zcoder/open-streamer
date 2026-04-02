package publisher

import (
	"log/slog"
	"sort"

	"github.com/ntthuan060102github/open-streamer/internal/domain"
	"github.com/yapingcat/gomedia/go-codec"
	gompeg2 "github.com/yapingcat/gomedia/go-mpeg2"
)

// minHlsIDRSpanBytes avoids cutting on every video frame: gomedia emits one PES per frame, each
// with PUSI=1 on the first TS packet. Only IDR frames get Random_access_indicator on that packet
// (see gomedia TSMuxer.writePES). Many passthrough sources (e.g. RTSP MPEG-TS) never set RA; we
// then fall back to "enough bytes after the first video PUSI in this buffer" using the same
// threshold so segments are not a few KB.
const minHlsIDRSpanBytes = 200 * gompeg2.TS_PAKCET_SIZE

// hlsMpegTSSegmentCut returns a byte offset into buf (188-byte aligned) so buf[:cut] ends before
// a safe boundary for the next HLS segment:
//  1. Prefer cut before the next IDR (PUSI + Random_access_indicator), with at least
//     minHlsIDRSpanBytes from the first such packet in buf (gomedia / well-muxed TS).
//  2. Else cut before the Nth video PUSI (new PES) with the same min span from the first video
//     PUSI in buf — PES-aligned for RTSP and other TS without RA flags.
//
// Returns 0 if the buffer is still too small / not enough PUSI boundaries yet.
func hlsMpegTSSegmentCut(buf []byte) int {
	const pktSize = gompeg2.TS_PAKCET_SIZE
	if len(buf) < 2*pktSize {
		return 0
	}
	videoPID, ok := hlsDiscoverVideoPID(buf)
	if !ok {
		videoPID = 0x100 // gomedia TSMuxer default first elementary PID
	}
	idrRA, pusiStarts := hlsScanVideoBoundaries(buf, videoPID)
	// Some muxers never set random_access_indicator; detect IDR by scanning H.264/HEVC PES payloads.
	idrNAL := hlsScanNALIDRPacketOffsets(buf, videoPID)
	idrStarts := hlsMergeSortedUniqueInts(idrRA, idrNAL)
	if cut := hlsCutAtMinSpan(idrStarts); cut > 0 {
		return cut
	}
	// gomedia RTSP→TS sets RA only on IDRs. If GOP is shorter than minHlsIDRSpanBytes, two IDRs can
	// exist but hlsCutAtMinSpan returns 0 — still cut before the second IDR so we do not stall until
	// a timed 188-byte force cut (breaks PES and common players).
	if len(idrStarts) >= 2 {
		return idrStarts[1]
	}
	// Exactly one IDR in the buffer: waiting for a second IDR can stall for an entire GOP while
	// coalesced video emits one PUSI per frame — no segment is cut and the playlist freezes.
	// Prefer a PUSI-aligned cut with the usual min-byte guard so segments track wall-clock cadence.
	if len(idrStarts) == 1 {
		if cut := hlsCutAtMinSpan(pusiStarts); cut > 0 {
			return cut
		}
		return 0
	}
	return hlsCutAtMinSpan(pusiStarts)
}

func hlsMergeSortedUniqueInts(a, b []int) []int {
	seen := make(map[int]struct{}, len(a)+len(b))
	for _, x := range a {
		seen[x] = struct{}{}
	}
	for _, x := range b {
		seen[x] = struct{}{}
	}
	out := make([]int, 0, len(seen))
	for x := range seen {
		out = append(out, x)
	}
	sort.Ints(out)
	return out
}

// hlsTSPayloadMeta returns the TS payload bytes, PID, and PUSI for one 188-byte packet.
func hlsTSPayloadMeta(pkt []byte) (payload []byte, pid uint16, pusi bool, ok bool) {
	const ts = gompeg2.TS_PAKCET_SIZE
	if len(pkt) != ts || pkt[0] != 0x47 {
		return nil, 0, false, false
	}
	pid = uint16(pkt[1]&0x1F)<<8 | uint16(pkt[2])
	pusi = pkt[1]&0x40 != 0
	afc := (pkt[3] >> 4) & 0x03
	i := 4
	if afc == 2 || afc == 3 {
		if i >= len(pkt) {
			return nil, pid, pusi, false
		}
		adl := int(pkt[i])
		i += 1 + adl
		if i > len(pkt) {
			return nil, pid, pusi, false
		}
	}
	if afc == 2 {
		return nil, pid, pusi, true
	}
	return pkt[i:], pid, pusi, true
}

// hlsPESHeaderDataOffset returns the offset of the first PES payload byte (after the variable PES header).
func hlsPESHeaderDataOffset(p []byte) int {
	if len(p) < 9 {
		return 0
	}
	if p[0] != 0 || p[1] != 0 || p[2] != 1 {
		return 0
	}
	hlen := int(p[8])
	if 9+hlen > len(p) {
		return 0
	}
	return 9 + hlen
}

// hlsScanNALIDRPacketOffsets walks TS in buffer order, reassembles each video PES (by PID), and records
// the file offset of the first TS packet of each PES that contains an H.264 IDR or H.265 IRAP slice.
func hlsScanNALIDRPacketOffsets(buf []byte, videoPID uint16) []int {
	const pkt = gompeg2.TS_PAKCET_SIZE
	var idrs []int
	var (
		inPES   bool
		pusiOff int
		body    []byte
	)
	flush := func() {
		if !inPES || len(body) == 0 {
			inPES = false
			body = body[:0]
			return
		}
		if codec.IsH264IDRFrame(body) || codec.IsH265IDRFrame(body) {
			idrs = append(idrs, pusiOff)
		}
		inPES = false
		body = body[:0]
	}
	for off := 0; off+pkt <= len(buf); off += pkt {
		if buf[off] != 0x47 {
			continue
		}
		pl, pid, pusi, ok := hlsTSPayloadMeta(buf[off : off+pkt])
		if !ok || pid != videoPID {
			continue
		}
		if pusi {
			flush()
			pusiOff = off
			skip := hlsPESHeaderDataOffset(pl)
			if skip > 0 && skip <= len(pl) {
				body = append(body[:0], pl[skip:]...)
				inPES = true
			} else {
				inPES = false
			}
			continue
		}
		if inPES && len(pl) > 0 {
			body = append(body, pl...)
		}
	}
	flush()
	return idrs
}

func hlsScanVideoBoundaries(buf []byte, videoPID uint16) (idrStarts []int, pusiStarts []int) {
	const pktSize = gompeg2.TS_PAKCET_SIZE
	for off := 0; off+pktSize <= len(buf); off += pktSize {
		if buf[off] != 0x47 {
			continue
		}
		bs := codec.NewBitStream(buf[off : off+pktSize])
		var pkg gompeg2.TSPacket
		if err := pkg.DecodeHeader(bs); err != nil {
			continue
		}
		if pkg.PID != videoPID || pkg.Payload_unit_start_indicator != 1 {
			continue
		}
		pusiStarts = append(pusiStarts, off)
		if pkg.Field != nil && pkg.Field.Random_access_indicator == 1 {
			idrStarts = append(idrStarts, off)
		}
	}
	return idrStarts, pusiStarts
}

func hlsCutAtMinSpan(offsets []int) int {
	if len(offsets) < 2 {
		return 0
	}
	first := offsets[0]
	for i := 1; i < len(offsets); i++ {
		if offsets[i]-first >= minHlsIDRSpanBytes {
			return offsets[i]
		}
	}
	return 0
}

func hlsDiscoverVideoPID(buf []byte) (uint16, bool) {
	const pktSize = gompeg2.TS_PAKCET_SIZE
	pmtPIDs := make(map[uint16]struct{})
	var videoPID uint16
	var haveVideo bool

	for off := 0; off+pktSize <= len(buf); off += pktSize {
		if buf[off] != 0x47 {
			continue
		}
		pkt := buf[off : off+pktSize]
		bs := codec.NewBitStream(pkt)
		var pkg gompeg2.TSPacket
		if err := pkg.DecodeHeader(bs); err != nil {
			continue
		}
		pid := pkg.PID

		switch {
		case pid == uint16(gompeg2.TS_PID_PAT):
			if pkg.Payload_unit_start_indicator == 1 {
				bs.SkipBits(8) // pointer_field
			}
			sec, err := gompeg2.ReadSection(gompeg2.TS_TID_PAS, bs)
			if err != nil {
				continue
			}
			pat, ok := sec.(*gompeg2.Pat)
			if !ok {
				continue
			}
			for _, pair := range pat.Pmts {
				if pair.Program_number != 0 {
					pmtPIDs[pair.PID] = struct{}{}
				}
			}

		case hlsPMTTablePID(pmtPIDs, pid):
			if pkg.Payload_unit_start_indicator == 1 {
				bs.SkipBits(8)
			}
			sec, err := gompeg2.ReadSection(gompeg2.TS_TID_PMS, bs)
			if err != nil {
				continue
			}
			pmt, ok := sec.(*gompeg2.Pmt)
			if !ok {
				continue
			}
			for _, s := range pmt.Streams {
				st := gompeg2.TS_STREAM_TYPE(s.StreamType)
				if st == gompeg2.TS_STREAM_H264 || st == gompeg2.TS_STREAM_H265 {
					videoPID = s.Elementary_PID
					haveVideo = true
					goto done
				}
			}
		}
	}
done:
	return videoPID, haveVideo
}

func hlsPMTTablePID(pmtPIDs map[uint16]struct{}, pid uint16) bool {
	_, ok := pmtPIDs[pid]
	return ok
}

// hlsMpegTSForceCutLen returns the largest multiple of 188 not exceeding len(buf), or 0 if empty.
// Used only when a timed segment is overdue and no clean video boundary was found.
func hlsMpegTSForceCutLen(buf []byte) int {
	const pktSize = gompeg2.TS_PAKCET_SIZE
	n := (len(buf) / pktSize) * pktSize
	if n <= 0 {
		return 0
	}
	return n
}

func hlsLogForcedSegmentCut(streamID domain.StreamCode, segIndex int, reason string) {
	slog.Warn("publisher: HLS segment cut without PES boundary (188-byte aligned; playback may glitch)",
		"stream_code", streamID, "segment_index", segIndex, "reason", reason)
}
