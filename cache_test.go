package cachex_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

func TestGetFetchesAMissAndBackfillsIt(t *testing.T) {
	ctx := context.Background()
	src := newSource(map[string]string{"a": "1"})
	mem := cachextest.NewMap[string]()
	_, now := newClock()
	c := cachex.New(src, []cachex.Layer[string]{
		cachex.NewLayer(mem, cachex.TTL(time.Minute, 0), cachex.Jitter(0)),
	}, now)

	v, err := c.Get(ctx, "a")
	require.NoError(t, err)
	assert.Equal(t, "1", v)
	e, ok := stored(t, mem, "a")
	require.True(t, ok, "the fetched value is backfilled")
	assert.Equal(t, cachex.Entry[string]{
		Value:      "1",
		CachedAt:   epoch,
		FreshUntil: epoch.Add(time.Minute),
		ExpiresAt:  epoch.Add(time.Minute),
	}, e)

	v, err = c.Get(ctx, "a")
	require.NoError(t, err)
	assert.Equal(t, "1", v)
	assert.EqualValues(t, 1, src.calls.Load(), "the second Get hits the layer")
}

func TestGetOfAMissingKey(t *testing.T) {
	ctx := context.Background()

	t.Run("not-found is not cached without a not-found TTL", func(t *testing.T) {
		src := newSource(nil)
		mem := cachextest.NewMap[string]()
		c := cachex.New(src, []cachex.Layer[string]{cachex.NewLayer(mem, cachex.TTL(time.Minute, 0))})
		for range 2 {
			_, err := c.Get(ctx, "x")
			assert.ErrorIs(t, err, cachex.ErrNotFound)
		}
		assert.EqualValues(t, 2, src.calls.Load())
		assert.Zero(t, mem.Len())
	})

	t.Run("not-found is cached with a not-found TTL", func(t *testing.T) {
		src := newSource(nil)
		mem := cachextest.NewMap[string]()
		_, now := newClock()
		c := cachex.New(src, []cachex.Layer[string]{
			cachex.NewLayer(mem, cachex.TTL(time.Minute, 0), cachex.NotFoundTTL(time.Second, 0), cachex.Jitter(0)),
		}, now)
		for range 2 {
			_, err := c.Get(ctx, "x")
			assert.ErrorIs(t, err, cachex.ErrNotFound)
		}
		assert.EqualValues(t, 1, src.calls.Load())
		e, ok := stored(t, mem, "x")
		require.True(t, ok)
		assert.Equal(t, cachex.Entry[string]{
			NotFound:   true,
			CachedAt:   epoch,
			FreshUntil: epoch.Add(time.Second),
			ExpiresAt:  epoch.Add(time.Second),
		}, e)
	})

	t.Run("a wrapped ErrNotFound from the source counts", func(t *testing.T) {
		src := newSource(nil)
		src.fail["x"] = errors.Join(errors.New("no such row"), cachex.ErrNotFound)
		mem := cachextest.NewMap[string]()
		c := cachex.New(src, []cachex.Layer[string]{
			cachex.NewLayer(mem, cachex.TTL(time.Minute, 0), cachex.NotFoundTTL(time.Second, 0)),
		})
		_, err := c.Get(ctx, "x")
		assert.ErrorIs(t, err, cachex.ErrNotFound)
		e, ok := stored(t, mem, "x")
		require.True(t, ok)
		assert.True(t, e.NotFound)
	})
}

func TestGetSourceErrorsAreReturnedAndNotCached(t *testing.T) {
	ctx := context.Background()
	src := newSource(map[string]string{"a": "1"})
	src.fail["a"] = errBoom
	mem := cachextest.NewMap[string]()
	c := cachex.New(src, []cachex.Layer[string]{
		cachex.NewLayer(mem, cachex.TTL(time.Minute, 0), cachex.NotFoundTTL(time.Minute, 0)),
	})

	_, err := c.Get(ctx, "a")
	require.ErrorIs(t, err, errBoom)
	assert.NotErrorIs(t, err, cachex.ErrNotFound)
	assert.Zero(t, mem.Len())

	delete(src.fail, "a")
	v, err := c.Get(ctx, "a")
	require.NoError(t, err)
	assert.Equal(t, "1", v)
}

func TestGetFailsWhenALayerCannotBeRead(t *testing.T) {
	ctx := context.Background()
	src := newSource(map[string]string{"a": "1"})
	mem := newFaulty[string](cachextest.NewMap[string]())
	mem.failOn("get", errBoom)
	c := cachex.New(src, []cachex.Layer[string]{cachex.NewLayer(mem, cachex.TTL(time.Minute, 0))})

	_, err := c.Get(ctx, "a")
	require.ErrorIs(t, err, errBoom)
	assert.Zero(t, src.calls.Load(), "a broken layer does not send every read to the source")
}

func TestGetWithoutLayersAsksTheSource(t *testing.T) {
	src := newSource(map[string]string{"a": "1"})
	c := cachex.New[string](src, nil)
	v, err := c.Get(context.Background(), "a")
	require.NoError(t, err)
	assert.Equal(t, "1", v)
	_, err = c.Get(context.Background(), "x")
	assert.ErrorIs(t, err, cachex.ErrNotFound)
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	src := newSource(nil)
	mem := cachextest.NewMap[string]()
	cases := map[string]func(){
		"nil source":            func() { cachex.New[string](nil, nil) },
		"nil backend":           func() { cachex.NewLayer[string](nil, cachex.TTL(time.Second, 0)) },
		"no TTL":                func() { cachex.New(src, []cachex.Layer[string]{cachex.NewLayer(mem)}) },
		"negative stale TTL":    func() { cachex.NewLayer(mem, cachex.TTL(time.Second, -1)) },
		"negative not-found":    func() { cachex.NewLayer(mem, cachex.TTL(time.Second, 0), cachex.NotFoundTTL(-1, 0)) },
		"jitter of 1":           func() { cachex.NewLayer(mem, cachex.TTL(time.Second, 0), cachex.Jitter(1)) },
		"stale not-found only":  func() { cachex.NewLayer(mem, cachex.TTL(time.Second, 0), cachex.NotFoundTTL(0, time.Second)) },
		"nil logger":            func() { cachex.New[string](src, nil, cachex.WithLogger(nil)) },
		"nil clock":             func() { cachex.New[string](src, nil, cachex.WithNow(nil)) },
		"zero fetch timeout":    func() { cachex.New[string](src, nil, cachex.WithFetchTimeout(0)) },
		"zero fetches per key":  func() { cachex.New[string](src, nil, cachex.WithFetchesPerKey(0)) },
		"zero GetMany conc":     func() { cachex.New[string](src, nil, cachex.WithGetManyConcurrency(0)) },
		"negative chunk size":   func() { cachex.New[string](src, nil, cachex.WithGetManyChunkSize(-1)) },
		"max age of other type": func() { cachex.New[string](src, nil, cachex.WithMaxAge(func(int) time.Duration { return 0 })) },
	}
	for name, f := range cases {
		assert.Panics(t, f, name)
	}
}
