//go:build integration

package beads

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/testutil"
)

// integrationClient initialises beads in a fresh fixture repository and
// returns a client for it.
func integrationClient(t *testing.T) (*Client, *testutil.FixtureRepo) {
	t.Helper()
	integrationEnv(t)
	repo := testutil.NewFixtureRepo(t)
	_, err := EnsureInitialised(context.Background(), repo.Dir, InitOptions{Prefix: "fx"})
	require.NoError(t, err)
	return New(repo.Dir, Options{Actor: "sc:orch", Timeout: time.Minute}), repo
}

// TestIntegrationClientLifecycle drives every client method against the
// real bd on a fixture repository: statuses, a graph plan, a hand-made
// bead, claim, every D-07 status, comments, dependencies, the close
// refusal, close, readiness, export, and the read-only refusal.
func TestIntegrationClientLifecycle(t *testing.T) {
	c, repo := integrationClient(t)
	ctx := context.Background()

	ping, err := c.Ping(ctx)
	require.NoError(t, err)
	assert.Equal(t, "ok", ping.Status)

	// Custom statuses (D-07), unioned with whatever the repo already had.
	current, err := c.ConfigGet(ctx, StatusCustomKey)
	require.NoError(t, err)
	existing, err := ParseCustomStatuses(current)
	require.NoError(t, err)
	require.NoError(t, c.SetCustomStatuses(ctx, MergeCustomStatuses(existing, WorkflowCustomStatuses())))
	statuses, err := c.Statuses(ctx)
	require.NoError(t, err)
	for _, s := range domain.WorkflowBeadStates() {
		assert.True(t, statuses.Has(string(s)), "%s registered", s)
	}
	value, err := c.ConfigGet(ctx, StatusCustomKey)
	require.NoError(t, err)
	assert.Contains(t, value, "in_review:wip")

	// A graph plan with a diamond-free single edge (D-24 edges).
	plan := GraphPlan{
		CommitMessage: "sc run r1",
		Nodes: []GraphNode{
			{Key: "epic", Title: "Run r1", Type: domain.BeadTypeEpic, Priority: 1, Labels: []string{"sc:run:r1"}, Metadata: map[string]any{MetaRun: "r1", MetaKind: "epic"}},
			{Key: "mean", Title: "Fix Mean", Type: domain.BeadTypeBug, Priority: 1, ParentKey: "epic", Labels: []string{"sc:run:r1"}, Metadata: map[string]any{MetaRun: "r1"}},
			{Key: "max", Title: "Fix Max", Type: domain.BeadTypeBug, Priority: 2, ParentKey: "epic", Labels: []string{"sc:run:r1"}, Metadata: map[string]any{MetaRun: "r1"}},
		},
		Edges: []GraphEdge{{FromKey: "max", ToKey: "mean", Type: DepBlocks}},
	}
	dry, err := c.CreateGraph(ctx, plan, true)
	require.NoError(t, err)
	assert.True(t, dry.DryRun)
	assert.Equal(t, 3, dry.NodeCount)
	assert.Equal(t, 1, dry.EdgeCount)
	assert.Empty(t, dry.Warnings)
	all, err := c.List(ctx, ListFilter{All: true})
	require.NoError(t, err)
	assert.Empty(t, all, "a dry run creates nothing")

	res, err := c.CreateGraph(ctx, plan, false)
	require.NoError(t, err)
	epic, mean, max := res.IDs["epic"], res.IDs["mean"], res.IDs["max"]
	require.NotEmpty(t, epic)
	require.NotEmpty(t, mean)
	require.NotEmpty(t, max)

	// A minimal plan dry-runs without warnings (GraphNode has no field bd
	// 1.2.2 drops; the unknown-field warning is covered by the captured
	// create-graph-dry-run-warning sample).
	clean, err := c.CreateGraph(ctx, GraphPlan{Nodes: []GraphNode{{Key: "a", Title: "A", Priority: 2}}}, true)
	require.NoError(t, err)
	assert.Empty(t, clean.Warnings)

	// Hand-made beads with metadata through a file.
	median, err := c.Create(ctx, BeadInput{
		Title: "Add Median", Type: domain.BeadTypeTask, Priority: domain.PriorityDefault,
		Description: "README promises Median", Acceptance: "TestMedian passes",
		Labels: []string{"sc:run:r1", "sc:role:engineer"}, Metadata: map[string]any{MetaRun: "r1", MetaRound: 0, MetaKind: "work"},
		Parent: epic,
	})
	require.NoError(t, err)
	assert.Empty(t, median.ParentID(), "bd create --json prints no parent field")
	medianShown, err := c.Show(ctx, median.ID)
	require.NoError(t, err)
	assert.Equal(t, epic, medianShown[0].ParentID())
	round, ok := median.Metadata.Int(MetaRound)
	assert.True(t, ok)
	assert.Zero(t, round)
	tests, err := c.Create(ctx, BeadInput{Title: "Write Median tests", Priority: 3, Labels: []string{"sc:run:r1"}, Parent: epic})
	require.NoError(t, err)
	_, err = c.Create(ctx, BeadInput{Title: "X", Priority: 2, Parent: "nope-1"})
	assert.ErrorIs(t, err, ErrNotFound)

	// Show returns an array even for one id, with comments.
	shown, err := c.Show(ctx, mean)
	require.NoError(t, err)
	require.Len(t, shown, 1)
	assert.Equal(t, "Fix Mean", shown[0].Title)
	_, err = c.Show(ctx, "nope-1")
	assert.ErrorIs(t, err, ErrNotFound)
	partial, err := c.Show(ctx, mean, "nope-1")
	assert.ErrorIs(t, err, ErrNotFound)
	assert.Equal(t, []domain.BeadID{mean}, IssueIDs(partial))

	// Dependencies: median depends on tests; a cycle is refused.
	dep, err := c.AddDep(ctx, median.ID, tests.ID)
	require.NoError(t, err)
	assert.Equal(t, "added", dep.Status)
	_, err = c.AddDep(ctx, tests.ID, median.ID)
	assert.ErrorIs(t, err, ErrCycle)
	deps, err := c.DepList(ctx, median.ID)
	require.NoError(t, err)
	var blockers []domain.BeadID
	for _, d := range deps {
		assert.Equal(t, median.ID, d.IssueID)
		if d.Type == DepBlocks {
			blockers = append(blockers, d.DependsOnID)
		}
	}
	assert.Equal(t, []domain.BeadID{tests.ID}, blockers)
	multi, err := c.DepList(ctx, median.ID, max)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(multi), 3)

	// Ready never lists the epic; max is blocked by mean, median by tests.
	ready, err := c.Ready(ctx, "")
	require.NoError(t, err)
	assert.ElementsMatch(t, []domain.BeadID{mean, tests.ID}, IssueIDs(ready))
	readyChildren, err := c.Ready(ctx, epic)
	require.NoError(t, err)
	assert.ElementsMatch(t, []domain.BeadID{mean, tests.ID}, IssueIDs(readyChildren))
	blocked, err := c.Blocked(ctx)
	require.NoError(t, err)
	byID := map[domain.BeadID]Issue{}
	for _, is := range blocked {
		byID[is.ID] = is
	}
	assert.Equal(t, []domain.BeadID{mean}, byID[max].BlockedBy)
	assert.Equal(t, []domain.BeadID{tests.ID}, byID[median.ID].BlockedBy)

	// Claim: atomic, idempotent for the same actor, refused for another.
	claimed, err := c.Claim(ctx, mean, "sc:engineer-1")
	require.NoError(t, err)
	assert.Equal(t, "in_progress", claimed.Status)
	assert.Equal(t, "sc:engineer-1", claimed.Assignee)
	_, err = c.Claim(ctx, mean, "sc:engineer-1")
	assert.NoError(t, err)
	_, err = c.Claim(ctx, mean, "sc:engineer-2")
	assert.ErrorIs(t, err, ErrAlreadyClaimed)

	// Comments as the agent, read back in order.
	body := "Implementation report\n\n```json\n{\"type\":\"impl_report\",\"summary\":\"fixed Mean\"}\n```\n"
	cm, err := c.WithActor("sc:engineer-1").Comment(ctx, mean, body)
	require.NoError(t, err)
	assert.Equal(t, "sc:engineer-1", cm.Author)
	assert.Equal(t, mean, cm.IssueID)
	_, err = c.Comment(ctx, mean, "verdict follows")
	require.NoError(t, err)
	comments, err := c.Comments(ctx, mean)
	require.NoError(t, err)
	require.Len(t, comments, 2)
	assert.Contains(t, comments[0].Text, "impl_report")
	assert.Equal(t, "sc:orch", comments[1].Author)
	shown, err = c.Show(ctx, mean)
	require.NoError(t, err)
	assert.Len(t, shown[0].Comments, 2)

	// Every D-07 status, with metadata in both flavours.
	for _, state := range []domain.BeadState{
		domain.BeadStateInReview, domain.BeadStateChangesRequested, domain.BeadStateInProgress,
		domain.BeadStateInReview, domain.BeadStateApproved, domain.BeadStateDelivering,
	} {
		is, err := c.Update(ctx, mean, Patch{Status: state, SetMetadata: map[string]string{MetaRound: "1"}})
		require.NoError(t, err, state)
		assert.Equal(t, string(state), is.Status)
	}
	is, err := c.Update(ctx, mean, Patch{Metadata: map[string]any{MetaPRURL: "https://example.com/pr/7", MetaPRNumber: 7}, AddLabels: []string{"sc:role:engineer"}})
	require.NoError(t, err)
	n, ok := is.Metadata.Int(MetaPRNumber)
	assert.True(t, ok)
	assert.Equal(t, 7, n)
	round, ok = is.Metadata.Int(MetaRound)
	assert.True(t, ok)
	assert.Equal(t, 1, round)
	assert.True(t, is.HasLabel("sc:role:engineer"))
	b, err := is.Bead()
	require.NoError(t, err)
	assert.Equal(t, domain.BeadStateDelivering, b.State)
	assert.Equal(t, 7, b.PRNumber)
	assert.Equal(t, 1, b.Round)
	_, err = c.Update(ctx, mean, Patch{Status: "not_a_status"})
	assert.ErrorIs(t, err, ErrInvalid)
	_, err = c.Update(ctx, "nope-1", Patch{Status: domain.BeadStateOpen})
	assert.ErrorIs(t, err, ErrNotFound)

	// Custom statuses keep beads out of bd ready.
	ready, err = c.Ready(ctx, "")
	require.NoError(t, err)
	assert.NotContains(t, IssueIDs(ready), mean)

	// Filtered listings.
	filtered, err := c.List(ctx, ListFilter{Statuses: []domain.BeadState{domain.BeadStateDelivering}, Labels: []string{"sc:run:r1"}, Parent: epic, Metadata: map[string]string{MetaRun: "r1"}})
	require.NoError(t, err)
	assert.Equal(t, []domain.BeadID{mean}, IssueIDs(filtered))

	// Close: refused while blockers are open, then done.
	_, err = c.Close(ctx, median.ID, "done")
	require.ErrorIs(t, err, ErrBlockedOpen)
	assert.Contains(t, err.Error(), string(tests.ID))
	closed, err := c.Close(ctx, mean, "fixed in round 1")
	require.NoError(t, err)
	assert.Equal(t, "closed", closed.Status)
	assert.Equal(t, "fixed in round 1", closed.CloseReason)
	_, err = c.Close(ctx, mean, "again")
	assert.NoError(t, err)
	ready, err = c.Ready(ctx, "")
	require.NoError(t, err)
	assert.Contains(t, IssueIDs(ready), max, "closing the blocker unblocks max")
	for _, is := range ready {
		assert.False(t, is.IsEpic())
	}
	open, err := c.List(ctx, ListFilter{})
	require.NoError(t, err)
	assert.NotContains(t, IssueIDs(open), mean, "default listings exclude closed")
	all, err = c.List(ctx, ListFilter{All: true})
	require.NoError(t, err)
	assert.Contains(t, IssueIDs(all), mean)

	// Export: every issue with edges and comments.
	exported, err := c.Export(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, IssueIDs(all), IssueIDs(exported))
	for _, is := range exported {
		switch is.ID {
		case mean:
			assert.Len(t, is.Comments, 2)
			assert.Equal(t, "closed", is.Status)
		case max:
			assert.Equal(t, []domain.BeadID{mean}, is.Blockers())
		case median.ID:
			assert.Equal(t, []domain.BeadID{tests.ID}, is.Blockers())
		}
	}

	// Read-only client: reads work, writes are refused.
	ro := New(repo.Dir, Options{Actor: "sc:engineer-1", ReadOnly: true})
	roShown, err := ro.Show(ctx, max)
	require.NoError(t, err)
	assert.Len(t, roShown, 1)
	_, err = ro.Update(ctx, max, Patch{Status: domain.BeadStateOpen})
	assert.ErrorIs(t, err, ErrReadOnly)
	_, err = ro.Close(ctx, max, "x")
	assert.ErrorIs(t, err, ErrReadOnly)
	_, err = ro.Comment(ctx, max, "x")
	assert.ErrorIs(t, err, ErrReadOnly)

	// The repository itself is untouched apart from the excluded .beads/.
	assert.Empty(t, repo.Git(t, "status", "--porcelain", "--untracked-files=all"))
}

func TestIntegrationClientNoProject(t *testing.T) {
	integrationEnv(t)
	c := New(t.TempDir(), Options{})
	_, err := c.Ready(context.Background(), "")
	assert.ErrorIs(t, err, ErrNoProject)
}

func TestIntegrationCallsAreSerialised(t *testing.T) {
	c, _ := integrationClient(t)
	ctx := context.Background()
	epic, err := c.Create(ctx, BeadInput{Title: "Epic", Type: domain.BeadTypeEpic, Priority: 1})
	require.NoError(t, err)
	done := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func(i int) {
			_, err := c.Create(ctx, BeadInput{Title: "child", Priority: 2, Parent: epic.ID, Metadata: map[string]any{"i": i}})
			done <- err
		}(i)
	}
	for i := 0; i < 10; i++ {
		assert.NoError(t, <-done)
	}
	children, err := c.List(ctx, ListFilter{Parent: epic.ID})
	require.NoError(t, err)
	assert.Len(t, children, 10)
}
