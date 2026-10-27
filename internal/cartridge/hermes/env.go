package hermes

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// These are endpoint and credential inputs, not Hermes session/profile state.
// HERMES_HOME, HERMES_PROFILE and all other HERMES_* variables are deliberately
// excluded so a parent session cannot capture a firing.
var envNames = []string{
	"NOUS_API_KEY", "NOUS_BASE_URL", "NOUS_PORTAL_BASE_URL",
	"NOUS_INFERENCE_BASE_URL", "HERMES_PORTAL_BASE_URL",
}

// InheritEnv adds only Nous endpoint/auth inputs to base. The isolated HOME,
// HERMES_HOME and shared-auth path are set explicitly for each firing.
func InheritEnv(base []string) []string {
	set := make(map[string]bool, len(base)+len(envNames))
	for _, name := range base {
		set[name] = true
	}
	for _, name := range envNames {
		set[name] = true
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

type sessionHome struct {
	root       string
	hermes     string
	home       string
	sharedAuth string
	rootAuth   string
}

// newSessionHome allocates private state. Auth paths are inspected only with
// Stat; no credential contents are opened or copied by this package.
func newSessionHome(cfgEnv, specEnv map[string]string, timeout time.Duration) (*sessionHome, error) {
	root, err := os.MkdirTemp("", "sc-hermes-*")
	if err != nil {
		return nil, err
	}
	hermesRoot := originalHermesRoot(cfgEnv, specEnv)
	h := &sessionHome{
		root:       root,
		hermes:     filepath.Join(root, "hermes"),
		home:       filepath.Join(root, "home"),
		sharedAuth: sharedAuthDir(cfgEnv, specEnv),
	}
	if hermesRoot != "" {
		h.rootAuth = filepath.Join(hermesRoot, "auth.json")
	}
	for _, dir := range []string{h.hermes, h.home} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			_ = os.RemoveAll(root)
			return nil, err
		}
	}
	// Hermes' first-run inventory does not count explicit --provider nous.
	// A fresh non-secret model config satisfies that guard without inheriting
	// any user configuration or weakening the explicit runtime provider flag.
	config := []byte("model:\n  provider: nous\n")
	if timeout > 0 {
		config = append(config, []byte("agent:\n  run_budget_seconds: "+strconv.FormatFloat(timeout.Seconds(), 'f', -1, 64)+"\n")...)
	}
	if err := os.WriteFile(filepath.Join(h.hermes, "config.yaml"), config, 0o600); err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	if err := h.linkRootAuth(); err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	return h, nil
}

// linkRootAuth preserves an existing root login through Hermes' supported
// auth.json symlink alias. The lock alias is required too: each temporary home
// has a different path, but every process must flock the same root lock during
// token refresh. Hermes' atomic_replace resolves and preserves the auth alias.
func (h *sessionHome) linkRootAuth() error {
	if h.rootAuth == "" {
		return nil
	}
	info, err := os.Stat(h.rootAuth)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect root Hermes auth store: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("root Hermes auth store is not a regular file")
	}
	rootAuth, err := filepath.Abs(h.rootAuth)
	if err != nil {
		return err
	}
	for _, link := range []struct{ name, target string }{
		{"auth.json", rootAuth},
		{"auth.lock", filepath.Join(filepath.Dir(rootAuth), "auth.lock")},
	} {
		if err := os.Symlink(link.target, filepath.Join(h.hermes, link.name)); err != nil {
			return fmt.Errorf("link root Hermes %s: %w", link.name, err)
		}
	}
	return nil
}

func (h *sessionHome) cleanup() { _ = os.RemoveAll(h.root) }

func envLookup(cfgEnv, specEnv map[string]string, name string) (string, bool) {
	if value, ok := specEnv[name]; ok {
		return value, true
	}
	if value, ok := cfgEnv[name]; ok {
		return value, true
	}
	return os.LookupEnv(name)
}

func originalHermesRoot(cfgEnv, specEnv map[string]string) string {
	if value, ok := envLookup(cfgEnv, specEnv, "HERMES_HOME"); ok && strings.TrimSpace(value) != "" {
		root := filepath.Clean(value)
		if filepath.Base(filepath.Dir(root)) == "profiles" {
			root = filepath.Dir(filepath.Dir(root))
		}
		return root
	}
	if value, ok := envLookup(cfgEnv, specEnv, "HOME"); ok && strings.TrimSpace(value) != "" {
		return filepath.Join(value, ".hermes")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".hermes")
	}
	return ""
}

func sharedAuthDir(cfgEnv, specEnv map[string]string) string {
	if value, ok := envLookup(cfgEnv, specEnv, "HERMES_SHARED_AUTH_DIR"); ok && strings.TrimSpace(value) != "" {
		return value
	}
	if root := originalHermesRoot(cfgEnv, specEnv); root != "" {
		return filepath.Join(root, "shared")
	}
	return ""
}

func (a *Adapter) childEnv(h *sessionHome, specEnv map[string]string) map[string]string {
	env := make(map[string]string, len(a.cfg.Env)+len(specEnv)+4)
	for key, value := range a.cfg.Env {
		env[key] = value
	}
	for key, value := range specEnv {
		env[key] = value
	}
	env["HOME"] = h.home
	env["HERMES_HOME"] = h.hermes
	if h.sharedAuth != "" {
		env["HERMES_SHARED_AUTH_DIR"] = h.sharedAuth
	} else {
		delete(env, "HERMES_SHARED_AUTH_DIR")
	}
	return env
}
