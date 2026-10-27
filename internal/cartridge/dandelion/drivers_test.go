package dandelion

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/agy"
	"slop-cannon/internal/cartridge/codex"
	"slop-cannon/internal/cartridge/grok"
	"slop-cannon/internal/cartridge/hermes"
	"slop-cannon/internal/cartridge/junie"
	"slop-cannon/internal/cartridge/kilo"
	"slop-cannon/internal/cartridge/kimi"
	"slop-cannon/internal/config"
	"slop-cannon/internal/testutil"
)

type fakeDriver struct {
	native  bool
	configs []config.Cartridge
	specs   []cartridge.RunSpec
}

func (f *fakeDriver) factory(name string, cfg config.Cartridge) (cartridge.Cartridge, error) {
	f.configs = append(f.configs, cfg)
	return &fakeDriverCartridge{name: name, owner: f}, nil
}

type fakeDriverCartridge struct {
	name  string
	owner *fakeDriver
}

func (f *fakeDriverCartridge) Name() string { return f.name }

func (f *fakeDriverCartridge) Detect(context.Context) (cartridge.Status, error) {
	return cartridge.Status{Installed: true}, nil
}

func (f *fakeDriverCartridge) Run(_ context.Context, spec cartridge.RunSpec) (cartridge.Session, error) {
	f.owner.specs = append(f.owner.specs, spec)
	pipe := cartridge.NewPipe(f.name, nil)
	pipe.Finish(cartridge.Result{Output: "fake"}, nil)
	return pipe, nil
}

func (f *fakeDriverCartridge) ToolPolicyEnforcement(policy cartridge.ToolPolicy) (agents.Enforcement, error) {
	if !policy.Valid() {
		return agents.Enforcement{}, fmt.Errorf("invalid policy %q", policy)
	}
	if !f.owner.native && policy != cartridge.ToolReadWrite {
		return agents.Enforcement{}, fmt.Errorf("no native controls")
	}
	return agents.Enforcement{
		Cartridge: f.name,
		Policy:    policy,
		Mode:      agents.EnforceNative,
		Controls:  []string{"--fake-policy", string(policy)},
	}, nil
}

func TestEnabledDriverIdentities(t *testing.T) {
	t.Parallel()
	for id, driver := range map[string]Driver{
		"agy": DriverAgy, "codex": DriverCodex, "kimi": DriverKimi,
		"grok": DriverGrok, "junie": DriverJunie, "hermes": DriverHermes, "kilo": DriverKilo,
	} {
		assert.Equal(t, id, string(driver))
		p, ok := providerByID(id)
		require.True(t, ok)
		assert.Equal(t, driver, p.Driver)
		capabilities, registered := builtinDrivers[driver]
		assert.True(t, registered, "%s has a verified factory", id)
		assert.NotNil(t, capabilities.factory)
		assert.True(t, capabilities.checkBinary)
		assert.NotEmpty(t, capabilities.binary)
	}
	p, ok := providerByID("codex-animus")
	require.True(t, ok)
	assert.Equal(t, DriverCodex, p.Driver, "codex-animus reuses Codex with an isolated CODEX_HOME")
	assert.NotContains(t, config.KnownCartridges, "codex-animus")
}

func TestDriverPolicyMatrix(t *testing.T) {
	t.Parallel()
	policies := cartridge.ToolPolicies()
	for _, p := range Providers {
		for _, policy := range policies {
			want := p.Driver == DriverClaude ||
				(policy == cartridge.ToolReadExec && (p.Driver == DriverCodex || p.Driver == DriverKimi || p.Driver == DriverGrok || p.Driver == DriverHermes || p.Driver == DriverKilo)) ||
				(policy == cartridge.ToolReadWrite && p.Driver != DriverNone)
			assert.Equal(t, want, p.allows(policy), "%s %s", p.ID, policy)
			got := allowedProviders([]Provider{p}, builtinDrivers, policy, func(Driver) bool { return true })
			if want {
				assert.Equal(t, []string{p.ID}, got, "%s %s", p.ID, policy)
			} else {
				assert.Empty(t, got, "%s %s", p.ID, policy)
			}
		}
	}
	for _, policy := range []cartridge.ToolPolicy{cartridge.ToolReadOnly, cartridge.ToolReadExec} {
		_, ok := (driverCapabilities{
			factory:  (&fakeDriver{}).factory,
			policies: map[cartridge.ToolPolicy]agents.EnforcementMode{policy: agents.EnforceNone},
		}).policyMode(policy)
		assert.False(t, ok, "no restriction cannot support %s", policy)
	}
	_, ok := (driverCapabilities{
		factory:  (&fakeDriver{}).factory,
		policies: map[cartridge.ToolPolicy]agents.EnforcementMode{cartridge.ToolReadOnly: agents.EnforceCopyWorktree},
	}).policyMode(cartridge.ToolReadOnly)
	assert.False(t, ok, "copied worktrees are not native Dandelion enforcement")
}

func TestFakeDriverProviderPoliciesAndStaleRoute(t *testing.T) {
	a, _, _ := newAdapter(t, config.Cartridge{})
	fake := &fakeDriver{native: true}
	driver := Driver("fake")
	provider := Provider{ID: "fake-account", Driver: driver}
	a.providers = []Provider{provider}
	a.drivers = map[Driver]driverCapabilities{
		driver: {
			factory: fake.factory,
			policies: map[cartridge.ToolPolicy]agents.EnforcementMode{
				cartridge.ToolReadOnly:  agents.EnforceNative,
				cartridge.ToolReadWrite: agents.EnforceNone,
			},
			detail: "fake accounts",
		},
	}

	readOnly := a.allowed(cartridge.ToolReadOnly)
	assert.Equal(t, []string{provider.ID}, readOnly)
	assert.Empty(t, a.allowed(cartridge.ToolReadExec))
	assert.Equal(t, []string{provider.ID}, a.allowed(cartridge.ToolReadWrite))
	assert.True(t, a.routeUsable(provider, readOnly, cartridge.ToolReadOnly))
	assert.False(t, a.routeUsable(provider, []string{provider.ID}, cartridge.ToolReadExec),
		"a cached route cannot change the driver's policy capabilities")

	en, err := a.ToolPolicyEnforcement(cartridge.ToolReadOnly)
	require.NoError(t, err)
	assert.Equal(t, agents.EnforceNative, en.Mode)
	assert.Equal(t, []string{"--fake-policy", "read_only"}, en.Controls)
	assert.Contains(t, en.Detail, "routed to fake accounts only")
	en, err = a.ToolPolicyEnforcement(cartridge.ToolReadWrite)
	require.NoError(t, err)
	assert.Equal(t, agents.EnforceNone, en.Mode)
	_, err = a.ToolPolicyEnforcement(cartridge.ToolReadExec)
	assert.Error(t, err)

	capabilities := a.drivers[driver]
	capabilities.checkBinary = true
	capabilities.binary = "sc-test-driver-not-on-path"
	a.drivers[driver] = capabilities
	assert.False(t, a.routeUsable(provider, readOnly, cartridge.ToolReadOnly),
		"a cached route is rejected when the delegated binary becomes unavailable")
	assert.Empty(t, a.allowed(cartridge.ToolReadOnly))
}

func TestToolPolicyEnforcementChecksEveryEligibleDriver(t *testing.T) {
	a, _, _ := newAdapter(t, config.Cartridge{})
	first := &fakeDriver{native: true}
	second := &fakeDriver{native: true}
	a.providers = []Provider{
		{ID: "first-account", Driver: "fake-first"},
		{ID: "second-account", Driver: "fake-second"},
	}
	a.drivers = map[Driver]driverCapabilities{
		"fake-first": {
			factory: first.factory, detail: "first fake",
			policies: map[cartridge.ToolPolicy]agents.EnforcementMode{cartridge.ToolReadOnly: agents.EnforceNative},
		},
		"fake-second": {
			factory: second.factory, detail: "second fake",
			policies: map[cartridge.ToolPolicy]agents.EnforcementMode{cartridge.ToolReadOnly: agents.EnforceNative},
		},
	}
	en, err := a.ToolPolicyEnforcement(cartridge.ToolReadOnly)
	require.NoError(t, err)
	assert.Equal(t, agents.EnforceNative, en.Mode)
	assert.Empty(t, en.Controls, "controls are route-specific when eligible drivers differ")
	assert.Contains(t, en.Detail, "first fake, second fake")
	assert.Len(t, first.configs, 1)
	assert.Len(t, second.configs, 1)
}

func TestFakeDriverRejectsUnverifiedNativeEnforcement(t *testing.T) {
	a, _, _ := newAdapter(t, config.Cartridge{})
	fake := &fakeDriver{native: false}
	driver := Driver("fake-unverified")
	a.providers = []Provider{{ID: "fake-account", Driver: driver}}
	a.drivers = map[Driver]driverCapabilities{
		driver: {
			factory: fake.factory,
			policies: map[cartridge.ToolPolicy]agents.EnforcementMode{
				cartridge.ToolReadOnly: agents.EnforceNative,
			},
		},
	}
	_, err := a.ToolPolicyEnforcement(cartridge.ToolReadOnly)
	assert.ErrorContains(t, err, "refused tool policy")
}

func TestFakeDriverRouteMappingAndDirectRun(t *testing.T) {
	a, _, _ := newAdapter(t, config.Cartridge{Env: map[string]string{"FROM_CONFIG": "yes"}})
	fake := &fakeDriver{native: true}
	driver := Driver("fake-codex")
	provider := Provider{ID: "fake-codex-account", Driver: driver}
	a.drivers[driver] = driverCapabilities{
		factory: fake.factory,
		mapRoute: func(request delegateRequest) (delegateMapping, error) {
			mapped, err := routeModel(request)
			if err != nil {
				return delegateMapping{}, err
			}
			mapped.cfg.Args = []string{"--reasoning", request.route.Effort}
			mapped.effort = request.route.Effort
			mapped.spec.Env = withChildEnv(mapped.spec.Env, map[string]string{"CODEX_HOME": "/accounts/animus"})
			return mapped, nil
		},
	}
	originalEnv := map[string]string{"KEEP": "yes", "CODEX_HOME": "/default"}
	run := spec(t, cartridge.ToolReadWrite, originalEnv)
	route := Route{Model: "routed-model", Effort: "high", Account: provider.ID}

	inner, effort, err := a.delegate(provider, route, &run)
	require.NoError(t, err)
	assert.Equal(t, "high", effort)
	assert.Equal(t, "routed-model", run.Model)
	assert.Equal(t, map[string]string{"KEEP": "yes", "CODEX_HOME": "/accounts/animus"}, run.Env)
	assert.Equal(t, map[string]string{"KEEP": "yes", "CODEX_HOME": "/default"}, originalEnv,
		"account isolation must not mutate the caller's environment")
	require.Len(t, fake.configs, 1)
	assert.Equal(t, "routed-model", fake.configs[0].Model)
	assert.Equal(t, []string{"--reasoning", "high"}, fake.configs[0].Args)
	assert.Equal(t, map[string]string{"FROM_CONFIG": "yes"}, fake.configs[0].Env)

	session, err := inner.Run(context.Background(), run)
	require.NoError(t, err)
	result, err := cartridge.Collect(session, nil)
	require.NoError(t, err)
	assert.Equal(t, "fake", result.Output)
	require.Len(t, fake.specs, 1)
	assert.Equal(t, run, fake.specs[0], "delegates use the ordinary Adapter.Run fallback")
}

func TestDefaultRouteMappingDropsEffort(t *testing.T) {
	a, _, _ := newAdapter(t, config.Cartridge{})
	fake := &fakeDriver{native: true}
	driver := Driver("fake-default")
	provider := Provider{ID: "fake-account", Driver: driver}
	a.drivers[driver] = driverCapabilities{factory: fake.factory}
	run := spec(t, cartridge.ToolReadWrite, nil)
	_, effort, err := a.delegate(provider, Route{Model: "routed", Effort: "max", Account: provider.ID}, &run)
	require.NoError(t, err)
	assert.Empty(t, effort)
	assert.Equal(t, "routed", run.Model)
	require.Len(t, fake.configs, 1)
	assert.Empty(t, fake.configs[0].Args)
}

func TestWithChildEnvCopiesAndOverrides(t *testing.T) {
	t.Parallel()
	assert.Nil(t, withChildEnv(nil, nil))
	base := map[string]string{"KEEP": "base", "CODEX_HOME": "/default"}
	overrides := map[string]string{"CODEX_HOME": "/isolated", "NEW": "value"}
	got := withChildEnv(base, overrides)
	assert.Equal(t, map[string]string{"KEEP": "base", "CODEX_HOME": "/isolated", "NEW": "value"}, got)
	base["KEEP"] = "changed"
	overrides["NEW"] = "changed"
	assert.Equal(t, "base", got["KEEP"])
	assert.Equal(t, "value", got["NEW"])
	got["CODEX_HOME"] = "/later"
	assert.Equal(t, "/default", base["CODEX_HOME"])
}

func delegatedArgs(t *testing.T, inner cartridge.Cartridge, run cartridge.RunSpec) []string {
	t.Helper()
	switch a := inner.(type) {
	case *agy.Adapter:
		return a.Args(run)
	case *codex.Adapter:
		return a.Args(run)
	case *kimi.Adapter:
		return a.Args(run, "profile", "input")
	case *grok.Adapter:
		return a.Args(run, "prompt")
	case *junie.Adapter:
		return a.Args(run, "extensions")
	case *hermes.Adapter:
		return a.Args(run)
	case *kilo.Adapter:
		return a.Args(run)
	default:
		t.Fatalf("unexpected delegated adapter %T", inner)
		return nil
	}
}

func TestRoutedEffortAndPolicyArgs(t *testing.T) {
	cases := []struct {
		account string
		policy  cartridge.ToolPolicy
		want    string
		wantEnv map[string]string
	}{
		{"agy", cartridge.ToolReadWrite, "--effort high --mode accept-edits --dangerously-skip-permissions", nil},
		{"codex", cartridge.ToolReadExec, "-c model_reasoning_effort=high", nil},
		{"kimi", cartridge.ToolReadExec, "--agent-file profile", map[string]string{"KIMI_MODEL_THINKING_EFFORT": "high"}},
		{"grok", cartridge.ToolReadExec, "--reasoning-effort high --permission-mode bypassPermissions", nil},
		{"junie", cartridge.ToolReadWrite, "--effort high --task", nil},
		{"hermes", cartridge.ToolReadExec, "--toolsets terminal --reasoning high", nil},
		{"kilo", cartridge.ToolReadExec, "run --variant high", map[string]string{"SC_KILO_MIN_BALANCE_USD": "10"}},
	}
	for _, tc := range cases {
		t.Run(tc.account, func(t *testing.T) {
			a, _, _ := newAdapter(t, config.Cartridge{
				Args: []string{"--sandbox", "danger-full-access", "--yolo"},
				Env:  map[string]string{"KIMI_MODEL_THINKING_EFFORT": "configured", "SC_KILO_MIN_BALANCE_USD": "999"},
			})
			p, ok := providerByID(tc.account)
			require.True(t, ok)
			run := spec(t, tc.policy, map[string]string{"KIMI_MODEL_THINKING_EFFORT": "spec", "SC_KILO_MIN_BALANCE_USD": "999"})
			inner, effort, err := a.delegate(p, Route{Model: "routed", Effort: "high", Account: tc.account}, &run)
			require.NoError(t, err)
			assert.Equal(t, "high", effort)
			assert.Equal(t, "routed", run.Model)
			args := delegatedArgs(t, inner, run)
			assert.Contains(t, strings.Join(args, " "), tc.want)
			assert.NotContains(t, args, "danger-full-access", "router args must not become delegated policy overrides")
			assert.NotContains(t, args, "--yolo", "router args must not become delegated policy overrides")
			for key, value := range tc.wantEnv {
				assert.Equal(t, value, run.Env[key])
			}
			switch tc.account {
			case "codex":
				assert.Equal(t, []string{"-c", "approval_policy=\"never\"", "--sandbox", "read-only"}, args[len(args)-4:])
			case "kimi":
				assert.Equal(t, "high", run.Env["KIMI_MODEL_THINKING_EFFORT"], "the route overrides configured and spec effort")
			}
		})
	}
}

func TestRoutedEffortUnsupportedIsDropped(t *testing.T) {
	for _, account := range []string{"agy", "codex", "kimi", "grok", "junie", "hermes", "kilo"} {
		t.Run(account, func(t *testing.T) {
			for _, effort := range []string{"turbo", "max"} {
				a, _, _ := newAdapter(t, config.Cartridge{})
				p, ok := providerByID(account)
				require.True(t, ok)
				run := spec(t, cartridge.ToolReadWrite, nil)
				inner, gotEffort, err := a.delegate(p, Route{Model: "routed", Effort: effort, Account: account}, &run)
				require.NoError(t, err, effort)
				if account != "junie" && effort == "max" {
					assert.Equal(t, "max", gotEffort)
					if account == "kilo" {
						assert.Contains(t, strings.Join(delegatedArgs(t, inner, run), " "), "--variant max")
					}
					continue
				}
				assert.Empty(t, gotEffort, effort)
				joined := strings.Join(delegatedArgs(t, inner, run), " ")
				assert.NotContains(t, joined, "turbo")
				assert.NotContains(t, joined, "model_reasoning_effort=max")
				assert.NotContains(t, joined, "--effort max")
				assert.NotContains(t, joined, "--reasoning-effort max")
				assert.NotContains(t, joined, "--reasoning max")
				assert.NotContains(t, run.Env["KIMI_MODEL_THINKING_EFFORT"], effort)
			}
		})
	}
}

func TestCodexAnimusHomeOverridesEveryOtherEnv(t *testing.T) {
	home := t.TempDir()
	a, _, _ := newAdapter(t, config.Cartridge{Env: map[string]string{"DANDELION_CODEX_ANIMUS_HOME": "/configured/animus"}})
	a.homeDir = func() (string, error) { return home, nil }
	p, ok := providerByID("codex-animus")
	require.True(t, ok)
	original := map[string]string{"CODEX_HOME": "/personal", "KEEP": "yes"}
	run := spec(t, cartridge.ToolReadWrite, original)
	inner, _, err := a.delegate(p, Route{Model: "routed", Effort: "high", Account: p.ID}, &run)
	require.NoError(t, err)
	assert.Equal(t, "/configured/animus", run.Env["CODEX_HOME"])
	assert.Equal(t, "yes", run.Env["KEEP"])
	assert.Equal(t, "/personal", original["CODEX_HOME"], "the caller's map is not mutated")
	assert.Contains(t, strings.Join(delegatedArgs(t, inner, run), " "), "model_reasoning_effort=high")

	a, _, _ = newAdapter(t, config.Cartridge{Env: map[string]string{"DANDELION_CODEX_ANIMUS_HOME": ""}})
	a.homeDir = func() (string, error) { return home, nil }
	run = spec(t, cartridge.ToolReadWrite, map[string]string{"CODEX_HOME": "/personal"})
	_, _, err = a.delegate(p, Route{Model: "routed", Account: p.ID}, &run)
	require.NoError(t, err)
	assert.Equal(t, home+"/.codex-animus", run.Env["CODEX_HOME"])

	personal, ok := providerByID("codex")
	require.True(t, ok)
	run = spec(t, cartridge.ToolReadWrite, map[string]string{"CODEX_HOME": "/personal"})
	_, _, err = a.delegate(personal, Route{Model: "routed", Account: personal.ID}, &run)
	require.NoError(t, err)
	assert.Equal(t, "/personal", run.Env["CODEX_HOME"])
}

func TestProviderPolicyAvailabilityAndFinalRejectionAgree(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	shims := map[string]*testutil.Shim{}
	for _, capabilities := range builtinDrivers {
		if capabilities.checkBinary {
			shims[capabilities.binary] = testutil.FakeBin(t, capabilities.binary, "exit 0")
		}
	}
	a, _, _ := newAdapter(t, config.Cartridge{})
	for _, p := range Providers {
		for _, policy := range cartridge.ToolPolicies() {
			allowed := a.allowed(policy)
			inAllowed := slices.Contains(allowed, p.ID)
			assert.Equal(t, p.allows(policy), inAllowed, "%s %s", p.ID, policy)
			assert.Equal(t, inAllowed, a.routeUsable(p, []string{p.ID}, policy), "%s %s final rejection", p.ID, policy)
		}
	}

	grokProvider, ok := providerByID("grok")
	require.True(t, ok)
	allowed := a.allowed(cartridge.ToolReadExec)
	require.Contains(t, allowed, "grok")
	require.NoError(t, os.Remove(shims[grok.Name].Path))
	assert.False(t, a.routeUsable(grokProvider, allowed, cartridge.ToolReadExec),
		"a cached route is rejected after the real driver binary disappears")
}
