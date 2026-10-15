package cachex

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/theplant/cachex/v2/internal/stripe"
)

// The write-order guard: every use of the stripes is here. A write holds its
// keys' stripes for writing and, when it ends, drops the keys' flights and
// counts itself; a backfill holds its key's stripe for reading and lands only
// if no write counted since it took the generation. So a backfill either
// lands before a write or not at all, and a read never waits on a write.
// Backfills of a stripe also take its fill lock, so that they land one at a
// time and each sees what the one before wrote (see Cache.backfill). See
// docs/design/write-order.md, ADR 0003 and ADR 0022.

// epoch is key's stripe epoch: take it before reading the first layer, and a
// double check is due only if it changed (see shouldDoubleCheck). It is not a
// method of the generic Cache, which costs the hit path a call.
func epoch(s *stripe.Set, key string) uint64 { return s.For(key).Epoch() }

// generation is key's write generation: take it before reading below the first
// layer, and pass it to beginFill.
func (c *Cache[T]) generation(key string) uint64 { return c.stripes.For(key).Generation() }

// lockWrite takes key's stripe for a write; end the write with unlockWrite. A
// free stripe is taken even with a done ctx, without allocating. If ctx ends
// while waiting, lockWrite returns an error, and once the stripe is free the
// key is dropped from every layer, as after any failed write: the caller may
// have changed the source already. Close waits for that.
func (c *Cache[T]) lockWrite(ctx context.Context, key string) error {
	s := c.stripes.For(key)
	if s.TryLock() { // the closures below cost allocations, so only when waiting
		return nil
	}
	return c.lockStripe(ctx, s, c.lateWrite(ctx, s, []string{key}, false), c.latePublish(ctx, []string{key}, false))
}

// unlockWrite ends a write of key.
func (c *Cache[T]) unlockWrite(key string) { c.endWrite(c.stripes.For(key), key) }

// endWrite counts a write of keys on their stripe s and releases s.
func (c *Cache[T]) endWrite(s *stripe.Stripe, keys ...string) {
	c.countWrite(s, keys...)
	s.Unlock()
}

// countWrite counts a write of keys on their stripe s, even a partial one: a
// backfill that took its generation before is skipped, and a read from now on
// fetches anew instead of joining a fetch that may have read the layers before
// the write.
//
// The flights go first: a fetch that registers its call to the source in
// between took its generation before the count, so its backfill is skipped
// (and it asked the source after the caller changed it). The other way
// round, a fetch registering after the count but before the drop would have
// its call dropped while its backfill still lands, and a read after the
// write would ask the source again at the same time (see ADR 0022).
func (c *Cache[T]) countWrite(s *stripe.Stripe, keys ...string) {
	for _, key := range keys {
		c.dropFlights(key)
	}
	s.AddWrite()
}

// lockWrites is lockWrite for many keys; unlock ends the write of them all. It
// takes the stripes in increasing order, so writes never deadlock. If ctx ends
// while waiting, the keys of the stripes it holds or can take at once are
// dropped (see drop) in one call per layer, and those of the others once each
// is free. local is for a broadcast's invalidation: it drops from the local
// layers only.
func (c *Cache[T]) lockWrites(ctx context.Context, keys []string, local bool) (unlock func(), err error) {
	byStripe := map[int][]string{}
	for _, key := range keys {
		i := c.stripes.Index(key)
		byStripe[i] = append(byStripe[i], key)
	}
	order := make([]int, 0, len(byStripe))
	for i := range byStripe {
		order = append(order, i)
	}
	slices.Sort(order)
	end := func(order []int) {
		for _, i := range order {
			c.endWrite(c.stripes.At(i), byStripe[i]...)
		}
	}

	for n, i := range order {
		s := c.stripes.At(i)
		if s.TryLock() {
			continue
		}
		if err := c.lockStripe(ctx, s, c.lateWrite(ctx, s, byStripe[i], local), c.latePublish(ctx, byStripe[i], local)); err != nil {
			held := order[:n:n]
			for _, j := range order[n+1:] {
				r := c.stripes.At(j)
				// ctx is done: this does not wait
				if r.TryLock() || c.lockStripe(ctx, r, c.lateWrite(ctx, r, byStripe[j], local), c.latePublish(ctx, byStripe[j], local)) == nil {
					held = append(held, j)
				}
			}
			var dropped []string
			for _, j := range held {
				dropped = append(dropped, byStripe[j]...)
			}
			func() {
				defer end(held) // even if a layer calls runtime.Goexit
				if len(dropped) > 0 {
					c.drop(ctx, dropped, local)
				}
			}()
			return nil, err
		}
	}
	return func() { end(order) }, nil
}

// lateWrite is what a write that gave up waiting for s does once it holds s:
// it drops keys and counts a write (see latePublish for what follows).
func (c *Cache[T]) lateWrite(ctx context.Context, s *stripe.Stripe, keys []string, local bool) func() {
	return func() {
		defer func() { // it runs in a goroutine of its own: a panic would end the process
			if r := recover(); r != nil {
				c.opts.logger.ErrorContext(ctx, "cachex: panic during a late drop", "keys", keys, "panic", r)
			}
		}()
		defer c.countWrite(s, keys...) // even if a layer calls runtime.Goexit
		c.drop(ctx, keys, local)
	}
}

// latePublish is what follows a write's late drop (not a broadcast's, local),
// once s is free: the drop is published again, since the message the write
// sent when it gave up may have reached an instance that then copied the old
// value from a shared layer before the drop. Not while holding s: a publish
// can wait up to a fetch timeout, and a clear (see clearLocal) waits for every
// stripe.
func (c *Cache[T]) latePublish(ctx context.Context, keys []string, local bool) func() {
	if local {
		return nil
	}
	return func() {
		defer func() { // it runs in a goroutine of its own: a panic would end the process
			if r := recover(); r != nil {
				c.opts.logger.ErrorContext(ctx, "cachex: panic while publishing a late drop", "keys", keys, "panic", r)
			}
		}()
		c.publish(ctx, keys)
	}
}

// drop drops keys after a write gave up waiting: from every layer (the caller
// may have changed the source already), or, for a broadcast's invalidation
// (local), from the local layers only, since the shared ones hold what the
// writer wrote.
func (c *Cache[T]) drop(ctx context.Context, keys []string, local bool) {
	if !local {
		c.invalidate(ctx, keys, len(c.layers)-1)
		return
	}
	dctx, cancel := context.WithTimeout(sharedCtx(ctx), c.opts.fetchTimeout)
	defer cancel()
	n := 0
	defer func() { // a layer that called runtime.Goexit: still drop from the others
		for n++; n < len(c.local); n++ {
			c.invalidateLayer(dctx, c.local[n], keys)
		}
	}()
	for ; n < len(c.local); n++ {
		c.invalidateLayer(dctx, c.local[n], keys)
	}
}

// lockStripe takes s for writing; if ctx ends first, it returns an error and
// late runs once s is free, holding it; s is released after it, and then
// after runs (if not nil). Close waits for both.
func (c *Cache[T]) lockStripe(ctx context.Context, s *stripe.Stripe, late, after func()) error {
	done, _ := c.track()
	err := s.Lock(ctx, late, func() {
		defer done()
		if after != nil {
			after()
		}
	})
	if err == nil {
		done() // late never runs
		return nil
	}
	return fmt.Errorf("cachex: context done while waiting to write: %w", err)
}

// beginFill takes key's stripe for a backfill, if no write holds it and none
// counted since gen, and then its fill lock; end the backfill with endFill. It
// reports false, holding nothing, if the backfill must be skipped.
func (c *Cache[T]) beginFill(key string, gen uint64) bool {
	s := c.stripes.For(key)
	if !s.TryRLock() {
		return false
	}
	if s.Generation() != gen {
		s.RUnlock()
		return false
	}
	s.LockFill()
	return true
}

// endFill ends a backfill of key.
func (c *Cache[T]) endFill(key string) {
	s := c.stripes.For(key)
	s.AddFill() // see stripe.Stripe.Epoch
	s.UnlockFill()
	s.RUnlock()
}

// beginFills is beginFill for many keys: it returns the indexes of the fills
// that may land, and end, which ends them all. It takes the fill locks in
// increasing stripe order, so batch backfills never deadlock.
func (c *Cache[T]) beginFills(fills []fill[T]) (ok []int, end func()) {
	held := map[int]bool{}
	filled := map[int]struct{}{}
	for i, f := range fills {
		si := c.stripes.Index(f.key)
		s := c.stripes.At(si)
		h, seen := held[si]
		if !seen {
			h = s.TryRLock()
			held[si] = h
		}
		if h && s.Generation() == f.gen {
			ok = append(ok, i)
			filled[si] = struct{}{}
		}
	}
	order := slices.Sorted(maps.Keys(filled))
	for _, si := range order {
		c.stripes.At(si).LockFill()
	}
	return ok, func() {
		for _, si := range order {
			s := c.stripes.At(si)
			s.AddFill()
			s.UnlockFill()
		}
		for si, h := range held {
			if h {
				c.stripes.At(si).RUnlock()
			}
		}
	}
}

// invalidateLocal drops keys, written on another instance, from the local
// layers as a write of them would: holding their stripes, and counting a
// write, so that a fetch of them that started before does not backfill its
// answer after. If the stripes stay busy past the fetch timeout, the keys are
// dropped from the local layers once each is free (see lockWrites).
func (c *Cache[T]) invalidateLocal(keys []string) {
	keys = slices.DeleteFunc(uniqueKeys(keys), func(key string) bool { return !supportedKey(key) })
	if len(keys) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.fetchTimeout)
	defer cancel()
	unlock, err := c.lockWrites(ctx, keys, true)
	if err != nil {
		c.opts.logger.WarnContext(ctx, "cachex: broadcast invalidation waited too long", "error", err)
		return
	}
	defer unlock()
	c.drop(ctx, keys, true)
}

// clearLocal clears the local layers, holding every stripe meanwhile and
// counting a write on each, so that no fetch that started before lands its
// backfill after; and drops every flight, so that reads from now on fetch
// anew. Writes wait for it; reads do not.
func (c *Cache[T]) clearLocal() {
	ctx := context.Background()
	for i := range stripe.Count {
		_ = c.stripes.At(i).Lock(ctx, func() {}, nil) // a background ctx never gives up
	}
	defer func() { // even if a layer panics or calls runtime.Goexit
		c.sourcing.Clear() // before the flights, as in dropFlights
		c.flights.DropAll()
		for i := range stripe.Count {
			s := c.stripes.At(i)
			s.AddWrite()
			s.Unlock()
		}
	}()
	cctx, cancel := context.WithTimeout(ctx, c.opts.fetchTimeout)
	defer cancel()
	n := 0
	defer func() { // a layer that called runtime.Goexit: still clear the others
		for n++; n < len(c.local); n++ {
			c.clearLayer(cctx, c.local[n])
		}
	}()
	for ; n < len(c.local); n++ {
		c.clearLayer(cctx, c.local[n])
	}
}

func (c *Cache[T]) clearLayer(ctx context.Context, j int) {
	defer func() {
		if r := recover(); r != nil {
			c.opts.logger.ErrorContext(ctx, "cachex: panic while clearing a local layer", "layer", j, "panic", r)
		}
	}()
	if err := c.layers[j].backend.(LocalBackend).Clear(ctx); err != nil {
		c.opts.logger.ErrorContext(ctx, "cachex: clearing a local layer failed", "layer", j, "error", err)
	}
}
