package beads

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"slop-cannon/internal/domain"
)

// Lease timing defaults (P3-05). A claim carries an sc_lease timestamp
// that the supervisor renews while the agent runs; a bead whose lease is
// older than the max age and whose agent is gone is handed back to the
// ready set by RecoverStale. The supervisor may use its own intervals.
const (
	// DefaultLeaseMaxAge is how old a lease may be before a bead held by a
	// dead agent counts as abandoned.
	DefaultLeaseMaxAge = 15 * time.Minute
	// DefaultRenewInterval is how often a running agent's lease is renewed,
	// well inside DefaultLeaseMaxAge so a live agent is never recovered.
	DefaultRenewInterval = 5 * time.Minute
)

// Errors of the claim protocol, matched with errors.Is.
var (
	// ErrNotClaimable: `bd update --claim` refused because of the bead's
	// status. Verified against bd 1.2.2: a claim succeeds only when the
	// bead is open and unassigned, or in_progress and already assigned to
	// the same actor (idempotent); any other status, closed or a D-07
	// custom status included, prints "issue not claimable: status <s>".
	ErrNotClaimable = errors.New("bead is not claimable in its current status")
	// ErrLeaseLost: Renew found the bead no longer held by the agent (it
	// was released, recovered, claimed by another agent, or moved on to
	// another status). The holder must stop treating the bead as its own.
	ErrLeaseLost = errors.New("bead is no longer held by the agent")
)

// ClaimFor claims id for agent in one atomic bd call:
//
//	bd update <id> --claim --metadata @{sc_agent, sc_lease: now} --json
//
// run as actor sc:<agent> (ActorFor), so bd sets the assignee and the
// in_progress status atomically (Appendix A) and stores the lease in the
// same write; a refused claim writes no metadata (verified). Exactly one
// of several concurrent callers wins: the losers get ErrAlreadyClaimed.
// A repeated claim by the same agent while the bead is in_progress is
// idempotent and renews the lease. Beads in any other status are
// ErrNotClaimable; the previous round's assignee of a changes_requested
// bead makes bd answer ErrAlreadyClaimed instead, so a new round goes
// through Reclaim. An unknown id is ErrNotFound; an empty agent is
// ErrInvalid without a bd call.
func (c *Client) ClaimFor(ctx context.Context, id domain.BeadID, agent domain.AgentID) (Issue, error) {
	if err := checkLeaseArgs("claim", id, agent); err != nil {
		return Issue{}, err
	}
	args, cleanup, err := leaseArgs(id, "--claim", LeaseMetadata(agent, time.Now()))
	if err != nil {
		return Issue{}, err
	}
	defer cleanup()
	is, err := c.WithActor(ActorFor(agent)).callIssue(ctx, args...)
	return is, claimError(err)
}

// Reclaim hands a bead that came back from review (changes_requested, the
// `changes_requested → in_progress` edge of D-07) or a conflict round to
// agent for the next round:
//
//	bd update <id> -s in_progress --assignee sc:<agent> --metadata @{sc_agent, sc_lease: now} --json
//
// bd's atomic --claim cannot do this (ErrNotClaimable for the status, or
// ErrAlreadyClaimed because the previous engineer is still the assignee),
// so Reclaim is a plain update relying on the orchestrator being the only
// writer (D-21). It works from any non-closed status and replaces whatever
// assignee and lease the bead had; callers check the status first.
func (c *Client) Reclaim(ctx context.Context, id domain.BeadID, agent domain.AgentID) (Issue, error) {
	if err := checkLeaseArgs("reclaim", id, agent); err != nil {
		return Issue{}, err
	}
	return c.Update(ctx, id, Patch{
		Status:   domain.BeadStateInProgress,
		Assignee: ptr(ActorFor(agent)),
		Metadata: LeaseMetadata(agent, time.Now()),
	})
}

// Renew refreshes the sc_lease of a bead agent holds. It first reads the
// bead: unless it is in_progress and assigned to sc:<agent> the lease is
// gone and Renew returns ErrLeaseLost (wrapping the reason) without
// writing; then it renews with the same-actor idempotent claim,
//
//	bd update <id> --claim --metadata @{sc_lease: now} --json
//
// which bd refuses atomically should the bead have changed hands in
// between (also ErrLeaseLost). The supervisor calls this every
// DefaultRenewInterval while the agent runs.
func (c *Client) Renew(ctx context.Context, id domain.BeadID, agent domain.AgentID) (Issue, error) {
	if err := checkLeaseArgs("renew", id, agent); err != nil {
		return Issue{}, err
	}
	issues, err := c.Show(ctx, id)
	if err != nil {
		return Issue{}, err
	}
	is := issues[0]
	if is.Status != string(domain.BeadStateInProgress) || is.Assignee != ActorFor(agent) {
		return is, fmt.Errorf("beads: renew %s: %w: status %s, assignee %q", id, ErrLeaseLost, is.Status, is.Assignee)
	}
	args, cleanup, err := leaseArgs(id, "--claim", map[string]any{MetaLease: time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		return Issue{}, err
	}
	defer cleanup()
	is, err = c.WithActor(ActorFor(agent)).callIssue(ctx, args...)
	if err = claimError(err); err != nil {
		if errors.Is(err, ErrAlreadyClaimed) || errors.Is(err, ErrNotClaimable) {
			return Issue{}, fmt.Errorf("beads: renew %s: %w: %w", id, ErrLeaseLost, err)
		}
		return Issue{}, err
	}
	return is, nil
}

// Release returns a bead to the ready set in one bd call:
//
//	bd update <id> -s open --assignee "" --unset-metadata sc_agent --unset-metadata sc_lease --json
//
// (the D-07 release, verified as a single update). It works from any
// non-closed status and does not check who held the bead; callers that
// care compare Issue.Agent() first. Releasing an open bead is harmless.
func (c *Client) Release(ctx context.Context, id domain.BeadID) (Issue, error) {
	if id == "" {
		return Issue{}, fmt.Errorf("beads: release: %w: empty bead id", ErrInvalid)
	}
	return c.Update(ctx, id, Patch{
		Status:        domain.BeadStateOpen,
		Assignee:      ptr(""),
		UnsetMetadata: append([]string(nil), LeaseKeys...),
	})
}

// LeaseHolder returns the agent holding the bead's claim: the sc_agent
// metadata, else the agent behind an sc:<agent-id> assignee. A bead held
// by nobody, or by an actor that is not a slop-cannon agent, yields false.
func LeaseHolder(is Issue) (domain.AgentID, bool) {
	if agent := is.Agent(); agent != "" {
		return agent, true
	}
	return AgentOf(is.Assignee)
}

// LeaseTime returns when the bead's claim was last renewed: the sc_lease
// metadata, else bd's started_at (set by --claim), else updated_at. It is
// false only when bd printed none of them.
func LeaseTime(is Issue) (time.Time, bool) {
	if at, ok := is.Lease(); ok {
		return at, true
	}
	if !is.StartedAt.IsZero() {
		return is.StartedAt, true
	}
	if !is.UpdatedAt.IsZero() {
		return is.UpdatedAt, true
	}
	return time.Time{}, false
}

// RecoverOptions controls RecoverStale and StaleLeases.
type RecoverOptions struct {
	// MaxAge is how old a lease must be for its bead to count as stale.
	// Zero or negative means any age: every bead held by an agent that is
	// not alive is stale, which is what a resume wants (nothing from the
	// previous process survived).
	MaxAge time.Duration
	// Run limits recovery to the beads of one run (the sc:run label);
	// empty considers every in_progress bead the cannon holds.
	Run domain.RunID
	// Alive reports whether an agent is still running; a live agent's bead
	// is never recovered however old its lease. nil means no agent is
	// alive.
	Alive func(domain.AgentID) bool
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// DryRun reports the stale beads without releasing them.
	DryRun bool
}

// StaleLease describes one bead held past its lease by an agent that is
// gone.
type StaleLease struct {
	// ID is the bead.
	ID domain.BeadID
	// Agent is the holder (LeaseHolder); Assignee is bd's raw assignee.
	Agent    domain.AgentID
	Assignee string
	// LeaseAt is the last renewal (LeaseTime); zero when bd printed no
	// timestamp at all, in which case Age is zero too and the bead counts
	// as stale for its dead agent alone.
	LeaseAt time.Time
	// Age is now minus LeaseAt.
	Age time.Duration
}

// String renders "fx-1 held by eng-1 for 20m0s".
func (s StaleLease) String() string {
	if s.LeaseAt.IsZero() {
		return fmt.Sprintf("%s held by %s with no lease", s.ID, s.Agent)
	}
	return fmt.Sprintf("%s held by %s for %s", s.ID, s.Agent, s.Age.Round(time.Second))
}

// RecoverFailure is a stale bead whose release failed.
type RecoverFailure struct {
	StaleLease
	Err error
}

// RecoverResult reports what RecoverStale found and did.
type RecoverResult struct {
	// Checked is the number of in_progress beads that were examined.
	Checked int
	// Stale are the beads that qualified, in listing order.
	Stale []StaleLease
	// Released are the stale beads that were returned to open (empty on a
	// dry run).
	Released []StaleLease
	// Failed are the stale beads whose release failed; RecoverStale goes
	// on with the rest.
	Failed []RecoverFailure
	// DryRun echoes the option.
	DryRun bool
}

// IDs returns the ids of the released beads.
func (r RecoverResult) IDs() []domain.BeadID {
	out := make([]domain.BeadID, len(r.Released))
	for k, s := range r.Released {
		out[k] = s.ID
	}
	return out
}

// StaleLeases picks the beads of issues that RecoverStale would release:
// status in_progress, held by a slop-cannon agent (LeaseHolder), that
// agent not alive, and the lease (LeaseTime) older than opts.MaxAge or
// absent. Beads held by other actors are never touched. Pure; the run
// filter is not applied here.
func StaleLeases(issues []Issue, opts RecoverOptions) []StaleLease {
	now := time.Now()
	if opts.Now != nil {
		now = opts.Now()
	}
	var out []StaleLease
	for _, is := range issues {
		if is.Status != string(domain.BeadStateInProgress) {
			continue
		}
		agent, ok := LeaseHolder(is)
		if !ok || (opts.Alive != nil && opts.Alive(agent)) {
			continue
		}
		s := StaleLease{ID: is.ID, Agent: agent, Assignee: is.Assignee}
		if at, ok := LeaseTime(is); ok {
			s.LeaseAt = at
			s.Age = now.Sub(at)
			if opts.MaxAge > 0 && s.Age <= opts.MaxAge {
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

// RecoverStale re-opens the beads whose lease has expired while their
// agent is gone: it lists the in_progress beads (`bd list -s in_progress`,
// narrowed to opts.Run when set), selects with StaleLeases, and calls
// Release on each, continuing past individual failures. Used on resume
// (Alive nil, MaxAge 0) and periodically by the supervisor (Alive = its
// process table, MaxAge = DefaultLeaseMaxAge). Only a failure to list is
// an error.
func (c *Client) RecoverStale(ctx context.Context, opts RecoverOptions) (RecoverResult, error) {
	filter := ListFilter{Statuses: []domain.BeadState{domain.BeadStateInProgress}}
	if opts.Run != "" {
		filter.Labels = []string{RunLabel(opts.Run)}
	}
	issues, err := c.List(ctx, filter)
	if err != nil {
		return RecoverResult{}, err
	}
	if opts.Run != "" {
		kept := issues[:0]
		for _, is := range issues {
			if is.InRun(opts.Run) {
				kept = append(kept, is)
			}
		}
		issues = kept
	}
	res := RecoverResult{Checked: len(issues), Stale: StaleLeases(issues, opts), DryRun: opts.DryRun}
	if opts.DryRun {
		return res, nil
	}
	for _, s := range res.Stale {
		if err := ctx.Err(); err != nil {
			res.Failed = append(res.Failed, RecoverFailure{StaleLease: s, Err: err})
			continue
		}
		if _, err := c.Release(ctx, s.ID); err != nil {
			c.logger.WarnContext(ctx, "stale bead release failed", "bead", string(s.ID), "agent", string(s.Agent), "error", err.Error())
			res.Failed = append(res.Failed, RecoverFailure{StaleLease: s, Err: err})
			continue
		}
		c.logger.InfoContext(ctx, "stale bead released", "bead", string(s.ID), "agent", string(s.Agent), "age", s.Age.Round(time.Second).String())
		res.Released = append(res.Released, s)
	}
	return res, nil
}

// checkLeaseArgs validates the id and agent of a lease operation.
func checkLeaseArgs(op string, id domain.BeadID, agent domain.AgentID) error {
	if id == "" {
		return fmt.Errorf("beads: %s: %w: empty bead id", op, ErrInvalid)
	}
	if strings.TrimSpace(string(agent)) == "" {
		return fmt.Errorf("beads: %s %s: %w: empty agent id", op, id, ErrInvalid)
	}
	return nil
}

// leaseArgs builds `update <id> <flag> --metadata @file --json` with meta
// written to a temporary file (keys sorted for stable output), returning
// the cleanup for the file.
func leaseArgs(id domain.BeadID, flag string, meta map[string]any) ([]string, func(), error) {
	path, cleanup, err := writeJSONFile("sc-lease-*.json", meta)
	if err != nil {
		return nil, nil, err
	}
	return []string{"update", string(id), flag, "--metadata", "@" + path, "--json"}, cleanup, nil
}

// claimError refines the *Error of a refused claim: bd 1.2.2's "issue not
// claimable: status <s>" matches no kind in errors.go, so it becomes
// ErrNotClaimable here. Other errors pass through unchanged.
func claimError(err error) error {
	var e *Error
	if errors.As(err, &e) && e.Kind == nil && strings.Contains(strings.ToLower(e.Message), "not claimable") {
		e.Kind = ErrNotClaimable
	}
	return err
}

// ptr returns a pointer to s, for Patch fields that distinguish unset
// from empty.
func ptr(s string) *string { return &s }
