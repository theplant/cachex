package cachex

import (
	"context"
	"strconv"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Age  int    `json:"age"`
}

func TestTransformCacheBasics(t *testing.T) {
	ctx := context.Background()

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
		MaintNotificationsConfig: &maintnotifications.Config{
			Mode: "disabled",
		},
	})
	t.Cleanup(func() { client.Close() })

	// Create a Cache[string]
	stringCache := NewRedisCache[string](&RedisCacheConfig{
		Client: client,
	})

	// Transform to Cache[int] using custom encode/decode
	intCache := Transform(
		stringCache,
		func(i int) (string, error) {
			return strconv.Itoa(i), nil
		},
		func(s string) (int, error) {
			return strconv.Atoi(s)
		},
	)

	// Test Set and Get
	require.NoError(t, intCache.Set(ctx, "age", 42))

	age, err := intCache.Get(ctx, "age")
	require.NoError(t, err)
	assert.Equal(t, 42, age)

	// Verify underlying storage
	rawValue, err := mr.Get("age")
	require.NoError(t, err)
	assert.Equal(t, "42", rawValue)

	// Test Del
	require.NoError(t, intCache.Del(ctx, "age"))
	_, err = intCache.Get(ctx, "age")
	assert.True(t, IsErrKeyNotFound(err))
}

func TestJSONTransform(t *testing.T) {
	ctx := context.Background()

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
		MaintNotificationsConfig: &maintnotifications.Config{
			Mode: "disabled",
		},
	})
	t.Cleanup(func() { client.Close() })

	// Create a Cache[[]byte]
	byteCache := NewRedisCache[[]byte](&RedisCacheConfig{
		Client: client,
	})

	// Transform to Cache[User] using JSON
	userCache := JSONTransform[User](byteCache)

	user := User{
		ID:   "user-123",
		Name: "Alice",
		Age:  30,
	}

	// Test Set and Get
	require.NoError(t, userCache.Set(ctx, "user:1", user))

	retrieved, err := userCache.Get(ctx, "user:1")
	require.NoError(t, err)
	assert.Equal(t, user.ID, retrieved.ID)
	assert.Equal(t, user.Name, retrieved.Name)
	assert.Equal(t, user.Age, retrieved.Age)

	// Verify underlying storage is JSON
	rawValue, err := mr.Get("user:1")
	require.NoError(t, err)
	assert.Contains(t, rawValue, `"id"`)
	assert.Contains(t, rawValue, `"name"`)
	assert.Contains(t, rawValue, `"age"`)
}

func TestStringJSONTransform(t *testing.T) {
	ctx := context.Background()

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
		MaintNotificationsConfig: &maintnotifications.Config{
			Mode: "disabled",
		},
	})
	t.Cleanup(func() { client.Close() })

	// Create a Cache[string]
	stringCache := NewRedisCache[string](&RedisCacheConfig{
		Client: client,
	})

	// Transform to Cache[User] using JSON
	userCache := StringJSONTransform[User](stringCache)

	user := User{
		ID:   "user-456",
		Name: "Bob",
		Age:  25,
	}

	// Test Set and Get
	require.NoError(t, userCache.Set(ctx, "user:2", user))

	retrieved, err := userCache.Get(ctx, "user:2")
	require.NoError(t, err)
	assert.Equal(t, user.ID, retrieved.ID)
	assert.Equal(t, user.Name, retrieved.Name)
	assert.Equal(t, user.Age, retrieved.Age)

	// Verify underlying storage is JSON string
	rawValue, err := mr.Get("user:2")
	require.NoError(t, err)
	assert.Contains(t, rawValue, `"id"`)
	assert.Contains(t, rawValue, `"name"`)
	assert.Contains(t, rawValue, `"age"`)
}

func TestTransformCacheErrors(t *testing.T) {
	ctx := context.Background()

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
		MaintNotificationsConfig: &maintnotifications.Config{
			Mode: "disabled",
		},
	})
	t.Cleanup(func() { client.Close() })

	stringCache := NewRedisCache[string](&RedisCacheConfig{
		Client: client,
	})

	// Create a transform that always fails on encode
	badCache := Transform(
		stringCache,
		func(i int) (string, error) {
			return "", assert.AnError
		},
		func(s string) (int, error) {
			return strconv.Atoi(s)
		},
	)

	// Set should fail due to encode error
	err := badCache.Set(ctx, "key", 42)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to encode value")

	// Store a valid value directly
	require.NoError(t, stringCache.Set(ctx, "invalid", "not-a-number"))

	// Create a transform that always fails on decode
	badDecodeCache := Transform(
		stringCache,
		func(i int) (string, error) {
			return strconv.Itoa(i), nil
		},
		func(s string) (int, error) {
			return 0, assert.AnError
		},
	)

	// Get should fail due to decode error
	_, err = badDecodeCache.Get(ctx, "invalid")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to decode value")
}
