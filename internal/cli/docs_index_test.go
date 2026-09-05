package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// P10-04. Prose drifts silently: docs/releasing.md shipped naming environment
// variables install.sh does not have, and the docs index shipped without the
// page P10-04 itself added. These two tests are what keeps the release prose
// honest about the files it describes.

// repoFile reads a file at the repository root, from internal/cli.
func repoFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", name))
	require.NoError(t, err, "reading %s", name)
	return string(b)
}

// TestDocsIndexListsEveryPage guards P10-04: README.md calls docs/README.md
// "the index: every page below", so a page in docs/ that the index does not
// list is unreachable from the entry point a reader actually starts at. The
// changelog is held to the same rule — release notes nothing links to are
// release notes nobody finds.
func TestDocsIndexListsEveryPage(t *testing.T) {
	index := repoFile(t, "docs/README.md")

	pages, err := filepath.Glob(filepath.Join("..", "..", "docs", "*.md"))
	require.NoError(t, err)
	require.NotEmpty(t, pages)

	listed := 0
	for _, page := range pages {
		name := filepath.Base(page)
		if name == "README.md" {
			continue
		}
		require.Contains(t, index, "]("+name+")",
			"docs/README.md does not link docs/%s; the index must list every page in docs/", name)
		listed++
	}
	require.NotZero(t, listed)

	require.Contains(t, index, "](../CHANGELOG.md)",
		"docs/README.md does not link the changelog")
	require.Contains(t, repoFile(t, "README.md"), "](CHANGELOG.md)",
		"README.md's Documentation table does not link the changelog")
}

var docsEnvVarRe = regexp.MustCompile("`(SC_[A-Z0-9_]+)`")

// TestReleasingDocNamesRealEnvironmentVariables guards P10-04: docs/releasing.md
// told the reader to set SC_REPO_OWNER / SC_REPO_NAME for install.sh, which
// reads neither — it takes the pair as one SC_REPO. Every SC_* variable the
// page names must exist in an artefact that reads it, and the page must name
// install.sh's real repository override rather than only goreleaser's pair.
func TestReleasingDocNamesRealEnvironmentVariables(t *testing.T) {
	doc := repoFile(t, "docs/releasing.md")
	sources := map[string]string{
		"install.sh":                    repoFile(t, "install.sh"),
		".goreleaser.yaml":              repoFile(t, ".goreleaser.yaml"),
		".github/workflows/release.yml": repoFile(t, ".github/workflows/release.yml"),
	}

	seen := map[string]bool{}
	for _, m := range docsEnvVarRe.FindAllStringSubmatch(doc, -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		found := ""
		for file, body := range sources {
			if strings.Contains(body, name) {
				found = file
				break
			}
		}
		require.NotEmpty(t, found,
			"docs/releasing.md names %s, which none of install.sh, .goreleaser.yaml or the release workflow reads", name)
	}

	require.True(t, strings.Contains(sources["install.sh"], "SC_REPO:-"),
		"install.sh no longer reads SC_REPO; docs/releasing.md and this test need updating together")
	require.True(t, seen["SC_REPO"],
		"docs/releasing.md never names SC_REPO, the variable install.sh actually reads for the repository")
}
