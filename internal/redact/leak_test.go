package redact_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ntt0601zcoder/open-streamer/internal/logging"
	"github.com/ntt0601zcoder/open-streamer/internal/redact"
)

// leakNames are secret names (SecretName) placed in the URLs of
// FuzzNoSecretSurvives, R-T2's table and the review's odd forms.
var leakNames = []string{
	"password", "PASSWORD", "pass%77ord", "pwd", "psw", "pw", "pass", "passphrase", "client_secret", "access_token",
	"hdnts", "hdnea", "wmsauthsign", "sig", "X-Amz-Signature", "X-Amz-Credential", "auth", "key", "api_key",
	"streamkey", "loginpas", "authbasic", "jwt", "hmac", "hash", "md5", "password_b64", "token_v2", "secret_id",
	"pass.word", "my.password", "2password", "api%5Fkey",
}

// leakPlaces are URL templates: %[1]s is the scheme, %[2]s the name and
// %[3]s the secret, escaped.
var leakPlaces = []string{
	"%[1]s://h:9/x?a=1&%[2]s=%[3]s&b=2",
	"%[1]s://h:9/x?a=1;%[2]s=%[3]s;b=2",
	"%[1]s://h:9/user=a_%[2]s=%[3]s_channel=1/x",
	"%[1]s://h:9/live/user=a&%[2]s=%[3]s&channel=1",
	"%[1]s://h:9/app/%[2]s%%3D%[3]s/streamkey",
	"%[1]s://h:9/app/user=a%%26%[2]s=%[3]s/streamkey",
	"%[1]s://h:9/app/a%%2F%[2]s=%[3]s/streamkey",
	"%[1]s://h:9/x#a/%[2]s=%[3]s",
	"%[1]s://h:9?streamid=%%23!::r=x,%[2]s=%[3]s,m=y",
	"%[1]s://h:9?streamid=%%23!::r=x,s=%[3]s",
	"%[1]s://h:9?streamid=live%%2Fcam%%3F%[2]s%%3D%[3]s",
	"%[1]s://h:9?streamid=publish:cam:user:%[3]s",
	"%[1]s://h:9?streamid=read:cam:user:%[3]s;x=1",
	"%[1]s://u:%[3]s@h:9/x",
}

// leakSchemes are the schemes of FuzzNoSecretSurvives. Templates with an
// escaped '=', '&' or '/' in the path hide the secret on the RTMP family
// only, whose rule 5 decodes the path; elsewhere the escape stays and
// separates nothing (R-T2), so they are run with an RTMP scheme.
var leakSchemes = []string{"rtsp", "rtmp", "rtmps", "rtmp+insecure", "srt", "http"}

// escapeAll escapes every byte of s but letters and digits.
func escapeAll(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// FuzzNoSecretSurvives builds a URL from a fuzzed secret, a secret name, a
// place and a scheme, and checks that the secret, as written in the URL,
// is in neither URL's result nor a scrubbed log line.
func FuzzNoSecretSurvives(f *testing.F) {
	for i := range leakPlaces {
		f.Add("S3CR3Tv4lue", uint8(i), uint8(i), uint8(1))
		f.Add("a_b&c=d/e?f", uint8(i), uint8(i+3), uint8(i))
	}
	f.Add("x%26y=z", uint8(0), uint8(4), uint8(1))
	f.Add("p/q;r,s", uint8(2), uint8(5), uint8(2))
	f.Add("tlJwpbo6", uint8(0), uint8(2), uint8(0))
	f.Fuzz(func(t *testing.T, secret string, name, place, scheme uint8) {
		tmpl := leakPlaces[int(place)%len(leakPlaces)]
		esc := escapeAll(secret)
		if strings.Contains(tmpl, "streamid=") && !strings.Contains(tmpl, ":user:") || strings.Contains(tmpl, "%%3D%[3]s") {
			// A streamid is walked once decoded (R-T2 rule 4), and an
			// RTMP path is written decoded by rule 5, so a value after an
			// escaped '=' or inside a streamid is literal only when
			// escaped twice.
			esc = escapeAll(esc)
		}
		if len(esc) < 4 || strings.Contains(redact.Mask, esc) {
			return
		}
		sch := leakSchemes[int(scheme)%len(leakSchemes)]
		if strings.Contains(tmpl, "/app/") {
			sch = leakSchemes[1+int(scheme)%3]
		}
		u := fmt.Sprintf(tmpl, sch, leakNames[int(name)%len(leakNames)], esc)
		// The secret must not be part of the template's own text.
		if strings.Count(u, esc) != 1 || strings.Contains(fmt.Sprintf(tmpl, sch, leakNames[int(name)%len(leakNames)], "Q"), esc) {
			return
		}
		// Rule 5 writes the path decoded, so the secret is also looked
		// for as it is, when that cannot be the template's own text.
		forms := []string{esc}
		if len(secret) >= 4 && secret != esc && !strings.Contains(redact.Mask, secret) &&
			!strings.Contains(fmt.Sprintf(tmpl, sch, leakNames[int(name)%len(leakNames)], "Q"), secret) {
			forms = append(forms, secret)
		}
		out, line := redact.URL(u), logging.Scrub("open "+u+": x")
		for _, s := range forms {
			if strings.Contains(out, s) {
				t.Errorf("URL(%q) = %q holds %q", u, out, s)
			}
			if strings.Contains(line, s) {
				t.Errorf("Scrub of %q = %q holds %q", u, line, s)
			}
		}
	})
}
