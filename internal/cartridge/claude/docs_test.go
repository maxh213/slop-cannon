package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// claudeReadDoc returns docs/cartridges.md, the human-facing copy of the
// tables in this package.
func claudeReadDoc(t testing.TB) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "docs", "cartridges.md"))
	require.NoError(t, err)
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	return string(data)
}

// TestDocs_MappingTableMatchesCode fails when docs/cartridges.md drifts from
// the code: every TextRules needle, the verified CLI version, the minimum
// version, every flag the adapter passes and every ReadOnlyShell command
// must be mentioned in the document.
func TestDocs_MappingTableMatchesCode(t *testing.T) {
	t.Parallel()
	doc := claudeReadDoc(t)

	// mentions asserts without dumping the whole document on failure.
	mentions := func(what, needle string) {
		t.Helper()
		assert.True(t, strings.Contains(doc, needle), "%s %q is not mentioned in docs/cartridges.md", what, needle)
	}

	mentions("verified version", "`"+VerifiedVersion+" (Claude Code)`")
	mentions("minimum version", "`"+MinVersion.String()+"`")
	for _, r := range TextRules {
		mentions("TextRules needle", "`"+r.Needle+"`")
	}
	for _, flag := range []string{
		flagPrint, flagOutputFormat, outputFormatStream, flagVerbose, flagModel, flagAppendSystem,
		flagMaxTurns, flagPermissionMode, permissionModeNoAsk, flagAllowedTools, flagDisallowedTools,
		versionFlag, authStatusSubcommand + " status --json",
	} {
		mentions("flag", flag)
	}
	for _, tool := range editTools {
		mentions("edit tool", tool)
	}
	for _, rule := range ReadOnlyShell {
		cmd := strings.TrimSuffix(strings.TrimPrefix(rule, "Bash("), ")")
		cmd = strings.TrimSuffix(cmd, ":*")
		mentions("read-only shell command", "`"+cmd+"`")
	}
	for name := range sessionMarkers {
		mentions("session marker", "`"+name+"`")
	}
	for _, name := range envNames {
		mentions("inherited variable", "`"+name+"`")
	}
}
