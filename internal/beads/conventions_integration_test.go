//go:build integration

package beads

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
)

// TestIntegrationConventions drives the P3-03/P3-14 conventions against
// the real bd on a fixture repository: status registration that unions
// with a repository's own custom statuses, the run epic, every D-07
// status keeping a bead out of `bd ready`, the six structured messages
// round-tripping through comments, the timeline, metadata and label
// helpers, and a foreign status decoding through List into a safe state.
func TestIntegrationConventions(t *testing.T) {
	c, _ := integrationClient(t)
	ctx := context.Background()
	const run = domain.RunID("it1")

	// The repository already has custom statuses of its own (D-07: merge,
	// never replace).
	foreign := []CustomStatus{{Name: "triage", Category: domain.CategoryActive}, {Name: "parked", Category: domain.CategoryFrozen}}
	require.NoError(t, c.SetCustomStatuses(ctx, foreign))

	res, err := c.EnsureStatuses(ctx)
	require.NoError(t, err)
	assert.Equal(t, foreign, res.Existing)
	assert.Equal(t, WorkflowCustomStatuses(), res.Added)
	value, err := c.ConfigGet(ctx, StatusCustomKey)
	require.NoError(t, err)
	assert.Equal(t, "triage:active,parked:frozen,"+FormatCustomStatuses(WorkflowCustomStatuses()), value)
	assert.True(t, res.Statuses.Has("triage"))

	again, err := c.EnsureStatuses(ctx)
	require.NoError(t, err)
	assert.Nil(t, again.Added, "the second call is a no-op")
	assert.Equal(t, value, again.Value)
	assert.Equal(t, MergeCustomStatuses(foreign, WorkflowCustomStatuses()), again.Existing)

	// The run epic and a work bead under it.
	epic, err := c.CreateRunEpic(ctx, RunEpicInput{Run: run, Description: "integration run"})
	require.NoError(t, err)
	assert.True(t, epic.IsRunEpic())
	assert.Equal(t, run, epic.RunID())
	assert.Equal(t, RunEpicPriority, epic.Priority)
	found, err := c.FindRunEpic(ctx, run)
	require.NoError(t, err)
	assert.Equal(t, epic.ID, found.ID)
	_, err = c.FindRunEpic(ctx, "nope")
	assert.ErrorIs(t, err, ErrNotFound)

	work, err := c.Create(ctx, BeadInput{Title: "Integration work", Priority: domain.PriorityDefault, Labels: []string{RunLabel(run)}, Metadata: WorkMetadata(run, domain.SizeS), Parent: epic.ID})
	require.NoError(t, err)
	assert.Equal(t, KindWork, work.Kind())
	size, ok := work.Size()
	assert.True(t, ok)
	assert.Equal(t, domain.SizeS, size)

	ready, err := c.Ready(ctx, epic.ID)
	require.NoError(t, err)
	assert.Contains(t, IssueIDs(ready), work.ID)

	// Every custom status keeps the bead out of `bd ready` and still
	// converts through the strict converter.
	for _, s := range domain.CustomBeadStates() {
		is, err := c.SetStatus(ctx, work.ID, s)
		require.NoError(t, err, s)
		assert.Equal(t, string(s), is.Status)
		ready, err := c.Ready(ctx, "")
		require.NoError(t, err)
		assert.NotContains(t, IssueIDs(ready), work.ID, "%s must not be ready", s)
		all, err := c.List(ctx, ListFilter{All: true})
		require.NoError(t, err)
		var seen bool
		for _, is := range all {
			if is.ID == work.ID {
				seen = true
				b, err := is.Bead()
				require.NoError(t, err)
				assert.Equal(t, s, b.State)
			}
		}
		assert.True(t, seen, "%s is listed with --all", s)
	}
	_, err = c.SetStatus(ctx, work.ID, domain.BeadStateOpen)
	require.NoError(t, err)
	ready, err = c.Ready(ctx, epic.ID)
	require.NoError(t, err)
	assert.Contains(t, IssueIDs(ready), work.ID, "open again")
	_, err = c.SetStatus(ctx, work.ID, domain.BeadStateBlocked)
	assert.ErrorIs(t, err, ErrInvalid)

	// Metadata and label helpers.
	is, err := c.SetMetadata(ctx, work.ID, MergeMeta(RoundMetadata(2), PRMetadata("https://example.com/pr/9", 9)))
	require.NoError(t, err)
	assert.Equal(t, 2, is.Round())
	assert.Equal(t, 9, is.PRNumber())
	assert.Equal(t, "https://example.com/pr/9", is.PRURL())
	is, err = c.AddLabels(ctx, work.ID, RoleLabel(domain.RoleEngineer))
	require.NoError(t, err)
	assert.Equal(t, []domain.Role{domain.RoleEngineer}, is.Roles())
	is, err = c.UnsetMetadata(ctx, work.ID, MetaPRURL, MetaPRNumber)
	require.NoError(t, err)
	assert.Empty(t, is.PRURL())
	assert.Equal(t, 2, is.Round(), "other keys survive")

	// The six structured messages, each by its own actor.
	payloads := messagesPayloads()
	authors := map[domain.MessageType]string{
		domain.MessagePlan:        ActorFor("po-it1"),
		domain.MessageImplReport:  ActorFor("eng-it1"),
		domain.MessageVerdict:     ActorFor("tester-qa-it1"),
		domain.MessageResponse:    ActorFor("eng-it1"),
		domain.MessageArbitration: ActorFor("po-it1"),
		domain.MessageEscalation:  ActorFor("po-it1"),
	}
	_, err = c.Comment(ctx, work.ID, "A human note that is not a message.")
	require.NoError(t, err)
	for _, mt := range domain.MessageTypes() {
		posted, err := c.PostMessage(ctx, work.ID, authors[mt], payloads[mt])
		require.NoError(t, err, mt)
		assert.Equal(t, mt, posted.Type)
		assert.Equal(t, authors[mt], posted.Author)
		assert.NotEmpty(t, posted.CommentID)
	}
	msgs, err := c.Messages(ctx, work.ID)
	require.NoError(t, err)
	require.Len(t, msgs, len(domain.MessageTypes()), "the prose comment is skipped")
	for k, mt := range domain.MessageTypes() {
		assert.Equal(t, mt, msgs[k].Type)
		assert.Nil(t, msgs[k].Err)
		assert.Equal(t, payloads[mt], msgs[k].Payload, "%s round-trips through bd", mt)
		assert.Equal(t, authors[mt], msgs[k].Author)
		assert.Equal(t, Headline(mt, payloads[mt]), msgs[k].Prose)
	}

	shown, err := c.Show(ctx, work.ID)
	require.NoError(t, err)
	require.Len(t, shown, 1)
	assert.Len(t, ParseMessages(shown[0].Comments), len(msgs), "show --include-comments carries the same messages")
	timeline := FormatTimeline(shown[0], msgs)
	t.Log("\n" + timeline)
	assert.True(t, strings.HasPrefix(timeline, string(work.ID)+"  Integration work\n  status open; run it1; round 2"), timeline)
	for _, mt := range domain.MessageTypes() {
		assert.Contains(t, timeline, Headline(mt, payloads[mt]))
	}
	assert.Contains(t, timeline, "created (task, p2)")

	// A bead in the repository's own status decodes and converts safely
	// (P3-14) and never shows up in the run's listing.
	other, err := c.Create(ctx, BeadInput{Title: "Someone else's bead", Priority: domain.PriorityDefault})
	require.NoError(t, err)
	_, err = c.Update(ctx, other.ID, Patch{Status: domain.BeadState("triage")})
	require.NoError(t, err)
	m, err := c.StatusMap(ctx)
	require.NoError(t, err)
	all, err := c.List(ctx, ListFilter{All: true})
	require.NoError(t, err)
	var seen bool
	for _, is := range all {
		if is.ID != other.ID {
			continue
		}
		seen = true
		assert.Equal(t, "triage", is.Status)
		assert.True(t, is.Foreign())
		_, err := is.Bead()
		assert.ErrorIs(t, err, domain.ErrUnknownValue)
		b, origin := is.SafeBead(m)
		assert.Equal(t, OriginForeign, origin)
		assert.Equal(t, domain.BeadStatePinned, b.State)
		assert.False(t, b.State.Workflow())
	}
	assert.True(t, seen)
	mine, err := c.ListRun(ctx, run, true)
	require.NoError(t, err)
	ids := IssueIDs(mine)
	assert.Contains(t, ids, epic.ID)
	assert.Contains(t, ids, work.ID)
	assert.NotContains(t, ids, other.ID)
}
