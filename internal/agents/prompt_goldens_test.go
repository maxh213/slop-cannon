package agents_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pgGoldens is every prompt golden checked into the repository, by directory,
// with the test that writes it. Nothing else may sit in these directories:
// a golden nobody produces is a prompt nobody reviews any more, and unlike
// the TUI frames a stale one is easy to miss, because a renamed fixture
// simply writes a new file next to it (docs/goldens.md).
//
// Add the file here in the same change that adds the test that writes it.
var pgGoldens = map[string]map[string]string{
	"../../testdata/prompts": {
		"po-plan.golden.md":                         "prompt.TestBuildGoldenPO",
		"engineer-implement.golden.md":              "prompt.TestBuildGoldenEngineer",
		"tester-qa-review.golden.md":                "prompt.TestBuildGoldenTester",
		"agent-engineer-implement.golden.md":        "agents.TestEngineerGoldenImplement",
		"agent-engineer-address-feedback.golden.md": "agents.TestEngineerGoldenAddressFeedback",
		"agent-engineer-resolve-conflict.golden.md": "agents.TestEngineerGoldenResolveConflict",
	},
	"../../testdata/agents/tester": {
		"qa-review.golden.md":              "agents.TestTesterPromptGoldens",
		"qa-re_review.golden.md":           "agents.TestTesterPromptGoldens",
		"qa-evaluate_pushback.golden.md":   "agents.TestTesterPromptGoldens",
		"ux-review.golden.md":              "agents.TestTesterPromptGoldens",
		"ux-re_review.golden.md":           "agents.TestTesterPromptGoldens",
		"ux-evaluate_pushback.golden.md":   "agents.TestTesterPromptGoldens",
		"test-review.golden.md":            "agents.TestTesterPromptGoldens",
		"test-re_review.golden.md":         "agents.TestTesterPromptGoldens",
		"test-evaluate_pushback.golden.md": "agents.TestTesterPromptGoldens",
	},
	"testdata/plan": {
		"po-plan-autopilot.golden.md": "agents.TestPOPlanPromptGolden",
		"po-plan-brief.golden.md":     "agents.TestPOPlanPromptGolden",
	},
	"testdata/arbitration": {
		"po-arbitrate.golden.md": "agents.TestArbitrationPromptGolden",
		"po-escalate.golden.md":  "agents.TestEscalationPromptGolden",
	},
}

// TestPromptGoldenFilesAreAccountedFor is the orphan check for the prompt
// goldens: every .golden.md on disk is produced by a named test, every named
// golden exists, and each file is readable UTF-8 text without carriage
// returns. Renaming or dropping a fixture therefore fails here until the list
// is updated too.
func TestPromptGoldenFilesAreAccountedFor(t *testing.T) {
	t.Parallel()
	for dir, want := range pgGoldens {
		t.Run(filepath.ToSlash(dir), func(t *testing.T) {
			t.Parallel()
			entries, err := os.ReadDir(dir)
			require.NoError(t, err, "the golden directory must exist")

			found := make([]string, 0, len(want))
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() || !strings.HasSuffix(name, ".golden.md") {
					continue // README.md and any nested fixtures are not goldens
				}
				found = append(found, name)
				assert.Containsf(t, want, name,
					"%s is not produced by any test; delete it or list it in pgGoldens", filepath.Join(dir, name))

				body, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a checked-in test fixture
				require.NoError(t, err)
				assert.NotEmpty(t, body, "%s is empty", name)
				assert.True(t, utf8.Valid(body), "%s is not valid UTF-8", name)
				assert.NotContains(t, string(body), "\r", "%s has carriage returns", name)
				assert.True(t, strings.HasSuffix(string(body), "\n"), "%s does not end in a newline", name)
			}

			names := make([]string, 0, len(want))
			for name, test := range want {
				names = append(names, name)
				assert.Containsf(t, found, name,
					"%s is written by %s but is not on disk; run `make update-golden`", name, test)
			}
			sort.Strings(names)
			sort.Strings(found)
			assert.Equal(t, names, found, "the goldens on disk and the ones listed here must be the same set")
		})
	}
}
