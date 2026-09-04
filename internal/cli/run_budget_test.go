package cli

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/orchestrator"
)

// The run budget, fired for real (P9-01).
//
// internal/orchestrator's limits_test.go proves [orchestrator.RunBudget]
// behaves; nothing proved that `sc run` puts it around the parts it builds,
// so unwiring any of the four wrappers in runAssemble left the whole suite
// green. These two tests drive the binary over the fixture repository with
// the mock cartridge, whose per-rule token usage is pinned so a
// `limits.max_tokens` in the config file is crossed at a known firing:
//
//   - the first crosses it on the *planning* session, which only
//     MeterPlanRunner meters (P9-17), and then needs Work to hold the
//     engineer back and Stop to finish the brief run;
//   - the second lets the plan through and crosses it on the engineer,
//     which only MeterPlanner meters, and then needs the round's reviews to
//     be given their agents anyway while the engineer the round asks for
//     next is held back.
//
// Drop budget.Work, budget.Stop, budget.MeterPlanner or budget.MeterPlanRunner
// from internal/cli/run.go and one of these fails.

// runBudgetSpend is the token spend pinned on the scripted scenario's
// rules, by rule name.
type runBudgetSpend map[string]int

// runBudgetSetup is [runFireSetup] with every firing's reported usage
// pinned and a token budget in the global config file. Safety is off, so
// the run merges its own work and no `gh` pull request is involved.
func runBudgetSetup(t *testing.T, tokens int, sc *mock.Scenario, spend runBudgetSpend) *runFireEnv {
	t.Helper()
	env := runFireSetup(t, false)

	for _, r := range sc.Rules {
		n, ok := spend[r.Name]
		require.Truef(t, ok, "the scenario's %q rule has no pinned spend", r.Name)
		r.Usage(cartridge.Usage{InputTokens: n})
	}
	require.NoError(t, sc.WriteFile(env.Scenario))

	cfg, err := os.ReadFile(config.GlobalPath())
	require.NoError(t, err)
	configWriteFile(t, config.GlobalPath(),
		string(cfg)+fmt.Sprintf("\n[limits]\nmax_tokens = %d\n", tokens))
	return env
}

// runBudgetSummary folds the run log of the last run of repo.
func runBudgetSummary(t *testing.T, repo string) orchestrator.RunSummary {
	t.Helper()
	dir := config.RunDir(repo, string(runLatestID(t, repo)))
	sum, err := orchestrator.SummariseLog(dir, -1)
	require.NoError(t, err)
	return sum
}

// TestRunStopsABriefRunThatCrossesItsTokenBudget crosses the cap on the
// planning product owner: the plan is still applied, no engineer is ever
// offered a bead, and the brief run finishes as a stop with the budget in
// its summary.
func TestRunStopsABriefRunThatCrossesItsTokenBudget(t *testing.T) {
	env := runBudgetSetup(t, 500, runFireScenario(),
		runBudgetSpend{"plan": 1000, "engineer": 10, "tester": 10})

	stdout, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief,
		"--headless", "--cartridge", "mock")
	require.Equal(t, 0, code, "stdout:\n%s\nstderr:\n%s", stdout, stderr)

	require.Contains(t, stdout, "fired po(mock) [plan", "the planning session still happens")
	require.NotContains(t, stdout, "fired engineer",
		"a crossed budget starts no new work: no bead may be implemented")
	require.Contains(t, stdout, "run finished: stopped")
	require.Contains(t, stdout, "limits.max_tokens",
		"the banner is published as a warning the moment the budget fires")

	sum := runBudgetSummary(t, env.Repo.Dir)
	require.Equal(t, events.FinishStopped, sum.Finish)
	require.Contains(t, sum.FinishSummary, "1000 tokens of its 500 budget (limits.max_tokens)")
	require.Contains(t, sum.FinishSummary, "over 1 agent session",
		"the planning session is the only one that was metered, and the only one that ran")
	require.Equal(t, 1, sum.Fired, "no agent fired after the budget was crossed")
	require.Zero(t, sum.Merges, "nothing was implemented, so nothing landed")
}

// runBudgetChangesScenario is the scripted run with the qa lens asking for
// changes, so the round ends owing the engineer another firing: work the
// budget must hold back.
func runBudgetChangesScenario() *mock.Scenario {
	sc := runFireScenario()
	held := mock.NewScenario("held").
		Rule("tester-qa", mock.Match{Role: string(domain.RoleTester), Lens: string(domain.LensQA)}).
		Say("The note needs a title.").
		Structured(domain.Verdict{
			Outcome: domain.VerdictChanges,
			Summary: "the note has no title",
			Items: []domain.FeedbackItem{{
				ID: "qa-1", Severity: domain.SeverityMajor, Status: domain.ItemOpen,
				Title: "the note needs a title", Detail: "add a heading to " + runFireWork,
				File: runFireWork,
			}},
		})
	// Rules are tried in order, so the lens's own rule must come before the
	// scenario's catch-all tester rule.
	sc.Rules = append([]*mock.Rule{held}, sc.Rules...)
	return sc
}

// TestRunChargesEveryAgentSessionAgainstTheRunBudget crosses the cap on the
// engineer instead: the plan is under the budget, the engineer's session
// takes it over, the reviews of the round already in flight are still given
// their agents — and the engineer that round then asks for is not, because
// answering feedback is new work.
func TestRunChargesEveryAgentSessionAgainstTheRunBudget(t *testing.T) {
	env := runBudgetSetup(t, 500, runBudgetChangesScenario(),
		runBudgetSpend{"plan": 100, "engineer": 1000, "tester": 10, "tester-qa": 10})

	stdout, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief,
		"--headless", "--cartridge", "mock")
	require.Equal(t, 0, code, "stdout:\n%s\nstderr:\n%s", stdout, stderr)

	require.Contains(t, stdout, "fired engineer(mock)", "the plan was under the budget")
	require.Contains(t, stdout, "says approve",
		"the reviews of the round in flight are still given their agents")
	require.Contains(t, stdout, "says changes", "the qa lens asked for a fix")
	require.Equal(t, 1, strings.Count(stdout, "fired engineer(mock)"),
		"a crossed budget starts no new work: the engineer that owes the round an answer is held back")
	require.Contains(t, stdout, "run finished: stopped")

	sum := runBudgetSummary(t, env.Repo.Dir)
	require.Equal(t, events.FinishStopped, sum.Finish)
	// The banner is written at the crossing: the planning session's 100 plus
	// the engineer's 1000. Without MeterPlanner the engineer's session is
	// never charged, the budget is never crossed, and the run carries on to
	// deliver the bead. (How many tester sessions follow is the loop's
	// business and is not pinned here: a lens whose demand is re-offered
	// while the round closes can be fired more than once.)
	require.Contains(t, sum.FinishSummary, "1100 tokens of its 500 budget (limits.max_tokens)")
	require.Zero(t, sum.Merges, "the bead still owes a fix, so nothing landed")
}

// TestRunRefusesAConfigThatAsksForNoPushbackRounds is the other half of
// P9-01's config surface: `limits.max_pushback_rounds = 0` used to be
// accepted, stored and shown, and then read as "unset" by the feedback
// loop, which is the default two exchanges.
func TestRunRefusesAConfigThatAsksForNoPushbackRounds(t *testing.T) {
	_, repo := runTestEnv(t)
	runAddCartridge(t, "claude", "fable")
	configWriteFile(t, config.GlobalPath(),
		"active_cartridge = \"claude\"\n[cartridges.claude]\nmodel = \"fable\"\n[limits]\nmax_pushback_rounds = 0\n")

	_, stderr, code := runResolve(t, repo)
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "limits.max_pushback_rounds")
	require.Contains(t, stderr, "must be at least 1")
}
