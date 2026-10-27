package grok

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
)

type streamEvent struct {
	Type              string          `json:"type"`
	Data              json.RawMessage `json:"data"`
	Text              string          `json:"text"`
	Message           string          `json:"message"`
	ToolName          string          `json:"toolName"`
	Name              string          `json:"name"`
	Kind              string          `json:"kind"`
	ToolCallID        string          `json:"toolCallId"`
	CallID            string          `json:"call_id"`
	Status            string          `json:"status"`
	StopReason        string          `json:"stopReason"`
	SessionID         string          `json:"sessionId"`
	SessionIDAlt      string          `json:"session_id"`
	RawInput          json.RawMessage `json:"rawInput"`
	Input             json.RawMessage `json:"input"`
	Arguments         json.RawMessage `json:"arguments"`
	Parameters        json.RawMessage `json:"parameters"`
	RawOutput         json.RawMessage `json:"rawOutput"`
	Output            json.RawMessage `json:"output"`
	Content           json.RawMessage `json:"content"`
	Result            json.RawMessage `json:"result"`
	Error             json.RawMessage `json:"error"`
	IsError           bool            `json:"is_error"`
	UsageIsIncomplete bool            `json:"usage_is_incomplete"`
	CostIsPartial     bool            `json:"cost_is_partial"`
	TotalCostUSD      *float64        `json:"total_cost_usd"`
	CostUSD           *float64        `json:"cost_usd"`
	Usage             *streamUsage    `json:"usage"`
	Turns             int             `json:"turns"`
	NumTurns          int             `json:"num_turns"`
}

type streamUsage struct {
	InputTokens              int      `json:"input_tokens"`
	InputTokensCamel         int      `json:"inputTokens"`
	OutputTokens             int      `json:"output_tokens"`
	OutputTokensCamel        int      `json:"outputTokens"`
	CacheReadTokens          int      `json:"cache_read_tokens"`
	CacheReadTokensCamel     int      `json:"cacheReadTokens"`
	CacheReadInputTokens     int      `json:"cache_read_input_tokens"`
	CacheWriteTokens         int      `json:"cache_write_tokens"`
	CacheWriteTokensCamel    int      `json:"cacheWriteTokens"`
	CacheCreationInputTokens int      `json:"cache_creation_input_tokens"`
	TotalCostUSD             *float64 `json:"total_cost_usd"`
	CostUSD                  *float64 `json:"cost_usd"`
	Turns                    int      `json:"turns"`
}

// Parser converts Grok streaming-json lines into cartridge events.
type Parser struct {
	sessionID string
	toolNames map[string]string
	segments  []string
	sinceTool int

	usage          cartridge.Usage
	costUSD        *float64
	costIncomplete bool
	ended          bool
	endError       string
	errorMsg       string
	noise          []string
	stderr         []string
	lines          int
}

// NewParser returns an empty streaming-json parser.
func NewParser() *Parser { return &Parser{toolNames: map[string]string{}} }

// Line consumes one streaming-json stdout line. Unknown event types and
// malformed lines are ignored for event purposes; diagnostics retain a
// bounded sample of malformed output.
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
	var ev streamEvent
	if err := json.Unmarshal([]byte(trimmed), &ev); err != nil {
		p.addNoise(trimmed)
		return nil
	}
	if ev.SessionID == "" {
		ev.SessionID = ev.SessionIDAlt
	}
	if ev.SessionID != "" {
		p.sessionID = ev.SessionID
	}
	if p.ended {
		return nil
	}
	switch ev.Type {
	case "text":
		data := rawString(ev.Data)
		if data == "" {
			data = ev.Text
		}
		if data == "" {
			return nil
		}
		p.segments = append(p.segments, data)
		return []cartridge.SessionEvent{cartridge.TextDelta(data)}
	case "tool", "tool_call":
		id := firstNonEmpty(ev.ToolCallID, ev.CallID)
		name := firstNonEmpty(ev.ToolName, ev.Name, ev.Kind)
		p.toolNames[id] = name
		p.sinceTool = len(p.segments)
		raw := firstRaw(ev.RawInput, ev.Input, ev.Arguments, ev.Parameters, ev.Data)
		return []cartridge.SessionEvent{cartridge.ToolUse(name, id, renderToolInput(name, raw))}
	case "tool_update", "tool_call_update":
		if !toolStatusDone(ev.Status) {
			return nil
		}
		id := firstNonEmpty(ev.ToolCallID, ev.CallID)
		raw := firstRaw(ev.RawOutput, ev.Output, ev.Content, ev.Result, ev.Error, ev.Data)
		return []cartridge.SessionEvent{cartridge.ToolResult(p.toolNames[id], id, renderToolResult(raw), ev.IsError || toolStatusError(ev.Status) || len(ev.Error) > 0 && string(ev.Error) != "null")}
	case "usage":
		p.applyUsage(ev)
		return nil
	case "end":
		p.applyUsage(ev)
		p.ended = true
		if ev.IsError || toolStatusError(ev.Status) || strings.HasPrefix(strings.ToLower(ev.StopReason), "error") {
			p.endError = firstNonEmpty(ev.Message, rawString(ev.Error), ev.StopReason, ev.Status, "grok reported an error at end")
		}
		return nil
	case "error":
		p.applyUsage(ev)
		if p.errorMsg == "" {
			p.errorMsg = firstNonEmpty(ev.Message, rawString(ev.Error), rawString(ev.Data), "grok reported an error")
		}
		return nil
	default:
		return nil
	}
}

// Stderr records a bounded diagnostic line.
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

func (p *Parser) applyUsage(ev streamEvent) {
	p.costIncomplete = p.costIncomplete || ev.UsageIsIncomplete || ev.CostIsPartial
	if ev.Usage != nil {
		u := ev.Usage
		p.usage.InputTokens = firstInt(u.InputTokens, u.InputTokensCamel)
		p.usage.OutputTokens = firstInt(u.OutputTokens, u.OutputTokensCamel)
		p.usage.CacheReadTokens = firstInt(u.CacheReadTokens, u.CacheReadTokensCamel, u.CacheReadInputTokens)
		p.usage.CacheWriteTokens = firstInt(u.CacheWriteTokens, u.CacheWriteTokensCamel, u.CacheCreationInputTokens)
		if u.Turns != 0 {
			p.usage.Turns = u.Turns
		}
		if u.TotalCostUSD != nil {
			p.costUSD = u.TotalCostUSD
		} else if u.CostUSD != nil {
			p.costUSD = u.CostUSD
		}
	}
	if ev.TotalCostUSD != nil {
		p.costUSD = ev.TotalCostUSD
	} else if ev.CostUSD != nil {
		p.costUSD = ev.CostUSD
	}
	if turns := firstInt(ev.Turns, ev.NumTurns); turns != 0 {
		p.usage.Turns = turns
	}
}

// SessionID returns the Grok session identifier, when reported.
func (p *Parser) SessionID() string { return p.sessionID }

// Lines returns the number of stdout lines consumed.
func (p *Parser) Lines() int { return p.lines }

// HasEnd reports whether the mandatory terminal end event arrived.
func (p *Parser) HasEnd() bool { return p.ended }

// Output returns the final assistant text after the last tool call.
func (p *Parser) Output() string {
	if p.sinceTool < len(p.segments) {
		return strings.Join(p.segments[p.sinceTool:], "")
	}
	return ""
}

// Usage returns token totals and cost only when the spend is complete.
func (p *Parser) Usage() cartridge.Usage {
	u := p.usage
	if p.ended && !p.costIncomplete && p.costUSD != nil {
		u.CostUSD = *p.costUSD
		u.HasCost = true
	} else {
		u.CostUSD = 0
		u.HasCost = false
	}
	return u
}

// Result builds a session result using process exit metadata.
func (p *Parser) Result(exitCode int, duration time.Duration) cartridge.Result {
	return cartridge.Result{
		Output: p.Output(), Usage: p.Usage(), Duration: duration,
		SessionID: p.sessionID, ExitCode: exitCode,
	}
}

func toolStatusDone(status string) bool {
	switch strings.ToLower(status) {
	case "completed", "complete", "succeeded", "success", "failed", "failure", "error", "rejected", "denied", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func toolStatusError(status string) bool {
	switch strings.ToLower(status) {
	case "failed", "failure", "error", "rejected", "denied", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func renderToolInput(tool string, raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err == nil {
		keys := []string{"command", "path", "file_path", "pattern", "query", "url"}
		if tool == "search_replace" {
			keys = []string{"path", "file_path", "old_string", "new_string"}
		}
		for _, key := range keys {
			if s, ok := m[key].(string); ok && s != "" {
				return truncate(s, maxToolInput)
			}
		}
		keys = keys[:0]
		for key := range m {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			v, err := json.Marshal(m[key])
			if err == nil {
				parts = append(parts, fmt.Sprintf("%s=%s", key, v))
			}
		}
		return truncate(strings.Join(parts, " "), maxToolInput)
	}
	return truncate(rawString(raw), maxToolInput)
}

func renderToolResult(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if s := rawString(raw); s != "" {
		return truncate(s, maxToolOutput)
	}
	return truncate(strings.TrimSpace(string(raw)), maxToolOutput)
}

func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

func firstRaw(values ...json.RawMessage) json.RawMessage {
	for _, raw := range values {
		if len(raw) > 0 && string(raw) != "null" {
			return raw
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstInt(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
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
