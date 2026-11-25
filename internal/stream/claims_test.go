package stream

import (
	"testing"

	"github.com/stretchr/testify/assert"

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
