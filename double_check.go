package cachex

import (
	"context"
	"fmt"
	"time"

	"github.com/allegro/bigcache/v3"
	"github.com/pkg/errors"
)

// NewDefaultDoubleCheckFunc creates the default double-check cache with 10ms window.
// Can be overridden to customize default behavior or set to nil to disable by default.
var NewDefaultDoubleCheckFunc = func() (Cache[[]byte], time.Duration, error) {
	cache, err := NewBigCache(context.Background(), BigCacheConfig{
		Config: bigcache.Config{
			Shards:             16,
			LifeWindow:         10 * time.Millisecond,
			MaxEntriesInWindow: 10000,
			MaxEntrySize:       2, // Only store 2-byte timestamp
			CleanWindow:        1 * time.Second,
		},
	})
	if err != nil {
		return nil, 0, errors.Wrap(err, "failed to create default double-check cache")
	}
	return cache, 10 * time.Millisecond, nil
}

// parseDoubleCheckWindow validates and converts the window parameter for double-check optimization.
// Returns windowMS in milliseconds or an error if invalid.
func parseDoubleCheckWindow(window time.Duration) (int64, error) {
	windowMS := window.Milliseconds()

	// Strict check: window must be exactly representable in milliseconds
	if window != time.Duration(windowMS)*time.Millisecond {
		return 0, fmt.Errorf(
			"window %v is not a whole number of milliseconds (precision limited to 1ms)",
			window,
		)
	}

	if windowMS <= 0 {
		return 0, fmt.Errorf("window must be at least 1 millisecond")
	}

	if windowMS > 65535 {
		return 0, fmt.Errorf(
			"window %v exceeds maximum of 65535ms (65.5s) due to uint16 storage with millisecond precision",
			window,
		)
	}

	return windowMS, nil
}

// WithDoubleCheck configures or disables the double-check optimization.
//
// Note: Double-check is ENABLED BY DEFAULT with an internal 10ms window BigCache.
// Singleflight already prevents 99%+ of redundant fetches by deduplicating
// concurrent requests for the same key. Double-check is a supplementary optimization
// that eliminates the remaining edge cases in the narrow race window.
//
// Problem: When multiple requests concurrently access a missing key, Request B may
// check the cache (miss) while Request A is fetching. After A completes and writes
// the result, B would normally fetch again, causing redundant upstream calls.
//
// Solution: After A writes, it marks the key as "recently written". When B enters
// its fetch path, it detects this marker and re-checks the cache first, finding
// A's result and avoiding the redundant fetch.
//
// The window parameter defines how long a key is considered "recently written"
// (max 65535ms, must be whole milliseconds).
//
// See TestDoubleCheckRaceWindowProbability for a controlled test that demonstrates
// the race window scenario and double-check's effectiveness.
//
// Usage:
//   - WithDoubleCheck(nil, 0): Disable double-check optimization
//   - WithDoubleCheck(customCache, window): Use custom cache and window
//
// Parameters:
//   - cache: Cache to track recently written keys, or nil to disable
//   - window: Time window to consider a write as "recent" (whole milliseconds, max 65535ms)
//
// Resource Management:
//   - When using custom cache, you are responsible for closing it
//   - The client will NOT close custom caches provided via this option
//   - Always call defer client.Close() to clean up default resources
//
// Example (disable):
//
//	client := cachex.NewClient(backend, upstream,
//	    cachex.WithDoubleCheck[string](nil, 0),
//	)
//	defer client.Close()
//
// Example (custom):
//
//	cache, _ := cachex.NewBigCache(ctx, cachex.BigCacheConfig{...})
//	defer cache.Close() // You must close custom cache yourself
//	client := cachex.NewClient(backend, upstream,
//	    cachex.WithDoubleCheck[string](cache, 100*time.Millisecond),
//	)
//	defer client.Close()
func WithDoubleCheck[T any](cache Cache[[]byte], window time.Duration) ClientOption[T] {
	// Allow nil cache to disable double-check
	if cache == nil {
		return func(c *Client[T]) {
			c.recentWrites = nil
			c.recentWritesWindowMS = 0
			c.ownRecentWrites = true // Mark as explicitly configured
		}
	}

	windowMS, err := parseDoubleCheckWindow(window)
	if err != nil {
		panic(err)
	}

	return func(c *Client[T]) {
		c.recentWrites = cache
		c.recentWritesWindowMS = windowMS
		c.ownRecentWrites = false // User-provided cache, not managed by Client
	}
}
