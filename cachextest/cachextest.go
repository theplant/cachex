// Package cachextest has helpers for testing code that uses cachex: an
// in-memory Backend, a manual clock, and a contract test for Backend
// implementations.
package cachextest

import (
	"context"
	"sync"
	"time"

	"github.com/theplant/cachex/v2"
)

// Map is an unbounded in-memory Backend for tests. It never drops entries,
// not even expired ones (the Cache does not serve those), so it is not meant
// for production.
type Map[T any] struct {
	mu      sync.RWMutex
	entries map[string]cachex.Entry[T]
}

var _ cachex.Backend[any] = (*Map[any])(nil)

// NewMap returns an empty Map.
func NewMap[T any]() *Map[T] { return &Map[T]{entries: map[string]cachex.Entry[T]{}} }

func (m *Map[T]) Get(_ context.Context, key string) (cachex.Entry[T], bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.entries[key]
	return e, ok, nil
}

func (m *Map[T]) GetMany(_ context.Context, keys []string) (map[string]cachex.Entry[T], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]cachex.Entry[T], len(keys))
	for _, key := range keys {
		if e, ok := m.entries[key]; ok {
			out[key] = e
		}
	}
	return out, nil
}

func (m *Map[T]) Set(_ context.Context, key string, e cachex.Entry[T]) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[key] = e
	return nil
}

func (m *Map[T]) SetMany(_ context.Context, entries map[string]cachex.Entry[T]) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, e := range entries {
		m.entries[key] = e
	}
	return nil
}

func (m *Map[T]) Del(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, key)
	return nil
}

func (m *Map[T]) DelMany(_ context.Context, keys []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, key := range keys {
		delete(m.entries, key)
	}
	return nil
}

// Len is the number of stored entries, expired ones included.
func (m *Map[T]) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.entries)
}

// Clock is a manual clock: pass its Now to cachex.WithNow.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a Clock reading start.
func NewClock(start time.Time) *Clock { return &Clock{now: start} }

// Now returns the clock's time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
