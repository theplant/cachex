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

	"golang.org/x/sync/semaphore"
)

// Product represents a search result
type Product struct {
	ID          string
	Name        string
	Price       int64
	Stock       int
	Description string
}

// mockDB simulates a database with connection limit and latency
type mockDB struct {
	connLimit     int64 // 0 means unlimited, represents max concurrent connections
	queryLatency  time.Duration
	sem           *semaphore.Weighted
	totalQueries  atomic.Int64
	products      map[string]*Product
	mu            sync.RWMutex
	rejectedCount atomic.Int64 // Tracks queries that failed to acquire semaphore
}

func newMockDB(connLimit int64, queryLatency time.Duration) *mockDB {
	db := &mockDB{
		connLimit:    connLimit,
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

	// Initialize semaphore for connection limit
	if connLimit > 0 {
		db.sem = semaphore.NewWeighted(connLimit)
	}

	return db
}

func (db *mockDB) Query(ctx context.Context, id string) (*Product, error) {
	// Acquire connection from semaphore (wait if all connections are busy)
	if db.sem != nil {
		if err := db.sem.Acquire(ctx, 1); err != nil {
			db.rejectedCount.Add(1)
			return nil, fmt.Errorf("failed to acquire DB connection: %w", err)
		}
		defer db.sem.Release(1)
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
	// No cleanup needed for semaphore
}

func (db *mockDB) Stats() (total, rejected int64) {
	return db.totalQueries.Load(), db.rejectedCount.Load()
}

// BenchmarkScenario represents different DB configurations
type BenchmarkScenario struct {
	Name             string
	DBConnLimit      int64         // Max concurrent DB connections (0=unlimited)
	DBLatency        time.Duration // Simulated DB query latency
	FetchTimeout     time.Duration // Timeout for upstream fetch
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
			DBConnLimit:      100, // High-performance DB with large connection pool
			DBLatency:        90 * time.Millisecond,
			FetchTimeout:     2 * time.Second,
			DataFreshTTL:     1 * time.Second,
			DataStaleTTL:     24 * time.Hour,
			NotFoundFreshTTL: 500 * time.Millisecond,
			NotFoundStaleTTL: 24 * time.Hour,
			Concurrency:      600,
			Duration:         10 * time.Second,
			RequestsFunc:     realisticTrafficPattern,
		},
		{
			Name:             "Cloud_DB_1000QPS",
			DBConnLimit:      20, // Target 90-93% utilization
			DBLatency:        85 * time.Millisecond,
			FetchTimeout:     1 * time.Second,
			DataFreshTTL:     5 * time.Second,
			DataStaleTTL:     24 * time.Hour,
			NotFoundFreshTTL: 3 * time.Second,
			NotFoundStaleTTL: 24 * time.Hour,
			Concurrency:      100,
			Duration:         10 * time.Second,
			RequestsFunc:     realisticTrafficPattern,
		},
		{
			Name:             "Shared_DB_100QPS",
			DBConnLimit:      13, // Target 90-93% utilization
			DBLatency:        125 * time.Millisecond,
			FetchTimeout:     5 * time.Second,
			DataFreshTTL:     10 * time.Second,
			DataStaleTTL:     24 * time.Hour,
			NotFoundFreshTTL: 5 * time.Second,
			NotFoundStaleTTL: 24 * time.Hour,
			Concurrency:      100,
			Duration:         10 * time.Second,
			RequestsFunc:     realisticTrafficPattern,
		},
		{
			Name:             "Constrained_DB_50QPS",
			DBConnLimit:      8, // Target 90-93% utilization
			DBLatency:        190 * time.Millisecond,
			FetchTimeout:     10 * time.Second,
			DataFreshTTL:     20 * time.Second,
			DataStaleTTL:     24 * time.Hour,
			NotFoundFreshTTL: 10 * time.Second,
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
	db := newMockDB(scenario.DBConnLimit, scenario.DBLatency)
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
		WithFetchTimeout[*Entry[*Product]](scenario.FetchTimeout),
	)

	// No pre-warming - test cold start performance
	ctx := context.Background()

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
	if scenario.DBConnLimit > 0 {
		fmt.Printf("  DB Conn Limit:       %d\n", scenario.DBConnLimit)
	} else {
		fmt.Printf("  DB Conn Limit:       unlimited\n")
	}
	fmt.Printf("  DB Latency:          %v\n", scenario.DBLatency)
	fmt.Printf("  Fetch Timeout:       %v\n", scenario.FetchTimeout)
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
	actualDBQPS := float64(dbTotal) / elapsed.Seconds()
	fmt.Printf("  DB QPS:           %.1f req/s\n", actualDBQPS)
	fmt.Printf("  DB Rejected:      %d\n", dbRejected)
	if scenario.DBConnLimit > 0 {
		expectedMaxQueries := float64(scenario.DBConnLimit) * elapsed.Seconds() / scenario.DBLatency.Seconds()
		dbUtilization := float64(dbTotal) / expectedMaxQueries * 100
		fmt.Printf("  DB Utilization:   %.1f%% of capacity\n", dbUtilization)
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
