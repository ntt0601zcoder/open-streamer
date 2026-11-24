package transcode

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/clock"
	"github.com/ntt0601zcoder/open-streamer/internal/hub"
	"github.com/ntt0601zcoder/open-streamer/internal/media"
	"github.com/ntt0601zcoder/open-streamer/internal/ts"
)

// step is one write to a source hub: an anchor, or a frame that arrives at
// stream tick at.
type step struct {
	anchor *media.Anchor
	f      media.Frame
	at     int64
}

// fixtureSteps demuxes fixtures, one after the other, through one Clock and
// returns their anchors and frames in arrival order, each anchor before its
// epoch's first frame.
func fixtureSteps(t *testing.T, names ...string) []step {
	t.Helper()
	var c *clock.Clock
	var out []step
	anchored := map[uint32]bool{}
	add := func(frames []media.Frame) {
		for _, f := range frames {
			if !anchored[f.Epoch] {
				anchored[f.Epoch] = true
				for _, e := range c.Epochs() {
					if e.ID == f.Epoch {
						out = append(out, step{anchor: &media.Anchor{Epoch: e.ID, Start: e.Start, Wall: e.Wall}})
					}
				}
			}
			out = append(out, step{f: f, at: f.DTS})
		}
	}
	for _, name := range names {
		data, err := os.ReadFile("../../testdata/fixtures/" + name)
		require.NoError(t, err)
		d, err := ts.NewDemuxer(bytes.NewReader(data))
		require.NoError(t, err)
		if c == nil {
			c = clock.New(clock.Options{Kinds: d.TrackKinds()})
		}
		for {
			f, err := d.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			require.NoError(t, err)
			add(c.Push(f))
		}
	}
	add(c.Flush())
	return out
}

// audioLead returns steps in an arrival order where, within each epoch, audio
// leads video by lead ticks: video arrives at its DTS, audio at its PTS −
// lead but never before the epoch's first video frame (the Clock releases an
// epoch from its keyframe). It is T1's reordering helper of
// TestAudioCutAtVideoPTS, applied per epoch.
func audioLead(steps []step, lead int64) []step {
	var out []step
	for i := 0; i < len(steps); {
		j := i + 1
		for j < len(steps) && (steps[j].anchor == nil || steps[j].anchor.Epoch == steps[i].anchor.Epoch) {
			j++
		}
		ep := slices.Clone(steps[i:j])
		first := int64(0)
		found := false
		for _, s := range ep {
			if s.anchor == nil && s.f.Kind() == media.KindVideo {
				first, found = s.f.DTS, true
				break
			}
		}
		for k := range ep {
			if s := &ep[k]; s.anchor == nil && s.f.Kind() == media.KindAudio {
				s.at = s.f.PTS - lead
				if found {
					s.at = max(s.at, first)
				}
			}
		}
		head, body := ep[:1], ep[1:]
		if head[0].anchor == nil {
			head, body = nil, ep
		}
		slices.SortStableFunc(body, func(a, b step) int {
			if c := cmp.Compare(a.at, b.at); c != 0 {
				return c
			}
			return cmp.Compare(kindRank(a.f), kindRank(b.f))
		})
		out = append(out, head...)
		out = append(out, body...)
		i = j
	}
	return out
}

func kindRank(f media.Frame) int {
	if f.Kind() == media.KindVideo {
		return 0
	}
	return 1
}

// audioOnly drops the video frames.
func audioOnly(steps []step) []step {
	return slices.DeleteFunc(slices.Clone(steps), func(s step) bool {
		return s.anchor == nil && s.f.Kind() == media.KindVideo
	})
}

// frames returns the frames of steps.
func frames(steps []step) []media.Frame {
	var out []media.Frame
	for _, s := range steps {
		if s.anchor == nil {
			out = append(out, s.f)
		}
	}
	return out
}

// play writes steps to h, paced at speed times real time (0: unpaced), and
// closes h when close is set. It stops early when ctx ends. The returned
// channel is closed once it is done.
func play(ctx context.Context, h *hub.Hub, steps []step, speed float64, closeAfter bool) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if closeAfter {
			defer h.Close()
		}
		start := time.Now()
		var first int64
		haveFirst := false
		for _, s := range steps {
			if s.anchor != nil {
				h.SetAnchor(*s.anchor)
				continue
			}
			if speed > 0 {
				if !haveFirst {
					first, haveFirst = s.at, true
				}
				due := start.Add(time.Duration(float64(media.ToDuration(s.at-first)) / speed))
				if d := time.Until(due); d > 0 {
					select {
					case <-ctx.Done():
						return
					case <-time.After(d):
					}
				}
			}
			if ctx.Err() != nil {
				return
			}
			h.Write(s.f)
		}
	}()
	return done
}

// waitSub waits until h has a subscriber called name.
func waitSub(t *testing.T, h *hub.Hub, name string) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, s := range h.Stats() {
			if s.Name == name {
				return true
			}
		}
		return false
	}, 5*time.Second, time.Millisecond)
}

// collector records what a rendition hub delivers, and the anchor the hub
// holds when each epoch's first frame arrives.
type collector struct {
	done    chan struct{}
	mu      sync.Mutex
	frames  []media.Frame
	anchors map[uint32]media.Anchor
	missing []uint32 // epochs whose anchor was not set before their first frame
}

func collect(h *hub.Hub) *collector {
	c := &collector{done: make(chan struct{}), anchors: map[uint32]media.Anchor{}}
	sub := h.Subscribe("test", 1<<16)
	go func() {
		defer close(c.done)
		for d := range sub.C() {
			c.mu.Lock()
			f := d.Frame
			if _, ok := c.anchors[f.Epoch]; !ok && !slices.Contains(c.missing, f.Epoch) {
				if a, ok := h.Anchor(f.Epoch); ok {
					c.anchors[f.Epoch] = a
				} else {
					c.missing = append(c.missing, f.Epoch)
				}
			}
			c.frames = append(c.frames, f)
			c.mu.Unlock()
		}
	}()
	return c
}

func (c *collector) wait(t *testing.T) []media.Frame {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(10 * time.Second):
		t.Fatal("rendition hub not closed")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.frames
}

// epochs returns the epochs of frames, in order of first appearance.
func epochs(fs []media.Frame) []uint32 {
	var out []uint32
	for _, f := range fs {
		if !slices.Contains(out, f.Epoch) {
			out = append(out, f.Epoch)
		}
	}
	return out
}

// firstOfEpoch returns the first frame of epoch e.
func firstOfEpoch(fs []media.Frame, e uint32) media.Frame {
	for _, f := range fs {
		if f.Epoch == e {
			return f
		}
	}
	return media.Frame{}
}
