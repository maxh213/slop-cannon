package agy

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"slop-cannon/internal/cartridge"
)

const (
	maxDiagnosticLines = 40
	maxDiagnosticLine  = 500
	agentErrorPrefix   = "AGY_ERROR:"
)

type resultEnvelope struct {
	ConversationID string         `json:"conversation_id"`
	Status         string         `json:"status"`
	Response       *string        `json:"response"`
	Error          string         `json:"error"`
	NumTurns       int            `json:"num_turns"`
	Usage          *usage         `json:"usage"`
	DeniedActions  []deniedAction `json:"denied_actions"`
}

type deniedAction struct {
	Action      string `json:"action"`
	DisplayName string `json:"display_name"`
}

type usage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	ThinkingTokens  int `json:"thinking_tokens"`
	CacheReadTokens int `json:"cache_read_tokens"`
	TotalTokens     int `json:"total_tokens"`
}

// agentError is the structured diagnostic agy 1.2.6+ writes to stderr.
type agentError struct {
	ShortError string `json:"short_error"`
	Status     string `json:"status"`
	ErrorCode  int    `json:"error_code"`
	CodeKind   string `json:"code_kind"`
	Retryable  bool   `json:"retryable"`
	ErrorID    string `json:"error_id"`
}

// Parser collects the final JSON envelope and bounded stderr diagnostics.
// Stdout is retained because it contains the model's final response.
type Parser struct {
	stdout   strings.Builder
	stderr   []string
	envelope *resultEnvelope
	agentErr *agentError
	parseErr error
	parsed   bool
}

// NewParser returns an empty final-envelope parser.
func NewParser() *Parser { return &Parser{} }

// Stdout collects one stdout line for final JSON decoding.
func (p *Parser) Stdout(text string) {
	p.stdout.WriteString(text)
	p.stdout.WriteByte('\n')
}

// Stderr records one bounded diagnostic and any structured AGY_ERROR payload.
func (p *Parser) Stderr(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if strings.HasPrefix(text, agentErrorPrefix) {
		var ae agentError
		raw := strings.TrimSpace(strings.TrimPrefix(text, agentErrorPrefix))
		if strings.HasPrefix(raw, "{") && json.Unmarshal([]byte(raw), &ae) == nil {
			p.agentErr = &ae
		}
	}
	if len(p.stderr) < maxDiagnosticLines {
		p.stderr = append(p.stderr, truncate(text, maxDiagnosticLine))
	}
}

// Parse decodes exactly one final JSON object. Missing or malformed result
// envelopes are failures, never successful empty responses.
func (p *Parser) Parse() {
	if p.parsed {
		return
	}
	p.parsed = true
	text := strings.TrimSpace(p.stdout.String())
	if text == "" {
		p.parseErr = errors.New("missing final JSON result")
		return
	}
	var envelope resultEnvelope
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		p.parseErr = fmt.Errorf("malformed final JSON result: %w", err)
		return
	}
	p.envelope = &envelope
}

// Output returns the final response, including a partial response on failure.
func (p *Parser) Output() string {
	if p.envelope == nil || p.envelope.Response == nil {
		return ""
	}
	return *p.envelope.Response
}

// SessionID returns agy's resumable conversation id.
func (p *Parser) SessionID() string {
	if p.envelope == nil {
		return ""
	}
	return p.envelope.ConversationID
}

// Usage returns agy's token and turn accounting. Its envelope reports no cost.
func (p *Parser) Usage() cartridge.Usage {
	if p.envelope == nil {
		return cartridge.Usage{}
	}
	u := cartridge.Usage{Turns: p.envelope.NumTurns}
	if p.envelope.Usage != nil {
		u.InputTokens = p.envelope.Usage.InputTokens
		u.OutputTokens = p.envelope.Usage.OutputTokens
		u.CacheReadTokens = p.envelope.Usage.CacheReadTokens
	}
	return u
}

// Result builds the session result using process wall time and exit status.
func (p *Parser) Result(exitCode int, duration time.Duration) cartridge.Result {
	return cartridge.Result{
		Output: p.Output(), Usage: p.Usage(), Duration: duration,
		SessionID: p.SessionID(), ExitCode: exitCode,
	}
}

func (p *Parser) diagnostics() string {
	return strings.Join(p.stderr, "\n")
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
