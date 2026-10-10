package stripe

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLockTakesAFreeStripeEvenWithADoneCtx(t *testing.T) {
	var s Stripe
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, s.Lock(ctx, func() { t.Error("late must not run") }))
	s.Unlock()
}

func TestLockGivesUpWaitingAndRunsLateWhileHoldingTheLock(t *testing.T) {
	var s Stripe
	require.True(t, s.TryLock()) // another write holds it

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	lateRan := make(chan bool, 1)
	err := s.Lock(ctx, func() { lateRan <- !s.TryRLock() })
	require.ErrorIs(t, err, context.DeadlineExceeded)

	s.Unlock() // the other write ends
	select {
	case held := <-lateRan:
		assert.True(t, held, "late runs while the given-up lock is held")
	case <-time.After(time.Second):
		t.Fatal("late never ran")
	}
	require.Eventually(t, s.TryLock, time.Second, time.Millisecond, "and the lock is released after it")
	s.Unlock()
}

func TestBackfillsShareTheStripeButNotWithAWrite(t *testing.T) {
	var s Stripe
	require.True(t, s.TryRLock())
	assert.True(t, s.TryRLock(), "backfills share the stripe")
	assert.False(t, s.TryLock(), "a write waits for backfills")
	s.RUnlock()
	s.RUnlock()
	require.True(t, s.TryLock())
	assert.False(t, s.TryRLock(), "a backfill is skipped while a write holds the stripe")
	s.Unlock()
}

func TestCounters(t *testing.T) {
	var s Stripe
	gen, epoch := s.Generation(), s.Epoch()
	s.AddFill()
	assert.Equal(t, gen, s.Generation(), "a backfill is not a write")
	assert.NotEqual(t, epoch, s.Epoch())
	epoch = s.Epoch()
	s.AddWrite()
	assert.NotEqual(t, gen, s.Generation())
	assert.NotEqual(t, epoch, s.Epoch())
}

func TestForIsStablePerSet(t *testing.T) {
	set := NewSet()
	assert.Same(t, set.For("k"), set.For("k"))
}

func TestIndexAndAtAgreeWithFor(t *testing.T) {
	s := NewSet()
	for _, key := range []string{"a", "b", "user:1", ""} {
		i := s.Index(key)
		if i < 0 || i >= Count {
			t.Fatalf("Index(%q) = %d, out of range", key, i)
		}
		if s.At(i) != s.For(key) {
			t.Fatalf("At(Index(%q)) is not For(%q)", key, key)
		}
	}
}
