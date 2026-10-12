package cachextest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

func TestLeasingMap(t *testing.T) {
	cachextest.TestBackend(t, func(*testing.T) cachex.Backend[string] { return cachextest.NewLeasingMap[string]() }, []string{"v", "", "w"})
	cachextest.TestLeaser(t, func(*testing.T) cachex.Backend[string] { return cachextest.NewLeasingMap[string]() }, "v")
}

func TestLocality(t *testing.T) {
	var m any = cachextest.NewMap[string]()
	_, local := m.(cachex.LocalBackend)
	assert.True(t, local, "a Map is local")
	var l any = cachextest.NewLeasingMap[string]()
	_, local = l.(cachex.LocalBackend)
	assert.False(t, local, "a leasing Map stands for a layer instances share")
	cachextest.TestBackend(t, func(*testing.T) cachex.Backend[string] { return cachextest.NewMap[string]() }, []string{"v", "", "w"})
}
