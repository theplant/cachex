package cachex

import (
	"context"
	"time"

	"github.com/dgraph-io/ristretto/v2"
	"github.com/pkg/errors"
)

// RistrettoCache is a cache implementation using ristretto
type RistrettoCache[T any] struct {
	cache *ristretto.Cache[string, T]
	ttl   time.Duration
}

var _ Cache[any] = &RistrettoCache[any]{}

// RistrettoCacheConfig holds configuration for RistrettoCache
type RistrettoCacheConfig[T any] struct {
	// Config is the ristretto configuration
	*ristretto.Config[string, T]

	// TTL is the time-to-live for cache entries
	// Zero means no expiration
	TTL time.Duration
}

// DefaultRistrettoCacheConfig returns a default configuration
func DefaultRistrettoCacheConfig[T any]() *RistrettoCacheConfig[T] {
	return &RistrettoCacheConfig[T]{
		Config: &ristretto.Config[string, T]{
			NumCounters: 1e7,     // 10M counters
			MaxCost:     1 << 30, // 1GB
			BufferItems: 64,
		},
		TTL: 0, // No expiration by default
	}
}

// NewRistrettoCache creates a new ristretto-based cache
func NewRistrettoCache[T any](config *RistrettoCacheConfig[T]) (*RistrettoCache[T], error) {
	cache, err := ristretto.NewCache(config.Config)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create ristretto cache")
	}

	return &RistrettoCache[T]{
		cache: cache,
		ttl:   config.TTL,
	}, nil
}

// Set stores a value in the cache with cost of 1
func (r *RistrettoCache[T]) Set(_ context.Context, key string, value T) error {
	// Note: SetWithTTL may return false in two cases:
	// 1. Cost exceeds MaxCost (unlikely with cost=1)
	// 2. Rejected by admission policy (W-TinyLFU)
	//
	// When false is returned, the item is dropped and not added to the cache.
	// When true is returned, the item enters the buffer and will be processed.
	//
	// We don't treat rejection as an error because:
	// - Admission policy rejection is a feature, not a bug
	// - It prevents cache pollution by low-value entries
	// - The cache remains consistent and functional
	// - Callers can still retrieve data from upstream on cache miss
	success := r.cache.SetWithTTL(key, value, 1, r.ttl)
	if success {
		// Wait ensures all buffered writes are applied before returning
		r.cache.Wait()
	}
	// Note: When success is false, there are no buffered writes to wait for
	return nil
}

// Get retrieves a value from the cache
func (r *RistrettoCache[T]) Get(_ context.Context, key string) (T, error) {
	var zero T
	value, found := r.cache.Get(key)
	if !found {
		return zero, errors.Wrapf(&ErrKeyNotFound{}, "key not found in ristretto cache for key: %s", key)
	}
	return value, nil
}

// Del removes a value from the cache
// Note: Del immediately removes the item from storage (see cache.go:376-377).
// However, it also sends a deletion flag to setBuf to handle operation ordering.
// If a concurrent Set(new key) is in progress, the order is:
//  1. Set sends itemNew to setBuf (but doesn't update storedItems yet)
//  2. Del immediately removes from storedItems (removes nothing if key is new)
//  3. Del sends itemDelete to setBuf
//  4. setBuf processes: itemNew (adds key) → itemDelete (removes key)
//
// Between steps 4's itemNew and itemDelete processing, there's a race window
// where Get might find the key. Calling Wait() ensures itemDelete is processed.
func (r *RistrettoCache[T]) Del(_ context.Context, key string) error {
	r.cache.Del(key)
	r.cache.Wait()
	return nil
}

// Close closes the cache and stops all background goroutines
func (r *RistrettoCache[T]) Close() error {
	r.cache.Close()
	return nil
}
