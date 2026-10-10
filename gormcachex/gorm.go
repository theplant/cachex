// Package gormcachex is a cachex backend on a database table through GORM:
// one row per key, the entry encoded in the value column. Tested on MySQL
// 8.0.17+, PostgreSQL and SQLite, at each database's default isolation level.
//
// Batch writes lock rows in one order (keys sorted by bytes) so that
// concurrent writers do not deadlock, and the few deadlocks MySQL's gap locks
// still cause are retried. Expired rows are not removed: delete rows whose
// expires_at has passed from time to time.
package gormcachex

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/theplant/cachex/v2"
)

// DefaultChunkSize is how many keys one statement carries by default.
const DefaultChunkSize = 1000

// maxChunkSize keeps every statement under the databases' bound parameter
// limits: an upsert binds 4 parameters per row, and SQLite allows 32766.
const maxChunkSize = 8000

// deadlockAttempts is how many times a write chosen as a deadlock victim is
// run in all. Writes take row locks in one order (sorted keys), but MySQL's gap
// locks can still deadlock now and then, and InnoDB expects a retry.
const deadlockAttempts = 5

// Config configures a Backend.
type Config[T any] struct {
	// DB is the database. Required.
	DB *gorm.DB
	// TableName is the cache table. Required.
	TableName string
	// KeyPrefix is prepended to every key, so that one table can hold several
	// caches. Change it when the value type changes incompatibly (see
	// rediscachex.Config.KeyPrefix).
	KeyPrefix string
	// ChunkSize is how many keys one statement carries; a larger call runs
	// several statements, one after another, each on its own. Default
	// DefaultChunkSize, at most 8000.
	ChunkSize int
	// Codec encodes values. Default cachex.DefaultCodec.
	Codec cachex.Codec[T]
	// Logger reports dropped entries that do not decode. Default slog.Default().
	Logger *slog.Logger
}

// Backend is a GORM-backed cachex.Backend.
type Backend[T any] struct {
	db        *gorm.DB
	table     string
	prefix    string
	chunkSize int
	codec     cachex.Codec[T]
	logger    *slog.Logger
}

var _ cachex.Backend[any] = (*Backend[any])(nil)

type row struct {
	Key       string    `gorm:"not null;primaryKey;size:255"`
	Value     []byte    `gorm:"not null"`
	ExpiresAt time.Time `gorm:"not null;index"`
	UpdatedAt time.Time `gorm:"not null"`
}

// New returns a Backend on cfg.TableName; call Migrate to create the table.
func New[T any](cfg Config[T]) *Backend[T] {
	if cfg.DB == nil {
		panic("gormcachex: DB is required")
	}
	if cfg.TableName == "" {
		panic("gormcachex: TableName is required")
	}
	b := &Backend[T]{db: cfg.DB, table: cfg.TableName, prefix: cfg.KeyPrefix, codec: cfg.Codec, logger: cfg.Logger}
	b.chunkSize = min(cmp.Or(max(cfg.ChunkSize, 0), DefaultChunkSize), maxChunkSize)
	if b.codec == nil {
		b.codec = cachex.DefaultCodec[T]()
	}
	if b.logger == nil {
		b.logger = slog.Default()
	}
	return b
}

type txKey struct{}

// WithTx makes the backend's reads and writes with ctx run in tx (which the
// caller commits or rolls back), except those of a fetch shared by several
// callers (see cachex.IsShared), which never join one caller's transaction.
func WithTx(ctx context.Context, tx *gorm.DB) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

// TxFrom returns the transaction WithTx put in ctx, or nil.
func TxFrom(ctx context.Context) *gorm.DB {
	tx, _ := ctx.Value(txKey{}).(*gorm.DB)
	return tx
}

// conn is where ctx's statements run: its transaction, if it may join it.
func (b *Backend[T]) conn(ctx context.Context) (db *gorm.DB, inTx bool) {
	if tx := TxFrom(ctx); tx != nil && !cachex.IsShared(ctx) {
		return tx.WithContext(ctx), true
	}
	return b.db.WithContext(ctx), false
}

// keyColumn is the key column as a clause, so GORM quotes it: "key" is a
// reserved word in MySQL.
var keyColumn = clause.Column{Name: "key"}

var upsert = clause.OnConflict{
	Columns:   []clause.Column{keyColumn},
	DoUpdates: clause.AssignmentColumns([]string{"key", "value", "expires_at", "updated_at"}), // the key too: on a key column that is not byte-exact, a shared row belongs to the key written last
}

func anys(keys []string) []any {
	out := make([]any, len(keys))
	for i, key := range keys {
		out[i] = key
	}
	return out
}

// Migrate creates the table, or checks an existing one. On MySQL the key
// column must compare keys exactly (case, trailing spaces): Migrate creates
// tables with utf8mb4_0900_bin, which needs MySQL 8.0.17 or later, and rejects
// an existing table whose key column uses another collation, with the
// statement that converts it.
func (b *Backend[T]) Migrate(ctx context.Context) error {
	db, _ := b.conn(ctx)
	db = db.Table(b.table)
	if db.Name() == "mysql" {
		if db.Migrator().HasTable(b.table) {
			if err := b.checkMySQLKeyCollation(db); err != nil {
				return err
			}
		} else {
			var version string
			if err := db.Raw("SELECT VERSION()").Scan(&version).Error; err != nil {
				return fmt.Errorf("gormcachex: read the MySQL version: %w", err)
			}
			if err := checkMySQLVersion(version); err != nil {
				return err
			}
		}
		db = db.Set("gorm:table_options", "CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin")
	}
	if err := db.AutoMigrate(&row{}); err != nil {
		return fmt.Errorf("gormcachex: migrate table %s: %w", b.table, err)
	}
	return nil
}

// checkMySQLVersion fails unless version (SELECT VERSION()) is MySQL 8.0.17
// or later, the first with utf8mb4_0900_bin. MariaDB has no such collation.
func checkMySQLVersion(version string) error {
	fail := fmt.Errorf("gormcachex: needs MySQL 8.0.17 or later to create its table (utf8mb4_0900_bin collation), found %q", version)
	if strings.Contains(version, "MariaDB") {
		return fail
	}
	var major, minor, patch int
	if _, err := fmt.Sscanf(version, "%d.%d.%d", &major, &minor, &patch); err != nil {
		return fail
	}
	if cmp.Or(cmp.Compare(major, 8), cmp.Compare(minor, 0), cmp.Compare(patch, 17)) < 0 {
		return fail
	}
	return nil
}

func (b *Backend[T]) checkMySQLKeyCollation(db *gorm.DB) error {
	var collation sql.NullString
	if err := db.Raw("SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?",
		b.table, "key").Scan(&collation).Error; err != nil {
		return fmt.Errorf("gormcachex: read the key column's collation of table %s: %w", b.table, err)
	}
	if collation.String != "utf8mb4_0900_bin" {
		return fmt.Errorf("gormcachex: the key column of table %s must use utf8mb4_0900_bin, found %q; convert it with: ALTER TABLE %s CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin",
			b.table, collation.String, b.table)
	}
	return nil
}

// write runs a statement, retrying it if the database aborts it as a deadlock
// victim, unless it runs in the caller's transaction, which the deadlock has
// already rolled back as a whole.
func (b *Backend[T]) write(ctx context.Context, f func(db *gorm.DB) error) error {
	db, inTx := b.conn(ctx)
	if inTx {
		return f(db)
	}
	for attempt := 1; ; attempt++ {
		err := f(db)
		if !isDeadlock(err) || attempt == deadlockAttempts {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(1+rand.IntN(5*attempt)) * time.Millisecond):
		}
	}
}

// isDeadlock reports whether err aborted a statement as a deadlock victim:
// SQLSTATE 40P01/40001 from PostgreSQL drivers (pgx, lib/pq), or MySQL error
// 1213, matched by text since go-sql-driver's error has no SQLState method.
func isDeadlock(err error) bool {
	if err == nil {
		return false
	}
	var pg interface{ SQLState() string }
	if errors.As(err, &pg) {
		return pg.SQLState() == "40P01" || pg.SQLState() == "40001"
	}
	return strings.Contains(err.Error(), "Error 1213 (40001)")
}

func (b *Backend[T]) Get(ctx context.Context, key string) (cachex.Entry[T], bool, error) {
	m, err := b.GetMany(ctx, []string{key})
	if err != nil {
		return cachex.Entry[T]{}, false, errOf(err, key)
	}
	e, ok := m[key]
	return e, ok, nil
}

// GetMany reads keys with `WHERE key IN (...)` queries of up to ChunkSize keys.
// Only the row stored under exactly a requested key is returned, even if the
// key column compares keys loosely.
func (b *Backend[T]) GetMany(ctx context.Context, keys []string) (map[string]cachex.Entry[T], error) {
	out := make(map[string]cachex.Entry[T], len(keys))
	errs := map[string]error{}
	db, _ := b.conn(ctx)
	var bad []string
	for chunk := range slices.Chunk(keys, b.chunkSize) {
		prefixed := make([]string, len(chunk))
		for i, key := range chunk {
			prefixed[i] = b.prefix + key
		}
		var rows []row
		if err := db.Table(b.table).Select("?, ?", keyColumn, clause.Column{Name: "value"}).
			Where(clause.IN{Column: keyColumn, Values: anys(prefixed)}).Find(&rows).Error; err != nil {
			for _, key := range chunk {
				errs[key] = fmt.Errorf("gormcachex: get %q: %w", key, err)
			}
			continue
		}
		byKey := make(map[string][]byte, len(rows))
		for _, r := range rows {
			byKey[r.Key] = r.Value
		}
		for i, key := range chunk {
			data, ok := byKey[prefixed[i]]
			if !ok {
				continue
			}
			e, err := cachex.DecodeEntry(b.codec, data)
			if err != nil {
				b.logger.WarnContext(ctx, "gormcachex: dropped an entry that does not decode", "key", key, "error", err)
				bad = append(bad, key)
				continue
			}
			out[key] = e
		}
	}
	if len(bad) > 0 {
		_ = b.DelMany(ctx, bad)
	}
	return out, batchError(errs)
}

func (b *Backend[T]) Set(ctx context.Context, key string, e cachex.Entry[T]) error {
	return errOf(b.SetMany(ctx, map[string]cachex.Entry[T]{key: e}), key)
}

// SetMany upserts entries with multi-row statements of up to ChunkSize rows,
// in key order; an entry already expired deletes its key instead.
func (b *Backend[T]) SetMany(ctx context.Context, entries map[string]cachex.Entry[T]) error {
	errs := map[string]error{}
	now := time.Now()
	type pending struct {
		key string
		row row
	}
	var rows []pending
	var expired []string
	for key, e := range entries {
		if !e.ExpiresAt.After(now) {
			expired = append(expired, key)
			continue
		}
		data, err := cachex.EncodeEntry(b.codec, e)
		if err != nil {
			errs[key] = err
			continue
		}
		rows = append(rows, pending{key, row{Key: b.prefix + key, Value: data, ExpiresAt: e.ExpiresAt, UpdatedAt: now}})
	}
	// one row order for every writer, or overlapping batches lock rows in
	// opposite orders and deadlock
	slices.SortFunc(rows, func(a, b pending) int { return strings.Compare(a.row.Key, b.row.Key) })
	for chunk := range slices.Chunk(rows, b.chunkSize) {
		batch := make([]row, len(chunk))
		for i, p := range chunk {
			batch[i] = p.row
		}
		if err := b.write(ctx, func(db *gorm.DB) error {
			return db.Table(b.table).Clauses(upsert).Create(&batch).Error
		}); err != nil {
			for _, p := range chunk {
				errs[p.key] = fmt.Errorf("gormcachex: set %q: %w", p.key, err)
			}
		}
	}
	if err := b.DelMany(ctx, expired); err != nil {
		for _, key := range expired {
			if kerr := errOf(err, key); kerr != nil {
				errs[key] = kerr
			}
		}
	}
	return batchError(errs)
}

func (b *Backend[T]) Del(ctx context.Context, key string) error {
	return errOf(b.DelMany(ctx, []string{key}), key)
}

// DelMany deletes keys with `WHERE key IN (...)` statements of up to
// ChunkSize keys, locking rows in key order.
func (b *Backend[T]) DelMany(ctx context.Context, keys []string) error {
	keys = slices.Sorted(slices.Values(keys))
	errs := map[string]error{}
	for chunk := range slices.Chunk(keys, b.chunkSize) {
		prefixed := make([]string, len(chunk))
		for i, key := range chunk {
			prefixed[i] = b.prefix + key
		}
		if err := b.write(ctx, func(db *gorm.DB) error {
			in := clause.IN{Column: keyColumn, Values: anys(prefixed)}
			if db.Name() != "postgres" {
				return db.Table(b.table).Where(in).Delete(nil).Error
			}
			// DELETE locks rows in scan order, which follows the column's
			// collation (often en_US), not SetMany's byte order: lock them in
			// byte order first, or the two deadlock each other
			locked := db.Session(&gorm.Session{NewDB: true}).Table(b.table).
				Select("?", keyColumn).Where(in).
				Order(clause.OrderBy{Expression: clause.Expr{SQL: `? COLLATE "C"`, Vars: []any{keyColumn}, WithoutParentheses: true}}).
				Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
			return db.Table(b.table).Where("? IN (?)", keyColumn, locked).Delete(nil).Error
		}); err != nil {
			for _, key := range chunk {
				errs[key] = fmt.Errorf("gormcachex: delete %q: %w", key, err)
			}
		}
	}
	return batchError(errs)
}

func batchError(errs map[string]error) error {
	if len(errs) == 0 {
		return nil
	}
	return &cachex.BatchError{Errors: errs}
}

// errOf is the error a batch call reported for key: its own entry if err is
// a *cachex.BatchError, otherwise err itself.
func errOf(err error, key string) error {
	if be, ok := err.(*cachex.BatchError); ok { //nolint:errorlint // returned unwrapped above
		return be.Errors[key]
	}
	return err
}
