package bigcachex

import (
	"context"
	"testing"
	"time"

	"github.com/allegro/bigcache/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

func newBigCache(t *testing.T) *bigcache.BigCache {
	t.Helper()
	bc, err := bigcache.New(context.Background(), bigcache.DefaultConfig(time.Hour))
	require.NoError(t, err)
	t.Cleanup(func() { _ = bc.Close() })
	return bc
}

func TestContract(t *testing.T) {
	cachextest.TestBackend(t, func(t *testing.T) cachex.Backend[string] {
		return New[string](Config[string]{Cache: newBigCache(t)})
	}, []string{"v", "", "héllo, 世界", "\x00\xff"})
	cachextest.TestBackend(t, func(t *testing.T) cachex.Backend[item] {
		return New[item](Config[item]{Cache: newBigCache(t)})
	}, []item{{ID: 1, Tags: []string{"a"}}, {}, {ID: 2}})
}

func TestByteBackend(t *testing.T) {
	cachextest.TestByteBackend(t, func(t *testing.T) cachex.ByteBackend { return byteBackend{newBigCache(t)} })
}

func TestValuesAreCopies(t *testing.T) {
	ctx := context.Background()
	type item struct{ N int }
	b := New[*item](Config[*item]{Cache: newBigCache(t)})
	require.NoError(t, b.Set(ctx, "k", cachex.Entry[*item]{Value: &item{1}, ExpiresAt: time.Now().Add(time.Hour)}))
	e, _, err := b.Get(ctx, "k")
	require.NoError(t, err)
	e.Value.N = 2
	e, _, err = b.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, 1, e.Value.N, "a decoded value is the caller's own")
}

type item struct {
	ID   int
	Tags []string
}
