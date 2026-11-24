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
)

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
	if once != "" {
		if _, err := os.Stat(once); err == nil {
			crashAfter = -1
		}
	}
	delay, _ := time.ParseDuration(os.Getenv(EnvDelay))

	crash := func() {
		if once != "" {
			_ = os.WriteFile(once, nil, 0o600)
		}
		os.Exit(2)
	}

	if err := w.Write(wire.Hello{Version: version, PID: os.Getpid()}); err != nil {
		os.Exit(1)
	}
	var cfg wire.Configure
	n := 0
	for {
		m, err := r.Read()
		if errors.Is(err, io.EOF) {
			os.Exit(0)
		}
		if err != nil {
			os.Exit(1)
		}
		switch v := m.(type) {
		case wire.Configure:
			cfg = v
			if crashAfter == 0 {
				crash()
			}
		case wire.Frame:
			if delay > 0 {
				time.Sleep(delay)
			}
			if v.F.Kind() == media.KindVideo {
				for i := range cfg.Rungs {
					if w.Write(wire.Frame{Rung: i, F: v.F}) != nil {
						os.Exit(1)
					}
				}
			} else if w.Write(wire.Frame{Rung: -1, F: v.F}) != nil {
				os.Exit(1)
			}
			n++
			if crashAfter > 0 && n >= crashAfter {
				crash()
			}
		case wire.Stop:
			os.Exit(0)
		default:
		}
	}
}
