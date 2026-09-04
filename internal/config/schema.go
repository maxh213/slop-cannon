package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"slop-cannon/internal/domain"
)

// SchemaVersion is the schema_version written by this build. A file with a
// lower version is migrated on load (see migrate.go); a higher one is
// refused because it was written by a newer sc.
const SchemaVersion = 1

// schemaVersion is the version the loader targets. It equals SchemaVersion
// except in tests that exercise the migration hook.
var schemaVersion = SchemaVersion

// Names used to locate configuration.
const (
	// AppName is the directory name under XDG_CONFIG_HOME and XDG_STATE_HOME.
	AppName = "slop-cannon"
	// RepoDirName is the directory of the repo config inside the target repo.
	RepoDirName = ".slop-cannon"
	// FileName is the config file name in both locations.
	FileName = "config.toml"
)

// Built-in defaults (D-04, D-05, D-08, D-18).
const (
	// DefaultAmmo is the concurrency cap when nothing sets ammo.
	DefaultAmmo = 10
	// MinAmmo is the smallest allowed ammo: one PO, one engineer, one tester.
	MinAmmo = 3
	// DefaultSafety is true: finished work becomes a PR, never a direct merge.
	DefaultSafety = true
	// DefaultIdleReplanInterval is how long an idle autopilot waits before the
	// PO plans again (D-18).
	DefaultIdleReplanInterval = 10 * time.Minute
	// DefaultMaxBeadsPerPlan is how many beads one planning firing may
	// propose before the plan is rejected outright (P5-02, P1-16). Dropping
	// the surplus is not an option: it would break the dependencies of the
	// beads that stayed.
	DefaultMaxBeadsPerPlan = 8
	// DefaultMaxReviewRounds is the review-round cap before PO escalation (D-08).
	DefaultMaxReviewRounds = 5
	// DefaultMaxPushbackRounds is the pushback cap per feedback item (D-08).
	DefaultMaxPushbackRounds = 2
	// DefaultAgentTimeout is the per-agent session timeout.
	DefaultAgentTimeout = 30 * time.Minute
	// MaxPromptFileSize is the largest vibes or rules file accepted (64 KiB).
	MaxPromptFileSize = 64 * 1024
)

// KnownCartridges lists every cartridge name sc can drive. Only Claude Code
// exists for now (D-19); more adapters are planned.
var KnownCartridges = []string{"claude"}

// ValidateCartridgeName reports whether name is a cartridge sc supports. The
// error text is the D-19 message shared by every command.
func ValidateCartridgeName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("cartridge name must not be empty")
	}
	for _, k := range KnownCartridges {
		if name == k {
			return nil
		}
	}
	return fmt.Errorf("cartridge %q is not supported yet (only %s for now)", name, quoteList(KnownCartridges))
}

// quoteList renders names as `"a", "b"` for error messages.
func quoteList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(q, ", ")
}

// Duration is a time.Duration that reads and writes as a Go duration string
// ("10m", "1h30m") in TOML and JSON. It wraps the value in a struct so a bare
// TOML integer is rejected instead of being read as nanoseconds.
type Duration struct{ d time.Duration }

// NewDuration wraps td.
func NewDuration(td time.Duration) Duration { return Duration{d: td} }

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return d.d }

// String formats the duration compactly: "10m", "1h30m", "45s".
func (d Duration) String() string {
	td := d.d
	switch {
	case td <= 0:
		return td.String()
	case td%time.Hour == 0:
		return fmt.Sprintf("%dh", td/time.Hour)
	case td%time.Minute == 0:
		if h := td / time.Hour; h > 0 {
			return fmt.Sprintf("%dh%dm", h, (td%time.Hour)/time.Minute)
		}
		return fmt.Sprintf("%dm", td/time.Minute)
	}
	// time.Duration.String yields "10m0s" and "1h0m0s"; the two cases above
	// cover those, so whatever remains is already compact ("1m30s", "500ms").
	return td.String()
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	s := strings.TrimSpace(string(b))
	td, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q (use a Go duration such as \"10m\" or \"1h30m\")", s)
	}
	d.d = td
	return nil
}

// Cartridge is one configured LLM CLI subscription ([cartridges.<name>]).
type Cartridge struct {
	// Model is passed to the CLI (for example "fable" for Claude Code).
	Model string `toml:"model,omitempty" json:"model,omitempty"`
	// Command overrides the executable name; empty means the cartridge name.
	Command string `toml:"command,omitempty" json:"command,omitempty"`
	// Args are extra arguments prepended to every invocation.
	Args []string `toml:"args,omitempty" json:"args,omitempty"`
	// Env is extra environment for the CLI process (an allowlist, D-21 style).
	Env map[string]string `toml:"env,omitempty" json:"env,omitempty"`
}

// Binary returns the executable to run: Command when set, else name.
func (c Cartridge) Binary(name string) string {
	if c.Command != "" {
		return c.Command
	}
	return name
}

// RoleConfig is the per-role override table ([roles.<role>], D-12).
type RoleConfig struct {
	// Cartridge names the cartridge this role uses instead of the active one.
	Cartridge string `toml:"cartridge,omitempty" json:"cartridge,omitempty"`
}

// LimitsFile is the [limits] table as it appears in a file. Pointer fields
// distinguish "not set" from an explicit zero so layers can be merged.
type LimitsFile struct {
	MaxReviewRounds   *int      `toml:"max_review_rounds,omitempty"`
	MaxPushbackRounds *int      `toml:"max_pushback_rounds,omitempty"`
	AgentTimeout      *Duration `toml:"agent_timeout,omitempty"`
	MaxTokens         *int64    `toml:"max_tokens,omitempty"`
	MaxCostUSD        *float64  `toml:"max_cost_usd,omitempty"`
}

// File is the on-disk TOML schema, shared by the global and repo files.
// Pointer and slice fields are nil when the file does not set them, which is
// how [Load] tracks the source of every effective value.
type File struct {
	// SchemaVersion is the version of this layout; see SchemaVersion.
	SchemaVersion int `toml:"schema_version"`
	// ActiveCartridge names the cartridge runs use by default (global only).
	ActiveCartridge *string `toml:"active_cartridge,omitempty"`
	// Ammo is the concurrency cap (minimum MinAmmo).
	Ammo *int `toml:"ammo,omitempty"`
	// Safety true means PRs; false means straight to the default branch.
	Safety *bool `toml:"safety,omitempty"`
	// Vibes are style preference files, relative to this file's directory or
	// absolute.
	Vibes []string `toml:"vibes,omitempty"`
	// Rules are hard constraint files, relative to this file's directory or
	// absolute.
	Rules []string `toml:"rules,omitempty"`
	// CheckCommand is run before a safety-off merge; empty means autodetect.
	CheckCommand *string `toml:"check_command,omitempty"`
	// IdleReplanInterval is the autopilot re-plan delay when idle (D-18).
	IdleReplanInterval *Duration `toml:"idle_replan_interval,omitempty"`
	// MaxBeadsPerPlan caps how many beads one PO plan may propose (P5-02).
	MaxBeadsPerPlan *int `toml:"max_beads_per_plan,omitempty"`
	// Limits holds the feedback-loop and budget limits (D-08, D-18).
	Limits LimitsFile `toml:"limits,omitempty"`
	// Cartridges are the configured subscriptions, keyed by name (global only).
	Cartridges map[string]Cartridge `toml:"cartridges,omitempty"`
	// Roles are per-role cartridge overrides keyed by role name (D-12).
	Roles map[string]RoleConfig `toml:"roles,omitempty"`
}

// CartridgeNames returns the configured cartridge names in sorted order.
func (f *File) CartridgeNames() []string {
	names := make([]string, 0, len(f.Cartridges))
	for n := range f.Cartridges {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Limits are the effective feedback-loop and budget limits.
type Limits struct {
	// MaxReviewRounds caps review rounds per bead before PO escalation (D-08).
	MaxReviewRounds int `json:"max_review_rounds"`
	// MaxPushbackRounds caps pushback exchanges per item before arbitration (D-08).
	MaxPushbackRounds int `json:"max_pushback_rounds"`
	// AgentTimeout bounds one agent session.
	AgentTimeout Duration `json:"agent_timeout"`
	// MaxTokens is an opt-in token budget; 0 means unlimited (D-18).
	MaxTokens int64 `json:"max_tokens,omitempty"`
	// MaxCostUSD is an opt-in cost budget; 0 means unlimited (D-18).
	MaxCostUSD float64 `json:"max_cost_usd,omitempty"`
}

// Config is the merged, fully resolved configuration. Every field has a
// value: defaults fill whatever no file sets. Vibes and Rules are absolute
// paths.
type Config struct {
	// ActiveCartridge is the cartridge runs use unless overridden; empty
	// until one is added.
	ActiveCartridge string `json:"active_cartridge"`
	// Cartridges are the configured subscriptions keyed by name.
	Cartridges map[string]Cartridge `json:"cartridges"`
	// Ammo is the concurrency cap on agent subprocesses.
	Ammo int `json:"ammo"`
	// Safety true means PRs; false means straight to the default branch.
	Safety bool `json:"safety"`
	// Vibes are absolute paths of style preference files.
	Vibes []string `json:"vibes"`
	// Rules are absolute paths of hard constraint files.
	Rules []string `json:"rules"`
	// CheckCommand is the repo's check command; empty means autodetect.
	CheckCommand string `json:"check_command"`
	// IdleReplanInterval is the autopilot re-plan delay when idle.
	IdleReplanInterval Duration `json:"idle_replan_interval"`
	// MaxBeadsPerPlan caps how many beads one PO plan may propose (P5-02);
	// always at least 1.
	MaxBeadsPerPlan int `json:"max_beads_per_plan"`
	// Limits are the feedback-loop and budget limits.
	Limits Limits `json:"limits"`
	// Roles maps a role to the cartridge it should use (D-12); roles absent
	// from the map use ActiveCartridge.
	Roles map[domain.Role]string `json:"roles"`
}

// Defaults returns the built-in configuration (D-04 and friends).
func Defaults() Config {
	return Config{
		Cartridges:         map[string]Cartridge{},
		Ammo:               DefaultAmmo,
		Safety:             DefaultSafety,
		Vibes:              []string{},
		Rules:              []string{},
		IdleReplanInterval: NewDuration(DefaultIdleReplanInterval),
		MaxBeadsPerPlan:    DefaultMaxBeadsPerPlan,
		Limits: Limits{
			MaxReviewRounds:   DefaultMaxReviewRounds,
			MaxPushbackRounds: DefaultMaxPushbackRounds,
			AgentTimeout:      NewDuration(DefaultAgentTimeout),
		},
		Roles: map[domain.Role]string{},
	}
}

// CartridgeFor returns the cartridge name a role should use: the D-12
// override when set, else the active cartridge. ok is false when neither
// exists.
func (c Config) CartridgeFor(role domain.Role) (name string, ok bool) {
	if n := c.Roles[role]; n != "" {
		return n, true
	}
	return c.ActiveCartridge, c.ActiveCartridge != ""
}

// Active returns the active cartridge's settings; ok is false when no
// cartridge is active.
func (c Config) Active() (name string, cart Cartridge, ok bool) {
	if c.ActiveCartridge == "" {
		return "", Cartridge{}, false
	}
	cart, ok = c.Cartridges[c.ActiveCartridge]
	return c.ActiveCartridge, cart, ok
}

// Snapshot freezes the values a run records (domain.ConfigSnapshot). The
// caller sets ReviewPRs and Headless from the run flags.
func (c Config) Snapshot() domain.ConfigSnapshot {
	snap := domain.ConfigSnapshot{
		Ammo:      c.Ammo,
		Safety:    c.Safety,
		Cartridge: c.ActiveCartridge,
	}
	if _, cart, ok := c.Active(); ok {
		snap.Model = cart.Model
	}
	if len(c.Vibes) > 0 {
		snap.Vibes = append([]string(nil), c.Vibes...)
	}
	if len(c.Rules) > 0 {
		snap.Rules = append([]string(nil), c.Rules...)
	}
	return snap
}

// ErrAmmoTooLow is returned for ammo below MinAmmo.
var ErrAmmoTooLow = errors.New("ammo must be at least 3: one engineer, one tester, one product owner")

// Allocation is what an ammo value implies under D-05: one PO slot when
// needed, max(1, floor((ammo-1)/4)) engineers in flight, and every remaining
// slot available to testers.
type Allocation struct {
	Ammo         int `json:"ammo"`
	PO           int `json:"po"`
	MaxEngineers int `json:"max_engineers"`
	MaxTesters   int `json:"max_testers"`
}

// Allocate computes the D-05 allocation for ammo, or ErrAmmoTooLow.
func Allocate(ammo int) (Allocation, error) {
	if ammo < MinAmmo {
		return Allocation{}, ErrAmmoTooLow
	}
	eng := (ammo - 1) / 4
	if eng < 1 {
		eng = 1
	}
	return Allocation{Ammo: ammo, PO: 1, MaxEngineers: eng, MaxTesters: ammo - 1 - eng}, nil
}

// String renders the allocation as the P1-04 preview line, for example
// "ammo 12 → 1 PO, up to 2 engineers, up to 9 testers".
func (a Allocation) String() string {
	return fmt.Sprintf("ammo %d → %d PO, up to %d %s, up to %d %s",
		a.Ammo, a.PO, a.MaxEngineers, plural(a.MaxEngineers, "engineer"), a.MaxTesters, plural(a.MaxTesters, "tester"))
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// ParseSafety accepts on/off, true/false, yes/no, 1/0 (case-insensitive).
func ParseSafety(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "true", "yes", "1", "t", "y":
		return true, nil
	case "off", "false", "no", "0", "f", "n":
		return false, nil
	}
	return false, fmt.Errorf("safety must be on or off, got %q", s)
}
