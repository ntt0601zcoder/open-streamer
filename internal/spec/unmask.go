package spec

import (
	"slices"
	"strconv"
	"strings"

	"github.com/ntt0601zcoder/open-streamer/internal/redact"
)

// maskNotSecret is what Validate says of a field that holds the mask.
const maskNotSecret = "holds the redaction mask, not the secret; send the real value"

// isMask reports whether u is a URL as a response shows it with a secret
// hidden: one that redact.Masked reports, or the bare mask, which Redacted
// shows for a URL that does not parse.
func isMask(u string) bool {
	return u == redact.Mask || redact.Masked(u)
}

// KeepSecrets resolves one list of URLs by R-T4: sent is a stream's input
// URLs or its push URLs as written, stored the same list of the stored
// stream (nil when there is none). out is sent with every element that
// redact.Masked reports, or that is the bare mask, replaced by a stored
// URL; bad holds the indexes of the elements that could not be replaced,
// ascending, and out[i] is sent[i] for them. For each redacted form among
// sent's masks:
//   - one distinct stored value of the form: every mask of the form takes
//     it;
//   - n > 1 distinct stored values and exactly n masks of the form: the
//     i-th mask takes the i-th distinct stored value, both in order of
//     occurrence;
//   - otherwise every mask of the form is bad. The bare mask is always
//     bad.
//
// An element that is not masked is taken as sent, also when its redact.URL
// form is a sent mask's (R-T4).
func KeepSecrets(sent, stored []string) (out []string, bad []int) {
	out = slices.Clone(sent)
	// masks holds, for each redacted form, the indexes of sent's masks of
	// it, ascending.
	masks := make(map[string][]int)
	for i, u := range sent {
		if isMask(u) {
			masks[u] = append(masks[u], i)
		}
	}
	for form, at := range masks {
		var values []string // the distinct stored values of the form, in order
		if form != redact.Mask {
			for _, v := range stored {
				if redact.URL(v) == form && !slices.Contains(values, v) {
					values = append(values, v)
				}
			}
		}
		switch {
		case len(values) == 1:
			for _, i := range at {
				out[i] = values[0]
			}
		case len(values) > 1 && len(values) == len(at):
			for n, i := range at {
				out[i] = values[n]
			}
		default:
			bad = append(bad, at...)
		}
	}
	slices.Sort(bad)
	return out, bad
}

// KeepPassphrase resolves an srt_passphrase by R-T4: sent is returned as
// it is unless it is the mask; the mask is replaced by stored, and ok is
// false when stored is empty. Unmask and config.Unmask both use it.
func KeepPassphrase(sent, stored string) (out string, ok bool) {
	if sent != redact.Mask {
		return sent, true
	}
	if stored == "" {
		return sent, false
	}
	return stored, true
}

// MaskRefusal is the text of one refused mask, for the field named field
// ("inputs[0].url"): field + ": holds the redaction mask and the stored
// stream has no one secret it stands for; send the real value". Unmask and
// config.Unmask both take their texts from it.
func MaskRefusal(field string) string {
	return field + ": holds the redaction mask and the stored stream has no one secret it stands for; send the real value"
}

// UnmaskError lists the fields a mask could not be replaced in, each as
// "inputs[0].url: holds the redaction mask and the stored stream has no
// one secret it stands for; send the real value".
type UnmaskError []string

// Error returns the texts joined with "; ".
func (e UnmaskError) Error() string {
	return strings.Join(e, "; ")
}

// Unmask returns s with its input URLs and its push URLs resolved by
// KeepSecrets, and an SRTPassphrase equal to the mask replaced by stored's
// (R-T4); stored is nil when no stream of s's name is stored. Fields that
// hold no mask are taken as sent. The error is an UnmaskError naming every
// field that could not be replaced; s is returned unchanged with it.
func (s Stream) Unmask(stored *Stream) (Stream, error) {
	var old Stream
	if stored != nil {
		old = *stored
	}
	var refused UnmaskError

	inputs, bad := KeepSecrets(inputURLs(s.Inputs), inputURLs(old.Inputs))
	for _, i := range bad {
		refused = append(refused, MaskRefusal("inputs["+strconv.Itoa(i)+"].url"))
	}
	pushes, bad := KeepSecrets(pushURLs(s.Outputs.Push), pushURLs(old.Outputs.Push))
	for _, i := range bad {
		refused = append(refused, MaskRefusal("outputs.push["+strconv.Itoa(i)+"].url"))
	}
	passphrase, ok := KeepPassphrase(s.Outputs.SRTPassphrase, old.Outputs.SRTPassphrase)
	if !ok {
		refused = append(refused, MaskRefusal("outputs.srt_passphrase"))
	}
	if len(refused) > 0 {
		return s, refused
	}

	out := s.clone()
	for i := range out.Inputs {
		out.Inputs[i].URL = inputs[i]
	}
	for i := range out.Outputs.Push {
		out.Outputs.Push[i].URL = pushes[i]
	}
	out.Outputs.SRTPassphrase = passphrase
	return out, nil
}

func inputURLs(in []Input) []string {
	urls := make([]string, len(in))
	for i, v := range in {
		urls[i] = v.URL
	}
	return urls
}

func pushURLs(p []Push) []string {
	urls := make([]string, len(p))
	for i, v := range p {
		urls[i] = v.URL
	}
	return urls
}
