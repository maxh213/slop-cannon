package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
)

// deliverySettlerFake is the [PRSettler] a safety-off resume is handed.
type deliverySettlerFake struct {
	inStep    bool
	inStepErr error
	gateErr   error
	closeErr  error

	mu      sync.Mutex
	cleared []domain.BeadID
	closed  []int
	comment string
}

func (s *deliverySettlerFake) InStep(context.Context, domain.Bead) (bool, error) {
	return s.inStep, s.inStepErr
}

func (s *deliverySettlerFake) ClearGate(_ context.Context, id domain.BeadID, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gateErr != nil {
		return s.gateErr
	}
	s.cleared = append(s.cleared, id)
	return nil
}

func (s *deliverySettlerFake) ClosePR(_ context.Context, number int, comment string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = append(s.closed, number)
	s.comment = comment
	return s.closeErr
}

// settleHarness is a safety-off deliverer with a settler.
func settleHarness(t *testing.T, s *deliverySettlerFake) *deliveryHarness {
	t.Helper()
	return newDeliveryHarness(t, false, func(o *DeliveryOptions, _ *deliveryHarness) { o.Settler = s })
}

// settleEntry is a resumed bead in delivering behind pull request #7.
func settleEntry(prState string) gitx.ResumeEntry {
	return gitx.ResumeEntry{
		Bead: deliveryBead(domain.BeadStateDelivering), Branch: "sc/sc-68s-add-retry", BranchExists: true,
		PR: &gitx.PullRequest{Number: 7, URL: "https://example.test/pr/7"}, PRState: prState,
		State: domain.BeadStateDelivering,
	}
}

// A run resumed with safety off lands an open pull request's bead the
// safety-off way and closes the pull request, naming the commit.
func TestDelivererSafetyOffLandsAnOpenPullRequest(t *testing.T) {
	t.Parallel()
	s := &deliverySettlerFake{inStep: true}
	h := settleHarness(t, s)
	if err := h.RedeliverPR(context.Background(), settleEntry("OPEN")); err != nil {
		t.Fatalf("RedeliverPR: %v", err)
	}
	if len(h.merge.calls) != 1 {
		t.Fatalf("merges = %d, want 1", len(h.merge.calls))
	}
	if len(s.cleared) != 1 || s.cleared[0] != "sc-68s" {
		t.Errorf("gates cleared = %v, want sc-68s's", s.cleared)
	}
	if len(s.closed) != 1 || s.closed[0] != 7 {
		t.Fatalf("pull requests closed = %v, want [7]", s.closed)
	}
	if !strings.Contains(s.comment, "main as abcdef1234567") {
		t.Errorf("close comment = %q, want it to name the commit", s.comment)
	}
	if got := h.host.states(); len(got) != 1 || got[0] != "delivering→closed" {
		t.Errorf("transitions = %v, want [delivering→closed]", got)
	}
	if w := h.host.warnings(); len(w) != 0 {
		t.Errorf("warnings = %v, want none", w)
	}
}

// Someone pushed to the pull request after sc opened it: landing the local
// branch would leave their commits behind, so nothing is touched.
func TestDelivererSafetyOffLeavesAPullRequestSomeonePushedTo(t *testing.T) {
	t.Parallel()
	for name, s := range map[string]*deliverySettlerFake{
		"behind":        {inStep: false},
		"unknown":       {inStepErr: errors.New("ls-remote failed")},
		"gate not ours": {inStep: true, gateErr: errors.New("not a pull request gate")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := settleHarness(t, s)
			if err := h.RedeliverPR(context.Background(), settleEntry("OPEN")); err != nil {
				t.Fatalf("RedeliverPR: %v", err)
			}
			if len(h.merge.calls) != 0 || len(s.closed) != 0 {
				t.Errorf("merges = %d, closed = %v, want nothing", len(h.merge.calls), s.closed)
			}
			if got := h.host.states(); len(got) != 0 {
				t.Errorf("transitions = %v, want none", got)
			}
			w := h.host.warnings()
			if len(w) != 1 || !strings.Contains(w[0], "leaving it to a human") {
				t.Errorf("warnings = %v, want one leaving PR #7 to a human", w)
			}
		})
	}
}

// A pull request a human already merged closes its bead: nothing is merged
// or pushed again, and the merge is announced once.
func TestDelivererSafetyOffClosesTheBeadOfAMergedPullRequest(t *testing.T) {
	t.Parallel()
	s := &deliverySettlerFake{}
	h := settleHarness(t, s)
	for range 2 {
		entry := settleEntry("MERGED")
		if err := h.RedeliverPR(context.Background(), entry); err != nil {
			t.Fatalf("RedeliverPR: %v", err)
		}
	}
	if len(h.merge.calls) != 0 || len(s.closed) != 0 {
		t.Errorf("merges = %d, closed = %v, want nothing", len(h.merge.calls), s.closed)
	}
	if len(s.cleared) != 2 {
		t.Errorf("gates cleared = %v, want the gate cleared each time", s.cleared)
	}
	merged := h.host.published(events.KindMerged)
	if len(merged) != 1 {
		t.Fatalf("merges announced = %d, want 1", len(merged))
	}
	if m, ok := merged[0].(events.Merged); !ok || m.PRNumber != 7 {
		t.Errorf("announced = %#v, want a merge of PR 7", merged[0])
	}
	if got := h.host.states(); len(got) == 0 || got[0] != "delivering→closed" {
		t.Errorf("transitions = %v, want delivering→closed", got)
	}
}

// Without a settler (an embedder that wires none) the old behaviour stands.
func TestDelivererSafetyOffWithoutASettlerLeavesThePullRequest(t *testing.T) {
	t.Parallel()
	h := newDeliveryHarness(t, false)
	if err := h.RedeliverPR(context.Background(), settleEntry("OPEN")); err != nil {
		t.Fatalf("RedeliverPR: %v", err)
	}
	if len(h.merge.calls) != 0 {
		t.Errorf("merges = %d, want 0", len(h.merge.calls))
	}
	if w := h.host.warnings(); len(w) != 1 {
		t.Errorf("warnings = %v, want one", w)
	}
}
