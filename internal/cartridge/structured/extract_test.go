package structured

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
	assert.Equal(t, "structured: result does not match schema (verdict): 2 violation(s)"+
		"\n  - at '/outcome': value must be one of 'approve', 'request_changes'"+
		"\n  - at '/items/0': missing property 'severity'"+
		"\n  cause: x", pe.Error(), "P9-31: a cause whose words are nowhere else is rendered")
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

// multiViolationContract has five required properties with five different
// types, so one bad document breaks several keywords at once — the shape
// that used to come back in a different order on every run (P2-12).
const multiViolationContract = `{
  "type": "object",
  "required": ["a", "b", "c", "d", "e"],
  "properties": {
    "a": {"type": "string"},
    "b": {"type": "integer"},
    "c": {"type": "string"},
    "d": {"type": "string"},
    "e": {"type": "string"}
  }
}`

// TestProblemsAreSortedAndStable pins P2-12: jsonschema v6 walks the
// schema's keyword maps, so repeated validations of one document used to
// report the same violations in a different order. Every caller that shows
// Problems — a golden, a log line, a retry prompt — depends on one order,
// so leafProblems sorts.
func TestProblemsAreSortedAndStable(t *testing.T) {
	t.Parallel()
	const doc = `{"a": 1, "b": "x", "c": 2}`
	want := []string{
		"at '': missing properties 'd', 'e'",
		"at '/a': got number, want string",
		"at '/b': got string, want integer",
		"at '/c': got number, want string",
	}
	// A fresh contract per round: the compiler's own maps are re-walked too.
	for i := range 200 {
		c, err := NewContract("multi", []byte(multiViolationContract))
		require.NoError(t, err)
		pe, ok := AsParseError(c.Validate(json.RawMessage(doc)))
		require.True(t, ok, "round %d", i)
		assert.Equal(t, want, pe.Problems, "round %d: the problem order must not depend on map iteration", i)
		assert.True(t, sort.StringsAreSorted(pe.Problems), "round %d", i)
	}
}

// TestProblemsAreDeduplicated keeps the property the pre-P2-12 code had:
// two branches of the tree that fail the same way are one line.
func TestProblemsAreDeduplicated(t *testing.T) {
	t.Parallel()
	c, err := NewContract("dupes", []byte(`{
		"type": "object",
		"properties": {"n": {"anyOf": [{"type": "string"}, {"type": "string"}]}}
	}`))
	require.NoError(t, err)
	pe, ok := AsParseError(c.Validate(json.RawMessage(`{"n": 1}`)))
	require.True(t, ok)
	seen := map[string]int{}
	for _, p := range pe.Problems {
		seen[p]++
	}
	for p, n := range seen {
		assert.Equal(t, 1, n, "problem %q is repeated", p)
	}
	assert.Equal(t, len(pe.Problems), len(seen))
	assert.Contains(t, pe.Detail, strconv.Itoa(len(pe.Problems)), "the detail counts the lines it printed")
}

// TestErrorRendersACauseNothingElseSays pins P9-31: a cause whose words are
// not repeated by Detail or Problems reaches the log line, so a wrapped
// sentinel is readable and not merely matchable with errors.Is.
func TestErrorRendersACauseNothingElseSays(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("agents: plan rejected")

	t.Run("a wrapped sentinel is rendered", func(t *testing.T) {
		t.Parallel()
		pe := &ParseError{
			Reason:   ReasonSchema,
			Contract: "plan",
			Detail:   "the plan breaks a planning rule",
			Problems: []string{"at '/beads/0': the acceptance criteria are empty"},
			Cause:    fmt.Errorf("%w: bead \"a\" has no acceptance criteria", sentinel),
		}
		got := pe.Error()
		assert.Contains(t, got, "agents: plan rejected", "the sentinel's words must survive into the log line")
		assert.True(t, strings.HasPrefix(got, "structured: result does not match schema (plan): the plan breaks a planning rule"))
		assert.Contains(t, got, "\n  cause: ")
		assert.ErrorIs(t, pe, sentinel)
		assert.ErrorIs(t, pe, ErrSchema)
	})

	t.Run("a multi-line cause is indented under its first line", func(t *testing.T) {
		t.Parallel()
		pe := &ParseError{Reason: ReasonSchema, Detail: "two rules", Cause: errors.New("first rule\nsecond rule")}
		assert.Equal(t, "structured: result does not match schema: two rules\n  cause: first rule\n    second rule", pe.Error())
	})

	t.Run("a cause the detail already quotes is not printed twice", func(t *testing.T) {
		t.Parallel()
		cause := errors.New("invalid character 'h' looking for beginning of value")
		pe := &ParseError{Reason: ReasonInvalidJSON, Contract: "impl_report", Detail: cause.Error(), Cause: cause}
		assert.Equal(t, "structured: sc-result block is not valid JSON (impl_report): "+cause.Error(), pe.Error())
		assert.Equal(t, 1, strings.Count(pe.Error(), "invalid character"))
	})

	t.Run("a cause the problems already list is not printed twice", func(t *testing.T) {
		t.Parallel()
		cause := errors.New("the responses answer no item")
		pe := &ParseError{Reason: ReasonSchema, Contract: "response", Detail: cause.Error(),
			Problems: []string{cause.Error()}, Cause: cause}
		assert.Equal(t, 2, strings.Count(pe.Error(), cause.Error()), "the detail and the problem line, and no third copy")
		assert.NotContains(t, pe.Error(), "cause:")
	})

	t.Run("the validator's own tree is never repeated", func(t *testing.T) {
		t.Parallel()
		c, err := NewContract("multi", []byte(multiViolationContract))
		require.NoError(t, err)
		pe, ok := AsParseError(c.Validate(json.RawMessage(`{"a": 1, "b": "x", "c": 2}`)))
		require.True(t, ok)
		require.NotNil(t, pe.Cause, "the validation error stays reachable through Unwrap")
		got := pe.Error()
		assert.NotContains(t, got, "cause:")
		assert.NotContains(t, got, BaseURL, "the internal schema URL is not user-facing")
		for _, p := range pe.Problems {
			assert.Equal(t, 1, strings.Count(got, p), "problem %q is printed once", p)
		}
	})

	t.Run("no cause renders as before", func(t *testing.T) {
		t.Parallel()
		pe := &ParseError{Reason: ReasonMissing, Detail: "empty block"}
		assert.Equal(t, "structured: no sc-result block in output: empty block", pe.Error())
	})
}
