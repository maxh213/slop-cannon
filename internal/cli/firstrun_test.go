package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/gitx"
	"slop-cannon/internal/orchestrator"
)

// frAdvise prints the advice for err through the whole first-run failure
// path and returns what the user would see on stderr. The returned error is
// always the quiet *ExitError, because the advice is the message.
func frAdvise(t *testing.T, repo string, err error) string {
	t.Helper()
	var buf bytes.Buffer
	got := StartupFailure(context.Background(), &buf, repo, err)
	var exit *ExitError
	require.ErrorAs(t, got, &exit, "the command should exit 1 without printing the error twice")
	require.Equal(t, 1, exit.Code)
	require.NoError(t, exit.Err, "the advice already said everything")
	return buf.String()
}

// TestStartupNoCartridgeGolden is the commonest first failure of all: sc is
// installed, the repository is fine, and no cartridge has been added yet.
func TestStartupNoCartridgeGolden(t *testing.T) {
	doctorEnv(t)
	repo := doctorRepoDir(t, true, false)
	git := doctorGitShim(t, "2.55.0", "")
	bd := doctorBDVersionShim(t, "1.2.2")

	_, err := ResolveRun(RunOptions{Path: repo})
	require.ErrorIs(t, err, config.ErrNoCartridge)

	out := frAdvise(t, repo, err)
	rootGolden(t, "firstrun-no-cartridge", doctorNormalise(out, repo, git, bd))

	require.Contains(t, out, "1. cartridge: no cartridge configured")
	require.Contains(t, out, CartridgeAddHint)
	require.NotContains(t, out, ".beads", "an uninitialised bead tracker is not a problem: sc creates it (D-13)")
	require.NotContains(t, out, "gh ", "gh is not why this run would not start")
}

// TestStartupNoBeadsBinaryGolden covers the second: everything is
// configured and bd was never installed, so no bead tracker can be prepared.
func TestStartupNoBeadsBinaryGolden(t *testing.T) {
	doctorEnv(t)
	doctorAddCartridge(t, "claude", "fable")
	repo := doctorRepoDir(t, true, false)
	git := doctorGitShim(t, "2.55.0", "")
	claude := doctorClaudeShim(t)

	_, err := beads.EnsureInitialised(context.Background(), repo, beads.InitOptions{})
	require.Error(t, err)
	err = fmt.Errorf("preparing the bead tracker: %w", err)

	out := frAdvise(t, repo, err)
	rootGolden(t, "firstrun-no-bd", doctorNormalise(out, repo, git, claude))

	require.Contains(t, out, "1. bd: not found on PATH")
	require.Contains(t, out, beads.InstallHint)
}

// TestStartupNotAGitRepositoryGolden covers the third: sc was fired at a
// directory that is not a repository, on a machine where nothing is
// installed yet either.
func TestStartupNotAGitRepositoryGolden(t *testing.T) {
	doctorEnv(t)
	repo := doctorRepoDir(t, false, false)

	_, err := ResolveRun(RunOptions{Path: repo})
	require.Error(t, err)

	out := frAdvise(t, repo, err)
	rootGolden(t, "firstrun-not-a-repo", doctorNormalise(out, repo))

	// The failure's own class leads, whatever order sc doctor reports in.
	require.Contains(t, out, "1. repo: not a git repository")
	require.Contains(t, out, "2. git: not found on PATH")
	require.Contains(t, out, "3. bd: not found on PATH")
}

// TestStartupAdviceTextComesFromDoctorRows is P8-09: the fix list is the
// sc doctor report, not a second copy of the same advice that can drift
// away from it.
func TestStartupAdviceTextComesFromDoctorRows(t *testing.T) {
	report := &DoctorReport{
		Repo: "/repo",
		Checks: []DoctorCheck{
			{Name: "git", Status: DoctorPass, Detail: "2.55.0 (/usr/bin/git)"},
			{Name: "bd", Status: DoctorFail, Detail: "bd detail from doctor", Hint: "bd hint from doctor"},
			{Name: "gh", Status: DoctorWarn, Detail: "gh detail from doctor", Hint: "gh hint from doctor"},
			{Name: "cartridge", Status: DoctorWarn, Detail: "cartridge detail from doctor", Hint: "cartridge hint from doctor"},
		},
	}
	prev := firstRunDoctor
	t.Cleanup(func() { firstRunDoctor = prev })
	firstRunDoctor = func(context.Context, string) *DoctorReport { return report }

	a := AdviseStartup(context.Background(), "/repo", fmt.Errorf("wrapped: %w", config.ErrNoCartridge))
	require.Equal(t, []StartupStep{
		{Check: "cartridge", Problem: "cartridge detail from doctor", Fix: "cartridge hint from doctor"},
		{Check: "bd", Problem: "bd detail from doctor", Fix: "bd hint from doctor"},
	}, a.Steps, "the class's own row leads; failing rows follow; unrelated warnings stay out")

	out := frRenderAdvice(t, a)
	for _, want := range []string{"bd detail from doctor", "bd hint from doctor", "cartridge detail from doctor", "cartridge hint from doctor"} {
		require.Contains(t, out, want)
	}
	require.NotContains(t, out, "gh hint from doctor", "gh is a warning unrelated to this failure")
	require.NotContains(t, out, "2.55.0", "a passing row is not a fix")
}

// TestStartupAdviceFallsBackToTheErrorsOwnFix covers a failure sc doctor
// cannot see at all: another run holds the lock.
func TestStartupAdviceFallsBackToTheErrorsOwnFix(t *testing.T) {
	prev := firstRunDoctor
	t.Cleanup(func() { firstRunDoctor = prev })
	firstRunDoctor = func(context.Context, string) *DoctorReport {
		return &DoctorReport{Checks: []DoctorCheck{{Name: "git", Status: DoctorPass, Detail: "fine"}}}
	}

	err := fmt.Errorf("%w: run 20260101-000000-abcd is already going in this repository", orchestrator.ErrLocked)
	a := AdviseStartup(context.Background(), "/repo", err)
	require.Len(t, a.Steps, 1)
	require.Equal(t, "lock", a.Steps[0].Check)
	require.Equal(t, startupFixLock, a.Steps[0].Fix)
	require.Contains(t, frRenderAdvice(t, a), "sc doctor")
}

// TestStartupAdviceKnowsEveryClass pins the classification: each of these
// errors must reach its own class, so the fix a user is given matches the
// failure they hit.
func TestStartupAdviceKnowsEveryClass(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"lock", orchestrator.ErrLocked, "lock"},
		{"not a repo", gitx.ErrNotRepository, "repo"},
		{"no cartridge", config.ErrNoCartridge, "cartridge"},
		{"cartridge not built", cartridge.ErrCartridgeNotBuilt, "cartridge"},
		{"not authenticated", cartridge.ErrAuth, "cartridge"},
		{"credits", cartridge.ErrCreditsExhausted, "cartridge"},
		{"bd missing", &beads.CheckError{Command: "bd version", Reason: beads.ReasonNotFound}, "bd"},
		{"no beads project", beads.ErrNoProject, "bd"},
		{"gh", gitx.ErrGHUnauthenticated, "gh"},
		{"dirty tree", gitx.ErrRepoDirty, "repo"},
		{"bad config", &config.ParseError{Path: "/c.toml", Line: 3, Msg: "bad"}, "config"},
		{"ammo", config.ErrAmmoTooLow, "ammo"},
		{"terminal", errors.New("the TUI needs a terminal: stdin and stdout are not a TTY"), "terminal"},
		{"unknown", errors.New("something else entirely"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := fmt.Errorf("wrapped: %w", tc.err)
			class := startupClassOf(wrapped)
			require.Equal(t, tc.want, class.name)
			step, ok := class.step(wrapped)
			if tc.want == "" {
				require.False(t, ok, "an unrecognised failure invents no fix")
				return
			}
			require.True(t, ok, "every recognised class has something to tell the user")
			require.NotEmpty(t, step.Fix)
			require.NotEmpty(t, step.Problem)
			require.NotEqual(t, startupReason(wrapped), step.Problem,
				"the reason is printed above the list; a step must not repeat it")
		})
	}
}

// TestStartupAdviceWithNothingToSayStillPointsAtDoctor keeps the exit from
// an unrecognised failure honest: no invented fixes, but never a dead end.
func TestStartupAdviceWithNothingToSayStillPointsAtDoctor(t *testing.T) {
	prev := firstRunDoctor
	t.Cleanup(func() { firstRunDoctor = prev })
	firstRunDoctor = func(context.Context, string) *DoctorReport { return &DoctorReport{OK: true} }

	out := frAdvise(t, "/repo", errors.New("the moon was in the wrong phase"))
	require.Equal(t, "sc could not start a run in /repo: the moon was in the wrong phase\n\nThen check everything at once with: sc doctor\n", out)
}

// TestStartupAdviceFoldsAMultiLineError keeps a wrapped multi-line error
// from breaking the numbered layout.
func TestStartupAdviceFoldsAMultiLineError(t *testing.T) {
	require.Equal(t, "one two three", startupReason(errors.New("one\ntwo\n\n  three  ")))
	require.Equal(t, "", startupReason(nil))
}

// TestStartupFailureKeepsTheErrorWhenItCannotPrint falls back to the error
// itself when the advice cannot be written.
func TestStartupFailureKeepsTheErrorWhenItCannotPrint(t *testing.T) {
	prev := firstRunDoctor
	t.Cleanup(func() { firstRunDoctor = prev })
	firstRunDoctor = func(context.Context, string) *DoctorReport { return &DoctorReport{} }

	want := errors.New("boom")
	got := StartupFailure(context.Background(), frBrokenWriter{}, "/repo", want)
	require.ErrorIs(t, got, want)
	require.NoError(t, StartupFailure(context.Background(), frBrokenWriter{}, "/repo", nil))
}

// frBrokenWriter fails every write, standing in for a closed stderr.
type frBrokenWriter struct{}

func (frBrokenWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

// renderAdvice returns what WriteStartupAdvice prints for a.
func frRenderAdvice(t *testing.T, a StartupAdvice) string {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, WriteStartupAdvice(&buf, a))
	return buf.String()
}

// ---------------------------------------------------------------------------
// The first successful run in a repository
// ---------------------------------------------------------------------------

// frRun builds a run whose state directory is the D-23 one for repo.
func frRun(repo string, id domain.RunID) domain.Run {
	return domain.Run{ID: id, RepoPath: repo, StateDir: config.RunDir(repo, id.String())}
}

// frMakeRunDir creates the run directory of id, the way a real run does.
func frMakeRunDir(t *testing.T, repo string, id domain.RunID) {
	t.Helper()
	_, err := orchestrator.EnsureRunDir(config.RunDir(repo, id.String()))
	require.NoError(t, err)
}

func TestFirstRunNoticeGolden(t *testing.T) {
	home := doctorEnv(t)
	repo := doctorRepoDir(t, true, false)
	id := domain.RunID("20260101-120000-abcd")
	frMakeRunDir(t, repo, id)

	var buf bytes.Buffer
	require.NoError(t, ReportFirstRun(&buf, frRun(repo, id)))
	out := buf.String()
	require.NotEmpty(t, out)

	rootGolden(t, "firstrun-notice", frNormalise(out, repo))
	require.NotContains(t, out, repo+string(filepath.Separator)+".slop-cannon",
		"nothing sc writes lives inside the repository (D-23)")
	_ = home
}

// TestFirstRunNoticeNamesTheRepoConfigWhenThereIsOne adds the one file sc
// does write inside the repository, and only when it exists.
func TestFirstRunNoticeNamesTheRepoConfigWhenThereIsOne(t *testing.T) {
	doctorEnv(t)
	repo := doctorRepoDir(t, true, false)
	id := domain.RunID("20260101-120000-abcd")
	frMakeRunDir(t, repo, id)

	n := NewFirstRunNotice(frRun(repo, id))
	require.Empty(t, n.RepoConfig)

	path := config.RepoConfigPath(repo)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("safety = \"off\"\n"), 0o600))

	n = NewFirstRunNotice(frRun(repo, id))
	require.Equal(t, path, n.RepoConfig)

	var buf bytes.Buffer
	require.NoError(t, WriteFirstRunNotice(&buf, n))
	out := buf.String()
	require.Contains(t, out, "repo config "+path)
	// The header is about what the run wrote, and the one path above that
	// is inside the repository says so itself, instead of standing under a
	// sentence that denies it exists.
	require.Contains(t, out, "Nothing the run wrote lives inside the repository")
	require.Contains(t, out, "The repo config is the only sc file inside the repository")
	require.Contains(t, out, "an sc config command wrote it, and the run only read it")
}

// TestFirstRunNoticeSaysNothingAboutARepoConfigThatIsNotThere keeps the
// explanation with the row it explains.
func TestFirstRunNoticeSaysNothingAboutARepoConfigThatIsNotThere(t *testing.T) {
	doctorEnv(t)
	repo := doctorRepoDir(t, true, false)
	id := domain.RunID("20260101-120000-abcd")
	frMakeRunDir(t, repo, id)

	var buf bytes.Buffer
	require.NoError(t, WriteFirstRunNotice(&buf, NewFirstRunNotice(frRun(repo, id))))
	require.NotContains(t, buf.String(), "repo config")
	require.NotContains(t, buf.String(), "only sc file inside the repository")
}

// TestFirstRunNoticeIsPrintedOnlyOnce is the whole point: the second run in
// a repository says nothing.
func TestFirstRunNoticeIsPrintedOnlyOnce(t *testing.T) {
	doctorEnv(t)
	repo := doctorRepoDir(t, true, false)
	first := domain.RunID("20260101-120000-aaaa")
	second := domain.RunID("20260102-120000-bbbb")

	// Before anything has run there is no runs directory at all.
	require.False(t, FirstRunInRepo(repo, first))

	frMakeRunDir(t, repo, first)
	require.True(t, FirstRunInRepo(repo, first))

	frMakeRunDir(t, repo, second)
	require.False(t, FirstRunInRepo(repo, second), "the second run is not the first")
	require.False(t, FirstRunInRepo(repo, first))

	var buf bytes.Buffer
	require.NoError(t, ReportFirstRun(&buf, frRun(repo, second)))
	require.Empty(t, buf.String())
}

// TestFirstRunNoticeCoversEveryPlaceARunWrites: every path in the notice is
// one the run really uses, and all of them are under the run directory.
func TestFirstRunNoticeCoversEveryPlaceARunWrites(t *testing.T) {
	doctorEnv(t)
	repo := doctorRepoDir(t, true, false)
	id := domain.RunID("20260101-120000-abcd")
	dirs, err := orchestrator.EnsureRunDir(config.RunDir(repo, id.String()))
	require.NoError(t, err)

	n := NewFirstRunNotice(frRun(repo, id))
	require.Equal(t, dirs.Dir, n.Dir)
	require.Equal(t, dirs.Agents, n.Agents)
	require.Equal(t, dirs.Worktrees, n.Worktrees)
	require.Equal(t, orchestrator.EventLogPath(dirs.Dir), n.Events)
	require.Equal(t, config.GlobalPath(), n.Config)
	for _, p := range []string{n.Log, n.Agents, n.Prompts, n.Events, n.Worktrees} {
		require.True(t, strings.HasPrefix(p, dirs.Dir+string(filepath.Separator)), p)
		require.False(t, strings.HasPrefix(p, repo+string(filepath.Separator)), "D-23: not inside the repository: "+p)
	}
}

// TestNewFirstRunNoticeDerivesTheRunDirectory covers a run value that
// carries no state directory of its own.
func TestNewFirstRunNoticeDerivesTheRunDirectory(t *testing.T) {
	doctorEnv(t)
	repo := doctorRepoDir(t, true, false)
	id := domain.RunID("20260101-120000-abcd")
	n := NewFirstRunNotice(domain.Run{ID: id, RepoPath: repo})
	require.Equal(t, config.RunDir(repo, id.String()), n.Dir)
}

// frNormalise replaces the machine's temp paths with stable placeholders.
func frNormalise(out, repo string) string {
	out = strings.ReplaceAll(out, config.StateDir(repo), "/state")
	out = strings.ReplaceAll(out, config.GlobalPath(), "/config/slop-cannon/config.toml")
	return strings.ReplaceAll(out, repo, "/repo")
}

// TestFirstRunNoticeIsNotRepeatedWhenTheSameRunIsResumed is the resume
// half of "printed only once": `sc --resume` reuses the latest run
// directory and its id, so counting run directories still finds exactly one
// named after this run. The notice is owed to a repository, not to a run
// directory, and the marker beside the run directories is what remembers
// that it was paid.
func TestFirstRunNoticeIsNotRepeatedWhenTheSameRunIsResumed(t *testing.T) {
	doctorEnv(t)
	repo := doctorRepoDir(t, true, false)
	id := domain.RunID("20260101-120000-abcd")
	frMakeRunDir(t, repo, id)

	var first bytes.Buffer
	require.NoError(t, ReportFirstRun(&first, frRun(repo, id)))
	require.Contains(t, first.String(), "That was the first sc run in")

	// Nothing on disk tells the two apart but the marker: the state
	// directory still holds exactly one run directory, and it is still
	// this run's, which is what made the count answer "first" twice.
	entries, err := os.ReadDir(filepath.Join(config.StateDir(repo), config.RunsDirName))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, id.String(), entries[0].Name())

	// The same run, resumed: same id, same directory, nothing new to say.
	var again bytes.Buffer
	require.NoError(t, ReportFirstRun(&again, frRun(repo, id)))
	require.Empty(t, again.String(), "a resumed first run does not print the notice twice")
	require.False(t, FirstRunInRepo(repo, id))

	marker := firstRunNoticePath(repo)
	require.FileExists(t, marker)
	require.False(t, strings.HasPrefix(marker, repo+string(filepath.Separator)),
		"D-23: the marker lives in the state directory, not in the repository")
	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Equal(t, id.String()+"\n", string(data), "the marker names the run that earned the notice")
}

// TestFirstRunNoticeIsPrintedWhenTheMarkerCannotBeRead covers a state
// directory that has been emptied by hand: the notice is a courtesy, so a
// missing marker means "not printed yet" rather than an error.
func TestFirstRunNoticeIsPrintedWhenTheMarkerCannotBeRead(t *testing.T) {
	doctorEnv(t)
	repo := doctorRepoDir(t, true, false)
	id := domain.RunID("20260101-120000-abcd")
	frMakeRunDir(t, repo, id)

	var buf bytes.Buffer
	require.NoError(t, ReportFirstRun(&buf, frRun(repo, id)))
	require.NotEmpty(t, buf.String())

	require.NoError(t, os.Remove(firstRunNoticePath(repo)))
	buf.Reset()
	require.NoError(t, ReportFirstRun(&buf, frRun(repo, id)))
	require.NotEmpty(t, buf.String())
}

// TestRunResumedDoesNotRepeatTheFirstRunNotice drives the same thing
// through the production command: a first run in a repository prints the
// notice, and resuming that very run does not print it again.
func TestRunResumedDoesNotRepeatTheFirstRunNotice(t *testing.T) {
	env := runFireSetup(t, false)

	_, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief, "--headless", "--cartridge", "mock")
	require.Equal(t, 0, code, stderr)
	require.Contains(t, stderr, "That was the first sc run in")

	_, stderr, code = runFireExec(t, env.Repo.Dir, "--resume", "--headless", "--cartridge", "mock")
	require.Equal(t, 0, code, stderr)
	require.NotContains(t, stderr, "That was the first sc run in",
		"resuming the first run reuses its directory and its id, and must not repeat the notice")
}

// TestStartupWithoutATerminalPrintsTheFixList is the "terminal" class
// through the command a user actually types: `sc <repo>` with no TTY. The
// refusal goes through the same numbered list as every other startup
// failure, so --headless is a step rather than a parenthesis (P8-06).
func TestStartupWithoutATerminalPrintsTheFixList(t *testing.T) {
	_, repo := runTestEnv(t)
	runAddCartridge(t, "claude", "fable")
	runStubTerminal(t, false)
	// The class asks sc doctor nothing, so this cannot probe the machine.
	prev := firstRunDoctor
	t.Cleanup(func() { firstRunDoctor = prev })
	firstRunDoctor = func(context.Context, string) *DoctorReport {
		t.Error("a missing terminal is not something sc doctor can see")
		return &DoctorReport{}
	}

	stdout, stderr, code := runExec(t, repo)
	require.Equal(t, 1, code)
	require.Empty(t, stdout)
	require.Contains(t, stderr, "the TUI needs a terminal")
	require.Contains(t, stderr, "Fix it in this order:")
	require.Contains(t, stderr, "1. terminal: sc was not started from a terminal")
	require.Contains(t, stderr, "sc <repo> --"+FlagRunHeadless)
	require.Contains(t, stderr, "Then check everything at once with: sc doctor")
}

// TestStartupAdviceNeverRepeatsTheReason covers the fallback path of every
// class — the one taken when sc doctor has no row about the failure. The
// error is the reason line at the top; a step that prints the same sentence
// again is noise.
func TestStartupAdviceNeverRepeatsTheReason(t *testing.T) {
	prev := firstRunDoctor
	t.Cleanup(func() { firstRunDoctor = prev })
	firstRunDoctor = func(context.Context, string) *DoctorReport {
		return &DoctorReport{OK: true, Checks: []DoctorCheck{{Name: "git", Status: DoctorPass, Detail: "fine"}}}
	}

	for _, err := range []error{
		orchestrator.ErrLocked,
		gitx.ErrNotRepository,
		config.ErrNoCartridge,
		cartridge.ErrAuth,
		cartridge.ErrCreditsExhausted,
		beads.ErrNoProject,
		gitx.ErrGHUnauthenticated,
		gitx.ErrRepoDirty,
		&config.ParseError{Path: "/c.toml", Line: 3, Msg: "bad"},
		config.ErrAmmoTooLow,
		errors.New("the TUI needs a terminal: stdin and stdout are not a TTY"),
	} {
		wrapped := fmt.Errorf("wrapped: %w", err)
		class := startupClassOf(wrapped)
		t.Run(class.name+"/"+err.Error(), func(t *testing.T) {
			a := AdviseStartup(context.Background(), "/repo", wrapped)
			require.Len(t, a.Steps, 1, "no doctor row fails, so the class's own step is the list")
			out := frRenderAdvice(t, a)
			require.Equal(t, 1, strings.Count(out, a.Reason),
				"the error is printed once, as the reason:\n%s", out)
			require.NotEmpty(t, a.Steps[0].Fix, "and the step is worth its line")
		})
	}
}

// TestStartupRepoAdviceIsTheDoctorRowsOwnWords is P8-09 for the fallback
// step: when the report carries no repo row, the advice still uses the row's
// wording rather than a second spelling of it.
func TestStartupRepoAdviceIsTheDoctorRowsOwnWords(t *testing.T) {
	doctorEnv(t)
	dir := doctorRepoDir(t, false, false)

	report := RunDoctor(context.Background(), dir)
	var hint string
	for _, c := range report.Checks {
		if c.Name == "repo" {
			hint = c.Hint
		}
	}
	require.NotEmpty(t, hint, "sc doctor has a repo row with a hint")
	require.Equal(t, hint, startupFixRepo, "worded once (P8-09): the fallback step is the doctor row's hint")

	prev := firstRunDoctor
	t.Cleanup(func() { firstRunDoctor = prev })
	firstRunDoctor = func(context.Context, string) *DoctorReport { return &DoctorReport{OK: true} }
	a := AdviseStartup(context.Background(), dir, fmt.Errorf("wrapped: %w", gitx.ErrNotRepository))
	require.Len(t, a.Steps, 1)
	require.Equal(t, hint, a.Steps[0].Fix)
}

// ---------------------------------------------------------------------------
// P9-08: the troubleshooting doc and the messages in the code
// ---------------------------------------------------------------------------

// tsDoc is the page every user-facing message is documented on.
const tsDoc = "../../docs/troubleshooting.md"

// tsPackages are the packages — and every package under them — whose
// exported sentinel errors must be documented: everything a user can reach
// through the CLI. internal/domain, internal/events and internal/logging
// are left out on purpose — their errors are the vocabulary of the
// libraries underneath, and every one that reaches a terminal does so
// wrapped in a message from this list.
var tsPackages = []string{
	"internal/cli",
	"internal/config",
	"internal/beads",
	"internal/gitx",
	"internal/cartridge",
	"internal/agents",
	"internal/orchestrator",
	"internal/proc",
}

// P9-09 shipped a second guard over the same ground
// (internal/cli/troubleshooting_messages_test.go: internal/cli only, raw
// format strings, its own exemption map). The integrator removed it in
// round 3: this one is the strict superset — it covers internal/config as
// well, and it renders a format's string constants instead of leaving them
// as verbs — and two guards meant a new message had to satisfy two
// contracts that disagreed about several existing ones.
//
// tsMessagePackages are the packages where *every* message a non-test file
// builds — each fmt.Errorf as well as each errors.New — must be documented
// or exempted, not only the exported sentinels. They are the two packages
// the words a user reads are written in: internal/cli is the command layer,
// which prints what it returns as `sc: <message>`, and internal/config
// reads the files a user edits by hand. The deeper packages are covered by
// their sentinels; their wrapped messages are P9-19.
var tsMessagePackages = []string{
	"internal/cli",
	"internal/config",
}

// tsWildcard stands for a formatting verb whose argument is not a string
// constant, so no documented key may span one: a key has to match text a
// user can actually read, not text interrupted by a path or an id.
const tsWildcard = "\x00"

// tsExempt are the sentinels docs/troubleshooting.md deliberately does not
// document, each with the reason. They are assembly-time invariants of sc's
// own wiring or internal control flow: reaching one is a bug in sc, not a
// situation a user can be advised out of. A new sentinel must be documented
// or listed here, and a stale entry fails too.
var tsExempt = map[string]string{
	"cli.ErrNotImplemented":               "dead placeholder: no released command returns it, and P9-16 decides whether it is deleted",
	"agents.ErrInvalidAgentRun":           "a firing sc assembled wrongly; the user cannot cause it",
	"agents.ErrInvalidPlanRequest":        "same: the planning request is built by sc",
	"agents.ErrIncompleteRuling":          "same: the arbitration request is built by sc",
	"agents.ErrUnknownMessageType":        "a schema lookup for a type that does not exist: a programming error",
	"beads.ErrNoJSON":                     "internal decoding step; callers report the bd error around it",
	"beads.ErrNoMessage":                  "control flow: a bd comment that is not one of sc's structured messages",
	"beads.ErrBadMessage":                 "control flow: the same comment when it is malformed; the run logs and skips it",
	"beads.ErrUnsupportedPayload":         "encoding guard for a message type sc did not define",
	"beads.ErrShimLayout":                 "the read-only bd shim was asked for a directory that is not a run worktree: sc's own layout",
	"beads.ErrNoGate":                     "control flow: 'this bead has no open gate', which the gate poller expects",
	"mock.ErrNoMatch":                     "the mock cartridge is a test double; it is never fired at a user's repository",
	"orchestrator.ErrNoDeliveryHost":      "assembly invariant: the deliverer is always built with a host",
	"orchestrator.ErrNoGateStore":         "assembly invariant: the gate poller is always built with a store",
	"orchestrator.ErrScopeNeedsBeads":     "assembly invariant: a scoped poller is always built with a bead reader",
	"orchestrator.ErrNoPlanConfig":        "assembly invariant: the planner is always built with the run config",
	"orchestrator.ErrNoPlanRunner":        "assembly invariant: the planner is always built with a runner",
	"orchestrator.ErrNoPlanApplier":       "assembly invariant: the planner is always built with an applier",
	"orchestrator.ErrNoResumeStore":       "assembly invariant: the resume tracker is always built with a store",
	"orchestrator.ErrNoResumeReader":      "assembly invariant: the resume tracker is always built with a reader",
	"orchestrator.ErrNoSupervisorHost":    "assembly invariant: the supervisor is attached before it fires",
	"orchestrator.ErrReleaseOnlyStore":    "reaching it is a bug, as its own doc comment says",
	"orchestrator.ErrNotAPlanDemand":      "routing guard between the planner and the feedback loop",
	"orchestrator.ErrUnknownFeedbackWork": "the same guard from the other side",
	"orchestrator.ErrNotAResumeEdge":      "internal guard of the resume tracker",
	"orchestrator.ErrNoFirer":             "assembly invariant: the scheduler is always built with a firer",
	"orchestrator.ErrNotStarted":          "lifecycle guard: Wait or Pause before Start",
	"orchestrator.ErrAlreadyStarted":      "lifecycle guard: a second Start on one run",
	"orchestrator.ErrInvalidFiring":       "a firing sc assembled wrongly; the cause is reported with it",
	"orchestrator.ErrUnknownBeadEvent":    "a bead event that does not exist: a programming error",
	"orchestrator.ErrNotWorkflowState":    "a state outside the D-07 table; the user-facing half is the foreign-status message",
	"orchestrator.ErrNoAgent":             "a claiming transition applied without an agent id: a programming error",
	"orchestrator.ErrNoFeedbackThread":    "control flow: the loop is not tracking this bead",
	"orchestrator.ErrNoPRWatcher":         "assembly invariant: the pull request reviewer is always built with a watcher",
	"orchestrator.ErrNoPRReviewBead":      "control flow: the review bead was settled while the firing waited",
	"orchestrator.ErrNoPRCheckout":        "assembly invariant: the reviewer is always built with a checkout",
	"orchestrator.ErrNoFeedbackContext":   "control flow: no prompt context for a bead the loop dropped",
}

// tsExemptMessages are the messages of tsMessagePackages that the page does
// not document, keyed by the text the code builds (with "…" wherever a verb
// interpolates something that is not a string constant), each with the
// reason. Almost all of them are wrappers: they name the step or the file
// and hand on an error whose own message is what the user reads, and that
// message is documented on its own row. A pure wrapper — one whose static
// text is punctuation, like "%s: %w" — needs no entry: tsSaysSomething
// skips it.
var tsExemptMessages = map[string]string{
	"editor …: …": "wraps the editor's own failure; the words a user reads come from the editor",
	"read …: …":   "names the log file a read failed on; the message is the OS error's",
	"--repo …: …": "names the flag; the wrapped error is the documented `no such directory`/`not a directory`/`is not a git repository`",
	"--safety: …": "names the flag; the wrapped error is the documented `safety must be on or off, got`",
	"…: … file …": "names the vibes or rules file that could not be read; the wrapped error is documented on its own row",
	"… — add one with: sc cartridge --add claude --model <model>": "decorates the documented `no cartridge loaded` with the command that fixes it",
	"cli: newPRReviewer: no run configuration":                    "assembly invariant: the pull request reviewer is always built with a run configuration",
	"run config: effective configuration is nil":                  "assembly invariant: sc resolves the configuration before it builds a run",
	"open repo config: repository root is empty":                  "assembly invariant: the repository path is resolved before a config file is read",
	"save repo config: repository root is empty":                  "the same, for the write half",
	"encode config: …":                                            "wraps the TOML encoder; reaching it means sc built a value TOML cannot hold",
	"create config directory: …":                                  "names the step; the message is the OS error's, and `sc doctor` reports the directory",
	"write config: …":                                             "the same, for the write itself",
	"list runs: …":                                                "names the step; the message is the OS error's",
	"read config: …":                                              "names the step; the message is the OS error's",
	"migrate config from schema … to …: …":                        "names the step; the wrapped error is the migration's own",
	"--ammo …: …":                                                 "names the flag; the wrapped error is the documented `ammo must be at least 3`",
}

// tsSentinel is one Err… = errors.New("…") declaration found in the source.
type tsSentinel struct {
	pkg  string // the package's base name, e.g. "beads"
	name string // the identifier, e.g. "ErrNotFound"
	msg  string // the message text
	file string
}

// key is how a sentinel is named in tsExempt.
func (s tsSentinel) key() string { return s.pkg + "." + s.name }

// tsMessage is one message a non-test file builds with errors.New or
// fmt.Errorf, rendered as far as the source allows: every verb whose
// argument is a string constant is substituted, and the rest become
// tsWildcard, so a documented key can only match text a user really sees.
type tsMessage struct {
	text string // the rendered text, with tsWildcard for unknown verbs
	file string
	line int
}

// display renders the message the way a failure should print it.
func (m tsMessage) display() string { return strings.ReplaceAll(m.text, tsWildcard, "…") }

// tsGoFiles lists every non-test .go file of a package and the packages
// under it. Recursing matters: a sentinel in internal/cartridge/structured
// is as user-facing as one in internal/cartridge.
func tsGoFiles(t *testing.T, pkg string) []string {
	t.Helper()
	var out []string
	dir := filepath.Join("..", "..", pkg)
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			out = append(out, path)
		}
		return nil
	})
	require.NoError(t, err, dir)
	require.NotEmpty(t, out, pkg)
	return out
}

// tsParse parses one file, failing the test if it cannot.
func tsParse(t *testing.T, path string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	require.NoError(t, err, path)
	return f
}

// tsSentinels parses every package in tsPackages and returns its exported
// sentinel errors declared as errors.New with a literal message.
func tsSentinels(t *testing.T) []tsSentinel {
	t.Helper()
	var out []tsSentinel
	seen := map[string]bool{}
	for _, pkg := range tsPackages {
		for _, path := range tsGoFiles(t, pkg) {
			if seen[path] {
				continue // a package listed under one already walked
			}
			seen[path] = true
			f := tsParse(t, path)
			ast.Inspect(f, func(n ast.Node) bool {
				spec, ok := n.(*ast.ValueSpec)
				if !ok {
					return true
				}
				for i, name := range spec.Names {
					if !strings.HasPrefix(name.Name, "Err") || i >= len(spec.Values) {
						continue
					}
					msg, ok := tsErrorsNew(spec.Values[i])
					if !ok {
						continue
					}
					out = append(out, tsSentinel{pkg: f.Name.Name, name: name.Name, msg: msg, file: path})
				}
				return true
			})
		}
	}
	require.NotEmpty(t, out)
	return out
}

// tsErrorsNew returns the literal message of an errors.New("…") expression.
func tsErrorsNew(expr ast.Expr) (string, bool) {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "New" {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "errors" {
		return "", false
	}
	return tsLiteral(call.Args[0])
}

// tsLiteral returns the value of a string literal, or of a concatenation of
// string literals — how a long message is wrapped across lines.
func tsLiteral(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		if err != nil {
			return "", false
		}
		return s, true
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		l, ok := tsLiteral(e.X)
		if !ok {
			return "", false
		}
		r, ok := tsLiteral(e.Y)
		if !ok {
			return "", false
		}
		return l + r, true
	default:
		return "", false
	}
}

// tsConsts holds every package-level string constant of the module, by the
// directory it is declared in and by its package's base name, so a verb's
// argument can be substituted: FlagRepo becomes "repo" and
// logging.RunLogName becomes "sc.log". Without it a message like
// "--%s requires --%s NAME" could only ever be documented by a fragment.
type tsConsts struct {
	byDir map[string]map[string]string
	byPkg map[string]map[string]string
}

// lookup resolves an expression to the string it evaluates to at compile
// time, within the file's directory.
func (c tsConsts) lookup(dir string, expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit, *ast.BinaryExpr:
		return tsLiteral(expr)
	case *ast.Ident:
		v, ok := c.byDir[dir][e.Name]
		return v, ok
	case *ast.SelectorExpr:
		pkg, ok := e.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		v, ok := c.byPkg[pkg.Name][e.Sel.Name]
		return v, ok
	default:
		return "", false
	}
}

// tsStringConsts collects the module's package-level string constants.
func tsStringConsts(t *testing.T) tsConsts {
	t.Helper()
	c := tsConsts{byDir: map[string]map[string]string{}, byPkg: map[string]map[string]string{}}
	for path := range tsSources(t) {
		f := tsParse(t, path)
		dir, pkg := filepath.Dir(path), f.Name.Name
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					s, ok := tsLiteral(vs.Values[i])
					if !ok {
						continue
					}
					if c.byDir[dir] == nil {
						c.byDir[dir] = map[string]string{}
					}
					if c.byPkg[pkg] == nil {
						c.byPkg[pkg] = map[string]string{}
					}
					c.byDir[dir][name.Name] = s
					c.byPkg[pkg][name.Name] = s
				}
			}
		}
	}
	require.NotEmpty(t, c.byPkg["cli"])
	return c
}

// tsVerb matches one formatting verb of a format string.
var tsVerb = regexp.MustCompile(`%[-+# 0']*[0-9]*(?:\.[0-9]+)?[a-zA-Z%]`)

// tsRender substitutes the arguments sc knows at compile time into a format
// string and replaces the rest with tsWildcard.
func tsRender(format string, args []ast.Expr, dir string, consts tsConsts) string {
	i := 0
	return tsVerb.ReplaceAllStringFunc(format, func(verb string) string {
		if verb == "%%" {
			return "%"
		}
		var arg ast.Expr
		if i < len(args) {
			arg = args[i]
		}
		i++
		if arg == nil {
			return tsWildcard
		}
		if s, ok := consts.lookup(dir, arg); ok {
			return s
		}
		return tsWildcard
	})
}

// tsCodeMessages returns every message the non-test files of the given
// packages build with errors.New or fmt.Errorf, rendered by tsRender.
func tsCodeMessages(t *testing.T, packages []string) []tsMessage {
	t.Helper()
	consts := tsStringConsts(t)
	var out []tsMessage
	for _, pkg := range packages {
		for _, path := range tsGoFiles(t, pkg) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			require.NoError(t, err, path)
			dir := filepath.Dir(path)
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkgIdent, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				switch {
				case pkgIdent.Name == "errors" && sel.Sel.Name == "New":
				case pkgIdent.Name == "fmt" && (sel.Sel.Name == "Errorf" || sel.Sel.Name == "Sprintf"):
				default:
					return true
				}
				format, ok := tsLiteral(call.Args[0])
				if !ok {
					return true
				}
				if pkgIdent.Name == "fmt" && sel.Sel.Name == "Sprintf" {
					return true // Sprintf builds text, not errors; only Errorf/New are messages
				}
				out = append(out, tsMessage{
					text: tsRender(format, call.Args[1:], dir, consts),
					file: path,
					line: fset.Position(call.Pos()).Line,
				})
				return true
			})
		}
	}
	require.NotEmpty(t, out)
	return out
}

// tsWords matches three or more letters in a row: a message whose static
// text has none says nothing of its own ("%s: %w"), so there is nothing to
// document and nothing to exempt.
var tsWords = regexp.MustCompile(`\p{L}{3,}`)

// tsSaysSomething reports whether a rendered message has words of its own
// outside the verbs.
func tsSaysSomething(text string) bool {
	for _, seg := range strings.Split(text, tsWildcard) {
		if tsWords.MatchString(seg) {
			return true
		}
	}
	return false
}

// tsCovered reports whether a documented key matches the message: the key
// has to sit inside one uninterrupted run of literal text, so a row can
// never claim a phrase a verb splits in two.
func tsCovered(text string, documented map[string]bool) bool {
	for _, seg := range strings.Split(text, tsWildcard) {
		for key := range documented {
			if strings.Contains(seg, key) {
				return true
			}
		}
	}
	return false
}

// tsMessages returns every message documented in docs/troubleshooting.md:
// the first column of each table whose first heading is "Message".
func tsMessages(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(tsDoc)
	require.NoError(t, err, "docs/troubleshooting.md is the P9-08 deliverable")
	var out []string
	inTable := false
	for i, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "|") {
			inTable = false
			continue
		}
		cells := tsCells(trimmed)
		require.NotEmpty(t, cells, "line %d: %s", i+1, line)
		switch {
		case strings.EqualFold(cells[0], "Message"):
			inTable = true
			require.Len(t, cells, 3, "line %d: a message table has three columns", i+1)
			require.Equal(t, []string{"Message", "What happened", "What to do"}, cells, "line %d", i+1)
		case !inTable:
			// A table about something else; ignore it.
		case strings.HasPrefix(cells[0], "---"):
			// The header rule.
		default:
			require.Len(t, cells, 3, "line %d: %s", i+1, line)
			msg := strings.TrimSpace(cells[0])
			require.True(t, strings.HasPrefix(msg, "`") && strings.HasSuffix(msg, "`") && len(msg) > 2,
				"line %d: a message must be the whole first cell in backticks, got %q", i+1, cells[0])
			for _, cell := range cells[1:] {
				require.NotEmpty(t, strings.TrimSpace(cell), "line %d: %s has an empty column", i+1, msg)
			}
			out = append(out, strings.Trim(msg, "`"))
		}
	}
	require.NotEmpty(t, out, "no messages documented")
	return out
}

// tsDocumented is tsMessages as a set.
func tsDocumented(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, msg := range tsMessages(t) {
		out[msg] = true
	}
	return out
}

// tsCells splits a markdown table row into its cells.
func tsCells(line string) []string {
	line = strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(line), "|"), "|")
	parts := strings.Split(line, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// tsSources reads every non-test .go file of the module once.
func tsSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	root := filepath.Join("..", "..")
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path) //nolint:gosec // test reading the module's own source
			if err != nil {
				return err
			}
			out[path] = string(b)
			return nil
		})
		require.NoError(t, err)
	}
	require.NotEmpty(t, out)
	return out
}

// TestTroubleshootingMessagesStillExistInTheCode is the P9-08 acceptance
// test: every message the doc is keyed by must still be somewhere in the
// source, so a reworded error breaks the page that documents it instead of
// leaving a user searching for a string that no longer exists.
func TestTroubleshootingMessagesStillExistInTheCode(t *testing.T) {
	sources := tsSources(t)
	// A key is usually literal source text. It may instead be text only the
	// running command has — "--model requires --add NAME" is built from
	// "--%s requires --%s NAME" and two flag constants — so the rendered
	// messages count too: what matters is that a user can read it.
	rendered := tsCodeMessages(t, tsMessagePackages)
	seen := map[string]bool{}
	for _, msg := range tsMessages(t) {
		require.False(t, seen[msg], "documented twice: %q", msg)
		seen[msg] = true
		found := ""
		for path, src := range sources {
			if strings.Contains(src, msg) {
				found = path
				break
			}
		}
		if found == "" {
			for _, m := range rendered {
				if tsCovered(m.text, map[string]bool{msg: true}) {
					found = m.file
					break
				}
			}
		}
		require.NotEmpty(t, found, "docs/troubleshooting.md documents %q, which nothing in the code says "+
			"any more; reword the page with the message, or drop the row", msg)
	}
}

// TestTroubleshootingCoversEveryUserFacingSentinel is the other direction:
// a new sentinel error in a package a user can reach must be documented (or
// exempted with a reason), which is what makes the page complete rather
// than a sample.
func TestTroubleshootingCoversEveryUserFacingSentinel(t *testing.T) {
	documented := tsDocumented(t)
	for _, s := range tsSentinels(t) {
		if _, ok := tsExempt[s.key()]; ok {
			require.False(t, documented[s.msg], "%s is both exempt and documented: drop the exemption", s.key())
			continue
		}
		require.True(t, documented[s.msg],
			"%s (%s) is user-facing and undocumented: add %q to docs/troubleshooting.md, "+
				"or add it to tsExempt with the reason it can never reach a user", s.key(), s.file, s.msg)
	}
}

// tsUndocumented returns every message the command layer builds that no
// row of the page matches: the ones that need either a row or an entry in
// tsExemptMessages. A message whose static text is only punctuation ("%s:
// %w") is not one of them — the wrapped error is what a user reads — and
// neither is an exempted sentinel, whose reason is in tsExempt.
func tsUndocumented(t *testing.T) map[string]tsMessage {
	t.Helper()
	documented := tsDocumented(t)
	exemptText := map[string]bool{}
	for _, s := range tsSentinels(t) {
		if _, ok := tsExempt[s.key()]; ok {
			exemptText[s.msg] = true
		}
	}
	out := map[string]tsMessage{}
	for _, m := range tsCodeMessages(t, tsMessagePackages) {
		if !tsSaysSomething(m.text) || exemptText[m.text] || tsCovered(m.text, documented) {
			continue
		}
		out[m.display()] = m
	}
	return out
}

// TestTroubleshootingCoversEveryMessageTheCommandLayerBuilds is the
// direction the sentinel test cannot reach: most of what a user reads is a
// fmt.Errorf, not an errors.New, and a sentinel that is only an errors.Is
// target is documented text nothing prints. Every message internal/cli and
// internal/config build has to be matched by a row here (or exempted with a
// reason), so `sc logs`' refusals cannot go undocumented the way they did
// before this test existed.
func TestTroubleshootingCoversEveryMessageTheCommandLayerBuilds(t *testing.T) {
	var missing []string
	for text, m := range tsUndocumented(t) {
		if reason := tsExemptMessages[text]; reason != "" {
			continue
		}
		missing = append(missing, fmt.Sprintf("%s:%d %q", m.file, m.line, text))
	}
	sort.Strings(missing)
	require.Empty(t, missing,
		"docs/troubleshooting.md matches no row for these messages: add a row keyed by a phrase of one "+
			"that no verb interrupts, or add it to tsExemptMessages with the reason a user never has to look it up")
}

// TestTroubleshootingExemptionsAreReal keeps both exemption lists from
// outliving the errors they excuse.
func TestTroubleshootingExemptionsAreReal(t *testing.T) {
	live := map[string]bool{}
	for _, s := range tsSentinels(t) {
		live[s.key()] = true
	}
	for key, reason := range tsExempt {
		require.True(t, live[key], "tsExempt names %s, which no longer exists: drop the entry", key)
		require.NotEmpty(t, reason, "%s needs a reason", key)
	}
	undocumented := tsUndocumented(t)
	for text, reason := range tsExemptMessages {
		require.Contains(t, undocumented, text,
			"tsExemptMessages names %q, which nothing builds any more, or which a row of the page now "+
				"documents: drop the entry", text)
		require.NotEmpty(t, reason, "%q needs a reason", text)
	}
}

// TestTroubleshootingLinksAreLocalFiles keeps the page's cross references
// pointing at documents that exist.
func TestTroubleshootingLinksAreLocalFiles(t *testing.T) {
	data, err := os.ReadFile(tsDoc)
	require.NoError(t, err)
	for _, part := range strings.Split(string(data), "](") {
		link, _, ok := strings.Cut(part, ")")
		if !ok || strings.HasPrefix(link, "http") || strings.HasPrefix(link, "#") {
			continue
		}
		link, _, _ = strings.Cut(link, "#")
		_, err := os.Stat(filepath.Join("..", "..", "docs", link))
		require.NoError(t, err, "docs/troubleshooting.md links to %s", link)
	}
}

// TestTroubleshootingIsReachableFromTheReadme is the other half of the
// links test: a page nothing points at is a page nobody finds, whatever it
// says. The README is where a user starts.
func TestTroubleshootingIsReachableFromTheReadme(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	require.NoError(t, err)
	require.Contains(t, string(data), "docs/troubleshooting.md",
		"nothing links to the troubleshooting page: link it from the README")
}

// tsAgentLogsAreWritten reports whether any non-test file outside
// internal/logging calls Agent(…) on a run logger, which is the only thing
// that writes runs/<run-id>/agents/<id>.log. When P9-08 audited the page,
// logging.RunLogger.Agent had that writer and no caller at all (P9-18), and
// four rows still told a user to read the file it would have written.
func tsAgentLogsAreWritten(t *testing.T) bool {
	t.Helper()
	for path, src := range tsSources(t) {
		if strings.Contains(path, filepath.Join("internal", "logging")) {
			continue
		}
		if !strings.Contains(src, `"slop-cannon/internal/logging"`) {
			continue
		}
		found := false
		ast.Inspect(tsParse(t, path), func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Agent" {
				return true
			}
			if recv, ok := sel.X.(*ast.Ident); ok && recv.Name == "domain" {
				return true // domain.Agent(nil), a conversion
			}
			found = true
			return false
		})
		if found {
			return true
		}
	}
	return false
}

// tsAdvice is what the page tells a user to do, line by line: every "What
// to do" cell, plus the prose between the tables. The "Message" and "What
// happened" columns are left out — a message may well be about the agent
// logs; a remedy may not send anyone to one.
func tsAdvice(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(tsDoc)
	require.NoError(t, err)
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "|") {
			out = append(out, line)
			continue
		}
		if cells := tsCells(trimmed); len(cells) == 3 {
			out = append(out, cells[2])
		}
	}
	return out
}

// TestTroubleshootingSendsNobodyToAFileNothingWrites: a remedy that says
// "read the agent log" cannot be followed while nothing writes one, and the
// page's prose may not claim the file exists either. Both may name the
// per-agent logs, as they do today, exactly as long as something writes
// them; the day that writer goes away this fails instead of the user.
func TestTroubleshootingSendsNobodyToAFileNothingWrites(t *testing.T) {
	if tsAgentLogsAreWritten(t) {
		return
	}
	for _, line := range tsAdvice(t) {
		for _, phrase := range []string{"agents/", "the agent log", "an agent log"} {
			require.NotContains(t, line, phrase,
				"nothing calls logging.RunLogger.Agent, so runs/<run-id>/agents/ stays empty (P9-18): "+
					"this remedy must name sc.log, `sc logs` or events.jsonl instead")
		}
	}
}
