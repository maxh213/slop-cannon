package kilo

import (
	"os"
	"sort"
	"strings"

	"slop-cannon/internal/cartridge"
)

const (
	versionFlag       = "--version"
	minBalanceEnv     = "SC_KILO_MIN_BALANCE_USD"
	configContentEnv  = "KILO_CONFIG_CONTENT"
	systemPromptSplit = "\n\n---\n\n"
	dandelionModelID  = "deepseek/deepseek-v4.1-flash-1017"
)

// Args builds one Kilo run. Configured arguments, including --variant, come
// first so the mandatory invocation controls always take precedence.
func (a *Adapter) Args(spec cartridge.RunSpec) []string {
	args := []string{"run"}
	args = append(args, a.cfg.Args...)
	args = append(args, "--pure", "--auto", "--format", "json", "--agent", "sc", "--dir", spec.Workdir)
	if model := a.model(spec); model != "" {
		args = append(args, "--model", cliModel(model))
	}
	return args
}

// cliModel qualifies the Dandelion gateway model ID for Kilo's provider/model
// argument without mutating the route model or rewriting direct custom models.
func cliModel(model string) string {
	if model == dandelionModelID {
		return "kilo/" + model
	}
	return model
}

// Stdin combines the additional instructions and user prompt without putting
// either in argv. Kilo has no separate system-prompt flag for a run.
func Stdin(spec cartridge.RunSpec) string {
	if strings.TrimSpace(spec.SystemPrompt) == "" {
		return spec.Prompt
	}
	return spec.SystemPrompt + systemPromptSplit + spec.Prompt
}

func (a *Adapter) model(spec cartridge.RunSpec) string {
	if spec.Model != "" {
		return spec.Model
	}
	return a.cfg.Model
}

// InheritEnv extends base with Kilo endpoint and authentication environment.
// Mandatory values are overwritten and the Dandelion floor is removed later.
func InheritEnv(base []string) []string {
	set := make(map[string]bool, len(base))
	for _, name := range base {
		set[name] = true
	}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "KILO_") {
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

func (a *Adapter) childEnv(specEnv map[string]string, configContent string) map[string]string {
	env := make(map[string]string, len(a.cfg.Env)+len(specEnv)+4)
	for key, value := range a.cfg.Env {
		env[key] = value
	}
	for key, value := range specEnv {
		env[key] = value
	}
	delete(env, minBalanceEnv)
	env["KILO_NO_DAEMON"] = "1"
	env["KILO_DISABLE_PROJECT_CONFIG"] = "1"
	env["KILO_DISABLE_AUTOUPDATE"] = "1"
	if configContent != "" {
		env[configContentEnv] = configContent
	} else {
		delete(env, configContentEnv)
	}
	return env
}
