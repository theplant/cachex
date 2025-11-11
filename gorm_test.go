package cachex

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newGORMCache[T any](tb testing.TB, tableName string) *GORMCache[T] {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(tb, err)
	cache := NewGORMCache[T](&GORMCacheConfig{
		DB:        db,
		TableName: tableName,
	})
	require.NoError(tb, cache.Migrate(context.Background()))
	return cache
}

func TestGORMCacheBasics(t *testing.T) {
	ctx := context.Background()
	cache := newGORMCache[string](t, "test_cache")

	require.NoError(t, cache.Set(ctx, "key1", "value1"))

	value, err := cache.Get(ctx, "key1")
	require.NoError(t, err)
	assert.Equal(t, "value1", value)

	require.NoError(t, cache.Del(ctx, "key1"))

	_, err = cache.Get(ctx, "key1")
	assert.True(t, IsErrKeyNotFound(err))
}

func TestGORMCacheWithBytes(t *testing.T) {
	ctx := context.Background()
	cache := newGORMCache[[]byte](t, "bytes_cache")

	testData := []byte("raw binary data \x00\x01\x02")

	require.NoError(t, cache.Set(ctx, "key1", testData))

	value, err := cache.Get(ctx, "key1")
	require.NoError(t, err)
	assert.Equal(t, testData, value)

	require.NoError(t, cache.Del(ctx, "key1"))

	_, err = cache.Get(ctx, "key1")
	assert.True(t, IsErrKeyNotFound(err))
}

func TestGORMCacheConfigWithPrefix(t *testing.T) {
	ctx := context.Background()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)

	prodCache := NewGORMCache[string](&GORMCacheConfig{
		DB:        db,
		TableName: "config_cache",
		KeyPrefix: "prod:",
	})
	require.NoError(t, prodCache.Migrate(ctx))

	devCache := NewGORMCache[string](&GORMCacheConfig{
		DB:        db,
		TableName: "config_cache",
		KeyPrefix: "dev:",
	})

	require.NoError(t, prodCache.Set(ctx, "api_key", "prod-secret-123"))
	require.NoError(t, devCache.Set(ctx, "api_key", "dev-secret-456"))

	prodValue, err := prodCache.Get(ctx, "api_key")
	require.NoError(t, err)
	assert.Equal(t, "prod-secret-123", prodValue)

	devValue, err := devCache.Get(ctx, "api_key")
	require.NoError(t, err)
	assert.Equal(t, "dev-secret-456", devValue)
}
