package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"slop-cannon/internal/domain"
)

// Dolt backend modes as `bd context --json` and .beads/metadata.json report
// them (Appendix A). Embedded is the only mode sc supports in Phase 3: the
// engine runs in-process inside bd, takes an exclusive lock on the data
// directory, and needs no dolt binary or server. Server mode is bd's
// multi-writer path through `dolt sql-server`; sc has not been tested
// against it and only warns when it finds it (CheckDolt).
const (
	DoltModeEmbedded = "embedded"
	DoltModeServer   = "server"
)

// AutoCommitKey is the bd config key that decides whether every write is a
// Dolt commit. bd 1.2.2 defaults it to AutoCommitOn in embedded mode
// (`bd config get dolt.auto-commit` prints "on" even when config.yaml has
// no such line), which is what makes `bd history <id>` show one entry per
// sc write and why sc never runs `bd dolt commit`.
const AutoCommitKey = "dolt.auto-commit"

// Values of AutoCommitKey. Only AutoCommitOn keeps the one-commit-per-write
// invariant; "batch" and "off" leave writes in the Dolt working set until
// `bd dolt commit`.
const (
	AutoCommitOn    = "on"
	AutoCommitOff   = "off"
	AutoCommitBatch = "batch"
)

// ProjectMetadataFile is the file inside .beads/ that records the backend
// (`dolt_mode`, `dolt_database`, `project_id`). It is written by bd init and
// readable without running bd.
const ProjectMetadataFile = "metadata.json"

// Context is the payload of `bd context --json`: where the beads project
// lives and which backend it uses. bd reads it from its config files, so it
// works even when the database cannot be opened.
type Context struct {
	// BeadsDir is the .beads directory bd resolved.
	BeadsDir string `json:"beads_dir"`
	// RepoRoot is the repository that owns BeadsDir; CwdRepoRoot is the
	// repository of the working directory (they differ under a redirect).
	RepoRoot    string `json:"repo_root"`
	CwdRepoRoot string `json:"cwd_repo_root"`
	// IsRedirected is true when a .beads redirect file points elsewhere;
	// IsWorktree when bd resolved the project through a linked worktree.
	IsRedirected bool `json:"is_redirected"`
	IsWorktree   bool `json:"is_worktree"`
	// Backend is the storage backend ("dolt").
	Backend string `json:"backend"`
	// DoltMode is DoltModeEmbedded or DoltModeServer.
	DoltMode string `json:"dolt_mode"`
	// ServerHost is printed in server mode only.
	ServerHost string `json:"server_host,omitempty"`
	// Database is the Dolt database name (the issue prefix by default).
	Database string `json:"database"`
	// ProjectID is the uuid bd init minted for the project.
	ProjectID string `json:"project_id"`
	// Role is bd's git-config role for this clone ("maintainer",
	// "contributor").
	Role string `json:"role"`
	// BDVersion is the version of the bd that answered.
	BDVersion string `json:"bd_version"`
}

// Embedded reports whether the backend is embedded Dolt (the supported
// mode).
func (c Context) Embedded() bool { return c.DoltMode == DoltModeEmbedded }

// ProjectMetadata is the content of .beads/metadata.json as bd 1.2.2 writes
// it.
type ProjectMetadata struct {
	// Database and Backend name the storage engine ("dolt").
	Database string `json:"database"`
	Backend  string `json:"backend"`
	// DoltMode is DoltModeEmbedded or DoltModeServer.
	DoltMode string `json:"dolt_mode"`
	// DoltDatabase is the Dolt database name.
	DoltDatabase string `json:"dolt_database"`
	// ProjectID is the project uuid.
	ProjectID string `json:"project_id"`
}

// Embedded reports whether metadata.json declares embedded mode.
func (m ProjectMetadata) Embedded() bool { return m.DoltMode == DoltModeEmbedded }

// ReadProjectMetadata reads and decodes <beadsDir>/metadata.json. It never
// runs bd, so it is the cheapest way to learn the Dolt mode of a
// repository; a missing file satisfies errors.Is(err, os.ErrNotExist).
func ReadProjectMetadata(beadsDir string) (ProjectMetadata, error) {
	path := filepath.Join(beadsDir, ProjectMetadataFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return ProjectMetadata{}, fmt.Errorf("beads: reading %s: %w", path, err)
	}
	var m ProjectMetadata
	if err := json.Unmarshal(data, &m); err != nil {
		return ProjectMetadata{}, fmt.Errorf("beads: decoding %s: %w", path, err)
	}
	return m, nil
}

// ReadContext runs `bd -C <repo> context --json` outside any client queue
// (for sc doctor and other plumbing) and decodes the result. Failures are
// *Error values; a repository without .beads/ yields ErrNoProject.
func ReadContext(ctx context.Context, repo string) (Context, error) {
	args := []string{"context", "--json"}
	res, err := Run(ctx, CallOptions{Repo: repo}, args...)
	if err := classify(args, res, err); err != nil {
		return Context{}, err
	}
	var bc Context
	if err := Decode(res.Stdout, &bc); err != nil {
		return Context{}, fmt.Errorf("bd context: %w", err)
	}
	return bc, nil
}

// Context runs `bd context --json` through the client's queue.
func (c *Client) Context(ctx context.Context) (Context, error) {
	var bc Context
	if err := c.call(ctx, &bc, "context", "--json"); err != nil {
		return Context{}, err
	}
	return bc, nil
}

// HistoryEntry is one Dolt commit as `bd history <id> --json` prints it:
// the commit and the issue as it looked after that commit.
type HistoryEntry struct {
	// Commit is the Dolt commit hash.
	Commit string `json:"CommitHash"`
	// Committer is the Dolt committer name ("root" for embedded writes).
	Committer string `json:"Committer"`
	// Date is the Dolt commit time.
	Date time.Time `json:"CommitDate"`
	// Issue is the issue row at that commit (core fields only: id, title,
	// status, priority, type, owner, timestamps; no labels or metadata).
	Issue Issue `json:"Issue"`
}

// History runs `bd history <id> --json [--limit n]` and returns the Dolt
// commits recorded since the bead was created, newest first. With
// dolt.auto-commit on (the embedded default) every sc write is exactly one
// commit, so the list grows by one per write to any bead in the database
// (bd shows every commit made after the bead existed, not only those that
// changed it), which makes it the audit trail for debugging a run. limit
// 0 means all entries. bd prints `data: null` with exit 0 for an unknown
// id; that (or an empty list) is reported as ErrNotFound, because with
// auto-commit on a real bead always has at least its creation commit.
func (c *Client) History(ctx context.Context, id domain.BeadID, limit int) ([]HistoryEntry, error) {
	if strings.TrimSpace(string(id)) == "" {
		return nil, fmt.Errorf("beads: history: %w: empty id", ErrInvalid)
	}
	args := []string{"history", string(id), "--json"}
	if limit > 0 {
		args = append(args, "--limit", strconv.Itoa(limit))
	}
	var entries []HistoryEntry
	if err := c.call(ctx, &entries, args...); err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, &Error{
			Command: "history", Args: args, Kind: ErrNotFound,
			Message: fmt.Sprintf("no history for %s (unknown id, or %s is not %q)", id, AutoCommitKey, AutoCommitOn),
		}
	}
	return entries, nil
}

// DoltStatus is what CheckDolt learned about the repository's backend.
type DoltStatus struct {
	// Context is what `bd context --json` reported.
	Context Context
	// Metadata is .beads/metadata.json, when it could be read (the zero
	// value otherwise, with a warning).
	Metadata ProjectMetadata
	// AutoCommit is the dolt.auto-commit value bd reports.
	AutoCommit string
	// Warnings lists every departure from the supported setup, in the
	// words CheckDolt logged. Empty means embedded mode with auto-commit
	// on, the configuration every sc write relies on.
	Warnings []string
}

// Embedded reports whether bd runs the supported embedded backend.
func (s DoltStatus) Embedded() bool { return s.Context.Embedded() }

// CheckDolt verifies the Dolt setup D-21 assumes before a run writes
// anything: `bd context --json` must report embedded mode and
// `dolt.auto-commit` must be on, and .beads/metadata.json should agree.
// Nothing here is fatal: every departure is logged at Warn level through
// the client's logger and returned in DoltStatus.Warnings, and the run
// continues, because sc has not been tested against server mode rather
// than known to fail there. The error is non-nil only when bd itself
// failed (no project, timeout, bd missing).
func (c *Client) CheckDolt(ctx context.Context) (DoltStatus, error) {
	bc, err := c.Context(ctx)
	if err != nil {
		return DoltStatus{}, err
	}
	status := DoltStatus{Context: bc}
	if !bc.Embedded() {
		status.warn(ServerModeWarning(bc))
	}
	meta, err := ReadProjectMetadata(bc.BeadsDir)
	if err != nil {
		status.warn("could not read the beads project metadata: " + err.Error())
	} else {
		status.Metadata = meta
		if meta.DoltMode != bc.DoltMode {
			status.warn(fmt.Sprintf("%s says dolt_mode %q but bd context reports %q; trusting bd", ProjectMetadataFile, meta.DoltMode, bc.DoltMode))
		}
	}
	auto, err := c.ConfigGet(ctx, AutoCommitKey)
	if err != nil {
		status.log(ctx, c)
		return status, err
	}
	status.AutoCommit = auto
	if auto != AutoCommitOn && auto != "" {
		status.warn(AutoCommitWarning(auto))
	}
	status.log(ctx, c)
	return status, nil
}

// warn appends a warning.
func (s *DoltStatus) warn(text string) { s.Warnings = append(s.Warnings, text) }

// log writes every collected warning to the client's logger at Warn
// level. CheckDolt calls it on both exits so a warning is never returned
// without being logged.
func (s DoltStatus) log(ctx context.Context, c *Client) {
	for _, w := range s.Warnings {
		c.logger.WarnContext(ctx, w, "repo", c.repo, "dolt_mode", s.Context.DoltMode)
	}
}

// ServerModeWarning is the warning CheckDolt records when bd is not in
// embedded mode: sc continues, but has not been tested there.
func ServerModeWarning(bc Context) string {
	mode := bc.DoltMode
	if mode == "" {
		mode = "unknown"
	}
	text := fmt.Sprintf("beads reports Dolt mode %q; sc has only been tested against embedded mode and continues anyway", mode)
	if bc.ServerHost != "" {
		text += fmt.Sprintf(" (server %s)", bc.ServerHost)
	}
	return text
}

// AutoCommitWarning is the warning CheckDolt records when dolt.auto-commit
// is not on: writes stay in the Dolt working set until `bd dolt commit`,
// so `bd history` no longer shows one commit per sc write.
func AutoCommitWarning(value string) string {
	return fmt.Sprintf("%s is %q, not %q: writes are not Dolt commits until `bd dolt commit`, so `bd history` will not show one entry per sc write",
		AutoCommitKey, value, AutoCommitOn)
}
