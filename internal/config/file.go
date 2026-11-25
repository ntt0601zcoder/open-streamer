package config

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// ErrStale is File.Write's error when the file changed on disk since it
// was last loaded, written or adopted.
var ErrStale = errors.New("config: the file changed on disk")

// File is the config file on disk. Its methods may be called from several
// goroutines; Write calls are serialised.
type File struct {
	name string // the path as given, for messages
	path string // the real path, symlinks resolved, absolute

	mu   sync.Mutex
	sum  [sha256.Size]byte // of the text last loaded, written or adopted
	hook func(step string) error
}

// Open resolves path's symlinks once (filepath.EvalSymlinks: the rename
// must replace the real file, not the link), reads the file (at most
// MaxBytes), removes the temp files a crashed write of this file left, and
// remembers the text's hash. On Unix it refuses a file whose mode has any
// group or other bit ("config: <path> must be mode 0600").
func Open(path string) (*File, []byte, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, nil, fmt.Errorf("config: open: %w", err)
	}
	if real, err = filepath.Abs(real); err != nil {
		return nil, nil, fmt.Errorf("config: open: %w", err)
	}
	f := &File{name: path, path: real}
	text, err := f.read(true)
	if err != nil {
		return nil, nil, err
	}
	if err := f.removeTemps(); err != nil {
		return nil, nil, err
	}
	f.sum = sha256.Sum256(text)
	return f, text, nil
}

// Read returns the text on disk now, with the same mode check as Open,
// every time: a reload reads through it.
func (f *File) Read() ([]byte, error) { return f.read(true) }

// Stale reports whether the text on disk has another hash than the
// remembered one. It compares bytes only, with no mode check. An
// unreadable file is (true, err).
func (f *File) Stale() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.staleLocked()
}

// Write replaces the file's text atomically and remembers its hash: a temp
// file .<base>.<8 hex digits>.tmp in the file's directory, mode 0600,
// written, fsynced, renamed over the file, then the directory fsynced. It
// returns ErrStale, and writes nothing, when the file changed on disk. A
// failure after the rename is returned and the hash is not remembered: the
// new text is on disk but the caller did not apply it, so the file is
// stale until a reload adopts it.
func (f *File) Write(text []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	stale, err := f.staleLocked()
	if err != nil {
		return fmt.Errorf("config: write: %w: %w", ErrStale, err)
	}
	if stale {
		return ErrStale
	}
	if err := f.replace(text); err != nil {
		return err
	}
	f.sum = sha256.Sum256(text)
	return nil
}

// SetHook is for tests only; no production code calls it. f is called after
// each step of a write (created, written, synced, renamed, dirsynced), and
// an error it returns ends the write there, as a crash or a failed fsync
// would. nil removes it.
func (f *File) SetHook(hook func(step string) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hook = hook
}

// Adopt remembers text's hash: after a reload took the file as it is.
func (f *File) Adopt(text []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sum = sha256.Sum256(text)
}

func (f *File) staleLocked() (bool, error) {
	text, err := f.read(false)
	if err != nil {
		return true, err
	}
	return sha256.Sum256(text) != f.sum, nil
}

// read returns the file's text, refusing anything but a regular file of
// at most MaxBytes and, with mode, a file not private to its owner.
func (f *File) read(mode bool) ([]byte, error) {
	fh, err := os.Open(f.path)
	if err != nil {
		return nil, fmt.Errorf("config: read: %w", err)
	}
	defer func() { _ = fh.Close() }() // read only: a close error loses nothing
	fi, err := fh.Stat()
	if err != nil {
		return nil, fmt.Errorf("config: read: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("config: %s is not a regular file", f.name)
	}
	if mode {
		if err := checkMode(f.name, fi.Mode()); err != nil {
			return nil, err
		}
	}
	text, err := io.ReadAll(io.LimitReader(fh, MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("config: read: %w", err)
	}
	if len(text) > MaxBytes {
		return nil, fmt.Errorf("config: %s is larger than %d bytes", f.name, MaxBytes)
	}
	return text, nil
}

// step calls the test hook after a step of replace.
func (f *File) step(name string) error {
	if f.hook == nil {
		return nil
	}
	return f.hook(name)
}

// replace is the atomic write. A real error before the rename removes the
// temp file; a hook error returns at once and removes nothing, which is
// what a crash leaves.
func (f *File) replace(text []byte) (err error) {
	dir, base := filepath.Dir(f.path), filepath.Base(f.path)
	tmp := filepath.Join(dir, "."+base+"."+randomHex()+".tmp")
	fh, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("config: write: %w", err)
	}
	open := true
	defer func() {
		if open {
			_ = fh.Close()
		}
	}()
	fail := func(err error) error {
		if open {
			_ = fh.Close()
			open = false
		}
		_ = os.Remove(tmp)
		return fmt.Errorf("config: write: %w", err)
	}
	if err := f.step("created"); err != nil {
		return err
	}
	if _, err := fh.Write(text); err != nil {
		return fail(err)
	}
	if err := f.step("written"); err != nil {
		return err
	}
	if err := fh.Sync(); err != nil {
		return fail(err)
	}
	if err := f.step("synced"); err != nil {
		return err
	}
	open = false
	if err := fh.Close(); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmp, f.path); err != nil {
		return fail(err)
	}
	if err := f.step("renamed"); err != nil {
		return err
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("config: write: sync %s: %w", dir, err)
	}
	return f.step("dirsynced")
}

// removeTemps removes the temp files a crashed write of this file left:
// exactly .<base>.<8 hex digits>.tmp in its directory.
func (f *File) removeTemps() error {
	dir, base := filepath.Dir(f.path), filepath.Base(f.path)
	re := regexp.MustCompile(`^\.` + regexp.QuoteMeta(base) + `\.[0-9a-f]{8}\.tmp$`)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("config: open: %w", err)
	}
	for _, e := range entries {
		if !re.MatchString(e.Name()) || !e.Type().IsRegular() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("config: open: %w", err)
		}
	}
	return nil
}

// randomHex returns 8 random hex digits.
func randomHex() string {
	var b [4]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails on a supported platform
	return hex.EncodeToString(b[:])
}
