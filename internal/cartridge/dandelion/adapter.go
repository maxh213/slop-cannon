package dandelion

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/agy"
	"slop-cannon/internal/cartridge/claude"
	"slop-cannon/internal/cartridge/kilo"
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
	// AccountRetryAfter is the longest pause asked for when the account a
	// firing was routed to runs out of quota, is rate limited or is logged
	// out. It is short because the supervisor pauses the whole cartridge by
	// name, and the account is benched, so the next route names another.
	AccountRetryAfter = time.Minute
	// BenchFor is how long an account that failed a firing is kept out of
	// routing when the failure gave no reset time of its own.
	BenchFor = 5 * time.Minute
)

// Adapter is the dandelion cartridge: every firing asks the router which
// account to use and delegates to its registered driver under this
// cartridge's name. Build it through the registry
// (cartridge.New("dandelion", cfg)) or New; the zero value is not usable.
type Adapter struct {
	name      string
	cfg       config.Cartridge
	mode      Mode
	providers []Provider
	drivers   map[Driver]driverCapabilities

	// Seams for tests.
	now       func() time.Time
	lookupEnv func(string) (string, bool)
	homeDir   func() (string, error)
	logger    *slog.Logger

	mu      sync.Mutex
	cache   map[routeKey]cachedRoute
	flights map[routeKey]*flight
	// epoch counts invalidations; clean[key] is the epoch at the start of
	// key's last successful fresh router call. A key whose clean epoch is
	// behind is probed afresh.
	epoch uint64
	clean map[routeKey]uint64
	// benched maps an account that failed a firing to when it may be
	// routed to again: until then it is left out of the allowed set, so
	// the router is told to skip it even when its own probe disagrees.
	benched map[string]time.Time
}

var (
	_ cartridge.Cartridge       = (*Adapter)(nil)
	_ agents.ToolPolicyEnforcer = (*Adapter)(nil)
)

func init() {
	cartridge.MustRegister(Name, New)
	// agy is the one delegated adapter without self-registration.
	cartridge.MustRegister(agy.Name, agy.New)
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
		providers: append([]Provider(nil), Providers...), drivers: cloneDrivers(),
		now: time.Now, homeDir: os.UserHomeDir,
		cache: map[routeKey]cachedRoute{}, flights: map[routeKey]*flight{},
		clean: map[routeKey]uint64{}, benched: map[string]time.Time{},
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
// detects Claude Code as the baseline adapter available to every restricted
// policy. Other delegated binaries are checked when the allowed route set is
// built. Detect does not ask the router for a route: a probe round starts
// every provider's CLI. Status.Version is Claude Code's, and Detail says the
// mode and the baseline accounts sc can drive.
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
	return driverAvailable(builtinDrivers, DriverCursor)
}

func (a *Adapter) allowed(policy cartridge.ToolPolicy) []string {
	return allowedProviders(a.providers, a.drivers, policy, func(driver Driver) bool {
		return driverAvailable(a.drivers, driver)
	})
}

func (a *Adapter) providerByID(id string) (Provider, bool) {
	for _, p := range a.providers {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

// routeUsable repeats the capability checks after routing. In particular, a
// cached route cannot survive a policy or binary-availability change merely
// because the router printed it before that change.
func (a *Adapter) routeUsable(p Provider, allowed []string, policy cartridge.ToolPolicy) bool {
	return slices.Contains(allowed, p.ID) &&
		driverAllows(a.drivers, p.Driver, policy) && driverAvailable(a.drivers, p.Driver)
}

type policyDriver struct {
	driver       Driver
	capabilities driverCapabilities
}

// policyDrivers is the eligible driver set shared conceptually by routing and
// enforcement, deduplicated when several accounts use the same adapter.
func (a *Adapter) policyDrivers(policy cartridge.ToolPolicy) []policyDriver {
	var out []policyDriver
	seen := map[Driver]bool{}
	for _, provider := range a.providers {
		if seen[provider.Driver] {
			continue
		}
		capabilities := a.drivers[provider.Driver]
		if _, ok := capabilities.policyMode(policy); !ok {
			continue
		}
		seen[provider.Driver] = true
		out = append(out, policyDriver{driver: provider.Driver, capabilities: capabilities})
	}
	return out
}

// ToolPolicyEnforcement resolves the same driver policy declarations used by
// Allowed and routeUsable. Every driver eligible for a restricted policy must
// confirm native enforcement; EnforceNone is accepted only for read_write.
func (a *Adapter) ToolPolicyEnforcement(p cartridge.ToolPolicy) (agents.Enforcement, error) {
	if !p.Valid() {
		return agents.Enforcement{}, fmt.Errorf("%w: tool policy %q", domain.ErrUnknownValue, string(p))
	}
	drivers := a.policyDrivers(p)
	if len(drivers) == 0 {
		return agents.Enforcement{}, fmt.Errorf("cartridge %s: no routed driver can enforce %s", a.name, p)
	}
	details := make([]string, 0, len(drivers))
	for _, driver := range drivers {
		detail := driver.capabilities.detail
		if detail == "" {
			detail = string(driver.driver)
		}
		details = append(details, detail)
	}
	if p == cartridge.ToolReadWrite {
		return agents.Enforcement{
			Cartridge: a.name, Policy: p, Mode: agents.EnforceNone,
			Detail: "nothing to restrict; routed to " + strings.Join(details, ", "),
		}, nil
	}

	var aggregate agents.Enforcement
	for _, driver := range drivers {
		inner, err := driver.capabilities.factory(a.name, config.Cartridge{Env: a.cfg.Env})
		if err != nil {
			return agents.Enforcement{}, fmt.Errorf("cartridge %s: building driver %q for tool-policy enforcement: %w", a.name, driver.driver, err)
		}
		if inner == nil {
			return agents.Enforcement{}, fmt.Errorf("cartridge %s: driver %q returned no adapter for tool-policy enforcement", a.name, driver.driver)
		}
		en, err := agents.EnforcementFor(inner, p)
		if err != nil {
			return agents.Enforcement{}, err
		}
		if en.Mode != agents.EnforceNative || en.Check() != nil {
			return agents.Enforcement{}, fmt.Errorf("cartridge %s: driver %q does not natively enforce %s", a.name, driver.driver, p)
		}
		if aggregate.Mode == "" {
			aggregate = en
		}
	}
	aggregate.Cartridge = a.name
	if len(drivers) == 1 {
		aggregate.Detail = "routed to " + details[0] + " only; " + aggregate.Detail
	} else {
		aggregate.Controls = nil
		aggregate.Detail = fmt.Sprintf("every routed driver enforces %s natively (%s)", p, strings.Join(details, ", "))
	}
	return aggregate, nil
}

// fired is what one firing was routed to, for the outcome translation.
type fired struct {
	route Route
}

// Run implements cartridge.Cartridge: it asks the router for an account the
// firing's tool policy and installed drivers allow (see Allowed), then starts
// the session through that account's adapter with the routed model and effort.
// spec.Model selects a route mode only when it is standard or high, trimmed
// and case-insensitively; every other value keeps the configured mode. The
// routed model still replaces spec.Model before delegation. Kilo additionally
// requires fresh route and balance authorization on every launch; ErrBudget
// invalidates Kilo and retries afresh without it.
func (a *Adapter) Run(ctx context.Context, spec cartridge.RunSpec) (cartridge.Session, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	mode := a.mode
	switch {
	case strings.EqualFold(strings.TrimSpace(spec.Model), string(ModeStandard)):
		mode = ModeStandard
	case strings.EqualFold(strings.TrimSpace(spec.Model), string(ModeHigh)):
		mode = ModeHigh
	}
	excludeKilo := false
	for range 4 {
		candidates := a.allowed(spec.ToolPolicy)
		if excludeKilo {
			candidates = slices.DeleteFunc(candidates, func(id string) bool { return id == kilo.Name })
		}
		allowed, wait := a.unbenched(candidates)
		if len(allowed) == 0 {
			if excludeKilo {
				return nil, a.noRoute("Kilo's balance check failed and no other account is routable")
			}
			e := a.noRoute("every account sc can use for this firing failed recently and is benched")
			e.RetryAfter = min(wait, NoRouteRetryAfter)
			return nil, e
		}
		key := routeKey{mode: mode, allowed: strings.Join(allowed, ",")}
		user, _ := a.lookupEnv(disableEnv)
		r, cached, err := a.route(ctx, key, Disabled(allowed, user))
		if err != nil {
			return nil, err
		}
		p, known := a.providerByID(r.Account)
		if !known || !a.routeUsable(p, allowed, spec.ToolPolicy) {
			a.invalidate(r)
			msg := fmt.Sprintf("dandelion routed to %q (%s), which is not one sc can drive", r.Account, r)
			switch {
			case a.isBenched(r.Account):
				msg = fmt.Sprintf("dandelion routed to %q (%s), which failed recently and is benched", r.Account, r)
			case known && driverAllows(a.drivers, p.Driver, spec.ToolPolicy) && !driverAvailable(a.drivers, p.Driver):
				msg = fmt.Sprintf("dandelion routed to %q (%s), which needs %s on PATH", r.Account, r, a.drivers[p.Driver].binary)
			case known && p.Driver != DriverNone:
				msg = fmt.Sprintf("dandelion routed to %q (%s), which cannot hold %s", r.Account, r, spec.ToolPolicy)
			}
			return nil, a.noRoute(msg)
		}
		if p.Driver == DriverKilo && cached {
			// A cached Kilo route is only a routing hint, never launch
			// authorization. Probe again before checking Kilo's live balance.
			a.invalidate(r)
			continue
		}

		inner, effort, err := a.delegate(p, r, &spec)
		if err != nil {
			return nil, err
		}
		a.log().Info("dandelion routed a firing",
			slog.String("agent", spec.Labels.Get(cartridge.LabelAgent)),
			slog.String("account", r.Account), slog.String("model", r.Model), slog.String("effort", effort),
			slog.String("mode", string(mode)), slog.String("policy", string(spec.ToolPolicy)),
			slog.Bool("cached", cached))

		s, err := inner.Run(ctx, spec)
		if p.Driver == DriverKilo && errors.Is(err, kilo.ErrBudget) {
			// Kilo checks its profile before starting the provider process.
			// Drop every Kilo route and retry once without it; do not bench
			// the account or turn this into a cartridge-retiring error.
			a.invalidate(r)
			excludeKilo = true
			continue
		}
		if err != nil {
			return nil, a.translate(err, fired{r})
		}
		return a.watch(s, fired{r}), nil
	}
	return nil, cartridge.NewError(cartridge.KindCrash, a.name, "could not obtain a fresh dandelion route for launch", nil)
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

// translate is relabel plus its side effects. A failure that belongs to
// the account (quota, credits, login) drops the cached route so the next
// firing asks the router again, afresh, and benches the account: until its
// reset time (or BenchFor) it is left out of routing, so the router cannot
// name it again even when its own probe still sees headroom. A delegated
// CLI that has gone missing only drops the route: Run stops routing to
// cursor once cursor-agent is off PATH, and a missing claude is not one
// account's fault.
func (a *Adapter) translate(err error, f fired) error {
	if err == nil {
		return nil
	}
	switch cartridge.KindOf(err) {
	case cartridge.KindCreditsExhausted, cartridge.KindRateLimited, cartridge.KindAuth:
		until := a.bench(f.route.Account, retryAfterOf(err))
		a.invalidate(f.route)
		a.log().Warn("dandelion account failed; benched, and the next firing is routed again",
			slog.String("account", f.route.Account), slog.String("model", f.route.Model),
			slog.Time("until", until), slog.Any("err", err))
	case cartridge.KindNotInstalled:
		a.invalidate(f.route)
	}
	return a.relabel(err, f)
}

// relabel names the account in a delegated failure and turns an account's
// exhausted credits or failed login into a short rate limit. The supervisor
// retires a cartridge whose credits are exhausted for the rest of the run,
// by name, and fails an auth error without a retry: one account must not
// take every other account behind the router with it. A rate limit is
// shortened to at most AccountRetryAfter for the same reason; when every
// account is out the next route says so ("none", a NoRouteRetryAfter
// pause).
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
	case cartridge.KindAuth:
		e.Kind = cartridge.KindRateLimited
		e.Msg = joinMsg(prefix+" is not logged in; routing again", e.Msg)
		e.RetryAfter = AccountRetryAfter
	case cartridge.KindRateLimited:
		e.Msg = joinMsg(prefix+" is rate limited; routing again", e.Msg)
		if e.RetryAfter <= 0 || e.RetryAfter > AccountRetryAfter {
			e.RetryAfter = AccountRetryAfter
		}
	default:
		e.Msg = joinMsg(prefix, e.Msg)
	}
	return &e
}

// retryAfterOf is the reset time a delegated failure carried, or zero.
func retryAfterOf(err error) time.Duration {
	if e := cartridge.AsError("", err); e != nil {
		return e.RetryAfter
	}
	return 0
}

// bench keeps account out of routing for d, or BenchFor when d is zero,
// and returns until when.
func (a *Adapter) bench(account string, d time.Duration) time.Time {
	if d <= 0 {
		d = BenchFor
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	until := a.now().Add(d)
	if until.After(a.benched[account]) {
		a.benched[account] = until
	}
	return a.benched[account]
}

func (a *Adapter) isBenched(account string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.now().Before(a.benched[account])
}

// unbenched returns ids without the accounts still benched, and when none
// is left, how long until the first of them may be routed to again.
func (a *Adapter) unbenched(ids []string) ([]string, time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	var out []string
	var wait time.Duration
	for _, id := range ids {
		until, ok := a.benched[id]
		switch {
		case !ok:
			out = append(out, id)
		case !now.Before(until):
			delete(a.benched, id)
			out = append(out, id)
		case wait == 0 || until.Sub(now) < wait:
			wait = until.Sub(now)
		}
	}
	return out, wait
}

func joinMsg(a, b string) string {
	if b == "" {
		return a
	}
	return a + ": " + b
}
