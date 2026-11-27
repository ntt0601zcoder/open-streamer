//go:build unix

package main

import (
	"context"
	"net/http"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sendHUP sends the test process a SIGHUP. Every caller has called
// notifyHUP first: an unhandled SIGHUP ends the process.
func sendHUP(t *testing.T) {
	t.Helper()
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGHUP))
}

func TestSIGHUP(t *testing.T) {
	t.Run("end to end", func(t *testing.T) {
		// main's own set-up: the channel is registered before the server
		// starts, so a HUP during start-up is kept and not fatal.
		hup := notifyHUP()
		require.Equal(t, 1, cap(hup))
		sendHUP(t)
		text := configText(editorHash(t), streamEntry(t, "cam"))
		r := startRun(t, text, hup)
		require.Eventually(t, func() bool { return r.reloads() == 1 }, waitFor, 5*time.Millisecond,
			"the HUP of the start-up is applied once the server runs")

		edited := text + streamEntry(t, "two")
		require.NoError(t, os.WriteFile(r.path, []byte(edited), 0o600))
		status, body := r.get("/api/v1/config/status")
		require.Equal(t, http.StatusOK, status)
		require.Contains(t, body, `"stale":true`)
		sendHUP(t)
		deadline := time.Now().Add(5 * time.Second)
		for r.reloads() < 2 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		require.Equal(t, 2, r.reloads(), "the reload did not come within 5 s")
		status, _ = r.get("/api/v1/streams/two")
		assert.Equal(t, http.StatusOK, status)
		status, body = r.get("/api/v1/config")
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, edited, body)
		status, body = r.get("/api/v1/config/status")
		require.Equal(t, http.StatusOK, status)
		assert.Contains(t, body, `"stale":false`)

		// After ctx is cancelled a HUP starts no reload, and does not kill
		// the process that is shutting down.
		r.cancel()
		sendHUP(t)
		assert.Equal(t, 0, r.stop())
		sendHUP(t)
		time.Sleep(50 * time.Millisecond)
		assert.Equal(t, 2, r.reloads())
	})

	t.Run("coalescing", func(t *testing.T) {
		hup := notifyHUP()
		var (
			calls   atomic.Int32
			marker  atomic.Bool
			sawMark atomic.Bool
			started = make(chan struct{}, 16)
			release = make(chan struct{})
		)
		// A slow reload: the first call blocks until the test releases it.
		reload := func(context.Context) {
			n := calls.Add(1)
			sawMark.Store(marker.Load())
			started <- struct{}{}
			if n == 1 {
				<-release
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { hupLoop(ctx, hup, reload); close(done) }()
		defer func() { cancel(); <-done }()

		sendHUP(t)
		select {
		case <-started:
		case <-time.After(waitFor):
			require.Fail(t, "the first HUP started no reload")
		}
		// Three more arrive while it runs: the process survives, and they
		// coalesce into the one pending reload.
		sendHUP(t)
		sendHUP(t)
		marker.Store(true)
		sendHUP(t)
		require.Eventually(t, func() bool { return len(hup) == 1 }, waitFor, time.Millisecond)
		time.Sleep(50 * time.Millisecond) // the runtime has delivered all three
		require.EqualValues(t, 1, calls.Load())
		close(release)
		select {
		case <-started:
		case <-time.After(waitFor):
			require.Fail(t, "the pending HUP was lost")
		}
		time.Sleep(100 * time.Millisecond)
		n := calls.Load()
		assert.GreaterOrEqual(t, n, int32(2), "a signal that arrived during the reload was lost")
		assert.LessOrEqual(t, n, int32(3), "more reloads than pending signals")
		assert.True(t, sawMark.Load(), "the last reload ran before the last HUP")
	})
}
