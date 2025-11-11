package cachex

import (
	"context"
	"fmt"
	"testing"
	"time"

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
	l1Client := NewClient(l1, l2Client)

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
