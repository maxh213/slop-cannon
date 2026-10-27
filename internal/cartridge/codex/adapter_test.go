package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
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

const codexShimScript = `
case "$1" in
  --version)
    echo "${SC_CODEX_VERSION:-codex-cli 0.164.0}"
    exit 0;;
  login)
    if [ -n "$SC_CODEX_STATUS_FILE" ]; then cat "$SC_CODEX_STATUS_FILE"; fi
    exit "${SC_CODEX_STATUS_EXIT:-0}";;
esac
if [ -n "$SC_CODEX_ENV_DUMP" ]; then env | sort > "$SC_CODEX_ENV_DUMP"; fi
if [ -n "$SC_CODEX_REAP_FILE" ]; then (sleep 0.4; echo leaked > "$SC_CODEX_REAP_FILE") & fi
if [ -n "$SC_CODEX_SLEEP" ]; then sleep "$SC_CODEX_SLEEP"; fi
if [ -n "$SC_CODEX_STDERR_FILE" ]; then cat "$SC_CODEX_STDERR_FILE" >&2; fi
if [ -n "$SC_CODEX_STDERR" ]; then echo "$SC_CODEX_STDERR" >&2; fi
if [ -n "$SC_CODEX_FIXTURE" ]; then cat "$SC_CODEX_FIXTURE"; fi
exit "${SC_CODEX_EXIT:-0}"
`

func codexShim(t *testing.T) *testutil.Shim {
	t.Helper()
	return testutil.FakeBin(t, Name, codexShimScript, testutil.WithStdinCapture())
}

func codexAdapter(t *testing.T, name string, cfg config.Cartridge) *Adapter {
	t.Helper()
	c, err := New(name, cfg)
	require.NoError(t, err)
	a, ok := c.(*Adapter)
	require.True(t, ok)
	return a
}

func codexSpec(t *testing.T, env map[string]string) cartridge.RunSpec {
	t.Helper()
	return cartridge.RunSpec{
		Prompt: "Reply with exactly IMPLEMENTED", SystemPrompt: "Follow the test contract.",
		Workdir: t.TempDir(), Model: "gpt-routed", ToolPolicy: cartridge.ToolReadWrite,
		Timeout: 20 * time.Second, Env: env,
	}
}

func runCodex(t *testing.T, a *Adapter, spec cartridge.RunSpec) ([]cartridge.SessionEvent, cartridge.Result, error) {
	t.Helper()
	session, err := a.Run(context.Background(), spec)
	require.NoError(t, err)
	var events []cartridge.SessionEvent
	result, err := cartridge.Collect(session, func(event cartridge.SessionEvent) {
		events = append(events, event)
	})
	return events, result, err
}

func TestRegistryAndFactoryAccountNames(t *testing.T) {
	assert.True(t, cartridge.Default.Has(Name))
	assert.False(t, cartridge.Default.Has(AnimusName), "codex-animus is only a Dandelion delegated account")
	_, err := cartridge.New(AnimusName, config.Cartridge{})
	require.Error(t, err)

	for _, name := range []string{Name, AnimusName, "dandelion"} {
		a := codexAdapter(t, name, config.Cartridge{})
		assert.Equal(t, name, a.Name())
		assert.Equal(t, "codex", a.Binary())
	}
}

func TestDetect_LoginStatus(t *testing.T) {
	shim := codexShim(t)
	a := codexAdapter(t, Name, config.Cartridge{
		Args: []string{"-c", "model_reasoning_effort=high"},
		Env:  map[string]string{"SC_CODEX_STATUS_FILE": codexFixture(t, "login-status.txt")},
	})
	status, err := a.Detect(context.Background())
	require.NoError(t, err)
	assert.True(t, status.Installed)
	assert.Equal(t, shim.Path, status.Path)
	assert.Equal(t, "0.164.0", status.Version)
	assert.Equal(t, cartridge.AuthOK, status.Auth)
	assert.Equal(t, "API key configured", status.Detail)
	assert.Equal(t, [][]string{{"--version"}, {"login", "status"}}, shim.ArgLists(t))
}

func TestDetect_LoggedOutEvenWhenProbeExitsNonzero(t *testing.T) {
	codexShim(t)
	a := codexAdapter(t, Name, config.Cartridge{Env: map[string]string{
		"SC_CODEX_STATUS_FILE": codexFixture(t, "logged-out.txt"),
		"SC_CODEX_STATUS_EXIT": "1",
	}})
	status, err := a.Detect(context.Background())
	require.Error(t, err)
	assert.True(t, errors.Is(err, cartridge.ErrAuth))
	assert.Equal(t, cartridge.AuthMissing, status.Auth)
	assert.False(t, status.Ready())
}

func TestDetect_AuthProbeUnknown(t *testing.T) {
	codexShim(t)
	a := codexAdapter(t, Name, config.Cartridge{Env: map[string]string{"SC_CODEX_STATUS_EXIT": "2"}})
	status, err := a.Detect(context.Background())
	require.NoError(t, err)
	assert.Equal(t, cartridge.AuthUnknown, status.Auth)
	assert.Contains(t, status.Detail, "auth probe unavailable")
}

func TestDetect_Missing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	a := codexAdapter(t, Name, config.Cartridge{})
	status, err := a.Detect(context.Background())
	require.Error(t, err)
	assert.Equal(t, cartridge.KindNotInstalled, cartridge.KindOf(err))
	assert.False(t, status.Installed)
}

func TestArgs_RoutedModelEffortAndPolicies(t *testing.T) {
	a := codexAdapter(t, Name, config.Cartridge{
		Model: "gpt-config", Args: []string{"-c", "model_reasoning_effort=xhigh", "--sandbox", "danger-full-access"},
	})
	spec := cartridge.RunSpec{
		Prompt: "not in argv", SystemPrompt: "line one\nline two", Workdir: "/work",
		Model: "gpt-routed", ToolPolicy: cartridge.ToolReadWrite, Web: true,
	}
	assert.Equal(t, []string{
		"exec", "--json", "--skip-git-repo-check", "-C", "/work",
		"-c", "model_reasoning_effort=xhigh", "--sandbox", "danger-full-access",
		"--model", "gpt-routed", "-c", `instructions="line one\nline two"`, "-c", webSearchEnabled,
		"-c", approvalPolicyNever, "--sandbox", "workspace-write",
	}, a.Args(spec))
	assert.NotContains(t, strings.Join(a.Args(spec), "\x00"), spec.Prompt)
	assert.NotContains(t, a.Args(spec), "--ask-for-approval")
	assert.NotContains(t, a.Args(spec), "--search")

	spec.SystemPrompt = ""
	spec.Model = ""
	spec.Web = false
	spec.ToolPolicy = cartridge.ToolReadExec
	args := a.Args(spec)
	assert.Contains(t, strings.Join(args, " "), "--model gpt-config")
	assert.Equal(t, []string{"-c", approvalPolicyNever, "--sandbox", "read-only"}, args[len(args)-4:])
	assert.Nil(t, a.Args(cartridge.RunSpec{ToolPolicy: cartridge.ToolReadOnly}))
}

func TestToolPolicyEnforcement(t *testing.T) {
	a := codexAdapter(t, Name, config.Cartridge{})
	for _, policy := range []cartridge.ToolPolicy{cartridge.ToolReadExec, cartridge.ToolReadWrite} {
		enforcement, err := agents.EnforcementFor(a, policy)
		require.NoError(t, err)
		assert.Equal(t, agents.EnforceNative, enforcement.Mode)
		assert.Equal(t, ToolFlags(policy), enforcement.Controls)
		assert.NoError(t, enforcement.Check())
	}
	_, err := agents.EnforcementFor(a, cartridge.ToolReadOnly)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "still permits shell")
	_, err = a.ToolPolicyEnforcement("unknown")
	assert.ErrorIs(t, err, domain.ErrUnknownValue)
}

func TestRun_SuccessAndInvocation(t *testing.T) {
	shim := codexShim(t)
	a := codexAdapter(t, Name, config.Cartridge{Args: []string{"-c", "model_reasoning_effort=high"}})
	spec := codexSpec(t, map[string]string{"SC_CODEX_FIXTURE": codexFixture(t, "success.jsonl")})
	events, result, err := runCodex(t, a, spec)
	require.NoError(t, err)
	assert.Equal(t, "IMPLEMENTED", result.Output)
	assert.Equal(t, "thread-123", result.SessionID)
	assert.Equal(t, 1200, result.Usage.InputTokens)
	assert.Equal(t, 800, result.Usage.CacheReadTokens)
	assert.Equal(t, cartridge.EventDone, events[len(events)-1].Kind)

	calls := shim.Invocations(t)
	require.Len(t, calls, 1)
	assert.Equal(t, spec.Prompt, calls[0].Stdin)
	assert.Equal(t, testutil.RealPath(t, spec.Workdir), calls[0].Dir)
	assert.Equal(t, []string{
		"exec", "--json", "--skip-git-repo-check", "-C", spec.Workdir,
		"-c", "model_reasoning_effort=high", "--model", "gpt-routed",
		"-c", `instructions="Follow the test contract."`,
		"-c", approvalPolicyNever, "--sandbox", "workspace-write",
	}, calls[0].Args)
}

func TestRun_ReadOnlyNeverStarts(t *testing.T) {
	shim := codexShim(t)
	a := codexAdapter(t, Name, config.Cartridge{})
	spec := codexSpec(t, nil)
	spec.ToolPolicy = cartridge.ToolReadOnly
	_, err := a.Run(context.Background(), spec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot enforce read_only")
	assert.Empty(t, shim.Invocations(t))
}

func TestRun_AccountHomeIsolation(t *testing.T) {
	t.Run("personal does not inherit ambient Animus home", func(t *testing.T) {
		codexShim(t)
		t.Setenv("CODEX_HOME", "/accounts/animus")
		dump := filepath.Join(t.TempDir(), "env")
		a := codexAdapter(t, Name, config.Cartridge{})
		spec := codexSpec(t, map[string]string{
			"SC_CODEX_ENV_DUMP": dump, "SC_CODEX_FIXTURE": codexFixture(t, "success.jsonl"),
		})
		_, _, err := runCodex(t, a, spec)
		require.NoError(t, err)
		data, err := os.ReadFile(dump)
		require.NoError(t, err)
		assert.NotContains(t, string(data), "CODEX_HOME=")
	})

	t.Run("personal explicit home is preserved", func(t *testing.T) {
		codexShim(t)
		t.Setenv("CODEX_HOME", "/accounts/animus")
		dump := filepath.Join(t.TempDir(), "env")
		a := codexAdapter(t, Name, config.Cartridge{Env: map[string]string{"CODEX_HOME": "/accounts/personal-config"}})
		spec := codexSpec(t, map[string]string{
			"CODEX_HOME": "/accounts/personal-spec", "SC_CODEX_ENV_DUMP": dump,
			"SC_CODEX_FIXTURE": codexFixture(t, "success.jsonl"),
		})
		_, _, err := runCodex(t, a, spec)
		require.NoError(t, err)
		data, err := os.ReadFile(dump)
		require.NoError(t, err)
		assert.Contains(t, string(data), "CODEX_HOME=/accounts/personal-spec\n")
		assert.NotContains(t, string(data), "/accounts/animus")
	})

	t.Run("Dandelion delegate preserves its explicitly mapped home", func(t *testing.T) {
		codexShim(t)
		t.Setenv(animusHomeEnv, "/accounts/unrelated-fallback")
		dump := filepath.Join(t.TempDir(), "env")
		a := codexAdapter(t, "dandelion", config.Cartridge{})
		spec := codexSpec(t, map[string]string{
			"CODEX_HOME": "/accounts/delegated-animus", "SC_CODEX_ENV_DUMP": dump,
			"SC_CODEX_FIXTURE": codexFixture(t, "success.jsonl"),
		})
		_, _, err := runCodex(t, a, spec)
		require.NoError(t, err)
		data, err := os.ReadFile(dump)
		require.NoError(t, err)
		assert.Contains(t, string(data), "CODEX_HOME=/accounts/delegated-animus\n")
		assert.NotContains(t, string(data), "/accounts/unrelated-fallback")
	})

	t.Run("Animus Dandelion home wins over every other layer", func(t *testing.T) {
		codexShim(t)
		t.Setenv(animusHomeEnv, "/accounts/animus-dandelion")
		dump := filepath.Join(t.TempDir(), "env")
		a := codexAdapter(t, AnimusName, config.Cartridge{Env: map[string]string{"CODEX_HOME": "/accounts/personal-config"}})
		spec := codexSpec(t, map[string]string{
			"CODEX_HOME": "/accounts/personal-spec", "SC_CODEX_ENV_DUMP": dump,
			"SC_CODEX_FIXTURE": codexFixture(t, "success.jsonl"),
		})
		_, _, err := runCodex(t, a, spec)
		require.NoError(t, err)
		data, err := os.ReadFile(dump)
		require.NoError(t, err)
		assert.Contains(t, string(data), "CODEX_HOME=/accounts/animus-dandelion\n")
		assert.NotContains(t, string(data), "/accounts/personal-")
	})

	t.Run("Animus fallback does not leak ambient personal home", func(t *testing.T) {
		codexShim(t)
		t.Setenv(animusHomeEnv, "")
		t.Setenv("CODEX_HOME", "/accounts/personal")
		dump := filepath.Join(t.TempDir(), "env")
		a := codexAdapter(t, AnimusName, config.Cartridge{})
		a.homeDir = func() (string, error) { return "/home/test", nil }
		spec := codexSpec(t, map[string]string{
			"SC_CODEX_ENV_DUMP": dump, "SC_CODEX_FIXTURE": codexFixture(t, "success.jsonl"),
		})
		_, _, err := runCodex(t, a, spec)
		require.NoError(t, err)
		data, err := os.ReadFile(dump)
		require.NoError(t, err)
		assert.Contains(t, string(data), "CODEX_HOME=/home/test/.codex-animus\n")
		assert.NotContains(t, string(data), "CODEX_HOME=/accounts/personal\n")
	})
}

func TestRun_EnvironmentIsClean(t *testing.T) {
	codexShim(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("CODEX_API_KEY", "codex-key")
	t.Setenv("SC_CODEX_SECRET", "must-not-leak")
	dump := filepath.Join(t.TempDir(), "env")
	a := codexAdapter(t, Name, config.Cartridge{})
	spec := codexSpec(t, map[string]string{
		"SC_CODEX_ENV_DUMP": dump, "SC_CODEX_FIXTURE": codexFixture(t, "success.jsonl"),
	})
	_, _, err := runCodex(t, a, spec)
	require.NoError(t, err)
	data, err := os.ReadFile(dump)
	require.NoError(t, err)
	assert.Contains(t, string(data), "OPENAI_API_KEY=test-key\n")
	assert.Contains(t, string(data), "CODEX_API_KEY=codex-key\n")
	assert.NotContains(t, string(data), "SC_CODEX_SECRET")
}

func TestInheritEnvSortedUniqueAndNoAmbientHome(t *testing.T) {
	t.Setenv("CODEX_HOME", "/accounts/animus")
	t.Setenv("CODEX_API_KEY", "key")
	got := InheritEnv([]string{"HOME", "CODEX_API_KEY", "CODEX_HOME"})
	assert.True(t, sort.StringsAreSorted(got))
	assert.Contains(t, got, "CODEX_API_KEY")
	assert.NotContains(t, got, "CODEX_HOME")
	seen := map[string]bool{}
	for _, name := range got {
		assert.False(t, seen[name], "duplicate %s", name)
		seen[name] = true
	}
}

func TestRun_LargePromptUsesStdin(t *testing.T) {
	shim := codexShim(t)
	a := codexAdapter(t, Name, config.Cartridge{})
	spec := codexSpec(t, map[string]string{"SC_CODEX_FIXTURE": codexFixture(t, "success.jsonl")})
	spec.Prompt = strings.Repeat("large λ prompt\n", 70000)
	_, _, err := runCodex(t, a, spec)
	require.NoError(t, err)
	calls := shim.Invocations(t)
	require.Len(t, calls, 1)
	assert.Equal(t, spec.Prompt, calls[0].Stdin)
	assert.NotContains(t, strings.Join(calls[0].Args, "\x00"), "large λ prompt")
}

func TestRun_TypedFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		env    map[string]string
		kind   cartridge.Kind
		needle string
	}{
		{"rate limit", map[string]string{"SC_CODEX_FIXTURE": codexFixture(t, "turn-failed.jsonl")}, cartridge.KindRateLimited, "usage limit"},
		{"top-level auth", map[string]string{"SC_CODEX_FIXTURE": codexFixture(t, "top-level-error.jsonl")}, cartridge.KindAuth, "invalid API key"},
		{"credits", map[string]string{"SC_CODEX_STDERR_FILE": codexFixture(t, "credits.stderr"), "SC_CODEX_EXIT": "1"}, cartridge.KindCreditsExhausted, "insufficient_quota"},
		{"stderr auth", map[string]string{"SC_CODEX_STDERR_FILE": codexFixture(t, "auth.stderr"), "SC_CODEX_EXIT": "1"}, cartridge.KindAuth, "Not logged in"},
		{"missing terminal", nil, cartridge.KindCrash, "without a successful turn.completed"},
		{"malformed terminal", map[string]string{"SC_CODEX_FIXTURE": codexFixture(t, "malformed-terminal.jsonl")}, cartridge.KindCrash, "missing usage"},
	} {
		t.Run(test.name, func(t *testing.T) {
			codexShim(t)
			a := codexAdapter(t, Name, config.Cartridge{})
			_, _, err := runCodex(t, a, codexSpec(t, test.env))
			require.Error(t, err)
			assert.Equal(t, test.kind, cartridge.KindOf(err))
			assert.Contains(t, err.Error(), test.needle)
		})
	}
}

func TestRun_TimeoutKillsAndReaps(t *testing.T) {
	codexShim(t)
	a := codexAdapter(t, Name, config.Cartridge{})
	marker := filepath.Join(t.TempDir(), "descendant-survived")
	spec := codexSpec(t, map[string]string{"SC_CODEX_SLEEP": "10", "SC_CODEX_REAP_FILE": marker})
	spec.Timeout = 100 * time.Millisecond
	start := time.Now()
	_, result, err := runCodex(t, a, spec)
	require.Error(t, err)
	assert.Equal(t, cartridge.KindTimeout, cartridge.KindOf(err))
	assert.Equal(t, -1, result.ExitCode)
	assert.Less(t, time.Since(start), 8*time.Second)
	time.Sleep(500 * time.Millisecond)
	_, statErr := os.Stat(marker)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "a descendant survived the session")
}

func TestRun_Kill(t *testing.T) {
	codexShim(t)
	a := codexAdapter(t, Name, config.Cartridge{})
	session, err := a.Run(context.Background(), codexSpec(t, map[string]string{"SC_CODEX_SLEEP": "10"}))
	require.NoError(t, err)
	time.AfterFunc(100*time.Millisecond, session.Kill)
	_, err = cartridge.Collect(session, nil)
	require.Error(t, err)
	assert.Equal(t, cartridge.KindKilled, cartridge.KindOf(err))
}

func TestKindForTextPrecedence(t *testing.T) {
	assert.Equal(t, cartridge.KindCreditsExhausted, KindForText("billing and rate limit"))
	assert.Equal(t, cartridge.KindAuth, KindForText("401 unauthorized; too many requests"))
	assert.Equal(t, cartridge.KindRateLimited, KindForText("429 too many requests"))
	assert.Equal(t, cartridge.KindUnknown, KindForText("ordinary compiler error"))
}
