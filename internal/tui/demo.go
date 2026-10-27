package tui

import (
	"context"
	"sync"
	"time"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// Default pacing and shape of the scripted demo run.
const (
	// DemoRunID is the run id of the scripted run. It is fixed so demo
	// frames are byte-for-byte reproducible.
	DemoRunID domain.RunID = "demo-20260101-120000"
	// DemoStep is the simulated time between two scripted events; it is what
	// the timestamps in the log view advance by.
	DemoStep = 3 * time.Second
	// DemoDelay is the wall-clock pause between two scripted events when the
	// script is played. Zero plays the whole script at once.
	DemoDelay = 250 * time.Millisecond
	// DemoAmmo is the ammo of the scripted run.
	DemoAmmo = 10
)

// DemoStart is the wall-clock time the scripted run pretends to start at.
// It is fixed (and UTC) so every timestamp the demo renders is stable.
var DemoStart = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// DemoOptions configure the scripted event stream. The zero value is valid
// and means the fixed defaults above.
type DemoOptions struct {
	// RunID names the scripted run; empty means [DemoRunID].
	RunID domain.RunID
	// Start is the simulated start time; zero means [DemoStart].
	Start time.Time
	// Step is the simulated gap between events; zero means [DemoStep], and a
	// negative value means no gap at all.
	Step time.Duration
	// Delay is the wall-clock pause [PlayScript] takes between events; zero
	// means [DemoDelay], and a negative value means none (as fast as the bus
	// accepts them).
	Delay time.Duration
	// Ammo is the run's concurrency cap; zero means [DemoAmmo].
	Ammo int
	// Safety is the run's safety setting; the script opens a pull request
	// when it is on and merges straight to the default branch when it is off.
	Safety bool
	// Brief makes the scripted run a brief run instead of autopilot.
	Brief string
	// RepoPath is the repository the run pretends to fire at.
	RepoPath string
}

// withDefaults fills the zero fields in.
func (o DemoOptions) withDefaults() DemoOptions {
	if o.RunID == "" {
		o.RunID = DemoRunID
	}
	if o.Start.IsZero() {
		o.Start = DemoStart
	}
	switch {
	case o.Step == 0:
		o.Step = DemoStep
	case o.Step < 0:
		o.Step = 0
	}
	switch {
	case o.Delay == 0:
		o.Delay = DemoDelay
	case o.Delay < 0:
		o.Delay = 0
	}
	if o.Ammo <= 0 {
		o.Ammo = DemoAmmo
	}
	if o.RepoPath == "" {
		o.RepoPath = "/tmp/demo-repo"
	}
	return o
}

// Mode returns the run mode the options imply.
func (o DemoOptions) Mode() domain.RunMode {
	if o.Brief != "" {
		return domain.RunModeBrief
	}
	return domain.RunModeAutopilot
}

// DemoRun returns the [domain.Run] the scripted stream describes. Pass it as
// [Options.Run] so the header is populated before the first event lands.
func DemoRun(opts DemoOptions) domain.Run {
	o := opts.withDefaults()
	return domain.Run{
		ID:        o.RunID,
		RepoPath:  o.RepoPath,
		StateDir:  "/tmp/demo-state/" + string(o.RunID),
		Mode:      o.Mode(),
		Brief:     o.Brief,
		StartedAt: o.Start,
		Config: domain.ConfigSnapshot{
			Ammo:      o.Ammo,
			Safety:    o.Safety,
			Cartridge: "claude",
			Model:     "fable",
			ReviewPRs: true,
		},
	}
}

// ScriptedEvent is one event of the demo stream plus the wall-clock pause to
// take before publishing it.
type ScriptedEvent struct {
	// Delay is how long [PlayScript] waits before publishing Event.
	Delay time.Duration
	// Event is the event to publish.
	Event events.Event
}

// The beads and agents of the scripted run.
const (
	demoEpic    domain.BeadID  = "demo-1"
	demoBeadA   domain.BeadID  = "demo-2"
	demoBeadB   domain.BeadID  = "demo-3"
	demoPO      domain.AgentID = "po-1"
	demoEng1    domain.AgentID = "eng-demo-2-r1"
	demoEng2    domain.AgentID = "eng-demo-2-r2"
	demoTestQA  domain.AgentID = "tester-qa-demo-2-r1"
	demoTestUX  domain.AgentID = "tester-ux-demo-2-r1"
	demoTestQA2 domain.AgentID = "tester-qa-demo-2-r2"
	demoEngB    domain.AgentID = "eng-demo-3-r1"
)

// DemoScript returns the whole scripted run: a plan, one bead through a
// full feedback loop (change request, pushback, arbitration, approval,
// delivery) and a second bead that fails, plus the pause/resume and stop
// events. Every event kind of [events.Kinds] appears at least once, so a
// view can be developed and tested against it without a real run.
func DemoScript(opts DemoOptions) []ScriptedEvent {
	o := opts.withDefaults()
	run := DemoRun(o)
	b := &demoBuilder{opts: o, at: o.Start}

	b.add(events.RunStarted{Meta: b.meta(), Info: run})
	b.spawn(demoPO, domain.RolePO, "", "", "", events.TaskPlan, 0)
	b.add(events.AgentOutput{Meta: b.meta(), Agent: demoPO, Stream: events.StreamStdout, Line: "reading the repository…"})
	b.add(events.AgentOutput{Meta: b.meta(), Agent: demoPO, Stream: events.StreamStderr, Line: "note: no test command configured, using `make test`"})

	plan := domain.Plan{
		Brief: o.Brief,
		Beads: []domain.PlanBead{
			{Key: "retry", Title: "Retry failed HTTP requests", Acceptance: "requests retry three times with backoff", Type: domain.BeadTypeFeature, Priority: domain.PriorityDefault, Size: domain.SizeM},
			{Key: "docs", Title: "Document the retry policy", Acceptance: "README explains the backoff", Type: domain.BeadTypeChore, Priority: 3, Size: domain.SizeS, Deps: []string{"retry"}},
		},
	}
	b.add(events.AgentFinished{Meta: b.meta(), Agent: demoPO, Role: domain.RolePO, Duration: 12 * time.Second, Produced: domain.MessagePlan})
	b.add(events.PlanReviewed{Meta: b.meta(), Planner: demoPO, Candidate: "p2:retry", Decision: domain.PlanReviewDuplicate,
		Canonical: "p1:retry", Provenance: domain.PlanProvenanceDeterministic})
	b.add(events.PlanProduced{
		Meta:    b.meta(),
		Agent:   demoPO,
		Epic:    demoEpic,
		Plan:    plan,
		Created: map[string]domain.BeadID{"retry": demoBeadA, "docs": demoBeadB},
	})

	epic := domain.Bead{ID: demoEpic, Title: "Run " + string(o.RunID), Type: domain.BeadTypeEpic, Priority: domain.PriorityDefault, State: domain.BeadStateOpen}
	beadA := domain.Bead{ID: demoBeadA, Title: "Retry failed HTTP requests", Acceptance: "requests retry three times with backoff", Type: domain.BeadTypeFeature, Priority: domain.PriorityDefault, Size: domain.SizeM, State: domain.BeadStateOpen}
	beadB := domain.Bead{ID: demoBeadB, Title: "Document the retry policy", Acceptance: "README explains the backoff", Type: domain.BeadTypeChore, Priority: 3, Size: domain.SizeS, State: domain.BeadStateOpen, Deps: []domain.BeadID{demoBeadA}}
	b.add(events.BeadCreated{Meta: b.meta(), Bead: epic})
	b.add(events.BeadCreated{Meta: b.meta(), Bead: beadA})
	b.add(events.BeadCreated{Meta: b.meta(), Bead: beadB})
	b.add(events.GraphChanged{Meta: b.meta(), Epic: demoEpic, Beads: []domain.Bead{beadA, beadB}})

	// Bead A, round 1: implement, review, changes requested.
	b.add(events.BeadStateChanged{Meta: b.meta(), Bead: demoBeadA, From: domain.BeadStateOpen, To: domain.BeadStateInProgress, Round: 1, Reason: "claimed by " + string(demoEng1)})
	b.spawn(demoEng1, domain.RoleEngineer, "", demoBeadA, beadA.Title, events.TaskImplement, 1)
	b.add(events.AgentOutput{Meta: b.meta(), Agent: demoEng1, Stream: events.StreamStdout, Line: "editing internal/http/client.go"})
	b.add(events.AgentFinished{Meta: b.meta(), Agent: demoEng1, Role: domain.RoleEngineer, Bead: demoBeadA, Duration: 96 * time.Second, Produced: domain.MessageImplReport})
	b.add(events.ResponseReceived{
		Meta:  b.meta(),
		Bead:  demoBeadA,
		Round: 1,
		Agent: demoEng1,
		Report: &domain.ImplReport{
			Summary:      "retry with exponential backoff, capped at three attempts",
			FilesChanged: []string{"internal/http/client.go", "internal/http/client_test.go"},
			HowToVerify:  "go test ./internal/http/...",
			Commit:       "1f0c4d9a5b6e7f8091a2b3c4d5e6f708192a3b4c",
		},
	})
	b.add(events.BeadStateChanged{Meta: b.meta(), Bead: demoBeadA, From: domain.BeadStateInProgress, To: domain.BeadStateInReview, Round: 1})
	b.add(events.RoundStarted{Meta: b.meta(), Bead: demoBeadA, Round: 1, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensQA, domain.LensUX}, Commit: "1f0c4d9a5b6e7f8091a2b3c4d5e6f708192a3b4c"})
	b.spawn(demoTestQA, domain.RoleTester, domain.LensQA, demoBeadA, beadA.Title, events.TaskReview, 1)
	b.spawn(demoTestUX, domain.RoleTester, domain.LensUX, demoBeadA, beadA.Title, events.TaskReview, 1)
	b.add(events.AgentFinished{Meta: b.meta(), Agent: demoTestUX, Role: domain.RoleTester, Bead: demoBeadA, Duration: 41 * time.Second, Produced: domain.MessageVerdict})
	b.add(events.VerdictReceived{Meta: b.meta(), Bead: demoBeadA, Round: 1, Agent: demoTestUX, Verdict: domain.Verdict{
		Lens: domain.LensUX, Round: 1, Outcome: domain.VerdictApprove, Summary: "error messages read well",
	}})
	b.add(events.AgentFinished{Meta: b.meta(), Agent: demoTestQA, Role: domain.RoleTester, Bead: demoBeadA, Duration: 58 * time.Second, Produced: domain.MessageVerdict})
	b.add(events.VerdictReceived{Meta: b.meta(), Bead: demoBeadA, Round: 1, Agent: demoTestQA, Verdict: domain.Verdict{
		Lens: domain.LensQA, Round: 1, Outcome: domain.VerdictChanges, Summary: "backoff is not jittered and the cap is off by one",
		Items: []domain.FeedbackItem{
			{ID: "qa-1", Severity: domain.SeverityMajor, Title: "off-by-one in the attempt cap", Detail: "four requests are sent, not three", File: "internal/http/client.go", Line: 88, Status: domain.ItemOpen},
			{ID: "qa-2", Severity: domain.SeverityNit, Title: "add jitter to the backoff", Status: domain.ItemOpen},
		},
	}})
	b.add(events.BeadStateChanged{Meta: b.meta(), Bead: demoBeadA, From: domain.BeadStateInReview, To: domain.BeadStateChangesRequested, Round: 1, Reason: "qa requested changes"})

	// Bead A, round 2: fix, pushback, arbitration, approval.
	b.spawn(demoEng2, domain.RoleEngineer, "", demoBeadA, beadA.Title, events.TaskAddressFeedback, 2)
	b.add(events.ResponseReceived{Meta: b.meta(), Bead: demoBeadA, Round: 2, Agent: demoEng2, Responses: []domain.Response{
		{ItemID: "qa-1", Action: domain.ActionFixed, Rationale: "the loop counter now starts at one"},
		{ItemID: "qa-2", Action: domain.ActionPushback, Rationale: "jitter belongs to the caller's policy, not the client"},
	}})
	b.add(events.AgentFinished{Meta: b.meta(), Agent: demoEng2, Role: domain.RoleEngineer, Bead: demoBeadA, Duration: 74 * time.Second, Produced: domain.MessageResponse})
	b.add(events.ArbitrationReceived{Meta: b.meta(), Bead: demoBeadA, Round: 2, Agent: demoPO, Rulings: []domain.Arbitration{
		{ItemID: "qa-2", Ruling: domain.RulingUpholdEngineer, Rationale: "out of scope for this bead", Instruction: "open a follow-up bead"},
	}})
	b.add(events.Warning{Meta: b.meta(), Source: "gates", Message: "gh not found, pull-request gates will be polled manually"})
	b.add(events.BeadUpdated{Meta: b.meta(), Bead: withRound(beadA, domain.BeadStateInReview, 2), Fields: []string{"round", "state"}})
	b.add(events.RoundStarted{Meta: b.meta(), Bead: demoBeadA, Round: 2, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensQA}, Commit: "2b7d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f"})
	b.spawn(demoTestQA2, domain.RoleTester, domain.LensQA, demoBeadA, beadA.Title, events.TaskReview, 2)
	b.add(events.AgentFinished{Meta: b.meta(), Agent: demoTestQA2, Role: domain.RoleTester, Bead: demoBeadA, Duration: 33 * time.Second, Produced: domain.MessageVerdict})
	b.add(events.VerdictReceived{Meta: b.meta(), Bead: demoBeadA, Round: 2, Agent: demoTestQA2, Verdict: domain.Verdict{
		Lens: domain.LensQA, Round: 2, Outcome: domain.VerdictApprove, Summary: "cap is right now",
	}})
	b.add(events.BeadStateChanged{Meta: b.meta(), Bead: demoBeadA, From: domain.BeadStateInReview, To: domain.BeadStateApproved, Round: 2, Reason: "all testers approved"})
	b.add(events.BeadStateChanged{Meta: b.meta(), Bead: demoBeadA, From: domain.BeadStateApproved, To: domain.BeadStateDelivering, Round: 2})
	b.add(events.PROpened{Meta: b.meta(), Bead: demoBeadA, URL: "https://example.invalid/demo/pull/42", Number: 42, Branch: "sc/demo-2-retry-failed-http-requests", Base: "main"})
	b.add(events.Merged{Meta: b.meta(), Bead: demoBeadA, SHA: "2b7d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f", Branch: "main", PRNumber: 42, Pushed: true})
	b.add(events.BeadStateChanged{Meta: b.meta(), Bead: demoBeadA, From: domain.BeadStateDelivering, To: domain.BeadStateClosed, Round: 2, Reason: "merged"})

	// Bead B: the engineer crashes, the run pauses on a cost cap, resumes,
	// and the user stops it.
	b.spawn(demoEngB, domain.RoleEngineer, "", demoBeadB, beadB.Title, events.TaskImplement, 1)
	b.add(events.AgentFailed{Meta: b.meta(), Agent: demoEngB, Role: domain.RoleEngineer, Bead: demoBeadB, Failure: events.FailureTimeout, Err: "agent timeout after 15m", Duration: 15 * time.Minute, Attempt: 1, WillRetry: true})
	b.add(events.Error{Meta: b.meta(), Source: "supervisor", Message: "engineer timed out twice on " + string(demoBeadB), Bead: demoBeadB, Agent: demoEngB})
	b.add(events.ArbitrationReceived{Meta: b.meta(), Bead: demoBeadB, Round: 1, Agent: demoPO, Escalation: &domain.Escalation{
		Outcome: domain.EscalationBlock, Summary: "the docs bead needs a human", Notes: "the engineer cannot reach the docs site",
	}})
	b.add(events.BeadStateChanged{Meta: b.meta(), Bead: demoBeadB, From: domain.BeadStateInProgress, To: domain.BeadStateNeedsHuman, Round: 1, Reason: "escalated by the product owner"})
	b.add(events.RunPaused{Meta: b.meta(), Reason: "cost_cap"})
	b.add(events.RunResumed{Meta: b.meta(), Reason: "cost_cap raised"})
	b.add(events.RunStopped{Meta: b.meta(), Reason: "key"})
	b.add(events.RunFinished{Meta: b.meta(), Reason: events.FinishStopped, Summary: "1 bead delivered, 1 left open", Duration: b.at.Sub(o.Start)})

	return b.out
}

// DemoEvents returns just the events of [DemoScript], without their pacing.
// `sc tui-demo --events` prints them as JSONL, which is also the recorded
// stream later golden tests can replay.
func DemoEvents(opts DemoOptions) []events.Event {
	script := DemoScript(opts)
	out := make([]events.Event, len(script))
	for i, s := range script {
		out[i] = s.Event
	}
	return out
}

// PlayScript publishes the script to the bus, waiting each event's delay
// first. It returns when the script is exhausted or ctx is done; it does not
// close the bus, so the TUI keeps rendering the finished run.
func PlayScript(ctx context.Context, bus *events.Bus, script []ScriptedEvent) error {
	return playScript(ctx, bus, script, nil)
}

// playScript is [PlayScript] with a hook called with each event just before
// it is published. [Demo] uses it to move the simulated clock on, so a frame
// drawn for an event reads the time that event happened at.
func playScript(ctx context.Context, bus *events.Bus, script []ScriptedEvent, before func(events.Event)) error {
	if bus == nil {
		return nil
	}
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	for _, s := range script {
		if s.Delay > 0 {
			timer.Reset(s.Delay)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return ctx.Err()
			case <-timer.C:
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if before != nil {
			before(s.Event)
		}
		bus.Publish(s.Event)
	}
	return nil
}

// Demo runs the TUI against the scripted stream: it creates a bus, plays the
// script into it in the background, and renders it. opts.Bus, opts.Run and
// opts.Now are filled in from demo; everything else in opts (output, input,
// colour) is the caller's. It returns when the user quits or ctx is
// cancelled.
func Demo(ctx context.Context, demo DemoOptions, opts Options) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	bus := events.NewBus()
	defer bus.Close()
	clock := demoClockFor(demo, &opts, bus)

	// The program is built (and so subscribed) before the script starts, or
	// a fast script would be published into a bus nobody is listening to.
	prog := NewProgram(ctx, opts)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = playScript(ctx, bus, DemoScript(demo), clock.at)
	}()

	err := runProgram(ctx, prog)
	cancel()
	<-done
	return err
}

// demoClockFor fills in the parts of opts the scripted run owns — the bus,
// the run and the simulated clock — and returns the clock, whose at method
// is the hook [playScript] moves it on with.
//
// Without the clock the views would measure a live agent's elapsed time from
// the script's fixed start date (2026-01-01) to the wall clock, and a demo
// card would read `5885h30m57s`. The clock keeps the whole frame inside the
// scripted timeline; a caller that already has one keeps it.
func demoClockFor(demo DemoOptions, opts *Options, bus *events.Bus) *demoClock {
	opts.Bus = bus
	opts.Run = DemoRun(demo)
	clock := newDemoClock(demo.withDefaults().Start)
	if opts.Now == nil {
		opts.Now = clock.Now
	}
	return clock
}

// demoClock is the simulated clock of a scripted run: it reports the
// timestamp of the last event published, so elapsed times are measured in
// the run's own timeline rather than against the wall clock. It is read by
// the render loop while the script writes it, so it locks, and it never goes
// backwards, whatever pacing the script is played at.
type demoClock struct {
	mu sync.Mutex
	t  time.Time
}

// newDemoClock returns a clock reading start.
func newDemoClock(start time.Time) *demoClock { return &demoClock{t: start} }

// Now returns the simulated time. It matches [Options.Now].
func (c *demoClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// at moves the clock to ev's timestamp, unless that is in the past.
func (c *demoClock) at(ev events.Event) {
	t := ev.At()
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.After(c.t) {
		c.t = t
	}
}

// demoBuilder stamps each scripted event with the next simulated timestamp
// and the uniform playback delay.
type demoBuilder struct {
	opts DemoOptions
	at   time.Time
	out  []ScriptedEvent
}

// meta returns the Meta for the next event and advances the clock.
func (b *demoBuilder) meta() events.Meta {
	m := events.MetaAt(b.opts.RunID, b.at)
	b.at = b.at.Add(b.opts.Step)
	return m
}

// add appends an event, with no delay before the very first one so the
// header is populated as soon as the program starts.
func (b *demoBuilder) add(ev events.Event) {
	delay := b.opts.Delay
	if len(b.out) == 0 {
		delay = 0
	}
	b.out = append(b.out, ScriptedEvent{Delay: delay, Event: ev})
}

// spawn appends an [events.AgentSpawned] whose agent record is stamped with
// the same simulated time as the event itself.
func (b *demoBuilder) spawn(id domain.AgentID, role domain.Role, lens domain.Lens, bead domain.BeadID, title string, task events.Task, round int) {
	at := b.at
	b.add(events.AgentSpawned{
		Meta: b.meta(),
		Agent: domain.Agent{
			ID:        id,
			Role:      role,
			Lens:      lens,
			Cartridge: "claude",
			Model:     "fable",
			Bead:      bead,
			State:     domain.AgentStateRunning,
			StartedAt: at,
		},
		Task:      task,
		BeadTitle: title,
		Round:     round,
		Attempt:   1,
	})
}

// withRound copies a bead with a new state and round.
func withRound(b domain.Bead, state domain.BeadState, round int) domain.Bead {
	b.State, b.Round = state, round
	return b
}
