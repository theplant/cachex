package cachex

import (
	"context"
	"fmt"
	"slices"

	"github.com/theplant/cachex/v2/internal/stripe"
)

// Set writes value for key into every layer, bottom up, as if the source had
// just answered it. It does not write the source: change the source first,
// then call Set (or Del), so that a read in between cannot cache the old value
// after the write.
//
// Until Set returns, a concurrent read may still see the old value; once it
// returns, no read gets a value fetched before it, and a fetch that started
// before it does not backfill over it. Writes of one key through one Cache
// apply one at a time, in the same order on every layer. If a layer fails,
// the key is dropped from that layer and every layer above it (what the
// layers below hold was written), and the error is returned.
//
// Set waits while another write of a key in the same stripe is in progress;
// if ctx ends first it returns ctx's error, writes nothing, and drops the key
// from every layer once the stripe is free, since the caller may have changed
// the source already.
func (c *Cache[T]) Set(ctx context.Context, key string, value T) error {
	return c.writeOne(ctx, key, &value)
}

// Del drops key from every layer, bottom up, so the next read asks the
// source. Like Set, call it after changing the source; it records nothing
// about whether the key exists. Failures and waiting are as for Set.
func (c *Cache[T]) Del(ctx context.Context, key string) error {
	return c.writeOne(ctx, key, nil)
}

// SetMany is Set for many keys, with one call per layer. A key that fails in a
// layer is dropped from that layer and those above and listed in the returned
// *BatchError; the other keys are written. An error that is not a *BatchError
// means nothing was written (ctx ended while waiting).
func (c *Cache[T]) SetMany(ctx context.Context, values map[string]T) error {
	return c.write(ctx, values, nil)
}

// DelMany is Del for many keys, with one call per layer; failures are reported
// as for SetMany.
func (c *Cache[T]) DelMany(ctx context.Context, keys []string) error {
	return c.write(ctx, nil, uniqueKeys(keys))
}

// writeOne is write for one key (deleting it if value is nil), without the
// bookkeeping of many.
func (c *Cache[T]) writeOne(ctx context.Context, key string, value *T) error {
	s := c.stripes.For(key)
	done := func() {
		s.AddWrite()
		c.dropFlights(key)
	}
	if err := s.Lock(ctx, func() { done(); c.invalidate(ctx, []string{key}, len(c.layers)-1) }); err != nil {
		return fmt.Errorf("cachex: context done while waiting to write: %w", err)
	}
	defer s.Unlock()
	defer done()

	var a Entry[T]
	if value != nil {
		a = c.sourceFill(key, 0, *value, false).entry
	}
	now := c.opts.now()
	for j := len(c.layers) - 1; j >= 0; j-- {
		l := &c.layers[j]
		var e Entry[T]
		keep := false
		if value != nil {
			e, keep = l.entry(a.Value, false, a.CachedAt, a.FreshUntil, a.ExpiresAt)
			keep = keep && now.Before(e.ExpiresAt)
		}
		var err error
		if keep {
			if err = l.backend.Set(ctx, key, e); err != nil {
				err = fmt.Errorf("cachex: set %q in layer %d: %w", key, j, err)
			}
		} else if err = l.backend.Del(ctx, key); err != nil {
			err = fmt.Errorf("cachex: delete %q in layer %d: %w", key, j, err)
		}
		if err != nil {
			c.invalidate(ctx, []string{key}, j)
			return err
		}
	}
	return nil
}

// write sets values and deletes dels in every layer, bottom up, holding the
// stripes of all their keys.
func (c *Cache[T]) write(ctx context.Context, values map[string]T, dels []string) error {
	byStripe := map[int][]string{}
	for key := range values {
		i := c.stripes.Index(key)
		byStripe[i] = append(byStripe[i], key)
	}
	for _, key := range dels {
		i := c.stripes.Index(key)
		byStripe[i] = append(byStripe[i], key)
	}
	order := make([]int, 0, len(byStripe))
	for i := range byStripe {
		order = append(order, i)
	}
	slices.Sort(order) // every write locks in increasing order: no deadlock

	// done marks a stripe written, even partly: a backfill that took its gen
	// before is skipped, and a read that starts from now on fetches anew
	// instead of joining a fetch that may have read the layers before.
	done := func(s *stripe.Stripe, keys []string) {
		s.AddWrite()
		for _, key := range keys {
			c.dropFlights(key)
		}
	}
	// abandoned is what a write that gives up does to a stripe once it holds
	// it: like any failed write, it drops the keys from every layer.
	abandoned := func(s *stripe.Stripe, keys []string) func() {
		return func() {
			done(s, keys)
			c.invalidate(ctx, keys, len(c.layers)-1)
		}
	}
	for n, i := range order {
		s := c.stripes.At(i)
		if err := s.Lock(ctx, abandoned(s, byStripe[i])); err != nil {
			for _, j := range order[:n] {
				h := c.stripes.At(j)
				abandoned(h, byStripe[j])()
				h.Unlock()
			}
			for _, j := range order[n+1:] {
				r := c.stripes.At(j)
				if r.Lock(ctx, abandoned(r, byStripe[j])) == nil {
					abandoned(r, byStripe[j])()
					r.Unlock()
				}
			}
			return fmt.Errorf("cachex: context done while waiting to write: %w", err)
		}
	}
	defer func() {
		for _, i := range order {
			s := c.stripes.At(i)
			done(s, byStripe[i])
			s.Unlock()
		}
	}()

	answers := make(map[string]Entry[T], len(values)) // as if the source answered now
	for key, value := range values {
		answers[key] = c.sourceFill(key, 0, value, false).entry
	}
	errs := map[string]error{}
	failedAt := map[string]int{}
	now := c.opts.now()
	for j := len(c.layers) - 1; j >= 0; j-- {
		l := &c.layers[j]
		sets := map[string]Entry[T]{}
		var drop []string
		for key, a := range answers {
			if _, failed := errs[key]; failed {
				continue
			}
			if e, keep := l.entry(a.Value, false, a.CachedAt, a.FreshUntil, a.ExpiresAt); keep && now.Before(e.ExpiresAt) {
				sets[key] = e
			} else {
				drop = append(drop, key)
			}
		}
		for _, key := range dels {
			if _, failed := errs[key]; !failed {
				drop = append(drop, key)
			}
		}
		if len(sets) > 0 {
			err := l.backend.SetMany(ctx, sets)
			for key := range sets {
				if kerr := errForKey(err, key); kerr != nil {
					errs[key] = fmt.Errorf("cachex: set %q in layer %d: %w", key, j, kerr)
					failedAt[key] = j
				}
			}
		}
		if len(drop) > 0 {
			err := l.backend.DelMany(ctx, drop)
			for _, key := range drop {
				if kerr := errForKey(err, key); kerr != nil {
					errs[key] = fmt.Errorf("cachex: delete %q in layer %d: %w", key, j, kerr)
					failedAt[key] = j
				}
			}
		}
	}
	// a failed layer's state is unknown (the write may have landed and only
	// the reply been lost): drop the key there and above
	byLayer := map[int][]string{}
	for key, j := range failedAt {
		byLayer[j] = append(byLayer[j], key)
	}
	for j, keys := range byLayer {
		c.invalidate(ctx, keys, j)
	}
	return batchError(errs)
}

// invalidate drops keys from layers top..0, best effort. It runs after a write
// failed, often because of ctx itself, so it does not use ctx's cancellation,
// nor its transaction (see IsShared).
func (c *Cache[T]) invalidate(ctx context.Context, keys []string, top int) {
	ictx, cancel := context.WithTimeout(sharedCtx(ctx), c.opts.fetchTimeout)
	defer cancel()
	for j := top; j >= 0; j-- {
		if err := c.layers[j].backend.DelMany(ictx, keys); err != nil {
			c.opts.logger.WarnContext(ictx, "cachex: invalidation failed", "layer", j, "keys", keys, "error", err)
		}
	}
}
