package cartridge

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// Selection errors.
var (
	// ErrNoActiveCartridge is returned by NewSelector when the configuration
	// names no active cartridge: a run refuses to start without one (D-04).
	ErrNoActiveCartridge = errors.New("cartridge: no active cartridge configured (add one with: sc cartridge --add claude --model <model>)")
	// ErrCartridgeNotBuilt is wrapped when the active cartridge or a
	// per-role override names a cartridge that was not built, so a stale
	// configuration is caught before the first agent fires.
	ErrCartridgeNotBuilt = errors.New("cartridge: not built")
)

// DefaultDetectTTL is how long a Selector trusts a Detect result before
// probing the CLI again.
const DefaultDetectTTL = 5 * time.Minute

// Publisher receives the events a Selector raises. *events.Bus satisfies it.
type Publisher interface {
	Publish(events.Event)
}

// PublisherFunc adapts a function to Publisher.
type PublisherFunc func(events.Event)

// Publish implements Publisher.
func (f PublisherFunc) Publish(ev events.Event) { f(ev) }

// SelectorOption configures a Selector.
type SelectorOption func(*Selector)

// WithPublisher sets where the Selector publishes its Warning events. With
// no publisher the warnings are dropped and the fallback still happens.
func WithPublisher(p Publisher) SelectorOption {
	return func(s *Selector) { s.pub = p }
}

// WithRun stamps the run id on every event the Selector publishes.
func WithRun(run domain.RunID) SelectorOption {
	return func(s *Selector) { s.run = run }
}

// WithDetectTTL sets how long a Detect verdict is cached per cartridge. Zero
// disables caching so every Select probes again; negative caches forever.
// The default is DefaultDetectTTL.
func WithDetectTTL(d time.Duration) SelectorOption {
	return func(s *Selector) { s.ttl = d }
}

// WithClock replaces the clock used for cache expiry and event timestamps
// (tests drive it; the default is time.Now).
func WithClock(now func() time.Time) SelectorOption {
	return func(s *Selector) {
		if now != nil {
			s.now = now
		}
	}
}

// Selection is what Choose decided for one role.
type Selection struct {
	// Role is the role the choice was made for.
	Role domain.Role
	// Requested is the cartridge D-12 asked for: the role's override when
	// configured, else the active cartridge.
	Requested string
	// Name is the cartridge actually chosen; it differs from Requested only
	// when FellBack is true.
	Name string
	// Cartridge is the chosen adapter.
	Cartridge Cartridge
	// Status is the chosen cartridge's Detect result.
	Status Status
	// FellBack is true when Requested failed detection and the active
	// cartridge was used instead.
	FellBack bool
	// Reason is why Requested was rejected; nil unless FellBack.
	Reason error
}

// Selector implements the D-12 cartridge selection policy: a role uses its
// configured override ([roles.<role>] cartridge = "name") when there is one,
// else the active cartridge. A chosen cartridge is probed with Detect first;
// when an override fails the probe the Selector publishes an events.Warning
// and falls back to the active cartridge. Nothing here assumes a single
// adapter (D-19): any number of built cartridges may be mixed across roles.
//
// Detect verdicts are cached per cartridge for the configured TTL so that
// firing many agents does not spawn a probe each time, and concurrent
// callers that find the cache cold share one in-flight probe rather than
// each spawning their own; a warning is raised once per fresh failed probe,
// not once per Select. A probe that fails only because the caller's context
// was cancelled or timed out is neither cached nor warned about, so one
// dead context cannot poison selection for the rest of the TTL. Selector is
// safe for concurrent use.
type Selector struct {
	// cfg holds the active cartridge and the role overrides; the lookup
	// itself is config.Config.CartridgeFor so the policy lives in one place.
	cfg        config.Config
	cartridges map[string]Cartridge

	pub Publisher
	run domain.RunID
	ttl time.Duration
	now func() time.Time

	mu       sync.Mutex
	cache    map[string]detectEntry
	inflight map[string]*detectProbe
}

// detectEntry is one cached Detect verdict.
type detectEntry struct {
	status Status
	err    error
	at     time.Time
}

// detectProbe is one in-flight Detect that concurrent callers wait on
// instead of probing again. done is closed once the fields are final.
type detectProbe struct {
	done chan struct{}
	// cancelled is true when the probe failed because the leader's context
	// ended; waiters then probe again with their own context.
	cancelled bool
	status    Status
	err       error
}

// NewSelector builds a Selector for cfg over cartridges (normally the map
// Registry.Build returned). It fails with ErrNoActiveCartridge when
// cfg.ActiveCartridge is empty and wraps ErrCartridgeNotBuilt when the
// active cartridge or a role override names a cartridge missing from the
// map, so configuration mistakes surface before any agent fires.
func NewSelector(cfg config.Config, cartridges map[string]Cartridge, opts ...SelectorOption) (*Selector, error) {
	if cfg.ActiveCartridge == "" {
		return nil, ErrNoActiveCartridge
	}
	if _, ok := cartridges[cfg.ActiveCartridge]; !ok {
		return nil, fmt.Errorf("%w: active cartridge %q (built: %s)", ErrCartridgeNotBuilt, cfg.ActiveCartridge, builtList(cartridges))
	}
	roles := make(map[domain.Role]string, len(cfg.Roles))
	for _, role := range sortedRoles(cfg.Roles) {
		name := cfg.Roles[role]
		if name == "" {
			continue
		}
		if _, ok := cartridges[name]; !ok {
			return nil, fmt.Errorf("%w: role %s override %q (built: %s)", ErrCartridgeNotBuilt, role, name, builtList(cartridges))
		}
		roles[role] = name
	}
	s := &Selector{
		cfg:        config.Config{ActiveCartridge: cfg.ActiveCartridge, Roles: roles},
		cartridges: make(map[string]Cartridge, len(cartridges)),
		ttl:        DefaultDetectTTL,
		now:        time.Now,
		cache:      map[string]detectEntry{},
		inflight:   map[string]*detectProbe{},
	}
	for n, c := range cartridges {
		s.cartridges[n] = c
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// BuildSelector constructs every configured cartridge from r and wraps the
// result in a Selector. It is NewSelector(cfg, r.Build(cfg), opts...).
func BuildSelector(r *Registry, cfg config.Config, opts ...SelectorOption) (*Selector, error) {
	built, err := r.Build(cfg)
	if err != nil {
		return nil, err
	}
	return NewSelector(cfg, built, opts...)
}

// Active returns the active cartridge's name.
func (s *Selector) Active() string { return s.cfg.ActiveCartridge }

// Names returns the built cartridge names in sorted order.
func (s *Selector) Names() []string {
	names := make([]string, 0, len(s.cartridges))
	for n := range s.cartridges {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Cartridge returns the built cartridge registered under name.
func (s *Selector) Cartridge(name string) (Cartridge, bool) {
	c, ok := s.cartridges[name]
	return c, ok
}

// Requested returns the cartridge name D-12 assigns to role before any
// detection: the role's override when configured, else the active cartridge.
func (s *Selector) Requested(role domain.Role) string {
	name, _ := s.cfg.CartridgeFor(role)
	return name
}

// Select returns the cartridge role should use, probing it with Detect and
// falling back to the active cartridge (with a Warning) when the role's
// override fails the probe. The error is the active cartridge's own Detect
// error (a *Error) when nothing usable is left.
func (s *Selector) Select(ctx context.Context, role domain.Role) (Cartridge, error) {
	sel, err := s.Choose(ctx, role)
	if err != nil {
		return nil, err
	}
	return sel.Cartridge, nil
}

// Choose is Select with the full decision: which cartridge was requested,
// which was chosen, its Status, and why a fallback happened. A probe that
// fails because ctx itself is done returns that error with no fallback:
// the caller has given up, and the override was never shown to be broken.
func (s *Selector) Choose(ctx context.Context, role domain.Role) (Selection, error) {
	sel := Selection{Role: role, Requested: s.Requested(role)}
	status, err := s.detect(ctx, sel.Requested)
	if err == nil {
		sel.Name, sel.Cartridge, sel.Status = sel.Requested, s.cartridges[sel.Requested], status
		return sel, nil
	}
	active := s.Active()
	if sel.Requested == active || contextFailed(ctx, err) {
		sel.Status = status
		return sel, err
	}
	sel.FellBack, sel.Reason = true, err
	status, err = s.detect(ctx, active)
	if err != nil {
		sel.Status = status
		return sel, err
	}
	sel.Name, sel.Cartridge, sel.Status = active, s.cartridges[active], status
	return sel, nil
}

// Detect returns the cached Detect verdict for name, probing when the cache
// is stale. It wraps ErrCartridgeNotBuilt for an unknown name.
func (s *Selector) Detect(ctx context.Context, name string) (Status, error) {
	if _, ok := s.cartridges[name]; !ok {
		return Status{}, fmt.Errorf("%w: %q", ErrCartridgeNotBuilt, name)
	}
	return s.detect(ctx, name)
}

// Invalidate forgets the cached Detect verdict for name (every cartridge
// when name is empty) so the next Select probes again. The supervisor calls
// it after a session fails in a way that changes a cartridge's standing.
func (s *Selector) Invalidate(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == "" {
		s.cache = map[string]detectEntry{}
		return
	}
	delete(s.cache, name)
}

// detect serves name's verdict from the cache, joins an in-flight probe for
// the same cartridge, or probes and records the result. A fresh failed
// probe publishes one Warning. A probe that fails only because the leader's
// context ended is not cached and not warned about: the leader gets the
// error, and any waiter (or the next caller) probes again with its own
// context.
func (s *Selector) detect(ctx context.Context, name string) (Status, error) {
	for {
		now := s.now()
		s.mu.Lock()
		if e, ok := s.cache[name]; ok && s.fresh(e, now) {
			s.mu.Unlock()
			return e.status, e.err
		}
		if p, ok := s.inflight[name]; ok {
			s.mu.Unlock()
			select {
			case <-p.done:
				if p.cancelled {
					continue
				}
				return p.status, p.err
			case <-ctx.Done():
				return Status{}, cancelledError(ctx, name)
			}
		}
		p := &detectProbe{done: make(chan struct{})}
		s.inflight[name] = p
		s.mu.Unlock()
		return s.probe(ctx, name, p, now)
	}
}

// probe runs Detect for name as the leader of p, records the verdict and
// releases the waiters.
func (s *Selector) probe(ctx context.Context, name string, p *detectProbe, now time.Time) (Status, error) {
	status, err := s.cartridges[name].Detect(ctx)
	if err == nil && !status.Ready() {
		err = notReadyError(name, status)
	}
	cancelled := contextFailed(ctx, err)
	s.mu.Lock()
	delete(s.inflight, name)
	if !cancelled {
		s.cache[name] = detectEntry{status: status, err: err, at: now}
	}
	p.status, p.err, p.cancelled = status, err, cancelled
	close(p.done)
	s.mu.Unlock()
	if err != nil && !cancelled {
		s.warn(name, status, err)
	}
	return status, err
}

// contextFailed reports whether err is a failure caused by ctx (or any
// context) ending rather than by the cartridge: a non-nil err while ctx is
// done, or an err wrapping context.Canceled or context.DeadlineExceeded.
func contextFailed(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// cancelledError is what a waiter gets when its own context ends before the
// probe it joined finishes.
func cancelledError(ctx context.Context, name string) error {
	return NewError(KindKilled, name, "detect cancelled", context.Cause(ctx))
}

// fresh reports whether a cached entry is still within the TTL.
func (s *Selector) fresh(e detectEntry, now time.Time) bool {
	switch {
	case s.ttl < 0:
		return true
	case s.ttl == 0:
		return false
	default:
		return now.Sub(e.at) < s.ttl
	}
}

// notReadyError turns a Detect that returned a nil error but a Status that
// is not Ready into the *Error the interface promised. Status.Ready is
// false exactly when the CLI is not installed or its login is missing.
func notReadyError(name string, st Status) error {
	if !st.Installed {
		return NewError(KindNotInstalled, name, st.Detail, nil)
	}
	return NewError(KindAuth, name, st.Detail, nil)
}

// warn publishes the detection failure. When name is an override the
// message names the roles that will fall back to the active cartridge; when
// no role uses it (a direct Detect call) it says so instead.
func (s *Selector) warn(name string, st Status, err error) {
	if s.pub == nil {
		return
	}
	msg := fmt.Sprintf("cartridge %q failed detection", name)
	if st.Version != "" {
		msg += fmt.Sprintf(" (version %s)", st.Version)
	}
	active := s.Active()
	switch roles := s.rolesUsing(name); {
	case name == active:
		msg += "; it is the active cartridge, so there is no fallback"
	case len(roles) == 0:
		msg += "; no role is configured to use it"
	case len(roles) == 1:
		msg += fmt.Sprintf("; role %s falls back to the active cartridge %q", roles[0], active)
	default:
		msg += fmt.Sprintf("; roles %s fall back to the active cartridge %q", strings.Join(roles, ", "), active)
	}
	s.pub.Publish(events.Warning{
		Meta:    events.MetaAt(s.run, s.now()),
		Source:  "cartridge",
		Message: msg,
		Err:     err.Error(),
	})
}

// rolesUsing lists, in order, the roles whose override is name.
func (s *Selector) rolesUsing(name string) []string {
	var out []string
	for _, r := range sortedRoles(s.cfg.Roles) {
		if s.cfg.Roles[r] == name {
			out = append(out, r.String())
		}
	}
	return out
}

func sortedRoles(m map[domain.Role]string) []domain.Role {
	roles := make([]domain.Role, 0, len(m))
	for r := range m {
		roles = append(roles, r)
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i] < roles[j] })
	return roles
}

func builtList(m map[string]Cartridge) string {
	if len(m) == 0 {
		return "none"
	}
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
