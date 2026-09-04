package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// P9-09 puts the redaction hook at the one point a run publishes, and P9-05
// supplies the rules it applies. The tests beside this one plant credentials
// the rules recognise at the start of a line — `sk-ant-…`, `Bearer …`,
// `AKIA…`, `DATABASE_PASSWORD=…` as the first thing on its line — which is
// the shape a leak takes least often. A credential reaches a transcript,
// a bead comment or a pull request body mostly *inside* a line somebody
// else wrote: a check's log line, a sentence in a report, a quoted command.
// The round-4 review found that the `assignment` rule was anchored to the
// start of a line and so masked none of those, and that no test in the
// repository would have noticed.
//
// This file is that test, driven through the real binary: the same
// credential assignment, always behind a prefix, planted in what the
// cartridge prints and in the structured prose the run stores and sends,
// and then looked for in every sink a run has — the terminal, the run log,
// the event log, every agent transcript, every prompt, the bead tracker's
// own store and the body of the pull request, which is the only one of them
// that leaves the machine.

const (
	// p909SecretName looks like an environment variable holding a
	// credential, which is what the `assignment` rule keys on.
	p909SecretName = "MY_API_TOKEN"
	// p909SecretValue is the value that must reach no sink at all. It is
	// matched by no other rule: nothing about it is shaped like an
	// `sk-ant-…` key, so this test fails unless the assignment rule fires.
	p909SecretValue = "prefixedsecretvalue2"
	// p909Assignment is the credential as an agent would print it.
	p909Assignment = p909SecretName + "=" + p909SecretValue
	// p909Masked is what a reader should see instead.
	p909Masked = p909SecretName + "=[REDACTED]"
)

// TestRunMasksACredentialAssignmentInEverySink fires a whole run whose
// cartridge prints, reports and reviews a credential assignment that is
// never the first thing on its line, and then reads every sink the run
// wrote. The value must be in none of them, and the masked form must be in
// the terminal, so the test cannot pass by the run failing to say anything.
func TestRunMasksACredentialAssignmentInEverySink(t *testing.T) {
	env := runFireSetup(t, true)
	require.NoError(t, p909Scenario().WriteFile(env.Scenario))

	stdout, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief,
		"--headless", "--cartridge", "mock")
	require.Equal(t, 0, code, "stdout:\n%s\nstderr:\n%s", stdout, stderr)
	require.Contains(t, stdout, "run finished")
	require.Contains(t, stdout, p909Masked,
		"the line still reaches the reader, with the value masked")

	sinks := map[string]string{"stdout": stdout, "stderr": stderr}
	dir := config.RunDir(env.Repo.Dir, string(runLatestID(t, env.Repo.Dir)))
	// Everything the run wrote under its own directory: sc.log,
	// events.jsonl, every agent transcript and every prompt (D-23).
	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, rerr := os.ReadFile(path) //nolint:gosec // the test's own run directory
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		sinks["<run>/"+filepath.ToSlash(rel)] = string(data)
		return nil
	}))
	// The bead tracker's own store, which holds the comment the feedback
	// loop writes on the bead.
	sinks["bd store"] = p909BDStore(t, env)
	// The pull request body, the only sink that leaves the machine.
	bodies := ghPRBodies(t, env)
	require.NotEmpty(t, bodies, "the run opened no pull request, so nothing was sent anywhere")
	for i, body := range bodies {
		sinks["gh pr body "+strconv.Itoa(i+1)] = body
		assert.Contains(t, body, "## Implementation report", "the body is still the real one")
	}

	require.Contains(t, sinks, "<run>/events.jsonl")
	require.Contains(t, sinks, "<run>/sc.log")
	var transcripts int
	for name, text := range sinks {
		if strings.HasPrefix(name, "<run>/agents/") {
			transcripts++
		}
		assert.NotContains(t, text, p909SecretValue, "%s carries the credential", name)
	}
	assert.Positive(t, transcripts, "the run wrote no agent transcript, so that sink went unchecked")
	assert.Contains(t, sinks["bd store"], "Implementation report",
		"the bd store holds no report, so that sink went unchecked")
}

// p909Scenario is [runFireScenario] with the credential assignment planted
// wherever an agent's own words travel: what the cartridge prints, the
// engineer's report (which the bd comment, the pull request body and every
// later prompt quote) and a tester's verdict. Every one of them puts it
// behind a prefix, because that is the case the anchored rule missed.
func p909Scenario() *mock.Scenario {
	sc := mock.NewScenario("sc run prints a prefixed credential assignment")
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
		Say("2026-01-01 INFO  the check printed "+p909Assignment+" before it passed").
		Write(runFireWork, "the first note\n").
		Structured(domain.ImplReport{
			Summary:      "Added " + runFireWork + "; the failing check logged " + p909Assignment + " first.",
			FilesChanged: []string{runFireWork},
			HowToVerify:  "run: env " + p909Assignment + " ls " + runFireWork,
			Risks:        []string{"the log line ended with " + p909Assignment},
		})
	sc.Rule("tester", mock.Match{Role: string(domain.RoleTester)}).
		Say("Reading the diff.").
		Structured(domain.Verdict{
			Outcome: domain.VerdictApprove,
			Summary: "Fine, though the report quotes " + p909Assignment + " in its risks.",
		})
	sc.Otherwise("unscripted").
		Say("no rule matched this firing").
		Fail(mock.FailCrash, "the run fired an agent this scenario does not script")
	return sc
}

// p909BDDir finds the fake tracker's state directory the way the shim
// carries it: [runFireBDScript] writes it into the generated script as an
// exported variable, so the script itself is the record of where the bead
// database lives.
var p909BDDir = regexp.MustCompile(`SC_E2E_FAKEBD_DIR=("[^"]*")`)

// p909BDStore returns the fake tracker's database as text: the beads, their
// fields and the comments the run posted on them.
func p909BDStore(t *testing.T, env *runFireEnv) string {
	t.Helper()
	script, err := os.ReadFile(env.BD.Path) //nolint:gosec // the test's own shim
	require.NoError(t, err)
	m := p909BDDir.FindSubmatch(script)
	require.Len(t, m, 2, "the bd shim no longer names its state directory: %s", script)
	dir, err := strconv.Unquote(string(m[1]))
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(dir, "db.json")) //nolint:gosec // the test's own tracker
	require.NoError(t, err, "the fake tracker wrote no database")
	return string(data)
}
