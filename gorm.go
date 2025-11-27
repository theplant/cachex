package cachex

import (
	"cmp"
	"context"
	"encoding/json"
	"time"

	"github.com/pkg/errors"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// GORMCache is a cache implementation using GORM
type GORMCache[T any] struct {
	db        *gorm.DB
	tableName string
	keyPrefix string
}

var _ Cache[any] = &GORMCache[any]{}

type cacheEntry struct {
	Key       string         `gorm:"not null;primaryKey;size:255"`
	Value     datatypes.JSON `gorm:"not null;type:json"`
	UpdatedAt time.Time      `gorm:"not null;index"`
}

// GORMCacheConfig holds configuration for GORMCache
type GORMCacheConfig struct {
	// DB is the GORM database connection
	DB *gorm.DB

	// TableName is the name of the cache table
	TableName string

	// KeyPrefix is the prefix for all keys (optional)
	KeyPrefix string
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
	}
}

func (g *GORMCache[T]) prefixedKey(key string) string {
	return g.keyPrefix + key
}

// Migrate creates or updates the cache table schema
func (g *GORMCache[T]) Migrate(ctx context.Context) error {
	tx := cmp.Or(GetGORMTx(ctx), g.db)
	if err := tx.WithContext(ctx).Table(g.tableName).AutoMigrate(&cacheEntry{}); err != nil {
		return errors.Wrapf(err, "failed to migrate cache table for table: %s", g.tableName)
	}
	return nil
}

type ctxKeyGORMTx struct{}

func WithGORMTx(ctx context.Context, tx *gorm.DB) context.Context {
	return context.WithValue(ctx, ctxKeyGORMTx{}, tx)
}

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

	tx := cmp.Or(GetGORMTx(ctx), g.db)
	if err := tx.WithContext(ctx).
		Table(g.tableName).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "key"}},
			UpdateAll: true,
		}).
		Create(&entry).Error; err != nil {
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
		Where("key = ?", g.prefixedKey(key)).
		First(&entry).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return zero, errors.Wrapf(&ErrKeyNotFound{}, "key not found in gorm cache for key: %s", key)
		}
		return zero, errors.Wrapf(err, "failed to get cache entry for key: %s", key)
	}

	var value T
	if err := json.Unmarshal(entry.Value, &value); err != nil {
		return zero, errors.Wrapf(err, "failed to unmarshal value for key: %s", key)
	}

	return value, nil
}

// Del removes a value from the cache
func (g *GORMCache[T]) Del(ctx context.Context, key string) error {
	tx := cmp.Or(GetGORMTx(ctx), g.db)
	if err := tx.WithContext(ctx).
		Table(g.tableName).
		Where("key = ?", g.prefixedKey(key)).
		Delete(nil).Error; err != nil {
		return errors.Wrapf(err, "failed to delete cache entry for key: %s", key)
	}
	return nil
}
