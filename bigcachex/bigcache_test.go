package bigcachex_test

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/allegro/bigcache/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/bigcachex"
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
		return bigcachex.New[string](bigcachex.Config[string]{Cache: newBigCache(t)})
	})
}

func TestAnEntryThatDoesNotDecodeIsAMiss(t *testing.T) {
	ctx := context.Background()
	bc := newBigCache(t)
	var log bytes.Buffer
	writer := bigcachex.New[string](bigcachex.Config[string]{Cache: bc})
	reader := bigcachex.New[string](bigcachex.Config[string]{
		Cache:  bc,
		Codec:  cachex.JSONCodec[string](),
		Logger: slog.New(slog.NewTextHandler(&log, nil)),
	})
	require.NoError(t, writer.Set(ctx, "k", cachex.Entry[string]{Value: "not json", ExpiresAt: time.Now().Add(time.Hour)}))

	_, ok, err := reader.Get(ctx, "k")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Contains(t, log.String(), "dropped an entry that does not decode")
	_, ok, err = writer.Get(ctx, "k")
	require.NoError(t, err)
	assert.False(t, ok, "and dropped it")

	require.NoError(t, writer.Set(ctx, "k", cachex.Entry[string]{Value: "not json", ExpiresAt: time.Now().Add(time.Hour)}))
	m, err := reader.GetMany(ctx, []string{"k"})
	require.NoError(t, err)
	assert.Empty(t, m)
}

func TestValuesAreCopies(t *testing.T) {
	ctx := context.Background()
	type item struct{ N int }
	b := bigcachex.New[*item](bigcachex.Config[*item]{Cache: newBigCache(t)})
	require.NoError(t, b.Set(ctx, "k", cachex.Entry[*item]{Value: &item{1}, ExpiresAt: time.Now().Add(time.Hour)}))
	e, _, err := b.Get(ctx, "k")
	require.NoError(t, err)
	e.Value.N = 2
	e, _, err = b.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, 1, e.Value.N, "a decoded value is the caller's own")
}
