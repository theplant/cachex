package cachex

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newRistrettoCache[T any](tb testing.TB) *RistrettoCache[T] {
	cache, err := NewRistrettoCache[T](DefaultRistrettoCacheConfig[T]())
	require.NoError(tb, err)
	tb.Cleanup(func() { cache.Close() })
	return cache
}

func TestRistrettoCacheBasics(t *testing.T) {
	ctx := context.Background()
	cache := newRistrettoCache[string](t)

	require.NoError(t, cache.Set(ctx, "key1", "value1"))

	value, err := cache.Get(ctx, "key1")
	require.NoError(t, err)
	assert.Equal(t, "value1", value)

	require.NoError(t, cache.Del(ctx, "key1"))

	_, err = cache.Get(ctx, "key1")
	assert.True(t, IsErrKeyNotFound(err))
}
