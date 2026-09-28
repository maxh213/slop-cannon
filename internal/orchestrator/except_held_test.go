package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

type heldBeads map[domain.BeadID]bool

func (h heldBeads) Holds(b domain.BeadID) bool { return h[b] }

func TestExceptHeldDropsOnlyTheHeldBeadsDemands(t *testing.T) {
	t.Parallel()
	boom := errors.New("bd is down")
	src := WorkSourceFunc(func(context.Context) ([]Demand, error) {
		return []Demand{
			schedReview("sc-1", domain.LensQA, 1),
			schedReview("sc-2", domain.LensQA, 1),
			{Kind: DemandPlan}, // no bead: never held
		}, boom
	})
	ds, err := ExceptHeld(heldBeads{"sc-1": true}, src).Demands(context.Background())
	require.ErrorIs(t, err, boom, "the source's error still reaches the scheduler")
	var beads []domain.BeadID
	for _, d := range ds {
		beads = append(beads, d.Bead)
	}
	assert.Equal(t, []domain.BeadID{"sc-2", ""}, beads)

	assert.Nil(t, ExceptHeld(heldBeads{}, nil))
	var noHolder BeadHolder
	assert.NotNil(t, ExceptHeld(noHolder, src), "no holder, nothing filtered")
}

// The deliverer holds a bead from the conflict round it opens until the
// round is given up, and not otherwise.
func TestDelivererHoldsABeadForItsConflictRound(t *testing.T) {
	t.Parallel()
	h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
		h.merge.results, h.merge.errs = nil, []error{deliveryConflictErr("a.go")}
	})
	assert.False(t, h.Holds("sc-68s"))
	out, err := h.Deliver(context.Background(), DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved), Round: 1})
	require.NoError(t, err)
	require.Equal(t, DeliveryConflict, out.Kind)
	assert.True(t, h.Holds("sc-68s"), "the conflict round is pending")
	assert.False(t, h.Holds("sc-other"))

	require.NoError(t, h.AbandonConflict(context.Background(), "sc-68s", "gave up", nil))
	assert.False(t, h.Holds("sc-68s"), "an abandoned round releases the bead")

	var none *Deliverer
	assert.False(t, none.Holds("sc-68s"), "a nil deliverer holds nothing")
}

// P6-66: a resumed run found a bead mid-rebase. The deliverer offered its
// conflict round and the feedback loop, restoring the bead from its
// comments, offered a review of it. Only the conflict engineer may fire; a
// review of another bead still does.
func TestSchedulerFiresNothingElseOnABeadHeldForAConflictRound(t *testing.T) {
	gate := newSchedGate()
	conflict := Demand{Kind: DemandImplement, Task: events.TaskResolveConflict, Bead: "sc-1", Claim: ClaimRework}
	feedback := schedStatic(
		schedReview("sc-1", domain.LensUX, 1), schedReview("sc-1", domain.LensQA, 1), schedReview("sc-1", domain.LensTest, 1),
		schedReview("sc-2", domain.LensQA, 1),
	)
	f := schedSetup(t, 10, SchedulerOptions{
		Work: WorkSources(ExceptHeld(heldBeads{"sc-1": true}, feedback), schedStatic(conflict)),
		Fire: gate,
	})
	t.Cleanup(gate.Open)
	schedEventually(t, "the conflict engineer and sc-2's tester", func() bool { return len(gate.Firings()) == 2 })
	for i := 0; i < 5; i++ {
		f.Clock.Advance(2 * DefaultTickInterval)
	}
	var fired []string
	for _, fi := range gate.Firings() {
		fired = append(fired, string(fi.Agent.Role)+"@"+string(fi.Agent.Bead))
	}
	assert.ElementsMatch(t, []string{"engineer@sc-1", "tester@sc-2"}, fired)
}
