package cli

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/gitx"
)

// TestRunAutoMergeIsWiredOnlyWhenTheSnapshotAsksForIt is D-31's switch in
// `sc run`: the gate poller gets something to merge through only when the
// run's frozen snapshot has both safety and auto_merge on and a pull
// request path exists. Every other combination hands it nil, and a nil
// AutoMerge never merges anything.
func TestRunAutoMergeIsWiredOnlyWhenTheSnapshotAsksForIt(t *testing.T) {
	started := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	rc := func(safety, auto bool) *config.RunConfig {
		return &config.RunConfig{Run: domain.Run{
			ID: "20261024-090000-0001", StartedAt: started,
			Config: domain.ConfigSnapshot{
				Safety: safety, AutoMerge: auto,
				AutoMergeChecks: []string{"test"}, AutoMergeSpacing: 7 * time.Minute,
			},
		}}
	}
	pr := &gitx.PRDeliverer{}

	assert.Nil(t, runAutoMerge(rc(true, false), pr, nil, nil), "auto_merge off")
	assert.Nil(t, runAutoMerge(rc(false, true), pr, nil, nil), "safety off")
	assert.Nil(t, runAutoMerge(rc(false, false), pr, nil, nil))
	assert.Nil(t, runAutoMerge(rc(true, true), nil, nil, nil), "no pull request path")

	on := runAutoMerge(rc(true, true), pr, nil, nil)
	require.NotNil(t, on)
	assert.Equal(t, started, on.RunStarted, "ownership is judged from the run's original start")
	assert.Equal(t, []string{"test"}, on.Checks)
	assert.Equal(t, 7*time.Minute, on.Spacing)
	assert.Same(t, pr, on.Branches)
}

// TestRunDeliveryJudgesExistingPullRequestsFromTheRunsStart: without the
// run's start, every existing pull request — the run's own included —
// would fail D-31's ownership check, and each re-delivery to it (a
// send-back, a resume) would go to a person instead.
func TestRunDeliveryJudgesExistingPullRequestsFromTheRunsStart(t *testing.T) {
	started := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	rc := &config.RunConfig{Effective: &config.Effective{}, Run: domain.Run{ID: "20261024-090000-0001", StartedAt: started}}
	opts := runDeliveryOptions(nil, nil, nil, nil, rc, nil)
	assert.Equal(t, started, opts.RunStarted)
}
