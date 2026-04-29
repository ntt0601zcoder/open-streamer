package domain

import "testing"

func TestOutputTracks_AudioFollowedByLadder(t *testing.T) {
	tc := &TranscoderConfig{
		Audio: AudioTranscodeConfig{
			Codec:   AudioCodecAAC,
			Bitrate: 156,
		},
		Video: VideoTranscodeConfig{
			Profiles: []VideoProfile{
				{Codec: VideoCodecH264, Width: 1280, Height: 720, Bitrate: 1639},
				{Codec: VideoCodecH264, Width: 854, Height: 480, Bitrate: 782},
			},
		},
	}
	got := OutputTracks(tc)
	if len(got) != 3 {
		t.Fatalf("expected 3 tracks (1 audio + 2 video), got %d (%+v)", len(got), got)
	}
	if got[0].Kind != MediaTrackAudio || got[0].Codec != "aac" || got[0].BitrateKbps != 156 {
		t.Fatalf("audio mismatch: %+v", got[0])
	}
	if got[1].Codec != "h264" || got[1].Width != 1280 || got[1].Height != 720 || got[1].BitrateKbps != 1639 {
		t.Fatalf("v1 mismatch: %+v", got[1])
	}
	if got[2].Codec != "h264" || got[2].Width != 854 || got[2].Height != 480 || got[2].BitrateKbps != 782 {
		t.Fatalf("v2 mismatch: %+v", got[2])
	}
}

func TestOutputTracks_CopyLabelled(t *testing.T) {
	tc := &TranscoderConfig{
		Audio: AudioTranscodeConfig{Copy: true, Bitrate: 130},
		Video: VideoTranscodeConfig{
			Profiles: []VideoProfile{
				{Codec: VideoCodecCopy, Bitrate: 0},
			},
		},
	}
	got := OutputTracks(tc)
	if len(got) != 2 {
		t.Fatalf("expected 2 tracks, got %d", len(got))
	}
	if got[0].Codec != "copy" || got[1].Codec != "copy" {
		t.Fatalf("expected both rows labelled copy: %+v", got)
	}
}

func TestOutputTracks_NilConfig(t *testing.T) {
	if got := OutputTracks(nil); got != nil {
		t.Fatalf("expected nil for nil config, got %+v", got)
	}
}

func TestAggregateBitrateKbps(t *testing.T) {
	tracks := []MediaTrackInfo{
		{BitrateKbps: 156},
		{BitrateKbps: 1639},
		{BitrateKbps: 782},
	}
	if got := AggregateBitrateKbps(tracks); got != 2577 {
		t.Fatalf("expected 2577, got %d", got)
	}
}
