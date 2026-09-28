package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/domain"
)

// A bead the product owner gave a longer timeout gets it, capped at the
// maximum; it never shortens the run's own limit.
func TestSupervisorTimeoutForABead(t *testing.T) {
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: supApproval("alpha")})
	perBead := map[domain.BeadID]time.Duration{"long": 10 * time.Hour, "huge": 48 * time.Hour, "short": time.Minute}
	s := f.supervisor(t, func(o *SupervisorOptions) {
		o.Timeout = 6 * time.Hour
		o.MaxTimeout = 24 * time.Hour
		o.BeadTimeout = func(_ context.Context, b domain.BeadID) time.Duration { return perBead[b] }
	})
	ctx := context.Background()
	assert.Equal(t, 10*time.Hour, s.timeoutFor(ctx, "long"))
	assert.Equal(t, 24*time.Hour, s.timeoutFor(ctx, "huge"), "capped at limits.max_agent_timeout")
	assert.Equal(t, 6*time.Hour, s.timeoutFor(ctx, "short"), "never below the run's own limit")
	assert.Equal(t, 6*time.Hour, s.timeoutFor(ctx, "none"))
	assert.Equal(t, 6*time.Hour, s.timeoutFor(ctx, ""), "a firing with no bead (planning) keeps the run's limit")

	plain := f.supervisor(t, func(o *SupervisorOptions) { o.Timeout = 6 * time.Hour })
	assert.Equal(t, 6*time.Hour, plain.timeoutFor(ctx, "long"), "without a lookup nothing changes")
}
