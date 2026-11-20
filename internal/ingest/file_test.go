package ingest

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
	"github.com/ntt0601zcoder/open-streamer/pkg/mpegts"
)

func fixtureAbs(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(fixturePath(name))
	require.NoError(t, err)
	return p
}

func demuxFrames(t *testing.T, name string) []media.Frame {
	t.Helper()
	b, err := os.ReadFile(fixturePath(name))
	require.NoError(t, err)
	d, err := ts.NewDemuxerWithOptions(bytes.NewReader(b), ts.DemuxOptions{Remap: true})
	require.NoError(t, err)
	var out []media.Frame
	for {
		f, err := d.Read()
		if err == io.EOF {
			return out
		}
		require.NoError(t, err)
		out = append(out, f)
	}
}

func TestFileReader(t *testing.T) {
	want := demuxFrames(t, "h264_aac.ts")
	now := time.Unix(1000, 0)
	r, err := Open(context.Background(), fixtureAbs(t, "h264_aac.ts"), Env{Now: func() time.Time { return now }})
	require.NoError(t, err)
	defer r.Close()
	assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, r.Kinds())

	n := 0
	for {
		f, err := r.Read()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		require.False(t, f.Arrival.IsZero())
		require.Less(t, n, len(want))
		assert.Equal(t, want[n].DTS, f.DTS)
		assert.Equal(t, want[n].Track, f.Track)
		n++
	}
	assert.Equal(t, len(want), n)
	st := r.Stats()
	for _, k := range []string{"lost_frames", "pmt_changes", "split_aus", "split_aus_reordered", "continued", "cc_errors"} {
		assert.Contains(t, st, k)
	}
	assert.Zero(t, st["pmt_changes"])
}

func TestFileLoop(t *testing.T) {
	path := fixtureAbs(t, "h264_aac.ts")
	r, err := Open(context.Background(), "file://"+path+"?loop=1", Env{})
	require.NoError(t, err)
	defer r.Close()

	var first []Frame
	var last Frame
	for {
		f, err := r.Read()
		if err == ErrDiscontinuity {
			break
		}
		require.NoError(t, err)
		if len(first) < 2 {
			first = append(first, f)
		}
		last = f
	}
	require.NotZero(t, last.DTS)
	for i := range first {
		f, err := r.Read()
		require.NoError(t, err)
		assert.Same(t, first[i].Config, f.Config, "config pointers are stable across passes")
		assert.Equal(t, first[i].DTS, f.DTS)
	}
	assert.EqualValues(t, 2, r.Stats()["passes"])
}

// stallFile serves data, then blocks Read until Close, like a FIFO whose
// writer went quiet. blocked is closed when a Read first blocks.
type stallFile struct {
	data    []byte
	blocked chan struct{}
	closed  chan struct{}
	once    sync.Once
	close   sync.Once
}

func newStallFile(data []byte) *stallFile {
	return &stallFile{data: data, blocked: make(chan struct{}), closed: make(chan struct{})}
}

func (f *stallFile) Read(p []byte) (int, error) {
	if len(f.data) > 0 {
		n := copy(p, f.data)
		f.data = f.data[n:]
		return n, nil
	}
	f.once.Do(func() { close(f.blocked) })
	<-f.closed
	return 0, os.ErrClosed
}

func (f *stallFile) Close() error {
	f.close.Do(func() { close(f.closed) })
	return nil
}

func TestFileCloseDuringBlockedRead(t *testing.T) {
	b, err := os.ReadFile(fixturePath("h264_aac.ts"))
	require.NoError(t, err)
	sf := newStallFile(b)
	r := &fileReader{
		path: "stall.ts", loop: true, done: map[string]uint64{}, now: time.Now,
		open: func(string) (io.ReadCloser, error) { return sf, nil },
	}
	require.NoError(t, r.openPass(context.Background()))

	done := make(chan error, 1)
	go func() {
		for {
			if _, err := r.Read(); err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case <-sf.blocked:
	case err := <-done:
		t.Fatalf("Read ended before blocking: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Read never blocked")
	}
	start := time.Now()
	require.NoError(t, r.Close())
	select {
	case err := <-done:
		assert.ErrorIs(t, err, errClosed)
	case <-time.After(time.Second):
		t.Fatal("Read did not stop after Close")
	}
	assert.Less(t, time.Since(start), 100*time.Millisecond)
}

func TestFileLoopEmptyPass(t *testing.T) {
	// A PAT and a PMT, no PES.
	var buf bytes.Buffer
	w := mpegts.NewWriter(&buf)
	require.NoError(t, w.SetProgram(mpegts.PMT{PCRPID: 0x100, Streams: []mpegts.Stream{{PID: 0x100, Type: 0x1B}}}))
	require.NoError(t, w.WriteTables())
	path := filepath.Join(t.TempDir(), "empty.ts")
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))

	r, err := Open(context.Background(), "file://"+path+"?loop=1", Env{})
	require.NoError(t, err)
	defer r.Close()
	done := make(chan error, 1)
	go func() {
		_, err := r.Read()
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrDiscontinuity)
		assert.NotErrorIs(t, err, io.EOF)
	case <-time.After(2 * time.Second):
		t.Fatal("Read spun on an empty looping file")
	}
}

func TestFileReaderStatsConcurrent(t *testing.T) {
	r, err := Open(context.Background(), "file://"+fixtureAbs(t, "h264_aac.ts")+"?loop=1", Env{})
	require.NoError(t, err)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = r.Stats()
				_ = r.Kinds()
			}
		}
	}()
	for range 400 {
		_, err := r.Read()
		if err != nil {
			require.ErrorIs(t, err, ErrDiscontinuity)
		}
	}
	close(stop)
	wg.Wait()
	require.NoError(t, r.Close())
}

func TestTSReaderKindsStable(t *testing.T) {
	r := openTSFixture(t, "h264_aac_pmtchange.ts")
	defer r.Close()
	readThroughBreaks(t, r, nil)
	assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, r.Kinds())
	assert.EqualValues(t, 1, r.Stats()["pmt_changes"])
}

func TestTSReaderPMTChangeIsOneEpoch(t *testing.T) {
	r := openTSFixture(t, "h264_aac_pmtchange.ts")
	defer r.Close()
	c := clock.New(clock.Options{Kinds: r.Kinds()})

	oldCfg := map[int]map[*media.TrackConfig]bool{}
	newCfg := map[int]map[*media.TrackConfig]bool{}
	breaks, before, after := 0, 0, 0
	var firstAfter *Frame
	readThroughBreaks(t, r, func(f *Frame, err error) {
		if err != nil {
			breaks++
			c.Break(false)
			return
		}
		side := oldCfg
		if breaks > 0 {
			side = newCfg
			after++
			if firstAfter == nil {
				firstAfter = f
			}
		} else {
			before++
		}
		if side[f.Track] == nil {
			side[f.Track] = map[*media.TrackConfig]bool{}
		}
		side[f.Track][f.Config] = true
		c.PushAt(f.Frame, f.Arrival)
	})
	c.Flush()

	assert.Equal(t, 1, breaks)
	assert.Positive(t, before)
	assert.Positive(t, after)
	assert.Len(t, c.Epochs(), 2)
	require.NotNil(t, firstAfter)
	for track := range 2 {
		// One Config per side per track: nothing of the old PMT leaks past
		// the break, and the remapped slot gets a new pointer although the
		// codecs are identical.
		require.Len(t, oldCfg[track], 1, "track %d", track)
		require.Len(t, newCfg[track], 1, "track %d", track)
		for o := range oldCfg[track] {
			for n := range newCfg[track] {
				assert.NotSame(t, o, n, "track %d", track)
				assert.True(t, o.Equal(n), "identical codecs, track %d", track)
			}
		}
	}
	assert.True(t, newCfg[firstAfter.Track][firstAfter.Config], "the first frame after the break has a new Config")
}

func TestTSReaderAppendedSlotIsNoBreak(t *testing.T) {
	// Video only, then the PMT gains an audio stream.
	b, err := os.ReadFile(fixturePath("h264_aac.ts"))
	require.NoError(t, err)
	d, err := ts.NewDemuxerWithOptions(bytes.NewReader(b), ts.DemuxOptions{Remap: true})
	require.NoError(t, err)
	var frames []media.Frame
	for {
		f, err := d.Read()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		frames = append(frames, f)
	}
	cfgs := make([]*media.TrackConfig, 2)
	for _, f := range frames {
		cfgs[f.Track] = f.Config
	}

	var out bytes.Buffer
	m, err := ts.NewMuxer(&out, []*media.TrackConfig{cfgs[0]})
	require.NoError(t, err)
	video, switched := 0, false
	for _, f := range frames {
		if f.Track == 0 {
			video++
			if video == 40 && !switched {
				require.NoError(t, m.SetTracks(cfgs))
				switched = true
			}
		} else if !switched {
			continue
		}
		require.NoError(t, m.WriteFrame(f))
	}

	r, err := NewTSReader(io.NopCloser(bytes.NewReader(out.Bytes())), nil)
	require.NoError(t, err)
	defer r.Close()
	assert.Equal(t, []media.Kind{media.KindVideo}, r.Kinds())
	var audio int
	readThroughBreaks(t, r, func(f *Frame, err error) {
		require.NoError(t, err, "no break")
		if f.Track == 1 {
			audio++
		}
	})
	assert.Positive(t, audio)
	assert.EqualValues(t, 1, r.Stats()["pmt_changes"])
	assert.Equal(t, []media.Kind{media.KindVideo, media.KindAudio}, r.Kinds())
}

func openTSFixture(t *testing.T, name string) Reader {
	t.Helper()
	f, err := os.Open(fixturePath(name))
	require.NoError(t, err)
	r, err := NewTSReader(f, nil)
	require.NoError(t, err)
	return r
}

// readThroughBreaks reads to EOF, calling fn per frame, or per
// ErrDiscontinuity with a nil frame.
func readThroughBreaks(t *testing.T, r Reader, fn func(*Frame, error)) {
	t.Helper()
	for {
		f, err := r.Read()
		switch err {
		case io.EOF:
			return
		case ErrDiscontinuity:
			if fn != nil {
				fn(nil, err)
			}
		default:
			require.NoError(t, err)
			if fn != nil {
				fn(&f, nil)
			}
		}
	}
}

func TestNewTSReaderErrors(t *testing.T) {
	_, err := NewTSReader(io.NopCloser(bytes.NewReader([]byte("junk"))), nil)
	assert.Error(t, err)

	// Closing the reader aborts a blocked open.
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		_, err := NewTSReader(pr, nil)
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	_ = pr.Close()
	select {
	case err := <-errc:
		assert.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("open did not abort")
	}
	_ = pw.Close()
}

func TestOpenContextAbortsBlockedOpen(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := openTS(ctx, pr, nil, Spec{}, &media.ConfigCache{})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}
