package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ntt0601zcoder/open-streamer/internal/config"
	"github.com/ntt0601zcoder/open-streamer/internal/logging"
	"github.com/ntt0601zcoder/open-streamer/internal/stream"
)

// The codes of the error document.
const (
	codeBadRequest   = "bad_request"
	codeUnauthorized = "unauthorized"
	codeForbidden    = "forbidden"
	codeNotFound     = "not_found"
	codeConflict     = "conflict"
	codeChanged      = "config_changed"
	codeUnreadable   = "config_unreadable"
	codePrecondition = "precondition_failed"
	codeTooLarge     = "too_large"
	codeMediaType    = "unsupported_media_type"
	codeInvalidConf  = "invalid_config"
	codeInvalidSpec  = "invalid_spec"
	codePrecondReq   = "precondition_required"
	codeTooMany      = "too_many_attempts"
	codeWriteFailed  = "config_write_failed"
	codeInternal     = "internal"
	codeUnavailable  = "unavailable"

	msgInternal = "internal error"
)

// errorBody is the error document: {"error":{"code","message","errors"}}.
type errorBody struct {
	Error errorDoc `json:"error"`
}

type errorDoc struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Errors  []string `json:"errors,omitempty"`
}

// writeJSON writes v as the JSON body of a response with the status code.
func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		// Only documents of strings, numbers, booleans and times are
		// written, which always encode.
		code, b = http.StatusInternalServerError, []byte(`{"error":{"code":"internal","message":"internal error"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(append(b, '\n'))
}

// writeError writes the error document; every text is scrubbed.
func writeError(w http.ResponseWriter, status int, code, message string, list []string) {
	for i, s := range list {
		list[i] = logging.Scrub(s)
	}
	writeJSON(w, status, errorBody{Error: errorDoc{Code: code, Message: logging.Scrub(message), Errors: list}})
}

// fail answers with the row of the error table err belongs to.
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	var (
		syntax *config.SyntaxError
		list   config.Errors
	)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, config.ErrNoStream):
		writeError(w, http.StatusNotFound, codeNotFound, "not found", nil)
	case errors.Is(err, ErrPrecondition):
		writeError(w, http.StatusPreconditionFailed, codePrecondition, "the If-Match header does not match", nil)
	case errors.Is(err, ErrPreconditionRequired):
		writeError(w, http.StatusPreconditionRequired, codePrecondReq, "the If-Match header is required", nil)
	case errors.Is(err, config.ErrStale):
		writeError(w, http.StatusConflict, codeChanged, "the config file changed on disk; reload first", nil)
	case errors.Is(err, config.ErrUnreadable):
		// A reload of a file that cannot be read, or whose mode is not
		// private: nothing changed, and the operator must be told why. Not
		// logged here: the reload logged it.
		writeError(w, http.StatusConflict, codeUnreadable, "the config file cannot be read: "+err.Error(), nil)
	case errors.Is(err, config.ErrWrite):
		// The edit was judged and then could not be written (a read-only
		// directory, a full disk): nothing was applied, and the operator
		// can act on the reason. A failure after the rename leaves the new
		// text on disk, and then the next write says config_changed.
		reason := logging.Scrub(err.Error())
		a.log.Error("api: the config file was not written", "error", reason)
		writeError(w, http.StatusInternalServerError, codeWriteFailed, "the config file could not be written: "+reason, nil)
	case errors.As(err, &syntax):
		// Its Error hides the values the YAML library quoted.
		writeError(w, http.StatusBadRequest, codeBadRequest, syntax.Error(), nil)
	case errors.As(err, &list):
		texts := make([]string, len(list))
		for i, e := range list {
			texts[i] = e.Error()
		}
		writeError(w, http.StatusUnprocessableEntity, codeInvalidConf, "the config is invalid", texts)
	case errors.Is(err, stream.ErrInvalid):
		// It takes precedence: every joined error is listed, the
		// conflicts included.
		writeError(w, http.StatusUnprocessableEntity, codeInvalidSpec, "the stream cannot run", texts(split(err)))
	case errors.Is(err, stream.ErrConflict):
		writeError(w, http.StatusConflict, codeConflict, "the stream conflicts with another", texts(split(err)))
	case errors.Is(err, stream.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, codeUnavailable, "the server is stopping", nil)
	case r.Context().Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)):
		// The client went away while the edit waited; nothing failed.
		writeError(w, http.StatusServiceUnavailable, codeUnavailable, "the request was cancelled", nil)
	default:
		a.log.Error("api: internal error", "error", logging.Scrub(err.Error()))
		writeError(w, http.StatusInternalServerError, codeInternal, msgInternal, nil)
	}
}

func texts(errs []error) []string {
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = e.Error()
	}
	return out
}

// isKind reports whether err is an edit error of the stream manager.
func isKind(err error) bool {
	return errors.Is(err, stream.ErrInvalid) || errors.Is(err, stream.ErrConflict)
}

// bare reports whether err is one of the two sentinels itself.
func bare(err error) bool {
	return err == stream.ErrInvalid || err == stream.ErrConflict //nolint:errorlint // identity is the point
}

// split returns the single reasons of a refused edit, which the manager
// joins into one error. A reason is an error that wraps a sentinel directly
// (the manager's own type unwraps to its kind and its cause); everything
// above the reasons, a join or a wrap with a prefix, is taken apart.
func split(err error) []error {
	switch e := err.(type) { //nolint:errorlint // the tree is walked by hand
	case interface{ Unwrap() []error }:
		parts := e.Unwrap()
		for _, p := range parts {
			if p == nil || bare(p) || !isKind(p) {
				return []error{err}
			}
		}
		if len(parts) == 0 {
			return []error{err}
		}
		var out []error
		for _, p := range parts {
			out = append(out, split(p)...)
		}
		return out
	case interface{ Unwrap() error }:
		if in := e.Unwrap(); in != nil && !bare(in) && isKind(in) {
			return split(in)
		}
	}
	return []error{err}
}
