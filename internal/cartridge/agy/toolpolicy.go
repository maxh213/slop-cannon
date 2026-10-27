package agy

import (
	"errors"
	"fmt"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/domain"
)

var (
	// ErrReadOnlyUnverified refuses native read_only enablement before the live canary.
	ErrReadOnlyUnverified = errors.New("agy read_only plan mode requires the live write-refusal canary in testdata/agy/README.md")
	// ErrReadExecUnsupported reports that agy has no execute-without-write mode.
	ErrReadExecUnsupported = errors.New("agy has no exact native read_exec enforcement")
	// ErrPlanSlashConflict reports a combination that silently disables plan mode.
	ErrPlanSlashConflict = errors.New("agy read_only plan mode is incompatible with --disable-slash-commands")
)

var _ agents.ToolPolicyEnforcer = (*Adapter)(nil)

// ToolPolicyEnforcement declares only the noninteractive read_write mapping.
// Plan mode remains a candidate until its native write refusal is verified.
func (a *Adapter) ToolPolicyEnforcement(p cartridge.ToolPolicy) (agents.Enforcement, error) {
	if !p.Valid() {
		return agents.Enforcement{}, fmt.Errorf("%w: tool policy %q", domain.ErrUnknownValue, string(p))
	}
	base := agents.Enforcement{Cartridge: a.Name(), Policy: p}
	switch p {
	case cartridge.ToolReadWrite:
		base.Mode = agents.EnforceNative
		base.Controls = ToolFlags(p)
		base.Detail = "accept-edits mode with every tool permission auto-approved"
		return base, nil
	case cartridge.ToolReadOnly:
		return agents.Enforcement{}, ErrReadOnlyUnverified
	default:
		return agents.Enforcement{}, ErrReadExecUnsupported
	}
}
