package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// ErrInvalidPlan is wrapped by every Plan.Validate failure.
var ErrInvalidPlan = errors.New("invalid plan")

// PlanBead is one proposed bead in a PO plan. Keys are plan-local; the
// beads client maps them to minted BeadIDs when the plan is applied (D-22).
type PlanBead struct {
	// Key is the plan-local identifier other beads use in Deps. It must be
	// non-empty and contain no whitespace; it becomes the graph-plan node
	// key and the from_key/to_key of every edge (D-24).
	Key string `json:"key"`
	// Title is the one-line summary.
	Title string `json:"title"`
	// Description explains the work.
	Description string `json:"description,omitempty"`
	// Acceptance holds the acceptance criteria; must not be empty.
	Acceptance string `json:"acceptance"`
	// Type is the bd issue type; empty means task. Only Plannable types
	// (task, bug, feature, chore) are allowed.
	Type BeadType `json:"type,omitempty"`
	// Priority is on bd's 0 (highest) to 4 (lowest) scale. When decoded from
	// JSON a missing, null or empty value means PriorityDefault (D-22).
	Priority Priority `json:"priority"`
	// Size is the S/M/L estimate.
	Size Size `json:"size,omitempty"`
	// Deps lists the keys this bead depends on, each at most once.
	Deps []string `json:"deps,omitempty"`
	// Timeout, when set, is a longer agent session limit the product owner
	// asks for because one session on this bead will need it (a Go
	// duration such as "10h"). The run caps it at limits.max_agent_timeout
	// and never goes below limits.agent_timeout.
	Timeout string `json:"timeout,omitempty"`
}

// UnmarshalJSON implements json.Unmarshaler. It decodes like the plain struct
// except that a missing "priority" yields PriorityDefault rather than
// PriorityHighest, so a PO plan that omits priorities is not silently
// top-urgency (D-22).
func (b *PlanBead) UnmarshalJSON(data []byte) error {
	type plain PlanBead // no methods, so the default decoder is used
	v := plain{Priority: PriorityDefault}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*b = PlanBead(v)
	return nil
}

// DependencyCount returns the number of dependency edges in the plan: the
// sum of len(Deps) over all beads. P3-10 checks it against the edge_count
// reported by `bd create --graph --dry-run` (D-24); Validate guarantees the
// two agree by rejecting duplicate and dangling dependencies.
func (p Plan) DependencyCount() int {
	n := 0
	for _, b := range p.Beads {
		n += len(b.Deps)
	}
	return n
}

// Plan is the PO's proposal: a short brief plus the beads to create.
type Plan struct {
	// Brief is the PO's summary of what the plan achieves.
	Brief string `json:"brief,omitempty"`
	// Beads are the proposed beads in the PO's preferred order.
	Beads []PlanBead `json:"beads"`
}

// PlanCandidate is one planner's contribution to a combined plan. Planner
// orders otherwise independent producers; Plan.Keys remain local until the
// candidate is namespaced by the planning reconciler.
type PlanCandidate struct {
	Planner AgentID `json:"planner"`
	Plan    Plan    `json:"plan"`
}

// PlanReviewDecision is the complete vocabulary a plan judge may use. It
// classifies work; it does not edit a candidate or the dependency graph.
type PlanReviewDecision string

const (
	// PlanReviewIndependent accepts a candidate as distinct work.
	PlanReviewIndependent PlanReviewDecision = "independent"
	// PlanReviewDuplicate resolves a candidate to an existing canonical bead.
	PlanReviewDuplicate PlanReviewDecision = "duplicate"
	// PlanReviewConflict rejects a candidate whose work cannot be reconciled safely.
	PlanReviewConflict PlanReviewDecision = "conflict"
)

// Valid reports whether d is a judge decision.
func (d PlanReviewDecision) Valid() bool {
	return d == PlanReviewIndependent || d == PlanReviewDuplicate || d == PlanReviewConflict
}

// PlanReviewProvenance says whether a decision came from deterministic
// matching, the judge, or the no-retry fallback used when judging fails.
type PlanReviewProvenance string

const (
	// PlanProvenanceExisting marks an exact match to a fresh existing bead.
	PlanProvenanceExisting PlanReviewProvenance = "existing"
	// PlanProvenanceDeterministic marks an exact match between candidates.
	PlanProvenanceDeterministic PlanReviewProvenance = "deterministic"
	// PlanProvenanceJudge marks a semantic PO judge classification.
	PlanProvenanceJudge PlanReviewProvenance = "judge"
	// PlanProvenanceFallback marks a deterministic no-retry fallback decision.
	PlanProvenanceFallback PlanReviewProvenance = "fallback"
)

// Valid reports whether p is a known decision source.
func (p PlanReviewProvenance) Valid() bool {
	return p == PlanProvenanceExisting || p == PlanProvenanceDeterministic ||
		p == PlanProvenanceJudge || p == PlanProvenanceFallback
}

// MaxPlanReviewReferenceLength bounds candidate keys and canonical references
// in review decisions and their events.
const MaxPlanReviewReferenceLength = 256

// PlanReview is one bounded, prose-free candidate classification. Candidate
// and Canonical are namespaced plan keys; Canonical may instead be an
// existing bead ID for a duplicate. It is empty for independent and conflict
// decisions.
type PlanReview struct {
	Planner    AgentID              `json:"planner"`
	Candidate  string               `json:"candidate"`
	Decision   PlanReviewDecision   `json:"decision"`
	Canonical  string               `json:"canonical,omitempty"`
	Provenance PlanReviewProvenance `json:"provenance"`
}

// PlanExternalDependency is an edge from a new plan bead to an existing
// canonical bead. Keeping these edges outside PlanBead.Deps prevents an
// agent-produced local key from being mistaken for a tracker ID.
type PlanExternalDependency struct {
	FromKey string `json:"from_key"`
	ToID    BeadID `json:"to_id"`
}

// CombinedPlan is a reconciled set of new beads plus edges to existing
// canonical beads. Existing beads are references only: no field here can
// change their state, priority, title, or acceptance criteria.
type CombinedPlan struct {
	Plan         Plan                     `json:"plan"`
	ExternalDeps []PlanExternalDependency `json:"external_deps,omitempty"`
}

// DependencyCount includes both local and existing-bead edges.
func (p CombinedPlan) DependencyCount() int {
	return p.Plan.DependencyCount() + len(p.ExternalDeps)
}

// Validate checks the new plan and every external edge. Local cycles are
// checked by Plan.Validate; existing beads cannot introduce a new cycle
// because none of their edges are rewritten.
func (p CombinedPlan) Validate() error {
	if err := p.Plan.Validate(); err != nil {
		return err
	}
	keys := make(map[string]bool, len(p.Plan.Beads))
	for _, b := range p.Plan.Beads {
		keys[b.Key] = true
	}
	seen := make(map[PlanExternalDependency]bool, len(p.ExternalDeps))
	var errs []error
	for _, edge := range p.ExternalDeps {
		switch {
		case !keys[edge.FromKey]:
			errs = append(errs, fmt.Errorf("external dependency source %q is not a key in the plan", edge.FromKey))
		case strings.TrimSpace(string(edge.ToID)) == "" || strings.ContainsFunc(string(edge.ToID), unicode.IsSpace):
			errs = append(errs, fmt.Errorf("bead %q: malformed external dependency %q", edge.FromKey, edge.ToID))
		case seen[edge]:
			errs = append(errs, fmt.Errorf("bead %q: duplicate external dependency %q", edge.FromKey, edge.ToID))
		}
		seen[edge] = true
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrInvalidPlan, errors.Join(errs...))
	}
	return nil
}

// Validate checks the structural rules a plan must satisfy before it is
// turned into beads: keys present, free of whitespace and unique; titles
// present; acceptance criteria non-empty; priorities in range; types
// Plannable and sizes valid; every dependency naming a key in the plan, no
// bead depending on itself or listing the same dependency twice; and no
// dependency cycles. The brief and every bead's title, description and
// acceptance criteria must also be free of control characters (P0-11): a
// title carrying "\x1b[2J" is rejected here rather than left to repaint a
// terminal further down the pipe; see CheckText and CheckLine. An empty plan
// is valid (autopilot uses it to mean
// "nothing to add", D-18). Every error wraps ErrInvalidPlan. Validate never
// normalises values: a key with surrounding whitespace is rejected, not
// trimmed, so the keys the caller sees are the keys bd will see.
func (p Plan) Validate() error {
	var errs []error
	index := make(map[string]int, len(p.Beads))
	for i, b := range p.Beads {
		where := fmt.Sprintf("bead %d", i)
		switch {
		case strings.TrimSpace(b.Key) == "":
			errs = append(errs, fmt.Errorf("%s: empty key", where))
		case strings.ContainsFunc(b.Key, unicode.IsSpace):
			errs = append(errs, fmt.Errorf("%s: malformed key %q (must not contain whitespace)", where, b.Key))
		default:
			where = fmt.Sprintf("bead %q", b.Key)
			if _, dup := index[b.Key]; dup {
				errs = append(errs, fmt.Errorf("%s: duplicate key", where))
			} else {
				index[b.Key] = i
			}
		}
		if strings.TrimSpace(b.Title) == "" {
			errs = append(errs, fmt.Errorf("%s: empty title", where))
		}
		if strings.TrimSpace(b.Acceptance) == "" {
			errs = append(errs, fmt.Errorf("%s: empty acceptance criteria", where))
		}
		if !b.Priority.Valid() {
			errs = append(errs, fmt.Errorf("%s: priority %d out of range %d..%d", where, b.Priority, PriorityHighest, PriorityLowest))
		}
		if b.Type != "" && !b.Type.Plannable() {
			errs = append(errs, fmt.Errorf("%s: type %q is not allowed in a plan (want one of %s)", where, string(b.Type), joinBeadTypes(PlannableBeadTypes())))
		}
		if b.Size != "" && !b.Size.Valid() {
			errs = append(errs, fmt.Errorf("%s: unknown size %q", where, string(b.Size)))
		}
		if b.Timeout != "" {
			if d, err := time.ParseDuration(b.Timeout); err != nil || d <= 0 {
				errs = append(errs, fmt.Errorf("%s: timeout %q is not a positive Go duration such as \"10h\"", where, b.Timeout))
			}
		}
	}
	for _, b := range p.Beads {
		seen := make(map[string]bool, len(b.Deps))
		for _, d := range b.Deps {
			switch {
			case seen[d]:
				errs = append(errs, fmt.Errorf("bead %q: duplicate dependency %q", b.Key, d))
			case d == b.Key:
				errs = append(errs, fmt.Errorf("bead %q: depends on itself", b.Key))
			default:
				if _, ok := index[d]; !ok {
					errs = append(errs, fmt.Errorf("bead %q: dependency %q is not a key in the plan", b.Key, d))
				}
			}
			seen[d] = true
		}
	}
	// Control characters in the brief and in every bead's prose (P0-11).
	errs = append(errs, planTextProblems(p)...)
	if len(errs) == 0 {
		if cycle := p.findCycle(index); cycle != nil {
			errs = append(errs, fmt.Errorf("dependency cycle: %s", strings.Join(cycle, " -> ")))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrInvalidPlan, errors.Join(errs...))
}

// findCycle returns the keys of one dependency cycle (first key repeated at
// the end), or nil when the dependency graph is acyclic. It assumes every
// dependency resolves in index.
func (p Plan) findCycle(index map[string]int) []string {
	const (
		unvisited = iota
		visiting
		done
	)
	state := make([]int, len(p.Beads))
	var stack []string
	var visit func(i int) []string
	visit = func(i int) []string {
		state[i] = visiting
		stack = append(stack, p.Beads[i].Key)
		for _, d := range p.Beads[i].Deps {
			j := index[d]
			switch state[j] {
			case visiting:
				for k, key := range stack {
					if key == d {
						cycle := append([]string(nil), stack[k:]...)
						return append(cycle, d)
					}
				}
			case unvisited:
				if c := visit(j); c != nil {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[i] = done
		return nil
	}
	for i := range p.Beads {
		if state[i] == unvisited {
			if c := visit(i); c != nil {
				return c
			}
		}
	}
	return nil
}

// joinBeadTypes renders types for error messages.
func joinBeadTypes(ts []BeadType) string {
	parts := make([]string, len(ts))
	for i, t := range ts {
		parts[i] = string(t)
	}
	return strings.Join(parts, ", ")
}
