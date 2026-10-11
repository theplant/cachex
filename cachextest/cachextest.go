// Package cachextest has helpers for testing code that uses cachex: an
// in-memory Backend, a manual clock, and the contract tests for Backend and
// ByteBackend implementations (TestBackend, TestByteBackend).
package cachextest

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/theplant/cachex/v2"
)

// Map is an unbounded in-memory Backend for tests. It never drops entries,
// not even expired ones (the Cache does not serve those), so it is not meant
// for production.
type Map[T any] struct {
	entries sync.Map // string -> *cachex.Entry[T]: a pointer, so a read copies the entry once
	leases  *leases  // nil: leases are off
}

var (
	_ cachex.Backend[any] = (*Map[any])(nil)
	_ cachex.Leasing[any] = (*Map[any])(nil)
)

// NewMap returns an empty Map.
func NewMap[T any]() *Map[T] { return &Map[T]{} }

// NewLeasingMap returns an empty Map whose leases are on (see cachex.Leaser),
// to stand for a layer that several instances share: build each instance's
// Cache over the same Map.
func NewLeasingMap[T any]() *Map[T] { return &Map[T]{leases: &leases{held: map[string]heldLease{}}} }

type leases struct {
	mu   sync.Mutex // held by every write too, so that a write and PutLeased do not interleave
	held map[string]heldLease
	n    int
}

type heldLease struct {
	token string
	until time.Time
}

// Leaser returns the Map's Leaser, nil unless it was built by NewLeasingMap.
func (m *Map[T]) Leaser() cachex.Leaser[T] {
	if m.leases == nil {
		return nil
	}
	return mapLeaser[T]{m}
}

// lock locks the leases, if they are on; unlock unlocks them.
func (m *Map[T]) lock() {
	if m.leases != nil {
		m.leases.mu.Lock()
	}
}

func (m *Map[T]) unlock() {
	if m.leases != nil {
		m.leases.mu.Unlock()
	}
}

// end ends key's lease; the leases are locked, or off.
func (m *Map[T]) end(key string) {
	if m.leases != nil {
		delete(m.leases.held, key)
	}
}

type mapLeaser[T any] struct{ m *Map[T] }

func (l mapLeaser[T]) Lease(_ context.Context, keys []string, ttl time.Duration) (map[string]string, error) {
	ls := l.m.leases
	ls.mu.Lock()
	defer ls.mu.Unlock()
	now := time.Now()
	out := map[string]string{}
	for _, key := range keys {
		if h, ok := ls.held[key]; ok && now.Before(h.until) {
			continue
		}
		ls.n++
		token := strconv.Itoa(ls.n)
		ls.held[key] = heldLease{token: token, until: now.Add(ttl)}
		out[key] = token
	}
	return out, nil
}

// holds reports whether key's lease holds token; the leases are locked.
func (l mapLeaser[T]) holds(key, token string) bool {
	h, ok := l.m.leases.held[key]
	return ok && h.token == token && time.Now().Before(h.until)
}

func (l mapLeaser[T]) PutLeased(_ context.Context, tokens map[string]string, entries map[string]cachex.Entry[T]) error {
	l.m.lock()
	defer l.m.unlock()
	for key, token := range tokens {
		if !l.holds(key, token) {
			continue
		}
		if e, ok := entries[key]; ok {
			l.m.entries.Store(key, &e)
		} else {
			l.m.entries.Delete(key)
		}
		l.m.end(key)
	}
	return nil
}

func (l mapLeaser[T]) Release(_ context.Context, tokens map[string]string) error {
	l.m.lock()
	defer l.m.unlock()
	for key, token := range tokens {
		if l.holds(key, token) {
			l.m.end(key)
		}
	}
	return nil
}

func (m *Map[T]) Get(_ context.Context, key string) (cachex.Entry[T], bool, error) {
	e, ok := m.entries.Load(key)
	if !ok {
		return cachex.Entry[T]{}, false, nil
	}
	return *e.(*cachex.Entry[T]), true, nil
}

func (m *Map[T]) GetMany(ctx context.Context, keys []string) (map[string]cachex.Entry[T], error) {
	out := make(map[string]cachex.Entry[T], len(keys))
	for _, key := range keys {
		if e, ok, _ := m.Get(ctx, key); ok {
			out[key] = e
		}
	}
	return out, nil
}

func (m *Map[T]) Set(_ context.Context, key string, e cachex.Entry[T]) error {
	m.lock()
	defer m.unlock()
	m.entries.Store(key, &e)
	m.end(key)
	return nil
}

func (m *Map[T]) SetMany(_ context.Context, entries map[string]cachex.Entry[T]) error {
	m.lock()
	defer m.unlock()
	for key, e := range entries {
		m.entries.Store(key, &e)
		m.end(key)
	}
	return nil
}

func (m *Map[T]) Del(_ context.Context, key string) error {
	m.lock()
	defer m.unlock()
	m.entries.Delete(key)
	m.end(key)
	return nil
}

func (m *Map[T]) DelMany(_ context.Context, keys []string) error {
	m.lock()
	defer m.unlock()
	for _, key := range keys {
		m.entries.Delete(key)
		m.end(key)
	}
	return nil
}

// Len is the number of stored entries, expired ones included.
func (m *Map[T]) Len() int {
	n := 0
	m.entries.Range(func(any, any) bool { n++; return true })
	return n
}

// Clock is a manual clock: pass its Now to cachex.WithNow. With a backend
// that expires entries by the wall clock, start it at time.Now() (see
// cachex.WithNow).
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
