package publisher

// push_rtmp.go — outbound RTMP/RTMPS re-stream (push to external endpoint).
//
// Data flow:
//
//	Buffer Hub → tsmux.FeedWirePacket → tsBuffer → mpeg2.TSDemuxer
//	                                                        │
//	                                        H.264 Annex-B + AAC ADTS
//	                                                        │
//	                         joy4 RTMP publish → remote RTMP(S) server
//
// Codec probing: accumulates SPS+PPS bytes from H.264 frames and AAC config
// from the first ADTS frame. Once both are known, dials the destination and
// calls WriteHeader. Frames that arrive before the connection is ready are
// held in a bounded pending queue; the queue is flushed starting from the
// first keyframe after the connection is established.
//
// Reconnect: on any dial or write error the session ends; the caller
// (serveRTMPPush) waits RetryTimeoutSec and retries. Codec data is preserved
// across reconnects so the connection can be re-established quickly.
//
// Input switching: when the ingestor switches to a different input source it
// marks the first packet with Discontinuity=true.  feedLoop detects this,
// closes the tsBuffer (which causes the TSDemuxer to exit), and signals the
// session to end via errDiscontinuity.  serveRTMPPush then immediately starts
// a fresh session with clean codec probing — no retry delay, no preserved
// codec data — because the new source may have different codec parameters.
//
// RTMPS: rtmps:// URLs use a TLS-wrapped connection (default port 443).
// joy4's NewConn accepts any net.Conn, so the RTMP handshake and framing
// are identical — only the transport layer changes.

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/avc"
	joyav "github.com/nareix/joy4/av"
	"github.com/nareix/joy4/codec/aacparser"
	joyh264 "github.com/nareix/joy4/codec/h264parser"
	joyrtmp "github.com/nareix/joy4/format/rtmp"
	mpeg2 "github.com/yapingcat/gomedia/go-mpeg2"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/tsmux"
)

const (
	// rtmpPushPendingMax caps the pending frame queue while waiting for codec init.
	rtmpPushPendingMax = 300
)

// errDiscontinuity signals that the input source changed (failover / manual
// switch).  The session must be torn down and restarted with fresh codec
// probing because the new source may have different codec parameters.
var errDiscontinuity = errors.New("input discontinuity")

// rtmpPendingFrame is one queued frame before the connection is established.
type rtmpPendingFrame struct {
	pkt joyav.Packet
}

// rtmpPushPackager holds state for one outbound RTMP push session.
type rtmpPushPackager struct {
	streamID   domain.StreamCode
	url        string
	timeoutSec int

	// Codec state — set once and preserved across reconnects.
	videoPS []byte // accumulated Annex-B bytes for SPS/PPS detection
	h264CD  joyav.CodecData
	aacCfg  *aacparser.MPEG4AudioConfig
	aacCD   joyav.CodecData

	// Connection state — reset on each reconnect attempt.
	conn  *joyrtmp.Conn
	ready atomic.Bool // true once WriteHeader has succeeded

	// Pending frames buffered before conn is ready.
	pending []rtmpPendingFrame

	// Base DTS for relative timestamps (reset on reconnect).
	baseDTS     uint64
	baseDTSSet  bool
	baseADTS    uint64
	baseADTSSet bool

	// connErr holds the first connection failure (write error or server-side
	// close).  Set under failOnce so the first error wins; subsequent
	// failures are dropped.  run() reads it after the demuxer exits to
	// decide what to return.  atomic.Pointer makes cross-goroutine access
	// race-free without taking a mutex on the hot write path.
	connErr   atomic.Pointer[error]
	failOnce  sync.Once
	closeOnce sync.Once

	// tsBuf is the TS buffer fed by feedLoop and read by the demuxer.  Stored
	// on the packager so other goroutines (onTSFrame, drainServerMessages)
	// can close it on connection failure — this unblocks the demuxer and
	// lets run() exit so serveRTMPPush can retry.
	tsBuf *tsBuffer

	// gotDiscontinuity is set by feedLoop when it receives a packet with
	// Discontinuity=true (input source changed).  run() checks this after the
	// demuxer exits and returns errDiscontinuity to the caller.
	gotDiscontinuity atomic.Bool
}

// failConn marks the session as failed and unblocks the demuxer so run() can
// exit and serveRTMPPush can retry.  Safe to call from multiple goroutines;
// the first call wins, subsequent calls are no-ops.
func (p *rtmpPushPackager) failConn(err error) {
	p.failOnce.Do(func() {
		p.connErr.Store(&err)
		p.closeConn()
		if p.tsBuf != nil {
			p.tsBuf.Close()
		}
	})
}

// run is the entry point: wires the TS pipe, demuxer, and drives the session.
// Returns when ctx is cancelled or a write/dial error occurs.
func (p *rtmpPushPackager) run(ctx context.Context, sub *buffer.Subscriber) error {
	tb := newTSBuffer()
	p.tsBuf = tb
	go p.feedLoop(ctx, sub, tb)

	demux := mpeg2.NewTSDemuxer()
	demux.OnFrame = p.onTSFrame

	demuxDone := make(chan error, 1)
	go func() { demuxDone <- demux.Input(tb) }()

	select {
	case <-ctx.Done():
		tb.Close()
		<-demuxDone
	case err := <-demuxDone:
		if err != nil && ctx.Err() == nil && !p.gotDiscontinuity.Load() {
			slog.Warn("publisher: RTMP push TS demux ended",
				"stream_code", p.streamID, "url", p.url, "err", err)
		}
	}

	// Flush any buffered frames before closing so the remote endpoint sees a
	// clean end-of-stream rather than a truncated TCP close mid-frame.
	if p.conn != nil && p.ready.Load() && p.connErr.Load() == nil {
		if err := p.conn.WriteTrailer(); err != nil {
			slog.Debug("publisher: RTMP push write trailer",
				"stream_code", p.streamID, "url", p.url, "err", err)
		}
	}
	p.closeConn()

	if p.gotDiscontinuity.Load() {
		slog.Info("publisher: RTMP push session ending on discontinuity",
			"stream_code", p.streamID, "url", p.url,
			"had_h264_cd", p.h264CD != nil, "had_aac_cd", p.aacCD != nil,
			"was_ready", p.ready.Load())
		return errDiscontinuity
	}
	if errPtr := p.connErr.Load(); errPtr != nil {
		return *errPtr
	}
	return nil
}

// feedLoop reads packets from sub, reassembles 188-byte TS packets via
// alignedFeed, and pipes them into tb.  Runs in its own goroutine.
//
// When a Discontinuity packet arrives (input source changed), feedLoop sets
// gotDiscontinuity and returns.  The deferred tb.Close() causes the demuxer
// to exit, which in turn causes run() to return errDiscontinuity.
func (p *rtmpPushPackager) feedLoop(ctx context.Context, sub *buffer.Subscriber, tb *tsBuffer) {
	defer tb.Close()
	var tsCarry []byte
	var avMux *tsmux.FromAV

	for {
		select {
		case <-ctx.Done():
			return
		case pkt, ok := <-sub.Recv():
			if !ok {
				return
			}
			if pkt.AV != nil && pkt.AV.Discontinuity {
				p.gotDiscontinuity.Store(true)
				return
			}
			tsmux.FeedWirePacket(pkt.TS, pkt.AV, &avMux, func(b []byte) {
				alignedFeed(b, &tsCarry, func(pkt188 []byte) bool {
					_, err := tb.Write(pkt188)
					return err == nil
				})
			})
		}
	}
}

// onTSFrame is the TSDemuxer callback; runs in the demuxer goroutine.
func (p *rtmpPushPackager) onTSFrame(cid mpeg2.TS_STREAM_TYPE, frame []byte, pts, dts uint64) {
	if p.connErr.Load() != nil {
		return
	}

	switch cid {
	case mpeg2.TS_STREAM_H264:
		if len(frame) == 0 {
			return
		}
		p.handleVideoFrame(frame, pts, dts)

	case mpeg2.TS_STREAM_H265:
		// H.265 is not supported by the RTMP spec; skip silently.
		return

	case mpeg2.TS_STREAM_AAC:
		p.handleAudioFrame(frame, dts)

	case mpeg2.TS_STREAM_AUDIO_MPEG1, mpeg2.TS_STREAM_AUDIO_MPEG2:
		// MP3 audio in TS — not supported for RTMP push.
		return

	default:
		return
	}
}

func (p *rtmpPushPackager) handleVideoFrame(frame []byte, pts, dts uint64) {
	// Accumulate SPS/PPS until H.264 codec data is built.
	if p.h264CD == nil {
		p.videoPS = append(p.videoPS, frame...)
		p.tryBuildH264CD()
	}

	avcc := h264AnnexBToAVCC(frame)
	if len(avcc) == 0 {
		return
	}
	isKey := tsmux.KeyFrameH264(frame)

	if !p.baseDTSSet {
		p.baseDTS = dts
		p.baseDTSSet = true
	}
	elapsed := dts - p.baseDTS
	cto := int64(pts) - int64(dts)
	if cto < 0 {
		cto = 0
	}

	pkt := joyav.Packet{
		Idx:             0,
		IsKeyFrame:      isKey,
		Data:            avcc,
		Time:            time.Duration(elapsed) * time.Millisecond,
		CompositionTime: time.Duration(cto) * time.Millisecond,
	}

	p.enqueueOrWrite(pkt)
}

func (p *rtmpPushPackager) handleAudioFrame(frame []byte, dts uint64) {
	pos := 0
	for pos+7 <= len(frame) {
		hdr, hLen, err := aac.DecodeADTSHeader(bytes.NewReader(frame[pos:]))
		if err != nil {
			pos++
			continue
		}
		frameLen := int(hdr.HeaderLength) + int(hdr.PayloadLength)
		if pos+frameLen > len(frame) {
			break
		}
		rawAAC := frame[pos+hLen : pos+frameLen]

		// Build AAC codec data from first ADTS frame.
		if p.aacCD == nil && p.aacCfg == nil {
			joyCfg, joyErr := adtsHdrToJoyCfg(hdr)
			if joyErr == nil {
				p.aacCfg = &joyCfg
				cd, cdErr := aacparser.NewCodecDataFromMPEG4AudioConfig(joyCfg)
				if cdErr == nil {
					p.aacCD = cd
				}
			}
		}

		if !p.baseADTSSet {
			p.baseADTS = dts
			p.baseADTSSet = true
		}
		elapsed := dts - p.baseADTS

		pkt := joyav.Packet{
			Idx:        1,
			IsKeyFrame: true,
			Data:       append([]byte(nil), rawAAC...),
			Time:       time.Duration(elapsed) * time.Millisecond,
		}
		p.enqueueOrWrite(pkt)

		pos += frameLen
	}
}

// enqueueOrWrite either buffers the packet (during codec init / before connect)
// or writes it directly to the open connection.
func (p *rtmpPushPackager) enqueueOrWrite(pkt joyav.Packet) {
	if !p.ready.Load() {
		// Try to connect now that we may have codec data.
		if p.h264CD != nil && p.aacCD != nil {
			if err := p.connect(); err != nil {
				slog.Warn("publisher: RTMP push connect failed",
					"stream_code", p.streamID, "url", p.url, "err", err)
				p.failConn(err)
				return
			}
			// Flush pending from first keyframe.
			p.flushPending()
		} else {
			// Buffer until codec is ready.
			if len(p.pending) >= rtmpPushPendingMax {
				p.pending = p.pending[1:] // drop oldest
			}
			p.pending = append(p.pending, rtmpPendingFrame{pkt: pkt})
			return
		}
	}

	if err := p.conn.WritePacket(pkt); err != nil {
		slog.Warn("publisher: RTMP push write error",
			"stream_code", p.streamID, "url", p.url, "err", err,
			"local_addr", connLocalAddr(p.conn))
		// failConn closes the conn and tsBuf so the demuxer unblocks and
		// run() returns — without this the session would zombify (conn
		// closed, onTSFrame drops all frames, no retry triggered).
		p.failConn(err)
	}
}

// rtmpDial opens a joy4 RTMP connection for both rtmp:// and rtmps:// URLs.
//
//   - rtmp://  — plain TCP, default port 1935.
//   - rtmps:// — TLS over TCP, default port 443.  joy4's NewConn wraps any
//     net.Conn, so the RTMP framing layer is identical in both cases.
func rtmpDial(rawURL string, timeout time.Duration) (*joyrtmp.Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}

	// Apply scheme-specific default port when none is present.
	host := u.Host
	if _, _, splitErr := net.SplitHostPort(host); splitErr != nil {
		switch u.Scheme {
		case "rtmps":
			host += ":443"
		default:
			host += ":1935"
		}
		u.Host = host
	}

	dialer := &net.Dialer{Timeout: timeout}

	var netconn net.Conn
	switch u.Scheme {
	case "rtmps":
		tlsDialer := &tls.Dialer{
			NetDialer: dialer,
			Config:    &tls.Config{ServerName: u.Hostname()},
		}
		netconn, err = tlsDialer.DialContext(context.Background(), "tcp", host)
		if err != nil {
			return nil, fmt.Errorf("tls dial: %w", err)
		}
	default: // "rtmp"
		netconn, err = dialer.Dial("tcp", host)
		if err != nil {
			return nil, fmt.Errorf("tcp dial: %w", err)
		}
	}

	conn := joyrtmp.NewConn(netconn)
	conn.URL = u
	return conn, nil
}

// connect dials the RTMP/RTMPS URL and calls WriteHeader.
func (p *rtmpPushPackager) connect() error {
	timeout := time.Duration(p.timeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	slog.Info("publisher: RTMP push connecting", "stream_code", p.streamID, "url", p.url)

	conn, err := rtmpDial(p.url, timeout)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	streams := []joyav.CodecData{p.h264CD, p.aacCD}
	if err := conn.WriteHeader(streams); err != nil {
		_ = conn.Close()
		return fmt.Errorf("write header: %w", err)
	}

	p.conn = conn
	p.ready.Store(true)
	slog.Info("publisher: RTMP push connected",
		"stream_code", p.streamID, "url", p.url,
		"local_addr", connLocalAddr(conn))

	go p.drainServerMessages(conn)
	return nil
}

// drainServerMessages reads any data the RTMP server sends after WriteHeader
// and discards it.  Joy4's publish path is write-only after handshake — it
// never reads from the connection again — so the kernel TCP receive buffer
// fills with server-side messages (e.g. Window Acknowledgement, UserControl
// PingRequest).  Some servers (Flussonic, nginx-rtmp) expect a PingResponse
// within ~60s and close the connection if none arrives, manifesting as the
// next WritePacket failing with "broken pipe".
//
// This goroutine is purely diagnostic: it logs the first batch of received
// bytes (so we can identify the message type) and detects server-initiated
// close immediately rather than waiting for the next write to fail.  It does
// NOT send any reply, so it will not by itself prevent the disconnect — but
// the log output is what tells us whether ping/pong is the actual cause.
func (p *rtmpPushPackager) drainServerMessages(conn *joyrtmp.Conn) {
	nc := conn.NetConn()
	if nc == nil {
		return
	}
	buf := make([]byte, 4096)
	logged := false
	totalRx := 0
	for {
		n, err := nc.Read(buf)
		if n > 0 {
			totalRx += n
			if !logged {
				slog.Info("publisher: RTMP push received server data",
					"stream_code", p.streamID, "url", p.url,
					"local_addr", connLocalAddr(conn),
					"first_bytes_hex", fmt.Sprintf("%x", buf[:n]),
					"len", n)
				logged = true
			}
		}
		if err != nil {
			slog.Info("publisher: RTMP push server-side connection ended",
				"stream_code", p.streamID, "url", p.url,
				"local_addr", connLocalAddr(conn),
				"total_rx_bytes", totalRx, "err", err)
			// Treat server close like a write error — exit the session so
			// serveRTMPPush can retry.  failConn is idempotent so this is
			// safe even if a concurrent write also fails.
			p.failConn(fmt.Errorf("server closed connection: %w", err))
			return
		}
	}
}

// connLocalAddr returns the local TCP address of the connection (best-effort,
// for log correlation with kernel error messages like "write tcp <local>...").
func connLocalAddr(c *joyrtmp.Conn) string {
	if c == nil || c.NetConn() == nil {
		return ""
	}
	if a := c.NetConn().LocalAddr(); a != nil {
		return a.String()
	}
	return ""
}

// flushPending writes queued frames starting from the first video keyframe.
func (p *rtmpPushPackager) flushPending() {
	// Find the first video keyframe in pending queue.
	start := 0
	for i, f := range p.pending {
		if f.pkt.Idx == 0 && f.pkt.IsKeyFrame {
			start = i
			break
		}
	}
	for _, f := range p.pending[start:] {
		if err := p.conn.WritePacket(f.pkt); err != nil {
			slog.Warn("publisher: RTMP push flush error",
				"stream_code", p.streamID, "url", p.url, "err", err,
				"local_addr", connLocalAddr(p.conn))
			p.failConn(err)
			break
		}
	}
	p.pending = p.pending[:0]
}

// closeConn closes the RTMP connection exactly once.  Safe to call from
// multiple goroutines (failConn from drain/onTSFrame, plus run() at end).
func (p *rtmpPushPackager) closeConn() {
	p.closeOnce.Do(func() {
		if p.conn != nil {
			_ = p.conn.Close()
		}
		p.ready.Store(false)
	})
}

// tryBuildH264CD scans accumulated videoPS for SPS+PPS and builds joy4 CodecData.
func (p *rtmpPushPackager) tryBuildH264CD() {
	if len(p.videoPS) < 20 {
		return
	}
	psBuf := annexB4To3ForPSExtract(p.videoPS)
	spss := avc.ExtractNalusOfTypeFromByteStream(avc.NALU_SPS, psBuf, false)
	ppss := avc.ExtractNalusOfTypeFromByteStream(avc.NALU_PPS, psBuf, false)
	if len(spss) == 0 || len(ppss) == 0 {
		return
	}
	cd, err := joyh264.NewCodecDataFromSPSAndPPS(spss[0], ppss[0])
	if err != nil {
		slog.Warn("publisher: RTMP push H.264 codec data error",
			"stream_code", p.streamID, "err", err)
		return
	}
	p.h264CD = cd
	p.videoPS = nil
}

// adtsHdrToJoyCfg converts an mp4ff ADTS header to a joy4 MPEG4AudioConfig.
func adtsHdrToJoyCfg(hdr *aac.ADTSHeader) (aacparser.MPEG4AudioConfig, error) {
	freq := int(hdr.Frequency())
	if freq <= 0 {
		return aacparser.MPEG4AudioConfig{}, fmt.Errorf("invalid AAC sample rate from ADTS")
	}
	// Map frequency to joy4 SampleRateIndex.
	srIdx := aacSampleRateIndex(freq)
	chCfg := uint(hdr.ChannelConfig)
	cfg := aacparser.MPEG4AudioConfig{
		ObjectType:      2, // AAC-LC
		SampleRate:      freq,
		SampleRateIndex: srIdx,
		ChannelConfig:   chCfg,
	}
	cfg.Complete()
	return cfg, nil
}

// aacSampleRateIndex returns the MPEG-4 Audio sample rate index for a given Hz value.
func aacSampleRateIndex(hz int) uint {
	table := []int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}
	for i, r := range table {
		if hz == r {
			return uint(i)
		}
	}
	return 4 // default to 44100
}

// serveRTMPPush is the publisher goroutine for one outbound PushDestination.
// It creates a fresh packager session per connection attempt and reconnects on error.
//
// On input discontinuity (source switch / failover), the current session is torn
// down and a new one starts immediately with fresh codec probing.  This ensures
// the RTMP connection is re-established with codec headers that match the new
// source, even if the resolution, bitrate, or codec parameters differ.
func (s *Service) serveRTMPPush(
	ctx context.Context,
	streamID domain.StreamCode,
	mediaBufferID domain.StreamCode,
	dest domain.PushDestination,
) {
	if !strings.HasPrefix(dest.URL, "rtmp://") && !strings.HasPrefix(dest.URL, "rtmps://") {
		slog.Warn("publisher: RTMP push unsupported scheme (only rtmp:// and rtmps:// supported)",
			"stream_code", streamID, "url", dest.URL)
		return
	}

	retryDelay := time.Duration(dest.RetryTimeoutSec) * time.Second
	if retryDelay <= 0 {
		retryDelay = 5 * time.Second
	}

	attempts := 0
	var preserved *rtmpPushPackager // codec data reused across reconnects

	for {
		if ctx.Err() != nil {
			return
		}
		if dest.Limit > 0 && attempts >= dest.Limit {
			slog.Warn("publisher: RTMP push reached retry limit",
				"stream_code", streamID, "url", dest.URL, "limit", dest.Limit)
			return
		}
		attempts++

		slog.Info("publisher: RTMP push session starting",
			"stream_code", streamID, "url", dest.URL,
			"attempt", attempts, "preserved_codec", preserved != nil)

		p, sessionErr := s.runOnePushSession(ctx, streamID, mediaBufferID, dest, preserved)

		if ctx.Err() != nil {
			return
		}

		// Input switched — restart immediately with preserved codec data so the
		// new session can connect on the very first frame from the new source
		// instead of waiting to re-probe SPS/PPS + AAC config.  This minimises
		// the publish gap when switching between mirrors / backup feeds of the
		// same channel (the common case).
		//
		// If the new source actually uses different codec parameters, viewers
		// may see a brief decoder glitch until the next IDR's in-band SPS/PPS
		// allows decoders to resync — accepted trade-off for minimum downtime.
		if errors.Is(sessionErr, errDiscontinuity) {
			preservedCodec := p != nil && p.h264CD != nil
			slog.Info("publisher: RTMP push input discontinuity, restarting session",
				"stream_code", streamID, "url", dest.URL,
				"preserved_codec", preservedCodec)
			preserved = p
			attempts = 0
			continue
		}

		preserved = p

		if sessionErr != nil {
			slog.Warn("publisher: RTMP push session ended, retrying",
				"stream_code", streamID, "url", dest.URL, "err", sessionErr,
				"retry_in", retryDelay)
		} else {
			slog.Info("publisher: RTMP push session ended cleanly, retrying",
				"stream_code", streamID, "url", dest.URL,
				"retry_in", retryDelay)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(retryDelay):
		}
	}
}

// runOnePushSession subscribes to the media buffer, runs one packager session,
// then unsubscribes.  Returns the packager (for codec reuse on the next attempt)
// and any session error.
func (s *Service) runOnePushSession(
	ctx context.Context,
	streamID domain.StreamCode,
	mediaBufferID domain.StreamCode,
	dest domain.PushDestination,
	preserved *rtmpPushPackager,
) (*rtmpPushPackager, error) {
	sub, err := s.buf.Subscribe(mediaBufferID)
	if err != nil {
		slog.Warn("publisher: RTMP push subscribe failed",
			"stream_code", streamID, "url", dest.URL, "err", err)
		return preserved, err
	}

	p := &rtmpPushPackager{
		streamID:   streamID,
		url:        dest.URL,
		timeoutSec: dest.TimeoutSec,
	}
	// Reuse codec data from previous session so reconnect is fast.
	if preserved != nil {
		p.h264CD = preserved.h264CD
		p.aacCD = preserved.aacCD
		p.aacCfg = preserved.aacCfg
	}

	sessionErr := p.run(ctx, sub)
	s.buf.Unsubscribe(mediaBufferID, sub)
	return p, sessionErr
}
