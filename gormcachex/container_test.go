package gormcachex

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
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
	"gorm.io/gorm/logger"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

// startContainer runs start, up to three times, and returns the DSN it gives.
// Docker Desktop hands out host ports in sequence without checking the Mac: a
// process listening on [::] at the port it picks fails the port forward, and
// the start fails (the port is "not found", or the reaper times out). Another
// try gets another port.
func startContainer(t *testing.T, start func() (string, testcontainers.Container, error)) string {
	t.Helper()
	var err error
	for try := 1; try <= 3; try++ {
		var dsn string
		var c testcontainers.Container
		dsn, c, err = start()
		testcontainers.CleanupContainer(t, c)
		if err == nil {
			return dsn
		}
		t.Logf("starting the container, try %d: %v", try, err)
	}
	require.NoError(t, err)
	return ""
}

// TestOnRealDatabases runs the backend against MySQL and PostgreSQL in
// containers (skipped without Docker): SQLite accepts things they do not, such
// as the bare reserved word "key", and compares keys byte-exactly by default.
func TestOnRealDatabases(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()

	dbs := map[string]func(t *testing.T) gorm.Dialector{
		"mysql": func(t *testing.T) gorm.Dialector {
			return mysql.Open(startContainer(t, func() (string, testcontainers.Container, error) {
				c, err := tcmysql.Run(ctx, "mysql:8.4")
				if err != nil {
					return "", c, err
				}
				dsn, err := c.ConnectionString(ctx, "parseTime=true")
				return dsn, c, err
			}))
		},
		"postgres": func(t *testing.T) gorm.Dialector {
			return postgres.Open(startContainer(t, func() (string, testcontainers.Container, error) {
				c, err := tcpostgres.Run(ctx, "postgres:16", tcpostgres.BasicWaitStrategies())
				if err != nil {
					return "", c, err
				}
				dsn, err := c.ConnectionString(ctx, "sslmode=disable")
				return dsn, c, err
			}))
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
				cachextest.TestBackend(t, func(t *testing.T) cachex.Backend[string] { return newB(t) }, strs)
				cachextest.TestBackend(t, func(t *testing.T) cachex.Backend[item] {
					return New[item](Config[item]{DB: db, TableName: newB(t).bytes.table})
				}, items)
				cachextest.TestByteBackend(t, func(t *testing.T) cachex.ByteBackend {
					b := newB(t).bytes
					b.chunkSize = 3
					return b
				})
			})

			t.Run("leases", func(t *testing.T) {
				testLeases(t, func(t *testing.T, chunkSize int) *Backend[string] {
					table := newB(t).bytes.table
					return New[string](Config[string]{DB: db, TableName: table, KeyPrefix: "p:", Leases: true, ChunkSize: chunkSize})
				})
			})

			t.Run("a key it cannot store fails alone", func(t *testing.T) {
				testUnstorableKeys(t, newB(t))
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

			t.Run("concurrent leases and writes do not fail", func(t *testing.T) {
				// the lease statements lock rows too: in byte order, like SetMany
				deadlocks := &deadlockCounter{Interface: db.Logger}
				lb := New[string](Config[string]{DB: db.Session(&gorm.Session{Logger: deadlocks}), TableName: newB(t).bytes.table, Leases: true})
				if name == "postgres" { // MySQL's gap locks deadlock now and then (see ADR 0010)
					t.Cleanup(func() { assert.Zero(t, deadlocks.n.Load(), "rows are locked in one order") })
				}
				l := lb.Leaser()
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
							sub := make([]string, len(pick))
							entries := map[string]cachex.Entry[string]{}
							for i, j := range pick {
								sub[i] = keys[j]
								entries[keys[j]] = entry("v")
							}
							switch round % 4 {
							case 0:
								assert.NoError(t, lb.DelMany(ctx, sub))
							case 1:
								assert.NoError(t, lb.SetMany(ctx, entries))
							case 2: // lease, then backfill half and release the rest
								tokens, err := l.Lease(ctx, sub, time.Minute)
								assert.NoError(t, err)
								put, rest := map[string]string{}, map[string]string{}
								for k, tok := range tokens {
									if len(put) < len(tokens)/2 {
										put[k] = tok
									} else {
										rest[k] = tok
									}
								}
								assert.NoError(t, l.PutLeased(ctx, put, entries))
								assert.NoError(t, l.Release(ctx, rest))
							case 3: // lease, then backfill deletes
								tokens, err := l.Lease(ctx, sub, time.Minute)
								assert.NoError(t, err)
								assert.NoError(t, l.PutLeased(ctx, tokens, nil))
							}
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
				require.NoError(t, c.Close()) // the backfill finishes after the read returns
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

// deadlockCounter counts the statements aborted as deadlock victims.
type deadlockCounter struct {
	logger.Interface
	n atomic.Int64
}

func (c *deadlockCounter) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	if isDeadlock(err) {
		c.n.Add(1)
	}
	c.Interface.Trace(ctx, begin, fc, err)
}
