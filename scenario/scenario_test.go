// Package scenario runs the deployments cachex is mostly used in, end to end,
// with several instances: memory → Redis → source, and memory → PostgreSQL →
// source, both with leases on the shared layer and an invalidation broadcast
// (over Redis and over go-bus). The PostgreSQL one needs Docker and is
// skipped without it.
package scenario

import (
	"context"
	"database/sql"
	"errors"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	_ "github.com/jackc/pgx/v5/stdlib"
	bus "github.com/qor5/go-bus"
	"github.com/qor5/go-bus/pgbus"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
	"github.com/theplant/cachex/v2/gobuscachex"
	"github.com/theplant/cachex/v2/gormcachex"
	"github.com/theplant/cachex/v2/ottercachex"
	"github.com/theplant/cachex/v2/rediscachex"
)

// env is one deployment: what each instance builds its shared layer and its
// broadcast from. name isolates a test from the others in the same env.
type env struct {
	newShared    func(t *testing.T, name string) cachex.Backend[int]
	newBroadcast func(t *testing.T, name string) cachex.Broadcast
}

func TestScenarios(t *testing.T) {
	envs := map[string]func(t *testing.T) env{"redis": redisEnv, "postgres": postgresEnv}
	for name, start := range envs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := start(t)
			t.Run("a write reaches every instance", func(t *testing.T) { testWriteThenRead(t, e) })
			t.Run("reads never go back under concurrent writes", func(t *testing.T) { testNoGoingBack(t, e) })
			t.Run("a missed broadcast clears the memory layer", func(t *testing.T) { testMissedBroadcast(t, e) })
			t.Run("a stale hot key is refreshed once across instances", func(t *testing.T) { testOneRefresh(t, e) })
		})
	}
}

func redisEnv(t *testing.T) env {
	mr := miniredis.RunT(t)
	followTheClock(t, mr)
	client := func(t *testing.T) *redis.Client {
		c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	return env{
		newShared: func(t *testing.T, name string) cachex.Backend[int] {
			return rediscachex.New[int](rediscachex.Config[int]{Client: client(t), KeyPrefix: name + ":", Leases: true})
		},
		newBroadcast: func(t *testing.T, name string) cachex.Broadcast {
			return rediscachex.NewBroadcast(rediscachex.BroadcastConfig{Client: client(t), KeyPrefix: name + ":"})
		},
	}
}

// followTheClock makes mr's TTLs run with the wall clock, as Redis's do.
func followTheClock(t *testing.T, mr *miniredis.Miniredis) {
	done, stopped := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(done); <-stopped })
	go func() {
		defer close(stopped)
		last := time.Now()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-tick.C:
				mr.FastForward(now.Sub(last))
				last = now
			}
		}
	}()
}

func postgresEnv(t *testing.T) env {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()
	var dsn string
	var err error
	for try := 1; try <= 3; try++ { // see gormcachex's startContainer
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
	gdb, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sqlDB, err := gdb.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return env{
		newShared: func(t *testing.T, name string) cachex.Backend[int] {
			b := gormcachex.New[int](gormcachex.Config[int]{DB: gdb, TableName: "cache_" + name, Leases: true})
			require.NoError(t, b.Migrate(context.Background()))
			return b
		},
		newBroadcast: func(t *testing.T, name string) cachex.Broadcast {
			db, err := sql.Open("pgx", dsn) // a bus per instance, as in production
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			b, err := pgbus.New(db)
			require.NoError(t, err)
			t.Cleanup(func() { _ = b.Close() })
			return gobuscachex.NewBroadcast(gobuscachex.BroadcastConfig{
				Bus: b, Subject: "cachex.invalidate." + name,
				WorkerConfig: &bus.WorkerConfig{MaxLockPerSecond: 50, MaxBufferJobsCount: 50, MaxPerformPerSecond: 1000, MaxConcurrentPerformCount: 10},
			})
		},
	}
}

// source is the database the caches read through: a map of versions.
type source struct {
	mu    sync.Mutex
	data  map[string]int
	calls atomic.Int64
	delay func() time.Duration
}

func newSource(data map[string]int) *source { return &source{data: data, delay: func() time.Duration { return 0 }} }

func (s *source) Get(_ context.Context, key string) (int, error) {
	s.calls.Add(1)
	s.mu.Lock()
	v, ok := s.data[key]
	s.mu.Unlock()
	time.Sleep(s.delay()) // the answer travels back while writes go on
	if !ok {
		return 0, cachex.ErrNotFound
	}
	return v, nil
}

func (s *source) set(key string, v int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = v
}

func (s *source) del(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
}

// instance is one process: a memory layer over the shared one, and a
// broadcast that can be cut.
type instance struct {
	*cachex.Cache[int]
	mem *ottercachex.Backend[int]
	br  *switchable
}

type ttls struct{ memFresh, memStale, sharedFresh, sharedStale time.Duration }

// start builds n instances of e, and returns once each has subscribed (and
// cleared its memory layer for it).
func start(t *testing.T, e env, name string, n int, src *source, ttl ttls, opts ...cachex.Option) []*instance {
	t.Helper()
	out := make([]*instance, n)
	for i := range out {
		mem, err := ottercachex.New[int](ottercachex.Config[int]{MaximumSize: 10_000})
		require.NoError(t, err)
		br := newSwitchable(e.newBroadcast(t, name))
		c := cachex.New[int](src, []cachex.Layer[int]{
			cachex.NewLayer[int](mem, cachex.TTL(ttl.memFresh, ttl.memStale), cachex.NotFoundTTL(ttl.memFresh, 0)),
			cachex.NewLayer(e.newShared(t, name), cachex.TTL(ttl.sharedFresh, ttl.sharedStale), cachex.NotFoundTTL(ttl.sharedFresh, 0)),
		}, append([]cachex.Option{cachex.WithBroadcast(br)}, opts...)...)
		t.Cleanup(func() { _ = c.Close() })
		out[i] = &instance{Cache: c, mem: mem, br: br}
	}
	for _, in := range out {
		in.br.waitReady(t)
	}
	return out
}

func testWriteThenRead(t *testing.T, e env) {
	ctx := context.Background()
	src := newSource(map[string]int{"k": 1})
	ins := start(t, e, "wtr", 3, src, ttls{time.Hour, 0, time.Hour, 0})
	for _, in := range ins {
		v, err := in.Get(ctx, "k")
		require.NoError(t, err)
		require.Equal(t, 1, v)
	}

	src.set("k", 2)
	require.NoError(t, ins[0].Set(ctx, "k", 2))
	for _, in := range ins[1:] { // their memory layers held 1 for an hour
		assert.Eventually(t, func() bool { v, err := in.Get(ctx, "k"); return err == nil && v == 2 }, 20*time.Second, 5*time.Millisecond)
	}

	src.del("k")
	require.NoError(t, ins[1].Del(ctx, "k"))
	for _, in := range []*instance{ins[0], ins[2]} {
		assert.Eventually(t, func() bool { _, err := in.Get(ctx, "k"); return errors.Is(err, cachex.ErrNotFound) }, 20*time.Second, 5*time.Millisecond)
	}
}

// testNoGoingBack has one writer bump two keys' versions through the
// instances in turn while readers on every instance read them with short TTLs
// (so misses, refreshes and source fetches race the writes). What holds:
//
//   - once an instance's own write returned, no reader on it sees an older
//     version (ADR 0022: the order holds within a process; another
//     instance's write reaches it through the broadcast, and until then an
//     older version may come back);
//   - the shared layer never holds an older version than the last write
//     (leases);
//   - once the writes stop, every instance reads the last write.
func testNoGoingBack(t *testing.T, e env) {
	ctx := context.Background()
	keys := []string{"a", "b"}
	src := newSource(map[string]int{"a": 0, "b": 0})
	src.delay = func() time.Duration { return time.Duration(rand.IntN(2000)) * time.Microsecond }
	ins := start(t, e, "ngb", 3, src, ttls{5 * time.Millisecond, time.Hour, 20 * time.Millisecond, time.Hour})
	floors := make([]map[string]*atomic.Int64, len(ins)) // by instance: the last version it wrote, by key
	for i := range floors {
		floors[i] = map[string]*atomic.Int64{"a": {}, "b": {}}
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for r := range 2 * len(ins) {
		in, floor := ins[r%len(ins)], floors[r%len(ins)]
		wg.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				key := keys[i%len(keys)]
				min := floor[key].Load()
				v, err := in.Get(ctx, key)
				if !assert.NoError(t, err) {
					return
				}
				if int64(v) < min {
					t.Errorf("reader %d: %s is %d, older than %d its instance wrote", r, key, v, min)
					return
				}
			}
		})
	}
	const writes = 300
	for i := 1; i <= writes; i++ {
		key := keys[i%len(keys)]
		src.set(key, i)
		require.NoError(t, ins[i%len(ins)].Set(ctx, key, i))
		floors[i%len(ins)][key].Store(int64(i))
	}
	close(stop)
	wg.Wait()

	want := map[string]int{keys[writes%len(keys)]: writes, keys[(writes-1)%len(keys)]: writes - 1}
	for _, in := range ins {
		for _, key := range keys {
			assert.Eventually(t, func() bool { v, err := in.Get(ctx, key); return err == nil && v == want[key] }, 20*time.Second, 5*time.Millisecond)
		}
	}
	for _, in := range ins {
		require.NoError(t, in.Close()) // waits for the backfills in flight
	}
	shared := e.newShared(t, "ngb")
	for _, key := range keys {
		got, ok, err := shared.Get(ctx, key)
		require.NoError(t, err)
		if ok { // a key may have rotted away; never may it be older
			assert.Equal(t, want[key], got.Value, "%s in the shared layer", key)
		}
	}
}

func testMissedBroadcast(t *testing.T, e env) {
	ctx := context.Background()
	src := newSource(map[string]int{"k": 1})
	ins := start(t, e, "miss", 2, src, ttls{time.Hour, 0, time.Hour, 0})
	a, b := ins[0], ins[1]
	v, err := b.Get(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, 1, v)

	b.br.cut()
	src.set("k", 2)
	require.NoError(t, a.Set(ctx, "k", 2))
	v, err = b.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, 1, v, "b missed the write: its memory layer still has 1")

	b.br.restore()
	b.br.waitReady(t)
	v, err = b.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, 2, v, "subscribing again cleared b's memory layer")
}

// testOneRefresh lets a hot key turn stale on every instance at once: each
// instance refreshes it, and the shared layer's lease lets one of them ask the
// source.
func testOneRefresh(t *testing.T, e env) {
	ctx := context.Background()
	clock := cachextest.NewClock(time.Now())
	src := newSource(map[string]int{"k": 1})
	ins := start(t, e, "refresh", 3, src, ttls{time.Minute, time.Hour, time.Minute, time.Hour}, cachex.WithNow(clock.Now))
	shared := e.newShared(t, "refresh")
	for i, in := range ins {
		_, err := in.Get(ctx, "k")
		require.NoError(t, err)
		if i == 0 { // its backfill of the shared layer comes after the answer
			require.Eventually(t, func() bool { _, ok, _ := shared.Get(ctx, "k"); return ok }, 10*time.Second, time.Millisecond)
		}
	}
	require.EqualValues(t, 1, src.calls.Load(), "the others found it in the shared layer")

	src.set("k", 2)
	src.delay = func() time.Duration { return 50 * time.Millisecond } // the lease is held meanwhile
	clock.Advance(2 * time.Minute)
	var wg sync.WaitGroup
	for _, in := range ins {
		for range 10 {
			wg.Go(func() {
				v, err := in.Get(ctx, "k")
				assert.NoError(t, err)
				assert.Equal(t, 1, v, "stale, served at once")
			})
		}
	}
	wg.Wait()
	for _, in := range ins {
		require.NoError(t, in.Close()) // waits for its refresh
	}
	assert.EqualValues(t, 2, src.calls.Load(), "one refresh asked the source")
	e2, ok, err := shared.Get(ctx, "k")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 2, e2.Value)
}

// switchable is a broadcast that can be cut: cut ends the current
// subscription (what is published then is lost to this instance) and restore
// subscribes again. It reports each ready.
type switchable struct {
	inner cachex.Broadcast
	ready chan struct{}

	mu      sync.Mutex
	up      chan struct{} // closed while not cut
	cancel  context.CancelFunc
	stopped chan struct{} // closed when the current subscription has ended
}

func newSwitchable(inner cachex.Broadcast) *switchable {
	up := make(chan struct{})
	close(up)
	return &switchable{inner: inner, ready: make(chan struct{}, 64), up: up}
}

func (s *switchable) Publish(ctx context.Context, msg []byte) error { return s.inner.Publish(ctx, msg) }

func (s *switchable) Listen(ctx context.Context, ready func(), deliver func([]byte)) error {
	for {
		s.mu.Lock()
		up := s.up
		s.mu.Unlock()
		select {
		case <-up:
		case <-ctx.Done():
			return ctx.Err()
		}
		lctx, cancel := context.WithCancel(ctx)
		stopped := make(chan struct{})
		s.mu.Lock()
		s.cancel, s.stopped = cancel, stopped
		s.mu.Unlock()
		err := s.inner.Listen(lctx, func() { ready(); s.ready <- struct{}{} }, deliver)
		cancel()
		close(stopped)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.mu.Lock()
		wasCut := s.up != up
		s.mu.Unlock()
		if !wasCut {
			return err // failed on its own
		}
	}
}

// cut ends the subscription and returns once it has.
func (s *switchable) cut() {
	s.mu.Lock()
	s.up = make(chan struct{})
	cancel, stopped := s.cancel, s.stopped
	s.mu.Unlock()
	cancel()
	<-stopped
}

func (s *switchable) restore() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.up)
}

func (s *switchable) waitReady(t *testing.T) {
	t.Helper()
	select {
	case <-s.ready:
	case <-time.After(30 * time.Second):
		t.Fatal("the broadcast did not subscribe")
	}
}
