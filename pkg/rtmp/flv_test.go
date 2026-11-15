package rtmp

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/Eyevinn/mp4ff/hevc"
)

func readFixture(t *testing.T, name string) []FLVTag {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/fixtures/" + name)
	if err != nil {
		t.Fatal(err)
	}
	tags, err := ReadFLV(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return tags
}

func TestFLVVideoRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		tag      VideoTag
		enhanced bool
	}{
		{"avc header", VideoTag{Codec: VideoAVC, Kind: VideoSequenceStart, Key: true, Data: []byte{1, 2, 3}}, false},
		{"avc nalu -2", VideoTag{Codec: VideoAVC, Kind: VideoCodedFrames, CompTime: -2, Data: []byte{0, 0, 0, 1, 9}}, false},
		{"avc nalu +80", VideoTag{Codec: VideoAVC, Kind: VideoCodedFrames, Key: true, CompTime: 80, Data: []byte{7}}, false},
		{"avc end", VideoTag{Codec: VideoAVC, Kind: VideoSequenceEnd}, false},
		{"legacy hevc", VideoTag{Codec: VideoHEVC, Kind: VideoCodedFrames, CompTime: 40, Data: []byte{5, 6}}, false},
		{"ex hevc header", VideoTag{Codec: VideoHEVC, Kind: VideoSequenceStart, Key: true, Data: []byte{1, 9}}, true},
		{"ex hevc coded", VideoTag{Codec: VideoHEVC, Kind: VideoCodedFrames, CompTime: -40, Data: []byte{4}}, true},
		{"ex hevc codedX", VideoTag{Codec: VideoHEVC, Kind: VideoCodedFrames, Key: true, Data: []byte{4, 4}}, true},
		{"ex avc coded", VideoTag{Codec: VideoAVC, Kind: VideoCodedFrames, CompTime: 80, Data: []byte{4}}, true},
		{"ex end", VideoTag{Codec: VideoHEVC, Kind: VideoSequenceEnd}, true},
		{"ex metadata", VideoTag{Codec: VideoHEVC, Kind: VideoMetadata, Data: []byte{1}}, true},
		{"command", VideoTag{Codec: VideoAVC, Kind: VideoCommand, Data: []byte{0}}, false},
		{"ex command", VideoTag{Kind: VideoCommand, Data: []byte{1}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := AppendVideoTag(nil, c.tag, c.enhanced)
			got, err := ParseVideoTag(b)
			if err != nil {
				t.Fatal(err)
			}
			if c.tag.Kind == VideoCommand {
				c.tag.Codec = got.Codec // a command frame carries no meaningful codec in the enhanced form
			}
			if got.Codec != c.tag.Codec || got.Kind != c.tag.Kind || got.Key != c.tag.Key ||
				got.CompTime != c.tag.CompTime || !bytes.Equal(got.Data, c.tag.Data) {
				t.Fatalf("got %+v want %+v", got, c.tag)
			}
		})
	}
	// Legacy bytes are as the FLV specification lays them out.
	b := AppendVideoTag(nil, VideoTag{Codec: VideoAVC, Kind: VideoCodedFrames, CompTime: -2, Data: []byte{9}}, false)
	if want := []byte{0x27, 1, 0xFF, 0xFF, 0xFE, 9}; !bytes.Equal(b, want) {
		t.Fatalf("% x", b)
	}
}

func TestFLVAudioRoundTrip(t *testing.T) {
	for _, hdr := range []bool{true, false} {
		in := AudioTag{SequenceHeader: hdr, Data: []byte{0x12, 0x10}}
		b := AppendAudioTag(nil, in)
		if b[0]>>4 != 10 {
			t.Fatalf("% x", b)
		}
		got, err := ParseAudioTag(b)
		if err != nil || got.SequenceHeader != hdr || !bytes.Equal(got.Data, in.Data) {
			t.Fatalf("%+v %v", got, err)
		}
	}
	if _, err := ParseAudioTag([]byte{0x2F, 1, 0}); !errors.Is(err, ErrUnsupportedCodec) {
		t.Fatalf("mp3: %v", err)
	}
}

func TestFLVDataIsOwned(t *testing.T) {
	b := AppendVideoTag(nil, VideoTag{Codec: VideoAVC, Kind: VideoCodedFrames, Data: []byte{1, 2}}, false)
	v, _ := ParseVideoTag(b)
	b[len(b)-1] = 99
	if v.Data[1] != 2 {
		t.Fatal("Data aliases the input")
	}
}

func TestFLVModExSkipped(t *testing.T) {
	// ModEx packet (7), 3 bytes of ModEx data, then type nibble 0, next packet type CodedFramesX (3).
	b := []byte{0x80 | 2<<4 | exModEx, 2, 0xAA, 0xBB, 0xCC, 0x03, 'h', 'v', 'c', '1', 0xDE, 0xAD}
	got, err := ParseVideoTag(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Codec != VideoHEVC || got.Kind != VideoCodedFrames || got.CompTime != 0 || !bytes.Equal(got.Data, []byte{0xDE, 0xAD}) {
		t.Fatalf("%+v", got)
	}
}

func TestFLVModExExtendedSize(t *testing.T) {
	// Size byte 0xFF selects a UI16 size: 0x0001+1 = 2 bytes of ModEx data.
	b := []byte{0x80 | 1<<4 | exModEx, 0xFF, 0x00, 0x01, 0xAA, 0xBB, 0x01, 'a', 'v', 'c', '1', 0, 0, 5, 0x77}
	got, err := ParseVideoTag(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Codec != VideoAVC || got.Kind != VideoCodedFrames || got.CompTime != 5 || !got.Key || !bytes.Equal(got.Data, []byte{0x77}) {
		t.Fatalf("%+v", got)
	}
}

func TestFLVHostile(t *testing.T) {
	cases := map[string][]byte{
		"empty":          {},
		"legacy short":   {0x17, 1, 0},
		"avc unknown":    {0x17, 9, 0, 0, 0},
		"ex no fourcc":   {0x90, 'h', 'v'},
		"ex unknown pkt": {0x80 | 1<<4 | 5, 'h', 'v', 'c', '1'},
		"ex coded short": {0x80 | 1<<4 | exCoded, 'h', 'v', 'c', '1', 0},
		"modex short":    {0x80 | exModEx, 5, 1},
		"multitrack":     {0x80 | 1<<4 | exMultitrack, 'h', 'v', 'c', '1'},
		"modex overrun":  {0x80 | exModEx, 0xFF, 0x00, 0x05, 1, 2},
		"mpeg2ts start":  {0x80 | 1<<4 | 5, 'h', 'v', 'c', '1'},
		"unknown fourcc": {0x80 | 1<<4 | exCoded, 'a', 'v', '0', '1', 0, 0, 0},
		"codec id 2":     {0x12, 0, 0, 0, 0, 0},
	}
	for name, b := range cases {
		if _, err := ParseVideoTag(b); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	for _, n := range []string{"multitrack", "mpeg2ts start", "ex unknown pkt"} {
		if _, err := ParseVideoTag(cases[n]); !errors.Is(err, ErrUnsupportedCodec) {
			t.Errorf("%s: %v", n, err)
		}
	}
	if _, err := ParseAudioTag([]byte{0xAF}); err == nil {
		t.Error("truncated audio")
	}
	if _, err := ParseAudioTag([]byte{0xAF, 7}); err == nil {
		t.Error("unknown AAC packet")
	}
	// A full 11-byte tag header declaring 0xFFFFFF bytes of data.
	big := []byte("FLV\x01\x05\x00\x00\x00\x09\x00\x00\x00\x00\x09\xFF\xFF\xFF\x00\x00\x00\x00\x00\x00\x00")
	if _, err := ReadFLV(bytes.NewReader(big)); !errors.Is(err, errFLVTagTooLarge) {
		t.Errorf("oversized tag: %v", err)
	}
	if _, err := ReadFLV(bytes.NewReader([]byte("XYZ\x01\x05\x00\x00\x00\x09"))); err == nil {
		t.Error("bad signature")
	}
}

func TestFLVFixtureHEVC(t *testing.T) {
	var video, codedX, meta, audio, seq, skipped int
	var firstHdr []byte
	for _, tag := range readFixture(t, "h265_aac.flv") {
		switch tag.Type {
		case FLVVideo:
			v, err := ParseVideoTag(tag.Data)
			if err != nil {
				t.Fatal(err)
			}
			if v.Kind != VideoCommand && v.Codec != VideoHEVC {
				t.Fatalf("codec %d kind %d", v.Codec, v.Kind)
			}
			switch v.Kind {
			case VideoSequenceStart:
				seq++
				var rec hevc.DecConfRec
				rec, err = hevc.DecodeHEVCDecConfRec(v.Data)
				if err != nil {
					t.Fatal(err)
				}
				if len(rec.GetNalusForType(hevc.NALU_SPS)) == 0 {
					t.Fatal("no SPS in record")
				}
			case VideoCodedFrames:
				video++
				if tag.Data[0]&0x0F == exCodedX {
					codedX++
				}
			case VideoMetadata:
				meta++
			case VideoSequenceEnd, VideoCommand:
			}
		case FLVAudio:
			a, err := ParseAudioTag(tag.Data)
			if tag.Data[0]>>4 == 9 {
				// ffmpeg 8 adds an enhanced-audio multichannel config tag for 5.1.
				if !errors.Is(err, ErrUnsupportedCodec) {
					t.Fatalf("enhanced audio tag: %v", err)
				}
				skipped++
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if !a.SequenceHeader {
				audio++
			} else if len(a.Data) > 0 && firstHdr == nil {
				firstHdr = a.Data
			}
		}
	}
	if seq != 1 || video != 150 || audio != 260 || skipped != 2 || meta != 1 || codedX != 41 {
		t.Fatalf("seq %d video %d audio %d skipped %d meta %d codedX %d", seq, video, audio, skipped, meta, codedX)
	}
	// AOT 2 (AAC-LC), 44.1 kHz, 6 channels.
	if !bytes.Equal(firstHdr, []byte{0x12, 0x30}) {
		t.Fatalf("first non-empty ASC % x", firstHdr)
	}
}

func TestFLVFixtureAVC(t *testing.T) {
	var video, audio, hdr int
	for _, tag := range readFixture(t, "h264_aac.flv") {
		switch tag.Type {
		case FLVVideo:
			v, err := ParseVideoTag(tag.Data)
			if err != nil {
				t.Fatal(err)
			}
			if v.Codec != VideoAVC {
				t.Fatalf("codec %d", v.Codec)
			}
			if v.Kind == VideoCodedFrames {
				video++
			}
		case FLVAudio:
			a, err := ParseAudioTag(tag.Data)
			if err != nil {
				t.Fatal(err)
			}
			if a.SequenceHeader {
				hdr++
			} else {
				audio++
			}
		}
	}
	if video != 150 || hdr != 2 || audio != 283 {
		t.Fatalf("video %d hdr %d audio %d", video, hdr, audio)
	}
}
