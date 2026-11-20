// Package rtmppush publishes a hub's stream to an RTMP server: a push
// target such as a CDN ingest or a restreaming service.
//
// A Pusher keeps one connection to its target at a time. Each connection
// publishes (rtmp.Publish, with the target's query sent verbatim),
// subscribes to the hub with a queue that holds the whole GOP cache
// (hub.SubscribeCached), so it starts at once on the cached keyframe, and
// keeps one flv.Muxer. A delivery that reports lost frames resyncs the
// muxer, which resumes at the next keyframe with onMetaData and headers on
// the same connection, so a slow target never holds up the hub. A codec
// change (flv.ErrCodecChanged) closes the connection and publishes again at
// once; any other failure (refused, rejected, closed by the target, a write
// that misses the write timeout) closes it and reconnects after an
// exponential backoff, 1 s doubling to 30 s, that starts again at 1 s after
// a connection that sent for 30 s.
//
// The target's stream key, userinfo and query values are secrets: String,
// Stats, errors and the hub subscriber name only ever show the target
// through redact.URL, and error text is scrubbed of them.
package rtmppush

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/mux/flv"
	"github.com/ntt0601zcoder/open-streamer/internal/redact"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
)

const (
	defaultQueue        = 256
	defaultWriteTimeout = 10 * time.Second
	defaultMinBackoff   = time.Second
	defaultMaxBackoff   = 30 * time.Second
	// resetAfter is how long a connection must have sent for the backoff to
	// start again at its minimum.
	resetAfter = 30 * time.Second
	// mask replaces a secret in scrubbed text.
	mask = "xxxxx"
)

// Options tunes a Pusher. Zero values select the defaults.
type Options struct {
	// Queue is the smallest subscription queue; it grows to the cached
	// frames plus 64 (hub.SubscribeCached). 0 means 256.
	Queue int
	// WriteTimeout bounds each write to the target; a write that misses it
	// ends the connection. 0 means 10 s. It overrides RTMP.WriteTimeout.
	WriteTimeout time.Duration
	// MinBackoff and MaxBackoff bound the wait before a reconnect, which
	// doubles from MinBackoff up to MaxBackoff. 0 means 1 s and 30 s.
	MinBackoff, MaxBackoff time.Duration
	// After waits for a backoff. Default time.After.
	After func(time.Duration) <-chan time.Time
	// Now tells how long a connection has sent, to reset the backoff.
	// Default time.Now.
	Now func() time.Time
	// RTMP is the client Config of each connection (Dial and timeouts);
	// WriteTimeout and KeepQuery are set by the Pusher.
	RTMP rtmp.Config
}

// State is what a Pusher is doing.
type State string

// The states of a Pusher.
const (
	StateIdle       State = "idle"       // Run has not started
	StateConnecting State = "connecting" // publishing to the target
	StatePublishing State = "publishing" // sending
	StateBackoff    State = "backoff"    // waiting to reconnect after a failure
	StateStopped    State = "stopped"    // Run has returned
)

// Stats counts a Pusher's connections and frames.
type Stats struct {
	State State
	// Connects counts accepted publishes; Failures the connection attempts
	// and connections that failed (each followed by a backoff); Republishes
	// the connections closed for a codec change and published again at
	// once.
	Connects, Failures, Republishes uint64
	// LastError is the latest failure, with the target redacted.
	LastError string
	// Frames counts the hub frames received, Messages the RTMP messages
	// written.
	Frames, Messages uint64
	// FrameErrors counts frames the muxer could not send (no track config,
	// a kind RTMP does not carry); they are skipped.
	FrameErrors uint64
	// Resyncs counts deliveries that reported lost frames.
	Resyncs uint64
	// DroppedOnRepublish counts frames dropped after a republish because
	// the hub replayed them from before the codec change, or because a
	// keyframe of the new codec was still awaited.
	DroppedOnRepublish uint64
	// Mux sums the muxer counters of every connection.
	Mux flv.MuxStats
}

// Pusher publishes one hub to one RTMP target. Run it once; Stats and
// String are safe from any goroutine.
type Pusher struct {
	h        *hub.Hub
	target   string
	redacted string
	scrubber *strings.Replacer
	opt      Options

	mu      sync.Mutex
	st      Stats
	muxDone flv.MuxStats // the muxer counters of ended connections
	muxCur  flv.MuxStats // the current connection's
}

// New returns a Pusher of h to target, an rtmp://host[:port]/app[/…]/key
// URL; its query is sent to the target verbatim. Errors never contain the
// URL.
func New(h *hub.Hub, target string, opt Options) (*Pusher, error) {
	if h == nil {
		return nil, errors.New("rtmppush: new: no hub")
	}
	if err := checkTarget(target); err != nil {
		return nil, fmt.Errorf("rtmppush: new: %w", err)
	}
	if opt.Queue <= 0 {
		opt.Queue = defaultQueue
	}
	if opt.WriteTimeout <= 0 {
		opt.WriteTimeout = defaultWriteTimeout
	}
	if opt.MinBackoff <= 0 {
		opt.MinBackoff = defaultMinBackoff
	}
	if opt.MaxBackoff <= 0 {
		opt.MaxBackoff = defaultMaxBackoff
	}
	opt.MaxBackoff = max(opt.MaxBackoff, opt.MinBackoff)
	if opt.After == nil {
		opt.After = time.After
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	opt.RTMP.WriteTimeout = opt.WriteTimeout
	opt.RTMP.KeepQuery = true
	return &Pusher{
		h:        h,
		target:   target,
		redacted: redact.URL(target),
		scrubber: scrubber(target),
		opt:      opt,
		st:       Stats{State: StateIdle},
	}, nil
}

// checkTarget accepts rtmp://host[:port]/app[/…]/key. Its errors never
// contain the URL.
func checkTarget(target string) error {
	u, err := url.Parse(target)
	if err != nil {
		return errors.New("invalid URL")
	}
	if u.Scheme != "rtmp" || u.Opaque != "" {
		return errors.New("not an rtmp:// URL")
	}
	if u.Hostname() == "" {
		return errors.New("the URL has no host")
	}
	segs := strings.Split(strings.TrimSuffix(strings.TrimPrefix(u.EscapedPath(), "/"), "/"), "/")
	if len(segs) < 2 || slices.Contains(segs, "") {
		return errors.New("the URL needs an app and a stream key")
	}
	return nil
}

// scrubber replaces the target's secrets in text: the stream key, with its
// query and alone, escaped and not, by its redacted form, and the userinfo
// and every query value by "xxxxx". Strings of up to 2 characters are left
// alone: replacing them would mangle the text.
func scrubber(target string) *strings.Replacer {
	u, err := url.Parse(target)
	if err != nil {
		return strings.NewReplacer()
	}
	var pairs []string
	add := func(secret, by string) {
		if len([]rune(secret)) > 2 {
			pairs = append(pairs, secret, by)
		}
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	escKey := path[strings.LastIndexByte(path, '/')+1:]
	key, err := url.PathUnescape(escKey)
	if err != nil {
		key = escKey
	}
	masked := redact.RTMPKey(key)
	if u.RawQuery != "" {
		add(escKey+"?"+u.RawQuery, masked)
		add(key+"?"+u.RawQuery, masked)
	}
	add(escKey, masked)
	add(key, masked)
	if u.User != nil {
		pw, _ := u.User.Password()
		add(pw, mask)
		add(u.User.Username(), mask)
	}
	for pair := range strings.SplitSeq(u.RawQuery, "&") {
		_, v, _ := strings.Cut(pair, "=")
		add(v, mask)
		if uv, err := url.QueryUnescape(v); err == nil {
			add(uv, mask)
		}
	}
	return strings.NewReplacer(pairs...)
}

// scrub returns err's text without the target's secrets.
func (p *Pusher) scrub(err error) string {
	return p.scrubber.Replace(err.Error())
}

// String returns the target, redacted.
func (p *Pusher) String() string { return p.redacted }

// Stats returns the counters.
func (p *Pusher) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.st
	st.Mux = addMux(p.muxDone, p.muxCur)
	return st
}

func addMux(a, b flv.MuxStats) flv.MuxStats {
	return flv.MuxStats{
		SkippedTracks:     a.SkippedTracks + b.SkippedTracks,
		DroppedBeforeKey:  a.DroppedBeforeKey + b.DroppedBeforeKey,
		DroppedBeforeBase: a.DroppedBeforeBase + b.DroppedBeforeBase,
		DroppedEarlyAudio: a.DroppedEarlyAudio + b.DroppedEarlyAudio,
		Resyncs:           a.Resyncs + b.Resyncs,
		Epochs:            a.Epochs + b.Epochs,
	}
}

func (p *Pusher) setState(s State) {
	p.mu.Lock()
	p.st.State = s
	p.mu.Unlock()
}

// end is how a connection ended.
type end uint8

const (
	endFailed       end = iota // the target or the network failed: back off
	endCodecChanged            // republish at once
	endHubClosed               // the stream is over
	endCanceled                // ctx ended
)

// Run pushes until ctx ends, closing the connection (which unblocks a
// pending write) and returning ctx's error, or until the hub closes, ending
// the publish and returning nil. A hub that closes during a backoff is seen
// at the next connection.
func (p *Pusher) Run(ctx context.Context) error {
	defer p.setState(StateStopped)
	wait := p.opt.MinBackoff
	var rs resume
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("rtmppush: run: %w", err)
		}
		why, sent, err := p.connection(ctx, &rs)
		switch why {
		case endHubClosed:
			return nil
		case endCanceled:
			return fmt.Errorf("rtmppush: run: %w", ctx.Err())
		case endCodecChanged:
			p.mu.Lock()
			p.st.Republishes++
			p.mu.Unlock()
			continue
		case endFailed:
		}
		if sent >= resetAfter {
			wait = p.opt.MinBackoff
		}
		p.mu.Lock()
		p.st.Failures++
		p.st.LastError = p.scrub(err)
		p.st.State = StateBackoff
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return fmt.Errorf("rtmppush: run: %w", ctx.Err())
		case <-p.opt.After(wait):
		}
		wait = min(2*wait, p.opt.MaxBackoff)
	}
}

// connection publishes once and sends until the connection must end. It
// reports why, how long it sent, and for endFailed the error. After a codec
// change rs is set for the next connection.
func (p *Pusher) connection(ctx context.Context, rs *resume) (end, time.Duration, error) {
	p.setState(StateConnecting)
	conn, err := rtmp.Publish(ctx, p.target, p.opt.RTMP)
	if err != nil {
		if ctx.Err() != nil {
			return endCanceled, 0, nil
		}
		return endFailed, 0, fmt.Errorf("rtmppush: publish: %w", err)
	}
	// Closing on ctx's end fails a write blocked on a stalled target.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	defer func() { _ = conn.Close() }()
	start := p.opt.Now()
	p.mu.Lock()
	p.st.Connects++
	p.st.State = StatePublishing
	p.mu.Unlock()
	sub := p.h.SubscribeCached("rtmppush "+p.redacted, p.opt.Queue)
	defer sub.Close()
	c := &pushConn{p: p, conn: conn, mux: flv.NewMuxer(), rs: rs}
	defer c.finish()

	failed := func(err error) (end, time.Duration, error) {
		if ctx.Err() != nil {
			// Our own Close failed the conn: err is no failure.
			return endCanceled, 0, err
		}
		return endFailed, p.opt.Now().Sub(start), err
	}
	for {
		select {
		case <-ctx.Done():
			return endCanceled, 0, nil
		case <-conn.Done():
			return failed(fmt.Errorf("rtmppush: connection: %w", conn.Err()))
		case d, ok := <-sub.C():
			if !ok {
				return endHubClosed, 0, nil
			}
			codecChanged, err := c.deliver(d)
			if err != nil {
				return failed(err)
			}
			if codecChanged {
				return endCodecChanged, 0, nil
			}
		}
	}
}

// pushConn is one connection being sent.
type pushConn struct {
	p     *Pusher
	conn  *rtmp.PublishConn
	mux   *flv.Muxer
	rs    *resume
	video bool // a video frame was handed to the muxer
}

// deliver muxes and writes one delivery. codecChanged reports a codec
// change, after setting the resume point of the next connection; err a
// failed write.
func (c *pushConn) deliver(d hub.Delivery) (codecChanged bool, err error) {
	p := c.p
	f := d.Frame
	if d.Lost > 0 {
		c.mux.Resync()
	}
	pass := c.rs.pass(f)
	var msgs []rtmp.Media
	if pass {
		msgs, err = c.mux.Frame(f)
	}
	p.mu.Lock()
	p.st.Frames++
	if d.Lost > 0 {
		p.st.Resyncs++
	}
	p.muxCur = c.mux.Stats()
	switch {
	case !pass:
		p.st.DroppedOnRepublish++
	case errors.Is(err, flv.ErrCodecChanged):
		codecChanged = true
	case err != nil:
		p.st.FrameErrors++
	}
	p.mu.Unlock()
	if !pass || err != nil {
		if codecChanged {
			*c.rs = resume{active: true, epoch: f.Epoch, video: c.video || f.Kind() == media.KindVideo}
		}
		return codecChanged, nil
	}
	if f.Kind() == media.KindVideo {
		c.video = true
	}
	for _, m := range msgs {
		if err := c.conn.WriteMedia(m); err != nil {
			return false, fmt.Errorf("rtmppush: write: %w", err)
		}
		p.mu.Lock()
		p.st.Messages++
		p.mu.Unlock()
	}
	return false, nil
}

// finish moves the connection's muxer counters to the ended ones.
func (c *pushConn) finish() {
	p := c.p
	p.mu.Lock()
	defer p.mu.Unlock()
	p.muxDone = addMux(p.muxDone, c.mux.Stats())
	p.muxCur = flv.MuxStats{}
}

// resume is where the connection after a codec change starts. The hub
// replays its cache, which can still hold frames from before the change
// (the GOP the change interrupted); sending them would carry the old codec
// again and republish once more. So frames of an epoch before the change
// are dropped, and, when the stream has video, everything up to a video
// keyframe of the new epoch, so the muxer starts on it rather than on
// audio as an audio-only connection. A keyframe that does not come within
// hub.LiveWindow of stream time stops the wait.
type resume struct {
	active  bool
	epoch   uint32 // the epoch of the frame that changed the codec
	video   bool   // wait for a video keyframe
	waiting bool   // a frame of the new epoch was dropped awaiting it
	from    int64  // its DTS
}

// pass reports whether f is sent, ending the resume when it is.
func (r *resume) pass(f media.Frame) bool {
	if !r.active {
		return true
	}
	if int32(f.Epoch-r.epoch) < 0 {
		return false
	}
	switch {
	case !r.video, f.Kind() == media.KindVideo && f.Key:
	case !r.waiting:
		r.waiting, r.from = true, f.DTS
		return false
	case f.DTS-r.from < hub.LiveWindow:
		return false
	}
	*r = resume{}
	return true
}
