package claude

import (
	"os"
	"sort"
	"strconv"
	"strings"

	"slop-cannon/internal/cartridge"
)

// Flags of the Claude Code CLI the adapter relies on. Every one was verified
// against "claude --help" of the version in VerifiedVersion; the list is
// mirrored in docs/cartridges.md.
const (
	flagPrint            = "-p"
	flagOutputFormat     = "--output-format"
	flagVerbose          = "--verbose"
	flagModel            = "--model"
	flagAppendSystem     = "--append-system-prompt"
	flagMaxTurns         = "--max-turns"
	flagPermissionMode   = "--permission-mode"
	flagAllowedTools     = "--allowedTools"
	flagDisallowedTools  = "--disallowedTools"
	outputFormatStream   = "stream-json"
	permissionModeNoAsk  = "dontAsk"
	versionFlag          = "--version"
	authStatusSubcommand = "auth"
)

// File-editing tools removed from the session under read_only and
// read_exec. Removing a tool with --disallowedTools makes it unavailable to
// the model and to its subagents ("No such tool available", fixture
// tool-disallowed.jsonl).
var editTools = []string{"Edit", "Write", "MultiEdit", "NotebookEdit"}

// ReadOnlyShell is the Bash permission allowlist for ToolReadOnly, as
// Claude Code permission rules ("Bash(prefix:*)" matches commands starting
// with prefix). Claude Code 2.1.258 offers its Glob and Grep tools only
// when Bash is disallowed (compare the init lines of tool-disallowed.jsonl
// and shell-denied.jsonl), so with a restricted Bash a planner explores
// with these commands. Under dontAsk every other shell command is refused
// without a prompt (fixture shell-denied.jsonl).
//
// Keeping this list rather than dropping the shell altogether
// (--disallowedTools Bash,Edit,Write,MultiEdit,NotebookEdit, which restores
// Glob and Grep) is the P5-09 decision, recorded in docs/cartridges.md: the
// product owner is the only role fired under read_only and its job needs bd
// (through the read-only shim, D-21) and git log/diff/show, which Glob and
// Grep cannot do, while grep, rg and find here cover what those built-ins
// offered. The conformance suite (P2-09) checks that writes are refused
// under either mapping, so it does not decide this; adding a command here
// means adding it to the docs/cartridges.md table too.
var ReadOnlyShell = []string{
	"Bash(ls:*)", "Bash(pwd)", "Bash(tree:*)", "Bash(find:*)", "Bash(fd:*)",
	"Bash(cat:*)", "Bash(head:*)", "Bash(tail:*)", "Bash(wc:*)", "Bash(file:*)", "Bash(stat:*)",
	"Bash(grep:*)", "Bash(rg:*)", "Bash(ag:*)", "Bash(diff:*)",
	"Bash(git status:*)", "Bash(git log:*)", "Bash(git show:*)", "Bash(git diff:*)",
	"Bash(git branch:*)", "Bash(git ls-files:*)", "Bash(git blame:*)", "Bash(git rev-parse:*)",
	"Bash(bd:*)",
}

// ToolFlags maps a tool policy onto CLI arguments. Every policy runs with
// --permission-mode dontAsk, which turns anything not pre-approved into a
// silent denial instead of an interactive prompt (unattended use):
//
//   - read_only:  --allowedTools <ReadOnlyShell> --disallowedTools <editTools>
//   - read_exec:  --allowedTools Bash --disallowedTools <editTools>
//   - read_write: --allowedTools Bash,<editTools>
//
// Read is never gated. WebFetch and WebSearch are allowed only when the
// spec asks for them (RunSpec.Web, see Args); other prompting tools stay
// denied under every policy. Note that read_exec cannot stop a shell command
// from writing a file; the policy is about which tools are offered, and the
// worktree is disposable.
func ToolFlags(p cartridge.ToolPolicy) []string {
	args := []string{flagPermissionMode, permissionModeNoAsk}
	switch p {
	case cartridge.ToolReadOnly:
		args = append(args,
			flagAllowedTools, strings.Join(ReadOnlyShell, ","),
			flagDisallowedTools, strings.Join(editTools, ","))
	case cartridge.ToolReadExec:
		args = append(args,
			flagAllowedTools, "Bash",
			flagDisallowedTools, strings.Join(editTools, ","))
	case cartridge.ToolReadWrite:
		args = append(args, flagAllowedTools, strings.Join(append([]string{"Bash"}, editTools...), ","))
	}
	return args
}

// Args builds the full argument list for one firing: print mode with
// stream-json output, the model (spec.Model, else the configured one), the
// system prompt suffix, the turn cap, the tool-policy flags, and finally the
// configured extra args so a user's flag wins over the adapter's when both
// set the same option. The user prompt is not an argument: it goes to stdin
// so no length limit or option parsing applies to it.
func (a *Adapter) Args(spec cartridge.RunSpec) []string {
	args := []string{flagPrint, flagOutputFormat, outputFormatStream, flagVerbose}
	if m := a.model(spec); m != "" {
		args = append(args, flagModel, m)
	}
	if spec.SystemPrompt != "" {
		args = append(args, flagAppendSystem, spec.SystemPrompt)
	}
	if spec.MaxTurns > 0 {
		args = append(args, flagMaxTurns, strconv.Itoa(spec.MaxTurns))
	}
	args = append(args, withWebTools(ToolFlags(spec.ToolPolicy), spec.Web)...)
	args = append(args, a.cfg.Args...)
	return args
}

// WebTools are the CLI's read-only web tools a RunSpec with Web set may use.
var WebTools = []string{"WebSearch", "WebFetch"}

// withWebTools adds WebTools to the allowed tools in flags when web is set.
// They join the existing --allowedTools value rather than a second flag, so
// no CLI version can read one list as replacing the other.
func withWebTools(flags []string, web bool) []string {
	if !web {
		return flags
	}
	out := append([]string(nil), flags...)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == flagAllowedTools {
			out[i+1] += "," + strings.Join(WebTools, ",")
			return out
		}
	}
	return append(out, flagAllowedTools, strings.Join(WebTools, ","))
}

func (a *Adapter) model(spec cartridge.RunSpec) string {
	if spec.Model != "" {
		return spec.Model
	}
	return a.cfg.Model
}

// Environment variable prefixes and names the CLI reads for authentication,
// endpoints and behaviour. They are passed through from sc's environment on
// top of cartridge.DefaultInheritEnv.
var (
	envPrefixes = []string{"ANTHROPIC_", "CLAUDE_", "AWS_", "GOOGLE_"}
	envNames    = []string{
		"CLOUD_ML_REGION", "DISABLE_AUTOUPDATER", "DISABLE_TELEMETRY", "DISABLE_ERROR_REPORTING",
		"DISABLE_NON_ESSENTIAL_MODEL_CALLS", "MAX_THINKING_TOKENS",
		"BASH_DEFAULT_TIMEOUT_MS", "BASH_MAX_TIMEOUT_MS", "MCP_TIMEOUT",
	}
	// sessionMarkers are set by a running Claude Code session for its own
	// children. Passing them on would make the agent believe it is nested
	// inside (and attach to) the session sc itself runs in. They are
	// dropped even though they match envPrefixes.
	sessionMarkers = map[string]bool{
		"CLAUDECODE":                    true,
		"CLAUDE_CODE_ENTRYPOINT":        true,
		"CLAUDE_CODE_SESSION_ID":        true,
		"CLAUDE_CODE_CHILD_SESSION":     true,
		"CLAUDE_CODE_BRIDGE_SESSION_ID": true,
		"CLAUDE_CODE_EXECPATH":          true,
		"CLAUDE_CODE_SSE_PORT":          true,
		"CLAUDE_PID":                    true,
		"CLAUDE_EFFORT":                 true,
	}
	sessionMarkerPrefixes = []string{"CLAUDE_CODE_MESSAGING_"}
)

// DefaultEnv is set on every child unless the configuration or the RunSpec
// overrides it: agents must not self-update mid-run.
var DefaultEnv = map[string]string{"DISABLE_AUTOUPDATER": "1"}

// InheritEnv extends base (normally cartridge.RunSpec.EnvAllowlist) with the
// Claude-specific variables present in sc's environment: ANTHROPIC_*,
// CLAUDE_*, AWS_* and GOOGLE_* (Bedrock and Vertex users), plus a few named
// knobs, minus the markers a parent Claude Code session sets. The result is
// sorted and free of duplicates.
func InheritEnv(base []string) []string {
	set := map[string]bool{}
	for _, n := range base {
		set[n] = true
	}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if inheritable(name) {
			set[name] = true
		}
	}
	for _, n := range envNames {
		set[n] = true
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// inheritable reports whether a Claude-related variable may reach a child.
func inheritable(name string) bool {
	if IsSessionMarker(name) {
		return false
	}
	for _, p := range envPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// IsSessionMarker reports whether name is one of the variables a running
// Claude Code session sets for its children, which the adapter never passes
// on.
func IsSessionMarker(name string) bool {
	if sessionMarkers[name] {
		return true
	}
	for _, p := range sessionMarkerPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// childEnv layers DefaultEnv, the configured env and the spec env, later
// entries winning.
func (a *Adapter) childEnv(specEnv map[string]string) map[string]string {
	env := make(map[string]string, len(DefaultEnv)+len(a.cfg.Env)+len(specEnv))
	for k, v := range DefaultEnv {
		env[k] = v
	}
	for k, v := range a.cfg.Env {
		env[k] = v
	}
	for k, v := range specEnv {
		env[k] = v
	}
	return env
}
