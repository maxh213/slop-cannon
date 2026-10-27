package agy

import (
	"os"
	"sort"
	"strings"

	"slop-cannon/internal/cartridge"
)

const (
	flagPrint                = "--print"
	flagOutputFormat         = "--output-format"
	outputFormatJSON         = "json"
	flagAddDir               = "--add-dir"
	flagModel                = "--model"
	flagPrintTimeout         = "--print-timeout"
	flagMode                 = "--mode"
	modeAcceptEdits          = "accept-edits"
	modePlan                 = "plan"
	flagSkipPermissions      = "--dangerously-skip-permissions"
	flagDisableSlashCommands = "--disable-slash-commands"
	versionFlag              = "--version"
	// SystemPromptSeparator divides the two prompts in agy's single prompt value.
	SystemPromptSeparator = "\n\n---\n\n"
)

// Args builds one headless invocation. Extra configured arguments carry
// routed options such as --effort. Policy controls follow them so configuration
// cannot replace a restricted mode. --print and its prompt value are last:
// agy ignores a positional prompt and does not read it from ordinary stdin.
// MaxTurns has no agy equivalent and is intentionally not mapped.
func (a *Adapter) Args(spec cartridge.RunSpec) []string {
	return a.argsWithPrompt(spec, Prompt(spec))
}

func (a *Adapter) argsWithPrompt(spec cartridge.RunSpec, prompt string) []string {
	args := []string{flagOutputFormat, outputFormatJSON, flagAddDir, spec.Workdir}
	if m := a.model(spec); m != "" {
		args = append(args, flagModel, m)
	}
	if spec.Timeout > 0 {
		args = append(args, flagPrintTimeout, spec.Timeout.String())
	}
	args = append(args, a.cfg.Args...)
	args = append(args, ToolFlags(spec.ToolPolicy)...)
	return append(args, flagPrint, prompt)
}

// ToolFlags maps read_write to explicit editing and noninteractive permission
// handling, and read_only to the candidate plan mode. Unsupported policies fail
// closed to plan mode; Run and ToolPolicyEnforcement reject them before enablement.
func ToolFlags(p cartridge.ToolPolicy) []string {
	if p == cartridge.ToolReadWrite {
		return []string{flagMode, modeAcceptEdits, flagSkipPermissions}
	}
	return []string{flagMode, modePlan}
}

// Prompt combines the system and user prompts verbatim because agy has no
// system-prompt flag. Run passes it directly or through the large-prompt file.
func Prompt(spec cartridge.RunSpec) string {
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

var envPrefixes = []string{"AGY_", "GEMINI_", "GOOGLE_"}

// InheritEnv extends base with agy and Google authentication/configuration
// variables present in the parent. The result is sorted and deduplicated.
func InheritEnv(base []string) []string {
	set := make(map[string]bool, len(base))
	for _, name := range base {
		set[name] = true
	}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		for _, prefix := range envPrefixes {
			if strings.HasPrefix(name, prefix) {
				set[name] = true
				break
			}
		}
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (a *Adapter) childEnv(specEnv map[string]string) map[string]string {
	env := make(map[string]string, len(a.cfg.Env)+len(specEnv))
	for k, v := range a.cfg.Env {
		env[k] = v
	}
	for k, v := range specEnv {
		env[k] = v
	}
	return env
}
