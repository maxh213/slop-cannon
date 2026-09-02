package beads

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/proc"
	"slop-cannon/internal/testutil"
)

// detectVersionShim installs a bd that answers `bd version --json` with the
// given envelope body and everything else with a ping sample.
func detectVersionShim(t *testing.T, versionJSON string) *testutil.Shim {
	t.Helper()
	envIsolate(t)
	return testutil.FakeBin(t, "bd", fmt.Sprintf(`case "$1" in
  version) printf '%%s\n' '%s'; exit 0 ;;
esac
cat %q`, versionJSON, filepath.Join(envSamplesDir(t), "ping.json")))
}

func TestDetect(t *testing.T) {
	envIsolate(t)
	bd := testutil.FakeBD(t, envSamplesDir(t))
	info, err := Detect(context.Background())
	require.NoError(t, err)
	assert.Equal(t, bd.Path, info.Path)
	assert.Equal(t, proc.V(1, 2, 2), info.Version)
	assert.Equal(t, "dev", info.Build)
	assert.Empty(t, info.Branch, "the captured sample carries no branch")
	assert.Equal(t, "1.2.2 (build dev, "+bd.Path+")", info.String())
	assert.Equal(t, [][]string{{"version", "--json"}}, bd.ArgLists(t))
	assert.Empty(t, testutil.MissingSamples(t, bd))
}

func TestDetectReportsBranchAndNoBuild(t *testing.T) {
	bd := detectVersionShim(t, `{"data":{"branch":"main","version":"1.3.0"},"schema_version":1}`)
	info, err := Detect(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "main", info.Branch)
	assert.Empty(t, info.Build)
	assert.Equal(t, "1.3.0 ("+bd.Path+")", info.String())
}

func TestDetectAgainstTestutilSamples(t *testing.T) {
	// The shared samples doctor uses must keep working through beads.Detect.
	envIsolate(t)
	testutil.FakeBD(t, testutil.BDSamplesDir(t))
	info, err := Detect(context.Background())
	require.NoError(t, err)
	assert.True(t, info.Version.AtLeast(MinVersion))
}

func TestDetectFailures(t *testing.T) {
	t.Run("not on PATH", func(t *testing.T) {
		envIsolate(t)
		t.Setenv("PATH", t.TempDir())
		_, err := Detect(context.Background())
		var ce *CheckError
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, ReasonNotFound, ce.Reason)
		assert.Equal(t, "not found on PATH", err.Error())
		assert.Equal(t, InstallHint, ce.Hint())
		assert.True(t, proc.IsNotFound(err), "the underlying not-found error is preserved")
	})
	t.Run("version command fails", func(t *testing.T) {
		envIsolate(t)
		bd := testutil.FakeBin(t, "bd", `echo "unknown flag: --json" >&2; exit 2`)
		info, err := Detect(context.Background())
		var ce *CheckError
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, ReasonFailed, ce.Reason)
		assert.Equal(t, "bd version failed: exit status 2: unknown flag: --json", err.Error())
		assert.Equal(t, bd.Path, info.Path, "the path is still reported")
		assert.Equal(t, InstallHint, ce.Hint())
	})
	t.Run("text output", func(t *testing.T) {
		detectVersionShim(t, "bd version 1.2.2 (dev)")
		_, err := Detect(context.Background())
		var ce *CheckError
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, ReasonBadOutput, ce.Reason)
		assert.Contains(t, err.Error(), "bd version: no JSON object in output")
	})
	t.Run("unparseable version", func(t *testing.T) {
		bd := detectVersionShim(t, `{"data":{"version":"unknown"},"schema_version":1}`)
		_, err := Detect(context.Background())
		var ce *CheckError
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, ReasonBadOutput, ce.Reason)
		assert.Equal(t, fmt.Sprintf(`bd version: reported version "unknown" (%s)`, bd.Path), err.Error())
	})
	t.Run("too old", func(t *testing.T) {
		bd := detectVersionShim(t, `{"data":{"version":"1.1.0","build":"dev"},"schema_version":1}`)
		info, err := Detect(context.Background())
		var ce *CheckError
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, ReasonTooOld, ce.Reason)
		assert.Equal(t, proc.V(1, 1, 0), ce.Version)
		assert.Equal(t, proc.V(1, 1, 0), info.Version, "the info is still filled in")
		assert.Equal(t, "1.1.0 is older than 1.2.2 ("+bd.Path+")", err.Error())
		assert.Equal(t, InstallHint, ce.Hint())
	})
	t.Run("newer is fine and preamble is skipped", func(t *testing.T) {
		envIsolate(t)
		testutil.FakeBin(t, "bd", `printf '%s\n' "Thanks for using bd! Quick heads-up: bd shares anonymous usage metrics {see bd metrics}" '{"data":{"build":"abc","version":"2.0.0"},"schema_version":1}'`)
		info, err := Detect(context.Background())
		require.NoError(t, err)
		assert.Equal(t, proc.V(2, 0, 0), info.Version)
		assert.Equal(t, "abc", info.Build)
	})
	t.Run("timeout", func(t *testing.T) {
		envIsolate(t)
		testutil.FakeBin(t, "bd", `sleep 30`)
		ctx, cancel := context.WithTimeout(context.Background(), 200_000_000)
		defer cancel()
		_, err := Detect(ctx)
		var ce *CheckError
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, ReasonFailed, ce.Reason)
		assert.True(t, errors.Is(err, context.DeadlineExceeded), "got %v", err)
	})
}

func TestPing(t *testing.T) {
	envIsolate(t)
	bd := testutil.FakeBD(t, envSamplesDir(t))
	repo := t.TempDir()
	ping, err := Ping(context.Background(), repo)
	require.NoError(t, err)
	var want PingResult
	require.NoError(t, Decode(envSample(t, "ping.json"), &want))
	assert.Equal(t, want, ping)
	assert.Equal(t, "ok", ping.Status)
	assert.Positive(t, ping.TotalMS)
	assert.Equal(t, [][]string{{"-C", repo, "ping", "--json"}}, bd.ArgLists(t))
}

func TestPingFailures(t *testing.T) {
	t.Run("no database", func(t *testing.T) {
		envIsolate(t)
		dir := testutil.StageBDSamples(t, nil)
		// Replace the ping sample with the captured "no beads project" refusal.
		require.NoError(t, os.Remove(filepath.Join(dir, "ping.json")))
		for _, ext := range []string{".stderr", ".exit"} {
			data, err := os.ReadFile(filepath.Join(envSamplesDir(t), "ping-no-db"+ext))
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "ping"+ext), data, 0o644))
		}
		testutil.FakeBD(t, dir)
		_, err := Ping(context.Background(), "/tmp/fixture-repo")
		var ce *CheckError
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, ReasonFailed, ce.Reason)
		assert.Equal(t, `bd ping failed: exit status 1: Error: cannot use -C directory "/tmp/fixture-repo": no beads project found`, err.Error())
		assert.Equal(t, PingHint, ce.Hint())
		var exit *proc.ExitError
		assert.True(t, errors.As(err, &exit))
	})
	t.Run("degraded", func(t *testing.T) {
		envIsolate(t)
		testutil.FakeBin(t, "bd", `echo '{"data":{"status":"degraded","total_ms":4},"schema_version":1}'`)
		ping, err := Ping(context.Background(), t.TempDir())
		var ce *CheckError
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, ReasonUnhealthy, ce.Reason)
		assert.Equal(t, "degraded", ping.Status, "the answer is returned alongside the error")
		assert.Equal(t, `bd ping reported status "degraded"`, err.Error())
		assert.Equal(t, PingHint, ce.Hint())
	})
	t.Run("text output", func(t *testing.T) {
		envIsolate(t)
		testutil.FakeBin(t, "bd", `echo "pong"`)
		_, err := Ping(context.Background(), t.TempDir())
		var ce *CheckError
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, ReasonBadOutput, ce.Reason)
		assert.Contains(t, err.Error(), "bd ping: no JSON object in output")
	})
	t.Run("not installed", func(t *testing.T) {
		envIsolate(t)
		t.Setenv("PATH", t.TempDir())
		_, err := Ping(context.Background(), t.TempDir())
		var ce *CheckError
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, ReasonNotFound, ce.Reason)
	})
}

func TestCheckErrorFallbackText(t *testing.T) {
	t.Parallel()
	e := &CheckError{Command: "bd ping", Reason: ReasonBadOutput}
	assert.Equal(t, "bd ping: unusable output", e.Error())
	assert.Nil(t, e.Unwrap())
	assert.Contains(t, InstallHint, "install beads 1.2.2 or newer")
}
