package transcode

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/transcode/wire"
)

// errVersion means a child's Hello announced another wire version. It is
// fatal for Run: a respawn would start the same binary.
var errVersion = errors.New("transcode: child speaks another wire version")

// readHello reads the child's first message and checks it: its type, then
// its version. It only reads and checks. A read error comes back wrapped
// (so wire.ErrMalformed and the socket's errors survive errors.Is); a first
// message that is not a Hello is an error; another version is errVersion
// with both versions, which is fatal for Run.
func readHello(r *wire.Reader) (wire.Hello, error) {
	m, err := r.Read()
	if err != nil {
		return wire.Hello{}, fmt.Errorf("transcode: hello: %w", err)
	}
	h, ok := m.(wire.Hello)
	if !ok {
		return wire.Hello{}, fmt.Errorf("transcode: hello: first message is %T", m)
	}
	if h.Version != wire.Version {
		return wire.Hello{}, fmt.Errorf("%w: %d, want %d", errVersion, h.Version, wire.Version)
	}
	return h, nil
}

// helloFailed maps an error of readHello, other than errVersion, to why the
// host gave up on the child: nil when the socket only ended or the child has
// exited (the exit is the event), the read deadline to "no Hello within
// timeout", and anything else as it is.
func helloFailed(err error, exited bool, timeout time.Duration) error {
	switch {
	case socketEnded(err) || exited:
		return nil
	case errors.Is(err, os.ErrDeadlineExceeded):
		return fmt.Errorf("transcode: hello: none within %v", timeout)
	default:
		return err // readHello's: "transcode: hello: …"
	}
}

// backoff is the respawn delay: min, doubling up to max, back to min after
// a child ran for healthy.
type backoff struct{ min, max, healthy, cur time.Duration }

// next returns the delay before the respawn of a child that ran for uptime
// (0 for one that did not start).
func (b *backoff) next(uptime time.Duration) time.Duration {
	if b.cur <= 0 || uptime >= b.healthy {
		b.cur = b.min
	}
	d := b.cur
	b.cur = min(2*b.cur, b.max)
	return d
}

// waitOrKill is the tail of a stop: it waits for done up to stopGrace, on a
// real timer (not Options.After, which tests fake), then calls kill. It
// returns after the kill, without waiting for done again: a caller that
// must return only once the child is reaped waits on done itself. kill must
// be safe once the process is reaped.
func waitOrKill(done <-chan struct{}, kill func()) {
	timer := time.NewTimer(stopGrace)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
	}
	kill()
}

// shotRetry is the retry rule for key-only thumbnails: one early retry per
// run of failures. A child that is sent only key frames reports each shot
// it could not make in its Stats; the first reported failure that follows a
// success, or the child's start, resets the caller's schedule, so the next
// key frame is offered. While failures stay consecutive the schedule
// stands: a key that never decodes alone costs one attempt per interval,
// not one per key frame.
type shotRetry struct {
	thumbs, failed uint64 // the child's cumulative counters, as last seen
	afterSuccess   bool   // no failure since the last success or the child's start
}

// newShotRetry returns the rule as it stands at a child's start.
func newShotRetry() shotRetry { return shotRetry{afterSuccess: true} }

// look compares the child's cumulative counters with the last seen and
// reports whether the caller should reset its schedule. When both rose
// since the last look, the success is counted first.
func (r *shotRetry) look(thumbs, failed uint64) (reset bool) {
	if thumbs > r.thumbs {
		r.afterSuccess = true
	}
	if failed > r.failed && r.afterSuccess {
		r.afterSuccess = false
		reset = true
	}
	r.thumbs, r.failed = thumbs, failed
	return reset
}

// childExited puts the rule back to a child's start: the next child counts
// from zero.
func (r *shotRetry) childExited() { *r = newShotRetry() }
