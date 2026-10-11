package cachex_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

func TestAKeyWithAStrayClosingBraceIsUnsupported(t *testing.T) {
	ctx := context.Background()
	unsupported := []string{"a}b", "{}x}", "x{}{y}", "}"}
	supported := []string{"plain", "a{b", "{user:1}:profile", "{a}b}", "x{y}z}", "{"}
	data := map[string]string{}
	for _, k := range append(append([]string{}, unsupported...), supported...) {
		data[k] = "v:" + k
	}
	src := newSource(data)
	mem := cachextest.NewMap[string]()
	c := cachex.New[string](src, []cachex.Layer[string]{cachex.NewLayer(mem, cachex.TTL(time.Minute, 0))})

	for _, k := range unsupported {
		_, err := c.Get(ctx, k)
		require.ErrorIs(t, err, cachex.ErrUnsupportedKey, "%q", k)
		assert.Contains(t, err.Error(), k)
		require.ErrorIs(t, c.Set(ctx, k, "x"), cachex.ErrUnsupportedKey, "%q", k)
		require.NoError(t, c.Del(ctx, k), "%q: never stored, so deleted", k)
	}
	assert.Zero(t, src.calls.Load(), "an unsupported key never reaches the source")
	assert.Zero(t, mem.Len(), "nor a layer")

	for _, k := range supported {
		v, err := c.Get(ctx, k)
		require.NoError(t, err, "%q", k)
		assert.Equal(t, "v:"+k, v)
		require.NoError(t, c.Set(ctx, k, "x"), "%q", k)
		require.NoError(t, c.Del(ctx, k), "%q", k)
	}

	all := append(append([]string{}, unsupported...), supported...)
	got, err := c.GetMany(ctx, all)
	var be *cachex.BatchError
	require.ErrorAs(t, err, &be)
	assert.ElementsMatch(t, unsupported, keysOf(be.Errors))
	for _, kerr := range be.Errors {
		assert.ErrorIs(t, kerr, cachex.ErrUnsupportedKey)
	}
	assert.ElementsMatch(t, supported, keysOf(got))

	entries := map[string]string{}
	for _, k := range all {
		entries[k] = "y"
	}
	require.ErrorAs(t, c.SetMany(ctx, entries), &be)
	assert.ElementsMatch(t, unsupported, keysOf(be.Errors))
	for _, k := range supported {
		e, ok, err := mem.Get(ctx, k)
		require.NoError(t, err)
		require.True(t, ok, "%q: the supported keys are written", k)
		assert.Equal(t, "y", e.Value)
	}
	require.NoError(t, c.DelMany(ctx, all))
	assert.Zero(t, mem.Len())
}
