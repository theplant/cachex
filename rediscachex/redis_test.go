package rediscachex_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
	"github.com/theplant/cachex/v2/rediscachex"
)

func newRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = client.Close() })
	return mr, client
}

func TestContract(t *testing.T) {
	cachextest.TestBackend(t, func(t *testing.T) cachex.Backend[string] {
		_, client := newRedis(t)
		return rediscachex.New[string](rediscachex.Config[string]{Client: client, KeyPrefix: "p:"})
	})
	t.Run("in chunks", func(t *testing.T) {
		cachextest.TestBackend(t, func(t *testing.T) cachex.Backend[string] {
			_, client := newRedis(t)
			return rediscachex.New[string](rediscachex.Config[string]{Client: client, ChunkSize: 2})
		})
	})
}

func TestEntriesExpireAtTheirExpiresAt(t *testing.T) {
	ctx := context.Background()
	mr, client := newRedis(t)
	b := rediscachex.New[string](rediscachex.Config[string]{Client: client, KeyPrefix: "p:"})
	now := time.Now()
	require.NoError(t, b.SetMany(ctx, map[string]cachex.Entry[string]{
		"short": {Value: "v", ExpiresAt: now.Add(time.Minute)},
		"long":  {Value: "v", ExpiresAt: now.Add(time.Hour)},
	}))
	assert.InDelta(t, time.Minute, mr.TTL("p:short"), float64(time.Second), "the native TTL comes from the entry")
	mr.FastForward(2 * time.Minute)
	_, ok, err := b.Get(ctx, "short")
	require.NoError(t, err)
	assert.False(t, ok)
	_, ok, err = b.Get(ctx, "long")
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestAnExpiredEntryIsNotStored(t *testing.T) {
	ctx := context.Background()
	mr, client := newRedis(t)
	b := rediscachex.New[string](rediscachex.Config[string]{Client: client})
	require.NoError(t, b.Set(ctx, "k", cachex.Entry[string]{Value: "v", ExpiresAt: time.Now().Add(time.Hour)}))
	require.NoError(t, b.Set(ctx, "k", cachex.Entry[string]{Value: "v", ExpiresAt: time.Now().Add(-time.Second)}))
	assert.False(t, mr.Exists("k"), "and it replaces what was there")
}

func TestAnUnreachableRedisFailsEveryKey(t *testing.T) {
	ctx := context.Background()
	mr, client := newRedis(t)
	b := rediscachex.New[string](rediscachex.Config[string]{Client: client, ChunkSize: 2})
	mr.Close()
	_, _, err := b.Get(ctx, "a")
	require.Error(t, err)
	m, err := b.GetMany(ctx, []string{"a", "b", "c"})
	assert.Empty(t, m)
	var be *cachex.BatchError
	require.ErrorAs(t, err, &be, "not read as misses")
	assert.Len(t, be.Errors, 3)
	err = b.SetMany(ctx, map[string]cachex.Entry[string]{"a": {Value: "v", ExpiresAt: time.Now().Add(time.Hour)}})
	require.ErrorAs(t, err, &be)
	err = b.DelMany(ctx, []string{"a"})
	require.ErrorAs(t, err, &be)
}

func TestAnEntryThatDoesNotDecodeIsAMiss(t *testing.T) {
	ctx := context.Background()
	mr, client := newRedis(t)
	var log bytes.Buffer
	b := rediscachex.New[string](rediscachex.Config[string]{Client: client, Logger: slog.New(slog.NewTextHandler(&log, nil))})
	require.NoError(t, mr.Set("k", "written by v1"))
	_, ok, err := b.Get(ctx, "k")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Contains(t, log.String(), "dropped an entry that does not decode")
	assert.False(t, mr.Exists("k"))

	require.NoError(t, mr.Set("k", "written by v1"))
	m, err := b.GetMany(ctx, []string{"k"})
	require.NoError(t, err)
	assert.Empty(t, m)
}

func TestManyKeysAcrossChunks(t *testing.T) {
	ctx := context.Background()
	_, client := newRedis(t)
	b := rediscachex.New[string](rediscachex.Config[string]{Client: client, ChunkSize: 3})
	entries := map[string]cachex.Entry[string]{}
	var keys []string
	for i := range 10 {
		k := fmt.Sprint(i)
		entries[k] = cachex.Entry[string]{Value: "v" + k, ExpiresAt: time.Now().Add(time.Hour)}
		keys = append(keys, k)
	}
	require.NoError(t, b.SetMany(ctx, entries))
	m, err := b.GetMany(ctx, keys)
	require.NoError(t, err)
	assert.Len(t, m, 10)
	require.NoError(t, b.DelMany(ctx, keys))
	m, err = b.GetMany(ctx, keys)
	require.NoError(t, err)
	assert.Empty(t, m)
}
