package claude

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

// claudeShimScript is the fake "claude" used by the adapter tests. Probes
// answer from files; a firing replays a recorded stream-json fixture. Every
// knob arrives through the (allowlisted) environment because that is the
// only channel the adapter gives a child.
const claudeShimScript = `
case "$1" in
  --version)
    if [ -n "$SC_CLAUDE_VERSION" ]; then echo "$SC_CLAUDE_VERSION"; else echo "2.1.258 (Claude Code)"; fi
    exit 0;;
  auth)
    if [ -n "$SC_CLAUDE_AUTH_FILE" ]; then cat "$SC_CLAUDE_AUTH_FILE"; fi
    exit "${SC_CLAUDE_AUTH_EXIT:-0}";;
esac
if [ -n "$SC_CLAUDE_ENV_DUMP" ]; then env | sort > "$SC_CLAUDE_ENV_DUMP"; fi
if [ -n "$SC_CLAUDE_SLEEP" ]; then sleep "$SC_CLAUDE_SLEEP"; fi
if [ -n "$SC_CLAUDE_STDERR" ]; then echo "$SC_CLAUDE_STDERR" >&2; fi
if [ -n "$SC_CLAUDE_FIXTURE" ]; then cat "$SC_CLAUDE_FIXTURE"; fi
exit "${SC_CLAUDE_EXIT:-0}"
`

// claudeShim installs the fake claude on PATH (captures stdin) and returns
// it. It uses t.Setenv, so callers must not be parallel.
func claudeShim(t *testing.T) *testutil.Shim {
	t.Helper()
	return testutil.FakeBin(t, "claude", claudeShimScript, testutil.WithStdinCapture())
}

func claudeSpec(t *testing.T, env map[string]string) cartridge.RunSpec {
	t.Helper()
	return cartridge.RunSpec{
		Prompt:       "Reply with exactly the word PONG",
		SystemPrompt: "You are a test.",
		Workdir:      t.TempDir(),
		Model:        "sonnet",
		ToolPolicy:   cartridge.ToolReadOnly,
		MaxTurns:     3,
		Timeout:      20 * time.Second,
		Env:          env,
	}
}

func TestDetect_LoggedIn(t *testing.T) {
	shim := claudeShim(t)
	a := claudeNewAdapter(t, config.Cartridge{Env: map[string]string{"SC_CLAUDE_AUTH_FILE": claudeFixture(t, "auth-status.json")}})

	st, err := a.Detect(context.Background())
	require.NoError(t, err)
	assert.True(t, st.Installed)
	assert.Equal(t, shim.Path, st.Path)
	assert.Equal(t, "2.1.258", st.Version)
	assert.Equal(t, cartridge.AuthOK, st.Auth)
	assert.Equal(t, "logged in as user@example.com, max", st.Detail)
	assert.True(t, st.Ready())

	calls := shim.ArgLists(t)
	require.Len(t, calls, 2)
	assert.Equal(t, []string{"--version"}, calls[0])
	assert.Equal(t, []string{"auth", "status", "--json"}, calls[1])
}

func TestDetect_ConfiguredArgsNotUsedForProbes(t *testing.T) {
	shim := claudeShim(t)
	a := claudeNewAdapter(t, config.Cartridge{Args: []string{"--effort", "low"}, Env: map[string]string{"SC_CLAUDE_AUTH_FILE": claudeFixture(t, "auth-status.json")}})
	_, err := a.Detect(context.Background())
	require.NoError(t, err)
	for _, call := range shim.ArgLists(t) {
		assert.NotContains(t, call, "--effort")
	}
}

func TestDetect_NotLoggedIn(t *testing.T) {
	claudeShim(t)
	authFile := filepath.Join(t.TempDir(), "auth.json")
	require.NoError(t, os.WriteFile(authFile, []byte(`{"loggedIn":false,"authMethod":"none","apiProvider":"firstParty"}`), 0o644))
	a := claudeNewAdapter(t, config.Cartridge{Env: map[string]string{"SC_CLAUDE_AUTH_FILE": authFile}})

	st, err := a.Detect(context.Background())
	require.Error(t, err)
	assert.True(t, errors.Is(err, cartridge.ErrAuth))
	assert.True(t, st.Installed)
	assert.Equal(t, "2.1.258", st.Version)
	assert.Equal(t, cartridge.AuthMissing, st.Auth)
	assert.Contains(t, st.Detail, "not logged in")
	assert.False(t, st.Ready())
}

func TestDetect_AuthProbeUnavailable(t *testing.T) {
	claudeShim(t)
	a := claudeNewAdapter(t, config.Cartridge{Env: map[string]string{"SC_CLAUDE_AUTH_EXIT": "1"}})

	st, err := a.Detect(context.Background())
	require.NoError(t, err, "an old CLI without auth status is not a failure")
	assert.Equal(t, cartridge.AuthUnknown, st.Auth)
	assert.Contains(t, st.Detail, "auth probe unavailable")
	assert.True(t, st.Ready())

	// Auth status that prints something that is not JSON is unknown too.
	junk := filepath.Join(t.TempDir(), "auth.txt")
	require.NoError(t, os.WriteFile(junk, []byte("Logged in as someone\n"), 0o644))
	b := claudeNewAdapter(t, config.Cartridge{Env: map[string]string{"SC_CLAUDE_AUTH_FILE": junk}})
	st, err = b.Detect(context.Background())
	require.NoError(t, err)
	assert.Equal(t, cartridge.AuthUnknown, st.Auth)
	assert.Equal(t, "auth probe printed no JSON", st.Detail)
}

func TestDetect_AuthProbeSkipsPreamble(t *testing.T) {
	claudeShim(t)
	authFile := filepath.Join(t.TempDir(), "auth.json")
	require.NoError(t, os.WriteFile(authFile, []byte("note: something\n{\"loggedIn\":true,\"authMethod\":\"apiKey\",\"apiProvider\":\"bedrock\"}\ntrailing\n"), 0o644))
	a := claudeNewAdapter(t, config.Cartridge{Env: map[string]string{"SC_CLAUDE_AUTH_FILE": authFile}})
	st, err := a.Detect(context.Background())
	require.NoError(t, err)
	assert.Equal(t, cartridge.AuthOK, st.Auth)
	assert.Equal(t, "logged in via apiKey, bedrock", st.Detail)
}

func TestDetect_TooOld(t *testing.T) {
	claudeShim(t)
	a := claudeNewAdapter(t, config.Cartridge{Env: map[string]string{"SC_CLAUDE_VERSION": "1.0.83 (Claude Code)"}})

	st, err := a.Detect(context.Background())
	require.Error(t, err)
	assert.True(t, errors.Is(err, cartridge.ErrNotInstalled))
	assert.True(t, st.Installed)
	assert.Equal(t, "1.0.83", st.Version)
	assert.Contains(t, st.Detail, "older than the minimum 2.0.0")
	assert.Contains(t, st.Detail, VerifiedVersion)
}

func TestDetect_NotInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	a := claudeNewAdapter(t, config.Cartridge{})

	st, err := a.Detect(context.Background())
	require.Error(t, err)
	assert.True(t, errors.Is(err, cartridge.ErrNotInstalled))
	assert.False(t, st.Installed)
	assert.Equal(t, "claude not found on PATH", st.Detail)

	// A configured command is what gets looked up.
	b := claudeNewAdapter(t, config.Cartridge{Command: "claude-nightly"})
	st, err = b.Detect(context.Background())
	require.Error(t, err)
	assert.Equal(t, "claude-nightly not found on PATH", st.Detail)
}

func TestDetect_VersionProbeCrash(t *testing.T) {
	testutil.FakeBin(t, "claude", `echo "boom" >&2; exit 3`)
	a := claudeNewAdapter(t, config.Cartridge{})
	st, err := a.Detect(context.Background())
	require.Error(t, err)
	assert.True(t, errors.Is(err, cartridge.ErrCrash))
	assert.True(t, st.Installed)
	assert.Equal(t, "claude --version failed", st.Detail)
}

func TestDetect_ContextCancelled(t *testing.T) {
	testutil.FakeBin(t, "claude", `sleep 10`)
	a := claudeNewAdapter(t, config.Cartridge{})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := a.Detect(ctx)
	require.Error(t, err)
	k := cartridge.KindOf(err)
	assert.Contains(t, []cartridge.Kind{cartridge.KindTimeout, cartridge.KindKilled}, k)
}

func TestRun_TextReply(t *testing.T) {
	shim := claudeShim(t)
	a := claudeNewAdapter(t, config.Cartridge{Model: "cfg-model", Args: []string{"--effort", "low"}})
	spec := claudeSpec(t, map[string]string{"SC_CLAUDE_FIXTURE": claudeFixture(t, "text-reply.jsonl")})

	s, err := a.Run(context.Background(), spec)
	require.NoError(t, err)
	var evs []cartridge.SessionEvent
	res, err := cartridge.Collect(s, func(ev cartridge.SessionEvent) { evs = append(evs, ev) })
	require.NoError(t, err)

	assert.Equal(t, "PONG", res.Output)
	assert.Equal(t, 0, res.ExitCode)
	assert.Equal(t, "78cf1c28-8e85-41e1-8948-53e0cacf1ce6", res.SessionID)
	assert.Greater(t, res.Duration, time.Duration(0))
	assert.Equal(t, 10, res.Usage.InputTokens)
	assert.Equal(t, 50, res.Usage.OutputTokens)
	assert.True(t, res.Usage.HasCost)

	require.Equal(t, []cartridge.EventKind{cartridge.EventTextDelta, cartridge.EventDone}, claudeKinds(evs))
	assert.Equal(t, "PONG", evs[0].Text)
	require.NotNil(t, evs[1].Result)
	assert.Equal(t, res, *evs[1].Result)

	// Wait is idempotent after Collect.
	res2, err := s.Wait()
	require.NoError(t, err)
	assert.Equal(t, res, res2)

	calls := shim.Invocations(t)
	require.Len(t, calls, 1)
	assert.Equal(t, spec.Workdir, calls[0].Dir, "cwd is the workdir")
	assert.Equal(t, spec.Prompt, calls[0].Stdin, "the prompt arrives on stdin")
	assert.Equal(t, []string{
		"-p", "--output-format", "stream-json", "--verbose",
		"--model", "sonnet",
		"--append-system-prompt", "You are a test.",
		"--max-turns", "3",
		"--permission-mode", "dontAsk",
		"--allowedTools", strings.Join(ReadOnlyShell, ","),
		"--disallowedTools", "Edit,Write,MultiEdit,NotebookEdit",
		"--effort", "low",
	}, calls[0].Args)
}

func TestRun_CleanEnvironment(t *testing.T) {
	claudeShim(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "parent")
	t.Setenv("SC_LEAK_CHECK", "leak")
	dump := filepath.Join(t.TempDir(), "env.txt")
	a := claudeNewAdapter(t, config.Cartridge{Env: map[string]string{"FROM_CFG": "1"}})
	spec := claudeSpec(t, map[string]string{
		"SC_CLAUDE_FIXTURE":  claudeFixture(t, "text-reply.jsonl"),
		"SC_CLAUDE_ENV_DUMP": dump,
	})

	s, err := a.Run(context.Background(), spec)
	require.NoError(t, err)
	_, err = cartridge.Collect(s, nil)
	require.NoError(t, err)

	data, err := os.ReadFile(dump)
	require.NoError(t, err)
	env := string(data)
	assert.Contains(t, env, "ANTHROPIC_API_KEY=sk-test\n")
	assert.Contains(t, env, "DISABLE_AUTOUPDATER=1\n")
	assert.Contains(t, env, "FROM_CFG=1\n")
	assert.Contains(t, env, "PATH=")
	assert.NotContains(t, env, "CLAUDECODE=")
	assert.NotContains(t, env, "CLAUDE_CODE_SESSION_ID=")
	assert.NotContains(t, env, "SC_LEAK_CHECK=")
}

func TestRun_ToolUseEvents(t *testing.T) {
	claudeShim(t)
	a := claudeNewAdapter(t, config.Cartridge{})
	spec := claudeSpec(t, map[string]string{"SC_CLAUDE_FIXTURE": claudeFixture(t, "shell-denied.jsonl")})
	spec.ToolPolicy = cartridge.ToolReadExec

	s, err := a.Run(context.Background(), spec)
	require.NoError(t, err)
	var evs []cartridge.SessionEvent
	res, err := cartridge.Collect(s, func(ev cartridge.SessionEvent) { evs = append(evs, ev) })
	require.NoError(t, err)
	assert.Equal(t, []cartridge.EventKind{
		cartridge.EventToolUse, cartridge.EventToolUse,
		cartridge.EventToolResult, cartridge.EventToolResult,
		cartridge.EventTextDelta, cartridge.EventDone,
	}, claudeKinds(evs))
	assert.Equal(t, cartridge.Transcript(evs), res.Output)
	for _, ev := range evs {
		assert.False(t, ev.At.IsZero())
	}
}

func TestRun_ErrorResult(t *testing.T) {
	claudeShim(t)
	a := claudeNewAdapter(t, config.Cartridge{})
	spec := claudeSpec(t, map[string]string{
		"SC_CLAUDE_FIXTURE": claudeFixture(t, "api-error-unknown-model.jsonl"),
		"SC_CLAUDE_STDERR":  `[claude-code:unrecognized_model] {"model":"claude-no-such-model-999","query_source":"sdk"}`,
		"SC_CLAUDE_EXIT":    "1",
	})

	s, err := a.Run(context.Background(), spec)
	require.NoError(t, err)
	var evs []cartridge.SessionEvent
	res, err := cartridge.Collect(s, func(ev cartridge.SessionEvent) { evs = append(evs, ev) })
	require.Error(t, err)
	assert.True(t, errors.Is(err, cartridge.ErrCrash))
	var ce *cartridge.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, "claude", ce.Cartridge)
	assert.Contains(t, ce.Msg, "HTTP 404")
	assert.Equal(t, 1, res.ExitCode)
	assert.Contains(t, res.Output, "issue with the selected model")

	require.Equal(t, []cartridge.EventKind{cartridge.EventTextDelta, cartridge.EventError}, claudeKinds(evs))
	assert.Equal(t, err, evs[1].Err)
}

func TestRun_MaxTurnsIsRetryableCrash(t *testing.T) {
	claudeShim(t)
	a := claudeNewAdapter(t, config.Cartridge{})
	spec := claudeSpec(t, map[string]string{
		"SC_CLAUDE_FIXTURE": claudeFixture(t, "error-max-turns.jsonl"),
		"SC_CLAUDE_EXIT":    "1",
	})
	s, err := a.Run(context.Background(), spec)
	require.NoError(t, err)
	res, err := cartridge.Collect(s, nil)
	require.Error(t, err)
	assert.Equal(t, cartridge.KindCrash, cartridge.KindOf(err))
	assert.True(t, cartridge.KindOf(err).Retryable())
	assert.Contains(t, err.Error(), "max turns reached")
	assert.Equal(t, 2, res.Usage.Turns)
	assert.True(t, res.Usage.HasCost)
}

func TestRun_StderrClassification(t *testing.T) {
	claudeShim(t)
	a := claudeNewAdapter(t, config.Cartridge{})
	cases := []struct {
		stderr string
		want   error
	}{
		{"Not logged in · Please run /login", cartridge.ErrAuth},
		{"Your credit balance is too low to access the Anthropic API.", cartridge.ErrCreditsExhausted},
		{"API Error: 429 rate_limit_error", cartridge.ErrRateLimited},
		{"segfault", cartridge.ErrCrash},
	}
	for _, tc := range cases {
		t.Run(tc.stderr, func(t *testing.T) {
			spec := claudeSpec(t, map[string]string{"SC_CLAUDE_STDERR": tc.stderr, "SC_CLAUDE_EXIT": "1"})
			s, err := a.Run(context.Background(), spec)
			require.NoError(t, err)
			res, err := cartridge.Collect(s, nil)
			require.Error(t, err)
			assert.True(t, errors.Is(err, tc.want), "%v", err)
			assert.Contains(t, err.Error(), tc.stderr)
			assert.Equal(t, 1, res.ExitCode)
			assert.Equal(t, cartridge.Usage{}, res.Usage)
		})
	}
}

func TestRun_ExitZeroWithoutResult(t *testing.T) {
	claudeShim(t)
	a := claudeNewAdapter(t, config.Cartridge{})
	s, err := a.Run(context.Background(), claudeSpec(t, nil))
	require.NoError(t, err)
	_, err = cartridge.Collect(s, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, cartridge.ErrCrash))
	assert.Contains(t, err.Error(), "without a result line")
}

func TestRun_Timeout(t *testing.T) {
	claudeShim(t)
	a := claudeNewAdapter(t, config.Cartridge{})
	spec := claudeSpec(t, map[string]string{"SC_CLAUDE_SLEEP": "10"})
	spec.Timeout = 300 * time.Millisecond

	start := time.Now()
	s, err := a.Run(context.Background(), spec)
	require.NoError(t, err)
	res, err := cartridge.Collect(s, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, cartridge.ErrTimeout), "%v", err)
	assert.Less(t, time.Since(start), 8*time.Second, "the process group is killed after the grace period")
	assert.Equal(t, -1, res.ExitCode)
}

func TestRun_Kill(t *testing.T) {
	claudeShim(t)
	a := claudeNewAdapter(t, config.Cartridge{})
	s, err := a.Run(context.Background(), claudeSpec(t, map[string]string{"SC_CLAUDE_SLEEP": "10"}))
	require.NoError(t, err)
	time.AfterFunc(100*time.Millisecond, s.Kill)
	_, err = cartridge.Collect(s, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, cartridge.ErrKilled), "%v", err)
	assert.False(t, cartridge.KindOf(err).Retryable())
}

func TestRun_ContextCancel(t *testing.T) {
	claudeShim(t)
	a := claudeNewAdapter(t, config.Cartridge{})
	ctx, cancel := context.WithCancel(context.Background())
	s, err := a.Run(ctx, claudeSpec(t, map[string]string{"SC_CLAUDE_SLEEP": "10"}))
	require.NoError(t, err)
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err = cartridge.Collect(s, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, cartridge.ErrKilled), "%v", err)
}

func TestRun_InvalidSpec(t *testing.T) {
	shim := claudeShim(t)
	a := claudeNewAdapter(t, config.Cartridge{})
	spec := claudeSpec(t, nil)
	spec.Prompt = " "
	_, err := a.Run(context.Background(), spec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty prompt")
	spec = claudeSpec(t, nil)
	spec.Workdir = "relative"
	_, err = a.Run(context.Background(), spec)
	require.Error(t, err)
	assert.Empty(t, shim.Invocations(t), "nothing is started on a bad spec")
}

func TestRun_NotInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	a := claudeNewAdapter(t, config.Cartridge{})
	_, err := a.Run(context.Background(), claudeSpec(t, nil))
	require.Error(t, err)
	assert.True(t, errors.Is(err, cartridge.ErrNotInstalled))
}

func TestRun_MissingWorkdirIsCrash(t *testing.T) {
	claudeShim(t)
	a := claudeNewAdapter(t, config.Cartridge{})
	spec := claudeSpec(t, nil)
	spec.Workdir = filepath.Join(t.TempDir(), "does-not-exist")
	_, err := a.Run(context.Background(), spec)
	require.Error(t, err)
	assert.True(t, errors.Is(err, cartridge.ErrCrash))
}

func TestRun_WaitWithoutDraining(t *testing.T) {
	claudeShim(t)
	a := claudeNewAdapter(t, config.Cartridge{})
	s, err := a.Run(context.Background(), claudeSpec(t, map[string]string{"SC_CLAUDE_FIXTURE": claudeFixture(t, "tool-use-read.jsonl")}))
	require.NoError(t, err)
	res, err := s.Wait()
	require.NoError(t, err)
	assert.Equal(t, "the quick brown fox", res.Output)
	n := 0
	for range s.Events() {
		n++
	}
	assert.Equal(t, 4, n, "events are still delivered after Wait")
}

func TestExtractJSONObject(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "", extractJSONObject("no json"))
	assert.Equal(t, `{"a":1}`, extractJSONObject(`pre {"a":1} post`))
	assert.Equal(t, `{"a":{"b":"}"}}`, extractJSONObject(`{"a":{"b":"}"}} tail`))
	assert.Equal(t, `{"a":"\"}"}`, extractJSONObject(`{"a":"\"}"}`))
	assert.Equal(t, `{"unterminated":`, extractJSONObject(`{"unterminated":`))
}
