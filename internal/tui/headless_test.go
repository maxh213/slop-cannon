package tui_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/tui"
)

// headlessGoldenDir is where the headless goldens live, relative to this
// package.
const headlessGoldenDir = "../../testdata/tui"

// headlessRunID is the run id of the recorded run below. It is fixed so the
// goldens are byte-for-byte reproducible.
const headlessRunID domain.RunID = "sc-20260101-120000"

// headlessStart is the recorded run's first event time, in UTC; the golden
// tests render in UTC so the clock stamps never depend on the machine.
var headlessStart = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// headlessGolden compares got with testdata/tui/<name>.golden. Set
// UPDATE_GOLDEN=1 (make update-golden) to rewrite the file instead.
func headlessGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(headlessGoldenDir, name+".golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "missing golden %s; run `make update-golden`", path)
	require.Equal(t, string(want), got, "output differs from %s; run `make update-golden` if the change is intended", path)
}

// headlessClock hands out the recorded run's timestamps, three seconds apart.
type headlessClock struct{ at time.Time }

// meta stamps the next event and advances the clock.
func (c *headlessClock) meta() events.Meta {
	m := events.MetaAt(headlessRunID, c.at)
	c.at = c.at.Add(3 * time.Second)
	return m
}

// headlessRecording is a recorded run covering every event kind: a plan, one
// bead through a full feedback loop to a merged pull request, and a second
// bead that times out, is escalated and leaves the run stopped by the user.
func headlessRecording() []events.Event {
	c := &headlessClock{at: headlessStart}
	run := domain.Run{
		ID:        headlessRunID,
		RepoPath:  "/repos/example",
		StateDir:  "/state/example/runs/" + string(headlessRunID),
		Mode:      domain.RunModeBrief,
		Brief:     "add retries and document them",
		StartedAt: headlessStart,
		Config: domain.ConfigSnapshot{
			Ammo: 10, Safety: true, Cartridge: "claude", Model: "fable",
			ReviewPRs: true, Headless: true,
		},
	}
	const (
		epic  domain.BeadID  = "sc-1"
		beadA domain.BeadID  = "sc-2"
		beadB domain.BeadID  = "sc-3"
		poID  domain.AgentID = "po-1"
		eng1  domain.AgentID = "eng-sc-2-r1"
		eng2  domain.AgentID = "eng-sc-2-r2"
		engB  domain.AgentID = "eng-sc-3-r1"
		qa1   domain.AgentID = "tester-qa-sc-2-r1"

		commit1 = "1f0c4d9a5b6e7f8091a2b3c4d5e6f708192a3b4c"
		commit2 = "2b7d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f"
	)

	beadOne := domain.Bead{
		ID: beadA, Title: "Add retry", Acceptance: "requests retry three times",
		Type: domain.BeadTypeFeature, Priority: domain.PriorityDefault, Size: domain.SizeM,
		State: domain.BeadStateOpen,
	}
	beadTwo := domain.Bead{
		ID: beadB, Title: "Document the retry policy", Acceptance: "README explains the backoff",
		Type: domain.BeadTypeChore, Priority: 3, Size: domain.SizeS,
		State: domain.BeadStateOpen, Deps: []domain.BeadID{beadA},
	}
	inReview := beadOne
	inReview.State, inReview.Round = domain.BeadStateInReview, 2

	return []events.Event{
		events.RunStarted{Meta: c.meta(), Info: run},
		events.AgentSpawned{
			Meta: c.meta(),
			Agent: domain.Agent{
				ID: poID, Role: domain.RolePO, Cartridge: "claude", Model: "fable",
				State: domain.AgentStateRunning, StartedAt: headlessStart,
			},
			Task: events.TaskPlan, Attempt: 1,
		},
		events.AgentOutput{Meta: c.meta(), Agent: poID, Stream: events.StreamStdout, Line: "reading the repository"},
		events.AgentFinished{Meta: c.meta(), Agent: poID, Role: domain.RolePO, Duration: 12 * time.Second, Produced: domain.MessagePlan},
		events.PlanReviewed{Meta: c.meta(), Planner: poID, Candidate: "p2:retry", Decision: domain.PlanReviewDuplicate,
			Canonical: "p1:retry", Provenance: domain.PlanProvenanceDeterministic},
		events.PlanProduced{
			Meta: c.meta(), Agent: poID, Epic: epic,
			Plan: domain.Plan{Brief: run.Brief, Beads: []domain.PlanBead{
				{Key: "retry", Title: "Add retry", Acceptance: "requests retry three times", Type: domain.BeadTypeFeature, Priority: domain.PriorityDefault, Size: domain.SizeM},
				{Key: "docs", Title: "Document the retry policy", Acceptance: "README explains the backoff", Type: domain.BeadTypeChore, Priority: 3, Size: domain.SizeS, Deps: []string{"retry"}},
			}},
			Created: map[string]domain.BeadID{"retry": beadA, "docs": beadB},
		},
		events.BeadCreated{Meta: c.meta(), Bead: beadOne},
		events.BeadCreated{Meta: c.meta(), Bead: beadTwo},
		events.GraphChanged{Meta: c.meta(), Epic: epic, Beads: []domain.Bead{beadOne, beadTwo}},
		events.BeadStateChanged{
			Meta: c.meta(), Bead: beadA, From: domain.BeadStateOpen, To: domain.BeadStateInProgress,
			Round: 1, Reason: "claimed by " + string(eng1),
		},
		events.AgentSpawned{
			Meta: c.meta(),
			Agent: domain.Agent{
				ID: eng1, Role: domain.RoleEngineer, Cartridge: "claude", Model: "fable", Bead: beadA,
				State: domain.AgentStateRunning, StartedAt: headlessStart,
			},
			Task: events.TaskImplement, BeadTitle: beadOne.Title, Round: 1, Attempt: 1,
		},
		events.ResponseReceived{
			Meta: c.meta(), Bead: beadA, Round: 1, Agent: eng1,
			Report: &domain.ImplReport{
				Summary:      "retry with exponential backoff",
				FilesChanged: []string{"internal/http/client.go", "internal/http/client_test.go"},
				HowToVerify:  "go test ./internal/http/...",
				Commit:       commit1,
			},
		},
		events.AgentFinished{Meta: c.meta(), Agent: eng1, Role: domain.RoleEngineer, Bead: beadA, Duration: 96 * time.Second, Produced: domain.MessageImplReport},
		events.RoundStarted{
			Meta: c.meta(), Bead: beadA, Round: 1, RoundKind: events.RoundReview,
			Lenses: []domain.Lens{domain.LensQA, domain.LensUX}, Commit: commit1,
		},
		events.VerdictReceived{Meta: c.meta(), Bead: beadA, Round: 1, Agent: qa1, Verdict: domain.Verdict{
			Lens: domain.LensQA, Round: 1, Outcome: domain.VerdictChanges, Summary: "the cap is off by one",
			Items: []domain.FeedbackItem{
				{ID: "qa-1", Severity: domain.SeverityMajor, Title: "off-by-one in the attempt cap", File: "internal/http/client.go", Line: 88, Status: domain.ItemOpen},
				{ID: "qa-2", Severity: domain.SeverityNit, Title: "add jitter to the backoff", Status: domain.ItemOpen},
			},
		}},
		events.BeadStateChanged{Meta: c.meta(), Bead: beadA, From: domain.BeadStateInReview, To: domain.BeadStateChangesRequested, Round: 1, Reason: "qa requested changes"},
		events.AgentSpawned{
			Meta: c.meta(),
			Agent: domain.Agent{
				ID: eng2, Role: domain.RoleEngineer, Cartridge: "claude", Model: "fable", Bead: beadA,
				State: domain.AgentStateRunning, StartedAt: headlessStart,
			},
			Task: events.TaskAddressFeedback, BeadTitle: beadOne.Title, Round: 2, Attempt: 2,
		},
		events.ResponseReceived{Meta: c.meta(), Bead: beadA, Round: 2, Agent: eng2, Responses: []domain.Response{
			{ItemID: "qa-1", Action: domain.ActionFixed, Rationale: "the counter starts at one now"},
			{ItemID: "qa-2", Action: domain.ActionPushback, Rationale: "jitter belongs to the caller"},
		}},
		events.ArbitrationReceived{Meta: c.meta(), Bead: beadA, Round: 2, Agent: poID, Rulings: []domain.Arbitration{
			{ItemID: "qa-2", Ruling: domain.RulingUpholdEngineer, Rationale: "out of scope", Instruction: "open a follow-up bead"},
		}},
		events.BeadUpdated{Meta: c.meta(), Bead: inReview, Fields: []string{"round", "state"}},
		events.VerdictReceived{Meta: c.meta(), Bead: beadA, Round: 2, Agent: qa1, Verdict: domain.Verdict{
			Lens: domain.LensQA, Round: 2, Outcome: domain.VerdictApprove, Summary: "the cap is right now",
		}},
		events.PROpened{
			Meta: c.meta(), Bead: beadA, URL: "https://example.invalid/example/pull/42", Number: 42,
			Branch: "sc/sc-2-add-retry", Base: "main",
		},
		events.Merged{Meta: c.meta(), Bead: beadA, SHA: commit2, Branch: "main", PRNumber: 42, Pushed: true},
		events.Warning{Meta: c.meta(), Source: "gates", Message: "gh not found, pull-request gates are polled manually"},
		events.AgentFailed{
			Meta: c.meta(), Agent: engB, Role: domain.RoleEngineer, Bead: beadB, Failure: events.FailureTimeout,
			Err: "agent timeout after 15m", Duration: 15 * time.Minute, ExitCode: -1, Attempt: 1, WillRetry: true,
		},
		events.Error{Meta: c.meta(), Source: "supervisor", Message: "engineer timed out twice", Bead: beadB, Agent: engB},
		events.ArbitrationReceived{Meta: c.meta(), Bead: beadB, Round: 1, Agent: poID, Escalation: &domain.Escalation{
			Outcome: domain.EscalationBlock, Summary: "the docs bead needs a human", Notes: "the docs site is unreachable",
		}},
		events.RunPaused{Meta: c.meta(), Reason: "cost_cap"},
		events.RunResumed{Meta: c.meta(), Reason: "cost_cap raised"},
		events.RunStopped{Meta: c.meta(), Reason: "key"},
		events.RunFinished{
			Meta: c.meta(), Reason: events.FinishStopped,
			Summary: "1 bead delivered, 1 needs a human", Duration: 4 * time.Minute,
		},
	}
}

// headlessRender writes evs through a renderer built with opts (Out and
// Location are filled in) and returns the output and the renderer.
func headlessRender(t *testing.T, evs []events.Event, opts tui.HeadlessOptions) (string, *tui.Headless) {
	t.Helper()
	var buf bytes.Buffer
	opts.Out = &buf
	if opts.Location == nil {
		opts.Location = time.UTC
	}
	h := tui.NewHeadless(opts)
	for _, ev := range evs {
		require.NoError(t, h.Write(ev))
	}
	return buf.String(), h
}

func TestHeadlessGolden(t *testing.T) {
	out, h := headlessRender(t, headlessRecording(), tui.HeadlessOptions{})
	headlessGolden(t, "headless", out)
	require.Equal(t, uint64(len(headlessRecording())), h.Written())
	require.NoError(t, h.Err())
}

func TestHeadlessJSONGolden(t *testing.T) {
	recording := headlessRecording()
	out, h := headlessRender(t, recording, tui.HeadlessOptions{JSON: true})
	headlessGolden(t, "headless-json", out)

	// The JSON mode is the recorded stream: it must read back as the very
	// events that went in.
	back, err := events.ReadAll(strings.NewReader(out))
	require.NoError(t, err)
	require.Equal(t, recording, back)
	require.Equal(t, tui.ExitOK, h.ExitCode())
}

func TestHeadlessRecordingCoversEveryKind(t *testing.T) {
	seen := make(map[events.Kind]bool)
	for _, ev := range headlessRecording() {
		seen[ev.Kind()] = true
	}
	for _, k := range events.Kinds() {
		require.True(t, seen[k], "the recorded run has no %s event", k)
	}
}

func TestHeadlessLinesAreSingleAndPlain(t *testing.T) {
	for _, ev := range headlessRecording() {
		line := tui.FormatHeadless(ev, time.UTC)
		require.NotContains(t, line, "\n", "%s spans more than one line", ev.Kind())
		require.NotContains(t, line, "\x1b", "%s contains an ANSI escape", ev.Kind())
		require.Regexp(t, `^\d\d:\d\d:\d\d \S `, line, "%s has no clock stamp and glyph", ev.Kind())
		body := tui.HeadlessLine(ev)
		require.NotEmpty(t, body, "%s renders an empty line", ev.Kind())
	}
	require.Empty(t, tui.FormatHeadless(nil, time.UTC))
}

func TestHeadlessFiredLineMatchesTheSpec(t *testing.T) {
	ev := events.AgentSpawned{
		Meta: events.MetaAt(headlessRunID, headlessStart.Add(63*time.Second)),
		Agent: domain.Agent{
			ID: "eng-1", Role: domain.RoleEngineer, Cartridge: "claude", Model: "fable",
			Bead: "bd-1a2b", State: domain.AgentStateRunning,
		},
		Task: events.TaskImplement, BeadTitle: "Add retry", Round: 1, Attempt: 1,
	}
	line := tui.FormatHeadless(ev, time.UTC)
	require.True(t, strings.HasPrefix(line, `12:01:03 ▶ fired engineer(claude/fable) at bd-1a2b "Add retry"`), line)
	require.Equal(t,
		`12:01:03 ▶ fired engineer(claude/fable) at bd-1a2b "Add retry" [implement, round 1, agent eng-1]`,
		line)

	// A retry says so, and a tester carries its lens.
	ev.Attempt = 3
	require.Contains(t, tui.HeadlessLine(ev), "attempt 3")
	ev.Agent.Role, ev.Agent.Lens, ev.Task = domain.RoleTester, domain.LensQA, events.TaskReview
	require.Contains(t, tui.HeadlessLine(ev), "fired tester/qa(claude/fable) at bd-1a2b")

	// A planning PO has no bead to be fired at.
	po := events.AgentSpawned{
		Meta:  events.MetaAt(headlessRunID, headlessStart),
		Agent: domain.Agent{ID: "po-1", Role: domain.RolePO, Cartridge: "claude", Model: "fable"},
		Task:  events.TaskPlan,
	}
	require.Equal(t, "fired po(claude/fable) [plan, agent po-1]", tui.HeadlessLine(po))
}

func TestHeadlessLocalTimeByDefault(t *testing.T) {
	at := time.Date(2026, 1, 1, 12, 1, 3, 0, time.UTC)
	ev := events.RunStopped{Meta: events.MetaAt(headlessRunID, at), Reason: "key"}

	// A nil location means time.Local, whatever zone the machine is in —
	// never the event's own zone, which is always UTC.
	require.Equal(t, at.In(time.Local).Format("15:04:05")+" ■ run stopping (key)", tui.FormatHeadless(ev, nil))

	// An explicit location is honoured, so the same instant stamps
	// differently in different zones. These are fixed offsets rather than
	// named zones so the assertion holds without a tzdata database and does
	// not move with the machine's own TZ.
	for _, offset := range []int{-8 * 3600, 0, 5*3600 + 1800, 9 * 3600} {
		loc := time.FixedZone("headless-test", offset)
		require.Equalf(t, at.In(loc).Format("15:04:05")+" ■ run stopping (key)",
			tui.FormatHeadless(ev, loc), "offset %ds", offset)
	}
}

func TestExitCodeFor(t *testing.T) {
	for _, tc := range []struct {
		reason events.FinishReason
		want   int
	}{
		{events.FinishCompleted, tui.ExitOK},
		{events.FinishStopped, tui.ExitOK},
		{events.FinishFailed, tui.ExitFailed},
		{events.FinishCreditsExhausted, tui.ExitCreditsExhausted},
		{events.FinishReason("something new"), tui.ExitFailed},
		{"", tui.ExitFailed},
	} {
		require.Equal(t, tc.want, tui.ExitCodeFor(tc.reason), "reason %q", tc.reason)
	}
}

func TestHeadlessExitCodeFollowsTheFinishReason(t *testing.T) {
	for _, tc := range []struct {
		reason events.FinishReason
		want   int
	}{
		{events.FinishCompleted, tui.ExitOK},
		{events.FinishStopped, tui.ExitOK},
		{events.FinishCreditsExhausted, tui.ExitCreditsExhausted},
		{events.FinishFailed, tui.ExitFailed},
	} {
		fin := events.RunFinished{Meta: events.MetaAt(headlessRunID, headlessStart), Reason: tc.reason, Summary: "done"}
		out, h := headlessRender(t, []events.Event{fin}, tui.HeadlessOptions{})
		require.Equal(t, tc.want, h.ExitCode(), "reason %q", tc.reason)
		got, ok := h.Finish()
		require.True(t, ok)
		require.Equal(t, tc.reason, got.Reason)
		require.Contains(t, out, string(tc.reason))
		require.Contains(t, out, "(exit "+strconv.Itoa(tc.want)+")")
	}

	// A stream that never carried a RunFinished failed, whatever else it said.
	_, h := headlessRender(t, []events.Event{
		events.RunStarted{Meta: events.MetaAt(headlessRunID, headlessStart)},
		events.RunStopped{Meta: events.MetaAt(headlessRunID, headlessStart), Reason: "SIGINT"},
	}, tui.HeadlessOptions{})
	require.Equal(t, tui.ExitFailed, h.ExitCode())
	_, ok := h.Finish()
	require.False(t, ok)

	// The exit code survives a filter that hides the finish line.
	_, hidden := headlessRender(t, []events.Event{
		events.RunFinished{Meta: events.MetaAt(headlessRunID, headlessStart), Reason: events.FinishCreditsExhausted},
	}, tui.HeadlessOptions{Filter: func(events.Event) bool { return false }})
	require.Equal(t, tui.ExitCreditsExhausted, hidden.ExitCode())
	require.Equal(t, uint64(0), hidden.Written())
}

func TestHeadlessFilter(t *testing.T) {
	out, h := headlessRender(t, headlessRecording(), tui.HeadlessOptions{
		Filter: func(ev events.Event) bool { return ev.Kind() != events.KindAgentOutput },
	})
	require.NotContains(t, out, "reading the repository")
	require.Contains(t, out, "fired po(claude/fable) [plan, agent po-1]")
	require.Equal(t, uint64(len(headlessRecording())-1), h.Written())
}

func TestHeadlessFlushesEveryEvent(t *testing.T) {
	var sink bytes.Buffer
	bw := bufio.NewWriterSize(&sink, 4096)
	h := tui.NewHeadless(tui.HeadlessOptions{Out: bw, Location: time.UTC})
	for i, ev := range headlessRecording() {
		require.NoError(t, h.Write(ev))
		require.Equal(t, i+1, strings.Count(sink.String(), "\n"),
			"event %d is still sitting in the buffer", i+1)
	}
}

// headlessFailingWriter fails every write with its own error.
type headlessFailingWriter struct{ err error }

// Write implements io.Writer.
func (w headlessFailingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestHeadlessWriteErrorIsSticky(t *testing.T) {
	boom := errors.New("disk full")
	h := tui.NewHeadless(tui.HeadlessOptions{Out: headlessFailingWriter{err: boom}, Location: time.UTC})
	first := h.Write(events.RunStopped{Meta: events.MetaAt(headlessRunID, headlessStart), Reason: "key"})
	require.ErrorIs(t, first, boom)
	second := h.Write(events.RunPaused{Meta: events.MetaAt(headlessRunID, headlessStart), Reason: "user"})
	require.Equal(t, first, second)
	require.Equal(t, first, h.Err())
	require.Equal(t, uint64(0), h.Written())
}

func TestHeadlessStream(t *testing.T) {
	recording := headlessRecording()
	ch := make(chan events.Event, len(recording))
	for _, ev := range recording {
		ch <- ev
	}
	close(ch)

	var buf bytes.Buffer
	h := tui.NewHeadless(tui.HeadlessOptions{Out: &buf, Location: time.UTC})
	require.NoError(t, h.Stream(t.Context(), ch))
	require.Equal(t, uint64(len(recording)), h.Written())
	require.Equal(t, tui.ExitOK, h.ExitCode())
}

func TestHeadlessStreamStopsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	ch := make(chan events.Event)
	h := tui.NewHeadless(tui.HeadlessOptions{Out: &bytes.Buffer{}, Location: time.UTC})
	done := make(chan error, 1)
	go func() { done <- h.Stream(ctx, ch) }()
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestHeadlessServe(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()

	var buf bytes.Buffer
	h := tui.NewHeadless(tui.HeadlessOptions{Out: &buf, Location: time.UTC})
	done := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		close(ready)
		done <- h.Serve(t.Context(), bus, 64)
	}()
	<-ready
	// Subscribing happens inside Serve, so publish until the renderer has
	// seen something rather than racing it.
	fin := events.RunFinished{Meta: events.MetaAt(headlessRunID, headlessStart), Reason: events.FinishCompleted, Summary: "all beads settled"}
	require.Eventually(t, func() bool {
		bus.Publish(fin)
		return h.Written() > 0
	}, 2*time.Second, 5*time.Millisecond)

	bus.Close()
	require.NoError(t, <-done)
	require.Contains(t, buf.String(), "run finished: completed (exit 0) — all beads settled")
	require.Equal(t, tui.ExitOK, h.ExitCode())
	require.Nil(t, headlessServeNil(t))
}

// headlessServeNil checks that a nil bus is a no-op rather than a panic.
func headlessServeNil(t *testing.T) error {
	t.Helper()
	h := tui.NewHeadless(tui.HeadlessOptions{Out: &bytes.Buffer{}})
	return h.Serve(t.Context(), nil, 0)
}

// headlessSafeWriter is a buffer a test can read while the renderer writes
// to it from another goroutine.
type headlessSafeWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write implements io.Writer.
func (w *headlessSafeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// String returns everything written so far.
func (w *headlessSafeWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// headlessGatedWriter blocks its first write until release is closed, so a
// test can overrun a subscription's buffer deterministically.
type headlessGatedWriter struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

// Write implements io.Writer, holding the very first call until released.
func (w *headlessGatedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() {
		close(w.entered)
		<-w.release
	})
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// String returns everything written so far.
func (w *headlessGatedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestHeadlessServeReportsDroppedEvents(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()

	out := &headlessGatedWriter{entered: make(chan struct{}), release: make(chan struct{})}
	h := tui.NewHeadless(tui.HeadlessOptions{Out: out, Location: time.UTC})
	done := make(chan error, 1)
	go func() { done <- h.Serve(t.Context(), bus, 2) }()

	// Publish until the renderer is inside its first (blocked) write.
	ev := events.RunPaused{Meta: events.MetaAt(headlessRunID, headlessStart), Reason: "cost_cap"}
	for {
		bus.Publish(ev)
		select {
		case <-out.entered:
		default:
			continue
		}
		break
	}
	// The reader is parked, so this burst overruns the two-event buffer.
	for range 50 {
		bus.Publish(ev)
	}
	close(out.release)

	require.Eventually(t, func() bool {
		return strings.Contains(out.String(), "warning [headless] dropped")
	}, 2*time.Second, 5*time.Millisecond)
	bus.Close()
	require.NoError(t, <-done)
	require.Contains(t, out.String(), "the renderer fell behind")
}

func TestRunHeadless(t *testing.T) {
	recording := headlessRecording()
	ch := make(chan events.Event, len(recording))
	for _, ev := range recording {
		ch <- ev
	}
	close(ch)

	var buf bytes.Buffer
	code, err := tui.RunHeadless(t.Context(), tui.Options{Events: ch, Output: &buf}, tui.HeadlessOptions{Location: time.UTC})
	require.NoError(t, err)
	require.Equal(t, tui.ExitOK, code)
	require.Contains(t, buf.String(), "run finished: stopped after 4m0s (exit 0)")
}

func TestRunHeadlessFromABus(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()

	buf := &headlessSafeWriter{}
	type result struct {
		code int
		err  error
	}
	res := make(chan result, 1)
	go func() {
		code, err := tui.RunHeadless(t.Context(), tui.Options{Bus: bus, Output: buf, Buffer: 32}, tui.HeadlessOptions{Location: time.UTC})
		res <- result{code, err}
	}()

	fin := events.RunFinished{Meta: events.MetaAt(headlessRunID, headlessStart), Reason: events.FinishCreditsExhausted, Summary: "out of credits"}
	require.Eventually(t, func() bool {
		bus.Publish(fin)
		return strings.Contains(buf.String(), "credits_exhausted")
	}, 2*time.Second, 5*time.Millisecond)
	bus.Close()

	got := <-res
	require.NoError(t, got.err)
	require.Equal(t, tui.ExitCreditsExhausted, got.code)
}

func TestRunHeadlessCancelledContextIsNotAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	bus := events.NewBus()
	defer bus.Close()

	done := make(chan int, 1)
	go func() {
		code, err := tui.RunHeadless(ctx, tui.Options{Bus: bus, Output: &bytes.Buffer{}}, tui.HeadlessOptions{Location: time.UTC})
		require.NoError(t, err)
		done <- code
	}()
	cancel()
	require.Equal(t, tui.ExitFailed, <-done)
}

func TestRunHeadlessWithNoSource(t *testing.T) {
	code, err := tui.RunHeadless(t.Context(), tui.Options{Output: &bytes.Buffer{}}, tui.HeadlessOptions{})
	require.NoError(t, err)
	require.Equal(t, tui.ExitFailed, code)
}

// headlessOddEvent is an event kind this build does not know, to prove the
// renderer degrades to the kind name instead of dropping the line.
type headlessOddEvent struct{ events.Meta }

// Kind implements events.Event.
func (headlessOddEvent) Kind() events.Kind { return events.Kind("something_new") }

// headlessSparse is a recording of the same kinds with every optional field
// left at its zero value: the renderer must still produce one readable line
// each, with no stray punctuation or empty brackets.
func headlessSparse() []events.Event {
	c := &headlessClock{at: headlessStart}
	return []events.Event{
		events.RunStarted{Meta: c.meta(), Info: domain.Run{ID: headlessRunID}, Resumed: true},
		events.RunResumed{Meta: c.meta()},
		events.PlanProduced{Meta: c.meta(), Agent: "po-1"},
		events.BeadCreated{Meta: c.meta(), Bead: domain.Bead{ID: "sc-9"}},
		events.BeadUpdated{Meta: c.meta(), Bead: domain.Bead{ID: "sc-9"}},
		events.BeadStateChanged{Meta: c.meta(), Bead: "sc-9", To: domain.BeadStateClosed},
		events.GraphChanged{Meta: c.meta()},
		events.AgentSpawned{Meta: c.meta(), Agent: domain.Agent{ID: "a-1"}},
		events.AgentFinished{Meta: c.meta(), Agent: "a-1"},
		events.AgentFailed{Meta: c.meta(), Agent: "a-1", Failure: events.FailureBadOutput},
		events.RoundStarted{Meta: c.meta(), Bead: "sc-9", Round: 1},
		events.VerdictReceived{Meta: c.meta(), Bead: "sc-9", Round: 1, Agent: "t-1", Verdict: domain.Verdict{
			Outcome: domain.VerdictChanges,
			Items:   []domain.FeedbackItem{{ID: "x-1", Severity: domain.Severity("weird")}},
		}},
		events.ResponseReceived{Meta: c.meta(), Bead: "sc-9", Round: 1, Agent: "e-1"},
		events.ResponseReceived{Meta: c.meta(), Bead: "sc-9", Round: 1, Agent: "e-1", Responses: []domain.Response{
			{ItemID: "x-1", Action: domain.ResponseAction("shrug")},
		}},
		events.ArbitrationReceived{Meta: c.meta(), Bead: "sc-9", Round: 1, Agent: "po-1"},
		events.PROpened{Meta: c.meta(), Bead: "sc-9", Number: 7, URL: "https://example.invalid/x/pull/7", External: true},
		events.Merged{Meta: c.meta(), Bead: "sc-9"},
		events.Warning{Meta: c.meta(), Message: "something odd", Err: "context deadline exceeded"},
		headlessOddEvent{Meta: c.meta()},
	}
}

func TestHeadlessSparseGolden(t *testing.T) {
	out, h := headlessRender(t, headlessSparse(), tui.HeadlessOptions{})
	headlessGolden(t, "headless-sparse", out)
	require.NotContains(t, out, "()")
	require.NotContains(t, out, "[]")
	require.NotContains(t, out, "  ")
	// No RunFinished in this recording: the run did not end.
	require.Equal(t, tui.ExitFailed, h.ExitCode())
}

// TestGlyphColumnUnderEastAsianWidth re-runs the glyph-column assertions in a
// child process started with RUNEWIDTH_EASTASIAN=1, the setting a terminal
// configured for ambiguous-width 2 exports (P7-16). That is the mode where
// the assertions bite: there ○, ⧗, ◍, ◐, ·, ⊘, ◆, ▶ and § are two cells and
// ✓, ✔, ✗, !, →, ⤴ and │ are still one, so a view that keeps a glyph set of
// its own outside the palette shifts a column by a cell unless every glyph
// goes through [tui.PadGlyph].
//
// It has to be a second process: the width libraries latch the mode at
// package initialisation, so it cannot be turned on from inside a running
// test. The child is marked with themeEAChildEnv — the same marker
// [TestThemeGlyphColumnHoldsUnderEastAsianWidth] uses — because that is what
// tells this package's TestMain that this process wants the mode on purpose
// and must not be handed on to a child without it.
func TestGlyphColumnUnderEastAsianWidth(t *testing.T) {
	t.Parallel()

	if os.Getenv(themeEAChildEnv) != "" {
		t.Skip("already the re-executed child")
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], //nolint:gosec // re-runs this very test binary
		"-test.run", "^TestGlyphColumnHolds", "-test.count=1", "-test.timeout=5m")
	cmd.Env = append(os.Environ(), "RUNEWIDTH_EASTASIAN=1", themeEAChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "a view's own glyphs do not hold the palette's column with RUNEWIDTH_EASTASIAN=1:\n%s", out)
}

// TestHeadlessFinishSummaryReadsAsOneSentence pins P7-25. A finish summary
// is not always a sentence: gitx.CleanupResult names every worktree it kept
// on an indented line of its own, and the renderer's one-event-per-line
// promise used to fold that with a bare newline→space swap, leaving the
// report's own indentation in the middle of the line ("kept 1:   sc-2
// (in_progress)"). The whole run of whitespace is collapsed instead, which
// is what `sc status` does with the same text.
func TestHeadlessFinishSummaryReadsAsOneSentence(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 3, 4, 12, 1, 13, 0, time.UTC)
	line := tui.FormatHeadless(events.RunFinished{
		Meta:     events.MetaAt("sc-1", at),
		Reason:   events.FinishCompleted,
		Duration: 73 * time.Second,
		Summary:  "removed 1 worktree, kept 1:\n  sc-2 (in_progress)\n",
	}, time.UTC)

	require.Equal(t,
		"12:01:13 ◇ run finished: completed after 1m13s (exit 0) — removed 1 worktree, kept 1: sc-2 (in_progress)",
		line)
	require.NotContains(t, line, "  ", "the report's own indentation survived the fold")
	require.Len(t, strings.Split(line, "\n"), 1)

	// A summary that is already one sentence is untouched, doubled spaces
	// and all — the fold only collapses what a line break brought with it.
	for _, s := range []string{"nothing to clean up", "kept sc-2"} {
		require.Contains(t, tui.FormatHeadless(events.RunFinished{
			Meta: events.MetaAt("sc-1", at), Reason: events.FinishStopped, Summary: s,
		}, time.UTC), "— "+s)
	}

	// The other two free-prose summaries the renderer appends after an em
	// dash fold the same way.
	require.Contains(t, tui.FormatHeadless(events.VerdictReceived{
		Meta: events.MetaAt("sc-1", at), Bead: "sc-2", Round: 1, Agent: "t-1",
		Verdict: domain.Verdict{Outcome: domain.VerdictApprove, Summary: "looks good\n\n  ships it"},
	}, time.UTC), "— looks good ships it")
	require.Contains(t, tui.FormatHeadless(events.ArbitrationReceived{
		Meta: events.MetaAt("sc-1", at), Bead: "sc-2", Round: 5, Agent: "po-1",
		Escalation: &domain.Escalation{Outcome: domain.EscalationBlock, Summary: "a human should look\n\tat the retry"},
	}, time.UTC), "— a human should look at the retry")
}
