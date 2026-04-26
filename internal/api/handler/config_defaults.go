package handler

import (
	"net/http"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// configDefaultsResponse mirrors the user-configurable surface of
// GlobalConfig + Stream-level config and reports the implicit value the
// server applies when the field is left zero / empty.
//
// UI fetches this once on app init and uses each value as form field
// placeholder text — so users see "1024" instead of literal "default".
//
// Shape choice: NESTED to match GlobalConfig / Stream JSON paths exactly,
// so frontend can do `defaults.transcoder.audio.bitrate_k` without
// remapping. Field names are snake_case to match the existing config
// JSON tags.
type configDefaultsResponse struct {
	Buffer struct {
		Capacity int `json:"capacity"`
	} `json:"buffer"`

	Manager struct {
		InputPacketTimeoutSec int `json:"input_packet_timeout_sec"`
	} `json:"manager"`

	Publisher struct {
		HLS  liveSegmentDefaults `json:"hls"`
		DASH liveSegmentDefaults `json:"dash"`
	} `json:"publisher"`

	Hook struct {
		MaxRetries int `json:"max_retries"`
		TimeoutSec int `json:"timeout_sec"`
	} `json:"hook"`

	Push struct {
		TimeoutSec      int `json:"timeout_sec"`
		RetryTimeoutSec int `json:"retry_timeout_sec"`
	} `json:"push"`

	DVR struct {
		// SegmentDuration is in seconds.
		SegmentDuration int `json:"segment_duration"`
		// StoragePathTemplate uses {streamCode} as the placeholder for
		// the stream code — frontend must substitute it client-side.
		StoragePathTemplate string `json:"storage_path_template"`
	} `json:"dvr"`

	Transcoder struct {
		Video struct {
			BitrateK   int               `json:"bitrate_k"`
			ResizeMode domain.ResizeMode `json:"resize_mode"`
		} `json:"video"`
		Audio struct {
			Codec    domain.AudioCodec `json:"codec"`
			BitrateK int               `json:"bitrate_k"`
		} `json:"audio"`
		Global struct {
			HW domain.HWAccel `json:"hw"`
		} `json:"global"`
	} `json:"transcoder"`

	Listeners struct {
		RTMP listenerDefaults     `json:"rtmp"`
		RTSP rtspListenerDefaults `json:"rtsp"`
		SRT  listenerDefaults     `json:"srt"`
	} `json:"listeners"`

	Ingestor struct {
		HLSPlaylistTimeoutSec int `json:"hls_playlist_timeout_sec"`
		HLSSegmentTimeoutSec  int `json:"hls_segment_timeout_sec"`
	} `json:"ingestor"`
}

type liveSegmentDefaults struct {
	LiveSegmentSec int `json:"live_segment_sec"`
	LiveWindow     int `json:"live_window"`
	LiveHistory    int `json:"live_history"`
}

type listenerDefaults struct {
	Port int `json:"port"`
}

type rtspListenerDefaults struct {
	Port      int    `json:"port"`
	Transport string `json:"transport"`
}

// buildConfigDefaults constructs the response from the constants in
// internal/domain/defaults.go. Pure function — no state, no IO — so the
// handler is trivially testable and the response is deterministic.
//
// Listener ports + RTSP transport + HW + audio codec + resize mode are
// reported here even though no service consumes constants for them
// directly (they are well-known protocol defaults). Operators / UI need
// these too; defining them inline rather than in defaults.go keeps the
// domain package free of "API-only" constants.
func buildConfigDefaults() configDefaultsResponse {
	const (
		// Well-known protocol ports — IANA assignments / convention.
		rtmpPort      = 1935
		rtspPort      = 554
		srtPort       = 9999
		rtspTransport = "tcp"
	)

	var resp configDefaultsResponse

	resp.Buffer.Capacity = domain.DefaultBufferCapacity
	resp.Manager.InputPacketTimeoutSec = domain.DefaultInputPacketTimeoutSec

	resp.Publisher.HLS = liveSegmentDefaults{
		LiveSegmentSec: domain.DefaultLiveSegmentSec,
		LiveWindow:     domain.DefaultLiveWindow,
		LiveHistory:    domain.DefaultLiveHistory,
	}
	resp.Publisher.DASH = liveSegmentDefaults{
		LiveSegmentSec: domain.DefaultLiveSegmentSec,
		LiveWindow:     domain.DefaultLiveWindow,
		LiveHistory:    domain.DefaultLiveHistory,
	}

	resp.Hook.MaxRetries = domain.DefaultHookMaxRetries
	resp.Hook.TimeoutSec = domain.DefaultHookTimeoutSec

	resp.Push.TimeoutSec = domain.DefaultPushTimeoutSec
	resp.Push.RetryTimeoutSec = domain.DefaultPushRetryTimeoutSec

	resp.DVR.SegmentDuration = domain.DefaultDVRSegmentDuration
	resp.DVR.StoragePathTemplate = domain.DefaultDVRRoot + "/{streamCode}"

	resp.Transcoder.Video.BitrateK = domain.DefaultVideoBitrateK
	resp.Transcoder.Video.ResizeMode = domain.ResizeModePad
	resp.Transcoder.Audio.Codec = domain.AudioCodecAAC
	resp.Transcoder.Audio.BitrateK = domain.DefaultAudioBitrateK
	resp.Transcoder.Global.HW = domain.HWAccelNone

	resp.Listeners.RTMP = listenerDefaults{Port: rtmpPort}
	resp.Listeners.RTSP = rtspListenerDefaults{Port: rtspPort, Transport: rtspTransport}
	resp.Listeners.SRT = listenerDefaults{Port: srtPort}

	resp.Ingestor.HLSPlaylistTimeoutSec = domain.DefaultHLSPlaylistTimeoutSec
	resp.Ingestor.HLSSegmentTimeoutSec = domain.DefaultHLSSegmentTimeoutSec

	return resp
}

// GetConfigDefaults returns the implicit values the server applies when
// the corresponding configuration field is left zero / empty by the user.
//
// Frontend usage: fetch once on app init, cache, use as form field
// placeholder text so users see the actual default value (e.g. "1024",
// "aac", "h264") instead of a generic "default" word.
//
// Note: a few fields with context-aware resolution (video codec name
// depending on HW backend) are NOT included here — they need the caller's
// current HW selection to compute. Frontend handles those client-side.
//
// @Summary     Get system configuration defaults.
// @Description Static values the server fills in for unset configuration fields. Use as form placeholders so users see real defaults instead of "default" text.
// @Tags        system
// @Produce     json
// @Success     200 {object} handler.configDefaultsResponse
// @Router      /config/defaults [get].
func (h *ConfigHandler) GetConfigDefaults(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, buildConfigDefaults())
}
