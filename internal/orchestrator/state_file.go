package orchestrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
	"slop-cannon/internal/logging"
)

// The run's durable state (P6-01). Everything a resumed run needs that is
// not already in beads (D-06) lives in one file, <run-dir>/state.json,
// rewritten atomically on every lifecycle transition. The run directory is
// outside the target repository (D-23): <state-dir>/runs/<run-id>/.
//
// The frozen [domain.ConfigSnapshot] in State.Run.Config is the reason the
// file exists at all: `--resume` rebuilds its RunConfig from the *current*
// config files (P1-08), and [RestoreRunConfig] puts the original values
// back so a resumed run behaves like the original (P6-11).

const (
	// StateFileName is the run state file inside the run directory.
	StateFileName = "state.json"
	// StateFileVersion is the schema version written to [State.Version]. A
	// file from a newer sc is refused rather than misread.
	StateFileVersion = 1
	// LockFileName is the run lock. It lives in the repository's *state*
	// directory, never in the repository itself (D-23); see [LockPath].
	LockFileName = "run.lock"
)

// Directory permissions for everything the run owns. Run state holds
// prompts, diffs and repository paths, so it is private to the user.
const (
	runDirPerm  = 0o700
	runFilePerm = 0o600
)

// ErrNoState is returned by [LoadState] when the run directory has no
// state file yet: a fresh run, or one that crashed before its first
// transition.
var ErrNoState = errors.New("orchestrator: no state file")

// State is one run's durable state, written to <run-dir>/state.json on
// every lifecycle transition. It is the file `sc status` reads and the file
// `--resume` restores the frozen configuration from (P6-11).
type State struct {
	// Version is [StateFileVersion].
	Version int `json:"version"`
	// Run is the run record: id, repository, mode, brief, epic, start time
	// and the frozen config snapshot.
	Run domain.Run `json:"run"`
	// Phase is the lifecycle phase as of this write.
	Phase Phase `json:"phase"`
	// Resumes counts how many times the run was picked up with --resume;
	// 0 for a run that has only ever been started once.
	Resumes int `json:"resumes"`
	// UpdatedAt is when the file was written.
	UpdatedAt time.Time `json:"updated_at"`
	// Agents are the run's live agents (pending or running) as of this
	// write, in the order they were started.
	Agents []domain.Agent `json:"agents,omitempty"`
	// LastSeq is the number of events the run has published, i.e. the
	// sequence number of the last one. Events are not persisted here, so
	// the count is as of the last transition, not the last event.
	LastSeq uint64 `json:"last_seq"`
	// LastEventKind is the kind of that last event, for a human reading
	// the file after a crash.
	LastEventKind events.Kind `json:"last_event_kind,omitempty"`
	// LastEventAt is when it was published.
	LastEventAt time.Time `json:"last_event_at,omitempty"`
	// StopReason names whatever asked the run to stop ("user", "SIGINT",
	// "sc stop"); empty while nothing has.
	StopReason string `json:"stop_reason,omitempty"`
	// Finish is why the run ended; empty while it is still live.
	Finish events.FinishReason `json:"finish_reason,omitempty"`
	// FinishSummary is the human-readable account of the outcome,
	// including the worktrees cleanup kept (P6-18).
	FinishSummary string `json:"finish_summary,omitempty"`
	// FinishedAt is when the run ended; zero while it is live.
	FinishedAt time.Time `json:"finished_at,omitempty"`
	// FinishError is the unrecoverable failure that ended the run, if any.
	FinishError string `json:"finish_error,omitempty"`
	// Vitals is the "why is nothing happening?" snapshot as of this write:
	// what the planner is doing and what the gate poller has seen (P6-29).
	// It is absent for a run assembled without a planner or a gate poller,
	// and for a state file written by an older sc. `sc status` reads a run
	// off disk and has no live component to ask, so this is the only way
	// the two numbers reach it.
	Vitals *RunVitals `json:"vitals,omitempty"`
	// Budget is what the run has spent and how much headroom it has been
	// granted, as of this write (P9-01). It is what makes
	// `limits.max_tokens` and `limits.max_cost_usd` caps on the *run*
	// rather than on one invocation of it: a resumed run reads it back
	// through [RunBudgetOptions.Restore] instead of counting from zero.
	// It is absent for a run assembled without a budget and for a state
	// file written by an older sc.
	Budget *BudgetState `json:"budget,omitempty"`
	// AutoMerge is the run's auto-merge ledger (D-31): when it last merged
	// one of its own pull requests. It is written before the merge is
	// asked for, so a resumed run waits out auto_merge_spacing from the
	// last merge any invocation of it made. Absent for a run that never
	// auto-merged.
	AutoMerge *AutoMergeState `json:"auto_merge,omitempty"`
}

// AutoMergeState is what [State.AutoMerge] records.
type AutoMergeState struct {
	// LastAt is when the run last asked GitHub to merge a pull request.
	LastAt time.Time `json:"last_at"`
	// LastBead and LastPR are the bead and pull request it merged.
	LastBead domain.BeadID `json:"last_bead,omitempty"`
	LastPR   int           `json:"last_pr,omitempty"`
	// Merges counts the merges asked for over the life of the run.
	Merges int `json:"merges,omitempty"`
}

// Spent is the run's budget state, or the zero value for a state file that
// carries none.
func (s State) Spent() BudgetState {
	if s.Budget == nil {
		return BudgetState{}
	}
	return *s.Budget
}

// clone returns a copy that shares no slice with s.
func (s State) clone() State {
	out := s
	if len(s.Agents) > 0 {
		out.Agents = append([]domain.Agent(nil), s.Agents...)
	}
	out.Run.Config = cloneSnapshot(s.Run.Config)
	if s.AutoMerge != nil {
		am := *s.AutoMerge
		out.AutoMerge = &am
	}
	return out
}

// cloneSnapshot copies a config snapshot including its string slices.
func cloneSnapshot(c domain.ConfigSnapshot) domain.ConfigSnapshot {
	out := c
	if len(c.Vibes) > 0 {
		out.Vibes = append([]string(nil), c.Vibes...)
	}
	if len(c.Rules) > 0 {
		out.Rules = append([]string(nil), c.Rules...)
	}
	if len(c.AutoMergeChecks) > 0 {
		out.AutoMergeChecks = append([]string(nil), c.AutoMergeChecks...)
	}
	return out
}

// StatePath returns <runDir>/state.json. The directory is not created.
func StatePath(runDir string) string {
	return filepath.Join(filepath.Clean(runDir), StateFileName)
}

// RunDirs are the directories one run owns under its run directory (D-23).
type RunDirs struct {
	// Dir is the run directory itself, <state-dir>/runs/<run-id>.
	Dir string
	// Agents holds one log per agent (logging.AgentsDirName).
	Agents string
	// Worktrees holds one git worktree per bead (gitx.WorktreesDirName).
	Worktrees string
	// Shims holds one read-only bd shim directory per bead
	// (beads.ShimDirName); P6-17 fills it.
	Shims string
}

// DirsFor computes the run's directories from its run directory. Nothing is
// created; see [EnsureRunDir].
func DirsFor(runDir string) RunDirs {
	dir := filepath.Clean(runDir)
	return RunDirs{
		Dir:       dir,
		Agents:    filepath.Join(dir, logging.AgentsDirName),
		Worktrees: filepath.Join(dir, gitx.WorktreesDirName),
		Shims:     filepath.Join(dir, beads.ShimDirName),
	}
}

// EnsureRunDir creates the run directory and the subdirectories the run
// needs, all private to the user (0700). It is idempotent, which is what
// --resume needs.
func EnsureRunDir(runDir string) (RunDirs, error) {
	d := DirsFor(runDir)
	for _, p := range []string{d.Dir, d.Agents, d.Worktrees, d.Shims} {
		if err := os.MkdirAll(p, runDirPerm); err != nil {
			return d, fmt.Errorf("orchestrator: create run directory: %w", err)
		}
	}
	return d, nil
}

// WriteState writes s to <runDir>/state.json atomically (temp file, fsync,
// rename), so a crash mid-write leaves either the previous file or the new
// one and never a truncated one. The version and timestamp are stamped by
// the caller; see [Run.persist].
func WriteState(runDir string, s State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("orchestrator: encode state: %w", err)
	}
	data = append(data, '\n')
	path := StatePath(runDir)
	if err := config.WriteAtomic(path, data, runFilePerm); err != nil {
		return fmt.Errorf("orchestrator: write %s: %w", path, err)
	}
	return nil
}

// LoadState reads <runDir>/state.json. A missing file returns [ErrNoState]
// so a caller can tell "never ran" from "unreadable".
func LoadState(runDir string) (State, error) {
	path := StatePath(runDir)
	data, err := os.ReadFile(path) //nolint:gosec // the path is the run directory sc owns (D-23).
	if errors.Is(err, os.ErrNotExist) {
		return State{}, fmt.Errorf("%w at %s", ErrNoState, path)
	}
	if err != nil {
		return State{}, fmt.Errorf("orchestrator: read %s: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("orchestrator: parse %s: %w", path, err)
	}
	if s.Version > StateFileVersion {
		return State{}, fmt.Errorf("orchestrator: %s was written by a newer sc (state version %d, this build understands %d)",
			path, s.Version, StateFileVersion)
	}
	return s, nil
}

// RestoreResult reports what [RestoreRunConfig] did.
type RestoreResult struct {
	// State is the state file that was read; the zero value when
	// Restored is false.
	State State
	// Restored is true when a state file was found and applied.
	Restored bool
	// Overridden lists the config keys whose restored value was discarded
	// because the current invocation passed the corresponding flag (see
	// [RestoreRunConfig]).
	Overridden []string
	// Notes are one-line explanations for a human, suitable for a warning.
	Notes []string
}

// RestoreRunConfig puts a resumed run back on its original footing (P6-11).
//
// `sc --resume` locates the latest run directory and rebuilds its RunConfig
// from the *current* config files (P1-08), so a config edit between the two
// invocations would silently change the run. [domain.ConfigSnapshot]
// promises the opposite, so this replaces rc.Run's frozen snapshot, brief,
// mode, epic and start time with the values in <run-dir>/state.json, and
// recomputes the ammo allocation and the active cartridge from the restored
// snapshot.
//
// Explicit flags win over the snapshot and are listed in
// [RestoreResult.Overridden]: `--ammo`, `--safety` and `--cartridge` are a
// deliberate act by the user in this invocation, not a config file that
// drifted, so they are honoured rather than refused. Everything the user
// chose per invocation anyway — headless, no-anim, review-prs — is left as
// resolved. rc.Effective is not modified: it still describes what the
// config files say now, which is what `sc config show` reports.
//
// A run directory with no state file is not an error: Restored is false and
// rc is untouched, so a run that crashed before its first transition can
// still be resumed from the current configuration.
func RestoreRunConfig(rc *config.RunConfig) (RestoreResult, error) {
	if rc == nil {
		return RestoreResult{}, errors.New("orchestrator: RestoreRunConfig: nil run config")
	}
	st, err := LoadState(rc.Run.StateDir)
	if errors.Is(err, ErrNoState) {
		return RestoreResult{Notes: []string{fmt.Sprintf(
			"run %s has no %s; resuming from the current configuration", rc.Run.ID, StateFileName)}}, nil
	}
	if err != nil {
		return RestoreResult{}, err
	}
	res := RestoreResult{State: st, Restored: true}

	if st.Run.ID != "" && st.Run.ID != rc.Run.ID {
		return res, fmt.Errorf("orchestrator: %s belongs to run %s, not %s",
			StatePath(rc.Run.StateDir), st.Run.ID, rc.Run.ID)
	}

	frozen := cloneSnapshot(st.Run.Config)
	current := rc.Run.Config
	// Per-invocation presentation and scope flags always come from this
	// invocation: a run started in the TUI may be resumed headless.
	frozen.Headless = current.Headless
	frozen.ReviewPRs = current.ReviewPRs

	flagged := func(key string) bool {
		return rc.Effective != nil && rc.Effective.Sources[key] == config.SourceFlag
	}
	// drifted says which config file now disagrees with the snapshot, so
	// the resume can say so instead of keeping the recorded value silently.
	drifted := func(key string, differs bool) (config.Source, bool) {
		if !differs || rc.Effective == nil {
			return "", false
		}
		src := rc.Effective.Sources[key]
		return src, src == config.SourceGlobal || src == config.SourceRepo
	}
	if src, ok := drifted(config.KeyAmmo, frozen.Ammo != current.Ammo); ok {
		res.Notes = append(res.Notes, fmt.Sprintf("the %s config now says ammo %d, but run %s keeps its recorded ammo %d; pass --ammo %d to change it",
			src, current.Ammo, rc.Run.ID, frozen.Ammo, current.Ammo))
	}
	if src, ok := drifted(config.KeySafety, frozen.Safety != current.Safety); ok {
		res.Notes = append(res.Notes, fmt.Sprintf("the %s config now says safety %s, but run %s keeps its recorded safety %s; pass --safety %s to change it",
			src, safetyWord(current.Safety), rc.Run.ID, safetyWord(frozen.Safety), safetyWord(current.Safety)))
	}
	if src, ok := drifted(config.KeyActiveCartridge, frozen.Cartridge != "" && frozen.Cartridge != current.Cartridge); ok {
		res.Notes = append(res.Notes, fmt.Sprintf("the %s config now says cartridge %s, but run %s keeps its recorded cartridge %s; pass --cartridge %s to change it",
			src, current.Cartridge, rc.Run.ID, frozen.Cartridge, current.Cartridge))
	}
	if flagged(config.KeyAmmo) && frozen.Ammo != current.Ammo {
		res.Overridden = append(res.Overridden, config.KeyAmmo)
		res.Notes = append(res.Notes, fmt.Sprintf("--ammo %d overrides the run's recorded ammo %d", current.Ammo, frozen.Ammo))
		frozen.Ammo = current.Ammo
	}
	if flagged(config.KeySafety) && frozen.Safety != current.Safety {
		res.Overridden = append(res.Overridden, config.KeySafety)
		res.Notes = append(res.Notes, fmt.Sprintf("--safety %s overrides the run's recorded safety %s",
			safetyWord(current.Safety), safetyWord(frozen.Safety)))
		frozen.Safety = current.Safety
	}
	cartridgeFlagged := flagged(config.KeyActiveCartridge)
	if cartridgeFlagged && frozen.Cartridge != current.Cartridge {
		res.Overridden = append(res.Overridden, config.KeyActiveCartridge)
		res.Notes = append(res.Notes, fmt.Sprintf("--cartridge %s overrides the run's recorded cartridge %s",
			current.Cartridge, frozen.Cartridge))
		frozen.Cartridge = current.Cartridge
		frozen.Model = current.Model
	}
	// D-31: what the run may merge is frozen like safety, and --auto-merge
	// overrides it like --safety. Turning it on now takes the checks and the
	// spacing the configuration has now: a run that never merged recorded
	// neither.
	if src, ok := drifted(config.KeyAutoMerge, frozen.AutoMerge != current.AutoMerge); ok {
		res.Notes = append(res.Notes, fmt.Sprintf("the %s config now says auto_merge %t, but run %s keeps its recorded auto_merge %t; pass --auto-merge=%t to change it",
			src, current.AutoMerge, rc.Run.ID, frozen.AutoMerge, current.AutoMerge))
	}
	if flagged(config.KeyAutoMerge) && frozen.AutoMerge != current.AutoMerge {
		res.Overridden = append(res.Overridden, config.KeyAutoMerge)
		res.Notes = append(res.Notes, fmt.Sprintf("--auto-merge=%t overrides the run's recorded auto_merge %t", current.AutoMerge, frozen.AutoMerge))
		frozen.AutoMerge = current.AutoMerge
		frozen.AutoMergeChecks = append([]string(nil), current.AutoMergeChecks...)
		frozen.AutoMergeSpacing = current.AutoMergeSpacing
	}
	if frozen.AutoMerge && !frozen.Safety {
		return res, fmt.Errorf("orchestrator: run %s merges its own pull requests (auto_merge), which %w; pass --auto-merge=false with --safety off",
			rc.Run.ID, config.ErrAutoMergeNeedsSafety)
	}

	alloc, err := config.Allocate(frozen.Ammo)
	if err != nil {
		return res, fmt.Errorf("orchestrator: run %s recorded ammo %d: %w", rc.Run.ID, frozen.Ammo, err)
	}

	// Fire the cartridge the run was started with, at the model it froze,
	// unless this invocation named another one.
	if !cartridgeFlagged && frozen.Cartridge != "" && frozen.Cartridge != rc.CartridgeName {
		cart, ok := configuredCartridge(rc, frozen.Cartridge)
		if !ok {
			return res, fmt.Errorf("orchestrator: run %s was started with cartridge %q, which is no longer configured; add it with `sc cartridge --add %s` or start a new run",
				rc.Run.ID, frozen.Cartridge, frozen.Cartridge)
		}
		rc.CartridgeName = frozen.Cartridge
		rc.Cartridge = cart
		res.Notes = append(res.Notes, fmt.Sprintf("restored cartridge %s from the run's snapshot", frozen.Cartridge))
	}
	if frozen.Model != "" && frozen.Cartridge == rc.CartridgeName {
		rc.Cartridge.Model = frozen.Model
	}

	rc.Run.Config = frozen
	rc.Run.Mode = st.Run.Mode
	rc.Run.Brief = st.Run.Brief
	rc.Run.Epic = st.Run.Epic
	if !st.Run.StartedAt.IsZero() {
		rc.Run.StartedAt = st.Run.StartedAt
	}
	rc.Allocation = alloc
	return res, nil
}

// configuredCartridge returns a deep copy of the named cartridge from the
// effective configuration.
func configuredCartridge(rc *config.RunConfig, name string) (config.Cartridge, bool) {
	if rc.Effective == nil {
		return config.Cartridge{}, false
	}
	cart, ok := rc.Effective.Cartridges[name]
	if !ok {
		return config.Cartridge{}, false
	}
	out := cart
	if len(cart.Args) > 0 {
		out.Args = append([]string(nil), cart.Args...)
	}
	if len(cart.Env) > 0 {
		out.Env = make(map[string]string, len(cart.Env))
		for k, v := range cart.Env {
			out.Env[k] = v
		}
	}
	return out, true
}

// safetyWord renders a safety value the way the CLI does.
func safetyWord(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
