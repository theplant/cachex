package cachex

import (
	"context"
	"errors"
	"fmt"

	"github.com/allegro/bigcache/v3"
)

// BigCache is a cache implementation using BigCache
// It only supports []byte values as BigCache is designed for raw byte storage
type BigCache struct {
	cache *bigcache.BigCache
}

var _ Cache[[]byte] = &BigCache{}

// BigCacheConfig holds configuration for BigCache
type BigCacheConfig struct {
	bigcache.Config
}

// NewBigCache creates a new BigCache-based cache
func NewBigCache(ctx context.Context, config BigCacheConfig) (*BigCache, error) {
	cache, err := bigcache.New(ctx, config.Config)
	if err != nil {
		return nil, fmt.Errorf("failed to create bigcache: %w", err)
	}

	return &BigCache{
		cache: cache,
	}, nil
}

// Set stores a value in the cache
func (b *BigCache) Set(_ context.Context, key string, value []byte) error {
	err := b.cache.Set(key, value)
	if err != nil {
		return fmt.Errorf("failed to set value in bigcache for key: %s: %w", key, err)
	}
	return nil
}

// Get retrieves a value from the cache
func (b *BigCache) Get(_ context.Context, key string) ([]byte, error) {
	data, err := b.cache.Get(key)
	if err != nil {
		if errors.Is(err, bigcache.ErrEntryNotFound) {
			return nil, fmt.Errorf("key not found in bigcache for key: %s: %w", key, &ErrKeyNotFound{})
		}
		return nil, fmt.Errorf("failed to get value from bigcache for key: %s: %w", key, err)
	}
	return data, nil
}

// Del removes a value from the cache
func (b *BigCache) Del(_ context.Context, key string) error {
	err := b.cache.Delete(key)
	if err != nil {
		return fmt.Errorf("failed to delete value from bigcache for key: %s: %w", key, err)
	}
	return nil
}

// Close closes the cache and releases resources
func (b *BigCache) Close() error {
	err := b.cache.Close()
	if err != nil {
		return fmt.Errorf("failed to close bigcache: %w", err)
	}
	return nil
}
