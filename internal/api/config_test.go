package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/config"
	"github.com/ntt0601zcoder/open-streamer/internal/stream"
)

const configText = "# the config\r\nversion: 1 # one\n\nusers:\n  - name: ops\n    role: editor\n" +
	"    password_hash: pbkdf2-sha256$600000$c2FsdHNhbHRzYWx0c2FsdA$a2V5\nstreams:\n  - name: cam\n" +
	"    inputs:\n      - url: rtmp://h/live/secretkey\n\n"

func yamlBody(s string) reqOpt { return body("application/yaml", []byte(s)) }

func TestConfigGet(t *testing.T) {
	e := newEnv(t)
	e.be.text, e.be.etag = []byte(configText), config.Hash([]byte(configText))

	rec := e.do(http.MethodGet, "/api/v1/config", editor...)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, configText, rec.Body.String(), "verbatim")
	assert.Equal(t, "application/yaml", rec.Header().Get("Content-Type"))
	assert.Equal(t, e.be.etag, rec.Header().Get("ETag"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	rec = e.do(http.MethodGet, "/api/v1/config", with(editor, hdr("If-None-Match", e.be.etag))...)
	assert.Equal(t, http.StatusNotModified, rec.Code)
	assert.Empty(t, rec.Body.String())

	// The viewer gets no byte of it, on any spelling of the path.
	e.be.reset()
	for _, target := range []string{"/api/v1/config", "/api/v1/config?secrets=1", "/api/v1//config", "/api/v1/config/../config"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			rec = e.do(method, target, viewer...)
			assert.Equal(t, http.StatusForbidden, rec.Code, target)
			for _, s := range []string{"secretkey", "pbkdf2", "version"} {
				assert.NotContains(t, rec.Body.String(), s)
			}
			assert.Empty(t, rec.Header().Get("ETag"))
		}
	}
	assert.Empty(t, e.be.callList())
	assert.NotContains(t, e.logs.String(), "secretkey")
	assert.NotContains(t, e.logs.String(), "pbkdf2")
}

func TestConfigPut(t *testing.T) {
	e := newEnv(t)
	e.be.result = ApplyResult{
		Streams:         []stream.Outcome{{Name: "cam", Applied: "rebuild"}},
		RestartRequired: []string{"server.rtmp.listen"},
		ETag:            `"new"`,
	}
	rec := e.do(http.MethodPut, "/api/v1/config", with(editor, yamlBody(configText), hdr("If-Match", `"old"`))...)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, configText, string(e.be.gotConfig), "byte for byte")
	assert.Equal(t, []string{`"old"`}, e.be.ifMatch)
	assert.Equal(t, `"new"`, rec.Header().Get("ETag"))
	var got ApplyResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, e.be.result, got)

	// Empty lists are [], never null.
	e.be.result = ApplyResult{ETag: `"new"`}
	rec = e.do(http.MethodPut, "/api/v1/config", with(editor, yamlBody(configText), hdr("If-Match", `"old"`))...)
	assert.JSONEq(t, `{"streams":[],"restart_required":[],"etag":"\"new\""}`, rec.Body.String())

	e.be.reset()
	refused := func(status int, code string, opts ...reqOpt) {
		t.Helper()
		rec := e.do(http.MethodPut, "/api/v1/config", opts...)
		require.Equal(t, status, rec.Code, rec.Body.String())
		assert.Equal(t, code, errOf(t, rec).Code)
		assert.Empty(t, e.be.callList(), "the backend is not called")
	}
	ifMatch := hdr("If-Match", `"old"`)
	refused(http.StatusPreconditionRequired, codePrecondReq, with(editor, yamlBody(configText))...)
	refused(http.StatusUnsupportedMediaType, codeMediaType, with(editor, body("text/plain", []byte(configText)), ifMatch)...)
	refused(http.StatusUnsupportedMediaType, codeMediaType, with(editor, body("application/json", []byte(configText)), ifMatch)...)
	refused(http.StatusUnsupportedMediaType, codeMediaType, with(editor, body("", []byte(configText)), ifMatch)...)
	big := strings.Repeat("#", config.MaxBytes+1)
	refused(http.StatusRequestEntityTooLarge, codeTooLarge, with(editor, yamlBody(big), ifMatch)...)
	refused(http.StatusRequestEntityTooLarge, codeTooLarge,
		with(editor, yamlBody(big), ifMatch, func(r *http.Request) { r.ContentLength = -1 })...)
	refused(http.StatusForbidden, codeForbidden, with(viewer, yamlBody(configText), ifMatch)...)

	// Exactly MaxBytes is accepted.
	rec = e.do(http.MethodPut, "/api/v1/config", with(editor, yamlBody(big[1:]), ifMatch)...)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, e.be.gotConfig, config.MaxBytes)

	// The backend's refusals.
	for _, c := range []struct {
		err    error
		status int
		code   string
		n      int
	}{
		{ErrPrecondition, http.StatusPreconditionFailed, codePrecondition, 0},
		{config.ErrStale, http.StatusConflict, codeChanged, 0},
		{&config.SyntaxError{Err: errors.New("yaml: cannot unmarshal !!str `hunter2` into int")}, http.StatusBadRequest, codeBadRequest, 0},
		{config.Errors{errors.New("a"), errors.New("b"), errors.New("c")}, http.StatusUnprocessableEntity, codeInvalidConf, 3},
		{errors.New("rename: disk full"), http.StatusInternalServerError, codeInternal, 0},
	} {
		e.be.configErr = c.err
		rec = e.do(http.MethodPut, "/api/v1/config", with(editor, yamlBody(configText), ifMatch)...)
		require.Equal(t, c.status, rec.Code)
		doc := errOf(t, rec)
		assert.Equal(t, c.code, doc.Code)
		assert.Len(t, doc.Errors, c.n)
		assert.Empty(t, rec.Header().Get("ETag"), "an error has no ETag")
		assert.NotContains(t, rec.Body.String(), "hunter2")
	}
	// The body is never logged.
	assert.NotContains(t, e.logs.String(), "secretkey")
	assert.NotContains(t, e.logs.String(), "pbkdf2")
}

func TestReloadAndStatus(t *testing.T) {
	e := newEnv(t)
	e.be.result = ApplyResult{Streams: []stream.Outcome{{Name: "cam", Applied: "stopped"}}, RestartRequired: []string{}, ETag: `"e"`}
	rec := e.do(http.MethodPost, "/api/v1/reload", editor...)
	require.Equal(t, http.StatusOK, rec.Code)
	var got ApplyResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, e.be.result, got)
	assert.Equal(t, http.StatusForbidden, e.do(http.MethodPost, "/api/v1/reload", viewer...).Code)
	assert.Equal(t, []string{"Reload"}, e.be.callList())

	e.be.reloadErr = &config.SyntaxError{Err: errors.New("yaml: line 1: did not find expected key")}
	rec = e.do(http.MethodPost, "/api/v1/reload", editor...)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, errOf(t, rec).Message, "did not find expected key")

	at := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	e.be.cstatus = ConfigStatus{
		ETag: `"e"`, LoadedAt: at, Stale: true,
		LoadErrors:   []string{`streams[2] (door): cannot open rtsp://u:hunter2@h/a`},
		LastReloadAt: &at, LastReloadError: "config: yaml: line 3",
		RestartRequired: []string{"server.api.listen"},
	}
	for _, who := range [][]reqOpt{viewer, editor} {
		rec = e.do(http.MethodGet, "/api/v1/config/status", who...)
		require.Equal(t, http.StatusOK, rec.Code)
		var st ConfigStatus
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
		want := e.be.cstatus
		want.LoadErrors = []string{`streams[2] (door): cannot open rtsp://u:xxxxx@h/a`}
		want.LoadErrors[0] = st.LoadErrors[0]
		assert.Equal(t, want, st)
		assert.NotContains(t, rec.Body.String(), "hunter2", "scrubbed for the viewer")
		assert.Contains(t, st.LoadErrors[0], "streams[2] (door)")
	}
	assert.Equal(t, `streams[2] (door): cannot open rtsp://u:hunter2@h/a`, e.be.cstatus.LoadErrors[0], "the backend's slice is not written to")

	e.be.cstatus = ConfigStatus{ETag: `"e"`, LoadedAt: at}
	rec = e.do(http.MethodGet, "/api/v1/config/status", viewer...)
	assert.JSONEq(t, `{"etag":"\"e\"","loaded_at":"2026-11-26T10:00:00Z","stale":false,"load_errors":[],"restart_required":[]}`,
		rec.Body.String())
}
