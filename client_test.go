package cachex

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientBasics(t *testing.T) {
	ctx := context.Background()
	backend := newRistrettoCache[string](t)

	fetchCount := 0
	upstream := UpstreamFunc[string](func(ctx context.Context, key string) (string, error) {
		fetchCount++
		return "fetched-" + key, nil
	})

	cli := NewClient(backend, upstream)
	defer func() {
		assert.NoError(t, cli.Close())
	}()

	t.Run("fetch from upstream on miss", func(t *testing.T) {
		value, err := cli.Get(ctx, "key1")
		require.NoError(t, err)
		assert.Equal(t, "fetched-key1", value)
		assert.Equal(t, 1, fetchCount)
	})

	t.Run("use backend cache on hit", func(t *testing.T) {
		value, err := cli.Get(ctx, "key1")
		require.NoError(t, err)
		assert.Equal(t, "fetched-key1", value)
		assert.Equal(t, 1, fetchCount, "should still be 1 fetch (cached)")
	})

	t.Run("set and get", func(t *testing.T) {
		require.NoError(t, cli.Set(ctx, "key2", "manual-value"))

		value, err := cli.Get(ctx, "key2")
		require.NoError(t, err)
		assert.Equal(t, "manual-value", value)

		value, err = backend.Get(ctx, "key2")
		require.NoError(t, err)
		assert.Equal(t, "manual-value", value)
	})

	t.Run("del removes from backend", func(t *testing.T) {
		require.NoError(t, cli.Set(ctx, "key3", "temp-value"))
		require.NoError(t, cli.Del(ctx, "key3"))

		_, err := backend.Get(ctx, "key3")
		assert.True(t, IsErrKeyNotFound(err))
	})
}

func TestClientStaleHandling(t *testing.T) {
	ctx := context.Background()
	clock := NewMockClock(time.Now())
	defer clock.Install()()

	type testValue struct {
		Data      string
		Timestamp time.Time
	}

	backend := newRistrettoCache[*testValue](t)
	fetchCount := 0

	upstream := UpstreamFunc[*testValue](func(ctx context.Context, key string) (*testValue, error) {
		fetchCount++
		return &testValue{Data: fmt.Sprintf("fetch-%d", fetchCount), Timestamp: NowFunc()}, nil
	})

	checkStale := func(v *testValue) State {
		age := NowFunc().Sub(v.Timestamp)
		if age < 50*time.Millisecond {
			return StateFresh
		}
		if age < 150*time.Millisecond {
			return StateStale
		}
		return StateTooStale
	}

	t.Run("without serve stale", func(t *testing.T) {
		fetchCount = 0

		cli := NewClient(backend, upstream, WithStale(checkStale))
		defer func() {
			assert.NoError(t, cli.Close())
		}()

		value, err := cli.Get(ctx, "key1")
		require.NoError(t, err)
		assert.Equal(t, "fetch-1", value.Data)

		clock.Advance(60 * time.Millisecond)

		value, err = cli.Get(ctx, "key1")
		require.NoError(t, err)
		assert.Equal(t, "fetch-2", value.Data, "should refetch when stale without serve-stale")
	})

	t.Run("with serve stale", func(t *testing.T) {
		fetchCount = 0
		err := backend.Del(ctx, "key2")
		require.NoError(t, err)

		cli := NewClient(backend, upstream,
			WithStale(checkStale),
			WithServeStale[*testValue](true),
		)
		defer func() {
			assert.NoError(t, cli.Close())
		}()

		value, err := cli.Get(ctx, "key2")
		require.NoError(t, err)
		assert.Equal(t, "fetch-1", value.Data)
		assert.Equal(t, 1, fetchCount)

		clock.Advance(60 * time.Millisecond)

		value, err = cli.Get(ctx, "key2")
		require.NoError(t, err)
		assert.Equal(t, "fetch-1", value.Data, "should serve stale value")

		time.Sleep(10 * time.Millisecond)
		assert.Equal(t, 2, fetchCount, "async refresh should have completed")

		clock.Advance(60 * time.Millisecond)

		value, err = cli.Get(ctx, "key2")
		require.NoError(t, err)
		assert.Equal(t, "fetch-2", value.Data, "should serve new stale value after async refresh")

		time.Sleep(10 * time.Millisecond)
		assert.Equal(t, 3, fetchCount, "second async refresh should have completed")

		clock.Advance(200 * time.Millisecond)

		value, err = cli.Get(ctx, "key2")
		require.NoError(t, err)
		assert.Equal(t, "fetch-4", value.Data, "should refetch when too stale")
		assert.Equal(t, 4, fetchCount)
	})
}

func TestClientNotFoundCache(t *testing.T) {
	ctx := context.Background()
	clock := NewMockClock(time.Now())
	defer clock.Install()()

	backend := newRistrettoCache[string](t)
	notFoundCache := newRistrettoCache[time.Time](t)

	fetchCount := 0
	upstream := UpstreamFunc[string](func(ctx context.Context, key string) (string, error) {
		fetchCount++
		if key == "not-exist" {
			return "", &ErrKeyNotFound{}
		}
		return "value-" + key, nil
	})

	cli := NewClient(backend, upstream,
		NotFoundWithTTL[string](notFoundCache, 100*time.Millisecond, 0),
	)
	defer func() {
		assert.NoError(t, cli.Close())
	}()

	t.Run("cache not found", func(t *testing.T) {
		_, err := cli.Get(ctx, "not-exist")
		assert.True(t, IsErrKeyNotFound(err))
		assert.Equal(t, 1, fetchCount)
	})

	t.Run("use not found cache", func(t *testing.T) {
		_, err := cli.Get(ctx, "not-exist")
		assert.True(t, IsErrKeyNotFound(err))
		assert.Equal(t, 1, fetchCount, "should still be 1 fetch (cached)")
	})

	t.Run("expire not found cache", func(t *testing.T) {
		clock.Advance(150 * time.Millisecond)

		_, err := cli.Get(ctx, "not-exist")
		assert.True(t, IsErrKeyNotFound(err))
		assert.Equal(t, 2, fetchCount, "should refetch after expiration")
	})
}

func TestClientValidation(t *testing.T) {
	backend := newRistrettoCache[string](t)
	upstream := UpstreamFunc[string](func(ctx context.Context, key string) (string, error) {
		return "value", nil
	})

	tests := []struct {
		name      string
		opts      []ClientOption[string]
		wantPanic bool
	}{
		{"negative fetchTimeout", []ClientOption[string]{WithFetchTimeout[string](-1 * time.Second)}, true},
		{"zero fetchTimeout", []ClientOption[string]{WithFetchTimeout[string](0)}, true},
		{"negative fetchConcurrency", []ClientOption[string]{WithFetchConcurrency[string](-1)}, true},
		{"zero fetchConcurrency", []ClientOption[string]{WithFetchConcurrency[string](0)}, true},
		{"valid config", []ClientOption[string]{WithFetchTimeout[string](10 * time.Second), WithFetchConcurrency[string](5)}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.wantPanic {
				assert.Panics(t, func() {
					NewClient(backend, upstream, tt.opts...)
				})
			} else {
				assert.NotPanics(t, func() {
					NewClient(backend, upstream, tt.opts...)
				})
			}
		})
	}
}

func TestClientLayeredCache(t *testing.T) {
	ctx := context.Background()

	type User struct {
		ID   int
		Name string
	}

	l1 := newRistrettoCache[*User](t)
	l2 := newGORMCache[*User](t, "user_cache")

	apiCallCount := 0
	apiUpstream := UpstreamFunc[*User](func(ctx context.Context, key string) (*User, error) {
		apiCallCount++
		time.Sleep(10 * time.Millisecond)
		return &User{ID: 123, Name: "User from API"}, nil
	})

	l2Client := NewClient(l2, apiUpstream)
	defer func() {
		assert.NoError(t, l2Client.Close())
	}()
	l1Client := NewClient(l1, l2Client)
	defer func() {
		assert.NoError(t, l1Client.Close())
	}()

	t.Run("cold cache - fetch from API", func(t *testing.T) {
		user, err := l1Client.Get(ctx, "user:123")
		require.NoError(t, err)
		assert.Equal(t, "User from API", user.Name)
		assert.Equal(t, 1, apiCallCount)
	})

	t.Run("L1 cache hit - no L2 or API", func(t *testing.T) {
		user, err := l1Client.Get(ctx, "user:123")
		require.NoError(t, err)
		assert.Equal(t, "User from API", user.Name)
		assert.Equal(t, 1, apiCallCount, "should still be 1 API call")
	})

	t.Run("L1 miss, L2 hit - no API", func(t *testing.T) {
		err := l1.Del(ctx, "user:123")
		require.NoError(t, err)

		user, err := l1Client.Get(ctx, "user:123")
		require.NoError(t, err)
		assert.Equal(t, "User from API", user.Name)
		assert.Equal(t, 1, apiCallCount, "should still be 1 API call (L2 hit)")
	})

	t.Run("both cache miss - fetch from API again", func(t *testing.T) {
		err := l1.Del(ctx, "user:123")
		require.NoError(t, err)
		err = l2.Del(ctx, "user:123")
		require.NoError(t, err)

		user, err := l1Client.Get(ctx, "user:123")
		require.NoError(t, err)
		assert.Equal(t, "User from API", user.Name)
		assert.Equal(t, 2, apiCallCount)
	})
}

func TestErrKeyNotFound(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{"ErrKeyNotFound", &ErrKeyNotFound{}, true},
		{"ErrKeyNotFound with cache state", &ErrKeyNotFound{Cached: true, CacheState: StateFresh}, true},
		{"wrapped ErrKeyNotFound", fmt.Errorf("wrapped: %w", &ErrKeyNotFound{}), true},
		{"other error", fmt.Errorf("some error"), false},
		{"nil", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsErrKeyNotFound(tt.err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestStaleDataCleanupWhenUpstreamDeletes(t *testing.T) {
	ctx := context.Background()
	clock := NewMockClock(time.Now())
	defer clock.Install()()

	type timestampedValue struct {
		Data      string
		ExpiresAt time.Time
	}

	backend := newRistrettoCache[*timestampedValue](t)

	// Track upstream fetch count
	fetchCount := 0

	// Real data source that can be modified
	realDataExists := true

	upstream := UpstreamFunc[*timestampedValue](func(ctx context.Context, key string) (*timestampedValue, error) {
		fetchCount++
		if realDataExists {
			// Return data with expiration time
			return &timestampedValue{
				Data:      "original-value",
				ExpiresAt: clock.Now().Add(100 * time.Millisecond),
			}, nil
		}
		return nil, &ErrKeyNotFound{}
	})

	// Stale check: fresh for 100ms, then TooStale (force refetch)
	checkStale := func(v *timestampedValue) State {
		if clock.Now().Before(v.ExpiresAt) {
			return StateFresh
		}
		return StateTooStale
	}

	notFoundCache := newRistrettoCache[time.Time](t)
	client := NewClient(backend, upstream,
		WithStale(checkStale),
		WithNotFound[*timestampedValue](notFoundCache, nil),
	)
	defer func() {
		assert.NoError(t, client.Close())
	}()

	// Step 1: Get key1 - fetch from upstream and cache it
	value, err := client.Get(ctx, "key1")
	require.NoError(t, err)
	assert.Equal(t, "original-value", value.Data)
	assert.Equal(t, clock.Now().Add(100*time.Millisecond), value.ExpiresAt)
	assert.Equal(t, 1, fetchCount, "should fetch once from upstream")

	// Verify it's in backend cache
	cachedValue, err := backend.Get(ctx, "key1")
	require.NoError(t, err)
	assert.Equal(t, "original-value", cachedValue.Data)

	// Step 2: Advance time to make cached data stale (past expiration)
	clock.Advance(150 * time.Millisecond)

	// Verify cached data is now stale
	assert.Equal(t, StateTooStale, checkStale(cachedValue), "cached data should be stale")

	// Step 3: Meanwhile, data was deleted from upstream
	realDataExists = false

	// Step 4: Get key1 again
	// - Backend has stale data
	// - Client detects stale → refetch from upstream
	// - Upstream returns ErrKeyNotFound
	// BUG FIX: Should clean up stale backend cache entry
	_, err = client.Get(ctx, "key1")
	assert.True(t, IsErrKeyNotFound(err), "should return ErrKeyNotFound from upstream")
	assert.Equal(t, 2, fetchCount, "should fetch again due to stale data")

	// Step 5: Verify backend cache was cleaned up
	// This is the critical assertion - before fix, stale data would remain
	_, err = backend.Get(ctx, "key1")
	assert.True(t, IsErrKeyNotFound(err), "backend should be cleaned up, not contain stale data")

	// Step 6: Subsequent Get should return cached ErrKeyNotFound
	// Should NOT trigger another upstream fetch
	_, err = client.Get(ctx, "key1")
	assert.True(t, IsErrKeyNotFound(err), "should return cached ErrKeyNotFound")
	assert.Equal(t, 2, fetchCount, "should not fetch again, use notFoundCache")

	var knfErr *ErrKeyNotFound
	require.True(t, errors.As(err, &knfErr), "should be ErrKeyNotFound")
	assert.True(t, knfErr.Cached, "should be cached from notFoundCache")
}

func TestDelSetsNotFoundCache(t *testing.T) {
	ctx := context.Background()
	backend := newRistrettoCache[string](t)
	notFoundCache := newRistrettoCache[time.Time](t)

	fetchCount := 0
	upstream := UpstreamFunc[string](func(ctx context.Context, key string) (string, error) {
		fetchCount++
		return "value", nil
	})

	client := NewClient(backend, upstream, WithNotFound[string](notFoundCache, nil))
	defer func() {
		assert.NoError(t, client.Close())
	}()

	// Step 1: Get key to cache it
	value, err := client.Get(ctx, "key1")
	require.NoError(t, err)
	assert.Equal(t, "value", value)
	assert.Equal(t, 1, fetchCount)

	// Verify it's in backend
	cachedValue, err := backend.Get(ctx, "key1")
	require.NoError(t, err)
	assert.Equal(t, "value", cachedValue)

	// Step 2: Delete the key
	err = client.Del(ctx, "key1")
	require.NoError(t, err)

	// Step 3: Verify backend is clean
	_, err = backend.Get(ctx, "key1")
	assert.True(t, IsErrKeyNotFound(err), "backend should be clean")

	// Step 4: Verify notFoundCache was set (not deleted)
	_, err = notFoundCache.Get(ctx, "key1")
	assert.NoError(t, err, "notFoundCache should be set after Del")

	// Step 5: Get again - should return ErrKeyNotFound from notFoundCache
	// without fetching from upstream
	_, err = client.Get(ctx, "key1")
	assert.True(t, IsErrKeyNotFound(err), "should return ErrKeyNotFound")
	assert.Equal(t, 1, fetchCount, "should not fetch from upstream, use notFoundCache")

	// Verify it's a cached response
	var knfErr *ErrKeyNotFound
	require.True(t, errors.As(err, &knfErr), "should be ErrKeyNotFound")
	assert.True(t, knfErr.Cached, "should be cached from notFoundCache")
}

func TestDoFetchDoesNotTouchUpstream(t *testing.T) {
	t.Run("setWithoutUpstream after successful fetch", func(t *testing.T) {
		ctx := context.Background()
		backend := newRistrettoCache[string](t)
		upstream := newRistrettoCache[string](t)

		// Pre-populate upstream with data
		err := upstream.Set(ctx, "key1", "value-from-source")
		require.NoError(t, err)

		// Track upstream cache operations
		upstreamSetCalled := false
		upstreamDelCalled := false

		// Create tracked upstream that implements Cache[T]
		trackedUpstream := &trackedCache[string]{
			onGet: func(key string) (string, error) {
				return upstream.Get(ctx, key)
			},
			onSet: func(key string, value string) error {
				upstreamSetCalled = true
				return upstream.Set(ctx, key, value)
			},
			onDel: func(key string) error {
				upstreamDelCalled = true
				return upstream.Del(ctx, key)
			},
		}

		client := NewClient(backend, trackedUpstream)
		defer func() {
			assert.NoError(t, client.Close())
		}()

		// Fetch from upstream
		value, err := client.Get(ctx, "key1")
		require.NoError(t, err)
		assert.Equal(t, "value-from-source", value)

		// Verify upstream cache was NOT set during fetch
		assert.False(t, upstreamSetCalled, "upstream cache should not be set during doFetch")
		assert.False(t, upstreamDelCalled, "upstream cache should not be deleted during doFetch")

		// Verify backend was set
		cachedValue, err := backend.Get(ctx, "key1")
		require.NoError(t, err)
		assert.Equal(t, "value-from-source", cachedValue)

		// Verify upstream cache still has the original data (proving Set was never called to overwrite)
		upstreamValue, err := upstream.Get(ctx, "key1")
		require.NoError(t, err)
		assert.Equal(t, "value-from-source", upstreamValue, "upstream should still have original data")
	})

	t.Run("delWithoutUpstream when upstream returns NotFound", func(t *testing.T) {
		ctx := context.Background()
		clock := NewMockClock(time.Now())
		defer clock.Install()()

		type timestampedValue struct {
			Data      string
			ExpiresAt time.Time
		}

		backend := newRistrettoCache[*timestampedValue](t)
		upstream := newRistrettoCache[*timestampedValue](t)
		notFoundCache := newRistrettoCache[time.Time](t)

		// Pre-populate backend with stale data
		err := backend.Set(ctx, "key1", &timestampedValue{
			Data:      "stale-value",
			ExpiresAt: clock.Now().Add(-10 * time.Millisecond), // Already expired
		})
		require.NoError(t, err)

		// upstream is empty (key1 not found)

		// Track upstream cache operations
		upstreamSetCalled := false
		upstreamDelCalled := false

		// Create tracked upstream that implements Cache[T]
		trackedUpstream := &trackedCache[*timestampedValue]{
			onGet: func(key string) (*timestampedValue, error) {
				return upstream.Get(ctx, key)
			},
			onSet: func(key string, value *timestampedValue) error {
				upstreamSetCalled = true
				return upstream.Set(ctx, key, value)
			},
			onDel: func(key string) error {
				upstreamDelCalled = true
				return upstream.Del(ctx, key)
			},
		}

		// Stale check: fresh for 100ms
		checkStale := func(v *timestampedValue) State {
			if clock.Now().Before(v.ExpiresAt) {
				return StateFresh
			}
			return StateTooStale
		}

		client := NewClient(backend, trackedUpstream,
			WithStale(checkStale),
			WithNotFound[*timestampedValue](notFoundCache, nil),
		)
		defer func() {
			assert.NoError(t, client.Close())
		}()

		// Get should return NotFound (backend has stale data, upstream returns NotFound)
		_, err = client.Get(ctx, "key1")
		assert.True(t, IsErrKeyNotFound(err))

		// Verify upstream cache was NOT modified
		assert.False(t, upstreamSetCalled, "upstream cache should not be set during doFetch")
		assert.False(t, upstreamDelCalled, "upstream cache should not be deleted during doFetch")

		// Verify backend was cleaned
		_, err = backend.Get(ctx, "key1")
		assert.True(t, IsErrKeyNotFound(err))

		// Verify notFoundCache was set
		_, err = notFoundCache.Get(ctx, "key1")
		assert.NoError(t, err, "notFoundCache should be set")

		// Verify upstream cache is still empty (proving Del was never called)
		_, err = upstream.Get(ctx, "key1")
		assert.True(t, IsErrKeyNotFound(err), "upstream cache should still be empty")
	})
}

// trackedCache is a test helper that tracks cache operations
type trackedCache[T any] struct {
	onGet func(key string) (T, error)
	onSet func(key string, value T) error
	onDel func(key string) error
}

func (t *trackedCache[T]) Get(ctx context.Context, key string) (T, error) {
	return t.onGet(key)
}

func (t *trackedCache[T]) Set(ctx context.Context, key string, value T) error {
	return t.onSet(key, value)
}

func (t *trackedCache[T]) Del(ctx context.Context, key string) error {
	return t.onDel(key)
}
