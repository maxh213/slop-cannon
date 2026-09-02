package claude

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

// claudeFixture returns the absolute path of a file in testdata/claude.
func claudeFixture(t testing.TB, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", "claude", name))
	require.NoError(t, err)
	_, err = os.Stat(p)
	require.NoError(t, err, "fixture %s", name)
	return p
}

// claudeParseFixture feeds every line of a fixture through a fresh parser
// and returns it with the events produced.
func claudeParseFixture(t testing.TB, name string) (*Parser, []cartridge.SessionEvent) {
	t.Helper()
	f, err := os.Open(claudeFixture(t, name))
	require.NoError(t, err)
	defer f.Close()
	p := NewParser()
	var evs []cartridge.SessionEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		evs = append(evs, p.Line(sc.Text())...)
	}
	require.NoError(t, sc.Err())
	return p, evs
}

func claudeKinds(evs []cartridge.SessionEvent) []cartridge.EventKind {
	out := make([]cartridge.EventKind, len(evs))
	for i, ev := range evs {
		out[i] = ev.Kind
	}
	return out
}

func TestParser_TextReply(t *testing.T) {
	t.Parallel()
	p, evs := claudeParseFixture(t, "text-reply.jsonl")

	assert.Equal(t, []cartridge.EventKind{cartridge.EventTextDelta}, claudeKinds(evs))
	assert.Equal(t, "PONG", evs[0].Text)
	assert.Equal(t, "PONG", p.Output())
	assert.Equal(t, "PONG", p.Transcript())
	assert.Equal(t, "78cf1c28-8e85-41e1-8948-53e0cacf1ce6", p.SessionID())
	assert.Equal(t, "claude-haiku-4-5-20251001", p.Model())
	assert.True(t, p.HasResult())
	assert.Equal(t, 0, p.Denials())
	assert.Equal(t, 9, p.Lines())

	u := p.Usage()
	assert.Equal(t, cartridge.Usage{
		InputTokens: 10, OutputTokens: 50, CacheReadTokens: 13615, CacheWriteTokens: 7029,
		CostUSD: 0.0166345, HasCost: true, Turns: 1,
	}, u)

	// The stray plain-text line the CLI printed after the result is noise.
	require.Len(t, p.noise, 1)
	assert.Contains(t, p.noise[0], "Client.listTools()")

	assert.NoError(t, Classify("claude", p, nil, 0))
	res := p.Result(0, 2*time.Second)
	assert.Equal(t, "PONG", res.Output)
	assert.Equal(t, 2*time.Second, res.Duration)
	assert.Equal(t, p.SessionID(), res.SessionID)
}

func TestParser_ToolUseRead(t *testing.T) {
	t.Parallel()
	p, evs := claudeParseFixture(t, "tool-use-read.jsonl")

	require.Equal(t, []cartridge.EventKind{cartridge.EventToolUse, cartridge.EventToolResult, cartridge.EventTextDelta}, claudeKinds(evs))
	assert.Equal(t, "Read", evs[0].Tool)
	assert.Equal(t, "toolu_014U6ebBNyULVd32yBs3FiKp", evs[0].ToolID)
	assert.Equal(t, "/work/wt/hello.txt", evs[0].Input)
	assert.Equal(t, "Read", evs[1].Tool, "tool results are labelled with the tool_use's name")
	assert.Equal(t, evs[0].ToolID, evs[1].ToolID)
	assert.Equal(t, "1\tthe quick brown fox\n2\t", evs[1].Output)
	assert.False(t, evs[1].IsError)
	assert.Equal(t, "the quick brown fox", evs[2].Text)

	assert.Equal(t, "the quick brown fox", p.Output())
	u := p.Usage()
	assert.Equal(t, 18, u.InputTokens)
	assert.Equal(t, 342, u.OutputTokens)
	assert.Equal(t, 96711, u.CacheWriteTokens)
	assert.Equal(t, 4486, u.CacheReadTokens)
	assert.True(t, u.HasCost)
	assert.InDelta(t, 0.1965666, u.CostUSD, 1e-6)
	assert.Equal(t, 2, u.Turns)
	assert.NoError(t, Classify("claude", p, nil, 0))
}

func TestParser_PartialStream_UsageAndOutputWithoutResult(t *testing.T) {
	t.Parallel()
	// Everything up to but excluding the result line: what a crash mid-run
	// leaves behind.
	data, err := os.ReadFile(claudeFixture(t, "tool-use-read.jsonl"))
	require.NoError(t, err)
	p := NewParser()
	var evs []cartridge.SessionEvent
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, `"type":"result"`) {
			break
		}
		evs = append(evs, p.Line(line)...)
	}
	require.Len(t, evs, 3)
	assert.False(t, p.HasResult())

	// Per-message usage is summed once per message id even though the CLI
	// repeats it on every content-block line.
	u := p.Usage()
	assert.Equal(t, 10+8, u.InputTokens)
	assert.Equal(t, 3+1, u.OutputTokens)
	assert.Equal(t, 4701+92010, u.CacheWriteTokens)
	assert.Equal(t, 4486, u.CacheReadTokens)
	assert.False(t, u.HasCost)
	assert.Equal(t, 2, u.Turns)
	assert.Equal(t, "the quick brown fox", p.Output(), "last assistant text stands in for the missing result")

	err = Classify("claude", p, nil, 0)
	require.Error(t, err)
	assert.Equal(t, cartridge.KindCrash, cartridge.KindOf(err))
	assert.Contains(t, err.Error(), "without a result line")
}

func TestParser_APIErrorUnknownModel(t *testing.T) {
	t.Parallel()
	p, evs := claudeParseFixture(t, "api-error-unknown-model.jsonl")

	require.Equal(t, []cartridge.EventKind{cartridge.EventTextDelta}, claudeKinds(evs))
	assert.Contains(t, evs[0].Text, "issue with the selected model")
	assert.True(t, p.HasResult())
	require.NotNil(t, p.result.APIErrorStatus)
	assert.Equal(t, 404, *p.result.APIErrorStatus)
	assert.Equal(t, "api_error", p.result.TerminalReason)

	err := Classify("claude", p, errors.New("exit 1"), 1)
	require.Error(t, err)
	var ce *cartridge.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, cartridge.KindCrash, ce.Kind)
	assert.Equal(t, "claude", ce.Cartridge)
	assert.Contains(t, ce.Msg, "api error (HTTP 404)")
	assert.Contains(t, ce.Msg, "claude-no-such-model-999")
	assert.True(t, errors.Is(err, cartridge.ErrCrash))
	assert.Equal(t, 0, p.Usage().OutputTokens)
	assert.True(t, p.Usage().HasCost)
}

func TestParser_ErrorMaxTurns(t *testing.T) {
	t.Parallel()
	p, evs := claudeParseFixture(t, "error-max-turns.jsonl")

	require.Equal(t, []cartridge.EventKind{cartridge.EventToolUse, cartridge.EventToolResult}, claudeKinds(evs))
	assert.Equal(t, "Read", evs[0].Tool)
	assert.Equal(t, "Read", evs[1].Tool)
	assert.Equal(t, "1\tthe quick brown fox\n2\t", evs[1].Output)
	assert.Equal(t, "error_max_turns", p.result.Subtype)
	assert.Equal(t, "", p.Output(), "no text was produced before the cap")
	assert.Equal(t, 2, p.Usage().Turns)
	assert.InDelta(t, 0.0177705, p.Usage().CostUSD, 1e-9)

	err := Classify("claude", p, errors.New("exit 1"), 1)
	require.Error(t, err)
	assert.Equal(t, cartridge.KindCrash, cartridge.KindOf(err))
	assert.Contains(t, err.Error(), "max turns reached: Reached maximum number of turns (1)")
	assert.True(t, cartridge.KindOf(err).Retryable())
}

func TestParser_ToolDisallowed(t *testing.T) {
	t.Parallel()
	p, evs := claudeParseFixture(t, "tool-disallowed.jsonl")

	require.Equal(t, []cartridge.EventKind{
		cartridge.EventToolUse, cartridge.EventToolResult,
		cartridge.EventToolUse, cartridge.EventToolResult,
	}, claudeKinds(evs))
	assert.Equal(t, "ToolSearch", evs[0].Tool)
	assert.Equal(t, `max_results=1 query="select:Write"`, evs[0].Input, "unknown tools render sorted compact input")
	assert.Equal(t, "No matching deferred tools found", evs[1].Output)
	assert.Equal(t, "Write", evs[2].Tool)
	assert.Equal(t, "/work/wt/out.txt", evs[2].Input)
	assert.Equal(t, "Write", evs[3].Tool)
	assert.True(t, evs[3].IsError)
	assert.Contains(t, evs[3].Output, "No such tool available: Write")

	err := Classify("claude", p, errors.New("exit 1"), 1)
	assert.Equal(t, cartridge.KindCrash, cartridge.KindOf(err))
}

func TestParser_ShellDenied(t *testing.T) {
	t.Parallel()
	p, evs := claudeParseFixture(t, "shell-denied.jsonl")

	require.Equal(t, []cartridge.EventKind{
		cartridge.EventToolUse, cartridge.EventToolUse,
		cartridge.EventToolResult, cartridge.EventToolResult,
		cartridge.EventTextDelta,
	}, claudeKinds(evs))
	assert.Equal(t, "ls", evs[0].Input)
	assert.Equal(t, "touch zzz.txt", evs[1].Input)
	assert.Equal(t, "Bash", evs[2].Tool)
	assert.Equal(t, "hello.txt", evs[2].Output)
	assert.Equal(t, evs[1].ToolID, evs[3].ToolID)
	assert.Contains(t, evs[3].Output, "Permission to use Bash has been denied")
	assert.Equal(t, 1, p.Denials())
	assert.Contains(t, p.Output(), "Only the `ls` command succeeded")
	assert.NoError(t, Classify("claude", p, nil, 0))
}

func TestParser_RateLimitRejectedSetsRetryAfter(t *testing.T) {
	t.Parallel()
	p := NewParser()
	resets := time.Now().Add(90 * time.Minute).Unix()
	p.Line(`{"type":"system","subtype":"init","session_id":"s1","model":"m"}`)
	p.Line(fmt.Sprintf(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":%d,"rateLimitType":"five_hour"}}`, resets))
	p.Line(`{"type":"result","subtype":"success","is_error":true,"api_error_status":null,"result":"Something went wrong","session_id":"s1","num_turns":1}`)

	err := Classify("claude", p, errors.New("exit 1"), 1)
	var ce *cartridge.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, cartridge.KindRateLimited, ce.Kind)
	assert.Greater(t, ce.RetryAfter, 80*time.Minute)
	assert.LessOrEqual(t, ce.RetryAfter, 90*time.Minute)
	assert.True(t, errors.Is(err, cartridge.ErrRateLimited))
}

func TestParser_IgnoresGarbageAndUnknownTypes(t *testing.T) {
	t.Parallel()
	p := NewParser()
	assert.Empty(t, p.Line(""))
	assert.Empty(t, p.Line("   "))
	assert.Empty(t, p.Line("not json at all"))
	assert.Empty(t, p.Line("{not json either"))
	assert.Empty(t, p.Line(`{"type":"prompt_suggestion","text":"x"}`))
	assert.Empty(t, p.Line(`{"type":"assistant"}`))
	assert.Empty(t, p.Line(`{"type":"user","message":{"content":[{"type":"text","text":"ignored"}]}}`))
	assert.Equal(t, 7, p.Lines())
	assert.Equal(t, []string{"not json at all", "{not json either"}, p.noise)
	assert.False(t, p.HasResult())
	assert.Equal(t, cartridge.Usage{}, p.Usage())
	assert.Equal(t, "", p.Output())
}

func TestParser_ToolResultContentArray(t *testing.T) {
	t.Parallel()
	p := NewParser()
	p.Line(`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"Grep","input":{"pattern":"foo","path":"."}}]}}`)
	evs := p.Line(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"a.go:1"},{"type":"text","text":"b.go:2"}],"is_error":false}]}}`)
	require.Len(t, evs, 1)
	assert.Equal(t, "Grep", evs[0].Tool)
	assert.Equal(t, "a.go:1\nb.go:2", evs[0].Output)
}

func TestParser_TruncatesLongToolOutput(t *testing.T) {
	t.Parallel()
	p := NewParser()
	big := strings.Repeat("é", maxToolOutput)
	evs := p.Line(fmt.Sprintf(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":%q}]}}`, big))
	require.Len(t, evs, 1)
	assert.True(t, strings.HasSuffix(evs[0].Output, "…"))
	assert.LessOrEqual(t, len(evs[0].Output), maxToolOutput+len("…"))
	assert.True(t, strings.HasPrefix(big, strings.TrimSuffix(evs[0].Output, "…")), "cut on a rune boundary")
}

func TestRenderToolInput(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "", renderToolInput("Bash", nil))
	assert.Equal(t, "go test ./...", renderToolInput("Bash", []byte(`{"command":"go test ./...","description":"x"}`)))
	assert.Equal(t, "/a/b.go", renderToolInput("Edit", []byte(`{"file_path":"/a/b.go","old_string":"x","new_string":"y"}`)))
	assert.Equal(t, "*.go", renderToolInput("Glob", []byte(`{"pattern":"*.go"}`)))
	assert.Equal(t, `a=1 b="two"`, renderToolInput("Other", []byte(`{"b":"two","a":1}`)))
	assert.Equal(t, "plain", renderToolInput("Other", []byte(`plain`)))
	assert.Equal(t, `x=""`, renderToolInput("Bash", []byte(`{"x":""}`)), "falls back to compact JSON when the preferred key is absent")
}
