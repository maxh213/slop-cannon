//go:build integration

package beads

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
)

// TestIntegrationClaimLease drives the P3-05 protocol against the real bd:
// ten goroutines race for one bead through the serialised client and
// exactly one wins; the winner renews, a loser cannot; the bead is
// released; a backdated lease is recovered on "resume"; and a
// changes_requested bead is reclaimed for the next round.
func TestIntegrationClaimLease(t *testing.T) {
	c, _ := integrationClient(t)
	ctx := context.Background()
	_, err := c.EnsureStatuses(ctx)
	require.NoError(t, err)

	bead, err := c.Create(ctx, BeadInput{Title: "Race me", Priority: domain.PriorityDefault, Labels: []string{RunLabel("r1")}, Metadata: WorkMetadata("r1", "")})
	require.NoError(t, err)

	// The race: one winner, nine ErrAlreadyClaimed.
	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.ClaimFor(ctx, bead.ID, domain.AgentID(fmt.Sprintf("eng-%d", i)))
		}(i)
	}
	wg.Wait()
	var winner domain.AgentID
	for i, err := range errs {
		if err == nil {
			require.Empty(t, winner, "two claims succeeded")
			winner = domain.AgentID(fmt.Sprintf("eng-%d", i))
			continue
		}
		assert.ErrorIs(t, err, ErrAlreadyClaimed, "goroutine %d", i)
	}
	require.NotEmpty(t, winner, "nobody won")

	shown, err := c.Show(ctx, bead.ID)
	require.NoError(t, err)
	is := shown[0]
	assert.Equal(t, "in_progress", is.Status)
	assert.Equal(t, ActorFor(winner), is.Assignee)
	assert.Equal(t, winner, is.Agent())
	holder, ok := LeaseHolder(is)
	assert.True(t, ok)
	assert.Equal(t, winner, holder)
	first, ok := is.Lease()
	require.True(t, ok, "the claim wrote sc_lease: %v", is.Metadata)
	assert.WithinDuration(t, time.Now(), first, time.Minute)
	assert.Equal(t, "r1", string(is.RunID()), "the claim merged into the existing metadata")

	// Renew: the holder can, a loser cannot.
	time.Sleep(1100 * time.Millisecond)
	renewed, err := c.Renew(ctx, bead.ID, winner)
	require.NoError(t, err)
	second, ok := renewed.Lease()
	require.True(t, ok)
	assert.True(t, second.After(first), "renew moved the lease forward: %s -> %s", first, second)
	assert.Equal(t, winner, renewed.Agent())
	_, err = c.Renew(ctx, bead.ID, "eng-nobody")
	assert.ErrorIs(t, err, ErrLeaseLost)

	// Not claimable once it moves on.
	_, err = c.SetStatus(ctx, bead.ID, domain.BeadStateInReview)
	require.NoError(t, err)
	_, err = c.ClaimFor(ctx, bead.ID, winner)
	assert.ErrorIs(t, err, ErrNotClaimable)
	_, err = c.Renew(ctx, bead.ID, winner)
	assert.ErrorIs(t, err, ErrLeaseLost)

	// Reclaim for round two from changes_requested.
	_, err = c.SetStatus(ctx, bead.ID, domain.BeadStateChangesRequested)
	require.NoError(t, err)
	_, err = c.ClaimFor(ctx, bead.ID, "eng-round2")
	assert.ErrorIs(t, err, ErrAlreadyClaimed, "the previous assignee blocks --claim")
	reclaimed, err := c.Reclaim(ctx, bead.ID, "eng-round2")
	require.NoError(t, err)
	assert.Equal(t, "in_progress", reclaimed.Status)
	assert.Equal(t, "sc:eng-round2", reclaimed.Assignee)
	assert.Equal(t, domain.AgentID("eng-round2"), reclaimed.Agent())

	// Release: back in the ready set with the lease gone.
	released, err := c.Release(ctx, bead.ID)
	require.NoError(t, err)
	assert.Equal(t, "open", released.Status)
	assert.Empty(t, released.Assignee)
	_, ok = LeaseHolder(released)
	assert.False(t, ok)
	_, ok = released.Lease()
	assert.False(t, ok)
	assert.Equal(t, "r1", string(released.RunID()), "other metadata survives the release")
	ready, err := c.Ready(ctx, "")
	require.NoError(t, err)
	assert.Contains(t, IssueIDs(ready), bead.ID)

	// Stale recovery: claim, backdate the lease, "resume".
	_, err = c.ClaimFor(ctx, bead.ID, "eng-crashed")
	require.NoError(t, err)
	other, err := c.Create(ctx, BeadInput{Title: "Still alive", Priority: domain.PriorityDefault, Labels: []string{RunLabel("r1")}, Metadata: WorkMetadata("r1", "")})
	require.NoError(t, err)
	_, err = c.ClaimFor(ctx, other.ID, "eng-alive")
	require.NoError(t, err)
	_, err = c.SetMetadata(ctx, bead.ID, map[string]any{MetaLease: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)})
	require.NoError(t, err)

	alive := func(a domain.AgentID) bool { return a == "eng-alive" }
	dry, err := c.RecoverStale(ctx, RecoverOptions{MaxAge: time.Hour, Run: "r1", Alive: alive, DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, 2, dry.Checked)
	assert.Equal(t, []domain.BeadID{bead.ID}, claimStaleIDs(dry.Stale))
	assert.Empty(t, dry.Released)

	res, err := c.RecoverStale(ctx, RecoverOptions{MaxAge: time.Hour, Run: "r1", Alive: alive})
	require.NoError(t, err)
	assert.Equal(t, []domain.BeadID{bead.ID}, res.IDs())
	assert.Empty(t, res.Failed)
	assert.Equal(t, domain.AgentID("eng-crashed"), res.Released[0].Agent)
	assert.Greater(t, res.Released[0].Age, time.Hour)

	shown, err = c.Show(ctx, bead.ID, other.ID)
	require.NoError(t, err)
	byID := map[domain.BeadID]Issue{}
	for _, is := range shown {
		byID[is.ID] = is
	}
	assert.Equal(t, "open", byID[bead.ID].Status)
	assert.Empty(t, byID[bead.ID].Assignee)
	assert.Equal(t, "in_progress", byID[other.ID].Status, "a live agent keeps its bead")
	assert.Equal(t, domain.AgentID("eng-alive"), byID[other.ID].Agent())
	ready, err = c.Ready(ctx, "")
	require.NoError(t, err)
	assert.Contains(t, IssueIDs(ready), bead.ID, "the recovered bead is claimable again")

	// Resume semantics: nobody alive, any age.
	res, err = c.RecoverStale(ctx, RecoverOptions{})
	require.NoError(t, err)
	assert.Equal(t, []domain.BeadID{other.ID}, res.IDs())
	_, err = c.ClaimFor(ctx, other.ID, "eng-next")
	require.NoError(t, err, "a recovered bead can be claimed by a new agent")
}
