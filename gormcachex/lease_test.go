package gormcachex

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

// testLeases runs the lease contract on backends from newB, which returns
// an empty table's backend with leases on.
func testLeases(t *testing.T, newB func(t *testing.T, chunkSize int) *Backend[string]) {
	cachextest.TestLeaser(t, func(t *testing.T) cachex.Backend[string] { return newB(t, 0) }, "v")
	t.Run("in chunks", func(t *testing.T) {
		cachextest.TestLeaser(t, func(t *testing.T) cachex.Backend[string] { return newB(t, 2) }, "v")
	})
	cachextest.TestBackend(t, func(t *testing.T) cachex.Backend[string] { return newB(t, 0) }, strs)
	t.Run("statements keep working once PostgreSQL plans them generically", func(t *testing.T) {
		// a prepared statement gets a generic plan from its 6th run, which
		// some statements (ON CONFLICT on a partial index, say) fail under
		ctx := context.Background()
		b := newB(t, 0)
		l := b.Leaser()
		e := cachex.Entry[string]{Value: "v", ExpiresAt: time.Now().Add(time.Hour)}
		for i := range 8 {
			keys := []string{fmt.Sprint("a", i), fmt.Sprint("b", i)}
			tokens, err := l.Lease(ctx, keys, time.Minute)
			require.NoError(t, err, "run %d", i+1)
			require.Len(t, tokens, 2, "run %d", i+1)
			require.NoError(t, l.PutLeased(ctx, tokens, map[string]cachex.Entry[string]{keys[0]: e}), "run %d", i+1)
			tokens, err = l.Lease(ctx, keys, time.Minute)
			require.NoError(t, err, "run %d", i+1)
			require.NoError(t, l.Release(ctx, tokens), "run %d", i+1)
			require.NoError(t, b.SetMany(ctx, map[string]cachex.Entry[string]{keys[0]: e, keys[1]: e}), "run %d", i+1)
			require.NoError(t, b.DelMany(ctx, keys), "run %d", i+1)
		}
	})
	t.Run("a lease of a missing key is not a row a read sees", func(t *testing.T) {
		ctx := context.Background()
		b := newB(t, 0)
		l := b.Leaser()
		tokens, err := l.Lease(ctx, []string{"k"}, time.Minute)
		require.NoError(t, err)
		require.NotEmpty(t, tokens["k"])
		_, ok, err := b.Get(ctx, "k")
		require.NoError(t, err)
		assert.False(t, ok)
		m, err := b.GetMany(ctx, []string{"k"})
		require.NoError(t, err)
		assert.Empty(t, m)
		assert.Empty(t, mustLease(t, l, "k"), "the read did not drop the lease as an entry that does not decode")
	})
}

func mustLease(t *testing.T, l cachex.Leaser[string], keys ...string) map[string]string {
	t.Helper()
	tokens, err := l.Lease(context.Background(), keys, time.Minute)
	require.NoError(t, err)
	return tokens
}

func TestLeases(t *testing.T) {
	testLeases(t, func(t *testing.T, chunkSize int) *Backend[string] {
		return newBackend(t, Config[string]{KeyPrefix: "p:", Leases: true, ChunkSize: chunkSize})
	})
}

func TestLeasesAreOffByDefault(t *testing.T) {
	assert.Nil(t, newBackend(t, Config[string]{}).Leaser())
}

func TestATableWithoutLeaseColumnsWorksWithLeasesOff(t *testing.T) {
	// a table made before leases, or by hand: only leases need Migrate's columns
	ctx := context.Background()
	db := newSQLite(t)
	require.NoError(t, db.Exec("CREATE TABLE old (`key` TEXT PRIMARY KEY, `value` BLOB NOT NULL, `expires_at` DATETIME NOT NULL, `updated_at` DATETIME NOT NULL)").Error)
	b := New[string](Config[string]{DB: db, TableName: "old"})
	require.NoError(t, b.Set(ctx, "a", entry("v")))
	require.NoError(t, b.SetMany(ctx, map[string]cachex.Entry[string]{"b": entry("v"), "c": entry("v")}))
	m, err := b.GetMany(ctx, []string{"a", "b", "c"})
	require.NoError(t, err)
	assert.Len(t, m, 3)
	require.NoError(t, b.Migrate(ctx))
	assert.True(t, db.Table("old").Migrator().HasColumn(&leasedRow{}, "lease_token"), "Migrate adds the lease columns")
}

func TestItIsNotLocal(t *testing.T) {
	var b any = newBackend(t, Config[string]{})
	_, ok := b.(cachex.LocalBackend)
	assert.False(t, ok, "a table is shared: a broadcast never clears it")
}

func TestTheLargestChunkWritesWithLeasesOn(t *testing.T) {
	// a leased row binds 6 parameters, not 4: 8000 of them pass SQLite's
	// 32766
	ctx := context.Background()
	b := newBackend(t, Config[string]{Leases: true, ChunkSize: maxChunkSize})
	entries := map[string]cachex.Entry[string]{}
	for i := range maxChunkSize {
		entries[fmt.Sprint("k", i)] = cachex.Entry[string]{Value: "v", ExpiresAt: time.Now().Add(time.Hour)}
	}
	require.NoError(t, b.SetMany(ctx, entries))
}

func TestAKeyTooLongToLeaseIsStillRefreshed(t *testing.T) {
	// it can never be leased (nor stored): that is not another instance
	// holding its lease, so a stale hit still refreshes it
	ctx := context.Background()
	key := strings.Repeat("k", 300)
	var mu sync.Mutex
	data := "v1"
	src := cachex.SourceFunc[string](func(context.Context, string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		return data, nil
	})
	clock := cachextest.NewClock(time.Now())
	c := cachex.New(src, []cachex.Layer[string]{
		cachex.NewLayer[string](cachextest.NewMap[string](), cachex.TTL(time.Second, time.Hour), cachex.Jitter(0)),
		cachex.NewLayer[string](newBackend(t, Config[string]{Leases: true}), cachex.TTL(time.Second, time.Hour), cachex.Jitter(0)),
	}, cachex.WithNow(clock.Now))
	defer func() { _ = c.Close() }()
	_, err := c.Get(ctx, key)
	require.NoError(t, err)
	mu.Lock()
	data = "v2"
	mu.Unlock()
	clock.Advance(2 * time.Second)
	assert.Eventually(t, func() bool { v, err := c.Get(ctx, key); return err == nil && v == "v2" }, 5*time.Second, 5*time.Millisecond)
}
