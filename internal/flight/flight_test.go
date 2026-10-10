package flight

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaimAnswersAtOnce(t *testing.T) {
	var g Group[string, string]
	f, leader := g.Claim("k")
	require.True(t, leader, "the first claim leads")
	f2, leader2 := g.Claim("k")
	assert.False(t, leader2, "a second claim waits")
	assert.Same(t, f, f2)
	_, leaderOther := g.Claim("other")
	assert.True(t, leaderOther, "keys are independent")
}

func TestFinishPublishesThenAClaimStartsAnew(t *testing.T) {
	var g Group[string, string]
	f, _ := g.Claim("k")
	g.Finish("k", f, "v", nil)
	<-f.Done()
	v, err := f.Result()
	require.NoError(t, err)
	assert.Equal(t, "v", v)

	f2, leader := g.Claim("k")
	assert.True(t, leader, "a finished flight is not joined")
	assert.NotSame(t, f, f2)
}

func TestDropKeepsTheOldFlightsWaitersAndItsReplacement(t *testing.T) {
	var g Group[string, string]
	old, _ := g.Claim("k")
	g.Drop("k")
	select {
	case <-old.Done():
		t.Fatal("Drop must not publish")
	default:
	}

	replacement, leader := g.Claim("k")
	require.True(t, leader, "after Drop, the next claim starts a new flight")

	g.Finish("k", old, "old", nil)
	<-old.Done()
	v, _ := old.Result()
	assert.Equal(t, "old", v, "the dropped flight's waiters still get its result")
	assert.Equal(t, 1, g.Len(), "finishing the dropped flight leaves its replacement registered")
	f, leader := g.Claim("k")
	assert.False(t, leader)
	assert.Same(t, replacement, f)
}
