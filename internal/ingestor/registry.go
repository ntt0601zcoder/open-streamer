package ingestor

import (
	"fmt"
	"sync"

	"github.com/ntthuan060102github/open-streamer/internal/buffer"
	"github.com/ntthuan060102github/open-streamer/internal/domain"
)

// Registry maps ingest credentials (stream key, SRT stream ID) to the
// destination buffer for a specific stream. Push servers (RTMP, SRT) use this
// to route incoming connections to the correct Buffer Hub slot.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]registryEntry
}

type registryEntry struct {
	streamID      domain.StreamCode // logical stream (logging, health)
	bufferWriteID domain.StreamCode // Buffer Hub id passed to buf.Write
	buf           *buffer.Service
}

// NewRegistry constructs an empty Registry.
func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]registryEntry)}
}

// Register maps key (stream key or SRT stream ID) to a stream's buffer.
// bufferWriteID is the slot used for buf.Write; if empty, streamID is used.
// Registering the same key twice overwrites the previous entry.
func (r *Registry) Register(key string, streamID domain.StreamCode, buf *buffer.Service, bufferWriteID domain.StreamCode) {
	if bufferWriteID == "" {
		bufferWriteID = streamID
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[key] = registryEntry{
		streamID:      streamID,
		bufferWriteID: bufferWriteID,
		buf:           buf,
	}
}

// Unregister removes a key from the registry.
func (r *Registry) Unregister(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, key)
}

// Lookup returns the buffer write target, logical stream id, and buffer service for key.
func (r *Registry) Lookup(key string) (bufferWriteID domain.StreamCode, streamID domain.StreamCode, buf *buffer.Service, err error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[key]
	if !ok {
		return "", "", nil, fmt.Errorf("ingestor: no stream registered for key %q", key)
	}
	return e.bufferWriteID, e.streamID, e.buf, nil
}
