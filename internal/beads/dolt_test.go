package beads

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/testutil"
)

// doltSampleBeadsDir is the beads_dir the captured context samples name.
const doltSampleBeadsDir = "/tmp/fixture-repo/.beads"

// doltStage stages the captured samples with overrides (see clientStage)
// and rewrites the context sample's beads_dir to beadsDir so CheckDolt
// reads metadata.json from a directory the test controls.
func doltStage(t *testing.T, overrides map[string]string, beadsDir string) string {
	t.Helper()
	dir := clientStage(t, overrides)
	path := filepath.Join(dir, "context.json")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	data = bytes.ReplaceAll(data, []byte(doltSampleBeadsDir), []byte(beadsDir))
	require.NoError(t, os.WriteFile(path, data, 0o644))
	return dir
}

// doltBeadsDir creates a .beads directory holding the captured
// metadata.json, optionally with its dolt_mode rewritten.
func doltBeadsDir(t *testing.T, mode string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".beads")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	data := envSample(t, "project-metadata.json")
	if mode != "" {
		data = strings.Replace(data, `"dolt_mode": "embedded"`, `"dolt_mode": "`+mode+`"`, 1)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, ProjectMetadataFile), []byte(data), 0o644))
	return dir
}

// doltClient installs the shim over the staged samples and returns a
// client whose Warn lines land in the returned buffer.
func doltClient(t *testing.T, samples string) (*Client, *testutil.Shim, *bytes.Buffer) {
	t.Helper()
	envIsolate(t)
	bd := testutil.FakeBD(t, samples)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	c := New("/tmp/fixture-repo", Options{Actor: "sc:test", Timeout: 20 * time.Second, Logger: logger})
	return c, bd, &logs
}

func TestDoltContext(t *testing.T) {
	c, bd := clientFake(t, nil)
	bc, err := c.Context(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "context", "--json"}, clientLastArgs(t, bd))
	assert.Equal(t, doltSampleBeadsDir, bc.BeadsDir)
	assert.Equal(t, "/tmp/fixture-repo", bc.RepoRoot)
	assert.Equal(t, "/tmp/fixture-repo", bc.CwdRepoRoot)
	assert.False(t, bc.IsRedirected)
	assert.False(t, bc.IsWorktree)
	assert.Equal(t, "dolt", bc.Backend)
	assert.Equal(t, DoltModeEmbedded, bc.DoltMode)
	assert.True(t, bc.Embedded())
	assert.Empty(t, bc.ServerHost)
	assert.Equal(t, "fx", bc.Database)
	assert.Equal(t, "40c805e3-f911-4857-a6ad-435872e0e005", bc.ProjectID)
	assert.Equal(t, "maintainer", bc.Role)
	assert.Equal(t, "1.2.2", bc.BDVersion)
	assert.Empty(t, testutil.MissingSamples(t, bd))
}

func TestDoltContextServerMode(t *testing.T) {
	c, _ := clientFake(t, map[string]string{"context": "context-server"})
	bc, err := c.Context(context.Background())
	require.NoError(t, err)
	assert.Equal(t, DoltModeServer, bc.DoltMode)
	assert.False(t, bc.Embedded())
	assert.Equal(t, "127.0.0.1", bc.ServerHost)
	assert.True(t, bc.IsRedirected)
	assert.Equal(t, "contributor", bc.Role)
}

func TestDoltReadContext(t *testing.T) {
	envIsolate(t)
	bd := testutil.FakeBD(t, envSamplesDir(t))
	bc, err := ReadContext(context.Background(), "/tmp/fixture-repo")
	require.NoError(t, err)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "context", "--json"}, clientLastArgs(t, bd))
	assert.True(t, bc.Embedded())
	assert.Equal(t, "fx", bc.Database)
}

func TestDoltReadContextNoProject(t *testing.T) {
	envIsolate(t)
	testutil.FakeBD(t, clientStage(t, map[string]string{"context": "error-context-no-project"}))
	_, err := ReadContext(context.Background(), "/tmp/plain")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoProject)
	var bdErr *Error
	require.ErrorAs(t, err, &bdErr)
	assert.Equal(t, "context", bdErr.Command)
	assert.Equal(t, 1, bdErr.ExitCode)
	assert.Contains(t, err.Error(), "no beads project found")
}

func TestDoltReadContextNotInstalled(t *testing.T) {
	envIsolate(t)
	t.Setenv("PATH", t.TempDir())
	_, err := ReadContext(context.Background(), "/tmp/fixture-repo")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found on PATH")
}

func TestDoltReadProjectMetadata(t *testing.T) {
	t.Run("embedded", func(t *testing.T) {
		dir := doltBeadsDir(t, "")
		m, err := ReadProjectMetadata(dir)
		require.NoError(t, err)
		assert.Equal(t, ProjectMetadata{
			Database: "dolt", Backend: "dolt", DoltMode: DoltModeEmbedded, DoltDatabase: "fx",
			ProjectID: "40c805e3-f911-4857-a6ad-435872e0e005",
		}, m)
		assert.True(t, m.Embedded())
	})
	t.Run("server", func(t *testing.T) {
		m, err := ReadProjectMetadata(doltBeadsDir(t, DoltModeServer))
		require.NoError(t, err)
		assert.False(t, m.Embedded())
	})
	t.Run("missing", func(t *testing.T) {
		_, err := ReadProjectMetadata(filepath.Join(t.TempDir(), ".beads"))
		require.Error(t, err)
		assert.ErrorIs(t, err, os.ErrNotExist)
		assert.Contains(t, err.Error(), ProjectMetadataFile)
	})
	t.Run("malformed", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, ProjectMetadataFile), []byte("{nope"), 0o644))
		_, err := ReadProjectMetadata(dir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "decoding")
	})
}

func TestDoltHistory(t *testing.T) {
	c, bd := clientFake(t, nil)
	ctx := context.Background()

	entries, err := c.History(ctx, "fx-fbq", 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "history", "fx-fbq", "--json"}, clientLastArgs(t, bd))
	require.Len(t, entries, 4)
	newest := entries[0]
	assert.Equal(t, "66iccmnvssp49u81n94q1im5cns0d5i9", newest.Commit)
	assert.Equal(t, "root", newest.Committer)
	assert.Equal(t, 2026, newest.Date.Year())
	assert.False(t, newest.Date.IsZero())
	assert.Equal(t, "fx-fbq", string(newest.Issue.ID))
	assert.Equal(t, "Add Median", newest.Issue.Title)
	assert.Equal(t, "in_progress", newest.Issue.Status)
	assert.Equal(t, "task", newest.Issue.Type)
	assert.Equal(t, "sc", newest.Issue.CreatedBy)
	for i := 1; i < len(entries); i++ {
		assert.False(t, entries[i].Date.After(entries[i-1].Date), "entry %d is newer than entry %d", i, i-1)
		assert.NotEqual(t, entries[i].Commit, entries[i-1].Commit)
	}

	_, err = c.History(ctx, "fx-fbq", 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "history", "fx-fbq", "--json", "--limit", "2"}, clientLastArgs(t, bd))

	_, err = c.History(ctx, "  ", 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalid)
	assert.Empty(t, testutil.MissingSamples(t, bd))
}

func TestDoltHistoryUnknownID(t *testing.T) {
	c, _ := clientFake(t, map[string]string{"history": "history-missing"})
	_, err := c.History(context.Background(), "nope-1", 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
	var bdErr *Error
	require.ErrorAs(t, err, &bdErr)
	assert.Equal(t, "history", bdErr.Command)
	assert.Equal(t, "bd history: bead not found: no history for nope-1 (unknown id, or dolt.auto-commit is not \"on\")", err.Error())
}

func TestDoltHistoryDecodesCapturedSample(t *testing.T) {
	var entries []HistoryEntry
	require.NoError(t, Decode(envSample(t, "history.json"), &entries))
	require.Len(t, entries, 4)
	assert.Equal(t, "open", entries[3].Issue.Status, "the oldest entry is the creation commit")
	assert.Equal(t, 2, int(entries[3].Issue.Priority))
}

func TestDoltCheck(t *testing.T) {
	ctx := context.Background()

	t.Run("embedded with auto-commit on has no warnings", func(t *testing.T) {
		beads := doltBeadsDir(t, "")
		c, bd, logs := doltClient(t, doltStage(t, map[string]string{"config-get": "config-get-auto-commit"}, beads))
		status, err := c.CheckDolt(ctx)
		require.NoError(t, err)
		assert.Empty(t, status.Warnings)
		assert.True(t, status.Embedded())
		assert.Equal(t, DoltModeEmbedded, status.Context.DoltMode)
		assert.Equal(t, beads, status.Context.BeadsDir)
		assert.Equal(t, DoltModeEmbedded, status.Metadata.DoltMode)
		assert.Equal(t, "fx", status.Metadata.DoltDatabase)
		assert.Equal(t, AutoCommitOn, status.AutoCommit)
		assert.Empty(t, logs.String())
		calls := bd.ArgLists(t)
		require.Len(t, calls, 2, "context and config get, nothing else")
		assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "context", "--json"}, calls[0])
		assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "config", "get", AutoCommitKey, "--json"}, calls[1])
		assert.Empty(t, testutil.MissingSamples(t, bd))
	})

	t.Run("server mode warns and continues", func(t *testing.T) {
		beads := doltBeadsDir(t, "")
		c, _, logs := doltClient(t, doltStage(t, map[string]string{"context": "context-server", "config-get": "config-get-auto-commit"}, beads))
		status, err := c.CheckDolt(ctx)
		require.NoError(t, err)
		assert.False(t, status.Embedded())
		require.Len(t, status.Warnings, 2, strings.Join(status.Warnings, "\n"))
		assert.Equal(t, ServerModeWarning(status.Context), status.Warnings[0])
		assert.Contains(t, status.Warnings[0], `Dolt mode "server"`)
		assert.Contains(t, status.Warnings[0], "server 127.0.0.1")
		assert.Contains(t, status.Warnings[0], "continues anyway")
		assert.Contains(t, status.Warnings[1], `metadata.json says dolt_mode "embedded" but bd context reports "server"`)
		assert.Equal(t, AutoCommitOn, status.AutoCommit)
		assert.Contains(t, logs.String(), "level=WARN")
		assert.Contains(t, logs.String(), "dolt_mode=server")
		assert.Contains(t, logs.String(), "only been tested against embedded mode")
	})

	t.Run("auto-commit off warns", func(t *testing.T) {
		beads := doltBeadsDir(t, "")
		c, _, logs := doltClient(t, doltStage(t, map[string]string{"config-get": "config-get-auto-commit-off"}, beads))
		status, err := c.CheckDolt(ctx)
		require.NoError(t, err)
		assert.Equal(t, AutoCommitOff, status.AutoCommit)
		require.Len(t, status.Warnings, 1)
		assert.Equal(t, AutoCommitWarning(AutoCommitOff), status.Warnings[0])
		assert.Contains(t, status.Warnings[0], `dolt.auto-commit is "off", not "on"`)
		assert.Contains(t, status.Warnings[0], "bd dolt commit")
		assert.Contains(t, logs.String(), "level=WARN")
	})

	t.Run("unset auto-commit is the embedded default", func(t *testing.T) {
		beads := doltBeadsDir(t, "")
		c, _, _ := doltClient(t, doltStage(t, map[string]string{"config-get": "config-get-missing"}, beads))
		status, err := c.CheckDolt(ctx)
		require.NoError(t, err)
		assert.Empty(t, status.AutoCommit)
		assert.Empty(t, status.Warnings)
	})

	t.Run("unreadable metadata warns", func(t *testing.T) {
		beads := filepath.Join(t.TempDir(), ".beads")
		c, _, _ := doltClient(t, doltStage(t, map[string]string{"config-get": "config-get-auto-commit"}, beads))
		status, err := c.CheckDolt(ctx)
		require.NoError(t, err)
		require.Len(t, status.Warnings, 1)
		assert.Contains(t, status.Warnings[0], "could not read the beads project metadata")
		assert.Contains(t, status.Warnings[0], ProjectMetadataFile)
		assert.Equal(t, ProjectMetadata{}, status.Metadata)
		assert.True(t, status.Embedded())
	})

	t.Run("no project is an error", func(t *testing.T) {
		c, _, _ := doltClient(t, clientStage(t, map[string]string{"context": "error-context-no-project"}))
		_, err := c.CheckDolt(ctx)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrNoProject)
	})

	t.Run("config get failure is an error", func(t *testing.T) {
		beads := doltBeadsDir(t, "")
		c, _, _ := doltClient(t, doltStage(t, map[string]string{"config-get": "error-no-project"}, beads))
		_, err := c.CheckDolt(ctx)
		require.Error(t, err)
		var bdErr *Error
		require.True(t, errors.As(err, &bdErr))
		assert.Equal(t, "config get", bdErr.Command)
	})

	t.Run("warnings found before a config get failure are still logged", func(t *testing.T) {
		beads := doltBeadsDir(t, "")
		c, _, logs := doltClient(t, doltStage(t, map[string]string{"context": "context-server", "config-get": "error-no-project"}, beads))
		status, err := c.CheckDolt(ctx)
		require.Error(t, err)
		require.Len(t, status.Warnings, 2, strings.Join(status.Warnings, "\n"))
		assert.Equal(t, ServerModeWarning(status.Context), status.Warnings[0])
		assert.Empty(t, status.AutoCommit)
		assert.Contains(t, logs.String(), "only been tested against embedded mode")
		assert.Contains(t, logs.String(), "dolt_mode=server")
	})
}

func TestDoltWarningText(t *testing.T) {
	assert.Equal(t, `beads reports Dolt mode "unknown"; sc has only been tested against embedded mode and continues anyway`, ServerModeWarning(Context{}))
	assert.Equal(t, `beads reports Dolt mode "server"; sc has only been tested against embedded mode and continues anyway (server db.example)`,
		ServerModeWarning(Context{DoltMode: DoltModeServer, ServerHost: "db.example"}))
	assert.Equal(t, `dolt.auto-commit is "batch", not "on": writes are not Dolt commits until `+"`bd dolt commit`"+`, so `+"`bd history`"+` will not show one entry per sc write`,
		AutoCommitWarning(AutoCommitBatch))
}
