package kilo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/structured"
	"slop-cannon/internal/config"
)

const kiloLiveModel = "deepseek/deepseek-v4.1-flash-1017"

func kiloLiveEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("SC_KILO_LIVE") != "1" {
		t.Skip("set SC_KILO_LIVE=1 for bounded live Kilo verification")
	}
}

func kiloLiveAdapter(t *testing.T) *Adapter {
	t.Helper()
	return kiloAdapter(t, config.Cartridge{Model: kiloLiveModel})
}

func kiloLiveSpec(t *testing.T, prompt string, policy cartridge.ToolPolicy) cartridge.RunSpec {
	t.Helper()
	return cartridge.RunSpec{
		SystemPrompt: "You are a non-sensitive adapter verification fixture. Follow the request exactly and do not inspect unrelated files or configuration.",
		Prompt:       prompt, Workdir: t.TempDir(), Model: kiloLiveModel, ToolPolicy: policy,
		MaxTurns: 4, Timeout: 90 * time.Second,
	}
}

func kiloLiveCollect(t *testing.T, adapter *Adapter, spec cartridge.RunSpec) (cartridge.Result, []cartridge.SessionEvent) {
	t.Helper()
	session, err := adapter.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("live startup failed with kind %s", cartridge.KindOf(err))
	}
	var events []cartridge.SessionEvent
	result, err := cartridge.Collect(session, func(event cartridge.SessionEvent) { events = append(events, event) })
	if err != nil {
		category := "process or provider failure"
		diagnostic := strings.ToLower(err.Error())
		switch {
		case strings.Contains(diagnostic, "without a valid step_finish"):
			category = "missing valid step_finish"
		case strings.Contains(diagnostic, "malformed step_finish"):
			category = "malformed step_finish"
		case strings.Contains(diagnostic, "without an assistant"), strings.Contains(diagnostic, "no output"):
			category = "empty model response"
		case strings.Contains(diagnostic, "not found"), strings.Contains(diagnostic, "does not exist"):
			category = "requested object not found"
		case strings.Contains(diagnostic, "not supported"), strings.Contains(diagnostic, "unsupported"):
			category = "unsupported request"
		case strings.Contains(diagnostic, "invalid"):
			category = "invalid request"
		case strings.Contains(diagnostic, "config"):
			category = "config rejection"
		case strings.Contains(diagnostic, "model"), strings.Contains(diagnostic, "provider"):
			category = "model/provider rejection"
		case strings.Contains(diagnostic, "permission"):
			category = "permission rejection"
		case strings.Contains(diagnostic, "agent"):
			category = "agent rejection"
		}
		t.Fatalf("live session failed with kind %s (%s, exit=%d, events=%d, turns=%d)", cartridge.KindOf(err), category, result.ExitCode, len(events), result.Usage.Turns)
	}
	if result.ExitCode != 0 {
		t.Fatal("live session returned a nonzero exit status")
	}
	return result, events
}

func TestLiveDetect(t *testing.T) {
	kiloLiveEnabled(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	status, err := kiloLiveAdapter(t).Detect(ctx)
	if err != nil {
		t.Fatalf("detect failed with kind %s", cartridge.KindOf(err))
	}
	assert.True(t, status.Installed)
	assert.True(t, status.Ready())
	assert.Equal(t, VerifiedVersion, status.Version)
}

func TestLiveEchoUsageAndFreshBalanceAllowance(t *testing.T) {
	kiloLiveEnabled(t)
	adapter := kiloLiveAdapter(t)
	spec := kiloLiveSpec(t, "Reply with exactly KILO-LIVE-OK and nothing else. Do not call tools.", cartridge.ToolReadExec)
	spec.MaxTurns = 1
	spec.Env = map[string]string{minBalanceEnv: "10"}
	allowanceBefore, err := adapter.checkBalance(context.Background(), spec)
	if err != nil {
		t.Fatalf("initial balance check failed with kind %s", cartridge.KindOf(err))
	}
	result, _ := kiloLiveCollect(t, adapter, spec)
	if !strings.Contains(result.Output, "KILO-LIVE-OK") {
		t.Fatal("echo response did not contain the synthetic token")
	}
	assert.NotEmpty(t, result.SessionID)
	assert.Positive(t, result.Usage.InputTokens)
	assert.Positive(t, result.Usage.OutputTokens)
	assert.Positive(t, result.Usage.Turns)
	assert.True(t, result.Usage.HasCost)
	allowanceAfter, err := adapter.checkBalance(context.Background(), spec)
	if err != nil {
		t.Fatalf("final balance check failed with kind %s", cartridge.KindOf(err))
	}
	assert.GreaterOrEqual(t, result.Usage.CostUSD, 0.0)
	assert.Less(t, result.Usage.CostUSD, allowanceBefore)
	assert.Less(t, result.Usage.CostUSD, allowanceAfter)
}

func TestLiveStructuredContract(t *testing.T) {
	kiloLiveEnabled(t)
	adapter := kiloLiveAdapter(t)
	contract := structured.MustContract("kilo-live", []byte(`{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean","const":true}},"additionalProperties":false}`))
	spec := kiloLiveSpec(t, "Complete the output contract with ok set to true. Do not call tools.", cartridge.ToolReadExec)
	spec.MaxTurns = 1
	outcome, err := structured.Fire(context.Background(), adapter, spec, contract, structured.WithMaxRetries(0))
	if err != nil {
		t.Fatalf("structured session failed with kind %s", cartridge.KindOf(err))
	}
	var reply struct {
		OK bool `json:"ok"`
	}
	if err := outcome.Parsed.Decode(&reply); err != nil || !reply.OK {
		t.Fatal("structured result did not satisfy the contract")
	}
}

func TestLiveReadWriteNativeCreation(t *testing.T) {
	kiloLiveEnabled(t)
	adapter := kiloLiveAdapter(t)
	spec := kiloLiveSpec(t, "Use the native write tool exactly once to create native-write.txt containing exactly native-ok. Do not use bash. Finish with exactly WRITE-DONE.", cartridge.ToolReadWrite)
	result, events := kiloLiveCollect(t, adapter, spec)
	assert.NotEmpty(t, result.SessionID)
	assert.True(t, kiloLiveToolSucceeded(events, "write"), "native write did not complete successfully")
	data, err := os.ReadFile(filepath.Join(spec.Workdir, "native-write.txt"))
	require.NoError(t, err)
	assert.Equal(t, "native-ok", strings.TrimSpace(string(data)))
}

func TestLiveReadExecShellAndNativeDenials(t *testing.T) {
	kiloLiveEnabled(t)
	adapter := kiloLiveAdapter(t)
	spec := kiloLiveSpec(t, "First use bash exactly once to run `printf 'shell-ok\\n' > shell.txt`. Then attempt the native write tool to create native-denied.txt, and attempt the task tool to create task-denied.txt. Do not use bash for either denied file. Finish with exactly POLICY-DONE.", cartridge.ToolReadExec)
	spec.MaxTurns = 6
	_, events := kiloLiveCollect(t, adapter, spec)
	assert.True(t, kiloLiveToolSucceeded(events, "bash"), "read_exec bash did not complete successfully")
	assert.False(t, kiloLiveToolSucceeded(events, "write"), "native write succeeded under read_exec")
	assert.False(t, kiloLiveToolSucceeded(events, "task"), "native task succeeded under read_exec")
	data, err := os.ReadFile(filepath.Join(spec.Workdir, "shell.txt"))
	require.NoError(t, err)
	assert.Equal(t, "shell-ok", strings.TrimSpace(string(data)))
	assert.NoFileExists(t, filepath.Join(spec.Workdir, "native-denied.txt"))
	assert.NoFileExists(t, filepath.Join(spec.Workdir, "task-denied.txt"))
}

func TestLiveReadOnlyRefusedBeforeBalanceOrSession(t *testing.T) {
	kiloLiveEnabled(t)
	adapter := kiloLiveAdapter(t)
	spec := kiloLiveSpec(t, "This must not start a profile or model session.", cartridge.ToolReadOnly)
	spec.Env = map[string]string{minBalanceEnv: "invalid-floor-canary"}
	session, err := adapter.Run(context.Background(), spec)
	if session != nil {
		session.Kill()
	}
	assert.Nil(t, session)
	assert.ErrorIs(t, err, ErrReadOnlyUnavailable)
	assert.NotErrorIs(t, err, ErrBudget)
}

func TestLiveLargeStdin(t *testing.T) {
	kiloLiveEnabled(t)
	adapter := kiloLiveAdapter(t)
	spec := kiloLiveSpec(t, strings.Repeat("Non-sensitive padding.\n", 7000)+"Reply with exactly LARGE-OK and nothing else. Do not call tools.", cartridge.ToolReadExec)
	spec.MaxTurns = 1
	require.Greater(t, len(Stdin(spec)), 128*1024)
	result, _ := kiloLiveCollect(t, adapter, spec)
	if !strings.Contains(result.Output, "LARGE-OK") {
		t.Fatal("large-stdin response did not contain the synthetic token")
	}
}

func TestLiveTimeoutCleanup(t *testing.T) {
	kiloLiveEnabled(t)
	kiloLiveRequireProcfs(t)
	before := kiloLiveProcessSet(t, "kilo", "")
	spec := kiloLiveSpec(t, "Take several minutes before answering. Do not call tools.", cartridge.ToolReadExec)
	spec.Timeout = 500 * time.Millisecond
	session, err := kiloLiveAdapter(t).Run(context.Background(), spec)
	require.NoError(t, err)
	_, err = cartridge.Collect(session, nil)
	assert.True(t, errors.Is(err, cartridge.ErrTimeout), "timeout outcome kind=%s", cartridge.KindOf(err))
	kiloLiveRequireNoNewProcesses(t, before, "kilo", "")
}

func TestLiveKillReapsKiloAndToolDescendants(t *testing.T) {
	kiloLiveEnabled(t)
	kiloLiveRequireProcfs(t)
	beforeKilo := kiloLiveProcessSet(t, "kilo", "")
	beforeSleep := kiloLiveProcessSet(t, "sleep", "123.456")
	spec := kiloLiveSpec(t, "Use bash exactly once to run `sleep 123.456` in the foreground and wait for it. Do not use another command.", cartridge.ToolReadExec)
	spec.MaxTurns = 2
	session, err := kiloLiveAdapter(t).Run(context.Background(), spec)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return len(kiloLiveProcessSet(t, "sleep", "123.456")) > len(beforeSleep)
	}, 45*time.Second, 50*time.Millisecond, "unique synthetic tool descendant did not start")
	session.Kill()
	_, err = cartridge.Collect(session, nil)
	assert.True(t, errors.Is(err, cartridge.ErrKilled), "kill outcome kind=%s", cartridge.KindOf(err))
	kiloLiveRequireNoNewProcesses(t, beforeSleep, "sleep", "123.456")
	kiloLiveRequireNoNewProcesses(t, beforeKilo, "kilo", "")
}

func kiloLiveToolSucceeded(events []cartridge.SessionEvent, name string) bool {
	ids := map[string]bool{}
	for _, event := range events {
		if event.Kind == cartridge.EventToolUse && event.Tool == name {
			ids[event.ToolID] = true
		}
	}
	for _, event := range events {
		if event.Kind == cartridge.EventToolResult && ids[event.ToolID] && !event.IsError {
			return true
		}
	}
	return false
}

func kiloLiveRequireProcfs(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("live process audit requires procfs")
	}
}

func kiloLiveProcessSet(t *testing.T, kind, marker string) map[int]bool {
	t.Helper()
	processes := map[int]bool{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return processes
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
		if kind == "kilo" && !strings.Contains(strings.ToLower(name), "kilo") || kind == "sleep" && name != "sleep" {
			continue
		}
		if marker != "" {
			cmdline, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
			if err != nil || !strings.Contains(string(cmdline), marker) {
				continue
			}
		}
		processes[pid] = true
	}
	return processes
}

func kiloLiveRequireNoNewProcesses(t *testing.T, before map[int]bool, kind, marker string) {
	t.Helper()
	require.Eventually(t, func() bool {
		current := kiloLiveProcessSet(t, kind, marker)
		for pid := range current {
			if !before[pid] {
				return false
			}
		}
		return true
	}, 6*time.Second, 50*time.Millisecond, "new %s process survived cleanup", kind)
}
