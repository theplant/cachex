package cachex_test

import (
	"bytes"
	"context"
	"log/slog"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

// byteMap is an in-memory ByteBackend; failKey fails every operation on that
// key, and a key in it fails a Many call as a *BatchError.
type byteMap struct {
	mu      sync.Mutex
	data    map[string]cachex.ByteEntry
	failKey string
}

func newByteMap() *byteMap { return &byteMap{data: map[string]cachex.ByteEntry{}} }

func (m *byteMap) Get(_ context.Context, key string) ([]byte, bool, error) {
	if key == m.failKey {
		return nil, false, errBoom
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.data[key]
	return e.Data, ok, nil
}

func (m *byteMap) GetMany(ctx context.Context, keys []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	errs := map[string]error{}
	for _, key := range keys {
		data, ok, err := m.Get(ctx, key)
		switch {
		case err != nil:
			errs[key] = err
		case ok:
			out[key] = data
		}
	}
	return out, cachex.NewBatchError(errs)
}

func (m *byteMap) Set(_ context.Context, key string, data []byte, expiresAt time.Time) error {
	if key == m.failKey {
		return errBoom
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = cachex.ByteEntry{Data: data, ExpiresAt: expiresAt}
	return nil
}

func (m *byteMap) SetMany(ctx context.Context, entries map[string]cachex.ByteEntry) error {
	errs := map[string]error{}
	for key, e := range entries {
		if err := m.Set(ctx, key, e.Data, e.ExpiresAt); err != nil {
			errs[key] = err
		}
	}
	return cachex.NewBatchError(errs)
}

func (m *byteMap) Del(_ context.Context, key string) error {
	if key == m.failKey {
		return errBoom
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

func (m *byteMap) DelMany(ctx context.Context, keys []string) error {
	errs := map[string]error{}
	for _, key := range keys {
		if err := m.Del(ctx, key); err != nil {
			errs[key] = err
		}
	}
	return cachex.NewBatchError(errs)
}

func (m *byteMap) has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.data[key]
	return ok
}

func TestByteMap(t *testing.T) {
	cachextest.TestByteBackend(t, func(t *testing.T) cachex.ByteBackend { return newByteMap() })
}

func TestEncoded(t *testing.T) {
	ctx := context.Background()
	later := time.Now().Add(time.Hour)

	cachextest.TestBackend(t, func(t *testing.T) cachex.Backend[string] {
		return cachex.Encoded[string](newByteMap(), nil, nil)
	}, strs)
	cachextest.TestBackend(t, func(t *testing.T) cachex.Backend[product] {
		return cachex.Encoded[product](newByteMap(), nil, nil)
	}, products)

	t.Run("data that does not decode is a miss, logged and dropped", func(t *testing.T) {
		m := newByteMap()
		var log bytes.Buffer
		b := cachex.Encoded(m, cachex.JSONCodec[string](), slog.New(slog.NewTextHandler(&log, nil)))
		require.NoError(t, m.Set(ctx, "k", []byte("written by something else"), later))
		_, ok, err := b.Get(ctx, "k")
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Contains(t, log.String(), "dropped an entry that does not decode")
		assert.False(t, m.has("k"), "and deleted")

		// the value's type changed: the entry decodes, its value does not
		data, err := cachex.EncodeEntry(cachex.DefaultCodec[string](), cachex.Entry[string]{Value: "not json", ExpiresAt: later})
		require.NoError(t, err)
		require.NoError(t, m.SetMany(ctx, map[string]cachex.ByteEntry{"k": {Data: data, ExpiresAt: later}, "ok": {Data: data, ExpiresAt: later}}))
		require.NoError(t, b.Set(ctx, "ok", cachex.Entry[string]{Value: "v", ExpiresAt: later}))
		got, err := b.GetMany(ctx, []string{"k", "ok"})
		require.NoError(t, err)
		assert.Equal(t, []string{"ok"}, keysOf(got))
		assert.False(t, m.has("k"))
	})

	t.Run("an entry already expired deletes its key", func(t *testing.T) {
		m := newByteMap()
		b := cachex.Encoded[string](m, nil, nil)
		past := cachex.Entry[string]{Value: "v", ExpiresAt: time.Now().Add(-time.Second)}
		require.NoError(t, b.Set(ctx, "k", cachex.Entry[string]{Value: "v", ExpiresAt: later}))
		require.NoError(t, b.Set(ctx, "k", past))
		assert.False(t, m.has("k"))
		require.NoError(t, b.SetMany(ctx, map[string]cachex.Entry[string]{"k": {Value: "v", ExpiresAt: later}}))
		require.NoError(t, b.SetMany(ctx, map[string]cachex.Entry[string]{"k": past, "new": past, "live": {Value: "v", ExpiresAt: later}}))
		assert.False(t, m.has("k"))
		assert.False(t, m.has("new"))
		assert.True(t, m.has("live"))
	})

	t.Run("a failed key fails alone; single keys fail with plain errors", func(t *testing.T) {
		m := newByteMap()
		m.failKey = "bad"
		b := cachex.Encoded[string](m, nil, nil)
		e := cachex.Entry[string]{Value: "v", ExpiresAt: later}
		past := cachex.Entry[string]{Value: "v", ExpiresAt: time.Now().Add(-time.Second)}
		for name, err := range map[string]error{
			"SetMany":            b.SetMany(ctx, map[string]cachex.Entry[string]{"ok": e, "bad": e}),
			"SetMany of expired": b.SetMany(ctx, map[string]cachex.Entry[string]{"ok": past, "bad": past}),
			"GetMany":            func() error { _, err := b.GetMany(ctx, []string{"ok", "bad"}); return err }(),
			"DelMany":            b.DelMany(ctx, []string{"ok", "bad"}),
		} {
			var be *cachex.BatchError
			require.ErrorAs(t, err, &be, name)
			assert.Len(t, be.Errors, 1, name)
			assert.ErrorIs(t, be.Errors["bad"], errBoom, name)
		}
		for name, err := range map[string]error{
			"Get":         func() error { _, _, err := b.Get(ctx, "bad"); return err }(),
			"Set":         b.Set(ctx, "bad", e),
			"Set expired": b.Set(ctx, "bad", past),
			"Del":         b.Del(ctx, "bad"),
		} {
			var be *cachex.BatchError
			assert.ErrorIs(t, err, errBoom, name)
			assert.NotErrorAs(t, err, &be, name)
		}
	})

	t.Run("a value that does not encode fails its key alone", func(t *testing.T) {
		m := newByteMap()
		b := cachex.Encoded[float64](m, nil, nil) // JSON has no NaN
		nan := cachex.Entry[float64]{Value: math.NaN(), ExpiresAt: later}
		require.Error(t, b.Set(ctx, "k", nan))
		err := b.SetMany(ctx, map[string]cachex.Entry[float64]{"bad": nan, "ok": {Value: 1, ExpiresAt: later}})
		var be *cachex.BatchError
		require.ErrorAs(t, err, &be)
		assert.Contains(t, be.Errors, "bad")
		assert.Len(t, be.Errors, 1)
		assert.True(t, m.has("ok"))
	})
}

func keysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
