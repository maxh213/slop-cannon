package cli

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/logging"
)

// loggingWiringRestoreDefault puts the process-wide slog default back when
// the test ends, since rootPreRun replaces it.
func loggingWiringRestoreDefault(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
}

// loggingWiringRun executes `sc <args>` with stdout and stderr captured
// separately and returns the stderr text, where log lines land.
func loggingWiringRun(t *testing.T, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	root := NewRootCommand(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(args)
	require.NoError(t, root.Execute())
	return stderr.String()
}

func TestLoggingWiringVerboseEmitsDebug(t *testing.T) {
	loggingWiringRestoreDefault(t)

	stderr := loggingWiringRun(t, "--verbose", "version")
	require.Contains(t, stderr, "level=DEBUG")
	require.Contains(t, stderr, `msg="verbose logging enabled"`)
	require.Contains(t, stderr, `command="sc version"`)

	// The installed default logger keeps writing debug lines to the same
	// stderr after the command returns, so later packages inherit the level.
	var out, err2 bytes.Buffer
	root := NewRootCommand(&out)
	root.SetErr(&err2)
	root.SetArgs([]string{"-v", "version"})
	require.NoError(t, root.Execute())
	slog.Debug("probe-debug")
	slog.Info("probe-info")
	require.Contains(t, err2.String(), "msg=probe-debug")
	require.Contains(t, err2.String(), "msg=probe-info")
	require.NotContains(t, out.String(), "probe-", "log lines must not reach stdout")
}

func TestLoggingWiringDefaultLevelIsInfo(t *testing.T) {
	loggingWiringRestoreDefault(t)

	var out, stderr bytes.Buffer
	root := NewRootCommand(&out)
	root.SetErr(&stderr)
	root.SetArgs([]string{"version"})
	require.NoError(t, root.Execute())
	require.Empty(t, stderr.String(), "no log lines without --verbose")

	slog.Debug("probe-debug")
	slog.Info("probe-info")
	require.NotContains(t, stderr.String(), "probe-debug")
	require.Contains(t, stderr.String(), "level=INFO msg=probe-info")
}

func TestLoggingWiringHelpIsUntouched(t *testing.T) {
	loggingWiringRestoreDefault(t)
	var out bytes.Buffer
	root := NewRootCommand(&out)
	root.SetArgs([]string{"--verbose", "--help"})
	require.NoError(t, root.Execute())
	require.NotContains(t, out.String(), "level=DEBUG", "help output must stay golden-stable under --verbose")
}

func TestLoggingWiringRedactsSecrets(t *testing.T) {
	loggingWiringRestoreDefault(t)
	var buf bytes.Buffer
	root := NewRootCommand(&buf)
	root.SetArgs([]string{"-v", "version"})
	require.NoError(t, root.Execute())
	slog.Info("token", "key", "sk-abcdefghijklmnopqrstuvwxyz0123456789")
	require.NotContains(t, buf.String(), "sk-abcdefghijklmnopqrstuvwxyz0123456789")
	require.Contains(t, buf.String(), "sk-[REDACTED]")
}

func TestLoggingWiringSubcommandHookDoesNotShadowRoot(t *testing.T) {
	loggingWiringRestoreDefault(t)
	var out, stderr bytes.Buffer
	root := NewRootCommand(&out)
	root.SetErr(&stderr)
	ran := false
	root.AddCommand(&cobra.Command{
		Use:               "wiring-probe",
		Hidden:            true,
		PersistentPreRunE: func(*cobra.Command, []string) error { ran = true; return nil },
		RunE: func(*cobra.Command, []string) error {
			slog.Debug("from-subcommand")
			return nil
		},
	})
	root.SetArgs([]string{"--verbose", "wiring-probe"})
	require.NoError(t, root.Execute())
	require.True(t, ran, "subcommand hook must still run")
	require.Contains(t, stderr.String(), "msg=from-subcommand", "root hook must run despite the subcommand hook")
}

func TestLoggingWiringLogOptions(t *testing.T) {
	require.Equal(t, slog.LevelDebug, logging.Level(LogOptions(true).Verbose))
	require.Equal(t, slog.LevelInfo, logging.Level(LogOptions(false).Verbose))
	require.Nil(t, LogOptions(true).Level, "LogOptions leaves Level to Verbose")
}

// TestLoggingWiringRunLogLandsInRunDir pins the contract the run command
// (P8-01) relies on: OpenRunLog with config.RunDir writes sc.log under the
// D-23 run directory, at the level chosen by --verbose.
func TestLoggingWiringRunLogLandsInRunDir(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()
	const runID = "run-wiring-1"

	runDir := config.RunDir(repo, runID)
	want := filepath.Join(stateHome, config.AppName, config.RepoID(repo), config.RunsDirName, runID)
	require.Equal(t, want, runDir)

	rl, err := logging.OpenRunLog(runDir, domain.RunID(runID), LogOptions(true))
	require.NoError(t, err)
	rl.Logger().Debug("wiring-debug")
	rl.Logger().Info("wiring-info")
	require.NoError(t, rl.Close())

	data, err := os.ReadFile(filepath.Join(want, logging.RunLogName))
	require.NoError(t, err)
	require.Contains(t, string(data), `"msg":"wiring-debug"`)
	require.Contains(t, string(data), `"msg":"wiring-info"`)
	require.Contains(t, string(data), `"run":"`+runID+`"`)
	require.DirExists(t, filepath.Join(want, logging.AgentsDirName))

	// Nothing of ours lands in the target repo (D-23).
	entries, err := os.ReadDir(repo)
	require.NoError(t, err)
	require.Empty(t, entries)

	// Without --verbose the run file drops debug lines.
	quiet, err := logging.OpenRunLog(config.RunDir(repo, "run-wiring-2"), "run-wiring-2", LogOptions(false))
	require.NoError(t, err)
	quiet.Logger().Debug("hidden")
	require.NoError(t, quiet.Close())
	data, err = os.ReadFile(filepath.Join(config.RunDir(repo, "run-wiring-2"), logging.RunLogName))
	require.NoError(t, err)
	require.NotContains(t, string(data), "hidden")
}
