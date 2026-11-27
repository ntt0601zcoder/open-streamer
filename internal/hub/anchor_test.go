package hub

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

func TestHubAnchors(t *testing.T) {
	h := New(Options{})
	t0 := time.Unix(1000, 0)
	for e := uint32(1); e <= 70; e++ {
		h.SetAnchor(media.Anchor{Epoch: e, Start: int64(e) * 10, Wall: t0.Add(time.Duration(e) * time.Second)})
	}
	_, ok := h.Anchor(6)
	assert.False(t, ok)
	a, ok := h.Anchor(7)
	assert.True(t, ok)
	assert.Equal(t, int64(70), a.Start)
	a, ok = h.Anchor(70)
	assert.True(t, ok)
	assert.Equal(t, int64(700), a.Start)

	h.SetAnchor(media.Anchor{Epoch: 70, Start: 1, Wall: t0})
	a, _ = h.Anchor(70)
	assert.Equal(t, int64(1), a.Start)
	_, ok = h.Anchor(7)
	assert.True(t, ok, "replacing must not evict")

	var wg sync.WaitGroup
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 500 {
				switch g % 3 {
				case 0:
					h.SetAnchor(media.Anchor{Epoch: uint32(100 + i), Start: int64(i)})
				case 1:
					h.Anchor(uint32(100 + i))
				default:
					h.Write(vid(0, int64(i)*vDur, i%10 == 0))
				}
			}
		}()
	}
	wg.Wait()
}
