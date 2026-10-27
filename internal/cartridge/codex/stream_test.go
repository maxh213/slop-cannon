package codex

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
)

func codexFixture(t testing.TB, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", "codex", name))
	require.NoError(t, err)
	_, err = os.Stat(path)
	require.NoError(t, err)
	return path
}

func parseFixture(t testing.TB, name string) (*Parser, []cartridge.SessionEvent) {
	t.Helper()
	file, err := os.Open(codexFixture(t, name))
	require.NoError(t, err)
	defer file.Close()
	p := NewParser()
	var events []cartridge.SessionEvent
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		events = append(events, p.Line(scanner.Text())...)
	}
	require.NoError(t, scanner.Err())
	return p, events
}

func eventKinds(events []cartridge.SessionEvent) []cartridge.EventKind {
	out := make([]cartridge.EventKind, len(events))
	for i, event := range events {
		out[i] = event.Kind
	}
	return out
}

func TestParser_AllEventFamilies(t *testing.T) {
	t.Parallel()
	p, events := parseFixture(t, "success.jsonl")

	assert.Equal(t, []cartridge.EventKind{
		cartridge.EventToolUse, cartridge.EventToolResult,
		cartridge.EventToolUse, cartridge.EventToolResult,
		cartridge.EventToolUse, cartridge.EventToolResult,
		cartridge.EventToolUse, cartridge.EventToolResult,
		cartridge.EventTextDelta,
	}, eventKinds(events))
	assert.Equal(t, "thread-123", p.SessionID())
	assert.Equal(t, "IMPLEMENTED", p.Output())
	assert.True(t, p.HasResult())
	assert.Equal(t, cartridge.Usage{
		InputTokens: 1200, OutputTokens: 42,
		CacheReadTokens: 800, CacheWriteTokens: 100, Turns: 1,
	}, p.Usage())
	assert.Equal(t, 17, p.ReasoningTokens())
	assert.NoError(t, Classify(Name, p, nil, 0))

	assert.Equal(t, "command_execution", events[0].Tool)
	assert.Equal(t, "go test ./...", events[0].Input)
	assert.Equal(t, "cmd-1", events[0].ToolID)
	assert.Equal(t, "ok ./pkg\n", events[1].Output)
	assert.False(t, events[1].IsError)
	assert.Equal(t, "file_change", events[2].Tool)
	assert.Contains(t, events[2].Input, "pkg/a.go")
	assert.Equal(t, "docs.lookup", events[4].Tool)
	assert.Contains(t, events[4].Input, `"query":"codex"`)
	assert.Contains(t, events[5].Output, "found")
	assert.Equal(t, "web_search", events[6].Tool)
	assert.Equal(t, "Codex JSON events", events[6].Input)
	assert.Equal(t, "IMPLEMENTED", events[8].Text)

	result := p.Result(0, 2*time.Second)
	assert.Equal(t, "IMPLEMENTED", result.Output)
	assert.Equal(t, "thread-123", result.SessionID)
	assert.Equal(t, 2*time.Second, result.Duration)
}

func TestParser_CommandFailureIsToolErrorNotTurnFailure(t *testing.T) {
	t.Parallel()
	p, events := parseFixture(t, "command-failed.jsonl")
	require.Len(t, events, 3)
	assert.True(t, events[1].IsError)
	assert.Equal(t, "missing\n", events[1].Output)
	assert.Equal(t, "TEST FAILED", p.Output())
	assert.NoError(t, Classify(Name, p, nil, 0))
}

func TestParser_TurnFailed(t *testing.T) {
	t.Parallel()
	p, _ := parseFixture(t, "turn-failed.jsonl")
	err := Classify(Name, p, nil, 0)
	require.Error(t, err)
	assert.Equal(t, cartridge.KindRateLimited, cartridge.KindOf(err))
	assert.Contains(t, err.Error(), "usage limit")
}

func TestParser_TopLevelError(t *testing.T) {
	t.Parallel()
	p, _ := parseFixture(t, "top-level-error.jsonl")
	err := Classify(Name, p, nil, 0)
	require.Error(t, err)
	assert.Equal(t, cartridge.KindAuth, cartridge.KindOf(err))
	assert.Contains(t, err.Error(), "invalid API key")
}

func TestParser_MalformedOrMissingTerminal(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		fixture string
		want    string
	}{
		{"missing usage", "malformed-terminal.jsonl", "missing usage"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, _ := parseFixture(t, test.fixture)
			err := Classify(Name, p, nil, 0)
			require.Error(t, err)
			assert.Equal(t, cartridge.KindCrash, cartridge.KindOf(err))
			assert.Contains(t, err.Error(), test.want)
		})
	}

	p := NewParser()
	p.Line(`{"type":"thread.started","thread_id":"t"}`)
	p.Line(`{"type":"turn.started"}`)
	p.Line(`{"type":"item.completed","item":{"id":"m","type":"agent_message","text":"partial"}}`)
	p.Line(`{"type":"turn.completed","usage":`)
	err := Classify(Name, p, nil, 0)
	assert.Equal(t, cartridge.KindCrash, cartridge.KindOf(err))
	assert.Contains(t, err.Error(), "without a successful turn.completed")
	assert.Equal(t, "partial", p.Output())

	p = NewParser()
	p.Line(`{"type":"turn.completed","usage":{}}`)
	err = Classify(Name, p, nil, 0)
	assert.Equal(t, cartridge.KindCrash, cartridge.KindOf(err))
	assert.Contains(t, err.Error(), "missing, negative or invalid core token usage")

	p = NewParser()
	p.Line(`{"type":"turn.completed","usage":{"input_tokens":-1,"output_tokens":0}}`)
	err = Classify(Name, p, nil, 0)
	assert.Equal(t, cartridge.KindCrash, cartridge.KindOf(err))
	assert.Contains(t, err.Error(), "negative")
}

func TestParser_CompletedTurnDoesNotHideExitOrError(t *testing.T) {
	t.Parallel()
	p, _ := parseFixture(t, "success.jsonl")
	assert.Equal(t, cartridge.KindCrash, cartridge.KindOf(Classify(Name, p, errors.New("exit 1"), 1)))
	p.Line(`{"type":"error","message":"stream failed after terminal"}`)
	assert.Equal(t, cartridge.KindCrash, cartridge.KindOf(Classify(Name, p, nil, 0)))
}

func TestUsageAliases(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		line      string
		cache     int
		reasoning int
	}{
		{"cache creation", `{"type":"turn.completed","usage":{"cache_creation_input_tokens":11,"reasoning_tokens":7}}`, 11, 7},
		{"cache write", `{"type":"turn.completed","usage":{"cache_write_tokens":12,"reasoning_output_tokens":8}}`, 12, 8},
		{"cache write input", `{"type":"turn.completed","usage":{"cache_write_input_tokens":13,"reasoning_output_tokens":9}}`, 13, 9},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := NewParser()
			p.Line(test.line)
			assert.Equal(t, test.cache, p.Usage().CacheWriteTokens)
			assert.Equal(t, test.reasoning, p.ReasoningTokens())
		})
	}
}

func TestParser_MissingAgentAndReasoningItemsAreSafe(t *testing.T) {
	t.Parallel()
	p := NewParser()
	assert.Empty(t, p.Line(`{"type":"item.started"}`))
	assert.Empty(t, p.Line(`{"type":"item.completed"}`))
	assert.Empty(t, p.Line(`{"type":"item.completed","item":{"id":"r","type":"reasoning","text":"hidden"}}`))
	assert.Empty(t, p.Output())
	assert.Empty(t, p.Line("not json"))
	assert.Len(t, p.noise, 1)
}
