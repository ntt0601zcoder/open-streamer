package publisher

// push_rtmp.go — outbound RTMP/RTMPS re-stream (push to external endpoint).
//
// Data flow:
//
//	Buffer Hub → tsmux.FeedWirePacket → tsBuffer → mpeg2.TSDemuxer
//	                                                        │
//	                                        H.264 Annex-B + AAC ADTS
//	                                                        │
//	                          gomedia go-rtmp publish → remote RTMP(S) server
//
// Connection: dial TCP (or TLS for rtmps://) → drive the gomedia RtmpClient
// state machine (handshake → connect → releaseStream/FCPublish → createStream
// → publish).  The session is considered ready only after the server replies
// with onStatus("NetStream.Publish.Start") — we do not write any media before
// that.  This is the FMLE handshake YouTube/Twitch require; without it some
// ingest endpoints accept the TCP connection, send a 12-byte ack, then drop.
//
// Frames that arrive before the connection is ready are held in a bounded
// pending queue.  When publish.start fires, the queue is flushed starting from
// the first H.264 keyframe — earlier frames are discarded so the remote
// decoder always seeds from an IDR.
//
// Reconnect: on any dial, handshake, or write error the session ends; the
// caller (serveRTMPPush) waits RetryTimeoutSec and retries with a fresh
// session.  gomedia's AVCMuxer extracts SPS/PPS from each new stream itself,
// so no codec state needs to be preserved across reconnects.
//
// Input switching: when the ingestor switches to a different input source it
// marks the first packet with Discontinuity=true.  feedLoop detects this and
// signals the session to end via errDiscontinuity.  serveRTMPPush then starts
// a fresh session with no retry delay so the new source can take over with
// minimum gap.
//
// RTMPS: rtmps:// URLs use a TLS-wrapped connection (default port 443).  The
// RTMP framing layer is identical — gomedia just writes to / reads from the
// net.Conn we hand it via SetOutput + the reader goroutine.

import (
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

	"github.com/yapingcat/gomedia/go-codec"
	mpeg2 "github.com/yapingcat/gomedia/go-mpeg2"
	rtmp "github.com/yapingcat/gomedia/go-rtmp"

	"github.com/ntt0601zcoder/open-streamer/internal/buffer"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
	"github.com/ntt0601zcoder/open-streamer/internal/tsmux"
)

const (
	// rtmpPushPendingMax caps the pending frame queue while waiting for publish.start.
	rtmpPushPendingMax = 300
)

// errDiscontinuity signals that the input source changed (failover / manual
// switch).  The session must be torn down and restarted because the new source
// may have different codec parameters.
var errDiscontinuity = errors.New("input discontinuity")

// rtmpPendingFrame is one queued frame held until publish.start fires.
type rtmpPendingFrame struct {
	cid   codec.CodecID
	data  []byte
	pts   uint32
	dts   uint32
	isKey bool // video keyframe; false for audio
}

// rtmpPushPackager holds state for one outbound RTMP push session.
type rtmpPushPackager struct {
	streamID   domain.StreamCode
	url        string
	timeoutSec int

	// Connection state.
	netConn net.Conn
	cli     *rtmp.RtmpClient
	writeMu sync.Mutex // serialises writes to netConn (output cb + WriteFrame)

	// Lifecycle signals.
	ready    atomic.Bool   // true once NetStream.Publish.Start arrives
	readyCh  chan struct{} // closed on ready
	failedCh chan error    // first server-side / handshake failure

	// Pending frames buffered before publish.start.
	pending []rtmpPendingFrame

	// Base DTS for relative timestamps.
	baseDTS     uint64
	baseDTSSet  bool
	baseADTS    uint64
	baseADTSSet bool

	// connErr holds the first session failure.  failOnce ensures the first
	// error wins; subsequent failures are dropped.
	connErr   atomic.Pointer[error]
	failOnce  sync.Once
	closeOnce sync.Once

	// tsBuf is the TS buffer fed by feedLoop and read by the demuxer.  Stored
	// on the packager so other goroutines (onTSFrame, readLoop) can close it
	// on connection failure to unblock the demuxer.
	tsBuf *tsBuffer

	// gotDiscontinuity is set by feedLoop when it receives a packet with
	// Discontinuity=true while the session is already publishing.  run()
	// returns errDiscontinuity to the caller so a fresh session starts.
	gotDiscontinuity atomic.Bool
}

// failConn marks the session as failed and closes the connection so the
// reader/demuxer goroutines unblock and run() can exit.  Idempotent.
func (p *rtmpPushPackager) failConn(err error) {
	p.failOnce.Do(func() {
		p.connErr.Store(&err)
		p.closeConn()
		if p.tsBuf != nil {
			p.tsBuf.Close()
		}
	})
}

// run is the entry point: dials and waits for publish.start, then drives the
// TS demuxer that feeds frames to the RTMP client.  Returns when ctx is
// cancelled, the input ends, or a write/server error occurs.
func (p *rtmpPushPackager) run(ctx context.Context, sub *buffer.Subscriber) error {
	p.readyCh = make(chan struct{})
	p.failedCh = make(chan error, 4)

	if err := p.connect(ctx); err != nil {
		return err
	}

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
	case err := <-p.failedCh:
		p.failConn(err)
		<-demuxDone
	}

	p.closeConn()

	if p.gotDiscontinuity.Load() {
		slog.Info("publisher: RTMP push session ending on discontinuity",
			"stream_code", p.streamID, "url", p.url, "was_ready", p.ready.Load())
		return errDiscontinuity
	}
	if errPtr := p.connErr.Load(); errPtr != nil {
		return *errPtr
	}
	return nil
}

// connect dials the RTMP/RTMPS endpoint, drives the gomedia client through
// handshake + connect + publish, and returns once the server replies with
// NetStream.Publish.Start.  All subsequent state (incoming server messages,
// failure detection) is handled by the reader goroutine started here.
func (p *rtmpPushPackager) connect(ctx context.Context) error {
	timeout := time.Duration(p.timeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	u, err := url.Parse(p.url)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	host := u.Host
	if _, _, splitErr := net.SplitHostPort(host); splitErr != nil {
		switch u.Scheme {
		case "rtmps":
			host += ":443"
		default:
			host += ":1935"
		}
	}

	slog.Info("publisher: RTMP push connecting",
		"stream_code", p.streamID, "url", p.url)

	dialer := &net.Dialer{Timeout: timeout}
	dialCtx, dialCancel := context.WithTimeout(ctx, timeout)
	defer dialCancel()

	var nc net.Conn
	switch u.Scheme {
	case "rtmps":
		td := &tls.Dialer{
			NetDialer: dialer,
			Config:    &tls.Config{ServerName: u.Hostname()},
		}
		nc, err = td.DialContext(dialCtx, "tcp", host)
		if err != nil {
			return fmt.Errorf("tls dial: %w", err)
		}
	default:
		nc, err = dialer.DialContext(dialCtx, "tcp", host)
		if err != nil {
			return fmt.Errorf("tcp dial: %w", err)
		}
	}
	p.netConn = nc

	cli := rtmp.NewRtmpClient(
		rtmp.WithComplexHandshake(),
		rtmp.WithComplexHandshakeSchema(rtmp.HANDSHAKE_COMPLEX_SCHEMA1),
		rtmp.WithEnablePublish(),
	)
	cli.SetOutput(func(data []byte) error {
		p.writeMu.Lock()
		defer p.writeMu.Unlock()
		_, werr := p.netConn.Write(data)
		return werr
	})
	cli.OnStateChange(func(s rtmp.RtmpState) {
		switch s {
		case rtmp.STATE_RTMP_PUBLISH_START:
			p.ready.Store(true)
			close(p.readyCh)
		case rtmp.STATE_RTMP_PUBLISH_FAILED:
			select {
			case p.failedCh <- errors.New("rtmp publish failed"):
			default:
			}
		}
	})
	cli.OnStatus(func(code, level, describe string) {
		slog.Info("publisher: RTMP push status",
			"stream_code", p.streamID, "url", p.url,
			"code", code, "level", level, "description", describe)
	})
	cli.OnError(func(code, describe string) {
		slog.Warn("publisher: RTMP push server error",
			"stream_code", p.streamID, "url", p.url,
			"code", code, "description", describe)
	})
	p.cli = cli

	// gomedia's Start parses an "rtmp://host/app/stream" URL itself to derive
	// the tcUrl, app, and stream name.  rtmps:// shares the same logical URL
	// structure — only the transport differs — so we feed Start the rtmp://
	// form regardless of scheme.  The TLS connection we already established
	// is what carries the bytes.
	rtmpURL := "rtmp://" + host + u.Path
	if u.RawQuery != "" {
		rtmpURL += "?" + u.RawQuery
	}
	cli.Start(rtmpURL)

	go p.readLoop()

	select {
	case <-p.readyCh:
		slog.Info("publisher: RTMP push publish started",
			"stream_code", p.streamID, "url", p.url,
			"local_addr", connLocalAddr(nc))
		return nil
	case err := <-p.failedCh:
		_ = nc.Close()
		return fmt.Errorf("publish failed: %w", err)
	case <-time.After(timeout):
		_ = nc.Close()
		return fmt.Errorf("publish.start timeout after %s", timeout)
	case <-ctx.Done():
		_ = nc.Close()
		return ctx.Err()
	}
}

// readLoop pumps server-to-client bytes into the gomedia client.  It is the
// only reader of netConn; it terminates on EOF or any read error, which is
// also how server-initiated close is detected.
func (p *rtmpPushPackager) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := p.netConn.Read(buf)
		if n > 0 {
			if inErr := p.cli.Input(buf[:n]); inErr != nil {
				p.failConn(fmt.Errorf("rtmp parse: %w", inErr))
				return
			}
		}
		if err != nil {
			p.failConn(fmt.Errorf("server closed connection: %w", err))
			return
		}
	}
}

// feedLoop reads packets from sub, reassembles 188-byte TS packets via
// alignedFeed, and pipes them into tb.  Runs in its own goroutine.
//
// Discontinuity handling: the ingestor flags the first packet of every
// (re)connect with Discontinuity=true — it has no notion of source switch vs.
// initial start vs. transient reconnect, that's by design.  The publisher
// decides what to do:
//
//   - If we've already published media (ready==true), the remote endpoint has
//     state tied to the current codec parameters; tear the session down and
//     reconnect with fresh probing.  This is the source-switch / failover path.
//   - If we haven't published anything yet (no connection, or pre-publish.start),
//     there is nothing to tear down.  Absorb the flag silently and keep
//     processing — typical for the very first packet of a stream and for
//     reconnect-within-same-source bursts before we get to publish.start.
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
			if pkt.AV != nil && pkt.AV.Discontinuity && p.ready.Load() {
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
		// H.265 over RTMP requires Enhanced RTMP; gomedia supports it but
		// most ingest servers (YouTube/Twitch) don't accept it via push.
		// Skip silently to keep parity with the previous joy4-based code.
		return

	case mpeg2.TS_STREAM_AAC:
		if len(frame) == 0 {
			return
		}
		p.handleAudioFrame(frame, dts)

	default:
		// MP3 / private streams not supported for RTMP push.
		return
	}
}

func (p *rtmpPushPackager) handleVideoFrame(frame []byte, pts, dts uint64) {
	if !p.baseDTSSet {
		p.baseDTS = dts
		p.baseDTSSet = true
	}
	elapsed := int64(dts) - int64(p.baseDTS)
	if elapsed < 0 {
		elapsed = 0
	}
	cto := int64(pts) - int64(dts)
	if cto < 0 {
		cto = 0
	}

	pkt := rtmpPendingFrame{
		cid:   codec.CODECID_VIDEO_H264,
		data:  append([]byte(nil), frame...),
		pts:   uint32(elapsed + cto),
		dts:   uint32(elapsed),
		isKey: tsmux.KeyFrameH264(frame),
	}
	p.enqueueOrWrite(pkt)
}

func (p *rtmpPushPackager) handleAudioFrame(frame []byte, dts uint64) {
	if !p.baseADTSSet {
		p.baseADTS = dts
		p.baseADTSSet = true
	}
	elapsed := int64(dts) - int64(p.baseADTS)
	if elapsed < 0 {
		elapsed = 0
	}

	pkt := rtmpPendingFrame{
		cid:   codec.CODECID_AUDIO_AAC,
		data:  append([]byte(nil), frame...), // ADTS-prefixed; gomedia AACMuxer splits + strips header
		pts:   uint32(elapsed),
		dts:   uint32(elapsed),
		isKey: true,
	}
	p.enqueueOrWrite(pkt)
}

// enqueueOrWrite either buffers the frame (before publish.start) or writes
// it directly via the gomedia client.
func (p *rtmpPushPackager) enqueueOrWrite(pkt rtmpPendingFrame) {
	if !p.ready.Load() {
		if len(p.pending) >= rtmpPushPendingMax {
			p.pending = p.pending[1:] // drop oldest
		}
		p.pending = append(p.pending, pkt)
		return
	}

	if len(p.pending) > 0 {
		// publish.start fired between the last queue check and this frame —
		// drain the backlog before sending the current packet so ordering
		// is preserved.
		p.flushPending()
	}

	if err := p.writeFrame(pkt); err != nil {
		slog.Warn("publisher: RTMP push write error",
			"stream_code", p.streamID, "url", p.url, "err", err,
			"local_addr", connLocalAddr(p.netConn))
		p.failConn(err)
	}
}

// flushPending writes queued frames starting from the first video keyframe.
// Frames before the first keyframe are discarded — sending non-IDR video to
// a freshly-connected ingest server makes the remote decoder unable to seed
// prediction and many servers respond by closing the connection.
func (p *rtmpPushPackager) flushPending() {
	start := -1
	for i, f := range p.pending {
		if f.cid == codec.CODECID_VIDEO_H264 && f.isKey {
			start = i
			break
		}
	}
	if start < 0 {
		// No keyframe in queue yet — keep buffering audio with the queue
		// reset so we don't grow without bound.  enqueueOrWrite will already
		// drop the oldest if we hit the cap, so just leave pending in place
		// until a keyframe finally arrives.
		return
	}
	for _, f := range p.pending[start:] {
		if err := p.writeFrame(f); err != nil {
			slog.Warn("publisher: RTMP push flush error",
				"stream_code", p.streamID, "url", p.url, "err", err,
				"local_addr", connLocalAddr(p.netConn))
			p.failConn(err)
			break
		}
	}
	p.pending = p.pending[:0]
}

// writeFrame dispatches one queued frame to the gomedia client.  WriteFrame
// performs FLV tag muxing internally and calls the registered output callback,
// which writes to netConn under writeMu.
func (p *rtmpPushPackager) writeFrame(f rtmpPendingFrame) error {
	return p.cli.WriteFrame(f.cid, f.data, f.pts, f.dts)
}

// closeConn closes the underlying net.Conn exactly once.  Safe to call from
// multiple goroutines (failConn from readLoop, plus run() at end).
func (p *rtmpPushPackager) closeConn() {
	p.closeOnce.Do(func() {
		if p.netConn != nil {
			_ = p.netConn.Close()
		}
		p.ready.Store(false)
	})
}

// connLocalAddr returns the local TCP address of the connection (best-effort,
// for log correlation with kernel error messages like "write tcp <local>...").
func connLocalAddr(c net.Conn) string {
	if c == nil {
		return ""
	}
	if a := c.LocalAddr(); a != nil {
		return a.String()
	}
	return ""
}

// serveRTMPPush is the publisher goroutine for one outbound PushDestination.
// It creates a fresh packager session per connection attempt and reconnects on error.
//
// On input discontinuity (source switch / failover), the current session is torn
// down and a new one starts immediately.  gomedia's AVCMuxer extracts SPS/PPS
// from the new stream itself, so no codec state needs to be carried across the
// restart.
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
			"stream_code", streamID, "url", dest.URL, "attempt", attempts)

		sessionErr := s.runOnePushSession(ctx, streamID, mediaBufferID, dest)

		if ctx.Err() != nil {
			return
		}

		if errors.Is(sessionErr, errDiscontinuity) {
			slog.Info("publisher: RTMP push input discontinuity, restarting session",
				"stream_code", streamID, "url", dest.URL)
			attempts = 0
			continue
		}

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
// then unsubscribes.
func (s *Service) runOnePushSession(
	ctx context.Context,
	streamID domain.StreamCode,
	mediaBufferID domain.StreamCode,
	dest domain.PushDestination,
) error {
	sub, err := s.buf.Subscribe(mediaBufferID)
	if err != nil {
		slog.Warn("publisher: RTMP push subscribe failed",
			"stream_code", streamID, "url", dest.URL, "err", err)
		return err
	}

	p := &rtmpPushPackager{
		streamID:   streamID,
		url:        dest.URL,
		timeoutSec: dest.TimeoutSec,
	}

	sessionErr := p.run(ctx, sub)
	s.buf.Unsubscribe(mediaBufferID, sub)
	return sessionErr
}
