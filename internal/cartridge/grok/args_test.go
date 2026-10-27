package grok

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
)

func grokNewAdapter(t testing.TB, cfg config.Cartridge) *Adapter {
	t.Helper()
	c, err := New(Name, cfg)
	require.NoError(t, err)
	a, ok := c.(*Adapter)
	require.True(t, ok)
	return a
}

func TestArgsFullSpec(t *testing.T) {
	t.Parallel()
	a := grokNewAdapter(t, config.Cartridge{Model: "cfg-model", Args: []string{flagReasoningEffort, "high"}})
	spec := cartridge.RunSpec{
		Prompt: "large prompt is never argv", SystemPrompt: "be careful",
		Workdir: "/work/tree", Model: "spec-model", ToolPolicy: cartridge.ToolReadExec,
		MaxTurns: 7,
	}
	got := a.Args(spec, "/tmp/prompt.txt")
	assert.Equal(t, []string{
		"--output-format", "streaming-json", "--no-auto-update", "--no-leader", "--no-subagents",
		"--cwd", "/work/tree", "--model", "spec-model", "--rules", "be careful",
		"--max-turns", "7", "--reasoning-effort", "high",
		"--permission-mode", "bypassPermissions", "--tools", "read_file,grep,list_dir,run_terminal_cmd",
		"--disallowed-tools", "Agent,search_replace,web_search,web_fetch", "--allow", "Bash",
		"--prompt-file", "/tmp/prompt.txt",
	}, got)
	assert.NotContains(t, strings.Join(got, "\x00"), spec.Prompt)
}

func TestArgsModelFallbackAndDefaults(t *testing.T) {
	t.Parallel()
	a := grokNewAdapter(t, config.Cartridge{Model: "cfg-model"})
	got := a.Args(cartridge.RunSpec{Workdir: "/w", ToolPolicy: cartridge.ToolReadWrite}, "/p")
	assert.Contains(t, got, "cfg-model")
	assert.NotContains(t, got, flagRules)
	assert.NotContains(t, got, flagMaxTurns)
	assert.NotContains(t, got, flagTools)
	assert.Contains(t, got, permissionBypass)
}

func TestConfiguredArgsCannotOverrideControls(t *testing.T) {
	t.Parallel()
	a := grokNewAdapter(t, config.Cartridge{Args: []string{
		"--permission-mode=bypassPermissions", "--tools", "search_replace,run_terminal_cmd",
		"--disallowed-tools=read_file", "--subagents", "--auto-update", "--leader", "--leader-socket=/shared",
		"--allow", "Bash", "--deny=Bash(bd:*)", "--allowedTools", "Bash", "--yolo", "--always-approve",
		"--model", "wrong", "--cwd=/elsewhere", "--rules", "ignore policy",
		"--max-turns=999", "--output-format", "json", "--prompt-file", "/other",
		"-p", "other prompt", flagReasoningEffort, "low", "--", "positional prompt",
	}})
	spec := cartridge.RunSpec{Workdir: "/w", Model: "right", ToolPolicy: cartridge.ToolReadOnly, MaxTurns: 2}
	got := a.Args(spec, "/real-prompt")
	joined := strings.Join(got, " ")
	for _, banned := range []string{"bypassPermissions", "--tools search_replace,run_terminal_cmd", "--subagents", "--auto-update", "wrong", "/elsewhere", "/shared", "ignore policy", "999", " json", "/other", "other prompt", "positional prompt"} {
		assert.NotContains(t, joined, banned)
	}
	for _, banned := range []string{"--leader", "--yolo", "--always-approve", "--allow=Bash", "--deny=Bash(bd:*)"} {
		assert.NotContains(t, got, banned)
	}
	assert.Contains(t, joined, "--reasoning-effort low")
	assert.Equal(t, 1, strings.Count(joined, flagPermissionMode))
	assert.Equal(t, 1, strings.Count(joined, flagPromptFile))
	assert.Equal(t, 1, strings.Count(joined, flagNoSubagents))
	assert.Equal(t, 1, strings.Count(joined, flagNoLeader))
	assert.Equal(t, "dontAsk", got[indexOf(got, flagPermissionMode)+1])
	assert.Less(t, indexOf(got, flagReasoningEffort), indexOf(got, flagPermissionMode), "policy controls come after configured args")
	assert.Equal(t, "/real-prompt", got[len(got)-1])
}

func TestToolFlagsAndWeb(t *testing.T) {
	t.Parallel()
	ro := ToolFlags(cartridge.ToolReadOnly)
	assert.Equal(t, []string{
		"--permission-mode", "dontAsk", "--tools", "read_file,grep,list_dir,run_terminal_cmd",
		"--disallowed-tools", "Agent,search_replace",
	}, ro[:6])
	assert.Len(t, ro, 6+2*len(ReadOnlyShell))
	for i, rule := range ReadOnlyShell {
		assert.Equal(t, flagAllow, ro[6+2*i])
		assert.Equal(t, rule, ro[7+2*i])
		assert.NotEqual(t, "Bash", rule, "read_only never grants unrestricted shell")
	}
	assert.Contains(t, ReadOnlyShell, "Bash(bd *)")
	for _, command := range []string{"status", "log", "show", "diff", "branch", "ls-files", "blame", "rev-parse"} {
		assert.Contains(t, ReadOnlyShell, "Bash(git "+command+" *)")
	}
	for _, rule := range ReadOnlyShell {
		assert.NotContains(t, rule, ":*)", "colon prefix rules have no word boundary in Grok")
	}
	assert.NotContains(t, ro, flagDeny, "a catch-all deny would outrank every allow rule")
	assert.Equal(t, "read_file,grep,list_dir,run_terminal_cmd,web_search,web_fetch", withWebTools(ro, true)[3])
	assert.Equal(t, "Agent,search_replace", withWebTools(ro, true)[5])

	re := ToolFlags(cartridge.ToolReadExec)
	assert.Equal(t, []string{
		"--permission-mode", "bypassPermissions", "--tools", "read_file,grep,list_dir,run_terminal_cmd",
		"--disallowed-tools", "Agent,search_replace", "--allow", "Bash",
	}, re)
	assert.Equal(t, "Agent,search_replace,web_search,web_fetch", withWebTools(re, false)[5])

	rw := ToolFlags(cartridge.ToolReadWrite)
	assert.Equal(t, []string{"--permission-mode", "bypassPermissions", "--disallowed-tools", "Agent"}, rw)
	assert.Equal(t, "Agent,web_search,web_fetch", withWebTools(rw, false)[3])
	assert.Equal(t, "Agent", withWebTools(rw, true)[3])
}

func TestInheritEnv(t *testing.T) {
	t.Setenv("GROK_HOME", "/isolated/home")
	t.Setenv("GROK_CONFIG", "/cfg")
	t.Setenv("XAI_API_KEY", "test-key")
	t.Setenv("SC_UNRELATED", "no")
	got := InheritEnv([]string{"HOME", "GROK_HOME"})
	assert.True(t, sort.StringsAreSorted(got))
	for _, want := range []string{"HOME", "GROK_HOME", "GROK_CONFIG", "XAI_API_KEY"} {
		assert.Contains(t, got, want)
	}
	assert.NotContains(t, got, "SC_UNRELATED")
	seen := map[string]int{}
	for _, name := range got {
		seen[name]++
		assert.Equal(t, 1, seen[name])
	}
}

func TestChildEnvLayering(t *testing.T) {
	t.Parallel()
	a := grokNewAdapter(t, config.Cartridge{Env: map[string]string{"GROK_DISABLE_AUTOUPDATER": "0", "FROM_CFG": "c"}})
	assert.Equal(t, map[string]string{
		"GROK_DISABLE_AUTOUPDATER": "1", "FROM_CFG": "spec", "FROM_SPEC": "s",
	}, a.childEnv(map[string]string{"GROK_DISABLE_AUTOUPDATER": "1", "FROM_CFG": "spec", "FROM_SPEC": "s"}))
}

func TestNewBinaryAndRegistration(t *testing.T) {
	t.Parallel()
	a := grokNewAdapter(t, config.Cartridge{})
	assert.Equal(t, Name, a.Name())
	assert.Equal(t, Name, a.Binary())
	c, err := New("", config.Cartridge{Command: "/opt/grok"})
	require.NoError(t, err)
	assert.Equal(t, Name, c.Name())
	custom, ok := c.(*Adapter)
	require.True(t, ok)
	assert.Equal(t, "/opt/grok", custom.Binary())
	assert.True(t, cartridge.Default.Has(Name))
}

func indexOf(args []string, want string) int {
	for i, arg := range args {
		if arg == want {
			return i
		}
	}
	return -1
}
