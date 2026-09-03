//go:build integration

package beads

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/testutil"
)

// shimAgentRun runs `bd <args>` the way an agent would: resolved from a PATH
// that starts with the shim directory, from the worktree, with the D-21
// environment and the isolated HOME/XDG variables of integrationEnv.
func shimAgentRun(t *testing.T, s Shim, dir string, args ...string) (string, string, int) {
	t.Helper()
	// exec.Command resolves the name on the parent's PATH, not on cmd.Env,
	// so look bd up on the agent's PATH by hand: the shim must come first.
	var found string
	for _, d := range filepath.SplitList(s.PathEnv()) {
		if info, err := os.Stat(filepath.Join(d, "bd")); err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			found = filepath.Join(d, "bd")
			break
		}
	}
	require.Equal(t, s.Path, found, "the shim shadows the real bd on the agent's PATH")
	cmd := exec.Command(found, args...)
	cmd.Dir = dir
	env := []string{"PATH=" + s.PathEnv(), "BD_JSON_ENVELOPE=1", "BD_NON_INTERACTIVE=1", "BEADS_ACTOR=sc:engineer-1"}
	for _, name := range InheritEnv {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		require.ErrorAs(t, err, &ee, "running bd through the shim: %v", err)
		code = ee.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

// shimOverrideArg returns the --readonly argument in args, the one the
// shim must name in its refusal.
func shimOverrideArg(args []string) string {
	for _, a := range args {
		if a == ReadOnlyFlag || strings.HasPrefix(a, ReadOnlyFlag+"=") {
			return a
		}
	}
	return ""
}

// shimRealBD runs the genuine bd (not the shim) in dir and returns its
// output, so a test can read state the shim refuses to show.
func shimRealBD(t *testing.T, dir string, args ...string) string {
	t.Helper()
	real, err := testutil.RealBinary("bd")
	require.NoError(t, err)
	cmd := exec.Command(real, args...)
	cmd.Dir = dir
	env := []string{"BD_JSON_ENVELOPE=1", "BD_NON_INTERACTIVE=1", "BEADS_ACTOR=sc:test"}
	for _, name := range InheritEnv {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Run(), stderr.String())
	return stdout.String()
}

// TestIntegrationShimAgainstRealBD proves the D-21 agent contract with the
// real binary from inside a linked worktree: every write is refused with
// bd's read-only message — by the shim's allow-list, before bd runs, and by
// bd itself when the real binary is called with --readonly (Shim.Verify) —
// the state-changing commands bd leaves open change nothing, and reads keep
// working and decode.
func TestIntegrationShimAgainstRealBD(t *testing.T) {
	ctx := context.Background()
	// bd below MinVersion has a different CLI (1.0.5 rejects the D-13
	// `bd init --init-if-missing`), so the fixture below would fail to set
	// up for an environment reason. Skip instead, with the install hint.
	if _, err := Detect(ctx); err != nil {
		t.Skipf("bd is unusable for this test: %v; %s", err, InstallHint)
	}
	c, repo := integrationClient(t)

	bead, err := c.Create(ctx, BeadInput{Title: "Shim probe", Type: domain.BeadTypeTask, Priority: domain.PriorityDefault})
	require.NoError(t, err)

	// A plain linked worktree outside the repo, as P4-02 creates them.
	wt := filepath.Join(t.TempDir(), "wt", string(bead.ID))
	repo.Git(t, "worktree", "add", "-q", wt, "-b", "sc/"+string(bead.ID)+"-probe", "main")

	s, err := WriteShim(filepath.Join(t.TempDir(), "bin"), ShimOptions{})
	require.NoError(t, err)
	real, err := testutil.RealBinary("bd")
	require.NoError(t, err)
	assert.Equal(t, real, s.Real)

	// The shim's own self-check.
	require.NoError(t, s.Verify(ctx, wt))

	t.Run("bd update is refused", func(t *testing.T) {
		out, errOut, code := shimAgentRun(t, s, wt, "update", string(bead.ID), "-s", "in_progress", "--json")
		assert.Equal(t, 1, code)
		assert.Empty(t, strings.TrimSpace(out))
		assert.Equal(t, ReadOnlyMessage("update"), StderrMessage(errOut))
		assert.ErrorIs(t, KindOf("", StderrMessage(errOut)), ErrReadOnly)
	})
	t.Run("bd claim, comment, close, create, dep add and label add are refused", func(t *testing.T) {
		cases := []struct {
			args []string
			op   string
		}{
			{[]string{"update", string(bead.ID), "--claim", "--json"}, "update"},
			{[]string{"comment", string(bead.ID), "hello"}, "comment"},
			{[]string{"close", string(bead.ID), "--reason", "done", "--json"}, "close"},
			{[]string{"create", "sneaky", "-t", "task", "--json"}, "create"},
			{[]string{"dep", "add", string(bead.ID), string(bead.ID), "--json"}, "dep add"},
			{[]string{"label", "add", string(bead.ID), "x"}, "label"},
		}
		for _, tc := range cases {
			_, errOut, code := shimAgentRun(t, s, wt, tc.args...)
			assert.Equal(t, 1, code, strings.Join(tc.args, " "))
			assert.Equal(t, ReadOnlyMessage(tc.op), StderrMessage(errOut), strings.Join(tc.args, " "))
		}
	})
	t.Run("a --readonly override is refused", func(t *testing.T) {
		// bd's --readonly is a plain pflag bool: without the shim's guard
		// `--readonly=false` wins and every write goes through (verified
		// live against bd 1.2.2 during the P3-09 review).
		for _, args := range [][]string{
			{"--readonly=false", "update", string(bead.ID), "-s", "in_progress", "--json"},
			{"update", string(bead.ID), "--readonly=0", "-s", "in_progress", "--json"},
			{"comment", string(bead.ID), "sneaky", "--readonly=F"},
			{"--readonly", "create", "sneaky", "-t", "task", "--json"},
		} {
			out, errOut, code := shimAgentRun(t, s, wt, args...)
			assert.Equal(t, 1, code, strings.Join(args, " "))
			assert.Empty(t, strings.TrimSpace(out), strings.Join(args, " "))
			assert.Equal(t, ReadOnlyMessage(shimOverrideArg(args)), StderrMessage(errOut), strings.Join(args, " "))
			assert.Contains(t, errOut, ShimOverrideHint)
			assert.ErrorIs(t, KindOf("", StderrMessage(errOut)), ErrReadOnly)
		}
	})
	t.Run("commands bd leaves open are refused by the shim", func(t *testing.T) {
		// Everything here runs to completion under bd 1.2.2's --readonly
		// (P3-09 reviews, reproduced live), so the shim's allow-list has to
		// stop them: `config set`/`set-many` rewrite configuration — the
		// second one overwrote the D-07 status registry through the round-2
		// deny-list — `import` inserts issues, `backup` can push the whole
		// database off-machine, `federation` persists a peer, `audit`
		// appends to .beads/interactions.jsonl, and the Dolt-level `branch`
		// and `vc` commit and merge.
		// A run registers the D-07 custom statuses before any agent
		// fires; `config set-many` below tries to overwrite that registry,
		// so it has to be there to be protected.
		_, err := c.EnsureStatuses(ctx)
		require.NoError(t, err)
		statuses, err := c.ConfigGet(ctx, StatusCustomKey)
		require.NoError(t, err)
		require.NotEmpty(t, statuses, "the run's custom statuses are configured")
		before := shimRealBD(t, repo.Dir, "branch")

		backupDir := t.TempDir()
		for _, tc := range []struct {
			args []string
			op   string
		}{
			{[]string{"config", "set", "custom.sc_probe", "1"}, "config set"},
			{[]string{"config", "set-many", StatusCustomKey + "=pwned:wip"}, "config set-many"},
			{[]string{"config", "apply"}, "config apply"},
			{[]string{"import", "-i", filepath.Join(t.TempDir(), "none.jsonl")}, "import"},
			{[]string{"backup", "init", "file://" + backupDir}, "backup"},
			{[]string{"backup", "sync"}, "backup"},
			{[]string{"federation", "add-peer", "evil", "http://evil.example"}, "federation"},
			{[]string{"audit", "record", "--kind", "label"}, "audit"},
			{[]string{"comments", "add", string(bead.ID), "sneaky"}, "comments add"},
			{[]string{"branch", "sc-agent-evil"}, "branch"},
			{[]string{"vc", "commit", "-m", "sneaky"}, "vc"},
			{[]string{"vc", "merge", "main"}, "vc"},
		} {
			out, errOut, code := shimAgentRun(t, s, wt, tc.args...)
			assert.Equal(t, 1, code, strings.Join(tc.args, " "))
			assert.Empty(t, strings.TrimSpace(out), strings.Join(tc.args, " "))
			assert.Equal(t, ReadOnlyMessage(tc.op), StderrMessage(errOut), strings.Join(tc.args, " "))
			assert.Contains(t, errOut, ShimDeniedHint, strings.Join(tc.args, " "))
		}

		value, err := c.ConfigGet(ctx, "custom.sc_probe")
		require.NoError(t, err)
		assert.Empty(t, value, "nothing was written")
		after, err := c.ConfigGet(ctx, StatusCustomKey)
		require.NoError(t, err)
		assert.Equal(t, statuses, after, "the D-07 status registry is intact")
		entries, err := os.ReadDir(backupDir)
		require.NoError(t, err)
		assert.Empty(t, entries, "no backup destination was filled")

		branches := shimRealBD(t, repo.Dir, "branch")
		assert.Equal(t, before, branches, "no Dolt branch was created")
		assert.NotContains(t, branches, "sc-agent-evil")
	})
	t.Run("bd ready and bd show work", func(t *testing.T) {
		out, errOut, code := shimAgentRun(t, s, wt, "ready", "--json", "-n", "0")
		require.Equal(t, 0, code, errOut)
		var ready []Issue
		require.NoError(t, Decode(out, &ready))
		assert.Contains(t, IssueIDs(ready), bead.ID)

		out, errOut, code = shimAgentRun(t, s, wt, "show", string(bead.ID), "--json", "--include-comments")
		require.Equal(t, 0, code, errOut)
		var shown []Issue
		require.NoError(t, Decode(out, &shown))
		require.Len(t, shown, 1)
		assert.Equal(t, "Shim probe", shown[0].Title)

		out, errOut, code = shimAgentRun(t, s, wt, "config", "get", StatusCustomKey)
		require.Equal(t, 0, code, errOut)
		assert.NotEmpty(t, strings.TrimSpace(out))
	})
	t.Run("InstallForWorktree is the orchestrator's entry point", func(t *testing.T) {
		inst, err := InstallForWorktree(ctx, wt, ShimOptions{})
		require.NoError(t, err)
		want, err := ShimDirFor(wt)
		require.NoError(t, err)
		assert.Equal(t, want, inst.Dir)
		assert.Equal(t, real, inst.Real)
		assert.False(t, strings.HasPrefix(inst.Dir, wt+string(filepath.Separator)),
			"the shim lives beside the worktree, not inside it (D-23)")
		assert.Empty(t, strings.TrimSpace(repo.Git(t, "-C", wt, "status", "--porcelain")),
			"the agent's worktree stays clean")

		out, errOut, code := shimAgentRun(t, inst, wt, "ready", "--json", "-n", "0")
		require.Equal(t, 0, code, errOut)
		var ready []Issue
		require.NoError(t, Decode(out, &ready))
		assert.Contains(t, IssueIDs(ready), bead.ID)

		_, errOut, code = shimAgentRun(t, inst, wt, "update", string(bead.ID), "-s", "closed", "--json")
		assert.Equal(t, 1, code)
		assert.Equal(t, ReadOnlyMessage("update"), StderrMessage(errOut))

		// The env an agent process gets: the shim directory comes first.
		env := map[string]string{"PATH": "/usr/bin"}
		inst.Apply(env)
		assert.Equal(t, inst.Dir+string(os.PathListSeparator)+"/usr/bin", env["PATH"])
	})
	t.Run("the bead is untouched", func(t *testing.T) {
		issues, err := c.Show(ctx, bead.ID)
		require.NoError(t, err)
		require.Len(t, issues, 1)
		assert.Equal(t, "open", issues[0].Status)
		assert.Empty(t, issues[0].Assignee)
		assert.Empty(t, issues[0].Comments)
		all, err := c.List(ctx, ListFilter{All: true})
		require.NoError(t, err)
		assert.Len(t, all, 1, "no sneaky create")
	})
}
