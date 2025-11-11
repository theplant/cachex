package cachex

import (
	"context"
)

// State represents the staleness state of cached data
type State int8

const (
	StateFresh    State = iota // Data is fresh and valid
	StateStale                 // Data is stale but usable
	StateTooStale              // Data is too stale and must be refreshed
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
