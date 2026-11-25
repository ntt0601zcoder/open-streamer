package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/netip"
	"sync"
	"time"
)

// The limiter's bounds.
const (
	// maxAttempts is how many failures plus attempts in flight a key may
	// have before Reserve refuses, and the failure count at which the
	// backoff starts.
	maxAttempts = 5
	// baseBackoff is the backoff at maxAttempts failures; each further
	// failure doubles it, up to maxBackoff.
	baseBackoff = time.Second
	maxBackoff  = 5 * time.Minute
	// idleAfter is how long after its last failure a key with nothing in
	// flight is forgotten.
	idleAfter = 15 * time.Minute
	// maxKeys bounds the table.
	maxKeys = 4096
	// sweepEvery is how often a call also sweeps idle keys out.
	sweepEvery = time.Minute
	// maxFailures caps the counter; the backoff is at its ceiling long
	// before.
	maxFailures = 1 << 20
)

// entry is one key's state. An entry with no failures and nothing in
// flight is the same as no entry, and is deleted.
type entry struct {
	failures int
	inFlight int
	lastFail time.Time
}

// Limiter counts failed logins per client address and presented user name
// and refuses further attempts while a key is in a backoff. It is safe for
// concurrent use and starts no goroutine: it prunes when it is called.
type Limiter struct {
	now    func() time.Time
	secret []byte // HMAC key of Key, per Limiter

	mu        sync.Mutex
	entries   map[string]*entry
	lastSweep time.Time
}

// NewLimiter returns an empty Limiter reading the time from now; nil
// means time.Now.
func NewLimiter(now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	secret := make([]byte, 32)
	randomBytes(secret)
	return &Limiter{now: now, secret: secret, entries: make(map[string]*entry)}
}

// Key is the limiter's key for a client and the user name it presents:
// hex(HMAC-SHA-256(key, ClientKey ‖ 0x00 ‖ user)), the HMAC key random per
// Limiter. Hashed, so the table holds no names and a long name costs no
// memory.
func (l *Limiter) Key(remoteAddr, user string) string {
	m := hmac.New(sha256.New, l.secret)
	m.Write([]byte(ClientKey(remoteAddr)))
	m.Write([]byte{0})
	m.Write([]byte(user))
	return hex.EncodeToString(m.Sum(nil))
}

// backoff is the block after the last failure for a count of failures, or
// 0 under maxAttempts.
func backoff(failures int) time.Duration {
	if failures < maxAttempts {
		return 0
	}
	d := baseBackoff
	for i := maxAttempts; i < failures && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}

// lookup returns key's entry, forgetting it first when it is idle. The
// caller holds l.mu.
func (l *Limiter) lookup(key string, now time.Time) *entry {
	if now.Sub(l.lastSweep) >= sweepEvery {
		l.lastSweep = now
		for k, e := range l.entries {
			if idle(e, now) {
				delete(l.entries, k)
			}
		}
	}
	e := l.entries[key]
	if e != nil && idle(e, now) {
		delete(l.entries, key)
		return nil
	}
	return e
}

func idle(e *entry, now time.Time) bool {
	return e.inFlight == 0 && now.Sub(e.lastFail) >= idleAfter
}

// blockedFor is how long e stays blocked. The caller holds l.mu.
func blockedFor(e *entry, now time.Time) time.Duration {
	if e == nil || e.failures < maxAttempts {
		return 0
	}
	left := e.lastFail.Add(backoff(e.failures)).Sub(now)
	return max(left, 0)
}

// Blocked reports whether key is in a backoff now, and for how long.
// Blocked is true for min(1 s × 2^(failures−5), 5 min) after the last
// failure once failures ≥ 5.
func (l *Limiter) Blocked(key string) (retryAfter time.Duration, blocked bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	d := blockedFor(l.lookup(key, now), now)
	return d, d > 0
}

// Reserve takes one in-flight attempt for key, unless the key is blocked
// or its counted failures plus its attempts in flight are already 5. Past
// 5 failures, once the backoff has run out, failures count as 4, so the
// key gets one attempt at a time.
//
// The caller defers attempt.Abandon() right after a successful Reserve:
// an unresolved Attempt holds its place for ever, and Abandon after
// Succeed or Fail does nothing.
//
// Eviction: a key the table does not hold is always admitted (it has no
// failures and nothing in flight), so Reserve evicts only when it is about
// to insert, never for a refusal. When the table is full it drops, among
// the keys with nothing in flight, the one with the oldest last failure
// under 5 failures, or failing that the one with the oldest last failure
// at 5 or more, so a client cannot flush its own block out with other
// names. When every key has an attempt in flight, Reserve refuses. The
// new key's attempt may still end in ErrBusy and be abandoned, which
// deletes the new key; that leaves a vacancy the next new key fills
// without evicting, so abandoned attempts cost at most one eviction per
// vacancy they create, and only failures that are counted grow the table.
func (l *Limiter) Reserve(key string) (*Attempt, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.lookup(key, now)
	if blockedFor(e, now) > 0 {
		return nil, false
	}
	if e == nil {
		if len(l.entries) >= maxKeys && !l.evict() {
			return nil, false
		}
		e = &entry{}
		l.entries[key] = e
	}
	if min(e.failures, maxAttempts-1)+e.inFlight >= maxAttempts {
		return nil, false
	}
	e.inFlight++
	return &Attempt{l: l, key: key}, true
}

// evict drops, among the keys with nothing in flight, the one with the
// oldest last failure under maxAttempts failures, or when there is none
// the one with the oldest last failure at maxAttempts or more. It reports
// false when every key has an attempt in flight. The caller holds l.mu.
func (l *Limiter) evict() bool {
	var (
		victim string
		best   *entry
	)
	for k, e := range l.entries {
		if e.inFlight > 0 {
			continue
		}
		if best == nil || evictBefore(e, best) {
			victim, best = k, e
		}
	}
	if best == nil {
		return false
	}
	delete(l.entries, victim)
	return true
}

// evictBefore reports whether a goes before b: a key under maxAttempts
// failures before one at or over it, then the older last failure.
func evictBefore(a, b *entry) bool {
	aLow, bLow := a.failures < maxAttempts, b.failures < maxAttempts
	if aLow != bLow {
		return aLow
	}
	return a.lastFail.Before(b.lastFail)
}

// dropIfEmpty deletes an entry that holds nothing. An entry with an
// attempt in flight is never deleted, so an Attempt always finds its
// entry. The caller holds l.mu.
func (l *Limiter) dropIfEmpty(key string, e *entry) {
	if e.failures == 0 && e.inFlight == 0 {
		delete(l.entries, key)
	}
}

// Attempt is one reserved login attempt. Exactly one of Succeed, Fail and
// Abandon resolves it; later calls do nothing, so the caller can defer
// Abandon right after Reserve and still call Succeed or Fail. All three
// do nothing on a nil *Attempt.
type Attempt struct {
	l    *Limiter
	key  string
	done bool // guarded by l.mu
}

type outcome int

const (
	succeeded outcome = iota
	failed
	abandoned
)

func (a *Attempt) resolve(o outcome) {
	if a == nil || a.l == nil {
		return
	}
	l := a.l
	l.mu.Lock()
	defer l.mu.Unlock()
	if a.done {
		return
	}
	a.done = true
	e := l.entries[a.key]
	if e == nil {
		return // unreachable: an entry with an attempt in flight is never deleted
	}
	e.inFlight--
	switch o {
	case succeeded:
		e.failures = 0
	case failed:
		e.failures = min(e.failures+1, maxFailures)
		e.lastFail = l.now()
	case abandoned:
	}
	l.dropIfEmpty(a.key, e)
}

// Succeed gives the place back and zeroes the key's failures. Only a real
// verification calls it; a remembered login touches no limiter state.
func (a *Attempt) Succeed() { a.resolve(succeeded) }

// Fail gives the place back, counts one failure and starts or lengthens
// the backoff.
func (a *Attempt) Fail() { a.resolve(failed) }

// Abandon gives the place back and counts nothing: the request went away,
// or the server was busy.
func (a *Attempt) Abandon() { a.resolve(abandoned) }

// ClientKey is the client part of a limiter key: the IP of a host:port (or
// of a bare address), Unmap()ped first, the zone dropped, and an IPv6
// address masked to its /64; the input itself when it does not parse.
func ClientKey(remoteAddr string) string {
	var addr netip.Addr
	if ap, err := netip.ParseAddrPort(remoteAddr); err == nil {
		addr = ap.Addr()
	} else if a, err := netip.ParseAddr(remoteAddr); err == nil {
		addr = a
	} else {
		return remoteAddr
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is4() {
		return addr.String()
	}
	p, err := addr.Prefix(64)
	if err != nil {
		return remoteAddr // unreachable: a valid IPv6 address has a /64
	}
	return p.String()
}
