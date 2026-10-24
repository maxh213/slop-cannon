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
	// DefaultGatePollInterval is how often the run asks bd whether the pull
	// request behind a `gh:pr` gate has merged (P6-10). Every poll shells
	// out to `gh` once per open gate and a human merge is never urgent, so
	// the default is a minute.
	DefaultGatePollInterval = 60 * time.Second
	// DefaultPRPollInterval is how often the run asks GitHub for the open
	// pull requests it might review (D-11, P8-04). Every poll shells out to
	// `gh pr list` once, and a pull request somebody opened while the
	// cannon was firing can wait a minute, so the default matches
	// DefaultGatePollInterval. It is gitx.DefaultPRPollInterval, which is
	// an alias of this constant so there is one number.
	DefaultPRPollInterval = 60 * time.Second
	// DefaultGraphPollInterval is how often the graph watcher lists the
	// beads database to publish what changed (P3-07, P1-14). It is the
	// number beads.DefaultPollInterval carries: two seconds is fast enough
	// that the TUI graph feels live and slow enough that a listing costing
	// ~0.2 s of bd time leaves the database free for the run's writes.
	DefaultGraphPollInterval = 2 * time.Second
	// DefaultMaxDiffBytes is the largest unified diff handed to a reviewer
	// in full (200 KiB, P4-04, P1-14). It is the number
	// gitx.DefaultMaxDiffBytes carries: a bigger diff comes back as a file
	// list with per-file statistics and the testers open the files
	// themselves.
	DefaultMaxDiffBytes = 200 << 10
	// MinMaxDiffBytes is the smallest max_diff_bytes accepted (1 KiB).
	// Anything smaller truncates every diff to nothing, and zero would be
	// read by gitx as "use the default", so both are refused rather than
	// silently ignored.
	MinMaxDiffBytes = 1 << 10
	// DefaultReReviewThreshold is the D-09 default: an earlier approval
	// stands unless the engineer's diff moved by more than 40% of its
	// changed lines between rounds, in which case every tester re-reviews.
	// It is the number gitx.DefaultReReviewThreshold carries.
	DefaultReReviewThreshold = 0.4
	// DefaultMaxBeadsPerPlan is how many beads one planning firing may
	// propose before the plan is rejected outright (P5-02, P1-16). Dropping
	// the surplus is not an option: it would break the dependencies of the
	// beads that stayed.
	DefaultMaxBeadsPerPlan = 8
	// DefaultNice is the scheduling niceness a run gives itself and so every
	// process it starts: agents, check commands and the browsers their test
	// suites open. A dozen agents running a test suite at once can saturate
	// a machine, and at 10 the rest of the desktop stays responsive.
	DefaultNice = 10
	// DefaultResearch lets the planning product owner use web search and
	// fetch: an autopilot plan that pitches features is better grounded in
	// what comparable products do and what their users value.
	DefaultResearch = true
	// MaxNice is the largest niceness there is (the lowest priority).
	MaxNice = 19
	// DefaultMaxReviewRounds is the review-round cap before PO escalation (D-08).
	DefaultMaxReviewRounds = 5
	// DefaultMaxPushbackRounds is the pushback cap per feedback item (D-08).
	DefaultMaxPushbackRounds = 2
	// DefaultMaxConflictRounds bounds how many times one bead may bounce
	// between the delivery and its engineer for the same delivery (D-10,
	// P6-06, P1-18). An engineer that keeps leaving the worktree
	// mid-rebase would otherwise loop forever; after this many rounds the
	// bead goes to needs_human. Three is the smallest number that still
	// allows the two rounds a legitimate conflict can need — one for the
	// conflict itself and one more for the default branch moving again
	// underneath the first resolution — plus a spare. It is not a review
	// budget: D-10 keeps conflict rounds out of max_review_rounds and
	// max_pushback_rounds, and a conflict round fires one engineer and no
	// testers unless D-09 asks for them. It is the number
	// orchestrator.DefaultMaxConflictRounds carries.
	DefaultMaxConflictRounds = 3
	// DefaultMaxPlanRejections is how many plans in a row the planning
	// validator may reject before the run gives up on that round of
	// planning (P6-19, P1-19). One rejection has already cost two plans:
	// the agent layer retries a rule failure once inside the firing itself.
	DefaultMaxPlanRejections = 2
	// DefaultMaxPlanFailures is how many planning firings in a row may fail
	// for a reason that is not the plan's fault — a cartridge crash, an
	// agent timeout, an unreadable rules file — before the run gives up on
	// that round the same way (P6-19, P1-19). It is looser than
	// DefaultMaxPlanRejections because those failures are the transient
	// kind and the supervisor has already spent its own retries on them.
	DefaultMaxPlanFailures = 5
	// DefaultAgentTimeout is the per-agent session timeout.
	DefaultAgentTimeout = 6 * time.Hour
	// DefaultMaxAgentTimeout caps the longer limit the product owner may give
	// one bead's agents (a plan bead's timeout).
	DefaultMaxAgentTimeout = 24 * time.Hour
	// DefaultCheckTimeout bounds the check command a delivery runs before it
	// merges; it matches gitx.DefaultCheckTimeout.
	DefaultCheckTimeout = 30 * time.Minute
	// DefaultAutoMerge is false: with safety on a human merges every pull
	// request unless the run is told otherwise (D-10, D-31).
	DefaultAutoMerge = false
	// DefaultAutoMergeSpacing is the least time between two auto-merges by
	// one run (D-31). A repository that deploys every push to its default
	// branch gets one deploy finished before the next merge starts another.
	DefaultAutoMergeSpacing = 10 * time.Minute
	// DefaultConventionalPRTitles is false: a pull request is titled with
	// the bead's own title unless the repository asks for Conventional
	// Commit titles (D-31).
	DefaultConventionalPRTitles = false
	// DefaultSignalsInterval is how often an autopilot run re-runs the
	// configured signals command (P6-71, D-32).
	DefaultSignalsInterval = 30 * time.Minute
	// DefaultSignalsTimeout bounds one run of the signals command.
	DefaultSignalsTimeout = 5 * time.Minute
	// MaxPromptFileSize is the largest vibes or rules file accepted (64 KiB).
	MaxPromptFileSize = 64 * 1024
)

// KnownCartridges lists every cartridge name sc can drive: Claude Code and
// Cursor Agent (D-29, superseding the Claude-only scope of D-19), and the
// dandelion quota router in front of them (D-30); more adapters are planned.
var KnownCartridges = []string{"claude", "cursor", "dandelion"}

// DandelionModes are the values [cartridges.dandelion] model may take
// (D-30): "high" asks the router for the strongest model that still has
// quota ("dandelion route --high"), "standard" (or nothing) for its everyday
// route. The router names the real model, so nothing else is meaningful.
var DandelionModes = []string{"high", "standard"}

// ValidateCartridgeModel reports whether model is a value the cartridge
// name accepts. Only dandelion constrains it (DandelionModes, compared
// trimmed and case-insensitively; empty means "standard"); every other
// cartridge passes its model to its CLI unchecked.
func ValidateCartridgeModel(name, model string) error {
	if strings.TrimSpace(name) != "dandelion" {
		return nil
	}
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return nil
	}
	for _, ok := range DandelionModes {
		if m == ok {
			return nil
		}
	}
	return fmt.Errorf("cartridge %q takes model %q or %q, not %q: the router picks the real model", name, DandelionModes[0], DandelionModes[1], model)
}

// ValidateCartridgeName reports whether name is a cartridge sc supports. The
// error text is the D-19/D-29 message shared by every command.
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
		return errors.New(invalidDurationMsg(s))
	}
	d.d = td
	return nil
}

// invalidDurationMsg is the one wording for a value that is not a Go
// duration, shared by [Duration.UnmarshalText] and the loader's
// position lookup for values TOML never offered as text (P1-13).
func invalidDurationMsg(value string) string {
	return fmt.Sprintf("invalid duration %q (use a Go duration such as %q or %q)", value, "10m", "1h30m")
}

// Cartridge is one configured LLM CLI subscription ([cartridges.<name>]).
type Cartridge struct {
	// Model is passed to the CLI (for example "fable" for Claude Code).
	Model string `toml:"model,omitempty" json:"model,omitempty"`
	// Command overrides the executable name; empty means DefaultBinaries[name]
	// or the cartridge name.
	Command string `toml:"command,omitempty" json:"command,omitempty"`
	// Args are extra arguments prepended to every invocation.
	Args []string `toml:"args,omitempty" json:"args,omitempty"`
	// Env is extra environment for the CLI process (an allowlist, D-21 style).
	Env map[string]string `toml:"env,omitempty" json:"env,omitempty"`
}

// Binary returns the executable to run: Command when set, else the
// cartridge's DefaultBinaries entry, else name.
func (c Cartridge) Binary(name string) string {
	if c.Command != "" {
		return c.Command
	}
	if bin, ok := DefaultBinaries[name]; ok {
		return bin
	}
	return name
}

// DefaultBinaries names the executable of a cartridge whose CLI is not
// installed under the cartridge's own name. Cursor's CLI is "cursor-agent":
// "cursor" on PATH is the editor (D-29).
var DefaultBinaries = map[string]string{"cursor": "cursor-agent"}

// RoleConfig is the per-role override table ([roles.<role>], D-12).
type RoleConfig struct {
	// Cartridge names the cartridge this role uses instead of the active one.
	Cartridge string `toml:"cartridge,omitempty" json:"cartridge,omitempty"`
	// Model is the model this role's agents are fired with instead of the
	// cartridge's own, e.g. a stronger model for the product owner only.
	Model string `toml:"model,omitempty" json:"model,omitempty"`
}

// LimitsFile is the [limits] table as it appears in a file. Pointer fields
// distinguish "not set" from an explicit zero so layers can be merged.
type LimitsFile struct {
	MaxReviewRounds   *int      `toml:"max_review_rounds,omitempty"`
	MaxPushbackRounds *int      `toml:"max_pushback_rounds,omitempty"`
	MaxConflictRounds *int      `toml:"max_conflict_rounds,omitempty"`
	AgentTimeout      *Duration `toml:"agent_timeout,omitempty"`
	MaxAgentTimeout   *Duration `toml:"max_agent_timeout,omitempty"`
	CheckTimeout      *Duration `toml:"check_timeout,omitempty"`
	SignalsTimeout    *Duration `toml:"signals_timeout,omitempty"`
	MaxTokens         *int64    `toml:"max_tokens,omitempty"`
	MaxCostUSD        *float64  `toml:"max_cost_usd,omitempty"`
	MaxPlanRejections *int      `toml:"max_plan_rejections,omitempty"`
	MaxPlanFailures   *int      `toml:"max_plan_failures,omitempty"`
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
	// Exclude lists extra path patterns agents must never be shown, on top
	// of the built-in ones (agents.DefaultSecretPatterns): dotenv files,
	// PEM keys and id_* private keys are always excluded (P9-05). Each
	// entry is a slash-separated glob in the syntax of path.Match, matched
	// against a path's base name, its whole repository-relative path, and
	// every one of its parent directory names.
	Exclude []string `toml:"exclude,omitempty"`
	// CheckCommand is run before a safety-off merge; empty means autodetect.
	CheckCommand *string `toml:"check_command,omitempty"`
	// IdleReplanInterval is the autopilot re-plan delay when idle (D-18).
	IdleReplanInterval *Duration `toml:"idle_replan_interval,omitempty"`
	// SignalsCommand is the read-only command whose output an autopilot
	// product owner is given as production evidence; empty means off
	// (P6-71, D-32).
	SignalsCommand *string `toml:"signals_command,omitempty"`
	// SignalsInterval is how often the signals command is re-run.
	SignalsInterval *Duration `toml:"signals_interval,omitempty"`
	// GatePollInterval is how often the gh:pr gates are checked (P6-10).
	GatePollInterval *Duration `toml:"gate_poll_interval,omitempty"`
	// PRPollInterval is how often GitHub is asked for external pull
	// requests to review (D-11, P8-04).
	PRPollInterval *Duration `toml:"pr_poll_interval,omitempty"`
	// GraphPollInterval is how often the graph watcher lists the beads
	// database (P3-07, P1-14).
	GraphPollInterval *Duration `toml:"graph_poll_interval,omitempty"`
	// MaxDiffBytes caps the unified diff a reviewer is shown in full
	// (P4-04, P1-14).
	MaxDiffBytes *int `toml:"max_diff_bytes,omitempty"`
	// ReReviewThreshold is the D-09 fraction of changed lines above which
	// every tester re-reviews instead of only those who did not approve.
	ReReviewThreshold *float64 `toml:"re_review_threshold,omitempty"`
	// MaxBeadsPerPlan caps how many beads one PO plan may propose (P5-02).
	MaxBeadsPerPlan *int `toml:"max_beads_per_plan,omitempty"`
	// Nice is the niceness a run gives itself and its children; 0 leaves it.
	Nice *int `toml:"nice,omitempty"`
	// Research lets the planning product owner search and read the web.
	Research *bool `toml:"research,omitempty"`
	// AutoMerge lets the run merge its own pull requests once they are
	// green (D-31); it needs Safety.
	AutoMerge *bool `toml:"auto_merge,omitempty"`
	// AutoMergeChecks names the checks that must be present and successful
	// before an auto-merge (D-31).
	AutoMergeChecks []string `toml:"auto_merge_checks,omitempty"`
	// AutoMergeSpacing is the least time between two auto-merges (D-31).
	AutoMergeSpacing *Duration `toml:"auto_merge_spacing,omitempty"`
	// ConventionalPRTitles titles pull requests "<type>: <title>" (D-31).
	ConventionalPRTitles *bool `toml:"conventional_pr_titles,omitempty"`
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
	// MaxConflictRounds caps how many times one delivery may send a bead
	// back to its engineer to resolve a merge conflict before it is parked
	// in needs_human (D-10, P6-06). Conflict rounds are deliberately
	// outside MaxReviewRounds and MaxPushbackRounds, so this is the only
	// thing bounding them.
	MaxConflictRounds int `json:"max_conflict_rounds"`
	// AgentTimeout bounds one agent session.
	AgentTimeout Duration `json:"agent_timeout"`
	// MaxAgentTimeout caps the longer session limit the product owner may
	// give one bead in its plan; it is never below AgentTimeout.
	MaxAgentTimeout Duration `json:"max_agent_timeout"`
	// CheckTimeout bounds the repository's check command when a delivery
	// runs it before merging (P4-19).
	CheckTimeout Duration `json:"check_timeout"`
	// SignalsTimeout bounds one run of the signals command (P6-71).
	SignalsTimeout Duration `json:"signals_timeout"`
	// MaxTokens is an opt-in token budget; 0 means unlimited (D-18).
	MaxTokens int64 `json:"max_tokens,omitempty"`
	// MaxCostUSD is an opt-in cost budget; 0 means unlimited (D-18).
	MaxCostUSD float64 `json:"max_cost_usd,omitempty"`
	// MaxPlanRejections caps how many plans in a row the planning validator
	// may reject before the run gives up on that round of planning (P6-19).
	MaxPlanRejections int `json:"max_plan_rejections"`
	// MaxPlanFailures caps how many planning firings in a row may fail for
	// a reason that is not the plan's fault before the run gives up on that
	// round of planning (P6-19).
	MaxPlanFailures int `json:"max_plan_failures"`
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
	// Exclude lists the configured `exclude` patterns: extra paths agents
	// must never be shown, unioned global-then-repo the way Vibes and Rules
	// are. The built-in secret patterns apply on top of it, so an empty
	// list still hides .env*, *.pem and id_* (P9-05). Pass it to
	// agents.NewSecretFilter, which is what applies it.
	Exclude []string `json:"exclude,omitempty"`
	// CheckCommand is the repo's check command; empty means autodetect.
	CheckCommand string `json:"check_command"`
	// IdleReplanInterval is the autopilot re-plan delay when idle.
	IdleReplanInterval Duration `json:"idle_replan_interval"`
	// SignalsCommand is run with "sh -c" in the repository root before an
	// autopilot plan, and its output is given to the product owner as
	// untrusted evidence; empty means off (P6-71, D-32).
	SignalsCommand string `json:"signals_command"`
	// SignalsInterval is the least time between two runs of SignalsCommand.
	SignalsInterval Duration `json:"signals_interval"`
	// GatePollInterval is how often a delivered bead's gh:pr gate is
	// checked for the merge that closes it (P6-10).
	GatePollInterval Duration `json:"gate_poll_interval"`
	// PRPollInterval is how often the external pull request reviewer asks
	// GitHub for open pull requests it has not reviewed yet (D-11, P8-04).
	// It bounds how quickly a pull request opened during a run is picked
	// up, and how quickly one that is merged or closed while a review is
	// in flight settles its review bead (P8-11).
	PRPollInterval Duration `json:"pr_poll_interval"`
	// GraphPollInterval is how often the graph watcher lists the beads
	// database so the TUI and the scheduler see what changed (P3-07). Pass
	// it as beads.WatcherOptions.Interval.
	GraphPollInterval Duration `json:"graph_poll_interval"`
	// MaxDiffBytes is the largest unified diff a reviewer is handed in
	// full; a bigger change set comes back as a file list with per-file
	// statistics (P4-04). Pass it as gitx.WithMaxDiffBytes.
	MaxDiffBytes int `json:"max_diff_bytes"`
	// ReReviewThreshold is the D-09 fraction of changed lines above which
	// an earlier approval no longer stands and every tester re-reviews.
	// Pass it to gitx.ChangeRatio.Exceeds (through the orchestrator's
	// ReReviewThreshold options).
	ReReviewThreshold float64 `json:"re_review_threshold"`
	// MaxBeadsPerPlan caps how many beads one PO plan may propose (P5-02);
	// always at least 1.
	MaxBeadsPerPlan int `json:"max_beads_per_plan"`
	// Nice is the niceness (0-19) a run applies to itself, which every
	// process it starts inherits; 0 leaves the priority alone.
	Nice int `json:"nice"`
	// Limits are the feedback-loop and budget limits.
	Limits Limits `json:"limits"`
	// Roles maps a role to the cartridge it should use (D-12); roles absent
	// from the map use ActiveCartridge.
	Roles map[domain.Role]string `json:"roles"`
	// RoleModels maps a role to the model its agents are fired with instead
	// of the cartridge's own; roles absent from the map use the cartridge's.
	RoleModels map[domain.Role]string `json:"role_models"`
	// Research lets the planning product owner search and read the web, so
	// an autopilot plan can be grounded in comparable products.
	Research bool `json:"research"`
	// AutoMerge lets a safety-on run squash-merge the pull requests it
	// opened itself, once every check on them is green, GitHub says they
	// are mergeable and AutoMergeSpacing has passed since its last merge
	// (D-31). It is refused without Safety.
	AutoMerge bool `json:"auto_merge"`
	// AutoMergeChecks are the check or status names that must be present
	// on the pull request and successful before it is auto-merged, on top
	// of "every check that is there passed". Unioned global-then-repo.
	AutoMergeChecks []string `json:"auto_merge_checks"`
	// AutoMergeSpacing is the least time between two auto-merges by one
	// run, so each deploy the default branch triggers finishes before the
	// next merge starts another.
	AutoMergeSpacing Duration `json:"auto_merge_spacing"`
	// ConventionalPRTitles titles the run's pull requests
	// "<type>: <title starting lowercase>", the type taken from the bead's
	// issue type, for repositories whose CI enforces Conventional Commit
	// titles (a squash merge makes the title the commit subject).
	ConventionalPRTitles bool `json:"conventional_pr_titles"`
}

// Defaults returns the built-in configuration (D-04 and friends).
func Defaults() Config {
	return Config{
		Cartridges:         map[string]Cartridge{},
		Ammo:               DefaultAmmo,
		Safety:             DefaultSafety,
		Vibes:              []string{},
		Rules:              []string{},
		Exclude:            []string{},
		IdleReplanInterval: NewDuration(DefaultIdleReplanInterval),
		SignalsInterval:    NewDuration(DefaultSignalsInterval),
		GatePollInterval:   NewDuration(DefaultGatePollInterval),
		PRPollInterval:     NewDuration(DefaultPRPollInterval),
		GraphPollInterval:  NewDuration(DefaultGraphPollInterval),
		MaxDiffBytes:       DefaultMaxDiffBytes,
		ReReviewThreshold:  DefaultReReviewThreshold,
		MaxBeadsPerPlan:    DefaultMaxBeadsPerPlan,
		Nice:               DefaultNice,
		Limits: Limits{
			MaxReviewRounds:   DefaultMaxReviewRounds,
			MaxPushbackRounds: DefaultMaxPushbackRounds,
			MaxConflictRounds: DefaultMaxConflictRounds,
			AgentTimeout:      NewDuration(DefaultAgentTimeout),
			MaxAgentTimeout:   NewDuration(DefaultMaxAgentTimeout),
			CheckTimeout:      NewDuration(DefaultCheckTimeout),
			SignalsTimeout:    NewDuration(DefaultSignalsTimeout),
			MaxPlanRejections: DefaultMaxPlanRejections,
			MaxPlanFailures:   DefaultMaxPlanFailures,
		},
		Roles:                map[domain.Role]string{},
		RoleModels:           map[domain.Role]string{},
		Research:             DefaultResearch,
		AutoMerge:            DefaultAutoMerge,
		AutoMergeChecks:      []string{},
		AutoMergeSpacing:     NewDuration(DefaultAutoMergeSpacing),
		ConventionalPRTitles: DefaultConventionalPRTitles,
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
	// D-31: what a run may merge is frozen with it, so a resume keeps it.
	// The checks and the spacing mean nothing without auto-merge, so a run
	// that does not merge records neither.
	snap.ConventionalPRTitles = c.ConventionalPRTitles
	if c.AutoMerge {
		snap.AutoMerge = true
		snap.AutoMergeChecks = append([]string(nil), c.AutoMergeChecks...)
		snap.AutoMergeSpacing = c.AutoMergeSpacing.Duration()
	}
	return snap
}

// MergeExcludes unions two `exclude` lists: cur, then the entries of add
// that are not already in it, with blanks dropped and order preserved. The
// list is unioned global-then-repo rather than replaced, exactly like Vibes
// and Rules (P1-02): a repository must be able to hide one more file
// without losing what the global config hides.
func MergeExcludes(cur, add []string) []string {
	out := append([]string(nil), cur...)
	for _, p := range add {
		p = strings.TrimSpace(p)
		if p == "" || containsString(out, p) {
			continue
		}
		out = append(out, p)
	}
	return out
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
