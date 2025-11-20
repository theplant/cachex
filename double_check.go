package cachex

// DoubleCheckMode defines the double-check optimization strategy
type DoubleCheckMode int

const (
	// DoubleCheckDisabled turns off double-check optimization
	DoubleCheckDisabled DoubleCheckMode = iota

	// DoubleCheckEnabled always performs double-check before upstream fetch
	DoubleCheckEnabled

	// DoubleCheckAuto enables double-check based on configuration (default):
	// - Enabled when notFoundCache exists (can leverage it to catch not-found in race window)
	// - Disabled when no notFoundCache (cost limited to backend only)
	DoubleCheckAuto
)

// WithDoubleCheck configures the double-check optimization mode.
//
// Default: DoubleCheckAuto (smart detection based on notFoundCache configuration)
//
// Background: Double-check works together with singleflight to reduce redundant upstream calls:
//   - Singleflight: Deduplicates concurrent requests for the same key (same moment)
//   - Double-check: Handles slightly staggered requests in race window (near-miss timing)
//
// Double-check queries backend (and notFoundCache if configured) one more time
// before going to upstream. This addresses the race window where:
//  1. Request A writes to cache
//  2. Request B misses cache (A's write not yet visible or in-flight)
//  3. Request B enters fetch path and would normally query upstream
//  4. Double-check catches A's write, avoiding redundant upstream query
//
// Effectiveness (see TestDoubleCheckRaceWindowProbability for controlled test):
//   - Test simulates worst-case scenario: two-wave concurrent pattern with precise timing
//   - Test results: ~40% redundant fetches without double-check, 0% with double-check
//   - Real-world impact: typically much lower race window probability, actual benefit varies
//
// Effectiveness depends on:
//   - Concurrent access patterns (higher concurrency = more benefit)
//   - Race window duration (network latency, cache propagation delay)
//   - Cost ratio between double-check and upstream query
//
// Modes:
//   - DoubleCheckDisabled: Skip double-check
//     Use when: backend query cost >= upstream cost, or backend is unreliable/slow,
//     or without notFoundCache in scenarios where upstream frequently returns not-found
//     (double-check cannot catch not-found without notFoundCache, reducing effectiveness)
//   - DoubleCheckEnabled: Always double-check (adds query cost, reduces upstream calls)
//     Use when: upstream is significantly more expensive than backend queries
//   - DoubleCheckAuto: Smart detection based on notFoundCache (default)
//     Enables when notFoundCache exists (double-check covers both found and not-found scenarios),
//     disables otherwise (double-check only covers found scenario, limited effectiveness)
//
// Cost-benefit analysis:
//
//	Cost = backend_query [+ notFoundCache_query if configured]
//	Benefit = Avoid upstream_query when hitting race window
//
//	Worth enabling when: upstream_cost >> (backend_cost + notFoundCache_cost)
//
// Recommendations by scenario:
//   - Memory cache -> DB: DoubleCheckEnabled (DB ≫ memory, ~10000x difference)
//   - Redis -> DB: DoubleCheckEnabled (DB ≫ Redis, ~10-50x difference)
//   - Redis (+ notFoundCache) -> Redis: DoubleCheckDisabled (cost ≈ benefit)
//   - Default/Uncertain: DoubleCheckAuto (smart heuristic)
func WithDoubleCheck[T any](mode DoubleCheckMode) ClientOption[T] {
	return func(c *Client[T]) {
		c.doubleCheckMode = mode
	}
}
