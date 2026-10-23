package dandelion

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/claude"
	"slop-cannon/internal/config"
	"slop-cannon/internal/testutil"
)

// routerShimScript is the fake "dandelion". It records DANDELION_DISABLE and
// whether a Claude Code session marker reached it, then answers from files
// in its own directory so a test can change the answer between calls.
const routerShimScript = `
d="$SC_FAKEBIN_DIR"
printf '%s' "$DANDELION_DISABLE" > "$d/disable.$SC_FAKEBIN_SEQ"
printf '%s' "${CLAUDECODE-unset}" > "$d/marker.$SC_FAKEBIN_SEQ"
printf '%s' "${SC_ROUTER_KNOB-unset}" > "$d/knob.$SC_FAKEBIN_SEQ"
if [ -f "$d/sleep" ]; then sleep "$(cat "$d/sleep")"; fi
if [ -f "$d/stderr" ]; then cat "$d/stderr" >&2; fi
if [ -f "$d/line" ]; then cat "$d/line"; fi
if [ -f "$d/exit" ]; then exit "$(cat "$d/exit")"; fi
exit 0
`

// claudeShimScript is the fake "claude": probes answer at once, a firing
// records its CLAUDE_CONFIG_DIR and replays a fixture or fails with the
// stderr and exit status the spec's environment asks for.
const claudeShimScript = `
case "$1" in
  --version) echo "2.1.309 (Claude Code)"; exit 0;;
  auth) echo '{"loggedIn":true,"email":"user@example.com","subscriptionType":"max"}'; exit 0;;
esac
printf '%s' "${CLAUDE_CONFIG_DIR-unset}" > "$SC_FAKEBIN_DIR/config.$SC_FAKEBIN_SEQ"
if [ -n "$SC_CLAUDE_STDERR" ]; then echo "$SC_CLAUDE_STDERR" >&2; fi
if [ -n "$SC_CLAUDE_FIXTURE" ]; then cat "$SC_CLAUDE_FIXTURE"; fi
exit "${SC_CLAUDE_EXIT:-0}"
`

const cursorShimScript = `
if [ -n "$SC_CURSOR_FIXTURE" ]; then cat "$SC_CURSOR_FIXTURE"; fi
exit 0
`

// fakes are the shims one test runs against.
type fakes struct {
	router, claude, cursor *testutil.Shim
}

// isolate gives the test a PATH with only the system tools the shims need,
// so a real dandelion, claude or cursor-agent on the machine is never found.
// It uses t.Setenv, so callers must not be parallel.
func isolate(t *testing.T, withCursor bool) fakes {
	t.Helper()
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv(disableEnv, "")
	t.Setenv("CLAUDE_CONFIG_DIR", "/sc-env/claude-home")
	f := fakes{
		router: testutil.FakeBin(t, DefaultBinary, routerShimScript),
		claude: testutil.FakeBin(t, "claude", claudeShimScript, testutil.WithStdinCapture()),
	}
	if withCursor {
		f.cursor = testutil.FakeBin(t, "cursor-agent", cursorShimScript, testutil.WithStdinCapture())
	}
	return f
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// say makes the fake router print line and exit 0.
func say(t *testing.T, router *testutil.Shim, line string) {
	t.Helper()
	writeFile(t, filepath.Join(router.Dir, "line"), line+"\n")
	_ = os.Remove(filepath.Join(router.Dir, "exit"))
}

// fail makes the fake router exit code with stderr.
func fail(t *testing.T, router *testutil.Shim, code int, stderr string) {
	t.Helper()
	writeFile(t, filepath.Join(router.Dir, "exit"), strconv.Itoa(code))
	writeFile(t, filepath.Join(router.Dir, "stderr"), stderr)
	_ = os.Remove(filepath.Join(router.Dir, "line"))
}

// recorded reads what call seq of a shim wrote to name.
func recorded(t *testing.T, s *testutil.Shim, name string, seq int) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.Dir, name+"."+strconv.Itoa(seq)))
	require.NoError(t, err)
	return string(data)
}

// firings returns the argument lists of a shim's session calls (probes
// excluded).
func firings(t *testing.T, s *testutil.Shim) [][]string {
	t.Helper()
	var out [][]string
	for _, args := range s.ArgLists(t) {
		if len(args) > 0 && (args[0] == "--version" || args[0] == "auth") {
			continue
		}
		out = append(out, args)
	}
	return out
}

func fixture(t *testing.T, dir, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", dir, name))
	require.NoError(t, err)
	_, err = os.Stat(p)
	require.NoError(t, err)
	return p
}

// newAdapter builds the adapter with a log buffer and a controllable clock.
func newAdapter(t *testing.T, cfg config.Cartridge) (*Adapter, *bytes.Buffer, *fakeClock) {
	t.Helper()
	c, err := New(Name, cfg)
	require.NoError(t, err)
	a, ok := c.(*Adapter)
	require.True(t, ok)
	var logs bytes.Buffer
	a.logger = slog.New(slog.NewTextHandler(&logs, nil))
	clock := &fakeClock{t: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	a.now = clock.Now
	return a, &logs, clock
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func spec(t *testing.T, policy cartridge.ToolPolicy, env map[string]string) cartridge.RunSpec {
	t.Helper()
	return cartridge.RunSpec{
		Prompt:     "Reply with exactly the word PONG",
		Workdir:    t.TempDir(),
		Model:      "sonnet",
		ToolPolicy: policy,
		Timeout:    20 * time.Second,
		Env:        env,
		Labels:     cartridge.Labels{cartridge.LabelAgent: "eng-1"},
	}
}

// okEnv makes the fake claude replay a successful session.
func okEnv(t *testing.T) map[string]string {
	return map[string]string{"SC_CLAUDE_FIXTURE": fixture(t, "claude", "text-reply.jsonl")}
}

func collect(t *testing.T, s cartridge.Session) ([]cartridge.SessionEvent, cartridge.Result, error) {
	t.Helper()
	var evs []cartridge.SessionEvent
	res, err := cartridge.Collect(s, func(ev cartridge.SessionEvent) { evs = append(evs, ev) })
	return evs, res, err
}

func fire(t *testing.T, a *Adapter, sp cartridge.RunSpec) error {
	t.Helper()
	s, err := a.Run(context.Background(), sp)
	if err != nil {
		return err
	}
	_, _, err = collect(t, s)
	return err
}

func cerr(t *testing.T, err error) *cartridge.Error {
	t.Helper()
	require.Error(t, err)
	var ce *cartridge.Error
	require.ErrorAs(t, err, &ce)
	return ce
}

func TestRegistered(t *testing.T) {
	c, err := cartridge.New(Name, config.Cartridge{Model: "high"})
	require.NoError(t, err)
	assert.Equal(t, Name, c.Name())
	a, ok := c.(*Adapter)
	require.True(t, ok)
	assert.Equal(t, ModeHigh, a.Mode())
}

func TestNew_Modes(t *testing.T) {
	for model, want := range map[string]Mode{"high": ModeHigh, " HIGH ": ModeHigh, "standard": ModeStandard, "": ModeStandard} {
		c, err := New(Name, config.Cartridge{Model: model})
		require.NoError(t, err, model)
		a, ok := c.(*Adapter)
		require.True(t, ok)
		assert.Equal(t, want, a.Mode(), model)
	}
	_, err := New(Name, config.Cartridge{Model: "claude-opus-5-5"})
	require.EqualError(t, err, `cartridge "dandelion" takes model "high" or "standard", not "claude-opus-5-5": the router picks the real model`)
}

func TestParseRoute(t *testing.T) {
	r, err := ParseRoute("claude-fable-5-1 max claude")
	require.NoError(t, err)
	assert.Equal(t, Route{Model: "claude-fable-5-1", Effort: "max", Account: "claude"}, r)
	assert.Equal(t, "claude-fable-5-1 max claude", r.String())
	r, err = ParseRoute("auto cursor")
	require.NoError(t, err)
	assert.Equal(t, Route{Model: "auto", Account: "cursor"}, r)
	assert.Equal(t, "auto cursor", r.String())
	for _, bad := range []string{"", "claude", "a b c d"} {
		_, err := ParseRoute(bad)
		assert.Error(t, err, bad)
	}
}

func TestRun_RouterArgsPerMode(t *testing.T) {
	f := isolate(t, false)
	say(t, f.router, "claude-opus-5-5 high claude")

	high, _, _ := newAdapter(t, config.Cartridge{Model: "high", Args: []string{"--why-not"}})
	require.NoError(t, fire(t, high, spec(t, cartridge.ToolReadOnly, okEnv(t))))
	std, _, _ := newAdapter(t, config.Cartridge{})
	require.NoError(t, fire(t, std, spec(t, cartridge.ToolReadOnly, okEnv(t))))

	assert.Equal(t, [][]string{
		{"route", "--high", "--max-age", "120", "--why-not"},
		{"route", "--max-age", "120"},
	}, f.router.ArgLists(t))
}

func TestRun_DelegatesToClaudeWithRoutedModelAndEffort(t *testing.T) {
	f := isolate(t, false)
	say(t, f.router, "claude-fable-5-1 max claude")
	a, logs, _ := newAdapter(t, config.Cartridge{Model: "high"})

	s, err := a.Run(context.Background(), spec(t, cartridge.ToolReadOnly, okEnv(t)))
	require.NoError(t, err)
	evs, res, err := collect(t, s)
	require.NoError(t, err)
	assert.NotEmpty(t, res.Output)
	require.NotEmpty(t, evs)
	assert.Equal(t, cartridge.EventDone, evs[len(evs)-1].Kind)

	calls := firings(t, f.claude)
	require.Len(t, calls, 1)
	args := calls[0]
	i := slices.Index(args, "--model")
	require.GreaterOrEqual(t, i, 0)
	assert.Equal(t, "claude-fable-5-1", args[i+1], "the routed model wins over spec.Model")
	assert.NotContains(t, args, "sonnet")
	assert.Equal(t, []string{"--effort", "max"}, args[len(args)-2:])
	assert.Equal(t, claude.ToolFlags(cartridge.ToolReadOnly), args[slices.Index(args, "--permission-mode"):len(args)-2],
		"the read_only firing carries Claude Code's own tool controls")
	// The default account inherits sc's CLAUDE_CONFIG_DIR, as launch.ts does.
	assert.Equal(t, "/sc-env/claude-home", recorded(t, f.claude, "config", 1))

	log := logs.String()
	for _, want := range []string{"dandelion routed a firing", "agent=eng-1", "account=claude", "model=claude-fable-5-1", "effort=max", "mode=high", "policy=read_only", "cached=false"} {
		assert.Contains(t, log, want)
	}
}

func TestRun_ClaudeConfigDirPerAccount(t *testing.T) {
	f := isolate(t, false)
	home := t.TempDir()
	t.Setenv("HOME", home)
	cases := []struct{ account, want string }{
		{"claude-work", "/custom/work"},
		{"claude-deepseek", filepath.Join(home, ".claude-deepseek")},
		{"claude-ollama", filepath.Join(home, ".claude-ollama")},
	}
	for i, tc := range cases {
		say(t, f.router, "some-model "+tc.account)
		a, _, _ := newAdapter(t, config.Cartridge{Env: map[string]string{"DANDELION_CLAUDE_WORK_CONFIG_DIR": "/custom/work"}})
		require.NoError(t, fire(t, a, spec(t, cartridge.ToolReadExec, okEnv(t))), tc.account)
		// Each case is one firing; probes do not write the config file.
		seq := len(f.claude.ArgLists(t))
		assert.Equal(t, tc.want, recorded(t, f.claude, "config", seq), "case %d %s", i, tc.account)
	}
	// An empty override falls back to the home directory, as in launch.ts.
	say(t, f.router, "some-model claude-work")
	a, _, _ := newAdapter(t, config.Cartridge{Env: map[string]string{"DANDELION_CLAUDE_WORK_CONFIG_DIR": ""}})
	require.NoError(t, fire(t, a, spec(t, cartridge.ToolReadExec, okEnv(t))))
	assert.Equal(t, filepath.Join(home, ".claude-work"), recorded(t, f.claude, "config", len(f.claude.ArgLists(t))))
}

func TestRun_EffortOnlyWhenValid(t *testing.T) {
	f := isolate(t, false)
	for _, line := range []string{"glm-5.3-flash claude-ollama", "claude-opus-5-5 turbo claude"} {
		say(t, f.router, line)
		a, logs, _ := newAdapter(t, config.Cartridge{})
		require.NoError(t, fire(t, a, spec(t, cartridge.ToolReadWrite, okEnv(t))), line)
		calls := firings(t, f.claude)
		assert.NotContains(t, calls[len(calls)-1], "--effort", line)
		if strings.Contains(line, "turbo") {
			assert.Contains(t, logs.String(), "does not accept")
		}
	}
}

func TestRun_DisabledProvidersPerPolicy(t *testing.T) {
	unsupported := "agy,kimi,grok,codex,codex-animus,junie,hermes,kilo"
	cases := []struct {
		name       string
		withCursor bool
		policy     cartridge.ToolPolicy
		want       string
	}{
		{"read_only", true, cartridge.ToolReadOnly, "agy,kimi,grok,codex,codex-animus,cursor,junie,hermes,kilo"},
		{"read_exec", true, cartridge.ToolReadExec, "agy,kimi,grok,codex,codex-animus,cursor,junie,hermes,kilo"},
		{"read_write", true, cartridge.ToolReadWrite, unsupported},
		{"read_write without cursor-agent", false, cartridge.ToolReadWrite, "agy,kimi,grok,codex,codex-animus,cursor,junie,hermes,kilo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := isolate(t, tc.withCursor)
			say(t, f.router, "claude-opus-5-5 high claude")
			a, _, _ := newAdapter(t, config.Cartridge{})
			require.NoError(t, fire(t, a, spec(t, tc.policy, okEnv(t))))
			assert.Equal(t, tc.want, recorded(t, f.router, "disable", 1))
		})
	}
}

func TestRun_UserDisabledProvidersAreKept(t *testing.T) {
	f := isolate(t, true)
	t.Setenv(disableEnv, "claude-work, hermes,someday")
	say(t, f.router, "claude-opus-5-5 high claude")
	a, _, _ := newAdapter(t, config.Cartridge{})
	require.NoError(t, fire(t, a, spec(t, cartridge.ToolReadWrite, okEnv(t))))
	assert.Equal(t, "claude-work,agy,kimi,grok,codex,codex-animus,junie,hermes,kilo,someday", recorded(t, f.router, "disable", 1))
}

func TestRun_RouterEnvironment(t *testing.T) {
	f := isolate(t, false)
	t.Setenv("CLAUDECODE", "1")
	say(t, f.router, "claude-opus-5-5 high claude")
	a, _, _ := newAdapter(t, config.Cartridge{Env: map[string]string{"SC_ROUTER_KNOB": "on"}})
	require.NoError(t, fire(t, a, spec(t, cartridge.ToolReadOnly, okEnv(t))))
	assert.Equal(t, "unset", recorded(t, f.router, "marker", 1), "a parent Claude Code session's marker must not reach the router")
	assert.Equal(t, "on", recorded(t, f.router, "knob", 1), "the configured env reaches the router")
}

// TestRun_ReadOnlyNeverReachesCursor is the defence in depth behind the
// native enforcement this adapter reports: even when the router ignores
// DANDELION_DISABLE and names cursor, a read_only or read_exec firing is
// refused as "no route" and cursor-agent is never started.
func TestRun_ReadOnlyNeverReachesCursor(t *testing.T) {
	f := isolate(t, true)
	say(t, f.router, "auto cursor")
	for _, policy := range []cartridge.ToolPolicy{cartridge.ToolReadOnly, cartridge.ToolReadExec} {
		a, _, _ := newAdapter(t, config.Cartridge{})
		err := fire(t, a, spec(t, policy, nil))
		e := cerr(t, err)
		assert.Equal(t, cartridge.KindRateLimited, e.Kind, policy)
		assert.Equal(t, NoRouteRetryAfter, e.RetryAfter)
		assert.Contains(t, e.Error(), `dandelion routed to "cursor" (auto cursor), which cannot hold `+string(policy))
	}
	assert.Empty(t, f.cursor.ArgLists(t), "cursor-agent was started for a read-only firing")
	assert.Empty(t, firings(t, f.claude))
}

func TestRun_CursorNeedsCursorAgent(t *testing.T) {
	f := isolate(t, false)
	say(t, f.router, "auto cursor")
	a, _, _ := newAdapter(t, config.Cartridge{})
	e := cerr(t, fire(t, a, spec(t, cartridge.ToolReadWrite, nil)))
	assert.Equal(t, cartridge.KindRateLimited, e.Kind)
	assert.Contains(t, e.Msg, "which needs cursor-agent on PATH")
}

func TestRun_CursorForWriteFirings(t *testing.T) {
	f := isolate(t, true)
	say(t, f.router, "auto cursor")
	a, logs, _ := newAdapter(t, config.Cartridge{})
	sp := spec(t, cartridge.ToolReadWrite, map[string]string{"SC_CURSOR_FIXTURE": fixture(t, "cursor", "tools-reply.jsonl")})
	require.NoError(t, fire(t, a, sp))
	calls := f.cursor.ArgLists(t)
	require.Len(t, calls, 1)
	i := slices.Index(calls[0], "--model")
	require.GreaterOrEqual(t, i, 0)
	assert.Equal(t, "auto", calls[0][i+1])
	assert.Contains(t, logs.String(), "account=cursor")
}

func TestRun_UnsupportedAccountsAreNoRoute(t *testing.T) {
	f := isolate(t, true)
	for _, line := range []string{"gpt-6-astra xhigh codex", "grok-4.7 xhigh grok", "m brand-new-provider"} {
		say(t, f.router, line)
		a, _, _ := newAdapter(t, config.Cartridge{})
		e := cerr(t, fire(t, a, spec(t, cartridge.ToolReadWrite, nil)))
		assert.Equal(t, cartridge.KindRateLimited, e.Kind, line)
		assert.Contains(t, e.Msg, "which is not one sc can drive", line)
		assert.Equal(t, Name, e.Cartridge)
	}
	assert.Empty(t, firings(t, f.claude))
	assert.Empty(t, f.cursor.ArgLists(t))
}

func TestRun_RouterErrors(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(t *testing.T, router *testutil.Shim)
		kind   cartridge.Kind
		retry  time.Duration
		substr string
	}{
		{"exit 1 none", func(t *testing.T, r *testutil.Shim) {
			fail(t, r, 1, "")
			writeFile(t, filepath.Join(r.Dir, "line"), "none\n")
		}, cartridge.KindRateLimited, NoRouteRetryAfter, "no account with quota left"},
		{"none on exit 0", func(t *testing.T, r *testutil.Shim) { say(t, r, "none") }, cartridge.KindRateLimited, NoRouteRetryAfter, "no account with quota left"},
		{"routes.json fault", func(t *testing.T, r *testutil.Shim) {
			fail(t, r, 2, "dandelion: routes file /x/routes.json: unknown key route.kilo\n")
		}, cartridge.KindNotInstalled, 0, "dandelion cannot route: dandelion: routes file /x/routes.json: unknown key route.kilo"},
		{"unparseable line", func(t *testing.T, r *testutil.Shim) { say(t, r, "garbage") }, cartridge.KindCrash, 0, `route line "garbage"`},
		{"two lines", func(t *testing.T, r *testutil.Shim) { say(t, r, "a claude\nb claude") }, cartridge.KindCrash, 0, "more than one line"},
		{"other exit", func(t *testing.T, r *testutil.Shim) { fail(t, r, 3, "boom") }, cartridge.KindCrash, 0, "dandelion route failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := isolate(t, false)
			tc.setup(t, f.router)
			a, _, _ := newAdapter(t, config.Cartridge{})
			e := cerr(t, fire(t, a, spec(t, cartridge.ToolReadOnly, nil)))
			assert.Equal(t, tc.kind, e.Kind)
			assert.Equal(t, tc.retry, e.RetryAfter)
			assert.Contains(t, e.Error(), tc.substr)
			assert.Equal(t, Name, e.Cartridge)
			assert.Empty(t, firings(t, f.claude))
			// A failure is never cached: the next firing asks again.
			_ = fire(t, a, spec(t, cartridge.ToolReadOnly, nil))
			assert.Len(t, f.router.ArgLists(t), 2)
		})
	}
	t.Run("routes.json fault is not retryable", func(t *testing.T) {
		assert.False(t, cartridge.KindNotInstalled.Retryable())
	})
}

func TestRun_RouterMissing(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	a, _, _ := newAdapter(t, config.Cartridge{Command: "no-such-dandelion"})
	e := cerr(t, fire(t, a, spec(t, cartridge.ToolReadOnly, nil)))
	assert.Equal(t, cartridge.KindNotInstalled, e.Kind)
	assert.Contains(t, e.Error(), "no-such-dandelion not found on PATH")
}

func TestRun_RouterTimeoutOrCancelIsKilled(t *testing.T) {
	f := isolate(t, false)
	say(t, f.router, "claude-opus-5-5 high claude")
	writeFile(t, filepath.Join(f.router.Dir, "sleep"), "1")
	a, _, _ := newAdapter(t, config.Cartridge{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := a.Run(ctx, spec(t, cartridge.ToolReadOnly, okEnv(t)))
	assert.Equal(t, cartridge.KindKilled, cerr(t, err).Kind)
	// The abandoned call still finishes and is cached for the next firing.
	require.Eventually(t, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return len(a.cache) == 1
	}, testutil.Patience(5*time.Second), 20*time.Millisecond)
	require.NoError(t, fire(t, a, spec(t, cartridge.ToolReadOnly, okEnv(t))))
	assert.Len(t, f.router.ArgLists(t), 1)
}

func TestRoute_CacheHitAndExpiry(t *testing.T) {
	f := isolate(t, true)
	say(t, f.router, "claude-opus-5-5 high claude")
	a, logs, clock := newAdapter(t, config.Cartridge{})

	require.NoError(t, fire(t, a, spec(t, cartridge.ToolReadOnly, okEnv(t))))
	clock.Advance(CacheTTL - time.Second)
	require.NoError(t, fire(t, a, spec(t, cartridge.ToolReadExec, okEnv(t))), "same allowed set as read_only")
	assert.Len(t, f.router.ArgLists(t), 1, "the second firing used the cached route")
	assert.Contains(t, logs.String(), "cached=true")

	// read_write may also use cursor: another account set, another route.
	require.NoError(t, fire(t, a, spec(t, cartridge.ToolReadWrite, okEnv(t))))
	assert.Len(t, f.router.ArgLists(t), 2)

	clock.Advance(time.Second)
	require.NoError(t, fire(t, a, spec(t, cartridge.ToolReadOnly, okEnv(t))))
	assert.Len(t, f.router.ArgLists(t), 3, "an expired route is asked for again")
}

func TestRoute_Singleflight(t *testing.T) {
	f := isolate(t, false)
	say(t, f.router, "claude-opus-5-5 high claude")
	writeFile(t, filepath.Join(f.router.Dir, "sleep"), "0.5")
	a, _, _ := newAdapter(t, config.Cartridge{})

	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := a.Run(context.Background(), spec(t, cartridge.ToolReadOnly, okEnv(t)))
			if err == nil {
				_, err = cartridge.Collect(s, nil)
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	assert.Len(t, f.router.ArgLists(t), 1, "concurrent firings share one router call")
	assert.Len(t, firings(t, f.claude), n)
}

func TestRun_InnerFailures(t *testing.T) {
	cases := []struct {
		name        string
		stderr      string
		kind        cartridge.Kind
		retry       time.Duration
		msg         string
		invalidates bool
	}{
		{"credits become a short rate limit", "Credit balance is too low", cartridge.KindRateLimited, AccountRetryAfter,
			"account claude-work (claude-opus-5-5) is out of credits; routing again", true},
		{"rate limit passes through", "API Error: Rate limit reached", cartridge.KindRateLimited, AccountRetryAfter,
			"account claude-work (claude-opus-5-5) is rate limited; routing again", true},
		{"crash keeps the route", "segfault", cartridge.KindCrash, 0, "account claude-work (claude-opus-5-5)", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := isolate(t, false)
			say(t, f.router, "claude-opus-5-5 high claude-work")
			a, _, _ := newAdapter(t, config.Cartridge{})
			s, err := a.Run(context.Background(), spec(t, cartridge.ToolReadOnly, map[string]string{"SC_CLAUDE_STDERR": tc.stderr, "SC_CLAUDE_EXIT": "1"}))
			require.NoError(t, err)
			evs, _, err := collect(t, s)
			e := cerr(t, err)
			assert.Equal(t, tc.kind, e.Kind)
			assert.Equal(t, tc.retry, e.RetryAfter)
			assert.Contains(t, e.Msg, tc.msg)
			assert.Equal(t, Name, e.Cartridge)
			assert.False(t, errors.Is(err, cartridge.ErrCreditsExhausted), "an account's credits must never retire the cartridge")
			last := evs[len(evs)-1]
			require.Equal(t, cartridge.EventError, last.Kind)
			assert.Equal(t, tc.kind, cartridge.KindOf(last.Err), "the terminal event agrees with Wait")

			// The next firing asks the router again only after a quota
			// failure, and then without --max-age, so the router probes
			// afresh instead of answering from a stale snapshot.
			require.NoError(t, fire(t, a, spec(t, cartridge.ToolReadOnly, okEnv(t))))
			calls := f.router.ArgLists(t)
			if tc.invalidates {
				require.Len(t, calls, 2)
				assert.Equal(t, []string{"route"}, calls[1])
				require.NoError(t, fire(t, a, spec(t, cartridge.ToolReadOnly, okEnv(t))))
				assert.Len(t, f.router.ArgLists(t), 2, "the fresh answer is cached again")
			} else {
				assert.Len(t, calls, 1)
			}
		})
	}
}

func TestToolPolicyEnforcement(t *testing.T) {
	t.Parallel()
	c, err := New(Name, config.Cartridge{Model: "high"})
	require.NoError(t, err)
	for _, p := range []cartridge.ToolPolicy{cartridge.ToolReadOnly, cartridge.ToolReadExec} {
		en, err := agents.EnforcementFor(c, p)
		require.NoError(t, err)
		assert.Equal(t, agents.EnforceNative, en.Mode, p)
		assert.Equal(t, Name, en.Cartridge)
		assert.Equal(t, claude.ToolFlags(p), en.Controls)
		assert.Contains(t, en.Detail, "routed to Claude Code accounts only")
		require.NoError(t, en.Check())
	}
	en, err := agents.EnforcementFor(c, cartridge.ToolReadWrite)
	require.NoError(t, err)
	assert.Equal(t, agents.EnforceNone, en.Mode)
	enf, ok := c.(agents.ToolPolicyEnforcer)
	require.True(t, ok)
	_, err = enf.ToolPolicyEnforcement("bogus")
	assert.Error(t, err)
}

func TestDetect(t *testing.T) {
	f := isolate(t, false)
	a, _, _ := newAdapter(t, config.Cartridge{Model: "high"})
	st, err := a.Detect(context.Background())
	require.NoError(t, err)
	assert.True(t, st.Ready())
	assert.Equal(t, f.router.Path, st.Path)
	assert.Equal(t, "2.1.309", st.Version)
	assert.Equal(t, cartridge.AuthOK, st.Auth)
	assert.Contains(t, st.Detail, "dandelion route --high over claude, claude-work, claude-deepseek, claude-ollama (Claude Code)")
	assert.Contains(t, st.Detail, "cursor-agent is not on PATH")
	assert.Contains(t, st.Detail, "claude: logged in as user@example.com")
	assert.Empty(t, f.router.ArgLists(t), "Detect must not run a probe round")
	assert.Equal(t, [][]string{{"--version"}, {"auth", "status", "--json"}}, f.claude.ArgLists(t))

	f.cursor = testutil.FakeBin(t, "cursor-agent", cursorShimScript)
	std, _, _ := newAdapter(t, config.Cartridge{})
	st, err = std.Detect(context.Background())
	require.NoError(t, err)
	assert.Contains(t, st.Detail, "dandelion route over")
	assert.Contains(t, st.Detail, "and cursor (Cursor Agent, read_write firings only)")
}

func TestDetect_Missing(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	a, _, _ := newAdapter(t, config.Cartridge{})
	st, err := a.Detect(context.Background())
	assert.Equal(t, cartridge.KindNotInstalled, cerr(t, err).Kind)
	assert.False(t, st.Installed)
	assert.Equal(t, "dandelion not found on PATH", st.Detail)

	// The router alone is not enough: the product owner and testers need
	// Claude Code.
	testutil.FakeBin(t, DefaultBinary, routerShimScript)
	_, err = a.Detect(context.Background())
	assert.Equal(t, cartridge.KindNotInstalled, cerr(t, err).Kind)
	assert.Contains(t, err.Error(), "cartridge dandelion")
}

func TestDisabled(t *testing.T) {
	t.Parallel()
	all := make([]string, 0, len(Providers))
	for _, p := range Providers {
		all = append(all, p.ID)
	}
	assert.Empty(t, Disabled(all, ""))
	assert.Equal(t, strings.Join(all, ","), Disabled(nil, ""))
	assert.Equal(t, "claude,x", Disabled(all, " claude ,x,,claude"))
}
