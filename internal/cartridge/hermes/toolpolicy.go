package hermes

import (
	"fmt"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/domain"
)

var _ agents.ToolPolicyEnforcer = (*Adapter)(nil)

// ToolPolicyEnforcement reports Hermes' exact native toolset mappings. It
// refuses read_only rather than offering a terminal or prompting the model to
// behave as though it were read-only.
func (a *Adapter) ToolPolicyEnforcement(p cartridge.ToolPolicy) (agents.Enforcement, error) {
	if !p.Valid() {
		return agents.Enforcement{}, fmt.Errorf("%w: tool policy %q", domain.ErrUnknownValue, string(p))
	}
	if p == cartridge.ToolReadOnly {
		return agents.Enforcement{}, fmt.Errorf("hermes cannot enforce read_only: its read tools are bundled with native edit tools")
	}
	name := a.Name()
	if name == "" {
		name = Name
	}
	detail := "terminal toolset only; no native file-edit tools"
	if p == cartridge.ToolReadWrite {
		detail = "terminal and file toolsets, including write_file and patch"
	}
	return agents.Enforcement{
		Cartridge: name,
		Policy:    p,
		Mode:      agents.EnforceNative,
		Controls:  ToolFlags(p),
		Detail:    detail,
	}, nil
}
