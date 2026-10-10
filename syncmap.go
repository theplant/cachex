package cachex

import (
	"context"
	"fmt"
	"sync"
)

// SyncMap is a cache implementation using sync.Map
type SyncMap[T any] struct {
	sync.Map
}

var _ BatchCache[any] = &SyncMap[any]{}

func NewSyncMap[T any]() *SyncMap[T] {
	return &SyncMap[T]{}
}

func (s *SyncMap[T]) Set(_ context.Context, key string, value T) error {
	s.Store(key, value)
	return nil
}

func (s *SyncMap[T]) Get(_ context.Context, key string) (T, error) {
	var zero T
	v, ok := s.Load(key)
	if !ok {
		return zero, fmt.Errorf("key not found in syncmap for key: %s: %w", key, &ErrKeyNotFound{})
	}
	return v.(T), nil
}

func (s *SyncMap[T]) Del(_ context.Context, key string) error {
	s.Delete(key)
	return nil
}

func (s *SyncMap[T]) GetMany(_ context.Context, keys []string) (map[string]T, error) {
	out := make(map[string]T, len(keys))
	for _, key := range keys {
		if v, ok := s.Load(key); ok {
			out[key] = v.(T)
		}
	}
	return out, nil
}

func (s *SyncMap[T]) SetMany(_ context.Context, values map[string]T) error {
	for key, value := range values {
		s.Store(key, value)
	}
	return nil
}

func (s *SyncMap[T]) DelMany(_ context.Context, keys []string) error {
	for _, key := range keys {
		s.Delete(key)
	}
	return nil
}
