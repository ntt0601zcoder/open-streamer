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

// addrInFlight is how many unresolved attempts one client address may hold.
const addrInFlight = 4

// entry is one key's state. An entry with no failures and nothing in
// flight is the same as no entry, and is deleted.
type entry struct {
	failures int
	inFlight int
	lastFail time.Time
}

// Limiter counts failed logins per client address and presented user name
// and refuses further attempts while a key is in a backoff. It also caps
// the unresolved attempts of one client address, whatever names it
// presents, at addrInFlight. It is safe for concurrent use and starts no
// goroutine: it prunes when it is called.
type Limiter struct {
	now    func() time.Time
	secret []byte // HMAC key of Key, per Limiter

	mu        sync.Mutex
	entries   map[string]*entry
	lastSweep time.Time
	// addrs is the unresolved attempts per ClientKey. An address with none
	// has no entry, so the attempts in flight bound the map: it has no
	// eviction, no idle rule and no sweep, and it never counts failures.
	addrs map[string]int
}

// NewLimiter returns an empty Limiter reading the time from now; nil
// means time.Now.
func NewLimiter(now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	secret := make([]byte, 32)
	randomBytes(secret)
	return &Limiter{
		now:     now,
		secret:  secret,
		entries: make(map[string]*entry),
		addrs:   make(map[string]int),
	}
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

// Reserve takes one in-flight attempt for key and one of the addrInFlight
// places of the client of remoteAddr (ClientKey), or neither. The address
// is judged first: a refusal by its cap looks up, inserts and evicts
// nothing in the name table. Then key is judged exactly as before.
//
// The address: a client with addrInFlight unresolved attempts is refused,
// whatever name it presents, until one of them is resolved. Nothing else
// is kept per address: no failure is counted against it and it is never
// blocked, because behind a shared address (a reverse proxy, a NAT) a
// block would let one client lock every other out.
//
// The key: it is refused when it is blocked or when its counted failures
// plus its attempts in flight are already 5. Past 5 failures, once the
// backoff has run out, failures count as 4, so the key gets one attempt at
// a time. A refusal by the key takes no place of the address.
//
// The caller defers attempt.Abandon() right after a successful Reserve:
// an unresolved Attempt holds its two places for ever, and Abandon after
// Succeed or Fail does nothing.
//
// Eviction: a key the table does not hold is always admitted (it has no
// failures and nothing in flight), so Reserve evicts only when it is about
// to insert, never for a refusal. When the table is full it drops, among
// the keys with nothing in flight, the one with the oldest last failure
// under 5 failures, or failing that the one with the oldest last failure
// at 5 or more. So a client that wants its own block evicted must first
// make every other key of the table a newer block: about 20 000 failed
// verifications (4096 keys at 5 failures each), each a full PBKDF2, for
// one block; other names with fewer failures only evict each other. When
// every key has an attempt in flight, Reserve refuses. The
// new key's attempt may still end in ErrBusy and be abandoned, which
// deletes the new key; that leaves a vacancy the next new key fills
// without evicting, so abandoned attempts cost at most one eviction per
// vacancy they create, and only failures that are counted grow the table.
func (l *Limiter) Reserve(key, remoteAddr string) (*Attempt, bool) {
	addr := ClientKey(remoteAddr)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.addrs[addr] >= addrInFlight {
		return nil, false
	}
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
	l.addrs[addr]++
	return &Attempt{l: l, key: key, addr: addr}, true
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
	addr string // the ClientKey whose place it holds
	done bool   // guarded by l.mu
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
	// The address's place goes back first, so that it cannot leak whatever
	// became of the name entry.
	if n := l.addrs[a.addr] - 1; n > 0 {
		l.addrs[a.addr] = n
	} else {
		delete(l.addrs, a.addr)
	}
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

// Succeed gives both places back and zeroes the key's failures. Only a real
// verification calls it; a remembered login touches no limiter state.
func (a *Attempt) Succeed() { a.resolve(succeeded) }

// Fail gives both places back, counts one failure against the key and
// starts or lengthens its backoff. Nothing is counted against the address.
func (a *Attempt) Fail() { a.resolve(failed) }

// Abandon gives both places back and counts nothing: the request went away,
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
