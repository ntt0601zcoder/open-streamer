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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/mux/fmp4"
	"github.com/ntt0601zcoder/open-streamer/pkg/hls"
	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
)

const (
	// maxPlaylistBytes caps a playlist body.
	maxPlaylistBytes = 4 << 20
	// maxSegmentBytes caps a segment body, which is held in memory whole.
	maxSegmentBytes = 64 << 20
	// maxInitBytes caps an fMP4 init segment (EXT-X-MAP).
	maxInitBytes = 1 << 20
	// liveStartSegments is how far from the end a live playlist is joined
	// (RFC 8216 §6.3.3).
	liveStartSegments = 3
	// maxTargetDuration caps EXT-X-TARGETDURATION, which sets the reload
	// interval and StallTimeout, so neither can grow without bound.
	maxTargetDuration = 60 * time.Second
	// maxRetryDelay caps the wait before a failed fetch is tried again; the
	// wait is a quarter target below it.
	maxRetryDelay = 500 * time.Millisecond
	// alignSlack is how far apart, in 90 kHz ticks, the variant's and the
	// audio rendition's frames may start after a break without either
	// being dropped, and how far past the broken playlist's last frame the
	// other playlist's frames still belong before the break.
	alignSlack = int64(media.ClockRate) // 1 s
	// pumpQueue is how many frames a playlist loop reads ahead of the merge.
	pumpQueue = 64
	// variantKey is the query parameter selecting a variant (0-based).
	variantKey = "variant"
)

var (
	errHLSEncrypted = errors.New("encrypted HLS is not supported")
	errHLSClosed    = errors.New("closed")
)

// runEnd says why a run, the segments fed to one demuxer, ended.
type runEnd uint8

const (
	runOpen   runEnd = iota // still feeding
	runBreak                // a timeline break: ErrDiscontinuity, then a new run
	runEnded                // the end of an EXT-X-ENDLIST playlist
	runFailed               // runState.fail holds the error
)

// hlsReader pulls an HLS input: a media playlist, or the chosen variant of a
// multivariant playlist (the highest BANDWIDTH, or ?variant=N in playlist
// order). When that variant's audio is a separate EXT-X-MEDIA rendition, the
// reader follows both media playlists, each in its own goroutine (pump), and
// merges their frames in DTS order: both are on the presentation timeline
// (RFC 8216 §6.2.4), which TS runs keep by not rebasing their timestamps
// (see timeDecoder) and fMP4 runs by their absolute tfdt. The rendition's audio is the reader track after the
// variant's own tracks.
//
// A break on either playlist is one ErrDiscontinuity. Before it, the other
// playlist's frames up to the broken one's last frame (plus alignSlack) are
// still delivered; after it, the frames of whichever playlist starts more
// than alignSlack earlier are dropped (dropped_unaligned) until both are on
// the same discontinuity sequence and within alignSlack. When either
// playlist stalls, Read waits for it: the pipeline's stall rule fails the
// whole input, there is no degraded mode with one playlist.
//
// Without a rendition, all fetching happens in the Read goroutine.
type hlsReader struct {
	rawURL    string // the input URL, for errors
	client    *http.Client
	transport *http.Transport // the reader's own, when Env.HTTP is nil
	ctx       context.Context // cancelled by Close
	cancel    context.CancelFunc
	now       func() time.Time
	spec      Spec

	main       *hlsSource
	audio      *hlsSource // the separate audio rendition; nil without one
	audioTrack int        // the rendition's reader track: after the variant's own

	// Merge state, Read goroutine only.
	sides      [2]mergeSide // main, audio
	aligning   bool         // after a break (and at the start): drop until aligned
	aligned    bool         // discOffset is set
	discOffset uint64       // added to the audio's discontinuity sequence

	unaligned, otherTracks atomic.Uint64
	wg                     sync.WaitGroup // the pumps

	mu     sync.Mutex
	closed bool
}

// openHLS starts a reader on a playlist fetched from final, whose load began
// at loaded, and reads up to the first PMT or init segment of every media
// playlist it follows. playlistURL is the URL a media playlist is reloaded
// from: the input URL without our variant parameter. Every request runs
// under rctx, which the reader owns and Close cancels (cancel); openHTTP
// cancels it when the open ctx ends first. Without Env.HTTP the reader has
// its own client, whose connections are kept alive between fetches and
// closed with it; the playlists of the open are fetched with client.
func openHLS(rctx context.Context, cancel context.CancelFunc, client *http.Client, rawURL string, playlistURL *url.URL,
	body []byte, final *url.URL, loaded time.Time, env Env,
) (*hlsReader, error) {
	r := &hlsReader{rawURL: rawURL, client: client, ctx: rctx, cancel: cancel, now: env.now()}
	if err := r.open(playlistURL, body, final, loaded, env); err != nil {
		r.closeSources()
		r.closeIdle()
		if errors.Is(err, io.EOF) {
			err = errors.New("playlist ended before the first PMT or init segment")
		}
		return nil, r.errf("open", err)
	}
	return r, nil
}

func (r *hlsReader) open(playlistURL *url.URL, body []byte, final *url.URL, loaded time.Time, env Env) error {
	mainURL := playlistURL
	var audioURL *url.URL
	var audioBody []byte
	var audioFinal *url.URL
	var audioLoaded time.Time
	if isMultivariant(body) {
		mv, err := hls.ParseMultivariant(body)
		if err != nil {
			return fmt.Errorf("parse multivariant playlist: %w", err)
		}
		v, err := selectVariant(mv, r.rawURL)
		if err != nil {
			return err
		}
		if mainURL, err = final.Parse(v.URI); err != nil {
			return errors.New("variant uri does not parse")
		}
		if rend, ok := audioRendition(mv, v); ok {
			if audioURL, err = final.Parse(rend.URI); err != nil {
				return errors.New("audio rendition uri does not parse")
			}
		}
		loaded = time.Now()
		if body, final, err = r.get(mainURL, nil, maxPlaylistBytes); err != nil {
			return fmt.Errorf("variant playlist: %w", err)
		}
		if audioURL != nil {
			audioLoaded = time.Now()
			if audioBody, audioFinal, err = r.get(audioURL, nil, maxPlaylistBytes); err != nil {
				return fmt.Errorf("audio rendition playlist: %w", err)
			}
		}
	}
	var err error
	if r.main, err = r.newSource(mainURL, body, final, loaded); err != nil {
		return err
	}
	target := r.main.target
	if audioURL != nil {
		if r.audio, err = r.newSource(audioURL, audioBody, audioFinal, audioLoaded); err != nil {
			return fmt.Errorf("audio rendition: %w", err)
		}
		target = max(target, r.audio.target)
	}
	r.spec = Spec{Pace: true, Live: true, StallTimeout: target * 3 / 2}
	if env.HTTP == nil {
		r.transport = newTransport()
		r.transport.MaxIdleConnsPerHost = 2
		r.transport.IdleConnTimeout = 2 * target
		r.client = &http.Client{Transport: r.transport}
	}
	if err := r.main.startRun(); err != nil {
		return err
	}
	if r.audio == nil {
		return nil
	}
	if err := r.audio.startRun(); err != nil {
		return fmt.Errorf("audio rendition: %w", err)
	}
	if !slices.Contains(r.audio.kindsNow(), media.KindAudio) {
		return errors.New("audio rendition has no audio track")
	}
	r.audioTrack = len(r.main.kindsNow())
	r.aligning = true
	mainCh, audioCh := make(chan item, pumpQueue), make(chan item, pumpQueue)
	r.sides = [2]mergeSide{{ch: mainCh}, {ch: audioCh}}
	r.wg.Add(2)
	go r.main.pump(mainCh, r.mapMain)
	go r.audio.pump(audioCh, r.mapAudio)
	return nil
}

func (r *hlsReader) errf(op string, err error) error {
	return fmt.Errorf("ingest: hls: %s %s: %w", op, Redact(r.rawURL), err)
}

// isMultivariant reports whether a playlist body is a multivariant one.
func isMultivariant(body []byte) bool {
	return bytes.Contains(body, []byte("#EXT-X-STREAM-INF"))
}

// selectVariant returns the variant that rawURL's variant parameter names,
// else the one with the highest BANDWIDTH (the first of equals).
func selectVariant(mv hls.MultivariantPlaylist, rawURL string) (hls.Variant, error) {
	if len(mv.Variants) == 0 {
		return hls.Variant{}, errors.New("multivariant playlist lists no variant")
	}
	if u, err := url.Parse(rawURL); err == nil {
		if vs, ok := u.Query()[variantKey]; ok {
			n, err := strconv.Atoi(vs[0])
			if err != nil || n < 0 || n >= len(mv.Variants) {
				return hls.Variant{}, fmt.Errorf("%s=%q: the playlist has variants 0 to %d", variantKey, vs[0], len(mv.Variants)-1)
			}
			return mv.Variants[n], nil
		}
	}
	best := mv.Variants[0]
	for _, v := range mv.Variants[1:] {
		if v.Bandwidth > best.Bandwidth {
			best = v
		}
	}
	return best, nil
}

// audioRendition returns the EXT-X-MEDIA audio rendition holding the
// variant's audio: the DEFAULT=YES entry of its AUDIO group, else the
// group's first. ok is false when the variant has no group, or when that
// entry has no URI, which means the audio is in the variant itself.
func audioRendition(mv hls.MultivariantPlaylist, v hls.Variant) (hls.Rendition, bool) {
	if v.Audio == "" {
		return hls.Rendition{}, false
	}
	var pick *hls.Rendition
	for i := range mv.Renditions {
		rd := &mv.Renditions[i]
		if rd.Type != "AUDIO" || rd.GroupID != v.Audio {
			continue
		}
		if pick == nil || (rd.Default && !pick.Default) {
			pick = rd
		}
	}
	if pick == nil || pick.URI == "" {
		return hls.Rendition{}, false
	}
	return *pick, true
}

// dropQuery returns u without the query parameter key, keeping the other
// pairs byte for byte (a signed URL stays valid).
func dropQuery(u *url.URL, key string) *url.URL {
	if u.RawQuery == "" {
		return u
	}
	pairs := strings.Split(u.RawQuery, "&")
	kept := pairs[:0:0]
	for _, pair := range pairs {
		k, _, _ := strings.Cut(pair, "=")
		if name, err := url.QueryUnescape(k); err == nil && name == key {
			continue
		}
		kept = append(kept, pair)
	}
	out := *u
	out.RawQuery = strings.Join(kept, "&")
	return &out
}

// parsePlaylist parses a media playlist and checks that it can be played.
func parsePlaylist(body []byte) (hls.MediaPlaylist, error) {
	if isMultivariant(body) {
		return hls.MediaPlaylist{}, errors.New("a multivariant playlist where a media playlist was expected")
	}
	pl, err := hls.ParseMedia(body)
	if err != nil {
		return hls.MediaPlaylist{}, fmt.Errorf("parse playlist: %w", err)
	}
	if pl.TargetDuration <= 0 {
		return hls.MediaPlaylist{}, errors.New("playlist has no EXT-X-TARGETDURATION")
	}
	for _, s := range pl.Segments {
		if s.Key.Method != "" && s.Key.Method != "NONE" {
			return hls.MediaPlaylist{}, errHLSEncrypted
		}
	}
	return pl, nil
}

// windowEnd returns the media sequence number after the playlist's last
// segment.
func windowEnd(pl hls.MediaPlaylist) uint64 {
	return pl.MediaSequence + uint64(len(pl.Segments))
}

// Read implements Reader.
func (r *hlsReader) Read() (Frame, error) {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return Frame{}, r.errf("read", errHLSClosed)
	}
	if r.audio != nil {
		f, err := r.readMerged()
		return f, r.readErr(err)
	}
	f, err := r.main.read()
	return f, r.readErr(err)
}

// readErr wraps a failure; ErrDiscontinuity and io.EOF pass as they are.
func (r *hlsReader) readErr(err error) error {
	if err == nil || errors.Is(err, ErrDiscontinuity) || errors.Is(err, io.EOF) {
		return err
	}
	return r.errf("read", err)
}

// mapMain moves the variant's tracks from the audio track's index up, so
// that a variant growing a track never collides with the rendition.
func (r *hlsReader) mapMain(f Frame) (Frame, bool) {
	if f.Track >= r.audioTrack {
		f.Track++
	}
	return f, true
}

// mapAudio keeps the rendition's first audio track, as the reader's audio
// track, and drops the others.
func (r *hlsReader) mapAudio(f Frame) (Frame, bool) {
	if slices.Index(r.audio.kindsNow(), media.KindAudio) != f.Track {
		r.otherTracks.Add(1)
		return f, false
	}
	f.Track = r.audioTrack
	return f, true
}

// item is what a pump hands to the merge: a frame with the discontinuity
// sequence of its segment, or an error (ErrDiscontinuity, io.EOF or a
// failure, after which the pump ends).
type item struct {
	f    Frame
	disc uint64
	err  error
}

// mergeSide is one playlist as the merge sees it.
type mergeSide struct {
	ch    <-chan item
	head  *item // the next frame; nil until received
	brk   bool  // the playlist broke before head
	ended bool  // io.EOF received
	last  int64 // DTS of the last frame delivered from this side
	any   bool  // a frame was delivered
	err   error // the failure that ended the pump
}

// fill receives the side's next frame unless it has one or ended.
func (r *hlsReader) fill(s *mergeSide) error {
	for s.head == nil && !s.ended {
		if s.err != nil {
			return s.err
		}
		select {
		case it := <-s.ch:
			switch {
			case it.err == nil:
				s.head = &it
			case errors.Is(it.err, ErrDiscontinuity):
				s.brk = true
			case errors.Is(it.err, io.EOF):
				s.ended = true
			default:
				s.err = it.err
			}
		case <-r.ctx.Done():
			return errHLSClosed
		}
	}
	return nil
}

// readMerged returns the next frame of the variant and the audio rendition
// in DTS order.
func (r *hlsReader) readMerged() (Frame, error) {
	m, a := &r.sides[0], &r.sides[1]
	for {
		if err := r.fill(m); err != nil {
			return Frame{}, err
		}
		if err := r.fill(a); err != nil {
			return Frame{}, err
		}
		if m.head == nil && a.head == nil {
			return Frame{}, io.EOF
		}
		if r.aligning {
			if r.dropUnaligned() {
				continue
			}
			r.aligning, m.brk, a.brk = false, false, false
		}
		mBrk, aBrk := m.brk && m.head != nil, a.brk && a.head != nil
		if mBrk || aBrk {
			// The other playlist first delivers what it has up to where
			// the broken one stopped.
			x, o := m, a
			if aBrk {
				x, o = a, m
			}
			if mBrk != aBrk && o.head != nil && x.any && o.head.f.DTS <= x.last+alignSlack {
				return r.deliver(o), nil
			}
			r.aligning = true
			return Frame{}, ErrDiscontinuity
		}
		s := m
		if m.head == nil || (a.head != nil && a.head.f.DTS < m.head.f.DTS) {
			s = a
		}
		return r.deliver(s), nil
	}
}

func (r *hlsReader) deliver(s *mergeSide) Frame {
	f := s.head.f
	s.head = nil
	s.last, s.any = f.DTS, true
	return f
}

// dropUnaligned drops one head that starts more than alignSlack before the
// other, or on an earlier discontinuity sequence, and reports whether it
// did. The first time both heads are there, it takes the difference of
// their discontinuity sequences as the offset between the playlists, which
// RFC 8216 wants zero but a packager may not keep so.
func (r *hlsReader) dropUnaligned() bool {
	m, a := &r.sides[0], &r.sides[1]
	if m.head == nil || a.head == nil {
		return false
	}
	if !r.aligned {
		r.discOffset, r.aligned = m.head.disc-a.head.disc, true
	}
	md, ad := m.head.disc, a.head.disc+r.discOffset
	switch {
	case md < ad:
		m.head = nil
	case ad < md:
		a.head = nil
	case m.head.f.DTS < a.head.f.DTS-alignSlack:
		m.head = nil
	case a.head.f.DTS < m.head.f.DTS-alignSlack:
		a.head = nil
	default:
		return false
	}
	r.unaligned.Add(1)
	return true
}

// Kinds implements Reader.
func (r *hlsReader) Kinds() []media.Kind {
	mk := r.main.kindsNow()
	if r.audio == nil {
		return mk
	}
	out := make([]media.Kind, r.audioTrack, max(len(mk), r.audioTrack)+1)
	copy(out, mk) // a variant that lost tracks leaves zero kinds, which map nowhere
	out = append(out, media.KindAudio)
	if len(mk) > r.audioTrack {
		out = append(out, mk[r.audioTrack:]...)
	}
	return out
}

// Spec implements Reader.
func (r *hlsReader) Spec() Spec { return r.spec }

// Stats implements Reader. Playlist counters are summed over the variant and
// the audio rendition.
func (r *hlsReader) Stats() map[string]uint64 {
	out := map[string]uint64{}
	r.main.addStats(out)
	if r.audio != nil {
		r.audio.addStats(out)
		out["dropped_unaligned"] = r.unaligned.Load()
		out["dropped_rendition_tracks"] = r.otherTracks.Load()
	}
	return out
}

// Close implements Reader.
func (r *hlsReader) Close() error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.cancel()
	r.closeSources()
	r.wg.Wait()
	r.closeIdle()
	return nil
}

func (r *hlsReader) closeSources() {
	for _, s := range []*hlsSource{r.main, r.audio} {
		if s != nil {
			s.close()
		}
	}
}

// closeIdle closes the idle connections of the reader's own transport.
func (r *hlsReader) closeIdle() {
	if r.transport != nil {
		r.transport.CloseIdleConnections()
	}
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
	defer func() {
		_ = resp.Body.Close()
		if r.ctx.Err() != nil {
			// Close may have run before this connection went idle.
			r.closeIdle()
		}
	}()
	partial := false
	switch {
	case br != nil && resp.StatusCode == http.StatusPartialContent:
		if err := checkContentRange(resp.Header.Get("Content-Range"), br); err != nil {
			return nil, nil, err
		}
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
	body, err := readLimited(resp.Body, resp.ContentLength, limit)
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

// checkContentRange checks that a 206 answers the requested range.
func checkContentRange(header string, br *hls.ByteRange) error {
	var from, to int64
	if _, err := fmt.Sscanf(header, "bytes %d-%d/", &from, &to); err != nil {
		return fmt.Errorf("206 with Content-Range %q", header)
	}
	if from != br.Offset || to != br.Offset+br.Length-1 {
		return fmt.Errorf("asked bytes %d-%d, got %d-%d", br.Offset, br.Offset+br.Length-1, from, to)
	}
	return nil
}

// readLimited reads up to limit+1 bytes of rd, preallocating for size bytes
// when the size is known (>= 0).
func readLimited(rd io.Reader, size, limit int64) ([]byte, error) {
	n := int64(512)
	if size >= 0 {
		n = min(size, limit) + 1
	}
	b := make([]byte, 0, n)
	lr := io.LimitReader(rd, limit+1)
	for {
		m, err := lr.Read(b[len(b):cap(b)])
		b = b[:len(b)+m]
		if errors.Is(err, io.EOF) {
			return b, nil
		}
		if err != nil {
			return b, err //nolint:wrapcheck // the caller wraps.
		}
		if len(b) == cap(b) {
			b = append(b, 0)[:len(b)]
		}
	}
}

// runner is the demuxer of one run: a tsReader over a segFeed, or an
// fmp4Run.
type runner interface {
	Read() (Frame, error)
	Kinds() []media.Kind
	Stats() map[string]uint64
	Close() error
}

// hlsSource follows one media playlist. The segments between two timeline
// breaks are one run: they feed one demuxer in order, so a TS PES may span
// segments. Every break (EXT-X-DISCONTINUITY, a new EXT-X-MAP, a skipped
// segment, a reader that fell behind the live window, a playlist that
// restarted) ends the run with ErrDiscontinuity, and the next run has a new
// demuxer; the config cache is shared, so equal configs keep their pointer.
// A reload that returns an older copy of the playlist, as load-balanced
// edges do, is ignored.
//
// Everything but the counters, the run and Close belongs to one goroutine:
// the Read goroutine, or the source's pump.
type hlsSource struct {
	r      *hlsReader
	url    *url.URL      // the playlist, reloaded as given
	target time.Duration // EXT-X-TARGETDURATION of the first load

	// Owner goroutine only.
	pl       hls.MediaPlaylist
	discs    []uint64  // discontinuity sequence of each segment of pl
	base     *url.URL  // where the playlist was last fetched from, after redirects
	next     uint64    // media sequence number of the next segment to fetch
	loaded   time.Time // when the last playlist load began
	grew     bool      // the last load listed a segment past the previous end
	arrival  time.Time // when the last segment finished downloading
	cache    media.ConfigCache
	st       *runState // of the current run
	runFrame bool      // the current run returned a frame
	initURI  string    // the last init segment fetched, as listed
	initData []byte

	segments, segmentErrors, skipped  atomic.Uint64
	fellBehind, reloads, reloadErrors atomic.Uint64
	discontinuities, restarts, stale  atomic.Uint64
	badSegments                       atomic.Uint64

	// The DTS of the last frame read, for the other playlist's next TS run.
	lastDTS atomic.Int64
	hasDTS  atomic.Bool

	mu     sync.Mutex // guards the fields below
	run    runner
	kinds  []media.Kind      // of the latest run, kept between runs
	done   map[string]uint64 // demuxer counters of finished runs
	closed bool
}

// newSource parses a media playlist fetched from final, whose load began at
// loaded, and positions the source at its start: the first segment of an
// EXT-X-ENDLIST playlist, else three from the end.
func (r *hlsReader) newSource(u *url.URL, body []byte, final *url.URL, loaded time.Time) (*hlsSource, error) {
	pl, err := parsePlaylist(body)
	if err != nil {
		return nil, err
	}
	s := &hlsSource{r: r, url: u, loaded: loaded, grew: true, done: map[string]uint64{}, arrival: r.now()}
	s.setPlaylist(pl, final)
	s.target = time.Duration(min(pl.TargetDuration, int(maxTargetDuration/time.Second))) * time.Second
	s.next = pl.MediaSequence
	if !pl.Ended {
		s.next = s.liveStart()
	}
	return s, nil
}

// setPlaylist makes pl the current playlist and numbers its segments'
// discontinuity sequences: EXT-X-DISCONTINUITY-SEQUENCE, plus one at each
// EXT-X-DISCONTINUITY up to and including the segment.
func (s *hlsSource) setPlaylist(pl hls.MediaPlaylist, base *url.URL) {
	s.pl, s.base = pl, base
	s.discs = make([]uint64, len(pl.Segments))
	d := pl.DiscontinuitySequence
	for i, seg := range pl.Segments {
		if seg.Discontinuity {
			d++
		}
		s.discs[i] = d
	}
}

// restarted reports whether pl, which ends before the current playlist,
// restarted the stream rather than being an older copy of it. An older copy
// lags by at most a window and has the same discontinuity sequence.
func (s *hlsSource) restarted(pl hls.MediaPlaylist) bool {
	window := uint64(max(len(s.pl.Segments), liveStartSegments))
	return pl.DiscontinuitySequence != s.pl.DiscontinuitySequence || windowEnd(pl)+window < windowEnd(s.pl)
}

// retryDelay is the wait before a failed fetch is tried again.
func (s *hlsSource) retryDelay() time.Duration {
	return min(s.target/4, maxRetryDelay)
}

// liveStart returns the media sequence number liveStartSegments from the
// end of the current playlist.
func (s *hlsSource) liveStart() uint64 {
	n := uint64(len(s.pl.Segments))
	return s.pl.MediaSequence + n - min(n, liveStartSegments)
}

// startRun opens a demuxer on the next run: a TS demuxer, or an fMP4 one on
// the segment's EXT-X-MAP. It returns io.EOF at the end of the playlist. A
// run that breaks before its first PMT is dropped.
func (s *hlsSource) startRun() error {
	for {
		seg, disc, end, err := s.advance(false)
		switch end {
		case runOpen:
		case runEnded:
			return io.EOF
		case runBreak, runFailed:
			return err
		}
		st := &runState{src: s, mapURI: seg.Map, disc: disc}
		s.st = st
		var run runner
		if seg.Map == "" {
			run, err = newTSReader(&segFeed{st: st}, func() time.Time { return s.arrival }, Spec{}, &s.cache, s.timeDecoder())
		} else {
			var init []byte
			init, err = s.fetchInit(seg)
			switch {
			case err != nil && s.r.ctx.Err() != nil:
				return err
			case err != nil:
				// The segment cannot be read without it: skip it.
				s.next++
				s.skipped.Add(1)
				continue
			}
			run, err = newFMP4Run(st, init, &s.cache)
		}
		if err != nil {
			switch st.end {
			case runOpen:
				return err
			case runBreak:
				s.discontinuities.Add(1)
				continue
			case runEnded:
				return io.EOF
			case runFailed:
				return st.fail
			}
			return err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			_ = run.Close()
			return errHLSClosed
		}
		s.run = run
		s.kinds = run.Kinds()
		s.runFrame = false
		return nil
	}
}

// timeDecoder returns the timestamp decoder of a new TS run. A lone playlist
// gets a private one (nil), whose timeline starts at 0. When two playlists
// are merged, both must stay on the raw 33-bit timeline they share (RFC 8216
// §6.2.4), not each rebased to its own first PES: a run starts on the raw
// value, unwrapped near the last DTS read from the other playlist (else from
// this one), so that a break on one side across a wrap of the counter does
// not leave the sides 2^33 apart.
func (s *hlsSource) timeDecoder() *mpegts.TimeDecoder {
	if s.r.audio == nil {
		return nil
	}
	dec := &mpegts.TimeDecoder{}
	other := s.r.main
	if s == s.r.main {
		other = s.r.audio
	}
	switch {
	case other.hasDTS.Load():
		dec.AnchorNear(other.lastDTS.Load())
	case s.hasDTS.Load():
		dec.AnchorNear(s.lastDTS.Load())
	default:
		dec.AnchorRaw()
	}
	return dec
}

// fetchInit returns the init segment of seg's EXT-X-MAP, fetching it unless
// it is the last one fetched.
func (s *hlsSource) fetchInit(seg hls.Segment) ([]byte, error) {
	if seg.Map == s.initURI && s.initData != nil {
		return s.initData, nil
	}
	data, err := s.fetch(seg.Map, nil, maxInitBytes)
	if err != nil {
		return nil, err
	}
	s.initURI, s.initData = seg.Map, data
	return data, nil
}

// endRun retires the current run, keeping its counters.
func (s *hlsSource) endRun(run runner) {
	s.mu.Lock()
	for k, v := range run.Stats() {
		s.done[k] += v
	}
	s.kinds = run.Kinds()
	s.run = nil
	s.mu.Unlock()
	_ = run.Close()
}

// read returns the next frame of the playlist, ErrDiscontinuity once per
// break, or io.EOF at the end. Failures are not wrapped.
func (s *hlsSource) read() (Frame, error) {
	for {
		s.mu.Lock()
		run, closed := s.run, s.closed
		s.mu.Unlock()
		if closed {
			return Frame{}, errHLSClosed
		}
		if run == nil {
			if err := s.startRun(); err != nil {
				return Frame{}, err
			}
			continue
		}
		f, err := run.Read()
		if err == nil {
			s.runFrame = true
		}
		if err == nil || errors.Is(err, ErrDiscontinuity) {
			return f, err
		}
		st := s.st
		s.endRun(run)
		switch st.end {
		case runOpen:
		case runBreak:
			s.discontinuities.Add(1)
			if !s.runFrame {
				continue // nothing since the previous break: no new one
			}
			return Frame{}, ErrDiscontinuity
		case runEnded:
			return Frame{}, io.EOF
		case runFailed:
			return Frame{}, st.fail
		}
		return Frame{}, err
	}
}

// pump reads the playlist into ch until a failure, the end, or Close. Each
// frame goes through keep, which sets its reader track or drops it.
func (s *hlsSource) pump(ch chan<- item, keep func(Frame) (Frame, bool)) {
	defer s.r.wg.Done()
	for {
		f, err := s.read()
		if err == nil {
			s.lastDTS.Store(f.DTS)
			s.hasDTS.Store(true)
			var ok bool
			if f, ok = keep(f); !ok {
				continue
			}
		}
		select {
		case ch <- item{f: f, disc: s.st.disc, err: err}:
		case <-s.r.ctx.Done():
			return
		}
		if err != nil && !errors.Is(err, ErrDiscontinuity) {
			return
		}
	}
}

// advance moves to the next listed segment, reloading the playlist as
// needed, and returns it with its discontinuity sequence without fetching
// it. started reports whether the run has a segment already: a jump of the
// position before its first segment is no break.
func (s *hlsSource) advance(started bool) (hls.Segment, uint64, runEnd, error) {
	for {
		if err := s.r.ctx.Err(); err != nil {
			return hls.Segment{}, 0, runFailed, err
		}
		first, end := s.pl.MediaSequence, windowEnd(s.pl)
		switch {
		case s.next < first || s.next > end:
			// The wanted segment left the window, or the playlist restarted
			// below it (reload accepts no older copy): rejoin the live edge.
			if s.next < first {
				s.fellBehind.Add(1)
			} else {
				s.restarts.Add(1)
			}
			s.next = s.liveStart()
			if started {
				return hls.Segment{}, 0, runBreak, nil
			}
			continue
		case s.next < end:
			i := s.next - first
			return s.pl.Segments[i], s.discs[i], runOpen, nil
		case s.pl.Ended:
			return hls.Segment{}, 0, runEnded, nil
		}
		if err := s.reload(); err != nil {
			return hls.Segment{}, 0, runFailed, err
		}
	}
}

// fetchSegment downloads a segment, retrying once, and stamps its arrival.
func (s *hlsSource) fetchSegment(seg hls.Segment) ([]byte, error) {
	data, err := s.fetch(seg.URI, seg.ByteRange, maxSegmentBytes)
	if err != nil {
		return nil, err
	}
	s.arrival = s.r.now()
	return data, nil
}

// fetch downloads uri, relative to the playlist, retrying once.
func (s *hlsSource) fetch(uri string, br *hls.ByteRange, limit int64) ([]byte, error) {
	u, err := s.base.Parse(uri)
	if err != nil {
		s.segmentErrors.Add(1)
		return nil, fmt.Errorf("segment uri: %w", err)
	}
	for try := range 2 {
		if try > 0 {
			if err := s.r.sleepUntil(time.Now().Add(s.retryDelay())); err != nil {
				return nil, err
			}
		}
		var data []byte
		data, _, err = s.r.get(u, br, limit)
		if err == nil {
			return data, nil
		}
		if s.r.ctx.Err() != nil {
			return nil, err
		}
		s.segmentErrors.Add(1)
	}
	return nil, err
}

// reload waits out the reload interval, then loads the playlist again,
// retrying once. An older copy than the current playlist is ignored and
// counts as a load without a new segment.
func (s *hlsSource) reload() error {
	interval := s.target
	if !s.grew {
		interval /= 2
	}
	if err := s.r.sleepUntil(s.loaded.Add(interval)); err != nil {
		return err
	}
	var (
		pl    hls.MediaPlaylist
		final *url.URL
		err   error
	)
	for try := range 2 {
		if try > 0 {
			if err := s.r.sleepUntil(time.Now().Add(s.retryDelay())); err != nil {
				return err
			}
		}
		if pl, final, err = s.loadPlaylist(); err == nil {
			break
		}
		if s.r.ctx.Err() != nil {
			return err
		}
		s.reloadErrors.Add(1)
	}
	if err != nil {
		return fmt.Errorf("reload: %w", err)
	}
	s.reloads.Add(1)
	prevEnd := windowEnd(s.pl)
	if windowEnd(pl) < prevEnd && !s.restarted(pl) {
		s.stale.Add(1)
		s.grew = false
		return nil
	}
	s.grew = windowEnd(pl) > prevEnd
	s.setPlaylist(pl, final)
	return nil
}

func (s *hlsSource) loadPlaylist() (hls.MediaPlaylist, *url.URL, error) {
	s.loaded = time.Now()
	body, final, err := s.r.get(s.url, nil, maxPlaylistBytes)
	if err != nil {
		return hls.MediaPlaylist{}, nil, err
	}
	pl, err := parsePlaylist(body)
	return pl, final, err
}

// kindsNow returns the tracks of the current run, else of the last one.
func (s *hlsSource) kindsNow() []media.Kind {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.run != nil {
		s.kinds = s.run.Kinds()
	}
	return slices.Clone(s.kinds)
}

// addStats adds the source's counters to out.
func (s *hlsSource) addStats(out map[string]uint64) {
	s.mu.Lock()
	for k, v := range s.done {
		out[k] += v
	}
	if s.run != nil {
		for k, v := range s.run.Stats() {
			out[k] += v
		}
	}
	s.mu.Unlock()
	out["segments"] += s.segments.Load()
	out["segment_errors"] += s.segmentErrors.Load()
	out["skipped_segments"] += s.skipped.Load()
	out["bad_segments"] += s.badSegments.Load()
	out["fell_behind"] += s.fellBehind.Load()
	out["playlist_reloads"] += s.reloads.Load()
	out["playlist_errors"] += s.reloadErrors.Load()
	out["discontinuities"] += s.discontinuities.Load()
	out["playlist_restarts"] += s.restarts.Load()
	out["stale_playlists"] += s.stale.Load()
}

// close stops the source: its run is closed and no new one starts.
func (s *hlsSource) close() {
	s.mu.Lock()
	s.closed = true
	run := s.run
	s.mu.Unlock()
	if run != nil {
		_ = run.Close()
	}
}

// runState is the position of one run in its playlist.
type runState struct {
	src     *hlsSource
	mapURI  string // the run's EXT-X-MAP as listed; "" for TS
	disc    uint64 // discontinuity sequence of the run's segments
	started bool   // a segment was handed out
	end     runEnd
	fail    error
}

// next returns the next segment of the run, or false when the run ends
// (st.end says why). A segment on another discontinuity sequence or map, or
// after an EXT-X-DISCONTINUITY, starts a new run.
func (st *runState) next() ([]byte, bool) {
	s := st.src
	for {
		seg, disc, end, err := s.advance(st.started)
		if end != runOpen {
			st.end, st.fail = end, err
			return nil, false
		}
		switch {
		case seg.Map != st.mapURI:
			st.end = runBreak // a new demuxer, even before the first segment
			return nil, false
		case st.started && (seg.Discontinuity || disc != st.disc):
			st.end = runBreak
			return nil, false
		}
		st.disc = disc
		data, err := s.fetchSegment(seg)
		if err != nil {
			if s.r.ctx.Err() != nil {
				st.end, st.fail = runFailed, s.r.ctx.Err()
				return nil, false
			}
			s.next++
			s.skipped.Add(1)
			if st.started {
				st.end = runBreak
				return nil, false
			}
			continue
		}
		s.next++
		s.segments.Add(1)
		st.started = true
		return data, true
	}
}

// segFeed hands the segments of one TS run to its demuxer, fetching each
// when the demuxer has read the previous one. It ends with io.EOF at a break
// or at the end of the playlist, and with the error when fetching fails.
type segFeed struct {
	st  *runState
	buf []byte
}

// Read implements io.Reader.
func (f *segFeed) Read(p []byte) (int, error) {
	for len(f.buf) == 0 {
		switch f.st.end {
		case runOpen:
		case runFailed:
			return 0, f.st.fail
		case runBreak, runEnded:
			return 0, io.EOF
		}
		if data, ok := f.st.next(); ok {
			f.buf = data
		}
	}
	n := copy(p, f.buf)
	f.buf = f.buf[n:]
	return n, nil
}

// Close implements io.Closer. Fetches stop when the reader closes.
func (f *segFeed) Close() error { return nil }

// fmp4Run reads the fMP4 segments of one run with one demuxer. A segment is
// demuxed whole when it arrives; its frames carry its arrival. A segment
// that does not demux is lost: before the run's first frame it is skipped,
// after it the run breaks.
type fmp4Run struct {
	st      *runState
	dmx     *fmp4.Demuxer
	kinds   []media.Kind
	cache   *media.ConfigCache // keyed by track, shared across runs
	queue   []media.Frame
	arrival time.Time
	emitted bool

	mu    sync.Mutex
	stats fmp4.DemuxStats
}

func newFMP4Run(st *runState, init []byte, cache *media.ConfigCache) (*fmp4Run, error) {
	dmx, err := fmp4.NewDemuxer(init)
	if err != nil {
		return nil, fmt.Errorf("init segment: %w", err)
	}
	return &fmp4Run{st: st, dmx: dmx, kinds: dmx.Kinds(), cache: cache}, nil
}

// Read implements runner. It returns io.EOF when the run ends.
func (f *fmp4Run) Read() (Frame, error) {
	for len(f.queue) == 0 {
		if f.st.end != runOpen {
			return Frame{}, io.EOF
		}
		data, ok := f.st.next()
		if !ok {
			return Frame{}, io.EOF
		}
		frames, err := f.dmx.Segment(data)
		f.mu.Lock()
		f.stats = f.dmx.Stats()
		f.mu.Unlock()
		if err != nil {
			f.st.src.badSegments.Add(1)
			if f.emitted {
				f.st.end = runBreak
				return Frame{}, io.EOF
			}
			continue
		}
		f.queue, f.arrival = frames, f.st.src.arrival
	}
	mf := f.queue[0]
	f.queue = f.queue[1:]
	mf.Config = f.cache.Stable(mf.Track, mf.Config)
	f.emitted = true
	return Frame{Frame: mf, Arrival: f.arrival}, nil
}

// Kinds implements runner.
func (f *fmp4Run) Kinds() []media.Kind { return slices.Clone(f.kinds) }

// Stats implements runner.
func (f *fmp4Run) Stats() map[string]uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return map[string]uint64{
		"dropped_no_config":      f.stats.DroppedNoConfig,
		"dropped_pts_before_dts": f.stats.DroppedPTSBeforeDTS,
		"dropped_empty":          f.stats.DroppedEmpty,
		"decode_errors":          f.stats.DecodeErrors,
		"clamped_overlaps":       f.stats.ClampedOverlaps,
	}
}

// Close implements runner.
func (f *fmp4Run) Close() error { return nil }
