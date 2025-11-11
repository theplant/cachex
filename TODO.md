# TODO

## GORM Cache Improvements

### Cleanup Worker

Implement a background cleanup worker for `GORMCache` to remove stale entries:

- **Requirements:**

  - Must operate with fine-grained batches (avoid large bulk deletions)
  - Should run continuously with a configurable interval
  - Delete strategy: small batches over time (e.g., 100 entries per batch)
  - Should respect database load and implement backpressure
  - Consider adding metrics for monitoring cleanup operations

- **Implementation considerations:**
  - Use `updated_at` timestamp for identifying stale entries
  - Configurable staleness threshold (e.g., delete entries older than 7 days)
  - Graceful shutdown support (stop worker when cache is closed)
  - Optional: adaptive batch sizing based on database performance

## Hotness Statistics

### Core Hotness Tracking Feature

Implement a hotness statistics mechanism to track cache key access patterns:

- **Requirements:**

  - Track access frequency for each key
  - Track last access timestamp
  - Decay mechanism to reduce weight of old accesses
  - Efficient data structure (e.g., concurrent map or time-windowed counter)
  - Configurable retention period for statistics

- **Metrics to track:**
  - Hit count per key
  - Last access time
  - Access rate (hits per time window)
  - Optional: distinguish between cache hits and cache misses

### CacheWrapper for Hot Key Recording

Implement a `CacheWrapper` that wraps existing `Cache` implementations to record hot keys:

- **Requirements:**

  - Wraps any `Cache[T]` implementation
  - Records top N hottest keys based on access patterns
  - Export hot keys list on `Close()` for persistence
  - Support loading hot keys on startup for cache warming

- **Use cases:**

  - Identify which keys should be warmed up on application restart
  - Analyze cache access patterns
  - Optimize cache allocation strategies

- **API design considerations:**

```go
type HotKeyRecorder[T any] struct {
    backend Cache[T]
    stats   *HotnessStats
    topN    int
}

// Export hot keys when closing
func (h *HotKeyRecorder[T]) Close() (hotKeys []string, err error)

// Load hot keys for warming
// Note: For batch warming with RistrettoCache, Wait() can be called once
// at the end after all Set operations to improve performance
func (h *HotKeyRecorder[T]) WarmUp(ctx context.Context, keys []string) error
```

### Traffic Shaping Based on Hotness

Implement a traffic shaping mechanism to protect upstream services and databases:

- **Requirements:**

  - Differentiate fetch concurrency based on key hotness
  - Hot keys: higher concurrency (faster response for majority users)
  - Cold keys: lower concurrency (protect database stability)
  - Gradual throttling to avoid sudden traffic spikes

- **Implementation strategy:**

  - Maintain separate fetch concurrency limits for hot/cold keys
  - Hot key threshold: configurable percentile (e.g., top 20% by access rate)
  - Cold keys: use smaller concurrency or rate limiting
  - Dynamic adjustment based on real-time access patterns

- **Configuration example:**

```go
type TrafficShapingConfig struct {
    HotKeyConcurrency  int     // e.g., 10
    ColdKeyConcurrency int     // e.g., 2
    HotKeyPercentile   float64 // e.g., 0.8 (top 20%)
}
```

- **Benefits:**
  - Protect database from cold key storms
  - Ensure good performance for majority of users (hot keys)
  - Prevent slow queries on cold keys from affecting hot key performance
  - Better resource allocation under high load

## Implementation Priority

1. **Phase 1:** Hotness Statistics (foundation for other features)
2. **Phase 2:** CacheWrapper for Hot Key Recording (enables cache warming)
3. **Phase 3:** GORM Cleanup Worker (operational stability)
4. **Phase 4:** Traffic Shaping (advanced optimization)

## Additional Considerations

- All features should be optional and configurable
- Maintain backward compatibility with existing API
- Add comprehensive benchmarks to measure overhead
- Document performance characteristics and trade-offs
