// Reproductions of the reads that can still go back (docs/todo.md #7): they
// fail with the skip removed.
package cachex_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

type leasePaused struct {
	*cachextest.LeasingMap[string]
	armed  atomic.Bool
	leased chan struct{}
	resume chan struct{}
}

func (l *leasePaused) Leaser() cachex.Leaser[string] { return pausingLeaser{l, l.LeasingMap.Leaser()} }

type pausingLeaser struct {
	l *leasePaused
	cachex.Leaser[string]
}

func (p pausingLeaser) Lease(ctx context.Context, keys []string, ttl time.Duration) (map[string]string, error) {
	toks, err := p.Leaser.Lease(ctx, keys, ttl)
	if p.l.armed.CompareAndSwap(true, false) {
		p.l.leased <- struct{}{}
		<-p.l.resume
	}
	return toks, err
}

func TestALeaseEndedByAnotherFetchLeavesNoOlderAnswer(t *testing.T) {
	// L0 local, L1 leasing. A refresh leases L1 and pauses before the source.
	// A read refused L1's lease asks the source (v2) and deletes L1's older,
	// rotten entry: a write, which ends the refresh's lease. A second read
	// leases L1 and lands v2 there. The refresh then asks the source (v3):
	// its PutLeased is refused silently, L0 gets v3, L1 keeps v2. Once L0's
	// entry goes, a read returns v2 after one returned v3.
	ctx := context.Background()
	src := newSource(map[string]string{"k": "v2"})
	clock, now := newClock()
	m0 := cachextest.NewMap[string]()
	l1 := &leasePaused{LeasingMap: cachextest.NewLeasingMap[string](), leased: make(chan struct{}), resume: make(chan struct{})}
	c := cachex.New[string](src, []cachex.Layer[string]{
		cachex.NewLayer[string](m0, cachex.TTL(time.Minute, time.Minute), cachex.Jitter(0)),
		cachex.NewLayer[string](l1, cachex.TTL(time.Minute, time.Hour), cachex.Jitter(0)),
	}, now)
	defer func() { _ = c.Close() }()
	// L0: v1 stale from now on; L1: v1 already rotten
	require.NoError(t, m0.Set(ctx, "k", cachex.Entry[string]{Value: "v1", CachedAt: epoch, FreshUntil: epoch, ExpiresAt: epoch.Add(time.Minute)}))
	require.NoError(t, l1.Set(ctx, "k", cachex.Entry[string]{Value: "v1", CachedAt: epoch, FreshUntil: epoch, ExpiresAt: epoch}))

	l1.armed.Store(true)
	v, err := c.Get(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, "v1", v, "stale; the refresh leases L1 and pauses")
	<-l1.leased

	clock.Advance(2 * time.Minute) // L0's v1 rots
	v, err = c.Get(ctx, "k")       // refused L1's lease: source v2, deletes L1's older v1
	require.NoError(t, err)
	require.Equal(t, "v2", v)
	require.Eventually(t, func() bool { return !cachex.Sourcing(c, "k") }, 5*time.Second, time.Millisecond) // its backfill is done

	clock.Advance(3 * time.Minute) // L0's v2 rots
	v, err = c.Get(ctx, "k")       // leases L1: source v2 into L1 and L0
	require.NoError(t, err)
	require.Equal(t, "v2", v)
	require.Eventually(t, func() bool { return !cachex.Sourcing(c, "k") }, 5*time.Second, time.Millisecond)

	src.set("k", "v3")
	clock.Advance(time.Second)
	l1.resume <- struct{}{} // the refresh asks the source: v3
	cachex.Settle(c)
	e0, _ := stored(t, m0, "k")
	e1, _ := stored(t, l1, "k")
	t.Logf("L0 holds %q, L1 holds %q", e0.Value, e1.Value)

	v, err = c.Get(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, "v3", v)
	clock.Advance(3 * time.Minute) // L0's v3 rots; L1's v2 is still usable
	v, err = c.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "v3", v, "a read after one that returned v3 went back")
}

func TestKnownGapACopyLandsOverANewerAnswerEvictedFromTheFirstLayer(t *testing.T) {
	t.Skip("known gap, docs/todo.md #7")
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
	t.Skip("known gap, docs/todo.md #7")
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
