// Package ottercachex is an in-memory cachex backend built on otter: bounded
// by entry count or weight, and each entry expires at its own ExpiresAt.
//
// Values are stored as they are: a Get returns the very value that was set,
// so a caller that modifies a returned pointer modifies the cache. Store
// values that are not modified, or copies.
package ottercachex

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/maypok86/otter/v2"

	"github.com/theplant/cachex/v2"
)

// Config configures a Backend. Exactly one of MaximumSize and MaximumWeight
// (with Weigher) is required: an in-memory layer must be bounded.
type Config[T any] struct {
	// MaximumSize bounds the number of entries.
	MaximumSize int
	// MaximumWeight bounds the total weight of entries, as Weigher measures
	// them (in bytes, say).
	MaximumWeight uint64
	Weigher       func(key string, e cachex.Entry[T]) uint32
}

// Backend is an otter-backed cachex.Backend.
type Backend[T any] struct {
	cache *otter.Cache[string, cachex.Entry[T]]
}

var _ cachex.Backend[any] = (*Backend[any])(nil)

// New returns an empty Backend.
func New[T any](cfg Config[T]) (*Backend[T], error) {
	if (cfg.MaximumSize > 0) == (cfg.MaximumWeight > 0) {
		return nil, errors.New("ottercachex: set exactly one of MaximumSize and MaximumWeight")
	}
	opts := &otter.Options[string, cachex.Entry[T]]{
		MaximumSize:   cfg.MaximumSize,
		MaximumWeight: cfg.MaximumWeight,
		Weigher:       cfg.Weigher,
		ExpiryCalculator: otter.ExpiryWritingFunc(func(e otter.Entry[string, cachex.Entry[T]]) time.Duration {
			return time.Until(e.Value.ExpiresAt)
		}),
	}
	cache, err := otter.New(opts)
	if err != nil {
		return nil, fmt.Errorf("ottercachex: %w", err)
	}
	return &Backend[T]{cache: cache}, nil
}

func (b *Backend[T]) Get(_ context.Context, key string) (cachex.Entry[T], bool, error) {
	e, ok := b.cache.GetIfPresent(key)
	return e, ok, nil
}

func (b *Backend[T]) GetMany(_ context.Context, keys []string) (map[string]cachex.Entry[T], error) {
	out := make(map[string]cachex.Entry[T], len(keys))
	for _, key := range keys {
		if e, ok := b.cache.GetIfPresent(key); ok {
			out[key] = e
		}
	}
	return out, nil
}

func (b *Backend[T]) Set(_ context.Context, key string, e cachex.Entry[T]) error {
	b.cache.Set(key, e)
	return nil
}

func (b *Backend[T]) SetMany(_ context.Context, entries map[string]cachex.Entry[T]) error {
	for key, e := range entries {
		b.cache.Set(key, e)
	}
	return nil
}

func (b *Backend[T]) Del(_ context.Context, key string) error {
	b.cache.Invalidate(key)
	return nil
}

func (b *Backend[T]) DelMany(_ context.Context, keys []string) error {
	for _, key := range keys {
		b.cache.Invalidate(key)
	}
	return nil
}

// Len is the number of entries, possibly including expired ones not yet
// removed.
func (b *Backend[T]) Len() int { return b.cache.EstimatedSize() }
