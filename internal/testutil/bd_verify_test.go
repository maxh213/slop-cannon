package testutil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/beads"
)

// P8-27 — what a stand-in bd has to implement before `sc` will drive it.
//
// The requirement lives in beads.Shim.Verify, which runs before every
// agent and reaches past the read-only shim to the binary underneath. It
// is not exercised by any command a run makes, so it was found only by
// running the demo. These tests are that requirement written down where a
// stand-in is built, with teeth: [BDVerifyPrologue] is checked against the
// real self-check, and a stand-in without it is checked to fail — so a new
// probe in internal/beads breaks this test and names itself, instead of
// breaking the demo.

// bdvStandIn installs a stand-in `bd` with prologue in front of a dispatch
// that answers reads with an empty envelope and refuses nothing else, and
// returns its path. FakeBin also puts it first on PATH, which is where
// beads.RealBD looks.
func bdvStandIn(t testing.TB, prologue string) string {
	t.Helper()
	sh := FakeBin(t, "bd", prologue+`
echo '{"schema_version":1,"data":[]}'
exit 0
`)
	return sh.Path
}

// bdvInstall writes the production read-only shim over real and returns
// it, together with the directory Verify is run from.
func bdvInstall(t testing.TB, real string) (beads.Shim, string) {
	t.Helper()
	dir := t.TempDir()
	wt := filepath.Join(t.TempDir(), "worktree")
	require.NoError(t, os.MkdirAll(wt, 0o755))
	s, err := beads.InstallForWorktree(context.Background(), wt,
		beads.ShimOptions{Dir: dir, Real: real, SkipVerify: true})
	require.NoError(t, err)
	return s, wt
}

func TestBDVerifyPrologueSatisfiesTheReadOnlyShimSelfCheck(t *testing.T) {
	real := bdvStandIn(t, BDVerifyPrologue(""))
	shim, wt := bdvInstall(t, real)
	require.NoError(t, shim.Verify(context.Background(), wt),
		"a stand-in carrying BDVerifyPrologue must get past beads.Shim.Verify")
}

func TestBDVerifyPrologueAnswersTheTwoProbesTheRunNeverMakes(t *testing.T) {
	real := bdvStandIn(t, BDVerifyPrologue("9.9.9"))

	out, err := bdvRun(t, real, "version", "--json")
	require.NoError(t, err, "`bd version --json` must succeed: %s", out)
	assert.Contains(t, out, `"version":"9.9.9"`, "and print the version it was given")

	out, err = bdvRun(t, real, "ping", "--json")
	require.NoError(t, err, "`bd ping --json` is D-13's health check: %s", out)
	assert.Contains(t, out, `"status":"ok"`)

	// D-21's second line of defence: the binary under the shim refuses the
	// write itself, in bd's own words.
	out, err = bdvRun(t, real, beads.ReadOnlyFlag, "update", "sc-verify-probe", "-s", "open")
	require.Error(t, err, "`bd --readonly update` must be refused: %s", out)
	assert.Contains(t, out, beads.ReadOnlyMessage("update"))

	// A read is still a read while --readonly is in force, wherever the
	// global flags sit on the command line.
	out, err = bdvRun(t, real, "-C", t.TempDir(), beads.ReadOnlyFlag, "ready", "--json", "-n", "0")
	require.NoError(t, err, "a read must not be refused: %s", out)
}

// TestAStandInWithoutThePrologueIsRefused is the other half: without the
// prologue the self-check fails, which is why the requirement has to be
// written down at all.
func TestAStandInWithoutThePrologueIsRefused(t *testing.T) {
	t.Run("no version", func(t *testing.T) {
		real := bdvStandIn(t, "")
		shim, wt := bdvInstall(t, real)
		err := shim.Verify(context.Background(), wt)
		require.Error(t, err, "a stand-in that prints no version must be refused")
		assert.Contains(t, err.Error(), "printed no version")
	})

	t.Run("no --readonly enforcement", func(t *testing.T) {
		// Everything the prologue does except refuse a write.
		version := strings.SplitN(BDVerifyPrologue(""), `if [ "$_sc_ro" = 1 ]`, 2)[0]
		real := bdvStandIn(t, version)
		shim, wt := bdvInstall(t, real)
		err := shim.Verify(context.Background(), wt)
		require.Error(t, err, "a stand-in that lets a --readonly write through must be refused")
		assert.Contains(t, err.Error(), "does not enforce "+beads.ReadOnlyFlag)
	})
}

// bdvRun runs the stand-in directly and returns its combined output.
func bdvRun(t testing.TB, path string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(path, args...) //nolint:gosec // a shim this test just installed
	out, err := cmd.CombinedOutput()
	return string(out), err
}
