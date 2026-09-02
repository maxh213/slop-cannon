package mock

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"slop-cannon/internal/cartridge"
)

// Duration is a time.Duration that reads and writes as a Go duration string
// ("500ms", "2m") in scenario JSON. Bare numbers are rejected so a scenario
// never accidentally sleeps for nanoseconds.
type Duration time.Duration

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// UnmarshalJSON implements json.Unmarshaler; only quoted duration strings
// are accepted.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("mock: duration must be a string such as \"500ms\": %w", err)
	}
	td, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("mock: invalid duration %q: %w", s, err)
	}
	if td < 0 {
		return fmt.Errorf("mock: duration %q is negative", s)
	}
	*d = Duration(td)
	return nil
}

// FailKind names an injected failure. All but FailMalformed map onto a
// cartridge.Kind; FailMalformed is a successful session whose output ends
// with an sc-result block that is not valid JSON.
type FailKind string

// Injected failure kinds.
const (
	// FailCrash ends the session as cartridge.ErrCrash with a non-zero exit
	// code (Failure.ExitCode, default 1); it stands in for an unparseable
	// CLI stream as much as for a real crash.
	FailCrash FailKind = "crash"
	// FailTimeout ends the session as cartridge.ErrTimeout. When the spec
	// has a Timeout the failure waits (on the Clock) until it elapses, so
	// the session times out exactly as a hung CLI would.
	FailTimeout FailKind = "timeout"
	// FailRateLimited ends the session as cartridge.ErrRateLimited, with
	// Failure.RetryAfter as the back-off hint.
	FailRateLimited FailKind = "rate_limited"
	// FailCreditsExhausted ends the session as cartridge.ErrCreditsExhausted
	// (the cartridge is dead for the run, D-18).
	FailCreditsExhausted FailKind = "credits_exhausted"
	// FailAuth ends the session as cartridge.ErrAuth.
	FailAuth FailKind = "auth"
	// FailKilled ends the session as cartridge.ErrKilled, as if something
	// outside the orchestrator stopped the process.
	FailKilled FailKind = "killed"
	// FailNotInstalled makes Detect report cartridge.ErrNotInstalled; in a
	// session it behaves like FailCrash.
	FailNotInstalled FailKind = "not_installed"
	// FailMalformed finishes successfully with an sc-result block that is
	// not valid JSON (Failure.Message when set, a truncated object
	// otherwise), which is what a model that botched its structured output
	// looks like to P2-07.
	FailMalformed FailKind = "malformed"
)

var failKinds = []FailKind{FailCrash, FailTimeout, FailRateLimited, FailCreditsExhausted, FailAuth, FailKilled, FailNotInstalled, FailMalformed}

// FailKinds returns every injectable failure kind.
func FailKinds() []FailKind { return append([]FailKind(nil), failKinds...) }

// String returns the canonical name.
func (k FailKind) String() string { return string(k) }

// Valid reports whether k is a declared kind.
func (k FailKind) Valid() bool {
	for _, x := range failKinds {
		if x == k {
			return true
		}
	}
	return false
}

// CartridgeKind returns the cartridge.Kind the failure reports as;
// FailMalformed is not an error and yields cartridge.KindUnknown.
func (k FailKind) CartridgeKind() cartridge.Kind {
	switch k {
	case FailCrash:
		return cartridge.KindCrash
	case FailTimeout:
		return cartridge.KindTimeout
	case FailRateLimited:
		return cartridge.KindRateLimited
	case FailCreditsExhausted:
		return cartridge.KindCreditsExhausted
	case FailAuth:
		return cartridge.KindAuth
	case FailKilled:
		return cartridge.KindKilled
	case FailNotInstalled:
		return cartridge.KindNotInstalled
	default:
		return cartridge.KindUnknown
	}
}

// Failure describes an injected failure.
type Failure struct {
	// Kind selects the failure.
	Kind FailKind `json:"kind"`
	// Message is the CLI's supposed message; for FailMalformed it is the
	// broken block body.
	Message string `json:"message,omitempty"`
	// RetryAfter is the back-off hint reported with FailRateLimited.
	RetryAfter Duration `json:"retry_after,omitempty"`
	// ExitCode is the exit status reported with FailCrash (default 1).
	ExitCode int `json:"exit_code,omitempty"`
}

// Err builds the *cartridge.Error the failure reports as, stamped with the
// adapter name. It is nil for FailMalformed.
func (f Failure) Err(name string) *cartridge.Error {
	kind := f.Kind.CartridgeKind()
	if kind == cartridge.KindUnknown {
		return nil
	}
	msg := f.Message
	if msg == "" {
		msg = "injected " + f.Kind.String()
	}
	e := cartridge.NewError(kind, name, msg, nil)
	e.RetryAfter = f.RetryAfter.Duration()
	return e
}

// Validate reports an unknown kind or nonsensical fields.
func (f Failure) Validate() error {
	if !f.Kind.Valid() {
		return fmt.Errorf("mock: unknown failure kind %q (want one of %s)", string(f.Kind), joinFailKinds())
	}
	if f.ExitCode < 0 {
		return fmt.Errorf("mock: failure exit code %d is negative", f.ExitCode)
	}
	return nil
}

func joinFailKinds() string {
	parts := make([]string, len(failKinds))
	for i, k := range failKinds {
		parts[i] = string(k)
	}
	return strings.Join(parts, ", ")
}

// StepKind names what a Step does.
type StepKind string

// Step kinds.
const (
	// StepText emits Text as an assistant text delta.
	StepText StepKind = "text"
	// StepEcho emits the spec's Prompt as an assistant text delta.
	StepEcho StepKind = "echo"
	// StepToolUse emits a tool-use event (Tool, ToolID, Input) without doing
	// anything.
	StepToolUse StepKind = "tool_use"
	// StepToolResult emits a tool-result event (Tool, ToolID, Output,
	// IsError) without doing anything.
	StepToolResult StepKind = "tool_result"
	// StepWrite writes Content to Path under the workdir (Append appends);
	// refused unless the policy allows writes.
	StepWrite StepKind = "write"
	// StepDelete removes Path under the workdir; refused unless the policy
	// allows writes.
	StepDelete StepKind = "delete"
	// StepExec runs Command with Args in the workdir and emits the output as
	// a tool result; refused unless the policy allows exec. A failing
	// command is an error tool result, or a crash when Must is set.
	StepExec StepKind = "exec"
	// StepSleep waits Delay and emits nothing.
	StepSleep StepKind = "sleep"
	// StepFail injects Failure and ends the session.
	StepFail StepKind = "fail"
)

var stepKinds = []StepKind{StepText, StepEcho, StepToolUse, StepToolResult, StepWrite, StepDelete, StepExec, StepSleep, StepFail}

// StepKinds returns every step kind.
func StepKinds() []StepKind { return append([]StepKind(nil), stepKinds...) }

// String returns the canonical name.
func (k StepKind) String() string { return string(k) }

// Valid reports whether k is a declared kind.
func (k StepKind) Valid() bool {
	for _, x := range stepKinds {
		if x == k {
			return true
		}
	}
	return false
}

// Step is one scripted action. Delay is waited on the Clock before the step
// runs, whatever the kind; the other fields are read per kind.
type Step struct {
	// Kind selects the action.
	Kind StepKind `json:"kind"`
	// Delay is waited before the step runs.
	Delay Duration `json:"delay,omitempty"`
	// Text is the assistant text (StepText).
	Text string `json:"text,omitempty"`
	// Tool, ToolID, Input, Output and IsError describe tool events
	// (StepToolUse, StepToolResult).
	Tool    string `json:"tool,omitempty"`
	ToolID  string `json:"tool_id,omitempty"`
	Input   string `json:"input,omitempty"`
	Output  string `json:"output,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
	// Path and Content name a file relative to the workdir and what to put
	// in it (StepWrite, StepDelete); Append appends instead of replacing.
	Path    string `json:"path,omitempty"`
	Content string `json:"content,omitempty"`
	Append  bool   `json:"append,omitempty"`
	// Command and Args are the program to run (StepExec); Must turns a
	// failing command into a session crash.
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	Must    bool     `json:"must,omitempty"`
	// Failure is what to inject (StepFail).
	Failure *Failure `json:"failure,omitempty"`
}

// Validate reports a malformed step.
func (s Step) Validate() error {
	if !s.Kind.Valid() {
		return fmt.Errorf("mock: unknown step kind %q", string(s.Kind))
	}
	switch s.Kind {
	case StepWrite, StepDelete:
		if err := checkRelPath(s.Path); err != nil {
			return err
		}
	case StepExec:
		if strings.TrimSpace(s.Command) == "" {
			return errors.New("mock: exec step has no command")
		}
	case StepFail:
		if s.Failure == nil {
			return errors.New("mock: fail step has no failure")
		}
		return s.Failure.Validate()
	}
	return nil
}

func checkRelPath(p string) error {
	switch {
	case p == "":
		return errors.New("mock: file step has no path")
	case filepath.IsAbs(p):
		return fmt.Errorf("mock: file step path %q must be relative to the workdir", p)
	}
	clean := filepath.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("mock: file step path %q escapes the workdir", p)
	}
	return nil
}

// Match selects the firings a Rule applies to. Every set field must hold;
// an empty Match matches everything. Role, Lens, Bead, Round, Task and
// Agent compare against the standard labels cartridge.LabelsFor sets;
// Labels compares any label, where the value "*" only requires the key to
// be present.
type Match struct {
	// Role is the agent role label ("po", "engineer", "tester").
	Role string `json:"role,omitempty"`
	// Lens is the tester lens label ("ux", "qa", "test").
	Lens string `json:"lens,omitempty"`
	// Bead is the bead id label.
	Bead string `json:"bead,omitempty"`
	// Round is the review round label; zero matches any round.
	Round int `json:"round,omitempty"`
	// Task is the events.Task label ("implement", "review", ...).
	Task string `json:"task,omitempty"`
	// Agent is the agent id label.
	Agent string `json:"agent,omitempty"`
	// Labels are further label requirements.
	Labels map[string]string `json:"labels,omitempty"`
	// PromptContains must be a substring of the prompt.
	PromptContains string `json:"prompt_contains,omitempty"`
	// SystemPromptContains must be a substring of the system prompt.
	SystemPromptContains string `json:"system_prompt_contains,omitempty"`
	// ToolPolicy must equal the spec's policy.
	ToolPolicy string `json:"tool_policy,omitempty"`
	// Model must equal the spec's model.
	Model string `json:"model,omitempty"`
}

// Matches reports whether spec satisfies every requirement of m.
func (m Match) Matches(spec cartridge.RunSpec) bool {
	l := spec.Labels
	if m.Role != "" && l.Get(cartridge.LabelRole) != m.Role {
		return false
	}
	if m.Lens != "" && l.Get(cartridge.LabelLens) != m.Lens {
		return false
	}
	if m.Bead != "" && l.Get(cartridge.LabelBead) != m.Bead {
		return false
	}
	if m.Round != 0 && l.Get(cartridge.LabelRound) != strconv.Itoa(m.Round) {
		return false
	}
	if m.Task != "" && l.Get(cartridge.LabelTask) != m.Task {
		return false
	}
	if m.Agent != "" && l.Get(cartridge.LabelAgent) != m.Agent {
		return false
	}
	for k, want := range m.Labels {
		got, ok := l[k]
		if !ok || (want != "*" && got != want) {
			return false
		}
	}
	if m.PromptContains != "" && !strings.Contains(spec.Prompt, m.PromptContains) {
		return false
	}
	if m.SystemPromptContains != "" && !strings.Contains(spec.SystemPrompt, m.SystemPromptContains) {
		return false
	}
	if m.ToolPolicy != "" && string(spec.ToolPolicy) != m.ToolPolicy {
		return false
	}
	if m.Model != "" && spec.Model != m.Model {
		return false
	}
	return true
}

// String renders the match compactly for logs and errors.
func (m Match) String() string {
	var parts []string
	add := func(k, v string) {
		if v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	add("role", m.Role)
	add("lens", m.Lens)
	add("bead", m.Bead)
	if m.Round != 0 {
		add("round", strconv.Itoa(m.Round))
	}
	add("task", m.Task)
	add("agent", m.Agent)
	for _, k := range sortedKeys(m.Labels) {
		add(k, m.Labels[k])
	}
	add("prompt~", m.PromptContains)
	add("system~", m.SystemPromptContains)
	add("policy", m.ToolPolicy)
	add("model", m.Model)
	if len(parts) == 0 {
		return "*"
	}
	return strings.Join(parts, " ")
}

func sortedKeys(m map[string]string) []string {
	return cartridge.Labels(m).Keys()
}

// ResultSpec shapes the successful outcome of a rule.
type ResultSpec struct {
	// Output replaces the transcript of text steps as Result.Output when
	// set.
	Output string `json:"output,omitempty"`
	// Structured, when set, is appended to the output as an sc-result block
	// (see ResultBlock).
	Structured json.RawMessage `json:"structured,omitempty"`
	// Usage overrides the derived token counts when set.
	Usage *cartridge.Usage `json:"usage,omitempty"`
	// SessionID is reported as Result.SessionID; default "mock-<seq>".
	SessionID string `json:"session_id,omitempty"`
}

// Rule is one scripted response: a Match, an optional use budget, the Steps
// to play and the Result to finish with. Rules are matched in scenario
// order; a rule whose MaxUses is spent is skipped so a sequence of rules
// with the same Match plays out call by call (rate-limit first, succeed
// next).
type Rule struct {
	// Name identifies the rule in Call records and errors.
	Name string `json:"name"`
	// Match selects the firings the rule answers.
	Match Match `json:"match"`
	// MaxUses caps how many sessions the rule answers; zero is unlimited.
	MaxUses int `json:"max_uses,omitempty"`
	// Steps are played in order.
	Steps []Step `json:"steps,omitempty"`
	// Result shapes the success outcome (ignored when a step fails).
	Result ResultSpec `json:"result"`

	err error
}

// Validate reports the first problem with the rule.
func (r *Rule) Validate() error {
	if r == nil {
		return errors.New("mock: nil rule")
	}
	if r.err != nil {
		return fmt.Errorf("mock: rule %q: %w", r.Name, r.err)
	}
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("mock: rule has no name")
	}
	if r.MaxUses < 0 {
		return fmt.Errorf("mock: rule %q: max uses %d is negative", r.Name, r.MaxUses)
	}
	for i, s := range r.Steps {
		if err := s.Validate(); err != nil {
			return fmt.Errorf("mock: rule %q step %d: %w", r.Name, i+1, err)
		}
	}
	if len(r.Result.Structured) > 0 && !json.Valid(r.Result.Structured) {
		return fmt.Errorf("mock: rule %q: structured result is not valid JSON", r.Name)
	}
	return nil
}

// DetectSpec scripts what Detect reports.
type DetectSpec struct {
	// Installed reports the binary missing when explicitly false.
	Installed *bool `json:"installed,omitempty"`
	// Version is the reported version; default "mock/1".
	Version string `json:"version,omitempty"`
	// Auth is the reported auth state: "ok" (default), "missing" or "".
	Auth cartridge.AuthState `json:"auth,omitempty"`
	// Delay is waited on the Clock before answering.
	Delay Duration `json:"delay,omitempty"`
	// Failure, when set, is returned as Detect's error (its kind); Status
	// is still filled in.
	Failure *Failure `json:"failure,omitempty"`
}

// Validate reports a malformed detect spec.
func (d DetectSpec) Validate() error {
	switch d.Auth {
	case cartridge.AuthUnknown, cartridge.AuthOK, cartridge.AuthMissing:
	default:
		return fmt.Errorf("mock: detect auth %q is not one of ok, missing", string(d.Auth))
	}
	if d.Failure != nil {
		if err := d.Failure.Validate(); err != nil {
			return fmt.Errorf("mock: detect: %w", err)
		}
	}
	return nil
}

// Scenario is the script an Adapter plays.
type Scenario struct {
	// Name labels the scenario in Detect's Status.Detail.
	Name string `json:"name,omitempty"`
	// Detect scripts Detect.
	Detect DetectSpec `json:"detect"`
	// Rules are tried in order for every Run.
	Rules []*Rule `json:"rules"`
	// Fallback answers firings no rule matches; nil means Run fails with
	// ErrNoMatch.
	Fallback *Rule `json:"fallback,omitempty"`
}

// NewScenario returns an empty scenario for the builder methods.
func NewScenario(name string) *Scenario { return &Scenario{Name: name} }

// Rule appends a rule and returns it for chaining.
func (s *Scenario) Rule(name string, m Match) *Rule {
	r := &Rule{Name: name, Match: m}
	s.Rules = append(s.Rules, r)
	return r
}

// Otherwise sets the fallback rule and returns it for chaining.
func (s *Scenario) Otherwise(name string) *Rule {
	r := &Rule{Name: name}
	s.Fallback = r
	return r
}

// Validate reports the first problem with the scenario: an invalid detect
// spec, a duplicate or malformed rule, a malformed fallback.
func (s *Scenario) Validate() error {
	if s == nil {
		return errors.New("mock: nil scenario")
	}
	if err := s.Detect.Validate(); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, r := range s.Rules {
		if err := r.Validate(); err != nil {
			return err
		}
		if seen[r.Name] {
			return fmt.Errorf("mock: duplicate rule name %q", r.Name)
		}
		seen[r.Name] = true
	}
	if s.Fallback != nil {
		if err := s.Fallback.Validate(); err != nil {
			return fmt.Errorf("mock: fallback: %w", err)
		}
		if seen[s.Fallback.Name] {
			return fmt.Errorf("mock: fallback rule name %q duplicates a rule", s.Fallback.Name)
		}
	}
	return nil
}

// Marshal renders the scenario as indented JSON that Parse reads back.
func (s *Scenario) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("mock: marshal scenario: %w", err)
	}
	return append(b, '\n'), nil
}

// WriteFile validates the scenario and writes it as JSON to path, creating
// parent directories. Tests use it to hand a programmatic scenario to an
// adapter built from configuration.
func (s *Scenario) WriteFile(path string) error {
	if err := s.Validate(); err != nil {
		return err
	}
	b, err := s.Marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mock: write scenario: %w", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("mock: write scenario: %w", err)
	}
	return nil
}

// Parse decodes and validates scenario JSON. Unknown fields are errors so a
// typo in a fixture does not silently disable a step.
func Parse(data []byte) (*Scenario, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var s Scenario
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("mock: parse scenario: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// Load reads and parses a scenario file.
func Load(path string) (*Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("mock: load scenario: %w", err)
	}
	s, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// DefaultScenario is what an adapter built from configuration without a
// scenario file plays: Detect succeeds and every firing echoes its prompt
// and finishes successfully.
func DefaultScenario() *Scenario {
	s := NewScenario("default")
	s.Otherwise("echo").Echo()
	return s
}

// ResultBlock renders v as the fenced sc-result block the structured-output
// contract (P2-07) expects at the end of a model's answer. Strings are
// embedded as they are (which is how a broken block is scripted); raw JSON
// is compacted; anything else is JSON-encoded.
func ResultBlock(v any) (string, error) {
	var body string
	switch x := v.(type) {
	case string:
		body = x
	case json.RawMessage:
		body = compactJSON(x)
	case []byte:
		body = compactJSON(x)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "", fmt.Errorf("mock: result block: %w", err)
		}
		body = string(b)
	}
	return "```sc-result\n" + strings.TrimRight(body, "\n") + "\n```", nil
}

// compactJSON strips insignificant whitespace from valid JSON and returns
// anything else unchanged.
func compactJSON(b []byte) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return string(b)
	}
	return buf.String()
}

// Builder methods on Rule. Each appends a step (or sets a field) and returns
// the rule so calls chain.

// Say appends a text step.
func (r *Rule) Say(text string) *Rule { return r.add(Step{Kind: StepText, Text: text}) }

// Echo appends a step that repeats the prompt as assistant text.
func (r *Rule) Echo() *Rule { return r.add(Step{Kind: StepEcho}) }

// Sleep appends a pause.
func (r *Rule) Sleep(d time.Duration) *Rule { return r.add(Step{Kind: StepSleep, Delay: Duration(d)}) }

// Tool appends a tool-use event.
func (r *Rule) Tool(tool, id, input string) *Rule {
	return r.add(Step{Kind: StepToolUse, Tool: tool, ToolID: id, Input: input})
}

// ToolResult appends a tool-result event.
func (r *Rule) ToolResult(tool, id, output string, isError bool) *Rule {
	return r.add(Step{Kind: StepToolResult, Tool: tool, ToolID: id, Output: output, IsError: isError})
}

// Write appends a step that writes content to path under the workdir.
func (r *Rule) Write(path, content string) *Rule {
	return r.add(Step{Kind: StepWrite, Path: path, Content: content})
}

// AppendFile appends a step that appends content to path under the workdir.
func (r *Rule) AppendFile(path, content string) *Rule {
	return r.add(Step{Kind: StepWrite, Path: path, Content: content, Append: true})
}

// Delete appends a step that removes path under the workdir.
func (r *Rule) Delete(path string) *Rule { return r.add(Step{Kind: StepDelete, Path: path}) }

// Exec appends a step that runs command in the workdir; a failing command
// becomes an error tool result.
func (r *Rule) Exec(command string, args ...string) *Rule {
	return r.add(Step{Kind: StepExec, Command: command, Args: append([]string(nil), args...)})
}

// MustExec is Exec whose failure crashes the session.
func (r *Rule) MustExec(command string, args ...string) *Rule {
	return r.add(Step{Kind: StepExec, Command: command, Args: append([]string(nil), args...), Must: true})
}

// Fail appends a failure of kind with message; it ends the session.
func (r *Rule) Fail(kind FailKind, message string) *Rule {
	return r.FailWith(Failure{Kind: kind, Message: message})
}

// FailWith appends the given failure; it ends the session.
func (r *Rule) FailWith(f Failure) *Rule {
	cp := f
	return r.add(Step{Kind: StepFail, Failure: &cp})
}

// Delay sets the delay of the most recently added step (a no-op before the
// first step).
func (r *Rule) Delay(d time.Duration) *Rule {
	if n := len(r.Steps); n > 0 {
		r.Steps[n-1].Delay = Duration(d)
	}
	return r
}

// Output sets the final output text, replacing the transcript.
func (r *Rule) Output(text string) *Rule {
	r.Result.Output = text
	return r
}

// Structured appends v as an sc-result block to the output. A value that
// cannot be encoded makes Validate fail.
func (r *Rule) Structured(v any) *Rule {
	var raw json.RawMessage
	switch x := v.(type) {
	case json.RawMessage:
		raw = x
	case string:
		raw = json.RawMessage(x)
	case []byte:
		raw = json.RawMessage(x)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			r.err = fmt.Errorf("structured result: %w", err)
			return r
		}
		raw = b
	}
	r.Result.Structured = raw
	return r
}

// Usage sets the reported usage.
func (r *Rule) Usage(u cartridge.Usage) *Rule {
	r.Result.Usage = &u
	return r
}

// SessionID sets the reported session id.
func (r *Rule) SessionID(id string) *Rule {
	r.Result.SessionID = id
	return r
}

// Once limits the rule to a single session.
func (r *Rule) Once() *Rule { return r.Times(1) }

// Times limits the rule to n sessions.
func (r *Rule) Times(n int) *Rule {
	r.MaxUses = n
	return r
}

func (r *Rule) add(s Step) *Rule {
	r.Steps = append(r.Steps, s)
	return r
}
