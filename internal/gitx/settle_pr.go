package gitx

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/domain"
)

// PRSettler settles, on a run with safety off, the pull request a bead was
// given while the run still had safety on: a run resumed with --safety off
// finds beads parked behind gh:pr gates that nothing will ever poll. It
// clears the gate so the bead can close, and closes a pull request whose
// work the run landed on the default branch itself.
type PRSettler struct {
	m       *Manager
	tracker PRTracker
	gh      *GH
}

// NewPRSettler returns the settler for m's repository.
func NewPRSettler(m *Manager, tracker PRTracker) (*PRSettler, error) {
	if m == nil {
		return nil, errors.New("gitx: NewPRSettler: nil manager")
	}
	if tracker == nil {
		return nil, errors.New("gitx: NewPRSettler: nil tracker")
	}
	return &PRSettler{m: m, tracker: tracker, gh: NewGH(m.Repo())}, nil
}

// InStep reports whether the bead's local branch holds everything its copy
// on the remote does: the remote has no such branch, has the same tip, or
// has a tip the local branch already contains. A pull request someone
// pushed to after sc opened it is not in step, and landing the local branch
// would silently leave their commits behind.
func (s *PRSettler) InStep(ctx context.Context, bead domain.Bead) (bool, error) {
	repo := s.m.Repo()
	branch, exists, err := s.m.BranchFor(ctx, bead)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, fmt.Errorf("%w: %s in %s", ErrBranchMissing, branch, repo.Root)
	}
	remote, err := repo.DefaultRemote(ctx)
	if errors.Is(err, ErrNoRemote) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	out, err := repo.Git(ctx, "ls-remote", remote, "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return true, nil
	}
	remoteTip := fields[0]
	local, err := repo.RevParse(ctx, "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	if remoteTip == local {
		return true, nil
	}
	// A remote tip this clone has never fetched cannot be in the local
	// history.
	known, err := repo.refResolves(ctx, remoteTip+"^{commit}")
	if err != nil || !known {
		return false, err
	}
	return repo.isAncestor(ctx, remoteTip, local)
}

// ClearGate resolves the gh:pr gate blocking the bead, if there is one. A
// gate of another type is not delivery's to clear, so it is an error.
func (s *PRSettler) ClearGate(ctx context.Context, bead domain.BeadID, reason string) error {
	g, err := s.tracker.GateFor(ctx, bead)
	if errors.Is(err, beads.ErrNoGate) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, ok := g.PRNumber(); !ok {
		return fmt.Errorf("gitx: %s is blocked by gate %s (%s %s), which is not a pull request gate; leaving it alone",
			bead, g.ID, g.AwaitType, g.AwaitID)
	}
	return s.tracker.ResolveGate(ctx, g.ID, reason)
}

// ClosePR closes pull request number with a comment saying why.
func (s *PRSettler) ClosePR(ctx context.Context, number int, comment string) error {
	_, err := s.gh.run(ctx, "", "pr", "close", strconv.Itoa(number), "--comment", comment)
	return err
}
