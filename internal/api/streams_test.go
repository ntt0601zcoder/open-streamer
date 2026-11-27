package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/config"
	"github.com/ntt0601zcoder/open-streamer/internal/redact"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
	"github.com/ntt0601zcoder/open-streamer/internal/stream"
)

func jsonBody(s string) reqOpt { return body("application/json", []byte(s)) }

func TestStreamCRUD(t *testing.T) {
	e := newEnv(t)
	want, err := spec.Decode(strings.NewReader(camSpec))
	require.NoError(t, err)
	want.Name = "cam"

	rec := e.do(http.MethodPut, "/api/v1/streams/cam", with(editor, jsonBody(camSpec), hdr("If-Match", `"abc"`))...)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, want.ETag(), rec.Header().Get("ETag"))
	var put putResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &put))
	assert.Equal(t, "created", put.Applied)
	assert.Equal(t, want, put.Stream, "no name in the body: the path's")
	assert.Equal(t, []string{`"abc"`}, e.be.ifMatch, "If-Match reaches the backend verbatim")

	rec = e.do(http.MethodGet, "/api/v1/streams/cam", viewer...)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, want.ETag(), rec.Header().Get("ETag"))
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var got spec.Stream
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, want, got)

	// Again, with the name in the body and a charset on the media type.
	rec = e.do(http.MethodPut, "/api/v1/streams/cam",
		with(editor, body("application/json; charset=utf-8", []byte(`{"name":"cam",`+camSpec[1:])))...)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &put))
	assert.Equal(t, "unchanged", put.Applied)
	assert.Equal(t, []string{`"abc"`, ""}, e.be.ifMatch)

	e.be.streams["a"] = spec.Stream{Name: "a", Inputs: []spec.Input{{URL: "udp://239.0.0.9:1"}}}
	e.be.streams["z"] = spec.Stream{Name: "z", Inputs: []spec.Input{{URL: "udp://239.0.0.9:2"}}}
	rec = e.do(http.MethodGet, "/api/v1/streams", viewer...)
	require.Equal(t, http.StatusOK, rec.Code)
	var list struct {
		Streams []spec.Stream `json:"streams"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Streams, 3)
	assert.Equal(t, []string{"a", "cam", "z"}, []string{list.Streams[0].Name, list.Streams[1].Name, list.Streams[2].Name})

	// A body name other than the path's.
	e.be.reset()
	rec = e.do(http.MethodPut, "/api/v1/streams/cam", with(editor, jsonBody(`{"name":"other",`+camSpec[1:]))...)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, codeBadRequest, errOf(t, rec).Code)
	assert.Empty(t, e.be.callList())

	rec = e.do(http.MethodDelete, "/api/v1/streams/cam", with(editor, hdr("If-Match", want.ETag()))...)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String())
	assert.Equal(t, []string{want.ETag()}, e.be.ifMatch)
	rec = e.do(http.MethodGet, "/api/v1/streams/cam", viewer...)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, codeNotFound, errOf(t, rec).Code)
	assert.Empty(t, rec.Header().Get("ETag"))

	// With no stream the list is [], not null.
	delete(e.be.streams, "a")
	delete(e.be.streams, "z")
	rec = e.do(http.MethodGet, "/api/v1/streams", viewer...)
	assert.JSONEq(t, `{"streams":[]}`, rec.Body.String())
}

// reason is an error shaped like the stream manager's own: it unwraps to
// its kind and, for an invalid spec, its cause.
type reason struct {
	kind, cause error
	msg         string
}

func (r *reason) Error() string { return r.msg }
func (r *reason) Unwrap() []error {
	if r.cause == nil {
		return []error{r.kind}
	}
	return []error{r.kind, r.cause}
}

func TestErrors(t *testing.T) {
	invalid := &reason{kind: stream.ErrInvalid, cause: errors.New("no transcoder"), msg: `stream "cam": no transcoder`}
	claim := &reason{kind: stream.ErrConflict, msg: `stream "cam": udp :5000 is claimed by "door"`}
	cycle := &reason{kind: stream.ErrConflict, msg: `stream "cam": copy:// cycle: cam, door, cam`}
	put := func(opts ...reqOpt) (string, string, []reqOpt) {
		return http.MethodPut, "/api/v1/streams/cam", with(editor, append([]reqOpt{jsonBody(camSpec)}, opts...)...)
	}
	type row struct {
		name   string
		err    error // what the Backend's PutStream returns
		req    func() (method, target string, opts []reqOpt)
		status int
		code   string
		errs   []string
	}
	rows := []row{
		{name: "not JSON", req: func() (string, string, []reqOpt) { return put(jsonBody(`{"inputs":`)) }, status: 400, code: codeBadRequest},
		{name: "two documents", req: func() (string, string, []reqOpt) { return put(jsonBody(camSpec + camSpec)) }, status: 400, code: codeBadRequest},
		{name: "an unknown field", req: func() (string, string, []reqOpt) { return put(jsonBody(`{"nope":1}`)) }, status: 400, code: codeBadRequest},
		{name: "an empty body", req: func() (string, string, []reqOpt) { return put(jsonBody("")) }, status: 400, code: codeBadRequest},
		{name: "another name", req: func() (string, string, []reqOpt) { return put(jsonBody(`{"name":"door"}`)) }, status: 400, code: codeBadRequest},
		{name: "a config syntax error", err: &config.SyntaxError{Err: errors.New("yaml: line 3: cannot unmarshal !!str `hunter2` into int")}, status: 400, code: codeBadRequest},
		{name: "not found", err: ErrNotFound, status: 404, code: codeNotFound},
		{name: "not found, wrapped", err: fmt.Errorf("server: %w", ErrNotFound), status: 404, code: codeNotFound},
		{name: "no such stream", err: fmt.Errorf("server: %w", config.ErrNoStream), status: 404, code: codeNotFound},
		{
			name: "two conflicts", err: errors.Join(claim, cycle), status: 409, code: codeConflict,
			errs: []string{claim.msg, cycle.msg},
		},
		{name: "one conflict", err: claim, status: 409, code: codeConflict, errs: []string{claim.msg}},
		{
			name: "conflicts, wrapped", err: fmt.Errorf("server: apply: %w", errors.Join(claim, cycle)), status: 409, code: codeConflict,
			errs: []string{claim.msg, cycle.msg},
		},
		{
			name: "a plain wrapped conflict", err: fmt.Errorf("%w: the port is taken", stream.ErrConflict), status: 409, code: codeConflict,
			errs: []string{"stream: conflict: the port is taken"},
		},
		{name: "the file changed", err: fmt.Errorf("server: %w", config.ErrStale), status: 409, code: codeChanged},
		{name: "precondition failed", err: ErrPrecondition, status: 412, code: codePrecondition},
		{
			name: "too large", status: 413, code: codeTooLarge,
			req: func() (string, string, []reqOpt) {
				return put(jsonBody(`{"inputs":[{"url":"` + strings.Repeat("a", spec.MaxSpecBytes) + `"}]}`))
			},
		},
		{
			name: "too large, of unknown length", status: 413, code: codeTooLarge,
			req: func() (string, string, []reqOpt) {
				return put(jsonBody(`{"inputs":[{"url":"`+strings.Repeat("a", spec.MaxSpecBytes)+`"}]}`),
					func(r *http.Request) { r.ContentLength = -1 })
			},
		},
		{name: "text/plain", req: func() (string, string, []reqOpt) { return put(body("text/plain", []byte(camSpec))) }, status: 415, code: codeMediaType},
		{name: "no media type", req: func() (string, string, []reqOpt) { return put(body("", []byte(camSpec))) }, status: 415, code: codeMediaType},
		{name: "yaml for a stream", req: func() (string, string, []reqOpt) { return put(body("application/yaml", []byte(camSpec))) }, status: 415, code: codeMediaType},
		{
			name:   "an invalid config",
			err:    config.Errors{errors.New("streams[0] (cam): one"), errors.New("streams[1] (door): two"), errors.New("users[0].role: three")},
			status: 422, code: codeInvalidConf,
			errs: []string{"streams[0] (cam): one", "streams[1] (door): two", "users[0].role: three"},
		},
		{
			name: "an invalid config, wrapped", err: fmt.Errorf("server: %w", config.Errors{errors.New("one")}),
			status: 422, code: codeInvalidConf, errs: []string{"one"},
		},
		{
			name: "an invalid spec beside two conflicts", err: errors.Join(claim, invalid, cycle), status: 422, code: codeInvalidSpec,
			errs: []string{claim.msg, invalid.msg, cycle.msg},
		},
		{name: "an invalid spec", err: invalid, status: 422, code: codeInvalidSpec, errs: []string{invalid.msg}},
		{
			name: "invalid, wrapped", err: fmt.Errorf("server: %w", errors.Join(invalid, claim)), status: 422, code: codeInvalidSpec,
			errs: []string{invalid.msg, claim.msg},
		},
		{name: "precondition required", err: ErrPreconditionRequired, status: 428, code: codePrecondReq},
		{name: "anything else", err: errors.New("write /etc/os/config.yaml: disk full, rtsp://u:hunter2@h/a"), status: 500, code: codeInternal},
		{name: "a bare Validate error", err: errors.New("spec: validate: name"), status: 500, code: codeInternal},
		{name: "closed", err: fmt.Errorf("server: %w", stream.ErrClosed), status: 503, code: codeUnavailable},
	}
	for _, c := range rows {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.be.putErr = c.err
			req := c.req
			if req == nil {
				req = func() (string, string, []reqOpt) { return put() }
			}
			method, target, opts := req()
			rec := e.do(method, target, opts...)
			require.Equal(t, c.status, rec.Code, rec.Body.String())
			doc := errOf(t, rec)
			assert.Equal(t, c.code, doc.Code)
			assert.NotEmpty(t, doc.Message)
			assert.Equal(t, c.errs, doc.Errors)
			assert.Empty(t, rec.Header().Get("ETag"), "an error has no ETag")
			assert.NotContains(t, rec.Body.String(), "hunter2")
			if c.err == nil {
				assert.Empty(t, e.be.callList(), "refused before the backend")
			}
			logged := e.logs.lines(t, "api: internal error")
			if c.status != http.StatusInternalServerError {
				assert.Empty(t, logged)
				return
			}
			assert.Equal(t, "internal error", doc.Message)
			require.Len(t, logged, 1)
			assert.Equal(t, "ERROR", logged[0]["level"])
			assert.Contains(t, logged[0]["error"], strings.SplitN(c.err.Error(), ",", 2)[0])
			assert.NotContains(t, e.logs.String(), "hunter2")
		})
	}

	// The same rows through DELETE.
	e := newEnv(t)
	e.be.delErr = errors.Join(invalid, claim)
	rec := e.do(http.MethodDelete, "/api/v1/streams/cam", editor...)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Len(t, errOf(t, rec).Errors, 2)
	e.be.delErr = nil
	rec = e.do(http.MethodDelete, "/api/v1/streams/cam", editor...)
	assert.Equal(t, http.StatusNotFound, rec.Code, "config.ErrNoStream")

	// A request whose client went away while the edit waited is not a
	// failure of the server.
	e.be.putErr = fmt.Errorf("server: %w", context.Canceled)
	ctx, cancel := context.WithCancel(context.Background())
	require.Equal(t, http.StatusOK, e.do(http.MethodGet, "/api/v1/version", editor...).Code) // remembered
	cancel()
	rec = e.doCtx(ctx, http.MethodPut, "/api/v1/streams/cam", with(editor, jsonBody(camSpec))...)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Empty(t, e.logs.lines(t, "api: internal error"))
	// The same error while the client is still there is a 500.
	rec = e.do(http.MethodPut, "/api/v1/streams/cam", with(editor, jsonBody(camSpec))...)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// TestErrorsFromManager runs a refused edit through the real stream
// manager, whose error type the API cannot name: its joined error is split
// into one text per reason.
func TestErrorsFromManager(t *testing.T) {
	m := stream.NewManager(stream.Env{})
	mk := func(name, doc string) spec.Stream {
		sp, err := spec.Decode(strings.NewReader(doc))
		require.NoError(t, err)
		sp.Name = name
		require.NoError(t, sp.Validate())
		return sp
	}
	const listen = `{"inputs":[{"url":"udp://127.0.0.1:45000"}],"outputs":{"hls":true}}`
	const ladder = `{"inputs":[{"url":"udp://127.0.0.1:45000"}],"outputs":{"hls":true},
		"transcode":{"renditions":[{"label":"low","height":360,"bitrate_kbps":800}],"audio":{"copy":true}}}`

	_, conflicts := m.Replace(context.Background(), []spec.Stream{mk("a", listen), mk("b", listen)}, nil)
	require.ErrorIs(t, conflicts, stream.ErrConflict)
	require.NotErrorIs(t, conflicts, stream.ErrInvalid)
	_, mixed := m.Replace(context.Background(), []spec.Stream{mk("a", listen), mk("b", ladder)}, nil)
	require.ErrorIs(t, mixed, stream.ErrConflict)
	require.ErrorIs(t, mixed, stream.ErrInvalid)

	e := newEnv(t)
	e.be.putErr = conflicts
	rec := e.do(http.MethodPut, "/api/v1/streams/cam", with(editor, jsonBody(camSpec))...)
	require.Equal(t, http.StatusConflict, rec.Code)
	doc := errOf(t, rec)
	assert.Equal(t, codeConflict, doc.Code)
	require.Len(t, doc.Errors, 1)
	assert.Contains(t, doc.Errors[0], `stream "b"`)

	e.be.putErr = fmt.Errorf("server: apply: %w", mixed)
	rec = e.do(http.MethodPut, "/api/v1/streams/cam", with(editor, jsonBody(camSpec))...)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	doc = errOf(t, rec)
	assert.Equal(t, codeInvalidSpec, doc.Code)
	require.Len(t, doc.Errors, 2, "the invalid spec and the conflict")
	for _, s := range doc.Errors {
		assert.Contains(t, s, `stream "b"`)
		assert.NotEqual(t, stream.ErrInvalid.Error(), s)
		assert.NotEqual(t, stream.ErrConflict.Error(), s)
	}
}

// TestErrorsFromManagerCapped: an edit refused for more reasons than
// config.MaxErrors lists the first MaxErrors in order, then "and N more".
func TestErrorsFromManagerCapped(t *testing.T) {
	reasons := func(kind error, n int) ([]string, error) {
		errs := make([]error, n)
		msgs := make([]string, n)
		for i := range n {
			msgs[i] = fmt.Sprintf(`stream "s%03d": reason %d`, i, i)
			errs[i] = &reason{kind: kind, msg: msgs[i]}
		}
		return msgs, errors.Join(errs...)
	}
	for _, c := range []struct {
		kind   error
		status int
		code   string
	}{
		{stream.ErrInvalid, http.StatusUnprocessableEntity, codeInvalidSpec},
		{stream.ErrConflict, http.StatusConflict, codeConflict},
	} {
		t.Run(c.code, func(t *testing.T) {
			e := newEnv(t)
			msgs, err := reasons(c.kind, config.MaxErrors+50)
			e.be.putErr = fmt.Errorf("server: apply: %w", err)
			rec := e.do(http.MethodPut, "/api/v1/streams/cam", with(editor, jsonBody(camSpec))...)
			require.Equal(t, c.status, rec.Code, rec.Body.String())
			doc := errOf(t, rec)
			assert.Equal(t, c.code, doc.Code)
			require.Len(t, doc.Errors, config.MaxErrors+1)
			assert.Equal(t, msgs[:config.MaxErrors], doc.Errors[:config.MaxErrors])
			assert.Equal(t, "and 50 more", doc.Errors[config.MaxErrors])

			msgs, err = reasons(c.kind, config.MaxErrors)
			e.be.putErr = err
			rec = e.do(http.MethodPut, "/api/v1/streams/cam", with(editor, jsonBody(camSpec))...)
			require.Equal(t, c.status, rec.Code, rec.Body.String())
			assert.Equal(t, msgs, errOf(t, rec).Errors, "exactly MaxErrors: no tail")
		})
	}
}

func TestPathNames(t *testing.T) {
	e := newEnv(t)
	e.seedCam()
	e.be.reset()
	for _, name := range []string{"%2e%2e", "a%2Fb", "A", "x_y", "con", strings.Repeat("a", 64), "%00", "cam%20", "c%C3%A4m"} {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			for _, suffix := range []string{"", "/status", "/thumbnail.jpg"} {
				if suffix != "" && method != http.MethodGet {
					continue
				}
				rec := e.do(method, "/api/v1/streams/"+name+suffix, with(editor, jsonBody(camSpec))...)
				require.Equal(t, http.StatusNotFound, rec.Code, "%s %s%s", method, name, suffix)
				assert.Equal(t, codeNotFound, errOf(t, rec).Code)
			}
		}
	}
	// The mux cleans a literal ".." into a redirect before any handler.
	for _, opts := range [][]reqOpt{nil, editor, viewer} {
		for _, target := range []string{"/api/v1/streams/../x", "/api/v1/streams/../../healthz/../api/v1/config", "/api/v1/streams/cam/../../config"} {
			rec := e.do(http.MethodGet, target, opts...)
			assert.GreaterOrEqual(t, rec.Code, 300, target)
			assert.NotContains(t, rec.Body.String(), "version: 1")
		}
	}
	assert.Empty(t, e.be.callList(), "the backend was not called")

	// The longest valid name is a name.
	rec := e.do(http.MethodGet, "/api/v1/streams/"+strings.Repeat("a", 63), viewer...)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Len(t, e.be.callList(), 1)
}

const (
	secretSpec = `{"inputs":[{"url":"rtsp://u:hunter2@h/a"}],"outputs":{"push":[{"url":"rtmp://h/live/secretkey"}]}}`
)

func TestResponsesRedact(t *testing.T) {
	e := newEnv(t)
	holds := func(s string) bool { return strings.Contains(s, "hunter2") || strings.Contains(s, "secretkey") }

	for _, who := range [][]reqOpt{editor} {
		rec := e.do(http.MethodPut, "/api/v1/streams/cam", with(who, jsonBody(secretSpec))...)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		assert.False(t, holds(rec.Body.String()), rec.Body.String())
		assert.Contains(t, rec.Body.String(), "rtsp://u:")
	}
	stored, _ := e.be.Get("cam")
	assert.Equal(t, "rtsp://u:hunter2@h/a", stored.Inputs[0].URL, "the backend got the secret")

	for _, who := range [][]reqOpt{viewer, editor} {
		for _, target := range []string{
			"/api/v1/streams/cam", "/api/v1/streams",
			"/api/v1/streams/cam?x=1", "/api/v1/streams/cam?Secrets=1", "/api/v1/streams/cam?secret=1",
		} {
			rec := e.do(http.MethodGet, target, who...)
			require.Equal(t, http.StatusOK, rec.Code, target)
			assert.False(t, holds(rec.Body.String()), "%s: %s", target, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "rtsp://u:")
		}
	}
	// A query that holds the key secrets, whatever its value and whatever
	// else is in it, is a 400 for every role: there is no unredacted view.
	e.be.reset()
	for _, target := range []string{
		"/api/v1/streams/cam?secrets=1", "/api/v1/streams?secrets=1",
		"/api/v1/streams/cam?secrets=0", "/api/v1/streams?secrets=0", "/api/v1/streams/cam?secrets=true",
		"/api/v1/streams/cam?secrets=1&x=1", "/api/v1/streams?secrets=1&x=1", "/api/v1/streams?x=1&secrets=1",
		"/api/v1/streams/cam?secrets=1&secrets=1", "/api/v1/streams/cam?secrets=1&", "/api/v1/streams/cam?secrets=%31",
	} {
		for _, who := range [][]reqOpt{viewer, editor} {
			rec := e.do(http.MethodGet, target, who...)
			require.Equal(t, http.StatusBadRequest, rec.Code, target)
			assert.Equal(t, codeBadRequest, errOf(t, rec).Code)
			assert.False(t, holds(rec.Body.String()), target)
		}
	}
	// The PUT too: the backend is not asked.
	rec := e.do(http.MethodPut, "/api/v1/streams/cam?secrets=1", with(editor, jsonBody(secretSpec))...)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.False(t, holds(rec.Body.String()))
	assert.Empty(t, e.be.callList())

	// The ETag is the stored document's, for every role.
	for _, who := range [][]reqOpt{viewer, editor} {
		rec := e.do(http.MethodGet, "/api/v1/streams/cam", who...)
		assert.Equal(t, stored.ETag(), rec.Header().Get("ETag"))
	}

	assert.False(t, holds(e.logs.String()), "nothing of a spec is logged")
}

// TestPutResolvesMaskedSecrets: a redacted document written back as it
// came keeps the stored secrets (R-T4); it was refused before.
func TestPutResolvesMaskedSecrets(t *testing.T) {
	e := newEnv(t)
	e.be.validate = true
	rec := e.do(http.MethodPut, "/api/v1/streams/cam", with(editor, jsonBody(secretSpec))...)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = e.do(http.MethodGet, "/api/v1/streams/cam", editor...)
	require.Equal(t, http.StatusOK, rec.Code)
	redacted := bytes.Clone(rec.Body.Bytes())
	require.NotContains(t, string(redacted), "hunter2")

	// The redacted document, written back as it came.
	rec = e.do(http.MethodPut, "/api/v1/streams/cam", with(editor, body("application/json", redacted))...)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var put putResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &put))
	assert.Equal(t, "unchanged", put.Applied)
	assert.NotContains(t, rec.Body.String(), "hunter2")

	stored := e.be.streams["cam"]
	assert.Equal(t, "rtsp://u:hunter2@h/a", stored.Inputs[0].URL)
	assert.Equal(t, "rtmp://h/live/secretkey", stored.Outputs.Push[0].URL)
}

func TestStatusAndThumbnail(t *testing.T) {
	e := newEnv(t)
	e.seedCam()
	at := time.Date(2026, 11, 26, 11, 0, 0, 0, time.UTC)
	st := stream.Status{
		Name: "cam", State: "running", Generation: 2, Since: at, Restarts: 1,
		Served: []string{"cam"}, Inputs: []string{"udp://239.0.0.1:5000"}, ActiveInput: 0,
		Tracks: []stream.Track{{Track: 0, Kind: "video", Codec: "h264"}},
	}
	e.be.statuses["cam"] = st

	rec := e.do(http.MethodGet, "/api/v1/streams/cam/status", viewer...)
	require.Equal(t, http.StatusOK, rec.Code)
	want, err := json.Marshal(st)
	require.NoError(t, err)
	assert.JSONEq(t, string(want), rec.Body.String(), "the backend's document")

	// Between generations there is no since: null, not the year 1.
	st.Since, st.State = time.Time{}, "backoff"
	e.be.statuses["cam"] = st
	rec = e.do(http.MethodGet, "/api/v1/streams/cam/status", viewer...)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "0001-01-01")
	var doc map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
	since, ok := doc["since"]
	assert.True(t, ok)
	assert.Nil(t, since)
	assert.Equal(t, "backoff", doc["state"])

	assert.Equal(t, http.StatusNotFound, e.do(http.MethodGet, "/api/v1/streams/nope/status", viewer...).Code)

	rec = e.do(http.MethodGet, "/api/v1/streams/cam/thumbnail.jpg", viewer...)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "\xff\xd8 a jpeg \xff\xd9", rec.Body.String())
	assert.Equal(t, "image/jpeg", rec.Header().Get("Content-Type"))
	assert.Equal(t, `"3-90000"`, rec.Header().Get("ETag"))
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "Thu, 26 Nov 2026 11:59:00 GMT", rec.Header().Get("Last-Modified"))

	rec = e.do(http.MethodGet, "/api/v1/streams/cam/thumbnail.jpg", with(viewer, hdr("If-None-Match", `"3-90000"`))...)
	assert.Equal(t, http.StatusNotModified, rec.Code)
	assert.Empty(t, rec.Body.String())

	// No shot yet, and an unknown stream.
	delete(e.be.shots, "cam")
	rec = e.do(http.MethodGet, "/api/v1/streams/cam/thumbnail.jpg", viewer...)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, codeNotFound, errOf(t, rec).Code)
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, http.StatusNotFound, e.do(http.MethodGet, "/api/v1/streams/nope/thumbnail.jpg", viewer...).Code)
}

// The secrets of the write-only tests: literals found nowhere else.
const (
	woPass  = "wo-api-pw-4815"
	woKey   = "wokey-api-162342"
	woSRT   = "wosrtpass-api-0815"
	woInput = "rtsp://u:" + woPass + "@cam.example/s"
	woPush  = "rtmp://push.example/live/" + woKey
)

// seedSecrets stores the stream cam with an input password, a push key and
// an SRT passphrase.
func (e *env) seedSecrets() spec.Stream {
	e.t.Helper()
	sp := spec.Stream{
		Name:   "cam",
		Inputs: []spec.Input{{URL: woInput}, {URL: "udp://239.0.0.1:5000"}},
		Outputs: spec.Outputs{
			HLS: true, SRT: true, SRTPassphrase: woSRT,
			Push: []spec.Push{{URL: woPush}},
		},
	}
	e.be.mu.Lock()
	e.be.streams["cam"] = sp
	e.be.mu.Unlock()
	return sp
}

func holdsWO(s string) bool {
	return strings.Contains(s, woPass) || strings.Contains(s, woKey) || strings.Contains(s, woSRT)
}

func TestStreamsAlwaysRedacted(t *testing.T) {
	e := newEnv(t)
	sp := e.seedSecrets()
	check := func(got spec.Stream) {
		t.Helper()
		require.Len(t, got.Inputs, 2)
		assert.Equal(t, redact.URL(woInput), got.Inputs[0].URL)
		assert.Equal(t, "udp://239.0.0.1:5000", got.Inputs[1].URL)
		require.Len(t, got.Outputs.Push, 1)
		assert.Equal(t, redact.URL(woPush), got.Outputs.Push[0].URL)
		assert.Equal(t, redact.Mask, got.Outputs.SRTPassphrase)
	}
	for _, who := range [][]reqOpt{viewer, editor} {
		rec := e.do(http.MethodGet, "/api/v1/streams", who...)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.False(t, holdsWO(rec.Body.String()), rec.Body.String())
		var list struct {
			Streams []spec.Stream `json:"streams"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
		require.Len(t, list.Streams, 1)
		check(list.Streams[0])

		rec = e.do(http.MethodGet, "/api/v1/streams/cam", who...)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.False(t, holdsWO(rec.Body.String()), rec.Body.String())
		assert.Equal(t, sp.ETag(), rec.Header().Get("ETag"))
		var got spec.Stream
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		check(got)
	}

	// The PUT answer, of a document that holds the real values.
	doc, err := json.Marshal(sp)
	require.NoError(t, err)
	rec := e.do(http.MethodPut, "/api/v1/streams/cam", with(editor, body("application/json", doc))...)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, holdsWO(rec.Body.String()), rec.Body.String())
	var put putResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &put))
	check(put.Stream)
	assert.Equal(t, sp.ETag(), rec.Header().Get("ETag"))
	// The viewer cannot PUT, and is not answered a document.
	rec = e.do(http.MethodPut, "/api/v1/streams/cam", with(viewer, body("application/json", doc))...)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.False(t, holdsWO(rec.Body.String()))
	assert.False(t, holdsWO(e.logs.String()))
}

func TestSecretsQueryGone(t *testing.T) {
	e := newEnv(t)
	e.seedCam()
	e.seedSecrets()
	var gets []string
	for _, rt := range Routes() {
		if rt.Method == http.MethodGet && rt.Role != "" {
			gets = append(gets, strings.ReplaceAll(strings.ReplaceAll(rt.Path, "{name}", "cam"), "assets/cam", "assets/logo.png"))
		}
	}
	require.Contains(t, gets, pathConfig)
	require.Contains(t, gets, pathPprof)

	e.be.reset()
	for _, path := range gets {
		for _, query := range []string{"secrets=1", "secrets=1&x=1", "x=1&secrets=1", "secrets=0", "secrets", "secrets=1;x=1", "x=1;secrets", "%73ecrets=1"} {
			target := path + "?" + query
			for _, who := range [][]reqOpt{viewer, editor} {
				rec := e.do(http.MethodGet, target, who...)
				require.Equal(t, http.StatusBadRequest, rec.Code, target)
				doc := errOf(t, rec)
				assert.Equal(t, codeBadRequest, doc.Code)
				assert.Equal(t, "?secrets=1 is gone: secrets are write-only. "+
					"A response shows xxxxx where a secret is stored, and a write that sends xxxxx back keeps it", doc.Message)
			}
			rec := e.do(http.MethodGet, target)
			assert.Equal(t, http.StatusUnauthorized, rec.Code, target)
		}
	}
	// A write that asks is refused alike.
	rec := e.do(http.MethodPut, "/api/v1/streams/cam?secrets=1", with(editor, jsonBody(camSpec))...)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, e.be.callList(), "the backend is never asked")

	// Other keys are ignored, as before, and the bodies are redacted.
	for _, query := range []string{"x=1", "secret=1", "Secrets=1", "secretsx=1"} {
		for _, path := range []string{pathStreams, "/api/v1/streams/cam", "/api/v1/version"} {
			for _, who := range [][]reqOpt{viewer, editor} {
				rec := e.do(http.MethodGet, path+"?"+query, who...)
				require.Equal(t, http.StatusOK, rec.Code, path+"?"+query)
				assert.False(t, holdsWO(rec.Body.String()), rec.Body.String())
			}
		}
		rec := e.do(http.MethodGet, pathConfig+"?"+query, viewer...)
		assert.Equal(t, http.StatusForbidden, rec.Code, "the role's 403, as before")
		rec = e.do(http.MethodGet, pathPprof+"?"+query, viewer...)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	}
}

func TestPutStreamPassesMasks(t *testing.T) {
	e := newEnv(t)
	sp := e.seedSecrets()
	rec := e.do(http.MethodGet, "/api/v1/streams/cam", editor...)
	require.Equal(t, http.StatusOK, rec.Code)
	masked := bytes.Clone(rec.Body.Bytes())
	var sent spec.Stream
	require.NoError(t, json.Unmarshal(masked, &sent))
	sent.Name = "cam"

	e.be.reset()
	rec = e.do(http.MethodPut, "/api/v1/streams/cam", with(editor, body("application/json", masked))...)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, e.be.gotStreams, 1)
	assert.Equal(t, sent, e.be.gotStreams[0], "the document reaches the backend as sent, masks and all")
	assert.Equal(t, sp.ETag(), rec.Header().Get("ETag"), "the stored document's")
	assert.NotEqual(t, sent.ETag(), rec.Header().Get("ETag"), "not the sent one's")
	assert.False(t, holdsWO(rec.Body.String()))
	assert.Equal(t, sp, e.be.streams["cam"], "the secrets are kept")

	// Only the push masked, the input changed: the push resolves.
	doc := `{"inputs":[{"url":"udp://239.0.0.1:5000"}],"outputs":{"push":[{"url":"` + redact.URL(woPush) + `"}]}}`
	rec = e.do(http.MethodPut, "/api/v1/streams/cam", with(editor, jsonBody(doc))...)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, woPush, e.be.streams["cam"].Outputs.Push[0].URL)

	// Under another name there is nothing the mask stands for: the
	// backend's config.Errors is 422 invalid_config with its texts.
	e.be.reset()
	rec = e.do(http.MethodPut, "/api/v1/streams/new", with(editor, jsonBody(doc))...)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	got := errOf(t, rec)
	assert.Equal(t, codeInvalidConf, got.Code)
	assert.Equal(t, []string{"spec: validate: " + spec.MaskRefusal("outputs.push[0].url")}, got.Errors)
	assert.Empty(t, rec.Header().Get("ETag"))
	assert.Equal(t, []string{"PutStream new"}, e.be.callList())

	e.be.putErr = config.Errors{errors.New("spec: validate: inputs[0].url: a"), errors.New("spec: validate: inputs[1].url: b")}
	rec = e.do(http.MethodPut, "/api/v1/streams/cam", with(editor, body("application/json", masked))...)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	got = errOf(t, rec)
	assert.Equal(t, codeInvalidConf, got.Code)
	assert.Equal(t, []string{"spec: validate: inputs[0].url: a", "spec: validate: inputs[1].url: b"}, got.Errors)
	assert.False(t, holdsWO(e.logs.String()))
}
