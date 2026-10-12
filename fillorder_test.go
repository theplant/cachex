package cachex_test

import (
	"context"
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
	assert.Equal(t, "v1", <-got, "the stale copy is still a valid answer")
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
