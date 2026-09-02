package conformance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	_ "slop-cannon/internal/cartridge/claude" // registers "claude" for CARTRIDGE=claude
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/config"
)

// conformanceDefaultModels names the cheapest model per real cartridge for
// the driver test when SC_CONFORMANCE_MODEL is not set.
var conformanceDefaultModels = map[string]string{
	"claude": "claude-haiku-4-5-20251001",
}

// conformanceFastOptions are Options that make a mock run finish in well
// under a second.
func conformanceFastOptions(t *testing.T, token string) Options {
	t.Helper()
	return Options{
		Token:       token,
		Timeout:     10 * time.Second,
		HangTimeout: 100 * time.Millisecond,
		KillAfter:   50 * time.Millisecond,
		BD:          conformanceFakeBD(t),
	}
}

// conformanceStatuses maps check name to status for assertions.
func conformanceStatuses(rep Report) map[string]Status {
	out := map[string]Status{}
	for _, r := range rep.Results {
		out[r.Check] = r.Status
	}
	return out
}

// conformanceResult returns the named result.
func conformanceResult(t testing.TB, rep Report, name string) Result {
	t.Helper()
	for _, r := range rep.Results {
		if r.Check == name {
			return r
		}
	}
	t.Fatalf("no result for check %q", name)
	return Result{}
}

// TestConformance is the driver behind `make cartridge-conformance`. With
// CARTRIDGE unset or "mock" it runs the suite against a mock built from
// configuration (a scenario file named through SC_MOCK_SCENARIO), which is
// what the normal test run and CI execute. Any other name builds that
// cartridge from the default registry and fires the real CLI; that needs
// the binary, a login and a network, so it is skipped when CI is set.
// SC_CONFORMANCE_MODEL picks the model (a cheap default per cartridge),
// SC_CONFORMANCE_CHECKS a comma-separated subset of the checks.
func TestConformance(t *testing.T) {
	name := strings.TrimSpace(os.Getenv("CARTRIDGE"))
	if name == "" {
		name = mock.Name
	}
	var (
		c    cartridge.Cartridge
		opts Options
		err  error
	)
	if name == mock.Name {
		token := NewToken()
		path := filepath.Join(t.TempDir(), "conformance.json")
		require.NoError(t, MockScenario(token).WriteFile(path))
		c, err = cartridge.New(mock.Name, config.Cartridge{Env: map[string]string{mock.ScenarioEnv: path}})
		require.NoError(t, err)
		opts = conformanceFastOptions(t, token)
	} else {
		if os.Getenv("CI") != "" {
			t.Skipf("CARTRIDGE=%s needs the real CLI; skipped in CI", name)
		}
		model := os.Getenv("SC_CONFORMANCE_MODEL")
		if model == "" {
			model = conformanceDefaultModels[name]
		}
		c, err = cartridge.New(name, config.Cartridge{Model: model})
		require.NoError(t, err)
		opts = Options{Observer: func(check string, ev cartridge.SessionEvent) {
			t.Logf("[%s] %s", check, cartridge.Describe(ev))
		}}
	}
	if sel := strings.TrimSpace(os.Getenv("SC_CONFORMANCE_CHECKS")); sel != "" {
		opts.Checks = strings.Split(sel, ",")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	rep, err := Run(ctx, c, opts)
	require.NoError(t, err)
	// The table goes to stdout so `make cartridge-conformance` shows it
	// whatever the verbosity; -v repeats it in the test log.
	fmt.Print(rep.Table())
	if err := rep.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestRun_MockPassesEveryCheck(t *testing.T) {
	t.Parallel()
	const token = "sc-0123456789ab"
	a, err := NewMock("", token)
	require.NoError(t, err)

	var mu sync.Mutex
	seen := map[string]int{}
	opts := conformanceFastOptions(t, token)
	opts.Observer = func(check string, _ cartridge.SessionEvent) {
		mu.Lock()
		seen[check]++
		mu.Unlock()
	}
	rep, err := Run(context.Background(), a, opts)
	require.NoError(t, err)
	assert.True(t, rep.Passed(), rep.Table())
	require.NoError(t, rep.Err())
	assert.Equal(t, mock.Name, rep.Cartridge)
	assert.Equal(t, token, rep.Token)
	assert.True(t, rep.Status.Ready())
	assert.Equal(t, "mock/1", rep.Status.Version)
	assert.Len(t, rep.Results, len(checks))
	for i, r := range rep.Results {
		assert.Equal(t, checks[i].Name, r.Check)
		assert.Equal(t, StatusPass, r.Status, "%s: %v", r.Check, r.Err)
		assert.NoError(t, r.Err)
		assert.NotEmpty(t, r.Detail)
	}
	pass, fail, skip := rep.Counts()
	assert.Equal(t, []int{len(checks), 0, 0}, []int{pass, fail, skip})
	assert.Empty(t, rep.Failed())
	assert.Positive(t, rep.Usage().TotalTokens())

	// The timeout check waited for the hang timeout, the kill check for
	// KillAfter; neither ran anywhere near the session timeout.
	to := conformanceResult(t, rep, CheckTimeout)
	assert.GreaterOrEqual(t, to.Duration, opts.HangTimeout)
	assert.Less(t, to.Duration, opts.Timeout)
	kill := conformanceResult(t, rep, CheckKill)
	assert.GreaterOrEqual(t, kill.Duration, opts.KillAfter)
	assert.Less(t, kill.Duration, opts.Timeout)
	assert.Contains(t, conformanceResult(t, rep, CheckRefuse).Detail, "1 refused tool call")

	// Every session check streamed events to the observer.
	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{CheckEcho, CheckStructured, CheckWrite, CheckRefuse, CheckTimeout, CheckKill, CheckBeads} {
		assert.Positive(t, seen[name], "observer saw no events for %s", name)
	}
	assert.Zero(t, seen[CheckDetect])

	// The adapter saw one spec per session check, each with the expected
	// policy, an absolute workdir of its own and the conformance labels.
	calls := a.Calls()
	require.Len(t, calls, 7)
	policies := map[string]cartridge.ToolPolicy{}
	dirs := map[string]bool{}
	for _, c := range calls {
		check := c.Spec.Labels.Get("check")
		policies[check] = c.Spec.ToolPolicy
		assert.True(t, filepath.IsAbs(c.Spec.Workdir))
		assert.Equal(t, check, filepath.Base(c.Spec.Workdir))
		assert.Equal(t, "conformance", c.Spec.Labels.Get(cartridge.LabelTask))
		assert.True(t, c.Matched)
		dirs[c.Spec.Workdir] = true
	}
	assert.Len(t, dirs, 7)
	assert.Equal(t, map[string]cartridge.ToolPolicy{
		CheckEcho: cartridge.ToolReadOnly, CheckStructured: cartridge.ToolReadOnly,
		CheckWrite: cartridge.ToolReadWrite, CheckRefuse: cartridge.ToolReadOnly,
		CheckTimeout: cartridge.ToolReadExec, CheckKill: cartridge.ToolReadExec,
		CheckBeads: cartridge.ToolReadExec,
	}, policies)
	assert.Equal(t, 1, a.Detects())

	table := rep.Table()
	assert.Contains(t, table, "cartridge conformance: mock")
	assert.Contains(t, table, "CHECK")
	assert.Contains(t, table, "8 passed, 0 failed, 0 skipped")
	for _, name := range CheckNames() {
		assert.Contains(t, table, name)
	}
	assert.NotContains(t, table, "FAIL")
}

func TestRun_TemporaryWorkdirIsRemoved(t *testing.T) {
	t.Parallel()
	const token = "sc-tmpdir"
	a, err := NewMock("", token)
	require.NoError(t, err)
	rep, err := Run(context.Background(), a, conformanceFastOptions(t, token))
	require.NoError(t, err)
	require.True(t, rep.Passed(), rep.Table())
	call, ok := a.LastCall()
	require.True(t, ok)
	base := filepath.Dir(call.Spec.Workdir)
	_, statErr := os.Stat(base)
	assert.True(t, errors.Is(statErr, os.ErrNotExist), "temporary workdir %s still exists", base)
}

func TestRun_KeepsGivenWorkdir(t *testing.T) {
	t.Parallel()
	const token = "sc-keepdir"
	a, err := NewMock("", token)
	require.NoError(t, err)
	base := filepath.Join(t.TempDir(), "keep")
	opts := conformanceFastOptions(t, token)
	opts.Workdir = base
	rep, err := Run(context.Background(), a, opts)
	require.NoError(t, err)
	require.True(t, rep.Passed(), rep.Table())
	data, err := os.ReadFile(filepath.Join(base, CheckWrite, WriteFileName))
	require.NoError(t, err)
	assert.Equal(t, token+"\n", string(data))
	entries, err := os.ReadDir(filepath.Join(base, CheckRefuse))
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestRun_ConfiguredMockFromScenarioFile(t *testing.T) {
	t.Parallel()
	token := NewToken()
	path := filepath.Join(t.TempDir(), "scenario.json")
	require.NoError(t, MockScenario(token).WriteFile(path))
	c, err := cartridge.New(mock.Name, config.Cartridge{Env: map[string]string{mock.ScenarioEnv: path}})
	require.NoError(t, err)
	rep, err := Run(context.Background(), c, conformanceFastOptions(t, token))
	require.NoError(t, err)
	assert.True(t, rep.Passed(), rep.Table())
}

func TestRun_DetectFailureSkipsTheRest(t *testing.T) {
	t.Parallel()
	const token = "sc-nodetect"
	s := MockScenario(token)
	s.Detect.Failure = &mock.Failure{Kind: mock.FailNotInstalled, Message: "binary missing"}
	a, err := mock.NewAdapter("", s)
	require.NoError(t, err)
	rep, err := Run(context.Background(), a, conformanceFastOptions(t, token))
	require.NoError(t, err)
	assert.False(t, rep.Passed())
	st := conformanceStatuses(rep)
	assert.Equal(t, StatusFail, st[CheckDetect])
	for _, name := range CheckNames()[1:] {
		assert.Equal(t, StatusSkip, st[name], name)
		assert.Equal(t, "detect failed", conformanceResult(t, rep, name).Detail)
	}
	det := conformanceResult(t, rep, CheckDetect)
	assert.True(t, errors.Is(det.Err, cartridge.ErrNotInstalled))
	assert.Empty(t, a.Calls(), "no session runs when detect fails")
	pass, fail, skip := rep.Counts()
	assert.Equal(t, []int{0, 1, 7}, []int{pass, fail, skip})
	require.Error(t, rep.Err())
	assert.Contains(t, rep.Err().Error(), "failed 1 of 8 checks")
	assert.Contains(t, rep.Err().Error(), "detect: detect probe: ")
	assert.Contains(t, rep.Table(), "0 passed, 1 failed, 7 skipped")
	assert.Contains(t, rep.Table(), "FAIL: detect probe: cartridge mock: not installed: binary missing")
}

func TestRun_NotReadyStatusFailsDetect(t *testing.T) {
	t.Parallel()
	const token = "sc-noauth"
	s := MockScenario(token)
	s.Detect.Auth = cartridge.AuthMissing
	a, err := mock.NewAdapter("", s)
	require.NoError(t, err)
	rep, err := Run(context.Background(), a, conformanceFastOptions(t, token))
	require.NoError(t, err)
	det := conformanceResult(t, rep, CheckDetect)
	assert.Equal(t, StatusFail, det.Status)
	assert.True(t, errors.Is(det.Err, cartridge.ErrAuth), det.Err)
	assert.Contains(t, det.Detail, "auth=missing")
}

// TestRun_ReportsMisbehaviour scripts a cartridge that gets most checks
// wrong and asserts each failure is reported with the reason.
func TestRun_ReportsMisbehaviour(t *testing.T) {
	t.Parallel()
	const token = "sc-misbehave"
	s := mock.NewScenario("misbehaving")
	s.Rule("echo", mock.Match{PromptContains: echoMarker}).Say("I would rather not.")
	s.Rule("structured", mock.Match{PromptContains: structuredMarker}).Say("no block here")
	s.Rule("write", mock.Match{PromptContains: writeMarker, ToolPolicy: string(cartridge.ToolReadWrite)}).Say("I did not write anything.")
	// The read_only variant runs a shell command that the mock refuses
	// (policy honoured) but leaves nothing behind either: refuse passes.
	s.Rule("refuse", mock.Match{PromptContains: writeMarker}).Exec("sh", "-c", "echo x > "+WriteFileName)
	s.Rule("hang", mock.Match{PromptContains: hangMarker}).Say("finished at once")
	s.Rule("beads", mock.Match{PromptContains: beadsMarker}).Say("bd worked fine")
	a, err := mock.NewAdapter("", s)
	require.NoError(t, err)

	rep, err := Run(context.Background(), a, conformanceFastOptions(t, token))
	require.NoError(t, err)
	assert.False(t, rep.Passed())
	st := conformanceStatuses(rep)
	assert.Equal(t, StatusPass, st[CheckDetect])
	assert.Equal(t, StatusFail, st[CheckEcho])
	assert.Equal(t, StatusFail, st[CheckStructured])
	assert.Equal(t, StatusFail, st[CheckWrite])
	assert.Equal(t, StatusPass, st[CheckRefuse])
	assert.Equal(t, StatusFail, st[CheckTimeout])
	assert.Equal(t, StatusFail, st[CheckKill])
	assert.Equal(t, StatusFail, st[CheckBeads])

	assert.Contains(t, conformanceResult(t, rep, CheckEcho).Err.Error(), "does not contain the token")
	assert.Contains(t, conformanceResult(t, rep, CheckStructured).Err.Error(), "structured.Fire")
	assert.Contains(t, conformanceResult(t, rep, CheckStructured).Detail, "2 attempt(s)", "the structured check goes through the retry")
	assert.Contains(t, conformanceResult(t, rep, CheckWrite).Err.Error(), "was not created under read_write")
	assert.Contains(t, conformanceResult(t, rep, CheckTimeout).Err.Error(), "instead of timing out")
	assert.Contains(t, conformanceResult(t, rep, CheckKill).Err.Error(), "instead of being killed")
	assert.Contains(t, conformanceResult(t, rep, CheckBeads).Err.Error(), "never reported the read-only refusal")
	assert.Len(t, rep.Failed(), 6)
	table := rep.Table()
	assert.Contains(t, table, "2 passed, 6 failed, 0 skipped")
	assert.Equal(t, 6, strings.Count(table, "FAIL:"))
}

func TestRun_WrongFailureKindsAreReported(t *testing.T) {
	t.Parallel()
	const token = "sc-wrongkind"
	s := MockScenario(token)
	// Replace the hang rule: the session dies as a crash instead of hanging.
	for _, r := range s.Rules {
		if r.Name == "hang" {
			r.Steps = nil
			r.Fail(mock.FailCrash, "boom")
		}
	}
	a, err := mock.NewAdapter("", s)
	require.NoError(t, err)
	opts := conformanceFastOptions(t, token)
	opts.Checks = []string{CheckDetect, CheckTimeout, CheckKill}
	rep, err := Run(context.Background(), a, opts)
	require.NoError(t, err)
	to := conformanceResult(t, rep, CheckTimeout)
	assert.Equal(t, StatusFail, to.Status)
	assert.Contains(t, to.Err.Error(), "ended with crash, want ErrTimeout")
	assert.True(t, errors.Is(to.Err, cartridge.ErrCrash))
	kill := conformanceResult(t, rep, CheckKill)
	assert.Equal(t, StatusFail, kill.Status)
	assert.Contains(t, kill.Err.Error(), "ended with crash, want ErrKilled")
}

func TestRun_ChecksSubset(t *testing.T) {
	t.Parallel()
	const token = "sc-subset"
	a, err := NewMock("", token)
	require.NoError(t, err)
	opts := conformanceFastOptions(t, token)
	opts.Checks = []string{CheckEcho, " write ", ""}
	rep, err := Run(context.Background(), a, opts)
	require.NoError(t, err)
	assert.True(t, rep.Passed(), rep.Table())
	st := conformanceStatuses(rep)
	assert.Equal(t, StatusPass, st[CheckEcho])
	assert.Equal(t, StatusPass, st[CheckWrite])
	for _, name := range []string{CheckDetect, CheckStructured, CheckRefuse, CheckTimeout, CheckKill, CheckBeads} {
		assert.Equal(t, StatusSkip, st[name], name)
		assert.Equal(t, "not selected", conformanceResult(t, rep, name).Detail)
	}
	assert.Zero(t, a.Detects(), "detect not selected, not run")
	assert.Len(t, a.Calls(), 2)
	pass, fail, skip := rep.Counts()
	assert.Equal(t, []int{2, 0, 6}, []int{pass, fail, skip})

	opts.Checks = []string{"nope"}
	_, err = Run(context.Background(), a, opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown check "nope"`)
	assert.Contains(t, err.Error(), strings.Join(CheckNames(), ", "))
}

func TestRun_RejectsBadInput(t *testing.T) {
	t.Parallel()
	_, err := Run(context.Background(), nil, Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil cartridge")

	a, err := NewMock("", "x")
	require.NoError(t, err)
	_, err = Run(context.Background(), a, Options{HangTimeout: -1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "negative")

	// A workdir that cannot be created.
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))
	_, err = Run(context.Background(), a, Options{Workdir: filepath.Join(file, "sub")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create workdir")
}

func TestRun_DefaultsAndRandomToken(t *testing.T) {
	t.Parallel()
	// A scenario that does not know the token cannot pass echo, but the
	// run still uses a fresh random token and the default timeouts.
	a, err := NewMock("", "unknown")
	require.NoError(t, err)
	opts := Options{Checks: []string{CheckDetect, CheckEcho}}
	rep, err := Run(context.Background(), a, opts)
	require.NoError(t, err)
	assert.Regexp(t, `^sc-[0-9a-f]{12}$`, rep.Token)
	assert.Equal(t, StatusFail, conformanceStatuses(rep)[CheckEcho])
	call, ok := a.LastCall()
	require.True(t, ok)
	assert.Equal(t, DefaultTimeout, call.Spec.Timeout)
	assert.Contains(t, call.Spec.Prompt, rep.Token)
}

func TestRun_ModelAndEnvReachTheSpec(t *testing.T) {
	t.Parallel()
	const token = "sc-modelenv"
	a, err := NewMock("", token)
	require.NoError(t, err)
	opts := conformanceFastOptions(t, token)
	opts.Model = "tiny"
	opts.Env = map[string]string{"GIT_AUTHOR_NAME": "sc"}
	opts.Checks = []string{CheckEcho}
	rep, err := Run(context.Background(), a, opts)
	require.NoError(t, err)
	require.True(t, rep.Passed(), rep.Table())
	call, ok := a.LastCall()
	require.True(t, ok)
	assert.Equal(t, "tiny", call.Spec.Model)
	assert.Equal(t, map[string]string{"GIT_AUTHOR_NAME": "sc"}, call.Spec.Env)
	assert.Equal(t, 1, call.Spec.MaxTurns)
}

func TestRun_CancelledContextFailsSessionChecks(t *testing.T) {
	t.Parallel()
	const token = "sc-cancel"
	a, err := NewMock("", token)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	opts := conformanceFastOptions(t, token)
	done := make(chan struct{})
	var rep Report
	go func() {
		defer close(done)
		rep, err = Run(ctx, a, opts)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return with a cancelled context")
	}
	require.NoError(t, err)
	assert.False(t, rep.Passed())
	for _, name := range []string{CheckEcho, CheckStructured, CheckWrite, CheckRefuse, CheckTimeout} {
		r := conformanceResult(t, rep, name)
		assert.Equal(t, StatusFail, r.Status, name)
		assert.True(t, errors.Is(r.Err, cartridge.ErrKilled), "%s: %v", name, r.Err)
	}
	// Kill is what the kill check wants, whoever did it.
	assert.Equal(t, StatusPass, conformanceResult(t, rep, CheckKill).Status)
}

func TestStreamContract(t *testing.T) {
	t.Parallel()
	done := cartridge.Done(cartridge.Result{})
	errEv := cartridge.ErrorEvent(cartridge.ErrCrash, cartridge.Usage{})
	text := cartridge.TextDelta("x")
	crash := cartridge.NewError(cartridge.KindCrash, "m", "", nil)
	cases := []struct {
		name string
		o    outcome
		want string
	}{
		{"ok done", outcome{events: []cartridge.SessionEvent{text, done}}, ""},
		{"ok error", outcome{events: []cartridge.SessionEvent{errEv}, err: crash}, ""},
		{"no events", outcome{}, "no events"},
		{"two terminals", outcome{events: []cartridge.SessionEvent{done, done}}, "2 terminal events"},
		{"no terminal", outcome{events: []cartridge.SessionEvent{text}}, "0 terminal events"},
		{"terminal not last", outcome{events: []cartridge.SessionEvent{done, text}}, "last event is text_delta"},
		{"done but err", outcome{events: []cartridge.SessionEvent{done}, err: crash}, "ended with done but Wait returned"},
		{"error but nil", outcome{events: []cartridge.SessionEvent{errEv}}, "ended with error but Wait returned nil"},
		{"error without err", outcome{events: []cartridge.SessionEvent{{Kind: cartridge.EventError}}, err: crash}, "carries no error"},
		{"plain error", outcome{events: []cartridge.SessionEvent{errEv}, err: errors.New("plain")}, "want *cartridge.Error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := streamContract(tc.o)
			if tc.want == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestReport_Table(t *testing.T) {
	t.Parallel()
	rep := Report{
		Cartridge: "x",
		Status:    cartridge.Status{Installed: true, Version: "1.2", Auth: cartridge.AuthOK, Path: "/usr/bin/x", Detail: "logged in"},
		Token:     "sc-abc",
		Results: []Result{
			{Check: CheckDetect, Status: StatusPass, Detail: "fine", Duration: 1500 * time.Microsecond},
			{Check: CheckEcho, Status: StatusFail, Err: errors.New("bad\nnews"), Detail: "2 events", Duration: 1234 * time.Millisecond,
				Usage: cartridge.Usage{InputTokens: 10, OutputTokens: 5, CostUSD: 0.01, HasCost: true}},
			{Check: CheckKill, Status: StatusSkip, Detail: "not selected"},
		},
		Duration: 61 * time.Second,
	}
	table := rep.Table()
	lines := strings.Split(strings.TrimRight(table, "\n"), "\n")
	assert.Equal(t, "cartridge conformance: x", lines[0])
	assert.Equal(t, `detect: installed=true version="1.2" auth=ok path=/usr/bin/x (logged in)`, lines[1])
	assert.Equal(t, "token: sc-abc", lines[2])
	assert.Contains(t, table, "CHECK")
	assert.Contains(t, table, "CHECK   RESULT  TIME   DETAIL\n")
	assert.Contains(t, table, "detect  PASS    2ms    fine\n")
	assert.Contains(t, table, "echo    FAIL    1.23s  FAIL: bad | news [2 events]\n")
	assert.Contains(t, table, "kill    SKIP    0s     not selected\n")
	assert.Contains(t, table, "1 passed, 1 failed, 1 skipped in 1m1s; 10 in / 5 out tokens, $0.0100")
	assert.Equal(t, table, func() string { var b strings.Builder; rep.WriteTable(&b); return b.String() }())

	empty := Report{Cartridge: "y"}
	assert.Equal(t, "cartridge conformance: y\n\nCHECK  RESULT  TIME  DETAIL\n\n0 passed, 0 failed, 0 skipped in 0s\n", empty.Table())
	assert.NoError(t, empty.Err())
	assert.True(t, empty.Passed())
}

func TestPrompts(t *testing.T) {
	t.Parallel()
	const token = "sc-prompt"
	assert.True(t, strings.HasPrefix(EchoPrompt(token), echoMarker))
	assert.Contains(t, EchoPrompt(token), token)
	assert.True(t, strings.HasPrefix(StructuredPrompt(token), structuredMarker))
	assert.Contains(t, StructuredPrompt(token), token)
	assert.True(t, strings.HasPrefix(WritePrompt(token), writeMarker))
	assert.Contains(t, WritePrompt(token), WriteFileName)
	assert.Contains(t, WritePrompt(token), token)
	assert.Contains(t, HangPrompt(), hangMarker)
	// The markers are mutually exclusive so a scenario rule matches one
	// prompt only.
	prompts := map[string]string{echoMarker: EchoPrompt(token), structuredMarker: StructuredPrompt(token), writeMarker: WritePrompt(token), hangMarker: HangPrompt()}
	for marker := range prompts {
		for other, p := range prompts {
			if other != marker {
				assert.NotContains(t, p, marker)
			}
		}
	}
	assert.Equal(t, ContractName, Contract().Name())
	assert.NotContains(t, Contract().WithPromptSuffix(StructuredPrompt(token)), echoMarker)
}

func TestChecks(t *testing.T) {
	t.Parallel()
	cs := Checks()
	require.Len(t, cs, 8)
	assert.Equal(t, []string{CheckDetect, CheckEcho, CheckStructured, CheckWrite, CheckRefuse, CheckTimeout, CheckKill, CheckBeads}, CheckNames())
	for _, c := range cs {
		assert.NotEmpty(t, c.Description, c.Name)
		assert.NotNil(t, c.run, c.Name)
	}
	cs[0].Name = "changed"
	assert.Equal(t, CheckDetect, Checks()[0].Name, "Checks returns a copy")
	assert.Equal(t, "pass", StatusPass.String())
}

func TestNewToken(t *testing.T) {
	t.Parallel()
	a, b := NewToken(), NewToken()
	assert.Regexp(t, `^sc-[0-9a-f]{12}$`, a)
	assert.NotEqual(t, a, b)
}

func TestMockScenario_RoundTrip(t *testing.T) {
	t.Parallel()
	s := MockScenario("sc-rt")
	require.NoError(t, s.Validate())
	data, err := s.Marshal()
	require.NoError(t, err)
	back, err := mock.Parse(data)
	require.NoError(t, err)
	assert.Equal(t, len(s.Rules), len(back.Rules))
	require.NotNil(t, back.Fallback)
	a, err := NewMock("other", "sc-rt")
	require.NoError(t, err)
	assert.Equal(t, "other", a.Name())
}

// TestDocs_ConformanceSection fails when docs/cartridges.md stops
// describing the suite: every check name, the make target and the
// environment knobs must be mentioned.
func TestDocs_ConformanceSection(t *testing.T) {
	t.Parallel()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "docs", "cartridges.md"))
	require.NoError(t, err)
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	doc := string(data)
	for _, needle := range append(CheckNames(),
		"make cartridge-conformance", "CARTRIDGE=claude", "CARTRIDGE=mock",
		"SC_CONFORMANCE_MODEL", "SC_CONFORMANCE_CHECKS", WriteFileName, "`"+ContractName+"`") {
		assert.True(t, strings.Contains(doc, needle), "%q is not mentioned in docs/cartridges.md", needle)
	}
}
