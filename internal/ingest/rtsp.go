package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtsp"
)

const (
	schemeRTSP  = "rtsp"
	schemeRTSPS = "rtsps"

	// rtspMaxOpenPackets bounds the RTP packets queued while Open waits for
	// sender reports.
	rtspMaxOpenPackets = 2048
	// rtspMaxOpenBytes bounds the RTP payload bytes queued meanwhile.
	rtspMaxOpenBytes = 16 << 20
)

// rtspSyncWait is how long Open waits for a sender report on every track
// before it lets frames out (the tests change it).
var rtspSyncWait = time.Second

// errRTSPClosed is returned by Read after Close.
var errRTSPClosed = errors.New("ingest: rtsp: closed")

// parseRTSP checks an rtsp:// URL: a host, an optional port and an optional
// transport parameter.
func parseRTSP(u *url.URL, q url.Values) (Spec, error) {
	if u.Hostname() == "" {
		return Spec{}, errors.New("no host")
	}
	if u.Port() != "" {
		if err := checkPort(u.Port()); err != nil {
			return Spec{}, err
		}
	}
	if v, ok := q["transport"]; ok {
		if len(v) > 1 {
			return Spec{}, errors.New("transport given more than once")
		}
		if _, err := rtspTransport(v[0]); err != nil {
			return Spec{}, err
		}
	}
	return Spec{Live: true}, nil
}

// rtspTransport normalises a transport parameter value.
func rtspTransport(v string) (string, error) {
	switch s := strings.ToLower(v); s {
	case "tcp", "udp", "auto":
		return s, nil
	}
	return "", fmt.Errorf("unsupported transport %q", v)
}

// rtspDialURL returns rawURL without our transport parameter, and the
// transport it named (tcp by default). The rest of the URL is untouched.
func rtspDialURL(rawURL string) (dial, transport string) {
	transport = "tcp"
	base, frag, hasFrag := strings.Cut(rawURL, "#")
	head, query, hasQuery := strings.Cut(base, "?")
	if hasQuery {
		var keep []string
		for _, pair := range strings.Split(query, "&") {
			k, v, _ := strings.Cut(pair, "=")
			if name, err := url.QueryUnescape(k); err == nil {
				k = name
			}
			if k == "transport" {
				// The same decoded value parse checked.
				if dv, err := url.QueryUnescape(v); err == nil {
					if tr, err := rtspTransport(dv); err == nil {
						transport = tr
					}
				}
				continue
			}
			keep = append(keep, pair)
		}
		base = head
		if len(keep) > 0 {
			base += "?" + strings.Join(keep, "&")
		}
	}
	if hasFrag {
		base += "#" + frag
	}
	return base, transport
}

// rtspTrack is the reading state of one converted track.
type rtspTrack struct {
	dep rtsp.Depacketizer
	seq rtsp.SeqTracker

	// ssrc is the SSRC of the latest RTP packet read, for mapping sender
	// reports to the track; handled is the one of the latest packet
	// processed, which differs from it while packets sit in the open queue.
	ssrc, handled       uint32
	hasSSRC, hasHandled bool
	sr                  bool // a sender report arrived (Open's wait)
}

// rtspEvent is a frame or a discontinuity, in the order Read returns them.
type rtspEvent struct {
	frame Frame
	disc  bool
}

type rtspCounters struct {
	lost, dups, resets, skippedPackets, foreignSR, restartDropped atomic.Uint64
}

// rtspPull is an RTSP pull input: one session for the reader's life. It
// ends as the other pull inputs do: when the server ends the session or
// stops sending (the client's idle timeout), Read returns what the
// depacketizers hold, then io.ErrUnexpectedEOF, and the pipeline retries or
// fails over.
//
// Only the goroutine calling Read (Open's before it returns) reads the
// client and drives the depacketizers and rtpFrames; Kinds, Stats and Close
// are safe concurrently with it.
type rtspPull struct {
	rawURL string
	spec   Spec
	now    func() time.Time // the Env clock
	cl     *rtsp.Client
	frames *rtpFrames
	tracks []*rtspTrack // by media index; nil when not converted
	kinds  []media.Kind

	// skipped maps a stats key to 1 for each media that is not converted.
	skipped map[string]uint64
	// restarted holds the tracks that restarted in the current camera
	// restart.
	restarted map[int]bool
	episode   time.Time // the arrival of the first restart in it

	// The reading goroutine's own state.
	pending      []rtsp.Packet // RTP read while Open waited, in order
	pendingBytes int
	events       []rtspEvent
	err          error // the end of the session, returned once the events are out

	n      rtspCounters
	closed atomic.Bool
}

func openRTSP(ctx context.Context, rawURL string, p parsed, env Env) (Reader, error) {
	dialURL, transport := rtspDialURL(rawURL)
	cl, err := rtsp.Dial(ctx, dialURL, rtsp.Config{Transport: transport, Now: env.now()})
	if err != nil {
		return nil, fmt.Errorf("ingest: rtsp: open %s: %w", Redact(rawURL), err)
	}
	r := &rtspPull{rawURL: rawURL, spec: p.spec, now: env.now(), cl: cl}
	if err := r.setup(); err != nil {
		_ = cl.Close() //nolint:contextcheck // Close bounds its own TEARDOWN wait; the open ctx does not apply to it.
		return nil, fmt.Errorf("ingest: rtsp: open %s: %w", Redact(rawURL), err)
	}

	// The sender reports, bounded by ctx and the wait; the RTP read meanwhile
	// stays queued.
	stop := context.AfterFunc(ctx, func() { _ = cl.Close() }) //nolint:contextcheck // Close bounds its own TEARDOWN wait; the open ctx does not apply to it.
	err = r.awaitSync(r.now().Add(rtspSyncWait))
	if err != nil {
		err = fmt.Errorf("the session ended before it was in sync: %w", err)
	}
	if !stop() {
		err = ctx.Err()
	}
	if err != nil {
		_ = cl.Close() //nolint:contextcheck // Close bounds its own TEARDOWN wait; the open ctx does not apply to it.
		return nil, fmt.Errorf("ingest: rtsp: open %s: %w", Redact(rawURL), err)
	}
	return r, nil
}

// setup builds the converter and one depacketizer per converted track.
func (r *rtspPull) setup() error {
	desc := r.cl.Description()
	r.frames = newRTPFrames(desc, r.cl.RTPInfo(), &media.ConfigCache{})
	r.kinds = r.frames.kinds()
	r.tracks = make([]*rtspTrack, len(r.kinds))
	r.skipped = map[string]uint64{}
	r.restarted = map[int]bool{}
	converted := 0
	for _, m := range desc.Media {
		if m.Index < 0 || m.Index >= len(r.kinds) {
			continue
		}
		if r.kinds[m.Index] == 0 {
			reason := r.frames.skipReason[m.Index]
			r.skipped[fmt.Sprintf("skipped_track_%d_%s", m.Index, snake(reason))] = 1
			continue
		}
		dep, err := rtsp.NewDepacketizer(m, 0)
		if err != nil {
			return err
		}
		r.tracks[m.Index] = &rtspTrack{dep: dep}
		converted++
	}
	if converted == 0 {
		return errors.New("no supported track")
	}
	return nil
}

// snake lowercases s and joins its words with underscores.
func snake(s string) string {
	var b strings.Builder
	sep := false
	for _, c := range strings.ToLower(s) {
		if unicode.IsLetter(c) || unicode.IsDigit(c) {
			if sep && b.Len() > 0 {
				b.WriteByte('_')
			}
			sep = false
			b.WriteRune(c)
		} else {
			sep = true
		}
	}
	return b.String()
}

// awaitSync reads packets until every converted track has had a sender
// report, deadline has passed, or the open queue is full. RTP packets are
// queued; sender reports are applied at once. With fewer than two converted
// tracks there is nothing to align and it returns at once. The wait ends
// between packets: a silent source holds it until the client's idle timeout
// or the open ctx.
func (r *rtspPull) awaitSync(deadline time.Time) error {
	for !r.synced() && len(r.pending) < rtspMaxOpenPackets &&
		r.pendingBytes < rtspMaxOpenBytes && r.now().Before(deadline) {
		p, err := r.cl.ReadPacket()
		if err != nil {
			return err
		}
		r.observe(p)
		if p.RTCP != nil {
			r.handleRTCP(p)
			continue
		}
		r.pending = append(r.pending, p)
		r.pendingBytes += len(p.RTP.Payload)
	}
	return nil
}

// synced reports whether Open need not wait any longer: every converted
// track has had a sender report, or there is nothing to align.
func (r *rtspPull) synced() bool {
	conv, waiting := 0, 0
	for _, t := range r.tracks {
		if t == nil {
			continue
		}
		conv++
		if !t.sr {
			waiting++
		}
	}
	return conv < 2 || waiting == 0
}

// observe records the SSRC of an RTP packet's track.
func (r *rtspPull) observe(p rtsp.Packet) {
	if p.RTP == nil || p.Track < 0 || p.Track >= len(r.tracks) || r.tracks[p.Track] == nil {
		return
	}
	t := r.tracks[p.Track]
	t.ssrc, t.hasSSRC = p.RTP.SSRC, true
}

// srTrack maps a sender report to its track: the track whose RTP carries the
// SR's SSRC; failing that, the track the report came on, but only until
// that track has shown an RTP SSRC (after which a report with another SSRC
// is a stale or foreign one and is not applied).
func (r *rtspPull) srTrack(p rtsp.Packet, sr rtsp.SenderReport) (int, bool) {
	for i, t := range r.tracks {
		if t != nil && t.hasSSRC && t.ssrc == sr.SSRC {
			return i, true
		}
	}
	if p.Track < 0 || p.Track >= len(r.tracks) || r.tracks[p.Track] == nil || r.tracks[p.Track].hasSSRC {
		return 0, false
	}
	return p.Track, true
}

// handleRTCP applies the sender reports of an RTCP packet.
func (r *rtspPull) handleRTCP(p rtsp.Packet) {
	for _, sr := range rtsp.ParseSenderReports(p.RTCP) {
		i, ok := r.srTrack(p, sr)
		if !ok {
			r.n.foreignSR.Add(1)
			continue
		}
		if sr.NTP.After(ntpEpoch) {
			r.tracks[i].sr = true
		}
		if r.frames.senderReport(i, sr) {
			r.disc()
		}
	}
}

// disc queues a discontinuity, once for a run of them.
func (r *rtspPull) disc() {
	if n := len(r.events); n > 0 && r.events[n-1].disc {
		return
	}
	r.events = append(r.events, rtspEvent{disc: true})
}

// handleRTP runs an RTP packet through the sequence tracker and the
// depacketizer of its track.
func (r *rtspPull) handleRTP(p rtsp.Packet) {
	if p.Track < 0 || p.Track >= len(r.tracks) || r.tracks[p.Track] == nil {
		r.n.skippedPackets.Add(1)
		return
	}
	t := r.tracks[p.Track]
	gap, reset, dup := t.seq.Next(p.RTP.SequenceNumber, p.RTP.SSRC)
	if dup {
		r.n.dups.Add(1)
		return
	}
	if gap > 0 {
		r.n.lost.Add(uint64(gap))
	}
	restarted := reset && t.hasHandled && t.handled != p.RTP.SSRC
	if reset {
		r.n.resets.Add(1)
	}
	switch {
	case restarted:
		// A new SSRC is a restart, not a loss: the pending AU belongs to the
		// old timeline and goes out (an unfinished fragment is lost by
		// Flush itself), and the new stream's first AU is not marked.
		old := t.dep.Flush()
		if r.joinsRestart(p.Track, p.Arrival) {
			// Another track restarted just before, and its discontinuity
			// is out: this track's old AU would land after it.
			r.n.restartDropped.Add(uint64(len(old)))
		} else {
			r.pushAUs(old)
		}
		if r.frames.restart(p.Track) {
			r.disc()
		}
		if len(r.restarted) == 0 {
			r.episode = p.Arrival
		}
		r.restarted[p.Track] = true
	case gap > 0 || reset:
		t.dep.Gap()
	}
	t.handled, t.hasHandled = p.RTP.SSRC, true
	r.pushAUs(t.dep.Push(p.RTP, p.Arrival))
}

// rtspRestartEpisode is how long after the first track restarted another
// track's restart still counts as the same camera restart. The tracks of a
// camera restart together, within the delivery skew of a few packets. Frames
// cannot end an episode: the restarted track's first new AU is emitted at
// once, and the old frames of the track still to restart are what it is
// there to drop.
const rtspRestartEpisode = 500 * time.Millisecond

// joinsRestart reports whether another track restarted before track in the
// same restart episode, by packet arrival time. A track restarting again
// starts a new episode.
func (r *rtspPull) joinsRestart(track int, at time.Time) bool {
	if r.restarted[track] || at.Sub(r.episode) > rtspRestartEpisode {
		clear(r.restarted)
	}
	return len(r.restarted) > 0
}

// pushAUs converts access units and queues their frames. A bad AU is
// counted by rtpFrames and skipped; the session goes on.
func (r *rtspPull) pushAUs(aus []rtsp.AU) {
	for _, au := range aus {
		frames, disc, _ := r.frames.push(au)
		if disc {
			r.disc()
		}
		for _, f := range frames {
			r.events = append(r.events, rtspEvent{frame: f})
		}
	}
}

// step takes the next packet, from the open queue or the client, and queues
// what it completes. At the end of the session it flushes the
// depacketizers and records the end: a clean end becomes
// io.ErrUnexpectedEOF.
func (r *rtspPull) step() {
	var p rtsp.Packet
	if len(r.pending) > 0 {
		p = r.pending[0]
		r.pending[0] = rtsp.Packet{}
		r.pending = r.pending[1:]
		if p.RTP != nil {
			r.pendingBytes -= len(p.RTP.Payload)
		}
	} else {
		var err error
		if p, err = r.cl.ReadPacket(); err != nil {
			for _, t := range r.tracks {
				if t != nil {
					r.pushAUs(t.dep.Flush())
				}
			}
			if errors.Is(err, io.EOF) || errors.Is(err, rtsp.ErrIdle) {
				err = io.ErrUnexpectedEOF
			}
			r.err = err
			return
		}
		r.observe(p)
	}
	if p.RTCP != nil {
		r.handleRTCP(p)
		return
	}
	if p.RTP != nil {
		r.handleRTP(p)
	}
}

// Read implements Reader.
func (r *rtspPull) Read() (Frame, error) {
	for {
		if r.closed.Load() {
			return Frame{}, errRTSPClosed
		}
		if len(r.events) > 0 {
			e := r.events[0]
			r.events[0] = rtspEvent{}
			r.events = r.events[1:]
			if e.disc {
				return Frame{}, ErrDiscontinuity
			}
			return e.frame, nil
		}
		if r.err != nil {
			return Frame{}, fmt.Errorf("ingest: rtsp: read %s: %w", Redact(r.rawURL), r.err)
		}
		r.step()
	}
}

// Kinds implements Reader: one entry per media of the session, 0 for the
// media that are not converted.
func (r *rtspPull) Kinds() []media.Kind { return append([]media.Kind(nil), r.kinds...) }

// Spec implements Reader.
func (r *rtspPull) Spec() Spec { return r.spec }

// Stats implements Reader: the rtpFrames counters, the packet counters
// (loss is counted here, from the sequence numbers, and nowhere else), the
// client's, and one skipped_track_<index>_<reason> per media that is not
// converted.
func (r *rtspPull) Stats() map[string]uint64 {
	out := r.frames.stats()
	cs := r.cl.Stats()
	out["lost_packets"] = r.n.lost.Load()
	out["duplicate_packets"] = r.n.dups.Load() + cs.DupPackets
	out["resets"] = r.n.resets.Load()
	out["reordered_packets"] = cs.Reordered
	out["dropped_packets"] = cs.Dropped
	out["skipped_packets"] = r.n.skippedPackets.Load()
	out["restart_dropped_aus"] = r.n.restartDropped.Load()
	out["foreign_sr"] = r.n.foreignSR.Load()
	out["fallback_tcp"] = cs.FallbackTCP
	out["reauth"] = cs.Reauth
	out["resyncs"] = cs.Resyncs
	out["skipped_tracks"] = uint64(len(r.skipped))
	for k, v := range r.skipped {
		out[k] = v
	}
	return out
}

// Close implements Reader. It closes the session, unblocking a pending
// Read.
func (r *rtspPull) Close() error {
	if r.closed.Swap(true) {
		return nil
	}
	_ = r.cl.Close()
	return nil
}
