package cli

import (
	"context"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/gitx"
	"slop-cannon/internal/orchestrator"
)

// The secrets filter on the external pull request path (P9-05, D-11).
//
// A run's own diffs are filtered where the conductor builds them, which
// covers every prompt the feedback loop assembles. The external review path
// does not go through the conductor for its change set: the reviewer checks
// the pull request out into a throwaway worktree and takes its own diff
// against the base ([orchestrator.PRWorkspace.Diff]), and that diff is what
// the tester's prompt quotes. It is exactly the diff most likely to carry
// somebody else's `.env` — it is a contributor's branch, reviewed by us,
// with none of the discipline our own engineers' prompts impose.
//
// So the checkout is decorated here, at the one place `sc run` builds it.
// Filtering the workspace rather than the request keeps the seam narrow: a
// [orchestrator.PRCheckout] is the only thing that produces a pull
// request's change set, so nothing downstream can quote an unfiltered one.

// prSecretCheckout is an [orchestrator.PRCheckout] whose workspaces have
// been through the run's secrets filter.
type prSecretCheckout struct {
	inner  orchestrator.PRCheckout
	filter *agents.SecretFilter
}

// filterPRDiffs wraps a checkout so the change set it hands the reviewer has
// every excluded file replaced by [agents.SecretPlaceholder]. A nil checkout
// stays nil, which the reviewer reads as "no checkout configured"
// ([orchestrator.ErrNoPRCheckout]); a nil filter still applies
// [agents.DefaultSecretPatterns], because the built-in patterns are the
// floor rather than an opt-in.
func filterPRDiffs(inner orchestrator.PRCheckout, filter *agents.SecretFilter) orchestrator.PRCheckout {
	if inner == nil {
		return nil
	}
	return prSecretCheckout{inner: inner, filter: filter}
}

// Checkout implements [orchestrator.PRCheckout]: the inner checkout does the
// git work, and the diff it produces is filtered before anybody sees it.
func (c prSecretCheckout) Checkout(ctx context.Context, bead domain.Bead, pr gitx.PullRequest) (orchestrator.PRWorkspace, error) {
	ws, err := c.inner.Checkout(ctx, bead, pr)
	if err != nil {
		return ws, err
	}
	ws.Diff = c.filter.GitDiff(ws.Diff)
	return ws, nil
}

// prSecretCheckout satisfies the seam it decorates.
var _ orchestrator.PRCheckout = prSecretCheckout{}
