package conform

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
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
	// Duration of media to capture. When ReadHLS or ReadDASH polls, it
	// stops once one playlist or representation has that much (or at the
	// end of the stream), so 0 means a single pass: to poll until the end,
	// give a Duration longer than the stream. For ReadTS and ReadRTMP, 0
	// means read until the end of the stream.
	Duration time.Duration
	// Once reads the current playlist or MPD once and fetches every listed
	// segment, instead of polling. ReadTS ignores it.
	Once bool
	// Client makes the requests; nil means http.DefaultClient.
	Client *http.Client
	// Timeout bounds each playlist, manifest or segment request, body
	// included, and for ReadTS the wait for the response headers and every
	// stall between body reads, so a stalled server cannot hang a reader.
	// For ReadRTMP it bounds the connection set-up and every wait for the
	// next message. 0 means 30 s.
	Timeout time.Duration
	// MTU is the largest RTP payload ReadRTSP accepts; 0 means 1200.
	MTU int
	// Dial, when set, opens ReadRTSP's control connection; tests use it to
	// interpose on the stream.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// TLS is the client config ReadRTMP uses for an rtmps:// URL and
	// ReadRTSP for an rtsps:// one; nil means the system roots. The HTTP
	// readers take theirs from Client.
	TLS *tls.Config
	// AlignedSequences makes ReadHLS require that the media playlists of a
	// master number their segments alike: equal MEDIA-SEQUENCE, so one
	// media sequence number is one segment in every rendition. RFC 8216bis
	// lets servers number renditions independently, so it is off by
	// default; set it for servers that promise alignment (ours do).
	AlignedSequences bool
	// AudioLanguage, when set, is the language ReadHLS and ReadDASH expect
	// on the audio (RuleAudioLanguage, R-L6 (b) to (d)). The other readers
	// ignore it.
	AudioLanguage string
}

const defaultTimeout = 30 * time.Second

// Capture is a read-back output: the frames of every track merged by DTS
// (stable), and the container and frame rule violations found.
type Capture struct {
	Frames     []media.Frame
	Violations []Violation
	// RTPLost counts the RTP packets ReadRTSP found missing from a track's
	// sequence numbers, over UDP (not a violation) and TCP alike.
	RTPLost uint64
	// ByeTracks counts the tracks that sent an RTCP BYE (ReadRTSP only).
	ByeTracks int
	// HLSGaps counts the EXT-X-GAP entries ReadHLS saw, once per playlist
	// and media sequence number. Gap entries are never fetched.
	HLSGaps int
	// AudioLanguages are the distinct audio languages ReadHLS and ReadDASH
	// read, in the order first seen: the LANGUAGE of the TYPE=AUDIO
	// renditions of every master version, and the lang of the audio
	// adaptation sets of every MPD version (R-L6).
	AudioLanguages []string
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
	real     map[uint32]bool // epochs with a real entry seen
}

// hlsEntry is what the gap-duration rule needs from one media playlist
// entry, by media sequence number.
type hlsEntry struct {
	src    string // playlist
	url    string
	gap    bool
	extinf int64 // ticks
	tol    int64 // ticks: one frame of a real entry's track; 0 for a gap
	bytes  int   // size of the segment; real entries only
	start  segStart
}

// segStart is the first video frame of a segment in decode order.
type segStart struct {
	video bool  // the segment has a video frame
	key   bool  // that frame is a keyframe
	pts   int64 // its PTS
}

// firstVideo returns the first video frame of frames, in the order given.
func firstVideo(frames []media.Frame) segStart {
	for _, f := range frames {
		if f.Kind() == media.KindVideo {
			return segStart{video: true, key: f.Key, pts: f.PTS}
		}
	}
	return segStart{}
}

// hlsRead is one media playlist read in a pass.
type hlsRead struct {
	pl *hlsPlaylist
	p  hls.MediaPlaylist
}

// alignTries bounds the back-to-back reads of a master's playlists that
// disagree before the disagreement is a violation: a playlist update
// between two reads is a race, not a fault.
const alignTries = 3

// gapTolerance is the gap-duration tolerance between two gap entries:
// EXTINF prints milliseconds.
const gapTolerance = media.ClockRate / 1000

// hlsCapture is the state of one ReadHLS.
type hlsCapture struct {
	r        reader
	opt      ReadOptions
	b        *builder
	lists    []*hlsPlaylist
	byURL    map[string]*hlsPlaylist
	active   map[string]bool // playlists listed by the latest master
	inits    map[string]fmp4Init
	badInits map[string]bool // inits that did not answer 200, reported once
	dec      mpegts.TimeDecoder
	codecs   []string // union of the CODECS of every master read
	master   bool
	gaps     int
	entries  map[uint64][]hlsEntry // by media sequence number; AlignedSequences only
	// variants are the master's variants in order of first appearance, with
	// the largest BANDWIDTH any master read gave each; defaults maps an
	// audio group to the URL of its DEFAULT rendition, as of the latest read.
	variants []*hlsVariant
	defaults map[string]string
	// misaligned: the renditions' media sequence numbers disagreed, so
	// entries cannot be matched by number.
	misaligned bool
	// pending is the rule the previous pass's playlists disagreed on
	// without AlignedSequences, not reported yet.
	pending string
}

// hlsVariant is one variant URI of the masters read.
type hlsVariant struct {
	url       string
	audio     string // audio group, as of the latest master
	bandwidth int    // largest BANDWIDTH any master gave
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
// A master's playlists are read back to back on every pass and must agree
// (RFC 8216 section 6.2.4), or they are read again, up to alignTries times
// in all. With AlignedSequences, MEDIA-SEQUENCE must be equal
// (RuleRenditionSequence) and so must the discontinuity sequence number of
// every media sequence number they share (RuleRenditionDiscontinuity); the
// re-reads follow at once, and a disagreement on the last is reported.
// Without it, the discontinuity sequence number of their last entries must
// be equal (RuleRenditionDiscontinuity); since renditions may differ by up
// to a target duration at the live edge, the re-reads are half the largest
// target duration apart, and a disagreement on the last is reported only
// when the next pass's reads disagree too, or at once with Once (which has
// no next pass; its reads span a target duration) or when every playlist
// has ended.
//
// Gap entries (EXT-X-GAP) are counted in HLSGaps and never fetched; their
// discontinuities count like any entry's. A playlist whose first entry in
// an epoch is a gap declares its media missing there, so in that epoch its
// tracks may start later than the others by the summed EXTINF of its gaps
// before its first real entry (RuleStartOffset; the others are not measured
// from them). With AlignedSequences, a gap
// entry's EXTINF must match the EXTINF of the same media sequence number
// in every other playlist, within one frame of a real entry's track (1 ms
// against another gap): RuleGapDuration.
//
// With AlignedSequences and two or more playlists that carry video, the PTS
// of the first video frame in decode order of each media sequence number must
// be equal across them (RuleVariantAlignment), compared only when every one
// of those entries starts with a keyframe; and for a master with two or more
// variants, each variant's measured peak bitrate, over its own playlist and
// its audio group's DEFAULT rendition playlist, must not exceed 1.1 times
// the largest BANDWIDTH a master gave it (RuleVariantBandwidth).
//
// TS tracks are keyed by PID, so they keep their capture track across
// segments.
func ReadHLS(ctx context.Context, u string, opt ReadOptions) (Capture, error) {
	c := &hlsCapture{
		r: newReader(opt), opt: opt, b: newBuilder(), byURL: map[string]*hlsPlaylist{},
		inits: map[string]fmp4Init{}, badInits: map[string]bool{}, entries: map[uint64][]hlsEntry{},
		defaults: map[string]string{},
	}
	body, err := c.r.getOK(ctx, u)
	if err != nil {
		return Capture{}, err
	}
	c.master = bytes.Contains(body, []byte("#EXT-X-STREAM-INF"))
	if !c.master {
		c.lists = []*hlsPlaylist{{url: u, src: "pl0"}}
		if opt.AudioLanguage != "" {
			c.b.once("lang-media:"+u, RuleAudioLanguage, -1,
				"%s: a media playlist, no master to carry a language (expected %q)", u, opt.AudioLanguage)
		}
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
		reads, err := c.readPlaylists(ctx, body, pass)
		if err != nil {
			return Capture{}, err
		}
		for _, rd := range reads {
			if err := c.poll(ctx, rd.pl, rd.p); err != nil {
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
	c.checkGaps()
	c.checkVariantAlignment()
	if c.master {
		// With Once, the master is read again only for RuleVariantBandwidth
		// (a peak segment may have appeared after the last master read).
		if !opt.Once || (opt.AlignedSequences && len(c.variants) >= 2) {
			c.finalMaster(ctx, u)
		}
		c.checkVariantBandwidth()
		c.b.checkMasterCodecs(c.codecs)
	}
	out := c.b.finish()
	out.HLSGaps = c.gaps
	return out, nil
}

// readPlaylists reads the media playlists of one pass. Without a master it
// is the one playlist (body on the first pass). With one, it reads every
// active playlist that has not ended, back to back, and checks that they
// agree, reading them again on a disagreement; one that persists is a
// violation, reported once per rule.
func (c *hlsCapture) readPlaylists(ctx context.Context, body []byte, pass int) ([]hlsRead, error) {
	if !c.master {
		pl := c.lists[0]
		if pl.ended {
			return nil, nil
		}
		if pass > 0 {
			var err error
			if body, err = c.r.getOK(ctx, pl.url); err != nil {
				return nil, err
			}
		}
		p, err := parseMedia(pl, body)
		if err != nil {
			return nil, err
		}
		return []hlsRead{{pl, p}}, nil
	}
	for try := 1; ; try++ {
		var reads []hlsRead
		for _, pl := range c.lists {
			if pl.ended || !c.active[pl.url] {
				continue
			}
			b, err := c.r.getOK(ctx, pl.url)
			if err != nil {
				return nil, err
			}
			p, err := parseMedia(pl, b)
			if err != nil {
				return nil, err
			}
			reads = append(reads, hlsRead{pl, p})
		}
		rule, detail := c.disagreement(reads)
		if rule == "" {
			c.pending = ""
			return reads, nil
		}
		if try < alignTries {
			if !c.opt.AlignedSequences {
				// Renditions may differ by up to a target duration at the
				// live edge (RFC 8216 section 6.2.4): read again later.
				if err := wait(ctx, maxTarget(reads)/2); err != nil {
					return nil, err
				}
			}
			continue
		}
		switch {
		case c.opt.AlignedSequences, c.opt.Once, c.pending == rule, allEnded(reads):
			if rule == RuleRenditionSequence {
				c.misaligned = true
			}
			c.b.once("align:"+rule, rule, -1, "%s", detail)
		default:
			c.pending = rule // flagged if the next pass still disagrees
		}
		return reads, nil
	}
}

// allEnded reports whether every read playlist has EXT-X-ENDLIST: final, so
// no later pass can bring them into agreement.
func allEnded(reads []hlsRead) bool {
	for _, rd := range reads {
		if !rd.p.Ended {
			return false
		}
	}
	return true
}

// maxTarget returns the largest EXT-X-TARGETDURATION of the reads.
func maxTarget(reads []hlsRead) time.Duration {
	var d time.Duration
	for _, rd := range reads {
		d = max(d, time.Duration(rd.p.TargetDuration)*time.Second)
	}
	return d
}

func parseMedia(pl *hlsPlaylist, body []byte) (hls.MediaPlaylist, error) {
	p, err := hls.ParseMedia(body)
	if err != nil {
		return p, fmt.Errorf("conform: read: %s: %w", pl.url, err)
	}
	return p, nil
}

// discSeq returns the discontinuity sequence number of each entry of p.
func discSeq(p hls.MediaPlaylist) []uint64 {
	out := make([]uint64, len(p.Segments))
	d := p.DiscontinuitySequence
	for i, s := range p.Segments {
		if s.Discontinuity {
			d++
		}
		out[i] = d
	}
	return out
}

// disagreement returns the rule the playlists of one pass break by
// disagreeing, and a detail; rule is "" when they agree.
func (c *hlsCapture) disagreement(reads []hlsRead) (rule, detail string) {
	if len(reads) < 2 {
		return "", ""
	}
	ref := reads[0]
	refD := discSeq(ref.p)
	if !c.opt.AlignedSequences {
		end := func(p hls.MediaPlaylist, d []uint64) uint64 {
			if len(d) == 0 {
				return p.DiscontinuitySequence
			}
			return d[len(d)-1]
		}
		for _, rd := range reads[1:] {
			if a, b := end(ref.p, refD), end(rd.p, discSeq(rd.p)); a != b {
				return RuleRenditionDiscontinuity, fmt.Sprintf("last entry: discontinuity sequence %d in %s, %d in %s",
					a, ref.pl.url, b, rd.pl.url)
			}
		}
		return "", ""
	}
	for _, rd := range reads[1:] {
		if rd.p.MediaSequence != ref.p.MediaSequence {
			return RuleRenditionSequence, fmt.Sprintf("MEDIA-SEQUENCE %d in %s, %d in %s",
				ref.p.MediaSequence, ref.pl.url, rd.p.MediaSequence, rd.pl.url)
		}
	}
	for _, rd := range reads[1:] {
		for i, d := range discSeq(rd.p) {
			if i < len(refD) && refD[i] != d {
				return RuleRenditionDiscontinuity, fmt.Sprintf("media sequence %d: discontinuity sequence %d in %s, %d in %s",
					rd.p.MediaSequence+uint64(i), refD[i], ref.pl.url, d, rd.pl.url)
			}
		}
	}
	return "", ""
}

// entry records one entry for the gap-duration rule.
func (c *hlsCapture) entry(msn uint64, e hlsEntry) {
	if c.opt.AlignedSequences {
		c.entries[msn] = append(c.entries[msn], e)
	}
}

// checkGaps applies RuleGapDuration: with aligned sequences, each gap
// entry's EXTINF matches the other playlists' entries of its media sequence
// number.
func (c *hlsCapture) checkGaps() {
	if !c.opt.AlignedSequences || c.misaligned {
		return
	}
	msns := make([]uint64, 0, len(c.entries))
	for msn := range c.entries {
		msns = append(msns, msn)
	}
	slices.Sort(msns)
	for _, msn := range msns {
		es := c.entries[msn]
		for _, g := range es {
			if !g.gap {
				continue
			}
			for _, o := range es {
				if o.src == g.src {
					continue
				}
				tol := o.tol
				if o.gap {
					tol = gapTolerance
				}
				if d := g.extinf - o.extinf; d > tol || -d > tol {
					c.b.once("gap:"+g.url, RuleGapDuration, -1, "%s: EXTINF %d ticks, %s EXTINF %d ticks (tolerance %d)",
						g.url, g.extinf, o.url, o.extinf, tol)
				}
			}
		}
	}
}

// checkVariantAlignment applies RuleVariantAlignment: with aligned
// sequences, the entries of a media sequence number that carry video start
// at the same PTS in every playlist, once all of them start with a keyframe.
func (c *hlsCapture) checkVariantAlignment() {
	if !c.opt.AlignedSequences || c.misaligned {
		return
	}
	msns := make([]uint64, 0, len(c.entries))
	for msn := range c.entries {
		msns = append(msns, msn)
	}
	slices.Sort(msns)
	for _, msn := range msns {
		var vs []hlsEntry
		for _, e := range c.entries[msn] {
			if !e.gap && e.start.video {
				vs = append(vs, e)
			}
		}
		if len(vs) < 2 || slices.ContainsFunc(vs, func(e hlsEntry) bool { return !e.start.key }) {
			continue
		}
		for i, a := range vs {
			for _, b := range vs[i+1:] {
				if a.start.pts != b.start.pts {
					lo, hi := min(a.src, b.src), max(a.src, b.src)
					c.b.once("aligned:"+lo+"|"+hi, RuleVariantAlignment, -1,
						"media sequence %d: first video PTS %d in %s, %d in %s", msn, a.start.pts, a.url, b.start.pts, b.url)
				}
			}
		}
	}
}

// bandwidthSlack is RuleVariantBandwidth's tolerance, the fraction 11/10.
const bandwidthSlackNum, bandwidthSlackDen = 11, 10

// checkVariantBandwidth applies RuleVariantBandwidth: a variant's measured
// peak bitrate does not exceed 1.1 times its largest BANDWIDTH.
func (c *hlsCapture) checkVariantBandwidth() {
	if !c.opt.AlignedSequences || c.misaligned || len(c.variants) < 2 {
		return
	}
	rate := func(e hlsEntry) int64 {
		if e.gap || e.extinf <= 0 {
			return 0
		}
		return int64(e.bytes) * 8 * media.ClockRate / e.extinf
	}
	for _, v := range c.variants {
		pl := c.byURL[v.url]
		if pl == nil || v.bandwidth <= 0 {
			continue
		}
		audioSrc := ""
		if ap := c.byURL[c.defaults[v.audio]]; ap != nil && v.audio != "" {
			audioSrc = ap.src
		}
		var peak int64
		var at uint64
		for msn, es := range c.entries {
			var own, aud int64
			var haveOwn bool
			for _, e := range es {
				switch {
				case e.src == pl.src && !e.gap:
					own, haveOwn = rate(e), true
				case audioSrc != "" && e.src == audioSrc:
					aud = rate(e)
				}
			}
			if r := own + aud; haveOwn && (r > peak || r == peak && msn < at) {
				peak, at = r, msn
			}
		}
		if peak*bandwidthSlackDen > int64(v.bandwidth)*bandwidthSlackNum {
			c.b.once("bandwidth:"+v.url, RuleVariantBandwidth, -1,
				"%s: measured peak %d bit/s (media sequence %d) above 1.1 times BANDWIDTH %d", v.url, peak, at, v.bandwidth)
		}
	}
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

// readMaster reads one version of the master: it checks its audio
// languages, adds its CODECS to the union, marks the playlists it lists as
// active and adds the new ones.
func (c *hlsCapture) readMaster(u string, body []byte) error {
	m, err := hls.ParseMultivariant(body)
	if err != nil {
		return fmt.Errorf("conform: read: %s: %w", u, err)
	}
	c.masterLanguages(u, m)
	var uris []string
	for _, rd := range m.Renditions {
		if rd.Default && rd.URI != "" && rd.Type == "AUDIO" {
			abs, err := resolve(u, rd.URI)
			if err != nil {
				return err
			}
			c.defaults[rd.GroupID] = abs
		}
	}
	for _, v := range m.Variants {
		uris = append(uris, v.URI)
		abs, err := resolve(u, v.URI)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(c.variants, func(x *hlsVariant) bool { return x.url == abs })
		if i < 0 {
			c.variants = append(c.variants, &hlsVariant{url: abs})
			i = len(c.variants) - 1
		}
		c.variants[i].audio = v.Audio
		c.variants[i].bandwidth = max(c.variants[i].bandwidth, v.Bandwidth)
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

// masterLanguages applies RuleAudioLanguage to one version of the master.
// It has audio when it has a TYPE=AUDIO rendition, a variant with an AUDIO
// group, or an audio codec in a variant's CODECS.
func (c *hlsCapture) masterLanguages(u string, m hls.MultivariantPlaylist) {
	var labels []string
	hasAudio := false
	for _, rd := range m.Renditions {
		if rd.Type == "AUDIO" {
			hasAudio = true
			labels = append(labels, rd.Language)
		}
	}
	for _, v := range m.Variants {
		if v.Audio != "" || slices.ContainsFunc(strings.Split(v.Codecs, ","), audioCodec) {
			hasAudio = true
		}
	}
	c.b.checkAudioLanguages(u, c.opt.AudioLanguage, "rendition", labels, hasAudio)
}

// audioCodec reports whether a CODECS entry is an audio codec: mp4a, ac-3,
// ec-3, opus, fLaC, ac-4, alac or mhm1, by its first dot-separated part,
// compared case-insensitively.
func audioCodec(codec string) bool {
	head, _, _ := strings.Cut(strings.TrimSpace(codec), ".")
	switch strings.ToLower(head) {
	case "mp4a", "ac-3", "ec-3", "opus", "flac", "ac-4", "alac", "mhm1":
		return true
	}
	return false
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
// fetched yet, and counts the gap entries instead of fetching them.
func (c *hlsCapture) poll(ctx context.Context, pl *hlsPlaylist, p hls.MediaPlaylist) error {
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
		if pl.real == nil {
			pl.real = map[uint32]bool{}
		}
		if s.Gap && !pl.real[epoch] {
			// Its media starts late in the epoch, by as much as its
			// leading gaps declare.
			c.b.gapStart[srcEpoch{pl.src, epoch}] += media.FromDuration(s.Duration)
		}
		if !s.Gap {
			pl.real[epoch] = true
		}
		if s.Gap {
			u, err := resolve(pl.url, s.URI)
			if err != nil {
				return err
			}
			c.gaps++
			c.entry(msn, hlsEntry{src: pl.src, url: u, gap: true, extinf: media.FromDuration(s.Duration)})
			continue
		}
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
		start, err := c.tsSegment(pl, u, body, msn, epoch, s.Discontinuity, extinf)
		if err != nil {
			return err
		}
		c.entry(msn, hlsEntry{
			src: pl.src, url: u, extinf: extinf, tol: pl.records[len(pl.records)-1].frame,
			bytes: len(body), start: start,
		})
		return nil
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
	frame := media.SampleTime(int64(seg.duration)/int64(seg.samples), ts)
	c.b.checkExtinf(u, track, extinf, media.SampleTime(int64(seg.duration), ts), frame)
	c.entry(msn, hlsEntry{src: pl.src, url: u, extinf: extinf, tol: frame, bytes: len(body), start: firstVideo(seg.frames)})
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
func (c *hlsCapture) tsSegment(pl *hlsPlaylist, u string, body []byte, msn uint64, epoch uint32, disc bool, extinf int64) (segStart, error) {
	d, err := ts.NewDemuxerWithOptions(bytes.NewReader(body), ts.DemuxOptions{Decoder: &c.dec})
	if err != nil {
		return segStart{}, fmt.Errorf("conform: read: %s: %w", u, err)
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
			return segStart{}, fmt.Errorf("conform: read: %s: %w", u, err)
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
	return firstVideo(frames), nil
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
	r      reader
	b      *builder
	url    string
	lang   string // ReadOptions.AudioLanguage
	reps   map[string]*dashRep
	inits  map[string]fmp4Init
	bad    map[string]bool // inits that did not answer 200, reported once
	epochs map[string]uint32
	// startKey records, per fetched segment, whether its first sample is a
	// keyframe.
	startKey map[segID]bool
	ast      *time.Time
	polled   bool
	docSegs  int64 // timeline segments of the MPD being read
	total    int   // distinct segments, init URLs and representations in the capture
	firstOK  bool  // the first period's start was decided
}

// ReadDASH reads a DASH MPD with SegmentTemplate timelines back into frames.
// Epochs number periods from 1 in order of first appearance; tracks are
// keyed by Representation@id. A timeline expanding to more than 100000
// segments is an error.
//
// In each period, an adaptation set with two or more video representations
// must be aligned in time (RuleVariantAlignment): over the span two of them
// share, no segment of one contains the start of one of the other's strictly
// inside it (a segment ends at its S@t + S@d or at its successor's S@t,
// whichever comes first), compared only where both segments start on a
// keyframe; with
// segmentAlignment="true" their resolved timelines must also be equal. A
// segment is compared only when its first video frame is a keyframe at its
// S@t. Each pair is reported once per period.
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
		r: newReader(opt), b: newBuilder(), url: u, lang: opt.AudioLanguage,
		reps: map[string]*dashRep{}, inits: map[string]fmp4Init{}, bad: map[string]bool{}, epochs: map[string]uint32{},
		startKey: map[segID]bool{},
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

// segID names a segment of a representation by its timeline start.
type segID struct {
	rep string
	t   uint64
}

// dashSet is one representation of a period, resolved.
type dashSet struct {
	set   int  // index of its adaptation set in the period
	align bool // the set's segmentAlignment
	rep   dash.Representation
	tpl   dash.SegmentTemplate
	segs  []dash.Seg
	in    fmp4Init
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
	c.languages(m)
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
		c.checkAlignment(key, sets)
		c.firstOK = true
	}
	return nil
}

// span is a segment on the 90 kHz clock.
type span struct{ start, end int64 }

// checkAlignment applies RuleVariantAlignment to the adaptation sets of one
// period that have two or more video representations: over the span two
// representations share, no segment of one contains the start of the
// other's strictly inside it, comparing only segments that start on a
// keyframe. With segmentAlignment, the timelines must also be equal.
func (c *dashCapture) checkAlignment(period string, sets []dashSet) {
	bySet := map[int][]dashSet{}
	for _, s := range sets {
		if s.in.cfg.Codec.Kind() == media.KindVideo {
			bySet[s.set] = append(bySet[s.set], s)
		}
	}
	ids := make([]int, 0, len(bySet))
	for id := range bySet {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		reps := bySet[id]
		for i, a := range reps {
			for _, b := range reps[i+1:] {
				lo, hi := min(a.rep.ID, b.rep.ID), max(a.rep.ID, b.rep.ID)
				key := "aligned:" + period + "|" + lo + "|" + hi
				if detail := c.misaligned(a, b); detail != "" {
					c.b.once(key, RuleVariantAlignment, -1, "period %s: representations %s and %s: %s",
						period, a.rep.ID, b.rep.ID, detail)
				}
			}
		}
	}
}

// timeline returns the segments of s on the 90 kHz clock. A segment ends
// at S@t + S@d, or at the next segment's S@t when that comes first: S@d is
// the media's decode duration and S@t its earliest presentation time, so
// where an encoder reorders across a source PTS gap a Representation's own
// timeline overlaps itself, and that is not a misalignment with another.
func (s dashSet) timeline(c *dashCapture) (all, keyed []span) {
	scale := int(s.tpl.Timescale)
	for i, sg := range s.segs {
		end := sg.T + sg.D
		if i+1 < len(s.segs) {
			end = min(end, s.segs[i+1].T)
		}
		sp := span{media.SampleTime(int64(sg.T), scale), media.SampleTime(int64(end), scale)}
		all = append(all, sp)
		if c.startKey[segID{s.rep.ID, sg.T}] {
			keyed = append(keyed, sp)
		}
	}
	return all, keyed
}

// misaligned describes how a and b are not aligned, or returns "".
func (c *dashCapture) misaligned(a, b dashSet) string {
	allA, keyA := a.timeline(c)
	allB, keyB := b.timeline(c)
	if len(allA) == 0 || len(allB) == 0 {
		return ""
	}
	if a.align || b.align {
		if !slices.Equal(allA, allB) {
			return "segmentAlignment is true but the timelines differ"
		}
	}
	lo := max(allA[0].start, allB[0].start)
	hi := min(allA[len(allA)-1].end, allB[len(allB)-1].end)
	straddles := func(x, y []span) (span, int64, bool) {
		for _, sx := range x {
			for _, sy := range y {
				if sy.start >= lo && sy.start < hi && sx.start < sy.start && sy.start < sx.end {
					return sx, sy.start, true
				}
			}
		}
		return span{}, 0, false
	}
	if sx, t, ok := straddles(keyA, keyB); ok {
		return fmt.Sprintf("a segment of %s (%d to %d) contains a segment start of %s (%d)", a.rep.ID, sx.start, sx.end, b.rep.ID, t)
	}
	if sx, t, ok := straddles(keyB, keyA); ok {
		return fmt.Sprintf("a segment of %s (%d to %d) contains a segment start of %s (%d)", b.rep.ID, sx.start, sx.end, a.rep.ID, t)
	}
	return ""
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

// languages applies RuleAudioLanguage to one version of the MPD: the lang
// of the audio adaptation sets of every period.
func (c *dashCapture) languages(m dash.MPD) {
	var labels []string
	hasAudio := false
	for _, per := range m.Periods {
		for _, as := range per.AdaptationSets {
			if audioSet(as) {
				hasAudio = true
				labels = append(labels, as.Lang)
			}
		}
	}
	c.b.checkAudioLanguages(c.url, c.lang, "adaptation set", labels, hasAudio)
}

// audioSet reports whether an adaptation set is audio: its contentType is
// audio, or its mimeType (the set's, else its first representation's)
// starts with audio/.
func audioSet(as dash.AdaptationSet) bool {
	if as.ContentType == "audio" {
		return true
	}
	mt := as.MimeType
	if mt == "" && len(as.Representations) > 0 {
		mt = as.Representations[0].MimeType
	}
	return strings.HasPrefix(mt, "audio/")
}

// period resolves a period's representations and reads their inits.
func (c *dashCapture) period(ctx context.Context, per dash.Period) ([]dashSet, error) {
	var out []dashSet
	for asi, as := range per.AdaptationSets {
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
			out = append(out, dashSet{set: asi, align: as.SegmentAlignment, rep: rep, tpl: tpl, segs: tpl.Timeline.Resolve(), in: in})
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
	// Keyed: the first video frame is a keyframe at S@t. An open-GOP segment
	// starts earlier (leading pictures), so it is not compared.
	fv := firstVideo(seg.frames)
	c.startKey[segID{s.rep.ID, sg.T}] = fv.video && fv.key && fv.pts == media.SampleTime(int64(sg.T), int(s.tpl.Timescale))
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

// ReadTS reads a continuous MPEG-TS stream (HTTP-TS, or SRT for an srt://
// URL, see readSRT) with one demuxer that follows PMT changes. It reads until
// the stream ends, or until ReadOptions.Duration of media (by DTS) has been
// captured. The whole stream is one epoch with no signalled start, so
// the stream is joined mid-epoch: neither config rule applies.
func ReadTS(ctx context.Context, u string, opt ReadOptions) (Capture, error) {
	if pu, err := url.Parse(u); err == nil && strings.EqualFold(pu.Scheme, "srt") {
		return readSRT(ctx, pu, opt)
	}
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
	return checkTS(ctx, body, opt, false, fail)
}

// RuleTSPSI names the rule that a TS stream carries a PAT and a PMT before
// its first frame: within psiMaxBytes and the read timeout. A stream that
// ends before its first byte is a read error instead.
const RuleTSPSI = "ts-psi-present"

// psiMaxBytes bounds the bytes read before the first PMT: room for a
// third-party stream that repeats its tables rarely at a high bitrate; the
// read timeout bounds the wait in time.
const psiMaxBytes = 32 << 20

// psiGuard bounds the read up to the first PMT, while armed.
type psiGuard struct {
	r        io.Reader
	wait     time.Duration
	deadline time.Time
	armed    bool
	n        int
	exceeded bool // the byte or time budget ran out
	eof      bool // the stream ended
}

func (g *psiGuard) Read(b []byte) (int, error) {
	if g.armed && (g.n >= psiMaxBytes || time.Now().After(g.deadline)) {
		g.exceeded = true
		return 0, errors.New("no PAT and PMT within the budget")
	}
	n, err := g.r.Read(b)
	g.n += n
	if errors.Is(err, io.EOF) {
		g.eof = true
	}
	return n, err
}

// checkTS demuxes a TS byte stream and runs the frame checks. fail wraps a
// read or demux error with the source and any stall. With strict, the
// stream's own continuity and start are checked too (RuleTSContinuity,
// RuleTSStart).
func checkTS(ctx context.Context, body io.Reader, opt ReadOptions, strict bool, fail func(error) error) (Capture, error) {
	var sniff *sniffReader
	if strict {
		sniff = &sniffReader{r: body}
		body = sniff
	}
	// The wait for the first PMT is bounded in time and bytes: a stream that
	// keeps sending but never carries PSI would not stall.
	wait := opt.Timeout
	if wait <= 0 {
		wait = defaultTimeout
	}
	guard := &psiGuard{r: body, wait: wait, deadline: time.Now().Add(wait), armed: true}
	d, err := ts.NewDemuxerWithOptions(guard, ts.DemuxOptions{FollowPMT: true})
	guard.armed = false
	if err != nil {
		if ctx.Err() != nil {
			return Capture{}, fmt.Errorf("conform: read: %w", ctx.Err())
		}
		if guard.eof && guard.n == 0 {
			return Capture{}, fail(errors.New("the stream ended before any byte"))
		}
		if guard.exceeded || guard.eof {
			return Capture{Violations: []Violation{{
				Rule: RuleTSPSI, Index: -1,
				Detail: fmt.Sprintf("no PAT and PMT in the first %d bytes, or within %v", guard.n, wait),
			}}}, nil
		}
		return Capture{}, fail(err)
	}
	vpid, haveVideo := firstVideoPID(d)
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
	c := b.finish()
	if strict {
		c.Violations = append(c.Violations, strictTS(d, sniff, vpid, haveVideo, c.Frames)...)
	}
	return c, nil
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
