package junie

import (
	"fmt"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/domain"
)

var _ agents.ToolPolicyEnforcer = (*Adapter)(nil)

// ToolPolicyEnforcement accepts read_write only. Junie's interactive --plan
// mode and global action allowlist do not enforce a per-firing policy.
func (a *Adapter) ToolPolicyEnforcement(p cartridge.ToolPolicy) (agents.Enforcement, error) {
	if !p.Valid() {
		return agents.Enforcement{}, fmt.Errorf("%w: tool policy %q", domain.ErrUnknownValue, string(p))
	}
	if p != cartridge.ToolReadWrite {
		return agents.Enforcement{}, fmt.Errorf("junie has no native enforcement for %s; only %s is supported", p, cartridge.ToolReadWrite)
	}
	return agents.Enforcement{
		Cartridge: a.Name(),
		Policy:    p,
		Mode:      agents.EnforceNone,
		Detail:    "read_write restricts nothing; Junie has no verified per-firing restricted policies",
	}, nil
}

func validatePolicy(p cartridge.ToolPolicy) error {
	if p != cartridge.ToolReadWrite {
		return fmt.Errorf("cartridge junie supports only %s, not %s", cartridge.ToolReadWrite, p)
	}
	return nil
}
