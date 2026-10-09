package cachex

import (
	"context"
	"encoding"
	"encoding/json"
	stderrors "errors"
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

var _ BatchCache[any] = &RedisCache[any]{}

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

func (r *RedisCache[T]) encode(key string, value T) (any, error) {
	if r.useBinary {
		// Use BinaryMarshaler interface
		marshaler, ok := any(value).(encoding.BinaryMarshaler)
		if !ok {
			return nil, errors.Errorf("value does not implement encoding.BinaryMarshaler for key: %s", key)
		}
		data, err := marshaler.MarshalBinary()
		if err != nil {
			return nil, errors.Wrapf(err, "failed to marshal binary for key: %s", key)
		}
		return data, nil
	}

	switch any(value).(type) {
	case string, []byte:
		return value, nil
	default:
		// For other types: marshal to JSON
		data, err := json.Marshal(value)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to marshal value for key: %s", key)
		}
		return data, nil
	}
}

// decode reads a GET reply. cmd.Bytes() shares the reply's memory instead of
// copying it; the cmd is discarded afterwards, so nothing else holds it.
func (r *RedisCache[T]) decode(key string, cmd *redis.StringCmd) (T, error) {
	var zero T

	if _, ok := any(zero).(string); ok {
		return any(cmd.Val()).(T), nil
	}
	data, _ := cmd.Bytes()
	if _, ok := any(zero).([]byte); ok {
		return any(data).(T), nil
	}

	var value T
	if r.useBinary {
		if unmarshaler, ok := any(&value).(encoding.BinaryUnmarshaler); ok {
			if err := unmarshaler.UnmarshalBinary(data); err != nil {
				return zero, errors.Wrapf(err, "failed to unmarshal binary for key: %s", key)
			}
		}
		return value, nil
	}

	// For other types: unmarshal from JSON
	if err := json.Unmarshal(data, &value); err != nil {
		return zero, errors.Wrapf(err, "failed to unmarshal value for key: %s", key)
	}
	return value, nil
}

// Set stores a value in the cache
func (r *RedisCache[T]) Set(ctx context.Context, key string, value T) error {
	data, err := r.encode(key, value)
	if err != nil {
		return err
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
	if err := cmd.Err(); err != nil {
		return zero, r.handleRedisError(err, key)
	}
	return r.decode(key, cmd)
}

// Del removes a value from the cache
func (r *RedisCache[T]) Del(ctx context.Context, key string) error {
	if err := r.client.Del(ctx, r.prefixedKey(key)).Err(); err != nil {
		return errors.Wrapf(err, "failed to delete cache entry for key: %s", key)
	}
	return nil
}

// GetMany retrieves many values in one round trip (a pipeline of GETs, which,
// unlike MGET, also works across slots on Redis Cluster).
// Missing keys are absent from the result; a value that fails to decode is
// reported in a *BatchError without hiding the others.
func (r *RedisCache[T]) GetMany(ctx context.Context, keys []string) (map[string]T, error) {
	out := make(map[string]T, len(keys))
	if len(keys) == 0 {
		return out, nil
	}

	pipe := r.client.Pipeline()
	cmds := make([]*redis.StringCmd, len(keys))
	for i, key := range keys {
		cmds[i] = pipe.Get(ctx, r.prefixedKey(key))
	}
	// Exec reports only the first failed command; every command carries its own
	// error (a connection failure is set on all of them), so read them per key.
	_, _ = pipe.Exec(ctx)

	var keyErrs map[string]error
	for i, key := range keys {
		err := cmds[i].Err()
		if errors.Is(err, redis.Nil) {
			continue
		}
		var value T
		if err == nil {
			value, err = r.decode(key, cmds[i])
		} else {
			err = r.handleRedisError(err, key)
		}
		if err != nil {
			if keyErrs == nil {
				keyErrs = map[string]error{}
			}
			keyErrs[key] = err
			continue
		}
		out[key] = value
	}
	if keyErrs != nil {
		return out, &BatchError{Errors: keyErrs}
	}
	return out, nil
}

// SetMany stores many values in one round trip (a pipeline of SETs with the configured TTL)
func (r *RedisCache[T]) SetMany(ctx context.Context, values map[string]T) error {
	if len(values) == 0 {
		return nil
	}

	// A value that fails to encode is reported without holding back the others.
	var encodeErrs []error
	pipe := r.client.Pipeline()
	for key, value := range values {
		data, err := r.encode(key, value)
		if err != nil {
			encodeErrs = append(encodeErrs, err)
			continue
		}
		pipe.Set(ctx, r.prefixedKey(key), data, r.ttl)
	}
	if pipe.Len() > 0 {
		if _, err := pipe.Exec(ctx); err != nil {
			return errors.Wrap(err, "failed to set cache entries")
		}
	}
	return stderrors.Join(encodeErrs...)
}

// DelMany removes many keys in one round trip
func (r *RedisCache[T]) DelMany(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}

	pipe := r.client.Pipeline()
	for _, key := range keys {
		pipe.Del(ctx, r.prefixedKey(key))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return errors.Wrap(err, "failed to delete cache entries")
	}
	return nil
}
