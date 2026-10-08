package cachex

import (
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"time"

	"github.com/pkg/errors"
)

var (
	DefaultFetchTimeout     = 60 * time.Second
	DefaultFetchConcurrency = 1
	// DefaultGetManyConcurrency bounds the concurrent upstream.Get calls of one
	// GetMany when the upstream does not implement BatchUpstream.
	DefaultGetManyConcurrency = 16
	NowFunc                   = time.Now
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
	getManyConc      int
	logger           *slog.Logger

	flights         flightGroup[T]
	asyncRefreshing sync.Map

	// Double-check optimization
	doubleCheckMode   DoubleCheckMode // User configuration (immutable)
	enableDoubleCheck bool            // Resolved execution decision

	// Test hooks for simulating race conditions
	testHooks *testHooks
}

type testHooks struct {
	beforeSingleflightStart func(ctx context.Context, key string)
	afterSingleflightStart  func(ctx context.Context, key string)
	afterSingleflightEnd    func(ctx context.Context, key string)
}

// NewClient creates a new client that manages the backend cache and fetches from upstream.
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
		getManyConc:      DefaultGetManyConcurrency,
		logger:           slog.Default(),
		doubleCheckMode:  DoubleCheckAuto, // Default: auto (smart heuristic)
	}

	// Apply user options
	for _, opt := range opts {
		opt(c)
	}

	// Resolve double-check mode to boolean flag
	c.enableDoubleCheck = c.resolveDoubleCheckMode()

	if c.fetchTimeout <= 0 {
		panic("fetchTimeout must be positive")
	}
	if c.fetchConcurrency <= 0 {
		panic("fetchConcurrency must be positive")
	}
	if c.getManyConc <= 0 {
		panic("getManyConcurrency must be positive")
	}

	return c
}

// Get retrieves a value from the cache or upstream
func (c *Client[T]) Get(ctx context.Context, key string) (T, error) {
	return c.get(ctx, key, false)
}

func (c *Client[T]) get(ctx context.Context, key string, doubleCheck bool) (T, error) {
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
			if c.serveStale && !doubleCheck {
				c.asyncRefresh(context.WithoutCancel(ctx), key)
				return value, nil
			}

		case StateRotten:
			// Rotten, must refresh
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
				return zero, errors.Wrapf(&ErrKeyNotFound{
					Cached:     true,
					CacheState: StateFresh,
				}, "key not found in cache for key: %s", key)

			case StateStale:
				if c.serveStale && !doubleCheck {
					c.asyncRefresh(context.WithoutCancel(ctx), key)
					return zero, errors.Wrapf(&ErrKeyNotFound{
						Cached:     true,
						CacheState: StateStale,
					}, "key not found in cache for key: %s", key)
				}

			case StateRotten:
				// Rotten, must refresh
			}
		} else if !IsErrKeyNotFound(err) {
			return zero, errors.Wrapf(err, "get from notFoundCache failed for key: %s", key)
		}
	}

	if doubleCheck {
		return zero, errors.Wrapf(&ErrKeyNotFound{}, "key not found in cache for key: %s", key)
	}

	return c.fetchFromUpstream(ctx, key)
}

// Del removes a value from the cache and propagates deletion through cache layers.
//
// Cache Layer Propagation:
// Del will propagate through all cache layers where upstream implements Cache[T],
// automatically stopping when upstream doesn't implement Cache[T] (e.g. UpstreamFunc
// for databases). This ensures consistency across multi-level cache architectures.
//
// Examples:
//
//	Single-level (L1 -> Database):
//	  client.Del(ctx, key)  // Deletes from L1 only
//
//	Multi-level (L1 -> L2 -> Database):
//	  l1Client.Del(ctx, key)  // Deletes from L1 and L2, stops at Database
//
// This supports both write-through and cache-aside patterns, as the chain
// naturally terminates when upstream is not a Cache[T] implementation.
func (c *Client[T]) Del(ctx context.Context, key string) error {
	if err := c.delWithoutUpstream(ctx, key); err != nil {
		return err
	}

	if upstreamCache, ok := c.upstream.(Cache[T]); ok {
		if err := upstreamCache.Del(ctx, key); err != nil {
			return errors.Wrapf(err, "delete from upstream failed for key: %s", key)
		}
	}

	return nil
}

// delWithoutUpstream records a not-found and deletes the backend entry; a
// failed not-found write does not keep the old value.
func (c *Client[T]) delWithoutUpstream(ctx context.Context, key string) error {
	var errs []error
	if c.notFoundCache != nil {
		if err := c.notFoundCache.Set(ctx, key, NowFunc()); err != nil {
			errs = append(errs, errors.Wrapf(err, "failed to set notFoundCache for key: %s", key))
		}
	}
	if err := c.backend.Del(ctx, key); err != nil {
		errs = append(errs, errors.Wrapf(err, "delete from backend failed for key: %s", key))
	}
	return stderrors.Join(errs...)
}

// Set stores a value in the cache and propagates through cache layers.
//
// Cache Layer Propagation:
// Set will propagate through all cache layers where upstream implements Cache[T],
// automatically stopping when upstream doesn't implement Cache[T] (e.g. UpstreamFunc
// for databases). This ensures consistency across multi-level cache architectures.
//
// Examples:
//
//	Single-level cache-aside pattern (L1 -> Database):
//	  db.Update(user)           // Update database first
//	  client.Set(ctx, key, user) // Then update L1 cache only
//
//	Multi-level cache-aside pattern (L1 -> L2 -> Database):
//	  db.Update(user)             // Update database first
//	  l1Client.Set(ctx, key, user) // Then update L1 and L2, stops at Database
//
// The type-based propagation automatically handles both write-through (multi-level caches)
// and cache-aside (with data source) patterns correctly.
func (c *Client[T]) Set(ctx context.Context, key string, value T) error {
	if err := c.setWithoutUpstream(ctx, key, value); err != nil {
		return err
	}

	if upstreamCache, ok := c.upstream.(Cache[T]); ok {
		if err := upstreamCache.Set(ctx, key, value); err != nil {
			return errors.Wrapf(err, "set in upstream failed for key: %s", key)
		}
	}

	return nil
}

// setWithoutUpstream clears a cached not-found and writes the backend; a failed
// not-found cleanup does not stop the write, since reads check the backend first.
func (c *Client[T]) setWithoutUpstream(ctx context.Context, key string, value T) error {
	var errs []error
	if c.notFoundCache != nil {
		if err := c.notFoundCache.Del(ctx, key); err != nil {
			errs = append(errs, errors.Wrapf(err, "delete from notFoundCache failed for key: %s", key))
		}
	}
	if err := c.backend.Set(ctx, key, value); err != nil {
		errs = append(errs, errors.Wrapf(err, "set in backend failed for key: %s", key))
	}
	return stderrors.Join(errs...)
}

func (c *Client[T]) fetchFromUpstream(ctx context.Context, key string) (T, error) {
	return c.fetchFromUpstreamWithSFKey(ctx, key, c.makeSFKey(key))
}

func (c *Client[T]) fetchFromUpstreamWithSFKey(ctx context.Context, key string, sfKey string) (T, error) {
	var zero T

	if c.testHooks != nil && c.testHooks.beforeSingleflightStart != nil {
		c.testHooks.beforeSingleflightStart(ctx, key)
	}

	f, leader := c.flights.claim(sfKey)
	if leader {
		go c.runClaimed(ctx, key, sfKey, f)
	}

	select {
	case <-ctx.Done():
		return zero, errors.Wrapf(ctx.Err(), "context cancelled during fetch for key: %s", key)
	case <-f.done:
		if c.testHooks != nil && c.testHooks.afterSingleflightEnd != nil {
			c.testHooks.afterSingleflightEnd(ctx, key)
		}
		if f.err != nil {
			return zero, f.err
		}
		return f.value, nil
	}
}

// runClaimed fetches a key this request has claimed and publishes the result.
func (c *Client[T]) runClaimed(ctx context.Context, key, sfKey string, f *flight[T]) {
	returned := false
	defer func() {
		if !returned {
			// runtime.Goexit in the upstream: release the key without
			// publishing, like x/sync/singleflight; waiters wait for their ctx.
			c.flights.forget(sfKey, f)
		}
	}()
	value, err := c.fetchClaimed(ctx, key)
	returned = true
	c.flights.finish(sfKey, f, value, err)
}

// fetchClaimed fetches a key this request has claimed in the flight group.
func (c *Client[T]) fetchClaimed(ctx context.Context, key string) (result T, resultErr error) {
	if c.testHooks != nil && c.testHooks.afterSingleflightStart != nil {
		c.testHooks.afterSingleflightStart(ctx, key)
	}

	defer func() {
		if r := recover(); r != nil {
			c.logger.ErrorContext(ctx, "panic during upstream fetch",
				"key", key,
				"panic", r,
				"stack", string(debug.Stack()))
			var zero T
			result = zero
			resultErr = errors.Errorf("panic during upstream fetch: %v", r)
		}
	}()

	// Double-check optimization: check cache again before fetching from upstream
	// This handles the narrow window after a write completes but before singleflight releases
	//
	// Note: We use the original key (not sfKey) because:
	// 1. fetchConcurrency allows multiple slots to fetch concurrently (exploration phase)
	// 2. Once ANY slot completes, ALL slots should converge to reuse that result (convergence phase)
	// 3. Using key ensures cross-slot visibility, maximizing result reuse after first completion
	if c.enableDoubleCheck {
		cachedValue, err := c.get(ctx, key, true)

		if err == nil {
			return cachedValue, nil
		}
		if isCachedFreshNotFound(err) {
			var zero T
			return zero, err
		}
		// otherwise, fetch from upstream
	}

	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.fetchTimeout)
	defer cancel()
	return c.doFetch(fetchCtx, key)
}

func isCachedFreshNotFound(err error) bool {
	var e *ErrKeyNotFound
	return IsErrKeyNotFound(err) && errors.As(err, &e) && e.Cached && e.CacheState == StateFresh
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

func (c *Client[T]) doFetch(ctx context.Context, key string) (T, error) {
	value, err := c.upstream.Get(ctx, key)
	if err != nil {
		if IsErrKeyNotFound(err) {
			if delErr := c.delWithoutUpstream(ctx, key); delErr != nil {
				c.logger.WarnContext(ctx, "failed to delete cache entry", "key", key, "error", delErr)
			}
		}
		var zero T
		return zero, errors.Wrapf(err, "get from upstream failed for key: %s", key)
	}

	if setErr := c.setWithoutUpstream(ctx, key, value); setErr != nil {
		c.logger.WarnContext(ctx, "failed to set cache entry", "key", key, "error", setErr)
	}

	return value, nil
}

// resolveDoubleCheckMode converts the mode to a boolean decision
func (c *Client[T]) resolveDoubleCheckMode() bool {
	switch c.doubleCheckMode {
	case DoubleCheckEnabled:
		return true
	case DoubleCheckDisabled:
		return false
	case DoubleCheckAuto:
		// Auto mode: enable when notFoundCache exists (can leverage it in double-check)
		return c.notFoundCache != nil
	default:
		return false
	}
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
		return StateRotten
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
//
// Philosophy: Concurrent exploration + Result convergence
//   - Exploration phase: When cache misses, allow N concurrent fetches to maximize throughput
//   - Convergence phase: Once any fetch completes, all subsequent requests reuse that result
//
// Behavior:
//   - concurrency = 1 (default): Full singleflight, all requests wait for single fetch
//   - concurrency > 1: Requests distributed across N slots, allowing moderate redundancy
//
// Example: WithFetchConcurrency(5) allows up to 5 concurrent upstream fetches for the same key.
func WithFetchConcurrency[T any](concurrency int) ClientOption[T] {
	return func(c *Client[T]) {
		c.fetchConcurrency = concurrency
	}
}

// WithGetManyConcurrency sets the maximum number of keys one GetMany fetches
// at a time when the upstream does not implement BatchUpstream (default
// DefaultGetManyConcurrency); each is fetched as Get would, with its own fetch
// timeout. It does not apply to a BatchUpstream, which gets one GetMany call.
func WithGetManyConcurrency[T any](concurrency int) ClientOption[T] {
	return func(c *Client[T]) {
		c.getManyConc = concurrency
	}
}

// WithLogger sets the logger for the client.
// If not set, slog.Default() is used.
func WithLogger[T any](logger *slog.Logger) ClientOption[T] {
	return func(c *Client[T]) {
		c.logger = logger
	}
}
