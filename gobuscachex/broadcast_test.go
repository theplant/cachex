package gobuscachex

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	bus "github.com/qor5/go-bus"
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
	// a database restart or failover cuts every connection: go-que's mutex
	// behind the consumer stays broken while the heartbeats succeed, so
	// Listen notices that its own probes stop arriving, and subscribes again
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
	// messages and probes
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
	time.Sleep(time.Second) // ponytail: a few heartbeat ticks, each with a probe
	cancel()
	<-done
	var rows int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM goque_jobs").Scan(&rows))
	assert.Zero(t, rows, "messages and probes are destroyed once handled, the rest drained")
}

func TestALongSubjectStillSubscribes(t *testing.T) {
	// go-bus allows 16 tokens and 255 characters: the probe's subject must not
	// build on the configured one
	b, _ := newBus(t)
	for _, subject := range []string{
		strings.TrimSuffix(strings.Repeat("a.", 16), "."),
		strings.TrimSuffix(strings.Repeat(strings.Repeat("a", 59)+".", 4), "."),
	} {
		ctx, cancel := context.WithCancel(context.Background())
		ready := make(chan struct{}, 1)
		go func() {
			_ = NewBroadcast(BroadcastConfig{Bus: b, Subject: subject, WorkerConfig: fast}).Listen(ctx, func() { ready <- struct{}{} }, func([]byte) {})
		}()
		waitFor(t, ready)
		cancel()
	}
}
