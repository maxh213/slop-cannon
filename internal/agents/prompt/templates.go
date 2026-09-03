package prompt

import (
	"embed"
	"fmt"
	"io/fs"
	"strings"

	"slop-cannon/internal/domain"
)

// templateFiles holds the role base prompts: one `<role>.md` per
// domain.Role and one `lens_<lens>.md` per domain.Lens. They are the only
// part of a prompt that is fixed at build time; everything else is assembled
// from the run's configuration and state.
//
//go:embed templates/*.md
var templateFiles embed.FS

// TemplatesDir is the path of the embedded template directory inside this
// package.
const TemplatesDir = "templates"

// TemplateFS returns the embedded role and lens templates as a filesystem
// whose root holds the `.md` files, so a caller can list or publish them.
func TemplateFS() fs.FS {
	sub, err := fs.Sub(templateFiles, TemplatesDir)
	if err != nil {
		// Impossible: TemplatesDir is a literal directory in the embed pattern.
		panic(fmt.Errorf("prompt: embedded template dir: %w", err))
	}
	return sub
}

// RoleTemplate returns the base prompt for a role: the paragraph block every
// agent of that role starts from, with no trailing newline. An unknown role
// yields an error wrapping domain.ErrUnknownValue.
func RoleTemplate(r domain.Role) (string, error) {
	if !r.Valid() {
		return "", fmt.Errorf("%w: role %q has no prompt template", domain.ErrUnknownValue, string(r))
	}
	return readTemplate(r.String() + ".md")
}

// LensTemplate returns the extra paragraph a tester of that lens is given.
// An unknown lens yields an error wrapping domain.ErrUnknownValue.
func LensTemplate(l domain.Lens) (string, error) {
	if !l.Valid() {
		return "", fmt.Errorf("%w: lens %q has no prompt template", domain.ErrUnknownValue, string(l))
	}
	return readTemplate("lens_" + l.String() + ".md")
}

// readTemplate reads one embedded template and trims its trailing newline so
// the renderer controls all blank lines.
func readTemplate(name string) (string, error) {
	b, err := fs.ReadFile(TemplateFS(), name)
	if err != nil {
		return "", fmt.Errorf("prompt: reading template %s: %w", name, err)
	}
	return strings.TrimRight(string(b), "\n"), nil
}
