package junie

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParserRequiresOneFinalJSONResult(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", "without a final JSON result"},
		{"malformed", `{"result":`, "malformed final JSON"},
		{"missing result", `{"sessionId":"s"}`, "no result field"},
		{"trailing output", `{"result":"OK"} trailing`, "malformed final JSON"},
		{"two objects", "{\"result\":\"one\"}\n{\"result\":\"two\"}", "malformed final JSON"},
		{"null", `null`, "no result field"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := NewParser()
			p.Stdout(test.input)
			err := Classify(Name, p, nil, 0)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.want)
		})
	}
}

func TestParserRejectsErrorsEvenWhenResultExists(t *testing.T) {
	t.Parallel()
	p := NewParser()
	p.Stdout(`{"result":"partial","errors":["task failed"]}`)
	err := Classify(Name, p, nil, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "task failed")
	assert.Equal(t, "partial", p.Result(0, 0).Output)
}

func TestSumUsageRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	for _, entries := range [][]llmUsage{
		{{Calls: -1}},
		{{InputTokens: -1}},
		{{CacheInputTokens: -1}},
		{{CacheCreateTokens: -1}},
		{{OutputTokens: -1}},
	} {
		_, err := sumUsage(entries)
		require.Error(t, err)
	}
	cost := -0.1
	_, err := sumUsage([]llmUsage{{Cost: &cost}})
	require.Error(t, err)
}
