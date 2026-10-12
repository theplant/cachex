package cachextest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
)

// TestLeaser is the contract test for a Backend whose leases are on (see
// cachex.Leaser): a lease is taken once, until it ends or expires; a write of
// the key ends it; PutLeased writes only under the lease's token; Release
// ends it without writing. newBackend returns an empty backend; value is a
// value of T.
func TestLeaser[T any](t *testing.T, newBackend func(t *testing.T) cachex.Backend[T], value T) {
	t.Helper()
	ctx := context.Background()
	const ttl = time.Minute
	entry := cachex.Entry[T]{Value: value, ExpiresAt: time.Now().Add(time.Hour)}
	setup := func(t *testing.T) (cachex.Backend[T], cachex.Leaser[T]) {
		t.Helper()
		b := newBackend(t)
		l, ok := b.(cachex.Leasing[T])
		require.True(t, ok, "the backend implements cachex.Leasing")
		leaser := l.Leaser()
		require.NotNil(t, leaser, "its leases are on")
		return b, leaser
	}
	lease := func(t *testing.T, l cachex.Leaser[T], keys ...string) map[string]string {
		t.Helper()
		tokens, err := l.Lease(ctx, keys, ttl)
		require.NoError(t, err)
		return tokens
	}
	absent := func(t *testing.T, b cachex.Backend[T], key string, msg string) {
		t.Helper()
		_, ok, err := b.Get(ctx, key)
		require.NoError(t, err)
		assert.False(t, ok, msg)
	}
	present := func(t *testing.T, b cachex.Backend[T], key string, msg string) {
		t.Helper()
		_, ok, err := b.Get(ctx, key)
		require.NoError(t, err)
		assert.True(t, ok, msg)
	}

	t.Run("a lease is taken once", func(t *testing.T) {
		_, l := setup(t)
		tokens := lease(t, l, "k")
		require.NotEmpty(t, tokens["k"])
		assert.Empty(t, lease(t, l, "k"), "leased already")
	})

	t.Run("a repeated key is leased once", func(t *testing.T) {
		_, l := setup(t)
		tokens := lease(t, l, "k", "k")
		require.NotEmpty(t, tokens["k"])
		assert.Empty(t, lease(t, l, "k"), "leased already")
		require.NoError(t, l.Release(ctx, tokens))
		assert.NotEmpty(t, lease(t, l, "k")["k"], "its token was the one held")
	})

	t.Run("PutLeased writes under the token and ends the lease", func(t *testing.T) {
		b, l := setup(t)
		tokens := lease(t, l, "k")
		require.NoError(t, l.PutLeased(ctx, tokens, map[string]cachex.Entry[T]{"k": entry}))
		present(t, b, "k", "written")
		assert.NotEmpty(t, lease(t, l, "k")["k"], "the lease ended")
	})

	t.Run("PutLeased without an entry deletes the key", func(t *testing.T) {
		b, l := setup(t)
		require.NoError(t, b.Set(ctx, "k", entry))
		tokens := lease(t, l, "k")
		require.NotEmpty(t, tokens["k"], "a key that exists can be leased")
		require.NoError(t, l.PutLeased(ctx, tokens, nil))
		absent(t, b, "k", "deleted")
		assert.NotEmpty(t, lease(t, l, "k")["k"], "the lease ended")
	})

	writes := map[string]func(b cachex.Backend[T]) error{
		"Set":     func(b cachex.Backend[T]) error { return b.Set(ctx, "k", entry) },
		"SetMany": func(b cachex.Backend[T]) error { return b.SetMany(ctx, map[string]cachex.Entry[T]{"k": entry}) },
		"Del":     func(b cachex.Backend[T]) error { return b.Del(ctx, "k") },
		"DelMany": func(b cachex.Backend[T]) error { return b.DelMany(ctx, []string{"k"}) },
	}
	for name, write := range writes {
		t.Run(name+" ends the lease", func(t *testing.T) {
			b, l := setup(t)
			tokens := lease(t, l, "k")
			require.NoError(t, write(b))
			require.NoError(t, l.PutLeased(ctx, tokens, nil))
			if name == "Set" || name == "SetMany" {
				present(t, b, "k", "an ended lease deletes nothing")
			}
			require.NoError(t, b.Del(ctx, "k"))
			require.NoError(t, l.PutLeased(ctx, tokens, map[string]cachex.Entry[T]{"k": entry}))
			absent(t, b, "k", "an ended lease writes nothing")
			assert.NotEmpty(t, lease(t, l, "k")["k"], "the key can be leased again")
		})
	}

	t.Run("another token writes nothing", func(t *testing.T) {
		b, l := setup(t)
		lease(t, l, "k")
		require.NoError(t, l.PutLeased(ctx, map[string]string{"k": "not-the-token"}, map[string]cachex.Entry[T]{"k": entry}))
		absent(t, b, "k", "not written")
		assert.Empty(t, lease(t, l, "k"), "the lease holds")
		require.NoError(t, l.Release(ctx, map[string]string{"k": "not-the-token"}))
		assert.Empty(t, lease(t, l, "k"), "nor is it released")
	})

	t.Run("Release ends the lease and writes nothing", func(t *testing.T) {
		b, l := setup(t)
		require.NoError(t, b.Set(ctx, "k", entry))
		tokens := lease(t, l, "k")
		require.NoError(t, l.Release(ctx, tokens))
		present(t, b, "k", "kept")
		require.NoError(t, l.PutLeased(ctx, tokens, nil))
		present(t, b, "k", "a released token deletes nothing")
		assert.NotEmpty(t, lease(t, l, "k")["k"])
	})

	t.Run("a lease expires", func(t *testing.T) {
		_, l := setup(t)
		tokens, err := l.Lease(ctx, []string{"k"}, 50*time.Millisecond)
		require.NoError(t, err)
		require.NotEmpty(t, tokens["k"])
		require.Eventually(t, func() bool {
			tokens, err := l.Lease(ctx, []string{"k"}, ttl)
			return err == nil && tokens["k"] != ""
		}, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("an entry already expired does not leave the old one readable", func(t *testing.T) {
		b, l := setup(t)
		require.NoError(t, b.Set(ctx, "k", entry))
		tokens := lease(t, l, "k")
		expired := entry
		expired.ExpiresAt = time.Now().Add(-time.Second)
		require.NoError(t, l.PutLeased(ctx, tokens, map[string]cachex.Entry[T]{"k": expired}))
		e, ok, err := b.Get(ctx, "k")
		require.NoError(t, err)
		if ok { // kept as given (the Cache does not serve it) or dropped, never the old one
			assert.True(t, e.ExpiresAt.Equal(expired.ExpiresAt))
		}
	})

	t.Run("batches", func(t *testing.T) {
		b, l := setup(t)
		lease(t, l, "taken")
		keys := []string{"taken"}
		for i := range 5 {
			keys = append(keys, fmt.Sprintf("k%d", i))
		}
		require.NoError(t, b.Set(ctx, "k4", entry))
		tokens := lease(t, l, keys...)
		assert.Len(t, tokens, 5, "every key but the one leased already")
		assert.NotContains(t, tokens, "taken")
		require.NoError(t, b.Del(ctx, "k0")) // ends k0's lease
		entries := map[string]cachex.Entry[T]{}
		for _, k := range keys {
			entries[k] = entry
		}
		delete(entries, "k4") // k4 is deleted
		require.NoError(t, l.PutLeased(ctx, tokens, entries))
		absent(t, b, "k0", "its lease ended: not written")
		for _, k := range []string{"k1", "k2", "k3"} {
			present(t, b, k, k)
		}
		absent(t, b, "k4", "no entry: deleted")
		absent(t, b, "taken", "not leased by this call")
		tokens, err := l.Lease(ctx, nil, ttl)
		require.NoError(t, err)
		assert.Empty(t, tokens)
		require.NoError(t, l.PutLeased(ctx, nil, nil))
		require.NoError(t, l.Release(ctx, nil))
	})
}
