package grok

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
)

func grokFixture(t testing.TB, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", "grok", name))
	require.NoError(t, err)
	_, err = os.Stat(path)
	require.NoError(t, err)
	return path
}

func grokParseFixture(t testing.TB, name string) (*Parser, []cartridge.SessionEvent) {
	t.Helper()
	f, err := os.Open(grokFixture(t, name))
	require.NoError(t, err)
	defer f.Close()
	p := NewParser()
	var events []cartridge.SessionEvent
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		events = append(events, p.Line(scanner.Text())...)
	}
	require.NoError(t, scanner.Err())
	return p, events
}

func grokKinds(events []cartridge.SessionEvent) []cartridge.EventKind {
	out := make([]cartridge.EventKind, len(events))
	for i, ev := range events {
		out[i] = ev.Kind
	}
	return out
}

func TestParserSuccess(t *testing.T) {
	t.Parallel()
	p, events := grokParseFixture(t, "stream-success.jsonl")
	require.Equal(t, []cartridge.EventKind{cartridge.EventToolUse, cartridge.EventToolResult, cartridge.EventTextDelta, cartridge.EventTextDelta}, grokKinds(events))
	assert.Equal(t, "read_file", events[0].Tool)
	assert.Equal(t, "call_1", events[0].ToolID)
	assert.Equal(t, "/work/hello.txt", events[0].Input)
	assert.Equal(t, "read_file", events[1].Tool)
	assert.Equal(t, "hello from disk", events[1].Output)
	assert.False(t, events[1].IsError)
	assert.Equal(t, "Hello world", p.Output())
	assert.Equal(t, "abc", p.SessionID())
	assert.True(t, p.HasEnd())
	assert.Equal(t, cartridge.Usage{InputTokens: 10, OutputTokens: 4, CacheReadTokens: 7, CacheWriteTokens: 3, CostUSD: 0.012, HasCost: true, Turns: 2}, p.Usage())
	assert.NoError(t, Classify(Name, p, nil, 0))
	result := p.Result(0, 2*time.Second)
	assert.Equal(t, "Hello world", result.Output)
	assert.Equal(t, 2*time.Second, result.Duration)
}

func TestParserCurrentAliasesAndZeroCost(t *testing.T) {
	t.Parallel()
	p, events := grokParseFixture(t, "stream-aliases.jsonl")
	require.Equal(t, []cartridge.EventKind{cartridge.EventToolUse, cartridge.EventToolResult, cartridge.EventTextDelta}, grokKinds(events))
	assert.Equal(t, "run_terminal_cmd", events[0].Tool)
	assert.Equal(t, "t2", events[0].ToolID)
	assert.Equal(t, "go test ./...", events[0].Input)
	assert.True(t, events[1].IsError)
	assert.Equal(t, "exit status 1", events[1].Output)
	assert.Equal(t, "Tests failed", p.Output())
	assert.Equal(t, "alias-session", p.SessionID())
	assert.Equal(t, cartridge.Usage{InputTokens: 11, OutputTokens: 6, CacheReadTokens: 4, CacheWriteTokens: 1, Turns: 2, HasCost: true}, p.Usage())
}

func TestParserIncompleteRequiresEndAndDropsCost(t *testing.T) {
	t.Parallel()
	p, events := grokParseFixture(t, "stream-incomplete.jsonl")
	require.Len(t, events, 1)
	assert.False(t, p.HasEnd())
	assert.Equal(t, "partial answer", p.Output())
	assert.Equal(t, cartridge.Usage{InputTokens: 12, OutputTokens: 5, Turns: 1}, p.Usage())
	assert.False(t, p.Usage().HasCost)
	assert.Zero(t, p.Usage().CostUSD)
	err := Classify(Name, p, nil, 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, cartridge.ErrCrash)
	assert.Contains(t, err.Error(), "without an end event")
}

func TestParserCompleteEndWithIncompleteSpendDropsCost(t *testing.T) {
	t.Parallel()
	for _, marker := range []string{`"usage_is_incomplete":true`, `"cost_is_partial":true`} {
		p := NewParser()
		p.Line(fmt.Sprintf(`{"type":"end","stopReason":"end_turn","usage":{"input_tokens":9,"output_tokens":2},"total_cost_usd":0.5,%s}`, marker))
		assert.True(t, p.HasEnd())
		assert.NoError(t, Classify(Name, p, nil, 0))
		assert.Equal(t, cartridge.Usage{InputTokens: 9, OutputTokens: 2}, p.Usage())
		assert.False(t, p.Usage().HasCost)
		assert.Zero(t, p.Usage().CostUSD)
	}
}

func TestParserIgnoresUnknownGarbageAndNonterminalToolUpdates(t *testing.T) {
	t.Parallel()
	p := NewParser()
	assert.Empty(t, p.Line(""))
	assert.Empty(t, p.Line("warning"))
	assert.Empty(t, p.Line("{broken"))
	assert.Empty(t, p.Line(`{"type":"future_event","data":"ignored"}`))
	assert.Len(t, p.Line(`{"type":"tool_call","toolCallId":"x","toolName":"read_file"}`), 1)
	assert.Empty(t, p.Line(`{"type":"tool_update","toolCallId":"x","status":"in_progress","output":"partial"}`))
	assert.Equal(t, 6, p.Lines())
	assert.Equal(t, []string{"warning", "{broken"}, p.noise)
	assert.False(t, p.HasEnd())
}

func TestParserErrorFixtures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		fixture string
		kind    cartridge.Kind
	}{
		{"stream-auth.jsonl", cartridge.KindAuth},
		{"stream-credits.jsonl", cartridge.KindCreditsExhausted},
		{"stream-rate.jsonl", cartridge.KindRateLimited},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			p, events := grokParseFixture(t, tc.fixture)
			assert.Empty(t, events)
			assert.False(t, p.HasEnd())
			err := Classify("grok-custom", p, nil, 0)
			require.Error(t, err)
			assert.Equal(t, tc.kind, cartridge.KindOf(err))
			var ce *cartridge.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, "grok-custom", ce.Cartridge)
		})
	}
}

func TestClassifyProcessAndEndFailures(t *testing.T) {
	t.Parallel()
	p := NewParser()
	p.Line(`{"type":"end","stopReason":"error","message":"HTTP 401 unauthorized"}`)
	assert.Equal(t, cartridge.KindAuth, cartridge.KindOf(Classify(Name, p, nil, 0)))

	p = NewParser()
	p.Line(`{"type":"end","stopReason":"end_turn"}`)
	assert.Equal(t, cartridge.KindCrash, cartridge.KindOf(Classify(Name, p, errors.New("exit 1"), 1)))

	p = NewParser()
	p.Stderr("Your credit balance has insufficient credit")
	assert.Equal(t, cartridge.KindCreditsExhausted, cartridge.KindOf(Classify(Name, p, errors.New("exit 1"), 1)))
	assert.Equal(t, cartridge.KindTimeout, cartridge.KindOf(Classify(Name, p, context.DeadlineExceeded, 0)))
	assert.Equal(t, cartridge.KindKilled, cartridge.KindOf(Classify(Name, p, context.Canceled, 0)))
}

func TestKindForTextPrecedence(t *testing.T) {
	t.Parallel()
	assert.Equal(t, cartridge.KindCreditsExhausted, KindForText("quota exceeded; HTTP 429 rate limit"))
	assert.Equal(t, cartridge.KindAuth, KindForText("invalid api key"))
	assert.Equal(t, cartridge.KindRateLimited, KindForText("RESOURCE_EXHAUSTED"))
	assert.Equal(t, cartridge.KindUnknown, KindForText("segfault"))
}

func TestParserTruncatesToolPayloads(t *testing.T) {
	t.Parallel()
	p := NewParser()
	big := strings.Repeat("é", maxToolOutput)
	use := p.Line(fmt.Sprintf(`{"type":"tool","toolCallId":"x","toolName":"other","input":{"z":%q,"a":1}}`, big))
	require.Len(t, use, 1)
	assert.LessOrEqual(t, len(use[0].Input), maxToolInput+len("…"))
	result := p.Line(fmt.Sprintf(`{"type":"tool_update","toolCallId":"x","status":"completed","output":%q}`, big))
	require.Len(t, result, 1)
	assert.LessOrEqual(t, len(result[0].Output), maxToolOutput+len("…"))
	assert.Contains(t, result[0].Output, "…")
}
