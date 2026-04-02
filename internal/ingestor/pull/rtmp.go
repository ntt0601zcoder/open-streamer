package pull

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	joyav "github.com/nareix/joy4/av"
	"github.com/nareix/joy4/codec/aacparser"
	joyh264 "github.com/nareix/joy4/codec/h264parser"
	joyrtmp "github.com/nareix/joy4/format/rtmp"

	gocodec "github.com/yapingcat/gomedia/go-codec"

	"github.com/ntthuan060102github/open-streamer/internal/domain"
)

const rtmpChanSize = 16384

// RTMPReader connects to a remote RTMP server in play (pull) mode and emits domain.AVPacket.
// Signalling uses joy4 (some servers send AMF0 shapes gomedia's client cannot parse).
// Video: AVCC → Annex B; audio: raw AAC → ADTS when needed (same layout as former TS path).
type RTMPReader struct {
	input domain.Input

	mu   sync.Mutex
	conn *joyrtmp.Conn
	pkts chan domain.AVPacket
	done chan struct{}

	filtered            []joyav.CodecData
	idxMap              map[int]int // source stream index -> index in filtered slice
	h264KeyPrefixAnnexB [][]byte    // per filtered index: 0x00000001+SPS+0x00000001+PPS
	aacCfg              *aacparser.MPEG4AudioConfig
}

// NewRTMPReader constructs an RTMPReader.
func NewRTMPReader(input domain.Input) *RTMPReader {
	return &RTMPReader{input: input}
}

// Open connects to the RTMP source and starts the read loop.
func (r *RTMPReader) Open(ctx context.Context) error {
	_ = ctx

	r.mu.Lock()
	if r.conn != nil {
		r.mu.Unlock()
		return fmt.Errorf("rtmp reader: already open")
	}
	r.pkts = make(chan domain.AVPacket, rtmpChanSize)
	r.done = make(chan struct{})
	r.mu.Unlock()

	conn, err := joyrtmp.Dial(r.input.URL)
	if err != nil {
		r.abortOpen()
		return fmt.Errorf("rtmp reader: dial %q: %w", r.input.URL, err)
	}
	streams, err := conn.Streams()
	if err != nil {
		_ = conn.Close()
		r.abortOpen()
		return fmt.Errorf("rtmp reader: streams %q: %w", r.input.URL, err)
	}
	for i, s := range streams {
		supported := s.Type() == joyav.H264 || s.Type() == joyav.AAC
		slog.Info("rtmp reader: source stream discovered",
			"url", r.input.URL,
			"source_stream_idx", i,
			"codec", s.Type().String(),
			"supported", supported,
		)
		if s.Type() == joyav.AAC {
			if cd, ok := s.(aacparser.CodecData); ok {
				x := new(aacparser.MPEG4AudioConfig)
				*x = cd.Config
				r.aacCfg = x
			}
		}
	}

	filtered, idxMap := filterSupportedRTMPStreams(streams)
	if len(filtered) == 0 {
		_ = conn.Close()
		r.abortOpen()
		return fmt.Errorf("rtmp reader: no supported streams in %q (need H264 and/or AAC)", r.input.URL)
	}
	for srcIdx, mappedIdx := range idxMap {
		slog.Info("rtmp reader: stream selected",
			"url", r.input.URL,
			"source_stream_idx", srcIdx,
			"es_stream_idx", mappedIdx,
			"codec", streams[srcIdx].Type().String(),
		)
	}

	h264Prefix := make([][]byte, len(filtered))
	for i, s := range filtered {
		if s.Type() != joyav.H264 {
			continue
		}
		cd, ok := s.(joyh264.CodecData)
		if !ok {
			slog.Warn("rtmp reader: H264 stream has no h264parser.CodecData — cannot inject SPS/PPS",
				"url", r.input.URL, "es_stream_idx", i)
			continue
		}
		sps, pps := cd.SPS(), cd.PPS()
		if len(sps) == 0 || len(pps) == 0 {
			slog.Warn("rtmp reader: H264 codec data missing SPS or PPS",
				"url", r.input.URL, "es_stream_idx", i)
			continue
		}
		var b []byte
		b = append(b, 0, 0, 0, 1)
		b = append(b, sps...)
		b = append(b, 0, 0, 0, 1)
		b = append(b, pps...)
		h264Prefix[i] = b
	}

	r.mu.Lock()
	r.conn = conn
	r.filtered = filtered
	r.idxMap = idxMap
	r.h264KeyPrefixAnnexB = h264Prefix
	r.mu.Unlock()

	go r.readLoop()
	return nil
}

func (r *RTMPReader) abortOpen() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pkts != nil {
		close(r.pkts)
	}
	r.pkts = nil
	r.done = nil
	r.conn = nil
}

func (r *RTMPReader) readLoop() {
	defer r.teardownAfterReadLoop()
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("rtmp reader: panic in joy4, closing connection",
				"url", r.input.URL,
				"panic", rec,
			)
		}
	}()

	for {
		r.mu.Lock()
		c := r.conn
		r.mu.Unlock()
		if c == nil {
			return
		}
		pkt, err := c.ReadPacket()
		if err != nil {
			slog.Warn("rtmp reader: read packet ended", "url", r.input.URL, "err", err)
			return
		}
		if r.emitJoyPacket(pkt) {
			return
		}
	}
}

// emitJoyPacket converts one joy4 packet to AVPacket and sends it. Returns true to stop the read loop.
func (r *RTMPReader) emitJoyPacket(pkt joyav.Packet) bool {
	mapped, ok := r.idxMap[int(pkt.Idx)]
	if !ok || mapped < 0 || mapped >= len(r.filtered) {
		return false
	}
	codec := r.filtered[mapped].Type()
	dtsMS := uint64(pkt.Time / time.Millisecond)
	ptsMS := dtsMS + uint64(pkt.CompositionTime/time.Millisecond)

	r.mu.Lock()
	outCh := r.pkts
	doneCh := r.done
	r.mu.Unlock()
	if outCh == nil {
		return true
	}

	switch codec {
	case joyav.H264:
		annexB := h264ForTSMuxer(pkt.Data)
		data := h264AccessUnitForTS(pkt.IsKeyFrame, r.h264KeyPrefixAnnexB[mapped], annexB)
		if len(data) == 0 {
			return false
		}
		p := domain.AVPacket{
			Codec:    domain.AVCodecH264,
			Data:     data,
			PTSms:    ptsMS,
			DTSms:    dtsMS,
			KeyFrame: gocodec.IsH264IDRFrame(data),
		}
		select {
		case outCh <- p:
		case <-doneCh:
			return true
		}
	case joyav.AAC:
		data := r.aacForADTS(pkt.Data)
		if len(data) == 0 {
			return false
		}
		p := domain.AVPacket{
			Codec: domain.AVCodecAAC,
			Data:  data,
			PTSms: dtsMS,
			DTSms: dtsMS,
		}
		select {
		case outCh <- p:
		case <-doneCh:
			return true
		}
	default:
		return false
	}
	return false
}

func (r *RTMPReader) aacForADTS(raw []byte) []byte {
	if len(raw) >= 2 && raw[0] == 0xff && raw[1]&0xf0 == 0xf0 {
		return raw
	}
	if r.aacCfg == nil {
		return raw
	}
	hdr := make([]byte, aacparser.ADTSHeaderLength)
	aacparser.FillADTSHeader(hdr, *r.aacCfg, 1024, len(raw))
	out := make([]byte, 0, len(hdr)+len(raw))
	out = append(out, hdr...)
	out = append(out, raw...)
	return out
}

// h264AccessUnitForTS prepends SPS/PPS (from RTMP codec config) on keyframes for decoders that expect in-band PS.
func h264AccessUnitForTS(isKeyFrame bool, psPrefixAnnexB, annexBAU []byte) []byte {
	if len(annexBAU) == 0 {
		return nil
	}
	if len(psPrefixAnnexB) == 0 || !isKeyFrame {
		return annexBAU
	}
	out := make([]byte, 0, len(psPrefixAnnexB)+len(annexBAU))
	out = append(out, psPrefixAnnexB...)
	out = append(out, annexBAU...)
	return out
}

// h264ForTSMuxer returns Annex-B NALs (name kept for tests). Joy4 RTMP video is usually AVCC.
func h264ForTSMuxer(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	if isH264AVCCAccessUnit(b) {
		return avccAccessUnitToAnnexB(b)
	}
	return b
}

const maxH264NALSize = 8 * 1024 * 1024

func isH264AVCCAccessUnit(b []byte) bool {
	i := 0
	for i+4 <= len(b) {
		n := int(binary.BigEndian.Uint32(b[i : i+4]))
		if n <= 0 || n > maxH264NALSize || i+4+n > len(b) {
			return false
		}
		i += 4 + n
	}
	return i == len(b)
}

func avccAccessUnitToAnnexB(src []byte) []byte {
	var out []byte
	i := 0
	for i+4 <= len(src) {
		n := int(binary.BigEndian.Uint32(src[i : i+4]))
		i += 4
		if n <= 0 || i+n > len(src) {
			return src
		}
		out = append(out, 0, 0, 0, 1)
		out = append(out, src[i:i+n]...)
		i += n
	}
	if len(out) == 0 {
		return src
	}
	return out
}

func (r *RTMPReader) teardownAfterReadLoop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn != nil {
		_ = r.conn.Close()
		r.conn = nil
	}
	if r.done != nil {
		close(r.done)
		r.done = nil
	}
	if r.pkts != nil {
		close(r.pkts)
		r.pkts = nil
	}
}

// ReadPackets returns the next batch of access units.
func (r *RTMPReader) ReadPackets(ctx context.Context) ([]domain.AVPacket, error) {
	r.mu.Lock()
	ch := r.pkts
	r.mu.Unlock()
	if ch == nil {
		return nil, io.EOF
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case p, ok := <-ch:
		if !ok {
			return nil, io.EOF
		}
		batch := []domain.AVPacket{p}
		for len(batch) < 256 {
			select {
			case p2, ok2 := <-ch:
				if !ok2 {
					return batch, nil
				}
				batch = append(batch, p2)
			default:
				return batch, nil
			}
		}
		return batch, nil
	}
}

// Close closes the RTMP connection; readLoop then closes packet channels.
func (r *RTMPReader) Close() error {
	r.mu.Lock()
	c := r.conn
	r.conn = nil
	r.mu.Unlock()
	if c != nil {
		return c.Close()
	}
	return nil
}

func filterSupportedRTMPStreams(streams []joyav.CodecData) ([]joyav.CodecData, map[int]int) {
	filtered := make([]joyav.CodecData, 0, len(streams))
	idxMap := make(map[int]int, len(streams))
	for i, s := range streams {
		switch s.Type() {
		case joyav.H264, joyav.AAC:
			idxMap[i] = len(filtered)
			filtered = append(filtered, s)
		}
	}
	return filtered, idxMap
}
