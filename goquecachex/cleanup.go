// Package goquecachex runs a cache's cleanup (gormcachex's DeleteExpired) as
// a recurring job on go-que (github.com/qor5/go-que), on one instance at a
// time however many run it. go-que needs PostgreSQL for its queue; the
// table being cleaned may live elsewhere (MySQL, say).
package goquecachex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/qor5/go-bus/quex"
	que "github.com/qor5/go-que"
	"github.com/qor5/go-que/pg"
)

// DefaultInterval is how often the cleanup runs by default.
const DefaultInterval = time.Hour

// Cleaner deletes what has expired and reports how much: a
// *gormcachex.Backend is one.
type Cleaner interface {
	DeleteExpired(ctx context.Context) (int64, error)
}

// CleanupConfig configures StartCleanup.
type CleanupConfig struct {
	// DB is the PostgreSQL database of go-que's queue; its tables are
	// created if missing. Required.
	DB *sql.DB
	// Cleaner is what the job runs. Required.
	Cleaner Cleaner
	// Name identifies the job among the instances: those that start a
	// cleanup with the same Name share one job. Give each table its own.
	// Required.
	Name string
	// Interval is the time from the end of one run to the start of the
	// next. Default DefaultInterval.
	Interval time.Duration
	// Logger reports failed runs. Default slog.Default().
	Logger *slog.Logger
}

// Cleanup is a running cleanup; Stop it on shutdown.
type Cleanup struct {
	worker quex.WorkerController
}

// StartCleanup starts running cfg.Cleaner every cfg.Interval as a go-que
// job of its own, and returns once its worker runs; it is meant to be called
// on every instance. The job reschedules itself: each run destroys it and
// enqueues the next in one transaction, and a job's UniqueID keeps one job
// in the queue, so one instance runs it at a time. A failed run is logged and
// tried again within a minute (or the interval, if shorter). A failed
// rescheduling is retried by go-que for as long as it takes: go-que expires
// a job whose retries ran out, which would end the chain. Each instance also
// enqueues the job on start, so one deleted by hand comes back.
func StartCleanup(ctx context.Context, cfg CleanupConfig) (*Cleanup, error) {
	switch {
	case cfg.DB == nil:
		return nil, errors.New("goquecachex: StartCleanup: DB is required")
	case cfg.Cleaner == nil:
		return nil, errors.New("goquecachex: StartCleanup: Cleaner is required")
	case cfg.Name == "":
		return nil, errors.New("goquecachex: StartCleanup: Name is required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	q, err := pg.NewWithOptions(pg.Options{DB: cfg.DB, DBMigrate: true})
	if err != nil {
		return nil, fmt.Errorf("goquecachex: StartCleanup: %w", err)
	}
	j := &job{cfg: cfg, queue: q, name: "cachex-cleanup:" + cfg.Name}
	if err := j.seed(ctx); err != nil {
		return nil, err
	}
	worker, err := quex.StartWorker(ctx, que.WorkerOptions{
		Queue:                     j.name,
		Perform:                   j.perform,
		MaxLockPerSecond:          2,
		MaxBufferJobsCount:        1,
		MaxPerformPerSecond:       2,
		MaxConcurrentPerformCount: 1,
	}, quex.WithMutexFactory(q.Mutex)) // a go-que mutex is done after one error (a dropped connection, say): each recreated worker gets a new one
	if err != nil {
		return nil, fmt.Errorf("goquecachex: StartCleanup: %w", err)
	}
	return &Cleanup{worker: worker}, nil
}

// Stop stops the instance's worker; a run in progress finishes first, unless
// ctx ends. The job stays queued for the other instances.
func (c *Cleanup) Stop(ctx context.Context) error { return c.worker.Stop(ctx) }

type job struct {
	cfg   CleanupConfig
	queue que.Queue
	name  string // the job's queue and UniqueID
}

func (j *job) plan(runAt time.Time) que.Plan {
	return que.Plan{
		Queue:           j.name,
		Args:            que.Args(),
		RunAt:           runAt,
		RetryPolicy:     que.RetryPolicy{InitialInterval: time.Second, MaxInterval: time.Minute, NextIntervalMultiplier: 2, IntervalRandomPercent: 20, MaxRetryCount: -1}, // forever: see StartCleanup
		UniqueID:        &j.name,
		UniqueLifecycle: que.Lockable,
	}
}

// seed enqueues the job to run now, unless it is queued already.
func (j *job) seed(ctx context.Context) error {
	_, err := j.queue.Enqueue(ctx, nil, j.plan(time.Now()))
	if err != nil && !errors.Is(err, que.ErrViolateUniqueConstraint) {
		return fmt.Errorf("goquecachex: seed the cleanup %s: %w", j.cfg.Name, err)
	}
	return nil
}

// perform runs the cleanup, then destroys the job and enqueues the next run
// in one transaction. A failed cleanup only brings the next run closer; only
// a failed rescheduling returns an error, for go-que to retry.
func (j *job) perform(ctx context.Context, jb que.Job) error {
	next := j.cfg.Interval
	n, err := j.cfg.Cleaner.DeleteExpired(ctx)
	if err != nil {
		j.cfg.Logger.ErrorContext(ctx, "goquecachex: cleanup failed", "name", j.cfg.Name, "error", err)
		next = min(next, time.Minute)
	} else {
		j.cfg.Logger.DebugContext(ctx, "goquecachex: cleanup done", "name", j.cfg.Name, "deleted", n)
	}
	tx, err := j.cfg.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("goquecachex: reschedule the cleanup %s: %w", j.cfg.Name, err)
	}
	defer func() { _ = tx.Rollback() }()
	jb.In(tx)
	defer jb.In(nil)
	if err := jb.Destroy(ctx); err != nil {
		return fmt.Errorf("goquecachex: reschedule the cleanup %s: %w", j.cfg.Name, err)
	}
	if _, err := j.queue.Enqueue(ctx, tx, j.plan(time.Now().Add(next))); err != nil {
		return fmt.Errorf("goquecachex: reschedule the cleanup %s: %w", j.cfg.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("goquecachex: reschedule the cleanup %s: %w", j.cfg.Name, err)
	}
	return nil
}
