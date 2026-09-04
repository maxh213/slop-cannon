package claude

import (
	"fmt"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/domain"
)

// This file is P5-11: the adapter answers for its own tool-policy
// enforcement instead of being recognised by concrete type inside
// internal/agents. The mapping itself is unchanged; only its home moved, so
// internal/agents no longer imports this package (and no longer drags the
// init-time registry registration into every importer of the agents layer).

// Compile-time proof that the adapter answers for its own enforcement, so
// agents.EnforcementFor resolves it through the interface rather than a type
// branch.
var _ agents.ToolPolicyEnforcer = (*Adapter)(nil)

// ToolPolicyEnforcement reports how Claude Code holds p: natively, in every
// case, through the flags [ToolFlags] builds (--permission-mode dontAsk plus
// the allowed and disallowed tool lists verified against Claude Code
// 2.1.258). read_only keeps a restricted Bash allowlist because that CLI
// offers Glob and Grep only when Bash is disallowed (P5-09); read_exec adds
// Bash and still denies the file-editing tools; read_write allows them.
//
// The error wraps domain.ErrUnknownValue for a policy that is not one of
// cartridge.ToolPolicies(); agents.EnforcementFor turns that into a refusal
// to fire.
func (a *Adapter) ToolPolicyEnforcement(p cartridge.ToolPolicy) (agents.Enforcement, error) {
	if !p.Valid() {
		return agents.Enforcement{}, fmt.Errorf("%w: tool policy %q", domain.ErrUnknownValue, string(p))
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

// toolPolicyDetail is the one-line human explanation each policy carries.
var toolPolicyDetail = map[cartridge.ToolPolicy]string{
	cartridge.ToolReadOnly:  "file-editing tools removed; Bash restricted to a read-only allowlist",
	cartridge.ToolReadExec:  "file-editing tools removed; Bash allowed for tests and builds",
	cartridge.ToolReadWrite: "Bash and the file-editing tools allowed",
}
