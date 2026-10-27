package hermes

import (
	"bufio"
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

func hermesFixture(t testing.TB, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", "hermes", name))
	require.NoError(t, err)
	_, err = os.Stat(path)
	require.NoError(t, err)
	return path
}

func parseFixture(t testing.TB, name string) (*Parser, []cartridge.SessionEvent) {
	t.Helper()
	file, err := os.Open(hermesFixture(t, name))
	require.NoError(t, err)
	defer file.Close()
	parser := NewParser()
	var events []cartridge.SessionEvent
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		events = append(events, parser.Line(scanner.Text())...)
	}
	require.NoError(t, scanner.Err())
	return parser, events
}

func eventKinds(events []cartridge.SessionEvent) []cartridge.EventKind {
	kinds := make([]cartridge.EventKind, len(events))
	for i, event := range events {
		kinds[i] = event.Kind
	}
	return kinds
}

func TestParserSuccess(t *testing.T) {
	t.Parallel()
	parser, events := parseFixture(t, "success.jsonl")
	assert.Equal(t, []cartridge.EventKind{
		cartridge.EventTextDelta, cartridge.EventToolUse, cartridge.EventToolResult, cartridge.EventTextDelta,
	}, eventKinds(events))
	assert.Equal(t, "terminal", events[1].Tool)
	assert.Equal(t, "call-1", events[1].ToolID)
	assert.Equal(t, "go test ./...", events[1].Input)
	assert.Equal(t, "ok\n", events[2].Output)
	assert.False(t, events[2].IsError)
	assert.Equal(t, "session-123", parser.SessionID())
	assert.Equal(t, "nous/hermes-4", parser.Model())
	assert.Equal(t, "All tests pass.", parser.Output())
	assert.Equal(t, "I will check it.All tests pass.", parser.Transcript())
	assert.Equal(t, cartridge.Usage{InputTokens: 120, OutputTokens: 30, CacheReadTokens: 80, CacheWriteTokens: 12}, parser.Usage())
	assert.NoError(t, Classify(Name, parser, nil, 0))
	result := parser.Result(0, 2*time.Second)
	assert.Equal(t, 2*time.Second, result.Duration)
	assert.Equal(t, "All tests pass.", result.Output)
}

func TestParserPartialStream(t *testing.T) {
	t.Parallel()
	parser := NewParser()
	parser.Line(`{"type":"system","subtype":"init","session_id":"s","model":"m"}`)
	events := parser.Line(`{"type":"text","text":"partial"}`)
	require.Len(t, events, 1)
	assert.Equal(t, "partial", parser.Output())
	assert.Equal(t, cartridge.Usage{}, parser.Usage())
	err := Classify(Name, parser, nil, 0)
	assert.Equal(t, cartridge.KindCrash, cartridge.KindOf(err))
	assert.Contains(t, err.Error(), "without a result line")
}

func TestParserErrorFixtures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		fixture string
		kind    cartridge.Kind
		message string
	}{
		{"auth-error.jsonl", cartridge.KindAuth, "token expired"},
		{"rate-limit-error.jsonl", cartridge.KindRateLimited, "Too many requests"},
		{"credits-error.jsonl", cartridge.KindCreditsExhausted, "insufficient credit"},
		{"max-turns-error.jsonl", cartridge.KindCrash, "Maximum number of turns"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			parser, _ := parseFixture(t, tc.fixture)
			err := Classify(Name, parser, errors.New("exit status 1"), 1)
			require.Error(t, err)
			assert.Equal(t, tc.kind, cartridge.KindOf(err))
			assert.Contains(t, err.Error(), tc.message)
			assert.Equal(t, tc.kind.Retryable(), cartridge.KindOf(err).Retryable())
		})
	}
}

func TestParserNoiseAndToolRendering(t *testing.T) {
	t.Parallel()
	parser := NewParser()
	assert.Empty(t, parser.Line("plain warning"))
	assert.Empty(t, parser.Line("{bad json"))
	assert.Empty(t, parser.Line(`{"type":"unknown","text":"ignored"}`))
	assert.Len(t, parser.noise, 2)

	use := parser.Line(`{"type":"tool_use","name":"other","tool_call_id":"x","input":{"b":"two","a":1}}`)
	require.Len(t, use, 1)
	assert.Equal(t, `a=1 b="two"`, use[0].Input)
	result := parser.Line(`{"type":"tool_result","tool_call_id":"x","output":"failed","is_error":true}`)
	require.Len(t, result, 1)
	assert.Equal(t, "other", result[0].Tool)
	assert.True(t, result[0].IsError)

	long := strings.Repeat("é", maxToolOutput)
	result = parser.Line(fmt.Sprintf(`{"type":"tool_result","name":"terminal","output":%q}`, long))
	require.Len(t, result, 1)
	assert.LessOrEqual(t, len(result[0].Output), maxToolOutput+len("…"))
	assert.True(t, strings.HasPrefix(long, strings.TrimSuffix(result[0].Output, "…")))
}

func TestRenderToolInput(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "ls -la", renderToolInput("terminal", []byte(`{"command":"ls -la"}`)))
	assert.Equal(t, "/tmp/a", renderToolInput("read_file", []byte(`{"path":"/tmp/a"}`)))
	assert.Equal(t, "golang", renderToolInput("web_search", []byte(`{"query":"golang"}`)))
	assert.Equal(t, "plain", renderToolInput("other", []byte(`plain`)))
	assert.Empty(t, renderToolInput("terminal", nil))
}
