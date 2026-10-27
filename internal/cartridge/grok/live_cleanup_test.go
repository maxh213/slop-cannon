package grok

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
)

func TestLiveTimeoutAndProcessCleanup(t *testing.T) {
	grokLiveEnabled(t)
	if runtime.GOOS != "linux" {
		t.Skip("live process audit requires procfs")
	}
	adapter := grokLiveAdapter(t)
	before := grokLiveProcessSet(t, "grok", "")
	spec := grokLiveSpec(t, "Reply with one word after a long pause.")
	spec.Timeout = 250 * time.Millisecond
	session, err := adapter.Run(context.Background(), spec)
	require.NoError(t, err)
	result, err := cartridge.Collect(session, nil)
	if err == nil || cartridge.KindOf(err) != cartridge.KindTimeout {
		t.Fatalf("timeout outcome kind=%s", cartridge.KindOf(err))
	}
	assert.False(t, result.Usage.HasCost)
	grokLiveRequireNoNewProcesses(t, before, "grok", "")
	t.Log("timeout and CLI/leader cleanup passed")
}

func TestLiveKillReapsToolDescendant(t *testing.T) {
	grokLiveEnabled(t)
	if runtime.GOOS != "linux" {
		t.Skip("live process audit requires procfs")
	}
	adapter := grokLiveAdapter(t)
	beforeGrok := grokLiveProcessSet(t, "grok", "")
	beforeSleep := grokLiveProcessSet(t, "sleep", "123.456")
	spec := grokLiveSpec(t, "Call run_terminal_cmd exactly once with `sleep 123.456`. Do not use another command and do not reply until the command finishes.")
	spec.ToolPolicy = cartridge.ToolReadExec
	spec.MaxTurns = 2
	session, err := adapter.Run(context.Background(), spec)
	require.NoError(t, err)

	sawTool := false
	for ev := range session.Events() {
		if ev.Kind == cartridge.EventToolUse && (ev.Tool == terminalTool || ev.Tool == "run_terminal_command") && strings.Contains(ev.Input, "123.456") {
			sawTool = true
			break
		}
	}
	if !sawTool {
		session.Kill()
		_, _ = session.Wait()
		t.Fatal("sleep tool was not attempted")
	}
	require.Eventually(t, func() bool {
		return len(grokLiveProcessSet(t, "sleep", "123.456")) > len(beforeSleep)
	}, 5*time.Second, 25*time.Millisecond, "unique tool descendant started")
	session.Kill()
	for range session.Events() {
	}
	result, err := session.Wait()
	if err == nil || cartridge.KindOf(err) != cartridge.KindKilled {
		t.Fatalf("kill outcome kind=%s", cartridge.KindOf(err))
	}
	assert.False(t, result.Usage.HasCost)
	grokLiveRequireNoNewProcesses(t, beforeSleep, "sleep", "123.456")
	grokLiveRequireNoNewProcesses(t, beforeGrok, "grok", "")
	t.Log("kill, CLI/leader, and tool-descendant cleanup passed")
}

func grokLiveProcessSet(t *testing.T, kind, marker string) map[int]bool {
	t.Helper()
	out := map[int]bool{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		comm, err := os.ReadFile("/proc/" + entry.Name() + "/comm")
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(comm))
		if kind == "grok" && !strings.Contains(name, "grok") || kind == "sleep" && name != "sleep" {
			continue
		}
		if marker != "" {
			cmdline, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
			if err != nil || !strings.Contains(string(cmdline), marker) {
				continue
			}
		}
		out[pid] = true
	}
	return out
}

func grokLiveRequireNoNewProcesses(t *testing.T, before map[int]bool, kind, marker string) {
	t.Helper()
	require.Eventually(t, func() bool {
		current := grokLiveProcessSet(t, kind, marker)
		for pid := range current {
			if !before[pid] {
				return false
			}
		}
		return true
	}, 6*time.Second, 50*time.Millisecond, "new %s process survived cleanup", kind)
}
