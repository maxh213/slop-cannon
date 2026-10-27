package junie

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

const junieShimScript = `
if [ "$1" = "--version" ]; then
  if [ -n "$SC_JUNIE_VERSION_FILE" ]; then cat "$SC_JUNIE_VERSION_FILE"; else echo "Junie CLI 2026.10.23"; fi
  exit 0
fi
if [ -n "$SC_JUNIE_ENV_DUMP" ]; then env | sort > "$SC_JUNIE_ENV_DUMP"; fi
if [ -n "$SC_JUNIE_EXTENSION_DUMP" ]; then
  for arg in "$@"; do
    if [ "$previous" = "--extensions-default-location" ]; then echo "$arg" > "$SC_JUNIE_EXTENSION_DUMP"; fi
    previous="$arg"
  done
fi
if [ -n "$SC_JUNIE_SLEEP" ]; then sleep "$SC_JUNIE_SLEEP"; fi
if [ -n "$SC_JUNIE_STDERR_FILE" ]; then cat "$SC_JUNIE_STDERR_FILE" >&2; fi
if [ -n "$SC_JUNIE_STDOUT_FILE" ]; then cat "$SC_JUNIE_STDOUT_FILE"; fi
exit "${SC_JUNIE_EXIT:-0}"
`

func junieShim(t *testing.T) *testutil.Shim {
	t.Helper()
	return testutil.FakeBin(t, DefaultBinary, junieShimScript)
}

func junieFixture(t testing.TB, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", "junie", name))
	require.NoError(t, err)
	_, err = os.Stat(path)
	require.NoError(t, err)
	return path
}

func junieSpec(t *testing.T, env map[string]string) cartridge.RunSpec {
	t.Helper()
	return cartridge.RunSpec{
		Prompt: "Reply with exactly OK", SystemPrompt: "Be terse",
		Workdir: t.TempDir(), Model: "routed-model", ToolPolicy: cartridge.ToolReadWrite,
		MaxTurns: 4, Timeout: 20 * time.Second, Env: env,
	}
}

func runJunie(t *testing.T, a *Adapter, spec cartridge.RunSpec) ([]cartridge.SessionEvent, cartridge.Result, error) {
	t.Helper()
	s, err := a.Run(context.Background(), spec)
	require.NoError(t, err)
	var events []cartridge.SessionEvent
	res, err := cartridge.Collect(s, func(ev cartridge.SessionEvent) { events = append(events, ev) })
	return events, res, err
}

func TestDetectVersionAndAuthUnknown(t *testing.T) {
	shim := junieShim(t)
	a := junieAdapter(t, config.Cartridge{Env: map[string]string{"SC_JUNIE_VERSION_FILE": junieFixture(t, "version.txt")}})
	st, err := a.Detect(context.Background())
	require.NoError(t, err)
	assert.True(t, st.Installed)
	assert.Equal(t, shim.Path, st.Path)
	assert.Equal(t, "Junie CLI 2026.10.23", st.Version)
	assert.Equal(t, cartridge.AuthUnknown, st.Auth)
	assert.Contains(t, st.Detail, "not probed")
	assert.Equal(t, [][]string{{"--version"}}, shim.ArgLists(t))
}

func TestDetectMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	st, err := junieAdapter(t, config.Cartridge{}).Detect(context.Background())
	require.Error(t, err)
	assert.False(t, st.Installed)
	assert.Equal(t, cartridge.KindNotInstalled, cartridge.KindOf(err))
}

func TestRunFinalJSONResultAndUsage(t *testing.T) {
	shim := junieShim(t)
	extensionDump := filepath.Join(t.TempDir(), "extension-dir")
	a := junieAdapter(t, config.Cartridge{Args: []string{"--effort", "medium"}})
	spec := junieSpec(t, map[string]string{
		"SC_JUNIE_STDOUT_FILE":    junieFixture(t, "success.json"),
		"SC_JUNIE_EXTENSION_DUMP": extensionDump,
	})
	events, res, err := runJunie(t, a, spec)
	require.NoError(t, err)
	assert.Equal(t, "OK", res.Output)
	assert.Equal(t, "session-261024-120000-abcd", res.SessionID)
	assert.Equal(t, 1100, res.Usage.InputTokens)
	assert.Equal(t, 44, res.Usage.OutputTokens)
	assert.Equal(t, 220, res.Usage.CacheReadTokens)
	assert.Equal(t, 33, res.Usage.CacheWriteTokens)
	assert.Equal(t, 3, res.Usage.Turns)
	assert.True(t, res.Usage.HasCost)
	assert.InDelta(t, 0.15, res.Usage.CostUSD, 0.000001)
	assert.Equal(t, 0, res.ExitCode)
	require.Len(t, events, 2)
	assert.Equal(t, cartridge.EventTextDelta, events[0].Kind)
	assert.Equal(t, "OK", events[0].Text)
	assert.Equal(t, cartridge.EventDone, events[1].Kind)

	calls := shim.Invocations(t)
	require.Len(t, calls, 1)
	assert.Equal(t, testutil.RealPath(t, spec.Workdir), calls[0].Dir)
	assert.Equal(t, "Reply with exactly OK", calls[0].Args[len(calls[0].Args)-1])
	assert.Contains(t, calls[0].Args, "--effort")
	assert.NotContains(t, calls[0].Args, "--gateway-stop")

	data, err := os.ReadFile(extensionDump)
	require.NoError(t, err)
	extensionDir := strings.TrimSpace(string(data))
	assert.NotEmpty(t, extensionDir)
	_, err = os.Stat(extensionDir)
	assert.True(t, os.IsNotExist(err), "the isolated extension directory must be removed")
}

func TestRunRejectsRestrictedPoliciesBeforeStart(t *testing.T) {
	shim := junieShim(t)
	a := junieAdapter(t, config.Cartridge{})
	for _, policy := range []cartridge.ToolPolicy{cartridge.ToolReadOnly, cartridge.ToolReadExec} {
		spec := junieSpec(t, nil)
		spec.ToolPolicy = policy
		_, err := a.Run(context.Background(), spec)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read_write")
	}
	assert.Empty(t, shim.Invocations(t))
}

func TestRunMalformedAndMissingResults(t *testing.T) {
	for _, fixture := range []string{"malformed.json", "missing-result.json"} {
		t.Run(fixture, func(t *testing.T) {
			junieShim(t)
			a := junieAdapter(t, config.Cartridge{})
			_, _, err := runJunie(t, a, junieSpec(t, map[string]string{"SC_JUNIE_STDOUT_FILE": junieFixture(t, fixture)}))
			require.Error(t, err)
			assert.Equal(t, cartridge.KindCrash, cartridge.KindOf(err))
		})
	}
}

func TestRunNoResultIsCrash(t *testing.T) {
	junieShim(t)
	a := junieAdapter(t, config.Cartridge{})
	_, _, err := runJunie(t, a, junieSpec(t, nil))
	require.Error(t, err)
	assert.Equal(t, cartridge.KindCrash, cartridge.KindOf(err))
	assert.Contains(t, err.Error(), "without a final JSON result")
}

func TestRunTypedProviderErrors(t *testing.T) {
	for _, test := range []struct {
		fixture string
		kind    cartridge.Kind
	}{
		{"auth.stderr", cartridge.KindAuth},
		{"credits.stderr", cartridge.KindCreditsExhausted},
		{"rate.stderr", cartridge.KindRateLimited},
	} {
		t.Run(test.fixture, func(t *testing.T) {
			junieShim(t)
			a := junieAdapter(t, config.Cartridge{})
			_, _, err := runJunie(t, a, junieSpec(t, map[string]string{
				"SC_JUNIE_STDERR_FILE": junieFixture(t, test.fixture), "SC_JUNIE_EXIT": "1",
			}))
			require.Error(t, err)
			assert.Equal(t, test.kind, cartridge.KindOf(err))
		})
	}
}

func TestRunUnknownModelReportsHeadlineNotModelList(t *testing.T) {
	junieShim(t)
	a := junieAdapter(t, config.Cartridge{})
	_, _, err := runJunie(t, a, junieSpec(t, map[string]string{
		"SC_JUNIE_STDERR_FILE": junieFixture(t, "unknown-model.stderr"), "SC_JUNIE_EXIT": "1",
	}))
	require.Error(t, err)
	assert.Equal(t, cartridge.KindCrash, cartridge.KindOf(err))
	assert.Contains(t, err.Error(), "Invalid model: gpt-4.1-mini")
	assert.NotContains(t, err.Error(), "junie-lite")
}

func TestRunFinalJSONErrorsAreClassified(t *testing.T) {
	junieShim(t)
	a := junieAdapter(t, config.Cartridge{})
	_, res, err := runJunie(t, a, junieSpec(t, map[string]string{
		"SC_JUNIE_STDOUT_FILE": junieFixture(t, "error-result.json"),
	}))
	require.Error(t, err)
	assert.Equal(t, cartridge.KindAuth, cartridge.KindOf(err))
	assert.Contains(t, err.Error(), "Failed Authentication")
	assert.Equal(t, "session-auth-failure", res.SessionID)
	assert.Equal(t, 25, res.Usage.InputTokens, "usage survives a JSON-reported failure")
	assert.Empty(t, res.Output)
}

func TestRunValidResultWithNonzeroExitIsCrash(t *testing.T) {
	junieShim(t)
	a := junieAdapter(t, config.Cartridge{})
	_, res, err := runJunie(t, a, junieSpec(t, map[string]string{
		"SC_JUNIE_STDOUT_FILE": junieFixture(t, "success.json"), "SC_JUNIE_EXIT": "2",
	}))
	require.Error(t, err)
	assert.Equal(t, cartridge.KindCrash, cartridge.KindOf(err))
	assert.Equal(t, 2, res.ExitCode)
	assert.Equal(t, 1100, res.Usage.InputTokens, "usage remains available on failure")
}

func TestRunTimeout(t *testing.T) {
	junieShim(t)
	a := junieAdapter(t, config.Cartridge{})
	spec := junieSpec(t, map[string]string{"SC_JUNIE_SLEEP": "5"})
	spec.Timeout = 250 * time.Millisecond
	_, _, err := runJunie(t, a, spec)
	require.Error(t, err)
	assert.Equal(t, cartridge.KindTimeout, cartridge.KindOf(err))
}

func TestRunKill(t *testing.T) {
	junieShim(t)
	a := junieAdapter(t, config.Cartridge{})
	s, err := a.Run(context.Background(), junieSpec(t, map[string]string{"SC_JUNIE_SLEEP": "30"}))
	require.NoError(t, err)
	s.Kill()
	_, err = cartridge.Collect(s, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, cartridge.ErrKilled))
}

func TestRunEnvironmentIsClean(t *testing.T) {
	junieShim(t)
	dump := filepath.Join(t.TempDir(), "env")
	t.Setenv("JUNIE_API_KEY", "token")
	t.Setenv("SC_UNRELATED_SECRET", "not-for-child")
	a := junieAdapter(t, config.Cartridge{})
	_, _, _ = runJunie(t, a, junieSpec(t, map[string]string{"SC_JUNIE_ENV_DUMP": dump}))
	data, err := os.ReadFile(dump)
	require.NoError(t, err)
	assert.Contains(t, string(data), "JUNIE_API_KEY=token")
	assert.NotContains(t, string(data), "SC_UNRELATED_SECRET")
}
