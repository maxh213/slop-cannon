package prompt

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"slop-cannon/internal/config"
)

// Doc is one rules or vibes file, quoted verbatim in the prompt. The content
// is never rewritten, summarised or truncated: a rules file the agent only
// half sees is worse than no rules file at all.
type Doc struct {
	// Name is what the prompt calls the file, normally its base name.
	Name string
	// Path is where it was read from; kept for logs and error messages.
	Path string
	// Content is the file as written, with trailing blank lines removed.
	Content string
}

// LoadDoc reads one rules or vibes file. It applies the same guards as
// `sc rules --add` (regular file, at most config.MaxPromptFileSize, valid
// UTF-8 text) so a run fails with the same message the config command would
// have given.
func LoadDoc(path string) (Doc, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Doc{}, fmt.Errorf("prompt: %s: %w", path, err)
	}
	if err := config.ValidatePromptFile(abs); err != nil {
		return Doc{}, fmt.Errorf("prompt: %w", err)
	}
	data, err := os.ReadFile(abs) //nolint:gosec // the path comes from the user's own config
	if err != nil {
		return Doc{}, fmt.Errorf("prompt: %s: %w", abs, err)
	}
	return Doc{
		Name:    filepath.Base(abs),
		Path:    abs,
		Content: strings.TrimRight(string(data), "\n \t"),
	}, nil
}

// LoadDocs reads every path in order (config.Config.Vibes or .Rules, which
// are absolute). It stops at the first unreadable file, because a run that
// silently drops a rules file is a run whose testers cannot do their job.
func LoadDocs(paths []string) ([]Doc, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	docs := make([]Doc, 0, len(paths))
	for _, p := range paths {
		d, err := LoadDoc(p)
		if err != nil {
			return nil, err
		}
		docs = append(docs, d)
	}
	return docs, nil
}

// renderDocs renders a rules or vibes section: one sub-heading per file,
// then its content verbatim.
func renderDocs(docs []Doc, preamble string) string {
	if len(docs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(preamble)
	for _, d := range docs {
		name := strings.TrimSpace(d.Name)
		if name == "" {
			name = filepath.Base(d.Path)
		}
		fmt.Fprintf(&b, "\n\n### %s\n\n%s", name, strings.TrimRight(d.Content, "\n"))
	}
	return b.String()
}
