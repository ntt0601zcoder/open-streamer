package ingest

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// httpTimeout is the dial, TLS handshake and response header timeout of the
// default HTTP client.
const httpTimeout = 10 * time.Second

// errHLSNotAvailable is returned for HTTP inputs that are HLS playlists until
// the HLS reader exists.
var errHLSNotAvailable = errors.New("HLS input not available")

// defaultHTTPClient has dial and response header timeouts and no overall
// timeout: the body of a live pull never ends.
func defaultHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: httpTimeout}).DialContext,
		TLSHandshakeTimeout:   httpTimeout,
		ResponseHeaderTimeout: httpTimeout,
	}}
}

func httpErr(rawURL, op string, err error) error {
	return fmt.Errorf("ingest: http: %s %s: %w", op, Redact(rawURL), err)
}

// isHLSType reports whether a Content-Type names an HLS playlist.
func isHLSType(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mt = strings.ToLower(strings.TrimSpace(contentType))
	}
	switch mt {
	case "application/vnd.apple.mpegurl", "application/x-mpegurl", "audio/mpegurl", "audio/x-mpegurl":
		return true
	}
	return false
}

// httpBody is the response body behind a sniffing buffer; Close cancels the
// request.
type httpBody struct {
	r      *bufio.Reader
	body   io.ReadCloser
	cancel context.CancelFunc
}

// Read implements io.Reader.
func (b *httpBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	return n, err //nolint:wrapcheck // io.Reader contract: errors pass through unwrapped.
}

// Close cancels the request and closes the body.
func (b *httpBody) Close() error {
	b.cancel()
	_ = b.body.Close()
	return nil
}

// httpReader reads one HTTP-TS response.
type httpReader struct {
	ts     *tsReader
	rawURL string
}

func openHTTP(ctx context.Context, rawURL string, p parsed, env Env) (Reader, error) {
	if p.spec.Pace {
		return nil, httpErr(rawURL, "open", errHLSNotAvailable)
	}
	client := env.HTTP
	if client == nil {
		client = defaultHTTPClient()
	}
	// The request outlives ctx; only Close (or ctx, before the open ends)
	// cancels it.
	rctx, cancel := context.WithCancel(context.Background())
	stop := context.AfterFunc(ctx, cancel)
	r, err := doHTTP(ctx, rctx, cancel, client, rawURL, env)
	if !stop() {
		// ctx ended during the open: the request is cancelled.
		if r != nil {
			_ = r.Close()
		}
		if err == nil {
			err = ctx.Err()
		}
		return nil, httpErr(rawURL, "open", err)
	}
	if err != nil {
		cancel()
		return nil, err
	}
	return r, nil
}

func doHTTP(ctx, rctx context.Context, cancel context.CancelFunc, client *http.Client, rawURL string, env Env) (*httpReader, error) {
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, httpErr(rawURL, "request", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		// net/http errors embed the URL: report ours, redacted.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, httpErr(rawURL, "get", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_ = resp.Body.Close()
		return nil, httpErr(rawURL, "get", fmt.Errorf("status %s", resp.Status))
	}
	if isHLSType(resp.Header.Get("Content-Type")) {
		_ = resp.Body.Close()
		return nil, httpErr(rawURL, "open", errHLSNotAvailable)
	}
	body := &httpBody{r: bufio.NewReaderSize(resp.Body, 64<<10), body: resp.Body, cancel: cancel}
	// MPEG-TS starts with 0x47; only a '#' needs a longer look.
	if first, err := body.r.Peek(1); err == nil && first[0] == '#' {
		if head, _ := body.r.Peek(len("#EXTM3U")); string(head) == "#EXTM3U" {
			_ = body.Close()
			return nil, httpErr(rawURL, "open", errHLSNotAvailable)
		}
	}
	t, err := openTS(ctx, body, env.now(), Spec{Live: true}, &media.ConfigCache{})
	if err != nil {
		return nil, httpErr(rawURL, "open", err)
	}
	return &httpReader{ts: t, rawURL: rawURL}, nil
}

// Read implements Reader. The end of the body is an error, not io.EOF: a
// live pull never ends cleanly.
func (h *httpReader) Read() (Frame, error) {
	f, err := h.ts.Read()
	if err == nil || errors.Is(err, ErrDiscontinuity) {
		return f, err
	}
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return Frame{}, httpErr(h.rawURL, "read", err)
}

// Kinds implements Reader.
func (h *httpReader) Kinds() []media.Kind { return h.ts.Kinds() }

// Spec implements Reader.
func (h *httpReader) Spec() Spec { return h.ts.Spec() }

// Stats implements Reader.
func (h *httpReader) Stats() map[string]uint64 { return h.ts.Stats() }

// Close implements Reader.
func (h *httpReader) Close() error {
	if err := h.ts.Close(); err != nil {
		return httpErr(h.rawURL, "close", err)
	}
	return nil
}
