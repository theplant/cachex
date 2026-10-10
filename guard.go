package cachex

import (
	"context"
	"fmt"
	"slices"

	"github.com/theplant/cachex/v2/internal/stripe"
)

// The write-order guard: every use of the stripes is here. A write holds its
// keys' stripes for writing and, when it ends, counts itself and drops the
// keys' flights; a backfill holds its key's stripe for reading and lands only
// if no write counted since it took the generation. So a backfill either
// lands before a write or not at all, and a read never waits on a write. See
// docs/design/write-order.md and ADR 0003.

// epoch is key's stripe epoch: take it before reading the first layer, and a
// double check is due only if it changed (see shouldDoubleCheck).
func (c *Cache[T]) epoch(key string) uint64 { return c.stripes.For(key).Epoch() }

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
	return c.lockStripe(ctx, s, c.lateWrite(ctx, s, []string{key}))
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
func (c *Cache[T]) countWrite(s *stripe.Stripe, keys ...string) {
	s.AddWrite()
	for _, key := range keys {
		c.dropFlights(key)
	}
}

// lockWrites is lockWrite for many keys; unlock ends the write of them all. It
// takes the stripes in increasing order, so writes never deadlock. If ctx ends
// while waiting, the keys of the stripes it holds or can take at once are
// dropped from every layer in one call per layer, and those of the others once
// each is free.
func (c *Cache[T]) lockWrites(ctx context.Context, keys []string) (unlock func(), err error) {
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
		if err := c.lockStripe(ctx, s, c.lateWrite(ctx, s, byStripe[i])); err != nil {
			held := order[:n:n]
			for _, j := range order[n+1:] {
				r := c.stripes.At(j)
				// ctx is done: this does not wait
				if r.TryLock() || c.lockStripe(ctx, r, c.lateWrite(ctx, r, byStripe[j])) == nil {
					held = append(held, j)
				}
			}
			var dropped []string
			for _, j := range held {
				dropped = append(dropped, byStripe[j]...)
			}
			if len(dropped) > 0 {
				c.invalidate(ctx, dropped, len(c.layers)-1)
			}
			end(held)
			return nil, err
		}
	}
	return func() { end(order) }, nil
}

// lateWrite is what a write that gave up waiting for s does once it holds s.
func (c *Cache[T]) lateWrite(ctx context.Context, s *stripe.Stripe, keys []string) func() {
	return func() {
		c.invalidate(ctx, keys, len(c.layers)-1)
		c.countWrite(s, keys...)
	}
}

// lockStripe takes s for writing; if ctx ends first, it returns an error and
// late runs once s is free, holding it; s is released after it. Close waits
// for late.
func (c *Cache[T]) lockStripe(ctx context.Context, s *stripe.Stripe, late func()) error {
	done, _ := c.track()
	err := s.Lock(ctx, func() {
		defer done()
		late()
	})
	if err == nil {
		done() // late never runs
		return nil
	}
	return fmt.Errorf("cachex: context done while waiting to write: %w", err)
}

// beginFill takes key's stripe for a backfill, if no write holds it and none
// counted since gen; end the backfill with endFill. It reports false, holding
// nothing, if the backfill must be skipped.
func (c *Cache[T]) beginFill(key string, gen uint64) bool {
	s := c.stripes.For(key)
	if !s.TryRLock() {
		return false
	}
	if s.Generation() != gen {
		s.RUnlock()
		return false
	}
	return true
}

// endFill ends a backfill of key that wrote the layers.
func (c *Cache[T]) endFill(key string) {
	s := c.stripes.For(key)
	s.AddFill() // see stripe.Stripe.Epoch
	s.RUnlock()
}

// beginFills is beginFill for many keys: it returns the indexes of the fills
// that may land, and end, which ends them all.
func (c *Cache[T]) beginFills(fills []fill[T]) (ok []int, end func()) {
	held := map[*stripe.Stripe]bool{}
	filled := map[*stripe.Stripe]struct{}{}
	for i, f := range fills {
		s := c.stripes.For(f.key)
		h, seen := held[s]
		if !seen {
			h = s.TryRLock()
			held[s] = h
		}
		if h && s.Generation() == f.gen {
			ok = append(ok, i)
			filled[s] = struct{}{}
		}
	}
	return ok, func() {
		for s := range filled {
			s.AddFill()
		}
		for s, h := range held {
			if h {
				s.RUnlock()
			}
		}
	}
}
