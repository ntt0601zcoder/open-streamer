package rtsp

import (
	"time"

	"github.com/pion/rtp"
)

// aacSampleRates is the sampling_frequency_index table of ISO/IEC 14496-3.
var aacSampleRates = [13]uint32{
	96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350,
}

const (
	aacFrameSamples = 1024
	aacOTEscape     = 31
	aacFreqExplicit = 15
	aacOTSBR        = 5
	aacOTPS         = 29
)

// samplesPerAU returns the RTP clock ticks one AAC frame lasts: 1024 samples
// at the ASC's core sampling rate, scaled to the RTP clock (so 2048 for an
// HE-AAC stream sent at the SBR clock). It falls back to 1024 when the
// config cannot be read.
func samplesPerAU(f *AACFormat, clock uint32) uint32 {
	rate := ascRate(f.Config)
	if rate == 0 || clock == 0 {
		return aacFrameSamples
	}
	n := aacFrameSamples * clock / rate
	if n == 0 {
		return aacFrameSamples
	}
	return n
}

// ascRate reads the core sampling frequency from an AudioSpecificConfig.
func ascRate(cfg []byte) uint32 {
	r := bitReader{b: cfg, n: len(cfg) * 8}
	if ot := r.read(5); ot == aacOTEscape {
		r.read(6)
	}
	idx := r.read(4)
	var rate uint32
	if idx == aacFreqExplicit {
		rate = r.read(24)
	} else if int(idx) < len(aacSampleRates) {
		rate = aacSampleRates[idx]
	}
	if r.err || rate == 0 {
		return 0
	}
	return rate
}

// bitReader reads big-endian bit fields; a read past the end sets err.
type bitReader struct {
	b   []byte
	pos int // bit position
	n   int // total readable bits
	err bool
}

func (r *bitReader) left() int { return r.n - r.pos }

func (r *bitReader) read(n int) uint32 {
	if n < 0 || n > 32 || r.pos+n > r.n {
		r.err = true
		r.pos = r.n
		return 0
	}
	var v uint64
	for i := 0; i < n; i++ {
		p := r.pos + i
		v = v<<1 | uint64(r.b[p>>3]>>(7-p&7)&1)
	}
	r.pos += n
	return uint32(v)
}

func (r *bitReader) skip(n int) { r.read(n) }

// bitWriter writes big-endian bit fields.
type bitWriter struct {
	buf []byte
	n   int // bits written
}

func (w *bitWriter) write(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.n&7 == 0 {
			w.buf = append(w.buf, 0)
		}
		if v>>i&1 != 0 {
			w.buf[w.n>>3] |= 0x80 >> (w.n & 7)
		}
		w.n++
	}
}

// pad advances to the next byte boundary with zero bits.
func (w *bitWriter) pad() {
	if w.n&7 != 0 {
		w.n += 8 - w.n&7
	}
}

func (w *bitWriter) bytes() []byte { return w.buf }

// aacDepack implements RFC 3640 for mpeg4-generic AAC. Every access unit
// becomes one AU with a single entry in Frames.
type aacDepack struct {
	f       *AACFormat
	index   int
	maxAU   int
	step    uint32
	lost    bool
	oversze uint64

	frag     []byte
	fragOn   bool
	fragTS   uint32
	fragSize int
}

func newAACDepack(m Media, maxAU int) *aacDepack {
	return &aacDepack{f: m.AAC, index: m.Index, maxAU: maxAU, step: samplesPerAU(m.AAC, m.ClockRate)}
}

// Oversize implements OversizeCounter.
func (d *aacDepack) Oversize() uint64 { return d.oversze }

// Flush implements Depacketizer; AAC frames are emitted as they complete.
func (d *aacDepack) Flush() []AU { return nil }

// Gap implements Depacketizer.
func (d *aacDepack) Gap() {
	d.frag, d.fragOn = nil, false
	d.lost = true
}

type aacHeader struct {
	size       int
	interleave bool
}

// parseHeaders reads the AU-headers section of pl; it returns the headers
// and the offset of the first AU byte.
func (d *aacDepack) parseHeaders(pl []byte) (hs []aacHeader, dataOff int, ok bool) {
	if len(pl) < 2 {
		return nil, 0, false
	}
	bits := int(pl[0])<<8 | int(pl[1])
	nbytes := (bits + 7) / 8
	if 2+nbytes > len(pl) {
		return nil, 0, false
	}
	r := bitReader{b: pl[2 : 2+nbytes], n: bits}
	f := d.f
	for i := 0; r.left() > 0; i++ {
		h := aacHeader{size: int(r.read(f.SizeLength))}
		il := f.IndexDeltaLength
		if i == 0 {
			il = f.IndexLength
		}
		h.interleave = r.read(il) != 0
		if f.CTSDeltaLength > 0 && r.read(1) == 1 {
			r.skip(f.CTSDeltaLength)
		}
		if f.DTSDeltaLength > 0 && r.read(1) == 1 {
			r.skip(f.DTSDeltaLength)
		}
		r.skip(f.RAPLength)
		if r.err {
			return nil, 0, false
		}
		hs = append(hs, h)
	}
	off := 2 + nbytes
	if f.AuxLength > 0 {
		ar := bitReader{b: pl[off:], n: (len(pl) - off) * 8}
		auxBits := int(ar.read(f.AuxLength))
		if ar.err {
			return nil, 0, false
		}
		off += (f.AuxLength + auxBits + 7) / 8
		if off > len(pl) {
			return nil, 0, false
		}
	}
	return hs, off, len(hs) > 0
}

// Push implements Depacketizer.
func (d *aacDepack) Push(p *rtp.Packet, arrival time.Time) []AU {
	hs, off, ok := d.parseHeaders(p.Payload)
	if !ok {
		d.lost = true
		return nil
	}
	data := p.Payload[off:]
	if d.fragOn {
		if out, handled := d.continueFrag(p, hs, data, arrival); handled {
			return out
		}
	}
	if len(hs) == 1 && len(data) < hs[0].size {
		d.startFrag(p, hs[0], data)
		return nil
	}
	total := 0
	for _, h := range hs {
		total += h.size
	}
	if total > len(data) {
		d.lost = true
		return nil
	}
	var out []AU
	for i, h := range hs {
		fr := data[:h.size]
		data = data[h.size:]
		if h.interleave || h.size == 0 {
			continue
		}
		if h.size > d.maxAU {
			d.oversze++
			continue
		}
		out = append(out, d.emit(p.Timestamp+uint32(i)*d.step, append([]byte(nil), fr...), arrival))
	}
	return out
}

func (d *aacDepack) emit(ts uint32, frame []byte, arrival time.Time) AU {
	au := AU{Track: d.index, Timestamp: ts, Frames: [][]byte{frame}, Lost: d.lost, Arrival: arrival}
	d.lost = false
	return au
}

// continueFrag appends p to the fragmented AU in progress. handled is false
// when p does not continue it, in which case the fragment was dropped.
func (d *aacDepack) continueFrag(p *rtp.Packet, hs []aacHeader, data []byte, arrival time.Time) (out []AU, handled bool) {
	if len(hs) != 1 || p.Timestamp != d.fragTS || hs[0].size != d.fragSize ||
		len(d.frag)+len(data) > d.fragSize {
		d.frag, d.fragOn = nil, false
		d.lost = true
		return nil, false
	}
	d.frag = append(d.frag, data...)
	if len(d.frag) < d.fragSize {
		return nil, true
	}
	fr := d.frag
	d.frag, d.fragOn = nil, false
	return []AU{d.emit(d.fragTS, fr, arrival)}, true
}

func (d *aacDepack) startFrag(p *rtp.Packet, h aacHeader, data []byte) {
	if h.interleave {
		return
	}
	if h.size > d.maxAU {
		d.oversze++
		return
	}
	d.frag = append(make([]byte, 0, h.size), data...)
	d.fragOn, d.fragTS, d.fragSize = true, p.Timestamp, h.size
}

// PacketizeAAC packs AAC frames into RFC 3640 AAC-hbr RTP packets. Frame i
// is stamped ts + i*samplesPerAU; mtu bounds the whole RTP packet (default
// 1400). A frame that does not fit a packet is fragmented over several
// packets that share its timestamp. The marker is set on every packet that
// completes at least one frame. Sequence numbers, payload type and SSRC are
// left for the caller. Empty frames, and frames longer than the size field
// can express, are skipped.
func PacketizeAAC(frames [][]byte, ts uint32, samplesPerAU uint32, mtu int, f AACFormat) []*rtp.Packet {
	const rtpHeader = 12
	if mtu <= 0 {
		mtu = 1400
	}
	room := mtu - rtpHeader
	var (
		out     []*rtp.Packet
		batch   [][]byte
		batchTS uint32
		dsum    int
	)
	flush := func() {
		if len(batch) > 0 {
			out = append(out, newAACPacket(batchTS, true, aacPayload(f, batch, 0, nil)))
		}
		batch, dsum = nil, 0
	}
	for i, fr := range frames {
		if len(fr) == 0 || (f.SizeLength < 32 && len(fr) >= 1<<f.SizeLength) {
			flush()
			continue
		}
		fts := ts + uint32(i)*samplesPerAU
		if len(batch) > 0 && aacPayloadLen(f, len(batch)+1, dsum+len(fr)) > room {
			flush()
		}
		if len(batch) == 0 && aacPayloadLen(f, 1, len(fr)) > room {
			chunk := max(room-aacPayloadLen(f, 1, 0), 1)
			for off := 0; off < len(fr); off += chunk {
				end := min(off+chunk, len(fr))
				out = append(out, newAACPacket(fts, end == len(fr), aacPayload(f, nil, len(fr), fr[off:end])))
			}
			continue
		}
		if len(batch) == 0 {
			batchTS = fts
		}
		batch = append(batch, fr)
		dsum += len(fr)
	}
	flush()
	return out
}

func newAACPacket(ts uint32, marker bool, payload []byte) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{Version: 2, Timestamp: ts, Marker: marker}, Payload: payload}
}

// aacHeaderBits is the size of the AU-headers section for n headers.
func aacHeaderBits(f AACFormat, n int) int {
	per := f.SizeLength + f.RAPLength
	if f.CTSDeltaLength > 0 {
		per++
	}
	if f.DTSDeltaLength > 0 {
		per++
	}
	return n*per + f.IndexLength + (n-1)*f.IndexDeltaLength
}

func aacAuxBytes(f AACFormat) int {
	if f.AuxLength == 0 {
		return 0
	}
	return (f.AuxLength + 7) / 8
}

// aacPayloadLen is the payload size for n headers and d data bytes.
func aacPayloadLen(f AACFormat, n, d int) int {
	return 2 + (aacHeaderBits(f, n)+7)/8 + aacAuxBytes(f) + d
}

// aacPayload builds one RTP payload: either the whole frames, or (when
// fragTotal > 0) one fragment of a frame of that size.
func aacPayload(f AACFormat, frames [][]byte, fragTotal int, frag []byte) []byte {
	sizes := make([]int, 0, len(frames)+1)
	for _, fr := range frames {
		sizes = append(sizes, len(fr))
	}
	if fragTotal > 0 {
		sizes = append(sizes, fragTotal)
	}
	var w bitWriter
	for i, sz := range sizes {
		w.write(uint32(sz), f.SizeLength)
		if i == 0 {
			w.write(0, f.IndexLength)
		} else {
			w.write(0, f.IndexDeltaLength)
		}
		if f.CTSDeltaLength > 0 {
			w.write(0, 1)
		}
		if f.DTSDeltaLength > 0 {
			w.write(0, 1)
		}
		w.write(0, f.RAPLength)
	}
	hbits := w.n
	w.pad()
	out := append([]byte{byte(hbits >> 8), byte(hbits)}, w.bytes()...)
	if f.AuxLength > 0 {
		out = append(out, make([]byte, aacAuxBytes(f))...)
	}
	for _, fr := range frames {
		out = append(out, fr...)
	}
	return append(out, frag...)
}
