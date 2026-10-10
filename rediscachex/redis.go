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

// Config configures a backend.
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

// New returns a backend on cfg.Client, its entries encoded by cachex.Encoded
// with cfg.Codec and cfg.Logger.
func New[T any](cfg Config[T]) cachex.Backend[T] {
	if cfg.Client == nil {
		panic("rediscachex: Client is required")
	}
	b := &byteBackend{client: cfg.Client, prefix: cfg.KeyPrefix, chunkSize: cfg.ChunkSize}
	if b.chunkSize <= 0 {
		b.chunkSize = DefaultChunkSize
	}
	return cachex.Encoded(b, cfg.Codec, cfg.Logger)
}

type byteBackend struct {
	client    redis.UniversalClient
	prefix    string
	chunkSize int
}

var _ cachex.ByteBackend = (*byteBackend)(nil)

// ttl is how long to keep data expiring at expiresAt: at least a millisecond,
// as go-redis takes a negative TTL as "keep the current one".
func ttl(expiresAt time.Time) time.Duration { return max(time.Until(expiresAt), time.Millisecond) }

// Get reads with GET; Bytes() shares the reply's memory rather than copying
// it, and the command is discarded afterwards.
func (b *byteBackend) Get(ctx context.Context, key string) ([]byte, bool, error) {
	data, err := b.client.Get(ctx, b.prefix+key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("rediscachex: get %q: %w", key, err)
	}
	return data, true, nil
}

func (b *byteBackend) GetMany(ctx context.Context, keys []string) (map[string][]byte, error) {
	out := make(map[string][]byte, len(keys))
	errs := map[string]error{}
	for chunk := range slices.Chunk(keys, b.chunkSize) {
		pipe := b.client.Pipeline()
		cmds := make([]*redis.StringCmd, len(chunk))
		for i, key := range chunk {
			cmds[i] = pipe.Get(ctx, b.prefix+key)
		}
		execPipe(ctx, pipe)
		for i, key := range chunk {
			switch data, err := cmds[i].Bytes(); {
			case errors.Is(err, redis.Nil):
			case err != nil:
				errs[key] = fmt.Errorf("rediscachex: get %q: %w", key, err)
			default:
				out[key] = data
			}
		}
	}
	return out, cachex.NewBatchError(errs)
}

func (b *byteBackend) Set(ctx context.Context, key string, data []byte, expiresAt time.Time) error {
	if err := b.client.Set(ctx, b.prefix+key, data, ttl(expiresAt)).Err(); err != nil {
		return fmt.Errorf("rediscachex: set %q: %w", key, err)
	}
	return nil
}

// SetMany stores entries with a TTL up to their ExpiresAt.
func (b *byteBackend) SetMany(ctx context.Context, entries map[string]cachex.ByteEntry) error {
	errs := map[string]error{}
	for chunk := range slices.Chunk(slices.Collect(maps.Keys(entries)), b.chunkSize) {
		pipe := b.client.Pipeline()
		cmds := make([]*redis.StatusCmd, len(chunk))
		for i, key := range chunk {
			cmds[i] = pipe.Set(ctx, b.prefix+key, entries[key].Data, ttl(entries[key].ExpiresAt))
		}
		execPipe(ctx, pipe)
		for i, cmd := range cmds {
			if err := cmd.Err(); err != nil {
				errs[chunk[i]] = fmt.Errorf("rediscachex: set %q: %w", chunk[i], err)
			}
		}
	}
	return cachex.NewBatchError(errs)
}

func (b *byteBackend) Del(ctx context.Context, key string) error {
	if err := b.client.Del(ctx, b.prefix+key).Err(); err != nil {
		return fmt.Errorf("rediscachex: delete %q: %w", key, err)
	}
	return nil
}

func (b *byteBackend) DelMany(ctx context.Context, keys []string) error {
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
	return cachex.NewBatchError(errs)
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
