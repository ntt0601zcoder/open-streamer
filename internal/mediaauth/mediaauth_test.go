package mediaauth

import (
	"net"
	"testing"
	"time"

	"github.com/ntt0601zcoder/open-streamer/config"
	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

const testCode = domain.StreamCode("ch1")

func ip(s string) net.IP { return net.ParseIP(s) }

// ── token sign/verify ──

func TestToken_SignVerifyRoundTrip(t *testing.T) {
	t.Parallel()
	st := buildState(config.MediaAuthConfig{Enabled: true, TokenSecret: "sekret"})
	exp := time.Now().Add(time.Hour).Unix()
	tok := SignToken(st.secret, testCode, exp)
	if !st.verify(testCode, tok) {
		t.Fatal("valid token failed to verify")
	}
}

func TestToken_Rejects(t *testing.T) {
	t.Parallel()
	st := buildState(config.MediaAuthConfig{Enabled: true, TokenSecret: "sekret"})
	valid := SignToken(st.secret, testCode, time.Now().Add(time.Hour).Unix())

	t.Run("expired", func(t *testing.T) {
		old := SignToken(st.secret, testCode, time.Now().Add(-2*time.Hour).Unix())
		if st.verify(testCode, old) {
			t.Error("expired token verified")
		}
	})
	t.Run("wrong_stream_code", func(t *testing.T) {
		if st.verify("other", valid) {
			t.Error("token for ch1 verified against 'other'")
		}
	})
	t.Run("tampered_sig", func(t *testing.T) {
		if st.verify(testCode, valid+"x") {
			t.Error("tampered token verified")
		}
	})
	t.Run("different_secret", func(t *testing.T) {
		other := buildState(config.MediaAuthConfig{Enabled: true, TokenSecret: "other"})
		if other.verify(testCode, valid) {
			t.Error("token verified under a different secret")
		}
	})
	t.Run("malformed", func(t *testing.T) {
		for _, bad := range []string{"", "noexp", "abc.def", ".sig", "999"} {
			if st.verify(testCode, bad) {
				t.Errorf("malformed token %q verified", bad)
			}
		}
	})
}

// ── chain ──

func newAuthz(t *testing.T, cfg config.MediaAuthConfig, geo GeoResolver, pol PolicyResolver) *Authorizer {
	t.Helper()
	return New(cfg, geo, pol)
}

func TestAuthorize_DisabledAllowsAll(t *testing.T) {
	t.Parallel()
	a := newAuthz(t, config.MediaAuthConfig{Enabled: false, DefaultPolicy: PolicyToken}, nil, nil)
	if d := a.Authorize(AuthRequest{Code: testCode}); !d.Allow {
		t.Fatalf("disabled must allow, got deny: %s", d.Reason)
	}
}

func TestAuthorize_IPRules(t *testing.T) {
	t.Parallel()
	a := newAuthz(t, config.MediaAuthConfig{
		Enabled:  true,
		DenyIPs:  []string{"8.8.8.8", "10.0.0.0/8"},
		AllowIPs: []string{"1.2.3.4", "192.168.0.0/16"},
	}, nil, nil)

	cases := []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", false},     // deny exact
		{"10.5.5.5", false},    // deny CIDR
		{"1.2.3.4", true},      // allow exact
		{"192.168.1.9", true},  // allow CIDR
		{"203.0.113.7", false}, // not on allow list → deny
	}
	for _, c := range cases {
		if d := a.Authorize(AuthRequest{Code: testCode, ClientIP: ip(c.ip)}); d.Allow != c.want {
			t.Errorf("ip %s: allow=%v want %v (%s)", c.ip, d.Allow, c.want, d.Reason)
		}
	}
}

func TestAuthorize_CountryRules(t *testing.T) {
	t.Parallel()
	geo := func(p net.IP) string {
		switch p.String() {
		case "1.1.1.1":
			return "VN"
		case "2.2.2.2":
			return "RU"
		}
		return "" // unknown
	}
	a := newAuthz(t, config.MediaAuthConfig{
		Enabled:        true,
		AllowCountries: []string{"VN", "US"},
		DenyCountries:  []string{"RU"},
	}, geo, nil)

	if d := a.Authorize(AuthRequest{Code: testCode, ClientIP: ip("1.1.1.1")}); !d.Allow {
		t.Errorf("VN must be allowed: %s", d.Reason)
	}
	if d := a.Authorize(AuthRequest{Code: testCode, ClientIP: ip("2.2.2.2")}); d.Allow {
		t.Error("RU must be denied")
	}
	if d := a.Authorize(AuthRequest{Code: testCode, ClientIP: ip("9.9.9.9")}); d.Allow {
		t.Error("unknown country must fail the allow-list gate")
	}
}

func TestAuthorize_UserAgentRules(t *testing.T) {
	t.Parallel()
	a := newAuthz(t, config.MediaAuthConfig{
		Enabled:        true,
		DenyUserAgents: []string{"badbot"},
	}, nil, nil)
	if d := a.Authorize(AuthRequest{Code: testCode, UserAgent: "Mozilla BadBot/1.0"}); d.Allow {
		t.Error("denied UA substring must block")
	}
	if d := a.Authorize(AuthRequest{Code: testCode, UserAgent: "VLC/3.0"}); !d.Allow {
		t.Errorf("allowed UA must pass: %s", d.Reason)
	}

	b := newAuthz(t, config.MediaAuthConfig{Enabled: true, AllowUserAgents: []string{"exoplayer"}}, nil, nil)
	if d := b.Authorize(AuthRequest{Code: testCode, UserAgent: "ExoPlayerLib/2.1"}); !d.Allow {
		t.Errorf("UA on allow list must pass: %s", d.Reason)
	}
	if d := b.Authorize(AuthRequest{Code: testCode, UserAgent: "curl/8"}); d.Allow {
		t.Error("UA off allow list must be denied")
	}
}

func TestAuthorize_AllowedDomains(t *testing.T) {
	t.Parallel()
	a := newAuthz(t, config.MediaAuthConfig{Enabled: true, AllowedDomains: []string{"example.com"}}, nil, nil)
	cases := []struct {
		ref  string
		want bool
	}{
		{"https://example.com/player", true},
		{"https://play.example.com/embed", true}, // subdomain
		{"https://evil.com/", false},
		{"", false}, // no referer → fail the gate
	}
	for _, c := range cases {
		if d := a.Authorize(AuthRequest{Code: testCode, Referer: c.ref}); d.Allow != c.want {
			t.Errorf("referer %q: allow=%v want %v", c.ref, d.Allow, c.want)
		}
	}
}

func TestAuthorize_TokenPolicy(t *testing.T) {
	t.Parallel()
	a := newAuthz(t, config.MediaAuthConfig{Enabled: true, DefaultPolicy: PolicyToken, TokenSecret: "sk"}, nil, nil)
	good := SignToken([]byte("sk"), testCode, time.Now().Add(time.Hour).Unix())
	if d := a.Authorize(AuthRequest{Code: testCode, Token: good}); !d.Allow {
		t.Errorf("valid token must pass: %s", d.Reason)
	}
	if d := a.Authorize(AuthRequest{Code: testCode, Token: ""}); d.Allow {
		t.Error("missing token under token policy must deny")
	}
	if d := a.Authorize(AuthRequest{Code: testCode, Token: "bogus.sig"}); d.Allow {
		t.Error("bogus token must deny")
	}
	// A token minted for ch1 must not work for another stream.
	if d := a.Authorize(AuthRequest{Code: "ch2", Token: good}); d.Allow {
		t.Error("token bound to ch1 must not authorize ch2")
	}
}

func TestAuthorize_TokenPolicyNoSecretFailsClosed(t *testing.T) {
	t.Parallel()
	a := newAuthz(t, config.MediaAuthConfig{Enabled: true, DefaultPolicy: PolicyToken}, nil, nil) // no secret
	if d := a.Authorize(AuthRequest{Code: testCode, Token: "x.y"}); d.Allow {
		t.Error("token policy without a secret must fail closed")
	}
}

func TestAuthorize_PerStreamPolicyOverride(t *testing.T) {
	t.Parallel()
	// Global default = token, but ch1 is explicitly public → allowed w/o token.
	pol := func(c domain.StreamCode) string {
		if c == "public1" {
			return PolicyPublic
		}
		if c == "token1" {
			return PolicyToken
		}
		return ""
	}
	a := newAuthz(t, config.MediaAuthConfig{Enabled: true, DefaultPolicy: PolicyToken, TokenSecret: "sk"}, nil, pol)

	if d := a.Authorize(AuthRequest{Code: "public1"}); !d.Allow {
		t.Errorf("per-stream public override must allow without token: %s", d.Reason)
	}
	if d := a.Authorize(AuthRequest{Code: "inherits"}); d.Allow {
		t.Error("stream inheriting global token policy must require a token")
	}

	// Global default = public, but token1 explicitly requires a token.
	b := newAuthz(t, config.MediaAuthConfig{Enabled: true, DefaultPolicy: PolicyPublic, TokenSecret: "sk"}, nil, pol)
	if d := b.Authorize(AuthRequest{Code: "token1"}); d.Allow {
		t.Error("per-stream token override must require a token even when global is public")
	}
	if d := b.Authorize(AuthRequest{Code: "other"}); !d.Allow {
		t.Errorf("public-default stream must allow: %s", d.Reason)
	}
}

func TestAuthorize_DenyBeatsAllow(t *testing.T) {
	t.Parallel()
	// IP is on both lists — deny must win.
	a := newAuthz(t, config.MediaAuthConfig{
		Enabled:  true,
		AllowIPs: []string{"1.2.3.4"},
		DenyIPs:  []string{"1.2.3.4"},
	}, nil, nil)
	if d := a.Authorize(AuthRequest{Code: testCode, ClientIP: ip("1.2.3.4")}); d.Allow {
		t.Error("deny must take precedence over allow")
	}
}

func TestAuthorize_HotReload(t *testing.T) {
	t.Parallel()
	a := newAuthz(t, config.MediaAuthConfig{Enabled: false}, nil, nil)
	if d := a.Authorize(AuthRequest{Code: testCode, ClientIP: ip("8.8.8.8")}); !d.Allow {
		t.Fatal("disabled should allow")
	}
	a.SetConfig(config.MediaAuthConfig{Enabled: true, DenyIPs: []string{"8.8.8.8"}})
	if d := a.Authorize(AuthRequest{Code: testCode, ClientIP: ip("8.8.8.8")}); d.Allow {
		t.Error("after reload, denied IP must be blocked")
	}
}
