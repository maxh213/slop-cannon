package mock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/testutil"
)

// mockSpec builds a valid RunSpec in workdir with the standard labels for
// the given agent.
func mockSpec(workdir string, role domain.Role, lens domain.Lens, bead domain.BeadID, task events.Task, round int) cartridge.RunSpec {
	a := domain.Agent{ID: domain.AgentID(string(role) + "-1"), Role: role, Lens: lens, Bead: bead, Cartridge: Name}
	policy := cartridge.ToolReadWrite
	switch role {
	case domain.RolePO:
		policy = cartridge.ToolReadOnly
	case domain.RoleTester:
		policy = cartridge.ToolReadExec
	}
	return cartridge.RunSpec{
		SystemPrompt: "system for " + string(role),
		Prompt:       "do " + string(task),
		Workdir:      workdir,
		ToolPolicy:   policy,
		Labels:       cartridge.LabelsFor("run-1", a, task, round),
	}
}

// mockCollect drains a session and returns its events and outcome, failing
// the test if it does not finish within a few seconds of wall time.
func mockCollect(t testing.TB, s cartridge.Session) ([]cartridge.SessionEvent, cartridge.Result, error) {
	t.Helper()
	type outcome struct {
		evs []cartridge.SessionEvent
		res cartridge.Result
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		var evs []cartridge.SessionEvent
		res, err := cartridge.Collect(s, func(ev cartridge.SessionEvent) { evs = append(evs, ev) })
		ch <- outcome{evs, res, err}
	}()
	select {
	case o := <-ch:
		return o.evs, o.res, o.err
	case <-time.After(10 * time.Second):
		t.Fatal("session did not finish")
		return nil, cartridge.Result{}, nil
	}
}

// mockAssertStream checks the events contract: exactly one terminal event,
// last.
func mockAssertStream(t testing.TB, evs []cartridge.SessionEvent) cartridge.SessionEvent {
	t.Helper()
	require.NotEmpty(t, evs)
	terminals := 0
	for _, ev := range evs {
		if ev.Kind.Terminal() {
			terminals++
		}
		assert.False(t, ev.At.IsZero(), "event without timestamp: %s", cartridge.Describe(ev))
	}
	assert.Equal(t, 1, terminals, "exactly one terminal event")
	last := evs[len(evs)-1]
	assert.True(t, last.Kind.Terminal(), "terminal event is last")
	return last
}

func mockEnvMap(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "PATH" {
			out[k] = v
		}
	}
	return out
}

func mockLoad(t testing.TB, name string, opts ...Option) *Adapter {
	t.Helper()
	s, err := Load(mockTestdata(t, name))
	require.NoError(t, err)
	a, err := NewAdapter(Name, s, opts...)
	require.NoError(t, err)
	return a
}

func TestRegisteredInDefaultRegistry(t *testing.T) {
	t.Parallel()
	require.True(t, cartridge.Default.Has(Name))
	c, err := cartridge.New(Name, config.Cartridge{})
	require.NoError(t, err)
	assert.Equal(t, Name, c.Name())

	dir := t.TempDir()
	spec := mockSpec(dir, domain.RoleEngineer, "", "sc-1", events.TaskImplement, 0)
	spec.Prompt = "echo this token: XYZZY"
	sess, err := c.Run(context.Background(), spec)
	require.NoError(t, err)
	evs, res, err := mockCollect(t, sess)
	require.NoError(t, err)
	last := mockAssertStream(t, evs)
	assert.Equal(t, cartridge.EventDone, last.Kind)
	assert.Equal(t, "echo this token: XYZZY", res.Output)
	assert.Equal(t, res.Output, cartridge.Transcript(evs))
	assert.Equal(t, "mock-1", res.SessionID)
	assert.Equal(t, 0, res.ExitCode)
	assert.Positive(t, res.Usage.InputTokens)
	assert.Positive(t, res.Usage.OutputTokens)
	assert.Equal(t, 1, res.Usage.Turns)
}

func TestFactoryLoadsScenarioFromEnv(t *testing.T) {
	t.Parallel()
	c, err := New("mock-a", config.Cartridge{Env: map[string]string{ScenarioEnv: mockTestdata(t, "detect-credits.json")}})
	require.NoError(t, err)
	assert.Equal(t, "mock-a", c.Name())
	a, ok := c.(*Adapter)
	require.True(t, ok)
	assert.Equal(t, "out of credits", a.Scenario().Name)

	_, err = New(Name, config.Cartridge{Env: map[string]string{ScenarioEnv: filepath.Join(t.TempDir(), "missing.json")}})
	require.Error(t, err)

	bad := filepath.Join(t.TempDir(), "bad.json")
	require.NoError(t, os.WriteFile(bad, []byte(`{"rules":[{"name":"x","match":{},"steps":[{"kind":"nope"}]}]}`), 0o644))
	_, err = New(Name, config.Cartridge{Env: map[string]string{ScenarioEnv: bad}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad.json")

	_, err = cartridge.Build(config.Config{Cartridges: map[string]config.Cartridge{Name: {}}})
	require.NoError(t, err)
}

func TestNewAdapterValidates(t *testing.T) {
	t.Parallel()
	_, err := NewAdapter("", nil)
	require.Error(t, err)
	s := NewScenario("x")
	s.Rule("", Match{})
	_, err = NewAdapter("", s)
	require.Error(t, err)
	a, err := NewAdapter("", NewScenario("empty"))
	require.NoError(t, err)
	assert.Equal(t, Name, a.Name(), "empty name defaults to mock")
	assert.NotNil(t, a.Clock())
}

func TestRunRejectsInvalidSpec(t *testing.T) {
	t.Parallel()
	a, err := NewAdapter(Name, DefaultScenario())
	require.NoError(t, err)
	_, err = a.Run(context.Background(), cartridge.RunSpec{})
	require.Error(t, err)
	assert.Empty(t, a.Calls(), "invalid specs are not recorded")
}

func TestRunNoMatchNoFallback(t *testing.T) {
	t.Parallel()
	s := NewScenario("strict")
	s.Rule("eng", Match{Role: "engineer"}).Say("x")
	a, err := NewAdapter(Name, s)
	require.NoError(t, err)
	_, err = a.Run(context.Background(), mockSpec(t.TempDir(), domain.RoleTester, domain.LensQA, "sc-1", events.TaskReview, 1))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoMatch)
	assert.ErrorIs(t, err, cartridge.ErrCrash)
	assert.Equal(t, cartridge.KindCrash, cartridge.KindOf(err))
	assert.Contains(t, err.Error(), "role=tester")
	calls := a.Calls()
	require.Len(t, calls, 1)
	assert.False(t, calls[0].Matched)
	assert.Empty(t, calls[0].Rule)
}

func TestMatchingOnLabelsAndBudgets(t *testing.T) {
	t.Parallel()
	clock := NewFakeClock(mockEpoch)
	a := mockLoad(t, "review-loop.json", WithClock(clock))
	dir := t.TempDir()
	ctx := context.Background()

	run := func(spec cartridge.RunSpec) (cartridge.Result, error) {
		t.Helper()
		sess, err := a.Run(ctx, spec)
		require.NoError(t, err)
		done := make(chan struct{})
		var evs []cartridge.SessionEvent
		var res cartridge.Result
		go func() {
			defer close(done)
			evs, res, err = mockCollect(t, sess)
		}()
		// Drive every scripted delay without waiting for real time.
		for {
			select {
			case <-done:
				mockAssertStream(t, evs)
				return res, err
			default:
			}
			if clock.Waiters() > 0 {
				clock.Advance(time.Second)
			} else {
				time.Sleep(time.Millisecond)
			}
		}
	}

	// PO plan.
	res, err := run(mockSpec(dir, domain.RolePO, "", "", events.TaskPlan, 0))
	require.NoError(t, err)
	assert.Contains(t, res.Output, "Researching the repository.")
	assert.Contains(t, res.Output, "```sc-result\n{\"beads\":")

	// QA round 1 is answered once by "qa first round", then by "approve".
	qa1 := mockSpec(dir, domain.RoleTester, domain.LensQA, "sc-1", events.TaskReview, 1)
	res, err = run(qa1)
	require.NoError(t, err)
	assert.Contains(t, res.Output, `"outcome":"changes"`)
	res, err = run(qa1)
	require.NoError(t, err)
	assert.Contains(t, res.Output, `"outcome":"approve"`, "budget spent, next rule answers")

	// UX round 1 never matches the QA rule.
	res, err = run(mockSpec(dir, domain.RoleTester, domain.LensUX, "sc-1", events.TaskReview, 1))
	require.NoError(t, err)
	assert.Contains(t, res.Output, `"outcome":"approve"`)

	// Unmatched firings fall back to echo.
	other := mockSpec(dir, domain.RoleEngineer, "", "sc-1", events.TaskResolveConflict, 0)
	other.Prompt = "resolve it"
	res, err = run(other)
	require.NoError(t, err)
	assert.Equal(t, "resolve it", res.Output)

	calls := a.Calls()
	require.Len(t, calls, 5)
	names := make([]string, len(calls))
	for i, c := range calls {
		names[i] = c.Rule
		assert.Equal(t, i+1, c.Seq)
		assert.True(t, c.Matched)
	}
	assert.Equal(t, []string{"plan", "qa first round", "approve", "approve", "echo"}, names)
	assert.Equal(t, 1, a.Uses("qa first round"))
	assert.Equal(t, 2, a.Uses("approve"))
	assert.Equal(t, 0, a.Uses("nonexistent"))
	assert.Len(t, a.CallsMatching(Match{Role: "tester"}), 3)
	last, ok := a.LastCall()
	require.True(t, ok)
	assert.Equal(t, "resolve it", last.Spec.Prompt)
	assert.Equal(t, "run-1", last.Spec.Labels.Get(cartridge.LabelRun))

	a.Reset()
	assert.Empty(t, a.Calls())
	assert.Equal(t, 0, a.Uses("approve"))
	_, ok = a.LastCall()
	assert.False(t, ok)
	// After Reset the once-only rule answers again.
	res, err = run(qa1)
	require.NoError(t, err)
	assert.Contains(t, res.Output, `"outcome":"changes"`)
}

func TestRecordsSpecsIndependently(t *testing.T) {
	t.Parallel()
	a, err := NewAdapter(Name, DefaultScenario())
	require.NoError(t, err)
	spec := mockSpec(t.TempDir(), domain.RoleEngineer, "", "sc-1", events.TaskImplement, 0)
	spec.Env = map[string]string{"A": "1"}
	spec.InheritEnv = []string{"HOME"}
	sess, err := a.Run(context.Background(), spec)
	require.NoError(t, err)
	_, _, err = mockCollect(t, sess)
	require.NoError(t, err)
	// Mutating the caller's maps after Run does not change the record.
	spec.Labels[cartridge.LabelBead] = "changed"
	spec.Env["A"] = "2"
	spec.InheritEnv[0] = "X"
	c := a.Calls()[0]
	assert.Equal(t, "sc-1", c.Spec.Labels.Get(cartridge.LabelBead))
	assert.Equal(t, "1", c.Spec.Env["A"])
	assert.Equal(t, []string{"HOME"}, c.Spec.InheritEnv)
	assert.Equal(t, cartridge.ToolReadWrite, c.Spec.ToolPolicy)
}

func TestDelaysUnderFakeClock(t *testing.T) {
	t.Parallel()
	clock := NewFakeClock(mockEpoch)
	s := NewScenario("slow")
	s.Otherwise("slow").Say("one").Sleep(30 * time.Second).Say("two").Delay(time.Minute)
	a, err := NewAdapter(Name, s, WithClock(clock))
	require.NoError(t, err)

	sess, err := a.Run(context.Background(), mockSpec(t.TempDir(), domain.RolePO, "", "", events.TaskPlan, 0))
	require.NoError(t, err)
	evs := sess.Events()

	first := <-evs
	assert.Equal(t, "one", first.Text)
	assert.Equal(t, mockEpoch, first.At)

	clock.BlockUntil(1)
	select {
	case ev := <-evs:
		t.Fatalf("event before the clock moved: %s", cartridge.Describe(ev))
	case <-time.After(20 * time.Millisecond):
	}
	clock.Advance(30 * time.Second) // the sleep step
	clock.BlockUntil(1)
	clock.Advance(59 * time.Second) // not yet the full minute
	select {
	case ev := <-evs:
		t.Fatalf("event before the delay elapsed: %s", cartridge.Describe(ev))
	case <-time.After(20 * time.Millisecond):
	}
	clock.Advance(time.Second)
	second := <-evs
	assert.Equal(t, "two", second.Text)
	assert.Equal(t, mockEpoch.Add(90*time.Second), second.At)
	done := <-evs
	assert.Equal(t, cartridge.EventDone, done.Kind)
	_, open := <-evs
	assert.False(t, open, "channel closed after the terminal event")
	res, err := sess.Wait()
	require.NoError(t, err)
	assert.Equal(t, 90*time.Second, res.Duration)
	assert.Equal(t, "onetwo", res.Output)
}

func TestTimeoutUnderFakeClock(t *testing.T) {
	t.Parallel()
	clock := NewFakeClock(mockEpoch)
	s := NewScenario("hang")
	s.Otherwise("hang").Say("started ").Sleep(time.Hour).Say("never")
	a, err := NewAdapter(Name, s, WithClock(clock))
	require.NoError(t, err)

	spec := mockSpec(t.TempDir(), domain.RoleEngineer, "", "sc-1", events.TaskImplement, 0)
	spec.Timeout = 5 * time.Minute
	sess, err := a.Run(context.Background(), spec)
	require.NoError(t, err)
	clock.BlockUntil(2) // the timeout and the sleep
	clock.Advance(5 * time.Minute)

	evs, res, err := mockCollect(t, sess)
	last := mockAssertStream(t, evs)
	assert.Equal(t, cartridge.EventError, last.Kind)
	require.Error(t, err)
	assert.ErrorIs(t, err, cartridge.ErrTimeout)
	assert.Equal(t, cartridge.KindTimeout, cartridge.KindOf(err))
	assert.Contains(t, err.Error(), "5m0s timeout")
	assert.Equal(t, "started ", res.Output, "partial output is kept")
	assert.Equal(t, 5*time.Minute, res.Duration)
	assert.Equal(t, -1, res.ExitCode)
	assert.Equal(t, res.Usage, last.Usage)
}

func TestInjectedTimeoutWaitsForSpecTimeout(t *testing.T) {
	t.Parallel()
	clock := NewFakeClock(mockEpoch)
	a := mockLoad(t, "failures.json", WithClock(clock))
	spec := mockSpec(t.TempDir(), domain.RoleEngineer, "", "timeout", events.TaskImplement, 0)
	spec.Timeout = time.Minute
	sess, err := a.Run(context.Background(), spec)
	require.NoError(t, err)
	clock.BlockUntil(1)
	pipe, ok := sess.(*cartridge.Pipe)
	require.True(t, ok)
	select {
	case <-pipe.Done():
		t.Fatal("timed out before the spec timeout elapsed")
	case <-time.After(20 * time.Millisecond):
	}
	clock.Advance(time.Minute)
	evs, res, err := mockCollect(t, sess)
	mockAssertStream(t, evs)
	assert.ErrorIs(t, err, cartridge.ErrTimeout)
	assert.Equal(t, "thinking...", res.Output)
	assert.Equal(t, time.Minute, res.Duration)

	// Without a spec timeout the injected timeout is immediate.
	spec.Timeout = 0
	sess, err = a.Run(context.Background(), spec)
	require.NoError(t, err)
	_, _, err = mockCollect(t, sess)
	assert.ErrorIs(t, err, cartridge.ErrTimeout)
}

func TestKillAndCancel(t *testing.T) {
	t.Parallel()
	clock := NewFakeClock(mockEpoch)
	s := NewScenario("hang")
	s.Otherwise("hang").Say("a").Sleep(time.Hour).Say("b")
	a, err := NewAdapter(Name, s, WithClock(clock))
	require.NoError(t, err)
	spec := mockSpec(t.TempDir(), domain.RoleEngineer, "", "sc-1", events.TaskImplement, 0)

	t.Run("kill", func(t *testing.T) {
		sess, err := a.Run(context.Background(), spec)
		require.NoError(t, err)
		clock.BlockUntil(1)
		sess.Kill()
		sess.Kill() // idempotent
		evs, res, err := mockCollect(t, sess)
		last := mockAssertStream(t, evs)
		assert.Equal(t, cartridge.EventError, last.Kind)
		assert.ErrorIs(t, err, cartridge.ErrKilled)
		assert.Equal(t, "a", res.Output)
		assert.Equal(t, -1, res.ExitCode)
	})

	t.Run("context", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		sess, err := a.Run(ctx, spec)
		require.NoError(t, err)
		clock.BlockUntil(1)
		cause := errors.New("operator stop")
		cancel(cause)
		evs, _, err := mockCollect(t, sess)
		mockAssertStream(t, evs)
		assert.ErrorIs(t, err, cartridge.ErrKilled)
		assert.ErrorIs(t, err, cause, "the cancellation cause is the error's cause")
		assert.Equal(t, cartridge.KindKilled, cartridge.KindOf(err))
	})

	t.Run("cancelled before the first step", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		sess, err := a.Run(ctx, spec)
		require.NoError(t, err, "Run itself succeeds; the session reports the kill")
		evs, _, err := mockCollect(t, sess)
		mockAssertStream(t, evs)
		assert.ErrorIs(t, err, cartridge.ErrKilled)
	})

	t.Run("kill after finish is harmless", func(t *testing.T) {
		quick, err := NewAdapter(Name, DefaultScenario())
		require.NoError(t, err)
		sess, err := quick.Run(context.Background(), spec)
		require.NoError(t, err)
		_, _, err = mockCollect(t, sess)
		require.NoError(t, err)
		sess.Kill()
		_, err = sess.Wait()
		require.NoError(t, err)
	})
}

func TestSideEffectsInFixtureRepo(t *testing.T) {
	repo := testutil.NewFixtureRepo(t)
	a := mockLoad(t, "review-loop.json")
	head := repo.Head(t)

	spec := mockSpec(repo.Dir, domain.RoleEngineer, "", "sc-1", events.TaskImplement, 0)
	spec.Env = mockEnvMap(repo.Env())
	sess, err := a.Run(context.Background(), spec)
	require.NoError(t, err)
	evs, res, err := mockCollect(t, sess)
	require.NoError(t, err)
	mockAssertStream(t, evs)

	// The file exists and the commit landed.
	data, err := os.ReadFile(filepath.Join(repo.Dir, "FIX.md"))
	require.NoError(t, err)
	assert.Equal(t, "fixed by the mock engineer\n", string(data))
	assert.NotEqual(t, head, repo.Head(t), "a new commit was made")
	assert.Equal(t, "", repo.Status(t), "working tree is clean")
	assert.Contains(t, repo.Log(t)[0], "mock: fix")

	// Tool events describe what happened, in order.
	var tools []string
	for _, ev := range evs {
		switch ev.Kind {
		case cartridge.EventToolUse:
			tools = append(tools, "use:"+ev.Tool+":"+ev.Input)
		case cartridge.EventToolResult:
			assert.False(t, ev.IsError, cartridge.Describe(ev))
			tools = append(tools, "result:"+ev.Tool)
		}
	}
	assert.Equal(t, []string{
		"use:Write:FIX.md", "result:Write",
		"use:Bash:git add -A", "result:Bash",
		"use:Bash:git commit -q -m mock: fix", "result:Bash",
	}, tools)

	// Result carries the scripted usage, session id and structured block.
	assert.Equal(t, "sess-impl", res.SessionID)
	assert.Equal(t, cartridge.Usage{InputTokens: 1200, OutputTokens: 300, CostUSD: 0.02, HasCost: true, Turns: 3}, res.Usage)
	assert.True(t, strings.HasPrefix(res.Output, "Fixing the bug.\n```sc-result\n"), res.Output)
	assert.Contains(t, res.Output, `"files":["FIX.md"]`)
	assert.True(t, strings.HasSuffix(res.Output, "\n```"), res.Output)
}

func TestSideEffectsHonourToolPolicy(t *testing.T) {
	t.Parallel()
	s := NewScenario("effects")
	s.Otherwise("all").Write("out.txt", "x").AppendFile("out.txt", "y").Delete("out.txt").Exec("sh", "-c", "echo ran")
	a, err := NewAdapter(Name, s)
	require.NoError(t, err)

	results := func(evs []cartridge.SessionEvent) []cartridge.SessionEvent {
		var out []cartridge.SessionEvent
		for _, ev := range evs {
			if ev.Kind == cartridge.EventToolResult {
				out = append(out, ev)
			}
		}
		return out
	}

	t.Run("read_write performs everything", func(t *testing.T) {
		dir := t.TempDir()
		spec := mockSpec(dir, domain.RoleEngineer, "", "sc-1", events.TaskImplement, 0)
		sess, err := a.Run(context.Background(), spec)
		require.NoError(t, err)
		evs, res, err := mockCollect(t, sess)
		require.NoError(t, err)
		rs := results(evs)
		require.Len(t, rs, 4)
		for _, r := range rs {
			assert.False(t, r.IsError, cartridge.Describe(r))
		}
		assert.Equal(t, "wrote 1 bytes to out.txt", rs[0].Output)
		assert.Equal(t, "appended 1 bytes to out.txt", rs[1].Output)
		assert.Equal(t, "removed out.txt", rs[2].Output)
		assert.Equal(t, "ran", rs[3].Output)
		_, statErr := os.Stat(filepath.Join(dir, "out.txt"))
		assert.True(t, os.IsNotExist(statErr))
		assert.Equal(t, 5, res.Usage.Turns)
	})

	t.Run("read_exec refuses writes but runs commands", func(t *testing.T) {
		dir := t.TempDir()
		spec := mockSpec(dir, domain.RoleTester, domain.LensQA, "sc-1", events.TaskReview, 1)
		sess, err := a.Run(context.Background(), spec)
		require.NoError(t, err)
		evs, _, err := mockCollect(t, sess)
		require.NoError(t, err)
		rs := results(evs)
		require.Len(t, rs, 4)
		for _, r := range rs[:3] {
			assert.True(t, r.IsError, cartridge.Describe(r))
			assert.Contains(t, r.Output, "tool policy read_exec forbids")
		}
		assert.False(t, rs[3].IsError)
		assert.Equal(t, "ran", rs[3].Output)
		_, statErr := os.Stat(filepath.Join(dir, "out.txt"))
		assert.True(t, os.IsNotExist(statErr), "nothing was written")
	})

	t.Run("read_only refuses everything", func(t *testing.T) {
		dir := t.TempDir()
		spec := mockSpec(dir, domain.RolePO, "", "", events.TaskPlan, 0)
		sess, err := a.Run(context.Background(), spec)
		require.NoError(t, err)
		evs, _, err := mockCollect(t, sess)
		require.NoError(t, err)
		rs := results(evs)
		require.Len(t, rs, 4)
		for _, r := range rs {
			assert.True(t, r.IsError, cartridge.Describe(r))
		}
		assert.Contains(t, rs[3].Output, "forbids running commands")
	})
}

func TestWriteCreatesParentDirsAndRejectsEscapes(t *testing.T) {
	t.Parallel()
	s := NewScenario("paths")
	s.Otherwise("w").Write("deep/nested/file.txt", "hi").Delete("does-not-exist").Write("ok/../../escape.txt", "no")
	_, err := NewAdapter(Name, s)
	require.Error(t, err, "an escaping path is rejected when the scenario is validated")
	assert.Contains(t, err.Error(), "escapes the workdir")
	s.Fallback.Steps = s.Fallback.Steps[:2]
	a, err := NewAdapter(Name, s)
	require.NoError(t, err)
	dir := t.TempDir()
	sess, err := a.Run(context.Background(), mockSpec(dir, domain.RoleEngineer, "", "sc-1", events.TaskImplement, 0))
	require.NoError(t, err)
	evs, _, err := mockCollect(t, sess)
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(dir, "deep", "nested", "file.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hi", string(data))
	var errs []string
	for _, ev := range evs {
		if ev.Kind == cartridge.EventToolResult && ev.IsError {
			errs = append(errs, ev.Output)
		}
	}
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0], "does-not-exist")
}

func TestResolveGuardsAtRuntime(t *testing.T) {
	t.Parallel()
	s := &session{spec: cartridge.RunSpec{Workdir: t.TempDir()}}
	for _, p := range []string{"", "/abs", "..", "../x", "a/../../x"} {
		_, err := s.resolve(p)
		assert.Error(t, err, p)
	}
	full, err := s.resolve("a/../b")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(s.spec.Workdir, "b"), full)
}

func TestExecFailures(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	spec := mockSpec(dir, domain.RoleEngineer, "", "sc-1", events.TaskImplement, 0)

	t.Run("failing command is an error result", func(t *testing.T) {
		s := NewScenario("x")
		s.Otherwise("r").Exec("sh", "-c", "echo oops >&2; exit 3").Say("continued")
		a, err := NewAdapter(Name, s)
		require.NoError(t, err)
		sess, err := a.Run(context.Background(), spec)
		require.NoError(t, err)
		evs, res, err := mockCollect(t, sess)
		require.NoError(t, err)
		assert.Equal(t, "continued", res.Output)
		var tr cartridge.SessionEvent
		for _, ev := range evs {
			if ev.Kind == cartridge.EventToolResult {
				tr = ev
			}
		}
		assert.True(t, tr.IsError)
		assert.Contains(t, tr.Output, "oops")
		assert.Contains(t, tr.Output, "exit status 3")
	})

	t.Run("must command crashes the session", func(t *testing.T) {
		s := NewScenario("x")
		s.Otherwise("r").MustExec("sh", "-c", "exit 7").Say("never")
		a, err := NewAdapter(Name, s)
		require.NoError(t, err)
		sess, err := a.Run(context.Background(), spec)
		require.NoError(t, err)
		evs, res, err := mockCollect(t, sess)
		mockAssertStream(t, evs)
		assert.ErrorIs(t, err, cartridge.ErrCrash)
		assert.Contains(t, err.Error(), "required command failed")
		assert.Equal(t, 7, res.ExitCode)
		assert.Empty(t, res.Output)
	})

	t.Run("missing binary", func(t *testing.T) {
		s := NewScenario("x")
		s.Otherwise("r").MustExec("definitely-not-a-real-binary-xyz")
		a, err := NewAdapter(Name, s)
		require.NoError(t, err)
		sess, err := a.Run(context.Background(), spec)
		require.NoError(t, err)
		_, res, err := mockCollect(t, sess)
		assert.ErrorIs(t, err, cartridge.ErrCrash)
		assert.Equal(t, 1, res.ExitCode)
	})

	t.Run("must under read_only crashes", func(t *testing.T) {
		s := NewScenario("x")
		s.Otherwise("r").MustExec("true")
		a, err := NewAdapter(Name, s)
		require.NoError(t, err)
		ro := spec
		ro.ToolPolicy = cartridge.ToolReadOnly
		sess, err := a.Run(context.Background(), ro)
		require.NoError(t, err)
		_, _, err = mockCollect(t, sess)
		assert.ErrorIs(t, err, cartridge.ErrCrash)
		assert.Contains(t, err.Error(), "refused by tool policy")
	})

	t.Run("long output is truncated", func(t *testing.T) {
		s := NewScenario("x")
		s.Otherwise("r").Exec("sh", "-c", "head -c 10000 /dev/zero | tr '\\0' 'a'")
		a, err := NewAdapter(Name, s)
		require.NoError(t, err)
		sess, err := a.Run(context.Background(), spec)
		require.NoError(t, err)
		evs, _, err := mockCollect(t, sess)
		require.NoError(t, err)
		for _, ev := range evs {
			if ev.Kind == cartridge.EventToolResult {
				assert.LessOrEqual(t, len(ev.Output), maxToolOutput+len("…"))
				assert.True(t, strings.HasSuffix(ev.Output, "…"))
			}
		}
	})
}

func TestFailureInjection(t *testing.T) {
	t.Parallel()
	a := mockLoad(t, "failures.json")
	dir := t.TempDir()
	cases := []struct {
		bead     string
		sentinel error
		kind     cartridge.Kind
		exit     int
		output   string
		msg      string
	}{
		{"crash", cartridge.ErrCrash, cartridge.KindCrash, 134, "partial output", "segfault in tool loop"},
		{"timeout", cartridge.ErrTimeout, cartridge.KindTimeout, -1, "thinking...", "timeout"},
		{"rate_limited", cartridge.ErrRateLimited, cartridge.KindRateLimited, -1, "", "429 too many requests"},
		{"credits_exhausted", cartridge.ErrCreditsExhausted, cartridge.KindCreditsExhausted, -1, "", "credit balance"},
		{"auth", cartridge.ErrAuth, cartridge.KindAuth, -1, "", "not logged in"},
		{"killed", cartridge.ErrKilled, cartridge.KindKilled, -1, "", "injected killed"},
		{"not_installed", cartridge.ErrNotInstalled, cartridge.KindNotInstalled, 1, "", "injected not_installed"},
	}
	for _, tc := range cases {
		t.Run(tc.bead, func(t *testing.T) {
			t.Parallel()
			sess, err := a.Run(context.Background(), mockSpec(dir, domain.RoleEngineer, "", domain.BeadID(tc.bead), events.TaskImplement, 0))
			require.NoError(t, err)
			evs, res, err := mockCollect(t, sess)
			last := mockAssertStream(t, evs)
			assert.Equal(t, cartridge.EventError, last.Kind)
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.sentinel)
			assert.Equal(t, tc.kind, cartridge.KindOf(err))
			assert.Same(t, err, last.Err, "the Error event carries the same error Wait returns")
			var ce *cartridge.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, Name, ce.Cartridge)
			assert.Contains(t, err.Error(), tc.msg)
			assert.Equal(t, tc.exit, res.ExitCode)
			assert.Equal(t, tc.output, res.Output)
			assert.Equal(t, res.Usage, last.Usage, "usage before the failure is reported")
			assert.Equal(t, tc.kind.Retryable(), ce.Kind.Retryable())
		})
	}

	t.Run("rate limit carries retry_after", func(t *testing.T) {
		t.Parallel()
		sess, err := a.Run(context.Background(), mockSpec(dir, domain.RoleEngineer, "", "rate_limited", events.TaskImplement, 0))
		require.NoError(t, err)
		_, _, err = mockCollect(t, sess)
		var ce *cartridge.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, 30*time.Second, ce.RetryAfter)
	})

	t.Run("malformed succeeds with a broken block", func(t *testing.T) {
		t.Parallel()
		sess, err := a.Run(context.Background(), mockSpec(dir, domain.RoleEngineer, "", "malformed", events.TaskImplement, 0))
		require.NoError(t, err)
		evs, res, err := mockCollect(t, sess)
		require.NoError(t, err)
		last := mockAssertStream(t, evs)
		assert.Equal(t, cartridge.EventDone, last.Kind)
		assert.Equal(t, "Here is the result:\n```sc-result\n{\"ok\": tru\n```", res.Output)
		assert.Equal(t, 0, res.ExitCode)
	})

	t.Run("malformed with a custom body", func(t *testing.T) {
		t.Parallel()
		s := NewScenario("m")
		s.Otherwise("m").Fail(FailMalformed, "not even close")
		b, err := NewAdapter(Name, s)
		require.NoError(t, err)
		sess, err := b.Run(context.Background(), mockSpec(dir, domain.RoleEngineer, "", "x", events.TaskImplement, 0))
		require.NoError(t, err)
		_, res, err := mockCollect(t, sess)
		require.NoError(t, err)
		assert.Equal(t, "```sc-result\nnot even close\n```", res.Output)
	})

	t.Run("timeout with a message waits for the spec timeout", func(t *testing.T) {
		t.Parallel()
		clock := NewFakeClock(mockEpoch)
		s := NewScenario("t")
		s.Otherwise("t").Fail(FailTimeout, "the CLI hung")
		b, err := NewAdapter(Name, s, WithClock(clock))
		require.NoError(t, err)
		spec := mockSpec(dir, domain.RoleEngineer, "", "x", events.TaskImplement, 0)
		spec.Timeout = time.Hour
		sess, err := b.Run(context.Background(), spec)
		require.NoError(t, err)
		clock.BlockUntil(1)
		clock.Advance(time.Hour)
		_, res, err := mockCollect(t, sess)
		assert.ErrorIs(t, err, cartridge.ErrTimeout)
		assert.Contains(t, err.Error(), "the CLI hung")
		assert.Equal(t, time.Hour, res.Duration)
	})

	t.Run("injected timeout can still be killed", func(t *testing.T) {
		t.Parallel()
		clock := NewFakeClock(mockEpoch)
		s := NewScenario("t")
		s.Otherwise("t").Fail(FailTimeout, "")
		b, err := NewAdapter(Name, s, WithClock(clock))
		require.NoError(t, err)
		spec := mockSpec(dir, domain.RoleEngineer, "", "x", events.TaskImplement, 0)
		spec.Timeout = time.Hour
		sess, err := b.Run(context.Background(), spec)
		require.NoError(t, err)
		clock.BlockUntil(1)
		sess.Kill()
		_, _, err = mockCollect(t, sess)
		assert.ErrorIs(t, err, cartridge.ErrKilled)

		ctx, cancel := context.WithCancel(context.Background())
		sess, err = b.Run(ctx, spec)
		require.NoError(t, err)
		clock.BlockUntil(1)
		cancel()
		_, _, err = mockCollect(t, sess)
		assert.ErrorIs(t, err, cartridge.ErrKilled)
	})

	t.Run("steps after a failure do not run", func(t *testing.T) {
		t.Parallel()
		s := NewScenario("f")
		s.Otherwise("f").Fail(FailCrash, "boom").Write("after.txt", "x")
		b, err := NewAdapter(Name, s)
		require.NoError(t, err)
		d := t.TempDir()
		sess, err := b.Run(context.Background(), mockSpec(d, domain.RoleEngineer, "", "x", events.TaskImplement, 0))
		require.NoError(t, err)
		_, _, err = mockCollect(t, sess)
		assert.ErrorIs(t, err, cartridge.ErrCrash)
		_, statErr := os.Stat(filepath.Join(d, "after.txt"))
		assert.True(t, os.IsNotExist(statErr))
	})
}

func TestDetect(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	no := false

	t.Run("default", func(t *testing.T) {
		t.Parallel()
		a, err := NewAdapter("mock-x", DefaultScenario())
		require.NoError(t, err)
		st, err := a.Detect(ctx)
		require.NoError(t, err)
		assert.True(t, st.Ready())
		assert.Equal(t, cartridge.Status{Installed: true, Path: "mock-x", Version: "mock/1", Auth: cartridge.AuthOK, Detail: "scenario default"}, st)
		assert.Equal(t, 1, a.Detects())
	})

	t.Run("not installed", func(t *testing.T) {
		t.Parallel()
		s := NewScenario("gone")
		s.Detect.Installed = &no
		a, err := NewAdapter(Name, s)
		require.NoError(t, err)
		st, err := a.Detect(ctx)
		assert.ErrorIs(t, err, cartridge.ErrNotInstalled)
		assert.False(t, st.Installed)
		assert.False(t, st.Ready())
	})

	t.Run("auth missing", func(t *testing.T) {
		t.Parallel()
		s := NewScenario("logged out")
		s.Detect.Auth = cartridge.AuthMissing
		s.Detect.Version = "mock/3"
		a, err := NewAdapter(Name, s)
		require.NoError(t, err)
		st, err := a.Detect(ctx)
		assert.ErrorIs(t, err, cartridge.ErrAuth)
		assert.True(t, st.Installed)
		assert.Equal(t, "mock/3", st.Version)
		assert.Equal(t, cartridge.AuthMissing, st.Auth)
		assert.False(t, st.Ready())
	})

	t.Run("failure kinds", func(t *testing.T) {
		t.Parallel()
		for _, k := range []FailKind{FailCrash, FailTimeout, FailRateLimited, FailCreditsExhausted, FailAuth, FailKilled, FailNotInstalled} {
			s := NewScenario(string(k))
			s.Detect.Failure = &Failure{Kind: k, Message: "probe said so"}
			a, err := NewAdapter(Name, s)
			require.NoError(t, err)
			st, err := a.Detect(ctx)
			require.Error(t, err, k)
			assert.Equal(t, k.CartridgeKind(), cartridge.KindOf(err), k)
			assert.Contains(t, err.Error(), "probe said so")
			switch k {
			case FailNotInstalled:
				assert.False(t, st.Installed)
			case FailAuth:
				assert.True(t, st.Installed)
				assert.Equal(t, cartridge.AuthMissing, st.Auth)
			default:
				assert.True(t, st.Installed, k)
			}
		}
		// Malformed is not an error for Detect.
		s := NewScenario("m")
		s.Detect.Failure = &Failure{Kind: FailMalformed}
		a, err := NewAdapter(Name, s)
		require.NoError(t, err)
		_, err = a.Detect(ctx)
		require.NoError(t, err)
	})

	t.Run("delay under the fake clock and cancellation", func(t *testing.T) {
		t.Parallel()
		clock := NewFakeClock(mockEpoch)
		a := mockLoad(t, "detect-credits.json", WithClock(clock))
		type out struct {
			st  cartridge.Status
			err error
		}
		ch := make(chan out, 1)
		go func() {
			st, err := a.Detect(ctx)
			ch <- out{st, err}
		}()
		clock.BlockUntil(1)
		select {
		case <-ch:
			t.Fatal("answered before the delay")
		case <-time.After(20 * time.Millisecond):
		}
		clock.Advance(time.Second)
		o := <-ch
		assert.ErrorIs(t, o.err, cartridge.ErrCreditsExhausted)
		assert.Equal(t, "mock/2", o.st.Version)
		assert.True(t, o.st.Ready(), "status alone says installed and logged in; the error says dead")

		cctx, cancel := context.WithCancel(ctx)
		go func() {
			clock.BlockUntil(1)
			cancel()
		}()
		_, err := a.Detect(cctx)
		assert.ErrorIs(t, err, cartridge.ErrTimeout)
		assert.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, 2, a.Detects())

		// Sessions of a dead cartridge fail the same way.
		sess, err := a.Run(ctx, mockSpec(t.TempDir(), domain.RolePO, "", "", events.TaskPlan, 0))
		require.NoError(t, err)
		_, _, err = mockCollect(t, sess)
		assert.ErrorIs(t, err, cartridge.ErrCreditsExhausted)
	})
}

func TestConcurrentSessions(t *testing.T) {
	t.Parallel()
	s := NewScenario("many")
	s.Rule("once", Match{}).Once().Say("first")
	s.Otherwise("rest").Say("rest").Tool("Read", "", "x").Tool("Read", "", "y")
	a, err := NewAdapter(Name, s)
	require.NoError(t, err)
	dir := t.TempDir()
	const n = 20
	var wg sync.WaitGroup
	outputs := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sess, err := a.Run(context.Background(), mockSpec(dir, domain.RoleEngineer, "", "sc-1", events.TaskImplement, i))
			if !assert.NoError(t, err) {
				return
			}
			evs, res, err := mockCollect(t, sess)
			assert.NoError(t, err)
			ids := map[string]bool{}
			for _, ev := range evs {
				if ev.Kind == cartridge.EventToolUse {
					assert.False(t, ids[ev.ToolID], "tool ids are unique within a session")
					ids[ev.ToolID] = true
				}
			}
			outputs[i] = res.Output
		}(i)
	}
	wg.Wait()
	firsts := 0
	for _, o := range outputs {
		if o == "first" {
			firsts++
		} else {
			assert.Equal(t, "rest", o)
		}
	}
	assert.Equal(t, 1, firsts, "a once-only rule answers exactly one concurrent session")
	assert.Len(t, a.Calls(), n)
	assert.Equal(t, 1, a.Uses("once"))
	assert.Equal(t, n-1, a.Uses("rest"))
	seen := map[int]bool{}
	for _, c := range a.Calls() {
		assert.False(t, seen[c.Seq])
		seen[c.Seq] = true
	}
}

func TestWaitWithoutDrainingDoesNotBlock(t *testing.T) {
	t.Parallel()
	s := NewScenario("chatty")
	r := s.Otherwise("chatty")
	for i := 0; i < 500; i++ {
		r.Say("x")
	}
	a, err := NewAdapter(Name, s)
	require.NoError(t, err)
	sess, err := a.Run(context.Background(), mockSpec(t.TempDir(), domain.RolePO, "", "", events.TaskPlan, 0))
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		defer close(done)
		res, err := sess.Wait()
		assert.NoError(t, err)
		assert.Len(t, res.Output, 500)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait blocked on an undrained channel")
	}
}
