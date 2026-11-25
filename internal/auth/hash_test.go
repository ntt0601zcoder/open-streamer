package auth

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// No test of this package calls t.Parallel: the tests that set the hash
// hook count every PBKDF2 computation of the process. PBKDF2 at
// Iterations takes seconds under -race, so the tests use it only where
// the brief requires it (HashPassword and the dummy hash).

func TestHashFormat(t *testing.T) {
	const pw = "correct horse battery"
	s1, err := HashPassword(pw)
	require.NoError(t, err)
	re := regexp.MustCompile(`^pbkdf2-sha256\$600000\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$`)
	assert.Regexp(t, re, s1)

	s2, err := HashPassword(pw)
	require.NoError(t, err)
	assert.NotEqual(t, s1, s2, "two hashes of one password share a salt")

	h, err := ParseHash(s1, MinIterations)
	require.NoError(t, err)
	assert.Equal(t, s1, h.String())
	assert.Equal(t, Iterations, h.Iterations)
	assert.Len(t, h.Salt, SaltBytes)
	assert.Len(t, h.Key, KeyBytes)

	assert.True(t, h.Verify(pw))
	b := []byte(pw)
	b[3] ^= 1
	assert.False(t, h.Verify(string(b)))

	_, err = HashPassword(strings.Repeat("x", MinPasswordBytes-1))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "xxxxxx")
}

func TestHashKnownAnswer(t *testing.T) {
	h := NewHash("password", []byte("salt"), 1)
	assert.Equal(t, "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b", hex.EncodeToString(h.Key))
	assert.Equal(t, 1, h.Iterations)
	assert.Equal(t, []byte("salt"), h.Salt)
	assert.True(t, h.Verify("password"))
	assert.False(t, h.Verify("passwore"))
}

// TestHashVerifyEmptyKey: a Hash without a key matches nothing, not even
// a derivation that failed.
func TestHashVerifyEmptyKey(t *testing.T) {
	assert.False(t, Hash{}.Verify(""))
	assert.False(t, Hash{Iterations: 1, Salt: []byte("saltsalt")}.Verify("x"))
	h := NewHash("password", []byte("salt"), 1)
	h.Key = h.Key[:16]
	assert.False(t, h.Verify("password"), "a short key is not a prefix match")
}

func TestParseHashRefuses(t *testing.T) {
	b64 := base64.RawStdEncoding.EncodeToString
	salt16 := b64([]byte("0123456789abcdef"))
	key32 := b64([]byte("0123456789abcdef0123456789abcdef"))
	cases := map[string]string{
		"plaintext":      "hunter2-plaintext",
		"empty":          "",
		"bcrypt":         "$2b$12$R9h/cIPz0gi.URNNX3kh2OPST9/PgBkqquzi.Ss7KIUgO2t0jWMUW",
		"sha1":           "pbkdf2-sha1$600000$a$b",
		"three fields":   "pbkdf2-sha256$600000$" + salt16,
		"five fields":    "pbkdf2-sha256$600000$" + salt16 + "$" + key32 + "$" + key32,
		"iter abc":       "pbkdf2-sha256$abc$" + salt16 + "$" + key32,
		"iter low":       "pbkdf2-sha256$599999$" + salt16 + "$" + key32,
		"iter high":      "pbkdf2-sha256$10000001$" + salt16 + "$" + key32,
		"iter plus":      "pbkdf2-sha256$+600000$" + salt16 + "$" + key32,
		"iter zero lead": "pbkdf2-sha256$0600000$" + salt16 + "$" + key32,
		"iter negative":  "pbkdf2-sha256$-600000$" + salt16 + "$" + key32,
		"iter huge":      "pbkdf2-sha256$99999999999999999999999$" + salt16 + "$" + key32,
		"padded salt":    "pbkdf2-sha256$600000$" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")) + "$" + key32,
		"padded key":     "pbkdf2-sha256$600000$" + salt16 + "$" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
		"7-byte salt":    "pbkdf2-sha256$600000$" + b64([]byte("0123456")) + "$" + key32,
		"31-byte key":    "pbkdf2-sha256$600000$" + salt16 + "$" + b64([]byte("0123456789abcdef0123456789abcde")),
		"33-byte key":    "pbkdf2-sha256$600000$" + salt16 + "$" + b64([]byte("0123456789abcdef0123456789abcdefg")),
		"bad salt b64":   "pbkdf2-sha256$600000$!!!!!!!!!!!!!!!!!!!!!!$" + key32,
		"url b64 key":    "pbkdf2-sha256$600000$" + salt16 + "$" + strings.ReplaceAll(strings.ReplaceAll(key32, "+", "-"), "/", "_") + "-",
		"space":          " pbkdf2-sha256$600000$" + salt16 + "$" + key32,
		"upper prefix":   "PBKDF2-SHA256$600000$" + salt16 + "$" + key32,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseHash(in, MinIterations)
			require.Error(t, err)
			msg := err.Error()
			require.NotEmpty(t, msg)
			if in != "" {
				assert.NotContains(t, msg, in)
			}
			for i := 0; i+6 <= len(in); i++ {
				assert.NotContains(t, msg, in[i:i+6], "the error leaks a piece of the input")
			}
		})
	}

	// The same shape with valid parts parses, so the refusals above are
	// about the part each one breaks.
	good := "pbkdf2-sha256$600000$" + salt16 + "$" + key32
	_, err := ParseHash(good, MinIterations)
	require.NoError(t, err)
	_, err = ParseHash("pbkdf2-sha256$10000000$"+salt16+"$"+key32, MinIterations)
	require.NoError(t, err)
	_, err = ParseHash("pbkdf2-sha256$600000$"+b64([]byte("01234567"))+"$"+key32, MinIterations)
	require.NoError(t, err, "an 8-byte salt is enough")
	// minIterations is the caller's floor.
	_, err = ParseHash("pbkdf2-sha256$1000$"+salt16+"$"+key32, 1000)
	require.NoError(t, err)
	_, err = ParseHash("pbkdf2-sha256$999$"+salt16+"$"+key32, 1000)
	require.Error(t, err)
	// A floor under 1 still refuses 0.
	_, err = ParseHash("pbkdf2-sha256$0$"+salt16+"$"+key32, 0)
	require.Error(t, err)
}

func TestHashStringRoundTrip(t *testing.T) {
	h := NewHash("a long enough password", []byte("0123456789abcdef"), 1000)
	s := h.String()
	assert.True(t, strings.HasPrefix(s, fmt.Sprintf("%s$1000$", Prefix)))
	got, err := ParseHash(s, 1000)
	require.NoError(t, err)
	assert.Equal(t, h, got)
	assert.True(t, got.Verify("a long enough password"))
}
