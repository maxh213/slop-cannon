package dandelion

import (
	"fmt"
	"log/slog"
	"slices"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/agy"
	"slop-cannon/internal/cartridge/claude"
	"slop-cannon/internal/cartridge/codex"
	"slop-cannon/internal/cartridge/cursor"
	"slop-cannon/internal/cartridge/grok"
	"slop-cannon/internal/cartridge/hermes"
	"slop-cannon/internal/cartridge/junie"
	"slop-cannon/internal/cartridge/kilo"
	"slop-cannon/internal/cartridge/kimi"
	"slop-cannon/internal/config"
	"slop-cannon/internal/proc"
)

// driverCapabilities is the complete integration contract for a delegated
// CLI. factory is the adapter's ordinary cartridge.Factory. policies declares
// only policies the adapter can hold: native restriction, or none for an
// unrestricted read_write firing. checkBinary removes its providers from
// routing when binary is absent. mapRoute customizes the routed model, effort
// and account environment; nil uses routeModel.
type driverCapabilities struct {
	binary      string
	checkBinary bool
	factory     cartridge.Factory
	policies    map[cartridge.ToolPolicy]agents.EnforcementMode
	detail      string
	mapRoute    routeMapper
}

// builtinDrivers is the one dispatch table. Restricted policies appear only
// where the delegated adapter has exact native controls. agy and Grok
// read_only stay absent because their installed-CLI canaries failed.
var builtinDrivers = map[Driver]driverCapabilities{
	DriverClaude: {
		binary:  claude.Name,
		factory: claude.New,
		policies: map[cartridge.ToolPolicy]agents.EnforcementMode{
			cartridge.ToolReadOnly:  agents.EnforceNative,
			cartridge.ToolReadExec:  agents.EnforceNative,
			cartridge.ToolReadWrite: agents.EnforceNone,
		},
		detail:   "Claude Code accounts",
		mapRoute: claudeRoute,
	},
	DriverCursor: {
		binary:      cursor.DefaultBinary,
		checkBinary: true,
		factory:     cursor.New,
		policies: map[cartridge.ToolPolicy]agents.EnforcementMode{
			cartridge.ToolReadWrite: agents.EnforceNone,
		},
		detail: "Cursor Agent",
	},
	DriverAgy: {
		binary: agy.Name, checkBinary: true, factory: agy.New,
		policies: map[cartridge.ToolPolicy]agents.EnforcementMode{
			cartridge.ToolReadWrite: agents.EnforceNone,
		},
		detail: "agy", mapRoute: argumentRoute("--effort", Efforts),
	},
	DriverCodex: {
		binary: codex.Name, checkBinary: true, factory: codex.New,
		policies: map[cartridge.ToolPolicy]agents.EnforcementMode{
			cartridge.ToolReadExec:  agents.EnforceNative,
			cartridge.ToolReadWrite: agents.EnforceNone,
		},
		detail: "Codex accounts", mapRoute: codexRoute,
	},
	DriverKimi: {
		binary: kimi.Name, checkBinary: true, factory: kimi.New,
		policies: map[cartridge.ToolPolicy]agents.EnforcementMode{
			cartridge.ToolReadExec:  agents.EnforceNative,
			cartridge.ToolReadWrite: agents.EnforceNone,
		},
		detail: "Kimi Code", mapRoute: kimiRoute,
	},
	DriverGrok: {
		binary: grok.Name, checkBinary: true, factory: grok.New,
		policies: map[cartridge.ToolPolicy]agents.EnforcementMode{
			cartridge.ToolReadExec:  agents.EnforceNative,
			cartridge.ToolReadWrite: agents.EnforceNone,
		},
		detail: "Grok Build", mapRoute: argumentRoute("--reasoning-effort", Efforts),
	},
	DriverJunie: {
		binary: junie.DefaultBinary, checkBinary: true, factory: junie.New,
		policies: map[cartridge.ToolPolicy]agents.EnforcementMode{
			cartridge.ToolReadWrite: agents.EnforceNone,
		},
		detail: "Junie", mapRoute: argumentRoute("--effort", junie.Efforts),
	},
	DriverHermes: {
		binary: hermes.Name, checkBinary: true, factory: hermes.New,
		policies: map[cartridge.ToolPolicy]agents.EnforcementMode{
			cartridge.ToolReadExec:  agents.EnforceNative,
			cartridge.ToolReadWrite: agents.EnforceNone,
		},
		detail: "Hermes Agent", mapRoute: argumentRoute("--reasoning", Efforts),
	},
	DriverKilo: {
		binary: kilo.Name, checkBinary: true, factory: kilo.New,
		policies: map[cartridge.ToolPolicy]agents.EnforcementMode{
			cartridge.ToolReadExec:  agents.EnforceNative,
			cartridge.ToolReadWrite: agents.EnforceNone,
		},
		detail: "Kilo", mapRoute: kiloRoute,
	},
}

func cloneDrivers() map[Driver]driverCapabilities {
	out := make(map[Driver]driverCapabilities, len(builtinDrivers))
	for d, capabilities := range builtinDrivers {
		out[d] = capabilities
	}
	return out
}

// policyMode returns a declared, constructible enforcement mode. EnforceNone
// is never sufficient for a restricted policy, even if a table entry says so.
func (c driverCapabilities) policyMode(policy cartridge.ToolPolicy) (agents.EnforcementMode, bool) {
	if c.factory == nil {
		return "", false
	}
	mode, ok := c.policies[policy]
	if !ok {
		return "", false
	}
	switch mode {
	case agents.EnforceNative:
		return mode, true
	case agents.EnforceNone:
		return mode, policy == cartridge.ToolReadWrite
	default:
		return "", false
	}
}

func driverAllows(drivers map[Driver]driverCapabilities, driver Driver, policy cartridge.ToolPolicy) bool {
	capabilities, ok := drivers[driver]
	if !ok {
		return false
	}
	_, ok = capabilities.policyMode(policy)
	return ok
}

func driverAvailable(drivers map[Driver]driverCapabilities, driver Driver) bool {
	capabilities, ok := drivers[driver]
	if !ok || !capabilities.checkBinary {
		return ok
	}
	if capabilities.binary == "" {
		return false
	}
	_, err := proc.LookPath(capabilities.binary)
	return err == nil
}

func allowedProviders(providers []Provider, drivers map[Driver]driverCapabilities, policy cartridge.ToolPolicy, available func(Driver) bool) []string {
	var out []string
	for _, p := range providers {
		if driverAllows(drivers, p.Driver, policy) && available(p.Driver) {
			out = append(out, p.ID)
		}
	}
	return out
}

// delegateRequest is one routed firing before its driver maps the route.
type delegateRequest struct {
	provider  Provider
	route     Route
	spec      cartridge.RunSpec
	env       map[string]string
	lookupEnv func(string) (string, bool)
	homeDir   func() (string, error)
	logger    *slog.Logger
}

// delegateMapping is the adapter configuration and run spec to launch. effort
// is retained for route logging even when it is represented by CLI-specific
// configuration rather than a common flag.
type delegateMapping struct {
	cfg    config.Cartridge
	spec   cartridge.RunSpec
	effort string
}

type routeMapper func(delegateRequest) (delegateMapping, error)

// routeModel is the default model mapping: the routed model wins, effort is
// not passed to a CLI that has not declared how to accept it, and the router's
// configured environment reaches the delegated adapter.
func routeModel(request delegateRequest) (delegateMapping, error) {
	request.spec.Model = request.route.Model
	return delegateMapping{
		cfg:  config.Cartridge{Model: request.route.Model, Env: request.env},
		spec: request.spec,
	}, nil
}

// argumentRoute passes a supported routed effort as flag/value. Unknown or
// empty efforts add no arguments; in particular Junie's xhigh and max routes
// are never rewritten to a different effort.
func argumentRoute(flag string, efforts []string) routeMapper {
	return func(request delegateRequest) (delegateMapping, error) {
		return mapEffort(request, efforts, func(mapped *delegateMapping, effort string) {
			mapped.cfg.Args = []string{flag, effort}
		})
	}
}

func mapEffort(request delegateRequest, efforts []string, apply func(*delegateMapping, string)) (delegateMapping, error) {
	mapped, err := routeModel(request)
	if err != nil {
		return delegateMapping{}, err
	}
	effort := request.route.Effort
	if !slices.Contains(efforts, effort) {
		if effort != "" {
			request.logger.Warn("dandelion route has an effort the delegated CLI does not accept; dropped",
				slog.String("driver", string(request.provider.Driver)), slog.String("route", request.route.String()))
		}
		return mapped, nil
	}
	apply(&mapped, effort)
	mapped.effort = effort
	return mapped, nil
}

func codexRoute(request delegateRequest) (delegateMapping, error) {
	mapped, err := mapEffort(request, Efforts, func(mapped *delegateMapping, effort string) {
		mapped.cfg.Args = []string{"-c", "model_reasoning_effort=" + effort}
	})
	if err != nil || request.provider.ID != codex.AnimusName {
		return mapped, err
	}
	home := request.provider
	home.ConfigDirEnv = "DANDELION_CODEX_ANIMUS_HOME"
	home.ConfigDirFallback = ".codex-animus"
	dir, err := home.configDir(request.lookupEnv, request.homeDir)
	if err != nil {
		return delegateMapping{}, err
	}
	mapped.spec.Env = withChildEnv(mapped.spec.Env, map[string]string{"CODEX_HOME": dir})
	return mapped, nil
}

func kimiRoute(request delegateRequest) (delegateMapping, error) {
	return mapEffort(request, Efforts, func(mapped *delegateMapping, effort string) {
		mapped.spec.Env = withChildEnv(mapped.spec.Env, map[string]string{"KIMI_MODEL_THINKING_EFFORT": effort})
	})
}

func kiloRoute(request delegateRequest) (delegateMapping, error) {
	mapped, err := argumentRoute("--variant", Efforts)(request)
	if err != nil {
		return delegateMapping{}, err
	}
	mapped.spec.Env = withChildEnv(mapped.spec.Env, map[string]string{"SC_KILO_MIN_BALANCE_USD": "10"})
	return mapped, nil
}

func claudeRoute(request delegateRequest) (delegateMapping, error) {
	mapped, err := routeModel(request)
	if err != nil {
		return delegateMapping{}, err
	}
	dir, err := request.provider.configDir(request.lookupEnv, request.homeDir)
	if err != nil {
		return delegateMapping{}, err
	}
	if dir != "" {
		mapped.spec.Env = withChildEnv(mapped.spec.Env, map[string]string{"CLAUDE_CONFIG_DIR": dir})
	}
	mapped.effort = request.route.Effort
	if slices.Contains(Efforts, mapped.effort) {
		mapped.cfg.Args = []string{"--effort", mapped.effort}
		return mapped, nil
	}
	if mapped.effort != "" {
		request.logger.Warn("dandelion route has an effort Claude Code does not accept; dropped",
			slog.String("route", request.route.String()))
	}
	mapped.effort = ""
	return mapped, nil
}

// withChildEnv returns a new environment with overrides applied. It neither
// mutates nor retains base or overrides, so account-specific homes such as
// CLAUDE_CONFIG_DIR and CODEX_HOME cannot leak into another firing.
func withChildEnv(base, overrides map[string]string) map[string]string {
	if base == nil && len(overrides) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(overrides))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range overrides {
		out[key] = value
	}
	return out
}

func (a *Adapter) delegate(p Provider, r Route, spec *cartridge.RunSpec) (cartridge.Cartridge, string, error) {
	capabilities, ok := a.drivers[p.Driver]
	if !ok || capabilities.factory == nil {
		return nil, "", fmt.Errorf("dandelion has no factory for driver %q", p.Driver)
	}
	request := delegateRequest{
		provider: p, route: r, spec: *spec, env: a.cfg.Env,
		lookupEnv: a.lookupEnv, homeDir: a.homeDir, logger: a.log(),
	}
	mapper := capabilities.mapRoute
	if mapper == nil {
		mapper = routeModel
	}
	mapped, err := mapper(request)
	if err != nil {
		return nil, "", cartridge.NewError(cartridge.KindCrash, a.name, "", err)
	}
	inner, err := capabilities.factory(a.name, mapped.cfg)
	if err != nil {
		return nil, "", fmt.Errorf("building dandelion driver %q: %w", p.Driver, err)
	}
	if inner == nil {
		return nil, "", fmt.Errorf("building dandelion driver %q: factory returned nil", p.Driver)
	}
	*spec = mapped.spec
	return inner, mapped.effort, nil
}
