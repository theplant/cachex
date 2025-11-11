package cachex

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/pkg/errors"
	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newRedisCache[T any](tb testing.TB) (*RedisCache[T], *miniredis.Miniredis) {
	mr := miniredis.RunT(tb)

	client := redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
		MaintNotificationsConfig: &maintnotifications.Config{
			Mode: "disabled",
		},
	})

	tb.Cleanup(func() {
		client.Close()
	})

	cache := NewRedisCache[T](&RedisCacheConfig{
		Client: client,
	})

	return cache, mr
}

func TestRedisCacheBasics(t *testing.T) {
	ctx := context.Background()
	cache, mr := newRedisCache[string](t)

	require.NoError(t, cache.Set(ctx, "key1", "value1"))

	value, err := cache.Get(ctx, "key1")
	require.NoError(t, err)
	assert.Equal(t, "value1", value)

	// Verify that string is stored directly without JSON encoding
	rawValue, err := mr.Get("key1")
	require.NoError(t, err)
	assert.Equal(t, "value1", rawValue, "String should be stored as-is without JSON encoding")

	require.NoError(t, cache.Del(ctx, "key1"))

	_, err = cache.Get(ctx, "key1")
	assert.True(t, IsErrKeyNotFound(err))
}

func TestRedisCacheWithBytes(t *testing.T) {
	ctx := context.Background()
	cache, mr := newRedisCache[[]byte](t)

	// Test with raw binary data including null bytes
	testData := []byte("raw binary data \x00\x01\x02\xff\xfe")

	require.NoError(t, cache.Set(ctx, "key1", testData))

	value, err := cache.Get(ctx, "key1")
	require.NoError(t, err)
	assert.Equal(t, testData, value)

	// Verify that data is stored as-is without JSON encoding
	rawValue, err := mr.Get("key1")
	require.NoError(t, err)
	assert.Equal(t, string(testData), rawValue, "Data should be stored as-is without JSON encoding")

	require.NoError(t, cache.Del(ctx, "key1"))

	_, err = cache.Get(ctx, "key1")
	assert.True(t, IsErrKeyNotFound(err))
}

func TestRedisCacheWithStruct(t *testing.T) {
	type User struct {
		Name  string
		Age   int
		Email string
	}

	ctx := context.Background()
	cache, mr := newRedisCache[User](t)

	user := User{
		Name:  "Alice",
		Age:   30,
		Email: "alice@example.com",
	}

	require.NoError(t, cache.Set(ctx, "user:1", user))

	retrieved, err := cache.Get(ctx, "user:1")
	require.NoError(t, err)
	assert.Equal(t, user, retrieved)

	// Verify that struct is stored as JSON
	rawValue, err := mr.Get("user:1")
	require.NoError(t, err)
	assert.Contains(t, rawValue, `"Name":"Alice"`, "Struct should be stored as JSON")
	assert.Contains(t, rawValue, `"Age":30`, "Struct should be stored as JSON")
}

func TestRedisCacheWithTTL(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)

	client := redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
		MaintNotificationsConfig: &maintnotifications.Config{
			Mode: "disabled",
		},
	})
	defer client.Close()

	cache := NewRedisCache[string](&RedisCacheConfig{
		Client: client,
		TTL:    100 * time.Millisecond,
	})

	require.NoError(t, cache.Set(ctx, "expiring-key", "value"))

	value, err := cache.Get(ctx, "expiring-key")
	require.NoError(t, err)
	assert.Equal(t, "value", value)

	// Fast forward time in miniredis
	mr.FastForward(101 * time.Millisecond)

	_, err = cache.Get(ctx, "expiring-key")
	assert.True(t, IsErrKeyNotFound(err), "Key should be expired")
}

func TestRedisCacheConfigWithPrefix(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)

	client := redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
		MaintNotificationsConfig: &maintnotifications.Config{
			Mode: "disabled",
		},
	})
	defer client.Close()

	prodCache := NewRedisCache[string](&RedisCacheConfig{
		Client:    client,
		KeyPrefix: "prod:",
	})

	devCache := NewRedisCache[string](&RedisCacheConfig{
		Client:    client,
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

	// Verify keys are actually stored with prefixes
	keys := mr.Keys()
	assert.Contains(t, keys, "prod:api_key")
	assert.Contains(t, keys, "dev:api_key")
}

func TestRedisCacheUpdate(t *testing.T) {
	ctx := context.Background()
	cache, _ := newRedisCache[string](t)

	require.NoError(t, cache.Set(ctx, "key1", "value1"))

	value, err := cache.Get(ctx, "key1")
	require.NoError(t, err)
	assert.Equal(t, "value1", value)

	// Update the value
	require.NoError(t, cache.Set(ctx, "key1", "value2"))

	value, err = cache.Get(ctx, "key1")
	require.NoError(t, err)
	assert.Equal(t, "value2", value)
}

func TestRedisCacheWithRawMessage(t *testing.T) {
	ctx := context.Background()
	cache, mr := newRedisCache[json.RawMessage](t)

	// json.RawMessage is already valid JSON
	testJSON := json.RawMessage(`{"name":"Alice","age":30,"email":"alice@example.com"}`)

	require.NoError(t, cache.Set(ctx, "raw:1", testJSON))

	retrieved, err := cache.Get(ctx, "raw:1")
	require.NoError(t, err)
	assert.Equal(t, testJSON, retrieved)

	// Verify that json.RawMessage is stored as-is without additional JSON encoding
	rawValue, err := mr.Get("raw:1")
	require.NoError(t, err)
	assert.Equal(t, string(testJSON), rawValue, "json.RawMessage should be stored as-is without double encoding")
	assert.Contains(t, rawValue, `"name":"Alice"`, "Should contain original JSON content")

	// Verify we can unmarshal the retrieved value
	var user struct {
		Name  string `json:"name"`
		Age   int    `json:"age"`
		Email string `json:"email"`
	}
	require.NoError(t, json.Unmarshal(retrieved, &user))
	assert.Equal(t, "Alice", user.Name)
	assert.Equal(t, 30, user.Age)
	assert.Equal(t, "alice@example.com", user.Email)
}

// CustomBinaryType implements encoding.BinaryMarshaler and encoding.BinaryUnmarshaler
type CustomBinaryType struct {
	ID   string
	Data []byte
}

func (c *CustomBinaryType) MarshalBinary() ([]byte, error) {
	// Simple format: ID length (4 bytes) + ID + Data
	idLen := len(c.ID)
	result := make([]byte, 4+idLen+len(c.Data))
	result[0] = byte(idLen >> 24)
	result[1] = byte(idLen >> 16)
	result[2] = byte(idLen >> 8)
	result[3] = byte(idLen)
	copy(result[4:], c.ID)
	copy(result[4+idLen:], c.Data)
	return result, nil
}

func (c *CustomBinaryType) UnmarshalBinary(data []byte) error {
	if len(data) < 4 {
		return errors.New("invalid data: too short")
	}
	idLen := int(data[0])<<24 | int(data[1])<<16 | int(data[2])<<8 | int(data[3])
	if len(data) < 4+idLen {
		return errors.New("invalid data: ID length mismatch")
	}
	c.ID = string(data[4 : 4+idLen])
	c.Data = data[4+idLen:]
	return nil
}

func TestRedisCacheWithBinaryMarshaler(t *testing.T) {
	ctx := context.Background()

	t.Run("non_standard_practice_uses_JSON", func(t *testing.T) {
		// CustomBinaryType implements both BinaryMarshaler/UnmarshalBinary on pointer receiver
		// This does NOT follow standard practice, so it will use JSON
		cache, mr := newRedisCache[CustomBinaryType](t)

		original := CustomBinaryType{
			ID:   "test-123",
			Data: []byte("some raw data \x00\x01\x02"),
		}

		require.NoError(t, cache.Set(ctx, "val:1", original))

		retrieved, err := cache.Get(ctx, "val:1")
		require.NoError(t, err)
		assert.Equal(t, original.ID, retrieved.ID)
		assert.Equal(t, original.Data, retrieved.Data)

		// Verify that it's stored as JSON (not binary)
		rawValue, err := mr.Get("val:1")
		require.NoError(t, err)
		assert.Contains(t, rawValue, `"ID"`, "Should be stored as JSON")
		assert.Contains(t, rawValue, `"Data"`, "Should be stored as JSON")
	})
}

// StandardBinaryType follows standard practice:
// MarshalBinary on value receiver, UnmarshalBinary on pointer receiver
type StandardBinaryType struct {
	X int32
	Y int32
}

func (s StandardBinaryType) MarshalBinary() ([]byte, error) {
	buf := make([]byte, 8)
	buf[0] = byte(s.X >> 24)
	buf[1] = byte(s.X >> 16)
	buf[2] = byte(s.X >> 8)
	buf[3] = byte(s.X)
	buf[4] = byte(s.Y >> 24)
	buf[5] = byte(s.Y >> 16)
	buf[6] = byte(s.Y >> 8)
	buf[7] = byte(s.Y)
	return buf, nil
}

func (s *StandardBinaryType) UnmarshalBinary(data []byte) error {
	if len(data) != 8 {
		return errors.New("invalid data length")
	}
	s.X = int32(data[0])<<24 | int32(data[1])<<16 | int32(data[2])<<8 | int32(data[3])
	s.Y = int32(data[4])<<24 | int32(data[5])<<16 | int32(data[6])<<8 | int32(data[7])
	return nil
}

func TestRedisCacheWithStandardBinaryMarshaler(t *testing.T) {
	ctx := context.Background()

	t.Run("standard_practice_value_type", func(t *testing.T) {
		// StandardBinaryType: MarshalBinary on value receiver, UnmarshalBinary on pointer receiver
		// This is the standard practice
		cache, mr := newRedisCache[StandardBinaryType](t)

		original := StandardBinaryType{X: 100, Y: 200}

		require.NoError(t, cache.Set(ctx, "std:1", original))

		retrieved, err := cache.Get(ctx, "std:1")
		require.NoError(t, err)
		assert.Equal(t, original.X, retrieved.X)
		assert.Equal(t, original.Y, retrieved.Y)

		// Verify that it's stored in binary format (8 bytes)
		rawValue, err := mr.Get("std:1")
		require.NoError(t, err)
		assert.Len(t, rawValue, 8, "Should be stored as 8 bytes")
		assert.NotContains(t, rawValue, `"X"`, "Should be stored in binary format, not JSON")
	})
}
