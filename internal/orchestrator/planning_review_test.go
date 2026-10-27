package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/cartridge/structured"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

type planningApplierFunc func(context.Context, beads.PlanRequest, bool) (beads.PlanResult, error)

func (fn planningApplierFunc) ApplyPlan(ctx context.Context, req beads.PlanRequest, dry bool) (beads.PlanResult, error) {
	return fn(ctx, req, dry)
}

func planningParallelFixture(t *testing.T, brief string, runner PlanRunner, reviewer PlanReviewRunner) *planningFixture {
	t.Helper()
	f := planningSetup(t, brief, func(o *PlannerOptions) {
		o.PlannerCount = 2
		o.Runner = runner
		o.Reviewer = reviewer
	})
	if concrete, ok := runner.(*planningRunner); ok {
		f.Runner = concrete
	}
	return f
}

func planningIndependentJudge() PlanReviewRunner {
	return PlanReviewRunnerFunc(func(_ context.Context, req agents.PlanReconciliationRequest) ([]domain.PlanReview, error) {
		existing := make(map[string]bool)
		for _, b := range req.Existing {
			if b.State != domain.BeadStateClosed {
				existing[agents.NormalisePlanTitle(b.Title)] = true
			}
		}
		seen := make(map[string]bool)
		var out []domain.PlanReview
		for _, candidate := range req.Candidates {
			for _, b := range candidate.Plan.Beads {
				title := agents.NormalisePlanTitle(b.Title)
				if existing[title] || seen[title] {
					continue
				}
				seen[title] = true
				out = append(out, domain.PlanReview{
					Candidate: b.Key, Decision: domain.PlanReviewIndependent,
				})
			}
		}
		return out, nil
	})
}

func planningParallelPlan(prefix string, n int) domain.Plan {
	plan := domain.Plan{Brief: "combined work"}
	for i := 0; i < n; i++ {
		plan.Beads = append(plan.Beads, domain.PlanBead{
			Key: fmt.Sprintf("shared-%d", i), Title: fmt.Sprintf("%s %d", prefix, i),
			Acceptance: "verified", Priority: domain.PriorityDefault,
		})
	}
	return plan
}

func planningParallelFire(f *planningFixture) Firing {
	return Firing{Demand: planDemand(), Agent: domain.Agent{ID: "po-plan-1"}, Run: f.Run}
}

func planningAppliedResult(req beads.PlanRequest) beads.PlanResult {
	res := beads.PlanResult{Run: req.Run, Epic: req.Epic, IDs: make(map[string]domain.BeadID)}
	if res.Epic == "" {
		res.Epic, res.EpicCreated, res.EpicKey = "sc-epic", true, "epic"
	}
	for _, b := range req.Plan.Beads {
		id := domain.BeadID("sc-" + b.Key)
		res.IDs[b.Key] = id
		res.Beads = append(res.Beads, id)
	}
	return res
}

func planningReviewEvents(evs []events.Event) ([]events.PlanReviewed, []events.PlanProduced) {
	var reviews []events.PlanReviewed
	var produced []events.PlanProduced
	for _, ev := range evs {
		switch ev := ev.(type) {
		case events.PlanReviewed:
			reviews = append(reviews, ev)
		case events.PlanProduced:
			produced = append(produced, ev)
		}
	}
	return reviews, produced
}

func TestParallelPlanningBarrierReversedCompletionAndSameRawKeys(t *testing.T) {
	releaseFirst := make(chan struct{})
	secondDone := make(chan struct{})
	var once sync.Once
	runner := PlanRunnerFunc(func(ctx context.Context, _ Firing, req agents.PlanRequest) (domain.Plan, agents.Transcript, error) {
		switch req.AgentID {
		case "po-plan-1-p1":
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return domain.Plan{}, agents.Transcript{}, ctx.Err()
			}
			return planningParallelPlan("First", 1), agents.Transcript{}, nil
		case "po-plan-1-p2":
			once.Do(func() { close(secondDone) })
			return planningParallelPlan("Second", 1), agents.Transcript{}, nil
		default:
			return domain.Plan{}, agents.Transcript{}, fmt.Errorf("unexpected planner %s", req.AgentID)
		}
	})
	f := planningParallelFixture(t, "ship it", runner, planningIndependentJudge())
	done := make(chan error, 1)
	go func() {
		_, err := f.Planner.Fire(context.Background(), planningParallelFire(f))
		done <- err
	}()
	select {
	case <-secondDone:
	case <-time.After(10 * time.Second):
		t.Fatal("second producer did not finish while the first was blocked")
	}
	assert.Zero(t, f.Applier.count(), "the completion barrier precedes persistence")
	planningEventually(t, "the second worker to leave the outstanding set", func() bool {
		f.Planner.mu.Lock()
		defer f.Planner.mu.Unlock()
		return f.Planner.outstanding == 1
	})
	assert.Zero(t, f.Applier.count(), "the first producer still holds the barrier")
	close(releaseFirst)
	require.NoError(t, <-done)

	require.Equal(t, 1, f.Applier.count())
	f.Applier.mu.Lock()
	req := f.Applier.calls[0]
	f.Applier.mu.Unlock()
	require.Len(t, req.Plan.Beads, 2)
	assert.Equal(t, []string{"p1:shared-0", "p2:shared-0"}, []string{req.Plan.Beads[0].Key, req.Plan.Beads[1].Key})
	assert.Equal(t, []string{"First 0", "Second 0"}, []string{req.Plan.Beads[0].Title, req.Plan.Beads[1].Title},
		"planner position, not completion order, determines the graph")
	reviews, produced := planningReviewEvents(f.events())
	assert.Len(t, reviews, 2)
	require.Len(t, produced, 1)
	assert.Equal(t, []domain.AgentID{"po-plan-1-p1", "po-plan-1-p2"}, produced[0].Contributors)
}

func TestParallelPlanningProducerFailureKeepsOtherCandidate(t *testing.T) {
	runner := PlanRunnerFunc(func(_ context.Context, _ Firing, req agents.PlanRequest) (domain.Plan, agents.Transcript, error) {
		if req.AgentID == "po-plan-1-p1" {
			return domain.Plan{}, agents.Transcript{}, errors.New("first producer crashed")
		}
		return planningParallelPlan("Survivor", 1), agents.Transcript{}, nil
	})
	f := planningParallelFixture(t, "ship it", runner, planningIndependentJudge())
	_, err := f.Planner.Fire(context.Background(), planningParallelFire(f))
	require.NoError(t, err)
	assert.Equal(t, 1, f.Applier.count())
	assert.Equal(t, []string{"Survivor 0"}, f.Applier.titles(1))
	_, produced := planningReviewEvents(f.events())
	require.Len(t, produced, 1)
	assert.Equal(t, []domain.AgentID{"po-plan-1-p2"}, produced[0].Contributors)
}

func TestParallelPlanningCombinedQuotaAndExternalDependency(t *testing.T) {
	t.Run("combined quota multiplies the per-producer cap", func(t *testing.T) {
		runner := PlanRunnerFunc(func(_ context.Context, _ Firing, req agents.PlanRequest) (domain.Plan, agents.Transcript, error) {
			assert.Equal(t, 1, req.MaxBeads)
			return planningParallelPlan(string(req.AgentID), 1), agents.Transcript{}, nil
		})
		base := planningIndependentJudge()
		var judgeCalls, combinedCap atomic.Int32
		reviewer := PlanReviewRunnerFunc(func(ctx context.Context, req agents.PlanReconciliationRequest) ([]domain.PlanReview, error) {
			judgeCalls.Add(1)
			combinedCap.Store(int32(req.MaxBeads))
			return base.ReviewPlans(ctx, req)
		})
		f := planningParallelFixture(t, "ship it", runner, reviewer)
		f.Planner.maxBeads = 1
		_, err := f.Planner.Fire(context.Background(), planningParallelFire(f))
		require.NoError(t, err)
		assert.Equal(t, int32(1), judgeCalls.Load())
		assert.Equal(t, int32(2), combinedCap.Load())
		assert.Equal(t, 2, f.Planner.Status().Beads)
		assert.Equal(t, 1, f.Applier.count())
	})

	t.Run("duplicate dependency becomes an external to_id edge", func(t *testing.T) {
		runner := PlanRunnerFunc(func(_ context.Context, _ Firing, req agents.PlanRequest) (domain.Plan, agents.Transcript, error) {
			if req.AgentID == "po-plan-1-p1" {
				plan := planningParallelPlan("Existing work", 1)
				plan.Beads[0].Key = "old"
				plan.Beads = append(plan.Beads, domain.PlanBead{
					Key: "follow", Title: "Follow", Acceptance: "verified", Priority: domain.PriorityDefault, Deps: []string{"old"},
				})
				return plan, agents.Transcript{}, nil
			}
			return planningParallelPlan("Independent", 1), agents.Transcript{}, nil
		})
		prioritiser := &planningPrioritiser{}
		f := planningParallelFixture(t, "", runner, planningIndependentJudge())
		f.Planner.opts.Prioritiser = prioritiser
		f.Beads.set(domain.Bead{ID: "bd-existing", Title: "Existing work 0", State: domain.BeadStateOpen})
		_, err := f.Planner.Fire(context.Background(), planningParallelFire(f))
		require.NoError(t, err)
		f.Applier.mu.Lock()
		req := f.Applier.calls[0]
		f.Applier.mu.Unlock()
		require.Len(t, req.Plan.Beads, 2)
		assert.Equal(t, "p1:follow", req.Plan.Beads[0].Key)
		assert.Empty(t, req.Plan.Beads[0].Deps)
		assert.Equal(t, []domain.PlanExternalDependency{{FromKey: "p1:follow", ToID: "bd-existing"}}, req.ExternalDeps)
		assert.Empty(t, prioritiser.applied(), "multi-producer reconciliation never deprioritises existing beads")
	})
}

func TestParallelPlanningConflictDropsDependentsWithoutDanglingEdges(t *testing.T) {
	runner := PlanRunnerFunc(func(_ context.Context, _ Firing, req agents.PlanRequest) (domain.Plan, agents.Transcript, error) {
		if req.AgentID == "po-plan-1-p1" {
			plan := planningParallelPlan("Conflict", 1)
			plan.Beads[0].Key = "root"
			plan.Beads = append(plan.Beads, domain.PlanBead{
				Key: "child", Title: "Child", Acceptance: "verified", Priority: domain.PriorityDefault, Deps: []string{"root"},
			})
			return plan, agents.Transcript{}, nil
		}
		return planningParallelPlan("Safe", 1), agents.Transcript{}, nil
	})
	base := planningIndependentJudge()
	reviewer := PlanReviewRunnerFunc(func(ctx context.Context, req agents.PlanReconciliationRequest) ([]domain.PlanReview, error) {
		decisions, err := base.ReviewPlans(ctx, req)
		for i := range decisions {
			if decisions[i].Candidate == "p1:root" {
				decisions[i].Decision = domain.PlanReviewConflict
			}
		}
		return decisions, err
	})
	f := planningParallelFixture(t, "ship it", runner, reviewer)
	_, err := f.Planner.Fire(context.Background(), planningParallelFire(f))
	require.NoError(t, err)
	assert.Equal(t, []string{"Safe 0"}, f.Applier.titles(1))
	reviews, _ := planningReviewEvents(f.events())
	require.Len(t, reviews, 3)
	assert.Equal(t, domain.PlanReviewConflict, reviews[0].Decision)
	assert.Equal(t, domain.PlanReviewConflict, reviews[1].Decision)
	assert.Equal(t, domain.PlanReviewIndependent, reviews[2].Decision)
}

func TestParallelPlanningJudgeFailureAndUncertainDecisions(t *testing.T) {
	t.Run("judge failure uses one no-retry fallback", func(t *testing.T) {
		runner := PlanRunnerFunc(func(_ context.Context, _ Firing, req agents.PlanRequest) (domain.Plan, agents.Transcript, error) {
			return planningParallelPlan(string(req.AgentID), 1), agents.Transcript{}, nil
		})
		var calls atomic.Int32
		reviewer := PlanReviewRunnerFunc(func(context.Context, agents.PlanReconciliationRequest) ([]domain.PlanReview, error) {
			calls.Add(1)
			return nil, errors.New("judge unavailable")
		})
		f := planningParallelFixture(t, "ship it", runner, reviewer)
		_, err := f.Planner.Fire(context.Background(), planningParallelFire(f))
		require.NoError(t, err)
		assert.Equal(t, int32(1), calls.Load())
		assert.Equal(t, 1, f.Applier.count())
		reviews, _ := planningReviewEvents(f.events())
		require.Len(t, reviews, 2)
		for _, review := range reviews {
			assert.Equal(t, domain.PlanProvenanceFallback, review.Provenance)
			assert.Equal(t, domain.PlanReviewIndependent, review.Decision)
		}
	})

	t.Run("uncertain candidates fail closed", func(t *testing.T) {
		runner := PlanRunnerFunc(func(_ context.Context, _ Firing, req agents.PlanRequest) (domain.Plan, agents.Transcript, error) {
			return planningParallelPlan(string(req.AgentID), 1), agents.Transcript{}, nil
		})
		var calls atomic.Int32
		reviewer := PlanReviewRunnerFunc(func(context.Context, agents.PlanReconciliationRequest) ([]domain.PlanReview, error) {
			calls.Add(1)
			return nil, nil
		})
		f := planningParallelFixture(t, "", runner, reviewer)
		_, err := f.Planner.Fire(context.Background(), planningParallelFire(f))
		require.NoError(t, err, "an empty reconciled autopilot plan is an idle answer")
		assert.Equal(t, int32(1), calls.Load())
		assert.Zero(t, f.Applier.count())
		assert.Equal(t, 1, f.Planner.Status().Empty)
		reviews, _ := planningReviewEvents(f.events())
		require.Len(t, reviews, 2)
		for _, review := range reviews {
			assert.Equal(t, domain.PlanReviewConflict, review.Decision)
		}
	})
}

func TestParallelPlanningCommitErrorAdoptsGraphBeforeRetry(t *testing.T) {
	for _, withIDs := range []bool{true, false} {
		name := "finalisation result carries IDs"
		if !withIDs {
			name = "uncertain result is recovered from a fresh listing"
		}
		t.Run(name, func(t *testing.T) {
			var producers atomic.Int32
			runner := PlanRunnerFunc(func(_ context.Context, _ Firing, req agents.PlanRequest) (domain.Plan, agents.Transcript, error) {
				producers.Add(1)
				return planningParallelPlan(string(req.AgentID), 1), agents.Transcript{}, nil
			})
			var applies atomic.Int32
			applier := planningApplierFunc(func(_ context.Context, req beads.PlanRequest, _ bool) (beads.PlanResult, error) {
				applies.Add(1)
				if applies.Load() == 1 {
					res := beads.PlanResult{Run: req.Run}
					if withIDs {
						res = planningAppliedResult(req)
						res.Unfinished = append([]domain.BeadID(nil), res.Beads...)
					}
					return res, errors.New("acceptance finalisation failed")
				}
				return planningAppliedResult(req), nil
			})
			f := planningParallelFixture(t, "ship it", runner, planningIndependentJudge())
			f.Planner.opts.Applier = applier
			_, err := f.Planner.Fire(context.Background(), planningParallelFire(f))
			require.Error(t, err)
			assert.Equal(t, int32(1), applies.Load())

			f.Beads.set(
				domain.Bead{ID: "sc-epic", Type: domain.BeadTypeEpic, State: domain.BeadStateOpen},
				planningBead("sc-first", domain.BeadStateOpen), planningBead("sc-second", domain.BeadStateOpen),
			)
			f.Beads.mu.Lock()
			f.Beads.list[1].Title = "po-plan-1-p1 0"
			f.Beads.list[2].Title = "po-plan-1-p2 0"
			f.Beads.mu.Unlock()
			_, err = f.Planner.Fire(context.Background(), planningParallelFire(f))
			require.NoError(t, err)
			assert.Equal(t, int32(1), applies.Load(), "recovery adopts rather than applying a second graph")
			assert.Equal(t, int32(2), producers.Load(), "recovery does not fire another pair of producers")
			assert.Equal(t, domain.BeadID("sc-epic"), f.Run.Epic())
			reviews, produced := planningReviewEvents(f.events())
			assert.Len(t, reviews, 2)
			assert.Len(t, produced, 1, "adoption announces the committed plan exactly once")
			assert.Equal(t, 2, f.Planner.Status().Beads)
		})
	}
}

func TestParallelPlanningResumeDoesNotDuplicatePlanning(t *testing.T) {
	t.Run("before epic persistence", func(t *testing.T) {
		f := planningParallelFixture(t, "ship it", &planningRunner{}, planningIndependentJudge())
		f.Planner.cfg.Resume = true
		f.Beads.set(
			domain.Bead{ID: "sc-epic", Type: domain.BeadTypeEpic, State: domain.BeadStateOpen},
			planningBead("sc-work", domain.BeadStateOpen),
		)
		_, err := f.Planner.Fire(context.Background(), planningParallelFire(f))
		require.NoError(t, err)
		assert.Zero(t, f.Runner.count())
		assert.Zero(t, f.Applier.count())
		assert.Equal(t, domain.BeadID("sc-epic"), f.Run.Epic())
	})

	t.Run("after epic persistence", func(t *testing.T) {
		f := planningParallelFixture(t, "ship it", &planningRunner{}, planningIndependentJudge())
		f.Planner.cfg.Resume = true
		require.NoError(t, f.Run.SetEpic("sc-epic"))
		f.Planner.Attach(f.Run)
		demands, err := f.Planner.Demands(context.Background())
		require.NoError(t, err)
		assert.Empty(t, demands)
		assert.Zero(t, f.Runner.count())
		assert.Zero(t, f.Applier.count())
		assert.True(t, f.Planner.Status().ResumedPlan)
	})
}

func TestParallelPlanningDemandReservesProducerSlots(t *testing.T) {
	p, err := NewPlanner(PlannerOptions{
		Config: planningRunConfig(t, "ship it"), PlannerCount: 2,
		Runner: &planningRunner{}, Applier: &planningApplier{}, NoShim: true,
	})
	require.NoError(t, err)
	demands, err := p.Demands(context.Background())
	require.NoError(t, err)
	require.Len(t, demands, 1)
	assert.Equal(t, 2, demands[0].Slots)
	assert.Equal(t, planDemand().Key(), demands[0].Key())
	assert.Empty(t, demands[0].Agent, "multi-PO rounds retain the scheduler's outer firing identity")
}

func TestDefaultOnePlanningPathIsUnchanged(t *testing.T) {
	runner := &planningRunner{plan: planningPlan("alpha")}
	reviewer := PlanReviewRunnerFunc(func(context.Context, agents.PlanReconciliationRequest) ([]domain.PlanReview, error) {
		t.Fatal("default one must not invoke the semantic judge")
		return nil, nil
	})
	f := planningSetup(t, "ship it", func(o *PlannerOptions) {
		o.Runner = runner
		o.Reviewer = reviewer
	})
	demands, err := f.Planner.Demands(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []Demand{planDemand()}, demands, "default-one keeps the exact legacy demand, including zero Slots")
	_, err = f.Planner.Fire(context.Background(), planningParallelFire(f))
	require.NoError(t, err)
	assert.Equal(t, 1, f.Planner.POPlanners())
	assert.Equal(t, 8, f.Planner.CombinedMaxBeads())
	assert.Equal(t, 1, runner.count())
	req, ok := runner.last()
	require.True(t, ok)
	assert.Equal(t, domain.AgentID("po-plan-1"), req.AgentID)
	f.Applier.mu.Lock()
	keys := f.Applier.calls[0].Plan.Beads[0].Key
	f.Applier.mu.Unlock()
	assert.Equal(t, "alpha", keys, "single-producer keys are not namespaced")
	_, produced := planningReviewEvents(f.events())
	require.Len(t, produced, 1)
	assert.Nil(t, produced[0].Contributors)
}

func TestParallelCandidateCheckDefersExistingDuplicatesToReconciliation(t *testing.T) {
	rc := planningRunConfig(t, "ship it")
	repo, err := prompt.ScanRepo(rc.Run.RepoPath, prompt.ScanOptions{})
	require.NoError(t, err)
	adapter, err := mock.NewAdapter("mock", mock.NewScenario("candidate-check"))
	require.NoError(t, err)
	plan := planningParallelPlan("Existing work", 1)
	payload, err := json.Marshal(plan)
	require.NoError(t, err)
	newRun := func(id domain.AgentID) agents.AgentRun {
		req := agents.PlanRequest{
			Run: rc.Run.ID, AgentID: id, Mode: rc.Run.Mode, Brief: rc.Run.Brief, Repo: repo, MaxBeads: 8,
			OpenBeads: []domain.Bead{{ID: "bd-old", Title: "Existing work 0", State: domain.BeadStateOpen}},
		}
		run, err := PlanAgentRun(FiringContext{Agent: domain.Agent{ID: id, Role: domain.RolePO}, Cartridge: adapter}, nil, req)
		require.NoError(t, err)
		return run
	}
	assert.NoError(t, newRun("po-plan-1-p1").Check(structured.Parsed{}, string(payload)))
	assert.ErrorIs(t, newRun("po-plan-1").Check(structured.Parsed{}, string(payload)), agents.ErrPlanRejected,
		"default-one validation still rejects the duplicate before reconciliation exists")
}

func TestRealTwoProducerMockRoundAcceptsAtMostSixteen(t *testing.T) {
	var sup *Supervisor
	var mu sync.Mutex
	ids := make(map[domain.AgentID]bool)
	runner := PlanRunnerFunc(func(ctx context.Context, firing Firing, req agents.PlanRequest) (domain.Plan, agents.Transcript, error) {
		mu.Lock()
		ids[req.AgentID] = true
		mu.Unlock()
		assert.Equal(t, 8, req.MaxBeads)
		assert.False(t, strings.Contains(strings.Join(req.Notes, "\n"), "deprioritise"))
		return SupervisorPlanRunner(sup).RunPlan(ctx, firing, req)
	})
	f := planningParallelFixture(t, "ship sixteen", runner, planningIndependentJudge())
	f.Planner.poPlanners = 0
	f.Planner.opts.PlannerCount = 0
	f.Planner.cfg.Effective.POPlanners = 2
	f.Planner.opts.NoShim = false
	f.Planner.opts.ShimOptions = beads.ShimOptions{Real: planningFakeBD(t), SkipVerify: true}

	sc := mock.NewScenario("two-planning-product-owners-and-a-judge")
	var decisions []map[string]string
	for i, id := range []string{"po-plan-1-p1", "po-plan-1-p2"} {
		plan := planningParallelPlan(id, 8)
		payload, err := json.Marshal(plan)
		require.NoError(t, err)
		sc.Rule(id, mock.Match{Agent: id}).Say("candidate ready\n").Structured(string(payload))
		for _, b := range plan.Beads {
			decisions = append(decisions, map[string]string{
				"candidate": fmt.Sprintf("p%d:%s", i+1, b.Key), "decision": string(domain.PlanReviewIndependent),
			})
		}
	}
	const judgeID = "po-plan-1-p1-judge"
	judgePayload, err := json.Marshal(map[string]any{"decisions": decisions})
	require.NoError(t, err)
	sc.Rule("semantic judge", mock.Match{Agent: judgeID}).Say("reviewed\n").Structured(string(judgePayload))
	judgeAdapter, err := mock.NewAdapter("mock", sc)
	require.NoError(t, err)
	repo, err := prompt.ScanRepo(f.Repo, prompt.ScanOptions{})
	require.NoError(t, err)
	var judgeUsage []cartridge.Usage
	f.Planner.opts.Reviewer = agents.StructuredPlanReviewRunner{
		Cartridge: judgeAdapter,
		Request: agents.PlanReviewRequest{
			Run: f.Run.ID(), Round: 1, Repo: repo, Brief: f.Planner.cfg.Run.Brief, Model: "opus",
			OnUsage: func(u cartridge.Usage) { judgeUsage = append(judgeUsage, u) },
		},
	}
	rebuilt, err := NewPlanner(f.Planner.opts)
	require.NoError(t, err)
	rebuilt.Attach(f.Run)
	f.Planner = rebuilt
	sup = planningSupervisor(t, f, sc)
	_, err = f.Planner.Fire(context.Background(), planningParallelFire(f))
	require.NoError(t, err)
	judgeCalls := judgeAdapter.Calls()
	require.Len(t, judgeCalls, 1, "the combined graph comes from one real semantic-judge session")
	assert.Equal(t, judgeID, judgeCalls[0].Spec.Labels.Get(cartridge.LabelAgent), "the judge id is derived from the round's first planner")
	assert.Contains(t, judgeCalls[0].Spec.Env["PATH"], PlanShimDirName, "the judge receives the producer's read-only bd shim")
	assert.Equal(t, "opus", judgeCalls[0].Spec.Model)
	require.Len(t, judgeUsage, 1)
	assert.True(t, UsageReported(judgeUsage[0]), "the real session reports usage to its metering hook")
	assert.Equal(t, 16, f.Planner.CombinedMaxBeads())
	assert.Equal(t, 2, len(ids))
	assert.Equal(t, 1, f.Applier.count())
	f.Applier.mu.Lock()
	req := f.Applier.calls[0]
	f.Applier.mu.Unlock()
	assert.LessOrEqual(t, len(req.Plan.Beads), 16)
	assert.Len(t, req.Plan.Beads, 16)
	assert.Equal(t, 1, f.Planner.Status().Plans)
	assert.Equal(t, 16, f.Planner.Status().Beads)
	evs := f.events()
	for _, ev := range evs {
		if warning, ok := ev.(events.Warning); ok {
			t.Logf("planning warning: %s: %s", warning.Message, warning.Err)
		}
	}
	reviews, produced := planningReviewEvents(evs)
	assert.Len(t, reviews, 16)
	for _, review := range reviews {
		assert.Equal(t, domain.PlanProvenanceJudge, review.Provenance)
	}
	require.Len(t, produced, 1)
	assert.Len(t, produced[0].Created, 16)
	assert.Len(t, produced[0].Contributors, 2)
	assert.Equal(t, domain.BeadID("sc-epic"), produced[0].Epic)
}
