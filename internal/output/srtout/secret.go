package srtout

import (
	"net"
	"time"
)

const (
	// maxSecretFailures is how many wrong passphrases a peer key may send
	// inside secretWindow before its encrypted requests are refused
	// unchecked.
	maxSecretFailures = 5
	// secretWindow is how long a peer key's failures count, from its first
	// counted failure.
	secretWindow = 10 * time.Second
	// maxSecretKeys bounds the peer keys the failure table tracks.
	maxSecretKeys = 4096
)

// failure is one peer key's wrong passphrases: how many, and when the first
// of them was counted.
type failure struct {
	count int
	first time.Time
}

// failures is the table of wrong passphrases by peer key. A key whose entry
// counts maxSecretFailures is limited until its entry expires, secretWindow
// after its first counted failure; a correct passphrase clears nothing. When
// the table holds maxSecretKeys keys, a failure of a new key first drops the
// expired entries, and is not tracked when that frees no room. It is
// guarded by Server.mu.
type failures struct {
	now func() time.Time      // the clock; a seam of the tests
	key func(net.Addr) string // a remote address's peer key; a seam of the tests
	m   map[string]failure
}

func newFailures() failures {
	return failures{now: time.Now, key: peerKey, m: map[string]failure{}}
}

// live reports whether e has not expired at now.
func live(e failure, now time.Time) bool { return now.Before(e.first.Add(secretWindow)) }

// limited reports whether key's encrypted requests are refused unchecked.
func (f *failures) limited(key string) bool {
	e, ok := f.m[key]
	return ok && e.count >= maxSecretFailures && live(e, f.now())
}

// fail counts a wrong passphrase from key.
func (f *failures) fail(key string) {
	now := f.now()
	e, ok := f.m[key]
	switch {
	case ok && live(e, now):
		e.count++
		f.m[key] = e
		return
	case !ok && len(f.m) >= maxSecretKeys:
		for k, e := range f.m {
			if !live(e, now) {
				delete(f.m, k)
			}
		}
		if len(f.m) >= maxSecretKeys {
			return
		}
	}
	f.m[key] = failure{count: 1, first: now}
}
