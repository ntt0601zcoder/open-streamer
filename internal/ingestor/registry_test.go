package ingestor_test

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntthuan060102github/open-streamer/internal/buffer"
	"github.com/ntthuan060102github/open-streamer/internal/domain"
	"github.com/ntthuan060102github/open-streamer/internal/ingestor"
)

func TestRegistry_RegisterAndLookup(t *testing.T) {
	t.Parallel()

	buf := buffer.NewServiceForTesting(64)
	reg := ingestor.NewRegistry()

	reg.Register("mykey", "stream-1", buf, "")

	writeID, streamID, gotBuf, err := reg.Lookup("mykey")
	require.NoError(t, err)
	assert.Equal(t, domain.StreamCode("stream-1"), writeID)
	assert.Equal(t, domain.StreamCode("stream-1"), streamID)
	assert.Same(t, buf, gotBuf)
}

func TestRegistry_RegisterSeparateBufferWriteID(t *testing.T) {
	t.Parallel()

	buf := buffer.NewServiceForTesting(64)
	reg := ingestor.NewRegistry()
	logical := domain.StreamCode("cam1")
	raw := buffer.RawIngestBufferID(logical)

	reg.Register("pushkey", logical, buf, raw)

	writeID, streamID, gotBuf, err := reg.Lookup("pushkey")
	require.NoError(t, err)
	assert.Equal(t, raw, writeID)
	assert.Equal(t, logical, streamID)
	assert.Same(t, buf, gotBuf)
}

func TestRegistry_Lookup_NotFound(t *testing.T) {
	t.Parallel()

	reg := ingestor.NewRegistry()
	_, _, _, err := reg.Lookup("nonexistent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nonexistent")
}

func TestRegistry_Overwrite(t *testing.T) {
	t.Parallel()

	buf1 := buffer.NewServiceForTesting(64)
	buf2 := buffer.NewServiceForTesting(64)
	reg := ingestor.NewRegistry()

	reg.Register("key", "stream-1", buf1, "")
	reg.Register("key", "stream-2", buf2, "")

	writeID, streamID, gotBuf, err := reg.Lookup("key")
	require.NoError(t, err)
	assert.Equal(t, domain.StreamCode("stream-2"), writeID)
	assert.Equal(t, domain.StreamCode("stream-2"), streamID)
	assert.Same(t, buf2, gotBuf)
}

func TestRegistry_Unregister(t *testing.T) {
	t.Parallel()

	buf := buffer.NewServiceForTesting(64)
	reg := ingestor.NewRegistry()

	reg.Register("key", "stream-1", buf, "")
	reg.Unregister("key")

	_, _, _, err := reg.Lookup("key")
	require.Error(t, err)
}

func TestRegistry_Unregister_NonExistent(t *testing.T) {
	t.Parallel()

	reg := ingestor.NewRegistry()
	// Must not panic.
	assert.NotPanics(t, func() { reg.Unregister("ghost") })
}

func TestRegistry_MultipleKeys(t *testing.T) {
	t.Parallel()

	reg := ingestor.NewRegistry()
	buf := buffer.NewServiceForTesting(64)

	keys := []string{"stream-a", "stream-b", "stream-c"}
	for _, k := range keys {
		reg.Register(k, domain.StreamCode(k), buf, "")
	}

	for _, k := range keys {
		writeID, streamID, _, err := reg.Lookup(k)
		require.NoError(t, err)
		assert.Equal(t, domain.StreamCode(k), writeID)
		assert.Equal(t, domain.StreamCode(k), streamID)
	}
}

func TestRegistry_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	reg := ingestor.NewRegistry()
	buf := buffer.NewServiceForTesting(64)
	const goroutines = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func(i int) {
			defer wg.Done()
			key := "key"
			reg.Register(key, domain.StreamCode("s"), buf, "")
			_, _, _, _ = reg.Lookup(key)
			if i%5 == 0 {
				reg.Unregister(key)
			}
		}(i)
	}
	wg.Wait()
}
