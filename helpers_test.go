package cachex_test

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// source is a Source over a map. calls counts Get calls; a key in fail
// returns that error; gate, if set, is waited on by every call.
type source struct {
	mu    sync.Mutex
	data  map[string]string
	fail  map[string]error
	calls atomic.Int64
	gate  chan struct{}
	onGet func(ctx context.Context, key string)
}

func newSource(data map[string]string) *source {
	if data == nil {
		data = map[string]string{}
	}
	return &source{data: maps.Clone(data), fail: map[string]error{}}
}

func (s *source) Get(ctx context.Context, key string) (string, error) {
	s.calls.Add(1)
	if s.onGet != nil {
		s.onGet(ctx, key)
	}
	if s.gate != nil {
		select {
		case <-s.gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail[key]; err != nil {
		return "", err
	}
	v, ok := s.data[key]
	if !ok {
		return "", cachex.ErrNotFound
	}
	return v, nil
}

func (s *source) set(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
}

func (s *source) del(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
}

// batchSource is a source that also answers many keys in one call; batches
// records the keys of every GetMany call.
type batchSource struct {
	*source
	mu      sync.Mutex
	batches [][]string
	failAll error
}

func newBatchSource(data map[string]string) *batchSource {
	return &batchSource{source: newSource(data)}
}

func (s *batchSource) GetMany(ctx context.Context, keys []string) (map[string]string, error) {
	s.mu.Lock()
	s.batches = append(s.batches, append([]string(nil), keys...))
	failAll := s.failAll
	s.mu.Unlock()
	if s.onGet != nil {
		for _, key := range keys {
			s.onGet(ctx, key)
		}
	}
	if s.gate != nil {
		select {
		case <-s.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if failAll != nil {
		return nil, failAll
	}
	s.source.mu.Lock()
	defer s.source.mu.Unlock()
	out := map[string]string{}
	errs := map[string]error{}
	for _, key := range keys {
		if err := s.fail[key]; err != nil {
			errs[key] = err
		} else if v, ok := s.data[key]; ok {
			out[key] = v
		}
	}
	if len(errs) > 0 {
		return out, &cachex.BatchError{Errors: errs}
	}
	return out, nil
}

func (s *batchSource) batchCalls() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string(nil), s.batches...)
}

// hooked wraps a Backend: hook runs before every call, with its operation
// ("get", "set" or "del"), its keys, and whether it is a Many call; an error
// from hook fails the call. Single and Many calls share the hook, so a test
// does not depend on which of them the Cache makes.
type hooked[T any] struct {
	cachex.Backend[T]
	hook func(op string, keys []string, many bool) error
}

func (h *hooked[T]) Get(ctx context.Context, key string) (cachex.Entry[T], bool, error) {
	if err := h.hook("get", []string{key}, false); err != nil {
		return cachex.Entry[T]{}, false, err
	}
	return h.Backend.Get(ctx, key)
}

func (h *hooked[T]) GetMany(ctx context.Context, keys []string) (map[string]cachex.Entry[T], error) {
	if err := h.hook("get", keys, true); err != nil {
		return nil, err
	}
	return h.Backend.GetMany(ctx, keys)
}

func (h *hooked[T]) Set(ctx context.Context, key string, e cachex.Entry[T]) error {
	if err := h.hook("set", []string{key}, false); err != nil {
		return err
	}
	return h.Backend.Set(ctx, key, e)
}

func (h *hooked[T]) SetMany(ctx context.Context, entries map[string]cachex.Entry[T]) error {
	if err := h.hook("set", slices.Collect(maps.Keys(entries)), true); err != nil {
		return err
	}
	return h.Backend.SetMany(ctx, entries)
}

func (h *hooked[T]) Del(ctx context.Context, key string) error {
	if err := h.hook("del", []string{key}, false); err != nil {
		return err
	}
	return h.Backend.Del(ctx, key)
}

func (h *hooked[T]) DelMany(ctx context.Context, keys []string) error {
	if err := h.hook("del", keys, true); err != nil {
		return err
	}
	return h.Backend.DelMany(ctx, keys)
}

// hook wraps b with f, for hooks that never fail a call.
func hook[T any](b cachex.Backend[T], f func(op string, keys []string, many bool)) *hooked[T] {
	return &hooked[T]{Backend: b, hook: func(op string, keys []string, many bool) error {
		f(op, keys, many)
		return nil
	}}
}

// faulty wraps a Backend: an error set for an operation fails it.
type faulty[T any] struct {
	*hooked[T]
	mu   sync.Mutex
	errs map[string]error // by operation: "get", "set", "del"
}

func newFaulty[T any](b cachex.Backend[T]) *faulty[T] {
	f := &faulty[T]{errs: map[string]error{}}
	f.hooked = &hooked[T]{Backend: b, hook: func(op string, _ []string, _ bool) error { return f.err(op) }}
	return f
}

func (f *faulty[T]) failOn(op string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[op] = err
}

func (f *faulty[T]) err(op string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.errs[op]
}

// blocker holds writes: a write of key (of any key if key is empty) signals
// entered, then waits for release.
type blocker struct {
	key     string
	entered chan struct{}
	release chan struct{}
}

func newBlocker(key string) *blocker {
	return &blocker{key: key, entered: make(chan struct{}, 1), release: make(chan struct{})}
}

// wrap returns b with its writes held.
func (bl *blocker) wrap(b cachex.Backend[string]) cachex.Backend[string] {
	return hook(b, func(op string, keys []string, _ bool) {
		if op == "set" && (bl.key == "" || slices.Contains(keys, bl.key)) {
			bl.entered <- struct{}{}
			<-bl.release
		}
	})
}

// counting wraps b and counts its calls of op.
func counting(b cachex.Backend[string], op string, n *atomic.Int64) cachex.Backend[string] {
	return hook(b, func(o string, _ []string, _ bool) {
		if o == op {
			n.Add(1)
		}
	})
}

var errBoom = errors.New("boom")

// stored returns the entry a backend holds for key.
func stored[T any](t *testing.T, b cachex.Backend[T], key string) (cachex.Entry[T], bool) {
	t.Helper()
	e, ok, err := b.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("backend get %q: %v", key, err)
	}
	return e, ok
}

// newClock returns a clock at epoch and the option that makes a Cache use it.
func newClock() (*cachextest.Clock, cachex.Option) {
	c := cachextest.NewClock(epoch)
	return c, cachex.WithNow(c.Now)
}
