package structured

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// structuredOutput reads testdata/outputs/<name>.txt.
func structuredOutput(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "outputs", name+".txt"))
	require.NoError(t, err)
	return string(b)
}

func TestExtractTable(t *testing.T) {
	tests := []struct {
		name     string
		file     string
		wantErr  error
		reason   Reason
		source   Source
		blocks   int
		unclosed bool
		outcome  string
	}{
		{name: "valid", file: "valid", source: SourceFence, blocks: 1, outcome: "approve"},
		{name: "prose wrapped", file: "prose-wrapped", source: SourceFence, blocks: 1, outcome: "request_changes"},
		{name: "double block takes the last", file: "double-block", source: SourceFence, blocks: 2, outcome: "request_changes"},
		{name: "truncated", file: "truncated", wantErr: ErrTruncated, reason: ReasonTruncated, blocks: 1},
		{name: "unclosed but complete", file: "unclosed-complete", source: SourceFence, blocks: 1, unclosed: true, outcome: "approve"},
		{name: "missing", file: "missing", wantErr: ErrMissing, reason: ReasonMissing},
		{name: "invalid json", file: "invalid-json", wantErr: ErrInvalidJSON, reason: ReasonInvalidJSON, blocks: 1},
		{name: "two values in one block", file: "two-values", wantErr: ErrInvalidJSON, reason: ReasonInvalidJSON, blocks: 1},
		{name: "json fence fallback", file: "json-fence", source: SourceJSONFence, outcome: "approve"},
		{name: "bare object fallback", file: "bare", source: SourceBare, outcome: "approve"},
		{name: "tilde fence, indented", file: "tilde-indented", source: SourceFence, blocks: 1, outcome: "approve"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := Extract(structuredOutput(t, tc.file))
			if tc.wantErr != nil {
				require.Error(t, err)
				assert.ErrorIs(t, err, tc.wantErr)
				pe, ok := AsParseError(err)
				require.True(t, ok, "error must be a *ParseError")
				assert.Equal(t, tc.reason, pe.Reason)
				assert.NotEmpty(t, pe.Feedback())
				assert.Equal(t, tc.blocks, b.Blocks)
				assert.Empty(t, b.JSON)
				return
			}
			require.NoError(t, err)
			assert.True(t, json.Valid(b.JSON), "JSON must be valid: %s", b.JSON)
			assert.Equal(t, tc.source, b.Source)
			assert.Equal(t, tc.blocks, b.Blocks)
			assert.Equal(t, tc.unclosed, b.Unclosed)
			var v struct {
				Outcome string `json:"outcome"`
			}
			require.NoError(t, b.Decode(&v))
			assert.Equal(t, tc.outcome, v.Outcome)
		})
	}
}

func TestExtractEdgeCases(t *testing.T) {
	t.Run("empty output is missing", func(t *testing.T) {
		_, err := Extract("")
		assert.ErrorIs(t, err, ErrMissing)
	})
	t.Run("crlf line endings", func(t *testing.T) {
		b, err := Extract("hi\r\n```sc-result\r\n{\"a\": 1}\r\n```\r\n")
		require.NoError(t, err)
		assert.JSONEq(t, `{"a":1}`, string(b.JSON))
	})
	t.Run("info string with trailing words", func(t *testing.T) {
		b, err := Extract("```sc-result json\n{\"a\": 1}\n```")
		require.NoError(t, err)
		assert.Equal(t, SourceFence, b.Source)
	})
	t.Run("longer fences close only on a fence at least as long", func(t *testing.T) {
		b, err := Extract("````sc-result\n{\"a\": 1}\n````\n")
		require.NoError(t, err)
		assert.JSONEq(t, `{"a":1}`, string(b.JSON))
		// A shorter fence inside the block is body text, which makes the
		// body two "values" and therefore invalid JSON.
		_, err = Extract("````sc-result\n{\"a\": 1}\n```\n````\n")
		assert.ErrorIs(t, err, ErrInvalidJSON)
	})
	t.Run("sc-result fence wins over a later json fence", func(t *testing.T) {
		b, err := Extract("```sc-result\n{\"a\": 1}\n```\n```json\n{\"a\": 2}\n```\n")
		require.NoError(t, err)
		assert.JSONEq(t, `{"a":1}`, string(b.JSON))
		assert.Equal(t, SourceFence, b.Source)
	})
	t.Run("unclosed block with trailing prose keeps the first value", func(t *testing.T) {
		b, err := Extract("```sc-result\n{\"a\": 1}\nthanks for reading\n")
		require.NoError(t, err)
		assert.True(t, b.Unclosed)
		assert.JSONEq(t, `{"a":1}`, string(b.JSON))
	})
	t.Run("non-object json fence is ignored by the bare fallback", func(t *testing.T) {
		_, err := Extract("```json\n[1, 2]\n```\nno objects here")
		// The json fence holds a valid value, so it is used as-is; the schema decides.
		require.NoError(t, err)
	})
	t.Run("braces in prose that are not json are missing", func(t *testing.T) {
		_, err := Extract("use {braces} like this { and } that")
		assert.ErrorIs(t, err, ErrMissing)
	})
	t.Run("bare fallback takes the last complete top-level object", func(t *testing.T) {
		b, err := Extract("first {\"a\": 1} then {\"a\": {\"nested\": 2}} finally {\"a\": broken")
		require.NoError(t, err)
		assert.Equal(t, SourceBare, b.Source)
		assert.JSONEq(t, `{"a":{"nested":2}}`, string(b.JSON))
	})
	t.Run("decode of an empty block fails", func(t *testing.T) {
		var v map[string]any
		assert.ErrorIs(t, Block{}.Decode(&v), ErrMissing)
	})
	t.Run("decode type mismatch is invalid json", func(t *testing.T) {
		var v struct {
			A string `json:"a"`
		}
		assert.ErrorIs(t, Block{JSON: json.RawMessage(`{"a": 1}`)}.Decode(&v), ErrInvalidJSON)
	})
}

func TestParseErrorRendering(t *testing.T) {
	pe := &ParseError{Reason: ReasonSchema, Contract: "verdict", Detail: "2 violation(s)", Problems: []string{"at '/outcome': value must be one of 'approve', 'request_changes'", "at '/items/0': missing property 'severity'"}, Cause: errors.New("x")}
	assert.Equal(t, "structured: result does not match schema (verdict): 2 violation(s)\n  - at '/outcome': value must be one of 'approve', 'request_changes'\n  - at '/items/0': missing property 'severity'", pe.Error())
	assert.Contains(t, pe.Feedback(), `did not match the "verdict" schema:`)
	assert.Contains(t, pe.Feedback(), "\n- at '/items/0': missing property 'severity'")
	assert.NotContains(t, pe.Feedback(), "2 violation(s)", "the count is a log detail, not model feedback")
	assert.ErrorIs(t, pe, ErrSchema)
	assert.NotErrorIs(t, pe, ErrMissing)
	assert.Equal(t, "x", errors.Unwrap(pe).Error())

	wrapped := errors.Join(errors.New("outer"), &ParseError{Reason: ReasonTruncated, Detail: "eof"})
	got, ok := AsParseError(wrapped)
	require.True(t, ok)
	assert.Equal(t, ReasonTruncated, got.Reason)
	assert.Contains(t, got.Feedback(), "(eof)")
	_, ok = AsParseError(errors.New("plain"))
	assert.False(t, ok)

	assert.Contains(t, (&ParseError{Reason: ReasonMissing}).Feedback(), "no ```sc-result fenced block")
	assert.Contains(t, (&ParseError{Reason: ReasonInvalidJSON}).Feedback(), "not contain valid JSON")
	assert.Contains(t, (&ParseError{Reason: "other"}).Feedback(), "usable result")
	assert.Equal(t, "structured: other", (&ParseError{Reason: "other"}).Error())
}

func TestReasons(t *testing.T) {
	assert.Equal(t, []Reason{ReasonMissing, ReasonTruncated, ReasonInvalidJSON, ReasonSchema}, Reasons())
	for _, r := range Reasons() {
		assert.True(t, r.Valid(), r)
		assert.NotNil(t, r.Sentinel(), r)
		assert.Equal(t, string(r), r.String())
	}
	assert.False(t, Reason("nope").Valid())
	assert.Nil(t, Reason("nope").Sentinel())
}
