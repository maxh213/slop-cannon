package cartridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/proc"
)

// specFakeCartridge is the minimal in-package adapter used by the registry
// and session tests. Real adapters live in subpackages.
type specFakeCartridge struct {
	name string
	cfg  config.Cartridge
	// script is played by Run: every event is emitted, then Finish is
	// called with result and err.
	script []SessionEvent
	result Result
	err    error
	killed chan struct{}
}

// Compile-time check every adapter must carry.
var _ Cartridge = (*specFakeCartridge)(nil)

func (f *specFakeCartridge) Name() string { return f.name }

func (f *specFakeCartridge) Detect(context.Context) (Status, error) {
	return Status{Installed: true, Path: "/usr/bin/" + f.name, Version: "1.0.0", Auth: AuthOK}, nil
}

func (f *specFakeCartridge) Run(_ context.Context, spec RunSpec) (Session, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	p := NewPipe(f.name, func() {
		if f.killed != nil {
			close(f.killed)
		}
	})
	go func() {
		for _, ev := range f.script {
			p.Emit(ev)
		}
		p.Finish(f.result, f.err)
	}()
	return p, nil
}

func specFakeFactory(name string, cfg config.Cartridge) (Cartridge, error) {
	return &specFakeCartridge{name: name, cfg: cfg}, nil
}

func specValidSpec(t *testing.T) RunSpec {
	t.Helper()
	return RunSpec{
		Prompt:     "say hi",
		Workdir:    t.TempDir(),
		ToolPolicy: ToolReadOnly,
		Timeout:    time.Minute,
	}
}

// --- ToolPolicy ---------------------------------------------------------

func TestToolPolicyParseAndMarshal(t *testing.T) {
	cases := []struct {
		in   string
		want ToolPolicy
		ok   bool
	}{
		{"read_only", ToolReadOnly, true},
		{"READ-EXEC", ToolReadExec, true},
		{"  read_write ", ToolReadWrite, true},
		{"", "", false},
		{"write", "", false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.in), func(t *testing.T) {
			got, err := ParseToolPolicy(tc.in)
			if !tc.ok {
				require.Error(t, err)
				assert.ErrorIs(t, err, domain.ErrUnknownValue)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.True(t, got.Valid())
		})
	}
	assert.Equal(t, []ToolPolicy{ToolReadOnly, ToolReadExec, ToolReadWrite}, ToolPolicies())

	// JSON round trip through a struct, including the zero value.
	type wrap struct {
		P ToolPolicy `json:"p"`
	}
	for _, p := range append(ToolPolicies(), "") {
		b, err := json.Marshal(wrap{P: p})
		require.NoError(t, err)
		var back wrap
		require.NoError(t, json.Unmarshal(b, &back))
		assert.Equal(t, p, back.P)
	}
	_, err := json.Marshal(wrap{P: "bogus"})
	assert.Error(t, err)
	var bad wrap
	assert.Error(t, json.Unmarshal([]byte(`{"p":"bogus"}`), &bad))

	assert.False(t, ToolReadOnly.AllowsExec())
	assert.False(t, ToolReadOnly.AllowsWrite())
	assert.True(t, ToolReadExec.AllowsExec())
	assert.False(t, ToolReadExec.AllowsWrite())
	assert.True(t, ToolReadWrite.AllowsExec())
	assert.True(t, ToolReadWrite.AllowsWrite())
	assert.Equal(t, "read_exec", ToolReadExec.String())
}

// --- RunSpec ------------------------------------------------------------

func TestRunSpecValidate(t *testing.T) {
	ok := specValidSpec(t)
	require.NoError(t, ok.Validate())

	cases := []struct {
		name   string
		mutate func(*RunSpec)
		want   string
	}{
		{"empty prompt", func(s *RunSpec) { s.Prompt = "  " }, "empty prompt"},
		{"no workdir", func(s *RunSpec) { s.Workdir = "" }, "no workdir"},
		{"relative workdir", func(s *RunSpec) { s.Workdir = "rel/dir" }, "not absolute"},
		{"no policy", func(s *RunSpec) { s.ToolPolicy = "" }, "no tool policy"},
		{"bad policy", func(s *RunSpec) { s.ToolPolicy = "yolo" }, `tool policy "yolo"`},
		{"negative turns", func(s *RunSpec) { s.MaxTurns = -1 }, "max turns -1"},
		{"negative timeout", func(s *RunSpec) { s.Timeout = -time.Second }, "timeout -1s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := ok
			tc.mutate(&s)
			err := s.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestRunSpecEnvAllowlistBuildsCleanEnvironment(t *testing.T) {
	t.Setenv("SC_TEST_SECRET", "leak")
	t.Setenv("SC_TEST_ALLOWED", "keep")
	t.Setenv("HOME", "/tmp/spec-home")

	s := specValidSpec(t)
	s.Env = map[string]string{"SC_TEST_EXPLICIT": "yes", "HOME": "/override"}

	// Nil InheritEnv means the default allowlist.
	assert.Equal(t, DefaultInheritEnv(), s.EnvAllowlist())
	ps := s.ProcSpec("claude", "-p", "hi")
	assert.Equal(t, "claude", ps.Command)
	assert.Equal(t, []string{"-p", "hi"}, ps.Args)
	assert.Equal(t, s.Workdir, ps.Dir)
	assert.Equal(t, s.Timeout, ps.Timeout)
	env := ps.Environ()
	_, leaked := proc.EnvValue(env, "SC_TEST_SECRET")
	assert.False(t, leaked, "unlisted variable must not reach the child")
	_, allowedByDefault := proc.EnvValue(env, "SC_TEST_ALLOWED")
	assert.False(t, allowedByDefault)
	home, _ := proc.EnvValue(env, "HOME")
	assert.Equal(t, "/override", home, "explicit Env wins over inherited")
	explicit, _ := proc.EnvValue(env, "SC_TEST_EXPLICIT")
	assert.Equal(t, "yes", explicit)
	_, hasPath := proc.EnvValue(env, "PATH")
	assert.True(t, hasPath, "PATH is always inherited")

	// An explicit allowlist replaces the default entirely.
	s.InheritEnv = []string{"SC_TEST_ALLOWED"}
	env = s.ProcSpec("x").Environ()
	v, ok := proc.EnvValue(env, "SC_TEST_ALLOWED")
	assert.True(t, ok)
	assert.Equal(t, "keep", v)
	_, hasUser := proc.EnvValue(env, "USER")
	assert.False(t, hasUser)

	// An empty, non-nil allowlist inherits nothing but PATH and Env.
	s.InheritEnv = []string{}
	s.Env = nil
	env = s.ProcSpec("x").Environ()
	assert.Len(t, env, 1)
	assert.True(t, strings.HasPrefix(env[0], "PATH="))

	// The returned copies are independent of the spec.
	s.InheritEnv = []string{"A"}
	got := s.EnvAllowlist()
	got[0] = "B"
	assert.Equal(t, []string{"A"}, s.InheritEnv)
	s.Env = map[string]string{"K": "v"}
	ps = s.ProcSpec("x")
	ps.Env["K"] = "changed"
	assert.Equal(t, "v", s.Env["K"])
}

func TestRunSpecEnvAllowlistRunsRealProcess(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	t.Setenv("SC_TEST_SECRET", "leak")
	t.Setenv("SC_TEST_ALLOWED", "keep")
	s := specValidSpec(t)
	s.InheritEnv = []string{"SC_TEST_ALLOWED"}
	s.Env = map[string]string{"SC_TEST_EXPLICIT": "yes"}
	res, err := proc.Run(context.Background(), s.ProcSpec("sh", "-c", "env"))
	require.NoError(t, err)
	assert.Contains(t, res.Stdout, "SC_TEST_ALLOWED=keep")
	assert.Contains(t, res.Stdout, "SC_TEST_EXPLICIT=yes")
	assert.NotContains(t, res.Stdout, "SC_TEST_SECRET")
}

func TestLabels(t *testing.T) {
	var nilLabels Labels
	assert.Equal(t, "", nilLabels.Get("x"))
	assert.Nil(t, nilLabels.Clone())
	assert.Empty(t, nilLabels.Keys())

	a := domain.Agent{ID: "tester-qa-sc-1-r2", Role: domain.RoleTester, Lens: domain.LensQA, Bead: "sc-1"}
	l := LabelsFor("run-1", a, events.TaskReview, 2)
	assert.Equal(t, Labels{
		LabelRun: "run-1", LabelAgent: "tester-qa-sc-1-r2", LabelRole: "tester",
		LabelLens: "qa", LabelBead: "sc-1", LabelRound: "2", LabelTask: "review",
	}, l)
	assert.Equal(t, []string{"agent", "bead", "lens", "role", "round", "run", "task"}, l.Keys())

	c := l.Clone()
	c[LabelRole] = "engineer"
	assert.Equal(t, "tester", l.Get(LabelRole))

	po := LabelsFor("", domain.Agent{ID: "po-1", Role: domain.RolePO}, events.TaskPlan, 0)
	assert.Equal(t, Labels{LabelAgent: "po-1", LabelRole: "po", LabelTask: "plan"}, po)
}

func TestUsageAddAndTotal(t *testing.T) {
	a := Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 1, Turns: 1}
	b := Usage{InputTokens: 20, OutputTokens: 7, CacheWriteTokens: 2, CostUSD: 0.5, HasCost: true, Turns: 3}
	sum := a.Add(b)
	assert.Equal(t, Usage{InputTokens: 30, OutputTokens: 12, CacheReadTokens: 1, CacheWriteTokens: 2, CostUSD: 0.5, HasCost: true, Turns: 4}, sum)
	assert.Equal(t, 42, sum.TotalTokens())
	assert.False(t, a.Add(Usage{}).HasCost)
}

// --- Error kinds --------------------------------------------------------

func TestKindSentinelsAndErrorsIs(t *testing.T) {
	want := map[Kind]error{
		KindRateLimited:      ErrRateLimited,
		KindCreditsExhausted: ErrCreditsExhausted,
		KindAuth:             ErrAuth,
		KindCrash:            ErrCrash,
		KindTimeout:          ErrTimeout,
		KindKilled:           ErrKilled,
		KindNotInstalled:     ErrNotInstalled,
	}
	for _, k := range Kinds() {
		assert.True(t, k.Valid(), k)
		s, has := want[k]
		if k == KindUnknown {
			assert.Nil(t, k.Sentinel())
			assert.False(t, has)
			continue
		}
		require.True(t, has, "kind %s has no sentinel in the test table", k)
		assert.Same(t, s, k.Sentinel())

		e := NewError(k, "claude", "boom", nil)
		assert.ErrorIs(t, e, s, k)
		assert.Equal(t, k, KindOf(e))
		assert.Equal(t, k, KindOf(fmt.Errorf("wrapped: %w", e)))
		assert.Equal(t, k, KindOf(s), "bare sentinel")
		assert.Equal(t, k, KindOf(fmt.Errorf("outer: %w", s)))
		for other, os := range want {
			if other != k {
				assert.False(t, errors.Is(e, os), "%s must not match %s", k, other)
			}
		}
	}
	assert.False(t, Kind("nope").Valid())
	assert.Equal(t, "unknown", Kind("").String())
	assert.Equal(t, "auth", KindAuth.String())
}

func TestKindRetryableAndFailure(t *testing.T) {
	assert.True(t, KindRateLimited.Retryable())
	assert.True(t, KindCrash.Retryable())
	assert.True(t, KindTimeout.Retryable())
	assert.True(t, KindUnknown.Retryable())
	assert.False(t, KindCreditsExhausted.Retryable())
	assert.False(t, KindAuth.Retryable())
	assert.False(t, KindKilled.Retryable())
	assert.False(t, KindNotInstalled.Retryable())

	assert.Equal(t, events.FailureRateLimited, KindRateLimited.Failure())
	assert.Equal(t, events.FailureCreditsExhausted, KindCreditsExhausted.Failure())
	assert.Equal(t, events.FailureTimeout, KindTimeout.Failure())
	assert.Equal(t, events.FailureKilled, KindKilled.Failure())
	assert.Equal(t, events.FailureCrash, KindCrash.Failure())
	assert.Equal(t, events.FailureCrash, KindAuth.Failure())
	assert.Equal(t, events.FailureCrash, KindUnknown.Failure())
}

func TestKindOfClassifiesProcAndContextErrors(t *testing.T) {
	assert.Equal(t, KindUnknown, KindOf(nil))
	assert.Equal(t, KindUnknown, KindOf(errors.New("plain")))
	assert.Equal(t, KindTimeout, KindOf(proc.ErrTimeout))
	assert.Equal(t, KindTimeout, KindOf(context.DeadlineExceeded))
	assert.Equal(t, KindKilled, KindOf(proc.ErrKilled))
	assert.Equal(t, KindKilled, KindOf(context.Canceled))
	assert.Equal(t, KindCrash, KindOf(&proc.ExitError{Command: "x", Result: proc.Result{ExitCode: 2}}))
	assert.Equal(t, KindTimeout, KindOf(&proc.ExitError{Command: "x", Cause: proc.ErrTimeout}))

	// An unknown-kind Error defers to its cause.
	assert.Equal(t, KindTimeout, KindOf(NewError(KindUnknown, "", "", proc.ErrTimeout)))
	assert.Equal(t, KindUnknown, KindOf(NewError("", "", "", nil)))
	// A classified Error wins over its cause.
	assert.Equal(t, KindRateLimited, KindOf(NewError(KindRateLimited, "", "", proc.ErrTimeout)))
}

func TestErrorStringAndUnwrap(t *testing.T) {
	cause := errors.New("exit status 1")
	e := NewError(KindCrash, "claude", "stream ended early", cause)
	assert.Equal(t, "cartridge claude: crashed: stream ended early: exit status 1", e.Error())
	assert.ErrorIs(t, e, cause)
	assert.Same(t, cause, errors.Unwrap(e))

	assert.Equal(t, "cartridge: error", NewError(KindUnknown, "", "", nil).Error())
	assert.Equal(t, "cartridge mock: rate limited", NewError(KindRateLimited, "mock", "", nil).Error())
	assert.Equal(t, "cartridge: not authenticated: run claude login", NewError(KindAuth, "", "run claude login", nil).Error())

	e.RetryAfter = 30 * time.Second
	assert.Equal(t, 30*time.Second, e.RetryAfter)
}

func TestAsError(t *testing.T) {
	// nil becomes a terminal unknown error.
	e := AsError("mock", nil)
	require.NotNil(t, e)
	assert.Equal(t, KindUnknown, e.Kind)
	assert.Equal(t, "mock", e.Cartridge)

	// An existing *Error passes through, gaining the name only when it has none.
	orig := NewError(KindAuth, "", "", nil)
	got := AsError("claude", orig)
	assert.Equal(t, "claude", got.Cartridge)
	assert.Equal(t, "", orig.Cartridge, "original must not be mutated")
	named := NewError(KindAuth, "kimi", "", nil)
	assert.Same(t, named, AsError("claude", named))
	wrapped := fmt.Errorf("ctx: %w", named)
	assert.Same(t, named, AsError("", wrapped))

	// Plain errors are classified; unknown ones become crashes.
	assert.Equal(t, KindTimeout, AsError("", proc.ErrTimeout).Kind)
	bare := AsError("m", ErrRateLimited)
	assert.Equal(t, KindRateLimited, bare.Kind)
	assert.Nil(t, bare.Cause, "a bare sentinel is not kept as its own cause")
	assert.Equal(t, "cartridge m: rate limited", bare.Error())
	wrappedSentinel := AsError("m", fmt.Errorf("429 from api: %w", ErrRateLimited))
	assert.Equal(t, KindRateLimited, wrappedSentinel.Kind)
	assert.Equal(t, "cartridge m: rate limited: 429 from api: cartridge: rate limited", wrappedSentinel.Error())
	assert.Equal(t, KindKilled, AsError("", context.Canceled).Kind)
	plain := errors.New("weird")
	pe := AsError("m", plain)
	assert.Equal(t, KindCrash, pe.Kind)
	assert.ErrorIs(t, pe, plain)
	assert.ErrorIs(t, pe, ErrCrash)
}

// --- Status -------------------------------------------------------------

func TestStatusReadyAndAuthState(t *testing.T) {
	assert.False(t, Status{}.Ready())
	assert.True(t, Status{Installed: true}.Ready(), "unknown auth is assumed usable")
	assert.True(t, Status{Installed: true, Auth: AuthOK}.Ready())
	assert.False(t, Status{Installed: true, Auth: AuthMissing}.Ready())
	assert.Equal(t, "unknown", AuthUnknown.String())
	assert.Equal(t, "ok", AuthOK.String())
	assert.Equal(t, "missing", AuthMissing.String())
}

// --- Session events -----------------------------------------------------

func TestSessionEventConstructors(t *testing.T) {
	td := TextDelta("hi")
	assert.Equal(t, EventTextDelta, td.Kind)
	assert.Equal(t, "hi", td.Text)
	assert.False(t, td.At.IsZero())
	assert.False(t, td.Kind.Terminal())

	tu := ToolUse("Bash", "t1", "ls")
	assert.Equal(t, EventToolUse, tu.Kind)
	assert.Equal(t, "Bash", tu.Tool)
	assert.Equal(t, "t1", tu.ToolID)
	assert.Equal(t, "ls", tu.Input)

	tr := ToolResult("Bash", "t1", "a.go", true)
	assert.Equal(t, EventToolResult, tr.Kind)
	assert.True(t, tr.IsError)
	assert.Equal(t, "a.go", tr.Output)

	res := Result{Output: "done", Usage: Usage{InputTokens: 3}}
	d := Done(res)
	assert.True(t, d.Kind.Terminal())
	require.NotNil(t, d.Result)
	assert.Equal(t, res, *d.Result)
	assert.Equal(t, res.Usage, d.Usage)
	res.Output = "changed"
	assert.Equal(t, "done", d.Result.Output, "Done copies the result")

	e := ErrorEvent(proc.ErrTimeout, Usage{OutputTokens: 1})
	assert.True(t, e.Kind.Terminal())
	assert.ErrorIs(t, e.Err, ErrTimeout)
	assert.Equal(t, 1, e.Usage.OutputTokens)
	var ce *Error
	require.ErrorAs(t, ErrorEvent(nil, Usage{}).Err, &ce)
	assert.Equal(t, KindUnknown, ce.Kind)

	// Err is excluded from JSON, the rest round-trips.
	b, err := json.Marshal(tr)
	require.NoError(t, err)
	assert.NotContains(t, string(b), `"err"`)
	var back SessionEvent
	require.NoError(t, json.Unmarshal(b, &back))
	assert.Equal(t, tr.Tool, back.Tool)
	assert.Equal(t, tr.IsError, back.IsError)
	assert.Equal(t, "done", EventDone.String())
}

func TestDescribeAndTranscript(t *testing.T) {
	evs := []SessionEvent{
		TextDelta("hello "),
		ToolUse("Read", "1", "main.go"),
		ToolUse("Read", "2", ""),
		ToolResult("Read", "1", "package main", false),
		ToolResult("Read", "2", "nope", true),
		TextDelta("world"),
		Done(Result{Usage: Usage{InputTokens: 2, OutputTokens: 1}, Duration: 1500 * time.Millisecond}),
	}
	assert.Equal(t, "hello world", Transcript(evs))
	assert.Equal(t, "hello ", Describe(evs[0]))
	assert.Equal(t, "[tool Read] main.go", Describe(evs[1]))
	assert.Equal(t, "[tool Read]", Describe(evs[2]))
	assert.Equal(t, "[result Read ok] package main", Describe(evs[3]))
	assert.Equal(t, "[result Read error] nope", Describe(evs[4]))
	assert.Equal(t, "[done] 2 in / 1 out tokens in 1.5s", Describe(evs[6]))
	assert.Equal(t, "[done]", Describe(SessionEvent{Kind: EventDone}))
	assert.Equal(t, "[error]", Describe(SessionEvent{Kind: EventError}))
	assert.Equal(t, "[error] cartridge: rate limited", Describe(ErrorEvent(ErrRateLimited, Usage{})))
	assert.Equal(t, "[weird]", Describe(SessionEvent{Kind: "weird"}))
}

// --- Pipe ---------------------------------------------------------------

func TestPipeDeliversEventsInOrderAndAppendsDone(t *testing.T) {
	p := NewPipe("mock", nil)
	require.True(t, p.Emit(TextDelta("a")))
	require.True(t, p.Emit(ToolUse("Bash", "1", "ls")))
	require.True(t, p.Emit(SessionEvent{Kind: EventTextDelta, Text: "b"}), "zero At is allowed")
	res := Result{Output: "ab", Usage: Usage{InputTokens: 1}, ExitCode: 0}
	p.Finish(res, nil)
	p.Finish(Result{Output: "ignored"}, errors.New("ignored"))

	var got []SessionEvent
	out, err := Collect(p, func(ev SessionEvent) { got = append(got, ev) })
	require.NoError(t, err)
	assert.Equal(t, res, out)
	require.Len(t, got, 4)
	assert.Equal(t, "a", got[0].Text)
	assert.Equal(t, EventToolUse, got[1].Kind)
	assert.Equal(t, "b", got[2].Text)
	assert.False(t, got[2].At.IsZero())
	assert.Equal(t, EventDone, got[3].Kind)
	assert.Equal(t, res, *got[3].Result)

	// Wait is idempotent and Emit after Finish is rejected.
	out2, err2 := p.Wait()
	assert.Equal(t, out, out2)
	assert.NoError(t, err2)
	assert.False(t, p.Emit(TextDelta("late")))
	select {
	case <-p.Done():
	default:
		t.Fatal("Done should be closed")
	}
}

func TestPipeFinishWithErrorAppendsErrorEvent(t *testing.T) {
	p := NewPipe("claude", nil)
	p.Emit(TextDelta("partial"))
	p.Finish(Result{Usage: Usage{OutputTokens: 7}}, &proc.ExitError{Command: "claude", Result: proc.Result{ExitCode: 1}})

	var got []SessionEvent
	res, err := Collect(p, func(ev SessionEvent) { got = append(got, ev) })
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCrash)
	var ce *Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, "claude", ce.Cartridge)
	assert.Equal(t, 7, res.Usage.OutputTokens)
	require.Len(t, got, 2)
	assert.Equal(t, EventError, got[1].Kind)
	assert.ErrorIs(t, got[1].Err, ErrCrash)
	assert.Equal(t, 7, got[1].Usage.OutputTokens)
}

func TestPipeRespectsProducerTerminalEvent(t *testing.T) {
	p := NewPipe("mock", nil)
	p.Emit(TextDelta("x"))
	require.True(t, p.Emit(SessionEvent{Kind: EventError, Err: errors.New("bad json")}))
	assert.False(t, p.Emit(TextDelta("after terminal")), "nothing after a terminal event")
	p.Finish(Result{}, ErrCrash)

	var got []SessionEvent
	_, err := Collect(p, func(ev SessionEvent) { got = append(got, ev) })
	assert.ErrorIs(t, err, ErrCrash)
	require.Len(t, got, 2, "no second terminal event is appended")
	assert.Equal(t, EventError, got[1].Kind)
	var ce *Error
	require.ErrorAs(t, got[1].Err, &ce, "plain errors on Error events are wrapped")
	assert.Equal(t, "mock", ce.Cartridge)
	assert.Equal(t, KindCrash, ce.Kind)

	// A producer Done followed by Finish(nil) keeps the single Done.
	p2 := NewPipe("mock", nil)
	p2.Emit(Done(Result{Output: "ok"}))
	p2.Finish(Result{Output: "ok"}, nil)
	n := 0
	_, err = Collect(p2, func(SessionEvent) { n++ })
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestPipeKillRunsCallbackOnceAndReportsKilled(t *testing.T) {
	calls := 0
	p := NewPipe("mock", func() { calls++ })
	assert.False(t, p.Killed())
	p.Kill()
	p.Kill()
	assert.Equal(t, 1, calls)
	assert.True(t, p.Killed())

	// A clean exit after Kill still reports ErrKilled.
	p.Finish(Result{ExitCode: 0}, nil)
	_, err := Collect(p, nil)
	assert.ErrorIs(t, err, ErrKilled)
	assert.Equal(t, KindKilled, KindOf(err))

	// A nil callback is fine.
	p2 := NewPipe("mock", nil)
	p2.Kill()
	p2.Finish(Result{}, proc.ErrKilled)
	_, err = p2.Wait()
	assert.ErrorIs(t, err, ErrKilled)
}

func TestPipeWaitNeverDeadlocksWithoutConsumer(t *testing.T) {
	p := NewPipe("mock", nil)
	for i := 0; i < 1000; i++ {
		p.Emit(TextDelta("x"))
	}
	p.Finish(Result{Output: "x"}, nil)
	done := make(chan struct{})
	go func() {
		_, _ = p.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait blocked on an undrained event channel")
	}
	n := 0
	for range p.Events() {
		n++
	}
	assert.Equal(t, 1001, n)
}

func TestPipeConcurrentProducers(t *testing.T) {
	p := NewPipe("mock", nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				p.Emit(TextDelta("t"))
			}
		}()
	}
	go func() {
		wg.Wait()
		p.Finish(Result{}, nil)
	}()
	n := 0
	_, err := Collect(p, func(SessionEvent) { n++ })
	require.NoError(t, err)
	assert.Equal(t, 401, n)
}

// --- Cartridge through the interface ------------------------------------

func TestFakeCartridgeRunThroughInterface(t *testing.T) {
	var c Cartridge = &specFakeCartridge{
		name:   "mock",
		script: []SessionEvent{TextDelta("hi")},
		result: Result{Output: "hi", Usage: Usage{InputTokens: 1, OutputTokens: 1}},
	}
	st, err := c.Detect(context.Background())
	require.NoError(t, err)
	assert.True(t, st.Ready())

	_, err = c.Run(context.Background(), RunSpec{})
	require.Error(t, err, "invalid spec is rejected before anything starts")

	s, err := c.Run(context.Background(), specValidSpec(t))
	require.NoError(t, err)
	var evs []SessionEvent
	res, err := Collect(s, func(ev SessionEvent) { evs = append(evs, ev) })
	require.NoError(t, err)
	assert.Equal(t, "hi", res.Output)
	assert.Equal(t, res.Output, Transcript(evs))
	assert.Equal(t, EventDone, evs[len(evs)-1].Kind)
}

func TestFakeCartridgeKill(t *testing.T) {
	f := &specFakeCartridge{name: "mock", killed: make(chan struct{}), err: ErrKilled}
	s, err := f.Run(context.Background(), specValidSpec(t))
	require.NoError(t, err)
	s.Kill()
	<-f.killed
	_, err = Collect(s, nil)
	assert.ErrorIs(t, err, ErrKilled)
}

// --- Registry -----------------------------------------------------------

func TestRegistryRegisterAndNew(t *testing.T) {
	r := NewRegistry()
	assert.Empty(t, r.Names())
	assert.False(t, r.Has("claude"))

	require.NoError(t, r.Register("claude", specFakeFactory))
	require.NoError(t, r.Register("mock", specFakeFactory))
	assert.Equal(t, []string{"claude", "mock"}, r.Names())
	assert.True(t, r.Has("claude"))

	err := r.Register("claude", specFakeFactory)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"claude" is already registered`)
	assert.Error(t, r.Register("", specFakeFactory))
	assert.Error(t, r.Register("  ", specFakeFactory))
	assert.Error(t, r.Register("nilfactory", nil))
	assert.Panics(t, func() { r.MustRegister("claude", specFakeFactory) })
	assert.NotPanics(t, func() { r.MustRegister("third", specFakeFactory) })

	cfg := config.Cartridge{Model: "fable", Args: []string{"--verbose"}}
	c, err := r.New("claude", cfg)
	require.NoError(t, err)
	assert.Equal(t, "claude", c.Name())
	fake, ok := c.(*specFakeCartridge)
	require.True(t, ok)
	assert.Equal(t, cfg, fake.cfg, "factory receives the config")

	_, err = r.New("kimi", config.Cartridge{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnknownCartridge)
	assert.Contains(t, err.Error(), `"kimi" (registered: claude, mock, third)`)

	_, err = NewRegistry().New("x", config.Cartridge{})
	assert.Contains(t, err.Error(), "(registered: none)")
}

func TestRegistryFactoryErrors(t *testing.T) {
	r := NewRegistry()
	boom := errors.New("boom")
	require.NoError(t, r.Register("bad", func(string, config.Cartridge) (Cartridge, error) { return nil, boom }))
	require.NoError(t, r.Register("nil", func(string, config.Cartridge) (Cartridge, error) { return nil, nil }))
	_, err := r.New("bad", config.Cartridge{})
	assert.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), `cartridge "bad": boom`)
	_, err = r.New("nil", config.Cartridge{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "factory returned nil")
}

func TestRegistryBuildFromConfig(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Register("claude", specFakeFactory))

	cfg := config.Defaults()
	built, err := r.Build(cfg)
	require.NoError(t, err)
	assert.NotNil(t, built)
	assert.Empty(t, built)

	cfg.Cartridges = map[string]config.Cartridge{"claude": {Model: "fable"}}
	cfg.ActiveCartridge = "claude"
	built, err = r.Build(cfg)
	require.NoError(t, err)
	require.Len(t, built, 1)
	builtFake, ok := built["claude"].(*specFakeCartridge)
	require.True(t, ok)
	assert.Equal(t, "fable", builtFake.cfg.Model)

	cfg.Cartridges["kimi"] = config.Cartridge{Model: "3"}
	_, err = r.Build(cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnknownCartridge)
	assert.Contains(t, err.Error(), `"kimi"`)
}

func TestRegistryConcurrentAccess(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = r.Register(fmt.Sprintf("c%02d", i), specFakeFactory)
			_ = r.Names()
			_, _ = r.New(fmt.Sprintf("c%02d", i), config.Cartridge{})
			_ = r.Has("c00")
		}(i)
	}
	wg.Wait()
	assert.Len(t, r.Names(), 20)
}

func TestDefaultRegistryWrappers(t *testing.T) {
	// Use a unique name so other packages' init registrations are unaffected.
	name := fmt.Sprintf("spec-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	require.False(t, Default.Has(name))
	require.NoError(t, Register(name, specFakeFactory))
	assert.Contains(t, Names(), name)
	c, err := New(name, config.Cartridge{Model: "m"})
	require.NoError(t, err)
	assert.Equal(t, name, c.Name())
	cfg := config.Defaults()
	cfg.Cartridges = map[string]config.Cartridge{name: {}}
	built, err := Build(cfg)
	require.NoError(t, err)
	assert.Len(t, built, 1)
	assert.Panics(t, func() { MustRegister(name, specFakeFactory) })
}
