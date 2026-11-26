package stream

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/spec"
)

// The conflict rule alone (R-D11).
func TestConflict(t *testing.T) {
	const p = 5000
	udp := func(host string, port int) Claim { return Claim{UDP: true, Host: host, Port: port} }
	tcp := func(host string, port int) Claim { return Claim{Host: host, Port: port} }
	pub := func(host, path string) Claim { return Claim{RTMP: true, Host: host, Port: p, Path: path} }
	play := func(host string) Claim { return Claim{RTMP: true, Host: host, Port: p} }
	rows := []struct {
		name string
		a, b Claim
		want bool
	}{
		{"udp wildcard twice", udp("", p), udp("", p), true},
		{"udp wildcard and a host", udp("", p), udp("127.0.0.1", p), true},
		{"udp one host twice", udp("10.0.0.1", p), udp("10.0.0.1", p), true},
		{"rtmp one path twice", pub("127.0.0.1", "a"), pub("127.0.0.1", "a"), true},
		{"rtmp wildcard and a host, other paths", pub("", "a"), pub("127.0.0.1", "b"), true},
		{"rtmp and the http listener", pub("", "a"), tcp("", p), true},
		{"rtmp on a host and the play listener on the wildcard", pub("127.0.0.1", "a"), play(""), true},

		{"different ports", udp("", p), udp("", p+1), false},
		{"tcp and udp", tcp("", p), udp("", p), false},
		{"udp two hosts", udp("10.0.0.1", p), udp("10.0.0.2", p), false},
		{"rtmp two paths of one listener", pub("127.0.0.1", "a"), pub("127.0.0.1", "b"), false},
		{"rtmp and the play listener of its address", pub("127.0.0.1", "a"), play("127.0.0.1"), false},
		{"rtmp wildcard and the wildcard play listener", pub("", "a"), play(""), false},
	}
	for _, r := range rows {
		assert.Equal(t, r.want, conflict(r.a, r.b), r.name)
		assert.Equal(t, r.want, conflict(r.b, r.a), "%s, reversed", r.name)
	}
}

// One claim per listen input, and none for a pull or a multicast group.
func TestClaimsOf(t *testing.T) {
	rows := []struct {
		url  string
		want []Claim
	}{
		{"rtmp://:1935/in/k?mode=listen", []Claim{{RTMP: true, Port: 1935, Path: "in/k", Owner: "cam"}}},
		{"rtmp://0.0.0.0/in/k?mode=listener", []Claim{{RTMP: true, Port: 1935, Path: "in/k", Owner: "cam"}}},
		{"rtmp://127.0.0.1:1936/in/k?mode=listen", []Claim{{RTMP: true, Host: "127.0.0.1", Port: 1936, Path: "in/k", Owner: "cam"}}},
		{"rtmp://h/in/k", nil},
		{"udp://:5000", []Claim{{UDP: true, Port: 5000, Owner: "cam"}}},
		{"udp://@:5000", []Claim{{UDP: true, Port: 5000, Owner: "cam"}}},
		{"udp://[::]:5000", []Claim{{UDP: true, Port: 5000, Owner: "cam"}}},
		{"udp://127.0.0.1:5000", []Claim{{UDP: true, Host: "127.0.0.1", Port: 5000, Owner: "cam"}}},
		{"srt://0.0.0.0:5000?mode=listener", []Claim{{UDP: true, Port: 5000, Owner: "cam"}}},
		{"srt://h:5000", nil},
		{"udp://239.1.1.1:5000", nil},
		{"udp://[ff02::1]:5000", nil},
		{"rtsp://h/cam", nil},
		{"http://h/cam.ts", nil},
		{"copy://other", nil},
		{"udp://:0", nil}, // does not parse: Check's to refuse
	}
	for _, r := range rows {
		sp := spec.Stream{Name: "cam", Inputs: []spec.Input{{URL: r.url}}}
		assert.Equal(t, r.want, claimsOf(sp), r.url)
	}
	both := spec.Stream{Name: "cam", Inputs: []spec.Input{{URL: "http://h/cam.ts"}, {URL: "udp://:5000"}, {URL: "srt://:5001?mode=listener"}}}
	assert.Equal(t, []Claim{{UDP: true, Port: 5000, Owner: "cam"}, {UDP: true, Port: 5001, Owner: "cam"}}, claimsOf(both))
}

// copy://x and copy://x_label read x; the walk reports the path.
func TestCopyCycle(t *testing.T) {
	in := func(name string, urls ...string) spec.Stream {
		sp := spec.Stream{Name: name}
		for _, u := range urls {
			sp.Inputs = append(sp.Inputs, spec.Input{URL: u})
		}
		return sp
	}
	assert.Equal(t, []string{"b", "c"}, copyTargets(in("a", "http://h/x.ts", "copy://b", "copy://c_720")))
	set := map[string]spec.Stream{
		"a": in("a", "copy://nope", "copy://b"),
		"b": in("b", "copy://d", "copy://c_720"),
		"c": in("c", "copy://a"),
		"d": in("d", "http://h/x.ts"),
		"e": in("e", "copy://a"),
	}
	for name, want := range map[string]string{
		"a": "copy cycle a -> b -> c -> a", "b": "copy cycle b -> c -> a -> b", "c": "copy cycle c -> a -> b -> c",
	} {
		err := cycleError(name, set)
		if assert.NotNil(t, err, name) {
			assert.Equal(t, want, err.msg)
			assert.ErrorIs(t, err, ErrConflict)
		}
	}
	assert.Nil(t, cycleError("d", set))
	assert.Nil(t, cycleError("e", set), "e reads a cycle and is not on it")
	self := map[string]spec.Stream{"a": in("a", "copy://a_720")}
	assert.Equal(t, "copy cycle a -> a", cycleError("a", self).msg)
}

// An RTMPS claim shares its listener only with RTMPS claims: the server's
// RTMPS listener and a stream's rtmps listen input on it share; two rtmps
// listen inputs of one path conflict; a plain rtmp listen input on the RTMPS
// address, and an rtmps one on the plain address, conflict as an address.
// Every ErrConflict names the address and the holder, never a path.
func TestClaimsTLS(t *testing.T) {
	const key = "pub/" + "sekrit" + "key123"
	serverTLS := Claim{RTMP: true, TLS: true, Port: 1936, Owner: serverOwner}
	serverPlain := Claim{RTMP: true, Port: 1935, Owner: serverOwner}
	held := []Claim{serverTLS, serverPlain}
	stream := func(name string, urls ...string) spec.Stream {
		sp := spec.Stream{Name: name}
		for _, u := range urls {
			sp.Inputs = append(sp.Inputs, spec.Input{URL: u})
		}
		return sp
	}

	// The claims of rtmps listen inputs; the default port is 443.
	assert.Equal(t, []Claim{
		{RTMP: true, TLS: true, Port: 1936, Path: key, Owner: "a"},
		{RTMP: true, TLS: true, Host: "127.0.0.1", Port: 443, Path: key, Owner: "a"},
	}, claimsOf(stream("a", "rtmps://0.0.0.0:1936/"+key+"?mode=listen", "RTMPS://127.0.0.1/"+key+"?mode=listener")))
	assert.Empty(t, claimsOf(stream("a", "rtmps://h:1936/"+key, "rtmps+insecure://:1936/"+key+"?mode=listen")),
		"a pull claims nothing, and an rtmps+insecure listen input does not parse")

	assert.False(t, conflict(serverTLS, Claim{RTMP: true, TLS: true, Port: 1936, Path: key}), "the RTMPS listener and its publish path")
	assert.True(t, conflict(Claim{RTMP: true, TLS: true, Port: 1936, Path: key}, Claim{RTMP: true, TLS: true, Port: 1936, Path: key}))
	assert.True(t, conflict(serverTLS, Claim{RTMP: true, Port: 1936, Path: key}), "a plain path on the RTMPS listener")
	assert.True(t, conflict(serverPlain, Claim{RTMP: true, TLS: true, Port: 1935, Path: key}), "an RTMPS path on the plain listener")
	assert.True(t, conflict(Claim{RTMP: true, TLS: true, Port: 1936}, Claim{RTMP: true, Port: 1936}), "two play listeners of both kinds")

	rows := []struct {
		name    string
		claims  []Claim
		addr    string // "" accepts
		holder  string
		message string
	}{
		{"an rtmps listen input on the RTMPS listener", claimsOf(stream("a", "rtmps://:1936/"+key+"?mode=listen")), "", "", ""},
		{"two rtmps listen inputs of one path", append(
			claimsOf(stream("a", "rtmps://:1936/"+key+"?mode=listen")),
			claimsOf(stream("b", "rtmps://:1936/"+key+"?mode=listen"))...,
		), "tcp :1936", "stream \"a\"", "has the RTMP publish path of an input of"},
		{
			"a plain rtmp listen input on the RTMPS address", claimsOf(stream("a", "rtmp://:1936/"+key+"?mode=listen")),
			"tcp :1936", "the server", "conflicts with the RTMPS listener tcp :1936 of",
		},
		{
			"an rtmps listen input on the plain RTMP address", claimsOf(stream("a", "rtmps://:1935/"+key+"?mode=listen")),
			"tcp :1935", "the server", "conflicts with the RTMP listener tcp :1935 of",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			require.NotEmpty(t, row.claims)
			errs := claimErrors("a", row.claims, held)
			if row.addr == "" {
				assert.Empty(t, errs)
				return
			}
			require.Len(t, errs, 1)
			err := errs[0]
			require.ErrorIs(t, err, ErrConflict)
			assert.Contains(t, err.Error(), row.message)
			assert.Contains(t, err.Error(), "listen address "+row.addr)
			assert.Contains(t, err.Error(), row.holder)
			assert.NotContains(t, err.Error(), key)
			assert.NotContains(t, err.Error(), "sekrit")
		})
	}

	// Through the Manager, with the server's listeners reserved.
	r := newMrig(t, func(env *Env) {
		env.RTMPSPlay = ":1936"
		env.Reserved = []Claim{{RTMP: true, TLS: true, Port: 1936}, {RTMP: true, Port: 1935}}
	})
	a := withInputs(named("a"), "rtmps://:1936/"+key+"?mode=listen")
	assert.Equal(t, outcomes("a", "created"), r.replace(a))
	err := r.refuse(a, withInputs(named("b"), "rtmps://0.0.0.0:1936/"+key+"?mode=listen"))
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), `stream "b": listen address tcp :1936 has the RTMP publish path of an input of stream "a"`)
	err = r.refuse(a, withInputs(named("c"), "rtmp://:1936/in/x?mode=listen"))
	require.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, err.Error(), `stream "c": listen address tcp :1936 conflicts with the RTMPS listener tcp :1936 of the server`)
	assert.False(t, errors.Is(err, ErrInvalid), "Check passes it: the conflict is the claims'")
	assert.NotContains(t, err.Error(), key)
}
