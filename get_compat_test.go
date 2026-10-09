package cachex

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file pin the observable behavior of the single-key path
// (Get/Set/Del, singleflight, fetch timeout, panics, ctx cancellation, test
// hooks, double-check, not-found cache, serve-stale). They use only the API
// that existed before GetMany and pass unchanged against the
// x/sync/singleflight implementation, so any drift in the single-key path
// shows up here.

// hookCounts counts every test hook call of one client.
type hookCounts struct {
	before, start, end atomic.Int64
}

func (h *hookCounts) install(c interface{ setHooks(*testHooks) }, before func(ctx context.Context, key string)) {
	c.setHooks(&testHooks{
		beforeSingleflightStart: func(ctx context.Context, key string) {
			h.before.Add(1)
			if before != nil {
				before(ctx, key)
			}
		},
		afterSingleflightStart: func(context.Context, string) { h.start.Add(1) },
		afterSingleflightEnd:   func(context.Context, string) { h.end.Add(1) },
	})
}

func (c *Client[T]) setHooks(h *testHooks) { c.testHooks = h }

// gate is an upstream that blocks every call until released.
type gate struct {
	calls   atomic.Int64
	entered chan struct{}
	release chan struct{}
}

func newGate() *gate {
	return &gate{entered: make(chan struct{}, 1000), release: make(chan struct{})}
}

func (g *gate) upstream(fn func(ctx context.Context, key string) (string, error)) UpstreamFunc[string] {
	return func(ctx context.Context, key string) (string, error) {
		g.calls.Add(1)
		g.entered <- struct{}{}
		<-g.release
		return fn(ctx, key)
	}
}

func getConcurrently(ctx context.Context, cli *Client[string], key string, n int) ([]string, []error) {
	values := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			values[i], errs[i] = cli.Get(ctx, key)
		}()
	}
	wg.Wait()
	return values, errs
}

func TestGetCompatConcurrentMissesCoalesce(t *testing.T) {
	ctx := context.Background()
	g := newGate()
	cli := NewClient(NewSyncMap[string](), g.upstream(func(_ context.Context, key string) (string, error) {
		return "v-" + key, nil
	}))
	var hooks hookCounts
	hooks.install(cli, nil)

	go func() {
		<-g.entered
		assert.Eventually(t, func() bool { return hooks.before.Load() == 20 }, time.Second, time.Millisecond)
		time.Sleep(50 * time.Millisecond) // let every caller join the flight
		close(g.release)
	}()
	values, errs := getConcurrently(ctx, cli, "k", 20)

	for i := range values {
		require.NoError(t, errs[i])
		assert.Equal(t, "v-k", values[i])
	}
	assert.Equal(t, int64(1), g.calls.Load(), "20 concurrent misses fetch once")
	assert.Equal(t, int64(20), hooks.before.Load(), "beforeSingleflightStart runs once per caller")
	assert.Equal(t, int64(1), hooks.start.Load(), "afterSingleflightStart runs once per fetch")
	assert.Equal(t, int64(20), hooks.end.Load(), "afterSingleflightEnd runs once per caller that got the result")

	v, err := cli.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "v-k", v)
	assert.Equal(t, int64(1), g.calls.Load(), "the fetched value was written to the backend")
	assert.Equal(t, int64(20), hooks.before.Load(), "a hit does not reach the fetch path")
}

func TestGetCompatUpstreamErrorIsSharedAndNotCached(t *testing.T) {
	ctx := context.Background()
	g := newGate()
	boom := errors.New("boom")
	backend := NewSyncMap[string]()
	cli := NewClient(backend, g.upstream(func(context.Context, string) (string, error) { return "", boom }))

	go func() {
		<-g.entered
		time.Sleep(50 * time.Millisecond)
		close(g.release)
	}()
	_, errs := getConcurrently(ctx, cli, "k", 5)
	for _, err := range errs {
		require.Error(t, err)
		assert.ErrorIs(t, err, boom)
		assert.Equal(t, "get from upstream failed for key: k: boom", err.Error())
	}
	assert.Equal(t, int64(1), g.calls.Load())

	_, err := backend.Get(ctx, "k")
	assert.True(t, IsErrKeyNotFound(err), "an upstream error writes nothing")

	_, err = cli.Get(ctx, "k")
	require.Error(t, err)
	assert.Equal(t, int64(2), g.calls.Load(), "errors are not cached: the next Get fetches again")
}

func TestGetCompatUpstreamPanic(t *testing.T) {
	ctx := context.Background()
	var logBuf bytes.Buffer
	var calls atomic.Int64
	entered := make(chan struct{}, 10)
	release := make(chan struct{})
	backend := NewSyncMap[string]()
	cli := NewClient(backend, UpstreamFunc[string](func(context.Context, string) (string, error) {
		if calls.Add(1) == 1 {
			entered <- struct{}{}
			<-release
			panic("kaboom")
		}
		return "ok", nil
	}), WithLogger[string](slog.New(slog.NewTextHandler(&logBuf, nil))))
	var hooks hookCounts
	hooks.install(cli, nil)

	go func() {
		<-entered
		assert.Eventually(t, func() bool { return hooks.before.Load() == 5 }, time.Second, time.Millisecond)
		time.Sleep(50 * time.Millisecond)
		close(release)
	}()
	_, errs := getConcurrently(ctx, cli, "k", 5)
	for _, err := range errs {
		require.Error(t, err)
		assert.Equal(t, "panic during upstream fetch: kaboom", err.Error(), "every waiter gets the panic as an error")
	}
	assert.Equal(t, int64(5), hooks.end.Load())
	assert.Contains(t, logBuf.String(), `msg="panic during upstream fetch" key=k panic=kaboom stack=`)

	v, err := cli.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "ok", v, "the key is released after a panic")
	assert.Equal(t, int64(2), calls.Load())
}

func TestGetCompatUpstreamGoexit(t *testing.T) {
	var calls atomic.Int64
	cli := NewClient(NewSyncMap[string](), UpstreamFunc[string](func(context.Context, string) (string, error) {
		if calls.Add(1) == 1 {
			runtime.Goexit()
		}
		return "ok", nil
	}))

	// unlike main (x/sync's DoChan never answered its waiters then), the fetch
	// publishes an error, so a waiter without a deadline does not hang
	_, err := cli.Get(context.Background(), "k")
	require.Error(t, err)
	assert.Equal(t, "upstream fetch exited without returning (runtime.Goexit)", err.Error())

	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	v, err := cli.Get(ctx2, "k")
	require.NoError(t, err, "the key is released, so the next Get starts a new fetch")
	assert.Equal(t, "ok", v)
	assert.Equal(t, int64(2), calls.Load())
}

func TestGetCompatFetchTimeout(t *testing.T) {
	ctx := context.Background()
	var upstreamCtxErr atomic.Value
	cli := NewClient(NewSyncMap[string](), UpstreamFunc[string](func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		upstreamCtxErr.Store(ctx.Err())
		return "", ctx.Err()
	}), WithFetchTimeout[string](30*time.Millisecond))

	start := time.Now()
	_, err := cli.Get(ctx, "k")
	require.Error(t, err)
	assert.Less(t, time.Since(start), time.Second)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, "get from upstream failed for key: k: context deadline exceeded", err.Error())
	assert.Equal(t, context.DeadlineExceeded, upstreamCtxErr.Load())
}

func TestGetCompatCallerCancelLeavesFetchRunning(t *testing.T) {
	g := newGate()
	var upstreamCtxErr atomic.Value
	backend := NewSyncMap[string]()
	cli := NewClient(backend, g.upstream(func(ctx context.Context, key string) (string, error) {
		upstreamCtxErr.Store(errors.New("none"))
		if ctx.Err() != nil {
			upstreamCtxErr.Store(ctx.Err())
		}
		return "v-" + key, nil
	}))
	var hooks hookCounts
	hooks.install(cli, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-g.entered
		cancel()
	}()
	_, err := cli.Get(ctx, "k")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, "context cancelled during fetch for key: k: context canceled", err.Error())
	assert.Equal(t, int64(0), hooks.end.Load(), "afterSingleflightEnd only runs when the result is received")

	close(g.release)
	require.Eventually(t, func() bool {
		v, err := backend.Get(context.Background(), "k")
		return err == nil && v == "v-k"
	}, time.Second, time.Millisecond, "the fetch outlives the cancelled caller and fills the backend")
	assert.Equal(t, "none", upstreamCtxErr.Load().(error).Error(), "the upstream ctx is detached from the caller's cancellation")

	v, err := cli.Get(context.Background(), "k")
	require.NoError(t, err)
	assert.Equal(t, "v-k", v)
	assert.Equal(t, int64(1), g.calls.Load())
}

func TestGetCompatFetchConcurrencySlots(t *testing.T) {
	ctx := context.Background()
	g := newGate()
	cli := NewClient(NewSyncMap[string](), g.upstream(func(_ context.Context, key string) (string, error) {
		return "v-" + key, nil
	}), WithFetchConcurrency[string](3))
	var hooks hookCounts
	hooks.install(cli, nil)

	go func() {
		<-g.entered
		assert.Eventually(t, func() bool { return hooks.before.Load() == 60 }, time.Second, time.Millisecond)
		time.Sleep(50 * time.Millisecond)
		close(g.release)
	}()
	values, errs := getConcurrently(ctx, cli, "k", 60)
	for i := range values {
		require.NoError(t, errs[i])
		assert.Equal(t, "v-k", values[i])
	}
	assert.GreaterOrEqual(t, g.calls.Load(), int64(1))
	assert.LessOrEqual(t, g.calls.Load(), int64(3), "at most one fetch per slot")
	assert.Equal(t, g.calls.Load(), hooks.start.Load(), "one afterSingleflightStart per slot that fetched")
}

func TestGetCompatDoubleCheckRaceWindow(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mode  DoubleCheckMode
		calls int64
	}{
		{"enabled: the late request reuses the value just written", DoubleCheckEnabled, 1},
		{"disabled: the late request fetches again", DoubleCheckDisabled, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			var calls atomic.Int64
			cli := NewClient(NewSyncMap[string](), UpstreamFunc[string](func(_ context.Context, key string) (string, error) {
				calls.Add(1)
				return "v-" + key, nil
			}), WithDoubleCheck[string](tc.mode))

			lateMissed := make(chan struct{})
			earlyDone := make(chan struct{})
			var first atomic.Bool
			var hooks hookCounts
			hooks.install(cli, func(context.Context, string) {
				if first.CompareAndSwap(false, true) {
					close(lateMissed)
					<-earlyDone // the late request missed the cache, then waits out the early one
				}
			})

			var late string
			var lateErr error
			done := make(chan struct{})
			go func() {
				defer close(done)
				late, lateErr = cli.Get(ctx, "k")
			}()
			<-lateMissed
			early, err := cli.Get(ctx, "k")
			require.NoError(t, err)
			close(earlyDone)
			<-done

			require.NoError(t, lateErr)
			assert.Equal(t, "v-k", early)
			assert.Equal(t, "v-k", late)
			assert.Equal(t, tc.calls, calls.Load())
			assert.Equal(t, int64(2), hooks.start.Load(), "two separate flights either way")
		})
	}
}

func TestGetCompatNotFoundCache(t *testing.T) {
	ctx := context.Background()
	clock := NewMockClock(time.Now())
	defer clock.Install()()

	var calls atomic.Int64
	cli := NewClient(NewSyncMap[string](), UpstreamFunc[string](func(context.Context, string) (string, error) {
		calls.Add(1)
		return "", &ErrKeyNotFound{}
	}),
		NotFoundWithTTL[string](NewSyncMap[time.Time](), 100*time.Millisecond, 500*time.Millisecond),
		WithServeStale[string](true),
	)

	type step struct {
		advance time.Duration
		msg     string
		cached  bool
		state   State
		calls   int64
	}
	for i, s := range []step{
		{0, "get from upstream failed for key: nf: key not found", false, StateFresh, 1},
		{0, "key not found in cache for key: nf: key not found (cached, fresh)", true, StateFresh, 1},
		{150 * time.Millisecond, "key not found in cache for key: nf: key not found (cached, stale)", true, StateStale, 1},
		{600 * time.Millisecond, "get from upstream failed for key: nf: key not found", false, StateFresh, 3},
	} {
		clock.Advance(s.advance)
		_, err := cli.Get(ctx, "nf")
		var e *ErrKeyNotFound
		require.ErrorAs(t, err, &e, "step %d", i)
		assert.Equal(t, s.msg, err.Error(), "step %d", i)
		assert.Equal(t, s.cached, e.Cached, "step %d", i)
		if s.cached {
			assert.Equal(t, s.state, e.CacheState, "step %d", i)
		}
		waitAsyncRefreshDone(t, cli)
		if i == 2 {
			assert.Equal(t, int64(2), calls.Load(), "a stale not-found is served and refreshed in the background")
		} else {
			assert.Equal(t, s.calls, calls.Load(), "step %d", i)
		}
	}

	require.NoError(t, cli.Set(ctx, "nf", "now-exists"))
	v, err := cli.Get(ctx, "nf")
	require.NoError(t, err, "Set clears the cached not-found")
	assert.Equal(t, "now-exists", v)

	require.NoError(t, cli.Del(ctx, "nf"))
	_, err = cli.Get(ctx, "nf")
	var e *ErrKeyNotFound
	require.ErrorAs(t, err, &e)
	assert.True(t, e.Cached, "Del records a fresh not-found")
	assert.Equal(t, int64(3), calls.Load())
}

func TestGetCompatServeStale(t *testing.T) {
	ctx := context.Background()
	clock := NewMockClock(time.Now())
	defer clock.Install()()

	var calls atomic.Int64
	refreshGate := make(chan struct{})
	cli := NewClient(NewSyncMap[*Entry[string]](), UpstreamFunc[*Entry[string]](func(context.Context, string) (*Entry[string], error) {
		n := calls.Add(1)
		if n == 2 {
			<-refreshGate
		}
		return &Entry[string]{Data: map[int64]string{1: "one", 2: "two"}[n], CachedAt: NowFunc()}, nil
	}),
		EntryWithTTL[string](100*time.Millisecond, 500*time.Millisecond),
		WithServeStale[*Entry[string]](true),
	)

	v, err := cli.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "one", v.Data)

	clock.Advance(150 * time.Millisecond)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			v, err := cli.Get(ctx, "k")
			assert.NoError(t, err)
			assert.Equal(t, "one", v.Data, "stale value is served while refreshing")
		})
	}
	wg.Wait()
	close(refreshGate)
	waitAsyncRefreshDone(t, cli)
	assert.Equal(t, int64(2), calls.Load(), "20 stale reads trigger one background refresh")

	v, err = cli.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "two", v.Data)
	assert.Equal(t, int64(2), calls.Load())
}

// errBackend fails every Get.
type errBackend struct{ Cache[string] }

func (errBackend) Get(context.Context, string) (string, error) { return "", errors.New("disk") }

func TestGetCompatBackendError(t *testing.T) {
	var calls atomic.Int64
	cli := NewClient[string](errBackend{NewSyncMap[string]()}, UpstreamFunc[string](func(context.Context, string) (string, error) {
		calls.Add(1)
		return "v", nil
	}))
	_, err := cli.Get(context.Background(), "k")
	require.Error(t, err)
	assert.Equal(t, "get from backend failed for key: k: disk", err.Error())
	assert.Equal(t, int64(0), calls.Load(), "a backend error does not fall through to the upstream")
}

func TestGetCompatWritePropagation(t *testing.T) {
	ctx := context.Background()
	source := map[string]string{"k": "src"}
	l2Backend := NewSyncMap[string]()
	l2 := NewClient(l2Backend, UpstreamFunc[string](func(_ context.Context, key string) (string, error) {
		if v, ok := source[key]; ok {
			return v, nil
		}
		return "", &ErrKeyNotFound{}
	}))
	l1Backend := NewSyncMap[string]()
	l1 := NewClient[string](l1Backend, l2)

	v, err := l1.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "src", v)
	v, _ = l2Backend.Get(ctx, "k")
	assert.Equal(t, "src", v, "a miss fills every layer on the way back")

	require.NoError(t, l1.Set(ctx, "k", "new"))
	v, _ = l1Backend.Get(ctx, "k")
	assert.Equal(t, "new", v)
	v, _ = l2Backend.Get(ctx, "k")
	assert.Equal(t, "new", v, "Set propagates to the cache layer below")
	assert.Equal(t, "src", source["k"], "and stops at the data source")

	require.NoError(t, l1.Del(ctx, "k"))
	_, err = l1Backend.Get(ctx, "k")
	assert.True(t, IsErrKeyNotFound(err))
	_, err = l2Backend.Get(ctx, "k")
	assert.True(t, IsErrKeyNotFound(err), "Del propagates to the cache layer below")
}
