package storage

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemoryStore is a concurrency-safe in-memory ObjectStore for tests and local
// dry runs. It intentionally returns copies so callers cannot mutate stored
// bytes after a successful Put.
type MemoryStore struct {
	mu      sync.RWMutex
	objects map[string]memoryObject
}

type memoryObject struct {
	data []byte
	when time.Time
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{objects: make(map[string]memoryObject)}
}

func (s *MemoryStore) Put(ctx context.Context, key string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	copyData := append([]byte(nil), data...)
	s.objects[key] = memoryObject{data: copyData, when: time.Now().UTC()}
	return nil
}

func (s *MemoryStore) Get(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	obj, ok := s.objects[key]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), obj.data...), nil
}

func (s *MemoryStore) List(ctx context.Context, prefix string) ([]Object, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.objects))
	for key := range s.objects {
		if len(prefix) == 0 || hasPrefixKey(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	objects := make([]Object, 0, len(keys))
	for _, key := range keys {
		obj := s.objects[key]
		objects = append(objects, Object{Key: key, Size: int64(len(obj.data)), LastModified: obj.when})
	}
	return objects, nil
}

func (s *MemoryStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	return nil
}

func hasPrefixKey(key, prefix string) bool {
	if len(key) < len(prefix) || key[:len(prefix)] != prefix {
		return false
	}
	return len(key) == len(prefix) || prefix[len(prefix)-1] == '/' || key[len(prefix)] == '/'
}

var _ ObjectStore = (*MemoryStore)(nil)
