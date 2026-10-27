package grok

import (
	"os"
	"sort"
	"strconv"
	"strings"

	"slop-cannon/internal/cartridge"
)

const (
	flagPromptFile      = "--prompt-file"
	flagOutputFormat    = "--output-format"
	outputFormatStream  = "streaming-json"
	flagNoAutoUpdate    = "--no-auto-update"
	flagNoLeader        = "--no-leader"
	flagNoSubagents     = "--no-subagents"
	flagCwd             = "--cwd"
	flagModel           = "--model"
	flagReasoningEffort = "--reasoning-effort"
	flagRules           = "--rules"
	flagMaxTurns        = "--max-turns"
	flagPermissionMode  = "--permission-mode"
	flagTools           = "--tools"
	flagDisallowedTools = "--disallowed-tools"
	flagAllow           = "--allow"
	flagDeny            = "--deny"
	permissionNoAsk     = "dontAsk"
	permissionBypass    = "bypassPermissions"
	versionFlag         = "--version"
)

var (
	readTools    = []string{"read_file", "grep", "list_dir"}
	terminalTool = "run_terminal_cmd"
	editTool     = "search_replace"
	subagentTool = "Agent"
	// WebTools are enabled only when RunSpec.Web requests them.
	WebTools = []string{"web_search", "web_fetch"}

	// ReadOnlyShell retains the commands SC planning needs. Grok splits
	// chained commands and requires every segment to match an allow rule;
	// dontAsk denies every unmatched segment without prompting. The space
	// before * is significant: prefix rules otherwise have no word boundary.
	ReadOnlyShell = []string{
		"Bash(ls *)", "Bash(tree *)", "Bash(find *)", "Bash(fd *)",
		"Bash(cat *)", "Bash(head *)", "Bash(tail *)", "Bash(wc *)", "Bash(file *)", "Bash(stat *)",
		"Bash(grep *)", "Bash(rg *)", "Bash(ag *)", "Bash(diff *)",
		"Bash(git status *)", "Bash(git log *)", "Bash(git show *)", "Bash(git diff *)",
		"Bash(git branch *)", "Bash(git ls-files *)", "Bash(git blame *)", "Bash(git rev-parse *)",
		"Bash(bd *)",
	}
	protectedArgs = map[string]bool{
		flagPromptFile: true, "-p": true, "--prompt": true, "--single": true, "--prompt-json": true,
		flagOutputFormat: true, flagNoAutoUpdate: true, "--auto-update": true,
		flagNoLeader: true, "--leader": true, "--leader-socket": true,
		flagNoSubagents: true, "--subagents": true, "--agents": true,
		flagCwd: true, flagModel: true, flagRules: true, flagMaxTurns: true,
		flagPermissionMode: true, flagTools: true, flagDisallowedTools: true,
		flagAllow: true, flagDeny: true, "--allowedTools": true, "--disallowedTools": true,
		"--always-approve": true, "--yolo": true, "--dangerously-skip-permissions": true,
		"--disable-web-search": true,
	}
)

// ToolFlags maps a policy onto Grok's native permission and tool controls.
// Web tools are added separately only when RunSpec.Web is true. A catch-all
// --deny Bash is deliberately not used: Grok deny rules outrank allow rules.
func ToolFlags(p cartridge.ToolPolicy) []string {
	perm := permissionNoAsk
	if p == cartridge.ToolReadExec || p == cartridge.ToolReadWrite {
		perm = permissionBypass
	}
	args := []string{flagPermissionMode, perm}

	allow := append([]string(nil), readTools...)
	deny := []string{subagentTool}
	switch p {
	case cartridge.ToolReadOnly, cartridge.ToolReadExec:
		allow = append(allow, terminalTool)
		deny = append(deny, editTool)
	case cartridge.ToolReadWrite:
		allow = nil
	default:
		deny = append(deny, editTool)
	}
	if len(allow) > 0 {
		args = append(args, flagTools, strings.Join(allow, ","))
	}
	args = append(args, flagDisallowedTools, strings.Join(deny, ","))
	switch p {
	case cartridge.ToolReadOnly:
		for _, rule := range ReadOnlyShell {
			args = append(args, flagAllow, rule)
		}
	case cartridge.ToolReadExec:
		args = append(args, flagAllow, "Bash")
	}
	return args
}

func withWebTools(flags []string, web bool) []string {
	out := append([]string(nil), flags...)
	if web {
		out = append(out, flagAllow, "WebFetch", flagAllow, "WebSearch")
	}
	for i := 0; i+1 < len(out); i++ {
		switch out[i] {
		case flagTools:
			if web {
				out[i+1] += "," + strings.Join(WebTools, ",")
			}
		case flagDisallowedTools:
			if !web {
				out[i+1] += "," + strings.Join(WebTools, ",")
			}
		}
	}
	return out
}

// Args builds one headless invocation. promptFile carries the user prompt;
// protected configured arguments are discarded so they cannot replace the
// protocol, workspace, prompt, model, or native policy controls.
func (a *Adapter) Args(spec cartridge.RunSpec, promptFile string) []string {
	args := []string{flagOutputFormat, outputFormatStream, flagNoAutoUpdate, flagNoLeader, flagNoSubagents, flagCwd, spec.Workdir}
	if m := a.model(spec); m != "" {
		args = append(args, flagModel, m)
	}
	if spec.SystemPrompt != "" {
		args = append(args, flagRules, spec.SystemPrompt)
	}
	if spec.MaxTurns > 0 {
		args = append(args, flagMaxTurns, strconv.Itoa(spec.MaxTurns))
	}
	args = append(args, safeExtraArgs(a.cfg.Args)...)
	args = append(args, withWebTools(ToolFlags(spec.ToolPolicy), spec.Web)...)
	return append(args, flagPromptFile, promptFile)
}

func safeExtraArgs(configured []string) []string {
	out := make([]string, 0, len(configured))
	for i := 0; i < len(configured); i++ {
		arg := configured[i]
		name, _, hasValue := strings.Cut(arg, "=")
		if arg == "--" {
			break
		}
		if protectedArgs[name] {
			if !hasValue && takesValue(name) && i+1 < len(configured) {
				i++
			}
			continue
		}
		out = append(out, arg)
	}
	return out
}

func takesValue(name string) bool {
	switch name {
	case flagNoAutoUpdate, "--auto-update", flagNoLeader, "--leader", flagNoSubagents, "--subagents",
		"--always-approve", "--yolo", "--dangerously-skip-permissions", "--disable-web-search":
		return false
	default:
		return true
	}
}

func (a *Adapter) model(spec cartridge.RunSpec) string {
	if spec.Model != "" {
		return spec.Model
	}
	return a.cfg.Model
}

var envPrefixes = []string{"GROK_", "XAI_"}

// DefaultEnv disables background self-updates for every firing.
var DefaultEnv = map[string]string{"GROK_DISABLE_AUTOUPDATER": "1"}

// InheritEnv adds Grok and xAI configuration and authentication variables,
// including GROK_HOME and XAI_API_KEY, to the base allowlist.
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
