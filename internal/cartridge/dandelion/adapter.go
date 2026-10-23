package dandelion

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/claude"
	"slop-cannon/internal/cartridge/cursor"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/proc"
)

// Name is the registry name the adapter registers under.
const Name = "dandelion"

// DefaultBinary is the router the adapter runs when the configuration names
// no command.
const DefaultBinary = "dandelion"

// Mode is which of the router's two answers a firing asks for.
type Mode string

// Modes, selected by [cartridges.dandelion] model (config.DandelionModes).
const (
	// ModeStandard is "dandelion route": the subscription to use right now.
	ModeStandard Mode = "standard"
	// ModeHigh is "dandelion route --high": the strongest model that still
	// has quota.
	ModeHigh Mode = "high"
)

// ParseMode reads a configured model as a Mode: "high", or "standard" and
// empty for the plain route, trimmed and case-insensitive. Anything else is
// the config.ValidateCartridgeModel error.
func ParseMode(model string) (Mode, error) {
	if err := config.ValidateCartridgeModel(Name, model); err != nil {
		return "", err
	}
	if strings.EqualFold(strings.TrimSpace(model), string(ModeHigh)) {
		return ModeHigh, nil
	}
	return ModeStandard, nil
}

// Timing.
const (
	// CacheTTL is how long one router answer serves every firing with the
	// same mode and account set. It is also the --max-age passed to the
	// router, so a running dandelion dashboard's snapshot can answer
	// without probing. Each probe round starts every provider's CLI and
	// takes about six seconds.
	CacheTTL = 2 * time.Minute
	// RouteTimeout bounds one router call.
	RouteTimeout = 90 * time.Second
	// NoRouteRetryAfter is the pause asked for when no account has quota.
	NoRouteRetryAfter = 5 * time.Minute
	// AccountRetryAfter is the pause asked for when the account a firing
	// was routed to runs out of quota or is rate limited. It is short
	// because the supervisor pauses the whole cartridge by name, and the
	// next route most likely names another account.
	AccountRetryAfter = time.Minute
)

// Adapter is the dandelion cartridge: every firing asks the router which
// account to use and delegates to the Claude Code or Cursor Agent adapter
// built under this cartridge's name. Build it through the registry
// (cartridge.New("dandelion", cfg)) or New; the zero value is not usable.
type Adapter struct {
	name string
	cfg  config.Cartridge
	mode Mode

	// Seams for tests.
	now       func() time.Time
	lookupEnv func(string) (string, bool)
	homeDir   func() (string, error)
	logger    *slog.Logger

	mu      sync.Mutex
	cache   map[routeKey]cachedRoute
	flights map[routeKey]*flight
	fresh   map[routeKey]bool
}

var (
	_ cartridge.Cartridge       = (*Adapter)(nil)
	_ agents.ToolPolicyEnforcer = (*Adapter)(nil)
)

func init() {
	cartridge.MustRegister(Name, New)
}

// New is the cartridge.Factory for dandelion. name is the registry name;
// cfg.Model selects the mode (ParseMode), cfg.Command the router binary,
// cfg.Args extra router arguments and cfg.Env variables for the router and
// the delegated CLI. It does no I/O.
func New(name string, cfg config.Cartridge) (cartridge.Cartridge, error) {
	if strings.TrimSpace(name) == "" {
		name = Name
	}
	mode, err := ParseMode(cfg.Model)
	if err != nil {
		return nil, err
	}
	a := &Adapter{
		name: name, cfg: cfg, mode: mode,
		now: time.Now, homeDir: os.UserHomeDir,
		cache: map[routeKey]cachedRoute{}, flights: map[routeKey]*flight{}, fresh: map[routeKey]bool{},
	}
	a.lookupEnv = a.envLookup
	return a, nil
}

// Name implements cartridge.Cartridge.
func (a *Adapter) Name() string { return a.name }

// Mode returns the configured mode.
func (a *Adapter) Mode() Mode { return a.mode }

// Binary returns the router executable: the configured Command, else
// "dandelion".
func (a *Adapter) Binary() string { return a.cfg.Binary(Name) }

// envLookup reads a variable from the configured env, then sc's own.
func (a *Adapter) envLookup(name string) (string, bool) {
	if v, ok := a.cfg.Env[name]; ok {
		return v, true
	}
	return os.LookupEnv(name)
}

func (a *Adapter) log() *slog.Logger {
	if a.logger != nil {
		return a.logger
	}
	return slog.Default()
}

// claudeAdapter builds a Claude Code adapter under this cartridge's name,
// so its errors and events say "dandelion".
func (a *Adapter) claudeAdapter(cfg config.Cartridge) cartridge.Cartridge {
	c, _ := claude.New(a.name, cfg) // never fails
	return c
}

// Detect implements cartridge.Cartridge: it resolves the router binary and
// then detects Claude Code, which every read_only and read_exec firing (the
// product owner, the testers) is routed to. It does not ask the router for
// a route: a probe round starts every provider's CLI. Status.Version is
// Claude Code's, and Detail says the mode and the accounts sc can drive.
func (a *Adapter) Detect(ctx context.Context) (cartridge.Status, error) {
	var st cartridge.Status
	bin := a.Binary()
	path, err := proc.LookPath(bin)
	if err != nil {
		st.Detail = fmt.Sprintf("%s not found on PATH", bin)
		return st, cartridge.NewError(cartridge.KindNotInstalled, a.name, st.Detail, err)
	}
	cst, cerr := a.claudeAdapter(config.Cartridge{Env: a.cfg.Env}).Detect(ctx)
	st = cst
	st.Path = path
	st.Detail = a.describe() + "; claude: " + cst.Detail
	return st, cerr
}

// describe is the Status detail: the mode and the accounts sc can drive.
func (a *Adapter) describe() string {
	route := "dandelion route"
	if a.mode == ModeHigh {
		route += " --high"
	}
	var claudes []string
	for _, p := range Providers {
		if p.Driver == DriverClaude {
			claudes = append(claudes, p.ID)
		}
	}
	s := fmt.Sprintf("%s over %s (Claude Code)", route, strings.Join(claudes, ", "))
	if cursorInstalled() {
		return s + " and cursor (Cursor Agent, read_write firings only)"
	}
	return s + "; cursor-agent is not on PATH, so cursor is never routed"
}

// cursorInstalled reports whether the Cursor Agent CLI is on PATH.
func cursorInstalled() bool {
	_, err := proc.LookPath(config.Cartridge{}.Binary(cursor.Name))
	return err == nil
}

// ToolPolicyEnforcement implements agents.ToolPolicyEnforcer. read_only
// and read_exec firings are only ever routed to Claude accounts (Run
// disables cursor for them and refuses a route that names anything else),
// so they are held natively, exactly as the Claude adapter holds them;
// this is what lets a planning product owner, which has no worktree to
// copy, fire on dandelion. read_write may land on Cursor too and needs no
// restriction.
func (a *Adapter) ToolPolicyEnforcement(p cartridge.ToolPolicy) (agents.Enforcement, error) {
	if !p.Valid() {
		return agents.Enforcement{}, fmt.Errorf("%w: tool policy %q", domain.ErrUnknownValue, string(p))
	}
	if p == cartridge.ToolReadWrite {
		return agents.Enforcement{
			Cartridge: a.name, Policy: p, Mode: agents.EnforceNone,
			Detail: "nothing to restrict; routed to a Claude Code account or to Cursor Agent",
		}, nil
	}
	inner, ok := a.claudeAdapter(config.Cartridge{}).(agents.ToolPolicyEnforcer)
	if !ok {
		return agents.Enforcement{}, fmt.Errorf("cartridge %s: the Claude Code adapter reports no tool-policy enforcement", a.name)
	}
	en, err := inner.ToolPolicyEnforcement(p)
	if err != nil {
		return agents.Enforcement{}, err
	}
	en.Cartridge = a.name
	en.Detail = "routed to Claude Code accounts only; " + en.Detail
	return en, nil
}

// fired is what one firing was routed to, for the outcome translation.
type fired struct {
	key   routeKey
	route Route
}

// Run implements cartridge.Cartridge: it asks the router for an account the
// firing's tool policy allows (see Allowed), then starts the session on it
// through the Claude Code or Cursor Agent adapter with the routed model.
// spec.Model is ignored: the router's model wins. A route naming an account
// the policy does not allow is treated as no route at all.
func (a *Adapter) Run(ctx context.Context, spec cartridge.RunSpec) (cartridge.Session, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	allowed := Allowed(spec.ToolPolicy, cursorInstalled())
	key := routeKey{mode: a.mode, allowed: strings.Join(allowed, ",")}
	user, _ := a.lookupEnv(disableEnv)
	r, cached, err := a.route(ctx, key, Disabled(allowed, user))
	if err != nil {
		return nil, err
	}
	p, known := providerByID(r.Account)
	if !known || !slices.Contains(allowed, r.Account) {
		a.invalidate(key, r)
		msg := fmt.Sprintf("dandelion routed to %q (%s), which is not one sc can drive", r.Account, r)
		switch {
		case known && p.allows(spec.ToolPolicy):
			msg = fmt.Sprintf("dandelion routed to %q (%s), which needs cursor-agent on PATH", r.Account, r)
		case known && p.Driver != DriverNone:
			msg = fmt.Sprintf("dandelion routed to %q (%s), which cannot hold %s", r.Account, r, spec.ToolPolicy)
		}
		return nil, a.noRoute(msg)
	}

	inner, effort, err := a.delegate(p, r, &spec)
	if err != nil {
		return nil, err
	}
	a.log().Info("dandelion routed a firing",
		slog.String("agent", spec.Labels.Get(cartridge.LabelAgent)),
		slog.String("account", r.Account), slog.String("model", r.Model), slog.String("effort", effort),
		slog.String("mode", string(a.mode)), slog.String("policy", string(spec.ToolPolicy)),
		slog.Bool("cached", cached))

	s, err := inner.Run(ctx, spec)
	if err != nil {
		return nil, a.translate(err, fired{key, r})
	}
	return a.watch(s, fired{key, r}), nil
}

// delegate builds the adapter for p and points spec at the routed model
// and, for a Claude account, its config dir. effort is the --effort value
// passed on, empty when the line had none or one Claude Code does not
// accept.
func (a *Adapter) delegate(p Provider, r Route, spec *cartridge.RunSpec) (cartridge.Cartridge, string, error) {
	spec.Model = r.Model
	switch p.Driver {
	case DriverCursor:
		c, _ := cursor.New(a.name, config.Cartridge{Model: r.Model, Env: a.cfg.Env}) // never fails
		return c, "", nil
	default:
		dir, err := p.configDir(a.lookupEnv, a.homeDir)
		if err != nil {
			return nil, "", cartridge.NewError(cartridge.KindCrash, a.name, "", err)
		}
		if dir != "" {
			env := make(map[string]string, len(spec.Env)+1)
			for k, v := range spec.Env {
				env[k] = v
			}
			env["CLAUDE_CONFIG_DIR"] = dir
			spec.Env = env
		}
		var args []string
		effort := r.Effort
		if slices.Contains(Efforts, effort) {
			args = []string{"--effort", effort}
		} else {
			if effort != "" {
				a.log().Warn("dandelion route has an effort Claude Code does not accept; dropped",
					slog.String("route", r.String()))
			}
			effort = ""
		}
		return a.claudeAdapter(config.Cartridge{Model: r.Model, Args: args, Env: a.cfg.Env}), effort, nil
	}
}

// watch relays the delegated session, translating its failure.
func (a *Adapter) watch(s cartridge.Session, f fired) cartridge.Session {
	pipe := cartridge.NewPipe(a.name, s.Kill)
	go func() {
		for ev := range s.Events() {
			if ev.Kind == cartridge.EventError {
				ev.Err = a.relabel(ev.Err, f)
			}
			pipe.Emit(ev)
		}
		res, err := s.Wait()
		pipe.Finish(res, a.translate(err, f))
	}()
	return pipe
}

// translate is relabel plus its side effects: a quota failure drops the
// cached route so the next firing asks the router again.
func (a *Adapter) translate(err error, f fired) error {
	if err == nil {
		return nil
	}
	switch cartridge.KindOf(err) {
	case cartridge.KindCreditsExhausted, cartridge.KindRateLimited:
		a.invalidate(f.key, f.route)
		a.log().Warn("dandelion account out of quota; the next firing is routed again",
			slog.String("account", f.route.Account), slog.String("model", f.route.Model), slog.Any("err", err))
	}
	return a.relabel(err, f)
}

// relabel names the account in a delegated failure and turns an account's
// exhausted credits into a short rate limit. The supervisor retires a
// cartridge whose credits are exhausted for the rest of the run, by name:
// one empty account must not take every other account behind the router
// with it. A rate limit is shortened to AccountRetryAfter for the same
// reason; when every account is out the next route says so ("none", a
// NoRouteRetryAfter pause).
func (a *Adapter) relabel(err error, f fired) error {
	if err == nil {
		return nil
	}
	e := *cartridge.AsError(a.name, err)
	prefix := fmt.Sprintf("account %s (%s)", f.route.Account, f.route.Model)
	switch e.Kind {
	case cartridge.KindCreditsExhausted:
		e.Kind = cartridge.KindRateLimited
		e.Msg = joinMsg(prefix+" is out of credits; routing again", e.Msg)
		e.RetryAfter = AccountRetryAfter
	case cartridge.KindRateLimited:
		e.Msg = joinMsg(prefix+" is rate limited; routing again", e.Msg)
		e.RetryAfter = AccountRetryAfter
	default:
		e.Msg = joinMsg(prefix, e.Msg)
	}
	return &e
}

func joinMsg(a, b string) string {
	if b == "" {
		return a
	}
	return a + ": " + b
}
