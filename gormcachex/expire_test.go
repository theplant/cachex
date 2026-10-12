package gormcachex

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
)

// testDeleteExpired checks DeleteExpired on backends from newB, which
// returns an empty table's backend with leases on and the given ChunkSize.
func testDeleteExpired(t *testing.T, newB func(t *testing.T, chunkSize int) *Backend[string]) {
	ctx := context.Background()
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)

	t.Run("deletes the rotten rows, in batches, and keeps the others", func(t *testing.T) {
		b := newB(t, 2)
		entries := map[string]cachex.ByteEntry{}
		for i := range 5 {
			entries[fmt.Sprint("old", i)] = cachex.ByteEntry{Data: []byte("x"), ExpiresAt: past}
		}
		require.NoError(t, b.bytes.SetMany(ctx, entries))
		require.NoError(t, b.Set(ctx, "live", cachex.Entry[string]{Value: "v", ExpiresAt: future}))
		n, err := b.DeleteExpired(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(5), n)
		for key := range entries {
			_, ok, err := b.bytes.Get(ctx, key)
			require.NoError(t, err)
			assert.False(t, ok, key)
		}
		_, ok, err := b.Get(ctx, "live")
		require.NoError(t, err)
		assert.True(t, ok)
		n, err = b.DeleteExpired(ctx)
		require.NoError(t, err)
		assert.Zero(t, n, "nothing left")
	})

	t.Run("keeps a row whose lease is held, and drops one whose lease expired", func(t *testing.T) {
		b := newB(t, 0)
		l := b.Leaser()
		require.NoError(t, b.bytes.SetMany(ctx, map[string]cachex.ByteEntry{"held": {Data: []byte("x"), ExpiresAt: past}}))
		tokens, err := l.Lease(ctx, []string{"held"}, time.Minute) // a fetch of the rotten key is running
		require.NoError(t, err)
		require.NotEmpty(t, tokens["held"])
		_, err = l.Lease(ctx, []string{"gone"}, time.Millisecond) // a lease of a missing key, which expires
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond)
		n, err := b.DeleteExpired(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(1), n, "only the row of the expired lease")
		require.NoError(t, l.PutLeased(ctx, tokens, map[string]cachex.Entry[string]{"held": {Value: "v", ExpiresAt: future}}))
		_, ok, err := b.Get(ctx, "held")
		require.NoError(t, err)
		assert.True(t, ok, "the fetch holding the lease backfilled its row")
	})

	t.Run("stops when ctx ends", func(t *testing.T) {
		b := newB(t, 1)
		require.NoError(t, b.bytes.SetMany(ctx, map[string]cachex.ByteEntry{"a": {Data: []byte("x"), ExpiresAt: past}, "b": {Data: []byte("x"), ExpiresAt: past}}))
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		_, err := b.DeleteExpired(cctx)
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestDeleteExpired(t *testing.T) {
	testDeleteExpired(t, func(t *testing.T, chunkSize int) *Backend[string] {
		return newBackend(t, Config[string]{KeyPrefix: "p:", Leases: true, ChunkSize: chunkSize})
	})
}

func TestDeleteExpiredOnATableWithoutLeaseColumns(t *testing.T) {
	ctx := context.Background()
	db := newSQLite(t)
	require.NoError(t, db.Exec("CREATE TABLE old (`key` TEXT PRIMARY KEY, `value` BLOB NOT NULL, `expires_at` DATETIME NOT NULL, `updated_at` DATETIME NOT NULL)").Error)
	b := New[string](Config[string]{DB: db, TableName: "old"})
	require.NoError(t, b.bytes.SetMany(ctx, map[string]cachex.ByteEntry{"a": {Data: []byte("x"), ExpiresAt: time.Now().Add(-time.Hour)}}))
	n, err := b.DeleteExpired(ctx)
	require.NoError(t, err, "leases off: the lease columns are not needed")
	assert.Equal(t, int64(1), n)
}

func TestTimesInAnotherOffsetCompareRightOnSQLite(t *testing.T) {
	// SQLite compares times as text with their offsets: an hour ahead in
	// UTC-10 reads as long past next to now in UTC or local time
	ctx := context.Background()
	b := newBackend(t, Config[string]{})
	far := time.Now().Add(time.Hour).In(time.FixedZone("W", -10*3600))
	require.NoError(t, b.Set(ctx, "live", cachex.Entry[string]{Value: "v", ExpiresAt: far}))
	n, err := b.DeleteExpired(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "the entry has an hour left")
}
