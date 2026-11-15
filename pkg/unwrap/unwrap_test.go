package unwrap

import (
	"math/rand"
	"testing"

	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
)

func TestUnwrap32Wrap(t *testing.T) {
	u := New(32)
	in := []uint64{0xFFFFFF00, 0xFFFFFFF0, 0x10, 0x100}
	var prev int64
	for i, v := range in {
		got := u.Unwrap(v)
		if i == 0 {
			eq(t, int64(0xFFFFFF00), got)
		} else {
			if got <= prev {
				t.Fatalf("not increasing: %d after %d", got, prev)
			}
		}
		prev = got
	}
	eq(t, int64(1)<<32+0x100, prev)
}

func TestUnwrapBackStep(t *testing.T) {
	u := New(32)
	eq(t, int64(1000), u.Unwrap(1000))
	eq(t, int64(960), u.Unwrap(960))
	eq(t, int64(1040), u.Unwrap(1040))
}

func TestUnwrapHalfRange(t *testing.T) {
	u := New(32)
	eq(t, int64(0), u.Unwrap(0))
	eq(t, -(int64(1) << 31), u.Unwrap(1<<31))
}

func TestUnwrap33AgreesWithTimeDecoder(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	u := New(33)
	var d mpegts.TimeDecoder
	cur := int64(r.Int63n(1 << 33))
	first := u.Unwrap(uint64(cur))
	eq(t, int64(0), d.Decode(cur))
	for i := 0; i < 10000; i++ {
		cur = ((cur+r.Int63n(1<<20)-1<<18)%(1<<33) + 1<<33) % (1 << 33)
		eq(t, d.Decode(cur), u.Unwrap(uint64(cur))-first)
	}
}

func TestUnwrapReset(t *testing.T) {
	u := New(32)
	u.Unwrap(0xFFFFFFF0)
	u.Unwrap(0x10)
	u.Reset()
	eq(t, int64(5), u.Unwrap(5))
}

func TestNewPanics(t *testing.T) {
	if !panics(func() { New(0) }) {
		t.Fatal("New(0) must panic")
	}
	if !panics(func() { New(64) }) {
		t.Fatal("New(64) must panic")
	}
	if panics(func() { New(63).Unwrap(1 << 62) }) {
		t.Fatal("New(63) must not panic")
	}
}

func eq[T comparable](t *testing.T, want, got T) {
	t.Helper()
	if want != got {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func panics(f func()) (p bool) {
	defer func() { p = recover() != nil }()
	f()
	return false
}
