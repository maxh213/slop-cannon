package kimi

import (
	"fmt"
	"strings"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/domain"
)

var _ agents.ToolPolicyEnforcer = (*Adapter)(nil)

// ToolPolicyEnforcement reports the native YAML tool and subagent controls.
// read_only is refused instead of substituting Plan mode or prompt language,
// neither of which supplies the enforcement the planner needs.
func (a *Adapter) ToolPolicyEnforcement(policy cartridge.ToolPolicy) (agents.Enforcement, error) {
	if !policy.Valid() {
		return agents.Enforcement{}, fmt.Errorf("%w: tool policy %q", domain.ErrUnknownValue, policy)
	}
	if policy == cartridge.ToolReadOnly {
		return agents.Enforcement{}, ErrReadOnlyUnavailable
	}
	spec := cartridge.RunSpec{ToolPolicy: policy}
	tools, err := profileTools(spec)
	if err != nil {
		return agents.Enforcement{}, err
	}
	name := a.Name()
	if name == "" {
		name = Name
	}
	detail := "file-writing tools and all subagents removed; Bash and read tools allowed"
	toolControl := "tools=" + strings.Join(tools, ",")
	if policy == cartridge.ToolReadWrite {
		detail = "built-in and configured tools allowed; subagent delegation disabled"
		toolControl = "tools=*"
	}
	return agents.Enforcement{
		Cartridge: name,
		Policy:    policy,
		Mode:      agents.EnforceNative,
		Controls:  []string{flagAgentFile, "<per-run>", toolControl, "subagents=[]"},
		Detail:    detail,
	}, nil
}
