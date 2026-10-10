package cachex

// StripeIndex exposes a key's stripe to the external tests and benchmarks.
func StripeIndex[T any](c *Cache[T], key string) int { return c.stripes.Index(key) }
