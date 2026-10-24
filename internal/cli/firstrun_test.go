package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
	"slop-cannon/internal/orchestrator"
	"slop-cannon/internal/testutil"
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
	// And the missing cartridge is on the list even though nothing about
	// this failure is about cartridges: it is a doctor warning that refuses
	// every run (D-04), so a user who fixes the three steps above and
	// nothing else would be turned away again on the next shot.
	require.Contains(t, out, "4. cartridge: no cartridge configured")
	require.Contains(t, out, CartridgeAddHint)
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

// frFixedDoctor makes the advice use a report with nothing wrong in it, so
// a test sees the class's own step and nothing else.
func frFixedDoctor(t *testing.T) {
	t.Helper()
	prev := firstRunDoctor
	t.Cleanup(func() { firstRunDoctor = prev })
	firstRunDoctor = func(context.Context, string) *DoctorReport {
		return &DoctorReport{OK: true, Checks: []DoctorCheck{{Name: "git", Status: DoctorPass, Detail: "fine"}}}
	}
}

// frLockedError builds the refusal orchestrator.Run.Start raises when
// another cannon holds the repository's lock.
func frLockedError(state orchestrator.LockState) *orchestrator.LockedError {
	return &orchestrator.LockedError{
		Path:  "/state/run.lock",
		State: state,
		Record: orchestrator.LockRecord{LockHolder: orchestrator.LockHolder{
			Run: "20260101-000000-aaaa", PID: 4242, Host: "elsewhere",
			TakenAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		}},
	}
}

// TestStartupLockAdviceAddsOnlyWhatTheErrorDidNotSay: a LockedError already
// names the live run, points at `sc status` and says to wait for it or stop
// it, so the step under "Fix it in this order:" must carry the one thing it
// does not — what --force is for — and nothing else. When the error says
// that too (the lock written on another machine), the class has nothing to
// add and prints no step at all.
func TestStartupLockAdviceAddsOnlyWhatTheErrorDidNotSay(t *testing.T) {
	frFixedDoctor(t)

	live := frLockedError(orchestrator.LockLive)
	require.ErrorIs(t, live, orchestrator.ErrLocked)
	a := AdviseStartup(context.Background(), "/repo", live)
	require.Len(t, a.Steps, 1)
	require.Equal(t, startupFixLock, a.Steps[0].Fix)
	out := frRenderAdvice(t, a)
	require.Equal(t, 1, strings.Count(out, "sc status"),
		"the reason already sent the user to sc status; the step must not send them again:\n%s", out)
	require.Equal(t, 1, strings.Count(out, "wait for it"), out)
	require.Equal(t, 1, strings.Count(out, "--force"), "and --force is the step's own news:\n%s", out)

	// The lock written on another machine names --force itself.
	away := frLockedError(orchestrator.LockElsewhere)
	require.Contains(t, away.Error(), "--force")
	a = AdviseStartup(context.Background(), "/repo", away)
	require.Empty(t, a.Steps, "the error said all of it; a numbered step would only repeat it")
	out = frRenderAdvice(t, a)
	require.NotContains(t, out, "Fix it in this order:")
	require.Equal(t, 1, strings.Count(out, "--force"), out)
	require.Contains(t, out, "Then check everything at once with: sc doctor")
}

// TestStartupAdviceDropsAStepTheErrorAlreadyGave is the same rule one level
// down, and it looks at the fix rather than only at the problem: a class
// step exists to add the remedy, so when the error's own message carries
// that remedy word for word there is no step at all.
func TestStartupAdviceDropsAStepTheErrorAlreadyGave(t *testing.T) {
	class := startupClass{name: "demo", problem: "something is wrong", fix: "run: sc doctor --fix"}

	_, ok := class.step(errors.New("it broke; run: sc doctor --fix and try again"))
	require.False(t, ok, "the error already gave that instruction")

	step, ok := class.step(errors.New("it broke"))
	require.True(t, ok)
	require.Equal(t, "run: sc doctor --fix", step.Fix)
	require.Equal(t, "something is wrong", step.Problem)

	// A problem the reason already spelled out leaves the check name to
	// lead the step, and the fix keeps its line.
	step, ok = class.step(errors.New("something is wrong here"))
	require.True(t, ok)
	require.Empty(t, step.Problem)
	require.Equal(t, "demo", step.title())

	// The real case: the refusal a user actually sees carries the add hint
	// in its own message, so the class adds no step of its own.
	noCart := fmt.Errorf("%w — add one with: %s", config.ErrNoCartridge, CartridgeAddHint)
	cart := startupClassOf(noCart)
	require.Equal(t, "cartridge", cart.name)
	_, ok = cart.step(noCart)
	require.False(t, ok, "the reason above the list already tells them to run it")

	// The bare sentinel does not carry it, and then the step is worth its
	// line.
	step, ok = cart.step(config.ErrNoCartridge)
	require.True(t, ok)
	require.Equal(t, "add one with: "+CartridgeAddHint, step.Fix)
}

// TestStartupAdviceListsAMissingCartridgeWhateverFailedFirst is the fix
// list's promise to the user it exists for. sc doctor warns rather than
// fails when no cartridge is configured, but no run can fire an agent
// without one (D-04): a brand-new user who is refused for another reason
// must be told about it here, or they do everything the list says and are
// refused again on the next shot.
func TestStartupAdviceListsAMissingCartridgeWhateverFailedFirst(t *testing.T) {
	report := &DoctorReport{
		Repo: "/repo",
		Checks: []DoctorCheck{
			{Name: "repo", Status: DoctorWarn, Detail: "not a git repository: /repo", Hint: doctorHintRepo},
			{Name: "cartridge", Status: DoctorWarn, Detail: "no cartridge configured", Hint: doctorHintNoCart},
			{Name: "gh", Status: DoctorWarn, Detail: "not found on PATH", Hint: "install the GitHub CLI"},
			{Name: ".beads", Status: DoctorWarn, Detail: "not initialised", Hint: "sc will run bd init"},
		},
	}
	prev := firstRunDoctor
	t.Cleanup(func() { firstRunDoctor = prev })
	firstRunDoctor = func(context.Context, string) *DoctorReport { return report }

	a := AdviseStartup(context.Background(), "/repo", fmt.Errorf("wrapped: %w", gitx.ErrNotRepository))
	require.Equal(t, []StartupStep{
		{Check: "repo", Problem: "not a git repository: /repo", Fix: doctorHintRepo},
		{Check: "cartridge", Problem: "no cartridge configured", Fix: doctorHintNoCart},
	}, a.Steps, "the failure leads and the blocker follows; the warnings that stop nothing stay out")

	out := frRenderAdvice(t, a)
	require.Contains(t, out, "2. cartridge: no cartridge configured")
	require.Contains(t, out, CartridgeAddHint)
	require.NotContains(t, out, "GitHub CLI", "gh stops no run: safety off still fires (D-10)")
	require.NotContains(t, out, ".beads", "sc creates the bead tracker on the first run (D-13)")

	// "no active cartridge" is the same blocker under its other spelling.
	report.Checks[1] = DoctorCheck{Name: "cartridge", Status: DoctorWarn, Detail: "no active cartridge", Hint: doctorHintNoActive}
	a = AdviseStartup(context.Background(), "/repo", fmt.Errorf("wrapped: %w", gitx.ErrNotRepository))
	require.Len(t, a.Steps, 2)
	require.Equal(t, "no active cartridge", a.Steps[1].Problem)
}

// TestStartupAdviceKnowsEveryClass pins the classification: each of these
// errors must reach its own class, so the fix a user is given matches the
// failure they hit.
func TestStartupAdviceKnowsEveryClass(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
		// mute is set for a sentinel that carries its own remedy, so the
		// class deliberately adds no step of its own: the reason line above
		// the list already told the user what to run.
		mute bool
	}{
		{"lock", orchestrator.ErrLocked, "lock", false},
		{"not a repo", gitx.ErrNotRepository, "repo", false},
		{"no cartridge", config.ErrNoCartridge, "cartridge", false},
		{"cartridge not built", cartridge.ErrCartridgeNotBuilt, "cartridge", false},
		{"not authenticated", cartridge.ErrAuth, "cartridge", false},
		{"credits", cartridge.ErrCreditsExhausted, "cartridge", false},
		{"bd missing", &beads.CheckError{Command: "bd version", Reason: beads.ReasonNotFound}, "bd", false},
		{"no beads project", beads.ErrNoProject, "bd", false},
		{"gh", gitx.ErrGHUnauthenticated, "gh", false},
		{"dirty tree", gitx.ErrRepoDirty, "repo", false},
		{"bad config", &config.ParseError{Path: "/c.toml", Line: 3, Msg: "bad"}, "config", false},
		{"ammo", config.ErrAmmoTooLow, "ammo", false},
		{"terminal", errors.New("the TUI needs a terminal: stdin and stdout are not a TTY"), "terminal", false},
		{"unknown", errors.New("something else entirely"), "", false},
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
			if tc.mute {
				require.False(t, ok, "the error's own message is the fix; a step would repeat it")
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

// frWritePrompt files a debug copy of one agent's prompt under the run
// directory, the way a firing does (prompt.Prompt.Write). EnsureRunDir does
// not create prompts/, so this is what makes the row real.
func frWritePrompt(t *testing.T, repo string, id domain.RunID, agent domain.AgentID) string {
	t.Helper()
	path, err := prompt.WriteFile(config.RunDir(repo, id.String()), agent, "# prompt\n")
	require.NoError(t, err)
	return path
}

func TestFirstRunNoticeGolden(t *testing.T) {
	home := doctorEnv(t)
	repo := doctorRepoDir(t, true, false)
	id := domain.RunID("20260101-120000-abcd")
	frMakeRunDir(t, repo, id)
	frWritePrompt(t, repo, id, "po-plan-1")

	var buf bytes.Buffer
	require.NoError(t, ReportFirstRun(&buf, frRun(repo, id), events.FinishCompleted))
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

	frMakeRunDir(t, repo, first)
	require.True(t, FirstRunInRepo(repo))

	var buf bytes.Buffer
	require.NoError(t, ReportFirstRun(&buf, frRun(repo, first), events.FinishCompleted))
	require.Contains(t, buf.String(), "That was the first sc run in")

	frMakeRunDir(t, repo, second)
	require.False(t, FirstRunInRepo(repo), "the notice has been paid for this repository")

	buf.Reset()
	require.NoError(t, ReportFirstRun(&buf, frRun(repo, second), events.FinishCompleted))
	require.Empty(t, buf.String())
}

// TestFirstRunNoticeSurvivesARunThatWasKilled is the crash half of the same
// question, and the reason the marker is the only criterion. A run that is
// killed outright (kill -9, a power cut, the OOM killer — P9-03 makes that a
// supported path) leaves its run directory behind and no marker. Counting
// run directories would see two from then on and stay silent for the life of
// the repository, so the first run that actually succeeds would never be
// told where anything lives.
func TestFirstRunNoticeSurvivesARunThatWasKilled(t *testing.T) {
	doctorEnv(t)
	repo := doctorRepoDir(t, true, false)
	killed := domain.RunID("20260101-120000-aaaa")
	next := domain.RunID("20260102-120000-bbbb")

	// The killed run: a run directory, no notice, no marker.
	frMakeRunDir(t, repo, killed)
	require.NoFileExists(t, firstRunNoticePath(repo))

	// The next run finds two run directories and is still owed the notice.
	frMakeRunDir(t, repo, next)
	entries, err := os.ReadDir(filepath.Join(config.StateDir(repo), config.RunsDirName))
	require.NoError(t, err)
	require.Len(t, entries, 2, "the killed run left its directory behind")
	require.True(t, FirstRunInRepo(repo), "a crashed run must not cost the repository its notice")

	var buf bytes.Buffer
	require.NoError(t, ReportFirstRun(&buf, frRun(repo, next), events.FinishCompleted))
	require.Contains(t, buf.String(), "That was the first sc run in")
	require.FileExists(t, firstRunNoticePath(repo))
}

// TestFirstRunNoticeWaitsForARunThatSucceeds is the task's "on first
// successful run": a run that was stopped, failed or ran out of credits
// prints nothing and — the point — does not spend the notice, so the run
// that finally completes still gets it.
func TestFirstRunNoticeWaitsForARunThatSucceeds(t *testing.T) {
	doctorEnv(t)
	repo := doctorRepoDir(t, true, false)
	id := domain.RunID("20260101-120000-abcd")
	frMakeRunDir(t, repo, id)

	for _, reason := range []events.FinishReason{
		events.FinishStopped, events.FinishFailed, events.FinishCreditsExhausted, events.FinishReason(""),
	} {
		var buf bytes.Buffer
		require.NoError(t, ReportFirstRun(&buf, frRun(repo, id), reason))
		require.Empty(t, buf.String(), "a run that finished %q is not the first successful run", reason)
		require.NoFileExists(t, firstRunNoticePath(repo), "and it does not burn the marker")
		require.False(t, FirstRunNoticeEarned(reason))
	}

	require.True(t, FirstRunNoticeEarned(events.FinishCompleted))
	var buf bytes.Buffer
	require.NoError(t, ReportFirstRun(&buf, frRun(repo, id), events.FinishCompleted))
	require.Contains(t, buf.String(), "That was the first sc run in")
}

// TestFirstRunNoticeCoversEveryPlaceARunWrites: every path in the notice is
// one the run really uses, and all of them are under the run directory.
func TestFirstRunNoticeCoversEveryPlaceARunWrites(t *testing.T) {
	doctorEnv(t)
	repo := doctorRepoDir(t, true, false)
	id := domain.RunID("20260101-120000-abcd")
	dirs, err := orchestrator.EnsureRunDir(config.RunDir(repo, id.String()))
	require.NoError(t, err)
	promptPath := frWritePrompt(t, repo, id, "po-plan-1")

	n := NewFirstRunNotice(frRun(repo, id))
	require.Equal(t, dirs.Dir, n.Dir)
	require.Equal(t, dirs.Agents, n.Agents)
	require.Equal(t, dirs.Worktrees, n.Worktrees)
	require.Equal(t, orchestrator.EventLogPath(dirs.Dir), n.Events)
	require.Equal(t, config.GlobalPath(), n.Config)
	require.Equal(t, filepath.Dir(promptPath), n.Prompts, "the prompts row is the directory the firings write to")
	for _, p := range []string{n.Log, n.Agents, n.Prompts, n.Events, n.Worktrees} {
		require.True(t, strings.HasPrefix(p, dirs.Dir+string(filepath.Separator)), p)
		require.False(t, strings.HasPrefix(p, repo+string(filepath.Separator)), "D-23: not inside the repository: "+p)
	}
	// Every directory the notice names exists: a row is an invitation to
	// open something, and the run directory is created without prompts/.
	for _, d := range []string{n.Agents, n.Prompts, n.Worktrees} {
		require.DirExists(t, d)
	}
}

// TestFirstRunNoticeLeavesOutAPromptsDirectoryThatIsNotThere is the other
// side of it: EnsureRunDir does not create prompts/, and only a firing that
// files its debug copy does (P5-13 is the open task that makes every firing
// leave one). A run that ends before any firing writes one must not name a
// directory that is not there.
func TestFirstRunNoticeLeavesOutAPromptsDirectoryThatIsNotThere(t *testing.T) {
	doctorEnv(t)
	repo := doctorRepoDir(t, true, false)
	id := domain.RunID("20260101-120000-abcd")
	dirs, err := orchestrator.EnsureRunDir(config.RunDir(repo, id.String()))
	require.NoError(t, err)
	require.NoDirExists(t, prompt.Dir(dirs.Dir), "EnsureRunDir does not create prompts/")

	n := NewFirstRunNotice(frRun(repo, id))
	require.Empty(t, n.Prompts)
	var buf bytes.Buffer
	require.NoError(t, WriteFirstRunNotice(&buf, n))
	require.NotContains(t, buf.String(), "prompts")

	// One filed prompt is all it takes for the row to come back.
	frWritePrompt(t, repo, id, "po-plan-1")
	buf.Reset()
	require.NoError(t, WriteFirstRunNotice(&buf, NewFirstRunNotice(frRun(repo, id))))
	require.Contains(t, buf.String(), "prompts     "+prompt.Dir(dirs.Dir))
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
	require.NoError(t, ReportFirstRun(&first, frRun(repo, id), events.FinishCompleted))
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
	require.NoError(t, ReportFirstRun(&again, frRun(repo, id), events.FinishCompleted))
	require.Empty(t, again.String(), "a resumed first run does not print the notice twice")
	require.False(t, FirstRunInRepo(repo))

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
	require.NoError(t, ReportFirstRun(&buf, frRun(repo, id), events.FinishCompleted))
	require.NotEmpty(t, buf.String())

	require.NoError(t, os.Remove(firstRunNoticePath(repo)))
	buf.Reset()
	require.NoError(t, ReportFirstRun(&buf, frRun(repo, id), events.FinishCompleted))
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

// frNoticeRows returns the label/path rows of the notice printed in out,
// exactly as a reader sees them.
func frNoticeRows(t *testing.T, out string) map[string]string {
	t.Helper()
	rows := map[string]string{}
	lines := strings.Split(out, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "That was the first sc run in ") {
			start = i
			break
		}
	}
	require.GreaterOrEqual(t, start, 0, "no first-run notice in:\n%s", out)
	for _, l := range lines[start:] {
		if !strings.HasPrefix(l, "  ") {
			continue
		}
		fields := strings.Fields(l)
		if len(fields) < 2 || !filepath.IsAbs(fields[len(fields)-1]) {
			continue
		}
		label := strings.Join(fields[:len(fields)-1], " ")
		rows[label] = fields[len(fields)-1]
	}
	require.NotEmpty(t, rows)
	return rows
}

// TestRunNoticeNamesOnlyPathsTheRunReallyWrote opens everything the notice
// tells a first-time user to look at, after a real run of the production
// path. A row is an invitation, so a path that is not there — the prompts
// directory EnsureRunDir never creates among them — must not be printed.
func TestRunNoticeNamesOnlyPathsTheRunReallyWrote(t *testing.T) {
	env := runFireSetup(t, false)

	_, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief, "--headless", "--cartridge", "mock")
	require.Equal(t, 0, code, stderr)

	rows := frNoticeRows(t, stderr)
	for label, path := range rows {
		_, err := os.Stat(path)
		require.NoErrorf(t, err, "the notice's %q row names %s, which nothing wrote", label, path)
	}
	for _, label := range []string{"run log", "agent logs", "events", "worktrees", "config"} {
		require.Contains(t, rows, label)
	}

	// The prompts row is printed because this run filed prompts, and every
	// file in it belongs to an agent the run fired. Since P5-13 the firing
	// path files a copy for every agent, not only the planning one, so the
	// two directories hold the same names.
	dir := config.RunDir(env.Repo.Dir, runLatestID(t, env.Repo.Dir).String())
	require.Equal(t, prompt.Dir(dir), rows["prompts"])
	prompts, err := os.ReadDir(rows["prompts"])
	require.NoError(t, err)
	require.NotEmpty(t, prompts, "a run that fired agents filed at least one prompt")
	agents, err := os.ReadDir(rows["agent logs"])
	require.NoError(t, err)
	fired := map[string]bool{}
	for _, a := range agents {
		fired[strings.TrimSuffix(a.Name(), filepath.Ext(a.Name()))] = true
	}
	filed := map[string]bool{}
	for _, p := range prompts {
		name := strings.TrimSuffix(p.Name(), prompt.PromptFileExt)
		require.Truef(t, fired[name], "prompts/%s belongs to no agent this run fired", p.Name())
		filed[name] = true
		info, err := p.Info()
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "a prompt quotes the user's rules and vibes")
	}
	// P5-13: every firing files the prompt it sent, not only the planning
	// one, so an agent with a log has a prompt beside it.
	for name := range fired {
		require.Truef(t, filed[name], "agent %s has a log but no prompt under prompts/", name)
	}
	require.Greater(t, len(filed), 1, "a real run fires more than the product owner")
}

// TestRunLockRefusalFromStartPrintsTheFixList closes the window the
// preflight cannot: fireCannon checks the repository's run lock before it
// builds anything, and orchestrator.Run.Start takes that lock again when
// the run begins, so a second sc that arrives in between is refused by
// Start. That refusal is the startup failure P8-06 exists for, and it must
// print the numbered advice rather than a bare error line.
//
// The window is opened deterministically: the `bd` the assembly runs plants
// a live lock the first time it is called, which is after the preflight and
// before Start.
func TestRunLockRefusalFromStartPrintsTheFixList(t *testing.T) {
	env := runFireSetup(t, false)

	// A lock held by a live process that is not this one.
	held := exec.Command("sleep", "60")
	require.NoError(t, held.Start())
	t.Cleanup(func() { _ = held.Process.Kill(); _, _ = held.Process.Wait() })

	host, err := os.Hostname()
	require.NoError(t, err)
	rec, err := json.Marshal(orchestrator.LockHolder{
		Run: "20260101-000000-aaaa", PID: held.Process.Pid, Host: host, TakenAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	record := filepath.Join(t.TempDir(), "run.lock")
	require.NoError(t, os.WriteFile(record, rec, 0o600))

	lock := orchestrator.RepoLockPath(config.StateDir(env.Repo.Dir))
	testutil.FakeBin(t, "bd", fmt.Sprintf(`if [ ! -f %[1]q ]; then
  mkdir -p %[2]q
  cp %[3]q %[1]q
fi
exec %[4]q "$@"
`, lock, filepath.Dir(lock), record, env.BD.Path))

	_, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief, "--headless", "--cartridge", "mock")
	require.Equal(t, 1, code, stderr)
	require.FileExists(t, lock, "a refused run leaves the other run's lock alone")

	// The lock was planted during assembly, so this refusal came out of
	// orchestrator.Run.Start — whose own failure path logs this line —
	// rather than out of the preflight check, which had already passed.
	require.Contains(t, stderr, "run failed to start",
		"the refusal came from the preflight, not from Start; this test proves nothing")

	require.Contains(t, stderr, "sc could not start a run in "+env.Repo.Dir)
	require.Contains(t, stderr, "20260101-000000-aaaa")
	require.Contains(t, stderr, "Fix it in this order:")
	require.Contains(t, stderr, "--force")
	require.Contains(t, stderr, "Then check everything at once with: sc doctor")
	require.NotContains(t, stderr, "sc: orchestrator: another run holds the lock",
		"the bare error line is what the fix list replaces")
	require.NotContains(t, stderr, "That was the first sc run in",
		"a run that never started says nothing about where it would have written")
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
// reads the files a user edits by hand. The deeper packages have a guard of
// their own over the same ground, with its own exemption list: see
// TestTroubleshootingCoversEveryMessageTheWrappedPackagesBuild (P9-19).
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
	"gitx.ErrNoHeadCommit":                "a merge guard: the gate poller always passes the head it read; only a programming error trips it",
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
	// messages, warnings and doctor details count too: what matters is that
	// a user can read it.
	rendered := tsCodeMessages(t, tsMessagePackages)
	// P9-19 put the packages underneath in the net as well, so a row may be
	// keyed on a phrase only their rendered text has: "after setting
	// status.custom=" is built from a format and a string constant in
	// internal/beads.
	rendered = append(rendered, tsCodeMessages(t, tsWrappedPackages)...)
	rendered = append(rendered, tsWarnings(t)...)
	rendered = append(rendered, tsDetails(t)...)
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

// --- Warnings (P9-08 round 4) -------------------------------------------
//
// A warning is the other half of what sc prints. It is not an error — the
// run carries on — but it is what a user reads when a bead stops being
// worked on, when a plan is dropped, or when part of the run degrades, and
// it reaches sc.log, events.jsonl, the TUI and `sc status` alike. Warnings
// are built with fmt.Sprintf rather than fmt.Errorf, so the message guard
// above cannot see them; these two helpers and their test hold them to a
// row each, the same way.

// tsWarnFuncs are the functions that raise a warning, and the index of the
// argument that carries its message. -1 means the signature varies
// (slog's Warn takes the message first, Run.Warn takes a source first), so
// the message is the first argument that renders to words.
var tsWarnFuncs = map[string]int{
	"Warn":      -1,
	"warn":      -1,
	"warnOnce":  2,
	"warnAbout": 3,
}

// tsStringy renders an expression to the text it produces, as far as the
// source allows, and reports whether it produces text at all: a string
// literal, a concatenation involving one, a package-level string constant,
// or a fmt.Sprintf. Anything else — a bead id, an error, a struct field —
// is not text, and saying so is what keeps a warning's message argument
// from being confused with the bead it is about.
func tsStringy(dir string, consts tsConsts, expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		return tsLiteral(e)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		l, lok := tsStringy(dir, consts, e.X)
		r, rok := tsStringy(dir, consts, e.Y)
		if !lok && !rok {
			return "", false
		}
		if !lok {
			l = tsWildcard
		}
		if !rok {
			r = tsWildcard
		}
		return l + r, true
	case *ast.Ident, *ast.SelectorExpr:
		return consts.lookup(dir, expr)
	case *ast.CallExpr:
		sel, ok := e.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Sprintf" || len(e.Args) == 0 {
			return "", false
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "fmt" {
			return "", false
		}
		format, ok := tsLiteral(e.Args[0])
		if !ok {
			return "", false
		}
		return tsRender(format, e.Args[1:], dir, consts), true
	default:
		return "", false
	}
}

// tsLocals maps each local variable of a function body to the texts it is
// assigned, so a warning whose message is built a line above the call —
// `msg := fmt.Sprintf(…)`, then `msg += " (…)"` — is read rather than
// missed. Both halves count as messages: both reach the user.
func tsLocals(dir string, consts tsConsts, returns tsReturnTexts, fn ast.Node) map[string][]string {
	out := map[string][]string{}
	ast.Inspect(fn, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		if as.Tok != token.ASSIGN && as.Tok != token.DEFINE && as.Tok != token.ADD_ASSIGN {
			return true
		}
		if len(as.Rhs) == 1 && len(as.Lhs) >= 1 {
			// `b.reason, b.banner = b.describeLocked()`: the words are in
			// the callee's return statements (P9-30). One hop, no further.
			tsFoldReturns(dir, out, returns, as)
		}
		if len(as.Lhs) != len(as.Rhs) {
			return true
		}
		for i, lhs := range as.Lhs {
			name := tsAssignKey(lhs)
			if name == "" || name == "_" {
				continue
			}
			if text, ok := tsStringy(dir, consts, as.Rhs[i]); ok {
				out[name] = append(out[name], text)
				continue
			}
			// `reason, banner := b.reason, b.banner`: a field this same
			// function filled from a callee a moment ago.
			if from := tsAssignKey(as.Rhs[i]); from != "" {
				out[name] = append(out[name], out[from]...)
			}
		}
		return true
	})
	return out
}

// tsAssignKey names the thing an assignment writes to or reads from: a
// local (`banner`) or a field of the receiver (`b.banner`). Both are one
// place a warning's words can sit between being built and being warned.
func tsAssignKey(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	default:
		return ""
	}
}

// tsFoldReturns resolves `a, b := f()` (and `a := f()`) to the texts f's
// return statements put in each position.
func tsFoldReturns(dir string, out map[string][]string, returns tsReturnTexts, as *ast.AssignStmt) {
	call, ok := as.Rhs[0].(*ast.CallExpr)
	if !ok {
		return
	}
	byPos, ok := returns[tsCallKey(dir, call)]
	if !ok {
		return
	}
	for i, lhs := range as.Lhs {
		key := tsAssignKey(lhs)
		if key == "" || key == "_" {
			continue
		}
		out[key] = append(out[key], byPos[i]...)
	}
}

// tsReturnTexts is, for a function name, the texts each of its result
// positions is ever returned with.
type tsReturnTexts map[string]map[int][]string

// tsCollectReturnTexts indexes what every function in the scanned packages
// returns, so a warning whose text is built in one function, handed back to
// another and warned there is read rather than missed (P9-30). The budget
// banner — built in RunBudget.describeLocked, warned in RunBudget.fire — is
// the live example.
func tsCollectReturnTexts(t *testing.T, consts tsConsts) tsReturnTexts {
	t.Helper()
	out := tsReturnTexts{}
	seen := map[string]bool{}
	for _, pkg := range tsPackages {
		for _, path := range tsGoFiles(t, pkg) {
			if seen[path] {
				continue
			}
			seen[path] = true
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			require.NoError(t, err, path)
			dir := filepath.Dir(path)
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				// Locals of the callee itself, with no return index of
				// their own: one hop only.
				locals := tsLocals(dir, consts, nil, fn.Body)
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					ret, ok := n.(*ast.ReturnStmt)
					if !ok {
						return true
					}
					for i, res := range ret.Results {
						for _, text := range tsWarnTexts(dir, consts, locals, nil, res) {
							if !tsSaysSomething(text) {
								continue
							}
							key := tsFuncKey(dir, fn)
							if out[key] == nil {
								out[key] = map[int][]string{}
							}
							out[key][i] = append(out[key][i], text)
						}
					}
					return true
				})
			}
		}
	}
	return out
}

// tsWarnTexts renders one argument of a warning call to the texts it can
// carry: its own, those of the local it names, or — one level up (P9-30) —
// those every caller passes for the parameter it names.
func tsWarnTexts(dir string, consts tsConsts, locals, params map[string][]string, arg ast.Expr) []string {
	if id, ok := arg.(*ast.Ident); ok {
		if texts := locals[id.Name]; len(texts) > 0 {
			return texts
		}
		if texts := params[id.Name]; len(texts) > 0 {
			return texts
		}
	}
	// `what + ": " + next`, where the halves are locals of the function
	// that builds the sentence. tsStringy would render both identifiers as
	// wildcards and hand back a sentence with no words in it, so the
	// concatenation is resolved a side at a time first (P9-30).
	if bin, ok := arg.(*ast.BinaryExpr); ok && bin.Op == token.ADD {
		l := tsWarnTexts(dir, consts, locals, params, bin.X)
		r := tsWarnTexts(dir, consts, locals, params, bin.Y)
		if len(l) > 0 || len(r) > 0 {
			if len(l) == 0 {
				l = []string{tsWildcard}
			}
			if len(r) == 0 {
				r = []string{tsWildcard}
			}
			var out []string
			for _, a := range l {
				for _, b := range r {
					out = append(out, a+b)
				}
			}
			return out
		}
	}
	if text, ok := tsStringy(dir, consts, arg); ok {
		return []string{text}
	}
	return nil
}

// tsFuncKey and tsCallKey give a function declaration and a call of it the
// same name, so two functions of one name in one package — internal/
// orchestrator has three unrelated `fire` methods — cannot lend each other
// their words. A method is keyed by its receiver's own variable name, which
// is what a call site says too (`b.fire(…)`); a plain function is keyed by
// its name alone. The pairing is a heuristic and a wrong one can only make
// the audit ask for a row a warning does not carry, never let one through.
func tsFuncKey(dir string, fn *ast.FuncDecl) string {
	recv := ""
	if fn.Recv != nil && len(fn.Recv.List) == 1 && len(fn.Recv.List[0].Names) == 1 {
		recv = fn.Recv.List[0].Names[0].Name
	}
	return dir + "." + recv + "." + fn.Name.Name
}

// tsCallKey is tsFuncKey's other half, read off a call expression.
func tsCallKey(dir string, call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return dir + ".." + fun.Name
	case *ast.SelectorExpr:
		recv := ""
		if id, ok := fun.X.(*ast.Ident); ok {
			recv = id.Name
		}
		return dir + "." + recv + "." + fun.Sel.Name
	default:
		return ""
	}
}

// tsParamTexts is what one level of parameter flow yields: for a function
// name, the texts each of its parameter positions is ever called with.
// A warning built in one function and passed into another as a plain
// parameter — internal/orchestrator/limits.go's budget banner is the live
// example — is invisible without it, and a whole sentence a user reads can
// then leave the page undocumented (P9-30).
//
// One level is deliberate: it reads the arguments at every call site of
// every function in the scanned packages, resolving each with that caller's
// own locals and constants, and goes no further. A warning built two hops
// away is still unreadable here — and would have to be built at its call
// site instead.
type tsParamTexts map[string]map[string][]string

// tsCollectParamTexts indexes, for every function declared in the scanned
// packages, the texts its callers pass for each named parameter.
func tsCollectParamTexts(t *testing.T, consts tsConsts, returns tsReturnTexts) tsParamTexts {
	t.Helper()
	// names[fn] is the parameter list of the function called fn.
	names := map[string][]string{}
	type callSite struct {
		dir  string
		fn   string
		args []ast.Expr
		locs map[string][]string
	}
	var calls []callSite
	seen := map[string]bool{}
	for _, pkg := range tsPackages {
		for _, path := range tsGoFiles(t, pkg) {
			if seen[path] {
				continue
			}
			seen[path] = true
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			require.NoError(t, err, path)
			dir := filepath.Dir(path)
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				var ps []string
				for _, field := range fn.Type.Params.List {
					for _, id := range field.Names {
						ps = append(ps, id.Name)
					}
					if len(field.Names) == 0 {
						ps = append(ps, "")
					}
				}
				names[tsFuncKey(dir, fn)] = ps
				locals := tsLocals(dir, consts, returns, fn.Body)
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					if key := tsCallKey(dir, call); key != "" {
						calls = append(calls, callSite{dir: dir, fn: key, args: call.Args, locs: locals})
					}
					return true
				})
			}
		}
	}
	out := tsParamTexts{}
	for _, c := range calls {
		ps, ok := names[c.fn]
		if !ok {
			continue
		}
		for i, arg := range c.args {
			if i >= len(ps) || ps[i] == "" {
				continue
			}
			for _, text := range tsWarnTexts(c.dir, consts, c.locs, nil, arg) {
				if !tsSaysSomething(text) {
					continue
				}
				if out[c.fn] == nil {
					out[c.fn] = map[string][]string{}
				}
				out[c.fn][ps[i]] = append(out[c.fn][ps[i]], text)
			}
		}
	}
	return out
}

// tsIsSourceArg reports whether an argument is one of the orchestrator's
// Source… constants, which name the component a warning came from rather
// than saying anything to a user.
func tsIsSourceArg(arg ast.Expr) bool {
	switch e := arg.(type) {
	case *ast.Ident:
		return strings.HasPrefix(e.Name, "Source")
	case *ast.SelectorExpr:
		return strings.HasPrefix(e.Sel.Name, "Source")
	default:
		return false
	}
}

// tsWarnings returns every warning message the non-test files of tsPackages
// raise, rendered the way tsCodeMessages renders an error: verbs whose
// argument sc knows at compile time are substituted, the rest become
// tsWildcard, so a documented key can only match text a user really sees.
func tsWarnings(t *testing.T) []tsMessage {
	t.Helper()
	consts := tsStringConsts(t)
	returns := tsCollectReturnTexts(t, consts)
	params := tsCollectParamTexts(t, consts, returns)
	var out []tsMessage
	seen := map[string]bool{}
	for _, pkg := range tsPackages {
		for _, path := range tsGoFiles(t, pkg) {
			if seen[path] {
				continue
			}
			seen[path] = true
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			require.NoError(t, err, path)
			dir := filepath.Dir(path)
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				locals := tsLocals(dir, consts, returns, fn.Body)
				fnParams := params[tsFuncKey(dir, fn)]
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					name := ""
					switch fun := call.Fun.(type) {
					case *ast.Ident:
						name = fun.Name
					case *ast.SelectorExpr:
						name = fun.Sel.Name
					}
					idx, ok := tsWarnFuncs[name]
					if !ok {
						return true
					}
					args := call.Args
					if idx >= 0 {
						if idx >= len(args) {
							return true
						}
						args = args[idx : idx+1]
					}
					for _, arg := range args {
						if tsIsSourceArg(arg) {
							continue
						}
						texts := tsWarnTexts(dir, consts, locals, fnParams, arg)
						said := false
						for _, text := range texts {
							if !tsSaysSomething(text) {
								continue
							}
							said = true
							out = append(out, tsMessage{
								text: text, file: path, line: fset.Position(call.Pos()).Line,
							})
						}
						if said && idx < 0 {
							break // the first argument with words is the message
						}
					}
					return true
				})
			}
		}
	}
	require.NotEmpty(t, out, "no warnings found: the extraction is broken, not the code")
	return out
}

// TestTroubleshootingCoversEveryWarningTheRunRaises is the guard the round-3
// page did not have. `tsCodeMessages` skips fmt.Sprintf on purpose — it
// builds text, not errors — and every warning is a Sprintf handed to
// Run.Warn, so the whole warning vocabulary was outside the audit: a bead
// that quietly stopped being worked on had no row, and nothing failed.
// Every warning now needs a row keyed by what it prints, or an entry in
// tsExemptWarnings with the reason a user never has to look it up.
func TestTroubleshootingCoversEveryWarningTheRunRaises(t *testing.T) {
	documented := tsDocumented(t)
	var missing []string
	for _, w := range tsWarnings(t) {
		if tsCovered(w.text, documented) || tsExemptWarnings[w.display()] != "" {
			continue
		}
		missing = append(missing, fmt.Sprintf("%s:%d %q", w.file, w.line, w.display()))
	}
	sort.Strings(missing)
	require.Empty(t, missing,
		"docs/troubleshooting.md matches no row for these warnings: add a row keyed by a phrase of one "+
			"that no verb interrupts, or add it to tsExemptWarnings with the reason a user never has to look it up")
}

// tsExemptWarnings are the warnings docs/troubleshooting.md deliberately
// does not document, keyed by what they print, each with the reason. Nearly
// all of them are the run saying that a piece of bookkeeping failed after
// the work itself was done — a bd write, a lease, a shim, a temporary
// directory. The run carries on, nothing is lost, and the sentence a user
// would look up is the wrapped error's, which has a row of its own. The
// rest are prefixes and sc's own programming errors. A new warning must be
// documented or listed here, and a stale entry fails too.
var tsExemptWarnings = map[string]string{
	// Bookkeeping after the work: a bd or git step that failed, reported so
	// the log shows it. The run keeps going and re-does or skips the step.
	"could not record agent …":                                                           "the run could not write an agent to its own state; the firing itself is unaffected",
	"could not clear agent …":                                                            "the same, when the agent finished",
	"could not reap agent …":                                                             "the same, for a session that had already exited",
	"could not claim … for …":                                                            "the claim's own message is the documented `bead is already claimed by another actor`; the scheduler picks other work",
	"could not park … as failed":                                                         "bd would not take the failed edge; the give-up warning above it is the one a user reads",
	"could not release the lease on …":                                                   "a lease left behind at shutdown; the next run's --resume recovers it",
	"could not renew the lease on … for …":                                               "the same, mid-firing; the documented `stopped renewing the lease` row is the user-facing half",
	"could not read the run's beads before releasing their leases":                       "the same, for the whole set: --resume recovers them",
	"could not list the run's beads for the worktree cleanup":                            "the documented `the worktree cleanup did not run` warning is raised beside it",
	"could not remove the bd shim …":                                                     "a temporary file under the run directory; it is removed with the run directory",
	"could not remove the planning bd shim at …":                                         "the same shim, on the planning side",
	"could not delete the worktree copy …":                                               "a temporary copy under the run directory, removed with it",
	"could not remove the directory of a run that never started":                         "an empty run directory left behind by a run that was refused; it holds nothing",
	"could not close the review bead":                                                    "bd bookkeeping on an external pull request's review bead; the review itself was posted",
	"could not hand the review bead to a human":                                          "the same, when the reviewer wanted a person to look at it",
	"could not record the verdict on the review bead":                                    "the same, for the verdict comment",
	"could not record the message on the bead":                                           "the same, for a feedback message; the round carries on",
	"could not write the narrowed acceptance criteria to the bead":                       "the narrowing is applied in the loop either way; only the bd copy is missing",
	"could not read the diff for the next review round":                                  "the round is fired without the diff rather than not at all",
	"could not measure how far the round moved the change set":                           "a progress measurement (D-09), not a decision",
	"could not record the run epic …":                                                    "the plan was applied; only the epic's id could not be stored",
	"could not record the run epic … found in bd":                                        "the same, on resume",
	"could not deprioritise … (…) from priority … to …":                                  "a tidy-up of the backlog's priorities; nothing about the run depends on it",
	"could not check whether this resumed run had already planned":                       "the resumed run plans again rather than not at all",
	"could not tell whether the run's beads have settled; trying again on the next poll": "it says so itself: the next poll retries",
	"bd reported a warning while creating the plan: …":                                   "the documented `bd warned about the graph plan` row is the refusal a user acts on",
	"could not resolve the escalated gate … on …":                                        "bd would not close a gate sc had settled; the gate stays open and the next poll retries",
	"could not read gate …":                                                              "one gate could not be read this poll; the next poll reads it again",
	"could not read …, the bead gate … blocks":                                           "the same, for the bead the gate names",
	"could not move … on …":                                                              "a bd transition the gate poller could not apply; it is retried on the next poll",
	"gate poller: transition refused":                                                    "the D-07 table refused the move; the documented `orchestrator: invalid bead state transition` row is the same event",

	// Deliverer.retryable's reasons (P9-30 made them visible to this
	// guard): each is one delivery step that failed and will be tried
	// again. The words a user acts on are the wrapped error's, which has
	// its own row, and when the retries run out the documented `a delivery
	// failed too many times` warning follows.
	"the worktree could not be inspected":               "a delivery step that is retried; the bead stays in delivering",
	"the engineer's leftovers could not be committed":   "the same, before the delivery itself (D-14)",
	"the pull request could not be delivered":           "the same, for the safety-on path; gh's own message is the one to read",
	"the merge reported neither a result nor an error":  "the same, for a safety-off merge that answered nothing",
	"the merge landed but the bead could not be closed": "the merge is done; only the bd write is retried",
	"the merge failed":                                      "a delivery step that is retried; git's own message is the one to read",
	"the delivery failed at …":                              "the same, naming the stage gitx stopped at",
	"the check failed and the bead could not be moved back": "the documented `the check command failed at delivery` warning is the user-facing half; only the bd move is retried",
	"the bead could not be handed to a human":               "the documented `delivery handed to a human` warning is the user-facing half; only the bd move is retried",
	"the conflict round could not be opened":                "a delivery step that is retried; the conflict itself is in the worktree either way",
	"the resolved conflict could not be submitted":          "the same, after the engineer resolved it",
	"the resolved conflict could not be approved":           "the same, for the approval that follows",

	// Prefixed forms of the bookkeeping warnings above, resolved by the
	// P9-30 parameter flow: the prefix names the component, the rest is the
	// message that already has its reason here.
	"feedback loop: could not measure how far the round moved the change set":       "the unprefixed form is exempt above",
	"feedback loop: could not read the diff for the next review round":              "the same",
	"feedback loop: could not record the message on the bead":                       "the same",
	"feedback loop: could not write the narrowed acceptance criteria to the bead":   "the same",
	"external pull request review: could not close the review bead":                 "the same",
	"external pull request review: could not hand the review bead to a human":       "the same",
	"external pull request review: could not record the verdict on the review bead": "the same",

	// Prefixes: the words a user reads are in the error they carry.
	"resume: …":            "the same, for a resume step",
	"…; … fires … instead": "the reason is the cartridge selection failure, which is documented above; the rest names the role and the cartridge that fired in its place",

	// sc's own programming errors: reaching one is a bug, not a situation.
	"ignored a demand of unknown kind …":                               "a demand kind that does not exist: the scheduler drops it and says so",
	"could not park …: nothing told the scheduler what state it is in": "the demand reached the give-up path with no state to move from, which sc's own wiring is supposed to prevent",

	// A clause, not a message.
	" (the last agent answered: it was the run's own bookkeeping afterwards that failed, not the cartridge)": "appended to the documented give-up warning to say the cartridge was not at fault",
}

// --- What sc doctor prints (P9-08 round 4) ------------------------------

// tsDoctorFile is the file the doctor report's rows are built in. Its
// Detail strings are neither errors nor warnings — they are the words each
// row carries — and they are the first thing a refused run prints, because
// the startup fix list renders them verbatim. The page tells every reader
// to start with `sc doctor`, so its own vocabulary is the one that has to
// be keyed.
const tsDoctorFile = "doctor.go"

// tsExemptDetails are the doctor details docs/troubleshooting.md does not
// document, each with the reason. They are all details of a passing row:
// a version, a path, a count. Nobody looks up the words of a row that is
// telling them everything is fine.
var tsExemptDetails = map[string]string{
	"embedded database answered bd ping in … ms": "the bd row passing, with how long the embedded database took",
	"…, authenticated":                           "the gh row passing",
	"…, authenticated as … on …":                 "the same, when gh also named the account and the host",
	"clean working tree at …":                    "the repository row passing",
	"… present":                                  "the beads row passing, naming the .beads directory it found",
	"ignores .env":                               "the .gitignore row passing",
	"embedded: bd runs Dolt in-process, so the standalone dolt CLI is not needed":                                         "the dolt row passing, saying why the dolt on the machine is irrelevant (P1-15)",
	"embedded: bd runs Dolt in-process, so the standalone dolt CLI is not needed (from metadata.json; bd context failed)": "the same row passing on what .beads/metadata.json says, because bd could not answer",
}

// tsDetails returns the Detail of every doctor row, rendered the way
// tsCodeMessages renders an error.
func tsDetails(t *testing.T) []tsMessage {
	t.Helper()
	consts := tsStringConsts(t)
	var out []tsMessage
	seen := map[string]bool{}
	for _, pkg := range tsMessagePackages {
		for _, path := range tsGoFiles(t, pkg) {
			if seen[path] || filepath.Base(path) != tsDoctorFile {
				continue
			}
			seen[path] = true
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			require.NoError(t, err, path)
			dir := filepath.Dir(path)
			add := func(pos token.Pos, expr ast.Expr) {
				text, ok := tsStringy(dir, consts, expr)
				if !ok || !tsSaysSomething(text) {
					return
				}
				out = append(out, tsMessage{text: text, file: path, line: fset.Position(pos).Line})
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch e := n.(type) {
				case *ast.AssignStmt:
					if len(e.Lhs) != len(e.Rhs) {
						return true
					}
					for i, lhs := range e.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Detail" {
							add(e.Pos(), e.Rhs[i])
						}
					}
				case *ast.KeyValueExpr:
					if key, ok := e.Key.(*ast.Ident); ok && key.Name == "Detail" {
						add(e.Pos(), e.Value)
					}
				}
				return true
			})
		}
	}
	require.NotEmpty(t, out, "no doctor details found: the extraction is broken, not the code")
	return out
}

// TestTroubleshootingCoversTheDoctorRowsOwnWords holds the doctor report to
// the same standard as the errors and the warnings. `sc doctor` is what the
// page tells every reader to run first, and a failing run prints its rows
// as the numbered fix list, so `cartridge:mock: active: "mock" not found on
// PATH` is often the very first thing a user sees. Round 3 keyed the
// nearest row by the sentinel (`cartridge: not installed`) rather than by
// those words.
func TestTroubleshootingCoversTheDoctorRowsOwnWords(t *testing.T) {
	documented := tsDocumented(t)
	var missing []string
	for _, d := range tsDetails(t) {
		if tsCovered(d.text, documented) || tsExemptDetails[d.display()] != "" {
			continue
		}
		missing = append(missing, fmt.Sprintf("%s:%d %q", d.file, d.line, d.display()))
	}
	sort.Strings(missing)
	require.Empty(t, missing,
		"docs/troubleshooting.md matches no row for these `sc doctor` details: add a row keyed by a phrase "+
			"of one that no verb interrupts, or add it to tsExemptDetails with the reason a passing row needs none")
}

// TestTroubleshootingWarningAndDetailExemptionsAreReal is
// TestTroubleshootingExemptionsAreReal for the two lists round 4 added:
// neither may outlive the text it excuses, or quietly cover one the page
// now documents.
func TestTroubleshootingWarningAndDetailExemptionsAreReal(t *testing.T) {
	documented := tsDocumented(t)
	live := func(all []tsMessage) map[string]bool {
		out := map[string]bool{}
		for _, m := range all {
			if !tsCovered(m.text, documented) {
				out[m.display()] = true
			}
		}
		return out
	}
	warnings := live(tsWarnings(t))
	for text, reason := range tsExemptWarnings {
		require.True(t, warnings[text],
			"tsExemptWarnings names %q, which nothing warns any more, or which a row of the page now "+
				"documents: drop the entry", text)
		require.NotEmpty(t, reason, "%q needs a reason", text)
	}
	details := live(tsDetails(t))
	for text, reason := range tsExemptDetails {
		require.True(t, details[text],
			"tsExemptDetails names %q, which no doctor row carries any more, or which a row of the page "+
				"now documents: drop the entry", text)
		require.NotEmpty(t, reason, "%q needs a reason", text)
	}
}
