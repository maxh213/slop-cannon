package events_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

func TestPlanProducedContributorCompatibility(t *testing.T) {
	legacy := []byte(`{"kind":"plan_produced","at":"2026-09-02T12:01:03.5Z","run_id":"r1","agent":"po-1","plan":{"beads":[]}}`)
	decoded, err := events.Unmarshal(legacy)
	require.NoError(t, err)
	produced, ok := decoded.(events.PlanProduced)
	require.True(t, ok)
	assert.Nil(t, produced.Contributors)
	encoded, err := events.Marshal(produced)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "contributors")

	produced.Contributors = []domain.AgentID{"po-1", "po-2"}
	encoded, err = events.Marshal(produced)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"contributors":["po-1","po-2"]`)
}

func TestPlanReviewedValidationAndRedaction(t *testing.T) {
	valid := events.PlanReviewed{
		Meta: events.NewMeta("r1"), Round: 2, Planner: "po-2", Candidate: "p2:a",
		Decision: domain.PlanReviewDuplicate, Canonical: "bd-existing", Provenance: domain.PlanProvenanceExisting,
	}
	require.NoError(t, valid.Validate())
	assert.Equal(t, valid, events.Redacted(valid, func(string) string { return "rewritten" }),
		"review events contain correlation data, never candidate prose")

	for _, invalid := range []events.PlanReviewed{
		{Round: -1, Planner: "po", Candidate: "p:a", Decision: domain.PlanReviewConflict, Provenance: domain.PlanProvenanceJudge},
		{Planner: "po", Candidate: "p:a", Decision: domain.PlanReviewDuplicate, Provenance: domain.PlanProvenanceJudge},
		{Planner: "po", Candidate: "p:a", Decision: domain.PlanReviewIndependent, Canonical: "bd", Provenance: domain.PlanProvenanceJudge},
		{Planner: "po", Candidate: strings.Repeat("x", domain.MaxPlanReviewReferenceLength+1), Decision: domain.PlanReviewConflict, Provenance: domain.PlanProvenanceJudge},
		{Planner: "po", Candidate: "p:a", Decision: "merge", Provenance: domain.PlanProvenanceJudge},
	} {
		assert.ErrorIs(t, invalid.Validate(), events.ErrInvalidPlanReviewed)
	}
}

func TestPlanReviewedCodec(t *testing.T) {
	original := events.PlanReviewed{
		Meta: events.NewMeta("r1"), Round: 1, Planner: "po-1", Candidate: "p1:a",
		Decision: domain.PlanReviewIndependent, Provenance: domain.PlanProvenanceJudge,
	}
	encoded, err := events.Marshal(original)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"kind":"plan_reviewed"`)
	assert.Contains(t, string(encoded), `"decision":"independent"`)
	assert.NotContains(t, string(encoded), "canonical")

	decoded, err := events.Unmarshal(encoded)
	require.NoError(t, err)
	reviewed, ok := decoded.(events.PlanReviewed)
	require.True(t, ok)
	assert.Equal(t, original, reviewed)
	require.NoError(t, reviewed.Validate())
}
