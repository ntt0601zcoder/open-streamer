package spec

import (
	"fmt"
	"regexp"
	"slices"

	"github.com/ntt0601zcoder/open-streamer/internal/redact"
)

// imageRE is the shape of a watermark image's asset name (R-C8).
var imageRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}\.(png|jpg|jpeg)$`)

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
		return fmt.Errorf("name: %q is not a valid name", s.Name)
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
			return fmt.Errorf("layout[%d]: %q is neither video nor audio", i, l)
		}
		if slices.Contains(s.Layout[:i], l) {
			return fmt.Errorf("layout[%d]: %q is repeated", i, l)
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
			return fmt.Errorf("renditions[%d].label: %q is repeated", i, r.Label)
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
		return fmt.Errorf(".label: %q is not a valid label", r.Label)
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
		return fmt.Errorf(".encoder: %q, want x264 or nvenc", r.Encoder)
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
		return fmt.Errorf(".image: %q is not a png or jpeg asset name", w.Image)
	}
	switch w.Position {
	case "", "tl", "tr", "bl", "br", "c":
	default:
		return fmt.Errorf(".position: %q, want tl, tr, bl, br or c", w.Position)
	}
	return nil
}

func (o Outputs) validate() error {
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

// checkURL checks an input or push URL. Its errors never quote the URL,
// which may hold a secret.
func checkURL(u string) error {
	switch {
	case u == "":
		return fmt.Errorf("empty")
	case len(u) > MaxURLBytes:
		return fmt.Errorf("%d bytes, want at most %d", len(u), MaxURLBytes)
	case redact.Masked(u):
		return fmt.Errorf("holds the redaction mask, not the secret; send the real value")
	}
	return nil
}
