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
)

// Get returns key's value from the first layer with a usable entry, or else
// from the source; concurrent misses of a key share one fetch, whose result is
// backfilled into the layers above where it was found. A stale entry is
// returned at once and refreshed in the background. Get returns ErrNotFound
// for a key the source does not have.
func (c *Cache[T]) Get(ctx context.Context, key string) (T, error) {
	var seen uint64
	if len(c.layers) > 0 {
		seen = c.epoch(key) // before reading, see shouldDoubleCheck
		e, ok, err := c.layers[0].backend.Get(ctx, key)
		if err != nil {
			var zero T
			return zero, readError(key, 0, err)
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
	fk := c.flightKey(key, false)
	f, leader := c.flights.Claim(fk)
	if leader {
		p := c.claim([]string{key}, []flightKey{fk}, []*flight.Flight[Entry[T]]{f}, []uint64{seen}, false)
		fctx := sharedCtx(ctx)
		c.background(func() { c.resolve(fctx, p) })
	}
	e, err := c.await(ctx, key, f)
	if err != nil {
		var zero T
		return zero, err
	}
	return e.result()
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
			seen[i] = c.epoch(key) // before reading, see shouldDoubleCheck
		}
		es, err := c.layers[0].backend.GetMany(ctx, keys)
		now := c.opts.now()
		var staleKeys []string
		var staleSeen []uint64
		missKeys, missSeen = nil, nil
		for i, key := range keys {
			if kerr := errForKey(err, key); kerr != nil {
				errs[key] = readError(key, 0, kerr)
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
		case r.err != nil:
			errs[key] = r.err
		case !r.entry.NotFound:
			out[key] = r.entry.Value
		}
	}
	return out, batchError(errs)
}

// result is what a fetch answered for a key: an entry (a value or a
// not-found), or an error.
type result[T any] struct {
	entry Entry[T]
	err   error
}

// fetchMany fetches keys through the flight group: it claims every key,
// resolves the ones it leads in one background call, and waits for all of
// them (including those led by others). seen is each key's stripe epoch from
// before the caller read the first layer. A refresh claims flights of its own
// (see flightKey).
func (c *Cache[T]) fetchMany(ctx context.Context, keys []string, seen []uint64, refresh bool) []result[T] {
	if len(keys) == 0 {
		return nil
	}
	flights := make([]*flight.Flight[Entry[T]], len(keys))
	var ownKeys []string
	var ownFKs []flightKey
	var ownFlights []*flight.Flight[Entry[T]]
	var ownSeen []uint64
	for i, key := range keys {
		fk := c.flightKey(key, refresh)
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
		fctx, p := sharedCtx(ctx), c.claim(ownKeys, ownFKs, ownFlights, ownSeen, refresh)
		c.background(func() { c.resolve(fctx, p) })
	}
	results := make([]result[T], len(keys))
	for i, f := range flights {
		results[i].entry, results[i].err = c.await(ctx, keys[i], f)
	}
	return results
}

// await waits for f, the flight of key, or for ctx.
func (c *Cache[T]) await(ctx context.Context, key string, f *flight.Flight[Entry[T]]) (Entry[T], error) {
	select {
	case <-f.Done():
		return f.Result()
	case <-ctx.Done():
	}
	select { // a result that arrived together with ctx's end wins
	case <-f.Done():
		return f.Result()
	default:
		return Entry[T]{}, fmt.Errorf("cachex: context done while fetching %q: %w", key, ctx.Err())
	}
}

func (c *Cache[T]) flightKey(key string, refresh bool) flightKey {
	if c.opts.fetchesPerKey == 1 {
		return flightKey{key: key, refresh: refresh}
	}
	return flightKey{key: key, slot: rand.IntN(c.opts.fetchesPerKey), refresh: refresh}
}

// dropFlights unregisters every fetch of key without interrupting it: its
// waiters still get its result, and the next read starts a new fetch.
func (c *Cache[T]) dropFlights(key string) {
	if c.opts.fetchesPerKey == 1 {
		c.flights.Drop(flightKey{key: key}, flightKey{key: key, refresh: true})
		return
	}
	fks := make([]flightKey, 0, 2*c.opts.fetchesPerKey)
	for slot := range c.opts.fetchesPerKey {
		fks = append(fks, flightKey{key: key, slot: slot}, flightKey{key: key, slot: slot, refresh: true})
	}
	c.flights.Drop(fks...)
}

// claimed are the flights one request leads, and what their fetch knows of
// each key. Each flight gets exactly one result, published as soon as it is
// known and before it is backfilled; the flight stays registered until the
// backfill is done, so a read that comes meanwhile joins it instead of
// fetching again. run makes sure a fetch that panics or calls runtime.Goexit
// still answers every waiter, with an error.
type claimed[T any] struct {
	c       *Cache[T]
	keys    []string
	fks     []flightKey
	flights []*flight.Flight[Entry[T]]
	seen    []uint64 // stripe epochs from before the caller read the first layer
	gens    []uint64 // write generations from before the fetch read below it
	refresh bool     // stale entries below do not count
}

func (c *Cache[T]) claim(keys []string, fks []flightKey, flights []*flight.Flight[Entry[T]], seen []uint64, refresh bool) *claimed[T] {
	return &claimed[T]{c: c, keys: keys, fks: fks, flights: flights, seen: seen, gens: make([]uint64, len(keys)), refresh: refresh}
}

// publish answers flight i with e or err, which is not backfilled.
func (p *claimed[T]) publish(i int, e Entry[T], err error) {
	p.c.flights.Publish(p.flights[i], e, err)
	p.c.flights.Forget(p.fks[i], p.flights[i])
}

// settle answers flights idxs with entries, found at layer origin
// (len(c.layers) for the source's answers): it publishes them, backfills them
// into the layers above origin, and only then unregisters the flights.
func (p *claimed[T]) settle(ctx context.Context, origin int, idxs []int, entries []Entry[T]) {
	for j, i := range idxs {
		p.c.flights.Publish(p.flights[i], entries[j], nil)
	}
	if origin > 0 {
		if len(idxs) == 1 {
			p.c.backfillOne(ctx, origin, fill[T]{key: p.keys[idxs[0]], gen: p.gens[idxs[0]], entry: entries[0]})
		} else {
			fills := make([]fill[T], len(idxs))
			for j, i := range idxs {
				fills[j] = fill[T]{key: p.keys[i], gen: p.gens[i], entry: entries[j]}
			}
			p.c.backfill(ctx, origin, fills)
		}
	}
	for _, i := range idxs {
		p.c.flights.Forget(p.fks[i], p.flights[i])
	}
}

// run runs body, which resolves flights idxs, and publishes an error for each
// of them still unanswered if body panics or calls runtime.Goexit.
func (p *claimed[T]) run(ctx context.Context, idxs []int, body func()) {
	returned := false
	defer func() {
		var err error
		if r := recover(); r != nil {
			p.c.opts.logger.ErrorContext(ctx, "cachex: panic during fetch",
				slog.Any("keys", keysAt(p.keys, idxs)), "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("cachex: panic during fetch: %v", r)
		} else if !returned {
			err = errors.New("cachex: fetch exited without returning (runtime.Goexit)")
		}
		for _, i := range idxs {
			if err != nil {
				p.c.flights.Publish(p.flights[i], Entry[T]{}, err) // does nothing if answered
			}
			p.c.flights.Forget(p.fks[i], p.flights[i])
		}
	}()
	body()
	returned = true
}

// resolve answers the claimed flights from below the first layer: it
// double-checks the first layer, reads the lower layers in turn, asks the
// source for what is left, and backfills each answer into the layers above
// where it was found.
func (c *Cache[T]) resolve(ctx context.Context, p *claimed[T]) {
	all := make([]int, len(p.keys))
	for i := range all {
		all[i] = i
	}
	p.run(ctx, all, func() {
		pending := c.doubleCheck(ctx, p, all)
		for _, i := range pending { // a write after this voids the backfill
			p.gens[i] = c.generation(p.keys[i])
		}
		pending = c.readBelow(ctx, p, pending)
		c.askSource(ctx, p, pending)
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
		return c.epoch(key) != seen
	}
}

// doubleCheck re-reads the first layer for the flights idxs that need it and
// answers those it finds fresh there; it returns the others.
func (c *Cache[T]) doubleCheck(ctx context.Context, p *claimed[T], idxs []int) []int {
	if len(c.layers) == 0 {
		return idxs
	}
	var check, pending []int
	for _, i := range idxs {
		if c.shouldDoubleCheck(p.keys[i], p.seen[i]) {
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
	var hit []int
	var entries []Entry[T]
	for _, i := range check {
		if e, ok, err := found(p.keys[i]); ok && err == nil && e.state(now) == fresh {
			hit = append(hit, i)
			entries = append(entries, e)
			continue
		}
		pending = append(pending, i) // a failed re-read falls through to the fetch
	}
	p.settle(ctx, 0, hit, entries)
	return pending
}

// fill is an answer to backfill: where it was found (a lower layer's entry,
// or the source's answer), and the key's write generation from before it was
// read.
type fill[T any] struct {
	key   string
	gen   uint64
	entry Entry[T]
}

// readBelow reads the layers below the first for the flights idxs, settles
// those found usable, and returns the rest. A layer's backfill shares the
// timeout of its read.
func (c *Cache[T]) readBelow(ctx context.Context, p *claimed[T], idxs []int) []int {
	for li := 1; li < len(c.layers) && len(idxs) > 0; li++ {
		lctx, cancel := context.WithTimeout(ctx, c.opts.fetchTimeout)
		found := readLayer(lctx, c.layers[li].backend, p.keys, idxs)
		now := c.opts.now()
		var next, hit []int
		var entries []Entry[T]
		var staleKeys []string
		for _, i := range idxs {
			key := p.keys[i]
			e, ok, err := found(key)
			if err != nil {
				p.publish(i, Entry[T]{}, readError(key, li, err))
				continue
			}
			if ok {
				if st := e.state(now); st == fresh || st == stale && !p.refresh {
					hit = append(hit, i)
					entries = append(entries, e)
					if st == stale {
						staleKeys = append(staleKeys, key)
					}
					continue
				}
			}
			next = append(next, i)
		}
		p.settle(lctx, li, hit, entries)
		cancel()
		if len(staleKeys) > 0 {
			c.refresh(ctx, staleKeys, nil)
		}
		idxs = next
	}
	return idxs
}

// askSource fetches the flights idxs from the source: in chunk calls if it is
// a BatchSource, key by key otherwise, up to getManyConc calls at once. Each
// call's answers are settled as soon as it returns, and its backfill shares
// its timeout.
func (c *Cache[T]) askSource(ctx context.Context, p *claimed[T], idxs []int) {
	switch len(idxs) {
	case 0:
		return
	case 1:
		c.askSourceOne(ctx, p, idxs[0])
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
			spawn(chunk, func() { c.askSourceChunk(ctx, p, chunk) })
		}
	} else {
		for _, i := range idxs {
			spawn([]int{i}, func() { c.askSourceOne(ctx, p, i) })
		}
	}
	wg.Wait()
}

func (c *Cache[T]) askSourceOne(ctx context.Context, p *claimed[T], i int) {
	key := p.keys[i]
	sctx, cancel := context.WithTimeout(ctx, c.opts.fetchTimeout)
	defer cancel()
	value, err := c.source.Get(sctx, key)
	switch {
	case err == nil:
		p.settle(sctx, len(c.layers), []int{i}, []Entry[T]{c.answer(value, false)})
	case errors.Is(err, ErrNotFound):
		var zero T
		p.settle(sctx, len(c.layers), []int{i}, []Entry[T]{c.answer(zero, true)})
	default:
		p.publish(i, Entry[T]{}, fmt.Errorf("cachex: get %q from source: %w", key, err))
	}
}

func (c *Cache[T]) askSourceChunk(ctx context.Context, p *claimed[T], idxs []int) {
	sctx, cancel := context.WithTimeout(ctx, c.opts.fetchTimeout)
	defer cancel()
	values, err := c.batchSource.GetMany(sctx, keysAt(p.keys, idxs))
	answered := make([]int, 0, len(idxs))
	entries := make([]Entry[T], 0, len(idxs))
	for _, i := range idxs {
		key := p.keys[i]
		switch kerr := errForKey(err, key); {
		case errors.Is(kerr, ErrNotFound):
			// a misuse (see BatchSource): not found, but not cached
			p.publish(i, Entry[T]{NotFound: true}, nil)
			continue
		case kerr != nil:
			p.publish(i, Entry[T]{}, fmt.Errorf("cachex: get %q from source: %w", key, kerr))
			continue
		}
		value, ok := values[key]
		answered = append(answered, i)
		entries = append(entries, c.answer(value, !ok))
	}
	p.settle(sctx, len(c.layers), answered, entries)
}

// backfill writes answers found at layer origin into the layers above it,
// bottom up, under the write-order guard (see beginFills): a fill whose key
// was written since its generation was taken, or is being written, is
// skipped, at the cost of a spare miss.
func (c *Cache[T]) backfill(ctx context.Context, origin int, fills []fill[T]) {
	ok, end := c.beginFills(fills)
	defer end()
	if len(ok) == 0 {
		return
	}
	answers := make(map[string]Entry[T], len(ok))
	for _, i := range ok {
		answers[fills[i].key] = fills[i].entry
	}
	now := c.opts.now()
	for j := origin - 1; j >= 0; j-- {
		if failed := c.layers[j].putMany(ctx, answers, nil, nil, now); len(failed) > 0 {
			c.opts.logger.WarnContext(ctx, "cachex: backfill failed", "layer", j, "error", batchError(failed))
		}
	}
}

// backfillOne is backfill for one answer, without the bookkeeping of many.
func (c *Cache[T]) backfillOne(ctx context.Context, origin int, f fill[T]) {
	if !c.beginFill(f.key, f.gen) {
		return
	}
	defer c.endFill(f.key)
	now := c.opts.now()
	for j := origin - 1; j >= 0; j-- {
		if err := c.layers[j].put(ctx, f.key, f.entry, now); err != nil {
			c.opts.logger.WarnContext(ctx, "cachex: backfill failed", "layer", j, "error", err)
		}
	}
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
			ownSeen = append(ownSeen, c.epoch(key))
		}
	}
	if len(own) == 0 {
		return
	}
	forget := func() {
		for _, key := range own {
			c.refreshing.Delete(key)
		}
	}
	done, ok := c.track()
	if !ok { // closed: no more refreshes
		forget()
		return
	}
	ctx = sharedCtx(ctx)
	go func() {
		defer done()
		defer forget()
		for i, r := range c.fetchMany(ctx, own, ownSeen, true) {
			if r.err != nil {
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

// readError is a failure to read key from layer li. A backend reports a
// missing key with false, never with an error, so its error is a failure even
// if it wraps ErrNotFound: errors.Is does not find ErrNotFound in a readError.
func readError(key string, li int, err error) error {
	return &layerError{msg: fmt.Sprintf("cachex: get %q from layer %d: %v", key, li, err), err: err}
}

type layerError struct {
	msg string
	err error
}

func (e *layerError) Error() string { return e.msg }

func (e *layerError) Unwrap() error {
	if errors.Is(e.err, ErrNotFound) {
		return nil
	}
	return e.err
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
