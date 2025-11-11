package cachex

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/sync/singleflight"
)

var (
	DefaultFetchTimeout     = 60 * time.Second
	DefaultFetchConcurrency = 1
	NowFunc                 = time.Now
)

// Client manages cache operations with automatic upstream fetching
type Client[T any] struct {
	backend  Cache[T]
	upstream Upstream[T]

	checkDataStale     func(T) State
	notFoundCache      Cache[time.Time]
	checkNotFoundStale func(time.Time) State

	serveStale       bool
	fetchTimeout     time.Duration
	fetchConcurrency int
	logger           *slog.Logger

	sfg             singleflight.Group
	asyncRefreshing sync.Map
}

// NewClient creates a new client that manages the backend cache and fetches from upstream
func NewClient[T any](backend Cache[T], upstream Upstream[T], opts ...ClientOption[T]) *Client[T] {
	if backend == nil {
		panic("backend cache is required")
	}
	if upstream == nil {
		panic("upstream is required")
	}

	c := &Client[T]{
		backend:          backend,
		upstream:         upstream,
		fetchTimeout:     DefaultFetchTimeout,
		fetchConcurrency: DefaultFetchConcurrency,
		logger:           slog.Default(),
	}

	for _, opt := range opts {
		opt(c)
	}

	if c.fetchTimeout <= 0 {
		panic("fetchTimeout must be positive")
	}
	if c.fetchConcurrency <= 0 {
		panic("fetchConcurrency must be positive")
	}

	return c
}

// GetBackend returns the underlying backend cache
func (c *Client[T]) GetBackend() Cache[T] {
	return c.backend
}

// GetUpstream returns the upstream data source
func (c *Client[T]) GetUpstream() Upstream[T] {
	return c.upstream
}

// Get retrieves a value from the cache or upstream
func (c *Client[T]) Get(ctx context.Context, key string) (T, error) {
	var zero T

	// Check backend cache first
	value, err := c.backend.Get(ctx, key)
	if err == nil {
		checkDataStale := c.checkDataStale
		if checkDataStale == nil {
			checkDataStale = alwaysFresh[T]
		}
		state := checkDataStale(value)

		switch state {
		case StateFresh:
			return value, nil

		case StateStale:
			if c.serveStale {
				c.asyncRefresh(context.WithoutCancel(ctx), key)
				return value, nil
			}

		case StateTooStale:
			// Too stale, must refresh
		}
	} else if !IsErrKeyNotFound(err) {
		return zero, errors.Wrapf(err, "get from backend failed for key: %s", key)
	}

	// Backend miss, check notFoundCache
	if err != nil && c.notFoundCache != nil {
		cachedAt, err := c.notFoundCache.Get(ctx, key)
		if err == nil {
			checkNotFoundStale := c.checkNotFoundStale
			if checkNotFoundStale == nil {
				checkNotFoundStale = alwaysFresh[time.Time]
			}
			state := checkNotFoundStale(cachedAt)

			switch state {
			case StateFresh:
				return zero, &ErrKeyNotFound{
					Cached:     true,
					CacheState: StateFresh,
				}

			case StateStale:
				if c.serveStale {
					c.asyncRefresh(context.WithoutCancel(ctx), key)
					return zero, &ErrKeyNotFound{
						Cached:     true,
						CacheState: StateStale,
					}
				}

			case StateTooStale:
				// Too stale, must refresh
			}
		} else if !IsErrKeyNotFound(err) {
			return zero, errors.Wrapf(err, "get from notFoundCache failed for key: %s", key)
		}
	}

	// Fetch from upstream
	return c.fetchFromUpstream(ctx, key)
}

// Del removes a value from the cache
func (c *Client[T]) Del(ctx context.Context, key string) error {
	if c.notFoundCache != nil {
		if err := c.notFoundCache.Del(ctx, key); err != nil {
			return errors.Wrapf(err, "delete from notFoundCache failed for key: %s", key)
		}
	}

	if upstreamCache, ok := c.upstream.(Cache[T]); ok {
		if err := upstreamCache.Del(ctx, key); err != nil {
			return errors.Wrapf(err, "delete from upstream failed for key: %s", key)
		}
	}

	if err := c.backend.Del(ctx, key); err != nil {
		return errors.Wrapf(err, "delete from backend failed for key: %s", key)
	}

	return nil
}

// Set stores a value in the cache
func (c *Client[T]) Set(ctx context.Context, key string, value T) error {
	if c.notFoundCache != nil {
		if err := c.notFoundCache.Del(ctx, key); err != nil {
			return errors.Wrapf(err, "delete from notFoundCache failed for key: %s", key)
		}
	}

	if upstreamCache, ok := c.upstream.(Cache[T]); ok {
		if err := upstreamCache.Set(ctx, key, value); err != nil {
			return errors.Wrapf(err, "set in upstream failed for key: %s", key)
		}
	}

	if err := c.backend.Set(ctx, key, value); err != nil {
		return errors.Wrapf(err, "set in backend failed for key: %s", key)
	}

	return nil
}

func (c *Client[T]) fetchFromUpstream(ctx context.Context, key string) (T, error) {
	return c.fetchFromUpstreamWithSFKey(ctx, key, c.makeSFKey(key))
}

func (c *Client[T]) fetchFromUpstreamWithSFKey(ctx context.Context, key string, sfKey string) (T, error) {
	var zero T

	resChan := c.sfg.DoChan(sfKey, func() (any, error) {
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.fetchTimeout)
		defer cancel()
		return c.doFetch(fetchCtx, key)
	})

	select {
	case <-ctx.Done():
		return zero, errors.Wrapf(ctx.Err(), "context cancelled during fetch for key: %s", key)
	case res := <-resChan:
		if res.Err != nil {
			return zero, res.Err
		}
		return res.Val.(T), nil
	}
}

func (c *Client[T]) makeSFKey(key string) string {
	if c.fetchConcurrency > 1 {
		prefix := rand.IntN(c.fetchConcurrency)
		return fmt.Sprintf("%d:%s", prefix, key)
	}
	return key
}

func (c *Client[T]) asyncRefresh(ctx context.Context, key string) {
	sfKey := c.makeSFKey(key)

	if _, loaded := c.asyncRefreshing.LoadOrStore(sfKey, struct{}{}); loaded {
		return
	}

	go func() {
		defer c.asyncRefreshing.Delete(sfKey)

		if _, err := c.fetchFromUpstreamWithSFKey(ctx, key, sfKey); err != nil {
			c.logger.ErrorContext(ctx, "async refresh failed", "key", key, "error", err)
		}
	}()
}

func (c *Client[T]) doFetch(ctx context.Context, key string) (result T, resultErr error) {
	var zero T

	defer func() {
		if r := recover(); r != nil {
			c.logger.ErrorContext(ctx, "panic during upstream fetch",
				"key", key,
				"panic", r,
				"stack", string(debug.Stack()))
			result = zero
			resultErr = errors.Errorf("panic during upstream fetch: %v", r)
		}
	}()

	value, err := c.upstream.Get(ctx, key)
	if err != nil {
		if IsErrKeyNotFound(err) && c.notFoundCache != nil {
			if setErr := c.notFoundCache.Set(ctx, key, NowFunc()); setErr != nil {
				c.logger.WarnContext(ctx, "failed to set notFoundCache entry", "key", key, "error", setErr)
			}
		}
		return zero, errors.Wrapf(err, "get from upstream failed for key: %s", key)
	}

	if err := c.Set(ctx, key, value); err != nil {
		return zero, err
	}

	return value, nil
}

// ClientOption is a functional option for configuring a Client
type ClientOption[T any] func(*Client[T])

func alwaysFresh[T any](T) State {
	return StateFresh
}

// WithStale sets the function to check if cached data is stale
func WithStale[T any](fn func(T) State) ClientOption[T] {
	return func(c *Client[T]) {
		c.checkDataStale = fn
	}
}

// WithNotFound configures not-found caching with a custom staleness check
func WithNotFound[T any](cache Cache[time.Time], checkStale func(time.Time) State) ClientOption[T] {
	return func(c *Client[T]) {
		c.notFoundCache = cache
		c.checkNotFoundStale = checkStale
	}
}

// NotFoundWithTTL is a convenience function to configure not-found caching with TTL
// freshTTL: how long the not-found result stays fresh
// staleTTL: how long the not-found result stays stale (additional time after freshTTL)
// Entries in [0, freshTTL) are fresh, [freshTTL, freshTTL+staleTTL) are stale
func NotFoundWithTTL[T any](cache Cache[time.Time], freshTTL time.Duration, staleTTL time.Duration) ClientOption[T] {
	return WithNotFound[T](cache, func(cachedAt time.Time) State {
		age := NowFunc().Sub(cachedAt)
		if age < freshTTL {
			return StateFresh
		}
		if staleTTL > 0 && age < freshTTL+staleTTL {
			return StateStale
		}
		return StateTooStale
	})
}

// WithServeStale configures whether to serve stale data while refreshing asynchronously
func WithServeStale[T any](serveStale bool) ClientOption[T] {
	return func(c *Client[T]) {
		c.serveStale = serveStale
	}
}

// WithFetchTimeout sets the timeout for upstream fetch operations
func WithFetchTimeout[T any](timeout time.Duration) ClientOption[T] {
	return func(c *Client[T]) {
		c.fetchTimeout = timeout
	}
}

// WithFetchConcurrency sets the maximum number of concurrent fetch operations per key.
// If set to 1 (default), all requests for the same key are merged into a single fetch.
// If set to N > 1, requests are randomly distributed across N concurrent fetches.
// Example: WithFetchConcurrency(5) allows up to 5 concurrent upstream fetches for the same key.
func WithFetchConcurrency[T any](concurrency int) ClientOption[T] {
	return func(c *Client[T]) {
		c.fetchConcurrency = concurrency
	}
}

// WithLogger sets the logger for the client.
// If not set, slog.Default() is used.
func WithLogger[T any](logger *slog.Logger) ClientOption[T] {
	return func(c *Client[T]) {
		c.logger = logger
	}
}
