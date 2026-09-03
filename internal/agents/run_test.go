package agents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/cartridge/structured"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// runReport is a minimal impl_report the schema accepts.
var runReport = json.RawMessage(`{"summary":"Renamed the helper and updated its two callers."}`)

// runAgentIdentity is the agent every test in this file fires.
func runAgentIdentity() domain.Agent {
	return domain.Agent{
		ID:        domain.AgentID("eng-sc-1a2b-r1"),
		Role:      domain.RoleEngineer,
		Cartridge: mock.Name,
		Bead:      domain.BeadID("sc-1a2b"),
		State:     domain.AgentStateRunning,
	}
}

// runSpecFor builds a valid RunSpec in a throwaway workdir.
func runSpecFor(t *testing.T, policy cartridge.ToolPolicy) cartridge.RunSpec {
	t.Helper()
	return cartridge.RunSpec{
		Prompt:     "Implement the bead.",
		Workdir:    t.TempDir(),
		ToolPolicy: policy,
		Timeout:    time.Minute,
		Labels:     cartridge.LabelsFor("run-1", runAgentIdentity(), events.TaskImplement, 1),
	}
}

// runMockAdapter builds a mock adapter playing s under the standard mock
// name, which agents already knows how to enforce a tool policy for.
func runMockAdapter(t *testing.T, s *mock.Scenario) *mock.Adapter {
	t.Helper()
	a, err := mock.NewAdapter(mock.Name, s)
	require.NoError(t, err)
	return a
}

// runAgentRun assembles the standard firing for the tests below.
func runAgentRun(t *testing.T, c cartridge.Cartridge, bus *events.Bus) AgentRun {
	t.Helper()
	return AgentRun{
		Cartridge: c,
		Spec:      runSpecFor(t, cartridge.ToolReadWrite),
		Message:   domain.MessageImplReport,
		Agent:     runAgentIdentity(),
		Task:      events.TaskImplement,
		Run:       domain.RunID("run-1"),
		Bus:       bus,
	}
}

// runBusSink subscribes to bus and returns a function that ends the
// subscription and returns every AgentOutput event it saw.
func runBusSink(t *testing.T, bus *events.Bus) func() []events.AgentOutput {
	t.Helper()
	sub := bus.Subscribe(context.Background(), 512)
	return func() []events.AgentOutput {
		sub.Unsubscribe()
		var out []events.AgentOutput
		for ev := range sub.Events() {
			if o, ok := ev.(events.AgentOutput); ok {
				out = append(out, o)
			}
		}
		assert.Zero(t, sub.Dropped(), "the test subscription dropped events")
		return out
	}
}

func runLinesOn(evs []events.AgentOutput, stream events.Stream) []string {
	var out []string
	for _, ev := range evs {
		if ev.Stream == stream {
			out = append(out, ev.Line)
		}
	}
	return out
}

func TestRunAgentSucceedsOnTheFirstAttempt(t *testing.T) {
	t.Parallel()
	s := mock.NewScenario("report")
	s.Rule("implement", mock.Match{Role: "engineer"}).
		Say("Reading the bead.\nEditing helper.go\n").
		Tool("Edit", "t1", "helper.go").
		ToolResult("Edit", "t1", "1 change", false).
		Say("Done, no partial line").
		Structured(runReport).
		Usage(cartridge.Usage{InputTokens: 100, OutputTokens: 20})
	a := runMockAdapter(t, s)

	bus := events.NewBus()
	t.Cleanup(bus.Close)
	collect := runBusSink(t, bus)

	report, tr, err := RunAgent[domain.ImplReport](context.Background(), runAgentRun(t, a, bus))
	require.NoError(t, err)
	assert.Equal(t, "Renamed the helper and updated its two callers.", report.Summary)

	assert.Len(t, tr.Attempts, 1)
	assert.Equal(t, 0, tr.Retries())
	assert.False(t, tr.Failed())
	assert.Equal(t, "impl_report", tr.Contract)
	assert.Equal(t, mock.Name, tr.Cartridge)
	assert.Equal(t, domain.AgentID("eng-sc-1a2b-r1"), tr.Agent)
	assert.Equal(t, events.TaskImplement, tr.Task)
	assert.Equal(t, EnforceNative, tr.Enforcement.Mode)
	assert.Equal(t, cartridge.Usage{InputTokens: 100, OutputTokens: 20}, tr.Usage)
	assert.Contains(t, tr.Output, "sc-result")
	assert.NotEmpty(t, tr.Parsed.JSON)
	assert.Equal(t, "impl_report", tr.Parsed.Contract)
	assert.Zero(t, tr.Dropped)
	assert.Positive(t, tr.Duration)

	evs := collect()
	require.NotEmpty(t, evs)
	for _, ev := range evs {
		assert.Equal(t, domain.RunID("run-1"), ev.RunID())
		assert.Equal(t, domain.AgentID("eng-sc-1a2b-r1"), ev.Agent)
		assert.NotEmpty(t, ev.Line)
		assert.False(t, strings.Contains(ev.Line, "\n"), "a line event must hold exactly one line")
	}
	stdout := runLinesOn(evs, events.StreamStdout)
	assert.Equal(t, []string{"Reading the bead.", "Editing helper.go", "Done, no partial line"}, stdout[:3],
		"assistant text is published line by line, the last partial line included")
	stderr := runLinesOn(evs, events.StreamStderr)
	require.Len(t, stderr, 2)
	assert.Contains(t, stderr[0], "[tool Edit]")
	assert.Contains(t, stderr[1], "[result Edit ok]")

	// The transcript keeps exactly what was published.
	assert.Len(t, tr.Lines, len(evs))
	assert.Equal(t, tr.Text(), strings.Join(runLinesOfTranscript(tr), "\n"))
}

func runLinesOfTranscript(tr Transcript) []string {
	out := make([]string, len(tr.Lines))
	for i, l := range tr.Lines {
		out[i] = l.Text
	}
	return out
}

func TestRunAgentRetriesOnceOnASchemaFailure(t *testing.T) {
	t.Parallel()
	s := mock.NewScenario("retry")
	s.Rule("bad", mock.Match{}).Once().
		Say("Here you go.").
		Structured(json.RawMessage(`{"nope":1}`))
	s.Rule("good", mock.Match{}).
		Say("Sorry, corrected.").
		Structured(runReport)
	a := runMockAdapter(t, s)

	bus := events.NewBus()
	t.Cleanup(bus.Close)
	collect := runBusSink(t, bus)

	report, tr, err := RunAgent[domain.ImplReport](context.Background(), runAgentRun(t, a, bus))
	require.NoError(t, err)
	assert.Equal(t, "Renamed the helper and updated its two callers.", report.Summary)
	require.Len(t, tr.Attempts, 2)
	assert.Equal(t, 1, tr.Retries())
	assert.False(t, tr.Failed())

	first := tr.Attempts[0]
	require.Error(t, first.Err)
	assert.ErrorIs(t, first.Err, structured.ErrSchema)
	assert.Equal(t, events.FailureBadOutput, first.Failure)
	assert.Equal(t, 1, first.Number)
	assert.NoError(t, tr.Attempts[1].Err)

	// The retry prompt quotes the failure back to the model.
	assert.NotContains(t, first.Prompt, "Correction required")
	assert.Contains(t, tr.Attempts[1].Prompt, "Correction required")
	assert.Contains(t, tr.Attempts[1].Prompt, "impl_report")

	// And the retry is visible on the event stream.
	stderr := runLinesOn(collect(), events.StreamStderr)
	require.NotEmpty(t, stderr)
	joined := strings.Join(stderr, "\n")
	assert.Contains(t, joined, "[retry 2]")
	assert.Equal(t, 2, a.Uses("bad")+a.Uses("good"))
}

func TestRunAgentGivesUpAfterTheRetry(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		rule     func(*mock.Rule)
		sentinel error
	}{
		{
			name:     "schema violation",
			rule:     func(r *mock.Rule) { r.Say("done").Structured(json.RawMessage(`{"nope":1}`)) },
			sentinel: structured.ErrSchema,
		},
		{
			name:     "invalid json",
			rule:     func(r *mock.Rule) { r.Say("done").Fail(mock.FailMalformed, `{"summary": }`) },
			sentinel: structured.ErrInvalidJSON,
		},
		{
			name:     "no block at all",
			rule:     func(r *mock.Rule) { r.Say("I could not finish, sorry.") },
			sentinel: structured.ErrMissing,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := mock.NewScenario("bad")
			tc.rule(s.Otherwise("always-bad"))
			a := runMockAdapter(t, s)

			_, tr, err := RunAgent[domain.ImplReport](context.Background(), runAgentRun(t, a, nil))
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.sentinel)

			var ae *AgentError
			require.ErrorAs(t, err, &ae)
			assert.Equal(t, events.FailureBadOutput, ae.Failure)
			assert.Equal(t, 2, ae.Attempts, "the schema retry is fired exactly once")
			assert.False(t, ae.Retryable())
			pe, ok := ae.ParseError()
			require.True(t, ok)
			assert.NotEmpty(t, pe.Feedback())
			assert.Equal(t, pe.Feedback(), ae.Feedback())
			assert.Contains(t, ae.Error(), "eng-sc-1a2b-r1")

			require.Len(t, tr.Attempts, 2)
			assert.True(t, tr.Failed())
			assert.Equal(t, 1, tr.Retries())
		})
	}
}

func TestRunAgentSchemaRetriesCanBeDisabled(t *testing.T) {
	t.Parallel()
	s := mock.NewScenario("bad")
	s.Otherwise("always-bad").Say("done").Structured(json.RawMessage(`{"nope":1}`))
	a := runMockAdapter(t, s)

	run := runAgentRun(t, a, nil)
	run.SchemaRetries = -1
	_, tr, err := RunAgent[domain.ImplReport](context.Background(), run)
	require.Error(t, err)
	assert.Len(t, tr.Attempts, 1)
	assert.Len(t, a.Calls(), 1)
}

func TestRunAgentTypedFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		failure   mock.Failure
		failClass events.Failure
		kind      cartridge.Kind
		sentinel  error
		retryable bool
	}{
		{"crash", mock.Failure{Kind: mock.FailCrash, Message: "boom", ExitCode: 2}, events.FailureCrash, cartridge.KindCrash, cartridge.ErrCrash, true},
		{"timeout", mock.Failure{Kind: mock.FailTimeout, Message: "the model stalled"}, events.FailureTimeout, cartridge.KindTimeout, cartridge.ErrTimeout, true},
		{"rate limit", mock.Failure{Kind: mock.FailRateLimited, Message: "slow down", RetryAfter: mock.Duration(30 * time.Second)}, events.FailureRateLimited, cartridge.KindRateLimited, cartridge.ErrRateLimited, true},
		{"credits", mock.Failure{Kind: mock.FailCreditsExhausted, Message: "no credits"}, events.FailureCreditsExhausted, cartridge.KindCreditsExhausted, cartridge.ErrCreditsExhausted, false},
		{"auth", mock.Failure{Kind: mock.FailAuth, Message: "log in"}, events.FailureCrash, cartridge.KindAuth, cartridge.ErrAuth, false},
		{"killed", mock.Failure{Kind: mock.FailKilled, Message: "stopped"}, events.FailureKilled, cartridge.KindKilled, cartridge.ErrKilled, false},
		{"not installed", mock.Failure{Kind: mock.FailNotInstalled, Message: "no binary"}, events.FailureCrash, cartridge.KindNotInstalled, cartridge.ErrNotInstalled, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := mock.NewScenario("failing")
			s.Otherwise("fail").Say("working on it\n").FailWith(tc.failure)
			a := runMockAdapter(t, s)

			bus := events.NewBus()
			t.Cleanup(bus.Close)
			collect := runBusSink(t, bus)

			run := runAgentRun(t, a, bus)
			run.Spec.Timeout = 20 * time.Millisecond // the scripted timeout waits for it
			_, tr, err := RunAgent[domain.ImplReport](context.Background(), run)
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.sentinel)

			var ae *AgentError
			require.ErrorAs(t, err, &ae)
			assert.Equal(t, tc.failClass, ae.Failure)
			assert.Equal(t, tc.kind, ae.Kind)
			assert.Equal(t, tc.retryable, ae.Retryable())
			assert.Equal(t, 1, ae.Attempts, "a cartridge failure is never retried here")
			assert.Equal(t, domain.BeadID("sc-1a2b"), ae.Bead)
			assert.Equal(t, events.TaskImplement, ae.Task)
			assert.Equal(t, mock.Name, ae.Cartridge)
			_, isParse := ae.ParseError()
			assert.False(t, isParse)
			assert.Empty(t, ae.Feedback())
			if tc.kind == cartridge.KindRateLimited {
				assert.Equal(t, 30*time.Second, ae.RetryAfter)
			}

			require.Len(t, tr.Attempts, 1)
			assert.True(t, tr.Failed())
			assert.Equal(t, tc.failClass, tr.Attempts[0].Failure)
			assert.Len(t, a.Calls(), 1)

			// Whatever the agent said before it died is still on the bus.
			assert.Contains(t, runLinesOn(collect(), events.StreamStdout), "working on it")
		})
	}
}

func TestRunAgentReportsACancelledContext(t *testing.T) {
	t.Parallel()
	s := mock.NewScenario("slow")
	s.Otherwise("slow").Say("thinking").Sleep(time.Minute).Structured(runReport)
	a := runMockAdapter(t, s)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	_, tr, err := RunAgent[domain.ImplReport](ctx, runAgentRun(t, a, nil))
	require.Error(t, err)
	assert.ErrorIs(t, err, cartridge.ErrKilled)
	var ae *AgentError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, events.FailureKilled, ae.Failure)
	assert.False(t, ae.Retryable())
	assert.Len(t, tr.Attempts, 1)
}

func TestRunAgentReportsASessionThatNeverStarted(t *testing.T) {
	t.Parallel()
	// A scenario with no matching rule and no fallback: Run itself fails,
	// so there is a failed attempt with no output at all.
	s := mock.NewScenario("no-match")
	s.Rule("testers-only", mock.Match{Role: "tester"}).Say("hi").Structured(runReport)
	a := runMockAdapter(t, s)

	_, tr, err := RunAgent[domain.ImplReport](context.Background(), runAgentRun(t, a, nil))
	require.Error(t, err)
	assert.ErrorIs(t, err, cartridge.ErrCrash)
	assert.ErrorIs(t, err, mock.ErrNoMatch)
	var ae *AgentError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, events.FailureCrash, ae.Failure)
	assert.True(t, ae.Retryable())
	require.Len(t, tr.Attempts, 1)
	assert.Empty(t, tr.Attempts[0].Output)
	assert.Empty(t, tr.Lines)
}

func TestRunAgentRefusesAFiringTheCartridgeCannotHold(t *testing.T) {
	t.Parallel()
	// An adapter under an unknown name has no tool controls, so read_only
	// would need the copied-worktree degrade that nobody implements.
	a, err := mock.NewAdapter("run-no-controls", mock.DefaultScenario())
	require.NoError(t, err)

	run := runAgentRun(t, a, nil)
	run.Spec.ToolPolicy = cartridge.ToolReadOnly
	_, tr, err := RunAgent[domain.Plan](context.Background(), run)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCopyWorktreeUnimplemented)
	assert.Empty(t, tr.Attempts)
	assert.Empty(t, a.Calls(), "nothing may be fired when the policy cannot be held")
}

func TestRunAgentValidate(t *testing.T) {
	t.Parallel()
	a := runMockAdapter(t, mock.DefaultScenario())
	base := func() AgentRun { return runAgentRun(t, a, nil) }

	cases := []struct {
		name string
		mut  func(*AgentRun)
		want string
	}{
		{"no cartridge", func(r *AgentRun) { r.Cartridge = nil }, "no cartridge"},
		{"empty prompt", func(r *AgentRun) { r.Spec.Prompt = " " }, "empty prompt"},
		{"relative workdir", func(r *AgentRun) { r.Spec.Workdir = "relative" }, "not absolute"},
		{"no policy", func(r *AgentRun) { r.Spec.ToolPolicy = "" }, "no tool policy"},
		{"no agent id", func(r *AgentRun) { r.Agent.ID = "" }, "no agent id"},
		{"bad role", func(r *AgentRun) { r.Agent.Role = "wizard" }, "is not a role"},
		{"bad lens", func(r *AgentRun) { r.Agent.Lens = "vibes" }, "is not a lens"},
		{"no contract", func(r *AgentRun) { r.Message = "" }, "neither a contract nor a message type"},
		{"unknown message", func(r *AgentRun) { r.Message = "manifesto" }, "unknown message type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := base()
			tc.mut(&run)
			err := run.Validate()
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidAgentRun)
			assert.Contains(t, err.Error(), tc.want)

			_, _, ferr := RunAgent[domain.ImplReport](context.Background(), run)
			require.Error(t, ferr, "an invalid run never fires")
			assert.ErrorIs(t, ferr, ErrInvalidAgentRun)
		})
	}

	t.Run("a valid run passes", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, base().Validate())
	})

	t.Run("an explicit contract stands in for the message type", func(t *testing.T) {
		t.Parallel()
		run := base()
		run.Message = ""
		run.Contract = MustContractFor(domain.MessageVerdict)
		require.NoError(t, run.Validate())
		c, err := run.OutputContract()
		require.NoError(t, err)
		assert.Equal(t, "verdict", c.Name())
	})
}

func TestRunMessageDecodesThePayloadType(t *testing.T) {
	t.Parallel()
	responses := json.RawMessage(`{"responses":[{"item_id":"qa-r1-1","action":"fixed","rationale":"Added the missing nil check."}]}`)
	s := mock.NewScenario("responses")
	s.Otherwise("answer").Say("Answered every item.").Structured(responses)
	a := runMockAdapter(t, s)

	run := runAgentRun(t, a, nil)
	run.Message = domain.MessageResponse
	run.Task = events.TaskAddressFeedback
	payload, tr, err := RunMessage(context.Background(), run)
	require.NoError(t, err)
	got, ok := payload.([]domain.Response)
	require.True(t, ok, "the response payload is a slice, as beads.Message.Payload holds it")
	require.Len(t, got, 1)
	assert.Equal(t, "qa-r1-1", got[0].ItemID)
	assert.Equal(t, domain.ActionFixed, got[0].Action)
	assert.Equal(t, "response", tr.Contract)

	t.Run("and needs a message type", func(t *testing.T) {
		t.Parallel()
		bare := runAgentRun(t, a, nil)
		bare.Message = ""
		bare.Contract = MustContractFor(domain.MessageResponse)
		_, _, err := RunMessage(context.Background(), bare)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidAgentRun)
	})
}

func TestRunAgentDecodeMismatchIsABadOutput(t *testing.T) {
	t.Parallel()
	s := mock.NewScenario("report")
	s.Otherwise("ok").Say("done").Structured(runReport)
	a := runMockAdapter(t, s)

	// A valid impl_report decoded into a type it cannot fit.
	_, tr, err := RunAgent[[]domain.Response](context.Background(), runAgentRun(t, a, nil))
	require.Error(t, err)
	var ae *AgentError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, events.FailureBadOutput, ae.Failure)
	assert.ErrorIs(t, err, structured.ErrSchema)
	require.Len(t, tr.Attempts, 1)
	assert.Equal(t, events.FailureBadOutput, tr.Attempts[0].Failure)
}

func TestRunAgentCapsTheTranscriptWithoutCappingTheBus(t *testing.T) {
	t.Parallel()
	s := mock.NewScenario("chatty")
	s.Otherwise("chatty").Say("one\ntwo\nthree\nfour\nfive\n").Structured(runReport)
	a := runMockAdapter(t, s)

	bus := events.NewBus()
	t.Cleanup(bus.Close)
	collect := runBusSink(t, bus)

	run := runAgentRun(t, a, bus)
	run.MaxLines = 2
	_, tr, err := RunAgent[domain.ImplReport](context.Background(), run)
	require.NoError(t, err)
	assert.Len(t, tr.Lines, 2)
	assert.Equal(t, 3, tr.Dropped)
	assert.Equal(t, "one\ntwo", tr.Text())
	assert.Len(t, collect(), 5, "every line still reaches the bus")

	t.Run("negative keeps everything", func(t *testing.T) {
		t.Parallel()
		run := runAgentRun(t, a, nil)
		run.MaxLines = -1
		_, tr, err := RunAgent[domain.ImplReport](context.Background(), run)
		require.NoError(t, err)
		assert.Len(t, tr.Lines, 5)
		assert.Zero(t, tr.Dropped)
	})
}

func TestRunAgentCallsTheObserverForEveryEvent(t *testing.T) {
	t.Parallel()
	s := mock.NewScenario("observed")
	s.Otherwise("ok").Say("hello").Tool("Read", "t1", "main.go").Structured(runReport)
	a := runMockAdapter(t, s)

	var mu sync.Mutex
	var kinds []cartridge.EventKind
	run := runAgentRun(t, a, nil)
	run.Observer = func(ev cartridge.SessionEvent) {
		mu.Lock()
		defer mu.Unlock()
		kinds = append(kinds, ev.Kind)
	}
	_, _, err := RunAgent[domain.ImplReport](context.Background(), run)
	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []cartridge.EventKind{cartridge.EventTextDelta, cartridge.EventToolUse, cartridge.EventDone}, kinds)
}

func TestRunAgentWorksWithoutABus(t *testing.T) {
	t.Parallel()
	s := mock.NewScenario("quiet")
	s.Otherwise("ok").Say("hello\nworld\n").Structured(runReport)
	a := runMockAdapter(t, s)

	report, tr, err := RunAgent[domain.ImplReport](context.Background(), runAgentRun(t, a, nil))
	require.NoError(t, err)
	assert.NotEmpty(t, report.Summary)
	assert.Equal(t, []string{"hello", "world"}, runLinesOfTranscript(tr))
}

func TestRunAgentUsesTheInjectedClock(t *testing.T) {
	t.Parallel()
	s := mock.NewScenario("clocked")
	s.Otherwise("ok").Say("hi").Structured(runReport)
	a := runMockAdapter(t, s)

	base := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	var calls int
	run := runAgentRun(t, a, nil)
	run.Now = func() time.Time {
		calls++
		return base.Add(time.Duration(calls) * time.Second)
	}
	_, tr, err := RunAgent[domain.ImplReport](context.Background(), run)
	require.NoError(t, err)
	assert.Positive(t, tr.Duration)
	assert.Equal(t, tr.Duration%time.Second, time.Duration(0))
}

func TestFailureOf(t *testing.T) {
	t.Parallel()
	assert.Equal(t, events.Failure(""), FailureOf(nil))
	assert.Equal(t, events.FailureBadOutput, FailureOf(&structured.ParseError{Reason: structured.ReasonSchema}))
	assert.Equal(t, events.FailureTimeout, FailureOf(cartridge.NewError(cartridge.KindTimeout, "mock", "", nil)))
	assert.Equal(t, events.FailureRateLimited, FailureOf(cartridge.NewError(cartridge.KindRateLimited, "mock", "", nil)))
	assert.Equal(t, events.FailureCreditsExhausted, FailureOf(cartridge.ErrCreditsExhausted))
	assert.Equal(t, events.FailureCrash, FailureOf(errors.New("who knows")))
}

func TestTranscriptString(t *testing.T) {
	t.Parallel()
	tr := Transcript{
		Agent: "eng-1", Cartridge: mock.Name, Contract: "impl_report", Task: events.TaskImplement,
		Attempts: []Attempt{{Number: 1, Err: errors.New("nope")}},
		Lines:    []OutputLine{{Stream: events.StreamStdout, Text: "hi"}},
		Dropped:  2,
		Usage:    cartridge.Usage{InputTokens: 10, OutputTokens: 5},
		Duration: 1500 * time.Millisecond,
	}
	s := tr.String()
	assert.Contains(t, s, "eng-1 mock/impl_report implement")
	assert.Contains(t, s, "1 attempt(s)")
	assert.Contains(t, s, "3 line(s)")
	assert.Contains(t, s, "15 tokens")
	assert.Contains(t, s, "failed")
	assert.True(t, Transcript{}.Failed(), "a firing with no attempt at all failed")
}

func TestDefaultSchemaRetriesIsOne(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 1, DefaultSchemaRetries)
	assert.Equal(t, 1, AgentRun{}.retries())
	assert.Equal(t, 0, AgentRun{SchemaRetries: -3}.retries())
	assert.Equal(t, 2, AgentRun{SchemaRetries: 2}.retries())
	assert.Equal(t, DefaultTranscriptLines, AgentRun{}.maxLines())
}
