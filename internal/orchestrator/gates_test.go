package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/testutil"
)

// The gate poller tests (P6-10, P6-14) feed the poller the exact text bd
// 1.2.2 prints for a `bd gate check --type=gh:pr` run — the samples
// captured live in internal/beads/testdata/bd — through the real
// beads.ParseGateCheck, so what the poller acts on is what bd says and not
// a hand-written struct.

const (
	gateRun    = domain.RunID("20260903-120000-abcd")
	gateBranch = "main"
)

var gateEpoch = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

// The captured bd output. Each pair is one `bd gate check` run.
const (
	gateOutMerged = `✓ fx-qba: resolved - PR 'Fix Max' was merged

Checked 1 gates: 1 resolved, 0 escalated, 0 errors
{
  "data": {"checked": 1, "dry_run": false, "errors": 0, "escalated": 0, "resolved": 1},
  "schema_version": 1
}
`
	gateOutPending = `○ fx-qba: pending - PR 'Fix Max' is still open

Checked 1 gates: 0 resolved, 0 escalated, 0 errors
{
  "data": {"checked": 1, "dry_run": false, "errors": 0, "escalated": 0, "resolved": 0},
  "schema_version": 1
}
`
	gateOutClosedPR = `⚠ fx-8za: ESCALATE - PR 'Fix Max' was closed without merging

Checked 1 gates: 0 resolved, 1 escalated, 0 errors
{
  "data": {"checked": 1, "dry_run": false, "errors": 0, "escalated": 1, "resolved": 0},
  "schema_version": 1
}
`
	gateOutErrors = `
Checked 1 gates: 0 resolved, 0 escalated, 1 errors
{
  "data": {"checked": 1, "dry_run": false, "errors": 1, "escalated": 0, "resolved": 0},
  "schema_version": 1
}
`
	gateErrMissingGH = "✗ fx-qba: error checking - gh CLI not installed\n"
	gateErrNetwork   = "✗ fx-qba: error checking - gh pr view failed: error connecting to api.github.com\n"

	gateOutNone = "No open gates of type 'gh:pr' found.\n"

	// One run reporting on two gates at once.
	gateOutBoth = `✓ fx-qba: resolved - PR 'Fix Max' was merged
⚠ fx-8za: ESCALATE - PR 'Fix Max' was closed without merging

Checked 2 gates: 1 resolved, 1 escalated, 0 errors
{
  "data": {"checked": 2, "dry_run": false, "errors": 0, "escalated": 1, "resolved": 1},
  "schema_version": 1
}
`
)

// gateCheck parses one captured bd run.
func gateCheck(t *testing.T, stdout, stderr string) beads.GateCheck {
	t.Helper()
	check, err := beads.ParseGateCheck(stdout, stderr)
	require.NoError(t, err, "the captured bd output must parse")
	return check
}

// gateClock is a fake beads.Clock: nothing in the poller reads the wall
// clock, so a test drives every interval itself.
type gateClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []gateWaiter
}

type gateWaiter struct {
	at time.Time
	ch chan time.Time
}

func newGateClock() *gateClock { return &gateClock{now: gateEpoch} }

func (c *gateClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *gateClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, gateWaiter{at: c.now.Add(d), ch: ch})
	return ch
}

// Advance moves the clock and releases every waiter that has come due.
func (c *gateClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var keep []gateWaiter
	var due []gateWaiter
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			due = append(due, w)
			continue
		}
		keep = append(keep, w)
	}
	c.waiters = keep
	now := c.now
	c.mu.Unlock()
	for _, w := range due {
		w.ch <- now
	}
}

// Waiting reports how many timers are pending.
func (c *gateClock) Waiting() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// gateFakeBD is the bd the poller talks to: the gate store, the bead
// reader and the state store in one, so a transition the poller applies is
// visible to the next poll.
type gateFakeBD struct {
	mu sync.Mutex

	checks   []beads.GateCheck
	last     beads.GateCheck
	hasLast  bool
	checkErr error
	Checks   int

	gates       map[domain.BeadID]beads.Gate
	issues      map[domain.BeadID]beads.Issue
	showGateErr error
	showErr     error
	gateForErr  error
	resolveErr  error
	closeErr    error
	statusErr   error

	Resolved []string // "<gate>: <reason>"
	Statuses []string // "<bead>=<state>"
	Closed   []string // "<bead>: <reason>"
}

func newGateFakeBD() *gateFakeBD {
	return &gateFakeBD{gates: map[domain.BeadID]beads.Gate{}, issues: map[domain.BeadID]beads.Issue{}}
}

// Answer queues one `bd gate check` reply. A queued answer is served
// verbatim, so a test can say "bd reported this merge twice"; when the
// queue runs out the last answer repeats, but with the resolved gates
// dropped, because real bd closes a gate as it resolves it and the very
// next check then prints "No open gates of type 'gh:pr' found." — the
// merge verdict for a gate arrives exactly once (verified against bd
// 1.2.2). Pending, escalated and errored gates stay open in bd, so they do
// keep being reported.
func (f *gateFakeBD) Answer(checks ...beads.GateCheck) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks = append(f.checks, checks...)
}

// repeatLocked is the steady-state answer bd gives once every queued reply
// has been served: the last one, minus the gates it has already resolved
// and closed.
func (f *gateFakeBD) repeatLocked() beads.GateCheck {
	if len(f.last.Results) == 0 {
		return f.last // bd printed counts but no verdicts; nothing to drop
	}
	out := f.last
	out.Results = nil
	out.Checked, out.Resolved = 0, 0
	for _, r := range f.last.Results {
		if r.Outcome == beads.GateResolved {
			continue
		}
		out.Results = append(out.Results, r)
		out.Checked++
	}
	return out
}

// AddGate registers a gh:pr gate blocking bead.
func (f *gateFakeBD) AddGate(id, bead domain.BeadID, pr int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gates[id] = beads.Gate{
		ID:        id,
		Title:     fmt.Sprintf("Gate: gh:pr %d", pr),
		Status:    "open",
		Type:      "gate",
		AwaitType: beads.GateTypePR,
		AwaitID:   fmt.Sprint(pr),
		Blocks:    bead,
	}
}

// AddBead registers a bead of the run in the given state.
func (f *gateFakeBD) AddBead(id domain.BeadID, state domain.BeadState, pr int, labels ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(labels) == 0 {
		labels = []string{beads.RunLabel(gateRun)}
	}
	f.issues[id] = beads.Issue{
		ID:       id,
		Title:    "Fix Max",
		Status:   string(state),
		Labels:   labels,
		Metadata: beads.Metadata{beads.MetaPRNumber: pr, beads.MetaRound: 2},
	}
}

// SetMeta sets one metadata key of a bead.
func (f *gateFakeBD) SetMeta(id domain.BeadID, key string, value any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issues[id]
	meta := beads.Metadata{}
	for k, v := range is.Metadata {
		meta[k] = v
	}
	meta[key] = value
	is.Metadata = meta
	f.issues[id] = is
}

// gate returns a gate as bd holds it now, open or closed.
func (f *gateFakeBD) gate(id domain.BeadID) beads.Gate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gates[id]
}

// State returns a bead's current status.
func (f *gateFakeBD) State(id domain.BeadID) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issues[id].Status
}

func (f *gateFakeBD) CheckGates(_ context.Context, opts beads.GateCheckOptions) (beads.GateCheck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Checks++
	if f.checkErr != nil {
		return beads.GateCheck{}, f.checkErr
	}
	if opts.Escalate || opts.DryRun {
		return beads.GateCheck{}, fmt.Errorf("unexpected options %+v", opts)
	}
	if len(f.checks) > 0 {
		f.last, f.hasLast = f.checks[0], true
		f.checks = f.checks[1:]
		return f.last, nil
	}
	if !f.hasLast {
		return beads.GateCheck{}, nil
	}
	f.last = f.repeatLocked()
	return f.last, nil
}

func (f *gateFakeBD) ShowGate(_ context.Context, id domain.BeadID) (beads.Gate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.showGateErr != nil {
		return beads.Gate{}, f.showGateErr
	}
	g, ok := f.gates[id]
	if !ok {
		return beads.Gate{}, fmt.Errorf("beads: %w: gate %s", beads.ErrNotFound, id)
	}
	return g, nil
}

func (f *gateFakeBD) ResolveGate(_ context.Context, id domain.BeadID, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resolveErr != nil {
		return f.resolveErr
	}
	g, ok := f.gates[id]
	if !ok {
		return fmt.Errorf("beads: %w: gate %s", beads.ErrNotFound, id)
	}
	g.Status = "closed"
	g.CloseReason = reason
	f.gates[id] = g
	f.Resolved = append(f.Resolved, fmt.Sprintf("%s: %s", id, reason))
	return nil
}

func (f *gateFakeBD) GateFor(_ context.Context, bead domain.BeadID) (beads.Gate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gateForErr != nil {
		return beads.Gate{}, f.gateForErr
	}
	for _, g := range f.gates {
		if g.Blocks == bead && g.IsOpen() {
			return g, nil
		}
	}
	return beads.Gate{}, fmt.Errorf("beads: %w: %s", beads.ErrNoGate, bead)
}

func (f *gateFakeBD) Show(_ context.Context, ids ...domain.BeadID) ([]beads.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.showErr != nil {
		return nil, f.showErr
	}
	var out []beads.Issue
	for _, id := range ids {
		if is, ok := f.issues[id]; ok {
			out = append(out, is)
		}
	}
	if len(out) != len(ids) {
		return out, fmt.Errorf("beads: %w: %v", beads.ErrNotFound, ids)
	}
	return out, nil
}

func (f *gateFakeBD) SetStatus(_ context.Context, id domain.BeadID, state domain.BeadState) (beads.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return beads.Issue{}, f.statusErr
	}
	f.Statuses = append(f.Statuses, fmt.Sprintf("%s=%s", id, state))
	return f.setLocked(id, state), nil
}

// fail makes the named bd call fail until it is called again with nil. It
// is the tests' one way to set the fake's error fields, so nothing reaches
// into the fake's lock by hand.
func (f *gateFakeBD) fail(what string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch what {
	case "check":
		f.checkErr = err
	case "show-gate":
		f.showGateErr = err
	case "show":
		f.showErr = err
	case "resolve":
		f.resolveErr = err
	case "close":
		f.closeErr = err
	case "status":
		f.statusErr = err
	default:
		panic("unknown bd call " + what)
	}
}

func (f *gateFakeBD) Close(_ context.Context, id domain.BeadID, reason string) (beads.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closeErr != nil {
		return beads.Issue{}, f.closeErr
	}
	f.Closed = append(f.Closed, fmt.Sprintf("%s: %s", id, reason))
	is := f.setLocked(id, domain.BeadStateClosed)
	is.CloseReason = reason
	f.issues[id] = is
	return is, nil
}

func (f *gateFakeBD) ClaimFor(_ context.Context, id domain.BeadID, _ domain.AgentID) (beads.Issue, error) {
	return beads.Issue{}, fmt.Errorf("the gate poller must never claim %s", id)
}

func (f *gateFakeBD) Reclaim(_ context.Context, id domain.BeadID, _ domain.AgentID) (beads.Issue, error) {
	return beads.Issue{}, fmt.Errorf("the gate poller must never reclaim %s", id)
}

func (f *gateFakeBD) Release(_ context.Context, id domain.BeadID) (beads.Issue, error) {
	return beads.Issue{}, fmt.Errorf("the gate poller must never release %s", id)
}

// setLocked records the new status; the caller holds the lock.
func (f *gateFakeBD) setLocked(id domain.BeadID, state domain.BeadState) beads.Issue {
	is := f.issues[id]
	is.ID = id
	is.Status = string(state)
	f.issues[id] = is
	return is
}

// gateRecorder collects the published events.
type gateRecorder struct {
	mu  sync.Mutex
	evs []events.Event
}

func (r *gateRecorder) Publish(ev events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, ev)
}

func (r *gateRecorder) all() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]events.Event(nil), r.evs...)
}

func (r *gateRecorder) warnings() []events.Warning {
	var out []events.Warning
	for _, ev := range r.all() {
		if w, ok := ev.(events.Warning); ok {
			out = append(out, w)
		}
	}
	return out
}

func (r *gateRecorder) merges() []events.Merged {
	var out []events.Merged
	for _, ev := range r.all() {
		if m, ok := ev.(events.Merged); ok {
			out = append(out, m)
		}
	}
	return out
}

func (r *gateRecorder) transitions() []string {
	var out []string
	for _, ev := range r.all() {
		if c, ok := ev.(events.BeadStateChanged); ok {
			out = append(out, fmt.Sprintf("%s %s→%s", c.Bead, c.From, c.To))
		}
	}
	return out
}

func (r *gateRecorder) kinds() []events.Kind {
	var out []events.Kind
	for _, ev := range r.all() {
		out = append(out, ev.Kind())
	}
	return out
}

// gateHarness wires a poller to the fake bd, the fake clock and a recorder.
type gateHarness struct {
	t     *testing.T
	BD    *gateFakeBD
	Rec   *gateRecorder
	Clock *gateClock
	P     *GatePoller
}

func newGateHarness(t *testing.T, tweak func(*GatePollerOptions)) *gateHarness {
	t.Helper()
	bd := newGateFakeBD()
	rec := &gateRecorder{}
	clock := newGateClock()
	opts := GatePollerOptions{
		Gates:    bd,
		Beads:    bd,
		Sink:     rec,
		Run:      gateRun,
		Scope:    gateRun,
		Branch:   gateBranch,
		Clock:    clock,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Statuses: beads.StatusMap{},
	}
	if tweak != nil {
		tweak(&opts)
	}
	if opts.States == nil && opts.Gates == GateStore(bd) {
		opts.States = NewStateMachine(bd,
			WithStatePublisher(rec),
			WithStateRun(gateRun),
			WithStateClock(clock.Now))
	}
	p, err := NewGatePoller(opts)
	require.NoError(t, err)
	return &gateHarness{t: t, BD: bd, Rec: rec, Clock: clock, P: p}
}

// poll runs one pass and fails the test on an unexpected error.
func (h *gateHarness) poll() GatePollResult {
	h.t.Helper()
	res, err := h.P.Poll(context.Background())
	require.NoError(h.t, err)
	return res
}

func TestGatePollerNeedsAStore(t *testing.T) {
	t.Parallel()
	_, err := NewGatePoller(GatePollerOptions{})
	require.ErrorIs(t, err, ErrNoGateStore)
}

// A scope that could not be honoured is refused rather than silently
// ignored: the run label is on the bead, so without a bead reader the
// poller would close, park and announce every other run's beads while
// looking as though it were confined to one run.
func TestGatePollerScopeNeedsABeadReader(t *testing.T) {
	t.Parallel()
	_, err := NewGatePoller(GatePollerOptions{Gates: newGateFakeBD(), Scope: gateRun})
	require.ErrorIs(t, err, ErrScopeNeedsBeads)
	assert.Contains(t, err.Error(), string(gateRun))

	bd := newGateFakeBD()
	_, err = NewGatePoller(GatePollerOptions{Gates: bd, Beads: bd, Scope: gateRun})
	require.NoError(t, err, "a scope with a bead reader is the normal case")
	_, err = NewGatePoller(GatePollerOptions{Gates: bd})
	require.NoError(t, err, "and no scope at all is the documented opt-out")
}

func TestGatePollerDefaults(t *testing.T) {
	t.Parallel()
	p, err := NewGatePoller(GatePollerOptions{Gates: newGateFakeBD()})
	require.NoError(t, err)
	assert.Equal(t, DefaultGatePollInterval, p.Interval(), "the documented gate_poll_interval default")
	assert.Equal(t, 60*time.Second, DefaultGatePollInterval)
	assert.Equal(t, EscalatedGateResolve, p.Policy(), "P6-14: the poller resolves an escalated gate by default")
	assert.Equal(t, "resolve", p.Policy().String())
	assert.True(t, p.Policy().Resolves())
	assert.False(t, EscalatedGateKeep.Resolves())
	assert.Equal(t, "keep", EscalatedGateKeep.String())

	// A poller with no state machine is a dry run, not a nil dereference.
	bd := newGateFakeBD()
	bd.AddGate("fx-qba", "fx-byi", 1)
	bd.AddBead("fx-byi", domain.BeadStateDelivering, 1)
	bd.Answer(gateCheck(t, gateOutMerged, ""))
	dry, err := NewGatePoller(GatePollerOptions{Gates: bd, Beads: bd, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	require.NoError(t, err)
	res, err := dry.Poll(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []domain.BeadID{"fx-byi"}, res.Closed, "the dry run still reports what it would close")
	assert.Empty(t, bd.Closed, "and writes nothing to bd")
}

// P6-10: a resolved gate closes its bead and emits Merged.
func TestGatePollerClosesTheBeadOfAMergedPR(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	h.BD.AddGate("fx-qba", "fx-byi", 1)
	h.BD.AddBead("fx-byi", domain.BeadStateDelivering, 1)
	h.BD.Answer(gateCheck(t, gateOutMerged, ""))

	res := h.poll()

	assert.Equal(t, []domain.BeadID{"fx-byi"}, res.Closed)
	assert.Empty(t, res.Blocked)
	assert.False(t, res.Skipped)
	assert.False(t, res.Empty())
	assert.Equal(t, []string{"fx-byi: pull request #1 merged"}, h.BD.Closed, "the bead is closed with the PR in the reason")
	assert.Equal(t, string(domain.BeadStateClosed), h.BD.State("fx-byi"))
	assert.Equal(t, []string{"fx-byi delivering→closed"}, h.Rec.transitions())

	merges := h.Rec.merges()
	require.Len(t, merges, 1, "exactly one Merged event")
	assert.Equal(t, domain.BeadID("fx-byi"), merges[0].Bead)
	assert.Equal(t, 1, merges[0].PRNumber)
	assert.Equal(t, gateBranch, merges[0].Branch)
	assert.True(t, merges[0].Pushed, "a merged PR is on the remote")
	assert.Equal(t, gateRun, merges[0].Run)
	assert.Empty(t, h.Rec.warnings())

	stats := h.P.Stats()
	assert.Equal(t, 1, stats.Polls)
	assert.Equal(t, 1, stats.Closed)
	assert.Equal(t, gateEpoch, stats.LastAt)

	// A second verdict for the same merge (a resume, a duplicated line)
	// finds the bead closed and does nothing.
	h.BD.Answer(gateCheck(t, gateOutMerged, ""))
	second := h.poll()
	assert.True(t, second.Empty(), "closing a closed bead is not attempted twice")
	assert.Len(t, h.BD.Closed, 1)
	assert.Len(t, h.Rec.merges(), 1)
}

// P6-10 + P6-14: an escalated gate parks the bead and, with the default
// policy, the gate is resolved so it stops blocking the bead's dependents.
func TestGatePollerEscalatedGateBlocksTheBeadAndResolvesTheGate(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	h.BD.AddGate("fx-8za", "fx-6z4", 2)
	h.BD.AddBead("fx-6z4", domain.BeadStateDelivering, 2)
	h.BD.Answer(gateCheck(t, gateOutClosedPR, ""))

	res := h.poll()

	assert.Equal(t, []domain.BeadID{"fx-6z4"}, res.Blocked)
	assert.Equal(t, []domain.BeadID{"fx-8za"}, res.Resolved, "P6-14: the escalated gate is resolved")
	assert.Empty(t, res.Closed)
	assert.Equal(t, []string{"fx-6z4=needs_human"}, h.BD.Statuses)
	assert.Equal(t, string(domain.BeadStateNeedsHuman), h.BD.State("fx-6z4"))
	assert.Equal(t, []string{"fx-6z4 delivering→needs_human"}, h.Rec.transitions())
	assert.Empty(t, h.BD.Closed, "an abandoned PR never closes a bead")

	require.Len(t, h.BD.Resolved, 1)
	assert.Contains(t, h.BD.Resolved[0], "fx-8za: pull request #2 was closed without merging")
	assert.Contains(t, h.BD.Resolved[0], "PR 'Fix Max' was closed without merging", "bd's own detail is kept")

	warns := h.Rec.warnings()
	require.Len(t, warns, 1, "one Warning: a human has to look at the bead")
	assert.Equal(t, SourceGates, warns[0].Source)
	assert.Equal(t, domain.BeadID("fx-6z4"), warns[0].Bead)
	assert.Contains(t, warns[0].Message, "needs a human")
	assert.Equal(t, 1, h.P.Stats().Blocked)
	assert.Equal(t, 1, h.P.Stats().Resolved)
}

// P6-14, the other half of the choice: EscalatedGateKeep leaves the gate
// for a human, and says so once.
func TestGatePollerEscalatedGateCanBeKept(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, func(o *GatePollerOptions) { o.Escalated = EscalatedGateKeep })
	h.BD.AddGate("fx-8za", "fx-6z4", 2)
	h.BD.AddBead("fx-6z4", domain.BeadStateDelivering, 2)
	h.BD.Answer(gateCheck(t, gateOutClosedPR, ""))

	res := h.poll()

	assert.Equal(t, []domain.BeadID{"fx-6z4"}, res.Blocked)
	assert.Empty(t, res.Resolved)
	assert.Empty(t, h.BD.Resolved, "the gate is left open on purpose")
	assert.Equal(t, string(domain.BeadStateNeedsHuman), h.BD.State("fx-6z4"))

	var kept int
	for _, w := range h.Rec.warnings() {
		if strings.Contains(w.Message, "stays open") {
			kept++
		}
	}
	assert.Equal(t, 1, kept, "the kept gate is reported once")

	// bd keeps reporting the same escalation; the bead is already parked
	// and nothing is warned about twice.
	h.BD.Answer(gateCheck(t, gateOutClosedPR, ""))
	again := h.poll()
	assert.True(t, again.Empty())
	assert.Len(t, h.BD.Statuses, 1, "the bead is not moved to needs_human twice")
	assert.Len(t, h.Rec.warnings(), 2, "and no warning repeats")
}

// P6-14, the three ways an escalation can fail. None of them is the D-20
// hole a merge would be, because bd leaves an escalated gate open and
// reports it again on the very next poll — but each has to leave the gate
// alone until the bead really is parked, or the pull request's own record
// of the problem would be cleared with nothing to show for it.
func TestGatePollerEscalationRecoversFromAFailedStep(t *testing.T) {
	t.Parallel()

	t.Run("the gate will not read", func(t *testing.T) {
		t.Parallel()
		h := newGateHarness(t, nil)
		h.BD.AddGate("fx-8za", "fx-6z4", 2)
		h.BD.AddBead("fx-6z4", domain.BeadStateDelivering, 2)
		h.BD.fail("show-gate", errors.New("bd: dolt: database is locked"))
		h.BD.Answer(gateCheck(t, gateOutClosedPR, ""))

		res := h.poll()
		assert.True(t, res.Empty())
		assert.Empty(t, h.BD.Resolved, "a gate that would not read is not resolved either")
		assert.Empty(t, h.BD.Statuses)
		assert.Empty(t, res.PendingGates, "an escalation is not remembered: bd reports it again")
		require.Len(t, h.Rec.warnings(), 1)
		assert.Contains(t, h.Rec.warnings()[0].Message, "could not read gate fx-8za")

		// bd leaves an escalated gate open and reports it again; this time
		// the read works, and nothing about the escalation was lost.
		h.BD.fail("show-gate", nil)
		again := h.poll()
		require.Len(t, again.Check.Results, 1, "bd keeps reporting an open escalated gate")
		assert.Equal(t, []domain.BeadID{"fx-6z4"}, again.Blocked)
		assert.Equal(t, []domain.BeadID{"fx-8za"}, again.Resolved)
		assert.Equal(t, string(domain.BeadStateNeedsHuman), h.BD.State("fx-6z4"))
	})

	t.Run("the bead is not the poller's", func(t *testing.T) {
		t.Parallel()
		h := newGateHarness(t, func(o *GatePollerOptions) {
			o.Statuses = beads.StatusMap{"reviewing": domain.CategoryWIP}
		})
		h.BD.AddGate("fx-8za", "fx-6z4", 2)
		h.BD.AddBead("fx-6z4", domain.BeadState("reviewing"), 2)
		h.BD.Answer(gateCheck(t, gateOutClosedPR, ""))

		res := h.poll()
		assert.True(t, res.Empty())
		assert.Empty(t, h.BD.Statuses, "a foreign bead is never transitioned")
		assert.Empty(t, h.BD.Resolved, "and its gate is not the poller's to clear")
		require.Len(t, h.Rec.warnings(), 1)
		assert.Contains(t, h.Rec.warnings()[0].Message, `carries the status "reviewing"`)
		assert.Equal(t, "open", h.BD.gate("fx-8za").Status)
	})

	t.Run("the bead belongs to another run", func(t *testing.T) {
		t.Parallel()
		h := newGateHarness(t, nil)
		h.BD.AddGate("fx-8za", "fx-6z4", 2)
		h.BD.AddBead("fx-6z4", domain.BeadStateDelivering, 2, "sc:run:someone-else")
		h.BD.Answer(gateCheck(t, gateOutClosedPR, ""))

		res := h.poll()
		assert.True(t, res.Empty())
		assert.Empty(t, h.BD.Statuses)
		assert.Empty(t, h.BD.Resolved, "another run's gate stays open for that run to deal with")
		assert.Empty(t, h.Rec.all(), "and it is not this run's warning either")
	})

	t.Run("the bead will not move", func(t *testing.T) {
		t.Parallel()
		h := newGateHarness(t, nil)
		h.BD.AddGate("fx-8za", "fx-6z4", 2)
		h.BD.AddBead("fx-6z4", domain.BeadStateDelivering, 2)
		h.BD.fail("status", errors.New("bd: dolt: database is locked"))
		h.BD.Answer(gateCheck(t, gateOutClosedPR, ""))

		res := h.poll()
		assert.Empty(t, res.Blocked, "the bead is still delivering")
		assert.Empty(t, res.Resolved)
		assert.Empty(t, h.BD.Resolved,
			"the gate is the only remaining sign the pull request was abandoned; it is not cleared for an unparked bead")
		assert.Equal(t, "open", h.BD.gate("fx-8za").Status)
		assert.Equal(t, string(domain.BeadStateDelivering), h.BD.State("fx-6z4"))
		require.Len(t, h.Rec.warnings(), 1)
		assert.Contains(t, h.Rec.warnings()[0].Message, "could not move fx-6z4")

		// bd escalates the still-open gate again; when the write works the
		// bead is parked and only then is the gate cleared.
		h.BD.fail("status", nil)
		again := h.poll()
		assert.Equal(t, []domain.BeadID{"fx-6z4"}, again.Blocked)
		assert.Equal(t, []domain.BeadID{"fx-8za"}, again.Resolved)
		assert.Equal(t, string(domain.BeadStateNeedsHuman), h.BD.State("fx-6z4"))
		assert.Equal(t, "closed", h.BD.gate("fx-8za").Status)
	})
}

// A pull request that was reopened and merged after the gate escalated
// still closes its bead.
func TestGatePollerMergeAfterAnEscalationRevivesTheBead(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	h.BD.AddGate("fx-qba", "fx-byi", 1)
	h.BD.AddBead("fx-byi", domain.BeadStateNeedsHuman, 1)
	h.BD.Answer(gateCheck(t, gateOutMerged, ""))

	res := h.poll()

	assert.Equal(t, []domain.BeadID{"fx-byi"}, res.Closed)
	assert.Equal(t, []string{"fx-byi=delivering"}, h.BD.Statuses, "the bead goes back in flight before it closes")
	assert.Equal(t, []string{"fx-byi: pull request #1 merged"}, h.BD.Closed)
	assert.Equal(t, []string{"fx-byi needs_human→delivering", "fx-byi delivering→closed"}, h.Rec.transitions())
}

// A resolved gate on a bead that is neither delivering nor needs_human is
// reported, not forced: something else owns that bead.
func TestGatePollerLeavesABeadThatIsNotDelivering(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	h.BD.AddGate("fx-qba", "fx-byi", 1)
	h.BD.AddBead("fx-byi", domain.BeadStateInProgress, 1)
	h.BD.Answer(gateCheck(t, gateOutMerged, ""), gateCheck(t, gateOutMerged, ""))

	res := h.poll()
	assert.True(t, res.Empty())
	assert.Empty(t, h.BD.Closed)
	require.Len(t, h.Rec.warnings(), 1)
	assert.Contains(t, h.Rec.warnings()[0].Message, "is in_progress, not delivering")
	assert.Equal(t, []domain.BeadID{"fx-byi"}, res.Pending, "the merge is not forgotten either")

	h.poll()
	assert.Len(t, h.Rec.warnings(), 1, "the same complaint is not repeated every minute")

	// The gate is gone from bd, so the poller's own record of the merge is
	// all there is: when the bead comes back to delivering it closes.
	h.BD.AddBead("fx-byi", domain.BeadStateDelivering, 1)
	third := h.poll()
	assert.Equal(t, []domain.BeadID{"fx-byi"}, third.Closed)
	assert.Equal(t, []string{"fx-byi: pull request #1 merged"}, h.BD.Closed)
	assert.Empty(t, third.Pending)
}

// A pending gate is the normal case: no writes, no events, no noise.
func TestGatePollerPendingGateChangesNothing(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	h.BD.AddGate("fx-qba", "fx-byi", 1)
	h.BD.AddBead("fx-byi", domain.BeadStateDelivering, 1)
	h.BD.Answer(gateCheck(t, gateOutPending, ""))

	res := h.poll()

	assert.True(t, res.Empty())
	assert.Equal(t, 1, res.Check.Checked)
	assert.Empty(t, h.Rec.all(), "a pending gate publishes nothing at all")
	assert.Equal(t, string(domain.BeadStateDelivering), h.BD.State("fx-byi"))
}

// "No open gates" is not an error and not a warning.
func TestGatePollerWithNoGates(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	h.BD.Answer(gateCheck(t, gateOutNone, ""))

	res := h.poll()

	assert.True(t, res.Empty())
	assert.Zero(t, res.Check.Checked)
	assert.Empty(t, h.Rec.all())
}

// P6-10: gh missing means one Warning for the outage, not one per gate and
// not one per poll.
func TestGatePollerSkipsWithOneWarningWhenGHIsMissing(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	h.BD.AddGate("fx-qba", "fx-byi", 1)
	h.BD.AddBead("fx-byi", domain.BeadStateDelivering, 1)
	h.BD.Answer(gateCheck(t, gateOutErrors, gateErrMissingGH))

	for i := 0; i < 3; i++ {
		res := h.poll()
		assert.True(t, res.Skipped, "poll %d is skipped", i)
		assert.Contains(t, res.Reason, "gh CLI not installed")
	}

	warns := h.Rec.warnings()
	require.Len(t, warns, 1, "one Warning for the outage, however long it lasts")
	assert.Equal(t, SourceGates, warns[0].Source)
	assert.Contains(t, warns[0].Message, "gh is unavailable")
	assert.Contains(t, warns[0].Err, "gh CLI not installed")
	assert.Empty(t, warns[0].Bead, "the outage is not one bead's problem")
	assert.Empty(t, h.BD.Closed)
	assert.Empty(t, h.BD.Statuses)

	stats := h.P.Stats()
	assert.Equal(t, 3, stats.Polls)
	assert.Equal(t, 3, stats.Skipped)
	assert.True(t, stats.GHDown)

	// gh comes back, the merge is picked up, and a later outage warns again.
	h.BD.Answer(gateCheck(t, gateOutMerged, ""), gateCheck(t, gateOutErrors, gateErrMissingGH))
	assert.Equal(t, []domain.BeadID{"fx-byi"}, h.poll().Closed)
	assert.False(t, h.P.Stats().GHDown)
	assert.True(t, h.poll().Skipped)
	assert.Len(t, h.Rec.warnings(), 2, "a new outage is worth a new Warning")
}

// An unauthenticated gh is the same outage, recognised from bd's two-line
// message.
func TestGatePollerSkipsWhenGHIsUnauthenticated(t *testing.T) {
	t.Parallel()
	unauth := "✗ fx-qba: error checking - gh pr view failed: To get started with GitHub CLI, please run:  gh auth login\n" +
		"Alternatively, populate the GH_TOKEN environment variable with a GitHub API authentication token.\n"
	h := newGateHarness(t, nil)
	h.BD.Answer(gateCheck(t, gateOutErrors, unauth))

	res := h.poll()

	assert.True(t, res.Skipped)
	assert.Contains(t, res.Reason, "gh auth login")
	require.Len(t, h.Rec.warnings(), 1)
	assert.Contains(t, h.Rec.warnings()[0].Message, "gh is unavailable")
}

// A network failure is not gh being unavailable: it is one gate's problem,
// reported once and retried on the next poll.
func TestGatePollerReportsATransientCheckFailureOnce(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	h.BD.AddGate("fx-qba", "fx-byi", 1)
	h.BD.AddBead("fx-byi", domain.BeadStateDelivering, 1)
	h.BD.Answer(gateCheck(t, gateOutErrors, gateErrNetwork))

	first := h.poll()
	assert.False(t, first.Skipped, "one failing gate is not a gh outage")
	assert.True(t, first.Empty())
	require.Len(t, h.Rec.warnings(), 1)
	assert.Contains(t, h.Rec.warnings()[0].Message, "api.github.com")

	h.poll()
	assert.Len(t, h.Rec.warnings(), 1, "the same failure is not warned about every minute")

	// The next check succeeds and the bead closes.
	h.BD.Answer(gateCheck(t, gateOutMerged, ""))
	assert.Equal(t, []domain.BeadID{"fx-byi"}, h.poll().Closed)
	assert.False(t, h.P.Stats().GHDown)
}

// bd itself failing is returned to the caller, warned about once, and the
// loop keeps going.
func TestGatePollerSurfacesABDFailure(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	boom := errors.New("bd: exit status 1")
	h.BD.mu.Lock()
	h.BD.checkErr = boom
	h.BD.mu.Unlock()

	_, err := h.P.Poll(context.Background())
	require.ErrorIs(t, err, boom)
	_, err = h.P.Poll(context.Background())
	require.ErrorIs(t, err, boom)

	require.Len(t, h.Rec.warnings(), 1)
	assert.Contains(t, h.Rec.warnings()[0].Message, "could not check the pull request gates")
	stats := h.P.Stats()
	assert.Equal(t, 2, stats.Polls)
	assert.Equal(t, 2, stats.Failures)
}

// A gate the poller cannot read, and a gate that names no bead, are
// reported and skipped rather than crashing the loop.
func TestGatePollerHandlesUnreadableGates(t *testing.T) {
	t.Parallel()
	t.Run("gate show fails", func(t *testing.T) {
		t.Parallel()
		h := newGateHarness(t, nil)
		h.BD.Answer(gateCheck(t, gateOutMerged, ""))
		res := h.poll()
		assert.True(t, res.Empty())
		require.Len(t, h.Rec.warnings(), 1)
		assert.Contains(t, h.Rec.warnings()[0].Message, "could not read gate fx-qba")
		// The merge itself is not lost with the read: bd will never
		// mention this gate again, so the poller keeps the verdict by the
		// gate's id and reads the gate again on the next poll.
		assert.Equal(t, []domain.BeadID{"fx-qba"}, res.PendingGates)
		assert.Empty(t, res.Pending, "the bead is not known until the gate is read")
		assert.Equal(t, 1, h.P.Stats().Pending)
	})

	t.Run("gate blocks nothing", func(t *testing.T) {
		t.Parallel()
		h := newGateHarness(t, nil)
		h.BD.mu.Lock()
		h.BD.gates["fx-qba"] = beads.Gate{ID: "fx-qba", Status: "open", AwaitType: beads.GateTypePR, AwaitID: "1"}
		h.BD.mu.Unlock()
		h.BD.Answer(gateCheck(t, gateOutMerged, ""))
		res := h.poll()
		assert.True(t, res.Empty())
		require.Len(t, h.Rec.warnings(), 1)
		assert.Contains(t, h.Rec.warnings()[0].Message, "names no bead")
	})

	t.Run("bead show fails", func(t *testing.T) {
		t.Parallel()
		h := newGateHarness(t, nil)
		h.BD.AddGate("fx-qba", "fx-byi", 1)
		h.BD.Answer(gateCheck(t, gateOutMerged, ""))
		res := h.poll()
		assert.True(t, res.Empty())
		require.Len(t, h.Rec.warnings(), 1)
		assert.Contains(t, h.Rec.warnings()[0].Message, "could not read fx-byi")
		assert.Equal(t, []domain.BeadID{"fx-byi"}, res.Pending, "the merge waits for the bead read")
		assert.Equal(t, []domain.BeadID{"fx-qba"}, res.PendingGates)
	})
}

// The reviewer's second live repro, and the D-20 hole it found: `bd gate
// check` closes a gate as it resolves it, so the merge verdict for a gate
// arrives exactly once and the very next check reports no open gates. A
// `bd gate show` that fails on that one poll — a Dolt lock, a slow
// machine, any transient bd failure — used to lose the merge for ever,
// leaving the bead `delivering`, its dependents out of `bd ready`, a brief
// run unable to finish and an autopilot run idling for ever. The poller
// now remembers the merge by the only thing bd's verdict carries, the gate
// id, and reads the gate again on the next poll.
func TestGatePollerRetriesAGateReadThatFailed(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	h.BD.AddGate("fx-qba", "fx-byi", 1)
	h.BD.AddBead("fx-byi", domain.BeadStateDelivering, 1)
	h.BD.fail("show-gate", errors.New("bd: dolt: database is locked"))
	h.BD.Answer(gateCheck(t, gateOutMerged, ""))

	first := h.poll()
	require.True(t, first.Empty(), "nothing could be done with a gate that would not read")
	assert.Equal(t, []domain.BeadID{"fx-qba"}, first.PendingGates, "but the merge is remembered")
	assert.Empty(t, first.Pending, "by the gate id: the bead is not known yet")
	assert.Equal(t, string(domain.BeadStateDelivering), h.BD.State("fx-byi"))
	require.Len(t, h.Rec.warnings(), 1)
	assert.Contains(t, h.Rec.warnings()[0].Message, "could not read gate fx-qba")

	// bd never mentions the gate again — it closed it as it resolved it.
	second := h.poll()
	require.Empty(t, second.Check.Results, "bd hands out a merge verdict exactly once")
	assert.Equal(t, []domain.BeadID{"fx-qba"}, second.PendingGates, "so the retry has to be the poller's own")
	assert.Len(t, h.Rec.warnings(), 1, "and a read that keeps failing is still one Warning")

	// The lock clears; the next poll reads the gate and closes the bead,
	// with bd's gate check still saying nothing at all.
	h.BD.fail("show-gate", nil)
	third := h.poll()
	assert.Equal(t, []domain.BeadID{"fx-byi"}, third.Closed)
	assert.Empty(t, third.Pending)
	assert.Empty(t, third.PendingGates)
	assert.Empty(t, third.Check.Results)
	assert.Equal(t, string(domain.BeadStateClosed), h.BD.State("fx-byi"))
	assert.Equal(t, []string{"fx-byi: pull request #1 merged"}, h.BD.Closed)
	require.Len(t, h.Rec.merges(), 1, "exactly one Merged, published when the bead really closed")
	assert.Equal(t, 1, h.Rec.merges()[0].PRNumber)

	stats := h.P.Stats()
	assert.Equal(t, 1, stats.Closed)
	assert.Zero(t, stats.Pending)
	assert.Equal(t, 2, stats.Retries)
	cache, pending, _ := gateMaps(h.P)
	assert.Zero(t, cache)
	assert.Zero(t, pending, "and nothing is left behind")
}

// The same recovery through a gh outage: the gate read is bd's business,
// not gh's, so a merge whose gate could not be read is still finished while
// polling itself is skipped.
func TestGatePollerRetriesAnUnreadGateWhileGHIsDown(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	h.BD.AddGate("fx-qba", "fx-byi", 1)
	h.BD.AddBead("fx-byi", domain.BeadStateDelivering, 1)
	h.BD.fail("show-gate", errors.New("bd: dolt: database is locked"))
	h.BD.Answer(gateCheck(t, gateOutMerged, ""), gateCheck(t, gateOutErrors, gateErrMissingGH))

	require.Equal(t, []domain.BeadID{"fx-qba"}, h.poll().PendingGates)
	h.BD.fail("show-gate", nil)

	res := h.poll()
	assert.True(t, res.Skipped, "gh is gone")
	assert.Equal(t, []domain.BeadID{"fx-byi"}, res.Closed, "the merge that already happened still lands")
	assert.Empty(t, res.PendingGates)
	assert.Equal(t, string(domain.BeadStateClosed), h.BD.State("fx-byi"))
}

// A gate that names no bead it blocks is a different thing from one that
// would not read: reading it again would only reach the same answer, so the
// merge is dropped after one Warning rather than retried for ever.
func TestGatePollerForgetsAMergedGateThatNamesNoBead(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	h.BD.mu.Lock()
	h.BD.gates["fx-qba"] = beads.Gate{ID: "fx-qba", Status: "open", AwaitType: beads.GateTypePR, AwaitID: "1"}
	h.BD.mu.Unlock()
	h.BD.Answer(gateCheck(t, gateOutMerged, ""))

	res := h.poll()
	assert.True(t, res.Empty())
	assert.Empty(t, res.PendingGates, "there is no bead to come back to")
	require.Len(t, h.Rec.warnings(), 1)
	assert.Contains(t, h.Rec.warnings()[0].Message, "names no bead")

	h.poll()
	assert.Zero(t, h.P.Stats().Pending)
	assert.Len(t, h.Rec.warnings(), 1, "and it is not complained about every minute")
	_, pending, _ := gateMaps(h.P)
	assert.Zero(t, pending)

	// The same answer ends a merge that was remembered because the gate
	// would not read: once it does read and names nobody, it is dropped
	// rather than retried until the run ends.
	h2 := newGateHarness(t, nil)
	h2.BD.fail("show-gate", errors.New("bd: dolt: database is locked"))
	h2.BD.Answer(gateCheck(t, gateOutMerged, ""))
	require.Equal(t, []domain.BeadID{"fx-qba"}, h2.poll().PendingGates)

	h2.BD.mu.Lock()
	h2.BD.gates["fx-qba"] = beads.Gate{ID: "fx-qba", Status: "open", AwaitType: beads.GateTypePR, AwaitID: "1"}
	h2.BD.mu.Unlock()
	h2.BD.fail("show-gate", nil)

	res = h2.poll()
	assert.True(t, res.Empty())
	assert.Empty(t, res.PendingGates, "a gate that blocks nothing is forgotten on the retry too")
	assert.Zero(t, h2.P.Stats().Pending)
}

// P3-14/P6-12: a bead carrying the repository's own status, or one of bd's
// read-only built-ins, is never transitioned — and P6-28: the user is told
// so once, by a Warning a reader finds by its Source, and the bead is
// counted so `sc status` can say how many the run is leaving alone.
func TestGatePollerRefusesForeignAndNonWorkflowBeads(t *testing.T) {
	t.Parallel()
	t.Run("foreign status", func(t *testing.T) {
		t.Parallel()
		h := newGateHarness(t, func(o *GatePollerOptions) {
			o.Statuses = beads.StatusMap{"reviewing": domain.CategoryWIP}
		})
		h.BD.AddGate("fx-qba", "fx-byi", 1)
		h.BD.AddBead("fx-byi", domain.BeadState("reviewing"), 1)
		h.BD.Answer(gateCheck(t, gateOutMerged, ""), gateCheck(t, gateOutMerged, ""))

		res := h.poll()
		assert.True(t, res.Empty())
		assert.Empty(t, h.BD.Closed)
		require.Len(t, h.Rec.warnings(), 1)
		assert.Equal(t, SourceForeignStatus, h.Rec.warnings()[0].Source,
			"P6-28: the report is found by its Source, not by its wording")
		assert.Equal(t, domain.BeadID("fx-byi"), h.Rec.warnings()[0].Bead)
		assert.Contains(t, h.Rec.warnings()[0].Message, `carries the status "reviewing"`)
		assert.Equal(t, 1, h.P.Stats().Foreign)

		// bd reports the same merge again: the bead is still not the
		// poller's, and saying so twice would be noise.
		assert.True(t, h.poll().Empty())
		assert.Len(t, h.Rec.warnings(), 1, "one Warning per bead, not one per poll")
		assert.Equal(t, 1, h.P.Stats().Foreign, "and the count is of beads, not of polls")
	})

	t.Run("bd built-in", func(t *testing.T) {
		t.Parallel()
		h := newGateHarness(t, nil)
		h.BD.AddGate("fx-qba", "fx-byi", 1)
		h.BD.AddBead("fx-byi", domain.BeadStateDeferred, 1)
		h.BD.Answer(gateCheck(t, gateOutMerged, ""))

		res := h.poll()
		assert.True(t, res.Empty())
		assert.Empty(t, h.BD.Closed)
		require.Len(t, h.Rec.warnings(), 1)
		assert.Equal(t, SourceForeignStatus, h.Rec.warnings()[0].Source)
		assert.Contains(t, h.Rec.warnings()[0].Message, "which slop-cannon only reads")
		assert.Equal(t, 1, h.P.Stats().Foreign)
	})
}

// P6-28: the reporter is the run's, not the poller's, so a bead the
// scheduler has already reported is not reported a second time when the
// gate poller meets it. The poller still counts what it passed over.
func TestGatePollerSharesTheRunsForeignStatusReporter(t *testing.T) {
	t.Parallel()
	rec := &gateRecorder{}
	shared := NewForeignStatusReporter(ForeignStatusOptions{
		Sink:     rec,
		Run:      gateRun,
		Statuses: beads.StatusMap{"reviewing": domain.CategoryWIP},
		Clock:    func() time.Time { return gateEpoch },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	// Somebody else met the bead first.
	_, first, err := shared.Resolve("fx-byi", "reviewing")
	require.Error(t, err)
	require.True(t, first)

	h := newGateHarness(t, func(o *GatePollerOptions) {
		o.Foreign = shared
		o.Statuses = beads.StatusMap{"reviewing": domain.CategoryWIP}
	})
	h.BD.AddGate("fx-qba", "fx-byi", 1)
	h.BD.AddBead("fx-byi", domain.BeadState("reviewing"), 1)
	h.BD.Answer(gateCheck(t, gateOutMerged, ""))

	assert.True(t, h.poll().Empty())
	assert.Empty(t, h.Rec.warnings(), "the run already told the user about this bead")
	assert.Len(t, rec.warnings(), 1)
	assert.Equal(t, 1, shared.Count())
	assert.Zero(t, h.P.Stats().Foreign, "the poller counts what it was the first to meet")
}

// P6-29: a reader must be able to say how long `gh` has been away without
// following the run log back to the Warning.
func TestGatePollerRecordsWhenGHWentDown(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	h.BD.AddGate("fx-qba", "fx-byi", 1)
	h.BD.AddBead("fx-byi", domain.BeadStateDelivering, 1)
	h.BD.Answer(gateCheck(t, gateOutErrors, gateErrMissingGH))

	h.poll()
	down := h.P.Stats()
	require.True(t, down.GHDown)
	assert.Equal(t, gateEpoch, down.GHDownSince)

	h.Clock.Advance(90 * time.Second)
	h.poll()
	assert.Equal(t, gateEpoch, h.P.Stats().GHDownSince, "the outage started when it started")

	// gh comes back and the record of the outage goes with it.
	h.BD.Answer(gateCheck(t, gateOutMerged, ""))
	h.poll()
	back := h.P.Stats()
	assert.False(t, back.GHDown)
	assert.True(t, back.GHDownSince.IsZero())

	// A second outage is timed from when it started, not from the first.
	h.Clock.Advance(time.Minute)
	h.BD.Answer(gateCheck(t, gateOutErrors, gateErrMissingGH))
	h.poll()
	assert.Equal(t, gateEpoch.Add(150*time.Second), h.P.Stats().GHDownSince)
}

// A gate on a bead of another run (or of no run at all) is not this run's
// business, and is passed over in silence.
func TestGatePollerIgnoresBeadsOutsideTheRun(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	h.BD.AddGate("fx-qba", "fx-byi", 1)
	h.BD.AddBead("fx-byi", domain.BeadStateDelivering, 1, "sc:run:someone-else")
	h.BD.Answer(gateCheck(t, gateOutMerged, ""))

	res := h.poll()

	assert.True(t, res.Empty())
	assert.Empty(t, h.BD.Closed)
	assert.Empty(t, h.Rec.all(), "another tool's gate is not a warning")

	// Without a scope the same gate is acted on.
	wide := newGateHarness(t, func(o *GatePollerOptions) { o.Scope = "" })
	wide.BD.AddGate("fx-qba", "fx-byi", 1)
	wide.BD.AddBead("fx-byi", domain.BeadStateDelivering, 1, "sc:run:someone-else")
	wide.BD.Answer(gateCheck(t, gateOutMerged, ""))
	assert.Equal(t, []domain.BeadID{"fx-byi"}, wide.poll().Closed)
}

// Without a bead reader the poller assumes the bead is delivering, which is
// where a gated bead sits by construction (D-20). Dropping the reader means
// dropping the scope with it: the scope lives on the bead.
func TestGatePollerWithoutABeadReaderAssumesDelivering(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, func(o *GatePollerOptions) { o.Beads, o.Scope = nil, "" })
	h.BD.AddGate("fx-qba", "fx-byi", 1)
	h.BD.Answer(gateCheck(t, gateOutMerged, ""))

	res := h.poll()

	assert.Equal(t, []domain.BeadID{"fx-byi"}, res.Closed)
	assert.Equal(t, []string{"fx-byi: pull request #1 merged"}, h.BD.Closed)
	require.Len(t, h.Rec.merges(), 1)
	assert.Equal(t, 1, h.Rec.merges()[0].PRNumber, "the PR number comes from the gate's --await-id")
}

// A bd write that fails leaves the bead where it was, with one Warning and
// no Merged event claiming work landed — and the bead on the poller's own
// list of merges still to finish.
func TestGatePollerSurfacesAFailedClose(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	h.BD.AddGate("fx-qba", "fx-byi", 1)
	h.BD.AddBead("fx-byi", domain.BeadStateDelivering, 1)
	h.BD.mu.Lock()
	h.BD.closeErr = fmt.Errorf("beads: close: %w", beads.ErrBlockedOpen)
	h.BD.mu.Unlock()
	h.BD.Answer(gateCheck(t, gateOutMerged, ""))

	res := h.poll()

	assert.True(t, res.Empty())
	assert.Empty(t, h.Rec.merges(), "nothing merged, so nothing is announced")
	require.Len(t, h.Rec.warnings(), 1)
	assert.Contains(t, h.Rec.warnings()[0].Message, "could not move fx-byi")
	assert.Equal(t, string(domain.BeadStateDelivering), h.BD.State("fx-byi"))
	assert.Equal(t, []domain.BeadID{"fx-byi"}, res.Pending, "and it is not forgotten")
	assert.Equal(t, 1, h.P.Stats().Pending)
}

// The reviewer's live repro, in one test: bd hands out the merge verdict
// for a gate exactly once (`bd gate check` closes the gate as it resolves
// it, and the next check reports no open gates), so a refused `bd close` —
// here a blocker added to the bead while the pull request was in review —
// would strand the bead in `delivering` for the life of the run, its
// dependents out of `bd ready` for ever and the run unable to finish
// (D-18, D-20). The poller remembers the merge and closes the bead itself
// on a later poll, without bd ever mentioning the gate again.
func TestGatePollerRetriesACloseBDRefused(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	h.BD.AddGate("fx-qba", "fx-byi", 1)
	h.BD.AddBead("fx-byi", domain.BeadStateDelivering, 1)
	h.BD.mu.Lock()
	h.BD.closeErr = fmt.Errorf("beads: close: %w: cannot close fx-byi: blocked by open issues [fx-3k0]", beads.ErrBlockedOpen)
	h.BD.mu.Unlock()
	h.BD.Answer(gateCheck(t, gateOutMerged, ""))

	first := h.poll()
	require.Equal(t, []domain.BeadID{"fx-byi"}, first.Pending)
	assert.Equal(t, string(domain.BeadStateDelivering), h.BD.State("fx-byi"))

	second := h.poll()
	require.Empty(t, second.Check.Results, "bd reports a resolved gate exactly once")
	assert.Equal(t, []domain.BeadID{"fx-byi"}, second.Pending, "so the retry has to be the poller's own")
	assert.Len(t, h.Rec.warnings(), 1, "and a refusal that repeats is still one Warning")
	assert.Equal(t, 1, h.P.Stats().Pending)

	// The blocker is closed by hand; the next poll closes the bead.
	h.BD.mu.Lock()
	h.BD.closeErr = nil
	h.BD.mu.Unlock()

	third := h.poll()
	assert.Equal(t, []domain.BeadID{"fx-byi"}, third.Closed)
	assert.Empty(t, third.Pending)
	assert.Empty(t, third.Check.Results, "closed without a word from bd's gate check")
	assert.Equal(t, string(domain.BeadStateClosed), h.BD.State("fx-byi"))
	assert.Equal(t, []string{"fx-byi: pull request #1 merged"}, h.BD.Closed)
	assert.Equal(t, []string{"fx-byi delivering→closed"}, h.Rec.transitions())
	require.Len(t, h.Rec.merges(), 1, "exactly one Merged, published when the bead really closed")
	assert.Equal(t, 1, h.Rec.merges()[0].PRNumber)

	stats := h.P.Stats()
	assert.Equal(t, 1, stats.Closed)
	assert.Zero(t, stats.Pending)
	assert.Equal(t, 2, stats.Retries)

	// Nothing is retried once there is nothing left to close.
	h.poll()
	assert.Equal(t, 2, h.P.Stats().Retries)
	assert.Len(t, h.BD.Closed, 1)
	cache, pending, _ := gateMaps(h.P)
	assert.Zero(t, cache, "and the gate is out of the cache")
	assert.Zero(t, pending)
}

// gh being down skips the gate check, but a close that failed earlier is
// bd's business and not gh's: it is retried anyway, so an outage cannot
// make a stranded bead permanent.
func TestGatePollerRetriesAPendingCloseWhileGHIsDown(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	h.BD.AddGate("fx-qba", "fx-byi", 1)
	h.BD.AddBead("fx-byi", domain.BeadStateDelivering, 1)
	h.BD.mu.Lock()
	h.BD.closeErr = errors.New("bd: dolt: database is locked")
	h.BD.mu.Unlock()
	h.BD.Answer(gateCheck(t, gateOutMerged, ""), gateCheck(t, gateOutErrors, gateErrMissingGH))

	require.Equal(t, []domain.BeadID{"fx-byi"}, h.poll().Pending)
	h.BD.mu.Lock()
	h.BD.closeErr = nil
	h.BD.mu.Unlock()

	res := h.poll()
	assert.True(t, res.Skipped, "gh is gone")
	assert.Equal(t, []domain.BeadID{"fx-byi"}, res.Closed, "the merge that already happened still lands")
	assert.Empty(t, res.Pending)
	assert.Equal(t, string(domain.BeadStateClosed), h.BD.State("fx-byi"))
}

// A remembered merge is dropped, silently and for good, when it turns out
// there is nothing left to do or the bead is not the poller's.
func TestGatePollerForgetsAPendingCloseItMustNotFinish(t *testing.T) {
	t.Parallel()

	t.Run("closed by someone else", func(t *testing.T) {
		t.Parallel()
		h := gateStrandedHarness(t)
		h.BD.AddBead("fx-byi", domain.BeadStateClosed, 1)

		res := h.poll()
		assert.Empty(t, res.Pending)
		assert.Empty(t, res.Closed)
		assert.Empty(t, h.BD.Closed, "a closed bead is not closed again")
		assert.Empty(t, h.Rec.merges(), "and nothing is announced twice")
		assert.Zero(t, h.P.Stats().Pending)
	})

	t.Run("relabelled into another run", func(t *testing.T) {
		t.Parallel()
		h := gateStrandedHarness(t)
		h.BD.AddBead("fx-byi", domain.BeadStateDelivering, 1, "sc:run:someone-else")

		res := h.poll()
		assert.Empty(t, res.Pending)
		assert.Empty(t, h.BD.Closed)
		assert.Len(t, h.Rec.warnings(), 1, "another run's bead is not a new complaint")
		assert.Zero(t, h.P.Stats().Pending)
	})
}

// gateStrandedHarness leaves the poller holding one merge it could not
// finish: gate fx-qba on bead fx-byi, whose close bd refused.
func gateStrandedHarness(t *testing.T) *gateHarness {
	t.Helper()
	h := newGateHarness(t, nil)
	h.BD.AddGate("fx-qba", "fx-byi", 1)
	h.BD.AddBead("fx-byi", domain.BeadStateDelivering, 1)
	h.BD.mu.Lock()
	h.BD.closeErr = fmt.Errorf("beads: close: %w", beads.ErrBlockedOpen)
	h.BD.mu.Unlock()
	h.BD.Answer(gateCheck(t, gateOutMerged, ""))
	require.Equal(t, []domain.BeadID{"fx-byi"}, h.poll().Pending)
	h.BD.mu.Lock()
	h.BD.closeErr = nil
	h.BD.Closed = nil
	h.BD.mu.Unlock()
	return h
}

// gateMaps reports how much bookkeeping the poller is holding on to: none
// of it may grow with a run that D-18 expects to last for days.
func gateMaps(p *GatePoller) (cache, pending, warned int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.cache), len(p.pending), len(p.warned)
}

// A gate the poller has finished with — closed by the merge, or resolved by
// the P6-14 policy — is dropped from the cache rather than kept for the
// life of the run.
func TestGatePollerForgetsAGateItHasFinishedWith(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	h.BD.AddGate("fx-qba", "fx-byi", 1)
	h.BD.AddGate("fx-8za", "fx-6z4", 2)
	h.BD.AddBead("fx-byi", domain.BeadStateDelivering, 1)
	h.BD.AddBead("fx-6z4", domain.BeadStateDelivering, 2)
	h.BD.Answer(gateCheck(t, gateOutBoth, ""))

	res := h.poll()
	require.Equal(t, []domain.BeadID{"fx-byi"}, res.Closed)
	require.Equal(t, []domain.BeadID{"fx-8za"}, res.Resolved)

	cache, pending, _ := gateMaps(h.P)
	assert.Zero(t, cache, "both gates are closed in bd, so neither is cached")
	assert.Zero(t, pending)
}

// The once-only warnings are keyed by the problem, never by the error text:
// a message that carries a lock path or a dialled port would otherwise warn
// on every poll and grow the bookkeeping with it. A success arms the same
// warning again, so a later outage is still news.
func TestGatePollerWarnsOncePerProblemNotPerErrorMessage(t *testing.T) {
	t.Parallel()

	t.Run("a close that keeps failing differently", func(t *testing.T) {
		t.Parallel()
		h := newGateHarness(t, nil)
		h.BD.AddGate("fx-qba", "fx-byi", 1)
		h.BD.AddBead("fx-byi", domain.BeadStateDelivering, 1)
		h.BD.Answer(gateCheck(t, gateOutMerged, ""))

		for i := 1; i <= 4; i++ {
			h.BD.mu.Lock()
			h.BD.closeErr = fmt.Errorf("bd: dolt: cannot lock /tmp/dolt-%d.lock", i)
			h.BD.mu.Unlock()
			h.poll()
		}

		assert.Len(t, h.Rec.warnings(), 1, "one problem, one Warning")
		_, pending, warned := gateMaps(h.P)
		assert.Equal(t, 1, pending)
		assert.Equal(t, 1, warned, "one key, whatever bd's message says")
	})

	t.Run("the bookkeeping is bounded", func(t *testing.T) {
		t.Parallel()
		h := newGateHarness(t, nil)
		for i := 0; i < 3*gateWarnKeyLimit; i++ {
			h.P.warnOnce(fmt.Sprintf("show-gate:fx-%d", i), "", "could not read a gate", nil)
			_, _, warned := gateMaps(h.P)
			require.LessOrEqual(t, warned, gateWarnKeyLimit,
				"a poller D-18 expects to run for days may not grow a map per problem")
		}
	})

	t.Run("a gate check that fails, works and fails again", func(t *testing.T) {
		t.Parallel()
		h := newGateHarness(t, nil)
		h.BD.Answer(gateCheck(t, gateOutNone, ""))
		for i := 1; i <= 3; i++ {
			h.BD.mu.Lock()
			h.BD.checkErr = fmt.Errorf("bd: exit status 1: connection refused on port %d", 5000+i)
			h.BD.mu.Unlock()
			_, err := h.P.Poll(context.Background())
			require.Error(t, err)
		}
		require.Len(t, h.Rec.warnings(), 1)
		_, _, warned := gateMaps(h.P)
		assert.Equal(t, 1, warned)

		h.BD.mu.Lock()
		h.BD.checkErr = nil
		h.BD.mu.Unlock()
		h.poll()
		_, _, warned = gateMaps(h.P)
		assert.Zero(t, warned, "a poll that works forgets the complaint")

		h.BD.mu.Lock()
		h.BD.checkErr = errors.New("bd: exit status 1: connection refused on port 6000")
		h.BD.mu.Unlock()
		_, err := h.P.Poll(context.Background())
		require.Error(t, err)
		assert.Len(t, h.Rec.warnings(), 2, "a new outage is news again")
	})
}

// bd printing counts but no verdict lines is worth exactly one complaint.
func TestGatePollerReportsMissingVerdictLines(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	h.BD.Answer(beads.GateCheck{Checked: 2, Resolved: 1})

	h.poll()
	h.poll()

	require.Len(t, h.Rec.warnings(), 1)
	assert.Contains(t, h.Rec.warnings()[0].Message, "no per-gate verdict")
}

// The poller never claims, reclaims or releases a bead: those belong to the
// scheduler (P6-13). The fake fails the test if it is asked to.
func TestGatePollerOnlyEverSetsStatusesAndCloses(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	h.BD.AddGate("fx-qba", "fx-byi", 1)
	h.BD.AddGate("fx-8za", "fx-6z4", 2)
	h.BD.AddBead("fx-byi", domain.BeadStateDelivering, 1)
	h.BD.AddBead("fx-6z4", domain.BeadStateDelivering, 2)
	h.BD.Answer(gateCheck(t, gateOutBoth, ""))

	res := h.poll()

	assert.Equal(t, []domain.BeadID{"fx-byi"}, res.Closed)
	assert.Equal(t, []domain.BeadID{"fx-6z4"}, res.Blocked)
	kinds := h.Rec.kinds()
	assert.Contains(t, kinds, events.KindMerged)
	assert.Contains(t, kinds, events.KindBeadStateChanged)
	assert.Contains(t, kinds, events.KindWarning)
}

// The Run loop polls at once, then every interval, and a Kick brings the
// next poll forward. Everything is driven by the fake clock.
func TestGatePollerRunLoopPollsOnTheInterval(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, func(o *GatePollerOptions) { o.Interval = 30 * time.Second })
	h.BD.Answer(gateCheck(t, gateOutPending, ""))
	require.Equal(t, 30*time.Second, h.P.Interval())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.P.Run(ctx) }()

	gateEventually(t, "the first poll", func() bool { return h.BD.polls() == 1 })
	gateEventually(t, "the loop to arm its timer", func() bool { return h.Clock.Waiting() == 1 })

	h.Clock.Advance(30 * time.Second)
	gateEventually(t, "the second poll", func() bool { return h.BD.polls() == 2 })

	gateEventually(t, "the loop to arm its timer again", func() bool { return h.Clock.Waiting() == 1 })
	h.P.Kick()
	gateEventually(t, "the kicked poll", func() bool { return h.BD.polls() == 3 })

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err, "cancellation ends the loop cleanly")
	case <-time.After(testutil.Patience(5 * time.Second)):
		t.Fatal("the gate poller did not stop")
	}
}

// A wake channel is the delivery handoff saying "a PR just opened".
func TestGatePollerRunLoopWakesOnDemand(t *testing.T) {
	t.Parallel()
	wake := make(chan struct{}, 1)
	h := newGateHarness(t, func(o *GatePollerOptions) { o.Wake = wake })
	h.BD.Answer(gateCheck(t, gateOutPending, ""))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- h.P.Run(ctx) }()

	gateEventually(t, "the first poll", func() bool { return h.BD.polls() == 1 })
	wake <- struct{}{}
	gateEventually(t, "the woken poll", func() bool { return h.BD.polls() == 2 })
	cancel()
	select {
	case <-done:
	case <-time.After(testutil.Patience(5 * time.Second)):
		t.Fatal("the gate poller did not stop")
	}
}

// Closing the wake channel wakes the loop one last time and is then
// ignored. A closed channel is ready for ever, so a loop that kept
// selecting on it would run `bd gate check` — and one `gh pr view` per open
// gate — as fast as bd could answer, for ever, with the interval never
// applying again.
func TestGatePollerRunLoopSurvivesAClosedWakeChannel(t *testing.T) {
	t.Parallel()
	wake := make(chan struct{})
	h := newGateHarness(t, func(o *GatePollerOptions) { o.Wake = wake; o.Interval = 30 * time.Second })
	h.BD.Answer(gateCheck(t, gateOutPending, ""))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- h.P.Run(ctx) }()

	gateEventually(t, "the first poll", func() bool { return h.BD.polls() == 1 })
	gateEventually(t, "the loop to arm its timer", func() bool { return h.Clock.Waiting() == 1 })

	close(wake)
	gateEventually(t, "the poll the close wakes", func() bool { return h.BD.polls() == 2 })
	// The timer the woken poll left behind is still armed, so a fresh one
	// makes two: the loop is waiting on the clock again, not on the channel.
	gateEventually(t, "the loop to arm its timer again", func() bool { return h.Clock.Waiting() >= 2 })

	// Nothing moves the clock, so nothing may poll. A spinning loop would
	// have run thousands of checks by the end of this window.
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 2, h.BD.polls(), "the closed channel is ignored, not spun on")

	h.Clock.Advance(30 * time.Second)
	gateEventually(t, "the interval to take over again", func() bool { return h.BD.polls() == 3 })

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(testutil.Patience(5 * time.Second)):
		t.Fatal("the gate poller did not stop")
	}
}

// Polls are serialised by a lock of their own, not merely by there being
// one caller today: two concurrent polls would double-count the retries,
// close a bead twice and publish two Merged events for one merge. The test
// stands in for a poll in flight by taking the poller's own poll lock.
func TestGatePollerSerialisesConcurrentPolls(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	h.BD.AddGate("fx-qba", "fx-byi", 1)
	h.BD.AddBead("fx-byi", domain.BeadStateDelivering, 1)
	h.BD.Answer(gateCheck(t, gateOutMerged, ""))

	h.P.pollMu.Lock()
	done := make(chan GatePollResult, 1)
	go func() {
		res, err := h.P.Poll(context.Background())
		assert.NoError(t, err)
		done <- res
	}()

	// Nothing may reach bd while a poll is in flight.
	select {
	case <-done:
		t.Fatal("a second poll ran beside the one in flight")
	case <-time.After(50 * time.Millisecond):
	}
	assert.Zero(t, h.BD.polls(), "not even `bd gate check`")

	h.P.pollMu.Unlock()
	select {
	case res := <-done:
		assert.Equal(t, []domain.BeadID{"fx-byi"}, res.Closed, "and it runs as soon as the lock frees")
	case <-time.After(testutil.Patience(5 * time.Second)):
		t.Fatal("the waiting poll never ran")
	}

	// Many callers at once still see exactly one merge announced.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.P.Poll(context.Background())
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	assert.Len(t, h.Rec.merges(), 1)
	assert.Equal(t, []string{"fx-byi: pull request #1 merged"}, h.BD.Closed)
	assert.Equal(t, 1, h.P.Stats().Closed)
}

// A cancelled context ends a poll without warning about the cancellation.
func TestGatePollerCancelledPollIsQuiet(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	h.BD.mu.Lock()
	h.BD.checkErr = context.Canceled
	h.BD.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := h.P.Poll(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, h.Rec.all(), "shutting down is not a warning")
	assert.NoError(t, h.P.Run(ctx))
}

// polls reports how many `bd gate check` calls the fake has served.
func (f *gateFakeBD) polls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Checks
}

// gateEventually polls cond until it holds, so a test never sleeps for a
// fixed time.
func gateEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// gateFakeHost is a run for the poller to publish and log through.
type gateFakeHost struct {
	rec *gateRecorder
	at  time.Time
}

func (h gateFakeHost) Meta() events.Meta       { return events.MetaAt(gateRun, h.at) }
func (h gateFakeHost) Publish(ev events.Event) { h.rec.Publish(ev) }
func (h gateFakeHost) Log() *slog.Logger       { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// With a host (the run, in production) every event goes through it and
// carries the run's own event header.
func TestGatePollerPublishesThroughItsHost(t *testing.T) {
	t.Parallel()
	rec := &gateRecorder{}
	host := gateFakeHost{rec: rec, at: gateEpoch.Add(90 * time.Second)}
	bd := newGateFakeBD()
	bd.AddGate("fx-qba", "fx-byi", 1)
	bd.AddBead("fx-byi", domain.BeadStateDelivering, 1)
	bd.Answer(gateCheck(t, gateOutMerged, ""))

	p, err := NewGatePoller(GatePollerOptions{
		Gates: bd, Beads: bd, Host: host, Run: gateRun, Branch: gateBranch, Clock: newGateClock(),
		States: NewStateMachine(bd, WithStatePublisher(rec), WithStateRun(gateRun)),
	})
	require.NoError(t, err)
	require.Equal(t, []domain.BeadID{"fx-byi"}, mustGatePoll(t, p).Closed)

	merges := rec.merges()
	require.Len(t, merges, 1)
	assert.Equal(t, host.at, merges[0].At(), "the host stamps the event, not the poller's clock")
	assert.Equal(t, gateRun, merges[0].Run)
}

// mustGatePoll runs one poll and fails on an error.
func mustGatePoll(t *testing.T, p *GatePoller) GatePollResult {
	t.Helper()
	res, err := p.Poll(context.Background())
	require.NoError(t, err)
	return res
}

// A gate whose bead is already closed or failed still gets cleared, so it
// stops blocking that bead's dependents (P6-14).
func TestGatePollerClearsAnEscalatedGateOnASettledBead(t *testing.T) {
	t.Parallel()
	for _, state := range []domain.BeadState{domain.BeadStateClosed, domain.BeadStateFailed, domain.BeadStateNeedsHuman} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			h := newGateHarness(t, nil)
			h.BD.AddGate("fx-8za", "fx-6z4", 2)
			h.BD.AddBead("fx-6z4", state, 2)
			h.BD.Answer(gateCheck(t, gateOutClosedPR, ""))

			res := h.poll()

			assert.Empty(t, res.Blocked, "a settled bead is not moved again")
			assert.Equal(t, []domain.BeadID{"fx-8za"}, res.Resolved)
			assert.Empty(t, h.BD.Statuses)
			assert.Empty(t, h.BD.Closed)
			require.Len(t, h.BD.Resolved, 1)
		})
	}
}

// A gate the poller cannot resolve is reported once and left open.
func TestGatePollerReportsAFailedGateResolve(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	h.BD.AddGate("fx-8za", "fx-6z4", 2)
	h.BD.AddBead("fx-6z4", domain.BeadStateDelivering, 2)
	h.BD.mu.Lock()
	h.BD.resolveErr = errors.New("bd: exit status 1")
	h.BD.mu.Unlock()
	h.BD.Answer(gateCheck(t, gateOutClosedPR, ""))

	res := h.poll()

	assert.Equal(t, []domain.BeadID{"fx-6z4"}, res.Blocked, "the bead is parked whatever the gate does")
	assert.Empty(t, res.Resolved)
	var complaints int
	for _, w := range h.Rec.warnings() {
		if strings.Contains(w.Message, "could not resolve the escalated gate") {
			complaints++
		}
	}
	assert.Equal(t, 1, complaints)
}

// The PR number falls back to the bead's own metadata when bd's gate
// carries no --await-id, and the reason says so plainly when there is no
// number at all.
func TestGatePollerPRNumberFallsBackToTheBead(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)
	h.BD.mu.Lock()
	h.BD.gates["fx-qba"] = beads.Gate{ID: "fx-qba", Status: "open", AwaitType: beads.GateTypePR, Blocks: "fx-byi"}
	h.BD.mu.Unlock()
	h.BD.AddBead("fx-byi", domain.BeadStateDelivering, 7)
	h.BD.Answer(gateCheck(t, gateOutMerged, ""))

	h.poll()

	assert.Equal(t, []string{"fx-byi: pull request #7 merged"}, h.BD.Closed)
	require.Len(t, h.Rec.merges(), 1)
	assert.Equal(t, 7, h.Rec.merges()[0].PRNumber)

	assert.Equal(t, "the pull request merged", gateMergeReason(0))
	assert.Equal(t, "the pull request was closed without merging", gateEscalationReason(0, ""))
	assert.Equal(t,
		"pull request #3 was closed without merging",
		gateEscalationReason(3, "pull request #3 was closed without merging"),
		"bd repeating what we already said is not appended twice")
}

// A transition the machine refuses is the machine's own Warning, not a
// second one from the poller.
func TestGatePollerDoesNotDoubleWarnOnARefusedTransition(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, nil)

	ok := h.P.apply(context.Background(), StateRequest{
		Bead:  "fx-byi",
		From:  domain.BeadStateClosed,
		Event: BeadEventDelivered,
	})

	assert.False(t, ok)
	warns := h.Rec.warnings()
	require.Len(t, warns, 1, "one Warning, from the state machine")
	assert.Equal(t, SourceState, warns[0].Source)
	assert.True(t, gateTransitionRefused(fmt.Errorf("wrapped: %w", ErrInvalidTransition)))
	assert.False(t, gateTransitionRefused(errors.New("bd: exit status 1")))
}

// ------------------ P6-29: the vitals `sc status` and the TUI header read

// vitalsAt is the instant every vitals test takes its snapshot at.
var vitalsAt = gateEpoch.Add(time.Hour)

// fakePlanning is a PlanningReporter that returns what it was given.
type fakePlanning struct{ st PlanningStatus }

func (f fakePlanning) Status() PlanningStatus { return f.st }

// fakeGates is a GateReporter that returns what it was given.
type fakeGates struct{ st GatePollStats }

func (f fakeGates) Stats() GatePollStats { return f.st }

// P6-29's first question: an idle autopilot run must say when it will plan
// again, rather than looking like a run that has died.
func TestRunVitalsReportsTheNextPlanningTime(t *testing.T) {
	t.Parallel()
	v := RunVitals{
		At: vitalsAt,
		Planning: PlanningStatus{
			Mode: domain.RunModeAutopilot, Attempts: 4, Plans: 3, Beads: 9, Empty: 1,
			NextAt: vitalsAt.Add(90 * time.Second),
		},
	}

	line := v.PlanningLine()

	assert.Contains(t, line, "planning (autopilot)")
	assert.Contains(t, line, "4 firings, 3 plans, 9 beads")
	assert.Contains(t, line, "next in 1m30s")
	assert.Contains(t, line, gateEpoch.Add(time.Hour+90*time.Second).Format(vitalsTimeFormat),
		"the absolute time is there for a log a person reads later")
	assert.Contains(t, line, "1 proposed nothing")
	assert.Contains(t, v.Header(), "plan in 1m30s")
}

// The other planning states each say what they are, so "idle" is never
// guesswork.
func TestRunVitalsPlanningLineCoversEveryState(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		st   PlanningStatus
		want string
	}{
		{"nothing yet", PlanningStatus{Mode: domain.RunModeBrief}, "nothing planned yet"},
		{"running", PlanningStatus{Attempts: 1, Running: true}, "planning now"},
		{"finished", PlanningStatus{Attempts: 1, Plans: 1, Finished: true}, "finished"},
		{"waiting", PlanningStatus{Attempts: 1, Plans: 1}, "waiting for the loop"},
		{"resumed", PlanningStatus{ResumedPlan: true}, "plan resumed"},
		{"rejections", PlanningStatus{Attempts: 2, Rejections: 2}, "2 rejected in a row"},
		{"failures", PlanningStatus{Attempts: 3, Failures: 3}, "3 failed in a row"},
		{"gave up", PlanningStatus{Attempts: 3, GaveUp: 1}, "1 given up"},
		{"deprioritised", PlanningStatus{Attempts: 1, Deprioritised: 2}, "2 deprioritised"},
		{"last error", PlanningStatus{Attempts: 1, LastError: "bd is locked"}, "last error: bd is locked"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			assert.Contains(t, RunVitals{At: vitalsAt, Planning: c.st}.PlanningLine(), c.want)
		})
	}
}

// P6-29's second question: a gh outage must be visible without reading the
// log, and it must say how long it has been going on.
func TestRunVitalsShowsAGHOutage(t *testing.T) {
	t.Parallel()
	v := RunVitals{
		At: vitalsAt,
		Gates: GatePollStats{
			Polls: 12, Skipped: 5, Closed: 2, LastAt: vitalsAt.Add(-30 * time.Second),
			GHDown: true, GHDownSince: vitalsAt.Add(-5 * time.Minute),
		},
	}

	line := v.GatesLine()

	assert.Contains(t, line, "12 polls")
	assert.Contains(t, line, "last 30s ago")
	assert.Contains(t, line, "2 merged")
	assert.Contains(t, line, "gh unavailable, 5m0s so far")
	assert.Contains(t, line, "5 polls skipped")
	assert.Equal(t, "gh down 5m0s", v.Header())
}

// P6-29's third question: a merge bd will not let the poller finish has to
// show as a pending merge, because anything but zero for long means a human
// is needed.
func TestRunVitalsShowsAStuckClose(t *testing.T) {
	t.Parallel()
	one := RunVitals{At: vitalsAt, Gates: GatePollStats{Polls: 9, Pending: 1, Retries: 4, Failures: 2}}
	assert.Contains(t, one.GatesLine(), "1 merge pending after 4 retries")
	assert.Contains(t, one.GatesLine(), "2 failed polls")
	assert.Contains(t, one.Header(), "1 pending")

	many := RunVitals{At: vitalsAt, Gates: GatePollStats{Polls: 9, Pending: 3}}
	assert.Contains(t, many.GatesLine(), "3 merges pending")
	assert.NotContains(t, many.GatesLine(), "after")
}

// P6-28's count reaches the same report: a run leaving beads alone says so
// where a person is already looking.
func TestRunVitalsShowsTheBeadsLeftAlone(t *testing.T) {
	t.Parallel()
	assert.Empty(t, RunVitals{At: vitalsAt}.ForeignLine())
	assert.Equal(t, "beads: 1 left alone, in a status slop-cannon did not register",
		RunVitals{At: vitalsAt, Foreign: 1}.ForeignLine())
	assert.Contains(t, RunVitals{At: vitalsAt, Foreign: 4}.ForeignLine(), "4 left alone")

	v := RunVitals{At: vitalsAt, Foreign: 4, Gates: GatePollStats{Polls: 1}}
	require.Len(t, v.Lines(), 3, "planning, gates and the beads left alone")
	assert.Contains(t, v.Header(), "4 left alone")
	assert.Len(t, RunVitals{At: vitalsAt}.Lines(), 2, "and nothing about them when there are none")
}

// A run nothing has happened in yet reads as one, and says nothing at all
// in the header — there is no news to put there.
func TestRunVitalsZeroValueIsQuiet(t *testing.T) {
	t.Parallel()
	v := RunVitals{At: vitalsAt}

	assert.Equal(t, []string{"planning: nothing planned yet", "gates: not polled yet"}, v.Lines())
	assert.Empty(t, v.Header())

	// With no snapshot clock every time is printed absolutely rather than
	// guessed at.
	noClock := RunVitals{Planning: PlanningStatus{Attempts: 1, NextAt: gateEpoch}}
	assert.Contains(t, noClock.PlanningLine(), "next at "+gateEpoch.Format(vitalsTimeFormat))
	assert.Contains(t, RunVitals{Gates: GatePollStats{GHDown: true, GHDownSince: gateEpoch}}.GatesLine(),
		"gh unavailable, for an unknown time")
}

// Vitals reads the live parts, and every one of them is optional: a run
// with safety off has no gate poller and a run resumed into an already
// finished plan has no planner to ask.
func TestVitalsSnapshotsTheLiveParts(t *testing.T) {
	t.Parallel()
	h := newGateHarness(t, func(o *GatePollerOptions) {
		o.Statuses = beads.StatusMap{"reviewing": domain.CategoryWIP}
	})
	h.BD.AddGate("fx-qba", "fx-byi", 1)
	h.BD.AddBead("fx-byi", domain.BeadState("reviewing"), 1)
	h.BD.Answer(gateCheck(t, gateOutMerged, ""))
	h.poll()

	plan := fakePlanning{st: PlanningStatus{Mode: domain.RunModeAutopilot, Attempts: 2, Plans: 2}}
	v := Vitals(vitalsAt, plan, h.P, h.P.Foreign())

	assert.Equal(t, vitalsAt, v.At)
	assert.Equal(t, 2, v.Planning.Attempts)
	assert.Equal(t, 1, v.Gates.Polls)
	assert.Equal(t, 1, v.Foreign, "the run's own reporter, not the poller's counter")
	assert.Contains(t, strings.Join(v.Lines(), "\n"), "left alone")

	empty := Vitals(vitalsAt, nil, nil, nil)
	assert.Equal(t, RunVitals{At: vitalsAt}, empty, "every part is optional")
	assert.Empty(t, empty.Header())

	// And the interfaces are the production types' own.
	var _ PlanningReporter = fakePlanning{}
	var _ GateReporter = fakeGates{st: GatePollStats{}}
}

// The stats are JSON-tagged so state.json and `sc status --json` can carry
// them without a second shape being defined for them (P6-29, P6-42).
func TestRunVitalsMarshalsWithSnakeCaseKeys(t *testing.T) {
	t.Parallel()
	v := RunVitals{
		At:    vitalsAt,
		Gates: GatePollStats{Polls: 3, Pending: 1, Foreign: 2, GHDown: true, GHDownSince: gateEpoch},
	}

	out, err := json.Marshal(v)

	require.NoError(t, err)
	for _, want := range []string{`"polls":3`, `"pending":1`, `"foreign":2`, `"gh_down":true`, `"gh_down_since"`} {
		assert.Contains(t, string(out), want)
	}
	assert.NotContains(t, string(out), `"closed"`, "a zero counter is left out")
}
