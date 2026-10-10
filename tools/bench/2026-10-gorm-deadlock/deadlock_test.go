//go:build bench

// Compares ways to keep concurrent GORMCache batch writes from deadlocking, on
// MySQL 8.4 and PostgreSQL 16 in testcontainers (Docker required). See
// docs/research/2026-10-gorm-deadlock.md.
//
//	go test -tags bench -run TestDeadlockStrategies -v -timeout 60m ./tools/bench/2026-10-gorm-deadlock/
//
// WORKERS and ROUNDS (env) scale the load; defaults 16 and 80. The "unordered"
// strategy is slow on PostgreSQL: every deadlock waits out deadlock_timeout.
package bench

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/theplant/cachex"
	"gorm.io/datatypes"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// row matches the table GORMCache.Migrate creates, for the strategies that
// write it with raw GORM (what GORMCache did before the fix).
type row struct {
	Key       string `gorm:"primaryKey"`
	Value     datatypes.JSON
	UpdatedAt time.Time
}

func isDeadlock(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "deadlock") || strings.Contains(s, "40p01") || strings.Contains(s, "(40001)")
}

// expectation is what a strategy must show for the run to be valid.
type expectation int

const (
	expectDeadlocks        expectation = iota // must reproduce the race, or the load did not exercise it
	expectNoneOnPG                            // no deadlocks on PostgreSQL; MySQL's gap locks may still cause a few
	expectNoFailures                          // no deadlocks and no errors at all
	expectNoFailuresSerial                    // same, and at most one write inside the lock at a time
)

type strategy struct {
	name   string
	expect expectation
	set    func(ctx context.Context, db *gorm.DB, c *cachex.GORMCache[string], table string, values map[string]string) error
	del    func(ctx context.Context, db *gorm.DB, c *cachex.GORMCache[string], table string, keys []string) error
}

var keyCol = clause.Column{Name: "key"}

func rawSet(sorted bool) func(context.Context, *gorm.DB, *cachex.GORMCache[string], string, map[string]string) error {
	return func(ctx context.Context, db *gorm.DB, _ *cachex.GORMCache[string], table string, values map[string]string) error {
		rows := make([]row, 0, len(values))
		for k, v := range values { // Go map order: random
			data, _ := json.Marshal(v)
			rows = append(rows, row{Key: k, Value: data, UpdatedAt: time.Now()})
		}
		if sorted {
			slices.SortFunc(rows, func(a, b row) int { return cmp.Compare(a.Key, b.Key) })
		}
		return db.WithContext(ctx).Table(table).Clauses(clause.OnConflict{
			Columns:   []clause.Column{keyCol},
			DoUpdates: clause.AssignmentColumns([]string{"key", "value", "updated_at"}),
		}).Create(&rows).Error
	}
}

func rawDel(ordered bool) func(context.Context, *gorm.DB, *cachex.GORMCache[string], string, []string) error {
	return func(ctx context.Context, db *gorm.DB, _ *cachex.GORMCache[string], table string, keys []string) error {
		in := clause.IN{Column: keyCol, Values: anys(keys)}
		if !ordered || db.Name() != "postgres" {
			return db.WithContext(ctx).Table(table).Where(in).Delete(nil).Error
		}
		locked := db.Session(&gorm.Session{NewDB: true}).Table(table).Select("?", keyCol).Where(in).
			Order(clause.OrderBy{Expression: clause.Expr{SQL: `? COLLATE "C"`, Vars: []any{keyCol}, WithoutParentheses: true}}).
			Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
		return db.WithContext(ctx).Table(table).Where("? IN (?)", keyCol, locked).Delete(nil).Error
	}
}

func anys(keys []string) []any {
	out := make([]any, len(keys))
	for i, k := range keys {
		out[i] = k
	}
	return out
}

// inLock and maxInLock check that the table lock really serializes writes.
var inLock, maxInLock atomic.Int32

func locked(f func() error) error {
	n := inLock.Add(1)
	for m := maxInLock.Load(); n > m && !maxInLock.CompareAndSwap(m, n); m = maxInLock.Load() {
	}
	defer inLock.Add(-1)
	return f()
}

// tableLocked serializes every write to the table: a named lock on MySQL, held
// on one connection around the whole transaction (taken before BEGIN, released
// after COMMIT); an advisory transaction lock on PostgreSQL. Either has the
// effect of a table lock.
func tableLocked(ctx context.Context, db *gorm.DB, table string, f func(ctx context.Context) error) error {
	if db.Name() != "mysql" {
		return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", "bench:"+table).Error; err != nil {
				return err
			}
			return locked(func() error { return f(cachex.WithGORMTx(ctx, tx)) })
		})
	}
	return db.WithContext(ctx).Connection(func(conn *gorm.DB) error {
		if err := conn.Exec("SELECT GET_LOCK(?, 30)", "bench:"+table).Error; err != nil {
			return err
		}
		defer conn.Exec("SELECT RELEASE_LOCK(?)", "bench:"+table)
		return conn.Transaction(func(tx *gorm.DB) error {
			return locked(func() error { return f(cachex.WithGORMTx(ctx, tx)) })
		})
	})
}

var strategies = []strategy{
	{name: "unordered", expect: expectDeadlocks, set: rawSet(false), del: rawDel(false)},
	{name: "ordered", expect: expectNoneOnPG, set: rawSet(true), del: rawDel(true)},
	{name: "ordered+retry (GORMCache)", expect: expectNoFailures,
		set: func(ctx context.Context, _ *gorm.DB, c *cachex.GORMCache[string], _ string, v map[string]string) error {
			return c.SetMany(ctx, v)
		},
		del: func(ctx context.Context, _ *gorm.DB, c *cachex.GORMCache[string], _ string, k []string) error {
			return c.DelMany(ctx, k)
		}},
	{name: "table lock", expect: expectNoFailuresSerial,
		set: func(ctx context.Context, db *gorm.DB, c *cachex.GORMCache[string], table string, v map[string]string) error {
			return tableLocked(ctx, db, table, func(ctx context.Context) error { return c.SetMany(ctx, v) })
		},
		del: func(ctx context.Context, db *gorm.DB, c *cachex.GORMCache[string], table string, k []string) error {
			return tableLocked(ctx, db, table, func(ctx context.Context) error { return c.DelMany(ctx, k) })
		}},
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

func TestDeadlockStrategies(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()
	workers, rounds := envInt("WORKERS", 16), envInt("ROUNDS", 80)

	mc, err := tcmysql.Run(ctx, "mysql:8.4")
	testcontainers.CleanupContainer(t, mc)
	require.NoError(t, err)
	mdsn, err := mc.ConnectionString(ctx, "parseTime=true")
	require.NoError(t, err)
	pc, err := tcpostgres.Run(ctx, "postgres:16", tcpostgres.BasicWaitStrategies())
	testcontainers.CleanupContainer(t, pc)
	require.NoError(t, err)
	pdsn, err := pc.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	// mixed case: byte order differs from PostgreSQL's default en_US collation
	keys := make([]string, 2000)
	for i := range keys {
		keys[i] = fmt.Sprintf("%c%04d", "aBcD"[i%4], i)
	}

	for _, d := range []struct {
		name string
		dial gorm.Dialector
	}{{"mysql", mysql.Open(mdsn)}, {"postgres", postgres.Open(pdsn)}} {
		db, err := gorm.Open(d.dial, &gorm.Config{Logger: logger.Discard})
		require.NoError(t, err)
		sqlDB, err := db.DB()
		require.NoError(t, err)
		sqlDB.SetMaxOpenConns(32)

		for i, st := range strategies {
			table := fmt.Sprintf("bench_deadlock_%d", i)
			require.NoError(t, db.Exec("DROP TABLE IF EXISTS "+table).Error)
			c := cachex.NewGORMCache[string](&cachex.GORMCacheConfig{DB: db, TableName: table})
			require.NoError(t, c.Migrate(ctx))

			var deadlocks, failures, ops atomic.Int64
			var sample atomic.Value // one real deadlock error, to show what was counted
			maxInLock.Store(0)
			start := time.Now()
			var wg sync.WaitGroup
			for range workers {
				wg.Go(func() {
					for range rounds {
						pick := rand.Perm(len(keys))[:100]
						var err error
						if rand.IntN(10) < 6 {
							values := map[string]string{}
							for _, j := range pick {
								values[keys[j]] = "v"
							}
							err = st.set(ctx, db, c, table, values)
						} else {
							sub := make([]string, len(pick))
							for n, j := range pick {
								sub[n] = keys[j]
							}
							err = st.del(ctx, db, c, table, sub)
						}
						ops.Add(1)
						if isDeadlock(err) {
							deadlocks.Add(1)
							sample.CompareAndSwap(nil, err.Error())
						} else if err != nil {
							failures.Add(1)
							t.Errorf("%s/%s: %v", d.name, st.name, err)
						}
					}
				})
			}
			wg.Wait()
			elapsed := time.Since(start)
			t.Logf("%-8s %-26s deadlocks %4d / %d   %7.2fs   %6.0f ops/s",
				d.name, st.name, deadlocks.Load(), ops.Load(), elapsed.Seconds(), float64(ops.Load())/elapsed.Seconds())
			if msg, ok := sample.Load().(string); ok {
				t.Logf("    sample: %.140s", msg)
			}

			switch st.expect {
			case expectDeadlocks:
				if deadlocks.Load() == 0 {
					t.Errorf("%s/%s: no deadlock reproduced; the load is too light to show the race", d.name, st.name)
				}
			case expectNoneOnPG:
				if d.name == "postgres" && deadlocks.Load() > 0 {
					t.Errorf("%s/%s: %d deadlocks with one lock order", d.name, st.name, deadlocks.Load())
				}
			case expectNoFailures, expectNoFailuresSerial:
				if deadlocks.Load()+failures.Load() > 0 {
					t.Errorf("%s/%s: %d deadlocks and %d other failures reached the caller", d.name, st.name, deadlocks.Load(), failures.Load())
				}
				if st.expect == expectNoFailuresSerial && maxInLock.Load() != 1 {
					t.Errorf("%s/%s: %d writes ran inside the lock at once", d.name, st.name, maxInLock.Load())
				}
			}
		}
	}
}
