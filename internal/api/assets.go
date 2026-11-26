package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/asset"
	"github.com/ntt0601zcoder/open-streamer/internal/stream"
)

// Asset is the asset document.
type Asset struct {
	Name     string    `json:"name"`
	Kind     string    `json:"kind"` // "image", "font"
	Type     string    `json:"type"` // the media type
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	ETag     string    `json:"etag"`
	UsedBy   []string  `json:"used_by"` // [] when no stream names it
}

// AssetPut is the answer to an upload: the asset, and what each stream that names it did with the new bytes.
type AssetPut struct {
	Asset
	Refreshed []stream.Refreshed `json:"refreshed"` // [] when no stream with a supervisor names it
	Retried   []stream.Outcome   `json:"retried"`   // the streams left stopped that name it, tried again; [] when none
}

// ErrSkippedEntries is DeleteAsset's error while the config holds an entry the start skipped.
var ErrSkippedEntries = errors.New("api: the config holds entries that were skipped")

// AssetInUseError is DeleteAsset's error while a stream names the asset; Streams are their names.
type AssetInUseError struct{ Streams []string }

// Error says how many streams name the asset; the names are in Streams.
func (e *AssetInUseError) Error() string {
	return fmt.Sprintf("api: the asset is in use by %d streams", len(e.Streams))
}

// The asset paths.
const (
	pathAssets = "/api/v1/assets"
	pathAsset  = "/api/v1/assets/{name}"
)

// uploadSlots is how many asset uploads run at once (R-E6): each holds up
// to its kind's limit in memory while it is checked.
const uploadSlots = 2

// timing holds the asset routes' times (R-E6). It is a seam: the tests
// scale it down.
type timing struct {
	base      time.Duration // the deadline of an empty body
	per256KiB time.Duration // added per 256 KiB, or part of it, of the body
	slotWait  time.Duration // the longest an upload waits for a slot
	writeMore time.Duration // the write deadline of an upload, beyond its read deadline
}

var defaultTiming = timing{
	base:      30 * time.Second,
	per256KiB: time.Second,
	slotWait:  30 * time.Second,
	writeMore: 60 * time.Second,
}

// deadline is the time a body of n bytes may take to cross the wire: 30 s
// and 1 s per 256 KiB, a part of 256 KiB counted whole.
func (t timing) deadline(n int64) time.Duration {
	const chunk = 256 << 10
	chunks := (max(n, 0) + chunk - 1) / chunk
	return t.base + time.Duration(chunks)*t.per256KiB
}

// assetName returns the path's asset name and its kind; a name no asset can
// have is answered 404, and the Backend is not asked.
func assetName(w http.ResponseWriter, r *http.Request) (string, asset.Kind, bool) {
	name := r.PathValue("name")
	kind, ok := asset.KindOf(name)
	if !ok {
		writeError(w, http.StatusNotFound, codeNotFound, "no such asset", nil)
		return "", "", false
	}
	return name, kind, true
}

// assetDoc is a with its lists as [] when they are empty.
func assetDoc(a Asset) Asset {
	if a.UsedBy == nil {
		a.UsedBy = []string{}
	}
	return a
}

func (a *API) listAssets(w http.ResponseWriter, r *http.Request) {
	list, err := a.opt.Backend.Assets()
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]Asset, len(list))
	for i, as := range list {
		out[i] = assetDoc(as)
	}
	slices.SortFunc(out, func(x, y Asset) int { return cmp.Compare(x.Name, y.Name) })
	writeJSON(w, http.StatusOK, map[string][]Asset{"assets": out})
}

// getAsset streams the asset's file: the write deadline grows with its
// size, and http.ServeContent answers If-None-Match with a 304.
func (a *API) getAsset(w http.ResponseWriter, r *http.Request) {
	name, _, ok := assetName(w, r)
	if !ok {
		return
	}
	inf, f, err := a.opt.Backend.Asset(name)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	defer func() { _ = f.Close() }() // read only
	// A writer without deadlines (a test's recorder) has nothing to extend.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(a.timing.deadline(inf.Size)))
	h := w.Header()
	h.Set("ETag", inf.ETag)
	h.Set("Content-Type", inf.Type)
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`) // the name is [a-z0-9_.-]
	h.Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "", inf.Modified, f)
}

// takeSlot waits for an upload slot: at most slotWait, and never past the
// request's context. On false it has answered.
func (a *API) takeSlot(ctx context.Context, w http.ResponseWriter) bool {
	t := time.NewTimer(a.timing.slotWait)
	defer t.Stop()
	select {
	case a.slots <- struct{}{}:
		return true
	case <-ctx.Done():
		writeError(w, http.StatusServiceUnavailable, codeUnavailable, "the request was cancelled", nil)
	case <-t.C:
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, codeUnavailable, "too many uploads are running", nil)
	}
	return false
}

func (a *API) putAsset(w http.ResponseWriter, r *http.Request) {
	name, kind, ok := assetName(w, r)
	if !ok {
		return
	}
	if !isMedia(r, "application/octet-stream") {
		writeError(w, http.StatusUnsupportedMediaType, codeMediaType, "the body must be application/octet-stream", nil)
		return
	}
	var noneMatch bool
	switch v := r.Header.Get("If-None-Match"); v {
	case "":
	case "*":
		noneMatch = true
	default:
		writeError(w, http.StatusBadRequest, codeBadRequest, "the If-None-Match header of an upload can only be *", nil)
		return
	}
	limit := asset.MaxBytes(kind)
	if r.ContentLength > limit {
		tooLarge(w, limit)
		return
	}
	if !a.takeSlot(r.Context(), w) {
		return
	}
	defer func() { <-a.slots }()
	// The deadlines start once the slot is held, before a byte is read:
	// the time spent waiting is not taken from the body's. A writer
	// without deadlines (a test's recorder) has nothing to extend.
	rc := http.NewResponseController(w)
	readBy := time.Now().Add(a.timing.deadline(limit))
	_ = rc.SetReadDeadline(readBy)
	_ = rc.SetWriteDeadline(readBy.Add(a.timing.writeMore))
	// One byte more than the limit, so that the backend finds a longer
	// body too large and does not store a cut one.
	body := http.MaxBytesReader(w, r.Body, limit+1)
	res, created, err := a.opt.Backend.PutAsset(r.Context(), name, body, r.Header.Get("If-Match"), noneMatch)
	if err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.Is(err, asset.ErrTooLarge), errors.As(err, &tooBig):
			tooLarge(w, limit)
		default:
			a.fail(w, r, err)
		}
		return
	}
	res.Asset = assetDoc(res.Asset)
	if res.Refreshed == nil {
		res.Refreshed = []stream.Refreshed{}
	}
	if res.Retried == nil {
		res.Retried = []stream.Outcome{}
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	w.Header().Set("ETag", res.ETag)
	writeJSON(w, status, res)
}

func (a *API) deleteAsset(w http.ResponseWriter, r *http.Request) {
	name, _, ok := assetName(w, r)
	if !ok {
		return
	}
	if err := a.opt.Backend.DeleteAsset(r.Context(), name, r.Header.Get("If-Match")); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
