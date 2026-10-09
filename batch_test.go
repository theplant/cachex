package cachex

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/pkg/errors"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// testBatchCache checks the BatchCache contract against one implementation.
func testBatchCache(t *testing.T, cache BatchCache[string]) {
	t.Helper()
	ctx := context.Background()

	got, err := cache.GetMany(ctx, []string{"a", "b"})
	require.NoError(t, err)
	assert.Empty(t, got, "nothing written yet, nothing returned")

	require.NoError(t, cache.SetMany(ctx, map[string]string{"a": "1", "b": "2", "c": "3"}))
	require.NoError(t, cache.SetMany(ctx, map[string]string{}), "empty SetMany is a no-op")

	got, err = cache.GetMany(ctx, []string{"a", "b", "missing"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"a": "1", "b": "2"}, got, "missing keys are absent, not errors")

	v, err := cache.Get(ctx, "c")
	require.NoError(t, err)
	assert.Equal(t, "3", v, "SetMany is visible to Get")

	require.NoError(t, cache.SetMany(ctx, map[string]string{"a": "1b"}))
	got, err = cache.GetMany(ctx, []string{"a"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"a": "1b"}, got, "SetMany overwrites")

	require.NoError(t, cache.DelMany(ctx, []string{"a", "c", "missing"}))
	got, err = cache.GetMany(ctx, []string{"a", "b", "c"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"b": "2"}, got, "DelMany removes only the given keys")

	got, err = cache.GetMany(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, got)
	require.NoError(t, cache.DelMany(ctx, nil))
}

func TestBatchCacheImplementations(t *testing.T) {
	t.Run("SyncMap", func(t *testing.T) {
		testBatchCache(t, NewSyncMap[string]())
	})
	t.Run("Ristretto", func(t *testing.T) {
		testBatchCache(t, newRistrettoCache[string](t))
	})
	t.Run("Redis", func(t *testing.T) {
		cache, _ := newRedisCache[string](t)
		testBatchCache(t, cache)
	})
	t.Run("Redis with prefix", func(t *testing.T) {
		cache, mr := newRedisCache[string](t)
		cache.keyPrefix = "p:"
		testBatchCache(t, cache)
		assert.True(t, mr.Exists("p:b"), "keys are stored with the prefix")
	})
	t.Run("GORM", func(t *testing.T) {
		cache, _ := newGORMCache[string](t, "batch_cache")
		testBatchCache(t, cache)
	})
	t.Run("GORM with prefix", func(t *testing.T) {
		cache, db := newGORMCache[string](t, "batch_cache_prefixed")
		cache.keyPrefix = "p:"
		testBatchCache(t, cache)
		var n int64
		require.NoError(t, db.Table("batch_cache_prefixed").Where("key = ?", "p:b").Count(&n).Error)
		assert.Equal(t, int64(1), n, "keys are stored with the prefix")
	})
}

func TestRedisCacheGetManyStruct(t *testing.T) {
	ctx := context.Background()
	type item struct{ N int }
	cache, mr := newRedisCache[*item](t)

	require.NoError(t, cache.SetMany(ctx, map[string]*item{"a": {N: 1}}))
	require.NoError(t, mr.Set("bad", "not json"))

	got, err := cache.GetMany(ctx, []string{"a", "bad"})
	assert.Equal(t, map[string]*item{"a": {N: 1}}, got, "a corrupt entry does not hide the others")
	var be *BatchError
	require.True(t, errors.As(err, &be))
	assert.Contains(t, be.Errors, "bad")
	assert.NotContains(t, be.Errors, "a")
}

// batchUpstream is a BatchUpstream fake that records every call.
type batchUpstream struct {
	mu       sync.Mutex
	data     map[string]string
	getCalls [][]string // keys of each GetMany call, sorted
	oneCalls []string   // keys of each Get call
	block    func(keys []string)
	err      error
}

func (u *batchUpstream) GetMany(_ context.Context, keys []string) (map[string]string, error) {
	sorted := slices.Sorted(slices.Values(keys))
	u.mu.Lock()
	u.getCalls = append(u.getCalls, sorted)
	u.mu.Unlock()
	if u.block != nil {
		u.block(sorted)
	}
	if u.err != nil {
		return nil, u.err
	}
	out := map[string]string{}
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, k := range keys {
		if v, ok := u.data[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

func (u *batchUpstream) Get(_ context.Context, key string) (string, error) {
	u.mu.Lock()
	u.oneCalls = append(u.oneCalls, key)
	u.mu.Unlock()
	if u.block != nil {
		u.block([]string{key})
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if v, ok := u.data[key]; ok {
		return v, nil
	}
	return "", &ErrKeyNotFound{}
}

func (u *batchUpstream) calls() ([][]string, []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.getCalls), slices.Clone(u.oneCalls)
}

func TestClientGetMany(t *testing.T) {
	ctx := context.Background()

	t.Run("partial hit fetches only the misses, in one upstream call", func(t *testing.T) {
		backend := NewSyncMap[string]()
		require.NoError(t, backend.Set(ctx, "a", "cached-a"))
		up := &batchUpstream{data: map[string]string{"a": "up-a", "b": "up-b", "c": "up-c"}}
		cli := NewClient(backend, up)

		got, err := cli.GetMany(ctx, []string{"a", "b", "c", "missing", "b"})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"a": "cached-a", "b": "up-b", "c": "up-c"}, got,
			"cached value wins, fetched values are returned, missing keys are absent")

		batches, singles := up.calls()
		assert.Equal(t, [][]string{{"b", "c", "missing"}}, batches, "one upstream call with only the misses, deduplicated")
		assert.Empty(t, singles)

		v, err := backend.Get(ctx, "b")
		require.NoError(t, err)
		assert.Equal(t, "up-b", v, "fetched values are written back")
	})

	t.Run("full hit does not touch upstream", func(t *testing.T) {
		backend := NewSyncMap[string]()
		require.NoError(t, backend.SetMany(ctx, map[string]string{"a": "1", "b": "2"}))
		up := &batchUpstream{}
		cli := NewClient(backend, up)

		got, err := cli.GetMany(ctx, []string{"a", "b"})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"a": "1", "b": "2"}, got)
		batches, singles := up.calls()
		assert.Empty(t, batches)
		assert.Empty(t, singles)
	})

	t.Run("empty keys", func(t *testing.T) {
		cli := NewClient(NewSyncMap[string](), &batchUpstream{})
		got, err := cli.GetMany(ctx, nil)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("upstream without batch support is called key by key", func(t *testing.T) {
		var mu sync.Mutex
		var keys []string
		up := UpstreamFunc[string](func(_ context.Context, key string) (string, error) {
			mu.Lock()
			keys = append(keys, key)
			mu.Unlock()
			if key == "missing" {
				return "", &ErrKeyNotFound{}
			}
			return "v-" + key, nil
		})
		backend := NewSyncMap[string]()
		cli := NewClient(backend, up)

		got, err := cli.GetMany(ctx, []string{"a", "b", "missing"})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"a": "v-a", "b": "v-b"}, got)
		assert.ElementsMatch(t, []string{"a", "b", "missing"}, keys)
		v, err := backend.Get(ctx, "a")
		require.NoError(t, err)
		assert.Equal(t, "v-a", v)
	})

	t.Run("backend batch reads and writes are used", func(t *testing.T) {
		backend := &countingBatchCache{SyncMap: NewSyncMap[string]()}
		up := &batchUpstream{data: map[string]string{"a": "1", "b": "2"}}
		cli := NewClient(backend, up)

		_, err := cli.GetMany(ctx, []string{"a", "b"})
		require.NoError(t, err)
		assert.Equal(t, int32(1), backend.getMany.Load(), "one batch read of the backend")
		assert.Equal(t, int32(1), backend.setMany.Load(), "one batch write back")
		assert.Equal(t, int32(0), backend.gets.Load(), "no per-key reads")
	})
}

type countingBatchCache struct {
	*SyncMap[string]
	gets, getMany, setMany atomic.Int32
}

func (c *countingBatchCache) Get(ctx context.Context, key string) (string, error) {
	c.gets.Add(1)
	return c.SyncMap.Get(ctx, key)
}

func (c *countingBatchCache) GetMany(ctx context.Context, keys []string) (map[string]string, error) {
	c.getMany.Add(1)
	return c.SyncMap.GetMany(ctx, keys)
}

func (c *countingBatchCache) SetMany(ctx context.Context, values map[string]string) error {
	c.setMany.Add(1)
	return c.SyncMap.SetMany(ctx, values)
}

func TestClientGetManyErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("whole upstream failure fails every fetched key", func(t *testing.T) {
		backend := NewSyncMap[string]()
		require.NoError(t, backend.Set(ctx, "a", "1"))
		boom := errors.New("boom")
		cli := NewClient(backend, &batchUpstream{err: boom})

		got, err := cli.GetMany(ctx, []string{"a", "b", "c"})
		assert.Equal(t, map[string]string{"a": "1"}, got, "keys that did not need the upstream are still returned")
		var be *BatchError
		require.True(t, errors.As(err, &be))
		assert.Len(t, be.Errors, 2)
		assert.ErrorIs(t, be.Errors["b"], boom)
		assert.ErrorIs(t, err, boom, "errors.Is sees through BatchError")
	})

	t.Run("per-key upstream failure keeps the other keys", func(t *testing.T) {
		boom := errors.New("boom")
		up := UpstreamFunc[string](func(_ context.Context, key string) (string, error) {
			if key == "bad" {
				return "", boom
			}
			return "v-" + key, nil
		})
		cli := NewClient(NewSyncMap[string](), up)

		got, err := cli.GetMany(ctx, []string{"a", "bad"})
		assert.Equal(t, map[string]string{"a": "v-a"}, got)
		var be *BatchError
		require.True(t, errors.As(err, &be))
		assert.Equal(t, []string{"bad"}, slices.Collect(maps.Keys(be.Errors)))
	})

	t.Run("panic in batch upstream is recovered", func(t *testing.T) {
		up := &batchUpstream{block: func([]string) { panic("kaboom") }}
		cli := NewClient(NewSyncMap[string](), up)

		got, err := cli.GetMany(ctx, []string{"a", "b"})
		assert.Empty(t, got)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "panic during upstream fetch")
	})
}

func TestClientGetManyNotFoundCache(t *testing.T) {
	ctx := context.Background()
	clock := NewMockClock(time.Now())
	defer clock.Install()()

	notFound := NewSyncMap[time.Time]()
	up := &batchUpstream{data: map[string]string{"a": "1"}}
	cli := NewClient(NewSyncMap[string](), up,
		NotFoundWithTTL[string](notFound, time.Minute, 0))

	got, err := cli.GetMany(ctx, []string{"a", "missing"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"a": "1"}, got)
	_, err = notFound.Get(ctx, "missing")
	require.NoError(t, err, "not-found result is cached")

	got, err = cli.GetMany(ctx, []string{"a", "missing"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"a": "1"}, got)
	batches, _ := up.calls()
	assert.Len(t, batches, 1, "second call is answered by backend and not-found cache")

	_, err = cli.Get(ctx, "missing")
	var e *ErrKeyNotFound
	require.True(t, errors.As(err, &e))
	assert.True(t, e.Cached, "single Get sees the not-found entry GetMany wrote")

	clock.Advance(2 * time.Minute)
	_, err = cli.GetMany(ctx, []string{"missing"})
	require.NoError(t, err)
	batches, _ = up.calls()
	assert.Equal(t, []string{"missing"}, batches[len(batches)-1], "rotten not-found entry is refetched")
}

func TestClientGetManyServeStale(t *testing.T) {
	ctx := context.Background()
	clock := NewMockClock(time.Now())
	defer clock.Install()()

	var version atomic.Int32
	var mu sync.Mutex
	var calls [][]string
	refreshed := make(chan struct{}, 10)
	up := batchUpstreamFunc[*Entry[string]](func(_ context.Context, keys []string) (map[string]*Entry[string], error) {
		mu.Lock()
		calls = append(calls, slices.Sorted(slices.Values(keys)))
		mu.Unlock()
		v := version.Add(1)
		out := map[string]*Entry[string]{}
		for _, k := range keys {
			out[k] = &Entry[string]{Data: fmt.Sprintf("%s-v%d", k, v), CachedAt: NowFunc()}
		}
		refreshed <- struct{}{}
		return out, nil
	})
	backend := NewSyncMap[*Entry[string]]()
	cli := NewClient(backend, up,
		EntryWithTTL[string](time.Minute, time.Hour),
		WithServeStale[*Entry[string]](true))

	got, err := cli.GetMany(ctx, []string{"a", "b"})
	require.NoError(t, err)
	assert.Equal(t, "a-v1", got["a"].Data)
	<-refreshed

	clock.Advance(2 * time.Minute) // stale
	got, err = cli.GetMany(ctx, []string{"a", "b"})
	require.NoError(t, err)
	assert.Equal(t, "a-v1", got["a"].Data, "stale value is served immediately")
	assert.Equal(t, "b-v1", got["b"].Data)

	select {
	case <-refreshed:
	case <-time.After(time.Second):
		t.Fatal("background refresh did not happen")
	}
	require.Eventually(t, func() bool {
		v, err := backend.Get(ctx, "a")
		return err == nil && v.Data == "a-v2"
	}, time.Second, 5*time.Millisecond, "background refresh writes the new value")

	mu.Lock()
	assert.Equal(t, [][]string{{"a", "b"}, {"a", "b"}}, calls, "stale keys are refreshed in one batch")
	mu.Unlock()
}

type batchUpstreamFunc[T any] func(ctx context.Context, keys []string) (map[string]T, error)

func (f batchUpstreamFunc[T]) GetMany(ctx context.Context, keys []string) (map[string]T, error) {
	return f(ctx, keys)
}

func (f batchUpstreamFunc[T]) Get(ctx context.Context, key string) (T, error) {
	var zero T
	m, err := f(ctx, []string{key})
	if err != nil {
		return zero, err
	}
	v, ok := m[key]
	if !ok {
		return zero, &ErrKeyNotFound{}
	}
	return v, nil
}

func TestClientGetManySharesSingleflightWithGet(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	entered := make(chan struct{})
	up := &batchUpstream{data: map[string]string{"x": "vx", "y": "vy"}}
	up.block = func(keys []string) {
		if keys[0] == "x" && len(keys) == 1 {
			close(entered)
			<-release
		}
	}
	cli := NewClient(NewSyncMap[string](), up)

	var wg sync.WaitGroup
	var single string
	wg.Go(func() {
		var err error
		single, err = cli.Get(ctx, "x")
		assert.NoError(t, err)
	})
	waitFor(t, entered) // single Get owns the fetch of x

	var many map[string]string
	wg.Go(func() {
		var err error
		many, err = cli.GetMany(ctx, []string{"x", "y"})
		assert.NoError(t, err)
	})
	require.Eventually(t, func() bool {
		batches, _ := up.calls()
		return len(batches) == 1
	}, time.Second, time.Millisecond)
	close(release)
	wg.Wait()

	batches, singles := up.calls()
	assert.Equal(t, []string{"x"}, singles, "x is fetched once, by the single Get")
	assert.Equal(t, [][]string{{"y"}}, batches, "GetMany waits for the in-flight x and fetches only y")
	assert.Equal(t, "vx", single)
	assert.Equal(t, map[string]string{"x": "vx", "y": "vy"}, many)
}

func TestClientGetManyOverlappingBatches(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	entered := make(chan struct{})
	up := &batchUpstream{data: map[string]string{"x": "vx", "y": "vy", "z": "vz"}}
	up.block = func(keys []string) {
		if keys[0] == "x" {
			close(entered)
			<-release
		}
	}
	cli := NewClient(NewSyncMap[string](), up)

	var wg sync.WaitGroup
	var a, b map[string]string
	wg.Go(func() {
		var err error
		a, err = cli.GetMany(ctx, []string{"x", "y"})
		assert.NoError(t, err)
	})
	waitFor(t, entered)

	wg.Go(func() {
		var err error
		b, err = cli.GetMany(ctx, []string{"y", "z"})
		assert.NoError(t, err)
	})
	require.Eventually(t, func() bool {
		batches, _ := up.calls()
		return len(batches) == 2
	}, time.Second, time.Millisecond)
	close(release)
	wg.Wait()

	batches, _ := up.calls()
	assert.Equal(t, [][]string{{"x", "y"}, {"z"}}, batches, "the shared key y is fetched only once")
	assert.Equal(t, map[string]string{"x": "vx", "y": "vy"}, a)
	assert.Equal(t, map[string]string{"y": "vy", "z": "vz"}, b)
}

func TestClientGetManyDoubleCheck(t *testing.T) {
	ctx := context.Background()
	// The backend misses on the first read and hits on the second, as if another
	// request wrote the value between the lookup and the claim.
	store := NewSyncMap[string]()
	var reads atomic.Int32
	backend := &trackedCache[string]{
		onGet: func(key string) (string, error) {
			if reads.Add(1) == 1 {
				return "", &ErrKeyNotFound{}
			}
			return store.Get(ctx, key)
		},
		onSet: func(key, value string) error { return store.Set(ctx, key, value) },
		onDel: func(key string) error { return store.Del(ctx, key) },
	}
	require.NoError(t, store.Set(ctx, "x", "written-meanwhile"))
	up := &batchUpstream{data: map[string]string{"x": "from-upstream"}}
	cli := NewClient(backend, up, WithDoubleCheck[string](DoubleCheckEnabled))

	got, err := cli.GetMany(ctx, []string{"x"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"x": "written-meanwhile"}, got)
	batches, _ := up.calls()
	assert.Empty(t, batches, "double-check finds the value, so upstream is not called")
}

func TestClientGetManyLayered(t *testing.T) {
	ctx := context.Background()
	l2, _ := newGORMCache[string](t, "layered_batch")
	bottom := &batchUpstream{data: map[string]string{"a": "1", "b": "2", "c": "3"}}
	l2Client := NewClient(l2, bottom)
	l1Client := NewClient(newRistrettoCache[string](t), l2Client)

	got, err := l1Client.GetMany(ctx, []string{"a", "b", "c", "missing"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"a": "1", "b": "2", "c": "3"}, got)

	batches, singles := bottom.calls()
	assert.Equal(t, [][]string{{"a", "b", "c", "missing"}}, batches, "the batch reaches the bottom upstream as one call")
	assert.Empty(t, singles)

	stored, err := l2.GetMany(ctx, []string{"a", "b", "c"})
	require.NoError(t, err)
	assert.Len(t, stored, 3, "the middle layer was filled too")
}

func TestClientGetManyContextCancel(t *testing.T) {
	release := make(chan struct{})
	up := &batchUpstream{data: map[string]string{"a": "1"}, block: func([]string) { <-release }}
	defer close(release)
	cli := NewClient(NewSyncMap[string](), up)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	got, err := cli.GetMany(ctx, []string{"a"})
	assert.Empty(t, got)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func waitFor(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting")
	}
}

// BenchmarkGetManyVsGet compares fetching 100 missing keys with one GetMany
// against a loop of Get, with an upstream that costs one round trip per call
// whether it answers one key or many (a DB query, an API call).
func BenchmarkGetManyVsGet(b *testing.B) {
	const n = 100
	const roundTrip = time.Millisecond
	ctx := context.Background()
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("key-%d", i)
	}

	newClient := func(calls *atomic.Int64) *Client[string] {
		up := batchUpstreamFunc[string](func(_ context.Context, keys []string) (map[string]string, error) {
			calls.Add(1)
			time.Sleep(roundTrip)
			out := make(map[string]string, len(keys))
			for _, k := range keys {
				out[k] = "v-" + k
			}
			return out, nil
		})
		return NewClient(NewSyncMap[string](), up)
	}

	b.Run("GetMany", func(b *testing.B) {
		var calls atomic.Int64
		for b.Loop() {
			cli := newClient(&calls)
			got, err := cli.GetMany(ctx, keys)
			if err != nil || len(got) != n {
				b.Fatal(err, len(got))
			}
		}
		b.ReportMetric(float64(calls.Load())/float64(b.N), "upstream-calls/op")
	})

	b.Run("loop Get", func(b *testing.B) {
		var calls atomic.Int64
		for b.Loop() {
			cli := newClient(&calls)
			for _, k := range keys {
				if _, err := cli.Get(ctx, k); err != nil {
					b.Fatal(err)
				}
			}
		}
		b.ReportMetric(float64(calls.Load())/float64(b.N), "upstream-calls/op")
	})
}

func TestClientGetManyUpstreamGoexit(t *testing.T) {
	t.Run("batch upstream: every claimed key gets the failure, like Get", func(t *testing.T) {
		var calls atomic.Int64
		up := batchUpstreamFunc[string](func(_ context.Context, keys []string) (map[string]string, error) {
			if calls.Add(1) == 1 {
				runtime.Goexit()
			}
			return map[string]string{"a": "1"}, nil
		})
		cli := NewClient(NewSyncMap[string](), up)

		_, err := cli.GetMany(context.Background(), []string{"a"}) // no deadline: must not hang
		var be *BatchError
		require.ErrorAs(t, err, &be)
		assert.Equal(t, "upstream fetch exited without returning (runtime.Goexit)", be.Errors["a"].Error())

		ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
		defer cancel2()
		got, err := cli.GetMany(ctx2, []string{"a"})
		require.NoError(t, err, "the next call starts a new fetch")
		assert.Equal(t, map[string]string{"a": "1"}, got)
	})

	t.Run("per-key upstream: the key is released like Get, the others are served", func(t *testing.T) {
		backend := NewSyncMap[string]()
		cli := NewClient(backend, UpstreamFunc[string](func(_ context.Context, key string) (string, error) {
			if key == "bad" {
				runtime.Goexit()
			}
			return "v-" + key, nil
		}))

		got, err := cli.GetMany(context.Background(), []string{"a", "bad"})
		assert.Equal(t, map[string]string{"a": "v-a"}, got)
		var be *BatchError
		require.ErrorAs(t, err, &be)
		assert.Equal(t, "upstream fetch exited without returning (runtime.Goexit)", be.Errors["bad"].Error())
		_, err = backend.Get(context.Background(), "bad")
		assert.True(t, IsErrKeyNotFound(err), "nothing is written for the key")
	})
}

// failingBatchGet answers every GetMany with err.
type failingBatchGet[T any] struct {
	*SyncMap[T]
	err error
}

func (f failingBatchGet[T]) GetMany(context.Context, []string) (map[string]T, error) {
	return nil, f.err
}

func TestClientGetManyWholeBatchErrors(t *testing.T) {
	ctx := context.Background()
	conn := errors.New("conn reset")
	joined := stderrors.Join(conn, &ErrKeyNotFound{})

	// requireFailedAll checks every key failed with an error that still is conn
	// but never reads as not-found.
	requireFailedAll := func(t *testing.T, got map[string]string, err error, keys ...string) {
		t.Helper()
		assert.Empty(t, got)
		var be *BatchError
		require.ErrorAs(t, err, &be)
		assert.ElementsMatch(t, keys, slices.Collect(maps.Keys(be.Errors)))
		for _, kerr := range be.Errors {
			assert.ErrorIs(t, kerr, conn, "the cause keeps its identity")
			assert.False(t, IsErrKeyNotFound(kerr), "a whole-batch failure must not read as not-found")
		}
	}

	for name, batchErr := range map[string]error{"bare not-found": &ErrKeyNotFound{}, "joined": joined} {
		t.Run("backend: "+name+" fails every key instead of reading as misses", func(t *testing.T) {
			up := &batchUpstream{data: map[string]string{"a": "1"}}
			cli := NewClient[string](failingBatchGet[string]{NewSyncMap[string](), batchErr}, up)
			got, err := cli.GetMany(ctx, []string{"a"})
			assert.Empty(t, got)
			var be *BatchError
			require.ErrorAs(t, err, &be)
			assert.False(t, IsErrKeyNotFound(be.Errors["a"]))
			batches, ones := up.calls()
			assert.Empty(t, batches, "a failed backend read is not a miss")
			assert.Empty(t, ones)
		})
	}

	t.Run("not-found cache: a joined error fails every key", func(t *testing.T) {
		up := &batchUpstream{data: map[string]string{"a": "1"}}
		cli := NewClient(NewSyncMap[string](), up,
			NotFoundWithTTL[string](failingBatchGet[time.Time]{NewSyncMap[time.Time](), joined}, time.Hour, 0))
		got, err := cli.GetMany(ctx, []string{"a", "b"})
		requireFailedAll(t, got, err, "a", "b")
		batches, ones := up.calls()
		assert.Empty(t, batches, "a failed not-found cache read is not a miss")
		assert.Empty(t, ones)
	})

	t.Run("upstream: a joined error keeps its cause and caches nothing", func(t *testing.T) {
		notFound := NewSyncMap[time.Time]()
		up := batchUpstreamFunc[string](func(context.Context, []string) (map[string]string, error) {
			return nil, joined
		})
		cli := NewClient(NewSyncMap[string](), up, NotFoundWithTTL[string](notFound, time.Hour, 0))
		got, err := cli.GetMany(ctx, []string{"a", "b"})
		requireFailedAll(t, got, err, "a", "b")
		_, err = notFound.Get(ctx, "a")
		assert.True(t, IsErrKeyNotFound(err), "no not-found is cached for a key the upstream never answered")
	})

	t.Run("layered: a lower layer's whole-batch failure stays a failure", func(t *testing.T) {
		lower := NewClient(NewSyncMap[string](), batchUpstreamFunc[string](func(context.Context, []string) (map[string]string, error) {
			return nil, joined
		}))
		notFound := NewSyncMap[time.Time]()
		cli := NewClient(NewSyncMap[string](), lower, NotFoundWithTTL[string](notFound, time.Hour, 0))
		got, err := cli.GetMany(ctx, []string{"a"})
		requireFailedAll(t, got, err, "a")
		_, err = notFound.Get(ctx, "a")
		assert.True(t, IsErrKeyNotFound(err))
	})
}

// missThenFailBatchGet misses every key on its first GetMany and answers
// later ones with err.
type missThenFailBatchGet struct {
	*SyncMap[string]
	calls atomic.Int32
	err   error
}

func (m *missThenFailBatchGet) GetMany(context.Context, []string) (map[string]string, error) {
	if m.calls.Add(1) == 1 {
		return map[string]string{}, nil
	}
	return nil, m.err
}

func TestClientGetManyWholeBatchErrorInterplay(t *testing.T) {
	ctx := context.Background()

	t.Run("a failed double-check falls through to the upstream, even if the error looks like a cached not-found", func(t *testing.T) {
		backend := &missThenFailBatchGet{SyncMap: NewSyncMap[string](), err: &ErrKeyNotFound{Cached: true, CacheState: StateFresh}}
		up := &batchUpstream{data: map[string]string{"a": "1"}}
		cli := NewClient[string](backend, up, WithDoubleCheck[string](DoubleCheckEnabled))

		got, err := cli.GetMany(ctx, []string{"a"})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"a": "1"}, got, "like Get, a failed double-check is not an answer")
		assert.Equal(t, int32(2), backend.calls.Load(), "precondition: the double-check ran")
	})

	t.Run("a Get that joins a failed batch fetch gets the failure, not a not-found", func(t *testing.T) {
		conn := errors.New("conn reset")
		entered, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		up := batchUpstreamFunc[string](func(context.Context, []string) (map[string]string, error) {
			if calls.Add(1) == 1 {
				close(entered)
				<-release
			}
			return nil, stderrors.Join(conn, &ErrKeyNotFound{})
		})
		notFound := NewSyncMap[time.Time]()
		cli := NewClient(NewSyncMap[string](), up, NotFoundWithTTL[string](notFound, time.Hour, 0))
		var hooks hookCounts
		hooks.install(cli, nil)

		var wg sync.WaitGroup
		wg.Go(func() {
			_, err := cli.GetMany(ctx, []string{"a"})
			assert.ErrorIs(t, err, conn)
		})
		waitFor(t, entered) // GetMany owns the fetch of a
		var getErr error
		wg.Go(func() { _, getErr = cli.Get(ctx, "a") })
		require.Eventually(t, func() bool { return hooks.before.Load() == 1 }, time.Second, time.Millisecond)
		time.Sleep(50 * time.Millisecond) // let the Get join the flight
		close(release)
		wg.Wait()

		assert.Equal(t, int32(1), calls.Load(), "the Get joined the batch's fetch")
		require.Error(t, getErr)
		assert.ErrorIs(t, getErr, conn)
		assert.False(t, IsErrKeyNotFound(getErr))
		_, err := notFound.Get(ctx, "a")
		assert.True(t, IsErrKeyNotFound(err), "no not-found is cached")
	})
}

// keyFailingCache is a Cache (not a BatchCache) whose Set and Del fail for bad.
type keyFailingCache[T any] struct {
	m   *SyncMap[T]
	bad string
}

func (c keyFailingCache[T]) Get(ctx context.Context, key string) (T, error) { return c.m.Get(ctx, key) }

func (c keyFailingCache[T]) Set(ctx context.Context, key string, value T) error {
	if key == c.bad {
		return errors.New("boom")
	}
	return c.m.Set(ctx, key, value)
}

func (c keyFailingCache[T]) Del(ctx context.Context, key string) error {
	if key == c.bad {
		return errors.New("boom")
	}
	return c.m.Del(ctx, key)
}

func TestClientGetManyPerKeyFallbackIsPerKey(t *testing.T) {
	ctx := context.Background()

	t.Run("every key gets its own fetch timeout", func(t *testing.T) {
		up := UpstreamFunc[string](func(ctx context.Context, key string) (string, error) {
			select {
			case <-time.After(30 * time.Millisecond):
				return key, nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		})
		cli := NewClient[string](NewSyncMap[string](), up,
			WithFetchTimeout[string](100*time.Millisecond), WithGetManyFetchConcurrency[string](1))
		keys := make([]string, 10)
		for i := range keys {
			keys[i] = fmt.Sprintf("k%d", i)
		}
		got, err := cli.GetMany(ctx, keys)
		require.NoError(t, err, "10 fetches of 30ms each run one at a time; none exceeds 100ms on its own")
		assert.Len(t, got, 10)
	})

	t.Run("a key is published as soon as it is fetched, not when the whole batch is", func(t *testing.T) {
		slowEntered, releaseSlow := make(chan struct{}), make(chan struct{})
		up := UpstreamFunc[string](func(_ context.Context, key string) (string, error) {
			if key == "slow" {
				close(slowEntered)
				<-releaseSlow
			}
			return "v-" + key, nil
		})
		cli := NewClient[string](NewSyncMap[string](), up, WithGetManyFetchConcurrency[string](2))

		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = cli.GetMany(ctx, []string{"fast", "slow"})
		}()
		waitFor(t, slowEntered)
		getCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		v, err := cli.Get(getCtx, "fast")
		close(releaseSlow)
		<-done
		require.NoError(t, err, "a Get of fast does not wait for slow")
		assert.Equal(t, "v-fast", v)
	})
}

func TestClientGetManyNotFoundCacheFailureKeepsBackendWrites(t *testing.T) {
	ctx := context.Background()

	t.Run("found values are still cached", func(t *testing.T) {
		backend := NewSyncMap[string]()
		notFound := keyFailingCache[time.Time]{m: NewSyncMap[time.Time](), bad: "bad"}
		up := &batchUpstream{data: map[string]string{"a": "1", "bad": "2"}}
		cli := NewClient(backend, up, NotFoundWithTTL[string](notFound, time.Hour, 0))

		got, err := cli.GetMany(ctx, []string{"a", "bad"})
		require.NoError(t, err, "a failed cache write is logged, like Get")
		assert.Equal(t, map[string]string{"a": "1", "bad": "2"}, got)
		v, err := backend.Get(ctx, "a")
		require.NoError(t, err, "one key's not-found cleanup failing does not stop the others being cached")
		assert.Equal(t, "1", v)
	})

	t.Run("keys gone upstream are still deleted from the backend", func(t *testing.T) {
		backend := NewSyncMap[string]()
		require.NoError(t, backend.Set(ctx, "a", "old"))
		require.NoError(t, backend.Set(ctx, "bad", "old"))
		notFound := keyFailingCache[time.Time]{m: NewSyncMap[time.Time](), bad: "bad"}
		up := &batchUpstream{data: map[string]string{}}
		cli := NewClient(backend, up, NotFoundWithTTL[string](notFound, time.Hour, 0),
			WithStale[string](func(string) State { return StateRotten }))

		got, err := cli.GetMany(ctx, []string{"a", "bad"})
		require.NoError(t, err)
		assert.Empty(t, got)
		_, err = backend.Get(ctx, "a")
		assert.True(t, IsErrKeyNotFound(err), "one key's not-found write failing does not keep the others' old values")
		_, err = backend.Get(ctx, "bad")
		assert.True(t, IsErrKeyNotFound(err), "nor its own")
	})
}

func TestClientGetManyCoverageGaps(t *testing.T) {
	ctx := context.Background()

	t.Run("WithFetchConcurrency bounds the fetches of one key across GetMany calls", func(t *testing.T) {
		var calls atomic.Int32
		release := make(chan struct{})
		up := UpstreamFunc[string](func(_ context.Context, key string) (string, error) {
			calls.Add(1)
			<-release
			return "v-" + key, nil
		})
		// DoubleCheck makes a caller that claims after a slot finished reuse its value
		cli := NewClient[string](NewSyncMap[string](), up, WithFetchConcurrency[string](3),
			WithDoubleCheck[string](DoubleCheckEnabled))

		var wg sync.WaitGroup
		for range 30 {
			wg.Go(func() {
				got, err := cli.GetMany(ctx, []string{"k"})
				assert.NoError(t, err)
				assert.Equal(t, map[string]string{"k": "v-k"}, got)
			})
		}
		require.Eventually(t, func() bool { return calls.Load() >= 1 }, time.Second, time.Millisecond)
		close(release)
		wg.Wait()
		assert.LessOrEqual(t, calls.Load(), int32(3), "at most one fetch per slot")
	})

	t.Run("a stale not-found is served as absent and refreshed in the background", func(t *testing.T) {
		clock := NewMockClock(time.Now())
		defer clock.Install()()
		notFound := NewSyncMap[time.Time]()
		require.NoError(t, notFound.Set(ctx, "k", NowFunc()))
		clock.Advance(time.Minute) // past fresh, within stale
		backend := NewSyncMap[string]()
		up := &batchUpstream{data: map[string]string{"k": "now-exists"}}
		cli := NewClient(backend, up,
			NotFoundWithTTL[string](notFound, time.Second, time.Hour),
			WithServeStale[string](true))

		got, err := cli.GetMany(ctx, []string{"k"})
		require.NoError(t, err)
		assert.Empty(t, got, "the stale not-found is served")
		require.Eventually(t, func() bool {
			v, err := backend.Get(ctx, "k")
			return err == nil && v == "now-exists"
		}, time.Second, time.Millisecond, "and refreshed in the background")
	})

	t.Run("Get and GetMany share the background refresh of a stale key", func(t *testing.T) {
		clock := NewMockClock(time.Now())
		defer clock.Install()()
		var calls atomic.Int32
		entered, release := make(chan struct{}), make(chan struct{})
		up := UpstreamFunc[*Entry[string]](func(context.Context, string) (*Entry[string], error) {
			if calls.Add(1) == 1 {
				close(entered)
				<-release
			}
			return &Entry[string]{Data: "new", CachedAt: NowFunc()}, nil
		})
		backend := NewSyncMap[*Entry[string]]()
		require.NoError(t, backend.Set(ctx, "k", &Entry[string]{Data: "old", CachedAt: NowFunc()}))
		clock.Advance(2 * time.Minute) // stale
		cli := NewClient(backend, up,
			EntryWithTTL[string](time.Minute, time.Hour),
			WithServeStale[*Entry[string]](true))

		v, err := cli.Get(ctx, "k")
		require.NoError(t, err)
		assert.Equal(t, "old", v.Data)
		waitFor(t, entered) // Get's refresh is in flight
		got, err := cli.GetMany(ctx, []string{"k"})
		require.NoError(t, err)
		assert.Equal(t, "old", got["k"].Data)
		close(release)
		require.Eventually(t, func() bool {
			e, err := backend.Get(ctx, "k")
			return err == nil && e.Data == "new"
		}, time.Second, time.Millisecond)
		assert.Equal(t, int32(1), calls.Load(), "GetMany does not start a second refresh")
	})
}

func TestClientGetManyPerKeyFallbackDoesNotQueueOthers(t *testing.T) {
	keys := make([]string, 20)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%02d", i)
	}

	t.Run("a Get of a key still queued in a GetMany does not wait for the queue", func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		up := UpstreamFunc[string](func(_ context.Context, key string) (string, error) {
			if key == "k00" {
				close(entered)
				<-release
			}
			calls.Add(1)
			return "v-" + key, nil
		})
		cli := NewClient[string](NewSyncMap[string](), up, WithGetManyFetchConcurrency[string](1))
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = cli.GetMany(context.Background(), keys)
		}()
		waitFor(t, entered) // the GetMany's only slot is busy with k00

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		v, err := cli.Get(ctx, "k19")
		close(release)
		<-done
		require.NoError(t, err, "k19 is not claimed until the GetMany gets to it")
		assert.Equal(t, "v-k19", v)
	})

	t.Run("a canceled GetMany starts no more fetches", func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		up := UpstreamFunc[string](func(_ context.Context, key string) (string, error) {
			if calls.Add(1) == 1 {
				close(entered)
				<-release
			}
			return "v-" + key, nil
		})
		cli := NewClient[string](NewSyncMap[string](), up, WithGetManyFetchConcurrency[string](1))
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() {
			_, err := cli.GetMany(ctx, keys)
			errc <- err
		}()
		waitFor(t, entered)
		cancel()
		err := <-errc
		var be *BatchError
		require.ErrorAs(t, err, &be)
		assert.Len(t, be.Errors, 20, "every key reports the cancellation")
		close(release)
		assert.Never(t, func() bool { return calls.Load() > 1 }, 100*time.Millisecond, 5*time.Millisecond,
			"only the fetch already started runs on")
	})
}

func TestClientGetManyRefreshOfManyKeysLogsNoFalseFailures(t *testing.T) {
	ctx := context.Background()
	keys := []string{"k0", "k1", "k2", "k3", "k4", "k5"}
	backend := NewSyncMap[string]()
	for _, k := range keys {
		require.NoError(t, backend.Set(ctx, k, "old"))
	}
	var logBuf syncBuffer
	up := UpstreamFunc[string](func(_ context.Context, key string) (string, error) {
		time.Sleep(40 * time.Millisecond)
		return "v-" + key, nil
	})
	cli := NewClient[string](backend, up,
		WithStale[string](func(v string) State {
			if v == "old" {
				return StateStale
			}
			return StateFresh
		}),
		WithServeStale[string](true),
		WithFetchTimeout[string](100*time.Millisecond), // each key fits, the six together do not
		WithGetManyFetchConcurrency[string](1),
		WithLogger[string](slog.New(slog.NewTextHandler(&logBuf, nil))))

	_, err := cli.GetMany(ctx, keys)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		got, _ := backend.GetMany(ctx, keys)
		for _, k := range keys {
			if got[k] != "v-"+k {
				return false
			}
		}
		return true
	}, 2*time.Second, 5*time.Millisecond, "every key is refreshed, each within its own fetch timeout")
	assert.NotContains(t, logBuf.String(), "async refresh failed")
}

func TestClientGetNotFoundCacheFailureKeepsBackendWrites(t *testing.T) {
	ctx := context.Background()

	t.Run("a found value is still cached", func(t *testing.T) {
		backend := NewSyncMap[string]()
		notFound := keyFailingCache[time.Time]{m: NewSyncMap[time.Time](), bad: "k"}
		cli := NewClient(backend, UpstreamFunc[string](func(context.Context, string) (string, error) { return "v", nil }),
			NotFoundWithTTL[string](notFound, time.Hour, 0))
		v, err := cli.Get(ctx, "k")
		require.NoError(t, err)
		assert.Equal(t, "v", v)
		v, err = backend.Get(ctx, "k")
		require.NoError(t, err, "like GetMany, a failed not-found cleanup does not stop the backend write")
		assert.Equal(t, "v", v)
	})

	t.Run("a key gone upstream is still deleted from the backend", func(t *testing.T) {
		backend := NewSyncMap[string]()
		require.NoError(t, backend.Set(ctx, "k", "old"))
		notFound := keyFailingCache[time.Time]{m: NewSyncMap[time.Time](), bad: "k"}
		cli := NewClient(backend, UpstreamFunc[string](func(context.Context, string) (string, error) { return "", &ErrKeyNotFound{} }),
			NotFoundWithTTL[string](notFound, time.Hour, 0),
			WithStale[string](func(string) State { return StateRotten }))
		_, err := cli.Get(ctx, "k")
		require.True(t, IsErrKeyNotFound(err))
		_, err = backend.Get(ctx, "k")
		assert.True(t, IsErrKeyNotFound(err), "like GetMany, the old value goes even if no not-found was recorded")
	})
}

func TestClientGetManyBatchRefreshThatTimesOutIsLogged(t *testing.T) {
	ctx := context.Background()
	clock := NewMockClock(time.Now())
	defer clock.Install()()
	backend := NewSyncMap[*Entry[string]]()
	require.NoError(t, backend.Set(ctx, "k", &Entry[string]{Data: "old", CachedAt: NowFunc()}))
	clock.Advance(2 * time.Minute) // stale
	var logBuf syncBuffer
	up := batchUpstreamFunc[*Entry[string]](func(ctx context.Context, _ []string) (map[string]*Entry[string], error) {
		<-ctx.Done() // hangs until its fetch timeout
		return nil, ctx.Err()
	})
	cli := NewClient(backend, up,
		EntryWithTTL[string](time.Minute, time.Hour),
		WithServeStale[*Entry[string]](true),
		WithFetchTimeout[*Entry[string]](50*time.Millisecond),
		WithLogger[*Entry[string]](slog.New(slog.NewTextHandler(&logBuf, nil))))

	got, err := cli.GetMany(ctx, []string{"k"})
	require.NoError(t, err)
	assert.Equal(t, "old", got["k"].Data)
	require.Eventually(t, func() bool { return strings.Contains(logBuf.String(), "async refresh") },
		2*time.Second, 5*time.Millisecond, "a refresh that never finishes in time is not silent")
}

func TestClientGetManyOverAClientIsNotBoundedAsOneFetch(t *testing.T) {
	// L2 fetches its source key by key, each well within its own timeout; L1
	// hands L2 the whole batch, which takes longer than one fetch timeout
	src := UpstreamFunc[string](func(_ context.Context, key string) (string, error) {
		time.Sleep(30 * time.Millisecond)
		return "v-" + key, nil
	})
	l2 := NewClient[string](NewSyncMap[string](), src, WithGetManyFetchConcurrency[string](1))
	l1 := NewClient[string](NewSyncMap[string](), l2, WithFetchTimeout[string](100*time.Millisecond))
	keys := []string{"k0", "k1", "k2", "k3", "k4", "k5", "k6", "k7"}
	got, err := l1.GetMany(context.Background(), keys)
	require.NoError(t, err, "no key fails just because the batch as a whole outlasted one fetch timeout")
	assert.Len(t, got, len(keys))
}

// hangingBatchCache is a BatchCache whose reads block until ctx is done.
type hangingBatchCache struct{ *SyncMap[string] }

func (hangingBatchCache) Get(ctx context.Context, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func (hangingBatchCache) GetMany(ctx context.Context, _ []string) (map[string]string, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestClientGetManyOverAClientIsStillBounded(t *testing.T) {
	src := UpstreamFunc[string](func(_ context.Context, key string) (string, error) { return "v-" + key, nil })
	l2 := NewClient[string](hangingBatchCache{NewSyncMap[string]()}, src, WithFetchTimeout[string](50*time.Millisecond))
	l1 := NewClient[string](NewSyncMap[string](), l2, WithFetchTimeout[string](50*time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := l1.GetMany(ctx, []string{"k"})
	require.Error(t, err)
	require.Eventually(t, func() bool {
		l1.flights.mu.Lock()
		defer l1.flights.mu.Unlock()
		return len(l1.flights.flights) == 0
	}, 2*time.Second, 5*time.Millisecond, "a lower layer that hangs does not keep the key claimed forever")
}

func TestRedisCacheGetManyPerKeyErrors(t *testing.T) {
	ctx := context.Background()
	cache, mr := newRedisCache[string](t)
	_, err := mr.Lpush("a-list", "x") // GET on a list fails with WRONGTYPE
	require.NoError(t, err)
	require.NoError(t, mr.Set("b", "2"))

	got, err := cache.GetMany(ctx, []string{"a-list", "b", "missing"})
	assert.Equal(t, map[string]string{"b": "2"}, got, "one failing command does not fail the batch")
	var be *BatchError
	require.ErrorAs(t, err, &be)
	assert.Equal(t, []string{"a-list"}, slices.Collect(maps.Keys(be.Errors)))
}

func TestGORMCacheBatchLargerThanBindLimit(t *testing.T) {
	ctx := context.Background()
	cache, _ := newGORMCache[string](t, "batch_large")
	const n = 40000 // above SQLite's 32766 bound parameters per statement
	values := make(map[string]string, n)
	keys := make([]string, 0, n)
	for i := range n {
		k := fmt.Sprintf("k%d", i)
		values[k] = "v"
		keys = append(keys, k)
	}

	require.NoError(t, cache.SetMany(ctx, values))
	got, err := cache.GetMany(ctx, keys)
	require.NoError(t, err)
	assert.Len(t, got, n)
	require.NoError(t, cache.DelMany(ctx, keys))
	got, err = cache.GetMany(ctx, keys)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// missFirstBatch is a SyncMap whose first GetMany misses everything, as if
// the values were written right after it.
type missFirstBatch struct {
	*SyncMap[string]
	calls atomic.Int64
}

func (m *missFirstBatch) GetMany(ctx context.Context, keys []string) (map[string]string, error) {
	if m.calls.Add(1) == 1 {
		return map[string]string{}, nil
	}
	return m.SyncMap.GetMany(ctx, keys)
}

// hookedCache is a plain Cache (no batch methods) whose Set/Del can be intercepted.
type hookedCache struct {
	m         *SyncMap[string]
	beforeSet func(key, value string) error
	afterSet  func(key, value string)
	beforeDel func(key string) error
}

func newHookedCache() *hookedCache { return &hookedCache{m: NewSyncMap[string]()} }

func (h *hookedCache) Get(ctx context.Context, key string) (string, error) { return h.m.Get(ctx, key) }

func (h *hookedCache) Set(ctx context.Context, key, value string) error {
	if h.beforeSet != nil {
		if err := h.beforeSet(key, value); err != nil {
			return err
		}
	}
	if err := h.m.Set(ctx, key, value); err != nil {
		return err
	}
	if h.afterSet != nil {
		h.afterSet(key, value)
	}
	return nil
}

func (h *hookedCache) Del(ctx context.Context, key string) error {
	if h.beforeDel != nil {
		if err := h.beforeDel(key); err != nil {
			return err
		}
	}
	return h.m.Del(ctx, key)
}

func TestClientGetManyReviewFixes(t *testing.T) {
	ctx := context.Background()

	t.Run("a whole-batch error that merely contains a BatchError fails every key", func(t *testing.T) {
		notFound := NewSyncMap[time.Time]()
		conn := errors.New("conn reset")
		up := batchUpstreamFunc[string](func(context.Context, []string) (map[string]string, error) {
			return nil, stderrors.Join(conn, &BatchError{Errors: map[string]error{"a": errors.New("x")}})
		})
		cli := NewClient(NewSyncMap[string](), up, NotFoundWithTTL[string](notFound, time.Hour, 0))

		got, err := cli.GetMany(ctx, []string{"a", "b"})
		assert.Empty(t, got)
		var be *BatchError
		require.ErrorAs(t, err, &be)
		assert.Len(t, be.Errors, 2, "b is a failure, not a not-found")
		_, err = notFound.Get(ctx, "b")
		assert.True(t, IsErrKeyNotFound(err), "no not-found is cached for a key the upstream never answered")
	})

	t.Run("a panic in the batch upstream keeps the keys the double-check found", func(t *testing.T) {
		backend := &missFirstBatch{SyncMap: NewSyncMap[string]()}
		require.NoError(t, backend.Set(ctx, "a", "1"))
		up := batchUpstreamFunc[string](func(context.Context, []string) (map[string]string, error) {
			panic("kaboom")
		})
		cli := NewClient[string](backend, up, WithDoubleCheck[string](DoubleCheckEnabled))

		got, err := cli.GetMany(ctx, []string{"a", "b"})
		assert.Equal(t, map[string]string{"a": "1"}, got, "the double-check found a before the upstream panicked")
		var be *BatchError
		require.ErrorAs(t, err, &be)
		assert.Equal(t, []string{"b"}, slices.Collect(maps.Keys(be.Errors)))
	})

	t.Run("a per-key backend write failure does not skip the other keys", func(t *testing.T) {
		backend := newHookedCache()
		backend.beforeSet = func(key, _ string) error {
			if key == "bad" {
				return errors.New("too large")
			}
			return nil
		}
		up := &batchUpstream{data: map[string]string{"a": "1", "bad": "2", "c": "3"}}
		cli := NewClient[string](backend, up)
		got, err := cli.GetMany(ctx, []string{"a", "bad", "c"})
		require.NoError(t, err)
		assert.Len(t, got, 3)
		for _, k := range []string{"a", "c"} {
			_, err := backend.Get(ctx, k)
			assert.NoError(t, err, "%s is cached", k)
		}
	})
}

func TestRedisCacheSetManyEncodeErrorKeepsOthers(t *testing.T) {
	ctx := context.Background()
	cache, mr := newRedisCache[any](t)
	err := cache.SetMany(ctx, map[string]any{"ok": 1, "bad": make(chan int)})
	require.Error(t, err)
	assert.True(t, mr.Exists("ok"), "the encodable value is still written")
}

// slowSecondBatch is a SyncMap whose GetMany sleeps from the second call on
// (the double-check), as if the backend were slow to answer it.
type slowSecondBatch struct {
	*SyncMap[string]
	calls atomic.Int64
	delay time.Duration
}

func (m *slowSecondBatch) GetMany(ctx context.Context, keys []string) (map[string]string, error) {
	if m.calls.Add(1) > 1 {
		time.Sleep(m.delay)
	}
	return m.SyncMap.GetMany(ctx, keys)
}

func TestClientGetManyReviewFixes2(t *testing.T) {
	ctx := context.Background()

	t.Run("the per-key fallback fetches at most WithGetManyFetchConcurrency keys at once", func(t *testing.T) {
		var cur, peak atomic.Int64
		up := UpstreamFunc[string](func(_ context.Context, key string) (string, error) {
			n := cur.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			cur.Add(-1)
			return key, nil
		})
		cli := NewClient[string](NewSyncMap[string](), up, WithGetManyFetchConcurrency[string](4))
		keys := make([]string, 50)
		for i := range keys {
			keys[i] = fmt.Sprintf("k%d", i)
		}
		got, err := cli.GetMany(ctx, keys)
		require.NoError(t, err)
		assert.Len(t, got, 50)
		assert.LessOrEqual(t, peak.Load(), int64(4))
		assert.Greater(t, peak.Load(), int64(1), "still concurrent")
	})

	t.Run("a whole-batch error that contains a not-found is a failure, not a not-found", func(t *testing.T) {
		notFound := NewSyncMap[time.Time]()
		up := batchUpstreamFunc[string](func(context.Context, []string) (map[string]string, error) {
			return nil, stderrors.Join(errors.New("conn reset"), &ErrKeyNotFound{})
		})
		cli := NewClient(NewSyncMap[string](), up, NotFoundWithTTL[string](notFound, time.Hour, 0))

		got, err := cli.GetMany(ctx, []string{"a", "b"})
		assert.Empty(t, got)
		var be *BatchError
		require.ErrorAs(t, err, &be)
		assert.Len(t, be.Errors, 2)
		for _, kerr := range be.Errors {
			assert.False(t, IsErrKeyNotFound(kerr), "a whole-batch failure must not read as not-found")
		}
		_, err = notFound.Get(ctx, "a")
		assert.True(t, IsErrKeyNotFound(err), "no not-found is cached for a key the upstream never answered")
	})

	t.Run("the fetch timeout starts after the double-check, like Get", func(t *testing.T) {
		backend := &slowSecondBatch{SyncMap: NewSyncMap[string](), delay: 100 * time.Millisecond}
		up := batchUpstreamFunc[string](func(ctx context.Context, keys []string) (map[string]string, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return map[string]string{"a": "1"}, nil
		})
		cli := NewClient[string](backend, up,
			WithDoubleCheck[string](DoubleCheckEnabled),
			WithFetchTimeout[string](50*time.Millisecond))
		got, err := cli.GetMany(ctx, []string{"a"})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"a": "1"}, got)
	})
}

// syncBuffer is a bytes.Buffer safe for a logger and a test to share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestBatchWritesAreBestEffortAndReportFailedKeys(t *testing.T) {
	ctx := context.Background()

	t.Run("RedisCache reports every key of a failed pipeline", func(t *testing.T) {
		cache, mr := newRedisCache[string](t)
		mr.Close()
		var be *BatchError
		require.ErrorAs(t, cache.SetMany(ctx, map[string]string{"a": "1", "b": "2"}), &be)
		assert.ElementsMatch(t, []string{"a", "b"}, slices.Collect(maps.Keys(be.Errors)))
		require.ErrorAs(t, cache.DelMany(ctx, []string{"a", "b"}), &be)
		assert.ElementsMatch(t, []string{"a", "b"}, slices.Collect(maps.Keys(be.Errors)))
	})

	t.Run("RedisCache reports a value that cannot be encoded by key", func(t *testing.T) {
		cache, _ := newRedisCache[float64](t)
		err := cache.SetMany(ctx, map[string]float64{"ok": 1, "bad": math.NaN()})
		var be *BatchError
		require.ErrorAs(t, err, &be)
		assert.Equal(t, []string{"bad"}, slices.Collect(maps.Keys(be.Errors)))
		v, err := cache.Get(ctx, "ok")
		require.NoError(t, err)
		assert.Equal(t, 1.0, v)
	})

	t.Run("GORMCache keeps writing the chunks after a failed one", func(t *testing.T) {
		cache, db := newGORMCacheWith[string](t, "besteffort", 2)
		var creates atomic.Int32
		require.NoError(t, db.Callback().Create().Before("gorm:create").Register("test:fail-first", func(tx *gorm.DB) {
			if creates.Add(1) == 1 {
				_ = tx.AddError(errors.New("boom"))
			}
		}))
		values := map[string]string{"a": "1", "b": "2", "c": "3", "d": "4"} // sorted: chunks [a b] [c d]
		err := cache.SetMany(ctx, values)
		var be *BatchError
		require.ErrorAs(t, err, &be)
		assert.ElementsMatch(t, []string{"a", "b"}, slices.Collect(maps.Keys(be.Errors)), "the keys of the failed chunk")
		got, err := cache.GetMany(ctx, []string{"a", "b", "c", "d"})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"c": "3", "d": "4"}, got, "the next chunk is still written")
		assert.Equal(t, int32(2), creates.Load(), "one statement per chunk")

		var deletes atomic.Int32
		require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register("test:fail-first", func(tx *gorm.DB) {
			if deletes.Add(1) == 1 {
				_ = tx.AddError(errors.New("boom"))
			}
		}))
		err = cache.DelMany(ctx, []string{"c", "d", "x", "y"})
		require.ErrorAs(t, err, &be)
		assert.ElementsMatch(t, []string{"c", "d"}, slices.Collect(maps.Keys(be.Errors)))
		assert.Equal(t, int32(2), deletes.Load(), "DelMany goes on after a failed chunk")
	})

	t.Run("the per-key fallback for a plain Cache reports by key too", func(t *testing.T) {
		c := keyFailingCache[string]{m: NewSyncMap[string](), bad: "bad"}
		var be *BatchError
		require.ErrorAs(t, setMany[string](ctx, c, map[string]string{"ok": "1", "bad": "2"}), &be)
		assert.Equal(t, []string{"bad"}, slices.Collect(maps.Keys(be.Errors)))
		require.ErrorAs(t, delMany[string](ctx, c, []string{"ok", "bad"}), &be)
		assert.Equal(t, []string{"bad"}, slices.Collect(maps.Keys(be.Errors)))
	})
}

func TestBatchBackendsSplitLargeCallsIntoChunks(t *testing.T) {
	ctx := context.Background()
	t.Run("RedisCache", func(t *testing.T) {
		mr := miniredis.RunT(t)
		client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = client.Close() })
		require.NoError(t, client.Ping(ctx).Err()) // the connection handshake is a pipeline too
		var pipelines atomic.Int32
		client.AddHook(pipelineCounter{&pipelines})
		cache := NewRedisCache[string](&RedisCacheConfig{Client: client, ChunkSize: 2})
		require.NoError(t, cache.SetMany(ctx, map[string]string{"a": "1", "b": "2", "c": "3"}))
		got, err := cache.GetMany(ctx, []string{"a", "b", "c"})
		require.NoError(t, err)
		assert.Len(t, got, 3)
		require.NoError(t, cache.DelMany(ctx, []string{"a", "b", "c"}))
		assert.Equal(t, int32(6), pipelines.Load(), "3 keys in chunks of 2: two pipelines per call")
	})
	t.Run("GORMCache", func(t *testing.T) {
		cache, db := newGORMCacheWith[string](t, "chunks", 2)
		var queries atomic.Int32
		require.NoError(t, db.Callback().Query().Before("gorm:query").Register("test:count", func(*gorm.DB) { queries.Add(1) }))
		require.NoError(t, cache.SetMany(ctx, map[string]string{"a": "1", "b": "2", "c": "3"}))
		got, err := cache.GetMany(ctx, []string{"a", "b", "c"})
		require.NoError(t, err)
		assert.Len(t, got, 3)
		assert.Equal(t, int32(2), queries.Load())
	})
}

// pipelineCounter counts the pipelines a go-redis client sends.
type pipelineCounter struct{ n *atomic.Int32 }

func (pipelineCounter) DialHook(next redis.DialHook) redis.DialHook          { return next }
func (pipelineCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }
func (p pipelineCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		p.n.Add(1)
		return next(ctx, cmds)
	}
}

func TestRedisCacheGetManyWhenRedisIsDown(t *testing.T) {
	ctx := context.Background()
	cache, mr := newRedisCache[string](t)
	require.NoError(t, cache.Set(ctx, "a", "1"))
	mr.Close()
	got, err := cache.GetMany(ctx, []string{"a", "b"})
	assert.Empty(t, got, "nothing was read, so nothing is returned as found")
	var be *BatchError
	require.ErrorAs(t, err, &be)
	assert.ElementsMatch(t, []string{"a", "b"}, slices.Collect(maps.Keys(be.Errors)))
}

func TestClientGetManyChunksBatchUpstreamCalls(t *testing.T) {
	ctx := context.Background()
	keys := []string{"k0", "k1", "k2", "k3", "k4"}

	t.Run("calls carry at most the chunk size, at most the fetch concurrency at once", func(t *testing.T) {
		var mu sync.Mutex
		var sizes []int
		var inFlight, peak atomic.Int32
		up := batchUpstreamFunc[string](func(_ context.Context, ks []string) (map[string]string, error) {
			n := inFlight.Add(1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			defer inFlight.Add(-1)
			mu.Lock()
			sizes = append(sizes, len(ks))
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			out := map[string]string{}
			for _, k := range ks {
				out[k] = "v-" + k
			}
			return out, nil
		})
		cli := NewClient[string](NewSyncMap[string](), up,
			WithGetManyChunkSize[string](2), WithGetManyFetchConcurrency[string](2))
		got, err := cli.GetMany(ctx, keys)
		require.NoError(t, err)
		assert.Len(t, got, 5)
		assert.ElementsMatch(t, []int{2, 2, 1}, sizes)
		assert.Equal(t, int32(2), peak.Load())
	})

	t.Run("a chunk's keys are answered as soon as the chunk is done", func(t *testing.T) {
		release := make(chan struct{})
		up := batchUpstreamFunc[string](func(_ context.Context, ks []string) (map[string]string, error) {
			if slices.Contains(ks, "k4") {
				<-release // the last chunk hangs
			}
			out := map[string]string{}
			for _, k := range ks {
				out[k] = "v-" + k
			}
			return out, nil
		})
		cli := NewClient[string](NewSyncMap[string](), up,
			WithGetManyChunkSize[string](2), WithGetManyFetchConcurrency[string](3))
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = cli.GetMany(ctx, keys)
		}()
		getCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		require.Eventually(t, func() bool { // k0 is claimed by the GetMany, then answered by its chunk
			v, err := cli.Get(getCtx, "k0")
			return err == nil && v == "v-k0"
		}, time.Second, 5*time.Millisecond)
		close(release)
		<-done
	})
}
