package cachex

import (
	"context"
	"encoding"
	"encoding/json"
	"time"

	"github.com/pkg/errors"
	"github.com/redis/go-redis/v9"
)

// RedisCache is a cache implementation using Redis
type RedisCache[T any] struct {
	client    redis.UniversalClient
	keyPrefix string
	ttl       time.Duration
	useBinary bool // true if T implements encoding.BinaryMarshaler and encoding.BinaryUnmarshaler
}

var _ Cache[any] = &RedisCache[any]{}

// RedisCacheConfig holds configuration for RedisCache
type RedisCacheConfig struct {
	// Client is the Redis client (supports both single and cluster)
	Client redis.UniversalClient

	// KeyPrefix is the prefix for all keys (optional)
	KeyPrefix string

	// TTL is the time-to-live for cache entries
	// Zero means no expiration
	TTL time.Duration
}

// NewRedisCache creates a new Redis-based cache with configuration
func NewRedisCache[T any](config *RedisCacheConfig) *RedisCache[T] {
	if config.Client == nil {
		panic("Client is required")
	}

	// Check if T is a type that can skip JSON marshaling/unmarshaling
	var zero T
	var useBinary bool

	// Standard practice: MarshalBinary on value receiver, UnmarshalBinary on pointer receiver
	// Only support: T implements BinaryMarshaler, *T implements BinaryUnmarshaler
	_, hasMarshal := any(zero).(encoding.BinaryMarshaler)
	_, hasUnmarshal := any(&zero).(encoding.BinaryUnmarshaler)

	if hasMarshal && hasUnmarshal {
		useBinary = true
	}

	return &RedisCache[T]{
		client:    config.Client,
		keyPrefix: config.KeyPrefix,
		ttl:       config.TTL,
		useBinary: useBinary,
	}
}

func (r *RedisCache[T]) prefixedKey(key string) string {
	return r.keyPrefix + key
}

// Set stores a value in the cache
func (r *RedisCache[T]) Set(ctx context.Context, key string, value T) error {
	var data any
	var err error

	if r.useBinary {
		// Use BinaryMarshaler interface
		if marshaler, ok := any(value).(encoding.BinaryMarshaler); ok {
			data, err = marshaler.MarshalBinary()
			if err != nil {
				return errors.Wrapf(err, "failed to marshal binary for key: %s", key)
			}
		} else {
			return errors.Errorf("value does not implement encoding.BinaryMarshaler for key: %s", key)
		}
	} else {
		switch any(value).(type) {
		case string, []byte:
			data = value
		default:
			// For other types: marshal to JSON
			data, err = json.Marshal(value)
			if err != nil {
				return errors.Wrapf(err, "failed to marshal value for key: %s", key)
			}
		}
	}

	if err := r.client.Set(ctx, r.prefixedKey(key), data, r.ttl).Err(); err != nil {
		return errors.Wrapf(err, "failed to set cache entry for key: %s", key)
	}

	return nil
}

func (r *RedisCache[T]) handleRedisError(err error, key string) error {
	if errors.Is(err, redis.Nil) {
		return errors.Wrapf(&ErrKeyNotFound{}, "key not found in redis cache for key: %s", key)
	}
	return errors.Wrapf(err, "failed to get cache entry for key: %s", key)
}

// Get retrieves a value from the cache
func (r *RedisCache[T]) Get(ctx context.Context, key string) (T, error) {
	var zero T
	cmd := r.client.Get(ctx, r.prefixedKey(key))

	if _, ok := any(zero).(string); ok {
		str, err := cmd.Result()
		if err != nil {
			return zero, r.handleRedisError(err, key)
		}
		return any(str).(T), nil
	}

	data, err := cmd.Bytes()
	if err != nil {
		return zero, r.handleRedisError(err, key)
	}

	if _, ok := any(zero).([]byte); ok {
		return any(data).(T), nil
	}

	if r.useBinary {
		var value T
		if unmarshaler, ok := any(&value).(encoding.BinaryUnmarshaler); ok {
			if err := unmarshaler.UnmarshalBinary(data); err != nil {
				return zero, errors.Wrapf(err, "failed to unmarshal binary for key: %s", key)
			}
		}
		return value, nil
	}

	// For other types: unmarshal from JSON
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		return zero, errors.Wrapf(err, "failed to unmarshal value for key: %s", key)
	}

	return value, nil
}

// Del removes a value from the cache
func (r *RedisCache[T]) Del(ctx context.Context, key string) error {
	if err := r.client.Del(ctx, r.prefixedKey(key)).Err(); err != nil {
		return errors.Wrapf(err, "failed to delete cache entry for key: %s", key)
	}
	return nil
}
