package domain

import (
	"encoding/json"
	"strings"
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
		{name: "every plannable type", plan: Plan{Beads: []PlanBead{
			{Key: "a", Title: "t", Acceptance: "x", Type: BeadTypeTask},
			{Key: "b", Title: "t", Acceptance: "x", Type: BeadTypeBug},
			{Key: "c", Title: "t", Acceptance: "x", Type: BeadTypeFeature},
			{Key: "d", Title: "t", Acceptance: "x", Type: BeadTypeChore},
		}}},
		{name: "empty key", plan: Plan{Beads: []PlanBead{{Key: " ", Title: "t", Acceptance: "x"}}}, wantErr: []string{"bead 0: empty key"}},
		{name: "key with leading space is rejected not trimmed", plan: Plan{Beads: []PlanBead{ok(" a"), ok("b", "a")}}, wantErr: []string{`bead 0: malformed key " a" (must not contain whitespace)`, `bead "b": dependency "a" is not a key in the plan`}},
		{name: "key with inner whitespace", plan: Plan{Beads: []PlanBead{ok("a b")}}, wantErr: []string{`bead 0: malformed key "a b"`}},
		{name: "key with tab", plan: Plan{Beads: []PlanBead{ok("a\t")}}, wantErr: []string{`malformed key "a\t"`}},
		{name: "duplicate key", plan: Plan{Beads: []PlanBead{ok("a"), ok("a")}}, wantErr: []string{`bead "a": duplicate key`}},
		{name: "empty title", plan: Plan{Beads: []PlanBead{{Key: "a", Acceptance: "x"}}}, wantErr: []string{`bead "a": empty title`}},
		{name: "empty acceptance", plan: Plan{Beads: []PlanBead{{Key: "a", Title: "t", Acceptance: "  "}}}, wantErr: []string{`bead "a": empty acceptance criteria`}},
		{name: "priority too high", plan: Plan{Beads: []PlanBead{{Key: "a", Title: "t", Acceptance: "x", Priority: -1}}}, wantErr: []string{"priority -1 out of range 0..4"}},
		{name: "priority too low", plan: Plan{Beads: []PlanBead{{Key: "a", Title: "t", Acceptance: "x", Priority: 5}}}, wantErr: []string{"priority 5 out of range 0..4"}},
		{name: "unknown type", plan: Plan{Beads: []PlanBead{{Key: "a", Title: "t", Acceptance: "x", Type: "saga"}}}, wantErr: []string{`type "saga" is not allowed in a plan (want one of task, bug, feature, chore)`}},
		{name: "valid but unplannable type", plan: Plan{Beads: []PlanBead{{Key: "a", Title: "t", Acceptance: "x", Type: BeadTypeEpic}}}, wantErr: []string{`type "epic" is not allowed in a plan`}},
		{name: "gate type", plan: Plan{Beads: []PlanBead{{Key: "a", Title: "t", Acceptance: "x", Type: BeadTypeGate}}}, wantErr: []string{`type "gate" is not allowed in a plan`}},
		{name: "unknown size", plan: Plan{Beads: []PlanBead{{Key: "a", Title: "t", Acceptance: "x", Size: "xl"}}}, wantErr: []string{`unknown size "xl"`}},
		{name: "dangling dep", plan: Plan{Beads: []PlanBead{ok("a", "zzz")}}, wantErr: []string{`bead "a": dependency "zzz" is not a key in the plan`}},
		{name: "self dep", plan: Plan{Beads: []PlanBead{ok("a", "a")}}, wantErr: []string{`bead "a": depends on itself`}},
		{name: "duplicate dep", plan: Plan{Beads: []PlanBead{ok("a"), ok("b", "a", "a")}}, wantErr: []string{`bead "b": duplicate dependency "a"`}},
		{name: "duplicate dep reported once per repeat", plan: Plan{Beads: []PlanBead{ok("a"), ok("b", "a", "a", "a")}}, wantErr: []string{`bead "b": duplicate dependency "a"`}},
		{name: "duplicate dangling dep", plan: Plan{Beads: []PlanBead{ok("a", "zzz", "zzz")}}, wantErr: []string{`dependency "zzz" is not a key in the plan`, `duplicate dependency "zzz"`}},
		{name: "dep with whitespace never matches", plan: Plan{Beads: []PlanBead{ok("a"), ok("b", " a")}}, wantErr: []string{`bead "b": dependency " a" is not a key in the plan`}},
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

func TestPlanValidateDuplicateDepReportedOnce(t *testing.T) {
	// Three mentions of "a" are one dependency plus two duplicates; the
	// error names the duplicate exactly twice, not as a self-dep or dangling.
	p := Plan{Beads: []PlanBead{
		{Key: "a", Title: "t", Acceptance: "x"},
		{Key: "b", Title: "t", Acceptance: "x", Deps: []string{"a", "a", "a"}},
	}}
	err := p.Validate()
	require.Error(t, err)
	assert.Equal(t, 2, strings.Count(err.Error(), `duplicate dependency "a"`))
}

func TestPlanDependencyCount(t *testing.T) {
	assert.Equal(t, 0, Plan{}.DependencyCount())
	assert.Equal(t, 1, samplePlan().DependencyCount())
	p := Plan{Beads: []PlanBead{
		{Key: "a"}, {Key: "b", Deps: []string{"a"}}, {Key: "c", Deps: []string{"a", "b"}},
	}}
	assert.Equal(t, 3, p.DependencyCount(), "one edge per (bead, dep) pair, matching D-24 edges[]")
}

func TestPlanJSONPriorityDefault(t *testing.T) {
	// D-22: a PO plan that omits priority gets bd's default, not the most
	// urgent one; explicit values, including 0, are kept.
	const in = `{"beads":[
		{"key":"a","title":"t","acceptance":"x"},
		{"key":"b","title":"t","acceptance":"x","priority":0},
		{"key":"c","title":"t","acceptance":"x","priority":null},
		{"key":"d","title":"t","acceptance":"x","priority":""},
		{"key":"e","title":"t","acceptance":"x","priority":"4"},
		{"key":"f","title":"t","acceptance":"x","priority":3}
	]}`
	var p Plan
	require.NoError(t, json.Unmarshal([]byte(in), &p))
	require.NoError(t, p.Validate())
	got := make([]Priority, len(p.Beads))
	for i, b := range p.Beads {
		got[i] = b.Priority
	}
	assert.Equal(t, []Priority{PriorityDefault, PriorityHighest, PriorityDefault, PriorityDefault, PriorityLowest, 3}, got)

	t.Run("out of range still fails validation", func(t *testing.T) {
		var p Plan
		require.NoError(t, json.Unmarshal([]byte(`{"beads":[{"key":"a","title":"t","acceptance":"x","priority":9}]}`), &p))
		err := p.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "priority 9 out of range 0..4")
	})

	t.Run("non-numeric priority is a decode error", func(t *testing.T) {
		var p Plan
		err := json.Unmarshal([]byte(`{"beads":[{"key":"a","title":"t","acceptance":"x","priority":"urgent"}]}`), &p)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrUnknownValue)
	})

	t.Run("other fields still decode through the custom unmarshaller", func(t *testing.T) {
		var b PlanBead
		require.NoError(t, json.Unmarshal([]byte(`{"key":"k","title":"T","description":"D","acceptance":"A","type":"bug","size":"L","deps":["x","y"]}`), &b))
		assert.Equal(t, PlanBead{Key: "k", Title: "T", Description: "D", Acceptance: "A", Type: BeadTypeBug, Priority: PriorityDefault, Size: SizeL, Deps: []string{"x", "y"}}, b)
		var bad PlanBead
		assert.Error(t, json.Unmarshal([]byte(`{"key":"k","type":"saga"}`), &bad), "enum errors still surface")
	})
}
