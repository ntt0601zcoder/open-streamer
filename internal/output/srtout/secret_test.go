package srtout

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/datarhei/gosrt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testClock is a clock the tests move by hand.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock { return &testClock{now: time.Unix(1_000_000, 0)} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestFailures(t *testing.T) {
	clk := newTestClock()
	f := newFailures()
	f.now = clk.Now
	for i := range maxSecretFailures {
		assert.False(t, f.limited("a"), "after %d failures", i)
		f.fail("a")
		clk.Add(time.Second)
	}
	assert.True(t, f.limited("a"))
	assert.False(t, f.limited("b"), "another key")
	clk.Add(secretWindow - maxSecretFailures*time.Second - time.Nanosecond)
	assert.True(t, f.limited("a"), "until 10 s after the first failure")
	clk.Add(time.Nanosecond)
	assert.False(t, f.limited("a"), "10 s after the first failure")
	f.fail("a")
	assert.Equal(t, failure{count: 1, first: clk.Now()}, f.m["a"], "an expired entry starts over")

	// Four failures inside the window and one after: not limited.
	f = newFailures()
	f.now = clk.Now
	for range maxSecretFailures - 1 {
		f.fail("c")
	}
	clk.Add(secretWindow)
	f.fail("c")
	assert.False(t, f.limited("c"))
}

// fakeReq is a connection request that answer refuses before it is
// accepted; checks counts its SetPassphrase calls.
type fakeReq struct {
	addr   net.Addr
	id     string
	pass   string // the caller's passphrase: "" for an unencrypted request
	checks *atomic.Int64
	reason *srt.RejectionReason
}

func (r fakeReq) RemoteAddr() net.Addr                   { return r.addr }
func (r fakeReq) Version() uint32                        { return 5 }
func (r fakeReq) StreamId() string                       { return r.id }
func (r fakeReq) SocketId() uint32                       { return 1 }
func (r fakeReq) PeerSocketId() uint32                   { return 2 }
func (r fakeReq) IsEncrypted() bool                      { return r.pass != "" }
func (r fakeReq) SetRejectionReason(srt.RejectionReason) {}
func (r fakeReq) Accept() (srt.Conn, error)              { return nil, errors.New("fake: not accepted") }
func (r fakeReq) Reject(reason srt.RejectionReason)      { *r.reason = reason }

func (r fakeReq) SetPassphrase(p string) error {
	r.checks.Add(1)
	if p != r.pass {
		return errors.New("fake: wrong passphrase")
	}
	return nil
}

// A peer key with 5 wrong passphrases in 10 s has its encrypted requests
// refused unchecked, and nobody else's; the table stays bounded.
func TestPassphraseLimiter(t *testing.T) {
	open, openRest := liveHub(t, 8)
	locked, lockedRest := liveHub(t, 8)
	v := newViews()
	v.set("open", open, "")
	v.set("locked", locked, lockedPass)
	clk := newTestClock()
	var realKey atomic.Value // the peer key of the test's gosrt callers
	realKey.Store("k1")
	rej := newRejections()
	srv, addr := serveSecure(t, Options{Secure: v.Secure}, func(s *Server) {
		s.request = rej.wrap
		s.fails.now = clk.Now
		s.fails.key = func(a net.Addr) string {
			if _, ok := a.(fakeAddr); ok {
				return peerKey(a)
			}
			return realKey.Load().(string)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	doneOpen, doneLocked := feed(ctx, open, openRest, 1), feed(ctx, locked, lockedRest, 1)
	defer func() { cancel(); <-doneOpen; <-doneLocked }()

	var checks atomic.Int64
	ask := func(key, pass string) srt.RejectionReason {
		var reason srt.RejectionReason
		srv.answer(fakeReq{addr: fakeAddr(key + ":1"), id: "live/locked", pass: pass, checks: &checks, reason: &reason})
		return reason
	}
	for range maxSecretFailures {
		assert.Equal(t, srt.REJ_BADSECRET, ask("k1", wrongPass))
	}
	st := srv.Stats()
	assert.Equal(t, uint64(5), st.RejectedSecret)
	assert.Zero(t, st.RejectedLimited)
	for i := range 1000 {
		pass := wrongPass
		if i%2 == 0 {
			pass = lockedPass
		}
		require.Equal(t, srt.REJ_RESOURCE, ask("k1", pass), i)
	}
	st = srv.Stats()
	assert.Equal(t, uint64(1000), st.RejectedLimited)
	assert.Equal(t, uint64(5), st.RejectedSecret, "none of them was checked")
	assert.Equal(t, int64(5), checks.Load(), "none of them was checked")
	assert.Equal(t, st.Rejected, rejectedSum(st))

	// The limited key still plays a stream without a passphrase; another
	// key connects at once.
	readSome(t, addr, "live/open", nil)
	_, err := dial(addr, "#!::r=live/locked,x=k1", withPass(lockedPass))
	require.Error(t, err)
	assert.Equal(t, srt.REJ_RESOURCE, rej.of("#!::r=live/locked,x=k1")[0])
	realKey.Store("k2")
	readSome(t, addr, "live/locked", withPass(lockedPass))

	// 10 s past the first failure, the first key connects.
	clk.Add(secretWindow)
	realKey.Store("k1")
	readSome(t, addr, "live/locked", withPass(lockedPass))

	// The table holds at most 4096 keys; after 10 s the next failure finds
	// it full of expired entries and drops them.
	for i := range maxSecretKeys + 1 {
		assert.Equal(t, srt.REJ_BADSECRET, ask("f"+strconv.Itoa(i), wrongPass))
	}
	tracked := func() int {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return len(srv.fails.m)
	}
	assert.Equal(t, maxSecretKeys, tracked())
	clk.Add(secretWindow)
	assert.Equal(t, srt.REJ_BADSECRET, ask("late", wrongPass))
	assert.Equal(t, 1, tracked(), "only the new failure is left")
	st = srv.Stats()
	assert.Equal(t, st.Rejected, rejectedSum(st))
	assert.Zero(t, st.RejectedCaps)
}
