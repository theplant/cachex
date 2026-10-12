package cachex_test

import (
	"context"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

// pausedAfterRead wraps a Backend: once armed, its next Get, once it has
// read, signals read and waits for resume, so a fetch can be held between
// reading a layer and backfilling what it read.
type pausedAfterRead[T any] struct {
	cachex.Backend[T]
	armed  atomic.Bool
	read   chan struct{}
	resume chan struct{}
}

func (p *pausedAfterRead[T]) Get(ctx context.Context, key string) (cachex.Entry[T], bool, error) {
	e, ok, err := p.Backend.Get(ctx, key)
	if p.armed.CompareAndSwap(true, false) {
		p.read <- struct{}{}
		<-p.resume
	}
	return e, ok, err
}

// writesOf wraps a Backend and records the values written to it, in order.
type writesOf struct {
	cachex.Backend[string]
	mu     sync.Mutex
	values []string
}

func (w *writesOf) Set(ctx context.Context, key string, e cachex.Entry[string]) error {
	w.mu.Lock()
	w.values = append(w.values, e.Value)
	w.mu.Unlock()
	return w.Backend.Set(ctx, key, e)
}

func (w *writesOf) SetMany(ctx context.Context, entries map[string]cachex.Entry[string]) error {
	w.mu.Lock()
	for _, e := range entries {
		w.values = append(w.values, e.Value)
	}
	w.mu.Unlock()
	return w.Backend.SetMany(ctx, entries)
}

func (w *writesOf) written() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.values)
}

func TestAnOlderCopyFromBelowDoesNotReplaceANewerAnswer(t *testing.T) {
	// a read copies a stale v1 up from the second layer; a refresh meanwhile
	// writes v2, the source's newer answer, into both: the copy lands after
	// it and must not bring v1 back
	ctx := context.Background()
	src := newSource(map[string]string{"k": "v1"})
	clock, now := newClock()
	m0 := &writesOf{Backend: cachextest.NewMap[string]()}
	l1 := &pausedAfterRead[string]{Backend: cachextest.NewMap[string](), read: make(chan struct{}), resume: make(chan struct{})}
	c := cachex.New[string](src, []cachex.Layer[string]{
		cachex.NewLayer(m0, cachex.TTL(time.Minute, time.Hour), cachex.Jitter(0)),
		cachex.NewLayer[string](l1, cachex.TTL(time.Minute, time.Hour), cachex.Jitter(0)),
	}, now)
	require.NoError(t, c.Set(ctx, "k", "v1"))
	clock.Advance(2 * time.Minute) // stale in both layers

	src.set("k", "v2") // changed behind the Cache's back
	src.gate = make(chan struct{})
	asked := make(chan struct{}, 1)
	src.onGet = func(context.Context, string) { asked <- struct{}{} }
	v, err := c.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "v1", v, "stale, and refreshed in the background")
	<-asked // the refresh waits in the source

	require.NoError(t, m0.Del(ctx, "k")) // evicted from the first layer
	l1.armed.Store(true)
	got := make(chan string)
	go func() {
		v, _ := c.Get(ctx, "k")
		got <- v
	}()
	<-l1.read // the read holds the stale v1 from the second layer

	close(src.gate)
	require.Eventually(t, func() bool { e, ok := stored(t, m0, "k"); return ok && e.Value == "v2" }, 5*time.Second, time.Millisecond)

	l1.resume <- struct{}{}
	assert.Equal(t, "v2", <-got, "the copy lands after the newer answer and returns it instead")
	require.NoError(t, c.Close()) // the read's backfill, and the refresh it starts
	w := m0.written()
	i := slices.Index(w, "v2")
	require.GreaterOrEqual(t, i, 0)
	assert.NotContains(t, w[i:], "v1", "the older copy did not replace the newer answer: %v", w)
}

func TestAKeyIsAskedOfTheSourceOnceAtATime(t *testing.T) {
	// a refresh is asking the source when a read misses: the read takes the
	// refresh's answer instead of asking again, since two overlapping
	// answers cannot be ordered and the older could land last
	ctx := context.Background()
	var data atomic.Value
	data.Store("v1")
	var calls atomic.Int64
	asked := make(chan struct{}, 2)
	release := make(chan struct{})
	src := cachex.SourceFunc[string](func(context.Context, string) (string, error) {
		snapshot := data.Load().(string)
		if calls.Add(1) == 1 {
			asked <- struct{}{}
			<-release
		}
		return snapshot, nil
	})
	clock, now := newClock()
	m0 := cachextest.NewMap[string]()
	c := cachex.New[string](src, []cachex.Layer[string]{
		cachex.NewLayer(m0, cachex.TTL(time.Minute, time.Hour), cachex.Jitter(0)),
	}, now)
	require.NoError(t, c.Set(ctx, "k", "v0"))
	clock.Advance(2 * time.Minute)

	_, err := c.Get(ctx, "k") // stale: the refresh reads v1 and waits
	require.NoError(t, err)
	<-asked
	data.Store("v2")
	require.NoError(t, m0.Del(ctx, "k")) // evicted: the next read misses
	got := make(chan string)
	go func() {
		v, _ := c.Get(ctx, "k")
		got <- v
	}()
	require.Eventually(t, func() bool { return calls.Load() > 1 || cachex.SourceWaiters(c, "k") > 0 }, 5*time.Second, time.Millisecond)
	assert.Equal(t, int64(1), calls.Load(), "the read did not ask the source again")
	close(release)
	v := <-got
	require.NoError(t, c.Close())
	after, err := c.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, v, after, "what a read returned is not followed by an older value")
}

func TestAReadDoesNotJoinAFlightOlderThanWhatAReadReturned(t *testing.T) {
	// a read copies the stale v1 up from the second layer while a refresh,
	// holding the fill lock, writes the source's v2. Had the read published v1
	// before its backfill, a read that got v2 from the first layer, saw it
	// evicted, and joined the first read's flight would get v1 back. The copy
	// waits for the refresh instead and answers the newer v2 it finds.
	ctx := context.Background()
	src := newSource(map[string]string{"k": "v1"})
	clock, now := newClock()

	var l1SetArmed, l1GetArmed atomic.Bool
	refreshInL1 := make(chan struct{})
	readInL1 := make(chan struct{})
	proceed := make(chan struct{})
	l1 := hook[string](cachextest.NewMap[string](), func(op string, _ []string, _ bool) {
		switch {
		case op == "set" && l1SetArmed.CompareAndSwap(true, false):
			close(refreshInL1) // the refresh's backfill, holding the fill lock
			<-proceed
		case op == "get" && l1GetArmed.CompareAndSwap(true, false):
			close(readInL1) // the read copying v1 up
		}
	})
	m0 := cachextest.NewMap[string]()
	c := cachex.New[string](src, []cachex.Layer[string]{
		cachex.NewLayer[string](m0, cachex.TTL(time.Minute, time.Hour), cachex.Jitter(0)),
		cachex.NewLayer[string](l1, cachex.TTL(time.Minute, time.Hour), cachex.Jitter(0)),
	}, now)
	defer func() { _ = c.Close() }()
	require.NoError(t, c.Set(ctx, "k", "v1"))
	clock.Advance(2 * time.Minute) // stale in both layers
	src.set("k", "v2")             // changed behind the Cache's back

	l1SetArmed.Store(true)
	v, err := c.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "v1", v, "stale, refreshed in the background")
	<-refreshInL1

	require.NoError(t, m0.Del(ctx, "k")) // evicted from the first layer
	l1GetArmed.Store(true)
	got := make(chan string, 1)
	go func() {
		v, err := c.Get(ctx, "k") // copies the stale v1 from the second layer
		assert.NoError(t, err)
		got <- v
	}()
	<-readInL1
	select {
	case v := <-got:
		close(proceed) // so that Close does not wait for the refresh forever
		t.Fatalf("the copy returned %q before the refresh landed", v)
	case <-time.After(10 * time.Millisecond):
	}

	close(proceed) // the refresh lands v2
	assert.Equal(t, "v2", <-got, "the copy answers the newer entry it found")

	require.NoError(t, m0.Del(ctx, "k")) // evicted again
	v, err = c.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "v2", v, "a read after one that returned v2 must not go back")
}

func TestReadsNeverGoBackUnderChurn(t *testing.T) {
	// the source moves on behind the Cache while entries keep turning stale
	// and rotten in three layers: a read that starts after another returned v
	// never returns older than v (ADR 0022)
	if testing.Short() {
		t.Skip("load")
	}
	ctx := context.Background()
	var version atomic.Int64
	src := cachex.SourceFunc[int64](func(context.Context, string) (int64, error) {
		v := version.Load()
		runtime.Gosched() // answers travel back while the source moves on
		return v, nil
	})
	layer := func() cachex.Layer[int64] {
		return cachex.NewLayer[int64](cachextest.NewMap[int64](), cachex.TTL(time.Millisecond, 3*time.Millisecond))
	}
	c := cachex.New[int64](src, []cachex.Layer[int64]{layer(), layer(), layer()})
	defer func() { _ = c.Close() }()

	stop := make(chan struct{})
	var bumps sync.WaitGroup
	bumps.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				version.Add(1)
				time.Sleep(50 * time.Microsecond)
			}
		}
	})
	var returned atomic.Int64 // the newest version a read returned
	var readers sync.WaitGroup
	deadline := time.Now().Add(300 * time.Millisecond)
	for range 8 {
		readers.Go(func() {
			for time.Now().Before(deadline) {
				floor := returned.Load()
				v, err := c.Get(ctx, "k")
				if !assert.NoError(t, err) {
					return
				}
				if v < floor {
					t.Errorf("read %d after a read had returned %d", v, floor)
					return
				}
				for cur := returned.Load(); v > cur && !returned.CompareAndSwap(cur, v); cur = returned.Load() {
				}
			}
		})
	}
	readers.Wait()
	close(stop)
	bumps.Wait()
}
