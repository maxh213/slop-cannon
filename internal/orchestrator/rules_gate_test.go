package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/cartridge/structured"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// P9-06: the rules compliance gate. The tests come in two halves — the
// state machine (what the gate remembers, what it refuses) and the loop
// (the same invariants driven through the real feedback loop, the real
// supervisor and the mock cartridge, plus the resume path, which is the one
// place a comment written by somebody else is applied without any role
// check at all).

// rgRun is the run id the gate's warnings are stamped with.
const rgRun = domain.RunID("20260903-100000-rg00")

// rgItem is one violation of a rules file, as a tester raises it.
func rgItem(id, title string, status domain.ItemStatus) domain.FeedbackItem {
	return domain.FeedbackItem{
		ID: id, Severity: domain.SeverityBlocker, Category: domain.CategoryRules,
		Title: title, Detail: "the change widens a public API without a bead",
		File: "retry.go", Line: 7, Status: status,
	}
}

// rgVerdict is one lens's verdict carrying items.
func rgVerdict(lens domain.Lens, round int, outcome domain.VerdictOutcome, items ...domain.FeedbackItem) domain.Verdict {
	return domain.Verdict{Lens: lens, Round: round, Outcome: outcome, Summary: "seen", Items: items}
}

// rgSink collects the warnings the gate publishes, and can forward them on
// to a bus that does not exist yet: fbSetup builds the run's bus after the
// gate has to be given one.
type rgSink struct {
	mu   sync.Mutex
	seen []events.Warning
	to   Publisher
}

// Publish implements [Publisher].
func (s *rgSink) Publish(ev events.Event) {
	s.mu.Lock()
	if w, ok := ev.(events.Warning); ok {
		s.seen = append(s.seen, w)
	}
	to := s.to
	s.mu.Unlock()
	if to != nil {
		to.Publish(ev)
	}
}

// forward sends everything published from now on to the run's bus as well.
func (s *rgSink) forward(to Publisher) {
	s.mu.Lock()
	s.to = to
	s.mu.Unlock()
}

// Warnings is what the gate published, in order.
func (s *rgSink) Warnings() []events.Warning {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]events.Warning(nil), s.seen...)
}

// rgGate is a gate with a collecting sink and a frozen clock.
func rgGate(t *testing.T) (*RulesGate, *rgSink) {
	t.Helper()
	sink := &rgSink{}
	g := NewRulesGate(RulesGateOptions{Run: rgRun, Bus: sink, Now: func() time.Time { return fbEpoch }})
	return g, sink
}

// --- the state machine -------------------------------------------------

func TestRulesGateShippingEventsAreDerivedFromTheTransitionTable(t *testing.T) {
	var shipping []BeadEvent
	for _, ev := range BeadEvents() {
		if RulesShippingEvent(ev) {
			shipping = append(shipping, ev)
		}
	}
	assert.Equal(t, []BeadEvent{
		BeadEventApproved, BeadEventApprovedWithNotes,
		BeadEventDeliveryStarted, BeadEventDeliveryRetried, BeadEventDelivered,
	}, shipping, "exactly the events that land a bead in approved, delivering or closed")

	// And the states they land in are the three the gate calls shipping.
	for _, s := range domain.WorkflowBeadStates() {
		want := s == domain.BeadStateApproved || s == domain.BeadStateDelivering || s == domain.BeadStateClosed
		assert.Equalf(t, want, RulesShippingState(s), "state %s", s)
	}
}

func TestRulesGateHoldsAViolationOpenUntilATesterResolvesIt(t *testing.T) {
	g, _ := rgGate(t)
	assert.False(t, g.Blocked(fbBead), "a bead nobody has reviewed carries no violation")

	g.ObserveVerdict(fbBead, rgVerdict(domain.LensQA, 1, domain.VerdictChanges,
		rgItem("qa-r1-1", "widens the public API", domain.ItemOpen)))
	require.True(t, g.Blocked(fbBead))
	open := g.Open(fbBead)
	require.Len(t, open, 1)
	assert.Equal(t, RulesViolation{
		Bead: fbBead, Item: "qa-r1-1", Title: "widens the public API",
		Round: 1, Lenses: []domain.Lens{domain.LensQA},
	}, open[0])
	assert.Equal(t, "qa-r1-1 (widens the public API)", open[0].String())

	// Neither of the two statuses that settle an ordinary item settles this
	// one: the code is still broken.
	for _, s := range []domain.ItemStatus{domain.ItemAccepted, domain.ItemOverruled, domain.ItemHeld} {
		g.ObserveVerdict(fbBead, rgVerdict(domain.LensQA, 2, domain.VerdictApprove,
			rgItem("qa-r1-1", "widens the public API", s)))
		assert.Truef(t, g.Blocked(fbBead), "status %s must not settle a rules violation", s)
	}

	// Only the tester that raised it saying the diff fixed it does.
	g.ObserveVerdict(fbBead, rgVerdict(domain.LensQA, 3, domain.VerdictApprove,
		rgItem("qa-r1-1", "widens the public API", domain.ItemResolved)))
	assert.False(t, g.Blocked(fbBead))
	assert.Empty(t, g.Open(fbBead))
}

func TestRulesGateNeedsEveryLensThatRaisedItToResolveIt(t *testing.T) {
	g, _ := rgGate(t)
	item := rgItem("qa-r1-1", "widens the public API", domain.ItemOpen)
	g.ObserveVerdict(fbBead, rgVerdict(domain.LensQA, 1, domain.VerdictChanges, item))
	g.ObserveVerdict(fbBead, rgVerdict(domain.LensTest, 1, domain.VerdictChanges, item))
	require.Equal(t, []domain.Lens{domain.LensQA, domain.LensTest}, g.Open(fbBead)[0].Lenses)

	resolved := rgItem("qa-r1-1", "widens the public API", domain.ItemResolved)
	g.ObserveVerdict(fbBead, rgVerdict(domain.LensQA, 2, domain.VerdictApprove, resolved))
	assert.True(t, g.Blocked(fbBead), "the other lens has not seen the fix yet")
	g.ObserveVerdict(fbBead, rgVerdict(domain.LensTest, 2, domain.VerdictApprove, resolved))
	assert.False(t, g.Blocked(fbBead))
}

func TestRulesGateIgnoresItemsThatAreNotRulesViolations(t *testing.T) {
	g, _ := rgGate(t)
	g.ObserveVerdict(fbBead, rgVerdict(domain.LensQA, 1, domain.VerdictChanges,
		fbItemAt("qa-r1-1", "the retry loop spins", "retry.go", domain.SeverityBlocker, domain.ItemOpen)))
	assert.False(t, g.Blocked(fbBead))
	assert.Equal(t, RulesGateStats{}, g.Stats())
}

func TestRulesGateRefusesAVerdictThatMishandlesAViolation(t *testing.T) {
	tests := []struct {
		name string
		item domain.FeedbackItem
		want string
	}{
		{"not a blocker", func() domain.FeedbackItem {
			it := rgItem("qa-r1-1", "widens the public API", domain.ItemOpen)
			it.Severity = domain.SeverityMinor
			return it
		}(), "its severity is \"blocker\""},
		{"accepted", rgItem("qa-r1-1", "widens the public API", domain.ItemAccepted),
			"cannot be accepted"},
		{"overruled", rgItem("qa-r1-1", "widens the public API", domain.ItemOverruled),
			"cannot be overruled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := rgGate(t)
			err := g.CheckVerdict(fbBead, rgVerdict(domain.LensQA, 1, domain.VerdictChanges, tc.item))
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrRulesBlocker)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestRulesGateRefusesAViolationRelabelledOutOfTheCategory(t *testing.T) {
	g, _ := rgGate(t)
	g.ObserveVerdict(fbBead, rgVerdict(domain.LensQA, 1, domain.VerdictChanges,
		rgItem("qa-r1-1", "widens the public API", domain.ItemOpen)))

	relabelled := rgItem("qa-r1-1", "widens the public API", domain.ItemResolved)
	relabelled.Category = "style"
	err := g.CheckVerdict(fbBead, rgVerdict(domain.LensQA, 2, domain.VerdictApprove, relabelled))
	require.ErrorIs(t, err, ErrRulesBlocker)
	assert.Contains(t, err.Error(), "cannot be moved to category \"style\"")

	// And folding it in anyway leaves it a violation: the category the gate
	// first saw is the one that counts.
	g.ObserveVerdict(fbBead, rgVerdict(domain.LensQA, 2, domain.VerdictApprove, relabelled))
	assert.True(t, g.Blocked(fbBead))
}

func TestRulesGateRefusesAnArbitrationInTheEngineersFavour(t *testing.T) {
	g, _ := rgGate(t)
	g.ObserveVerdict(fbBead, rgVerdict(domain.LensQA, 1, domain.VerdictChanges,
		rgItem("qa-r1-1", "widens the public API", domain.ItemOpen)))

	err := g.CheckArbitration(fbBead, domain.Arbitration{
		ItemID: "qa-r1-1", Ruling: domain.RulingUpholdEngineer, Rationale: "the API is internal in practice",
	})
	require.ErrorIs(t, err, ErrRulesBlocker)
	assert.Contains(t, err.Error(), "uphold the tester or impose a compromise")

	// The two rulings that still respect the rule are the product owner's
	// to give.
	for _, r := range []domain.ArbitrationRuling{domain.RulingUpholdTester, domain.RulingCompromise} {
		assert.NoErrorf(t, g.CheckArbitration(fbBead, domain.Arbitration{ItemID: "qa-r1-1", Ruling: r}), "ruling %s", r)
	}
	// An item the gate never saw as a violation is nobody's business here.
	assert.NoError(t, g.CheckArbitration(fbBead, domain.Arbitration{
		ItemID: "ux-r1-2", Ruling: domain.RulingUpholdEngineer,
	}))

	// A ruling never settles a violation, even one that reaches the gate.
	g.Observe(fbBead, domain.Arbitration{ItemID: "qa-r1-1", Ruling: domain.RulingUpholdEngineer})
	assert.True(t, g.Blocked(fbBead))
}

func TestRulesGateRefusesApproveWithNotesWhileAViolationIsOpen(t *testing.T) {
	g, _ := rgGate(t)
	g.ObserveVerdict(fbBead, rgVerdict(domain.LensQA, 1, domain.VerdictChanges,
		rgItem("qa-r1-1", "widens the public API", domain.ItemOpen)))

	err := g.CheckEscalation(fbBead, domain.Escalation{
		Outcome: domain.EscalationApproveWithNotes, Notes: "we will follow up",
	})
	require.ErrorIs(t, err, ErrRulesBlocker)
	assert.Contains(t, err.Error(), "narrow the scope or block the bead")

	// The two outcomes that do not ship the violation are allowed.
	assert.NoError(t, g.CheckEscalation(fbBead, domain.Escalation{
		Outcome: domain.EscalationBlock, Summary: "a human should look",
	}))
	assert.NoError(t, g.CheckEscalation(fbBead, domain.Escalation{
		Outcome: domain.EscalationNarrowScope, NewAcceptance: "the error path only",
	}))

	// Once the violation is resolved the ruling is the product owner's again.
	g.ObserveVerdict(fbBead, rgVerdict(domain.LensQA, 2, domain.VerdictApprove,
		rgItem("qa-r1-1", "widens the public API", domain.ItemResolved)))
	assert.NoError(t, g.CheckEscalation(fbBead, domain.Escalation{
		Outcome: domain.EscalationApproveWithNotes, Notes: "the rest is a nit",
	}))
}

func TestRulesGateNarrowScopeClearsTheSlate(t *testing.T) {
	g, warnings := rgGate(t)
	g.ObserveVerdict(fbBead, rgVerdict(domain.LensQA, 1, domain.VerdictChanges,
		rgItem("qa-r1-1", "widens the public API", domain.ItemOpen)))
	require.True(t, g.Blocked(fbBead))

	// The items were raised against acceptance criteria the ruling replaced
	// (D-08), so the loop drops them and so does the gate.
	g.Observe(fbBead, domain.Escalation{Outcome: domain.EscalationNarrowScope, NewAcceptance: "the error path only"})
	assert.False(t, g.Blocked(fbBead))

	// P9-06 review, round 4: this is the one thing that clears the gate's
	// memory without the code being fixed, and agents.ValidateEscalation
	// only asks such a ruling for a non-empty new_acceptance — so the drop
	// is counted and announced rather than quietly taken.
	assert.Equal(t, 1, g.Stats().Narrowed)
	require.Len(t, warnings.Warnings(), 1)
	w := warnings.Warnings()[0]
	assert.Equal(t, SourceRules, w.Source)
	assert.Equal(t, fbBead, w.Bead)
	assert.Contains(t, w.Message, "narrow_scope")
	assert.Contains(t, w.Err, "qa-r1-1", "the warning names what was dropped")
	assert.Equal(t, "rules: 1 violation dropped by narrow_scope",
		RunVitals{}.WithRules(g).RulesLine(), "and `sc status` says so too")

	// A ruling that had nothing to drop says nothing.
	before := len(warnings.Warnings())
	g.Observe(domain.BeadID("sc-none"), domain.Escalation{Outcome: domain.EscalationNarrowScope, NewAcceptance: "x"})
	assert.Len(t, warnings.Warnings(), before)
	assert.Equal(t, 1, g.Stats().Narrowed)

	// Blocking the bead does not: the violation goes to the human with it.
	g.ObserveVerdict(fbBead, rgVerdict(domain.LensQA, 1, domain.VerdictChanges,
		rgItem("qa-r2-1", "still widens the public API", domain.ItemOpen)))
	g.Observe(fbBead, domain.Escalation{Outcome: domain.EscalationBlock, Summary: "human"})
	assert.True(t, g.Blocked(fbBead))
}

func TestRulesGateRefusesEveryTransitionThatShipsTheBead(t *testing.T) {
	g, warnings := rgGate(t)
	g.ObserveVerdict(fbBead, rgVerdict(domain.LensQA, 1, domain.VerdictChanges,
		rgItem("qa-r1-1", "widens the public API", domain.ItemOpen)))

	for _, ev := range BeadEvents() {
		err := g.CheckTransition(fbBead, ev)
		if RulesShippingEvent(ev) {
			require.ErrorIsf(t, err, ErrRulesBlocker, "event %s must be refused", ev)
			assert.Containsf(t, err.Error(), string(ev), "event %s names itself", ev)
			continue
		}
		assert.NoErrorf(t, err, "event %s ships nothing and must pass", ev)
	}
	// The bead can still be worked on, escalated, parked or failed, which
	// is the whole point of refusing only the shipping edges.
	assert.NoError(t, g.CheckTransition(fbBead, BeadEventEscalated))
	assert.NoError(t, g.CheckTransition(fbBead, BeadEventBlocked))
	assert.Empty(t, warnings.Warnings(), "a check on its own publishes nothing")

	// A bead with no violation passes everything.
	assert.NoError(t, g.CheckTransition("sc-clean", BeadEventApproved))
}

func TestRulesGateGuardedStoreRefusesTheWritesThatShip(t *testing.T) {
	g, warnings := rgGate(t)
	g.ObserveVerdict(fbBead, rgVerdict(domain.LensQA, 1, domain.VerdictChanges,
		rgItem("qa-r1-1", "widens the public API", domain.ItemOpen)))
	inner := &rgStore{state: domain.BeadStateInReview}
	store := g.GuardStore(inner)
	ctx := context.Background()

	for _, s := range []domain.BeadState{domain.BeadStateApproved, domain.BeadStateDelivering} {
		_, err := store.SetStatus(ctx, fbBead, s)
		require.ErrorIsf(t, err, ErrRulesBlocker, "status %s", s)
	}
	_, err := store.Close(ctx, fbBead, "delivered")
	require.ErrorIs(t, err, ErrRulesBlocker)
	assert.Empty(t, inner.Moves(), "not one of them reached bd")

	// Everything that keeps the bead moving towards a fix passes through.
	_, err = store.SetStatus(ctx, fbBead, domain.BeadStateChangesRequested)
	require.NoError(t, err)
	_, err = store.ClaimFor(ctx, fbBead, "eng-1")
	require.NoError(t, err)
	_, err = store.Reclaim(ctx, fbBead, "eng-2")
	require.NoError(t, err)
	_, err = store.Release(ctx, fbBead)
	require.NoError(t, err)
	assert.Equal(t, []string{"status:changes_requested", "claim:eng-1", "reclaim:eng-2", "release"}, inner.Moves())

	// Each refusal is counted and announced.
	assert.Equal(t, RulesGateStats{Beads: 1, Blocked: 1, Violations: 1, Refused: 3}, g.Stats())
	published := warnings.Warnings()
	require.Len(t, published, 3)
	assert.Equal(t, SourceRules, published[0].Source)
	assert.Equal(t, fbBead, published[0].Bead)
	assert.Equal(t, rgRun, published[0].Run)

	// And once the violation is resolved the same writes land.
	g.ObserveVerdict(fbBead, rgVerdict(domain.LensQA, 2, domain.VerdictApprove,
		rgItem("qa-r1-1", "widens the public API", domain.ItemResolved)))
	_, err = store.SetStatus(ctx, fbBead, domain.BeadStateApproved)
	require.NoError(t, err)
	_, err = store.Close(ctx, fbBead, "delivered")
	require.NoError(t, err)
	assert.Equal(t, []string{"status:approved", "close:delivered"}, inner.Moves()[4:])
}

func TestRulesGateNamesEveryOpenViolationInTheRefusal(t *testing.T) {
	g, _ := rgGate(t)
	g.ObserveVerdict(fbBead, rgVerdict(domain.LensQA, 1, domain.VerdictChanges,
		rgItem("qa-r1-1", "widens the public API", domain.ItemOpen),
		rgItem("qa-r1-2", "writes outside the module", domain.ItemOpen)))

	err := g.CheckState(fbBead, domain.BeadStateApproved)
	require.ErrorIs(t, err, ErrRulesBlocker)
	assert.Contains(t, err.Error(), "qa-r1-1 (widens the public API), qa-r1-2 (writes outside the module) still violate a rules file")
	assert.Equal(t, RulesGateStats{Beads: 1, Blocked: 1, Violations: 2}, g.Stats(),
		"a check the caller made itself is counted by whoever acts on it, not here")
}

func TestRulesGateDefaultsAndTheThingsItLeavesAlone(t *testing.T) {
	// No bus, no logger, no clock: the gate still works and publishes
	// nothing.
	g := NewRulesGate(RulesGateOptions{})
	g.ObserveVerdict(fbBead, rgVerdict(domain.LensQA, 1, domain.VerdictChanges,
		rgItem("qa-r1-1", "widens the public API", domain.ItemOpen)))
	_, err := g.GuardStore(&rgStore{}).Close(context.Background(), fbBead, "delivered")
	assert.ErrorIs(t, err, ErrRulesBlocker)
	assert.Equal(t, 1, g.Stats().Refused, "the refusal is counted with no bus to publish it to")

	// A verdict with no bead to attach the items to is not remembered.
	g.ObserveVerdict("", rgVerdict(domain.LensQA, 1, domain.VerdictChanges,
		rgItem("qa-r1-1", "widens the public API", domain.ItemOpen)))
	assert.False(t, g.Blocked(""))

	// An ordinary item in a verdict about a bead that has a violation is
	// still none of the gate's business.
	assert.NoError(t, g.CheckVerdict(fbBead, rgVerdict(domain.LensUX, 2, domain.VerdictChanges,
		fbItemAt("ux-r2-1", "the wording is odd", "retry.go", domain.SeverityNit, domain.ItemAccepted))))

	// And so are the payloads that are not one of the three it judges.
	assert.NoError(t, g.CheckPayload(fbBead, domain.ImplReport{Summary: "did it"}))
	g.Observe(fbBead, domain.ImplReport{Summary: "did it"})
	assert.True(t, g.Blocked(fbBead), "folding an impl report changed nothing")
}

func TestRulesGateForgetsOnlyWhenAsked(t *testing.T) {
	g, _ := rgGate(t)
	g.ObserveVerdict(fbBead, rgVerdict(domain.LensQA, 1, domain.VerdictChanges,
		rgItem("qa-r1-1", "widens the public API", domain.ItemOpen)))
	require.Equal(t, 1, g.Stats().Beads)
	g.Forget(fbBead)
	assert.Equal(t, RulesGateStats{}, g.Stats())
	assert.False(t, g.Blocked(fbBead))
}

func TestRulesGateRestoresItsMemoryFromTheBeadsComments(t *testing.T) {
	g, _ := rgGate(t)
	g.RestoreMessages(fbBead, []beads.Message{
		{Type: domain.MessageImplReport, Payload: domain.ImplReport{Summary: "did it"}},
		{Type: domain.MessageVerdict, Payload: rgVerdict(domain.LensQA, 1, domain.VerdictChanges,
			rgItem("qa-r1-1", "widens the public API", domain.ItemOpen))},
		// A comment that failed to decode is skipped, like the loop skips it.
		{Type: domain.MessageVerdict, Err: errors.New("bad block")},
		// A ruling the loop would apply settles nothing here.
		{Type: domain.MessageArbitration, Payload: domain.Arbitration{
			ItemID: "qa-r1-1", Ruling: domain.RulingUpholdEngineer,
		}},
	})
	require.True(t, g.Blocked(fbBead))
	assert.Equal(t, "qa-r1-1", g.Open(fbBead)[0].Item)
}

// --- the seams ---------------------------------------------------------

func TestRulesGateCheckRefusesInsideTheRetryLoop(t *testing.T) {
	g, sink := rgGate(t)
	g.ObserveVerdict(fbBead, rgVerdict(domain.LensQA, 1, domain.VerdictChanges,
		rgItem("qa-r1-1", "widens the public API", domain.ItemOpen)))

	var ran int

	// The escalation contract's own check would pass; the gate's does not,
	// and it runs first, so the model is told the hard constraint before
	// anything else about the same reply.
	check := g.Check(fbBead, "po-fb-1-r3", domain.MessageEscalation, func(_ structured.Parsed, _ string) error {
		ran++
		return nil
	})
	require.NotNil(t, check)
	err := check(structured.Parsed{Block: structured.Block{JSON: []byte(
		`{"outcome":"approve_with_notes","notes":"we will follow up"}`)}}, "")
	require.ErrorIs(t, err, ErrRulesBlocker)
	assert.Zero(t, ran, "the gate refused before the role's own check ran")
	assert.Equal(t, 1, g.Stats().Refused)

	// The refusal names the agent that made the reply, so the run log and
	// the TUI attribute it exactly like the refusal on the Result path: a
	// warning with an empty agent says which bead, but not who.
	warnings := sink.Warnings()
	require.Len(t, warnings, 1)
	assert.Equal(t, SourceRules, warnings[0].Source)
	assert.Equal(t, fbBead, warnings[0].Bead)
	assert.Equal(t, domain.AgentID("po-fb-1-r3"), warnings[0].Agent)

	// A reply the gate has nothing against falls through to the role's check.
	require.NoError(t, check(structured.Parsed{Block: structured.Block{JSON: []byte(
		`{"outcome":"block","summary":"a human should look"}`)}}, ""))
	assert.Equal(t, 1, ran)

	// A message type the gate does not judge is left exactly as it was.
	sentinel := errors.New("the role's own complaint")
	role := func(_ structured.Parsed, _ string) error { return sentinel }
	wrapped := g.Check(fbBead, "eng-fb-1-r1", domain.MessageImplReport, role)
	assert.ErrorIs(t, wrapped(structured.Parsed{}, ""), sentinel)
	assert.Nil(t, g.Check(fbBead, "po-plan", domain.MessagePlan, nil))
}

func TestRulesGateGuardRefusesTheReplyAsItIsApplied(t *testing.T) {
	g, _ := rgGate(t)
	g.ObserveVerdict(fbBead, rgVerdict(domain.LensQA, 1, domain.VerdictChanges,
		rgItem("qa-r1-1", "widens the public API", domain.ItemOpen)))

	var applied []any
	planner := FiringPlannerFunc(func(_ context.Context, _ Firing) (AgentFiring, error) {
		return AgentFiring{
			Agent: domain.Agent{ID: "po-1", Role: domain.RolePO, Bead: fbBead},
			Result: func(payload any, _ agents.Transcript) error {
				applied = append(applied, payload)
				return nil
			},
		}, nil
	})
	guarded := g.GuardPlanner(planner)
	af, err := guarded.PlanFiring(context.Background(), Firing{Demand: Demand{Bead: fbBead}})
	require.NoError(t, err)

	// The ruling D-08 forbids never reaches the loop.
	err = af.Result(domain.Arbitration{ItemID: "qa-r1-1", Ruling: domain.RulingUpholdEngineer}, agents.Transcript{})
	require.ErrorIs(t, err, ErrRulesBlocker)
	assert.Empty(t, applied)

	// One that respects the rule is folded into the gate and handed on.
	require.NoError(t, af.Result(domain.Arbitration{
		ItemID: "qa-r1-1", Ruling: domain.RulingUpholdTester, Instruction: "file the bead",
	}, agents.Transcript{}))
	require.Len(t, applied, 1)
	assert.True(t, g.Blocked(fbBead), "upholding the tester leaves the violation open")

	// A firing with no bead of its own falls back to the agent's.
	af2 := g.Guard("", AgentFiring{
		Agent:  domain.Agent{ID: "po-2", Bead: fbBead},
		Result: func(any, agents.Transcript) error { return nil },
	})
	assert.ErrorIs(t,
		af2.Result(domain.Escalation{Outcome: domain.EscalationApproveWithNotes, Notes: "n"}, agents.Transcript{}),
		ErrRulesBlocker)

	// The firing's own contract check gains the gate's, so the refusal
	// happens inside structured.Fire's retry loop as well.
	built := g.Guard(fbBead, AgentFiring{
		Agent: domain.Agent{ID: "po-3", Bead: fbBead},
		Build: func(context.Context, FiringContext) (agents.AgentRun, error) {
			return agents.AgentRun{Message: domain.MessageEscalation}, nil
		},
	})
	run, err := built.Build(context.Background(), FiringContext{})
	require.NoError(t, err)
	require.NotNil(t, run.Check)
	assert.ErrorIs(t, run.Check(structured.Parsed{Block: structured.Block{JSON: []byte(
		`{"outcome":"approve_with_notes","notes":"n"}`)}}, ""), ErrRulesBlocker)
	// A block the message type cannot decode is left to the role's own
	// check to complain about.
	assert.NoError(t, run.Check(structured.Parsed{Block: structured.Block{JSON: []byte(`["not an object"]`)}}, ""))

	// A builder that failed is passed through untouched.
	buildBoom := errors.New("the prompt is over budget")
	broken := g.Guard(fbBead, AgentFiring{Build: func(context.Context, FiringContext) (agents.AgentRun, error) {
		return agents.AgentRun{}, buildBoom
	}})
	_, err = broken.Build(context.Background(), FiringContext{})
	assert.ErrorIs(t, err, buildBoom)

	// A planner that could not plan is passed through untouched.
	boom := errors.New("no worktree")
	_, err = g.GuardPlanner(FiringPlannerFunc(func(context.Context, Firing) (AgentFiring, error) {
		return AgentFiring{}, boom
	})).PlanFiring(context.Background(), Firing{})
	assert.ErrorIs(t, err, boom)
}

// --- the loop ----------------------------------------------------------

// rgRig is fbSetup's rig with the gate wired into both seams: the
// supervisor plans through [RulesGate.GuardPlanner] and the state machine
// writes through [RulesGate.GuardStore].
func rgRig(t *testing.T, sc *mock.Scenario, tweak func(*FeedbackOptions)) (*fbRig, *RulesGate, *rgStore) {
	t.Helper()
	gate, sink := rgGate(t)
	store := &rgStore{state: domain.BeadStateOpen}
	rig := fbSetup(t, sc, func(o *FeedbackOptions) {
		if tweak != nil {
			tweak(o)
		}
		o.States = NewStateMachine(gate.GuardStore(store),
			WithStatePublisher(o.Bus), WithStateRun(o.Run), WithStateClock(o.Now))
	})

	sink.forward(rig.Bus)

	cfg := rgConfig()
	now := func() time.Time { return fbEpoch }
	sel, err := cartridge.NewSelector(cfg, map[string]cartridge.Cartridge{fbCartridge: rig.Adapter}, cartridge.WithClock(now))
	require.NoError(t, err)
	sup, err := NewSupervisor(SupervisorOptions{
		Cartridges: sel, Planner: gate.GuardPlanner(rig.Loop), Bus: rig.Bus,
		Timeout: time.Minute, Now: now, NoShims: true,
	})
	require.NoError(t, err)
	sup.Attach(rig.Run)
	t.Cleanup(func() { _ = sup.Close() })
	rig.Sup = sup
	return rig, gate, store
}

// rgStep fires every demand the loop raises right now and reports the
// firings that failed, which [fbRig.step] refuses to tolerate.
func rgStep(t *testing.T, rig *fbRig) []error {
	t.Helper()
	ctx := context.Background()
	ds, err := rig.Loop.Demands(ctx)
	require.NoError(t, err)
	sortDemands(ds)
	var errs []error
	for _, d := range ds {
		agent := d.Agent
		if agent == "" {
			agent = mintAgentID(d, 1)
		}
		if _, ferr := rig.Sup.Fire(ctx, Firing{
			Agent: domain.Agent{
				ID: agent, Role: d.Role(), Lens: d.Lens, Bead: d.Bead,
				State: domain.AgentStateRunning, StartedAt: fbEpoch,
			},
			Demand: d,
			Run:    rig.Run,
		}); ferr != nil {
			errs = append(errs, ferr)
		}
	}
	return errs
}

// rgSteps runs n steps and returns every firing failure they produced.
func rgSteps(t *testing.T, rig *fbRig, n int) []error {
	t.Helper()
	var errs []error
	for i := 0; i < n; i++ {
		errs = append(errs, rgStep(t, rig)...)
	}
	return errs
}

// rgDisputed is the thread a killed run left behind mid-dispute: a tester
// raised a violation of a rules file, the engineer pushed back on it and the
// tester held its ground, so the product owner owes a ruling (D-08).
//
// No live run could have written the pushback — [agents.EngineerResultCheck]
// refuses one on a rules item — but [Feedback.Restore] replays the record it
// finds, so a resumed run can be exactly here. That is what makes it the
// loop's arbitration path worth guarding.
func rgDisputed() []beads.Message {
	item := rgItem("qa-r1-1", "widens the public API without a bead", domain.ItemOpen)
	held := item
	held.Status = domain.ItemHeld
	held.Detail = "the rules file says never, not rarely"
	return []beads.Message{
		{Type: domain.MessageImplReport, IssueID: fbBead, Payload: domain.ImplReport{Summary: "added the retry"}},
		{Type: domain.MessageVerdict, IssueID: fbBead, Payload: rgVerdict(domain.LensUX, 1, domain.VerdictApprove)},
		{Type: domain.MessageVerdict, IssueID: fbBead, Payload: rgVerdict(domain.LensTest, 1, domain.VerdictApprove)},
		{Type: domain.MessageVerdict, IssueID: fbBead, Payload: rgVerdict(domain.LensQA, 1, domain.VerdictChanges, item)},
		{Type: domain.MessageResponse, IssueID: fbBead, Payload: []domain.Response{{
			ItemID: "qa-r1-1", Action: domain.ActionPushback, Rationale: "the API is internal in practice",
		}}},
		{Type: domain.MessageVerdict, IssueID: fbBead, Payload: rgVerdict(domain.LensQA, 2, domain.VerdictChanges, held)},
	}
}

func TestRulesGateLoopRefusesAnArbitrationThatWouldShipAViolation(t *testing.T) {
	sc := fbScenario("rules-arbitration")
	// The product owner tries to wave the rule away. The rule is matched on
	// the task, so the test does not have to guess the agent id the
	// resumed round mints.
	sc.Rule("arbitrate", mock.Match{Task: events.TaskArbitrate.String()}).
		Say("ruling on the dispute").
		Structured(domain.Arbitration{
			ItemID: "qa-r1-1", Ruling: domain.RulingUpholdEngineer, Rationale: "it is internal in practice",
		})

	rig, gate, store := rgRig(t, sc, func(o *FeedbackOptions) { o.Limits.MaxPushbackRounds = 1 })
	msgs := rgDisputed()
	gate.RestoreMessages(fbBead, msgs)
	require.NoError(t, rig.Loop.Restore(context.Background(),
		domain.Bead{ID: fbBead, Title: "make the retry loop terminate", State: domain.BeadStateInReview}, msgs))
	require.Equal(t, FeedbackArbitrating, rig.Thread().Phase, "the resumed thread owes a ruling")

	errs := rgSteps(t, rig, 1)

	require.Len(t, errs, 1, "the arbitration firing is refused")
	assert.ErrorIs(t, errs[0], ErrRulesBlocker)
	assert.Positive(t, gate.Stats().Refused)

	th := rig.Thread()
	require.Len(t, th.Items, 1)
	assert.Nil(t, th.Items[0].Ruling, "the ruling was never applied")
	assert.NotEqual(t, domain.ItemOverruled, th.Items[0].Status)
	assert.NotEqual(t, domain.BeadStateApproved, th.State)
	assert.NotContains(t, store.Moves(), "status:approved")
	assert.True(t, gate.Blocked(fbBead))
	assert.NotContains(t, rig.Messages.Types(), domain.MessageArbitration,
		"a refused ruling is not written to the bead either")
}

func TestRulesGateLoopRefusesApproveWithNotesAtTheRoundCap(t *testing.T) {
	item := rgItem("qa-r1-1", "widens the public API without a bead", domain.ItemOpen)
	sc := fbScenario("rules-escalation")
	fbSay(sc, "eng-sc-fb1-r1", fbReport("added the retry"))
	fbSay(sc, "tester-ux-sc-fb1-r1", fbApprove("reads fine"))
	fbSay(sc, "tester-test-sc-fb1-r1", fbApprove("covered"))
	fbSay(sc, "tester-qa-sc-fb1-r1", fbChanges("the rules file forbids this", item))
	fbSay(sc, "eng-sc-fb1-r2", fbResponses(domain.Response{
		ItemID: "qa-r1-1", Action: domain.ActionFixed, Rationale: "narrowed the export",
	}))
	stillOpen := item
	stillOpen.Detail = "the export is still there"
	fbSay(sc, "tester-qa-sc-fb1-r2", fbChanges("still a violation", stillOpen))
	// The round cap is reached, and the product owner tries to ship it.
	fbSay(sc, "po-sc-fb1-r2-escalate", domain.Escalation{
		Outcome: domain.EscalationApproveWithNotes, Notes: "we will file a follow-up bead",
	})

	rig, gate, store := rgRig(t, sc, func(o *FeedbackOptions) { o.Limits.MaxReviewRounds = 2 })
	rig.setDiff("head2", 22, 0.1)
	errs := rgSteps(t, rig, 5)

	require.Len(t, errs, 1, "the escalation firing is the one that fails")
	assert.ErrorIs(t, errs[0], ErrRulesBlocker)

	th := rig.Thread()
	assert.Nil(t, th.Escalation, "the ruling was never applied")
	assert.Equal(t, domain.BeadStateEscalated, th.State)
	assert.Contains(t, store.Moves(), "status:escalated")
	assert.NotContains(t, store.Moves(), "status:approved")
	assert.True(t, gate.Blocked(fbBead))

	// The refusal is on the event stream for the TUI and the run log.
	var warned bool
	for _, ev := range rig.Events(events.KindWarning) {
		if w, ok := ev.(events.Warning); ok && w.Source == SourceRules {
			warned = true
		}
	}
	assert.True(t, warned, "the gate published its refusal")
}

// rgRestore replays a thread from bd comments the way a resumed run does
// ([Feedback.Restore]) and reports where the bead ended up.
// rgSettled is the thread a killed run left behind with a violation of a
// rules file settled the one way nothing may settle it: the tester marked
// its own item accepted, so the replay finds a bead with nothing
// outstanding and approves it.
//
// No live firing could write this — [agents.ValidateTesterVerdict] refuses
// it, and so does the gate — but [Feedback.Restore] applies the record as
// it finds it, which is why the resume is the path that needs the gate's
// memory rather than a role check.
func rgSettled(bead domain.BeadID) []beads.Message {
	item := rgItem("qa-r1-1", "widens the public API without a bead", domain.ItemOpen)
	settled := item
	settled.Status = domain.ItemAccepted
	return []beads.Message{
		{Type: domain.MessageImplReport, IssueID: bead, Payload: domain.ImplReport{Summary: "added the retry"}},
		{Type: domain.MessageVerdict, IssueID: bead, Payload: rgVerdict(domain.LensUX, 1, domain.VerdictApprove)},
		{Type: domain.MessageVerdict, IssueID: bead, Payload: rgVerdict(domain.LensTest, 1, domain.VerdictApprove)},
		{Type: domain.MessageVerdict, IssueID: bead, Payload: rgVerdict(domain.LensQA, 1, domain.VerdictChanges, item)},
		{Type: domain.MessageResponse, IssueID: bead, Payload: []domain.Response{{
			ItemID: "qa-r1-1", Action: domain.ActionFixed, Rationale: "narrowed the export",
		}}},
		{Type: domain.MessageVerdict, IssueID: bead, Payload: rgVerdict(domain.LensQA, 2, domain.VerdictApprove, settled)},
	}
}

func rgRestore(t *testing.T, gate *RulesGate) (*rgStore, error) {
	t.Helper()
	msgs := rgSettled(fbBead)

	store := &rgStore{state: domain.BeadStateInReview}
	var inner StateStore = store
	if gate != nil {
		inner = gate.GuardStore(store)
		gate.RestoreMessages(fbBead, msgs)
	}
	loop, err := NewFeedback(FeedbackOptions{
		Context: rgBeadContext,
		States:  NewStateMachine(inner, WithStateRun(rgRun), WithStateClock(func() time.Time { return fbEpoch })),
		Run:     rgRun,
		Now:     func() time.Time { return fbEpoch },
	})
	require.NoError(t, err)
	return store, loop.Restore(context.Background(),
		domain.Bead{ID: fbBead, Title: "make the retry loop terminate", State: domain.BeadStateInReview}, msgs)
}

func TestRulesGateStopsAResumeFromShippingASettledViolation(t *testing.T) {
	// Without the gate the replay believes the comments: the tester
	// accepted the item, so nothing is unsettled and the bead is approved.
	// This is the hole the gate exists to close — Feedback.Restore applies
	// the thread's record without re-running any role check.
	store, err := rgRestore(t, nil)
	require.NoError(t, err)
	require.Contains(t, store.Moves(), "status:approved")

	// With the gate the same replay cannot ship it.
	gate, _ := rgGate(t)
	store, err = rgRestore(t, gate)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRulesBlocker)
	assert.NotContains(t, store.Moves(), "status:approved")
	assert.Equal(t, domain.BeadStateInReview, store.State(), "the bead stayed where the killed run left it")
	assert.True(t, gate.Blocked(fbBead))
	assert.Equal(t, 1, gate.Stats().Refused)
}

// --- the resume, as production drives it -------------------------------

// rgResumeBead is the bead the conductor-driven resume tests replay.
const rgResumeBead = domain.BeadID("sc-1")

// rgResume runs one pass of the production glue over a killed run whose
// bead carries the thread of [rgSettled], and returns the gate, the store
// the state machine wrote through, the warnings the run published and what
// the pass came back with.
//
// wire says whether the conductor was given the gate
// ([ConductorOptions.RulesGate]), which is the whole difference between the
// two tests below: everything else is what `sc run --resume` assembles.
func rgResume(t *testing.T, wire bool) (*RulesGate, *rgStore, *rgSink, error) {
	t.Helper()
	r := rgResumeSetup(t, wire)
	return r.Gate, r.Store, r.Sink, r.sync(t)
}

// rgResumeRig is what [rgResumeSetup] built, for a test that needs more
// than one pass.
type rgResumeRig struct {
	Gate  *RulesGate
	Store *rgStore
	Sink  *rgSink
	Rig   *glueRig
	Loop  *Feedback
}

// sync runs one conductor pass, exactly as the run's own ticker does.
func (r *rgResumeRig) sync(t *testing.T) error {
	t.Helper()
	return r.Rig.Cond.Sync(context.Background())
}

// rgResumeSetup assembles the production glue over a killed run whose bead
// carries the thread of [rgSettled]. Everything but `wire` is what
// `sc run --resume` builds: the conductor is given the state machine it
// parks a refused bead with ([ConductorOptions.States]) whether or not the
// gate is wired, because that is what production does.
func rgResumeSetup(t *testing.T, wire bool) *rgResumeRig {
	t.Helper()
	bd := newGlueBD(
		glueIssue("sc-epic", domain.BeadTypeEpic, "open", "make the retry loop terminate"),
		glueIssue(rgResumeBead, domain.BeadTypeTask, "in_review", "make the retry loop terminate"),
	)
	bd.msgs[rgResumeBead] = rgSettled(rgResumeBead)

	gate, sink := rgGate(t)
	store := &rgStore{state: domain.BeadStateInReview}
	// The machine has to exist before the conductor does, so it is stamped
	// with the gate's own run id and clock rather than the rig's; nothing
	// here reads either.
	states := NewStateMachine(gate.GuardStore(store),
		WithStatePublisher(sink), WithStateRun(rgRun), WithStateClock(func() time.Time { return fbEpoch }))
	opts := ConductorOptions{Restore: glueYes(), States: states}
	if wire {
		opts.RulesGate = gate
	}
	rig := glueSetup(t, bd, opts, config.RunSettings{})
	loop, err := NewFeedback(FeedbackOptions{
		Context: rig.Cond.BeadContext,
		States:  states,
		Run:     rig.Config.Run.ID,
		Now:     rig.Fixture.Clock.Now,
	})
	require.NoError(t, err)
	rig.Cond.Attach(ConductorParts{Feedback: loop})

	require.NoError(t, rig.Run.Start(context.Background()))
	require.NoError(t, rig.Run.SetEpic("sc-epic"))
	t.Cleanup(func() {
		_ = rig.Run.Stop("test over")
		_, _ = rig.Run.Wait()
	})
	return &rgResumeRig{Gate: gate, Store: store, Sink: sink, Rig: rig, Loop: loop}
}

// TestRulesGateUnwiredAResumeShipsTheViolation is the regression this
// wiring exists for, and it fails the moment [ConductorOptions.RulesGate]
// stops being read: a conductor that does not tell the gate leaves its
// memory empty, so nothing is open, nothing is refused, and the replay
// walks the bead to approved on the strength of a comment no live firing
// could have written.
func TestRulesGateUnwiredAResumeShipsTheViolation(t *testing.T) {
	gate, store, sink, err := rgResume(t, false)
	require.NoError(t, err, "the replay believes the comments")
	assert.Contains(t, store.Moves(), "status:approved")
	assert.False(t, gate.Blocked(rgResumeBead), "the gate was never told")
	assert.Zero(t, gate.Stats().Refused)
	assert.Empty(t, sink.Warnings())
}

// TestRulesGateWiredAResumeCannotShipTheViolation drives the production
// resume path — [Conductor.restoreThreads], the same call `sc run --resume`
// makes — and not the loop directly: the conductor tells the gate about the
// bead's comments before [Feedback.Restore] replays them, so the violation
// is open again by the time the replay tries to approve the bead, and the
// guarded store refuses it.
func TestRulesGateWiredAResumeCannotShipTheViolation(t *testing.T) {
	gate, store, sink, err := rgResume(t, true)
	require.Error(t, err, "the pass reports what it could not restore")
	assert.ErrorIs(t, err, ErrRulesBlocker)

	assert.NotContains(t, store.Moves(), "status:approved")
	assert.Equal(t, domain.BeadStateNeedsHuman, store.State(),
		"and a person owns it: needs_human, not the in_review the killed run left")
	assert.True(t, gate.Blocked(rgResumeBead))
	open := gate.Open(rgResumeBead)
	require.Len(t, open, 1)
	assert.Equal(t, "qa-r1-1", open[0].Item)
	assert.Equal(t, 1, gate.Stats().Refused)

	// The refusal is on the event stream, so a person watching the resume
	// sees why the bead did not move.
	warnings := sink.Warnings()
	require.Len(t, warnings, 1)
	assert.Equal(t, SourceRules, warnings[0].Source)
	assert.Equal(t, rgResumeBead, warnings[0].Bead)
	assert.ErrorContains(t, err, "qa-r1-1")
}

// TestRulesGateARefusedResumeEndsWithAPersonOwningTheBead is the other half
// of the refusal, and the gap round 4 found: refusing the replay is not
// enough on its own.
//
// [Feedback.Restore] registers the thread before it tries the transition,
// so the failed first pass leaves a live thread the loop believes is
// settled. Every later pass then took the "a live thread is the true one"
// branch, dropped the bead from what the resume owed and said nothing —
// leaving bd holding it in in_review for the rest of the run, no agent ever
// offered, nothing owed and nobody told. It was not a livelock (one
// refusal, one warning, ten passes), which is precisely why nothing caught
// it.
//
// So the refusal now parks the bead: needs_human, through the same guarded
// store, with the loop told where it went.
func TestRulesGateARefusedResumeEndsWithAPersonOwningTheBead(t *testing.T) {
	r := rgResumeSetup(t, true)

	err := r.sync(t)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrRulesBlocker)
	require.Equal(t, []string{"status:needs_human"}, r.Store.Moves(),
		"nothing shipped, and the bead did not stay in in_review either")
	require.Equal(t, domain.BeadStateNeedsHuman, r.Store.State())

	// The run said so, naming the bead: the person who has to act finds it
	// on the event stream and in `sc status`, not only in a returned error.
	var parked int
	for _, ev := range r.Rig.Fixture.drain() {
		if w, ok := ev.(events.Warning); ok && w.Source == SourceRules {
			parked++
			assert.Contains(t, w.Message, "needs_human")
			assert.Equal(t, rgResumeBead, w.Bead, "the warning names the bead a person now owns")
			assert.Contains(t, w.Err, "qa-r1-1", "and what is open on it")
		}
	}
	assert.Equal(t, 1, parked, "one warning, once")

	// And it settles there. Nine more passes change nothing: no retry of a
	// refusal that can never succeed, no second warning, no unbounded
	// growth, and the loop is not waiting on the bead any more.
	for i := 0; i < 9; i++ {
		require.NoErrorf(t, r.sync(t), "pass %d", i+2)
	}
	assert.Equal(t, 1, r.Gate.Stats().Refused, "the refusal happened once")
	assert.Equal(t, []string{"status:needs_human"}, r.Store.Moves())
	assert.Len(t, r.Sink.Warnings(), 1)
	assert.Zero(t, r.Loop.Outstanding(), "the loop knows a person owns the bead")

	demands, err := r.Loop.Demands(context.Background())
	require.NoError(t, err)
	assert.Empty(t, demands, "and offers nobody work on it")

	// The gate still remembers, so anything that tries to ship the bead
	// afterwards is refused the same way.
	assert.True(t, r.Gate.Blocked(rgResumeBead))
}

// --- what a person sees ------------------------------------------------

// TestRulesGateStatsReachTheRunVitals pins the claim [RulesGateStats] makes
// about itself: the counters ride in the run's vitals, so they reach
// state.json and the `sc status` line, and a run with nothing to say prints
// nothing and writes nothing.
func TestRulesGateStatsReachTheRunVitals(t *testing.T) {
	clean := RunVitals{}.WithRules(nil)
	assert.True(t, clean.Rules.Zero())
	assert.Empty(t, clean.RulesLine(), "a run with no rules item says nothing")
	assert.Len(t, clean.Lines(), 2, "planning and gates, and no rules line")
	raw, err := json.Marshal(clean)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "rules", "and writes nothing into state.json either")

	g, _ := rgGate(t)
	g.ObserveVerdict(fbBead, rgVerdict(domain.LensQA, 1, domain.VerdictChanges,
		rgItem("qa-r1-1", "widens the public API", domain.ItemOpen)))
	_, err = g.GuardStore(&rgStore{}).SetStatus(context.Background(), fbBead, domain.BeadStateApproved)
	require.ErrorIs(t, err, ErrRulesBlocker)

	v := RunVitals{}.WithRules(g)
	assert.Equal(t, RulesGateStats{Beads: 1, Blocked: 1, Violations: 1, Refused: 1}, v.Rules)
	line := "rules: 1 bead with a rules item · 1 bead blocked by 1 open violation · 1 refusal"
	assert.Equal(t, line, v.RulesLine())
	assert.Contains(t, v.Lines(), line)

	// It survives the round trip through state.json, which is what
	// `sc status` reads a finished run out of.
	raw, err = json.Marshal(v)
	require.NoError(t, err)
	var back RunVitals
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, v.Rules, back.Rules)
	assert.Equal(t, line, back.RulesLine())

	// Once the tester says the diff fixed it, the line reports the history
	// without claiming anything is still held.
	g.ObserveVerdict(fbBead, rgVerdict(domain.LensQA, 2, domain.VerdictApprove,
		rgItem("qa-r1-1", "widens the public API", domain.ItemResolved)))
	assert.Equal(t, "rules: 1 bead with a rules item · none blocked · 1 refusal",
		RunVitals{}.WithRules(g).RulesLine())
}

// TestRulesGateDecodesOnlyWhatItChecks pins the two halves of the decoder
// against each other: [rulesChecks] admits exactly the message types
// [rulesDecode] handles, and a type it does not handle decodes to nothing
// rather than to a message of its own.
//
// P9-06 review, round 4: the old default branch built
// "the rules gate does not decode %q", which no path could reach and which
// P9-08's grep cannot see either — it scans internal/cli and internal/config
// for user-facing text and sentinel declarations elsewhere, so an inline
// orchestrator message was neither documented in docs/troubleshooting.md nor
// listed as an exemption.
func TestRulesGateDecodesOnlyWhatItChecks(t *testing.T) {
	for _, msg := range domain.MessageTypes() {
		payload, err := rulesDecode(msg, json.RawMessage(`{}`))
		if !rulesChecks(msg) {
			assert.NoErrorf(t, err, "%s decodes to nothing, not to a complaint", msg)
			assert.Nilf(t, payload, "%s decodes to nothing", msg)
			// And an undecoded payload is what CheckPayload passes.
			assert.NoError(t, (&RulesGate{}).CheckPayload(fbBead, payload))
			continue
		}
		require.NoErrorf(t, err, "%s is one of the three the gate judges", msg)
		require.NotNilf(t, payload, "%s decodes to a payload", msg)
	}
	// The three it does judge are exactly these.
	assert.True(t, rulesChecks(domain.MessageVerdict))
	assert.True(t, rulesChecks(domain.MessageArbitration))
	assert.True(t, rulesChecks(domain.MessageEscalation))
}

// --- fixtures ----------------------------------------------------------

// rgConfig is the mock cartridge's configuration, as fbSetup builds it.
func rgConfig() config.Config {
	cfg := config.Defaults()
	cfg.ActiveCartridge = fbCartridge
	cfg.Cartridges = map[string]config.Cartridge{fbCartridge: {}}
	return cfg
}

// rgBeadContext is the smallest FeedbackBeadContext a replayed thread needs:
// no firing is planned, so nothing reads the diff or the worktree.
func rgBeadContext(_ context.Context, bead domain.BeadID) (FeedbackBeadContext, error) {
	return FeedbackBeadContext{
		Bead: prompt.BeadContext{
			Bead: domain.Bead{
				ID: bead, Title: "make the retry loop terminate", Type: domain.BeadTypeTask,
				Description: "the retry loop spins forever", Acceptance: "the loop terminates",
				Priority: domain.PriorityDefault,
			},
		},
		Repo:  prompt.RepoContext{Root: "/repo", DefaultBranch: "main"},
		Brief: "make the retry loop terminate",
	}, nil
}

// rgStore is the StateStore the gate's tests write through: it records the
// call rather than talking to bd.
type rgStore struct {
	mu    sync.Mutex
	state domain.BeadState
	moves []string
}

func (s *rgStore) record(what string, to domain.BeadState) (beads.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.moves = append(s.moves, what)
	s.state = to
	return beads.Issue{ID: fbBead, Status: to.String()}, nil
}

func (s *rgStore) SetStatus(_ context.Context, _ domain.BeadID, st domain.BeadState) (beads.Issue, error) {
	return s.record("status:"+st.String(), st)
}

func (s *rgStore) ClaimFor(_ context.Context, _ domain.BeadID, a domain.AgentID) (beads.Issue, error) {
	return s.record("claim:"+a.String(), domain.BeadStateInProgress)
}

func (s *rgStore) Reclaim(_ context.Context, _ domain.BeadID, a domain.AgentID) (beads.Issue, error) {
	return s.record("reclaim:"+a.String(), domain.BeadStateInProgress)
}

func (s *rgStore) Release(_ context.Context, _ domain.BeadID) (beads.Issue, error) {
	return s.record("release", domain.BeadStateOpen)
}

func (s *rgStore) Close(_ context.Context, _ domain.BeadID, reason string) (beads.Issue, error) {
	return s.record("close:"+reason, domain.BeadStateClosed)
}

// Moves lists the writes that reached the store, in order.
func (s *rgStore) Moves() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.moves...)
}

// State is where the store thinks the bead is.
func (s *rgStore) State() domain.BeadState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}
