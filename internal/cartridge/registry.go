package cartridge

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"slop-cannon/internal/config"
)

// ErrUnknownCartridge is wrapped by Registry.New when no factory is
// registered under the requested name.
var ErrUnknownCartridge = errors.New("cartridge: unknown cartridge")

// Factory builds a cartridge from its configured name and settings. The name
// is the config key ([cartridges.<name>]) and the registry key; with only
// Claude supported (D-19) they coincide with the adapter name. A factory
// must not touch the network or spawn processes; that is Detect's job.
type Factory func(name string, cfg config.Cartridge) (Cartridge, error)

// Registry maps cartridge names to factories. It is safe for concurrent use.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{factories: map[string]Factory{}}
}

// Register adds a factory under name. It fails on an empty or already
// registered name and on a nil factory.
func (r *Registry) Register(name string, f Factory) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("cartridge: cannot register an empty name")
	}
	if f == nil {
		return fmt.Errorf("cartridge: cannot register %q with a nil factory", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.factories[name]; dup {
		return fmt.Errorf("cartridge: %q is already registered", name)
	}
	r.factories[name] = f
	return nil
}

// MustRegister is Register for init functions; it panics on error.
func (r *Registry) MustRegister(name string, f Factory) {
	if err := r.Register(name, f); err != nil {
		panic(err)
	}
}

// Has reports whether a factory is registered under name.
func (r *Registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.factories[name]
	return ok
}

// Names returns the registered names in sorted order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.factories))
	for n := range r.factories {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// New builds the cartridge registered under name with cfg. The error wraps
// ErrUnknownCartridge when nothing is registered under name and names the
// registered alternatives.
func (r *Registry) New(name string, cfg config.Cartridge) (Cartridge, error) {
	r.mu.RLock()
	f, ok := r.factories[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w %q (registered: %s)", ErrUnknownCartridge, name, r.registeredList())
	}
	c, err := f(name, cfg)
	if err != nil {
		return nil, fmt.Errorf("cartridge %q: %w", name, err)
	}
	if c == nil {
		return nil, fmt.Errorf("cartridge %q: factory returned nil", name)
	}
	return c, nil
}

// Build constructs every cartridge configured in cfg.Cartridges, keyed by
// name. It fails on the first configured name with no factory or whose
// factory fails, so a stale config is caught before a run starts. An empty
// configuration yields an empty, non-nil map.
func (r *Registry) Build(cfg config.Config) (map[string]Cartridge, error) {
	out := make(map[string]Cartridge, len(cfg.Cartridges))
	names := make([]string, 0, len(cfg.Cartridges))
	for n := range cfg.Cartridges {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c, err := r.New(n, cfg.Cartridges[n])
		if err != nil {
			return nil, err
		}
		out[n] = c
	}
	return out, nil
}

func (r *Registry) registeredList() string {
	names := r.Names()
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// Default is the process-wide registry adapters register with from init.
var Default = NewRegistry()

// Register adds a factory to Default.
func Register(name string, f Factory) error { return Default.Register(name, f) }

// MustRegister adds a factory to Default and panics on error.
func MustRegister(name string, f Factory) { Default.MustRegister(name, f) }

// New builds a cartridge from Default.
func New(name string, cfg config.Cartridge) (Cartridge, error) { return Default.New(name, cfg) }

// Names lists Default's registered names in sorted order.
func Names() []string { return Default.Names() }

// Build constructs every configured cartridge from Default.
func Build(cfg config.Config) (map[string]Cartridge, error) { return Default.Build(cfg) }
