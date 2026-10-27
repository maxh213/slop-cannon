package kimi

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

	roleAssistant = "assistant"
	roleTool      = "tool"
	roleMeta      = "meta"

	metaRetry   = "turn.step.retrying"
	metaResume  = "session.resume_hint"
	metaVersion = "system.version"
)

type streamLine struct {
	Role       string          `json:"role"`
	Type       string          `json:"type"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []toolCall      `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	SessionID  string          `json:"session_id"`
	Version    string          `json:"version"`
	Retry      *retryRecord    `json:"-"`

	FailedAttempt int    `json:"failed_attempt"`
	NextAttempt   int    `json:"next_attempt"`
	MaxAttempts   int    `json:"max_attempts"`
	DelayMS       int64  `json:"delay_ms"`
	ErrorName     string `json:"error_name"`
	ErrorMessage  string `json:"error_message"`
	StatusCode    int    `json:"status_code"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type retryRecord struct {
	failedAttempt int
	nextAttempt   int
	maxAttempts   int
	delay         time.Duration
	errorName     string
	errorMessage  string
	statusCode    int
}

// Parser consumes stream-json records. Kimi has no terminal usage record;
// Output is the content of the final assistant message that carried text.
type Parser struct {
	sessionID string
	version   string
	lastText  string
	toolNames map[string]string
	retry     *retryRecord
	stderr    []string
	noise     []string
	lines     int
	assistant int
}

// NewParser returns an empty stream-json parser.
func NewParser() *Parser { return &Parser{toolNames: map[string]string{}} }

// Line consumes one stdout record and returns its cartridge events.
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
	switch line.Role {
	case roleAssistant:
		return p.assistantMessage(line)
	case roleTool:
		name := p.toolNames[line.ToolCallID]
		return []cartridge.SessionEvent{cartridge.ToolResult(name, line.ToolCallID, renderContent(line.Content, maxToolOutput), false)}
	case roleMeta:
		p.meta(line)
	}
	return nil
}

func (p *Parser) assistantMessage(line streamLine) []cartridge.SessionEvent {
	p.assistant++
	var evs []cartridge.SessionEvent
	if content := renderContent(line.Content, 0); content != "" {
		p.lastText = content
		evs = append(evs, cartridge.TextDelta(content))
	}
	for _, call := range line.ToolCalls {
		id, name := call.ID, call.Function.Name
		p.toolNames[id] = name
		evs = append(evs, cartridge.ToolUse(name, id, renderToolInput(call.Function.Arguments)))
	}
	return evs
}

func (p *Parser) meta(line streamLine) {
	switch line.Type {
	case metaResume:
		if line.SessionID != "" {
			p.sessionID = line.SessionID
		}
	case metaVersion:
		p.version = line.Version
	case metaRetry:
		p.retry = &retryRecord{
			failedAttempt: line.FailedAttempt,
			nextAttempt:   line.NextAttempt,
			maxAttempts:   line.MaxAttempts,
			delay:         time.Duration(line.DelayMS) * time.Millisecond,
			errorName:     line.ErrorName,
			errorMessage:  line.ErrorMessage,
			statusCode:    line.StatusCode,
		}
	}
}

// Stderr records one diagnostic stderr line.
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

// Output returns the final assistant message that carried text.
func (p *Parser) Output() string { return p.lastText }

// SessionID returns the id from session.resume_hint, when present.
func (p *Parser) SessionID() string { return p.sessionID }

// Version returns the version announced by system.version.
func (p *Parser) Version() string { return p.version }

// HasAssistant reports whether any assistant record was observed.
func (p *Parser) HasAssistant() bool { return p.assistant > 0 }

// Usage returns zero: this protocol has no usage record.
func (p *Parser) Usage() cartridge.Usage { return cartridge.Usage{} }

// Retry returns the most recent retry metadata, when present.
func (p *Parser) Retry() *retryRecord { return p.retry }

// Lines returns the number of stdout lines consumed.
func (p *Parser) Lines() int { return p.lines }

// Result builds the process outcome without inventing usage.
func (p *Parser) Result(exitCode int, duration time.Duration) cartridge.Result {
	return cartridge.Result{
		Output: p.Output(), Usage: p.Usage(), Duration: duration,
		SessionID: p.sessionID, ExitCode: exitCode,
	}
}

func renderToolInput(arguments string) string {
	if arguments == "" {
		return ""
	}
	var args map[string]any
	if json.Unmarshal([]byte(arguments), &args) != nil {
		return truncate(arguments, maxToolInput)
	}
	keys := []string{"command", "path", "pattern", "url", "query", "description"}
	for _, key := range keys {
		if value, ok := args[key].(string); ok && value != "" {
			return truncate(value, maxToolInput)
		}
	}
	ordered := make([]string, 0, len(args))
	for key := range args {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	parts := make([]string, 0, len(ordered))
	for _, key := range ordered {
		value, err := json.Marshal(args[key])
		if err == nil {
			parts = append(parts, fmt.Sprintf("%s=%s", key, value))
		}
	}
	return truncate(strings.Join(parts, " "), maxToolInput)
}

func renderContent(raw json.RawMessage, limit int) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if limit > 0 {
			return truncate(text, limit)
		}
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, block := range blocks {
			if block.Type != "text" || block.Text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(block.Text)
		}
		if limit > 0 {
			return truncate(b.String(), limit)
		}
		return b.String()
	}
	if limit > 0 {
		return truncate(string(raw), limit)
	}
	return string(raw)
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && cut < len(s) && s[cut]&0xc0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
