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
	require.Contains(t, buf.String(), "repo config "+path)
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

// ---------------------------------------------------------------------------
// P9-08: the troubleshooting doc and the messages in the code
// ---------------------------------------------------------------------------

// tsDoc is the page every user-facing message is documented on.
const tsDoc = "../../docs/troubleshooting.md"

// tsPackages are the packages whose sentinel errors must be documented:
// everything a user can reach through the CLI. internal/domain,
// internal/events and internal/logging are left out on purpose — their
// errors are the vocabulary of the libraries underneath, and every one that
// reaches a terminal does so wrapped in a message from this list.
var tsPackages = []string{
	"internal/cli",
	"internal/config",
	"internal/beads",
	"internal/gitx",
	"internal/cartridge",
	"internal/cartridge/structured",
	"internal/agents",
	"internal/orchestrator",
	"internal/proc",
}

// tsExempt are the sentinels docs/troubleshooting.md deliberately does not
// document, each with the reason. They are assembly-time invariants of sc's
// own wiring or internal control flow: reaching one is a bug in sc, not a
// situation a user can be advised out of. A new sentinel must be documented
// or listed here, and a stale entry fails too.
var tsExempt = map[string]string{
	"cli.ErrNotImplemented":               "placeholder for behaviour a later task fills in; never returned by a released command",
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

// tsSentinel is one Err… = errors.New("…") declaration found in the source.
type tsSentinel struct {
	pkg  string // the package's base name, e.g. "beads"
	name string // the identifier, e.g. "ErrNotFound"
	msg  string // the message text
	file string
}

// key is how a sentinel is named in tsExempt.
func (s tsSentinel) key() string { return s.pkg + "." + s.name }

// tsSentinels parses every package in tsPackages and returns its exported
// sentinel errors declared as errors.New with a literal message.
func tsSentinels(t *testing.T) []tsSentinel {
	t.Helper()
	var out []tsSentinel
	for _, pkg := range tsPackages {
		dir := filepath.Join("..", "..", pkg)
		entries, err := os.ReadDir(dir)
		require.NoError(t, err, dir)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			require.NoError(t, err, path)
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
					out = append(out, tsSentinel{pkg: filepath.Base(pkg), name: name.Name, msg: msg, file: path})
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
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
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
		require.NotEmpty(t, found, "docs/troubleshooting.md documents %q, which is in no .go file any more; "+
			"reword the page with the message, or drop the row", msg)
	}
}

// TestTroubleshootingCoversEveryUserFacingSentinel is the other direction:
// a new sentinel error in a package a user can reach must be documented (or
// exempted with a reason), which is what makes the page complete rather
// than a sample.
func TestTroubleshootingCoversEveryUserFacingSentinel(t *testing.T) {
	documented := map[string]bool{}
	for _, msg := range tsMessages(t) {
		documented[msg] = true
	}
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

// TestTroubleshootingExemptionsAreReal keeps the exemption list from
// outliving the errors it excuses.
func TestTroubleshootingExemptionsAreReal(t *testing.T) {
	live := map[string]bool{}
	for _, s := range tsSentinels(t) {
		live[s.key()] = true
	}
	for key, reason := range tsExempt {
		require.True(t, live[key], "tsExempt names %s, which no longer exists: drop the entry", key)
		require.NotEmpty(t, reason, "%s needs a reason", key)
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
