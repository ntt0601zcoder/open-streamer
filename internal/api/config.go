package api

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/config"
	"github.com/ntt0601zcoder/open-streamer/internal/logging"
	"github.com/ntt0601zcoder/open-streamer/internal/stream"
)

var errTooLarge = errors.New("api: the body is too large")

// readAll reads a body of at most limit bytes; a longer one is errTooLarge.
func readAll(body io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(body, limit+1))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig), err == nil && int64(len(b)) > limit:
		return nil, errTooLarge
	case err != nil:
		return nil, fmt.Errorf("api: read the body: %w", err)
	}
	return b, nil
}

func tooLarge(w http.ResponseWriter, limit int64) {
	writeError(w, http.StatusRequestEntityTooLarge, codeTooLarge,
		fmt.Sprintf("the body is larger than %d bytes", limit), nil)
}

// getConfig serves the config text verbatim. It holds stream keys and
// password hashes, which is why the route is an editor's.
func (a *API) getConfig(w http.ResponseWriter, r *http.Request) {
	text, etag := a.opt.Backend.ConfigText()
	h := w.Header()
	h.Set("Content-Type", "application/yaml")
	h.Set("ETag", etag)
	// ServeContent answers If-None-Match with a 304.
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(text))
}

func (a *API) putConfig(w http.ResponseWriter, r *http.Request) {
	if !isMedia(r, "application/yaml") {
		writeError(w, http.StatusUnsupportedMediaType, codeMediaType, "the body must be application/yaml", nil)
		return
	}
	ifMatch := r.Header.Get("If-Match")
	if ifMatch == "" {
		a.fail(w, r, ErrPreconditionRequired)
		return
	}
	if r.ContentLength > config.MaxBytes {
		tooLarge(w, config.MaxBytes)
		return
	}
	text, err := readAll(http.MaxBytesReader(w, r.Body, config.MaxBytes+1), config.MaxBytes)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			tooLarge(w, config.MaxBytes)
			return
		}
		writeError(w, http.StatusBadRequest, codeBadRequest, "the body could not be read", nil)
		return
	}
	res, err := a.opt.Backend.PutConfig(r.Context(), text, ifMatch)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if res.ETag != "" {
		w.Header().Set("ETag", res.ETag)
	}
	writeJSON(w, http.StatusOK, applied(res))
}

func (a *API) reload(w http.ResponseWriter, r *http.Request) {
	res, err := a.opt.Backend.Reload(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, applied(res))
}

// applied is res with its lists as [] when they are empty.
func applied(res ApplyResult) ApplyResult {
	if res.Streams == nil {
		res.Streams = []stream.Outcome{}
	}
	if res.RestartRequired == nil {
		res.RestartRequired = []string{}
	}
	return res
}

func (a *API) configStatus(w http.ResponseWriter, _ *http.Request) {
	st := a.opt.Backend.ConfigStatus()
	// The texts come from load and reload errors: scrubbed again here,
	// since a viewer reads them.
	st.LoadErrors = slices.Clone(st.LoadErrors)
	for i, s := range st.LoadErrors {
		st.LoadErrors[i] = logging.Scrub(s)
	}
	if st.LoadErrors == nil {
		st.LoadErrors = []string{}
	}
	if st.RestartRequired == nil {
		st.RestartRequired = []string{}
	}
	st.LastReloadError = logging.Scrub(st.LastReloadError)
	if st.Certificate != nil {
		c := *st.Certificate // the backend's value is not written to
		if c.Names == nil {
			c.Names = []string{}
		}
		st.Certificate = &c
	}
	writeJSON(w, http.StatusOK, st)
}
