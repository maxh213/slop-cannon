package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"slop-cannon/internal/config"
	"slop-cannon/internal/proc"
)

// Minimum tool versions sc doctor enforces (Appendix A for bd).
var (
	// DoctorMinGit is the oldest git that supports every command sc uses.
	DoctorMinGit = proc.V(2, 30, 0)
	// DoctorMinBD is the oldest beads release whose CLI contract sc was
	// verified against (D-21, Appendix A).
	DoctorMinBD = proc.V(1, 2, 2)
)

// doctorTimeout bounds every probe sc doctor runs; gh auth status talks to
// the network, so it is longer than proc's default.
const doctorTimeout = 20 * time.Second

// Install hints printed under failing rows.
const (
	doctorHintGit = "install git 2.30 or newer (https://git-scm.com/downloads)"
	doctorHintBD  = "install beads 1.2.2 or newer: brew install beads, or the upstream install script " +
		"(scripts/install.sh at https://github.com/gastownhall/beads); " +
		"CGO_ENABLED=0 go install builds are server-mode only and cannot run embedded Dolt"
	doctorHintPing = "embedded Dolt did not answer: CGO_ENABLED=0 go install builds are server-mode only; " +
		"reinstall with brew install beads or the upstream install script"
	doctorHintGHMissing = "install the GitHub CLI (https://cli.github.com); " +
		"needed for safety-on gates and external PR review"
	doctorHintGHAuth   = "run: gh auth login; needed for safety-on gates and external PR review"
	doctorHintNoCart   = "add one with: sc cartridge --add claude --model <model>"
	doctorHintNoActive = "pick one with: sc cartridge --use NAME"
	doctorHintRepo     = "pass --repo PATH or run sc from inside a git repository"
	doctorHintDirty    = "commit or stash first; agents build on whatever is in the tree"
	doctorHintBeads    = "sc will run bd init --non-interactive --init-if-missing --skip-agents " +
		"--skip-hooks --setup-exclude on the first run (D-13)"
)

// DoctorStatus is the outcome of one sc doctor check.
type DoctorStatus string

// DoctorStatus values. Only DoctorFail makes sc doctor exit 1.
const (
	DoctorPass DoctorStatus = "pass"
	DoctorWarn DoctorStatus = "warn"
	DoctorFail DoctorStatus = "fail"
)

// DoctorCheck is one row of the sc doctor report.
type DoctorCheck struct {
	// Name identifies the check: "git", "bd", "beads", "gh", "config",
	// "cartridge", "cartridge:<name>", "repo", or ".beads".
	Name string `json:"name"`
	// Status is pass, warn, or fail.
	Status DoctorStatus `json:"status"`
	// Detail is a one-line description of what was found.
	Detail string `json:"detail"`
	// Hint says how to fix a warn or fail row; empty on pass.
	Hint string `json:"hint,omitempty"`
}

// DoctorReport is the --json shape of sc doctor and the value RunDoctor
// returns.
type DoctorReport struct {
	// Repo is the absolute --repo path the checks ran against.
	Repo string `json:"repo"`
	// Checks are the rows in display order.
	Checks []DoctorCheck `json:"checks"`
	// Passed, Warnings, and Failed count rows by status.
	Passed   int `json:"passed"`
	Warnings int `json:"warnings"`
	Failed   int `json:"failed"`
	// OK is true when no row failed.
	OK bool `json:"ok"`
}

func (r *DoctorReport) add(checks ...DoctorCheck) {
	for _, c := range checks {
		r.Checks = append(r.Checks, c)
		switch c.Status {
		case DoctorPass:
			r.Passed++
		case DoctorWarn:
			r.Warnings++
		case DoctorFail:
			r.Failed++
		}
	}
	r.OK = r.Failed == 0
}

func init() { register(newDoctorCommand) }

const doctorLong = `Check that everything a run needs is installed and configured:

  git        present and at least 2.30
  bd         present and at least 1.2.2 (the beads CLI with embedded Dolt)
  beads      bd ping answers when the repo already has a .beads/ directory
  gh         present and authenticated (warning only; needed for safety-on
             gates and external PR review)
  cartridge  every configured cartridge CLI is on PATH (failure when the
             active one is missing)
  repo       --repo is a git repository with a clean working tree (warning)
  .beads     present (warning: it is initialised on the first run)

Rows are pass, warn, or fail. The exit status is 1 when any row fails.`

func newDoctorCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check that git, bd, gh, and the cartridge CLIs are ready",
		Long:  doctorLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts, err := GlobalOpts(cmd)
			if err != nil {
				return err
			}
			report := RunDoctor(cmd.Context(), opts.Repo)
			if opts.JSON {
				err = writeJSON(cmd.OutOrStdout(), report)
			} else {
				err = writeDoctorReport(cmd.OutOrStdout(), report)
			}
			if err != nil {
				return err
			}
			if !report.OK {
				return &ExitError{Code: 1}
			}
			return nil
		},
	}
}

// RunDoctor performs every sc doctor check against repo (an absolute
// directory) and returns the report. It never returns an error: problems
// become warn or fail rows. Tools are looked up on PATH and run through
// internal/proc, so tests can substitute shims.
func RunDoctor(ctx context.Context, repo string) *DoctorReport {
	if ctx == nil {
		ctx = context.Background()
	}
	report := &DoctorReport{Repo: repo, OK: true}

	root, inRepo := config.FindRepoRoot(repo)
	if !inRepo {
		root = repo
	}
	beadsDir := filepath.Join(root, ".beads")
	beadsInfo, err := os.Stat(beadsDir)
	hasBeads := err == nil && beadsInfo.IsDir()

	gitCheck, gitOK := doctorGit(ctx)
	report.add(gitCheck)

	bdCheck, bdOK := doctorBD(ctx)
	report.add(bdCheck)
	if bdOK && hasBeads {
		report.add(doctorPing(ctx, root))
	}

	report.add(doctorGH(ctx))
	report.add(doctorCartridges(ctx, repo)...)
	report.add(doctorRepo(ctx, repo, root, inRepo, gitOK))
	report.add(doctorBeadsDir(beadsDir, hasBeads))
	return report
}

// doctorInherit are the parent variables every probe may see: enough for
// tools to find their own configuration, nothing else.
var doctorInherit = []string{"HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME"}

// doctorGit checks git is present and at least DoctorMinGit. ok reports
// whether git can be used for the repo check.
func doctorGit(ctx context.Context) (DoctorCheck, bool) {
	c := DoctorCheck{Name: "git"}
	tool, err := proc.Probe(ctx, proc.Spec{Command: "git", Args: []string{"--version"}, InheritEnv: doctorInherit, Timeout: doctorTimeout})
	switch {
	case proc.IsNotFound(err):
		c.Status, c.Detail, c.Hint = DoctorFail, "not found on PATH", doctorHintGit
		return c, false
	case err != nil:
		c.Status, c.Detail, c.Hint = DoctorFail, "git --version failed: "+doctorErr(err), doctorHintGit
		return c, false
	case !tool.HasVersion:
		c.Status, c.Detail, c.Hint = DoctorWarn, fmt.Sprintf("version unknown: %q (%s)", tool.Output, tool.Path), doctorHintGit
		return c, true
	case !tool.Version.AtLeast(DoctorMinGit):
		c.Status = DoctorFail
		c.Detail = fmt.Sprintf("%s is older than %s (%s)", tool.Version, DoctorMinGit, tool.Path)
		c.Hint = doctorHintGit
		return c, true
	}
	c.Status, c.Detail = DoctorPass, fmt.Sprintf("%s (%s)", tool.Version, tool.Path)
	return c, true
}

// bdSpec builds the Spec for one bd call with the D-21 environment.
func doctorBDSpec(args ...string) proc.Spec {
	return proc.Spec{
		Command:    "bd",
		Args:       args,
		Env:        map[string]string{"BD_JSON_ENVELOPE": "1", "BD_NON_INTERACTIVE": "1"},
		InheritEnv: append(append([]string(nil), doctorInherit...), "BEADS_DIR"),
		Timeout:    doctorTimeout,
	}
}

// doctorBD checks bd is present and `bd version --json` reports at least
// DoctorMinBD. ok reports whether bd can be used for the ping check.
func doctorBD(ctx context.Context) (DoctorCheck, bool) {
	c := DoctorCheck{Name: "bd"}
	path, err := proc.LookPath("bd")
	if err != nil {
		c.Status, c.Detail, c.Hint = DoctorFail, "not found on PATH", doctorHintBD
		return c, false
	}
	res, err := proc.Run(ctx, doctorBDSpec("version", "--json"))
	if err != nil {
		c.Status, c.Detail, c.Hint = DoctorFail, "bd version --json failed: "+doctorErr(err), doctorHintBD
		return c, false
	}
	var info struct {
		Version string `json:"version"`
		Build   string `json:"build"`
	}
	if err := doctorDecodeBD(res.Stdout, &info); err != nil {
		c.Status, c.Detail, c.Hint = DoctorFail, "bd version --json: "+err.Error(), doctorHintBD
		return c, false
	}
	v, ok := proc.ParseVersion(info.Version)
	if !ok {
		c.Status, c.Detail, c.Hint = DoctorFail, fmt.Sprintf("bd version --json reported %q (%s)", info.Version, path), doctorHintBD
		return c, false
	}
	detail := v.String()
	if info.Build != "" {
		detail += " (build " + info.Build + ", " + path + ")"
	} else {
		detail += " (" + path + ")"
	}
	if !v.AtLeast(DoctorMinBD) {
		c.Status, c.Detail, c.Hint = DoctorFail, fmt.Sprintf("%s is older than %s (%s)", v, DoctorMinBD, path), doctorHintBD
		return c, false
	}
	c.Status, c.Detail = DoctorPass, detail
	return c, true
}

// doctorPing confirms the embedded database answers `bd ping --json` in
// root, which already has a .beads/ directory.
func doctorPing(ctx context.Context, root string) DoctorCheck {
	c := DoctorCheck{Name: "beads"}
	res, err := proc.Run(ctx, doctorBDSpec("-C", root, "ping", "--json"))
	if err != nil {
		c.Status, c.Detail, c.Hint = DoctorFail, "bd ping failed: "+doctorErr(err), doctorHintPing
		return c
	}
	var ping struct {
		Status  string `json:"status"`
		TotalMS int64  `json:"total_ms"`
	}
	if err := doctorDecodeBD(res.Stdout, &ping); err != nil {
		c.Status, c.Detail, c.Hint = DoctorFail, "bd ping --json: "+err.Error(), doctorHintPing
		return c
	}
	if ping.Status != "ok" {
		c.Status, c.Detail, c.Hint = DoctorFail, fmt.Sprintf("bd ping reported status %q", ping.Status), doctorHintPing
		return c
	}
	c.Status, c.Detail = DoctorPass, fmt.Sprintf("embedded database answered bd ping in %d ms", ping.TotalMS)
	return c
}

var doctorGHLoggedIn = regexp.MustCompile(`Logged in to (\S+) account (\S+)`)

// doctorGH checks gh is present and authenticated. Both problems are
// warnings: gh is only needed for safety-on gates and external PR review.
func doctorGH(ctx context.Context) DoctorCheck {
	c := DoctorCheck{Name: "gh"}
	tool, err := proc.Probe(ctx, proc.Spec{Command: "gh", Args: []string{"--version"}, InheritEnv: doctorInherit, Timeout: doctorTimeout})
	if proc.IsNotFound(err) {
		c.Status, c.Detail, c.Hint = DoctorWarn, "not found on PATH", doctorHintGHMissing
		return c
	}
	if err != nil {
		c.Status, c.Detail, c.Hint = DoctorWarn, "gh --version failed: "+doctorErr(err), doctorHintGHMissing
		return c
	}
	version := tool.Path
	if tool.HasVersion {
		version = tool.Version.String() + " (" + tool.Path + ")"
	}
	res, err := proc.Run(ctx, proc.Spec{
		Command:    "gh",
		Args:       []string{"auth", "status"},
		InheritEnv: append(append([]string(nil), doctorInherit...), "GH_TOKEN", "GITHUB_TOKEN", "GH_HOST", "GH_CONFIG_DIR", "GH_ENTERPRISE_TOKEN"),
		Env:        map[string]string{"GH_PROMPT_DISABLED": "1", "NO_COLOR": "1"},
		Timeout:    doctorTimeout,
	})
	if err != nil {
		c.Status, c.Detail, c.Hint = DoctorWarn, version+", not authenticated: "+doctorFirstLine(res.Stdout, res.Stderr, err), doctorHintGHAuth
		return c
	}
	c.Status, c.Detail = DoctorPass, version+", authenticated"
	if m := doctorGHLoggedIn.FindStringSubmatch(res.Stdout + "\n" + res.Stderr); m != nil {
		c.Detail = fmt.Sprintf("%s, authenticated as %s on %s", version, m[2], m[1])
	}
	return c
}

// doctorCartridges loads the effective config for repo and checks that
// every configured cartridge CLI is on PATH. A missing active cartridge
// fails; a missing inactive one warns; no cartridge at all warns.
func doctorCartridges(ctx context.Context, repo string) []DoctorCheck {
	eff, err := config.Load(repo)
	if err != nil {
		return []DoctorCheck{{Name: "config", Status: DoctorFail, Detail: err.Error(), Hint: "fix the file, or see sc config show"}}
	}
	if len(eff.Cartridges) == 0 {
		return []DoctorCheck{{Name: "cartridge", Status: DoctorWarn, Detail: "no cartridge configured", Hint: doctorHintNoCart}}
	}
	var checks []DoctorCheck
	if eff.ActiveCartridge == "" {
		checks = append(checks, DoctorCheck{Name: "cartridge", Status: DoctorWarn, Detail: "no active cartridge", Hint: doctorHintNoActive})
	}
	names := make([]string, 0, len(eff.Cartridges))
	for n := range eff.Cartridges {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		checks = append(checks, doctorCartridge(ctx, name, eff.Cartridges[name], name == eff.ActiveCartridge))
	}
	return checks
}

// doctorCartridge checks one cartridge's CLI is on PATH and, best effort,
// reports its version.
func doctorCartridge(ctx context.Context, name string, cart config.Cartridge, active bool) DoctorCheck {
	c := DoctorCheck{Name: "cartridge:" + name}
	bin := cart.Binary(name)
	label := "inactive"
	if active {
		label = "active"
	}
	path, err := proc.LookPath(bin)
	if err != nil {
		c.Status = DoctorWarn
		if active {
			c.Status = DoctorFail
		}
		c.Detail = fmt.Sprintf("%s: %q not found on PATH", label, bin)
		c.Hint = fmt.Sprintf("install the %s CLI, or set [cartridges.%s] command in the global config", name, name)
		return c
	}
	c.Status = DoctorPass
	c.Detail = fmt.Sprintf("%s: %s", label, path)
	tool, err := proc.Probe(ctx, proc.Spec{Command: bin, Args: []string{"--version"}, InheritEnv: doctorInherit, Timeout: doctorTimeout})
	if err == nil && tool.HasVersion {
		c.Detail = fmt.Sprintf("%s: %s (%s)", label, tool.Version, path)
	}
	return c
}

// doctorRepo checks that repo is inside a git repository with a clean
// working tree. Both problems are warnings so doctor stays useful for
// checking tools from any directory.
func doctorRepo(ctx context.Context, repo, root string, inRepo, gitOK bool) DoctorCheck {
	c := DoctorCheck{Name: "repo"}
	if !inRepo {
		c.Status, c.Detail, c.Hint = DoctorWarn, "not a git repository: "+repo, doctorHintRepo
		return c
	}
	if !gitOK {
		c.Status, c.Detail = DoctorWarn, root+" (clean-tree check skipped: git is not usable)"
		return c
	}
	res, err := proc.Run(ctx, proc.Spec{
		Command:    "git",
		Args:       []string{"-C", root, "status", "--porcelain"},
		InheritEnv: doctorInherit,
		Env:        map[string]string{"GIT_TERMINAL_PROMPT": "0", "GIT_OPTIONAL_LOCKS": "0"},
		Timeout:    doctorTimeout,
	})
	if err != nil {
		c.Status, c.Detail = DoctorWarn, "git status failed in "+root+": "+doctorErr(err)
		return c
	}
	changes := 0
	for _, line := range strings.Split(res.Stdout, "\n") {
		if strings.TrimSpace(line) != "" {
			changes++
		}
	}
	if changes > 0 {
		c.Status, c.Hint = DoctorWarn, doctorHintDirty
		c.Detail = fmt.Sprintf("%d uncommitted change%s in %s", changes, doctorPlural(changes), root)
		return c
	}
	c.Status, c.Detail = DoctorPass, "clean working tree at "+root
	return c
}

// doctorBeadsDir reports whether the repo already has a .beads/ directory.
func doctorBeadsDir(beadsDir string, exists bool) DoctorCheck {
	c := DoctorCheck{Name: ".beads"}
	if exists {
		c.Status, c.Detail = DoctorPass, beadsDir+" present"
		return c
	}
	c.Status, c.Detail, c.Hint = DoctorWarn, "not initialised: "+beadsDir+" is missing", doctorHintBeads
	return c
}

// decodeBDJSON decodes the JSON object bd printed on stdout into v. It
// skips any non-JSON preamble (bd prints a one-time metrics notice and some
// commands print a text line first) and unwraps the BD_JSON_ENVELOPE=1
// {"schema_version":1,"data":...} shape when present.
func doctorDecodeBD(stdout string, v any) error {
	rest := stdout
	for {
		i := strings.IndexByte(rest, '{')
		if i < 0 {
			return fmt.Errorf("no JSON object in output %q", strings.TrimSpace(stdout))
		}
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(strings.NewReader(rest[i:])).Decode(&raw); err != nil {
			rest = rest[i+1:]
			continue
		}
		if data, ok := raw["data"]; ok {
			if _, versioned := raw["schema_version"]; versioned {
				return json.Unmarshal(data, v)
			}
		}
		return json.NewDecoder(strings.NewReader(rest[i:])).Decode(v)
	}
}

// doctorErr renders a probe failure on one line: the last stderr line when
// the process ran, else the error itself.
func doctorErr(err error) string {
	var exit *proc.ExitError
	if errors.As(err, &exit) {
		switch {
		case errors.Is(err, proc.ErrTimeout):
			return "timed out"
		case exit.Result.ExitCode >= 0:
			if tail := doctorFirstLine(exit.Result.Stderr, exit.Result.Stdout, nil); tail != "" {
				return fmt.Sprintf("exit status %d: %s", exit.Result.ExitCode, tail)
			}
			return fmt.Sprintf("exit status %d", exit.Result.ExitCode)
		}
	}
	return err.Error()
}

// doctorFirstLine returns the first non-empty line of a, else of b, else
// the error text, else "".
func doctorFirstLine(a, b string, err error) string {
	for _, s := range []string{a, b} {
		for _, line := range strings.Split(s, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				return line
			}
		}
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

func doctorPlural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// doctorLabels are the status column texts; FAIL is upper-case so it stands
// out without colour.
var doctorLabels = map[DoctorStatus]string{DoctorPass: "pass", DoctorWarn: "warn", DoctorFail: "FAIL"}

// doctorNameWidth is the minimum width of the name column, so rows line up
// the same way whether or not a "cartridge:<name>" row is present.
const doctorNameWidth = 16

// writeDoctorReport prints the report as aligned text rows followed by a
// summary line.
func writeDoctorReport(w io.Writer, r *DoctorReport) error {
	width := doctorNameWidth
	for _, c := range r.Checks {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}
	for _, c := range r.Checks {
		if _, err := fmt.Fprintf(w, "%-4s  %-*s  %s\n", doctorLabels[c.Status], width, c.Name, c.Detail); err != nil {
			return err
		}
		if c.Hint != "" {
			if _, err := fmt.Fprintf(w, "      %-*s  hint: %s\n", width, "", c.Hint); err != nil {
				return err
			}
		}
	}
	_, err := fmt.Fprintf(w, "\n%d passed, %d warning%s, %d failed\n", r.Passed, r.Warnings, doctorPlural(r.Warnings), r.Failed)
	return err
}
