package server

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/api"
	"github.com/ntt0601zcoder/open-streamer/internal/config"
	"github.com/ntt0601zcoder/open-streamer/internal/logging"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
	"github.com/ntt0601zcoder/open-streamer/internal/stream"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
)

// staleLogEvery is the shortest time between two logs of a config file that
// cannot be read for the stale check.
const staleLogEvery = 10 * time.Second

// loadError is one thing the start skipped or left stopped. name is the
// stream Manager.Load left stopped; "" for an entry the lenient parse
// skipped.
type loadError struct{ name, text string }

// state is the text in force and its status, published as one immutable
// value. status.Stale is never set here: ConfigStatus asks the file.
type state struct {
	text   []byte
	status api.ConfigStatus
}

// publish makes the fields cfg guards the snapshot the readers see. The
// caller holds s.cfg, or is New.
func (s *Server) publish() {
	st := &state{
		text: s.applied,
		status: api.ConfigStatus{
			ETag:            config.Hash(s.applied),
			LoadedAt:        s.loadedAt,
			LoadErrors:      make([]string, len(s.loadErrs)),
			LastReloadAt:    s.lastReloadAt,
			LastReloadError: s.lastReloadErr,
			RestartRequired: slices.Clone(s.restart),
		},
	}
	for i, e := range s.loadErrs {
		st.status.LoadErrors[i] = e.text
	}
	s.state.Store(st)
}

// apply makes text the config in force. write says whether the text came
// over the API (and must be written) or from the file (a reload). The
// caller holds s.cfg.
//
// The parse is strict, so a text that still holds an entry the start
// skipped is refused with that entry's errors, whichever way it came; and
// it runs every stream's Validate, which is where a whole-config PUT of a
// document that came back redacted (the mask where a stream key was) is
// refused: nothing before this sees that text.
func (s *Server) apply(ctx context.Context, text []byte, write bool) (api.ApplyResult, error) {
	p, err := config.Parse(text, s.parseOpt)
	if err != nil {
		return api.ApplyResult{}, err
	}
	// commit runs inside Replace, after every stream was judged.
	commit := func() error {
		if !write || bytes.Equal(text, s.applied) {
			return nil
		}
		return s.file.Write(text) // ErrStale if the file changed since the stale check
	}
	out, err := s.Manager().Replace(ctx, p.Doc.Streams, commit)
	if err != nil {
		return api.ApplyResult{}, err // nothing applied, nothing written
	}
	// After a successful commit Replace never fails, so from here on the
	// bookkeeping always runs, also when ctx has ended: the client hanging
	// up changes nothing.
	if !write {
		s.file.Adopt(text)
	}
	// Only a changed users section replaces the set: an unchanged one
	// keeps its remembered logins, so an edit costs its author no PBKDF2.
	if !s.users.Load().Equal(p.UserList) {
		s.users.Store(p.Users)
	}
	s.applied, s.loadedAt = bytes.Clone(text), time.Now()
	// The text parsed strictly, so no entry is skipped any more; a stream
	// the start left stopped is still so only when the edit left it alone.
	unchanged := map[string]bool{}
	for _, o := range out {
		unchanged[o.Name] = o.Applied == "unchanged"
	}
	s.loadErrs = slices.DeleteFunc(s.loadErrs, func(e loadError) bool { return e.name == "" || !unchanged[e.name] })
	restart := s.started.Diff(p.Doc.Server.Resolved())
	if restart == nil {
		restart = []string{}
	}
	if len(restart) > 0 && (!write || !slices.Equal(restart, s.restart)) {
		s.log.Warn("server: config: restart required: " + strings.Join(restart, ", "))
	}
	s.restart = restart
	s.publish()
	return api.ApplyResult{Streams: out, RestartRequired: slices.Clone(restart), ETag: config.Hash(text)}, nil
}

// stale refuses a write while the file on disk is not the text in force: a
// hand edit that no reload has taken yet. The caller holds s.cfg.
func (s *Server) stale() error {
	stale, err := s.file.Stale()
	if err != nil {
		return fmt.Errorf("server: %w: %w", config.ErrStale, err)
	}
	if stale {
		return config.ErrStale
	}
	return nil
}

// matchStream checks a per-stream If-Match: empty passes; otherwise the
// stream must exist, and the value be * or its ETag.
func (s *Server) matchStream(name, ifMatch string) error {
	if ifMatch == "" {
		return nil
	}
	cur, ok := s.Manager().Get(name)
	if !ok || ifMatch != "*" && ifMatch != cur.ETag() {
		return api.ErrPrecondition
	}
	return nil
}

// PutConfig makes text, the client's exact bytes, the config in force and
// the file's content. The stale check comes before If-Match: a client
// whose ETag is current still must not overwrite a hand edit.
func (s *Server) PutConfig(ctx context.Context, text []byte, ifMatch string) (api.ApplyResult, error) {
	s.cfg.Lock()
	defer s.cfg.Unlock()
	if ifMatch == "" {
		return api.ApplyResult{}, api.ErrPreconditionRequired
	}
	if err := s.stale(); err != nil {
		return api.ApplyResult{}, err
	}
	if ifMatch != config.Hash(s.applied) {
		return api.ApplyResult{}, api.ErrPrecondition
	}
	return s.apply(ctx, text, true)
}

// PutStream creates or replaces one stream: the entry of its name in the
// text in force, also one the start skipped.
func (s *Server) PutStream(ctx context.Context, sp spec.Stream, ifMatch string) (stream.Outcome, error) {
	s.cfg.Lock()
	defer s.cfg.Unlock()
	if err := s.stale(); err != nil {
		return stream.Outcome{}, err
	}
	if err := s.matchStream(sp.Name, ifMatch); err != nil {
		return stream.Outcome{}, err
	}
	text, err := config.SetStream(s.applied, sp)
	if err != nil {
		return stream.Outcome{}, err
	}
	res, err := s.apply(ctx, text, true)
	if err != nil {
		return stream.Outcome{}, err
	}
	for _, o := range res.Streams {
		if o.Name == sp.Name {
			return o, nil
		}
	}
	return stream.Outcome{Name: sp.Name, Applied: "unchanged"}, nil
}

// DeleteStream removes one stream: the entries of its name in the text in
// force, also one the start skipped. A name the text does not have is
// config.ErrNoStream.
func (s *Server) DeleteStream(ctx context.Context, name, ifMatch string) error {
	s.cfg.Lock()
	defer s.cfg.Unlock()
	if err := s.stale(); err != nil {
		return err
	}
	if err := s.matchStream(name, ifMatch); err != nil {
		return err
	}
	text, err := config.DeleteStream(s.applied, name)
	if err != nil {
		return err
	}
	_, err = s.apply(ctx, text, true)
	return err
}

// Reload applies the file as it is on disk; SIGHUP calls it too. On an
// error nothing changed, and the error is logged, counted and kept for
// ConfigStatus as well as returned: a SIGHUP has no caller to tell.
func (s *Server) Reload(ctx context.Context) (api.ApplyResult, error) {
	s.cfg.Lock()
	defer s.cfg.Unlock()
	text, err := s.file.Read()
	var res api.ApplyResult
	if err == nil {
		res, err = s.apply(ctx, text, false)
	}
	now := time.Now()
	s.lastReloadAt = &now
	if err != nil {
		s.lastReloadErr = logging.Scrub(err.Error())
		s.reloadsFailed.Add(1)
		s.log.Error("server: config: reload failed", "error", s.lastReloadErr)
		s.publish()
		return api.ApplyResult{}, err
	}
	s.lastReloadErr = ""
	s.reloadsOK.Add(1)
	s.log.Info("server: config: reloaded", "streams", len(res.Streams))
	s.publish()
	return res, nil
}

// ConfigText returns the text in force and its ETag. It never waits for an
// apply.
func (s *Server) ConfigText() ([]byte, string) {
	st := s.state.Load()
	return bytes.Clone(st.text), st.status.ETag
}

// ConfigStatus returns the status of the config in force, with Stale asked
// of the file now: a hand edit is not something the server sees happen. It
// never waits for an apply. A file that cannot be read is stale.
func (s *Server) ConfigStatus() api.ConfigStatus {
	status := s.state.Load().status
	status.LoadErrors = slices.Clone(status.LoadErrors)
	status.RestartRequired = slices.Clone(status.RestartRequired)
	stale, err := s.file.Stale()
	status.Stale = stale
	if err != nil {
		now := time.Now().UnixNano()
		if last := s.staleLogged.Load(); (last == 0 || now-last >= int64(staleLogEvery)) && s.staleLogged.CompareAndSwap(last, now) {
			s.log.Error("server: config: the file cannot be read", "error", logging.Scrub(err.Error()))
		}
	}
	return status
}

// Get returns the spec of a stream, as it was given.
func (s *Server) Get(name string) (spec.Stream, bool) { return s.Manager().Get(name) }

// List returns every stream's spec, sorted by name.
func (s *Server) List() []spec.Stream { return s.Manager().List() }

// Status returns the state of a stream.
func (s *Server) Status(name string) (stream.Status, bool) { return s.Manager().Status(name) }

// Shot returns the latest thumbnail of a stream and when it arrived.
func (s *Server) Shot(name string) (picture.Shot, time.Time, bool) { return s.Manager().Shot(name) }
