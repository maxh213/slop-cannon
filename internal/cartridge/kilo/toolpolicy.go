package kilo

import (
	"fmt"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/domain"
)

var _ agents.ToolPolicyEnforcer = (*Adapter)(nil)

// ToolPolicyEnforcement reports the generated agent's native permission rules.
func (a *Adapter) ToolPolicyEnforcement(policy cartridge.ToolPolicy) (agents.Enforcement, error) {
	if !policy.Valid() {
		return agents.Enforcement{}, fmt.Errorf("%w: tool policy %q", domain.ErrUnknownValue, policy)
	}
	if policy == cartridge.ToolReadOnly {
		return agents.Enforcement{}, ErrReadOnlyUnavailable
	}
	if _, err := permissions(cartridge.RunSpec{ToolPolicy: policy}); err != nil {
		return agents.Enforcement{}, err
	}
	name := a.Name()
	if name == "" {
		name = Name
	}
	detail := "Bash and reads allowed; native edits, tasks, and unrequested web tools denied"
	if policy == cartridge.ToolReadWrite {
		detail = "native writes allowed; tasks and unrequested web tools denied"
	}
	return agents.Enforcement{
		Cartridge: name,
		Policy:    policy,
		Mode:      agents.EnforceNative,
		Controls:  []string{configContentEnv, "agent=sc", "permission.task=deny"},
		Detail:    detail,
	}, nil
}
