package cachex

import (
	"context"
	stderrors "errors"
	"runtime/debug"
	"sync"
	"time"

	"github.com/pkg/errors"
)

var _ BatchUpstream[any] = &Client[any]{}

// flight is one in-flight upstream fetch of a singleflight key.
type flight[T any] struct {
	done  chan struct{}
	value T
	err   error
}

// flightGroup is the singleflight shared by Get and GetMany. Unlike
// x/sync/singleflight, claim tells the caller synchronously whether it now
// owns the fetch, which lets GetMany claim many keys and fetch the ones it
// owns as one batch without waiting on (and deadlocking with) other batches.
type flightGroup[T any] struct {
	mu      sync.Mutex
	flights map[string]*flight[T]
}

// claim returns the flight for sfKey and whether the caller owns it. The owner
// must call finish exactly once; everyone else waits on flight.done.
func (g *flightGroup[T]) claim(sfKey string) (*flight[T], bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if f, ok := g.flights[sfKey]; ok {
		return f, false
	}
	if g.flights == nil {
		g.flights = map[string]*flight[T]{}
	}
	f := &flight[T]{done: make(chan struct{})}
	g.flights[sfKey] = f
	return f, true
}

// finish releases the claim before publishing the result, so a request that
// sees the result and comes back starts a new flight (where double-check
// finds the value this flight wrote) instead of joining a finished one.
func (g *flightGroup[T]) finish(sfKey string, f *flight[T], value T, err error) {
	g.forget(sfKey, f)
	f.value, f.err = value, err
	close(f.done)
}

// forget releases the claim without publishing a result.
func (g *flightGroup[T]) forget(sfKey string, f *flight[T]) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.flights[sfKey] == f {
		delete(g.flights, sfKey)
	}
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
//     GetMany is waited for, not fetched again. The keys this call claims are
//     double-checked (WithDoubleCheck) and then fetched together: one
//     upstream.GetMany if the upstream implements BatchUpstream, otherwise
//     concurrent upstream.Get calls (at most WithGetManyConcurrency at a time).
//   - Fetched values are written to the backend (SetMany if supported) and
//     not-found keys to the not-found cache, without touching the upstream,
//     exactly like Get.
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
	for i, r := range c.lookupMany(ctx, keys, false) {
		key := keys[i]
		switch {
		case r.fetch:
			fetchKeys = append(fetchKeys, key)
		case r.err == nil:
			out[key] = r.value
		case !IsErrKeyNotFound(r.err):
			errs[key] = r.err
		}
		if r.refresh {
			refreshKeys = append(refreshKeys, key)
		}
	}

	if len(refreshKeys) > 0 {
		c.asyncRefreshMany(context.WithoutCancel(ctx), refreshKeys)
	}

	if len(fetchKeys) > 0 {
		sfKeys := make([]string, len(fetchKeys))
		for i, key := range fetchKeys {
			sfKeys[i] = c.makeSFKey(key)
		}
		for i, r := range c.fetchMany(ctx, fetchKeys, sfKeys) {
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
	err     error // final error (ErrKeyNotFound for a cached not-found), unless fetch
	fetch   bool  // must be fetched from upstream
	refresh bool  // served stale, refresh in the background
}

// lookupMany is the batch form of the cache part of get: it reads the backend
// and the not-found cache and decides, per key, what get would do.
func (c *Client[T]) lookupMany(ctx context.Context, keys []string, doubleCheck bool) []lookup[T] {
	res := make([]lookup[T], len(keys))

	checkDataStale := c.checkDataStale
	if checkDataStale == nil {
		checkDataStale = alwaysFresh[T]
	}

	values, err := getMany(ctx, c.backend, keys)
	var missing []int
	for i, key := range keys {
		if kerr := errForKey(err, key); kerr != nil && !IsErrKeyNotFound(kerr) {
			res[i].err = errors.Wrapf(kerr, "get from backend failed for key: %s", key)
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
			res[i].err = errors.Wrapf(kerr, "get from notFoundCache failed for key: %s", key)
			continue
		}
		cachedAt, ok := cachedAts[key]
		if !ok {
			res[i].fetch = true
			continue
		}
		switch state := checkNotFoundStale(cachedAt); state {
		case StateFresh:
			res[i].err = errors.Wrapf(&ErrKeyNotFound{Cached: true, CacheState: state}, "key not found in cache for key: %s", key)
		case StateStale:
			if c.serveStale && !doubleCheck {
				res[i].err = errors.Wrapf(&ErrKeyNotFound{Cached: true, CacheState: state}, "key not found in cache for key: %s", key)
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

// fetchMany claims every key in the flight group, fetches the claimed ones as
// one batch, and waits for all of them (including those claimed by others).
func (c *Client[T]) fetchMany(ctx context.Context, keys, sfKeys []string) []result[T] {
	flights := make([]*flight[T], len(keys))
	var ownKeys, ownSFKeys []string
	var ownFlights []*flight[T]
	for i, sfKey := range sfKeys {
		f, leader := c.flights.claim(sfKey)
		flights[i] = f
		if leader {
			ownKeys = append(ownKeys, keys[i])
			ownSFKeys = append(ownSFKeys, sfKey)
			ownFlights = append(ownFlights, f)
		}
	}
	if len(ownKeys) > 0 {
		go c.fetchClaimedMany(ctx, ownKeys, ownSFKeys, ownFlights)
	}

	results := make([]result[T], len(keys))
	for i, f := range flights {
		select {
		case <-f.done:
		case <-ctx.Done():
		}
		select {
		case <-f.done:
			results[i] = result[T]{value: f.value, err: f.err}
		default:
			results[i].err = errors.Wrapf(ctx.Err(), "context cancelled during fetch for key: %s", keys[i])
		}
	}
	return results
}

// fetchClaimedMany is the batch form of fetchClaimed: double-check, then fetch
// what is still missing, then finish every flight.
func (c *Client[T]) fetchClaimedMany(ctx context.Context, keys, sfKeys []string, flights []*flight[T]) {
	results := make([]result[T], len(keys))
	var pending []int // keys still to fetch after the double-check; nil until it ran
	returned := false
	defer func() {
		r := recover()
		if r == nil && !returned {
			// runtime.Goexit in the upstream: release the keys without
			// publishing, like Get; waiters wait for their ctx.
			for i, f := range flights {
				c.flights.forget(sfKeys[i], f)
			}
			return
		}
		if r != nil {
			c.logger.ErrorContext(ctx, "panic during upstream fetch",
				"keys", keys,
				"panic", r,
				"stack", string(debug.Stack()))
			err := errors.Errorf("panic during upstream fetch: %v", r)
			failed := pending
			if failed == nil {
				failed = make([]int, len(keys))
				for i := range failed {
					failed[i] = i
				}
			}
			for _, i := range failed {
				results[i] = result[T]{err: err}
			}
		}
		for i, f := range flights {
			c.flights.finish(sfKeys[i], f, results[i].value, results[i].err)
		}
	}()

	checked := make([]int, 0, len(keys))
	if c.enableDoubleCheck {
		// like Get, the double-check reads with the request ctx
		for i, r := range c.lookupMany(ctx, keys, true) {
			switch {
			case r.fetch:
				checked = append(checked, i)
			case r.err == nil:
				results[i].value = r.value
			case isCachedFreshNotFound(r.err):
				results[i].err = r.err
			default:
				checked = append(checked, i) // like Get: a failed double-check falls through to upstream
			}
		}
	} else {
		for i := range keys {
			checked = append(checked, i)
		}
	}
	pending = checked
	if len(pending) == 0 {
		returned = true
		return
	}

	pendingKeys := make([]string, len(pending))
	for j, i := range pending {
		pendingKeys[j] = keys[i]
	}
	// like Get, the fetch timeout starts after the double-check
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.fetchTimeout)
	defer cancel()
	for j, r := range c.doFetchMany(fetchCtx, pendingKeys) {
		results[pending[j]] = r
	}
	returned = true
}

// doFetchMany is the batch form of doFetch: fetch from upstream, then write
// the results to this layer only.
func (c *Client[T]) doFetchMany(ctx context.Context, keys []string) []result[T] {
	results := make([]result[T], len(keys))

	if batch, ok := c.upstream.(BatchUpstream[T]); ok {
		values, err := batch.GetMany(ctx, keys)
		_, partial := err.(*BatchError) //nolint:errorlint // see errForKey
		for i, key := range keys {
			if kerr := errForKey(err, key); kerr != nil {
				if !partial && IsErrKeyNotFound(kerr) {
					// A whole-batch failure is not a not-found for every key, even if
					// a not-found sits somewhere in its chain: keep only its message.
					kerr = errors.New(kerr.Error())
				}
				results[i].err = errors.Wrapf(kerr, "get from upstream failed for key: %s", key)
			} else if value, ok := values[key]; ok {
				results[i].value = value
			} else {
				results[i].err = errors.Wrapf(&ErrKeyNotFound{}, "get from upstream failed for key: %s", key)
			}
		}
	} else {
		var wg sync.WaitGroup
		sem := make(chan struct{}, c.getManyConc)
		for i, key := range keys {
			// overwritten on return; stays if the upstream calls runtime.Goexit
			results[i].err = errors.Errorf("upstream fetch exited without returning for key: %s", key)
			sem <- struct{}{}
			wg.Go(func() {
				defer func() { <-sem }()
				results[i] = c.getFromUpstream(ctx, key)
			})
		}
		wg.Wait()
	}

	found := map[string]T{}
	var notFound []string
	for i, key := range keys {
		switch {
		case results[i].err == nil:
			found[key] = results[i].value
		case IsErrKeyNotFound(results[i].err):
			notFound = append(notFound, key)
		}
	}
	if err := c.setManyWithoutUpstream(ctx, found); err != nil {
		c.logger.WarnContext(ctx, "failed to set cache entries", "error", err)
	}
	if err := c.delManyWithoutUpstream(ctx, notFound); err != nil {
		c.logger.WarnContext(ctx, "failed to delete cache entries", "error", err)
	}

	return results
}

func (c *Client[T]) getFromUpstream(ctx context.Context, key string) (res result[T]) {
	defer func() {
		if r := recover(); r != nil {
			c.logger.ErrorContext(ctx, "panic during upstream fetch",
				"key", key,
				"panic", r,
				"stack", string(debug.Stack()))
			res = result[T]{err: errors.Errorf("panic during upstream fetch: %v", r)}
		}
	}()
	value, err := c.upstream.Get(ctx, key)
	if err != nil {
		return result[T]{err: errors.Wrapf(err, "get from upstream failed for key: %s", key)}
	}
	return result[T]{value: value}
}

// setManyWithoutUpstream is the batch form of setWithoutUpstream.
func (c *Client[T]) setManyWithoutUpstream(ctx context.Context, values map[string]T) error {
	if len(values) == 0 {
		return nil
	}
	if c.notFoundCache != nil {
		keys := make([]string, 0, len(values))
		for key := range values {
			keys = append(keys, key)
		}
		if err := delMany(ctx, c.notFoundCache, keys); err != nil {
			return errors.Wrap(err, "delete from notFoundCache failed")
		}
	}
	if err := setMany(ctx, c.backend, values); err != nil {
		return errors.Wrap(err, "set in backend failed")
	}
	return nil
}

// delManyWithoutUpstream is the batch form of delWithoutUpstream.
func (c *Client[T]) delManyWithoutUpstream(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	if c.notFoundCache != nil {
		now := NowFunc()
		cachedAts := make(map[string]time.Time, len(keys))
		for _, key := range keys {
			cachedAts[key] = now
		}
		if err := setMany(ctx, c.notFoundCache, cachedAts); err != nil {
			return errors.Wrap(err, "failed to set notFoundCache")
		}
	}
	if err := delMany(ctx, c.backend, keys); err != nil {
		return errors.Wrap(err, "delete from backend failed")
	}
	return nil
}

// asyncRefreshMany is the batch form of asyncRefresh: the stale keys of one
// GetMany are refreshed together in the background.
func (c *Client[T]) asyncRefreshMany(ctx context.Context, keys []string) {
	var refreshKeys, sfKeys []string
	for _, key := range keys {
		sfKey := c.makeSFKey(key)
		if _, loaded := c.asyncRefreshing.LoadOrStore(sfKey, struct{}{}); !loaded {
			refreshKeys = append(refreshKeys, key)
			sfKeys = append(sfKeys, sfKey)
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
		// Bound the wait: some keys may be fetched by other callers, and a fetch
		// that never finishes (runtime.Goexit) must not pin the whole batch.
		ctx, cancel := context.WithTimeout(ctx, c.fetchTimeout)
		defer cancel()
		for i, r := range c.fetchMany(ctx, refreshKeys, sfKeys) {
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

// setMany writes many keys, in one call when cache implements BatchCache,
// otherwise key by key; like Get, one key failing does not stop the others.
func setMany[T any](ctx context.Context, cache Cache[T], values map[string]T) error {
	if batch, ok := cache.(BatchCache[T]); ok {
		return batch.SetMany(ctx, values)
	}
	var errs []error
	for key, value := range values {
		errs = append(errs, cache.Set(ctx, key, value))
	}
	return stderrors.Join(errs...)
}

// delMany is the delete counterpart of setMany.
func delMany[T any](ctx context.Context, cache Cache[T], keys []string) error {
	if batch, ok := cache.(BatchCache[T]); ok {
		return batch.DelMany(ctx, keys)
	}
	var errs []error
	for _, key := range keys {
		errs = append(errs, cache.Del(ctx, key))
	}
	return stderrors.Join(errs...)
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
