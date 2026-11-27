package stream

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/logging"
	"github.com/ntt0601zcoder/open-streamer/internal/metrics"
	"github.com/ntt0601zcoder/open-streamer/internal/output/live"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
)

// Outcome is what an edit did to one stream.
type Outcome struct {
	Name    string `json:"name"`
	Applied string `json:"applied"` // "created", "started", "stopped", "unchanged", "outputs", "live", "rebuild", "deleted"
}

// What an edit can do to a stream.
const (
	appliedCreated   = "created"   // the name is new
	appliedStarted   = "started"   // it existed without a supervisor and has one now
	appliedStopped   = "stopped"   // it changed and has no supervisor now: disabled
	appliedUnchanged = "unchanged" // the spec is the same
	appliedOutputs   = "outputs"   // a running stream's outputs were edited in place
	appliedLive      = "live"      // a running stream's inputs, watermark or audio language were edited in place
	appliedRebuild   = "rebuild"   // a running stream is rebuilt
	appliedDeleted   = "deleted"   // the name is gone
)

// entry is one stream of the set. It is immutable: an edit replaces it.
type entry struct {
	sp     spec.Stream
	sup    *Supervisor        // nil: the stream is disabled, or was left stopped at load
	cancel context.CancelFunc // sup's; nil without one
	err    string             // why it was left stopped, at load or at the last edit that tried it; "" otherwise
	errAt  time.Time
	cause  error // err's errors, joined; nil without err
}

// Stopped is an enabled stream that has no supervisor because it cannot
// run: Err is why, and errors.Is finds ErrInvalid and ErrConflict in it.
type Stopped struct {
	Name string
	Err  error
}

// failed returns the entry of a stream that is left stopped for errs.
func failed(sp spec.Stream, errs []*editError) *entry {
	msgs := make([]string, len(errs))
	for i, err := range errs {
		msgs[i] = err.msg
	}
	return &entry{sp: sp, err: strings.Join(msgs, "; "), errAt: time.Now(), cause: joined(errs)}
}

// stoppedOf returns the streams of set that are left stopped, by name.
func stoppedOf(set map[string]*entry) []Stopped {
	var out []Stopped
	for _, name := range slices.Sorted(maps.Keys(set)) {
		if e := set[name]; e.cause != nil {
			out = append(out, Stopped{Name: name, Err: e.cause})
		}
	}
	return out
}

// Stopped returns the enabled streams that have no supervisor because they
// cannot run, sorted by name: the ones Load left stopped that no edit has
// started since, each with the reason of the last time it was judged.
func (m *Manager) Stopped() []Stopped { return stoppedOf(*m.streams.Load()) }

// Manager owns the set of streams: one supervisor per stream that runs, the
// listen claims between them and the server, and the lookups of the output
// servers.
//
// # Locks
//
// Edits (Load, Replace, and the start of supervisors in both) are serialised
// by edit. Readers never take it: the set is an immutable map behind an
// atomic pointer, which an edit replaces with a changed copy as its last
// step. So a lookup, a status request and a scrape never wait for an edit,
// nor for the file's fsync inside its commit.
//
// # Goroutines
//
// Each supervisor's Run is a goroutine the manager starts and Run waits for.
//
// # Views
//
// No method returns a supervisor's View: View.Outputs.Push holds push URLs
// with their stream keys. The lookups return hubs and packagers, Status and
// Collect redact, and Get and List return the specs as they were given, for
// the callers that own the config file.
type Manager struct {
	env      Env
	reserved []Claim // env.Reserved, normalised

	ctx  context.Context // every supervisor's parent; ended by Run
	stop context.CancelFunc

	streams atomic.Pointer[map[string]*entry]

	edit   sync.Mutex
	closed bool // Run's context has ended: no supervisor is started any more
	// tails holds, per name, the Done of the last supervisor created for it,
	// also after the stream was stopped or deleted: the next supervisor of
	// the name waits for it (R-D5). A closed tail is the same as none, and is
	// dropped at the next edit.
	tails map[string]<-chan struct{}
	wg    sync.WaitGroup // the supervisors' Run goroutines; Add under edit, while not closed
}

// NewManager returns an empty manager. env.Lookup is replaced by the
// manager's CopyHub, and the TLS clients of env.RootCAs are built here, once
// for every supervisor.
func NewManager(env Env) *Manager {
	m := &Manager{tails: map[string]<-chan struct{}{}}
	m.ctx, m.stop = context.WithCancel(context.Background())
	for _, c := range env.Reserved {
		c.Host = normalHost(c.Host)
		if c.Owner == "" {
			c.Owner = serverOwner
		}
		m.reserved = append(m.reserved, c)
	}
	env.Lookup = m.CopyHub
	m.env = env.withClients()
	m.streams.Store(&map[string]*entry{})
	return m
}

// Run returns when ctx ends and every supervisor has stopped. Load and
// Replace may be called before or while it runs. It must be called once:
// the supervisors started before it are stopped by it too.
func (m *Manager) Run(ctx context.Context) {
	<-ctx.Done()
	// Closed under edit before the wait: no supervisor is started after it
	// began.
	m.edit.Lock()
	m.closed = true
	m.edit.Unlock()
	m.stop()
	m.wg.Wait()
}

// start runs a supervisor for e, chained on its name's tail. edit is held,
// and the manager is not closed.
func (m *Manager) start(e *entry) {
	name := e.sp.Name
	ctx, cancel := context.WithCancel(m.ctx)
	sup := NewSupervisor(e.sp, m.env, m.tails[name])
	m.tails[name] = sup.Done()
	e.sup, e.cancel = sup, cancel
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		sup.Run(ctx)
	}()
}

// pruneTails drops the tails that are closed: every supervisor of the name
// has stopped, so its next one waits for nothing. edit is held.
func (m *Manager) pruneTails() {
	for name, tail := range m.tails {
		select {
		case <-tail:
			delete(m.tails, name)
		default:
		}
	}
}

// judge returns why sp, enabled, cannot run beside the claims of held and
// the streams of running, which already holds sp: Check's error, one error
// per claim that conflicts, and its copy:// cycle.
func (m *Manager) judge(sp spec.Stream, held []Claim, running map[string]spec.Stream) []*editError {
	var errs []*editError
	if err := Check(sp, m.env); err != nil {
		errs = append(errs, invalid(sp.Name, err))
	}
	errs = append(errs, claimErrors(sp.Name, claimsOf(sp), held)...)
	if err := cycleError(sp.Name, running); err != nil {
		errs = append(errs, err)
	}
	return errs
}

// joined returns errs as one error that errors.Is finds every kind of.
func joined(errs []*editError) error {
	out := make([]error, len(errs))
	for i, e := range errs {
		out[i] = e
	}
	return errors.Join(out...)
}

// Load installs the start-up set, leniently (R-D4): in name order, a stream
// that fails Check, whose claims conflict with the server's or an earlier
// stream's, or that closes a copy:// cycle with earlier streams, is kept in
// the set without a supervisor, state "stopped", the reason as its LastError
// and in the returned list, one item per such stream, sorted by name. Every
// later edit tries such a stream again (see Replace). It is called once, on
// an empty manager; the error is ErrClosed, or that it is not empty.
func (m *Manager) Load(streams []spec.Stream) ([]Stopped, error) {
	m.edit.Lock()
	defer m.edit.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	if len(*m.streams.Load()) > 0 {
		return nil, errors.New("stream: load: the manager is not empty")
	}
	sorted := slices.Clone(streams)
	slices.SortFunc(sorted, func(a, b spec.Stream) int { return cmp.Compare(a.Name, b.Name) })
	next := make(map[string]*entry, len(sorted))
	held := slices.Clone(m.reserved)
	running := map[string]spec.Stream{}
	for _, sp := range sorted {
		e := &entry{sp: cloneSpec(sp)}
		next[sp.Name] = e
		if sp.Disabled {
			continue
		}
		running[sp.Name] = e.sp
		if errs := m.judge(e.sp, held, running); len(errs) > 0 {
			delete(running, sp.Name)
			next[sp.Name] = failed(e.sp, errs)
			continue
		}
		held = append(held, claimsOf(e.sp)...)
		m.start(e)
	}
	m.streams.Store(&next)
	return stoppedOf(next), nil
}

// act is what an edit does to one stream's supervisor.
type act uint8

const (
	actKeep   act = iota // the entry stays as it is
	actDrop              // deleted: cancel the supervisor, if any
	actSet               // a new entry without a supervisor; the old supervisor, if any, is cancelled
	actStart             // a new entry with a new supervisor
	actSubmit            // a new entry with the old supervisor, which gets the spec
	actFail              // the entry stays stopped, with the reason of this edit
)

// step is one stream of an edit.
type step struct {
	name    string
	old     *entry      // nil: created
	sp      spec.Stream // the new spec; unset for a deleted stream
	act     act
	applied string
	errs    []*editError // actFail: why it still cannot run
}

// plan sorts every name of the current and the new set into steps, by name,
// and judges the streams the edit creates or changes; then it tries the
// unchanged streams that were left stopped. edit is held.
func (m *Manager) plan(streams []spec.Stream) ([]step, error) {
	cur := *m.streams.Load()
	fresh := make(map[string]spec.Stream, len(streams))
	for _, sp := range streams {
		fresh[sp.Name] = sp
	}
	names := slices.Collect(maps.Keys(cur))
	for name := range fresh {
		if _, ok := cur[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)

	steps := make([]step, 0, len(names))
	held := slices.Clone(m.reserved)    // the server's claims and the holders'
	running := map[string]spec.Stream{} // the streams that have a supervisor after the edit
	var touched []spec.Stream           // created or changed, and enabled: judged below
	var retries []int                   // the steps of the unchanged streams left stopped: tried below
	for _, name := range names {
		old, sp := cur[name], fresh[name]
		_, stays := fresh[name]
		st := step{name: name, old: old}
		had := old != nil && old.sup != nil
		switch {
		case !stays:
			st.act, st.applied = actDrop, appliedDeleted
		case old == nil:
			st.sp, st.act, st.applied = cloneSpec(sp), actStart, appliedCreated
			if sp.Disabled {
				st.act = actSet
			}
		case spec.Diff(old.sp, sp) == spec.ChangeNone:
			st.act, st.applied = actKeep, appliedUnchanged
			switch {
			case had:
				held = append(held, claimsOf(old.sp)...)
				running[name] = old.sp
			case old.err != "" && !old.sp.Disabled:
				// Left stopped because it could not run: what stood in its
				// way may be gone, with this edit or since the last one.
				retries = append(retries, len(steps))
			}
		case sp.Disabled:
			// It is, or stays, without a supervisor.
			st.sp, st.act, st.applied = cloneSpec(sp), actSet, appliedStopped
		case !had:
			st.sp, st.act, st.applied = cloneSpec(sp), actStart, appliedStarted
		default:
			st.sp, st.act = cloneSpec(sp), actSubmit
			switch spec.Diff(old.sp, sp) {
			case spec.ChangeNone, spec.ChangeOutputs: // ChangeNone has its own case above
				st.applied = appliedOutputs
			case spec.ChangeLive:
				st.applied = appliedLive
			case spec.ChangeRebuild:
				st.applied = appliedRebuild
			}
		}
		if st.act == actStart || st.act == actSubmit {
			touched = append(touched, st.sp)
			running[name] = st.sp
		}
		steps = append(steps, st)
	}

	// Judge only what the edit touches: against the server, the holders,
	// itself and the touched streams before it in name order.
	errs := make([]*editError, 0, len(touched))
	for _, sp := range touched {
		errs = append(errs, m.judge(sp, held, running)...)
		held = append(held, claimsOf(sp)...)
	}
	if len(errs) > 0 {
		return nil, joined(errs)
	}

	// The streams left stopped come last, in name order, so none of them
	// takes what the edit itself asks for; one that still cannot run stays
	// stopped with today's reason, and never fails the edit.
	for _, i := range retries {
		st := &steps[i]
		sp := st.old.sp
		running[st.name] = sp
		if errs := m.judge(sp, held, running); len(errs) > 0 {
			delete(running, st.name)
			st.act, st.errs = actFail, errs
			continue
		}
		held = append(held, claimsOf(sp)...)
		st.sp, st.act, st.applied = sp, actStart, appliedStarted
	}
	return steps, nil
}

// apply makes the steps the set, and returns the answers of the supervisors
// that got an edit. edit is held; nothing here waits for a teardown.
func (m *Manager) apply(steps []step) []<-chan spec.Change {
	m.pruneTails()
	// The cancels first: the sooner a stream lets go of its sockets, the
	// sooner a successor of its name builds.
	for _, st := range steps {
		if (st.act == actDrop || st.act == actSet) && st.old != nil && st.old.sup != nil {
			st.old.cancel()
		}
	}
	next := make(map[string]*entry, len(steps))
	var waits []<-chan spec.Change
	for _, st := range steps {
		switch st.act {
		case actKeep:
			next[st.name] = st.old
		case actDrop:
		case actSet:
			next[st.name] = &entry{sp: st.sp}
		case actStart:
			e := &entry{sp: st.sp}
			m.start(e)
			next[st.name] = e
		case actSubmit:
			waits = append(waits, st.old.sup.Submit(st.sp))
			next[st.name] = &entry{sp: st.sp, sup: st.old.sup, cancel: st.old.cancel}
		case actFail:
			next[st.name] = failed(st.old.sp, st.errs)
		}
	}
	m.streams.Store(&next) // last: a reader sees the edit whole
	return waits
}

// Replace makes streams the whole set, all or nothing. It judges the streams
// that are new or changed and enabled: Check, their claims against the
// server's, each other's and those of the streams that have a supervisor and
// are not changed, and their copy:// cycles. On any error it returns every
// one of them, joined (errors.Is finds ErrInvalid and ErrConflict, both when
// both are there), and changes nothing. Otherwise it calls commit, when not
// nil; if commit fails, its error is returned and nothing changed. Then it
// applies the difference and returns one Outcome per name of the old and the
// new set, sorted by name.
//
// Once commit has succeeded, Replace returns a nil error, whatever happens
// to ctx: the edit is in the file and in force, and a caller that saw an
// error would keep stale bookkeeping. ctx only ends the wait for the
// supervisors to apply the edit; they finish on their own.
//
// A stream without a supervisor (disabled, or left stopped at load) holds no
// claims. A disabled one is not judged while it stays as it is; one that
// changes and stays disabled is "stopped". An enabled stream that was left
// stopped because it could not run, and that the edit does not change, is
// tried again by every edit, after the streams the edit touches: judged as
// a changed stream is, it starts ("started") when nothing stands in its way
// any more, and otherwise stays stopped ("unchanged") with the reason
// refreshed, which never fails the edit. The streams have passed Validate
// and have distinct names.
func (m *Manager) Replace(ctx context.Context, streams []spec.Stream, commit func() error) ([]Outcome, error) {
	m.edit.Lock()
	if m.closed {
		m.edit.Unlock()
		return nil, ErrClosed
	}
	steps, err := m.plan(streams)
	if err == nil && commit != nil {
		err = commit()
	}
	if err != nil {
		m.edit.Unlock()
		return nil, err
	}
	waits := m.apply(steps) //nolint:contextcheck // a supervisor outlives the call that created it: it runs under the manager's context
	m.edit.Unlock()

	out := make([]Outcome, len(steps))
	for i, st := range steps {
		out[i] = Outcome{Name: st.name, Applied: st.applied}
	}
	// The outcomes do not depend on the answers. A channel closed without
	// one is a supervisor that stopped first: the manager is closing.
	for _, w := range waits {
		select {
		case <-w:
		case <-ctx.Done():
			return out, nil
		}
	}
	return out, nil
}

// Get returns the spec of the stream name, as it was given: its URLs are
// not redacted.
func (m *Manager) Get(name string) (spec.Stream, bool) {
	e := (*m.streams.Load())[name]
	if e == nil {
		return spec.Stream{}, false
	}
	return cloneSpec(e.sp), true
}

// List returns every stream's spec, sorted by name, as they were given:
// their URLs are not redacted.
func (m *Manager) List() []spec.Stream {
	cur := *m.streams.Load()
	out := make([]spec.Stream, 0, len(cur))
	for _, name := range slices.Sorted(maps.Keys(cur)) {
		out = append(out, cloneSpec(cur[name].sp))
	}
	return out
}

// Status returns the state of the stream name. A stream without a
// supervisor reports state "stopped", with the reason it was left so, if
// any, as its LastError.
func (m *Manager) Status(name string) (Status, bool) {
	e := (*m.streams.Load())[name]
	if e == nil {
		return Status{}, false
	}
	if e.sup != nil {
		return e.sup.Status(), true
	}
	red := e.sp.Redacted()
	st := Status{
		Name:        name,
		State:       stateStopped,
		LastError:   e.err,
		Served:      e.sp.Served(),
		Outputs:     red.Outputs,
		Inputs:      inputsOf(red),
		ActiveInput: -1,
		Tracks:      []Track{},
	}
	if e.err != "" {
		at := e.errAt
		st.LastErrorAt = &at
	}
	return st, true
}

// Refreshed is what RefreshAsset did to one stream.
type Refreshed struct {
	Stream string `json:"stream"`
	OK     bool   `json:"ok"`              // the running picture has the new bytes
	Error  string `json:"error,omitempty"` // scrubbed; "between generations: taken at the next build" for ErrNoGeneration
}

// The Error texts of RefreshAsset that are not a build's error.
const (
	refreshBetween   = "between generations: taken at the next build"
	refreshCancelled = "cancelled"
	refreshStopped   = "stopped: the stream's supervisor has ended"
)

// AssetUsers returns the names of the streams whose spec refers to the asset, sorted: running, disabled or left
// stopped. It cannot see a config entry the start skipped (such an entry never became a stream): the server
// refuses asset deletes while one exists.
func (m *Manager) AssetUsers(name string) []string {
	cur := *m.streams.Load()
	var out []string
	for _, n := range slices.Sorted(maps.Keys(cur)) {
		if slices.Contains(cur[n].sp.Assets(), name) {
			out = append(out, n)
		}
	}
	return out
}

// RefreshAsset tells every stream that has a supervisor and refers to the asset to load it again, waits for
// their answers (or for ctx: the rest are reported with Error "cancelled"), and returns one entry per such
// stream, by name.
//
// Every refresh is asked for before the first wait, so a stream's answer
// does not wait behind another's, and ctx ends only the waits: a refresh
// that was asked for is still done. A supervisor that stops before it
// answers is reported with OK false.
func (m *Manager) RefreshAsset(ctx context.Context, name string) []Refreshed {
	cur := *m.streams.Load()
	var out []Refreshed
	var waits []<-chan error
	for _, n := range slices.Sorted(maps.Keys(cur)) {
		e := cur[n]
		if e.sup == nil || !slices.Contains(e.sp.Assets(), name) {
			continue
		}
		out = append(out, Refreshed{Stream: n})
		waits = append(waits, e.sup.Refresh())
	}
	for i, w := range waits {
		if ctx.Err() != nil {
			out[i].Error = refreshCancelled
			continue
		}
		select {
		case err, ok := <-w:
			switch {
			case !ok:
				out[i].Error = refreshStopped
			case err == nil:
				out[i].OK = true
			case errors.Is(err, ErrNoGeneration):
				out[i].Error = refreshBetween
			default:
				out[i].Error = logging.Scrub(err.Error())
			}
		case <-ctx.Done():
			out[i].Error = refreshCancelled
		}
	}
	return out
}

// Shot returns the latest thumbnail of the stream name and when it arrived.
func (m *Manager) Shot(name string) (picture.Shot, time.Time, bool) {
	e := (*m.streams.Load())[name]
	if e == nil || e.sup == nil {
		return picture.Shot{}, time.Time{}, false
	}
	return e.sup.Shot()
}

// view returns what the stream of a served name serves now: the name is
// split at its first "_", which no stream name has.
func (m *Manager) view(served string) (*View, bool) {
	name, _, _ := strings.Cut(served, "_")
	e := (*m.streams.Load())[name]
	if e == nil || e.sup == nil {
		return nil, false
	}
	return e.sup.View(), true
}

// Hub returns the hub of a served name for the output out, which is httpts,
// rtmp, rtsp or srt; ok is false while the stream does not serve that name
// on that output.
func (m *Manager) Hub(out Output, served string) (*hub.Hub, bool) {
	v, ok := m.view(served)
	if !ok {
		return nil, false
	}
	on := false
	switch out {
	case OutHTTPTS:
		on = v.Outputs.HTTPTS
	case OutRTMP:
		on = v.Outputs.RTMP
	case OutRTSP:
		on = v.Outputs.RTSP
	case OutSRT:
		on = v.Outputs.SRT
	case OutHLS, OutDASH: // served by Packager
	}
	if h := v.Single[served]; on && h != nil {
		return h, true
	}
	return nil, false
}

// SRT returns the hub of a served name for the SRT output and the passphrase
// its callers must present ("" for none), both from one View; ok is false
// while the stream does not serve that name over SRT. It never waits for an
// edit.
func (m *Manager) SRT(served string) (h *hub.Hub, passphrase string, ok bool) {
	v, found := m.view(served)
	if !found || !v.Outputs.SRT {
		return nil, "", false
	}
	if h = v.Single[served]; h == nil {
		return nil, "", false
	}
	return h, v.Outputs.SRTPassphrase, true
}

// Packager returns the packager of a served name for the output out, which
// is hls or dash; ok is false while the stream does not serve that name on
// that output.
func (m *Manager) Packager(out Output, served string) (*live.Packager, bool) {
	v, ok := m.view(served)
	if !ok {
		return nil, false
	}
	on := false
	switch out {
	case OutHLS:
		on = v.Outputs.HLS
	case OutDASH:
		on = v.Outputs.DASH
	case OutHTTPTS, OutRTMP, OutRTSP, OutSRT: // served by Hub
	}
	if p := v.Packs[served]; on && p != nil {
		return p, true
	}
	return nil, false
}

// CopyHub returns what copy://name reads: <stream> is its source hub,
// <stream>_<label> that rendition's hub, whatever outputs are on.
func (m *Manager) CopyHub(name string) (*hub.Hub, bool) {
	v, ok := m.view(name)
	if !ok {
		return nil, false
	}
	h := v.Source
	if strings.Contains(name, "_") {
		h = v.Single[name]
	}
	return h, h != nil
}

// states are the states of openstreamer_streams, in the order written.
var states = []string{stateRunning, stateStarting, stateNoInput, stateBackoff, stateStopped}

// Collect writes openstreamer_streams{state} and every supervisor's series,
// by name. Only the current supervisor of a name is collected: while a
// predecessor tears down, two supervisors have the name, and its series
// written twice would fail the whole scrape.
func (m *Manager) Collect(w *metrics.Writer) {
	cur := *m.streams.Load()
	names := slices.Sorted(maps.Keys(cur))
	count := map[string]int{}
	for _, name := range names {
		if sup := cur[name].sup; sup != nil {
			count[sup.state()]++
		} else {
			count[stateStopped]++
		}
	}
	for _, state := range states {
		w.Gauge("openstreamer_streams", "Streams, by state.", float64(count[state]), metrics.L("state", state))
	}
	for _, name := range names {
		if sup := cur[name].sup; sup != nil {
			sup.Collect(w)
		}
	}
}
