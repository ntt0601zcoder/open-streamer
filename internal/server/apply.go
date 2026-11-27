package server

import (
	"bytes"
	"context"
	"errors"
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

// maxReloadError is the longest last reload error kept and logged. The
// parse bounds its own errors; this bounds whatever else a reload fails
// with, such as one conflict per stream of a large file.
const maxReloadError = 16 << 10

// bounded returns s cut to maxReloadError bytes, marked when it was cut.
func bounded(s string) string {
	if len(s) <= maxReloadError {
		return s
	}
	return strings.ToValidUTF8(s[:maxReloadError], "") + "… (cut)"
}

// state is the text in force as a response shows it and its status,
// published as one immutable value. status.Stale is never set here:
// ConfigStatus asks the file.
type state struct {
	applied []byte // the text in force, which masked was made of
	masked  []byte // config.Mask(applied); nil when it could not be masked
	status  api.ConfigStatus
}

// publish makes the fields cfg guards the snapshot the readers see. The
// caller holds s.cfg, or is New. The text is masked once per applied
// text: a reload that fails keeps the snapshot's mask.
func (s *Server) publish() {
	st := &state{
		applied: s.applied,
		status: api.ConfigStatus{
			ETag:            config.ETag(s.applied),
			LoadedAt:        s.loadedAt,
			LoadErrors:      slices.Clone(s.loadErrs),
			LastReloadAt:    s.lastReloadAt,
			LastReloadError: s.lastReloadErr,
			RestartRequired: slices.Clone(s.restart),
		},
	}
	if prev := s.state.Load(); prev != nil && bytes.Equal(prev.applied, s.applied) {
		st.masked = prev.masked
	} else if masked, err := config.Mask(s.applied); err != nil {
		s.log.Error("server: config: the text cannot be shown with its secrets masked", "error", logging.Scrub(err.Error()))
	} else {
		st.masked = bytes.Clone(masked)
		if st.masked == nil {
			st.masked = []byte{}
		}
	}
	s.state.Store(st)
}

// apply makes text the config in force. write says whether the text came
// over the API (and must be written) or from the file (a reload). The
// caller holds s.cfg.
//
// The parse is strict, so a text that still holds an entry the start
// skipped is refused with that entry's errors, whichever way it came; and
// it runs every stream's Validate, which refuses the mask wherever it is
// left: PutConfig and PutStream resolve every mask they can before this
// (R-T4), so a mask that reaches it is one they could not resolve or one
// written into the file by hand, and it is never stored.
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
	s.skipped = false
	// The text parsed strictly, so no entry is skipped any more; what is
	// left are the streams that still cannot run, each with the reason of
	// this edit, which tried them again.
	s.loadErrs = stoppedTexts(s.Manager().Stopped())
	restart := s.started.Diff(p.Doc.Server.Resolved())
	if restart == nil {
		restart = []string{}
	}
	if len(restart) > 0 && (!write || !slices.Equal(restart, s.restart)) {
		s.log.Warn("server: config: restart required: " + strings.Join(restart, ", "))
	}
	s.restart = restart
	s.publish()
	return api.ApplyResult{Streams: out, RestartRequired: slices.Clone(restart), ETag: config.ETag(text)}, nil
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

// PutConfig makes text the config in force and the file's content. A
// mask in it stands for the stored secret and is resolved against the
// text in force (config.Unmask, R-T4); a text without masks is stored as
// the client's exact bytes. The stale check comes before If-Match: a
// client whose ETag is current still must not overwrite a hand edit.
func (s *Server) PutConfig(ctx context.Context, text []byte, ifMatch string) (api.ApplyResult, error) {
	s.cfg.Lock()
	defer s.cfg.Unlock()
	if ifMatch == "" {
		return api.ApplyResult{}, api.ErrPreconditionRequired
	}
	if err := s.stale(); err != nil {
		return api.ApplyResult{}, err
	}
	if ifMatch != config.ETag(s.applied) {
		return api.ApplyResult{}, api.ErrPrecondition
	}
	text, err := config.Unmask(text, s.applied)
	if err != nil {
		return api.ApplyResult{}, err // config.Errors: one per mask it could not resolve
	}
	return s.apply(ctx, text, true)
}

// PutStream creates or replaces one stream: the entry of its name in the
// text in force, also one the start skipped. sp is the document as sent: a
// mask in it stands for the secret the stored stream of its name holds
// (spec.Stream.Unmask, R-T4), and everything after works on the resolved
// document. A document equal to the one the stream has rewrites nothing:
// the text in force is applied as it is (which still tries a stream left
// stopped again), so the file keeps its layout, blank lines included. Not
// while the text still holds entries the start skipped: one of them may
// repeat this name, and the edit removes it. stored is the document as it
// is stored afterwards.
func (s *Server) PutStream(ctx context.Context, sp spec.Stream, ifMatch string) (stream.Outcome, spec.Stream, error) {
	s.cfg.Lock()
	defer s.cfg.Unlock()
	if err := s.stale(); err != nil {
		return stream.Outcome{}, spec.Stream{}, err
	}
	if err := s.matchStream(sp.Name, ifMatch); err != nil {
		return stream.Outcome{}, spec.Stream{}, err
	}
	cur, ok := s.Manager().Get(sp.Name)
	var old *spec.Stream
	if ok {
		old = &cur
	}
	sp, err := sp.Unmask(old)
	if err != nil {
		return stream.Outcome{}, spec.Stream{}, unmaskErrors(err)
	}
	text := s.applied
	if !ok || s.skipped || spec.Diff(cur, sp) != spec.ChangeNone {
		if text, err = config.SetStream(s.applied, sp); err != nil {
			return stream.Outcome{}, spec.Stream{}, err
		}
	}
	res, err := s.apply(ctx, text, true)
	if err != nil {
		return stream.Outcome{}, spec.Stream{}, err
	}
	stored, ok := s.Manager().Get(sp.Name)
	if !ok {
		stored = sp
	}
	for _, o := range res.Streams {
		if o.Name == sp.Name {
			return o, stored, nil
		}
	}
	return stream.Outcome{Name: sp.Name, Applied: "unchanged"}, stored, nil
}

// unmaskErrors turns the fields spec.Stream.Unmask could not resolve into
// config.Errors, each in the words of spec.Stream.Validate, which the API
// answers 422 invalid_config.
func unmaskErrors(err error) error {
	var refused spec.UnmaskError
	if !errors.As(err, &refused) {
		return fmt.Errorf("server: put stream: %w", err)
	}
	out := make(config.Errors, len(refused))
	for i, text := range refused {
		out[i] = errors.New("spec: validate: " + text)
	}
	return out
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
//
// With server.tls it reads the certificate pair again first, before it
// takes cfg (R-S3): a new certificate never waits for an apply, and is
// taken whatever the config's reload then does. Its result is reported as
// a poll's is, and is not this function's.
func (s *Server) Reload(ctx context.Context) (api.ApplyResult, error) {
	if s.store != nil {
		s.certReload()
	}
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
		s.lastReloadErr = bounded(logging.Scrub(err.Error()))
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

// ConfigText returns the text in force as a response shows it, masked by
// config.Mask, and the ETag of the text itself (R-T12). The text is nil
// when it could not be masked. It never waits for an apply.
func (s *Server) ConfigText() ([]byte, string) {
	st := s.state.Load()
	return bytes.Clone(st.masked), st.status.ETag
}

// ConfigStatus returns the status of the config in force, with Stale asked
// of the file now: a hand edit is not something the server sees happen, and
// the certificate asked of its store now: a poll changes it outside cfg. It
// never waits for an apply. A file that cannot be read is stale.
func (s *Server) ConfigStatus() api.ConfigStatus {
	status := s.state.Load().status
	status.LoadErrors = slices.Clone(status.LoadErrors)
	status.RestartRequired = slices.Clone(status.RestartRequired)
	stale, err := s.file.Stale()
	status.Stale = stale
	if s.store != nil {
		// The API writes nil names as [].
		c := s.store.Info()
		status.Certificate = &api.CertStatus{NotAfter: c.NotAfter, Names: c.DNSNames, LoadedAt: c.LoadedAt, LastError: c.LastError}
	}
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
