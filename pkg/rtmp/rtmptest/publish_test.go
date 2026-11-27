package rtmptest

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// silentServer accepts TCP connections and never answers.
func silentServer(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(io.Discard, c)
			}()
		}
	}()
	return "rtmp://" + ln.Addr().String() + "/live/s"
}

func TestDialDefaultDeadline(t *testing.T) {
	old := dialTimeout
	dialTimeout = 100 * time.Millisecond
	t.Cleanup(func() { dialTimeout = old })
	url := silentServer(t)

	start := time.Now()
	p, _, err := Dial(context.Background(), url)
	if p != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Dial: %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Dial to a silent server failed after %v", elapsed)
	}

	// A ctx deadline of its own is used as is.
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	start = time.Now()
	if _, _, err := Dial(ctx, url); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Dial: %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("Dial with a 400 ms ctx gave up after %v", elapsed)
	}
}
