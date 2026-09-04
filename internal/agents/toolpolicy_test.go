package agents_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/claude"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// toolPolicyClaude builds a Claude adapter without touching the CLI.
func toolPolicyClaude(t *testing.T, name string) cartridge.Cartridge {
	t.Helper()
	c, err := claude.New(name, config.Cartridge{})
	require.NoError(t, err)
	return c
}

// toolPolicyMock builds a mock adapter playing s under the given registry
// name.
func toolPolicyMock(t *testing.T, name string, s *mock.Scenario) *mock.Adapter {
	t.Helper()
	a, err := mock.NewAdapter(name, s)
	require.NoError(t, err)
	return a
}

// toolPolicySpec is a minimal valid run spec in dir.
func toolPolicySpec(dir, prompt string) cartridge.RunSpec {
	return cartridge.RunSpec{Prompt: prompt, Workdir: dir, Timeout: 30 * time.Second}
}

// toolPolicyRun fires one mock session and returns its events and result.
func toolPolicyRun(t *testing.T, a *mock.Adapter, spec cartridge.RunSpec) ([]cartridge.SessionEvent, cartridge.Result) {
	t.Helper()
	sess, err := a.Run(context.Background(), spec)
	require.NoError(t, err)
	var evs []cartridge.SessionEvent
	for ev := range sess.Events() {
		evs = append(evs, ev)
	}
	res, err := sess.Wait()
	require.NoError(t, err)
	return evs, res
}

// toolPolicyBare is a cartridge with no tool controls at all: the shape of a
// future adapter that can only be trusted with read_write.
type toolPolicyBare struct{ name string }

func (b toolPolicyBare) Name() string { return b.name }

func (b toolPolicyBare) Detect(context.Context) (cartridge.Status, error) {
	return cartridge.Status{Installed: true}, nil
}

func (b toolPolicyBare) Run(context.Context, cartridge.RunSpec) (cartridge.Session, error) {
	return nil, errors.New("not implemented in the test")
}

// toolPolicyDeclaring is a future adapter that implements the per-adapter
// method.
type toolPolicyDeclaring struct {
	toolPolicyBare
	en  agents.Enforcement
	err error
}

func (d toolPolicyDeclaring) ToolPolicyEnforcement(cartridge.ToolPolicy) (agents.Enforcement, error) {
	return d.en, d.err
}

func TestToolPolicyForRoleAndTask(t *testing.T) {
	t.Parallel()

	t.Run("roles", func(t *testing.T) {
		t.Parallel()
		for role, want := range map[domain.Role]cartridge.ToolPolicy{
			domain.RolePO:       cartridge.ToolReadOnly,
			domain.RoleEngineer: cartridge.ToolReadWrite,
			domain.RoleTester:   cartridge.ToolReadExec,
		} {
			got, err := agents.PolicyForRole(role)
			require.NoError(t, err, role)
			assert.Equal(t, want, got, role)
		}
		// Every domain role has a policy: nothing may fire unclassified.
		for _, role := range domain.Roles() {
			_, err := agents.PolicyForRole(role)
			assert.NoError(t, err, role)
		}
	})

	t.Run("tasks", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			task   events.Task
			role   domain.Role
			policy cartridge.ToolPolicy
		}{
			{events.TaskPlan, domain.RolePO, cartridge.ToolReadOnly},
			{events.TaskArbitrate, domain.RolePO, cartridge.ToolReadOnly},
			{events.TaskEscalate, domain.RolePO, cartridge.ToolReadOnly},
			{events.TaskImplement, domain.RoleEngineer, cartridge.ToolReadWrite},
			{events.TaskAddressFeedback, domain.RoleEngineer, cartridge.ToolReadWrite},
			{events.TaskResolveConflict, domain.RoleEngineer, cartridge.ToolReadWrite},
			{events.TaskReview, domain.RoleTester, cartridge.ToolReadExec},
			{events.TaskEvaluatePushback, domain.RoleTester, cartridge.ToolReadExec},
			{events.TaskReviewPR, domain.RoleTester, cartridge.ToolReadExec},
		}
		require.Len(t, agents.PolicyTasks(), len(cases), "every declared task is covered by this table")
		for _, c := range cases {
			p, err := agents.PolicyForTask(c.task)
			require.NoError(t, err, c.task)
			assert.Equal(t, c.policy, p, c.task)

			r, err := agents.RoleForTask(c.task)
			require.NoError(t, err, c.task)
			assert.Equal(t, c.role, r, c.task)

			p, err = agents.Policy(c.role, c.task)
			require.NoError(t, err, c.task)
			assert.Equal(t, c.policy, p, c.task)
		}
	})

	t.Run("unknown values", func(t *testing.T) {
		t.Parallel()
		_, err := agents.PolicyForRole("wizard")
		require.ErrorIs(t, err, domain.ErrUnknownValue)
		assert.Contains(t, err.Error(), "engineer")

		_, err = agents.PolicyForRole("")
		require.ErrorIs(t, err, domain.ErrUnknownValue)

		_, err = agents.PolicyForTask("deploy")
		require.ErrorIs(t, err, domain.ErrUnknownValue)
		assert.Contains(t, err.Error(), "implement")

		_, err = agents.RoleForTask("deploy")
		require.ErrorIs(t, err, domain.ErrUnknownValue)
	})

	t.Run("role and task must agree", func(t *testing.T) {
		t.Parallel()
		// A tester fired to implement is a bug, not a permission upgrade.
		_, err := agents.Policy(domain.RoleTester, events.TaskImplement)
		require.ErrorIs(t, err, domain.ErrUnknownValue)
		assert.Contains(t, err.Error(), `belongs to role "engineer"`)

		_, err = agents.Policy("wizard", events.TaskImplement)
		require.ErrorIs(t, err, domain.ErrUnknownValue)

		_, err = agents.Policy(domain.RoleEngineer, "deploy")
		require.ErrorIs(t, err, domain.ErrUnknownValue)
	})

	t.Run("empty half falls back", func(t *testing.T) {
		t.Parallel()
		p, err := agents.Policy(domain.RoleTester, "")
		require.NoError(t, err)
		assert.Equal(t, cartridge.ToolReadExec, p)

		p, err = agents.Policy("", events.TaskPlan)
		require.NoError(t, err)
		assert.Equal(t, cartridge.ToolReadOnly, p)

		_, err = agents.Policy("", "")
		require.ErrorIs(t, err, domain.ErrUnknownValue)
	})
}

func TestToolPolicyApplyPolicy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	spec := toolPolicySpec(dir, "review the diff")
	require.NoError(t, agents.ApplyPolicy(&spec, domain.RoleTester, events.TaskReview))
	assert.Equal(t, cartridge.ToolReadExec, spec.ToolPolicy)
	require.NoError(t, spec.Validate(), "the spec is fireable once the policy is set")

	require.NoError(t, agents.ApplyPolicy(&spec, domain.RoleEngineer, events.TaskImplement))
	assert.Equal(t, cartridge.ToolReadWrite, spec.ToolPolicy)

	// A rejected policy leaves the spec alone.
	err := agents.ApplyPolicy(&spec, domain.RoleTester, events.TaskImplement)
	require.ErrorIs(t, err, domain.ErrUnknownValue)
	assert.Equal(t, cartridge.ToolReadWrite, spec.ToolPolicy)

	require.Error(t, agents.ApplyPolicy(nil, domain.RolePO, events.TaskPlan))
}

func TestToolPolicyClaudeMapping(t *testing.T) {
	t.Parallel()

	c := toolPolicyClaude(t, claude.Name)
	editTools := []string{"Edit", "Write", "MultiEdit", "NotebookEdit"}

	t.Run("every policy is native and carries the verified flags", func(t *testing.T) {
		t.Parallel()
		for _, p := range cartridge.ToolPolicies() {
			en, err := agents.EnforcementFor(c, p)
			require.NoError(t, err, p)
			assert.Equal(t, claude.Name, en.Cartridge, p)
			assert.Equal(t, p, en.Policy, p)
			assert.Equal(t, agents.EnforceNative, en.Mode, p)
			assert.True(t, en.Native(), p)
			assert.NoError(t, en.Check(), p)
			assert.NotEmpty(t, en.Detail, p)
			// The controls are the adapter's own, not a second copy of the
			// mapping.
			assert.Equal(t, claude.ToolFlags(p), en.Controls, p)
			assert.Equal(t, []string{"--permission-mode", "dontAsk"}, en.Controls[:2], p)
		}
	})

	t.Run("read_only removes the editing tools and restricts the shell", func(t *testing.T) {
		t.Parallel()
		en, err := agents.EnforcementFor(c, cartridge.ToolReadOnly)
		require.NoError(t, err)
		allowed, disallowed := toolPolicyFlagValues(t, en.Controls)
		assert.Contains(t, allowed, "Bash(ls:*)")
		assert.Contains(t, allowed, "Bash(git diff:*)")
		assert.NotContains(t, allowed, "Bash,", "a bare Bash tool would allow any command")
		for _, tool := range editTools {
			assert.Contains(t, disallowed, tool)
			assert.NotContains(t, allowed, tool)
		}
	})

	t.Run("read_exec adds the shell but no writes", func(t *testing.T) {
		t.Parallel()
		en, err := agents.EnforcementFor(c, cartridge.ToolReadExec)
		require.NoError(t, err)
		allowed, disallowed := toolPolicyFlagValues(t, en.Controls)
		assert.Equal(t, "Bash", allowed)
		for _, tool := range editTools {
			assert.Contains(t, disallowed, tool)
		}
	})

	t.Run("read_write allows the editing tools", func(t *testing.T) {
		t.Parallel()
		en, err := agents.EnforcementFor(c, cartridge.ToolReadWrite)
		require.NoError(t, err)
		allowed, disallowed := toolPolicyFlagValues(t, en.Controls)
		assert.Empty(t, disallowed)
		for _, tool := range append([]string{"Bash"}, editTools...) {
			assert.Contains(t, allowed, tool)
		}
	})

	t.Run("the controls reach the CLI arguments", func(t *testing.T) {
		t.Parallel()
		a, err := claude.New(claude.Name, config.Cartridge{})
		require.NoError(t, err)
		adapter, ok := a.(*claude.Adapter)
		require.True(t, ok)
		spec := toolPolicySpec(t.TempDir(), "review the diff")
		require.NoError(t, agents.ApplyPolicy(&spec, domain.RoleTester, events.TaskReview))
		en, err := agents.EnforcementFor(a, spec.ToolPolicy)
		require.NoError(t, err)
		assert.True(t, toolPolicyContainsRun(adapter.Args(spec), en.Controls),
			"claude args %v must contain the controls %v", adapter.Args(spec), en.Controls)
	})

	t.Run("a Claude adapter under another registry name keeps the mapping", func(t *testing.T) {
		t.Parallel()
		en, err := agents.EnforcementFor(toolPolicyClaude(t, "claude-fable"), cartridge.ToolReadOnly)
		require.NoError(t, err)
		assert.Equal(t, "claude-fable", en.Cartridge)
		assert.Equal(t, agents.EnforceNative, en.Mode)
	})

	t.Run("the adapter's own method rejects an invalid policy", func(t *testing.T) {
		t.Parallel()
		// P5-11: the mapping is the adapter's, so it is asked directly here.
		adapter, ok := toolPolicyClaude(t, claude.Name).(agents.ToolPolicyEnforcer)
		require.True(t, ok, "the Claude adapter must implement agents.ToolPolicyEnforcer")
		_, err := adapter.ToolPolicyEnforcement("read_everything")
		require.ErrorIs(t, err, domain.ErrUnknownValue)

		en, err := adapter.ToolPolicyEnforcement(cartridge.ToolReadWrite)
		require.NoError(t, err)
		assert.Equal(t, claude.Name, en.Cartridge)
	})
}

// toolPolicyFlagValues returns the values of --allowedTools and
// --disallowedTools in args.
func toolPolicyFlagValues(t *testing.T, args []string) (allowed, disallowed string) {
	t.Helper()
	for i := 0; i < len(args)-1; i++ {
		switch args[i] {
		case "--allowedTools":
			allowed = args[i+1]
		case "--disallowedTools":
			disallowed = args[i+1]
		}
	}
	return allowed, disallowed
}

// toolPolicyContainsRun reports whether want appears in args as a contiguous
// run.
func toolPolicyContainsRun(args, want []string) bool {
	if len(want) == 0 {
		return true
	}
	for i := 0; i+len(want) <= len(args); i++ {
		match := true
		for j, w := range want {
			if args[i+j] != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func TestToolPolicyMockMapping(t *testing.T) {
	t.Parallel()

	t.Run("the mock enforces every policy in-process", func(t *testing.T) {
		t.Parallel()
		a := toolPolicyMock(t, mock.Name, mock.DefaultScenario())
		for _, p := range cartridge.ToolPolicies() {
			en, err := agents.EnforcementFor(a, p)
			require.NoError(t, err, p)
			assert.Equal(t, mock.Name, en.Cartridge, p)
			assert.Equal(t, p, en.Policy, p)
			assert.Equal(t, agents.EnforceNative, en.Mode, p)
			assert.NoError(t, en.Check(), p)
		}
	})

	t.Run("a read_exec firing cannot write", func(t *testing.T) {
		t.Parallel()
		s := mock.NewScenario("write attempt")
		s.Otherwise("write").Say("editing").Write("sc-tool-policy.txt", "written\n").Output("done")
		a := toolPolicyMock(t, mock.Name, s)

		dir := t.TempDir()
		spec := toolPolicySpec(dir, "review the diff")
		require.NoError(t, agents.ApplyPolicy(&spec, domain.RoleTester, events.TaskReview))
		en, err := agents.EnforcementFor(a, spec.ToolPolicy)
		require.NoError(t, err)
		require.NoError(t, en.Check())

		evs, _ := toolPolicyRun(t, a, spec)
		assert.True(t, toolPolicyRefused(evs), "the write must be refused: %v", evs)
		_, statErr := os.Stat(filepath.Join(dir, "sc-tool-policy.txt"))
		assert.True(t, os.IsNotExist(statErr), "read_exec must leave no file behind")
	})

	t.Run("a read_write firing can write", func(t *testing.T) {
		t.Parallel()
		s := mock.NewScenario("write attempt")
		s.Otherwise("write").Say("editing").Write("sc-tool-policy.txt", "written\n").Output("done")
		a := toolPolicyMock(t, mock.Name, s)

		dir := t.TempDir()
		spec := toolPolicySpec(dir, "implement the bead")
		require.NoError(t, agents.ApplyPolicy(&spec, domain.RoleEngineer, events.TaskImplement))

		evs, _ := toolPolicyRun(t, a, spec)
		assert.False(t, toolPolicyRefused(evs), "the write must succeed: %v", evs)
		b, err := os.ReadFile(filepath.Join(dir, "sc-tool-policy.txt"))
		require.NoError(t, err)
		assert.Equal(t, "written\n", string(b))
	})
}

// toolPolicyRefused reports whether any tool result in evs is an error
// mentioning the tool policy.
func toolPolicyRefused(evs []cartridge.SessionEvent) bool {
	for _, ev := range evs {
		if ev.Kind == cartridge.EventToolResult && ev.IsError {
			return true
		}
	}
	return false
}

func TestToolPolicyEnforcementFallback(t *testing.T) {
	t.Parallel()

	bare := toolPolicyBare{name: "future-cli"}

	t.Run("read_write needs no controls", func(t *testing.T) {
		t.Parallel()
		en, err := agents.EnforcementFor(bare, cartridge.ToolReadWrite)
		require.NoError(t, err)
		assert.Equal(t, agents.EnforceNone, en.Mode)
		assert.NoError(t, en.Check())
	})

	t.Run("restricted policies degrade to a copied worktree", func(t *testing.T) {
		t.Parallel()
		for _, p := range []cartridge.ToolPolicy{cartridge.ToolReadOnly, cartridge.ToolReadExec} {
			en, err := agents.EnforcementFor(bare, p)
			require.NoError(t, err, p)
			assert.Equal(t, agents.EnforceCopyWorktree, en.Mode, p)
			assert.False(t, en.Native(), p)
			require.ErrorIs(t, en.Check(), agents.ErrCopyWorktreeUnimplemented, p)
			assert.Contains(t, en.Check().Error(), "future-cli")
		}
	})

	t.Run("bad arguments", func(t *testing.T) {
		t.Parallel()
		_, err := agents.EnforcementFor(nil, cartridge.ToolReadOnly)
		require.Error(t, err)

		_, err = agents.EnforcementFor(bare, "read_everything")
		require.ErrorIs(t, err, domain.ErrUnknownValue)

		_, err = agents.EnforcementFor(bare, "")
		require.ErrorIs(t, err, domain.ErrUnknownValue)
	})
}

func TestToolPolicyPerAdapterMethod(t *testing.T) {
	t.Parallel()

	t.Run("an adapter that declares its own enforcement wins", func(t *testing.T) {
		t.Parallel()
		d := toolPolicyDeclaring{
			toolPolicyBare: toolPolicyBare{name: "declaring"},
			en:             agents.Enforcement{Mode: agents.EnforceNative, Controls: []string{"--sandbox=ro"}, Detail: "own sandbox"},
		}
		en, err := agents.EnforcementFor(d, cartridge.ToolReadOnly)
		require.NoError(t, err)
		// The resolver fills in what the adapter left blank.
		assert.Equal(t, "declaring", en.Cartridge)
		assert.Equal(t, cartridge.ToolReadOnly, en.Policy)
		assert.Equal(t, agents.EnforceNative, en.Mode)
		assert.Equal(t, []string{"--sandbox=ro"}, en.Controls)
		assert.NoError(t, en.Check())
	})

	t.Run("an adapter may refuse a policy", func(t *testing.T) {
		t.Parallel()
		d := toolPolicyDeclaring{
			toolPolicyBare: toolPolicyBare{name: "declaring"},
			err:            errors.New("no read-only mode"),
		}
		_, err := agents.EnforcementFor(d, cartridge.ToolReadOnly)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no read-only mode")
	})

	t.Run("an adapter must name a mode", func(t *testing.T) {
		t.Parallel()
		d := toolPolicyDeclaring{toolPolicyBare: toolPolicyBare{name: "declaring"}}
		_, err := agents.EnforcementFor(d, cartridge.ToolReadOnly)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no enforcement mode")
	})
}

func TestToolPolicyRegisterEnforcement(t *testing.T) {
	// Not parallel: it mutates the package-level registry.
	name := "mock-registered"
	err := agents.RegisterEnforcement(name, func(n string, p cartridge.ToolPolicy) (agents.Enforcement, error) {
		return agents.Enforcement{Cartridge: n, Policy: p, Mode: agents.EnforceNative, Detail: "scripted"}, nil
	})
	require.NoError(t, err)

	a := toolPolicyMock(t, name, mock.DefaultScenario())
	en, err := agents.EnforcementFor(a, cartridge.ToolReadOnly)
	require.NoError(t, err)
	assert.Equal(t, name, en.Cartridge)
	assert.Equal(t, agents.EnforceNative, en.Mode)
	assert.Equal(t, "scripted", en.Detail)

	require.Error(t, agents.RegisterEnforcement(name, func(string, cartridge.ToolPolicy) (agents.Enforcement, error) {
		return agents.Enforcement{}, nil
	}), "duplicate registration")
	require.Error(t, agents.RegisterEnforcement("  ", func(string, cartridge.ToolPolicy) (agents.Enforcement, error) {
		return agents.Enforcement{}, nil
	}))
	require.Error(t, agents.RegisterEnforcement("nil-fn", nil))

	t.Run("a registered function may refuse", func(t *testing.T) {
		refusing := "mock-refusing"
		require.NoError(t, agents.RegisterEnforcement(refusing, func(string, cartridge.ToolPolicy) (agents.Enforcement, error) {
			return agents.Enforcement{}, errors.New("unsupported")
		}))
		_, err := agents.EnforcementFor(toolPolicyMock(t, refusing, mock.DefaultScenario()), cartridge.ToolReadExec)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported")
	})
}

func TestToolPolicyEnforcementForFiring(t *testing.T) {
	t.Parallel()

	c := toolPolicyClaude(t, claude.Name)
	en, err := agents.EnforcementForFiring(c, domain.RolePO, events.TaskPlan)
	require.NoError(t, err)
	assert.Equal(t, cartridge.ToolReadOnly, en.Policy)
	assert.Equal(t, agents.EnforceNative, en.Mode)

	_, err = agents.EnforcementForFiring(c, domain.RolePO, events.TaskImplement)
	require.ErrorIs(t, err, domain.ErrUnknownValue)
}

func TestToolPolicyEnforcementString(t *testing.T) {
	t.Parallel()

	en := agents.Enforcement{Cartridge: "claude", Policy: cartridge.ToolReadExec, Mode: agents.EnforceNative, Controls: []string{"--allowedTools", "Bash"}}
	assert.Equal(t, "claude read_exec: native (--allowedTools Bash)", en.String())
	assert.Equal(t, "native", agents.EnforceNative.String())

	bare := agents.Enforcement{Cartridge: "future-cli", Policy: cartridge.ToolReadOnly, Mode: agents.EnforceCopyWorktree}
	assert.Equal(t, "future-cli read_only: copy_worktree", bare.String())
}
