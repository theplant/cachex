package cachex

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestEntry(t *testing.T) {
	t.Run("create entry with string", func(t *testing.T) {
		now := time.Now()
		entry := &Entry[string]{
			Data:     "test data",
			CachedAt: now,
		}

		assert.Equal(t, "test data", entry.Data)
		assert.Equal(t, now, entry.CachedAt)
	})

	t.Run("entry with pointer type", func(t *testing.T) {
		type User struct {
			ID   string
			Name string
		}

		now := time.Now()
		user := &User{
			ID:   "user-123",
			Name: "Test User",
		}
		entry := &Entry[*User]{
			Data:     user,
			CachedAt: now,
		}

		assert.Equal(t, "user-123", entry.Data.ID)
		assert.Equal(t, "Test User", entry.Data.Name)
		assert.Equal(t, now, entry.CachedAt)
	})

	t.Run("entry with struct type", func(t *testing.T) {
		type Product struct {
			ID    string
			Name  string
			Price float64
		}

		now := time.Now()
		product := Product{
			ID:    "prod-456",
			Name:  "Test Product",
			Price: 99.99,
		}
		entry := &Entry[Product]{
			Data:     product,
			CachedAt: now,
		}

		assert.Equal(t, "prod-456", entry.Data.ID)
		assert.Equal(t, "Test Product", entry.Data.Name)
		assert.Equal(t, 99.99, entry.Data.Price)
		assert.Equal(t, now, entry.CachedAt)
	})
}

func TestEntryWithTTL(t *testing.T) {
	freshTTL := 5 * time.Second
	staleTTL := 10 * time.Second

	t.Run("fresh entry", func(t *testing.T) {
		now := time.Now()
		oldNowFunc := NowFunc
		NowFunc = func() time.Time { return now }
		defer func() { NowFunc = oldNowFunc }()

		entry := &Entry[string]{
			Data:     "fresh data",
			CachedAt: now.Add(-3 * time.Second), // 3 seconds ago
		}

		option := EntryWithTTL[string](freshTTL, staleTTL)
		client := &Client[*Entry[string]]{}
		option(client)

		state := client.checkDataStale(entry)
		assert.Equal(t, StateFresh, state)
	})

	t.Run("stale entry", func(t *testing.T) {
		now := time.Now()
		oldNowFunc := NowFunc
		NowFunc = func() time.Time { return now }
		defer func() { NowFunc = oldNowFunc }()

		entry := &Entry[string]{
			Data:     "stale data",
			CachedAt: now.Add(-8 * time.Second), // 8 seconds ago
		}

		option := EntryWithTTL[string](freshTTL, staleTTL)
		client := &Client[*Entry[string]]{}
		option(client)

		state := client.checkDataStale(entry)
		assert.Equal(t, StateStale, state)
	})

	t.Run("rotten entry", func(t *testing.T) {
		now := time.Now()
		oldNowFunc := NowFunc
		NowFunc = func() time.Time { return now }
		defer func() { NowFunc = oldNowFunc }()

		entry := &Entry[string]{
			Data:     "rotten data",
			CachedAt: now.Add(-20 * time.Second), // 20 seconds ago
		}

		option := EntryWithTTL[string](freshTTL, staleTTL)
		client := &Client[*Entry[string]]{}
		option(client)

		state := client.checkDataStale(entry)
		assert.Equal(t, StateRotten, state)
	})

	t.Run("exact boundary - fresh to stale", func(t *testing.T) {
		now := time.Now()
		oldNowFunc := NowFunc
		NowFunc = func() time.Time { return now }
		defer func() { NowFunc = oldNowFunc }()

		entry := &Entry[string]{
			Data:     "boundary data",
			CachedAt: now.Add(-freshTTL), // exactly at freshTTL boundary
		}

		option := EntryWithTTL[string](freshTTL, staleTTL)
		client := &Client[*Entry[string]]{}
		option(client)

		state := client.checkDataStale(entry)
		assert.Equal(t, StateStale, state)
	})

	t.Run("exact boundary - stale to rotten", func(t *testing.T) {
		now := time.Now()
		oldNowFunc := NowFunc
		NowFunc = func() time.Time { return now }
		defer func() { NowFunc = oldNowFunc }()

		entry := &Entry[string]{
			Data:     "boundary data",
			CachedAt: now.Add(-(freshTTL + staleTTL)), // exactly at staleTTL boundary
		}

		option := EntryWithTTL[string](freshTTL, staleTTL)
		client := &Client[*Entry[string]]{}
		option(client)

		state := client.checkDataStale(entry)
		assert.Equal(t, StateRotten, state)
	})
}

func TestEntryWithClient(t *testing.T) {
	t.Run("complete integration example", func(t *testing.T) {
		// Create in-memory cache
		config := DefaultRistrettoCacheConfig[*Entry[string]]()
		config.TTL = 1 * time.Minute
		cache, err := NewRistrettoCache(config)
		assert.NoError(t, err)
		defer cache.Close()

		// Mock upstream data source
		fetchCount := 0
		upstream := UpstreamFunc[*Entry[string]](func(ctx context.Context, key string) (*Entry[string], error) {
			fetchCount++
			return &Entry[string]{
				Data:     "value-" + key,
				CachedAt: NowFunc(),
			}, nil
		})

		// Create client with EntryWithTTL
		client := NewClient(
			cache,
			upstream,
			EntryWithTTL[string](100*time.Millisecond, 500*time.Millisecond), // 100ms fresh, 500ms stale
			WithServeStale[*Entry[string]](true),
		)

		ctx := context.Background()

		// First call: cache miss, fetch from upstream
		entry1, err := client.Get(ctx, "test-key")
		assert.NoError(t, err)
		assert.Equal(t, "value-test-key", entry1.Data)
		assert.Equal(t, 1, fetchCount)

		// Second call: cache hit (fresh)
		entry2, err := client.Get(ctx, "test-key")
		assert.NoError(t, err)
		assert.Equal(t, "value-test-key", entry2.Data)
		assert.Equal(t, 1, fetchCount) // No additional fetch

		// Wait for data to become stale
		time.Sleep(150 * time.Millisecond)

		// Third call: cache hit (stale), async refresh
		entry3, err := client.Get(ctx, "test-key")
		assert.NoError(t, err)
		assert.Equal(t, "value-test-key", entry3.Data)
		// fetchCount might be 1 or 2 depending on async refresh timing

		// Wait for async refresh to complete
		time.Sleep(100 * time.Millisecond)

		// Fourth call: should have fresh data again
		entry4, err := client.Get(ctx, "test-key")
		assert.NoError(t, err)
		assert.Equal(t, "value-test-key", entry4.Data)
		assert.GreaterOrEqual(t, fetchCount, 2) // At least 2 fetches (initial + refresh)
	})
}
