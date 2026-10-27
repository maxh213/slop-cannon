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
	// DriverNone is an account sc has no adapter for. The router is told to
	// skip it (DANDELION_DISABLE) and a route naming it is treated as no route.
	DriverNone Driver = ""
	// DriverClaude launches the account through the Claude Code adapter,
	// which holds every tool policy natively.
	DriverClaude Driver = "claude"
	// DriverCursor launches the account through the Cursor Agent adapter,
	// which has no tool controls, so only read_write firings may use it.
	DriverCursor Driver = "cursor"
	DriverAgy    Driver = "agy"
	DriverCodex  Driver = "codex"
	DriverKimi   Driver = "kimi"
	DriverGrok   Driver = "grok"
	DriverJunie  Driver = "junie"
	DriverHermes Driver = "hermes"
	DriverKilo   Driver = "kilo"
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
// claude-work and so does sc. Kilo's delegated firings additionally check its
// balance immediately before launch.
var Providers = []Provider{
	{ID: "claude", Driver: DriverClaude},
	{ID: "claude-work", Driver: DriverClaude, ConfigDirEnv: "DANDELION_CLAUDE_WORK_CONFIG_DIR", ConfigDirFallback: ".claude-work"},
	{ID: "claude-deepseek", Driver: DriverClaude, ConfigDirEnv: "DANDELION_CLAUDE_DEEPSEEK_CONFIG_DIR", ConfigDirFallback: ".claude-deepseek"},
	{ID: "claude-ollama", Driver: DriverClaude, ConfigDirEnv: "DANDELION_CLAUDE_OLLAMA_CONFIG_DIR", ConfigDirFallback: ".claude-ollama"},
	{ID: "agy", Driver: DriverAgy},
	{ID: "kimi", Driver: DriverKimi},
	{ID: "grok", Driver: DriverGrok},
	{ID: "codex", Driver: DriverCodex},
	{ID: "codex-animus", Driver: DriverCodex},
	{ID: "cursor", Driver: DriverCursor},
	{ID: "junie", Driver: DriverJunie},
	{ID: "hermes", Driver: DriverHermes},
	{ID: "kilo", Driver: DriverKilo},
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

// allows reports whether p's registered driver declares native enforcement
// for policy, or needs no restriction for read_write. Binary availability is
// checked separately when the allowed route set is built.
func (p Provider) allows(policy cartridge.ToolPolicy) bool {
	return driverAllows(builtinDrivers, p.Driver, policy)
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
	return allowedProviders(Providers, builtinDrivers, policy, func(driver Driver) bool {
		if driver == DriverCursor {
			return cursorInstalled
		}
		return driverAvailable(builtinDrivers, driver)
	})
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

// Efforts is the routed effort vocabulary shared by Claude, agy, Codex, Kimi,
// Grok, Hermes and Kilo. Junie declares a narrower list in its own adapter. Anything
// outside the selected driver's list is dropped rather than rewritten.
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
