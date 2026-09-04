package cli

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/orchestrator"
)

// The rules compliance gate, as `sc run` assembles it (P9-06).
//
// internal/orchestrator/rules_gate_test.go proves the gate behaves — what it
// remembers, what it refuses, and that the production Conductor's resume
// path is refused when the gate is wired and ships the violation when it is
// not. What none of that could see is whether `sc run` still wires it:
// every one of those tests builds its own GuardStore, GuardPlanner and
// ConductorOptions. Deleting all three wire points from runAssemble left
// gofmt, the linter and the whole test suite green, because `gate` stayed
// referenced by the vitals (P9-06 review, round 4).
//
// So each wire point has a test here that fails without it:
//
//   - GuardStore: the assembled state machine refuses a shipping
//     transition on a bead the assembled gate holds a violation on;
//   - GuardPlanner: a whole scripted run whose tester raises a rules item
//     leaves the gate's counters in state.json, which only the guarded
//     firing planner can put there;
//   - ConductorOptions.RulesGate and .States: the assembled conductor holds
//     the same gate and the same machine, which is what a resume needs.

// runRulesBead is the bead the assembly tests plant a violation on. It does
// not have to exist in bd: the point is that the gate refuses before the
// store is ever called.
const runRulesBead = domain.BeadID("sc-rules-1")

// runRulesViolation is a tester's verdict raising one violation of a rules
// file, as agents.ValidateTesterVerdict requires one: category `rules`,
// severity blocker, open.
func runRulesViolation() domain.Verdict {
	return domain.Verdict{
		Lens: domain.LensQA, Round: 1, Outcome: domain.VerdictChanges,
		Summary: "the change widens a public API without a bead",
		Items: []domain.FeedbackItem{{
			ID: "qa-r1-1", Severity: domain.SeverityBlocker, Category: domain.CategoryRules,
			Title:  "the change widens a public API without a bead",
			Detail: "CLAUDE.md forbids a new exported symbol without a bead for it",
			File:   runFireWork, Status: domain.ItemOpen,
		}},
	}
}

// TestRunAssemblyGuardsTheStateStoreWithTheRulesGate pins
// `orchestrator.NewStateMachine(gate.GuardStore(client), …)`: the machine
// every part of the run writes bead statuses through must have the gate
// underneath it, or a resume, a delivery hand-off or the gate poller can
// walk a bead with an open rules violation into approved.
func TestRunAssemblyGuardsTheStateStoreWithTheRulesGate(t *testing.T) {
	env := runFireSetup(t, true)
	rc, set := runAssembleSetup(t, env)

	c, err := assembleRun(context.Background(), rc, set)
	require.NoError(t, err)
	defer c.close()
	require.NotNil(t, c.gate, "the assembly builds a rules gate")
	require.NotNil(t, c.states, "and a state machine over it")

	c.gate.ObserveVerdict(runRulesBead, runRulesViolation())
	require.True(t, c.gate.Blocked(runRulesBead))

	// Approving is shipping, so the machine must not reach bd at all.
	_, err = c.states.Apply(context.Background(), orchestrator.StateRequest{
		Bead: runRulesBead, From: domain.BeadStateInReview, Event: orchestrator.BeadEventApproved,
	})
	require.ErrorIs(t, err, orchestrator.ErrRulesBlocker,
		"the state machine the run writes through is not guarded by the rules gate")
	assert.Equal(t, 1, c.gate.Stats().Refused)

	// And the moves that are not shipping still go through: a blocked bead
	// has to be able to reach a person.
	_, err = c.states.Apply(context.Background(), orchestrator.StateRequest{
		Bead: runRulesBead, From: domain.BeadStateInReview, Event: orchestrator.BeadEventBlocked,
		Reason: "a rules-file violation is open",
	})
	assert.NotErrorIs(t, err, orchestrator.ErrRulesBlocker,
		"needs_human ships nothing, so the gate must let it through")
}

// TestRunAssemblyGivesTheConductorTheRulesGate pins the resume half:
// `ConductorOptions{RulesGate: gate, States: states}`. Without the first the
// gate's memory is empty on `sc run --resume` and a violation an old bd
// comment settled the wrong way ships; without the second a bead whose
// replay the gate refuses is left in in_review with nobody told.
func TestRunAssemblyGivesTheConductorTheRulesGate(t *testing.T) {
	env := runFireSetup(t, true)
	rc, set := runAssembleSetup(t, env)

	c, err := assembleRun(context.Background(), rc, set)
	require.NoError(t, err)
	defer c.close()

	require.NotNil(t, c.cond, "the assembly builds a conductor")
	w := c.cond.RulesWiring()
	require.NotNil(t, w.Gate, "the conductor must be told about the rules gate")
	assert.Same(t, c.gate, w.Gate, "and it must be the run's own gate, not a second one")
	require.NotNil(t, w.States, "the conductor must be able to park a bead the gate refused")
	assert.Same(t, c.states, w.States, "with the run's own guarded state machine")
}

// runRulesRunScenario is the scripted run of runFireScenario with one
// review round added: the qa lens finds a violation of a rules file, the
// engineer fixes it, and the qa lens marks the item resolved, which is the
// only thing that settles one. The run then ships as usual.
//
// Every message here is one agents.ValidateTesterVerdict and the rest of
// the role checks accept, on purpose: what is being proved is not that a
// bad message is refused (the orchestrator tests do that) but that the run
// still routes its firings through the gate at all.
func runRulesRunScenario() *mock.Scenario {
	v := runRulesViolation()
	resolved := v.Items[0]
	resolved.Status = domain.ItemResolved

	sc := runFireScenario()
	// Rules are tried in order, so the three specific ones are put in
	// front of runFireScenario's catch-all engineer and tester.
	base := sc.Rules
	sc.Rules = nil
	sc.Rule("engineer-fixes-it", mock.Match{
		Role: string(domain.RoleEngineer), Task: string(events.TaskAddressFeedback),
	}).
		Say("The qa lens is right; the export goes.").
		AppendFile(runFireWork, "\nThe note names its bead.\n").
		Structured(agents.ResponseSet{Responses: []domain.Response{{
			ItemID: "qa-r1-1", Action: domain.ActionFixed,
			Rationale: "the exported symbol is gone and the note names its bead",
		}}})
	sc.Rule("tester-qa-round-1", mock.Match{
		Role: string(domain.RoleTester), Lens: string(domain.LensQA), Round: 1,
	}).
		Say("Reading the diff against the rules files.").
		Structured(v)
	sc.Rule("tester-qa-round-2", mock.Match{
		Role: string(domain.RoleTester), Lens: string(domain.LensQA), Round: 2,
	}).
		Say("The rules file is satisfied now.").
		Structured(domain.Verdict{
			Lens: domain.LensQA, Round: 2, Outcome: domain.VerdictApprove,
			Summary: "the rules-file violation is fixed",
			Items:   []domain.FeedbackItem{resolved},
		})
	sc.Rules = append(sc.Rules, base...)
	return sc
}

// TestRunFiresEveryReplyThroughTheRulesGate pins
// `planFire = gate.GuardPlanner(…)`, the wrapper the supervisor's firing
// planner is given. It is the only thing in a fresh run that ever tells the
// gate anything: the gate's memory is built from the replies the guarded
// planner folds in, so if `sc run` stops wrapping the planner the gate goes
// through a whole run that raised a violation of a rules file having seen
// nothing at all.
//
// The counters are the observable end of that: they ride in the run's
// vitals into state.json (P6-29), which is where `sc status` reads them.
func TestRunFiresEveryReplyThroughTheRulesGate(t *testing.T) {
	env := runFireSetup(t, false)
	require.NoError(t, runRulesRunScenario().WriteFile(env.Scenario))

	stdout, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief,
		"--headless", "--cartridge", "mock")
	require.Equal(t, 0, code, "stdout:\n%s\nstderr:\n%s", stdout, stderr)
	require.Contains(t, stdout, "says changes", "the qa lens raised the violation")

	dir := config.RunDir(env.Repo.Dir, string(runLatestID(t, env.Repo.Dir)))
	st, err := orchestrator.LoadState(dir)
	require.NoError(t, err)
	require.NotNil(t, st.Vitals, "the run records its vitals")
	assert.Equal(t, 1, st.Vitals.Rules.Beads,
		"the rules gate never saw the run's verdicts: is the firing planner still guarded?")
	assert.Zero(t, st.Vitals.Rules.Blocked, "the tester that raised it marked it resolved")
	assert.Zero(t, st.Vitals.Rules.Refused, "so nothing had to be refused")
	assert.Equal(t, "rules: 1 bead with a rules item · none blocked", st.Vitals.RulesLine())
}

// TestRunRulesGateIsOnTheRunsClock pins the seam gap 4 of the round-4
// review named: `orchestrator.NewRulesGate` used to be built with no Now at
// all, so its warnings fell back to time.Now while everything else in the
// run — the state machine, Vitals(run.Now(), …), every event's Meta — was
// stamped through the run's own clock, and the gate's documented wiring
// example (`Now: run.Now`) disagreed with the code.
//
// The gate is built before the run exists, so it reads the clock through
// the same late binding its bus does; this is that binding.
func TestRunRulesGateIsOnTheRunsClock(t *testing.T) {
	frozen := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	saved := runNow
	t.Cleanup(func() { runNow = saved })
	runNow = func() time.Time { return frozen }

	sink := &lateRun{bus: events.NewBus()}
	t.Cleanup(sink.bus.Close)
	assert.Equal(t, frozen, sink.Now(), "before the run exists the package's own clock stands")

	env := runFireSetup(t, true)
	rc, set := runAssembleSetup(t, env)
	c, err := assembleRun(context.Background(), rc, set)
	require.NoError(t, err)
	defer c.close()

	sink.attach(c.run)
	// The run's clock is the real one, so the two calls differ in the
	// nanoseconds; what matters is that the frozen package clock is no
	// longer what answers.
	assert.NotEqual(t, frozen, sink.Now(), "once the run exists its clock is the one that answers")
	assert.WithinDuration(t, c.run.Now(), sink.Now(), time.Second,
		"and it is the run's, which is what the gate stamps its warnings with")
}
