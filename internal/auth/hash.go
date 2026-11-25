package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// The hash format and the limits on it.
const (
	Prefix           = "pbkdf2-sha256"
	Iterations       = 600000 // what HashPassword uses
	MinIterations    = 600000 // what a config file must have
	MaxIterations    = 10000000
	SaltBytes        = 16
	KeyBytes         = 32
	MinPasswordBytes = 12
	MaxUsers         = 64
)

// minSaltBytes is the shortest salt ParseHash and NewUsers accept.
const minSaltBytes = 8

// b64 is the encoding of salt and key: standard alphabet, no padding,
// strict about trailing bits.
var b64 = base64.RawStdEncoding.Strict()

// Hash is a parsed PBKDF2-SHA-256 password hash.
type Hash struct {
	Iterations int
	Salt, Key  []byte
}

// HashPassword hashes password with a random salt of SaltBytes at
// Iterations. A password under MinPasswordBytes is an error.
func HashPassword(password string) (string, error) {
	if len(password) < MinPasswordBytes {
		return "", fmt.Errorf("auth: hash password: shorter than %d bytes", MinPasswordBytes)
	}
	salt := make([]byte, SaltBytes)
	randomBytes(salt)
	h := NewHash(password, salt, Iterations)
	if len(h.Key) != KeyBytes {
		return "", errors.New("auth: hash password: key derivation failed")
	}
	return h.String(), nil
}

// NewHash derives the hash of password with salt at iterations. When the
// derivation fails (only possible in FIPS 140-only mode, or for an
// iteration count under 1) the Key is nil, and such a Hash matches nothing.
func NewHash(password string, salt []byte, iterations int) Hash {
	return Hash{
		Iterations: iterations,
		Salt:       append([]byte(nil), salt...),
		Key:        derive(password, salt, iterations),
	}
}

// String formats h as pbkdf2-sha256$<iterations>$<salt>$<key>, salt and
// key in base64.RawStdEncoding.
func (h Hash) String() string {
	return Prefix + "$" + strconv.Itoa(h.Iterations) + "$" + b64.EncodeToString(h.Salt) + "$" + b64.EncodeToString(h.Key)
}

// ParseHash refuses anything but the format String writes, a salt under 8
// bytes, a key that is not KeyBytes long, and an iteration count outside
// [minIterations, MaxIterations] (a floor under 1 counts as 1). Its errors
// say what is wrong and never contain s, so a password pasted where a hash
// belongs does not reach a log.
func ParseHash(s string, minIterations int) (Hash, error) {
	minIterations = max(minIterations, 1)
	fields := strings.Split(s, "$")
	if len(fields) != 4 {
		return Hash{}, errors.New("auth: parse hash: want four fields separated by dollar signs")
	}
	if fields[0] != Prefix {
		return Hash{}, errors.New("auth: parse hash: unsupported scheme")
	}
	iter, ok := parseDecimal(fields[1])
	if !ok {
		return Hash{}, errors.New("auth: parse hash: iteration count is not a plain decimal number")
	}
	if iter < minIterations || iter > MaxIterations {
		return Hash{}, errors.New("auth: parse hash: iteration count out of range")
	}
	salt, err := b64.DecodeString(fields[2])
	if err != nil {
		return Hash{}, errors.New("auth: parse hash: salt is not unpadded standard base64")
	}
	if len(salt) < minSaltBytes {
		return Hash{}, errors.New("auth: parse hash: salt too short")
	}
	key, err := b64.DecodeString(fields[3])
	if err != nil {
		return Hash{}, errors.New("auth: parse hash: key is not unpadded standard base64")
	}
	if len(key) != KeyBytes {
		return Hash{}, errors.New("auth: parse hash: key has the wrong length")
	}
	return Hash{Iterations: iter, Salt: salt, Key: key}, nil
}

// parseDecimal accepts only digits, no sign and no leading zero, of at
// most 9 digits (MaxIterations has 8), so it cannot overflow.
func parseDecimal(s string) (int, bool) {
	if s == "" || len(s) > 9 || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	n := 0
	for i := range len(s) {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// Verify derives password's key with h's salt and iterations and compares
// it to h.Key in constant time. A Hash without a key matches nothing.
func (h Hash) Verify(password string) bool {
	got := derive(password, h.Salt, h.Iterations)
	if len(h.Key) == 0 || len(got) != len(h.Key) {
		return false
	}
	return subtle.ConstantTimeCompare(got, h.Key) == 1
}

// derive is every PBKDF2 computation of the package. It returns nil when
// the derivation fails.
func derive(password string, salt []byte, iterations int) []byte {
	next := func() []byte {
		if iterations < 1 {
			return nil
		}
		k, err := pbkdf2.Key(sha256.New, password, salt, iterations, KeyBytes)
		if err != nil {
			return nil
		}
		return k
	}
	if f := hashHook.Load(); f != nil {
		return (*f)(next)
	}
	return next()
}

// hashHook is the test seam of SetHashHook.
var hashHook atomic.Pointer[func(next func() []byte) []byte]

// SetHashHook is for tests only; no production code calls it. While set,
// every PBKDF2 computation of the package goes through f, which gets the
// real computation as next and may count it, hold it, or both. It returns
// the function that removes the hook (restoring the one before it); a test
// calls it in t.Cleanup. Tests that use it must not run in parallel with
// anything.
func SetHashHook(f func(next func() []byte) []byte) (restore func()) {
	var p *func(next func() []byte) []byte
	if f != nil {
		p = &f
	}
	prev := hashHook.Swap(p)
	return func() { hashHook.Store(prev) }
}

// dummyBox holds the per-process dummy hash an unknown name is verified
// against, so a wrong name costs what a wrong password costs.
type dummyBox struct {
	once sync.Once
	h    Hash
}

var dummyCell atomic.Pointer[dummyBox]

func init() { dummyCell.Store(&dummyBox{}) }

// dummyHash returns the dummy hash, making it on first use at Iterations
// with a random password and salt.
func dummyHash() Hash {
	b := dummyCell.Load()
	b.once.Do(func() {
		pw := make([]byte, 32)
		salt := make([]byte, SaltBytes)
		randomBytes(pw)
		randomBytes(salt)
		b.h = NewHash(string(pw), salt, Iterations)
	})
	return b.h
}

// WarmDummy is for tests only; no production code calls it. It makes the
// per-process dummy hash now, if it does not exist yet.
func WarmDummy() { dummyHash() }

// randomBytes fills b from crypto/rand, which never fails on a supported
// platform; a failure is not recoverable.
func randomBytes(b []byte) {
	if _, err := rand.Read(b); err != nil {
		panic("auth: crypto/rand failed: " + err.Error())
	}
}
