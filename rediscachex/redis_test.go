package rediscachex

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
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
		return New[string](Config[string]{Client: client, KeyPrefix: "p:"})
	}, []string{"v", "", "héllo, 世界", "\x00\xff"})
	cachextest.TestBackend(t, func(t *testing.T) cachex.Backend[item] {
		_, client := newRedis(t)
		return New[item](Config[item]{Client: client, KeyPrefix: "p:"})
	}, []item{{ID: 1, Tags: []string{"a"}}, {}, {ID: 2}})
	t.Run("in chunks", func(t *testing.T) {
		cachextest.TestBackend(t, func(t *testing.T) cachex.Backend[string] {
			_, client := newRedis(t)
			return New[string](Config[string]{Client: client, ChunkSize: 2})
		}, []string{"v", "", "w"})
	})
}

func TestByteBackend(t *testing.T) {
	cachextest.TestByteBackend(t, func(t *testing.T) cachex.ByteBackend {
		_, client := newRedis(t)
		return &byteBackend{client: client, prefix: "p:", chunkSize: 3}
	})
}

func TestEntriesExpireAtTheirExpiresAt(t *testing.T) {
	ctx := context.Background()
	mr, client := newRedis(t)
	b := New[string](Config[string]{Client: client, KeyPrefix: "p:"})
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

func TestAnUnreachableRedisFailsEveryKey(t *testing.T) {
	ctx := context.Background()
	mr, client := newRedis(t)
	b := New[string](Config[string]{Client: client, ChunkSize: 2})
	mr.Close()
	var be *cachex.BatchError
	for name, err := range map[string]error{
		"Get": func() error { _, _, err := b.Get(ctx, "a"); return err }(),
		"Set": b.Set(ctx, "a", cachex.Entry[string]{Value: "v", ExpiresAt: time.Now().Add(time.Hour)}),
		"Del": b.Del(ctx, "a"),
	} {
		require.Error(t, err, name)
		assert.NotErrorAs(t, err, &be, "%s: a single key fails with a plain error", name)
	}
	m, err := b.GetMany(ctx, []string{"a", "b", "c"})
	assert.Empty(t, m)
	require.ErrorAs(t, err, &be, "not read as misses")
	assert.Len(t, be.Errors, 3)
	err = b.SetMany(ctx, map[string]cachex.Entry[string]{"a": {Value: "v", ExpiresAt: time.Now().Add(time.Hour)}})
	require.ErrorAs(t, err, &be)
	err = b.DelMany(ctx, []string{"a"})
	require.ErrorAs(t, err, &be)
}

type item struct {
	ID   int
	Tags []string
}
