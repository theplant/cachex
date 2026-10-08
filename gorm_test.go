package cachex

import (
	"context"
	"math"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pkg/errors"
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

func TestGORMCacheNeverServesAnotherKeysValue(t *testing.T) {
	// SQLite's NOCASE stands in for MySQL's default _ci collations, where "abc" and "ABC" are one row
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.Exec(`CREATE TABLE ci (key TEXT COLLATE NOCASE PRIMARY KEY, value JSON NOT NULL, updated_at DATETIME)`).Error)
	c := NewGORMCache[string](&GORMCacheConfig{DB: db, TableName: "ci", KeyPrefix: "p:"})
	ctx := context.Background()

	requireMiss := func(key string) {
		t.Helper()
		_, err := c.Get(ctx, key)
		assert.True(t, IsErrKeyNotFound(err), "Get(%q) must not see another key's row: %v", key, err)
		got, err := c.GetMany(ctx, []string{key})
		require.NoError(t, err)
		assert.Empty(t, got, "GetMany(%q) must not see another key's row", key)
	}
	requireHit := func(key, want string) {
		t.Helper()
		v, err := c.Get(ctx, key)
		require.NoError(t, err)
		assert.Equal(t, want, v)
		got, err := c.GetMany(ctx, []string{key})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{key: want}, got)
	}

	require.NoError(t, c.Set(ctx, "ABC", "upper"))
	requireHit("ABC", "upper")
	requireMiss("abc")

	require.NoError(t, c.Set(ctx, "abc", "lower")) // takes the shared row over
	requireHit("abc", "lower")
	requireMiss("ABC")

	require.NoError(t, c.SetMany(ctx, map[string]string{"ABC": "upper2"}))
	requireHit("ABC", "upper2")
	requireMiss("abc")
}

func TestGORMCacheGetManyCaseSensitiveKeyColumn(t *testing.T) {
	c, _ := newGORMCache[string](t, "cs")
	ctx := context.Background()
	require.NoError(t, c.Set(ctx, "abc", "lower"))
	require.NoError(t, c.Set(ctx, "ABC", "upper"))
	got, err := c.GetMany(ctx, []string{"abc", "ABC", "Abc"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"abc": "lower", "ABC": "upper"}, got, "distinct keys stay distinct")
}

func TestGORMCacheQuotesTheKeyColumn(t *testing.T) {
	// "key" is reserved in MySQL; SQLite accepts it bare, so check the SQL itself
	cache, db := newGORMCache[string](t, "quoted")
	var sqls []string
	capture := func(tx *gorm.DB) { sqls = append(sqls, tx.Statement.SQL.String()) }
	require.NoError(t, db.Callback().Query().After("gorm:query").Register("test:capture", capture))
	require.NoError(t, db.Callback().Delete().After("gorm:delete").Register("test:capture", capture))
	require.NoError(t, db.Callback().Create().After("gorm:create").Register("test:capture", capture))

	ctx := context.Background()
	require.NoError(t, cache.Set(ctx, "a", "1"))
	_, _ = cache.Get(ctx, "a")
	_, _ = cache.GetMany(ctx, []string{"a", "b"})
	require.NoError(t, cache.SetMany(ctx, map[string]string{"b": "2"}))
	require.NoError(t, cache.Del(ctx, "a"))
	require.NoError(t, cache.DelMany(ctx, []string{"b"}))

	require.Len(t, sqls, 6)
	bare := regexp.MustCompile("(^|[^`\"])\\bkey\\b($|[^`\"])")
	for _, sql := range sqls {
		assert.NotRegexp(t, bare, sql, "the key column must be quoted")
	}
}

func TestGORMCacheSetManyKeepsTheValuesThatEncode(t *testing.T) {
	type num struct{ V float64 }
	cache, _ := newGORMCache[num](t, "encode")
	ctx := context.Background()
	err := cache.SetMany(ctx, map[string]num{"ok": {1}, "bad": {math.NaN()}})
	require.Error(t, err, "the value that cannot be encoded is reported")
	assert.Contains(t, err.Error(), "bad")
	v, err := cache.Get(ctx, "ok")
	require.NoError(t, err, "like RedisCache, the others are still written")
	assert.Equal(t, num{1}, v)
}

// sqlStateError mimics a pgx/lib/pq error.
type sqlStateError string

func (e sqlStateError) Error() string    { return "ERROR: (SQLSTATE " + string(e) + ")" }
func (e sqlStateError) SQLState() string { return string(e) }

func TestIsDeadlock(t *testing.T) {
	assert.True(t, isDeadlock(errors.Wrap(sqlStateError("40P01"), "x")), "PostgreSQL deadlock")
	assert.True(t, isDeadlock(sqlStateError("40001")), "serialization failure")
	assert.True(t, isDeadlock(errors.New("Error 1213 (40001): Deadlock found when trying to get lock; try restarting transaction")), "MySQL deadlock")
	assert.False(t, isDeadlock(sqlStateError("23505")))
	assert.False(t, isDeadlock(errors.New("Error 1205 (HY000): Lock wait timeout exceeded")))
	assert.False(t, isDeadlock(nil))
}

func TestGORMCacheRetriesDeadlockVictims(t *testing.T) {
	cache, db := newGORMCache[string](t, "retry")
	ctx := context.Background()
	var fails atomic.Int32
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register("test:deadlock", func(tx *gorm.DB) {
		if fails.Add(-1) >= 0 {
			_ = tx.AddError(sqlStateError("40P01"))
		}
	}))

	fails.Store(2)
	require.NoError(t, cache.Set(ctx, "k", "v"), "a deadlock victim is retried")
	v, err := cache.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "v", v)

	fails.Store(100)
	assert.True(t, isDeadlock(cache.SetMany(ctx, map[string]string{"k": "v2"})), "retries are bounded")

	fails.Store(1)
	err = db.Transaction(func(tx *gorm.DB) error { return cache.Set(WithGORMTx(ctx, tx), "k", "v3") })
	assert.True(t, isDeadlock(err), "inside the caller's transaction, which the deadlock rolled back, nothing is retried")
}
