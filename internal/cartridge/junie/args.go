package junie

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"slop-cannon/internal/cartridge"
)

const (
	flagProject           = "--project"
	flagTask              = "--task"
	flagOutputFormat      = "--output-format"
	outputFormatJSON      = "json"
	flagModel             = "--model"
	flagEffort            = "--effort"
	flagSystemPrompt      = "--system-prompt"
	flagSkipUpdateCheck   = "--skip-update-check"
	flagStatistics        = "--share-anonymous-statistics"
	flagConfigDefaults    = "--config-default-locations"
	flagMCPDefaults       = "--mcp-default-locations"
	flagModelDefaults     = "--model-default-locations"
	flagSkillDefaults     = "--skill-default-locations"
	flagCommandDefaults   = "--command-default-location"
	flagAgentDefaults     = "--agent-default-location"
	flagExtensionsDefault = "--extensions-default-location"
	versionFlag           = "--version"
)

// Efforts are the reasoning efforts accepted by Junie CLI's --effort flag.
var Efforts = []string{"low", "medium", "high"}

var protectedArgs = map[string]bool{
	flagProject: true, "-p": true, flagTask: true,
	flagOutputFormat: true, "--json-output-file": true, "--input-format": true,
	flagModel: true, flagSystemPrompt: true,
	flagSkipUpdateCheck: true, flagStatistics: true,
	flagConfigDefaults: true, "--config-location": true,
	flagMCPDefaults: true, "--mcp-location": true,
	flagModelDefaults: true, "--model-location": true,
	flagSkillDefaults: true, "--skill-location": true,
	flagCommandDefaults: true, "--command-location": true,
	flagAgentDefaults: true, "--agent-location": true,
	flagExtensionsDefault: true,
	"--plan":              true, "--prompt": true, "--brave": true, "--acp": true,
	"--resume": true, "--session-id": true, "--fork-session": true,
	"--merge": true, "--rebase": true, "--review": true, "--goal": true,
	"--gateway-stop": true, "--gateway-status": true, "--gateway": true,
	"--verbose": true, "--agent-mode": true,
	"--demo": true, "--demo-host": true, "--update": true,
}

var booleanArgs = map[string]bool{
	flagSkipUpdateCheck: true, "--plan": true, "--brave": true,
	"--resume": true, "--fork-session": true, "--review": true, "--goal": true,
	"--gateway-stop": true, "--gateway-status": true, "--gateway": true,
	"--verbose": true, "--demo": true, "--demo-host": true, "--update": true,
}

// Args builds one non-interactive invocation. extensionDir is an empty,
// process-private directory that keeps installed extensions out of the run.
// Configured arguments cannot replace the protocol, workspace, prompt, model,
// discovery restrictions, or lifecycle controls; --effort remains available.
// Junie 26.9.22 has no per-invocation turn or step limit, so MaxTurns is ignored.
func (a *Adapter) Args(spec cartridge.RunSpec, extensionDir string) []string {
	return a.args(spec, extensionDir, spec.Prompt)
}

func (a *Adapter) args(spec cartridge.RunSpec, extensionDir, task string) []string {
	args := []string{
		flagProject, spec.Workdir,
		flagOutputFormat, outputFormatJSON,
		flagSkipUpdateCheck,
		flagStatistics, "false",
		flagConfigDefaults, "false",
		flagMCPDefaults, "false",
		flagModelDefaults, "false",
		flagSkillDefaults, "false",
		flagCommandDefaults, "false",
		flagAgentDefaults, "false",
		flagExtensionsDefault, extensionDir,
	}
	if m := a.model(spec); m != "" {
		args = append(args, flagModel, m)
	}
	if spec.SystemPrompt != "" {
		args = append(args, flagSystemPrompt, spec.SystemPrompt)
	}
	args = append(args, safeExtraArgs(a.cfg.Args)...)
	return append(args, flagTask, task)
}

func safeExtraArgs(configured []string) []string {
	out := make([]string, 0, len(configured))
	for i := 0; i < len(configured); i++ {
		arg := configured[i]
		if arg == "--" {
			break
		}
		name, _, hasValue := strings.Cut(arg, "=")
		if protectedArgs[name] {
			if !hasValue && !booleanArgs[name] && i+1 < len(configured) {
				i++
			}
			continue
		}
		out = append(out, arg)
	}
	return out
}

func validateEfforts(args []string) error {
	for i := 0; i < len(args); i++ {
		name, value, hasValue := strings.Cut(args[i], "=")
		if name != flagEffort {
			continue
		}
		if !hasValue {
			if i+1 >= len(args) {
				return fmt.Errorf("junie: --effort requires a value")
			}
			i++
			value = args[i]
		}
		if !slices.Contains(Efforts, value) {
			return fmt.Errorf("junie: unsupported effort %q (want low, medium, or high)", value)
		}
	}
	return nil
}

func (a *Adapter) model(spec cartridge.RunSpec) string {
	if spec.Model != "" {
		return spec.Model
	}
	return a.cfg.Model
}

var envPrefixes = []string{"JUNIE_"}

// InheritEnv adds Junie authentication, provider, endpoint, and configuration
// variables to base. Explicit invocation flags take priority where set.
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
