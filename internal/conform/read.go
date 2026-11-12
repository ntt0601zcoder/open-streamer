package conform

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
	"sync/atomic"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/dash"
	"github.com/ntt0601zcoder/open-streamer/pkg/hls"
	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
)

const (
	// maxBody bounds every playlist, manifest and segment body.
	maxBody = 256 << 20
	// repeatCap is the per-entry repeat cap of dash.SegmentTimeline.Resolve.
	repeatCap = 100000
	// maxWidth bounds a SegmentTemplate %0Nd width.
	maxWidth = 32
	// maxTemplate bounds a SegmentTemplate media or initialization string.
	maxTemplate = 2 << 10
	// minPoll is the shortest wait between two refreshes.
	minPoll = 50 * time.Millisecond
	// periodTolerance is RulePeriodOrder's tolerance.
	periodTolerance = time.Millisecond
	// Fallback frame durations of a TS anchor track with one frame.
	videoFrameTicks = 3600
	aacFrameSamples = 1024
)

// maxSegments bounds the timeline segments of one MPD, over all its periods
// and representations, and the distinct segments, init URLs and
// representations of one DASH capture.
var maxSegments = 100000

// ReadOptions configures the readers.
type ReadOptions struct {
	// Duration of media to capture when polling; for ReadTS, 0 means read
	// until EOF.
	Duration time.Duration
	// Once reads the current playlist or MPD once and fetches every listed
	// segment, instead of polling. ReadTS ignores it.
	Once bool
	// Client makes the requests; nil means http.DefaultClient.
	Client *http.Client
	// Timeout bounds each playlist, manifest or segment request, body
	// included, and for ReadTS the wait for the response headers and every
	// stall between body reads, so a stalled server cannot hang a reader.
	// 0 means 30 s.
	Timeout time.Duration
}

const defaultTimeout = 30 * time.Second

// Capture is a read-back output: the frames of every track merged by DTS
// (stable), and the container and frame rule violations found.
type Capture struct {
	Frames     []media.Frame
	Violations []Violation
}

// reader is the HTTP side shared by the readers.
type reader struct {
	client  *http.Client
	timeout time.Duration
}

func newReader(opt ReadOptions) reader {
	c := opt.Client
	if c == nil {
		c = http.DefaultClient
	}
	t := opt.Timeout
	if t <= 0 {
		t = defaultTimeout
	}
	return reader{client: c, timeout: t}
}

// get fetches a URL within the timeout and returns the status and body.
func (r reader) get(ctx context.Context, u string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("conform: read: %w", err)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("conform: read: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return 0, nil, fmt.Errorf("conform: read: %s: %w", u, err)
	}
	if len(body) > maxBody {
		return 0, nil, fmt.Errorf("conform: read: %s: body above %d bytes", u, maxBody)
	}
	return resp.StatusCode, body, nil
}

// getOK fetches a playlist or manifest, which must answer 200.
func (r reader) getOK(ctx context.Context, u string) ([]byte, error) {
	code, body, err := r.get(ctx, u)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("conform: read: GET %s: status %d", u, code)
	}
	return body, nil
}

// resolve resolves ref against base.
func resolve(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("conform: read: base URL: %w", err)
	}
	rf, err := url.Parse(ref)
	if err != nil {
		return "", fmt.Errorf("conform: read: URI %q: %w", ref, err)
	}
	return b.ResolveReference(rf).String(), nil
}

// wait sleeps d or until ctx is done.
func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(max(d, minPoll))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("conform: read: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}

// ---- HLS ----

// tsRecord is what RuleExtinf needs from one TS segment, checked once the
// capture is complete because it may need the next segment.
type tsRecord struct {
	url      string
	msn      uint64
	disc     bool
	track    int   // anchor track
	extinf   int64 // ticks
	hasKey   bool
	firstKey int64 // PTS of the anchor's first keyframe
	end      int64 // anchor's largest PTS + its last DTS delta
	frame    int64 // anchor's frame duration
}

// hlsPlaylist is one media playlist being captured.
type hlsPlaylist struct {
	url      string
	src      string
	next     uint64 // media sequence number of the next segment to fetch
	polled   bool
	ended    bool
	target   time.Duration
	captured time.Duration
	records  []tsRecord
}

// hlsCapture is the state of one ReadHLS.
type hlsCapture struct {
	r        reader
	b        *builder
	lists    []*hlsPlaylist
	byURL    map[string]*hlsPlaylist
	active   map[string]bool // playlists listed by the latest master
	inits    map[string]fmp4Init
	badInits map[string]bool // inits that did not answer 200, reported once
	dec      mpegts.TimeDecoder
	codecs   []string // union of the CODECS of every master read
	master   bool
}

// ReadHLS reads an HLS master or media playlist, with TS or fMP4 segments,
// back into frames. With a master, every variant and every rendition with a
// URI is read; capture tracks are numbered by playlist and track in order of
// first appearance. Epochs count discontinuities from 1, per playlist.
//
// The first epoch's start rules apply only when every playlist's first
// captured segment opens its epoch (MEDIA-SEQUENCE 0 or
// EXT-X-DISCONTINUITY).
//
// When polling, the master is read again on every pass: its CODECS is
// rebuilt from the window, so the check uses the union over every read, and
// playlists it newly lists join the capture; playlists it no longer lists
// are not polled and do not hold polling open. When polling, the master is
// read once more after the last playlists, so its CODECS covers the newest
// window they showed; if that read fails the union collected so far is
// kept.
//
// Accepted limitations, not flagged: a track absent for a whole epoch has
// no entry carrying that epoch's discontinuity, so its playlist's epoch
// count can diverge from the other renditions' (R4). TS tracks are keyed by
// PID, so they keep their capture track across segments.
func ReadHLS(ctx context.Context, u string, opt ReadOptions) (Capture, error) {
	c := &hlsCapture{
		r: newReader(opt), b: newBuilder(), byURL: map[string]*hlsPlaylist{},
		inits: map[string]fmp4Init{}, badInits: map[string]bool{},
	}
	body, err := c.r.getOK(ctx, u)
	if err != nil {
		return Capture{}, err
	}
	c.master = bytes.Contains(body, []byte("#EXT-X-STREAM-INF"))
	if !c.master {
		c.lists = []*hlsPlaylist{{url: u, src: "pl0"}}
	}
	for pass := 0; ; pass++ {
		if c.master {
			if pass > 0 {
				if body, err = c.r.getOK(ctx, u); err != nil {
					return Capture{}, err
				}
			}
			if err := c.readMaster(u, body); err != nil {
				return Capture{}, err
			}
		}
		for _, pl := range c.lists {
			if pl.ended || (c.master && !c.active[pl.url]) {
				continue
			}
			b := body
			if c.master || pass > 0 {
				if b, err = c.r.getOK(ctx, pl.url); err != nil {
					return Capture{}, err
				}
			}
			if err := c.poll(ctx, pl, b); err != nil {
				return Capture{}, err
			}
		}
		if opt.Once || c.done(opt.Duration) {
			break
		}
		if err := wait(ctx, c.lists[0].target/2); err != nil {
			return Capture{}, err
		}
	}
	c.checkTSExtinf()
	if c.master {
		if !opt.Once {
			c.finalMaster(ctx, u)
		}
		c.b.checkMasterCodecs(c.codecs)
	}
	return c.b.finish(), nil
}

// finalMaster reads the master once more after the last playlist poll: the
// playlists were read after the previous master, so a later window may hold
// a codec it did not list yet. A failed read (the master gone after the
// playlists ended, a transient error) keeps the union collected so far.
func (c *hlsCapture) finalMaster(ctx context.Context, u string) {
	body, err := c.r.getOK(ctx, u)
	if err != nil {
		return
	}
	_ = c.readMaster(u, body) // on error the union collected so far stands
}

// readMaster reads one version of the master: it adds its CODECS to the
// union, marks the playlists it lists as active and adds the new ones.
func (c *hlsCapture) readMaster(u string, body []byte) error {
	m, err := hls.ParseMultivariant(body)
	if err != nil {
		return fmt.Errorf("conform: read: %s: %w", u, err)
	}
	var uris []string
	for _, v := range m.Variants {
		uris = append(uris, v.URI)
		for _, c1 := range strings.Split(v.Codecs, ",") {
			if c1 = strings.TrimSpace(c1); c1 != "" && !slices.Contains(c.codecs, c1) {
				c.codecs = append(c.codecs, c1)
			}
		}
	}
	for _, rd := range m.Renditions {
		if rd.URI != "" {
			uris = append(uris, rd.URI)
		}
	}
	if len(uris) == 0 {
		return fmt.Errorf("conform: read: %s: master lists no playlist", u)
	}
	c.active = map[string]bool{}
	for _, ref := range uris {
		abs, err := resolve(u, ref)
		if err != nil {
			return err
		}
		c.active[abs] = true
		if c.byURL[abs] == nil {
			pl := &hlsPlaylist{url: abs, src: "pl" + strconv.Itoa(len(c.lists))}
			c.byURL[abs] = pl
			c.lists = append(c.lists, pl)
		}
	}
	return nil
}

// done reports whether polling can stop: every playlist ended, or one has
// captured d of media.
func (c *hlsCapture) done(d time.Duration) bool {
	ended := true
	for _, pl := range c.lists {
		if c.master && !c.active[pl.url] {
			continue // the master no longer lists it
		}
		if pl.captured >= d {
			return true
		}
		ended = ended && pl.ended
	}
	return ended
}

// poll handles one refresh of a media playlist: it fetches every segment not
// fetched yet.
func (c *hlsCapture) poll(ctx context.Context, pl *hlsPlaylist, body []byte) error {
	p, err := hls.ParseMedia(body)
	if err != nil {
		return fmt.Errorf("conform: read: %s: %w", pl.url, err)
	}
	pl.target = time.Duration(p.TargetDuration) * time.Second
	pl.ended = p.Ended
	// DISCONTINUITY-SEQUENCE counts the discontinuities before the first
	// entry; each listed EXT-X-DISCONTINUITY, the first entry's included,
	// opens the next epoch.
	epoch := uint32(p.DiscontinuitySequence) + 1
	for i, s := range p.Segments {
		if s.Discontinuity {
			epoch++
		}
		msn := p.MediaSequence + uint64(i)
		if pl.polled && msn < pl.next {
			continue
		}
		if !pl.polled {
			// Every playlist's first segment must open its epoch for the
			// start rules to apply to the first epoch.
			first := msn == 0 || s.Discontinuity
			if !c.b.started {
				c.b.start(epoch, first)
			} else if !first {
				c.b.fromStart = false
			}
			pl.polled = true
		}
		pl.next = msn + 1
		if err := c.segment(ctx, pl, s, msn, epoch); err != nil {
			return err
		}
	}
	pl.polled = true
	return nil
}

// segment fetches and reads one listed segment.
func (c *hlsCapture) segment(ctx context.Context, pl *hlsPlaylist, s hls.Segment, msn uint64, epoch uint32) error {
	u, err := resolve(pl.url, s.URI)
	if err != nil {
		return err
	}
	var in fmp4Init
	if s.Map != "" {
		iu, err := resolve(pl.url, s.Map)
		if err != nil {
			return err
		}
		var ok bool
		if in, ok, err = c.init(ctx, iu); err != nil || !ok {
			return err
		}
	}
	code, body, err := c.r.get(ctx, u)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		c.b.violate(RuleSegmentAvailable, -1, "GET %s: status %d", u, code)
		return nil
	}
	pl.captured += s.Duration
	extinf := media.FromDuration(s.Duration)
	if s.Map == "" {
		return c.tsSegment(pl, u, body, msn, epoch, s.Discontinuity, extinf)
	}
	seg, err := parseSegment(body, in)
	if err != nil {
		return fmt.Errorf("conform: read: %s: %w", u, err)
	}
	track := c.b.track(pl.src, 0)
	for i := range seg.frames {
		seg.frames[i].Track, seg.frames[i].Epoch = track, epoch
	}
	c.b.checkSegmentKey(u, seg.frames, 0)
	ts := int(in.timescale)
	c.b.checkExtinf(u, track, extinf, media.SampleTime(int64(seg.duration), ts),
		media.SampleTime(int64(seg.duration)/int64(seg.samples), ts))
	for _, f := range seg.frames {
		c.b.add(f)
	}
	return nil
}

// init returns the parsed init segment at u, fetching it once. ok is false
// when it did not answer 200 (a violation is recorded).
func (c *hlsCapture) init(ctx context.Context, u string) (fmp4Init, bool, error) {
	if in, ok := c.inits[u]; ok {
		return in, true, nil
	}
	if c.badInits[u] {
		return fmp4Init{}, false, nil
	}
	in, ok, err := fetchInit(ctx, c.r, c.b, u)
	switch {
	case ok:
		c.inits[u] = in
	case err == nil:
		c.badInits[u] = true
	}
	return in, ok, err
}

// fetchInit fetches and parses an init segment.
func fetchInit(ctx context.Context, r reader, b *builder, u string) (fmp4Init, bool, error) {
	code, body, err := r.get(ctx, u)
	if err != nil {
		return fmp4Init{}, false, err
	}
	if code != http.StatusOK {
		b.violate(RuleSegmentAvailable, -1, "GET %s: status %d", u, code)
		return fmp4Init{}, false, nil
	}
	in, err := parseInit(body)
	if err != nil {
		return fmp4Init{}, false, fmt.Errorf("conform: read: %s: %w", u, err)
	}
	return in, true, nil
}

// tsSegment demuxes one TS segment with the capture's shared time decoder.
func (c *hlsCapture) tsSegment(pl *hlsPlaylist, u string, body []byte, msn uint64, epoch uint32, disc bool, extinf int64) error {
	d, err := ts.NewDemuxerWithOptions(bytes.NewReader(body), ts.DemuxOptions{Decoder: &c.dec})
	if err != nil {
		return fmt.Errorf("conform: read: %s: %w", u, err)
	}
	// Tracks are keyed by PID: a segment's demuxer numbers tracks from its
	// own PMT, so a segment missing a lower track would renumber the rest.
	pids := d.TrackPIDs()
	var frames []media.Frame
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("conform: read: %s: %w", u, err)
		}
		f.Track = c.b.track(pl.src, int(pids[f.Track]))
		f.Epoch = epoch
		frames = append(frames, f)
	}
	c.b.checkSegmentKey(u, frames, d.Stats().DroppedNoConfig)
	pl.records = append(pl.records, tsAnchor(frames, tsRecord{url: u, msn: msn, disc: disc, extinf: extinf}))
	for _, f := range frames {
		c.b.add(f)
	}
	return nil
}

// tsAnchor fills a record from the segment's anchor track: its lowest video
// track, else its lowest audio track.
func tsAnchor(frames []media.Frame, rec tsRecord) tsRecord {
	anchor, video := -1, false
	for _, f := range frames {
		v := f.Kind() == media.KindVideo
		if anchor < 0 || (v && !video) || (v == video && f.Track < anchor) {
			anchor, video = f.Track, v
		}
	}
	rec.track = anchor
	var n int
	var first, last, maxPTS, delta int64
	var rate int
	for _, f := range frames {
		if f.Track != anchor {
			continue
		}
		if !rec.hasKey && f.Key {
			rec.hasKey, rec.firstKey = true, f.PTS
		}
		if n == 0 {
			first, maxPTS = f.DTS, f.PTS
			if f.Config != nil {
				rate = f.Config.SampleRate
			}
		} else {
			delta = f.DTS - last
		}
		maxPTS = max(maxPTS, f.PTS)
		last = f.DTS
		n++
	}
	switch {
	case n >= 2:
		rec.frame = (last - first) / int64(n-1)
	case video || rate <= 0:
		delta, rec.frame = videoFrameTicks, videoFrameTicks
	default:
		delta = media.SampleTime(aacFrameSamples, rate)
		rec.frame = delta
	}
	rec.end = maxPTS + delta
	return rec
}

// checkTSExtinf applies RuleExtinf to the TS segments: the media duration is
// up to the next segment's first keyframe when that segment directly follows
// without a discontinuity, else up to the segment's own end.
func (c *hlsCapture) checkTSExtinf() {
	for _, pl := range c.lists {
		for i, rec := range pl.records {
			if !rec.hasKey {
				continue // already a segment-starts-on-key violation, or no anchor
			}
			end := rec.end
			if i+1 < len(pl.records) {
				if nx := pl.records[i+1]; nx.msn == rec.msn+1 && !nx.disc && nx.hasKey && nx.track == rec.track {
					end = nx.firstKey
				}
			}
			c.b.checkExtinf(rec.url, rec.track, rec.extinf, end-rec.firstKey, rec.frame)
		}
	}
}

// ---- DASH ----

// dashRep is one representation being captured.
type dashRep struct {
	track    int
	fetched  map[string]bool
	captured time.Duration
}

// dashCapture is the state of one ReadDASH.
type dashCapture struct {
	r       reader
	b       *builder
	url     string
	reps    map[string]*dashRep
	inits   map[string]fmp4Init
	bad     map[string]bool // inits that did not answer 200, reported once
	epochs  map[string]uint32
	ast     *time.Time
	polled  bool
	docSegs int64 // timeline segments of the MPD being read
	total   int   // distinct segments, init URLs and representations in the capture
	firstOK bool  // the first period's start was decided
}

// ReadDASH reads a DASH MPD with SegmentTemplate timelines back into frames.
// Epochs number periods from 1 in order of first appearance; tracks are
// keyed by Representation@id. A timeline expanding to more than 100000
// segments is an error.
//
// The first period's start rules apply only when its anchor set's (first
// video, else first audio) first captured segment has a tfdt equal to its
// presentationTimeOffset.
//
// Accepted limitation, not flagged: video media starts one composition
// offset after Period@start (the offset maps the epoch's first DTS, not its
// first PTS); period-order measures media from the presentationTimeOffset,
// so it does not see this.
func ReadDASH(ctx context.Context, u string, opt ReadOptions) (Capture, error) {
	c := &dashCapture{
		r: newReader(opt), b: newBuilder(), url: u,
		reps: map[string]*dashRep{}, inits: map[string]fmp4Init{}, bad: map[string]bool{}, epochs: map[string]uint32{},
	}
	for {
		body, err := c.r.getOK(ctx, u)
		if err != nil {
			return Capture{}, err
		}
		m, err := dash.Parse(body)
		if err != nil {
			return Capture{}, fmt.Errorf("conform: read: %s: %w", u, err)
		}
		if err := c.poll(ctx, m); err != nil {
			return Capture{}, err
		}
		if opt.Once || m.Type != "dynamic" || m.MinimumUpdatePeriod <= 0 || c.done(opt.Duration) {
			break
		}
		if err := wait(ctx, time.Duration(m.MinimumUpdatePeriod)/2); err != nil {
			return Capture{}, err
		}
	}
	return c.b.finish(), nil
}

// count adds a new segment, init URL or representation to the capture
// total, which maxSegments bounds.
func (c *dashCapture) count() error {
	if c.total++; c.total > maxSegments {
		return fmt.Errorf("conform: read: capture above %d segments, inits and representations", maxSegments)
	}
	return nil
}

func (c *dashCapture) done(d time.Duration) bool {
	for _, r := range c.reps {
		if r.captured >= d {
			return true
		}
	}
	return false
}

// dashSet is one representation of a period, resolved.
type dashSet struct {
	rep  dash.Representation
	tpl  dash.SegmentTemplate
	segs []dash.Seg
	in   fmp4Init
}

// poll handles one MPD refresh.
func (c *dashCapture) poll(ctx context.Context, m dash.MPD) error {
	if c.polled {
		if (c.ast == nil) != (m.AvailabilityStartTime == nil) ||
			(c.ast != nil && !c.ast.Equal(*m.AvailabilityStartTime)) {
			c.b.violate(RuleASTStable, -1, "availabilityStartTime changed from %v to %v", c.ast, m.AvailabilityStartTime)
		}
	} else {
		c.ast = m.AvailabilityStartTime
	}
	c.polled = true
	c.docSegs = 0
	var prevStart, prevEnd time.Duration
	for pi, per := range m.Periods {
		key := per.ID
		if key == "" {
			key = "@" + time.Duration(per.Start).String()
		}
		epoch, ok := c.epochs[key]
		if !ok {
			epoch = uint32(len(c.epochs)) + 1
			c.epochs[key] = epoch
		}
		sets, err := c.period(ctx, per)
		if err != nil {
			return err
		}
		start := time.Duration(per.Start)
		if pi > 0 && start+periodTolerance < prevEnd {
			c.b.once("period:"+key, RulePeriodOrder, -1, "period %s starts at %v, before the previous period's media ends at %v (start %v)",
				key, start, prevEnd, prevStart)
		}
		prevStart, prevEnd = start, start+mediaSpan(sets)
		if err := c.fetchPeriod(ctx, sets, epoch, !c.firstOK); err != nil {
			return err
		}
		c.firstOK = true
	}
	return nil
}

// mediaSpan returns the longest span from period start to media end over
// the sets.
func mediaSpan(sets []dashSet) time.Duration {
	var span time.Duration
	for _, s := range sets {
		if len(s.segs) == 0 {
			continue
		}
		last := s.segs[len(s.segs)-1]
		end := last.T + last.D
		if end < s.tpl.PresentationTimeOffset {
			continue
		}
		ticks := end - s.tpl.PresentationTimeOffset
		span = max(span, time.Duration(ticks*uint64(time.Second)/uint64(s.tpl.Timescale)))
	}
	return span
}

// period resolves a period's representations and reads their inits.
func (c *dashCapture) period(ctx context.Context, per dash.Period) ([]dashSet, error) {
	var out []dashSet
	for _, as := range per.AdaptationSets {
		for _, rep := range as.Representations {
			tp := as.SegmentTemplate
			if rep.SegmentTemplate != nil {
				tp = rep.SegmentTemplate
			}
			if tp == nil || tp.Timeline == nil {
				return nil, fmt.Errorf("conform: read: representation %q: no SegmentTemplate with a SegmentTimeline", rep.ID)
			}
			if len(tp.Media) > maxTemplate || len(tp.Initialization) > maxTemplate {
				return nil, fmt.Errorf("conform: read: representation %q: template above %d bytes", rep.ID, maxTemplate)
			}
			tpl := *tp
			if tpl.Timescale == 0 {
				tpl.Timescale = 1
			}
			for _, s := range tpl.Timeline.S {
				// As Resolve expands it: r below 0 is 0, r above the cap is
				// the cap, so the sum cannot overflow.
				c.docSegs += min(max(s.R, 0), repeatCap) + 1
				if c.docSegs > int64(maxSegments) {
					return nil, fmt.Errorf("conform: read: MPD timelines above %d segments", maxSegments)
				}
			}
			ref, err := expand(tpl.Initialization, rep, 0, 0)
			if err != nil {
				return nil, err
			}
			iu, err := resolve(c.url, ref)
			if err != nil {
				return nil, err
			}
			if c.bad[iu] {
				continue
			}
			in, ok := c.inits[iu]
			if !ok {
				if err := c.count(); err != nil {
					return nil, err
				}
				var good bool
				if in, good, err = fetchInit(ctx, c.r, c.b, iu); err != nil {
					return nil, err
				}
				if !good {
					c.bad[iu] = true
					continue
				}
				c.inits[iu] = in
			}
			if got, err := media.CodecString(in.cfg); err != nil || got != rep.Codecs {
				c.b.once("codecs:"+per.ID+"/"+rep.ID, RuleCodecString, -1,
					"period %s representation %s: @codecs %q, content %q (%v)", per.ID, rep.ID, rep.Codecs, got, err)
			}
			out = append(out, dashSet{rep: rep, tpl: tpl, segs: tpl.Timeline.Resolve(), in: in})
		}
	}
	return out, nil
}

// fetchPeriod fetches the period's segments not fetched yet. first marks the
// capture's first period, whose start rules apply only when its anchor
// set's first segment begins at the presentationTimeOffset.
func (c *dashCapture) fetchPeriod(ctx context.Context, sets []dashSet, epoch uint32, first bool) error {
	anchor := -1
	for i, s := range sets {
		v := s.in.cfg.Codec.Kind() == media.KindVideo
		if anchor < 0 || (v && sets[anchor].in.cfg.Codec.Kind() != media.KindVideo) {
			anchor = i
		}
	}
	for i, s := range sets {
		rp := c.reps[s.rep.ID]
		if rp == nil {
			if err := c.count(); err != nil {
				return err
			}
			rp = &dashRep{track: c.b.track("rep:"+s.rep.ID, 0), fetched: map[string]bool{}}
			c.reps[s.rep.ID] = rp
		}
		start := uint64(1)
		if s.tpl.StartNumber != nil {
			start = *s.tpl.StartNumber
		}
		for j, sg := range s.segs {
			ref, err := expand(s.tpl.Media, s.rep, start+uint64(j), sg.T)
			if err != nil {
				return err
			}
			u, err := resolve(c.url, ref)
			if err != nil {
				return err
			}
			if rp.fetched[u] {
				continue
			}
			rp.fetched[u] = true
			if err := c.count(); err != nil {
				return err
			}
			seg, ok, err := c.segment(ctx, u, s, sg, rp.track, epoch)
			if err != nil {
				return err
			}
			if ok && first && i == anchor && !c.b.started {
				c.b.start(epoch, seg.baseTime == s.tpl.PresentationTimeOffset)
			}
			if ok {
				rp.captured += time.Duration(sg.D * uint64(time.Second) / uint64(s.tpl.Timescale))
			}
		}
	}
	if first {
		// The anchor's first segment was not read: the start is unknown.
		c.b.start(epoch, false)
	}
	return nil
}

// segment fetches one media segment and checks it against its timeline
// entry.
func (c *dashCapture) segment(ctx context.Context, u string, s dashSet, sg dash.Seg, track int, epoch uint32) (fmp4Seg, bool, error) {
	code, body, err := c.r.get(ctx, u)
	if err != nil {
		return fmp4Seg{}, false, err
	}
	if code != http.StatusOK {
		c.b.violate(RuleSegmentAvailable, track, "GET %s: status %d", u, code)
		return fmp4Seg{}, false, nil
	}
	seg, err := parseSegment(body, s.in)
	if err != nil {
		return fmp4Seg{}, false, fmt.Errorf("conform: read: %s: %w", u, err)
	}
	earliest, dur := rescale(seg.earliest, s.in.timescale, s.tpl.Timescale), rescale(seg.duration, s.in.timescale, s.tpl.Timescale)
	if earliest != sg.T || dur != sg.D {
		c.b.violate(RuleTimeline, track, "%s: S@t %d S@d %d, media earliest %d duration %d", u, sg.T, sg.D, earliest, dur)
	}
	for i := range seg.frames {
		seg.frames[i].Track, seg.frames[i].Epoch = track, epoch
	}
	c.b.checkSegmentKey(u, seg.frames, 0)
	for _, f := range seg.frames {
		c.b.add(f)
	}
	return seg, true, nil
}

// rescale converts v from timescale from to timescale to.
func rescale(v uint64, from, to uint32) uint64 {
	if from == to {
		return v
	}
	return uint64(media.SampleTime(int64(v), int(from)) * int64(to) / media.ClockRate)
}

// expand substitutes the SegmentTemplate identifiers $RepresentationID$,
// $Number$, $Time$, $Bandwidth$ (each but the first with an optional %0Nd
// width, N at most 32) and $$. An unknown identifier is left as written.
func expand(tpl string, rep dash.Representation, number, t uint64) (string, error) {
	var b strings.Builder
	for {
		i := strings.IndexByte(tpl, '$')
		j := -1
		if i >= 0 {
			j = strings.IndexByte(tpl[i+1:], '$')
		}
		if j < 0 {
			b.WriteString(tpl)
			return b.String(), nil
		}
		b.WriteString(tpl[:i])
		id := tpl[i+1 : i+1+j]
		tpl = tpl[i+2+j:]
		name, format, hasFormat := strings.Cut(id, "%")
		var v uint64
		switch name {
		case "":
			b.WriteByte('$')
			continue
		case "RepresentationID":
			b.WriteString(rep.ID)
			continue
		case "Number":
			v = number
		case "Time":
			v = t
		case "Bandwidth":
			v = uint64(max(rep.Bandwidth, 0))
		default:
			b.WriteString("$" + id + "$")
			continue
		}
		s := strconv.FormatUint(v, 10)
		if hasFormat {
			w, err := templateWidth(format)
			if err != nil {
				return "", fmt.Errorf("conform: read: template $%s$: %w", id, err)
			}
			if pad := w - len(s); pad > 0 {
				s = strings.Repeat("0", pad) + s
			}
		}
		b.WriteString(s)
	}
}

// templateWidth parses the "0Nd" of a %0Nd format tag.
func templateWidth(format string) (int, error) {
	digits, ok := strings.CutSuffix(format, "d")
	if !ok || len(digits) < 2 || digits[0] != '0' {
		return 0, fmt.Errorf("format %%%s is not %%0[width]d", format)
	}
	w, err := strconv.Atoi(digits[1:])
	if err != nil || w < 1 || w > maxWidth {
		return 0, fmt.Errorf("format %%%s: width above %d or invalid", format, maxWidth)
	}
	return w, nil
}

// ---- HTTP-TS ----

// ReadTS reads a continuous MPEG-TS stream (HTTP-TS) with one demuxer that
// follows PMT changes. It stops at EOF, which is a clean end, or once
// Duration of media (the DTS span) is read; Duration 0 means EOF. Every
// frame is in epoch 1, and the config and start rules that need signalled
// epochs are skipped.
func ReadTS(ctx context.Context, u string, opt ReadOptions) (Capture, error) {
	r := newReader(opt)
	// The request is cancelled when the server stalls for the timeout: the
	// timer runs until the headers arrive and restarts on every body read.
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stalled atomic.Bool
	timer := time.AfterFunc(r.timeout, func() {
		stalled.Store(true)
		cancel()
	})
	defer timer.Stop()
	fail := func(err error) error {
		if stalled.Load() && ctx.Err() == nil {
			return fmt.Errorf("conform: read: %s: stalled for %v: %w", u, r.timeout, err)
		}
		return fmt.Errorf("conform: read: %s: %w", u, err)
	}
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, u, nil)
	if err != nil {
		return Capture{}, fmt.Errorf("conform: read: %w", err)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return Capture{}, fail(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Capture{}, fmt.Errorf("conform: read: GET %s: status %d", u, resp.StatusCode)
	}
	timer.Reset(r.timeout)
	body := &stallReader{r: resp.Body, timer: timer, d: r.timeout}
	d, err := ts.NewDemuxerWithOptions(body, ts.DemuxOptions{FollowPMT: true})
	if err != nil {
		return Capture{}, fail(err)
	}
	b := newBuilder()
	b.httpTS = true
	limit := media.FromDuration(opt.Duration)
	var lo, hi int64
	for n := 0; ; n++ {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				return Capture{}, fmt.Errorf("conform: read: %w", ctx.Err())
			}
			return Capture{}, fail(err)
		}
		f.Track = b.track("ts", f.Track)
		f.Epoch = 1
		b.add(f)
		if n == 0 {
			lo, hi = f.DTS, f.DTS
		}
		lo, hi = min(lo, f.DTS), max(hi, f.DTS)
		if limit > 0 && hi-lo >= limit {
			break
		}
	}
	return b.finish(), nil
}

// stallReader restarts timer for d after every read.
type stallReader struct {
	r     io.Reader
	timer *time.Timer
	d     time.Duration
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.timer.Reset(s.d)
	return n, err
}
