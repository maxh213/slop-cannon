package orchestrator

// Production signals for the autopilot product owner (P6-71, D-32).
//
// The planning product owner can only read the repository: its shell is a
// fixed read-only allow-list, and widening it to reach an error tracker or
// a log store would hand a model the user's credentials. So the cannon runs
// the user's own `signals_command` itself, the way a delivery runs
// `check_command`, and gives the output to the product owner as fenced,
// untrusted evidence ([agents.PlanSignals]).
//
// The [Planner] keeps the last reading under its own mutex and re-runs the
// command only when `signals_interval` has passed: a plan in between reuses
// the reading it has. A changed reading may also ask for a plan of its own
// while engineers are busy (see [Planner.signalsWantLocked] for the rules),
// so an error production started throwing does not wait for the queue to
// drain. Brief mode never runs the command.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/proc"
	"slop-cannon/internal/textsafe"
)

// SignalsReading is one run of the signals command: what it printed (the
// tail, scrubbed and secret-filtered) and how it ended.
type SignalsReading struct {
	// Command is the command that was run.
	Command string `json:"command"`
	// ExitCode is its exit status; -1 when it was killed or never started.
	ExitCode int `json:"exit_code"`
	// RanAt is when it was started, and Duration how long it ran.
	RanAt    time.Time     `json:"ran_at"`
	Duration time.Duration `json:"duration"`
	// Output is the last [agents.MaxSignalsBytes] of its standard output,
	// cut at a line boundary, with terminal controls scrubbed and secrets
	// masked. Standard error is not evidence: only its last line is kept,
	// in Err, when the command fails.
	Output string `json:"output"`
	// Truncated is true when Output is the tail of a longer output.
	Truncated bool `json:"truncated,omitempty"`
	// Err is one line saying why the reading is unusable — a non-zero
	// exit, a timeout, a command that did not start; empty on success.
	Err string `json:"error,omitempty"`
	// class is Err without the standard error line — "exit status 3",
	// "timed out after 5m0s" — which is what a repeated failure is
	// recognised by: a CLI that puts a request id or a timestamp on its
	// last error line still fails the same way.
	class string
}

// Failed reports whether the reading is unusable as evidence.
func (r SignalsReading) Failed() bool { return r.Err != "" }

// failureKey is what tells one failure from another, for warning once per
// distinct failure.
func (r SignalsReading) failureKey() string {
	if r.class != "" {
		return r.class
	}
	return r.Err
}

// hash identifies a successful reading's output, so the planner can tell a
// changed reading from the one the last plan already saw. A failed reading
// has no hash: it is never evidence.
func (r SignalsReading) hash() string {
	if r.Failed() {
		return ""
	}
	sum := sha256.Sum256([]byte(r.Output))
	return hex.EncodeToString(sum[:])
}

// plan is the reading as the planning prompt shows it.
func (r SignalsReading) plan() *agents.PlanSignals {
	return &agents.PlanSignals{
		Command: r.Command, RanAt: r.RanAt, ExitCode: r.ExitCode,
		Output: r.Output, Truncated: r.Truncated, Failure: r.Err,
	}
}

// SignalsOptions is one run of the signals command.
type SignalsOptions struct {
	// Command is run with "sh -c".
	Command string
	// Dir is the directory it runs in: the repository root.
	Dir string
	// Timeout kills it, and everything it started, when it elapses; zero
	// means config.DefaultSignalsTimeout's five minutes.
	Timeout time.Duration
	// Filter is the run's secrets filter; nil means the built-in patterns.
	Filter *agents.SecretFilter
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// signalsDefaultTimeout is the timeout of a run with none configured.
const signalsDefaultTimeout = 5 * time.Minute

// signalsErrLineMax bounds the one line a failure is reported as.
const signalsErrLineMax = 300

// signalsStderrMax bounds how much standard error a reading holds while the
// command runs: only its last line is ever used.
const signalsStderrMax = 4 << 10

// RunSignals runs the signals command and returns its reading. It never
// returns an error: a command that fails, exits non-zero or times out is a
// reading whose Err says so, because production being unreachable must
// never fail a plan.
//
// It runs like the delivery check (gitx runCheck): "sh -c" in the
// repository root with the whole of the user's environment, so the command
// finds the tokens and tools the user's shell has, and every process it
// leaves behind is reaped.
func RunSignals(ctx context.Context, o SignalsOptions) SignalsReading {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = signalsDefaultTimeout
	}
	command := o.Filter.Text(textsafe.ScrubLine(o.Command))
	read := SignalsReading{Command: command, RanAt: now()}
	res, err := proc.Run(ctx, signalsSpec(o.Command, o.Dir, timeout))
	read.ExitCode, read.Duration = res.ExitCode, res.Duration
	read.Output, read.Truncated = signalsClean(res.Stdout, o.Filter)
	if err == nil {
		return read
	}
	var ee *proc.ExitError
	if !errors.As(err, &ee) {
		read.ExitCode = -1 // sh never started
	}
	why := ""
	switch {
	case errors.Is(err, proc.ErrTimeout):
		why = "timed out after " + timeout.String()
	case ctx.Err() != nil:
		why = "stopped with the run"
	case ee != nil && ee.Result.ExitCode >= 0:
		why = fmt.Sprintf("exit status %d", ee.Result.ExitCode)
	case ee != nil:
		why = "killed by a signal"
	default:
		why = "could not start: " + err.Error()
	}
	read.class = signalsLine(o.Filter.Text(textsafe.ScrubLine(why)))
	if last := signalsLastLine(res.Stderr); last != "" {
		why += ": " + last
	}
	read.Err = signalsLine(o.Filter.Text(textsafe.ScrubLine(why)))
	return read
}

// signalsSpec is how the signals command runs: "sh -c" in dir with the
// whole of the user's environment, its leftovers reaped, and only a tail of
// each stream held while it runs.
func signalsSpec(command, dir string, timeout time.Duration) proc.Spec {
	var inherit []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		inherit = append(inherit, name)
	}
	return proc.Spec{
		Command:         "sh",
		Args:            []string{"-c", command},
		Dir:             dir,
		InheritEnv:      inherit,
		Timeout:         timeout,
		ReapDescendants: true,
		// The command can print far more than is kept — `cat` of a whole
		// log — so only a tail is held while it runs. One byte over the
		// window is how [signalsClean] still sees that it was cut.
		StdoutTail: 4*agents.MaxSignalsBytes + 1,
		StderrTail: signalsStderrMax,
	}
}

// signalsClean turns raw standard output into prompt-safe evidence: cut to
// a bounded window first (so a command that prints megabytes costs the
// secret patterns a bounded scan), scrubbed of terminal controls with its
// line structure and tabs kept and a carriage-return progress line reduced
// to its final state, secret-masked, and cut again to the cap at a line
// boundary — after masking, so a mask can only shorten what is kept.
func signalsClean(raw string, f *agents.SecretFilter) (string, bool) {
	window, cut := signalsTail(raw, 4*agents.MaxSignalsBytes)
	clean := f.Text(textsafe.ScrubBlock(window, textsafe.Policy{Tab: "\t", ApplyCR: true}))
	out, cut2 := signalsTail(clean, agents.MaxSignalsBytes)
	return out, cut || cut2
}

// signalsTail keeps the last n bytes of s, starting at a line boundary: the
// partial first line of the window is dropped. A window with no line break
// at all (one enormous line) keeps its last n bytes, from the first whole
// rune. It reports whether anything was dropped.
func signalsTail(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	cut := s[len(s)-n:]
	if s[len(s)-n-1] != '\n' {
		if i := strings.IndexByte(cut, '\n'); i >= 0 {
			cut = cut[i+1:]
		}
	}
	for len(cut) > 0 && !utf8.RuneStart(cut[0]) {
		cut = cut[1:]
	}
	return cut, true
}

// signalsLastLine is the last non-blank line of s.
func signalsLastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// signalsLine bounds a one-line failure.
func signalsLine(s string) string {
	if len(s) <= signalsErrLineMax {
		return s
	}
	cut := s[:signalsErrLineMax]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}

// SignalsRunner takes one reading; [RunSignals] behind the configuration is
// the production one, and a test substitutes a function.
type SignalsRunner func(ctx context.Context) SignalsReading

// SignalsStatus is the signals half of [PlanningStatus], for `sc status`
// and the TUI header.
type SignalsStatus struct {
	// LastAt is when the last reading was taken; zero before the first.
	LastAt time.Time `json:"last_at,omitempty"`
	// ExitCode is the last reading's exit status.
	ExitCode int `json:"exit_code"`
	// Bytes is how much output the last reading kept.
	Bytes int `json:"bytes"`
	// NextAt is when the command is next due; zero means "on the next
	// plan".
	NextAt time.Time `json:"next_at,omitempty"`
	// Running is true while a reading is being taken.
	Running bool `json:"running,omitempty"`
	// LastError is the last reading's failure, if it failed.
	LastError string `json:"last_error,omitempty"`
}

// signalsState is everything the planner remembers about the signals
// command, guarded by [Planner.sigMu].
type signalsState struct {
	// last is the latest reading; nil before the first.
	last *SignalsReading
	// running is true while a reading is being taken, and done is closed
	// when it finishes.
	running bool
	done    chan struct{}
	// warned is the failure last warned about, so a command that keeps
	// failing the same way warns once.
	warned string
	// seen is the hash of the successful reading the last plan was given,
	// and given is true once any plan of this process has been given a
	// reading. Until then the first reading is the baseline, not a change:
	// a resumed run that adopted its plan from state.json has given no
	// plan a reading, and production printing what it printed before the
	// restart is no reason to plan.
	seen  string
	given bool
	// wanted latches a signals-driven plan demand until a plan fires.
	wanted bool
	// firing is true while the firing that consumed the latch runs, and
	// firedAt is when the last signals-driven plan fired.
	firing  bool
	firedAt time.Time
	// planBeads are the beads the last signals-driven plan created, and
	// untouched is true when, at the last reading, every one of them was
	// still open: nothing has picked any of them up yet.
	planBeads []domain.BeadID
	untouched bool
	// outstanding is how many of the run's beads were still to be shipped
	// at the last reading (see [signalsOutstanding]); -1 when the listing
	// failed.
	outstanding int
}

// signalsOn reports whether this run takes signals: autopilot with a
// configured command.
func (p *Planner) signalsOn() bool {
	return p.signalsRun != nil && p.cfg.Run.Mode == domain.RunModeAutopilot
}

// signalsDueLocked reports whether the command should run again. The
// caller holds sigMu.
func (p *Planner) signalsDueLocked(now time.Time) bool {
	return p.sig.last == nil || now.Sub(p.sig.last.RanAt) >= p.sigInterval
}

// signalsWantLocked decides whether a changed signals reading asks for a
// plan of its own while the ordinary autopilot guards hold planning back
// (engineers busy, the last dispatch still firing). The caller holds p.mu;
// this takes sigMu (the lock order is mu before sigMu, and nothing that
// holds sigMu ever takes mu).
//
// It is also what keeps the reading fresh between plans: when the interval
// has passed and no reading is being taken, it starts one in the
// background and answers no — the scheduler's loop never waits for a
// command that may take minutes. The demand is offered only when all of
// these hold:
//
//   - the latest reading succeeded, printed something, and its output
//     differs from the reading the last plan was given (the first reading
//     a process takes with no plan given one is the baseline);
//   - no signals-driven plan has fired within `signals_interval`;
//   - the previous signals-driven plan's beads are not all still open (all
//     open means nobody has picked any up yet, and another plan would only
//     pile more on top of them);
//   - fewer of the run's beads are still to be shipped (open, being worked
//     on, in review, approved, delivering or escalated) than one plan may
//     create (`max_beads_per_plan`). A reading with timestamps in it
//     changes every interval, and with engineers busy a plan every
//     `signals_interval` would otherwise queue beads far faster than the
//     run (and auto-merge's spacing) can ship them; once the backlog is
//     worked down, the next changed reading plans again. A listing that
//     failed counts as a full backlog.
//
// Once offered it stays offered until a plan fires, because the scheduler
// may not have a slot for it on this tick.
func (p *Planner) signalsWantLocked(ctx context.Context, now time.Time) bool {
	p.sigMu.Lock()
	defer p.sigMu.Unlock()
	s := &p.sig
	switch {
	case s.wanted:
		return true
	case s.running:
		return false
	case p.signalsDueLocked(now):
		p.startSignalsLocked(ctx)
		return false
	case s.last.Failed() || s.last.hash() == s.seen:
		return false
	case !s.given:
		s.seen, s.given = s.last.hash(), true
		return false
	case !s.last.plan().Evidence():
		// Production went quiet: nothing for a plan of its own to act on.
		return false
	case !s.firedAt.IsZero() && now.Sub(s.firedAt) < p.sigInterval:
		return false
	case s.untouched:
		return false
	case s.outstanding < 0 || s.outstanding >= p.maxBeads:
		return false
	}
	s.wanted = true
	return true
}

// startSignalsLocked starts a reading on its own goroutine. Each reading
// also looks up whether the previous signals-driven plan's beads are
// untouched, for the next demand decision, so a reading a quiet-time plan
// took refreshes it too. The caller holds sigMu.
func (p *Planner) startSignalsLocked(ctx context.Context) {
	s := &p.sig
	s.running, s.done = true, make(chan struct{})
	prev := append([]domain.BeadID(nil), s.planBeads...)
	go func() {
		read := p.signalsRun(ctx)
		untouched, outstanding := p.signalsBacklog(ctx, prev)
		p.finishSignals(read, untouched, outstanding, ctx.Err() != nil)
	}()
}

// signalsBacklog reports whether every bead of the previous
// signals-driven plan (ids) is still open, and how many of the run's beads
// are still to be shipped. A listing that fails counts as untouched and as
// a full backlog (-1): when the planner cannot tell, it waits rather than
// plans.
func (p *Planner) signalsBacklog(ctx context.Context, ids []domain.BeadID) (untouched bool, outstanding int) {
	if p.opts.Beads == nil {
		return false, 0
	}
	list, err := p.opts.Beads(ctx)
	if err != nil {
		return len(ids) > 0, -1
	}
	state := make(map[domain.BeadID]domain.BeadState, len(list))
	for _, b := range list {
		state[b.ID] = b.State
		if signalsOutstanding(b) {
			outstanding++
		}
	}
	if len(ids) == 0 {
		return false, outstanding
	}
	for _, id := range ids {
		if st, ok := state[id]; !ok || st != domain.BeadStateOpen {
			return false, outstanding
		}
	}
	return true, outstanding
}

// signalsOutstanding reports whether b is work the run has yet to ship:
// planned work that is not closed and not parked for a person.
func signalsOutstanding(b domain.Bead) bool {
	if planInfrastructure(b) {
		return false
	}
	switch b.State {
	case domain.BeadStateOpen, domain.BeadStateInProgress, domain.BeadStateInReview,
		domain.BeadStateChangesRequested, domain.BeadStateApproved, domain.BeadStateDelivering,
		domain.BeadStateEscalated:
		return true
	}
	return false
}

// finishSignals stores a reading, warns about a failure once per distinct
// error, and then wakes whoever is waiting for it, so a plan that waited
// for the reading is never fired ahead of its warning. It is called with no
// lock held, and warns only after releasing sigMu.
//
// A reading cut short because its context ended — the run stopping, the
// firing that wanted it abandoned — says nothing about production: it is
// dropped, with no warning, and the reading before it stays cached.
func (p *Planner) finishSignals(read SignalsReading, untouched bool, outstanding int, cancelled bool) {
	p.sigMu.Lock()
	s := &p.sig
	s.running = false
	done := s.done
	s.done = nil
	if cancelled {
		p.sigMu.Unlock()
		close(done)
		return
	}
	s.last, s.untouched, s.outstanding = &read, untouched, outstanding
	warn := read.Failed() && read.failureKey() != s.warned
	if read.Failed() {
		s.warned = read.failureKey()
	} else {
		s.warned = ""
	}
	p.sigMu.Unlock()

	if warn {
		p.warn("the signals command gave no reading; planning goes ahead without production evidence",
			errors.New(read.Err))
	}
	if r := p.host(); r != nil {
		r.VitalsChanged()
	}
	close(done)
}

// signalsForPlan returns the reading a planning firing is given: the cached
// one while it is fresh, the one being taken if a reading is in flight, or
// a new one taken now. It records the reading as seen, so a signals-driven
// demand only follows a reading no plan has been given, and it consumes a
// latched signals demand, marking this firing as signals-driven. Nil when
// the run takes no signals.
func (p *Planner) signalsForPlan(ctx context.Context) *agents.PlanSignals {
	if !p.signalsOn() {
		return nil
	}
	p.sigMu.Lock()
	if !p.sig.running && p.signalsDueLocked(p.opts.Now()) {
		p.startSignalsLocked(ctx)
	}
	done := p.sig.done
	p.sigMu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return nil
		}
	}

	p.sigMu.Lock()
	defer p.sigMu.Unlock()
	s := &p.sig
	if s.last == nil {
		return nil
	}
	if !s.last.Failed() {
		s.seen = s.last.hash()
	}
	s.given = true
	if s.wanted {
		s.wanted, s.firing, s.firedAt = false, true, p.opts.Now()
	}
	return s.last.plan()
}

// signalsDriven reports whether the firing now in progress consumed a
// signals demand, which [Planner.signalsForPlan] marks.
func (p *Planner) signalsDriven() bool {
	p.sigMu.Lock()
	defer p.sigMu.Unlock()
	return p.sig.firing
}

// signalsPlanned records the beads a signals-driven firing created, so the
// next signals demand waits until somebody has picked one of them up.
func (p *Planner) signalsPlanned(created map[string]domain.BeadID, err error) {
	p.sigMu.Lock()
	defer p.sigMu.Unlock()
	if !p.sig.firing {
		return
	}
	p.sig.firing = false
	if err != nil || len(created) == 0 {
		return
	}
	ids := make([]domain.BeadID, 0, len(created))
	for _, id := range created {
		ids = append(ids, id)
	}
	p.sig.planBeads, p.sig.untouched = ids, true
}

// signalsStatusLocked is the signals half of [Planner.Status]; nil when the
// run takes no signals. The caller holds p.mu.
func (p *Planner) signalsStatusLocked() *SignalsStatus {
	if !p.signalsOn() {
		return nil
	}
	p.sigMu.Lock()
	defer p.sigMu.Unlock()
	st := &SignalsStatus{Running: p.sig.running}
	if r := p.sig.last; r != nil {
		st.LastAt, st.ExitCode, st.Bytes, st.LastError = r.RanAt, r.ExitCode, len(r.Output), r.Err
		// Once it is due and nothing has taken it (a demand is latched,
		// a hold is on, or the run is quiet and the reading waits for the
		// next plan) it is due on the next plan, not at a time gone by.
		if next := r.RanAt.Add(p.sigInterval); next.After(p.opts.Now()) {
			st.NextAt = next
		}
	}
	return st
}
