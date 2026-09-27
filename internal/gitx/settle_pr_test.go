package gitx

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/domain"
)

// spSetup is a PR fixture with a settler over the same repository, tracker
// and gh shim.
func spSetup(t *testing.T) (*dpFixture, *PRSettler) {
	t.Helper()
	f := dpSetup(t, true)
	s, err := NewPRSettler(f.m, f.tracker)
	require.NoError(t, err)
	s.gh = NewGH(f.m.Repo())
	return f, s
}

func TestPRSettlerInStep(t *testing.T) {
	ctx := context.Background()
	bead := domain.Bead{ID: wtBead.ID, Title: wtBead.Title}

	t.Run("a branch the remote does not have is in step", func(t *testing.T) {
		_, s := spSetup(t)
		ok, err := s.InStep(ctx, bead)
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("the same tip, or a remote tip the local branch contains, is in step", func(t *testing.T) {
		f, s := spSetup(t)
		f.r.Git(t, "push", "origin", f.wt.Branch)
		ok, err := s.InStep(ctx, bead)
		require.NoError(t, err)
		assert.True(t, ok)

		wtRepo, err := f.m.Open(ctx, wtBead.ID)
		require.NoError(t, err)
		acWrite(t, f.wt.Path, "stats/MORE.md", "local work\n")
		_, err = wtRepo.CommitAll(ctx, "more local work")
		require.NoError(t, err)
		ok, err = s.InStep(ctx, bead)
		require.NoError(t, err)
		assert.True(t, ok, "the local branch is ahead")
	})

	t.Run("a remote tip with commits the local branch lacks is not in step", func(t *testing.T) {
		f, s := spSetup(t)
		foreign := f.r.Git(t, "commit-tree", f.wt.Branch+"^{tree}", "-p", f.wt.Branch, "-m", "pushed by a human")
		f.r.Git(t, "push", "origin", foreign+":refs/heads/"+f.wt.Branch)
		ok, err := s.InStep(ctx, bead)
		require.NoError(t, err)
		assert.False(t, ok)
	})
}

func TestPRSettlerClearGate(t *testing.T) {
	ctx := context.Background()

	t.Run("no gate is nothing to do", func(t *testing.T) {
		f, s := spSetup(t)
		require.NoError(t, s.ClearGate(ctx, wtBead.ID, "safety is off"))
		assert.Empty(t, f.tracker.resolved)
	})

	t.Run("a pull request gate is resolved", func(t *testing.T) {
		f, s := spSetup(t)
		f.tracker.dpSeedGate(wtBead.ID, 7)
		require.NoError(t, s.ClearGate(ctx, wtBead.ID, "safety is off"))
		assert.Equal(t, []string{"seed-7: safety is off"}, f.tracker.resolved)
	})

	t.Run("another kind of gate is refused", func(t *testing.T) {
		f, s := spSetup(t)
		f.tracker.gates = append(f.tracker.gates, beads.Gate{ID: "timer-1", Status: "open", AwaitType: "timer", Blocks: wtBead.ID})
		err := s.ClearGate(ctx, wtBead.ID, "safety is off")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a pull request gate")
		assert.Empty(t, f.tracker.resolved)
	})
}

func TestPRSettlerClosePR(t *testing.T) {
	f, s := spSetup(t)
	require.NoError(t, s.ClosePR(context.Background(), 7, "landed as abc"))
	calls := f.gh.Invocations(t)
	require.Len(t, calls, 1)
	assert.Equal(t, []string{"pr", "close", "7", "--comment", "landed as abc"}, calls[0].Args)
}
