package cursor

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

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/testutil"
)

// cursorShimScript is the fake "cursor-agent" used by the adapter tests.
// Probes answer from files; a firing replays a recorded stream-json fixture.
const cursorShimScript = `
case "$1" in
  --version)
    if [ -n "$SC_CURSOR_VERSION" ]; then echo "$SC_CURSOR_VERSION"; else echo "2026.09.28-64d2043"; fi
    exit 0;;
  status)
    if [ -n "$SC_CURSOR_STATUS_FILE" ]; then cat "$SC_CURSOR_STATUS_FILE"; fi
    exit 0;;
esac
if [ -n "$SC_CURSOR_ENV_DUMP" ]; then env | sort > "$SC_CURSOR_ENV_DUMP"; fi
if [ -n "$SC_CURSOR_SLEEP" ]; then sleep "$SC_CURSOR_SLEEP"; fi
if [ -n "$SC_CURSOR_STDERR_FILE" ]; then cat "$SC_CURSOR_STDERR_FILE" >&2; fi
if [ -n "$SC_CURSOR_FIXTURE" ]; then cat "$SC_CURSOR_FIXTURE"; fi
exit "${SC_CURSOR_EXIT:-0}"
`

func cursorShim(t *testing.T) *testutil.Shim {
	t.Helper()
	return testutil.FakeBin(t, DefaultBinary, cursorShimScript, testutil.WithStdinCapture())
}

func cursorFixture(t testing.TB, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", "cursor", name))
	require.NoError(t, err)
	_, err = os.Stat(p)
	require.NoError(t, err)
	return p
}

func cursorAdapter(t *testing.T, cfg config.Cartridge) *Adapter {
	t.Helper()
	c, err := New(Name, cfg)
	require.NoError(t, err)
	a, ok := c.(*Adapter)
	require.True(t, ok)
	return a
}

func cursorSpec(t *testing.T, env map[string]string) cartridge.RunSpec {
	t.Helper()
	return cartridge.RunSpec{
		Prompt:       "Reply with exactly the word OK",
		SystemPrompt: "You are a test.",
		Workdir:      t.TempDir(),
		ToolPolicy:   cartridge.ToolReadWrite,
		MaxTurns:     3,
		Timeout:      20 * time.Second,
		Env:          env,
	}
}

func run(t *testing.T, a *Adapter, spec cartridge.RunSpec) ([]cartridge.SessionEvent, cartridge.Result, error) {
	t.Helper()
	s, err := a.Run(context.Background(), spec)
	require.NoError(t, err)
	var evs []cartridge.SessionEvent
	res, err := cartridge.Collect(s, func(ev cartridge.SessionEvent) { evs = append(evs, ev) })
	return evs, res, err
}

func TestRegistered(t *testing.T) {
	c, err := cartridge.New(Name, config.Cartridge{Model: "auto"})
	require.NoError(t, err)
	assert.Equal(t, Name, c.Name())
}

func TestDetect_LoggedIn(t *testing.T) {
	shim := cursorShim(t)
	a := cursorAdapter(t, config.Cartridge{Env: map[string]string{"SC_CURSOR_STATUS_FILE": cursorFixture(t, "status-logged-in.txt")}})

	st, err := a.Detect(context.Background())
	require.NoError(t, err)
	assert.True(t, st.Installed)
	assert.Equal(t, shim.Path, st.Path)
	assert.Equal(t, "2026.09.28-64d2043", st.Version)
	assert.Equal(t, cartridge.AuthOK, st.Auth)
	assert.Equal(t, "logged in as user@example.com", st.Detail)
	assert.Equal(t, [][]string{{"--version"}, {"status"}}, shim.ArgLists(t))
}

func TestDetect_LoggedOut(t *testing.T) {
	cursorShim(t)
	a := cursorAdapter(t, config.Cartridge{Env: map[string]string{"SC_CURSOR_STATUS_FILE": cursorFixture(t, "status-logged-out.txt")}})

	st, err := a.Detect(context.Background())
	require.Error(t, err)
	assert.True(t, errors.Is(err, cartridge.ErrAuth))
	assert.Equal(t, cartridge.AuthMissing, st.Auth)
}

func TestDetect_TooOld(t *testing.T) {
	cursorShim(t)
	a := cursorAdapter(t, config.Cartridge{Env: map[string]string{"SC_CURSOR_VERSION": "2025.01.01-abc"}})

	_, err := a.Detect(context.Background())
	require.Error(t, err)
	assert.Equal(t, cartridge.KindNotInstalled, cartridge.KindOf(err))
}

func TestDetect_Missing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	a := cursorAdapter(t, config.Cartridge{})
	st, err := a.Detect(context.Background())
	require.Error(t, err)
	assert.False(t, st.Installed)
	assert.Equal(t, cartridge.KindNotInstalled, cartridge.KindOf(err))
}

func TestArgs_ModelFromSpecElseConfig(t *testing.T) {
	a := cursorAdapter(t, config.Cartridge{Model: "auto", Args: []string{"--approve-mcps"}})
	spec := cartridge.RunSpec{Workdir: "/w", Prompt: "p", ToolPolicy: cartridge.ToolReadOnly}
	assert.Equal(t, []string{
		"-p", "--output-format", "stream-json", "--force", "--trust", "--sandbox", "disabled",
		"--workspace", "/w", "--model", "auto", "--approve-mcps",
	}, a.Args(spec))

	spec.Model = "composer-2.5"
	assert.Contains(t, strings.Join(a.Args(spec), " "), "--model composer-2.5")

	noModel := cursorAdapter(t, config.Cartridge{})
	assert.NotContains(t, noModel.Args(cartridge.RunSpec{Workdir: "/w"}), flagModel)
}

func TestStdin_SystemPromptPrepended(t *testing.T) {
	assert.Equal(t, "sys"+SystemPromptSeparator+"do it", Stdin(cartridge.RunSpec{SystemPrompt: "sys", Prompt: "do it"}))
	assert.Equal(t, "do it", Stdin(cartridge.RunSpec{Prompt: "do it"}))
}

func TestRun_ReplaysToolsFixture(t *testing.T) {
	shim := cursorShim(t)
	a := cursorAdapter(t, config.Cartridge{Model: "auto"})
	spec := cursorSpec(t, map[string]string{"SC_CURSOR_FIXTURE": cursorFixture(t, "tools-reply.jsonl")})

	evs, res, err := run(t, a, spec)
	require.NoError(t, err)
	assert.Equal(t, "OK", res.Output)
	assert.Equal(t, "8e33eac0-1a94-496e-b751-20402a6b6085", res.SessionID)
	assert.Equal(t, 18405, res.Usage.InputTokens)
	assert.Equal(t, 135, res.Usage.OutputTokens)
	assert.Equal(t, 22784, res.Usage.CacheReadTokens)
	assert.False(t, res.Usage.HasCost)

	var tools []string
	for _, ev := range evs {
		if ev.Kind == cartridge.EventToolUse {
			tools = append(tools, ev.Tool+": "+ev.Input)
		}
	}
	assert.Equal(t, []string{"Shell: cat a.txt", "Edit: /tmp/sc-fixture/b.txt"}, tools)
	assert.Equal(t, cartridge.EventDone, evs[len(evs)-1].Kind)

	calls := shim.Invocations(t)
	require.Len(t, calls, 1)
	assert.Equal(t, "You are a test."+SystemPromptSeparator+"Reply with exactly the word OK", calls[0].Stdin)
	assert.Equal(t, spec.Workdir, calls[0].Dir)
	assert.Contains(t, calls[0].Args, "auto")
}

func TestRun_ShellFailureIsToolError(t *testing.T) {
	cursorShim(t)
	a := cursorAdapter(t, config.Cartridge{})
	evs, res, err := run(t, a, cursorSpec(t, map[string]string{"SC_CURSOR_FIXTURE": cursorFixture(t, "shell-failure.jsonl")}))
	require.NoError(t, err)
	assert.Equal(t, "DONE", res.Output, "the narration before the tool call is not the answer")
	var failed *cartridge.SessionEvent
	for i := range evs {
		if evs[i].Kind == cartridge.EventToolResult {
			failed = &evs[i]
		}
	}
	require.NotNil(t, failed)
	assert.True(t, failed.IsError)
	assert.Contains(t, failed.Output, "exit 2")
	assert.Contains(t, failed.Output, "No such file or directory")
}

func TestRun_BackgroundedCommandIsWaitedFor(t *testing.T) {
	cursorShim(t)
	a := cursorAdapter(t, config.Cartridge{})
	evs, res, err := run(t, a, cursorSpec(t, map[string]string{"SC_CURSOR_FIXTURE": cursorFixture(t, "background-shell.jsonl")}))
	require.NoError(t, err)
	assert.Contains(t, res.Output, "LATE")
	var bg bool
	for _, ev := range evs {
		if ev.Kind == cartridge.EventToolResult && strings.Contains(ev.Output, "moved to the background") {
			bg = true
		}
	}
	assert.True(t, bg, "a backgrounded command is labelled in its tool result")
}

func TestRun_ReadAndGrepTools(t *testing.T) {
	cursorShim(t)
	a := cursorAdapter(t, config.Cartridge{})
	evs, res, err := run(t, a, cursorSpec(t, map[string]string{"SC_CURSOR_FIXTURE": cursorFixture(t, "read-grep.jsonl")}))
	require.NoError(t, err)
	assert.Contains(t, res.Output, "READ-OK")
	var tools []string
	for _, ev := range evs {
		if ev.Kind == cartridge.EventToolUse {
			tools = append(tools, ev.Tool)
		}
	}
	assert.Equal(t, []string{"Read", "Grep"}, tools)
}

func TestRun_UnknownModelIsCrash(t *testing.T) {
	cursorShim(t)
	a := cursorAdapter(t, config.Cartridge{})
	_, _, err := run(t, a, cursorSpec(t, map[string]string{
		"SC_CURSOR_STDERR_FILE": cursorFixture(t, "unknown-model.stderr"), "SC_CURSOR_EXIT": "1",
	}))
	require.Error(t, err)
	assert.Equal(t, cartridge.KindCrash, cartridge.KindOf(err))
	assert.Contains(t, err.Error(), "Cannot use this model")
}

func TestRun_LoggedOutIsAuth(t *testing.T) {
	cursorShim(t)
	a := cursorAdapter(t, config.Cartridge{})
	_, _, err := run(t, a, cursorSpec(t, map[string]string{
		"SC_CURSOR_STDERR_FILE": cursorFixture(t, "logged-out.stderr"), "SC_CURSOR_EXIT": "1",
	}))
	require.Error(t, err)
	assert.True(t, errors.Is(err, cartridge.ErrAuth))
}

func TestRun_NoResultLineIsCrash(t *testing.T) {
	cursorShim(t)
	a := cursorAdapter(t, config.Cartridge{})
	_, _, err := run(t, a, cursorSpec(t, nil))
	require.Error(t, err)
	assert.Equal(t, cartridge.KindCrash, cartridge.KindOf(err))
	assert.Contains(t, err.Error(), "without a result line")
}

func TestRun_Timeout(t *testing.T) {
	cursorShim(t)
	a := cursorAdapter(t, config.Cartridge{})
	spec := cursorSpec(t, map[string]string{"SC_CURSOR_SLEEP": "5"})
	spec.Timeout = 300 * time.Millisecond
	_, _, err := run(t, a, spec)
	require.Error(t, err)
	assert.Equal(t, cartridge.KindTimeout, cartridge.KindOf(err))
}

func TestRun_EnvIsClean(t *testing.T) {
	cursorShim(t)
	dump := filepath.Join(t.TempDir(), "env")
	t.Setenv("CURSOR_API_KEY", "k")
	t.Setenv("CURSOR_AGENT_CHAT_ID", "parent")
	t.Setenv("SC_SECRET_THING", "nope")
	a := cursorAdapter(t, config.Cartridge{})
	_, _, _ = run(t, a, cursorSpec(t, map[string]string{"SC_CURSOR_ENV_DUMP": dump}))
	data, err := os.ReadFile(dump)
	require.NoError(t, err)
	env := string(data)
	assert.Contains(t, env, "CURSOR_API_KEY=k")
	assert.NotContains(t, env, "CURSOR_AGENT_CHAT_ID")
	assert.NotContains(t, env, "SC_SECRET_THING")
}

func TestClassify_ErrorResult(t *testing.T) {
	p := NewParser()
	p.Line(`{"type":"result","subtype":"error","is_error":true,"result":"Too many requests, try again later","session_id":"s"}`)
	err := Classify(Name, p, nil, 0)
	require.Error(t, err)
	assert.Equal(t, cartridge.KindRateLimited, cartridge.KindOf(err))

	p = NewParser()
	p.Line(`{"type":"result","subtype":"error","is_error":true,"result":"something odd","session_id":"s"}`)
	assert.Equal(t, cartridge.KindCrash, cartridge.KindOf(Classify(Name, p, nil, 0)))
}

func TestKindForText_Precedence(t *testing.T) {
	assert.Equal(t, cartridge.KindCreditsExhausted, KindForText("Billing: you hit your limit"))
	assert.Equal(t, cartridge.KindAuth, KindForText("Error: Authentication required. Please run 'agent login' first"))
	assert.Equal(t, cartridge.KindRateLimited, KindForText("rate limit exceeded"))
	assert.Equal(t, cartridge.KindUnknown, KindForText("all good"))
}

func TestToolName(t *testing.T) {
	assert.Equal(t, "Shell", ToolName("shellToolCall"))
	assert.Equal(t, "Edit", ToolName("editToolCall"))
	assert.Equal(t, "WebSearch", ToolName("webSearchToolCall"))
	assert.Equal(t, "ToolCall", ToolName("ToolCall"))
}

// TestToolPolicy_FallsBackToCopiedWorktree pins the P6-20 contract: the CLI
// has no per-invocation tool controls, so read_write needs nothing and the
// read-only policies need the supervisor's throwaway copy.
func TestToolPolicy_FallsBackToCopiedWorktree(t *testing.T) {
	a := cursorAdapter(t, config.Cartridge{})
	_, native := any(a).(agents.ToolPolicyEnforcer)
	assert.False(t, native)

	en, err := agents.EnforcementFor(a, cartridge.ToolReadWrite)
	require.NoError(t, err)
	assert.Equal(t, agents.EnforceNone, en.Mode)
	for _, p := range []cartridge.ToolPolicy{cartridge.ToolReadOnly, cartridge.ToolReadExec} {
		en, err := agents.EnforcementFor(a, p)
		require.NoError(t, err)
		assert.Equal(t, agents.EnforceCopyWorktree, en.Mode, p)
	}
}
