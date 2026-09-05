package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/orchestrator"
	"slop-cannon/internal/tui"
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
	require.Contains(t, stdout, "run finished: budget_reached",
		"a run its budget ended says so; a user stop is a different reason (P9-12)")
	require.Contains(t, stdout, "limits.max_tokens",
		"the banner is published as a warning the moment the budget fires")

	sum := runBudgetSummary(t, env.Repo.Dir)
	require.Equal(t, events.FinishBudgetReached, sum.Finish)
	require.Contains(t, sum.FinishSummary, "1000 tokens of its 500 budget (limits.max_tokens)")
	require.Contains(t, sum.FinishSummary, "over 1 agent session",
		"the planning session is the only one that was metered, and the only one that ran")
	require.Equal(t, 1, sum.Fired, "no agent fired after the budget was crossed")
	require.Zero(t, sum.Merges, "nothing was implemented, so nothing landed")

	// P9-12's Done-when: a machine consumer is told the budget ended the
	// run. The reason used to be `stopped`, which is what pressing `s`
	// leaves behind, so the only place the difference survived was the
	// prose of the summary.
	statusOut, statusErr, statusCode := runExec(t, "--json", "--repo", env.Repo.Dir, "status")
	require.Equal(t, 0, statusCode, statusErr)
	var rep StatusReport
	require.NoError(t, json.Unmarshal([]byte(statusOut), &rep), statusOut)
	require.NotNil(t, rep.State)
	require.Equal(t, events.FinishBudgetReached, rep.State.Finish)
	require.Contains(t, statusOut, `"finish_reason": "budget_reached"`)
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
	// One session per lens, and no more. A verdict that has just emptied the
	// round's reviewer list used to look, for as long as the bd comment took
	// to write, like a round nobody owed a verdict — so a dispatch landing
	// in that window re-derived the reviewers and fired the lens again. With
	// the budget holding back the engineer firing that closes the round,
	// that cycle had nothing to break it: runs were seen firing 181 tester
	// sessions on one bead and never finishing (P9-01, P9-21).
	require.Equal(t, len(domain.Lenses()), strings.Count(stdout, "fired tester/"),
		"each lens reviews the round once")
	require.Contains(t, stdout, "run finished: budget_reached")

	sum := runBudgetSummary(t, env.Repo.Dir)
	require.Equal(t, events.FinishBudgetReached, sum.Finish)
	// The banner is written at the crossing: the planning session's 100 plus
	// the engineer's 1000. Without MeterPlanner the engineer's session is
	// never charged, the budget is never crossed, and the run carries on to
	// deliver the bead. The three reviews the round already owed are fired
	// after it and add 30 more, which is why the total in the summary is
	// larger than the cap: it is a floor on when new work stops, not a
	// ceiling on the spend (docs/config.md).
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

// TestRunResumedCarriesTheBudgetItAlreadySpent is the budget's other real
// claim: `limits.max_tokens` caps the *run*, not one `sc run`.
//
// The spend used to live only in the process, so every `--resume` of the
// same run id started counting from zero: a 4000-token cap was seen to
// produce 18854 tokens over two invocations of one run, each of them
// warning about "its 4000 budget" as though nothing had been spent. Now the
// budget rides in state.json, and a resume is worth exactly one more
// budget's worth — the same grant clearing a pause makes.
func TestRunResumedCarriesTheBudgetItAlreadySpent(t *testing.T) {
	env := runBudgetSetup(t, 500, runFireScenario(),
		runBudgetSpend{"plan": 1000, "engineer": 10, "tester": 10})

	stdout, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief,
		"--headless", "--cartridge", "mock")
	require.Equal(t, 0, code, "stdout:\n%s\nstderr:\n%s", stdout, stderr)
	require.Contains(t, stdout, "1000 tokens of its 500 budget")

	first := runLatestID(t, env.Repo.Dir)
	st, err := orchestrator.LoadState(config.RunDir(env.Repo.Dir, string(first)))
	require.NoError(t, err)
	require.Equal(t, int64(1000), st.Spent().Spend.Tokens,
		"the run's own state file must carry what it spent, or --resume cannot")

	stdout, stderr, code = runFireExec(t, env.Repo.Dir, "--resume", "--headless", "--cartridge", "mock")
	require.Equal(t, 0, code, "stdout:\n%s\nstderr:\n%s", stdout, stderr)
	require.Equal(t, first, runLatestID(t, env.Repo.Dir), "the resume must be of the same run")

	// P9-28: the notice is published as the resumed run takes its first
	// tick, which is before the headless renderer has subscribed to the
	// bus. The startup replay holds those first events until something is
	// listening, so it reaches the screen as well as the run log.
	require.Contains(t, stdout+stderr, "the run was resumed over its budget",
		"a warning raised in the run's first tick must still reach the screen")
	log, err := os.ReadFile(filepath.Join(config.RunDir(env.Repo.Dir, string(first)), orchestrator.EventLogName))
	require.NoError(t, err)
	require.Contains(t, string(log), "the run was resumed over its budget: it carries the 1000 tokens",
		"a resumed run says what it inherited rather than starting again at zero")
	require.Contains(t, string(log), "will stop again at 1500 tokens",
		"a resume is worth one more budget's worth, not a whole fresh budget")

	// And the spend the summary reports is the run's, not this process's.
	out := stdout + stderr
	require.Regexp(t, `spend +1[0-9]{3} tokens`, out,
		"the summary counts the tokens of the earlier invocation too")

	st, err = orchestrator.LoadState(config.RunDir(env.Repo.Dir, string(first)))
	require.NoError(t, err)
	require.Greater(t, st.Spent().Spend.Tokens, int64(1000),
		"the resumed run wrote its own spend back on top of what it inherited")
	require.Equal(t, 3, st.Spent().Grants,
		"one grant to start with, and the two it took to put 1000 tokens under a 500 ceiling")
}

// TestRunAssemblyKeepsTheBudgetForTheTUIKey is P9-14's wire point: the `b`
// key raises the live budget through the controller `sc run` attaches, so
// the assembled run has to keep the budget it built rather than letting it
// disappear into the four wrappers.
func TestRunAssemblyKeepsTheBudgetForTheTUIKey(t *testing.T) {
	env := runFireSetup(t, true)
	rc, set := runAssembleSetup(t, env)

	c, err := assembleRun(context.Background(), rc, set)
	require.NoError(t, err)
	defer c.close()

	require.NotNil(t, c.budget, "the run's budget must survive the assembly for the TUI to raise it")
	ctl := &runController{run: c.run, budget: c.budget}
	require.Implements(t, (*tui.BudgetController)(nil), ctl)
}

// TestRunControllerRaisesTheLiveBudget drives the seam the `b` key calls:
// the amount is refused when it is under what the run has already spent,
// and accepted when it is above, which lets the held run carry on.
func TestRunControllerRaisesTheLiveBudget(t *testing.T) {
	budget := orchestrator.NewRunBudget(orchestrator.RunBudgetOptions{
		Limits: config.Limits{MaxTokens: 500}, Mode: domain.RunModeAutopilot,
	})
	budget.Record(cartridge.Usage{InputTokens: 700})
	require.True(t, budget.Tripped(), "the budget must be held before it can be raised")

	ctl := &runController{budget: budget}
	err := ctl.SetBudget(context.Background(), 100)
	require.ErrorIs(t, err, orchestrator.ErrBudgetTooSmall)
	require.True(t, budget.Tripped(), "a refused raise leaves the run held")

	require.NoError(t, ctl.SetBudget(context.Background(), 5000))
	require.False(t, budget.Tripped(), "the raised budget lets the run carry on")
	require.Equal(t, int64(700), budget.Spent().Tokens, "raising a budget spends nothing")

	// A run with no cap at all says so rather than pretending.
	none := &runController{budget: orchestrator.NewRunBudget(orchestrator.RunBudgetOptions{})}
	require.ErrorIs(t, none.SetBudget(context.Background(), 1000), orchestrator.ErrNoBudget)
}
