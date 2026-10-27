package grok

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
)

func TestLiveReadOnlyFailsClosed(t *testing.T) {
	grokLiveEnabled(t)
	adapter := grokLiveAdapter(t)
	spec := grokLiveSpec(t, "This session must not start under read_only.")
	spec.ToolPolicy = cartridge.ToolReadOnly
	session, err := adapter.Run(t.Context(), spec)
	if session != nil {
		session.Kill()
	}
	assert.Nil(t, session)
	assert.ErrorIs(t, err, errReadOnlyUnsupported)
	t.Log("read_only refused unsafe startup")
}

func TestLiveReadWriteNativeEdit(t *testing.T) {
	grokLiveEnabled(t)
	adapter := grokLiveAdapter(t)
	spec := grokLiveSpec(t, "Use the native search_replace tool on native.txt to replace BEFORE with AFTER. Do not use run_terminal_cmd. Then reply DONE.")
	spec.ToolPolicy = cartridge.ToolReadWrite
	spec.MaxTurns = 4
	path := filepath.Join(spec.Workdir, "native.txt")
	require.NoError(t, os.WriteFile(path, []byte("BEFORE\n"), 0o600))
	_, events := grokLiveCollectEvents(t, adapter, spec)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "AFTER\n", string(data))
	assert.True(t, grokLiveToolSucceeded(events, "search_replace"), "read_write native edit did not succeed")
	t.Log("read_write native edit passed")
}

func TestLiveReadExecShellAllowedNativeEditDenied(t *testing.T) {
	grokLiveEnabled(t)
	adapter := grokLiveAdapter(t)
	spec := grokLiveSpec(t, "First use run_terminal_cmd to execute `printf 'AFTER\\n' > shell.txt`. Then try the native search_replace tool on native.txt to replace BEFORE with AFTER. Do not use run_terminal_cmd for native.txt. Reply DONE.")
	spec.ToolPolicy = cartridge.ToolReadExec
	spec.MaxTurns = 6
	nativePath := filepath.Join(spec.Workdir, "native.txt")
	require.NoError(t, os.WriteFile(nativePath, []byte("BEFORE\n"), 0o600))
	_, events := grokLiveCollectEvents(t, adapter, spec)
	shellData, err := os.ReadFile(filepath.Join(spec.Workdir, "shell.txt"))
	require.NoError(t, err, "read_exec shell command did not write its expected file")
	assert.Equal(t, "AFTER\n", string(shellData))
	nativeData, err := os.ReadFile(nativePath)
	require.NoError(t, err)
	assert.Equal(t, "BEFORE\n", string(nativeData), "read_exec native edit changed the file")
	assert.False(t, grokLiveToolSucceeded(events, "search_replace"), "read_exec native edit unexpectedly succeeded")
	t.Log("read_exec shell allow and native-edit denial passed")
}

func TestLiveNoSubagents(t *testing.T) {
	grokLiveEnabled(t)
	adapter := grokLiveAdapter(t)
	spec := grokLiveSpec(t, "Use the Agent tool to spawn a subagent whose only task is to create subagent.txt. Do not create the file yourself and do not use another tool. Reply DONE when finished or if Agent is unavailable.")
	spec.ToolPolicy = cartridge.ToolReadWrite
	spec.MaxTurns = 3
	_, events := grokLiveCollectEvents(t, adapter, spec)
	for _, ev := range events {
		if ev.Kind == cartridge.EventToolUse {
			assert.False(t, strings.EqualFold(ev.Tool, "Agent"), "Agent tool was offered despite no-subagents")
		}
	}
	_, err := os.Stat(filepath.Join(spec.Workdir, "subagent.txt"))
	assert.ErrorIs(t, err, os.ErrNotExist)
	t.Log("no-subagents behavior passed")
}

func TestLiveWebToggle(t *testing.T) {
	grokLiveEnabled(t)
	adapter := grokLiveAdapter(t)
	for _, enabled := range []bool{false, true} {
		spec := grokLiveSpec(t, "Use web_fetch exactly once for https://example.com and then reply DONE. Do not use another web tool or shell.")
		spec.Web = enabled
		spec.MaxTurns = 3
		_, events := grokLiveCollectEvents(t, adapter, spec)
		used := grokLiveToolCalled(events, "web_fetch") || grokLiveToolCalled(events, "web_search")
		succeeded := grokLiveToolSucceeded(events, "web_fetch") || grokLiveToolSucceeded(events, "web_search")
		if enabled && !succeeded {
			for _, ev := range events {
				if ev.Kind == cartridge.EventToolResult && ev.IsError {
					t.Logf("web tool error=%q", truncate(ev.Output, 300))
				}
			}
		}
		if enabled {
			assert.True(t, used, "web-enabled session did not call a web tool")
			assert.True(t, succeeded, "web-enabled tool call did not succeed")
		} else {
			assert.False(t, used, "web-disabled session called a web tool")
		}
		t.Logf("web toggle passed: enabled=%t tool_used=%t", enabled, used)
	}
}

func grokLiveCommand(events []cartridge.SessionEvent, command string) (called, succeeded, permissionDenied bool) {
	ids := map[string]bool{}
	for _, ev := range events {
		if ev.Kind == cartridge.EventToolUse && (ev.Tool == terminalTool || ev.Tool == "run_terminal_command") && strings.Contains(ev.Input, command) {
			called = true
			ids[ev.ToolID] = true
		}
	}
	for _, ev := range events {
		if ev.Kind != cartridge.EventToolResult || !ids[ev.ToolID] {
			continue
		}
		if !ev.IsError {
			succeeded = true
		}
		output := strings.ToLower(ev.Output)
		permissionDenied = permissionDenied || strings.Contains(output, "permission") && (strings.Contains(output, "denied") || strings.Contains(output, "not allowed"))
	}
	return called, succeeded, permissionDenied
}

func grokLiveToolCalled(events []cartridge.SessionEvent, name string) bool {
	for _, ev := range events {
		if ev.Kind == cartridge.EventToolUse && ev.Tool == name {
			return true
		}
	}
	return false
}

func grokLiveToolSucceeded(events []cartridge.SessionEvent, name string) bool {
	ids := map[string]bool{}
	for _, ev := range events {
		if ev.Kind == cartridge.EventToolUse && ev.Tool == name {
			ids[ev.ToolID] = true
		}
	}
	for _, ev := range events {
		if ev.Kind == cartridge.EventToolResult && ids[ev.ToolID] && !ev.IsError {
			return true
		}
	}
	return false
}
