package pull

import (
	"context"
	"io"
	"sync"
	"sync/atomic"

	gocodec "github.com/yapingcat/gomedia/go-codec"
	gompeg2 "github.com/yapingcat/gomedia/go-mpeg2"

	"github.com/ntthuan060102github/open-streamer/internal/domain"
)

// TSDemuxPacketReader wraps a byte-level TS Reader and emits domain.AVPacket values.
type TSDemuxPacketReader struct {
	r TSChunkReader

	mu      sync.Mutex
	started bool
	pr      *io.PipeReader
	pw      *io.PipeWriter
	q       chan domain.AVPacket

	readLoopDone chan struct{}
	demuxDone    chan struct{}

	// shutdown is closed during Close so OnFrame can exit if blocked on q (must not drop frames under load).
	shutdown     chan struct{}
	shutSignaled atomic.Bool
}

// TSChunkReader is an MPEG-TS byte source (UDP, HLS, file, SRT, RTMP pull mux, …).
type TSChunkReader interface {
	Open(ctx context.Context) error
	Read(ctx context.Context) ([]byte, error)
	Close() error
}

// NewTSDemuxPacketReader wraps a TSChunkReader and emits domain.AVPacket values.
func NewTSDemuxPacketReader(r TSChunkReader) *TSDemuxPacketReader {
	return &TSDemuxPacketReader{r: r}
}

// Open starts pipe pump + TS demuxer goroutines.
func (d *TSDemuxPacketReader) Open(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started {
		return nil
	}
	if err := d.r.Open(ctx); err != nil {
		return err
	}
	d.shutSignaled.Store(false)
	d.shutdown = make(chan struct{})
	d.pr, d.pw = io.Pipe()
	d.q = make(chan domain.AVPacket, 16384)
	d.readLoopDone = make(chan struct{})
	d.demuxDone = make(chan struct{})
	d.started = true

	go d.pumpChunks(ctx)
	go d.runDemux()
	return nil
}

func (d *TSDemuxPacketReader) pumpChunks(ctx context.Context) {
	defer close(d.readLoopDone)
	defer func() { _ = d.pw.Close() }()
	for {
		if ctx.Err() != nil {
			return
		}
		chunk, rerr := d.r.Read(ctx)
		if len(chunk) > 0 {
			if _, werr := d.pw.Write(chunk); werr != nil {
				return
			}
		}
		if rerr != nil {
			return
		}
	}
}

func (d *TSDemuxPacketReader) runDemux() {
	defer close(d.demuxDone)
	defer close(d.q)

	demux := gompeg2.NewTSDemuxer()
	demux.OnFrame = func(cid gompeg2.TS_STREAM_TYPE, frame []byte, pts, dts uint64) {
		if len(frame) == 0 {
			return
		}
		var cdc domain.AVCodec
		switch cid {
		case gompeg2.TS_STREAM_H264:
			cdc = domain.AVCodecH264
		case gompeg2.TS_STREAM_H265:
			cdc = domain.AVCodecH265
		case gompeg2.TS_STREAM_AAC:
			cdc = domain.AVCodecAAC
		default:
			return
		}
		p := domain.AVPacket{
			Codec: cdc,
			Data:  append([]byte(nil), frame...),
			PTSms: pts,
			DTSms: dts,
		}
		switch cdc {
		case domain.AVCodecH264:
			p.KeyFrame = gocodec.IsH264IDRFrame(frame)
		case domain.AVCodecH265:
			p.KeyFrame = gocodec.IsH265IDRFrame(frame)
		}
		sh := d.shutdown
		if sh == nil {
			return
		}
		select {
		case d.q <- p:
		case <-sh:
			return
		}
	}
	_ = demux.Input(d.pr)
}

// ReadPackets blocks until at least one AVPacket is available or the source ends.
func (d *TSDemuxPacketReader) ReadPackets(ctx context.Context) ([]domain.AVPacket, error) {
	d.mu.Lock()
	q := d.q
	d.mu.Unlock()
	if q == nil {
		return nil, io.EOF
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case p, ok := <-q:
		if !ok {
			return nil, io.EOF
		}
		batch := []domain.AVPacket{p}
		for len(batch) < 256 {
			select {
			case p2, ok2 := <-q:
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

// Close tears down demux and the underlying TS reader.
func (d *TSDemuxPacketReader) Close() error {
	d.mu.Lock()
	if !d.started {
		d.mu.Unlock()
		return d.r.Close()
	}
	d.started = false
	pw := d.pw
	sh := d.shutdown
	d.pw = nil
	d.pr = nil
	d.q = nil
	d.shutdown = nil
	d.mu.Unlock()

	// Unblock demux OnFrame if it is waiting on q, before closing the pipe.
	if sh != nil && d.shutSignaled.CompareAndSwap(false, true) {
		close(sh)
	}

	if pw != nil {
		_ = pw.Close()
	}
	if d.readLoopDone != nil {
		<-d.readLoopDone
	}
	if d.demuxDone != nil {
		<-d.demuxDone
	}
	return d.r.Close()
}
