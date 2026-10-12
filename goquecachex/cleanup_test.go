package goquecachex

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()
	c, err := tcpostgres.Run(ctx, "postgres:16", tcpostgres.BasicWaitStrategies())
	testcontainers.CleanupContainer(t, c)
	require.NoError(t, err)
	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// cleaner counts its runs and the most that ran at once; it fails while
// fail is set.
type cleaner struct {
	runs, running, peak atomic.Int64
	fail                atomic.Bool
}

func (c *cleaner) DeleteExpired(context.Context) (int64, error) {
	n := c.running.Add(1)
	defer c.running.Add(-1)
	for p := c.peak.Load(); n > p && !c.peak.CompareAndSwap(p, n); p = c.peak.Load() {
	}
	time.Sleep(20 * time.Millisecond) // long enough for a second runner to overlap, if there were one
	c.runs.Add(1)
	if c.fail.Load() {
		return 0, errors.New("boom")
	}
	return 1, nil
}

func start(t *testing.T, db *sql.DB, c Cleaner, name string) *Cleanup {
	t.Helper()
	cl, err := StartCleanup(context.Background(), CleanupConfig{DB: db, Cleaner: c, Name: name, Interval: 100 * time.Millisecond})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cl.Stop(context.Background()) })
	return cl
}

func runsReach(t *testing.T, c *cleaner, n int64, msg string) {
	t.Helper()
	require.Eventually(t, func() bool { return c.runs.Load() >= n }, 30*time.Second, 10*time.Millisecond, msg)
}

func TestTheCleanupRunsOnOneInstanceAtATime(t *testing.T) {
	db := newDB(t)
	c := &cleaner{}
	for range 4 { // four instances
		start(t, db, c, "cache")
	}
	runsReach(t, c, 8, "the job keeps rescheduling itself")
	assert.Equal(t, int64(1), c.peak.Load(), "never two at once")
}

func TestAFailedCleanupIsTriedAgain(t *testing.T) {
	db := newDB(t)
	c := &cleaner{}
	c.fail.Store(true)
	start(t, db, c, "cache")
	runsReach(t, c, 3, "a failure schedules the next run")
	c.fail.Store(false)
	before := c.runs.Load()
	runsReach(t, c, before+2, "and runs go on once it succeeds")
}

func TestTheChainResumesAfterARestart(t *testing.T) {
	db := newDB(t)
	c := &cleaner{}
	cl := start(t, db, c, "cache")
	runsReach(t, c, 2, "running")
	require.NoError(t, cl.Stop(context.Background()))
	before := c.runs.Load()
	start(t, db, c, "cache")
	runsReach(t, c, before+2, "a new instance picks the job up")
}

func TestABrokenChainIsSeededAgain(t *testing.T) {
	// a job expired by go-que (retries exhausted) breaks the chain: the
	// instances seed it again
	db := newDB(t)
	c := &cleaner{}
	start(t, db, c, "cache")
	runsReach(t, c, 2, "running")
	_, err := db.Exec("DELETE FROM goque_jobs")
	require.NoError(t, err)
	before := c.runs.Load()
	runsReach(t, c, before+2, "seeded again")
}

func TestTwoTablesHaveTheirOwnJobs(t *testing.T) {
	db := newDB(t)
	a, b := &cleaner{}, &cleaner{}
	start(t, db, a, "a")
	start(t, db, b, "b")
	runsReach(t, a, 2, "a's job")
	runsReach(t, b, 2, "b's job")
}

func TestStartCleanupNeedsItsConfig(t *testing.T) {
	for name, cfg := range map[string]CleanupConfig{
		"no DB":      {Cleaner: &cleaner{}, Name: "x"},
		"no Cleaner": {DB: &sql.DB{}, Name: "x"},
		"no Name":    {DB: &sql.DB{}, Cleaner: &cleaner{}},
	} {
		_, err := StartCleanup(context.Background(), cfg)
		assert.Error(t, err, name)
	}
}
