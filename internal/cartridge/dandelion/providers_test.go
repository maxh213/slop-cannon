package dandelion

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
)

// cliProbeIDs names the provider id behind each probe that rawProbes in
// src/probes/index.ts builds with cliProbe(io, <probe>), whose id lives in
// another file. A probe not listed here fails the test: look its id up and
// add it, then classify the id in Providers.
var cliProbeIDs = map[string]string{
	"claudeProbe":       "claude",
	"claudeWorkProbe":   "claude-work",
	"claudeAnimusProbe": "claude-animus",
	"agyProbe":          "agy",
	"kiloProbe":         "kilo",
}

var (
	targetLine   = regexp.MustCompile(`^\s*'?([a-z][a-z0-9-]*)'?:\s*\{\s*command:\s*'([^']+)'`)
	homeLine     = regexp.MustCompile(`'([a-z0-9-]+)':\s*\{\s*variable:\s*'(\w+)',\s*name:\s*'(\w+)',\s*fallback:\s*'([^']+)'\s*\}`)
	probeLiteral = regexp.MustCompile(`id:\s*'([a-z0-9-]+)'`)
	probeCLI     = regexp.MustCompile(`cliProbe\(io,\s*(\w+)`)
)

// block returns the lines of src from the first one containing start up to
// the first line after it that is exactly end.
func block(t *testing.T, src, start, end string) []string {
	t.Helper()
	var out []string
	in := false
	for _, l := range strings.Split(src, "\n") {
		if !in && strings.Contains(l, start) {
			in = true
			continue
		}
		if in && strings.TrimSpace(l) == end {
			return out
		}
		if in {
			out = append(out, l)
		}
	}
	t.Fatalf("no %q block ending in %q", start, end)
	return nil
}

// checkDandelionSources parses launch.ts and probes/index.ts from dir and
// checks Providers against them.
func checkDandelionSources(t *testing.T, launchPath, probesPath string) {
	t.Helper()
	launch, err := os.ReadFile(launchPath)
	require.NoError(t, err)
	probes, err := os.ReadFile(probesPath)
	require.NoError(t, err)

	// Every account a route line can name is classified, and its driver
	// follows from the command launch.ts runs for it.
	targets := map[string]string{}
	for _, l := range block(t, string(launch), "const TARGETS", "};") {
		if m := targetLine.FindStringSubmatch(l); m != nil {
			targets[m[1]] = m[2]
		}
	}
	require.NotEmpty(t, targets, "no TARGETS parsed from %s", launchPath)
	for id, command := range targets {
		p, ok := providerByID(id)
		if !assert.True(t, ok, "launch.ts account %q is not in dandelion.Providers; classify it", id) {
			continue
		}
		want := Driver(command)
		switch id {
		case "cursor":
			want = DriverCursor
		case "codex-animus":
			want = DriverCodex
		}
		assert.Equal(t, want, p.Driver, "%s launches %s", id, command)
	}

	// A Claude account's config dir is exactly the one launch.ts sets, and
	// a Claude account with no ACCOUNT_HOMES entry uses the default home.
	homes := map[string][3]string{}
	for _, m := range homeLine.FindAllStringSubmatch(string(launch), -1) {
		homes[m[1]] = [3]string{m[2], m[3], m[4]}
	}
	require.NotEmpty(t, homes, "no ACCOUNT_HOMES parsed from %s", launchPath)
	for _, p := range Providers {
		if p.Driver != DriverClaude {
			continue
		}
		h, ok := homes[p.ID]
		if !ok {
			assert.Empty(t, p.ConfigDirEnv, "%s has no ACCOUNT_HOMES entry", p.ID)
			continue
		}
		assert.Equal(t, [3]string{"CLAUDE_CONFIG_DIR", p.ConfigDirEnv, p.ConfigDirFallback}, h, p.ID)
	}
	assert.Equal(t, [3]string{"CODEX_HOME", "DANDELION_CODEX_ANIMUS_HOME", ".codex-animus"}, homes["codex-animus"])

	// Every probe id is classified, so DANDELION_DISABLE can switch off
	// every provider sc cannot drive.
	var ids []string
	for _, l := range block(t, string(probes), "function rawProbes", "}") {
		if m := probeLiteral.FindStringSubmatch(l); m != nil {
			ids = append(ids, m[1])
			continue
		}
		if m := probeCLI.FindStringSubmatch(l); m != nil {
			id, ok := cliProbeIDs[m[1]]
			require.True(t, ok, "rawProbes uses cliProbe(io, %s): add its provider id to cliProbeIDs", m[1])
			ids = append(ids, id)
		}
	}
	require.NotEmpty(t, ids, "no probes parsed from %s", probesPath)
	table := make([]string, 0, len(Providers))
	for _, p := range Providers {
		table = append(table, p.ID)
	}
	assert.Equal(t, ids, table, "dandelion.Providers must list every probe id in dandelion's order")
}

// TestProviders_MatchPinnedDandelionSources checks Providers against the
// copies of dandelion's launch.ts and probes/index.ts in testdata/dandelion
// (see its README for refreshing them). SC_DANDELION_SRC=<checkout> checks
// a live dandelion checkout as well.
func TestProviders_MatchPinnedDandelionSources(t *testing.T) {
	t.Parallel()
	dir, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", "dandelion"))
	require.NoError(t, err)
	checkDandelionSources(t, filepath.Join(dir, "launch.ts"), filepath.Join(dir, "probes-index.ts"))

	if src := os.Getenv("SC_DANDELION_SRC"); src != "" {
		t.Run("live checkout", func(t *testing.T) {
			checkDandelionSources(t, filepath.Join(src, "src", "app", "launch.ts"), filepath.Join(src, "src", "probes", "index.ts"))
		})
	}
}

func TestProviders_Policy(t *testing.T) {
	claudes := []string{"claude", "claude-work", "claude-animus", "claude-deepseek", "claude-ollama"}
	available := func(Driver) bool { return true }
	assert.Equal(t, claudes, allowedProviders(Providers, builtinDrivers, cartridge.ToolReadOnly, available))
	assert.Equal(t, []string{"claude", "claude-work", "claude-animus", "claude-deepseek", "claude-ollama", "kimi", "grok", "codex", "codex-animus", "hermes", "kilo"},
		allowedProviders(Providers, builtinDrivers, cartridge.ToolReadExec, available))
	assert.Equal(t, []string{"claude", "claude-work", "claude-animus", "claude-deepseek", "claude-ollama", "agy", "kimi", "grok", "codex", "codex-animus", "cursor", "junie", "hermes", "kilo"},
		allowedProviders(Providers, builtinDrivers, cartridge.ToolReadWrite, available))
	for policy, want := range map[cartridge.ToolPolicy]string{
		cartridge.ToolReadOnly:  "agy,kimi,grok,codex,codex-animus,cursor,junie,hermes,kilo",
		cartridge.ToolReadExec:  "agy,cursor,junie",
		cartridge.ToolReadWrite: "",
	} {
		allowed := allowedProviders(Providers, builtinDrivers, policy, available)
		assert.Equal(t, want, Disabled(allowed, ""), policy)
	}
	for _, p := range Providers {
		for _, policy := range cartridge.ToolPolicies() {
			want := p.Driver == DriverClaude ||
				(policy == cartridge.ToolReadExec && (p.Driver == DriverCodex || p.Driver == DriverKimi || p.Driver == DriverGrok || p.Driver == DriverHermes || p.Driver == DriverKilo)) ||
				(policy == cartridge.ToolReadWrite && p.Driver != DriverNone)
			assert.Equal(t, want, p.allows(policy), "%s %s", p.ID, policy)
		}
	}

	// Public Allowed retains its Cursor test seam while every other driver
	// uses its real binary check rather than host-dependent implicit availability.
	t.Setenv("PATH", t.TempDir())
	assert.Equal(t, claudes, Allowed(cartridge.ToolReadOnly, true))
	assert.Equal(t, claudes, Allowed(cartridge.ToolReadExec, true))
	assert.Equal(t, append(append([]string{}, claudes...), "cursor"), Allowed(cartridge.ToolReadWrite, true))
	assert.Equal(t, claudes, Allowed(cartridge.ToolReadWrite, false))
}
