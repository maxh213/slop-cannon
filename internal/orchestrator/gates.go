package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/config"
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
// A merge verdict arrives exactly once. `bd gate check` *closes* the gate
// as it resolves it, so the very next check prints "No open gates of type
// 'gh:pr' found." (verified against bd 1.2.2) and nothing would ever bring
// the bead up again. Anything that fails between that one verdict and the
// closed bead — the `bd gate show` that says which bead the gate blocks,
// the `bd show` of the bead itself, the `bd close` (the bead picked up a
// blocker while the pull request was in review, a Dolt lock, any transient
// bd failure) — would therefore strand the bead in `delivering` for the
// life of the run: its dependents would never become ready, a brief run
// would never finish and an autopilot run would idle for ever (D-18). The
// poller instead *remembers* the whole merge, from the moment it is told
// about it and by the gate's id ([gatePendingClose]), and retries it on
// every later poll — including the polls skipped because gh is down (the
// close is bd's business, not gh's) — until the bead is closed or turns
// out to be someone else's. A remembered merge whose gate has not been
// read yet reads it again first; nothing about a merge is dropped for a
// transient failure. Memory the process loses to a kill is what `--resume`
// reconciles: gitx maps `delivering` + a MERGED pull request to
// RedeliverPR.
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
// The end of the file is P6-29: [RunVitals], the "why is nothing
// happening?" snapshot that gathers [GatePollStats] and [PlanningStatus]
// into the lines `sc status` prints and the one line the TUI header shows.
//
// The other half of P6-14 is a delivery retry meeting a gate left over from
// an earlier pull request, and it belongs to the step that creates the
// gate: [gitx.PRDeliverer] resolves an open gh:pr gate whose PR number is
// not the one it is delivering before it creates its own (P6-22), so the
// retry cannot end up with two gates on one bead — which under D-20 would
// mean the bead never closes, whichever pull request merges.

// SourceGates is the [events.Warning] Source the gate poller stamps
// on everything it reports.
const SourceGates = "gates"

// DefaultGatePollInterval is how often the poller runs `bd gate check`
// when the caller sets no interval. A human merge is not urgent and every
// poll shells out to `gh` once per open gate, so a minute is the
// documented default; it is the configuration key `gate_poll_interval`
// (P6-10, P1-17), and this is the same constant the loader defaults it to,
// not a second copy of the number.
const DefaultGatePollInterval = config.DefaultGatePollInterval

// ErrNoGateStore is returned by [NewGatePoller] when no gate store was
// given: there is nothing to poll.
var ErrNoGateStore = errors.New("orchestrator: gate poller needs a gate store")

// ErrScopeNeedsBeads is returned by [NewGatePoller] when
// [GatePollerOptions.Scope] is set without [GatePollerOptions.Beads]. The
// scope lives on the bead (`sc:run:<id>`), so with no bead reader nothing
// can be scoped and the poller would silently close, park and announce the
// beads of every other run and every other tool in the repository. A caller
// that really means "act on every gh:pr gate" leaves Scope empty.
var ErrScopeNeedsBeads = errors.New("orchestrator: gate poller scope needs a bead reader")

// gateWarnKeyLimit bounds the once-only warning bookkeeping. The keys are
// stable (a bead, a gate, a transition — never an error message, whose text
// varies with the port dialled or the lock file named), so a run reaches
// this only after hundreds of distinct problems; the map is then dropped
// rather than grown for ever in a process D-18 expects to run indefinitely.
// Starting again means a very old problem may be reported a second time,
// which is the right way round: a poller that has warned about hundreds of
// things has more to say, not less.
const gateWarnKeyLimit = 512

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
	// repository is left alone. Empty acts on every gh:pr gate. Setting it
	// without Beads is [ErrScopeNeedsBeads]: the label is on the bead, so
	// with no bead reader the scope could not be honoured and the poller
	// would act on every gate while looking as though it were confined to
	// one run.
	Scope domain.RunID
	// Statuses resolves the repository's own custom statuses so a foreign
	// bead is never transitioned (P3-14, P6-12). Nil still resolves every
	// D-07 state.
	Statuses beads.StatusMap
	// Foreign reports the beads the poller passes over because their status
	// is not one slop-cannon registered (P6-28). Pass the run's own
	// reporter, so a bead the scheduler and the poller both meet is
	// reported once by the run rather than once by each of them; nil makes
	// the poller build one of its own from Host/Sink, Run and Statuses.
	Foreign *ForeignStatusReporter
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
	// Wake is an extra kick channel: send to poll at once. A delivery that
	// has just opened a pull request can use it. Closing it wakes the loop
	// one last time and is then ignored (a closed channel is ready for ever,
	// which would otherwise spin the loop); cancel the context to stop.
	Wake <-chan struct{}
	// Logger overrides the host's logger; nil means the host's, else
	// slog.Default().
	Logger *slog.Logger
}

// GatePollStats counts what the poller has done, for `sc status`, the TUI
// header (P6-29) and tests. Every field is tagged, so the whole struct can
// be written into state.json and printed by `sc status --json` without a
// second shape being defined for it.
type GatePollStats struct {
	// Polls counts every `bd gate check` attempt.
	Polls int `json:"polls"`
	// Failures counts the polls where bd itself failed.
	Failures int `json:"failures,omitempty"`
	// Skipped counts the polls skipped because gh was unavailable.
	Skipped int `json:"skipped,omitempty"`
	// Closed, Blocked and Resolved count the beads closed, the beads moved
	// to needs_human and the gates resolved by the P6-14 policy.
	Closed   int `json:"closed,omitempty"`
	Blocked  int `json:"blocked,omitempty"`
	Resolved int `json:"resolved,omitempty"`
	// Retries counts the close attempts made for a bead whose gate had
	// already resolved: a bead that bd refused to close first time round.
	Retries int `json:"retries,omitempty"`
	// Pending is how many merges are waiting to be finished right now: a
	// bead whose close bd refused, or a gate whose read failed. Anything
	// but zero for long means bd keeps refusing, which the Warnings
	// explain.
	Pending int `json:"pending,omitempty"`
	// Foreign counts the distinct beads the poller passed over because bd
	// holds them in a status slop-cannon did not register, or in one of
	// bd's read-only built-ins (P6-28). It counts a bead once, however many
	// polls meet it.
	Foreign int `json:"foreign,omitempty"`
	// LastAt is the clock time of the last completed poll.
	LastAt time.Time `json:"last_at,omitempty"`
	// GHDown is true while gh is unavailable and polling is being skipped.
	GHDown bool `json:"gh_down,omitempty"`
	// GHDownSince is when the outage in force started, so a reader can say
	// how long `gh` has been away without following the run log back to the
	// Warning (P6-29). It is zero whenever GHDown is false.
	GHDownSince time.Time `json:"gh_down_since,omitempty"`
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
	// Pending lists the beads whose pull request merged but which are not
	// closed yet, in id order. The poller retries each of them on every
	// later poll; the list is the state after this one, so a bead closed by
	// this poll's retry is in Closed and not here. A merge whose gate could
	// not be read yet names no bead, so it appears in PendingGates only.
	Pending []domain.BeadID
	// PendingGates lists the gates of every remembered merge, in id order:
	// the ones in Pending and the ones whose `bd gate show` has not
	// succeeded yet, which have no bead id to name. It is the complete
	// picture of what the poller still owes, and its length is
	// [GatePollStats.Pending].
	PendingGates []domain.BeadID
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
// All methods are safe for concurrent use, and the polls themselves are
// serialised by a lock of their own: a [GatePoller.Kick] while a poll is in
// flight makes the next one happen immediately afterwards rather than at
// the same time, and a second caller of [GatePoller.Poll] waits for the
// first to finish rather than double-counting the retries, closing a bead
// twice or publishing two [events.Merged] for one merge.
type GatePoller struct {
	gates    GateStore
	reader   GateBeadReader
	states   *StateMachine
	host     EventHost
	sink     Publisher
	run      domain.RunID
	scope    domain.RunID
	statuses beads.StatusMap
	foreign  *ForeignStatusReporter
	branch   string
	policy   EscalatedGatePolicy
	interval time.Duration
	clock    beads.Clock
	wake     <-chan struct{}
	log      *slog.Logger

	kickCh chan struct{}

	// pollMu serialises whole polls. It is never held while mu is taken in
	// the other order — mu is only ever taken *under* it — so the two
	// cannot deadlock.
	pollMu sync.Mutex

	mu      sync.Mutex
	stats   GatePollStats
	warned  map[string]bool
	cache   map[domain.BeadID]beads.Gate
	pending map[domain.BeadID]gatePendingClose
}

// gatePendingClose is a merge the poller has been told about but has not
// finished: bd reported the gate resolved — the pull request landed and bd
// closed the gate as it said so — and something between that verdict and
// the closed bead failed. bd reports a resolved gate exactly once, so
// without this the bead would stay `delivering` for ever; the poller keeps
// what it knows and tries again on every poll.
//
// It is keyed and identified by the *gate* id, not the bead's, because the
// gate id is the only thing bd's verdict carries: a `bd gate show` that
// fails leaves the bead unknown, and a merge must survive that too. A bead
// that somehow ends up behind two gates is therefore remembered once per
// gate, and the second close finds the bead already closed and forgets it.
type gatePendingClose struct {
	// id is the gate the merge verdict named. Always set.
	id domain.BeadID
	// gate is the resolved gate, kept because it is no longer in the cache
	// and `bd gate show` need not be asked again for something immutable.
	// It is the zero Gate until read is true.
	gate beads.Gate
	// read says whether gate holds a gate `bd gate show` returned. False
	// means the show failed and the next poll must ask again before it can
	// know which bead the merge closes.
	read bool
	// since is the clock time of the first failure, and tries how many
	// attempts have been made, for the log line.
	since time.Time
	tries int
}

// bead is the bead the merge closes, or "" while the gate is unread.
func (e gatePendingClose) bead() domain.BeadID { return e.gate.Blocks }

// NewGatePoller returns a poller configured by opts. It fails when there is
// no gate store to poll ([ErrNoGateStore]) and when a scope was asked for
// that could not be honoured ([ErrScopeNeedsBeads]).
func NewGatePoller(opts GatePollerOptions) (*GatePoller, error) {
	if opts.Gates == nil {
		return nil, ErrNoGateStore
	}
	if opts.Scope != "" && opts.Beads == nil {
		return nil, fmt.Errorf("%w: scope %s", ErrScopeNeedsBeads, opts.Scope)
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
		foreign:  opts.Foreign,
		branch:   opts.Branch,
		policy:   opts.Escalated,
		interval: opts.Interval,
		clock:    opts.Clock,
		wake:     opts.Wake,
		log:      opts.Logger,
		kickCh:   make(chan struct{}, 1),
		warned:   map[string]bool{},
		cache:    map[domain.BeadID]beads.Gate{},
		pending:  map[domain.BeadID]gatePendingClose{},
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
	if p.foreign == nil {
		p.foreign = NewForeignStatusReporter(ForeignStatusOptions{
			Host:     opts.Host,
			Sink:     opts.Sink,
			Run:      p.run,
			Statuses: p.statuses,
			Clock:    p.clock.Now,
			Logger:   p.log,
		})
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

// Foreign is the reporter the poller tells about a bead it may not touch
// (P6-28): whatever [GatePollerOptions.Foreign] gave it, or the one it
// built for itself. The run reads it for [RunVitals] when it did not supply
// one of its own; it is never nil.
func (p *GatePoller) Foreign() *ForeignStatusReporter { return p.foreign }

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
	// A local copy of the wake channel, because a closed channel is ready
	// for ever: the loop drops it on the close rather than spinning on it
	// (one `bd gate check`, and one `gh pr view` per open gate, as fast as
	// bd will answer). Cancelling ctx is how a caller ends the loop.
	wake := p.wake
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
		case _, ok := <-wake:
			if !ok {
				p.log.Debug("the gate poller's wake channel was closed; polling on the interval from now on")
				wake = nil
			}
		case <-p.clock.After(p.interval):
		}
	}
}

// Poll runs one `bd gate check --type=gh:pr`, acts on every verdict, and
// then retries the closes an earlier poll could not finish.
//
// The `--escalate` flag is deliberately not passed: bd 1.2.2 reports
// ESCALATE for a closed pull request either way and the flag only shells
// out to Gas Town's `gt escalate`, which is not part of this tool
// (Appendix A). The error is bd itself failing to run; everything a single
// gate can get wrong is a Warning and another attempt next time — for a
// merge, whose verdict bd hands out exactly once, that attempt comes from
// the poller's own pending list rather than from bd.
//
// Polls are serialised: a second caller waits for the one in flight rather
// than acting on the same verdicts beside it.
func (p *GatePoller) Poll(ctx context.Context) (GatePollResult, error) {
	p.pollMu.Lock()
	defer p.pollMu.Unlock()
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
			p.warnOnce("check", "", "could not check the pull request gates", err)
		}
		p.record(&res)
		return res, err
	}
	p.clearWarning("check")
	res.Check = check

	if reason, down := check.GHUnavailable(); down {
		res.Skipped, res.Reason = true, reason
		p.ghUnavailable(reason)
		// gh is what is down, not bd: a close that failed earlier is still
		// worth retrying, and a bead stuck in `delivering` is exactly what
		// an outage must not make permanent.
		p.retryPendingCloses(ctx, nil, &res)
		p.record(&res)
		return res, nil
	}
	p.ghAvailable()

	for _, w := range check.Warnings {
		p.warnOnce("warning:"+w, "", w, nil)
	}
	if check.Checked > 0 && len(check.Results) == 0 {
		p.warnOnce("no-verdicts", "", fmt.Sprintf("bd checked %d gates but printed no per-gate verdict", check.Checked), nil)
	}

	seen := make(map[domain.BeadID]bool, len(check.Results))
	for _, r := range check.Results {
		if ctx.Err() != nil {
			p.record(&res)
			return res, nil
		}
		seen[r.GateID] = true
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
	p.retryPendingCloses(ctx, seen, &res)
	p.record(&res)
	return res, nil
}

// record folds one poll's work into the counters and reports what is still
// waiting to be closed.
func (p *GatePoller) record(res *GatePollResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stats.Closed += len(res.Closed)
	p.stats.Blocked += len(res.Blocked)
	p.stats.Resolved += len(res.Resolved)
	p.stats.Pending = len(p.pending)
	for id, e := range p.pending {
		res.PendingGates = append(res.PendingGates, id)
		if bead := e.bead(); bead != "" {
			res.Pending = append(res.Pending, bead)
		}
	}
	slices.Sort(res.Pending)
	slices.Sort(res.PendingGates)
}

// retryPendingCloses tries again to finish every merge an earlier poll
// could not: a gate whose `bd gate show` failed is read again first, and a
// bead bd would not close is closed again. Gates this poll has already
// acted on are skipped, so one verdict is never applied twice.
func (p *GatePoller) retryPendingCloses(ctx context.Context, seen map[domain.BeadID]bool, res *GatePollResult) {
	for _, e := range p.pendingCloses() {
		if ctx.Err() != nil {
			return
		}
		if seen[e.id] {
			continue
		}
		p.mu.Lock()
		p.stats.Retries++
		p.mu.Unlock()
		g := e.gate
		if !e.read {
			// The merge is known, the gate it named is not: read it again
			// before anything else can be decided about the bead.
			var verdict gateBeadVerdict
			g, verdict = p.gate(ctx, e.id)
			switch verdict {
			case gateBeadSkip:
				p.settle(e.id) // a gate naming no bead never will
				continue
			case gateBeadRetry:
				p.rememberGate(e.id) // still unreadable; try again next poll
				continue
			}
		}
		p.finishMerge(ctx, g, res)
	}
}

// pendingCloses snapshots the unfinished merges in gate-id order, so a poll
// is deterministic and the lock is not held across bd calls.
func (p *GatePoller) pendingCloses() []gatePendingClose {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.pending) == 0 {
		return nil
	}
	ids := make([]domain.BeadID, 0, len(p.pending))
	for id := range p.pending {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]gatePendingClose, 0, len(ids))
	for _, id := range ids {
		out = append(out, p.pending[id])
	}
	return out
}

// remember queues a merge whose bead was not closed for another attempt on
// the next poll. The gate is dropped from the cache at the same time: it is
// closed in bd now, the pending entry holds the copy, and nothing should
// keep growing in a process meant to run for days.
func (p *GatePoller) remember(g beads.Gate) { p.rememberMerge(g.ID, g, true) }

// rememberGate queues a merge whose gate could not be read. The gate id is
// all bd's verdict carried, and it is enough: the next poll reads the gate
// again and finishes the merge from there. Without this the one verdict bd
// hands out would be lost to a transient `bd gate show` failure and the
// bead would stay `delivering` for ever (D-20).
func (p *GatePoller) rememberGate(id domain.BeadID) { p.rememberMerge(id, beads.Gate{}, false) }

// rememberMerge records or refreshes one unfinished merge. A gate that has
// been read is never forgotten by a later attempt that could not read it.
func (p *GatePoller) rememberMerge(id domain.BeadID, g beads.Gate, read bool) {
	now := p.clock.Now()
	p.mu.Lock()
	e, ok := p.pending[id]
	if !ok {
		e = gatePendingClose{id: id, since: now}
	}
	if read || !e.read {
		e.gate, e.read = g, read
	}
	e.id = id
	e.tries++
	p.pending[id] = e
	delete(p.cache, id)
	tries, since := e.tries, e.since
	bead := e.bead()
	p.mu.Unlock()
	// The first attempt is worth a line; the repeats are not, because a
	// bead bd keeps refusing would otherwise log once a minute for days.
	// The one Warning the failure earned is what a human acts on.
	level := slog.LevelDebug
	if tries == 1 {
		level = slog.LevelInfo
	}
	p.log.Log(context.Background(), level, "the merge will be finished on the next poll",
		slog.String("bead", string(bead)),
		slog.String("gate", string(id)),
		slog.Bool("gate_read", read || bead != ""),
		slog.Int("attempts", tries),
		slog.Time("since", since))
}

// settle forgets a gate: its bead is closed, or is not the poller's to
// touch. Both maps drop it, so neither grows with the run.
func (p *GatePoller) settle(id domain.BeadID) {
	p.mu.Lock()
	delete(p.pending, id)
	delete(p.cache, id)
	p.mu.Unlock()
}

// merged closes the bead of a gate whose pull request landed (D-20) and
// publishes [events.Merged].
//
// bd hands out this verdict exactly once — it closed the gate as it
// resolved it — so a gate it cannot read is *remembered by its id* and read
// again on the next poll rather than dropped: dropping it would strand the
// bead in `delivering` for the life of the run.
func (p *GatePoller) merged(ctx context.Context, r beads.GateResult, res *GatePollResult) {
	g, verdict := p.gate(ctx, r.GateID)
	switch verdict {
	case gateBeadSkip:
		p.settle(r.GateID) // the gate names no bead; nothing to close, ever
		return
	case gateBeadRetry:
		p.rememberGate(r.GateID)
		return
	}
	p.finishMerge(ctx, g, res)
}

// finishMerge is the whole of "this pull request merged, so close its
// bead", reached either from bd's own resolved verdict or from the pending
// list on a later poll. A bead it could not close is remembered for the
// next poll; one already closed, or one that is not the poller's to touch,
// is forgotten.
func (p *GatePoller) finishMerge(ctx context.Context, g beads.Gate, res *GatePollResult) {
	issue, state, verdict := p.bead(ctx, g)
	switch verdict {
	case gateBeadSkip:
		p.settle(g.ID) // not ours: never retried
		return
	case gateBeadRetry:
		p.remember(g) // the read failed, so nothing is known yet
		return
	}
	pr := gatePRNumber(g, issue)
	if state == domain.BeadStateClosed {
		// Already closed: a resume, a second verdict for one merge, or a
		// close that landed elsewhere while this one was pending.
		p.settle(g.ID)
		return
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
			p.remember(g)
			return
		}
		state = domain.BeadStateDelivering
	}
	if state != domain.BeadStateDelivering {
		// Something else owns the bead just now (a conflict round, a
		// redelivery). The merge is not forced on it, but it is not
		// forgotten either: the gate is gone, so this is the only record
		// that the pull request landed.
		p.warnOnce(fmt.Sprintf("merged-state:%s:%s", g.ID, state), g.Blocks,
			fmt.Sprintf("gate %s resolved but %s is %s, not delivering; leaving it alone", g.ID, g.Blocks, state), nil)
		p.remember(g)
		return
	}
	if !p.apply(ctx, StateRequest{
		Bead:   g.Blocks,
		From:   domain.BeadStateDelivering,
		Event:  BeadEventDelivered,
		Reason: gateMergeReason(pr),
		Round:  issue.Round(),
	}) {
		p.remember(g)
		return
	}
	p.settle(g.ID)
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
//
// Nothing here is remembered when a step fails, because bd leaves an
// escalated gate open and reports it again on the very next poll — the
// opposite of a merge, whose verdict arrives once.
func (p *GatePoller) escalated(ctx context.Context, r beads.GateResult, res *GatePollResult) {
	g, verdict := p.gate(ctx, r.GateID)
	if verdict != gateBeadAct {
		return // unreadable or nameless; bd escalates it again next poll
	}
	issue, state, verdict := p.bead(ctx, g)
	if verdict != gateBeadAct {
		return // not ours, or unreadable; bd reports an escalation again next poll
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
		p.warnOnce("resolve:"+string(g.ID), g.Blocks,
			fmt.Sprintf("could not resolve the escalated gate %s on %s", g.ID, g.Blocks), err)
		return
	}
	p.clearWarning("resolve:" + string(g.ID))
	res.Resolved = append(res.Resolved, g.ID)
	p.settle(g.ID)
	p.log.Info("escalated gate resolved",
		slog.String("bead", string(g.Blocks)), slog.String("gate", string(g.ID)), slog.Int("pr", pr))
}

// gate resolves a gate id to the gate itself, which is the only way to
// learn the bead it blocks: `bd gate check` prints gate ids only. A gate
// does not change once created, so the lookup is cached — but only until
// the gate is closed ([GatePoller.settle], [GatePoller.remember]), so the
// cache holds the open gates rather than every gate the run has ever seen.
//
// The verdict is the same three-way answer [GatePoller.bead] gives, and
// for the same reason: gateBeadRetry means the read failed and is worth
// another go, gateBeadSkip means the gate names no bead it blocks and
// never will, so trying again would only reach the same answer.
func (p *GatePoller) gate(ctx context.Context, id domain.BeadID) (beads.Gate, gateBeadVerdict) {
	p.mu.Lock()
	g, ok := p.cache[id]
	p.mu.Unlock()
	if !ok {
		var err error
		g, err = p.gates.ShowGate(ctx, id)
		if err != nil {
			if ctx.Err() == nil {
				p.warnOnce("show-gate:"+string(id), "",
					fmt.Sprintf("could not read gate %s", id), err)
			}
			return beads.Gate{}, gateBeadRetry
		}
		p.clearWarning("show-gate:" + string(id))
		p.mu.Lock()
		p.cache[id] = g
		p.mu.Unlock()
	}
	if g.Blocks == "" {
		p.warnOnce("gate-blocks:"+string(id), "",
			fmt.Sprintf("gate %s names no bead it blocks; leaving it alone", id), nil)
		return beads.Gate{}, gateBeadSkip
	}
	return g, gateBeadAct
}

// gateBeadVerdict says what the poller may do with the bead behind a gate.
type gateBeadVerdict int

const (
	// gateBeadAct: the bead was read and slop-cannon owns its state.
	gateBeadAct gateBeadVerdict = iota
	// gateBeadSkip: not this poller's bead — another run's, one carrying a
	// status the repository defined itself, or one in a state bd owns
	// rather than the cannon. Never retried, because trying again would
	// only reach the same answer.
	gateBeadSkip
	// gateBeadRetry: the read itself failed, so nothing is known about the
	// bead yet and the next poll should look again.
	gateBeadRetry
)

// bead reads the bead a gate blocks and resolves the state to transition
// from, together with the verdict on whether the poller may act on it
// (P3-14, P6-12).
//
// With no bead reader the poller has nothing to read, so it assumes
// `delivering`, which is where a gated bead sits by construction (D-20).
func (p *GatePoller) bead(ctx context.Context, g beads.Gate) (beads.Issue, domain.BeadState, gateBeadVerdict) {
	if p.reader == nil {
		return beads.Issue{ID: g.Blocks}, domain.BeadStateDelivering, gateBeadAct
	}
	issues, err := p.reader.Show(ctx, g.Blocks)
	if len(issues) == 0 {
		if ctx.Err() == nil {
			p.warnOnce("show-bead:"+string(g.Blocks), g.Blocks,
				fmt.Sprintf("could not read %s, the bead gate %s blocks", g.Blocks, g.ID), err)
		}
		return beads.Issue{}, "", gateBeadRetry
	}
	p.clearWarning("show-bead:" + string(g.Blocks))
	issue := issues[0]
	if p.scope != "" && !issue.InRun(p.scope) {
		return beads.Issue{}, "", gateBeadSkip // another tool's gate; not ours to act on
	}
	// The P6-12 guard lives in state.go and is called, not re-implemented:
	// a foreign status is refused on its origin even when it maps onto a
	// workflow state, and bd's read-only built-ins are refused on the
	// state. The run's [ForeignStatusReporter] publishes the one Warning
	// the bead owes a human (P6-28) — once per bead, not once per poll —
	// and says whether this poll was the first to meet it, which is what
	// keeps [GatePollStats.Foreign] a count of beads rather than of polls.
	state, first, err := p.foreign.Resolve(g.Blocks, issue.Status)
	if err != nil {
		if first {
			p.mu.Lock()
			p.stats.Foreign++
			p.mu.Unlock()
		}
		return beads.Issue{}, "", gateBeadSkip
	}
	return issue, state, gateBeadAct
}

// apply runs one transition and reports whether it took. A refused
// transition already published its own Warning from the state machine, so
// only a bd failure is warned about here.
func (p *GatePoller) apply(ctx context.Context, req StateRequest) bool {
	key := fmt.Sprintf("apply:%s:%s", req.Bead, req.Event)
	if _, err := p.states.Apply(ctx, req); err != nil {
		if gateTransitionRefused(err) {
			p.log.Warn("gate poller: transition refused",
				slog.String("bead", string(req.Bead)),
				slog.String("event", req.Event.String()),
				slog.Any("err", err))
			return false
		}
		if ctx.Err() == nil {
			// The key is the bead and the event, never the error text: a bd
			// failure whose message carries a lock path or a port number
			// would otherwise warn again on every single poll.
			p.warnOnce(key, req.Bead, fmt.Sprintf("could not move %s on %q", req.Bead, req.Event), err)
		}
		return false
	}
	p.clearWarning(key)
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
	if first {
		p.stats.GHDownSince = p.clock.Now()
	}
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
	p.stats.GHDownSince = time.Time{}
	p.mu.Unlock()
	if back {
		p.log.Info("gh is available again; gate polling resumed")
	}
}

// warn publishes an events.Warning and logs it.
func (p *GatePoller) warn(bead domain.BeadID, message string, err error) {
	w := events.Warning{Meta: p.meta(), Source: SourceGates, Message: message, Bead: bead}
	if err != nil {
		w.Err = err.Error()
	}
	p.log.Warn(message, slog.String("source", SourceGates), slog.String("bead", string(bead)), slog.Any("err", err))
	p.publish(w)
}

// warnOnce is warn for something that would otherwise be repeated on every
// poll: a gate whose check keeps failing, a bead the poller must not touch.
// The key identifies the problem — the gate, the bead, the transition, and
// never an error message, whose text varies between attempts — so a
// different problem still warns and the same one does not warn twice.
// [GatePoller.clearWarning] arms it again once the operation succeeds.
func (p *GatePoller) warnOnce(key string, bead domain.BeadID, message string, err error) {
	p.mu.Lock()
	seen := p.warned[key]
	if !seen && len(p.warned) >= gateWarnKeyLimit {
		p.warned = make(map[string]bool, gateWarnKeyLimit)
		p.log.Debug("the gate poller has warned about too many distinct problems; starting the once-only bookkeeping again",
			slog.Int("limit", gateWarnKeyLimit))
	}
	p.warned[key] = true
	p.mu.Unlock()
	if seen {
		p.log.Debug(message, slog.String("source", SourceGates), slog.Any("err", err))
		return
	}
	p.warn(bead, message, err)
}

// clearWarning forgets a once-only warning, so the next failure of that
// same operation is worth reporting again. It is the counterpart of the
// stable keys: "the gate check failed" is warned about once per outage, not
// once per distinct error message and not once for the life of the run.
func (p *GatePoller) clearWarning(key string) {
	p.mu.Lock()
	delete(p.warned, key)
	p.mu.Unlock()
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

// ------------------ P6-29: the vitals `sc status` and the TUI header read

// A run that is doing nothing looks exactly like a run that is stuck, and
// until P6-29 nothing said which: [Planner.Status] knows when the next
// planning firing is due and [GatePoller.Stats] knows that `gh` has been
// unavailable for an hour and that a merge bd keeps refusing to close is
// still being retried, but neither number left the process. [RunVitals]
// gathers them — with the count of beads the run is passing over (P6-28) —
// into one value with one renderer, so `sc status` and the TUI header say
// the same thing in the same words.
//
// It lives beside [GatePollStats] because that is the half of it this file
// owns; [PlanningStatus] is only read.

// vitalsTimeFormat is how [RunVitals] prints an absolute time: UTC, to the
// second, the same layout `sc status` uses for its own header.
const vitalsTimeFormat = "2006-01-02 15:04:05Z"

// PlanningReporter is the slice of [*Planner] the vitals read.
type PlanningReporter interface {
	// Status is the planning loop's counters (P6-05).
	Status() PlanningStatus
}

// GateReporter is the slice of [*GatePoller] the vitals read.
type GateReporter interface {
	// Stats is the gate poller's counters (P6-10).
	Stats() GatePollStats
}

// The production reporters.
var (
	_ PlanningReporter = (*Planner)(nil)
	_ GateReporter     = (*GatePoller)(nil)
)

// RunVitals is why a run looks idle: what the planner is waiting for, what
// the gate poller is waiting for, and how many beads the run is leaving
// alone. It is a snapshot with no methods on the live parts, so it can be
// written into state.json, sent over an event or held by a TUI frame.
type RunVitals struct {
	// At is when the snapshot was taken; the renderers phrase a time
	// relative to it. Zero prints every time absolutely.
	At time.Time `json:"at,omitempty"`
	// Planning is the planning loop's counters, zero when the run has no
	// planner.
	Planning PlanningStatus `json:"planning"`
	// Gates is the gate poller's counters, zero when the run has no poller
	// (safety off never opens a pull request, so it needs none).
	Gates GatePollStats `json:"gates"`
	// Foreign is how many beads the run reported it may not touch (P6-28);
	// it counts every component's reports, not just the gate poller's.
	Foreign int `json:"foreign,omitempty"`
	// Rules is the rules compliance gate's counters (P9-06), zero for a run
	// that never saw a violation of a rules file. It is folded in with
	// [RunVitals.WithRules] and printed by [RunVitals.RulesLine].
	Rules RulesGateStats `json:"rules,omitzero"`
}

// Vitals snapshots the run's parts. Every argument is optional: a nil
// planner, poller or reporter leaves its half of the snapshot zero, which
// is what a run with no planning loop or with safety off produces.
func Vitals(at time.Time, plan PlanningReporter, gates GateReporter, foreign *ForeignStatusReporter) RunVitals {
	v := RunVitals{At: at, Foreign: foreign.Count()}
	if plan != nil {
		v.Planning = plan.Status()
	}
	if gates != nil {
		v.Gates = gates.Stats()
	}
	return v
}

// Lines are the vitals as `sc status` prints them: one line for planning,
// one for the gates, one for the beads the run is passing over when there
// are any, and one for the rules compliance gate when it has seen
// anything.
func (v RunVitals) Lines() []string {
	out := []string{v.PlanningLine(), v.GatesLine()}
	if l := v.SignalsLine(); l != "" {
		out = append(out, l)
	}
	if l := v.ForeignLine(); l != "" {
		out = append(out, l)
	}
	if l := v.RulesLine(); l != "" {
		out = append(out, l)
	}
	return out
}

// PlanningLine says what the planner has done and what it is waiting for.
func (v RunVitals) PlanningLine() string {
	p := v.Planning
	label := "planning"
	if p.Mode != "" {
		label += " (" + string(p.Mode) + ")"
	}
	if p.Attempts == 0 && !p.Running && !p.Finished && !p.ResumedPlan {
		return label + ": nothing planned yet"
	}
	parts := []string{fmt.Sprintf("%d firings, %d plans, %d beads", p.Attempts, p.Plans, p.Beads)}
	switch {
	case p.Finished:
		parts = append(parts, "finished")
	case p.Running:
		parts = append(parts, "planning now")
	case !p.NextAt.IsZero():
		parts = append(parts, "next "+v.when(p.NextAt))
	default:
		parts = append(parts, "waiting for the loop")
	}
	if p.ResumedPlan {
		parts = append(parts, "plan resumed")
	}
	if p.Empty > 0 {
		parts = append(parts, fmt.Sprintf("%d proposed nothing", p.Empty))
	}
	if p.Rejections > 0 {
		parts = append(parts, fmt.Sprintf("%d rejected in a row", p.Rejections))
	}
	if p.Failures > 0 {
		parts = append(parts, fmt.Sprintf("%d failed in a row", p.Failures))
	}
	if p.GaveUp > 0 {
		parts = append(parts, fmt.Sprintf("%d given up", p.GaveUp))
	}
	if p.Deprioritised > 0 {
		parts = append(parts, fmt.Sprintf("%d deprioritised", p.Deprioritised))
	}
	if p.LastError != "" {
		parts = append(parts, "last error: "+p.LastError)
	}
	return label + ": " + strings.Join(parts, " · ")
}

// GatesLine says what the gate poller has done and what is holding it up: a
// `gh` outage, or a merge bd will not let it finish.
func (v RunVitals) GatesLine() string {
	g := v.Gates
	if g.Polls == 0 {
		if g.GHDown {
			return "gates: not polled yet · " + v.outage(g)
		}
		return "gates: not polled yet"
	}
	parts := []string{fmt.Sprintf("%d polls", g.Polls)}
	if !g.LastAt.IsZero() {
		parts = append(parts, "last "+v.when(g.LastAt))
	}
	if g.Closed > 0 {
		parts = append(parts, fmt.Sprintf("%d merged", g.Closed))
	}
	if g.Blocked > 0 {
		parts = append(parts, fmt.Sprintf("%d need a human", g.Blocked))
	}
	if g.GHDown {
		parts = append(parts, v.outage(g))
	}
	if g.Pending > 0 {
		pending := fmt.Sprintf("%d merge pending", g.Pending)
		if g.Pending > 1 {
			pending = fmt.Sprintf("%d merges pending", g.Pending)
		}
		if g.Retries > 0 {
			pending += fmt.Sprintf(" after %d retries", g.Retries)
		}
		parts = append(parts, pending)
	}
	if g.Failures > 0 {
		parts = append(parts, fmt.Sprintf("%d failed polls", g.Failures))
	}
	return "gates: " + strings.Join(parts, " · ")
}

// SignalsLine reports the production signals command (P6-71): when it last
// ran, how it exited, how much it kept and when it is next due; "" when the
// run takes no signals.
func (v RunVitals) SignalsLine() string {
	s := v.Planning.Signals
	if s == nil {
		return ""
	}
	if s.LastAt.IsZero() {
		if s.Running {
			return "signals: running now"
		}
		return "signals: not run yet, due on the next plan"
	}
	parts := []string{"last " + v.when(s.LastAt), fmt.Sprintf("exit %d", s.ExitCode), fmt.Sprintf("%d bytes", s.Bytes)}
	switch {
	case s.Running:
		parts = append(parts, "running now")
	case !s.NextAt.IsZero():
		parts = append(parts, "next "+v.when(s.NextAt))
	default:
		parts = append(parts, "due on the next plan")
	}
	if s.LastError != "" {
		parts = append(parts, "last error: "+s.LastError)
	}
	return "signals: " + strings.Join(parts, " · ")
}

// ForeignLine reports the beads the run is leaving alone (P6-28), or "" if
// there are none.
func (v RunVitals) ForeignLine() string {
	if v.Foreign <= 0 {
		return ""
	}
	if v.Foreign == 1 {
		return "beads: 1 left alone, in a status slop-cannon did not register"
	}
	return fmt.Sprintf("beads: %d left alone, in statuses slop-cannon did not register", v.Foreign)
}

// Header is the vitals compressed to one line for the TUI header: only what
// a person needs to see without asking, and nothing when there is nothing
// to say.
func (v RunVitals) Header() string {
	var parts []string
	switch p := v.Planning; {
	case p.Running:
		parts = append(parts, "planning now")
	case p.Finished:
		parts = append(parts, "planning done")
	case !p.NextAt.IsZero():
		parts = append(parts, "plan "+v.when(p.NextAt))
	}
	if s := v.Planning.Signals; s != nil && !s.LastAt.IsZero() {
		sig := fmt.Sprintf("signals exit %d, %d bytes", s.ExitCode, s.Bytes)
		if !s.NextAt.IsZero() {
			sig += ", next " + v.when(s.NextAt)
		}
		parts = append(parts, sig)
	}
	if v.Gates.GHDown {
		parts = append(parts, "gh down "+v.since(v.Gates.GHDownSince))
	}
	if n := v.Gates.Pending; n > 0 {
		parts = append(parts, fmt.Sprintf("%d pending", n))
	}
	if v.Foreign > 0 {
		parts = append(parts, fmt.Sprintf("%d left alone", v.Foreign))
	}
	return strings.Join(parts, " · ")
}

// when phrases a time relative to the snapshot: "in 30s", "5m0s ago", or
// the absolute time when the snapshot carries no clock.
func (v RunVitals) when(t time.Time) string {
	stamp := t.UTC().Format(vitalsTimeFormat)
	if v.At.IsZero() {
		return "at " + stamp
	}
	if d := t.Sub(v.At); d > 0 {
		return fmt.Sprintf("in %s (%s)", vitalsDuration(d), stamp)
	}
	return fmt.Sprintf("%s ago (%s)", vitalsDuration(v.At.Sub(t)), stamp)
}

// since phrases how long ago something started.
func (v RunVitals) since(t time.Time) string {
	if t.IsZero() || v.At.IsZero() {
		return "for an unknown time"
	}
	return vitalsDuration(v.At.Sub(t))
}

// outage is the `gh` clause of [RunVitals.GatesLine].
func (v RunVitals) outage(g GatePollStats) string {
	out := "gh unavailable"
	if !g.GHDownSince.IsZero() {
		out += ", " + v.since(g.GHDownSince)
		if !v.At.IsZero() {
			out += " so far"
		}
	}
	if g.Skipped > 0 {
		out += fmt.Sprintf(" (%d polls skipped)", g.Skipped)
	}
	return out
}

// vitalsDuration renders a duration to the second, never negative.
func vitalsDuration(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}
	return d.Round(time.Second).String()
}
