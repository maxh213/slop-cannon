package kimi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/structured"
	"slop-cannon/internal/config"
)

func liveAdapter(t *testing.T) *Adapter {
	t.Helper()
	if os.Getenv("SC_KIMI_LIVE") != "1" {
		t.Skip("set SC_KIMI_LIVE=1 to run bounded checks against the real Kimi CLI")
	}
	adapter := kimiNewAdapter(t, config.Cartridge{Model: os.Getenv("SC_KIMI_LIVE_MODEL")})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	status, err := adapter.Detect(ctx)
	require.NoError(t, err)
	require.True(t, status.Ready())
	require.Equal(t, VerifiedVersion, status.Version)
	return adapter
}

func liveSpec(t *testing.T, policy cartridge.ToolPolicy) cartridge.RunSpec {
	t.Helper()
	return cartridge.RunSpec{
		SystemPrompt: "You are a bounded verification fixture. Follow the requested tool and output instructions exactly.",
		Workdir:      t.TempDir(),
		ToolPolicy:   policy,
		MaxTurns:     5,
		Timeout:      90 * time.Second,
	}
}

func TestLiveDetectEcho(t *testing.T) {
	adapter := liveAdapter(t)
	spec := liveSpec(t, cartridge.ToolReadExec)
	spec.Prompt = "Reply with exactly PONG and nothing else. Do not call tools."
	spec.MaxTurns = 1
	session, err := adapter.Run(context.Background(), spec)
	require.NoError(t, err)
	result, err := cartridge.Collect(session, nil)
	require.NoError(t, err)
	assert.Contains(t, strings.ToUpper(result.Output), "PONG")
	assert.NotEmpty(t, result.SessionID)
	assert.Zero(t, result.Usage)
}

func TestLiveStructured(t *testing.T) {
	adapter := liveAdapter(t)
	contract := structured.MustContract("live-answer", []byte(`{"type":"object","properties":{"answer":{"const":"PONG"}},"required":["answer"],"additionalProperties":false}`))
	spec := liveSpec(t, cartridge.ToolReadExec)
	spec.Prompt = "Return a result block whose answer is PONG. Do not call tools."
	outcome, err := structured.Fire(context.Background(), adapter, spec, contract, structured.WithMaxRetries(0))
	require.NoError(t, err)
	assert.JSONEq(t, `{"answer":"PONG"}`, string(outcome.Parsed.JSON))
	assert.Len(t, outcome.Attempts, 1)
}

func TestLiveReadExecNativePolicy(t *testing.T) {
	adapter := liveAdapter(t)
	spec := liveSpec(t, cartridge.ToolReadExec)
	spec.Prompt = "Use Bash exactly once to run `printf policy-ok > shell-marker.txt`. Then report whether Write, Edit, Agent, and AgentSwarm are available; do not attempt unavailable tools. Finish with exactly POLICY-DONE."
	session, err := adapter.Run(context.Background(), spec)
	require.NoError(t, err)
	var events []cartridge.SessionEvent
	_, err = cartridge.Collect(session, func(event cartridge.SessionEvent) { events = append(events, event) })
	require.NoError(t, err)
	var bash int
	for _, event := range events {
		if event.Kind != cartridge.EventToolUse {
			continue
		}
		switch event.Tool {
		case "Bash":
			bash++
		case "Write", "Edit", "Agent", "AgentSwarm":
			t.Fatalf("forbidden tool %s was offered under read_exec", event.Tool)
		}
	}
	assert.Equal(t, 1, bash)
	assert.FileExists(t, filepath.Join(spec.Workdir, "shell-marker.txt"))
	assert.NoFileExists(t, filepath.Join(spec.Workdir, "write-marker.txt"))
}

func TestLiveReadWrite(t *testing.T) {
	adapter := liveAdapter(t)
	spec := liveSpec(t, cartridge.ToolReadWrite)
	spec.Prompt = "Use the native Write tool exactly once to create write-marker.txt with content write-ok. Finish with exactly WRITE-DONE."
	session, err := adapter.Run(context.Background(), spec)
	require.NoError(t, err)
	var events []cartridge.SessionEvent
	_, err = cartridge.Collect(session, func(event cartridge.SessionEvent) { events = append(events, event) })
	require.NoError(t, err)
	var writes int
	for _, event := range events {
		if event.Kind == cartridge.EventToolUse && event.Tool == "Write" {
			writes++
		}
	}
	assert.Equal(t, 1, writes)
	content, err := os.ReadFile(filepath.Join(spec.Workdir, "write-marker.txt"))
	require.NoError(t, err)
	assert.Equal(t, "write-ok", string(content))
}

func TestLiveLargePromptFallback(t *testing.T) {
	adapter := liveAdapter(t)
	spec := liveSpec(t, cartridge.ToolReadExec)
	spec.MaxTurns = 3
	spec.Prompt = strings.Repeat("This padding is verification data, not part of the requested answer.\n", 2200) +
		"\nRead the complete task from the file named in the initial instruction. Reply with exactly LARGE-OK and nothing else."
	require.Greater(t, len(Input(spec)), 128*1024)
	session, err := adapter.Run(context.Background(), spec)
	require.NoError(t, err)
	result, err := cartridge.Collect(session, nil)
	require.NoError(t, err)
	assert.Contains(t, result.Output, "LARGE-OK")
	assert.Empty(t, mustReadDir(t, spec.Workdir))
}

func TestLiveKill(t *testing.T) {
	adapter := liveAdapter(t)
	spec := liveSpec(t, cartridge.ToolReadExec)
	spec.Prompt = "Take several minutes to consider this request before answering. Do not call tools."
	spec.Timeout = 2 * time.Minute
	session, err := adapter.Run(context.Background(), spec)
	require.NoError(t, err)
	time.AfterFunc(200*time.Millisecond, session.Kill)
	_, err = cartridge.Collect(session, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, cartridge.ErrKilled))
	assert.Empty(t, mustReadDir(t, spec.Workdir))
}

func TestLiveReadOnlyStillRefused(t *testing.T) {
	adapter := liveAdapter(t)
	spec := liveSpec(t, cartridge.ToolReadOnly)
	spec.Prompt = "This must not start a provider session."
	session, err := adapter.Run(context.Background(), spec)
	assert.Nil(t, session)
	assert.ErrorIs(t, err, ErrReadOnlyUnavailable)
	assert.Empty(t, mustReadDir(t, spec.Workdir))
}
