package spec

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/redact"
)

// The package's private copies of redact's two texts are redact's.
func TestMaskIsRedacts(t *testing.T) {
	assert.Equal(t, "rtsp://u:"+mask+"@h/s", redact.URL("rtsp://u:p@h/s"))
	assert.Equal(t, invalid, redact.URL("rtsp://u:p@h:port/s"))
}

// Two push URLs whose keys start alike, and the one form both redact to.
const (
	keyOne  = "rtmp://h/live/secretkey"
	keyTwo  = "rtmp://h/live/sesame"
	keyMask = "rtmp://h/live/sexxxxx"
)

func TestKeepSecrets(t *testing.T) {
	const (
		cam     = "rtsp://u:pw@cam/s"
		camMask = "rtsp://u:xxxxx@cam/s"
		fresh   = "rtmp://new/live/realkey"
		third   = "rtmp://h/live/sextant"
	)
	require.Equal(t, keyMask, redact.URL(keyOne))
	require.Equal(t, keyMask, redact.URL(keyTwo))
	require.Equal(t, keyMask, redact.URL(third))
	require.Equal(t, camMask, redact.URL(cam))

	for _, c := range []struct {
		name   string
		stored []string
		sent   []string
		want   []string
		bad    []int
	}{
		{"the stored list masked", []string{cam, keyOne}, []string{camMask, keyMask}, []string{cam, keyOne}, nil},
		{"reversed: by value", []string{cam, keyOne}, []string{keyMask, camMask}, []string{keyOne, cam}, nil},
		{"one value stored twice, one mask", []string{keyOne, keyOne}, []string{keyMask}, []string{keyOne}, nil},
		{"one value stored twice, two masks", []string{keyOne, keyOne}, []string{keyMask, keyMask}, []string{keyOne, keyOne}, nil},
		{"one value, three masks", []string{keyOne}, []string{keyMask, keyMask, keyMask}, []string{keyOne, keyOne, keyOne}, nil},

		{"in order", []string{keyOne, keyTwo}, []string{keyMask, keyMask}, []string{keyOne, keyTwo}, nil},
		{"in the stored order", []string{keyTwo, keyOne}, []string{keyMask, keyMask}, []string{keyTwo, keyOne}, nil},
		{
			"in order around others",
			[]string{keyOne, "udp://x:1", keyTwo},
			[]string{keyMask, fresh, keyMask},
			[]string{keyOne, fresh, keyTwo},
			nil,
		},
		{
			"in order, a value stored twice counted once",
			[]string{keyOne, keyTwo, keyOne},
			[]string{keyMask, keyMask},
			[]string{keyOne, keyTwo},
			nil,
		},

		{"fewer masks than values", []string{keyOne, keyTwo}, []string{keyMask}, []string{keyMask}, []int{0}},
		{
			"more masks than values",
			[]string{keyOne, keyTwo},
			[]string{keyMask, keyMask, keyMask},
			[]string{keyMask, keyMask, keyMask},
			[]int{0, 1, 2},
		},
		{
			"only the form whose counts differ",
			[]string{keyOne, keyTwo, cam},
			[]string{camMask, keyMask, "udp://x:1"},
			[]string{cam, keyMask, "udp://x:1"},
			[]int{1},
		},

		{"another host", []string{cam}, []string{"rtsp://u:xxxxx@other/s"}, []string{"rtsp://u:xxxxx@other/s"}, []int{0}},
		{"another user", []string{cam}, []string{"rtsp://v:xxxxx@cam/s"}, []string{"rtsp://v:xxxxx@cam/s"}, []int{0}},
		{"another port", []string{cam}, []string{"rtsp://u:xxxxx@cam:8554/s"}, []string{"rtsp://u:xxxxx@cam:8554/s"}, []int{0}},
		{"nothing stored", nil, []string{camMask, "udp://x:1", keyMask}, []string{camMask, "udp://x:1", keyMask}, []int{0, 2}},

		// A real URL beside a mask of its form (controller ruling).
		{
			"a new key beside the mask",
			[]string{keyOne},
			[]string{keyMask, "rtmp://h/live/senew-key"},
			[]string{keyOne, "rtmp://h/live/senew-key"},
			nil,
		},
		{"the stored key beside its mask", []string{keyOne}, []string{keyMask, keyOne}, []string{keyOne, keyOne}, nil},
		{
			"a third key beside two masks",
			[]string{keyOne, keyTwo},
			[]string{keyMask, keyMask, third},
			[]string{keyOne, keyTwo, third},
			nil,
		},
		{"no mask", []string{keyOne}, []string{keyOne}, []string{keyOne}, nil},
		{"no mask and nothing stored", nil, []string{keyOne, "udp://x:1"}, []string{keyOne, "udp://x:1"}, nil},

		{"the bare mask", []string{cam, keyOne}, []string{"xxxxx"}, []string{"xxxxx"}, []int{0}},
		{"the bare mask, nothing stored", nil, []string{"xxxxx"}, []string{"xxxxx"}, []int{0}},
		{"the bare mask, an unparseable URL stored", []string{"rtsp://u:pw@h:port/s"}, []string{"xxxxx"}, []string{"xxxxx"}, []int{0}},
		{"the bare mask, the bare mask stored", []string{"xxxxx"}, []string{"xxxxx"}, []string{"xxxxx"}, []int{0}},
		{"nothing sent", []string{cam}, nil, nil, nil},
	} {
		sent := append([]string(nil), c.sent...)
		stored := append([]string(nil), c.stored...)
		out, bad := KeepSecrets(c.sent, c.stored)
		assert.Equal(t, c.want, out, c.name)
		assert.Equal(t, c.bad, bad, c.name)
		assert.Equal(t, sent, c.sent, "%s: sent is not written to", c.name)
		assert.Equal(t, stored, c.stored, "%s: stored is not written to", c.name)
	}

	// bad is ascending whatever the forms are: eight forms, none stored.
	many := make([]string, 8)
	for i := range many {
		many[i] = "rtsp://u:xxxxx@" + string(rune('a'+i)) + "/s"
	}
	for range 20 {
		_, bad := KeepSecrets(many, []string{cam})
		assert.Equal(t, []int{0, 1, 2, 3, 4, 5, 6, 7}, bad)
	}

	// The result is a list of its own.
	sent := []string{"udp://x:1"}
	out, _ := KeepSecrets(sent, nil)
	out[0] = "udp://x:2"
	assert.Equal(t, "udp://x:1", sent[0])
}

func TestKeepPassphrase(t *testing.T) {
	for _, c := range []struct {
		sent, stored, want string
		ok                 bool
	}{
		{"xxxxx", "srtpass-0123", "srtpass-0123", true},
		{"xxxxx", "", "xxxxx", false},
		{"srtpass-4567", "srtpass-0123", "srtpass-4567", true},
		{"srtpass-4567", "", "srtpass-4567", true},
		{"", "srtpass-0123", "", true},
		{"", "", "", true},
	} {
		out, ok := KeepPassphrase(c.sent, c.stored)
		assert.Equal(t, c.want, out, "%q over %q", c.sent, c.stored)
		assert.Equal(t, c.ok, ok, "%q over %q", c.sent, c.stored)
	}
}

func TestMaskRefusal(t *testing.T) {
	assert.Equal(t,
		"inputs[0].url: holds the redaction mask and the stored stream has no one secret it stands for; send the real value",
		MaskRefusal("inputs[0].url"))
	e := UnmaskError{MaskRefusal("inputs[0].url"), MaskRefusal("outputs.srt_passphrase")}
	assert.Equal(t, MaskRefusal("inputs[0].url")+"; "+MaskRefusal("outputs.srt_passphrase"), e.Error())
}

// unmaskStored is the stored stream of TestUnmask and TestUnmaskRefuses: a
// password and a passphrase in its inputs, a key in its push, and an SRT
// passphrase.
func unmaskStored() Stream {
	return Stream{
		Name:   "cam",
		Inputs: []Input{{URL: "rtsp://u:pw@cam/s"}, {URL: "srt://h:9000?passphrase=0123456789"}},
		Outputs: Outputs{
			HLS: true, SRT: true, SRTPassphrase: "srtpass-0123",
			Push: []Push{{URL: keyOne}},
		},
	}
}

// An editor reads a stream, changes it and writes back what he read
// (Review Focus 1).
func TestUnmask(t *testing.T) {
	stored := unmaskStored()
	require.NoError(t, stored.Validate())
	kept := unmaskStored()

	red := stored.Redacted()
	require.Equal(t, "rtsp://u:xxxxx@cam/s", red.Inputs[0].URL)
	require.Equal(t, "srt://h:9000?passphrase=xxxxx", red.Inputs[1].URL)
	require.Equal(t, keyMask, red.Outputs.Push[0].URL)
	require.Equal(t, "xxxxx", red.Outputs.SRTPassphrase)
	require.Error(t, red.Validate(), "the redacted document is never valid")

	// A change beside the secrets.
	sent := stored.Redacted()
	sent.AudioLanguage = "vi"
	sentBefore := sent.clone()
	got, err := sent.Unmask(&stored)
	require.NoError(t, err)
	want := unmaskStored()
	want.AudioLanguage = "vi"
	assert.Equal(t, want, got)
	assert.Equal(t, ChangeLive, Diff(stored, got))
	require.NoError(t, got.Validate())
	assert.Equal(t, sentBefore, sent, "the receiver is unchanged")
	assert.Equal(t, kept, stored, "the stored stream is unchanged")
	// A deep copy: writing through the result reaches neither.
	got.Inputs[0].URL, got.Outputs.Push[0].URL = "udp://:1", "udp://:2"
	assert.Equal(t, sentBefore, sent)
	assert.Equal(t, kept, stored)

	// No change at all.
	got, err = stored.Redacted().Unmask(&stored)
	require.NoError(t, err)
	assert.Equal(t, stored, got)
	assert.Equal(t, ChangeNone, Diff(stored, got))
	assert.Equal(t, stored.ETag(), got.ETag())

	// The inputs swapped: by value.
	sent = stored.Redacted()
	sent.Inputs[0], sent.Inputs[1] = sent.Inputs[1], sent.Inputs[0]
	got, err = sent.Unmask(&stored)
	require.NoError(t, err)
	assert.Equal(t, []Input{stored.Inputs[1], stored.Inputs[0]}, got.Inputs)
	assert.Equal(t, ChangeLive, Diff(stored, got))

	// A real URL beside a masked one, of another form and of the same.
	sent = stored.Redacted()
	sent.Inputs[1].URL = "udp://:5000"
	got, err = sent.Unmask(&stored)
	require.NoError(t, err)
	assert.Equal(t, []Input{{URL: "rtsp://u:pw@cam/s"}, {URL: "udp://:5000"}}, got.Inputs)
	sent = stored.Redacted()
	sent.Inputs[1].URL = "rtsp://u:other-pw@cam/s"
	got, err = sent.Unmask(&stored)
	require.NoError(t, err)
	assert.Equal(t, []Input{{URL: "rtsp://u:pw@cam/s"}, {URL: "rtsp://u:other-pw@cam/s"}}, got.Inputs)
	require.NoError(t, got.Validate())

	// A push sent as its mask and again as a real URL of the same form:
	// both are taken, and Validate says "repeated" when the real one is
	// the stored one (the conceded oracle of R-T3).
	sent = stored.Redacted()
	sent.Outputs.Push = append(sent.Outputs.Push, Push{URL: keyTwo})
	got, err = sent.Unmask(&stored)
	require.NoError(t, err)
	assert.Equal(t, []Push{{URL: keyOne}, {URL: keyTwo}}, got.Outputs.Push)
	require.NoError(t, got.Validate())
	sent = stored.Redacted()
	sent.Outputs.Push = append(sent.Outputs.Push, Push{URL: keyOne})
	got, err = sent.Unmask(&stored)
	require.NoError(t, err)
	assert.Equal(t, []Push{{URL: keyOne}, {URL: keyOne}}, got.Outputs.Push)
	require.EqualError(t, got.Validate(), "spec: validate: outputs.push[1].url: repeated")

	// An input no URL parser reads is shown as the bare mask, and that
	// sent back is refused.
	odd := unmaskStored()
	odd.Inputs[0].URL = "rtsp://admin:p%ss@cam/s"
	sent = odd.Redacted()
	require.Equal(t, "xxxxx", sent.Inputs[0].URL)
	got, err = sent.Unmask(&odd)
	assert.Equal(t, UnmaskError{MaskRefusal("inputs[0].url")}, err)
	assert.Equal(t, sent, got)

	// Two pushes that redact alike, sent as two masks with a third push.
	two := unmaskStored()
	two.Outputs.Push = []Push{{URL: keyOne}, {URL: keyTwo}}
	sent = two.Redacted()
	require.Equal(t, []Push{{URL: keyMask}, {URL: keyMask}}, sent.Outputs.Push)
	sent.Outputs.Push = append(sent.Outputs.Push, Push{URL: "rtmp://other/live/thirdkey"})
	got, err = sent.Unmask(&two)
	require.NoError(t, err)
	assert.Equal(t, []Push{{URL: keyOne}, {URL: keyTwo}, {URL: "rtmp://other/live/thirdkey"}}, got.Outputs.Push)
	assert.Equal(t, ChangeOutputs, Diff(two, got))
	require.NoError(t, got.Validate())

	// The passphrase left out is removed (R-T3); a real one is taken.
	sent = stored.Redacted()
	sent.Outputs.SRTPassphrase = ""
	got, err = sent.Unmask(&stored)
	require.NoError(t, err)
	assert.Empty(t, got.Outputs.SRTPassphrase)
	assert.Equal(t, ChangeOutputs, Diff(stored, got))
	sent = stored.Redacted()
	sent.Outputs.SRTPassphrase = "srtpass-4567"
	got, err = sent.Unmask(&stored)
	require.NoError(t, err)
	assert.Equal(t, "srtpass-4567", got.Outputs.SRTPassphrase)
	assert.Equal(t, ChangeOutputs, Diff(stored, got))

	// A document without a mask is taken as sent, with or without a
	// stored stream.
	plain := unmaskStored()
	got, err = plain.Unmask(nil)
	require.NoError(t, err)
	assert.Equal(t, plain, got)
	got, err = plain.Unmask(&stored)
	require.NoError(t, err)
	assert.Equal(t, plain, got)
	var zero Stream
	got, err = zero.Unmask(nil)
	require.NoError(t, err)
	assert.Equal(t, zero, got)
}

// A mask pointed somewhere else (Review Focus 2): each is an UnmaskError
// naming exactly the listed fields, and the document comes back as sent.
func TestUnmaskRefuses(t *testing.T) {
	stored := unmaskStored()
	both := unmaskStored() // a password and a token in one URL
	both.Inputs[0].URL = "rtsp://u:pw@cam/s?token=tok-abc"
	two := unmaskStored() // two pushes that redact alike
	two.Outputs.Push = []Push{{URL: keyOne}, {URL: keyTwo}}
	plain := unmaskStored() // no passphrase stored
	plain.Outputs.SRTPassphrase = ""

	input := func(u string) func(*Stream) {
		return func(s *Stream) { s.Inputs[0].URL = u }
	}
	for _, c := range []struct {
		name   string
		stored *Stream
		edit   func(*Stream) // applied to the stored stream, redacted
		fields []string
	}{
		{"nothing stored", nil, func(s *Stream) {
			s.Inputs[1].URL, s.Outputs.Push, s.Outputs.SRTPassphrase = "udp://:5000", nil, ""
		}, []string{"inputs[0].url"}},
		{"the host", &stored, input("rtsp://u:xxxxx@evil.example/s"), []string{"inputs[0].url"}},
		{"the port", &stored, input("rtsp://u:xxxxx@cam:8554/s"), []string{"inputs[0].url"}},
		{"the user", &stored, input("rtsp://v:xxxxx@cam/s"), []string{"inputs[0].url"}},
		{"the path", &stored, input("rtsp://u:xxxxx@cam/t"), []string{"inputs[0].url"}},
		{"a query value", &stored, func(s *Stream) {
			s.Inputs[1].URL = "srt://h:9000?passphrase=xxxxx&mode=caller"
		}, []string{"inputs[1].url"}},
		{"the scheme", &stored, input("rtsps://u:xxxxx@cam/s"), []string{"inputs[0].url"}},
		{"one secret of two retyped", &both, input("rtsp://u:xxxxx@cam/s?token=tok-abc"), []string{"inputs[0].url"}},
		{"an input's mask among the pushes", &stored, func(s *Stream) {
			s.Outputs.Push[0].URL = "rtsp://u:xxxxx@cam/s"
		}, []string{"outputs.push[0].url"}},
		{"one mask for two keys", &two, func(s *Stream) {
			s.Outputs.Push = s.Outputs.Push[:1]
		}, []string{"outputs.push[0].url"}},
		{"the bare mask", &stored, input("xxxxx"), []string{"inputs[0].url"}},
		{"no stored passphrase", &plain, func(s *Stream) {
			s.Outputs.SRTPassphrase = "xxxxx"
		}, []string{"outputs.srt_passphrase"}},
		{"an input and a push", &stored, func(s *Stream) {
			s.Inputs[1].URL = "srt://other:9000?passphrase=xxxxx"
			s.Outputs.Push[0].URL = "rtmp://other/live/sexxxxx"
		}, []string{"inputs[1].url", "outputs.push[0].url"}},
		{
			"everything, nothing stored", nil, func(*Stream) {},
			[]string{"inputs[0].url", "inputs[1].url", "outputs.push[0].url", "outputs.srt_passphrase"},
		},
	} {
		sent := unmaskStored().Redacted()
		if c.stored != nil {
			sent = c.stored.Redacted()
		}
		c.edit(&sent)
		before := sent.clone()

		got, err := sent.Unmask(c.stored)
		var want UnmaskError
		for _, f := range c.fields {
			want = append(want, f+": holds the redaction mask and the stored stream has no one secret it stands for; send the real value")
		}
		var refused UnmaskError
		if assert.ErrorAs(t, err, &refused, c.name) {
			assert.Equal(t, want, refused, c.name)
		}
		assert.Equal(t, before, got, "%s: the document comes back as sent", c.name)
		assert.Equal(t, before, sent, "%s: the receiver is unchanged", c.name)

		// No text holds a secret of the stored stream, or a sent value.
		if err != nil {
			for _, secret := range []string{"pw", "0123456789", "secretkey", "sesame", "srtpass", "tok-abc", "evil", "xxxxx"} {
				assert.NotContains(t, err.Error(), secret, c.name)
			}
		}
		// Nothing of the stored stream reaches a refused document.
		b, jerr := json.Marshal(got)
		require.NoError(t, jerr)
		for _, secret := range []string{":pw@", "0123456789", "secretkey", "sesame", "srtpass-0123"} {
			assert.NotContains(t, string(b), secret, c.name)
		}
	}
}
