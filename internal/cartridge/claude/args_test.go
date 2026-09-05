package claude

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
)

func claudeNewAdapter(t testing.TB, cfg config.Cartridge) *Adapter {
	t.Helper()
	c, err := New("claude", cfg)
	require.NoError(t, err)
	a, ok := c.(*Adapter)
	require.True(t, ok)
	return a
}

func TestArgs_FullSpec(t *testing.T) {
	t.Parallel()
	a := claudeNewAdapter(t, config.Cartridge{Model: "cfg-model", Args: []string{"--effort", "high"}})
	spec := cartridge.RunSpec{
		Prompt:       "do it",
		SystemPrompt: "be terse",
		Workdir:      "/w",
		Model:        "spec-model",
		ToolPolicy:   cartridge.ToolReadExec,
		MaxTurns:     7,
	}
	got := a.Args(spec)
	assert.Equal(t, []string{
		"-p", "--output-format", "stream-json", "--verbose",
		"--model", "spec-model",
		"--append-system-prompt", "be terse",
		"--max-turns", "7",
		"--permission-mode", "dontAsk",
		"--allowedTools", "Bash",
		"--disallowedTools", "Edit,Write,MultiEdit,NotebookEdit",
		"--effort", "high",
	}, got)
	assert.NotContains(t, strings.Join(got, "\x00"), "do it", "the prompt goes to stdin, never argv")
}

func TestArgs_DefaultsAndModelFallback(t *testing.T) {
	t.Parallel()
	a := claudeNewAdapter(t, config.Cartridge{Model: "cfg-model"})
	got := a.Args(cartridge.RunSpec{Prompt: "x", Workdir: "/w", ToolPolicy: cartridge.ToolReadWrite})
	assert.Equal(t, []string{
		"-p", "--output-format", "stream-json", "--verbose",
		"--model", "cfg-model",
		"--permission-mode", "dontAsk",
		"--allowedTools", "Bash,Edit,Write,MultiEdit,NotebookEdit",
	}, got)

	none := claudeNewAdapter(t, config.Cartridge{})
	got = none.Args(cartridge.RunSpec{Prompt: "x", Workdir: "/w", ToolPolicy: cartridge.ToolReadOnly})
	assert.NotContains(t, got, "--model")
	assert.NotContains(t, got, "--max-turns")
	assert.NotContains(t, got, "--append-system-prompt")
}

func TestToolFlags(t *testing.T) {
	t.Parallel()
	ro := ToolFlags(cartridge.ToolReadOnly)
	assert.Equal(t, []string{"--permission-mode", "dontAsk"}, ro[:2])
	assert.Equal(t, "--allowedTools", ro[2])
	assert.Equal(t, strings.Join(ReadOnlyShell, ","), ro[3])
	assert.NotContains(t, ro[3], "Bash,", "read_only never allows unrestricted Bash")
	assert.Equal(t, []string{"--disallowedTools", "Edit,Write,MultiEdit,NotebookEdit"}, ro[4:])
	for _, rule := range ReadOnlyShell {
		assert.True(t, strings.HasPrefix(rule, "Bash("), rule)
		assert.True(t, strings.HasSuffix(rule, ")"), rule)
		assert.NotContains(t, rule, ",", "rules are comma-joined into one argument")
	}

	re := ToolFlags(cartridge.ToolReadExec)
	assert.Equal(t, []string{"--permission-mode", "dontAsk", "--allowedTools", "Bash", "--disallowedTools", "Edit,Write,MultiEdit,NotebookEdit"}, re)

	rw := ToolFlags(cartridge.ToolReadWrite)
	assert.Equal(t, []string{"--permission-mode", "dontAsk", "--allowedTools", "Bash,Edit,Write,MultiEdit,NotebookEdit"}, rw)
	assert.NotContains(t, rw, "--disallowedTools")

	// An invalid policy (Run validates first) still yields a non-prompting
	// mode.
	assert.Equal(t, []string{"--permission-mode", "dontAsk"}, ToolFlags(""))
}

func TestInheritEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	t.Setenv("ANTHROPIC_BASE_URL", "u")
	t.Setenv("CLAUDE_CONFIG_DIR", "/cfg")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("AWS_REGION", "eu-west-1")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s")
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "tok")
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "sock")
	t.Setenv("CLAUDE_PID", "1")
	t.Setenv("CLAUDE_EFFORT", "high")
	t.Setenv("SC_UNRELATED", "x")

	got := InheritEnv([]string{"HOME", "ANTHROPIC_API_KEY"})
	assert.True(t, sort.StringsAreSorted(got))
	for _, want := range []string{"HOME", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_USE_BEDROCK", "AWS_REGION", "DISABLE_AUTOUPDATER", "MCP_TIMEOUT"} {
		assert.Contains(t, got, want)
	}
	for _, drop := range []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_CODE_MESSAGING_SOCKET", "CLAUDE_PID", "CLAUDE_EFFORT", "SC_UNRELATED"} {
		assert.NotContains(t, got, drop)
	}
	seen := map[string]int{}
	for _, n := range got {
		seen[n]++
		assert.Equal(t, 1, seen[n], "duplicate %s", n)
	}
	assert.Nil(t, nil, InheritEnv(nil))
}

func TestIsSessionMarker(t *testing.T) {
	t.Parallel()
	assert.True(t, IsSessionMarker("CLAUDECODE"))
	assert.True(t, IsSessionMarker("CLAUDE_CODE_MESSAGING_ANYTHING"))
	assert.True(t, IsSessionMarker("CLAUDE_CODE_ENTRYPOINT"))
	assert.False(t, IsSessionMarker("CLAUDE_CODE_USE_VERTEX"))
	assert.False(t, IsSessionMarker("ANTHROPIC_API_KEY"))
}

func TestChildEnv_Layering(t *testing.T) {
	t.Parallel()
	a := claudeNewAdapter(t, config.Cartridge{Env: map[string]string{"DISABLE_AUTOUPDATER": "0", "FROM_CFG": "c"}})
	env := a.childEnv(map[string]string{"FROM_CFG": "spec", "FROM_SPEC": "s"})
	assert.Equal(t, map[string]string{"DISABLE_AUTOUPDATER": "0", "FROM_CFG": "spec", "FROM_SPEC": "s"}, env)

	plain := claudeNewAdapter(t, config.Cartridge{})
	assert.Equal(t, map[string]string{"DISABLE_AUTOUPDATER": "1"}, plain.childEnv(nil))
}

func TestNewAndBinary(t *testing.T) {
	t.Parallel()
	a := claudeNewAdapter(t, config.Cartridge{})
	assert.Equal(t, "claude", a.Name())
	assert.Equal(t, "claude", a.Binary())

	c, err := New("", config.Cartridge{Command: "/opt/claude/bin/claude"})
	require.NoError(t, err)
	assert.Equal(t, "claude", c.Name(), "an empty registry name falls back to the adapter name")
	custom, ok := c.(*Adapter)
	require.True(t, ok)
	assert.Equal(t, "/opt/claude/bin/claude", custom.Binary())

	c, err = New("claude-work", config.Cartridge{})
	require.NoError(t, err)
	assert.Equal(t, "claude-work", c.Name())
}

func TestRegisteredInDefaultRegistry(t *testing.T) {
	t.Parallel()
	assert.True(t, cartridge.Default.Has(Name))
	c, err := cartridge.New(Name, config.Cartridge{Model: "m"})
	require.NoError(t, err)
	_, ok := c.(*Adapter)
	assert.True(t, ok)
}

// TestReadOnlyKeepsTheShellAllowList pins the P5-09 decision. The rejected
// alternative is "--disallowedTools Bash,Edit,Write,MultiEdit,NotebookEdit",
// which restores Claude Code's Glob and Grep but takes away the shell the
// product owner needs for bd and git. The decision is recorded in
// docs/cartridges.md; this test fails if the code moves without it.
func TestReadOnlyKeepsTheShellAllowList(t *testing.T) {
	t.Parallel()
	ro := ToolFlags(cartridge.ToolReadOnly)

	allowed, disallowed := "", ""
	for i, a := range ro {
		switch a {
		case flagAllowedTools:
			require.Less(t, i+1, len(ro))
			allowed = ro[i+1]
		case flagDisallowedTools:
			require.Less(t, i+1, len(ro))
			disallowed = ro[i+1]
		}
	}
	require.NotEmpty(t, allowed, "read_only must pre-approve a shell allow-list, not remove the shell")
	assert.Equal(t, strings.Join(ReadOnlyShell, ","), allowed)
	assert.Equal(t, strings.Join(editTools, ","), disallowed,
		"the rejected alternative also disallows Bash; this mapping does not")
	for _, tool := range strings.Split(disallowed, ",") {
		assert.NotEqual(t, "Bash", tool, "P5-09: read_only never disallows Bash")
	}

	// The commands the decision rests on: bd (D-21 shim) and git history,
	// which Glob and Grep cannot replace, plus the search the removed
	// built-ins would have provided.
	for _, cmd := range []string{"bd", "git log", "git diff", "git show", "grep", "rg", "find"} {
		assert.Contains(t, ReadOnlyShell, "Bash("+cmd+":*)", "P5-09 relies on %q being allowed", cmd)
	}
	// And the ones a read-only role must never get.
	for _, rule := range ReadOnlyShell {
		for _, banned := range []string{"rm", "git commit", "git push", "sed", "tee", "sh", "bash", "go "} {
			assert.NotEqual(t, "Bash("+banned+":*)", rule, "%q writes; read_only must refuse it", banned)
		}
	}
}

// TestDocsRecordTheReadOnlyDecision is the other half of P5-09's "done
// when": the choice, and the alternative it rules out, are written down
// where an adapter author will read them.
func TestDocsRecordTheReadOnlyDecision(t *testing.T) {
	t.Parallel()
	doc := claudeReadDoc(t)
	assert.Contains(t, doc, "P5-09", "the decision must be attributed to its task")
	assert.Contains(t, doc, "`--disallowedTools Bash,Edit,Write,MultiEdit,NotebookEdit`",
		"the rejected alternative must be spelled out, so nobody re-opens it blind")
	assert.Contains(t, doc, "`read_only` keeps the restricted `Bash` allow-list")
}
