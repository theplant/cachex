package cachex

// StripeIndex exposes a key's stripe to the external tests and benchmarks.
func StripeIndex[T any](c *Cache[T], key string) int { return c.stripes.Index(key) }

// Settle waits for the background work c has started (fetches finishing their
// backfills, refreshes), for tests that look at a backend right after a read.
func Settle[T any](c *Cache[T]) { c.wg.Wait() }
