package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
)

// Doc is one config file opened for editing: its scope, path, and parsed
// contents. Mutating helpers change File in memory; Save writes it back
// atomically.
type Doc struct {
	// Scope is where the file lives.
	Scope Scope
	// Path is the file's location.
	Path string
	// Exists reports whether the file was present when opened.
	Exists bool
	// File is the parsed content (empty, at the current schema, when the
	// file did not exist).
	File File
}

// Open reads the config file for scope (repoRoot is used for ScopeRepo).
// A missing file yields an empty Doc that Save will create.
func Open(scope Scope, repoRoot string) (*Doc, error) {
	if scope == ScopeRepo && repoRoot == "" {
		return nil, errors.New("open repo config: repository root is empty")
	}
	path := PathFor(scope, repoRoot)
	f, exists, err := ReadFile(path)
	if err != nil {
		return nil, err
	}
	d := &Doc{Scope: scope, Path: path, Exists: exists}
	if f != nil {
		d.File = *f
	} else {
		d.File.SchemaVersion = schemaVersion
	}
	return d, nil
}

// OpenGlobal opens the global config file.
func OpenGlobal() (*Doc, error) { return Open(ScopeGlobal, "") }

// OpenRepo opens the repo config file of repoRoot.
func OpenRepo(repoRoot string) (*Doc, error) { return Open(ScopeRepo, repoRoot) }

// Dir is the directory holding the file. Vibes and rules paths are written
// relative to it when they live under Root.
func (d *Doc) Dir() string { return filepath.Dir(d.Path) }

// Root is the tree whose files are stored as relative paths: the repository
// for a repo Doc (its config sits in <repo>/.slop-cannon/, so a file at the
// repo root is written "../vibes.md" and the file stays committable), the
// config directory itself for the global Doc.
func (d *Doc) Root() string {
	if d.Scope == ScopeRepo {
		return filepath.Dir(d.Dir())
	}
	return d.Dir()
}

// Save writes the file atomically (see WriteAtomic), creating its directory.
// The global file is 0600 because cartridge env may hold tokens; the repo
// file is 0644 because it is meant to be committed.
func (d *Doc) Save() error {
	perm := os.FileMode(0o644)
	if d.Scope == ScopeGlobal {
		perm = 0o600
	}
	if err := SaveFile(d.Path, d.File, perm); err != nil {
		return err
	}
	d.Exists = true
	return nil
}

// SaveGlobal writes f as the global config file.
func SaveGlobal(f File) error { return SaveFile(GlobalPath(), f, 0o600) }

// SaveRepo writes f as the repo config file of repoRoot.
func SaveRepo(repoRoot string, f File) error {
	if repoRoot == "" {
		return errors.New("save repo config: repository root is empty")
	}
	return SaveFile(RepoConfigPath(repoRoot), f, 0o644)
}

// SaveFile marshals f (stamped with the current schema version) and writes
// it to path atomically with the given permissions.
func SaveFile(path string, f File, perm os.FileMode) error {
	f.SchemaVersion = schemaVersion
	data, err := Marshal(f)
	if err != nil {
		return err
	}
	return WriteAtomic(path, data, perm)
}

// Marshal renders f as TOML, with a header comment.
func Marshal(f File) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("# slop-cannon configuration; see docs/config.md for every key.\n")
	enc := toml.NewEncoder(&buf)
	enc.SetIndentTables(false)
	if err := enc.Encode(f); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	return buf.Bytes(), nil
}

// WriteAtomic writes data to path by creating a temporary file in the same
// directory, syncing it, and renaming it over path, so readers never see a
// partial file and a crash leaves either the old or the new content. The
// directory is created with 0755 (0700 when perm is 0600) if missing.
func WriteAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	dirPerm := os.FileMode(0o755)
	if perm&0o077 == 0 {
		dirPerm = 0o700
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if err = tmp.Chmod(perm); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// ErrNotGlobal is returned when a cartridge helper is used on a repo Doc:
// cartridges are subscriptions and live only in the global config (D-03).
var ErrNotGlobal = errors.New("cartridges live in the global config only; use the global file")

// AddCartridge adds (or, if already present, re-models) cartridge name with
// the given model. The first cartridge added becomes active (D-12);
// activated reports whether that happened. Names are checked per D-19.
func (d *Doc) AddCartridge(name, model string) (activated bool, err error) {
	if d.Scope != ScopeGlobal {
		return false, ErrNotGlobal
	}
	if err := ValidateCartridgeName(name); err != nil {
		return false, err
	}
	if d.File.Cartridges == nil {
		d.File.Cartridges = map[string]Cartridge{}
	}
	c := d.File.Cartridges[name]
	c.Model = model
	d.File.Cartridges[name] = c
	if d.File.ActiveCartridge == nil || *d.File.ActiveCartridge == "" {
		d.File.ActiveCartridge = &name
		return true, nil
	}
	return false, nil
}

// RemoveCartridge deletes cartridge name. When it was the active one the
// next configured cartridge (in name order) is promoted and returned in
// promoted; wasActive is true and promoted is "" when none remains, which
// the caller should warn about.
func (d *Doc) RemoveCartridge(name string) (promoted string, wasActive bool, err error) {
	if d.Scope != ScopeGlobal {
		return "", false, ErrNotGlobal
	}
	if _, ok := d.File.Cartridges[name]; !ok {
		return "", false, fmt.Errorf("cartridge %q is not configured", name)
	}
	delete(d.File.Cartridges, name)
	if d.File.ActiveCartridge != nil && *d.File.ActiveCartridge == name {
		wasActive = true
		if names := d.File.CartridgeNames(); len(names) > 0 {
			promoted = names[0]
		}
		next := promoted
		d.File.ActiveCartridge = &next
	}
	return promoted, wasActive, nil
}

// SetActive makes a configured cartridge the active one (sc cartridge --use).
func (d *Doc) SetActive(name string) error {
	if d.Scope != ScopeGlobal {
		return ErrNotGlobal
	}
	if err := ValidateCartridgeName(name); err != nil {
		return err
	}
	if _, ok := d.File.Cartridges[name]; !ok {
		return fmt.Errorf("cartridge %q is not configured; add it with: sc cartridge --add %s", name, name)
	}
	d.File.ActiveCartridge = &name
	return nil
}

// SetAmmo stores ammo after the MinAmmo check and returns the D-05
// allocation preview.
func (d *Doc) SetAmmo(ammo int) (Allocation, error) {
	alloc, err := Allocate(ammo)
	if err != nil {
		return Allocation{}, err
	}
	d.File.Ammo = &ammo
	return alloc, nil
}

// SetSafety stores the safety switch (D-10).
func (d *Doc) SetSafety(on bool) { d.File.Safety = &on }

// promptList names one of the two prompt-file lists.
type promptList string

const (
	listVibes promptList = "vibes"
	listRules promptList = "rules"
)

func (d *Doc) list(kind promptList) *[]string {
	if kind == listRules {
		return &d.File.Rules
	}
	return &d.File.Vibes
}

// AddVibes validates path (exists, regular, UTF-8 text, at most
// MaxPromptFileSize) and appends it to vibes. The stored form is relative
// to the config file's directory when the file lives inside it, otherwise
// absolute; it is returned. Adding a path already listed is a no-op.
func (d *Doc) AddVibes(path string) (stored string, err error) { return d.addPrompt(listVibes, path) }

// AddRules is AddVibes for the rules list.
func (d *Doc) AddRules(path string) (stored string, err error) { return d.addPrompt(listRules, path) }

// RemoveVibes drops path from vibes, matching on the resolved absolute
// path so either spelling works. A path not listed is an error.
func (d *Doc) RemoveVibes(path string) error { return d.removePrompt(listVibes, path) }

// RemoveRules is RemoveVibes for the rules list.
func (d *Doc) RemoveRules(path string) error { return d.removePrompt(listRules, path) }

// VibesPaths returns the vibes list resolved to absolute paths.
func (d *Doc) VibesPaths() []string { return d.resolved(listVibes) }

// RulesPaths returns the rules list resolved to absolute paths.
func (d *Doc) RulesPaths() []string { return d.resolved(listRules) }

func (d *Doc) resolved(kind promptList) []string {
	list := *d.list(kind)
	out := make([]string, len(list))
	for i, p := range list {
		out[i] = ResolvePath(d.Dir(), p)
	}
	return out
}

func (d *Doc) addPrompt(kind promptList, path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	if err := ValidatePromptFile(abs); err != nil {
		return "", err
	}
	stored := StoredPath(d.Dir(), d.Root(), abs)
	if containsString(d.resolved(kind), abs) {
		return stored, nil
	}
	list := d.list(kind)
	*list = append(*list, stored)
	return stored, nil
}

func (d *Doc) removePrompt(kind promptList, path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	list := d.list(kind)
	for i, p := range *list {
		if ResolvePath(d.Dir(), p) == abs {
			*list = append((*list)[:i], (*list)[i+1:]...)
			if len(*list) == 0 {
				*list = nil
			}
			return nil
		}
	}
	return fmt.Errorf("%s is not in %s (%s); nothing removed", path, kind, d.Path)
}

// StoredPath returns how the absolute file path abs is written into a config
// file living in dir: relative to dir when abs is under root (see Doc.Root),
// else unchanged. A relative result may begin with ".." when dir is below
// root.
func StoredPath(dir, root, abs string) string {
	inRoot, err := filepath.Rel(root, abs)
	if err != nil || inRoot == ".." || strings.HasPrefix(inRoot, ".."+string(filepath.Separator)) {
		return abs
	}
	rel, err := filepath.Rel(dir, abs)
	if err != nil {
		return abs
	}
	return rel
}

// ValidatePromptFile checks that path is an existing regular file of at
// most MaxPromptFileSize bytes containing valid UTF-8 text (no NUL bytes).
func ValidatePromptFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s: not a regular file", path)
	}
	if info.Size() > MaxPromptFileSize {
		return fmt.Errorf("%s: %d bytes exceeds the %d KiB limit", path, info.Size(), MaxPromptFileSize/1024)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if !utf8.Valid(data) {
		return fmt.Errorf("%s: not valid UTF-8 text", path)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return fmt.Errorf("%s: contains NUL bytes; not a text file", path)
	}
	return nil
}
