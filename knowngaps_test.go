// Reproductions of the reads that can still go back (docs/todo.md #6): they
// fail with the skip removed.
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
