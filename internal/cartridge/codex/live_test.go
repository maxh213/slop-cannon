package codex_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/codex"
	"slop-cannon/internal/cartridge/dandelion"
	"slop-cannon/internal/cartridge/structured"
	"slop-cannon/internal/config"
	"slop-cannon/internal/testutil"
)

const (
	liveModel = "gpt-5.1-codex-mini"
	liveToken = "sc-codex-live-ok"
)

var liveContract = structured.MustContract("codex-live", []byte(`{
  "type":"object",
  "required":["token","ok"],
  "properties":{"token":{"type":"string","const":"`+liveToken+`"},"ok":{"type":"boolean","const":true}},
  "additionalProperties":false
}`))

func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv("SC_CODEX_LIVE") != "1" {
		t.Skip("set SC_CODEX_LIVE=1 for real Codex verification")
	}
}

func liveAdapter(t *testing.T, name string, cfg config.Cartridge) cartridge.Cartridge {
	t.Helper()
	if home := os.Getenv("SC_CODEX_LIVE_HOME"); home != "" {
		env := make(map[string]string, len(cfg.Env)+1)
		for key, value := range cfg.Env {
			env[key] = value
		}
		env["CODEX_HOME"] = home
		cfg.Env = env
	}
	c, err := codex.New(name, cfg)
	if err != nil {
		t.Fatal("could not construct Codex adapter")
	}
	return c
}

func liveSpec(t *testing.T, prompt string, policy cartridge.ToolPolicy) cartridge.RunSpec {
	t.Helper()
	model := liveModel
	if override := os.Getenv("SC_CODEX_LIVE_MODEL"); override != "" {
		model = override
	}
	return cartridge.RunSpec{
		SystemPrompt: "You are running an automated adapter check. Follow the request exactly and do not inspect unrelated files.",
		Prompt:       prompt, Workdir: t.TempDir(), Model: model, ToolPolicy: policy,
		Timeout: 90 * time.Second,
	}
}

func collectLive(c cartridge.Cartridge, spec cartridge.RunSpec) (cartridge.Result, []cartridge.SessionEvent, error) {
	session, err := c.Run(context.Background(), spec)
	if err != nil {
		return cartridge.Result{}, nil, err
	}
	var events []cartridge.SessionEvent
	result, err := cartridge.Collect(session, func(event cartridge.SessionEvent) {
		events = append(events, event)
	})
	return result, events, err
}

func failLive(t *testing.T, check string, err error) {
	t.Helper()
	t.Fatalf("%s failed (%s)", check, cartridge.KindOf(err))
}

func blockedLive(err error) bool {
	switch cartridge.KindOf(err) {
	case cartridge.KindAuth, cartridge.KindCreditsExhausted, cartridge.KindRateLimited:
		return true
	default:
		return false
	}
}

func TestLiveDetectEchoStructured(t *testing.T) {
	requireLive(t)
	c := liveAdapter(t, codex.Name, config.Cartridge{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	status, err := c.Detect(ctx)
	if err != nil {
		failLive(t, "detect", err)
	}
	if !status.Installed || !status.Ready() || status.Version != "0.164.0" {
		t.Fatal("detect returned an unexpected installation state")
	}

	echo, events, err := collectLive(c, liveSpec(t, "Reply with exactly "+liveToken+" and nothing else. Do not use tools.", cartridge.ToolReadExec))
	if err != nil {
		failLive(t, "echo", err)
	}
	if !strings.Contains(echo.Output, liveToken) || !strings.Contains(cartridge.Transcript(events), liveToken) {
		t.Fatal("echo output or streamed transcript did not contain the token")
	}
	if echo.SessionID == "" || echo.Usage.InputTokens <= 0 || echo.Usage.OutputTokens <= 0 {
		t.Fatal("echo omitted session or usage data")
	}

	outcome, err := structured.Fire(context.Background(), c,
		liveSpec(t, "Report token "+liveToken+" with ok=true using the required structured block. Do not use tools.", cartridge.ToolReadExec),
		liveContract, structured.WithMaxRetries(0))
	if err != nil {
		failLive(t, "structured", err)
	}
	var reply struct {
		Token string `json:"token"`
		OK    bool   `json:"ok"`
	}
	if err := outcome.Parsed.Decode(&reply); err != nil {
		t.Fatal("structured result did not decode")
	}
	if reply.Token != liveToken || !reply.OK {
		t.Fatal("structured result did not satisfy the contract")
	}
}

func TestLiveNativePolicies(t *testing.T) {
	requireLive(t)
	c := liveAdapter(t, codex.Name, config.Cartridge{})

	writeSpec := liveSpec(t, "Create sc-live-write.txt in the current working directory containing exactly "+liveToken+" and nothing else, then stop.", cartridge.ToolReadWrite)
	_, _, err := collectLive(c, writeSpec)
	if err != nil {
		failLive(t, "read_write", err)
	}
	data, err := os.ReadFile(filepath.Join(writeSpec.Workdir, "sc-live-write.txt"))
	if err != nil || strings.TrimSpace(string(data)) != liveToken {
		t.Fatal("read_write did not create the requested file content")
	}

	readExecSpec := liveSpec(t, "First use the shell to run `pwd` as its own command. Then, as a separate shell command, attempt exactly `printf blocked > sc-live-forbidden.txt`. Do not use file-editing tools. Finish after observing whether that write succeeded.", cartridge.ToolReadExec)
	_, events, err := collectLive(c, readExecSpec)
	if err != nil {
		failLive(t, "read_exec", err)
	}
	shellSucceeded := false
	for _, event := range events {
		if event.Kind == cartridge.EventToolResult && event.Tool == "command_execution" && !event.IsError {
			shellSucceeded = true
		}
	}
	if !shellSucceeded {
		t.Fatal("read_exec did not complete a shell command")
	}
	entries, err := os.ReadDir(readExecSpec.Workdir)
	if err != nil {
		t.Fatal("could not check the read_exec workdir")
	}
	if len(entries) != 0 {
		t.Fatal("read_exec modified the filesystem")
	}
}

func TestLiveTimeoutAndKill(t *testing.T) {
	requireLive(t)
	c := liveAdapter(t, codex.Name, config.Cartridge{})

	timeoutSpec := liveSpec(t, "Run the shell command `sleep 60` in the foreground and wait for it. Do not run it in the background and do nothing else.", cartridge.ToolReadExec)
	timeoutSpec.Timeout = 8 * time.Second
	_, _, err := collectLive(c, timeoutSpec)
	if !errors.Is(err, cartridge.ErrTimeout) {
		t.Fatal("timeout session did not return ErrTimeout")
	}

	killSpec := liveSpec(t, "Run the shell command `sleep 60` in the foreground and wait for it. Do not run it in the background and do nothing else.", cartridge.ToolReadExec)
	killSpec.Timeout = 60 * time.Second
	session, err := c.Run(context.Background(), killSpec)
	if err != nil {
		failLive(t, "kill start", err)
	}
	time.AfterFunc(2*time.Second, session.Kill)
	_, err = cartridge.Collect(session, nil)
	if !errors.Is(err, cartridge.ErrKilled) {
		t.Fatal("killed session did not return ErrKilled")
	}
}

func TestLiveDandelionAnimus(t *testing.T) {
	requireLive(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal("could not resolve the account parent directory")
	}
	animusHome := filepath.Join(home, ".codex-animus")
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "nonexistent-personal-home"))
	t.Setenv("DANDELION_CODEX_ANIMUS_HOME", animusHome)

	isolated := liveAdapter(t, "dandelion", config.Cartridge{Env: map[string]string{"CODEX_HOME": animusHome}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	status, detectErr := isolated.Detect(ctx)
	cancel()
	if detectErr != nil {
		if blockedLive(detectErr) {
			t.Skipf("Animus account unavailable (%s)", cartridge.KindOf(detectErr))
		}
		failLive(t, "Animus detect", detectErr)
	}
	if !status.Ready() {
		t.Skip("Animus account has no usable login")
	}

	directSpec := liveSpec(t, "Reply with exactly "+liveToken+" and nothing else. Do not use tools.", cartridge.ToolReadWrite)
	directSpec.Model = ""
	direct, _, err := collectLive(isolated, directSpec)
	if err != nil {
		if blockedLive(err) {
			t.Skipf("isolated Animus smoke blocked (%s)", cartridge.KindOf(err))
		}
		failLive(t, "isolated Animus smoke", err)
	}
	if !strings.Contains(direct.Output, liveToken) {
		t.Fatal("isolated Animus smoke returned the wrong token")
	}

	testutil.FakeBin(t, "dandelion", `printf '%s\n' 'gpt-6.1-sol high codex-animus'`)
	router, err := dandelion.New(dandelion.Name, config.Cartridge{Model: "standard"})
	if err != nil {
		t.Fatal("could not construct Dandelion adapter")
	}
	delegated, _, err := collectLive(router, liveSpec(t, "Reply with exactly "+liveToken+" and nothing else. Do not use tools.", cartridge.ToolReadWrite))
	if err != nil {
		if blockedLive(err) {
			t.Skipf("delegated Animus smoke blocked (%s)", cartridge.KindOf(err))
		}
		failLive(t, "delegated Animus smoke", err)
	}
	if !strings.Contains(delegated.Output, liveToken) {
		t.Fatal("delegated Animus smoke returned the wrong token")
	}
}
