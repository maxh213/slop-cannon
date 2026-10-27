package kilo

import (
	"encoding/json"
	"errors"
	"fmt"

	"slop-cannon/internal/cartridge"
)

// ErrReadOnlyUnavailable explains why Kilo cannot enforce the planner policy.
var ErrReadOnlyUnavailable = errors.New("kilo: read_only is unavailable: Bash is not a read-only execution boundary")

type generatedConfig struct {
	Agent map[string]generatedAgent `json:"agent"`
}

type generatedAgent struct {
	Description string            `json:"description"`
	Mode        string            `json:"mode"`
	Steps       int               `json:"steps,omitempty"`
	Permission  map[string]string `json:"permission"`
}

// ConfigContent renders the private sc agent. Explicit deny rules remain in
// force under --auto; web tools are enabled only when RunSpec.Web requests them.
func ConfigContent(spec cartridge.RunSpec) (string, error) {
	permission, err := permissions(spec)
	if err != nil {
		return "", err
	}
	content, err := json.Marshal(generatedConfig{Agent: map[string]generatedAgent{
		"sc": {
			Description: "Slop Cannon unattended agent",
			Mode:        "primary",
			Steps:       spec.MaxTurns,
			Permission:  permission,
		},
	}})
	if err != nil {
		return "", fmt.Errorf("kilo: encode agent config: %w", err)
	}
	return string(content), nil
}

func permissions(spec cartridge.RunSpec) (map[string]string, error) {
	web := "deny"
	if spec.Web {
		web = "allow"
	}
	permission := map[string]string{
		"task":       "deny",
		"webfetch":   web,
		"websearch":  web,
		"codesearch": web,
	}
	switch spec.ToolPolicy {
	case cartridge.ToolReadExec:
		permission["bash"] = "allow"
		permission["edit"] = "deny"
	case cartridge.ToolReadWrite:
		permission["bash"] = "allow"
		permission["edit"] = "allow"
	case cartridge.ToolReadOnly:
		return nil, ErrReadOnlyUnavailable
	default:
		return nil, fmt.Errorf("kilo: unsupported tool policy %q", spec.ToolPolicy)
	}
	return permission, nil
}
