package kilo

import (
	"encoding/json"
	"fmt"
	"math"
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

type streamLine struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionID"`
	Part      json.RawMessage `json:"part"`
	Error     json.RawMessage `json:"error"`
	Info      *messageInfo    `json:"info"`
	Message   *messageInfo    `json:"message"`
}

type streamPart struct {
	ID     string      `json:"id"`
	CallID string      `json:"callID"`
	Type   string      `json:"type"`
	Text   string      `json:"text"`
	Tool   string      `json:"tool"`
	Cost   *float64    `json:"cost"`
	Tokens *stepTokens `json:"tokens"`
	State  *toolState  `json:"state"`
}

type stepTokens struct {
	Input     *int `json:"input"`
	Output    *int `json:"output"`
	Reasoning *int `json:"reasoning"`
	Cache     *struct {
		Read  *int `json:"read"`
		Write *int `json:"write"`
	} `json:"cache"`
}

type toolState struct {
	Status string          `json:"status"`
	Input  json.RawMessage `json:"input"`
	Output json.RawMessage `json:"output"`
	Error  json.RawMessage `json:"error"`
}

type messageInfo struct {
	Role string   `json:"role"`
	Cost *float64 `json:"cost"`
}

// Parser consumes Kilo's raw JSON events. It is not safe for concurrent use.
type Parser struct {
	sessionID       string
	lastText        string
	usage           cartridge.Usage
	stepCost        float64
	messageCost     *float64
	steps           int
	protocolProblem string
	errorMessage    string
	errorStatus     int
	stderr          []string
	noise           []string
	lines           int
}

// NewParser returns an empty Kilo JSON parser.
func NewParser() *Parser { return &Parser{} }

// Line consumes one stdout event and returns its transcript events.
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
	if line.SessionID != "" {
		p.sessionID = line.SessionID
	}
	switch line.Type {
	case "text":
		var part streamPart
		if json.Unmarshal(line.Part, &part) != nil || part.Type != "text" {
			p.addNoise(trimmed)
			return nil
		}
		if part.Text == "" {
			return nil
		}
		p.lastText = part.Text
		return []cartridge.SessionEvent{cartridge.TextDelta(part.Text)}
	case "tool_use":
		return p.tool(line.Part)
	case "step_finish":
		p.stepFinish(line.Part)
		return nil
	case "message", "message.updated", "message_cost":
		p.message(line)
		return nil
	case "error":
		p.explicitError(line.Error)
		return nil
	default:
		return nil
	}
}

func (p *Parser) tool(raw json.RawMessage) []cartridge.SessionEvent {
	var part streamPart
	if json.Unmarshal(raw, &part) != nil || part.Type != "tool" || part.State == nil {
		p.addNoise(string(raw))
		return nil
	}
	id := part.CallID
	if id == "" {
		id = part.ID
	}
	use := cartridge.ToolUse(part.Tool, id, renderToolInput(part.State.Input))
	switch part.State.Status {
	case "completed":
		return []cartridge.SessionEvent{use, cartridge.ToolResult(part.Tool, id, renderRaw(part.State.Output, maxToolOutput), false)}
	case "error":
		output := renderRaw(part.State.Error, maxToolOutput)
		if output == "" {
			output = renderRaw(part.State.Output, maxToolOutput)
		}
		return []cartridge.SessionEvent{use, cartridge.ToolResult(part.Tool, id, output, true)}
	default:
		return nil
	}
}

func (p *Parser) stepFinish(raw json.RawMessage) {
	var part streamPart
	if json.Unmarshal(raw, &part) != nil || part.Type != "step-finish" || !validCost(part.Cost) || !validTokens(part.Tokens) {
		if p.protocolProblem == "" {
			p.protocolProblem = "kilo emitted a malformed step_finish usage record"
		}
		return
	}
	tokens := part.Tokens
	p.steps++
	p.usage.InputTokens += *tokens.Input
	p.usage.OutputTokens += *tokens.Output
	p.usage.CacheReadTokens += *tokens.Cache.Read
	p.usage.CacheWriteTokens += *tokens.Cache.Write
	p.usage.Turns = p.steps
	p.stepCost += *part.Cost
}

func validCost(cost *float64) bool {
	return cost != nil && *cost >= 0 && !math.IsNaN(*cost) && !math.IsInf(*cost, 0)
}

func validTokens(tokens *stepTokens) bool {
	return tokens != nil && nonnegative(tokens.Input) && nonnegative(tokens.Output) && nonnegative(tokens.Reasoning) &&
		tokens.Cache != nil && nonnegative(tokens.Cache.Read) && nonnegative(tokens.Cache.Write)
}

func nonnegative(value *int) bool { return value != nil && *value >= 0 }

func (p *Parser) message(line streamLine) {
	info := line.Info
	if info == nil {
		info = line.Message
	}
	if info == nil || (info.Role != "" && info.Role != "assistant") || !validCost(info.Cost) {
		return
	}
	cost := *info.Cost
	p.messageCost = &cost
}

func (p *Parser) explicitError(raw json.RawMessage) {
	message, status := parseError(raw)
	if message == "" {
		message = "kilo reported an unspecified error"
	}
	if p.errorMessage == "" {
		p.errorMessage = message
		p.errorStatus = status
	}
}

func parseError(raw json.RawMessage) (string, int) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, 0
	}
	var body struct {
		Name       string `json:"name"`
		Message    string `json:"message"`
		StatusCode int    `json:"statusCode"`
		Data       *struct {
			Message    string `json:"message"`
			StatusCode int    `json:"statusCode"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return renderRaw(raw, maxToolOutput), 0
	}
	message, status := body.Message, body.StatusCode
	if body.Data != nil {
		if body.Data.Message != "" {
			message = body.Data.Message
		}
		if body.Data.StatusCode != 0 {
			status = body.Data.StatusCode
		}
	}
	if message == "" {
		message = body.Name
	}
	return message, status
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

// Cost returns cumulative assistant-message cost when available, otherwise
// the sum of root step costs.
func (p *Parser) Cost() (float64, bool) {
	if p.messageCost != nil {
		return *p.messageCost, true
	}
	if p.steps > 0 {
		return p.stepCost, true
	}
	return 0, false
}

// Usage aggregates every valid root step_finish record.
func (p *Parser) Usage() cartridge.Usage {
	usage := p.usage
	if cost, ok := p.Cost(); ok {
		usage.CostUSD = cost
		usage.HasCost = true
	}
	return usage
}

// Result builds a session result using process wall time and exit status.
func (p *Parser) Result(exitCode int, duration time.Duration) cartridge.Result {
	return cartridge.Result{Output: p.lastText, Usage: p.Usage(), Duration: duration, SessionID: p.sessionID, ExitCode: exitCode}
}

// ErrorMessage returns the first explicit structured error.
func (p *Parser) ErrorMessage() string { return p.errorMessage }

// ErrorStatus returns the explicit error's HTTP status, when present.
func (p *Parser) ErrorStatus() int { return p.errorStatus }

// ProtocolProblem reports malformed usage or a missing mandatory step finish.
func (p *Parser) ProtocolProblem() string {
	if p.protocolProblem != "" {
		return p.protocolProblem
	}
	if p.steps == 0 {
		return fmt.Sprintf("kilo exited without a valid step_finish (%d stdout lines)", p.lines)
	}
	return ""
}

func renderToolInput(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var values map[string]any
	if json.Unmarshal(raw, &values) == nil {
		for _, key := range []string{"command", "filePath", "path", "pattern", "query", "url", "description"} {
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
			encoded, err := json.Marshal(values[key])
			if err == nil {
				parts = append(parts, fmt.Sprintf("%s=%s", key, encoded))
			}
		}
		return truncate(strings.Join(parts, " "), maxToolInput)
	}
	return renderRaw(raw, maxToolInput)
}

func renderRaw(raw json.RawMessage, limit int) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return truncate(text, limit)
	}
	return truncate(strings.TrimSpace(string(raw)), limit)
}

func truncate(text string, limit int) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && cut < len(text) && text[cut]&0xc0 == 0x80 {
		cut--
	}
	return text[:cut] + "…"
}
