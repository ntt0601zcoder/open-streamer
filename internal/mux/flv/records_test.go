package flv

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/rtmp"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "fixtures", name))
	require.NoError(t, err)
	return data
}

// videoConfig demuxes a fixture and returns its first video Config.
func videoConfig(t *testing.T, name string, codec media.Codec) *media.TrackConfig {
	t.Helper()
	d, err := ts.NewDemuxer(bytes.NewReader(fixture(t, name)))
	require.NoError(t, err)
	for {
		f, err := d.Read()
		if errors.Is(err, io.EOF) {
			t.Fatalf("no %v config in %s", codec, name)
		}
		require.NoError(t, err)
		if f.Config != nil && f.Config.Codec == codec {
			return f.Config
		}
	}
}

func flvSeqHeader(t *testing.T, name string) []byte {
	t.Helper()
	tags, err := rtmp.ReadFLV(bytes.NewReader(fixture(t, name)))
	require.NoError(t, err)
	for _, tag := range tags {
		if tag.Type != rtmp.FLVVideo {
			continue
		}
		v, err := rtmp.ParseVideoTag(tag.Data)
		require.NoError(t, err)
		if v.Kind == rtmp.VideoSequenceStart {
			return v.Data
		}
	}
	t.Fatal("no sequence header")
	return nil
}

func TestAVCRecordFixtures(t *testing.T) {
	for _, name := range []string{"h264_aac.ts", "h264_pyramid.ts"} {
		cfg := videoConfig(t, name, media.CodecH264)
		rec, err := AVCRecord(cfg)
		require.NoError(t, err, name)
		dec, err := avc.DecodeAVCDecConfRec(rec)
		require.NoError(t, err, name)
		require.Equal(t, cfg.SPS, dec.SPSnalus, name)
		require.Equal(t, cfg.PPS, dec.PPSnalus, name)
		require.Equal(t, byte(0xff), rec[4], name)
		if name == "h264_aac.ts" {
			require.Equal(t, flvSeqHeader(t, "h264_aac.flv"), rec)
		}
	}
}

func TestHEVCRecordFixtures(t *testing.T) {
	cfg := videoConfig(t, "h265_aac51.ts", media.CodecH265)
	rec, err := HEVCRecord(cfg)
	require.NoError(t, err)
	dec, err := hevc.DecodeHEVCDecConfRec(rec)
	require.NoError(t, err)
	require.Equal(t, uint8(3), dec.LengthSizeMinusOne)
	require.Equal(t, cfg.VPS, dec.GetNalusForType(hevc.NALU_VPS))
	require.Equal(t, cfg.SPS, dec.GetNalusForType(hevc.NALU_SPS))
	require.Equal(t, cfg.PPS, dec.GetNalusForType(hevc.NALU_PPS))
	for _, a := range dec.NaluArrays {
		require.Equal(t, byte(1), a.Complete())
	}
}

// bits is a minimal RBSP bit writer for hand-built SPS units.
type bits struct {
	buf  []byte
	nbit int
}

func (w *bits) u(n int, v uint64) {
	for i := n - 1; i >= 0; i-- {
		if w.nbit%8 == 0 {
			w.buf = append(w.buf, 0)
		}
		if v>>uint(i)&1 == 1 {
			w.buf[len(w.buf)-1] |= 0x80 >> uint(w.nbit%8)
		}
		w.nbit++
	}
}

func (w *bits) ue(v uint64) {
	v++
	n := 0
	for x := v; x > 1; x >>= 1 {
		n++
	}
	w.u(n, 0)
	w.u(n+1, v)
}

func (w *bits) nal(hdr byte) []byte {
	w.u(1, 1)
	for w.nbit%8 != 0 {
		w.u(1, 0)
	}
	out := []byte{hdr}
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

func highSPS(profile uint8, chroma, depthMinus8 uint64) []byte {
	w := &bits{}
	w.u(8, uint64(profile))
	w.u(8, 0)
	w.u(8, 40)
	w.ue(0) // sps id
	w.ue(chroma)
	if chroma == 3 {
		w.u(1, 0)
	}
	w.ue(depthMinus8)
	w.ue(depthMinus8)
	w.u(1, 0) // qpprime
	w.u(1, 0) // scaling matrix
	w.ue(0)   // log2_max_frame_num_minus4
	w.ue(2)   // poc type
	w.ue(1)   // max ref frames
	w.u(1, 0) // gaps
	w.ue(19)  // width mbs - 1
	w.ue(14)  // height map units - 1
	w.u(1, 1) // frame_mbs_only
	w.u(1, 1) // direct_8x8
	w.u(1, 0) // cropping
	w.u(1, 0) // vui
	return w.nal(0x67)
}

func TestAVCRecordHigh10(t *testing.T) {
	sps := highSPS(122, 2, 2)
	pps := []byte{0x68, 0xce, 0x3c, 0x80}
	cfg := &media.TrackConfig{Codec: media.CodecH264, SPS: [][]byte{sps}, PPS: [][]byte{pps}}
	rec, err := AVCRecord(cfg)
	require.NoError(t, err)
	dec, err := avc.DecodeAVCDecConfRec(rec)
	require.NoError(t, err)
	require.Equal(t, byte(122), dec.AVCProfileIndication)
	require.Equal(t, byte(2), dec.ChromaFormat)
	require.Equal(t, byte(2), dec.BitDepthLumaMinus1)
	require.Equal(t, byte(2), dec.BitDepthChromaMinus1)
	tail := rec[len(rec)-4:]
	require.Equal(t, []byte{0xfc | 2, 0xf8 | 2, 0xf8 | 2, 0}, tail)
}

func TestAVCRecordHigh444Profile(t *testing.T) {
	// Profile 244 is not in mp4ff's extension list; the record must still carry it.
	sps := highSPS(244, 3, 0)
	cfg := &media.TrackConfig{Codec: media.CodecH264, SPS: [][]byte{sps}, PPS: [][]byte{{0x68, 0xce, 0x3c, 0x80}}}
	rec, err := AVCRecord(cfg)
	require.NoError(t, err)
	require.Equal(t, []byte{0xfc | 3, 0xf8, 0xf8, 0}, rec[len(rec)-4:])
}

func TestRecordErrors(t *testing.T) {
	_, err := AVCRecord(nil)
	require.Error(t, err)
	_, err = AVCRecord(&media.TrackConfig{Codec: media.CodecH264})
	require.Error(t, err)
	_, err = AVCRecord(&media.TrackConfig{Codec: media.CodecH264, SPS: [][]byte{{0x67, 0x00}}, PPS: [][]byte{{0x68}}})
	require.Error(t, err)
	_, err = AVCRecord(&media.TrackConfig{Codec: media.CodecH265})
	require.Error(t, err)
	_, err = HEVCRecord(nil)
	require.Error(t, err)
	_, err = HEVCRecord(&media.TrackConfig{Codec: media.CodecH265})
	require.Error(t, err)
	_, err = HEVCRecord(&media.TrackConfig{Codec: media.CodecH264})
	require.Error(t, err)
	_, err = HEVCRecord(&media.TrackConfig{Codec: media.CodecH265, SPS: [][]byte{{0x42, 0x01}}})
	require.Error(t, err)
}
