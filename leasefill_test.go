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
