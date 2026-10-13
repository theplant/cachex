package cachex

// StripeIndex exposes a key's stripe to the external tests and benchmarks.
func StripeIndex[T any](c *Cache[T], key string) int { return c.stripes.Index(key) }

// Settle waits for the background work c has started (fetches finishing their
// backfills, refreshes), for tests that look at a backend right after a read.
func Settle[T any](c *Cache[T]) { c.wg.Wait() }

// SourceWaiters is how many fetches of key wait for another fetch's request
// to the source instead of asking it themselves.
func SourceWaiters[T any](c *Cache[T], key string) int {
	if v, ok := c.sourcing.Load(key); ok {
		return int(v.(*sourceCall[T]).waiters.Load())
	}
	return 0
}
