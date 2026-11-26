package stream

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/pipeline"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode"
	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
)

// ErrNoGeneration is Refresh's answer between generations.
var ErrNoGeneration = errors.New("stream: no generation is running")

// The prefixes of the last errors a live edit records, and clears when the
// part they name is replaced.
const (
	prefixWatermark = "watermark: "
	prefixInputs    = "inputs: "
	prefixInput     = "input " // "input N: ", an input's failure
)

// Refresh makes the running generation build its watermark again from the
// asset directory and hand it to the transcoder (R-E7). The channel gets one
// value and is closed: nil when the transcoder took the watermark,
// ErrNoGeneration when the stream is between generations (its next build
// reads the asset), or why the watermark could not be built (the old one
// stays). It is closed without a value when the supervisor has stopped.
// Refresh never blocks.
func (s *Supervisor) Refresh() <-chan error {
	e := edit{refresh: true, ref: make(chan error, 1)}
	s.post(e)
	return e.ref
}

// watermarkOf returns the transcoder's watermark for sp, read from
// env.AssetsDir; nil without one. ladderOf, and so Check and build, use it;
// so does the live edit. Its errors do not say "watermark": the callers
// prefix them.
func watermarkOf(sp spec.Stream, env Env) (*picture.Watermark, error) {
	if sp.Transcode == nil || sp.Transcode.Watermark == nil {
		return nil, nil //nolint:nilnil // no watermark is an answer, not an error
	}
	w := sp.Transcode.Watermark
	pos, ok := positions[w.Position]
	if !ok {
		return nil, fmt.Errorf("unknown position %q", w.Position)
	}
	pw := &picture.Watermark{
		Text: w.Text, Position: pos, OpacityPct: w.OpacityPct, SizePct: w.SizePct, TimeZone: w.TimeZone,
	}
	if w.Image != "" {
		if env.AssetsDir == "" {
			return nil, errors.New("an image needs the assets directory, which is not configured")
		}
		img, err := picture.LoadAsset(env.AssetsDir, w.Image)
		if err != nil {
			return nil, err //nolint:wrapcheck // the callers prefix it
		}
		pw.Image = img
	}
	if w.Font != "" {
		if env.AssetsDir == "" {
			return nil, errors.New("a font needs the assets directory, which is not configured")
		}
		font, err := picture.LoadFont(env.AssetsDir, w.Font)
		if err != nil {
			return nil, err //nolint:wrapcheck // the callers prefix it
		}
		pw.Font = font
	}
	return pw, nil
}

// sameWatermark reports whether a and b have the same watermark setting.
func sameWatermark(a, b spec.Stream) bool {
	var wa, wb *spec.Watermark
	if a.Transcode != nil {
		wa = a.Transcode.Watermark
	}
	if b.Transcode != nil {
		wb = b.Transcode.Watermark
	}
	return reflect.DeepEqual(wa, wb)
}

// applyLive takes g from old to new, which differ in the inputs, the
// watermark, the audio language and maybe the outputs, without ending a
// session (R-E3, R-E4, R-L5).
// Run goroutine only. It never holds s.mu across a call into the pipeline or
// the transcoder: SetInputs waits for the pipeline's loop, whose OnEvent
// takes s.mu, and Status must not wait behind either.
func (s *Supervisor) applyLive(g *generation, old, new spec.Stream) {
	// 1. The spec first, so that the status never shows a new active input
	// against the old inputs. Until step 3 returns it may show the old index
	// against the new list.
	s.setSpec(new)
	// 2. The watermark. On an error the old one stays.
	if !sameWatermark(old, new) {
		_ = s.applyWatermark(g, new)
	}
	// 3. The inputs.
	if urls := inputsOf(new); !slices.Equal(inputsOf(old), urls) {
		set := g.pipe.SetInputs
		if s.setInputs != nil {
			set = s.setInputs
		}
		err := set(g.ctx, urls)
		switch {
		case err == nil:
			// It may name an input that is gone.
			s.clearErr(prefixInput)
		case errors.Is(err, pipeline.ErrNotRunning), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			// The generation is ending: the next one is built from the new
			// spec.
		default:
			// Check parsed these URLs, so this does not happen; it is
			// reported rather than assumed away.
			s.fail(prefixInputs, err)
			s.log.Error("stream: inputs not replaced", "err", err)
		}
	}
	// 4. The outputs, as for an output edit.
	if spec.Diff(spec.Stream{Outputs: old.Outputs}, spec.Stream{Outputs: new.Outputs}) != spec.ChangeNone {
		s.reattach(g, old.Outputs, new.Outputs)
	}
	// 5. The audio language, handed to every running packager; one that
	// step 4 started already has it. SetAudioLanguage is an atomic store, so
	// s.mu is not needed.
	if old.AudioLanguage != new.AudioLanguage {
		for _, r := range g.packs {
			r.p.SetAudioLanguage(new.AudioLanguage)
		}
	}
}

// applyWatermark builds sp's watermark and hands it to g's transcoder; a
// generation without one has nothing to do. An error is recorded as the last
// error, and the transcoder keeps the watermark it had; a success clears a
// last error that an earlier failure of this left.
func (s *Supervisor) applyWatermark(g *generation, sp spec.Stream) error {
	if g.tr == nil {
		return nil
	}
	w, err := watermarkOf(sp, s.env)
	if err == nil {
		set := (*transcode.Transcoder).SetWatermark
		if s.setWatermark != nil {
			set = s.setWatermark
		}
		// SetWatermark's error is "transcode: set watermark: <why>": the
		// last error says "watermark: <why>", the word once.
		if err = set(g.tr, w); err != nil {
			if why := errors.Unwrap(err); why != nil {
				err = why
			}
		}
	}
	if err != nil {
		s.fail(prefixWatermark, err)
		s.log.Warn("stream: watermark not replaced", "err", err)
		return err
	}
	s.clearErr(prefixWatermark)
	return nil
}

// refresh is Refresh in a generation: the watermark of the spec in force,
// built again. A spec without a watermark has nothing to build.
func (s *Supervisor) refresh(g *generation) error {
	if s.spec.Transcode == nil || s.spec.Transcode.Watermark == nil {
		return nil
	}
	return s.applyWatermark(g, s.spec)
}

// liveIdle is a live edit between generations: the next one is built from
// e's spec. The last error belongs to a generation built from a spec that is
// gone, so it is cleared, as rebuildFor does.
func (s *Supervisor) liveIdle(e edit) {
	s.mu.Lock()
	s.spec = e.sp
	s.lastErr, s.lastErrAt = "", time.Time{}
	s.mu.Unlock()
	e.answer(spec.ChangeLive)
}

// clearErr drops the last error when it starts with prefix.
func (s *Supervisor) clearErr(prefix string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.HasPrefix(s.lastErr, prefix) {
		s.lastErr, s.lastErrAt = "", time.Time{}
	}
}
