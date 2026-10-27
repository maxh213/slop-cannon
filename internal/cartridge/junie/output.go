package junie

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"slop-cannon/internal/cartridge"
)

const maxDiagnostics = 40

type resultMessage struct {
	SessionID string     `json:"sessionId"`
	Errors    []string   `json:"errors"`
	Result    *string    `json:"result"`
	LLMUsage  []llmUsage `json:"llmUsage"`
}

type llmUsage struct {
	Calls             int      `json:"calls"`
	Cost              *float64 `json:"cost"`
	InputTokens       int      `json:"inputTokens"`
	CacheInputTokens  int      `json:"cacheInputTokens"`
	CacheCreateTokens int      `json:"cacheCreateTokens"`
	OutputTokens      int      `json:"outputTokens"`
}

// Parser collects final JSON output. Junie's json-stream schema is not used.
type Parser struct {
	stdout strings.Builder
	stderr []string
	lines  int

	parsed   bool
	result   *resultMessage
	usage    cartridge.Usage
	parseErr error
}

// NewParser returns an empty final-JSON parser.
func NewParser() *Parser { return &Parser{} }

// Stdout collects one stdout line for final JSON decoding.
func (p *Parser) Stdout(text string) {
	p.lines++
	p.stdout.WriteString(text)
	p.stdout.WriteByte('\n')
}

// Stderr records one bounded diagnostic line.
func (p *Parser) Stderr(text string) {
	text = strings.TrimSpace(text)
	if text != "" && len(p.stderr) < maxDiagnostics {
		p.stderr = append(p.stderr, text)
	}
}

func (p *Parser) parse() {
	if p.parsed {
		return
	}
	p.parsed = true
	text := strings.TrimSpace(p.stdout.String())
	if text == "" {
		p.parseErr = fmt.Errorf("junie exited without a final JSON result")
		return
	}
	var r resultMessage
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		p.parseErr = fmt.Errorf("junie printed malformed final JSON: %w", err)
		return
	}
	u, err := sumUsage(r.LLMUsage)
	if err != nil {
		p.parseErr = err
		return
	}
	p.result, p.usage = &r, u
	if errors := nonEmpty(r.Errors); len(errors) > 0 {
		p.parseErr = fmt.Errorf("junie reported an error: %s", strings.Join(errors, "; "))
		return
	}
	if r.Result == nil {
		p.parseErr = fmt.Errorf("junie final JSON has no result field")
	}
}

func sumUsage(entries []llmUsage) (cartridge.Usage, error) {
	var u cartridge.Usage
	for i, entry := range entries {
		values := []struct {
			name  string
			value int
			dest  *int
		}{
			{"calls", entry.Calls, &u.Turns},
			{"inputTokens", entry.InputTokens, &u.InputTokens},
			{"cacheInputTokens", entry.CacheInputTokens, &u.CacheReadTokens},
			{"cacheCreateTokens", entry.CacheCreateTokens, &u.CacheWriteTokens},
			{"outputTokens", entry.OutputTokens, &u.OutputTokens},
		}
		for _, v := range values {
			if v.value < 0 || *v.dest > math.MaxInt-v.value {
				return cartridge.Usage{}, fmt.Errorf("junie final JSON has invalid llmUsage[%d].%s", i, v.name)
			}
			*v.dest += v.value
		}
		if entry.Cost != nil {
			cost := *entry.Cost
			if math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 || math.IsInf(u.CostUSD+cost, 0) {
				return cartridge.Usage{}, fmt.Errorf("junie final JSON has invalid llmUsage[%d].cost", i)
			}
			u.CostUSD += cost
			u.HasCost = true
		}
	}
	return u, nil
}

// Result builds the session result from the decoded final JSON.
func (p *Parser) Result(exitCode int, duration time.Duration) cartridge.Result {
	p.parse()
	res := cartridge.Result{Usage: p.usage, Duration: duration, ExitCode: exitCode}
	if p.result != nil {
		if p.result.Result != nil {
			res.Output = *p.result.Result
		}
		res.SessionID = p.result.SessionID
	}
	return res
}

func (p *Parser) diagnosticLines() []string {
	lines := append([]string(nil), p.stderr...)
	if p.result != nil {
		lines = append(lines, nonEmpty(p.result.Errors)...)
	}
	return lines
}

func (p *Parser) diagnostics() string {
	return strings.Join(p.diagnosticLines(), "\n")
}

func nonEmpty(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}
