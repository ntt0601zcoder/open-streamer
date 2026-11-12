package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/pkg/hls"
)

const (
	// maxPlaylistBytes caps a playlist body.
	maxPlaylistBytes = 4 << 20
	// maxSegmentBytes caps a segment body, which is held in memory whole.
	maxSegmentBytes = 64 << 20
	// liveStartSegments is how far from the end a live playlist is joined
	// (RFC 8216 §6.3.3).
	liveStartSegments = 3
)

var (
	errHLSEncrypted    = errors.New("encrypted HLS is not supported")
	errHLSMultivariant = errors.New("multivariant HLS playlists are not supported yet")
	errHLSFMP4         = errors.New("fMP4 HLS segments are not supported yet")
	errHLSClosed       = errors.New("closed")
)

// runEnd says why a run, the segments fed to one demuxer, ended.
type runEnd uint8

const (
	runOpen   runEnd = iota // still feeding
	runBreak                // a timeline break: ErrDiscontinuity, then a new run
	runEnded                // the end of an EXT-X-ENDLIST playlist
	runFailed               // segFeed.fail holds the error
)

// hlsReader pulls a media playlist of MPEG-TS segments. The segments between
// two timeline breaks are one run: they feed one demuxer in order, so a PES
// may span segments. Every break (EXT-X-DISCONTINUITY, a skipped segment, a
// reader that fell behind the live window) ends the run with
// ErrDiscontinuity, and the next run has a new demuxer and timestamp
// decoder; the config cache is shared, so equal configs keep their pointer.
//
// All fetching happens in the Read goroutine, as the demuxer reads.
type hlsReader struct {
	rawURL string // the input URL, reloaded as given
	client *http.Client
	ctx    context.Context // cancelled by Close
	cancel context.CancelFunc
	now    func() time.Time
	spec   Spec
	target time.Duration // EXT-X-TARGETDURATION of the first load

	// Read goroutine only.
	pl      hls.MediaPlaylist
	base    *url.URL  // where the playlist was last fetched from, after redirects
	next    uint64    // media sequence number of the next segment to fetch
	loaded  time.Time // when the last playlist load began
	grew    bool      // the last load listed a segment past the previous end
	arrival time.Time // when the last segment finished downloading
	cache   media.ConfigCache
	feed    *segFeed // of the current run

	segments, segmentErrors, skipped  atomic.Uint64
	fellBehind, reloads, reloadErrors atomic.Uint64
	discontinuities                   atomic.Uint64

	mu     sync.Mutex // guards the fields below
	run    *tsReader
	kinds  []media.Kind      // of the latest run, kept between runs
	done   map[string]uint64 // demuxer counters of finished runs
	closed bool
}

// openHLS starts a reader on a playlist fetched from final, whose load began
// at loaded, and reads up to the first PMT. Every request runs under rctx,
// which the reader owns and Close cancels (cancel); openHTTP cancels it when
// the open ctx ends first.
func openHLS(rctx context.Context, cancel context.CancelFunc, client *http.Client, rawURL string,
	body []byte, final *url.URL, loaded time.Time, env Env,
) (*hlsReader, error) {
	r := &hlsReader{
		rawURL: rawURL, client: client, ctx: rctx, cancel: cancel, now: env.now(),
		loaded: loaded, grew: true, done: map[string]uint64{},
	}
	if err := r.setPlaylist(body, final); err != nil {
		return nil, r.errf("open", err)
	}
	r.target = time.Duration(r.pl.TargetDuration) * time.Second
	r.spec = Spec{Pace: true, Live: true, StallTimeout: r.target * 3 / 2}
	r.next = r.pl.MediaSequence
	if !r.pl.Ended {
		r.next = r.liveStart()
	}
	r.arrival = r.now()
	if err := r.startRun(); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("playlist ended before the first PMT")
		}
		return nil, r.errf("open", err)
	}
	return r, nil
}

func (r *hlsReader) errf(op string, err error) error {
	return fmt.Errorf("ingest: hls: %s %s: %w", op, Redact(r.rawURL), err)
}

// setPlaylist parses a playlist fetched from final and makes it current.
func (r *hlsReader) setPlaylist(body []byte, final *url.URL) error {
	if bytes.Contains(body, []byte("#EXT-X-STREAM-INF")) {
		return errHLSMultivariant
	}
	pl, err := hls.ParseMedia(body)
	if err != nil {
		return fmt.Errorf("parse playlist: %w", err)
	}
	if pl.TargetDuration <= 0 {
		return errors.New("playlist has no EXT-X-TARGETDURATION")
	}
	for _, s := range pl.Segments {
		if s.Key.Method != "" && s.Key.Method != "NONE" {
			return errHLSEncrypted
		}
		if s.Map != "" {
			return errHLSFMP4
		}
	}
	r.pl, r.base = pl, final
	return nil
}

// liveStart returns the media sequence number liveStartSegments from the
// end of the current playlist.
func (r *hlsReader) liveStart() uint64 {
	n := uint64(len(r.pl.Segments))
	return r.pl.MediaSequence + n - min(n, liveStartSegments)
}

// startRun opens a demuxer on the next run. It returns io.EOF at the end of
// the playlist. A run that breaks before its first PMT is dropped.
func (r *hlsReader) startRun() error {
	for {
		feed := &segFeed{r: r}
		r.feed = feed
		run, err := newTSReader(feed, func() time.Time { return r.arrival }, r.spec, &r.cache)
		if err != nil {
			switch feed.end {
			case runOpen:
				return err
			case runBreak:
				r.discontinuities.Add(1)
				continue
			case runEnded:
				return io.EOF
			case runFailed:
				return feed.fail
			}
			return err
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.closed {
			_ = run.Close()
			return errHLSClosed
		}
		r.run = run
		r.kinds = run.Kinds()
		return nil
	}
}

// endRun retires the current run, keeping its counters.
func (r *hlsReader) endRun(run *tsReader) {
	r.mu.Lock()
	for k, v := range run.Stats() {
		r.done[k] += v
	}
	r.kinds = run.Kinds()
	r.run = nil
	r.mu.Unlock()
	_ = run.Close()
}

// Read implements Reader.
func (r *hlsReader) Read() (Frame, error) {
	r.mu.Lock()
	run, closed := r.run, r.closed
	r.mu.Unlock()
	if closed {
		return Frame{}, r.errf("read", errHLSClosed)
	}
	if run == nil {
		if err := r.startRun(); err != nil {
			if errors.Is(err, io.EOF) {
				return Frame{}, io.EOF
			}
			return Frame{}, r.errf("read", err)
		}
		return r.Read()
	}
	f, err := run.Read()
	if err == nil || errors.Is(err, ErrDiscontinuity) {
		return f, err
	}
	feed := r.feed
	r.endRun(run)
	switch feed.end {
	case runOpen:
	case runBreak:
		r.discontinuities.Add(1)
		return Frame{}, ErrDiscontinuity
	case runEnded:
		return Frame{}, io.EOF
	case runFailed:
		return Frame{}, r.errf("read", feed.fail)
	}
	return Frame{}, r.errf("read", err)
}

// nextSegment returns the next segment of the current run, or how the run
// ends. started reports whether the run has a segment already: a break
// before its first segment is no break.
func (r *hlsReader) nextSegment(started bool) ([]byte, runEnd, error) {
	for {
		if err := r.ctx.Err(); err != nil {
			return nil, runFailed, err
		}
		first := r.pl.MediaSequence
		end := first + uint64(len(r.pl.Segments))
		switch {
		case r.next < first || r.next > end:
			// The wanted segment left the window, or the playlist restarted
			// below it: rejoin the live edge.
			r.next = r.liveStart()
			r.fellBehind.Add(1)
			if started {
				return nil, runBreak, nil
			}
			continue
		case r.next < end:
			s := r.pl.Segments[r.next-first]
			if s.Discontinuity && started {
				return nil, runBreak, nil
			}
			data, err := r.fetchSegment(s)
			if err != nil {
				if r.ctx.Err() != nil {
					return nil, runFailed, r.ctx.Err()
				}
				r.next++
				r.skipped.Add(1)
				if started {
					return nil, runBreak, nil
				}
				continue
			}
			r.next++
			r.segments.Add(1)
			return data, runOpen, nil
		case r.pl.Ended:
			return nil, runEnded, nil
		}
		if err := r.reload(); err != nil {
			return nil, runFailed, err
		}
	}
}

// fetchSegment downloads a segment, retrying once, and stamps its arrival.
func (r *hlsReader) fetchSegment(s hls.Segment) ([]byte, error) {
	u, err := r.base.Parse(s.URI)
	if err != nil {
		r.segmentErrors.Add(1)
		return nil, fmt.Errorf("segment uri: %w", err)
	}
	for range 2 {
		var data []byte
		data, _, err = r.get(u, s.ByteRange, maxSegmentBytes)
		if err == nil {
			r.arrival = r.now()
			return data, nil
		}
		if r.ctx.Err() != nil {
			return nil, err
		}
		r.segmentErrors.Add(1)
	}
	return nil, err
}

// reload waits out the reload interval, then loads the playlist again,
// retrying once.
func (r *hlsReader) reload() error {
	interval := r.target
	if !r.grew {
		interval /= 2
	}
	if err := r.sleepUntil(r.loaded.Add(interval)); err != nil {
		return err
	}
	prevEnd := r.pl.MediaSequence + uint64(len(r.pl.Segments))
	var err error
	for range 2 {
		if err = r.loadPlaylist(); err == nil {
			break
		}
		if r.ctx.Err() != nil {
			return err
		}
		r.reloadErrors.Add(1)
	}
	if err != nil {
		return fmt.Errorf("reload: %w", err)
	}
	r.reloads.Add(1)
	r.grew = r.pl.MediaSequence+uint64(len(r.pl.Segments)) > prevEnd
	return nil
}

func (r *hlsReader) loadPlaylist() error {
	r.loaded = time.Now()
	u, err := url.Parse(r.rawURL)
	if err != nil {
		return errors.New("invalid url")
	}
	body, final, err := r.get(u, nil, maxPlaylistBytes)
	if err != nil {
		return err
	}
	return r.setPlaylist(body, final)
}

// sleepUntil waits until t or Close.
func (r *hlsReader) sleepUntil(t time.Time) error {
	d := time.Until(t)
	if d <= 0 {
		return r.ctx.Err()
	}
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-r.ctx.Done():
		return r.ctx.Err()
	case <-tm.C:
		return nil
	}
}

// get fetches u, or its range br, up to limit bytes, and returns the body
// and the URL it came from after redirects. Errors name no URL.
func (r *hlsReader) get(u *url.URL, br *hls.ByteRange, limit int64) ([]byte, *url.URL, error) {
	if br != nil && br.Length > limit {
		return nil, nil, fmt.Errorf("range of %d bytes exceeds %d", br.Length, limit)
	}
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, stripURLError(err)
	}
	if br != nil {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", br.Offset, br.Offset+br.Length-1))
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, nil, stripURLError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	partial := false
	switch {
	case br != nil && resp.StatusCode == http.StatusPartialContent:
		partial, limit = true, br.Length
	case resp.StatusCode == http.StatusOK && br != nil:
		// The server ignored Range: cut the range out of the whole body.
		if br.Offset+br.Length > limit {
			return nil, nil, fmt.Errorf("server ignored Range and the range ends past %d bytes", limit)
		}
		limit = br.Offset + br.Length
	case resp.StatusCode == http.StatusOK:
	default:
		return nil, nil, fmt.Errorf("status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, nil, stripURLError(err)
	}
	switch {
	case partial && int64(len(body)) < br.Length:
		return nil, nil, fmt.Errorf("range: got %d of %d bytes", len(body), br.Length)
	case partial:
		body = body[:br.Length]
	case br != nil:
		if int64(len(body)) < br.Offset+br.Length {
			return nil, nil, fmt.Errorf("range ends past the body's %d bytes", len(body))
		}
		body = body[br.Offset : br.Offset+br.Length]
	case int64(len(body)) > limit:
		return nil, nil, fmt.Errorf("body exceeds %d bytes", limit)
	}
	return body, resp.Request.URL, nil
}

// Kinds implements Reader.
func (r *hlsReader) Kinds() []media.Kind {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.run != nil {
		r.kinds = r.run.Kinds()
	}
	return slices.Clone(r.kinds)
}

// Spec implements Reader.
func (r *hlsReader) Spec() Spec { return r.spec }

// Stats implements Reader.
func (r *hlsReader) Stats() map[string]uint64 {
	r.mu.Lock()
	out := make(map[string]uint64, len(r.done)+7)
	for k, v := range r.done {
		out[k] = v
	}
	if r.run != nil {
		for k, v := range r.run.Stats() {
			out[k] += v
		}
	}
	r.mu.Unlock()
	out["segments"] = r.segments.Load()
	out["segment_errors"] = r.segmentErrors.Load()
	out["skipped_segments"] = r.skipped.Load()
	out["fell_behind"] = r.fellBehind.Load()
	out["playlist_reloads"] = r.reloads.Load()
	out["playlist_errors"] = r.reloadErrors.Load()
	out["discontinuities"] = r.discontinuities.Load()
	return out
}

// Close implements Reader.
func (r *hlsReader) Close() error {
	r.mu.Lock()
	r.closed = true
	run := r.run
	r.mu.Unlock()
	r.cancel()
	if run != nil {
		_ = run.Close()
	}
	return nil
}

// segFeed hands the segments of one run to its demuxer, fetching each when
// the demuxer has read the previous one. It ends with io.EOF at a break or
// at the end of the playlist, and with the error when fetching fails.
type segFeed struct {
	r       *hlsReader
	buf     []byte
	started bool // a segment was handed out
	end     runEnd
	fail    error
}

// Read implements io.Reader.
func (f *segFeed) Read(p []byte) (int, error) {
	for len(f.buf) == 0 {
		switch f.end {
		case runOpen:
		case runFailed:
			return 0, f.fail
		case runBreak, runEnded:
			return 0, io.EOF
		}
		var data []byte
		data, f.end, f.fail = f.r.nextSegment(f.started)
		if f.end == runOpen {
			f.buf, f.started = data, true
		}
	}
	n := copy(p, f.buf)
	f.buf = f.buf[n:]
	return n, nil
}

// Close implements io.Closer. Fetches stop when the reader closes.
func (f *segFeed) Close() error { return nil }
