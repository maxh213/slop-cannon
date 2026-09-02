package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"slop-cannon/internal/domain"
)

// RunConfig is everything a run needs before the orchestrator starts: the
// domain.Run record (id, repository, mode, brief, start time and the frozen
// ConfigSnapshot), the cartridge it fires, the ammo allocation, the
// presentation flags, and the full effective configuration the snapshot was
// taken from, with per-key sources. The CLI resolves it (P1-08) and the
// orchestrator consumes it (P6-01); its JSON form is what `sc <path> --json`
// prints.
type RunConfig struct {
	// Run is the run record. Run.StateDir is the D-23 run directory and
	// Run.Config the snapshot the run keeps even if the files change later.
	Run domain.Run `json:"run"`
	// Resume is true when the run continues the most recent earlier run of
	// the repository instead of starting a new one.
	Resume bool `json:"resume"`
	// NoAnim disables the TUI cannon animations; meaningless when headless.
	NoAnim bool `json:"no_anim"`
	// CartridgeName is the cartridge the run fires (the active one, or the
	// --cartridge override).
	CartridgeName string `json:"cartridge_name"`
	// Cartridge holds that cartridge's settings (model, command, args, env).
	Cartridge Cartridge `json:"cartridge"`
	// Allocation is the D-05 split of Run.Config.Ammo into roles.
	Allocation Allocation `json:"allocation"`
	// Effective is the merged configuration with sources, after CLI
	// overrides were applied.
	Effective *Effective `json:"effective"`
}

// ErrNoCartridge is returned by NewRunConfig when no cartridge is active; a
// run refuses to start without one (D-04).
var ErrNoCartridge = errors.New("no cartridge loaded")

// RunSettings are the per-run choices that come from the run command's
// flags rather than from the config files.
type RunSettings struct {
	// ID names the run. Empty means "generate one" (see NewRunID) unless
	// Resume is set, in which case the most recent run of the repository is
	// resumed (see LatestRunID).
	ID domain.RunID
	// Brief is the -m text; empty means autopilot.
	Brief string
	// Headless renders events as log lines instead of the TUI (D-15).
	Headless bool
	// Resume continues an earlier run instead of starting a new one.
	Resume bool
	// NoAnim disables the TUI animations.
	NoAnim bool
	// ReviewPRs enables reviewing open pull requests sc did not create (D-11).
	ReviewPRs bool
	// StartedAt is the run start time; the zero value means time.Now().
	StartedAt time.Time
}

// NewRunConfig assembles a RunConfig for repoRoot from an effective
// configuration (already loaded and with any flag overrides applied) and
// the run settings. It fails with ErrNoCartridge when eff has no active
// cartridge, and with an error when Resume is set but the repository has no
// recorded run. It does not touch the file system beyond listing the runs
// directory for Resume.
func NewRunConfig(repoRoot string, eff *Effective, s RunSettings) (*RunConfig, error) {
	if eff == nil {
		return nil, errors.New("run config: effective configuration is nil")
	}
	name, cart, ok := eff.Active()
	if !ok {
		return nil, ErrNoCartridge
	}
	alloc, err := Allocate(eff.Ammo)
	if err != nil {
		return nil, err
	}
	startedAt := s.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	id := s.ID
	switch {
	case s.Resume && id == "":
		latest, found, err := LatestRunID(repoRoot)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("nothing to resume: no runs recorded for %s (looked in %s)",
				repoRoot, filepath.Join(StateDir(repoRoot), RunsDirName))
		}
		id = latest
	case id == "":
		id = NewRunID(startedAt)
	}

	mode := domain.RunModeAutopilot
	if s.Brief != "" {
		mode = domain.RunModeBrief
	}
	snap := eff.Snapshot()
	snap.ReviewPRs = s.ReviewPRs
	snap.Headless = s.Headless

	return &RunConfig{
		Run: domain.Run{
			ID:        id,
			RepoPath:  repoRoot,
			StateDir:  RunDir(repoRoot, id.String()),
			Mode:      mode,
			Brief:     s.Brief,
			StartedAt: startedAt,
			Config:    snap,
		},
		Resume:        s.Resume,
		NoAnim:        s.NoAnim,
		CartridgeName: name,
		Cartridge:     cloneCartridge(cart),
		Allocation:    alloc,
		Effective:     eff,
	}, nil
}

// runIDStamp is the time layout of the leading part of a run id. It sorts
// chronologically as text, which LatestRunID relies on.
const runIDStamp = "20060102-150405"

// NewRunID mints a run id of the form "20060102-150405-1a2b" (UTC time plus
// four random hex digits). Ids sort chronologically as text and are safe as
// a single path element, so they double as the run directory name (D-23).
func NewRunID(now time.Time) domain.RunID {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is effectively impossible; fall back to the
		// clock so ids stay unique within a second at least.
		b[0], b[1] = byte(now.Nanosecond()>>8), byte(now.Nanosecond())
	}
	return domain.RunID(now.UTC().Format(runIDStamp) + "-" + hex.EncodeToString(b[:]))
}

// LatestRunID returns the id of the most recent run recorded for the
// repository: the greatest directory name under <StateDir>/runs, which is
// the newest run because NewRunID ids sort chronologically. found is false
// when the runs directory is absent or empty.
func LatestRunID(repoPath string) (id domain.RunID, found bool, err error) {
	dir := filepath.Join(StateDir(repoPath), RunsDirName)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("list runs: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", false, nil
	}
	sort.Strings(names)
	return domain.RunID(names[len(names)-1]), true, nil
}
