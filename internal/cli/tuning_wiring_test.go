package cli

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
)

// TestRunReadsTheTuningKeysFromTheConfig is the last clause of P1-14 and
// P1-18: the keys parse and `sc config show` lists them, but that is only
// worth anything if the run reads them instead of the package constants.
// Nothing the assembled run exposes can be asked what threshold or cap it
// took, so this reads the assembly itself — the same source-level guard
// TestVersionLdflagsMatchMakefile and the troubleshooting audits use.
//
// A key that gains a second reader is fine; a key that loses its only one
// fails here rather than silently falling back to a default the user never
// asked for.
func TestRunReadsTheTuningKeysFromTheConfig(t *testing.T) {
	src, err := os.ReadFile("run.go")
	require.NoError(t, err)
	for key, read := range map[string]string{
		config.KeyMaxDiffBytes:      "gitx.WithMaxDiffBytes(cfg.MaxDiffBytes)",
		config.KeyReReviewThreshold: "ReReviewThreshold: rc.Effective.ReReviewThreshold",
		config.KeyMaxConflictRounds: "MaxConflictRounds: rc.Effective.Limits.MaxConflictRounds",
	} {
		assert.Contains(t, string(src), read,
			"%s is a configuration key the run must read, not a package constant", key)
	}
	// The feedback loop measures the same D-09 ratio between review rounds.
	assert.Contains(t, string(src), "ReReviewThreshold: cfg.ReReviewThreshold")
}

// TestTuningDefaultsAreWhatTheRunFallsBackOn pins the other half: a run
// given no configuration at all still behaves as the documented defaults
// say, because the keys default to the package constants they replaced.
func TestTuningDefaultsAreWhatTheRunFallsBackOn(t *testing.T) {
	d := config.Defaults()
	assert.Equal(t, config.DefaultMaxDiffBytes, d.MaxDiffBytes)
	assert.Equal(t, config.DefaultReReviewThreshold, d.ReReviewThreshold)
	assert.Equal(t, config.DefaultMaxConflictRounds, d.Limits.MaxConflictRounds)
	assert.Equal(t, config.DefaultGraphPollInterval, d.GraphPollInterval.Duration())
}
