package prompt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/logging"
)

// Names and permissions of the debug copies.
const (
	// PromptsDirName is the directory of prompt copies inside a run
	// directory: <run-dir>/prompts (D-23; the run directory lives under
	// XDG_STATE_HOME, never inside the target repository).
	PromptsDirName = "prompts"
	// PromptFileExt is the extension of a written prompt.
	PromptFileExt = ".md"

	// dirPerm and filePerm match internal/logging: a prompt quotes the
	// user's rules and vibes, so it is no more readable than the run log.
	dirPerm  = 0o700
	filePerm = 0o600
)

// Dir returns the prompts directory inside runDir, which the caller obtains
// from config.RunDir(repo, runID).
func Dir(runDir string) string { return filepath.Join(filepath.Clean(runDir), PromptsDirName) }

// PathFor returns where the prompt of one agent is written:
// <runDir>/prompts/<agent-id>.md. The id goes through
// logging.SafeFileName, so it can never escape the directory.
func PathFor(runDir string, id domain.AgentID) string {
	return filepath.Join(Dir(runDir), logging.SafeFileName(id.String())+PromptFileExt)
}

// WriteFile writes text as the prompt of one agent under runDir, creating
// the directory, and returns the path it wrote. An existing file is
// replaced, so a retry of the same agent leaves the prompt that was
// actually fired.
func WriteFile(runDir string, id domain.AgentID, text string) (string, error) {
	if runDir == "" {
		return "", errors.New("prompt: cannot write a prompt without a run directory")
	}
	if id == "" {
		return "", errors.New("prompt: cannot write a prompt without an agent id")
	}
	dir := Dir(runDir)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return "", fmt.Errorf("prompt: creating %s: %w", dir, err)
	}
	path := PathFor(runDir, id)
	if err := os.WriteFile(path, []byte(text), filePerm); err != nil {
		return "", fmt.Errorf("prompt: writing %s: %w", path, err)
	}
	return path, nil
}

// Write saves the prompt for debugging under runDir and records where it
// went in Path (D-23). Build does not call it: whoever fires the agent is
// the one that knows the run directory, so the firing path must call Write
// for the debug copy to exist. It is a pointer method because it sets that
// field; failing to write a debug copy is reported, never silently ignored,
// so a caller can decide whether an unwritable state directory should stop
// the run.
func (p *Prompt) Write(runDir string) error {
	path, err := WriteFile(runDir, p.AgentID, p.Text)
	if err != nil {
		return err
	}
	p.Path = path
	return nil
}
