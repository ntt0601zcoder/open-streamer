package rtmp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
)

// AMF0 type markers.
const (
	amfNumber      = 0x00
	amfBoolean     = 0x01
	amfString      = 0x02
	amfObject      = 0x03
	amfNull        = 0x05
	amfUndefined   = 0x06
	amfReference   = 0x07
	amfECMAArray   = 0x08
	amfObjectEnd   = 0x09
	amfStrictArray = 0x0A
	amfDate        = 0x0B
	amfLongString  = 0x0C
	amfUnsupported = 0x0D
	amfXMLDoc      = 0x0F
	amfTypedObject = 0x10
	amfAVMPlus     = 0x11
)

// Decoder bounds for untrusted input.
const (
	amfMaxDepth   = 16
	amfMaxString  = 1 << 20
	amfMaxValues  = 4096
	amfShortLimit = 0xFFFF
)

// Property is one named member of an Object or ECMAArray.
type Property struct {
	Key   string
	Value any
}

// Object is an ordered AMF0 object.
type Object []Property

// ECMAArray is an AMF0 associative array.
type ECMAArray Object

// Null is the AMF0 null value.
type Null struct{}

// Undefined is the AMF0 undefined value.
type Undefined struct{}

// Get returns the value of the first property named key.
func (o Object) Get(key string) (any, bool) {
	for _, p := range o {
		if p.Key == key {
			return p.Value, true
		}
	}
	return nil, false
}

// EncodeAMF0 encodes vals in order. Supported: float64, int (as number), bool,
// string, Object, ECMAArray, []any (strict array), time.Time (date), Null,
// Undefined and nil (null).
func EncodeAMF0(vals ...any) ([]byte, error) {
	var out []byte
	var err error
	for _, v := range vals {
		if out, err = appendAMF0(out, v, 0); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func appendAMF0(b []byte, v any, depth int) ([]byte, error) {
	if depth > amfMaxDepth {
		return nil, errors.New("rtmp: amf0: encode: nesting too deep")
	}
	switch x := v.(type) {
	case nil, Null:
		return append(b, amfNull), nil
	case Undefined:
		return append(b, amfUndefined), nil
	case float64:
		return appendNumber(b, x), nil
	case int:
		return appendNumber(b, float64(x)), nil
	case bool:
		if x {
			return append(b, amfBoolean, 1), nil
		}
		return append(b, amfBoolean, 0), nil
	case string:
		if len(x) > amfShortLimit {
			b = append(b, amfLongString)
			b = binary.BigEndian.AppendUint32(b, uint32(len(x))) //nolint:gosec // bounded by memory, string fits uint32 in practice
			return append(b, x...), nil
		}
		b = append(b, amfString)
		return appendShortString(b, x), nil
	case time.Time:
		b = append(b, amfDate)
		b = binary.BigEndian.AppendUint64(b, math.Float64bits(float64(x.UnixMilli())))
		return append(b, 0, 0), nil
	case Object:
		b = append(b, amfObject)
		return appendProps(b, x, depth)
	case ECMAArray:
		b = append(b, amfECMAArray)
		b = binary.BigEndian.AppendUint32(b, uint32(len(x))) //nolint:gosec // property count
		return appendProps(b, Object(x), depth)
	case []any:
		b = append(b, amfStrictArray)
		b = binary.BigEndian.AppendUint32(b, uint32(len(x))) //nolint:gosec // element count
		var err error
		for _, e := range x {
			if b, err = appendAMF0(b, e, depth+1); err != nil {
				return nil, err
			}
		}
		return b, nil
	default:
		return nil, fmt.Errorf("rtmp: amf0: encode: unsupported type %T", v)
	}
}

func appendNumber(b []byte, f float64) []byte {
	b = append(b, amfNumber)
	return binary.BigEndian.AppendUint64(b, math.Float64bits(f))
}

func appendShortString(b []byte, s string) []byte {
	b = binary.BigEndian.AppendUint16(b, uint16(len(s))) //nolint:gosec // callers ensure len <= 0xFFFF
	return append(b, s...)
}

func appendProps(b []byte, props Object, depth int) ([]byte, error) {
	var err error
	for _, p := range props {
		if len(p.Key) > amfShortLimit {
			return nil, errors.New("rtmp: amf0: encode: property key too long")
		}
		b = appendShortString(b, p.Key)
		if b, err = appendAMF0(b, p.Value, depth+1); err != nil {
			return nil, err
		}
	}
	return append(b, 0, 0, amfObjectEnd), nil
}

// DecodeAMF0 decodes every value in b. Numbers become float64, dates
// time.Time, objects Object, ECMA arrays ECMAArray, strict arrays []any, null
// Null and undefined Undefined. Reference, AMF3-switch and unsupported markers
// are errors. Nesting is limited to 16 levels, strings to 1 MiB and the total
// value count to 4096.
func DecodeAMF0(b []byte) ([]any, error) {
	d := amfDecoder{b: b}
	var out []any
	for len(d.b) > 0 {
		v, err := d.value(0)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

type amfDecoder struct {
	b []byte
	n int // values decoded so far, nested included
}

func (d *amfDecoder) take(n int) ([]byte, error) {
	if n < 0 || n > len(d.b) {
		return nil, errors.New("rtmp: amf0: decode: truncated input")
	}
	s := d.b[:n]
	d.b = d.b[n:]
	return s, nil
}

func (d *amfDecoder) str(n uint32) (string, error) {
	if n > amfMaxString {
		return "", errors.New("rtmp: amf0: decode: string too long")
	}
	s, err := d.take(int(n))
	if err != nil {
		return "", err
	}
	return string(s), nil
}

func (d *amfDecoder) shortStr() (string, error) {
	h, err := d.take(2)
	if err != nil {
		return "", err
	}
	return d.str(uint32(binary.BigEndian.Uint16(h)))
}

func (d *amfDecoder) value(depth int) (any, error) {
	if depth > amfMaxDepth {
		return nil, errors.New("rtmp: amf0: decode: nesting too deep")
	}
	if d.n++; d.n > amfMaxValues {
		return nil, errors.New("rtmp: amf0: decode: too many values")
	}
	m, err := d.take(1)
	if err != nil {
		return nil, err
	}
	switch m[0] {
	case amfNumber:
		p, err := d.take(8)
		if err != nil {
			return nil, err
		}
		return math.Float64frombits(binary.BigEndian.Uint64(p)), nil
	case amfBoolean:
		p, err := d.take(1)
		if err != nil {
			return nil, err
		}
		return p[0] != 0, nil
	case amfString:
		return d.shortStr()
	case amfLongString, amfXMLDoc:
		p, err := d.take(4)
		if err != nil {
			return nil, err
		}
		return d.str(binary.BigEndian.Uint32(p))
	case amfNull:
		return Null{}, nil
	case amfUndefined:
		return Undefined{}, nil
	case amfObject:
		return d.props(depth)
	case amfTypedObject:
		if _, err := d.shortStr(); err != nil {
			return nil, err
		}
		return d.props(depth)
	case amfECMAArray:
		if _, err := d.take(4); err != nil {
			return nil, err
		}
		o, err := d.props(depth)
		return ECMAArray(o), err
	case amfStrictArray:
		return d.strictArray(depth)
	case amfDate:
		p, err := d.take(10)
		if err != nil {
			return nil, err
		}
		ms := math.Float64frombits(binary.BigEndian.Uint64(p))
		if math.IsNaN(ms) || math.IsInf(ms, 0) || math.Abs(ms) > 1e15 {
			return nil, errors.New("rtmp: amf0: decode: date out of range")
		}
		return time.UnixMilli(int64(ms)).UTC(), nil
	case amfReference, amfUnsupported, amfAVMPlus:
		return nil, fmt.Errorf("rtmp: amf0: decode: unsupported marker %#x", m[0])
	default:
		return nil, fmt.Errorf("rtmp: amf0: decode: unknown marker %#x", m[0])
	}
}

func (d *amfDecoder) strictArray(depth int) (any, error) {
	p, err := d.take(4)
	if err != nil {
		return nil, err
	}
	count := binary.BigEndian.Uint32(p)
	// Each element takes at least one byte, so a count beyond the remaining
	// input (or the value limit) cannot be satisfied; never pre-allocate it.
	if count > uint32(len(d.b)) || count > amfMaxValues {
		return nil, errors.New("rtmp: amf0: decode: strict array count exceeds input")
	}
	out := make([]any, 0, count)
	for range count {
		v, err := d.value(depth + 1)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (d *amfDecoder) props(depth int) (Object, error) {
	var o Object
	for {
		k, err := d.shortStr()
		if err != nil {
			return nil, err
		}
		if k == "" && len(d.b) > 0 && d.b[0] == amfObjectEnd {
			d.b = d.b[1:]
			if o == nil {
				o = Object{}
			}
			return o, nil
		}
		v, err := d.value(depth + 1)
		if err != nil {
			return nil, err
		}
		o = append(o, Property{Key: k, Value: v})
	}
}
