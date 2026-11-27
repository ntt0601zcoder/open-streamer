// Package srtout serves streams to SRT receivers.
//
// A Server owns one gosrt listener. A receiver (a caller in request mode)
// names the stream in its stream ID, either in the access-control form
// #!::r=<app>/<name>[,m=request][,t=stream] (no m means request; a t other
// than stream is refused; unknown keys are ignored) or as the plain
// <app>/<name>, where app is Options.App (default live) and name is looked
// up by the Server's lookup. The decision is made from the connection
// request, before the handshake completes: an empty stream ID (as a
// version-4 handshake has), a mode other than request, an encrypted
// request for a stream without a passphrase (or for no stream), an unknown
// name, a stream that is not live yet (hub.Live) and every cap are refused
// with the same reason, srt.REJ_RESOURCE, and no detail. A caller that
// retransmits its request after a refusal may be counted twice in Stats.
// Stream IDs are taken literally, never percent-decoded: ffmpeg 6.1 sends a
// URL's streamid value undecoded (ffmpeg 8 decodes it), so ffmpeg callers
// pass the ID raw (?streamid=#!::r=live/name) or with its -streamid option;
// URI parsers that treat # as a fragment need their own stream ID setting.
//
// # Passphrases
//
// With Options.Secure, a stream may require a passphrase: Secure gives the
// stream's hub and its passphrase from one reading of its state. A request
// for a live stream with a passphrase is refused with srt.REJ_UNSECURE when
// it is not encrypted and with srt.REJ_BADSECRET when its passphrase is
// wrong (gosrt's SetPassphrase fails), so that a player can tell its
// customer which; both count in Stats.RejectedSecret. The two reasons tell
// someone without the passphrase that the name is live. A peer key (the
// per-IP cap's) with 5 wrong passphrases inside 10 s has its encrypted
// requests refused unchecked, with srt.REJ_RESOURCE (Stats.RejectedLimited),
// until 10 s after its first counted failure; players behind one NAT share
// that key. At most 4096 keys are tracked. gosrt yields a request only after
// its cookie exchange, so the address is the peer's. An admitted connection
// carries the passphrase it was admitted under and asks Secure again once
// it has subscribed to the hub, before its first byte: a different
// passphrase, a different hub or no stream ends it as a closed hub would,
// so that no connection admitted under an old passphrase outlives a change
// (the caller also ends the hub's "srt " subscribers once, for those
// already subscribed). Keys are replaced every Options.KMRefresh packets
// (gosrt's default 2^24). A stream without a passphrase is served exactly
// as without Secure.
//
// Each accepted connection streams MPEG-TS through internal/output/tsstream
// with a subscription that holds the hub's cached GOP
// (hub.SubscribeCached), so it starts at the cached keyframe after one
// second of stream time, and resumes at a keyframe after hub loss. The
// listener sends 1316-byte payloads (7 TS packets), and every gosrt Write
// gets at most one payload, never an empty slice, so a chunk is queued whole
// or not at all. A caller whose MSS is smaller than 1360 bytes makes gosrt
// lower the connection's payload to its MSS less 44 bytes, and gosrt would
// split a larger Write over packets; such a connection's chunks are the
// whole TS packets that fit (6 at an MSS of 1200). A connection where fewer
// than 4 fit (an MSS under 796) is closed as soon as it is accepted and
// counted in Stats.RejectedMSS: gosrt decides the MSS in the handshake, so
// the caller sees its connection close rather than a rejection. gosrt
// answers a full send queue with io.EOF and drops the chunk; the same chunk
// is then retried every 5 ms (counted in Stats.SendQueueFull), and a
// connection whose retries last longer than Options.StallTimeout is ended
// as stalled.
//
// Writes are paced by a token bucket per connection, so that a GOP replay
// or a large keyframe leaves at a few times the stream's bitrate rather than
// at line rate, which on a WAN link would queue it past a 120 ms latency.
// The bucket's rate is 2.5 times the stream's bitrate R, clamped to
// [2 Mbit/s, 256 packets per 5 ms], and its depth 50 ms at that rate, at
// least 32 packets. R is taken at the join from the payload of the frames
// buffered before output starts (the hub's cached GOP and what followed
// it) over their DTS span, then from the TS bytes written over the last 2 s
// of stream time; the window starts over after a gap of more than 1 s and
// after hub loss, and the rate in force holds until it is covered again.
// Because of the 2 Mbit/s floor, a viewer on a link slower than 2 Mbit/s
// may still get a join faster than its link carries, and see it corrupt at
// 120 ms latency. On top of the bucket, writes never exceed 256 packets
// per 5 ms, so that no burst overflows a gosrt receiver's 1024-packet read
// queue. A pause only slows this connection's loop; the hub skips frames
// for it if it falls behind.
//
// Every connection has a reader goroutine whose Read returning (the peer's
// shutdown, gosrt's peer idle timeout, Close) ends the connection, so a
// receiver that vanishes is ended within Options.PeerIdleTimeout even while
// the hub is quiet; anything a receiver sends is discarded. A stats poller
// samples the connection every 500 ms and ends it as stalled when, over the
// last StallTimeout, the sender dropped more than 5 % of the packets it sent
// (too late to be delivered), or no ACK came while data was sent. A receiver
// whose application stops reading keeps ACKing while its own gosrt drops the
// data: that is invisible here, and harmless to the server.
//
// A connection that ends on a codec change or the hub closing stays open for
// twice the receiver's TSBPD delay plus 100 ms first, at most 5 s, so that
// its last packets are delivered, or retransmitted, before the shutdown.
package srtout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/datarhei/gosrt"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/output/tsstream"
)

const (
	defaultApp            = "live"
	defaultLatency        = 120 * time.Millisecond
	defaultStallTimeout   = 5 * time.Second
	defaultQueue          = 1024
	defaultMaxPerStream   = 256
	defaultMaxPerListener = 1024
	defaultMaxPerIP       = 64

	// payloadSize is the listener's payload size: 7 TS packets.
	payloadSize = 7 * tsPacket
	// minChunk is the smallest chunk a connection is served with: 4 TS
	// packets.
	minChunk = 4 * tsPacket
	tsPacket = 188
	// gosrtHeaders is what gosrt takes off a connection's MSS for its
	// payload: the SRT header and the IPv4 and UDP headers.
	gosrtHeaders = srt.SRT_HEADER_SIZE + srt.UDP_HEADER_SIZE
	// retryInterval is how often a chunk that found the send queue full is
	// retried.
	retryInterval = 5 * time.Millisecond
	// pacePackets is the most packets written per paceWindow: the
	// ceiling over the token bucket.
	pacePackets = 256
	paceWindow  = 5 * time.Millisecond
	// The token bucket: its rate is bucketFactor times the stream's
	// bitrate within [bucketFloor bit/s, the ceiling], and its depth
	// bucketDepthTime at that rate, at least bucketMinPackets packets.
	bucketFactor     = 2.5
	bucketFloor      = 2e6
	bucketDepthTime  = 50 * time.Millisecond
	bucketMinPackets = 32
	// minSleep is the debt, in time at the bucket's rate, under which a
	// write does not wait.
	minSleep = time.Millisecond
	// rateWindow is the stream time the bitrate is averaged over.
	rateWindow = 2 * media.ClockRate
	// newTimeline is how far a frame's DTS may run back, or ahead, before
	// the bitrate average starts over: a gap (an outage, a failover) would
	// otherwise count as stream time without bytes.
	newTimeline = media.ClockRate
	// maxDrain bounds how long a connection that ended cleanly stays open.
	maxDrain = 5 * time.Second
	// statsInterval is how often the stall rule samples a connection.
	statsInterval = 500 * time.Millisecond
	// maxDropPercent is the share of the packets sent that the sender may
	// drop over StallTimeout.
	maxDropPercent = 5
	// accessControl prefixes a stream ID in the access-control form.
	accessControl = "#!::"
)

// ErrServerClosed is returned by Serve, and by Listen, after Close.
var ErrServerClosed = errors.New("srtout: server closed")

// Why a connection's context was cancelled.
var (
	errStalled  = errors.New("srtout: receiver stalled")
	errGone     = errors.New("srtout: receiver gone")
	errShutdown = errors.New("srtout: server closed")
	errDone     = errors.New("srtout: connection ended")
	// errRevoked ends a connection whose stream, asked again after the
	// subscription, no longer has the hub or the passphrase it was
	// admitted under; it counts as the hub closing.
	errRevoked = errors.New("srtout: admission revoked")
)

// Options tunes a Server. Zero values select the defaults.
type Options struct {
	// App is the first part of a stream ID's resource, slashes trimmed; ""
	// means "live".
	App string
	// Latency is the SRT latency; 0 means 120 ms.
	Latency time.Duration
	// StallTimeout is the window of the stall rule and the longest a
	// chunk is retried on a full send queue; 0 means 5 s.
	StallTimeout time.Duration
	// PeerIdleTimeout ends a connection from which nothing arrived for that
	// long; 0 means gosrt's default (5 s).
	PeerIdleTimeout time.Duration
	// Queue is the smallest subscription queue of a connection; it grows to
	// the cached frames plus 64 (hub.SubscribeCached). 0 means 1024.
	Queue int
	// MaxPerStream, MaxPerListener and MaxPerIP bound the connections per
	// stream, on the listener and per remote IP (an IPv6 address by its
	// /64); 0 means 256, 1024 and 64.
	MaxPerStream, MaxPerListener, MaxPerIP int
	// Secure, when set, replaces New's lookup: it returns the hub of the
	// stream served as name and the passphrase its SRT callers must present
	// ("" for none), both from one reading of the stream's state (R-T9). It
	// is called in the accept loop for each request, and once more by each
	// connection after it subscribed; it must not block.
	Secure func(name string) (h *hub.Hub, passphrase string, ok bool)
	// KMRefresh is the number of packets after which a connection's key is
	// replaced; 0 means gosrt's default (1<<24). The new key is announced
	// KMRefresh/4 packets before. Tests lower it.
	KMRefresh uint64
}

// Stats counts a Server's connections.
type Stats struct {
	// Conns is the number of connections being served.
	Conns uint64
	// Rejected counts refused connection requests, the sum of the other
	// Rejected counters: RejectedMode those with an empty stream ID, a mode
	// other than request, a type other than stream or encryption with no
	// passphrase to check; RejectedUnknown those naming no live stream of
	// the app; RejectedCaps those over a cap; RejectedMSS those whose MSS
	// leaves room for fewer than 4 TS packets per SRT packet (accepted,
	// then closed at once).
	Rejected, RejectedUnknown, RejectedMode, RejectedCaps, RejectedMSS uint64
	// RejectedSecret counts requests refused for a missing or wrong
	// passphrase; RejectedLimited those refused unchecked because their
	// peer key had 5 wrong passphrases in the last 10 s.
	RejectedSecret, RejectedLimited uint64
	// Frames and Resyncs sum the frames written and the hub losses resynced
	// by each connection, added when the connection ends: a connection
	// being served is not counted yet.
	Frames, Resyncs uint64
	// SendQueueFull counts chunks that found gosrt's send queue full and
	// were retried.
	SendQueueFull uint64
	// Why connections ended: stalled (the stall rule, or retries longer
	// than StallTimeout), gone (the receiver's shutdown or idle timeout), a
	// codec change, the hub closing, Close, and a muxer error (a codec TS
	// cannot carry).
	EndedStalled, EndedGone, EndedCodecChange, EndedHubClosed, EndedShutdown, EndedError uint64
}

// Server serves hub streams to SRT receivers on one listener. It is safe for
// concurrent use.
type Server struct {
	lookup func(name string) (*hub.Hub, bool)
	opt    Options
	ctx    context.Context // cancelled by Close with errShutdown
	cancel context.CancelCauseFunc
	full   atomic.Uint64
	conns  sync.WaitGroup

	mu        sync.Mutex
	ln        srt.Listener
	closed    bool
	perStream map[string]int
	perIP     map[string]int
	total     int
	stats     Stats    // all but SendQueueFull
	fails     failures // wrong passphrases by peer key

	// Seams of the tests, set before Serve. wrap stands between each
	// connection's writer and gosrt. request stands between the listener
	// and answer. serving is called by each connection before it
	// subscribes, with the passphrase it was admitted under; the func it
	// returns, when not nil, is called when its stream has ended.
	wrap    func(io.Writer) io.Writer
	request func(srt.ConnRequest) srt.ConnRequest
	serving func(passphrase string) func()
}

// New returns a Server for the hubs that lookup finds by stream name;
// Options.Secure, when set, replaces lookup, which may then be nil.
func New(lookup func(name string) (*hub.Hub, bool), opt Options) *Server {
	opt.App = strings.Trim(opt.App, "/")
	if opt.App == "" {
		opt.App = defaultApp
	}
	if opt.Latency <= 0 {
		opt.Latency = defaultLatency
	}
	if opt.StallTimeout <= 0 {
		opt.StallTimeout = defaultStallTimeout
	}
	if opt.PeerIdleTimeout <= 0 {
		opt.PeerIdleTimeout = srt.DefaultConfig().PeerIdleTimeout
	}
	if opt.Queue <= 0 {
		opt.Queue = defaultQueue
	}
	if opt.MaxPerStream <= 0 {
		opt.MaxPerStream = defaultMaxPerStream
	}
	if opt.MaxPerListener <= 0 {
		opt.MaxPerListener = defaultMaxPerListener
	}
	if opt.MaxPerIP <= 0 {
		opt.MaxPerIP = defaultMaxPerIP
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	return &Server{
		lookup: lookup, opt: opt, ctx: ctx, cancel: cancel,
		perStream: map[string]int{}, perIP: map[string]int{}, fails: newFailures(),
	}
}

// Listen binds the server's gosrt listener on addr (host:port, UDP); Addr is
// valid once it returns. A Server listens once.
func (s *Server) Listen(addr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrServerClosed
	}
	if s.ln != nil {
		return fmt.Errorf("srtout: listen %s: already listening on %s", addr, s.ln.Addr())
	}
	cfg := srt.DefaultConfig()
	cfg.Latency = s.opt.Latency
	cfg.PayloadSize = payloadSize
	cfg.PeerIdleTimeout = s.opt.PeerIdleTimeout
	if s.opt.KMRefresh != 0 {
		cfg.KMRefreshRate = s.opt.KMRefresh
		cfg.KMPreAnnounce = s.opt.KMRefresh / 4
	}
	ln, err := srt.Listen("srt", addr, cfg)
	if err != nil {
		return fmt.Errorf("srtout: listen %s: %w", addr, err)
	}
	s.ln = ln
	return nil
}

// Addr returns the listener's address, or nil before Listen.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Serve answers connection requests until Close, then returns
// ErrServerClosed.
func (s *Server) Serve() error {
	s.mu.Lock()
	ln, closed := s.ln, s.closed
	s.mu.Unlock()
	switch {
	case closed:
		return ErrServerClosed
	case ln == nil:
		return errors.New("srtout: serve: not listening")
	}
	for {
		req, err := ln.Accept2()
		if err != nil {
			if s.ctx.Err() != nil {
				return ErrServerClosed
			}
			return fmt.Errorf("srtout: accept: %w", err)
		}
		if s.request != nil {
			req = s.request(req)
		}
		s.answer(req)
	}
}

// Close stops the listener, ends every connection and waits for them. It
// is idempotent.
func (s *Server) Close() error {
	s.mu.Lock()
	ln := s.ln
	first := !s.closed
	s.closed = true
	s.mu.Unlock()
	if first {
		// Cancelled before the listener closes the connections, so that
		// they end as shutdown, not as gone.
		s.cancel(errShutdown)
		if ln != nil {
			ln.Close()
		}
	}
	s.conns.Wait()
	return nil
}

// Stats returns the counters.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	st := s.stats
	s.mu.Unlock()
	st.SendQueueFull = s.full.Load()
	return st
}

// refusal is why a connection request is refused.
type refusal uint8

const (
	refuseMode refusal = iota
	refuseUnknown
	refuseCaps
	refuseMSS
	refuseSecret
	refuseLimited
)

// answer accepts or refuses one connection request.
func (s *Server) answer(req srt.ConnRequest) {
	h, name, pass, ok := s.stream(req)
	if !ok {
		return
	}
	key := peerKey(req.RemoteAddr())
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		req.Reject(srt.REJ_RESOURCE)
		return
	}
	if s.total >= s.opt.MaxPerListener || s.perStream[name] >= s.opt.MaxPerStream || s.perIP[key] >= s.opt.MaxPerIP {
		s.countRefusal(refuseCaps)
		s.mu.Unlock()
		req.Reject(srt.REJ_RESOURCE)
		return
	}
	s.total++
	s.perStream[name]++
	s.perIP[key]++
	s.stats.Conns++
	s.conns.Add(1)
	s.mu.Unlock()
	conn, err := req.Accept()
	if err != nil {
		s.finish(name, key, nil, tsstream.Stats{})
		return
	}
	var st srt.Statistics
	conn.Stats(&st)
	size := chunkSize(st.Instantaneous.ByteMSS)
	if size == 0 {
		_ = conn.Close()
		s.mu.Lock()
		s.countRefusal(refuseMSS)
		s.mu.Unlock()
		s.finish(name, key, nil, tsstream.Stats{})
		return
	}
	go s.serve(conn, h, name, key, pass, size)
}

// chunkSize returns the bytes a connection's gosrt Write may carry so that
// it is one SRT packet: gosrt's payload is the connection's MSS less 44
// bytes, capped by the listener's 1316, and a chunk is the whole TS packets
// that fit. It is 0, refusing the connection, when fewer than 4 fit.
func chunkSize(mss uint64) int {
	if mss <= gosrtHeaders {
		return 0
	}
	n := min(payloadSize, int(mss-gosrtHeaders)/tsPacket*tsPacket) //nolint:gosec // an MSS is at most 1500
	if n < minChunk {
		return 0
	}
	return n
}

// stream resolves the stream a request names and checks its passphrase, or
// refuses it. It returns the passphrase the request was admitted under (""
// for none). The order keeps the counts of a stream without a passphrase:
// a bad stream ID, then encryption with no passphrase to check against, are
// mode refusals; an unknown or not-live name is next; only then is a
// passphrase asked for and checked.
func (s *Server) stream(req srt.ConnRequest) (h *hub.Hub, name, pass string, ok bool) {
	refuse := func(why refusal, reason srt.RejectionReason) (*hub.Hub, string, string, bool) {
		s.mu.Lock()
		s.countRefusal(why)
		s.mu.Unlock()
		req.Reject(reason)
		return nil, "", "", false
	}
	res, ok := parseStreamID(req.StreamId())
	if !ok {
		return refuse(refuseMode, srt.REJ_RESOURCE)
	}
	name, named := strings.CutPrefix(res, s.opt.App+"/")
	known := false
	if named && name != "" && !strings.Contains(name, "/") {
		h, pass, known = s.find(name)
	}
	if !known {
		h, pass = nil, ""
	}
	encrypted := req.IsEncrypted()
	if encrypted && pass == "" {
		return refuse(refuseMode, srt.REJ_RESOURCE)
	}
	if h == nil || !h.Live() {
		return refuse(refuseUnknown, srt.REJ_RESOURCE)
	}
	if pass == "" {
		return h, name, "", true
	}
	if !encrypted {
		return refuse(refuseSecret, srt.REJ_UNSECURE)
	}
	key := s.fails.key(req.RemoteAddr())
	s.mu.Lock()
	limited := s.fails.limited(key)
	s.mu.Unlock()
	if limited {
		return refuse(refuseLimited, srt.REJ_RESOURCE)
	}
	// The check runs outside s.mu: one PBKDF2 and a key unwrap.
	if req.SetPassphrase(pass) != nil {
		s.mu.Lock()
		s.fails.fail(key)
		s.mu.Unlock()
		return refuse(refuseSecret, srt.REJ_BADSECRET)
	}
	return h, name, pass, true
}

// find looks name up: through Options.Secure, which gives the passphrase
// too, when it is set, else through New's lookup.
func (s *Server) find(name string) (*hub.Hub, string, bool) {
	if s.opt.Secure != nil {
		return s.opt.Secure(name)
	}
	h, ok := s.lookup(name)
	return h, "", ok
}

// countRefusal counts a refused request; s.mu is held.
func (s *Server) countRefusal(why refusal) {
	s.stats.Rejected++
	switch why {
	case refuseMode:
		s.stats.RejectedMode++
	case refuseUnknown:
		s.stats.RejectedUnknown++
	case refuseCaps:
		s.stats.RejectedCaps++
	case refuseMSS:
		s.stats.RejectedMSS++
	case refuseSecret:
		s.stats.RejectedSecret++
	case refuseLimited:
		s.stats.RejectedLimited++
	}
}

// parseStreamID returns the resource a stream ID names; ok is false for an
// empty ID and for an access-control ID whose mode is not request or whose
// type is not stream. Keys are case-sensitive and unknown ones are ignored.
func parseStreamID(id string) (resource string, ok bool) {
	if id == "" {
		return "", false
	}
	rest, ac := strings.CutPrefix(id, accessControl)
	if !ac {
		return id, true
	}
	for kv := range strings.SplitSeq(rest, ",") {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "r":
			resource = v
		case "m":
			if v != "request" {
				return "", false
			}
		case "t":
			if v != "stream" {
				return "", false
			}
		}
	}
	return resource, true
}

// peerKey is the key the per-IP cap counts a remote address under: an IPv4
// address as is (an IPv4-mapped IPv6 address unmapped), and an IPv6 address
// by its /64, which one host normally owns. An address that is not an IP
// falls back to its host part, or its whole string.
func peerKey(a net.Addr) string {
	var ip netip.Addr
	if u, ok := a.(*net.UDPAddr); ok {
		ip, _ = netip.AddrFromSlice(u.IP)
	} else if host, _, err := net.SplitHostPort(a.String()); err == nil {
		if ip, _ = netip.ParseAddr(host); !ip.IsValid() {
			return host
		}
	}
	if !ip.IsValid() {
		return a.String()
	}
	ip = ip.Unmap().WithZone("")
	if ip.Is4() {
		return ip.String()
	}
	return netip.PrefixFrom(ip, 64).Masked().String()
}

// end is why a connection ended.
type end uint8

const (
	endShutdown end = iota
	endGone
	endStalled
	endCodecChange
	endHubClosed
	endError
)

// serve streams h to one accepted connection, in chunks of size bytes,
// until it must end, then closes it and counts the end. With
// Options.Secure, the stream is asked again once the connection has
// subscribed, before its first byte: a different hub, a passphrase other
// than pass (the one it was admitted under) or no stream ends it with
// errRevoked.
func (s *Server) serve(conn srt.Conn, h *hub.Hub, name, key, pass string, size int) {
	ctx, cancel := context.WithCancelCause(s.ctx)
	defer cancel(errDone)
	var bg sync.WaitGroup
	bg.Go(func() {
		// Read returns once gosrt closes the connection: the peer's
		// shutdown, its idle timeout, or our own Close below.
		buf := make([]byte, 1500)
		for {
			if _, err := conn.Read(buf); err != nil {
				cancel(errGone)
				return
			}
		}
	})
	bg.Go(func() { s.watch(ctx, cancel, conn) })

	var out io.Writer = conn
	if s.wrap != nil {
		out = s.wrap(conn)
	}
	w := newWriter(ctx, cancel, out, size, s.opt.StallTimeout, &s.full)
	if secure := s.opt.Secure; secure != nil {
		// Called by the writer from this connection's stream, never under
		// s.mu: tsstream subscribes before its first Meter or Sink call.
		w.admit = func() bool {
			cur, p, ok := secure(name)
			return ok && cur == h && p == pass
		}
	}
	var ended func()
	if s.serving != nil {
		ended = s.serving(pass)
	}
	st, err := tsstream.Stream(ctx, h, "srt "+conn.RemoteAddr().String(), w, tsstream.Options{Queue: s.opt.Queue, Meter: w})
	if ended != nil {
		ended()
	}
	why := classify(ctx, st, err)
	if why == endCodecChange || why == endHubClosed {
		t := time.NewTimer(drainTime(conn, s.opt.Latency))
		select {
		case <-ctx.Done():
		case <-t.C:
		}
		t.Stop()
	}
	cancel(errDone)
	_ = conn.Close() // ends the reader
	bg.Wait()
	s.finish(name, key, &why, st)
}

// drainTime is how long a connection whose stream ended cleanly stays open
// before it is closed: the receiver delivers a packet its TSBPD delay (the
// larger of both sides' latencies) after it was sent and drops what it has
// not delivered when the shutdown comes, so the last packets get twice that
// delay plus 100 ms to be delivered, or retransmitted; at most 5 s, since a
// caller may ask for up to 65 s of latency.
func drainTime(conn srt.Conn, latency time.Duration) time.Duration {
	var st srt.Statistics
	conn.Stats(&st)
	d := max(time.Duration(st.Instantaneous.MsSendTsbPdDelay)*time.Millisecond, latency) //nolint:gosec // milliseconds of a 16-bit field
	return min(2*d+100*time.Millisecond, maxDrain)
}

// classify reports why a connection's stream ended.
func classify(ctx context.Context, st tsstream.Stats, err error) end {
	switch st.Ended {
	case tsstream.EndedCodecChange:
		return endCodecChange
	case tsstream.EndedClosed:
		return endHubClosed
	}
	if ctx.Err() != nil {
		switch cause := context.Cause(ctx); {
		case errors.Is(cause, errShutdown):
			return endShutdown
		case errors.Is(cause, errStalled):
			return endStalled
		case errors.Is(cause, errGone):
			return endGone
		case errors.Is(cause, errRevoked):
			return endHubClosed
		}
	}
	if err != nil {
		return endError
	}
	return endShutdown
}

// finish releases a connection's cap slots and counts its end; why is nil
// for a connection that was never served.
func (s *Server) finish(name, key string, why *end, st tsstream.Stats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.conns.Done()
	s.total--
	if s.perStream[name]--; s.perStream[name] == 0 {
		delete(s.perStream, name)
	}
	if s.perIP[key]--; s.perIP[key] == 0 {
		delete(s.perIP, key)
	}
	s.stats.Conns--
	if why == nil {
		return
	}
	s.stats.Frames += st.Frames
	s.stats.Resyncs += st.Resyncs
	switch *why {
	case endShutdown:
		s.stats.EndedShutdown++
	case endGone:
		s.stats.EndedGone++
	case endStalled:
		s.stats.EndedStalled++
	case endCodecChange:
		s.stats.EndedCodecChange++
	case endHubClosed:
		s.stats.EndedHubClosed++
	case endError:
		s.stats.EndedError++
	}
}

// watch samples conn's statistics every 500 ms and cancels ctx with
// errStalled when the stall rule holds, until ctx ends.
func (s *Server) watch(ctx context.Context, cancel context.CancelCauseFunc, conn srt.Conn) {
	t := time.NewTicker(statsInterval)
	defer t.Stop()
	m := stallMonitor{window: s.opt.StallTimeout}
	var st srt.Statistics
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			conn.Stats(&st)
			a := st.Accumulated
			if m.add(now, sample{sent: a.PktSent, drop: a.PktSendDrop, acks: a.PktRecvACK}) {
				cancel(errStalled)
				return
			}
		}
	}
}

// sample is a connection's accumulated counters at one time: packets sent
// (retransmissions included), packets the sender dropped, ACKs received.
type sample struct {
	at               time.Time
	sent, drop, acks uint64
}

// stallMonitor applies the stall rule to a connection's samples.
type stallMonitor struct {
	window  time.Duration
	samples []sample // oldest first; the first is the newest at least a window old
}

// add records the counters at now and reports whether the connection
// stalled over the last window: data was sent and either the sender
// dropped more than 5 % of it or no ACK came. Nothing is decided before a
// whole window has been sampled.
func (m *stallMonitor) add(now time.Time, c sample) bool {
	c.at = now
	m.samples = append(m.samples, c)
	cut := now.Add(-m.window)
	base := -1
	for i, smp := range m.samples {
		if smp.at.After(cut) {
			break
		}
		base = i
	}
	if base < 0 {
		return false
	}
	m.samples = m.samples[base:]
	b := m.samples[0]
	sent := c.sent - b.sent
	if sent == 0 {
		return false // nothing sent: an outage, not a stall
	}
	return (c.drop-b.drop)*100 > sent*maxDropPercent || c.acks == b.acks
}

// writer is the tsstream.Sink and tsstream.Meter of a connection: it hands
// gosrt at most one chunk (one SRT packet) per Write, paces the packets by
// the stream's bitrate and retries a chunk that found the send queue full.
type writer struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	w      io.Writer
	size   int // the chunk size: whole TS packets in one SRT packet
	stall  time.Duration
	full   *atomic.Uint64

	winStart   time.Time // the current ceiling window
	winPackets int       // packets written in it
	bucket     *bucket   // nil until the bitrate is known: only the ceiling applies
	meter      rateMeter

	// admit, when set, is asked once, at the first Prebuffered, Frame or
	// Write, whether the connection may still be served; false cancels it
	// with errRevoked before any byte is written.
	admit func() bool
}

func newWriter(ctx context.Context, cancel context.CancelCauseFunc, w io.Writer, size int, stall time.Duration, full *atomic.Uint64) *writer {
	return &writer{ctx: ctx, cancel: cancel, w: w, size: size, stall: stall, full: full}
}

// Write writes p in chunks of at most w.size bytes. It fails when the
// connection ends or stalls; n is then the bytes queued before.
func (w *writer) Write(p []byte) (int, error) {
	if !w.admitted() || w.ctx.Err() != nil {
		return 0, fmt.Errorf("srtout: write: %w", context.Cause(w.ctx))
	}
	for off := 0; off < len(p); {
		n := min(w.size, len(p)-off)
		if err := w.pace(n); err != nil {
			return off, err
		}
		if err := w.chunk(p[off : off+n]); err != nil {
			return off, err
		}
		off += n
	}
	return len(p), nil
}

// admitted asks w.admit, the first time only, and cancels the connection
// with errRevoked when it says no; it reports false then, and true
// otherwise.
func (w *writer) admitted() bool {
	if w.admit == nil {
		return true
	}
	ok := w.admit()
	w.admit = nil
	if !ok {
		w.cancel(errRevoked)
	}
	return ok
}

// SetWriteDeadline is a no-op: gosrt has no write deadline, and the stall
// rule bounds a connection instead.
func (w *writer) SetWriteDeadline(time.Time) error { return nil }

// Flush is a no-op: every chunk is queued by Write.
func (w *writer) Flush() error { return nil }

// Prebuffered sets the bucket's rate from the payload buffered before the
// stream's output starts; it implements tsstream.Meter.
func (w *writer) Prebuffered(bytes int, span int64) {
	if !w.admitted() {
		return
	}
	w.setRate(w.meter.prebuffered(bytes, span))
}

// Frame follows the TS bytes written, and sets the bucket's rate once 2 s
// of stream time have been seen; it implements tsstream.Meter.
func (w *writer) Frame(dts int64, bytes int) {
	if !w.admitted() {
		return
	}
	if r, ok := w.meter.frame(dts, bytes); ok {
		w.setRate(r)
	}
}

// Resync starts a new bitrate window after the hub skipped frames for this
// connection, which would otherwise count as stream time without bytes; the
// rate in force holds until the window is covered. It implements
// tsstream.Meter.
func (w *writer) Resync() { w.meter.reset() }

// setRate paces by a stream bitrate (bit/s, 0 when unknown).
func (w *writer) setRate(bitrate float64) {
	now := time.Now()
	if w.bucket == nil {
		w.bucket = newBucket(bitrate, now)
		return
	}
	w.bucket.setRate(bitrate, now)
}

// pace waits until a packet of n bytes may go: for the next ceiling window
// when the current one already holds pacePackets packets, and for the
// bucket's tokens.
func (w *writer) pace(n int) error {
	if err := w.ceiling(); err != nil {
		return err
	}
	if w.bucket == nil {
		return nil
	}
	if d := w.bucket.take(time.Now(), n); d > 0 {
		return w.sleep(d)
	}
	return nil
}

// ceiling waits, when the current window already holds pacePackets
// packets, for the next window.
func (w *writer) ceiling() error {
	now := time.Now()
	if now.Sub(w.winStart) >= paceWindow {
		w.winStart, w.winPackets = now, 0
	}
	if w.winPackets >= pacePackets {
		if err := w.sleep(w.winStart.Add(paceWindow).Sub(now)); err != nil {
			return err
		}
		w.winStart, w.winPackets = time.Now(), 0
	}
	w.winPackets++
	return nil
}

// chunk queues b, at most one SRT packet, retrying it while the send queue is
// full. gosrt reports a full queue, and a closed connection, with io.EOF:
// while the connection's context is not done it is the queue.
func (w *writer) chunk(b []byte) error {
	var since time.Time
	for {
		_, err := w.w.Write(b)
		if err == nil {
			return nil
		}
		if w.ctx.Err() != nil {
			return fmt.Errorf("srtout: write: %w", context.Cause(w.ctx))
		}
		if !errors.Is(err, io.EOF) {
			return fmt.Errorf("srtout: write: %w", err)
		}
		now := time.Now()
		if since.IsZero() {
			since = now
			// gosrt closing on its own just before the reader cancels
			// can count one spurious full queue here.
			w.full.Add(1)
		} else if now.Sub(since) >= w.stall {
			w.cancel(errStalled)
			return fmt.Errorf("srtout: write: %w", errStalled)
		}
		if err := w.sleep(retryInterval); err != nil {
			return err
		}
	}
}

// sleep waits d, or fails when the connection's context ends first.
func (w *writer) sleep(d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-w.ctx.Done():
		return fmt.Errorf("srtout: write: %w", context.Cause(w.ctx))
	case <-t.C:
		return nil
	}
}

// bucketRate is the bucket's rate, in bytes per second, for a stream of
// bitrate bit/s: 2.5 times it, within [2 Mbit/s, 256 packets per 5 ms].
func bucketRate(bitrate float64) float64 {
	ceil := float64(pacePackets*payloadSize) / paceWindow.Seconds()
	return min(max(bucketFactor*bitrate/8, bucketFloor/8), ceil)
}

// bucketDepth is the bucket's depth, in bytes, at rate bytes per second:
// 50 ms of it, at least 32 packets.
func bucketDepth(rate float64) float64 {
	return max(rate*bucketDepthTime.Seconds(), bucketMinPackets*payloadSize)
}

// bucket is a token bucket in bytes. Taking more than it holds runs a debt
// that the caller pays by waiting.
type bucket struct {
	rate, depth, tokens float64 // bytes per second, bytes, bytes
	last                time.Time
}

// newBucket returns a full bucket for a stream of bitrate bit/s.
func newBucket(bitrate float64, now time.Time) *bucket {
	b := &bucket{last: now}
	b.setRate(bitrate, now)
	b.tokens = b.depth
	return b
}

// setRate changes the stream bitrate; the tokens are kept, up to the new
// depth.
func (b *bucket) setRate(bitrate float64, now time.Time) {
	b.refill(now)
	b.rate = bucketRate(bitrate)
	b.depth = bucketDepth(b.rate)
	b.tokens = min(b.tokens, b.depth)
}

func (b *bucket) refill(now time.Time) {
	if el := now.Sub(b.last); el > 0 {
		b.tokens = min(b.depth, b.tokens+el.Seconds()*b.rate)
		b.last = now
	}
}

// take spends n bytes at now and returns how long to wait before sending
// them: zero while the debt is under 1 ms at the rate.
func (b *bucket) take(now time.Time, n int) time.Duration {
	b.refill(now)
	b.tokens -= float64(n)
	if b.tokens >= 0 {
		return 0
	}
	d := time.Duration(-b.tokens / b.rate * float64(time.Second))
	if d < minSleep {
		return 0
	}
	return d
}

// rateMeter estimates a stream's bitrate.
type rateMeter struct {
	samples []rateSample // oldest first; the first is the newest at least a window old
	newest  int64        // the largest DTS seen on this timeline
	started bool
}

// rateSample is the TS bytes of a frame written when the stream's clock
// (the largest DTS so far) read t.
type rateSample struct {
	t int64
	n int
}

// prebuffered returns the bitrate of payload bytes over span 90 kHz ticks,
// with the TS packet overhead (184 payload bytes per 188); 0 when the span
// is not positive.
func (m *rateMeter) prebuffered(bytes int, span int64) float64 {
	if span <= 0 {
		return 0
	}
	return float64(bytes) * mpegTSOverhead * 8 * media.ClockRate / float64(span)
}

// mpegTSOverhead is the TS packet size over its largest payload.
const mpegTSOverhead = 188.0 / 184

// reset starts a new window; the caller keeps the last rate until it is
// covered.
func (m *rateMeter) reset() {
	m.samples, m.started = m.samples[:0], false
}

// frame records n TS bytes at dts and returns the bitrate over the last
// 2 s of stream time; ok is false until the window is covered. A DTS more
// than 1 s behind or ahead of the stream's clock starts a new window.
func (m *rateMeter) frame(dts int64, n int) (float64, bool) {
	if m.started && (dts < m.newest-newTimeline || dts > m.newest+newTimeline) {
		m.reset()
	}
	if !m.started || dts > m.newest {
		m.newest, m.started = dts, true
	}
	m.samples = append(m.samples, rateSample{t: m.newest, n: n})
	base := -1
	for i, smp := range m.samples {
		if smp.t > m.newest-rateWindow {
			break
		}
		base = i
	}
	if base < 0 {
		return 0, false
	}
	m.samples = m.samples[base:]
	bytes := 0
	for _, smp := range m.samples[1:] {
		bytes += smp.n
	}
	return float64(bytes) * 8 * media.ClockRate / float64(m.newest-m.samples[0].t), true
}
