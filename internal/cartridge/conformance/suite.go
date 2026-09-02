package conformance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/structured"
)

// Check names, in the order the suite runs them.
const (
	// CheckDetect: Detect succeeds and Status.Ready is true. When it fails
	// every other check is skipped.
	CheckDetect = "detect"
	// CheckEcho: a read_only session replies with the token.
	CheckEcho = "echo"
	// CheckStructured: a read_only session ends with an sc-result block that
	// matches the conformance contract and carries the token.
	CheckStructured = "structured"
	// CheckWrite: a read_write session creates WriteFileName in its workdir
	// with the token as content.
	CheckWrite = "write"
	// CheckRefuse: the same request under read_only leaves no file behind.
	CheckRefuse = "refuse"
	// CheckTimeout: a hung session ends with cartridge.ErrTimeout once
	// RunSpec.Timeout elapses.
	CheckTimeout = "timeout"
	// CheckKill: Session.Kill ends a hung session with cartridge.ErrKilled.
	CheckKill = "kill"
	// CheckBeads is declared in beads.go: the read-only bd shim refuses
	// `bd update` inside a session.
)

// WriteFileName is the file the write and refuse checks ask the model to
// create, relative to the session's workdir.
const WriteFileName = "sc-conformance.txt"

// ContractName is the name of the structured-output contract the suite
// asks for.
const ContractName = "conformance"

// Defaults for Options.
const (
	// DefaultTimeout is the RunSpec.Timeout of every check but the timeout
	// check.
	DefaultTimeout = 3 * time.Minute
	// DefaultHangTimeout is the RunSpec.Timeout of the timeout check: the
	// hung session must end within it.
	DefaultHangTimeout = 15 * time.Second
	// DefaultKillAfter is how long the kill check lets the hung session run
	// before calling Session.Kill.
	DefaultKillAfter = 5 * time.Second
	// endSlack is how long past its timeout or kill a session may take to
	// end (the Claude adapter's SIGTERM grace period is 5 s).
	endSlack = 30 * time.Second
)

// Prompt markers. Every check's prompt starts with a distinct phrase so a
// mock scenario can match on it (MockScenario does).
const (
	echoMarker       = "Reply with exactly the token"
	structuredMarker = "Report the token"
	writeMarker      = "Create a file named"
	hangMarker       = "tail -f /dev/null"
)

// contractSchema is the JSON Schema of the structured check's block.
const contractSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["token", "ok"],
  "properties": {
    "token": {"type": "string", "minLength": 1, "description": "the token you were given, verbatim"},
    "ok": {"type": "boolean", "const": true}
  },
  "additionalProperties": false
}`

var contract = structured.MustContract(ContractName, []byte(contractSchema))

// Contract returns the structured-output contract the structured check
// asks for: an object with the token and ok: true.
func Contract() *structured.Contract { return contract }

// structuredReply is the decoded block of the structured check.
type structuredReply struct {
	Token string `json:"token"`
	OK    bool   `json:"ok"`
}

// EchoPrompt is the echo check's prompt.
func EchoPrompt(token string) string {
	return fmt.Sprintf("%s %s and nothing else: no punctuation, no quotes, no explanation. Do not use any tools.", echoMarker, token)
}

// StructuredPrompt is the structured check's prompt before the contract's
// suffix is appended by structured.Fire.
func StructuredPrompt(token string) string {
	return fmt.Sprintf("%s %s in the structured block described below, with ok set to true. Do not use any tools.", structuredMarker, token)
}

// WritePrompt is the prompt of the write and refuse checks.
func WritePrompt(token string) string {
	return fmt.Sprintf("%s %s in the current working directory containing exactly one line: %s. Use your file-writing tool and do nothing else. If you are not able to create files, say so and stop.", writeMarker, WriteFileName, token)
}

// HangPrompt is the prompt of the timeout and kill checks: it asks the
// model to block in a shell command that never exits. (Claude Code 2.1.258
// refuses a standalone `sleep N` in its Bash tool and steers the model to
// run it in the background, which is why the command is not sleep.)
func HangPrompt() string {
	return fmt.Sprintf("Run the shell command `%s` in the foreground and wait for it to exit before replying. Do not run it in the background, do not add a timeout, and do nothing else.", hangMarker)
}

// Options tune a suite run. The zero value is usable.
type Options struct {
	// Token is the string the model must echo; random when empty. Passing
	// one lets a scripted cartridge (MockScenario) know what to say.
	Token string
	// Model is RunSpec.Model for every session; empty means the cartridge's
	// configured default.
	Model string
	// Timeout is RunSpec.Timeout for every check but the timeout check;
	// DefaultTimeout when zero.
	Timeout time.Duration
	// HangTimeout is the timeout check's RunSpec.Timeout; DefaultHangTimeout
	// when zero.
	HangTimeout time.Duration
	// KillAfter is how long the kill check waits before Session.Kill;
	// DefaultKillAfter when zero.
	KillAfter time.Duration
	// Workdir is the directory the per-check workdirs are created under. A
	// temporary directory, removed afterwards, when empty.
	Workdir string
	// Checks selects checks by name; nil runs all of them in suite order.
	Checks []string
	// Env is RunSpec.Env for every session (a git identity, a proxy).
	Env map[string]string
	// Observer, when set, sees every session event as it streams, tagged
	// with the check name.
	Observer func(check string, ev cartridge.SessionEvent)
	// BD is the real bd binary the beads-readonly check chains its shim
	// to; empty means the first bd on PATH, and the check is skipped when
	// there is none. Tests point it at a fake.
	BD string
}

// Status is the outcome of one check.
type Status string

// Check outcomes.
const (
	// StatusPass: the check held.
	StatusPass Status = "pass"
	// StatusFail: the check did not hold; Result.Err says why.
	StatusFail Status = "fail"
	// StatusSkip: the check did not run (detect failed, it was not
	// selected, or a tool it needs is not installed).
	StatusSkip Status = "skip"
)

// String returns the canonical name.
func (s Status) String() string { return string(s) }

// Result is the outcome of one check.
type Result struct {
	// Check is the check name.
	Check string
	// Description says what the check asserts.
	Description string
	// Status is the outcome.
	Status Status
	// Detail is a one-line note: what was observed on success, the reason
	// on skip.
	Detail string
	// Err is why the check failed; nil otherwise.
	Err error
	// Duration is how long the check took.
	Duration time.Duration
	// Usage is what the check's sessions consumed, as reported.
	Usage cartridge.Usage
}

// Report is the outcome of a suite run.
type Report struct {
	// Cartridge is the cartridge's name.
	Cartridge string
	// Status is what Detect reported.
	Status cartridge.Status
	// Token is the token the sessions were asked to echo.
	Token string
	// Results holds one entry per check in suite order.
	Results []Result
	// Started is when the run began; Duration how long it took.
	Started  time.Time
	Duration time.Duration
}

// Passed reports whether no check failed.
func (r Report) Passed() bool {
	for _, res := range r.Results {
		if res.Status == StatusFail {
			return false
		}
	}
	return true
}

// Counts returns how many checks passed, failed and were skipped.
func (r Report) Counts() (pass, fail, skip int) {
	for _, res := range r.Results {
		switch res.Status {
		case StatusPass:
			pass++
		case StatusFail:
			fail++
		case StatusSkip:
			skip++
		}
	}
	return pass, fail, skip
}

// Failed returns the failed results in suite order.
func (r Report) Failed() []Result {
	var out []Result
	for _, res := range r.Results {
		if res.Status == StatusFail {
			out = append(out, res)
		}
	}
	return out
}

// Usage sums what every check consumed.
func (r Report) Usage() cartridge.Usage {
	var u cartridge.Usage
	for _, res := range r.Results {
		u = u.Add(res.Usage)
	}
	return u
}

// Err returns nil when the report passed and an error naming every failed
// check otherwise.
func (r Report) Err() error {
	failed := r.Failed()
	if len(failed) == 0 {
		return nil
	}
	parts := make([]string, len(failed))
	for i, f := range failed {
		parts[i] = fmt.Sprintf("%s: %v", f.Check, f.Err)
	}
	return fmt.Errorf("conformance: cartridge %q failed %d of %d checks: %s", r.Cartridge, len(failed), len(r.Results), strings.Join(parts, "; "))
}

// Check is one conformance check.
type Check struct {
	// Name identifies the check (the Check* constants).
	Name string
	// Description says what it asserts.
	Description string

	run func(ctx context.Context, e *env) (string, cartridge.Usage, error)
}

var checks = []Check{
	{CheckDetect, "Detect reports an installed, authenticated CLI", runDetect},
	{CheckEcho, "a read_only session echoes the token", runEcho},
	{CheckStructured, "a read_only session ends with a valid sc-result block", runStructured},
	{CheckWrite, "a read_write session creates " + WriteFileName + " in the workdir", runWrite},
	{CheckRefuse, "the same request under read_only creates no file", runRefuse},
	{CheckTimeout, "a hung session ends with ErrTimeout when RunSpec.Timeout elapses", runTimeout},
	{CheckKill, "Session.Kill ends a hung session with ErrKilled", runKill},
	{CheckBeads, "a read_exec session with the read-only bd shim on PATH has `bd update` refused", runBeads},
}

// Checks returns every check in suite order.
func Checks() []Check { return append([]Check(nil), checks...) }

// CheckNames returns every check name in suite order.
func CheckNames() []string {
	out := make([]string, len(checks))
	for i, c := range checks {
		out[i] = c.Name
	}
	return out
}

// NewToken returns a random token of the form sc-<12 hex digits>.
func NewToken() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on supported platforms; fall back to
		// the clock so a token is still unique enough for one run.
		return fmt.Sprintf("sc-%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	return "sc-" + hex.EncodeToString(b[:])
}

// env is what checks share during one run.
type env struct {
	c     cartridge.Cartridge
	opts  Options
	base  string
	token string
	// status is what the detect check saw.
	status cartridge.Status
}

// Run executes the selected checks against c in suite order and returns
// the report. The error is non-nil only when the run could not start: a
// nil cartridge, an unknown check name, or a workdir that cannot be
// created; check failures are reported in the Report, not the error.
// Cancelling ctx ends the running session and fails the remaining checks.
func Run(ctx context.Context, c cartridge.Cartridge, opts Options) (Report, error) {
	if c == nil {
		return Report{}, errors.New("conformance: nil cartridge")
	}
	selected, err := selectChecks(opts.Checks)
	if err != nil {
		return Report{}, err
	}
	if opts.Timeout == 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.HangTimeout == 0 {
		opts.HangTimeout = DefaultHangTimeout
	}
	if opts.KillAfter == 0 {
		opts.KillAfter = DefaultKillAfter
	}
	if opts.Timeout < 0 || opts.HangTimeout < 0 || opts.KillAfter < 0 {
		return Report{}, errors.New("conformance: timeouts must not be negative")
	}
	token := opts.Token
	if token == "" {
		token = NewToken()
	}
	base, cleanup, err := workdir(opts.Workdir)
	if err != nil {
		return Report{}, err
	}
	defer cleanup()

	e := &env{c: c, opts: opts, base: base, token: token}
	rep := Report{Cartridge: c.Name(), Token: token, Started: time.Now()}
	gate := true
	for _, ck := range checks {
		res := Result{Check: ck.Name, Description: ck.Description}
		switch {
		case !selected[ck.Name]:
			res.Status, res.Detail = StatusSkip, "not selected"
		case !gate:
			res.Status, res.Detail = StatusSkip, "detect failed"
		default:
			start := time.Now()
			detail, usage, err := ck.run(ctx, e)
			res.Duration, res.Usage = time.Since(start), usage
			switch reason, skipped := isSkip(err); {
			case skipped:
				res.Status, res.Detail = StatusSkip, reason
			case err != nil:
				res.Status, res.Err, res.Detail = StatusFail, err, detail
			default:
				res.Status, res.Detail = StatusPass, detail
			}
			if ck.Name == CheckDetect {
				rep.Status = e.status
				gate = err == nil
			}
		}
		rep.Results = append(rep.Results, res)
	}
	rep.Duration = time.Since(rep.Started)
	return rep, nil
}

func selectChecks(names []string) (map[string]bool, error) {
	sel := map[string]bool{}
	if names == nil {
		for _, c := range checks {
			sel[c.Name] = true
		}
		return sel, nil
	}
	known := map[string]bool{}
	for _, c := range checks {
		known[c.Name] = true
	}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if !known[n] {
			return nil, fmt.Errorf("conformance: unknown check %q (want one of %s)", n, strings.Join(CheckNames(), ", "))
		}
		sel[n] = true
	}
	return sel, nil
}

// workdir returns the absolute base directory and a cleanup that removes
// it when the suite created it.
func workdir(dir string) (string, func(), error) {
	if dir == "" {
		tmp, err := os.MkdirTemp("", "sc-conformance-*")
		if err != nil {
			return "", nil, fmt.Errorf("conformance: create workdir: %w", err)
		}
		return tmp, func() { _ = os.RemoveAll(tmp) }, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", nil, fmt.Errorf("conformance: workdir: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", nil, fmt.Errorf("conformance: create workdir: %w", err)
	}
	return abs, func() {}, nil
}

// spec builds the RunSpec of one check with a fresh workdir under base.
func (e *env) spec(check string, policy cartridge.ToolPolicy, prompt string) (cartridge.RunSpec, error) {
	dir := filepath.Join(e.base, check)
	if err := os.RemoveAll(dir); err != nil {
		return cartridge.RunSpec{}, fmt.Errorf("reset workdir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return cartridge.RunSpec{}, fmt.Errorf("create workdir: %w", err)
	}
	var envMap map[string]string
	if len(e.opts.Env) > 0 {
		envMap = make(map[string]string, len(e.opts.Env))
		for k, v := range e.opts.Env {
			envMap[k] = v
		}
	}
	return cartridge.RunSpec{
		SystemPrompt: "You are a test fixture for a cartridge conformance suite. Follow the instruction literally and briefly.",
		Prompt:       prompt,
		Workdir:      dir,
		Model:        e.opts.Model,
		ToolPolicy:   policy,
		Timeout:      e.opts.Timeout,
		Env:          envMap,
		Labels:       cartridge.Labels{cartridge.LabelTask: "conformance", "check": check},
	}, nil
}

func (e *env) observe(check string) func(cartridge.SessionEvent) {
	if e.opts.Observer == nil {
		return nil
	}
	return func(ev cartridge.SessionEvent) { e.opts.Observer(check, ev) }
}

// outcome is one collected session.
type outcome struct {
	events []cartridge.SessionEvent
	res    cartridge.Result
	err    error
	// elapsed is wall time from Run returning to Wait returning.
	elapsed time.Duration
}

// refusals counts error tool results, which is how adapters report a
// denied tool.
func (o outcome) refusals() int {
	n := 0
	for _, ev := range o.events {
		if ev.Kind == cartridge.EventToolResult && ev.IsError {
			n++
		}
	}
	return n
}

// run fires spec and drains it, applying the stream contract. onStart is
// called with the session once it is up (the kill check uses it).
func (e *env) run(ctx context.Context, check string, spec cartridge.RunSpec, onStart func(cartridge.Session)) (outcome, error) {
	s, err := e.c.Run(ctx, spec)
	if err != nil {
		return outcome{}, fmt.Errorf("start session: %w", err)
	}
	if onStart != nil {
		onStart(s)
	}
	start := time.Now()
	var o outcome
	obs := e.observe(check)
	o.res, o.err = cartridge.Collect(s, func(ev cartridge.SessionEvent) {
		o.events = append(o.events, ev)
		if obs != nil {
			obs(ev)
		}
	})
	o.elapsed = time.Since(start)
	if err := streamContract(o); err != nil {
		return o, err
	}
	return o, nil
}

// streamContract checks the Session contract on a drained session: at
// least one event, exactly one terminal event, in last position, agreeing
// with Wait, and Wait's error a *cartridge.Error.
func streamContract(o outcome) error {
	if len(o.events) == 0 {
		return errors.New("stream contract: the session emitted no events")
	}
	terminals := 0
	for _, ev := range o.events {
		if ev.Kind.Terminal() {
			terminals++
		}
	}
	if terminals != 1 {
		return fmt.Errorf("stream contract: %d terminal events, want exactly 1", terminals)
	}
	last := o.events[len(o.events)-1]
	if !last.Kind.Terminal() {
		return fmt.Errorf("stream contract: the last event is %s, want done or error", last.Kind)
	}
	switch {
	case last.Kind == cartridge.EventDone && o.err != nil:
		return fmt.Errorf("stream contract: the stream ended with done but Wait returned %v", o.err)
	case last.Kind == cartridge.EventError && o.err == nil:
		return errors.New("stream contract: the stream ended with error but Wait returned nil")
	case last.Kind == cartridge.EventError && last.Err == nil:
		return errors.New("stream contract: the error event carries no error")
	}
	if o.err != nil {
		var ce *cartridge.Error
		if !errors.As(o.err, &ce) {
			return fmt.Errorf("stream contract: Wait returned %T, want *cartridge.Error: %v", o.err, o.err)
		}
	}
	return nil
}

func containsToken(text, token string) bool {
	return strings.Contains(strings.ToLower(text), strings.ToLower(token))
}

func describeUsage(u cartridge.Usage) string {
	s := fmt.Sprintf("%d in / %d out tokens", u.InputTokens, u.OutputTokens)
	if u.HasCost {
		s += fmt.Sprintf(", $%.4f", u.CostUSD)
	}
	return s
}

// runDetect is CheckDetect.
func runDetect(ctx context.Context, e *env) (string, cartridge.Usage, error) {
	st, err := e.c.Detect(ctx)
	e.status = st
	detail := fmt.Sprintf("installed=%t version=%q auth=%s", st.Installed, st.Version, st.Auth)
	if st.Detail != "" {
		detail += " (" + st.Detail + ")"
	}
	if err != nil {
		return detail, cartridge.Usage{}, fmt.Errorf("detect probe: %w", err)
	}
	if !st.Ready() {
		return detail, cartridge.Usage{}, errors.New("detect returned no error but Status.Ready is false")
	}
	return detail, cartridge.Usage{}, nil
}

// runEcho is CheckEcho.
func runEcho(ctx context.Context, e *env) (string, cartridge.Usage, error) {
	spec, err := e.spec(CheckEcho, cartridge.ToolReadOnly, EchoPrompt(e.token))
	if err != nil {
		return "", cartridge.Usage{}, err
	}
	spec.MaxTurns = 1
	o, err := e.run(ctx, CheckEcho, spec, nil)
	if err != nil {
		return "", o.res.Usage, err
	}
	detail := fmt.Sprintf("%d events, %s", len(o.events), describeUsage(o.res.Usage))
	if o.err != nil {
		return detail, o.res.Usage, fmt.Errorf("session failed: %w", o.err)
	}
	if o.res.ExitCode != 0 {
		return detail, o.res.Usage, fmt.Errorf("exit code %d, want 0", o.res.ExitCode)
	}
	if !containsToken(o.res.Output, e.token) {
		return detail, o.res.Usage, fmt.Errorf("output %q does not contain the token %q", o.res.Output, e.token)
	}
	if !containsToken(cartridge.Transcript(o.events), e.token) {
		return detail, o.res.Usage, errors.New("the streamed text deltas do not contain the token although Result.Output does")
	}
	return "output contains the token; " + detail, o.res.Usage, nil
}

// runStructured is CheckStructured.
func runStructured(ctx context.Context, e *env) (string, cartridge.Usage, error) {
	spec, err := e.spec(CheckStructured, cartridge.ToolReadOnly, StructuredPrompt(e.token))
	if err != nil {
		return "", cartridge.Usage{}, err
	}
	var opts []structured.Option
	if obs := e.observe(CheckStructured); obs != nil {
		opts = append(opts, structured.WithObserver(obs))
	}
	out, err := structured.Fire(ctx, e.c, spec, contract, opts...)
	detail := fmt.Sprintf("%d attempt(s), %s", len(out.Attempts), describeUsage(out.Usage))
	if err != nil {
		return detail, out.Usage, fmt.Errorf("structured.Fire: %w", err)
	}
	var reply structuredReply
	if err := out.Parsed.Decode(&reply); err != nil {
		return detail, out.Usage, fmt.Errorf("decode block: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(reply.Token), e.token) {
		return detail, out.Usage, fmt.Errorf("block token %q, want %q", reply.Token, e.token)
	}
	if !reply.OK {
		return detail, out.Usage, errors.New("block has ok=false")
	}
	return fmt.Sprintf("valid block from %s; %s", out.Parsed.Source, detail), out.Usage, nil
}

// runWrite is CheckWrite.
func runWrite(ctx context.Context, e *env) (string, cartridge.Usage, error) {
	spec, err := e.spec(CheckWrite, cartridge.ToolReadWrite, WritePrompt(e.token))
	if err != nil {
		return "", cartridge.Usage{}, err
	}
	o, err := e.run(ctx, CheckWrite, spec, nil)
	if err != nil {
		return "", o.res.Usage, err
	}
	detail := fmt.Sprintf("%d events, %s", len(o.events), describeUsage(o.res.Usage))
	if o.err != nil {
		return detail, o.res.Usage, fmt.Errorf("session failed: %w", o.err)
	}
	data, rerr := os.ReadFile(filepath.Join(spec.Workdir, WriteFileName))
	if rerr != nil {
		return detail, o.res.Usage, fmt.Errorf("%s was not created under read_write: %w (output: %q)", WriteFileName, rerr, o.res.Output)
	}
	if !containsToken(string(data), e.token) {
		return detail, o.res.Usage, fmt.Errorf("%s contains %q, want the token %q", WriteFileName, string(data), e.token)
	}
	return fmt.Sprintf("%s created with the token; %s", WriteFileName, detail), o.res.Usage, nil
}

// runRefuse is CheckRefuse.
func runRefuse(ctx context.Context, e *env) (string, cartridge.Usage, error) {
	spec, err := e.spec(CheckRefuse, cartridge.ToolReadOnly, WritePrompt(e.token))
	if err != nil {
		return "", cartridge.Usage{}, err
	}
	o, err := e.run(ctx, CheckRefuse, spec, nil)
	if err != nil {
		return "", o.res.Usage, err
	}
	detail := fmt.Sprintf("%d events, %d refused tool call(s), %s", len(o.events), o.refusals(), describeUsage(o.res.Usage))
	if o.err != nil {
		return detail, o.res.Usage, fmt.Errorf("session failed: %w", o.err)
	}
	if entries, derr := os.ReadDir(spec.Workdir); derr == nil && len(entries) > 0 {
		names := make([]string, len(entries))
		for i, en := range entries {
			names[i] = en.Name()
		}
		sort.Strings(names)
		return detail, o.res.Usage, fmt.Errorf("read_only session created %s in the workdir (output: %q)", strings.Join(names, ", "), o.res.Output)
	}
	return "no file created; " + detail, o.res.Usage, nil
}

// runTimeout is CheckTimeout.
func runTimeout(ctx context.Context, e *env) (string, cartridge.Usage, error) {
	spec, err := e.spec(CheckTimeout, cartridge.ToolReadExec, HangPrompt())
	if err != nil {
		return "", cartridge.Usage{}, err
	}
	spec.Timeout = e.opts.HangTimeout
	o, err := e.run(ctx, CheckTimeout, spec, nil)
	if err != nil {
		return "", o.res.Usage, err
	}
	detail := fmt.Sprintf("ended after %s with timeout %s, %d events", o.elapsed.Round(time.Millisecond), spec.Timeout, len(o.events))
	switch {
	case o.err == nil:
		return detail, o.res.Usage, fmt.Errorf("the hung session finished successfully instead of timing out (output: %q)", o.res.Output)
	case !errors.Is(o.err, cartridge.ErrTimeout):
		return detail, o.res.Usage, fmt.Errorf("the hung session ended with %s, want ErrTimeout: %w", cartridge.KindOf(o.err), o.err)
	case o.elapsed > spec.Timeout+endSlack:
		return detail, o.res.Usage, fmt.Errorf("the session took %s to end after a %s timeout", o.elapsed.Round(time.Millisecond), spec.Timeout)
	}
	return detail, o.res.Usage, nil
}

// runKill is CheckKill.
func runKill(ctx context.Context, e *env) (string, cartridge.Usage, error) {
	spec, err := e.spec(CheckKill, cartridge.ToolReadExec, HangPrompt())
	if err != nil {
		return "", cartridge.Usage{}, err
	}
	killed := make(chan struct{})
	onStart := func(s cartridge.Session) {
		go func() {
			defer close(killed)
			t := time.NewTimer(e.opts.KillAfter)
			defer t.Stop()
			select {
			case <-t.C:
				s.Kill()
			case <-ctx.Done():
			}
		}()
	}
	o, err := e.run(ctx, CheckKill, spec, onStart)
	<-killed
	if err != nil {
		return "", o.res.Usage, err
	}
	detail := fmt.Sprintf("ended %s after Run with Kill at %s, %d events", o.elapsed.Round(time.Millisecond), e.opts.KillAfter, len(o.events))
	switch {
	case o.err == nil:
		return detail, o.res.Usage, fmt.Errorf("the hung session finished successfully instead of being killed (output: %q)", o.res.Output)
	case !errors.Is(o.err, cartridge.ErrKilled):
		return detail, o.res.Usage, fmt.Errorf("the killed session ended with %s, want ErrKilled: %w", cartridge.KindOf(o.err), o.err)
	case o.elapsed > e.opts.KillAfter+endSlack:
		return detail, o.res.Usage, fmt.Errorf("the session took %s to end after Kill at %s", o.elapsed.Round(time.Millisecond), e.opts.KillAfter)
	}
	return detail, o.res.Usage, nil
}
