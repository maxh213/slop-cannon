package agy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/testutil"
)

const agyShimScript = `
if [ "$1" = "--version" ]; then
  if [ -n "$SC_AGY_VERSION" ]; then echo "$SC_AGY_VERSION"; else echo "1.4.7"; fi
  exit 0
fi
if [ -n "$SC_AGY_ENV_DUMP" ]; then env | sort > "$SC_AGY_ENV_DUMP"; fi
for _sc_prompt in "$PWD"/.sc-agy-prompt-*; do
  [ -f "$_sc_prompt" ] || continue
  if [ -n "$SC_AGY_PROMPT_CAPTURE" ]; then cat "$_sc_prompt" > "$SC_AGY_PROMPT_CAPTURE"; fi
  if [ -n "$SC_AGY_PROMPT_MODE" ]; then
    if stat -c '%a' "$_sc_prompt" >/dev/null 2>&1; then
      stat -c '%a' "$_sc_prompt" > "$SC_AGY_PROMPT_MODE"
    else
      stat -f '%Lp' "$_sc_prompt" > "$SC_AGY_PROMPT_MODE"
    fi
  fi
done
if [ -n "$SC_AGY_SLEEP" ]; then sleep "$SC_AGY_SLEEP"; fi
if [ -n "$SC_AGY_STDERR" ]; then echo "$SC_AGY_STDERR" >&2; fi
if [ -n "$SC_AGY_STDERR_FILE" ]; then cat "$SC_AGY_STDERR_FILE" >&2; fi
if [ -n "$SC_AGY_FIXTURE" ]; then cat "$SC_AGY_FIXTURE"; fi
exit "${SC_AGY_EXIT:-0}"
`

func agyShim(t *testing.T) *testutil.Shim {
	t.Helper()
	return testutil.FakeBin(t, Name, agyShimScript)
}

func agyFixture(t testing.TB, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", "agy", name))
	require.NoError(t, err)
	_, err = os.Stat(path)
	require.NoError(t, err)
	return path
}

func agySpec(t *testing.T, env map[string]string) cartridge.RunSpec {
	t.Helper()
	return cartridge.RunSpec{
		SystemPrompt: "You are a test.", Prompt: "Reply with exactly PONG",
		Workdir: t.TempDir(), Model: "routed-model",
		ToolPolicy: cartridge.ToolReadWrite, MaxTurns: 3,
		Timeout: 20 * time.Second, Env: env,
	}
}

func collect(t *testing.T, a *Adapter, spec cartridge.RunSpec) ([]cartridge.SessionEvent, cartridge.Result, error) {
	t.Helper()
	s, err := a.Run(context.Background(), spec)
	require.NoError(t, err)
	var events []cartridge.SessionEvent
	res, err := cartridge.Collect(s, func(ev cartridge.SessionEvent) { events = append(events, ev) })
	return events, res, err
}

func TestDetectVersionAndAuthUnknown(t *testing.T) {
	shim := agyShim(t)
	a := newAdapter(t, config.Cartridge{Args: []string{"--effort", "high"}})
	st, err := a.Detect(context.Background())
	require.NoError(t, err)
	assert.True(t, st.Installed)
	assert.Equal(t, shim.Path, st.Path)
	assert.Equal(t, VerifiedVersion, st.Version)
	assert.Equal(t, cartridge.AuthUnknown, st.Auth)
	assert.Contains(t, st.Detail, "no cheap local auth")
	assert.True(t, st.Ready())
	assert.Equal(t, [][]string{{"--version"}}, shim.ArgLists(t), "no auth provider call and no configured firing args")
}

func TestDetectTooOld(t *testing.T) {
	agyShim(t)
	a := newAdapter(t, config.Cartridge{Env: map[string]string{"SC_AGY_VERSION": "1.4.6"}})
	st, err := a.Detect(context.Background())
	require.ErrorIs(t, err, cartridge.ErrNotInstalled)
	assert.True(t, st.Installed)
	assert.Equal(t, "1.4.6", st.Version)
}

func TestDetectMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	a := newAdapter(t, config.Cartridge{})
	st, err := a.Detect(context.Background())
	require.ErrorIs(t, err, cartridge.ErrNotInstalled)
	assert.False(t, st.Installed)
	assert.Equal(t, "agy not found on PATH", st.Detail)
}

func TestRunFinalJSONEnvelope(t *testing.T) {
	shim := agyShim(t)
	a := newAdapter(t, config.Cartridge{Args: []string{"--effort", "xhigh"}})
	spec := agySpec(t, map[string]string{"SC_AGY_FIXTURE": agyFixture(t, "success.json")})
	events, res, err := collect(t, a, spec)
	require.NoError(t, err)
	assert.Equal(t, "PONG", res.Output)
	assert.Equal(t, "agy-session-123", res.SessionID)
	assert.Equal(t, 0, res.ExitCode)
	assert.Greater(t, res.Duration, time.Duration(0))
	assert.Equal(t, cartridge.Usage{
		InputTokens: 120, OutputTokens: 45, CacheReadTokens: 80, Turns: 3,
	}, res.Usage)
	assert.False(t, res.Usage.HasCost, "agy's final turn envelope does not report cost")
	require.Len(t, events, 2)
	assert.Equal(t, cartridge.EventTextDelta, events[0].Kind)
	assert.Equal(t, "PONG", events[0].Text)
	assert.Equal(t, cartridge.EventDone, events[1].Kind)

	calls := shim.Invocations(t)
	require.Len(t, calls, 1)
	assert.Equal(t, testutil.RealPath(t, spec.Workdir), calls[0].Dir)
	assert.Empty(t, calls[0].Stdin)
	assert.Equal(t, []string{flagPrint, Prompt(spec)}, calls[0].Args[len(calls[0].Args)-2:])
	assert.Contains(t, calls[0].Args, "--add-dir")
	assert.Contains(t, calls[0].Args, spec.Workdir)
	assert.Contains(t, calls[0].Args, "--effort")
	assert.Contains(t, calls[0].Args, "xhigh")
}

func TestRunConversationID(t *testing.T) {
	agyShim(t)
	a := newAdapter(t, config.Cartridge{})
	_, res, err := collect(t, a, agySpec(t, map[string]string{"SC_AGY_FIXTURE": agyFixture(t, "conversation.json")}))
	require.NoError(t, err)
	assert.Equal(t, "conversation-456", res.SessionID)
	assert.Equal(t, "OK", res.Output)
	assert.Equal(t, 1, res.Usage.Turns)
	assert.False(t, res.Usage.HasCost)
}

func TestRunReadOnlyCandidateArgv(t *testing.T) {
	shim := agyShim(t)
	a := newAdapter(t, config.Cartridge{})
	spec := agySpec(t, map[string]string{"SC_AGY_FIXTURE": agyFixture(t, "success.json")})
	spec.ToolPolicy = cartridge.ToolReadOnly
	_, _, err := collect(t, a, spec)
	require.NoError(t, err)
	args := shim.Invocations(t)[0].Args
	assert.Contains(t, args, "plan")
	assert.NotContains(t, args, flagSkipPermissions)
	assert.NotContains(t, args, flagDisableSlashCommands)
	assert.Equal(t, []string{flagPrint, Prompt(spec)}, args[len(args)-2:])
}

func TestRunRejectsPlanSlashConflictBeforeStart(t *testing.T) {
	shim := agyShim(t)
	a := newAdapter(t, config.Cartridge{Args: []string{flagDisableSlashCommands}})
	spec := agySpec(t, nil)
	spec.ToolPolicy = cartridge.ToolReadOnly
	_, err := a.Run(context.Background(), spec)
	require.ErrorIs(t, err, ErrPlanSlashConflict)
	assert.Empty(t, shim.Invocations(t))
}

func TestRunRejectsReadExecBeforeStart(t *testing.T) {
	shim := agyShim(t)
	a := newAdapter(t, config.Cartridge{})
	spec := agySpec(t, nil)
	spec.ToolPolicy = cartridge.ToolReadExec
	_, err := a.Run(context.Background(), spec)
	require.ErrorIs(t, err, ErrReadExecUnsupported)
	assert.Empty(t, shim.Invocations(t))
}

func TestRunMalformedMissingAndEmptyResults(t *testing.T) {
	for _, tc := range []struct {
		name, fixture, want string
	}{
		{"malformed", "malformed.json", "malformed final JSON result"},
		{"empty success response", "missing-result.json", "SUCCESS with an empty response"},
		{"empty output", "", "missing final JSON result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agyShim(t)
			a := newAdapter(t, config.Cartridge{})
			env := map[string]string{}
			if tc.fixture != "" {
				env["SC_AGY_FIXTURE"] = agyFixture(t, tc.fixture)
			}
			_, _, err := collect(t, a, agySpec(t, env))
			require.ErrorIs(t, err, cartridge.ErrCrash)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestRunTypedProcessErrors(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		want    error
	}{
		{"auth.stderr", cartridge.ErrAuth},
		{"credits.stderr", cartridge.ErrCreditsExhausted},
		{"rate.stderr", cartridge.ErrRateLimited},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			agyShim(t)
			a := newAdapter(t, config.Cartridge{})
			_, res, err := collect(t, a, agySpec(t, map[string]string{
				"SC_AGY_STDERR_FILE": agyFixture(t, tc.fixture), "SC_AGY_EXIT": "1",
			}))
			require.True(t, errors.Is(err, tc.want), "%v", err)
			assert.Equal(t, 1, res.ExitCode)
		})
	}
}

func TestRunTypedErrorEnvelope(t *testing.T) {
	agyShim(t)
	a := newAdapter(t, config.Cartridge{})
	events, res, err := collect(t, a, agySpec(t, map[string]string{"SC_AGY_FIXTURE": agyFixture(t, "rate-error.json")}))
	require.ErrorIs(t, err, cartridge.ErrRateLimited)
	assert.Equal(t, "rate-session", res.SessionID)
	assert.Equal(t, 9, res.Usage.InputTokens)
	require.Len(t, events, 2)
	assert.Equal(t, cartridge.EventError, events[1].Kind)
}

func TestRunStructuredAgentError(t *testing.T) {
	agyShim(t)
	a := newAdapter(t, config.Cartridge{})
	_, res, err := collect(t, a, agySpec(t, map[string]string{
		"SC_AGY_STDERR": `AGY_ERROR: {"short_error":"RESOURCE_EXHAUSTED (code 429): quota resets soon","status":"RESOURCE_EXHAUSTED","error_code":429,"code_kind":"http","retryable":true,"error_id":"test-error"}`,
		"SC_AGY_EXIT":   "3",
	}))
	require.ErrorIs(t, err, cartridge.ErrRateLimited)
	assert.Equal(t, 3, res.ExitCode)
	assert.Contains(t, err.Error(), "quota resets soon")
}

func TestRunPrintTimeoutWarningIsTimeoutEvenOnSuccess(t *testing.T) {
	agyShim(t)
	a := newAdapter(t, config.Cartridge{})
	_, _, err := collect(t, a, agySpec(t, map[string]string{
		"SC_AGY_FIXTURE": agyFixture(t, "success.json"),
		"SC_AGY_STDERR":  "[agy] print timeout after 20s with turn in progress; returning partial output",
	}))
	require.ErrorIs(t, err, cartridge.ErrTimeout)
}

func TestRunTimeout(t *testing.T) {
	shim := agyShim(t)
	a := newAdapter(t, config.Cartridge{})
	spec := agySpec(t, map[string]string{"SC_AGY_SLEEP": "5"})
	spec.Timeout = 250 * time.Millisecond
	start := time.Now()
	_, res, err := collect(t, a, spec)
	require.ErrorIs(t, err, cartridge.ErrTimeout)
	assert.Equal(t, -1, res.ExitCode)
	assert.Less(t, time.Since(start), 3*time.Second)
	args := shim.Invocations(t)[0].Args
	assert.Contains(t, args, "250ms")
}

func TestRunKillAndContextCancellation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel bool
	}{
		{"kill", false}, {"context cancellation", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agyShim(t)
			a := newAdapter(t, config.Cartridge{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s, err := a.Run(ctx, agySpec(t, map[string]string{"SC_AGY_SLEEP": "5"}))
			require.NoError(t, err)
			if tc.cancel {
				time.AfterFunc(100*time.Millisecond, cancel)
			} else {
				time.AfterFunc(100*time.Millisecond, s.Kill)
			}
			_, err = cartridge.Collect(s, nil)
			require.ErrorIs(t, err, cartridge.ErrKilled)
		})
	}
}

func TestRunEnvironmentIsCleanAndLayered(t *testing.T) {
	agyShim(t)
	t.Setenv("AGY_ACCOUNT", "work")
	t.Setenv("GEMINI_API_KEY", "gemini-test")
	t.Setenv("SC_SECRET_THING", "do-not-leak")
	dump := filepath.Join(t.TempDir(), "env")
	a := newAdapter(t, config.Cartridge{Env: map[string]string{"FROM_CFG": "cfg", "SHARED": "cfg"}})
	spec := agySpec(t, map[string]string{
		"SC_AGY_ENV_DUMP": dump, "SC_AGY_FIXTURE": agyFixture(t, "success.json"),
		"FROM_SPEC": "spec", "SHARED": "spec",
	})
	_, _, err := collect(t, a, spec)
	require.NoError(t, err)
	data, err := os.ReadFile(dump)
	require.NoError(t, err)
	env := string(data)
	for _, want := range []string{"AGY_ACCOUNT=work\n", "GEMINI_API_KEY=gemini-test\n", "FROM_CFG=cfg\n", "FROM_SPEC=spec\n", "SHARED=spec\n", "PATH="} {
		assert.Contains(t, env, want)
	}
	assert.NotContains(t, env, "SC_SECRET_THING=")
}

func TestRunWaitWithoutDraining(t *testing.T) {
	agyShim(t)
	a := newAdapter(t, config.Cartridge{})
	s, err := a.Run(context.Background(), agySpec(t, map[string]string{"SC_AGY_FIXTURE": agyFixture(t, "success.json")}))
	require.NoError(t, err)
	res, err := s.Wait()
	require.NoError(t, err)
	assert.Equal(t, "PONG", res.Output)
	var kinds []cartridge.EventKind
	for ev := range s.Events() {
		kinds = append(kinds, ev.Kind)
	}
	assert.Equal(t, []cartridge.EventKind{cartridge.EventTextDelta, cartridge.EventDone}, kinds)
}

func TestRunInvalidSpecDoesNotStart(t *testing.T) {
	shim := agyShim(t)
	a := newAdapter(t, config.Cartridge{})
	spec := agySpec(t, nil)
	spec.Prompt = " "
	_, err := a.Run(context.Background(), spec)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "empty prompt"))
	assert.Empty(t, shim.Invocations(t))
}
