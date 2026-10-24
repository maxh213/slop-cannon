package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
	KeyExclude            = "exclude"
	KeyCheckCommand       = "check_command"
	KeyIdleReplanInterval = "idle_replan_interval"
	KeySignalsCommand     = "signals_command"
	KeySignalsInterval    = "signals_interval"
	KeyGatePollInterval   = "gate_poll_interval"
	KeyPRPollInterval     = "pr_poll_interval"
	KeyGraphPollInterval  = "graph_poll_interval"
	KeyMaxDiffBytes       = "max_diff_bytes"
	KeyReReviewThreshold  = "re_review_threshold"
	KeyMaxBeadsPerPlan    = "max_beads_per_plan"
	KeyNice               = "nice"
	KeyResearch           = "research"
	KeyAutoMerge          = "auto_merge"
	KeyAutoMergeChecks    = "auto_merge_checks"
	KeyAutoMergeSpacing   = "auto_merge_spacing"
	KeyConventionalTitles = "conventional_pr_titles"
	KeyMaxReviewRounds    = "limits.max_review_rounds"
	KeyMaxPushbackRounds  = "limits.max_pushback_rounds"
	KeyMaxConflictRounds  = "limits.max_conflict_rounds"
	KeyAgentTimeout       = "limits.agent_timeout"
	KeyMaxAgentTimeout    = "limits.max_agent_timeout"
	KeyCheckTimeout       = "limits.check_timeout"
	KeySignalsTimeout     = "limits.signals_timeout"
	KeyMaxTokens          = "limits.max_tokens"
	KeyMaxCostUSD         = "limits.max_cost_usd"
	KeyMaxPlanRejections  = "limits.max_plan_rejections"
	KeyMaxPlanFailures    = "limits.max_plan_failures"
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
	KeyActiveCartridge, KeyAmmo, KeySafety, KeyVibes, KeyRules, KeyExclude, KeyCheckCommand, KeyIdleReplanInterval,
	KeySignalsCommand, KeySignalsInterval, KeyGatePollInterval, KeyPRPollInterval, KeyGraphPollInterval, KeyMaxDiffBytes, KeyReReviewThreshold,
	KeyMaxBeadsPerPlan, KeyNice, KeyResearch,
	KeyAutoMerge, KeyAutoMergeChecks, KeyAutoMergeSpacing, KeyConventionalTitles,
	KeyMaxReviewRounds, KeyMaxPushbackRounds, KeyMaxConflictRounds, KeyAgentTimeout, KeyMaxAgentTimeout, KeyCheckTimeout, KeySignalsTimeout, KeyMaxTokens,
	KeyMaxCostUSD, KeyMaxPlanRejections, KeyMaxPlanFailures,
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
		return nil, locateDuration(parseError(path, err), path, data, raw)
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

// durationProbe is a decode target built from [File] with every *Duration
// field replaced by a *string, and everything else dropped. go-toml calls
// [Duration.UnmarshalText] with the raw text of a non-string value (a TOML
// integer, float or boolean) from a place that has no position to report,
// so that error arrives without a line; decoding the same document into
// the probe turns the same value into an ordinary "cannot decode TOML
// integer into ... string" error, which does carry the key and its
// position. It is built by reflection so a duration key added to File can
// never be forgotten here (P1-13).
var durationProbe = newDurationProbe(reflect.TypeOf(File{}))

// durationType is the type whose fields the probe replaces with strings.
var durationType = reflect.TypeOf(Duration{})

// newDurationProbe builds the probe struct type for t: every *Duration
// field becomes a *string with the same TOML tag, every nested struct is
// recursed into and kept only when it contributes a field, and every other
// field is dropped. It returns nil when t has no duration keys at all.
func newDurationProbe(t reflect.Type) reflect.Type {
	var fields []reflect.StructField
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported
		}
		switch {
		case f.Type.Kind() == reflect.Pointer && f.Type.Elem() == durationType:
			fields = append(fields, reflect.StructField{Name: f.Name, Type: reflect.TypeOf((*string)(nil)), Tag: f.Tag})
		case f.Type.Kind() == reflect.Struct:
			if inner := newDurationProbe(f.Type); inner != nil {
				fields = append(fields, reflect.StructField{Name: f.Name, Type: inner, Tag: f.Tag})
			}
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return reflect.StructOf(fields)
}

// locateDuration enriches a position-less duration error with the line and
// column of the offending key. err is returned unchanged when it already
// has a position, when it is not a *ParseError, or when the probe decode
// finds nothing wrong (P1-13).
func locateDuration(err error, path string, data []byte, raw map[string]any) error {
	var pe *ParseError
	if !errors.As(err, &pe) || pe.Line > 0 || durationProbe == nil {
		return err
	}
	probe := reflect.New(durationProbe).Interface()
	dec := toml.NewDecoder(strings.NewReader(string(data)))
	probeErr := dec.Decode(probe)
	if probeErr == nil {
		return err
	}
	var de *toml.DecodeError
	if !errors.As(probeErr, &de) {
		return err
	}
	key := strings.Join(de.Key(), ".")
	if key == "" {
		return err
	}
	line, col := de.Position()
	return &ParseError{Path: path, Line: line, Column: col, Msg: fmt.Sprintf(
		"%s: %s", key, invalidDurationMsg(rawValueText(raw, de.Key())))}
}

// rawValueText renders the value at key (a path of TOML key segments) in
// the decoded document the way it was written: "10" for an integer, "true"
// for a boolean. It falls back to Go's default formatting for anything
// else, and to the empty string when the path is not there.
func rawValueText(raw map[string]any, key []string) string {
	var cur any = raw
	for _, seg := range key {
		table, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		if cur, ok = table[seg]; !ok {
			return ""
		}
	}
	switch v := cur.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	default:
		return fmt.Sprint(v)
	}
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
			if err := ValidateCartridgeModel(name, c.Model); err != nil {
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
	if f.Exclude != nil {
		// Unioned global-then-repo like Vibes and Rules, and left as written:
		// the entries are globs, not paths, so they are never resolved
		// against the config file's directory (P9-05).
		e.Exclude = MergeExcludes(e.Exclude, f.Exclude)
		e.Sources[KeyExclude] = mergedSource(e.Sources[KeyExclude], src)
	}
	if f.CheckCommand != nil {
		e.CheckCommand = *f.CheckCommand
		e.Sources[KeyCheckCommand] = src
	}
	if f.IdleReplanInterval != nil {
		e.IdleReplanInterval = *f.IdleReplanInterval
		e.Sources[KeyIdleReplanInterval] = src
	}
	if f.SignalsCommand != nil {
		e.SignalsCommand = *f.SignalsCommand
		e.Sources[KeySignalsCommand] = src
	}
	if f.SignalsInterval != nil {
		e.SignalsInterval = *f.SignalsInterval
		e.Sources[KeySignalsInterval] = src
	}
	if f.GatePollInterval != nil {
		e.GatePollInterval = *f.GatePollInterval
		e.Sources[KeyGatePollInterval] = src
	}
	if f.PRPollInterval != nil {
		e.PRPollInterval = *f.PRPollInterval
		e.Sources[KeyPRPollInterval] = src
	}
	if f.GraphPollInterval != nil {
		e.GraphPollInterval = *f.GraphPollInterval
		e.Sources[KeyGraphPollInterval] = src
	}
	if f.MaxDiffBytes != nil {
		e.MaxDiffBytes = *f.MaxDiffBytes
		e.Sources[KeyMaxDiffBytes] = src
	}
	if f.ReReviewThreshold != nil {
		e.ReReviewThreshold = *f.ReReviewThreshold
		e.Sources[KeyReReviewThreshold] = src
	}
	if f.MaxBeadsPerPlan != nil {
		e.MaxBeadsPerPlan = *f.MaxBeadsPerPlan
		e.Sources[KeyMaxBeadsPerPlan] = src
	}
	if f.Nice != nil {
		e.Nice = *f.Nice
		e.Sources[KeyNice] = src
	}
	if f.Research != nil {
		e.Research = *f.Research
		e.Sources[KeyResearch] = src
	}
	if f.AutoMerge != nil {
		e.AutoMerge = *f.AutoMerge
		e.Sources[KeyAutoMerge] = src
	}
	if f.AutoMergeChecks != nil {
		// Unioned global-then-repo like exclude: a repository can require
		// one more check, never fewer than the global file asks for.
		e.AutoMergeChecks = MergeExcludes(e.AutoMergeChecks, f.AutoMergeChecks)
		e.Sources[KeyAutoMergeChecks] = mergedSource(e.Sources[KeyAutoMergeChecks], src)
	}
	if f.AutoMergeSpacing != nil {
		e.AutoMergeSpacing = *f.AutoMergeSpacing
		e.Sources[KeyAutoMergeSpacing] = src
	}
	if f.ConventionalPRTitles != nil {
		e.ConventionalPRTitles = *f.ConventionalPRTitles
		e.Sources[KeyConventionalTitles] = src
	}
	if v := f.Limits.MaxReviewRounds; v != nil {
		e.Limits.MaxReviewRounds = *v
		e.Sources[KeyMaxReviewRounds] = src
	}
	if v := f.Limits.MaxPushbackRounds; v != nil {
		e.Limits.MaxPushbackRounds = *v
		e.Sources[KeyMaxPushbackRounds] = src
	}
	if v := f.Limits.MaxConflictRounds; v != nil {
		e.Limits.MaxConflictRounds = *v
		e.Sources[KeyMaxConflictRounds] = src
	}
	if v := f.Limits.AgentTimeout; v != nil {
		e.Limits.AgentTimeout = *v
		e.Sources[KeyAgentTimeout] = src
	}
	if v := f.Limits.MaxAgentTimeout; v != nil {
		e.Limits.MaxAgentTimeout = *v
		e.Sources[KeyMaxAgentTimeout] = src
	}
	if v := f.Limits.CheckTimeout; v != nil {
		e.Limits.CheckTimeout = *v
		e.Sources[KeyCheckTimeout] = src
	}
	if v := f.Limits.SignalsTimeout; v != nil {
		e.Limits.SignalsTimeout = *v
		e.Sources[KeySignalsTimeout] = src
	}
	if v := f.Limits.MaxTokens; v != nil {
		e.Limits.MaxTokens = *v
		e.Sources[KeyMaxTokens] = src
	}
	if v := f.Limits.MaxCostUSD; v != nil {
		e.Limits.MaxCostUSD = *v
		e.Sources[KeyMaxCostUSD] = src
	}
	if v := f.Limits.MaxPlanRejections; v != nil {
		e.Limits.MaxPlanRejections = *v
		e.Sources[KeyMaxPlanRejections] = src
	}
	if v := f.Limits.MaxPlanFailures; v != nil {
		e.Limits.MaxPlanFailures = *v
		e.Sources[KeyMaxPlanFailures] = src
	}
	for name, rc := range f.Roles {
		role, err := domain.ParseRole(name)
		if err != nil {
			return &ValidationError{Path: path, Key: KeyRoles + "." + name, Msg: err.Error()}
		}
		if m := strings.TrimSpace(rc.Model); m != "" {
			e.RoleModels[role] = m
			e.Sources[KeyRoles+"."+role.String()+".model"] = src
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
	if e.SignalsInterval.Duration() <= 0 {
		return &ValidationError{Path: e.pathOf(KeySignalsInterval), Key: KeySignalsInterval, Msg: "must be a positive duration"}
	}
	if e.GatePollInterval.Duration() <= 0 {
		return &ValidationError{Path: e.pathOf(KeyGatePollInterval), Key: KeyGatePollInterval, Msg: "must be a positive duration"}
	}
	if e.PRPollInterval.Duration() <= 0 {
		return &ValidationError{Path: e.pathOf(KeyPRPollInterval), Key: KeyPRPollInterval, Msg: "must be a positive duration"}
	}
	if e.GraphPollInterval.Duration() <= 0 {
		return &ValidationError{Path: e.pathOf(KeyGraphPollInterval), Key: KeyGraphPollInterval, Msg: "must be a positive duration"}
	}
	if e.MaxDiffBytes < MinMaxDiffBytes {
		// Zero would be read by gitx as "use the default", and anything
		// under a kibibyte truncates every diff to nothing (P1-14).
		return &ValidationError{Path: e.pathOf(KeyMaxDiffBytes), Key: KeyMaxDiffBytes,
			Msg: fmt.Sprintf("must be at least %d (1 KiB)", MinMaxDiffBytes)}
	}
	if e.ReReviewThreshold <= 0 || e.ReReviewThreshold > 1 {
		// Zero would be read by gitx.ChangeRatio.Exceeds as "use the
		// default", and a fraction of the changed lines cannot exceed all
		// of them (D-09).
		return &ValidationError{Path: e.pathOf(KeyReReviewThreshold), Key: KeyReReviewThreshold,
			Msg: "must be greater than 0 and at most 1 (a fraction of the changed lines)"}
	}
	if e.MaxBeadsPerPlan < 1 {
		return &ValidationError{Path: e.pathOf(KeyMaxBeadsPerPlan), Key: KeyMaxBeadsPerPlan, Msg: "must be at least 1"}
	}
	if e.Nice < 0 || e.Nice > MaxNice {
		// A negative niceness needs privileges sc does not have, and would
		// raise the run above the rest of the machine rather than below it.
		return &ValidationError{Path: e.pathOf(KeyNice), Key: KeyNice,
			Msg: fmt.Sprintf("must be between 0 (leave the priority alone) and %d (the lowest)", MaxNice)}
	}
	if e.Limits.MaxReviewRounds < 1 {
		return &ValidationError{Path: e.pathOf(KeyMaxReviewRounds), Key: KeyMaxReviewRounds, Msg: "must be at least 1"}
	}
	if e.Limits.MaxPushbackRounds < 1 {
		// Zero used to be accepted, stored and shown, then read as "unset"
		// by the feedback loop, which gave a user who asked for no pushback
		// the default two exchanges. It is refused instead (P9-01).
		return &ValidationError{Path: e.pathOf(KeyMaxPushbackRounds), Key: KeyMaxPushbackRounds, Msg: "must be at least 1"}
	}
	if e.Limits.MaxConflictRounds < 1 {
		return &ValidationError{Path: e.pathOf(KeyMaxConflictRounds), Key: KeyMaxConflictRounds, Msg: "must be at least 1"}
	}
	if e.Limits.AgentTimeout.Duration() <= 0 {
		return &ValidationError{Path: e.pathOf(KeyAgentTimeout), Key: KeyAgentTimeout, Msg: "must be a positive duration"}
	}
	if e.Limits.MaxAgentTimeout.Duration() < e.Limits.AgentTimeout.Duration() {
		return &ValidationError{Path: e.pathOf(KeyMaxAgentTimeout), Key: KeyMaxAgentTimeout,
			Msg: fmt.Sprintf("must be at least limits.agent_timeout (%s)", e.Limits.AgentTimeout.String())}
	}
	if e.Limits.CheckTimeout.Duration() <= 0 {
		return &ValidationError{Path: e.pathOf(KeyCheckTimeout), Key: KeyCheckTimeout, Msg: "must be a positive duration"}
	}
	if e.Limits.SignalsTimeout.Duration() <= 0 {
		return &ValidationError{Path: e.pathOf(KeySignalsTimeout), Key: KeySignalsTimeout, Msg: "must be a positive duration"}
	}
	if e.Limits.MaxTokens < 0 {
		return &ValidationError{Path: e.pathOf(KeyMaxTokens), Key: KeyMaxTokens, Msg: "must not be negative (0 means unlimited)"}
	}
	if e.Limits.MaxCostUSD < 0 {
		return &ValidationError{Path: e.pathOf(KeyMaxCostUSD), Key: KeyMaxCostUSD, Msg: "must not be negative (0 means unlimited)"}
	}
	if e.Limits.MaxPlanRejections < 1 {
		return &ValidationError{Path: e.pathOf(KeyMaxPlanRejections), Key: KeyMaxPlanRejections, Msg: "must be at least 1"}
	}
	if e.Limits.MaxPlanFailures < 1 {
		return &ValidationError{Path: e.pathOf(KeyMaxPlanFailures), Key: KeyMaxPlanFailures, Msg: "must be at least 1"}
	}
	if e.AutoMergeSpacing.Duration() <= 0 {
		return &ValidationError{Path: e.pathOf(KeyAutoMergeSpacing), Key: KeyAutoMergeSpacing, Msg: "must be a positive duration"}
	}
	if e.AutoMerge && !e.Safety {
		return &ValidationError{Path: e.pathOf(KeyAutoMerge), Key: KeyAutoMerge, Msg: ErrAutoMergeNeedsSafety.Error()}
	}
	if e.AutoMerge && len(e.AutoMergeChecks) == 0 {
		return &ValidationError{Path: e.pathOf(KeyAutoMerge), Key: KeyAutoMerge, Msg: ErrAutoMergeNeedsChecks.Error()}
	}
	return nil
}

// ErrAutoMergeNeedsChecks refuses auto_merge without a name in
// auto_merge_checks (D-31): the rollup only lists the checks GitHub has
// created so far, so with nothing required a fast check that finishes
// before CI has registered the tests would read as all green.
var ErrAutoMergeNeedsChecks = errors.New("needs at least one auto_merge_checks name: without one, a pull request whose quick checks finish before its tests have even started reads as green")

// ErrAutoMergeNeedsSafety refuses auto_merge with safety off (D-31): the
// run only ever merges a pull request it opened itself, and safety off
// opens none — it merges into the default branch directly.
var ErrAutoMergeNeedsSafety = errors.New("needs safety = true: auto-merge merges the run's own pull requests, and safety off opens none")

// Overrides are the CLI flags that sit above both files (D-03). Nil means
// "not given".
type Overrides struct {
	Ammo      *int
	Safety    *bool
	Cartridge *string
	// AutoMerge is --auto-merge (D-31).
	AutoMerge *bool
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
	if o.AutoMerge != nil {
		e.AutoMerge = *o.AutoMerge
		e.Sources[KeyAutoMerge] = SourceFlag
	}
	if e.AutoMerge && !e.Safety {
		// Either half may come from a flag, so the pair is judged after
		// both are applied (D-31).
		return fmt.Errorf("auto_merge is on but safety is off: %w", ErrAutoMergeNeedsSafety)
	}
	if e.AutoMerge && len(e.AutoMergeChecks) == 0 {
		return fmt.Errorf("auto_merge is on: %w", ErrAutoMergeNeedsChecks)
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
	add(KeyExclude, formatList(e.Exclude), e.Sources[KeyExclude])
	add(KeyCheckCommand, e.CheckCommand, e.Sources[KeyCheckCommand])
	add(KeyIdleReplanInterval, e.IdleReplanInterval.String(), e.Sources[KeyIdleReplanInterval])
	add(KeySignalsCommand, e.SignalsCommand, e.Sources[KeySignalsCommand])
	add(KeySignalsInterval, e.SignalsInterval.String(), e.Sources[KeySignalsInterval])
	add(KeyGatePollInterval, e.GatePollInterval.String(), e.Sources[KeyGatePollInterval])
	add(KeyPRPollInterval, e.PRPollInterval.String(), e.Sources[KeyPRPollInterval])
	add(KeyGraphPollInterval, e.GraphPollInterval.String(), e.Sources[KeyGraphPollInterval])
	add(KeyMaxDiffBytes, strconv.Itoa(e.MaxDiffBytes), e.Sources[KeyMaxDiffBytes])
	add(KeyReReviewThreshold, strconv.FormatFloat(e.ReReviewThreshold, 'f', -1, 64), e.Sources[KeyReReviewThreshold])
	add(KeyMaxBeadsPerPlan, strconv.Itoa(e.MaxBeadsPerPlan), e.Sources[KeyMaxBeadsPerPlan])
	add(KeyNice, strconv.Itoa(e.Nice), e.Sources[KeyNice])
	add(KeyResearch, strconv.FormatBool(e.Research), e.Sources[KeyResearch])
	add(KeyAutoMerge, strconv.FormatBool(e.AutoMerge), e.Sources[KeyAutoMerge])
	add(KeyAutoMergeChecks, formatList(e.AutoMergeChecks), e.Sources[KeyAutoMergeChecks])
	add(KeyAutoMergeSpacing, e.AutoMergeSpacing.String(), e.Sources[KeyAutoMergeSpacing])
	add(KeyConventionalTitles, strconv.FormatBool(e.ConventionalPRTitles), e.Sources[KeyConventionalTitles])
	add(KeyMaxReviewRounds, strconv.Itoa(e.Limits.MaxReviewRounds), e.Sources[KeyMaxReviewRounds])
	add(KeyMaxPushbackRounds, strconv.Itoa(e.Limits.MaxPushbackRounds), e.Sources[KeyMaxPushbackRounds])
	add(KeyMaxConflictRounds, strconv.Itoa(e.Limits.MaxConflictRounds), e.Sources[KeyMaxConflictRounds])
	add(KeyAgentTimeout, e.Limits.AgentTimeout.String(), e.Sources[KeyAgentTimeout])
	add(KeyMaxAgentTimeout, e.Limits.MaxAgentTimeout.String(), e.Sources[KeyMaxAgentTimeout])
	add(KeyCheckTimeout, e.Limits.CheckTimeout.String(), e.Sources[KeyCheckTimeout])
	add(KeySignalsTimeout, e.Limits.SignalsTimeout.String(), e.Sources[KeySignalsTimeout])
	add(KeyMaxTokens, strconv.FormatInt(e.Limits.MaxTokens, 10), e.Sources[KeyMaxTokens])
	add(KeyMaxCostUSD, strconv.FormatFloat(e.Limits.MaxCostUSD, 'f', -1, 64), e.Sources[KeyMaxCostUSD])
	add(KeyMaxPlanRejections, strconv.Itoa(e.Limits.MaxPlanRejections), e.Sources[KeyMaxPlanRejections])
	add(KeyMaxPlanFailures, strconv.Itoa(e.Limits.MaxPlanFailures), e.Sources[KeyMaxPlanFailures])
	for _, role := range domain.Roles() {
		key := KeyRoles + "." + role.String() + ".cartridge"
		if name, ok := e.Roles[role]; ok {
			add(key, name, e.Sources[KeyRoles+"."+role.String()])
		} else {
			add(key, "", SourceDefault)
		}
		mkey := KeyRoles + "." + role.String() + ".model"
		if m, ok := e.RoleModels[role]; ok {
			add(mkey, m, e.Sources[mkey])
		} else {
			add(mkey, "", SourceDefault)
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
