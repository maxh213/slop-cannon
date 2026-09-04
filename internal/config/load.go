package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"slop-cannon/internal/domain"
)

// Source says which layer supplied an effective value (D-03).
type Source string

// Sources, lowest precedence first. SourceMerged marks list keys (vibes,
// rules) whose global and repo entries were combined.
const (
	SourceDefault Source = "default"
	SourceGlobal  Source = "global"
	SourceRepo    Source = "repo"
	SourceMerged  Source = "global+repo"
	SourceFlag    Source = "flag"
)

// String returns the source name.
func (s Source) String() string { return string(s) }

// Keys of the effective configuration as used in [Effective.Sources] and
// [Entry.Key]. Cartridges and roles use "cartridges.<name>" and
// "roles.<role>".
const (
	KeyActiveCartridge    = "active_cartridge"
	KeyCartridges         = "cartridges"
	KeyAmmo               = "ammo"
	KeySafety             = "safety"
	KeyVibes              = "vibes"
	KeyRules              = "rules"
	KeyCheckCommand       = "check_command"
	KeyIdleReplanInterval = "idle_replan_interval"
	KeyGatePollInterval   = "gate_poll_interval"
	KeyMaxBeadsPerPlan    = "max_beads_per_plan"
	KeyMaxReviewRounds    = "limits.max_review_rounds"
	KeyMaxPushbackRounds  = "limits.max_pushback_rounds"
	KeyAgentTimeout       = "limits.agent_timeout"
	KeyMaxTokens          = "limits.max_tokens"
	KeyMaxCostUSD         = "limits.max_cost_usd"
	KeyRoles              = "roles"
)

// ParseError is a malformed config file: a TOML syntax error, an unknown
// key, or a value of the wrong type. Line and Column are 1-based; Column is
// 0 when unknown.
type ParseError struct {
	Path   string
	Line   int
	Column int
	Msg    string
}

// Error implements error as "<path>:<line>:<column>: <msg>".
func (e *ParseError) Error() string {
	switch {
	case e.Line > 0 && e.Column > 0:
		return fmt.Sprintf("%s:%d:%d: %s", e.Path, e.Line, e.Column, e.Msg)
	case e.Line > 0:
		return fmt.Sprintf("%s:%d: %s", e.Path, e.Line, e.Msg)
	}
	return fmt.Sprintf("%s: %s", e.Path, e.Msg)
}

// ValidationError is a well-formed file whose values break a rule (ammo
// below the minimum, an unknown cartridge name, ...).
type ValidationError struct {
	Path string
	Key  string
	Msg  string
}

// Error implements error as "<path>: <key>: <msg>".
func (e *ValidationError) Error() string {
	if e.Key == "" {
		return fmt.Sprintf("%s: %s", e.Path, e.Msg)
	}
	return fmt.Sprintf("%s: %s: %s", e.Path, e.Key, e.Msg)
}

// Effective is the merged configuration plus where each value came from.
type Effective struct {
	// Config holds the merged values.
	Config `json:"config"`
	// Sources maps every key (see the Key constants) to the layer that
	// supplied its value.
	Sources map[string]Source `json:"sources"`
	// GlobalPath is the global config file consulted.
	GlobalPath string `json:"global_path"`
	// GlobalExists reports whether the global file was present.
	GlobalExists bool `json:"global_exists"`
	// RepoRoot is the repository whose config was consulted; empty when
	// Load was given no path.
	RepoRoot string `json:"repo_root,omitempty"`
	// RepoPath is the repo config file consulted; empty when RepoRoot is.
	RepoPath string `json:"repo_path,omitempty"`
	// RepoExists reports whether the repo file was present.
	RepoExists bool `json:"repo_exists"`
	// Warnings are non-fatal problems, such as global-only keys found in
	// the repo file (they are ignored).
	Warnings []string `json:"warnings,omitempty"`
}

// Load reads the global file and, when repoPath is not empty, the repo file
// of the repository containing repoPath (found with FindRepoRoot; if there
// is none, repoPath itself is used as the root), and merges them over the
// defaults per D-03. Missing files are not errors. Malformed files return a
// *ParseError, invalid values a *ValidationError.
func Load(repoPath string) (*Effective, error) {
	root := ""
	if repoPath != "" {
		if r, ok := FindRepoRoot(repoPath); ok {
			root = r
		} else if abs, err := filepath.Abs(repoPath); err == nil {
			root = abs
		} else {
			root = repoPath
		}
	}
	return load(GlobalPath(), root)
}

// load merges the file at globalPath with the repo file under repoRoot.
func load(globalPath, repoRoot string) (*Effective, error) {
	eff := &Effective{
		Config:     Defaults(),
		Sources:    map[string]Source{},
		GlobalPath: globalPath,
		RepoRoot:   repoRoot,
	}
	for _, key := range scalarKeys {
		eff.Sources[key] = SourceDefault
	}
	eff.Sources[KeyCartridges] = SourceDefault

	global, exists, err := ReadFile(globalPath)
	if err != nil {
		return nil, err
	}
	eff.GlobalExists = exists
	if global != nil {
		if err := eff.mergeLayer(global, globalPath, SourceGlobal); err != nil {
			return nil, err
		}
	}

	if repoRoot != "" {
		eff.RepoPath = RepoConfigPath(repoRoot)
		repo, exists, err := ReadFile(eff.RepoPath)
		if err != nil {
			return nil, err
		}
		eff.RepoExists = exists
		if repo != nil {
			if err := eff.mergeLayer(repo, eff.RepoPath, SourceRepo); err != nil {
				return nil, err
			}
		}
	}

	if err := eff.validate(); err != nil {
		return nil, err
	}
	return eff, nil
}

// scalarKeys are the keys whose source is tracked one by one.
var scalarKeys = []string{
	KeyActiveCartridge, KeyAmmo, KeySafety, KeyVibes, KeyRules, KeyCheckCommand, KeyIdleReplanInterval,
	KeyGatePollInterval, KeyMaxBeadsPerPlan,
	KeyMaxReviewRounds, KeyMaxPushbackRounds, KeyAgentTimeout, KeyMaxTokens, KeyMaxCostUSD,
}

// ReadFile parses one config file. exists is false (with a nil File and no
// error) when the file is absent. The file is migrated to the current
// schema in memory when it carries an older schema_version.
func ReadFile(path string) (f *File, exists bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read config: %w", err)
	}
	f, err = Parse(path, data)
	if err != nil {
		return nil, true, err
	}
	return f, true, nil
}

// Parse decodes TOML config data. path is used only in error messages.
// Unknown keys are errors so typos never pass silently.
func Parse(path string, data []byte) (*File, error) {
	var raw map[string]any
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, parseError(path, err)
	}
	version, err := rawSchemaVersion(raw)
	if err != nil {
		return nil, &ParseError{Path: path, Msg: err.Error()}
	}
	if version > schemaVersion {
		return nil, &ParseError{Path: path, Msg: fmt.Sprintf(
			"schema_version %d was written by a newer sc (this build supports %d); upgrade sc", version, schemaVersion)}
	}
	if version < schemaVersion {
		if err := migrate(raw, version); err != nil {
			return nil, &ParseError{Path: path, Msg: err.Error()}
		}
		// Re-encode so the strict decode below sees the migrated document.
		// Positions in later errors refer to this migrated text.
		if data, err = toml.Marshal(raw); err != nil {
			return nil, &ParseError{Path: path, Msg: fmt.Sprintf("re-encode migrated config: %v", err)}
		}
	}

	var f File
	dec := toml.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, parseError(path, err)
	}
	if f.SchemaVersion == 0 {
		f.SchemaVersion = schemaVersion
	}
	return &f, nil
}

// rawSchemaVersion reads schema_version from a decoded document; a missing
// key means 1, the first version that existed.
func rawSchemaVersion(raw map[string]any) (int, error) {
	v, ok := raw["schema_version"]
	if !ok {
		return 1, nil
	}
	switch n := v.(type) {
	case int64:
		if n < 1 {
			return 0, fmt.Errorf("schema_version must be a positive integer, got %d", n)
		}
		return int(n), nil
	case float64:
		if n < 1 || n != float64(int64(n)) {
			return 0, fmt.Errorf("schema_version must be a positive integer, got %v", n)
		}
		return int(n), nil
	}
	return 0, fmt.Errorf("schema_version must be an integer, got %T", v)
}

// parseError converts a go-toml error into a *ParseError with a position.
func parseError(path string, err error) error {
	var strict *toml.StrictMissingError
	if errors.As(err, &strict) && len(strict.Errors) > 0 {
		e := strict.Errors[0]
		line, col := e.Position()
		key := strings.Join(e.Key(), ".")
		msg := fmt.Sprintf("unknown key %q", key)
		if len(strict.Errors) > 1 {
			msg += fmt.Sprintf(" (and %d more)", len(strict.Errors)-1)
		}
		return &ParseError{Path: path, Line: line, Column: col, Msg: msg}
	}
	var de *toml.DecodeError
	if errors.As(err, &de) {
		line, col := de.Position()
		msg := strings.TrimPrefix(de.Error(), "toml: ")
		if key := strings.Join(de.Key(), "."); key != "" {
			msg = fmt.Sprintf("%s: %s", key, msg)
		}
		return &ParseError{Path: path, Line: line, Column: col, Msg: msg}
	}
	return &ParseError{Path: path, Msg: strings.TrimPrefix(err.Error(), "toml: ")}
}

// mergeLayer applies one file on top of the current values, recording src
// for every key the file sets. Global-only keys in the repo file produce a
// warning and are ignored (D-03).
func (e *Effective) mergeLayer(f *File, path string, src Source) error {
	base := filepath.Dir(path)

	if src == SourceRepo {
		if f.ActiveCartridge != nil || len(f.Cartridges) > 0 {
			e.Warnings = append(e.Warnings, fmt.Sprintf(
				"%s: cartridges and active_cartridge are ignored here; they live in the global config %s (D-03)", path, e.GlobalPath))
		}
	} else {
		for name, c := range f.Cartridges {
			if err := ValidateCartridgeName(name); err != nil {
				return &ValidationError{Path: path, Key: KeyCartridges + "." + name, Msg: err.Error()}
			}
			e.Cartridges[name] = cloneCartridge(c)
			e.Sources[KeyCartridges+"."+name] = src
			e.Sources[KeyCartridges] = src
		}
		if f.ActiveCartridge != nil {
			e.ActiveCartridge = *f.ActiveCartridge
			e.Sources[KeyActiveCartridge] = src
		}
	}

	if f.Ammo != nil {
		e.Ammo = *f.Ammo
		e.Sources[KeyAmmo] = src
	}
	if f.Safety != nil {
		e.Safety = *f.Safety
		e.Sources[KeySafety] = src
	}
	if f.Vibes != nil {
		e.Vibes = mergeList(e.Vibes, f.Vibes, base)
		e.Sources[KeyVibes] = mergedSource(e.Sources[KeyVibes], src)
	}
	if f.Rules != nil {
		e.Rules = mergeList(e.Rules, f.Rules, base)
		e.Sources[KeyRules] = mergedSource(e.Sources[KeyRules], src)
	}
	if f.CheckCommand != nil {
		e.CheckCommand = *f.CheckCommand
		e.Sources[KeyCheckCommand] = src
	}
	if f.IdleReplanInterval != nil {
		e.IdleReplanInterval = *f.IdleReplanInterval
		e.Sources[KeyIdleReplanInterval] = src
	}
	if f.GatePollInterval != nil {
		e.GatePollInterval = *f.GatePollInterval
		e.Sources[KeyGatePollInterval] = src
	}
	if f.MaxBeadsPerPlan != nil {
		e.MaxBeadsPerPlan = *f.MaxBeadsPerPlan
		e.Sources[KeyMaxBeadsPerPlan] = src
	}
	if v := f.Limits.MaxReviewRounds; v != nil {
		e.Limits.MaxReviewRounds = *v
		e.Sources[KeyMaxReviewRounds] = src
	}
	if v := f.Limits.MaxPushbackRounds; v != nil {
		e.Limits.MaxPushbackRounds = *v
		e.Sources[KeyMaxPushbackRounds] = src
	}
	if v := f.Limits.AgentTimeout; v != nil {
		e.Limits.AgentTimeout = *v
		e.Sources[KeyAgentTimeout] = src
	}
	if v := f.Limits.MaxTokens; v != nil {
		e.Limits.MaxTokens = *v
		e.Sources[KeyMaxTokens] = src
	}
	if v := f.Limits.MaxCostUSD; v != nil {
		e.Limits.MaxCostUSD = *v
		e.Sources[KeyMaxCostUSD] = src
	}
	for name, rc := range f.Roles {
		role, err := domain.ParseRole(name)
		if err != nil {
			return &ValidationError{Path: path, Key: KeyRoles + "." + name, Msg: err.Error()}
		}
		if rc.Cartridge == "" {
			continue
		}
		if err := ValidateCartridgeName(rc.Cartridge); err != nil {
			return &ValidationError{Path: path, Key: KeyRoles + "." + name + ".cartridge", Msg: err.Error()}
		}
		e.Roles[role] = rc.Cartridge
		e.Sources[KeyRoles+"."+role.String()] = src
	}
	return nil
}

// mergedSource combines the source of a list key across layers: a list set
// in both files is SourceMerged.
func mergedSource(prev, src Source) Source {
	if prev == SourceDefault || prev == src {
		return src
	}
	return SourceMerged
}

// mergeList appends the entries of add (resolved against base) to cur,
// skipping paths already present. Lists union across layers so global
// vibes/rules still apply when a repo adds its own.
func mergeList(cur, add []string, base string) []string {
	out := append([]string(nil), cur...)
	for _, p := range add {
		abs := ResolvePath(base, p)
		if !containsString(out, abs) {
			out = append(out, abs)
		}
	}
	return out
}

// ResolvePath makes p absolute relative to base (the config file's
// directory) unless it already is absolute or starts with "~/".
func ResolvePath(base, p string) string {
	if strings.HasPrefix(p, "~/") || p == "~" {
		p = filepath.Join(homeDir(), strings.TrimPrefix(p, "~"))
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func cloneCartridge(c Cartridge) Cartridge {
	out := c
	if c.Args != nil {
		out.Args = append([]string(nil), c.Args...)
	}
	if c.Env != nil {
		out.Env = make(map[string]string, len(c.Env))
		for k, v := range c.Env {
			out.Env[k] = v
		}
	}
	return out
}

// pathOf returns the file that supplied key, for error messages.
func (e *Effective) pathOf(key string) string {
	switch e.Sources[key] {
	case SourceRepo, SourceMerged:
		return e.RepoPath
	case SourceGlobal:
		return e.GlobalPath
	}
	return "config"
}

// validate checks the merged values.
func (e *Effective) validate() error {
	if e.Ammo < MinAmmo {
		return &ValidationError{Path: e.pathOf(KeyAmmo), Key: KeyAmmo, Msg: ErrAmmoTooLow.Error()}
	}
	if e.ActiveCartridge != "" {
		if err := ValidateCartridgeName(e.ActiveCartridge); err != nil {
			return &ValidationError{Path: e.pathOf(KeyActiveCartridge), Key: KeyActiveCartridge, Msg: err.Error()}
		}
		if _, ok := e.Cartridges[e.ActiveCartridge]; !ok {
			return &ValidationError{Path: e.pathOf(KeyActiveCartridge), Key: KeyActiveCartridge,
				Msg: fmt.Sprintf("cartridge %q is not configured; add it with: sc cartridge --add %s", e.ActiveCartridge, e.ActiveCartridge)}
		}
	}
	if e.IdleReplanInterval.Duration() <= 0 {
		return &ValidationError{Path: e.pathOf(KeyIdleReplanInterval), Key: KeyIdleReplanInterval, Msg: "must be a positive duration"}
	}
	if e.GatePollInterval.Duration() <= 0 {
		return &ValidationError{Path: e.pathOf(KeyGatePollInterval), Key: KeyGatePollInterval, Msg: "must be a positive duration"}
	}
	if e.MaxBeadsPerPlan < 1 {
		return &ValidationError{Path: e.pathOf(KeyMaxBeadsPerPlan), Key: KeyMaxBeadsPerPlan, Msg: "must be at least 1"}
	}
	if e.Limits.MaxReviewRounds < 1 {
		return &ValidationError{Path: e.pathOf(KeyMaxReviewRounds), Key: KeyMaxReviewRounds, Msg: "must be at least 1"}
	}
	if e.Limits.MaxPushbackRounds < 0 {
		return &ValidationError{Path: e.pathOf(KeyMaxPushbackRounds), Key: KeyMaxPushbackRounds, Msg: "must not be negative"}
	}
	if e.Limits.AgentTimeout.Duration() <= 0 {
		return &ValidationError{Path: e.pathOf(KeyAgentTimeout), Key: KeyAgentTimeout, Msg: "must be a positive duration"}
	}
	if e.Limits.MaxTokens < 0 {
		return &ValidationError{Path: e.pathOf(KeyMaxTokens), Key: KeyMaxTokens, Msg: "must not be negative (0 means unlimited)"}
	}
	if e.Limits.MaxCostUSD < 0 {
		return &ValidationError{Path: e.pathOf(KeyMaxCostUSD), Key: KeyMaxCostUSD, Msg: "must not be negative (0 means unlimited)"}
	}
	return nil
}

// Overrides are the CLI flags that sit above both files (D-03). Nil means
// "not given".
type Overrides struct {
	Ammo      *int
	Safety    *bool
	Cartridge *string
}

// Apply layers flag values over the merged config and marks them
// SourceFlag. A cartridge must be configured; ammo must be at least MinAmmo.
func (e *Effective) Apply(o Overrides) error {
	if o.Ammo != nil {
		if *o.Ammo < MinAmmo {
			return fmt.Errorf("--ammo %d: %w", *o.Ammo, ErrAmmoTooLow)
		}
		e.Ammo = *o.Ammo
		e.Sources[KeyAmmo] = SourceFlag
	}
	if o.Safety != nil {
		e.Safety = *o.Safety
		e.Sources[KeySafety] = SourceFlag
	}
	if o.Cartridge != nil {
		name := *o.Cartridge
		if err := ValidateCartridgeName(name); err != nil {
			return err
		}
		if _, ok := e.Cartridges[name]; !ok {
			return fmt.Errorf("cartridge %q is not configured; add it with: sc cartridge --add %s", name, name)
		}
		e.ActiveCartridge = name
		e.Sources[KeyActiveCartridge] = SourceFlag
	}
	return nil
}

// Entry is one row of `sc config show`: a key, its rendered value, and the
// layer that supplied it.
type Entry struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Source Source `json:"source"`
}

// Entries lists every effective key in a stable display order with its
// value rendered as text and its source.
func (e *Effective) Entries() []Entry {
	var out []Entry
	add := func(key, value string, src Source) { out = append(out, Entry{Key: key, Value: value, Source: src}) }

	add(KeyActiveCartridge, e.ActiveCartridge, e.Sources[KeyActiveCartridge])
	names := make([]string, 0, len(e.Cartridges))
	for n := range e.Cartridges {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := e.Cartridges[n]
		src := e.Sources[KeyCartridges+"."+n]
		add(KeyCartridges+"."+n+".model", c.Model, src)
		add(KeyCartridges+"."+n+".command", c.Binary(n), src)
		if len(c.Args) > 0 {
			add(KeyCartridges+"."+n+".args", formatList(c.Args), src)
		}
		if len(c.Env) > 0 {
			keys := make([]string, 0, len(c.Env))
			for k := range c.Env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			pairs := make([]string, len(keys))
			for i, k := range keys {
				pairs[i] = k + "=" + c.Env[k]
			}
			add(KeyCartridges+"."+n+".env", formatList(pairs), src)
		}
	}
	add(KeyAmmo, strconv.Itoa(e.Ammo), e.Sources[KeyAmmo])
	add(KeySafety, strconv.FormatBool(e.Safety), e.Sources[KeySafety])
	add(KeyVibes, formatList(e.Vibes), e.Sources[KeyVibes])
	add(KeyRules, formatList(e.Rules), e.Sources[KeyRules])
	add(KeyCheckCommand, e.CheckCommand, e.Sources[KeyCheckCommand])
	add(KeyIdleReplanInterval, e.IdleReplanInterval.String(), e.Sources[KeyIdleReplanInterval])
	add(KeyGatePollInterval, e.GatePollInterval.String(), e.Sources[KeyGatePollInterval])
	add(KeyMaxBeadsPerPlan, strconv.Itoa(e.MaxBeadsPerPlan), e.Sources[KeyMaxBeadsPerPlan])
	add(KeyMaxReviewRounds, strconv.Itoa(e.Limits.MaxReviewRounds), e.Sources[KeyMaxReviewRounds])
	add(KeyMaxPushbackRounds, strconv.Itoa(e.Limits.MaxPushbackRounds), e.Sources[KeyMaxPushbackRounds])
	add(KeyAgentTimeout, e.Limits.AgentTimeout.String(), e.Sources[KeyAgentTimeout])
	add(KeyMaxTokens, strconv.FormatInt(e.Limits.MaxTokens, 10), e.Sources[KeyMaxTokens])
	add(KeyMaxCostUSD, strconv.FormatFloat(e.Limits.MaxCostUSD, 'f', -1, 64), e.Sources[KeyMaxCostUSD])
	for _, role := range domain.Roles() {
		key := KeyRoles + "." + role.String() + ".cartridge"
		if name, ok := e.Roles[role]; ok {
			add(key, name, e.Sources[KeyRoles+"."+role.String()])
		} else {
			add(key, "", SourceDefault)
		}
	}
	return out
}

// formatList renders a string list as "[]" or "[a, b]".
func formatList(list []string) string {
	if len(list) == 0 {
		return "[]"
	}
	return "[" + strings.Join(list, ", ") + "]"
}
