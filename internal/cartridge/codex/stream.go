package codex

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"slop-cannon/internal/cartridge"
)

const (
	maxToolInput  = 400
	maxToolOutput = 4096
	maxNoise      = 40
)

type streamLine struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id"`
	Item     *item           `json:"item"`
	Usage    *usage          `json:"usage"`
	Message  string          `json:"message"`
	Error    json.RawMessage `json:"error"`
}

type item struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	Status           string          `json:"status"`
	Text             string          `json:"text"`
	Command          string          `json:"command"`
	AggregatedOutput string          `json:"aggregated_output"`
	ExitCode         *int            `json:"exit_code"`
	Query            string          `json:"query"`
	Server           string          `json:"server"`
	Tool             string          `json:"tool"`
	Arguments        json.RawMessage `json:"arguments"`
	Changes          json.RawMessage `json:"changes"`
	Result           json.RawMessage `json:"result"`
	Error            json.RawMessage `json:"error"`
}

type usage struct {
	InputTokens              *int `json:"input_tokens"`
	CachedInputTokens        int  `json:"cached_input_tokens"`
	OutputTokens             *int `json:"output_tokens"`
	CacheWriteInputTokens    int  `json:"cache_write_input_tokens"`
	CacheCreationInputTokens int  `json:"cache_creation_input_tokens"`
	CacheWriteTokens         int  `json:"cache_write_tokens"`
	ReasoningOutputTokens    int  `json:"reasoning_output_tokens"`
	ReasoningTokens          int  `json:"reasoning_tokens"`
}

func (u usage) valid() bool {
	return u.InputTokens != nil && *u.InputTokens >= 0 && u.OutputTokens != nil && *u.OutputTokens >= 0 &&
		u.CachedInputTokens >= 0 && u.CacheWriteInputTokens >= 0 && u.CacheCreationInputTokens >= 0 &&
		u.CacheWriteTokens >= 0 && u.ReasoningOutputTokens >= 0 && u.ReasoningTokens >= 0
}

func (u usage) input() int {
	if u.InputTokens == nil {
		return 0
	}
	return *u.InputTokens
}

func (u usage) output() int {
	if u.OutputTokens == nil {
		return 0
	}
	return *u.OutputTokens
}

func (u usage) cacheWrite() int {
	if u.CacheWriteInputTokens != 0 {
		return u.CacheWriteInputTokens
	}
	if u.CacheCreationInputTokens != 0 {
		return u.CacheCreationInputTokens
	}
	return u.CacheWriteTokens
}

func (u usage) reasoning() int {
	if u.ReasoningOutputTokens != 0 {
		return u.ReasoningOutputTokens
	}
	return u.ReasoningTokens
}

// Parser consumes codex exec JSONL. It is not safe for concurrent use.
type Parser struct {
	sessionID string
	output    string
	usage     *usage
	completed bool
	turnSeen  bool
	failure   string
	streamErr string
	started   map[string]bool
	noise     []string
	stderr    []string
	lines     int
}

// NewParser returns an empty Codex JSONL parser.
func NewParser() *Parser { return &Parser{started: map[string]bool{}} }

// Line parses one stdout line and returns its cartridge events.
func (p *Parser) Line(text string) []cartridge.SessionEvent {
	p.lines++
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	var line streamLine
	if !strings.HasPrefix(trimmed, "{") || json.Unmarshal([]byte(trimmed), &line) != nil {
		p.addNoise(trimmed)
		return nil
	}
	switch line.Type {
	case "thread.started":
		if line.ThreadID != "" {
			p.sessionID = line.ThreadID
		}
	case "turn.started":
		p.turnSeen = true
	case "item.started":
		return p.itemStarted(line.Item)
	case "item.completed":
		return p.itemCompleted(line.Item)
	case "turn.completed":
		p.completed = true
		p.usage = line.Usage
	case "turn.failed":
		p.failure = errorText(line.Error, line.Message)
	case "error":
		p.streamErr = errorText(line.Error, line.Message)
	}
	return nil
}

func (p *Parser) itemStarted(it *item) []cartridge.SessionEvent {
	if !isToolItem(it) {
		return nil
	}
	p.started[it.ID] = true
	return []cartridge.SessionEvent{cartridge.ToolUse(toolName(it), it.ID, toolInput(it))}
}

func (p *Parser) itemCompleted(it *item) []cartridge.SessionEvent {
	if it == nil {
		return nil
	}
	if it.Type == "agent_message" {
		if it.Text == "" {
			return nil
		}
		p.output = it.Text
		return []cartridge.SessionEvent{cartridge.TextDelta(it.Text)}
	}
	if !isToolItem(it) {
		return nil
	}
	var events []cartridge.SessionEvent
	if !p.started[it.ID] {
		events = append(events, cartridge.ToolUse(toolName(it), it.ID, toolInput(it)))
	}
	output, isErr := toolOutput(it)
	return append(events, cartridge.ToolResult(toolName(it), it.ID, output, isErr))
}

func isToolItem(it *item) bool {
	if it == nil || it.Type == "" {
		return false
	}
	switch it.Type {
	case "agent_message", "reasoning":
		return false
	default:
		return true
	}
}

func toolName(it *item) string {
	if it.Type == "mcp_tool_call" && it.Server != "" && it.Tool != "" {
		return it.Server + "." + it.Tool
	}
	return it.Type
}

func toolInput(it *item) string {
	for _, value := range []string{it.Command, it.Query} {
		if value != "" {
			return truncate(value, maxToolInput)
		}
	}
	if len(it.Changes) != 0 && string(it.Changes) != "null" {
		return truncate(renderRaw(it.Changes), maxToolInput)
	}
	if it.Type == "mcp_tool_call" {
		prefix := ""
		if it.Server != "" || it.Tool != "" {
			prefix = strings.Trim(strings.Join([]string{it.Server, it.Tool}, "."), ".") + " "
		}
		return truncate(prefix+renderRaw(it.Arguments), maxToolInput)
	}
	return truncate(renderRaw(it.Arguments), maxToolInput)
}

func toolOutput(it *item) (string, bool) {
	isErr := it.Status == "failed" || it.Status == "error" || (it.ExitCode != nil && *it.ExitCode != 0)
	if rawPresent(it.Error) {
		return truncate(renderRaw(it.Error), maxToolOutput), true
	}
	if it.AggregatedOutput != "" {
		return truncate(it.AggregatedOutput, maxToolOutput), isErr
	}
	if rawPresent(it.Result) {
		return truncate(renderRaw(it.Result), maxToolOutput), isErr
	}
	if it.Type == "file_change" && rawPresent(it.Changes) {
		return truncate(renderRaw(it.Changes), maxToolOutput), isErr
	}
	if isErr {
		return truncate(it.Status, maxToolOutput), true
	}
	return "", false
}

func rawPresent(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null" && s != `""` && s != "{}"
}

func renderRaw(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var value any
	if json.Unmarshal(raw, &value) == nil {
		if compact, err := json.Marshal(value); err == nil {
			return string(compact)
		}
	}
	return strings.TrimSpace(string(raw))
}

func errorText(raw json.RawMessage, fallback string) string {
	if fallback != "" {
		return fallback
	}
	if len(raw) == 0 {
		return "Codex reported an unspecified error"
	}
	var body struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &body) == nil && body.Message != "" {
		return body.Message
	}
	if text := renderRaw(raw); text != "" && text != "null" {
		return text
	}
	return "Codex reported an unspecified error"
}

// Stderr records one stderr line for failure diagnostics.
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

// SessionID returns the thread id announced by thread.started.
func (p *Parser) SessionID() string { return p.sessionID }

// Output returns the most recent completed agent message.
func (p *Parser) Output() string { return p.output }

// HasResult reports whether turn.completed was observed.
func (p *Parser) HasResult() bool { return p.completed }

// Usage returns the terminal turn's token accounting.
func (p *Parser) Usage() cartridge.Usage {
	if p.usage == nil {
		return cartridge.Usage{Turns: boolInt(p.turnSeen)}
	}
	return cartridge.Usage{
		InputTokens: p.usage.input(), OutputTokens: p.usage.output(),
		CacheReadTokens: p.usage.CachedInputTokens, CacheWriteTokens: p.usage.cacheWrite(),
		Turns: 1,
	}
}

// ReasoningTokens returns reasoning output reported by Codex. cartridge.Usage
// currently has no reasoning-token field, so it is kept separately here.
func (p *Parser) ReasoningTokens() int {
	if p.usage == nil {
		return 0
	}
	return p.usage.reasoning()
}

// Result builds the session result with process timing and exit status.
func (p *Parser) Result(exitCode int, duration time.Duration) cartridge.Result {
	return cartridge.Result{
		Output: p.Output(), Usage: p.Usage(), Duration: duration,
		SessionID: p.sessionID, ExitCode: exitCode,
	}
}

func (p *Parser) terminalProblem() string {
	if !p.completed {
		return fmt.Sprintf("codex exited without a successful turn.completed (%d stdout lines)", p.lines)
	}
	if p.usage == nil {
		return "malformed turn.completed: missing usage"
	}
	if !p.usage.valid() {
		return "malformed turn.completed: missing, negative or invalid core token usage"
	}
	return ""
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

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
