package cursor

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"slop-cannon/internal/cartridge"
)

// Limits on what the parser keeps from a session so a chatty run cannot grow
// memory without bound. They only bound diagnostics and transcript
// renderings, never the model's own output.
const (
	maxToolInput  = 400
	maxToolOutput = 4096
	maxNoise      = 40
)

// Line types and subtypes of the stream-json protocol, as observed with
// Cursor Agent 2026.09.28 (testdata/cursor/*.jsonl). "thinking" lines and
// "system"/"task_notification" lines (a backgrounded command finished) carry
// nothing the cartridge contract needs and are skipped.
const (
	typeSystem    = "system"
	typeAssistant = "assistant"
	typeToolCall  = "tool_call"
	typeResult    = "result"

	subtypeInit      = "init"
	subtypeStarted   = "started"
	subtypeCompleted = "completed"
	subtypeSuccess   = "success"

	blockText = "text"
)

// streamLine is the envelope shared by every stream-json line.
type streamLine struct {
	Type      string                     `json:"type"`
	Subtype   string                     `json:"subtype"`
	SessionID string                     `json:"session_id"`
	Model     string                     `json:"model"`
	CallID    string                     `json:"call_id"`
	Message   *apiMessage                `json:"message"`
	ToolCall  map[string]json.RawMessage `json:"tool_call"`
}

// apiMessage is the message embedded in assistant and user lines.
type apiMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// toolCall is the body under a tool_call line's single "<kind>ToolCall" key.
// Result holds exactly one of "success" or "failure" (or "error").
type toolCall struct {
	Args   map[string]any             `json:"args"`
	Result map[string]json.RawMessage `json:"result"`
}

// resultMessage is the final line of a session. Its result field is every
// assistant segment concatenated without separators, so the parser builds
// Output from the assistant lines instead.
type resultMessage struct {
	Subtype    string   `json:"subtype"`
	IsError    bool     `json:"is_error"`
	Result     string   `json:"result"`
	SessionID  string   `json:"session_id"`
	DurationMS int64    `json:"duration_ms"`
	Usage      *usage   `json:"usage"`
	Error      string   `json:"error"`
	Errors     []string `json:"errors"`
}

type usage struct {
	InputTokens      int `json:"inputTokens"`
	OutputTokens     int `json:"outputTokens"`
	CacheReadTokens  int `json:"cacheReadTokens"`
	CacheWriteTokens int `json:"cacheWriteTokens"`
}

// Parser turns the stdout of "cursor-agent -p --output-format stream-json"
// into cartridge events, one line at a time, and remembers what the terminal
// result line said. It is not safe for concurrent use.
type Parser struct {
	sessionID string
	model     string

	// segments are the assistant text lines seen so far; sinceTool is the
	// index of the first one after the most recent tool call, so Output is
	// the model's final answer rather than its narration between tools.
	segments  []string
	sinceTool int
	toolCalls int

	result *resultMessage
	noise  []string
	stderr []string
	lines  int
}

// NewParser returns an empty parser.
func NewParser() *Parser { return &Parser{} }

// Line consumes one stdout line and returns the events it produced (often
// none). Lines that are not JSON objects are kept as noise for diagnostics.
func (p *Parser) Line(text string) []cartridge.SessionEvent {
	p.lines++
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	if !strings.HasPrefix(trimmed, "{") {
		p.addNoise(trimmed)
		return nil
	}
	var line streamLine
	if err := json.Unmarshal([]byte(trimmed), &line); err != nil {
		p.addNoise(trimmed)
		return nil
	}
	if line.SessionID != "" && p.sessionID == "" {
		p.sessionID = line.SessionID
	}
	switch line.Type {
	case typeSystem:
		if line.Subtype == subtypeInit && line.Model != "" {
			p.model = line.Model
		}
		return nil
	case typeAssistant:
		return p.assistant(line.Message)
	case typeToolCall:
		return p.toolCall(line)
	case typeResult:
		var res resultMessage
		if err := json.Unmarshal([]byte(trimmed), &res); err != nil {
			p.addNoise(trimmed)
			return nil
		}
		p.result = &res
		if res.SessionID != "" {
			p.sessionID = res.SessionID
		}
		return nil
	default:
		return nil
	}
}

// Stderr records one stderr line for diagnostics; the CLI prints its own
// failures there (fixtures unknown-model.stderr, logged-out.stderr).
func (p *Parser) Stderr(text string) {
	text = strings.TrimSpace(text)
	if text != "" && len(p.stderr) < maxNoise {
		p.stderr = append(p.stderr, text)
	}
}

func (p *Parser) addNoise(text string) {
	if len(p.noise) < maxNoise {
		p.noise = append(p.noise, text)
	}
}

func (p *Parser) assistant(m *apiMessage) []cartridge.SessionEvent {
	if m == nil {
		return nil
	}
	var b strings.Builder
	for _, blk := range m.Content {
		if blk.Type == blockText {
			b.WriteString(blk.Text)
		}
	}
	if b.Len() == 0 {
		return nil
	}
	p.segments = append(p.segments, b.String())
	return []cartridge.SessionEvent{cartridge.TextDelta(b.String())}
}

func (p *Parser) toolCall(line streamLine) []cartridge.SessionEvent {
	for key, raw := range line.ToolCall {
		var tc toolCall
		if err := json.Unmarshal(raw, &tc); err != nil {
			continue
		}
		name := ToolName(key)
		switch line.Subtype {
		case subtypeStarted:
			p.toolCalls++
			p.sinceTool = len(p.segments)
			return []cartridge.SessionEvent{cartridge.ToolUse(name, line.CallID, renderToolInput(tc.Args))}
		case subtypeCompleted:
			out, isErr := renderToolResult(tc.Result)
			return []cartridge.SessionEvent{cartridge.ToolResult(name, line.CallID, out, isErr)}
		}
	}
	return nil
}

// ToolName turns a tool_call key such as "shellToolCall" into the tool's
// name ("Shell"). Unknown keys keep their name minus the suffix.
func ToolName(key string) string {
	n := strings.TrimSuffix(key, "ToolCall")
	if n == "" {
		return key
	}
	r := []rune(n)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// SessionID returns the CLI session id, when one was seen.
func (p *Parser) SessionID() string { return p.sessionID }

// Model returns the model the init line announced, when seen.
func (p *Parser) Model() string { return p.model }

// HasResult reports whether the terminal result line was seen.
func (p *Parser) HasResult() bool { return p.result != nil }

// ToolCalls returns how many tool calls the session started.
func (p *Parser) ToolCalls() int { return p.toolCalls }

// Output returns the model's final text: the assistant segments after the
// last tool call, joined by blank lines. With no assistant line at all it
// falls back to the result line's result field.
func (p *Parser) Output() string {
	final := p.segments
	if p.sinceTool < len(p.segments) {
		final = p.segments[p.sinceTool:]
	}
	if len(final) > 0 {
		return strings.Join(final, "\n\n")
	}
	if p.result != nil {
		return p.result.Result
	}
	return ""
}

// Usage returns what the session consumed, from the result line. The CLI
// reports tokens but no cost, so HasCost stays false and max_cost_usd never
// trips for this cartridge.
func (p *Parser) Usage() cartridge.Usage {
	if p.result == nil || p.result.Usage == nil {
		return cartridge.Usage{}
	}
	u := p.result.Usage
	return cartridge.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
		Turns:            p.toolCalls + 1,
	}
}

// Result builds the cartridge result from what was parsed.
func (p *Parser) Result(exitCode int, duration time.Duration) cartridge.Result {
	return cartridge.Result{
		Output:    p.Output(),
		Usage:     p.Usage(),
		Duration:  duration,
		SessionID: p.sessionID,
		ExitCode:  exitCode,
	}
}

// renderToolInput renders a tool's arguments on one line: the command for
// the shell, the path (and pattern) for file tools, otherwise compact JSON.
func renderToolInput(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	for _, k := range []string{"command", "pattern", "path", "globPattern", "url", "query"} {
		if s, ok := args[k].(string); ok && s != "" {
			return truncate(s, maxToolInput)
		}
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v, err := json.Marshal(args[k])
		if err != nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	return truncate(strings.Join(parts, " "), maxToolInput)
}

// renderToolResult flattens a tool result. A "success" result is the
// command's interleaved output (or the file content, or compact JSON); a
// "failure" or "error" result marks the event as an error. A command the
// CLI moved to the background says so, because its output arrives later.
func renderToolResult(res map[string]json.RawMessage) (string, bool) {
	for _, k := range []string{"success", "failure", "error", "rejected"} {
		raw, ok := res[k]
		if !ok {
			continue
		}
		isErr := k != "success"
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				return truncate(s, maxToolOutput), isErr
			}
			return truncate(string(raw), maxToolOutput), isErr
		}
		out := ""
		for _, f := range []string{"interleavedOutput", "content", "output", "message", "error"} {
			if s, ok := body[f].(string); ok && s != "" {
				out = s
				break
			}
		}
		if out == "" && isErr {
			if s, ok := body["stderr"].(string); ok {
				out = s
			}
		}
		if code, ok := body["exitCode"].(float64); ok && code != 0 {
			isErr = true
			out = fmt.Sprintf("exit %d\n%s", int(code), out)
		}
		if reason, ok := body["backgroundReason"].(string); ok && reason != "" {
			out = "(moved to the background: " + reason + ")\n" + out
		}
		if out == "" {
			b, _ := json.Marshal(body)
			out = string(b)
		}
		return truncate(out, maxToolOutput), isErr
	}
	return "", false
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && cut < len(s) && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
