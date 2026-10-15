package cachex_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

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

// A refresh that finds an older (fresh) entry below does not copy it over the
// newer (stale) one the layer holds, whether the layer leases or not: the
// lease keeps other writers out, not the layer's own newer entry.
func TestARefreshDoesNotCopyAnOlderEntryOverANewerOne(t *testing.T) {
	for name, top := range map[string]func() cachex.Backend[string]{
		"Map":        func() cachex.Backend[string] { return cachextest.NewMap[string]() },
		"LeasingMap": func() cachex.Backend[string] { return cachextest.NewLeasingMap[string]() },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			clock, now := newClock()
			l0, l1 := top(), cachextest.NewMap[string]()
			c := cachex.New[string](newSource(map[string]string{"k": "src"}), []cachex.Layer[string]{
				cachex.NewLayer(l0, cachex.TTL(time.Minute, time.Hour), cachex.Jitter(0)),
				cachex.NewLayer[string](l1, cachex.TTL(time.Hour, time.Hour), cachex.Jitter(0)),
			}, now)
			t0 := clock.Now()
			require.NoError(t, l1.Set(ctx, "k", cachex.Entry[string]{Value: "old", CachedAt: t0, FreshUntil: t0.Add(2 * time.Hour), ExpiresAt: t0.Add(3 * time.Hour)}))
			require.NoError(t, l0.Set(ctx, "k", cachex.Entry[string]{Value: "new", CachedAt: t0.Add(10 * time.Minute), FreshUntil: t0.Add(11 * time.Minute), ExpiresAt: t0.Add(time.Hour)}))
			clock.Advance(20 * time.Minute) // l0 stale, l1 fresh
			v, err := c.Get(ctx, "k")
			require.NoError(t, err)
			assert.Equal(t, "new", v)
			require.NoError(t, c.Close())
			e, ok := stored(t, l0, "k")
			require.True(t, ok)
			assert.Equal(t, "new", e.Value, "an older copy replaced a newer answer")
		})
	}
}

// A GetMany through a source that is not a BatchSource asks its keys
// WithGetManyConcurrency at a time, each within its own fetch timeout: the
// later keys outlive leases taken before the chunk read below, so they are
// leased again before their source is asked, and still backfilled.
func TestAGetManyAskingKeysInTurnStillBackfillsTheLastOnes(t *testing.T) {
	ctx := context.Background()
	shared := cachextest.NewLeasingMap[string]()
	src := cachex.SourceFunc[string](func(context.Context, string) (string, error) {
		time.Sleep(40 * time.Millisecond)
		return "v", nil
	})
	c := cachex.New[string](src, []cachex.Layer[string]{
		cachex.NewLayer[string](shared, cachex.TTL(time.Minute, 0), cachex.Jitter(0)),
	}, cachex.WithFetchTimeout(100*time.Millisecond), cachex.WithGetManyConcurrency(1))
	keys := []string{"a", "b", "c", "d", "e"}
	got, err := c.GetMany(ctx, keys)
	require.NoError(t, err)
	require.Len(t, got, 5)
	require.NoError(t, c.Close())
	for _, k := range keys {
		_, ok := stored(t, shared, k)
		assert.True(t, ok, "%s backfilled into the leasing layer", k)
	}
}

type failPutLeasing struct {
	*cachextest.LeasingMap[string]
}

func (f failPutLeasing) Leaser() cachex.Leaser[string] { return failPutLeaser{f.LeasingMap.Leaser()} }

type failPutLeaser struct{ cachex.Leaser[string] }

func (failPutLeaser) PutLeased(context.Context, map[string]string, map[string]cachex.Entry[string]) error {
	return errBoom
}

// A backfill whose PutLeased fails still ends the lease when the fetch ends:
// left held, it would refuse the key to every instance until it expired.
func TestAFailedBackfillReleasesItsLease(t *testing.T) {
	shared := cachextest.NewLeasingMap[string]()
	c := cachex.New[string](newSource(map[string]string{"k": "v"}), []cachex.Layer[string]{
		cachex.NewLayer[string](cachextest.NewMap[string](), cachex.TTL(time.Minute, 0)),
		cachex.NewLayer[string](failPutLeasing{shared}, cachex.TTL(time.Minute, 0)),
	})
	if _, err := c.Get(context.Background(), "k"); err != nil {
		t.Fatal(err)
	}
	cachex.Settle(c)
	tokens, _ := shared.Leaser().Lease(context.Background(), []string{"k"}, time.Minute)
	if _, ok := tokens["k"]; !ok {
		t.Fatalf("the fetch's lease on k is still held after the fetch ended")
	}
}
