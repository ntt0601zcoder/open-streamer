package auth

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// places counts how many attempts can be reserved for key now, and
// abandons them all.
func places(t *testing.T, l *Limiter, key string) int {
	t.Helper()
	var held []*Attempt
	for range 100 {
		a, ok := l.Reserve(key)
		if !ok {
			break
		}
		held = append(held, a)
	}
	for _, a := range held {
		a.Abandon()
	}
	return len(held)
}

func failOnce(t *testing.T, l *Limiter, key string) {
	t.Helper()
	a, ok := l.Reserve(key)
	require.True(t, ok, "reserve")
	a.Fail()
}

func (l *Limiter) entryFor(key string) (entry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		return entry{}, false
	}
	return *e, true
}

func (l *Limiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

func TestLimiter(t *testing.T) {
	clk := newFakeClock()
	l := NewLimiter(clk.now)
	key := l.Key("127.0.0.1:1234", "ops")

	for i := 1; i <= 4; i++ {
		failOnce(t, l, key)
		_, blocked := l.Blocked(key)
		assert.False(t, blocked, "after %d failures", i)
	}
	failOnce(t, l, key)
	ra, blocked := l.Blocked(key)
	assert.True(t, blocked)
	assert.Equal(t, time.Second, ra)
	_, ok := l.Reserve(key)
	assert.False(t, ok, "reserved while blocked")
	clk.add(500 * time.Millisecond)
	ra, blocked = l.Blocked(key)
	assert.True(t, blocked)
	assert.Equal(t, 500*time.Millisecond, ra)
	clk.add(500 * time.Millisecond)
	_, blocked = l.Blocked(key)
	assert.False(t, blocked, "at +1 s")

	// Past the threshold, one attempt at a time; each failure doubles the
	// backoff up to 5 min.
	want := []time.Duration{2, 4, 8, 16, 32, 64, 128, 256, 300, 300, 300}
	for i, w := range want {
		assert.Equal(t, 1, places(t, l, key), "failure %d", i+6)
		failOnce(t, l, key)
		ra, blocked = l.Blocked(key)
		require.True(t, blocked)
		assert.Equal(t, w*time.Second, ra, "failure %d", i+6)
		clk.add(ra)
	}
	e, ok := l.entryFor(key)
	require.True(t, ok)
	assert.Equal(t, 16, e.failures)

	// Abandon changes nothing.
	a, ok := l.Reserve(key)
	require.True(t, ok)
	a.Abandon()
	e2, ok := l.entryFor(key)
	require.True(t, ok)
	assert.Equal(t, e.failures, e2.failures)
	assert.Equal(t, 0, e2.inFlight)
	_, blocked = l.Blocked(key)
	assert.False(t, blocked)

	// Succeed clears it at once; the next failure counts from 1.
	failOnce(t, l, key)
	_, blocked = l.Blocked(key)
	require.True(t, blocked)
	l.Succeed(key)
	_, blocked = l.Blocked(key)
	assert.False(t, blocked)
	assert.Equal(t, 5, places(t, l, key))
	failOnce(t, l, key)
	e, ok = l.entryFor(key)
	require.True(t, ok)
	assert.Equal(t, 1, e.failures)
	assert.Equal(t, 4, places(t, l, key))

	// An Attempt.Succeed clears it too.
	failOnce(t, l, key)
	a, ok = l.Reserve(key)
	require.True(t, ok)
	a.Succeed()
	assert.Equal(t, 5, places(t, l, key))

	// An idle key is gone after 15 min.
	for range 5 {
		failOnce(t, l, key)
	}
	clk.add(15*time.Minute - time.Second)
	_, ok = l.entryFor(key)
	assert.True(t, ok)
	_, blocked = l.Blocked(key)
	assert.False(t, blocked, "the backoff is 1 s")
	assert.Equal(t, 1, places(t, l, key), "past the threshold, one at a time")
	clk.add(time.Second)
	_, blocked = l.Blocked(key)
	assert.False(t, blocked)
	assert.Equal(t, 0, l.size(), "the idle key is still tracked")
	assert.Equal(t, 5, places(t, l, key))
}

func TestLimiterInFlightAndSucceed(t *testing.T) {
	clk := newFakeClock()
	l := NewLimiter(clk.now)
	key := l.Key("10.0.0.1:1", "ops")

	held := make([]*Attempt, 0, 6)
	for range 3 {
		a, ok := l.Reserve(key)
		require.True(t, ok)
		held = append(held, a)
	}
	l.Succeed(key)
	assert.Equal(t, 2, places(t, l, key), "Limiter.Succeed dropped attempts in flight")
	held[0].Succeed()
	assert.Equal(t, 3, places(t, l, key), "Attempt.Succeed dropped the other attempts in flight")
	for range 3 {
		a, ok := l.Reserve(key)
		require.True(t, ok)
		held = append(held, a)
	}
	_, ok := l.Reserve(key)
	assert.False(t, ok, "5 in flight")
	held[1].Fail()
	held[2].Abandon()
	held[3].Succeed()
	held[4].Fail()
	held[5].Abandon()
	// Resolving twice counts once.
	held[0].Fail()
	held[1].Fail()
	held[3].Abandon()
	l.Succeed(key)
	e, tracked := l.entryFor(key)
	if tracked {
		assert.Equal(t, 0, e.inFlight)
		assert.Equal(t, 0, e.failures)
	}
	assert.Equal(t, 5, places(t, l, key), "inFlight is not back to exactly 0")

	// 10000 keys that never failed: Limiter.Succeed adds nothing, and a key
	// with 4 failures keeps them.
	l2 := NewLimiter(clk.now)
	k4 := l2.Key("10.0.0.2:1", "ops")
	for range 4 {
		failOnce(t, l2, k4)
	}
	for i := range 10000 {
		l2.Succeed(l2.Key(fmt.Sprintf("10.1.%d.%d:1", i/256, i%256), "ops"))
	}
	assert.Equal(t, 1, l2.size())
	e, ok = l2.entryFor(k4)
	require.True(t, ok)
	assert.Equal(t, 4, e.failures)
}

func TestLimiterReservesBeforeVerifying(t *testing.T) {
	l := NewLimiter(nil)
	key := l.Key("127.0.0.1:1", "ops")

	var (
		mu   sync.Mutex
		held []*Attempt
		wg   sync.WaitGroup
	)
	startGate := make(chan struct{})
	for range 200 {
		wg.Go(func() {
			<-startGate
			if a, ok := l.Reserve(key); ok {
				mu.Lock()
				held = append(held, a)
				mu.Unlock()
			}
		})
	}
	close(startGate)
	wg.Wait()
	require.Len(t, held, 5)
	for _, a := range held {
		a.Fail()
	}
	_, blocked := l.Blocked(key)
	assert.True(t, blocked)
	_, ok := l.Reserve(key)
	assert.False(t, ok)

	clk := newFakeClock()
	l = NewLimiter(clk.now)
	for range 3 {
		failOnce(t, l, key)
	}
	a1, ok := l.Reserve(key)
	require.True(t, ok)
	a2, ok := l.Reserve(key)
	require.True(t, ok)
	_, ok = l.Reserve(key)
	assert.False(t, ok, "3 failures and 2 in flight")
	a1.Abandon()
	a3, ok := l.Reserve(key)
	assert.True(t, ok, "an Abandon gives a place back")
	_, ok = l.Reserve(key)
	assert.False(t, ok)
	a2.Abandon()
	a3.Abandon()
}

func TestLimiterKeyIsAddressAndUser(t *testing.T) {
	l := NewLimiter(nil)
	k1 := l.Key("127.0.0.1:1", "scraper")
	k2 := l.Key("127.0.0.1:2", "ops")
	assert.NotEqual(t, k1, k2)
	for range 5 {
		failOnce(t, l, k1)
	}
	_, blocked := l.Blocked(k1)
	assert.True(t, blocked)
	_, blocked = l.Blocked(k2)
	assert.False(t, blocked)
	assert.Equal(t, 5, places(t, l, k2))

	assert.Equal(t, l.Key("127.0.0.1:1", "scraper"), l.Key("127.0.0.1:9999", "scraper"))
	assert.NotEqual(t, l.Key("127.0.0.1:1", "ops"), l.Key("127.0.0.2:1", "ops"))
	// The separator keeps address and name apart.
	assert.NotEqual(t, l.Key("ab", "c"), l.Key("a", "bc"))

	long := strings.Repeat("n", 1<<20)
	for _, k := range []string{k1, k2, l.Key("127.0.0.1:1", ""), l.Key("127.0.0.1:1", long), l.Key("garbage", long)} {
		assert.Regexp(t, `^[0-9a-f]{64}$`, k)
		assert.NotContains(t, k, "127.0.0.1")
		assert.NotContains(t, k, "scraper")
	}
	// Another Limiter has another key.
	assert.NotEqual(t, k1, NewLimiter(nil).Key("127.0.0.1:1", "scraper"))
}

func TestLimiterBounded(t *testing.T) {
	clk := newFakeClock()
	l := NewLimiter(clk.now)
	pinned := l.Key("192.168.0.1:1", "ops")
	failOnce(t, l, pinned)
	hold, ok := l.Reserve(pinned)
	require.True(t, ok)

	keys := make([]string, 5000)
	for i := range keys {
		keys[i] = l.Key(fmt.Sprintf("10.%d.%d.1:1", i/256, i%256), "ops")
		clk.add(time.Millisecond)
		failOnce(t, l, keys[i])
	}
	assert.LessOrEqual(t, l.size(), maxKeys)
	assert.Equal(t, maxKeys, l.size())
	_, ok = l.entryFor(pinned)
	assert.True(t, ok, "a key with an attempt in flight was dropped")
	for _, k := range keys[len(keys)-(maxKeys-1):] {
		_, ok := l.entryFor(k)
		require.True(t, ok, "a recent key was dropped")
	}
	_, ok = l.entryFor(keys[0])
	assert.False(t, ok)
	hold.Fail()
	e, ok := l.entryFor(pinned)
	require.True(t, ok)
	assert.Equal(t, 2, e.failures)
	assert.Equal(t, 0, e.inFlight)
}

// TestLimiterFullOfInFlight: when every tracked key has an attempt in
// flight, a new key is refused rather than any tracked one dropped.
func TestLimiterFullOfInFlight(t *testing.T) {
	l := NewLimiter(newFakeClock().now)
	held := make([]*Attempt, 0, maxKeys)
	for i := range maxKeys {
		a, ok := l.Reserve(l.Key(fmt.Sprintf("10.%d.%d.1:1", i/256, i%256), "ops"))
		require.True(t, ok)
		held = append(held, a)
	}
	_, ok := l.Reserve(l.Key("192.168.0.1:1", "ops"))
	assert.False(t, ok)
	assert.Equal(t, maxKeys, l.size())
	for _, a := range held {
		a.Abandon()
	}
	assert.Equal(t, 0, l.size())
	_, ok = l.Reserve(l.Key("192.168.0.1:1", "ops"))
	assert.True(t, ok)
}

func TestLimiterConcurrent(t *testing.T) {
	l := NewLimiter(nil)
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Go(func() {
			for i := range 500 {
				key := l.Key(fmt.Sprintf("10.0.%d.%d:1", g, i%7), "ops")
				l.Blocked(key)
				l.Succeed(key)
				if a, ok := l.Reserve(key); ok {
					switch i % 3 {
					case 0:
						a.Fail()
					case 1:
						a.Succeed()
					default:
						a.Abandon()
					}
				}
			}
		})
	}
	wg.Wait()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		assert.Equal(t, 0, e.inFlight)
	}
}

func TestClientKey(t *testing.T) {
	assert.Equal(t, "1.2.3.4", ClientKey("1.2.3.4:5"))
	assert.Equal(t, "1.2.3.4", ClientKey("[::ffff:1.2.3.4]:5"))
	assert.Equal(t, ClientKey("[2001:db8:1:2:3:4:5:6]:7"), ClientKey("[2001:db8:1:2::9]:7"))
	assert.NotEqual(t, ClientKey("[2001:db8:1:2::9]:7"), ClientKey("[2001:db8:1:3::9]:7"))
	assert.Equal(t, ClientKey("[fe80::1%eth0]:5"), ClientKey("[fe80::2%eth1]:5"))
	assert.NotContains(t, ClientKey("[fe80::1%eth0]:5"), "%")
	assert.NotContains(t, ClientKey("[fe80::1%eth0]:5"), "eth")
	assert.Equal(t, "garbage", ClientKey("garbage"))
	assert.Equal(t, "", ClientKey(""))
	assert.Equal(t, "1.2.3.4", ClientKey("1.2.3.4"), "a bare address")
	assert.Equal(t, ClientKey("[2001:db8:1:2::9]:7"), ClientKey("2001:db8:1:2::1"))
	assert.NotEqual(t, ClientKey("1.2.3.4:5"), ClientKey("1.2.3.5:5"))
}
