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

// TestIntegrationShimAgainstRealBD proves the D-21 agent contract with the
// real binary from inside a linked worktree: writes are refused with bd's
// read-only message, the commands bd does not block are refused by the
// shim, and reads keep working and decode.
func TestIntegrationShimAgainstRealBD(t *testing.T) {
	c, repo := integrationClient(t)
	ctx := context.Background()

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
	t.Run("bd claim, comment, close, create and dep add are refused", func(t *testing.T) {
		cases := []struct {
			args []string
			op   string
		}{
			{[]string{"update", string(bead.ID), "--claim", "--json"}, "update"},
			{[]string{"comment", string(bead.ID), "hello"}, "comment"},
			{[]string{"close", string(bead.ID), "--reason", "done", "--json"}, "close"},
			{[]string{"create", "sneaky", "-t", "task", "--json"}, "create"},
			{[]string{"dep", "add", string(bead.ID), string(bead.ID), "--json"}, "dep add"},
			{[]string{"label", "add", string(bead.ID), "x"}, "label add"},
		}
		for _, tc := range cases {
			_, errOut, code := shimAgentRun(t, s, wt, tc.args...)
			assert.Equal(t, 1, code, strings.Join(tc.args, " "))
			assert.Equal(t, ReadOnlyMessage(tc.op), StderrMessage(errOut), strings.Join(tc.args, " "))
		}
	})
	t.Run("commands bd leaves open are refused by the shim", func(t *testing.T) {
		_, errOut, code := shimAgentRun(t, s, wt, "config", "set", "custom.sc_probe", "1")
		assert.Equal(t, 1, code)
		assert.Equal(t, ReadOnlyMessage("config set"), StderrMessage(errOut))
		value, err := c.ConfigGet(ctx, "custom.sc_probe")
		require.NoError(t, err)
		assert.Empty(t, value, "nothing was written")

		_, errOut, code = shimAgentRun(t, s, wt, "import", "-i", filepath.Join(t.TempDir(), "none.jsonl"))
		assert.Equal(t, 1, code)
		assert.Equal(t, ReadOnlyMessage("import"), StderrMessage(errOut))
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
