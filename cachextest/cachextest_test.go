package cachextest_test

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func TestBus(t *testing.T) {
	cachextest.TestBroadcast(t, func(*testing.T) (cachex.Broadcast, cachex.Broadcast) {
		bus := cachextest.NewBus()
		return bus.Broadcast(), bus.Broadcast()
	})
}

// blockedIn reports whether a goroutine is blocked (on a channel or a lock)
// with fn on its stack.
func blockedIn(fn string) bool {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	for _, g := range strings.Split(string(buf), "\n\n") {
		header, _, _ := strings.Cut(g, "\n")
		if strings.Contains(g, fn) && (strings.Contains(header, "chan send") || strings.Contains(header, "select") || strings.Contains(header, "sync.Mutex.Lock")) {
			return true
		}
	}
	return false
}

func TestABusListenerThatWentAwayDoesNotBlockTheBus(t *testing.T) {
	// a listener 1024 messages behind, or 16 readies, blocks a Publish or a
	// Reconnect; once it is gone, they go on
	for name, push := range map[string]func(bus *cachextest.Bus, b cachex.Broadcast){
		"Publish":   func(_ *cachextest.Bus, b cachex.Broadcast) { _ = b.Publish(context.Background(), []byte("m")) },
		"Reconnect": func(bus *cachextest.Bus, _ cachex.Broadcast) { bus.Reconnect() },
	} {
		t.Run(name, func(t *testing.T) {
			bus := cachextest.NewBus()
			b := bus.Broadcast()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ready := make(chan struct{}, 1)
			entered, leave, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
			go func() {
				defer close(exited)
				defer func() { _ = recover() }()
				_ = b.Listen(ctx, func() {
					select {
					case ready <- struct{}{}:
					default:
					}
				}, func([]byte) {
					close(entered)
					<-leave
					panic("the listener goes away")
				})
			}()
			<-ready
			require.NoError(t, b.Publish(ctx, []byte("first")))
			<-entered // stuck in deliver: nothing more is taken off its channels
			for range 1024 {
				require.NoError(t, b.Publish(ctx, []byte("m")))
			}
			for range 16 {
				bus.Reconnect()
			}
			done := make(chan struct{})
			go func() { defer close(done); push(bus, b) }()
			require.Eventually(t, func() bool {
				return blockedIn("cachextest.(*Bus).Reconnect") || blockedIn("cachextest.busBroadcast.Publish")
			}, 10*time.Second, time.Millisecond, "the listener is full")
			close(leave)
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("blocked for good on a listener that went away")
			}
			select {
			case <-exited:
			case <-time.After(10 * time.Second):
				t.Fatal("the listener never finished going away")
			}
		})
	}
}
