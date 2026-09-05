package beads

import (
	"context"
	"fmt"
	"strings"

	"slop-cannon/internal/domain"
)

// StatusesResult reports what EnsureStatuses found and did.
type StatusesResult struct {
	// Existing is what `status.custom` held before the call, in order.
	Existing []CustomStatus
	// Added are the D-07 statuses that were missing and got appended; nil
	// when the repository already had every one of them.
	Added []CustomStatus
	// Value is the `status.custom` value in force after the call.
	Value string
	// Statuses is `bd statuses --json` after the call.
	Statuses StatusList
}

// EnsureStatuses registers the D-07 custom statuses (in_review,
// changes_requested, approved, delivering, escalated, needs_human, failed)
// in the repository, once per run: it reads the current `status.custom`,
// unions it with WorkflowCustomStatuses (existing entries keep their
// position and category; nothing is ever removed or replaced), writes the
// merged value only when something was missing, and verifies with `bd
// statuses --json` that every workflow status is now known. A malformed
// existing value is an error rather than something to overwrite.
func (c *Client) EnsureStatuses(ctx context.Context) (StatusesResult, error) {
	var res StatusesResult
	current, err := c.ConfigGet(ctx, StatusCustomKey)
	if err != nil {
		return res, err
	}
	existing, err := ParseCustomStatuses(current)
	if err != nil {
		return res, fmt.Errorf("beads: existing %s %q is malformed, refusing to replace it: %w", StatusCustomKey, current, err)
	}
	res.Existing = existing
	res.Value = current
	merged := MergeCustomStatuses(existing, WorkflowCustomStatuses())
	if len(merged) > len(existing) {
		res.Added = merged[len(existing):]
		res.Value = FormatCustomStatuses(merged)
		if err := c.SetCustomStatuses(ctx, merged); err != nil {
			return res, err
		}
	}
	list, err := c.Statuses(ctx)
	if err != nil {
		return res, err
	}
	res.Statuses = list
	var missing []string
	for _, s := range domain.WorkflowBeadStates() {
		if !list.Has(string(s)) {
			missing = append(missing, string(s))
		}
	}
	if len(missing) > 0 {
		return res, fmt.Errorf("beads: bd does not list the %s status(es) after setting %s=%q", strings.Join(missing, ", "), StatusCustomKey, res.Value)
	}
	return res, nil
}

// SetStatus moves a bead to a D-07 workflow status with `bd update -s`.
// Only workflow states are accepted: bd's blocked, hooked, deferred and
// pinned are never set by the cannon (D-07), closed goes through Close
// (which records a reason and honours open blockers), and in_progress is
// normally reached with Claim. An invalid state is ErrInvalid without a
// bd call.
func (c *Client) SetStatus(ctx context.Context, id domain.BeadID, state domain.BeadState) (Issue, error) {
	switch {
	case !state.Workflow():
		return Issue{}, fmt.Errorf("beads: set status %s: %w: %q is not a workflow state (want one of %s)", id, ErrInvalid, string(state), joinStates(domain.WorkflowBeadStates()))
	case state == domain.BeadStateClosed:
		return Issue{}, fmt.Errorf("beads: set status %s: %w: use Close to close a bead", id, ErrInvalid)
	}
	return c.Update(ctx, id, Patch{Status: state})
}

// joinStates renders states for error messages.
func joinStates(states []domain.BeadState) string {
	parts := make([]string, len(states))
	for i, s := range states {
		parts[i] = string(s)
	}
	return strings.Join(parts, ", ")
}

// StatusOrigin classifies a bd status for the converter (P3-14).
type StatusOrigin int

// Status origins.
const (
	// OriginWorkflow is a D-07 state the cannon moves beads through.
	OriginWorkflow StatusOrigin = iota
	// OriginBuiltIn is a bd built-in the cannon only reads: blocked,
	// hooked, deferred, pinned.
	OriginBuiltIn
	// OriginForeign is a custom status the repository defined itself. Its
	// bead is mapped to a safe state (see SafeState) and never touched.
	OriginForeign
)

// String returns "workflow", "built-in" or "foreign".
func (o StatusOrigin) String() string {
	switch o {
	case OriginWorkflow:
		return "workflow"
	case OriginBuiltIn:
		return "built-in"
	case OriginForeign:
		return "foreign"
	default:
		return fmt.Sprintf("StatusOrigin(%d)", int(o))
	}
}

// StatusMap maps every status name a repository knows to its category, as
// `bd statuses --json` reports them. A nil map still resolves the states
// slop-cannon knows; foreign statuses then get the unknown-category
// mapping.
type StatusMap map[string]domain.StateCategory

// NewStatusMap builds a StatusMap from `bd statuses --json`.
func NewStatusMap(list StatusList) StatusMap {
	m := make(StatusMap, len(list.BuiltIn)+len(list.Custom))
	for _, s := range list.BuiltIn {
		m[s.Name] = domain.StateCategory(s.Category)
	}
	for _, s := range list.Custom {
		m[s.Name] = domain.StateCategory(s.Category)
	}
	return m
}

// StatusMap runs `bd statuses --json` and returns the repository's
// StatusMap.
func (c *Client) StatusMap(ctx context.Context) (StatusMap, error) {
	list, err := c.Statuses(ctx)
	if err != nil {
		return nil, err
	}
	return NewStatusMap(list), nil
}

// Resolve returns the domain state for a bd status name and where it came
// from (see SafeState).
func (m StatusMap) Resolve(status string) (domain.BeadState, StatusOrigin) {
	return SafeState(status, m[status])
}

// SafeState maps a bd status name to a domain state the orchestrator can
// hold without misbehaving (P3-14). A D-07 state is itself; a bd built-in
// the cannon only reads (blocked, hooked, deferred, pinned) is itself with
// OriginBuiltIn; anything else is a foreign custom status and is mapped by
// its category to a state whose Workflow() is false, so the state machine
// never transitions it, or to closed when the category is done, so a
// foreign blocker that is finished still counts as satisfied:
//
//	active  → pinned    (open in bd, but not for the cannon)
//	wip     → blocked
//	frozen  → deferred
//	done    → closed
//	unknown → deferred
//
// The original name stays in Issue.Status.
func SafeState(status string, category domain.StateCategory) (domain.BeadState, StatusOrigin) {
	if state, err := domain.ParseBeadState(status); err == nil {
		if state.Workflow() {
			return state, OriginWorkflow
		}
		return state, OriginBuiltIn
	}
	switch category {
	case domain.CategoryActive:
		return domain.BeadStatePinned, OriginForeign
	case domain.CategoryWIP:
		return domain.BeadStateBlocked, OriginForeign
	case domain.CategoryDone:
		return domain.BeadStateClosed, OriginForeign
	default:
		return domain.BeadStateDeferred, OriginForeign
	}
}

// SafeBead converts the issue to the domain model like Bead, but never
// fails on a status or type outside the domain vocabulary: a foreign
// custom status is mapped with SafeState (the origin tells the caller) and
// an unknown issue type becomes a task. Use it for listings that may
// contain beads the cannon did not create; Bead stays strict.
//
// Like Bead it returns printable prose: the title, description and
// acceptance criteria come back through domain.Bead.Sanitized, so a bead
// carrying an escape sequence cannot repaint a TUI frame or smuggle
// instructions into a prompt (P3-21).
func (i Issue) SafeBead(m StatusMap) (domain.Bead, StatusOrigin) {
	state, origin := m.Resolve(i.Status)
	j := i
	j.Status = string(state)
	if j.Type != "" {
		if _, err := domain.ParseBeadType(j.Type); err != nil {
			j.Type = string(domain.BeadTypeTask)
		}
	}
	b, err := j.Bead()
	if err != nil {
		// Unreachable: state and type are now in the vocabulary. Keep the
		// id so a caller can still report the bead.
		b = domain.Bead{ID: i.ID, Title: i.Title, State: state, Priority: i.Priority}.Sanitized()
	}
	return b, origin
}

// SafeBeads converts issues with SafeBead, in order.
func SafeBeads(issues []Issue, m StatusMap) []domain.Bead {
	out := make([]domain.Bead, len(issues))
	for k, is := range issues {
		out[k], _ = is.SafeBead(m)
	}
	return out
}

// Foreign reports whether the issue carries a status the cannon did not
// register: neither a D-07 state nor a bd built-in.
func (i Issue) Foreign() bool {
	_, origin := SafeState(i.Status, "")
	return origin == OriginForeign
}
