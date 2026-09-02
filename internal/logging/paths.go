package logging

import (
	"path/filepath"
	"strings"

	"slop-cannon/internal/domain"
)

// File and directory names inside a run directory.
const (
	// RunLogName is the orchestrator's log file, <run-dir>/sc.log.
	RunLogName = "sc.log"
	// AgentsDirName holds one log per agent, <run-dir>/agents/<id>.log.
	AgentsDirName = "agents"
	// AgentLogExt is the extension of agent log files.
	AgentLogExt = ".log"
)

// RunPaths are the log locations for one run, derived from its directory by
// [PathsFor]. The run directory itself is <state-dir>/runs/<run-id>/ (D-23)
// and is chosen by the caller; this package never guesses it.
type RunPaths struct {
	// Dir is the run directory.
	Dir string
	// Log is the orchestrator log, Dir/sc.log.
	Log string
	// AgentsDir is the directory of per-agent logs, Dir/agents.
	AgentsDir string
}

// PathsFor computes the log paths under runDir. The directory is cleaned but
// not created or checked; see [OpenRunLog] for that.
func PathsFor(runDir string) RunPaths {
	dir := filepath.Clean(runDir)
	return RunPaths{
		Dir:       dir,
		Log:       filepath.Join(dir, RunLogName),
		AgentsDir: filepath.Join(dir, AgentsDirName),
	}
}

// AgentLog returns the log path for one agent, AgentsDir/<id>.log. The id
// is passed through [SafeFileName] so it can never escape AgentsDir.
func (p RunPaths) AgentLog(id domain.AgentID) string {
	return filepath.Join(p.AgentsDir, SafeFileName(id.String())+AgentLogExt)
}

// SafeFileName turns an identifier into a single path element: ASCII
// letters, digits, '_' and '-' are kept; a '.' is kept unless it would lead
// the name or follow another dot (so the result is never hidden and never
// contains ".."); every other byte becomes '_'; an empty input becomes "_".
func SafeFileName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
			b.WriteByte(c)
		case c == '.' && i > 0 && s[i-1] != '.':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}
