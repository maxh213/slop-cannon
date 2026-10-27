package agy

import (
	"fmt"
	"os"
)

// promptFileThreshold stays below Linux's 128 KiB single-argument limit with
// room for the other argv strings and platform differences.
const promptFileThreshold = 96 * 1024

const promptFilePattern = ".sc-agy-prompt-*"

func promptFileInstruction(path string) string {
	return fmt.Sprintf("Use the Read tool to read the exact file at %q. The complete contents of that file are the task instructions for this session. Follow those instructions exactly.", path)
}

func writePromptFile(workdir, prompt string) (string, error) {
	f, err := os.CreateTemp(workdir, promptFilePattern)
	if err != nil {
		return "", err
	}
	path := f.Name()
	complete := false
	defer func() {
		if !complete {
			_ = f.Close()
			_ = os.Remove(path)
		}
	}()
	if _, err := f.WriteString(prompt); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	complete = true
	return path, nil
}
