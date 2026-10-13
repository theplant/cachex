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
	"math/rand/v2"
	"sync"
	"time"

	"github.com/theplant/cachex/v2/internal/flight"
	"github.com/theplant/cachex/v2/internal/stripe"
)

// Defaults of the Cache options.
const (
	DefaultFetchTimeout       = 60 * time.Second
	DefaultGetManyConcurrency = 16
)

// Cache reads through its layers to its source. Build it with New.
type Cache[T any] struct {
	source      Source[T]
	batchSource BatchSource[T] // the source, if it answers many keys at once
	layers      []Layer[T]
	leasers     []Leaser[T] // by layer, nil for one that does not lease; nil if none does
	opts        options
	maxAge      func(T) time.Duration

	flights    flight.Group[flightKey, T]
	stripes    *stripe.Set
	refreshing sync.Map // keys being refreshed in the background
	sourcing   sync.Map // key -> *sourceCall[T]: the fetch asking the source for it (see askSource)

	mu     sync.Mutex // guards closed and adding to wg
	closed bool
	wg     sync.WaitGroup // fetches and refreshes started before Close

	local      []int    // the layers whose backend is a LocalBackend
	sender     [16]byte // this Cache in its broadcast messages
	stopListen context.CancelFunc
	listening  chan struct{} // closed once the broadcast listener returns
}

// flightKey is what a flight is registered under: a key, and whether it is a
// background refresh, which has flights of its own: it must not join a fetch
// that takes a stale entry from below.
type flightKey struct {
	key     string
	refresh bool
}

type options struct {
	fetchTimeout time.Duration
	getManyConc  int
	getManyChunk int
	logger       *slog.Logger
	now          func() time.Time
	maxAge       any
	broadcast    Broadcast
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
		fetchTimeout: DefaultFetchTimeout,
		getManyConc:  DefaultGetManyConcurrency,
		logger:       slog.Default(),
		now:          time.Now,
	}
	for _, opt := range opts {
		opt(&o)
	}
	switch {
	case o.fetchTimeout <= 0:
		panic("cachex: New: the fetch timeout must be positive")
	case o.getManyConc <= 0:
		panic("cachex: New: the GetMany concurrency must be positive")
	case o.getManyChunk < 0:
		panic("cachex: New: the GetMany chunk size must not be negative")
	case o.logger == nil:
		panic("cachex: New: the logger must not be nil")
	case o.now == nil:
		panic("cachex: New: the clock must not be nil")
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
	for i, l := range layers {
		if leaser := leaserOf(l.backend); leaser != nil {
			if c.leasers == nil {
				c.leasers = make([]Leaser[T], len(layers))
			}
			c.leasers[i] = leaser
		}
	}
	if o.maxAge != nil {
		f, ok := o.maxAge.(func(T) time.Duration)
		if !ok {
			panic(fmt.Sprintf("cachex: New: WithMaxAge takes a func(%T) time.Duration, got %T", *new(T), o.maxAge))
		}
		c.maxAge = f
	}
	if o.broadcast != nil {
		for i, l := range layers {
			if _, ok := l.backend.(LocalBackend); ok {
				c.local = append(c.local, i)
			}
		}
		if len(c.local) == 0 {
			panic("cachex: New: WithBroadcast needs a layer whose backend is a LocalBackend")
		}
		for i := range c.sender {
			c.sender[i] = byte(rand.Uint32())
		}
		var ctx context.Context
		ctx, c.stopListen = context.WithCancel(context.Background())
		c.listening = make(chan struct{})
		go func() {
			defer close(c.listening)
			c.listen(ctx)
		}()
	}
	return c
}

// WithFetchTimeout bounds, each on its own: a fetch's double check, each
// lower layer read together with the backfill of what it found, each source
// call together with the backfill of its answers, and the invalidation after
// a failed write. Default DefaultFetchTimeout.
func WithFetchTimeout(timeout time.Duration) Option {
	return func(o *options) { o.fetchTimeout = timeout }
}

// WithGetManyConcurrency sets how many source requests one GetMany has in
// flight at once: Get calls if the source is not a BatchSource, chunk calls
// otherwise. A chunk (see WithGetManyChunkSize) starts only when the source has
// room for its requests. Default DefaultGetManyConcurrency.
func WithGetManyConcurrency(n int) Option {
	return func(o *options) { o.getManyConc = n }
}

// WithGetManyChunkSize splits the keys one GetMany fetches (and a background
// refresh of many keys) into chunks of at most size keys. A chunk is claimed
// when it starts, read from each layer below the first in one call, and sent
// to the source in one call (a BatchSource) or as a Get per key; its answers
// are returned and backfilled as soon as they arrive. The keys of a chunk not
// started yet are not claimed: a Get of one of them fetches it at once instead
// of waiting for the GetMany, and if the GetMany's ctx ends they are not
// fetched at all. Smaller chunks cost more calls to the lower layers. Zero (the
// default) makes all the keys one chunk.
func WithGetManyChunkSize(size int) Option {
	return func(o *options) { o.getManyChunk = size }
}

// WithLogger sets the logger for failures that do not fail a call (a failed
// backfill, invalidation or background refresh) and for panics in fetches.
// Default slog.Default().
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) { o.logger = logger }
}

// WithNow sets the clock the Cache computes and judges entries by. Default
// time.Now. A backend that expires entries natively does so by the wall
// clock, whatever this clock says (see Encoded): with a clock far behind the
// wall clock, it drops every entry at once.
func WithNow(now func() time.Time) Option {
	return func(o *options) { o.now = now }
}

// WithMaxAge caps how long a value from the source is kept, in every layer:
// maxAge is called once when the value is fetched, and the entry is never
// served after that much time (zero or less: the value is not cached). Use it
// for values that expire on their own, such as tokens. Set and SetMany call it
// too, for the values they write. Its argument type must be the Cache's T, or
// New panics.
func WithMaxAge[T any](maxAge func(T) time.Duration) Option {
	return func(o *options) { o.maxAge = maxAge }
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

// Close stops listening to the broadcast, if any, and waits for the fetches
// and background refreshes in progress (and their backfills) and for the
// invalidations of writes that gave up waiting, so that the backends can be
// closed after it, and starts no more background refreshes. It does not
// close the backends. The Cache still serves reads and writes, but no longer
// hears of other instances' writes: its local layers may serve what they
// overwrote until it rots.
func (c *Cache[T]) Close() error {
	if c.stopListen != nil {
		c.stopListen()
		<-c.listening
	}
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.wg.Wait()
	return nil
}

// track counts a piece of background work that Close must wait for; call
// done once when it ends. After Close it reports false, and done does nothing.
func (c *Cache[T]) track() (done func(), ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return func() {}, false
	}
	c.wg.Add(1)
	return c.wg.Done, true
}

// background runs f in a goroutine that Close waits for.
func (c *Cache[T]) background(f func()) {
	done, _ := c.track()
	go func() {
		defer done()
		f()
	}()
}
