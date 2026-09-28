package orchestrator

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// roles.<role>.model and research are stamped on every firing in one place,
// so the product owner gets its model whether the planner or the feedback
// loop's arbitration fires it, and only the planning product owner may use
// the web.
func TestSupervisorStampsTheRoleModelAndThePlannersWebAccess(t *testing.T) {
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: supApproval("alpha")})
	s := f.supervisor(t, func(o *SupervisorOptions) {
		o.RoleModels = map[domain.Role]string{domain.RolePO: "claude-opus-5-5"}
		o.Research = true
	})
	stamp := func(role domain.Role, task events.Task) cartridge.RunSpec {
		run := agents.AgentRun{Spec: cartridge.RunSpec{Model: "sonnet"}}
		agent := domain.Agent{ID: "a", Role: role}
		s.stamp(&run, AgentFiring{Agent: agent, Task: task}, agent, supPrep{}, f.Run)
		return run.Spec
	}

	plan := stamp(domain.RolePO, events.TaskPlan)
	assert.Equal(t, "claude-opus-5-5", plan.Model)
	assert.True(t, plan.Web, "the planning product owner researches")

	arbitrate := stamp(domain.RolePO, events.TaskArbitrate)
	assert.Equal(t, "claude-opus-5-5", arbitrate.Model, "every product owner firing uses the role's model")
	assert.False(t, arbitrate.Web, "only planning is given the web")

	review := stamp(domain.RoleTester, events.TaskReview)
	assert.Equal(t, "sonnet", review.Model, "a role with no override keeps its builder's model")
	assert.False(t, review.Web)

	off := f.supervisor(t, nil)
	run := agents.AgentRun{Spec: cartridge.RunSpec{Model: "sonnet"}}
	agent := domain.Agent{ID: "b", Role: domain.RolePO}
	off.stamp(&run, AgentFiring{Agent: agent, Task: events.TaskPlan}, agent, supPrep{}, f.Run)
	assert.Equal(t, "sonnet", run.Spec.Model)
	assert.False(t, run.Spec.Web, "research off gives nobody the web")
}
