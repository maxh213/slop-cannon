package domain

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidPlan is wrapped by every Plan.Validate failure.
var ErrInvalidPlan = errors.New("invalid plan")

// PlanBead is one proposed bead in a PO plan. Keys are plan-local; the
// beads client maps them to minted BeadIDs when the plan is applied (D-22).
type PlanBead struct {
	// Key is the plan-local identifier other beads use in Deps.
	Key string `json:"key"`
	// Title is the one-line summary.
	Title string `json:"title"`
	// Description explains the work.
	Description string `json:"description,omitempty"`
	// Acceptance holds the acceptance criteria; must not be empty.
	Acceptance string `json:"acceptance"`
	// Type is the bd issue type; empty means task.
	Type BeadType `json:"type,omitempty"`
	// Priority is on bd's 0 (highest) to 4 (lowest) scale.
	Priority Priority `json:"priority"`
	// Size is the S/M/L estimate.
	Size Size `json:"size,omitempty"`
	// Deps lists the keys this bead depends on.
	Deps []string `json:"deps,omitempty"`
}

// Plan is the PO's proposal: a short brief plus the beads to create.
type Plan struct {
	// Brief is the PO's summary of what the plan achieves.
	Brief string `json:"brief,omitempty"`
	// Beads are the proposed beads in the PO's preferred order.
	Beads []PlanBead `json:"beads"`
}

// Validate checks the structural rules a plan must satisfy before it is
// turned into beads: keys and titles present, keys unique, acceptance
// criteria non-empty, priorities in range, enums valid, every dependency
// naming a key in the plan, and no dependency cycles. An empty plan is valid
// (autopilot uses it to mean "nothing to add", D-18). Every error wraps
// ErrInvalidPlan.
func (p Plan) Validate() error {
	var errs []error
	index := make(map[string]int, len(p.Beads))
	for i, b := range p.Beads {
		where := fmt.Sprintf("bead %d", i)
		if strings.TrimSpace(b.Key) == "" {
			errs = append(errs, fmt.Errorf("%s: empty key", where))
		} else {
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
		if b.Type != "" && !b.Type.Valid() {
			errs = append(errs, fmt.Errorf("%s: unknown type %q", where, string(b.Type)))
		}
		if b.Size != "" && !b.Size.Valid() {
			errs = append(errs, fmt.Errorf("%s: unknown size %q", where, string(b.Size)))
		}
	}
	for _, b := range p.Beads {
		for _, d := range b.Deps {
			if _, ok := index[d]; !ok {
				errs = append(errs, fmt.Errorf("bead %q: dependency %q is not a key in the plan", b.Key, d))
			} else if d == b.Key {
				errs = append(errs, fmt.Errorf("bead %q: depends on itself", b.Key))
			}
		}
	}
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
