package mock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/proc"
)

// Name is the registry name the adapter registers under.
const Name = "mock"

// ScenarioEnv is the key in a configured cartridge's env table
// ([cartridges.<name>] env) that names the scenario file to play.
const ScenarioEnv = "SC_MOCK_SCENARIO"

// ErrNoMatch is the cause of the error Run returns when no rule matches the
// spec and the scenario has no fallback.
var ErrNoMatch = errors.New("mock: no rule matches")

// maxToolOutput bounds the text of a tool result built from command output.
const maxToolOutput = 4000

func init() {
	cartridge.MustRegister(Name, New)
}

// New is the registry factory. The scenario comes from the file named by
// cfg.Env[ScenarioEnv]; without one the adapter plays DefaultScenario. It
// reads the file but touches nothing else.
func New(name string, cfg config.Cartridge) (cartridge.Cartridge, error) {
	s := DefaultScenario()
	if path := strings.TrimSpace(cfg.Env[ScenarioEnv]); path != "" {
		loaded, err := Load(path)
		if err != nil {
			return nil, err
		}
		s = loaded
	}
	return NewAdapter(name, s)
}

// Option customises an Adapter.
type Option func(*Adapter)

// WithClock makes the adapter measure delays, timeouts and event times on c
// instead of the system clock.
func WithClock(c Clock) Option {
	return func(a *Adapter) {
		if c != nil {
			a.clock = c
		}
	}
}

// Call is one recorded Run invocation.
type Call struct {
	// Seq numbers calls from 1 in the order Run was invoked.
	Seq int
	// At is the clock time of the call.
	At time.Time
	// Spec is the RunSpec exactly as received.
	Spec cartridge.RunSpec
	// Rule is the name of the rule that answered, or "" when none matched.
	Rule string
	// Matched is false when Run failed with ErrNoMatch.
	Matched bool
}

// Adapter is the mock cartridge. It is safe for concurrent use.
type Adapter struct {
	name     string
	scenario *Scenario
	clock    Clock

	mu      sync.Mutex
	calls   []Call
	uses    map[*Rule]int
	detects int
	seq     int
}

// Compile-time check that Adapter implements the interface.
var _ cartridge.Cartridge = (*Adapter)(nil)

// NewAdapter builds an adapter that plays s under name. The scenario is
// validated and shared, not copied: a test may keep mutating it through the
// builder before the first Run.
func NewAdapter(name string, s *Scenario, opts ...Option) (*Adapter, error) {
	if strings.TrimSpace(name) == "" {
		name = Name
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	a := &Adapter{name: name, scenario: s, clock: SystemClock(), uses: map[*Rule]int{}}
	for _, o := range opts {
		o(a)
	}
	return a, nil
}

// Name implements cartridge.Cartridge.
func (a *Adapter) Name() string { return a.name }

// Scenario returns the scenario the adapter plays.
func (a *Adapter) Scenario() *Scenario { return a.scenario }

// Clock returns the adapter's clock.
func (a *Adapter) Clock() Clock { return a.clock }

// Calls returns every Run invocation so far, in order.
func (a *Adapter) Calls() []Call {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Call(nil), a.calls...)
}

// CallsMatching returns the recorded calls whose spec satisfies m.
func (a *Adapter) CallsMatching(m Match) []Call {
	var out []Call
	for _, c := range a.Calls() {
		if m.Matches(c.Spec) {
			out = append(out, c)
		}
	}
	return out
}

// LastCall returns the most recent call and whether there was one.
func (a *Adapter) LastCall() (Call, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.calls) == 0 {
		return Call{}, false
	}
	return a.calls[len(a.calls)-1], true
}

// Uses returns how many sessions the named rule has answered.
func (a *Adapter) Uses(rule string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	for r, n := range a.uses {
		if r.Name == rule {
			return n
		}
	}
	return 0
}

// Detects returns how many times Detect was called.
func (a *Adapter) Detects() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.detects
}

// Reset forgets recorded calls, detect counts and rule use budgets.
func (a *Adapter) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = nil
	a.uses = map[*Rule]int{}
	a.detects = 0
	a.seq = 0
}

// Detect implements cartridge.Cartridge from the scenario's DetectSpec.
func (a *Adapter) Detect(ctx context.Context) (cartridge.Status, error) {
	a.mu.Lock()
	a.detects++
	a.mu.Unlock()
	d := a.scenario.Detect
	if d.Delay > 0 {
		select {
		case <-a.clock.After(d.Delay.Duration()):
		case <-ctx.Done():
			return cartridge.Status{}, cartridge.NewError(cartridge.KindTimeout, a.name, "detect cancelled", ctx.Err())
		}
	}
	st := cartridge.Status{Installed: true, Path: a.name, Version: d.Version, Auth: cartridge.AuthOK, Detail: "scenario " + a.scenario.Name}
	if st.Version == "" {
		st.Version = "mock/1"
	}
	if d.Installed != nil && !*d.Installed {
		return cartridge.Status{Installed: false, Detail: st.Detail}, cartridge.NewError(cartridge.KindNotInstalled, a.name, "scripted as not installed", nil)
	}
	if d.Auth != cartridge.AuthUnknown {
		st.Auth = d.Auth
	}
	if d.Failure != nil {
		if d.Failure.Kind == FailNotInstalled {
			st.Installed, st.Path, st.Version = false, "", ""
		}
		if d.Failure.Kind == FailAuth {
			st.Auth = cartridge.AuthMissing
		}
		if err := d.Failure.Err(a.name); err != nil {
			return st, err
		}
	}
	if st.Auth == cartridge.AuthMissing {
		return st, cartridge.NewError(cartridge.KindAuth, a.name, "scripted as not logged in", nil)
	}
	return st, nil
}

// Run implements cartridge.Cartridge: it validates spec, records the call,
// picks the first matching rule with budget left (or the fallback) and
// plays it in a goroutine.
func (a *Adapter) Run(ctx context.Context, spec cartridge.RunSpec) (cartridge.Session, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	rule, seq := a.pick(spec)
	if rule == nil {
		return nil, cartridge.NewError(cartridge.KindCrash, a.name,
			fmt.Sprintf("no rule matches labels {%s} and the scenario has no fallback", describeLabels(spec.Labels)), ErrNoMatch)
	}
	killed := make(chan struct{})
	var killOnce sync.Once
	p := cartridge.NewPipe(a.name, func() { killOnce.Do(func() { close(killed) }) })
	go a.play(ctx, spec, rule, seq, p, killed)
	return p, nil
}

func (a *Adapter) pick(spec cartridge.RunSpec) (*Rule, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	call := Call{Seq: a.seq, At: a.clock.Now(), Spec: cloneSpec(spec)}
	var chosen *Rule
	for _, r := range a.scenario.Rules {
		if r.MaxUses > 0 && a.uses[r] >= r.MaxUses {
			continue
		}
		if r.Match.Matches(spec) {
			chosen = r
			break
		}
	}
	if chosen == nil && a.scenario.Fallback != nil {
		chosen = a.scenario.Fallback
	}
	if chosen != nil {
		a.uses[chosen]++
		call.Rule, call.Matched = chosen.Name, true
	}
	a.calls = append(a.calls, call)
	return chosen, a.seq
}

func cloneSpec(spec cartridge.RunSpec) cartridge.RunSpec {
	cp := spec
	cp.Labels = spec.Labels.Clone()
	if spec.Env != nil {
		cp.Env = make(map[string]string, len(spec.Env))
		for k, v := range spec.Env {
			cp.Env[k] = v
		}
	}
	if spec.InheritEnv != nil {
		cp.InheritEnv = append([]string(nil), spec.InheritEnv...)
	}
	return cp
}

func describeLabels(l cartridge.Labels) string {
	parts := make([]string, 0, len(l))
	for _, k := range l.Keys() {
		parts = append(parts, k+"="+l[k])
	}
	return strings.Join(parts, " ")
}

// session is the state of one playing rule.
type session struct {
	a       *Adapter
	ctx     context.Context
	spec    cartridge.RunSpec
	rule    *Rule
	seq     int
	pipe    *cartridge.Pipe
	killed  <-chan struct{}
	timeout <-chan time.Time
	// timedOut records that the timeout channel fired (it fires once), so a
	// later injected FailTimeout does not wait on it again.
	timedOut bool
	start    time.Time
	text     strings.Builder
	tools    int
	toolSeq  int
}

// stop is why a session ended early.
type stop int

const (
	stopNone stop = iota
	stopTimeout
	stopKilled
	stopCancelled
)

func (a *Adapter) play(ctx context.Context, spec cartridge.RunSpec, rule *Rule, seq int, p *cartridge.Pipe, killed <-chan struct{}) {
	s := &session{a: a, ctx: ctx, spec: spec, rule: rule, seq: seq, pipe: p, killed: killed, start: a.clock.Now()}
	if spec.Timeout > 0 {
		s.timeout = a.clock.After(spec.Timeout)
	}
	for _, st := range rule.Steps {
		if why := s.wait(st.Delay.Duration()); why != stopNone {
			if why == stopTimeout && st.Kind == StepFail && st.Failure.Kind == FailTimeout {
				// The scripted timeout is due anyway; report it with the
				// scripted message rather than the generic one so the
				// outcome does not depend on when the clock moved.
				s.fail(*st.Failure)
				return
			}
			s.end(why)
			return
		}
		if done := s.step(st); done {
			return
		}
	}
	s.succeed(s.output())
}

// wait pauses for d on the clock, returning early when the timeout, a kill
// or the context fires. A zero d still polls those without blocking.
func (s *session) wait(d time.Duration) stop {
	var after <-chan time.Time
	if d > 0 {
		after = s.a.clock.After(d)
	} else {
		select {
		case <-s.timeout:
			s.timedOut = true
			return stopTimeout
		case <-s.killed:
			return stopKilled
		case <-s.ctx.Done():
			return stopCancelled
		default:
			return stopNone
		}
	}
	select {
	case <-after:
		return stopNone
	case <-s.timeout:
		s.timedOut = true
		return stopTimeout
	case <-s.killed:
		return stopKilled
	case <-s.ctx.Done():
		return stopCancelled
	}
}

func (s *session) now() time.Time { return s.a.clock.Now() }

func (s *session) emit(ev cartridge.SessionEvent) {
	ev.At = s.now()
	s.pipe.Emit(ev)
}

func (s *session) say(text string) {
	s.text.WriteString(text)
	s.emit(cartridge.TextDelta(text))
}

func (s *session) toolID(explicit string) string {
	if explicit != "" {
		return explicit
	}
	s.toolSeq++
	return "mock-tool-" + strconv.Itoa(s.toolSeq)
}

// step performs one step; it returns true when the session has ended.
func (s *session) step(st Step) bool {
	switch st.Kind {
	case StepText:
		s.say(st.Text)
	case StepEcho:
		s.say(s.spec.Prompt)
	case StepToolUse:
		s.tools++
		s.emit(cartridge.ToolUse(st.Tool, s.toolID(st.ToolID), st.Input))
	case StepToolResult:
		s.emit(cartridge.ToolResult(st.Tool, st.ToolID, st.Output, st.IsError))
	case StepWrite:
		s.write(st)
	case StepDelete:
		s.remove(st)
	case StepExec:
		return s.exec(st)
	case StepSleep:
		// The delay was already waited.
	case StepFail:
		s.fail(*st.Failure)
		return true
	}
	return false
}

func (s *session) tool(tool, input string) string {
	s.tools++
	id := s.toolID("")
	s.emit(cartridge.ToolUse(tool, id, input))
	return id
}

func (s *session) refuse(tool, id, what string) {
	s.emit(cartridge.ToolResult(tool, id, fmt.Sprintf("refused: tool policy %s forbids %s", s.spec.ToolPolicy, what), true))
}

func (s *session) resolve(rel string) (string, error) {
	if err := checkRelPath(rel); err != nil {
		return "", err
	}
	full := filepath.Join(s.spec.Workdir, rel)
	back, err := filepath.Rel(s.spec.Workdir, full)
	if err != nil || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("mock: path %q escapes the workdir", rel)
	}
	return full, nil
}

func (s *session) write(st Step) {
	tool := "Write"
	if st.Append {
		tool = "Edit"
	}
	id := s.tool(tool, st.Path)
	if !s.spec.ToolPolicy.AllowsWrite() {
		s.refuse(tool, id, "writing files")
		return
	}
	full, err := s.resolve(st.Path)
	if err != nil {
		s.emit(cartridge.ToolResult(tool, id, err.Error(), true))
		return
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		s.emit(cartridge.ToolResult(tool, id, err.Error(), true))
		return
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if st.Append {
		flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}
	f, err := os.OpenFile(full, flags, 0o644)
	if err == nil {
		_, err = f.WriteString(st.Content)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		s.emit(cartridge.ToolResult(tool, id, err.Error(), true))
		return
	}
	verb := "wrote"
	if st.Append {
		verb = "appended"
	}
	s.emit(cartridge.ToolResult(tool, id, fmt.Sprintf("%s %d bytes to %s", verb, len(st.Content), st.Path), false))
}

func (s *session) remove(st Step) {
	id := s.tool("Bash", "rm "+st.Path)
	if !s.spec.ToolPolicy.AllowsWrite() {
		s.refuse("Bash", id, "deleting files")
		return
	}
	full, err := s.resolve(st.Path)
	if err == nil {
		err = os.Remove(full)
	}
	if err != nil {
		s.emit(cartridge.ToolResult("Bash", id, err.Error(), true))
		return
	}
	s.emit(cartridge.ToolResult("Bash", id, "removed "+st.Path, false))
}

// exec runs the step's command in the workdir. It returns true when the
// session ended (a Must command failed).
func (s *session) exec(st Step) bool {
	line := strings.Join(append([]string{st.Command}, st.Args...), " ")
	id := s.tool("Bash", line)
	if !s.spec.ToolPolicy.AllowsExec() {
		s.refuse("Bash", id, "running commands")
		if st.Must {
			s.fail(Failure{Kind: FailCrash, Message: "required command refused by tool policy: " + line})
			return true
		}
		return false
	}
	ps := s.spec.ProcSpec(st.Command, st.Args...)
	res, err := proc.Run(s.ctx, ps)
	out := strings.TrimSpace(res.Stdout)
	if res.Stderr != "" {
		if out != "" {
			out += "\n"
		}
		out += strings.TrimSpace(res.Stderr)
	}
	if err != nil {
		if out != "" {
			out += "\n"
		}
		out += err.Error()
	}
	if len(out) > maxToolOutput {
		out = out[:maxToolOutput] + "…"
	}
	s.emit(cartridge.ToolResult("Bash", id, out, err != nil))
	if err != nil && st.Must {
		s.fail(Failure{Kind: FailCrash, Message: "required command failed: " + line, ExitCode: exitCodeOf(res, err)})
		return true
	}
	return false
}

func exitCodeOf(res proc.Result, err error) int {
	var ee *proc.ExitError
	if errors.As(err, &ee) && ee.Result.ExitCode > 0 {
		return ee.Result.ExitCode
	}
	if res.ExitCode > 0 {
		return res.ExitCode
	}
	return 1
}

// output is the final assistant text: the rule's Output when set, else the
// transcript, plus the structured block when configured.
func (s *session) output() string {
	out := s.rule.Result.Output
	if out == "" {
		out = s.text.String()
	}
	if len(s.rule.Result.Structured) > 0 {
		block, err := ResultBlock(s.rule.Result.Structured)
		if err == nil {
			if out != "" && !strings.HasSuffix(out, "\n") {
				out += "\n"
			}
			out += block
		}
	}
	return out
}

func (s *session) usage(output string) cartridge.Usage {
	if s.rule.Result.Usage != nil {
		return *s.rule.Result.Usage
	}
	return cartridge.Usage{
		InputTokens:  (len(s.spec.SystemPrompt) + len(s.spec.Prompt) + 3) / 4,
		OutputTokens: (len(output) + 3) / 4,
		Turns:        1 + s.tools,
	}
}

func (s *session) result(output string, exitCode int) cartridge.Result {
	id := s.rule.Result.SessionID
	if id == "" {
		id = "mock-" + strconv.Itoa(s.seq)
	}
	return cartridge.Result{
		Output:    output,
		Usage:     s.usage(output),
		Duration:  s.now().Sub(s.start),
		SessionID: id,
		ExitCode:  exitCode,
	}
}

func (s *session) succeed(output string) {
	s.pipe.Finish(s.result(output, 0), nil)
}

// end finishes a session stopped by the timeout, a kill or the context.
func (s *session) end(why stop) {
	partial := s.result(s.text.String(), -1)
	switch why {
	case stopTimeout:
		s.pipe.Finish(partial, cartridge.NewError(cartridge.KindTimeout, s.a.name,
			fmt.Sprintf("session exceeded its %s timeout", s.spec.Timeout), nil))
	case stopCancelled:
		s.pipe.Finish(partial, cartridge.NewError(cartridge.KindKilled, s.a.name, "context cancelled", context.Cause(s.ctx)))
	default:
		// Finish with a nil error after Kill reports ErrKilled.
		s.pipe.Finish(partial, nil)
	}
}

// fail injects f: a successful session with a broken block for
// FailMalformed, a wait until the spec's timeout for FailTimeout, and the
// matching cartridge error otherwise.
func (s *session) fail(f Failure) {
	switch f.Kind {
	case FailMalformed:
		body := f.Message
		if body == "" {
			body = `{"ok": tru`
		}
		block, _ := ResultBlock(body)
		out := s.text.String()
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		s.succeed(out + block)
		return
	case FailTimeout:
		if s.timeout != nil && !s.timedOut {
			select {
			case <-s.timeout:
				s.timedOut = true
			case <-s.killed:
				s.end(stopKilled)
				return
			case <-s.ctx.Done():
				s.end(stopCancelled)
				return
			}
		}
		if f.Message == "" {
			s.end(stopTimeout)
			return
		}
	}
	exit := -1
	if f.Kind == FailCrash || f.Kind == FailNotInstalled {
		exit = f.ExitCode
		if exit == 0 {
			exit = 1
		}
	}
	s.pipe.Finish(s.result(s.text.String(), exit), f.Err(s.a.name))
}
