package cachex_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

// keysInStripeOrder returns n keys whose stripes are distinct and increasing,
// the order a batch write locks them in.
func keysInStripeOrder[T any](c *cachex.Cache[T], n int) []string {
	var keys []string
	last := -1
	for i := 0; len(keys) < n; i++ {
		key := fmt.Sprintf("k%d", i)
		if idx := cachex.StripeIndex(c, key); idx > last {
			keys = append(keys, key)
			last = idx
		}
	}
	return keys
}

func TestAGetManyBackfillSkipsTheKeysWrittenDuringItsFetch(t *testing.T) {
	ctx := context.Background()
	src := newBatchSource(map[string]string{"a": "1", "b": "1"})
	src.gate = make(chan struct{})
	calls, onGet := started()
	src.onGet = onGet
	mem := cachextest.NewMap[string]()
	c := cachex.New[string](src, oneLayer(mem))

	got := make(chan map[string]string)
	go func() {
		m, err := c.GetMany(ctx, []string{"a", "b"})
		assert.NoError(t, err)
		got <- m
	}()
	<-calls
	<-calls
	require.NoError(t, c.Set(ctx, "a", "new"))
	close(src.gate)
	assert.Equal(t, map[string]string{"a": "1", "b": "1"}, <-got)
	cachex.Settle(c)

	e, ok := stored(t, mem, "a")
	require.True(t, ok)
	assert.Equal(t, "new", e.Value, "the fetch read a before the write: its backfill of a is skipped")
	e, ok = stored(t, mem, "b")
	require.True(t, ok)
	assert.Equal(t, "1", e.Value, "b was not written: it is backfilled")
}

func TestABackfillDoesNotWaitForAWriteInProgress(t *testing.T) {
	ctx := context.Background()
	mem := cachextest.NewMap[string]()
	bk := newBlocker("a")
	c := cachex.New(newSource(map[string]string{"a": "1"}), oneLayer(bk.wrap(mem)))

	written := make(chan error)
	go func() { written <- c.Set(ctx, "a", "new") }()
	<-bk.entered // the write holds a's stripe

	v, err := c.Get(ctx, "a")
	require.NoError(t, err)
	assert.Equal(t, "1", v, "the layer has nothing yet: the read asks the source")
	cachex.Settle(c) // returns while the write still holds the stripe: the backfill did not wait
	assert.Zero(t, mem.Len(), "and did not land")

	close(bk.release)
	require.NoError(t, <-written)
	e, ok := stored(t, mem, "a")
	require.True(t, ok)
	assert.Equal(t, "new", e.Value)
}

func TestASetManyThatGivesUpInvalidatesEveryKey(t *testing.T) {
	ctx := context.Background()
	mem := cachextest.NewMap[string]()
	var dels atomic.Int64
	bk := newBlocker("-") // a key never written, until it is set below
	c := cachex.New(newSource(nil), oneLayer(bk.wrap(counting(mem, "del", &dels))))
	keys := keysInStripeOrder(c, 3) // the batch takes keys[0], keys[1], then waits on keys[2]
	for _, key := range keys {
		require.NoError(t, mem.Set(ctx, key, cachex.Entry[string]{Value: "old", ExpiresAt: time.Now().Add(time.Hour)}))
	}
	bk.key = keys[2]

	first := make(chan error)
	go func() { first <- c.Set(ctx, keys[2], "first") }()
	<-bk.entered

	waitCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	err := c.SetMany(waitCtx, map[string]string{keys[0]: "x", keys[1]: "x", keys[2]: "x"})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	for _, key := range keys[:2] {
		_, ok := stored(t, mem, key)
		assert.False(t, ok, "%s: the caller may have changed the source already", key)
	}
	assert.EqualValues(t, 1, dels.Load(), "the stripes it held are invalidated in one call per layer")

	close(bk.release)
	require.NoError(t, <-first)
	require.Eventually(t, func() bool { return mem.Len() == 0 }, time.Second, time.Millisecond,
		"the stripe it gave up on is invalidated once free")
}

func TestPanicsInBatchFetchesAnswerEveryWaiter(t *testing.T) {
	ctx := context.Background()
	t.Run("a chunk of the source", func(t *testing.T) {
		src := newBatchSource(map[string]string{"a": "1", "b": "1", "c": "1"})
		src.onGet = func(_ context.Context, key string) {
			if key == "b" {
				panic("kaboom")
			}
		}
		var log logs
		c := cachex.New[string](src, oneLayer(cachextest.NewMap[string]()), cachex.WithGetManyChunkSize(1), log.option())
		m, err := c.GetMany(ctx, []string{"a", "b", "c"})
		assert.Equal(t, map[string]string{"a": "1", "c": "1"}, m)
		var be *cachex.BatchError
		require.ErrorAs(t, err, &be)
		assert.ErrorContains(t, be.Errors["b"], "panic during fetch: kaboom")
		assert.Len(t, be.Errors, 1)
	})
	t.Run("a lower layer", func(t *testing.T) {
		src := newSource(map[string]string{"a": "1", "b": "1"})
		c := cachex.New[string](src, []cachex.Layer[string]{
			cachex.NewLayer(cachextest.NewMap[string](), cachex.TTL(time.Minute, 0)),
			cachex.NewLayer[string](hook(cachextest.NewMap[string](), func(op string, _ []string, _ bool) {
				if op == "get" {
					panic("kaboom")
				}
			}), cachex.TTL(time.Minute, 0)),
		}, (&logs{}).option())
		var wg sync.WaitGroup
		for range 5 {
			wg.Go(func() {
				_, err := c.GetMany(ctx, []string{"a", "b"})
				var be *cachex.BatchError
				if assert.ErrorAs(t, err, &be) {
					assert.Len(t, be.Errors, 2)
				}
			})
		}
		wg.Wait()
	})
}

func TestAFirstLayerHitDoesNotAllocate(t *testing.T) {
	ctx := context.Background()
	c := cachex.New(newSource(map[string]string{"a": "1"}), oneLayer(cachextest.NewMap[string]()))
	_, err := c.Get(ctx, "a")
	require.NoError(t, err)
	cachex.Settle(c)
	allocs := testing.AllocsPerRun(100, func() { _, _ = c.Get(ctx, "a") })
	assert.Zero(t, allocs)
}

// gatedGet holds the first read of a backend until release.
type gatedGet struct {
	first   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (g *gatedGet) gate() {
	if g.first.CompareAndSwap(false, true) {
		close(g.entered)
		<-g.release
	}
}

func (g *gatedGet) wrap(b cachex.Backend[string]) cachex.Backend[string] {
	return hook(b, func(op string, _ []string, _ bool) {
		if op == "get" {
			g.gate()
		}
	})
}

func TestARefreshDoesNotJoinAFetchThatAcceptsAStaleEntry(t *testing.T) {
	ctx := context.Background()
	clock, now := newClock()
	src := newSource(map[string]string{"a": "new"})
	m0, m1 := cachextest.NewMap[string](), cachextest.NewMap[string]()
	g1 := &gatedGet{entered: make(chan struct{}), release: make(chan struct{})}
	c := cachex.New[string](src, []cachex.Layer[string]{
		cachex.NewLayer(m0, cachex.TTL(time.Minute, time.Hour), cachex.Jitter(0)),
		cachex.NewLayer[string](g1.wrap(m1), cachex.TTL(time.Minute, time.Hour), cachex.Jitter(0)),
	}, now)
	staleEntry := cachex.Entry[string]{Value: "old", CachedAt: epoch, FreshUntil: epoch.Add(time.Minute), ExpiresAt: epoch.Add(time.Hour)}
	require.NoError(t, m1.Set(ctx, "a", staleEntry))
	clock.Advance(2 * time.Minute)

	got := make(chan string)
	go func() { // layer 0 is empty: a fetch reads layer 1, where a is stale
		v, err := c.Get(ctx, "a")
		assert.NoError(t, err)
		got <- v
	}()
	<-g1.entered

	// another process sharing layer 0 copies the stale entry there
	require.NoError(t, m0.Set(ctx, "a", staleEntry))
	v, err := c.Get(ctx, "a")
	require.NoError(t, err)
	assert.Equal(t, "old", v, "served stale; a refresh starts")
	require.Eventually(t, func() bool { return src.calls.Load() == 1 }, time.Second, time.Millisecond,
		"the refresh asks the source instead of taking the stale entry of the fetch in progress")
	// the source being asked is not the refill being done: wait for that too
	require.Eventually(t, func() bool { e, _, _ := m1.Get(ctx, "a"); return e.Value == "new" }, time.Second, time.Millisecond,
		"the refresh refills layer 1")

	close(g1.release)
	assert.Equal(t, "new", <-got, "the first fetch reads layer 1 after the refresh refilled it")
	require.NoError(t, c.Close())
}
