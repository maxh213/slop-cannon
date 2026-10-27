package agents

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
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
//     differ, so the mapping lives behind a per-adapter method
//     ([ToolPolicyEnforcer]) and is resolved by [EnforcementFor]. Claude
//     Code translates the policy into its own tool controls
//     (--permission-mode dontAsk plus allowed/disallowed tool lists) in its
//     own package (P5-11) and the mock honours the policy in-process. An
//     adapter with no tool controls at all cannot be trusted with read_only
//     or read_exec, so it resolves to [EnforceCopyWorktree]: the firing may
//     only happen in a throwaway copy of the worktree, which the
//     orchestrator's supervisor makes (P6-20) and reports back as
//     [EnforceCopiedWorktree]. A caller that cannot make that copy gets
//     [ErrCopyWorktreeUnimplemented] from [Enforcement.Check] rather than an
//     unrestricted agent.

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
	events.TaskPlanReview:       {domain.RolePO, cartridge.ToolReadOnly},
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
	// copy of the worktree — and nothing has made one. [Enforcement.Check]
	// refuses it with [ErrCopyWorktreeUnimplemented] so no unrestricted
	// agent is ever fired by accident.
	EnforceCopyWorktree EnforcementMode = "copy_worktree"
	// EnforceCopiedWorktree is that same degrade once it is in force: the
	// orchestrator's agent supervisor has copied the worktree, points the
	// firing at the copy and throws it away afterwards (P6-20), so whatever
	// the adapter lets the model do cannot reach the real tree.
	// [Enforcement.Check] passes.
	EnforceCopiedWorktree EnforcementMode = "copied_worktree"
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
// Claude adapter implements it in internal/cartridge/claude (P5-11), which
// is why this package does not import any adapter.
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

// EnforcementFor reports how c will hold p, resolving in this order: the
// adapter's own [ToolPolicyEnforcer] method (Claude Code implements it,
// P5-11), an enforcement registered under the cartridge's name
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

// ---------------------------------------------------------------------------
// The git guard (P9-22)
// ---------------------------------------------------------------------------
//
// A read_write firing gets Bash with no deny list, and it runs in a linked
// worktree of the *target* repository, so `git push origin HEAD:main`,
// `git checkout main` or `git clean -fdx` from an agent would reach the
// user's own branches — the only guard used to be a sentence in the
// engineer's prompt. The git guard closes that: a `git` script is written
// into the same per-agent directory the read-only bd shim lives in
// (beads.ShimDirFor, already first on the firing's PATH), so every git an
// agent runs goes through it. It allows the verbs the role prompts actually
// ask for — status, diff, log, add, commit, rebase --continue — and refuses
// the ones that can reach outside the agent's own branch, with the same
// argv vocabulary internal/gitx's safety invariant test judges deliveries
// by.
//
// Like the bd shim it is an allow list, for the same reason: a deny list
// closes the holes someone thought of, and git grows verbs. And like the bd
// shim it is a guard rail, not a sandbox — an agent that runs
// /usr/bin/git by absolute path bypasses it, which is why the invariant
// tests (P9-02) still watch what a run does to the repository.

// GitGuardHeader is the second line of every generated guard. IsGitGuard
// recognises a file by it, and RealGit skips such files.
const GitGuardHeader = "# slop-cannon git guard (P9-22). Do not edit."

// GitGuardBinary is the name the guard is installed under: it shadows the
// real git on the agent's PATH.
const GitGuardBinary = "git"

// GitGuardExitCode is the status the guard exits with when it refuses,
// which is git's own status for a fatal error.
const GitGuardExitCode = 128

// GitGuardAllowed is the allow list of git subcommands the guard lets
// through. It holds the reads every role needs to explore a repository and
// the writes the role prompts ask for — `git add`, `git commit -am …`,
// `git rebase --continue` — and nothing that writes a ref other than the
// branch the worktree has checked out.
//
// Deliberately absent, each because it can reach past the agent's own
// branch: push, pull and fetch (they move refs, and a refspec can name the
// default branch), remote, clone and submodule (they change where the
// repository points, or fetch into it), checkout, switch and worktree (they
// change which branch this tree is on, or make a tree elsewhere), branch,
// tag, notes, update-ref, symbolic-ref and replace (ref writes that need no
// checkout), clean (it deletes untracked work), config (it can rewrite the
// remote or the hooks path), and the repository-wide rewriters — init, gc,
// prune, repack, reflog, fsck, filter-branch. `git branch` is the one
// exception: reading the branch list is useful and harmless, so it is
// allowed unless one of GitGuardBranchWriters is among its arguments.
var GitGuardAllowed = []string{
	"add", "annotate", "apply", "blame",
	"cat-file", "check-attr", "check-ignore", "cherry", "cherry-pick",
	"commit", "describe", "diff", "diff-files", "diff-index", "diff-tree",
	"grep", "help", "log", "ls-files", "ls-tree",
	"merge", "merge-base", "mv", "name-rev",
	"rebase", "reset", "restore", "rev-list", "rev-parse", "revert", "rm",
	"shortlog", "show", "show-branch", "stash", "status", "stripspace",
	"var", "verify-commit", "whatchanged",
}

// GitGuardBranchWriters are the arguments that turn `git branch` from a
// listing into a ref write: creating, deleting, renaming, copying or
// force-moving a branch, the default branch included. Any of them makes the
// guard refuse the call.
var GitGuardBranchWriters = []string{
	"-d", "-D", "--delete",
	"-m", "-M", "--move",
	"-c", "-C", "--copy",
	"-f", "--force",
	"-u", "--set-upstream-to", "--set-upstream", "--unset-upstream",
	"--edit-description",
}

// GitGuardGlobalWriters are git's own options, valid before or after the
// subcommand, that would point it at another repository, another working
// tree or another configuration — `git -C /repo checkout main`,
// `git --git-dir=… update-ref …`, `git -c core.hooksPath=… commit`. The
// guard refuses a call carrying any of them wherever it appears, because
// none of them can be part of doing the work in the agent's own worktree.
var GitGuardGlobalWriters = []string{
	"-C", "--git-dir", "--work-tree", "--namespace", "--exec-path",
	"-c", "--config-env",
}

// GitGuardHint is the second line the guard prints when it refuses, telling
// the agent what the git on its PATH is and what it may do instead. Like
// git's own hints it starts with "hint:", so the first ("fatal: …") line
// stays the message.
const GitGuardHint = "hint: the git on your PATH is slop-cannon's guard; it allows you to inspect this " +
	"worktree and to commit in it (git status, git diff, git log, git add, git commit, git rebase --continue), " +
	"and refuses anything that could move a branch outside it. The cannon pushes and opens the pull request for you."

// GitGuardDeniedMessage is the guard's refusal for one subcommand, in git's
// own "fatal: …" shape.
func GitGuardDeniedMessage(sub string) string {
	return fmt.Sprintf("fatal: git %s is not allowed from an agent session", sub)
}

// GitGuardOptionMessage is the guard's refusal for one global option.
func GitGuardOptionMessage(opt string) string {
	return fmt.Sprintf("fatal: the git option %s is not allowed from an agent session", opt)
}

// GitGuardOutsideMessage is the guard's refusal for a git run from outside
// the agent's worktree — `cd /repo && git commit` would otherwise land on
// whatever branch the user has checked out there.
func GitGuardOutsideMessage(wt string) string {
	return fmt.Sprintf("fatal: git may only be run inside %s from an agent session", wt)
}

// GitGuard is one installed guard.
type GitGuard struct {
	// Dir is the directory holding the script; put it first on the agent's
	// PATH with Apply.
	Dir string `json:"dir"`
	// Path is the script itself, Dir/git.
	Path string `json:"path"`
	// Real is the absolute path of the genuine git the script execs.
	Real string `json:"real"`
	// Worktree, when set, is the only directory the guard lets git run in.
	Worktree string `json:"worktree,omitempty"`
}

// GitGuardOptions configure WriteGitGuard.
type GitGuardOptions struct {
	// Real is the genuine git to exec; empty means RealGit().
	Real string
	// Worktree, when set, confines the guard: a git run from anywhere
	// outside that directory is refused, so an agent cannot step into the
	// repository's own working tree and commit there.
	Worktree string
}

// WriteGitGuard installs the guard script at dir/git, replacing any earlier
// one atomically so a running agent never reads a half-written file. dir is
// normally the per-agent shim directory the read-only bd shim already uses
// (beads.ShimDirFor), which is first on the firing's PATH.
func WriteGitGuard(dir string, opts GitGuardOptions) (GitGuard, error) {
	if runtime.GOOS == "windows" {
		return GitGuard{}, fmt.Errorf("agents: the git guard needs a POSIX shell, unsupported on %s", runtime.GOOS)
	}
	if dir == "" {
		return GitGuard{}, errors.New("agents: git guard directory is empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return GitGuard{}, fmt.Errorf("agents: resolving the git guard directory: %w", err)
	}
	real := opts.Real
	if real == "" {
		real, err = RealGit()
		if err != nil {
			return GitGuard{}, err
		}
	}
	if !filepath.IsAbs(real) {
		real, err = filepath.Abs(real)
		if err != nil {
			return GitGuard{}, fmt.Errorf("agents: resolving the real git path: %w", err)
		}
	}
	if IsGitGuard(real) {
		return GitGuard{}, fmt.Errorf("agents: %s is itself a git guard, refusing to chain guards", real)
	}
	wt := opts.Worktree
	if wt != "" {
		if !filepath.IsAbs(wt) {
			return GitGuard{}, fmt.Errorf("agents: git guard worktree %q is not absolute", wt)
		}
		wt = filepath.Clean(wt)
		if resolved, err := filepath.EvalSymlinks(wt); err == nil {
			wt = resolved
		}
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return GitGuard{}, fmt.Errorf("agents: creating the git guard directory: %w", err)
	}
	g := GitGuard{Dir: abs, Path: filepath.Join(abs, GitGuardBinary), Real: real, Worktree: wt}
	tmp, err := os.CreateTemp(abs, "."+GitGuardBinary+".*")
	if err != nil {
		return GitGuard{}, fmt.Errorf("agents: creating the git guard: %w", err)
	}
	tmpName := tmp.Name()
	_, werr := tmp.WriteString(GitGuardScript(real, wt))
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmpName)
		return GitGuard{}, fmt.Errorf("agents: writing the git guard: %w", errors.Join(werr, cerr))
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		_ = os.Remove(tmpName)
		return GitGuard{}, fmt.Errorf("agents: making the git guard executable: %w", err)
	}
	if err := os.Rename(tmpName, g.Path); err != nil {
		_ = os.Remove(tmpName)
		return GitGuard{}, fmt.Errorf("agents: installing the git guard: %w", err)
	}
	return g, nil
}

// InstallGitGuardForWorktree writes the guard for the agent worktree at wt
// into the per-agent directory the read-only bd shim already uses
// (beads.ShimDirFor(wt)), so one PATH entry carries both and the guard is
// thrown away with the run. dir overrides that placement for a caller with
// its own layout.
//
// The worktree is always baked in: a firing's git may only run inside its
// own worktree, so an agent cannot step into the repository's own working
// tree — where the default branch is the one checked out — and commit
// there.
func InstallGitGuardForWorktree(wt, dir string) (GitGuard, error) {
	if wt == "" {
		return GitGuard{}, errors.New("agents: InstallGitGuardForWorktree: empty worktree path")
	}
	if !filepath.IsAbs(wt) {
		return GitGuard{}, fmt.Errorf("agents: InstallGitGuardForWorktree: worktree path %q is not absolute", wt)
	}
	if dir == "" {
		var err error
		dir, err = beads.ShimDirFor(wt)
		if err != nil {
			return GitGuard{}, err
		}
	}
	return WriteGitGuard(dir, GitGuardOptions{Worktree: wt})
}

// Apply puts the guard's directory first on PATH in env (a
// cartridge.RunSpec.Env map), prepending to a PATH already in env rather
// than to the parent's. It is idempotent and does nothing on a nil map or
// an uninstalled guard.
func (g GitGuard) Apply(env map[string]string) {
	if env == nil || g.Dir == "" {
		return
	}
	if current, ok := env["PATH"]; ok {
		env["PATH"] = beads.PrependPath(g.Dir, current)
		return
	}
	env["PATH"] = beads.PrependPath(g.Dir, os.Getenv("PATH"))
}

// IsGitGuard reports whether path is a guard generated by WriteGitGuard
// (its second line is GitGuardHeader). Missing or unreadable files are not.
func IsGitGuard(path string) bool {
	f, err := os.Open(path) //nolint:gosec // the caller names a file on its own PATH
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 256)
	n, _ := f.Read(buf)
	lines := strings.SplitN(string(buf[:n]), "\n", 3)
	return len(lines) >= 2 && lines[1] == GitGuardHeader
}

// RealGit resolves the genuine git binary on PATH, skipping any guard, so a
// caller whose own PATH already carries one still finds the real tool. The
// error wraps exec.ErrNotFound.
func RealGit() (string, error) {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, GitGuardBinary)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 || IsGitGuard(candidate) {
			continue
		}
		if !filepath.IsAbs(candidate) {
			if abs, err := filepath.Abs(candidate); err == nil {
				candidate = abs
			}
		}
		return candidate, nil
	}
	return "", fmt.Errorf("agents: %s: %w (no real binary on PATH)", GitGuardBinary, exec.ErrNotFound)
}

// GitGuardScript renders the guard: a POSIX shell script that refuses the
// global options of GitGuardGlobalWriters wherever they appear, refuses any
// subcommand outside GitGuardAllowed, refuses a `git branch` carrying one of
// GitGuardBranchWriters, refuses a call made from outside wt when wt is
// non-empty, and execs real for everything else.
func GitGuardScript(real, wt string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString(GitGuardHeader + "\n")
	b.WriteString("# Agents run git through this guard. It allows the verbs the role prompts\n")
	b.WriteString("# ask for and refuses anything that could move a branch outside the\n")
	b.WriteString("# agent's own worktree; slop-cannon pushes and delivers the work itself.\n")
	if wt != "" {
		// pwd -P resolves symlinks, as filepath.EvalSymlinks did for wt, so
		// the two spellings of the same directory compare equal.
		b.WriteString("_sc_wt=" + shQuote(wt) + "\n")
		b.WriteString("_sc_pwd=$(pwd -P 2>/dev/null || pwd)\n")
		b.WriteString("case \"$_sc_pwd\" in\n")
		b.WriteString("  \"$_sc_wt\"|\"$_sc_wt\"/*) ;;\n")
		b.WriteString("  *)\n")
		b.WriteString("    echo " + shQuote(GitGuardOutsideMessage(wt)) + " >&2\n")
		b.WriteString("    echo " + shQuote(GitGuardHint) + " >&2\n")
		b.WriteString("    exit " + strconv.Itoa(GitGuardExitCode) + " ;;\n")
		b.WriteString("esac\n")
	}
	// One pass over the whole argument list: the first operand is the
	// subcommand, and a global writer anywhere refuses the call. Nothing
	// stops at "--", because git's global options are only ever read before
	// the subcommand and a "--" after it cannot make one appear.
	b.WriteString("_sc_sub=''\n_sc_bad=''\n_sc_branch=0\n")
	b.WriteString("for _sc_arg in \"$@\"; do\n")
	b.WriteString("  case \"$_sc_arg\" in\n")
	b.WriteString("    " + shQuoteJoin(GitGuardGlobalWriters, true) + ")\n")
	b.WriteString("      if [ -z \"$_sc_bad\" ]; then _sc_bad=$_sc_arg; fi\n")
	b.WriteString("      continue ;;\n")
	b.WriteString("    " + shQuoteJoin(GitGuardBranchWriters, false) + ")\n")
	b.WriteString("      _sc_branch=1\n")
	b.WriteString("      continue ;;\n")
	b.WriteString("    -*|'') continue ;;\n")
	b.WriteString("  esac\n")
	b.WriteString("  if [ -z \"$_sc_sub\" ]; then _sc_sub=$_sc_arg; fi\n")
	b.WriteString("done\n")
	b.WriteString("if [ -n \"$_sc_bad\" ]; then\n")
	b.WriteString("  echo " + shInterp(GitGuardOptionMessage(gitGuardWord), "$_sc_bad") + " >&2\n")
	b.WriteString("  echo " + shQuote(GitGuardHint) + " >&2\n")
	b.WriteString("  exit " + strconv.Itoa(GitGuardExitCode) + "\nfi\n")
	// No subcommand at all is `git`, `git --version`, `git --help`: it
	// prints and touches nothing.
	b.WriteString("_sc_ok=0\n")
	b.WriteString("if [ -z \"$_sc_sub\" ]; then _sc_ok=1; fi\n")
	b.WriteString("case \"$_sc_sub\" in\n  " + shQuoteJoin(GitGuardAllowed, false) + ") _sc_ok=1 ;;\nesac\n")
	b.WriteString("case \"$_sc_sub\" in\n  branch) if [ \"$_sc_branch\" = 0 ]; then _sc_ok=1; fi ;;\nesac\n")
	b.WriteString("if [ \"$_sc_ok\" = 0 ]; then\n")
	b.WriteString("  echo " + shInterp(GitGuardDeniedMessage(gitGuardWord), "$_sc_sub") + " >&2\n")
	b.WriteString("  echo " + shQuote(GitGuardHint) + " >&2\n")
	b.WriteString("  exit " + strconv.Itoa(GitGuardExitCode) + "\nfi\n")
	b.WriteString("exec " + shQuote(real) + " \"$@\"\n")
	return b.String()
}

// gitGuardWord is the placeholder the script's own copy of a refusal is
// built around: the message is rendered once with it, then split there and
// the shell variable holding the offending word put in its place, so the
// exported message functions and the generated script can never disagree.
const gitGuardWord = "\x00"

// shInterp renders msg — which must contain exactly one gitGuardWord — as a
// double-quoted shell word with the shell expression expr in its place.
func shInterp(msg, expr string) string {
	pre, post, ok := strings.Cut(msg, gitGuardWord)
	if !ok {
		return shQuote(msg)
	}
	return `"` + shEscape(pre) + expr + shEscape(post) + `"`
}

// shEscape escapes the four characters a double-quoted shell word still
// expands.
func shEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`")
	return r.Replace(s)
}

// shQuote renders s as a single shell word.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// shQuoteJoin renders words as the alternatives of a shell `case` pattern.
// withValues also matches the "--opt=value" spelling of each word.
func shQuoteJoin(words []string, withValues bool) string {
	out := make([]string, 0, len(words)*2)
	for _, w := range words {
		out = append(out, shQuote(w))
		if withValues {
			out = append(out, shQuote(w+"=")+"*")
		}
	}
	return strings.Join(out, "|")
}
