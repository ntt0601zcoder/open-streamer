package asset

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
)

// Kind is what an asset is.
type Kind string

// Asset kinds.
const (
	Image Kind = "image"
	Font  Kind = "font"
)

// Limits of the directory (R-E5).
const (
	MaxFiles      = 256
	MaxTotalBytes = 512 << 20
)

// Errors of the store.
var (
	ErrName     = errors.New("asset: not an asset name")
	ErrNotFound = errors.New("asset: not found")
	ErrContent  = errors.New("asset: the content is not what the name says") // wrapped with the reason
	ErrTooLarge = errors.New("asset: too large")
	ErrFull     = errors.New("asset: the directory is full")
	ErrOff      = errors.New("asset: no assets directory")
	ErrWrite    = errors.New("asset: write")                      // wrapped with the cause
	ErrProbe    = errors.New("asset: the font probe did not run") // wrapped with the cause; says nothing about the font
	ErrBody     = errors.New("asset: read the body")              // wrapped with the cause, e.g. an *http.MaxBytesError
)

// ErrPrecondition is a failed If-Match or If-None-Match condition.
var ErrPrecondition = errors.New("asset: precondition failed")

// contentError is ErrContent with the reason, which is safe to show a
// client: it names no path.
type contentError struct{ reason string }

func (e *contentError) Error() string { return ErrContent.Error() + ": " + e.reason }
func (e *contentError) Unwrap() error { return ErrContent }

// Reason returns the client-safe reason inside an ErrContent error, without
// any prefix, and "" for any other error.
func Reason(err error) string {
	var ce *contentError
	if errors.As(err, &ce) {
		return ce.reason
	}
	return ""
}

// Info describes one asset.
type Info struct {
	Name     string
	Kind     Kind
	Type     string // the media type, by content: image/png, image/jpeg, font/ttf, font/otf
	Size     int64
	Modified time.Time
	ETag     string // `"` + the first 16 hex digits of the SHA-256 of the bytes + `"`
}

// Options configures a Store.
type Options struct {
	// ProbeFont, when set, is the last check of an uploaded font: it gets the path of the temp file. refused
	// true means the probe judged the font bad (reason is for the client); a non-nil err means it could not judge.
	ProbeFont func(ctx context.Context, path string) (refused bool, reason string, err error)
}

// KindOf returns the kind an asset name stands for; ok is false for any other name.
func KindOf(name string) (kind Kind, ok bool) {
	switch {
	case picture.ImageName(name):
		return Image, true
	case picture.FontName(name):
		return Font, true
	}
	return "", false
}

// MaxBytes is the largest body of a kind: picture.MaxImageBytes or picture.MaxFontBytes.
func MaxBytes(k Kind) int64 {
	switch k {
	case Image:
		return picture.MaxImageBytes
	case Font:
		return picture.MaxFontBytes
	}
	return 0
}

// tempRE matches a temp file of a Put; its first group is the asset name.
var tempRE = regexp.MustCompile(`^\.(.+)\.[0-9a-f]{8}\.tmp$`)

// entry is a cached hash: valid while the file under the name is the same
// file (os.SameFile: on Unix the same device and inode, which a rename keeps)
// with the same size and modification time.
type entry struct {
	fi   os.FileInfo
	typ  string
	etag string
}

// Store is the asset directory. Its methods are safe for concurrent use.
type Store struct {
	dir string
	opt Options

	// mu serialises the commit of a Put (conditions, quota, rename) and a
	// Delete. It is never held while a body is read or a font probed.
	mu sync.Mutex

	// cmu guards cache and epoch; it is taken after mu, never before.
	cmu   sync.Mutex
	cache map[string]entry
	epoch uint64 // bumped by every Put and Delete commit

	// Seams for the tests.
	maxFiles int
	maxTotal int64
	hook     func(step string) error // a non-nil error stops a Put there, as a crash would
	hashes   atomic.Int64            // files hashed by reading them
	listHook func()                  // called by List after its ReadDir
}

// Open returns the store of dir and removes the temp files a crash left in it. dir "" gives a store
// whose every method returns ErrOff.
//
// There must be one live Store per directory: a second Open of a directory in use deletes the temp files of
// the first one's Puts in flight (their renames then fail with ErrWrite), and the two would not share the
// lock that keeps the quota.
func Open(dir string, opt Options) (*Store, error) {
	if dir == "" {
		return &Store{}, nil
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("asset: open: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("asset: open: %s is not a directory", dir)
	}
	s := &Store{
		dir:      dir,
		opt:      opt,
		cache:    make(map[string]entry),
		maxFiles: MaxFiles,
		maxTotal: MaxTotalBytes,
	}
	if err := s.sweep(); err != nil {
		return nil, err
	}
	return s, nil
}

// sweep removes the regular files named as a Put's temp file of an asset.
// It assumes no other Store has the directory open (see Open).
func (s *Store) sweep() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("asset: open: %w", err)
	}
	for _, e := range entries {
		m := tempRE.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		if _, ok := KindOf(m[1]); !ok {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("asset: open: remove a temp file: %w", err)
		}
	}
	return nil
}

func (s *Store) off() bool { return s == nil || s.dir == "" }

// List returns the assets by name: only regular files with an asset name,
// and none over its kind's MaxBytes (copied in by hand; the quota still
// counts it).
func (s *Store) List() ([]Info, error) {
	if s.off() {
		return nil, ErrOff
	}
	s.cmu.Lock()
	epoch := s.epoch
	s.cmu.Unlock()
	entries, err := os.ReadDir(s.dir) // sorted by name
	if err != nil {
		return nil, fmt.Errorf("asset: list: %w", err)
	}
	if s.listHook != nil {
		s.listHook()
	}
	out := make([]Info, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		name := e.Name()
		if _, ok := KindOf(name); !ok || !e.Type().IsRegular() {
			continue
		}
		inf, err := s.Stat(name)
		if err != nil {
			continue // gone, not regular or unreadable since ReadDir, or over its limit: not listed
		}
		seen[name] = true
		out = append(out, inf)
	}
	// Prune the entries of names that are gone, unless a Put or a Delete
	// committed since the ReadDir: its entry may be one the listing missed.
	s.cmu.Lock()
	if s.epoch == epoch {
		for name := range s.cache {
			if !seen[name] {
				delete(s.cache, name)
			}
		}
	}
	s.cmu.Unlock()
	return out, nil
}

// Stat returns the Info of the asset name.
func (s *Store) Stat(name string) (Info, error) {
	f, inf, err := s.Open(name)
	if err != nil {
		return Info{}, err
	}
	_ = f.Close() // read only
	return inf, nil
}

// Read returns the bytes of the asset name with its Info. A file larger than
// its kind's MaxBytes (copied in by hand) is ErrTooLarge.
func (s *Store) Read(name string) ([]byte, Info, error) {
	kind, f, fi, err := s.openAsset(name)
	if err != nil {
		return nil, Info{}, err
	}
	defer func() { _ = f.Close() }() // read only
	limit := MaxBytes(kind)
	if fi.Size() > limit {
		return nil, Info{}, fmt.Errorf("%w: %q is %d bytes, over %d", ErrTooLarge, name, fi.Size(), limit)
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, Info{}, fmt.Errorf("asset: read %q: %w", name, err)
	}
	if int64(len(b)) > limit {
		return nil, Info{}, fmt.Errorf("%w: %q is over %d bytes", ErrTooLarge, name, limit)
	}
	inf := Info{Name: name, Kind: kind, Size: int64(len(b)), Modified: fi.ModTime()}
	e, epoch, ok := s.lookup(name, fi)
	if ok && e.fi.Size() == inf.Size {
		inf.Type, inf.ETag = e.typ, e.etag
		return b, inf, nil
	}
	s.hashes.Add(1)
	inf.Type, inf.ETag = sniff(b), etag(b)
	if int64(len(b)) == fi.Size() { // else the file changed while it was read
		s.remember(name, epoch, entry{fi: fi, typ: inf.Type, etag: inf.ETag})
	}
	return b, inf, nil
}

// Open returns the asset as an open regular file, for streaming, with its Info (the cached ETag). A later
// replacement renames a new file over the name: this descriptor keeps the old bytes.
func (s *Store) Open(name string) (*os.File, Info, error) {
	kind, f, fi, err := s.openAsset(name)
	if err != nil {
		return nil, Info{}, err
	}
	inf, err := s.fileInfo(name, kind, f, fi)
	if err != nil {
		_ = f.Close() // read only
		return nil, Info{}, err
	}
	return f, inf, nil
}

// openAsset opens the asset name if it is a regular file. A missing name, a
// symbolic link, a directory or a FIFO is ErrNotFound.
func (s *Store) openAsset(name string) (Kind, *os.File, os.FileInfo, error) {
	if s.off() {
		return "", nil, nil, ErrOff
	}
	kind, ok := KindOf(name)
	if !ok {
		return "", nil, nil, fmt.Errorf("%w: %q", ErrName, name)
	}
	path := filepath.Join(s.dir, name)
	f, err := picture.OpenRegular(path)
	if err != nil {
		if notAsset(path, err) {
			return "", nil, nil, fmt.Errorf("%w: %q", ErrNotFound, name)
		}
		return "", nil, nil, fmt.Errorf("asset: open %q: %w", name, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close() // read only
		return "", nil, nil, fmt.Errorf("asset: open %q: %w", name, err)
	}
	return kind, f, fi, nil
}

// notAsset reports whether err, from opening path, means there is no regular
// file there. OpenRegular has refused the entry or the open failed; the Lstat
// only names the reason and opens nothing.
func notAsset(path string, err error) bool {
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	fi, lerr := os.Lstat(path)
	if lerr != nil {
		return errors.Is(lerr, fs.ErrNotExist)
	}
	return !fi.Mode().IsRegular()
}

// fileInfo returns the Info of the open asset f: the type and the ETag from
// the cache when it holds fi's file, else from reading f, which is then
// rewound. A file larger than its kind's MaxBytes (copied in by hand) is
// ErrTooLarge, before anything is read: it is never hashed, and so never
// while mu is held.
func (s *Store) fileInfo(name string, kind Kind, f *os.File, fi os.FileInfo) (Info, error) {
	if limit := MaxBytes(kind); fi.Size() > limit {
		return Info{}, fmt.Errorf("%w: %q is %d bytes, over %d", ErrTooLarge, name, fi.Size(), limit)
	}
	inf := Info{Name: name, Kind: kind, Size: fi.Size(), Modified: fi.ModTime()}
	e, epoch, ok := s.lookup(name, fi)
	if ok {
		inf.Type, inf.ETag = e.typ, e.etag
		return inf, nil
	}
	s.hashes.Add(1)
	var head [8]byte
	n, err := io.ReadFull(f, head[:])
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return Info{}, fmt.Errorf("asset: read %q: %w", name, err)
	}
	h := sha256.New()
	h.Write(head[:n])
	if _, err := io.Copy(h, io.LimitReader(f, fi.Size()-int64(n))); err != nil {
		return Info{}, fmt.Errorf("asset: read %q: %w", name, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Info{}, fmt.Errorf("asset: read %q: %w", name, err)
	}
	inf.Type, inf.ETag = sniff(head[:n]), etagOfHash(h)
	s.remember(name, epoch, entry{fi: fi, typ: inf.Type, etag: inf.ETag})
	return inf, nil
}

// lookup returns the cache entry of name if it is fi's file, and the epoch to
// pass to remember.
func (s *Store) lookup(name string, fi os.FileInfo) (entry, uint64, bool) {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	e, ok := s.cache[name]
	return e, s.epoch, ok && os.SameFile(e.fi, fi) && e.fi.Size() == fi.Size() && e.fi.ModTime().Equal(fi.ModTime())
}

// remember caches e for name unless a Put or a Delete has committed since
// the epoch was read: what a reader hashed may then be the bytes it replaced.
func (s *Store) remember(name string, epoch uint64, e entry) {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	if s.epoch == epoch {
		s.cache[name] = e
	}
}

// commitEntry is a Put or Delete commit's change to the cache: e for name, or
// no entry when e is nil.
func (s *Store) commitEntry(name string, e *entry) {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	s.epoch++
	if e == nil {
		delete(s.cache, name)
		return
	}
	s.cache[name] = *e
}

// Put checks and stores r under name: created reports whether the name was new. match, when not "", is an
// If-Match value (a tag, or "*": the asset exists); noneMatch true is If-None-Match: * (the asset must not exist);
// a failed condition is ErrPrecondition.
//
// An ErrWrite from the directory fsync comes after the rename: the new bytes
// are then already visible under the name and their entry cached, only their
// durability across a power loss is unknown. Every other error leaves the name
// as it was. A read of r that fails is ErrBody wrapping the cause.
func (s *Store) Put(ctx context.Context, name string, r io.Reader, match string, noneMatch bool) (info Info, created bool, err error) {
	if s.off() {
		return Info{}, false, ErrOff
	}
	kind, ok := KindOf(name)
	if !ok {
		return Info{}, false, fmt.Errorf("%w: %q", ErrName, name)
	}
	limit := MaxBytes(kind)
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return Info{}, false, fmt.Errorf("%w: %w", ErrBody, err)
	}
	if int64(len(b)) > limit {
		return Info{}, false, fmt.Errorf("%w: a %s is at most %d bytes", ErrTooLarge, kind, limit)
	}
	typ, err := check(kind, name, b)
	if err != nil {
		return Info{}, false, &contentError{reason: err.Error()}
	}
	inf := Info{Name: name, Kind: kind, Type: typ, Size: int64(len(b)), ETag: etag(b)}

	tmp, fi, err := s.writeTemp(name, b)
	if err != nil {
		return Info{}, false, err
	}
	// keep is set once the temp file must not be removed: it was renamed, or
	// a test hook stopped the Put where a crash would have.
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tmp)
		}
	}()
	inf.Modified = fi.ModTime()

	if kind == Font && s.opt.ProbeFont != nil {
		refused, reason, err := s.opt.ProbeFont(ctx, tmp)
		switch {
		case err != nil:
			return Info{}, false, fmt.Errorf("%w: %w", ErrProbe, err)
		case refused:
			if reason == "" {
				reason = "the font probe refused it"
			}
			return Info{}, false, &contentError{reason: reason}
		}
	}
	if err := s.step("probed"); err != nil {
		keep = true
		return Info{}, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.dir, name)
	exists, oldSize, oldTag, err := s.current(name, path, kind, match != "" && match != "*")
	if err != nil {
		return Info{}, false, err
	}
	switch {
	case noneMatch && exists:
		return Info{}, false, fmt.Errorf("%w: %q exists", ErrPrecondition, name)
	case match != "" && !exists:
		return Info{}, false, fmt.Errorf("%w: %q does not exist", ErrPrecondition, name)
	case match != "" && match != "*" && (oldTag == "" || match != oldTag):
		return Info{}, false, fmt.Errorf("%w: %q has changed", ErrPrecondition, name)
	}
	count, total, err := s.usage()
	if err != nil {
		return Info{}, false, err
	}
	if !exists && count+1 > s.maxFiles {
		return Info{}, false, fmt.Errorf("%w: %d files, at most %d", ErrFull, count, s.maxFiles)
	}
	if total-oldSize+inf.Size > s.maxTotal {
		return Info{}, false, fmt.Errorf("%w: %d bytes with this one, at most %d", ErrFull, total-oldSize+inf.Size, s.maxTotal)
	}
	if err := os.Rename(tmp, path); err != nil {
		return Info{}, false, fmt.Errorf("%w: rename: %w", ErrWrite, err)
	}
	keep = true
	s.commitEntry(name, &entry{fi: fi, typ: inf.Type, etag: inf.ETag})
	if err := s.step("renamed"); err != nil {
		return Info{}, false, err
	}
	if err := syncDir(s.dir); err != nil {
		return Info{}, false, fmt.Errorf("%w: sync the directory: %w", ErrWrite, err)
	}
	return inf, !exists, nil
}

// step calls the test hook after a step of Put.
func (s *Store) step(name string) error {
	if s.hook == nil {
		return nil
	}
	return s.hook(name)
}

// writeTemp writes b to a new temp file of name, mode 0600, fsynced and
// closed, and returns its path and its FileInfo (the size and modification
// time the renamed file keeps). On a failure the file is removed; a test
// hook's error removes nothing, as a crash would not.
func (s *Store) writeTemp(name string, b []byte) (string, os.FileInfo, error) {
	var (
		tmp string
		fh  *os.File
		err error
	)
	for range 3 {
		tmp = filepath.Join(s.dir, "."+name+"."+randomHex()+".tmp")
		fh, err = os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if !errors.Is(err, fs.ErrExist) {
			break
		}
	}
	if err != nil {
		return "", nil, fmt.Errorf("%w: create a temp file: %w", ErrWrite, err)
	}
	open := true
	fail := func(err error) (string, os.FileInfo, error) {
		if open {
			_ = fh.Close()
		}
		_ = os.Remove(tmp)
		return "", nil, fmt.Errorf("%w: %w", ErrWrite, err)
	}
	crash := func(err error) (string, os.FileInfo, error) {
		if open {
			_ = fh.Close()
		}
		return "", nil, err
	}
	if err := s.step("created"); err != nil {
		return crash(err)
	}
	if _, err := fh.Write(b); err != nil {
		return fail(err)
	}
	if err := s.step("written"); err != nil {
		return crash(err)
	}
	if err := fh.Sync(); err != nil {
		return fail(err)
	}
	fi, err := fh.Stat()
	if err != nil {
		return fail(err)
	}
	open = false
	if err := fh.Close(); err != nil {
		return fail(err)
	}
	if err := s.step("synced"); err != nil {
		return crash(err)
	}
	return tmp, fi, nil
}

// current reports whether a regular file holds name, with its size and, when
// wantTag, its ETag. Called with mu held.
func (s *Store) current(name, path string, kind Kind, wantTag bool) (exists bool, size int64, tag string, err error) {
	f, err := picture.OpenRegular(path)
	if err != nil {
		if notAsset(path, err) {
			return false, 0, "", nil
		}
		return false, 0, "", fmt.Errorf("%w: open the current %q: %w", ErrWrite, name, err)
	}
	defer func() { _ = f.Close() }() // read only
	fi, err := f.Stat()
	if err != nil {
		return false, 0, "", fmt.Errorf("%w: open the current %q: %w", ErrWrite, name, err)
	}
	if !wantTag {
		return true, fi.Size(), "", nil
	}
	inf, err := s.fileInfo(name, kind, f, fi)
	if errors.Is(err, ErrTooLarge) {
		return true, fi.Size(), "", nil // no tag of it was ever served: no If-Match tag matches
	}
	if err != nil {
		return false, 0, "", fmt.Errorf("%w: %w", ErrWrite, err)
	}
	return true, fi.Size(), inf.ETag, nil
}

// usage returns the number of assets and their bytes in all, from Lstat:
// regular files with an asset name. Called with mu held.
func (s *Store) usage() (int, int64, error) {
	t, err := s.scan()
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %w", ErrWrite, err)
	}
	img, font := t[Image], t[Font]
	return int(img[0] + font[0]), img[1] + font[1], nil
}

// scan counts the assets and their bytes per kind, from Lstat: it opens no
// file.
func (s *Store) scan() (map[Kind][2]int64, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("asset: scan: %w", err)
	}
	t := map[Kind][2]int64{Image: {}, Font: {}}
	for _, e := range entries {
		kind, ok := KindOf(e.Name())
		if !ok || !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue // removed since ReadDir
		}
		c := t[kind]
		c[0]++
		c[1] += fi.Size()
		t[kind] = c
	}
	return t, nil
}

// Delete removes the asset name. match, when not "", is an If-Match value (a
// tag, or "*": the asset exists); a failed condition is ErrPrecondition.
func (s *Store) Delete(name, match string) error {
	if s.off() {
		return ErrOff
	}
	if _, ok := KindOf(name); !ok {
		return fmt.Errorf("%w: %q", ErrName, name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kind, f, fi, err := s.openAsset(name)
	if err != nil {
		return err
	}
	if match != "" && match != "*" {
		inf, err := s.fileInfo(name, kind, f, fi)
		if err != nil && !errors.Is(err, ErrTooLarge) { // over the limit: no tag of it was ever served
			_ = f.Close() // read only
			return err
		}
		if err != nil || inf.ETag != match {
			_ = f.Close() // read only
			return fmt.Errorf("%w: %q has changed", ErrPrecondition, name)
		}
	}
	_ = f.Close() // read only
	if err := os.Remove(filepath.Join(s.dir, name)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %q", ErrNotFound, name)
		}
		return fmt.Errorf("%w: remove: %w", ErrWrite, err)
	}
	s.commitEntry(name, nil)
	if err := syncDir(s.dir); err != nil {
		return fmt.Errorf("%w: sync the directory: %w", ErrWrite, err)
	}
	return nil
}

// Totals returns the count and the bytes per kind, for the metrics. It is
// nil without a directory or when the directory cannot be read.
func (s *Store) Totals() map[Kind][2]int64 {
	if s.off() {
		return nil
	}
	t, err := s.scan()
	if err != nil {
		return nil
	}
	return t
}

// check reports why b is not the content name says, and returns its media
// type when it is.
func check(kind Kind, name string, b []byte) (string, error) {
	switch kind {
	case Image:
		if _, err := picture.DecodeImage(b); err != nil {
			return "", err
		}
		typ, want := sniff(b), "image/jpeg"
		if strings.HasSuffix(name, ".png") {
			want = "image/png"
		}
		if typ != want {
			return "", fmt.Errorf("image: the file is %s, the name says %s", typ, want)
		}
		return typ, nil
	case Font:
		if err := picture.CheckFont(b); err != nil {
			return "", err
		}
		return sniff(b), nil
	}
	return "", fmt.Errorf("no kind %q", kind)
}

// sniff returns the media type of a file that starts with head, by its
// magic: the same signatures image.DecodeConfig and picture.CheckFont use.
func sniff(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(head, []byte("\xff\xd8")):
		return "image/jpeg"
	case bytes.HasPrefix(head, []byte("OTTO")):
		return "font/otf"
	case bytes.HasPrefix(head, []byte("\x00\x01\x00\x00")), bytes.HasPrefix(head, []byte("true")):
		return "font/ttf"
	}
	return "application/octet-stream"
}

func etag(b []byte) string {
	h := sha256.New()
	h.Write(b)
	return etagOfHash(h)
}

func etagOfHash(h hash.Hash) string {
	return `"` + hex.EncodeToString(h.Sum(nil)[:8]) + `"`
}

// randomHex returns 8 random hex digits.
func randomHex() string {
	var b [4]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails on a supported platform
	return hex.EncodeToString(b[:])
}
