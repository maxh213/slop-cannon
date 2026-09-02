package logging

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"slop-cannon/internal/domain"
)

func TestPathsFor(t *testing.T) {
	p := PathsFor("/state/slop-cannon/repo/runs/r1/")
	want := RunPaths{
		Dir:       filepath.FromSlash("/state/slop-cannon/repo/runs/r1"),
		Log:       filepath.FromSlash("/state/slop-cannon/repo/runs/r1/sc.log"),
		AgentsDir: filepath.FromSlash("/state/slop-cannon/repo/runs/r1/agents"),
	}
	if p != want {
		t.Errorf("PathsFor = %+v, want %+v", p, want)
	}
	if got := p.AgentLog("engineer-1"); got != filepath.Join(want.AgentsDir, "engineer-1.log") {
		t.Errorf("AgentLog = %q", got)
	}
	got := p.AgentLog("../../etc/passwd")
	if got != filepath.Join(want.AgentsDir, "___.__etc_passwd.log") || strings.Contains(got, "..") {
		t.Errorf("AgentLog did not neutralise traversal: %q", got)
	}
}

func TestSafeFileName(t *testing.T) {
	cases := map[string]string{
		"engineer-1":   "engineer-1",
		"po.1_a":       "po.1_a",
		"":             "_",
		"..":           "__",
		".hidden":      "_hidden",
		"a/b\\c d":     "a_b_c_d",
		"tester:ux#2":  "tester_ux_2",
		"ünïcode":      "__n__code",
		"a..b":         "a._b",
		"..hidden":     "__hidden",
		"ok.with.dots": "ok.with.dots",
	}
	for in, want := range cases {
		if got := SafeFileName(in); got != want {
			t.Errorf("SafeFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func readJSONLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("%s: bad line %q: %v", path, sc.Text(), err)
		}
		out = append(out, rec)
	}
	return out
}

func TestOpenRunLogPlacementAndRedaction(t *testing.T) {
	state := t.TempDir()
	runDir := filepath.Join(state, "runs", "run-42")

	rl, err := OpenRunLog(runDir, "run-42", Options{})
	if err != nil {
		t.Fatalf("OpenRunLog: %v", err)
	}
	if rl.Paths() != PathsFor(runDir) {
		t.Errorf("Paths = %+v", rl.Paths())
	}
	rl.Logger().Info("run starting", "token", "sk-abcdefghijklmnop0001")

	eng, err := rl.Agent("engineer-1")
	if err != nil {
		t.Fatalf("Agent: %v", err)
	}
	eng.Info("spawned", "auth", "Bearer abcdefghijklmnop0002")
	again, err := rl.Agent("engineer-1")
	if err != nil || again != eng {
		t.Errorf("Agent should return the cached logger, got %p/%p err=%v", again, eng, err)
	}
	tst, err := rl.Agent("tester/ux")
	if err != nil {
		t.Fatalf("Agent: %v", err)
	}
	tst.Warn("odd id")

	if err := rl.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := rl.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := rl.Agent("late"); !errors.Is(err, ErrClosed) {
		t.Errorf("Agent after Close = %v, want ErrClosed", err)
	}

	// Placement.
	for _, p := range []string{
		filepath.Join(runDir, "sc.log"),
		filepath.Join(runDir, "agents", "engineer-1.log"),
		filepath.Join(runDir, "agents", "tester_ux.log"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected log file: %v", err)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(runDir, "agents"))
	if len(entries) != 2 {
		t.Errorf("agents dir has %d entries, want 2", len(entries))
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(filepath.Join(runDir, "sc.log")); fi.Mode().Perm() != 0o600 {
			t.Errorf("sc.log perm = %o, want 0600", fi.Mode().Perm())
		}
		if fi, _ := os.Stat(filepath.Join(runDir, "agents")); fi.Mode().Perm() != 0o700 {
			t.Errorf("agents dir perm = %o, want 0700", fi.Mode().Perm())
		}
	}

	// Content and redaction.
	run := readJSONLines(t, filepath.Join(runDir, "sc.log"))
	if len(run) != 1 || run[0]["msg"] != "run starting" || run[0]["run"] != "run-42" {
		t.Fatalf("sc.log = %v", run)
	}
	if run[0]["token"] != "sk-[REDACTED]" {
		t.Errorf("sc.log leaked token: %v", run[0]["token"])
	}
	agent := readJSONLines(t, filepath.Join(runDir, "agents", "engineer-1.log"))
	if len(agent) != 1 || agent[0]["agent"] != "engineer-1" || agent[0]["run"] != "run-42" {
		t.Fatalf("agent log = %v", agent)
	}
	if agent[0]["auth"] != "Bearer [REDACTED]" {
		t.Errorf("agent log leaked token: %v", agent[0]["auth"])
	}
	raw, _ := os.ReadFile(filepath.Join(runDir, "agents", "engineer-1.log"))
	if strings.Contains(string(raw), "abcdefghijklmnop0002") {
		t.Errorf("raw agent log leaks secret: %s", raw)
	}
}

func TestOpenRunLogAppendsOnReopen(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "runs", "r")
	for i := 0; i < 2; i++ {
		rl, err := OpenRunLog(runDir, "r", Options{})
		if err != nil {
			t.Fatal(err)
		}
		rl.Logger().Info("open", "i", i)
		if err := rl.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if got := readJSONLines(t, filepath.Join(runDir, "sc.log")); len(got) != 2 {
		t.Errorf("reopen should append, got %d lines", len(got))
	}
}

func TestOpenRunLogErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission semantics differ")
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRunLog(filepath.Join(blocker, "run"), "r", Options{}); err == nil {
		t.Error("expected error when run dir cannot be created")
	}
}

func TestRunLoggerConcurrentAgents(t *testing.T) {
	rl, err := OpenRunLog(filepath.Join(t.TempDir(), "run"), "r", Options{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := domain.AgentID([]string{"a", "b", "c"}[i%3])
			for j := 0; j < 20; j++ {
				l, err := rl.Agent(id)
				if err != nil {
					t.Error(err)
					return
				}
				l.Info("tick", "j", j)
				rl.Logger().Info("tock", "agent", id)
			}
		}(i)
	}
	wg.Wait()
	if err := rl.CloseAgent("a"); err != nil {
		t.Errorf("CloseAgent: %v", err)
	}
	if err := rl.CloseAgent("never-opened"); err != nil {
		t.Errorf("CloseAgent unknown: %v", err)
	}
	if err := rl.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	dir := rl.Paths().AgentsDir
	for _, id := range []string{"a", "b", "c"} {
		lines := readJSONLines(t, filepath.Join(dir, id+".log"))
		if len(lines) == 0 {
			t.Errorf("agent %s has no lines", id)
		}
	}
	if n := len(readJSONLines(t, rl.Paths().Log)); n != 160 {
		t.Errorf("sc.log has %d lines, want 160", n)
	}
}
