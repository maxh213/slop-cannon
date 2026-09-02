//go:build integration

package beads

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/testutil"
)

// TestIntegrationGates drives the gate commands against the real bd on a
// fixture repository with a `gh` shim standing in for GitHub: a gate is
// created for a bead, keeps it out of `bd ready`, stays pending while the
// PR is open, resolves when it merges (after which the bead closes), is
// escalated when the PR is closed without merging, and reports gh's
// absence as a warning rather than an error.
func TestIntegrationGates(t *testing.T) {
	c, _ := integrationClient(t)
	ctx := context.Background()
	gh := gatesChainGH(t)
	setGH := func(state string) {
		require.NoError(t, os.WriteFile(filepath.Join(gh.Dir, "state"), []byte(state), 0o644))
	}

	bead, err := c.Create(ctx, BeadInput{Title: "Fix Max", Type: domain.BeadTypeTask, Priority: 1})
	require.NoError(t, err)

	check, err := c.CheckGates(ctx, GateCheckOptions{})
	require.NoError(t, err)
	assert.Equal(t, GateCheck{}, check, "no gate yet")

	gate, err := c.CreatePRGate(ctx, bead.ID, 1, "PR #1")
	require.NoError(t, err)
	assert.Equal(t, bead.ID, gate.Blocks)
	assert.Equal(t, "1", gate.AwaitID)
	assert.True(t, gate.IsOpen())

	found, err := c.GateFor(ctx, bead.ID)
	require.NoError(t, err)
	assert.Equal(t, gate.ID, found.ID)
	shown, err := c.ShowGate(ctx, gate.ID)
	require.NoError(t, err)
	assert.Equal(t, gate.ID, shown.ID)

	ready, err := c.Ready(ctx, "")
	require.NoError(t, err)
	assert.NotContains(t, IssueIDs(ready), bead.ID, "the gate blocks the bead")
	_, err = c.Close(ctx, bead.ID, "too early")
	assert.ErrorIs(t, err, ErrBlockedOpen, "bd refuses to close a gated bead (D-20)")

	// Still open.
	setGH("OPEN")
	check, err = c.CheckGates(ctx, GateCheckOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, check.Checked)
	assert.Equal(t, []domain.BeadID{gate.ID}, check.ByOutcome(GatePending))
	assert.Empty(t, check.Warnings)
	calls := gh.ArgLists(t)
	require.NotEmpty(t, calls)
	assert.Equal(t, []string{"pr", "view", "1", "--json", "state,title"}, calls[len(calls)-1])

	// Merged: the gate resolves, the bead is ready and closes.
	setGH("MERGED")
	check, err = c.CheckGates(ctx, GateCheckOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, check.Resolved)
	assert.Equal(t, []domain.BeadID{gate.ID}, check.ByOutcome(GateResolved))
	_, err = c.GateFor(ctx, bead.ID)
	assert.ErrorIs(t, err, ErrNoGate)
	closedGate, err := c.ShowGate(ctx, gate.ID)
	require.NoError(t, err)
	assert.False(t, closedGate.IsOpen())
	assert.Contains(t, closedGate.CloseReason, "merged")
	all, err := c.Gates(ctx, true)
	require.NoError(t, err)
	assert.Len(t, all, 1)
	ready, err = c.Ready(ctx, "")
	require.NoError(t, err)
	assert.Contains(t, IssueIDs(ready), bead.ID)
	closed, err := c.Close(ctx, bead.ID, "merged")
	require.NoError(t, err)
	assert.Equal(t, "closed", closed.Status)

	// Closed without merge: escalated, gate left open, bead still blocked.
	bead2, err := c.Create(ctx, BeadInput{Title: "Fix Mean", Type: domain.BeadTypeTask, Priority: 1})
	require.NoError(t, err)
	gate2, err := c.CreatePRGate(ctx, bead2.ID, 2, "")
	require.NoError(t, err)
	setGH("CLOSED")
	check, err = c.CheckGates(ctx, GateCheckOptions{Escalate: true})
	require.NoError(t, err)
	assert.Equal(t, 1, check.Escalated)
	require.Len(t, check.Results, 1)
	assert.Equal(t, GateResult{GateID: gate2.ID, Outcome: GateEscalated, Detail: "PR 'Fix Max' was closed without merging"}, check.Results[0])
	still, err := c.GateFor(ctx, bead2.ID)
	require.NoError(t, err)
	assert.Equal(t, gate2.ID, still.ID)
	_, err = c.Close(ctx, bead2.ID, "nope")
	assert.ErrorIs(t, err, ErrBlockedOpen)

	// gh not authenticated: a warning, not an error.
	setGH("FAIL To get started with GitHub CLI, please run:  gh auth login")
	check, err = c.CheckGates(ctx, GateCheckOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, check.Errors)
	reason, unavailable := check.GHUnavailable()
	assert.True(t, unavailable, "%+v", check)
	assert.Contains(t, reason, "gh auth login")

	// gh not installed: PATH holds only bd.
	bin := t.TempDir()
	real, err := testutil.RealBinary("bd")
	require.NoError(t, err)
	require.NoError(t, os.Symlink(real, filepath.Join(bin, "bd")))
	t.Setenv("PATH", bin)
	check, err = c.CheckGates(ctx, GateCheckOptions{})
	require.NoError(t, err)
	reason, unavailable = check.GHUnavailable()
	assert.True(t, unavailable, "%+v", check)
	assert.Equal(t, "gh CLI not installed", reason)
	assert.Equal(t, []string{string(gate2.ID) + ": gh CLI not installed"}, check.Warnings)

	// Resolving by hand unblocks the bead.
	require.NoError(t, c.ResolveGate(ctx, gate2.ID, "human merged elsewhere"))
	_, err = c.GateFor(ctx, bead2.ID)
	assert.ErrorIs(t, err, ErrNoGate)
	assert.ErrorIs(t, c.ResolveGate(ctx, "nope-1", ""), ErrNotFound)
}
