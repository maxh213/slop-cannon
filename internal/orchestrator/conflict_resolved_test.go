package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
)

// P6-68: the re-delivery after a conflict round can come back retryable —
// on a busy run the default branch moves while the check runs and can no
// longer be fast-forwarded. The conductor owes that bead its retry, as it
// does a first delivery's; it used to return the error instead, which
// failed the engineer's finished session and left the bead in delivering
// with nothing to retry it.
func TestConflictResolvedRetriesARetryableRedelivery(t *testing.T) {
	bd := newGlueBD(
		glueIssue("sc-epic", domain.BeadTypeEpic, "open", "epic"),
		glueIssue("sc-1", domain.BeadTypeTask, "delivering", "add the retry"),
	)
	feedback := newGlueFeedback()
	notFF := errors.New("gitx: delivery failed at merge: gitx: default branch cannot be fast-forwarded")
	delivery := &glueDelivery{resolve: func(domain.BeadID) (*DeliveryOutcome, error) {
		return &DeliveryOutcome{Bead: "sc-1", Kind: DeliveryRetryable, State: domain.BeadStateDelivering, Round: 2,
			Reason: "the delivery failed at merge", Err: notFF}, notFF
	}}
	rig := glueSetup(t, bd, ConductorOptions{Feedback: feedback, Delivery: delivery}, config.RunSettings{})
	require.NoError(t, rig.Run.Start(context.Background()))
	require.NoError(t, rig.Run.SetEpic("sc-epic"))

	require.NoError(t, rig.Cond.ConflictResolved(context.Background(), "sc-1"),
		"the engineer's session succeeded; the retry is the conductor's")
	rig.Cond.mu.Lock()
	job, queued := rig.Cond.inflight["sc-1"]
	rig.Cond.mu.Unlock()
	require.True(t, queued, "the bead is queued for another delivery")
	assert.Equal(t, 1, job.attempt)
	assert.Equal(t, domain.BeadStateDelivering, feedback.seen("sc-1"))
}

// A refused request (no outcome at all) is still an error, and nothing is
// queued for it.
func TestConflictResolvedReportsARefusedRedelivery(t *testing.T) {
	bd := newGlueBD(glueIssue("sc-epic", domain.BeadTypeEpic, "open", "epic"))
	refused := errors.New("not deliverable")
	delivery := &glueDelivery{resolve: func(domain.BeadID) (*DeliveryOutcome, error) { return nil, refused }}
	rig := glueSetup(t, bd, ConductorOptions{Feedback: newGlueFeedback(), Delivery: delivery}, config.RunSettings{})
	require.NoError(t, rig.Run.Start(context.Background()))
	err := rig.Cond.ConflictResolved(context.Background(), "sc-1")
	require.ErrorIs(t, err, refused)
	rig.Cond.mu.Lock()
	_, queued := rig.Cond.inflight["sc-1"]
	rig.Cond.mu.Unlock()
	assert.False(t, queued)
}
