package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
)

// Tests for `sc resume` (P8-05). It is sugar for `sc PATH --resume`, so
// most of these assert exactly that: the two forms resolve to the same
// thing, character for character. The behaviour of resuming itself is
// tested where it lives (run_test.go's TestRunResume and
// TestRunResumeRestoresTheFrozenSnapshot).

// resumeSetup makes a repository with a cartridge and two recorded runs,
// the newer of which is the one a resume continues.
func resumeSetup(t *testing.T) (base, repo string) {
	t.Helper()
	base, repo = runTestEnv(t)
	runAddCartridge(t, "claude", "fable")
	for _, id := range []string{"20260901-000000-1111", "20260902-000000-2222"} {
		runWriteRunState(t, repo, id)
	}
	return base, repo
}

func TestResumeHelpGolden(t *testing.T) {
	out, err := rootRun(t, "resume", "--help")
	require.NoError(t, err)
	rootGolden(t, "resume-help", out)
}

// TestResumeIsTheRunFormWithTheFlag is the Done-when: whatever
// `sc PATH --resume` resolves to, `sc resume PATH` resolves to as well.
func TestResumeIsTheRunFormWithTheFlag(t *testing.T) {
	base, repo := resumeSetup(t)

	sugar, sugarErr, code := runResolve(t, "resume", repo)
	require.Equal(t, 0, code, sugarErr)
	flag, flagErr, code := runResolve(t, repo, "--resume")
	require.Equal(t, 0, code, flagErr)
	require.Equal(t, flag, sugar)
	require.Equal(t, flagErr, sugarErr)
	require.Contains(t, runNormalise(sugar, base, repo), "run        20260902-000000-2222 (resumed)\n")

	// The overrides that sit above the frozen snapshot work the same way,
	// notes included (P6-11).
	sugar, sugarErr, code = runResolve(t, "resume", repo, "--ammo", "6", "--headless")
	require.Equal(t, 0, code, sugarErr)
	flag, flagErr, code = runResolve(t, repo, "--resume", "--ammo", "6", "--headless")
	require.Equal(t, 0, code, flagErr)
	require.Equal(t, flag, sugar)
	require.Equal(t, flagErr, sugarErr)
}

// TestResumeWithoutAPathUsesTheRepoFlag: `sc resume [--repo]` is the form
// P8-05 names, so the path is optional.
func TestResumeWithoutAPathUsesTheRepoFlag(t *testing.T) {
	_, repo := resumeSetup(t)

	byFlag, stderr, code := runResolve(t, "resume", "--repo", repo)
	require.Equal(t, 0, code, stderr)
	byPath, _, code := runResolve(t, "resume", repo)
	require.Equal(t, 0, code)
	require.Equal(t, byPath, byFlag)

	// And the current directory when neither is given.
	t.Chdir(repo)
	byCwd, _, code := runResolve(t, "resume")
	require.Equal(t, 0, code)
	require.Equal(t, byPath, byCwd)
}

// TestResumeReportsTheSameConfigurationAsTheRunForm checks the resolved
// value rather than its rendering: same run id, same state directory, and
// Resume set.
func TestResumeReportsTheSameConfigurationAsTheRunForm(t *testing.T) {
	_, repo := resumeSetup(t)

	rc := runJSON(t, "resume", repo)
	require.True(t, rc.Resume)
	require.Equal(t, domain.RunID("20260902-000000-2222"), rc.Run.ID)
	require.Equal(t, config.RunDir(repo, "20260902-000000-2222"), rc.Run.StateDir)
	require.Equal(t, domain.RunModeAutopilot, rc.Run.Mode)
}

// TestResumeWithNothingToResume gives the run form's own explanation,
// which names where it looked.
func TestResumeWithNothingToResume(t *testing.T) {
	_, repo := runTestEnv(t)
	runAddCartridge(t, "claude", "fable")

	stdout, stderr, code := runResolve(t, "resume", repo)
	require.Equal(t, 1, code)
	require.Empty(t, stdout)
	require.Equal(t, "sc: nothing to resume: no runs recorded for "+repo+" (looked in "+
		filepath.Join(config.StateDir(repo), config.RunsDirName)+")\n", stderr)
}

// TestResumeRefusesABrief: a resumed run keeps the brief it was started
// with, so -m is refused here exactly as it is on the run form.
func TestResumeRefusesABrief(t *testing.T) {
	_, repo := resumeSetup(t)

	_, stderr, code := runResolve(t, "resume", repo, "-m", "something else")
	require.Equal(t, 1, code)
	require.Equal(t, "sc: --message cannot be combined with --resume: a resumed run keeps its original brief\n", stderr)
}

// TestResumeHidesTheFlagsItImplies: --resume and --message are not in this
// command's help (one is what it is, the other is refused), but both still
// parse, so a user who types one is told why rather than "unknown flag".
func TestResumeHidesTheFlagsItImplies(t *testing.T) {
	out, err := rootRun(t, "resume", "--help")
	require.NoError(t, err)
	require.NotContains(t, out, "\n      --resume", "the command is the flag")
	require.NotContains(t, out, "\n  -m, --message", "a brief is refused on a resume")
	require.Contains(t, out, "\n      --headless")
	require.Contains(t, out, "\n      --ammo")

	_, repo := resumeSetup(t)
	stdout, stderr, code := runResolve(t, "resume", repo, "--resume")
	require.Equal(t, 0, code, stderr)
	require.Contains(t, stdout, "(resumed)")
}

// TestResumeRejectsTwoArguments keeps the argument shape of the run form.
func TestResumeRejectsTwoArguments(t *testing.T) {
	_, repo := resumeSetup(t)
	_, err := rootRun(t, "resume", repo, repo)
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// P8-31: a run directory that recorded nothing is not resumable
// ---------------------------------------------------------------------------

// TestResumePicksTheNewestRunThatRecordedSomething is P8-31's Done-when.
// The selection used to be config.LatestRunID, the greatest directory name
// whatever was in it, so a directory left behind by a kill between
// logging.OpenRunLog and the first state write was adopted and `--resume`
// fired a brand-new autopilot run under a dead run's id. Now the newest run
// with a state.json is continued, and the note says why it is not the
// newest directory.
func TestResumePicksTheNewestRunThatRecordedSomething(t *testing.T) {
	base, repo := runTestEnv(t)
	runAddCartridge(t, "claude", "fable")
	runWriteRunState(t, repo, "20260901-000000-1111")
	// The newer directory never got as far as a state file: a kill in the
	// window, or a half-pruned state directory.
	dead := config.RunDir(repo, "20260902-000000-2222")
	require.NoError(t, os.MkdirAll(dead, 0o755))

	stdout, stderr, code := runResolve(t, "resume", repo)
	require.Equal(t, 0, code, stderr)
	require.Contains(t, runNormalise(stdout, base, repo), "run        20260901-000000-1111 (resumed)\n")
	require.Contains(t, stderr, "note: run 20260902-000000-2222 recorded no state.json")
	require.Contains(t, stderr, "continuing run 20260901-000000-1111, the newest one that did")

	// The run form says exactly the same thing.
	flagOut, flagErr, code := runResolve(t, repo, "--resume")
	require.Equal(t, 0, code, flagErr)
	require.Equal(t, stdout, flagOut)
	require.Equal(t, stderr, flagErr)
}

// TestResumeWithNothingRecordedStartsANewRun is the other half of the
// Done-when: when no recorded run has a state.json there is nothing to
// resume at all, so a new run is started under a new id — and said so —
// rather than a fresh autopilot run wearing a dead run's name.
func TestResumeWithNothingRecordedStartsANewRun(t *testing.T) {
	base, repo := runTestEnv(t)
	runAddCartridge(t, "claude", "fable")
	for _, id := range []string{"20260901-000000-1111", "20260902-000000-2222"} {
		require.NoError(t, os.MkdirAll(config.RunDir(repo, id), 0o755))
	}

	stdout, stderr, code := runResolve(t, "resume", repo)
	require.Equal(t, 0, code, stderr)
	require.Contains(t, stderr, "note: run 20260902-000000-2222 recorded no state.json, and neither did any older run; starting a new run instead")
	out := runNormalise(stdout, base, repo)
	require.Contains(t, out, "run        20260902-010203-abcd (new)\n")
	require.NotContains(t, out, "(resumed)")

	rc := runJSON(t, "resume", repo)
	require.False(t, rc.Resume, "there was nothing to resume")
	require.Equal(t, domain.RunID("20260902-010203-abcd"), rc.Run.ID)
}

// ---------------------------------------------------------------------------
// --run: continue a named run rather than the newest one
// ---------------------------------------------------------------------------

// TestResumeRunContinuesTheNamedRun: a newer run that did nothing useful —
// an autopilot run stopped seconds after it started — no longer hides the
// run the user wants back.
func TestResumeRunContinuesTheNamedRun(t *testing.T) {
	base, repo := runTestEnv(t)
	runAddCartridge(t, "claude", "fable")
	runWriteRunState(t, repo, "20260901-000000-1111")
	runWriteRunState(t, repo, "20260902-000000-2222")

	stdout, stderr, code := runResolve(t, "resume", repo, "--run", "20260901-000000-1111")
	require.Equal(t, 0, code, stderr)
	require.Contains(t, runNormalise(stdout, base, repo), "run        20260901-000000-1111 (resumed)\n")

	flagOut, flagErr, code := runResolve(t, repo, "--resume", "--run", "20260901-000000-1111")
	require.Equal(t, 0, code, flagErr)
	require.Equal(t, stdout, flagOut)
}

// TestResumeRunRefusesARunWithNothingRecorded: a named run is continued or
// refused, never swapped for a new run under that name.
func TestResumeRunRefusesARunWithNothingRecorded(t *testing.T) {
	_, repo := runTestEnv(t)
	runAddCartridge(t, "claude", "fable")
	runWriteRunState(t, repo, "20260901-000000-1111")
	require.NoError(t, os.MkdirAll(config.RunDir(repo, "20260902-000000-2222"), 0o755))

	for _, id := range []string{"20260902-000000-2222", "20260903-000000-3333"} {
		_, stderr, code := runResolve(t, "resume", repo, "--run", id)
		require.Equal(t, 1, code)
		require.Equal(t, "sc: nothing to resume: run "+id+" of "+repo+" has no state.json (sc status lists the runs)\n", stderr)
	}
}

// TestRunFlagNeedsResume: --run names a run to continue, so on the run form
// it means nothing without --resume.
func TestRunFlagNeedsResume(t *testing.T) {
	_, repo := resumeSetup(t)
	_, stderr, code := runResolve(t, repo, "--run", "20260901-000000-1111")
	require.Equal(t, 1, code)
	require.Equal(t, "sc: --run names a run to continue, so it needs --resume (or sc resume)\n", stderr)
}
