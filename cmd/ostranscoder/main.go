//go:build libav

package main

/*
#cgo pkg-config: libavcodec
#include <libavcodec/avcodec.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/asticode/go-astiav"

	"github.com/ntt0601zcoder/open-streamer/internal/transcode/native"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/wire"
)

// connFD is the descriptor of the host's socketpair end.
const connFD = 3

// statsInterval is how often the child writes Stats.
const statsInterval = time.Second

// errorRung is Error.Rung for an error not tied to one rung.
const errorRung = -1

// main prints the version with -version, and otherwise serves the wire
// protocol on fd 3: exit 0 after Stop or EOF from the host, 1 on an error,
// 2 on bad usage.
func main() {
	switch {
	case len(os.Args) == 2 && os.Args[1] == "-version":
		fmt.Printf("ostranscoder wire %d libavcodec %s\n", wire.Version, libavcodecVersion())
		return
	case len(os.Args) != 1:
		fmt.Fprintln(os.Stderr, "usage: ostranscoder [-version]")
		os.Exit(2)
	}
	// libav's default log callback writes to stderr.
	astiav.SetLogLevel(astiav.LogLevelWarning)
	conn, err := hostConn()
	if err == nil {
		err = run(conn)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// hostConn wraps fd 3 as a net.Conn, so that closing it unblocks a pending
// read.
func hostConn() (net.Conn, error) {
	f := os.NewFile(connFD, "host")
	if f == nil {
		return nil, fmt.Errorf("ostranscoder: fd %d: not open", connFD)
	}
	c, err := net.FileConn(f)
	_ = f.Close() // FileConn dups the descriptor
	if err != nil {
		return nil, fmt.Errorf("ostranscoder: fd %d: %w", connFD, err)
	}
	return c, nil
}

// libavcodecVersion is the version of the libavcodec loaded at run time, as
// major.minor.micro.
func libavcodecVersion() string {
	v := uint32(C.avcodec_version())
	return fmt.Sprintf("%d.%d.%d", v>>16, v>>8&0xff, v&0xff)
}

// run serves the child side of the wire protocol on conn and closes it
// before returning. It returns nil after Stop or EOF from the host, once the
// pipeline is flushed and its outputs written.
func run(conn io.ReadWriteCloser) error {
	return serve(conn, statsInterval)
}

// readResult is one message, or the error that ends the stream.
type readResult struct {
	m   wire.Message
	err error
}

// serve is run with the Stats interval as a parameter.
func serve(conn io.ReadWriteCloser, every time.Duration) error {
	r := wire.NewReader(conn)
	w := wire.NewWriter(conn)
	msgs := make(chan readResult)
	done := make(chan struct{})
	var wg sync.WaitGroup
	// Closing conn unblocks the reader, and done unblocks its send.
	defer func() {
		close(done)
		_ = conn.Close()
		wg.Wait()
	}()

	if err := w.Write(wire.Hello{Version: wire.Version, PID: os.Getpid()}); err != nil {
		return fmt.Errorf("ostranscoder: hello: %w", err)
	}
	c, err := configure(r, w)
	if err != nil {
		return err
	}
	if c == nil {
		slog.Info("ostranscoder: stopped before configure")
		return nil
	}
	defer c.p.Close()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			m, err := r.Read()
			select {
			case msgs <- readResult{m, err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if err := c.writeStats(); err != nil {
				return err
			}
		case res := <-msgs:
			stop, err := c.handle(res)
			if err != nil || stop {
				return err
			}
		}
	}
}

// configure reads the Configure message and builds the pipeline. Stop or EOF
// instead of Configure, as when the host is cancelled during start-up,
// returns a nil child and no error. On any other failure it writes a fatal
// Error and returns the failure.
func configure(r *wire.Reader, w *wire.Writer) (*child, error) {
	m, err := r.Read()
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	if err != nil {
		return nil, fatal(w, fmt.Errorf("ostranscoder: read configure: %w", err))
	}
	var cfg wire.Configure
	switch v := m.(type) {
	case wire.Configure:
		cfg = v
	case wire.Stop:
		return nil, nil
	default:
		return nil, fatal(w, fmt.Errorf("ostranscoder: configure: got %T first", m))
	}
	rungs := make([]native.Rung, len(cfg.Rungs))
	for i, r := range cfg.Rungs {
		rungs[i] = native.Rung{
			Height: r.Height, BitrateKbps: r.BitrateKbps, MaxBitrateKbps: r.MaxBitrateKbps,
			BFrames: r.BFrames, Preset: r.Preset,
		}
	}
	var audio *native.Audio
	if a := cfg.Audio; a != nil {
		audio = &native.Audio{BitrateKbps: a.BitrateKbps, SampleRate: a.SampleRate, Channels: a.Channels}
	}
	p, err := native.New(rungs, audio)
	if err != nil {
		return nil, fatal(w, fmt.Errorf("ostranscoder: configure: %w", err))
	}
	return &child{
		w: w, p: p,
		out:   make([]uint64, len(rungs)),
		nanos: make([]int64, len(rungs)),
	}, nil
}

// fatal writes err to the host as a fatal Error, best effort, and returns it.
func fatal(w *wire.Writer, err error) error {
	_ = w.Write(wire.Error{Rung: errorRung, Message: err.Error(), Fatal: true})
	return err
}

// child is the state of one served pipeline. Only serve's goroutine uses it.
type child struct {
	w *wire.Writer
	p *native.Pipeline

	in    uint64   // Frame messages received
	out   []uint64 // video frames written, per rung
	busy  int64    // nanoseconds spent in pipeline calls
	nanos []int64  // scratch for Stats.EncodeNanos

	logged native.Stats // native counters last logged
}

// handle processes one read result. It reports stop after Stop or EOF.
func (c *child) handle(res readResult) (stop bool, err error) {
	if res.err != nil {
		if errors.Is(res.err, io.EOF) {
			return true, c.finish()
		}
		return true, fatal(c.w, fmt.Errorf("ostranscoder: read: %w", res.err))
	}
	switch m := res.m.(type) {
	case wire.Frame:
		c.in++
		return false, c.call(func() ([]native.Output, error) { return c.p.Push(m.F) })
	case wire.Discontinuity:
		return false, c.call(c.p.Discontinuity)
	case wire.Stop:
		return true, c.finish()
	default:
		return true, fatal(c.w, fmt.Errorf("ostranscoder: unexpected %T", m))
	}
}

// finish flushes the pipeline, writes its outputs and the final Stats.
func (c *child) finish() error {
	if err := c.call(c.p.Flush); err != nil {
		return err
	}
	return c.writeStats()
}

// call runs one pipeline call, timing it, and writes its outputs. A pipeline
// error is written to the host as a fatal Error after the outputs that came
// with it.
func (c *child) call(fn func() ([]native.Output, error)) error {
	start := time.Now()
	outs, perr := fn()
	c.busy += int64(time.Since(start))
	for _, o := range outs {
		if err := c.w.Write(wire.Frame{Rung: o.Rung, F: o.F}); err != nil {
			return fmt.Errorf("ostranscoder: write frame: %w", err)
		}
		if o.Rung >= 0 && o.Rung < len(c.out) {
			c.out[o.Rung]++
		}
	}
	if perr != nil {
		return fatal(c.w, perr)
	}
	return nil
}

// writeStats writes the counters, and logs the native ones when they
// changed: wire.Stats has no field for them.
func (c *child) writeStats() error {
	// The rungs are encoded one after another in each call, after a shared
	// decode, so the time spent is not split per rung: every rung reports
	// the pipeline's total.
	for i := range c.nanos {
		c.nanos[i] = c.busy
	}
	if err := c.w.Write(wire.Stats{In: c.in, Out: c.out, EncodeNanos: c.nanos}); err != nil {
		return fmt.Errorf("ostranscoder: write stats: %w", err)
	}
	if st := c.p.Stats(); st != c.logged {
		c.logged = st
		slog.Warn("ostranscoder: pipeline counters",
			"dropped_tracks", st.DroppedTracks, "reanchors", st.Reanchors,
			"audio_dropped_samples", st.AudioDropped,
			"audio_corrupt", st.AudioCorrupt, "video_corrupt", st.VideoCorrupt)
	}
	return nil
}
