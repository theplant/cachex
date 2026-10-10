package cachex_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

func TestGetMany(t *testing.T) {
	ctx := context.Background()

	t.Run("hits, misses and absent keys in one result", func(t *testing.T) {
		src := newBatchSource(map[string]string{"a": "1", "b": "2", "c": "3"})
		mem := cachextest.NewMap[string]()
		keysRead := &keysOfGetMany{Backend: mem}
		c := cachex.New[string](src, oneLayer(keysRead))
		_, err := c.Get(ctx, "a")
		require.NoError(t, err)

		m, err := c.GetMany(ctx, []string{"a", "b", "c", "x", "b"})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"a": "1", "b": "2", "c": "3"}, m, "x does not exist: absent, not an error")
		require.Len(t, src.batchCalls(), 1)
		assert.ElementsMatch(t, []string{"b", "c", "x"}, src.batchCalls()[0], "one call for the misses, each once")
		assert.Equal(t, 3, mem.Len())
		assert.Equal(t, []string{"a", "b", "c", "x"}, keysRead.first(), "the layer is asked each key once")
	})

	t.Run("no keys", func(t *testing.T) {
		c := cachex.New[string](newBatchSource(nil), oneLayer(cachextest.NewMap[string]()))
		m, err := c.GetMany(ctx, nil)
		require.NoError(t, err)
		assert.Empty(t, m)
	})

	t.Run("a cached nil is present", func(t *testing.T) {
		src := cachex.SourceFunc[*string](func(context.Context, string) (*string, error) { return nil, nil })
		c := cachex.New(src, []cachex.Layer[*string]{cachex.NewLayer(cachextest.NewMap[*string](), cachex.TTL(time.Minute, 0))})
		for range 2 {
			m, err := c.GetMany(ctx, []string{"a"})
			require.NoError(t, err)
			v, ok := m["a"]
			assert.True(t, ok)
			assert.Nil(t, v)
		}
	})

	t.Run("keys the source failed are listed, the rest returned", func(t *testing.T) {
		src := newBatchSource(map[string]string{"a": "1", "b": "2"})
		src.fail["b"] = errBoom
		c := cachex.New[string](src, oneLayer(cachextest.NewMap[string]()))
		m, err := c.GetMany(ctx, []string{"a", "b", "x"})
		assert.Equal(t, map[string]string{"a": "1"}, m)
		var be *cachex.BatchError
		require.ErrorAs(t, err, &be)
		assert.Len(t, be.Errors, 1)
		assert.ErrorIs(t, be.Errors["b"], errBoom)
	})

	t.Run("a whole failed call fails every key", func(t *testing.T) {
		src := newBatchSource(map[string]string{"a": "1"})
		src.failAll = errBoom
		c := cachex.New[string](src, oneLayer(cachextest.NewMap[string]()))
		m, err := c.GetMany(ctx, []string{"a", "x"})
		assert.Empty(t, m)
		var be *cachex.BatchError
		require.ErrorAs(t, err, &be)
		assert.Len(t, be.Errors, 2)
		assert.ErrorIs(t, be.Errors["x"], errBoom)
	})

	t.Run("a wrapped BatchError fails the whole call", func(t *testing.T) {
		src := newBatchSource(map[string]string{"a": "1"})
		src.failAll = fmt.Errorf("wrapped: %w", &cachex.BatchError{Errors: map[string]error{"x": errBoom}})
		c := cachex.New[string](src, oneLayer(cachextest.NewMap[string]()))
		_, err := c.GetMany(ctx, []string{"a", "x"})
		var be *cachex.BatchError
		require.ErrorAs(t, err, &be)
		assert.Len(t, be.Errors, 2)
	})

	t.Run("a first layer that cannot be read fails its keys", func(t *testing.T) {
		mem := newFaulty[string](cachextest.NewMap[string]())
		mem.failOn("get", errBoom)
		src := newBatchSource(map[string]string{"a": "1"})
		c := cachex.New[string](src, oneLayer(mem))
		_, err := c.GetMany(ctx, []string{"a"})
		var be *cachex.BatchError
		require.ErrorAs(t, err, &be)
		assert.ErrorIs(t, be.Errors["a"], errBoom)
		assert.Empty(t, src.batchCalls())
	})

	t.Run("lower layers are read in one call each", func(t *testing.T) {
		src := newBatchSource(map[string]string{"a": "1", "b": "2", "c": "3"})
		var reads atomic.Int64
		l2 := &countingGets{Backend: cachextest.NewMap[string](), n: &reads}
		c := cachex.New[string](src, []cachex.Layer[string]{
			cachex.NewLayer(cachextest.NewMap[string](), cachex.TTL(time.Minute, 0)),
			cachex.NewLayer(l2, cachex.TTL(time.Hour, 0)),
		})
		require.NoError(t, l2.Set(ctx, "a", cachex.Entry[string]{Value: "below", FreshUntil: time.Now().Add(time.Hour), ExpiresAt: time.Now().Add(time.Hour)}))
		m, err := c.GetMany(ctx, []string{"a", "b", "c"})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"a": "below", "b": "2", "c": "3"}, m)
		assert.EqualValues(t, 1, reads.Load())
		require.Len(t, src.batchCalls(), 1)
		assert.ElementsMatch(t, []string{"b", "c"}, src.batchCalls()[0])
	})

	t.Run("stale keys are served and refreshed in one call", func(t *testing.T) {
		src := newBatchSource(map[string]string{"a": "1", "b": "2"})
		clock, now := newClock()
		c := cachex.New[string](src, []cachex.Layer[string]{
			cachex.NewLayer(cachextest.NewMap[string](), cachex.TTL(time.Minute, time.Hour)),
		}, now)
		_, err := c.GetMany(ctx, []string{"a", "b"})
		require.NoError(t, err)
		src.set("a", "10")
		clock.Advance(2 * time.Minute)
		m, err := c.GetMany(ctx, []string{"a", "b"})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"a": "1", "b": "2"}, m)
		require.NoError(t, c.Close())
		calls := src.batchCalls()
		require.Len(t, calls, 2)
		assert.ElementsMatch(t, []string{"a", "b"}, calls[1])
	})

	t.Run("a source without GetMany is asked key by key, a few at a time", func(t *testing.T) {
		gate := make(chan struct{})
		var running, peak atomic.Int64
		src := cachex.SourceFunc[string](func(_ context.Context, key string) (string, error) {
			n := running.Add(1)
			defer running.Add(-1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			<-gate
			if key == "x" {
				return "", cachex.ErrNotFound
			}
			return "v" + key, nil
		})
		c := cachex.New(src, oneLayer(cachextest.NewMap[string]()), cachex.WithGetManyConcurrency(2))
		done := make(chan map[string]string)
		go func() {
			m, err := c.GetMany(ctx, []string{"a", "b", "c", "d", "x"})
			assert.NoError(t, err)
			done <- m
		}()
		require.Eventually(t, func() bool { return running.Load() == 2 }, time.Second, time.Millisecond)
		close(gate)
		assert.Equal(t, map[string]string{"a": "va", "b": "vb", "c": "vc", "d": "vd"}, <-done)
		assert.EqualValues(t, 2, peak.Load())
	})

	t.Run("chunks are answered as each returns", func(t *testing.T) {
		src := newBatchSource(map[string]string{"a": "1", "b": "2"})
		release := map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{})}
		var mu sync.Mutex
		src.onGet = func(ctx context.Context, key string) {
			mu.Lock()
			ch := release[key]
			mu.Unlock()
			<-ch
		}
		c := cachex.New[string](src, oneLayer(cachextest.NewMap[string]()), cachex.WithGetManyChunkSize(1))
		go func() { _, _ = c.GetMany(ctx, []string{"a", "b"}) }()
		require.Eventually(t, func() bool { return len(src.batchCalls()) == 2 }, time.Second, time.Millisecond, "chunks run concurrently")
		close(release["a"])
		v, err := c.Get(ctx, "a") // joins chunk a, which no longer waits for chunk b
		require.NoError(t, err)
		assert.Equal(t, "1", v)
		close(release["b"])
	})

	t.Run("a context that ends fails the keys still waited for", func(t *testing.T) {
		src := newBatchSource(map[string]string{"a": "1"})
		src.gate = make(chan struct{})
		defer close(src.gate)
		c := cachex.New[string](src, oneLayer(cachextest.NewMap[string]()))
		cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		_, err := c.GetMany(cctx, []string{"a"})
		var be *cachex.BatchError
		require.ErrorAs(t, err, &be)
		assert.True(t, errors.Is(be.Errors["a"], context.DeadlineExceeded))
	})

	t.Run("a batch source answers a single Get with Get", func(t *testing.T) {
		src := newBatchSource(map[string]string{"a": "1"})
		c := cachex.New[string](src, oneLayer(cachextest.NewMap[string]()))
		_, err := c.Get(ctx, "a")
		require.NoError(t, err)
		assert.Empty(t, src.batchCalls())
		assert.EqualValues(t, 1, src.calls.Load())
	})
}

func TestBatchErrorMessage(t *testing.T) {
	err := &cachex.BatchError{Errors: map[string]error{"b": errBoom, "a": errBoom, "c": errBoom, "d": errBoom}}
	assert.Equal(t, "cachex: 4 keys failed; a: boom; b: boom; c: boom; and 1 more", err.Error())
}

// keysOfGetMany wraps a backend and records the keys of its GetMany calls.
type keysOfGetMany struct {
	cachex.Backend[string]
	mu    sync.Mutex
	calls [][]string
}

func (k *keysOfGetMany) GetMany(ctx context.Context, keys []string) (map[string]cachex.Entry[string], error) {
	k.mu.Lock()
	k.calls = append(k.calls, append([]string(nil), keys...))
	k.mu.Unlock()
	return k.Backend.GetMany(ctx, keys)
}

func (k *keysOfGetMany) first() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.calls) == 0 {
		return nil
	}
	return k.calls[0]
}
