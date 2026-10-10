package cachex

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/pkg/errors"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// gormDeadlockRetries is how many times a write chosen as a deadlock victim is
// run in all. Writes take row locks in one order (sorted keys), but MySQL's gap
// locks can still deadlock now and then, and InnoDB expects a retry.
const gormDeadlockRetries = 5

// write runs a write to the cache table, retrying it if the database aborts
// it as a deadlock victim, unless it runs in the caller's transaction, which
// the deadlock has already rolled back as a whole.
func (g *GORMCache[T]) write(ctx context.Context, f func(tx *gorm.DB) error) error {
	if tx := GetGORMTx(ctx); tx != nil {
		return f(tx.WithContext(ctx))
	}
	for attempt := 1; ; attempt++ {
		err := f(g.db.WithContext(ctx))
		if !isDeadlock(err) || attempt == gormDeadlockRetries {
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
	if stderrors.As(err, &pg) {
		return pg.SQLState() == "40P01" || pg.SQLState() == "40001"
	}
	return strings.Contains(err.Error(), "Error 1213 (40001)")
}

// checkMySQLVersion fails unless version (SELECT VERSION()) is MySQL 8.0.17
// or later, the first with the utf8mb4_0900_bin collation Migrate creates
// tables with. MariaDB has no such collation.
func checkMySQLVersion(version string) error {
	fail := errors.Errorf("GORMCache needs MySQL 8.0.17 or later to create its table (utf8mb4_0900_bin collation), found %q", version)
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

// keyColumn is the key column as a clause, so GORM quotes it: "key" is a
// reserved word in MySQL.
var keyColumn = clause.Column{Name: "key"}

func anys(keys []string) []any {
	out := make([]any, len(keys))
	for i, key := range keys {
		out[i] = key
	}
	return out
}

// upsertEntry also rewrites the key column, so on a key column that is not
// byte-exact the row belongs to the key written last, never to another one.
var upsertEntry = clause.OnConflict{
	Columns:   []clause.Column{keyColumn},
	DoUpdates: clause.AssignmentColumns([]string{"key", "value", "updated_at"}),
}

// GORMCache is a cache implementation using GORM
type GORMCache[T any] struct {
	db        *gorm.DB
	tableName string
	keyPrefix string
	chunkSize int
}

var _ BatchCache[any] = &GORMCache[any]{}

type cacheEntry struct {
	Key       string         `gorm:"not null;primaryKey;size:255"`
	Value     datatypes.JSON `gorm:"not null;type:json"`
	UpdatedAt time.Time      `gorm:"not null;index"`
}

// GORMCacheConfig holds configuration for GORMCache. Keys are case-sensitive:
// the key column should compare them exactly (Migrate creates MySQL tables with
// utf8mb4_0900_bin, so MySQL 8.0.17 or later is required). Otherwise keys that the column considers equal share one row,
// served only to the key that wrote it last.
type GORMCacheConfig struct {
	// DB is the GORM database connection
	DB *gorm.DB

	// TableName is the name of the cache table
	TableName string

	// KeyPrefix is the prefix for all keys (optional)
	KeyPrefix string

	// ChunkSize is how many keys one statement of GetMany, SetMany or DelMany
	// carries; a larger call runs several statements, one after another. Zero
	// means DefaultChunkSize; values above gormMaxChunkSize are capped to it.
	ChunkSize int
}

// NewGORMCache creates a new GORM-based cache with configuration
func NewGORMCache[T any](config *GORMCacheConfig) *GORMCache[T] {
	if config.DB == nil {
		panic("DB is required")
	}
	if config.TableName == "" {
		panic("TableName is required")
	}

	return &GORMCache[T]{
		db:        config.DB,
		tableName: config.TableName,
		keyPrefix: config.KeyPrefix,
		chunkSize: min(chunkSizeOr(config.ChunkSize, DefaultChunkSize), gormMaxChunkSize),
	}
}

func (g *GORMCache[T]) prefixedKey(key string) string {
	return g.keyPrefix + key
}

// Migrate creates or updates the cache table schema
func (g *GORMCache[T]) Migrate(ctx context.Context) error {
	tx := cmp.Or(GetGORMTx(ctx), g.db).WithContext(ctx).Table(g.tableName)
	if tx.Name() == "mysql" {
		if tx.Migrator().HasTable(g.tableName) {
			if err := g.checkMySQLKeyCollation(tx); err != nil {
				return err
			}
		} else {
			var version string
			if err := tx.Raw("SELECT VERSION()").Scan(&version).Error; err != nil {
				return errors.Wrap(err, "failed to read the MySQL version")
			}
			if err := checkMySQLVersion(version); err != nil {
				return err
			}
		}
		// keys compare exactly (case, trailing spaces), MySQL's defaults do not
		tx = tx.Set("gorm:table_options", "CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin") // MySQL 8.0.17+
	}
	if err := tx.AutoMigrate(&cacheEntry{}); err != nil {
		return errors.Wrapf(err, "failed to migrate cache table for table: %s", g.tableName)
	}
	return nil
}

// checkMySQLKeyCollation fails unless the existing table's key column uses
// utf8mb4_0900_bin: under any other collation keys differing in case or
// trailing spaces share a row, and rows are not locked in byte order.
func (g *GORMCache[T]) checkMySQLKeyCollation(tx *gorm.DB) error {
	var collation sql.NullString
	if err := tx.Raw("SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?",
		g.tableName, "key").Scan(&collation).Error; err != nil {
		return errors.Wrapf(err, "failed to read the key column's collation for table: %s", g.tableName)
	}
	if collation.String != "utf8mb4_0900_bin" {
		return errors.Errorf("GORMCache needs the key column of table %s to use utf8mb4_0900_bin, found %q; convert it with: ALTER TABLE %s CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin",
			g.tableName, collation.String, g.tableName)
	}
	return nil
}

type ctxKeyGORMTx struct{}

// WithGORMTx attaches a GORM transaction to the context.
// All GORMCache operations using this context will execute within the transaction.
// The transaction must be committed or rolled back by the caller.
func WithGORMTx(ctx context.Context, tx *gorm.DB) context.Context {
	return context.WithValue(ctx, ctxKeyGORMTx{}, tx)
}

// withoutGORMTx hides ctx's GORM transaction: a fetch shared by every waiter
// must not write inside one caller's transaction.
func withoutGORMTx(ctx context.Context) context.Context {
	if GetGORMTx(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyGORMTx{}, (*gorm.DB)(nil))
}

// GetGORMTx retrieves the GORM transaction from the context.
// Returns nil if no transaction is attached to the context.
func GetGORMTx(ctx context.Context) *gorm.DB {
	tx, _ := ctx.Value(ctxKeyGORMTx{}).(*gorm.DB)
	return tx
}

// Set stores a value in the cache
func (g *GORMCache[T]) Set(ctx context.Context, key string, value T) error {
	data, err := json.Marshal(value)
	if err != nil {
		return errors.Wrapf(err, "failed to marshal value for key: %s", key)
	}

	entry := cacheEntry{
		Key:   g.prefixedKey(key),
		Value: data,
	}

	if err := g.write(ctx, func(tx *gorm.DB) error {
		return tx.Table(g.tableName).Clauses(upsertEntry).Create(&entry).Error
	}); err != nil {
		return errors.Wrapf(err, "failed to set cache entry for key: %s", key)
	}

	return nil
}

// Get retrieves a value from the cache
func (g *GORMCache[T]) Get(ctx context.Context, key string) (T, error) {
	var zero T
	var entry cacheEntry

	tx := cmp.Or(GetGORMTx(ctx), g.db)
	if err := tx.WithContext(ctx).
		Table(g.tableName).
		Where(clause.Eq{Column: keyColumn, Value: g.prefixedKey(key)}).
		First(&entry).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return zero, errors.Wrapf(&ErrKeyNotFound{}, "key not found in gorm cache for key: %s", key)
		}
		return zero, errors.Wrapf(err, "failed to get cache entry for key: %s", key)
	}
	if entry.Key != g.prefixedKey(key) {
		// a key column that is not byte-exact (e.g. MySQL's _ci collations)
		// matched another key's row
		return zero, errors.Wrapf(&ErrKeyNotFound{}, "key not found in gorm cache for key: %s", key)
	}

	var value T
	if err := json.Unmarshal(entry.Value, &value); err != nil {
		return zero, errors.Wrapf(err, "failed to unmarshal value for key: %s", key)
	}

	return value, nil
}

// Del removes a value from the cache
func (g *GORMCache[T]) Del(ctx context.Context, key string) error {
	if err := g.write(ctx, func(tx *gorm.DB) error {
		return tx.Table(g.tableName).Where(clause.Eq{Column: keyColumn, Value: g.prefixedKey(key)}).Delete(nil).Error
	}); err != nil {
		return errors.Wrapf(err, "failed to delete cache entry for key: %s", key)
	}
	return nil
}

// gormMaxChunkSize keeps every statement under the databases' bound parameter
// limits: an upsert binds 3 parameters per row, and SQLite allows 32766.
const gormMaxChunkSize = 10000

// GetMany retrieves many values with `WHERE key IN (...)` queries of up to
// ChunkSize keys each.
// Missing keys are absent from the result; a key that fails (its chunk's query,
// or unmarshaling its value) is reported in a *BatchError without hiding the others.
func (g *GORMCache[T]) GetMany(ctx context.Context, keys []string) (map[string]T, error) {
	out := make(map[string]T, len(keys))
	if len(keys) == 0 {
		return out, nil
	}

	prefixed := make([]string, len(keys))
	for i, key := range keys {
		prefixed[i] = g.prefixedKey(key)
	}

	stored := make(map[string]cacheEntry, len(keys))
	keyErrs := map[string]error{}
	tx := cmp.Or(GetGORMTx(ctx), g.db)
	for start := 0; start < len(keys); start += g.chunkSize {
		chunk := prefixed[start:min(start+g.chunkSize, len(keys))]
		var found []cacheEntry
		if err := tx.WithContext(ctx).
			Table(g.tableName).
			Where(clause.IN{Column: keyColumn, Values: anys(chunk)}).
			Find(&found).Error; err != nil {
			for _, key := range keys[start : start+len(chunk)] {
				keyErrs[key] = errors.Wrapf(err, "failed to get cache entry for key: %s", key)
			}
			continue
		}
		for _, entry := range found {
			stored[entry.Key] = entry
		}
	}

	for i, key := range keys {
		// exact lookup: a key column that is not byte-exact (e.g. MySQL's _ci
		// collations) can return another key's row, which is not this key's value
		entry, ok := stored[prefixed[i]]
		if !ok {
			continue
		}
		if _, failed := keyErrs[key]; failed {
			continue
		}
		var value T
		if err := json.Unmarshal(entry.Value, &value); err != nil {
			keyErrs[key] = errors.Wrapf(err, "failed to unmarshal value for key: %s", key)
			continue
		}
		out[key] = value
	}
	return out, batchError(keyErrs)
}

// SetMany stores many values with multi-row upserts of up to ChunkSize rows
// each, every statement on its own. It is best effort: every key is tried, and
// the keys that failed (to be encoded, or their chunk's statement) are reported
// in a *BatchError.
func (g *GORMCache[T]) SetMany(ctx context.Context, values map[string]T) error {
	keyErrs := map[string]error{}
	type row struct {
		key   string
		entry cacheEntry
	}
	rows := make([]row, 0, len(values))
	for key, value := range values {
		data, err := json.Marshal(value)
		if err != nil {
			keyErrs[key] = errors.Wrapf(err, "failed to marshal value for key: %s", key)
			continue
		}
		rows = append(rows, row{key: key, entry: cacheEntry{Key: g.prefixedKey(key), Value: data}})
	}
	// one row order for every caller, or overlapping batches lock rows in
	// opposite orders and deadlock (MySQL, PostgreSQL)
	slices.SortFunc(rows, func(a, b row) int { return cmp.Compare(a.entry.Key, b.entry.Key) })

	for chunk := range slices.Chunk(rows, g.chunkSize) {
		entries := make([]cacheEntry, len(chunk))
		for i, r := range chunk {
			entries[i] = r.entry
		}
		if err := g.write(ctx, func(tx *gorm.DB) error {
			return tx.Table(g.tableName).Clauses(upsertEntry).Create(&entries).Error
		}); err != nil {
			for _, r := range chunk {
				keyErrs[r.key] = errors.Wrapf(err, "failed to set cache entry for key: %s", r.key)
			}
		}
	}
	return batchError(keyErrs)
}

// DelMany removes many keys with `WHERE key IN (...)` deletes of up to
// ChunkSize keys each, every statement on its own. It is best effort: every key
// is tried, and the keys of a failed statement are reported in a *BatchError.
func (g *GORMCache[T]) DelMany(ctx context.Context, keys []string) error {
	keyErrs := map[string]error{}
	for chunk := range slices.Chunk(keys, g.chunkSize) {
		prefixed := make([]string, len(chunk))
		for i, key := range chunk {
			prefixed[i] = g.prefixedKey(key)
		}
		if err := g.write(ctx, func(tx *gorm.DB) error {
			in := clause.IN{Column: keyColumn, Values: anys(prefixed)}
			if tx.Name() != "postgres" {
				return tx.Table(g.tableName).Where(in).Delete(nil).Error
			}
			// DELETE locks rows in scan order, which follows the column's
			// collation (often en_US), not SetMany's byte order: lock them in
			// byte order first, or the two deadlock each other
			locked := tx.Session(&gorm.Session{NewDB: true}).Table(g.tableName).
				Select("?", keyColumn).Where(in).
				Order(clause.OrderBy{Expression: clause.Expr{SQL: `? COLLATE "C"`, Vars: []any{keyColumn}, WithoutParentheses: true}}).
				Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
			return tx.Table(g.tableName).Where("? IN (?)", keyColumn, locked).Delete(nil).Error
		}); err != nil {
			for _, key := range chunk {
				keyErrs[key] = errors.Wrapf(err, "failed to delete cache entry for key: %s", key)
			}
		}
	}
	return batchError(keyErrs)
}
