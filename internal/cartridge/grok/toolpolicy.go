package grok

import (
	"fmt"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/domain"
)

var _ agents.ToolPolicyEnforcer = (*Adapter)(nil)

// ToolPolicyEnforcement describes Grok's native controls for a policy.
func (a *Adapter) ToolPolicyEnforcement(p cartridge.ToolPolicy) (agents.Enforcement, error) {
	if !p.Valid() {
		return agents.Enforcement{}, fmt.Errorf("%w: tool policy %q", domain.ErrUnknownValue, string(p))
	}
	if p == cartridge.ToolReadOnly {
		return agents.Enforcement{}, errReadOnlyUnsupported
	}
	name := a.Name()
	if name == "" {
		name = Name
	}
	return agents.Enforcement{
		Cartridge: name,
		Policy:    p,
		Mode:      agents.EnforceNative,
		Controls:  ToolFlags(p),
		Detail:    toolPolicyDetail[p],
	}, nil
}

var toolPolicyDetail = map[cartridge.ToolPolicy]string{
	cartridge.ToolReadExec:  "bypassPermissions with read tools and unrestricted shell; edits, web, and subagents denied",
	cartridge.ToolReadWrite: "bypassPermissions with edits and terminal allowed; web and subagents remain separately controlled",
}
