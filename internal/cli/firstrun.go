package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/gitx"
	"slop-cannon/internal/logging"
	"slop-cannon/internal/orchestrator"
)

// This file is the first-run experience (P8-06, P8-09).
//
// Two moments are covered. When a run cannot start, [StartupFailure] prints
// what went wrong and a numbered list of what to do about it, built from the
// rows of an [sc doctor] report rather than from a second, drifting copy of
// the same checks (P8-09) — so "install bd", "add a cartridge" and
// "authenticate gh" are worded once, in [RunDoctor], and appear here
// verbatim. When a run finishes and it was the first one sc ever fired at
// the repository, [ReportFirstRun] says where the logs, the prompts, the
// worktrees and the configuration live, because none of it is inside the
// repository (D-23) and nothing else in the output points at it.
//
// Both halves render to a writer and never touch the process: the caller
// chooses stderr (so `--json`'s stdout stays a stream of envelopes) and the
// exit code.

// startupDoctorTimeout bounds the doctor probes the advice runs. The user
// is already waiting on a failure, and one hung probe (gh talks to the
// network) must not turn a refusal into a hang.
const startupDoctorTimeout = 15 * time.Second

// firstRunDoctor is [RunDoctor], as a variable so a test can hand
// [AdviseStartup] a fixed report instead of probing the machine.
var firstRunDoctor = RunDoctor

// StartupStep is one numbered instruction in the fix list a failed startup
// prints.
type StartupStep struct {
	// Check names the sc doctor row this step came from ("bd", "gh",
	// "cartridge:claude"), and is empty for a step derived from the error
	// itself.
	Check string `json:"check,omitempty"`
	// Problem is what is wrong, in the words of whatever found it.
	Problem string `json:"problem"`
	// Fix is what to do about it; empty when there is nothing to add
	// beyond the problem.
	Fix string `json:"fix,omitempty"`
}

// StartupAdvice is everything sc has to say about a run that could not
// start: the reason, and the ordered list of things to fix.
type StartupAdvice struct {
	// Repo is the repository the run was aimed at.
	Repo string `json:"repo"`
	// Reason is the failure itself, on one line.
	Reason string `json:"reason"`
	// Steps are the fixes, most relevant first. It is empty when neither
	// the error nor sc doctor could name anything to do.
	Steps []StartupStep `json:"steps,omitempty"`
}

// StartupFailure is the whole first-run failure experience in one call: it
// prints why the run could not start, a numbered fix list, and the
// suggestion to re-check with sc doctor, then returns the error the command
// should return.
//
// That error is an *[ExitError] with code 1 and no message of its own,
// because the advice already said everything the "sc: …" line would have.
// A caller that wants the original error to survive to its own callers
// should use [AdviseStartup] and [WriteStartupAdvice] directly.
func StartupFailure(ctx context.Context, w io.Writer, repo string, err error) error {
	if err == nil {
		return nil
	}
	if werr := WriteStartupAdvice(w, AdviseStartup(ctx, repo, err)); werr != nil {
		// The advice could not be printed, so the error must speak for
		// itself after all.
		return err
	}
	return &ExitError{Code: 1}
}

// AdviseStartup works out what to tell a user whose run would not start. It
// runs the sc doctor checks against repo (P8-09) and keeps the rows that
// matter: every failing row, plus the warnings that belong to the kind of
// failure err is — a missing cartridge is a warning to doctor and the whole
// story here, while a missing .beads directory is a warning doctor prints
// and this list leaves out, because sc creates it on the first run (D-13).
//
// It never returns an error: a machine so broken that the checks cannot run
// still gets the reason and whatever the error itself suggests.
func AdviseStartup(ctx context.Context, repo string, err error) StartupAdvice {
	repo = startupAbs(repo)
	a := StartupAdvice{Repo: repo, Reason: startupReason(err)}
	if err == nil {
		return a
	}
	class := startupClassOf(err)

	var report *DoctorReport
	if !class.skipDoctor {
		if ctx == nil {
			ctx = context.Background()
		}
		dctx, cancel := context.WithTimeout(ctx, startupDoctorTimeout)
		defer cancel()
		report = firstRunDoctor(dctx, repo)
	}

	// The rows about this failure lead, whatever order doctor reports in;
	// the other failing rows follow, because they will stop the next run
	// even if they did not stop this one.
	var mine, rest []StartupStep
	if report != nil {
		for _, c := range report.Checks {
			if !startupWants(class, c) {
				continue
			}
			step := StartupStep{Check: c.Name, Problem: c.Detail, Fix: c.Hint}
			if class.covers(c.Name) {
				mine = append(mine, step)
				continue
			}
			rest = append(rest, step)
		}
	}
	// The error's own step is only worth printing when no doctor row
	// already says the same thing, so "no cartridge configured" is never
	// listed twice under two spellings.
	if len(mine) == 0 {
		if step, ok := class.step(err); ok {
			mine = append(mine, step)
		}
	}
	a.Steps = append(mine, rest...)
	return a
}

// WriteStartupAdvice prints the advice: the reason, the numbered fixes, and
// the sc doctor suggestion.
func WriteStartupAdvice(w io.Writer, a StartupAdvice) error {
	where := a.Repo
	if where == "" {
		where = "this repository"
	}
	if _, err := fmt.Fprintf(w, "sc could not start a run in %s: %s\n", where, a.Reason); err != nil {
		return err
	}
	if len(a.Steps) > 0 {
		if _, err := fmt.Fprint(w, "\nFix it in this order:\n"); err != nil {
			return err
		}
		for i, s := range a.Steps {
			lead := fmt.Sprintf("  %d. ", i+1)
			if _, err := fmt.Fprintf(w, "%s%s\n", lead, s.title()); err != nil {
				return err
			}
			if s.Fix == "" {
				continue
			}
			if _, err := fmt.Fprintf(w, "%s%s\n", strings.Repeat(" ", len(lead)), s.Fix); err != nil {
				return err
			}
		}
	}
	_, err := fmt.Fprint(w, "\nThen check everything at once with: sc doctor\n")
	return err
}

// title renders the problem line of a step: "<check>: <problem>", or the
// problem alone for a step that came from the error rather than a check.
func (s StartupStep) title() string {
	switch {
	case s.Check == "":
		return s.Problem
	case s.Problem == "":
		return s.Check
	}
	return s.Check + ": " + s.Problem
}

// startupAbs makes the repository path absolute so the advice names the
// same directory the checks ran against, whatever the user typed. A path
// that cannot be resolved is left as it is: an odd path in the message
// beats no message.
func startupAbs(repo string) string {
	if repo == "" {
		return ""
	}
	abs, err := filepath.Abs(repo)
	if err != nil {
		return repo
	}
	return abs
}

// startupReason renders err on one line: the message with any newlines
// folded, so a multi-line error cannot break the layout of the advice.
func startupReason(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.Join(strings.Fields(strings.ReplaceAll(err.Error(), "\n", " ")), " ")
	if msg == "" {
		return "the run failed to start"
	}
	return msg
}

// ---------------------------------------------------------------------------
// Classifying a startup failure
// ---------------------------------------------------------------------------

// startupClass is one recognised kind of startup failure: how to spot it,
// which sc doctor rows are about it, and what to say when doctor found
// nothing to say itself.
type startupClass struct {
	// name identifies the class in tests and in [StartupStep.Check] when
	// the step comes from the error.
	name string
	// match reports whether err belongs to this class.
	match func(error) bool
	// checks are the sc doctor row names this class is about; a warning
	// row is only shown when its name is one of these, and a failing row
	// with one of these names means the error is already covered. A name
	// ending in ":" matches every row with that prefix.
	checks []string
	// problem and fix are the step to add when no doctor row covered the
	// failure. An empty problem means "use the error itself".
	problem string
	fix     string
	// fixFor overrides fix when the error carries its own remedy.
	fixFor func(error) string
	// skipDoctor leaves the sc doctor probes out of the advice for a
	// failure doctor has no row for and no other row can explain — the
	// terminal. The probes cost seconds (gh's row talks to the network)
	// and a user who piped sc into a file is not helped by the state of
	// their git installation.
	skipDoctor bool
}

// covers reports whether a doctor row named name belongs to this class.
func (c startupClass) covers(name string) bool {
	for _, want := range c.checks {
		if strings.HasSuffix(want, ":") {
			if strings.HasPrefix(name, want) {
				return true
			}
			continue
		}
		if name == want {
			return true
		}
	}
	return false
}

// step returns the class's own instruction for err, when it has one.
func (c startupClass) step(err error) (StartupStep, bool) {
	fix := c.fix
	if c.fixFor != nil {
		if s := c.fixFor(err); s != "" {
			fix = s
		}
	}
	problem := c.problem
	if problem == "" && fix == "" {
		return StartupStep{}, false
	}
	if problem == startupReason(err) {
		// The reason is already printed above the list; a step that
		// repeats it word for word says nothing twice. The check name
		// alone leads the step instead.
		problem = ""
	}
	return StartupStep{Check: c.name, Problem: problem, Fix: fix}, true
}

// startupWants reports whether a doctor row belongs in the fix list for a
// failure of this class: every failing row does, and a warning does when
// the class is about it.
func startupWants(c startupClass, row DoctorCheck) bool {
	switch row.Status {
	case DoctorFail:
		return true
	case DoctorWarn:
		return c.covers(row.Name)
	}
	return false
}

// The install and authentication hints that belong to no sc doctor row,
// because doctor cannot see the failure: it probes tools, not sessions.
const (
	startupFixLock  = "`sc status` names the run that holds this repository: wait for it, or stop it, and only pass --force once you are sure it is gone"
	startupFixAuth  = "sign the cartridge CLI in yourself (run it once in a terminal and log in), then fire again"
	startupFixSpent = "the cartridge's subscription window is used up: wait for it to reset, " +
		"or fire another one with --cartridge NAME (sc cartridge --list)"
	startupFixDirty = "commit or stash the changes first: agents build on whatever is in the tree, " +
		"and sc will not carry your work into a worktree"
	startupFixConfig = "fix the file at the line above, then read the merged result back with: sc config show"
	startupFixAmmo   = "raise it with: sc ammo N (N at least 3), or pass --ammo N for this run only"
	startupFixGH     = "run: gh auth login — or fire with --safety off to merge locally instead of opening pull requests (D-10)"
	// startupFixRepo is the sc doctor repo row's own hint, not a second
	// spelling of it (P8-09): the fallback step and the row a user reads
	// in sc doctor must never drift apart.
	startupFixRepo   = doctorHintRepo
	startupFixNoRepo = "no bead tracker could be prepared: check that the path is a git repository you can write to"
)

// startupClasses are the failures sc knows how to advise on, in the order
// they are tried. The first match wins, so the specific classes come before
// the general ones.
var startupClasses = []startupClass{
	{
		name:    "lock",
		match:   func(err error) bool { return errors.Is(err, orchestrator.ErrLocked) },
		problem: "another sc run is already firing at this repository",
		fix:     startupFixLock,
		checks:  nil,
	},
	{
		name: "repo",
		match: func(err error) bool {
			return errors.Is(err, gitx.ErrNotRepository) || errors.Is(err, beads.ErrNotGitRepo) ||
				strings.Contains(err.Error(), "is not a git repository")
		},
		checks:  []string{"repo"},
		problem: "sc needs a git repository to fire at",
		fix:     startupFixRepo,
	},
	{
		name: "cartridge",
		match: func(err error) bool {
			return errors.Is(err, config.ErrNoCartridge) || errors.Is(err, cartridge.ErrNoActiveCartridge) ||
				errors.Is(err, cartridge.ErrUnknownCartridge) || errors.Is(err, cartridge.ErrNotInstalled) ||
				errors.Is(err, cartridge.ErrCartridgeNotBuilt)
		},
		checks:  []string{"cartridge", "cartridge:", "config"},
		problem: "no cartridge is loaded",
		fix:     "add one with: " + CartridgeAddHint,
	},
	{
		name:    "cartridge",
		match:   func(err error) bool { return errors.Is(err, cartridge.ErrAuth) },
		checks:  []string{"cartridge:"},
		problem: "the cartridge's CLI is not signed in",
		fix:     startupFixAuth,
	},
	{
		name: "cartridge",
		match: func(err error) bool {
			return errors.Is(err, cartridge.ErrCreditsExhausted) || errors.Is(err, cartridge.ErrRateLimited)
		},
		checks:  []string{"cartridge:"},
		problem: "the cartridge has nothing left to spend",
		fix:     startupFixSpent,
	},
	{
		name: "bd",
		match: func(err error) bool {
			var ce *beads.CheckError
			return errors.As(err, &ce) || errors.Is(err, beads.ErrNoProject) ||
				errors.Is(err, beads.ErrReadOnly) || errors.Is(err, beads.ErrLocked)
		},
		checks:  []string{"bd", "beads"},
		problem: "the bead tracker could not be prepared",
		fix:     startupFixNoRepo,
		fixFor: func(err error) string {
			var ce *beads.CheckError
			if errors.As(err, &ce) {
				return ce.Hint()
			}
			if errors.Is(err, beads.ErrLocked) {
				return "another program is holding the beads database: close it (or wait for it), then try again"
			}
			return ""
		},
	},
	{
		name: "gh",
		match: func(err error) bool {
			return errors.Is(err, gitx.ErrGHNotFound) || errors.Is(err, gitx.ErrGHUnauthenticated) ||
				errors.Is(err, gitx.ErrNoGitHubRemote)
		},
		checks:  []string{"gh"},
		problem: "the GitHub CLI cannot be used here",
		fix:     startupFixGH,
	},
	{
		name:    "repo",
		match:   func(err error) bool { return errors.Is(err, gitx.ErrRepoDirty) },
		checks:  []string{"repo"},
		problem: "the working tree has uncommitted changes",
		fix:     startupFixDirty,
	},
	{
		name: "config",
		match: func(err error) bool {
			var pe *config.ParseError
			var ve *config.ValidationError
			return errors.As(err, &pe) || errors.As(err, &ve)
		},
		checks:  []string{"config"},
		problem: "the configuration could not be loaded",
		fix:     startupFixConfig,
	},
	{
		name:    "ammo",
		match:   func(err error) bool { return errors.Is(err, config.ErrAmmoTooLow) },
		problem: "there is not enough ammo to field a crew",
		fix:     startupFixAmmo,
		checks:  nil,
	},
	{
		name:    "terminal",
		match:   func(err error) bool { return strings.Contains(err.Error(), "needs a terminal") },
		problem: "sc was not started from a terminal",
		fix: "fire again with the run as log lines: sc <repo> --" + FlagRunHeadless +
			" (add --json for one JSON envelope per event)",
		checks:     nil,
		skipDoctor: true,
	},
}

// startupOther is the class of a failure sc does not recognise: the doctor
// rows that fail are still worth printing, and the error speaks for itself.
var startupOther = startupClass{name: ""}

// startupClassOf returns the class err belongs to, or [startupOther].
func startupClassOf(err error) startupClass {
	for _, c := range startupClasses {
		if c.match != nil && c.match(err) {
			return c
		}
	}
	return startupOther
}

// ---------------------------------------------------------------------------
// The first successful run in a repository (P8-06)
// ---------------------------------------------------------------------------

// FirstRunNotice is where sc put everything it wrote for one run, printed
// after the first run it ever fires at a repository. Every path is outside
// the repository (D-23) except the repo config, which exists only when a
// config command created it.
type FirstRunNotice struct {
	// Repo is the repository that was fired at.
	Repo string `json:"repo"`
	// Run is the run whose directory the paths belong to.
	Run domain.RunID `json:"run"`
	// Dir is the run directory, <state-dir>/runs/<run-id>.
	Dir string `json:"dir"`
	// Log is the orchestrator log, Agents the per-agent log directory,
	// Prompts the copy of every prompt sent, Events the event log and
	// Worktrees the directory holding one git worktree per bead.
	Log       string `json:"log"`
	Agents    string `json:"agents"`
	Prompts   string `json:"prompts"`
	Events    string `json:"events"`
	Worktrees string `json:"worktrees"`
	// Config is the global config file; RepoConfig is the repository's own
	// file when it exists, and empty when it does not.
	Config     string `json:"config"`
	RepoConfig string `json:"repo_config,omitempty"`
}

// NewFirstRunNotice describes where the run wrote everything. It takes the
// run by value: the orchestrator mutates the [config.RunConfig] a caller
// still holds once the run is going, so a report printed at the end reads
// the copy the caller took before it started.
func NewFirstRunNotice(run domain.Run) FirstRunNotice {
	dir := run.StateDir
	if dir == "" {
		dir = config.RunDir(run.RepoPath, run.ID.String())
	}
	dirs := orchestrator.DirsFor(dir)
	paths := logging.PathsFor(dir)
	n := FirstRunNotice{
		Repo:      run.RepoPath,
		Run:       run.ID,
		Dir:       dirs.Dir,
		Log:       paths.Log,
		Agents:    paths.AgentsDir,
		Prompts:   prompt.Dir(dirs.Dir),
		Events:    orchestrator.EventLogPath(dirs.Dir),
		Worktrees: dirs.Worktrees,
		Config:    config.GlobalPath(),
	}
	if repoCfg := config.RepoConfigPath(run.RepoPath); frFileExists(repoCfg) {
		n.RepoConfig = repoCfg
	}
	return n
}

// firstRunNoticeName is the file that records that the first-run notice
// has been printed for a repository. It sits beside the run directories in
// the D-23 state directory — never inside the repository — and holds the id
// of the run that earned it.
//
// Counting run directories cannot answer the question on its own: `sc
// --resume` reuses the latest run directory and its id, so a resumed first
// run still sees exactly one directory named after itself, and without this
// marker the same run would print the notice a second time.
const firstRunNoticeName = "first-run-notice"

// firstRunNoticePath is where that marker lives for repoPath.
func firstRunNoticePath(repoPath string) string {
	return filepath.Join(config.StateDir(repoPath), firstRunNoticeName)
}

// FirstRunInRepo reports whether the first-run notice is still owed for id:
// sc has recorded exactly one run for the repository, that run is id, and
// the notice has never been printed there — so resuming that same run says
// nothing a second time. A run directory that cannot be read answers false:
// the notice is a courtesy, and printing it twice is worse than not
// printing it at all.
func FirstRunInRepo(repoPath string, id domain.RunID) bool {
	if _, err := os.Stat(firstRunNoticePath(repoPath)); err == nil {
		return false
	}
	entries, err := os.ReadDir(filepath.Join(config.StateDir(repoPath), config.RunsDirName))
	if err != nil {
		return false
	}
	seen := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Name() != logging.SafeFileName(id.String()) {
			return false
		}
		seen++
	}
	return seen == 1
}

// ReportFirstRun prints [FirstRunNotice] when run is the first one sc has
// fired at its repository and the notice has not been printed there before,
// and does nothing otherwise — including when that same first run is
// resumed. Call it after a run finishes; the caller decides what
// "successful" means, since a run that ended badly still wrote logs worth
// finding.
func ReportFirstRun(w io.Writer, run domain.Run) error {
	if !FirstRunInRepo(run.RepoPath, run.ID) {
		return nil
	}
	if !frClaimNotice(run.RepoPath, run.ID) {
		return nil
	}
	return WriteFirstRunNotice(w, NewFirstRunNotice(run))
}

// frClaimNotice records that the notice has been printed for repoPath and
// reports whether this caller is the one that may print it. The marker is
// created exclusively, so of two processes racing at one repository only
// one prints; a marker that cannot be created at all (an unwritable state
// directory) still prints, because the notice matters more than the
// bookkeeping around it, and the run that got that far wrote its logs
// somewhere else anyway.
func frClaimNotice(repoPath string, id domain.RunID) bool {
	path := firstRunNoticePath(repoPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return true
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return !errors.Is(err, os.ErrExist)
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintln(f, id.String())
	return true
}

// WriteFirstRunNotice prints the notice as an aligned list of locations.
func WriteFirstRunNotice(w io.Writer, n FirstRunNotice) error {
	rows := [][2]string{
		{"run log", n.Log},
		{"agent logs", n.Agents},
		{"prompts", n.Prompts},
		{"events", n.Events},
		{"worktrees", n.Worktrees},
		{"config", n.Config},
	}
	if n.RepoConfig != "" {
		rows = append(rows, [2]string{"repo config", n.RepoConfig})
	}
	header := fmt.Sprintf("That was the first sc run in %s.\n"+
		"Nothing the run wrote lives inside the repository; this is where it all went:\n\n", n.Repo)
	if _, err := fmt.Fprint(w, header); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := fmt.Fprintf(w, "  %-11s %s\n", r[0], r[1]); err != nil {
			return err
		}
	}
	if n.RepoConfig != "" {
		// The one path above that is inside the repository, and the run
		// only read it: an sc config command wrote it (D-23).
		if _, err := fmt.Fprint(w, "\nThe repo config is the only sc file inside the repository; "+
			"an sc config command wrote it, and the run only read it.\n"); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "\nRead this run back at any time with: sc status --repo %s\n", n.Repo)
	return err
}

// frFileExists reports whether path is a readable regular file.
func frFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
