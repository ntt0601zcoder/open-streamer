package h26x

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
	"github.com/stretchr/testify/require"
)

// bitWriter builds RBSP bit strings for hand-made parameter sets.
type bitWriter struct {
	buf  []byte
	nbit int
}

func (w *bitWriter) u(n int, v uint64) *bitWriter {
	for i := n - 1; i >= 0; i-- {
		if w.nbit%8 == 0 {
			w.buf = append(w.buf, 0)
		}
		if v>>uint(i)&1 == 1 {
			w.buf[len(w.buf)-1] |= 0x80 >> uint(w.nbit%8)
		}
		w.nbit++
	}
	return w
}

func (w *bitWriter) flag(b bool) *bitWriter {
	if b {
		return w.u(1, 1)
	}
	return w.u(1, 0)
}

func (w *bitWriter) ue(v uint64) *bitWriter {
	v++
	n := 0
	for x := v; x > 1; x >>= 1 {
		n++
	}
	w.u(n, 0)
	return w.u(n+1, v)
}

func (w *bitWriter) se(v int64) *bitWriter {
	if v > 0 {
		return w.ue(uint64(2*v - 1))
	}
	return w.ue(uint64(-2 * v))
}

// nal appends rbsp_trailing_bits and returns the NAL unit: header bytes
// followed by the payload with emulation prevention bytes inserted.
func (w *bitWriter) nal(header ...byte) []byte {
	w.u(1, 1)
	for w.nbit%8 != 0 {
		w.u(1, 0)
	}
	out := append([]byte{}, header...)
	zeros := 0
	for _, b := range w.buf {
		if zeros >= 2 && b <= 3 {
			out = append(out, 3)
			zeros = 0
		}
		out = append(out, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// splitAnnexB returns the NAL units of an Annex B byte stream.
func splitAnnexB(b []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+2 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				out = append(out, bytes.TrimRight(b[start:i], "\x00"))
			}
			start = i + 3
			i += 2
		}
	}
	if start >= 0 && start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

// fixtureSets returns the distinct SPS and PPS NAL units of a fixture's
// video track, in order of appearance.
func fixtureSets(t *testing.T, name string, c Codec) (sps, pps [][]byte) {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", "fixtures", name))
	require.NoError(t, err)
	defer f.Close()
	d := mpegts.NewDemux(f)
	var video uint16
	seen := map[string]bool{}
	for {
		ev, err := d.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if ev.PMT != nil {
			for _, s := range ev.PMT.Streams {
				if s.Type == 0x1b || s.Type == 0x24 {
					video = s.PID
				}
			}
			continue
		}
		if ev.PES == nil || ev.PES.PID != video {
			continue
		}
		for _, n := range splitAnnexB(ev.PES.Data) {
			if len(n) < 2 || seen[string(n)] {
				continue
			}
			var typ byte
			if c == H264 {
				typ = n[0] & 0x1f
			} else {
				typ = (n[0] >> 1) & 0x3f
			}
			switch {
			case (c == H264 && typ == 7) || (c == H265 && typ == 33):
				sps = append(sps, n)
			case (c == H264 && typ == 8) || (c == H265 && typ == 34):
				pps = append(pps, n)
			default:
				continue
			}
			seen[string(n)] = true
		}
	}
	require.NotEmpty(t, sps, name)
	require.NotEmpty(t, pps, name)
	return sps, pps
}
