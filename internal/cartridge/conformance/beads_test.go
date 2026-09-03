package conformance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/testutil"
)

// conformanceFakeBDScript emulates the bd 1.2.2 behaviour the beads check
// relies on: `init` creates .beads/, `version --json` prints an envelope,
// `update` is refused in read-only mode and `ready --json` lists nothing.
// The shim prepends --readonly, so the subcommand is found by scanning.
const conformanceFakeBDScript = `#!/bin/sh
for a in "$@"; do
  case "$a" in
    init) mkdir -p .beads; echo "Initialized beads (fake)"; exit 0 ;;
    version) printf '{"schema_version":1,"data":{"version":"1.2.2","build":"fake"}}\n'; exit 0 ;;
    update) echo "Error: operation 'update' is not allowed in read-only mode" >&2; exit 1 ;;
    ready) printf '{"schema_version":1,"data":[]}\n'; exit 0 ;;
  esac
done
printf '%s\n' "$@"
`

// conformanceFakeBD writes a fake bd into a fresh directory and returns its
// path for Options.BD. Unlike testutil.FakeBin it leaves PATH alone, so
// parallel tests can use it.
func conformanceFakeBD(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake bd needs a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "bd")
	require.NoError(t, os.WriteFile(path, []byte(conformanceFakeBDScript), 0o755))
	return path
}

func TestBeadsPrompt(t *testing.T) {
	t.Parallel()
	p := BeadsPrompt()
	assert.True(t, strings.HasPrefix(p, beadsMarker))
	assert.Contains(t, p, "bd update sc-shim-probe -s open")
	assert.Contains(t, p, "bd ready --json")
	assert.False(t, mock.Match{PromptContains: hangMarker}.Matches(cartridge.RunSpec{Prompt: p}), "the hang rule must not catch the beads prompt")
}

func TestBeadsCheck_PassesThroughTheShim(t *testing.T) {
	t.Parallel()
	const token = "sc-beadsok"
	a, err := NewMock("", token)
	require.NoError(t, err)
	opts := conformanceFastOptions(t, token)
	opts.Workdir = filepath.Join(t.TempDir(), "keep")
	opts.Checks = []string{CheckDetect, CheckBeads}
	rep, err := Run(context.Background(), a, opts)
	require.NoError(t, err)
	require.True(t, rep.Passed(), rep.Table())
	res := conformanceResult(t, rep, CheckBeads)
	assert.Equal(t, StatusPass, res.Status, rep.Table())
	assert.Contains(t, res.Detail, "bd update refused through the shim")

	// The session ran with the shim directory first on PATH, in a workdir
	// that is a beads project, under read_exec.
	call, ok := a.LastCall()
	require.True(t, ok)
	assert.Equal(t, CheckBeads, call.Spec.Labels.Get("check"))
	assert.Equal(t, cartridge.ToolReadExec, call.Spec.ToolPolicy)
	shimDir := filepath.Join(opts.Workdir, CheckBeads+"-bin")
	assert.True(t, strings.HasPrefix(call.Spec.Env["PATH"], shimDir+string(os.PathListSeparator)), "PATH %q does not start with the shim dir", call.Spec.Env["PATH"])
	assert.True(t, beads.IsShim(filepath.Join(shimDir, "bd")))
	assert.DirExists(t, filepath.Join(call.Spec.Workdir, ".beads"))
	assert.DirExists(t, filepath.Join(call.Spec.Workdir, ".git"))
}

func TestBeadsCheck_FailsWhenBDAcceptsWrites(t *testing.T) {
	t.Parallel()
	const token = "sc-beadsaccept"
	a, err := NewMock("", token)
	require.NoError(t, err)
	opts := conformanceFastOptions(t, token)
	// A bd that never refuses: the shim chain itself fails verification.
	bd := filepath.Join(t.TempDir(), "bd")
	script := strings.Replace(conformanceFakeBDScript, `update) echo "Error: operation 'update' is not allowed in read-only mode" >&2; exit 1 ;;`, `update) printf '{"schema_version":1,"data":[]}\n'; exit 0 ;;`, 1)
	require.NoError(t, os.WriteFile(bd, []byte(script), 0o755))
	opts.BD = bd
	opts.Checks = []string{CheckDetect, CheckBeads}
	rep, err := Run(context.Background(), a, opts)
	require.NoError(t, err)
	res := conformanceResult(t, rep, CheckBeads)
	assert.Equal(t, StatusFail, res.Status)
	require.Error(t, res.Err)
	assert.Contains(t, res.Err.Error(), "was not refused")
	assert.Empty(t, a.Calls(), "no session runs when the shim chain does not hold")
}

func TestBeadsCheck_FailsWhenTheSessionHidesTheRefusal(t *testing.T) {
	t.Parallel()
	const token = "sc-beadssilent"
	s := MockScenario(token)
	// Replace the beads rule with one that never runs bd.
	for _, r := range s.Rules {
		if r.Name == "beads" {
			r.Steps = nil
			r.Say("I ran both commands and everything worked.")
		}
	}
	a, err := mock.NewAdapter("", s)
	require.NoError(t, err)
	opts := conformanceFastOptions(t, token)
	opts.Checks = []string{CheckDetect, CheckBeads}
	rep, err := Run(context.Background(), a, opts)
	require.NoError(t, err)
	res := conformanceResult(t, rep, CheckBeads)
	assert.Equal(t, StatusFail, res.Status)
	require.Error(t, res.Err)
	assert.Contains(t, res.Err.Error(), "never reported the read-only refusal")
	assert.Contains(t, res.Err.Error(), beads.ReadOnlyMessage("update"))
	assert.Contains(t, rep.Table(), "1 failed")
}

func TestBeadsCheck_SkipsWithoutBD(t *testing.T) {
	// Not parallel: it rewrites PATH so no bd (real or fake) is reachable.
	dir := t.TempDir()
	for _, name := range []string{"git", "sh"} {
		real, err := testutil.RealBinary(name)
		if err != nil {
			t.Skipf("%s is not installed", name)
		}
		require.NoError(t, os.Symlink(real, filepath.Join(dir, name)))
	}
	t.Setenv("PATH", dir)
	const token = "sc-beadsnobd"
	a, err := NewMock("", token)
	require.NoError(t, err)
	opts := Options{Token: token, Checks: []string{CheckDetect, CheckBeads}}
	rep, err := Run(context.Background(), a, opts)
	require.NoError(t, err)
	assert.True(t, rep.Passed(), rep.Table())
	res := conformanceResult(t, rep, CheckBeads)
	assert.Equal(t, StatusSkip, res.Status)
	assert.Contains(t, res.Detail, "bd is not installed")
	assert.NoError(t, res.Err)
	assert.Empty(t, a.Calls())
	pass, fail, skip := rep.Counts()
	assert.Equal(t, []int{1, 0, 7}, []int{pass, fail, skip})
	assert.Contains(t, rep.Table(), "SKIP")
}

// TestBeadsCheck_SkipsWithOldBD covers the P3-09 review's version gate: a bd
// below beads.MinVersion has a different CLI (1.0.5 rejects
// `bd init --init-if-missing`), so the check must skip with an install hint
// instead of failing for an environment reason.
func TestBeadsCheck_SkipsWithOldBD(t *testing.T) {
	t.Parallel()
	const token = "sc-beadsoldbd"
	a, err := NewMock("", token)
	require.NoError(t, err)
	opts := conformanceFastOptions(t, token)
	bd := filepath.Join(t.TempDir(), "bd")
	script := strings.Replace(conformanceFakeBDScript, `"version":"1.2.2"`, `"version":"1.0.5"`, 1)
	require.NotEqual(t, conformanceFakeBDScript, script)
	require.NoError(t, os.WriteFile(bd, []byte(script), 0o755))
	opts.BD = bd
	opts.Checks = []string{CheckDetect, CheckBeads}
	rep, err := Run(context.Background(), a, opts)
	require.NoError(t, err)
	assert.True(t, rep.Passed(), rep.Table())
	res := conformanceResult(t, rep, CheckBeads)
	assert.Equal(t, StatusSkip, res.Status)
	assert.Contains(t, res.Detail, "older than "+beads.MinVersion.String())
	assert.Contains(t, res.Detail, "install beads")
	assert.NoError(t, res.Err)
	assert.Empty(t, a.Calls(), "no session runs for an unusable bd")
}

func TestRetryTextBusy(t *testing.T) {
	t.Parallel()
	t.Run("retries ETXTBSY until it clears", func(t *testing.T) {
		t.Parallel()
		calls := 0
		err := retryTextBusy(context.Background(), func() error {
			calls++
			if calls < 3 {
				return &os.PathError{Op: "fork/exec", Path: "/tmp/bd", Err: syscall.ETXTBSY}
			}
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, 3, calls)
	})
	t.Run("retries the shell's complaint", func(t *testing.T) {
		t.Parallel()
		calls := 0
		err := retryTextBusy(context.Background(), func() error {
			calls++
			if calls == 1 {
				return errors.New("proc: /tmp/bd version: exit status 126: sh: 1: /tmp/real/bd: Text file busy")
			}
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, 2, calls)
	})
	t.Run("returns other errors at once", func(t *testing.T) {
		t.Parallel()
		calls := 0
		want := errors.New("boom")
		err := retryTextBusy(context.Background(), func() error { calls++; return want })
		assert.Same(t, want, err)
		assert.Equal(t, 1, calls)
	})
	t.Run("gives up after the attempt budget", func(t *testing.T) {
		t.Parallel()
		calls := 0
		err := retryTextBusy(context.Background(), func() error { calls++; return syscall.ETXTBSY })
		assert.ErrorIs(t, err, syscall.ETXTBSY)
		assert.Equal(t, textBusyAttempts, calls)
	})
	t.Run("stops when the context ends", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		err := retryTextBusy(ctx, func() error { calls++; return syscall.ETXTBSY })
		assert.ErrorIs(t, err, syscall.ETXTBSY)
		assert.Equal(t, 1, calls)
	})
	assert.False(t, isTextBusy(nil))
}
