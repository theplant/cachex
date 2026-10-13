// Reproductions of the reads that can still go back (docs/todo.md #6): they
// fail with the skip removed.
package cachex_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

func TestKnownGapACopyLandsOverANewerAnswerEvictedFromTheFirstLayer(t *testing.T) {
	t.Skip("known gap, docs/todo.md #6")
	// two layers, evictions from the first only: a read copies a stale v1 up
	// from L1; a refresh lands v2 in L1 and L0; a read returns v2; L0's v2 is
	// evicted; the copy lands v1 in L0 (it reads only L0, not L1, which holds
	// v2), and the next read returns v1
	ctx := context.Background()
	src := newSource(map[string]string{"k": "v1"})
	clock, now := newClock()
	m0 := cachextest.NewMap[string]()
	l1 := &pausedAfterRead[string]{Backend: cachextest.NewMap[string](), read: make(chan struct{}), resume: make(chan struct{})}
	c := cachex.New[string](src, []cachex.Layer[string]{
		cachex.NewLayer(m0, cachex.TTL(time.Minute, time.Hour), cachex.Jitter(0)),
		cachex.NewLayer[string](l1, cachex.TTL(time.Minute, time.Hour), cachex.Jitter(0)),
	}, now)
	defer func() { _ = c.Close() }()
	require.NoError(t, c.Set(ctx, "k", "v1"))
	clock.Advance(2 * time.Minute) // stale in both layers

	src.set("k", "v2")
	src.gate = make(chan struct{})
	asked := make(chan struct{}, 1)
	src.onGet = func(context.Context, string) { asked <- struct{}{} }
	v, err := c.Get(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, "v1", v, "stale; the refresh waits in the source")
	<-asked

	require.NoError(t, m0.Del(ctx, "k")) // evicted
	l1.armed.Store(true)
	got := make(chan string, 1)
	go func() {
		v, _ := c.Get(ctx, "k")
		got <- v
	}()
	<-l1.read // holds L1's stale v1

	close(src.gate) // the refresh lands v2
	require.Eventually(t, func() bool { e, ok := stored(t, m0, "k"); return ok && e.Value == "v2" }, 5*time.Second, time.Millisecond)
	v, err = c.Get(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, "v2", v)

	require.NoError(t, m0.Del(ctx, "k")) // evicted again
	l1.resume <- struct{}{}
	l1.armed.Store(true) // holds the refresh the copy starts (it would copy v2 up and heal L0)
	t.Logf("the copying read returned %q", <-got)
	v, err = c.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "v2", v, "a read after one that returned v2 went back")
	<-l1.read
	l1.resume <- struct{}{}
}

func TestKnownGapACopyBringsBackAKeyTheSourceNoLongerHas(t *testing.T) {
	t.Skip("known gap, docs/todo.md #6")
	// L0 local (short-lived), L1 below. A refresh copies v1 up from L1 and is
	// held after reading it. L1's entry is evicted and L0's rots; the source
	// no longer has the key: a read returns ErrNotFound, and, with no
	// NotFoundTTL, leaves nothing in the layers for the copy to compare with.
	// The copy lands v1 in L0, and the next read returns v1.
	ctx := context.Background()
	src := newSource(map[string]string{"k": "v1"})
	clock, now := newClock()
	m0 := cachextest.NewMap[string]()
	m1 := cachextest.NewMap[string]()
	l1 := &pausedAfterRead[string]{Backend: m1, read: make(chan struct{}), resume: make(chan struct{})}
	c := cachex.New[string](src, []cachex.Layer[string]{
		cachex.NewLayer(m0, cachex.TTL(time.Minute, time.Minute), cachex.Jitter(0)),
		cachex.NewLayer[string](l1, cachex.TTL(time.Hour, time.Hour), cachex.Jitter(0)),
	}, now)
	defer func() { _ = c.Close() }()
	require.NoError(t, c.Set(ctx, "k", "v1"))
	clock.Advance(90 * time.Second) // stale in L0, fresh in L1

	l1.armed.Store(true)
	v, err := c.Get(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, "v1", v, "stale; the refresh copies L1's fresh v1")
	<-l1.read

	src.del("k")                         // gone from the source
	require.NoError(t, m1.Del(ctx, "k")) // evicted from L1
	clock.Advance(time.Minute)           // L0's v1 rots
	_, err = c.Get(ctx, "k")
	require.ErrorIs(t, err, cachex.ErrNotFound)

	l1.resume <- struct{}{}
	cachex.Settle(c)
	v, err = c.Get(ctx, "k")
	assert.ErrorIs(t, err, cachex.ErrNotFound, "a read after one that returned not found got %q", v)
}

// stepSource answers from a map; call n (1-based) waits on before[n] before
// reading the map and on after[n] after, if set. started gets n as each call
// starts.
type stepSource struct {
	mu      sync.Mutex
	data    map[string]string
	n       int
	before  map[int]chan struct{}
	after   map[int]chan struct{}
	started chan int
}

func (s *stepSource) Get(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	s.n++
	n := s.n
	b, a := s.before[n], s.after[n]
	s.mu.Unlock()
	s.started <- n
	if b != nil {
		<-b
	}
	s.mu.Lock()
	v := s.data[key]
	s.mu.Unlock()
	if a != nil {
		<-a
	}
	return v, nil
}

func (s *stepSource) set(key, v string) { s.mu.Lock(); s.data[key] = v; s.mu.Unlock() }

func TestKnownGapAPreWriteFetchPublishesNewerThanThePostWriteOne(t *testing.T) {
	t.Skip("known gap, docs/todo.md #7")
	// One layer. A read's fetch (call 1) is held before it reads the source.
	// The source changes to v2 and Set(v2) runs: it drops the fetch's flight
	// and call, so the next read, after v2 is evicted from L0, asks the source
	// again (call 2), which reads v2 and is held. The source then moves on to
	// v3 behind the Cache. Call 1 reads v3; its backfill is skipped (a write
	// counted), answerAsFirst finds L0 empty, and it publishes v3: the first
	// read returns v3. A read after that joins call 2's flight and returns v2.
	ctx := context.Background()
	src := &stepSource{data: map[string]string{"k": "v1"}, before: map[int]chan struct{}{1: make(chan struct{})},
		after: map[int]chan struct{}{2: make(chan struct{})}, started: make(chan int, 10)}
	m0 := cachextest.NewMap[string]()
	c := cachex.New[string](src, []cachex.Layer[string]{
		cachex.NewLayer[string](m0, cachex.TTL(time.Minute, time.Minute), cachex.Jitter(0)),
	})
	defer func() { _ = c.Close() }()

	first := make(chan string, 1)
	go func() {
		v, err := c.Get(ctx, "k")
		assert.NoError(t, err)
		first <- v
	}()
	require.Equal(t, 1, <-src.started)

	src.set("k", "v2")
	require.NoError(t, c.Set(ctx, "k", "v2"))
	require.NoError(t, m0.Del(ctx, "k")) // evicted (or a WithMaxAge of zero for v2)

	second := make(chan string, 1)
	go func() {
		v, err := c.Get(ctx, "k")
		assert.NoError(t, err)
		second <- v
	}()
	require.Equal(t, 2, <-src.started)
	require.Eventually(t, func() bool { return cachex.Sourcing(c, "k") }, time.Second, time.Millisecond)
	time.Sleep(10 * time.Millisecond) // call 2 has read v2 (it holds in after[2])

	src.set("k", "v3") // the source moves on behind the Cache
	close(src.before[1])
	v1 := <-first
	require.Equal(t, "v3", v1, "the read that started before the write returns v3")

	third := make(chan string, 1)
	go func() { // starts after a read returned v3
		v, err := c.Get(ctx, "k")
		assert.NoError(t, err)
		third <- v
	}()
	time.Sleep(10 * time.Millisecond) // joins call 2's flight
	close(src.after[2])
	assert.Equal(t, "v2", <-second)
	assert.Equal(t, v1, <-third, "a read after one that returned %q went back", v1)
}

// zzHookLeasing is a leasing map whose next PutLeased (once putArmed) and
// next Lease (once leaseArmed) signal and wait for resume.
