package cachex

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

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
		seen = epoch(c.stripes, key) // before reading, see shouldDoubleCheck
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
	if !supportedKey(key) { // checked on a miss only: no layer stores such a key
		var zero T
		return zero, unsupportedKey(key)
	}
	fk := flightKey{key: key}
	f, leader := c.flights.Claim(fk)
	if leader {
		p := c.claim(false)
		p.add(key, fk, f, seen)
		fctx := sharedCtx(ctx)
		c.background(func() { c.resolve(fctx, p, 1) })
	}
	return c.await(ctx, key, f)
}

// GetMany returns the values of keys that exist, with the semantics of calling
// Get for each: the first layer is read in one call, the layers below and a
// BatchSource in one call per chunk (see WithGetManyChunkSize; by default one
// chunk), any other source key by key (see WithGetManyConcurrency), and keys
// being fetched by other calls are waited for, not fetched again. A key the
// source does not have is absent from the map. If some keys failed, the map
// holds every other key and the error is a *BatchError listing the failed
// ones.
func (c *Cache[T]) GetMany(ctx context.Context, keys []string) (map[string]T, error) {
	errs := map[string]error{}
	keys = slices.DeleteFunc(uniqueKeys(keys), func(key string) bool {
		if !supportedKey(key) {
			errs[key] = unsupportedKey(key)
			return true
		}
		return false
	})
	out := make(map[string]T, len(keys))

	seen := make([]uint64, len(keys))
	missKeys, missSeen := keys, seen
	if len(c.layers) > 0 {
		for i, key := range keys {
			seen[i] = epoch(c.stripes, key) // before reading, see shouldDoubleCheck
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

	for i, r := range c.fetchMany(ctx, missKeys, missSeen) {
		key := missKeys[i]
		switch {
		case r.err == nil:
			out[key] = r.value
		case r.err != ErrNotFound: //nolint:errorlint // a flight's not-found is ErrNotFound itself, see claimed
			errs[key] = r.err
		}
	}
	return out, batchError(errs)
}

// sourceCall is a fetch asking the source for a key; another fetch of the
// key takes its answer instead of asking again (see askSource).
type sourceCall[T any] struct {
	flight  *flight.Flight[T]
	waiters atomic.Int32 // fetches waiting for it, for tests
}

// result is what a fetch answered for a key, as its flight holds it (see
// claimed).
type result[T any] struct {
	value T
	err   error
}

// fetchMany fetches keys in chunks (see fetchChunks) and waits for them all,
// including those led by other requests. seen is each key's stripe epoch from
// before the caller read the first layer. Once ctx ends no more chunks start:
// nobody waits for their keys.
func (c *Cache[T]) fetchMany(ctx context.Context, keys []string, seen []uint64) []result[T] {
	if len(keys) == 0 {
		return nil
	}
	flights := c.fetchChunks(sharedCtx(ctx), ctx.Done(), keys, seen, false, c.background)
	results := make([]result[T], len(keys))
	for i, f := range flights {
		if f == nil { // its chunk never started
			results[i].err = fmt.Errorf("cachex: context done while fetching %q: %w", keys[i], ctx.Err())
			continue
		}
		results[i].value, results[i].err = c.await(ctx, keys[i], f)
	}
	return results
}

// fetchChunks claims keys a chunk at a time (see WithGetManyChunkSize) and
// resolves each chunk's own flights with ctx, in a goroutine started by spawn.
// A chunk starts only when the source has room for its requests (one call for
// a BatchSource, a Get per key otherwise, up to WithGetManyConcurrency in
// all), and none starts once stop is closed. So the keys of a chunk not
// started yet are not claimed, and another request of one of them fetches it
// instead of waiting. It returns each key's flight, nil for a key whose chunk
// never started.
func (c *Cache[T]) fetchChunks(ctx context.Context, stop <-chan struct{}, keys []string, seen []uint64, refresh bool,
	spawn func(func())) []*flight.Flight[T] {
	flights := make([]*flight.Flight[T], len(keys))
	size := cmp.Or(c.opts.getManyChunk, len(keys))
	slots := make(chan struct{}, c.opts.getManyConc)
	for start := 0; start < len(keys); start += size {
		select {
		case <-stop:
			return flights
		default:
		}
		end := min(start+size, len(keys))
		conc := 1
		if c.batchSource == nil {
			conc = min(end-start, c.opts.getManyConc)
		}
		for range conc {
			select {
			case slots <- struct{}{}:
			case <-stop:
				return flights
			}
		}
		release := func() {
			for range conc {
				<-slots
			}
		}
		var p *claimed[T]
		for i := start; i < end; i++ {
			fk := flightKey{key: keys[i], refresh: refresh}
			f, leader := c.flights.Claim(fk)
			flights[i] = f
			if leader {
				if p == nil {
					p = c.claim(refresh)
				}
				p.add(keys[i], fk, f, seen[i])
			}
		}
		if p == nil {
			release()
			continue
		}
		spawn(func() {
			defer release()
			c.resolve(ctx, p, conc)
		})
	}
	return flights
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

// dropFlights unregisters both fetches of key, the read's and the refresh's,
// and its call to the source, without interrupting them: their waiters still
// get their results, and the next read starts a new fetch, which asks the
// source anew.
//
// The call to the source goes first: a read that joins it must have claimed
// its flight before, so the drop of the flights removes that one too. The
// other way round, a read claiming in between would join the old call with
// a flight that stays registered, and reads after the write would join it.
func (c *Cache[T]) dropFlights(key string) {
	c.sourcing.Delete(key) // a fetch after the write asks the source anew
	c.flights.Drop(flightKey{key: key}, flightKey{key: key, refresh: true})
}

// claimed are the flights one request leads, and what their fetch knows of
// each key. Each flight gets exactly one result, published as soon as it is
// known and before it is backfilled; the flight stays registered until the
// backfill is done, so a read that comes meanwhile joins it instead of
// fetching again. run makes sure a fetch that panics or calls runtime.Goexit
// still answers every waiter, with an error. A flight's result is a value, or
// ErrNotFound itself for a key that does not exist, or a failure, which is
// always wrapped: so a not-found is told by identity, and an error that only
// wraps ErrNotFound (a layer's, say) stays a failure. The source's errors are
// classified in askSourceOne and askSourceChunk, nowhere else.
type claimed[T any] struct {
	c       *Cache[T]
	keys    []string        // the keys, for the backends' calls
	ks      []claimedKey[T] // what the fetch knows of each key, in the same order
	refresh bool            // stale entries below do not count
}

// claimedKey is what a fetch knows of one key it leads.
type claimedKey[T any] struct {
	fk     flightKey
	flight *flight.Flight[T]
	seen   uint64         // stripe epoch from before the caller read the first layer
	gen    uint64         // write generation from before the fetch read below it
	leases *keyLeases     // nil: none taken or refused (see Leaser)
	source *sourceCall[T] // set while the fetch asks the source for the key (see askSource)
}

func (c *Cache[T]) claim(refresh bool) *claimed[T] { return &claimed[T]{c: c, refresh: refresh} }

// add makes p lead the flight f of key.
func (p *claimed[T]) add(key string, fk flightKey, f *flight.Flight[T], seen uint64) {
	p.keys = append(p.keys, key)
	p.ks = append(p.ks, claimedKey[T]{fk: fk, flight: f, seen: seen})
}

// forget unregisters the flight i.
func (p *claimed[T]) forget(i int) {
	k := &p.ks[i]
	p.c.flights.Forget(k.fk, k.flight)
	if k.source != nil {
		p.c.sourcing.CompareAndDelete(p.keys[i], k.source)
		k.source = nil
	}
}

// publish answers flight i with value or err, which is not backfilled.
func (p *claimed[T]) publish(i int, value T, err error) {
	p.c.flights.Publish(p.ks[i].flight, value, err)
	p.forget(i)
}

// settle answers flights idxs with entries, found at layer origin
// (len(c.layers) for the source's answers): it publishes them, backfills them
// into the layers above origin, and only then unregisters the flights.
func (p *claimed[T]) settle(ctx context.Context, origin int, idxs []int, entries []Entry[T]) {
	for j, i := range idxs {
		value, err := entries[j].result()
		p.c.flights.Publish(p.ks[i].flight, value, err)
	}
	if origin > 0 {
		if len(idxs) == 1 {
			p.c.backfillOne(ctx, origin, fill[T]{key: p.keys[idxs[0]], gen: p.ks[idxs[0]].gen, entry: entries[0], leases: p.ks[idxs[0]].leases})
		} else {
			fills := make([]fill[T], len(idxs))
			for j, i := range idxs {
				fills[j] = fill[T]{key: p.keys[i], gen: p.ks[i].gen, entry: entries[j], leases: p.ks[i].leases}
			}
			p.c.backfill(ctx, origin, fills)
		}
	}
	for _, i := range idxs {
		p.forget(i)
	}
}

// run runs body, which resolves flights idxs, and publishes an error for each
// of them still unanswered if body panics or calls runtime.Goexit. Then it
// releases the leases they hold still.
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
		var zero T
		for _, i := range idxs {
			if err != nil {
				p.c.flights.Publish(p.ks[i].flight, zero, err) // does nothing if answered
			}
			p.forget(i)
		}
		if p.c.leasers != nil {
			p.release(ctx, idxs)
		}
	}()
	body()
	returned = true
}

// resolve answers the claimed flights from below the first layer: it
// double-checks the first layer, reads the lower layers in turn, asks the
// source for what is left, and backfills each answer into the layers above
// where it was found, making up to conc source requests at once.
func (c *Cache[T]) resolve(ctx context.Context, p *claimed[T], conc int) {
	all := make([]int, len(p.keys))
	for i := range all {
		all[i] = i
	}
	p.run(ctx, all, func() {
		pending := c.doubleCheck(ctx, p, all)
		for _, i := range pending { // a write after this voids the backfill
			p.ks[i].gen = c.generation(p.keys[i])
		}
		if c.leasers != nil {
			c.lease(ctx, p, 0, pending)
		}
		pending = c.readBelow(ctx, p, pending)
		if p.refresh && c.leasers != nil {
			pending = p.skipLeased(pending)
		}
		c.askSource(ctx, p, pending, conc)
	})
}

// shouldDoubleCheck reports whether a claimed fetch re-reads the first layer:
// only if this Cache wrote the key's stripe since the request read the first
// layer (seen), since otherwise the re-read would find the same miss. A write
// to another key of the stripe makes it re-read for nothing. What other
// instances write to a shared first layer is not worth a re-read: they miss
// at the same moment as this one, so none has filled it yet (ADR 0008).
func (c *Cache[T]) shouldDoubleCheck(key string, seen uint64) bool {
	return epoch(c.stripes, key) != seen
}

// doubleCheck re-reads the first layer for the flights idxs that need it and
// answers those it finds fresh there; it returns the others.
func (c *Cache[T]) doubleCheck(ctx context.Context, p *claimed[T], idxs []int) []int {
	if len(c.layers) == 0 {
		return idxs
	}
	var check, pending []int
	for _, i := range idxs {
		if c.shouldDoubleCheck(p.keys[i], p.ks[i].seen) {
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
	key    string
	gen    uint64
	entry  Entry[T]
	leases *keyLeases // the fetch's leases (see claimedKey)
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
				var zero T
				p.publish(i, zero, readError(key, li, err))
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
		if c.leasers != nil {
			c.lease(ctx, p, li, next)
		}
		idxs = next
	}
	return idxs
}

// askSource fetches the flights idxs from the source: in one call if it is a
// BatchSource, key by key otherwise, up to conc calls at once. Each call's
// answers are settled as soon as it returns, and its backfill shares its
// timeout.
//
// A key is asked of the source once at a time: a key another fetch (a read's
// or a refresh's) is asking for already takes that fetch's answer, once this
// fetch's own calls are done. Two overlapping answers cannot be ordered, and
// the older could land last. The other fetch unregisters its call only after
// its backfill, so a later call of the key starts after the layers hold the
// earlier answer; a write unregisters it at once (see dropFlights).
func (c *Cache[T]) askSource(ctx context.Context, p *claimed[T], idxs []int, conc int) {
	if len(idxs) != 1 {
		c.askSourceMany(ctx, p, idxs, conc)
		return
	}
	// the common single-key miss, with a small frame: a bigger one grows the
	// fetch goroutine's stack, and copying it doubles the cost of a miss
	if call := c.claimSource(p, idxs[0]); call != nil {
		c.joinSource(p, idxs[0], call)
		return
	}
	c.askSourceOne(ctx, p, idxs[0])
}

// askSourceMany is askSource for several keys.
func (c *Cache[T]) askSourceMany(ctx context.Context, p *claimed[T], idxs []int, conc int) {
	var joined []int
	var calls []*sourceCall[T]
	own := idxs[:0:0]
	for _, i := range idxs {
		if call := c.claimSource(p, i); call != nil {
			joined, calls = append(joined, i), append(calls, call)
		} else {
			own = append(own, i)
		}
	}
	c.askSourceOwn(ctx, p, own, conc)
	for j, i := range joined {
		c.joinSource(p, i, calls[j])
	}
}

// claimSource registers flight i as asking the source for its key, or
// returns the call of the fetch that already is.
//
//go:noinline
func (c *Cache[T]) claimSource(p *claimed[T], i int) *sourceCall[T] {
	call := &sourceCall[T]{flight: p.ks[i].flight}
	if v, loaded := c.sourcing.LoadOrStore(p.keys[i], call); loaded {
		return v.(*sourceCall[T])
	}
	p.ks[i].source = call
	return nil
}

// joinSource answers flight i with the answer of call, another fetch's
// request to the source; that fetch backfills it.
//
//go:noinline
func (c *Cache[T]) joinSource(p *claimed[T], i int, call *sourceCall[T]) {
	call.waiters.Add(1)
	<-call.flight.Done() // published within its own fetch, whatever happens to it
	call.waiters.Add(-1)
	value, err := call.flight.Result()
	p.publish(i, value, err)
}

// askSourceOwn is askSource for keys no other fetch is asking for.
func (c *Cache[T]) askSourceOwn(ctx context.Context, p *claimed[T], idxs []int, conc int) {
	switch {
	case len(idxs) == 0:
		return
	case len(idxs) == 1:
		c.askSourceOne(ctx, p, idxs[0])
		return
	case c.batchSource != nil:
		c.askSourceChunk(ctx, p, idxs)
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, conc)
	for _, i := range idxs {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			p.run(ctx, []int{i}, func() { c.askSourceOne(ctx, p, i) })
		})
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
		var zero T
		p.publish(i, zero, fmt.Errorf("cachex: get %q from source: %w", key, err))
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
			var zero T
			p.publish(i, zero, ErrNotFound)
			continue
		case kerr != nil:
			var zero T
			p.publish(i, zero, fmt.Errorf("cachex: get %q from source: %w", key, kerr))
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
// skipped, at the cost of a spare miss. Backfills of a key land one at a time,
// and a copy of an entry from a lower layer does not replace one the layer
// holds from a later answer of the source (a later CachedAt): held up, it
// would otherwise undo a refresh. An answer of the source needs no such check
// (the source is asked once at a time, see askSource, so no other answer is
// newer), nor does a layer that leases (only the holder of its lease writes
// it).
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
		if c.leasers != nil && c.leasers[j] != nil {
			c.putLeased(ctx, j, fills, ok, now)
			continue
		}
		var newer map[string]error
		if origin < len(c.layers) {
			newer = c.newerIn(ctx, j, answers)
		}
		if failed := c.layers[j].putMany(ctx, answers, nil, newer, now); len(failed) > 0 {
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
		if c.leasers != nil && c.leasers[j] != nil {
			c.putLeasedOne(ctx, j, &f, now)
			continue
		}
		if origin < len(c.layers) && c.newerInOne(ctx, j, f.key, f.entry.CachedAt) {
			continue
		}
		if err := c.layers[j].put(ctx, f.key, f.entry, now); err != nil {
			c.opts.logger.WarnContext(ctx, "cachex: backfill failed", "layer", j, "error", err)
		}
	}
}

// newerIn returns the keys of answers whose entry in layer j comes from a
// later answer of the source: a backfill leaves them alone. A key the layer
// fails to read is written as before.
func (c *Cache[T]) newerIn(ctx context.Context, j int, answers map[string]Entry[T]) map[string]error {
	es, _ := c.layers[j].backend.GetMany(ctx, slices.Collect(maps.Keys(answers)))
	var newer map[string]error
	for key, e := range es {
		if e.CachedAt.After(answers[key].CachedAt) {
			if newer == nil {
				newer = map[string]error{}
			}
			newer[key] = nil
		}
	}
	return newer
}

// newerInOne is newerIn for one key, out of backfillOne so that its frame
// does not grow the fetch goroutine's stack.
//
//go:noinline
func (c *Cache[T]) newerInOne(ctx context.Context, j int, key string, cachedAt time.Time) bool {
	e, ok, err := c.layers[j].backend.Get(ctx, key)
	return err == nil && ok && e.CachedAt.After(cachedAt)
}

// refresh refetches keys in the background (stale entries below the first
// layer do not count), one refresh per key at a time, in chunks as GetMany
// fetches. seen, if not nil, is each key's stripe epoch from before the caller
// read the first layer. Its fetches run in the one goroutine Close waits for,
// so that Close also waits for their backfills.
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
			ownSeen = append(ownSeen, epoch(c.stripes, key))
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
		var wg sync.WaitGroup
		flights := c.fetchChunks(ctx, nil, own, ownSeen, true, wg.Go)
		wg.Wait()
		for i, f := range flights {
			<-f.Done()
			if _, err := f.Result(); err != nil && err != ErrNotFound && err != errLeased { //nolint:errorlint // see claimed
				c.opts.logger.ErrorContext(ctx, "cachex: background refresh failed", "key", own[i], "error", err)
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
