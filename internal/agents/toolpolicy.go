package agents

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/claude"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// This file is the tool-policy layer (P5-06). It answers two questions:
//
//  1. Which cartridge.ToolPolicy does a firing run under? That follows from
//     the role and the task: the product owner researches read-only, testers
//     may run commands but not edit, engineers may do everything. See
//     [Policy] and [ApplyPolicy].
//  2. How will the chosen cartridge actually enforce that policy? Adapters
//     differ, so the mapping lives behind a per-adapter method and is
//     resolved by [EnforcementFor]. Claude Code translates the policy into
//     its own tool controls (claude.ToolFlags: --permission-mode dontAsk
//     plus allowed/disallowed tool lists) and the mock honours the policy
//     in-process. A future adapter with no tool controls at all cannot be
//     trusted with read_only or read_exec, so it resolves to
//     [EnforceCopyWorktree]: the orchestrator would copy the worktree to a
//     temporary directory, fire the agent there, and throw the copy away, so
//     a tester that writes cannot damage the reviewed tree. That degrade is
//     documented, not implemented (P5-06); [Enforcement.Check] fails loudly
//     with [ErrCopyWorktreeUnimplemented] until it is.

// rolePolicies is the tool policy each role runs under when no task narrows
// it (D-05 roles; see the per-role Phase 5 tasks).
var rolePolicies = map[domain.Role]cartridge.ToolPolicy{
	domain.RolePO:       cartridge.ToolReadOnly,
	domain.RoleEngineer: cartridge.ToolReadWrite,
	domain.RoleTester:   cartridge.ToolReadExec,
}

// taskRule is the role that performs a task and the policy it needs.
type taskRule struct {
	role   domain.Role
	policy cartridge.ToolPolicy
}

// taskPolicies covers every events.Task the orchestrator fires. Tasks are
// free-form (events.Task), so an unknown one is an error rather than a
// silent read_write.
var taskPolicies = map[events.Task]taskRule{
	events.TaskPlan:             {domain.RolePO, cartridge.ToolReadOnly},
	events.TaskArbitrate:        {domain.RolePO, cartridge.ToolReadOnly},
	events.TaskEscalate:         {domain.RolePO, cartridge.ToolReadOnly},
	events.TaskImplement:        {domain.RoleEngineer, cartridge.ToolReadWrite},
	events.TaskAddressFeedback:  {domain.RoleEngineer, cartridge.ToolReadWrite},
	events.TaskResolveConflict:  {domain.RoleEngineer, cartridge.ToolReadWrite},
	events.TaskReview:           {domain.RoleTester, cartridge.ToolReadExec},
	events.TaskEvaluatePushback: {domain.RoleTester, cartridge.ToolReadExec},
	events.TaskReviewPR:         {domain.RoleTester, cartridge.ToolReadExec},
}

// PolicyForRole returns the tool policy a role runs under: read_only for the
// product owner, read_exec for testers, read_write for engineers. The error
// wraps domain.ErrUnknownValue for an unknown or empty role.
func PolicyForRole(r domain.Role) (cartridge.ToolPolicy, error) {
	p, ok := rolePolicies[r]
	if !ok {
		return "", fmt.Errorf("%w: role %q has no tool policy (want one of %s)", domain.ErrUnknownValue, string(r), joinRoles())
	}
	return p, nil
}

// PolicyForTask returns the tool policy a task runs under. The error wraps
// domain.ErrUnknownValue for a task the orchestrator does not define.
func PolicyForTask(t events.Task) (cartridge.ToolPolicy, error) {
	rule, ok := taskPolicies[t]
	if !ok {
		return "", fmt.Errorf("%w: task %q has no tool policy (want one of %s)", domain.ErrUnknownValue, string(t), joinTasks())
	}
	return rule.policy, nil
}

// RoleForTask returns the role that performs a task. The error wraps
// domain.ErrUnknownValue for an unknown task.
func RoleForTask(t events.Task) (domain.Role, error) {
	rule, ok := taskPolicies[t]
	if !ok {
		return "", fmt.Errorf("%w: task %q has no role (want one of %s)", domain.ErrUnknownValue, string(t), joinTasks())
	}
	return rule.role, nil
}

// PolicyTasks returns every task with a declared policy, in name order.
func PolicyTasks() []events.Task {
	out := make([]events.Task, 0, len(taskPolicies))
	for t := range taskPolicies {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Policy returns the tool policy for one firing. An empty task falls back to
// the role's default; an empty role is taken from the task. When both are
// given they must agree — a tester fired to implement is a bug, not a
// permission upgrade — and the error wraps domain.ErrUnknownValue.
func Policy(r domain.Role, t events.Task) (cartridge.ToolPolicy, error) {
	switch {
	case t == "":
		return PolicyForRole(r)
	case r == "":
		return PolicyForTask(t)
	}
	rule, ok := taskPolicies[t]
	if !ok {
		return "", fmt.Errorf("%w: task %q has no tool policy (want one of %s)", domain.ErrUnknownValue, string(t), joinTasks())
	}
	if _, ok := rolePolicies[r]; !ok {
		return "", fmt.Errorf("%w: role %q has no tool policy (want one of %s)", domain.ErrUnknownValue, string(r), joinRoles())
	}
	if rule.role != r {
		return "", fmt.Errorf("%w: task %q belongs to role %q, not %q", domain.ErrUnknownValue, string(t), string(rule.role), string(r))
	}
	return rule.policy, nil
}

// ApplyPolicy sets spec.ToolPolicy from the role and task (see [Policy]). It
// is the only way agent code should fill that field, so no firing can be
// built with a policy nobody chose.
func ApplyPolicy(spec *cartridge.RunSpec, r domain.Role, t events.Task) error {
	if spec == nil {
		return errors.New("agents: cannot apply a tool policy to a nil run spec")
	}
	p, err := Policy(r, t)
	if err != nil {
		return err
	}
	spec.ToolPolicy = p
	return nil
}

func joinRoles() string {
	names := make([]string, 0, len(rolePolicies))
	for _, r := range domain.Roles() {
		names = append(names, r.String())
	}
	return strings.Join(names, ", ")
}

func joinTasks() string {
	tasks := PolicyTasks()
	names := make([]string, len(tasks))
	for i, t := range tasks {
		names[i] = t.String()
	}
	return strings.Join(names, ", ")
}

// EnforcementMode says how a cartridge keeps a firing inside its tool
// policy.
type EnforcementMode string

// Enforcement modes.
const (
	// EnforceNative means the adapter restricts the session itself: Claude
	// Code through --permission-mode plus allowed/disallowed tool lists, the
	// mock by refusing forbidden steps in-process.
	EnforceNative EnforcementMode = "native"
	// EnforceNone means there is nothing to restrict: read_write allows
	// every tool, so an adapter without tool controls is already correct.
	EnforceNone EnforcementMode = "none"
	// EnforceCopyWorktree means the adapter has no tool controls and the
	// policy forbids something, so the firing must happen in a throwaway
	// copy of the worktree. Documented, not implemented (P5-06); see
	// [ErrCopyWorktreeUnimplemented].
	EnforceCopyWorktree EnforcementMode = "copy_worktree"
)

// String returns the canonical name.
func (m EnforcementMode) String() string { return string(m) }

// ErrCopyWorktreeUnimplemented is returned by [Enforcement.Check] when the
// only way to hold a policy would be the copied-worktree degrade, which no
// code path implements yet (P5-06). Only Claude ships today (D-19) and it
// has native tool controls, so nothing hits this in practice.
var ErrCopyWorktreeUnimplemented = errors.New("agents: cartridge has no tool controls and the copied-worktree fallback is not implemented")

// Enforcement is how one cartridge will hold one tool policy.
type Enforcement struct {
	// Cartridge is the cartridge's registry name.
	Cartridge string `json:"cartridge"`
	// Policy is the policy being enforced.
	Policy cartridge.ToolPolicy `json:"policy"`
	// Mode says by what means; see [EnforcementMode].
	Mode EnforcementMode `json:"mode"`
	// Controls are the adapter's own tool-control arguments, for logs and
	// `sc doctor`. Empty when the adapter takes no arguments for it.
	Controls []string `json:"controls,omitempty"`
	// Detail is a one-line human explanation.
	Detail string `json:"detail,omitempty"`
}

// Native reports whether the adapter restricts the session itself.
func (e Enforcement) Native() bool { return e.Mode == EnforceNative }

// Check returns [ErrCopyWorktreeUnimplemented] when the enforcement relies
// on the unimplemented copied-worktree degrade, and nil otherwise. Callers
// that are about to fire a session should call it and refuse to fire on an
// error rather than run an unrestricted agent.
func (e Enforcement) Check() error {
	if e.Mode == EnforceCopyWorktree {
		return fmt.Errorf("%w: cartridge %q cannot enforce %s", ErrCopyWorktreeUnimplemented, e.Cartridge, e.Policy)
	}
	return nil
}

// String renders the enforcement for a log line.
func (e Enforcement) String() string {
	s := fmt.Sprintf("%s %s: %s", e.Cartridge, e.Policy, e.Mode)
	if len(e.Controls) > 0 {
		s += " (" + strings.Join(e.Controls, " ") + ")"
	}
	return s
}

// ToolPolicyEnforcer is the per-adapter method the mapping hides behind. An
// adapter that can restrict its own sessions implements it and describes
// what it will do; anything else falls back to the copied worktree. The
// Claude adapter predates the interface and is recognised by type, so new
// adapters are the ones that implement this.
type ToolPolicyEnforcer interface {
	// ToolPolicyEnforcement reports how the adapter will hold p. Returning
	// an error means the adapter refuses the policy outright.
	ToolPolicyEnforcement(p cartridge.ToolPolicy) (Enforcement, error)
}

// EnforcementFunc reports how the cartridge registered under name holds p.
type EnforcementFunc func(name string, p cartridge.ToolPolicy) (Enforcement, error)

var (
	enforcementsMu sync.RWMutex
	enforcements   = map[string]EnforcementFunc{
		// The mock cartridge (internal/cartridge/mock) enforces the policy
		// itself: it refuses write steps under read_only and read_exec and
		// command steps under read_only. It is matched by registry name so
		// production code does not import the test adapter; a mock built
		// under another name can register itself with
		// [RegisterEnforcement].
		"mock": func(name string, p cartridge.ToolPolicy) (Enforcement, error) {
			return Enforcement{
				Cartridge: name, Policy: p, Mode: EnforceNative,
				Detail: "the mock refuses forbidden steps in-process",
			}, nil
		},
	}
)

// RegisterEnforcement records how the cartridge registered under name holds
// a tool policy, for adapters that cannot implement [ToolPolicyEnforcer]
// (for example a mock built under a test-specific name). It fails on an
// empty name, a nil function or a duplicate registration.
func RegisterEnforcement(name string, fn EnforcementFunc) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("agents: cannot register an enforcement for an empty cartridge name")
	}
	if fn == nil {
		return fmt.Errorf("agents: cannot register a nil enforcement for %q", name)
	}
	enforcementsMu.Lock()
	defer enforcementsMu.Unlock()
	if _, dup := enforcements[name]; dup {
		return fmt.Errorf("agents: an enforcement for %q is already registered", name)
	}
	enforcements[name] = fn
	return nil
}

// lookupEnforcement returns the registered function for name.
func lookupEnforcement(name string) (EnforcementFunc, bool) {
	enforcementsMu.RLock()
	defer enforcementsMu.RUnlock()
	fn, ok := enforcements[name]
	return fn, ok
}

// ClaudeEnforcement is the Claude Code mapping: every policy is native,
// carried by the flags claude.ToolFlags builds (--permission-mode dontAsk
// plus the allowed and disallowed tool lists verified against Claude Code
// 2.1.258). read_only keeps a restricted Bash allowlist because that CLI
// offers Glob and Grep only when Bash is disallowed (P5-09); read_exec adds
// Bash and still denies the file-editing tools; read_write allows them.
func ClaudeEnforcement(name string, p cartridge.ToolPolicy) (Enforcement, error) {
	if !p.Valid() {
		return Enforcement{}, fmt.Errorf("%w: tool policy %q", domain.ErrUnknownValue, string(p))
	}
	if name == "" {
		name = claude.Name
	}
	detail := map[cartridge.ToolPolicy]string{
		cartridge.ToolReadOnly:  "file-editing tools removed; Bash restricted to a read-only allowlist",
		cartridge.ToolReadExec:  "file-editing tools removed; Bash allowed for tests and builds",
		cartridge.ToolReadWrite: "Bash and the file-editing tools allowed",
	}[p]
	return Enforcement{
		Cartridge: name, Policy: p, Mode: EnforceNative,
		Controls: claude.ToolFlags(p),
		Detail:   detail,
	}, nil
}

// EnforcementFor reports how c will hold p, resolving in this order: the
// adapter's own [ToolPolicyEnforcer] method, the Claude adapter's mapping
// (matched by type), an enforcement registered under the cartridge's name
// (see [RegisterEnforcement]), and finally the fallback for an adapter with
// no tool controls: [EnforceNone] for read_write, which restricts nothing,
// and [EnforceCopyWorktree] for read_only and read_exec. Call
// [Enforcement.Check] on the result before firing.
func EnforcementFor(c cartridge.Cartridge, p cartridge.ToolPolicy) (Enforcement, error) {
	if c == nil {
		return Enforcement{}, errors.New("agents: cannot resolve a tool policy for a nil cartridge")
	}
	if !p.Valid() {
		return Enforcement{}, fmt.Errorf("%w: tool policy %q is not one of %s", domain.ErrUnknownValue, string(p), joinPolicies())
	}
	name := c.Name()
	if e, ok := c.(ToolPolicyEnforcer); ok {
		en, err := e.ToolPolicyEnforcement(p)
		if err != nil {
			return Enforcement{}, fmt.Errorf("agents: cartridge %q refused tool policy %s: %w", name, p, err)
		}
		if en.Cartridge == "" {
			en.Cartridge = name
		}
		if en.Policy == "" {
			en.Policy = p
		}
		if en.Mode == "" {
			return Enforcement{}, fmt.Errorf("agents: cartridge %q reported no enforcement mode for %s", name, p)
		}
		return en, nil
	}
	if _, ok := c.(*claude.Adapter); ok {
		return ClaudeEnforcement(name, p)
	}
	if fn, ok := lookupEnforcement(name); ok {
		en, err := fn(name, p)
		if err != nil {
			return Enforcement{}, fmt.Errorf("agents: cartridge %q refused tool policy %s: %w", name, p, err)
		}
		return en, nil
	}
	if p == cartridge.ToolReadWrite {
		return Enforcement{
			Cartridge: name, Policy: p, Mode: EnforceNone,
			Detail: "read_write restricts nothing, so no tool controls are needed",
		}, nil
	}
	return Enforcement{
		Cartridge: name, Policy: p, Mode: EnforceCopyWorktree,
		Detail: "no tool controls: the firing would need a throwaway copy of the worktree (P5-06, not implemented)",
	}, nil
}

// EnforcementForFiring is [EnforcementFor] for a role and task: it picks the
// policy with [Policy] and resolves how the cartridge will hold it.
func EnforcementForFiring(c cartridge.Cartridge, r domain.Role, t events.Task) (Enforcement, error) {
	p, err := Policy(r, t)
	if err != nil {
		return Enforcement{}, err
	}
	return EnforcementFor(c, p)
}

func joinPolicies() string {
	ps := cartridge.ToolPolicies()
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = p.String()
	}
	return strings.Join(names, ", ")
}
