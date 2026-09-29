package cursor

import (
	"os"
	"sort"
	"strings"

	"slop-cannon/internal/cartridge"
)

// Flags of the Cursor Agent CLI the adapter relies on. Every one was
// verified against "cursor-agent --help" of the version in VerifiedVersion;
// the list is mirrored in docs/cartridges.md.
const (
	flagPrint          = "-p"
	flagOutputFormat   = "--output-format"
	outputFormatStream = "stream-json"
	flagForce          = "--force"
	flagTrust          = "--trust"
	flagSandbox        = "--sandbox"
	sandboxDisabled    = "disabled"
	flagWorkspace      = "--workspace"
	flagModel          = "--model"
	versionFlag        = "--version"
	statusSubcommand   = "status"
)

// SystemPromptSeparator divides the system prompt from the prompt on stdin.
// The CLI has no flag for a system prompt, so both travel as one message.
const SystemPromptSeparator = "\n\n---\n\n"

// Args builds the full argument list for one firing: print mode with
// stream-json output, --force so no tool call waits for an approval that
// never comes, --trust so an unknown workspace does not stop at a prompt,
// --sandbox disabled so the check command can reach node_modules, browsers
// and the network, the workspace, the model (spec.Model, else the configured
// one), and finally the configured extra args so a user's flag wins over the
// adapter's.
//
// Nothing in the spec restricts tools: the CLI offers no per-invocation tool
// controls, and the supervisor holds read_only and read_exec by firing in a
// throwaway copy of the worktree (P6-20). MaxTurns has no flag either and is
// ignored. RunSpec.Web adds nothing: the CLI's own web tool is always on.
func (a *Adapter) Args(spec cartridge.RunSpec) []string {
	args := []string{
		flagPrint, flagOutputFormat, outputFormatStream,
		flagForce, flagTrust, flagSandbox, sandboxDisabled,
		flagWorkspace, spec.Workdir,
	}
	if m := a.model(spec); m != "" {
		args = append(args, flagModel, m)
	}
	return append(args, a.cfg.Args...)
}

// Stdin is what a firing writes to the CLI's standard input: the system
// prompt, SystemPromptSeparator and the prompt, or the prompt alone when
// there is no system prompt.
func Stdin(spec cartridge.RunSpec) string {
	if strings.TrimSpace(spec.SystemPrompt) == "" {
		return spec.Prompt
	}
	return spec.SystemPrompt + SystemPromptSeparator + spec.Prompt
}

func (a *Adapter) model(spec cartridge.RunSpec) string {
	if spec.Model != "" {
		return spec.Model
	}
	return a.cfg.Model
}

// envPrefixes are the variables the CLI reads for authentication, endpoints
// and its own configuration (CURSOR_API_KEY, CURSOR_API_ENDPOINT,
// CURSOR_CONFIG_DIR, ...). They are passed through from sc's environment on
// top of cartridge.DefaultInheritEnv, which already carries HOME and the
// XDG directories the stored login lives under.
var envPrefixes = []string{"CURSOR_"}

// sessionMarkers are set by a running Cursor Agent session for the commands
// it runs. Passing them on would make an agent that sc fired from inside a
// Cursor session believe it is that session's child.
var sessionMarkers = map[string]bool{
	"CURSOR_AGENT":           true,
	"CURSOR_AGENT_CHAT_ID":   true,
	"CURSOR_CONVERSATION_ID": true,
	"CURSOR_AGENT_SOCKET":    true,
	"CURSOR_AGENT_WORKER_ID": true,
}

// DefaultEnv is set on every child unless the configuration or the RunSpec
// overrides it. The CLI's print mode already waits for commands it moved to
// the background and wakes the model when they finish (fixture
// background-shell.jsonl), so unlike Claude Code nothing needs switching
// off; the map exists so the configuration layering matches the Claude
// adapter's.
var DefaultEnv = map[string]string{}

// InheritEnv extends base (normally cartridge.RunSpec.EnvAllowlist) with the
// CURSOR_* variables present in sc's environment, minus the session markers.
// The result is sorted and free of duplicates.
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
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func inheritable(name string) bool {
	if sessionMarkers[name] {
		return false
	}
	for _, p := range envPrefixes {
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
