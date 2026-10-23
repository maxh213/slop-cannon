package dandelion

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"slop-cannon/internal/cartridge"
)

// Driver says which of sc's adapters can launch a dandelion account.
type Driver string

// Drivers.
const (
	// DriverNone is an account sc has no adapter for (codex, grok, agy, …).
	// The router is told to skip it (DANDELION_DISABLE) and a route that
	// names it anyway is treated as no route.
	DriverNone Driver = ""
	// DriverClaude launches the account through the Claude Code adapter,
	// which holds every tool policy natively.
	DriverClaude Driver = "claude"
	// DriverCursor launches the account through the Cursor Agent adapter,
	// which has no tool controls, so only read_write firings may use it.
	DriverCursor Driver = "cursor"
)

// Provider is one dandelion provider id and how sc drives it. The table
// mirrors dandelion's src/app/launch.ts (TARGETS, ACCOUNT_HOMES) and
// src/probes/index.ts (rawProbes); the copies under testdata/dandelion pin
// the version it was checked against.
type Provider struct {
	// ID is the provider id: the account token a route line ends with and
	// a DANDELION_DISABLE entry.
	ID string
	// Driver is the adapter that launches it, DriverNone when sc has none.
	Driver Driver
	// ConfigDirEnv and ConfigDirFallback give a Claude account other than
	// the default one its CLAUDE_CONFIG_DIR, exactly as launch.ts does: the
	// value of ConfigDirEnv when set and non-empty, else
	// $HOME/ConfigDirFallback. Both are empty for the default home.
	ConfigDirEnv, ConfigDirFallback string
}

// Providers is every dandelion provider id in dandelion's own order, and
// the one place sc classifies them. claude-deepseek and claude-ollama are
// Claude Code with another config dir (their settings.json points the CLI
// at OpenRouter and Ollama Cloud), so launch.ts launches them exactly like
// claude-work and so does sc. kilo reports a balance and is never routed,
// but it is still probed unless disabled.
var Providers = []Provider{
	{ID: "claude", Driver: DriverClaude},
	{ID: "claude-work", Driver: DriverClaude, ConfigDirEnv: "DANDELION_CLAUDE_WORK_CONFIG_DIR", ConfigDirFallback: ".claude-work"},
	{ID: "claude-deepseek", Driver: DriverClaude, ConfigDirEnv: "DANDELION_CLAUDE_DEEPSEEK_CONFIG_DIR", ConfigDirFallback: ".claude-deepseek"},
	{ID: "claude-ollama", Driver: DriverClaude, ConfigDirEnv: "DANDELION_CLAUDE_OLLAMA_CONFIG_DIR", ConfigDirFallback: ".claude-ollama"},
	{ID: "agy"},
	{ID: "kimi"},
	{ID: "grok"},
	{ID: "codex"},
	{ID: "codex-animus"},
	{ID: "cursor", Driver: DriverCursor},
	{ID: "junie"},
	{ID: "hermes"},
	{ID: "kilo"},
}

// providerByID returns the table entry for id.
func providerByID(id string) (Provider, bool) {
	for _, p := range Providers {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

// allows reports whether a firing under policy may be delegated to p:
// Claude accounts always, Cursor only for read_write (it cannot hold
// read_only or read_exec, and a planning product owner has no worktree to
// copy), and nothing else ever.
func (p Provider) allows(policy cartridge.ToolPolicy) bool {
	switch p.Driver {
	case DriverClaude:
		return true
	case DriverCursor:
		return policy == cartridge.ToolReadWrite
	default:
		return false
	}
}

// configDir returns the CLAUDE_CONFIG_DIR the account launches with, or ""
// for the default home. lookup reads a variable; home is the user's home
// directory.
func (p Provider) configDir(lookup func(string) (string, bool), home func() (string, error)) (string, error) {
	if p.ConfigDirEnv == "" {
		return "", nil
	}
	if v, ok := lookup(p.ConfigDirEnv); ok && v != "" {
		return v, nil
	}
	h, err := home()
	if err != nil {
		return "", fmt.Errorf("resolving the %s config dir: %w", p.ID, err)
	}
	return filepath.Join(h, p.ConfigDirFallback), nil
}

// Allowed returns the provider ids a firing under policy may be routed to,
// in table order. cursorInstalled leaves cursor out when cursor-agent is
// not on PATH, so the router never names an account sc cannot start.
func Allowed(policy cartridge.ToolPolicy, cursorInstalled bool) []string {
	var out []string
	for _, p := range Providers {
		if !p.allows(policy) || (p.Driver == DriverCursor && !cursorInstalled) {
			continue
		}
		out = append(out, p.ID)
	}
	return out
}

// Disabled returns the DANDELION_DISABLE value that leaves the router only
// the allowed ids: every other provider in the table, plus whatever the
// user already disabled (extra), deduplicated in table order with the
// user's own unknown ids last.
func Disabled(allowed []string, extra string) string {
	var out []string
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	user := map[string]bool{}
	for _, id := range strings.Split(extra, ",") {
		user[strings.TrimSpace(id)] = true
	}
	for _, p := range Providers {
		if !slices.Contains(allowed, p.ID) || user[p.ID] {
			add(p.ID)
		}
	}
	for _, id := range strings.Split(extra, ",") {
		add(strings.TrimSpace(id))
	}
	return strings.Join(out, ",")
}

// Efforts are the values Claude Code's --effort flag accepts (verified
// against "claude --help" of 2.1.309). A route line's effort outside this
// list is dropped rather than passed on.
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// NoRoute is what the router prints when nothing is routable.
const NoRoute = "none"

// Route is one answer of "dandelion route": the model, the effort when the
// line has one, and the account to launch it on.
type Route struct {
	Model   string
	Effort  string
	Account string
}

// String renders the route the way the router printed it.
func (r Route) String() string {
	if r.Effort == "" {
		return r.Model + " " + r.Account
	}
	return r.Model + " " + r.Effort + " " + r.Account
}

// ParseRoute reads one route line, "<model>[ <effort>] <account>". The
// error names the line when it has the wrong number of fields; "none" is
// not a route and is an error too (the caller checks for it first).
func ParseRoute(line string) (Route, error) {
	f := strings.Fields(line)
	switch len(f) {
	case 2:
		return Route{Model: f[0], Account: f[1]}, nil
	case 3:
		return Route{Model: f[0], Effort: f[1], Account: f[2]}, nil
	default:
		return Route{}, fmt.Errorf("route line %q is not \"<model>[ <effort>] <account>\"", line)
	}
}
