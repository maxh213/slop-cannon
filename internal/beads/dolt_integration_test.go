//go:build integration

package beads

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
)

// TestIntegrationDoltOneCommitPerWrite proves the embedded Dolt contract
// P3-06 documents against the real bd: a fresh init is embedded mode with
// dolt.auto-commit on, every sc write is exactly one Dolt commit visible
// through `bd history`, and reads add none.
func TestIntegrationDoltOneCommitPerWrite(t *testing.T) {
	c, repo := integrationClient(t)
	ctx := context.Background()

	status, err := c.CheckDolt(ctx)
	require.NoError(t, err)
	assert.Empty(t, status.Warnings)
	assert.True(t, status.Embedded())
	assert.Equal(t, filepath.Join(repo.Dir, ".beads"), status.Context.BeadsDir)
	assert.Equal(t, repo.Dir, status.Context.RepoRoot)
	assert.Equal(t, "fx", status.Context.Database)
	assert.Equal(t, DoltModeEmbedded, status.Metadata.DoltMode)
	assert.Equal(t, status.Context.ProjectID, status.Metadata.ProjectID)
	assert.Equal(t, AutoCommitOn, status.AutoCommit)

	meta, err := ReadProjectMetadata(filepath.Join(repo.Dir, ".beads"))
	require.NoError(t, err)
	assert.Equal(t, status.Metadata, meta)

	// The D-07 statuses must exist before a bead can enter in_review.
	require.NoError(t, c.SetCustomStatuses(ctx, WorkflowCustomStatuses()))

	a, err := c.Create(ctx, BeadInput{Title: "A", Priority: domain.PriorityDefault})
	require.NoError(t, err)
	b, err := c.Create(ctx, BeadInput{Title: "B", Priority: domain.PriorityDefault})
	require.NoError(t, err)

	history := func(id domain.BeadID) []HistoryEntry {
		t.Helper()
		entries, err := c.History(ctx, id, 0)
		require.NoError(t, err)
		return entries
	}
	base := len(history(a.ID))
	require.GreaterOrEqual(t, base, 1, "the creation commit is the first entry")
	assert.Equal(t, "open", history(a.ID)[len(history(a.ID))-1].Issue.Status, "the oldest entry is the creation")

	// Every write, whichever bead it touches, is exactly one Dolt commit.
	writes := []struct {
		name string
		do   func() error
	}{
		{"claim", func() error { _, err := c.Claim(ctx, a.ID, "sc:engineer-1"); return err }},
		{"update status", func() error { _, err := c.Update(ctx, a.ID, Patch{Status: domain.BeadState("in_review")}); return err }},
		{"set metadata", func() error {
			_, err := c.Update(ctx, a.ID, Patch{SetMetadata: map[string]string{MetaRound: "1"}})
			return err
		}},
		{"add label", func() error { _, err := c.Update(ctx, a.ID, Patch{AddLabels: []string{"sc:run:r1"}}); return err }},
		{"comment", func() error { _, err := c.Comment(ctx, a.ID, "round 1 report"); return err }},
		{"dep add", func() error { _, err := c.AddDep(ctx, a.ID, b.ID); return err }},
		{"close blocker", func() error { _, err := c.Close(ctx, b.ID, "done"); return err }},
		{"close", func() error { _, err := c.Close(ctx, a.ID, "done"); return err }},
	}
	for i, w := range writes {
		require.NoError(t, w.do(), w.name)
		assert.Len(t, history(a.ID), base+i+1, "after %s", w.name)
	}
	total := base + len(writes)

	// Reads leave the history alone.
	_, err = c.Show(ctx, a.ID)
	require.NoError(t, err)
	_, err = c.List(ctx, ListFilter{All: true})
	require.NoError(t, err)
	_, err = c.Ready(ctx, "")
	require.NoError(t, err)
	_, err = c.Statuses(ctx)
	require.NoError(t, err)
	_, err = c.Context(ctx)
	require.NoError(t, err)
	entries := history(a.ID)
	require.Len(t, entries, total)

	// Newest first, distinct commits, and the latest row is the closed bead.
	assert.Equal(t, "closed", entries[0].Issue.Status)
	assert.Equal(t, a.ID, entries[0].Issue.ID)
	assert.NotEmpty(t, entries[0].Committer)
	seen := map[string]bool{}
	for i, e := range entries {
		assert.NotEmpty(t, e.Commit)
		assert.False(t, seen[e.Commit], "commit %s listed twice", e.Commit)
		seen[e.Commit] = true
		assert.False(t, e.Date.IsZero())
		if i > 0 {
			assert.False(t, e.Date.After(entries[i-1].Date), "entry %d newer than %d", i, i-1)
		}
	}

	limited, err := c.History(ctx, a.ID, 2)
	require.NoError(t, err)
	require.Len(t, limited, 2)
	assert.Equal(t, entries[0].Commit, limited[0].Commit)

	_, err = c.History(ctx, "nope-1", 0)
	assert.ErrorIs(t, err, ErrNotFound)

	// The plumbing reader agrees with the client.
	bc, err := ReadContext(ctx, repo.Dir)
	require.NoError(t, err)
	assert.Equal(t, status.Context, bc)
}
