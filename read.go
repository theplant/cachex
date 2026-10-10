package cachex

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"slices"
	"sync"

	"github.com/theplant/cachex/v2/internal/flight"
	"github.com/theplant/cachex/v2/internal/stripe"
)

// Get returns key's value from the first layer with a usable entry, or else
// from the source; concurrent misses of a key share one fetch, whose result is
// backfilled into the layers above where it was found. A stale entry is
// returned at once and refreshed in the background. Get returns ErrNotFound
// for a key the source does not have.
func (c *Cache[T]) Get(ctx context.Context, key string) (T, error) {
	var seen uint64
	if len(c.layers) > 0 {
		seen = c.stripes.For(key).Epoch() // before reading, see shouldDoubleCheck
		e, ok, err := c.layers[0].backend.Get(ctx, key)
		if err != nil {
			var zero T
			return zero, fmt.Errorf("cachex: get %q from layer 0: %w", key, err)
		}
		if ok {
			switch e.state(c.opts.now()) {
			case fresh:
				return e.result()
			case stale:
				c.refresh(ctx, []string{key}, []uint64{seen})
				return e.result()
			}
		}
	}
	fk := c.flightKey(key)
	f, leader := c.flights.Claim(fk)
	if leader {
		p := c.claim([]string{key}, []flightKey{fk}, []*flight.Flight[T]{f})
		fctx := sharedCtx(ctx)
		c.background(func() { c.resolve(fctx, p, []uint64{seen}, false) })
	}
	return c.await(ctx, key, f)
}

// GetMany returns the values of keys that exist, with the semantics of calling
// Get for each: every layer and the source are read in one call per layer (a
// BatchSource in chunks, see WithGetManyChunkSize; any other source key by
// key, see WithGetManyConcurrency), and keys being fetched by other calls are
// waited for, not fetched again. A key the source does not have is absent from
// the map. If some keys failed, the map holds every other key and the error is
// a *BatchError listing the failed ones.
func (c *Cache[T]) GetMany(ctx context.Context, keys []string) (map[string]T, error) {
	keys = uniqueKeys(keys)
	out := make(map[string]T, len(keys))
	errs := map[string]error{}

	seen := make([]uint64, len(keys))
	missKeys, missSeen := keys, seen
	if len(c.layers) > 0 {
		for i, key := range keys {
			seen[i] = c.stripes.For(key).Epoch() // before reading, see shouldDoubleCheck
		}
		es, err := c.layers[0].backend.GetMany(ctx, keys)
		now := c.opts.now()
		var staleKeys []string
		var staleSeen []uint64
		missKeys, missSeen = nil, nil
		for i, key := range keys {
			if kerr := errForKey(err, key); kerr != nil {
				errs[key] = fmt.Errorf("cachex: get %q from layer 0: %w", key, kerr)
				continue
			}
			if e, ok := es[key]; ok {
				if st := e.state(now); st != rotten {
					if !e.NotFound {
						out[key] = e.Value
					}
					if st == stale {
						staleKeys = append(staleKeys, key)
						staleSeen = append(staleSeen, seen[i])
					}
					continue
				}
			}
			missKeys = append(missKeys, key)
			missSeen = append(missSeen, seen[i])
		}
		if len(staleKeys) > 0 {
			c.refresh(ctx, staleKeys, staleSeen)
		}
	}

	for i, r := range c.fetchMany(ctx, missKeys, missSeen, false) {
		key := missKeys[i]
		switch {
		case r.err == nil:
			out[key] = r.value
		case !errors.Is(r.err, ErrNotFound):
			errs[key] = r.err
		}
	}
	return out, batchError(errs)
}

type result[T any] struct {
	value T
	err   error
}

// fetchMany fetches keys through the flight group: it claims every key,
// resolves the ones it leads in one background call, and waits for all of
// them (including those led by others). seen is each key's stripe epoch from
// before the caller read the first layer.
func (c *Cache[T]) fetchMany(ctx context.Context, keys []string, seen []uint64, refresh bool) []result[T] {
	if len(keys) == 0 {
		return nil
	}
	flights := make([]*flight.Flight[T], len(keys))
	var ownKeys []string
	var ownFKs []flightKey
	var ownFlights []*flight.Flight[T]
	var ownSeen []uint64
	for i, key := range keys {
		fk := c.flightKey(key)
		f, leader := c.flights.Claim(fk)
		flights[i] = f
		if leader {
			ownKeys = append(ownKeys, key)
			ownFKs = append(ownFKs, fk)
			ownFlights = append(ownFlights, f)
			ownSeen = append(ownSeen, seen[i])
		}
	}
	if len(ownKeys) > 0 {
		fctx, p := sharedCtx(ctx), c.claim(ownKeys, ownFKs, ownFlights)
		c.background(func() { c.resolve(fctx, p, ownSeen, refresh) })
	}
	results := make([]result[T], len(keys))
	for i, f := range flights {
		results[i].value, results[i].err = c.await(ctx, keys[i], f)
	}
	return results
}

// await waits for f, the flight of key, or for ctx.
func (c *Cache[T]) await(ctx context.Context, key string, f *flight.Flight[T]) (T, error) {
	select {
	case <-f.Done():
		return f.Result()
	case <-ctx.Done():
	}
	select { // a result that arrived together with ctx's end wins
	case <-f.Done():
		return f.Result()
	default:
		var zero T
		return zero, fmt.Errorf("cachex: context done while fetching %q: %w", key, ctx.Err())
	}
}

func (c *Cache[T]) flightKey(key string) flightKey {
	if c.opts.fetchesPerKey == 1 {
		return flightKey{key: key}
	}
	return flightKey{key: key, slot: rand.IntN(c.opts.fetchesPerKey)}
}

// dropFlights unregisters every fetch of key without interrupting it: its
// waiters still get its result, and the next read starts a new fetch.
func (c *Cache[T]) dropFlights(key string) {
	for slot := range c.opts.fetchesPerKey {
		c.flights.Drop(flightKey{key: key, slot: slot})
	}
}

// claimed are the flights one request leads. Each gets exactly one result,
// published as soon as it is known; run makes sure a fetch that panics or
// calls runtime.Goexit still answers every waiter, with an error.
type claimed[T any] struct {
	c       *Cache[T]
	keys    []string
	fks     []flightKey
	flights []*flight.Flight[T]
	mu      sync.Mutex
	done    []bool
}

func (c *Cache[T]) claim(keys []string, fks []flightKey, flights []*flight.Flight[T]) *claimed[T] {
	return &claimed[T]{c: c, keys: keys, fks: fks, flights: flights, done: make([]bool, len(keys))}
}

// publish answers the waiters of flight i, once. The flight stays registered
// until forget (or the end of run), so a read that comes while the answer is
// being backfilled joins it instead of fetching again.
func (p *claimed[T]) publish(i int, value T, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done[i] {
		return
	}
	p.done[i] = true
	p.c.flights.Publish(p.flights[i], value, err)
}

// forget unregisters flight i, once its answer is backfilled.
func (p *claimed[T]) forget(idxs ...int) {
	for _, i := range idxs {
		p.c.flights.Forget(p.fks[i], p.flights[i])
	}
}

// answer publishes an answer that is not backfilled.
func (p *claimed[T]) answer(i int, value T, err error) {
	p.publish(i, value, err)
	p.forget(i)
}

// run runs body, which resolves flights idxs, and publishes an error for each
// of them still unanswered if body panics or calls runtime.Goexit.
func (p *claimed[T]) run(ctx context.Context, idxs []int, body func()) {
	returned := false
	defer p.forget(idxs...)
	defer func() {
		var err error
		if r := recover(); r != nil {
			keys := make([]string, len(idxs))
			for j, i := range idxs {
				keys[j] = p.keys[i]
			}
			p.c.opts.logger.ErrorContext(ctx, "cachex: panic during fetch",
				slog.Any("keys", keys), "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("cachex: panic during fetch: %v", r)
		} else if !returned {
			err = errors.New("cachex: fetch exited without returning (runtime.Goexit)")
		}
		if err != nil {
			var zero T
			for _, i := range idxs {
				p.publish(i, zero, err)
			}
		}
	}()
	body()
	returned = true
}

// resolve answers the claimed flights from below the first layer: it
// double-checks the first layer, reads the lower layers in turn, asks the
// source for what is left, and backfills each answer into the layers above
// where it was found. With refresh, stale entries below do not count.
func (c *Cache[T]) resolve(ctx context.Context, p *claimed[T], seen []uint64, refresh bool) {
	all := make([]int, len(p.keys))
	for i := range all {
		all[i] = i
	}
	p.run(ctx, all, func() {
		pending := c.doubleCheck(ctx, p, seen, all)
		// taken before reading below: a write after it voids the backfill
		gens := make([]uint64, len(p.keys))
		for _, i := range pending {
			gens[i] = c.stripes.For(p.keys[i]).Generation()
		}
		pending = c.readBelow(ctx, p, gens, pending, refresh)
		c.askSource(ctx, p, gens, pending)
	})
}

// shouldDoubleCheck reports whether a claimed fetch re-reads the first layer.
// In DoubleCheckAuto it does only if this Cache wrote the key's stripe since
// the request read the first layer (seen): otherwise the re-read would find
// the same miss. A write to another key of the stripe makes it re-read for
// nothing.
func (c *Cache[T]) shouldDoubleCheck(key string, seen uint64) bool {
	switch c.opts.doubleCheck {
	case DoubleCheckEnabled:
		return true
	case DoubleCheckDisabled:
		return false
	default:
		return c.stripes.For(key).Epoch() != seen
	}
}

// doubleCheck re-reads the first layer for the flights idxs that need it and
// answers those it finds fresh there; it returns the others.
func (c *Cache[T]) doubleCheck(ctx context.Context, p *claimed[T], seen []uint64, idxs []int) []int {
	if len(c.layers) == 0 {
		return idxs
	}
	var check, pending []int
	for _, i := range idxs {
		if c.shouldDoubleCheck(p.keys[i], seen[i]) {
			check = append(check, i)
		} else {
			pending = append(pending, i)
		}
	}
	if len(check) == 0 {
		return pending
	}
	cctx, cancel := context.WithTimeout(ctx, c.opts.fetchTimeout)
	found := readLayer(cctx, c.layers[0].backend, p.keys, check)
	cancel()
	now := c.opts.now()
	for _, i := range check {
		key := p.keys[i]
		if e, ok, err := found(key); ok && err == nil && e.state(now) == fresh {
			value, rerr := e.result()
			p.answer(i, value, rerr)
			continue
		}
		pending = append(pending, i) // a failed re-read falls through to the fetch
	}
	return pending
}

// fill is an answer to backfill: where it was found (a lower layer's entry,
// or the source's answer with no bounds), and the key's stripe generation from
// before it was read.
type fill[T any] struct {
	key   string
	gen   uint64
	entry Entry[T]
}

// readBelow reads the layers below the first for the flights idxs, answers and
// backfills those found usable, and returns the rest.
func (c *Cache[T]) readBelow(ctx context.Context, p *claimed[T], gens []uint64, idxs []int, refresh bool) []int {
	for li := 1; li < len(c.layers) && len(idxs) > 0; li++ {
		lctx, cancel := context.WithTimeout(ctx, c.opts.fetchTimeout)
		found := readLayer(lctx, c.layers[li].backend, p.keys, idxs)
		now := c.opts.now()
		var next, hit []int
		var fills []fill[T]
		var staleKeys []string
		for _, i := range idxs {
			key := p.keys[i]
			e, ok, err := found(key)
			if err != nil {
				var zero T
				p.answer(i, zero, fmt.Errorf("cachex: get %q from layer %d: %w", key, li, err))
				continue
			}
			if ok {
				if st := e.state(now); st == fresh || st == stale && !refresh {
					hit = append(hit, i)
					fills = append(fills, fill[T]{key: key, gen: gens[i], entry: e})
					if st == stale {
						staleKeys = append(staleKeys, key)
					}
					continue
				}
			}
			next = append(next, i)
		}
		for j, i := range hit {
			value, rerr := fills[j].entry.result()
			p.publish(i, value, rerr)
		}
		c.backfill(lctx, li, fills)
		cancel()
		p.forget(hit...)
		if len(staleKeys) > 0 {
			c.refresh(ctx, staleKeys, nil)
		}
		idxs = next
	}
	return idxs
}

// askSource fetches the flights idxs from the source: in chunk calls if it is
// a BatchSource, key by key otherwise, up to getManyConc calls at once. Each
// call's answers are backfilled and published as soon as it returns.
func (c *Cache[T]) askSource(ctx context.Context, p *claimed[T], gens []uint64, idxs []int) {
	switch len(idxs) {
	case 0:
		return
	case 1:
		c.askSourceOne(ctx, p, gens, idxs[0])
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, c.opts.getManyConc)
	spawn := func(part []int, body func()) {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			p.run(ctx, part, body)
		})
	}
	if c.batchSource != nil {
		for chunk := range slices.Chunk(idxs, cmp.Or(c.opts.getManyChunk, len(idxs))) {
			spawn(chunk, func() { c.askSourceChunk(ctx, p, gens, chunk) })
		}
	} else {
		for _, i := range idxs {
			spawn([]int{i}, func() { c.askSourceOne(ctx, p, gens, i) })
		}
	}
	wg.Wait()
}

func (c *Cache[T]) askSourceOne(ctx context.Context, p *claimed[T], gens []uint64, i int) {
	key := p.keys[i]
	sctx, cancel := context.WithTimeout(ctx, c.opts.fetchTimeout)
	defer cancel() // after the backfill, which the same timeout bounds
	value, err := c.source.Get(sctx, key)
	var zero T
	switch {
	case err == nil:
		p.publish(i, value, nil)
		c.backfillOne(sctx, len(c.layers), c.sourceFill(key, gens[i], value, false))
		p.forget(i)
	case errors.Is(err, ErrNotFound):
		p.publish(i, zero, ErrNotFound)
		c.backfillOne(sctx, len(c.layers), c.sourceFill(key, gens[i], zero, true))
		p.forget(i)
	default:
		p.answer(i, zero, fmt.Errorf("cachex: get %q from source: %w", key, err))
	}
}

func (c *Cache[T]) askSourceChunk(ctx context.Context, p *claimed[T], gens []uint64, idxs []int) {
	sctx, cancel := context.WithTimeout(ctx, c.opts.fetchTimeout)
	defer cancel() // after the backfill, which the same timeout bounds
	values, err := c.batchSource.GetMany(sctx, keysAt(p.keys, idxs))
	var zero T
	fills := make([]fill[T], 0, len(idxs))
	var answered []int
	for _, i := range idxs {
		key := p.keys[i]
		if kerr := errForKey(err, key); kerr != nil {
			p.answer(i, zero, fmt.Errorf("cachex: get %q from source: %w", key, kerr))
			continue
		}
		value, ok := values[key]
		fills = append(fills, c.sourceFill(key, gens[i], value, !ok))
		answered = append(answered, i)
	}
	for j, i := range answered {
		value, rerr := fills[j].entry.result()
		p.publish(i, value, rerr)
	}
	c.backfill(sctx, len(c.layers), fills)
	p.forget(answered...)
}

// sourceFill is the source's answer for key as a fill: no bounds but the
// value's max age (see WithMaxAge).
func (c *Cache[T]) sourceFill(key string, gen uint64, value T, notFound bool) fill[T] {
	now := c.opts.now()
	e := Entry[T]{Value: value, NotFound: notFound, CachedAt: now}
	if !notFound && c.maxAge != nil {
		e.ExpiresAt = now.Add(c.maxAge(value))
		e.FreshUntil = e.ExpiresAt
		if !e.ExpiresAt.After(now) {
			e.ExpiresAt = now // zero means no bound; this one is already over
		}
	}
	return fill[T]{key: key, gen: gen, entry: e}
}

// backfill writes answers found at layer origin (len(c.layers) for the source)
// into the layers above it, bottom up, unless a write of the key happened since
// its gen was taken. It holds each key's stripe for reading, so a write waits
// for it and the fill either lands before the write or is skipped; a write in
// progress skips it too (TryRLock): reads never wait on writes, at the cost of
// a spare miss. An answer a layer does not keep (an expired entry, a not-found
// without a not-found TTL) deletes the key there instead.
func (c *Cache[T]) backfill(ctx context.Context, origin int, fills []fill[T]) {
	if origin == 0 || len(fills) == 0 {
		return
	}
	if len(fills) == 1 {
		c.backfillOne(ctx, origin, fills[0])
		return
	}
	held := map[*stripe.Stripe]bool{}
	defer func() {
		for s, ok := range held {
			if ok {
				s.RUnlock()
			}
		}
	}()
	var ok []fill[T]
	filled := map[*stripe.Stripe]struct{}{}
	for _, f := range fills {
		s := c.stripes.For(f.key)
		h, seen := held[s]
		if !seen {
			h = s.TryRLock()
			held[s] = h
		}
		if h && s.Generation() == f.gen {
			ok = append(ok, f)
			filled[s] = struct{}{}
		}
	}
	if len(ok) == 0 {
		return
	}
	now := c.opts.now()
	for j := origin - 1; j >= 0; j-- {
		l := &c.layers[j]
		sets := map[string]Entry[T]{}
		var dels []string
		for _, f := range ok {
			e, keep := l.entry(f.entry.Value, f.entry.NotFound, f.entry.CachedAt, now, f.entry.FreshUntil, f.entry.ExpiresAt)
			if keep && now.Before(e.ExpiresAt) {
				sets[f.key] = e
			} else {
				dels = append(dels, f.key)
			}
		}
		if len(sets) > 0 {
			if err := l.backend.SetMany(ctx, sets); err != nil {
				c.opts.logger.WarnContext(ctx, "cachex: backfill failed", "layer", j, "error", err)
			}
		}
		if len(dels) > 0 {
			if err := l.backend.DelMany(ctx, dels); err != nil {
				c.opts.logger.WarnContext(ctx, "cachex: backfill failed", "layer", j, "error", err)
			}
		}
	}
	for s := range filled { // see stripe.Stripe.Epoch
		s.AddFill()
	}
}

// backfillOne is backfill for one answer, without the bookkeeping of many.
func (c *Cache[T]) backfillOne(ctx context.Context, origin int, f fill[T]) {
	s := c.stripes.For(f.key)
	if !s.TryRLock() {
		return
	}
	defer s.RUnlock()
	if s.Generation() != f.gen {
		return
	}
	now := c.opts.now()
	for j := origin - 1; j >= 0; j-- {
		l := &c.layers[j]
		var err error
		if e, keep := l.entry(f.entry.Value, f.entry.NotFound, f.entry.CachedAt, now, f.entry.FreshUntil, f.entry.ExpiresAt); keep && now.Before(e.ExpiresAt) {
			err = l.backend.Set(ctx, f.key, e)
		} else {
			err = l.backend.Del(ctx, f.key)
		}
		if err != nil {
			c.opts.logger.WarnContext(ctx, "cachex: backfill failed", "layer", j, "error", err)
		}
	}
	s.AddFill()
}

// refresh refetches keys in the background (stale entries below the first
// layer do not count), one refresh per key at a time. seen, if not nil, is
// each key's stripe epoch from before the caller read the first layer.
func (c *Cache[T]) refresh(ctx context.Context, keys []string, seen []uint64) {
	var own []string
	var ownSeen []uint64
	for i, key := range keys {
		if _, loaded := c.refreshing.LoadOrStore(key, struct{}{}); loaded {
			continue
		}
		own = append(own, key)
		if seen != nil {
			ownSeen = append(ownSeen, seen[i])
		} else {
			ownSeen = append(ownSeen, c.stripes.For(key).Epoch())
		}
	}
	if len(own) == 0 {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		for _, key := range own {
			c.refreshing.Delete(key)
		}
		return
	}
	c.wg.Add(1)
	c.mu.Unlock()
	ctx = sharedCtx(ctx)
	go func() {
		defer c.wg.Done()
		defer func() {
			for _, key := range own {
				c.refreshing.Delete(key)
			}
		}()
		for i, r := range c.fetchMany(ctx, own, ownSeen, true) {
			if r.err != nil && !errors.Is(r.err, ErrNotFound) {
				c.opts.logger.ErrorContext(ctx, "cachex: background refresh failed", "key", own[i], "error", r.err)
			}
		}
	}()
}

// readLayer reads keys idxs from b, and returns what it found for a key:
// one Get for one key, which spares the common single-key miss a map.
func readLayer[T any](ctx context.Context, b Backend[T], keys []string, idxs []int) func(key string) (Entry[T], bool, error) {
	if len(idxs) == 1 {
		e, ok, err := b.Get(ctx, keys[idxs[0]])
		return func(string) (Entry[T], bool, error) { return e, ok, err }
	}
	es, err := b.GetMany(ctx, keysAt(keys, idxs))
	return func(key string) (Entry[T], bool, error) {
		if kerr := errForKey(err, key); kerr != nil {
			return Entry[T]{}, false, kerr
		}
		e, ok := es[key]
		return e, ok, nil
	}
}

func keysAt(keys []string, idxs []int) []string {
	out := make([]string, len(idxs))
	for j, i := range idxs {
		out[j] = keys[i]
	}
	return out
}

func uniqueKeys(keys []string) []string {
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			out = append(out, key)
		}
	}
	return out
}
