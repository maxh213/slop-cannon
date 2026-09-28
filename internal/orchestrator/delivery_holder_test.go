package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
)

func TestHolderOfTakesTheActorPrefixOff(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]domain.AgentID{
		"sc:eng-sc-68s-r2": "eng-sc-68s-r2",
		" sc:eng-x ":       "eng-x",
		"eng-x":            "eng-x",
		"alice":            "alice",
		"":                 "",
	} {
		if got := holderOf(in); got != want {
			t.Errorf("holderOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// A conflict round goes back to the bead's holder by agent id. Handing on
// bd's actor form made the next claim write the assignee `sc:sc:<agent>`,
// which no lease renewal could match.
func TestDelivererConflictRoundNamesTheHolderByAgentID(t *testing.T) {
	t.Parallel()
	h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
		h.merge.results, h.merge.errs = nil, []error{deliveryConflictErr("a.go")}
	})
	out, err := h.Deliver(context.Background(), DeliveryRequest{Bead: deliveryBead(domain.BeadStateApproved), Round: 1})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if out.Kind != DeliveryConflict {
		t.Fatalf("kind = %s, want conflict", out.Kind)
	}
	p, ok := h.PendingFor("sc-68s")
	if !ok {
		t.Fatal("no conflict round pending")
	}
	if p.Agent != "eng-sc-68s-r1" {
		t.Errorf("conflict engineer = %q, want the holder's agent id eng-sc-68s-r1", p.Agent)
	}
	demands, err := h.Demands(context.Background())
	if err != nil || len(demands) != 1 {
		t.Fatalf("demands = %v, %v; want one", demands, err)
	}
	if demands[0].HeldBy != "eng-sc-68s-r1" {
		t.Errorf("HeldBy = %q, want eng-sc-68s-r1", demands[0].HeldBy)
	}
}

// deliveryNothingErr is the merge failure of a branch with no commit.
func deliveryNothingErr() error {
	return &gitx.DeliveryError{
		Stage: gitx.StageMerge,
		Err:   fmt.Errorf("%w: sc/sc-68s-add-retry carries no commit beyond 0000000", gitx.ErrNothingToDeliver),
	}
}

func deliveryVerdicts(outcomes ...domain.VerdictOutcome) []domain.Verdict {
	lenses := []domain.Lens{domain.LensUX, domain.LensQA, domain.LensTest}
	var vs []domain.Verdict
	for i, o := range outcomes {
		vs = append(vs, domain.Verdict{Lens: lenses[i%len(lenses)], Round: 1, Outcome: o})
	}
	return vs
}

// An approved branch with nothing to merge: the testers checked the
// acceptance against the default branch as it is, so the bead is closed
// rather than parked for a human, and no merge is announced.
func TestDelivererClosesAnApprovedBeadThatHasNothingToMerge(t *testing.T) {
	t.Parallel()
	h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
		h.merge.results, h.merge.errs = nil, []error{deliveryNothingErr()}
	})
	out, err := h.Deliver(context.Background(), DeliveryRequest{
		Bead: deliveryBead(domain.BeadStateApproved), Round: 1,
		Verdicts: deliveryVerdicts(domain.VerdictApprove, domain.VerdictApprove, domain.VerdictApprove),
	})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if out.Kind != DeliveryAlreadySatisfied || out.State != domain.BeadStateClosed {
		t.Fatalf("outcome = %s/%s, want already_satisfied/closed", out.Kind, out.State)
	}
	if !strings.Contains(out.Summary(), "acceptance criteria already hold on main") {
		t.Errorf("summary = %q", out.Summary())
	}
	if got := h.host.states(); len(got) != 2 || got[1] != "delivering→closed" {
		t.Errorf("transitions = %v, want approved→delivering then delivering→closed", got)
	}
	if n := len(h.host.published(events.KindMerged)); n != 0 {
		t.Errorf("merges announced = %d, want 0: nothing merged", n)
	}
	if w := h.host.warnings(); len(w) != 1 || !strings.Contains(w[0], "closed with nothing to merge") {
		t.Errorf("warnings = %v, want one saying the bead closed with nothing to merge", w)
	}
}

// Without unanimous approval the empty branch is still a person's call
// (D-20): nothing proves the work is already there.
func TestDelivererHandsOffAnEmptyBranchWithoutUnanimousApproval(t *testing.T) {
	t.Parallel()
	for name, vs := range map[string][]domain.Verdict{
		"no verdicts":     nil,
		"one not approve": deliveryVerdicts(domain.VerdictApprove, domain.VerdictChanges, domain.VerdictApprove),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newDeliveryHarness(t, false, func(_ *DeliveryOptions, h *deliveryHarness) {
				h.merge.results, h.merge.errs = nil, []error{deliveryNothingErr()}
			})
			out, err := h.Deliver(context.Background(), DeliveryRequest{
				Bead: deliveryBead(domain.BeadStateApproved), Round: 1, Verdicts: vs,
			})
			if err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			if out.Kind != DeliveryHandedOff || out.State != domain.BeadStateNeedsHuman {
				t.Errorf("outcome = %s/%s, want handed_off/needs_human", out.Kind, out.State)
			}
		})
	}
}
