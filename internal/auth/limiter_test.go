package auth

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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

var addrSeq atomic.Uint32

// anyAddr is an address no other attempt of the test holds a place of, for
// the rows that are about the name key: the cap of attempts in flight per
// address never refuses them.
func anyAddr() string {
	n := addrSeq.Add(1)
	return fmt.Sprintf("198.%d.%d.%d:1", 18+n>>16&1, n>>8&255, n&255)
}

// addrCount is the attempts in flight the limiter counts for the client of
// remoteAddr, and whether it tracks the address at all.
func (l *Limiter) addrCount(remoteAddr string) (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, ok := l.addrs[ClientKey(remoteAddr)]
	return n, ok
}

func (l *Limiter) addrSize() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.addrs)
}

func (l *Limiter) keySet() map[string]entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]entry, len(l.entries))
	for k, e := range l.entries {
		out[k] = *e
	}
	return out
}

// places counts how many attempts can be reserved for key now, and
// abandons them all.
func places(t *testing.T, l *Limiter, key string) int {
	t.Helper()
	var held []*Attempt
	for range 100 {
		a, ok := l.Reserve(key, anyAddr())
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
	a, ok := l.Reserve(key, anyAddr())
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
	_, ok := l.Reserve(key, anyAddr())
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
	a, ok := l.Reserve(key, anyAddr())
	require.True(t, ok)
	a.Abandon()
	e2, ok := l.entryFor(key)
	require.True(t, ok)
	assert.Equal(t, e.failures, e2.failures)
	assert.Equal(t, 0, e2.inFlight)
	_, blocked = l.Blocked(key)
	assert.False(t, blocked)

	// An Attempt.Succeed (a real verification) clears it at once; the next
	// failure counts from 1.
	failOnce(t, l, key)
	ra, blocked = l.Blocked(key)
	require.True(t, blocked)
	clk.add(ra)
	a, ok = l.Reserve(key, anyAddr())
	require.True(t, ok)
	a.Succeed()
	_, blocked = l.Blocked(key)
	assert.False(t, blocked)
	assert.Equal(t, 5, places(t, l, key))
	failOnce(t, l, key)
	e, ok = l.entryFor(key)
	require.True(t, ok)
	assert.Equal(t, 1, e.failures)
	assert.Equal(t, 4, places(t, l, key))

	// Below the threshold too.
	failOnce(t, l, key)
	a, ok = l.Reserve(key, anyAddr())
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
		a, ok := l.Reserve(key, anyAddr())
		require.True(t, ok)
		held = append(held, a)
	}
	assert.Equal(t, 2, places(t, l, key))
	held[0].Succeed()
	assert.Equal(t, 3, places(t, l, key), "Attempt.Succeed dropped the other attempts in flight")
	for range 3 {
		a, ok := l.Reserve(key, anyAddr())
		require.True(t, ok)
		held = append(held, a)
	}
	_, ok := l.Reserve(key, anyAddr())
	assert.False(t, ok, "5 in flight")
	held[1].Fail()
	held[2].Abandon()
	held[4].Fail()
	held[3].Succeed()
	held[5].Abandon()
	// Resolving twice counts once.
	held[0].Fail()
	held[1].Fail()
	held[3].Abandon()
	e, tracked := l.entryFor(key)
	if tracked {
		assert.Equal(t, 0, e.inFlight)
		assert.Equal(t, 0, e.failures)
	}
	assert.Equal(t, 5, places(t, l, key), "inFlight is not back to exactly 0")
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
			if a, ok := l.Reserve(key, anyAddr()); ok {
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
	_, ok := l.Reserve(key, anyAddr())
	assert.False(t, ok)

	clk := newFakeClock()
	l = NewLimiter(clk.now)
	for range 3 {
		failOnce(t, l, key)
	}
	a1, ok := l.Reserve(key, anyAddr())
	require.True(t, ok)
	a2, ok := l.Reserve(key, anyAddr())
	require.True(t, ok)
	_, ok = l.Reserve(key, anyAddr())
	assert.False(t, ok, "3 failures and 2 in flight")
	a1.Abandon()
	a3, ok := l.Reserve(key, anyAddr())
	assert.True(t, ok, "an Abandon gives a place back")
	_, ok = l.Reserve(key, anyAddr())
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
	hold, ok := l.Reserve(pinned, anyAddr())
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
		a, ok := l.Reserve(l.Key(fmt.Sprintf("10.%d.%d.1:1", i/256, i%256), "ops"), anyAddr())
		require.True(t, ok)
		held = append(held, a)
	}
	_, ok := l.Reserve(l.Key("192.168.0.1:1", "ops"), anyAddr())
	assert.False(t, ok)
	assert.Equal(t, maxKeys, l.size())
	for _, a := range held {
		a.Abandon()
	}
	assert.Equal(t, 0, l.size())
	_, ok = l.Reserve(l.Key("192.168.0.1:1", "ops"), anyAddr())
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
				if a, ok := l.Reserve(key, anyAddr()); ok {
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

func TestAttemptNil(t *testing.T) {
	var a *Attempt
	assert.NotPanics(t, func() {
		a.Abandon()
		a.Fail()
		a.Succeed()
	})
}

// TestLimiterEvictsBlockedLast: an address blocked on one user name cannot
// flush that block out by presenting 4096 other names.
func TestLimiterEvictsBlockedLast(t *testing.T) {
	clk := newFakeClock()
	l := NewLimiter(clk.now)
	blockedKey := l.Key("203.0.113.9:1", "admin")
	for range 5 {
		failOnce(t, l, blockedKey)
	}
	_, blocked := l.Blocked(blockedKey)
	require.True(t, blocked)

	others := make([]string, maxKeys)
	for i := range others {
		others[i] = l.Key("203.0.113.9:1", fmt.Sprintf("name%d", i))
		clk.add(time.Millisecond)
		failOnce(t, l, others[i])
	}
	assert.Equal(t, maxKeys, l.size())
	e, ok := l.entryFor(blockedKey)
	require.True(t, ok, "the blocked key was evicted")
	assert.Equal(t, 5, e.failures)
	_, ok = l.entryFor(others[0])
	assert.False(t, ok, "the oldest key under 5 failures goes first")
	_, ok = l.entryFor(others[len(others)-1])
	assert.True(t, ok)

	// With only keys at 5 or more failures left to evict, the oldest of
	// them goes, never one with an attempt in flight.
	l = NewLimiter(clk.now)
	keys := make([]string, maxKeys)
	for i := range keys {
		keys[i] = l.Key(fmt.Sprintf("10.%d.%d.1:1", i/256, i%256), "ops")
		for range 5 {
			failOnce(t, l, keys[i])
		}
		clk.add(time.Millisecond) // each newer than the last, all inside 15 min
	}
	clk.add(time.Second) // keys[0]'s 1 s backoff has run out
	hold, ok := l.Reserve(keys[0], anyAddr())
	require.True(t, ok)
	_, ok = l.Reserve(l.Key("192.168.0.1:1", "ops"), anyAddr())
	require.True(t, ok)
	assert.Equal(t, maxKeys, l.size())
	_, ok = l.entryFor(keys[0])
	assert.True(t, ok, "a key with an attempt in flight was dropped")
	_, ok = l.entryFor(keys[1])
	assert.False(t, ok, "the oldest blocked key without an attempt in flight goes")
	hold.Abandon()
}

// TestLimiterRefusedReserveEvictsNothing: a Reserve that is refused leaves
// a full table as it was.
func TestLimiterRefusedReserveEvictsNothing(t *testing.T) {
	clk := newFakeClock()
	l := NewLimiter(clk.now)
	keys := make([]string, maxKeys)
	for i := range keys {
		keys[i] = l.Key(fmt.Sprintf("10.%d.%d.1:1", i/256, i%256), "ops")
		clk.add(time.Millisecond)
		failOnce(t, l, keys[i])
	}
	for range 4 {
		failOnce(t, l, keys[len(keys)-1])
	}
	_, ok := l.Reserve(keys[len(keys)-1], anyAddr())
	require.False(t, ok, "blocked")
	_, ok = l.entryFor(keys[0])
	assert.True(t, ok, "a refused Reserve evicted a key")
	assert.Equal(t, maxKeys, l.size())
}

// TestLimiterAddressCap: one client address holds at most addrInFlight
// unresolved attempts, whatever names it presents, and nothing else is kept
// per address.
func TestLimiterAddressCap(t *testing.T) {
	const a1, a2 = "203.0.113.9:1000", "203.0.113.10:1000"
	seq := 0
	// fresh reserves for a name the address has not presented before.
	fresh := func(l *Limiter, addr string) (*Attempt, bool) {
		seq++
		return l.Reserve(l.Key(addr, fmt.Sprintf("name%d", seq)), addr)
	}
	fill := func(t *testing.T, l *Limiter, addr string) []*Attempt {
		t.Helper()
		held := make([]*Attempt, 0, addrInFlight)
		for i := range addrInFlight {
			a, ok := fresh(l, addr)
			require.True(t, ok, "attempt %d", i+1)
			held = append(held, a)
		}
		return held
	}

	t.Run("the fifth is refused", func(t *testing.T) {
		l := NewLimiter(newFakeClock().now)
		held := fill(t, l, a1)
		_, ok := fresh(l, a1)
		assert.False(t, ok, "a 5th attempt in flight")
		// Another port of the address is the same client.
		_, ok = fresh(l, "203.0.113.9:2000")
		assert.False(t, ok, "another port")
		n, _ := l.addrCount(a1)
		assert.Equal(t, addrInFlight, n)

		// Another address is not concerned, before, during and after.
		other := fill(t, l, a2)
		_, ok = fresh(l, a2)
		assert.False(t, ok)
		for _, a := range other {
			a.Abandon()
		}
		for _, a := range held {
			a.Abandon()
		}
		a, ok := fresh(l, a2)
		require.True(t, ok)
		a.Abandon()
	})

	t.Run("each resolution gives a place back", func(t *testing.T) {
		for name, resolve := range map[string]func(*Attempt){
			"Succeed": (*Attempt).Succeed,
			"Fail":    (*Attempt).Fail,
			"Abandon": (*Attempt).Abandon,
		} {
			l := NewLimiter(newFakeClock().now)
			held := fill(t, l, a1)
			_, ok := fresh(l, a1)
			require.False(t, ok, name)
			resolve(held[0])
			a, ok := fresh(l, a1)
			require.True(t, ok, "after %s", name)
			_, ok = fresh(l, a1)
			assert.False(t, ok, "%s gave more than one place back", name)
			// Resolving again gives nothing more back.
			held[0].Abandon()
			held[0].Fail()
			held[0].Succeed()
			_, ok = fresh(l, a1)
			assert.False(t, ok, "%s twice gave a second place back", name)
			a.Abandon()
			for _, h := range held[1:] {
				h.Abandon()
			}
			assert.Equal(t, 0, l.addrSize(), name)
		}
	})

	t.Run("failures never block the address", func(t *testing.T) {
		clk := newFakeClock()
		l := NewLimiter(clk.now)
		for i := range 100 {
			a, ok := fresh(l, a1)
			require.True(t, ok, "attempt %d", i+1)
			a.Fail()
		}
		_, tracked := l.addrCount(a1)
		assert.False(t, tracked, "nothing is kept for an address with nothing in flight")
		a, ok := fresh(l, a1)
		require.True(t, ok, "the 101st")
		a.Abandon()
		held := fill(t, l, a1)
		for _, h := range held {
			h.Fail()
		}
		// There is no per-address state to expire: 15 min later nothing has
		// changed either.
		clk.add(idleAfter)
		held = fill(t, l, a1)
		_, ok = fresh(l, a1)
		assert.False(t, ok)
		for _, h := range held {
			h.Abandon()
		}
		assert.Equal(t, 0, l.addrSize())
	})

	t.Run("a refusal by the name key takes no place", func(t *testing.T) {
		l := NewLimiter(newFakeClock().now)
		blocked := l.Key(a1, "admin")
		for range maxAttempts {
			a, ok := l.Reserve(blocked, a1)
			require.True(t, ok)
			a.Fail()
		}
		_, isBlocked := l.Blocked(blocked)
		require.True(t, isBlocked)
		for range 10 {
			_, ok := l.Reserve(blocked, a1)
			require.False(t, ok, "the blocked key")
		}
		_, tracked := l.addrCount(a1)
		assert.False(t, tracked, "a refused Reserve took a place")
		held := fill(t, l, a1)
		for _, h := range held {
			h.Abandon()
		}

		// A key refused for its attempts in flight, too.
		l = NewLimiter(newFakeClock().now)
		busy := l.Key(a1, "ops")
		for range maxAttempts - 2 {
			a, ok := l.Reserve(busy, a1)
			require.True(t, ok)
			a.Fail()
		}
		h1, ok := l.Reserve(busy, a1)
		require.True(t, ok)
		h2, ok := l.Reserve(busy, a1)
		require.True(t, ok)
		for range 10 {
			_, ok = l.Reserve(busy, a1)
			require.False(t, ok, "3 failures and 2 in flight")
		}
		n, _ := l.addrCount(a1)
		assert.Equal(t, 2, n)
		h1.Abandon()
		h2.Abandon()
		assert.Equal(t, 0, l.addrSize())
	})

	t.Run("an IPv6 client is its /64", func(t *testing.T) {
		l := NewLimiter(newFakeClock().now)
		held := make([]*Attempt, 0, addrInFlight)
		for i := range addrInFlight {
			a, ok := fresh(l, fmt.Sprintf("[2001:db8:1:2::%x]:443", i+1))
			require.True(t, ok)
			held = append(held, a)
		}
		_, ok := fresh(l, "[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:443")
		assert.False(t, ok, "a 5th address of the /64")
		a, ok := fresh(l, "[2001:db8:1:3::1]:443")
		assert.True(t, ok, "another /64")
		a.Abandon()
		// An IPv4-mapped address is the IPv4 client.
		v4 := fill(t, l, "192.0.2.7:1")
		_, ok = fresh(l, "[::ffff:192.0.2.7]:9")
		assert.False(t, ok, "the IPv4-mapped form")
		for _, h := range append(held, v4...) {
			h.Abandon()
		}
		assert.Equal(t, 0, l.addrSize())
	})

	t.Run("the address map empties", func(t *testing.T) {
		l := NewLimiter(newFakeClock().now)
		held := make([]*Attempt, 0, 50*addrInFlight)
		for i := range 50 {
			held = append(held, fill(t, l, fmt.Sprintf("10.9.%d.1:1", i))...)
		}
		assert.Equal(t, 50, l.addrSize())
		for i, h := range held {
			switch i % 3 {
			case 0:
				h.Fail()
			case 1:
				h.Succeed()
			default:
				h.Abandon()
			}
		}
		assert.Equal(t, 0, l.addrSize(), "an address with nothing in flight is still tracked")
	})

	t.Run("a place cannot leak with the name entry", func(t *testing.T) {
		// Unreachable today: an entry with an attempt in flight is never
		// deleted. Were it, the address's place would still come back.
		l := NewLimiter(newFakeClock().now)
		key := l.Key(a1, "ops")
		a, ok := l.Reserve(key, a1)
		require.True(t, ok)
		l.mu.Lock()
		delete(l.entries, key)
		l.mu.Unlock()
		a.Fail()
		assert.Equal(t, 0, l.addrSize())
	})

	t.Run("concurrent", func(t *testing.T) {
		l := NewLimiter(nil)
		const addrs = 5
		var (
			inFlight [addrs]atomic.Int32
			most     [addrs]atomic.Int32
			refused  atomic.Int64
			wg       sync.WaitGroup
		)
		for g := range 50 {
			wg.Go(func() {
				n := g % addrs
				addr := fmt.Sprintf("10.8.0.%d:%d", n+1, 1000+g)
				for i := range 400 {
					a, ok := l.Reserve(l.Key(addr, fmt.Sprintf("g%d-%d", g, i)), addr)
					if !ok {
						refused.Add(1)
						continue
					}
					now := inFlight[n].Add(1)
					for {
						m := most[n].Load()
						if now <= m || most[n].CompareAndSwap(m, now) {
							break
						}
					}
					runtime.Gosched() // held for a moment, so that attempts overlap
					// Counted down before the place goes back, so the count
					// never runs ahead of the limiter's.
					inFlight[n].Add(-1)
					switch i % 3 {
					case 0:
						a.Fail()
					case 1:
						a.Succeed()
					default:
						a.Abandon()
					}
				}
			})
		}
		wg.Wait()
		for n := range addrs {
			assert.LessOrEqual(t, int(most[n].Load()), addrInFlight, "address %d", n)
		}
		assert.Equal(t, 0, l.addrSize())
		l.mu.Lock()
		defer l.mu.Unlock()
		for _, e := range l.entries {
			assert.Equal(t, 0, e.inFlight)
		}
	})
}

// TestLimiterAddressCapEvictsNothing: a Reserve the address's cap refuses
// leaves a full name table as it was: nothing evicted, nothing inserted.
func TestLimiterAddressCapEvictsNothing(t *testing.T) {
	clk := newFakeClock()
	l := NewLimiter(clk.now)
	const addr = "203.0.113.9:1"
	held := make([]*Attempt, 0, addrInFlight)
	for i := range addrInFlight {
		a, ok := l.Reserve(l.Key(addr, fmt.Sprintf("held%d", i)), addr)
		require.True(t, ok)
		held = append(held, a)
	}
	for i := 0; l.size() < maxKeys; i++ {
		clk.add(time.Millisecond)
		failOnce(t, l, l.Key(fmt.Sprintf("10.%d.%d.1:1", i/256, i%256), "ops"))
	}
	// Past sweepEvery, so a lookup would sweep, and past nothing's idleAfter.
	clk.add(2 * sweepEvery)
	before := l.keySet()
	require.Len(t, before, maxKeys)
	l.mu.Lock()
	lastSweep := l.lastSweep
	l.mu.Unlock()

	newKey := l.Key(addr, "a new name")
	_, ok := l.Reserve(newKey, addr)
	require.False(t, ok, "a 5th attempt in flight")
	assert.Equal(t, before, l.keySet(), "the refused Reserve changed the name table")
	_, ok = l.entryFor(newKey)
	assert.False(t, ok, "the refused Reserve inserted its key")
	l.mu.Lock()
	assert.Equal(t, lastSweep, l.lastSweep, "the refused Reserve looked the name table up")
	l.mu.Unlock()
	n, _ := l.addrCount(addr)
	assert.Equal(t, addrInFlight, n)

	// With a place back, the same Reserve is admitted.
	held[0].Abandon()
	a, ok := l.Reserve(newKey, addr)
	require.True(t, ok)
	_, ok = l.entryFor(newKey)
	assert.True(t, ok)
	a.Abandon()
	for _, h := range held[1:] {
		h.Abandon()
	}
	assert.Equal(t, 0, l.addrSize())
}

// TestLimiterCharge: a failure counted with no attempt blocks the key as a
// failed attempt does, and a burst counts no more than a burst of attempts.
func TestLimiterCharge(t *testing.T) {
	clk := newFakeClock()
	l := NewLimiter(clk.now)
	const addr = "203.0.113.9:1"
	key := l.Key(addr, "ops")

	for i := 1; i < maxAttempts; i++ {
		require.True(t, l.Charge(key))
		_, blocked := l.Blocked(key)
		assert.False(t, blocked, "after %d", i)
		assert.Equal(t, maxAttempts-i, places(t, l, key))
	}
	require.True(t, l.Charge(key))
	ra, blocked := l.Blocked(key)
	assert.True(t, blocked)
	assert.Equal(t, baseBackoff, ra)
	// Blocked: nothing more is counted, and the backoff does not move.
	clk.add(400 * time.Millisecond)
	for range 100 {
		require.False(t, l.Charge(key))
	}
	e, _ := l.entryFor(key)
	assert.Equal(t, maxAttempts, e.failures)
	ra, _ = l.Blocked(key)
	assert.Equal(t, 600*time.Millisecond, ra)
	// Past the backoff, one more is counted, and doubles it.
	clk.add(600 * time.Millisecond)
	require.True(t, l.Charge(key))
	require.False(t, l.Charge(key))
	ra, _ = l.Blocked(key)
	assert.Equal(t, 2*baseBackoff, ra)
	// It mixes with failed attempts, and a Succeed clears both.
	clk.add(ra)
	failOnce(t, l, key)
	e, _ = l.entryFor(key)
	assert.Equal(t, maxAttempts+2, e.failures)
	clk.add(4 * baseBackoff)
	a, ok := l.Reserve(key, anyAddr())
	require.True(t, ok)
	a.Succeed()
	_, tracked := l.entryFor(key)
	assert.False(t, tracked)

	// It takes no place, of the key or of the address, and is not held back
	// by a full address.
	held := make([]*Attempt, 0, addrInFlight)
	for i := range addrInFlight {
		a, ok := l.Reserve(l.Key(addr, fmt.Sprintf("ghost%d", i)), addr)
		require.True(t, ok)
		held = append(held, a)
	}
	require.True(t, l.Charge(key))
	e, _ = l.entryFor(key)
	assert.Equal(t, entry{failures: 1, lastFail: clk.now()}, e)
	n, _ := l.addrCount(addr)
	assert.Equal(t, addrInFlight, n)
	for _, h := range held {
		h.Abandon()
	}

	// A burst counts maxAttempts and is refused past that.
	l = NewLimiter(nil)
	key = l.Key(addr, "ops")
	var (
		counted atomic.Int64
		wg      sync.WaitGroup
	)
	for range 200 {
		wg.Go(func() {
			if l.Charge(key) {
				counted.Add(1)
			}
		})
	}
	wg.Wait()
	assert.EqualValues(t, maxAttempts, counted.Load())
	e, _ = l.entryFor(key)
	assert.Equal(t, maxAttempts, e.failures)
}

// TestLimiterChargeFullTable: a Charge evicts only to insert its key, and a
// refused one leaves a full table as it was.
func TestLimiterChargeFullTable(t *testing.T) {
	clk := newFakeClock()
	l := NewLimiter(clk.now)
	keys := make([]string, maxKeys)
	for i := range keys {
		keys[i] = l.Key(fmt.Sprintf("10.%d.%d.1:1", i/256, i%256), "ops")
		clk.add(time.Millisecond)
		failOnce(t, l, keys[i])
	}
	last := keys[len(keys)-1]
	for range maxAttempts - 1 {
		failOnce(t, l, last)
	}
	before := l.keySet()
	require.False(t, l.Charge(last), "blocked")
	assert.Equal(t, before, l.keySet(), "a refused Charge changed the table")

	// A tracked key is counted in place.
	require.True(t, l.Charge(keys[1]))
	assert.Equal(t, maxKeys, l.size())
	_, ok := l.entryFor(keys[0])
	assert.True(t, ok, "a Charge of a tracked key evicted")
	// A new key evicts the oldest under 5 failures.
	fresh := l.Key("192.168.0.1:1", "ops")
	require.True(t, l.Charge(fresh))
	assert.Equal(t, maxKeys, l.size())
	_, ok = l.entryFor(keys[0])
	assert.False(t, ok)
	e, ok := l.entryFor(fresh)
	require.True(t, ok)
	assert.Equal(t, 1, e.failures)

	// When every key has an attempt in flight, a new key cannot be counted.
	l = NewLimiter(clk.now)
	for i := range maxKeys {
		_, ok := l.Reserve(l.Key(fmt.Sprintf("10.%d.%d.1:1", i/256, i%256), "ops"), anyAddr())
		require.True(t, ok)
	}
	assert.False(t, l.Charge(fresh))
	assert.Equal(t, maxKeys, l.size())
}
