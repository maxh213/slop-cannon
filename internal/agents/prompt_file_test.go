package agents

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// pfContractSuffixed returns the prompt structured.Fire actually sends for
// an impl_report firing: the spec's prompt with the output contract
// appended.
func pfContractSuffixed(t *testing.T, text string) string {
	t.Helper()
	c, err := ContractFor(domain.MessageImplReport)
	require.NoError(t, err)
	return c.WithPromptSuffix(text)
}

// pfMode returns the permission bits of path.
func pfMode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	require.NoError(t, err)
	return fi.Mode().Perm()
}

// TestRunAgentFilesThePromptItFired is P5-13's acceptance test: a real
// firing through the mock cartridge leaves <run-dir>/prompts/<agent-id>.md
// behind, holding exactly the text the cartridge was given, with the
// permissions internal/logging uses for run state (D-23).
func TestRunAgentFilesThePromptItFired(t *testing.T) {
	t.Parallel()
	s := mock.NewScenario("filed")
	s.Rule("implement", mock.Match{}).Say("Done.").Structured(runReport)
	a := runMockAdapter(t, s)

	runDir := filepath.Join(t.TempDir(), "runs", "run-1")
	run := runAgentRun(t, a, nil)
	run.RunDir = runDir

	_, tr, err := RunAgent[domain.ImplReport](context.Background(), run)
	require.NoError(t, err)

	path := prompt.PathFor(runDir, run.Agent.ID)
	assert.Equal(t, path, tr.PromptPath, "the transcript names the copy it filed")
	assert.Equal(t, filepath.Join(runDir, "prompts", "eng-sc-1a2b-r1.md"), path)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	want := pfContractSuffixed(t, run.Spec.Prompt)
	assert.Equal(t, want, string(got), "the file holds the prompt the cartridge was given")

	// And the cartridge really was given that text, so the copy is not a
	// plausible-looking reconstruction.
	calls := a.Calls()
	require.Len(t, calls, 1)
	assert.Equal(t, want, calls[0].Spec.Prompt)

	// A prompt quotes the user's rules and vibes, so it is no more
	// readable than the run log: 0600 in a 0700 directory.
	assert.Equal(t, fs.FileMode(0o600), pfMode(t, path))
	assert.Equal(t, fs.FileMode(0o700), pfMode(t, prompt.Dir(runDir)))
}

// TestRunAgentFilesTheRetryPrompt pins the "prompt actually fired" rule
// through a retry: the copy is replaced, so the file is the last attempt's
// prompt rather than the first's.
func TestRunAgentFilesTheRetryPrompt(t *testing.T) {
	t.Parallel()
	s := mock.NewScenario("retry-filed")
	s.Rule("bad", mock.Match{}).Once().Say("Here you go.").
		Structured(json.RawMessage(`{"nope":1}`))
	s.Rule("good", mock.Match{}).Say("Sorry, corrected.").Structured(runReport)
	a := runMockAdapter(t, s)

	runDir := t.TempDir()
	run := runAgentRun(t, a, nil)
	run.RunDir = runDir

	_, tr, err := RunAgent[domain.ImplReport](context.Background(), run)
	require.NoError(t, err)
	require.Len(t, tr.Attempts, 2)

	got, err := os.ReadFile(tr.PromptPath)
	require.NoError(t, err)
	assert.Equal(t, tr.Attempts[1].Prompt, string(got))
	assert.Contains(t, string(got), "Correction required")
	assert.NotEqual(t, tr.Attempts[0].Prompt, string(got))

	// One agent, one file: the retry replaced the copy rather than adding
	// a second one.
	entries, err := os.ReadDir(prompt.Dir(runDir))
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

// TestRunAgentWithoutARunDirectoryFilesNothing keeps the field optional:
// the role builders' own tests, and any caller with no run directory, fire
// exactly as before.
func TestRunAgentWithoutARunDirectoryFilesNothing(t *testing.T) {
	t.Parallel()
	s := mock.NewScenario("unfiled")
	s.Rule("implement", mock.Match{}).Say("Done.").Structured(runReport)
	a := runMockAdapter(t, s)

	_, tr, err := RunAgent[domain.ImplReport](context.Background(), runAgentRun(t, a, nil))
	require.NoError(t, err)
	assert.Empty(t, tr.PromptPath)
}

// TestRunAgentSurvivesAnUnwritablePromptDirectory: a debug copy is not
// worth losing an agent's work over, so an unwritable state directory is
// one line on the agent's own output and the firing goes ahead.
func TestRunAgentSurvivesAnUnwritablePromptDirectory(t *testing.T) {
	t.Parallel()
	s := mock.NewScenario("unwritable")
	s.Rule("implement", mock.Match{}).Say("Done.").Structured(runReport)
	a := runMockAdapter(t, s)

	// A regular file where the run directory should be: MkdirAll of
	// <run-dir>/prompts cannot succeed under it.
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocked, []byte("x"), 0o600))

	bus := events.NewBus()
	t.Cleanup(bus.Close)
	collect := runBusSink(t, bus)

	run := runAgentRun(t, a, bus)
	run.RunDir = blocked

	report, tr, err := RunAgent[domain.ImplReport](context.Background(), run)
	require.NoError(t, err, "the firing goes ahead")
	assert.Equal(t, "Renamed the helper and updated its two callers.", report.Summary)
	assert.Empty(t, tr.PromptPath)

	var noted string
	for _, line := range runLinesOn(collect(), events.StreamStderr) {
		if strings.HasPrefix(line, "[prompt]") {
			noted = line
		}
	}
	assert.Contains(t, noted, "could not be written",
		"the failure is reported on the agent's output stream")
}

// TestRunAgentRefusesARelativeRunDirectory: the agent's own working
// directory is the target repository's worktree, so a relative run
// directory would file our debug copy inside the user's repository, which
// D-23 forbids. It is a programming error, not a transient one, so the
// firing is refused before any subprocess starts.
func TestRunAgentRefusesARelativeRunDirectory(t *testing.T) {
	t.Parallel()
	s := mock.NewScenario("relative")
	s.Rule("implement", mock.Match{}).Say("Done.").Structured(runReport)
	a := runMockAdapter(t, s)

	run := runAgentRun(t, a, nil)
	run.RunDir = filepath.Join(".slop-cannon", "run")

	_, tr, err := RunAgent[domain.ImplReport](context.Background(), run)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidAgentRun)
	assert.Contains(t, err.Error(), "is not absolute")
	assert.Empty(t, tr.Attempts)
	assert.Empty(t, a.Calls(), "nothing was fired")
}
