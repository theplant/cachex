// Package rediscachex is a cachex backend on Redis (or Redis Cluster): each
// entry is one string key, expiring natively at the entry's ExpiresAt. Batch
// calls are pipelines of GET, SET and DEL (which, unlike MGET, also work across
// Cluster slots), ChunkSize keys each.
package rediscachex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/theplant/cachex/v2"
)

// DefaultChunkSize is how many keys one pipeline carries by default.
const DefaultChunkSize = 1000

// Config configures a Backend.
type Config[T any] struct {
	// Client is the Redis client. Required.
	Client redis.UniversalClient
	// KeyPrefix is prepended to every key, so that one Redis can hold several
	// caches. Change it when the value type changes incompatibly: JSON
	// decodes leniently, so a renamed field reads back as a zero value rather
	// than failing.
	KeyPrefix string
	// ChunkSize is how many keys one pipeline carries. Default DefaultChunkSize.
	ChunkSize int
	// Codec encodes values. Default cachex.DefaultCodec.
	Codec cachex.Codec[T]
	// Logger reports dropped entries that do not decode. Default slog.Default().
	Logger *slog.Logger
}

// Backend is a Redis-backed cachex.Backend.
type Backend[T any] struct {
	client    redis.UniversalClient
	prefix    string
	chunkSize int
	codec     cachex.Codec[T]
	logger    *slog.Logger
}

var _ cachex.Backend[any] = (*Backend[any])(nil)

// New returns a Backend on cfg.Client.
func New[T any](cfg Config[T]) *Backend[T] {
	if cfg.Client == nil {
		panic("rediscachex: Client is required")
	}
	b := &Backend[T]{client: cfg.Client, prefix: cfg.KeyPrefix, chunkSize: cfg.ChunkSize, codec: cfg.Codec, logger: cfg.Logger}
	if b.chunkSize <= 0 {
		b.chunkSize = DefaultChunkSize
	}
	if b.codec == nil {
		b.codec = cachex.DefaultCodec[T]()
	}
	if b.logger == nil {
		b.logger = slog.Default()
	}
	return b
}

// decode reads a GET reply; data that does not decode is dropped and read as
// a miss (it was written by something else, or before the type changed).
// cmd.Bytes() shares the reply's memory rather than copying it; the cmd is
// discarded afterwards.
func (b *Backend[T]) decode(ctx context.Context, key string, cmd *redis.StringCmd) (cachex.Entry[T], bool) {
	data, _ := cmd.Bytes()
	e, err := cachex.DecodeEntry(b.codec, data)
	if err != nil {
		b.logger.WarnContext(ctx, "rediscachex: dropped an entry that does not decode", "key", key, "error", err)
		_ = b.client.Del(ctx, b.prefix+key).Err()
		return cachex.Entry[T]{}, false
	}
	return e, true
}

func (b *Backend[T]) Get(ctx context.Context, key string) (cachex.Entry[T], bool, error) {
	cmd := b.client.Get(ctx, b.prefix+key)
	if err := cmd.Err(); err != nil {
		if errors.Is(err, redis.Nil) {
			return cachex.Entry[T]{}, false, nil
		}
		return cachex.Entry[T]{}, false, fmt.Errorf("rediscachex: get %q: %w", key, err)
	}
	e, ok := b.decode(ctx, key, cmd)
	return e, ok, nil
}

func (b *Backend[T]) GetMany(ctx context.Context, keys []string) (map[string]cachex.Entry[T], error) {
	out := make(map[string]cachex.Entry[T], len(keys))
	errs := map[string]error{}
	for chunk := range slices.Chunk(keys, b.chunkSize) {
		pipe := b.client.Pipeline()
		cmds := make([]*redis.StringCmd, len(chunk))
		for i, key := range chunk {
			cmds[i] = pipe.Get(ctx, b.prefix+key)
		}
		execPipe(ctx, pipe)
		for i, key := range chunk {
			switch err := cmds[i].Err(); {
			case errors.Is(err, redis.Nil):
			case err != nil:
				errs[key] = fmt.Errorf("rediscachex: get %q: %w", key, err)
			default:
				if e, ok := b.decode(ctx, key, cmds[i]); ok {
					out[key] = e
				}
			}
		}
	}
	return out, batchError(errs)
}

func (b *Backend[T]) Set(ctx context.Context, key string, e cachex.Entry[T]) error {
	return b.SetMany(ctx, map[string]cachex.Entry[T]{key: e})
}

// SetMany stores entries with a TTL up to their ExpiresAt; an entry already
// expired deletes the key instead.
func (b *Backend[T]) SetMany(ctx context.Context, entries map[string]cachex.Entry[T]) error {
	errs := map[string]error{}
	now := time.Now()
	for chunk := range slices.Chunk(slices.Collect(maps.Keys(entries)), b.chunkSize) {
		pipe := b.client.Pipeline()
		sent := make([]string, 0, len(chunk))
		cmds := make([]redis.Cmder, 0, len(chunk))
		for _, key := range chunk {
			e := entries[key]
			ttl := e.ExpiresAt.Sub(now)
			if ttl <= 0 {
				cmds = append(cmds, pipe.Del(ctx, b.prefix+key))
				sent = append(sent, key)
				continue
			}
			data, err := cachex.EncodeEntry(b.codec, e)
			if err != nil {
				errs[key] = err
				continue
			}
			cmds = append(cmds, pipe.Set(ctx, b.prefix+key, data, ttl))
			sent = append(sent, key)
		}
		if len(cmds) == 0 {
			continue
		}
		execPipe(ctx, pipe)
		for i, cmd := range cmds {
			if err := cmd.Err(); err != nil {
				errs[sent[i]] = fmt.Errorf("rediscachex: set %q: %w", sent[i], err)
			}
		}
	}
	return batchError(errs)
}

func (b *Backend[T]) Del(ctx context.Context, key string) error {
	if err := b.client.Del(ctx, b.prefix+key).Err(); err != nil {
		return fmt.Errorf("rediscachex: delete %q: %w", key, err)
	}
	return nil
}

func (b *Backend[T]) DelMany(ctx context.Context, keys []string) error {
	errs := map[string]error{}
	for chunk := range slices.Chunk(keys, b.chunkSize) {
		pipe := b.client.Pipeline()
		cmds := make([]*redis.IntCmd, len(chunk))
		for i, key := range chunk {
			cmds[i] = pipe.Del(ctx, b.prefix+key)
		}
		execPipe(ctx, pipe)
		for i, cmd := range cmds {
			if err := cmd.Err(); err != nil {
				errs[chunk[i]] = fmt.Errorf("rediscachex: delete %q: %w", chunk[i], err)
			}
		}
	}
	return batchError(errs)
}

// execPipe runs a pipeline so that every command carries its own outcome.
// Exec returns only the first failed command's error. Errors replied by Redis
// are already set on their commands, but commands never sent (the connection
// could not be made, say) carry no error, and a GET would read as an empty
// value: set the pipeline's error on them.
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

func batchError(errs map[string]error) error {
	if len(errs) == 0 {
		return nil
	}
	return &cachex.BatchError{Errors: errs}
}
