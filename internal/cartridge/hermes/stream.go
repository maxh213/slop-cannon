package hermes

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"slop-cannon/internal/cartridge"
)

const (
	maxToolInput  = 400
	maxToolOutput = 4096
	maxNoise      = 40

	typeSystem     = "system"
	typeText       = "text"
	typeToolUse    = "tool_use"
	typeToolResult = "tool_result"
	typeResult     = "result"
	subtypeInit    = "init"
)

type streamLine struct {
	Type       string          `json:"type"`
	Subtype    string          `json:"subtype"`
	SessionID  string          `json:"session_id"`
	Model      string          `json:"model"`
	Text       string          `json:"text"`
	Name       string          `json:"name"`
	ToolCallID string          `json:"tool_call_id"`
	Input      json.RawMessage `json:"input"`
	Output     string          `json:"output"`
	IsError    bool            `json:"is_error"`
}

type tokenUsage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	Total      int `json:"total"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`
}

type resultMessage struct {
	SessionID  string     `json:"session_id"`
	ExitCode   int        `json:"exit_code"`
	Text       string     `json:"text"`
	Tokens     tokenUsage `json:"tokens"`
	DurationMS int64      `json:"duration_ms"`
	Error      string     `json:"error"`
}

// Parser consumes Hermes stream-json stdout. It is driven by one goroutine per
// firing and retains only bounded diagnostics and tool renderings.
type Parser struct {
	sessionID  string
	model      string
	toolNames  map[string]string
	transcript strings.Builder
	result     *resultMessage
	noise      []string
	stderr     []string
	lines      int
}

// NewParser returns an empty Hermes stream parser.
func NewParser() *Parser { return &Parser{toolNames: map[string]string{}} }

// Line consumes one stdout line and returns its cartridge events.
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
	case typeText:
		if line.Text != "" {
			p.transcript.WriteString(line.Text)
			return []cartridge.SessionEvent{cartridge.TextDelta(line.Text)}
		}
	case typeToolUse:
		p.toolNames[line.ToolCallID] = line.Name
		return []cartridge.SessionEvent{cartridge.ToolUse(line.Name, line.ToolCallID, renderToolInput(line.Name, line.Input))}
	case typeToolResult:
		name := line.Name
		if name == "" {
			name = p.toolNames[line.ToolCallID]
		}
		return []cartridge.SessionEvent{cartridge.ToolResult(name, line.ToolCallID, truncate(line.Output, maxToolOutput), line.IsError)}
	case typeResult:
		var result resultMessage
		if err := json.Unmarshal([]byte(trimmed), &result); err != nil {
			p.addNoise(trimmed)
			return nil
		}
		p.result = &result
		if result.SessionID != "" {
			p.sessionID = result.SessionID
		}
	}
	return nil
}

// Stderr records one bounded diagnostic line for final-error classification.
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

// SessionID returns the session announced by init or the terminal result.
func (p *Parser) SessionID() string { return p.sessionID }

// Model returns the model announced by the init event.
func (p *Parser) Model() string { return p.model }

// HasResult reports whether a terminal result event was parsed.
func (p *Parser) HasResult() bool { return p.result != nil }

// Transcript returns all streamed text deltas concatenated verbatim.
func (p *Parser) Transcript() string { return p.transcript.String() }

// Output returns the terminal response text, or the partial transcript when no
// terminal result arrived.
func (p *Parser) Output() string {
	if p.result != nil {
		return p.result.Text
	}
	return p.transcript.String()
}

// Usage returns the terminal event's token accounting.
func (p *Parser) Usage() cartridge.Usage {
	if p.result == nil {
		return cartridge.Usage{}
	}
	t := p.result.Tokens
	return cartridge.Usage{
		InputTokens: t.Input, OutputTokens: t.Output,
		CacheReadTokens: t.CacheRead, CacheWriteTokens: t.CacheWrite,
	}
}

// Result builds the session outcome using process wall time and exit status.
func (p *Parser) Result(exitCode int, duration time.Duration) cartridge.Result {
	return cartridge.Result{
		Output: p.Output(), Usage: p.Usage(), Duration: duration,
		SessionID: p.sessionID, ExitCode: exitCode,
	}
}

func renderToolInput(tool string, raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		return truncate(strings.TrimSpace(string(raw)), maxToolInput)
	}
	for _, key := range preferredKeys(tool) {
		if value, ok := values[key].(string); ok && value != "" {
			return truncate(value, maxToolInput)
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value, err := json.Marshal(values[key])
		if err == nil {
			parts = append(parts, fmt.Sprintf("%s=%s", key, value))
		}
	}
	return truncate(strings.Join(parts, " "), maxToolInput)
}

func preferredKeys(tool string) []string {
	switch tool {
	case "terminal":
		return []string{"command", "cmd"}
	case "read_file", "write_file", "patch":
		return []string{"path", "file_path"}
	case "search_files":
		return []string{"pattern", "path"}
	case "web_extract":
		return []string{"url"}
	case "web_search":
		return []string{"query"}
	default:
		return nil
	}
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
