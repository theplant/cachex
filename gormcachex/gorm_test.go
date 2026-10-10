package gormcachex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

func newSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1) // one in-memory database
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func newBackend(t *testing.T, cfg Config[string]) *Backend[string] {
	t.Helper()
	if cfg.DB == nil {
		cfg.DB = newSQLite(t)
	}
	if cfg.TableName == "" {
		cfg.TableName = "cache"
	}
	b := New[string](cfg)
	require.NoError(t, b.Migrate(context.Background()))
	return b
}

func entry(v string) cachex.Entry[string] {
	return cachex.Entry[string]{Value: v, ExpiresAt: time.Now().Add(time.Hour)}
}

func TestContract(t *testing.T) {
	cachextest.TestBackend(t, func(t *testing.T) cachex.Backend[string] {
		return newBackend(t, Config[string]{KeyPrefix: "p:"})
	})
	t.Run("in chunks", func(t *testing.T) {
		cachextest.TestBackend(t, func(t *testing.T) cachex.Backend[string] {
			return newBackend(t, Config[string]{ChunkSize: 2})
		})
	})
}

func TestTransactions(t *testing.T) {
	ctx := context.Background()

	t.Run("writes run in the caller's transaction", func(t *testing.T) {
		db := newSQLite(t)
		b := newBackend(t, Config[string]{DB: db})
		tx := db.Begin()
		require.NoError(t, b.Set(WithTx(ctx, tx), "k", entry("v")))
		_, ok, err := b.Get(WithTx(ctx, tx), "k")
		require.NoError(t, err)
		assert.True(t, ok)
		require.NoError(t, tx.Rollback().Error)
		_, ok, err = b.Get(ctx, "k")
		require.NoError(t, err)
		assert.False(t, ok, "rolled back with it")
	})

	t.Run("a shared fetch does not use the caller's transaction", func(t *testing.T) {
		db := newSQLite(t)
		b := newBackend(t, Config[string]{DB: db})
		tx := db.Begin()
		defer tx.Rollback()
		var sharedCtx context.Context
		c := cachex.New(cachex.SourceFunc[string](func(ctx context.Context, _ string) (string, error) {
			sharedCtx = ctx
			return "v", nil
		}), nil)
		_, err := c.Get(WithTx(ctx, tx), "k")
		require.NoError(t, err)
		require.True(t, cachex.IsShared(sharedCtx), "the Cache marks a fetch's ctx")
		_, inTx := b.conn(sharedCtx)
		assert.False(t, inTx, "and the backend does not join the caller's transaction with it")
		_, inTx = b.conn(WithTx(ctx, tx))
		assert.True(t, inTx)
	})
}

func TestTheKeyColumnIsQuoted(t *testing.T) {
	// "key" is reserved in MySQL; SQLite accepts it bare, so check the SQL itself
	db := newSQLite(t)
	b := newBackend(t, Config[string]{DB: db})
	var sqls []string
	capture := func(tx *gorm.DB) { sqls = append(sqls, tx.Statement.SQL.String()) }
	require.NoError(t, db.Callback().Query().After("gorm:query").Register("test:capture", capture))
	require.NoError(t, db.Callback().Delete().After("gorm:delete").Register("test:capture", capture))
	require.NoError(t, db.Callback().Create().After("gorm:create").Register("test:capture", capture))

	ctx := context.Background()
	require.NoError(t, b.Set(ctx, "a", entry("1")))
	_, _, _ = b.Get(ctx, "a")
	_, _ = b.GetMany(ctx, []string{"a", "b"})
	require.NoError(t, b.SetMany(ctx, map[string]cachex.Entry[string]{"b": entry("2")}))
	require.NoError(t, b.Del(ctx, "a"))
	require.NoError(t, b.DelMany(ctx, []string{"b"}))

	require.Len(t, sqls, 6)
	bare := regexp.MustCompile("(^|[^`\"])\\bkey\\b($|[^`\"])")
	for _, sql := range sqls {
		assert.NotRegexp(t, bare, sql, "the key column must be quoted")
	}
}

func TestOnlyTheRowOfExactlyTheKeyIsServed(t *testing.T) {
	ctx := context.Background()
	db := newSQLite(t)
	require.NoError(t, db.Exec("CREATE TABLE nocase (`key` TEXT COLLATE NOCASE PRIMARY KEY, `value` BLOB NOT NULL, `expires_at` DATETIME NOT NULL, `updated_at` DATETIME NOT NULL)").Error)
	b := New[string](Config[string]{DB: db, TableName: "nocase"})
	require.NoError(t, b.Set(ctx, "ABC", entry("upper")))
	_, ok, err := b.Get(ctx, "abc")
	require.NoError(t, err)
	assert.False(t, ok, "a key column that is not byte-exact matched ABC's row")
	m, err := b.GetMany(ctx, []string{"abc"})
	require.NoError(t, err)
	assert.Empty(t, m)
}

func TestAnExpiredEntryIsNotStored(t *testing.T) {
	ctx := context.Background()
	b := newBackend(t, Config[string]{})
	require.NoError(t, b.Set(ctx, "k", entry("v")))
	require.NoError(t, b.Set(ctx, "k", cachex.Entry[string]{Value: "v", ExpiresAt: time.Now().Add(-time.Second)}))
	_, ok, err := b.Get(ctx, "k")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestAnEntryThatDoesNotDecodeIsAMiss(t *testing.T) {
	ctx := context.Background()
	db := newSQLite(t)
	var log bytes.Buffer
	b := newBackend(t, Config[string]{DB: db, Logger: slog.New(slog.NewTextHandler(&log, nil))})
	require.NoError(t, db.Exec("INSERT INTO cache (`key`, `value`, `expires_at`, `updated_at`) VALUES ('k', X'7b7d', ?, ?)", time.Now().Add(time.Hour), time.Now()).Error)
	_, ok, err := b.Get(ctx, "k")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Contains(t, log.String(), "dropped an entry that does not decode")
	var n int64
	require.NoError(t, db.Table("cache").Count(&n).Error)
	assert.Zero(t, n, "and dropped it")
}

// sqlStateError mimics a pgx/lib/pq error.
type sqlStateError string

func (e sqlStateError) Error() string    { return "ERROR: (SQLSTATE " + string(e) + ")" }
func (e sqlStateError) SQLState() string { return string(e) }

func TestIsDeadlock(t *testing.T) {
	assert.True(t, isDeadlock(fmt.Errorf("x: %w", sqlStateError("40P01"))), "PostgreSQL deadlock")
	assert.True(t, isDeadlock(sqlStateError("40001")), "serialization failure")
	assert.True(t, isDeadlock(errors.New("Error 1213 (40001): Deadlock found when trying to get lock; try restarting transaction")), "MySQL deadlock")
	assert.False(t, isDeadlock(sqlStateError("23505")))
	assert.False(t, isDeadlock(errors.New("Error 1205 (HY000): Lock wait timeout exceeded")))
	assert.False(t, isDeadlock(nil))
}

func TestDeadlockVictimsAreRetried(t *testing.T) {
	ctx := context.Background()
	db := newSQLite(t)
	b := newBackend(t, Config[string]{DB: db})
	var fails atomic.Int32
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register("test:deadlock", func(tx *gorm.DB) {
		if fails.Add(-1) >= 0 {
			_ = tx.AddError(sqlStateError("40P01"))
		}
	}))

	fails.Store(2)
	require.NoError(t, b.Set(ctx, "k", entry("v")), "a deadlock victim is retried")
	_, ok, err := b.Get(ctx, "k")
	require.NoError(t, err)
	assert.True(t, ok)

	fails.Store(100)
	err = b.SetMany(ctx, map[string]cachex.Entry[string]{"k": entry("v2")})
	var be *cachex.BatchError
	require.ErrorAs(t, err, &be)
	assert.True(t, isDeadlock(be.Errors["k"]), "retries are bounded")

	fails.Store(1)
	err = db.Transaction(func(tx *gorm.DB) error { return b.Set(WithTx(ctx, tx), "k", entry("v3")) })
	assert.True(t, isDeadlock(err), "inside the caller's transaction, which the deadlock rolled back, nothing is retried")
}

func TestCheckMySQLVersion(t *testing.T) {
	for version, ok := range map[string]bool{
		"8.0.17":                  true,
		"8.0.36-0ubuntu0.22.04.1": true,
		"8.4.2":                   true,
		"9.1.0":                   true,
		"8.0.16":                  false,
		"5.7.44-log":              false,
		"10.11.6-MariaDB":         false,
		"11.4.2-MariaDB-ubu2404":  false,
		"garbage":                 false,
	} {
		err := checkMySQLVersion(version)
		if ok {
			assert.NoError(t, err, version)
		} else {
			assert.ErrorContains(t, err, "MySQL 8.0.17 or later", version)
		}
	}
}

func TestNewRejectsAMissingDBOrTable(t *testing.T) {
	assert.Panics(t, func() { New[string](Config[string]{TableName: "t"}) })
	assert.Panics(t, func() { New[string](Config[string]{DB: &gorm.DB{}}) })
}
