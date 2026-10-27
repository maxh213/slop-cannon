package hermes

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

const hermesShimScript = `
if [ "$1" = "--version" ]; then
  if [ -n "$SC_HERMES_VERSION" ]; then echo "$SC_HERMES_VERSION"; else echo "Hermes Agent v0.22.5"; fi
  exit 0
fi
if [ -n "$SC_HERMES_ENV_DUMP" ]; then env | sort > "$SC_HERMES_ENV_DUMP"; fi
if [ -n "$SC_HERMES_ENV_DUMP_PREFIX" ]; then env | sort > "$SC_HERMES_ENV_DUMP_PREFIX.$SC_FAKEBIN_SEQ"; fi
if [ -n "$SC_HERMES_HOME_LOG" ]; then printf '%s|%s\n' "$HERMES_HOME" "$HOME" >> "$SC_HERMES_HOME_LOG"; fi
if [ -n "$SC_HERMES_AUTH_ALIAS_LOG" ]; then
  if [ -L "$HERMES_HOME/auth.json" ] && [ -L "$HERMES_HOME/auth.lock" ]; then echo linked > "$SC_HERMES_AUTH_ALIAS_LOG"; else echo missing > "$SC_HERMES_AUTH_ALIAS_LOG"; fi
fi
if [ -n "$SC_HERMES_STATE_WRITE" ]; then printf 'private\n' > "$HERMES_HOME/memory.json"; fi
if [ -n "$SC_HERMES_SLEEP" ]; then sleep "$SC_HERMES_SLEEP"; fi
if [ -n "$SC_HERMES_STDERR" ]; then echo "$SC_HERMES_STDERR" >&2; fi
if [ -n "$SC_HERMES_FIXTURE" ]; then cat "$SC_HERMES_FIXTURE"; fi
exit "${SC_HERMES_EXIT:-0}"
`

func hermesShim(t *testing.T) *testutil.Shim {
	t.Helper()
	return testutil.FakeBin(t, Name, hermesShimScript, testutil.WithStdinCapture())
}

func hermesSpec(t *testing.T, env map[string]string) cartridge.RunSpec {
	t.Helper()
	return cartridge.RunSpec{
		SystemPrompt: "You are a test.", Prompt: "Reply with exactly OK",
		Workdir: t.TempDir(), Model: "routed-model", ToolPolicy: cartridge.ToolReadExec,
		MaxTurns: 3, Timeout: 20 * time.Second, Env: env,
	}
}

func runHermes(t *testing.T, adapter *Adapter, spec cartridge.RunSpec) ([]cartridge.SessionEvent, cartridge.Result, error) {
	t.Helper()
	session, err := adapter.Run(context.Background(), spec)
	require.NoError(t, err)
	var events []cartridge.SessionEvent
	result, err := cartridge.Collect(session, func(event cartridge.SessionEvent) { events = append(events, event) })
	return events, result, err
}

func TestDetect(t *testing.T) {
	shim := hermesShim(t)
	adapter := hermesAdapter(t, config.Cartridge{})
	status, err := adapter.Detect(context.Background())
	require.NoError(t, err)
	assert.True(t, status.Installed)
	assert.Equal(t, shim.Path, status.Path)
	assert.Equal(t, VerifiedVersion, status.Version)
	assert.Equal(t, cartridge.AuthUnknown, status.Auth)
	assert.True(t, status.Ready())
	assert.Equal(t, [][]string{{"--version"}}, shim.ArgLists(t))
}

func TestDetectTooOldAndMissing(t *testing.T) {
	hermesShim(t)
	adapter := hermesAdapter(t, config.Cartridge{Env: map[string]string{"SC_HERMES_VERSION": "Hermes Agent v0.20.9"}})
	status, err := adapter.Detect(context.Background())
	require.Error(t, err)
	assert.Equal(t, cartridge.KindNotInstalled, cartridge.KindOf(err))
	assert.Equal(t, "0.20.9", status.Version)

	t.Setenv("PATH", t.TempDir())
	status, err = adapter.Detect(context.Background())
	require.Error(t, err)
	assert.False(t, status.Installed)
	assert.Equal(t, cartridge.KindNotInstalled, cartridge.KindOf(err))
}

func TestRunSuccessArgsAndEvents(t *testing.T) {
	shim := hermesShim(t)
	adapter := hermesAdapter(t, config.Cartridge{Args: []string{"--reasoning", "high"}})
	spec := hermesSpec(t, map[string]string{"SC_HERMES_FIXTURE": hermesFixture(t, "success.jsonl")})
	spec.Web = true
	events, result, err := runHermes(t, adapter, spec)
	require.NoError(t, err)
	assert.Equal(t, "All tests pass.", result.Output)
	assert.Equal(t, "session-123", result.SessionID)
	assert.Equal(t, 0, result.ExitCode)
	assert.Greater(t, result.Duration, time.Duration(0))
	assert.Equal(t, cartridge.Usage{InputTokens: 120, OutputTokens: 30, CacheReadTokens: 80, CacheWriteTokens: 12}, result.Usage)
	assert.Equal(t, []cartridge.EventKind{
		cartridge.EventTextDelta, cartridge.EventToolUse, cartridge.EventToolResult,
		cartridge.EventTextDelta, cartridge.EventDone,
	}, eventKinds(events))
	for _, event := range events {
		assert.False(t, event.At.IsZero())
	}

	calls := shim.Invocations(t)
	require.Len(t, calls, 1)
	assert.Equal(t, testutil.RealPath(t, spec.Workdir), calls[0].Dir)
	assert.Equal(t, Stdin(spec), calls[0].Stdin)
	assert.Equal(t, []string{
		"chat", "--query-file", "-", "--oneshot", "--format", "stream-json",
		"--ignore-rules", "--source", "tool", "--in", spec.Workdir,
		"--provider", "nous", "--model", "routed-model", "--max-turns", "3",
		"--toolsets", "terminal,web", "--reasoning", "high",
	}, calls[0].Args)
}

func envDump(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	values := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	return values
}

func TestRunCleanEnvironmentAndHomeCleanup(t *testing.T) {
	hermesShim(t)
	parentHome := t.TempDir()
	parentHermes := filepath.Join(parentHome, ".hermes")
	require.NoError(t, os.Mkdir(parentHermes, 0o700))
	t.Setenv("HOME", parentHome)
	t.Setenv("HERMES_HOME", parentHermes)
	t.Setenv("HERMES_SHARED_AUTH_DIR", "")
	t.Setenv("HERMES_PROFILE", "parent-profile")
	t.Setenv("NOUS_API_KEY", "nous-test")
	t.Setenv("SC_HERMES_LEAK", "must-not-leak")
	dump := filepath.Join(t.TempDir(), "env")
	adapter := hermesAdapter(t, config.Cartridge{Env: map[string]string{"FROM_CFG": "yes"}})
	spec := hermesSpec(t, map[string]string{
		"SC_HERMES_FIXTURE":  hermesFixture(t, "success.jsonl"),
		"SC_HERMES_ENV_DUMP": dump, "SC_HERMES_STATE_WRITE": "1", "FROM_SPEC": "yes",
	})
	_, _, err := runHermes(t, adapter, spec)
	require.NoError(t, err)
	env := envDump(t, dump)
	assert.Equal(t, "yes", env["FROM_CFG"])
	assert.Equal(t, "yes", env["FROM_SPEC"])
	assert.Equal(t, "nous-test", env["NOUS_API_KEY"])
	assert.NotContains(t, env, "SC_HERMES_LEAK")
	assert.NotContains(t, env, "HERMES_PROFILE")
	assert.NotEqual(t, parentHome, env["HOME"])
	assert.NotEqual(t, parentHermes, env["HERMES_HOME"])
	assert.Equal(t, filepath.Join(parentHermes, "shared"), env["HERMES_SHARED_AUTH_DIR"])
	assert.Equal(t, filepath.Dir(env["HERMES_HOME"]), filepath.Dir(env["HOME"]))
	_, err = os.Stat(filepath.Dir(env["HERMES_HOME"]))
	assert.ErrorIs(t, err, os.ErrNotExist, "the whole per-firing root is removed after exit")
	_, err = os.Stat(parentHermes)
	assert.NoError(t, err, "the original Hermes root is untouched")
}

func TestRunRootAuthFallbackUsesLinkedStoreAndLock(t *testing.T) {
	hermesShim(t)
	parentHermes := filepath.Join(t.TempDir(), ".hermes")
	require.NoError(t, os.Mkdir(parentHermes, 0o700))
	rootAuth := filepath.Join(parentHermes, "auth.json")
	require.NoError(t, os.WriteFile(rootAuth, []byte("non-secret fixture"), 0o600))
	t.Setenv("HERMES_HOME", parentHermes)
	t.Setenv("HERMES_SHARED_AUTH_DIR", "")
	aliasLog := filepath.Join(t.TempDir(), "alias")
	spec := hermesSpec(t, map[string]string{
		"SC_HERMES_FIXTURE":        hermesFixture(t, "success.jsonl"),
		"SC_HERMES_AUTH_ALIAS_LOG": aliasLog,
	})
	_, _, err := runHermes(t, hermesAdapter(t, config.Cartridge{}), spec)
	require.NoError(t, err)
	data, err := os.ReadFile(aliasLog)
	require.NoError(t, err)
	assert.Equal(t, "linked\n", string(data))
	info, err := os.Lstat(rootAuth)
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular(), "the root store was replaced or removed")
}

func TestRunConcurrentHomesAreIsolatedAndCleaned(t *testing.T) {
	hermesShim(t)
	log := filepath.Join(t.TempDir(), "homes.log")
	prefix := filepath.Join(t.TempDir(), "env")
	adapter := hermesAdapter(t, config.Cartridge{})
	env := map[string]string{
		"SC_HERMES_FIXTURE":  hermesFixture(t, "success.jsonl"),
		"SC_HERMES_HOME_LOG": log, "SC_HERMES_ENV_DUMP_PREFIX": prefix,
		"SC_HERMES_STATE_WRITE": "1", "SC_HERMES_SLEEP": "0.3",
	}
	firstSpec := hermesSpec(t, env)
	secondSpec := hermesSpec(t, env)
	first, err := adapter.Run(context.Background(), firstSpec)
	require.NoError(t, err)
	second, err := adapter.Run(context.Background(), secondSpec)
	require.NoError(t, err)
	firstDone := make(chan error, 1)
	go func() { _, err := cartridge.Collect(first, nil); firstDone <- err }()
	_, err = cartridge.Collect(second, nil)
	require.NoError(t, err)
	require.NoError(t, <-firstDone)

	data, err := os.ReadFile(log)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.Len(t, lines, 2)
	firstHome, firstOSHome, ok := strings.Cut(lines[0], "|")
	require.True(t, ok)
	secondHome, secondOSHome, ok := strings.Cut(lines[1], "|")
	require.True(t, ok)
	assert.NotEqual(t, firstHome, secondHome)
	assert.NotEqual(t, firstOSHome, secondOSHome)
	assert.NotEqual(t, firstHome, firstOSHome)
	assert.NotEqual(t, secondHome, secondOSHome)
	for _, home := range []string{firstHome, secondHome, firstOSHome, secondOSHome} {
		_, err := os.Stat(home)
		assert.ErrorIs(t, err, os.ErrNotExist, home)
	}
}

func TestRunErrorResults(t *testing.T) {
	cases := []struct {
		fixture string
		kind    cartridge.Kind
		input   int
	}{
		{"auth-error.jsonl", cartridge.KindAuth, 7},
		{"rate-limit-error.jsonl", cartridge.KindRateLimited, 19},
		{"credits-error.jsonl", cartridge.KindCreditsExhausted, 11},
		{"max-turns-error.jsonl", cartridge.KindCrash, 40},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			hermesShim(t)
			adapter := hermesAdapter(t, config.Cartridge{})
			spec := hermesSpec(t, map[string]string{"SC_HERMES_FIXTURE": hermesFixture(t, tc.fixture), "SC_HERMES_EXIT": "1"})
			events, result, err := runHermes(t, adapter, spec)
			require.Error(t, err)
			assert.Equal(t, tc.kind, cartridge.KindOf(err))
			assert.Equal(t, 1, result.ExitCode)
			assert.Equal(t, tc.input, result.Usage.InputTokens)
			assert.Equal(t, cartridge.EventError, events[len(events)-1].Kind)
		})
	}
}

func TestRunFailuresWithoutResult(t *testing.T) {
	t.Run("stderr classification", func(t *testing.T) {
		hermesShim(t)
		adapter := hermesAdapter(t, config.Cartridge{})
		spec := hermesSpec(t, map[string]string{"SC_HERMES_STDERR": "401 Unauthorized", "SC_HERMES_EXIT": "1"})
		_, result, err := runHermes(t, adapter, spec)
		require.Error(t, err)
		assert.Equal(t, cartridge.KindAuth, cartridge.KindOf(err))
		assert.Equal(t, 1, result.ExitCode)
	})
	t.Run("zero exit violates protocol", func(t *testing.T) {
		hermesShim(t)
		adapter := hermesAdapter(t, config.Cartridge{})
		_, _, err := runHermes(t, adapter, hermesSpec(t, nil))
		require.Error(t, err)
		assert.Equal(t, cartridge.KindCrash, cartridge.KindOf(err))
		assert.Contains(t, err.Error(), "without a result line")
	})
}

func TestRunTimeoutAndKillCleanup(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind cartridge.Kind
		kill bool
	}{
		{"timeout", cartridge.KindTimeout, false},
		{"kill", cartridge.KindKilled, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hermesShim(t)
			adapter := hermesAdapter(t, config.Cartridge{})
			dump := filepath.Join(t.TempDir(), "env")
			spec := hermesSpec(t, map[string]string{"SC_HERMES_SLEEP": "10", "SC_HERMES_ENV_DUMP": dump})
			if !tc.kill {
				spec.Timeout = 300 * time.Millisecond
			}
			session, err := adapter.Run(context.Background(), spec)
			require.NoError(t, err)
			if tc.kill {
				time.AfterFunc(100*time.Millisecond, session.Kill)
			}
			result, err := cartridge.Collect(session, nil)
			require.Error(t, err)
			assert.Equal(t, tc.kind, cartridge.KindOf(err))
			assert.Equal(t, -1, result.ExitCode)
			env := envDump(t, dump)
			_, statErr := os.Stat(filepath.Dir(env["HERMES_HOME"]))
			assert.ErrorIs(t, statErr, os.ErrNotExist)
		})
	}
}

func TestRunRefusesReadOnlyAndInvalidSpec(t *testing.T) {
	shim := hermesShim(t)
	adapter := hermesAdapter(t, config.Cartridge{})
	spec := hermesSpec(t, nil)
	spec.ToolPolicy = cartridge.ToolReadOnly
	_, err := adapter.Run(context.Background(), spec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot enforce read_only")
	spec = hermesSpec(t, nil)
	spec.Prompt = " "
	_, err = adapter.Run(context.Background(), spec)
	require.Error(t, err)
	assert.Empty(t, shim.Invocations(t))
}

func TestRunNotInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	adapter := hermesAdapter(t, config.Cartridge{})
	_, err := adapter.Run(context.Background(), hermesSpec(t, nil))
	require.Error(t, err)
	assert.True(t, errors.Is(err, cartridge.ErrNotInstalled))
}
