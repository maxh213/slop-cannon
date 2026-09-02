//go:build integration

package beads

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/testutil"
)

// integrationEnv isolates HOME and the XDG directories (bd keeps its own
// state under ~/.config/bd, ~/.beads and ~/.dolt) and skips when the real
// bd is not installed.
func integrationEnv(t *testing.T) {
	t.Helper()
	if _, err := testutil.RealBinary("bd"); err != nil {
		t.Skip("bd not installed")
	}
	if _, err := testutil.RealBinary("git"); err != nil {
		t.Skip("git not installed")
	}
	envIsolate(t)
}

// integrationSnapshot captures everything about the fixture that init must
// leave untouched.
type integrationSnapshot struct {
	head      string
	log       []string
	status    string
	gitignore string
	hasIgnore bool
}

func integrationSnap(t *testing.T, r *testutil.FixtureRepo) integrationSnapshot {
	t.Helper()
	gi, has := initReadGitignore(t, r.Dir)
	return integrationSnapshot{
		head:      r.Head(t),
		log:       r.Log(t),
		status:    r.Git(t, "status", "--porcelain", "--untracked-files=all"),
		gitignore: gi,
		hasIgnore: has,
	}
}

func TestIntegrationDetectRealBD(t *testing.T) {
	integrationEnv(t)
	info, err := Detect(context.Background())
	require.NoError(t, err)
	assert.True(t, info.Version.AtLeast(MinVersion), info.String())
	assert.NotEmpty(t, info.Path)
}

func TestIntegrationEnsureInitialisedLeavesTheRepoUntouched(t *testing.T) {
	integrationEnv(t)
	fixture := testutil.NewFixtureRepo(t)
	before := integrationSnap(t, fixture)
	require.True(t, before.hasIgnore, "the fixture ships a .gitignore so the revert path is exercised")
	require.Empty(t, before.status, "fixture starts clean")

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	res, err := EnsureInitialised(context.Background(), fixture.Dir, InitOptions{Logger: logger})
	require.NoError(t, err, logs.String())

	assert.Equal(t, fixture.Dir, res.Root)
	assert.True(t, res.Initialised)
	assert.Equal(t, "repo", res.Prefix, "NewFixtureRepo's directory is named repo")
	assert.True(t, res.GitignoreReverted)
	assert.Empty(t, res.GitignoreWarning)
	assert.Equal(t, "ok", res.Ping.Status)
	assert.Contains(t, res.Output, "bd initialized successfully")
	assert.Contains(t, res.Output, "Mode: embedded")

	after := integrationSnap(t, fixture)
	assert.Equal(t, before.head, after.head, "no commit was made")
	assert.Equal(t, before.log, after.log, "git log is unchanged")
	assert.Empty(t, after.status, "git status shows nothing: .beads/ is excluded and .gitignore restored")
	assert.Equal(t, before.gitignore, after.gitignore, ".gitignore is byte-identical")

	assert.DirExists(t, filepath.Join(fixture.Dir, ".beads"))
	assert.FileExists(t, filepath.Join(fixture.Dir, ".beads", "metadata.json"))
	exclude, err := os.ReadFile(filepath.Join(fixture.Dir, ".git", "info", "exclude"))
	require.NoError(t, err)
	assert.Contains(t, string(exclude), ".beads/")
	assert.Equal(t, ".beads", fixture.Git(t, "check-ignore", ".beads"), ".beads/ is ignored through .git/info/exclude")
	assert.Empty(t, fixture.Git(t, "ls-files", "--others", "--exclude-standard"), "nothing new is untracked")

	// D-13 side effects that must not appear: agent files and hooks.
	for _, rel := range []string{"CLAUDE.md", "AGENTS.md", ".claude", ".codex", ".agents"} {
		assert.NoFileExists(t, filepath.Join(fixture.Dir, rel), rel)
		assert.NoDirExists(t, filepath.Join(fixture.Dir, rel), rel)
	}
	for _, hook := range []string{"pre-commit", "post-merge", "pre-push", "post-checkout", "prepare-commit-msg"} {
		assert.NoFileExists(t, filepath.Join(fixture.Dir, ".git", "hooks", hook), hook)
	}
	assert.Contains(t, logs.String(), "beads initialised")

	// Re-running is a no-op: .beads/ exists, so only the health check runs.
	logs.Reset()
	res, err = EnsureInitialised(context.Background(), filepath.Join(fixture.Dir, "stats"), InitOptions{Logger: logger})
	require.NoError(t, err, logs.String())
	assert.Equal(t, fixture.Dir, res.Root, "a nested path resolves to the root")
	assert.False(t, res.Initialised)
	assert.Equal(t, "ok", res.Ping.Status)
	again := integrationSnap(t, fixture)
	assert.Equal(t, after, again)
	assert.Contains(t, logs.String(), "beads already initialised")
	assert.NotContains(t, logs.String(), "level=WARN")

	// The real binary also honours --init-if-missing when asked directly.
	out, err := Run(context.Background(), CallOptions{Dir: fixture.Dir, Timeout: InitTimeout}, InitArgs("repo")...)
	require.NoError(t, err)
	assert.Empty(t, strings.TrimSpace(out.Stdout))
	assert.Contains(t, out.Stderr, "already initialized")
	assert.Equal(t, after, integrationSnap(t, fixture))
}

func TestIntegrationEnsureInitialisedWithoutGitignore(t *testing.T) {
	integrationEnv(t)
	fixture := testutil.NewFixtureRepo(t)
	fixture.Git(t, "rm", "-q", ".gitignore")
	fixture.Git(t, "commit", "-q", "-m", "drop .gitignore")
	before := integrationSnap(t, fixture)
	require.False(t, before.hasIgnore)

	res, err := EnsureInitialised(context.Background(), fixture.Dir, InitOptions{Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))})
	require.NoError(t, err)
	assert.True(t, res.Initialised)
	assert.True(t, res.GitignoreReverted, "the .gitignore bd created is deleted again")

	after := integrationSnap(t, fixture)
	assert.False(t, after.hasIgnore)
	assert.Equal(t, before.log, after.log)
	assert.Empty(t, after.status)
	assert.Equal(t, "ok", res.Ping.Status)
}

func TestIntegrationEnsureInitialisedKeepsUserGitignoreEdits(t *testing.T) {
	integrationEnv(t)
	fixture := testutil.NewFixtureRepo(t)
	// An uncommitted edit to .gitignore is the user's; init must restore
	// exactly that content, not the committed one.
	fixture.WriteFile(t, ".gitignore", "/fixture\nscratch/")
	before := integrationSnap(t, fixture)
	require.Equal(t, "M .gitignore", before.status, "FixtureRepo.Git trims the porcelain line")

	res, err := EnsureInitialised(context.Background(), fixture.Dir, InitOptions{Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))})
	require.NoError(t, err)
	assert.True(t, res.GitignoreReverted)
	after := integrationSnap(t, fixture)
	assert.Equal(t, before.gitignore, after.gitignore)
	assert.Equal(t, before.status, after.status)
	assert.Equal(t, before.log, after.log)
}

func TestIntegrationPingWithoutDatabase(t *testing.T) {
	integrationEnv(t)
	fixture := testutil.NewFixtureRepo(t)
	_, err := Ping(context.Background(), fixture.Dir)
	var ce *CheckError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, ReasonFailed, ce.Reason)
	assert.Contains(t, err.Error(), "no beads project found")
}
