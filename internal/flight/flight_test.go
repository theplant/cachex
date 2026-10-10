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

func TestDropKeepsTheOldFlightsWaitersAndItsReplacement(t *testing.T) {
	var g Group[string, string]
	old, _ := g.Claim("k")
	g.Drop("k")
	select {
	case <-old.Done():
		t.Fatal("Drop must not publish")
	default:
	}

	_, leader := g.Claim("k")
	require.True(t, leader, "after Drop, the next claim starts a new flight")

	g.Publish(old, "old", nil)
	<-old.Done()
	v, _ := old.Result()
	assert.Equal(t, "old", v, "the dropped flight's waiters still get its result")
}

func TestPublishOnlyTheFirstResult(t *testing.T) {
	var g Group[string, string]
	f, _ := g.Claim("k")
	g.Publish(f, "first", nil)
	g.Publish(f, "second", nil)
	v, _ := f.Result()
	assert.Equal(t, "first", v)
}

func TestPublishAnswersWaitersAndLaterClaimsUntilForget(t *testing.T) {
	var g Group[string, string]
	f, _ := g.Claim("k")
	g.Publish(f, "v", nil)
	<-f.Done()
	joined, leader := g.Claim("k")
	assert.False(t, leader, "a published flight still registered is joined")
	assert.Same(t, f, joined)
	v, _ := joined.Result()
	assert.Equal(t, "v", v)

	g.Forget("k", f)
	_, leader = g.Claim("k")
	assert.True(t, leader, "after Forget a claim starts anew")
}

func TestForgetLeavesAReplacementRegistered(t *testing.T) {
	var g Group[string, string]
	old, _ := g.Claim("k")
	g.Drop("k")
	replacement, _ := g.Claim("k")
	g.Publish(old, "old", nil)
	g.Forget("k", old)
	f, leader := g.Claim("k")
	assert.False(t, leader)
	assert.Same(t, replacement, f)
}
