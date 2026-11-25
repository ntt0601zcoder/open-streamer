package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testHash hashes at 1000 iterations so the tests stay fast; the dummy
// hash for unknown names is still made at Iterations.
func testHash(pw string) Hash {
	return NewHash(pw, []byte("0123456789abcdef"), 1000)
}

const (
	edPass = "editor password 1"
	viPass = "viewer password 1"
)

func testUsers(t *testing.T) *Users {
	t.Helper()
	u, err := NewUsers([]User{
		{Name: "ops", Role: Editor, Hash: testHash(edPass)},
		{Name: "watch", Role: Viewer, Hash: testHash(viPass)},
	})
	require.NoError(t, err)
	return u
}

// countHashes sets a hook that counts PBKDF2 computations. It warms the
// dummy first, so the count never includes the dummy's making.
func countHashes(t *testing.T) *atomic.Int64 {
	t.Helper()
	WarmDummy()
	var n atomic.Int64
	restore := SetHashHook(func(next func() []byte) []byte {
		n.Add(1)
		return next()
	})
	t.Cleanup(restore)
	return &n
}

func TestRoleAllows(t *testing.T) {
	assert.True(t, Editor.Allows(Editor))
	assert.True(t, Editor.Allows(Viewer))
	assert.True(t, Viewer.Allows(Viewer))
	assert.False(t, Viewer.Allows(Editor))
	assert.False(t, Role("admin").Allows(Viewer))
	assert.False(t, Role("").Allows(Viewer))
	assert.False(t, Editor.Allows(Role("admin")))
	assert.False(t, Editor.Allows(Role("")))
}

func TestNewUsers(t *testing.T) {
	h := testHash("some password 1")
	ed := func(name string) User { return User{Name: name, Role: Editor, Hash: h} }
	bad := map[string][]User{
		"empty name":   {ed("")},
		"upper":        {ed("Bob")},
		"space":        {ed("a b")},
		"33 bytes":     {ed(strings.Repeat("a", 33))},
		"leading dot":  {ed(".ops")},
		"leading dash": {ed("-ops")},
		"nul":          {ed("ops\x00")},
		"colon":        {ed("ops:x")},
		"duplicate":    {ed("ops"), ed("ops")},
		"role admin":   {ed("ops"), {Name: "x", Role: "admin", Hash: h}},
		"role empty":   {ed("ops"), {Name: "x", Role: "", Hash: h}},
		"viewers only": {{Name: "x", Role: Viewer, Hash: h}},
		"none":         {},
		"no key":       {{Name: "ops", Role: Editor, Hash: Hash{Iterations: 1000, Salt: h.Salt}}},
		"no iter":      {{Name: "ops", Role: Editor, Hash: Hash{Salt: h.Salt, Key: h.Key}}},
		"short salt":   {{Name: "ops", Role: Editor, Hash: Hash{Iterations: 1000, Salt: []byte("0123456789abcde"), Key: h.Key}}},
		"iter high":    {{Name: "ops", Role: Editor, Hash: Hash{Iterations: MaxIterations + 1, Salt: h.Salt, Key: h.Key}}},
	}
	many := make([]User, 0, MaxUsers+1)
	for i := range MaxUsers + 1 {
		many = append(many, ed(fmt.Sprintf("u%d", i)))
	}
	bad["65 users"] = many
	for name, list := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := NewUsers(list)
			require.Error(t, err)
		})
	}

	u, err := NewUsers([]User{ed("ops")})
	require.NoError(t, err)
	assert.Equal(t, 1, u.Len())

	u, err = NewUsers(many[:MaxUsers])
	require.NoError(t, err)
	assert.Equal(t, MaxUsers, u.Len())

	_, err = NewUsers([]User{ed(strings.Repeat("a", 32)), ed("0.a_b-c")})
	require.NoError(t, err)
}

// TestNewUsersCopies: a Users does not share the caller's slices.
func TestNewUsersCopies(t *testing.T) {
	h := testHash(edPass)
	list := []User{{Name: "ops", Role: Editor, Hash: h}}
	u, err := NewUsers(list)
	require.NoError(t, err)
	list[0].Hash.Key[0] ^= 0xff
	list[0].Role = Viewer
	role, err := u.Verify(context.Background(), "ops", edPass)
	require.NoError(t, err)
	assert.Equal(t, Editor, role)
}

func TestVerifyAndRemember(t *testing.T) {
	u := testUsers(t)
	n := countHashes(t)
	ctx := context.Background()

	_, ok := u.Remembered("ops", edPass)
	assert.False(t, ok, "remembered before any Verify")

	base := n.Load()
	role, err := u.Verify(ctx, "ops", edPass)
	require.NoError(t, err)
	assert.Equal(t, Editor, role)
	assert.Equal(t, base+1, n.Load())

	base = n.Load()
	role, ok = u.Remembered("ops", edPass)
	assert.True(t, ok)
	assert.Equal(t, Editor, role)
	_, ok = u.Remembered("ops", edPass+"x")
	assert.False(t, ok)
	_, ok = u.Remembered("ops", "")
	assert.False(t, ok)
	_, ok = u.Remembered("watch", edPass)
	assert.False(t, ok, "a tag is per user")
	_, ok = u.Remembered("nobody", edPass)
	assert.False(t, ok)
	assert.Equal(t, base, n.Load(), "Remembered runs PBKDF2")

	for _, c := range []struct{ name, pw string }{
		{"ops", "wrong password 1"},
		{"nobody", edPass},
		{"", edPass},
		{"watch", edPass},
	} {
		base = n.Load()
		_, err = u.Verify(ctx, c.name, c.pw)
		assert.ErrorIs(t, err, ErrBadCredentials, "%q", c.name)
		assert.Equal(t, base+1, n.Load(), "%q: one PBKDF2 per Verify", c.name)
	}

	// A failed Verify neither remembers its password nor forgets the
	// remembered one.
	_, ok = u.Remembered("ops", "wrong password 1")
	assert.False(t, ok)
	_, ok = u.Remembered("ops", edPass)
	assert.True(t, ok)
	_, ok = u.Remembered("watch", edPass)
	assert.False(t, ok)

	role, err = u.Verify(ctx, "watch", viPass)
	require.NoError(t, err)
	assert.Equal(t, Viewer, role)
	role, ok = u.Remembered("watch", viPass)
	assert.True(t, ok)
	assert.Equal(t, Viewer, role)

	// A remembered tag is per Users: a second set built from the same
	// list starts empty.
	u2 := testUsers(t)
	_, ok = u2.Remembered("ops", edPass)
	assert.False(t, ok)
}

// TestRememberedNameSeparator: the 0x00 between name and password keeps
// one split from posing as another.
func TestRememberedNameSeparator(t *testing.T) {
	u, err := NewUsers([]User{{Name: "ops", Role: Editor, Hash: testHash("x" + edPass)}})
	require.NoError(t, err)
	_, err = u.Verify(context.Background(), "ops", "x"+edPass)
	require.NoError(t, err)
	_, ok := u.Remembered("opsx", edPass)
	assert.False(t, ok)
	_, ok = u.Remembered("ops\x00x", edPass)
	assert.False(t, ok)
}

func TestVerifyContextDone(t *testing.T) {
	u := testUsers(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := u.Verify(ctx, "ops", edPass)
	assert.ErrorIs(t, err, context.Canceled)
	_, ok := u.Remembered("ops", edPass)
	assert.False(t, ok)
}

func TestVerifyBounded(t *testing.T) {
	a, b := testUsers(t), testUsers(t)
	WarmDummy()
	var inside, maxInside atomic.Int64
	release := make(chan struct{})
	restore := SetHashHook(func(next func() []byte) []byte {
		v := inside.Add(1)
		for {
			m := maxInside.Load()
			if v <= m || maxInside.CompareAndSwap(m, v) {
				break
			}
		}
		<-release
		inside.Add(-1)
		return next()
	})
	t.Cleanup(restore)
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseAll)

	type result struct {
		i   int
		err error
	}
	results := make(chan result, 64)
	cancels := make([]context.CancelFunc, 0, 64)
	t.Cleanup(func() {
		for _, c := range cancels {
			c()
		}
	})
	start := func(i int) {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		u := a
		if i%2 == 1 {
			u = b
		}
		go func() {
			_, err := u.Verify(ctx, "ops", "wrong password 1")
			results <- result{i, err}
		}()
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		require.Eventually(t, cond, 5*time.Second, time.Millisecond, what)
	}

	// 2 run: both are inside PBKDF2.
	start(0)
	start(1)
	waitFor("two inside", func() bool { return inside.Load() == 2 })
	// 16 wait: they hold an admission and no run slot.
	for i := 2; i < 18; i++ {
		start(i)
	}
	waitFor("16 waiting", func() bool { return len(verifyAdmit) == maxRunning+maxWaiting })
	// The other 12, half on each Users, are refused at once.
	busy := 0
	for i := 18; i < 30; i++ {
		start(i)
		r := <-results
		require.ErrorIs(t, r.err, ErrBusy)
		require.Equal(t, i, r.i)
		busy++
	}
	assert.Equal(t, 12, busy)
	assert.EqualValues(t, 2, inside.Load())

	// A waiter whose ctx ends leaves with ctx.Err(), and a new caller
	// takes its place.
	cancels[5]()
	r := <-results
	assert.Equal(t, 5, r.i)
	assert.ErrorIs(t, r.err, context.Canceled)
	waitFor("place freed", func() bool { return len(verifyAdmit) == maxRunning+maxWaiting-1 })
	start(30)
	waitFor("place taken", func() bool { return len(verifyAdmit) == maxRunning+maxWaiting })
	start(31)
	r = <-results
	assert.Equal(t, 31, r.i)
	assert.ErrorIs(t, r.err, ErrBusy)

	// Released, every waiter finishes, never more than 2 at once.
	releaseAll()
	for range 17 {
		select {
		case r := <-results:
			assert.ErrorIs(t, r.err, ErrBadCredentials, "caller %d", r.i)
		case <-time.After(10 * time.Second):
			t.Fatal("a waiter did not finish")
		}
	}
	assert.EqualValues(t, 2, maxInside.Load())
	waitFor("slots empty", func() bool { return len(verifyAdmit) == 0 && len(verifyRun) == 0 })
}

func TestDummyOncePerProcess(t *testing.T) {
	resetDummyForTest()
	t.Cleanup(WarmDummy) // runs after the hook is removed: leave a made dummy

	var n atomic.Int64
	restore := SetHashHook(func(next func() []byte) []byte {
		n.Add(1)
		return next()
	})
	t.Cleanup(restore)

	list := []User{
		{Name: "ops", Role: Editor, Hash: testHash(edPass)},
		{Name: "watch", Role: Viewer, Hash: testHash(viPass)},
	}
	base := n.Load()
	var u *Users
	for range 50 {
		var err error
		u, err = NewUsers(list)
		require.NoError(t, err)
	}
	assert.Equal(t, base, n.Load(), "NewUsers runs PBKDF2")

	ctx := context.Background()
	base = n.Load()
	_, err := u.Verify(ctx, "nobody", "any password 1")
	require.ErrorIs(t, err, ErrBadCredentials)
	assert.Equal(t, base+2, n.Load(), "the first unknown name makes the dummy and verifies against it")
	assert.Equal(t, Iterations, dummyHash().Iterations)
	assert.Len(t, dummyHash().Key, KeyBytes)

	base = n.Load()
	_, err = u.Verify(ctx, "nobody", "any password 1")
	require.ErrorIs(t, err, ErrBadCredentials)
	assert.Equal(t, base+1, n.Load())

	base = n.Load()
	WarmDummy()
	assert.Equal(t, base, n.Load(), "WarmDummy remakes the dummy")

	base = n.Load()
	_, err = u.Verify(ctx, "ops", "wrong password 1")
	require.ErrorIs(t, err, ErrBadCredentials)
	assert.Equal(t, base+1, n.Load())
}

func TestUsersEqual(t *testing.T) {
	h1, h2 := testHash("password one 1"), testHash("password two 2")
	list := []User{
		{Name: "ops", Role: Editor, Hash: h1},
		{Name: "watch", Role: Viewer, Hash: h2},
	}
	u, err := NewUsers(list)
	require.NoError(t, err)

	assert.True(t, u.Equal(list))
	assert.True(t, u.Equal([]User{list[1], list[0]}))

	assert.False(t, u.Equal([]User{list[0], {Name: "watch", Role: Viewer, Hash: h1}}), "hash differs")
	assert.False(t, u.Equal([]User{list[0], {Name: "watch", Role: Editor, Hash: h2}}), "role differs")
	assert.False(t, u.Equal([]User{list[0], {Name: "watch2", Role: Viewer, Hash: h2}}), "name differs")
	assert.False(t, u.Equal(append([]User{{Name: "more", Role: Viewer, Hash: h2}}, list...)), "user added")
	assert.False(t, u.Equal(list[:1]), "user removed")
	assert.False(t, u.Equal([]User{list[0], list[0]}), "a duplicate is not the other user")
	assert.False(t, u.Equal(nil))
	salted := NewHash("password two 2", []byte("another salt 123"), 1000)
	assert.False(t, u.Equal([]User{list[0], {Name: "watch", Role: Viewer, Hash: salted}}), "same password, other salt")
}

func TestErrorsAreDistinct(t *testing.T) {
	assert.False(t, errors.Is(ErrBusy, ErrBadCredentials))
}
