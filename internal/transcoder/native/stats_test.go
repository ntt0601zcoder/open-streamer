package native

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/ntt0601zcoder/open-streamer/internal/transcoder/native/proto"
)

// annexBPSlice is a non-IDR H.264 NAL (type 1): the pipeline must discard it
// until the first keyframe arrives.
var annexBPSlice = []byte{0x00, 0x00, 0x00, 0x01, 0x41, 0x9A, 0x00}

func TestStreamPipeline_StatsCountFramesPerRendition(t *testing.T) {
	t.Parallel()
	p := multiRenditionPipeline(t, 25)
	defer p.Close()
	src := buildSourceEncoder(t, 1920, 1080, 25)
	defer src.Close()

	feed(t, p, src, 1920, 1080, 30)
	_, err := p.Flush()
	require.NoError(t, err)

	st := p.Stats()
	assert.Equal(t, uint64(30), st.FramesDecoded)
	require.Len(t, st.Renditions, 3)
	for i, r := range st.Renditions {
		assert.Equal(t, uint64(30), r.FramesIn, "rendition %d frames in", i)
		assert.Positive(t, r.EncodeNanos, "rendition %d encode time", i)
	}
}

func TestStreamPipeline_StatsCountVideoDroppedBeforeFirstKeyframe(t *testing.T) {
	t.Parallel()
	p := renditionPipeline(t, 25)
	defer p.Close()

	for range 2 {
		_, err := p.ProcessPacket(annexBPSlice, esCodecH264, 0, 0)
		require.NoError(t, err)
	}

	st := p.Stats()
	assert.Equal(t, uint64(2), st.DroppedVideoPreKeyframe)
	assert.Zero(t, st.FramesDecoded)
}

func TestStreamPipeline_StatsCountAudioDroppedBeforeFirstKeyframe(t *testing.T) {
	t.Parallel()
	p := renditionPipeline(t, 25)
	defer p.Close()

	_, err := p.ProcessPacket([]byte{0xFF, 0xF1, 0x4C, 0x80}, esCodecAAC, 10, 10)
	require.NoError(t, err)

	assert.Equal(t, uint64(1), p.Stats().DroppedAudioPreKeyframe)
}

func TestStreamPipeline_StatsReportHeldAudioThenCountItDroppedOnSwitch(t *testing.T) {
	t.Parallel()
	p := renditionPipeline(t, 25)
	defer p.Close()
	p.pendingAudio = []OutputFrame{{PTS: 1}, {PTS: 2}}

	assert.Equal(t, 2, p.Stats().PendingAudioFrames)

	_, err := p.SwitchInput(DecoderConfig{Codec: "h264"})
	require.NoError(t, err)

	st := p.Stats()
	assert.Equal(t, uint64(2), st.DroppedAudioSwitch)
	assert.Zero(t, st.PendingAudioFrames)
}

func configureOneTarget(t *testing.T, stream pb.Transcoder_RunClient) {
	t.Helper()
	require.NoError(t, stream.Send(&pb.Request{Body: &pb.Request_Configure{Configure: &pb.ConfigureRequest{
		StreamCode: "test-stream",
		HwBackend:  pb.HWBackend_HW_BACKEND_CPU,
		Targets: []*pb.Target{{
			Width: 640, Height: 360, BitrateKbps: 512, Framerate: 25, GopSeconds: 1,
			Preset: "ultrafast", BframesOrNeg1: 0, RefsOrNeg1: -1,
		}},
	}}}))
}

func sendSourceFrames(t *testing.T, stream pb.Transcoder_RunClient, n int) {
	t.Helper()
	src := buildSourceEncoder(t, 640, 360, 25)
	defer src.Close()
	for i := range n {
		frame := allocTestNV12Frame(t, 640, 360, 0, int64(i))
		pkts, err := src.Encode(frame)
		frame.Free()
		require.NoError(t, err)
		for _, pkt := range pkts {
			require.NoError(t, stream.Send(&pb.Request{Body: &pb.Request_Packet{Packet: &pb.InputPacket{Data: pkt.Data}}}))
		}
	}
}

// TestServer_SendsFinalHealthOnStop: without a report at shutdown the frames
// processed since the last periodic report would never reach the metrics.
func TestServer_SendsFinalHealthOnStop(t *testing.T) {
	t.Parallel()
	client, cleanup := dialBufconn(t)
	defer cleanup()
	stream, err := client.Run(t.Context())
	require.NoError(t, err)
	configureOneTarget(t, stream)
	sendSourceFrames(t, stream, 25)
	require.NoError(t, stream.Send(&pb.Request{Body: &pb.Request_Stop{Stop: &pb.Stop{Reason: "test"}}}))
	require.NoError(t, stream.CloseSend())

	var last *pb.Health
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if h := ev.GetHealth(); h != nil {
			last = h
		}
	}

	require.NotNil(t, last, "no Health event received")
	assert.Equal(t, uint64(25), last.GetFramesDecoded())
	require.Len(t, last.GetProfiles(), 1)
	assert.Equal(t, int32(0), last.GetProfiles()[0].GetIndex())
	assert.Equal(t, uint64(25), last.GetProfiles()[0].GetFramesIn())
	assert.Positive(t, last.GetProfiles()[0].GetEncodeNanos())
}

// TestServer_SendsHealthPeriodically: the stream stays open here, so a server
// that only reports at shutdown never delivers a Health event.
func TestServer_SendsHealthPeriodically(t *testing.T) {
	t.Parallel()
	client, cleanup := dialServer(t, &Server{healthInterval: time.Nanosecond})
	defer cleanup()
	stream, err := client.Run(t.Context())
	require.NoError(t, err)
	configureOneTarget(t, stream)
	sendSourceFrames(t, stream, 5)

	got := make(chan struct{})
	go func() {
		for {
			ev, err := stream.Recv()
			if err != nil {
				return
			}
			if ev.GetHealth() != nil {
				close(got)
				return
			}
		}
	}()

	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("no Health event while the stream was running")
	}
}
