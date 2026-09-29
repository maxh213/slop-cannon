package cursor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDocs_MappingTableMatchesCode fails when docs/cartridges.md drifts from
// the code: every TextRules needle, the verified and minimum versions, every
// flag the adapter passes and every session marker must be mentioned.
func TestDocs_MappingTableMatchesCode(t *testing.T) {
	t.Parallel()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "docs", "cartridges.md"))
	require.NoError(t, err)
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	doc := string(data)

	mentions := func(what, needle string) {
		t.Helper()
		assert.True(t, strings.Contains(doc, needle), "%s %q is not mentioned in docs/cartridges.md", what, needle)
	}
	mentions("verified version", "`"+VerifiedVersion+"`")
	mentions("minimum version", "`"+MinVersion.String()+"`")
	for _, r := range TextRules {
		mentions("TextRules needle", "`"+r.Needle+"`")
	}
	for _, flag := range []string{
		flagPrint, flagOutputFormat, outputFormatStream, flagForce, flagTrust, flagSandbox, sandboxDisabled,
		flagWorkspace, flagModel, versionFlag, statusSubcommand,
	} {
		mentions("flag", "`"+flag)
	}
	for name := range sessionMarkers {
		mentions("session marker", "`"+name+"`")
	}
}
