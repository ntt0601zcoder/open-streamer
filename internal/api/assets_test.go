package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/asset"
	"github.com/ntt0601zcoder/open-streamer/internal/config"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
)

// pngOf returns a 4 × 4 PNG of one colour.
func pngOf(c color.NRGBA) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		panic(err)
	}
	return b.Bytes()
}

var (
	logoPNG  = pngOf(color.NRGBA{R: 200, A: 255})
	otherPNG = pngOf(color.NRGBA{G: 200, A: 255})
)

const fixtureFont = "../../testdata/fixtures/fonts/DejaVuSansMono.ttf"

// fontOf returns the fixture font padded to size bytes: the check reads the
// table directory only.
func fontOf(t *testing.T, size int) []byte {
	t.Helper()
	b, err := os.ReadFile(fixtureFont)
	require.NoError(t, err)
	require.Less(t, len(b), size)
	return append(b, make([]byte, size-len(b))...)
}

func octet(b []byte) reqOpt { return body("application/octet-stream", b) }

// decodeStrict decodes a JSON document into v, refusing unknown fields.
func decodeStrict(t *testing.T, b []byte, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(v), string(b))
}

// count returns how many calls of the fake backend start with prefix.
func (f *fakeBackend) count(prefix string) int {
	n := 0
	for _, c := range f.callList() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// dirEntries returns the names in the asset directory.
func (e *env) dirEntries() []string {
	e.t.Helper()
	ents, err := os.ReadDir(e.assetDir)
	require.NoError(e.t, err)
	out := make([]string, 0, len(ents))
	for _, d := range ents {
		out = append(out, d.Name())
	}
	return out
}

func TestAssetCRUD(t *testing.T) {
	e := newEnv(t)
	const target = "/api/v1/assets/logo.png"

	rec := e.do(http.MethodPut, target, with(editor, octet(logoPNG))...)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var put AssetPut
	decodeStrict(t, rec.Body.Bytes(), &put)
	assert.Equal(t, "logo.png", put.Name)
	assert.Equal(t, "image", put.Kind)
	assert.Equal(t, "image/png", put.Type)
	assert.Equal(t, int64(len(logoPNG)), put.Size)
	assert.False(t, put.Modified.IsZero())
	assert.NotEmpty(t, put.ETag)
	assert.Equal(t, put.ETag, rec.Header().Get("ETag"))
	for _, list := range []string{`"used_by":[]`, `"refreshed":[]`, `"retried":[]`} {
		assert.Contains(t, rec.Body.String(), list)
	}

	rec = e.do(http.MethodGet, "/api/v1/assets", viewer...)
	require.Equal(t, http.StatusOK, rec.Code)
	var list struct {
		Assets []Asset `json:"assets"`
	}
	decodeStrict(t, rec.Body.Bytes(), &list)
	require.Len(t, list.Assets, 1)
	assert.Equal(t, put.Name, list.Assets[0].Name)
	assert.Equal(t, put.ETag, list.Assets[0].ETag)
	assert.Equal(t, []string{}, list.Assets[0].UsedBy)

	rec = e.do(http.MethodGet, target, viewer...)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, logoPNG, rec.Body.Bytes())
	h := rec.Header()
	assert.Equal(t, "image/png", h.Get("Content-Type"))
	assert.Equal(t, put.ETag, h.Get("ETag"))
	assert.Equal(t, `attachment; filename="logo.png"`, h.Get("Content-Disposition"))
	assert.Equal(t, "nosniff", h.Get("X-Content-Type-Options"))
	assert.Equal(t, "no-cache", h.Get("Cache-Control"))

	rec = e.do(http.MethodGet, target, with(viewer, hdr("If-None-Match", put.ETag))...)
	assert.Equal(t, http.StatusNotModified, rec.Code)
	assert.Empty(t, rec.Body.Bytes())

	rec = e.do(http.MethodPut, target, with(editor, octet(otherPNG))...)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var again AssetPut
	decodeStrict(t, rec.Body.Bytes(), &again)
	assert.NotEqual(t, put.ETag, again.ETag)
	assert.Equal(t, again.ETag, rec.Header().Get("ETag"))

	assert.Equal(t, http.StatusNoContent, e.do(http.MethodDelete, target, editor...).Code)
	rec = e.do(http.MethodGet, target, viewer...)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, codeNotFound, errOf(t, rec).Code)
	rec = e.do(http.MethodDelete, target, editor...)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, codeNotFound, errOf(t, rec).Code)
}

func TestAssetNames(t *testing.T) {
	e := newEnv(t)
	e.seedCam()
	e.be.reset()
	names := []string{
		"..%2Flogo.png", "%2e%2e%2fx.png", "a%2Fb.png", "X.PNG", "a.gif", ".png", "a%00.png",
		strings.Repeat("a", 65) + ".png",
	}
	for _, name := range names {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			rec := e.do(method, "/api/v1/assets/"+name, with(editor, octet(logoPNG))...)
			require.Equal(t, http.StatusNotFound, rec.Code, "%s %s", method, name)
			assert.Equal(t, codeNotFound, errOf(t, rec).Code)
		}
	}
	assert.Empty(t, e.be.callList(), "the backend was not called")

	// Two segments are no route of ours.
	rec := e.do(http.MethodGet, "/api/v1/assets/a/b.png", editor...)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Empty(t, e.be.callList())

	// The longest stem is a name.
	rec = e.do(http.MethodGet, "/api/v1/assets/"+strings.Repeat("a", 64)+".png", viewer...)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, 1, e.be.count("Asset "))
}

// countingBody counts the bytes read from it.
type countingBody struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func (c *countingBody) Close() error { return nil }

func TestAssetTooLarge(t *testing.T) {
	e := newEnv(t)
	limit := int64(picture.MaxImageBytes)
	big := append(append([]byte(nil), logoPNG...), make([]byte, int(limit)+1-len(logoPNG))...)

	// With Content-Length: refused before a byte is read.
	c := &countingBody{r: bytes.NewReader(big)}
	rec := e.do(http.MethodPut, "/api/v1/assets/big.png", with(editor, octet(nil), func(r *http.Request) {
		r.Body, r.ContentLength = c, int64(len(big))
	})...)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	assert.Equal(t, codeTooLarge, errOf(t, rec).Code)
	assert.Zero(t, c.n.Load())
	assert.Zero(t, e.be.count("PutAsset"), "the backend was not called")

	// Chunked: refused after at most the limit and one byte.
	c = &countingBody{r: bytes.NewReader(append(big, make([]byte, 1<<20)...))}
	rec = e.do(http.MethodPut, "/api/v1/assets/big.png", with(editor, octet(nil), func(r *http.Request) {
		r.Body, r.ContentLength = c, -1
	})...)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	assert.Equal(t, codeTooLarge, errOf(t, rec).Code)
	assert.LessOrEqual(t, c.n.Load(), limit+1)
	assert.Empty(t, e.dirEntries(), "nothing was stored")

	// The image limit is not the font's.
	rec = e.do(http.MethodPut, "/api/v1/assets/big.ttf", with(editor, octet(fontOf(t, 2<<20)))...)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var put AssetPut
	decodeStrict(t, rec.Body.Bytes(), &put)
	assert.Equal(t, "font", put.Kind)
	assert.Equal(t, "font/ttf", put.Type)
	assert.Equal(t, int64(2<<20), put.Size)
}

func TestAssetContent(t *testing.T) {
	e := newEnv(t)
	rec := e.do(http.MethodPut, "/api/v1/assets/logo.png", with(editor, octet([]byte("<html><body>hi</body></html>")))...)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	doc := errOf(t, rec)
	assert.Equal(t, codeInvalidAsset, doc.Code)
	assert.Contains(t, doc.Message, "image")
	assert.NotContains(t, doc.Message, "asset:", "the reason without the store's prefix")
	assert.NotContains(t, doc.Message, e.assetDir)

	e.be.reset()
	for _, ct := range []string{"text/html", "multipart/form-data; boundary=x", "image/png", ""} {
		rec = e.do(http.MethodPut, "/api/v1/assets/logo.png", with(editor, body(ct, logoPNG))...)
		require.Equal(t, http.StatusUnsupportedMediaType, rec.Code, ct)
		assert.Equal(t, codeMediaType, errOf(t, rec).Code)
	}
	assert.Empty(t, e.be.callList())
	// Parameters do not matter.
	rec = e.do(http.MethodPut, "/api/v1/assets/logo.png", with(editor, body("application/octet-stream; x=y", logoPNG))...)
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

func TestAssetConditions(t *testing.T) {
	e := newEnv(t)
	const target = "/api/v1/assets/logo.png"
	rec := e.do(http.MethodPut, target, with(editor, octet(logoPNG), hdr("If-None-Match", "*"))...)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	tag := rec.Header().Get("ETag")

	rec = e.do(http.MethodPut, target, with(editor, octet(otherPNG), hdr("If-None-Match", "*"))...)
	require.Equal(t, http.StatusPreconditionFailed, rec.Code, rec.Body.String())
	assert.Equal(t, codePrecondition, errOf(t, rec).Code)

	rec = e.do(http.MethodPut, target, with(editor, octet(otherPNG), hdr("If-Match", `"0123456789abcdef"`))...)
	require.Equal(t, http.StatusPreconditionFailed, rec.Code, rec.Body.String())
	assert.Equal(t, codePrecondition, errOf(t, rec).Code)

	rec = e.do(http.MethodDelete, target, with(editor, hdr("If-Match", `"0123456789abcdef"`))...)
	require.Equal(t, http.StatusPreconditionFailed, rec.Code, rec.Body.String())

	rec = e.do(http.MethodPut, target, with(editor, octet(otherPNG), hdr("If-Match", tag))...)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotEqual(t, tag, rec.Header().Get("ETag"))

	e.be.reset()
	rec = e.do(http.MethodPut, target, with(editor, octet(otherPNG), hdr("If-None-Match", `"x"`))...)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, codeBadRequest, errOf(t, rec).Code)
	assert.Empty(t, e.be.callList())
}

func TestAssetErrors(t *testing.T) {
	const secretPath = "/srv/secret-assets"
	many := make([]string, config.MaxErrors+5)
	for i := range many {
		many[i] = fmt.Sprintf("s%d", i)
	}
	for _, c := range []struct {
		name    string
		method  string
		err     error
		status  int
		code    string
		errors  []string
		logMsg  string // the one error line, when there is one
		message string // a part of the message, when it matters
	}{
		{"full", http.MethodPut, fmt.Errorf("%w: 256 files, at most 256", asset.ErrFull), http.StatusInsufficientStorage, codeAssetsFull, nil, "", ""},
		{"off", http.MethodDelete, asset.ErrOff, http.StatusConflict, codeAssetsOff, nil, "", "server.assets_dir is not set"},
		{"off get", http.MethodGet, asset.ErrOff, http.StatusConflict, codeAssetsOff, nil, "", ""},
		{"in use", http.MethodDelete, &AssetInUseError{Streams: []string{"a", "b"}}, http.StatusConflict, codeAssetInUse, []string{"a", "b"}, "", ""},
		{
			"in use, capped", http.MethodDelete, &AssetInUseError{Streams: many}, http.StatusConflict, codeAssetInUse,
			append(append([]string(nil), many[:config.MaxErrors]...), "and 5 more"), "", "",
		},
		{
			"write", http.MethodPut,
			fmt.Errorf("%w: rename: %w", asset.ErrWrite, &os.LinkError{Op: "rename", Old: secretPath + "/.logo.png.0badf00d.tmp", New: secretPath + "/logo.png", Err: syscall.EROFS}),
			http.StatusInternalServerError, codeAssetWrite, nil, "api: the asset was not written", "",
		},
		{
			"probe", http.MethodPut,
			fmt.Errorf("%w: %w", asset.ErrProbe, errors.New("fork/exec "+secretPath+"/ostranscoder: no such file")),
			http.StatusServiceUnavailable, codeProbeFailed, nil, "api: the font probe did not run", "the font could not be checked; try again",
		},
		{
			"skipped", http.MethodDelete, ErrSkippedEntries, http.StatusConflict, codeSkipped, nil, "",
			"the config holds entries that were skipped at start; fix or remove them first",
		},
		{"not found", http.MethodGet, fmt.Errorf("%w: %q", asset.ErrNotFound, "logo.png"), http.StatusNotFound, codeNotFound, nil, "", ""},
		{"name", http.MethodDelete, fmt.Errorf("%w: %q", asset.ErrName, "logo.png"), http.StatusNotFound, codeNotFound, nil, "", ""},
		{"precondition", http.MethodDelete, fmt.Errorf("%w: %q has changed", asset.ErrPrecondition, "logo.png"), http.StatusPreconditionFailed, codePrecondition, nil, "", ""},
		{"too large on disk", http.MethodGet, fmt.Errorf("%w: hand-copied", asset.ErrTooLarge), http.StatusRequestEntityTooLarge, codeTooLarge, nil, "", ""},
		{"body", http.MethodPut, fmt.Errorf("%w: %w", asset.ErrBody, io.ErrUnexpectedEOF), http.StatusBadRequest, codeBadRequest, nil, "", "the body could not be read"},
		{
			"open", http.MethodGet,
			fmt.Errorf("asset: open %q: %w", "logo.png", &fs.PathError{Op: "open", Path: secretPath + "/logo.png", Err: syscall.EACCES}),
			http.StatusInternalServerError, codeInternal, nil, "api: internal error", "",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.be.assetErr = c.err
			opts := editor
			if c.method == http.MethodPut {
				opts = with(editor, octet(logoPNG))
			}
			rec := e.do(c.method, "/api/v1/assets/logo.png", opts...)
			require.Equal(t, c.status, rec.Code, rec.Body.String())
			doc := errOf(t, rec)
			assert.Equal(t, c.code, doc.Code)
			assert.Equal(t, c.errors, doc.Errors)
			assert.Contains(t, doc.Message, c.message)
			assert.NotContains(t, rec.Body.String(), secretPath, "no path reaches the client")
			assert.NotContains(t, rec.Body.String(), "asset:", "no store text reaches the client")
			if c.logMsg != "" {
				lines := e.logs.lines(t, c.logMsg)
				require.Len(t, lines, 1)
				assert.Contains(t, lines[0]["error"], secretPath, "the cause is in the log")
			}
		})
	}
	// Error counts the streams.
	in := &AssetInUseError{Streams: []string{"a", "b"}}
	assert.Equal(t, "api: the asset is in use by 2 streams", in.Error())
}

func TestAssetUploadSlots(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	var entered atomic.Int32
	e.be.putHook = func(ctx context.Context, _ string) {
		entered.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	var wg sync.WaitGroup
	codes := make([]int, 3)
	put := func(ctx context.Context, i int) {
		defer wg.Done()
		codes[i] = e.doCtx(ctx, http.MethodPut, fmt.Sprintf("/api/v1/assets/a%d.png", i), with(editor, octet(logoPNG))...).Code
	}
	wg.Add(2)
	go put(context.Background(), 0)
	go put(context.Background(), 1)
	waitFor(t, "two uploads in the backend", func() bool { return entered.Load() == 2 })

	wg.Add(1)
	go put(context.Background(), 2)
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(2), entered.Load(), "the third waits for a slot")
	assert.Equal(t, 2, e.be.count("PutAsset"))

	release <- struct{}{}
	waitFor(t, "the third in the backend", func() bool { return entered.Load() == 3 })
	release <- struct{}{}
	release <- struct{}{}
	wg.Wait()
	assert.Equal(t, []int{http.StatusCreated, http.StatusCreated, http.StatusCreated}, codes)

	// A third whose request ends while it waits answers 503.
	entered.Store(0)
	e.be.reset()
	wg.Add(2)
	go put(context.Background(), 0)
	go put(context.Background(), 1)
	waitFor(t, "two uploads in the backend", func() bool { return entered.Load() == 2 })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	rec := e.doCtx(ctx, http.MethodPut, "/api/v1/assets/a2.png", with(editor, octet(logoPNG))...)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Equal(t, codeUnavailable, errOf(t, rec).Code)
	assert.Equal(t, 2, e.be.count("PutAsset"), "never passed to the backend")
	release <- struct{}{}
	release <- struct{}{}
	wg.Wait()
	assert.Equal(t, []int{http.StatusOK, http.StatusOK}, codes[:2])
}

// serveReal runs the API behind a real http.Server on 127.0.0.1:0 with
// short read and write timeouts, and returns its address.
func serveReal(t *testing.T, e *env, readTimeout, writeTimeout time.Duration) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{Handler: e.api, ReadTimeout: readTimeout, WriteTimeout: writeTimeout}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		<-done
	})
	return ln.Addr().String()
}

// rawConn dials addr; the test closes it.
func rawConn(t *testing.T, addr string) *net.TCPConn {
	t.Helper()
	var d net.Dialer
	c, err := d.DialContext(context.Background(), "tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	tc, ok := c.(*net.TCPConn)
	require.True(t, ok)
	return tc
}

// head is a request's head as an editor sends it.
func head(method, path string, extra ...string) string {
	cred := base64.StdEncoding.EncodeToString([]byte("ops:" + edPass))
	lines := append([]string{
		method + " " + path + " HTTP/1.1", "Host: test", "Authorization: Basic " + cred, "X-Requested-By: test",
	}, extra...)
	return strings.Join(lines, "\r\n") + "\r\n\r\n"
}

// readPutAnswer reads the answer to a PUT from c, within 10 s: its status
// and its body.
func readPutAnswer(t *testing.T, c net.Conn) (int, []byte, error) {
	t.Helper()
	require.NoError(t, c.SetReadDeadline(time.Now().Add(10*time.Second)))
	res, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodPut})
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return res.StatusCode, b, err
}

// sendSlowly writes b in pieces, piece i at gap × i from now: the last
// one goes at gap × (pieces-1), whatever the writes take.
func sendSlowly(t *testing.T, w io.Writer, b []byte, pieces int, gap time.Duration) {
	t.Helper()
	start := time.Now()
	size := (len(b) + pieces - 1) / pieces
	for i := 0; len(b) > 0; i++ {
		time.Sleep(time.Until(start.Add(time.Duration(i) * gap)))
		n := min(size, len(b))
		_, err := w.Write(b[:n])
		require.NoError(t, err)
		b = b[n:]
	}
}

func TestAssetDeadlines(t *testing.T) {
	// The unscaled figures.
	assert.Equal(t, 34*time.Second, defaultTiming.deadline(1<<20))
	assert.Equal(t, 126*time.Second, defaultTiming.deadline(24<<20))
	assert.Equal(t, 30*time.Second, defaultTiming.deadline(0))
	assert.Equal(t, 31*time.Second, defaultTiming.deadline(1))

	e := newEnv(t)
	// deadline(an image's limit) is 1.12 s, deadline(8 MiB) 1.96 s.
	e.api.timing = timing{base: time.Second, per256KiB: 30 * time.Millisecond, slotWait: 30 * time.Second, writeMore: time.Second}
	addr := serveReal(t, e, 200*time.Millisecond, 200*time.Millisecond)
	putHead := func(name string, n int) string {
		return head(http.MethodPut, "/api/v1/assets/"+name, "Content-Type: application/octet-stream", fmt.Sprintf("Content-Length: %d", n))
	}

	t.Run("slow body", func(t *testing.T) {
		c := rawConn(t, addr)
		_, err := io.WriteString(c, putHead("slow.png", len(logoPNG)))
		require.NoError(t, err)
		sendSlowly(t, c, logoPNG, 6, 120*time.Millisecond) // the last piece at 600 ms, three times the server's ReadTimeout
		code, b, err := readPutAnswer(t, c)
		require.NoError(t, err)
		assert.Equal(t, http.StatusCreated, code, string(b))
	})

	t.Run("stalled body", func(t *testing.T) {
		c := rawConn(t, addr)
		_, err := io.WriteString(c, putHead("stall.png", len(logoPNG)))
		require.NoError(t, err)
		_, err = c.Write(logoPNG[:len(logoPNG)/2])
		require.NoError(t, err)
		start := time.Now()
		code, b, err := readPutAnswer(t, c)
		if err == nil { // or the connection was closed
			assert.Equal(t, http.StatusBadRequest, code, string(b))
			assert.Contains(t, string(b), `"code":"bad_request"`)
			assert.Contains(t, string(b), "the body could not be read")
		}
		assert.GreaterOrEqual(t, time.Since(start), 900*time.Millisecond, "its own deadline, not the server's ReadTimeout")
		for _, n := range e.dirEntries() {
			assert.NotContains(t, n, "stall", "no file and no temp file")
		}
	})

	t.Run("waits for a slot", func(t *testing.T) {
		release := make(chan struct{})
		var held atomic.Int32
		e.be.mu.Lock()
		e.be.putHook = func(ctx context.Context, name string) {
			if strings.HasPrefix(name, "held") {
				held.Add(1)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
		}
		e.be.mu.Unlock()
		t.Cleanup(func() {
			e.be.mu.Lock()
			e.be.putHook = nil
			e.be.mu.Unlock()
		})
		var wg sync.WaitGroup
		for i := range 2 {
			wg.Go(func() {
				e.do(http.MethodPut, fmt.Sprintf("/api/v1/assets/held%d.png", i), with(editor, octet(logoPNG))...)
			})
		}
		waitFor(t, "two uploads held", func() bool { return held.Load() == 2 })

		c := rawConn(t, addr)
		_, err := io.WriteString(c, putHead("third.png", len(logoPNG)))
		require.NoError(t, err)
		time.Sleep(400 * time.Millisecond)
		release <- struct{}{}
		// The last piece at 1.32 s from the head: past the 1.12 s of a
		// deadline that would have started with the request, inside the
		// 1.52 s of the one that starts with the slot, at 0.4 s.
		sendSlowly(t, c, logoPNG, 9, 115*time.Millisecond)
		code, b, err := readPutAnswer(t, c)
		require.NoError(t, err)
		assert.Equal(t, http.StatusCreated, code, string(b))
		release <- struct{}{}
		wg.Wait()
	})

	t.Run("slow reader", func(t *testing.T) {
		big := fontOf(t, 8<<20) // more than the loopback's buffers hold
		_, _, err := e.be.assets.Put(context.Background(), "big.ttf", bytes.NewReader(big), "", false)
		require.NoError(t, err)
		c := rawConn(t, addr)
		require.NoError(t, c.SetReadBuffer(32<<10))
		_, err = io.WriteString(c, head(http.MethodGet, "/api/v1/assets/big.ttf"))
		require.NoError(t, err)
		require.NoError(t, c.SetReadDeadline(time.Now().Add(10*time.Second)))
		br := bufio.NewReaderSize(c, 64<<10)
		res, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
		require.NoError(t, err)
		defer res.Body.Close()
		require.Equal(t, http.StatusOK, res.StatusCode)
		start := time.Now()
		var got bytes.Buffer
		chunk := make([]byte, 128<<10)
		for {
			n, err := io.ReadFull(res.Body, chunk)
			got.Write(chunk[:n])
			if err != nil {
				require.True(t, errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF), err)
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		assert.GreaterOrEqual(t, time.Since(start), 600*time.Millisecond)
		assert.True(t, bytes.Equal(big, got.Bytes()), "every byte: got %d of %d", got.Len(), len(big))
	})
}

// guardedFile fails the test on a Read of more than 1 MiB at once and
// counts its Closes.
type guardedFile struct {
	io.ReadSeekCloser
	t      *testing.T
	closes *atomic.Int32
}

func (g guardedFile) Read(p []byte) (int, error) {
	if len(p) > 1<<20 {
		g.t.Errorf("a read of %d bytes at once", len(p))
	}
	return g.ReadSeekCloser.Read(p)
}

func (g guardedFile) Close() error {
	g.closes.Add(1)
	return g.ReadSeekCloser.Close()
}

func TestAssetGetStreams(t *testing.T) {
	e := newEnv(t)
	font := fontOf(t, 2<<20)
	inf, _, err := e.be.assets.Put(context.Background(), "big.ttf", bytes.NewReader(font), "", false)
	require.NoError(t, err)
	var closes atomic.Int32
	e.be.openHook = func(f io.ReadSeekCloser) io.ReadSeekCloser {
		return guardedFile{ReadSeekCloser: f, t: t, closes: &closes}
	}

	rec := e.do(http.MethodGet, "/api/v1/assets/big.ttf", viewer...)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, bytes.Equal(font, rec.Body.Bytes()))
	assert.Equal(t, "font/ttf", rec.Header().Get("Content-Type"))
	assert.Equal(t, int32(1), closes.Load())

	rec = e.do(http.MethodGet, "/api/v1/assets/big.ttf", with(viewer, hdr("If-None-Match", inf.ETag))...)
	require.Equal(t, http.StatusNotModified, rec.Code)
	assert.Equal(t, int32(2), closes.Load())

	// The client hangs up part way.
	srv := httptest.NewServer(e.api)
	t.Cleanup(srv.Close)
	c := rawConn(t, strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, c.SetReadBuffer(16<<10))
	_, err = io.WriteString(c, head(http.MethodGet, "/api/v1/assets/big.ttf"))
	require.NoError(t, err)
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	require.NoError(t, err)
	defer res.Body.Close() // after the hang-up: it only fails
	require.Equal(t, http.StatusOK, res.StatusCode)
	_, err = io.ReadFull(res.Body, make([]byte, 4096))
	require.NoError(t, err)
	require.NoError(t, c.Close())
	waitFor(t, "the file closed", func() bool { return closes.Load() == 3 })
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int32(3), closes.Load(), "exactly once")
}

func TestAssetBodyAborted(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(e.api)
	t.Cleanup(srv.Close)
	c := rawConn(t, strings.TrimPrefix(srv.URL, "http://"))
	_, err := io.WriteString(c, head(http.MethodPut, "/api/v1/assets/logo.png",
		"Content-Type: application/octet-stream", "Content-Length: 1000"))
	require.NoError(t, err)
	_, err = c.Write(logoPNG[:10])
	require.NoError(t, err)
	require.NoError(t, c.CloseWrite())
	code, b, err := readPutAnswer(t, c)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, code, string(b))
	assert.Contains(t, string(b), `"code":"bad_request"`)
	assert.Contains(t, string(b), "the body could not be read")

	e.be.mu.Lock()
	putErrors := append([]error(nil), e.be.putErrors...)
	e.be.mu.Unlock()
	require.Len(t, putErrors, 1)
	require.ErrorIs(t, putErrors[0], asset.ErrBody)
	assert.ErrorIs(t, putErrors[0], io.ErrUnexpectedEOF)
	assert.Empty(t, e.dirEntries(), "nothing was stored")
}

func TestAssetRoles(t *testing.T) {
	e := newEnv(t)
	e.seedCam()
	const one = "/api/v1/assets/logo.png"
	assert.Equal(t, http.StatusOK, e.do(http.MethodGet, "/api/v1/assets", viewer...).Code)
	assert.Equal(t, http.StatusOK, e.do(http.MethodGet, one, viewer...).Code)
	e.be.reset()
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		rec := e.do(method, one, with(viewer, octet(otherPNG))...)
		require.Equal(t, http.StatusForbidden, rec.Code, method)
		assert.Equal(t, codeForbidden, errOf(t, rec).Code)
	}
	assert.Empty(t, e.be.callList())
	for _, c := range []struct{ method, target string }{
		{http.MethodGet, "/api/v1/assets"}, {http.MethodGet, one}, {http.MethodPut, one}, {http.MethodDelete, one},
	} {
		rec := e.do(c.method, c.target, xrb, octet(otherPNG))
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "%s %s", c.method, c.target)
	}
	rec := e.do(http.MethodPut, one, basic("ops", edPass), octet(otherPNG))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "the X-Requested-By header is required", errOf(t, rec).Message)
	assert.Empty(t, e.be.callList())
}
