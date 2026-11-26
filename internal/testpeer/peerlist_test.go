package testpeer

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// peerWord is the pattern scripts/test-ci-peers.sh picks its default
// packages with: a test file that matches it puts its package in the peer
// run. TestPeerListScript holds the two to each other.
const peerWord = `\b(ffmpeg|ffprobe|cvlc|gst-launch-1\.0|testpeer)\b`

// The script's pattern is the one this file checks with.
func TestPeerListScript(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "scripts", "test-ci-peers.sh"))
	require.NoError(t, err)
	assert.Contains(t, string(b), "grep -rlE '"+peerWord+"' --include='*_test.go' cmd internal pkg")
}

// The peer run is go test without tags, so every package it picks must
// build without them: a package whose Go files all need the libav tag (the
// native package) has no test file that names a peer program or this
// package, not even in a comment. Otherwise make test-ci-peers fails at
// "build constraints exclude all Go files" before that package runs a test.
func TestPeerListBuildsUntagged(t *testing.T) {
	word := regexp.MustCompile(peerWord)
	root := filepath.Join("..", "..")
	named := map[string]string{} // package directory: where a test file of it matches
	untagged := map[string]bool{}
	for _, dir := range []string{"cmd", "internal", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			b, err := os.ReadFile(path) //nolint:gosec // a file of this repository
			if err != nil {
				return err
			}
			pkg := filepath.Dir(path)
			if !libavOnly(string(b)) {
				untagged[pkg] = true
			}
			if !strings.HasSuffix(path, "_test.go") || named[pkg] != "" {
				return nil
			}
			sc := bufio.NewScanner(strings.NewReader(string(b)))
			for n := 1; sc.Scan(); n++ {
				if m := word.FindString(sc.Text()); m != "" {
					named[pkg] = fmt.Sprintf("%s:%d names %q", path, n, m)
					break
				}
			}
			return sc.Err()
		})
		require.NoError(t, err)
	}
	require.NotEmpty(t, named, "the packages of the peer run")
	for pkg, where := range named {
		assert.True(t, untagged[pkg], "%s: scripts/test-ci-peers.sh picks %s for the untagged peer run, and every Go file of it needs the libav tag",
			where, pkg)
	}
}

// libavOnly reports whether a Go file's build constraint, which stands
// before its package clause, needs the libav tag.
func libavOnly(src string) bool {
	for line := range strings.SplitSeq(src, "\n") {
		if strings.HasPrefix(line, "package ") {
			return false
		}
		if strings.HasPrefix(line, "//go:build ") {
			return strings.Contains(line, "libav") && !strings.Contains(line, "!libav")
		}
	}
	return false
}
