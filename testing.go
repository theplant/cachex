package cachex

import (
	"sync"
	"time"
)

// MockClock provides a controllable time source for testing
type MockClock struct {
	mu      sync.RWMutex
	current time.Time
}

// NewMockClock creates a new mock clock starting at the given time
func NewMockClock(start time.Time) *MockClock {
	return &MockClock{
		current: start,
	}
}

// Now returns the current mocked time
func (m *MockClock) Now() time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

// Advance moves the clock forward by the given duration
func (m *MockClock) Advance(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.current = m.current.Add(d)
}

// Set sets the clock to a specific time
func (m *MockClock) Set(t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.current = t
}

// Install replaces the global NowFunc with this mock clock
func (m *MockClock) Install() func() {
	originalNowFunc := NowFunc
	NowFunc = m.Now
	return func() {
		NowFunc = originalNowFunc
	}
}
