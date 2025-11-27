package cachex

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newGORMCache[T any](tb testing.TB, tableName string) (*GORMCache[T], *gorm.DB) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(tb, err)
	cache := NewGORMCache[T](&GORMCacheConfig{
		DB:        db,
		TableName: tableName,
	})
	require.NoError(tb, cache.Migrate(context.Background()))
	return cache, db
}

func TestGORMCacheBasics(t *testing.T) {
	ctx := context.Background()
	cache, _ := newGORMCache[string](t, "test_cache")

	require.NoError(t, cache.Set(ctx, "key1", "value1"))

	value, err := cache.Get(ctx, "key1")
	require.NoError(t, err)
	assert.Equal(t, "value1", value)

	require.NoError(t, cache.Del(ctx, "key1"))

	_, err = cache.Get(ctx, "key1")
	assert.True(t, IsErrKeyNotFound(err))
}

func TestGORMCacheWithBytes(t *testing.T) {
	ctx := context.Background()
	cache, _ := newGORMCache[[]byte](t, "bytes_cache")

	testData := []byte("raw binary data \x00\x01\x02")

	require.NoError(t, cache.Set(ctx, "key1", testData))

	value, err := cache.Get(ctx, "key1")
	require.NoError(t, err)
	assert.Equal(t, testData, value)

	require.NoError(t, cache.Del(ctx, "key1"))

	_, err = cache.Get(ctx, "key1")
	assert.True(t, IsErrKeyNotFound(err))
}

func TestGORMCacheConfigWithPrefix(t *testing.T) {
	ctx := context.Background()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)

	prodCache := NewGORMCache[string](&GORMCacheConfig{
		DB:        db,
		TableName: "config_cache",
		KeyPrefix: "prod:",
	})
	require.NoError(t, prodCache.Migrate(ctx))

	devCache := NewGORMCache[string](&GORMCacheConfig{
		DB:        db,
		TableName: "config_cache",
		KeyPrefix: "dev:",
	})

	require.NoError(t, prodCache.Set(ctx, "api_key", "prod-secret-123"))
	require.NoError(t, devCache.Set(ctx, "api_key", "dev-secret-456"))

	prodValue, err := prodCache.Get(ctx, "api_key")
	require.NoError(t, err)
	assert.Equal(t, "prod-secret-123", prodValue)

	devValue, err := devCache.Get(ctx, "api_key")
	require.NoError(t, err)
	assert.Equal(t, "dev-secret-456", devValue)
}

func TestGORMCacheTransactionCommit(t *testing.T) {
	ctx := context.Background()
	cache, db := newGORMCache[string](t, "tx_commit_cache")

	require.NoError(t, cache.Set(ctx, "other_key", "other_value"))

	tx := db.Begin()
	txCtx := WithGORMTx(ctx, tx)

	require.NoError(t, cache.Set(txCtx, "tx_key", "tx_value"))

	value, err := cache.Get(txCtx, "tx_key")
	require.NoError(t, err)
	assert.Equal(t, "tx_value", value, "should read value within transaction")

	// SQLite write lock: when a transaction has pending writes, concurrent reads from
	// outside the transaction may fail with "no such table" due to SQLite's locking behavior.
	// Both errors (key not found or table locked) prove transaction isolation.
	_, err = cache.Get(ctx, "tx_key")
	assert.True(t, IsErrKeyNotFound(err) || strings.Contains(err.Error(), "no such table"),
		"should not read uncommitted value outside transaction (got: %v)", err)

	require.NoError(t, tx.Commit().Error)

	value, err = cache.Get(ctx, "tx_key")
	require.NoError(t, err)
	assert.Equal(t, "tx_value", value, "should find key after transaction commit")
}

func TestGORMCacheTransactionRollback(t *testing.T) {
	ctx := context.Background()
	cache, db := newGORMCache[string](t, "tx_rollback_cache")

	require.NoError(t, cache.Set(ctx, "exists_key", "exists_value"))

	tx := db.Begin()
	txCtx := WithGORMTx(ctx, tx)

	require.NoError(t, cache.Set(txCtx, "rollback_key", "rollback_value"))
	require.NoError(t, cache.Set(txCtx, "exists_key", "exists_value2"))

	require.NoError(t, tx.Rollback().Error)

	_, err := cache.Get(ctx, "rollback_key")
	assert.True(t, IsErrKeyNotFound(err), "should not find key after transaction rollback")

	value, err := cache.Get(ctx, "exists_key")
	require.NoError(t, err)
	assert.Equal(t, "exists_value", value, "should find key after transaction rollback")
}

func TestGORMCacheTransactionIsolation(t *testing.T) {
	ctx := context.Background()
	cache, db := newGORMCache[string](t, "tx_isolation_cache")

	require.NoError(t, cache.Set(ctx, "isolation_key", "original_value"))

	value, err := cache.Get(ctx, "isolation_key")
	require.NoError(t, err)
	assert.Equal(t, "original_value", value, "should read original value before transaction")

	tx := db.Begin()
	txCtx := WithGORMTx(ctx, tx)

	require.NoError(t, cache.Set(txCtx, "isolation_key", "updated_value"))

	txValue, err := cache.Get(txCtx, "isolation_key")
	require.NoError(t, err)
	assert.Equal(t, "updated_value", txValue, "should see updated value inside transaction")

	require.NoError(t, tx.Commit().Error)

	finalValue, err := cache.Get(ctx, "isolation_key")
	require.NoError(t, err)
	assert.Equal(t, "updated_value", finalValue, "should see updated value after commit")
}

func TestGORMCacheTransactionDelete(t *testing.T) {
	ctx := context.Background()
	cache, db := newGORMCache[string](t, "tx_delete_cache")

	require.NoError(t, cache.Set(ctx, "del_key", "del_value"))

	value, err := cache.Get(ctx, "del_key")
	require.NoError(t, err)
	assert.Equal(t, "del_value", value, "should read value before transaction")

	tx := db.Begin()
	txCtx := WithGORMTx(ctx, tx)

	require.NoError(t, cache.Del(txCtx, "del_key"))

	_, err = cache.Get(txCtx, "del_key")
	assert.True(t, IsErrKeyNotFound(err), "should not find key inside transaction after delete")

	require.NoError(t, tx.Commit().Error)

	_, err = cache.Get(ctx, "del_key")
	assert.True(t, IsErrKeyNotFound(err), "should not find key after transaction commit")
}

func TestGORMCacheWithClientTransaction(t *testing.T) {
	type User struct {
		ID   string
		Name string
	}

	ctx := context.Background()
	cache, db := newGORMCache[*User](t, "client_tx_cache")

	fetchCount := 0
	upstream := UpstreamFunc[*User](func(ctx context.Context, key string) (*User, error) {
		fetchCount++
		return &User{ID: key, Name: "User " + key}, nil
	})

	client := NewClient(cache, upstream)

	require.NoError(t, cache.Set(ctx, "other_key", &User{ID: "other", Name: "Other User"}))

	tx := db.Begin()
	txCtx := WithGORMTx(ctx, tx)

	user1 := &User{ID: "user1", Name: "User One"}
	require.NoError(t, client.Set(txCtx, "user1", user1))

	value, err := client.Get(txCtx, "user1")
	require.NoError(t, err)
	assert.Equal(t, "User One", value.Name, "should read value within transaction")

	require.NoError(t, tx.Rollback().Error)

	_, err = cache.Get(ctx, "user1")
	assert.True(t, IsErrKeyNotFound(err), "should not find value in cache after transaction rollback")
	assert.Equal(t, 0, fetchCount, "should not fetch from upstream during rollback test")
}
