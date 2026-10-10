package cachextest

import (
	"context"
	"testing"
	"time"

	"github.com/theplant/cachex/v2"
)

// TestBackend checks that a Backend[string] keeps the contract a Cache relies
// on. newBackend returns an empty backend; it is called once per subtest.
func TestBackend(t *testing.T, newBackend func(t *testing.T) cachex.Backend[string]) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	entry := func(value string) cachex.Entry[string] {
		return cachex.Entry[string]{Value: value, CachedAt: now, FreshUntil: now.Add(time.Minute), ExpiresAt: now.Add(time.Hour)}
	}
	same := func(t *testing.T, want, got cachex.Entry[string]) {
		t.Helper()
		if want.Value != got.Value || want.NotFound != got.NotFound ||
			!want.CachedAt.Equal(got.CachedAt) || !want.FreshUntil.Equal(got.FreshUntil) || !want.ExpiresAt.Equal(got.ExpiresAt) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	}
	noErr := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	get := func(t *testing.T, b cachex.Backend[string], key string) (cachex.Entry[string], bool) {
		t.Helper()
		e, ok, err := b.Get(ctx, key)
		noErr(t, err)
		return e, ok
	}

	t.Run("a missing key is not an error", func(t *testing.T) {
		b := newBackend(t)
		if _, ok := get(t, b, "missing"); ok {
			t.Fatal("found a key never set")
		}
		m, err := b.GetMany(ctx, []string{"missing"})
		noErr(t, err)
		if len(m) != 0 {
			t.Fatalf("GetMany of a missing key: %v", m)
		}
		noErr(t, b.Del(ctx, "missing"))
		noErr(t, b.DelMany(ctx, []string{"missing"}))
	})

	t.Run("an entry reads back as written", func(t *testing.T) {
		b := newBackend(t)
		for _, v := range []string{"v", "", "héllo, 世界", "\x00\xff"} {
			noErr(t, b.Set(ctx, "k", entry(v)))
			e, ok := get(t, b, "k")
			if !ok {
				t.Fatalf("%q not found", v)
			}
			same(t, entry(v), e)
		}
	})

	t.Run("a not-found reads back", func(t *testing.T) {
		b := newBackend(t)
		nf := cachex.Entry[string]{NotFound: true, CachedAt: now, FreshUntil: now.Add(time.Minute), ExpiresAt: now.Add(time.Hour)}
		noErr(t, b.Set(ctx, "k", nf))
		e, ok := get(t, b, "k")
		if !ok {
			t.Fatal("not found")
		}
		same(t, nf, e)
	})

	t.Run("keys compare exactly", func(t *testing.T) {
		b := newBackend(t)
		keys := []string{"abc", "ABC", "Abc", "a", "a ", "é", "e"}
		for _, k := range keys {
			noErr(t, b.Set(ctx, k, entry("v"+k)))
		}
		for _, k := range keys {
			e, ok := get(t, b, k)
			if !ok || e.Value != "v"+k {
				t.Fatalf("key %q: got %q, %v", k, e.Value, ok)
			}
		}
		m, err := b.GetMany(ctx, keys)
		noErr(t, err)
		for _, k := range keys {
			if m[k].Value != "v"+k {
				t.Fatalf("GetMany key %q: got %q", k, m[k].Value)
			}
		}
	})

	t.Run("batch operations", func(t *testing.T) {
		b := newBackend(t)
		noErr(t, b.SetMany(ctx, map[string]cachex.Entry[string]{"a": entry("1"), "b": entry("2"), "c": entry("3")}))
		noErr(t, b.Set(ctx, "a", entry("1b")))
		m, err := b.GetMany(ctx, []string{"a", "b", "c", "x"})
		noErr(t, err)
		if len(m) != 3 || m["a"].Value != "1b" || m["b"].Value != "2" || m["c"].Value != "3" {
			t.Fatalf("GetMany: %v", m)
		}
		same(t, entry("2"), m["b"])
		noErr(t, b.DelMany(ctx, []string{"a", "b", "x"}))
		m, err = b.GetMany(ctx, []string{"a", "b", "c"})
		noErr(t, err)
		if len(m) != 1 || m["c"].Value != "3" {
			t.Fatalf("after DelMany: %v", m)
		}
		noErr(t, b.Del(ctx, "c"))
		if _, ok := get(t, b, "c"); ok {
			t.Fatal("c still there after Del")
		}
	})

	t.Run("empty batches", func(t *testing.T) {
		b := newBackend(t)
		m, err := b.GetMany(ctx, nil)
		noErr(t, err)
		if len(m) != 0 {
			t.Fatalf("GetMany(nil): %v", m)
		}
		noErr(t, b.SetMany(ctx, nil))
		noErr(t, b.DelMany(ctx, nil))
	})
}
