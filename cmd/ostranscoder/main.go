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
	"io/fs"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/asticode/go-astiav"

	"github.com/ntt0601zcoder/open-streamer/internal/transcode/native"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/wire"
)

// connFD is the descriptor of the host's socketpair end.
const connFD = 3

// statsInterval is how often the child writes Stats.
const statsInterval = time.Second

// errorRung is Error.Rung for an error not tied to one rung.
const errorRung = -1

// exitProbeFailed is the exit status of a probe that failed.
const exitProbeFailed = 3

// main prints the version with -version, probes NVENC with -probe nvenc or
// a font file with -probe font FILE (exit 0, 3, or 1 when the probe itself
// fails), and otherwise serves the wire protocol on fd 3: exit 0 after Stop
// or EOF from the host, 1 on an error, 2 on bad usage.
func main() {
	// libav's default log callback writes to stderr.
	astiav.SetLogLevel(astiav.LogLevelWarning)
	switch {
	case len(os.Args) == 2 && os.Args[1] == "-version":
		fmt.Printf("ostranscoder wire %d libavcodec %s\n", wire.Version, libavcodecVersion())
		return
	case len(os.Args) == 3 && os.Args[1] == "-probe" && os.Args[2] == "nvenc":
		os.Exit(probeNVENC(os.Stdout))
	case len(os.Args) == 4 && os.Args[1] == "-probe" && os.Args[2] == "font":
		os.Exit(probeFont(os.Stdout, os.Stderr, os.Args[3]))
	case len(os.Args) != 1:
		fmt.Fprintln(os.Stderr, "usage: ostranscoder [-version | -probe nvenc | -probe font FILE]")
		os.Exit(2)
	}
	conn, err := hostConn()
	if err == nil {
		err = run(conn)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// probeNVENC opens and closes an NVENC encoder, prints "nvenc: ok" or
// "nvenc: <error>" to w, and returns the exit status: 0, or
// exitProbeFailed.
func probeNVENC(w io.Writer) int {
	if err := native.ProbeNVENC(); err != nil {
		_, _ = fmt.Fprintf(w, "nvenc: %v\n", err)
		return exitProbeFailed
	}
	_, _ = fmt.Fprintln(w, "nvenc: ok")
	return 0
}

// probeFont reads at most picture.MaxFontBytes+1 bytes of the file at path
// (a regular file, not a link), checks them with picture.CheckFont, then
// draws with them (native.ProbeFont), and returns the exit status. Only a
// verdict on the font prints a line to w and exits exitProbeFailed:
// CheckFont's refusal or ProbeFont's *native.FontRefusedError, as
// "font: <reason>". A font that draws prints "font: ok" and exits 0.
// Every other failure (opening or reading the file, which is the host's
// own temp file; the probe's temp directory, writing the font, a
// picture's alloc or fill) says nothing about the font: its text goes to
// errw, nothing to w, and the exit status is 1. No reason printed to w
// holds a path: the host shows it.
func probeFont(w, errw io.Writer, path string) int {
	b, err := readFontFile(path)
	if err != nil {
		_, _ = fmt.Fprintf(errw, "ostranscoder: probe font: %v\n", err)
		return 1
	}
	if err := picture.CheckFont(b); err != nil {
		return refuseFont(w, err)
	}
	err = native.ProbeFont(b)
	if err == nil {
		_, _ = fmt.Fprintln(w, "font: ok")
		return 0
	}
	if refused, ok := errors.AsType[*native.FontRefusedError](err); ok {
		return refuseFont(w, refused.Reason)
	}
	_, _ = fmt.Fprintf(errw, "ostranscoder: %v\n", err)
	return 1
}

// refuseFont prints the verdict "font: <reason>" on one line to w and
// returns exitProbeFailed.
func refuseFont(w io.Writer, err error) int {
	reason := strings.Join(strings.Fields(err.Error()), " ")
	reason = strings.TrimPrefix(reason, "font: ")
	_, _ = fmt.Fprintf(w, "font: %s\n", reason)
	return exitProbeFailed
}

// readFontFile reads at most picture.MaxFontBytes+1 bytes of the regular
// file at path. No error it returns holds path: those of the open and the
// read are replaced by their cause.
func readFontFile(path string) ([]byte, error) {
	f, err := picture.OpenRegular(path)
	if err != nil {
		return nil, withoutPath("cannot open the file", err)
	}
	b, err := io.ReadAll(io.LimitReader(f, picture.MaxFontBytes+1))
	_ = f.Close()
	if err != nil {
		return nil, withoutPath("cannot read the file", err)
	}
	return b, nil
}

// withoutPath is what err, from opening or reading a file, says
// without the file's path: the system's reason (fs.PathError's Err), or
// for the only other failure of picture.OpenRegular, that the file is not
// a regular one.
func withoutPath(what string, err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		if errors.Is(pe.Err, syscall.ELOOP) {
			return fmt.Errorf("%s: it is a symbolic link", what)
		}
		return fmt.Errorf("%s: %w", what, pe.Err)
	}
	return fmt.Errorf("%s: not a regular file", what)
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
			BFrames: r.BFrames, Preset: r.Preset, Encoder: r.Encoder, NoFallback: r.NoFallback,
		}
	}
	var audio *native.Audio
	if a := cfg.Audio; a != nil {
		audio = &native.Audio{BitrateKbps: a.BitrateKbps, SampleRate: a.SampleRate, Channels: a.Channels}
	}
	p, err := native.NewWith(native.Config{
		Rungs: rungs, Audio: audio, Watermark: cfg.Watermark, Thumbnail: cfg.Thumbnail,
	})
	dropFont(cfg.Watermark)
	if err != nil {
		return nil, fatal(w, fmt.Errorf("ostranscoder: configure: %w", err))
	}
	return &child{
		w: w, p: p,
		out:   make([]uint64, len(rungs)),
		nanos: make([]int64, len(rungs)),
	}, nil
}

// dropFont drops a decoded watermark's reference to its font once the
// pipeline has it: the watermarker's is the only one the child keeps.
func dropFont(w *picture.Watermark) {
	if w != nil {
		w.Font = nil
	}
}

// fatal writes err to the host as a fatal Error, best effort, and returns
// it. A NoFallback rung's refusal (*native.RefusedError) is written with
// Code ErrorEncoderRefused and its rung; anything else with ErrorOther and
// no rung.
func fatal(w *wire.Writer, err error) error {
	e := wire.Error{Rung: errorRung, Code: wire.ErrorOther, Message: err.Error(), Fatal: true}
	if refused, ok := errors.AsType[*native.RefusedError](err); ok {
		e.Rung, e.Code = refused.Rung, wire.ErrorEncoderRefused
	}
	_ = w.Write(e)
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
	case wire.SetWatermark:
		// The host validated it: a watermark the child cannot take is
		// fatal (R-C9), so the stream never goes out without it.
		err := c.p.SetWatermark(m.W)
		dropFont(m.W)
		if err != nil {
			return true, fatal(c.w, fmt.Errorf("ostranscoder: %w", err))
		}
		return false, nil
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

// call runs one pipeline call, timing it, and writes its outputs: frames as
// Frame, thumbnails as Shot. A pipeline error is written to the host as a
// fatal Error after the outputs that came with it.
func (c *child) call(fn func() ([]native.Output, error)) error {
	start := time.Now()
	outs, perr := fn()
	c.busy += int64(time.Since(start))
	for _, o := range outs {
		if o.Rung == native.RungShot {
			if err := c.w.Write(wire.Shot{S: *o.Shot}); err != nil {
				return fmt.Errorf("ostranscoder: write shot: %w", err)
			}
			continue
		}
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

// writeStats writes the counters, the native pipeline's included.
func (c *child) writeStats() error {
	// The rungs are encoded one after another in each call, after a shared
	// decode, so the time spent is not split per rung: every rung reports
	// the pipeline's total.
	for i := range c.nanos {
		c.nanos[i] = c.busy
	}
	if err := c.w.Write(c.stats()); err != nil {
		return fmt.Errorf("ostranscoder: write stats: %w", err)
	}
	return nil
}

// stats builds the Stats message from the child's and the pipeline's
// counters.
func (c *child) stats() wire.Stats {
	st := c.p.Stats()
	return wire.Stats{
		In: c.in, Out: c.out, EncodeNanos: c.nanos,
		DroppedTracks: st.DroppedTracks, Reanchors: st.Reanchors, AudioDropped: st.AudioDropped,
		AudioCorrupt: st.AudioCorrupt, VideoCorrupt: st.VideoCorrupt,
		EncoderFallbacks: st.EncoderFallbacks,
		Thumbnails:       st.Thumbnails, ThumbnailsFailed: st.ThumbnailsFailed,
	}
}
