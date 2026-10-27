package kimi

import (
	"context"
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
	"slop-cannon/internal/domain"
	"slop-cannon/internal/testutil"
)

const kimiShimScript = `
case "$1" in
  --version)
    if [ -n "$SC_KIMI_VERSION" ]; then echo "$SC_KIMI_VERSION"; else echo "2.3.1"; fi
    exit "${SC_KIMI_VERSION_EXIT:-0}";;
esac
if [ -n "$SC_KIMI_ENV_DUMP" ]; then env | sort > "$SC_KIMI_ENV_DUMP"; fi
profile=''
prompt=''
previous=''
for arg in "$@"; do
  if [ "$previous" = '--agent-file' ]; then profile="$arg"; fi
  if [ "$previous" = '--prompt' ]; then prompt="$arg"; fi
  previous="$arg"
done
if [ -n "$SC_KIMI_PROFILE_DUMP" ] && [ -n "$profile" ]; then cp "$profile" "$SC_KIMI_PROFILE_DUMP"; fi
if [ -n "$SC_KIMI_TASK_CONTENT_DUMP" ]; then
  taskfile=$(printf '%s\n' "$prompt" | sed -n 's/^Use the Read tool to read this exact file: "\([^"]*\)".*$/\1/p')
  if [ -z "$taskfile" ] || [ ! -r "$taskfile" ]; then exit 92; fi
  cp -p "$taskfile" "$SC_KIMI_TASK_CONTENT_DUMP"
  printf '%s' "$taskfile" > "$SC_KIMI_TASK_PATH_DUMP"
fi
if [ -n "$SC_KIMI_SLEEP" ]; then sleep "$SC_KIMI_SLEEP"; fi
if [ -n "$SC_KIMI_STDERR" ]; then echo "$SC_KIMI_STDERR" >&2; fi
if [ -n "$SC_KIMI_FIXTURE" ]; then cat "$SC_KIMI_FIXTURE"; fi
exit "${SC_KIMI_EXIT:-0}"
`

func kimiShim(t *testing.T) *testutil.Shim {
	t.Helper()
	return testutil.FakeBin(t, "kimi", kimiShimScript, testutil.WithStdinCapture())
}

func kimiNewAdapter(t *testing.T, cfg config.Cartridge) *Adapter {
	t.Helper()
	created, err := New("", cfg)
	require.NoError(t, err)
	adapter, ok := created.(*Adapter)
	require.True(t, ok)
	return adapter
}

func kimiSpec(t *testing.T, env map[string]string) cartridge.RunSpec {
	t.Helper()
	return cartridge.RunSpec{
		Prompt:       "Reply with exactly PONG",
		SystemPrompt: "Follow the test contract.",
		Workdir:      t.TempDir(),
		Model:        "kimi-code/kimi-for-coding",
		ToolPolicy:   cartridge.ToolReadExec,
		MaxTurns:     4,
		Timeout:      20 * time.Second,
		Env:          env,
	}
}

func kimiFixture(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", "kimi", name))
	require.NoError(t, err)
	return path
}

func TestDetect_VersionAndAuthUnknown(t *testing.T) {
	shim := kimiShim(t)
	adapter := kimiNewAdapter(t, config.Cartridge{Args: []string{"--debug"}})
	st, err := adapter.Detect(context.Background())
	require.NoError(t, err)
	assert.True(t, st.Installed)
	assert.Equal(t, shim.Path, st.Path)
	assert.Equal(t, "2.3.1", st.Version)
	assert.Equal(t, cartridge.AuthUnknown, st.Auth)
	assert.Contains(t, st.Detail, "no cheap authentication")
	assert.True(t, st.Ready())
	require.Equal(t, [][]string{{"--version"}}, shim.ArgLists(t))
}

func TestDetect_TooOldAndMissing(t *testing.T) {
	kimiShim(t)
	adapter := kimiNewAdapter(t, config.Cartridge{Env: map[string]string{"SC_KIMI_VERSION": "2.3.0"}})
	st, err := adapter.Detect(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, cartridge.ErrNotInstalled)
	assert.True(t, st.Installed)
	assert.Equal(t, "2.3.0", st.Version)

	t.Setenv("PATH", t.TempDir())
	st, err = adapter.Detect(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, cartridge.ErrNotInstalled)
	assert.False(t, st.Installed)
}

func TestRun_ArgsPromptProfileAndEvents(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	shim := kimiShim(t)
	profileDump := filepath.Join(t.TempDir(), "profile.md")
	spec := kimiSpec(t, map[string]string{
		"SC_KIMI_FIXTURE":      kimiFixture(t, "success.jsonl"),
		"SC_KIMI_PROFILE_DUMP": profileDump,
	})
	spec.SystemPrompt = "Literal ${base_prompt} and {% include 'x' %} must stay data."
	adapter := kimiNewAdapter(t, config.Cartridge{Model: "configured-model", Args: []string{"--debug"}})

	session, err := adapter.Run(context.Background(), spec)
	require.NoError(t, err)
	var events []cartridge.SessionEvent
	result, err := cartridge.Collect(session, func(ev cartridge.SessionEvent) { events = append(events, ev) })
	require.NoError(t, err)
	assert.Equal(t, "Final answer.", result.Output)
	assert.Equal(t, "session-abc-123", result.SessionID)
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, cartridge.Usage{}, result.Usage, "Kimi reports no usage; do not invent it")
	assert.Equal(t, "Final answer.", cartridge.Transcript(events[len(events)-2:]))
	assert.Equal(t, []cartridge.EventKind{
		cartridge.EventTextDelta, cartridge.EventToolUse, cartridge.EventToolResult,
		cartridge.EventTextDelta, cartridge.EventDone,
	}, eventKinds(events))
	assert.Equal(t, "Bash", events[1].Tool)
	assert.Equal(t, "git status --short", events[1].Input)
	assert.Equal(t, "call-1", events[2].ToolID)
	assert.Contains(t, events[2].Output, "adapter.go")

	calls := shim.Invocations(t)
	require.Len(t, calls, 1)
	assert.Equal(t, testutil.RealPath(t, spec.Workdir), calls[0].Dir)
	assert.Equal(t, Input(spec), calls[0].Stdin)
	require.Len(t, calls[0].Args, 9)
	assert.Equal(t, []string{
		"--prompt", Input(spec), "--output-format", "stream-json",
		"--agent-file", calls[0].Args[5], "--model", spec.Model, "--debug",
	}, calls[0].Args)
	assert.NotContains(t, calls[0].Args, "--work-dir")
	assert.NotContains(t, calls[0].Args, "--max-steps-per-turn")

	profile, err := os.ReadFile(profileDump)
	require.NoError(t, err)
	yaml := string(profile)
	assert.Contains(t, yaml, "name: sc-kimi")
	assert.Contains(t, yaml, "${base_prompt}")
	assert.NotContains(t, yaml, spec.SystemPrompt)
	assert.Contains(t, yaml, toolShell)
	assert.Contains(t, yaml, toolRead)
	assert.NotContains(t, yaml, "Write")
	assert.NotContains(t, yaml, "Edit")
	assert.Contains(t, yaml, "subagents: []")
	assert.Empty(t, mustReadDir(t, spec.Workdir), "profile and prompt are removed after success")
}

func TestRun_OversizedPromptUsesReadableWorkdirFile(t *testing.T) {
	shim := kimiShim(t)
	dumps := t.TempDir()
	contentDump := filepath.Join(dumps, "content.txt")
	pathDump := filepath.Join(dumps, "path.txt")
	spec := kimiSpec(t, map[string]string{
		"SC_KIMI_FIXTURE":           kimiFixture(t, "retry.jsonl"),
		"SC_KIMI_TASK_CONTENT_DUMP": contentDump,
		"SC_KIMI_TASK_PATH_DUMP":    pathDump,
	})
	spec.Prompt = strings.Repeat("large prompt with ${literal} {% include 'x' %}\n", 6000)
	require.Greater(t, len(Input(spec)), 128*1024)
	adapter := kimiNewAdapter(t, config.Cartridge{})
	session, err := adapter.Run(context.Background(), spec)
	require.NoError(t, err)
	result, err := cartridge.Collect(session, nil)
	require.NoError(t, err)
	assert.Equal(t, "Recovered.", result.Output)

	calls := shim.Invocations(t)
	require.Len(t, calls, 1)
	for _, arg := range calls[0].Args {
		assert.Less(t, len(arg), maxPromptArgBytes, "large prompt never appears in argv")
	}
	taskPathBytes, err := os.ReadFile(pathDump)
	require.NoError(t, err)
	taskPath := string(taskPathBytes)
	assert.Equal(t, readPromptInstruction(taskPath), calls[0].Args[1])
	rel, err := filepath.Rel(spec.Workdir, taskPath)
	require.NoError(t, err)
	assert.False(t, strings.HasPrefix(rel, ".."), "prompt file is inside the allowed workdir")
	content, err := os.ReadFile(contentDump)
	require.NoError(t, err)
	assert.Equal(t, Input(spec), string(content), "complete combined prompt and structured suffix are preserved")
	info, err := os.Stat(contentDump)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	_, err = os.Stat(taskPath)
	assert.True(t, os.IsNotExist(err), "prompt file is removed after success")
	assert.Empty(t, mustReadDir(t, spec.Workdir))
}

func TestRun_Environment(t *testing.T) {
	kimiShim(t)
	t.Setenv("KIMI_MODEL_THINKING_EFFORT", "high")
	t.Setenv("KIMI_CODE_HOME", "/tmp/kimi-home")
	t.Setenv("SC_KIMI_LEAK", "must-not-leak")
	dump := filepath.Join(t.TempDir(), "env.txt")
	spec := kimiSpec(t, map[string]string{
		"SC_KIMI_FIXTURE":            kimiFixture(t, "retry.jsonl"),
		"SC_KIMI_ENV_DUMP":           dump,
		"KIMI_MODEL_THINKING_EFFORT": "max",
	})
	adapter := kimiNewAdapter(t, config.Cartridge{Env: map[string]string{"FROM_CONFIG": "yes"}})
	session, err := adapter.Run(context.Background(), spec)
	require.NoError(t, err)
	_, err = cartridge.Collect(session, nil)
	require.NoError(t, err)
	data, err := os.ReadFile(dump)
	require.NoError(t, err)
	env := string(data)
	assert.Contains(t, env, "KIMI_MODEL_THINKING_EFFORT=max\n")
	assert.Contains(t, env, "KIMI_CODE_HOME=/tmp/kimi-home\n")
	assert.Contains(t, env, "KIMI_DISABLE_TELEMETRY=1\n")
	assert.Contains(t, env, "KIMI_CODE_NO_AUTO_UPDATE=1\n")
	assert.Contains(t, env, "KIMI_LOOP_MAX_STEPS_PER_TURN=4\n")
	assert.Contains(t, env, "FROM_CONFIG=yes\n")
	assert.NotContains(t, env, "SC_KIMI_LEAK=")
}

func TestProfile_ReadWriteAndWeb(t *testing.T) {
	spec := kimiSpec(t, nil)
	spec.ToolPolicy = cartridge.ToolReadWrite
	profile, err := Profile(spec)
	require.NoError(t, err)
	assert.NotContains(t, profile, "tools:\n", "omitting tools enables the full native tool set")
	assert.Contains(t, profile, "subagents: []")

	spec.ToolPolicy = cartridge.ToolReadExec
	spec.Web = true
	profile, err = Profile(spec)
	require.NoError(t, err)
	assert.Contains(t, profile, toolSearchWeb)
	assert.Contains(t, profile, toolFetchURL)
	assert.NotContains(t, profile, "Agent")
	assert.NotContains(t, profile, "Write")
	assert.NotContains(t, profile, "Edit")
}

func TestToolPolicyEnforcement(t *testing.T) {
	adapter := kimiNewAdapter(t, config.Cartridge{})
	for _, policy := range []cartridge.ToolPolicy{cartridge.ToolReadExec, cartridge.ToolReadWrite} {
		enforcement, err := adapter.ToolPolicyEnforcement(policy)
		require.NoError(t, err)
		assert.Equal(t, agents.EnforceNative, enforcement.Mode)
		assert.NoError(t, enforcement.Check())
		assert.Contains(t, enforcement.Controls, "subagents=[]")
	}
	_, err := adapter.ToolPolicyEnforcement(cartridge.ToolReadOnly)
	assert.ErrorIs(t, err, ErrReadOnlyUnavailable)
	_, err = adapter.ToolPolicyEnforcement(cartridge.ToolPolicy("unsafe"))
	assert.ErrorIs(t, err, domain.ErrUnknownValue)
}

func TestRun_ReadOnlyRefusedBeforeStart(t *testing.T) {
	shim := kimiShim(t)
	adapter := kimiNewAdapter(t, config.Cartridge{})
	spec := kimiSpec(t, nil)
	spec.ToolPolicy = cartridge.ToolReadOnly
	_, err := adapter.Run(context.Background(), spec)
	assert.ErrorIs(t, err, ErrReadOnlyUnavailable)
	assert.Empty(t, shim.Invocations(t))
}

func TestRun_MachineReadableRateLimitAndQuota(t *testing.T) {
	kimiShim(t)
	adapter := kimiNewAdapter(t, config.Cartridge{})
	for _, tc := range []struct {
		fixture string
		want    error
		kind    cartridge.Kind
	}{
		{"rate-limit.jsonl", cartridge.ErrRateLimited, cartridge.KindRateLimited},
		{"quota.jsonl", cartridge.ErrCreditsExhausted, cartridge.KindCreditsExhausted},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			spec := kimiSpec(t, map[string]string{"SC_KIMI_FIXTURE": kimiFixture(t, tc.fixture), "SC_KIMI_EXIT": "75"})
			session, err := adapter.Run(context.Background(), spec)
			require.NoError(t, err)
			result, err := cartridge.Collect(session, nil)
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.want)
			assert.Equal(t, tc.kind, cartridge.KindOf(err))
			assert.Equal(t, 75, result.ExitCode)
			assert.Contains(t, err.Error(), "75")
		})
	}
}

func TestRun_StderrFailuresAndExitStatuses(t *testing.T) {
	kimiShim(t)
	adapter := kimiNewAdapter(t, config.Cartridge{})
	for _, tc := range []struct {
		stderr string
		want   error
	}{
		{"Error: authentication failed; please login", cartridge.ErrAuth},
		{"HTTP 429 too many requests", cartridge.ErrRateLimited},
		{"HTTP 429 quota exceeded", cartridge.ErrCreditsExhausted},
		{"configuration is invalid", cartridge.ErrCrash},
	} {
		t.Run(tc.stderr, func(t *testing.T) {
			spec := kimiSpec(t, map[string]string{"SC_KIMI_STDERR": tc.stderr, "SC_KIMI_EXIT": "1"})
			session, err := adapter.Run(context.Background(), spec)
			require.NoError(t, err)
			result, err := cartridge.Collect(session, nil)
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.want)
			assert.Contains(t, err.Error(), "status 1")
			assert.Contains(t, err.Error(), tc.stderr)
			assert.Equal(t, 1, result.ExitCode)
		})
	}
}

func TestRun_ExitZeroWithoutAssistant(t *testing.T) {
	kimiShim(t)
	adapter := kimiNewAdapter(t, config.Cartridge{})
	session, err := adapter.Run(context.Background(), kimiSpec(t, nil))
	require.NoError(t, err)
	_, err = cartridge.Collect(session, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, cartridge.ErrCrash)
	assert.Contains(t, err.Error(), "without an assistant message")
}

func TestRun_TimeoutAndKillCleanFiles(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(context.Context, *Adapter, cartridge.RunSpec) error
		want error
	}{
		{"timeout", func(ctx context.Context, adapter *Adapter, spec cartridge.RunSpec) error {
			spec.Timeout = 250 * time.Millisecond
			session, err := adapter.Run(ctx, spec)
			if err != nil {
				return err
			}
			_, err = cartridge.Collect(session, nil)
			return err
		}, cartridge.ErrTimeout},
		{"kill", func(ctx context.Context, adapter *Adapter, spec cartridge.RunSpec) error {
			session, err := adapter.Run(ctx, spec)
			if err != nil {
				return err
			}
			time.AfterFunc(100*time.Millisecond, session.Kill)
			_, err = cartridge.Collect(session, nil)
			return err
		}, cartridge.ErrKilled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kimiShim(t)
			adapter := kimiNewAdapter(t, config.Cartridge{})
			spec := kimiSpec(t, map[string]string{"SC_KIMI_SLEEP": "10"})
			spec.Prompt = strings.Repeat("timeout and kill prompt larger than 128 KiB\n", 4000)
			require.Greater(t, len(Input(spec)), 128*1024)
			err := tc.run(context.Background(), adapter, spec)
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.want)
			assert.Empty(t, mustReadDir(t, spec.Workdir))
		})
	}
}

func TestRun_StartFailureCleansFiles(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	kimiShim(t)
	adapter := kimiNewAdapter(t, config.Cartridge{})
	spec := kimiSpec(t, nil)
	spec.Workdir = filepath.Join(t.TempDir(), "missing")
	_, err := adapter.Run(context.Background(), spec)
	require.Error(t, err)
	assert.ErrorIs(t, err, cartridge.ErrCrash)
	assert.Empty(t, mustReadDir(t, tmp))
}

func TestRun_OversizedPromptStartFailureCleansFiles(t *testing.T) {
	kimiShim(t)
	missingBinary := filepath.Join(t.TempDir(), "missing-kimi")
	adapter := kimiNewAdapter(t, config.Cartridge{Command: missingBinary})
	spec := kimiSpec(t, nil)
	spec.Prompt = strings.Repeat("start failure prompt larger than 128 KiB\n", 4000)
	require.Greater(t, len(Input(spec)), 128*1024)
	_, err := adapter.Run(context.Background(), spec)
	require.Error(t, err)
	assert.ErrorIs(t, err, cartridge.ErrCrash)
	assert.Empty(t, mustReadDir(t, spec.Workdir))
}

func TestRun_InvalidSpecAndNotInstalled(t *testing.T) {
	shim := kimiShim(t)
	adapter := kimiNewAdapter(t, config.Cartridge{})
	spec := kimiSpec(t, nil)
	spec.Prompt = " "
	_, err := adapter.Run(context.Background(), spec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty prompt")
	assert.Empty(t, shim.Invocations(t))

	t.Setenv("PATH", t.TempDir())
	_, err = adapter.Run(context.Background(), kimiSpec(t, nil))
	assert.ErrorIs(t, err, cartridge.ErrNotInstalled)
}

func TestRun_WaitWithoutDraining(t *testing.T) {
	kimiShim(t)
	adapter := kimiNewAdapter(t, config.Cartridge{})
	session, err := adapter.Run(context.Background(), kimiSpec(t, map[string]string{"SC_KIMI_FIXTURE": kimiFixture(t, "success.jsonl")}))
	require.NoError(t, err)
	result, err := session.Wait()
	require.NoError(t, err)
	assert.Equal(t, "Final answer.", result.Output)
	count := 0
	for range session.Events() {
		count++
	}
	assert.Equal(t, 5, count)
}

func TestParser_RetryAndMalformedNoise(t *testing.T) {
	parser := NewParser()
	assert.Empty(t, parser.Line("not json"))
	assert.Empty(t, parser.Line("{broken"))
	events := parser.Line(`{"role":"assistant","tool_calls":[{"id":"1","function":{"name":"ReadFile","arguments":"{\"path\":\"/tmp/a\"}"}}]}`)
	require.Len(t, events, 1)
	assert.Equal(t, "/tmp/a", events[0].Input)
	assert.Empty(t, parser.Line(`{"role":"meta","type":"turn.step.retrying","status_code":429,"error_message":"slow down"}`))
	assert.Equal(t, 429, parser.Retry().statusCode)
	assert.True(t, parser.HasAssistant())
	assert.Equal(t, 1, parser.assistant)
}

func TestKindForStatus(t *testing.T) {
	assert.Equal(t, cartridge.KindRateLimited, KindForStatus(429))
	assert.Equal(t, cartridge.KindCreditsExhausted, KindForStatus(402))
	assert.Equal(t, cartridge.KindAuth, KindForStatus(401))
	assert.Equal(t, cartridge.KindCrash, KindForStatus(400))
	assert.Equal(t, cartridge.KindUnknown, KindForStatus(0))
}

func eventKinds(events []cartridge.SessionEvent) []cartridge.EventKind {
	kinds := make([]cartridge.EventKind, len(events))
	for i, event := range events {
		kinds[i] = event.Kind
	}
	return kinds
}

func mustReadDir(t *testing.T, path string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(path)
	require.NoError(t, err)
	return entries
}
