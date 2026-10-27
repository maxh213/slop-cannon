package kimi

import (
	"os"
	"sort"
	"strings"

	"slop-cannon/internal/cartridge"
)

const (
	flagPrompt         = "--prompt"
	flagOutputFormat   = "--output-format"
	outputFormatStream = "stream-json"
	flagModel          = "--model"
	flagAgentFile      = "--agent-file"
	envMaxSteps        = "KIMI_LOOP_MAX_STEPS_PER_TURN"
	versionFlag        = "--version"

	// maxPromptArgBytes stays below Linux's 128 KiB per-argument ceiling.
	// Larger inputs use the private prompt file through Kimi's native Read tool.
	maxPromptArgBytes = 96 * 1024
)

// Args builds Kimi Code 2.x print-mode arguments. Workdir comes from
// proc.Spec.Dir and MaxTurns from KIMI_LOOP_MAX_STEPS_PER_TURN; 2.3.1 has no
// CLI flags for either. Configured extra args come last.
func (a *Adapter) Args(spec cartridge.RunSpec, profilePath, input string) []string {
	args := []string{flagPrompt, input, flagOutputFormat, outputFormatStream, flagAgentFile, profilePath}
	if m := a.model(spec); m != "" {
		args = append(args, flagModel, m)
	}
	return append(args, a.cfg.Args...)
}

// Input combines the additional instructions and user prompt because Kimi
// Code 2.x has no separate system-prompt flag. The agent profile retains the
// built-in system prompt through ${base_prompt}.
func Input(spec cartridge.RunSpec) string {
	if strings.TrimSpace(spec.SystemPrompt) == "" {
		return spec.Prompt
	}
	return spec.SystemPrompt + "\n\n---\n\n" + spec.Prompt
}

func (a *Adapter) model(spec cartridge.RunSpec) string {
	if spec.Model != "" {
		return spec.Model
	}
	return a.cfg.Model
}

// DefaultEnv disables update and telemetry side effects during unattended
// firings. Configuration and RunSpec values take precedence.
var DefaultEnv = map[string]string{
	"KIMI_CODE_NO_AUTO_UPDATE": "1",
	"KIMI_DISABLE_TELEMETRY":   "1",
}

// InheritEnv extends base with Kimi configuration, model and endpoint
// variables present in the parent. In particular KIMI_MODEL_THINKING_EFFORT
// reaches routed firings. The result is sorted and duplicate-free.
func InheritEnv(base []string) []string {
	set := make(map[string]bool, len(base))
	for _, name := range base {
		set[name] = true
	}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "KIMI_") {
			set[name] = true
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
