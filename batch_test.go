package cachex

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
