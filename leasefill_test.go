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

func TestARefreshCopyDoesNotWriteAnOlderEntryIntoALeasingLayer(t *testing.T) {
	// L0 local, L1 shared with leases, L2 shared. A refresh holds L1's lease
	// and copies v1 from L2; meanwhile a read (refused L1's lease) gets v2
	// from the source and returns it; the refresh then writes v1 into L1,
	// with no CachedAt check. Once L0's entry goes, a read returns v1.
	ctx := context.Background()
	src := newSource(map[string]string{"k": "v1"})
	clock, now := newClock()
	m0 := cachextest.NewMap[string]()
	l1 := cachextest.NewLeasingMap[string]()
	m2 := cachextest.NewMap[string]()
	l2 := &pausedAfterRead[string]{Backend: m2, read: make(chan struct{}), resume: make(chan struct{})}
	c := cachex.New[string](src, []cachex.Layer[string]{
		cachex.NewLayer[string](m0, cachex.TTL(time.Minute, time.Hour), cachex.Jitter(0)),
		cachex.NewLayer[string](l1, cachex.TTL(time.Hour, time.Hour), cachex.Jitter(0)),
		cachex.NewLayer[string](l2, cachex.TTL(time.Hour, time.Hour), cachex.Jitter(0)),
	}, now)
	require.NoError(t, c.Set(ctx, "k", "v1"))
	require.NoError(t, l1.Del(ctx, "k")) // evicted from L1
	clock.Advance(2 * time.Minute)       // stale in L0 only
	src.set("k", "v2")

	l2.armed.Store(true)
	v, err := c.Get(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, "v1", v) // stale; the refresh leases L1 and reads v1 from L2
	<-l2.read

	require.NoError(t, m0.Del(ctx, "k")) // evicted from L0
	require.NoError(t, m2.Del(ctx, "k")) // and from L2
	v, err = c.Get(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, "v2", v, "the read asks the source")

	l2.resume <- struct{}{}
	cachex.Settle(c)
	e, _ := stored(t, l1, "k")
	t.Logf("L1 holds %q", e.Value)

	require.NoError(t, m0.Del(ctx, "k")) // L0's entry goes
	v, err = c.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "v2", v, "a read after one that returned v2 went back")
	require.NoError(t, c.Close())
}

func TestAReadRefusedALeaseLeavesNoOlderAnswerInThatLayer(t *testing.T) {
	// L0 local, L1 shared with leases, L2 shared. A refresh holds L1's lease
	// and asks the source (v1); a read misses L0, is refused L1's lease (by
	// the refresh of the same Cache), and asks the source after the refresh
	// landed: v2, which it lands in L2 and L0 but not in L1. Once L0's
	// shorter-lived entry rots, a read returns L1's v1.
	ctx := context.Background()
	src := newSource(map[string]string{"k": "v1"})
	clock, now := newClock()
	m0 := cachextest.NewMap[string]()
	l1 := cachextest.NewLeasingMap[string]()
	l2 := &pausedAfterRead[string]{Backend: cachextest.NewMap[string](), read: make(chan struct{}), resume: make(chan struct{})}
	c := cachex.New[string](src, []cachex.Layer[string]{
		cachex.NewLayer[string](m0, cachex.TTL(time.Minute, 10*time.Minute), cachex.Jitter(0)),
		cachex.NewLayer[string](l1, cachex.TTL(time.Hour, time.Hour), cachex.Jitter(0)),
		cachex.NewLayer[string](l2, cachex.TTL(time.Hour, time.Hour), cachex.Jitter(0)),
	}, now)
	// L0 holds a stale v0; L1 and L2 hold nothing
	require.NoError(t, m0.Set(ctx, "k", cachex.Entry[string]{Value: "v0", CachedAt: epoch, FreshUntil: epoch.Add(time.Minute), ExpiresAt: epoch.Add(11 * time.Minute)}))
	clock.Advance(2 * time.Minute)

	src.gate = make(chan struct{})
	asked := make(chan struct{}, 1)
	src.onGet = func(context.Context, string) { asked <- struct{}{} }
	v, err := c.Get(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, "v0", v) // stale: the refresh leases L1, misses L2, asks the source
	<-asked

	require.NoError(t, m0.Del(ctx, "k")) // so that the next read misses L0
	l2.armed.Store(true)
	got := make(chan string, 1)
	go func() {
		v, err := c.Get(ctx, "k") // refused L1's lease, misses L2
		assert.NoError(t, err)
		got <- v
	}()
	<-l2.read

	src.onGet = nil
	close(src.gate) // the refresh answers v1 and lands it
	require.Eventually(t, func() bool { e, ok := stored(t, m0, "k"); return ok && e.Value == "v1" }, 5*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { e, ok := stored(t, l1, "k"); return ok && e.Value == "v1" }, 5*time.Second, time.Millisecond)
	time.Sleep(10 * time.Millisecond) // let the refresh unregister its call to the source (it does so right after landing)
	src.set("k", "v2")
	l2.resume <- struct{}{}
	require.Equal(t, "v2", <-got, "the read asks the source after the refresh")
	cachex.Settle(c)

	clock.Advance(12 * time.Minute) // L0's entry rots; L1's is still fresh
	e, _ := stored(t, l1, "k")
	t.Logf("L1 holds %q", e.Value)
	v, err = c.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "v2", v, "a read after one that returned v2 went back")
	require.NoError(t, c.Close())
}

type pausedManyAfterRead[T any] struct {
	cachex.Backend[T]
	armed  atomic.Bool
	read   chan struct{}
	resume chan struct{}
}

func (p *pausedManyAfterRead[T]) GetMany(ctx context.Context, keys []string) (map[string]cachex.Entry[T], error) {
	es, err := p.Backend.GetMany(ctx, keys)
	if p.armed.CompareAndSwap(true, false) {
		p.read <- struct{}{}
		<-p.resume
	}
	return es, err
}

func TestABatchCopyWritesTheAdoptedEntryToALeasingLayer(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "batch"}[batch], func(t *testing.T) {
			ctx := context.Background()
			src := newSource(map[string]string{"k": "v1", "k2": "w1"})
			clock, now := newClock()
			m0 := cachextest.NewMap[string]()
			l1 := cachextest.NewLeasingMap[string]()
			m2 := cachextest.NewMap[string]()
			m3 := cachextest.NewMap[string]()
			read, resume := make(chan struct{}), make(chan struct{})
			inner := &pausedAfterRead[string]{Backend: m3, read: read, resume: resume}
			l3 := &pausedManyAfterRead[string]{Backend: inner, read: read, resume: resume}
			c := cachex.New[string](src, []cachex.Layer[string]{
				cachex.NewLayer[string](m0, cachex.TTL(time.Minute, time.Hour), cachex.Jitter(0)),
				cachex.NewLayer[string](l1, cachex.TTL(time.Hour, time.Hour), cachex.Jitter(0)),
				cachex.NewLayer[string](m2, cachex.TTL(time.Hour, time.Hour), cachex.Jitter(0)),
				cachex.NewLayer[string](l3, cachex.TTL(time.Hour, time.Hour), cachex.Jitter(0)),
			}, now)
			keys := []string{"k"}
			if batch {
				keys = []string{"k", "k2"}
			}
			require.NoError(t, c.SetMany(ctx, map[string]string{"k": "v1", "k2": "w1"}))
			for _, key := range keys {
				require.NoError(t, l1.Del(ctx, key)) // evicted from L1 and L2
				require.NoError(t, m2.Del(ctx, key))
			}
			clock.Advance(2 * time.Minute) // stale in L0 only
			src.set("k", "v2")

			if batch {
				l3.armed.Store(true)
			} else {
				inner.armed.Store(true)
			}
			_, err := c.GetMany(ctx, keys) // stale: the refresh leases L1, reads v1 from L3
			require.NoError(t, err)
			<-read

			require.NoError(t, m0.Del(ctx, "k"))
			require.NoError(t, m3.Del(ctx, "k"))
			v, err := c.Get(ctx, "k") // refused L1's lease; source v2 into L3, L2, L0
			require.NoError(t, err)
			require.Equal(t, "v2", v)

			resume <- struct{}{}
			cachex.Settle(c)
			e, _ := stored(t, l1, "k")
			t.Logf("L1 holds %q", e.Value)
			require.NoError(t, m0.Del(ctx, "k"))
			v, err = c.Get(ctx, "k")
			require.NoError(t, err)
			assert.Equal(t, "v2", v, "went back")
			require.NoError(t, c.Close())
		})
	}
}
