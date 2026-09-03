package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
)

func TestLoadDocs(t *testing.T) {
	dir := t.TempDir()
	rules := filepath.Join(dir, "rules.md")
	vibes := filepath.Join(dir, "vibes.md")
	require.NoError(t, os.WriteFile(rules, []byte("- No new dependencies.\n\n"), 0o644))
	require.NoError(t, os.WriteFile(vibes, []byte("- Short functions.\n"), 0o644))

	docs, err := LoadDocs([]string{rules, vibes})
	require.NoError(t, err)
	require.Len(t, docs, 2)
	assert.Equal(t, "rules.md", docs[0].Name)
	assert.Equal(t, rules, docs[0].Path)
	assert.Equal(t, "- No new dependencies.", docs[0].Content)
	assert.Equal(t, "- Short functions.", docs[1].Content)

	empty, err := LoadDocs(nil)
	require.NoError(t, err)
	assert.Nil(t, empty)
}

func TestLoadDocRejectsUnusableFiles(t *testing.T) {
	dir := t.TempDir()

	_, err := LoadDoc(filepath.Join(dir, "missing.md"))
	require.Error(t, err)

	_, err = LoadDoc(dir)
	require.ErrorContains(t, err, "not a regular file")

	binary := filepath.Join(dir, "binary.md")
	require.NoError(t, os.WriteFile(binary, []byte("a\x00b"), 0o644))
	_, err = LoadDoc(binary)
	require.ErrorContains(t, err, "NUL bytes")

	big := filepath.Join(dir, "big.md")
	require.NoError(t, os.WriteFile(big, []byte(strings.Repeat("x", config.MaxPromptFileSize+1)), 0o644))
	_, err = LoadDoc(big)
	require.ErrorContains(t, err, "limit")

	// One bad file fails the whole list: a rules file must never be dropped
	// silently.
	good := filepath.Join(dir, "good.md")
	require.NoError(t, os.WriteFile(good, []byte("- fine\n"), 0o644))
	_, err = LoadDocs([]string{good, binary})
	require.Error(t, err)
}

// TestLoadedDocsReachThePrompt wires the loader to the builder: content read
// from disk appears verbatim.
func TestLoadedDocsReachThePrompt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "house-rules.md")
	body := "- Never commit generated files.\n- `make check` must pass."
	require.NoError(t, os.WriteFile(path, []byte(body+"\n"), 0o644))

	docs, err := LoadDocs([]string{path})
	require.NoError(t, err)

	req := pbRequest(domain.RoleEngineer)
	req.Rules = docs
	p, err := Build(req)
	require.NoError(t, err)
	assert.Contains(t, p.Text, "### house-rules.md")
	assert.Contains(t, p.Text, body)
}

func TestRenderDocsSkipsEmptyList(t *testing.T) {
	assert.Empty(t, renderDocs(nil, "preamble"))
	out := renderDocs([]Doc{{Path: "/tmp/x/anon.md", Content: "body"}}, "preamble")
	assert.Contains(t, out, "### anon.md")
	assert.Contains(t, out, "body")
}
