package codex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/domain"
)

const (
	flagJSON             = "--json"
	flagSkipGitRepoCheck = "--skip-git-repo-check"
	flagWorkdir          = "-C"
	flagModel            = "--model"
	flagConfig           = "-c"
	approvalPolicyNever  = `approval_policy="never"`
	webSearchEnabled     = "tools.web_search=true"
	flagSandbox          = "--sandbox"
	versionFlag          = "--version"
)

var sandboxByPolicy = map[cartridge.ToolPolicy]string{
	cartridge.ToolReadExec:  "read-only",
	cartridge.ToolReadWrite: "workspace-write",
}

// ToolFlags returns the native non-interactive approval config and exact
// sandbox for p. It returns nil for read_only and unknown policies.
func ToolFlags(p cartridge.ToolPolicy) []string {
	sandbox, ok := sandboxByPolicy[p]
	if !ok {
		return nil
	}
	return []string{flagConfig, approvalPolicyNever, flagSandbox, sandbox}
}

// ToolPolicyEnforcement reports the native sandbox enforcement. Codex's
// read-only sandbox still permits shell and therefore is not read_only.
func (a *Adapter) ToolPolicyEnforcement(p cartridge.ToolPolicy) (agents.Enforcement, error) {
	if !p.Valid() {
		return agents.Enforcement{}, fmt.Errorf("%w: tool policy %q", domain.ErrUnknownValue, string(p))
	}
	flags := ToolFlags(p)
	if flags == nil {
		return agents.Enforcement{}, fmt.Errorf("codex cannot enforce %s exactly: its read-only sandbox still permits shell commands", p)
	}
	detail := "read-only sandbox permits execution but not file writes"
	if p == cartridge.ToolReadWrite {
		detail = "workspace-write sandbox permits execution and workspace file writes"
	}
	return agents.Enforcement{
		Cartridge: a.Name(), Policy: p, Mode: agents.EnforceNative,
		Controls: flags, Detail: detail,
	}, nil
}

// Args builds one firing's arguments. Configured extras carry routed effort
// as "-c model_reasoning_effort=<effort>". Model, approval and sandbox come
// last so extras cannot replace the routed model or weaken the tool policy.
func (a *Adapter) Args(spec cartridge.RunSpec) []string {
	if ToolFlags(spec.ToolPolicy) == nil {
		return nil
	}
	args := []string{"exec", flagJSON, flagSkipGitRepoCheck, flagWorkdir, spec.Workdir}
	args = append(args, a.cfg.Args...)
	if m := a.model(spec); m != "" {
		args = append(args, flagModel, m)
	}
	if spec.SystemPrompt != "" {
		instructions, _ := json.Marshal(spec.SystemPrompt)
		args = append(args, flagConfig, "instructions="+string(instructions))
	}
	if spec.Web {
		args = append(args, flagConfig, webSearchEnabled)
	}
	return append(args, ToolFlags(spec.ToolPolicy)...)
}

func (a *Adapter) model(spec cartridge.RunSpec) string {
	if spec.Model != "" {
		return spec.Model
	}
	return a.cfg.Model
}

// InheritEnv extends base with OpenAI and Codex configuration. CODEX_HOME is
// deliberately not inherited: an Animus home in sc's parent environment must
// never select that account for a personal Codex firing. It can still be set
// explicitly in configured or RunSpec env.
func InheritEnv(base []string) []string {
	set := map[string]bool{}
	for _, name := range base {
		if name != "CODEX_HOME" {
			set[name] = true
		}
	}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name == "CODEX_HOME" {
			continue
		}
		if strings.HasPrefix(name, "OPENAI_") || strings.HasPrefix(name, "CODEX_") {
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

// childEnv layers configured env and RunSpec env, then applies the Animus
// account home last. Personal Codex has no implicit CODEX_HOME override.
func (a *Adapter) childEnv(specEnv map[string]string) (map[string]string, error) {
	env := make(map[string]string, len(a.cfg.Env)+len(specEnv)+1)
	for k, v := range a.cfg.Env {
		env[k] = v
	}
	for k, v := range specEnv {
		env[k] = v
	}
	if a.name != AnimusName {
		return env, nil
	}
	home, ok := a.cfg.Env[animusHomeEnv]
	if !ok || home == "" {
		home, _ = os.LookupEnv(animusHomeEnv)
	}
	if home == "" {
		base, err := a.homeDir()
		if err != nil {
			return nil, fmt.Errorf("resolving codex-animus CODEX_HOME: %w", err)
		}
		home = filepath.Join(base, ".codex-animus")
	}
	env["CODEX_HOME"] = home
	return env, nil
}
