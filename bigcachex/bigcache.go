// Package bigcachex is an in-memory cachex backend that stores entries as
// bytes in a BigCache, out of reach of the garbage collector: worth it when
// the memory layer holds millions of values and GC shows in profiles, at the
// cost of decoding on every read.
//
// BigCache has one lifetime for every entry (its LifeWindow); set it to at
// least the longest TTL of the layer. Entries past their ExpiresAt stay until
// BigCache evicts them, but are never served.
package bigcachex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/allegro/bigcache/v3"

	"github.com/theplant/cachex/v2"
)

// Config configures a Backend.
type Config[T any] struct {
	// Cache stores the entries. Required.
	Cache *bigcache.BigCache
	// Codec encodes values. Default cachex.DefaultCodec.
	Codec cachex.Codec[T]
	// Logger reports dropped entries that do not decode. Default slog.Default().
	Logger *slog.Logger
}

// Backend is a BigCache-backed cachex.Backend.
type Backend[T any] struct {
	cache  *bigcache.BigCache
	codec  cachex.Codec[T]
	logger *slog.Logger
}

var _ cachex.Backend[any] = (*Backend[any])(nil)

// New returns a Backend over cfg.Cache.
func New[T any](cfg Config[T]) *Backend[T] {
	if cfg.Cache == nil {
		panic("bigcachex: Cache is required")
	}
	b := &Backend[T]{cache: cfg.Cache, codec: cfg.Codec, logger: cfg.Logger}
	if b.codec == nil {
		b.codec = cachex.DefaultCodec[T]()
	}
	if b.logger == nil {
		b.logger = slog.Default()
	}
	return b
}

func (b *Backend[T]) Get(ctx context.Context, key string) (cachex.Entry[T], bool, error) {
	data, err := b.cache.Get(key)
	if errors.Is(err, bigcache.ErrEntryNotFound) {
		return cachex.Entry[T]{}, false, nil
	}
	if err != nil {
		return cachex.Entry[T]{}, false, fmt.Errorf("bigcachex: get %q: %w", key, err)
	}
	e, err := cachex.DecodeEntry(b.codec, data)
	if err != nil {
		// written before the value's type changed, say: a miss, not a failure
		b.logger.WarnContext(ctx, "bigcachex: dropped an entry that does not decode", "key", key, "error", err)
		_ = b.cache.Delete(key)
		return cachex.Entry[T]{}, false, nil
	}
	return e, true, nil
}

func (b *Backend[T]) GetMany(ctx context.Context, keys []string) (map[string]cachex.Entry[T], error) {
	out := make(map[string]cachex.Entry[T], len(keys))
	errs := map[string]error{}
	for _, key := range keys {
		e, ok, err := b.Get(ctx, key)
		switch {
		case err != nil:
			errs[key] = err
		case ok:
			out[key] = e
		}
	}
	return out, batchError(errs)
}

func (b *Backend[T]) Set(_ context.Context, key string, e cachex.Entry[T]) error {
	data, err := cachex.EncodeEntry(b.codec, e)
	if err != nil {
		return err
	}
	if err := b.cache.Set(key, data); err != nil {
		return fmt.Errorf("bigcachex: set %q: %w", key, err)
	}
	return nil
}

func (b *Backend[T]) SetMany(ctx context.Context, entries map[string]cachex.Entry[T]) error {
	errs := map[string]error{}
	for key, e := range entries {
		if err := b.Set(ctx, key, e); err != nil {
			errs[key] = err
		}
	}
	return batchError(errs)
}

func (b *Backend[T]) Del(_ context.Context, key string) error {
	if err := b.cache.Delete(key); err != nil && !errors.Is(err, bigcache.ErrEntryNotFound) {
		return fmt.Errorf("bigcachex: delete %q: %w", key, err)
	}
	return nil
}

func (b *Backend[T]) DelMany(ctx context.Context, keys []string) error {
	errs := map[string]error{}
	for _, key := range keys {
		if err := b.Del(ctx, key); err != nil {
			errs[key] = err
		}
	}
	return batchError(errs)
}

func batchError(errs map[string]error) error {
	if len(errs) == 0 {
		return nil
	}
	return &cachex.BatchError{Errors: errs}
}
