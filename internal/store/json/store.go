// Package json provides a JSON file-based implementation of the store repositories.
// Intended for development and lightweight single-node deployments.
// Writes are atomic: data is written to a .tmp file then renamed into place.
package json

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/ntthuan060102github/open-streamer/internal/domain"
	"github.com/ntthuan060102github/open-streamer/internal/store"
)

// Store is a JSON-backed implementation of all repositories.
type Store struct {
	dir string
	mu  sync.RWMutex
}

// New creates a Store that persists data under dir.
// The directory is created if it does not exist.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("json store: mkdir %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// Streams returns a StreamRepository backed by this Store.
func (s *Store) Streams() store.StreamRepository { return &streamRepo{s} }

// Recordings returns a RecordingRepository backed by this Store.
func (s *Store) Recordings() store.RecordingRepository { return &recordingRepo{s} }

// Hooks returns a HookRepository backed by this Store.
func (s *Store) Hooks() store.HookRepository { return &hookRepo{s} }

// --- helpers ---

// readFile reads and unmarshals a JSON file. Caller must hold at least s.mu.RLock.
func (s *Store) readFile(name string, dst any) error {
	data, err := os.ReadFile(filepath.Join(s.dir, name))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("json store: read %s: %w", name, err)
	}
	return json.Unmarshal(data, dst)
}

// writeFile marshals and atomically writes a JSON file. Caller must hold s.mu.Lock.
func (s *Store) writeFile(name string, src any) error {
	data, err := json.MarshalIndent(src, "", "  ")
	if err != nil {
		return fmt.Errorf("json store: marshal %s: %w", name, err)
	}

	tmp := filepath.Join(s.dir, name+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("json store: write tmp %s: %w", name, err)
	}

	if err := os.Rename(tmp, filepath.Join(s.dir, name)); err != nil {
		return fmt.Errorf("json store: rename %s: %w", name, err)
	}
	return nil
}

// readAll reads a JSON file under a read lock.
func (s *Store) readAll(name string, dst any) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.readFile(name, dst)
}

// modify performs an atomic read-modify-write under a single write lock.
func (s *Store) modify(name string, dst any, fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readFile(name, dst); err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	return s.writeFile(name, dst)
}

// --- StreamRepository ---

type streamRepo struct{ s *Store }

// Save implements store.StreamRepository.
func (r *streamRepo) Save(_ context.Context, stream *domain.Stream) error {
	m := make(map[string]*domain.Stream)
	return r.s.modify("streams.json", &m, func() error {
		m[string(stream.Code)] = stream
		return nil
	})
}

// FindByCode implements store.StreamRepository.
func (r *streamRepo) FindByCode(_ context.Context, code domain.StreamCode) (*domain.Stream, error) {
	m := make(map[string]*domain.Stream)
	if err := r.s.readAll("streams.json", &m); err != nil {
		return nil, err
	}
	s, ok := m[string(code)]
	if !ok {
		return nil, fmt.Errorf("stream %s: %w", code, store.ErrNotFound)
	}
	return s, nil
}

// List implements store.StreamRepository.
func (r *streamRepo) List(_ context.Context, filter store.StreamFilter) ([]*domain.Stream, error) {
	m := make(map[string]*domain.Stream)
	if err := r.s.readAll("streams.json", &m); err != nil {
		return nil, err
	}
	result := make([]*domain.Stream, 0, len(m))
	for _, s := range m {
		if filter.Status != nil && s.Status != *filter.Status {
			continue
		}
		result = append(result, s)
	}
	return result, nil
}

// Delete implements store.StreamRepository.
func (r *streamRepo) Delete(_ context.Context, code domain.StreamCode) error {
	m := make(map[string]*domain.Stream)
	return r.s.modify("streams.json", &m, func() error {
		delete(m, string(code))
		return nil
	})
}

// --- RecordingRepository ---

type recordingRepo struct{ s *Store }

// Save implements store.RecordingRepository.
func (r *recordingRepo) Save(_ context.Context, rec *domain.Recording) error {
	m := make(map[string]*domain.Recording)
	return r.s.modify("recordings.json", &m, func() error {
		m[string(rec.ID)] = rec
		return nil
	})
}

// FindByID implements store.RecordingRepository.
func (r *recordingRepo) FindByID(_ context.Context, id domain.RecordingID) (*domain.Recording, error) {
	m := make(map[string]*domain.Recording)
	if err := r.s.readAll("recordings.json", &m); err != nil {
		return nil, err
	}
	rec, ok := m[string(id)]
	if !ok {
		return nil, fmt.Errorf("recording %s: %w", id, store.ErrNotFound)
	}
	return rec, nil
}

// ListByStream implements store.RecordingRepository.
func (r *recordingRepo) ListByStream(_ context.Context, streamCode domain.StreamCode) ([]*domain.Recording, error) {
	m := make(map[string]*domain.Recording)
	if err := r.s.readAll("recordings.json", &m); err != nil {
		return nil, err
	}
	result := make([]*domain.Recording, 0)
	for _, rec := range m {
		if rec.StreamCode == streamCode {
			result = append(result, rec)
		}
	}
	return result, nil
}

// Delete implements store.RecordingRepository.
func (r *recordingRepo) Delete(_ context.Context, id domain.RecordingID) error {
	m := make(map[string]*domain.Recording)
	return r.s.modify("recordings.json", &m, func() error {
		delete(m, string(id))
		return nil
	})
}

// --- HookRepository ---

type hookRepo struct{ s *Store }

// Save implements store.HookRepository.
func (r *hookRepo) Save(_ context.Context, hook *domain.Hook) error {
	m := make(map[string]*domain.Hook)
	return r.s.modify("hooks.json", &m, func() error {
		m[string(hook.ID)] = hook
		return nil
	})
}

// FindByID implements store.HookRepository.
func (r *hookRepo) FindByID(_ context.Context, id domain.HookID) (*domain.Hook, error) {
	m := make(map[string]*domain.Hook)
	if err := r.s.readAll("hooks.json", &m); err != nil {
		return nil, err
	}
	h, ok := m[string(id)]
	if !ok {
		return nil, fmt.Errorf("hook %s: %w", id, store.ErrNotFound)
	}
	return h, nil
}

// List implements store.HookRepository.
func (r *hookRepo) List(_ context.Context) ([]*domain.Hook, error) {
	m := make(map[string]*domain.Hook)
	if err := r.s.readAll("hooks.json", &m); err != nil {
		return nil, err
	}
	result := make([]*domain.Hook, 0, len(m))
	for _, h := range m {
		result = append(result, h)
	}
	return result, nil
}

// Delete implements store.HookRepository.
func (r *hookRepo) Delete(_ context.Context, id domain.HookID) error {
	m := make(map[string]*domain.Hook)
	return r.s.modify("hooks.json", &m, func() error {
		delete(m, string(id))
		return nil
	})
}
