//go:build libav

package native

// #cgo pkg-config: libavcodec libavutil
// #include <string.h>
// #include <libavcodec/avcodec.h>
//
// // osv2_fill_packet makes pkt own a copy of data with
// // AV_INPUT_BUFFER_PADDING_SIZE zeroed bytes after it: av_new_packet pads
// // and zeroes the padding.
// static int osv2_fill_packet(AVPacket *pkt, const void *data, int size, int64_t pts, int64_t dts) {
// 	int ret = av_new_packet(pkt, size);
// 	if (ret < 0)
// 		return ret;
// 	if (size > 0)
// 		memcpy(pkt->data, data, size);
// 	pkt->pts = pts;
// 	pkt->dts = dts;
// 	return 0;
// }
//
// static void osv2_set_pkt_timebase(AVCodecContext *cc, int num, int den) {
// 	cc->pkt_timebase = av_make_q(num, den);
// }
//
// // End of the cgo helpers.
import "C"

import (
	"errors"
	"math"
	"unsafe"

	"github.com/asticode/go-astiav"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// inputPadding is AV_INPUT_BUFFER_PADDING_SIZE: the zeroed bytes a decoder
// may read past the end of a packet.
const inputPadding = C.AV_INPUT_BUFFER_PADDING_SIZE

// inPacket is a decoder input packet. Unlike astiav.Packet.FromData, which
// allocates exactly the data's length, it keeps the padding every decoder
// and bitstream reader relies on.
type inPacket struct{ c *C.AVPacket }

func newInPacket() *inPacket { return &inPacket{c: C.av_packet_alloc()} }

// fill replaces the packet's payload with a padded copy of data.
func (p *inPacket) fill(data []byte, pts, dts int64) error {
	if p.c == nil {
		return errors.New("packet: not allocated")
	}
	if len(data) > math.MaxInt32-inputPadding {
		return errors.New("packet: too large")
	}
	C.av_packet_unref(p.c)
	var ptr unsafe.Pointer
	if len(data) > 0 {
		ptr = unsafe.Pointer(&data[0])
	}
	if ret := C.osv2_fill_packet(p.c, ptr, C.int(len(data)), C.int64_t(pts), C.int64_t(dts)); ret < 0 {
		return astiav.Error(ret)
	}
	return nil
}

// send sends the packet to a decoder and releases its payload.
func (p *inPacket) send(cc *astiav.CodecContext) error {
	ret := C.avcodec_send_packet((*C.AVCodecContext)(cc.UnsafePointer()), p.c)
	C.av_packet_unref(p.c)
	if ret < 0 {
		return astiav.Error(ret)
	}
	return nil
}

// padding returns a copy of the inputPadding bytes after the payload.
func (p *inPacket) padding() []byte {
	if p.c == nil || p.c.data == nil {
		return nil
	}
	return C.GoBytes(unsafe.Add(unsafe.Pointer(p.c.data), int(p.c.size)), inputPadding)
}

func (p *inPacket) free() {
	if p.c != nil {
		C.av_packet_free(&p.c)
	}
}

// setPktTimebase sets a decoder's packet time base to 1/90000.
func setPktTimebase(cc *astiav.CodecContext) {
	C.osv2_set_pkt_timebase((*C.AVCodecContext)(cc.UnsafePointer()), 1, media.ClockRate)
}
