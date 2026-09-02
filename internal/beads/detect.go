package beads

import (
	"context"
	"fmt"

	"slop-cannon/internal/proc"
)

// MinVersion is the oldest beads release whose CLI contract sc was verified
// against (D-21, Appendix A). Older binaries fail Detect.
var MinVersion = proc.V(1, 2, 2)

// InstallHint tells the user how to get a bd that satisfies MinVersion and
// runs embedded Dolt (Appendix A).
var InstallHint = fmt.Sprintf("install beads %s or newer: brew install beads, or the upstream install script "+
	"(scripts/install.sh at https://github.com/gastownhall/beads); "+
	"CGO_ENABLED=0 go install builds are server-mode only and cannot run embedded Dolt", MinVersion)

// PingHint explains a failed `bd ping`: the usual cause is a bd built
// without cgo, which cannot open the embedded database.
const PingHint = "embedded Dolt did not answer: CGO_ENABLED=0 go install builds are server-mode only; " +
	"reinstall with brew install beads or the upstream install script"

// Info is what `bd version --json` reported about the installed binary.
type Info struct {
	// Path is where bd was found on PATH.
	Path string `json:"path"`
	// Version is the parsed release number.
	Version proc.Version `json:"version"`
	// Build is bd's build identifier ("dev" for source builds).
	Build string `json:"build,omitempty"`
	// Branch is the source branch bd reports, when it does.
	Branch string `json:"branch,omitempty"`
}

// String renders the info the way sc doctor shows it: "1.2.2 (build dev,
// /usr/local/bin/bd)".
func (i Info) String() string {
	if i.Build != "" {
		return fmt.Sprintf("%s (build %s, %s)", i.Version, i.Build, i.Path)
	}
	return fmt.Sprintf("%s (%s)", i.Version, i.Path)
}

// PingResult is the payload of `bd ping --json`: the embedded database
// answered and how long each stage took.
type PingResult struct {
	// Status is "ok" when the database answered.
	Status string `json:"status"`
	// TotalMS is the round-trip time bd measured.
	TotalMS int64 `json:"total_ms"`
	// QueryMS, ResolveMS and StoreMS break TotalMS down.
	QueryMS   int64 `json:"query_ms"`
	ResolveMS int64 `json:"resolve_ms"`
	StoreMS   int64 `json:"store_ms"`
}

// Reason classifies why a bd check failed.
type Reason string

// Reason values.
const (
	// ReasonNotFound: bd is not on PATH.
	ReasonNotFound Reason = "not_found"
	// ReasonFailed: bd ran but exited non-zero, timed out, or could not start.
	ReasonFailed Reason = "failed"
	// ReasonBadOutput: bd exited 0 but printed nothing sc could decode.
	ReasonBadOutput Reason = "bad_output"
	// ReasonTooOld: bd is older than MinVersion.
	ReasonTooOld Reason = "too_old"
	// ReasonUnhealthy: `bd ping` answered with a status other than "ok".
	ReasonUnhealthy Reason = "unhealthy"
)

// CheckError is returned by Detect and Ping. Error renders a one-line
// detail suitable for a doctor row and Hint says what to do about it.
type CheckError struct {
	// Command is the bd command line that failed, e.g. "bd version --json".
	Command string
	// Reason classifies the failure.
	Reason Reason
	// Path is where bd was found, when it was.
	Path string
	// Version is the version found, for ReasonTooOld.
	Version proc.Version
	// Status is the ping status, for ReasonUnhealthy.
	Status string
	// Err is the underlying error, when there is one.
	Err error
}

// Error implements error.
func (e *CheckError) Error() string {
	switch e.Reason {
	case ReasonNotFound:
		return "not found on PATH"
	case ReasonFailed:
		return e.Command + " failed: " + ErrorSummary(e.Err)
	case ReasonTooOld:
		return fmt.Sprintf("%s is older than %s (%s)", e.Version, MinVersion, e.Path)
	case ReasonUnhealthy:
		return fmt.Sprintf("bd ping reported status %q", e.Status)
	default:
		if e.Err != nil {
			return e.Command + ": " + e.Err.Error()
		}
		return e.Command + ": unusable output"
	}
}

// Unwrap exposes the underlying error, so proc.IsNotFound and errors.Is on
// proc.ErrTimeout work through a CheckError.
func (e *CheckError) Unwrap() error { return e.Err }

// Hint returns the remedy for this failure: InstallHint for version
// problems, PingHint for a database that does not answer.
func (e *CheckError) Hint() string {
	if e.Reason == ReasonUnhealthy || e.Command == pingCommand {
		return PingHint
	}
	return InstallHint
}

// Command labels used in CheckError messages ("bd ping failed: …").
const (
	versionCommand = "bd version"
	pingCommand    = "bd ping"
)

// Detect locates bd on PATH, runs `bd version --json`, and enforces
// MinVersion. Any failure is a *CheckError whose Error and Hint are ready
// for sc doctor.
func Detect(ctx context.Context) (Info, error) {
	path, err := proc.LookPath(Binary)
	if err != nil {
		return Info{}, &CheckError{Command: versionCommand, Reason: ReasonNotFound, Err: err}
	}
	res, err := Run(ctx, CallOptions{}, "version", "--json")
	if err != nil {
		return Info{Path: path}, &CheckError{Command: versionCommand, Reason: ReasonFailed, Path: path, Err: err}
	}
	var reported struct {
		Version string `json:"version"`
		Build   string `json:"build"`
		Branch  string `json:"branch"`
	}
	if err := Decode(res.Stdout, &reported); err != nil {
		return Info{Path: path}, &CheckError{Command: versionCommand, Reason: ReasonBadOutput, Path: path, Err: err}
	}
	v, ok := proc.ParseVersion(reported.Version)
	if !ok {
		return Info{Path: path}, &CheckError{
			Command: versionCommand, Reason: ReasonBadOutput, Path: path,
			Err: fmt.Errorf("reported version %q (%s)", reported.Version, path),
		}
	}
	info := Info{Path: path, Version: v, Build: reported.Build, Branch: reported.Branch}
	if !v.AtLeast(MinVersion) {
		return info, &CheckError{Command: versionCommand, Reason: ReasonTooOld, Path: path, Version: v}
	}
	return info, nil
}

// Ping runs `bd -C <repo> ping --json` and reports whether the embedded
// database answered. It is the health check D-13 prescribes after init
// (`bd doctor --json` prints nothing in embedded mode). Failures are
// *CheckError values; a repo with no .beads/ yields ReasonFailed with bd's
// "no beads project found" line in the summary.
func Ping(ctx context.Context, repo string) (PingResult, error) {
	res, err := Run(ctx, CallOptions{Repo: repo}, "ping", "--json")
	if err != nil {
		reason := ReasonFailed
		if proc.IsNotFound(err) {
			reason = ReasonNotFound
		}
		return PingResult{}, &CheckError{Command: pingCommand, Reason: reason, Err: err}
	}
	var ping PingResult
	if err := Decode(res.Stdout, &ping); err != nil {
		return PingResult{}, &CheckError{Command: pingCommand, Reason: ReasonBadOutput, Err: err}
	}
	if ping.Status != "ok" {
		return ping, &CheckError{Command: pingCommand, Reason: ReasonUnhealthy, Status: ping.Status}
	}
	return ping, nil
}
