package cachex_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
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
		var mu sync.Mutex
		var batches [][]string
		keysRead := hook(mem, func(op string, keys []string, many bool) {
			if op == "get" && many {
				mu.Lock()
				defer mu.Unlock()
				batches = append(batches, keys)
			}
		})
		c := cachex.New[string](src, oneLayer(keysRead))
		_, err := c.Get(ctx, "a")
		require.NoError(t, err)

		m, err := c.GetMany(ctx, []string{"a", "b", "c", "x", "b"})
		require.NoError(t, err)
		cachex.Settle(c)
		assert.Equal(t, map[string]string{"a": "1", "b": "2", "c": "3"}, m, "x does not exist: absent, not an error")
		require.Len(t, src.batchCalls(), 1)
		assert.ElementsMatch(t, []string{"b", "c", "x"}, src.batchCalls()[0], "one call for the misses, each once")
		assert.Equal(t, 3, mem.Len())
		assert.Equal(t, []string{"a", "b", "c", "x"}, batches[0], "the layer is asked each key once")
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

	t.Run("a batch source's ErrNotFound reads as not found, and is not cached", func(t *testing.T) {
		for name, fail := range map[string]func(*batchSource){
			"whole call": func(s *batchSource) { s.failAll = fmt.Errorf("no rows: %w", cachex.ErrNotFound) },
			"per key":    func(s *batchSource) { s.fail["a"], s.fail["x"] = cachex.ErrNotFound, cachex.ErrNotFound },
		} {
			t.Run(name, func(t *testing.T) {
				src := newBatchSource(map[string]string{"a": "1"})
				fail(src)
				mem := cachextest.NewMap[string]()
				c := cachex.New[string](src, []cachex.Layer[string]{
					cachex.NewLayer[string](mem, cachex.TTL(time.Minute, 0), cachex.NotFoundTTL(time.Minute, 0)),
				})
				for range 2 {
					m, err := c.GetMany(ctx, []string{"a", "x"})
					require.NoError(t, err)
					assert.Empty(t, m)
					cachex.Settle(c)
				}
				assert.Equal(t, 0, mem.Len(), "nothing is cached")
				assert.Len(t, src.batchCalls(), 2, "so each read asks again")
			})
		}
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
		m2 := cachextest.NewMap[string]()
		l2 := counting(m2, "get", &reads)
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
		cachex.Settle(c)
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
		var entered atomic.Int64
		src.onGet = func(ctx context.Context, key string) {
			mu.Lock()
			ch := release[key]
			mu.Unlock()
			entered.Add(1)
			<-ch
		}
		c := cachex.New[string](src, oneLayer(cachextest.NewMap[string]()), cachex.WithGetManyChunkSize(1))
		go func() { _, _ = c.GetMany(ctx, []string{"a", "b"}) }()
		require.Eventually(t, func() bool { return entered.Load() == 2 }, time.Second, time.Millisecond, "chunks run concurrently")
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

func TestGetManyInChunks(t *testing.T) {
	ctx := context.Background()
	values := map[string]string{"k1": "1", "k2": "2", "k3": "3", "k4": "4", "k5": "5", "k6": "6"}
	keys := []string{"k1", "k2", "k3", "k4", "k5", "k6"}

	t.Run("a key of a chunk not started yet is not claimed: a Get of it does not wait", func(t *testing.T) {
		src := newSource(values)
		first := make(chan struct{})
		src.onGet = func(_ context.Context, key string) {
			if key == "k1" || key == "k2" {
				<-first
			}
		}
		mem := cachextest.NewMap[string]()
		c := cachex.New(src, oneLayer(mem), cachex.WithGetManyChunkSize(2), cachex.WithGetManyConcurrency(2))
		done := make(chan map[string]string)
		go func() {
			m, err := c.GetMany(ctx, keys)
			assert.NoError(t, err)
			done <- m
		}()
		require.Eventually(t, func() bool { return src.calls.Load() == 2 }, time.Second, time.Millisecond, "the first chunk is being fetched")

		gctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		v, err := c.Get(gctx, "k6")
		require.NoError(t, err, "k6 is fetched at once, not after the GetMany's first chunks")
		assert.Equal(t, "6", v)
		require.Eventually(t, func() bool { _, ok := stored(t, mem, "k6"); return ok }, time.Second, time.Millisecond)

		close(first)
		assert.Equal(t, values, <-done)
		assert.EqualValues(t, 6, src.calls.Load(), "the GetMany takes k6 from the layer, not from the source again")
	})

	t.Run("each chunk reads each lower layer in one call", func(t *testing.T) {
		src := newBatchSource(values)
		var mu sync.Mutex
		var reads [][]string
		l2 := hook(cachextest.NewMap[string](), func(op string, keys []string, many bool) {
			if op == "get" && many {
				mu.Lock()
				defer mu.Unlock()
				reads = append(reads, slices.Clone(keys))
			}
		})
		c := cachex.New[string](src, []cachex.Layer[string]{
			cachex.NewLayer(cachextest.NewMap[string](), cachex.TTL(time.Minute, 0)),
			cachex.NewLayer[string](l2, cachex.TTL(time.Hour, 0)),
		}, cachex.WithGetManyChunkSize(2), cachex.WithGetManyConcurrency(1))
		m, err := c.GetMany(ctx, keys)
		require.NoError(t, err)
		assert.Equal(t, values, m)
		// within a chunk, a key whose stripe an earlier chunk's backfill wrote
		// is double-checked and comes after the others
		sorted := func(chunks [][]string) [][]string {
			for _, c := range chunks {
				slices.Sort(c)
			}
			return chunks
		}
		assert.Equal(t, [][]string{{"k1", "k2"}, {"k3", "k4"}, {"k5", "k6"}}, sorted(reads))
		assert.Equal(t, [][]string{{"k1", "k2"}, {"k3", "k4"}, {"k5", "k6"}}, sorted(src.batchCalls()))
	})

	t.Run("overlapping GetManys in chunks do not wait on each other", func(t *testing.T) {
		src := newSource(values)
		src.onGet = func(context.Context, string) { runtime.Gosched() }
		for range 50 {
			c := cachex.New(src, oneLayer(cachextest.NewMap[string]()), cachex.WithGetManyChunkSize(1), cachex.WithGetManyConcurrency(1))
			reversed := slices.Clone(keys)
			slices.Reverse(reversed)
			gctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			var wg sync.WaitGroup
			for _, ks := range [][]string{keys, reversed} {
				wg.Go(func() {
					m, err := c.GetMany(gctx, ks)
					assert.NoError(t, err)
					assert.Equal(t, values, m)
				})
			}
			wg.Wait()
			cancel()
		}
	})

	t.Run("a GetMany whose context ends starts no more chunks", func(t *testing.T) {
		src := newSource(values)
		first := make(chan struct{})
		src.onGet = func(_ context.Context, key string) {
			if key == "k1" {
				<-first
			}
		}
		c := cachex.New(src, oneLayer(cachextest.NewMap[string]()), cachex.WithGetManyChunkSize(1), cachex.WithGetManyConcurrency(1))
		gctx, cancel := context.WithCancel(ctx)
		done := make(chan error)
		go func() {
			_, err := c.GetMany(gctx, keys)
			done <- err
		}()
		require.Eventually(t, func() bool { return src.calls.Load() == 1 }, time.Second, time.Millisecond)
		cancel()
		var be *cachex.BatchError
		require.ErrorAs(t, <-done, &be)
		assert.Len(t, be.Errors, 6)
		close(first)
		cachex.Settle(c)
		assert.EqualValues(t, 1, src.calls.Load(), "nobody waits for the keys of the chunks not started")
	})

	t.Run("a refresh goes in chunks too", func(t *testing.T) {
		src := newBatchSource(values)
		clock, now := newClock()
		c := cachex.New[string](src, []cachex.Layer[string]{
			cachex.NewLayer(cachextest.NewMap[string](), cachex.TTL(time.Minute, time.Hour)),
		}, now, cachex.WithGetManyChunkSize(4))
		_, err := c.GetMany(ctx, keys)
		require.NoError(t, err)
		cachex.Settle(c)
		clock.Advance(2 * time.Minute) // stale
		_, err = c.GetMany(ctx, keys)
		require.NoError(t, err)
		require.NoError(t, c.Close())
		var sizes []int
		for _, call := range src.batchCalls() {
			sizes = append(sizes, len(call))
		}
		slices.Sort(sizes)
		assert.Equal(t, []int{2, 2, 4, 4}, sizes, "two chunks to fetch, two to refresh")
	})
}
