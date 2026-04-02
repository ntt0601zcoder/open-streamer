package pull

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpmpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/pion/rtp"
	gocodec "github.com/yapingcat/gomedia/go-codec"

	"github.com/ntthuan060102github/open-streamer/internal/domain"
)

const rtspPktChanSize = 16384

// RTSPReader pulls an RTSP source (DESCRIBE → SETUP → PLAY) and emits domain.AVPacket.
// Supported: H.264 and MPEG-4 Audio (AAC-LC in RTP, RFC 3640) as separate medias.
// Credentials: use rtsp://user:pass@host/... in the URL.
type RTSPReader struct {
	input domain.Input

	mu       sync.Mutex
	client   *gortsplib.Client
	pkts     chan domain.AVPacket
	done     chan struct{}
	procMu   sync.Mutex // serializes RTP callbacks (forma + DTS state)
	h264DTS  *h264.DTSExtractor
}

// NewRTSPReader constructs an RTSPReader for the given input URL.
func NewRTSPReader(input domain.Input) *RTSPReader {
	return &RTSPReader{input: input}
}

// Open connects, negotiates tracks, and starts PLAY in the background.
func (r *RTSPReader) Open(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	r.mu.Lock()
	if r.client != nil {
		r.mu.Unlock()
		return fmt.Errorf("rtsp reader: already open")
	}
	r.pkts = make(chan domain.AVPacket, rtspPktChanSize)
	r.done = make(chan struct{})
	r.mu.Unlock()

	u, err := base.ParseURL(r.input.URL)
	if err != nil {
		r.abortOpen()
		return fmt.Errorf("rtsp reader: parse url %q: %w", r.input.URL, err)
	}

	dialer := &net.Dialer{Timeout: 15 * time.Second}
	client := &gortsplib.Client{
		Scheme: u.Scheme,
		Host:   u.Host,
		UserAgent: "open-streamer/" + u.Scheme,
		DialContext: func(cctx context.Context, network, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, address)
		},
	}

	if err := client.Start(); err != nil {
		r.abortOpen()
		return fmt.Errorf("rtsp reader: start client: %w", err)
	}

	desc, _, err := client.Describe(u)
	if err != nil {
		client.Close()
		r.abortOpen()
		return fmt.Errorf("rtsp reader: describe %q: %w", r.input.URL, err)
	}

	var h264Forma *format.H264
	h264Media := desc.FindFormat(&h264Forma)

	var mpeg4Forma *format.MPEG4Audio
	mpeg4Media := desc.FindFormat(&mpeg4Forma)

	if h264Media == nil && mpeg4Media == nil {
		client.Close()
		r.abortOpen()
		return fmt.Errorf(
			"rtsp reader: no supported tracks in %q (need H264 and/or MPEG-4 Audio AAC)",
			r.input.URL,
		)
	}

	var h264RTPDec *rtph264.Decoder
	if h264Media != nil {
		d, err := h264Forma.CreateDecoder()
		if err != nil {
			client.Close()
			r.abortOpen()
			return fmt.Errorf("rtsp reader: h264 rtp decoder: %w", err)
		}
		h264RTPDec = d
		slog.Info("rtsp reader: track selected", "url", r.input.URL, "kind", "H264")
	}

	var mpeg4RTPDec *rtpmpeg4audio.Decoder
	if mpeg4Media != nil {
		d, err := mpeg4Forma.CreateDecoder()
		if err != nil {
			client.Close()
			r.abortOpen()
			return fmt.Errorf("rtsp reader: mpeg4 audio rtp decoder: %w", err)
		}
		mpeg4RTPDec = d
		slog.Info("rtsp reader: track selected", "url", r.input.URL, "kind", "MPEG4Audio")
	}

	setupMedias := make([]*description.Media, 0, 2)
	seen := make(map[*description.Media]struct{})
	for _, m := range []*description.Media{h264Media, mpeg4Media} {
		if m == nil {
			continue
		}
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		setupMedias = append(setupMedias, m)
	}

	if err := client.SetupAll(desc.BaseURL, setupMedias); err != nil {
		client.Close()
		r.abortOpen()
		return fmt.Errorf("rtsp reader: setup %q: %w", r.input.URL, err)
	}

	if h264Media != nil {
		hF, hD := h264Forma, h264RTPDec
		client.OnPacketRTP(h264Media, hF, func(pkt *rtp.Packet) {
			r.onH264Packet(client, hF, hD, h264Media, pkt)
		})
	}

	if mpeg4Media != nil {
		aF, aD := mpeg4Forma, mpeg4RTPDec
		client.OnPacketRTP(mpeg4Media, aF, func(pkt *rtp.Packet) {
			r.onMPEG4AudioPacket(client, aF, aD, mpeg4Media, pkt)
		})
	}

	if _, err := client.Play(nil); err != nil {
		client.Close()
		r.abortOpen()
		return fmt.Errorf("rtsp reader: play %q: %w", r.input.URL, err)
	}

	if h264Media != nil {
		d := &h264.DTSExtractor{}
		d.Initialize()
		r.procMu.Lock()
		r.h264DTS = d
		r.procMu.Unlock()
	}

	r.mu.Lock()
	r.client = client
	r.mu.Unlock()

	go r.waitLoop()
	return nil
}

func (r *RTSPReader) abortOpen() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pkts != nil {
		close(r.pkts)
	}
	r.pkts = nil
	r.done = nil
	r.client = nil
}

func (r *RTSPReader) waitLoop() {
	r.mu.Lock()
	c := r.client
	r.mu.Unlock()
	if c == nil {
		return
	}
	_ = c.Wait()
	r.teardownAfterWait()
}

func (r *RTSPReader) teardownAfterWait() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.client = nil
	r.procMu.Lock()
	r.h264DTS = nil
	r.procMu.Unlock()
	if r.done != nil {
		close(r.done)
		r.done = nil
	}
	if r.pkts != nil {
		close(r.pkts)
		r.pkts = nil
	}
}

func ptsMillis(pts int64, clockRate int) uint64 {
	if clockRate <= 0 {
		return 0
	}
	return uint64(pts * 1000 / int64(clockRate))
}

func (r *RTSPReader) onH264Packet(
	c *gortsplib.Client,
	forma *format.H264,
	rtpDec *rtph264.Decoder,
	medi *description.Media,
	pkt *rtp.Packet,
) {
	pts90k, ok := c.PacketPTS(medi, pkt)
	if !ok {
		return
	}
	au, err := rtpDec.Decode(pkt)
	if err != nil {
		if !errors.Is(err, rtph264.ErrNonStartingPacketAndNoPrevious) && !errors.Is(err, rtph264.ErrMorePacketsNeeded) {
			slog.Debug("rtsp reader: h264 rtp decode", "url", r.input.URL, "err", err)
		}
		return
	}

	r.procMu.Lock()
	dts90k := pts90k
	if r.h264DTS != nil {
		if d, derr := r.h264DTS.Extract(au, pts90k); derr == nil {
			dts90k = d
		}
	}
	data, key := h264AUToPacket(forma, au)
	r.procMu.Unlock()

	if len(data) == 0 {
		return
	}

	cr := forma.ClockRate()
	ptsMS := ptsMillis(pts90k, cr)
	dtsMS := ptsMillis(dts90k, cr)
	if dtsMS > ptsMS {
		dtsMS = ptsMS
	}

	p := domain.AVPacket{
		Codec:    domain.AVCodecH264,
		Data:     data,
		PTSms:    ptsMS,
		DTSms:    dtsMS,
		KeyFrame: key || gocodec.IsH264IDRFrame(data),
	}

	r.mu.Lock()
	outCh := r.pkts
	doneCh := r.done
	r.mu.Unlock()
	if outCh == nil {
		return
	}
	select {
	case outCh <- p:
	case <-doneCh:
	}
}

// h264AUToPacket turns RTP reassembled NALUs into one Annex B access unit; updates forma SPS/PPS from stream.
// Caller must hold RTSPReader.procMu while mutating forma and if DTS extraction shares the same AU order.
func h264AUToPacket(forma *format.H264, au [][]byte) (data []byte, idr bool) {
	var filtered [][]byte
	nonIDR := false
	hasIDR := false

	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		typ := h264.NALUType(nalu[0] & 0x1F)
		switch typ {
		case h264.NALUTypeSPS:
			forma.SPS = append([]byte(nil), nalu...)
			continue
		case h264.NALUTypePPS:
			forma.PPS = append([]byte(nil), nalu...)
			continue
		case h264.NALUTypeAccessUnitDelimiter:
			continue
		case h264.NALUTypeFillerData:
			continue
		case h264.NALUTypeIDR:
			hasIDR = true
		case h264.NALUTypeNonIDR:
			nonIDR = true
		}
		filtered = append(filtered, nalu)
	}

	if len(filtered) == 0 || (!nonIDR && !hasIDR) {
		return nil, false
	}

	if hasIDR && len(forma.SPS) > 0 && len(forma.PPS) > 0 {
		filtered = append([][]byte{forma.SPS, forma.PPS}, filtered...)
	}

	var b []byte
	for _, nalu := range filtered {
		b = append(b, 0, 0, 0, 1)
		b = append(b, nalu...)
	}
	return b, hasIDR
}

func (r *RTSPReader) onMPEG4AudioPacket(
	c *gortsplib.Client,
	forma *format.MPEG4Audio,
	rtpDec *rtpmpeg4audio.Decoder,
	medi *description.Media,
	pkt *rtp.Packet,
) {
	pts0, ok := c.PacketPTS(medi, pkt)
	if !ok {
		return
	}
	aus, err := rtpDec.Decode(pkt)
	if err != nil {
		slog.Debug("rtsp reader: aac rtp decode", "url", r.input.URL, "err", err)
		return
	}
	if forma.Config == nil {
		return
	}

	cr := forma.ClockRate()
	// RFC 3640: one RTP timestamp marks the first AAC frame; additional frames are +1024 samples each.
	const samplesPerAU = 1024

	r.mu.Lock()
	outCh := r.pkts
	doneCh := r.done
	r.mu.Unlock()
	if outCh == nil {
		return
	}

	for i, au := range aus {
		adts, err := aacToADTS(forma.Config, au)
		if err != nil || len(adts) == 0 {
			continue
		}
		ptsTicks := pts0 + int64(i*samplesPerAU)
		ptsMS := ptsMillis(ptsTicks, cr)
		p := domain.AVPacket{
			Codec: domain.AVCodecAAC,
			Data:  adts,
			PTSms: ptsMS,
			DTSms: ptsMS,
		}
		select {
		case outCh <- p:
		case <-doneCh:
			return
		}
	}
}

func aacToADTS(cfg *mpeg4audio.AudioSpecificConfig, au []byte) ([]byte, error) {
	ch := cfg.ChannelConfig
	chCount := cfg.ChannelCount
	if ch == 0 && len(au) > 0 {
		if n, err := mpeg4audio.CountChannelsFromRawDataBlock(au); err == nil && n > 0 {
			chCount = n
		}
	}
	pkts := mpeg4audio.ADTSPackets{{
		Type:          cfg.Type,
		SampleRate:    cfg.SampleRate,
		ChannelConfig: ch,
		ChannelCount:  chCount,
		AU:            au,
	}}
	return pkts.Marshal()
}

// ReadPackets returns the next batch of access units.
func (r *RTSPReader) ReadPackets(ctx context.Context) ([]domain.AVPacket, error) {
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

// Close stops the RTSP session; the wait loop closes packet channels.
func (r *RTSPReader) Close() error {
	r.mu.Lock()
	c := r.client
	r.client = nil
	r.mu.Unlock()
	if c != nil {
		c.Close()
	}
	return nil
}
