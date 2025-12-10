package cachex

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSyncMapBasics(t *testing.T) {
	ctx := context.Background()
	cache := NewSyncMap[string]()

	require.NoError(t, cache.Set(ctx, "key1", "value1"))

	value, err := cache.Get(ctx, "key1")
	require.NoError(t, err)
	assert.Equal(t, "value1", value)

	require.NoError(t, cache.Del(ctx, "key1"))

	_, err = cache.Get(ctx, "key1")
	assert.True(t, IsErrKeyNotFound(err))
}

func TestSyncMapEmbeddedMethods(t *testing.T) {
	cache := NewSyncMap[int]()

	cache.Store("a", 1)
	cache.Store("b", 2)

	v, ok := cache.Load("a")
	require.True(t, ok)
	assert.Equal(t, 1, v)

	count := 0
	cache.Range(func(k, v any) bool {
		count++
		return true
	})
	assert.Equal(t, 2, count)
}
