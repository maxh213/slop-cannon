package domain

import "time"

// RunID identifies one firing of the cannon at a repository. It names the
// state directory (D-23) and the `sc:run:<id>` label on every bead the run
// creates.
type RunID string

// String returns the id.
func (id RunID) String() string { return string(id) }

// RunMode says where the work comes from: a human brief, or the PO's own
// research (autopilot).
type RunMode string

// Run modes.
const (
	// RunModeBrief decomposes the `-m` text into beads and finishes when they
	// are all settled.
	RunModeBrief RunMode = "brief"
	// RunModeAutopilot lets the PO invent the work; it never stops on its own
	// except when every cartridge is out of credits (D-18).
	RunModeAutopilot RunMode = "autopilot"
)

var runModeEnum = enum[RunMode]{kind: "run mode", values: []RunMode{RunModeBrief, RunModeAutopilot}}

// RunModes returns every valid RunMode in declaration order.
func RunModes() []RunMode { return runModeEnum.all() }

// ParseRunMode parses a mode name leniently; see ParseRole for the rules.
func ParseRunMode(s string) (RunMode, error) { return runModeEnum.parse(s) }

// String returns the canonical name.
func (m RunMode) String() string { return string(m) }

// Valid reports whether m is one of the declared modes.
func (m RunMode) Valid() bool { return runModeEnum.valid(m) }

// MarshalText implements encoding.TextMarshaler.
func (m RunMode) MarshalText() ([]byte, error) { return runModeEnum.marshal(m) }

// UnmarshalText implements encoding.TextUnmarshaler.
func (m *RunMode) UnmarshalText(b []byte) error {
	v, err := runModeEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*m = v
	return nil
}

// ConfigSnapshot is the effective configuration frozen at run start so a
// resumed run behaves exactly like the original even if the config files
// changed meanwhile. It carries the README's knobs; the config package
// (P1-02) owns loading and merging and copies the result here.
type ConfigSnapshot struct {
	// Ammo is the concurrency cap on agent subprocesses (minimum 3).
	Ammo int `json:"ammo"`
	// Safety true means push a branch and open a PR; false means merge to the
	// default branch directly (D-10).
	Safety bool `json:"safety"`
	// Cartridge is the active cartridge name.
	Cartridge string `json:"cartridge"`
	// Model is the model configured for the active cartridge, if any.
	Model string `json:"model,omitempty"`
	// Vibes are the style preference files appended to every prompt.
	Vibes []string `json:"vibes,omitempty"`
	// Rules are the hard constraint files appended to every prompt.
	Rules []string `json:"rules,omitempty"`
	// ReviewPRs enables reviewing external pull requests (D-11).
	ReviewPRs bool `json:"review_prs"`
	// Headless means events are rendered as log lines instead of the TUI (D-15).
	Headless bool `json:"headless"`
	// AutoMerge lets the run squash-merge the pull requests it opened once
	// they are green (D-31); meaningful only with Safety.
	AutoMerge bool `json:"auto_merge,omitempty"`
	// AutoMergeChecks are the check names an auto-merge requires (D-31).
	AutoMergeChecks []string `json:"auto_merge_checks,omitempty"`
	// AutoMergeSpacing is the least time between two auto-merges (D-31).
	AutoMergeSpacing time.Duration `json:"auto_merge_spacing,omitempty"`
	// ConventionalPRTitles titles pull requests "<type>: <title>" (D-31).
	ConventionalPRTitles bool `json:"conventional_pr_titles,omitempty"`
}

// Run is one orchestrator run against a target repository.
type Run struct {
	// ID identifies the run.
	ID RunID `json:"id"`
	// RepoPath is the absolute path of the target git repository.
	RepoPath string `json:"repo_path"`
	// StateDir is where run state, logs, prompts and worktrees live (D-23).
	StateDir string `json:"state_dir,omitempty"`
	// Mode is brief or autopilot.
	Mode RunMode `json:"mode"`
	// Brief is the human's description of the work; empty in autopilot.
	Brief string `json:"brief,omitempty"`
	// Epic is the parent bead that owns every work bead of this run (D-22);
	// empty until the first plan lands.
	Epic BeadID `json:"epic,omitempty"`
	// StartedAt is when the run started.
	StartedAt time.Time `json:"started_at"`
	// Config is the effective configuration at start.
	Config ConfigSnapshot `json:"config"`
}
