// Package cachex is a multi-layer read-through cache: a Cache reads its layers
// top down (memory, then Redis, then a database table, say) and asks the
// source only when none of them has a usable entry. Concurrent misses of a key
// are fetched once; writes through the Cache reach every layer in one order.
// See docs/design.md.
package cachex

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/theplant/cachex/v2/internal/flight"
	"github.com/theplant/cachex/v2/internal/stripe"
)

// Defaults of the Cache options.
const (
	DefaultFetchTimeout       = 60 * time.Second
	DefaultFetchesPerKey      = 1
	DefaultGetManyConcurrency = 16
)

// DoubleCheckMode says when a request that claimed the fetch of a key reads the
// first layer again before going down, in case another request has just filled
// it.
type DoubleCheckMode int8

const (
	// DoubleCheckAuto re-reads only if this Cache wrote the key's stripe since
	// the request read the first layer: otherwise the re-read would find the
	// same miss. The default.
	DoubleCheckAuto DoubleCheckMode = iota
	// DoubleCheckEnabled always re-reads: use it to see what other processes
	// wrote to a shared first layer.
	DoubleCheckEnabled
	// DoubleCheckDisabled never re-reads.
	DoubleCheckDisabled
)

// Cache reads through its layers to its source. Build it with New.
type Cache[T any] struct {
	source      Source[T]
	batchSource BatchSource[T] // the source, if it answers many keys at once
	layers      []Layer[T]
	opts        options
	maxAge      func(T) time.Duration

	flights    flight.Group[flightKey, T]
	stripes    *stripe.Set
	refreshing sync.Map // keys being refreshed in the background

	mu     sync.Mutex // guards closed and adding to wg
	closed bool
	wg     sync.WaitGroup // background refreshes
}

// flightKey is a key and one of its fetch slots (see WithFetchesPerKey).
type flightKey struct {
	key  string
	slot int
}

type options struct {
	fetchTimeout  time.Duration
	fetchesPerKey int
	getManyConc   int
	getManyChunk  int
	logger        *slog.Logger
	now           func() time.Time
	maxAge        any
	doubleCheck   DoubleCheckMode
}

// Option configures a Cache.
type Option func(*options)

// New returns a Cache reading layers top down (layers[0] first) and then
// source. With no layers it only merges concurrent fetches of a key.
func New[T any](source Source[T], layers []Layer[T], opts ...Option) *Cache[T] {
	if source == nil {
		panic("cachex: New: source is required")
	}
	o := options{
		fetchTimeout:  DefaultFetchTimeout,
		fetchesPerKey: DefaultFetchesPerKey,
		getManyConc:   DefaultGetManyConcurrency,
		logger:        slog.Default(),
		now:           time.Now,
	}
	for _, opt := range opts {
		opt(&o)
	}
	switch {
	case o.fetchTimeout <= 0:
		panic("cachex: New: the fetch timeout must be positive")
	case o.fetchesPerKey <= 0:
		panic("cachex: New: fetches per key must be positive")
	case o.getManyConc <= 0:
		panic("cachex: New: the GetMany concurrency must be positive")
	case o.getManyChunk < 0:
		panic("cachex: New: the GetMany chunk size must not be negative")
	}
	for i, l := range layers {
		if l.backend == nil {
			panic(fmt.Sprintf("cachex: New: layer %d was not built with NewLayer", i))
		}
	}
	c := &Cache[T]{
		source:  source,
		layers:  layers,
		opts:    o,
		stripes: stripe.NewSet(),
	}
	c.batchSource, _ = source.(BatchSource[T])
	if o.maxAge != nil {
		f, ok := o.maxAge.(func(T) time.Duration)
		if !ok {
			panic(fmt.Sprintf("cachex: New: WithMaxAge takes a func(%T) time.Duration, got %T", *new(T), o.maxAge))
		}
		c.maxAge = f
	}
	return c
}

// WithFetchTimeout bounds each call a fetch makes below the first layer: a
// lower layer read, a source call, a backfill. Default DefaultFetchTimeout.
func WithFetchTimeout(timeout time.Duration) Option {
	return func(o *options) { o.fetchTimeout = timeout }
}

// WithFetchesPerKey sets how many fetches of one key may run at once. With 1
// (the default) concurrent misses of a key share one fetch; with n they are
// spread over n, trading some load on the layers below for throughput.
func WithFetchesPerKey(n int) Option {
	return func(o *options) { o.fetchesPerKey = n }
}

// WithGetManyConcurrency sets how many source requests one GetMany has in
// flight at once: Get calls if the source is not a BatchSource, chunk calls
// otherwise (see WithGetManyChunkSize). Default DefaultGetManyConcurrency.
func WithGetManyConcurrency(n int) Option {
	return func(o *options) { o.getManyConc = n }
}

// WithGetManyChunkSize splits the keys one GetMany sends to a BatchSource into
// calls of at most size keys; each call's keys are answered and backfilled as
// soon as it returns. Zero (the default) sends them in one call.
func WithGetManyChunkSize(size int) Option {
	return func(o *options) { o.getManyChunk = size }
}

// WithLogger sets the logger for failures that do not fail a call (a failed
// backfill, a failed background refresh). Default slog.Default().
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) { o.logger = logger }
}

// WithNow sets the clock. Default time.Now.
func WithNow(now func() time.Time) Option {
	return func(o *options) { o.now = now }
}

// WithMaxAge caps how long a value from the source is kept, in every layer:
// maxAge is called once when the value is fetched, and the entry is never
// served after that much time (zero or less: the value is not cached). Use it
// for values that expire on their own, such as tokens. Its argument type must
// be the Cache's T, or New panics.
func WithMaxAge[T any](maxAge func(T) time.Duration) Option {
	return func(o *options) { o.maxAge = maxAge }
}

// WithDoubleCheck sets the double-check mode. Default DoubleCheckAuto.
func WithDoubleCheck(mode DoubleCheckMode) Option {
	return func(o *options) { o.doubleCheck = mode }
}

type sharedKey struct{}

// IsShared reports whether ctx belongs to a fetch shared by every request of a
// key rather than to one caller: it carries the caller's values but not its
// cancellation, and a backend must not use caller state from it, such as a
// database transaction.
func IsShared(ctx context.Context) bool {
	shared, _ := ctx.Value(sharedKey{}).(bool)
	return shared
}

func sharedCtx(ctx context.Context) context.Context {
	return context.WithValue(context.WithoutCancel(ctx), sharedKey{}, true)
}

// Close stops starting background refreshes and waits for the running ones.
// It does not close the backends. The Cache still serves reads and writes.
func (c *Cache[T]) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.wg.Wait()
	return nil
}
