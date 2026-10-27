package agents_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

func reviewBead(key, title string, deps ...string) domain.PlanBead {
	return domain.PlanBead{Key: key, Title: title, Acceptance: "tested", Priority: domain.PriorityDefault, Deps: deps}
}

func reviewCandidate(planner domain.AgentID, beads ...domain.PlanBead) domain.PlanCandidate {
	return domain.PlanCandidate{Planner: planner, Plan: domain.Plan{Beads: beads}}
}

func TestReconcilePlansDeterministicCanonicalRemapping(t *testing.T) {
	existing := []domain.Bead{{ID: "bd-existing", Title: "Existing work", State: domain.BeadStateOpen, Priority: 1}}
	original := append([]domain.Bead(nil), existing...)
	result, err := agents.ReconcilePlans(agents.PlanReconciliationRequest{
		Candidates: []domain.PlanCandidate{
			reviewCandidate("po-c", reviewBead("dupe", "SHARED!!!"), reviewBead("tail", "Tail", "dupe")),
			reviewCandidate("po-b", reviewBead("old", " existing   work "), reviewBead("follow", "Follow", "old")),
			reviewCandidate("po-a", reviewBead("root", "Shared"), reviewBead("child", "Child", "root")),
		},
		Existing: existing, MaxBeads: 10, JudgeFailed: true,
	})
	require.NoError(t, err)
	assert.Equal(t, existing, original, "reconciliation cannot modify the existing snapshot")
	assert.Equal(t, []string{"p1:root", "p1:child", "p2:follow", "p3:tail"}, planKeys(result.Plan.Plan))
	assert.Equal(t, []string{"p1:root"}, result.Plan.Plan.Beads[1].Deps)
	assert.Empty(t, result.Plan.Plan.Beads[2].Deps)
	assert.Equal(t, []string{"p1:root"}, result.Plan.Plan.Beads[3].Deps)
	assert.Equal(t, []domain.PlanExternalDependency{{FromKey: "p2:follow", ToID: "bd-existing"}}, result.Plan.ExternalDeps)
	assert.Equal(t, []domain.PlanReview{
		{Planner: "po-a", Candidate: "p1:root", Decision: domain.PlanReviewIndependent, Provenance: domain.PlanProvenanceFallback},
		{Planner: "po-a", Candidate: "p1:child", Decision: domain.PlanReviewIndependent, Provenance: domain.PlanProvenanceFallback},
		{Planner: "po-b", Candidate: "p2:old", Decision: domain.PlanReviewDuplicate, Canonical: "bd-existing", Provenance: domain.PlanProvenanceExisting},
		{Planner: "po-b", Candidate: "p2:follow", Decision: domain.PlanReviewIndependent, Provenance: domain.PlanProvenanceFallback},
		{Planner: "po-c", Candidate: "p3:dupe", Decision: domain.PlanReviewDuplicate, Canonical: "p1:root", Provenance: domain.PlanProvenanceDeterministic},
		{Planner: "po-c", Candidate: "p3:tail", Decision: domain.PlanReviewIndependent, Provenance: domain.PlanProvenanceFallback},
	}, result.Reviews)

	again, err := agents.ReconcilePlans(agents.PlanReconciliationRequest{
		Candidates: []domain.PlanCandidate{
			reviewCandidate("po-a", reviewBead("root", "Shared"), reviewBead("child", "Child", "root")),
			reviewCandidate("po-b", reviewBead("old", " existing   work "), reviewBead("follow", "Follow", "old")),
			reviewCandidate("po-c", reviewBead("dupe", "SHARED!!!"), reviewBead("tail", "Tail", "dupe")),
		},
		Existing: existing, MaxBeads: 10, JudgeFailed: true,
	})
	require.NoError(t, err)
	assert.Equal(t, result, again, "input completion order cannot affect the graph")
}

func TestReconcilePlansValidatesJudgeAndPropagatesConflicts(t *testing.T) {
	candidates := []domain.PlanCandidate{reviewCandidate("po-1",
		reviewBead("bad", "Conflicting"), reviewBead("dependent", "Dependent", "bad"), reviewBead("good", "Independent"),
	)}
	result, err := agents.ReconcilePlans(agents.PlanReconciliationRequest{
		Candidates: candidates, MaxBeads: 3,
		Decisions: []domain.PlanReview{
			{Candidate: "p1:bad", Decision: domain.PlanReviewConflict},
			{Candidate: "p1:dependent", Decision: domain.PlanReviewIndependent},
			{Candidate: "p1:good", Decision: domain.PlanReviewIndependent},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"p1:good"}, planKeys(result.Plan.Plan))
	assert.Equal(t, domain.PlanReviewConflict, result.Reviews[0].Decision)
	assert.Equal(t, domain.PlanReviewConflict, result.Reviews[1].Decision, "a dangling edge cannot be silently dropped")
	assert.Equal(t, domain.PlanProvenanceFallback, result.Reviews[1].Provenance)
	assert.Equal(t, domain.PlanReviewIndependent, result.Reviews[2].Decision)
}

func TestReconcilePlansJudgeFailureFallback(t *testing.T) {
	candidates := []domain.PlanCandidate{reviewCandidate("po-1", reviewBead("a", "A"), reviewBead("b", "B"))}
	result, err := agents.ReconcilePlans(agents.PlanReconciliationRequest{
		Candidates: candidates, MaxBeads: 2,
		Decisions: []domain.PlanReview{{Candidate: "p1:a", Decision: domain.PlanReviewDuplicate, Canonical: "invented-id"}},
	})
	require.NoError(t, err, "an invalid judge response uses the fallback rather than retrying")
	assert.Equal(t, []string{"p1:a", "p1:b"}, planKeys(result.Plan.Plan))
	for _, review := range result.Reviews {
		assert.Equal(t, domain.PlanReviewIndependent, review.Decision)
		assert.Equal(t, domain.PlanProvenanceFallback, review.Provenance)
	}

	result, err = agents.ReconcilePlans(agents.PlanReconciliationRequest{Candidates: candidates, MaxBeads: 2})
	require.NoError(t, err)
	assert.Empty(t, result.Plan.Plan.Beads, "a successful but incomplete judge response fails closed")
	for _, review := range result.Reviews {
		assert.Equal(t, domain.PlanReviewConflict, review.Decision)
	}
}

func TestReconcilePlansRejectsQuotaAndMalformedGraphs(t *testing.T) {
	_, err := agents.ReconcilePlans(agents.PlanReconciliationRequest{
		Candidates: []domain.PlanCandidate{reviewCandidate("po-1", reviewBead("a", "A"), reviewBead("b", "B"))},
		MaxBeads:   1, JudgeFailed: true,
	})
	assert.ErrorIs(t, err, agents.ErrCombinedPlanQuota)

	for _, bead := range []domain.PlanBead{
		reviewBead("a", "A", "missing"),
		reviewBead("a", "A", "a"),
	} {
		_, err = agents.ReconcilePlans(agents.PlanReconciliationRequest{
			Candidates: []domain.PlanCandidate{reviewCandidate("po-1", bead)}, MaxBeads: 1, JudgeFailed: true,
		})
		assert.ErrorIs(t, err, domain.ErrInvalidPlan)
	}
	_, err = agents.ReconcilePlans(agents.PlanReconciliationRequest{
		Candidates: []domain.PlanCandidate{reviewCandidate("po-1", reviewBead("a", "A", "b"), reviewBead("b", "B", "a"))},
		MaxBeads:   2, JudgeFailed: true,
	})
	assert.ErrorIs(t, err, domain.ErrInvalidPlan)
}

func TestValidatePlanReviewsRejectsIDEscalation(t *testing.T) {
	candidates := []domain.PlanCandidate{reviewCandidate("po-1", reviewBead("p1:a", "A"))}
	existing := []domain.Bead{{ID: "bd-open", State: domain.BeadStateOpen}, {ID: "bd-closed", State: domain.BeadStateClosed}}
	assert.NoError(t, agents.ValidatePlanReviews(candidates, existing, []domain.PlanReview{
		{Candidate: "p1:a", Decision: domain.PlanReviewDuplicate, Canonical: "bd-open"},
	}))
	for _, decision := range []domain.PlanReview{
		{Candidate: "p1:a", Decision: domain.PlanReviewDuplicate, Canonical: "bd-closed"},
		{Candidate: "p1:a", Decision: domain.PlanReviewDuplicate, Canonical: "p1:a"},
		{Candidate: "p1:a", Decision: domain.PlanReviewIndependent, Canonical: "bd-open"},
		{Candidate: "p1:other", Decision: domain.PlanReviewConflict},
	} {
		assert.ErrorIs(t, agents.ValidatePlanReviews(candidates, existing, []domain.PlanReview{decision}), agents.ErrInvalidPlanReview)
	}
}

func planKeys(plan domain.Plan) []string {
	out := make([]string, len(plan.Beads))
	for i, bead := range plan.Beads {
		out[i] = bead.Key
	}
	return out
}

func planReviewRequest(t *testing.T) agents.PlanReviewRequest {
	t.Helper()
	return agents.PlanReviewRequest{
		Run: "run-review", AgentID: "po-judge", Round: 1,
		Repo: prompt.RepoContext{Root: t.TempDir(), DefaultBranch: "main"},
		Candidates: []domain.PlanCandidate{
			reviewCandidate("po-b", reviewBead("dupe", "SHARED!!!"), reviewBead("old", "Old approach")),
			reviewCandidate("po-a", reviewBead("root", "Shared"), reviewBead("child", "Child", "root")),
		},
		Existing: []domain.Bead{{ID: "bd-open", Title: "Already tracked", State: domain.BeadStateOpen}},
	}
}

func planReviewAdapter(t *testing.T, output string) *mock.Adapter {
	t.Helper()
	scenario := mock.NewScenario("plan-review")
	scenario.Rule("judge", mock.Match{Role: string(domain.RolePO), Task: string(events.TaskPlanReview)}).Result = mock.ResultSpec{
		Structured: json.RawMessage(output),
	}
	adapter, err := mock.NewAdapter("mock", scenario)
	require.NoError(t, err)
	return adapter
}

func TestRunPlanReviewThroughStructuredFire(t *testing.T) {
	req := planReviewRequest(t)
	adapter := planReviewAdapter(t, `{"decisions":[
		{"candidate":"p1:root","decision":"independent"},
		{"candidate":"p1:child","decision":"independent"},
		{"candidate":"p2:old","decision":"duplicate","canonical":"p1:root"}
	]}`)
	decisions, outcome, err := agents.RunPlanReview(context.Background(), adapter, req)
	require.NoError(t, err)
	require.Len(t, outcome.Attempts, 1)
	assert.Equal(t, 1, adapter.Uses("judge"))
	assert.Equal(t, []domain.PlanReview{
		{Planner: "po-a", Candidate: "p1:root", Decision: domain.PlanReviewIndependent},
		{Planner: "po-a", Candidate: "p1:child", Decision: domain.PlanReviewIndependent},
		{Planner: "po-b", Candidate: "p2:old", Decision: domain.PlanReviewDuplicate, Canonical: "p1:root"},
	}, decisions)

	call, ok := adapter.LastCall()
	require.True(t, ok)
	assert.Equal(t, cartridge.ToolReadOnly, call.Spec.ToolPolicy)
	assert.Equal(t, string(events.TaskPlanReview), call.Spec.Labels.Get(cartridge.LabelTask))
	assert.Contains(t, call.Spec.Prompt, `"judge_only":["p1:root","p1:child","p2:old"]`)
	assert.Contains(t, call.Spec.Prompt, "When uncertain, choose conflict")

	reconciled, err := agents.ReconcilePlans(agents.PlanReconciliationRequest{
		Candidates: req.Candidates, Existing: req.Existing, Decisions: decisions, MaxBeads: 4,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"p1:root", "p1:child"}, planKeys(reconciled.Plan.Plan))
	assert.Equal(t, domain.PlanProvenanceJudge, reconciled.Reviews[3].Provenance)
}

func TestRunPlanReviewRejectsWithoutRetry(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{"unknown decision", `{"decisions":[{"candidate":"p1:root","decision":"merge"}]}`},
		{"malformed candidate", `{"decisions":[{"candidate":"p1: root","decision":"conflict"}]}`},
		{"unknown candidate", `{"decisions":[{"candidate":"p9:invented","decision":"conflict"}]}`},
		{"invented canonical ID", `{"decisions":[
			{"candidate":"p1:root","decision":"independent"},
			{"candidate":"p1:child","decision":"independent"},
			{"candidate":"p2:old","decision":"duplicate","canonical":"bd-invented"}]}`},
		{"canonical on independent", `{"decisions":[{"candidate":"p1:root","decision":"independent","canonical":"bd-open"}]}`},
		{"incomplete", `{"decisions":[{"candidate":"p1:root","decision":"independent"}]}`},
		{"resolved candidate", `{"decisions":[
			{"candidate":"p1:root","decision":"independent"},
			{"candidate":"p1:child","decision":"independent"},
			{"candidate":"p2:old","decision":"conflict"},
			{"candidate":"p2:dupe","decision":"duplicate","canonical":"p1:root"}]}`},
		{"prose or graph edit", `{"decisions":[],"title":"rewritten"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			adapter := planReviewAdapter(t, tc.output)
			decisions, outcome, err := agents.RunPlanReview(context.Background(), adapter, planReviewRequest(t))
			require.Error(t, err)
			assert.Nil(t, decisions)
			assert.Len(t, outcome.Attempts, 1, "a bad judge answer must not trigger a schema retry")
			assert.Equal(t, 1, adapter.Uses("judge"))
		})
	}
}

func TestRunPlanReviewFailureAndUncertaintyAreDeterministic(t *testing.T) {
	req := planReviewRequest(t)
	scenario := mock.NewScenario("plan-review-failure")
	scenario.Rule("crash", mock.Match{Task: string(events.TaskPlanReview)}).Fail(mock.FailCrash, "judge crashed")
	adapter, err := mock.NewAdapter("mock", scenario)
	require.NoError(t, err)
	decisions, outcome, err := agents.RunPlanReview(context.Background(), adapter, req)
	require.Error(t, err)
	assert.Nil(t, decisions)
	assert.Len(t, outcome.Attempts, 1)
	assert.Equal(t, 1, adapter.Uses("crash"))

	fallback, err := agents.ReconcilePlans(agents.PlanReconciliationRequest{
		Candidates: req.Candidates, Existing: req.Existing, MaxBeads: 4, JudgeFailed: true,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"p1:root", "p1:child", "p2:old"}, planKeys(fallback.Plan.Plan))

	uncertain := planReviewAdapter(t, `{"decisions":[
		{"candidate":"p1:root","decision":"conflict"},
		{"candidate":"p1:child","decision":"conflict"},
		{"candidate":"p2:old","decision":"conflict"}
	]}`)
	decisions, outcome, err = agents.RunPlanReview(context.Background(), uncertain, req)
	require.NoError(t, err, "conflict is a valid uncertain answer, not a runner failure")
	assert.Len(t, outcome.Attempts, 1)
	for _, decision := range decisions {
		assert.Equal(t, domain.PlanReviewConflict, decision.Decision)
	}
}

func TestStructuredPlanReviewRunnerAcceptsCoordinatorNamespacing(t *testing.T) {
	req := planReviewRequest(t)
	namespaced, err := agents.NamespacePlanCandidates(req.Candidates)
	require.NoError(t, err)
	adapter := planReviewAdapter(t, `{"decisions":[
		{"candidate":"p1:root","decision":"independent"},
		{"candidate":"p1:child","decision":"independent"},
		{"candidate":"p2:old","decision":"independent"}
	]}`)
	runner := agents.StructuredPlanReviewRunner{Cartridge: adapter, Request: req}
	decisions, err := runner.ReviewPlans(context.Background(), agents.PlanReconciliationRequest{
		Candidates: namespaced, Existing: req.Existing, MaxBeads: 4,
	})
	require.NoError(t, err)
	require.Len(t, decisions, 3)
	for _, decision := range decisions {
		assert.NotContains(t, decision.Candidate, "p1:p1:")
		assert.Equal(t, domain.PlanReviewIndependent, decision.Decision)
	}
}

func TestRunPlanReviewSkipsSessionWhenDeterminismResolvesEverything(t *testing.T) {
	req := planReviewRequest(t)
	req.Candidates = []domain.PlanCandidate{
		reviewCandidate("po-a", reviewBead("a", "Existing")),
		reviewCandidate("po-b", reviewBead("b", " existing!!! ")),
	}
	req.Existing = []domain.Bead{{ID: "bd-existing", Title: "Existing", State: domain.BeadStateOpen}}
	decisions, outcome, err := agents.RunPlanReview(context.Background(), nil, req)
	require.NoError(t, err)
	assert.Empty(t, decisions)
	assert.Empty(t, outcome.Attempts)
}

const planReviewAllIndependent = `{"decisions":[
	{"candidate":"p1:root","decision":"independent"},
	{"candidate":"p1:child","decision":"independent"},
	{"candidate":"p2:old","decision":"independent"}
]}`

func TestStructuredPlanReviewRunnerPropagatesReadOnlyShimEnv(t *testing.T) {
	req := planReviewRequest(t)
	req.Env = map[string]string{"TEMPLATE_ONLY": "discard"}
	namespaced, err := agents.NamespacePlanCandidates(req.Candidates)
	require.NoError(t, err)
	env := map[string]string{"PATH": "/usr/bin", "SC_JUDGE_TEST": "present"}
	beads.Shim{Dir: "/run/state/read-only-shim"}.Apply(env)
	wantPath := beads.PrependPath("/run/state/read-only-shim", "/usr/bin")
	adapter := planReviewAdapter(t, planReviewAllIndependent)
	runner := agents.StructuredPlanReviewRunner{Cartridge: adapter, Request: req}
	_, err = runner.ReviewPlans(context.Background(), agents.PlanReconciliationRequest{
		Candidates: namespaced, Existing: req.Existing, Env: env, MaxBeads: 4,
	})
	require.NoError(t, err)
	call, ok := adapter.LastCall()
	require.True(t, ok)
	assert.Equal(t, wantPath, call.Spec.Env["PATH"])
	assert.Equal(t, "present", call.Spec.Env["SC_JUDGE_TEST"])
	assert.NotContains(t, call.Spec.Env, "TEMPLATE_ONLY")

	env["PATH"] = "/ mutated after firing"
	env["SC_JUDGE_TEST"] = "changed"
	assert.Equal(t, wantPath, call.Spec.Env["PATH"], "the firing must not retain the coordinator's map")
	assert.Equal(t, "present", call.Spec.Env["SC_JUDGE_TEST"])
}

func TestRunPlanReviewReportsUsageOnSuccessAndFailure(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		want := cartridge.Usage{InputTokens: 120, OutputTokens: 30, CostUSD: 0.25, HasCost: true, Turns: 2}
		scenario := mock.NewScenario("plan-review-usage")
		rule := scenario.Rule("judge", mock.Match{Task: string(events.TaskPlanReview)})
		rule.Result = mock.ResultSpec{Structured: json.RawMessage(planReviewAllIndependent)}
		rule.Usage(want)
		adapter, err := mock.NewAdapter("mock", scenario)
		require.NoError(t, err)
		req := planReviewRequest(t)
		var callbacks []cartridge.Usage
		req.OnUsage = func(u cartridge.Usage) { callbacks = append(callbacks, u) }
		_, outcome, err := agents.RunPlanReview(context.Background(), adapter, req)
		require.NoError(t, err)
		assert.Equal(t, want, outcome.Usage)
		assert.Equal(t, []cartridge.Usage{want}, callbacks)
	})

	t.Run("session failure", func(t *testing.T) {
		want := cartridge.Usage{InputTokens: 80, OutputTokens: 7, Turns: 1}
		scenario := mock.NewScenario("plan-review-usage-failure")
		rule := scenario.Rule("judge", mock.Match{Task: string(events.TaskPlanReview)})
		rule.Fail(mock.FailCrash, "judge crashed").Usage(want)
		adapter, err := mock.NewAdapter("mock", scenario)
		require.NoError(t, err)
		req := planReviewRequest(t)
		var callbacks []cartridge.Usage
		req.OnUsage = func(u cartridge.Usage) { callbacks = append(callbacks, u) }
		_, outcome, err := agents.RunPlanReview(context.Background(), adapter, req)
		require.Error(t, err)
		assert.Equal(t, want, outcome.Usage)
		assert.Equal(t, []cartridge.Usage{want}, callbacks)
		assert.Equal(t, 1, adapter.Uses("judge"))
	})
}

func TestPlanReviewAgentIDDerivation(t *testing.T) {
	req := planReviewRequest(t)
	req.AgentID = ""
	require.NoError(t, req.Validate())
	adapter := planReviewAdapter(t, planReviewAllIndependent)
	_, _, err := agents.RunPlanReview(context.Background(), adapter, req)
	require.NoError(t, err)
	call, ok := adapter.LastCall()
	require.True(t, ok)
	assert.Equal(t, "po-b-judge", call.Spec.Labels.Get(cartridge.LabelAgent), "derive from the first candidate, not sorted planner order")
	assert.Empty(t, req.AgentID, "derivation must not mutate the caller's request")

	req.AgentID = "po-explicit"
	spec, p, err := agents.PlanReviewRunSpec(req)
	require.NoError(t, err)
	assert.Equal(t, "po-explicit", spec.Labels.Get(cartridge.LabelAgent))
	assert.Equal(t, domain.AgentID("po-explicit"), p.AgentID)
}

func TestPlanReviewAgentIDUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		candidates []domain.PlanCandidate
	}{
		{"no candidates", nil},
		{"blank first planner", []domain.PlanCandidate{reviewCandidate("", reviewBead("a", "A"))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := planReviewRequest(t)
			req.AgentID = ""
			req.Candidates = tc.candidates
			_, outcome, err := agents.RunPlanReview(context.Background(), nil, req)
			require.ErrorIs(t, err, agents.ErrInvalidPlanReview)
			assert.Contains(t, err.Error(), "no first candidate planner")
			assert.Empty(t, outcome.Attempts)
		})
	}
}

func TestRunPlanReviewRequiresRunID(t *testing.T) {
	req := planReviewRequest(t)
	req.Run = ""
	req.AgentID = ""
	req.Candidates = nil
	var callbacks []cartridge.Usage
	req.OnUsage = func(u cartridge.Usage) { callbacks = append(callbacks, u) }
	_, outcome, err := agents.RunPlanReview(context.Background(), nil, req)
	require.ErrorIs(t, err, agents.ErrInvalidPlanReview)
	assert.Contains(t, err.Error(), "empty run id")
	assert.Empty(t, outcome.Attempts)
	assert.Equal(t, []cartridge.Usage{{}}, callbacks, "preflight failures still report the returned zero usage")
}
