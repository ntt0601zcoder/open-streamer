package rtsp

import (
	"encoding/base64"
	"strings"
	"testing"
)

func fixedAuth(user, pass, cnonce string) *Auth {
	a := NewAuth(user, pass)
	a.cnonce = func() string { return cnonce }
	return a
}

func challenge(t *testing.T, a *Auth, vals ...string) {
	t.Helper()
	if err := a.Challenge(Header{"WWW-Authenticate": vals}); err != nil {
		t.Fatal(err)
	}
}

func TestDigestKnownAnswer(t *testing.T) {
	t.Run("RFC7616_MD5", func(t *testing.T) {
		a := fixedAuth("Mufasa", "Circle of Life", "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ")
		challenge(t, a, `Digest realm="http-auth@example.org", qop="auth, auth-int", algorithm=MD5, nonce="7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v", opaque="FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS"`)
		got := a.Authorize("GET", "/dir/index.html")
		for _, s := range []string{
			`response="8ca523f5e9506fed4657c9700eebdbec"`,
			`username="Mufasa"`, `nc=00000001`, `qop=auth`, `algorithm=MD5`,
			`opaque="FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS"`, `uri="/dir/index.html"`,
		} {
			if !strings.Contains(got, s) {
				t.Errorf("missing %s in %s", s, got)
			}
		}
		if !strings.HasPrefix(got, "Digest ") {
			t.Errorf("prefix: %s", got)
		}
	})
	t.Run("RFC7616_SHA256", func(t *testing.T) {
		a := fixedAuth("Mufasa", "Circle of Life", "f2/wE4q74E6zIJEtWaHKaf5wv/H5QzzpXusqGemxURZJ")
		challenge(t, a, `Digest realm="http-auth@example.org", qop="auth, auth-int", algorithm=SHA-256, nonce="7ypf/xlj9XXwfDPEoM4URrv/xwf94BcCAzFZH4GiTo0v", opaque="FQhe/qaU925kfnzjCev0ciny7QMkPqMAFRtzCUYo5tdS"`)
		got := a.Authorize("GET", "/dir/index.html")
		if !strings.Contains(got, `response="753927fa0e85d155564e2e272a28d1802ca10daf4496794697cf8db5856cb6c1"`) || !strings.Contains(got, "algorithm=SHA-256") {
			t.Errorf("got %s", got)
		}
	})
	t.Run("RFC2617", func(t *testing.T) {
		a := fixedAuth("Mufasa", "Circle Of Life", "0a4f113b")
		challenge(t, a, `Digest realm="testrealm@host.com", qop="auth,auth-int", nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093", opaque="5ccc069c403ebaf9f0171e9517f40e41"`)
		got := a.Authorize("GET", "/dir/index.html")
		if !strings.Contains(got, `response="6629fae49393a05397450978507c4ef1"`) || strings.Contains(got, "algorithm=") {
			t.Errorf("got %s", got)
		}
	})
	// The response printed in RFC 2069 §2.4 does not reproduce from its own
	// inputs (a known erratum), so this vector was computed independently with
	// md5sum: MD5(MD5(user:realm:pass):nonce:MD5(method:uri)).
	t.Run("RFC2069_noqop", func(t *testing.T) {
		a := fixedAuth("Mufasa", "CircleOfLife", "unused")
		challenge(t, a, `Digest realm="testrealm@host.com", nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093", opaque="5ccc069c403ebaf9f0171e9517f40e41"`)
		got := a.Authorize("GET", "/dir/index.html")
		if !strings.Contains(got, `response="1949323746fe6a43ef61f9606e7febea"`) {
			t.Errorf("got %s", got)
		}
		if strings.Contains(got, "cnonce") || strings.Contains(got, "nc=") || strings.Contains(got, "qop") {
			t.Errorf("qop fields in RFC 2069 response: %s", got)
		}
	})
}

func TestDigestSessAndNC(t *testing.T) {
	for _, alg := range []string{"MD5-sess", "SHA-256-sess"} {
		a := fixedAuth("u", "p", "cn")
		challenge(t, a, `Digest realm="r", nonce="n", qop="auth", algorithm=`+alg)
		first := a.Authorize("DESCRIBE", "rtsp://h/x")
		second := a.Authorize("DESCRIBE", "rtsp://h/x")
		if !strings.Contains(first, "nc=00000001") || !strings.Contains(second, "nc=00000002") {
			t.Errorf("%s nc: %s | %s", alg, first, second)
		}
		if first == second {
			t.Errorf("%s: response did not change with nc", alg)
		}
		if !strings.Contains(first, "algorithm="+alg) {
			t.Errorf("algorithm echoed: %s", first)
		}
	}
	// Fresh nonce restarts nc.
	a := fixedAuth("u", "p", "cn")
	challenge(t, a, `Digest realm="r", nonce="n1", qop="auth"`)
	a.Authorize("PLAY", "rtsp://h/x")
	challenge(t, a, `Digest realm="r", nonce="n2", qop="auth", stale=true`)
	if got := a.Authorize("PLAY", "rtsp://h/x"); !strings.Contains(got, "nc=00000001") || !strings.Contains(got, `nonce="n2"`) {
		t.Errorf("stale: %s", got)
	}
}

func TestRandomCnonce(t *testing.T) {
	a := NewAuth("u", "p")
	challenge(t, a, `Digest realm="r", nonce="n", qop="auth"`)
	c1 := a.Authorize("GET", "/")
	c2 := a.Authorize("GET", "/")
	if c1 == c2 {
		t.Fatal("identical headers")
	}
	get := func(s string) string {
		i := strings.Index(s, `cnonce="`)
		if i < 0 {
			t.Fatalf("no cnonce: %s", s)
		}
		s = s[i+8:]
		return s[:strings.Index(s, `"`)]
	}
	if n1, n2 := get(c1), get(c2); n1 == "" || n1 == n2 || len(n1) < 16 {
		t.Fatalf("cnonces %q %q", n1, n2)
	}
}

func TestAuthBasic(t *testing.T) {
	a := NewAuth("user", "pa:ss")
	if got := a.Authorize("GET", "/"); got != "" {
		t.Fatalf("before challenge: %q", got)
	}
	challenge(t, a, `Basic realm="cam"`)
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:pa:ss"))
	if got := a.Authorize("GET", "/"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestDigestPreferredOverBasic(t *testing.T) {
	a := NewAuth("u", "p")
	challenge(t, a, `Basic realm="b"`, `Digest realm="d", nonce="n"`)
	if got := a.Authorize("GET", "/"); !strings.HasPrefix(got, "Digest ") {
		t.Fatalf("got %q", got)
	}
	a = NewAuth("u", "p")
	challenge(t, a, `Basic realm="b", Digest realm="d", nonce="n"`)
	if got := a.Authorize("GET", "/"); !strings.HasPrefix(got, "Digest ") {
		t.Fatalf("combined: %q", got)
	}
	a = NewAuth("u", "p")
	challenge(t, a, `Digest realm="d", nonce="n", algorithm=MD5`, `Digest realm="d", nonce="n2", algorithm=SHA-256`)
	if got := a.Authorize("GET", "/"); !strings.Contains(got, "algorithm=SHA-256") {
		t.Fatalf("sha256 not preferred: %q", got)
	}
	a = NewAuth("u", "p")
	challenge(t, a, `Digest realm="d", nonce="n", algorithm=BOGUS`, `Digest realm="d", nonce="n", algorithm=MD5`)
	if got := a.Authorize("GET", "/"); !strings.Contains(got, "algorithm=MD5") {
		t.Fatalf("fallback to supported: %q", got)
	}
}

func TestDigestEscapesQuotes(t *testing.T) {
	a := fixedAuth(`us"er`, "p", "c")
	challenge(t, a, `Digest realm="r\"x", nonce="n"`)
	got := a.Authorize("GET", "/")
	if !strings.Contains(got, `username="us\"er"`) || !strings.Contains(got, `realm="r\"x"`) {
		t.Fatalf("got %s", got)
	}
}

// authErrorCases lists a challenge for every error path in auth.go.
func authErrorCases() map[string]Header {
	return map[string]Header{
		"no header":       {},
		"unknown scheme":  {"WWW-Authenticate": {`Negotiate abc`}},
		"empty value":     {"WWW-Authenticate": {``}},
		"unterminated":    {"WWW-Authenticate": {`Digest realm="abc`}},
		"no nonce":        {"WWW-Authenticate": {`Digest realm="r"`}},
		"bad algorithm":   {"WWW-Authenticate": {`Digest realm="r", nonce="n", algorithm=ROT13`}},
		"auth-int only":   {"WWW-Authenticate": {`Digest realm="r", nonce="n", qop="auth-int"`}},
		"stray equals":    {"WWW-Authenticate": {`=x`}},
		"bad escape tail": {"WWW-Authenticate": {`Digest realm="r\`}},
	}
}

func TestAuthErrors(t *testing.T) {
	for name, h := range authErrorCases() {
		if err := NewAuth("u", "p").Challenge(h); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// A failed challenge must not install state.
	a := NewAuth("u", "p")
	_ = a.Challenge(Header{"WWW-Authenticate": {`Digest realm="r"`}})
	if got := a.Authorize("GET", "/"); got != "" {
		t.Fatalf("state after failure: %q", got)
	}
}

func TestAuthNeverLeaks(t *testing.T) {
	const pass = "s3cretPassw0rd!"
	for name, h := range authErrorCases() {
		err := NewAuth("alice", pass).Challenge(h)
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		msg := err.Error()
		if !strings.HasPrefix(msg, "rtsp: ") {
			t.Errorf("%s: prefix: %q", name, msg)
		}
		if strings.Contains(msg, pass) || strings.Contains(msg, base64.StdEncoding.EncodeToString([]byte("alice:"+pass))) {
			t.Errorf("%s: leaks password: %q", name, msg)
		}
		for _, v := range h["WWW-Authenticate"] {
			if v != "" && strings.Contains(msg, v) {
				t.Errorf("%s: echoes challenge: %q", name, msg)
			}
		}
	}
	// The credentials must not appear where the server could echo them back
	// either: a successful Digest header contains the hash, never the password.
	a := fixedAuth("alice", pass, "cn")
	challenge(t, a, `Digest realm="r", nonce="n", qop="auth"`)
	if got := a.Authorize("GET", "/"); strings.Contains(got, pass) {
		t.Fatalf("digest header contains password: %s", got)
	}
}
