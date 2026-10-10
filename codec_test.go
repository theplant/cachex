package cachex_test

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

type product struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// point marshals itself as 8 bytes.
type point struct{ X, Y int32 }

func (p point) MarshalBinary() ([]byte, error) {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b, uint32(p.X))
	binary.BigEndian.PutUint32(b[4:], uint32(p.Y))
	return b, nil
}

func (p *point) UnmarshalBinary(b []byte) error {
	if len(b) != 8 {
		return errors.New("point: need 8 bytes")
	}
	p.X, p.Y = int32(binary.BigEndian.Uint32(b)), int32(binary.BigEndian.Uint32(b[4:]))
	return nil
}

func roundTrip[T any](t *testing.T, codec cachex.Codec[T], e cachex.Entry[T]) cachex.Entry[T] {
	t.Helper()
	data, err := cachex.EncodeEntry(codec, e)
	require.NoError(t, err)
	got, err := cachex.DecodeEntry(codec, data)
	require.NoError(t, err)
	return got
}

func TestEntryEncoding(t *testing.T) {
	times := cachex.Entry[string]{CachedAt: epoch, FreshUntil: epoch.Add(time.Minute), ExpiresAt: epoch.Add(time.Hour)}

	t.Run("a value and its times survive", func(t *testing.T) {
		e := times
		e.Value = "héllo"
		assert.Equal(t, e, roundTrip(t, cachex.DefaultCodec[string](), e))
	})

	t.Run("a not-found has no value", func(t *testing.T) {
		e := times
		e.NotFound = true
		assert.Equal(t, e, roundTrip(t, cachex.DefaultCodec[string](), e))
	})

	t.Run("zero times stay zero", func(t *testing.T) {
		assert.Equal(t, cachex.Entry[string]{Value: "v"}, roundTrip(t, cachex.DefaultCodec[string](), cachex.Entry[string]{Value: "v"}))
	})

	t.Run("struct values are JSON by default", func(t *testing.T) {
		e := cachex.Entry[*product]{Value: &product{ID: 1, Name: "p"}, CachedAt: epoch}
		got := roundTrip(t, cachex.DefaultCodec[*product](), e)
		assert.Equal(t, e, got)
		assert.NotSame(t, e.Value, got.Value)
	})

	t.Run("BinaryMarshaler values marshal themselves", func(t *testing.T) {
		data, err := cachex.DefaultCodec[point]().Marshal(point{1, 2})
		require.NoError(t, err)
		assert.Len(t, data, 8)
		assert.Equal(t, cachex.Entry[point]{Value: point{1, 2}}, roundTrip(t, cachex.DefaultCodec[point](), cachex.Entry[point]{Value: point{1, 2}}))
	})

	t.Run("strings and bytes are stored as is", func(t *testing.T) {
		data, err := cachex.DefaultCodec[[]byte]().Marshal([]byte{0xff, 0})
		require.NoError(t, err)
		assert.Equal(t, []byte{0xff, 0}, data)
		data, err = cachex.DefaultCodec[string]().Marshal("x")
		require.NoError(t, err)
		assert.Equal(t, []byte("x"), data)
	})

	t.Run("JSONCodec", func(t *testing.T) {
		data, err := cachex.JSONCodec[string]().Marshal("x")
		require.NoError(t, err)
		assert.Equal(t, `"x"`, string(data))
	})

	t.Run("garbage does not decode", func(t *testing.T) {
		long := func(format, flags byte) []byte { return append([]byte{format, flags}, make([]byte, 24)...) }
		for _, data := range [][]byte{nil, {9}, {1, 0, 1, 2}, long(2, 0), long(1, 2), append(long(1, 1), 'x')} {
			_, err := cachex.DecodeEntry(cachex.DefaultCodec[string](), data)
			assert.Error(t, err, "%v", data)
		}
		data, err := cachex.EncodeEntry(cachex.DefaultCodec[string](), cachex.Entry[string]{Value: "not json"})
		require.NoError(t, err)
		_, err = cachex.DecodeEntry(cachex.JSONCodec[string](), data)
		assert.Error(t, err)
	})
}

// singleMap is a SingleBackend; failKey fails every operation on that key.
type singleMap struct {
	*cachextest.Map[string]
	failKey string
	mu      sync.Mutex
}

func (s *singleMap) Get(ctx context.Context, key string) (cachex.Entry[string], bool, error) {
	if key == s.failKey {
		return cachex.Entry[string]{}, false, errBoom
	}
	return s.Map.Get(ctx, key)
}

func (s *singleMap) Set(ctx context.Context, key string, e cachex.Entry[string]) error {
	if key == s.failKey {
		return errBoom
	}
	return s.Map.Set(ctx, key, e)
}

func (s *singleMap) Del(ctx context.Context, key string) error {
	if key == s.failKey {
		return errBoom
	}
	return s.Map.Del(ctx, key)
}

func TestBatched(t *testing.T) {
	cachextest.TestBackend(t, func(t *testing.T) cachex.Backend[string] {
		return cachex.Batched[string](&singleMap{Map: cachextest.NewMap[string]()})
	})

	t.Run("failed keys are reported, the others done", func(t *testing.T) {
		ctx := context.Background()
		b := cachex.Batched[string](&singleMap{Map: cachextest.NewMap[string](), failKey: "bad"})
		e := cachex.Entry[string]{Value: "v", ExpiresAt: time.Now().Add(time.Hour)}
		for _, err := range []error{
			b.SetMany(ctx, map[string]cachex.Entry[string]{"ok": e, "bad": e}),
			func() error { _, err := b.GetMany(ctx, []string{"ok", "bad"}); return err }(),
			b.DelMany(ctx, []string{"ok", "bad"}),
		} {
			var be *cachex.BatchError
			require.ErrorAs(t, err, &be)
			assert.Len(t, be.Errors, 1)
			assert.ErrorIs(t, be.Errors["bad"], errBoom)
		}
	})
}

func TestMapBackend(t *testing.T) {
	cachextest.TestBackend(t, func(t *testing.T) cachex.Backend[string] { return cachextest.NewMap[string]() })
}
