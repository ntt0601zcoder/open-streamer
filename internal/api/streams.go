package api

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"slices"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/spec"
	"github.com/ntt0601zcoder/open-streamer/internal/stream"
)

// view is sp as a response shows it: redacted, unless an editor asked for
// the secrets.
func view(r *http.Request, sp spec.Stream) spec.Stream {
	if showSecrets(r) {
		return sp
	}
	return sp.Redacted()
}

// streamName returns the path's stream name; a name no stream can have is
// answered 404, and the Backend is not asked.
func streamName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if !spec.ValidName(name) {
		writeError(w, http.StatusNotFound, codeNotFound, "no such stream", nil)
		return "", false
	}
	return name, true
}

// isMedia reports whether the request's Content-Type is the media type
// want, whatever its parameters.
func isMedia(r *http.Request, want string) bool {
	got, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && got == want
}

func (a *API) listStreams(w http.ResponseWriter, r *http.Request) {
	list := a.opt.Backend.List()
	out := make([]spec.Stream, len(list))
	for i, sp := range list {
		out[i] = view(r, sp)
	}
	slices.SortFunc(out, func(x, y spec.Stream) int { return cmp.Compare(x.Name, y.Name) })
	writeJSON(w, http.StatusOK, map[string][]spec.Stream{"streams": out})
}

func (a *API) getStream(w http.ResponseWriter, r *http.Request) {
	name, ok := streamName(w, r)
	if !ok {
		return
	}
	sp, ok := a.opt.Backend.Get(name)
	if !ok {
		writeError(w, http.StatusNotFound, codeNotFound, "no such stream", nil)
		return
	}
	w.Header().Set("ETag", sp.ETag())
	writeJSON(w, http.StatusOK, view(r, sp))
}

// putResult is the answer to a stream PUT.
type putResult struct {
	Stream  spec.Stream `json:"stream"`
	Applied string      `json:"applied"`
}

func (a *API) putStream(w http.ResponseWriter, r *http.Request) {
	name, ok := streamName(w, r)
	if !ok {
		return
	}
	if !isMedia(r, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, codeMediaType, "the body must be application/json", nil)
		return
	}
	if r.ContentLength > spec.MaxSpecBytes {
		tooLarge(w, spec.MaxSpecBytes)
		return
	}
	// One byte more than Decode accepts, so a longer body is found too
	// long there and not cut into a document that happens to parse.
	body := http.MaxBytesReader(w, r.Body, spec.MaxSpecBytes+1)
	raw, err := readAll(body, spec.MaxSpecBytes)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			tooLarge(w, spec.MaxSpecBytes)
			return
		}
		writeError(w, http.StatusBadRequest, codeBadRequest, "the body could not be read", nil)
		return
	}
	sp, err := spec.Decode(bytes.NewReader(raw))
	if err != nil {
		// Decode's errors come from encoding/json, which names fields and
		// types and never quotes a string value.
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error(), nil)
		return
	}
	switch sp.Name {
	case "":
		sp.Name = name
	case name:
	default:
		writeError(w, http.StatusBadRequest, codeBadRequest, "the body's name is not the path's", nil)
		return
	}
	out, err := a.opt.Backend.PutStream(r.Context(), sp, r.Header.Get("If-Match"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if out.Applied == "created" {
		status = http.StatusCreated
	}
	w.Header().Set("ETag", sp.ETag())
	writeJSON(w, status, putResult{Stream: view(r, sp), Applied: out.Applied})
}

func (a *API) deleteStream(w http.ResponseWriter, r *http.Request) {
	name, ok := streamName(w, r)
	if !ok {
		return
	}
	if err := a.opt.Backend.DeleteStream(r.Context(), name, r.Header.Get("If-Match")); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// statusDoc is stream.Status with a since that is null between
// generations, where the manager reports the zero time.
type statusDoc struct {
	stream.Status
	Since *time.Time `json:"since"`
}

func (a *API) streamStatus(w http.ResponseWriter, r *http.Request) {
	name, ok := streamName(w, r)
	if !ok {
		return
	}
	st, ok := a.opt.Backend.Status(name)
	if !ok {
		writeError(w, http.StatusNotFound, codeNotFound, "no such stream", nil)
		return
	}
	doc := statusDoc{Status: st}
	if !st.Since.IsZero() {
		doc.Since = &st.Since
	}
	writeJSON(w, http.StatusOK, doc)
}

// thumbnail serves the latest shot, with its arrival time as Last-Modified
// and "<epoch>-<pts>" as ETag; 404 until the first shot.
func (a *API) thumbnail(w http.ResponseWriter, r *http.Request) {
	name, ok := streamName(w, r)
	if !ok {
		return
	}
	shot, at, ok := a.opt.Backend.Shot(name)
	if !ok {
		writeError(w, http.StatusNotFound, codeNotFound, "no thumbnail", nil)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", fmt.Sprintf(`"%d-%d"`, shot.Epoch, shot.PTS))
	// ServeContent adds Last-Modified and answers If-None-Match with a 304.
	http.ServeContent(w, r, "", at, bytes.NewReader(shot.JPEG))
}
