package scenario

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	bus "github.com/qor5/go-bus"
	"github.com/qor5/go-bus/pgbus"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/gobuscachex"
	"github.com/theplant/cachex/v2/goquecachex"
	"github.com/theplant/cachex/v2/gormcachex"
	"github.com/theplant/cachex/v2/ottercachex"
	"github.com/theplant/cachex/v2/rediscachex"
)

// ---- source with versions ----

type vsource struct {
	mu    sync.Mutex
	data  map[string]int
	slow  atomic.Bool
	calls atomic.Int64
}

func (s *vsource) Get(_ context.Context, key string) (int, error) {
	s.calls.Add(1)
	s.mu.Lock()
	v, ok := s.data[key]
	s.mu.Unlock()
	if s.slow.Load() {
		time.Sleep(time.Duration(rand.IntN(300)) * time.Millisecond)
	} else {
		time.Sleep(time.Duration(rand.IntN(2000)) * time.Microsecond)
	}
	if !ok {
		return 0, cachex.ErrNotFound
	}
	return v, nil
}

func (s *vsource) put(key string, v int, del bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if del {
		delete(s.data, key)
	} else {
		s.data[key] = v
	}
}

func (s *vsource) snapshot() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for k, v := range s.data {
		out[k] = v
	}
	return out
}

// ---- bookkeeping ----

// op packs a version and whether it was a delete.
func pack(ver int64, del bool) int64 {
	p := ver * 2
	if del {
		p++
	}
	return p
}
func unpack(p int64) (int64, bool) { return p / 2, p%2 == 1 }

type done struct {
	p  int64
	at time.Time
}

type book struct {
	keys     []string
	keyMu    map[string]*sync.Mutex
	ver      atomic.Int64
	floors   []map[string]*atomic.Int64 // per instance: last own successful write
	shared   map[string]*atomic.Int64   // last successful write by any instance
	delsMu   sync.Mutex
	dels     map[string][]int64 // versions of deletes applied to the source
	failures chan string
	histMu   sync.Mutex
	hist     map[string][]done // successful writes, in order
	lag      time.Duration
	lags     atomic.Int64
	stats    struct{ reads, writes, writeErrs, readErrs, faults atomic.Int64 }
}

func newBook(keys []string, n int) *book {
	b := &book{keys: keys, keyMu: map[string]*sync.Mutex{}, shared: map[string]*atomic.Int64{}, dels: map[string][]int64{}, hist: map[string][]done{}, failures: make(chan string, 1000)}
	for _, k := range keys {
		b.keyMu[k] = &sync.Mutex{}
		b.shared[k] = &atomic.Int64{}
	}
	for range n {
		m := map[string]*atomic.Int64{}
		for _, k := range keys {
			m[k] = &atomic.Int64{}
		}
		b.floors = append(b.floors, m)
	}
	return b
}

func (b *book) fail(format string, args ...any) {
	select {
	case b.failures <- fmt.Sprintf(format, args...):
	default:
	}
}

func (b *book) delAfter(key string, ver int64) bool {
	b.delsMu.Lock()
	defer b.delsMu.Unlock()
	for _, d := range b.dels[key] {
		if d > ver {
			return true
		}
	}
	return false
}

// check one read against floor packed f (loaded before the read)
func (b *book) check(who string, key string, f int64, v int, found bool) {
	fv, fdel := unpack(f)
	if found {
		if int64(v) < fv || (int64(v) == fv && fdel) {
			b.fail("%s: %s read %d, older than floor %d (del=%v)", who, key, v, fv, fdel)
		}
		return
	}
	if fv > 0 && !fdel && !b.delAfter(key, fv) {
		b.fail("%s: %s read not-found, but floor is set %d and no delete came after", who, key, fv)
	}
	if fv == 0 && !b.delAfter(key, 0) {
		b.fail("%s: %s read not-found, but no delete ever", who, key)
	}
}

// lagFloor is the last write of key that completed more than b.lag ago.
func (b *book) lagFloor(key string) int64 {
	b.histMu.Lock()
	defer b.histMu.Unlock()
	h := b.hist[key]
	cut := time.Now().Add(-b.lag)
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].at.Before(cut) {
			return h[i].p
		}
	}
	return 0
}

func (b *book) checkLag(who, key string, f int64, v int, found bool) {
	if b.lag == 0 || f == 0 {
		return
	}
	fv, fdel := unpack(f)
	bad := false
	if found {
		bad = int64(v) < fv || (int64(v) == fv && fdel)
	} else {
		bad = !fdel && !b.delAfter(key, fv)
	}
	if bad {
		b.lags.Add(1)
		b.fail("LAG %s: %s read %d (found %v), but write %d (del=%v) completed more than %v ago", who, key, v, found, fv, fdel, b.lag)
	}
}

func storeMax(a *atomic.Int64, p int64) {
	for {
		old := a.Load()
		if old >= p || a.CompareAndSwap(old, p) {
			return
		}
	}
}

// ---- deployments with fault hooks ----

type chaosEnv struct {
	name      string
	newShared func(t *testing.T) cachex.Backend[int]
	newBcast  func(t *testing.T) cachex.Broadcast
	faults    []func(t *testing.T) // each injects one fault and heals it
	heal      func()
	after     func(t *testing.T) // checks after everything closed
	converge  time.Duration
	lag       time.Duration
}

func chaosRedis(t *testing.T, prefix string) *chaosEnv {
	mr := miniredis.RunT(t)
	followTheClock(t, mr)
	var clients []*redis.Client
	client := func(t *testing.T) *redis.Client {
		c := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: 2, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second})
		clients = append(clients, c)
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	var mrMu sync.Mutex
	return &chaosEnv{
		name: "redis",
		newShared: func(t *testing.T) cachex.Backend[int] {
			return rediscachex.New[int](rediscachex.Config[int]{Client: client(t), KeyPrefix: prefix, Leases: true})
		},
		newBcast: func(t *testing.T) cachex.Broadcast {
			return rediscachex.NewBroadcast(rediscachex.BroadcastConfig{Client: client(t), KeyPrefix: prefix})
		},
		faults: []func(t *testing.T){
			func(t *testing.T) { // every command fails for a while
				mr.SetError("ERR chaos")
				time.Sleep(time.Duration(50+rand.IntN(250)) * time.Millisecond)
				mr.SetError("")
			},
			func(t *testing.T) { // Redis goes away, connections die, comes back with its data
				mrMu.Lock()
				defer mrMu.Unlock()
				mr.Close()
				time.Sleep(time.Duration(50+rand.IntN(400)) * time.Millisecond)
				if err := mr.Restart(); err != nil {
					t.Logf("restart: %v", err)
				}
			},
		},
		heal:     func() { mr.SetError("") },
		converge: 30 * time.Second,
		lag:      lagFor(5 * time.Second),
		after: func(t *testing.T) {
			for i, c := range clients {
				st := c.PoolStats()
				if st.TotalConns-st.IdleConns > 0 {
					t.Logf("redis client %d: %d conns not idle after Close (%+v)", i, st.TotalConns-st.IdleConns, *st)
				}
			}
		},
	}
}

// pgContainerDSN is the PostgreSQL that pgDSN started for the test running
// (and its subtests), cleared when that test ends with its container.
var pgContainerDSN string

func pgDSN(t *testing.T) string {
	if pgContainerDSN != "" {
		return pgContainerDSN
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()
	var err error
	for try := 1; try <= 3; try++ {
		var c *tcpostgres.PostgresContainer
		c, err = tcpostgres.Run(ctx, "postgres:16", tcpostgres.BasicWaitStrategies())
		testcontainers.CleanupContainer(t, c)
		if err == nil {
			pgContainerDSN, err = c.ConnectionString(ctx, "sslmode=disable")
		}
		if err == nil {
			t.Cleanup(func() { pgContainerDSN = "" }) // the container goes with t
		}
		if err == nil {
			break
		}
	}
	require.NoError(t, err)
	return pgContainerDSN
}

func chaosPostgres(t *testing.T, prefix string) *chaosEnv {
	dsn := pgDSN(t)
	gdb, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sqlDB, err := gdb.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	admin.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = admin.Close() })
	var adminPID int
	require.NoError(t, admin.QueryRow("select pg_backend_pid()").Scan(&adminPID))
	table := "cache_" + prefix
	var dbs []*sql.DB
	e := &chaosEnv{
		name: "postgres",
		newShared: func(t *testing.T) cachex.Backend[int] {
			b := gormcachex.New[int](gormcachex.Config[int]{DB: gdb, TableName: table, Leases: true})
			require.NoError(t, b.Migrate(context.Background()))
			return b
		},
		newBcast: func(t *testing.T) cachex.Broadcast {
			db, err := sql.Open("pgx", dsn)
			require.NoError(t, err)
			dbs = append(dbs, db)
			t.Cleanup(func() { _ = db.Close() })
			b, err := pgbus.New(db)
			require.NoError(t, err)
			t.Cleanup(func() { _ = b.Close() })
			return gobuscachex.NewBroadcast(gobuscachex.BroadcastConfig{
				Bus: b, Subject: "cachex.chaos." + prefix, TTL: 6 * time.Second,
				WorkerConfig: &bus.WorkerConfig{MaxLockPerSecond: 50, MaxBufferJobsCount: 50, MaxPerformPerSecond: 1000, MaxConcurrentPerformCount: 10},
			})
		},
		faults: []func(t *testing.T){
			func(t *testing.T) { // kill every other connection
				var n int
				err := admin.QueryRow(`select count(pg_terminate_backend(pid)) from pg_stat_activity where pid <> $1 and datname = current_database()`, adminPID).Scan(&n)
				if err != nil {
					t.Logf("terminate: %v", err)
				}
			},
		},
		heal:     func() {},
		converge: 120 * time.Second,
		lag:      lagFor(60 * time.Second),
	}
	// cleanup of rotten rows, every second, through go-que
	cdb, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cdb.Close() })
	backend := gormcachex.New[int](gormcachex.Config[int]{DB: gdb, TableName: table, Leases: true})
	require.NoError(t, backend.Migrate(context.Background()))
	cl, err := goquecachex.StartCleanup(context.Background(), goquecachex.CleanupConfig{DB: cdb, Cleaner: backend, Name: prefix, Interval: time.Second})
	require.NoError(t, err)
	e.after = func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := cl.Stop(ctx); err != nil {
			t.Errorf("cleanup Stop: %v", err)
		}
		st := sqlDB.Stats()
		if st.InUse > 0 {
			t.Errorf("gorm pool: %d connections in use after Close", st.InUse)
		}
		for i, db := range dbs {
			if s := db.Stats(); s.InUse > 0 {
				t.Logf("bus db %d: %d in use after Close", i, s.InUse)
			}
		}
	}
	return e
}

func lagFor(d time.Duration) time.Duration {
	if os.Getenv("CHAOS_LAG") == "0" {
		return 0
	}
	return d
}

// ---- the run ----

type chaosCfg struct {
	name               string
	memFresh, memStale time.Duration
	shFresh, shStale   time.Duration
	instances, keys    int
	duration           time.Duration
	broadcastCuts      bool
}

// TestChaos runs both deployments (memory -> Redis with leases, broadcast over
// Redis; memory -> PostgreSQL with leases, broadcast over go-bus, cleanup
// over go-que) with four instances writing and reading a few keys while it
// breaks connections, the bus and the source, and checks the guarantees:
// after a write returns no older value is read or kept, the shared layer
// never goes back, the instances converge, and nothing leaks or hangs. It is
// long, so it runs only with CHAOS=1 (CHAOS_DURATION per run, CHAOS_ENV
// redis|postgres, CHAOS_CFG short|longmem).
func TestChaos(t *testing.T) {
	if os.Getenv("CHAOS") == "" {
		t.Skip("set CHAOS=1")
	}
	dur := 30 * time.Second
	if d, err := time.ParseDuration(os.Getenv("CHAOS_DURATION")); err == nil {
		dur = d
	}
	cfgs := []chaosCfg{
		{name: "short", memFresh: 20 * time.Millisecond, memStale: time.Hour, shFresh: 60 * time.Millisecond, shStale: 300 * time.Millisecond, instances: 4, keys: 5, duration: dur, broadcastCuts: true},
		{name: "longmem", memFresh: time.Hour, memStale: 0, shFresh: 60 * time.Millisecond, shStale: 300 * time.Millisecond, instances: 4, keys: 5, duration: dur, broadcastCuts: true},
	}
	envs := map[string]func(t *testing.T, prefix string) *chaosEnv{"redis": chaosRedis, "postgres": chaosPostgres}
	only := os.Getenv("CHAOS_ENV")
	if only == "" || only == "postgres" {
		pgDSN(t) // one container for the whole test
	}
	for _, en := range []string{"redis", "postgres"} {
		if only != "" && only != en {
			continue
		}
		for _, cfg := range cfgs {
			if c := os.Getenv("CHAOS_CFG"); c != "" && c != cfg.name {
				continue
			}
			t.Run(en+"/"+cfg.name, func(t *testing.T) {
				runChaos(t, envs[en](t, "chaos"+cfg.name+strconv.Itoa(rand.IntN(1e6))), cfg)
			})
		}
	}
}

func runChaos(t *testing.T, e *chaosEnv, cfg chaosCfg) {
	goroutinesBefore := runtime.NumGoroutine()
	keys := make([]string, cfg.keys)
	init := map[string]int{}
	for i := range keys {
		keys[i] = "k" + strconv.Itoa(i)
	}
	b := newBook(keys, cfg.instances)
	b.lag = e.lag
	for _, k := range keys {
		v := int(b.ver.Add(1))
		init[k] = v
	}
	src := &vsource{data: init}
	for _, k := range keys { // initial values are a floor for the shared layer only after a write
		_ = k
	}

	ins := make([]*instance, cfg.instances)
	for i := range ins {
		mem, err := ottercachex.New[int](ottercachex.Config[int]{MaximumSize: 10_000})
		require.NoError(t, err)
		br := newSwitchable(e.newBcast(t))
		c := cachex.New[int](src, []cachex.Layer[int]{
			cachex.NewLayer[int](mem, cachex.TTL(cfg.memFresh, cfg.memStale), cachex.NotFoundTTL(cfg.memFresh, cfg.memStale)),
			cachex.NewLayer(e.newShared(t), cachex.TTL(cfg.shFresh, cfg.shStale), cachex.NotFoundTTL(cfg.shFresh, cfg.shStale)),
		}, cachex.WithBroadcast(br), cachex.WithFetchTimeout(3*time.Second))
		ins[i] = &instance{Cache: c, mem: mem, br: br}
	}
	for _, in := range ins {
		in.br.waitReady(t)
	}
	sharedView := e.newShared(t)

	ctx := context.Background()
	stop := make(chan struct{})
	stopped := func() bool {
		select {
		case <-stop:
			return true
		default:
			return false
		}
	}
	var wg sync.WaitGroup

	// readers
	for i, in := range ins {
		for r := range 3 {
			wg.Go(func() {
				who := fmt.Sprintf("instance %d reader %d", i, r)
				for !stopped() {
					rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
					if rand.IntN(4) == 0 {
						ks := []string{keys[rand.IntN(len(keys))], keys[rand.IntN(len(keys))], keys[rand.IntN(len(keys))]}
						fs := map[string]int64{}
						for _, k := range ks {
							fs[k] = b.floors[i][k].Load()
						}
						got, err := in.GetMany(rctx, ks)
						var be *cachex.BatchError
						if err != nil && !errors.As(err, &be) {
							b.stats.readErrs.Add(1)
						} else {
							for _, k := range ks {
								if err != nil && be.Errors[k] != nil {
									b.stats.readErrs.Add(1)
									continue
								}
								v, ok := got[k]
								b.check(who+" GetMany", k, fs[k], v, ok)
							}
						}
					} else {
						k := keys[rand.IntN(len(keys))]
						f := b.floors[i][k].Load()
						lf := b.lagFloor(k)
						v, err := in.Get(rctx, k)
						switch {
						case err == nil:
							b.check(who, k, f, v, true)
							b.checkLag(who, k, lf, v, true)
						case errors.Is(err, cachex.ErrNotFound):
							b.check(who, k, f, 0, false)
							b.checkLag(who, k, lf, 0, false)
						default:
							b.stats.readErrs.Add(1)
						}
					}
					cancel()
					b.stats.reads.Add(1)
				}
			})
		}
	}

	// writers: per key serialized; source first, then the cache, retried until it succeeds
	for w := range 4 {
		wg.Go(func() {
			for !stopped() {
				i := rand.IntN(len(ins))
				in := ins[i]
				var ks []string
				if rand.IntN(5) == 0 {
					a, c := rand.IntN(len(keys)), rand.IntN(len(keys))
					if a == c {
						continue
					}
					ks = []string{keys[min(a, c)], keys[max(a, c)]}
				} else {
					ks = []string{keys[rand.IntN(len(keys))]}
				}
				del := rand.IntN(4) == 0
				for _, k := range ks {
					b.keyMu[k].Lock()
				}
				vals := map[string]int{}
				for _, k := range ks {
					v := b.ver.Add(1)
					vals[k] = int(v)
					if del {
						b.delsMu.Lock()
						b.dels[k] = append(b.dels[k], v)
						b.delsMu.Unlock()
					}
					src.put(k, int(v), del)
				}
				for attempt := 0; ; attempt++ {
					wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
					var err error
					switch {
					case len(ks) == 1 && del:
						err = in.Del(wctx, ks[0])
					case len(ks) == 1:
						err = in.Set(wctx, ks[0], vals[ks[0]])
					case del:
						err = in.DelMany(wctx, ks)
					default:
						err = in.SetMany(wctx, vals)
					}
					cancel()
					b.stats.writes.Add(1)
					if err == nil {
						for _, k := range ks {
							p := pack(int64(vals[k]), del)
							storeMax(b.floors[i][k], p)
							storeMax(b.shared[k], p)
							b.histMu.Lock()
							b.hist[k] = append(b.hist[k], done{p, time.Now()})
							b.histMu.Unlock()
						}
						break
					}
					b.stats.writeErrs.Add(1)
					if attempt > 200 {
						b.fail("writer %d: %v never succeeded: %v", w, ks, err)
						break
					}
					time.Sleep(time.Duration(10+rand.IntN(40)) * time.Millisecond)
				}
				for _, k := range slices.Backward(ks) {
					b.keyMu[k].Unlock()
				}
				time.Sleep(time.Duration(rand.IntN(3)) * time.Millisecond)
			}
		})
	}

	// shared layer poller: never older than the last successful write before the poll
	wg.Go(func() {
		for !stopped() {
			k := keys[rand.IntN(len(keys))]
			f := b.shared[k].Load()
			pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			en, ok, err := sharedView.Get(pctx, k)
			cancel()
			if err == nil && ok && !en.NotFound {
				fv, fdel := unpack(f)
				if int64(en.Value) < fv || (int64(en.Value) == fv && fdel) {
					b.fail("shared layer: %s holds %d, older than the last write %d (del=%v)", k, en.Value, fv, fdel)
				}
			}
			time.Sleep(time.Millisecond)
		}
	})

	// faults
	faults := slices.Clone(e.faults)
	faults = append(faults, func(t *testing.T) {
		src.slow.Store(true)
		time.Sleep(time.Duration(200+rand.IntN(800)) * time.Millisecond)
		src.slow.Store(false)
	})
	if cfg.broadcastCuts {
		faults = append(faults, func(t *testing.T) {
			in := ins[rand.IntN(len(ins))]
			in.br.cut()
			time.Sleep(time.Duration(50+rand.IntN(400)) * time.Millisecond)
			in.br.restore()
		})
	}
	faultsDone := make(chan struct{})
	go func() {
		defer close(faultsDone)
		for !stopped() {
			select {
			case <-stop:
				return
			case <-time.After(time.Duration(200+rand.IntN(600)) * time.Millisecond):
			}
			b.stats.faults.Add(1)
			faults[rand.IntN(len(faults))](t)
		}
	}()

	time.Sleep(cfg.duration)
	close(stop)
	<-faultsDone
	e.heal()
	waitDone(t, &wg, 60*time.Second, "readers and writers")
	t.Logf("reads %d (errors %d), writes %d (errors %d), faults %d, source calls %d",
		b.stats.reads.Load(), b.stats.readErrs.Load(), b.stats.writes.Load(), b.stats.writeErrs.Load(), b.stats.faults.Load(), src.calls.Load())

	// convergence: every instance reads what the source has
	want := src.snapshot()
	deadline := time.Now().Add(e.converge)
	for i, in := range ins {
		for _, k := range keys {
			for {
				v, err := in.Get(ctx, k)
				wv, exists := want[k]
				okNow := (exists && err == nil && v == wv) || (!exists && errors.Is(err, cachex.ErrNotFound))
				if okNow {
					break
				}
				if time.Now().After(deadline) {
					b.fail("instance %d did not converge on %s: got %d, %v; source has %d (exists %v)", i, k, v, err, wv, exists)
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
	}

	for i, in := range ins {
		done := make(chan struct{})
		go func() { _ = in.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			buf := make([]byte, 1<<20)
			t.Fatalf("instance %d: Close did not return\n%s", i, buf[:runtime.Stack(buf, true)])
		}
	}
	// shared layer after Close: never older than the source
	for _, k := range keys {
		en, ok, err := sharedView.Get(ctx, k)
		if err == nil && ok && !en.NotFound {
			wv, exists := want[k]
			if !exists || en.Value != wv {
				b.fail("shared layer after Close: %s holds %d; source has %d (exists %v)", k, en.Value, wv, exists)
			}
		}
	}
	if e.after != nil {
		e.after(t)
	}
	close(b.failures)
	n := 0
	for f := range b.failures {
		if n < 30 {
			t.Error(f)
		}
		n++
	}
	if n > 0 {
		t.Errorf("%d violations (%d of them lag)", n, b.lags.Load())
	}
	time.Sleep(2 * time.Second)
	if g := runtime.NumGoroutine(); g > goroutinesBefore+50 {
		buf := make([]byte, 1<<20)
		t.Logf("goroutines: %d before, %d after\n%s", goroutinesBefore, g, buf[:runtime.Stack(buf, true)])
	}
}

func waitDone(t *testing.T, wg *sync.WaitGroup, d time.Duration, what string) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		buf := make([]byte, 4<<20)
		t.Fatalf("%s did not stop within %v (deadlock?)\n%s", what, d, buf[:runtime.Stack(buf, true)])
	}
}

// A write on A right after the database dropped every connection succeeds,
// and its publish goes first over a dead pooled connection: the Cache
// publishes again, so B, whose subscription survives (it is not cleared),
// still drops its old value.
func TestAWriteRightAfterTheDatabaseDroppedEveryConnectionReachesTheOthers(t *testing.T) {
	e := chaosPostgres(t, "lost"+strconv.Itoa(rand.IntN(1e6)))
	ctx := context.Background()
	src := &vsource{data: map[string]int{"k": 1}}
	var ins []*cachex.Cache[int]
	var brs []*switchable
	for range 2 {
		mem, err := ottercachex.New[int](ottercachex.Config[int]{MaximumSize: 100})
		require.NoError(t, err)
		br := newSwitchable(e.newBcast(t))
		c := cachex.New[int](src, []cachex.Layer[int]{
			cachex.NewLayer[int](mem, cachex.TTL(time.Hour, 0)),
			cachex.NewLayer(e.newShared(t), cachex.TTL(time.Hour, 0)),
		}, cachex.WithBroadcast(br))
		t.Cleanup(func() { _ = c.Close() })
		ins, brs = append(ins, c), append(brs, br)
	}
	for _, br := range brs {
		br.waitReady(t)
	}
	a, b := ins[0], ins[1]
	v, err := b.Get(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, 1, v)
	time.Sleep(2 * time.Second) // pooled connections idle
	e.faults[0](t)              // pg_terminate_backend on every other connection
	time.Sleep(500 * time.Millisecond)

	src.put("k", 2, false)
	// the write itself may hit a dead pooled connection too: it fails and
	// says so, and is tried again, as an application would
	require.Eventually(t, func() bool { return a.Set(ctx, "k", 2) == nil }, 10*time.Second, 50*time.Millisecond)
	ready := 0
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-brs[1].ready:
			ready++
		default:
		}
		v, err = b.Get(ctx, "k")
		if err == nil && v == 2 {
			t.Logf("B converged (resubscribed %d times)", ready)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("20s after A's write B still reads %d (%v); B resubscribed %d times", v, err, ready)
}
