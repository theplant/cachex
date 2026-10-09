package cachex

import (
	"context"
	stderrors "errors"
	"fmt"
	"hash/maphash"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
)

var (
	DefaultFetchTimeout     = 60 * time.Second
	DefaultFetchConcurrency = 1
	// DefaultGetManyFetchConcurrency bounds the upstream requests one GetMany
	// has in flight at once (see WithGetManyFetchConcurrency).
	DefaultGetManyFetchConcurrency = 16
	NowFunc                        = time.Now
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
	getManyChunk     int // keys per BatchUpstream call; 0 means all in one
	logger           *slog.Logger

	flights         flightGroup[T]
	asyncRefreshing sync.Map

	// Write ordering, see Set and backfill
	writeSeed maphash.Seed
	writes    [writeStripeCount]writeStripe

	// Double-check optimization
	doubleCheckMode DoubleCheckMode // User configuration (immutable)

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
		getManyConc:      DefaultGetManyFetchConcurrency,
		logger:           slog.Default(),
		doubleCheckMode:  DoubleCheckAuto, // Default: auto (smart heuristic)
		writeSeed:        maphash.MakeSeed(),
	}

	// Apply user options
	for _, opt := range opts {
		opt(c)
	}

	// Resolve double-check mode to boolean flag

	if c.fetchTimeout <= 0 {
		panic("fetchTimeout must be positive")
	}
	if c.fetchConcurrency <= 0 {
		panic("fetchConcurrency must be positive")
	}
	if c.getManyChunk < 0 {
		panic("getManyChunkSize must not be negative")
	}
	if c.getManyConc <= 0 {
		panic("getManyFetchConcurrency must be positive")
	}

	return c
}

// Get retrieves a value from the cache or upstream
func (c *Client[T]) Get(ctx context.Context, key string) (T, error) {
	return c.get(ctx, key, false)
}

func (c *Client[T]) get(ctx context.Context, key string, doubleCheck bool) (T, error) {
	var zero T

	seen := c.stripe(key).epoch() // before reading, see shouldDoubleCheck
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
				c.asyncRefresh(context.WithoutCancel(ctx), key, seen)
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
					c.asyncRefresh(context.WithoutCancel(ctx), key, seen)
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

	return c.fetchFromUpstreamWithSFKey(ctx, key, c.makeSFKey(key), seen)
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
//
// Order: the upstream is deleted first, then this layer, so until this layer's
// delete lands its readers still see the deleted value. If the upstream
// delete fails, this layer's entry is still dropped but no not-found is
// cached, since the upstream may still hold the key. If the upstream delete
// succeeds but this layer's fails, the error is returned and the upstream
// stays deleted.
// Reads after it and waiting for other writes behave as for Set.
func (c *Client[T]) Del(ctx context.Context, key string) error {
	return c.write(ctx, key,
		func(upstream Cache[T]) error {
			if err := upstream.Del(ctx, key); err != nil {
				return errors.Wrapf(err, "delete from upstream failed for key: %s", key)
			}
			return nil
		},
		func() error { return c.delWithoutUpstream(ctx, key) },
	)
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
//
// Order: the upstream is written first, then this layer, so a new value never
// shows up here before it is below; until this layer's write lands, readers of
// this layer still see the old value. If the upstream write fails, its
// state is unknown (it may have been applied and only the reply lost), so this
// layer's entry is dropped and the next read goes down. If the upstream write
// succeeds but this layer's fails, the error is returned too (the upstream
// keeps the new value) and this layer's entry is dropped. A read that starts
// after Set returned never gets a value fetched before it. Concurrent writes to
// one key through the same Client are applied one at a time, in the same order
// on every layer. Waiting for another write or a backfill of the key's stripe
// gives up when ctx is done: nothing is written upstream, and this layer's
// entry is dropped once the stripe is free. A write that need not wait runs
// even with a done ctx. Once started, a failed write's cleanup outlives ctx,
// for at most the fetch timeout.
func (c *Client[T]) Set(ctx context.Context, key string, value T) error {
	return c.write(ctx, key,
		func(upstream Cache[T]) error {
			if err := upstream.Set(ctx, key, value); err != nil {
				return errors.Wrapf(err, "set in upstream failed for key: %s", key)
			}
			return nil
		},
		func() error { return c.setWithoutUpstream(ctx, key, value) },
	)
}

// writeStripeCount is the number of write stripes per Client.
//
// Keys share stripes by hash, so two keys in one stripe serialize
// their writes and a write to one can skip the other's backfill (a spare
// cache miss, never a stale value); raise it if that shows up.
// 4096 stripes of 40 bytes cost 160 KB per Client, embedded, no allocation.
const writeStripeCount = 4096

// writeStripe orders writes and backfills of the keys that hash to it.
type writeStripe struct {
	mu    sync.RWMutex  // Set/Del hold it, so all layers apply them in one order; backfills hold it for reading
	gen   atomic.Uint64 // bumped by every Set/Del, checked by backfills
	fills atomic.Uint64 // bumped by every backfill that wrote this layer
}

// epoch changes whenever this Client writes this layer for a key of the
// stripe (a Set/Del or a backfill). If it has not changed since a request read
// the layer, re-reading it (the double-check) would find the same miss.
func (s *writeStripe) epoch() uint64 { return s.gen.Load() + s.fills.Load() }

// lock takes the stripe for writing. Waiting for another write or a backfill
// gives up when ctx is done; a free stripe is taken even with a done ctx. A
// lock given up on is still taken in the background, runs late while holding
// it, and is released.
func (s *writeStripe) lock(ctx context.Context, late func()) error {
	if s.mu.TryLock() {
		return nil
	}
	locked := make(chan struct{})
	go func() {
		s.mu.Lock()
		close(locked)
	}()
	select {
	case <-locked:
		return nil
	case <-ctx.Done():
		go func() {
			<-locked
			late()
			s.mu.Unlock()
		}()
		return ctx.Err()
	}
}

func (c *Client[T]) stripe(key string) *writeStripe {
	return &c.writes[maphash.String(c.writeSeed, key)%writeStripeCount]
}

// write runs a Set or Del: the upstream first (if it is a Cache), then this
// layer. Any failure leaves this layer without an entry for the key.
func (c *Client[T]) write(ctx context.Context, key string, toUpstream func(Cache[T]) error, toLayer func() error) error {
	s := c.stripe(key)

	// Called once the upstream is written: a backfill that read the upstream
	// before took an older gen, so it is skipped once this lock is released, and
	// a read that starts from now on fetches anew instead of joining a fetch
	// that may have read the upstream before the write.
	written := func() {
		s.gen.Add(1)
		c.dropFlights(key)
	}

	// A write that gave up waiting writes nothing upstream, but like any failed
	// write it leaves this layer without an entry, once the stripe is free.
	if err := s.lock(ctx, func() { written(); c.invalidate(ctx, key) }); err != nil {
		return errors.Wrapf(err, "context cancelled while waiting to write key: %s", key)
	}
	defer s.mu.Unlock()
	// also drop fetches claimed while this layer was being written: their
	// double-check may have read the old value
	defer c.dropFlights(key)

	if upstreamCache, ok := c.upstream.(Cache[T]); ok {
		if err := toUpstream(upstreamCache); err != nil {
			written()
			c.invalidate(ctx, key)
			return err
		}
	}

	written()
	if err := toLayer(); err != nil {
		c.invalidate(ctx, key)
		return err
	}
	return nil
}

// dropFlights releases the in-flight fetches of key (every fetch slot) without
// interrupting them: callers already waiting still get their result.
func (c *Client[T]) dropFlights(key string) {
	if c.fetchConcurrency <= 1 {
		c.flights.drop(key)
		return
	}
	for i := range c.fetchConcurrency {
		c.flights.drop(fmt.Sprintf("%d:%s", i, key))
	}
}

// invalidate drops this layer's entry and cached not-found for key, best
// effort. It runs after a failed write, often failed by ctx itself, so it does
// not reuse ctx's cancellation.
func (c *Client[T]) invalidate(ctx context.Context, key string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.fetchTimeout)
	defer cancel()
	if c.notFoundCache != nil {
		if err := c.notFoundCache.Del(ctx, key); err != nil {
			c.logger.WarnContext(ctx, "failed to invalidate notFoundCache entry", "key", key, "error", err)
		}
	}
	if err := c.backend.Del(ctx, key); err != nil {
		c.logger.WarnContext(ctx, "failed to invalidate cache entry", "key", key, "error", err)
	}
}

// backfill writes what a fetch read into this layer, unless a Set or Del of
// the key happened since gen was taken (before reading the upstream). It holds
// the key's stripe for reading, so a write waits for it, and the fill either
// lands before the write or is skipped. A write in progress skips it too
// (TryRLock): reads never wait on writes, at the cost of a spare cache miss.
func (c *Client[T]) backfill(ctx context.Context, key string, gen uint64, fill func() error, failMsg string) {
	s := c.stripe(key)
	if !s.mu.TryRLock() {
		return
	}
	defer s.mu.RUnlock()
	if s.gen.Load() != gen {
		return
	}
	if err := fill(); err != nil {
		c.logger.WarnContext(ctx, failMsg, "key", key, "error", err)
	}
	s.fills.Add(1)
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

// fetchFromUpstreamWithSFKey fetches key through the flight group; seen is the
// key's stripe epoch from before the caller read this layer.
func (c *Client[T]) fetchFromUpstreamWithSFKey(ctx context.Context, key string, sfKey string, seen uint64) (T, error) {
	var zero T

	if c.testHooks != nil && c.testHooks.beforeSingleflightStart != nil {
		c.testHooks.beforeSingleflightStart(ctx, key)
	}

	f, leader := c.flights.claim(sfKey)
	if leader {
		go c.runClaimed(flightCtx(ctx), key, sfKey, f, seen)
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

// flightCtx is the ctx of a fetch claimed by one request but awaited by every
// request of the key: it keeps ctx's values (tracing, etc.) but not its
// cancellation, which belongs to that one request, nor its GORM transaction.
func flightCtx(ctx context.Context) context.Context {
	return withoutGORMTx(context.WithoutCancel(ctx))
}

// runClaimed fetches a key this request has claimed and publishes the result.
func (c *Client[T]) runClaimed(ctx context.Context, key, sfKey string, f *flight[T], seen uint64) {
	p := c.claimedFlights([]string{key}, []string{sfKey}, []*flight[T]{f})
	p.run(ctx, []int{0}, func() {
		value, err := c.fetchClaimed(ctx, key, seen)
		p.publish(0, result[T]{value: value, err: err})
	})
}

// fetchClaimed fetches a key this request has claimed in the flight group.
func (c *Client[T]) fetchClaimed(ctx context.Context, key string, seen uint64) (T, error) {
	if c.testHooks != nil && c.testHooks.afterSingleflightStart != nil {
		c.testHooks.afterSingleflightStart(ctx, key)
	}

	// Double-check optimization: check cache again before fetching from upstream
	// This handles the narrow window after a write completes but before singleflight releases
	//
	// Note: We use the original key (not sfKey) because:
	// 1. fetchConcurrency allows multiple slots to fetch concurrently (exploration phase)
	// 2. Once ANY slot completes, ALL slots should converge to reuse that result (convergence phase)
	// 3. Using key ensures cross-slot visibility, maximizing result reuse after first completion
	if c.shouldDoubleCheck(key, seen) {
		checkCtx, cancel := context.WithTimeout(ctx, c.fetchTimeout)
		cachedValue, err := c.get(checkCtx, key, true)
		cancel()

		if err == nil {
			return cachedValue, nil
		}
		if isCachedFreshNotFound(err) {
			var zero T
			return zero, err
		}
		// otherwise, fetch from upstream
	}

	fetchCtx, cancel := context.WithTimeout(ctx, c.fetchTimeout)
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

func (c *Client[T]) asyncRefresh(ctx context.Context, key string, seen uint64) {
	sfKey := c.makeSFKey(key)

	if _, loaded := c.asyncRefreshing.LoadOrStore(sfKey, struct{}{}); loaded {
		return
	}

	go func() {
		defer c.asyncRefreshing.Delete(sfKey)

		if _, err := c.fetchFromUpstreamWithSFKey(ctx, key, sfKey, seen); err != nil {
			c.logger.ErrorContext(ctx, "async refresh failed", "key", key, "error", err)
		}
	}()
}

func (c *Client[T]) doFetch(ctx context.Context, key string) (T, error) {
	gen := c.stripe(key).gen.Load()
	value, err := c.upstream.Get(ctx, key)
	if err != nil {
		if IsErrKeyNotFound(err) {
			c.backfill(ctx, key, gen, func() error { return c.delWithoutUpstream(ctx, key) }, "failed to delete cache entry")
		}
		var zero T
		return zero, errors.Wrapf(err, "get from upstream failed for key: %s", key)
	}

	c.backfill(ctx, key, gen, func() error { return c.setWithoutUpstream(ctx, key, value) }, "failed to set cache entry")

	return value, nil
}

// shouldDoubleCheck reports whether a claimed fetch re-reads this layer first.
// In DoubleCheckAuto it does only if this Client wrote the key's stripe since
// the request read the layer (seen): otherwise the re-read would find the same
// miss. A write to another key of the stripe makes it re-read for nothing.
func (c *Client[T]) shouldDoubleCheck(key string, seen uint64) bool {
	switch c.doubleCheckMode {
	case DoubleCheckEnabled:
		return true
	case DoubleCheckDisabled:
		return false
	default:
		return c.stripe(key).epoch() != seen
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

// WithGetManyFetchConcurrency sets how many upstream requests one GetMany has
// in flight at once (default DefaultGetManyFetchConcurrency): upstream.Get
// calls when the upstream does not implement BatchUpstream (each key fetched as
// Get would), or chunks of a BatchUpstream call (see WithGetManyChunkSize).
// Unlike WithFetchConcurrency, which bounds the fetches of one key, this bounds
// the different keys (or chunks) of one GetMany.
func WithGetManyFetchConcurrency[T any](concurrency int) ClientOption[T] {
	return func(c *Client[T]) {
		c.getManyConc = concurrency
	}
}

// WithGetManyChunkSize splits the keys one GetMany sends to a BatchUpstream into
// calls of at most size keys, run concurrently up to WithGetManyFetchConcurrency;
// the keys of a call are answered as soon as it returns. Zero (the default)
// sends them in one call. Use it when the upstream limits its batch size.
func WithGetManyChunkSize[T any](size int) ClientOption[T] {
	return func(c *Client[T]) {
		c.getManyChunk = size
	}
}

// WithLogger sets the logger for the client.
// If not set, slog.Default() is used.
func WithLogger[T any](logger *slog.Logger) ClientOption[T] {
	return func(c *Client[T]) {
		c.logger = logger
	}
}
