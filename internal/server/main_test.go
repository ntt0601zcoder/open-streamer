package server

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/transcode/transcodetest"
)

// envFakeProbe makes the test binary, run as `-probe font FILE`, act as the
// transcoder's font probe. Its value is a mode (see fakeProbe), or
// "file:<path>" to read the mode from that file at every run, which is how
// a test scripts one server's probe row by row.
const envFakeProbe = "OS_SERVER_FAKE_PROBE"

// TestMain runs the test binary as the font-probe stand-in when its switch
// is set and it is called as a probe, as the fake transcoder child under
// OS_TRANSCODE_FAKE=1, and as the tests otherwise.
func TestMain(m *testing.M) {
	if mode, ok := os.LookupEnv(envFakeProbe); ok && len(os.Args) == 4 && os.Args[1] == "-probe" && os.Args[2] == "font" {
		fakeProbe(mode)
	}
	if os.Getenv("OS_TRANSCODE_FAKE") == "1" {
		transcodetest.Main()
	}
	os.Exit(m.Run())
}

// fakeProbe acts out one probe and exits. The modes:
//
//	ok           "font: ok", exit 0
//	bad          "font: bad tables" on stdout, a temp path on stderr, exit 3
//	empty        exit 3 with nothing on stdout
//	long         a noise line, a "font: " line of 400 bytes, 8 KiB more, exit 3
//	segv         death by SIGSEGV (Unix)
//	usage        a stale bundle's usage line on stderr, exit 2
//	stub         the stub build's line on stderr, exit 2
//	exit1        exit 1
//	sleep:<file> write the PID to file, then sleep a minute
func fakeProbe(mode string) {
	if path, ok := strings.CutPrefix(mode, "file:"); ok {
		b, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		mode = strings.TrimSpace(string(b))
	}
	switch {
	case mode == "ok":
		fmt.Println("font: ok")
		os.Exit(0)
	case mode == "bad":
		fmt.Println("font: bad tables")
		fmt.Fprintln(os.Stderr, "/tmp/secret/path: cannot load")
		os.Exit(exitProbeFailed)
	case mode == "empty":
		os.Exit(exitProbeFailed)
	case mode == "long":
		fmt.Println("noise before the verdict")
		fmt.Println("font: " + strings.Repeat("é", 200))
		fmt.Println(strings.Repeat("z", 8<<10))
		os.Exit(exitProbeFailed)
	case mode == "segv":
		dieBySegv()
	case mode == "usage":
		fmt.Fprintln(os.Stderr, "usage: ostranscoder [-version | -probe nvenc]")
		os.Exit(2)
	case mode == "stub":
		fmt.Fprintln(os.Stderr, "ostranscoder: built without libav (the libav build tag); cannot transcode")
		os.Exit(2)
	case mode == "exit1":
		os.Exit(1)
	case strings.HasPrefix(mode, "sleep:"):
		_ = os.WriteFile(strings.TrimPrefix(mode, "sleep:"), []byte(strconv.Itoa(os.Getpid())), 0o600)
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, "fake probe: unknown mode "+mode)
	os.Exit(1)
}
