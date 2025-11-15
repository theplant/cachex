package cachex

import (
	"context"
	"testing"
	"time"

	"github.com/allegro/bigcache/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newBigCache(tb testing.TB) *BigCache {
	cache, err := NewBigCache(context.Background(), BigCacheConfig{
		Config: bigcache.Config{
			Shards:             16,
			LifeWindow:         1 * time.Minute,
			CleanWindow:        1 * time.Second,
			MaxEntriesInWindow: 1000,
			MaxEntrySize:       100,
		},
	})
	require.NoError(tb, err)
	tb.Cleanup(func() { cache.Close() })
	return cache
}

func TestBigCacheBasics(t *testing.T) {
	ctx := context.Background()
	cache := newBigCache(t)

	require.NoError(t, cache.Set(ctx, "key1", []byte("value1")))

	value, err := cache.Get(ctx, "key1")
	require.NoError(t, err)
	assert.Equal(t, []byte("value1"), value)

	require.NoError(t, cache.Del(ctx, "key1"))

	_, err = cache.Get(ctx, "key1")
	assert.True(t, IsErrKeyNotFound(err))
}
