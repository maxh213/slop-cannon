package grok

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/testutil"
)

const grokShimScript = `
case "$1" in
  --version)
    if [ -n "$SC_GROK_VERSION" ]; then echo "$SC_GROK_VERSION"; else echo "1.0.30"; fi
    exit "${SC_GROK_VERSION_EXIT:-0}";;
esac
prompt_file=''
for arg in "$@"; do
  if [ "$previous" = '--prompt-file' ]; then prompt_file="$arg"; fi
  previous="$arg"
done
if [ -n "$SC_GROK_ENV_DUMP" ]; then env | sort > "$SC_GROK_ENV_DUMP"; fi
if [ -n "$SC_GROK_PROMPT_COPY" ] && [ -n "$prompt_file" ]; then cp "$prompt_file" "$SC_GROK_PROMPT_COPY"; fi
if [ -n "$SC_GROK_BLOCK" ]; then
  trap 'if [ -n "$SC_GROK_TERM_FILE" ]; then echo term > "$SC_GROK_TERM_FILE"; fi; exit 0' TERM
  if [ -n "$SC_GROK_CHILD_PID_FILE" ]; then
    setsid sleep 30 &
    echo "$!" > "$SC_GROK_CHILD_PID_FILE"
  fi
  while :; do sleep 1; done
fi
if [ -n "$SC_GROK_SLEEP" ]; then sleep "$SC_GROK_SLEEP"; fi
if [ -n "$SC_GROK_STDERR" ]; then echo "$SC_GROK_STDERR" >&2; fi
if [ -n "$SC_GROK_FIXTURE" ]; then cat "$SC_GROK_FIXTURE"; fi
exit "${SC_GROK_EXIT:-0}"
`

func grokShim(t *testing.T) *testutil.Shim {
	t.Helper()
	return testutil.FakeBin(t, Name, grokShimScript)
}

func grokSpec(t *testing.T, env map[string]string) cartridge.RunSpec {
	t.Helper()
	return cartridge.RunSpec{
		Prompt: "Reply with exactly PONG", SystemPrompt: "You are a test.",
		Workdir: t.TempDir(), Model: "grok-test", ToolPolicy: cartridge.ToolReadExec,
		MaxTurns: 3, Timeout: 20 * time.Second, Env: env,
	}
}

func TestDetect(t *testing.T) {
	shim := grokShim(t)
	a := grokNewAdapter(t, config.Cartridge{Args: []string{flagReasoningEffort, "high"}})
	status, err := a.Detect(context.Background())
	require.NoError(t, err)
	assert.True(t, status.Installed)
	assert.Equal(t, shim.Path, status.Path)
	assert.Equal(t, VerifiedVersion, status.Version)
	assert.Equal(t, cartridge.AuthUnknown, status.Auth)
	assert.Contains(t, status.Detail, "XAI_API_KEY")
	assert.True(t, status.Ready())
	require.Equal(t, [][]string{{"--version"}}, shim.ArgLists(t), "configured firing args are not probe args")
}

func TestDetectMissingOldAndFailedVersion(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		a := grokNewAdapter(t, config.Cartridge{})
		status, err := a.Detect(context.Background())
		require.ErrorIs(t, err, cartridge.ErrNotInstalled)
		assert.False(t, status.Installed)
		assert.Contains(t, status.Detail, "not found")
	})
	t.Run("old", func(t *testing.T) {
		grokShim(t)
		a := grokNewAdapter(t, config.Cartridge{Env: map[string]string{"SC_GROK_VERSION": "1.0.12"}})
		status, err := a.Detect(context.Background())
		require.ErrorIs(t, err, cartridge.ErrNotInstalled)
		assert.True(t, status.Installed)
		assert.Equal(t, "1.0.12", status.Version)
		assert.Contains(t, status.Detail, "older than")
	})
	t.Run("failed", func(t *testing.T) {
		grokShim(t)
		a := grokNewAdapter(t, config.Cartridge{Env: map[string]string{"SC_GROK_VERSION_EXIT": "3"}})
		status, err := a.Detect(context.Background())
		require.ErrorIs(t, err, cartridge.ErrCrash)
		assert.True(t, status.Installed)
		assert.Equal(t, "grok --version failed", status.Detail)
	})
}

func TestRunLargePromptArgEnvAndCleanup(t *testing.T) {
	shim := grokShim(t)
	t.Setenv("XAI_API_KEY", "inherited-key")
	t.Setenv("GROK_HOME", "/isolated/grok-home")
	t.Setenv("SC_LEAK_CHECK", "must-not-leak")
	promptCopy := filepath.Join(t.TempDir(), "prompt-copy.txt")
	envDump := filepath.Join(t.TempDir(), "env.txt")
	largePrompt := strings.Repeat("large prompt with spaces and --flags\n", 12000)
	cfg := config.Cartridge{
		Model: "cfg-model", Args: []string{flagReasoningEffort, "high"},
		Env: map[string]string{"FROM_CFG": "1"},
	}
	a := grokNewAdapter(t, cfg)
	spec := grokSpec(t, map[string]string{
		"SC_GROK_FIXTURE":     grokFixture(t, "stream-success.jsonl"),
		"SC_GROK_PROMPT_COPY": promptCopy, "SC_GROK_ENV_DUMP": envDump,
	})
	spec.Prompt = largePrompt

	session, err := a.Run(context.Background(), spec)
	require.NoError(t, err)
	var events []cartridge.SessionEvent
	result, err := cartridge.Collect(session, func(ev cartridge.SessionEvent) { events = append(events, ev) })
	require.NoError(t, err)
	assert.Equal(t, "Hello world", result.Output)
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, "abc", result.SessionID)
	assert.True(t, result.Usage.HasCost)
	assert.Equal(t, []cartridge.EventKind{cartridge.EventToolUse, cartridge.EventToolResult, cartridge.EventTextDelta, cartridge.EventTextDelta, cartridge.EventDone}, grokKinds(events))
	assert.Equal(t, "Hello world", cartridge.Transcript(events))
	for _, ev := range events {
		assert.False(t, ev.At.IsZero())
	}

	calls := shim.Invocations(t)
	require.Len(t, calls, 1)
	assert.Equal(t, testutil.RealPath(t, spec.Workdir), calls[0].Dir)
	args := calls[0].Args
	assert.Equal(t, "high", args[indexOf(args, flagReasoningEffort)+1])
	assert.Equal(t, spec.Workdir, args[indexOf(args, flagCwd)+1])
	assert.Equal(t, spec.Model, args[indexOf(args, flagModel)+1])
	promptFile := args[indexOf(args, flagPromptFile)+1]
	_, err = os.Stat(promptFile)
	assert.ErrorIs(t, err, os.ErrNotExist, "prompt file is removed after the run")
	copied, err := os.ReadFile(promptCopy)
	require.NoError(t, err)
	assert.Equal(t, largePrompt, string(copied))

	data, err := os.ReadFile(envDump)
	require.NoError(t, err)
	environment := string(data)
	for _, want := range []string{"XAI_API_KEY=inherited-key\n", "GROK_HOME=/isolated/grok-home\n", "GROK_DISABLE_AUTOUPDATER=1\n", "FROM_CFG=1\n", "PATH="} {
		assert.Contains(t, environment, want)
	}
	assert.NotContains(t, environment, "SC_LEAK_CHECK=")
}

func TestRunWebPolicyReachesProcess(t *testing.T) {
	shim := grokShim(t)
	a := grokNewAdapter(t, config.Cartridge{})
	spec := grokSpec(t, map[string]string{"SC_GROK_FIXTURE": grokFixture(t, "stream-success.jsonl")})
	spec.Web = true
	session, err := a.Run(context.Background(), spec)
	require.NoError(t, err)
	_, err = cartridge.Collect(session, nil)
	require.NoError(t, err)
	args := shim.Invocations(t)[0].Args
	assert.Equal(t, "read_file,grep,list_dir,run_terminal_cmd,web_search,web_fetch", args[indexOf(args, flagTools)+1])
	assert.Equal(t, "Agent,search_replace", args[indexOf(args, flagDisallowedTools)+1])
	assert.Contains(t, args, flagNoLeader)
	assert.Contains(t, args, flagNoSubagents)
	assert.Contains(t, args, "WebFetch")
	assert.Contains(t, args, "WebSearch")
}

func TestRunStructuredErrors(t *testing.T) {
	grokShim(t)
	a := grokNewAdapter(t, config.Cartridge{})
	for _, tc := range []struct {
		fixture string
		want    error
	}{
		{"stream-auth.jsonl", cartridge.ErrAuth},
		{"stream-credits.jsonl", cartridge.ErrCreditsExhausted},
		{"stream-rate.jsonl", cartridge.ErrRateLimited},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			spec := grokSpec(t, map[string]string{"SC_GROK_FIXTURE": grokFixture(t, tc.fixture)})
			session, err := a.Run(context.Background(), spec)
			require.NoError(t, err)
			var events []cartridge.SessionEvent
			result, err := cartridge.Collect(session, func(ev cartridge.SessionEvent) { events = append(events, ev) })
			require.ErrorIs(t, err, tc.want)
			assert.Equal(t, 0, result.ExitCode)
			require.Equal(t, []cartridge.EventKind{cartridge.EventError}, grokKinds(events))
			assert.Equal(t, err, events[0].Err)
		})
	}
}

func TestRunStderrClassification(t *testing.T) {
	grokShim(t)
	a := grokNewAdapter(t, config.Cartridge{})
	for _, tc := range []struct {
		stderr string
		want   error
	}{
		{"not logged in: run grok login", cartridge.ErrAuth},
		{"insufficient credit for this request", cartridge.ErrCreditsExhausted},
		{"HTTP 429 too many requests", cartridge.ErrRateLimited},
		{"segmentation fault", cartridge.ErrCrash},
	} {
		t.Run(tc.stderr, func(t *testing.T) {
			spec := grokSpec(t, map[string]string{"SC_GROK_STDERR": tc.stderr, "SC_GROK_EXIT": "1"})
			session, err := a.Run(context.Background(), spec)
			require.NoError(t, err)
			result, err := cartridge.Collect(session, nil)
			require.ErrorIs(t, err, tc.want)
			assert.Contains(t, err.Error(), tc.stderr)
			assert.Equal(t, 1, result.ExitCode)
			assert.False(t, result.Usage.HasCost)
		})
	}
}

func TestRunIncompleteStream(t *testing.T) {
	grokShim(t)
	a := grokNewAdapter(t, config.Cartridge{})
	spec := grokSpec(t, map[string]string{"SC_GROK_FIXTURE": grokFixture(t, "stream-incomplete.jsonl")})
	session, err := a.Run(context.Background(), spec)
	require.NoError(t, err)
	result, err := cartridge.Collect(session, nil)
	require.ErrorIs(t, err, cartridge.ErrCrash)
	assert.Contains(t, err.Error(), "without an end event")
	assert.Equal(t, 12, result.Usage.InputTokens)
	assert.False(t, result.Usage.HasCost)
	assert.Zero(t, result.Usage.CostUSD)
}

func TestRunTimeoutKillAndContextCancel(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T, *Adapter, cartridge.RunSpec) error
		want error
	}{
		{"timeout", func(t *testing.T, a *Adapter, spec cartridge.RunSpec) error {
			spec.Timeout = 300 * time.Millisecond
			session, err := a.Run(context.Background(), spec)
			require.NoError(t, err)
			_, err = cartridge.Collect(session, nil)
			return err
		}, cartridge.ErrTimeout},
		{"kill", func(t *testing.T, a *Adapter, spec cartridge.RunSpec) error {
			session, err := a.Run(context.Background(), spec)
			require.NoError(t, err)
			time.AfterFunc(100*time.Millisecond, session.Kill)
			_, err = cartridge.Collect(session, nil)
			return err
		}, cartridge.ErrKilled},
		{"context", func(t *testing.T, a *Adapter, spec cartridge.RunSpec) error {
			ctx, cancel := context.WithCancel(context.Background())
			session, err := a.Run(ctx, spec)
			require.NoError(t, err)
			time.AfterFunc(100*time.Millisecond, cancel)
			_, err = cartridge.Collect(session, nil)
			return err
		}, cartridge.ErrKilled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			grokShim(t)
			a := grokNewAdapter(t, config.Cartridge{})
			spec := grokSpec(t, map[string]string{"SC_GROK_SLEEP": "10"})
			err := tc.run(t, a, spec)
			require.ErrorIs(t, err, tc.want)
		})
	}
}

func TestRunKillTermAndDescendantCleanup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descendant tag sweep is Linux-specific")
	}
	shim := grokShim(t)
	termFile := filepath.Join(t.TempDir(), "term.txt")
	childFile := filepath.Join(t.TempDir(), "child.pid")
	a := grokNewAdapter(t, config.Cartridge{})
	spec := grokSpec(t, map[string]string{
		"SC_GROK_BLOCK": "1", "SC_GROK_TERM_FILE": termFile,
		"SC_GROK_CHILD_PID_FILE": childFile,
	})
	session, err := a.Run(context.Background(), spec)
	require.NoError(t, err)
	var childPID int
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(childFile)
		if err != nil {
			return false
		}
		childPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil && childPID > 0
	}, 2*time.Second, 20*time.Millisecond)
	calls := shim.Invocations(t)
	require.Len(t, calls, 1)
	promptFile := calls[0].Args[indexOf(calls[0].Args, flagPromptFile)+1]
	_, err = os.Stat(promptFile)
	require.NoError(t, err, "prompt exists while the leader is running")

	session.Kill()
	result, err := cartridge.Collect(session, nil)
	require.ErrorIs(t, err, cartridge.ErrKilled)
	assert.Equal(t, 0, result.ExitCode, "the fake leader handles SIGTERM and exits cleanly")
	data, err := os.ReadFile(termFile)
	require.NoError(t, err, "leader receives SIGTERM before SIGKILL")
	assert.Equal(t, "term\n", string(data))
	_, err = os.Stat(promptFile)
	assert.ErrorIs(t, err, os.ErrNotExist)
	require.Eventually(t, func() bool { return !grokProcessRunning(childPID) }, 3*time.Second, 20*time.Millisecond, "detached descendant is reaped")
}

func TestRunStartFailureRemovesPrompt(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	t.Setenv("PATH", t.TempDir())
	a := grokNewAdapter(t, config.Cartridge{})
	_, err := a.Run(context.Background(), grokSpec(t, nil))
	require.ErrorIs(t, err, cartridge.ErrNotInstalled)
	entries, readErr := os.ReadDir(temp)
	require.NoError(t, readErr)
	assert.Empty(t, entries)
}

func TestRunReadOnlyFailsClosed(t *testing.T) {
	shim := grokShim(t)
	a := grokNewAdapter(t, config.Cartridge{})
	spec := grokSpec(t, nil)
	spec.ToolPolicy = cartridge.ToolReadOnly
	session, err := a.Run(context.Background(), spec)
	assert.Nil(t, session)
	require.ErrorIs(t, err, errReadOnlyUnsupported)
	assert.Empty(t, shim.Invocations(t))
}

func TestRunInvalidSpecAndMissingWorkdir(t *testing.T) {
	shim := grokShim(t)
	a := grokNewAdapter(t, config.Cartridge{})
	spec := grokSpec(t, nil)
	spec.Prompt = " "
	_, err := a.Run(context.Background(), spec)
	require.Error(t, err)
	assert.Empty(t, shim.Invocations(t))

	spec = grokSpec(t, nil)
	spec.Workdir = filepath.Join(t.TempDir(), "missing")
	_, err = a.Run(context.Background(), spec)
	require.ErrorIs(t, err, cartridge.ErrCrash)
}

func TestRunWaitWithoutDraining(t *testing.T) {
	grokShim(t)
	a := grokNewAdapter(t, config.Cartridge{})
	spec := grokSpec(t, map[string]string{"SC_GROK_FIXTURE": grokFixture(t, "stream-success.jsonl")})
	session, err := a.Run(context.Background(), spec)
	require.NoError(t, err)
	result, err := session.Wait()
	require.NoError(t, err)
	assert.Equal(t, "Hello world", result.Output)
	count := 0
	for range session.Events() {
		count++
	}
	assert.Equal(t, 5, count)
	result2, err := session.Wait()
	require.NoError(t, err)
	assert.Equal(t, result, result2)
}

func grokProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return errors.Is(err, syscall.EPERM)
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err == nil {
		fields := strings.Fields(string(data))
		if len(fields) > 2 && fields[2] == "Z" {
			return false
		}
	}
	return true
}
