package structured

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// structuredRegistry loads testdata/schemas.
func structuredRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := LoadFS(os.DirFS(filepath.Join("testdata", "schemas")))
	require.NoError(t, err)
	return r
}

func TestLoadFSAndRegistry(t *testing.T) {
	r := structuredRegistry(t)
	assert.Equal(t, []string{"item", "verdict"}, r.Names())
	assert.True(t, r.Has("verdict"))
	assert.False(t, r.Has("plan"))

	c, err := r.Get("verdict")
	require.NoError(t, err)
	assert.Equal(t, "verdict", c.Name())
	assert.Contains(t, string(c.SchemaJSON()), `"title": "verdict (test fixture)"`)
	assert.NotPanics(t, func() { r.MustGet("item") })

	_, err = r.Get("plan")
	assert.ErrorIs(t, err, ErrUnknownContract)
	assert.Contains(t, err.Error(), `"plan" (known: item, verdict)`)
	assert.Panics(t, func() { r.MustGet("plan") })
	assert.NotPanics(t, func() { MustLoadFS(os.DirFS(filepath.Join("testdata", "schemas"))) })
}

func TestLoadFSErrors(t *testing.T) {
	t.Run("missing dir", func(t *testing.T) {
		_, err := LoadFS(os.DirFS(filepath.Join(t.TempDir(), "nope")))
		require.Error(t, err)
	})
	t.Run("invalid json schema file", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{"), 0o644))
		_, err := LoadFS(os.DirFS(dir))
		require.ErrorContains(t, err, "bad.json is not valid JSON")
		assert.Panics(t, func() { MustLoadFS(os.DirFS(dir)) })
	})
	t.Run("schema that does not compile", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.json"), []byte(`{"type": "nonsense"}`), 0o644))
		_, err := LoadFS(os.DirFS(dir))
		require.ErrorContains(t, err, "compile schema bad.json")
	})
	t.Run("unresolvable ref", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.json"), []byte(`{"$ref": "missing.json"}`), 0o644))
		_, err := LoadFS(os.DirFS(dir))
		require.Error(t, err)
	})
	t.Run("duplicate names", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.json"), []byte(`{}`), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.schema.json"), []byte(`{}`), 0o644))
		_, err := LoadFS(os.DirFS(dir))
		require.ErrorContains(t, err, `two schema files named "a"`)
	})
	t.Run("subdirectories and non-json files are ignored", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "x.json"), []byte("{"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "ok.schema.json"), []byte(`{"type":"object"}`), 0o644))
		r, err := LoadFS(os.DirFS(dir))
		require.NoError(t, err)
		assert.Equal(t, []string{"ok"}, r.Names())
	})
}

func TestNewContract(t *testing.T) {
	c, err := NewContract("thing", []byte(`{"type": "object", "required": ["n"], "properties": {"n": {"type": "integer", "minimum": 1}}}`))
	require.NoError(t, err)
	assert.Equal(t, "thing", c.Name())
	assert.NoError(t, c.Validate(json.RawMessage(`{"n": 3}`)))
	assert.ErrorIs(t, c.Validate(json.RawMessage(`{"n": 0}`)), ErrSchema)
	assert.ErrorIs(t, c.Validate(json.RawMessage(`{"n": `)), ErrInvalidJSON)

	_, err = NewContract("bad", []byte(`{"type": 12}`))
	require.Error(t, err)
	assert.Panics(t, func() { MustContract("bad", []byte(`{"type": 12}`)) })
	assert.NotPanics(t, func() { MustContract("ok", []byte(`true`)) })
}

func TestContractValidateProblemsAreReadable(t *testing.T) {
	c := structuredRegistry(t).MustGet("verdict")
	err := c.Validate(json.RawMessage(`{"outcome": "maybe", "items": [{"title": "", "severity": "huge"}], "extra": true}`))
	require.Error(t, err)
	pe, ok := AsParseError(err)
	require.True(t, ok)
	assert.Equal(t, ReasonSchema, pe.Reason)
	assert.Equal(t, "verdict", pe.Contract)
	joined := strings.Join(pe.Problems, "\n")
	assert.Contains(t, joined, "at '/outcome'")
	assert.Contains(t, joined, "at '/items/0/title'")
	assert.Contains(t, joined, "at '/items/0/severity'")
	assert.Contains(t, joined, "'extra'")
	assert.NotContains(t, joined, "sc://", "problem lines must not leak schema URLs")
	for _, p := range pe.Problems {
		assert.False(t, strings.Contains(p, "\n"), "one line per problem: %q", p)
	}
	assert.Contains(t, pe.Error(), "violation(s)")
	assert.Contains(t, pe.Feedback(), "at '/outcome'")
}

func TestContractPromptSuffix(t *testing.T) {
	c := structuredRegistry(t).MustGet("verdict")
	s := c.PromptSuffix()
	assert.True(t, strings.HasPrefix(s, "## Output contract (verdict)\n"))
	assert.Contains(t, s, "```sc-result\n{ ... }\n```")
	assert.Contains(t, s, "JSON Schema `verdict`:")
	assert.Contains(t, s, "\"outcome\": {\n")
	assert.Equal(t, s, c.PromptSuffix(), "deterministic")

	prompt := "Review the diff.\n"
	with := c.WithPromptSuffix(prompt)
	assert.True(t, strings.HasPrefix(with, "Review the diff.\n\n## Output contract"))
	assert.True(t, c.HasPromptSuffix(with))
	assert.False(t, c.HasPromptSuffix(prompt))
	assert.Equal(t, with, c.WithPromptSuffix(with), "never appended twice")
	assert.Equal(t, 1, strings.Count(c.WithPromptSuffix(c.WithPromptSuffix(prompt)), "## Output contract"))
}

func TestContractParseAndDecode(t *testing.T) {
	c := structuredRegistry(t).MustGet("verdict")
	type item struct {
		Title    string `json:"title"`
		Severity string `json:"severity"`
		File     string `json:"file"`
	}
	type verdict struct {
		Outcome string `json:"outcome"`
		Summary string `json:"summary"`
		Items   []item `json:"items"`
	}

	cases := map[string]struct {
		wantErr error
		outcome string
		items   int
	}{
		"valid":             {outcome: "approve"},
		"prose-wrapped":     {outcome: "request_changes", items: 1},
		"double-block":      {outcome: "request_changes", items: 1},
		"unclosed-complete": {outcome: "approve"},
		"json-fence":        {outcome: "approve"},
		"bare":              {outcome: "approve", items: 1},
		"tilde-indented":    {outcome: "approve"},
		"truncated":         {wantErr: ErrTruncated},
		"missing":           {wantErr: ErrMissing},
		"invalid-json":      {wantErr: ErrInvalidJSON},
		"two-values":        {wantErr: ErrInvalidJSON},
		"schema-violation":  {wantErr: ErrSchema},
	}
	for file, tc := range cases {
		t.Run(file, func(t *testing.T) {
			v, p, err := Decode[verdict](c, structuredOutput(t, file))
			assert.Equal(t, "verdict", p.Contract)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				pe, ok := AsParseError(err)
				require.True(t, ok)
				assert.Equal(t, "verdict", pe.Contract, "parse errors carry the contract name")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.outcome, v.Outcome)
			assert.Len(t, v.Items, tc.items)
			assert.NotEmpty(t, p.JSON)
		})
	}

	t.Run("decode type mismatch after a valid parse", func(t *testing.T) {
		_, _, err := Decode[struct {
			Outcome int `json:"outcome"`
		}](c, structuredOutput(t, "valid"))
		assert.ErrorIs(t, err, ErrInvalidJSON)
	})
}

// TestAgentSchemasCompile guards the real schema directory (P5-07): every
// file there must load through this package. It passes vacuously until
// schemas exist.
func TestAgentSchemasCompile(t *testing.T) {
	dir := filepath.Join("..", "..", "agents", "schemas")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("no schema dir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	if n == 0 {
		t.Skip("no schema files yet (P5-07)")
	}
	r, err := LoadFS(os.DirFS(dir))
	require.NoError(t, err)
	assert.Len(t, r.Names(), n)
	for _, name := range r.Names() {
		assert.NotEmpty(t, r.MustGet(name).PromptSuffix())
	}
}
