package cachex

import (
	"context"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisCache is a cache implementation using Redis
type RedisCache[T any] struct {
	client    redis.UniversalClient
	keyPrefix string
	ttl       time.Duration
	useBinary bool // true if T implements encoding.BinaryMarshaler and encoding.BinaryUnmarshaler
	chunkSize int
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

	// ChunkSize is how many commands one pipeline of GetMany, SetMany or
	// DelMany carries; a larger call is sent as several pipelines, one after
	// another. Zero means DefaultChunkSize.
	ChunkSize int
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
		chunkSize: chunkSizeOr(config.ChunkSize, DefaultChunkSize),
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
			return nil, fmt.Errorf("value does not implement encoding.BinaryMarshaler for key: %s", key)
		}
		data, err := marshaler.MarshalBinary()
		if err != nil {
			return nil, fmt.Errorf("failed to marshal binary for key: %s: %w", key, err)
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
			return nil, fmt.Errorf("failed to marshal value for key: %s: %w", key, err)
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
				return zero, fmt.Errorf("failed to unmarshal binary for key: %s: %w", key, err)
			}
		}
		return value, nil
	}

	// For other types: unmarshal from JSON
	if err := json.Unmarshal(data, &value); err != nil {
		return zero, fmt.Errorf("failed to unmarshal value for key: %s: %w", key, err)
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
		return fmt.Errorf("failed to set cache entry for key: %s: %w", key, err)
	}

	return nil
}

func (r *RedisCache[T]) handleRedisError(err error, key string) error {
	if errors.Is(err, redis.Nil) {
		return fmt.Errorf("key not found in redis cache for key: %s: %w", key, &ErrKeyNotFound{})
	}
	return fmt.Errorf("failed to get cache entry for key: %s: %w", key, err)
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
		return fmt.Errorf("failed to delete cache entry for key: %s: %w", key, err)
	}
	return nil
}

// execPipe runs a pipeline so that every command carries its own outcome.
// Exec returns only the first failed command's error. Errors replied by Redis
// are already set on their commands, but commands never sent (e.g. the
// connection could not be made) carry no error, and a GET would read as an
// empty value: set the pipeline's error on them.
func execPipe(ctx context.Context, pipe redis.Pipeliner) {
	cmds, err := pipe.Exec(ctx)
	var replied redis.Error
	if err == nil || errors.As(err, &replied) {
		return
	}
	for _, cmd := range cmds {
		if cmd.Err() == nil {
			cmd.SetErr(err)
		}
	}
}

// GetMany retrieves many values with pipelines of GETs (which, unlike MGET,
// also work across slots on Redis Cluster), ChunkSize keys each.
// Missing keys are absent from the result; a key that fails (to be read or
// decoded) is reported in a *BatchError without hiding the others.
func (r *RedisCache[T]) GetMany(ctx context.Context, keys []string) (map[string]T, error) {
	out := make(map[string]T, len(keys))
	keyErrs := map[string]error{}
	for chunk := range slices.Chunk(keys, r.chunkSize) {
		pipe := r.client.Pipeline()
		cmds := make([]*redis.StringCmd, len(chunk))
		for i, key := range chunk {
			cmds[i] = pipe.Get(ctx, r.prefixedKey(key))
		}
		execPipe(ctx, pipe)

		for i, key := range chunk {
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
				keyErrs[key] = err
				continue
			}
			out[key] = value
		}
	}
	return out, batchError(keyErrs)
}

// SetMany stores many values with pipelines of SETs (with the configured TTL),
// ChunkSize keys each. It is best effort: every key is tried, and the keys that
// failed (to be encoded or written) are reported in a *BatchError.
func (r *RedisCache[T]) SetMany(ctx context.Context, values map[string]T) error {
	keyErrs := map[string]error{}
	for chunk := range slices.Chunk(slices.Collect(maps.Keys(values)), r.chunkSize) {
		pipe := r.client.Pipeline()
		sent := make([]string, 0, len(chunk))
		cmds := make([]*redis.StatusCmd, 0, len(chunk))
		for _, key := range chunk {
			data, err := r.encode(key, values[key])
			if err != nil {
				keyErrs[key] = err
				continue
			}
			sent = append(sent, key)
			cmds = append(cmds, pipe.Set(ctx, r.prefixedKey(key), data, r.ttl))
		}
		if len(cmds) == 0 {
			continue
		}
		execPipe(ctx, pipe)
		for i, cmd := range cmds {
			if err := cmd.Err(); err != nil {
				keyErrs[sent[i]] = fmt.Errorf("failed to set cache entry for key: %s: %w", sent[i], err)
			}
		}
	}
	return batchError(keyErrs)
}

// DelMany removes many keys with pipelines of DELs, ChunkSize keys each. It is
// best effort: every key is tried, and the keys that failed are reported in a
// *BatchError.
func (r *RedisCache[T]) DelMany(ctx context.Context, keys []string) error {
	keyErrs := map[string]error{}
	for chunk := range slices.Chunk(keys, r.chunkSize) {
		pipe := r.client.Pipeline()
		cmds := make([]*redis.IntCmd, len(chunk))
		for i, key := range chunk {
			cmds[i] = pipe.Del(ctx, r.prefixedKey(key))
		}
		execPipe(ctx, pipe)
		for i, cmd := range cmds {
			if err := cmd.Err(); err != nil {
				keyErrs[chunk[i]] = fmt.Errorf("failed to delete cache entry for key: %s: %w", chunk[i], err)
			}
		}
	}
	return batchError(keyErrs)
}
