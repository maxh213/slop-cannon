package claude

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"slop-cannon/internal/cartridge"
)

// Limits on what the parser keeps from a session so a chatty run cannot grow
// memory without bound. They only bound diagnostics and transcript renderings,
// never the model's own output.
const (
	// maxToolInput is the longest one-line tool input rendered into an
	// EventToolUse.
	maxToolInput = 400
	// maxToolOutput is the longest tool result kept in an EventToolResult.
	maxToolOutput = 4096
	// maxNoise is how many non-JSON stdout lines and stderr lines are kept
	// for error messages.
	maxNoise = 40
)

// Line types and subtypes of the stream-json protocol, as observed with
// Claude Code 2.1.258 (testdata/claude/*.jsonl).
const (
	typeSystem    = "system"
	typeAssistant = "assistant"
	typeUser      = "user"
	typeResult    = "result"
	typeRateLimit = "rate_limit_event"

	subtypeInit    = "init"
	subtypeSuccess = "success"

	blockText       = "text"
	blockToolUse    = "tool_use"
	blockToolResult = "tool_result"
)

// streamLine is the envelope shared by every stream-json line.
type streamLine struct {
	Type            string         `json:"type"`
	Subtype         string         `json:"subtype"`
	SessionID       string         `json:"session_id"`
	Model           string         `json:"model"`
	ParentToolUseID *string        `json:"parent_tool_use_id"`
	Message         *apiMessage    `json:"message"`
	RateLimitInfo   *rateLimitInfo `json:"rate_limit_info"`
}

// apiMessage is the Anthropic Messages API message embedded in assistant and
// user lines.
type apiMessage struct {
	ID      string         `json:"id"`
	Role    string         `json:"role"`
	Model   string         `json:"model"`
	Content []contentBlock `json:"content"`
	Usage   *apiUsage      `json:"usage"`
}

// contentBlock is one element of a message's content array. Fields are the
// union of the text, tool_use and tool_result shapes; thinking blocks are
// recognised by Type and skipped.
type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// apiUsage is the per-message token accounting.
type apiUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// rateLimitInfo is the payload of a rate_limit_event line. Status is
// "allowed", "allowed_warning" or "rejected"; ResetsAt is a Unix timestamp.
type rateLimitInfo struct {
	Status        string `json:"status"`
	ResetsAt      int64  `json:"resetsAt"`
	RateLimitType string `json:"rateLimitType"`
	OverageStatus string `json:"overageStatus"`
}

// resultMessage is the final line of a session. Result is set on success,
// Errors on the error_* subtypes; IsError with subtype "success" and a
// non-null APIErrorStatus is how an API failure is reported (fixture
// api-error-unknown-model.jsonl).
type resultMessage struct {
	Subtype           string             `json:"subtype"`
	IsError           bool               `json:"is_error"`
	NumTurns          int                `json:"num_turns"`
	Result            string             `json:"result"`
	Errors            []string           `json:"errors"`
	SessionID         string             `json:"session_id"`
	TotalCostUSD      *float64           `json:"total_cost_usd"`
	Usage             *apiUsage          `json:"usage"`
	DurationMS        int64              `json:"duration_ms"`
	APIErrorStatus    *int               `json:"api_error_status"`
	TerminalReason    string             `json:"terminal_reason"`
	StopReason        string             `json:"stop_reason"`
	PermissionDenials []permissionDenial `json:"permission_denials"`
}

// permissionDenial is one tool call the permission system refused.
type permissionDenial struct {
	ToolName  string          `json:"tool_name"`
	ToolUseID string          `json:"tool_use_id"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// Parser turns the stdout of "claude -p --output-format stream-json" into
// cartridge events, one line at a time, and remembers what the terminal
// result line said. It is not safe for concurrent use; the adapter drives it
// from a single goroutine.
type Parser struct {
	sessionID string
	model     string

	// Tool names by tool_use id, so tool results can be labelled.
	toolNames map[string]string
	// Per-message usage, keyed by message id, because the CLI emits one line
	// per content block and repeats the message's usage on each.
	usageByMsg map[string]apiUsage
	msgOrder   []string

	// Text of the assistant message being received and of the last one that
	// carried any text, so Output can be filled when the result line is
	// missing or has no result field.
	curMsgID   string
	curText    strings.Builder
	lastText   string
	transcript strings.Builder

	result    *resultMessage
	rateLimit *rateLimitInfo
	noise     []string
	stderr    []string
	lines     int
	denials   int
}

// NewParser returns an empty parser.
func NewParser() *Parser {
	return &Parser{toolNames: map[string]string{}, usageByMsg: map[string]apiUsage{}}
}

// Line consumes one stdout line and returns the events it produced (often
// none). Lines that are not JSON objects are kept as noise for diagnostics
// and otherwise ignored: the CLI occasionally prints plain-text warnings on
// stdout (fixture text-reply.jsonl ends with one).
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
	case typeRateLimit:
		if line.RateLimitInfo != nil && line.RateLimitInfo.Status != "" && line.RateLimitInfo.Status != "allowed" {
			rl := *line.RateLimitInfo
			p.rateLimit = &rl
		}
		return nil
	case typeAssistant:
		return p.assistant(line.Message)
	case typeUser:
		return p.user(line.Message)
	case typeResult:
		var res resultMessage
		if err := json.Unmarshal([]byte(trimmed), &res); err != nil {
			p.addNoise(trimmed)
			return nil
		}
		p.flushMessage()
		p.result = &res
		if res.SessionID != "" {
			p.sessionID = res.SessionID
		}
		p.denials = len(res.PermissionDenials)
		return nil
	default:
		return nil
	}
}

// Stderr records one stderr line for diagnostics; the CLI prints structured
// error tags there (fixture api-error-unknown-model.stderr).
func (p *Parser) Stderr(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if len(p.stderr) < maxNoise {
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
	if m.ID != "" && m.ID != p.curMsgID {
		p.flushMessage()
		p.curMsgID = m.ID
	}
	if m.Usage != nil && m.ID != "" {
		if _, seen := p.usageByMsg[m.ID]; !seen {
			p.msgOrder = append(p.msgOrder, m.ID)
		}
		p.usageByMsg[m.ID] = *m.Usage
	}
	var evs []cartridge.SessionEvent
	for _, b := range m.Content {
		switch b.Type {
		case blockText:
			if b.Text == "" {
				continue
			}
			p.curText.WriteString(b.Text)
			p.transcript.WriteString(b.Text)
			evs = append(evs, cartridge.TextDelta(b.Text))
		case blockToolUse:
			p.toolNames[b.ID] = b.Name
			evs = append(evs, cartridge.ToolUse(b.Name, b.ID, renderToolInput(b.Name, b.Input)))
		}
	}
	return evs
}

func (p *Parser) user(m *apiMessage) []cartridge.SessionEvent {
	if m == nil {
		return nil
	}
	var evs []cartridge.SessionEvent
	for _, b := range m.Content {
		if b.Type != blockToolResult {
			continue
		}
		name := p.toolNames[b.ToolUseID]
		evs = append(evs, cartridge.ToolResult(name, b.ToolUseID, renderToolResult(b.Content), b.IsError))
	}
	return evs
}

// flushMessage closes the assistant message being received.
func (p *Parser) flushMessage() {
	if p.curText.Len() > 0 {
		p.lastText = p.curText.String()
		p.curText.Reset()
	}
	p.curMsgID = ""
}

// SessionID returns the CLI session id, when one was seen.
func (p *Parser) SessionID() string { return p.sessionID }

// Model returns the model the init line announced, when seen.
func (p *Parser) Model() string { return p.model }

// Lines returns how many stdout lines were consumed.
func (p *Parser) Lines() int { return p.lines }

// HasResult reports whether the terminal result line was seen.
func (p *Parser) HasResult() bool { return p.result != nil }

// Denials returns how many tool calls the permission system refused, from
// the result line.
func (p *Parser) Denials() int { return p.denials }

// Transcript returns every assistant text block in order.
func (p *Parser) Transcript() string { return p.transcript.String() }

// Output returns the model's final text: the result line's result field on
// success, otherwise the last assistant message that carried text.
func (p *Parser) Output() string {
	p.flushMessage()
	if p.result != nil && p.result.Result != "" {
		return p.result.Result
	}
	return p.lastText
}

// Usage returns what the session consumed: the result line's totals when it
// arrived (they include the cost), else the per-message sums seen so far.
func (p *Parser) Usage() cartridge.Usage {
	if p.result != nil && p.result.Usage != nil {
		u := usageFrom(*p.result.Usage)
		if p.result.TotalCostUSD != nil {
			u.CostUSD = *p.result.TotalCostUSD
			u.HasCost = true
		}
		u.Turns = p.result.NumTurns
		return u
	}
	var u cartridge.Usage
	for _, id := range p.msgOrder {
		u = u.Add(usageFrom(p.usageByMsg[id]))
	}
	u.Turns = len(p.msgOrder)
	return u
}

// Result builds the cartridge result from what was parsed. exitCode and
// duration come from the process; the CLI's own duration is not used
// because it excludes start-up.
func (p *Parser) Result(exitCode int, duration time.Duration) cartridge.Result {
	return cartridge.Result{
		Output:    p.Output(),
		Usage:     p.Usage(),
		Duration:  duration,
		SessionID: p.sessionID,
		ExitCode:  exitCode,
	}
}

func usageFrom(u apiUsage) cartridge.Usage {
	return cartridge.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
}

// renderToolInput renders a tool's input on one line: the command for Bash,
// the path for file tools, otherwise compact JSON. Long inputs are cut.
func renderToolInput(tool string, raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err == nil {
		if s := preferredInput(tool, m); s != "" {
			return truncate(s, maxToolInput)
		}
		return truncate(compactJSON(m), maxToolInput)
	}
	return truncate(strings.TrimSpace(string(raw)), maxToolInput)
}

func preferredInput(tool string, m map[string]any) string {
	var keys []string
	switch tool {
	case "Bash":
		keys = []string{"command"}
	case "Read", "Write", "Edit", "MultiEdit", "NotebookEdit":
		keys = []string{"file_path", "notebook_path"}
	case "Glob", "Grep":
		keys = []string{"pattern"}
	case "WebFetch":
		keys = []string{"url"}
	case "Task":
		keys = []string{"description"}
	default:
		return ""
	}
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func compactJSON(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v, err := json.Marshal(m[k])
		if err != nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	return strings.Join(parts, " ")
}

// renderToolResult flattens a tool_result content field, which is either a
// string or an array of text blocks.
func renderToolResult(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return truncate(s, maxToolOutput)
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == blockText && blk.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(blk.Text)
			}
		}
		return truncate(b.String(), maxToolOutput)
	}
	return truncate(strings.TrimSpace(string(raw)), maxToolOutput)
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && cut < len(s) && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
