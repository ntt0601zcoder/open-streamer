package pull

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	gomp4 "github.com/yapingcat/gomedia/go-mp4"
	gompeg2 "github.com/yapingcat/gomedia/go-mpeg2"
)

// Demuxer converts some source format into MPEG-TS chunks for the rest of the pipeline.
// Implementations must be used from a single goroutine.
type Demuxer interface {
	Read(ctx context.Context) ([]byte, error)
	Close() error
}

type passthroughDemuxer struct {
	r   io.Reader
	buf []byte
}

func newPassthroughDemuxer(r io.Reader, chunkSize int) Demuxer {
	if chunkSize <= 0 {
		chunkSize = 188 * 56
	}
	return &passthroughDemuxer{
		r:   r,
		buf: make([]byte, chunkSize),
	}
}

func (d *passthroughDemuxer) Read(ctx context.Context) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	n, err := d.r.Read(d.buf)
	if n > 0 {
		out := make([]byte, n)
		copy(out, d.buf[:n])
		return out, nil
	}
	if err == io.EOF {
		return nil, io.EOF
	}
	return nil, err
}

func (d *passthroughDemuxer) Close() error { return nil }

type mp4ToTSDemuxer struct {
	rs io.ReadSeeker

	demux *gomp4.MovDemuxer
	mux   *gompeg2.TSMuxer

	queue [][]byte
	vpid  uint16
	apid  uint16
	vset  bool
	aset  bool

	loop bool

	// pace timestamps to wall clock (MP4 demuxer returns ms-based timestamps).
	started bool
	startDTS uint64
	startAt  time.Time
}

func newMP4ToTSDemuxer(rs io.ReadSeeker, loop bool) (*mp4ToTSDemuxer, error) {
	d := &mp4ToTSDemuxer{rs: rs, loop: loop}
	d.mux = gompeg2.NewTSMuxer()
	d.mux.OnPacket = func(pkg []byte) {
		out := make([]byte, len(pkg))
		copy(out, pkg)
		d.queue = append(d.queue, out)
	}
	if err := d.reset(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *mp4ToTSDemuxer) reset() error {
	if _, err := d.rs.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("mp4 demux: seek start: %w", err)
	}
	demux := gomp4.CreateMp4Demuxer(d.rs)
	if _, err := demux.ReadHead(); err != nil {
		return fmt.Errorf("mp4 demux: read header: %w", err)
	}
	d.demux = demux
	d.queue = d.queue[:0]
	d.vpid, d.apid = 0, 0
	d.vset, d.aset = false, false
	d.started = false
	return nil
}

func (d *mp4ToTSDemuxer) Read(ctx context.Context) ([]byte, error) {
	for {
		if len(d.queue) > 0 {
			out := d.queue[0]
			d.queue = d.queue[1:]
			return out, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		pkt, err := d.nextPacket()
		if err != nil {
			return nil, err
		}
		if pkt == nil {
			continue
		}

		d.pace(ctx, pkt.Dts)
		d.writePacket(pkt)
	}
}

func (d *mp4ToTSDemuxer) nextPacket() (*gomp4.AVPacket, error) {
	pkt, err := d.demux.ReadPacket()
	if errors.Is(err, io.EOF) {
		if !d.loop {
			return nil, io.EOF
		}
		if err := d.reset(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mp4 demux: read packet: %w", err)
	}
	return pkt, nil
}

func (d *mp4ToTSDemuxer) writePacket(pkt *gomp4.AVPacket) {
	switch pkt.Cid {
	case gomp4.MP4_CODEC_H264:
		if !d.vset {
			d.vpid = d.mux.AddStream(gompeg2.TS_STREAM_H264)
			d.vset = true
		}
		_ = d.mux.Write(d.vpid, pkt.Data, pkt.Pts, pkt.Dts)
	case gomp4.MP4_CODEC_H265:
		if !d.vset {
			d.vpid = d.mux.AddStream(gompeg2.TS_STREAM_H265)
			d.vset = true
		}
		_ = d.mux.Write(d.vpid, pkt.Data, pkt.Pts, pkt.Dts)
	case gomp4.MP4_CODEC_AAC:
		if !d.aset {
			d.apid = d.mux.AddStream(gompeg2.TS_STREAM_AAC)
			d.aset = true
		}
		_ = d.mux.Write(d.apid, pkt.Data, pkt.Pts, pkt.Dts)
	}
}

func (d *mp4ToTSDemuxer) pace(ctx context.Context, dtsMS uint64) {
	if !d.started {
		d.started = true
		d.startDTS = dtsMS
		d.startAt = time.Now()
		return
	}
	target := d.startAt.Add(time.Duration(dtsMS-d.startDTS) * time.Millisecond)
	wait := time.Until(target)
	if wait <= 0 {
		return
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
		return
	}
}

func (d *mp4ToTSDemuxer) Close() error { return nil }

