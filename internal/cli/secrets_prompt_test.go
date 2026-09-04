package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// P9-05's first clause — "prompts never include .env*, *.pem, id_*, or files
// matched by a configurable exclude list" — driven through a whole run, from
// the other end: not by asking the filter what it would hide, but by asking
// the agents what they were actually shown.
//
// It is the leak an earlier review found and the reason this test exists.
// The path half of the filter was written, tested and wired into the
// repository scan, but not into the diff the loop hands a tester: an
// engineer that wrote `.env` had it committed as leftovers (D-14), and the
// contents came back in the diff of every review prompt of that round. A
// unit test of [agents.SecretFilter.GitDiff] cannot catch that — it passes
// whether or not anything calls it — so the assertion here is made by the
// cartridge itself, which is the only party that sees a finished prompt.
//
// TestRunNeverPutsAPlantedSecretInAPrompt covers the breadth of the same
// clause: three planted values, the `exclude` config key, the tree half of
// the prompt and the prompt files on disk. This one covers the one thing
// breadth cannot prove — that the diff is *filtered rather than absent*. A
// run that stopped handing testers a diff at all would satisfy every "the
// secret is not in the prompt" assertion ever written, and would be a
// different bug; here the tester approves only when the visible half of the
// engineer's change is in front of it, so the excluded files are the only
// thing missing.

// runLeakCanary appears in this test only inside files the filter must hide.
// It is not credential-shaped on purpose: the value redactor must not be
// what saves this run, because it is the *path* rule that is under test.
const runLeakCanary = "LEAKCANARY-b41c9d2e"

// runLeakVisible is the content of the file the engineer is *supposed* to
// write, which every review prompt of that round must carry: the diff is
// filtered, not withheld.
const runLeakVisible = "the first note"

// runLeakNoDiff is what a tester says when it was shown no diff at all.
const runLeakNoDiff = "THE-DIFF-NEVER-REACHED-THE-TESTER"

// runLeakReached is what the mock says when a prompt carries the canary.
// The scenario's first rule matches on the prompt, so any firing that was
// given the contents of an excluded file says this and fails, which puts the
// string in the run's output twice over.
const runLeakReached = "CANARY-REACHED-A-PROMPT"

// TestRunNeverQuotesAnExcludedFileInAPrompt fires a whole run in which the
// engineer writes two excluded files beside its real work: `.env`
// (DefaultSecretPatterns' first rule) and `config/id_rsa` (the `id_*` rule,
// in a subdirectory, so the match is on the file name rather than the path).
// Both are committed with the round's leftovers and are therefore in the
// branch diff the review round is built from.
//
// The run must finish normally, and no firing may ever have been handed
// either file's contents.
func TestRunNeverQuotesAnExcludedFileInAPrompt(t *testing.T) {
	env := runFireSetup(t, false)
	require.NoError(t, runLeakScenario().WriteFile(env.Scenario))

	stdout, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief,
		"--headless", "--cartridge", "mock")
	require.Equal(t, 0, code, "stdout:\n%s\nstderr:\n%s", stdout, stderr)

	// The assertion the cartridge makes: no prompt carried the canary.
	require.NotContains(t, stdout, runLeakReached,
		"an agent was given the contents of an excluded file:\n%s", stdout)
	require.NotContains(t, stderr, runLeakReached)
	require.NotContains(t, stdout, runLeakNoDiff,
		"a tester was shown no diff at all, so this run proves nothing:\n%s", stdout)

	// And the canary is nowhere in what the run wrote, either: the run log,
	// the event stream and the terminal are all built from the same events.
	require.NotContains(t, stdout, runLeakCanary, "the terminal quoted the excluded file")
	require.NotContains(t, stderr, runLeakCanary)
	dir := config.RunDir(env.Repo.Dir, string(runLatestID(t, env.Repo.Dir)))
	for _, name := range []string{"events.jsonl", "sc.log"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err, name)
		require.NotContains(t, string(data), runLeakCanary, "%s quoted the excluded file", name)
	}

	// Non-vacuity: the engineer really did write both files, they really
	// were committed, and the diff a tester would have been shown really
	// did contain them. Without this the test would pass just as happily
	// against a run in which no agent wrote anything at all.
	for _, path := range []string{".env", "config/id_rsa"} {
		out, err := exec.Command("git", "-C", env.Repo.Dir, "show", "HEAD:"+path).CombinedOutput()
		require.NoError(t, err, "%s was never committed, so nothing was hidden: %s", path, out)
		require.Contains(t, string(out), runLeakCanary, "%s reached the branch without the canary", path)
	}
}

// runLeakScenario is [runFireScenarioSaying]'s script with two differences:
// the engineer writes the two excluded files as well as its real work, and
// the first rule of all fires on any prompt carrying the canary — whichever
// role, whichever round — so a leak into any prompt announces itself and
// stops that agent.
func runLeakScenario() *mock.Scenario {
	sc := mock.NewScenario("an engineer that writes secrets")
	sc.Rule("canary", mock.Match{PromptContains: runLeakCanary}).
		Say(runLeakReached).
		Fail(mock.FailCrash, runLeakReached)
	sc.Rule("plan", mock.Match{Role: string(domain.RolePO), Task: string(events.TaskPlan)}).
		Say("One bead is enough.").
		Structured(domain.Plan{
			Brief: runFireBrief,
			Beads: []domain.PlanBead{{
				Key: "notes", Title: "Add a notes file", Acceptance: runFireWork + " exists",
				Description: "Start the notes directory.", Priority: domain.PriorityDefault,
				Type: domain.BeadTypeTask,
			}},
		})
	sc.Rule("engineer", mock.Match{Role: string(domain.RoleEngineer)}).
		Say("Writing the note, and some local settings beside it.").
		Write(runFireWork, "the first note\n").
		// The two files the run must never quote. Neither path contains the
		// canary, so anything that finds it in a prompt found the contents.
		Write(".env", "AWS_SECRET_ACCESS_KEY="+runLeakCanary+"\n").
		Write("config/id_rsa", "-----BEGIN OPENSSH PRIVATE KEY-----\n"+runLeakCanary+"\n-----END OPENSSH PRIVATE KEY-----\n").
		Structured(domain.ImplReport{
			Summary: "Added " + runFireWork + ".", FilesChanged: []string{runFireWork},
			HowToVerify: "ls " + runFireWork,
		})
	// The tester approves only when the visible half of the diff really is
	// in its prompt. That is this test's guard against passing for the
	// wrong reason: a run that showed the testers no diff at all would hide
	// the canary just as well, and would be a different bug.
	sc.Rule("tester", mock.Match{Role: string(domain.RoleTester), PromptContains: runLeakVisible}).
		Say("Reading the diff.").
		Structured(domain.Verdict{Outcome: domain.VerdictApprove, Summary: "The note reads well."})
	sc.Rule("tester-without-a-diff", mock.Match{Role: string(domain.RoleTester)}).
		Say(runLeakNoDiff).
		Fail(mock.FailCrash, runLeakNoDiff)
	sc.Otherwise("unscripted").
		Say("no rule matched this firing").
		Fail(mock.FailCrash, "the run fired an agent this scenario does not script")
	return sc
}
