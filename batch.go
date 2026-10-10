package cachex

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/theplant/cachex/internal/flight"
	"github.com/theplant/cachex/internal/stripe"
)

var _ BatchUpstream[any] = &Client[any]{}

// claimed are flights one request has claimed. Each gets exactly one result,
// published as soon as it is known; run makes sure an upstream that panics or
// calls runtime.Goexit still answers every waiter, with an error.
type claimed[T any] struct {
	c       *Client[T]
	keys    []string
	sfKeys  []string
	flights []*flight.Flight[T]
	mu      sync.Mutex
	done    []bool
}

func (c *Client[T]) claimedFlights(keys, sfKeys []string, flights []*flight.Flight[T]) *claimed[T] {
	return &claimed[T]{c: c, keys: keys, sfKeys: sfKeys, flights: flights, done: make([]bool, len(keys))}
}

// publish answers the waiters of flight i, once.
func (p *claimed[T]) publish(i int, r result[T]) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done[i] {
		return
	}
	p.done[i] = true
	p.c.flights.Finish(p.sfKeys[i], p.flights[i], r.value, r.err)
}

// run runs body, which fetches flights idxs, and publishes an error for each
// of them still unanswered if body panics or calls runtime.Goexit.
func (p *claimed[T]) run(ctx context.Context, idxs []int, body func()) {
	returned := false
	defer func() {
		var err error
		if r := recover(); r != nil {
			keys := make([]string, len(idxs))
			for j, i := range idxs {
				keys[j] = p.keys[i]
			}
			attr := slog.Any("keys", keys)
			if len(keys) == 1 {
				attr = slog.String("key", keys[0])
			}
			p.c.logger.ErrorContext(ctx, "panic during upstream fetch",
				attr,
				"panic", r,
				"stack", string(debug.Stack()))
			err = fmt.Errorf("panic during upstream fetch: %v", r)
		} else if !returned {
			err = errors.New("upstream fetch exited without returning (runtime.Goexit)")
		}
		if err != nil {
			for _, i := range idxs {
				p.publish(i, result[T]{err: err})
			}
		}
	}()
	body()
	returned = true
}

type result[T any] struct {
	value T
	err   error
}

// GetMany retrieves many keys at once, with the same semantics as calling Get
// for each key:
//
//   - The backend is read in one call (if it implements BatchCache) and every
//     value is classified by staleness: fresh values are returned, stale values
//     are returned and refreshed in the background (WithServeStale), rotten
//     values and misses are fetched.
//   - Misses are checked against the not-found cache first, exactly like Get.
//   - Keys that need the upstream are claimed one by one in the same
//     singleflight Get uses: a key already being fetched by a Get or another
//     GetMany is waited for, not fetched again. If the upstream implements
//     BatchUpstream, the keys this call claims are double-checked
//     (WithDoubleCheck) and fetched together with one upstream.GetMany under
//     one fetch timeout. Otherwise each key is fetched exactly as Get would
//     (claimed only when its turn comes), at most WithGetManyFetchConcurrency at a
//     time, and a canceled GetMany starts no more keys.
//   - Fetched values are written to the backend and not-found keys to the
//     not-found cache, without touching the upstream, exactly like Get: in
//     one SetMany/DelMany per batch with a BatchUpstream (if the backend is a
//     BatchCache), key by key otherwise.
//
// The returned map holds only the keys that exist; keys that do not exist are
// simply absent. If some keys failed (backend, upstream, or ctx errors), the
// map still holds every key that succeeded and the error is a *BatchError
// listing the failed keys.
//
// As with Get, an upstream must not call back into the same Client for a key
// it is being asked for: that key is claimed by the caller and waits for itself.
//
// Client implements BatchUpstream through GetMany, so in a layered setup
// (memory -> Redis/DB -> source) a batch reaches the bottom upstream as one call.
func (c *Client[T]) GetMany(ctx context.Context, keys []string) (map[string]T, error) {
	keys = uniqueKeys(keys)
	out := make(map[string]T, len(keys))
	errs := map[string]error{}

	var fetchKeys, refreshKeys []string
	var fetchSeen, refreshSeen []uint64
	for i, r := range c.lookupMany(ctx, keys, false) {
		key := keys[i]
		switch {
		case r.fetch:
			fetchKeys = append(fetchKeys, key)
			fetchSeen = append(fetchSeen, r.seen)
		case r.err == nil:
			out[key] = r.value
		case !IsErrKeyNotFound(r.err):
			errs[key] = r.err
		}
		if r.refresh {
			refreshKeys = append(refreshKeys, key)
			refreshSeen = append(refreshSeen, r.seen)
		}
	}

	if len(refreshKeys) > 0 {
		c.asyncRefreshMany(context.WithoutCancel(ctx), refreshKeys, refreshSeen)
	}

	if len(fetchKeys) > 0 {
		sfKeys := make([]string, len(fetchKeys))
		for i, key := range fetchKeys {
			sfKeys[i] = c.makeSFKey(key)
		}
		for i, r := range c.fetchMany(ctx, fetchKeys, sfKeys, fetchSeen) {
			key := fetchKeys[i]
			switch {
			case r.err == nil:
				out[key] = r.value
			case !IsErrKeyNotFound(r.err):
				errs[key] = r.err
			}
		}
	}

	if len(errs) > 0 {
		return out, &BatchError{Errors: errs}
	}
	return out, nil
}

// lookup is what the caches say about one key.
type lookup[T any] struct {
	value   T
	err     error  // final error (ErrKeyNotFound for a cached not-found), unless fetch
	fetch   bool   // must be fetched from upstream
	refresh bool   // served stale, refresh in the background
	seen    uint64 // the key's stripe epoch before reading, see shouldDoubleCheck
}

// lookupMany is the batch form of the cache part of get: it reads the backend
// and the not-found cache and decides, per key, what get would do.
func (c *Client[T]) lookupMany(ctx context.Context, keys []string, doubleCheck bool) []lookup[T] {
	res := make([]lookup[T], len(keys))

	checkDataStale := c.checkDataStale
	if checkDataStale == nil {
		checkDataStale = alwaysFresh[T]
	}

	for i, key := range keys {
		res[i].seen = c.stripe(key).Epoch()
	}
	values, err := getMany(ctx, c.backend, keys)
	var missing []int
	for i, key := range keys {
		if kerr := errForKey(err, key); kerr != nil && !IsErrKeyNotFound(kerr) {
			res[i].err = fmt.Errorf("get from backend failed for key: %s: %w", key, kerr)
			continue
		}
		value, ok := values[key]
		if !ok {
			missing = append(missing, i)
			continue
		}
		switch checkDataStale(value) {
		case StateFresh:
			res[i].value = value
		case StateStale:
			if c.serveStale && !doubleCheck {
				res[i].value = value
				res[i].refresh = true
			} else {
				res[i].fetch = true
			}
		default:
			res[i].fetch = true
		}
	}

	if len(missing) == 0 {
		return res
	}
	if c.notFoundCache == nil {
		for _, i := range missing {
			res[i].fetch = true
		}
		return res
	}

	checkNotFoundStale := c.checkNotFoundStale
	if checkNotFoundStale == nil {
		checkNotFoundStale = alwaysFresh[time.Time]
	}

	missingKeys := make([]string, len(missing))
	for j, i := range missing {
		missingKeys[j] = keys[i]
	}
	cachedAts, err := getMany(ctx, c.notFoundCache, missingKeys)
	for _, i := range missing {
		key := keys[i]
		if kerr := errForKey(err, key); kerr != nil && !IsErrKeyNotFound(kerr) {
			res[i].err = fmt.Errorf("get from notFoundCache failed for key: %s: %w", key, kerr)
			continue
		}
		cachedAt, ok := cachedAts[key]
		if !ok {
			res[i].fetch = true
			continue
		}
		switch state := checkNotFoundStale(cachedAt); state {
		case StateFresh:
			res[i].err = fmt.Errorf("key not found in cache for key: %s: %w", key, &ErrKeyNotFound{Cached: true, CacheState: state})
		case StateStale:
			if c.serveStale && !doubleCheck {
				res[i].err = fmt.Errorf("key not found in cache for key: %s: %w", key, &ErrKeyNotFound{Cached: true, CacheState: state})
				res[i].refresh = true
			} else {
				res[i].fetch = true
			}
		default:
			res[i].fetch = true
		}
	}
	return res
}

// fetchMany fetches keys through the flight group. For a BatchUpstream it
// claims every key, fetches the claimed ones as one batch, and waits for all
// of them (including those claimed by others); otherwise see fetchEach.
func (c *Client[T]) fetchMany(ctx context.Context, keys, sfKeys []string, seen []uint64) []result[T] {
	if _, ok := c.upstream.(BatchUpstream[T]); !ok {
		return c.fetchEach(ctx, keys, sfKeys, seen)
	}
	flights := make([]*flight.Flight[T], len(keys))
	var ownKeys, ownSFKeys []string
	var ownFlights []*flight.Flight[T]
	var ownSeen []uint64
	for i, sfKey := range sfKeys {
		f, leader := c.flights.Claim(sfKey)
		flights[i] = f
		if leader {
			ownKeys = append(ownKeys, keys[i])
			ownSFKeys = append(ownSFKeys, sfKey)
			ownFlights = append(ownFlights, f)
			ownSeen = append(ownSeen, seen[i])
		}
	}
	if len(ownKeys) > 0 {
		go c.fetchClaimedMany(flightCtx(ctx), ownKeys, ownSFKeys, ownFlights, ownSeen)
	}

	results := make([]result[T], len(keys))
	for i, f := range flights {
		select {
		case <-f.Done():
		case <-ctx.Done():
		}
		select {
		case <-f.Done():
			value, err := f.Result()
			results[i] = result[T]{value: value, err: err}
		default:
			results[i].err = fmt.Errorf("context cancelled during fetch for key: %s: %w", keys[i], ctx.Err())
		}
	}
	return results
}

// fetchEach fetches keys from an upstream without batch support, each exactly
// as Get would, at most getManyConc at a time. A key is claimed only when its
// turn comes, so a Get of a key still queued here does not wait for the queue,
// and once ctx is done no more keys are started.
func (c *Client[T]) fetchEach(ctx context.Context, keys, sfKeys []string, seen []uint64) []result[T] {
	results := make([]result[T], len(keys))
	sem := make(chan struct{}, c.getManyConc)
	var wg sync.WaitGroup
	for i, key := range keys {
		if ctx.Err() == nil {
			select {
			case sem <- struct{}{}:
				wg.Go(func() {
					defer func() { <-sem }()
					value, err := c.fetchFromUpstreamWithSFKey(ctx, key, sfKeys[i], seen[i])
					results[i] = result[T]{value: value, err: err}
				})
				continue
			case <-ctx.Done():
			}
		}
		results[i].err = fmt.Errorf("context cancelled during fetch for key: %s: %w", key, ctx.Err())
	}
	wg.Wait()
	return results
}

// fetchClaimedMany is the batch form of fetchClaimed for a BatchUpstream:
// double-check, then fetch what is still missing; every flight is answered as
// soon as its result is known.
func (c *Client[T]) fetchClaimedMany(ctx context.Context, keys, sfKeys []string, flights []*flight.Flight[T], seen []uint64) {
	p := c.claimedFlights(keys, sfKeys, flights)
	all := make([]int, len(keys))
	for i := range all {
		all[i] = i
	}
	p.run(ctx, all, func() {
		var pending, check []int
		for i, key := range keys {
			if c.shouldDoubleCheck(key, seen[i]) {
				check = append(check, i)
			} else {
				pending = append(pending, i)
			}
		}
		if len(check) > 0 {
			checkKeys := make([]string, len(check))
			for j, i := range check {
				checkKeys[j] = keys[i]
			}
			checkCtx, cancel := context.WithTimeout(ctx, c.fetchTimeout)
			lookups := c.lookupMany(checkCtx, checkKeys, true)
			cancel()
			for j, r := range lookups {
				i := check[j]
				switch {
				case r.fetch:
					pending = append(pending, i)
				case r.err == nil:
					p.publish(i, result[T]{value: r.value})
				case isCachedFreshNotFound(r.err):
					p.publish(i, result[T]{err: r.err})
				default:
					pending = append(pending, i) // like Get: a failed double-check falls through to upstream
				}
			}
		}
		if len(pending) == 0 {
			return
		}

		// one call per chunk, getManyConc at a time; each answers its keys
		var wg sync.WaitGroup
		sem := make(chan struct{}, c.getManyConc)
		for chunk := range slices.Chunk(pending, cmp.Or(c.getManyChunk, len(pending))) {
			sem <- struct{}{}
			wg.Go(func() {
				defer func() { <-sem }()
				p.run(ctx, chunk, func() { c.fetchChunk(ctx, p, chunk) })
			})
		}
		wg.Wait()
	})
}

// fetchChunk fetches the claimed flights idxs with one upstream.GetMany.
func (c *Client[T]) fetchChunk(ctx context.Context, p *claimed[T], idxs []int) {
	keys := make([]string, len(idxs))
	for j, i := range idxs {
		keys[j] = p.keys[i]
	}
	// like Get, the fetch timeout starts after the double-check. A Client below
	// bounds each of its own fetches, so its call is not bounded as one fetch:
	// keys still queued there would fail with this deadline, not their own.
	timeout := c.fetchTimeout
	if lower, layered := c.upstream.(*Client[T]); layered {
		timeout += lower.batchTimeout(len(keys))
	}
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for j, r := range c.doFetchMany(fetchCtx, keys) {
		p.publish(idxs[j], r)
	}
}

// batchTimeout is how long a GetMany of n keys sent to this Client as an
// upstream may legitimately take: a fetch timeout for its own reads and
// write-back, plus its fetches: rounds of getManyConc calls at a time, each a
// fetch timeout (a lower Client's own budget per call), or per-key fetches.
func (c *Client[T]) batchTimeout(n int) time.Duration {
	ceil := func(a, b int) int { return (a + b - 1) / b }
	if _, ok := c.upstream.(BatchUpstream[T]); !ok {
		return c.fetchTimeout * time.Duration(1+ceil(n, c.getManyConc))
	}
	size := max(1, min(n, cmp.Or(c.getManyChunk, n)))
	rounds := time.Duration(ceil(ceil(n, size), c.getManyConc))
	if lower, layered := c.upstream.(*Client[T]); layered {
		return c.fetchTimeout + rounds*lower.batchTimeout(size)
	}
	return c.fetchTimeout + rounds*c.fetchTimeout
}

// doFetchMany is the batch form of doFetch: fetch from the batch upstream,
// then write the results to this layer only.
func (c *Client[T]) doFetchMany(ctx context.Context, keys []string) []result[T] {
	results := make([]result[T], len(keys))
	gens := make([]uint64, len(keys))
	for i, key := range keys {
		gens[i] = c.stripe(key).Generation()
	}

	values, err := c.upstream.(BatchUpstream[T]).GetMany(ctx, keys)
	for i, key := range keys {
		if kerr := errForKey(err, key); kerr != nil {
			results[i].err = fmt.Errorf("get from upstream failed for key: %s: %w", key, kerr)
		} else if value, ok := values[key]; ok {
			results[i].value = value
		} else {
			results[i].err = fmt.Errorf("get from upstream failed for key: %s: %w", key, &ErrKeyNotFound{})
		}
	}

	// The batch form of backfill: hold each key's stripe for reading (TryRLock
	// never blocks, so no lock order is needed) and skip the keys whose stripe
	// has a write in progress or whose gen moved since it was taken.
	held := map[*stripe.Stripe]bool{}
	defer func() {
		for s, ok := range held {
			if ok {
				s.RUnlock()
			}
		}
	}()
	found := map[string]T{}
	var notFound []string
	filled := map[*stripe.Stripe]bool{}
	for i, key := range keys {
		s := c.stripe(key)
		ok, seen := held[s]
		if !seen {
			ok = s.TryRLock()
			held[s] = ok
		}
		if !ok || s.Generation() != gens[i] {
			continue
		}
		switch {
		case results[i].err == nil:
			found[key] = results[i].value
			filled[s] = true
		case IsErrKeyNotFound(results[i].err):
			notFound = append(notFound, key)
			filled[s] = true
		}
	}
	if err := c.setManyWithoutUpstream(ctx, found); err != nil {
		c.logger.WarnContext(ctx, "failed to set cache entries", "error", err)
	}
	if err := c.delManyWithoutUpstream(ctx, notFound); err != nil {
		c.logger.WarnContext(ctx, "failed to delete cache entries", "error", err)
	}
	for s := range filled { // see stripe.Stripe.Epoch
		s.AddFill()
	}

	return results
}

// setManyWithoutUpstream is the batch form of setWithoutUpstream. A failed
// not-found cleanup does not stop the backend write: reads check the backend
// first, so caching the value is right either way.
func (c *Client[T]) setManyWithoutUpstream(ctx context.Context, values map[string]T) error {
	if len(values) == 0 {
		return nil
	}
	var errs []error
	if c.notFoundCache != nil {
		keys := make([]string, 0, len(values))
		for key := range values {
			keys = append(keys, key)
		}
		if err := delMany(ctx, c.notFoundCache, keys); err != nil {
			errs = append(errs, fmt.Errorf("delete from notFoundCache failed: %w", err))
		}
	}
	if err := setMany(ctx, c.backend, values); err != nil {
		errs = append(errs, fmt.Errorf("set in backend failed: %w", err))
	}
	return errors.Join(errs...)
}

// delManyWithoutUpstream is the batch form of delWithoutUpstream.
func (c *Client[T]) delManyWithoutUpstream(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	var errs []error
	if c.notFoundCache != nil {
		now := NowFunc()
		cachedAts := make(map[string]time.Time, len(keys))
		for _, key := range keys {
			cachedAts[key] = now
		}
		if err := setMany(ctx, c.notFoundCache, cachedAts); err != nil {
			errs = append(errs, fmt.Errorf("failed to set notFoundCache: %w", err))
		}
	}
	// the key is gone upstream, so its old value goes even if no not-found was recorded
	if err := delMany(ctx, c.backend, keys); err != nil {
		errs = append(errs, fmt.Errorf("delete from backend failed: %w", err))
	}
	return errors.Join(errs...)
}

// asyncRefreshMany is the batch form of asyncRefresh: the stale keys of one
// GetMany are refreshed together in the background.
func (c *Client[T]) asyncRefreshMany(ctx context.Context, keys []string, seen []uint64) {
	var refreshKeys, sfKeys []string
	var refreshSeen []uint64
	for i, key := range keys {
		sfKey := c.makeSFKey(key)
		if _, loaded := c.asyncRefreshing.LoadOrStore(sfKey, struct{}{}); !loaded {
			refreshKeys = append(refreshKeys, key)
			sfKeys = append(sfKeys, sfKey)
			refreshSeen = append(refreshSeen, seen[i])
		}
	}
	if len(refreshKeys) == 0 {
		return
	}

	go func() {
		defer func() {
			for _, sfKey := range sfKeys {
				c.asyncRefreshing.Delete(sfKey)
			}
		}()
		// every fetch answers its waiters (bounded by its fetch timeout, and with
		// an error if it panics or exits), so this wait needs no bound of its own
		for i, r := range c.fetchMany(ctx, refreshKeys, sfKeys, refreshSeen) {
			if r.err != nil && !IsErrKeyNotFound(r.err) {
				c.logger.ErrorContext(ctx, "async refresh failed", "key", refreshKeys[i], "error", r.err)
			}
		}
	}()
}

// getMany reads many keys from a cache or upstream, in one call when it
// implements BatchUpstream, otherwise key by key.
func getMany[T any](ctx context.Context, from Upstream[T], keys []string) (map[string]T, error) {
	if batch, ok := from.(BatchUpstream[T]); ok {
		return batch.GetMany(ctx, keys)
	}
	out := make(map[string]T, len(keys))
	errs := map[string]error{}
	for _, key := range keys {
		value, err := from.Get(ctx, key)
		switch {
		case err == nil:
			out[key] = value
		case !IsErrKeyNotFound(err):
			errs[key] = err
		}
	}
	if len(errs) > 0 {
		return out, &BatchError{Errors: errs}
	}
	return out, nil
}

// DefaultChunkSize is how many keys one pipeline or statement of the built-in
// batch backends carries unless configured otherwise.
const DefaultChunkSize = 1000

func chunkSizeOr(size, def int) int {
	if size <= 0 {
		return def
	}
	return size
}

// setMany writes many keys, in one call when cache implements BatchCache,
// otherwise key by key; like Get, one key failing does not stop the others.
func setMany[T any](ctx context.Context, cache Cache[T], values map[string]T) error {
	if batch, ok := cache.(BatchCache[T]); ok {
		return batch.SetMany(ctx, values)
	}
	errs := map[string]error{}
	for key, value := range values {
		if err := cache.Set(ctx, key, value); err != nil {
			errs[key] = err
		}
	}
	return batchError(errs)
}

// delMany is the delete counterpart of setMany.
func delMany[T any](ctx context.Context, cache Cache[T], keys []string) error {
	if batch, ok := cache.(BatchCache[T]); ok {
		return batch.DelMany(ctx, keys)
	}
	errs := map[string]error{}
	for _, key := range keys {
		if err := cache.Del(ctx, key); err != nil {
			errs[key] = err
		}
	}
	return batchError(errs)
}

// errForKey returns the error a batch call reported for key: the key's own
// entry if err is a *BatchError, otherwise err itself (the whole batch failed)
// marked as a wholeBatchError. Only an unwrapped *BatchError is partial: a
// wrapped or joined one may sit next to an error that failed the whole batch.
func errForKey(err error, key string) error {
	if batchErr, ok := err.(*BatchError); ok { //nolint:errorlint // see above
		return batchErr.Errors[key]
	}
	if err == nil {
		return nil
	}
	return &wholeBatchError{err: err}
}

// wholeBatchError is a whole-batch failure reported for one key. It is never
// a not-found (see IsErrKeyNotFound), even if one sits in its chain, while
// errors.Is and errors.As still see the original error.
type wholeBatchError struct{ err error }

func (e *wholeBatchError) Error() string { return e.err.Error() }
func (e *wholeBatchError) Unwrap() error { return e.err }

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
