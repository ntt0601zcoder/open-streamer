package ingest

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/testpeer"
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
	c, err := net.Dial("udp", addr)
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
func openUDPWith(t *testing.T, port int, query string, send func(addr string)) Reader {
	t.Helper()
	type res struct {
		r   Reader
		err error
	}
	ch := make(chan res, 1)
	go func() {
		r, err := Open(context.Background(), fmt.Sprintf("udp://127.0.0.1:%d%s", port, query), Env{})
		ch <- res{r, err}
	}()
	time.Sleep(200 * time.Millisecond)
	go send(fmt.Sprintf("127.0.0.1:%d", port))
	o := <-ch
	require.NoError(t, o.err)
	return o.r
}

// udpHeldBack is how many trailing frames of a track a live demuxer still
// holds when the sender stops: they complete only when the next one starts.
const udpHeldBack = 2

// assertCounts checks the per-track counts equal want, allowing for them.
func assertCounts(t *testing.T, got, want map[int]int) {
	t.Helper()
	for tr, n := range want {
		assert.GreaterOrEqual(t, got[tr], n-udpHeldBack, "track %d", tr)
		assert.LessOrEqual(t, got[tr], n, "track %d", tr)
	}
}

func waitCounts(t *testing.T, res *srtResult, mu interface {
	Lock()
	Unlock()
}, want map[int]int) {
	t.Helper()
	assert.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return res.counts[0] >= want[0]-udpHeldBack && res.counts[1] >= want[1]-udpHeldBack
	}, 5*time.Second, 20*time.Millisecond)
}

func TestUDPRawTS(t *testing.T) {
	port := freeUDPPort(t)
	dgs := fixtureDatagrams(t)
	r := openUDPWith(t, port, "", func(addr string) { sendAll(t, addr, dgs) })
	assert.Equal(t, Spec{Live: true, Passive: true}, r.Spec())
	res, wg, mu := drain(r)
	want := fixtureCounts(t, "h264_aac.ts")
	waitCounts(t, res, mu, want)
	require.NoError(t, r.Close())
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	assertCounts(t, res.counts, want)
	assert.Zero(t, res.breaks)
	assert.Zero(t, r.Stats()["lost_frames"])
}

func TestUDPRTPWrapped(t *testing.T) {
	styles := []rtpStyle{{pt: 33}, {pt: 96}, {pt: 96, csrc: 2, ext: true}}
	for _, st := range styles {
		t.Run(fmt.Sprintf("pt%d_csrc%d_ext%v", st.pt, st.csrc, st.ext), func(t *testing.T) {
			port := freeUDPPort(t)
			var wrapped [][]byte
			for i, d := range fixtureDatagrams(t) {
				wrapped = append(wrapped, rtpWrap(uint16(65500+i), st, d)) // crosses the seq wrap
			}
			r := openUDPWith(t, port, "", func(addr string) { sendAll(t, addr, wrapped) })
			res, wg, mu := drain(r)
			want := fixtureCounts(t, "h264_aac.ts")
			waitCounts(t, res, mu, want)
			stats := r.Stats()
			require.NoError(t, r.Close())
			wg.Wait()
			mu.Lock()
			defer mu.Unlock()
			assertCounts(t, res.counts, want)
			assert.Zero(t, stats["rtp_gaps"])
			assert.Zero(t, stats["non_ts_datagrams"])
		})
	}
}

func TestUDPRTPGapsCounted(t *testing.T) {
	port := freeUDPPort(t)
	var wrapped [][]byte
	for i, d := range fixtureDatagrams(t)[:60] {
		if i == 20 || i == 21 || i == 40 {
			continue // three lost datagrams
		}
		wrapped = append(wrapped, rtpWrap(uint16(i), rtpStyle{pt: 33}, d))
	}
	r := openUDPWith(t, port, "", func(addr string) { sendAll(t, addr, wrapped) })
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
	r := openUDPWith(t, port, "", func(addr string) { sendAll(t, addr, dgs) })
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
	type res struct {
		r   Reader
		err error
	}
	ch := make(chan res, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		r, err := Open(ctx, fmt.Sprintf("udp://%s:%d?iface=%s", group, port, lo), Env{})
		ch <- res{r, err}
	}()
	time.Sleep(300 * time.Millisecond)
	go func() {
		c, err := net.DialUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, &net.UDPAddr{IP: net.ParseIP(group), Port: port})
		if err != nil {
			return
		}
		defer c.Close()
		for i, d := range fixtureDatagrams(t) {
			if _, err := c.Write(d); err != nil {
				return
			}
			if i%8 == 7 {
				time.Sleep(time.Millisecond)
			}
		}
	}()
	o := <-ch
	if o.err != nil {
		t.Skipf("multicast on %s is not usable here: %v", lo, o.err)
	}
	res2, wg, mu := drain(o.r)
	want := fixtureCounts(t, "h264_aac.ts")
	waitCounts(t, res2, mu, want)
	require.NoError(t, o.r.Close())
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	assertCounts(t, res2.counts, want)
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

	r := openUDPWith(t, port, "", func(addr string) { sendAll(t, addr, fixtureDatagrams(t)[:20]) })
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
	r := openUDPWith(t, port, "", func(addr string) { sendAll(t, addr, fixtureDatagrams(t)) })
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
	assert.NotContains(t, err.Error(), "sekrit")
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
}
