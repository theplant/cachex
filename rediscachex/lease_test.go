package rediscachex

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

func TestLeases(t *testing.T) {
	newB := func(chunkSize int) func(t *testing.T) cachex.Backend[string] {
		return func(t *testing.T) cachex.Backend[string] {
			mr, client := newRedis(t)
			followTheClock(t, mr)
			return New[string](Config[string]{Client: client, KeyPrefix: "p:", Leases: true, ChunkSize: chunkSize})
		}
	}
	cachextest.TestLeaser(t, newB(0), "v")
	t.Run("in chunks", func(t *testing.T) { cachextest.TestLeaser(t, newB(2), "v") })
	cachextest.TestBackend(t, newB(0), []string{"v", "", "w"})
}

// followTheClock makes mr's TTLs run with the wall clock, as Redis's do
// (miniredis expires keys only when told to).
func followTheClock(t *testing.T, mr *miniredis.Miniredis) {
	done := make(chan struct{})
	stopped := make(chan struct{})
	t.Cleanup(func() { close(done); <-stopped })
	go func() {
		defer close(stopped)
		last := time.Now()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-tick.C:
				mr.FastForward(now.Sub(last))
				last = now
			}
		}
	}()
}

func TestLeasesAreOffByDefault(t *testing.T) {
	_, client := newRedis(t)
	b := New[string](Config[string]{Client: client})
	l, ok := b.(cachex.Leasing[string])
	require.True(t, ok)
	assert.Nil(t, l.Leaser())
}

func TestTheLeaseKeySharesTheDataKeysSlot(t *testing.T) {
	for prefixed, want := range map[string]string{
		"p:user:1":         "cachex-lease:{p:user:1}",
		"p:{user:1}:posts": "cachex-lease:t:p:{user:1}:posts",
		"{app}:x":          "cachex-lease:t:{app}:x",
		"p:a{b":            "cachex-lease:{p:a{b}",
		"p:{a}b}":          "cachex-lease:t:p:{a}b}",
	} {
		got, ok := leaseKey(prefixed)
		require.True(t, ok, prefixed)
		assert.Equal(t, want, got, prefixed)
		assert.Equal(t, hashTag(prefixed), hashTag(got), "%s: same slot", prefixed)
	}
	for _, prefixed := range []string{"p:a}b", "p:{}x}"} {
		_, ok := leaseKey(prefixed)
		assert.False(t, ok, "%s: no key in its slot can be built", prefixed)
	}
}

// hashTag is the part of key Redis Cluster hashes.
func hashTag(key string) string {
	for i := 0; i < len(key); i++ {
		if key[i] == '{' {
			for j := i + 1; j < len(key); j++ {
				if key[j] == '}' {
					if j > i+1 {
						return key[i+1 : j]
					}
					return key
				}
			}
			return key
		}
	}
	return key
}

func TestAWriteEndsTheLeaseBeforeWriting(t *testing.T) {
	// deleted after the write, the lease would let a backfill holding its
	// token land between the two
	_, client := newRedis(t)
	var mu sync.Mutex
	var sent []string
	client.AddHook(pipelineRecorder(func(cmds []redis.Cmder) {
		mu.Lock()
		defer mu.Unlock()
		for _, cmd := range cmds {
			if cmd.Name() != "client" { // a new connection's setup
				sent = append(sent, fmt.Sprint(cmd.Args()...))
			}
		}
	}))
	b := New[string](Config[string]{Client: client, KeyPrefix: "p:", Leases: true})
	ctx := context.Background()
	e := cachex.Entry[string]{Value: "v", ExpiresAt: time.Now().Add(time.Hour)}
	for name, write := range map[string]func() error{
		"Set":     func() error { return b.Set(ctx, "k", e) },
		"SetMany": func() error { return b.SetMany(ctx, map[string]cachex.Entry[string]{"k": e}) },
		"Del":     func() error { return b.Del(ctx, "k") },
		"DelMany": func() error { return b.DelMany(ctx, []string{"k"}) },
	} {
		sent = nil
		require.NoError(t, write(), name)
		require.Len(t, sent, 2, name)
		assert.Equal(t, "delcachex-lease:{p:k}", sent[0], name)
		assert.True(t, strings.HasPrefix(sent[1], "setp:k") || sent[1] == "delp:k", "%s: then %s", name, sent[1])
	}
}

type pipelineRecorder func([]redis.Cmder)

func (pipelineRecorder) DialHook(next redis.DialHook) redis.DialHook          { return next }
func (pipelineRecorder) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }
func (r pipelineRecorder) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		r(cmds)
		return next(ctx, cmds)
	}
}

func TestAScriptFlushedFromRedisIsSentAgain(t *testing.T) {
	mr, client := newRedis(t)
	b := New[string](Config[string]{Client: client, Leases: true})
	l := b.(cachex.Leasing[string]).Leaser()
	ctx := context.Background()
	for range 2 {
		mr.FlushAll()
		require.NoError(t, client.ScriptFlush(ctx).Err())
		tokens, err := l.Lease(ctx, []string{"a", "b"}, time.Minute)
		require.NoError(t, err)
		require.Len(t, tokens, 2)
		require.NoError(t, l.PutLeased(ctx, tokens, map[string]cachex.Entry[string]{"a": {Value: "v", ExpiresAt: time.Now().Add(time.Hour)}}))
		_, ok, err := b.Get(ctx, "a")
		require.NoError(t, err)
		assert.True(t, ok)
	}
}

func TestAnUnreachableRedisFailsEveryLease(t *testing.T) {
	mr, client := newRedis(t)
	b := New[string](Config[string]{Client: client, Leases: true})
	l := b.(cachex.Leasing[string]).Leaser()
	mr.Close()
	ctx := context.Background()
	tokens, err := l.Lease(ctx, []string{"a", "b"}, time.Minute)
	assert.Empty(t, tokens)
	var be *cachex.BatchError
	require.ErrorAs(t, err, &be)
	assert.Len(t, be.Errors, 2)
	require.ErrorAs(t, l.PutLeased(ctx, map[string]string{"a": "t"}, nil), &be)
	require.ErrorAs(t, l.Release(ctx, map[string]string{"a": "t"}), &be)
}

func TestTwoInstancesDoNotBackfillAnOldValue(t *testing.T) {
	ctx := context.Background()
	_, client := newRedis(t)
	asked, gate := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	data := "v1"
	src := cachex.SourceFunc[string](func(context.Context, string) (string, error) {
		mu.Lock()
		v := data
		mu.Unlock()
		asked <- struct{}{}
		<-gate
		return v, nil
	})
	newInstance := func() *cachex.Cache[string] {
		b := New[string](Config[string]{Client: client, KeyPrefix: "p:", Leases: true})
		return cachex.New(src, []cachex.Layer[string]{cachex.NewLayer(b, cachex.TTL(time.Hour, 0))})
	}
	a, b := newInstance(), newInstance()
	got := make(chan string)
	go func() {
		v, _ := a.Get(ctx, "k")
		got <- v
	}()
	<-asked // a read v1
	mu.Lock()
	data = "v2"
	mu.Unlock()
	require.NoError(t, b.Set(ctx, "k", "v2"))
	close(gate)
	assert.Equal(t, "v1", <-got)
	require.NoError(t, a.Close()) // waits for its backfill
	shared := New[string](Config[string]{Client: client, KeyPrefix: "p:"})
	e, ok, err := shared.Get(ctx, "k")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "v2", e.Value, "b's write stands in Redis")
}

func TestAKeyPrefixInTheLeasesNamespaceIsRejected(t *testing.T) {
	_, client := newRedis(t)
	assert.Panics(t, func() { New[string](Config[string]{Client: client, KeyPrefix: "cachex-lease:x:"}) })
	assert.NotPanics(t, func() { New[string](Config[string]{Client: client, KeyPrefix: "app:cachex-lease:"}) })
}

func TestItIsNotLocal(t *testing.T) {
	_, client := newRedis(t)
	_, ok := New[string](Config[string]{Client: client}).(cachex.LocalBackend)
	assert.False(t, ok, "a Redis is shared: a broadcast never clears it")
}

func TestAKeyInTheLeasesNamespaceIsNotStored(t *testing.T) {
	// with an empty prefix (or one that "cachex-lease:" starts with), a data
	// key could be the key of another key's lease
	ctx := context.Background()
	for _, prefix := range []string{"", "cachex-"} {
		_, client := newRedis(t)
		b := New[string](Config[string]{Client: client, KeyPrefix: prefix, Leases: true})
		l := b.(cachex.Leasing[string]).Leaser()
		tokens, err := l.Lease(ctx, []string{"x"}, time.Minute)
		require.NoError(t, err)
		require.Contains(t, tokens, "x")

		clash := strings.TrimPrefix("cachex-lease:{x}", prefix)
		e := cachex.Entry[string]{Value: "v", FreshUntil: time.Now().Add(time.Hour), ExpiresAt: time.Now().Add(time.Hour)}
		require.ErrorIs(t, b.Set(ctx, clash, e), cachex.ErrUnsupportedKey)
		var be *cachex.BatchError
		require.ErrorAs(t, b.SetMany(ctx, map[string]cachex.Entry[string]{clash: e}), &be)
		require.ErrorIs(t, be.Errors[clash], cachex.ErrUnsupportedKey)
		_, ok, err := b.Get(ctx, clash)
		require.NoError(t, err)
		assert.False(t, ok, "%q is not stored", clash)
		require.NoError(t, b.Del(ctx, clash))
		require.NoError(t, l.PutLeased(ctx, tokens, map[string]cachex.Entry[string]{"x": e}))
		got, ok, err := b.Get(ctx, "x")
		require.NoError(t, err)
		require.True(t, ok, "x's lease survived the writes of %q", clash)
		assert.Equal(t, "v", got.Value)
	}
}

func TestALeaseWhoseReplyWasLostIsStillHeld(t *testing.T) {
	// go-redis retries a pipeline on a network error: if the first SET NX
	// landed and its reply was lost, the retry finds the lease taken, by the
	// same token
	ctx := context.Background()
	mr, client := newRedis(t)
	defer func(f func() string) { newToken = f }(newToken)
	newToken = func() string { return "mine" }
	require.NoError(t, mr.Set("cachex-lease:{p:k}", "mine")) // the first try's
	tokens, err := New[string](Config[string]{Client: client, KeyPrefix: "p:", Leases: true}).(cachex.Leasing[string]).Leaser().Lease(ctx, []string{"k"}, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"k": "mine"}, tokens)
}

func TestAKeyPrefixWithAStrayBraceIsRejectedWithLeases(t *testing.T) {
	_, client := newRedis(t)
	assert.Panics(t, func() { New[string](Config[string]{Client: client, KeyPrefix: "p}", Leases: true}) })
	assert.NotPanics(t, func() { New[string](Config[string]{Client: client, KeyPrefix: "p}"}) })
	assert.NotPanics(t, func() { New[string](Config[string]{Client: client, KeyPrefix: "{tenant}:", Leases: true}) })
}

func TestAKeyAndItsBracedSelfHaveTheirOwnLeases(t *testing.T) {
	ctx := context.Background()
	_, client := newRedis(t)
	l := New[string](Config[string]{Client: client, Leases: true}).(cachex.Leasing[string]).Leaser()
	for _, key := range []string{"X", "{X}"} {
		tokens, err := l.Lease(ctx, []string{key}, time.Minute)
		require.NoError(t, err)
		assert.NotEmpty(t, tokens[key], "%s is leased on its own", key)
	}
}

func TestAKeyPrefixWithAnUnclosedBraceIsRejectedWithLeases(t *testing.T) {
	// "a{" before "}b{c}" (which the Cache accepts) makes "a{}b{c}": no slot
	_, client := newRedis(t)
	for _, prefix := range []string{"a{", "{}", "a{b"} {
		assert.Panics(t, func() { New[string](Config[string]{Client: client, KeyPrefix: prefix, Leases: true}) }, prefix)
	}
}

func TestTheEmptyKeyHasNoLeaseWithoutAPrefix(t *testing.T) {
	// "cachex-lease:{}" has an empty tag: Redis Cluster would hash all of it,
	// into another slot than the data key ""
	_, ok := leaseKey("")
	assert.False(t, ok)
	_, client := newRedis(t)
	b := New[string](Config[string]{Client: client, Leases: true})
	err := b.Set(context.Background(), "", cachex.Entry[string]{Value: "v", ExpiresAt: time.Now().Add(time.Hour)})
	assert.ErrorIs(t, err, cachex.ErrUnsupportedKey)
}

func TestALeasedEntryThatDoesNotEncodeEndsItsLease(t *testing.T) {
	ctx := context.Background()
	_, client := newRedis(t)
	l := New[float64](Config[float64]{Client: client, KeyPrefix: "p:", Leases: true}).(cachex.Leasing[float64]).Leaser()
	tokens, err := l.Lease(ctx, []string{"k"}, time.Minute)
	require.NoError(t, err)
	err = l.PutLeased(ctx, tokens, map[string]cachex.Entry[float64]{"k": {Value: math.NaN(), ExpiresAt: time.Now().Add(time.Hour)}}) // JSON has no NaN
	require.Error(t, err)
	again, err := l.Lease(ctx, []string{"k"}, time.Minute)
	require.NoError(t, err)
	assert.NotEmpty(t, again["k"], "the lease ended, not left until its TTL")
}

type lostReply struct{ armed *atomic.Bool }

func (lostReply) DialHook(next redis.DialHook) redis.DialHook          { return next }
func (lostReply) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }
func (h lostReply) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		err := next(ctx, cmds)
		if len(cmds) > 0 && cmds[0].Name() == "set" && h.armed.CompareAndSwap(true, false) {
			return errors.New("i/o timeout") // the commands landed, their replies did not
		}
		return err
	}
}

// A Lease whose commands landed but whose replies were lost (after go-redis's
// own retries) reports the key failed, and ends the lease it may have taken:
// left, a token nobody has would refuse the key to every instance until its
// TTL.
func TestAFailedLeaseLeavesNoLeaseBehind(t *testing.T) {
	ctx := context.Background()
	_, client := newRedis(t)
	armed := &atomic.Bool{}
	client.AddHook(lostReply{armed})
	l := New[string](Config[string]{Client: client, KeyPrefix: "p:", Leases: true}).(cachex.Leasing[string]).Leaser()
	armed.Store(true)
	tokens, err := l.Lease(ctx, []string{"k"}, time.Minute)
	require.Error(t, err)
	require.Empty(t, tokens)
	tokens, err = l.Lease(ctx, []string{"k"}, time.Minute)
	require.NoError(t, err)
	assert.NotEmpty(t, tokens["k"], "a lease nobody holds a token of blocks the key")
}
