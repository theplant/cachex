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

func TestARefusedInstanceDoesNotDeleteTheHoldersFreshEntry(t *testing.T) {
	ctx := context.Background()
	clock, now := newClock()
	shared := cachextest.NewLeasingMap[string]()
	instance := func(src *source) *cachex.Cache[string] {
		return cachex.New[string](src, []cachex.Layer[string]{
			cachex.NewLayer[string](cachextest.NewMap[string](), cachex.TTL(time.Minute, 0), cachex.Jitter(0)),
			cachex.NewLayer[string](shared, cachex.TTL(time.Hour, time.Hour), cachex.Jitter(0)),
		}, now)
	}
	gated := func() (*source, chan struct{}, chan struct{}) {
		s := newSource(map[string]string{"k": "v"})
		s.gate = make(chan struct{})
		asked := make(chan struct{}, 1)
		s.onGet = func(context.Context, string) { asked <- struct{}{} }
		return s, s.gate, asked
	}
	srcA, gateA, askedA := gated()
	srcB, gateB, askedB := gated()
	a, b := instance(srcA), instance(srcB)

	doneA := make(chan error, 1)
	go func() { _, err := a.Get(ctx, "k"); doneA <- err }()
	<-askedA // A holds the shared layer's lease
	doneB := make(chan error, 1)
	go func() { _, err := b.Get(ctx, "k"); doneB <- err }()
	<-askedB // B was refused it and asks the source too

	close(gateA)
	require.NoError(t, <-doneA)
	cachex.Settle(a)
	_, ok := stored(t, shared, "k")
	require.True(t, ok, "A landed its answer in the shared layer")

	clock.Advance(time.Second) // B's answer comes a moment later
	close(gateB)
	require.NoError(t, <-doneB)
	cachex.Settle(b)
	_, ok = stored(t, shared, "k")
	assert.True(t, ok, "B deleted A's entry from the shared layer")

	srcC := newSource(map[string]string{"k": "v"})
	c := instance(srcC)
	_, err := c.Get(ctx, "k")
	require.NoError(t, err)
	assert.Zero(t, srcC.calls.Load(), "a third instance should hit the shared layer")
	for _, x := range []*cachex.Cache[string]{a, b, c} {
		require.NoError(t, x.Close())
	}
}

// The shared layer still holds a rotten entry (gormcachex keeps expired rows
// until DeleteExpired). A misses it and leases; B is refused, asks the source
// and lands first: it deletes the rotten entry, and the delete, a write, ends
// A's lease. A's PutLeased is then refused: the shared layer stays empty.
func TestARefusedInstanceDeletingARottenEntryDoesNotEndTheHoldersLease(t *testing.T) {
	ctx := context.Background()
	clock, now := newClock()
	shared := cachextest.NewLeasingMap[string]()
	require.NoError(t, shared.Set(ctx, "k", cachex.Entry[string]{Value: "old", CachedAt: epoch.Add(-2 * time.Hour), FreshUntil: epoch.Add(-time.Hour), ExpiresAt: epoch.Add(-time.Minute)}))
	instance := func(src *source) *cachex.Cache[string] {
		return cachex.New[string](src, []cachex.Layer[string]{
			cachex.NewLayer[string](cachextest.NewMap[string](), cachex.TTL(time.Minute, 0), cachex.Jitter(0)),
			cachex.NewLayer[string](shared, cachex.TTL(time.Hour, time.Hour), cachex.Jitter(0)),
		}, now)
	}
	gated := func() (*source, chan struct{}, chan struct{}) {
		s := newSource(map[string]string{"k": "v"})
		s.gate = make(chan struct{})
		asked := make(chan struct{}, 1)
		s.onGet = func(context.Context, string) { asked <- struct{}{} }
		return s, s.gate, asked
	}
	srcA, gateA, askedA := gated()
	srcB, gateB, askedB := gated()
	a, b := instance(srcA), instance(srcB)

	doneA := make(chan error, 1)
	go func() { _, err := a.Get(ctx, "k"); doneA <- err }()
	<-askedA // A holds the lease
	doneB := make(chan error, 1)
	go func() { _, err := b.Get(ctx, "k"); doneB <- err }()
	<-askedB // B was refused

	close(gateB) // B answers first
	require.NoError(t, <-doneB)
	cachex.Settle(b)
	clock.Advance(time.Second)
	close(gateA)
	require.NoError(t, <-doneA)
	cachex.Settle(a)

	e, ok := stored(t, shared, "k")
	assert.True(t, ok && e.Value == "v", "the holder's answer should land in the shared layer; got %+v %v", e, ok)
	for _, x := range []*cachex.Cache[string]{a, b} {
		require.NoError(t, x.Close())
	}
}

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

type hookedLeasingLeasing struct {
	*cachextest.LeasingMap[string]
	putArmed, leaseArmed atomic.Bool
	paused               chan map[string]string
	resume               chan struct{}
}

func (h *hookedLeasingLeasing) Leaser() cachex.Leaser[string] {
	return hookedLeasingLeaser{h.LeasingMap.Leaser(), h}
}

type hookedLeasingLeaser struct {
	cachex.Leaser[string]
	h *hookedLeasingLeasing
}

func (l hookedLeasingLeaser) Lease(ctx context.Context, keys []string, ttl time.Duration) (map[string]string, error) {
	tokens, err := l.Leaser.Lease(ctx, keys, ttl)
	if l.h.leaseArmed.CompareAndSwap(true, false) {
		l.h.paused <- tokens
		<-l.h.resume
	}
	return tokens, err
}

func (l hookedLeasingLeaser) PutLeased(ctx context.Context, tokens map[string]string, entries map[string]cachex.Entry[string]) error {
	if l.h.putArmed.CompareAndSwap(true, false) {
		l.h.paused <- tokens
		<-l.h.resume
	}
	return l.Leaser.PutLeased(ctx, tokens, entries)
}

func TestARefusalByASupersededReadOfThisProcessCounts(t *testing.T) {
	// One leased layer. A refresh lands while a read R1 claims its flight:
	// the refresh's second supersede drops R1's flight, though R1 has only
	// leased so far. R2 claims a new flight and is refused the lease by R1,
	// a read of this process (byOwn looks at refreshes only). R1 lands v2;
	// R2 asks the source after it (v3), may not write the layer and does not
	// delete v2 there: it returns v3, and the next read hits v2.
	ctx := context.Background()
	src := newSource(map[string]string{"k": "v1"})
	clock, now := newClock()
	m0 := &hookedLeasingLeasing{LeasingMap: cachextest.NewLeasingMap[string](), paused: make(chan map[string]string), resume: make(chan struct{})}
	c := cachex.New[string](src, []cachex.Layer[string]{
		cachex.NewLayer[string](m0, cachex.TTL(time.Minute, time.Minute), cachex.Jitter(0)),
	}, now)
	defer func() { _ = c.Close() }()
	require.NoError(t, c.Set(ctx, "k", "v1"))
	clock.Advance(90 * time.Second) // stale

	src.set("k", "v2")
	m0.putArmed.Store(true)
	v, err := c.Get(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, "v1", v, "stale; the refresh fetches v2 and pauses in PutLeased, holding the fill lock")
	<-m0.paused
	require.NoError(t, m0.Del(ctx, "k")) // evicted: ends the refresh's lease

	m0.leaseArmed.Store(true)
	r1 := make(chan string, 1)
	go func() {
		v, err := c.Get(ctx, "k")
		assert.NoError(t, err)
		r1 <- v
	}()
	require.NotEmpty(t, <-m0.paused, "R1 leases k")

	m0.resume <- struct{}{} // the refresh's PutLeased (refused) and its supersede, which drops R1's flight
	require.Eventually(t, func() bool { return !cachex.Sourcing(c, "k") }, time.Second, time.Millisecond)
	time.Sleep(10 * time.Millisecond) // the refresh ends

	m0.leaseArmed.Store(true)
	r2 := make(chan string, 1)
	go func() {
		v, err := c.Get(ctx, "k")
		assert.NoError(t, err)
		r2 <- v
	}()
	require.Empty(t, <-m0.paused, "R2 has a flight of its own and is refused: R1 holds the lease")

	m0.resume <- struct{}{} // R1 asks the source (v2) and lands it
	require.Equal(t, "v2", <-r1)
	require.Eventually(t, func() bool { return !cachex.Sourcing(c, "k") }, time.Second, time.Millisecond)

	src.set("k", "v3")
	m0.resume <- struct{}{} // R2 asks the source (v3)
	first := <-r2
	require.Equal(t, "v3", first)
	v, err = c.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, first, v, "a read after one that returned %q went back", first)
}

// pausingLeasingLeasing is a leasing map whose next Lease, once armed, signals
// leased after taking (or being refused) the leases and waits for resume
// before it returns.
type pausingLeasingLeasing struct {
	*cachextest.LeasingMap[string]
	armed  atomic.Bool
	leased chan map[string]string
	resume chan struct{}
}

func (p *pausingLeasingLeasing) Leaser() cachex.Leaser[string] {
	return pausingLeasingLeaser{p.LeasingMap.Leaser(), p}
}

type pausingLeasingLeaser struct {
	cachex.Leaser[string]
	p *pausingLeasingLeasing
}

func (l pausingLeasingLeaser) Lease(ctx context.Context, keys []string, ttl time.Duration) (map[string]string, error) {
	tokens, err := l.Leaser.Lease(ctx, keys, ttl)
	if l.p.armed.CompareAndSwap(true, false) {
		l.p.leased <- tokens
		<-l.p.resume
	}
	return tokens, err
}

func TestARefusalByARefreshThatEndsDuringTheLeaseCounts(t *testing.T) {
	// One leased layer. A refresh holds k's lease and waits in the source. A
	// read misses (the stale entry rotted) and is refused the lease; before
	// it looks at c.refreshing, the refresh lands v2 and unregisters. The
	// read takes byOwn = false, so its fill neither writes the layer nor
	// deletes v2 from it: it returns v3, and the next read hits v2.
	ctx := context.Background()
	src := newSource(map[string]string{"k": "v1"})
	clock, now := newClock()
	m0 := &pausingLeasingLeasing{LeasingMap: cachextest.NewLeasingMap[string](), leased: make(chan map[string]string), resume: make(chan struct{})}
	c := cachex.New[string](src, []cachex.Layer[string]{
		cachex.NewLayer[string](m0, cachex.TTL(time.Minute, time.Minute), cachex.Jitter(0)),
	}, now)
	defer func() { _ = c.Close() }()
	require.NoError(t, c.Set(ctx, "k", "v1"))
	clock.Advance(90 * time.Second) // stale

	src.set("k", "v2")
	src.gate = make(chan struct{})
	asked := make(chan struct{}, 1)
	src.onGet = func(context.Context, string) { asked <- struct{}{} }
	v, err := c.Get(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, "v1", v, "stale; the refresh leases k and waits in the source")
	<-asked
	src.onGet = nil

	clock.Advance(time.Minute) // v1 rots
	m0.armed.Store(true)
	got := make(chan string, 1)
	go func() {
		v, err := c.Get(ctx, "k")
		assert.NoError(t, err)
		got <- v
	}()
	require.Empty(t, <-m0.leased, "refused: the refresh holds the lease")

	close(src.gate) // the refresh lands v2 and ends
	require.Eventually(t, func() bool { e, ok := stored(t, m0, "k"); return ok && e.Value == "v2" }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return !cachex.Sourcing(c, "k") }, time.Second, time.Millisecond)
	time.Sleep(10 * time.Millisecond) // and leaves c.refreshing

	src.set("k", "v3")
	m0.resume <- struct{}{}
	first := <-got
	require.Equal(t, "v3", first)
	v, err = c.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, first, v, "a read after one that returned %q went back", first)
}
