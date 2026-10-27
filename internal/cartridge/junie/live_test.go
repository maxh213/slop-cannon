package junie

import (
	"context"
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

const junieLiveModel = "gemini-3.1-flash-lite"

func liveAdapter(t *testing.T) *Adapter {
	t.Helper()
	if os.Getenv("SC_JUNIE_LIVE") != "1" {
		t.Skip("set SC_JUNIE_LIVE=1 to run against the real junie CLI")
	}
	return junieAdapter(t, config.Cartridge{})
}

func liveSpec(t *testing.T, prompt string) cartridge.RunSpec {
	t.Helper()
	return cartridge.RunSpec{
		SystemPrompt: "You are a test fixture. Follow the requested output exactly and do not add explanation.",
		Prompt:       prompt,
		Workdir:      t.TempDir(),
		Model:        junieLiveModel,
		ToolPolicy:   cartridge.ToolReadWrite,
		Timeout:      2 * time.Minute,
	}
}

// TestLiveDetectAndEcho checks the installed CLI and one minimal response.
func TestLiveDetectAndEcho(t *testing.T) {
	a := liveAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	st, err := a.Detect(ctx)
	require.NoError(t, err, "detect: %+v", st)
	assert.True(t, st.Ready())
	assert.Equal(t, cartridge.AuthUnknown, st.Auth)
	t.Logf("detect: %+v", st)

	s, err := a.Run(ctx, liveSpec(t, "Reply with exactly the word PONG and nothing else. Do not use tools."))
	require.NoError(t, err)
	res, err := cartridge.Collect(s, func(ev cartridge.SessionEvent) { t.Log(cartridge.Describe(ev)) })
	t.Logf("result: exit=%d session=%q output=%q usage=%+v", res.ExitCode, res.SessionID, res.Output, res.Usage)
	require.NoError(t, err)
	assert.Contains(t, strings.ToUpper(res.Output), "PONG")
	assert.Equal(t, 0, res.ExitCode)
	assert.NotEmpty(t, res.SessionID)
	assert.Positive(t, res.Duration)
	assert.Positive(t, res.Usage.InputTokens)
	assert.Positive(t, res.Usage.OutputTokens)
	assert.Positive(t, res.Usage.Turns)
	t.Logf("usage: %+v; cost reported: %v", res.Usage, res.Usage.HasCost)
}

var liveContract = structured.MustContract("junie-live", []byte(`{
  "type":"object",
  "required":["token","ok"],
  "properties":{"token":{"type":"string"},"ok":{"type":"boolean","const":true}},
  "additionalProperties":false
}`))

type liveStructuredReply struct {
	Token string `json:"token"`
	OK    bool   `json:"ok"`
}

// TestLiveStructured checks a validated sc-result block in the final JSON result.
func TestLiveStructured(t *testing.T) {
	a := liveAdapter(t)
	prompt := liveContract.WithPromptSuffix("Report the token LIVE-STRUCTURED with ok true. Do not use tools.")
	s, err := a.Run(context.Background(), liveSpec(t, prompt))
	require.NoError(t, err)
	res, err := cartridge.Collect(s, nil)
	t.Logf("result: exit=%d session=%q usage=%+v", res.ExitCode, res.SessionID, res.Usage)
	require.NoError(t, err)
	reply, _, err := structured.Decode[liveStructuredReply](liveContract, res.Output)
	require.NoError(t, err)
	assert.Equal(t, liveStructuredReply{Token: "LIVE-STRUCTURED", OK: true}, reply)
	assert.Positive(t, res.Usage.OutputTokens)
}

// TestLiveReadWrite checks a real file edit in the firing worktree.
func TestLiveReadWrite(t *testing.T) {
	a := liveAdapter(t)
	spec := liveSpec(t, "Create a file named sc-junie-live.txt in the current project containing exactly one line: LIVE-WRITE. Use your file-writing tool and do nothing else.")
	s, err := a.Run(context.Background(), spec)
	require.NoError(t, err)
	res, err := cartridge.Collect(s, nil)
	t.Logf("result: exit=%d session=%q usage=%+v", res.ExitCode, res.SessionID, res.Usage)
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(spec.Workdir, "sc-junie-live.txt"))
	require.NoError(t, err)
	assert.Equal(t, "LIVE-WRITE", strings.TrimSpace(string(data)))
}

// TestLiveLargePrompt checks file-indirection compliance without logging the prompt.
func TestLiveLargePrompt(t *testing.T) {
	a := liveAdapter(t)
	prompt := "Reply with exactly LARGE-OK and nothing else. Do not use tools.\n" + strings.Repeat(" ", 129*1024)
	spec := liveSpec(t, prompt)
	s, err := a.Run(context.Background(), spec)
	require.NoError(t, err)
	res, err := cartridge.Collect(s, nil)
	t.Logf("large prompt: bytes=%d exit=%d session=%q usage=%+v", len(prompt), res.ExitCode, res.SessionID, res.Usage)
	require.NoError(t, err)
	assert.Contains(t, strings.ToUpper(res.Output), "LARGE-OK")
	leftovers, err := filepath.Glob(filepath.Join(spec.Workdir, promptFilePattern))
	require.NoError(t, err)
	assert.Empty(t, leftovers)
}
