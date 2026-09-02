package cartridge

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/proc"
)

// ToolPolicy says what an agent may do to the working directory. Adapters map
// it onto their CLI's tool controls (P5-06); the mock honours it directly.
type ToolPolicy string

// Tool policies, from least to most permissive.
const (
	// ToolReadOnly lets the model read files and search; no shell, no writes.
	// Used by the planning PO.
	ToolReadOnly ToolPolicy = "read_only"
	// ToolReadExec adds running commands (tests, builds) but still no file
	// writes. Used by testers.
	ToolReadExec ToolPolicy = "read_exec"
	// ToolReadWrite allows everything: reads, shell, edits. Used by engineers.
	ToolReadWrite ToolPolicy = "read_write"
)

var toolPolicies = []ToolPolicy{ToolReadOnly, ToolReadExec, ToolReadWrite}

// ToolPolicies returns every valid policy from least to most permissive.
func ToolPolicies() []ToolPolicy { return append([]ToolPolicy(nil), toolPolicies...) }

// ParseToolPolicy parses a policy name leniently: trimmed, case-insensitive,
// hyphens accepted for underscores. The error wraps domain.ErrUnknownValue.
func ParseToolPolicy(s string) (ToolPolicy, error) {
	n := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "_")
	if n == "" {
		return "", fmt.Errorf("%w: empty tool policy (want one of %s)", domain.ErrUnknownValue, joinPolicies())
	}
	for _, p := range toolPolicies {
		if string(p) == n {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: tool policy %q (want one of %s)", domain.ErrUnknownValue, s, joinPolicies())
}

func joinPolicies() string {
	parts := make([]string, len(toolPolicies))
	for i, p := range toolPolicies {
		parts[i] = string(p)
	}
	return strings.Join(parts, ", ")
}

// String returns the canonical name.
func (p ToolPolicy) String() string { return string(p) }

// Valid reports whether p is one of the declared policies.
func (p ToolPolicy) Valid() bool {
	for _, x := range toolPolicies {
		if x == p {
			return true
		}
	}
	return false
}

// AllowsExec reports whether the policy permits running commands.
func (p ToolPolicy) AllowsExec() bool { return p == ToolReadExec || p == ToolReadWrite }

// AllowsWrite reports whether the policy permits editing files.
func (p ToolPolicy) AllowsWrite() bool { return p == ToolReadWrite }

// MarshalText implements encoding.TextMarshaler; the zero value marshals as
// the empty string, anything else must be valid.
func (p ToolPolicy) MarshalText() ([]byte, error) {
	if p == "" {
		return []byte{}, nil
	}
	if !p.Valid() {
		return nil, fmt.Errorf("%w: cannot marshal tool policy %q (want one of %s)", domain.ErrUnknownValue, string(p), joinPolicies())
	}
	return []byte(p), nil
}

// UnmarshalText implements encoding.TextUnmarshaler; empty text yields the
// zero value.
func (p *ToolPolicy) UnmarshalText(b []byte) error {
	if strings.TrimSpace(string(b)) == "" {
		*p = ""
		return nil
	}
	v, err := ParseToolPolicy(string(b))
	if err != nil {
		return err
	}
	*p = v
	return nil
}

// Labels are free-form key/value tags on a RunSpec. The orchestrator sets the
// Label* keys below so the mock cartridge can match scenarios on them
// (P2-02) and adapters can name log files; adapters must not change
// behaviour based on them.
type Labels map[string]string

// Well-known label keys.
const (
	// LabelRun is the run id.
	LabelRun = "run"
	// LabelAgent is the agent id.
	LabelAgent = "agent"
	// LabelRole is the agent role: po, engineer, tester.
	LabelRole = "role"
	// LabelLens is the tester lens: ux, qa, test (testers only).
	LabelLens = "lens"
	// LabelBead is the bead id the agent works on (empty for planning).
	LabelBead = "bead"
	// LabelRound is the review round number, as a decimal string.
	LabelRound = "round"
	// LabelTask is the events.Task the agent was fired for.
	LabelTask = "task"
)

// Get returns the value for key, or "" when absent (also on a nil map).
func (l Labels) Get(key string) string { return l[key] }

// Clone returns an independent copy; nil stays nil.
func (l Labels) Clone() Labels {
	if l == nil {
		return nil
	}
	out := make(Labels, len(l))
	for k, v := range l {
		out[k] = v
	}
	return out
}

// Keys returns the label keys in sorted order.
func (l Labels) Keys() []string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// LabelsFor builds the standard labels for an agent firing: run is omitted
// when empty, lens and bead when the agent has none, round when zero.
func LabelsFor(run domain.RunID, a domain.Agent, task events.Task, round int) Labels {
	l := Labels{LabelAgent: a.ID.String(), LabelRole: a.Role.String(), LabelTask: task.String()}
	if run != "" {
		l[LabelRun] = run.String()
	}
	if a.Lens != "" {
		l[LabelLens] = a.Lens.String()
	}
	if a.Bead != "" {
		l[LabelBead] = a.Bead.String()
	}
	if round > 0 {
		l[LabelRound] = strconv.Itoa(round)
	}
	return l
}

// DefaultInheritEnv returns the parent environment variables a CLI needs to
// find its home, config, cache, locale and TLS roots. PATH is always passed
// through by internal/proc and is not listed. Adapters extend the list with
// their own variables (for example Claude's ANTHROPIC_* keys); nothing else
// leaks into the child, which is what keeps agent environments clean.
func DefaultInheritEnv() []string {
	return []string{
		"HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TERM",
		"LANG", "LC_ALL", "LC_CTYPE",
		"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "NO_PROXY", "HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "no_proxy",
	}
}

// RunSpec describes one firing of a cartridge: what to say, where, and under
// which limits. It is a value; adapters must not retain the Env map.
type RunSpec struct {
	// SystemPrompt is appended to (or replaces, adapter-dependent) the CLI's
	// system prompt. May be empty.
	SystemPrompt string `json:"system_prompt,omitempty"`
	// Prompt is the user prompt. Required.
	Prompt string `json:"prompt"`
	// Workdir is the working directory the CLI runs in, normally the agent's
	// worktree. Required; must be absolute.
	Workdir string `json:"workdir"`
	// Model is passed to the CLI when non-empty; the adapter's default
	// otherwise.
	Model string `json:"model,omitempty"`
	// ToolPolicy bounds what the model may do in Workdir. Required.
	ToolPolicy ToolPolicy `json:"tool_policy"`
	// MaxTurns caps agentic turns; zero means the adapter's default.
	MaxTurns int `json:"max_turns,omitempty"`
	// Timeout kills the session when it elapses; zero means no limit (the
	// orchestrator always sets one, D-08 agent_timeout).
	Timeout time.Duration `json:"timeout,omitempty"`
	// Env holds variables set explicitly on the child process, on top of the
	// allowlist. Values here win over inherited ones of the same name.
	Env map[string]string `json:"env,omitempty"`
	// InheritEnv is the allowlist of parent variables passed through when
	// set. PATH is always inherited. Nil means DefaultInheritEnv().
	InheritEnv []string `json:"inherit_env,omitempty"`
	// Labels tag the firing for the mock and for logs; see LabelsFor.
	Labels Labels `json:"labels,omitempty"`
}

// Validate reports the first problem with the spec: an empty prompt, a
// missing or relative workdir, an invalid tool policy, or negative limits.
func (s RunSpec) Validate() error {
	switch {
	case strings.TrimSpace(s.Prompt) == "":
		return errors.New("cartridge: run spec has an empty prompt")
	case s.Workdir == "":
		return errors.New("cartridge: run spec has no workdir")
	case !filepath.IsAbs(s.Workdir):
		return fmt.Errorf("cartridge: run spec workdir %q is not absolute", s.Workdir)
	case s.ToolPolicy == "":
		return errors.New("cartridge: run spec has no tool policy")
	case !s.ToolPolicy.Valid():
		return fmt.Errorf("cartridge: run spec tool policy %q is not one of %s", string(s.ToolPolicy), joinPolicies())
	case s.MaxTurns < 0:
		return fmt.Errorf("cartridge: run spec max turns %d is negative", s.MaxTurns)
	case s.Timeout < 0:
		return fmt.Errorf("cartridge: run spec timeout %v is negative", s.Timeout)
	}
	return nil
}

// EnvAllowlist returns the effective allowlist: InheritEnv when set, else
// DefaultInheritEnv().
func (s RunSpec) EnvAllowlist() []string {
	if s.InheritEnv != nil {
		return append([]string(nil), s.InheritEnv...)
	}
	return DefaultInheritEnv()
}

// ProcSpec builds the internal/proc invocation for command and args with the
// spec's workdir, timeout and clean environment (the allowlist plus Env).
// Adapters set Stdin and GracePeriod themselves.
func (s RunSpec) ProcSpec(command string, args ...string) proc.Spec {
	env := make(map[string]string, len(s.Env))
	for k, v := range s.Env {
		env[k] = v
	}
	return proc.Spec{
		Command:    command,
		Args:       append([]string(nil), args...),
		Dir:        s.Workdir,
		Env:        env,
		InheritEnv: s.EnvAllowlist(),
		Timeout:    s.Timeout,
	}
}

// Usage is what a session consumed, as far as the CLI reported it. Adapters
// leave fields zero when their CLI does not report them.
type Usage struct {
	// InputTokens and OutputTokens are the totals over every turn.
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// CacheReadTokens and CacheWriteTokens are prompt-cache totals when the
	// CLI distinguishes them.
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	// CostUSD is the cost the CLI reported; meaningful only when HasCost.
	CostUSD float64 `json:"cost_usd,omitempty"`
	// HasCost is true when the CLI reported a cost figure (P9-01 enforces
	// max_cost_usd only when it is).
	HasCost bool `json:"has_cost,omitempty"`
	// Turns is the number of agentic turns, when reported.
	Turns int `json:"turns,omitempty"`
}

// Add returns the sum of u and other; HasCost is set when either has one.
func (u Usage) Add(other Usage) Usage {
	return Usage{
		InputTokens:      u.InputTokens + other.InputTokens,
		OutputTokens:     u.OutputTokens + other.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens + other.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens + other.CacheWriteTokens,
		CostUSD:          u.CostUSD + other.CostUSD,
		HasCost:          u.HasCost || other.HasCost,
		Turns:            u.Turns + other.Turns,
	}
}

// TotalTokens is input plus output tokens (cache tokens excluded).
func (u Usage) TotalTokens() int { return u.InputTokens + u.OutputTokens }

// Result is what a finished session produced.
type Result struct {
	// Output is the model's final text: the last assistant message in full.
	// The structured-output parser (P2-07) reads the sc-result block from it.
	Output string `json:"output"`
	// Usage is what the session consumed.
	Usage Usage `json:"usage"`
	// Duration is wall time from spawn to exit.
	Duration time.Duration `json:"duration"`
	// SessionID is the CLI's own session or conversation id when it reports
	// one, so a human can resume the transcript with the CLI.
	SessionID string `json:"session_id,omitempty"`
	// ExitCode is the subprocess exit status, or -1 when killed or unknown.
	ExitCode int `json:"exit_code"`
}

// EventKind names the variants of SessionEvent.
type EventKind string

// Session event kinds.
const (
	// EventTextDelta carries a chunk of assistant text in Text.
	EventTextDelta EventKind = "text_delta"
	// EventToolUse says the model invoked a tool: Tool, ToolID, Input.
	EventToolUse EventKind = "tool_use"
	// EventToolResult carries the tool's output: Tool, ToolID, Output, IsError.
	EventToolResult EventKind = "tool_result"
	// EventDone is the terminal success event; Result is set.
	EventDone EventKind = "done"
	// EventError is the terminal failure event; Err is a *Error and Usage
	// holds what was consumed before the failure.
	EventError EventKind = "error"
)

// String returns the canonical name.
func (k EventKind) String() string { return string(k) }

// Terminal reports whether the kind ends a session's event stream.
func (k EventKind) Terminal() bool { return k == EventDone || k == EventError }

// SessionEvent is one thing a session did, delivered on Session.Events.
// Exactly one field group is set per Kind; see the EventKind constants.
// A stream ends with exactly one terminal event (Done or Error).
type SessionEvent struct {
	// Kind selects the variant.
	Kind EventKind `json:"kind"`
	// At is when the adapter observed it.
	At time.Time `json:"at"`
	// Text is the assistant text chunk (EventTextDelta).
	Text string `json:"text,omitempty"`
	// Tool is the tool name (EventToolUse, EventToolResult).
	Tool string `json:"tool,omitempty"`
	// ToolID correlates a tool result with its use, when the CLI provides
	// one.
	ToolID string `json:"tool_id,omitempty"`
	// Input is a one-line rendering of the tool input (EventToolUse), for
	// transcripts; adapters keep it short.
	Input string `json:"input,omitempty"`
	// Output is the tool result text, possibly truncated by the adapter
	// (EventToolResult).
	Output string `json:"output,omitempty"`
	// IsError marks a tool result the tool itself reported as failed.
	IsError bool `json:"is_error,omitempty"`
	// Result is the session outcome (EventDone).
	Result *Result `json:"result,omitempty"`
	// Usage is what was consumed before a failure (EventError); for
	// EventDone it duplicates Result.Usage.
	Usage Usage `json:"usage,omitempty"`
	// Err is the failure (EventError), always a *Error.
	Err error `json:"-"`
}

// TextDelta builds an EventTextDelta event stamped now.
func TextDelta(text string) SessionEvent {
	return SessionEvent{Kind: EventTextDelta, At: time.Now(), Text: text}
}

// ToolUse builds an EventToolUse event stamped now.
func ToolUse(tool, id, input string) SessionEvent {
	return SessionEvent{Kind: EventToolUse, At: time.Now(), Tool: tool, ToolID: id, Input: input}
}

// ToolResult builds an EventToolResult event stamped now.
func ToolResult(tool, id, output string, isError bool) SessionEvent {
	return SessionEvent{Kind: EventToolResult, At: time.Now(), Tool: tool, ToolID: id, Output: output, IsError: isError}
}

// Done builds the terminal success event stamped now.
func Done(res Result) SessionEvent {
	r := res
	return SessionEvent{Kind: EventDone, At: time.Now(), Result: &r, Usage: res.Usage}
}

// ErrorEvent builds the terminal failure event stamped now. A nil err is
// replaced by an unknown-kind Error so the stream still terminates; an err
// that is not a *Error is wrapped with the kind KindOf reports.
func ErrorEvent(err error, usage Usage) SessionEvent {
	return SessionEvent{Kind: EventError, At: time.Now(), Err: AsError("", err), Usage: usage}
}
