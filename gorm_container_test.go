package cachex

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// TestGORMCacheOnRealDatabases runs GORMCache against MySQL and PostgreSQL in
// containers (skipped without Docker): SQLite accepts things they do not, such
// as the bare reserved word "key", and compares keys byte-exactly by default.
func TestGORMCacheOnRealDatabases(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()

	dbs := map[string]func(t *testing.T) gorm.Dialector{
		"mysql": func(t *testing.T) gorm.Dialector {
			c, err := tcmysql.Run(ctx, "mysql:8.4")
			testcontainers.CleanupContainer(t, c)
			require.NoError(t, err)
			dsn, err := c.ConnectionString(ctx, "parseTime=true")
			require.NoError(t, err)
			return mysql.Open(dsn)
		},
		"postgres": func(t *testing.T) gorm.Dialector {
			c, err := tcpostgres.Run(ctx, "postgres:16", tcpostgres.BasicWaitStrategies())
			testcontainers.CleanupContainer(t, c)
			require.NoError(t, err)
			dsn, err := c.ConnectionString(ctx, "sslmode=disable")
			require.NoError(t, err)
			return postgres.Open(dsn)
		},
	}
	for name, open := range dbs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db, err := gorm.Open(open(t), &gorm.Config{})
			require.NoError(t, err)
			newCache := func(t *testing.T) *GORMCache[string] {
				c := NewGORMCache[string](&GORMCacheConfig{DB: db, TableName: "cache_" + t.Name()[len(name)+1+len("TestGORMCacheOnRealDatabases/"):], KeyPrefix: "p:"})
				require.NoError(t, c.Migrate(ctx))
				return c
			}

			t.Run("contract", func(t *testing.T) { testBatchCache(t, newCache(t)) })

			t.Run("batch larger than one statement", func(t *testing.T) {
				c := newCache(t)
				values, keys := map[string]string{}, []string{}
				for i := range 2500 {
					k := fmt.Sprintf("k%04d", i)
					values[k], keys = "v"+k, append(keys, k)
				}
				require.NoError(t, c.SetMany(ctx, values))
				got, err := c.GetMany(ctx, keys)
				require.NoError(t, err)
				assert.Equal(t, values, got)
				require.NoError(t, c.DelMany(ctx, keys))
				got, err = c.GetMany(ctx, keys)
				require.NoError(t, err)
				assert.Empty(t, got)
			})

			t.Run("keys are case-sensitive", func(t *testing.T) {
				c := newCache(t)
				require.NoError(t, c.Set(ctx, "ABC", "upper"))
				require.NoError(t, c.Set(ctx, "abc", "lower"))
				got, err := c.GetMany(ctx, []string{"ABC", "abc", "Abc"})
				require.NoError(t, err)
				assert.Equal(t, map[string]string{"ABC": "upper", "abc": "lower"}, got, "a table Migrate created keeps them apart")
				v, err := c.Get(ctx, "ABC")
				require.NoError(t, err)
				assert.Equal(t, "upper", v)
			})

			if name == "mysql" {
				t.Run("an existing case-insensitive table never serves another key's value", func(t *testing.T) {
					table := "cache_legacy_ci"
					require.NoError(t, db.Exec("CREATE TABLE "+table+" (`key` varchar(255) NOT NULL PRIMARY KEY, `value` json NOT NULL, `updated_at` datetime(3) NOT NULL) COLLATE=utf8mb4_0900_ai_ci").Error)
					c := NewGORMCache[string](&GORMCacheConfig{DB: db, TableName: table})
					require.NoError(t, c.Migrate(ctx), "an existing table is left as it is")

					require.NoError(t, c.Set(ctx, "ABC", "upper"))
					_, err := c.Get(ctx, "abc")
					assert.True(t, IsErrKeyNotFound(err), "abc does not get ABC's row: %v", err)
					require.NoError(t, c.Set(ctx, "abc", "lower")) // takes the shared row over
					got, err := c.GetMany(ctx, []string{"ABC", "abc"})
					require.NoError(t, err)
					assert.Equal(t, map[string]string{"abc": "lower"}, got)
				})
			}

			t.Run("Client over it", func(t *testing.T) {
				c := newCache(t)
				up := &batchUpstream{data: map[string]string{"a": "1", "b": "2"}}
				cli := NewClient[string](c, up)
				got, err := cli.GetMany(ctx, []string{"a", "b", "x"})
				require.NoError(t, err)
				assert.Equal(t, map[string]string{"a": "1", "b": "2"}, got)
				l1 := NewClient[string](NewSyncMap[string](), c)
				require.NoError(t, l1.Set(ctx, "k", "v1"))
				v, err := c.Get(ctx, "k")
				require.NoError(t, err)
				assert.Equal(t, "v1", v, "Set writes down to the database layer")
				require.NoError(t, l1.Del(ctx, "k"))
				_, err = l1.Get(ctx, "k")
				assert.True(t, IsErrKeyNotFound(err))
			})
		})
	}
}
