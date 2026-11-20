package ingest

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"maps"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
)

// fixtureDatagrams splits the fixture into 7-packet TS datagrams.
func fixtureDatagrams(t *testing.T) [][]byte {
	t.Helper()
	data, err := os.ReadFile(fixturePath("h264_aac.ts"))
	require.NoError(t, err)
	var out [][]byte
	for off := 0; off < len(data); off += 7 * 188 {
		out = append(out, data[off:min(off+7*188, len(data))])
	}
	return out
}

type rtpStyle struct {
	pt   byte
	csrc int
	ext  bool
}

// rtpWrap prefixes payload with an RTP header.
func rtpWrap(seq uint16, st rtpStyle, payload []byte) []byte {
	h := make([]byte, 12)
	h[0] = 0x80 | byte(st.csrc)
	if st.ext {
		h[0] |= 0x10
	}
	h[1] = st.pt
	binary.BigEndian.PutUint16(h[2:], seq)
	binary.BigEndian.PutUint32(h[8:], 0xdeadbeef)
	h = append(h, make([]byte, 4*st.csrc)...)
	if st.ext {
		h = append(h, 0xbe, 0xde, 0, 2, 1, 2, 3, 4, 5, 6, 7, 8)
	}
	return append(h, payload...)
}

// sendAll writes the datagrams to addr, yielding now and then so the
// receiver keeps up.
func sendAll(t *testing.T, addr string, dgs [][]byte) {
	t.Helper()
	c, err := (&net.Dialer{}).DialContext(t.Context(), "udp", addr)
	if err != nil {
		t.Error(err)
		return
	}
	defer c.Close()
	for i, d := range dgs {
		if _, err := c.Write(d); err != nil {
			return // the receiver closed
		}
		if i%8 == 7 {
			time.Sleep(time.Millisecond)
		}
	}
}

// openUDPWith opens the listener while send starts shortly after it is up.
func openUDPWith(t *testing.T, port int, send func(addr string)) Reader {
	t.Helper()
	type res struct {
		r   Reader
		err error
	}
	ch := make(chan res, 1)
	go func() {
		r, err := Open(context.Background(), fmt.Sprintf("udp://127.0.0.1:%d", port), Env{})
		ch <- res{r, err}
	}()
	time.Sleep(200 * time.Millisecond)
	go send(fmt.Sprintf("127.0.0.1:%d", port))
	o := <-ch
	require.NoError(t, o.err)
	return o.r
}

// udpHeldBackVideo bounds the video frames the demuxer keeps pending when a
// live source goes quiet (the fixture shows 2: the pending access unit and
// the group waiting on the next PES). Audio keeps none.
const udpHeldBackVideo = 2

// heldBackCounts returns the per-track frame counts a live demuxer yields for
// the fixture when the source then goes quiet without ending: it flushes
// nothing at the end, so the last access units stay pending. This is the
// demuxer's own holdback (video 2 for this fixture, audio 0), independent of
// the UDP layer; the tests compare the UDP reader against it exactly.
func heldBackCounts(t *testing.T) map[int]int {
	t.Helper()
	data, err := os.ReadFile(fixturePath("h264_aac.ts"))
	require.NoError(t, err)
	src := &quietReader{data: data, quiet: make(chan struct{})}
	d, err := ts.NewDemuxerWithOptions(src, ts.DemuxOptions{Remap: true})
	require.NoError(t, err)
	var mu sync.Mutex
	counts := map[int]int{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			f, err := d.Read()
			if err != nil {
				return
			}
			mu.Lock()
			counts[f.Track]++
			mu.Unlock()
		}
	}()
	require.Eventually(t, func() bool { return src.drained.Load() }, 3*time.Second, 5*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	out := maps.Clone(counts)
	mu.Unlock()
	close(src.quiet)
	<-done
	return out
}

// quietReader serves data, then blocks until quiet is closed, then EOF.
type quietReader struct {
	data    []byte
	quiet   chan struct{}
	drained atomic.Bool
}

func (q *quietReader) Read(p []byte) (int, error) {
	if len(q.data) == 0 {
		q.drained.Store(true)
		<-q.quiet
		return 0, io.EOF
	}
	n := copy(p, q.data)
	q.data = q.data[n:]
	return n, nil
}

// waitSettled waits until the reader has received sent datagrams and its
// per-track counts equal want, and checks they then stay put.
func waitSettled(t *testing.T, r Reader, res *srtResult, mu *sync.Mutex, sent int, want map[int]int) {
	t.Helper()
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return r.Stats()["datagrams"] >= uint64(sent) && maps.Equal(res.counts, want)
	}, 5*time.Second, 10*time.Millisecond)
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, want, res.counts, "counts stay exact")
}

// assertClean checks a loss-free input: nothing lost, no CC errors, nothing
// that is not TS.
func assertClean(t *testing.T, st map[string]uint64) {
	t.Helper()
	assert.Zero(t, st["lost_frames"], "lost_frames")
	assert.Zero(t, st["cc_errors"], "cc_errors")
	assert.Zero(t, st["non_ts_datagrams"], "non_ts_datagrams")
	assert.Zero(t, st["rtp_gaps"], "rtp_gaps")
	assert.Zero(t, st["decode_errors"], "decode_errors")
}

func TestUDPRawTS(t *testing.T) {
	port := freeUDPPort(t)
	dgs := fixtureDatagrams(t)
	r := openUDPWith(t, port, func(addr string) { sendAll(t, addr, dgs) })
	assert.Equal(t, Spec{Live: true, Passive: true}, r.Spec())
	res, wg, mu := drain(r)
	full, want := fixtureCounts(t, "h264_aac.ts"), heldBackCounts(t)
	assert.Equal(t, full[1], want[1], "audio: nothing held back")
	assert.GreaterOrEqual(t, want[0], full[0]-udpHeldBackVideo)
	waitSettled(t, r, res, mu, len(dgs), want)
	assertClean(t, r.Stats())
	require.NoError(t, r.Close())
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	assert.Zero(t, res.breaks)
}

func TestUDPRTPWrapped(t *testing.T) {
	styles := []rtpStyle{{pt: 33}, {pt: 96}, {pt: 96, csrc: 2, ext: true}}
	for _, st := range styles {
		t.Run(fmt.Sprintf("pt%d_csrc%d_ext%v", st.pt, st.csrc, st.ext), func(t *testing.T) {
			port := freeUDPPort(t)
			wrapped := make([][]byte, 0, len(fixtureDatagrams(t)))
			for i, d := range fixtureDatagrams(t) {
				wrapped = append(wrapped, rtpWrap(uint16(65500+i), st, d)) // crosses the seq wrap
			}
			r := openUDPWith(t, port, func(addr string) { sendAll(t, addr, wrapped) })
			res, wg, mu := drain(r)
			waitSettled(t, r, res, mu, len(wrapped), heldBackCounts(t))
			assertClean(t, r.Stats())
			require.NoError(t, r.Close())
			wg.Wait()
		})
	}
}

func TestUDPRTPGapsCounted(t *testing.T) {
	port := freeUDPPort(t)
	wrapped := make([][]byte, 0, len(fixtureDatagrams(t)))
	for i, d := range fixtureDatagrams(t)[:60] {
		if i == 20 || i == 21 || i == 40 {
			continue // three lost datagrams
		}
		wrapped = append(wrapped, rtpWrap(uint16(i), rtpStyle{pt: 33}, d))
	}
	r := openUDPWith(t, port, func(addr string) { sendAll(t, addr, wrapped) })
	defer r.Close()
	_, _, _ = drain(r)
	assert.Eventually(t, func() bool { return r.Stats()["datagrams"] >= uint64(len(wrapped)) }, 3*time.Second, 20*time.Millisecond)
	assert.EqualValues(t, 3, r.Stats()["rtp_gaps"])
}

func TestUDPLossAndReorder(t *testing.T) {
	port := freeUDPPort(t)
	rng := rand.New(rand.NewSource(7)) //nolint:gosec // seeded test data.
	src := fixtureDatagrams(t)
	var dgs [][]byte
	for i := 0; i < len(src); i++ {
		if i > 3 && rng.Intn(100) == 0 { // keep the PAT/PMT head intact
			continue
		}
		dgs = append(dgs, src[i])
	}
	for i := 4; i+1 < len(dgs); i++ {
		if rng.Intn(100) == 0 {
			dgs[i], dgs[i+1] = dgs[i+1], dgs[i]
			i++
		}
	}
	require.Less(t, len(dgs), len(src), "the seed drops something")
	r := openUDPWith(t, port, func(addr string) { sendAll(t, addr, dgs) })
	res, wg, mu := drain(r)
	assert.Eventually(t, func() bool { return r.Stats()["datagrams"] >= uint64(len(dgs)) }, 5*time.Second, 20*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	stats := r.Stats()
	require.NoError(t, r.Close())
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	assert.Positive(t, stats["lost_frames"])
	require.NotEmpty(t, res.frames)
	clockCheck(t, r, res.frames)
}

func TestUDPMulticast(t *testing.T) {
	const group = "239.255.0.1"
	lo := loopbackInterface(t)
	port := freeUDPPort(t)
	dgs := fixtureDatagrams(t)

	stop := make(chan struct{})
	var senders sync.WaitGroup
	defer func() { close(stop); senders.Wait() }() // no sender outlives the test, skip or not
	senders.Add(1)
	go func() {
		defer senders.Done()
		select {
		case <-time.After(300 * time.Millisecond):
		case <-stop:
			return
		}
		c, err := net.DialUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, &net.UDPAddr{IP: net.ParseIP(group), Port: port})
		if err != nil {
			return
		}
		defer c.Close()
		for i, d := range dgs {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := c.Write(d); err != nil {
				return
			}
			if i%8 == 7 {
				time.Sleep(time.Millisecond)
			}
		}
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r, err := Open(ctx, fmt.Sprintf("udp://%s:%d?iface=%s", group, port, lo), Env{})
	if err != nil && strings.Contains(err.Error(), "udp: listen") {
		t.Skipf("multicast group %s cannot be joined on %s here: %v", group, lo, err)
	}
	require.NoError(t, err)
	res, wg, mu := drain(r)
	waitSettled(t, r, res, mu, len(dgs), heldBackCounts(t))
	assertClean(t, r.Stats())
	require.NoError(t, r.Close())
	wg.Wait()
}

func loopbackInterface(t *testing.T) string {
	t.Helper()
	ifs, err := net.Interfaces()
	require.NoError(t, err)
	for _, i := range ifs {
		if i.Flags&net.FlagLoopback != 0 && i.Flags&net.FlagUp != 0 {
			return i.Name
		}
	}
	t.Skip("no loopback interface")
	return ""
}

func TestUDPCloseUnblocksAndAbortsOpen(t *testing.T) {
	port := freeUDPPort(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Open(ctx, fmt.Sprintf("udp://127.0.0.1:%d", port), Env{})
	require.Error(t, err)
	assert.Less(t, time.Since(start), 2*time.Second)
	assert.False(t, udpPortBound(port), "an aborted open frees the port")

	r := openUDPWith(t, port, func(addr string) { sendAll(t, addr, fixtureDatagrams(t)[:20]) })
	done := make(chan error, 1)
	go func() {
		for {
			if _, err := r.Read(); err != nil {
				done <- err
				return
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)
	require.NoError(t, r.Close())
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock Read")
	}
	assert.False(t, udpPortBound(port), "Close frees the port")
}

func TestOpenUDP(t *testing.T) {
	port := freeUDPPort(t)
	r := openUDPWith(t, port, func(addr string) { sendAll(t, addr, fixtureDatagrams(t)) })
	defer r.Close()
	assert.Equal(t, Spec{Live: true, Passive: true}, r.Spec())
	assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, r.Kinds())
	st := r.Stats()
	assert.Contains(t, st, "cc_errors")
	assert.Contains(t, st, "lost_frames")
	assert.Positive(t, st["udp_read_buffer"])
	assert.Contains(t, st, "udp_read_buffer_low")

	// A bad interface and a bad port fail the open.
	_, err := Open(context.Background(), "udp://239.255.0.1:5000?iface=nosuchif0", Env{})
	require.Error(t, err)
	_, err = Open(context.Background(), "udp://@:0", Env{})
	require.Error(t, err)
}

func TestUDPWithFFmpeg(t *testing.T) {
	ffmpeg := testpeer.Require(t, "ffmpeg", "udp")
	port := freeUDPPort(t)
	type res struct {
		r   Reader
		err error
	}
	ch := make(chan res, 1)
	go func() {
		r, err := Open(context.Background(), "udp://@:"+strconv.Itoa(port), Env{})
		ch <- res{r, err}
	}()
	time.Sleep(300 * time.Millisecond)
	cmd := exec.CommandContext(t.Context(), ffmpeg, "-hide_banner", "-loglevel", "error", "-re",
		"-i", fixturePath("h264_aac.ts"), "-c", "copy", "-f", "mpegts",
		fmt.Sprintf("udp://127.0.0.1:%d?pkt_size=1316", port))
	require.NoError(t, cmd.Start())
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	o := <-ch
	require.NoError(t, o.err)
	r, wg, mu := drain(o.r)
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(r.frames) > 100
	}, 15*time.Second, 50*time.Millisecond)
	require.NoError(t, o.r.Close())
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	clockCheck(t, o.r, r.frames)
	assert.Zero(t, o.r.Stats()["lost_frames"])
	assert.Zero(t, o.r.Stats()["non_ts_datagrams"])
}
