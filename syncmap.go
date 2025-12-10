package cachex

import (
	"context"
	"sync"

	"github.com/pkg/errors"
)

// SyncMap is a cache implementation using sync.Map
type SyncMap[T any] struct {
	sync.Map
}

var _ Cache[any] = &SyncMap[any]{}

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
		return zero, errors.Wrapf(&ErrKeyNotFound{}, "key not found in syncmap for key: %s", key)
	}
	return v.(T), nil
}

func (s *SyncMap[T]) Del(_ context.Context, key string) error {
	s.Delete(key)
	return nil
}
