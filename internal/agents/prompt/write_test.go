package prompt

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
)

// TestPromptWriteUsesRunDir covers D-23: the debug copy goes to
// <run-dir>/prompts/<agent-id>.md under the state directory the caller
// passes in, never into the target repository.
func TestPromptWriteUsesRunDir(t *testing.T) {
	state := t.TempDir()
	repo := filepath.Join(t.TempDir(), "widgets")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	t.Setenv("XDG_STATE_HOME", state)

	runDir := config.RunDir(repo, "20260902-101500-1a2b")
	p := MustBuild(pbEngineerRequest())
	require.NoError(t, p.Write(runDir))

	assert.Equal(t, filepath.Join(runDir, PromptsDirName, "eng-sc-7a1-r2.md"), p.Path)
	assert.Equal(t, PathFor(runDir, p.AgentID), p.Path)
	assert.Equal(t, filepath.Join(runDir, "prompts"), Dir(runDir))

	got, err := os.ReadFile(p.Path)
	require.NoError(t, err)
	assert.Equal(t, p.Text, string(got))

	info, err := os.Stat(p.Path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	dirInfo, err := os.Stat(Dir(runDir))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())

	// Nothing was written inside the repository.
	entries, err := os.ReadDir(repo)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestWriteFileSanitisesAgentID(t *testing.T) {
	runDir := t.TempDir()
	path, err := WriteFile(runDir, domain.AgentID("../../escape id"), "hi")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(runDir, PromptsDirName, "___.__escape_id.md"), path)
	assert.Equal(t, filepath.Dir(path), Dir(runDir))
}

func TestWriteFileReplacesAnEarlierPrompt(t *testing.T) {
	runDir := t.TempDir()
	_, err := WriteFile(runDir, "eng-1", "first attempt")
	require.NoError(t, err)
	path, err := WriteFile(runDir, "eng-1", "retry")
	require.NoError(t, err)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "retry", string(got))
}

func TestWriteFileErrors(t *testing.T) {
	_, err := WriteFile("", "eng-1", "x")
	require.ErrorContains(t, err, "run directory")
	_, err = WriteFile(t.TempDir(), "", "x")
	require.ErrorContains(t, err, "agent id")

	// A run directory that cannot hold the prompts directory is reported.
	dir := t.TempDir()
	blocked := filepath.Join(dir, "run")
	require.NoError(t, os.WriteFile(blocked, []byte("not a directory"), 0o600))
	_, err = WriteFile(blocked, "eng-1", "x")
	require.Error(t, err)

	p := MustBuild(pbPORequest())
	require.Error(t, p.Write(blocked))
	assert.Empty(t, p.Path)
}
