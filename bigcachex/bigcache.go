// Package bigcachex is an in-memory cachex backend that stores entries as
// bytes in a BigCache, out of reach of the garbage collector: worth it when
// the memory layer holds millions of values and GC shows in profiles, at the
// cost of decoding on every read.
//
// BigCache has one lifetime for every entry (its LifeWindow); set it to at
// least the layer's longest fresh + stale TTL, the not-found TTLs included.
// Entries past their ExpiresAt stay until BigCache evicts them, but are never
// served.
package bigcachex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/allegro/bigcache/v3"

	"github.com/theplant/cachex/v2"
)

// Config configures a backend.
type Config[T any] struct {
	// Cache stores the entries. Required.
	Cache *bigcache.BigCache
	// Codec encodes values. Default cachex.DefaultCodec.
	Codec cachex.Codec[T]
	// Logger reports dropped entries that do not decode. Default slog.Default().
	Logger *slog.Logger
}

// New returns a backend over cfg.Cache, its entries encoded by cachex.Encoded
// with cfg.Codec and cfg.Logger. BigCache has no batch operations: the Many
// methods go key by key, best effort.
func New[T any](cfg Config[T]) cachex.Backend[T] {
	if cfg.Cache == nil {
		panic("bigcachex: Cache is required")
	}
	return cachex.Encoded(byteBackend{cfg.Cache}, cfg.Codec, cfg.Logger)
}

type byteBackend struct{ cache *bigcache.BigCache }

var _ cachex.ByteBackend = byteBackend{}

func (b byteBackend) Get(_ context.Context, key string) ([]byte, bool, error) {
	data, err := b.cache.Get(key)
	if errors.Is(err, bigcache.ErrEntryNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("bigcachex: get %q: %w", key, err)
	}
	return data, true, nil
}

func (b byteBackend) GetMany(ctx context.Context, keys []string) (map[string][]byte, error) {
	out := make(map[string][]byte, len(keys))
	errs := map[string]error{}
	for _, key := range keys {
		data, ok, err := b.Get(ctx, key)
		switch {
		case err != nil:
			errs[key] = err
		case ok:
			out[key] = data
		}
	}
	return out, cachex.NewBatchError(errs)
}

// Set ignores expiresAt: BigCache drops entries after its LifeWindow.
func (b byteBackend) Set(_ context.Context, key string, data []byte, _ time.Time) error {
	if err := b.cache.Set(key, data); err != nil {
		return fmt.Errorf("bigcachex: set %q: %w", key, err)
	}
	return nil
}

func (b byteBackend) SetMany(ctx context.Context, entries map[string]cachex.ByteEntry) error {
	errs := map[string]error{}
	for key, e := range entries {
		if err := b.Set(ctx, key, e.Data, e.ExpiresAt); err != nil {
			errs[key] = err
		}
	}
	return cachex.NewBatchError(errs)
}

func (b byteBackend) Del(_ context.Context, key string) error {
	if err := b.cache.Delete(key); err != nil && !errors.Is(err, bigcache.ErrEntryNotFound) {
		return fmt.Errorf("bigcachex: delete %q: %w", key, err)
	}
	return nil
}

func (b byteBackend) DelMany(ctx context.Context, keys []string) error {
	errs := map[string]error{}
	for _, key := range keys {
		if err := b.Del(ctx, key); err != nil {
			errs[key] = err
		}
	}
	return cachex.NewBatchError(errs)
}
