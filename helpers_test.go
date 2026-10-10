package cachex_test

import (
	"context"
	"errors"
	"maps"
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

// faulty wraps a Backend: an error set for an operation fails it.
type faulty[T any] struct {
	cachex.Backend[T]
	mu   sync.Mutex
	errs map[string]error // by operation: "get", "set", "del"
}

func newFaulty[T any](b cachex.Backend[T]) *faulty[T] {
	return &faulty[T]{Backend: b, errs: map[string]error{}}
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

func (f *faulty[T]) Get(ctx context.Context, key string) (cachex.Entry[T], bool, error) {
	if err := f.err("get"); err != nil {
		return cachex.Entry[T]{}, false, err
	}
	return f.Backend.Get(ctx, key)
}

func (f *faulty[T]) GetMany(ctx context.Context, keys []string) (map[string]cachex.Entry[T], error) {
	if err := f.err("get"); err != nil {
		return nil, err
	}
	return f.Backend.GetMany(ctx, keys)
}

func (f *faulty[T]) Set(ctx context.Context, key string, e cachex.Entry[T]) error {
	if err := f.err("set"); err != nil {
		return err
	}
	return f.Backend.Set(ctx, key, e)
}

func (f *faulty[T]) SetMany(ctx context.Context, entries map[string]cachex.Entry[T]) error {
	if err := f.err("set"); err != nil {
		return err
	}
	return f.Backend.SetMany(ctx, entries)
}

func (f *faulty[T]) Del(ctx context.Context, key string) error {
	if err := f.err("del"); err != nil {
		return err
	}
	return f.Backend.Del(ctx, key)
}

func (f *faulty[T]) DelMany(ctx context.Context, keys []string) error {
	if err := f.err("del"); err != nil {
		return err
	}
	return f.Backend.DelMany(ctx, keys)
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
