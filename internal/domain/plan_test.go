package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlanValidate(t *testing.T) {
	ok := func(key string, deps ...string) PlanBead {
		return PlanBead{Key: key, Title: "Title " + key, Acceptance: "Done when it works.", Priority: PriorityDefault, Deps: deps}
	}

	tests := []struct {
		name    string
		plan    Plan
		wantErr []string // substrings that must appear; empty means valid
	}{
		{name: "empty plan is valid", plan: Plan{}},
		{name: "single bead", plan: Plan{Beads: []PlanBead{ok("a")}}},
		{name: "chain", plan: Plan{Beads: []PlanBead{ok("a"), ok("b", "a"), ok("c", "b")}}},
		{name: "diamond", plan: Plan{Beads: []PlanBead{ok("a"), ok("b", "a"), ok("c", "a"), ok("d", "b", "c")}}},
		{name: "forward reference", plan: Plan{Beads: []PlanBead{ok("a", "b"), ok("b")}}},
		{name: "typed and sized", plan: Plan{Beads: []PlanBead{{Key: "a", Title: "t", Acceptance: "x", Type: BeadTypeFeature, Size: SizeL, Priority: PriorityLowest}}}},
		{name: "empty key", plan: Plan{Beads: []PlanBead{{Key: " ", Title: "t", Acceptance: "x"}}}, wantErr: []string{"bead 0: empty key"}},
		{name: "duplicate key", plan: Plan{Beads: []PlanBead{ok("a"), ok("a")}}, wantErr: []string{`bead "a": duplicate key`}},
		{name: "empty title", plan: Plan{Beads: []PlanBead{{Key: "a", Acceptance: "x"}}}, wantErr: []string{`bead "a": empty title`}},
		{name: "empty acceptance", plan: Plan{Beads: []PlanBead{{Key: "a", Title: "t", Acceptance: "  "}}}, wantErr: []string{`bead "a": empty acceptance criteria`}},
		{name: "priority too high", plan: Plan{Beads: []PlanBead{{Key: "a", Title: "t", Acceptance: "x", Priority: -1}}}, wantErr: []string{"priority -1 out of range 0..4"}},
		{name: "priority too low", plan: Plan{Beads: []PlanBead{{Key: "a", Title: "t", Acceptance: "x", Priority: 5}}}, wantErr: []string{"priority 5 out of range 0..4"}},
		{name: "unknown type", plan: Plan{Beads: []PlanBead{{Key: "a", Title: "t", Acceptance: "x", Type: "saga"}}}, wantErr: []string{`unknown type "saga"`}},
		{name: "unknown size", plan: Plan{Beads: []PlanBead{{Key: "a", Title: "t", Acceptance: "x", Size: "xl"}}}, wantErr: []string{`unknown size "xl"`}},
		{name: "dangling dep", plan: Plan{Beads: []PlanBead{ok("a", "zzz")}}, wantErr: []string{`bead "a": dependency "zzz" is not a key in the plan`}},
		{name: "self dep", plan: Plan{Beads: []PlanBead{ok("a", "a")}}, wantErr: []string{`bead "a": depends on itself`}},
		{name: "two-cycle", plan: Plan{Beads: []PlanBead{ok("a", "b"), ok("b", "a")}}, wantErr: []string{"dependency cycle: a -> b -> a"}},
		{name: "long cycle behind a valid prefix", plan: Plan{Beads: []PlanBead{ok("x"), ok("a", "x", "b"), ok("b", "c"), ok("c", "a")}}, wantErr: []string{"dependency cycle: a -> b -> c -> a"}},
		{name: "several errors are joined", plan: Plan{Beads: []PlanBead{{Key: "a"}, {Key: "b", Title: "t", Acceptance: "x", Deps: []string{"nope"}}}}, wantErr: []string{"empty title", "empty acceptance", `dependency "nope"`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.plan.Validate()
			if len(tc.wantErr) == 0 {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidPlan)
			for _, want := range tc.wantErr {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

func TestPlanValidateDoesNotMutate(t *testing.T) {
	p := samplePlan()
	before := roundTrip(t, p)
	require.NoError(t, p.Validate())
	assert.Equal(t, before, roundTrip(t, p))
}
