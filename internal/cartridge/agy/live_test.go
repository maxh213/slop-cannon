package agy

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
	"slop-cannon/internal/cartridge/conformance"
	"slop-cannon/internal/config"
)

func liveEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("SC_AGY_LIVE") != "1" {
		t.Skip("set SC_AGY_LIVE=1 to run bounded checks against the real agy CLI")
	}
}

func liveAdapter(t *testing.T) *Adapter {
	t.Helper()
	a := newAdapter(t, config.Cartridge{Model: os.Getenv("SC_AGY_LIVE_MODEL")})
	return a
}

func liveContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 4*time.Minute)
}

func runLiveConformance(t *testing.T, check string) {
	t.Helper()
	liveEnabled(t)
	ctx, cancel := liveContext(t)
	defer cancel()
	report, err := conformance.Run(ctx, liveAdapter(t), conformance.Options{
		Model: os.Getenv("SC_AGY_LIVE_MODEL"), Timeout: 90 * time.Second,
		Checks: []string{check},
	})
	require.NoError(t, err)
	for _, result := range report.Results {
		if result.Check != check {
			continue
		}
		if result.Status != conformance.StatusPass {
			t.Fatalf("live %s did not pass: status=%s kind=%s", check, result.Status, cartridge.KindOf(result.Err))
		}
		t.Logf("live %s: pass (%d input, %d output tokens)", check, result.Usage.InputTokens, result.Usage.OutputTokens)
	}
}

func TestLive_Detect(t *testing.T) {
	liveEnabled(t)
	ctx, cancel := liveContext(t)
	defer cancel()
	st, err := liveAdapter(t).Detect(ctx)
	require.NoError(t, err)
	assert.True(t, st.Installed)
	assert.Equal(t, VerifiedVersion, st.Version)
	assert.Equal(t, cartridge.AuthUnknown, st.Auth)
	assert.True(t, st.Ready())
	t.Logf("live detect: pass (version=%s, auth=%s)", st.Version, st.Auth)
}

func TestLive_Echo(t *testing.T) {
	runLiveConformance(t, conformance.CheckEcho)
}

func TestLive_Structured(t *testing.T) {
	runLiveConformance(t, conformance.CheckStructured)
}

func TestLive_ReadWrite(t *testing.T) {
	runLiveConformance(t, conformance.CheckWrite)
}

func TestLive_Timeout(t *testing.T) {
	liveEnabled(t)
	ctx, cancel := liveContext(t)
	defer cancel()
	spec := cartridge.RunSpec{
		Prompt: conformance.EchoPrompt("sc-live-timeout"), Workdir: t.TempDir(),
		Model: os.Getenv("SC_AGY_LIVE_MODEL"), ToolPolicy: cartridge.ToolReadWrite,
		Timeout: time.Second,
	}
	s, err := liveAdapter(t).Run(ctx, spec)
	require.NoError(t, err)
	_, err = cartridge.Collect(s, nil)
	require.ErrorIs(t, err, cartridge.ErrTimeout)
	t.Log("live timeout: pass")
}

func TestLive_Kill(t *testing.T) {
	liveEnabled(t)
	ctx, cancel := liveContext(t)
	defer cancel()
	spec := cartridge.RunSpec{
		Prompt: conformance.EchoPrompt("sc-live-kill"), Workdir: t.TempDir(),
		Model: os.Getenv("SC_AGY_LIVE_MODEL"), ToolPolicy: cartridge.ToolReadWrite,
		Timeout: 90 * time.Second,
	}
	s, err := liveAdapter(t).Run(ctx, spec)
	require.NoError(t, err)
	time.AfterFunc(100*time.Millisecond, s.Kill)
	_, err = cartridge.Collect(s, nil)
	require.ErrorIs(t, err, cartridge.ErrKilled)
	t.Log("live kill: pass")
}

func TestLive_PlanWriteRefusal(t *testing.T) {
	liveEnabled(t)
	ctx, cancel := liveContext(t)
	defer cancel()
	workdir := t.TempDir()
	existing := filepath.Join(workdir, "existing.txt")
	const before = "original canary content\n"
	require.NoError(t, os.WriteFile(existing, []byte(before), 0o600))
	spec := cartridge.RunSpec{
		Prompt:  "Attempt both writes with your file-writing tool: create AGY-PLAN-WRITE-CANARY and replace existing.txt. Then report whether the writes were refused.",
		Workdir: workdir, Model: os.Getenv("SC_AGY_LIVE_MODEL"),
		ToolPolicy: cartridge.ToolReadOnly, Timeout: 90 * time.Second,
	}
	s, err := liveAdapter(t).Run(ctx, spec)
	require.NoError(t, err)
	res, err := cartridge.Collect(s, nil)
	if err != nil {
		t.Fatalf("plan canary session failed: kind=%s files_created=%t response_empty=%t", cartridge.KindOf(err), planMarkerExists(workdir), res.Output == "")
	}
	if planMarkerExists(workdir) {
		t.Fatal("plan canary failed: plan mode created the new marker")
	}
	after, readErr := os.ReadFile(existing)
	require.NoError(t, readErr)
	if string(after) != before {
		t.Fatal("plan canary failed: plan mode modified the existing file")
	}
	entries, readErr := os.ReadDir(workdir)
	require.NoError(t, readErr)
	if len(entries) != 1 || entries[0].Name() != "existing.txt" {
		t.Fatalf("plan canary failed: workdir contains %d entries", len(entries))
	}
	if res.Output == "" {
		t.Fatal("plan canary failed: empty successful response")
	}
	t.Log("live plan write-refusal: pass (both files unchanged, parseable nonempty response)")
}

func TestLive_LargePrompt(t *testing.T) {
	liveEnabled(t)
	ctx, cancel := liveContext(t)
	defer cancel()
	spec := cartridge.RunSpec{
		Prompt:  strings.Repeat("Inert context sentence.\n", 7000) + conformance.EchoPrompt("sc-large-live"),
		Workdir: t.TempDir(), Model: os.Getenv("SC_AGY_LIVE_MODEL"),
		ToolPolicy: cartridge.ToolReadWrite, Timeout: 90 * time.Second,
	}
	require.Greater(t, len(Prompt(spec)), 128*1024)
	s, err := liveAdapter(t).Run(ctx, spec)
	require.NoError(t, err)
	res, err := cartridge.Collect(s, nil)
	if err != nil {
		t.Fatalf("large-prompt smoke failed: kind=%s", cartridge.KindOf(err))
	}
	if !strings.Contains(res.Output, "sc-large-live") {
		t.Fatal("large-prompt smoke failed: token missing")
	}
	assert.Empty(t, promptFiles(t, spec.Workdir))
	t.Logf("live large prompt: pass (%d input, %d output tokens)", res.Usage.InputTokens, res.Usage.OutputTokens)
}

func planMarkerExists(workdir string) bool {
	_, err := os.Stat(filepath.Join(workdir, "AGY-PLAN-WRITE-CANARY"))
	return err == nil || !errors.Is(err, os.ErrNotExist)
}
