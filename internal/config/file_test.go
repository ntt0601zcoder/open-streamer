package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeConfig writes text to dir/config.yaml at mode 0600 and returns the
// path.
func writeConfig(t *testing.T, dir, text string) string {
	t.Helper()
	p := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(p, []byte(text), 0o600))
	require.NoError(t, os.Chmod(p, 0o600))
	return p
}

// tempFiles lists the temp files a write of config.yaml leaves in dir.
func tempFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, ".config.yaml.*.tmp"))
	require.NoError(t, err)
	return m
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	return string(b)
}

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("file modes are Unix only")
	}
}

func TestOpenMode(t *testing.T) {
	skipOnWindows(t)
	for _, m := range []os.FileMode{0o644, 0o640, 0o604, 0o610} {
		t.Run(m.String(), func(t *testing.T) {
			p := writeConfig(t, t.TempDir(), head())
			require.NoError(t, os.Chmod(p, m))
			_, _, err := Open(p)
			require.EqualError(t, err, "config: "+p+" must be mode 0600")
		})
	}
	for _, m := range []os.FileMode{0o600, 0o400} {
		t.Run(m.String(), func(t *testing.T) {
			p := writeConfig(t, t.TempDir(), head())
			require.NoError(t, os.Chmod(p, m))
			f, text, err := Open(p)
			require.NoError(t, err)
			assert.Equal(t, head(), string(text))
			got, err := f.Read()
			require.NoError(t, err)
			assert.Equal(t, head(), string(got))
		})
	}
	t.Run("chmod after Open", func(t *testing.T) {
		p := writeConfig(t, t.TempDir(), head())
		f, _, err := Open(p)
		require.NoError(t, err)
		require.NoError(t, os.Chmod(p, 0o644))
		_, err = f.Read()
		require.EqualError(t, err, "config: "+p+" must be mode 0600")
		require.ErrorIs(t, err, ErrUnreadable)
		require.NoError(t, os.Remove(p))
		_, err = f.Read()
		require.ErrorIs(t, err, ErrUnreadable)
		require.ErrorIs(t, err, os.ErrNotExist)
		_, err = f.Stale()
		require.ErrorIs(t, err, ErrUnreadable)
		require.NoError(t, os.WriteFile(p, []byte(head()), 0o644))
		stale, err := f.Stale()
		require.NoError(t, err, "Stale checks the bytes only")
		assert.False(t, stale)
	})
}

func TestOpenErrors(t *testing.T) {
	dir := t.TempDir()
	_, _, err := Open(filepath.Join(dir, "missing.yaml"))
	require.ErrorContains(t, err, "config: open: ")

	_, _, err = Open(dir)
	require.ErrorContains(t, err, "is not a regular file")

	big := writeConfig(t, t.TempDir(), head()+"#"+strings.Repeat("x", MaxBytes)+"\n")
	_, _, err = Open(big)
	require.ErrorContains(t, err, "is larger than 4194304 bytes")
}

func TestOpenSymlink(t *testing.T) {
	skipOnWindows(t)
	realDir := t.TempDir()
	real := writeConfig(t, realDir, head())
	linkDir := t.TempDir()
	link := filepath.Join(linkDir, "config.yaml")
	require.NoError(t, os.Symlink(real, link))

	f, text, err := Open(link)
	require.NoError(t, err)
	assert.Equal(t, head(), string(text))

	var tempDir string
	f.hook = func(step string) error {
		if step == "created" {
			if m := tempFiles(t, realDir); len(m) == 1 {
				tempDir = filepath.Dir(m[0])
			}
		}
		return nil
	}
	next := head() + "# edited\n"
	require.NoError(t, f.Write([]byte(next)))
	assert.Equal(t, realDir, tempDir, "the temp file is made next to the real file")

	fi, err := os.Lstat(link)
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&os.ModeSymlink, "the link is still a link")
	assert.Equal(t, next, readFile(t, real))
	assert.Equal(t, next, readFile(t, link))
	assert.Empty(t, tempFiles(t, realDir))
	assert.Empty(t, tempFiles(t, linkDir))
}

func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, head())
	f, _, err := Open(p)
	require.NoError(t, err)

	next := head() + "streams:\n" + streamYAML("cam")
	require.NoError(t, f.Write([]byte(next)))
	assert.Equal(t, next, readFile(t, p))
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(p)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	}
	assert.Empty(t, tempFiles(t, dir))
	stale, err := f.Stale()
	require.NoError(t, err)
	assert.False(t, stale)
	got, err := f.Read()
	require.NoError(t, err)
	assert.Equal(t, next, string(got))

	t.Run("read-only directory", func(t *testing.T) {
		skipOnWindows(t)
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		require.NoError(t, os.Chmod(dir, 0o500))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		err := f.Write([]byte(head() + "# lost\n"))
		require.ErrorContains(t, err, "config: write: ")
		assert.Equal(t, next, readFile(t, p))
		assert.Empty(t, tempFiles(t, dir))
	})
}

func TestWriteRealErrorsRemoveTheTempFile(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, head())
	f, _, err := Open(p)
	require.NoError(t, err)
	// After the stale check, replace the target by a non-empty directory:
	// the rename fails.
	f.hook = func(step string) error {
		if step == "synced" {
			require.NoError(t, os.Remove(p))
			require.NoError(t, os.MkdirAll(filepath.Join(p, "x"), 0o700))
		}
		return nil
	}
	err = f.Write([]byte(head() + "# next\n"))
	require.ErrorContains(t, err, "config: write: ")
	assert.Empty(t, tempFiles(t, dir))
}

func TestWriteCrashPoints(t *testing.T) {
	steps := []string{"created", "written", "synced", "renamed", "dirsynced"}
	for i, step := range steps {
		t.Run(step, func(t *testing.T) {
			dir := t.TempDir()
			old := head() + "# old\n"
			p := writeConfig(t, dir, old)
			f, _, err := Open(p)
			require.NoError(t, err)

			crash := errors.New("crash at " + step)
			f.hook = func(s string) error {
				if s == step {
					return crash
				}
				return nil
			}
			next := head() + "streams:\n" + streamYAML("cam")
			err = f.Write([]byte(next))
			require.ErrorIs(t, err, crash)

			onDisk := readFile(t, p)
			afterRename := i >= 3
			if afterRename {
				assert.Equal(t, next, onDisk)
			} else {
				assert.Equal(t, old, onDisk)
			}
			_, err = Parse([]byte(onDisk), testOpt)
			require.NoError(t, err)
			if i < 3 {
				assert.Len(t, tempFiles(t, dir), 1, "a crash leaves the temp file")
			}

			if afterRename {
				stale, err := f.Stale()
				require.NoError(t, err)
				assert.True(t, stale, "the new text is on disk but was not adopted")
				f.hook = nil
				require.ErrorIs(t, f.Write([]byte(next)), ErrStale)
				f.Adopt([]byte(onDisk))
				stale, err = f.Stale()
				require.NoError(t, err)
				assert.False(t, stale)
			}

			// A fresh Open removes the temp file and loads what survived.
			require.NoError(t, os.WriteFile(filepath.Join(dir, ".config.yaml.zzzzzzzz.tmp"), nil, 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, ".other.yaml.0123abcd.tmp"), nil, 0o600))
			g, text, err := Open(p)
			require.NoError(t, err)
			assert.Equal(t, onDisk, string(text))
			assert.Equal(t, []string{filepath.Join(dir, ".config.yaml.zzzzzzzz.tmp")}, tempFiles(t, dir),
				"only this file's temp files, by their exact shape, are removed")
			assert.FileExists(t, filepath.Join(dir, ".other.yaml.0123abcd.tmp"))
			require.NoError(t, g.Write([]byte(old)))
			assert.Equal(t, old, readFile(t, p))
		})
	}
}

func TestStale(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, head())
	f, _, err := Open(p)
	require.NoError(t, err)
	stale, err := f.Stale()
	require.NoError(t, err)
	assert.False(t, stale)

	hand := head() + "# by hand\n"
	require.NoError(t, os.WriteFile(p, []byte(hand), 0o600))
	stale, err = f.Stale()
	require.NoError(t, err)
	assert.True(t, stale)
	require.ErrorIs(t, f.Write([]byte(head()+"# api\n")), ErrStale)
	assert.Equal(t, hand, readFile(t, p), "the hand edit is intact")

	f.Adopt([]byte(hand))
	stale, err = f.Stale()
	require.NoError(t, err)
	assert.False(t, stale)
	api := head() + "# api\n"
	require.NoError(t, f.Write([]byte(api)))
	assert.Equal(t, api, readFile(t, p))

	// A hand edit that restores the same bytes is not stale.
	require.NoError(t, os.WriteFile(p, []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(p, []byte(api), 0o600))
	stale, err = f.Stale()
	require.NoError(t, err)
	assert.False(t, stale)

	// An unreadable file is stale, with the error.
	require.NoError(t, os.Remove(p))
	stale, err = f.Stale()
	require.Error(t, err)
	assert.True(t, stale)
	err = f.Write([]byte(api))
	require.ErrorIs(t, err, ErrStale)
	assert.NoFileExists(t, p)
}

func TestConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, head())
	f, _, err := Open(p)
	require.NoError(t, err)

	texts := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 30 {
				text := fmt.Sprintf("%s# writer %d, write %d\n", head(), g, i)
				mu.Lock()
				texts[text] = true
				mu.Unlock()
				assert.NoError(t, f.Write([]byte(text)))
			}
		}()
	}
	wg.Wait()
	final := readFile(t, p)
	assert.True(t, texts[final], final)
	assert.Empty(t, tempFiles(t, dir))
	stale, err := f.Stale()
	require.NoError(t, err)
	assert.False(t, stale)
}
