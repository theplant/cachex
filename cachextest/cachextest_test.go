package cachextest_test

import (
	"testing"

	"github.com/theplant/cachex/v2"
	"github.com/theplant/cachex/v2/cachextest"
)

func TestLeasingMap(t *testing.T) {
	cachextest.TestBackend(t, func(*testing.T) cachex.Backend[string] { return cachextest.NewLeasingMap[string]() }, []string{"v", "", "w"})
	cachextest.TestLeaser(t, func(*testing.T) cachex.Backend[string] { return cachextest.NewLeasingMap[string]() }, "v")
}
