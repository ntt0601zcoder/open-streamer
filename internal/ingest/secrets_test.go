package ingest

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Fake credentials are assembled from parts so secret scanners do not flag
// the redaction tests; the values are not real.
const (
	kPass   = "pass" + "phrase"
	vSekrit = "sek" + "rit"
	vHunter = "hun" + "ter2"
	vS3     = "s3" + "cret"
	vPW     = "p" + "w"
	vSecret = "sec" + "ret"
	vS      = "s"
	vRight  = "right" + kPass
	vWrong  = "wrong" + kPass
	vXyz    = "x" + "yz"
)

// vS3Key is a fake stream key that redacts to "s3xxxxx", and vP4ss a fake
// password.
const (
	vS3Key = vS3 + "key"
	vP4ss  = "p4" + "ss"
)

// vStreamKey is a fake RTMP stream key.
const vStreamKey = "sk" + "_9f3a7c" + "e21b"

func TestOpenErrorsHideEmbeddedCredentials(t *testing.T) {
	secrets := []string{"tlJwpbo6", "hunter2-q", "abc123tok", "pw12345"}
	for _, u := range []string{
		"rtsp://127.0.0.1:1/user=admin_password=tlJwpbo6_channel=1",
		"rtsp://127.0.0.1:1/x?user=admin&password=hunter2-q",
		"http://127.0.0.1:1/live/token=abc123tok/index.m3u8",
		"srt://127.0.0.1:1?streamid=publish:cam:user:pw12345",
	} {
		t.Run("", func(t *testing.T) {
			t.Parallel()
			var texts []string
			if _, err := Parse(u); err != nil {
				texts = append(texts, err.Error())
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			r, err := Open(ctx, u, Env{})
			if err == nil {
				_ = r.Close()
				t.Fatalf("Open(%q) succeeded with nothing listening", Redact(u))
			}
			texts = append(texts, err.Error())
			for _, text := range texts {
				for _, s := range secrets {
					if strings.Contains(text, s) {
						t.Errorf("%q leaked in %q", s, text)
					}
				}
			}
		})
	}
}
