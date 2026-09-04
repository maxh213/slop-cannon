package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// Limits and budgets (P9-01). Two kinds of limit live in `[limits]`:
//
//   - The three that bound one *thing*: `max_review_rounds` and
//     `max_pushback_rounds` bound a bead's feedback loop (D-08, enforced by
//     [NewFeedback]) and `agent_timeout` bounds one agent session (enforced
//     by the [Supervisor]). Each is resolved here, once, by
//     [EffectiveLimits], so the assembler cannot hand one component the
//     configured value and another a default.
//
//   - The two that bound the *run*: `max_tokens` and `max_cost_usd` are
//     opt-in (unset means unlimited) and are enforced only as far as the
//     cartridges report usage. They are the [RunBudget] below.
//
// There is deliberately no bead-count and no runtime cap: D-18 is the law,
// and an autopilot run ends only when the user stops it or every cartridge
// is out of credits. Crossing a budget therefore never *ends* an autopilot
// run — it stops new work and pauses with a banner, and the user decides.
// A brief run, which finishes on its own anyway, finishes early instead,
// with the reason in its summary.

// SourceLimits is the [events.Warning.Source] the budget stamps on its
// banner and on the notice it publishes when a resumed run carries on over
// its budget.
const SourceLimits = "limits"

// The [Run.Pause] reasons a crossed budget pauses an autopilot run with.
// They name the config key so the TUI banner and the run log say which cap
// was hit.
const (
	// PauseReasonTokenCap means `limits.max_tokens` was reached.
	PauseReasonTokenCap = "token_cap"
	// PauseReasonCostCap means `limits.max_cost_usd` was reached.
	PauseReasonCostCap = "cost_cap"
)

// EffectiveLimits resolves the limits a run behaves by: the values in the
// run's effective configuration, with [config.Defaults] filling anything a
// hand-built or partial configuration left unset or out of range. A nil
// config, or one with no effective configuration behind it, is all
// defaults.
//
// Every consumer of `[limits]` should take its value from here rather than
// reading cfg.Effective.Limits itself, so one run cannot fire agents with a
// 30-minute timeout while its feedback loop believes in a different
// `max_review_rounds`.
//
// The two budget keys are the exception to "out of range becomes the
// default": they are opt-in, zero means unlimited, and a negative value —
// which [config.Load] rejects, so only a hand-built config has one — is
// read as unlimited rather than as a budget nobody can spend.
func EffectiveLimits(cfg *config.RunConfig) config.Limits {
	def := config.Defaults().Limits
	if cfg == nil || cfg.Effective == nil {
		return def
	}
	lim := cfg.Effective.Limits
	if lim.MaxReviewRounds < 1 {
		lim.MaxReviewRounds = def.MaxReviewRounds
	}
	if lim.MaxPushbackRounds < 1 {
		// Zero is not "no pushback": NewFeedback reads it as "unset" too,
		// and config.Load never leaves the key at zero.
		lim.MaxPushbackRounds = def.MaxPushbackRounds
	}
	if lim.AgentTimeout.Duration() <= 0 {
		lim.AgentTimeout = def.AgentTimeout
	}
	if lim.MaxPlanRejections < 1 {
		lim.MaxPlanRejections = def.MaxPlanRejections
	}
	if lim.MaxPlanFailures < 1 {
		lim.MaxPlanFailures = def.MaxPlanFailures
	}
	if lim.MaxTokens < 0 {
		lim.MaxTokens = 0
	}
	if lim.MaxCostUSD < 0 {
		lim.MaxCostUSD = 0
	}
	return lim
}

// Spend is what a run has consumed, as far as its cartridges reported it.
// A cartridge that reports nothing leaves it at zero, and a budget over
// nothing is never crossed: that is what "enforced only when cartridges
// report usage" means.
type Spend struct {
	// Tokens is input plus output tokens over every metered firing (cache
	// tokens excluded, matching [cartridge.Usage.TotalTokens]).
	Tokens int64 `json:"tokens"`
	// CostUSD is the cost the cartridges reported. It is meaningful only
	// when HasCost is true: an adapter whose CLI prints no cost figure
	// leaves it at zero, which is not the same as a free run.
	CostUSD float64 `json:"cost_usd"`
	// HasCost is true once any metered firing reported a cost.
	HasCost bool `json:"has_cost"`
	// Firings is how many agent sessions were metered.
	Firings int `json:"firings"`
}

// BudgetPauser is the slice of [*Run] a [RunBudget] uses: it pauses an
// autopilot run that crossed its budget (D-18) and notices, on the next
// dispatch, that the user resumed it.
type BudgetPauser interface {
	// Pause stops the scheduler firing new agents; the running ones finish.
	Pause(reason string) error
	// Paused reports whether the run is paused.
	Paused() bool
}

// *Run is the production pauser.
var _ BudgetPauser = (*Run)(nil)

// RunBudgetOptions configures [NewRunBudget]. The zero value is a budget
// with no caps, which is what an unconfigured run has: every wrapper it
// returns is then the thing it was given.
type RunBudgetOptions struct {
	// Limits supplies MaxTokens and MaxCostUSD; the other fields are
	// ignored. Pass [EffectiveLimits] of the run's config.
	Limits config.Limits
	// Mode decides what crossing a budget does: an autopilot run pauses and
	// waits for the user, a brief run finishes once the reviews in flight
	// are done (D-18). An empty mode is treated as autopilot, the mode that
	// never ends a run on its own.
	Mode domain.RunMode
	// Run is paused when an autopilot run crosses a budget. Nil means the
	// budget can only stop new work, not pause.
	Run BudgetPauser
	// Host publishes the banner and supplies the run's clock and logger.
	// Nil means the budget is silent.
	Host EventHost
	// Logger overrides the host's logger.
	Logger *slog.Logger
}

// RunBudget enforces the opt-in `limits.max_tokens` and
// `limits.max_cost_usd` budgets of one run (P9-01, D-18).
//
// It is fed by [RunBudget.Record], one call per finished agent session, and
// acts through three wrappers the assembler puts around parts it already
// builds:
//
//		budget := NewRunBudget(RunBudgetOptions{Limits: EffectiveLimits(cfg), Mode: cfg.Run.Mode, Run: run, Host: run})
//		sup := NewSupervisor(SupervisorOptions{Planner: budget.MeterPlanner(feedback), …})
//		sch := NewScheduler(SchedulerOptions{Work: budget.Work(sources), Stop: budget.Stop(planner), …})
//
//	  - [RunBudget.MeterPlanner] meters every firing the scheduler makes.
//	  - [RunBudget.Work] stops new work once a budget is crossed: no bead is
//	    implemented and no plan is made, while the reviews already in flight
//	    are still offered their agents so the round they are in can finish.
//	  - [RunBudget.Stop] finishes a brief run once that has drained, with the
//	    budget in the summary. It never finishes an autopilot run.
//
// Crossing a budget in autopilot pauses the run with a banner instead
// (D-18). Resuming it is the user saying "carry on": the budget then grants
// itself as much headroom again as the cap allows, so the run pauses once
// more after another budget's worth rather than at once.
//
// A nil *RunBudget is usable and does nothing, so a caller with no budget
// can wire the wrappers unconditionally. All methods are safe for
// concurrent use: Record runs on the goroutines of the agents being
// metered, the wrappers on the scheduler's loop goroutine.
type RunBudget struct {
	maxTokens int64
	maxCost   float64
	mode      domain.RunMode
	pauser    BudgetPauser
	host      EventHost
	log       *slog.Logger

	mu      sync.Mutex
	spend   Spend
	grants  int
	tripped bool
	// paused is true while the run is paused *because of this budget*, and
	// is what tells a resume ("the user cleared our pause") apart from a
	// pause that never happened ("the run refused it").
	paused bool
	reason string
	banner string
}

// NewRunBudget builds the budget. It never fails: a configuration with no
// caps yields a budget that meters the spend and gates nothing.
func NewRunBudget(opts RunBudgetOptions) *RunBudget {
	b := &RunBudget{
		maxTokens: max(opts.Limits.MaxTokens, 0),
		maxCost:   max(opts.Limits.MaxCostUSD, 0),
		mode:      opts.Mode,
		pauser:    opts.Run,
		host:      opts.Host,
		log:       opts.Logger,
		grants:    1,
	}
	if b.mode == "" {
		b.mode = domain.RunModeAutopilot
	}
	if b.log == nil && opts.Host != nil {
		b.log = opts.Host.Log()
	}
	if b.log == nil {
		b.log = slog.Default()
	}
	b.log = b.log.With(slog.String("source", SourceLimits))
	return b
}

// Enabled reports whether either budget is set. A disabled budget still
// meters the spend (so `sc status` can report it) but gates nothing.
func (b *RunBudget) Enabled() bool {
	if b == nil {
		return false
	}
	return b.maxTokens > 0 || b.maxCost > 0
}

// Mode is the run mode the budget acts by.
func (b *RunBudget) Mode() domain.RunMode {
	if b == nil {
		return ""
	}
	return b.mode
}

// Spent returns the metered spend so far.
func (b *RunBudget) Spent() Spend {
	if b == nil {
		return Spend{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spend
}

// Grants is how many budgets' worth of headroom the run has been given: one
// to start with, and one more each time the user resumed a run that had
// paused over its budget.
func (b *RunBudget) Grants() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.grants
}

// Exceeded reports whether the metered spend has reached a budget.
func (b *RunBudget) Exceeded() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exceededLocked()
}

// Tripped reports whether the budget has fired and not yet been forgiven by
// a resume: new work is being held back.
func (b *RunBudget) Tripped() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tripped
}

// Reason is the pause reason of the budget that fired ([PauseReasonCostCap]
// or [PauseReasonTokenCap]), empty while none has.
func (b *RunBudget) Reason() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reason
}

// Banner is the one-line account of the crossed budget: what was spent, out
// of what, which key set it, and what happens next. It is empty until a
// budget fires. It is published as an [events.Warning] the moment the
// budget fires, so the TUI and the headless log both show it.
func (b *RunBudget) Banner() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.banner
}

// Record meters one finished agent session and returns the spend including
// it. Crossing a budget here publishes the banner and, in autopilot, pauses
// the run; the caller is not told, because there is nothing for it to do —
// the gating is the wrappers' job.
//
// Usage a cartridge did not report is zero, and zero spends nothing: a
// budget is only ever enforced as far as the CLI counted (P9-01). In
// particular the cost budget waits for [cartridge.Usage.HasCost], so an
// adapter that reports tokens but no price never trips it.
func (b *RunBudget) Record(u cartridge.Usage) Spend {
	if b == nil {
		return Spend{}
	}
	b.mu.Lock()
	b.spend.Firings++
	if n := u.TotalTokens(); n > 0 {
		b.spend.Tokens += int64(n)
	}
	if u.HasCost {
		b.spend.HasCost = true
		b.spend.CostUSD += u.CostUSD
	}
	spend := b.spend
	fire := !b.tripped && b.exceededLocked()
	if fire {
		b.tripped = true
		b.reason, b.banner = b.describeLocked()
	}
	reason, banner := b.reason, b.banner
	b.mu.Unlock()

	if fire {
		b.fire(reason, banner)
	}
	return spend
}

// fire announces a crossed budget and, in autopilot, pauses the run (D-18).
func (b *RunBudget) fire(reason, banner string) {
	b.log.Warn(banner, slog.String("cap", reason))
	if b.host != nil {
		b.host.Publish(events.Warning{Meta: b.host.Meta(), Source: SourceLimits, Message: banner})
	}
	if b.mode == domain.RunModeBrief || b.pauser == nil {
		return
	}
	if err := b.pauser.Pause(reason); err != nil {
		// The run is finishing, or never started. New work stays held back
		// either way: the gate does not depend on the pause.
		b.log.Warn("the run could not be paused over its budget", slog.Any("err", err))
		return
	}
	b.mu.Lock()
	b.paused = true
	b.mu.Unlock()
}

// exceededLocked reports whether the spend has reached either ceiling. A
// budget over a figure nothing reported is never crossed.
func (b *RunBudget) exceededLocked() bool {
	if b.maxTokens > 0 && b.spend.Tokens > 0 && b.spend.Tokens >= b.ceilTokensLocked() {
		return true
	}
	return b.maxCost > 0 && b.spend.HasCost && b.spend.CostUSD >= b.ceilCostLocked()
}

// ceilTokensLocked is the token ceiling in force: the cap times the grants
// made, saturating rather than wrapping.
func (b *RunBudget) ceilTokensLocked() int64 {
	g := int64(max(b.grants, 1))
	if b.maxTokens > math.MaxInt64/g {
		return math.MaxInt64
	}
	return b.maxTokens * g
}

// ceilCostLocked is the cost ceiling in force.
func (b *RunBudget) ceilCostLocked() float64 {
	return b.maxCost * float64(max(b.grants, 1))
}

// describeLocked names the budget that was crossed and writes the banner.
// When both were crossed at once the cost is the one reported: it is the
// number the user set the cap in.
func (b *RunBudget) describeLocked() (reason, banner string) {
	var what string
	switch {
	case b.maxCost > 0 && b.spend.HasCost && b.spend.CostUSD >= b.ceilCostLocked():
		reason = PauseReasonCostCap
		what = fmt.Sprintf("the run has spent $%.2f of its $%.2f budget (limits.max_cost_usd)",
			b.spend.CostUSD, b.ceilCostLocked())
	default:
		reason = PauseReasonTokenCap
		what = fmt.Sprintf("the run has spent %d tokens of its %d budget (limits.max_tokens)",
			b.spend.Tokens, b.ceilTokensLocked())
	}
	next := "no new work will be started; the run is paused and waiting for you — " +
		"resume it to carry on with another budget's worth, or stop it"
	switch {
	case b.mode == domain.RunModeBrief:
		next = "no new work will be started; the run will finish once the reviews in flight are done"
	case b.pauser == nil:
		next = "no new work will be started"
	}
	return reason, what + ": " + next
}

// Work wraps a work source so that a crossed budget stops new work: while
// the budget is tripped the run is offered only the reviews and
// arbitrations that finish a round already in flight, never a bead to
// implement and never a plan to make.
//
// It is also where a resume is noticed: an autopilot run the user resumed
// is no longer paused, so the budget grants itself the headroom to carry on
// (see [RunBudget]) and the work flows again. A nil or disabled budget
// returns inner unchanged.
func (b *RunBudget) Work(inner WorkSource) WorkSource {
	if b == nil || !b.Enabled() {
		return inner
	}
	return WorkSourceFunc(func(ctx context.Context) ([]Demand, error) {
		if inner == nil {
			return nil, nil
		}
		if !b.holding() {
			return inner.Demands(ctx)
		}
		ds, err := inner.Demands(ctx)
		return budgetFinishing(ds), err
	})
}

// holding reports whether new work is being held back, after giving a
// resumed autopilot run its fresh headroom.
func (b *RunBudget) holding() bool {
	b.mu.Lock()
	if !b.tripped {
		b.mu.Unlock()
		return false
	}
	// Only the user can clear a pause, so a run this budget paused that is
	// no longer paused is one the user told to carry on. A budget that
	// could not pause the run has nothing to read a resume from, and holds
	// new work back until the run ends.
	if !b.paused || b.pauser == nil || b.pauser.Paused() {
		b.mu.Unlock()
		return true
	}
	grants, ceiling := b.grantLocked()
	b.tripped, b.paused, b.reason, b.banner = false, false, "", ""
	b.mu.Unlock()

	b.log.Info("the run was resumed over its budget", slog.Int("grants", grants))
	if b.host != nil {
		b.host.Publish(events.Warning{Meta: b.host.Meta(), Source: SourceLimits, Message: fmt.Sprintf(
			"the run was resumed over its budget: it will pause again at %s", ceiling)})
	}
	return false
}

// grantLocked widens the ceilings until the spend so far is under them
// again, so a resumed run really gets another budget's worth of headroom
// rather than pausing again on its next agent. It returns the grants made
// and the new ceiling, for the notice.
func (b *RunBudget) grantLocked() (grants int, ceiling string) {
	for range 64 {
		b.grants++
		if !b.exceededLocked() {
			break
		}
	}
	switch {
	case b.maxCost > 0 && b.spend.HasCost:
		ceiling = fmt.Sprintf("$%.2f", b.ceilCostLocked())
	default:
		ceiling = fmt.Sprintf("%d tokens", b.ceilTokensLocked())
	}
	return b.grants, ceiling
}

// budgetFinishing keeps only the demands that finish a review round already
// in flight. Implementation and planning are the new work a crossed budget
// stops.
func budgetFinishing(ds []Demand) []Demand {
	kept := make([]Demand, 0, len(ds))
	for _, d := range ds {
		switch d.Kind {
		case DemandReview, DemandArbitrate:
			kept = append(kept, d)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// Stop wraps a stop condition so that a brief run whose budget was crossed
// finishes once the reviews in flight have drained, with the budget in the
// summary (P9-01). An autopilot run is never finished here: D-18 says a cap
// pauses it, and [RunBudget.Record] has already done that.
//
// A nil or disabled budget returns inner unchanged.
func (b *RunBudget) Stop(inner StopCondition) StopCondition {
	if b == nil || !b.Enabled() {
		return inner
	}
	return StopConditionFunc(func(ctx context.Context, r *Run, st SchedulerStatus) (Outcome, bool, error) {
		if b.mode == domain.RunModeBrief && b.Tripped() && st.Live == 0 && st.Idle {
			return Outcome{Reason: events.FinishStopped, Summary: b.Summary()}, true, nil
		}
		if inner == nil {
			return Outcome{}, false, nil
		}
		return inner.Done(ctx, r, st)
	})
}

// Summary is the finish summary of a run the budget ended: the banner,
// followed by what it cost. It is [Outcome.Summary], which is what
// [events.RunFinished] carries and `sc status` prints, so the reason a
// brief run stopped short is on the record (P9-01). It is empty until a
// budget fires.
func (b *RunBudget) Summary() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.banner == "" {
		return ""
	}
	return fmt.Sprintf("%s (%s over %s)", b.banner,
		budgetPlural(b.spend.Tokens, "token"), budgetPlural(int64(b.spend.Firings), "agent session"))
}

// budgetPlural renders "1 token" / "2 tokens".
func budgetPlural(n int64, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// MeterPlanner wraps a firing planner so that every firing it plans reports
// its usage to the budget when the session ends. It is how the budget is
// fed in production: the supervisor plans every scheduler firing through
// one [FiringPlanner], so one wrapper meters engineers, testers, arbitrating
// and planning product owners alike.
//
// Only a session that finished and decoded is metered:
// [AgentFiring.Result], the hook this chains onto, runs on success. A
// crashed or timed-out session's partial usage is not counted, which errs
// towards letting the run carry on rather than pausing it over work that
// produced nothing. A caller that holds such a transcript can add it with
// [RunBudget.Record].
//
// A nil or disabled budget returns inner unchanged.
func (b *RunBudget) MeterPlanner(inner FiringPlanner) FiringPlanner {
	if b == nil || !b.Enabled() || inner == nil {
		return inner
	}
	return FiringPlannerFunc(func(ctx context.Context, f Firing) (AgentFiring, error) {
		af, err := inner.PlanFiring(ctx, f)
		if err != nil {
			return af, err
		}
		b.MeterFiring(&af)
		return af, nil
	})
}

// MeterPlanRunner wraps the planning product owner's runner so its session
// is metered too (P9-17).
//
// The planning firing does not go through the scheduler's [FiringPlanner]:
// [PlanRouter] hands a plan demand to the [Planner], which fires the
// product owner through its own [PlanRunner] and builds the [AgentFiring]
// itself. So [RunBudget.MeterPlanner] never sees it, and without this
// wrapper `limits.max_tokens` and `limits.max_cost_usd` under-count by the
// whole planning spend — which in autopilot is the one agent that fires for
// ever (D-18).
//
// Unlike a firing planner's Result hook, which only runs on a session that
// finished and decoded, the plan runner is handed the transcript on both
// paths, and a plan the planner rejected (P6-19) cost real tokens and will
// be fired again. So anything that reported usage is counted, whatever the
// outcome; a session that reported none is not counted at all.
//
// A nil or disabled budget returns inner unchanged.
func (b *RunBudget) MeterPlanRunner(inner PlanRunner) PlanRunner {
	if b == nil || !b.Enabled() || inner == nil {
		return inner
	}
	return PlanRunnerFunc(func(ctx context.Context, f Firing, req agents.PlanRequest) (domain.Plan, agents.Transcript, error) {
		plan, tr, err := inner.RunPlan(ctx, f, req)
		if UsageReported(tr.Usage) {
			b.Record(tr.Usage)
		}
		return plan, tr, err
	})
}

// UsageReported reports whether a cartridge said anything about what a
// session cost. A cartridge that reports nothing leaves an empty
// [cartridge.Usage], and metering that would count sessions the budget
// cannot price.
func UsageReported(u cartridge.Usage) bool {
	return u.TotalTokens() > 0 || u.CacheReadTokens > 0 || u.CacheWriteTokens > 0 || u.HasCost
}

// MeterFiring chains usage accounting onto one firing's [AgentFiring.Result]
// hook, for a caller that builds an [AgentFiring] itself rather than
// through a [FiringPlanner]. The hook it adds never fails, so it can never
// turn a finished session into a failed firing.
func (b *RunBudget) MeterFiring(af *AgentFiring) {
	if b == nil || af == nil || !b.Enabled() {
		return
	}
	prev := af.Result
	af.Result = func(payload any, tr agents.Transcript) error {
		b.Record(tr.Usage)
		if prev != nil {
			return prev(payload, tr)
		}
		return nil
	}
}
