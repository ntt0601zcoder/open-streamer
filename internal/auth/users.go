package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
)

// Role is what a user may do.
type Role string

// The two roles: a viewer reads, an editor reads and writes.
const (
	Viewer Role = "viewer"
	Editor Role = "editor"
)

// Allows reports whether r may do what need may: an editor allows both
// roles, a viewer allows viewer. An unknown role allows nothing and is
// allowed by nothing.
func (r Role) Allows(need Role) bool {
	if need != Viewer && need != Editor {
		return false
	}
	switch r {
	case Editor:
		return true
	case Viewer:
		return need == Viewer
	default:
		return false
	}
}

// User is one entry of the users section of the config file.
type User struct {
	Name string
	Role Role
	Hash Hash
}

// The errors of Verify.
var (
	ErrBadCredentials = errors.New("auth: bad credentials")
	ErrBusy           = errors.New("auth: too many logins waiting")
)

// Verify's process-wide bounds: at most maxRunning PBKDF2 at once and at
// most maxWaiting callers queued behind them.
const (
	maxRunning = 2
	maxWaiting = 16
)

var (
	// verifyAdmit holds a place for each caller running or waiting.
	verifyAdmit = make(chan struct{}, maxRunning+maxWaiting)
	// verifyRun holds a place for each caller running PBKDF2.
	verifyRun = make(chan struct{}, maxRunning)
)

// rememberKey keys the remembered-login tags. It is made once per process
// and never leaves it.
var rememberKey = sync.OnceValue(func() []byte {
	k := make([]byte, 32)
	randomBytes(k)
	return k
})

type userEntry struct {
	role Role
	hash Hash
	str  string // hash.String(), for Equal
}

// Users is an immutable set of users, apart from the logins it remembers.
type Users struct {
	byName map[string]userEntry

	mu   sync.Mutex
	tags map[string][sha256.Size]byte // name → tag of its last verified password
}

// NewUsers refuses: a name not matching ^[a-z0-9][a-z0-9._-]{0,31}$, a
// duplicate name, a role other than viewer or editor, more than MaxUsers,
// a list without an editor, and a hash with no usable key, salt or
// iteration count. It runs no PBKDF2. Its errors never contain a hash.
func NewUsers(list []User) (*Users, error) {
	if len(list) > MaxUsers {
		return nil, fmt.Errorf("auth: users: more than %d users", MaxUsers)
	}
	u := &Users{
		byName: make(map[string]userEntry, len(list)),
		tags:   make(map[string][sha256.Size]byte),
	}
	editor := false
	for i, usr := range list {
		if !validName(usr.Name) {
			return nil, fmt.Errorf("auth: users: user %d: name must match ^[a-z0-9][a-z0-9._-]{0,31}$", i+1)
		}
		if _, dup := u.byName[usr.Name]; dup {
			return nil, fmt.Errorf("auth: users: user %q appears twice", usr.Name)
		}
		switch usr.Role {
		case Editor:
			editor = true
		case Viewer:
		default:
			return nil, fmt.Errorf("auth: users: user %q: role must be viewer or editor", usr.Name)
		}
		h := usr.Hash
		if h.Iterations < 1 || h.Iterations > MaxIterations || len(h.Salt) < minSaltBytes || len(h.Key) != KeyBytes {
			return nil, fmt.Errorf("auth: users: user %q: unusable password hash", usr.Name)
		}
		h = Hash{
			Iterations: h.Iterations,
			Salt:       append([]byte(nil), h.Salt...),
			Key:        append([]byte(nil), h.Key...),
		}
		u.byName[usr.Name] = userEntry{role: usr.Role, hash: h, str: h.String()}
	}
	if !editor {
		return nil, errors.New("auth: users: no editor")
	}
	return u, nil
}

// validName reports whether name matches ^[a-z0-9][a-z0-9._-]{0,31}$.
func validName(name string) bool {
	if len(name) == 0 || len(name) > 32 {
		return false
	}
	for i := range len(name) {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case i > 0 && (c == '.' || c == '_' || c == '-'):
		default:
			return false
		}
	}
	return true
}

// Len is the number of users.
func (u *Users) Len() int { return len(u.byName) }

// Equal reports whether list has the same names with the same role and the
// same hash string as u, in any order.
func (u *Users) Equal(list []User) bool {
	if len(list) != len(u.byName) {
		return false
	}
	seen := make(map[string]struct{}, len(list))
	for _, usr := range list {
		e, ok := u.byName[usr.Name]
		if !ok || e.role != usr.Role || e.str != usr.Hash.String() {
			return false
		}
		if _, dup := seen[usr.Name]; dup {
			return false
		}
		seen[usr.Name] = struct{}{}
	}
	return true
}

// tag is HMAC-SHA-256(rememberKey, name ‖ 0x00 ‖ password). A valid name
// holds no 0x00, so the split is unambiguous.
func tag(name, password string) [sha256.Size]byte {
	m := hmac.New(sha256.New, rememberKey())
	m.Write([]byte(name))
	m.Write([]byte{0})
	m.Write([]byte(password))
	var t [sha256.Size]byte
	m.Sum(t[:0])
	return t
}

// Remembered reports a login this set has already verified: no PBKDF2,
// constant time in the password. It is never true for a wrong password.
func (u *Users) Remembered(name, password string) (Role, bool) {
	role, m := u.Recall(name, password)
	return role, m == Match
}

// Memory is what a set remembers of a name, for a presented password.
type Memory int

// The answers of Recall.
const (
	// Unknown: the set remembers no login of the name. The password may be
	// right or wrong: only Verify tells.
	Unknown Memory = iota
	// Match: the password is the one the set verified for the name.
	Match
	// Mismatch: the set remembers a login of the name, and the password is
	// another one. It is a wrong password: a set is immutable, so the hash
	// that verified the remembered password is still the name's hash, and a
	// user has one password.
	Mismatch
)

// Recall is Remembered that also tells a wrong password apart: no PBKDF2,
// constant time in the password, and one reading of the memory, so a login
// that is being remembered meanwhile is either Unknown or Match, never
// Mismatch for the right password.
//
// A changed password never meets the old memory: the users are immutable,
// so a new hash comes with a new set, which remembers nothing.
func (u *Users) Recall(name, password string) (Role, Memory) {
	got := tag(name, password)
	u.mu.Lock()
	want, has := u.tags[name]
	u.mu.Unlock()
	// Compare even when there is no tag, so the work does not depend on it.
	eq := hmac.Equal(got[:], want[:])
	e, ok := u.byName[name]
	switch {
	case !has || !ok:
		return "", Unknown
	case !eq:
		return "", Mismatch
	default:
		return e.role, Match
	}
}

// Verify runs PBKDF2 (against the dummy hash for an unknown name) and
// remembers a success, replacing the user's older remembered login. At
// most 2 run at once and at most 16 wait, in the whole process: the slots
// are package-level, not per Users, so replacing the user set on a reload
// neither resets the queue nor doubles the CPU. Beyond that it returns
// ErrBusy at once. A waiter whose ctx ends leaves the queue and gets
// ctx.Err().
func (u *Users) Verify(ctx context.Context, name, password string) (Role, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	select {
	case verifyAdmit <- struct{}{}:
	default:
		return "", ErrBusy
	}
	defer func() { <-verifyAdmit }()
	select {
	case verifyRun <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-verifyRun }()
	if err := ctx.Err(); err != nil {
		return "", err
	}

	e, known := u.byName[name]
	h := e.hash
	if !known {
		h = dummyHash()
	}
	if !h.Verify(password) || !known {
		return "", ErrBadCredentials
	}
	t := tag(name, password)
	u.mu.Lock()
	u.tags[name] = t
	u.mu.Unlock()
	return e.role, nil
}
