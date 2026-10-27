package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// PlanReviewRunner performs the one semantic review allowed in a combined
// planning round. Candidates have already been namespaced. Implementations
// must classify rather than rewrite them; ReconcilePlans validates the answer.
type PlanReviewRunner interface {
	ReviewPlans(ctx context.Context, req agents.PlanReconciliationRequest) ([]domain.PlanReview, error)
}

var _ PlanReviewRunner = agents.StructuredPlanReviewRunner{}

// PlanReviewRunnerFunc adapts a function to PlanReviewRunner.
type PlanReviewRunnerFunc func(context.Context, agents.PlanReconciliationRequest) ([]domain.PlanReview, error)

// ReviewPlans implements PlanReviewRunner.
func (fn PlanReviewRunnerFunc) ReviewPlans(ctx context.Context, req agents.PlanReconciliationRequest) ([]domain.PlanReview, error) {
	return fn(ctx, req)
}

// planningCommit is the review and graph state shared by the original apply
// and any recovery firing. Only the current round owner accesses its contents;
// mu publishes the pointer and the round's completion channel.
type planningCommit struct {
	round            int
	reconciliation   agents.PlanReconciliation
	contributors     []domain.AgentID
	result           beads.PlanResult
	reviewsPublished bool
	planPublished    bool
}

type planningProducerResult struct {
	planner domain.AgentID
	request agents.PlanRequest
	plan    domain.Plan
	err     error
}

func planningCandidateRequest(req agents.PlanRequest) agents.PlanRequest {
	req.OpenBeads = nil
	return req
}

func parallelPlanAgent(id domain.AgentID) bool {
	text := strings.TrimPrefix(string(id), "po-plan-")
	if text == string(id) {
		return false
	}
	round, planner, found := strings.Cut(text, "-p")
	if !found || round == "" || planner == "" {
		return false
	}
	for _, part := range []string{round, planner} {
		if _, err := strconv.Atoi(part); err != nil {
			return false
		}
	}
	return true
}

// fireParallel runs every producer to completion before reviewing or writing
// anything. Worker results occupy their planner position, never completion
// order, and no worker has access to the applier or run epic. Concurrent Fire
// callers wait on a channel rather than a mutex held across external calls.
func (p *Planner) fireParallel(ctx context.Context, f Firing) (map[string]domain.BeadID, error) {
	for {
		p.mu.Lock()
		if p.parallelDone != nil {
			done := p.parallelDone
			p.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		p.parallelDone = make(chan struct{})
		pending := p.commit
		p.mu.Unlock()
		defer func() {
			p.mu.Lock()
			close(p.parallelDone)
			p.parallelDone = nil
			p.mu.Unlock()
		}()

		if pending != nil {
			created, adopted, err := p.adoptPlanningCommit(ctx, f, pending)
			if err != nil {
				return nil, err
			}
			if adopted {
				p.mu.Lock()
				p.commit = nil
				p.mu.Unlock()
				return created, nil
			}
			p.mu.Lock()
			p.commit = nil
			p.mu.Unlock()
		}
		return p.produceParallel(ctx, f)
	}
}

func (p *Planner) produceParallel(ctx context.Context, f Firing) (map[string]domain.BeadID, error) {
	p.mu.Lock()
	round := p.attempts
	p.outstanding = p.poPlanners
	p.mu.Unlock()

	count := p.poPlanners
	results := make([]planningProducerResult, count)
	width := len(strconv.Itoa(count))
	var wg sync.WaitGroup
	wg.Add(count)
	for i := 0; i < count; i++ {
		go func() {
			defer wg.Done()
			defer func() {
				p.mu.Lock()
				p.outstanding--
				p.mu.Unlock()
			}()
			id := domain.AgentID(fmt.Sprintf("po-plan-%d-p%0*d", round, width, i+1))
			results[i].planner = id
			worker := f
			worker.Agent.ID = id
			worker.Update = nil
			req, err := p.request(ctx, worker)
			if err != nil {
				results[i].err = fmt.Errorf("planner %s: %w", id, err)
				return
			}
			req.AgentID = id
			plan, _, err := p.opts.Runner.RunPlan(ctx, worker, req)
			if err == nil {
				err = agents.ValidatePlan(planningCandidateRequest(req), plan)
			}
			results[i].request, results[i].plan = req, plan
			if err != nil {
				results[i].err = fmt.Errorf("planner %s: %w", id, err)
			}
		}()
	}
	wg.Wait()

	candidates := make([]domain.PlanCandidate, 0, count)
	producers := make([]planningProducerResult, 0, count)
	failures := make([]error, 0, count)
	for _, result := range results {
		if result.err != nil {
			failures = append(failures, result.err)
			continue
		}
		producers = append(producers, result)
		candidates = append(candidates, domain.PlanCandidate{Planner: result.planner, Plan: result.plan})
	}
	for _, err := range failures {
		p.warn("a planning producer failed; retaining the other candidates", err)
	}
	if len(candidates) == 0 {
		return nil, errors.Join(failures...)
	}
	if p.opts.Applier == nil {
		return nil, ErrNoPlanApplier
	}
	proposed := 0
	for _, candidate := range candidates {
		proposed += len(candidate.Plan.Beads)
	}
	if proposed > p.CombinedMaxBeads() {
		return nil, fmt.Errorf("%w: proposed %d beads, combined limit is %d",
			agents.ErrCombinedPlanQuota, proposed, p.CombinedMaxBeads())
	}

	existing, err := p.freshPlanSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	namespaced, err := agents.NamespacePlanCandidates(candidates)
	if err != nil {
		return nil, err
	}
	reviewReq := agents.PlanReconciliationRequest{
		Candidates: namespaced,
		Existing:   existing,
		Env:        producers[0].request.Env,
		MaxBeads:   p.CombinedMaxBeads(),
	}
	var decisions []domain.PlanReview
	judgeFailed := p.opts.Reviewer == nil
	if !judgeFailed {
		decisions, err = p.opts.Reviewer.ReviewPlans(ctx, reviewReq)
		if err != nil {
			judgeFailed = true
			decisions = nil
			p.warn("the semantic plan judge failed; using the no-retry reconciliation fallback", err)
		}
	}
	reconciled, err := agents.ReconcilePlans(agents.PlanReconciliationRequest{
		Candidates:  candidates,
		Existing:    existing,
		Decisions:   decisions,
		MaxBeads:    p.CombinedMaxBeads(),
		JudgeFailed: judgeFailed,
	})
	if err != nil {
		return nil, err
	}
	reconciled.Plan.Plan.Brief = candidates[0].Plan.Brief

	applyReq := producers[0].request
	applyReq.AgentID = f.Agent.ID
	applyReq.MaxBeads = p.CombinedMaxBeads()
	applyReq.OpenBeads = existing
	commit := &planningCommit{
		round: round, reconciliation: reconciled,
		contributors: planningContributors(reconciled.Reviews),
	}
	p.publishPlanningReviews(commit)
	if err := agents.ValidatePlan(applyReq, reconciled.Plan.Plan); err != nil {
		return nil, err
	}
	if len(reconciled.Plan.Plan.Beads) == 0 {
		p.publishPlanningPlan(f, commit, nil, "")
		return nil, fmt.Errorf("%w (run %s)", agents.ErrEmptyPlan, p.cfg.Run.ID)
	}

	p.mu.Lock()
	p.commit = commit
	p.mu.Unlock()
	graphReq := agents.BeadsPlanRequest(applyReq, reconciled.Plan.Plan)
	graphReq.ExternalDeps = append([]domain.PlanExternalDependency(nil), reconciled.Plan.ExternalDeps...)
	res, err := p.opts.Applier.ApplyPlan(ctx, graphReq, false)
	commit.result = res
	if err != nil {
		return res.IDs, err
	}
	p.finishPlanningCommit(f, commit, res)
	p.mu.Lock()
	p.commit = nil
	p.mu.Unlock()
	return res.IDs, nil
}

// freshPlanSnapshot is deliberately taken after the producer barrier. The
// judge and deterministic reconciler therefore see the same current graph,
// not the independent and potentially stale prompt snapshots.
func (p *Planner) freshPlanSnapshot(ctx context.Context) ([]domain.Bead, error) {
	if p.opts.Beads == nil {
		return nil, nil
	}
	list, err := p.opts.Beads(ctx)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: planning: fresh reconciliation snapshot: %w", err)
	}
	out := make([]domain.Bead, 0, len(list))
	for _, b := range list {
		if !planInfrastructure(b) {
			out = append(out, b)
		}
	}
	return out, nil
}

func planningContributors(reviews []domain.PlanReview) []domain.AgentID {
	seen := make(map[domain.AgentID]bool)
	var out []domain.AgentID
	for _, review := range reviews {
		if review.Decision == domain.PlanReviewIndependent && !seen[review.Planner] {
			seen[review.Planner] = true
			out = append(out, review.Planner)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (p *Planner) publishPlanningReviews(commit *planningCommit) {
	if commit.reviewsPublished {
		return
	}
	r := p.host()
	if r == nil {
		return
	}
	for _, review := range commit.reconciliation.Reviews {
		r.Publish(events.PlanReviewed{
			Meta: r.Meta(), Round: commit.round, Planner: review.Planner,
			Candidate: review.Candidate, Decision: review.Decision,
			Canonical: review.Canonical, Provenance: review.Provenance,
		})
	}
	commit.reviewsPublished = true
}

func (p *Planner) publishPlanningPlan(f Firing, commit *planningCommit, created map[string]domain.BeadID, epic domain.BeadID) {
	if commit.planPublished {
		return
	}
	r := p.host()
	if r == nil {
		return
	}
	if epic == "" {
		epic = r.Epic()
	}
	r.Publish(events.PlanProduced{
		Meta: r.Meta(), Agent: f.Agent.ID, Epic: epic,
		Plan: commit.reconciliation.Plan.Plan, Created: created,
		Contributors: append([]domain.AgentID(nil), commit.contributors...),
	})
	commit.planPublished = true
}

func (p *Planner) finishPlanningCommit(f Firing, commit *planningCommit, res beads.PlanResult) {
	if res.EpicCreated && res.Epic != "" && f.Run != nil {
		if err := f.Run.SetEpic(res.Epic); err != nil {
			p.warn("could not record the run epic "+string(res.Epic), err)
		}
	}
	for _, warning := range res.Warnings {
		p.warn("bd reported a warning while creating the plan: "+warning, nil)
	}
	p.publishPlanningPlan(f, commit, res.IDs, res.Epic)
}

// adoptPlanningCommit runs before any retry. Returned IDs are positive
// evidence that the graph landed even when finalisation failed; otherwise a
// fresh run-scoped listing decides whether the old graph must be adopted.
func (p *Planner) adoptPlanningCommit(ctx context.Context, f Firing, commit *planningCommit) (map[string]domain.BeadID, bool, error) {
	res := commit.result
	evidence := res.Epic != "" && (len(res.IDs) > 0 || len(res.Beads) > 0 || len(res.Unfinished) > 0)
	var list []domain.Bead
	var err error
	if p.opts.Beads != nil {
		list, err = p.opts.Beads(ctx)
		if err != nil && !evidence {
			return nil, false, fmt.Errorf("orchestrator: planning: adopting the previous graph: %w", err)
		}
	} else if !evidence {
		return nil, false, errors.New("orchestrator: planning: cannot verify the previous graph without a bead source")
	}

	created := make(map[string]domain.BeadID, len(commit.reconciliation.Plan.Plan.Beads))
	for key, id := range res.IDs {
		created[key] = id
	}
	byTitle := make(map[string]domain.BeadID, len(list))
	epic, work := res.Epic, 0
	for _, b := range list {
		if b.Type == domain.BeadTypeEpic {
			if epic == "" {
				epic = b.ID
			}
			continue
		}
		if planInfrastructure(b) {
			continue
		}
		work++
		byTitle[agents.NormalisePlanTitle(b.Title)] = b.ID
	}
	for _, b := range commit.reconciliation.Plan.Plan.Beads {
		if created[b.Key] == "" {
			created[b.Key] = byTitle[agents.NormalisePlanTitle(b.Title)]
		}
	}
	for key, id := range created {
		if id == "" {
			delete(created, key)
		}
	}
	adopted := evidence || (epic != "" && work > 0)
	if !adopted {
		return nil, false, nil
	}
	res.Epic = epic
	res.EpicCreated = epic != ""
	res.IDs = created
	p.finishPlanningCommit(f, commit, res)
	return created, true, nil
}
