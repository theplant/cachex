package cachextest

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
)

// TestBackend checks that a Backend keeps the contract a Cache relies on.
// newBackend returns an empty backend; it is called once per subtest. values
// are at least three different values of T that the backend stores; run it
// with a string and with a struct type, so that a backend that encodes values
// is tested with its codec's main paths.
func TestBackend[T any](t *testing.T, newBackend func(t *testing.T) cachex.Backend[T], values []T) {
	t.Helper()
	require.GreaterOrEqual(t, len(values), 3, "TestBackend needs at least three values")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	entry := func(i int) cachex.Entry[T] {
		return cachex.Entry[T]{Value: values[i%len(values)], CachedAt: now, FreshUntil: now.Add(time.Minute), ExpiresAt: now.Add(time.Hour)}
	}
	notFound := cachex.Entry[T]{NotFound: true, CachedAt: now, FreshUntil: now.Add(time.Minute), ExpiresAt: now.Add(time.Hour)}
	same := func(t *testing.T, want, got cachex.Entry[T]) {
		t.Helper()
		assert.Equal(t, want.Value, got.Value)
		assert.Equal(t, want.NotFound, got.NotFound)
		assert.True(t, want.CachedAt.Equal(got.CachedAt), "CachedAt: got %v, want %v", got.CachedAt, want.CachedAt)
		assert.True(t, want.FreshUntil.Equal(got.FreshUntil), "FreshUntil: got %v, want %v", got.FreshUntil, want.FreshUntil)
		assert.True(t, want.ExpiresAt.Equal(got.ExpiresAt), "ExpiresAt: got %v, want %v", got.ExpiresAt, want.ExpiresAt)
	}
	get := func(t *testing.T, b cachex.Backend[T], key string) (cachex.Entry[T], bool) {
		t.Helper()
		e, ok, err := b.Get(ctx, key)
		require.NoError(t, err)
		return e, ok
	}

	t.Run("a missing key is not an error", func(t *testing.T) {
		b := newBackend(t)
		_, ok := get(t, b, "missing")
		assert.False(t, ok, "found a key never set")
		m, err := b.GetMany(ctx, []string{"missing"})
		require.NoError(t, err)
		assert.Empty(t, m)
		require.NoError(t, b.Del(ctx, "missing"))
		require.NoError(t, b.DelMany(ctx, []string{"missing"}))
	})

	t.Run("an entry reads back as written", func(t *testing.T) {
		b := newBackend(t)
		for i := range values {
			require.NoError(t, b.Set(ctx, "k", entry(i)))
			e, ok := get(t, b, "k")
			require.True(t, ok, "value %d not found", i)
			same(t, entry(i), e)
		}
	})

	t.Run("a not-found reads back", func(t *testing.T) {
		b := newBackend(t)
		require.NoError(t, b.Set(ctx, "k", notFound))
		e, ok := get(t, b, "k")
		require.True(t, ok)
		same(t, notFound, e)
	})

	t.Run("a value and a not-found overwrite each other", func(t *testing.T) {
		b := newBackend(t)
		require.NoError(t, b.Set(ctx, "k", entry(0)))
		require.NoError(t, b.Set(ctx, "k", notFound))
		e, ok := get(t, b, "k")
		require.True(t, ok)
		same(t, notFound, e)
		require.NoError(t, b.SetMany(ctx, map[string]cachex.Entry[T]{"k": entry(1)}))
		e, ok = get(t, b, "k")
		require.True(t, ok)
		same(t, entry(1), e)
	})

	t.Run("an entry already expired does not leave the old value readable", func(t *testing.T) {
		b := newBackend(t)
		expired := entry(1)
		expired.FreshUntil, expired.ExpiresAt = now.Add(-2*time.Second), now.Add(-time.Second)
		require.NoError(t, b.Set(ctx, "one", entry(0)))
		require.NoError(t, b.Set(ctx, "one", expired))
		require.NoError(t, b.SetMany(ctx, map[string]cachex.Entry[T]{"a": entry(0), "b": entry(0)}))
		require.NoError(t, b.SetMany(ctx, map[string]cachex.Entry[T]{"a": expired, "b": entry(2)}))
		for _, key := range []string{"one", "a"} {
			if e, ok := get(t, b, key); ok {
				same(t, expired, e) // kept as given (the Cache does not serve it) or dropped, never the old one
			}
		}
		e, ok := get(t, b, "b")
		require.True(t, ok, "the other entries of the batch are written")
		same(t, entry(2), e)
	})

	t.Run("keys compare exactly", func(t *testing.T) {
		b := newBackend(t)
		keys := []string{"abc", "ABC", "Abc", "a", "a ", "é", "e"}
		for i, k := range keys {
			require.NoError(t, b.Set(ctx, k, entry(i)))
		}
		for i, k := range keys {
			e, ok := get(t, b, k)
			require.True(t, ok, "key %q", k)
			assert.Equal(t, entry(i).Value, e.Value, "key %q", k)
		}
		m, err := b.GetMany(ctx, keys)
		require.NoError(t, err)
		for i, k := range keys {
			assert.Equal(t, entry(i).Value, m[k].Value, "GetMany key %q", k)
		}
	})

	t.Run("batch operations", func(t *testing.T) {
		b := newBackend(t)
		require.NoError(t, b.SetMany(ctx, map[string]cachex.Entry[T]{"a": entry(0), "b": entry(1), "c": entry(2)}))
		require.NoError(t, b.Set(ctx, "a", entry(1)))
		m, err := b.GetMany(ctx, []string{"a", "b", "c", "x"})
		require.NoError(t, err)
		require.Len(t, m, 3)
		same(t, entry(1), m["a"])
		same(t, entry(1), m["b"])
		same(t, entry(2), m["c"])
		require.NoError(t, b.DelMany(ctx, []string{"a", "b", "x"}))
		m, err = b.GetMany(ctx, []string{"a", "b", "c"})
		require.NoError(t, err)
		require.Len(t, m, 1)
		same(t, entry(2), m["c"])
		require.NoError(t, b.Del(ctx, "c"))
		_, ok := get(t, b, "c")
		assert.False(t, ok, "c still there after Del")
	})

	t.Run("a key repeated in a batch", func(t *testing.T) {
		b := newBackend(t)
		require.NoError(t, b.SetMany(ctx, map[string]cachex.Entry[T]{"a": entry(0), "b": entry(1)}))
		m, err := b.GetMany(ctx, []string{"a", "x", "a", "b", "x"})
		require.NoError(t, err)
		require.Len(t, m, 2)
		same(t, entry(0), m["a"])
		require.NoError(t, b.DelMany(ctx, []string{"a", "a", "x", "x"}))
		m, err = b.GetMany(ctx, []string{"a", "b"})
		require.NoError(t, err)
		assert.Len(t, m, 1)
	})

	t.Run("empty batches", func(t *testing.T) {
		b := newBackend(t)
		m, err := b.GetMany(ctx, nil)
		require.NoError(t, err)
		assert.Empty(t, m)
		require.NoError(t, b.SetMany(ctx, nil))
		require.NoError(t, b.DelMany(ctx, nil))
	})
}

// TestByteBackend checks that a ByteBackend keeps the contract cachex.Encoded
// relies on. newBackend returns an empty backend; it is called once per
// subtest.
func TestByteBackend(t *testing.T, newBackend func(t *testing.T) cachex.ByteBackend) {
	t.Helper()
	ctx := context.Background()
	later := time.Now().Add(time.Hour)
	values := [][]byte{[]byte("v"), {0}, []byte("héllo, 世界"), {0x00, 0xff, 0x01, 0x80}, make([]byte, 64<<10)}
	get := func(t *testing.T, b cachex.ByteBackend, key string) ([]byte, bool) {
		t.Helper()
		data, ok, err := b.Get(ctx, key)
		require.NoError(t, err)
		return data, ok
	}

	t.Run("a missing key is not an error", func(t *testing.T) {
		b := newBackend(t)
		_, ok := get(t, b, "missing")
		assert.False(t, ok)
		m, err := b.GetMany(ctx, []string{"missing"})
		require.NoError(t, err)
		assert.Empty(t, m)
		require.NoError(t, b.Del(ctx, "missing"))
		require.NoError(t, b.DelMany(ctx, []string{"missing"}))
	})

	t.Run("bytes read back as written and overwrite", func(t *testing.T) {
		b := newBackend(t)
		for i, v := range values {
			require.NoError(t, b.Set(ctx, "k", v, later))
			data, ok := get(t, b, "k")
			require.True(t, ok, "value %d", i)
			assert.Equal(t, v, data, "value %d", i)
		}
	})

	t.Run("keys compare exactly", func(t *testing.T) {
		b := newBackend(t)
		keys := []string{"abc", "ABC", "Abc", "a", "a ", "é", "e"}
		for _, k := range keys {
			require.NoError(t, b.Set(ctx, k, []byte("v"+k), later))
		}
		for _, k := range keys {
			data, ok := get(t, b, k)
			require.True(t, ok, "key %q", k)
			assert.Equal(t, "v"+k, string(data), "key %q", k)
		}
		m, err := b.GetMany(ctx, keys)
		require.NoError(t, err)
		for _, k := range keys {
			assert.Equal(t, "v"+k, string(m[k]), "GetMany key %q", k)
		}
	})

	t.Run("batch operations", func(t *testing.T) {
		b := newBackend(t)
		entries := map[string]cachex.ByteEntry{}
		var keys []string
		for i := range 10 {
			k := fmt.Sprint("k", i)
			entries[k] = cachex.ByteEntry{Data: []byte("v" + k), ExpiresAt: later}
			keys = append(keys, k)
		}
		require.NoError(t, b.SetMany(ctx, entries))
		require.NoError(t, b.Set(ctx, "k0", []byte("new"), later))
		m, err := b.GetMany(ctx, slices.Concat(keys, []string{"x", "k1"}))
		require.NoError(t, err)
		require.Len(t, m, 10)
		assert.Equal(t, "new", string(m["k0"]))
		assert.Equal(t, "vk9", string(m["k9"]))
		require.NoError(t, b.DelMany(ctx, slices.Concat(keys[:5], []string{"x", "k0"})))
		m, err = b.GetMany(ctx, keys)
		require.NoError(t, err)
		assert.Len(t, m, 5)
		require.NoError(t, b.Del(ctx, "k9"))
		_, ok := get(t, b, "k9")
		assert.False(t, ok)
	})

	t.Run("empty batches", func(t *testing.T) {
		b := newBackend(t)
		m, err := b.GetMany(ctx, nil)
		require.NoError(t, err)
		assert.Empty(t, m)
		require.NoError(t, b.SetMany(ctx, nil))
		require.NoError(t, b.DelMany(ctx, nil))
	})
}
