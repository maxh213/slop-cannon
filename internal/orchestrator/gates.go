package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// This file is the gate polling loop (P6-10) and the policy for escalated
// gh:pr gates (P6-14).
//
// With safety on, a delivered bead does not close when its pull request is
// opened: the cannon creates a native `gh:pr` gate that blocks the bead
// (D-20), so the bead stays `delivering` and its dependents stay out of
// `bd ready` until a human merges. Something has to notice the merge, and
// that is [GatePoller]: every [GatePollerOptions.Interval] it runs
// `bd gate check --type=gh:pr`, which asks `gh pr view` about every open
// gate, and then acts on the verdicts:
//
//	resolved  → the PR merged; bd closed the gate, so the bead is closed
//	            (delivered → closed) and an [events.Merged] is published
//	pending   → the PR is still open; nothing happens
//	ESCALATE  → the PR was closed without merging; the bead is moved to
//	            needs_human and the gate is resolved (see below)
//	error     → the check itself failed; one Warning, retried next poll
//
// When `gh` is missing or unauthenticated every check errors, so the poll
// is skipped with a single Warning rather than one per gate, and the next
// Warning waits until gh has come back and gone away again.
//
// P6-14 — the policy for an escalated gate. bd 1.2.2 reports ESCALATE for a
// PR that was closed without merging (or that no longer exists) but changes
// nothing: the gate stays open, and an open gate keeps the bead out of
// `bd ready` and refuses `bd close` for its dependents for ever. A human
// who abandons a PR has no reason to know that a tracker gate is still
// waiting on it. The poller therefore **resolves the gate itself**
// ([EscalatedGateResolve], the default) at the same time as it moves the
// bead to `needs_human`: the bead is then in a frozen state a person must
// act on, which is the real block, and nothing invisible is left holding
// the dependents. [EscalatedGateKeep] is there for a caller that would
// rather leave the gate for a human to clear by hand.
//
// The other half of P6-14 is a delivery retry meeting a gate left over from
// an earlier pull request: [ClearStaleGate] resolves an open gh:pr gate
// whose PR number is not the one about to be delivered, so the retry does
// not end up with two gates on one bead.

// GateWarningSource is the [events.Warning] Source the gate poller stamps
// on everything it reports.
const GateWarningSource = "gates"

// DefaultGatePollInterval is how often the poller runs `bd gate check`
// when the caller sets no interval (`gate_poll_interval`, P6-10). A human
// merge is not urgent and every poll shells out to `gh` once per open
// gate, so a minute is the documented default.
const DefaultGatePollInterval = 60 * time.Second

// ErrNoGateStore is returned by [NewGatePoller] when no gate store was
// given: there is nothing to poll.
var ErrNoGateStore = errors.New("orchestrator: gate poller needs a gate store")

// EscalatedGatePolicy says what the poller does with a gate bd escalated
// (P6-14).
type EscalatedGatePolicy string

const (
	// EscalatedGateResolve resolves the gate as the bead moves to
	// needs_human, so the abandoned pull request stops blocking the bead's
	// dependents. It is the default and the zero value.
	EscalatedGateResolve EscalatedGatePolicy = ""
	// EscalatedGateKeep leaves the gate open for a human to resolve or for
	// the pull request to be reopened and merged. The bead still moves to
	// needs_human.
	EscalatedGateKeep EscalatedGatePolicy = "keep"
)

// String returns the policy name ("resolve" for the zero value).
func (p EscalatedGatePolicy) String() string {
	if p == EscalatedGateResolve {
		return "resolve"
	}
	return string(p)
}

// Resolves reports whether the policy clears the gate.
func (p EscalatedGatePolicy) Resolves() bool { return p != EscalatedGateKeep }

// GateStore is the slice of *beads.Client the poller writes and reads
// gates through (P3-08). Every method matches the client's signature, so
// *beads.Client satisfies it and tests use a fake.
type GateStore interface {
	// CheckGates runs `bd gate check --type=gh:pr`, which consults `gh`
	// and closes the gates whose pull request merged.
	CheckGates(ctx context.Context, opts beads.GateCheckOptions) (beads.GateCheck, error)
	// ShowGate reads one gate, open or closed: it is the only way to learn
	// which bead a gate blocks, because `bd gate check` prints ids only.
	ShowGate(ctx context.Context, id domain.BeadID) (beads.Gate, error)
	// ResolveGate closes a gate by hand (the P6-14 escalation policy).
	ResolveGate(ctx context.Context, id domain.BeadID, reason string) error
}

// GateBeadReader is the read the poller needs on the beads themselves: a
// transition needs the state the bead is in, and the machine deliberately
// never looks it up. *beads.Client satisfies it.
type GateBeadReader interface {
	Show(ctx context.Context, ids ...domain.BeadID) ([]beads.Issue, error)
}

// *beads.Client is the production gate store and bead reader.
var (
	_ GateStore      = (*beads.Client)(nil)
	_ GateBeadReader = (*beads.Client)(nil)
)

// GatePollerOptions configures a [GatePoller].
type GatePollerOptions struct {
	// Gates is the gate store. Required.
	Gates GateStore
	// Beads reads the beads a gate blocks. Nil means the poller cannot see
	// what state a bead is in, so it assumes `delivering` — which is where
	// a gated bead sits by construction (D-20) — and acts on that.
	Beads GateBeadReader
	// States applies and persists the transitions. Nil means a dry run: the
	// poller reports what it would do and publishes the events, but writes
	// nothing to bd (a [StateMachine] with no store, see [NewStateMachine]).
	States *StateMachine
	// Host is the run. It receives every event and every log line; nil
	// falls back to Sink, Run and Logger.
	Host EventHost
	// Sink receives the events when there is no Host. Nil publishes
	// nothing.
	Sink Publisher
	// Run stamps the events when there is no Host.
	Run domain.RunID
	// Scope, when set, restricts the poller to the beads of that run
	// (`sc:run:<id>`), so a gate another tool created in the same
	// repository is left alone. Empty acts on every gh:pr gate. It needs
	// Beads to be set; without a bead reader nothing can be scoped and the
	// poller acts on every gate.
	Scope domain.RunID
	// Statuses resolves the repository's own custom statuses so a foreign
	// bead is never transitioned (P3-14, P6-12). Nil still resolves every
	// D-07 state.
	Statuses beads.StatusMap
	// Branch is the default branch name, recorded on [events.Merged].
	Branch string
	// Escalated is the P6-14 policy for a pull request closed without
	// merging; the zero value resolves the gate.
	Escalated EscalatedGatePolicy
	// Interval is the polling period; zero or negative means
	// [DefaultGatePollInterval].
	Interval time.Duration
	// Clock is the time source; nil means [beads.SystemClock]. Tests pass a
	// fake one and nothing in the loop reads the wall clock.
	Clock beads.Clock
	// Wake is an extra kick channel: send (or close) to poll at once. A
	// delivery that has just opened a pull request can use it.
	Wake <-chan struct{}
	// Logger overrides the host's logger; nil means the host's, else
	// slog.Default().
	Logger *slog.Logger
}

// GatePollStats counts what the poller has done, for `sc status` and tests.
type GatePollStats struct {
	// Polls counts every `bd gate check` attempt.
	Polls int
	// Failures counts the polls where bd itself failed.
	Failures int
	// Skipped counts the polls skipped because gh was unavailable.
	Skipped int
	// Closed, Blocked and Resolved count the beads closed, the beads moved
	// to needs_human and the gates resolved by the P6-14 policy.
	Closed   int
	Blocked  int
	Resolved int
	// LastAt is the clock time of the last completed poll.
	LastAt time.Time
	// GHDown is true while gh is unavailable and polling is being skipped.
	GHDown bool
}

// GatePollResult is what one poll did.
type GatePollResult struct {
	// Check is bd's own report, counts and per-gate verdicts included.
	Check beads.GateCheck
	// Closed lists the beads whose pull request merged, now closed.
	Closed []domain.BeadID
	// Blocked lists the beads whose pull request was closed without
	// merging, now needs_human.
	Blocked []domain.BeadID
	// Resolved lists the gates the P6-14 policy closed.
	Resolved []domain.BeadID
	// Skipped is true when gh was unavailable and no gate was checked.
	Skipped bool
	// Reason explains a skip.
	Reason string
}

// Empty reports whether the poll changed nothing.
func (r GatePollResult) Empty() bool {
	return len(r.Closed) == 0 && len(r.Blocked) == 0 && len(r.Resolved) == 0
}

// GatePoller polls the run's `gh:pr` gates and acts on the verdicts
// (P6-10, P6-14). Obtain one with [NewGatePoller]; [GatePoller.Run] drives
// it in its own goroutine alongside the scheduler, and
// [GatePoller.Poll] runs a single pass for a caller that wants to drive the
// timing itself.
//
// All methods are safe for concurrent use, but the polls themselves are
// serialised: a [GatePoller.Kick] while a poll is in flight makes the next
// one happen immediately afterwards rather than at the same time.
type GatePoller struct {
	gates    GateStore
	reader   GateBeadReader
	states   *StateMachine
	host     EventHost
	sink     Publisher
	run      domain.RunID
	scope    domain.RunID
	statuses beads.StatusMap
	branch   string
	policy   EscalatedGatePolicy
	interval time.Duration
	clock    beads.Clock
	wake     <-chan struct{}
	log      *slog.Logger

	kickCh chan struct{}

	mu     sync.Mutex
	stats  GatePollStats
	warned map[string]bool
	cache  map[domain.BeadID]beads.Gate
}

// NewGatePoller returns a poller configured by opts. It fails only when
// there is no gate store to poll.
func NewGatePoller(opts GatePollerOptions) (*GatePoller, error) {
	if opts.Gates == nil {
		return nil, ErrNoGateStore
	}
	p := &GatePoller{
		gates:    opts.Gates,
		reader:   opts.Beads,
		states:   opts.States,
		host:     opts.Host,
		sink:     opts.Sink,
		run:      opts.Run,
		scope:    opts.Scope,
		statuses: opts.Statuses,
		branch:   opts.Branch,
		policy:   opts.Escalated,
		interval: opts.Interval,
		clock:    opts.Clock,
		wake:     opts.Wake,
		log:      opts.Logger,
		kickCh:   make(chan struct{}, 1),
		warned:   map[string]bool{},
		cache:    map[domain.BeadID]beads.Gate{},
	}
	if p.interval <= 0 {
		p.interval = DefaultGatePollInterval
	}
	if p.clock == nil {
		p.clock = beads.SystemClock()
	}
	if p.log == nil {
		if opts.Host != nil {
			p.log = opts.Host.Log()
		} else {
			p.log = slog.Default()
		}
	}
	if p.states == nil {
		// A machine with no store is the documented dry run: it validates
		// the transition and publishes the event without writing to bd.
		p.states = NewStateMachine(nil,
			WithStatePublisher(PublisherFunc(p.publish)),
			WithStateRun(p.run),
			WithStateClock(p.clock.Now))
	}
	return p, nil
}

// Interval is the polling period in force.
func (p *GatePoller) Interval() time.Duration { return p.interval }

// Policy is the escalated-gate policy in force (P6-14).
func (p *GatePoller) Policy() EscalatedGatePolicy { return p.policy }

// Stats returns a snapshot of the counters.
func (p *GatePoller) Stats() GatePollStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stats
}

// Kick makes the loop poll at once instead of waiting for the next
// interval. It never blocks: a kick that arrives while one is pending is
// dropped, because a poll that has not happened yet will ask `gh` the same
// question.
func (p *GatePoller) Kick() {
	select {
	case p.kickCh <- struct{}{}:
	default:
	}
}

// Run polls until ctx is cancelled, starting with one poll straight away
// and then on every kick, on every send to [GatePollerOptions.Wake] and at
// least every interval. It always returns nil: a failing poll is a Warning
// and the next poll tries again, because the gates outlive any one check
// (a `gh` outage must not take the run down). Cancelling ctx ends it.
//
// The poller keeps running while the run is paused: it fires no agent and
// spends no credits, and a human who merges a pull request during a pause
// should still see the bead close.
func (p *GatePoller) Run(ctx context.Context) error {
	p.log.Info("gate polling started",
		slog.Duration("interval", p.interval),
		slog.String("escalated_gate_policy", p.policy.String()))
	for {
		if ctx.Err() != nil {
			return nil
		}
		if _, err := p.Poll(ctx); err != nil && ctx.Err() != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-p.kickCh:
		case <-p.wake:
		case <-p.clock.After(p.interval):
		}
	}
}

// Poll runs one `bd gate check --type=gh:pr` and acts on every verdict.
//
// The `--escalate` flag is deliberately not passed: bd 1.2.2 reports
// ESCALATE for a closed pull request either way and the flag only shells
// out to Gas Town's `gt escalate`, which is not part of this tool
// (Appendix A). The error is bd itself failing to run; everything a single
// gate can get wrong is a Warning and a retry next time.
func (p *GatePoller) Poll(ctx context.Context) (GatePollResult, error) {
	var res GatePollResult
	check, err := p.gates.CheckGates(ctx, beads.GateCheckOptions{})
	p.mu.Lock()
	p.stats.Polls++
	p.stats.LastAt = p.clock.Now()
	if err != nil {
		p.stats.Failures++
	}
	p.mu.Unlock()
	if err != nil {
		if ctx.Err() == nil {
			p.warnOnce("check:"+err.Error(), "", "could not check the pull request gates", err)
		}
		return res, err
	}
	res.Check = check

	if reason, down := check.GHUnavailable(); down {
		res.Skipped, res.Reason = true, reason
		p.ghUnavailable(reason)
		return res, nil
	}
	p.ghAvailable()

	for _, w := range check.Warnings {
		p.warnOnce("warning:"+w, "", w, nil)
	}
	if check.Checked > 0 && len(check.Results) == 0 {
		p.warnOnce("no-verdicts", "", fmt.Sprintf("bd checked %d gates but printed no per-gate verdict", check.Checked), nil)
	}

	for _, r := range check.Results {
		if ctx.Err() != nil {
			return res, nil
		}
		switch r.Outcome {
		case beads.GateResolved:
			p.merged(ctx, r, &res)
		case beads.GateEscalated:
			p.escalated(ctx, r, &res)
		case beads.GatePending, beads.GateError:
			// Pending is the normal case and needs nothing; an errored
			// check is already in check.Warnings and is retried next poll.
		}
	}
	p.mu.Lock()
	p.stats.Closed += len(res.Closed)
	p.stats.Blocked += len(res.Blocked)
	p.stats.Resolved += len(res.Resolved)
	p.mu.Unlock()
	return res, nil
}

// merged closes the bead of a gate whose pull request landed (D-20) and
// publishes [events.Merged].
func (p *GatePoller) merged(ctx context.Context, r beads.GateResult, res *GatePollResult) {
	g, ok := p.gate(ctx, r.GateID)
	if !ok {
		return
	}
	issue, state, ok := p.bead(ctx, g)
	if !ok {
		return
	}
	pr := gatePRNumber(g, issue)
	if state == domain.BeadStateClosed {
		return // already closed: a resume, or a second verdict for one merge
	}
	if state == domain.BeadStateNeedsHuman {
		// The gate escalated earlier and the pull request was reopened and
		// merged after all. Put the bead back in flight before closing it
		// (P6-16 uses the same edge for a delivery retry).
		if !p.apply(ctx, StateRequest{
			Bead:   g.Blocks,
			From:   state,
			Event:  BeadEventDeliveryRetried,
			Reason: gateMergeReason(pr),
			Round:  issue.Round(),
		}) {
			return
		}
		state = domain.BeadStateDelivering
	}
	if state != domain.BeadStateDelivering {
		p.warnOnce(fmt.Sprintf("merged-state:%s:%s", g.ID, state), g.Blocks,
			fmt.Sprintf("gate %s resolved but %s is %s, not delivering; leaving it alone", g.ID, g.Blocks, state), nil)
		return
	}
	if !p.apply(ctx, StateRequest{
		Bead:   g.Blocks,
		From:   domain.BeadStateDelivering,
		Event:  BeadEventDelivered,
		Reason: gateMergeReason(pr),
		Round:  issue.Round(),
	}) {
		return
	}
	res.Closed = append(res.Closed, g.Blocks)
	p.log.Info("gate resolved: the pull request merged",
		slog.String("bead", string(g.Blocks)), slog.String("gate", string(g.ID)), slog.Int("pr", pr))
	p.publish(events.Merged{
		Meta:     p.meta(),
		Bead:     g.Blocks,
		SHA:      issue.MergeSHA(),
		Branch:   p.branch,
		PRNumber: pr,
		Pushed:   true,
	})
}

// escalated parks the bead of a gate whose pull request was closed without
// merging and applies the P6-14 policy to the gate itself.
func (p *GatePoller) escalated(ctx context.Context, r beads.GateResult, res *GatePollResult) {
	g, ok := p.gate(ctx, r.GateID)
	if !ok {
		return
	}
	issue, state, ok := p.bead(ctx, g)
	if !ok {
		return
	}
	pr := gatePRNumber(g, issue)
	reason := gateEscalationReason(pr, r.Detail)
	switch state {
	case domain.BeadStateClosed, domain.BeadStateFailed:
		// Nothing left to park; the gate is still cleared below so it stops
		// blocking anything else.
	case domain.BeadStateNeedsHuman:
		// Already waiting for a person.
	default:
		if !p.apply(ctx, StateRequest{
			Bead:   g.Blocks,
			From:   state,
			Event:  BeadEventBlocked,
			Reason: reason,
			Round:  issue.Round(),
		}) {
			return
		}
		res.Blocked = append(res.Blocked, g.Blocks)
		p.warn(g.Blocks, fmt.Sprintf("%s: %s needs a human", reason, g.Blocks), nil)
	}
	if !p.policy.Resolves() {
		p.warnOnce("kept-gate:"+string(g.ID), g.Blocks,
			fmt.Sprintf("gate %s stays open on %s until the pull request is merged or a human resolves it", g.ID, g.Blocks), nil)
		return
	}
	if err := p.gates.ResolveGate(ctx, g.ID, reason); err != nil {
		p.warnOnce("resolve:"+string(g.ID)+":"+err.Error(), g.Blocks,
			fmt.Sprintf("could not resolve the escalated gate %s on %s", g.ID, g.Blocks), err)
		return
	}
	res.Resolved = append(res.Resolved, g.ID)
	p.forget(g.ID)
	p.log.Info("escalated gate resolved",
		slog.String("bead", string(g.Blocks)), slog.String("gate", string(g.ID)), slog.Int("pr", pr))
}

// gate resolves a gate id to the gate itself, which is the only way to
// learn the bead it blocks: `bd gate check` prints gate ids only. Gates do
// not change once created, so the lookup is cached for the poller's life.
func (p *GatePoller) gate(ctx context.Context, id domain.BeadID) (beads.Gate, bool) {
	p.mu.Lock()
	g, ok := p.cache[id]
	p.mu.Unlock()
	if !ok {
		var err error
		g, err = p.gates.ShowGate(ctx, id)
		if err != nil {
			if ctx.Err() == nil {
				p.warnOnce("show-gate:"+string(id)+":"+err.Error(), "",
					fmt.Sprintf("could not read gate %s", id), err)
			}
			return beads.Gate{}, false
		}
		p.mu.Lock()
		p.cache[id] = g
		p.mu.Unlock()
	}
	if g.Blocks == "" {
		p.warnOnce("gate-blocks:"+string(id), "",
			fmt.Sprintf("gate %s names no bead it blocks; leaving it alone", id), nil)
		return beads.Gate{}, false
	}
	return g, true
}

// forget drops a gate from the cache, so a later gate on the same bead is
// read afresh.
func (p *GatePoller) forget(id domain.BeadID) {
	p.mu.Lock()
	delete(p.cache, id)
	p.mu.Unlock()
}

// bead reads the bead a gate blocks and resolves the state to transition
// from. It reports false for a bead the poller must not touch: one outside
// the scoped run, one carrying a status the repository defined itself, or
// one in a state bd owns rather than the cannon (P3-14, P6-12).
//
// With no bead reader the poller has nothing to read, so it assumes
// `delivering`, which is where a gated bead sits by construction (D-20).
func (p *GatePoller) bead(ctx context.Context, g beads.Gate) (beads.Issue, domain.BeadState, bool) {
	if p.reader == nil {
		return beads.Issue{ID: g.Blocks}, domain.BeadStateDelivering, true
	}
	issues, err := p.reader.Show(ctx, g.Blocks)
	if len(issues) == 0 {
		if ctx.Err() == nil {
			p.warnOnce("show-bead:"+string(g.Blocks), g.Blocks,
				fmt.Sprintf("could not read %s, the bead gate %s blocks", g.Blocks, g.ID), err)
		}
		return beads.Issue{}, "", false
	}
	issue := issues[0]
	if p.scope != "" && !issue.InRun(p.scope) {
		return beads.Issue{}, "", false // another tool's gate; not ours to act on
	}
	state, origin := beads.SafeState(issue.Status, p.statuses[issue.Status])
	if origin == beads.OriginForeign {
		p.warnOnce("foreign:"+string(g.Blocks)+":"+issue.Status, g.Blocks,
			fmt.Sprintf("%s carries the status %q, which slop-cannon did not register; leaving it alone", g.Blocks, issue.Status), nil)
		return beads.Issue{}, "", false
	}
	if !state.Workflow() {
		p.warnOnce("nonworkflow:"+string(g.Blocks)+":"+string(state), g.Blocks,
			fmt.Sprintf("%s is %s, which slop-cannon only reads; leaving it alone", g.Blocks, state), nil)
		return beads.Issue{}, "", false
	}
	return issue, state, true
}

// apply runs one transition and reports whether it took. A refused
// transition already published its own Warning from the state machine, so
// only a bd failure is warned about here.
func (p *GatePoller) apply(ctx context.Context, req StateRequest) bool {
	if _, err := p.states.Apply(ctx, req); err != nil {
		if gateTransitionRefused(err) {
			p.log.Warn("gate poller: transition refused",
				slog.String("bead", string(req.Bead)),
				slog.String("event", req.Event.String()),
				slog.Any("err", err))
			return false
		}
		if ctx.Err() == nil {
			p.warnOnce(fmt.Sprintf("apply:%s:%s:%s", req.Bead, req.Event, err), req.Bead,
				fmt.Sprintf("could not move %s on %q", req.Bead, req.Event), err)
		}
		return false
	}
	return true
}

// gateTransitionRefused reports whether the state machine rejected the
// request itself (and so has already warned) rather than bd failing.
func gateTransitionRefused(err error) bool {
	return errors.Is(err, ErrInvalidTransition) ||
		errors.Is(err, ErrNotWorkflowState) ||
		errors.Is(err, ErrUnknownBeadEvent) ||
		errors.Is(err, ErrNoAgent)
}

// ghUnavailable records a poll skipped because `gh` is missing or not
// authenticated and publishes the single Warning P6-10 asks for: one per
// outage, not one per gate and not one per poll.
func (p *GatePoller) ghUnavailable(reason string) {
	p.mu.Lock()
	p.stats.Skipped++
	first := !p.stats.GHDown
	p.stats.GHDown = true
	p.mu.Unlock()
	if !first {
		p.log.Debug("gate polling still skipped: gh is unavailable", slog.String("reason", reason))
		return
	}
	p.warn("", "gate polling skipped: gh is unavailable, so merged pull requests will not close their beads", errors.New(reason))
}

// ghAvailable clears the outage, so the next one is warned about again.
func (p *GatePoller) ghAvailable() {
	p.mu.Lock()
	back := p.stats.GHDown
	p.stats.GHDown = false
	p.mu.Unlock()
	if back {
		p.log.Info("gh is available again; gate polling resumed")
	}
}

// warn publishes an events.Warning and logs it.
func (p *GatePoller) warn(bead domain.BeadID, message string, err error) {
	w := events.Warning{Meta: p.meta(), Source: GateWarningSource, Message: message, Bead: bead}
	if err != nil {
		w.Err = err.Error()
	}
	p.log.Warn(message, slog.String("source", GateWarningSource), slog.String("bead", string(bead)), slog.Any("err", err))
	p.publish(w)
}

// warnOnce is warn for something that would otherwise be repeated on every
// poll: a gate whose check keeps failing, a bead the poller must not touch.
// The key identifies the problem, so a different problem still warns.
func (p *GatePoller) warnOnce(key string, bead domain.BeadID, message string, err error) {
	p.mu.Lock()
	seen := p.warned[key]
	p.warned[key] = true
	p.mu.Unlock()
	if seen {
		p.log.Debug(message, slog.String("source", GateWarningSource), slog.Any("err", err))
		return
	}
	p.warn(bead, message, err)
}

// publish sends ev to the host, or to the sink when there is no host.
func (p *GatePoller) publish(ev events.Event) {
	switch {
	case p.host != nil:
		p.host.Publish(ev)
	case p.sink != nil:
		p.sink.Publish(ev)
	}
}

// meta stamps an event with the run and the poller's clock.
func (p *GatePoller) meta() events.Meta {
	if p.host != nil {
		return p.host.Meta()
	}
	return events.MetaAt(p.run, p.clock.Now())
}

// gatePRNumber is the pull request the gate waits on: the gate's own
// --await-id, or the bead's sc_pr_number metadata when the gate carries
// none.
func gatePRNumber(g beads.Gate, issue beads.Issue) int {
	if n, ok := g.PRNumber(); ok {
		return n
	}
	return issue.PRNumber()
}

// gateMergeReason is the `bd close --reason` text for a merged pull
// request.
func gateMergeReason(pr int) string {
	if pr > 0 {
		return fmt.Sprintf("pull request #%d merged", pr)
	}
	return "the pull request merged"
}

// gateEscalationReason is the reason recorded on the bead and the gate when
// a pull request was closed without merging. bd's own detail is appended
// when it says something the number does not.
func gateEscalationReason(pr int, detail string) string {
	reason := "the pull request was closed without merging"
	if pr > 0 {
		reason = fmt.Sprintf("pull request #%d was closed without merging", pr)
	}
	if d := strings.TrimSpace(detail); d != "" && !strings.EqualFold(d, reason) {
		reason += " (" + d + ")"
	}
	return reason
}

// GateResolver is what [ClearStaleGate] needs; *beads.Client satisfies it.
type GateResolver interface {
	// GateFor returns the open gate blocking a bead, or beads.ErrNoGate.
	GateFor(ctx context.Context, bead domain.BeadID) (beads.Gate, error)
	// ResolveGate closes a gate by hand.
	ResolveGate(ctx context.Context, id domain.BeadID, reason string) error
}

// *beads.Client is the production GateResolver.
var _ GateResolver = (*beads.Client)(nil)

// ClearStaleGate resolves an open `gh:pr` gate left on a bead by an earlier
// pull request, so a delivery retry can create the gate for the pull
// request it is about to open without the bead ending up behind two gates
// (P6-14). It is the call [gitx] cannot make itself: the beads client
// belongs to the orchestrator.
//
// It reports the gate it cleared and true. Nothing is cleared, and false is
// reported with a nil error, when the bead has no open gate
// ([beads.ErrNoGate]), when its open gate is not a gh:pr gate, or when the
// gate already waits on prNumber — that gate is the one the retry wants and
// creating it again is unnecessary, not wrong. A non-positive prNumber
// clears any open gh:pr gate, which is what a delivery that has lost its
// pull request wants.
func ClearStaleGate(ctx context.Context, r GateResolver, bead domain.BeadID, prNumber int, reason string) (beads.Gate, bool, error) {
	if r == nil {
		return beads.Gate{}, false, fmt.Errorf("orchestrator: clear stale gate: %w", ErrNoGateStore)
	}
	g, err := r.GateFor(ctx, bead)
	if err != nil {
		if errors.Is(err, beads.ErrNoGate) {
			return beads.Gate{}, false, nil
		}
		return beads.Gate{}, false, err
	}
	if !g.IsPR() {
		return beads.Gate{}, false, nil
	}
	if n, ok := g.PRNumber(); ok && prNumber > 0 && n == prNumber {
		return g, false, nil
	}
	if strings.TrimSpace(reason) == "" {
		reason = "superseded by a new delivery"
		if prNumber > 0 {
			reason = fmt.Sprintf("superseded by pull request #%d", prNumber)
		}
	}
	if err := r.ResolveGate(ctx, g.ID, reason); err != nil {
		return g, false, err
	}
	return g, true, nil
}
