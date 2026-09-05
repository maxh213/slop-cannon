package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/config"
	"slop-cannon/internal/gitx"
)

// TestTuningDefaultsAreOneNumber pins the Phase 3/4 tuning defaults to the
// packages that consume them (P1-14). internal/beads and internal/gitx
// import internal/config, so the constants cannot live there and be
// referenced here; this test is what keeps the two copies equal, and it is
// an external test package because config itself must not import them.
func TestTuningDefaultsAreOneNumber(t *testing.T) {
	assert.Equal(t, beads.DefaultPollInterval, config.DefaultGraphPollInterval,
		"graph_poll_interval defaults to beads.DefaultPollInterval")
	assert.Equal(t, gitx.DefaultMaxDiffBytes, config.DefaultMaxDiffBytes,
		"max_diff_bytes defaults to gitx.DefaultMaxDiffBytes")
	assert.InEpsilon(t, gitx.DefaultReReviewThreshold, config.DefaultReReviewThreshold, 1e-9,
		"re_review_threshold defaults to gitx.DefaultReReviewThreshold (D-09)")
	assert.Equal(t, gitx.DefaultPRPollInterval, config.DefaultPRPollInterval,
		"pr_poll_interval is already shared the other way round: gitx aliases config")
}
