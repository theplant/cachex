package cachex

import (
	"context"
	"fmt"
	"maps"
	"slices"
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
	if err := c.lockWrite(ctx, key); err != nil {
		return err
	}
	defer c.unlockWrite(key)

	var a Entry[T]
	if value != nil {
		a = c.answer(*value, false)
	}
	now := c.opts.now()
	for j := len(c.layers) - 1; j >= 0; j-- {
		l := &c.layers[j]
		var err error
		if value != nil {
			err = l.put(ctx, key, a, now)
		} else {
			err = l.backend.Del(ctx, key)
		}
		if err != nil {
			c.invalidate(ctx, []string{key}, j)
			return fmt.Errorf("cachex: write %q to layer %d: %w", key, j, err)
		}
	}
	return nil
}

// write sets values and deletes dels in every layer, bottom up, holding the
// stripes of all their keys.
func (c *Cache[T]) write(ctx context.Context, values map[string]T, dels []string) error {
	keys := slices.AppendSeq(make([]string, 0, len(values)+len(dels)), maps.Keys(values))
	unlock, err := c.lockWrites(ctx, append(keys, dels...))
	if err != nil {
		return err
	}
	defer unlock()

	answers := make(map[string]Entry[T], len(values))
	for key, value := range values {
		answers[key] = c.answer(value, false)
	}
	errs := map[string]error{}
	failedAt := map[string]int{}
	now := c.opts.now()
	for j := len(c.layers) - 1; j >= 0; j-- {
		var drop []string
		for _, key := range dels {
			if _, failed := errs[key]; !failed {
				drop = append(drop, key)
			}
		}
		for key, kerr := range c.layers[j].putMany(ctx, answers, drop, errs, now) {
			errs[key] = fmt.Errorf("cachex: write %q to layer %d: %w", key, j, kerr)
			failedAt[key] = j
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
