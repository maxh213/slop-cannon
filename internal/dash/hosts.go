package dash

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

var defaultAllowedHosts = []string{"localhost", "127.0.0.1", "::1"}

// NormalizeAllowedHosts validates exact DNS names or IP literals accepted in
// the HTTP Host header. Schemes, ports, paths, wildcards and empty names are
// rejected. Names are case-folded and a final DNS dot is removed.
func NormalizeAllowedHosts(hosts []string) ([]string, error) {
	out := make([]string, 0, len(hosts))
	seen := make(map[string]bool, len(hosts))
	for _, input := range hosts {
		host, err := normalizeAllowedHost(input)
		if err != nil {
			return nil, err
		}
		if !seen[host] {
			seen[host] = true
			out = append(out, host)
		}
	}
	return out, nil
}

func normalizeAllowedHost(input string) (string, error) {
	host := strings.ToLower(strings.TrimSpace(input))
	if host == "" {
		return "", errors.New("dash: allowed host must not be empty")
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	if net.ParseIP(host) != nil {
		return host, nil
	}
	if len(host) > 253 || strings.ContainsAny(host, "/?#@:") {
		return "", fmt.Errorf("dash: invalid allowed host %q; use an exact hostname or IP without a scheme, port or path", input)
	}
	host = strings.TrimSuffix(host, ".")
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("dash: invalid allowed host %q", input)
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return "", fmt.Errorf("dash: invalid allowed host %q; use an ASCII hostname or IP", input)
			}
		}
	}
	return host, nil
}

func allowedHostSet(explicit []string) map[string]bool {
	set := make(map[string]bool, len(defaultAllowedHosts)+len(explicit))
	for _, host := range defaultAllowedHosts {
		set[host] = true
	}
	for _, host := range explicit {
		set[host] = true
	}
	return set
}

// requestHostAllowed accepts a Host header only when its normalized hostname
// is explicitly allowed. Ports are syntax-checked but do not widen the match.
func requestHostAllowed(header string, allowed map[string]bool) bool {
	host := strings.ToLower(strings.TrimSpace(header))
	if host == "" {
		return false
	}
	if strings.HasPrefix(host, "[") {
		end := strings.LastIndex(host, "]")
		if end < 0 {
			return false
		}
		literal := host[1:end]
		if net.ParseIP(literal) == nil {
			return false
		}
		rest := host[end+1:]
		if rest != "" {
			if !strings.HasPrefix(rest, ":") || !validHostPort(rest[1:]) {
				return false
			}
		}
		return allowed[literal]
	}
	if ip := net.ParseIP(host); ip != nil {
		return allowed[host]
	}
	if strings.Contains(host, ":") {
		name, port, err := net.SplitHostPort(host)
		if err != nil || !validHostPort(port) {
			return false
		}
		host = name
	}
	host, err := normalizeAllowedHost(host)
	return err == nil && allowed[host]
}

func validHostPort(port string) bool {
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n <= 65535
}
