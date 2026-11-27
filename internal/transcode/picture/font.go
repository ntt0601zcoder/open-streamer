package picture

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
)

// MaxFontBytes is the largest font file a watermark may carry.
const MaxFontBytes = 24 << 20

// Font table directory limits (R-E8).
const (
	fontHeaderLen = 12
	fontRecordLen = 16
	maxFontTables = 256
)

var (
	imageNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}\.(png|jpg|jpeg)$`)
	fontNameRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}\.(ttf|otf)$`)
)

// ImageName reports whether name is an image asset's name (R-E5). A name
// that matches cannot leave the directory it is joined to.
func ImageName(name string) bool { return imageNameRE.MatchString(name) }

// FontName reports whether name is a font asset's name (R-E5). A name that
// matches cannot leave the directory it is joined to.
func FontName(name string) bool { return fontNameRE.MatchString(name) }

// CheckFont reports why b is not a font the child may be given (R-E8). It
// reads the table directory only: an sfnt header (00 01 00 00, OTTO or
// true), 1 to 256 table records, each inside b, and the tables cmap, head,
// hhea, hmtx and maxp with outlines (glyf with loca, CFF or CFF2). Font
// collections and WOFF are refused by name. The real parser is the child's
// FreeType.
func CheckFont(b []byte) error {
	switch {
	case len(b) == 0:
		return errors.New("font: empty")
	case len(b) > MaxFontBytes:
		return fmt.Errorf("font: %d bytes, over %d", len(b), MaxFontBytes)
	case len(b) < fontHeaderLen:
		return fmt.Errorf("font: %d bytes, shorter than the %d-byte header", len(b), fontHeaderLen)
	}
	switch magic := string(b[:4]); magic {
	case "\x00\x01\x00\x00", "OTTO", "true":
	case "ttcf":
		return errors.New("font: a font collection is not accepted: extract one face")
	case "wOFF", "wOF2":
		return errors.New("font: WOFF is not accepted")
	default:
		return fmt.Errorf("font: not a TrueType or OpenType file (magic %q)", magic)
	}
	n := int(binary.BigEndian.Uint16(b[4:]))
	if n < 1 || n > maxFontTables {
		return fmt.Errorf("font: %d tables, want 1 to %d", n, maxFontTables)
	}
	if need := fontHeaderLen + fontRecordLen*n; need > len(b) {
		return fmt.Errorf("font: table directory cut short: %d records need %d bytes, the file has %d", n, need, len(b))
	}
	has := make(map[string]bool, n)
	for i := range n {
		r := b[fontHeaderLen+fontRecordLen*i:]
		tag := string(r[:4])
		off, size := uint64(binary.BigEndian.Uint32(r[8:])), uint64(binary.BigEndian.Uint32(r[12:]))
		end := off + size
		if end > math.MaxUint32 {
			return fmt.Errorf("font: table %q: offset %d plus length %d overflows", tag, off, size)
		}
		if end > uint64(len(b)) {
			return fmt.Errorf("font: table %q ends at %d, past the end of the file (%d bytes)", tag, end, len(b))
		}
		has[tag] = true
	}
	for _, tag := range []string{"cmap", "head", "hhea", "hmtx", "maxp"} {
		if !has[tag] {
			return fmt.Errorf("font: no %q table", tag)
		}
	}
	outlines := has["glyf"] && has["loca"] || has["CFF "] || has["CFF2"]
	if !outlines {
		return errors.New("font: no outlines: neither glyf with loca nor CFF or CFF2")
	}
	return nil
}

// LoadFont reads the font asset name from dir: the name must match the rule,
// the entry must be a regular file (OpenRegular: a symlink, a directory or a
// FIFO is refused and cannot hang), it is at most MaxFontBytes, and it passes
// CheckFont. The returned slice is never written to again by anyone (see
// Watermark.Font); its capacity is its length, so an append copies it.
func LoadFont(dir, name string) ([]byte, error) {
	if !FontName(name) {
		return nil, fmt.Errorf("picture: font name %q is not allowed", name)
	}
	b, err := readAsset(dir, name, MaxFontBytes)
	if err != nil {
		return nil, err
	}
	if err := CheckFont(b); err != nil {
		return nil, fmt.Errorf("picture: font %q: %w", name, err)
	}
	return b, nil
}

// readAsset reads the regular file name in dir, at most limit bytes. The
// name has been checked against an asset rule. Its errors name the asset,
// never the directory (pathless): they reach HTTP clients.
func readAsset(dir, name string, limit int64) ([]byte, error) {
	f, err := OpenRegular(filepath.Join(dir, name))
	if err != nil {
		return nil, fmt.Errorf("picture: asset %q: %w", name, pathless(err))
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("picture: asset %q: %w", name, pathless(err))
	}
	if fi.Size() > limit {
		return nil, fmt.Errorf("picture: asset %q is %d bytes, over %d", name, fi.Size(), limit)
	}
	// Room for the size Stat gave plus one byte, so that a file that has not
	// grown is read without growing the buffer and its end is seen at once.
	b := make([]byte, 0, fi.Size()+1)
	r := io.LimitReader(f, limit+1)
	for {
		if len(b) == cap(b) {
			b = append(b, 0)[:len(b)]
		}
		n, err := r.Read(b[len(b):cap(b)])
		b = b[:len(b)+n]
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("picture: read asset %q: %w", name, pathless(err))
		}
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("picture: asset %q is over %d bytes", name, limit)
	}
	return b[:len(b):len(b)], nil
}

// notRegularError is OpenRegular's error for an entry that is not a
// regular file: what it is ("a symbolic link", "a directory", ...), and the
// error of the open that found it, if any.
type notRegularError struct {
	path, what string
	err        error
}

func (e *notRegularError) Error() string {
	if e.what == symlink {
		return fmt.Sprintf("picture: open: %s is %s", e.path, e.what)
	}
	return fmt.Sprintf("picture: open: %s is %s, not a regular file", e.path, e.what)
}

func (e *notRegularError) Unwrap() error { return e.err }

// symlink is notRegularError.what for a symbolic link.
const symlink = "a symbolic link"

// pathless is err, from OpenRegular, Stat or a read, without the file's
// path: what the entry is when it is not a regular file, else the
// system's reason (fs.PathError's Err). errors.Is(err, fs.ErrNotExist)
// still holds of what it returns.
func pathless(err error) error {
	if nr, ok := errors.AsType[*notRegularError](err); ok {
		if nr.what == symlink {
			return errors.New("is a symbolic link")
		}
		return fmt.Errorf("is %s, not a regular file", nr.what)
	}
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err //nolint:wrapcheck // the caller wraps it with the asset's name
	}
	return err
}

// checkRegular returns f if the file it has open is a regular file, and
// closes it otherwise. On Unix f was opened with O_NONBLOCK, which a read of
// a regular file ignores.
func checkRegular(f *os.File) (*os.File, error) {
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("picture: open: %w", err)
	}
	if m := fi.Mode(); !m.IsRegular() {
		_ = f.Close()
		what := "something else"
		switch {
		case m.IsDir():
			what = "a directory"
		case m&os.ModeNamedPipe != 0:
			what = "a FIFO"
		case m&os.ModeDevice != 0:
			what = "a device"
		case m&os.ModeSocket != 0:
			what = "a socket"
		}
		return nil, &notRegularError{path: f.Name(), what: what}
	}
	return f, nil
}
