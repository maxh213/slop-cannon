package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/structured"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

var (
	// ErrInvalidPlanReview means a candidate or judge decision cannot be
	// reconciled safely.
	ErrInvalidPlanReview = errors.New("agents: invalid plan review")
	// ErrCombinedPlanQuota means the producers exceeded the single combined
	// bead budget. Reconciliation never trims a graph to fit.
	ErrCombinedPlanQuota = errors.New("agents: combined plan bead quota exceeded")
)

// PlanReconciliationRequest contains a fresh existing-bead snapshot and the
// output of at most one judge attempt. Candidates may arrive in any order;
// planner IDs define their deterministic reconciliation order.
type PlanReconciliationRequest struct {
	Candidates []domain.PlanCandidate
	Existing   []domain.Bead
	Decisions  []domain.PlanReview
	// Env carries the coordinator's read-only shim environment to the semantic
	// judge. Reconciliation itself does not read or retain it.
	Env map[string]string
	// MaxBeads is the combined cap (po_planners × max_beads_per_plan), not a
	// per-producer allowance. It must be positive.
	MaxBeads int
	// JudgeFailed selects the no-retry fallback. An invalid judge response
	// selects the same fallback automatically.
	JudgeFailed bool
}

// PlanReconciliation is the single combined graph and one review record for
// every proposed bead, including duplicates and conflicts.
type PlanReconciliation struct {
	Plan    domain.CombinedPlan
	Reviews []domain.PlanReview
}

type planCandidateBead struct {
	bead          domain.PlanBead
	review        domain.PlanReview
	deterministic bool
}

// NamespacePlanCandidates returns deep-enough copies of candidates with keys
// and local dependencies rewritten to p<planner-position>:<original-key>.
// Candidates are sorted by planner ID; every planner must be unique.
func NamespacePlanCandidates(candidates []domain.PlanCandidate) ([]domain.PlanCandidate, error) {
	ordered := append([]domain.PlanCandidate(nil), candidates...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Planner < ordered[j].Planner })
	out := make([]domain.PlanCandidate, len(ordered))
	seen := make(map[domain.AgentID]bool, len(ordered))
	for i, candidate := range ordered {
		if strings.TrimSpace(string(candidate.Planner)) == "" {
			return nil, fmt.Errorf("%w: empty planner id", ErrInvalidPlanReview)
		}
		if seen[candidate.Planner] {
			return nil, fmt.Errorf("%w: planner %q appears more than once", ErrInvalidPlanReview, candidate.Planner)
		}
		seen[candidate.Planner] = true
		if err := candidate.Plan.Validate(); err != nil {
			return nil, fmt.Errorf("%w: planner %q: %w", ErrInvalidPlanReview, candidate.Planner, err)
		}
		prefix := "p" + strconv.Itoa(i+1) + ":"
		candidate.Plan.Beads = append([]domain.PlanBead(nil), candidate.Plan.Beads...)
		keys := make(map[string]string, len(candidate.Plan.Beads))
		for _, bead := range candidate.Plan.Beads {
			key := prefix + bead.Key
			if len(key) > domain.MaxPlanReviewReferenceLength {
				return nil, fmt.Errorf("%w: candidate key %q exceeds %d bytes", ErrInvalidPlanReview, key, domain.MaxPlanReviewReferenceLength)
			}
			keys[bead.Key] = key
		}
		for j, bead := range candidate.Plan.Beads {
			candidate.Plan.Beads[j].Key = keys[bead.Key]
			candidate.Plan.Beads[j].Deps = make([]string, len(bead.Deps))
			for k, dep := range bead.Deps {
				candidate.Plan.Beads[j].Deps[k] = keys[dep]
			}
		}
		out[i] = candidate
	}
	return out, nil
}

// ValidatePlanReviews checks a judge response without changing it. Decisions
// may name only known candidates; duplicates may canonicalise only to another
// known candidate or a fresh open existing bead. No decision can introduce an
// ID, rewrite prose, add a bead, or alter an edge.
func ValidatePlanReviews(candidates []domain.PlanCandidate, existing []domain.Bead, decisions []domain.PlanReview) error {
	known := make(map[string]bool)
	for _, candidate := range candidates {
		for _, bead := range candidate.Plan.Beads {
			known[bead.Key] = true
		}
	}
	existingIDs := openBeadIDs(existing)
	seen := make(map[string]bool, len(decisions))
	for _, decision := range decisions {
		if !known[decision.Candidate] {
			return fmt.Errorf("%w: unknown candidate %q", ErrInvalidPlanReview, decision.Candidate)
		}
		if seen[decision.Candidate] {
			return fmt.Errorf("%w: candidate %q classified more than once", ErrInvalidPlanReview, decision.Candidate)
		}
		seen[decision.Candidate] = true
		if !decision.Decision.Valid() {
			return fmt.Errorf("%w: candidate %q has decision %q", ErrInvalidPlanReview, decision.Candidate, decision.Decision)
		}
		switch decision.Decision {
		case domain.PlanReviewDuplicate:
			if !known[decision.Canonical] && !existingIDs[domain.BeadID(decision.Canonical)] {
				return fmt.Errorf("%w: duplicate %q has unknown canonical target %q", ErrInvalidPlanReview, decision.Candidate, decision.Canonical)
			}
			if decision.Canonical == decision.Candidate {
				return fmt.Errorf("%w: candidate %q duplicates itself", ErrInvalidPlanReview, decision.Candidate)
			}
		case domain.PlanReviewIndependent, domain.PlanReviewConflict:
			if decision.Canonical != "" {
				return fmt.Errorf("%w: %s candidate %q has a canonical target", ErrInvalidPlanReview, decision.Decision, decision.Candidate)
			}
		}
	}
	return nil
}

// ReconcilePlans combines concurrent PO output into one graph. Existing beads
// always win normalized-title matches, then the earliest candidate wins among
// producers. A judge may only classify unresolved candidates. On judge failure
// those candidates are accepted as independent only when deterministic
// matching found no ambiguity; there is deliberately no retry path here.
func ReconcilePlans(req PlanReconciliationRequest) (PlanReconciliation, error) {
	if req.MaxBeads <= 0 {
		return PlanReconciliation{}, fmt.Errorf("%w: combined bead cap must be positive", ErrInvalidPlanReview)
	}
	candidates, err := NamespacePlanCandidates(req.Candidates)
	if err != nil {
		return PlanReconciliation{}, err
	}
	total := 0
	for _, candidate := range candidates {
		total += len(candidate.Plan.Beads)
	}
	if total > req.MaxBeads {
		return PlanReconciliation{}, fmt.Errorf("%w: proposed %d beads, combined limit is %d", ErrCombinedPlanQuota, total, req.MaxBeads)
	}

	existingByTitle := make(map[string]domain.Bead)
	existingIDs := openBeadIDs(req.Existing)
	for _, bead := range req.Existing {
		if bead.State == domain.BeadStateClosed {
			continue
		}
		title := NormalisePlanTitle(bead.Title)
		if title == "" {
			continue
		}
		if _, found := existingByTitle[title]; !found {
			existingByTitle[title] = bead
		}
	}

	var beads []*planCandidateBead
	byKey := make(map[string]*planCandidateBead, total)
	byTitle := make(map[string]*planCandidateBead, total)
	for _, candidate := range candidates {
		for _, bead := range candidate.Plan.Beads {
			item := &planCandidateBead{bead: bead, review: domain.PlanReview{
				Planner: candidate.Planner, Candidate: bead.Key,
				Decision: domain.PlanReviewIndependent, Provenance: domain.PlanProvenanceFallback,
			}}
			beads = append(beads, item)
			byKey[bead.Key] = item
			title := NormalisePlanTitle(bead.Title)
			if existing, found := existingByTitle[title]; found {
				item.review.Decision = domain.PlanReviewDuplicate
				item.review.Canonical = string(existing.ID)
				item.review.Provenance = domain.PlanProvenanceExisting
				item.deterministic = true
				continue
			}
			if first, found := byTitle[title]; found && title != "" {
				item.review.Decision = domain.PlanReviewDuplicate
				item.review.Canonical = first.review.Candidate
				item.review.Provenance = domain.PlanProvenanceDeterministic
				item.deterministic = true
				continue
			}
			if title != "" {
				byTitle[title] = item
			}
		}
	}

	judgeFailed := req.JudgeFailed
	if !judgeFailed {
		if err := ValidatePlanReviews(candidates, req.Existing, req.Decisions); err != nil {
			judgeFailed = true
		} else {
			for _, decision := range req.Decisions {
				if byKey[decision.Candidate].deterministic {
					judgeFailed = true
					break
				}
			}
		}
	}
	if !judgeFailed {
		for _, decision := range req.Decisions {
			item := byKey[decision.Candidate]
			item.review.Decision = decision.Decision
			item.review.Canonical = decision.Canonical
			item.review.Provenance = domain.PlanProvenanceJudge
		}
	}
	for _, item := range beads {
		if item.deterministic {
			continue
		}
		if judgeFailed {
			item.review.Decision = domain.PlanReviewIndependent
			item.review.Canonical = ""
			item.review.Provenance = domain.PlanProvenanceFallback
		} else if item.review.Provenance != domain.PlanProvenanceJudge {
			item.review.Decision = domain.PlanReviewConflict
			item.review.Canonical = ""
		}
	}

	resolved := make(map[string]string, len(beads))
	var resolve func(string, map[string]bool) (string, bool)
	resolve = func(key string, visiting map[string]bool) (string, bool) {
		item := byKey[key]
		if item == nil || item.review.Decision == domain.PlanReviewConflict || visiting[key] {
			return "", false
		}
		if canonical, ok := resolved[key]; ok {
			return canonical, canonical != ""
		}
		if item.review.Decision == domain.PlanReviewIndependent {
			resolved[key] = key
			return key, true
		}
		visiting[key] = true
		canonical := item.review.Canonical
		if existingIDs[domain.BeadID(canonical)] {
			resolved[key] = canonical
			delete(visiting, key)
			return canonical, true
		}
		canonical, ok := resolve(canonical, visiting)
		delete(visiting, key)
		if !ok {
			item.review.Decision = domain.PlanReviewConflict
			item.review.Canonical = ""
			item.review.Provenance = domain.PlanProvenanceFallback
			resolved[key] = ""
			return "", false
		}
		item.review.Canonical = canonical
		resolved[key] = canonical
		return canonical, true
	}
	for _, item := range beads {
		if item.review.Decision == domain.PlanReviewDuplicate {
			resolve(item.review.Candidate, make(map[string]bool))
		}
	}

	// Reject dependents of conflicts rather than silently dropping an edge.
	changed := true
	for changed {
		changed = false
		resolved = make(map[string]string, len(beads))
		for _, item := range beads {
			if item.review.Decision != domain.PlanReviewIndependent {
				continue
			}
			for _, dep := range item.bead.Deps {
				if _, ok := resolve(dep, make(map[string]bool)); !ok {
					item.review.Decision = domain.PlanReviewConflict
					item.review.Canonical = ""
					item.review.Provenance = domain.PlanProvenanceFallback
					resolved[item.review.Candidate] = ""
					changed = true
					break
				}
			}
		}
	}

	out := PlanReconciliation{Reviews: make([]domain.PlanReview, 0, len(beads))}
	for _, item := range beads {
		out.Reviews = append(out.Reviews, item.review)
		if item.review.Decision != domain.PlanReviewIndependent {
			continue
		}
		bead := item.bead
		bead.Deps = nil
		for _, dep := range item.bead.Deps {
			canonical, ok := resolve(dep, make(map[string]bool))
			if !ok {
				return PlanReconciliation{}, fmt.Errorf("%w: unresolved dependency %q", ErrInvalidPlanReview, dep)
			}
			if byKey[canonical] != nil {
				bead.Deps = append(bead.Deps, canonical)
			} else {
				out.Plan.ExternalDeps = append(out.Plan.ExternalDeps, domain.PlanExternalDependency{FromKey: bead.Key, ToID: domain.BeadID(canonical)})
			}
		}
		out.Plan.Plan.Beads = append(out.Plan.Plan.Beads, bead)
	}
	if len(out.Plan.Plan.Beads) > req.MaxBeads {
		return PlanReconciliation{}, fmt.Errorf("%w: accepted %d beads, combined limit is %d", ErrCombinedPlanQuota, len(out.Plan.Plan.Beads), req.MaxBeads)
	}
	if err := out.Plan.Validate(); err != nil {
		return PlanReconciliation{}, fmt.Errorf("%w: reconciled graph: %w", ErrInvalidPlanReview, err)
	}
	return out, nil
}

func openBeadIDs(existing []domain.Bead) map[domain.BeadID]bool {
	out := make(map[domain.BeadID]bool, len(existing))
	for _, bead := range existing {
		if bead.State != domain.BeadStateClosed && validPlanReviewReference(string(bead.ID)) {
			out[bead.ID] = true
		}
	}
	return out
}

// PlanReviewRequest is the coordinator-facing input to one read-only semantic
// PO judge firing. Candidates are raw producer plans; the runner namespaces
// them exactly as ReconcilePlans does. Existing must be the fresh open/closed
// bead snapshot used by the subsequent reconciliation.
type PlanReviewRequest struct {
	Run        domain.RunID
	AgentID    domain.AgentID
	Round      int
	Workdir    string
	Repo       prompt.RepoContext
	Brief      string
	Rules      []prompt.Doc
	Vibes      []prompt.Doc
	Candidates []domain.PlanCandidate
	Existing   []domain.Bead
	Budget     prompt.Budget
	Model      string
	MaxTurns   int
	Timeout    time.Duration
	Env        map[string]string
	InheritEnv []string
	Observer   func(cartridge.SessionEvent)
	// OnUsage receives the returned Outcome.Usage exactly once, including on
	// failure. It carries metering data only, never prompt or profile text.
	OnUsage    func(cartridge.Usage)
	namespaced bool
}

// Dir returns the judge's working directory.
func (r PlanReviewRequest) Dir() string {
	if strings.TrimSpace(r.Workdir) != "" {
		return r.Workdir
	}
	return r.Repo.Root
}

// UseShim installs the read-only bd shim used by PO repository firings.
func (r *PlanReviewRequest) UseShim(s beads.Shim) {
	if r.Env == nil {
		r.Env = make(map[string]string, 1)
	}
	s.Apply(r.Env)
}

// Validate checks the firing context and every candidate before a session starts.
func (r PlanReviewRequest) Validate() error {
	switch {
	case strings.TrimSpace(string(r.Run)) == "":
		return fmt.Errorf("%w: empty run id", ErrInvalidPlanReview)
	case r.Round < 0:
		return fmt.Errorf("%w: negative round", ErrInvalidPlanReview)
	case strings.TrimSpace(r.Dir()) == "":
		return fmt.Errorf("%w: no repository root and no workdir", ErrInvalidPlanReview)
	case !filepath.IsAbs(r.Dir()):
		return fmt.Errorf("%w: workdir %q is not absolute", ErrInvalidPlanReview, r.Dir())
	}
	if _, err := r.judgeAgentID(); err != nil {
		return err
	}
	if r.namespaced {
		return validateNamespacedPlanReviewCandidates(r.Candidates)
	}
	_, err := NamespacePlanCandidates(r.Candidates)
	return err
}

func (r PlanReviewRequest) judgeAgentID() (domain.AgentID, error) {
	if strings.TrimSpace(string(r.AgentID)) != "" {
		return r.AgentID, nil
	}
	if len(r.Candidates) == 0 || strings.TrimSpace(string(r.Candidates[0].Planner)) == "" {
		return "", fmt.Errorf("%w: empty judge agent id and no first candidate planner available to derive it", ErrInvalidPlanReview)
	}
	return domain.AgentID(string(r.Candidates[0].Planner) + "-judge"), nil
}

func (r PlanReviewRequest) withJudgeAgentID() (PlanReviewRequest, error) {
	id, err := r.judgeAgentID()
	if err != nil {
		return r, err
	}
	r.AgentID = id
	return r, nil
}

func validateNamespacedPlanReviewCandidates(candidates []domain.PlanCandidate) error {
	seen := make(map[domain.AgentID]bool, len(candidates))
	for _, candidate := range candidates {
		if strings.TrimSpace(string(candidate.Planner)) == "" || seen[candidate.Planner] {
			return fmt.Errorf("%w: empty or duplicate planner id %q", ErrInvalidPlanReview, candidate.Planner)
		}
		seen[candidate.Planner] = true
		if err := candidate.Plan.Validate(); err != nil {
			return fmt.Errorf("%w: planner %q: %w", ErrInvalidPlanReview, candidate.Planner, err)
		}
		for _, bead := range candidate.Plan.Beads {
			if !validPlanReviewReference(bead.Key) {
				return fmt.Errorf("%w: malformed or oversized candidate key %q", ErrInvalidPlanReview, bead.Key)
			}
		}
	}
	return nil
}

const planReviewSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["decisions"],
  "properties": {
    "decisions": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["candidate", "decision"],
        "properties": {
          "candidate": {"type": "string", "minLength": 1, "maxLength": 256, "pattern": "^\\S+$"},
          "decision": {"enum": ["duplicate", "conflict", "independent"]},
          "canonical": {"type": "string", "minLength": 1, "maxLength": 256, "pattern": "^\\S+$"}
        }
      }
    }
  }
}`

var planReviewContracts = sync.OnceValues(func() (*structured.Contract, error) {
	return structured.NewContract("plan_review", []byte(planReviewSchema))
})

// PlanReviewContract returns the prose-free structured judge output contract.
func PlanReviewContract() (*structured.Contract, error) { return planReviewContracts() }

type planReviewResponse struct {
	Decisions []domain.PlanReview `json:"decisions"`
}

type planReviewPromptInput struct {
	Candidates []domain.PlanCandidate `json:"candidates"`
	Existing   []domain.Bead          `json:"existing"`
	JudgeOnly  []string               `json:"judge_only"`
}

// PlanReviewRunSpec builds the read-only PO firing. The coordinator normally
// calls RunPlanReview directly; this builder is exposed for prompt filing and
// cartridge enforcement checks. The task is events.TaskPlanReview.
func PlanReviewRunSpec(r PlanReviewRequest) (cartridge.RunSpec, prompt.Prompt, error) {
	if strings.TrimSpace(string(r.Run)) == "" {
		return cartridge.RunSpec{}, prompt.Prompt{}, fmt.Errorf("%w: empty run id", ErrInvalidPlanReview)
	}
	r, err := r.withJudgeAgentID()
	if err != nil {
		return cartridge.RunSpec{}, prompt.Prompt{}, err
	}
	candidates, judgeOnly, err := planReviewInput(r)
	if err != nil {
		return cartridge.RunSpec{}, prompt.Prompt{}, err
	}
	contract, err := PlanReviewContract()
	if err != nil {
		return cartridge.RunSpec{}, prompt.Prompt{}, err
	}
	policy, err := Policy(domain.RolePO, events.TaskPlanReview)
	if err != nil {
		return cartridge.RunSpec{}, prompt.Prompt{}, err
	}
	input, err := json.Marshal(planReviewPromptInput{Candidates: candidates, Existing: r.Existing, JudgeOnly: judgeOnly})
	if err != nil {
		return cartridge.RunSpec{}, prompt.Prompt{}, fmt.Errorf("%w: encoding judge input: %w", ErrInvalidPlanReview, err)
	}
	p, err := prompt.Build(prompt.Request{
		Role: domain.RolePO, Task: events.TaskPlanReview, AgentID: r.AgentID, RunID: r.Run,
		ToolPolicy: policy,
		Objective:  "Semantically reconcile the parallel planning candidates. Classify only the listed unresolved candidates; write no code and change no plan or tracker state.",
		Brief:      r.Brief, Rules: r.Rules, Vibes: r.Vibes, Repo: r.Repo, OpenBeads: r.Existing,
		Instructions: []prompt.Section{
			{Title: "Candidates and fresh existing beads", Body: string(input)},
			{Title: "How to judge", Body: "Return exactly one decision for every key in judge_only, and no others. " +
				"Use duplicate only with canonical set to another known candidate key or an open existing bead ID; " +
				"independent means the work is distinct and can coexist; conflict means it cannot safely coexist or you are uncertain. " +
				"Do not classify candidates outside judge_only: exact-title matches are already resolved deterministically. " +
				"You may compare all candidate and existing-bead prose, but your result must contain no prose, new IDs, rewritten beads, dependencies, graph, or quota changes. " +
				"When uncertain, choose conflict. Ask no questions."},
		},
		Contract: contract, Budget: r.Budget,
	})
	if err != nil {
		return cartridge.RunSpec{}, prompt.Prompt{}, err
	}
	spec := cartridge.RunSpec{
		Prompt: p.Text, Workdir: r.Dir(), Model: r.Model, MaxTurns: r.MaxTurns, Timeout: r.Timeout,
		Env: cloneEngineerEnv(r.Env), InheritEnv: append([]string(nil), r.InheritEnv...),
		Labels: cartridge.LabelsFor(r.Run, domain.Agent{ID: r.AgentID, Role: domain.RolePO}, events.TaskPlanReview, r.Round),
	}
	if err := ApplyPolicy(&spec, domain.RolePO, events.TaskPlanReview); err != nil {
		return cartridge.RunSpec{}, p, err
	}
	if err := spec.Validate(); err != nil {
		return cartridge.RunSpec{}, p, err
	}
	if p.OverBudget {
		return spec, p, promptBudgetError("plan review", p)
	}
	return spec, p, nil
}

// RunPlanReview fires the semantic judge exactly once. Malformed JSON,
// decisions, candidate/canonical IDs, incomplete answers, and cartridge
// failures all return an error with no decisions; structured retries are
// explicitly disabled. The coordinator must then make one ReconcilePlans call
// with JudgeFailed true and no decisions. A valid conflict decision is not an
// error: it is the deterministic answer for semantic uncertainty. The returned
// Outcome retains the single attempt and usage for supervisor accounting.
// When deterministic matching leaves nothing to judge, it returns no decisions
// and a zero Outcome without starting a session.
func RunPlanReview(ctx context.Context, c cartridge.Cartridge, r PlanReviewRequest) (decisions []domain.PlanReview, out structured.Outcome, err error) {
	if r.OnUsage != nil {
		defer func() { r.OnUsage(out.Usage) }()
	}
	spec, _, err := PlanReviewRunSpec(r)
	if err != nil {
		return nil, out, err
	}
	candidates, judgeOnly, err := planReviewInput(r)
	if err != nil {
		return nil, out, err
	}
	if len(judgeOnly) == 0 {
		return nil, out, nil
	}
	contract, err := PlanReviewContract()
	if err != nil {
		return nil, out, err
	}
	opts := []structured.Option{
		structured.WithMaxRetries(0),
		structured.WithCheck(func(parsed structured.Parsed, _ string) error {
			var response planReviewResponse
			if err := parsed.Decode(&response); err != nil {
				return err
			}
			return validatePlanReviewResponse(candidates, r.Existing, judgeOnly, response.Decisions)
		}),
	}
	if r.Observer != nil {
		opts = append(opts, structured.WithObserver(r.Observer))
	}
	out, err = structured.Fire(ctx, c, spec, contract, opts...)
	if err != nil {
		return nil, out, err
	}
	var response planReviewResponse
	if err := out.Parsed.Decode(&response); err != nil {
		return nil, out, err
	}
	planners := make(map[string]domain.AgentID)
	for _, candidate := range candidates {
		for _, bead := range candidate.Plan.Beads {
			planners[bead.Key] = candidate.Planner
		}
	}
	for i := range response.Decisions {
		response.Decisions[i].Planner = planners[response.Decisions[i].Candidate]
	}
	return response.Decisions, out, nil
}

// StructuredPlanReviewRunner adapts the real PO judge to the coordinator's
// ReviewPlans(context, PlanReconciliationRequest) interface. Request supplies
// the run, judge agent, repository, prompt and cartridge settings; each call
// replaces its Candidates, Existing and Env with the coordinator's fresh
// snapshot and cloned read-only shim environment. OnUsage is retained.
// The coordinator's candidates are already namespaced and are not prefixed a
// second time. ReviewPlans never retries; an error selects the coordinator's
// existing JudgeFailed fallback.
type StructuredPlanReviewRunner struct {
	Cartridge cartridge.Cartridge
	Request   PlanReviewRequest
}

// ReviewPlans implements the coordinator's semantic plan-review runner.
func (r StructuredPlanReviewRunner) ReviewPlans(ctx context.Context, req PlanReconciliationRequest) ([]domain.PlanReview, error) {
	review := r.Request
	review.Candidates = req.Candidates
	review.Existing = req.Existing
	review.Env = cloneEngineerEnv(req.Env)
	review.namespaced = true
	decisions, _, err := RunPlanReview(ctx, r.Cartridge, review)
	return decisions, err
}

func planReviewInput(r PlanReviewRequest) ([]domain.PlanCandidate, []string, error) {
	if err := r.Validate(); err != nil {
		return nil, nil, err
	}
	candidates := append([]domain.PlanCandidate(nil), r.Candidates...)
	if !r.namespaced {
		var err error
		candidates, err = NamespacePlanCandidates(r.Candidates)
		if err != nil {
			return nil, nil, err
		}
	}
	resolved := deterministicPlanReviewKeys(candidates, r.Existing)
	var judgeOnly []string
	for _, candidate := range candidates {
		for _, bead := range candidate.Plan.Beads {
			if !resolved[bead.Key] {
				judgeOnly = append(judgeOnly, bead.Key)
			}
		}
	}
	return candidates, judgeOnly, nil
}

func deterministicPlanReviewKeys(candidates []domain.PlanCandidate, existing []domain.Bead) map[string]bool {
	titles := make(map[string]bool)
	for _, bead := range existing {
		if bead.State != domain.BeadStateClosed {
			if title := NormalisePlanTitle(bead.Title); title != "" {
				titles[title] = true
			}
		}
	}
	resolved := make(map[string]bool)
	for _, candidate := range candidates {
		for _, bead := range candidate.Plan.Beads {
			title := NormalisePlanTitle(bead.Title)
			if title != "" && titles[title] {
				resolved[bead.Key] = true
			}
			if title != "" {
				titles[title] = true
			}
		}
	}
	return resolved
}

func validatePlanReviewResponse(candidates []domain.PlanCandidate, existing []domain.Bead, judgeOnly []string, decisions []domain.PlanReview) error {
	if err := ValidatePlanReviews(candidates, existing, decisions); err != nil {
		return err
	}
	resolved := deterministicPlanReviewKeys(candidates, existing)
	seen := make(map[string]bool, len(decisions))
	for _, decision := range decisions {
		if resolved[decision.Candidate] {
			return fmt.Errorf("%w: candidate %q was already resolved deterministically", ErrInvalidPlanReview, decision.Candidate)
		}
		seen[decision.Candidate] = true
	}
	for _, key := range judgeOnly {
		if !seen[key] {
			return fmt.Errorf("%w: unresolved candidate %q has no decision", ErrInvalidPlanReview, key)
		}
	}
	if len(decisions) != len(judgeOnly) {
		return fmt.Errorf("%w: got %d decisions for %d unresolved candidates", ErrInvalidPlanReview, len(decisions), len(judgeOnly))
	}
	return nil
}

func validPlanReviewReference(s string) bool {
	return s != "" && len(s) <= domain.MaxPlanReviewReferenceLength && !strings.ContainsFunc(s, unicode.IsSpace)
}
