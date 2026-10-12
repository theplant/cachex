package rediscachex

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

func TestBroadcast(t *testing.T) {
	cachextest.TestBroadcast(t, func(t *testing.T) (cachex.Broadcast, cachex.Broadcast) {
		_, client := newRedis(t)
		return NewBroadcast(BroadcastConfig{Client: client}), NewBroadcast(BroadcastConfig{Client: client})
	})
}

func TestTheChannelFollowsTheKeyPrefix(t *testing.T) {
	_, client := newRedis(t)
	assert.Equal(t, "cachex-invalidate:p:", NewBroadcast(BroadcastConfig{Client: client, KeyPrefix: "p:"}).(*broadcast).channel)
	assert.Equal(t, "mine", NewBroadcast(BroadcastConfig{Client: client, KeyPrefix: "p:", Channel: "mine"}).(*broadcast).channel)
	assert.Panics(t, func() { NewBroadcast(BroadcastConfig{}) })
}

func TestAResubscriptionAfterRedisRestartsCallsReady(t *testing.T) {
	// what was published while the subscription was down is lost: the Cache
	// clears its local layers on the new ready
	mr, client := newRedis(t)
	b := NewBroadcast(BroadcastConfig{Client: client})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{}, 16)
	got := make(chan []byte, 16)
	go func() { _ = b.Listen(ctx, func() { ready <- struct{}{} }, func(m []byte) { got <- m }) }()
	waitFor(t, ready)

	mr.Close()
	require.NoError(t, mr.Restart())
	waitFor(t, ready)
	require.NoError(t, b.Publish(ctx, []byte("after")))
	select {
	case m := <-got:
		assert.Equal(t, "after", string(m))
	case <-time.After(10 * time.Second):
		t.Fatal("no message after the resubscription")
	}
}

func waitFor(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("ready was not called")
	}
}

func TestTwoInstancesShareTheirWrites(t *testing.T) {
	ctx := context.Background()
	_, client := newRedis(t)
	src := cachex.SourceFunc[string](func(context.Context, string) (string, error) { return "v1", nil })
	newInstance := func() (*cachex.Cache[string], *cachextest.Map[string]) {
		local := cachextest.NewMap[string]()
		shared := New[string](Config[string]{Client: client, KeyPrefix: "p:", Leases: true})
		c := cachex.New(src, []cachex.Layer[string]{
			cachex.NewLayer(local, cachex.TTL(time.Hour, 0)),
			cachex.NewLayer(shared, cachex.TTL(time.Hour, 0)),
		}, cachex.WithBroadcast(NewBroadcast(BroadcastConfig{Client: client, KeyPrefix: "p:"})))
		t.Cleanup(func() { _ = c.Close() })
		return c, local
	}
	a, _ := newInstance()
	b, bLocal := newInstance()
	// b's first clear may come after its first read: wait until b's memory
	// layer keeps what it reads
	require.Eventually(t, func() bool {
		_, err := b.Get(ctx, "k")
		_, ok, _ := bLocal.Get(ctx, "k")
		return err == nil && ok
	}, 10*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool { // and a has subscribed too: its writes reach b
		require.NoError(t, a.Set(ctx, "k", "v2"))
		_, ok, _ := bLocal.Get(ctx, "k")
		return !ok
	}, 10*time.Second, 10*time.Millisecond)
	v, err := b.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, "v2", v)
}
