package ottercachex_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
	"github.com/theplant/cachex/v2/ottercachex"
)

func TestContract(t *testing.T) {
	cachextest.TestBackend(t, func(t *testing.T) cachex.Backend[string] {
		b, err := ottercachex.New[string](ottercachex.Config[string]{MaximumSize: 1000})
		require.NoError(t, err)
		return b
	})
}

func TestEntriesExpireOnTheirOwn(t *testing.T) {
	ctx := context.Background()
	b, err := ottercachex.New[string](ottercachex.Config[string]{MaximumSize: 10})
	require.NoError(t, err)
	now := time.Now()
	require.NoError(t, b.Set(ctx, "short", cachex.Entry[string]{Value: "v", ExpiresAt: now.Add(50 * time.Millisecond)}))
	require.NoError(t, b.Set(ctx, "long", cachex.Entry[string]{Value: "v", ExpiresAt: now.Add(time.Hour)}))
	require.Eventually(t, func() bool {
		_, ok, _ := b.Get(ctx, "short")
		return !ok
	}, 2*time.Second, 5*time.Millisecond)
	_, ok, err := b.Get(ctx, "long")
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestTheSizeIsBounded(t *testing.T) {
	ctx := context.Background()
	b, err := ottercachex.New[string](ottercachex.Config[string]{MaximumSize: 100})
	require.NoError(t, err)
	e := cachex.Entry[string]{Value: "v", ExpiresAt: time.Now().Add(time.Hour)}
	for i := range 10_000 {
		require.NoError(t, b.Set(ctx, fmt.Sprint(i), e))
	}
	// otter evicts in its maintenance, which reads and writes drive
	require.Eventually(t, func() bool { _, _, _ = b.Get(ctx, "0"); return b.Len() <= 100 }, 5*time.Second, time.Millisecond)
}

func TestWeight(t *testing.T) {
	ctx := context.Background()
	b, err := ottercachex.New[string](ottercachex.Config[string]{
		MaximumWeight: 1000,
		Weigher:       func(key string, e cachex.Entry[string]) uint32 { return uint32(len(key) + len(e.Value)) },
	})
	require.NoError(t, err)
	e := cachex.Entry[string]{Value: string(make([]byte, 100)), ExpiresAt: time.Now().Add(time.Hour)}
	for i := range 100 {
		require.NoError(t, b.Set(ctx, fmt.Sprint(i), e))
	}
	require.Eventually(t, func() bool { _, _, _ = b.Get(ctx, "0"); return b.Len() <= 10 }, 5*time.Second, time.Millisecond)
}

func TestConfigNeedsABound(t *testing.T) {
	_, err := ottercachex.New[string](ottercachex.Config[string]{})
	assert.Error(t, err)
}
