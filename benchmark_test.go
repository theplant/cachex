package cachex

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Product represents a search result
type Product struct {
	ID          string
	Name        string
	Price       int64
	Stock       int
	Description string
}

// mockDB simulates a database with QPS limit and latency
type mockDB struct {
	qpsLimit      int64 // 0 means unlimited
	queryLatency  time.Duration
	currentQPS    atomic.Int64
	totalQueries  atomic.Int64
	ticker        *time.Ticker
	products      map[string]*Product
	mu            sync.RWMutex
	rejectedCount atomic.Int64
}

func newMockDB(qpsLimit int64, queryLatency time.Duration) *mockDB {
	db := &mockDB{
		qpsLimit:     qpsLimit,
		queryLatency: queryLatency,
		products:     make(map[string]*Product),
	}

	// Initialize 10000 products to simulate realistic scale
	for i := 1; i <= 10000; i++ {
		id := fmt.Sprintf("product-%d", i)
		db.products[id] = &Product{
			ID:          id,
			Name:        fmt.Sprintf("Product %d", i),
			Price:       int64(1000 + rand.Intn(9000)),
			Stock:       rand.Intn(1000),
			Description: fmt.Sprintf("Description for product %d", i),
		}
	}

	if qpsLimit > 0 {
		db.ticker = time.NewTicker(time.Second)
		go func() {
			for range db.ticker.C {
				db.currentQPS.Store(0)
			}
		}()
	}

	return db
}

func (db *mockDB) Query(ctx context.Context, id string) (*Product, error) {
	// Check QPS limit
	if db.qpsLimit > 0 {
		current := db.currentQPS.Add(1)
		if current > db.qpsLimit {
			db.rejectedCount.Add(1)
			return nil, fmt.Errorf("DB QPS limit exceeded")
		}
	}

	db.totalQueries.Add(1)

	// Simulate query latency
	time.Sleep(db.queryLatency)

	db.mu.RLock()
	product, exists := db.products[id]
	db.mu.RUnlock()

	if !exists {
		return nil, &ErrKeyNotFound{}
	}

	return product, nil
}

func (db *mockDB) Close() {
	if db.ticker != nil {
		db.ticker.Stop()
	}
}

func (db *mockDB) Stats() (total, rejected int64) {
	return db.totalQueries.Load(), db.rejectedCount.Load()
}

// BenchmarkScenario represents different DB configurations
type BenchmarkScenario struct {
	Name             string
	DBQPSLimit       int64
	DBLatency        time.Duration
	DataFreshTTL     time.Duration
	DataStaleTTL     time.Duration
	NotFoundFreshTTL time.Duration
	NotFoundStaleTTL time.Duration
	Concurrency      int
	Duration         time.Duration
	RequestsFunc     func() string // Generate request pattern
}

func BenchmarkProductSearch(b *testing.B) {
	// Realistic traffic pattern: 80% hot, 15% warm, 4% cold, 1% not-found (Pareto principle)
	realisticTrafficPattern := func() string {
		r := rand.Intn(1000)
		switch {
		case r < 800: // 80% hot products (top 20)
			return fmt.Sprintf("product-%d", rand.Intn(20)+1)
		case r < 950: // 15% warm products (21-200)
			return fmt.Sprintf("product-%d", rand.Intn(180)+21)
		case r < 990: // 4% cold products (201-1000)
			return fmt.Sprintf("product-%d", rand.Intn(800)+201)
		default: // 1% not found
			return fmt.Sprintf("product-notfound-%d", rand.Intn(50))
		}
	}

	scenarios := []BenchmarkScenario{
		{
			Name:             "High_Performance_DB",
			DBQPSLimit:       0,
			DBLatency:        5 * time.Millisecond,
			DataFreshTTL:     30 * time.Second, // Aggressive: more refreshes
			DataStaleTTL:     24 * time.Hour,
			NotFoundFreshTTL: 10 * time.Second,
			NotFoundStaleTTL: 24 * time.Hour,
			Concurrency:      100,
			Duration:         10 * time.Second,
			RequestsFunc:     realisticTrafficPattern,
		},
		{
			Name:             "Cloud_DB_1000QPS",
			DBQPSLimit:       1000,
			DBLatency:        10 * time.Millisecond,
			DataFreshTTL:     1 * time.Minute, // Moderate: balance freshness and load
			DataStaleTTL:     24 * time.Hour,
			NotFoundFreshTTL: 30 * time.Second,
			NotFoundStaleTTL: 24 * time.Hour,
			Concurrency:      100,
			Duration:         10 * time.Second,
			RequestsFunc:     realisticTrafficPattern,
		},
		{
			Name:             "Shared_DB_100QPS",
			DBQPSLimit:       100,
			DBLatency:        20 * time.Millisecond,
			DataFreshTTL:     5 * time.Minute, // Conservative: reduce refresh pressure
			DataStaleTTL:     24 * time.Hour,
			NotFoundFreshTTL: 2 * time.Minute,
			NotFoundStaleTTL: 24 * time.Hour,
			Concurrency:      100,
			Duration:         10 * time.Second,
			RequestsFunc:     realisticTrafficPattern,
		},
		{
			Name:             "Constrained_DB_50QPS",
			DBQPSLimit:       50,
			DBLatency:        30 * time.Millisecond,
			DataFreshTTL:     10 * time.Minute, // Very conservative: minimize DB load
			DataStaleTTL:     24 * time.Hour,
			NotFoundFreshTTL: 5 * time.Minute,
			NotFoundStaleTTL: 24 * time.Hour,
			Concurrency:      100,
			Duration:         10 * time.Second,
			RequestsFunc:     realisticTrafficPattern,
		},
	}

	for _, scenario := range scenarios {
		b.Run(scenario.Name, func(b *testing.B) {
			runScenario(b, scenario)
		})
	}
}

func runScenario(b *testing.B, scenario BenchmarkScenario) {
	// Setup mock DB
	db := newMockDB(scenario.DBQPSLimit, scenario.DBLatency)
	defer db.Close()

	// Setup cache layers: Memory (L1)
	// Use freshTTL + staleTTL as cache expiration to enable stale data serving
	config := DefaultRistrettoCacheConfig[*Entry[*Product]]()
	config.TTL = scenario.DataFreshTTL + scenario.DataStaleTTL

	l1Cache, err := NewRistrettoCache(config)
	if err != nil {
		b.Fatal(err)
	}
	defer l1Cache.Close()

	// Setup not-found cache
	// Use freshTTL + staleTTL as cache expiration
	notFoundConfig := DefaultRistrettoCacheConfig[time.Time]()
	notFoundConfig.TTL = scenario.NotFoundFreshTTL + scenario.NotFoundStaleTTL
	notFoundCache, err := NewRistrettoCache(notFoundConfig)
	if err != nil {
		b.Fatal(err)
	}
	defer notFoundCache.Close()

	// Upstream: DB query wrapped in Entry
	upstream := UpstreamFunc[*Entry[*Product]](func(ctx context.Context, key string) (*Entry[*Product], error) {
		product, err := db.Query(ctx, key)
		if err != nil {
			return nil, err
		}
		return &Entry[*Product]{
			Data:     product,
			CachedAt: NowFunc(),
		}, nil
	})

	// Build client with all features enabled
	client := NewClient(
		l1Cache,
		upstream,
		EntryWithTTL[*Product](scenario.DataFreshTTL, scenario.DataStaleTTL),
		NotFoundWithTTL[*Entry[*Product]](notFoundCache, scenario.NotFoundFreshTTL, scenario.NotFoundStaleTTL),
		WithServeStale[*Entry[*Product]](true),
		WithFetchTimeout[*Entry[*Product]](5*time.Second),
		WithFetchConcurrency[*Entry[*Product]](1), // Full singleflight (merge all concurrent requests)
	)

	// Warm up: pre-populate hot, warm, and cold products
	ctx := context.Background()

	// Warm up all hot products (top 20) - 100% coverage
	for i := 1; i <= 20; i++ {
		_, _ = client.Get(ctx, fmt.Sprintf("product-%d", i))
	}

	// Warm up all warm products (21-200) - 100% coverage
	for i := 21; i <= 200; i++ {
		_, _ = client.Get(ctx, fmt.Sprintf("product-%d", i))
	}

	// Warm up cold products (201-1000) - full coverage
	for i := 201; i <= 1000; i++ {
		_, _ = client.Get(ctx, fmt.Sprintf("product-%d", i))
	}

	// Warm up all not-found keys
	for i := 0; i < 50; i++ {
		_, _ = client.Get(ctx, fmt.Sprintf("product-notfound-%d", i))
	}

	// Wait for cache to settle
	time.Sleep(1 * time.Second)

	// Statistics
	var (
		totalRequests  atomic.Int64
		successCount   atomic.Int64
		notFoundCount  atomic.Int64
		errorCount     atomic.Int64
		latencies      sync.Map
		latencyBuckets [10]atomic.Int64 // <1ms, <5ms, <10ms, <50ms, <100ms, <200ms, <500ms, <1s, <2s, >=2s
	)

	// Start benchmark
	startTime := time.Now()
	var wg sync.WaitGroup

	// Launch concurrent workers
	stopCh := make(chan struct{})
	for i := 0; i < scenario.Concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
					// Generate request
					id := scenario.RequestsFunc()

					reqStart := time.Now()
					entry, err := client.Get(ctx, id)
					latency := time.Since(reqStart)

					totalRequests.Add(1)

					if err == nil && entry != nil && entry.Data != nil {
						successCount.Add(1)
					} else if IsErrKeyNotFound(err) {
						notFoundCount.Add(1)
					} else {
						errorCount.Add(1)
					}

					// Record latency
					recordLatency(&latencyBuckets, latency)

					// Sample latencies (store 1 in every 100 to avoid memory issues)
					if totalRequests.Load()%100 == 0 {
						latencies.Store(totalRequests.Load(), latency)
					}

					// Small sleep to avoid CPU spinning
					time.Sleep(time.Millisecond)
				}
			}
		}(i)
	}

	// Run for specified duration
	time.Sleep(scenario.Duration)
	close(stopCh)
	wg.Wait()

	elapsed := time.Since(startTime)

	// Calculate statistics
	total := totalRequests.Load()
	success := successCount.Load()
	notFound := notFoundCount.Load()
	errors := errorCount.Load()
	dbTotal, dbRejected := db.Stats()

	qps := float64(total) / elapsed.Seconds()
	cacheHitRate := float64(total-dbTotal) / float64(total) * 100

	// Calculate latency percentiles
	var allLatencies []time.Duration
	latencies.Range(func(key, value interface{}) bool {
		allLatencies = append(allLatencies, value.(time.Duration))
		return true
	})

	p50, p95, p99 := calculatePercentiles(allLatencies)

	// Report results
	b.ReportMetric(qps, "req/s")
	b.ReportMetric(float64(p50.Microseconds()), "p50_μs")
	b.ReportMetric(float64(p95.Microseconds()), "p95_μs")
	b.ReportMetric(float64(p99.Microseconds()), "p99_μs")
	b.ReportMetric(cacheHitRate, "cache_hit_%")
	b.ReportMetric(float64(dbTotal), "db_queries")
	b.ReportMetric(float64(dbRejected), "db_rejected")

	// Print detailed report
	fmt.Printf("\n")
	fmt.Printf("========== Scenario: %s ==========\n", scenario.Name)
	fmt.Printf("Configuration:\n")
	fmt.Printf("  DB QPS Limit:        %d/s (0=unlimited)\n", scenario.DBQPSLimit)
	fmt.Printf("  DB Latency:          %v\n", scenario.DBLatency)
	fmt.Printf("  Data Fresh TTL:      %v\n", scenario.DataFreshTTL)
	fmt.Printf("  Data Stale TTL:      %v\n", scenario.DataStaleTTL)
	fmt.Printf("  NotFound Fresh TTL:  %v\n", scenario.NotFoundFreshTTL)
	fmt.Printf("  NotFound Stale TTL:  %v\n", scenario.NotFoundStaleTTL)
	fmt.Printf("  Concurrency:         %d\n", scenario.Concurrency)
	fmt.Printf("  Duration:            %v\n", scenario.Duration)
	fmt.Printf("\n")
	fmt.Printf("Results:\n")
	fmt.Printf("  Total Requests:   %d\n", total)
	fmt.Printf("  Success:          %d (%.1f%%)\n", success, float64(success)/float64(total)*100)
	fmt.Printf("  Not Found:        %d (%.1f%%)\n", notFound, float64(notFound)/float64(total)*100)
	fmt.Printf("  Errors:           %d (%.1f%%)\n", errors, float64(errors)/float64(total)*100)
	fmt.Printf("  Overall QPS:      %.0f req/s\n", qps)
	fmt.Printf("\n")
	fmt.Printf("Cache Performance:\n")
	fmt.Printf("  Cache Hit Rate:   %.2f%%\n", cacheHitRate)
	fmt.Printf("  DB Queries:       %d (%.1f%%)\n", dbTotal, float64(dbTotal)/float64(total)*100)
	fmt.Printf("  DB Rejected:      %d\n", dbRejected)
	if scenario.DBQPSLimit > 0 {
		dbUtilization := float64(dbTotal) / float64(scenario.DBQPSLimit) / elapsed.Seconds() * 100
		fmt.Printf("  DB Utilization:   %.1f%% of limit\n", dbUtilization)
	}
	fmt.Printf("\n")
	fmt.Printf("Latency:\n")
	fmt.Printf("  P50:              %v\n", p50)
	fmt.Printf("  P95:              %v\n", p95)
	fmt.Printf("  P99:              %v\n", p99)
	fmt.Printf("\n")
	fmt.Printf("Latency Distribution:\n")
	printLatencyDistribution(&latencyBuckets, total)
	fmt.Printf("==========================================\n\n")
}

func recordLatency(buckets *[10]atomic.Int64, latency time.Duration) {
	switch {
	case latency < time.Millisecond:
		buckets[0].Add(1)
	case latency < 5*time.Millisecond:
		buckets[1].Add(1)
	case latency < 10*time.Millisecond:
		buckets[2].Add(1)
	case latency < 50*time.Millisecond:
		buckets[3].Add(1)
	case latency < 100*time.Millisecond:
		buckets[4].Add(1)
	case latency < 200*time.Millisecond:
		buckets[5].Add(1)
	case latency < 500*time.Millisecond:
		buckets[6].Add(1)
	case latency < time.Second:
		buckets[7].Add(1)
	case latency < 2*time.Second:
		buckets[8].Add(1)
	default:
		buckets[9].Add(1)
	}
}

func calculatePercentiles(latencies []time.Duration) (p50, p95, p99 time.Duration) {
	if len(latencies) == 0 {
		return 0, 0, 0
	}

	// Sort latencies
	sorted := make([]time.Duration, len(latencies))
	copy(sorted, latencies)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i] < sorted[j]
	})

	p50 = sorted[len(sorted)*50/100]
	p95 = sorted[len(sorted)*95/100]
	p99 = sorted[len(sorted)*99/100]

	return
}

func printLatencyDistribution(buckets *[10]atomic.Int64, total int64) {
	labels := []string{
		"<1ms", "<5ms", "<10ms", "<50ms", "<100ms",
		"<200ms", "<500ms", "<1s", "<2s", ">=2s",
	}

	for i, label := range labels {
		count := buckets[i].Load()
		percentage := float64(count) / float64(total) * 100
		bar := ""
		barLen := int(percentage / 2)
		for j := 0; j < barLen; j++ {
			bar += "█"
		}
		fmt.Printf("  %-8s %6d (%5.1f%%) %s\n", label, count, percentage, bar)
	}
}
