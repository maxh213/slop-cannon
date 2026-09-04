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
	"slop-cannon/internal/events"
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
// verbatim. When a run completes and it is the first one sc ever finished
// successfully in that repository, [ReportFirstRun] says where the logs,
// the prompts, the worktrees and the configuration live, because none of it
// is inside the repository (D-23) and nothing else in the output points at
// it. A stopped or failed run leaves that notice unspent for the run that
// gets there.
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
// matter: every failing row, the warnings that belong to the kind of
// failure err is, and the warnings that would refuse the next run whatever
// this one died of ([startupBlockingWarn]) — a missing cartridge is a
// warning to doctor and a blocker to every run (D-04), while a missing
// .beads directory is a warning doctor prints and this list leaves out,
// because sc creates it on the first run (D-13).
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
	// quiet reports whether this error's own message already says
	// everything the class would add, so the class contributes no step at
	// all. A lock refusal is the case that needs it: its message names the
	// live run, points at `sc status`, says to wait for it or stop it, and
	// — when the lock was written on another machine — how to get past it
	// with --force, which leaves the step with nothing of its own to say.
	quiet func(error) bool
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
//
// It has none when the error's message already carries the whole
// instruction ([startupClass.quiet]): the reason is printed above the fix
// list, and a numbered step that says the same thing again is noise
// dressed as help.
func (c startupClass) step(err error) (StartupStep, bool) {
	if c.quiet != nil && c.quiet(err) {
		return StartupStep{}, false
	}
	fix := c.fix
	if c.fixFor != nil {
		if s := c.fixFor(err); s != "" {
			fix = s
		}
	}
	reason := startupReason(err)
	if startupSaidAlready(reason, fix) {
		// The class's step exists to add the remedy, and some errors are
		// written with their own remedy in them ("no cartridge loaded —
		// add one with: sc cartridge --add …"). Printing it again under a
		// heading is the same sentence twice, so there is no step: the
		// reason above the list already said all of it.
		return StartupStep{}, false
	}
	problem := c.problem
	if startupSaidAlready(reason, problem) {
		// The reason is printed above the list; a step that repeats it
		// says nothing twice. The check name alone leads the step instead.
		problem = ""
	}
	if problem == "" && fix == "" {
		return StartupStep{}, false
	}
	return StartupStep{Check: c.name, Problem: problem, Fix: fix}, true
}

// startupSaidAlready reports whether reason already contains fix word for
// word, ignoring case and the run of spaces the folding left: the advice
// then has nothing to add by repeating it.
func startupSaidAlready(reason, fix string) bool {
	fix = strings.TrimSpace(fix)
	if fix == "" {
		return false
	}
	return strings.Contains(strings.ToLower(reason), strings.ToLower(fix))
}

// startupLockSaidItAll reports whether a lock refusal already told the user
// about --force. [orchestrator.LockedError] does when the lock was written
// on another machine ("start again with --force once you are sure it
// stopped"), and that message also names the run and where to look, so
// there is nothing left for a numbered step to add.
func startupLockSaidItAll(err error) bool {
	return err != nil && strings.Contains(err.Error(), "--force")
}

// startupWants reports whether a doctor row belongs in the fix list for a
// failure of this class: every failing row does, a warning does when the
// class is about it, and so does a warning that will refuse the next run
// whatever this one died of ([startupBlockingWarn]).
func startupWants(c startupClass, row DoctorCheck) bool {
	switch row.Status {
	case DoctorFail:
		return true
	case DoctorWarn:
		return c.covers(row.Name) || startupBlockingWarn(row)
	}
	return false
}

// startupCartridgeRow is the sc doctor row name that carries "no cartridge
// configured" and "no active cartridge" (doctor.go). The per-cartridge rows
// are named "cartridge:<name>" and are not this.
const startupCartridgeRow = "cartridge"

// startupBlockingWarn reports whether a doctor warning is in truth a
// blocker for any run at all. sc doctor warns rather than fails when no
// cartridge is configured or none is active, because doctor is useful on a
// machine that only reads reports — but a run cannot fire a single agent
// without one (D-04), so a fix list that leaves it out sends a brand-new
// user round the loop again: they do everything the numbered steps say and
// are refused on the next shot for a reason nobody mentioned.
func startupBlockingWarn(row DoctorCheck) bool {
	return row.Name == startupCartridgeRow
}

// The install and authentication hints that belong to no sc doctor row,
// because doctor cannot see the failure: it probes tools, not sessions.
const (
	// startupFixLock adds the one thing an orchestrator.LockedError does
	// not already say: what --force is for. Its message names the run,
	// points at `sc status` and says to wait for it or stop it, so a step
	// that repeats any of that is noise (startupClass.quiet drops the step
	// altogether for the message that mentions --force itself).
	startupFixLock  = "--force takes the lock, and is only safe once you are certain that run is gone: two cannons in one repository fight over the same beads, worktrees and branches"
	startupFixAuth  = "sign the cartridge CLI in yourself (run it once in a terminal and log in), then fire again"
	startupFixSpent = "the cartridge's subscription window is used up: wait for it to reset, " +
		"or fire another one with --cartridge NAME (sc cartridge --list)"
	startupFixDirty = "commit or stash the changes first: agents build on whatever is in the tree, " +
		"and sc will not carry your work into a worktree"
	startupFixConfig = "fix the file at the line above, then read the merged result back with: sc config show"
	startupFixAmmo   = "raise it with: sc ammo N (N at least 3), or pass --ammo N for this run only"
	// startupFixExclude is for the one refusal that comes out of a config
	// value rather than out of the config file's syntax, so the parser's
	// "the line above" advice would point at nothing (P9-05).
	startupFixExclude = "fix or drop the pattern the message names: `exclude` takes path.Match globs, " +
		"such as \"*.key\", \"secrets/*\" or \"deploy/**\" — `sc config show` prints the merged list"
	startupFixGH = "run: gh auth login — or fire with --safety off to merge locally instead of opening pull requests (D-10)"
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
		problem: "sc fires one cannon at a repository at a time",
		fix:     startupFixLock,
		checks:  nil,
		quiet:   startupLockSaidItAll,
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
		// P9-05: an `exclude` entry that is not a valid glob. It is a
		// configuration failure, but not one config.Parse can raise — the
		// value is a well-formed string until agents.NewSecretFilter
		// compiles it — so it needs its own class to get a fix line.
		name:    "config",
		match:   func(err error) bool { return strings.Contains(err.Error(), "exclude pattern") },
		checks:  []string{"config"},
		problem: "an `exclude` pattern in the configuration is not a valid glob",
		fix:     startupFixExclude,
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
	// Events the event log and Worktrees the directory holding one git
	// worktree per bead. Prompts is the directory of debug copies of the
	// prompts agents were fired with, and is empty — and then not printed
	// at all — when the run wrote none: [orchestrator.EnsureRunDir] does
	// not create it, and only a firing that calls
	// [slop-cannon/internal/agents/prompt.Prompt.Write] fills it, which
	// today is the planning product owner alone (P5-13 is the open task
	// that makes every firing leave its copy). The notice names a
	// directory a reader can open, never one that is not there.
	Log       string `json:"log"`
	Agents    string `json:"agents"`
	Prompts   string `json:"prompts,omitempty"`
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
		Events:    orchestrator.EventLogPath(dirs.Dir),
		Worktrees: dirs.Worktrees,
		Config:    config.GlobalPath(),
	}
	if prompts := prompt.Dir(dirs.Dir); frDirExists(prompts) {
		n.Prompts = prompts
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
// It is the only thing that answers the question, because it is the only
// thing a run cannot fail to leave behind honestly. Counting run
// directories cannot: `sc --resume` reuses the latest run directory and its
// id, so a resumed first run still sees exactly one directory named after
// itself; and a run that is killed outright (kill -9, a power cut, the OOM
// killer — P9-03 makes that a supported, tested path) leaves a run
// directory and no marker, so a count would see two directories from then
// on and the repository would never be told where anything lives.
const firstRunNoticeName = "first-run-notice"

// firstRunNoticePath is where that marker lives for repoPath.
func firstRunNoticePath(repoPath string) string {
	return filepath.Join(config.StateDir(repoPath), firstRunNoticeName)
}

// FirstRunInRepo reports whether the first-run notice is still owed to
// repoPath: the marker beside the run directories does not exist yet, so
// no run has ever printed it there. [frClaimNotice] creates that marker
// exclusively, which is what keeps a resumed first run, and two cannons
// racing at one repository, from printing it twice.
//
// A marker that cannot be stat'ed for any reason other than "it is not
// there" answers false: the notice is a courtesy, and printing it twice is
// worse than not printing it at all.
func FirstRunInRepo(repoPath string) bool {
	_, err := os.Stat(firstRunNoticePath(repoPath))
	return errors.Is(err, os.ErrNotExist)
}

// FirstRunNoticeEarned reports whether a run that finished for this reason
// is the "first successful run" the notice is owed to (P8-06): a run that
// completed is, and a run that was stopped, failed, or ran out of credits
// is not. An interrupted first run must not spend the notice — the marker
// is claimed only when it is printed, so the notice waits for the run that
// gets there.
func FirstRunNoticeEarned(reason events.FinishReason) bool {
	return reason == events.FinishCompleted
}

// ReportFirstRun prints [FirstRunNotice] after the first run that finishes
// successfully in a repository ([FirstRunNoticeEarned]), and does nothing
// otherwise — for a later run, for that same first run resumed, and for a
// run that was stopped or failed, whose notice is left unspent for the run
// that succeeds. Call it after a run finishes.
func ReportFirstRun(w io.Writer, run domain.Run, reason events.FinishReason) error {
	if !FirstRunNoticeEarned(reason) {
		return nil
	}
	if !FirstRunInRepo(run.RepoPath) {
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
	}
	if n.Prompts != "" {
		rows = append(rows, [2]string{"prompts", n.Prompts})
	}
	rows = append(rows,
		[2]string{"events", n.Events},
		[2]string{"worktrees", n.Worktrees},
		[2]string{"config", n.Config},
	)
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

// frDirExists reports whether path is a directory that is really there. The
// notice names locations a reader is meant to open, so a path it cannot
// vouch for is left out rather than printed.
func frDirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
