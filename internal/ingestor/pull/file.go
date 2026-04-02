package pull

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/ntthuan060102github/open-streamer/internal/domain"
)

// FileReader reads a local media file and emits its bytes as MPEG-TS chunks.
// If the input URL has the query parameter "loop=true", the file is read
// repeatedly from the beginning after reaching EOF.
//
// Expected URL format: file:///absolute/path/to/source.ts
// Or plain path: /absolute/path/to/source.ts.
type FileReader struct {
	input domain.Input
	path  string
	loop  bool
	f     *os.File

	demux Demuxer
}

// NewFileReader constructs a FileReader for the given input.
func NewFileReader(input domain.Input) *FileReader {
	path, loop := parseFileURL(input.URL)
	return &FileReader{
		input: input,
		path:  path,
		loop:  loop,
	}
}

// Open opens the underlying file for reading.
func (r *FileReader) Open(_ context.Context) error {
	st, err := os.Stat(r.path)
	if err != nil {
		return fmt.Errorf("file reader: stat %q: %w", r.path, err)
	}
	if st.IsDir() {
		return fmt.Errorf("file reader: path %q is a directory", r.path)
	}
	if !shouldUseNativeMP4(r.path) {
		f, err := os.Open(r.path)
		if err != nil {
			return fmt.Errorf("file reader: open %q: %w", r.path, err)
		}
		r.f = f
		r.demux = newPassthroughDemuxer(r.f, 188*56)
		return nil
	}
	f, err := os.Open(r.path)
	if err != nil {
		return fmt.Errorf("file reader: open %q: %w", r.path, err)
	}
	r.f = f
	dmx, err := newMP4ToTSDemuxer(r.f, r.loop)
	if err != nil {
		_ = f.Close()
		r.f = nil
		return fmt.Errorf("file reader: mp4 demux init: %w", err)
	}
	r.demux = dmx
	return nil
}

func (r *FileReader) Read(ctx context.Context) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if r.demux == nil {
		return nil, fmt.Errorf("file reader: not opened")
	}
	chunk, err := r.demux.Read(ctx)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("file reader: read: %w", err)
	}
	return chunk, nil
}

// Close closes the open file, if any.
func (r *FileReader) Close() error {
	var closeErr error
	if r.demux != nil {
		_ = r.demux.Close()
		r.demux = nil
	}
	if r.f != nil {
		if err := r.f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			closeErr = err
		}
	}
	return closeErr
}

// parseFileURL extracts the filesystem path and loop flag from a URL.
// Accepts both "file:///path" and plain absolute paths.
func parseFileURL(rawURL string) (path string, loop bool) {
	if strings.HasPrefix(rawURL, "file://") {
		u, err := url.Parse(rawURL)
		if err == nil {
			loop = u.Query().Get("loop") == "true"
			return u.Path, loop
		}
	}
	return rawURL, false
}

func shouldUseNativeMP4(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".m4v", ".mov":
		return true
	default:
		return false
	}
}
