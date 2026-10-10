package cachex_test

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

func oneLayer(b cachex.Backend[string]) []cachex.Layer[string] {
	return []cachex.Layer[string]{cachex.NewLayer(b, cachex.TTL(time.Minute, 0))}
}

// started returns an onGet hook that reports each source call on the channel.
func started() (chan string, func(context.Context, string)) {
	ch := make(chan string, 100)
	return ch, func(_ context.Context, key string) { ch <- key }
}

func TestConcurrentMissesShareOneFetch(t *testing.T) {
	ctx := context.Background()
	src := newSource(map[string]string{"a": "1"})
	src.gate = make(chan struct{})
	calls, onGet := started()
	src.onGet = onGet
	c := cachex.New(src, oneLayer(cachextest.NewMap[string]()))

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			v, err := c.Get(ctx, "a")
			assert.NoError(t, err)
			assert.Equal(t, "1", v)
		})
	}
	wg.Go(func() {
		m, err := c.GetMany(ctx, []string{"a"})
		assert.NoError(t, err)
		assert.Equal(t, map[string]string{"a": "1"}, m)
	})
	<-calls
	// no need to wait for the others to join: one that comes after the fetch
	// finds the backfill, or double-checks into it
	close(src.gate)
	wg.Wait()
	assert.EqualValues(t, 1, src.calls.Load())
}

func TestFetchesPerKeyBoundsConcurrentFetchesOfAKey(t *testing.T) {
	ctx := context.Background()
	gate := make(chan struct{})
	var running, peak atomic.Int64
	src := cachex.SourceFunc[string](func(context.Context, string) (string, error) {
		n := running.Add(1)
		defer running.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		<-gate
		return "1", nil
	})
	c := cachex.New[string](src, nil, cachex.WithFetchesPerKey(3))
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			_, err := c.Get(ctx, "a")
			assert.NoError(t, err)
		})
	}
	// 50 callers spread over 3 slots fill all of them (all but surely)
	require.Eventually(t, func() bool { return running.Load() == 3 }, time.Second, time.Millisecond)
	close(gate)
	wg.Wait()
	assert.EqualValues(t, 3, peak.Load())
}

func TestAFetchThatPanicsOrExitsAnswersEveryWaiter(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		do   func()
		want string
	}{
		"panic":  {func() { panic("kaboom") }, "panic during fetch: kaboom"},
		"Goexit": {runtime.Goexit, "fetch exited without returning (runtime.Goexit)"},
	} {
		t.Run(name, func(t *testing.T) {
			var broken atomic.Bool
			broken.Store(true)
			src := newSource(map[string]string{"a": "1"})
			src.onGet = func(context.Context, string) {
				if broken.Load() {
					tc.do()
				}
			}
			var log logs
			c := cachex.New(src, oneLayer(cachextest.NewMap[string]()), log.option())
			var wg sync.WaitGroup
			for range 5 {
				wg.Go(func() {
					_, err := c.Get(ctx, "a")
					assert.ErrorContains(t, err, tc.want)
				})
			}
			wg.Wait()
			if name == "panic" {
				assert.Contains(t, log.String(), "panic during fetch")
				assert.Contains(t, log.String(), "stack=")
			}
			broken.Store(false)
			v, err := c.Get(ctx, "a")
			require.NoError(t, err, "the key is fetched anew")
			assert.Equal(t, "1", v)
		})
	}
}

func TestAFetchOutlivesTheCallerThatStartedIt(t *testing.T) {
	src := newSource(map[string]string{"a": "1"})
	src.gate = make(chan struct{})
	calls, onGet := started()
	var fetchCtx context.Context
	src.onGet = func(ctx context.Context, key string) { fetchCtx = ctx; onGet(ctx, key) }
	mem := cachextest.NewMap[string]()
	c := cachex.New(src, oneLayer(mem), cachex.WithFetchTimeout(time.Minute))

	type ctxKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "trace"))
	done := make(chan error)
	go func() {
		_, err := c.Get(ctx, "a")
		done <- err
	}()
	<-calls
	cancel()
	err := <-done
	require.ErrorIs(t, err, context.Canceled)
	assert.ErrorContains(t, err, `context done while fetching "a"`)

	assert.NoError(t, fetchCtx.Err(), "the fetch does not end with its caller")
	assert.Equal(t, "trace", fetchCtx.Value(ctxKey{}), "it keeps the caller's values")
	assert.True(t, cachex.IsShared(fetchCtx), "and is marked as shared")
	assert.False(t, cachex.IsShared(ctx))
	deadline, ok := fetchCtx.Deadline()
	require.True(t, ok)
	assert.WithinDuration(t, time.Now().Add(time.Minute), deadline, 5*time.Second)

	close(src.gate)
	require.Eventually(t, func() bool { return mem.Len() == 1 }, time.Second, time.Millisecond, "and backfills")
}

func TestFetchTimeout(t *testing.T) {
	src := newSource(map[string]string{"a": "1"})
	src.gate = make(chan struct{}) // never opened: the source waits for its ctx
	c := cachex.New(src, oneLayer(cachextest.NewMap[string]()), cachex.WithFetchTimeout(20*time.Millisecond))
	_, err := c.Get(context.Background(), "a")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.ErrorContains(t, err, `get "a" from source`)
}

// slowFirstGet wraps a backend: the first Get of key reads, then waits for
// release before returning what it read.
type slowFirstGet struct {
	cachex.Backend[string]
	once    sync.Once
	read    chan struct{}
	release chan struct{}
}

func (s *slowFirstGet) Get(ctx context.Context, key string) (cachex.Entry[string], bool, error) {
	e, ok, err := s.Backend.Get(ctx, key)
	first := false
	s.once.Do(func() { first = true })
	if first {
		close(s.read)
		<-s.release
	}
	return e, ok, err
}

func TestDoubleCheck(t *testing.T) {
	// A reads a miss; B fetches and backfills the key; only then does A claim
	// the fetch. Re-reading the layer saves A's fetch.
	run := func(t *testing.T, mode cachex.DoubleCheckMode) int64 {
		ctx := context.Background()
		src := newSource(map[string]string{"a": "1"})
		mem := &slowFirstGet{Backend: cachextest.NewMap[string](), read: make(chan struct{}), release: make(chan struct{})}
		c := cachex.New(src, oneLayer(mem), cachex.WithDoubleCheck(mode))
		done := make(chan struct{})
		go func() {
			defer close(done)
			v, err := c.Get(ctx, "a") // A
			assert.NoError(t, err)
			assert.Equal(t, "1", v)
		}()
		<-mem.read
		_, err := c.Get(ctx, "a") // B
		require.NoError(t, err)
		cachex.Settle(c) // B's fetch is backfilled and gone before A claims
		close(mem.release)
		<-done
		return src.calls.Load()
	}
	assert.EqualValues(t, 1, run(t, cachex.DoubleCheckAuto), "auto: this Cache filled the key since A read it")
	assert.EqualValues(t, 1, run(t, cachex.DoubleCheckEnabled))
	assert.EqualValues(t, 2, run(t, cachex.DoubleCheckDisabled))
}

func TestDoubleCheckAutoSkipsTheReReadWhenNothingWasWritten(t *testing.T) {
	src := newSource(map[string]string{"a": "1"})
	var reads atomic.Int64
	mem := &countingGets{Backend: cachextest.NewMap[string](), n: &reads}
	c := cachex.New(src, oneLayer(mem))
	_, err := c.Get(context.Background(), "a")
	require.NoError(t, err)
	assert.EqualValues(t, 1, reads.Load(), "a cold miss reads the layer once")

	c = cachex.New(src, oneLayer(&countingGets{Backend: cachextest.NewMap[string](), n: &reads}), cachex.WithDoubleCheck(cachex.DoubleCheckEnabled))
	reads.Store(0)
	_, err = c.Get(context.Background(), "a")
	require.NoError(t, err)
	assert.EqualValues(t, 2, reads.Load(), "enabled always re-reads")
}

type countingGets struct {
	cachex.Backend[string]
	n *atomic.Int64
}

func (c *countingGets) Get(ctx context.Context, key string) (cachex.Entry[string], bool, error) {
	c.n.Add(1)
	return c.Backend.Get(ctx, key)
}

func (c *countingGets) GetMany(ctx context.Context, keys []string) (map[string]cachex.Entry[string], error) {
	c.n.Add(1)
	return c.Backend.GetMany(ctx, keys)
}
