package prompt_test

// This file is an external test package on purpose: it imports
// slop-cannon/internal/agents for the real message contracts, and the
// agents package is free to import the prompt builder in turn (P5-02 …
// P5-04). The builder itself must never import agents.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// pxRequest is the smallest request that produces a usable prompt for role.
func pxRequest(role domain.Role, lens domain.Lens, task events.Task) prompt.Request {
	return prompt.Request{
		Role:       role,
		Lens:       lens,
		Task:       task,
		RunID:      "20260902-101500-1a2b",
		AgentID:    domain.AgentID("agent-" + role.String()),
		ToolPolicy: cartridge.ToolReadOnly,
		Objective:  "Do the job described below.",
	}
}

// TestRealContractsAreAppended checks that every P5-07 message contract can
// be pasted onto a prompt and is recognised as already present afterwards.
func TestRealContractsAreAppended(t *testing.T) {
	roles := map[domain.MessageType]prompt.Request{
		domain.MessagePlan:        pxRequest(domain.RolePO, "", events.TaskPlan),
		domain.MessageArbitration: pxRequest(domain.RolePO, "", events.TaskArbitrate),
		domain.MessageEscalation:  pxRequest(domain.RolePO, "", events.TaskEscalate),
		domain.MessageImplReport:  pxRequest(domain.RoleEngineer, "", events.TaskImplement),
		domain.MessageResponse:    pxRequest(domain.RoleEngineer, "", events.TaskAddressFeedback),
		domain.MessageVerdict:     pxRequest(domain.RoleTester, domain.LensQA, events.TaskReview),
	}
	require.Len(t, roles, len(domain.MessageTypes()))

	for msg, req := range roles {
		t.Run(msg.String(), func(t *testing.T) {
			contract, err := agents.ContractFor(msg)
			require.NoError(t, err)
			req.Contract = contract

			p, err := prompt.Build(req)
			require.NoError(t, err)
			assert.True(t, contract.HasPromptSuffix(p.Text), "contract missing from the prompt")
			assert.Contains(t, p.Text, "## Output contract ("+msg.String()+")")
			assert.Contains(t, p.Text, "```sc-result")
			assert.True(t, strings.HasSuffix(strings.TrimRight(p.Text, "\n"), "```"),
				"the schema block must be the last thing in the prompt")
			assert.False(t, p.OverBudget)
		})
	}
}

// TestPolicyAndPromptAgree fires the tool policy chosen by P5-06 through the
// builder, which is how the orchestrator will use the two together.
func TestPolicyAndPromptAgree(t *testing.T) {
	for _, task := range agents.PolicyTasks() {
		t.Run(task.String(), func(t *testing.T) {
			role, err := agents.RoleForTask(task)
			require.NoError(t, err)
			var spec cartridge.RunSpec
			require.NoError(t, agents.ApplyPolicy(&spec, role, task))

			lens := domain.Lens("")
			if role == domain.RoleTester {
				lens = domain.LensUX
			}
			req := pxRequest(role, lens, task)
			req.ToolPolicy = spec.ToolPolicy
			p, err := prompt.Build(req)
			require.NoError(t, err)
			assert.Contains(t, p.Text, "Tool policy: "+spec.ToolPolicy.String()+" —")
			assert.Contains(t, p.Text, "- Task: "+task.String())
		})
	}
}
