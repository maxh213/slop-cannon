package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// P9-05's Do clause is about a whole run: "prompts never include .env*,
// *.pem, id_*, or files matched by a configurable exclude list". A test that
// filters a diff itself and then builds a prompt out of the result proves
// only that the filter compiles, so this one plants the secrets in the
// repository the way a careless engineer would and lets the product's own
// prompt matcher report them: a mock rule that fires on `prompt_contains`
// is the cartridge saying the secret was in the prompt it was given.
const (
	// runSecretPassword is planted in a dotenv file, runSecretKey in a PEM
	// file and runSecretConfigured in a path only the `exclude` config key
	// hides. None of the three may reach a prompt, and neither may the
	// paths that carry them.
	runSecretPassword   = "hunter2-hunter2-hunter2"
	runSecretKey        = "plantedprivatekeymaterial"
	runSecretConfigured = "deployment-token-plantedvalue"
	// runLeakMarker is what a leak rule says. It cannot occur in a clean
	// run, so one line of the run's own output carrying it is the failure.
	runLeakMarker = "SECRET-REACHED-A-PROMPT"
)

// runSecretPromptTripwires are the strings no prompt of the run may hold:
// the three planted values and the paths of the five files that carry them.
func runSecretPromptTripwires() []string {
	return []string{
		runSecretPassword, runSecretKey, runSecretConfigured,
		".env", "prod.pem", "server.pem", "deploy.txt",
	}
}

// runFireScenarioPlantingSecrets is [runFireScenario] with two changes: the
// engineer writes three secret files beside its note, and a tripwire rule
// per forbidden string sits in front of every other rule, so any firing
// whose prompt carries one is answered by the tripwire instead of by the
// rule that would have done the work.
func runFireScenarioPlantingSecrets() *mock.Scenario {
	sc := mock.NewScenario("sc run plants secrets")
	for _, secret := range runSecretPromptTripwires() {
		sc.Rule("leak/"+secret, mock.Match{PromptContains: secret}).
			Say(runLeakMarker+": the prompt contains "+secret).
			Fail(mock.FailCrash, runLeakMarker+": "+secret)
	}
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
		Say("Adding the note.").
		Write(runFireWork, "the first note\n").
		Write(".env", "DATABASE_PASSWORD="+runSecretPassword+"\n").
		Write("certs/server.pem", "-----BEGIN RSA PRIVATE KEY-----\n"+runSecretKey+"\n-----END RSA PRIVATE KEY-----\n").
		Write("secrets/deploy.txt", runSecretConfigured+"\n").
		Structured(domain.ImplReport{
			Summary: "Added " + runFireWork + ".",
			// The engineer names what it wrote, secrets included. This is
			// the half of the path filter git cannot supply: the run never
			// asked for these paths, an agent volunteered them, and every
			// later prompt, the bd comment and the pull request body quote
			// the report verbatim.
			FilesChanged: []string{runFireWork, ".env", "certs/server.pem", "secrets/deploy.txt"},
			HowToVerify:  "ls " + runFireWork,
		})
	sc.Rule("tester", mock.Match{Role: string(domain.RoleTester)}).
		Say("Reading the diff.").
		Structured(domain.Verdict{Outcome: domain.VerdictApprove, Summary: "The note reads well."})
	sc.Otherwise("unscripted").
		Say("no rule matched this firing").
		Fail(mock.FailCrash, "the run fired an agent this scenario does not script")
	return sc
}

// TestRunNeverPutsAPlantedSecretInAPrompt is P9-05's Done-when for the path
// half, driven through the real binary: a repository that already holds a
// dotenv file, a key and an excluded path, and an engineer that writes three
// more, must leave none of them — value or path — in any prompt the run
// builds, whether the prompt lists the tree or quotes the diff.
func TestRunNeverPutsAPlantedSecretInAPrompt(t *testing.T) {
	env := runFireSetup(t, false)
	require.NoError(t, runFireScenarioPlantingSecrets().WriteFile(env.Scenario))

	// The configurable half: one pattern the built-in list does not cover.
	configWriteFile(t, config.RepoConfigPath(env.Repo.Dir), "exclude = [\"secrets/*\"]\n")

	// Secrets that were already in the repository before the run started,
	// so the first prompt of all — the plan, which lists the tree — is
	// covered too. They are committed because gitx refuses to add a
	// worktree to a dirty repository, and a leaked credential is one that
	// was committed anyway.
	env.Repo.WriteFile(t, ".env", "DATABASE_PASSWORD="+runSecretPassword+"\n")
	env.Repo.WriteFile(t, "certs/prod.pem",
		"-----BEGIN RSA PRIVATE KEY-----\n"+runSecretKey+"\n-----END RSA PRIVATE KEY-----\n")
	env.Repo.WriteFile(t, "secrets/deploy.txt", runSecretConfigured+"\n")
	env.Repo.Commit(t, "plant the secrets a careless commit would leave behind")

	stdout, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief,
		"--headless", "--cartridge", "mock")
	require.Equal(t, 0, code, "stdout:\n%s\nstderr:\n%s", stdout, stderr)

	// The tripwires: the mock only says this when the prompt it was handed
	// carried the secret.
	assert.NotContains(t, stdout, runLeakMarker, "a prompt of the run carried a planted secret")
	assert.NotContains(t, stderr, runLeakMarker)

	// Non-vacuity: the loop really did run, so the tripwires really were in
	// front of a tester's prompt.
	require.Contains(t, stdout, "fired tester", "no tester was fired, so no diff was ever reviewed")
	require.Contains(t, stdout, "run finished")

	// And the one prompt a run keeps on disk (D-23) says the same thing.
	dir := config.RunDir(env.Repo.Dir, string(runLatestID(t, env.Repo.Dir)))
	prompts := readPromptFiles(t, filepath.Join(dir, prompt.PromptsDirName))
	require.NotEmpty(t, prompts, "the run wrote no prompts to read back: %v", promptNames(prompts))
	for name, text := range prompts {
		for _, secret := range runSecretPromptTripwires() {
			assert.NotContains(t, text, secret, "prompts/%s leaks %q", name, secret)
		}
		assert.Contains(t, text, "main.go", "prompts/%s must still carry the repository", name)
	}

	// The reply half, which the tripwires above cover for the prompts a
	// tester is handed but which leaves no prompt file behind (only the
	// planning firing writes one today, P5-13): the engineer's report is
	// filtered once, where it is stored, so the event log — and with it the
	// bd comment and the pull request body, which are written from the same
	// filtered copy — says a path was withheld rather than naming it.
	data, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	require.NoError(t, err)
	lists := 0
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, "files_changed") {
			continue
		}
		lists++
		for _, secret := range []string{".env", "server.pem", "deploy.txt"} {
			assert.NotContains(t, line, secret, "an event's file list names %q", secret)
		}
		assert.Contains(t, line, agents.SecretPathPlaceholder)
	}
	assert.Positive(t, lists, "no event carried the engineer's file list, so nothing was proved about it")
}

// TestRunNeverNamesAnExcludedPathToTheRemote is the same reply half at the
// one sink that leaves the machine. `gh pr create` writes the implementation
// report's file list into the body verbatim, so an engineer that listed
// ".env" would publish the path to GitHub.
func TestRunNeverNamesAnExcludedPathToTheRemote(t *testing.T) {
	env := runFireSetup(t, true)
	require.NoError(t, runFireScenarioPlantingSecrets().WriteFile(env.Scenario))
	configWriteFile(t, config.RepoConfigPath(env.Repo.Dir), "exclude = [\"secrets/*\"]\n")
	// The repo config lives inside the repository, and gitx refuses to add
	// a worktree to a dirty one.
	env.Repo.Commit(t, "configure the exclude list")

	stdout, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief,
		"--headless", "--cartridge", "mock")
	require.Equal(t, 0, code, "stdout:\n%s\nstderr:\n%s", stdout, stderr)
	assert.NotContains(t, stdout, runLeakMarker, "a prompt of the run carried a planted secret")
	require.Contains(t, stdout, "run finished")

	bodies := ghPRBodies(t, env)
	require.NotEmpty(t, bodies, "the run opened no pull request, so nothing was sent anywhere")
	for _, body := range bodies {
		for _, secret := range runSecretPromptTripwires() {
			assert.NotContains(t, body, secret, "the pull request body leaks %q", secret)
		}
		assert.Contains(t, body, "## Implementation report", "the body is still the real one")
		assert.Contains(t, body, agents.SecretPathPlaceholder,
			"the body should say a file was withheld rather than drop the list")
		assert.Contains(t, body, runFireWork, "and the paths that are not excluded are still named")
	}
}

// runP905Password and runP905AWS are credentials only P9-05's own rules
// know: logging.DefaultPatterns (P0-07) matches neither, so a run that
// masks them is a run whose redactor really is the filter's.
const (
	runP905PasswordName  = "DATABASE_PASSWORD"
	runP905PasswordValue = "correct-horse-battery-staple-9"
	runP905AWSTail       = "PLANTEDKEYID7Q"
	runP905AWS           = "AKIA" + runP905AWSTail
)

// TestRunRedactsTheCredentialsOnlyTheP905RulesKnow is the non-vacuity guard
// on the value half's wiring. The end-to-end redaction test plants an
// `sk-ant-…` token, which P0-07's default rules already mask, so it passes
// whether `sc run` installs [agents.SecretFilter.Redactor] or a plain
// logging.NewRedactor. These two are matched only by the rules P9-05 adds —
// the credential assignment and the AWS access key id — so the terminal, the
// event log and the run log stay clean only if the filter's redactor is the
// one wired in.
func TestRunRedactsTheCredentialsOnlyTheP905RulesKnow(t *testing.T) {
	env := runFireSetup(t, false)
	sc := runFireScenarioSaying(runP905PasswordName + "=" + runP905PasswordValue +
		"\nexporting AWS_ACCESS_KEY_ID=" + runP905AWS)
	require.NoError(t, sc.WriteFile(env.Scenario))

	stdout, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief,
		"--headless", "--cartridge", "mock")
	require.Equal(t, 0, code, stderr)
	require.Contains(t, stdout, "[REDACTED]", "the lines themselves still reach the reader")
	require.Contains(t, stdout, runP905PasswordName+"=", "and keep the shape a reader needs")

	dir := config.RunDir(env.Repo.Dir, string(runLatestID(t, env.Repo.Dir)))
	sinks := map[string]string{"stdout": stdout, "stderr": stderr}
	for _, name := range []string{"events.jsonl", "sc.log"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err, name)
		sinks[name] = string(data)
	}
	for where, text := range sinks {
		assert.NotContains(t, text, runP905PasswordValue, "%s carries the password", where)
		assert.NotContains(t, text, runP905AWSTail, "%s carries the access key id", where)
	}
}

// TestRunRefusesAMalformedExcludePatternWithAdvice is P9-05's refusal in the
// shape P8-06 gives every other startup failure: the run that could not
// start, the reason, a numbered fix and `sc doctor`. The pattern is not a
// config *parse* error — it is a well-formed string until the filter
// compiles it — so it needs its own class in the startup advice.
func TestRunRefusesAMalformedExcludePatternWithAdvice(t *testing.T) {
	env := runFireSetup(t, false)
	configWriteFile(t, config.RepoConfigPath(env.Repo.Dir), "exclude = [\"secrets/[\"]\n")

	stdout, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief,
		"--headless", "--cartridge", "mock")
	require.Equal(t, 1, code, "stdout:\n%s\nstderr:\n%s", stdout, stderr)
	assert.Contains(t, stderr, "sc could not start a run in "+env.Repo.Dir)
	// The message docs/troubleshooting.md documents, still word for word.
	assert.Contains(t, stderr, "exclude pattern \"secrets/[\" is malformed")
	assert.Contains(t, stderr, "Fix it in this order:")
	assert.Contains(t, stderr, startupFixExclude)
	assert.Contains(t, stderr, "Then check everything at once with: sc doctor")
	assert.NoDirExists(t, filepath.Join(config.StateDir(env.Repo.Dir), "runs"),
		"a refused run leaves no run directory behind")
}

// readPromptFiles reads every prompt the run wrote, keyed by file name.
func readPromptFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "the run directory should have a %s directory", prompt.PromptsDirName)
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // the test's own run directory
		require.NoError(t, err)
		out[e.Name()] = string(data)
	}
	return out
}

// promptNames lists the prompt files a run wrote, sorted.
func promptNames(prompts map[string]string) []string {
	out := make([]string, 0, len(prompts))
	for k := range prompts {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestRunRedactsWhatAnAgentPrintsInThePullRequestBody is the other half of
// P9-05's value clause: a credential an agent echoed must not leave the
// machine. `gh pr create` is the one sink that sends agent prose to a
// remote, and the fake `gh` records the body it was handed.
func TestRunNeverSendsACredentialToTheRemote(t *testing.T) {
	env := runFireSetup(t, true)
	require.NoError(t, runFireScenarioEchoingACredential().WriteFile(env.Scenario))

	stdout, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief,
		"--headless", "--cartridge", "mock")
	require.Equal(t, 0, code, "stdout:\n%s\nstderr:\n%s", stdout, stderr)
	require.Contains(t, stdout, "run finished")

	bodies := ghPRBodies(t, env)
	require.NotEmpty(t, bodies, "the run opened no pull request, so nothing was sent anywhere")
	for _, body := range bodies {
		assert.NotContains(t, body, runFireSecret, "the pull request body leaks the credential")
		assert.Contains(t, body, "## Implementation report", "the body is still the real one")
	}
}

// runFireScenarioEchoingACredential is [runFireScenario] with the credential
// in the structured prose the pull request body renders — the engineer's
// report summary and the testers' verdict summaries — rather than only in
// what the cartridge printed.
func runFireScenarioEchoingACredential() *mock.Scenario {
	sc := mock.NewScenario("sc run echoes a credential")
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
		Say("Adding the note.").
		Write(runFireWork, "the first note\n").
		Structured(domain.ImplReport{
			Summary:      "Added " + runFireWork + " with ANTHROPIC_API_KEY=" + runFireSecret + " set.",
			FilesChanged: []string{runFireWork},
			HowToVerify:  "ANTHROPIC_API_KEY=" + runFireSecret + " ls " + runFireWork,
		})
	sc.Rule("tester", mock.Match{Role: string(domain.RoleTester)}).
		Say("Reading the diff.").
		Structured(domain.Verdict{
			Outcome: domain.VerdictApprove,
			Summary: "Fine, though the report quotes\nANTHROPIC_API_KEY=" + runFireSecret,
		})
	sc.Otherwise("unscripted").
		Say("no rule matched this firing").
		Fail(mock.FailCrash, "the run fired an agent this scenario does not script")
	return sc
}

// ghPRBodies returns the bodies the fake `gh` was handed on stdin for each
// `gh pr create`; the stand-in writes one file per call.
func ghPRBodies(t *testing.T, env *runFireEnv) []string {
	t.Helper()
	entries, err := os.ReadDir(env.GH.Dir)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "body.") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(env.GH.Dir, e.Name())) //nolint:gosec // the test's own shim directory
		require.NoError(t, err)
		out = append(out, string(data))
	}
	return out
}
