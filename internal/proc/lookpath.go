package proc

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// probeTimeout bounds a version probe so a hung tool cannot stall sc doctor.
const probeTimeout = 10 * time.Second

// LookPath resolves name to an absolute path on PATH. The returned error
// wraps exec.ErrNotFound when the tool is missing, so callers can test with
// errors.Is and print an install hint.
func LookPath(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("proc: %s: %w", name, err)
	}
	return path, nil
}

// Tool is a binary located on PATH together with what it reported when asked
// for its version.
type Tool struct {
	// Name is the command name that was looked up.
	Name string
	// Path is the resolved absolute path.
	Path string
	// Output is the trimmed combined output of the version probe.
	Output string
	// Version is the first dotted number found in Output, if any.
	Version Version
	// HasVersion is false when no version number could be parsed.
	HasVersion bool
}

// Probe locates spec.Command on PATH and runs it with spec.Args (for example
// "--version") to learn its version. Only PATH and HOME are passed through
// unless spec says otherwise; a 10 s timeout applies when spec.Timeout is
// zero. The error wraps exec.ErrNotFound when the tool is absent and
// *ExitError when the probe command fails.
func Probe(ctx context.Context, spec Spec) (Tool, error) {
	path, err := LookPath(spec.Command)
	if err != nil {
		return Tool{}, err
	}
	if spec.Timeout == 0 {
		spec.Timeout = probeTimeout
	}
	if len(spec.InheritEnv) == 0 {
		spec.InheritEnv = []string{"HOME"}
	}
	res, err := Run(ctx, spec)
	if err != nil {
		return Tool{Name: spec.Command, Path: path}, err
	}
	out := strings.TrimSpace(res.Stdout)
	if out == "" {
		out = strings.TrimSpace(res.Stderr)
	}
	t := Tool{Name: spec.Command, Path: path, Output: out}
	t.Version, t.HasVersion = ParseVersion(out)
	return t, nil
}

// Version is a dotted numeric version. Missing components are zero, so
// "2.43" compares as 2.43.0.
type Version struct {
	Major, Minor, Patch int
}

// V is shorthand for a Version literal.
func V(major, minor, patch int) Version { return Version{major, minor, patch} }

// String renders the version as major.minor.patch.
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Compare returns -1, 0, or 1 as v is less than, equal to, or greater than o.
func (v Version) Compare(o Version) int {
	for _, d := range [3]int{v.Major - o.Major, v.Minor - o.Minor, v.Patch - o.Patch} {
		if d < 0 {
			return -1
		}
		if d > 0 {
			return 1
		}
	}
	return 0
}

// AtLeast reports whether v >= min.
func (v Version) AtLeast(min Version) bool { return v.Compare(min) >= 0 }

var versionRE = regexp.MustCompile(`(?:^|[^0-9.])v?(\d+)(?:\.(\d+))?(?:\.(\d+))?`)

// ParseVersion extracts the first dotted number from tool output such as
// "git version 2.43.0", "bd version 1.2.2 (build abc)", or "v1.2". It
// reports false when the text contains no number.
func ParseVersion(text string) (Version, bool) {
	m := versionRE.FindStringSubmatch(text)
	if m == nil {
		return Version{}, false
	}
	var v Version
	var err error
	if v.Major, err = strconv.Atoi(m[1]); err != nil {
		return Version{}, false
	}
	if m[2] != "" {
		if v.Minor, err = strconv.Atoi(m[2]); err != nil {
			return Version{}, false
		}
	}
	if m[3] != "" {
		if v.Patch, err = strconv.Atoi(m[3]); err != nil {
			return Version{}, false
		}
	}
	return v, true
}

// IsNotFound reports whether err means the tool is not installed on PATH.
func IsNotFound(err error) bool { return errors.Is(err, exec.ErrNotFound) }
