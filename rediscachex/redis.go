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
	"strings"
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
	// caches. It must not start with "cachex-lease:", which the keys of leases
	// (see Leases) start with, and a key that it makes start so is not
	// stored (writes of it fail with cachex.ErrUnsupportedKey). With leases
	// on, it must not have a "}" without a hash tag. Change it when the value
	// type changes incompatibly: JSON
	// decodes leniently, so a renamed field reads back as a zero value rather
	// than failing.
	KeyPrefix string
	// ChunkSize is how many keys one pipeline carries. Default DefaultChunkSize.
	ChunkSize int
	// Leases turns leases on (see cachex.Leaser), for a Redis that several
	// instances share: a fetch leases a key it misses, with SET NX PX on a
	// lease key in the key's Cluster slot, a write deletes the lease first,
	// and a backfill writes the key, with a Lua script, only if its lease
	// still holds the fetch's token. So a value read from the source before
	// another instance wrote the key is never written over that write.
	Leases bool
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
	if strings.HasPrefix(cfg.KeyPrefix, leasePrefix) {
		panic("rediscachex: KeyPrefix must not start with " + leasePrefix + ", where the keys of leases are")
	}
	if _, ok := leaseKey(cfg.KeyPrefix); !ok && cfg.Leases {
		panic("rediscachex: KeyPrefix has a \"}\" but no hash tag: no key under it can have a lease")
	}
	b := &byteBackend{client: cfg.Client, prefix: cfg.KeyPrefix, chunkSize: cfg.ChunkSize, leases: cfg.Leases}
	if b.chunkSize <= 0 {
		b.chunkSize = DefaultChunkSize
	}
	return cachex.Encoded(b, cfg.Codec, cfg.Logger)
}

type byteBackend struct {
	client    redis.UniversalClient
	prefix    string
	chunkSize int
	leases    bool
}

var (
	_ cachex.ByteBackend = (*byteBackend)(nil)
	_ cachex.ByteLeasing = (*byteBackend)(nil)
)

// ttl is how long to keep data expiring at expiresAt: at least a millisecond,
// as go-redis takes a negative TTL as "keep the current one".
func ttl(expiresAt time.Time) time.Duration { return max(time.Until(expiresAt), time.Millisecond) }

// leaseSpace reports whether the Redis key of key starts with leasePrefix,
// where the keys of leases are (possible when KeyPrefix is a start of it, ""
// say): such a key could be another key's lease, so it is never stored: it
// reads as a miss, a write of it fails with cachex.ErrUnsupportedKey, and a
// delete does nothing.
func (b *byteBackend) leaseSpace(key string) bool {
	return strings.HasPrefix(leasePrefix, b.prefix) && strings.HasPrefix(key, leasePrefix[len(b.prefix):])
}

// Get reads with GET; Bytes() shares the reply's memory rather than copying
// it, and the command is discarded afterwards.
func (b *byteBackend) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if b.leaseSpace(key) {
		return nil, false, nil
	}
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
			if !b.leaseSpace(key) {
				cmds[i] = pipe.Get(ctx, b.prefix+key)
			}
		}
		if pipe.Len() == 0 {
			continue
		}
		execPipe(ctx, pipe)
		for i, key := range chunk {
			if cmds[i] == nil {
				continue
			}
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
	return b.write(ctx, []string{key}, map[string]cachex.ByteEntry{key: {Data: data, ExpiresAt: expiresAt}})[key]
}

// SetMany stores entries with a TTL up to their ExpiresAt.
func (b *byteBackend) SetMany(ctx context.Context, entries map[string]cachex.ByteEntry) error {
	return cachex.NewBatchError(b.write(ctx, slices.Collect(maps.Keys(entries)), entries))
}

func (b *byteBackend) Del(ctx context.Context, key string) error {
	return b.write(ctx, []string{key}, nil)[key]
}

func (b *byteBackend) DelMany(ctx context.Context, keys []string) error {
	return cachex.NewBatchError(b.write(ctx, keys, nil))
}

// write sets the keys that have an entry in entries and deletes the others,
// in pipelines of ChunkSize keys; with leases on, each key's lease is deleted
// first (see endLease).
func (b *byteBackend) write(ctx context.Context, keys []string, entries map[string]cachex.ByteEntry) map[string]error {
	errs := map[string]error{}
	for chunk := range slices.Chunk(keys, b.chunkSize) {
		pipe := b.client.Pipeline()
		cmds := make([][2]redis.Cmder, len(chunk))
		for i, key := range chunk {
			e, set := entries[key]
			if b.leaseSpace(key) {
				if set {
					errs[key] = fmt.Errorf("rediscachex: key %q is where leases are: %w", key, cachex.ErrUnsupportedKey)
				}
				continue
			}
			end, err := b.endLease(ctx, pipe, key)
			if err != nil && set {
				errs[key] = err
				continue
			}
			if end != nil {
				cmds[i][0] = end
			}
			if set {
				cmds[i][1] = pipe.Set(ctx, b.prefix+key, e.Data, ttl(e.ExpiresAt))
			} else {
				cmds[i][1] = pipe.Del(ctx, b.prefix+key)
			}
		}
		if pipe.Len() == 0 {
			continue
		}
		execPipe(ctx, pipe)
		for i, key := range chunk {
			op := "set"
			if _, set := entries[key]; !set {
				op = "delete"
			}
			for _, cmd := range cmds[i] {
				if cmd != nil && cmd.Err() != nil {
					errs[key] = fmt.Errorf("rediscachex: %s %q: %w", op, key, cmd.Err())
					break
				}
			}
		}
	}
	return errs
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
