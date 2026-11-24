// Package transcodetest provides a pure-Go fake of the transcoder child for
// the tests of internal/transcode, which re-execute their test binary as the
// child.
package transcodetest

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/wire"
)

// Environment variables read by Main.
const (
	// EnvCrashAfter makes the child exit 2 after that many input frames
	// (0: right after Configure).
	EnvCrashAfter = "OS_TRANSCODE_FAKE_CRASH_AFTER"
	// EnvCrashOnce names a file: the child crashes only if it is absent,
	// and creates it when it crashes.
	EnvCrashOnce = "OS_TRANSCODE_FAKE_CRASH_ONCE"
	// EnvDelay is a time.Duration slept per input frame.
	EnvDelay = "OS_TRANSCODE_FAKE_DELAY"
	// EnvVersion overrides the wire version in Hello.
	EnvVersion = "OS_TRANSCODE_FAKE_VERSION"
	// EnvStderr is a line the child writes to stderr as it starts.
	EnvStderr = "OS_TRANSCODE_FAKE_STDERR"
	// EnvStderrLines makes the child write that many numbered lines to
	// stderr as fast as it can as it starts, as libav does on lossy input.
	EnvStderrLines = "OS_TRANSCODE_FAKE_STDERR_LINES"
	// EnvStderrPanic makes the child, after any EnvStderrLines lines, write
	// "panic: boom" to stderr and exit 2 before its Hello, as a Go panic
	// or a crash in libav would.
	EnvStderrPanic = "OS_TRANSCODE_FAKE_STDERR_PANIC"
	// EnvJunk makes the child write bytes that do not decode instead of
	// Hello, then hang. With EnvCrashOnce, only while the file is absent.
	EnvJunk = "OS_TRANSCODE_FAKE_JUNK"
	// EnvNoHello makes the child hang without sending Hello. With
	// EnvCrashOnce, only while the file is absent.
	EnvNoHello = "OS_TRANSCODE_FAKE_NO_HELLO"
	// EnvJunkAfterHello makes the child send a valid Hello, then bytes that
	// do not decode, then hang. With EnvCrashOnce, only while the file is
	// absent.
	EnvJunkAfterHello = "OS_TRANSCODE_FAKE_JUNK_AFTER_HELLO"
	// EnvCloseThenExit makes the child, after Hello and Configure, close
	// fd 3, sleep 50 ms, write "closing child exits" to stderr and exit 2,
	// as the real child does on a pipeline error. With EnvCrashOnce, only
	// while the file is absent.
	EnvCloseThenExit = "OS_TRANSCODE_FAKE_CLOSE_THEN_EXIT"
	// EnvStopReadingAfter makes the child stop reading its input and
	// sending Stats, while it keeps running, after that many input frames,
	// as a child stuck in libav would. With EnvCrashOnce, only while the
	// file is absent.
	EnvStopReadingAfter = "OS_TRANSCODE_FAKE_STOP_READING_AFTER"
	// EnvStatsInterval is how often the child sends Stats with its input
	// count, as a time.Duration; default 1 s, as the real child.
	EnvStatsInterval = "OS_TRANSCODE_FAKE_STATS_INTERVAL"
	// EnvCounters makes every Stats carry the media counters in Counters,
	// and the child send one such Stats right after Configure.
	EnvCounters = "OS_TRANSCODE_FAKE_COUNTERS"
	// EnvVideoLag makes the child hold its video outputs back until that
	// many later input video frames have arrived, as an encoder's delay
	// does, while audio goes out at once. The held video goes out at the
	// end of the input and on Stop.
	EnvVideoLag = "OS_TRANSCODE_FAKE_VIDEO_LAG"
)

// Counters are the media counters a child run with EnvCounters reports.
var Counters = wire.Stats{DroppedTracks: 1, Reanchors: 2, AudioDropped: 3, AudioCorrupt: 4, VideoCorrupt: 5}

// Main runs the fake child on fd 3 and exits. It sends Hello, reads
// Configure, then echoes every input video frame unchanged to every rung
// and every input audio frame as rung -1. It exits 0 on Stop or at the end
// of its input.
func Main() {
	conn := os.NewFile(3, "transcode")
	if conn == nil {
		os.Exit(3)
	}
	w := wire.NewWriter(conn)
	r := wire.NewReader(bufio.NewReader(conn))

	version := uint32(wire.Version)
	if v, err := strconv.ParseUint(os.Getenv(EnvVersion), 10, 32); err == nil {
		version = uint32(v)
	}
	crashAfter := -1
	if n, err := strconv.Atoi(os.Getenv(EnvCrashAfter)); err == nil {
		crashAfter = n
	}
	once := os.Getenv(EnvCrashOnce)
	misbehave := true
	if once != "" {
		if _, err := os.Stat(once); err == nil {
			crashAfter, misbehave = -1, false
		}
	}
	delay, _ := time.ParseDuration(os.Getenv(EnvDelay))
	stopReading := -1
	if n, err := strconv.Atoi(os.Getenv(EnvStopReadingAfter)); err == nil && misbehave {
		stopReading = n
	}
	markOnce := func() {
		if once != "" {
			_ = os.WriteFile(once, nil, 0o600)
		}
	}
	crash := func() {
		markOnce()
		os.Exit(2)
	}

	if line := os.Getenv(EnvStderr); line != "" {
		_, _ = os.Stderr.WriteString(line + "\n")
	}
	if n, err := strconv.Atoi(os.Getenv(EnvStderrLines)); err == nil {
		for i := range n {
			_, _ = os.Stderr.WriteString("stderr line " + strconv.Itoa(i) + "\n")
		}
	}
	if os.Getenv(EnvStderrPanic) != "" {
		_, _ = os.Stderr.WriteString("panic: boom\n")
		os.Exit(2)
	}
	if misbehave && (os.Getenv(EnvJunk) != "" || os.Getenv(EnvNoHello) != "") {
		markOnce()
		if os.Getenv(EnvJunk) != "" {
			// A zero length is malformed.
			_, _ = conn.Write([]byte{0, 0, 0, 0, 0xff, 0xff})
		}
		for { // hang until killed
			time.Sleep(time.Hour)
		}
	}

	if err := w.Write(wire.Hello{Version: version, PID: os.Getpid()}); err != nil {
		os.Exit(1)
	}
	if misbehave && os.Getenv(EnvJunkAfterHello) != "" {
		markOnce()
		_, _ = conn.Write([]byte{0, 0, 0, 0, 0xff, 0xff})
		for { // hang until killed
			time.Sleep(time.Hour)
		}
	}
	statsEvery := time.Second
	if d, err := time.ParseDuration(os.Getenv(EnvStatsInterval)); err == nil && d > 0 {
		statsEvery = d
	}
	var in atomic.Uint64
	var stuck atomic.Bool
	counters := os.Getenv(EnvCounters) != ""
	stats := func() wire.Stats {
		s := wire.Stats{In: in.Load()}
		if counters {
			s.DroppedTracks, s.Reanchors, s.AudioDropped = Counters.DroppedTracks, Counters.Reanchors, Counters.AudioDropped
			s.AudioCorrupt, s.VideoCorrupt = Counters.AudioCorrupt, Counters.VideoCorrupt
		}
		return s
	}
	go func() {
		for {
			time.Sleep(statsEvery)
			if stuck.Load() {
				return
			}
			if w.Write(stats()) != nil {
				return
			}
		}
	}()

	lag, _ := strconv.Atoi(os.Getenv(EnvVideoLag))
	var lagged []media.Frame
	sendVideo := func(f media.Frame, rungs int) {
		for i := range rungs {
			if w.Write(wire.Frame{Rung: i, F: f}) != nil {
				os.Exit(1)
			}
		}
	}
	var cfg wire.Configure
	flushVideo := func() {
		for _, f := range lagged {
			sendVideo(f, len(cfg.Rungs))
		}
		lagged = nil
	}
	n := 0
	for {
		m, err := r.Read()
		if errors.Is(err, io.EOF) {
			flushVideo()
			os.Exit(0)
		}
		if err != nil {
			os.Exit(1)
		}
		switch v := m.(type) {
		case wire.Configure:
			cfg = v
			if misbehave && os.Getenv(EnvCloseThenExit) != "" {
				markOnce()
				_ = conn.Close()
				time.Sleep(50 * time.Millisecond)
				_, _ = os.Stderr.WriteString("closing child exits\n")
				os.Exit(2)
			}
			if counters && w.Write(stats()) != nil {
				os.Exit(1)
			}
			if crashAfter == 0 {
				crash()
			}
		case wire.Frame:
			in.Add(1)
			if delay > 0 {
				time.Sleep(delay)
			}
			if v.F.Kind() == media.KindVideo {
				lagged = append(lagged, v.F)
				for len(lagged) > lag {
					sendVideo(lagged[0], len(cfg.Rungs))
					lagged = lagged[1:]
				}
			} else if w.Write(wire.Frame{Rung: -1, F: v.F}) != nil {
				os.Exit(1)
			}
			n++
			if crashAfter > 0 && n >= crashAfter {
				crash()
			}
			if n == stopReading {
				stuck.Store(true)
				markOnce()
				for { // alive, never reading again
					time.Sleep(time.Hour)
				}
			}
		case wire.Stop:
			flushVideo()
			os.Exit(0)
		default:
		}
	}
}
