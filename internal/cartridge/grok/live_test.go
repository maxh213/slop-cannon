package grok

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/structured"
	"slop-cannon/internal/config"
)

const grokLiveModel = "grok-4.7-build-fast"

func grokLiveEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("SC_GROK_LIVE") != "1" {
		t.Skip("set SC_GROK_LIVE=1 for bounded live Grok verification")
	}
}

func grokLiveAdapter(t *testing.T) *Adapter {
	t.Helper()
	return grokNewAdapter(t, config.Cartridge{Model: grokLiveModel, Args: []string{flagReasoningEffort, "low"}})
}

func grokLiveSpec(t *testing.T, prompt string) cartridge.RunSpec {
	t.Helper()
	return cartridge.RunSpec{
		SystemPrompt: "You are a non-sensitive test fixture. Follow the request exactly and do not disclose configuration.",
		Prompt:       prompt,
		Workdir:      t.TempDir(),
		Model:        grokLiveModel,
		ToolPolicy:   cartridge.ToolReadExec,
		MaxTurns:     2,
		Timeout:      90 * time.Second,
	}
}

func grokLiveCollect(t *testing.T, adapter *Adapter, spec cartridge.RunSpec) cartridge.Result {
	t.Helper()
	result, _ := grokLiveCollectEvents(t, adapter, spec)
	return result
}

func grokLiveCollectEvents(t *testing.T, adapter *Adapter, spec cartridge.RunSpec) (cartridge.Result, []cartridge.SessionEvent) {
	t.Helper()
	session, err := adapter.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("live startup failed with kind %s", cartridge.KindOf(err))
	}
	var events []cartridge.SessionEvent
	result, err := cartridge.Collect(session, func(ev cartridge.SessionEvent) { events = append(events, ev) })
	for _, ev := range events {
		switch ev.Kind {
		case cartridge.EventToolUse:
			t.Logf("tool use: name=%s input=%q", ev.Tool, ev.Input)
		case cartridge.EventToolResult:
			t.Logf("tool result: name=%s error=%t", ev.Tool, ev.IsError)
			if ev.IsError && strings.Contains(strings.ToLower(ev.Output), "permission") {
				t.Logf("permission outcome=%q", ev.Output)
			}
		}
	}
	if err != nil {
		t.Fatalf("live session failed with kind %s", cartridge.KindOf(err))
	}
	assert.Equal(t, 0, result.ExitCode)
	assert.Positive(t, result.Duration)
	return result, events
}

func TestLiveDetect(t *testing.T) {
	grokLiveEnabled(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	status, err := grokLiveAdapter(t).Detect(ctx)
	require.NoError(t, err, "detect failed with kind %s", cartridge.KindOf(err))
	assert.True(t, status.Installed)
	assert.Equal(t, VerifiedVersion, status.Version)
	assert.True(t, status.Ready())
	t.Logf("detect passed: version=%s auth=%s", status.Version, status.Auth)
}

func TestLiveEcho(t *testing.T) {
	grokLiveEnabled(t)
	adapter := grokLiveAdapter(t)
	spec := grokLiveSpec(t, "Reply with exactly the single word PONG and nothing else.")
	spec.MaxTurns = 1
	result := grokLiveCollect(t, adapter, spec)
	assert.True(t, strings.EqualFold(strings.TrimSpace(result.Output), "PONG"), "echo response did not match")
	assert.NotEmpty(t, result.SessionID)
	assert.Positive(t, result.Usage.OutputTokens)
	assert.Positive(t, result.Usage.Turns)
	t.Logf("echo passed: input=%d output=%d turns=%d has_cost=%t", result.Usage.InputTokens, result.Usage.OutputTokens, result.Usage.Turns, result.Usage.HasCost)
}

func TestLiveStructuredContract(t *testing.T) {
	grokLiveEnabled(t)
	contract := structured.MustContract("grok-live", []byte(`{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean","const":true}},"additionalProperties":false}`))
	adapter := grokLiveAdapter(t)
	spec := grokLiveSpec(t, contract.WithPromptSuffix("Complete the output contract with ok set to true."))
	result := grokLiveCollect(t, adapter, spec)
	var decoded struct {
		OK bool `json:"ok"`
	}
	decoded, _, err := structured.Decode[struct {
		OK bool `json:"ok"`
	}](contract, result.Output)
	require.NoError(t, err, "structured contract was not satisfied")
	assert.True(t, decoded.OK)
	t.Logf("structured contract passed: turns=%d has_cost=%t", result.Usage.Turns, result.Usage.HasCost)
}
