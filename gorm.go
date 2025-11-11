package cachex

import (
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
	if err := g.db.WithContext(ctx).Table(g.tableName).AutoMigrate(&cacheEntry{}); err != nil {
		return errors.Wrap(err, "failed to migrate cache table")
	}
	return nil
}

// Set stores a value in the cache
func (g *GORMCache[T]) Set(ctx context.Context, key string, value T) error {
	data, err := json.Marshal(value)
	if err != nil {
		return errors.Wrap(err, "failed to marshal value")
	}

	entry := cacheEntry{
		Key:   g.prefixedKey(key),
		Value: data,
	}

	if err := g.db.WithContext(ctx).
		Table(g.tableName).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "key"}},
			UpdateAll: true,
		}).
		Create(&entry).Error; err != nil {
		return errors.Wrap(err, "failed to set cache entry")
	}

	return nil
}

// Get retrieves a value from the cache
func (g *GORMCache[T]) Get(ctx context.Context, key string) (T, error) {
	var zero T
	var entry cacheEntry

	if err := g.db.WithContext(ctx).
		Table(g.tableName).
		Where("key = ?", g.prefixedKey(key)).
		First(&entry).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return zero, &ErrKeyNotFound{}
		}
		return zero, errors.Wrap(err, "failed to get cache entry")
	}

	var value T
	if err := json.Unmarshal(entry.Value, &value); err != nil {
		return zero, errors.Wrap(err, "failed to unmarshal value")
	}

	return value, nil
}

// Del removes a value from the cache
func (g *GORMCache[T]) Del(ctx context.Context, key string) error {
	if err := g.db.WithContext(ctx).
		Table(g.tableName).
		Where("key = ?", g.prefixedKey(key)).
		Delete(nil).Error; err != nil {
		return errors.Wrap(err, "failed to delete cache entry")
	}
	return nil
}
