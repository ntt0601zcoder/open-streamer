package rtmp

import (
	"bytes"
	"encoding/binary"
	"math"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAMF0RoundTrip(t *testing.T) {
	long := strings.Repeat("x", 70000)
	when := time.UnixMilli(1700000000123).UTC()
	vals := []any{
		float64(1.5), true, false, "hello", long,
		Object{{"a", float64(1)}, {"b", Object{{"c", "d"}}}},
		ECMAArray{{"duration", float64(0)}, {"stereo", true}},
		[]any{float64(1), "two", Null{}},
		Null{},
		Undefined{},
		when,
	}
	b, err := EncodeAMF0(vals...)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeAMF0(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, vals) {
		t.Fatalf("round trip mismatch:\n got %#v\nwant %#v", got, vals)
	}
}

func TestAMF0EncodeConversions(t *testing.T) {
	b, err := EncodeAMF0(7, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeAMF0(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []any{float64(7), Null{}}) {
		t.Fatalf("got %#v", got)
	}
	if _, err := EncodeAMF0(struct{}{}); err == nil {
		t.Fatal("want error for unsupported type")
	}
}

func TestAMF0LongStringMarker(t *testing.T) {
	b, _ := EncodeAMF0(strings.Repeat("a", 65536))
	if b[0] != 0x0C {
		t.Fatalf("marker %#x, want long string", b[0])
	}
	b, _ = EncodeAMF0(strings.Repeat("a", 65535))
	if b[0] != 0x02 {
		t.Fatalf("marker %#x, want string", b[0])
	}
}

func amfStr(s string) []byte {
	out := binary.BigEndian.AppendUint16(nil, uint16(len(s)))
	return append(out, s...)
}

func amfNum(f float64) []byte {
	return binary.BigEndian.AppendUint64([]byte{0x00}, math.Float64bits(f))
}

func amfProp(k string, v []byte) []byte { return append(amfStr(k), v...) }

func amfS(s string) []byte { return append([]byte{0x02}, amfStr(s)...) }

var objEnd = []byte{0x00, 0x00, 0x09}

func TestAMF0DecodeConnectFFmpeg(t *testing.T) {
	b := make([]byte, 0, 256)
	b = append(b, amfS("connect")...)
	b = append(b, amfNum(1)...)
	b = append(b, 0x03)
	b = append(b, amfProp("app", amfS("live"))...)
	b = append(b, amfProp("type", amfS("nonprivate"))...)
	b = append(b, amfProp("flashVer", amfS("FMLE/3.0"))...)
	b = append(b, amfProp("tcUrl", amfS("rtmp://host/live"))...)
	b = append(b, objEnd...)
	got, err := DecodeAMF0(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "connect" || got[1] != float64(1) {
		t.Fatalf("got %#v", got)
	}
	o := got[2].(Object)
	if v, ok := o.Get("flashVer"); !ok || v != "FMLE/3.0" {
		t.Fatalf("flashVer %v %v", v, ok)
	}
	if v, _ := o.Get("app"); v != "live" {
		t.Fatalf("app %v", v)
	}
	if _, ok := o.Get("missing"); ok {
		t.Fatal("missing found")
	}
}

func TestAMF0DecodeConnectOBS(t *testing.T) {
	b := make([]byte, 0, 256)
	b = append(b, amfS("connect")...)
	b = append(b, amfNum(1)...)
	b = append(b, 0x03)
	b = append(b, amfProp("app", amfS("live"))...)
	b = append(b, amfProp("type", amfS("nonprivate"))...)
	b = append(b, amfProp("flashVer", amfS("FMLE/3.0 (compatible; obs)"))...)
	b = append(b, amfProp("swfUrl", amfS("rtmp://host/live"))...)
	b = append(b, amfProp("tcUrl", amfS("rtmp://host/live"))...)
	b = append(b, amfProp("objectEncoding", amfNum(0))...)
	b = append(b, objEnd...)
	got, err := DecodeAMF0(b)
	if err != nil {
		t.Fatal(err)
	}
	o := got[2].(Object)
	if v, _ := o.Get("objectEncoding"); v != float64(0) {
		t.Fatalf("objectEncoding %v", v)
	}
	if v, _ := o.Get("swfUrl"); v != "rtmp://host/live" {
		t.Fatalf("swfUrl %v", v)
	}
}

func TestAMF0DecodeSetDataFrame(t *testing.T) {
	b := make([]byte, 0, 256)
	b = append(b, amfS("@setDataFrame")...)
	b = append(b, amfS("onMetaData")...)
	b = append(b, 0x08, 0, 0, 0, 2)
	b = append(b, amfProp("width", amfNum(1280))...)
	b = append(b, amfProp("stereo", []byte{0x01, 0x01})...)
	b = append(b, objEnd...)
	got, err := DecodeAMF0(b)
	if err != nil {
		t.Fatal(err)
	}
	arr, ok := got[2].(ECMAArray)
	if !ok || len(got) != 3 || got[0] != "@setDataFrame" {
		t.Fatalf("got %#v", got)
	}
	if v, _ := Object(arr).Get("width"); v != float64(1280) {
		t.Fatalf("width %v", v)
	}
	if v, _ := Object(arr).Get("stereo"); v != true {
		t.Fatalf("stereo %v", v)
	}
}

func TestAMF0RejectedMarkers(t *testing.T) {
	for _, m := range []byte{0x07, 0x0D, 0x11, 0x04, 0x0E, 0x09, 0x20} {
		if _, err := DecodeAMF0([]byte{m, 0, 0}); err == nil {
			t.Fatalf("marker %#x accepted", m)
		}
	}
}

func TestAMF0TypedObjectAndXML(t *testing.T) {
	b := append(make([]byte, 0, 64), 0x10)
	b = append(b, amfStr("Cls")...)
	b = append(b, amfProp("k", amfNum(2))...)
	b = append(b, objEnd...)
	b = append(b, 0x0F, 0, 0, 0, 2, 'h', 'i')
	got, err := DecodeAMF0(b)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{Object{{"k", float64(2)}}, "hi"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestAMF0Hostile(t *testing.T) {
	deep := bytes.Repeat([]byte{0x03, 0x00, 0x01, 'a'}, 100)
	manyVals := bytes.Repeat([]byte{0x05}, 5000)
	cases := map[string][]byte{
		"depth":            deep,
		"longstring":       {0x0C, 0xFF, 0xFF, 0xFF, 0xFF},
		"stringtoobig":     append([]byte{0x0C}, binary.BigEndian.AppendUint32(nil, 1<<20+1)...),
		"truncatedstring":  {0x02, 0x00, 0x10, 'a'},
		"truncatednumber":  {0x00, 1, 2, 3},
		"missingobjectend": append([]byte{0x03}, amfProp("a", amfNum(1))...),
		"strictarraybomb":  {0x0A, 0xFF, 0xFF, 0xFF, 0xFF},
		"toomanyvalues":    manyVals,
		"truncateddate":    {0x0B, 0, 0},
		"empty-marker":     {},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			var m0, m1 runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&m0)
			_, err := DecodeAMF0(in)
			runtime.ReadMemStats(&m1)
			if name != "empty-marker" && err == nil {
				t.Fatal("want error")
			}
			if d := m1.TotalAlloc - m0.TotalAlloc; d > 1<<20 {
				t.Fatalf("allocated %d bytes", d)
			}
		})
	}
}

func TestAMF0DecodeEmpty(t *testing.T) {
	got, err := DecodeAMF0(nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestAMF0ValueLimit(t *testing.T) {
	if _, err := DecodeAMF0(bytes.Repeat([]byte{0x05}, 4096)); err != nil {
		t.Fatalf("4096 values rejected: %v", err)
	}
}
