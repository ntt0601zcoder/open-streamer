package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// GET /config/defaults must echo every constant from internal/domain/defaults.go
// — frontend uses it as the source of truth for form placeholders.
func TestGetConfigDefaultsShape(t *testing.T) {
	t.Parallel()
	h := &ConfigHandler{}

	w := httptest.NewRecorder()
	h.GetConfigDefaults(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/config/defaults", nil))
	require.Equal(t, http.StatusOK, w.Code)

	var got configDefaultsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))

	assert.Equal(t, domain.DefaultBufferCapacity, got.Buffer.Capacity)
	assert.Equal(t, domain.DefaultInputPacketTimeoutSec, got.Manager.InputPacketTimeoutSec)

	assert.Equal(t, domain.DefaultLiveSegmentSec, got.Publisher.HLS.LiveSegmentSec)
	assert.Equal(t, domain.DefaultLiveWindow, got.Publisher.HLS.LiveWindow)
	assert.Equal(t, domain.DefaultLiveHistory, got.Publisher.HLS.LiveHistory)
	assert.Equal(t, domain.DefaultLiveSegmentSec, got.Publisher.DASH.LiveSegmentSec)

	assert.Equal(t, domain.DefaultHookMaxRetries, got.Hook.MaxRetries)
	assert.Equal(t, domain.DefaultHookTimeoutSec, got.Hook.TimeoutSec)

	assert.Equal(t, domain.DefaultPushTimeoutSec, got.Push.TimeoutSec)
	assert.Equal(t, domain.DefaultPushRetryTimeoutSec, got.Push.RetryTimeoutSec)

	assert.Equal(t, domain.DefaultDVRSegmentDuration, got.DVR.SegmentDuration)

	assert.Equal(t, domain.DefaultVideoBitrateK, got.Transcoder.Video.BitrateK)
	assert.Equal(t, domain.ResizeModePad, got.Transcoder.Video.ResizeMode)
	assert.Equal(t, domain.AudioCodecAAC, got.Transcoder.Audio.Codec)
	assert.Equal(t, domain.DefaultAudioBitrateK, got.Transcoder.Audio.BitrateK)
	assert.Equal(t, domain.HWAccelNone, got.Transcoder.Global.HW)

	assert.Equal(t, 1935, got.Listeners.RTMP.Port)
	assert.Equal(t, 554, got.Listeners.RTSP.Port)
	assert.Equal(t, "tcp", got.Listeners.RTSP.Transport)
	assert.Equal(t, 9999, got.Listeners.SRT.Port)

	assert.Equal(t, domain.DefaultHLSPlaylistTimeoutSec, got.Ingestor.HLSPlaylistTimeoutSec)
	assert.Equal(t, domain.DefaultHLSSegmentTimeoutSec, got.Ingestor.HLSSegmentTimeoutSec)
}

// StoragePathTemplate must use the {streamCode} placeholder so frontend
// can substitute it client-side per-stream.
func TestGetConfigDefaults_DVRStoragePathTemplate(t *testing.T) {
	t.Parallel()
	h := &ConfigHandler{}

	w := httptest.NewRecorder()
	h.GetConfigDefaults(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/config/defaults", nil))
	require.Equal(t, http.StatusOK, w.Code)

	var got configDefaultsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))

	assert.True(t, strings.HasPrefix(got.DVR.StoragePathTemplate, domain.DefaultDVRRoot+"/"),
		"template must start with the DVR root: %q", got.DVR.StoragePathTemplate)
	assert.Contains(t, got.DVR.StoragePathTemplate, "{streamCode}",
		"template must include {streamCode} placeholder for client substitution")
}

// Endpoint must be deterministic — same input, same bytes (no map
// iteration randomness leaking through). Frontend caching depends on it.
func TestGetConfigDefaults_Deterministic(t *testing.T) {
	t.Parallel()
	h := &ConfigHandler{}

	body := func() []byte {
		w := httptest.NewRecorder()
		h.GetConfigDefaults(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/config/defaults", nil))
		return w.Body.Bytes()
	}

	first := body()
	for i := 0; i < 10; i++ {
		assert.Equal(t, first, body(),
			"response must be byte-identical across calls (iteration %d)", i)
	}
}
