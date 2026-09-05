package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/testutil"
)

// P9-01: every `[limits]` key is resolved in one place and the two budget
// keys behave as D-18 says — a crossed cap stops new work, finishes the
// reviews in flight, then pauses an autopilot run and finishes a brief one.

// limPauser stands in for the run's pause gate.
type limPauser struct {
	mu        sync.Mutex
	reasons   []string
	resumes   []string
	paused    bool
	err       error
	resumeErr error
}

func (p *limPauser) Pause(reason string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reasons = append(p.reasons, reason)
	if p.err != nil {
		return p.err
	}
	p.paused = true
	return nil
}

func (p *limPauser) Paused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paused
}

func (p *limPauser) resume() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused = false
}

// Resume is [BudgetResumer]: what *Run does when the budget is raised from
// the TUI (P9-14).
func (p *limPauser) Resume(reason string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resumes = append(p.resumes, reason)
	if p.resumeErr != nil {
		return p.resumeErr
	}
	p.paused = false
	return nil
}

func (p *limPauser) resumed() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.resumes...)
}

func (p *limPauser) pauses() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.reasons...)
}

// limPhasedPauser is a pauser that also knows the run's lifecycle phase,
// as *Run does ([BudgetPhaser]).
type limPhasedPauser struct {
	limPauser
	pmu   sync.Mutex
	phase Phase
}

func (p *limPhasedPauser) Phase() Phase {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	return p.phase
}

// stop is what [Run.Stop] does to a paused run: the phase moves on and the
// pause gate is opened, so Paused() is false although nobody resumed it.
func (p *limPhasedPauser) stop() {
	p.pmu.Lock()
	p.phase = PhaseStopping
	p.pmu.Unlock()
	p.resume()
}

// limHost is an EventHost that keeps every event, on a fixed clock.
type limHost struct {
	mu     sync.Mutex
	events []events.Event
}

func (h *limHost) Publish(ev events.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, ev)
}

func (h *limHost) Meta() events.Meta {
	return events.MetaAt("run-limits", time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
}

func (h *limHost) Log() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// warnings returns the messages of every events.Warning published.
func (h *limHost) warnings() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, ev := range h.events {
		if w, ok := ev.(events.Warning); ok {
			out = append(out, w.Message)
		}
	}
	return out
}

// limWork is a fixed work source.
type limWork []Demand

func (w limWork) Demands(context.Context) ([]Demand, error) { return append([]Demand(nil), w...), nil }

// limBudget builds a budget over the caps, in the mode, with a fake pauser
// and host.
func limBudget(t *testing.T, mode domain.RunMode, tokens int64, cost float64) (*RunBudget, *limPauser, *limHost) {
	t.Helper()
	p, h := &limPauser{}, &limHost{}
	b := NewRunBudget(RunBudgetOptions{
		Limits: config.Limits{MaxTokens: tokens, MaxCostUSD: cost},
		Mode:   mode,
		Run:    p,
		Host:   h,
	})
	return b, p, h
}

// limRunConfig is a run config with the given limits, enough for New.
func limRunConfig(t *testing.T, mode domain.RunMode, lim config.Limits) *config.RunConfig {
	t.Helper()
	dir := t.TempDir()
	eff := config.Defaults()
	eff.Limits = lim
	return &config.RunConfig{
		Run: domain.Run{
			ID:       "20260904-120000-abcd",
			RepoPath: dir,
			StateDir: filepath.Join(dir, "state"),
			Mode:     mode,
		},
		Effective: &config.Effective{Config: eff},
	}
}

func TestEffectiveLimitsFallsBackToTheDefaultForEveryUnsetLimit(t *testing.T) {
	t.Parallel()
	def := config.Defaults().Limits

	for _, tc := range []struct {
		name string
		cfg  *config.RunConfig
	}{
		{"no config at all", nil},
		{"no effective configuration", &config.RunConfig{}},
		{"a zero-valued limits table", &config.RunConfig{Effective: &config.Effective{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := EffectiveLimits(tc.cfg)
			if got.MaxReviewRounds != def.MaxReviewRounds {
				t.Errorf("max_review_rounds = %d, want the default %d", got.MaxReviewRounds, def.MaxReviewRounds)
			}
			if got.MaxPushbackRounds != def.MaxPushbackRounds {
				t.Errorf("max_pushback_rounds = %d, want the default %d", got.MaxPushbackRounds, def.MaxPushbackRounds)
			}
			if got.AgentTimeout.Duration() != def.AgentTimeout.Duration() {
				t.Errorf("agent_timeout = %s, want the default %s", got.AgentTimeout, def.AgentTimeout)
			}
			if got.MaxPlanRejections != def.MaxPlanRejections {
				t.Errorf("max_plan_rejections = %d, want the default %d", got.MaxPlanRejections, def.MaxPlanRejections)
			}
			if got.MaxPlanFailures != def.MaxPlanFailures {
				t.Errorf("max_plan_failures = %d, want the default %d", got.MaxPlanFailures, def.MaxPlanFailures)
			}
			if got.MaxTokens != 0 || got.MaxCostUSD != 0 {
				t.Errorf("the opt-in budgets default to unlimited, got %d tokens / $%v",
					got.MaxTokens, got.MaxCostUSD)
			}
		})
	}
}

func TestEffectiveLimitsKeepsEveryConfiguredValue(t *testing.T) {
	t.Parallel()
	want := config.Limits{
		MaxReviewRounds:   3,
		MaxPushbackRounds: 1,
		AgentTimeout:      config.NewDuration(90 * time.Second),
		MaxTokens:         1_000_000,
		MaxCostUSD:        12.5,
		MaxPlanRejections: 4,
		MaxPlanFailures:   7,
	}
	got := EffectiveLimits(limRunConfig(t, domain.RunModeBrief, want))
	if got != want {
		t.Errorf("EffectiveLimits = %+v, want %+v", got, want)
	}
}

func TestEffectiveLimitsReadsANegativeBudgetAsUnlimited(t *testing.T) {
	t.Parallel()
	got := EffectiveLimits(limRunConfig(t, domain.RunModeAutopilot, config.Limits{
		MaxTokens:  -1,
		MaxCostUSD: -0.5,
	}))
	if got.MaxTokens != 0 || got.MaxCostUSD != 0 {
		t.Errorf("a negative budget must read as unlimited, got %d tokens / $%v", got.MaxTokens, got.MaxCostUSD)
	}
	if NewRunBudget(RunBudgetOptions{Limits: got}).Enabled() {
		t.Error("a budget over unlimited caps must not be enabled")
	}
}

func TestAgentTimeoutLimitReachesTheSupervisor(t *testing.T) {
	t.Parallel()
	cfg := limRunConfig(t, domain.RunModeBrief, config.Limits{AgentTimeout: config.NewDuration(7 * time.Minute)})
	run, err := New(cfg, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sup, err := NewSupervisor(SupervisorOptions{})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	sup.Attach(run)
	if got := sup.Timeout(); got != 7*time.Minute {
		t.Errorf("supervisor timeout = %s, want the configured limits.agent_timeout 7m", got)
	}
}

func TestRunBudgetIsEnforcedOnlyAsFarAsCartridgesReportUsage(t *testing.T) {
	t.Parallel()
	b, p, _ := limBudget(t, domain.RunModeAutopilot, 0, 1.0)

	// A cartridge that reports tokens but no price never trips a cost cap.
	for range 5 {
		b.Record(cartridge.Usage{InputTokens: 1000, OutputTokens: 1000})
	}
	if b.Exceeded() || b.Tripped() {
		t.Fatalf("the cost cap fired without a reported cost: %+v", b.Spent())
	}
	if got := b.Spent(); got.Tokens != 10_000 || got.Firings != 5 || got.HasCost {
		t.Errorf("Spent = %+v, want 10000 tokens over 5 firings with no cost", got)
	}
	if len(p.pauses()) != 0 {
		t.Errorf("the run was paused over an unreported cost: %v", p.pauses())
	}

	// The moment one reports a price, the cap is enforceable.
	b.Record(cartridge.Usage{CostUSD: 1.25, HasCost: true})
	if !b.Tripped() {
		t.Fatal("the cost cap did not fire once a cartridge reported a cost")
	}
}

func TestRunBudgetPausesAnAutopilotRunOverItsTokenCap(t *testing.T) {
	t.Parallel()
	b, p, h := limBudget(t, domain.RunModeAutopilot, 1000, 0)

	b.Record(cartridge.Usage{InputTokens: 400, OutputTokens: 100})
	if b.Tripped() {
		t.Fatal("the budget fired under its cap")
	}
	if got := len(p.pauses()); got != 0 {
		t.Fatalf("the run was paused under its cap (%d times)", got)
	}

	b.Record(cartridge.Usage{InputTokens: 400, OutputTokens: 200})
	if !b.Tripped() {
		t.Fatal("the budget did not fire at its cap")
	}
	if got := p.pauses(); len(got) != 1 || got[0] != PauseReasonTokenCap {
		t.Fatalf("pauses = %v, want one %q", got, PauseReasonTokenCap)
	}
	if got := b.Reason(); got != PauseReasonTokenCap {
		t.Errorf("Reason = %q, want %q", got, PauseReasonTokenCap)
	}
	banner := b.Banner()
	for _, want := range []string{"1100 tokens", "1000", "limits.max_tokens", "paused"} {
		if !strings.Contains(banner, want) {
			t.Errorf("banner %q does not mention %q", banner, want)
		}
	}
	warned := h.warnings()
	if len(warned) != 1 || warned[0] != banner {
		t.Errorf("published warnings = %v, want the banner %q", warned, banner)
	}
}

func TestRunBudgetPausesAnAutopilotRunOverItsCostCap(t *testing.T) {
	t.Parallel()
	b, p, _ := limBudget(t, domain.RunModeAutopilot, 0, 10)

	b.Record(cartridge.Usage{InputTokens: 10, CostUSD: 4, HasCost: true})
	b.Record(cartridge.Usage{InputTokens: 10, CostUSD: 6.5, HasCost: true})

	if got := p.pauses(); len(got) != 1 || got[0] != PauseReasonCostCap {
		t.Fatalf("pauses = %v, want one %q", got, PauseReasonCostCap)
	}
	if got := b.Banner(); !strings.Contains(got, "$10.50") || !strings.Contains(got, "limits.max_cost_usd") {
		t.Errorf("banner = %q, want the spend and the key", got)
	}
	if got := b.Spent(); !got.HasCost || got.CostUSD != 10.5 {
		t.Errorf("Spent = %+v, want $10.50 with a cost", got)
	}
}

func TestRunBudgetStopsNewWorkButFinishesTheReviewsInFlight(t *testing.T) {
	t.Parallel()
	b, _, _ := limBudget(t, domain.RunModeBrief, 100, 0)
	work := limWork{
		{Kind: DemandImplement, Bead: "b-1"},
		{Kind: DemandReview, Bead: "b-2", Lens: domain.LensQA, Round: 1},
		{Kind: DemandArbitrate, Bead: "b-2", Round: 1},
		{Kind: DemandPlan},
	}
	gated := b.Work(work)

	before, err := gated.Demands(t.Context())
	if err != nil {
		t.Fatalf("Demands: %v", err)
	}
	if len(before) != 4 {
		t.Fatalf("an untripped budget gated %d of 4 demands", 4-len(before))
	}

	b.Record(cartridge.Usage{InputTokens: 100, OutputTokens: 100})
	after, err := gated.Demands(t.Context())
	if err != nil {
		t.Fatalf("Demands: %v", err)
	}
	var kinds []DemandKind
	for _, d := range after {
		kinds = append(kinds, d.Kind)
	}
	if len(kinds) != 2 || kinds[0] != DemandReview || kinds[1] != DemandArbitrate {
		t.Fatalf("a tripped budget offered %v, want only the review and the arbitration", kinds)
	}
}

func TestRunBudgetFinishesABriefRunWithTheReasonInTheSummary(t *testing.T) {
	t.Parallel()
	b, p, _ := limBudget(t, domain.RunModeBrief, 0, 5)
	stop := b.Stop(StopConditionFunc(func(context.Context, *Run, SchedulerStatus) (Outcome, bool, error) {
		return Outcome{}, false, nil
	}))

	b.Record(cartridge.Usage{CostUSD: 6, HasCost: true})
	if got := p.pauses(); len(got) != 0 {
		t.Fatalf("a brief run was paused over its budget: %v", got)
	}

	// The reviews in flight finish first: nothing is decided while an agent
	// is alive or the last dispatch fired something.
	if _, done, err := stop.Done(t.Context(), nil, SchedulerStatus{Live: 1}); done || err != nil {
		t.Fatalf("the run finished with an agent still running (done=%v err=%v)", done, err)
	}
	if _, done, err := stop.Done(t.Context(), nil, SchedulerStatus{Idle: false}); done || err != nil {
		t.Fatalf("the run finished before the loop went idle (done=%v err=%v)", done, err)
	}

	out, done, err := stop.Done(t.Context(), nil, SchedulerStatus{Idle: true})
	if err != nil || !done {
		t.Fatalf("Done = (%v, %v), want the run to finish", done, err)
	}
	// P9-12: the reason names the budget. It is not events.FinishStopped
	// (the user pressed `s`) and not events.FinishCompleted (the beads
	// settled), because neither is what happened.
	if out.Reason != events.FinishBudgetReached {
		t.Errorf("finish reason = %q, want %q", out.Reason, events.FinishBudgetReached)
	}
	for _, want := range []string{"$6.00", "$5.00", "limits.max_cost_usd", "agent session"} {
		if !strings.Contains(out.Summary, want) {
			t.Errorf("summary %q does not mention %q", out.Summary, want)
		}
	}
}

func TestRunBudgetNeverFinishesAnAutopilotRun(t *testing.T) {
	t.Parallel()
	b, _, _ := limBudget(t, domain.RunModeAutopilot, 10, 0)
	inner := StopConditionFunc(func(context.Context, *Run, SchedulerStatus) (Outcome, bool, error) {
		return Outcome{Reason: events.FinishCreditsExhausted}, true, nil
	})
	stop := b.Stop(inner)

	b.Record(cartridge.Usage{InputTokens: 100})
	if !b.Tripped() {
		t.Fatal("the budget did not fire")
	}
	out, done, err := stop.Done(t.Context(), nil, SchedulerStatus{Idle: true})
	if err != nil {
		t.Fatalf("Done: %v", err)
	}
	// It finished only because the inner condition said so (D-18: a cap
	// pauses an autopilot run, it never ends it).
	if !done || out.Reason != events.FinishCreditsExhausted {
		t.Errorf("Done = (%+v, %v), want the inner condition's outcome", out, done)
	}
}

func TestRunBudgetGrantsMoreHeadroomWhenTheUserResumes(t *testing.T) {
	t.Parallel()
	b, p, h := limBudget(t, domain.RunModeAutopilot, 100, 0)
	gated := b.Work(limWork{{Kind: DemandImplement, Bead: "b-1"}})

	b.Record(cartridge.Usage{InputTokens: 250})
	if !p.Paused() {
		t.Fatal("the run was not paused over its budget")
	}
	if ds, err := gated.Demands(t.Context()); err != nil || len(ds) != 0 {
		t.Fatalf("a paused budget offered %d demands (err=%v)", len(ds), err)
	}

	p.resume()
	ds, err := gated.Demands(t.Context())
	if err != nil {
		t.Fatalf("Demands: %v", err)
	}
	if len(ds) != 1 {
		t.Fatalf("a resumed run was offered %d demands, want 1", len(ds))
	}
	if b.Tripped() || b.Exceeded() {
		t.Error("a resumed run is still over its budget")
	}
	if got := b.Grants(); got != 3 {
		t.Errorf("Grants = %d, want 3: enough ceilings to put 250 tokens under 100 each", got)
	}
	if got := h.warnings(); len(got) != 2 || !strings.Contains(got[1], "300 tokens") {
		t.Errorf("warnings = %v, want a notice naming the new ceiling", got)
	}

	// The next budget's worth pauses it again.
	b.Record(cartridge.Usage{InputTokens: 100})
	if !b.Tripped() {
		t.Fatal("a resumed run did not pause again after another budget's worth")
	}
	if got := len(p.pauses()); got != 2 {
		t.Errorf("pauses = %d, want 2", got)
	}
}

// TestRunBudgetStillOffersADeliveryConflictRound is P9-01's other half of
// "the work in flight finishes": a merge-conflict round is a DemandImplement
// from the deliverer over a bead it has already moved to in_progress (D-10),
// so holding it back strands that bead half-delivered — with no engineer
// offered and, in brief mode, the run finishing as soon as the loop is idle.
func TestRunBudgetStillOffersADeliveryConflictRound(t *testing.T) {
	t.Parallel()
	b, _, _ := limBudget(t, domain.RunModeBrief, 100, 0)
	conflict := Demand{
		Kind: DemandImplement, Bead: "b-3", Round: 2,
		Task: events.TaskResolveConflict, Claim: ClaimRework, From: domain.BeadStateInProgress,
	}
	gated := b.Work(limWork{
		{Kind: DemandImplement, Bead: "b-1"},
		{Kind: DemandImplement, Bead: "b-2", Task: events.TaskAddressFeedback, Round: 2},
		conflict,
		{Kind: DemandPlan},
	})

	b.Record(cartridge.Usage{InputTokens: 200})
	after, err := gated.Demands(t.Context())
	if err != nil {
		t.Fatalf("Demands: %v", err)
	}
	if len(after) != 1 || after[0].Bead != conflict.Bead || after[0].FiringTask() != events.TaskResolveConflict {
		t.Fatalf("a tripped budget offered %+v, want only the conflict round of b-3", after)
	}
}

// TestRunBudgetNeverReadsAStoppingRunAsAResume is P9-01: [Run.Stop] leaves
// Paused() false because the phase moved to stopping, and reading that as
// the user saying "carry on" published a false "the run was resumed over
// its budget" notice — into the run log and the summary's notice count —
// three lines above "run finished: stopped".
func TestRunBudgetNeverReadsAStoppingRunAsAResume(t *testing.T) {
	t.Parallel()
	p, h := &limPhasedPauser{}, &limHost{}
	b := NewRunBudget(RunBudgetOptions{
		Limits: config.Limits{MaxTokens: 100},
		Mode:   domain.RunModeAutopilot,
		Run:    p,
		Host:   h,
	})
	gated := b.Work(limWork{{Kind: DemandImplement, Bead: "b-1"}})

	b.Record(cartridge.Usage{InputTokens: 250})
	if !p.Paused() {
		t.Fatal("the run was not paused over its budget")
	}

	p.stop()
	for range 3 {
		if ds, err := gated.Demands(t.Context()); err != nil || len(ds) != 0 {
			t.Fatalf("a stopping run was offered %d demands (err=%v)", len(ds), err)
		}
	}
	if !b.Tripped() {
		t.Error("a stopping run forgave its budget")
	}
	if got := b.Grants(); got != 1 {
		t.Errorf("Grants = %d, want the one grant the run started with", got)
	}
	if got := h.warnings(); len(got) != 1 {
		t.Errorf("warnings = %v, want the banner alone: a stopping run was never resumed", got)
	}
}

// TestRunBudgetGrantsHeadroomToAResumedRunThatKnowsItsPhase is the other
// side of that: a pauser that reports [PhaseRunning] is a live run, and
// clearing its pause is the resume it looks like.
func TestRunBudgetGrantsHeadroomToAResumedRunThatKnowsItsPhase(t *testing.T) {
	t.Parallel()
	p, h := &limPhasedPauser{}, &limHost{}
	p.phase = PhaseRunning
	b := NewRunBudget(RunBudgetOptions{
		Limits: config.Limits{MaxTokens: 100},
		Mode:   domain.RunModeAutopilot,
		Run:    p,
		Host:   h,
	})
	gated := b.Work(limWork{{Kind: DemandImplement, Bead: "b-1"}})

	b.Record(cartridge.Usage{InputTokens: 250})
	p.resume()
	ds, err := gated.Demands(t.Context())
	if err != nil || len(ds) != 1 {
		t.Fatalf("a resumed run was offered %d demands (err=%v), want its work back", len(ds), err)
	}
	if b.Tripped() {
		t.Error("a resumed run is still holding new work back")
	}
	if got := h.warnings(); len(got) != 2 {
		t.Errorf("warnings = %v, want the banner and the resume notice", got)
	}
}

func TestRunBudgetMeterPlannerRecordsWhatEachFiringSpent(t *testing.T) {
	t.Parallel()
	b, _, _ := limBudget(t, domain.RunModeAutopilot, 1000, 0)

	var inner []string
	planner := b.MeterPlanner(FiringPlannerFunc(func(context.Context, Firing) (AgentFiring, error) {
		return AgentFiring{
			Agent: domain.Agent{ID: "eng-b-1-r1"},
			Result: func(any, agents.Transcript) error {
				inner = append(inner, "called")
				return nil
			},
		}, nil
	}))

	af, err := planner.PlanFiring(t.Context(), Firing{})
	if err != nil {
		t.Fatalf("PlanFiring: %v", err)
	}
	if af.Result == nil {
		t.Fatal("the metered firing lost its Result hook")
	}
	if err := af.Result(nil, agents.Transcript{Usage: cartridge.Usage{InputTokens: 300, OutputTokens: 100}}); err != nil {
		t.Fatalf("Result: %v", err)
	}
	if got := b.Spent(); got.Tokens != 400 || got.Firings != 1 {
		t.Errorf("Spent = %+v, want 400 tokens over 1 firing", got)
	}
	if len(inner) != 1 {
		t.Errorf("the wrapped hook ran %d times, want 1", len(inner))
	}

	// A planner that fails is passed straight through.
	boom := errors.New("no firing")
	if _, err := b.MeterPlanner(FiringPlannerFunc(func(context.Context, Firing) (AgentFiring, error) {
		return AgentFiring{}, boom
	})).PlanFiring(t.Context(), Firing{}); !errors.Is(err, boom) {
		t.Errorf("PlanFiring error = %v, want %v", err, boom)
	}
}

// TestRunBudgetMeterPlanRunnerMetersThePlanningProductOwner is P9-17: the
// planning firing never goes through a FiringPlanner, so before this
// wrapper existed the budget under-counted by the whole planning spend.
func TestRunBudgetMeterPlanRunnerMetersThePlanningProductOwner(t *testing.T) {
	t.Parallel()
	b, _, _ := limBudget(t, domain.RunModeAutopilot, 1000, 0)

	tr := agents.Transcript{Usage: cartridge.Usage{InputTokens: 300, OutputTokens: 100}}
	runner := b.MeterPlanRunner(PlanRunnerFunc(func(context.Context, Firing, agents.PlanRequest) (domain.Plan, agents.Transcript, error) {
		return domain.Plan{Brief: "planned"}, tr, nil
	}))
	plan, got, err := runner.RunPlan(t.Context(), Firing{}, agents.PlanRequest{})
	if err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	if plan.Brief != "planned" || got.Usage != tr.Usage {
		t.Errorf("the wrapper changed the result: %+v / %+v", plan, got)
	}
	if spent := b.Spent(); spent.Tokens != 400 || spent.Firings != 1 {
		t.Errorf("Spent = %+v, want 400 tokens over 1 firing", spent)
	}

	// A rejected plan cost tokens too, and the planner will fire again.
	boom := errors.New("the plan was rejected")
	rejected := b.MeterPlanRunner(PlanRunnerFunc(func(context.Context, Firing, agents.PlanRequest) (domain.Plan, agents.Transcript, error) {
		return domain.Plan{}, agents.Transcript{Usage: cartridge.Usage{InputTokens: 50}}, boom
	}))
	if _, _, err := rejected.RunPlan(t.Context(), Firing{}, agents.PlanRequest{}); !errors.Is(err, boom) {
		t.Errorf("RunPlan error = %v, want %v", err, boom)
	}
	if spent := b.Spent(); spent.Tokens != 450 || spent.Firings != 2 {
		t.Errorf("Spent = %+v, want 450 tokens over 2 firings", spent)
	}

	// A session the cartridge reported nothing for is not a firing.
	silent := b.MeterPlanRunner(PlanRunnerFunc(func(context.Context, Firing, agents.PlanRequest) (domain.Plan, agents.Transcript, error) {
		return domain.Plan{}, agents.Transcript{}, nil
	}))
	if _, _, err := silent.RunPlan(t.Context(), Firing{}, agents.PlanRequest{}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	if spent := b.Spent(); spent.Firings != 2 {
		t.Errorf("Spent = %+v, want the silent session uncounted", spent)
	}

	// A disabled budget hands the runner straight back.
	var off *RunBudget
	inner := PlanRunnerFunc(func(context.Context, Firing, agents.PlanRequest) (domain.Plan, agents.Transcript, error) {
		return domain.Plan{}, agents.Transcript{}, nil
	})
	if got := off.MeterPlanRunner(inner); got == nil {
		t.Error("a nil budget dropped the runner")
	}
}

func TestUsageReported(t *testing.T) {
	t.Parallel()
	if UsageReported(cartridge.Usage{}) {
		t.Error("an empty usage reports nothing")
	}
	for _, u := range []cartridge.Usage{
		{InputTokens: 1}, {OutputTokens: 1}, {CacheReadTokens: 1},
		{CacheWriteTokens: 1}, {HasCost: true},
	} {
		if !UsageReported(u) {
			t.Errorf("UsageReported(%+v) = false", u)
		}
	}
}

func TestRunBudgetMeterFiringNeverFailsAFinishedSession(t *testing.T) {
	t.Parallel()
	b, _, _ := limBudget(t, domain.RunModeAutopilot, 10, 0)
	af := AgentFiring{Agent: domain.Agent{ID: "po-plan"}}
	b.MeterFiring(&af)
	if af.Result == nil {
		t.Fatal("MeterFiring installed no hook")
	}
	if err := af.Result(nil, agents.Transcript{Usage: cartridge.Usage{InputTokens: 99}}); err != nil {
		t.Fatalf("the meter failed a finished session: %v", err)
	}
	if got := b.Spent().Tokens; got != 99 {
		t.Errorf("Spent.Tokens = %d, want 99", got)
	}
}

func TestARunWithNoBudgetGatesNothing(t *testing.T) {
	t.Parallel()
	work := limWork{{Kind: DemandImplement, Bead: "b-1"}}
	stop := StopConditionFunc(func(context.Context, *Run, SchedulerStatus) (Outcome, bool, error) {
		return Outcome{}, false, nil
	})
	planner := FiringPlannerFunc(func(context.Context, Firing) (AgentFiring, error) {
		return AgentFiring{}, nil
	})

	for _, tc := range []struct {
		name string
		b    *RunBudget
	}{
		{"a nil budget", nil},
		{"a budget with no caps", NewRunBudget(RunBudgetOptions{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.b.Enabled() {
				t.Error("Enabled is true without a cap")
			}
			ds, err := tc.b.Work(work).Demands(t.Context())
			if err != nil || len(ds) != 1 {
				t.Errorf("Demands = (%d, %v), want the source's own 1", len(ds), err)
			}
			if _, done, err := tc.b.Stop(stop).Done(t.Context(), nil, SchedulerStatus{Idle: true}); done || err != nil {
				t.Errorf("Done = (%v, %v), want the inner condition's", done, err)
			}
			af, err := tc.b.MeterPlanner(planner).PlanFiring(t.Context(), Firing{})
			if err != nil {
				t.Fatalf("PlanFiring: %v", err)
			}
			if af.Result != nil {
				t.Error("a budget with no caps installed a meter")
			}
			tc.b.MeterFiring(&af)
			if af.Result != nil {
				t.Error("MeterFiring installed a meter without a cap")
			}
			tc.b.Record(cartridge.Usage{InputTokens: 1_000_000, CostUSD: 999, HasCost: true})
			if tc.b.Tripped() || tc.b.Exceeded() || tc.b.Banner() != "" || tc.b.Summary() != "" {
				t.Error("a budget with no caps fired")
			}
		})
	}
}

func TestRunBudgetSurvivesAPauseItCannotMake(t *testing.T) {
	t.Parallel()
	p := &limPauser{err: ErrFinished}
	b := NewRunBudget(RunBudgetOptions{
		Limits: config.Limits{MaxTokens: 10},
		Mode:   domain.RunModeAutopilot,
		Run:    p,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	b.Record(cartridge.Usage{InputTokens: 50})
	if !b.Tripped() {
		t.Fatal("the budget did not fire when the run could not be paused")
	}
	// The gate still holds new work back: a run that would not pause must
	// not carry on spending as though nothing happened.
	ds, err := b.Work(limWork{{Kind: DemandImplement, Bead: "b-1"}}).Demands(t.Context())
	if err != nil || len(ds) != 0 {
		t.Errorf("Demands = (%d, %v), want none", len(ds), err)
	}
}

// --- P9-01 round 4: the budget is a cap on the run, not on one `sc run` ---

// TestRunBudgetCarriesTheSpendOfAResumedRun is the review gap that made
// `limits.max_tokens` multipliable by restarting: state.json carried no
// spend, so every invocation of the same run started at zero and a 4000
// token cap produced 18854 tokens over two invocations of one run id.
func TestRunBudgetCarriesTheSpendOfAResumedRun(t *testing.T) {
	t.Parallel()
	p, h := &limPhasedPauser{}, &limHost{}
	p.phase = PhaseRunning
	b := NewRunBudget(RunBudgetOptions{
		Limits: config.Limits{MaxTokens: 500},
		Mode:   domain.RunModeBrief,
		Run:    p,
		Host:   h,
		Restore: BudgetState{
			Spend:  Spend{Tokens: 900, Firings: 6},
			Grants: 1,
		},
	})

	if got := b.Spent(); got.Tokens != 900 || got.Firings != 6 {
		t.Fatalf("Spent = %+v, want the 900 tokens over 6 sessions the earlier invocation spent", got)
	}
	// A resume is the user saying "carry on", so the run gets one more
	// budget's worth — and no more. Counting from zero would have given it
	// a whole fresh 500 on top of a spend it never sees again.
	if got := b.Grants(); got != 2 {
		t.Errorf("Grants = %d, want 2: one more budget's worth over the 900 already spent", got)
	}
	if b.Tripped() {
		t.Error("a resumed run under its widened ceiling is holding work back")
	}

	gated := b.Work(limWork{{Kind: DemandImplement, Bead: "b-1"}})
	if ds, err := gated.Demands(t.Context()); err != nil || len(ds) != 1 {
		t.Fatalf("a resumed run was offered %d demands (err=%v), want its work", len(ds), err)
	}
	warned := h.warnings()
	if len(warned) != 1 || !strings.Contains(warned[0], "900 tokens") || !strings.Contains(warned[0], "1000") {
		t.Fatalf("warnings = %v, want one notice naming the carried spend and the new ceiling", warned)
	}
	if got := h.warnings(); len(got) != 1 {
		t.Errorf("the resume notice was published %d times, want once", len(got))
	}

	// The next budget's worth stops it again, at the run's total rather
	// than at this invocation's.
	b.Record(cartridge.Usage{InputTokens: 100})
	if !b.Tripped() {
		t.Fatal("a resumed run never stopped: the carried spend was not counted")
	}
	if got := b.Summary(); !strings.Contains(got, "1000 tokens of its 1000 budget") ||
		!strings.Contains(got, "over 7 agent sessions") {
		t.Errorf("summary = %q, want the whole run's spend and session count", got)
	}
}

// TestRunBudgetSnapshotIsWhatARestoreCarries pins the round trip the run
// state file makes: what [RunBudget.Snapshot] hands the run is what
// [RunBudgetOptions.Restore] takes back.
func TestRunBudgetSnapshotIsWhatARestoreCarries(t *testing.T) {
	t.Parallel()
	b, _, _ := limBudget(t, domain.RunModeAutopilot, 1000, 0)
	b.Record(cartridge.Usage{InputTokens: 300, OutputTokens: 100, CostUSD: 0.25, HasCost: true})
	snap := b.Snapshot()
	if snap.Spend.Tokens != 400 || snap.Spend.Firings != 1 || !snap.Spend.HasCost || snap.Grants != 1 {
		t.Fatalf("Snapshot = %+v, want the metered spend and one grant", snap)
	}

	next := NewRunBudget(RunBudgetOptions{
		Limits: config.Limits{MaxTokens: 1000}, Mode: domain.RunModeBrief, Restore: snap,
	})
	if got := next.Snapshot(); got != snap {
		t.Errorf("a restored budget snapshots %+v, want the %+v it was given", got, snap)
	}
	next.Record(cartridge.Usage{InputTokens: 600})
	if !next.Tripped() {
		t.Error("the restored spend was not counted towards the cap")
	}
}

// TestRunBudgetSurvivesTheStateFile is the same claim end to end: a run
// writes its budget into state.json on every transition, and the run
// `--resume` builds reads it back.
func TestRunBudgetSurvivesTheStateFile(t *testing.T) {
	t.Parallel()
	cfg := limRunConfig(t, domain.RunModeBrief, config.Limits{MaxTokens: 500})
	first, err := New(cfg, Options{SkipLock: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The run is given a context of its own rather than t.Context(): Go
	// cancels the test's context after the body returns but *before* its
	// cleanups, so the run's finish path — which writes state.json — would
	// still be going while t.TempDir()'s RemoveAll deleted the directory
	// under it ("TempDir RemoveAll cleanup: directory not empty", P9-27).
	// Cleanups run last-in-first-out, and t.TempDir registered its own
	// inside limRunConfig above, so this one stops the run first.
	ctx, cancel := context.WithCancel(context.Background())
	if err := first.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-first.Done():
		case <-time.After(testutil.Patience(30 * time.Second)):
			t.Error("the run did not finish after its context was cancelled")
		}
		_ = first.Close()
	})

	b := NewRunBudget(RunBudgetOptions{
		Limits: config.Limits{MaxTokens: 500}, Mode: domain.RunModeBrief, Run: first, Host: first,
		Restore: first.State().Spent(),
	})
	first.UseBudget(b.Snapshot)
	b.Record(cartridge.Usage{InputTokens: 700})
	// Any transition persists; an agent starting is the one that happens
	// after every session in a real run.
	if err := first.AgentStarted(domain.Agent{ID: "eng-b-1-r1", Role: domain.RoleEngineer}); err != nil {
		t.Fatalf("AgentStarted: %v", err)
	}
	// The live-agent write is coalesced (P9-11), and this test reads the
	// file behind the run's back, so it asks for the flush the run would
	// have made a moment later anyway.
	if err := first.FlushState(); err != nil {
		t.Fatalf("FlushState: %v", err)
	}

	st, err := LoadState(cfg.Run.StateDir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got := st.Spent(); got.Spend.Tokens != 700 || got.Spend.Firings != 1 {
		t.Fatalf("state.json carries %+v, want the run's 700 tokens over 1 session", got)
	}

	// The resumed run picks it up.
	resumed := *cfg
	resumed.Resume = true
	next, err := New(&resumed, Options{SkipLock: true})
	if err != nil {
		t.Fatalf("New (resume): %v", err)
	}
	if got := next.State().Spent().Spend; got.Tokens != 700 || got.Firings != 1 {
		t.Fatalf("the resumed run carries %+v, want the 700 tokens of the first invocation", got)
	}
	if got := NewRunBudget(RunBudgetOptions{
		Limits: config.Limits{MaxTokens: 500}, Mode: domain.RunModeBrief,
		Restore: next.State().Spent(),
	}).Spent().Tokens; got != 700 {
		t.Errorf("the resumed run's budget starts at %d tokens, want the 700 already spent", got)
	}
}

// TestRunBudgetStopsAnExternalPullRequestReviewItCouldNotHaveStarted is the
// other half of "a crossed cap stops new work": [PRReview] adopts pull
// requests on its own goroutine (D-11), so a review demand of its is a
// round the watcher opened rather than one the run had under way. Keeping
// them spent three fresh tester sessions per pull request over a blown
// budget and, because the scheduler was never idle, stopped the brief run
// from ever finishing.
func TestRunBudgetStopsAnExternalPullRequestReviewItCouldNotHaveStarted(t *testing.T) {
	t.Parallel()
	b, _, _ := limBudget(t, domain.RunModeBrief, 100, 0)
	own := Demand{Kind: DemandReview, Bead: "b-2", Lens: domain.LensQA, Round: 1,
		Payload: FeedbackWork{Bead: "b-2", Round: 1, Lens: domain.LensQA}}
	external := Demand{Kind: DemandReview, Bead: "pr-7", Lens: domain.LensQA, Round: PRReviewRound,
		Task: events.TaskReview, Claim: ClaimNone,
		Payload: PRReviewWork{Bead: "pr-7", Lens: domain.LensQA, Round: PRReviewRound}}
	gated := b.Work(limWork{own, external})

	if ds, err := gated.Demands(t.Context()); err != nil || len(ds) != 2 {
		t.Fatalf("an untripped budget offered %d of 2 demands (err=%v)", len(ds), err)
	}
	b.Record(cartridge.Usage{InputTokens: 200})
	after, err := gated.Demands(t.Context())
	if err != nil {
		t.Fatalf("Demands: %v", err)
	}
	if len(after) != 1 || after[0].Bead != "b-2" {
		t.Fatalf("a tripped budget offered %+v, want only the run's own round", after)
	}
}

// TestAgentTimeoutLimitFallsBackToTheDefaultOnAHandBuiltConfig pins
// [Supervisor.Attach] to [EffectiveLimits]: a run whose config leaves
// agent_timeout unset must get the documented 30-minute default, not no
// timeout at all.
func TestAgentTimeoutLimitFallsBackToTheDefaultOnAHandBuiltConfig(t *testing.T) {
	t.Parallel()
	cfg := limRunConfig(t, domain.RunModeBrief, config.Limits{})
	run, err := New(cfg, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sup, err := NewSupervisor(SupervisorOptions{})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	sup.Attach(run)
	if got, want := sup.Timeout(), config.Defaults().Limits.AgentTimeout.Duration(); got != want {
		t.Errorf("supervisor timeout = %s, want the limits.agent_timeout default %s", got, want)
	}
}

// --- P9-01/P9-21: a verdict being folded in is not a round to re-fire ---

// limReentrantMessages is a [FeedbackMessageStore] that dispatches while it
// writes: [Feedback.testerReplied] empties the round's reviewer list under
// the loop's lock and then, *outside* it, posts the comment to bd before
// planning what happens next. PostMessage is therefore exactly the window a
// scheduler tick can land in, and calling Demands from here reproduces it
// with no goroutines and no timing.
type limReentrantMessages struct {
	loop *Feedback
	seen [][]Demand
}

func (m *limReentrantMessages) PostMessage(ctx context.Context, id domain.BeadID, _ string, _ any) (beads.Message, error) {
	ds, err := m.loop.Demands(ctx)
	if err != nil {
		return beads.Message{}, err
	}
	m.seen = append(m.seen, ds)
	return beads.Message{IssueID: id}, nil
}

// TestABudgetHeldRoundIsNotRefiredWhileItsVerdictIsWrittenToTheBead is the
// blocker P9-01's own wiring test tripped over: a dispatch landing between
// the last verdict of a round and the bead moving to changes_requested saw
// "reviewing, nobody owes a verdict", re-derived the reviewers from the
// still-open item and fired the lens again, for ever — a livelock, because
// the engineer firing that closes the round is exactly the new work a
// crossed budget holds back.
//
// It lives beside the budget tests because the budget is what turns the
// re-fire into an unbounded spend: with the [RunBudget.Work] gate on, the
// re-fired review is the only demand left in the run.
func TestABudgetHeldRoundIsNotRefiredWhileItsVerdictIsWrittenToTheBead(t *testing.T) {
	t.Parallel()
	msgs := &limReentrantMessages{}
	loop, err := NewFeedback(FeedbackOptions{
		Context: func(context.Context, domain.BeadID) (FeedbackBeadContext, error) {
			return FeedbackBeadContext{}, nil
		},
		Messages: msgs,
		Lenses:   []domain.Lens{domain.LensQA},
		Limits:   config.Limits{MaxReviewRounds: 5, MaxPushbackRounds: 2},
		Now:      func() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewFeedback: %v", err)
	}
	msgs.loop = loop

	const bead domain.BeadID = "sc-b1"
	loop.Track(domain.Bead{ID: bead, Title: "make the retry loop terminate", State: domain.BeadStateInReview, Round: 1})

	// The tick that fires the round's one tester.
	ds, err := loop.Demands(t.Context())
	if err != nil {
		t.Fatalf("Demands: %v", err)
	}
	if len(ds) != 1 || ds[0].Kind != DemandReview || ds[0].Lens != domain.LensQA {
		t.Fatalf("the adopted bead wanted %+v, want one qa review", ds)
	}

	// The verdict lands, asking for a change: the item stays open, which is
	// what made reviewersFor offer the lens its own review again.
	work := FeedbackWork{Bead: bead, Round: 1, Phase: FeedbackReviewing, Tester: agents.TesterModeReview, Lens: domain.LensQA}
	verdict := domain.Verdict{
		Lens: domain.LensQA, Round: 1, Outcome: domain.VerdictChanges, Summary: "the note has no title",
		Items: []domain.FeedbackItem{{
			ID: "qa-1", Severity: domain.SeverityMajor, Status: domain.ItemOpen,
			Title: "the note needs a title", Detail: "add a heading", File: "note.md",
		}},
	}
	if err := loop.testerReplied(t.Context(), work, "tester-qa-sc-b1-r1", verdict); err != nil {
		t.Fatalf("testerReplied: %v", err)
	}

	if len(msgs.seen) != 1 {
		t.Fatalf("the verdict was written to the bead %d times, want once", len(msgs.seen))
	}
	for _, d := range msgs.seen[0] {
		if d.Kind == DemandReview {
			t.Fatalf("the round was re-offered to %s while its verdict was being written: %+v", d.Lens, d)
		}
	}

	// And the round did close: the bead owes the engineer an answer.
	after, err := loop.Demands(t.Context())
	if err != nil {
		t.Fatalf("Demands: %v", err)
	}
	if len(after) != 1 || after[0].Kind != DemandImplement || after[0].FiringTask() != events.TaskAddressFeedback {
		t.Fatalf("after the verdict the loop wants %+v, want the engineer to answer the feedback", after)
	}
	th, ok := loop.Thread(bead)
	if !ok {
		t.Fatal("the thread vanished")
	}
	if th.Phase != FeedbackAddressing || th.State != domain.BeadStateChangesRequested {
		t.Errorf("thread is %s/%s, want addressing/changes_requested", th.Phase, th.State)
	}
}

// TestAnAdoptedBeadStillWorksOutItsOwnReviewers is the behaviour the fix
// above must not cost: a run that resumes a bead bd already holds in
// in_review has never been through [Feedback.plan], so the reviewer list is
// worked out on the first dispatch.
func TestAnAdoptedBeadStillWorksOutItsOwnReviewers(t *testing.T) {
	t.Parallel()
	loop, err := NewFeedback(FeedbackOptions{
		Context: func(context.Context, domain.BeadID) (FeedbackBeadContext, error) {
			return FeedbackBeadContext{}, nil
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewFeedback: %v", err)
	}
	loop.Track(domain.Bead{ID: "sc-b2", Title: "adopted", State: domain.BeadStateInReview, Round: 1})
	ds, err := loop.Demands(t.Context())
	if err != nil {
		t.Fatalf("Demands: %v", err)
	}
	if len(ds) != len(domain.Lenses()) {
		t.Fatalf("an adopted bead wanted %d reviews, want one per lens (%d)", len(ds), len(domain.Lenses()))
	}
	// And again on the next tick: the demand is stateless until it lands.
	again, err := loop.Demands(t.Context())
	if err != nil || len(again) != len(ds) {
		t.Fatalf("the second tick wanted %d demands (err=%v), want the same %d", len(again), err, len(ds))
	}
}

// TestRunBudgetChargesASessionKilledByTheAgentTimeout is P9-13: the budget
// used to be fed only by [AgentFiring.Result], which the supervisor runs
// for a session that finished *and* decoded. An agent killed by
// `limits.agent_timeout` after twenty minutes of work, or one that crashed
// on its last attempt, spent real tokens the budget never saw — so a run
// whose agents mostly time out could overrun `max_cost_usd` for ever.
//
// It fires the real supervisor over the mock cartridge, whose timed-out
// session reports the usage the rule pins, and asks the budget what it was
// charged.
func TestRunBudgetChargesASessionKilledByTheAgentTimeout(t *testing.T) {
	sc := mock.NewScenario("slow")
	sc.Otherwise("hangs").
		FailWith(mock.Failure{Kind: mock.FailTimeout}).
		Usage(cartridge.Usage{InputTokens: 600, OutputTokens: 100})
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: sc})
	sup := f.supervisor(t, func(o *SupervisorOptions) {
		o.Timeout = 20 * time.Millisecond
		o.CrashRetries = -1 // one attempt, so the arithmetic is unambiguous
	})

	b, p, _ := limBudget(t, domain.RunModeAutopilot, 500, 0)
	af := supTesterFiring("tester-qa-bd-1-r1", "bd-1", f.worktree(t, "bd-1"))
	b.MeterFiring(&af)

	_, tr, err := FireAgent[domain.Verdict](context.Background(), sup, af)
	if err == nil {
		t.Fatal("the mock session was supposed to time out")
	}
	if !errors.Is(err, cartridge.ErrTimeout) {
		t.Fatalf("firing error = %v, want a timeout", err)
	}
	if got := tr.Usage.TotalTokens(); got != 700 {
		t.Fatalf("the transcript of the timed-out session reports %d tokens, want 700", got)
	}

	spent := b.Spent()
	if spent.Tokens != 700 || spent.Firings != 1 {
		t.Errorf("Spent = %+v, want the timed-out session's 700 tokens over 1 firing", spent)
	}
	if !b.Tripped() {
		t.Fatal("the budget did not fire although the run is over its 500-token cap")
	}
	if got := p.pauses(); len(got) != 1 || got[0] != PauseReasonTokenCap {
		t.Errorf("pauses = %v, want one %q", got, PauseReasonTokenCap)
	}
}

// TestRunBudgetChargesEveryRetriedAttemptOfACrashedFiring is the other half
// of P9-13, through the production wiring the scheduler uses: the
// supervisor plans the firing with [RunBudget.MeterPlanner], crashes twice
// and approves on the third attempt. Every attempt spent a session's worth
// of tokens; the two failures are charged by [AgentFiring.Spent] and the
// success by [AgentFiring.Result], and neither counts the other's.
func TestRunBudgetChargesEveryRetriedAttemptOfACrashedFiring(t *testing.T) {
	sc := mock.NewScenario("flaky")
	sc.Rule("crash", mock.Match{}).Times(2).
		FailWith(mock.Failure{Kind: mock.FailCrash, ExitCode: 3}).
		Usage(cartridge.Usage{InputTokens: 100})
	sc.Otherwise("approve").
		Say("fine").
		Structured(json.RawMessage(`{"outcome":"approve"}`)).
		Usage(cartridge.Usage{InputTokens: 50})
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: sc})

	b, _, _ := limBudget(t, domain.RunModeAutopilot, 1_000_000, 0)
	wt := f.worktree(t, "bd-2")
	sup := f.supervisor(t, func(o *SupervisorOptions) {
		o.RetryBackoff = time.Millisecond
		o.Planner = b.MeterPlanner(FiringPlannerFunc(func(_ context.Context, sf Firing) (AgentFiring, error) {
			af := supTesterFiring(sf.Agent.ID, sf.Demand.Bead, wt)
			af.Agent = sf.Agent
			return af, nil
		}))
	})

	if _, err := sup.Fire(context.Background(), Firing{
		Agent:  domain.Agent{ID: "tester-qa-bd-2-r1", Role: domain.RoleTester, Lens: domain.LensQA, Bead: "bd-2"},
		Demand: Demand{Kind: DemandReview, Bead: "bd-2", Lens: domain.LensQA, Round: 1, BeadTitle: "review me"},
		Run:    f.Run,
		Update: func(domain.Agent) {},
	}); err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if spent := b.Spent(); spent.Tokens != 250 || spent.Firings != 3 {
		t.Errorf("Spent = %+v, want the two crashes (100 each) and the approval (50) over 3 sessions", spent)
	}
}

// TestRunBudgetMeterPlannerWiresTheFailurePathToo pins the wiring P9-13
// added: a firing the scheduler's planner produced carries the Spent hook,
// and a failed attempt the cartridge reported nothing for is not a session.
func TestRunBudgetMeterPlannerWiresTheFailurePathToo(t *testing.T) {
	t.Parallel()
	b, _, _ := limBudget(t, domain.RunModeAutopilot, 1000, 0)

	var inner int
	planner := b.MeterPlanner(FiringPlannerFunc(func(context.Context, Firing) (AgentFiring, error) {
		return AgentFiring{
			Agent: domain.Agent{ID: "eng-b-1-r1"},
			Spent: func(agents.Transcript) { inner++ },
		}, nil
	}))
	af, err := planner.PlanFiring(t.Context(), Firing{})
	if err != nil {
		t.Fatalf("PlanFiring: %v", err)
	}
	if af.Spent == nil {
		t.Fatal("the metered firing has no Spent hook, so a timed-out session is never charged")
	}

	af.Spent(agents.Transcript{Usage: cartridge.Usage{InputTokens: 300}})
	af.Spent(agents.Transcript{}) // a session the cartridge reported nothing for
	if got := b.Spent(); got.Tokens != 300 || got.Firings != 1 {
		t.Errorf("Spent = %+v, want 300 tokens over 1 firing", got)
	}
	if inner != 2 {
		t.Errorf("the wrapped hook ran %d times, want 2", inner)
	}
}

// TestRunBudgetCanBeRaisedWhileTheRunIsHeld is P9-14: a run paused on a cap
// used to be actionable in two ways only — resume it into another grant of
// the same size, or stop it — because `limits.max_tokens` is read once from
// the configuration the run was frozen with. Raising it in place is the
// third, and it is what the TUI's `b` key calls.
func TestRunBudgetCanBeRaisedWhileTheRunIsHeld(t *testing.T) {
	t.Parallel()
	b, p, h := limBudget(t, domain.RunModeAutopilot, 500, 0)
	work := b.Work(limWork{{Kind: DemandImplement, Bead: "sc-1"}})

	b.Record(cartridge.Usage{InputTokens: 700})
	if !b.Tripped() || !p.Paused() {
		t.Fatal("the budget did not stop the run at its cap")
	}
	if ds, err := work.Demands(t.Context()); err != nil || len(ds) != 0 {
		t.Fatalf("Demands = %v (err=%v), want no new work while the budget is held", ds, err)
	}

	ceiling, err := b.Raise(2000)
	if err != nil {
		t.Fatalf("Raise: %v", err)
	}
	if ceiling != "2000 tokens" {
		t.Errorf("the new ceiling is %q, want 2000 tokens", ceiling)
	}
	if b.Tripped() || b.Exceeded() {
		t.Error("the budget is still held after being raised above the spend")
	}
	if got := p.resumed(); len(got) != 1 || got[0] != BudgetRaisedReason {
		t.Errorf("resumes = %v, want one %q: raising a budget is the whole gesture", got, BudgetRaisedReason)
	}
	if p.Paused() {
		t.Error("the run is still paused after its budget was raised")
	}
	if ds, err := work.Demands(t.Context()); err != nil || len(ds) != 1 {
		t.Fatalf("Demands = %v (err=%v), want the work source back", ds, err)
	}

	// The new ceiling is the whole answer, not another grant on top of the
	// old one: the run carries on to 2000 and stops there.
	b.Record(cartridge.Usage{InputTokens: 1000})
	if b.Tripped() {
		t.Fatalf("the budget fired again at %+v, under its new 2000 ceiling", b.Spent())
	}
	b.Record(cartridge.Usage{InputTokens: 400})
	if !b.Tripped() {
		t.Fatal("the raised budget never fired again")
	}
	if got := b.Banner(); !strings.Contains(got, "2100 tokens of its 2000 budget") {
		t.Errorf("banner = %q, want the new ceiling in it", got)
	}
	if warned := h.warnings(); len(warned) != 3 {
		t.Errorf("published warnings = %v, want the first banner, the raise and the second banner", warned)
	}
}

// TestRunBudgetRaisesTheCapThatStoppedTheRun: the number the user types is
// in the unit of the cap that fired, which is the one they set.
func TestRunBudgetRaisesTheCapThatStoppedTheRun(t *testing.T) {
	t.Parallel()
	b, _, _ := limBudget(t, domain.RunModeAutopilot, 100_000, 10)

	b.Record(cartridge.Usage{InputTokens: 20, CostUSD: 12, HasCost: true})
	if got := b.Reason(); got != PauseReasonCostCap {
		t.Fatalf("Reason = %q, want the cost cap", got)
	}
	ceiling, err := b.Raise(25)
	if err != nil {
		t.Fatalf("Raise: %v", err)
	}
	if ceiling != "$25.00" {
		t.Errorf("the new ceiling is %q, want $25.00", ceiling)
	}
	if b.Tripped() {
		t.Error("the cost cap is still held after being raised")
	}
	b.Record(cartridge.Usage{CostUSD: 14, HasCost: true})
	if !b.Tripped() || b.Reason() != PauseReasonCostCap {
		t.Errorf("the raised cost cap did not fire again at $26: tripped=%v reason=%q", b.Tripped(), b.Reason())
	}
}

// TestRunBudgetRefusesARaiseItCannotHonour covers the two refusals a user
// can read in the TUI's footer.
func TestRunBudgetRefusesARaiseItCannotHonour(t *testing.T) {
	t.Parallel()
	b, _, _ := limBudget(t, domain.RunModeAutopilot, 500, 0)
	b.Record(cartridge.Usage{InputTokens: 700})

	for _, ceiling := range []float64{0, -1, 500, 700} {
		if _, err := b.Raise(ceiling); !errors.Is(err, ErrBudgetTooSmall) {
			t.Errorf("Raise(%v) = %v, want ErrBudgetTooSmall", ceiling, err)
		}
	}
	if !b.Tripped() {
		t.Error("a refused raise let the run go")
	}

	// A run with no cap has nothing to raise, and neither has no run.
	none, _, _ := limBudget(t, domain.RunModeAutopilot, 0, 0)
	if _, err := none.Raise(1000); !errors.Is(err, ErrNoBudget) {
		t.Errorf("Raise on an uncapped run = %v, want ErrNoBudget", err)
	}
	var nilBudget *RunBudget
	if _, err := nilBudget.Raise(1000); !errors.Is(err, ErrNoBudget) {
		t.Errorf("Raise on a nil budget = %v, want ErrNoBudget", err)
	}
}

// TestRunBudgetRaiseSaysWhenTheOtherCapIsStillCrossed: a run over both caps
// is not let go by raising one of them, and says so rather than reporting
// success.
func TestRunBudgetRaiseSaysWhenTheOtherCapIsStillCrossed(t *testing.T) {
	t.Parallel()
	b, _, _ := limBudget(t, domain.RunModeAutopilot, 500, 10)
	b.Record(cartridge.Usage{InputTokens: 700, CostUSD: 12, HasCost: true})

	// The cost cap is the one reported when both are crossed at once.
	if got := b.Reason(); got != PauseReasonCostCap {
		t.Fatalf("Reason = %q, want the cost cap", got)
	}
	_, err := b.Raise(25)
	if err == nil {
		t.Fatal("raising one cap over a run that is over both reported success")
	}
	if !strings.Contains(err.Error(), "other cap") {
		t.Errorf("error = %v, want it to name the cap still crossed", err)
	}
	if !b.Tripped() {
		t.Error("the run was let go although its token cap is still crossed")
	}
	// And raising the second one lets it go.
	if _, err := b.Raise(2000); err != nil {
		t.Fatalf("Raise (tokens): %v", err)
	}
	if b.Tripped() {
		t.Error("the run is still held after both caps were raised")
	}
}
