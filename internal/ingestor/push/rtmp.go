// Package push contains push-mode server implementations.
// Each server listens on a port and routes incoming connections to the
// correct stream's Buffer Hub slot via the Registry interface.
package push

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	gocodec "github.com/yapingcat/gomedia/go-codec"
	gompeg2 "github.com/yapingcat/gomedia/go-mpeg2"
	gortmp "github.com/yapingcat/gomedia/go-rtmp"

	"github.com/ntthuan060102github/open-streamer/config"
	"github.com/ntthuan060102github/open-streamer/internal/buffer"
	"github.com/ntthuan060102github/open-streamer/internal/domain"
)

// RTMPServer is a single-port RTMP push server.
// External encoders (OBS, hardware encoders, FFmpeg) connect to:
//
//	rtmp://<host>:<port>/<app>/<stream_key>
//
// Each accepted connection is handled in its own goroutine.
// Incoming FLV audio/video is converted to MPEG-TS in-process via
// yapingcat/gomedia — no external FFmpeg process is spawned.
type RTMPServer struct {
	cfg      config.IngestorConfig
	registry Registry
}

// NewRTMPServer constructs an RTMPServer.
func NewRTMPServer(cfg config.IngestorConfig, registry Registry) *RTMPServer {
	return &RTMPServer{cfg: cfg, registry: registry}
}

// ListenAndServe starts the TCP listener and blocks until ctx is cancelled.
// Returns nil on clean shutdown; returns a non-nil error on bind failure.
func (s *RTMPServer) ListenAndServe(ctx context.Context) error {
	addr := s.cfg.RTMPAddr
	if addr == "" {
		addr = ":1935"
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("rtmp server: listen %s: %w", addr, err)
	}
	defer func() { _ = ln.Close() }()

	slog.Info("rtmp server: listening", "addr", addr)

	// Close the listener when the server context is done so Accept unblocks.
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil //nolint:nilerr // clean shutdown while listener is closing
			}
			slog.Error("rtmp server: accept error", "err", err)
			continue
		}
		go s.handleConn(ctx, conn)
	}
}

// handleConn runs an rtmpSession for one accepted TCP connection.
func (s *RTMPServer) handleConn(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	sess := &rtmpSession{
		conn:     conn,
		registry: s.registry,
	}
	sess.run(ctx)
}

// ─── Session ─────────────────────────────────────────────────────────────────

// rtmpSession manages one RTMP TCP connection lifecycle.
//
// # Thread-safety
//
// The gomedia RtmpServerHandle calls all registered callbacks (OnPublish,
// OnFrame) synchronously inside handle.Input(), which is itself called from
// the single readLoop goroutine.  There is therefore no concurrent access to
// any session field and no mutex is needed.
//
// # RTMP → MPEG-TS conversion
//
// Incoming FLV frames (H.264 / H.265 / AAC) are fed into a gompeg2.TSMuxer
// which emits aligned 188-byte MPEG-TS packets.  Those packets are forwarded
// directly to the Buffer Hub as TSPacket values, matching the format produced
// by the SRT push server and expected by the HLS / DASH publishers.
//
// # One-pusher policy
//
// Registry.Acquire is called in onPublish.  If another encoder is already
// connected to the same stream, Acquire returns ErrStreamAlreadyActive and
// the connection is rejected.  Registry.Release is called when run() returns
// so the slot is freed for the next encoder.
type rtmpSession struct {
	conn     net.Conn
	registry Registry

	// gomedia objects, initialised in run().
	handle *gortmp.RtmpServerHandle
	mux    *gompeg2.TSMuxer

	// populated after Acquire succeeds in onPublish.
	streamKey     string            // registry key; non-empty iff Acquire succeeded
	streamID      domain.StreamCode
	bufferWriteID domain.StreamCode
	buf           *buffer.Service

	// track which elementary streams have been added to the TS muxer so that
	// each AddStream call happens exactly once per session.
	videoPid uint16
	audioPid uint16
	hasVideo bool
	hasAudio bool
}

// run initialises the RTMP server handle and processes incoming data until the
// connection closes or ctx is cancelled.
func (s *rtmpSession) run(ctx context.Context) {
	s.initMuxer()
	s.initHandle()

	// When the server context is cancelled (graceful shutdown), close the
	// underlying TCP connection so that readLoop's conn.Read unblocks
	// immediately instead of waiting for the next data or idle timeout.
	cancelConn := context.AfterFunc(ctx, func() { _ = s.conn.Close() })
	defer cancelConn()

	s.readLoop()

	// Release the registry slot only if Acquire succeeded in onPublish.
	if s.streamKey != "" {
		s.registry.Release(s.streamKey)
		slog.Info("rtmp: publisher disconnected",
			"stream_key", s.streamKey,
			"stream_code", s.streamID,
			"remote", s.conn.RemoteAddr(),
		)
	}
}

// initMuxer sets up the TSMuxer whose OnPacket callback forwards completed
// MPEG-TS packets to the Buffer Hub.
func (s *rtmpSession) initMuxer() {
	s.mux = gompeg2.NewTSMuxer()
	s.mux.OnPacket = func(pkg []byte) {
		if s.buf == nil {
			return
		}
		pkt := make([]byte, len(pkg))
		copy(pkt, pkg)
		if err := s.buf.Write(s.bufferWriteID, buffer.TSPacket(pkt)); err != nil {
			slog.Error("rtmp: buffer write failed",
				"stream_code", s.streamID,
				"err", err,
			)
		}
	}
}

// initHandle wires the gomedia RtmpServerHandle callbacks.
func (s *rtmpSession) initHandle() {
	s.handle = gortmp.NewRtmpServerHandle()
	s.handle.SetOutput(func(b []byte) error {
		_, err := s.conn.Write(b)
		return err
	})
	s.handle.OnPublish(s.onPublish)
	s.handle.OnFrame(s.onFrame)
}

// onPublish is called by the RTMP library when the encoder sends a PUBLISH
// command.  The first argument (app name) is intentionally ignored — encoders
// only need to supply the stream code as the stream name.
func (s *rtmpSession) onPublish(_, streamName string) gortmp.StatusCode {
	writeID, streamID, buf, err := s.registry.Acquire(streamName)
	if err != nil {
		if errors.Is(err, ErrStreamAlreadyActive) {
			slog.Warn("rtmp: rejected, stream already has an active pusher",
				"key", streamName,
				"remote", s.conn.RemoteAddr(),
			)
		} else {
			slog.Warn("rtmp: rejected unknown stream code",
				"key", streamName,
				"remote", s.conn.RemoteAddr(),
			)
		}
		return gortmp.NETCONNECT_CONNECT_REJECTED
	}

	s.streamKey = streamName
	s.streamID = streamID
	s.bufferWriteID = writeID
	s.buf = buf

	slog.Info("rtmp: publisher connected",
		"stream_key", streamName,
		"stream_code", streamID,
		"remote", s.conn.RemoteAddr(),
	)
	return gortmp.NETSTREAM_PUBLISH_START
}

// onFrame is called by the RTMP library for each decoded audio/video frame.
// It routes the frame into the TS muxer based on its codec ID.
func (s *rtmpSession) onFrame(cid gocodec.CodecID, pts, dts uint32, frame []byte) {
	switch cid { //nolint:exhaustive // push path supports H.264, H.265, and AAC only
	case gocodec.CODECID_VIDEO_H264:
		if !s.hasVideo {
			s.videoPid = s.mux.AddStream(gompeg2.TS_STREAM_H264)
			s.hasVideo = true
		}
		_ = s.mux.Write(s.videoPid, frame, uint64(pts), uint64(dts))

	case gocodec.CODECID_VIDEO_H265:
		if !s.hasVideo {
			s.videoPid = s.mux.AddStream(gompeg2.TS_STREAM_H265)
			s.hasVideo = true
		}
		_ = s.mux.Write(s.videoPid, frame, uint64(pts), uint64(dts))

	case gocodec.CODECID_AUDIO_AAC:
		if !s.hasAudio {
			s.audioPid = s.mux.AddStream(gompeg2.TS_STREAM_AAC)
			s.hasAudio = true
		}
		// Audio has no decode delay; DTS equals PTS.
		_ = s.mux.Write(s.audioPid, frame, uint64(pts), uint64(pts))
	}
}

// readLoop reads raw bytes from the TCP connection and feeds them to the RTMP
// library until the connection closes, an error occurs, or ctx fires.
//
// The loop exits via conn.Read returning an error; the context-triggered
// conn.Close (set up in run) ensures we unblock promptly on shutdown.
func (s *rtmpSession) readLoop() {
	buf := make([]byte, 65536)
	for {
		n, err := s.conn.Read(buf)
		if err != nil {
			return
		}
		if inputErr := s.safeInput(buf[:n]); inputErr != nil {
			slog.Warn("rtmp: input error",
				"stream_code", s.streamID,
				"remote", s.conn.RemoteAddr(),
				"err", inputErr,
			)
			return
		}
	}
}

// safeInput calls handle.Input and converts any panic into a returned error.
//
// The gomedia RTMP library may panic on certain malformed RTMP payloads
// (e.g. out-of-range bit-stream reads, unexpected chunk boundaries).
// Recovering the panic here prevents a single bad encoder from crashing the
// entire server process.
func (s *rtmpSession) safeInput(data []byte) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("rtmp handle panic: %v", r)
		}
	}()
	return s.handle.Input(data)
}
