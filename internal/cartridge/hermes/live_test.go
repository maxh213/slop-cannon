package hermes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/conformance"
	"slop-cannon/internal/cartridge/structured"
	"slop-cannon/internal/config"
)

// These tests are opt-in because they use the configured Nous account and
// network. They never log prompts, responses, tool output or environment.
const liveEnv = "SC_HERMES_LIVE"

func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv(liveEnv) != "1" {
		t.Skipf("set %s=1 for live Hermes verification", liveEnv)
	}
}

func liveAdapter(t *testing.T) *Adapter {
	t.Helper()
	return hermesAdapter(t, config.Cartridge{Model: os.Getenv("SC_HERMES_LIVE_MODEL")})
}

func liveSpec(t *testing.T, prompt string, policy cartridge.ToolPolicy) cartridge.RunSpec {
	t.Helper()
	return cartridge.RunSpec{
		SystemPrompt: "You are an offline-safe verification fixture. Follow the request exactly and be brief.",
		Prompt:       prompt, Workdir: t.TempDir(), ToolPolicy: policy,
		MaxTurns: 4, Timeout: 3 * time.Minute,
	}
}

func liveCollect(t *testing.T, session cartridge.Session) (cartridge.Result, []cartridge.SessionEvent, error) {
	t.Helper()
	var events []cartridge.SessionEvent
	result, err := cartridge.Collect(session, func(event cartridge.SessionEvent) { events = append(events, event) })
	return result, events, err
}

func liveRun(t *testing.T, adapter *Adapter, spec cartridge.RunSpec) (cartridge.Result, []cartridge.SessionEvent, error) {
	t.Helper()
	session, err := adapter.Run(context.Background(), spec)
	require.NoError(t, err)
	return liveCollect(t, session)
}

func liveHasToken(text, token string) bool {
	return strings.Contains(strings.ToLower(text), strings.ToLower(token))
}

func liveHomeSnapshot(t *testing.T) map[string]bool {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(os.TempDir(), "sc-hermes-*"))
	require.NoError(t, err)
	out := make(map[string]bool, len(paths))
	for _, path := range paths {
		out[path] = true
	}
	return out
}

func liveNewHomes(t *testing.T, before map[string]bool) []string {
	t.Helper()
	after := liveHomeSnapshot(t)
	var out []string
	for path := range after {
		if !before[path] {
			out = append(out, path)
		}
	}
	return out
}

func liveAssertHomesRemoved(t *testing.T, homes []string) {
	t.Helper()
	for _, home := range homes {
		_, err := os.Stat(home)
		assert.ErrorIs(t, err, os.ErrNotExist, "temporary Hermes state was not cleaned")
	}
}

func liveProcesses(t *testing.T) map[int]bool {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return map[int]bool{}
	}
	out := map[int]bool{}
	for _, entry := range entries {
		if !entry.IsDir() || strings.Trim(entry.Name(), "0123456789") != "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		cmdline := string(data)
		if strings.Contains(cmdline, "/hermes-agent/hermes") || strings.Contains(cmdline, "tail -f /dev/null") {
			var pid int
			_, err := fmt.Sscanf(entry.Name(), "%d", &pid)
			if err == nil {
				out[pid] = true
			}
		}
	}
	return out
}

func liveAssertNoNewHelpers(t *testing.T, before map[int]bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		after := liveProcesses(t)
		survivors := 0
		for pid := range after {
			if !before[pid] {
				survivors++
			}
		}
		if survivors == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a Hermes or terminal helper survived cancellation")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestLiveDetect(t *testing.T) {
	requireLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	status, err := liveAdapter(t).Detect(ctx)
	require.NoError(t, err)
	assert.True(t, status.Installed)
	assert.Equal(t, VerifiedVersion, status.Version)
	assert.True(t, status.Ready())
}

func TestLiveEcho(t *testing.T) {
	requireLive(t)
	token := conformance.NewToken()
	spec := liveSpec(t, conformance.EchoPrompt(token), cartridge.ToolReadExec)
	spec.MaxTurns = 1
	result, _, err := liveRun(t, liveAdapter(t), spec)
	require.NoError(t, err)
	assert.True(t, liveHasToken(result.Output, token), "echo response did not contain the sentinel")
}

func TestLiveStructured(t *testing.T) {
	requireLive(t)
	token := conformance.NewToken()
	spec := liveSpec(t, conformance.StructuredPrompt(token), cartridge.ToolReadExec)
	out, err := structured.Fire(context.Background(), liveAdapter(t), spec, conformance.Contract())
	require.NoError(t, err)
	var reply struct {
		Token string `json:"token"`
		OK    bool   `json:"ok"`
	}
	require.NoError(t, out.Parsed.Decode(&reply))
	assert.True(t, strings.EqualFold(strings.TrimSpace(reply.Token), token), "structured response used the wrong sentinel")
	assert.True(t, reply.OK)
}

func TestLiveReadExecTerminalWithoutNativeEdit(t *testing.T) {
	requireLive(t)
	token := conformance.NewToken()
	spec := liveSpec(t, "Check whether native write_file or patch tools are available, without using terminal for that check. Then use terminal to create sc-live-read-exec.txt in the current directory containing exactly "+token+". Do not use native edit tools.", cartridge.ToolReadExec)
	result, events, err := liveRun(t, liveAdapter(t), spec)
	require.NoError(t, err)
	terminal := false
	for _, event := range events {
		if event.Kind != cartridge.EventToolUse {
			continue
		}
		assert.NotEqual(t, "write_file", event.Tool, "read_exec exposed native write_file")
		assert.NotEqual(t, "patch", event.Tool, "read_exec exposed native patch")
		terminal = terminal || event.Tool == "terminal"
	}
	assert.True(t, terminal, "read_exec did not invoke terminal")
	data, err := os.ReadFile(filepath.Join(spec.Workdir, "sc-live-read-exec.txt"))
	require.NoError(t, err)
	assert.True(t, liveHasToken(string(data), token), "terminal did not create the sentinel")
	assert.NotEmpty(t, result.SessionID)
}

func TestLiveReadWriteNativeEdit(t *testing.T) {
	requireLive(t)
	token := conformance.NewToken()
	spec := liveSpec(t, "Use the native write_file tool, not terminal, to create sc-live-read-write.txt in the current directory containing exactly "+token+".", cartridge.ToolReadWrite)
	_, events, err := liveRun(t, liveAdapter(t), spec)
	require.NoError(t, err)
	nativeEdit := false
	for _, event := range events {
		if event.Kind == cartridge.EventToolUse && (event.Tool == "write_file" || event.Tool == "patch") {
			nativeEdit = true
		}
	}
	assert.True(t, nativeEdit, "read_write did not invoke a native edit tool")
	data, err := os.ReadFile(filepath.Join(spec.Workdir, "sc-live-read-write.txt"))
	require.NoError(t, err)
	assert.True(t, liveHasToken(string(data), token), "native edit did not create the sentinel")
}

func TestLiveConcurrentHomesAuthenticateIndependently(t *testing.T) {
	requireLive(t)
	adapter := liveAdapter(t)
	before := liveHomeSnapshot(t)
	firstToken, secondToken := conformance.NewToken(), conformance.NewToken()
	firstSpec := liveSpec(t, conformance.EchoPrompt(firstToken), cartridge.ToolReadExec)
	secondSpec := liveSpec(t, conformance.EchoPrompt(secondToken), cartridge.ToolReadExec)
	first, err := adapter.Run(context.Background(), firstSpec)
	require.NoError(t, err)
	second, err := adapter.Run(context.Background(), secondSpec)
	require.NoError(t, err)
	homes := liveNewHomes(t, before)
	require.Len(t, homes, 2, "concurrent firings did not get separate temporary roots")
	for _, root := range homes {
		for _, dir := range []string{filepath.Join(root, "hermes"), filepath.Join(root, "home")} {
			info, statErr := os.Stat(dir)
			require.NoError(t, statErr)
			assert.True(t, info.IsDir())
		}
	}
	type outcome struct {
		result cartridge.Result
		err    error
	}
	firstDone := make(chan outcome, 1)
	go func() {
		result, err := cartridge.Collect(first, nil)
		firstDone <- outcome{result: result, err: err}
	}()
	secondResult, err := cartridge.Collect(second, nil)
	require.NoError(t, err)
	firstResult := <-firstDone
	require.NoError(t, firstResult.err)
	assert.True(t, liveHasToken(firstResult.result.Output, firstToken), "first concurrent response used the wrong sentinel")
	assert.True(t, liveHasToken(secondResult.Output, secondToken), "second concurrent response used the wrong sentinel")
	liveAssertHomesRemoved(t, homes)
}

func TestLiveTimeoutCleansProcessesAndHome(t *testing.T) {
	requireLive(t)
	beforeProcesses := liveProcesses(t)
	beforeHomes := liveHomeSnapshot(t)
	spec := liveSpec(t, conformance.HangPrompt(), cartridge.ToolReadExec)
	spec.Timeout = 20 * time.Second
	spec.MaxTurns = 2
	session, err := liveAdapter(t).Run(context.Background(), spec)
	require.NoError(t, err)
	homes := liveNewHomes(t, beforeHomes)
	require.Len(t, homes, 1)
	_, _, err = liveCollect(t, session)
	require.Error(t, err)
	assert.True(t, errors.Is(err, cartridge.ErrTimeout), "timeout ended with %s", cartridge.KindOf(err))
	liveAssertHomesRemoved(t, homes)
	liveAssertNoNewHelpers(t, beforeProcesses)
}

func TestLiveKillCleansProcessesAndHome(t *testing.T) {
	requireLive(t)
	beforeProcesses := liveProcesses(t)
	beforeHomes := liveHomeSnapshot(t)
	spec := liveSpec(t, conformance.HangPrompt(), cartridge.ToolReadExec)
	spec.MaxTurns = 2
	session, err := liveAdapter(t).Run(context.Background(), spec)
	require.NoError(t, err)
	homes := liveNewHomes(t, beforeHomes)
	require.Len(t, homes, 1)
	terminal := false
	fallback := time.AfterFunc(90*time.Second, session.Kill)
	for event := range session.Events() {
		if event.Kind == cartridge.EventToolUse && event.Tool == "terminal" {
			terminal = true
			session.Kill()
		}
	}
	fallback.Stop()
	_, err = session.Wait()
	require.Error(t, err)
	assert.True(t, terminal, "the model did not start the requested terminal helper")
	assert.True(t, errors.Is(err, cartridge.ErrKilled), "kill ended with %s", cartridge.KindOf(err))
	liveAssertHomesRemoved(t, homes)
	liveAssertNoNewHelpers(t, beforeProcesses)
}
