package gobuscachex

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	bus "github.com/qor5/go-bus"
	"github.com/qor5/go-bus/bussql"
	"github.com/qor5/go-bus/pgbus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

// newBus starts PostgreSQL and returns a bus on it, and its database.
func newBus(t *testing.T) (bus.Bus, *sql.DB) {
	t.Helper()
	return busOn(t, startPostgres(t))
}

// startPostgres starts PostgreSQL in a container and returns its DSN.
func startPostgres(t *testing.T) string {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()
	var dsn string
	var err error
	for try := 1; try <= 3; try++ { // a loaded Docker sometimes fails a start: see gormcachex's startContainer
		var c *tcpostgres.PostgresContainer
		c, err = tcpostgres.Run(ctx, "postgres:16", tcpostgres.BasicWaitStrategies())
		testcontainers.CleanupContainer(t, c)
		if err == nil {
			dsn, err = c.ConnectionString(ctx, "sslmode=disable")
		}
		if err == nil {
			break
		}
		t.Logf("starting PostgreSQL, try %d: %v", try, err)
	}
	require.NoError(t, err)
	return dsn
}

// busOn returns a go-bus on its own connection pool to dsn.
func busOn(t *testing.T, dsn string) (bus.Bus, *sql.DB) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	b, err := pgbus.New(db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	return b, db
}

var fast = &bus.WorkerConfig{MaxLockPerSecond: 50, MaxBufferJobsCount: 50, MaxPerformPerSecond: 1000, MaxConcurrentPerformCount: 10}

func TestBroadcast(t *testing.T) {
	b, _ := newBus(t)
	var n atomic.Int64
	cachextest.TestBroadcast(t, func(*testing.T) (cachex.Broadcast, cachex.Broadcast) {
		subject := fmt.Sprintf("cachex.invalidate.t%d", n.Add(1))
		cfg := BroadcastConfig{Bus: b, Subject: subject, WorkerConfig: fast}
		return NewBroadcast(cfg), NewBroadcast(cfg)
	})
}

func TestNewBroadcastNeedsABus(t *testing.T) {
	assert.Panics(t, func() { NewBroadcast(BroadcastConfig{}) })
}

func TestAnExpiredSubscriptionIsRenewedAndCallsReady(t *testing.T) {
	// a subscription that missed its heartbeats expired, and with it what was
	// published meanwhile: the Cache clears its local layers on the new ready
	b, db := newBus(t)
	cfg := BroadcastConfig{Bus: b, Subject: "cachex.invalidate.renew", TTL: 1500 * time.Millisecond, WorkerConfig: fast}
	br := NewBroadcast(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{}, 16)
	got := make(chan []byte, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = br.Listen(ctx, func() { ready <- struct{}{} }, func(m []byte) { got <- m })
	}()
	waitFor(t, ready)

	_, err := db.ExecContext(ctx, "DELETE FROM gobus_subscriptions") // as if it expired
	require.NoError(t, err)
	waitFor(t, ready)
	require.NoError(t, NewBroadcast(cfg).Publish(ctx, []byte("after")))
	select {
	case m := <-got:
		assert.Equal(t, "after", string(m))
	case <-time.After(20 * time.Second):
		t.Fatal("no message after the renewal")
	}
	cancel()
	<-done
	var left int
	require.NoError(t, db.QueryRowContext(context.Background(), "SELECT count(*) FROM gobus_subscriptions WHERE deleted_at IS NULL").Scan(&left))
	assert.Zero(t, left, "Listen unsubscribed when its ctx ended")
}

func waitFor(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(20 * time.Second):
		t.Fatal("ready was not called")
	}
}

func TestTwoInstancesShareTheirWrites(t *testing.T) {
	ctx := context.Background()
	b, _ := newBus(t)
	cfg := BroadcastConfig{Bus: b, Subject: "cachex.invalidate.e2e", WorkerConfig: fast}
	src := cachex.SourceFunc[string](func(context.Context, string) (string, error) { return "v1", nil })
	shared := cachextest.NewLeasingMap[string]()
	newInstance := func() (*cachex.Cache[string], *cachextest.Map[string]) {
		local := cachextest.NewMap[string]()
		c := cachex.New(src, []cachex.Layer[string]{
			cachex.NewLayer(local, cachex.TTL(time.Hour, 0)),
			cachex.NewLayer(shared, cachex.TTL(time.Hour, 0)),
		}, cachex.WithBroadcast(NewBroadcast(cfg)))
		t.Cleanup(func() { _ = c.Close() })
		return c, local
	}
	a, _ := newInstance()
	bc, bLocal := newInstance()
	require.Eventually(t, func() bool { // b's first clear is behind it
		_, err := bc.Get(ctx, "k")
		_, ok, _ := bLocal.Get(ctx, "k")
		return err == nil && ok
	}, 20*time.Second, 50*time.Millisecond)
	require.Eventually(t, func() bool { // and a's writes reach b
		require.NoError(t, a.Set(ctx, "k", "v2"))
		_, ok, _ := bLocal.Get(ctx, "k")
		return !ok
	}, 20*time.Second, 10*time.Millisecond)
	v, err := bc.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "v2", v)
}

func TestDeliveriesResumeAfterADroppedConnection(t *testing.T) {
	// a database restart or failover cuts every connection: the consumer's
	// worker stops, and go-bus recreates it on its go-que mutex, which
	// connects again
	b, db := newBus(t)
	cfg := BroadcastConfig{Bus: b, Subject: "cachex.invalidate.drop", TTL: 3 * time.Second, WorkerConfig: fast}
	br := NewBroadcast(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{}, 16)
	got := make(chan []byte, 16)
	go func() { _ = br.Listen(ctx, func() { ready <- struct{}{} }, func(m []byte) { got <- m }) }()
	waitFor(t, ready)
	require.NoError(t, NewBroadcast(cfg).Publish(ctx, []byte("before"))) // the consumer's mutex holds a connection
	select {
	case <-got:
	case <-time.After(20 * time.Second):
		t.Fatal("no first message")
	}

	_, err := db.ExecContext(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE pid <> pg_backend_pid() AND datname = current_database()")
	require.NoError(t, err)
	// what is published before Listen subscribes again is lost (the Cache
	// clears its local layers on ready): publish until one arrives
	require.Eventually(t, func() bool {
		if NewBroadcast(cfg).Publish(ctx, []byte("after")) != nil {
			return false
		}
		select {
		case m := <-got:
			return string(m) == "after"
		case <-time.After(500 * time.Millisecond):
			return false
		}
	}, 40*time.Second, 10*time.Millisecond, "deliveries resumed")
}

func TestNothingIsLeftInTheQueueTable(t *testing.T) {
	// go-que's Done keeps a job's row; nothing would ever remove the rows of
	// messages
	b, db := newBus(t)
	cfg := BroadcastConfig{Bus: b, Subject: "cachex.invalidate.left", TTL: 600 * time.Millisecond, WorkerConfig: fast}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{}, 16)
	got := make(chan []byte, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = NewBroadcast(cfg).Listen(ctx, func() { ready <- struct{}{} }, func(m []byte) { got <- m })
	}()
	waitFor(t, ready)
	for range 3 {
		require.NoError(t, NewBroadcast(cfg).Publish(ctx, []byte("m")))
		<-got
	}
	time.Sleep(time.Second) // ponytail: a few heartbeat ticks
	cancel()
	<-done
	var rows int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM goque_jobs").Scan(&rows))
	assert.Zero(t, rows, "messages are destroyed once handled, the rest drained")
}

func TestADeadInstancesSubscriptionIsUnsubscribedByAPeer(t *testing.T) {
	// an instance killed without unsubscribing leaves its subscription and
	// its queue's messages; once it expired, a live instance's heartbeat
	// unsubscribes it and drains the queue
	b, db := newBus(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := BroadcastConfig{Bus: b, Subject: "cachex.invalidate.dead", TTL: 600 * time.Millisecond, WorkerConfig: fast}
	_, err := b.Queue("cachex-dead").Subscribe(ctx, cfg.Subject, bus.WithTTL(time.Minute), bus.WithAutoDrain(true))
	require.NoError(t, err)
	require.NoError(t, NewBroadcast(cfg).Publish(ctx, []byte("k")))
	_, err = db.ExecContext(ctx, "UPDATE gobus_subscriptions SET expires_at = NOW() - INTERVAL '1 second' WHERE queue = 'cachex-dead'")
	require.NoError(t, err)

	ready := make(chan struct{}, 4)
	go func() { _ = NewBroadcast(cfg).Listen(ctx, func() { ready <- struct{}{} }, func([]byte) {}) }()
	waitFor(t, ready)
	assert.Eventually(t, func() bool {
		var subs, jobs int
		require.NoError(t, db.QueryRow("SELECT count(*) FROM gobus_subscriptions WHERE queue = 'cachex-dead' AND deleted_at IS NULL").Scan(&subs))
		require.NoError(t, db.QueryRow("SELECT count(*) FROM goque_jobs WHERE queue = 'cachex-dead'").Scan(&jobs))
		return subs == 0 && jobs == 0
	}, 20*time.Second, 50*time.Millisecond, "the dead instance's subscription is unsubscribed and its queue drained")
}

func TestPublishNeverJoinsTheCallersTransaction(t *testing.T) {
	// go-bus publishes in a transaction it finds in ctx (bussql.NewContext):
	// a caller that changed the source in one, committed, and wrote the cache
	// with the same ctx would publish into a finished transaction
	ctx := context.Background()
	b, db := newBus(t)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	cfg := BroadcastConfig{Bus: b, Subject: "cachex.invalidate.tx", WorkerConfig: fast}
	assert.NoError(t, NewBroadcast(cfg).Publish(bussql.NewContext(ctx, tx), []byte("x")))
}
