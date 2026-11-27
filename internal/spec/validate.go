package spec

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"unicode/utf8"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/redact"
)

// imageRE is the shape of a watermark image's asset name (R-C8).
var imageRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}\.(png|jpg|jpeg)$`)

// fontRE is the shape of a watermark font's asset name (R-E8).
var fontRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}\.(ttf|otf)$`)

// Validate checks the document's shape and returns the first failure,
// naming its field as in "inputs[1].url: ...". Ranges the transcoder owns
// are not checked here.
func (s Stream) Validate() error {
	if err := s.validate(); err != nil {
		return fmt.Errorf("spec: validate: %w", err)
	}
	return nil
}

func (s Stream) validate() error {
	if !ValidName(s.Name) {
		return fmt.Errorf("name: %s is not a valid name", Quote(s.Name))
	}
	if n := len(s.Inputs); n < 1 || n > MaxInputs {
		return fmt.Errorf("inputs: %d inputs, want 1 to %d", n, MaxInputs)
	}
	for i, in := range s.Inputs {
		if err := checkURL(in.URL); err != nil {
			return fmt.Errorf("inputs[%d].url: %w", i, err)
		}
	}
	for i, l := range s.Layout {
		if l != "video" && l != "audio" {
			return fmt.Errorf("layout[%d]: %s is neither video nor audio", i, Quote(l))
		}
		if slices.Contains(s.Layout[:i], l) {
			return fmt.Errorf("layout[%d]: %s is repeated", i, Quote(l))
		}
	}
	if s.AudioLanguage != "" {
		if !media.ValidLanguage(s.AudioLanguage) {
			return fmt.Errorf("audio_language: %s is not a language tag (BCP 47), such as vi or en-US", Quote(s.AudioLanguage))
		}
		if len(s.Layout) > 0 && !slices.Contains(s.Layout, "audio") {
			return errors.New("audio_language: the layout has no audio")
		}
	}
	if s.Transcode != nil {
		if err := s.Transcode.validate(); err != nil {
			return fmt.Errorf("transcode.%w", err)
		}
	}
	return s.Outputs.validate()
}

func (t *Transcode) validate() error {
	if n := len(t.Renditions); n < 1 || n > MaxRenditions {
		return fmt.Errorf("renditions: %d renditions, want 1 to %d", n, MaxRenditions)
	}
	for i, r := range t.Renditions {
		if err := r.validate(); err != nil {
			return fmt.Errorf("renditions[%d]%w", i, err)
		}
		if slices.ContainsFunc(t.Renditions[:i], func(o Rendition) bool { return o.Label == r.Label }) {
			return fmt.Errorf("renditions[%d].label: %s is repeated", i, Quote(r.Label))
		}
	}
	if t.Watermark != nil {
		if err := t.Watermark.validate(); err != nil {
			return fmt.Errorf("watermark%w", err)
		}
	}
	return nil
}

// validate returns an error whose text starts with the field after the
// rendition's own path: ".label: ...", or ": ..." for the whole rendition.
func (r Rendition) validate() error {
	if !ValidName(r.Label) {
		return fmt.Errorf(".label: %s is not a valid label", Quote(r.Label))
	}
	if r.Copy {
		if r != (Rendition{Label: r.Label, Copy: true}) {
			return fmt.Errorf(": a copy rendition sets nothing but its label")
		}
		return nil
	}
	if r.Height < 2 || r.Height > 4320 || r.Height%2 != 0 {
		return fmt.Errorf(".height: %d, want an even height from 2 to 4320", r.Height)
	}
	if r.BitrateKbps <= 0 {
		return fmt.Errorf(".bitrate_kbps: %d, want more than 0", r.BitrateKbps)
	}
	switch r.Encoder {
	case "", "x264", "nvenc":
	default:
		return fmt.Errorf(".encoder: %s, want x264 or nvenc", Quote(r.Encoder))
	}
	if r.NoFallback && r.Encoder != "nvenc" {
		return fmt.Errorf(".no_fallback: needs encoder nvenc")
	}
	return nil
}

// validate returns an error whose text starts with the field after
// "watermark": ".image: ...", or ": ..." for the whole watermark.
func (w *Watermark) validate() error {
	if (w.Text == "") == (w.Image == "") {
		return fmt.Errorf(": set exactly one of text and image")
	}
	if w.Image != "" && !imageRE.MatchString(w.Image) {
		return fmt.Errorf(".image: %s is not a png or jpeg asset name", Quote(w.Image))
	}
	if w.Font != "" {
		if w.Text == "" {
			return fmt.Errorf(".font: needs text")
		}
		if !fontRE.MatchString(w.Font) {
			return fmt.Errorf(".font: %s is not a ttf or otf asset name", Quote(w.Font))
		}
	}
	switch w.Position {
	case "", "tl", "tr", "bl", "br", "c":
	default:
		return fmt.Errorf(".position: %s, want tl, tr, bl, br or c", Quote(w.Position))
	}
	return nil
}

func (o Outputs) validate() error {
	if err := o.checkPassphrase(); err != nil {
		return fmt.Errorf("outputs.srt_passphrase: %w", err)
	}
	if n := len(o.Push); n > MaxPushes {
		return fmt.Errorf("outputs.push: %d pushes, want at most %d", n, MaxPushes)
	}
	for i, p := range o.Push {
		if err := checkURL(p.URL); err != nil {
			return fmt.Errorf("outputs.push[%d].url: %w", i, err)
		}
		if slices.ContainsFunc(o.Push[:i], func(q Push) bool { return q.URL == p.URL }) {
			return fmt.Errorf("outputs.push[%d].url: repeated", i)
		}
	}
	return nil
}

// The lengths of an SRT passphrase, in bytes (R-T5): gosrt takes 10 to 80,
// and libsrt before 1.5 stops at 79.
const (
	minPassphraseBytes = 10
	maxPassphraseBytes = 79
)

// checkPassphrase checks the SRT passphrase (R-T5). Its errors never quote
// the value.
func (o Outputs) checkPassphrase() error {
	p := o.SRTPassphrase
	switch {
	case p == "":
		return nil
	case !o.SRT:
		return errors.New("needs outputs.srt")
	case p == redact.Mask:
		return errors.New(maskNotSecret)
	case len(p) < minPassphraseBytes || len(p) > maxPassphraseBytes:
		return fmt.Errorf("%d bytes, want %d to %d", len(p), minPassphraseBytes, maxPassphraseBytes)
	}
	for i := range len(p) {
		if !passphraseByte(p[i]) {
			return errors.New("holds a character outside A-Z, a-z, 0-9, '.', '_', '~' and '-'")
		}
	}
	return nil
}

// passphraseByte reports whether c may be in an SRT passphrase: the
// characters a URL carries unescaped.
func passphraseByte(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	case c == '.', c == '_', c == '~', c == '-':
		return true
	}
	return false
}

// maxQuoted is how many bytes of a value an error message quotes.
const maxQuoted = 64

// Quote returns v quoted for an error message, cut to its first 64 bytes
// and marked with … when it is longer: a message never grows with what a
// client sent.
func Quote(v string) string {
	if len(v) <= maxQuoted {
		return strconv.Quote(v)
	}
	n := maxQuoted
	for n > 0 && !utf8.RuneStart(v[n]) {
		n--
	}
	return strconv.Quote(v[:n]) + "…"
}

// checkURL checks an input or push URL. Its errors never quote the URL,
// which may hold a secret. Every mask is refused (isMask): the bare mask
// beside what redact.Masked reports, which is no string without "://", and
// a value that holds a masked token.
func checkURL(u string) error {
	switch {
	case u == "":
		return fmt.Errorf("empty")
	case len(u) > MaxURLBytes:
		return fmt.Errorf("%d bytes, want at most %d", len(u), MaxURLBytes)
	case isMask(u):
		return errors.New(maskNotSecret)
	}
	return nil
}
