package kimi

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"slop-cannon/internal/cartridge"
)

const (
	toolTodo       = "TodoList"
	toolShell      = "Bash"
	toolTaskList   = "TaskList"
	toolTaskOutput = "TaskOutput"
	toolTaskStop   = "TaskStop"
	toolWaitFor    = "WaitFor"
	toolRead       = "Read"
	toolReadMedia  = "ReadMediaFile"
	toolGlob       = "Glob"
	toolGrep       = "Grep"
	toolSearchWeb  = "WebSearch"
	toolFetchURL   = "FetchURL"

	profileFileMode = 0o600
)

var (
	readTools = []string{toolRead, toolReadMedia, toolGlob, toolGrep}
	execTools = []string{toolShell, toolTaskList, toolTaskOutput, toolTaskStop, toolWaitFor}
	webTools  = []string{toolSearchWeb, toolFetchURL}
	// ErrReadOnlyUnavailable explains why this adapter cannot safely fire a
	// planner under ToolReadOnly.
	ErrReadOnlyUnavailable = errors.New("kimi: read_only is unavailable: disabling Bash breaks the planner and allowing unrestricted Bash is unsafe")
)

// Profile renders a Kimi Code 2.x Markdown agent file with YAML frontmatter.
// The body expands the built-in prompt; additional instructions travel in the
// user input instead, so template-looking text is never interpreted by Kimi's
// system-prompt renderer. Empty tools under read_write means all built-in and
// configured tools. An explicit empty subagent list is enforced for both
// policies, preventing a coder subagent from bypassing read_exec.
func Profile(spec cartridge.RunSpec) (string, error) {
	tools, err := profileTools(spec)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("---\nname: sc-kimi\ndescription: Slop Cannon unattended agent\n")
	if tools != nil {
		b.WriteString("tools:\n")
		for _, tool := range tools {
			encoded, _ := json.Marshal(tool)
			b.WriteString("  - ")
			b.Write(encoded)
			b.WriteByte('\n')
		}
	}
	b.WriteString("subagents: []\n---\n\n${base_prompt}\n")
	return b.String(), nil
}

func profileTools(spec cartridge.RunSpec) ([]string, error) {
	var tools []string
	switch spec.ToolPolicy {
	case cartridge.ToolReadExec:
		tools = append(tools, toolTodo)
		tools = append(tools, execTools...)
		tools = append(tools, readTools...)
	case cartridge.ToolReadWrite:
		return nil, nil
	case cartridge.ToolReadOnly:
		return nil, ErrReadOnlyUnavailable
	default:
		return nil, fmt.Errorf("kimi: unsupported tool policy %q", spec.ToolPolicy)
	}
	if spec.Web {
		tools = append(tools, webTools...)
	}
	return tools, nil
}

func writeProfile(dir string, spec cartridge.RunSpec) (string, error) {
	contents, err := Profile(spec)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "agent-*.md")
	if err != nil {
		return "", fmt.Errorf("kimi: create agent profile: %w", err)
	}
	path := f.Name()
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(path)
		}
	}()
	if _, err := f.WriteString(contents); err != nil {
		return "", fmt.Errorf("kimi: write agent profile: %w", err)
	}
	if err := f.Chmod(profileFileMode); err != nil {
		return "", fmt.Errorf("kimi: protect agent profile: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("kimi: close agent profile: %w", err)
	}
	ok = true
	return path, nil
}
