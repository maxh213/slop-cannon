package proc

import (
	"os"
	"sort"
	"strings"
)

// Environ builds the child's environment from the allowlist in the Spec:
// PATH from the parent, every variable named in InheritEnv that is set in the
// parent, and finally the explicit Env values, which win on conflict. The
// result is sorted by name so it is stable for logging and tests.
func (s Spec) Environ() []string {
	vars := map[string]string{}
	if v, ok := os.LookupEnv("PATH"); ok {
		vars["PATH"] = v
	}
	for _, name := range s.InheritEnv {
		if v, ok := os.LookupEnv(name); ok {
			vars[name] = v
		}
	}
	for k, v := range s.Env {
		vars[k] = v
	}
	env := make([]string, 0, len(vars))
	for k, v := range vars {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	return env
}

// EnvValue returns the value of name from an environment slice in KEY=VALUE
// form, and whether it was present.
func EnvValue(env []string, name string) (string, bool) {
	prefix := name + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):], true
		}
	}
	return "", false
}
