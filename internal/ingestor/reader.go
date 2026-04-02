// Package ingestor handles raw stream ingestion.
//
// The ingestor accepts a plain URL from the user and automatically derives
// the protocol from the URL scheme — no protocol configuration is needed.
//
// Pull mode (server connects to remote source). All pull paths yield
// [domain.AVPacket] via [PacketReader] / [NewPacketReader]:
//   - UDP MPEG-TS  → TS demux → AVPacket
//   - HLS playlist → HTTP + M3U8 → TS demux → AVPacket
//   - Local file   → TS demux → AVPacket
//   - SRT pull     → gosrt → TS demux → AVPacket
//   - RTMP pull    → native RTMP → AVPacket
//   - RTSP pull    → gortsplib → AVPacket
//
// Push mode (external encoder connects to our server):
//   - RTMP → RTMPServer (gomedia RTMP server handle, native FLV→MPEG-TS)
//   - SRT  → SRTServer  (gosrt native, MPEG-TS passthrough)
//
// Push mode is auto-detected: if the URL host is 0.0.0.0 / :: and the scheme
// is rtmp or srt, the ingestor starts/uses the shared push server instead of
// a pull worker.
package ingestor

import (
	"context"
	"fmt"

	"github.com/ntthuan060102github/open-streamer/config"
	"github.com/ntthuan060102github/open-streamer/internal/domain"
	"github.com/ntthuan060102github/open-streamer/internal/ingestor/pull"
	"github.com/ntthuan060102github/open-streamer/pkg/protocol"
)

// PacketReader is a pull source that yields elementary-stream access units
// ([domain.AVPacket]).
type PacketReader interface {
	Open(ctx context.Context) error
	ReadPackets(ctx context.Context) ([]domain.AVPacket, error)
	Close() error
}

// NewPacketReader constructs the appropriate PacketReader for the given input URL.
// RTSP and RTMP pull emit native AVPackets; MPEG-TS transports are demuxed to AVPackets.
//
// Returns an error when:
//   - The URL scheme is unrecognised
//   - The URL describes a push-listen address (handled by the push servers)
func NewPacketReader(input domain.Input, cfg config.IngestorConfig) (PacketReader, error) {
	if protocol.IsPushListen(input.URL) {
		return nil, fmt.Errorf(
			"ingestor: %q is a push-listen address — handled by the push server, not a pull reader",
			input.URL,
		)
	}

	switch protocol.Detect(input.URL) {
	case protocol.KindRTSP:
		return pull.NewRTSPReader(input), nil
	case protocol.KindRTMP:
		return pull.NewRTMPReader(input), nil
	case protocol.KindUDP:
		return pull.NewTSDemuxPacketReader(pull.NewUDPReader(input)), nil
	case protocol.KindHLS:
		return pull.NewTSDemuxPacketReader(pull.NewHLSReader(input, cfg)), nil
	case protocol.KindFile:
		return pull.NewTSDemuxPacketReader(pull.NewFileReader(input)), nil
	case protocol.KindSRT:
		return pull.NewTSDemuxPacketReader(pull.NewSRTReader(input)), nil
	default:
		return nil, fmt.Errorf(
			"ingestor: cannot infer protocol from URL %q — unsupported scheme",
			input.URL,
		)
	}
}
