package junie

import (
	"io"
	"os"
	"strings"
)

const (
	promptFileThreshold = 32 * 1024
	promptFilePattern   = ".sc-junie-task-*.txt"
	promptFilePrefix    = "Read the file at this exact path: "
	promptFileSuffix    = ". Its complete contents are the task instructions; follow them as the task."
)

// taskForSpec keeps large prompts below Linux's per-argument size limit. The
// system prompt remains a separate flag; the spill file contains Prompt exactly.
func taskForSpec(specPrompt, workdir string) (task, promptPath string, err error) {
	if len(specPrompt) <= promptFileThreshold {
		return specPrompt, "", nil
	}
	f, err := os.CreateTemp(workdir, promptFilePattern)
	if err != nil {
		return "", "", err
	}
	promptPath = f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(promptPath)
		}
	}()
	if _, err = io.WriteString(f, specPrompt); err != nil {
		_ = f.Close()
		return "", "", err
	}
	if err = f.Close(); err != nil {
		return "", "", err
	}
	return promptFilePrefix + promptPath + promptFileSuffix, promptPath, nil
}

func removePromptFile(path string) {
	if path != "" {
		_ = os.Remove(path)
	}
}

func promptPathFromInstruction(task string) string {
	if !strings.HasPrefix(task, promptFilePrefix) || !strings.HasSuffix(task, promptFileSuffix) {
		return ""
	}
	path := strings.TrimSuffix(strings.TrimPrefix(task, promptFilePrefix), promptFileSuffix)
	if path == "" {
		return ""
	}
	return path
}
