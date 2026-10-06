package cachex

import (
	"context"
)

// State represents the staleness state of cached data
type State int8

const (
	StateFresh  State = iota // Data is fresh and valid
	StateStale               // Data is stale but usable
	StateRotten              // Data is rotten and must be refreshed
)

// Upstream defines the interface for a data source that can retrieve values
type Upstream[T any] interface {
	Get(ctx context.Context, key string) (T, error)
}

// Cache defines the interface for a generic key-value cache with read and write capabilities
type Cache[T any] interface {
	Upstream[T]
	Set(ctx context.Context, key string, value T) error
	Del(ctx context.Context, key string) error
}

// UpstreamFunc is a function adapter that implements Upstream interface
type UpstreamFunc[T any] func(ctx context.Context, key string) (T, error)

func (f UpstreamFunc[T]) Get(ctx context.Context, key string) (T, error) {
	return f(ctx, key)
}

// BatchUpstream is an optional interface for an Upstream (or Cache) that can
// retrieve many keys in one call. Client.GetMany uses it when available and
// falls back to calling Get per key otherwise.
//
// Keys absent from the returned map do not exist (the batch form of
// ErrKeyNotFound). A non-nil error fails the whole batch, unless it is a
// *BatchError returned as is (not wrapped), which fails only the keys it lists
// while the map still carries the rest.
type BatchUpstream[T any] interface {
	GetMany(ctx context.Context, keys []string) (map[string]T, error)
}

// BatchCache is an optional interface for a Cache that can read and write many
// keys in one call. Client.GetMany uses it for the backend and the not-found
// cache when available, and falls back to Get/Set/Del per key otherwise.
type BatchCache[T any] interface {
	Cache[T]
	BatchUpstream[T]
	SetMany(ctx context.Context, values map[string]T) error
	DelMany(ctx context.Context, keys []string) error
}
