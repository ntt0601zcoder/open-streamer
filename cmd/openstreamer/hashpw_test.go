package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/auth"
)

// hashRun runs hash-password through run with stdin and the arguments.
func hashRun(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(context.Background(), append([]string{"hash-password"}, args...), strings.NewReader(stdin), &out, &errOut, nil)
	return code, out.String(), errOut.String()
}

func TestHashPassword(t *testing.T) {
	// Each hash costs 600000 iterations, so only the two line ends are run
	// to a hash; the rest of the reading is TestReadPassword's.
	for _, in := range []string{"correct horse battery\n", "correct horse battery\r\n"} {
		code, out, errOut := hashRun(t, in)
		require.Equal(t, 0, code, errOut)
		assert.Empty(t, errOut)
		require.True(t, strings.HasSuffix(out, "\n"))
		require.Equal(t, 1, strings.Count(out, "\n"), "one line")
		h, err := auth.ParseHash(strings.TrimSuffix(out, "\n"), auth.MinIterations)
		require.NoError(t, err)
		assert.Equal(t, auth.Iterations, h.Iterations)
		assert.True(t, h.Verify("correct horse battery"), "the line end is not part of the password")
	}

	for name, in := range map[string]string{
		"short":    "short\n",
		"empty":    "",
		"11 bytes": "elevenbytes\n",
		"too long": strings.Repeat("x", maxPasswordBytes+1) + "\n",
	} {
		code, out, errOut := hashRun(t, in)
		assert.Equal(t, 2, code, name)
		assert.Empty(t, out, name)
		assert.NotEmpty(t, errOut, name)
		assert.NotContains(t, errOut, "elevenbytes", name)
		assert.NotContains(t, errOut, "xxxx", name)
	}

	// The password never comes from the command line, and an argument is
	// not echoed back: it may be one.
	code, out, errOut := hashRun(t, "correct horse battery\n", "hunter2-secret")
	assert.Equal(t, 2, code)
	assert.Empty(t, out)
	assert.NotContains(t, errOut, "hunter2-secret")
	assert.Contains(t, errOut, "stdin")
}

func TestReadPassword(t *testing.T) {
	longest := strings.Repeat("x", maxPasswordBytes)
	for in, want := range map[string]string{
		"correct horse battery":     "correct horse battery",
		"correct horse battery\n":   "correct horse battery",
		"correct horse battery\r\n": "correct horse battery",
		"correct horse battery\n\n": "correct horse battery\n", // one line end only
		"correct horse battery  \n": "correct horse battery  ",
		"two lines\nof a pipe\n":    "two lines\nof a pipe",
		longest + "\r\n":            longest,
		"":                          "",
	} {
		got, err := readPassword(strings.NewReader(in), io.Discard)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	_, err := readPassword(strings.NewReader(longest+"x"), io.Discard)
	require.ErrorIs(t, err, errTooLong)
}
