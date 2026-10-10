package cachex_test

import (
	"context"
	"encoding/binary"
	"errors"
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

// strs and products are values for cachextest.TestBackend.
var (
	strs     = []string{"v", "", "héllo, 世界", "\x00\xff"}
	products = []product{{ID: 1, Name: "a"}, {}, {ID: -2, Name: "héllo, 世界"}}
)

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

// roundTrip stores e through Encoded and reads it back.
func roundTrip[T any](t *testing.T, codec cachex.Codec[T], e cachex.Entry[T]) cachex.Entry[T] {
	t.Helper()
	ctx := context.Background()
	b := cachex.Encoded(newByteMap(), codec, nil)
	require.NoError(t, b.Set(ctx, "k", e))
	got, ok, err := b.Get(ctx, "k")
	require.NoError(t, err)
	require.True(t, ok)
	return got
}

func TestEntryEncoding(t *testing.T) {
	now := time.Now().UTC().Round(0) // as decoded: UTC, no monotonic reading
	later := now.Add(time.Hour)
	times := cachex.Entry[string]{CachedAt: now, FreshUntil: now.Add(time.Minute), ExpiresAt: later}

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
		e := cachex.Entry[string]{Value: "v", ExpiresAt: later}
		assert.Equal(t, e, roundTrip(t, cachex.DefaultCodec[string](), e))
	})

	t.Run("struct values are JSON by default", func(t *testing.T) {
		e := cachex.Entry[*product]{Value: &product{ID: 1, Name: "p"}, CachedAt: now, ExpiresAt: later}
		got := roundTrip(t, cachex.DefaultCodec[*product](), e)
		assert.Equal(t, e, got)
		assert.NotSame(t, e.Value, got.Value)
	})

	t.Run("BinaryMarshaler values marshal themselves", func(t *testing.T) {
		data, err := cachex.DefaultCodec[point]().Marshal(point{1, 2})
		require.NoError(t, err)
		assert.Len(t, data, 8)
		e := cachex.Entry[point]{Value: point{1, 2}, ExpiresAt: later}
		assert.Equal(t, e, roundTrip(t, cachex.DefaultCodec[point](), e))
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
		ctx := context.Background()
		long := func(format, flags byte) []byte { return append([]byte{format, flags}, make([]byte, 24)...) }
		for _, data := range [][]byte{nil, {9}, {1, 0, 1, 2}, long(2, 0), long(1, 2), append(long(1, 1), 'x')} {
			m := newByteMap()
			require.NoError(t, m.Set(ctx, "k", data, later))
			_, ok, err := cachex.Encoded[string](m, nil, nil).Get(ctx, "k")
			require.NoError(t, err)
			assert.False(t, ok, "%v", data)
		}
	})
}

// singleMap is a SingleBackend; failKey fails every operation on that key.
type singleMap struct {
	*cachextest.Map[string]
	failKey string
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
	}, strs)

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
	cachextest.TestBackend(t, func(t *testing.T) cachex.Backend[string] { return cachextest.NewMap[string]() }, strs)
	cachextest.TestBackend(t, func(t *testing.T) cachex.Backend[product] { return cachextest.NewMap[product]() }, products)
}
