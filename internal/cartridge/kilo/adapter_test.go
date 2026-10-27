package kilo

import (
	"context"
	"encoding/json"
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

const kiloShimScript = `
if [ "$1" = "--version" ]; then
  if [ -n "$SC_KILO_VERSION" ]; then echo "$SC_KILO_VERSION"; else echo "7.5.6"; fi
  exit "${SC_KILO_VERSION_EXIT:-0}"
fi
if [ "$1" = "profile" ]; then
  if [ -n "$SC_KILO_PROFILE_STDERR" ]; then echo "$SC_KILO_PROFILE_STDERR" >&2; fi
  if [ -n "$SC_KILO_PROFILE_FIXTURE" ]; then cat "$SC_KILO_PROFILE_FIXTURE"; fi
  exit "${SC_KILO_PROFILE_EXIT:-0}"
fi
if [ -n "$SC_KILO_ENV_DUMP" ]; then env | sort > "$SC_KILO_ENV_DUMP"; fi
if [ -n "$SC_KILO_CONFIG_DUMP" ]; then printf '%s' "$KILO_CONFIG_CONTENT" > "$SC_KILO_CONFIG_DUMP"; fi
if [ -n "$SC_KILO_STDERR" ]; then echo "$SC_KILO_STDERR" >&2; fi
if [ -n "$SC_KILO_FIXTURE" ]; then cat "$SC_KILO_FIXTURE"; fi
if [ -n "$SC_KILO_SLEEP" ]; then sleep "$SC_KILO_SLEEP" & wait; fi
exit "${SC_KILO_EXIT:-0}"
`

func kiloShim(t *testing.T) *testutil.Shim {
	t.Helper()
	return testutil.FakeBin(t, Name, kiloShimScript, testutil.WithStdinCapture())
}

func kiloAdapter(t *testing.T, cfg config.Cartridge) *Adapter {
	t.Helper()
	created, err := New("", cfg)
	require.NoError(t, err)
	adapter, ok := created.(*Adapter)
	require.True(t, ok)
	return adapter
}

func kiloSpec(t *testing.T, env map[string]string) cartridge.RunSpec {
	t.Helper()
	return cartridge.RunSpec{
		SystemPrompt: "Follow the test contract.", Prompt: "Reply with exactly PONG",
		Workdir: t.TempDir(), Model: "deepseek/deepseek-v4.1-flash-1017",
		ToolPolicy: cartridge.ToolReadExec, MaxTurns: 4, Timeout: 20 * time.Second, Env: env,
	}
}

func kiloFixture(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", "kilo", name))
	require.NoError(t, err)
	return path
}

func runKilo(t *testing.T, adapter *Adapter, spec cartridge.RunSpec) ([]cartridge.SessionEvent, cartridge.Result, error) {
	t.Helper()
	session, err := adapter.Run(context.Background(), spec)
	require.NoError(t, err)
	var events []cartridge.SessionEvent
	result, err := cartridge.Collect(session, func(event cartridge.SessionEvent) { events = append(events, event) })
	return events, result, err
}

func TestDetectVersionWithoutProfile(t *testing.T) {
	shim := kiloShim(t)
	adapter := kiloAdapter(t, config.Cartridge{})
	status, err := adapter.Detect(context.Background())
	require.NoError(t, err)
	assert.True(t, status.Installed)
	assert.Equal(t, shim.Path, status.Path)
	assert.Equal(t, VerifiedVersion, status.Version)
	assert.Equal(t, cartridge.AuthUnknown, status.Auth)
	assert.Equal(t, [][]string{{"--version"}}, shim.ArgLists(t))
}

func TestDetectTooOldAndMissing(t *testing.T) {
	kiloShim(t)
	adapter := kiloAdapter(t, config.Cartridge{Env: map[string]string{"SC_KILO_VERSION": "7.5.5"}})
	status, err := adapter.Detect(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, cartridge.ErrNotInstalled)
	assert.Equal(t, "7.5.5", status.Version)

	t.Setenv("PATH", t.TempDir())
	status, err = adapter.Detect(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, cartridge.ErrNotInstalled)
	assert.False(t, status.Installed)
}

func TestRunArgsEnvironmentAndEvents(t *testing.T) {
	shim := kiloShim(t)
	t.Setenv("KILO_NO_DAEMON", "0")
	t.Setenv("KILO_CONFIG_CONTENT", "parent config must not survive")
	t.Setenv("KILO_TEST_ENDPOINT", "inherited")
	configDump := filepath.Join(t.TempDir(), "config.json")
	envDump := filepath.Join(t.TempDir(), "env")
	spec := kiloSpec(t, map[string]string{
		"SC_KILO_FIXTURE": kiloFixture(t, "success.jsonl"), "SC_KILO_CONFIG_DUMP": configDump,
		"SC_KILO_ENV_DUMP": envDump, "KILO_DISABLE_PROJECT_CONFIG": "0",
	})
	adapter := kiloAdapter(t, config.Cartridge{
		Model: "configured-model",
		Args:  []string{"--variant", "high", "--format", "default", "--agent", "other", "--dir", "/tmp", "--model", "wrong", "--no-pure", "--no-auto"},
		Env:   map[string]string{"KILO_DISABLE_AUTOUPDATE": "0", "FROM_CONFIG": "yes"},
	})
	events, result, err := runKilo(t, adapter, spec)
	require.NoError(t, err)
	assert.Equal(t, "Final answer.", result.Output)
	assert.Equal(t, "session-kilo-123", result.SessionID)
	assert.Equal(t, cartridge.Usage{
		InputTokens: 180, OutputTokens: 50, CacheReadTokens: 60, CacheWriteTokens: 12,
		CostUSD: 0.03, HasCost: true, Turns: 2,
	}, result.Usage)
	assert.Equal(t, []cartridge.EventKind{
		cartridge.EventTextDelta, cartridge.EventToolUse, cartridge.EventToolResult,
		cartridge.EventTextDelta, cartridge.EventDone,
	}, kiloEventKinds(events))
	assert.Equal(t, "git status --short", events[1].Input)
	assert.Equal(t, "call-1", events[2].ToolID)
	assert.False(t, events[2].IsError)

	calls := shim.Invocations(t)
	require.Len(t, calls, 1)
	assert.Equal(t, Stdin(spec), calls[0].Stdin)
	assert.Equal(t, testutil.RealPath(t, spec.Workdir), calls[0].Dir)
	assert.Equal(t, "run", calls[0].Args[0])
	assert.Equal(t, "json", lastArgValue(calls[0].Args, "--format"))
	assert.Equal(t, "sc", lastArgValue(calls[0].Args, "--agent"))
	assert.Equal(t, spec.Workdir, lastArgValue(calls[0].Args, "--dir"))
	assert.Equal(t, "kilo/"+spec.Model, lastArgValue(calls[0].Args, "--model"))
	assert.Equal(t, dandelionModelID, spec.Model, "argument normalization must not mutate the routed model")
	assert.Equal(t, "high", lastArgValue(calls[0].Args, "--variant"))
	assert.Equal(t, "--pure", calls[0].Args[len(calls[0].Args)-10])
	assert.Contains(t, calls[0].Args, "--auto")
	assert.Less(t, indexOfArg(calls[0].Args, "--no-auto"), indexOfArg(calls[0].Args, "--auto"))

	data, err := os.ReadFile(configDump)
	require.NoError(t, err)
	var generated generatedConfig
	require.NoError(t, json.Unmarshal(data, &generated))
	agent := generated.Agent["sc"]
	assert.Equal(t, "primary", agent.Mode)
	assert.Equal(t, 4, agent.Steps)
	assert.Equal(t, "allow", agent.Permission["bash"])
	assert.Equal(t, "deny", agent.Permission["edit"])
	assert.Equal(t, "deny", agent.Permission["task"])
	assert.Equal(t, "deny", agent.Permission["webfetch"])
	assert.Equal(t, "deny", agent.Permission["websearch"])

	environment := kiloEnvDump(t, envDump)
	assert.Equal(t, "1", environment["KILO_NO_DAEMON"])
	assert.Equal(t, "1", environment["KILO_DISABLE_PROJECT_CONFIG"])
	assert.Equal(t, "1", environment["KILO_DISABLE_AUTOUPDATE"])
	assert.Equal(t, string(data), environment[configContentEnv])
	assert.Equal(t, "inherited", environment["KILO_TEST_ENDPOINT"])
	assert.Equal(t, "yes", environment["FROM_CONFIG"])
}

func TestArgsModelNormalization(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured string
		routed     string
		want       string
	}{
		{"Dandelion gateway ID", "configured-custom", dandelionModelID, "kilo/" + dandelionModelID},
		{"already qualified gateway ID", "", "kilo/" + dandelionModelID, "kilo/" + dandelionModelID},
		{"direct custom ID", "", "custom-provider/custom-model", "custom-provider/custom-model"},
		{"configured custom ID", "custom-provider/configured-model", "", "custom-provider/configured-model"},
		{"configured Dandelion ID", dandelionModelID, "", "kilo/" + dandelionModelID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := kiloAdapter(t, config.Cartridge{Model: tc.configured})
			spec := kiloSpec(t, nil)
			spec.Model = tc.routed
			args := adapter.Args(spec)
			assert.Equal(t, tc.want, lastArgValue(args, "--model"))
			assert.Equal(t, tc.routed, spec.Model)
		})
	}
}

func TestConfigReadWriteAndRequestedWeb(t *testing.T) {
	spec := kiloSpec(t, nil)
	spec.ToolPolicy = cartridge.ToolReadWrite
	spec.Web = true
	content, err := ConfigContent(spec)
	require.NoError(t, err)
	var generated generatedConfig
	require.NoError(t, json.Unmarshal([]byte(content), &generated))
	permission := generated.Agent["sc"].Permission
	assert.Equal(t, "allow", permission["edit"])
	assert.Equal(t, "allow", permission["webfetch"])
	assert.Equal(t, "allow", permission["websearch"])
	assert.Equal(t, "deny", permission["task"])
}

func TestToolPolicyEnforcementAndReadOnlyRefusal(t *testing.T) {
	shim := kiloShim(t)
	adapter := kiloAdapter(t, config.Cartridge{})
	for _, policy := range []cartridge.ToolPolicy{cartridge.ToolReadExec, cartridge.ToolReadWrite} {
		enforcement, err := adapter.ToolPolicyEnforcement(policy)
		require.NoError(t, err)
		assert.Equal(t, agents.EnforceNative, enforcement.Mode)
		assert.NoError(t, enforcement.Check())
	}
	spec := kiloSpec(t, map[string]string{minBalanceEnv: "10"})
	spec.ToolPolicy = cartridge.ToolReadOnly
	_, err := adapter.Run(context.Background(), spec)
	assert.ErrorIs(t, err, ErrReadOnlyUnavailable)
	assert.Empty(t, shim.Invocations(t), "read_only is refused before even a profile probe")
}

func TestRunLargePromptUsesOnlyStdin(t *testing.T) {
	shim := kiloShim(t)
	spec := kiloSpec(t, map[string]string{"SC_KILO_FIXTURE": kiloFixture(t, "success.jsonl")})
	spec.Prompt = strings.Repeat("complete large prompt with ${literal} and unicode ✓\n", 10000)
	require.Greater(t, len(Stdin(spec)), 256*1024)
	_, _, err := runKilo(t, kiloAdapter(t, config.Cartridge{}), spec)
	require.NoError(t, err)
	calls := shim.Invocations(t)
	require.Len(t, calls, 1)
	assert.Equal(t, Stdin(spec), calls[0].Stdin)
	for _, arg := range calls[0].Args {
		assert.NotContains(t, arg, "complete large prompt")
	}
}

func TestRunStructuredErrorsAndStderr(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture string
		stderr  string
		exit    string
		want    error
	}{
		{"auth", "auth-error.jsonl", "", "0", cartridge.ErrAuth},
		{"credits", "credits-error.jsonl", "", "0", cartridge.ErrCreditsExhausted},
		{"rate", "rate-error.jsonl", "", "1", cartridge.ErrRateLimited},
		{"stderr auth", "", "Not authenticated with Kilo Gateway", "1", cartridge.ErrAuth},
		{"stderr credits", "", "HTTP 429 insufficient credit", "1", cartridge.ErrCreditsExhausted},
		{"crash", "", "configuration is invalid", "1", cartridge.ErrCrash},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kiloShim(t)
			env := map[string]string{"SC_KILO_EXIT": tc.exit}
			if tc.fixture != "" {
				env["SC_KILO_FIXTURE"] = kiloFixture(t, tc.fixture)
			}
			if tc.stderr != "" {
				env["SC_KILO_STDERR"] = tc.stderr
			}
			_, _, err := runKilo(t, kiloAdapter(t, config.Cartridge{}), kiloSpec(t, env))
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.want)
		})
	}
}

func TestRunToolErrorAndMalformedStep(t *testing.T) {
	t.Run("tool error", func(t *testing.T) {
		kiloShim(t)
		spec := kiloSpec(t, map[string]string{"SC_KILO_FIXTURE": kiloFixture(t, "tool-error.jsonl")})
		events, _, err := runKilo(t, kiloAdapter(t, config.Cartridge{}), spec)
		require.NoError(t, err)
		require.Len(t, events, 4)
		assert.True(t, events[1].IsError)
		assert.Equal(t, "file not found", events[1].Output)
	})
	t.Run("missing usage", func(t *testing.T) {
		kiloShim(t)
		_, _, err := runKilo(t, kiloAdapter(t, config.Cartridge{}), kiloSpec(t, nil))
		require.Error(t, err)
		assert.ErrorIs(t, err, cartridge.ErrCrash)
		assert.Contains(t, err.Error(), "without a valid step_finish")
	})
	t.Run("malformed usage", func(t *testing.T) {
		kiloShim(t)
		spec := kiloSpec(t, map[string]string{"SC_KILO_FIXTURE": kiloFixture(t, "malformed-step.jsonl")})
		_, _, err := runKilo(t, kiloAdapter(t, config.Cartridge{}), spec)
		require.Error(t, err)
		assert.ErrorIs(t, err, cartridge.ErrCrash)
		assert.Contains(t, err.Error(), "malformed step_finish")
	})
}

func TestBalanceDisabledForDirectUse(t *testing.T) {
	shim := kiloShim(t)
	spec := kiloSpec(t, map[string]string{"SC_KILO_FIXTURE": kiloFixture(t, "success.jsonl")})
	_, _, err := runKilo(t, kiloAdapter(t, config.Cartridge{}), spec)
	require.NoError(t, err)
	require.Len(t, shim.Invocations(t), 1)
	assert.Equal(t, "run", shim.Invocations(t)[0].Args[0])
}

func TestBalanceFreshBeforeEveryLaunchAndControlNotInherited(t *testing.T) {
	shim := kiloShim(t)
	envDump := filepath.Join(t.TempDir(), "env")
	spec := kiloSpec(t, map[string]string{
		minBalanceEnv: "10", "SC_KILO_PROFILE_FIXTURE": kiloFixture(t, "profile.json"),
		"SC_KILO_FIXTURE": kiloFixture(t, "success.jsonl"), "SC_KILO_ENV_DUMP": envDump,
	})
	adapter := kiloAdapter(t, config.Cartridge{})
	for range 2 {
		_, _, err := runKilo(t, adapter, spec)
		require.NoError(t, err)
	}
	calls := shim.Invocations(t)
	require.Len(t, calls, 4)
	for index, want := range []string{"profile", "run", "profile", "run"} {
		assert.Equal(t, want, calls[index].Args[0])
	}
	assert.Equal(t, []string{"profile", "--json"}, calls[0].Args)
	assert.NotContains(t, kiloEnvDump(t, envDump), minBalanceEnv)
}

func TestBalanceFloorAndProfileFailuresFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name        string
		floor       string
		fixture     string
		profileExit string
		stderr      string
	}{
		{"equal to floor", "10", "profile-floor.json", "0", ""},
		{"profile failure", "10", "", "1", "Email: must-not-leak@example.invalid"},
		{"missing balance", "10", "success.jsonl", "0", ""},
		{"invalid floor", "not-a-number", "profile.json", "0", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shim := kiloShim(t)
			spec := kiloSpec(t, map[string]string{
				minBalanceEnv: tc.floor, "SC_KILO_PROFILE_EXIT": tc.profileExit,
				"SC_KILO_PROFILE_STDERR": tc.stderr, "SC_KILO_FIXTURE": kiloFixture(t, "success.jsonl"),
			})
			if tc.fixture != "" {
				spec.Env["SC_KILO_PROFILE_FIXTURE"] = kiloFixture(t, tc.fixture)
			}
			_, err := kiloAdapter(t, config.Cartridge{}).Run(context.Background(), spec)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrBudget)
			assert.ErrorIs(t, err, cartridge.ErrCreditsExhausted)
			assert.NotContains(t, err.Error(), "must-not-leak")
			assert.NotContains(t, err.Error(), "fixture-person")
			for _, call := range shim.Invocations(t) {
				assert.NotEqual(t, "run", call.Args[0], "a failed floor check must not launch Kilo")
			}
		})
	}
}

func TestMessageCostPreferredAndAllowanceGuard(t *testing.T) {
	t.Run("cumulative message cost in result", func(t *testing.T) {
		kiloShim(t)
		spec := kiloSpec(t, map[string]string{"SC_KILO_FIXTURE": kiloFixture(t, "message-cost.jsonl")})
		_, result, err := runKilo(t, kiloAdapter(t, config.Cartridge{}), spec)
		require.NoError(t, err)
		assert.Equal(t, 0.6, result.Usage.CostUSD)
		assert.Equal(t, 10, result.Usage.InputTokens)
	})
	t.Run("guard kills at allowance", func(t *testing.T) {
		kiloShim(t)
		spec := kiloSpec(t, map[string]string{
			minBalanceEnv: "10", "SC_KILO_PROFILE_FIXTURE": kiloFixture(t, "profile-cost-guard.json"),
			"SC_KILO_FIXTURE": kiloFixture(t, "message-cost.jsonl"), "SC_KILO_SLEEP": "10",
		})
		_, result, err := runKilo(t, kiloAdapter(t, config.Cartridge{}), spec)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrBudget)
		assert.ErrorIs(t, err, cartridge.ErrCreditsExhausted)
		assert.Contains(t, err.Error(), "allowance exhausted")
		assert.Equal(t, 0.6, result.Usage.CostUSD)
	})
	t.Run("guard uses summed root step cost", func(t *testing.T) {
		kiloShim(t)
		spec := kiloSpec(t, map[string]string{
			minBalanceEnv: "10", "SC_KILO_PROFILE_FIXTURE": kiloFixture(t, "profile-step-guard.json"),
			"SC_KILO_FIXTURE": kiloFixture(t, "step-cost-guard.jsonl"), "SC_KILO_SLEEP": "10",
		})
		_, result, err := runKilo(t, kiloAdapter(t, config.Cartridge{}), spec)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrBudget)
		assert.ErrorIs(t, err, cartridge.ErrCreditsExhausted)
		assert.Equal(t, 0.01, result.Usage.CostUSD)
		assert.Equal(t, 1, result.Usage.Turns)
	})
}

func TestRunTimeoutAndKill(t *testing.T) {
	for _, tc := range []struct {
		name string
		kill bool
		want error
	}{
		{"timeout", false, cartridge.ErrTimeout},
		{"kill", true, cartridge.ErrKilled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kiloShim(t)
			spec := kiloSpec(t, map[string]string{"SC_KILO_SLEEP": "10"})
			if !tc.kill {
				spec.Timeout = 250 * time.Millisecond
			}
			session, err := kiloAdapter(t, config.Cartridge{}).Run(context.Background(), spec)
			require.NoError(t, err)
			if tc.kill {
				time.AfterFunc(100*time.Millisecond, session.Kill)
			}
			result, err := cartridge.Collect(session, nil)
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.want)
			assert.Equal(t, -1, result.ExitCode)
		})
	}
}

func TestRunNotInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := kiloAdapter(t, config.Cartridge{}).Run(context.Background(), kiloSpec(t, nil))
	assert.True(t, errors.Is(err, cartridge.ErrNotInstalled))
}

func TestKindForStatus(t *testing.T) {
	assert.Equal(t, cartridge.KindAuth, KindForStatus(401))
	assert.Equal(t, cartridge.KindCreditsExhausted, KindForStatus(402))
	assert.Equal(t, cartridge.KindRateLimited, KindForStatus(429))
	assert.Equal(t, cartridge.KindCrash, KindForStatus(400))
	assert.Equal(t, cartridge.KindUnknown, KindForStatus(0))
}

func lastArgValue(args []string, flag string) string {
	for index := len(args) - 1; index >= 0; index-- {
		if args[index] == flag && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}

func indexOfArg(args []string, want string) int {
	for index, arg := range args {
		if arg == want {
			return index
		}
	}
	return -1
}

func kiloEventKinds(events []cartridge.SessionEvent) []cartridge.EventKind {
	kinds := make([]cartridge.EventKind, len(events))
	for index, event := range events {
		kinds[index] = event.Kind
	}
	return kinds
}

func kiloEnvDump(t *testing.T, path string) map[string]string {
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
