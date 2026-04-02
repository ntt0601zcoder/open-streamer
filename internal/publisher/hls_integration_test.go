//go:build integration

package publisher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ntthuan060102github/open-streamer/internal/domain"
)

// mediamtxFixtureYAML starts an FFmpeg loop publisher into path "test1" (HLS, RTSP, RTMP, SRT readers).
const mediamtxFixtureYAML = `
logLevel: warn
logDestinations: [stdout]

rtmpAddress: :1935
rtspAddress: :8554
hlsAddress: :8888
srtAddress: :8890

paths:
  test1:
    source: publisher
    runOnInit:
      - ffmpeg -re -stream_loop -1 -fflags +genpts -i /source.mp4 -c:v copy -c:a aac -ar 48000 -ac 2 -f flv rtmp://127.0.0.1:1935/test1
    runOnInitRestart: true
`

const mediamtxImage = "bluenviron/mediamtx:1.15.4"

const sampleMP4Name = "source.mp4"

// TestHLSProductionContainerDataSamples builds the production Docker image, runs it with MediaMTX fixtures,
// merges data/streams.*.json samples into one store, and checks HLS over the real HTTP API (/{code}/index.m3u8).
//
// Requires: Docker, ffmpeg/ffprobe, and data/source.mp4. Add gopls buildFlags: ["-tags=integration"] to edit this file in the IDE.
// Run: go test -tags=integration -timeout 30m ./internal/publisher/... -run TestHLSProductionContainerDataSamples$
func TestHLSProductionContainerDataSamples(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped with -short")
	}
	requireFFmpegTools(t)

	root := findModuleRoot(t)
	srcMP4 := filepath.Join(root, "data", sampleMP4Name)
	if _, err := os.Stat(srcMP4); err != nil {
		t.Skip("data/source.mp4 not found — add a sample MP4 under data/")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	dataDir, codes := prepareJSONStoreDir(t, root, srcMP4)
	ymlPath := filepath.Join(t.TempDir(), "mediamtx.yml")
	require.NoError(t, os.WriteFile(ymlPath, []byte(strings.TrimSpace(mediamtxFixtureYAML)+"\n"), 0o644))

	nw, mtxC, appC := startProductionStack(t, ctx, root, dataDir, ymlPath)
	// Cleanup order (t.Cleanup LIFO): terminate MediaMTX, terminate app, remove network.
	t.Cleanup(func() { _ = nw.Remove(context.WithoutCancel(ctx)) })
	testcontainers.CleanupContainer(t, appC)
	testcontainers.CleanupContainer(t, mtxC)

	baseURL := openStreamerBaseURL(t, ctx, appC)
	httpClient := &http.Client{Timeout: 45 * time.Second}
	for _, code := range codes {
		code := code
		t.Run(string(code), func(t *testing.T) {
			assertStreamHLSPublished(t, httpClient, baseURL, code)
		})
	}
}

func prepareJSONStoreDir(t *testing.T, repoRoot, srcMP4 string) (dataDir string, codes []domain.StreamCode) {
	t.Helper()
	dataDir = t.TempDir()
	require.NoError(t, copyFile(filepath.Join(dataDir, sampleMP4Name), srcMP4))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "recordings.json"), []byte("{}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "hooks.json"), []byte("{}"), 0o644))

	merged, codes := mergedStreamsFromDataSamples(t, repoRoot, "mediamtx")
	require.NotEmpty(t, merged, "no data/streams.*.json samples found")
	payload, err := json.MarshalIndent(merged, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "streams.json"), payload, 0o644))
	return dataDir, codes
}

func startProductionStack(t *testing.T, ctx context.Context, repoRoot, dataDir, ymlPath string) (
	nw *testcontainers.DockerNetwork,
	mtxC testcontainers.Container,
	appC testcontainers.Container,
) {
	t.Helper()
	var err error
	nw, err = network.New(ctx)
	require.NoError(t, err)

	mtxC, err = testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        mediamtxImage,
			ExposedPorts: []string{"1935/tcp", "8554/tcp", "8888/tcp", "8890/tcp", "8890/udp"},
			Networks:     []string{nw.Name},
			NetworkAliases: map[string][]string{
				nw.Name: {"mediamtx"},
			},
			Files: []testcontainers.ContainerFile{
				{HostFilePath: ymlPath, ContainerFilePath: "/mediamtx.yml", FileMode: 0o644},
				{HostFilePath: filepath.Join(dataDir, sampleMP4Name), ContainerFilePath: "/source.mp4", FileMode: 0o644},
			},
			WaitingFor: wait.ForHTTP("/test1/index.m3u8").
				WithPort("8888/tcp").
				WithStartupTimeout(6 * time.Minute).
				WithPollInterval(2 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err)

	dataBind := filepath.ToSlash(dataDir) + ":/data:rw"
	appC, err = testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			FromDockerfile: testcontainers.FromDockerfile{
				Context:       repoRoot,
				Dockerfile:    filepath.Join("build", "Dockerfile"),
				PrintBuildLog: testing.Verbose(),
			},
			ExposedPorts: []string{"8080/tcp"},
			Networks:     []string{nw.Name},
			Env: map[string]string{
				"OPEN_STREAMER_SERVER_HTTP_ADDR":                 ":8080",
				"OPEN_STREAMER_STORAGE_JSON_DIR":                 "/data",
				"OPEN_STREAMER_PUBLISHER_HLS_DIR":                "/hls",
				"OPEN_STREAMER_PUBLISHER_DASH_DIR":               "/dash",
				"OPEN_STREAMER_MANAGER_INPUT_PACKET_TIMEOUT_SEC": "120",
				"OPEN_STREAMER_BUFFER_CAPACITY":                  "24000",
				"OPEN_STREAMER_LOG_LEVEL":                        "warn",
			},
			HostConfigModifier: func(hc *container.HostConfig) {
				if hc.Binds == nil {
					hc.Binds = []string{dataBind}
				} else {
					hc.Binds = append(hc.Binds, dataBind)
				}
			},
			WaitingFor: wait.ForHTTP("/healthz").
				WithPort("8080/tcp").
				WithStartupTimeout(8 * time.Minute).
				WithPollInterval(1 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err)
	return nw, mtxC, appC
}

func openStreamerBaseURL(t *testing.T, ctx context.Context, appC testcontainers.Container) string {
	t.Helper()
	host, err := appC.Host(ctx)
	require.NoError(t, err)
	mapped, err := appC.MappedPort(ctx, "8080")
	require.NoError(t, err)
	return fmt.Sprintf("http://%s:%s", host, mapped.Port())
}

func assertStreamHLSPublished(t *testing.T, httpClient *http.Client, baseURL string, code domain.StreamCode) {
	t.Helper()
	u := baseURL + "/" + string(code) + "/index.m3u8"
	var body []byte
	require.Eventually(t, func() bool {
		resp, err := httpClient.Get(u)
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				_ = resp.Body.Close()
			}
			return false
		}
		b, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if rerr != nil {
			return false
		}
		ct := resp.Header.Get("Content-Type")
		if !strings.Contains(ct, "mpegurl") && !strings.Contains(ct, "m3u8") {
			return false
		}
		if !bytes.Contains(b, []byte("#EXTINF")) || !bytes.Contains(b, []byte(".ts")) {
			return false
		}
		body = b
		return true
	}, 4*time.Minute, 2*time.Second, "HLS manifest for %s", code)

	seg := firstTSegmentURIInM3U8(body)
	require.NotEmpty(t, seg, "segment URI in playlist")
	segURL := baseURL + "/" + string(code) + "/" + seg
	var segData []byte
	require.Eventually(t, func() bool {
		resp, err := httpClient.Get(segURL)
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				_ = resp.Body.Close()
			}
			return false
		}
		b, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if rerr != nil || len(b) < 188 {
			return false
		}
		segData = b
		return true
	}, 2*time.Minute, 1*time.Second, "HLS segment GET for %s", code)

	tmp := filepath.Join(t.TempDir(), "seg.ts")
	require.NoError(t, os.WriteFile(tmp, segData, 0o644))
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type",
		"-of", "csv=p=0", tmp).CombinedOutput()
	require.NoError(t, err, "ffprobe: %s", out)
	require.Contains(t, string(out), "video")
}

func findModuleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	d := wd
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			t.Fatalf("go.mod not found from %s", wd)
		}
		d = parent
	}
}

func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// mergedStreamsFromDataSamples reads data/streams.<kind>.json, rewrites the primary input URL for the
// docker network, disables transcoding for speed, and enables HLS-only publishing. Returns the store map
// and sorted stream codes.
func mergedStreamsFromDataSamples(t *testing.T, repoRoot, mediamtxHost string) (map[string]*domain.Stream, []domain.StreamCode) {
	t.Helper()
	pattern := filepath.Join(repoRoot, "data", "streams.*.json")
	matches, err := filepath.Glob(pattern)
	require.NoError(t, err)
	sort.Strings(matches)

	out := make(map[string]*domain.Stream)
	var codes []domain.StreamCode
	for _, path := range matches {
		base := filepath.Base(path)
		kind := strings.TrimSuffix(strings.TrimPrefix(base, "streams."), ".json")
		if kind == "" {
			continue
		}
		raw, err := os.ReadFile(path)
		require.NoError(t, err, path)
		var envelope map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(raw, &envelope), path)
		if len(envelope) == 0 {
			continue
		}
		keys := make([]string, 0, len(envelope))
		for k := range envelope {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		firstVal := envelope[keys[0]]
		var st domain.Stream
		require.NoError(t, json.Unmarshal(firstVal, &st), path)
		if len(st.Inputs) == 0 {
			t.Logf("skip %s: no inputs", path)
			continue
		}
		u := fixtureIngestURL(kind, mediamtxHost)
		if u == "" {
			t.Logf("skip %s: unknown sample kind %q", path, kind)
			continue
		}
		code := domain.StreamCode(kind + "_sample")
		require.NoError(t, domain.ValidateStreamCode(string(code)))

		net := st.Inputs[0].Net
		st.Code = code
		st.Inputs = []domain.Input{{URL: u, Priority: 0, Net: net}}
		st.Transcoder = nil
		st.Protocols = domain.OutputProtocols{HLS: true}
		out[string(code)] = &st
		codes = append(codes, code)
	}
	sort.Slice(codes, func(i, j int) bool { return string(codes[i]) < string(codes[j]) })
	return out, codes
}

func fixtureIngestURL(kind, mediamtxHost string) string {
	switch kind {
	case "file":
		return "file:///data/source.mp4?loop=true"
	case "hls":
		return fmt.Sprintf("http://%s:8888/test1/index.m3u8", mediamtxHost)
	case "rtmp":
		return fmt.Sprintf("rtmp://%s:1935/test1", mediamtxHost)
	case "rtsp":
		return fmt.Sprintf("rtsp://%s:8554/test1", mediamtxHost)
	case "srt":
		return fmt.Sprintf("srt://%s:8890?streamid=read:test1&latency=300", mediamtxHost)
	default:
		return ""
	}
}
