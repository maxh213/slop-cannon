package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// P9-01: every `[limits]` key is resolved in one place and the two budget
// keys behave as D-18 says — a crossed cap stops new work, finishes the
// reviews in flight, then pauses an autopilot run and finishes a brief one.

// limPauser stands in for the run's pause gate.
type limPauser struct {
	mu      sync.Mutex
	reasons []string
	paused  bool
	err     error
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
	if out.Reason != events.FinishStopped {
		t.Errorf("finish reason = %q, want %q", out.Reason, events.FinishStopped)
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
