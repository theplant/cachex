package gormcachex

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

// TestOnRealDatabases runs the backend against MySQL and PostgreSQL in
// containers (skipped without Docker): SQLite accepts things they do not, such
// as the bare reserved word "key", and compares keys byte-exactly by default.
func TestOnRealDatabases(t *testing.T) {
	if testing.Short() {
		t.Skip("containers")
	}
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
			tables := 0
			var mu sync.Mutex
			newB := func(t *testing.T) *Backend[string] {
				mu.Lock()
				tables++
				table := fmt.Sprintf("cache_%d", tables)
				mu.Unlock()
				b := New[string](Config[string]{DB: db, TableName: table, KeyPrefix: "p:"})
				require.NoError(t, b.Migrate(ctx))
				return b
			}

			t.Run("contract", func(t *testing.T) {
				cachextest.TestBackend(t, func(t *testing.T) cachex.Backend[string] { return newB(t) })
			})

			t.Run("a batch larger than one statement", func(t *testing.T) {
				b := newB(t)
				entries, keys := map[string]cachex.Entry[string]{}, []string{}
				for i := range 2500 {
					k := fmt.Sprintf("k%04d", i)
					entries[k], keys = entry("v"+k), append(keys, k)
				}
				require.NoError(t, b.SetMany(ctx, entries))
				got, err := b.GetMany(ctx, keys)
				require.NoError(t, err)
				assert.Len(t, got, 2500)
				require.NoError(t, b.DelMany(ctx, keys))
				got, err = b.GetMany(ctx, keys)
				require.NoError(t, err)
				assert.Empty(t, got)
			})

			if name == "mysql" {
				t.Run("Migrate rejects an existing table whose keys do not compare exactly", func(t *testing.T) {
					table := "cache_legacy_ci"
					require.NoError(t, db.Exec("CREATE TABLE "+table+" (`key` varchar(255) NOT NULL PRIMARY KEY, `value` longblob NOT NULL, `expires_at` datetime(3) NOT NULL, `updated_at` datetime(3) NOT NULL) COLLATE=utf8mb4_0900_ai_ci").Error)
					b := New[string](Config[string]{DB: db, TableName: table})
					err := b.Migrate(ctx)
					require.Error(t, err)
					assert.Contains(t, err.Error(), "utf8mb4_0900_ai_ci")
					alter := "ALTER TABLE " + table + " CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin"
					assert.Contains(t, err.Error(), alter, "the error says how to convert the table")
					require.NoError(t, db.Exec(alter).Error)
					require.NoError(t, b.Migrate(ctx))
				})
			}

			t.Run("concurrent SetMany and DelMany do not fail", func(t *testing.T) {
				// mixed case: byte order differs from PostgreSQL's default en_US
				// collation, which once made DELETE lock rows in another order
				b := newB(t)
				keys := make([]string, 400)
				for i := range keys {
					keys[i] = fmt.Sprintf("%c%03d", "aBcD"[i%4], i)
				}
				var wg sync.WaitGroup
				for w := range 8 {
					wg.Go(func() {
						for round := range 20 {
							r := rand.New(rand.NewPCG(uint64(w), uint64(round)))
							pick := r.Perm(len(keys))[:100]
							if round%3 == 0 {
								sub := make([]string, len(pick))
								for i, j := range pick {
									sub[i] = keys[j]
								}
								assert.NoError(t, b.DelMany(ctx, sub))
								continue
							}
							entries := map[string]cachex.Entry[string]{}
							for _, j := range pick {
								entries[keys[j]] = entry("v")
							}
							assert.NoError(t, b.SetMany(ctx, entries))
						}
					})
				}
				wg.Wait()
			})

			t.Run("a Cache over it", func(t *testing.T) {
				b := newB(t)
				c := cachex.New(cachex.SourceFunc[string](func(_ context.Context, key string) (string, error) {
					if key == "x" {
						return "", cachex.ErrNotFound
					}
					return "v" + key, nil
				}), []cachex.Layer[string]{cachex.NewLayer[string](b, cachex.TTL(time.Hour, 0), cachex.NotFoundTTL(time.Minute, 0))})
				m, err := c.GetMany(ctx, []string{"a", "b", "x"})
				require.NoError(t, err)
				assert.Equal(t, map[string]string{"a": "va", "b": "vb"}, m)
				require.NoError(t, c.Set(ctx, "a", "new"))
				e, ok, err := b.Get(ctx, "a")
				require.NoError(t, err)
				require.True(t, ok)
				assert.Equal(t, "new", e.Value)
				e, ok, err = b.Get(ctx, "x")
				require.NoError(t, err)
				require.True(t, ok)
				assert.True(t, e.NotFound)
			})
		})
	}
}
