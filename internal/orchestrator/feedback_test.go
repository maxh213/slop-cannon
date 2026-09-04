package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
)

// Tests for the feedback loop engine (P6-04) and the multi-tester
// consolidation folded into it (P6-21). Every helper here is prefixed "fb"
// so it cannot collide with another Phase 6 task's file in this package.
//
// The seven scenarios P6-04's "Done when" names are driven end to end
// through the mock cartridge: the real supervisor fires the real prompts,
// the mock answers with a scripted (and schema-valid) result block, and the
// loop's own parsing, consolidation, state transitions and events are what
// the assertions look at. fbRig.pump makes exactly the calls
// Scheduler.dispatch makes — ask the loop for demands, order them, fire each
// one — so the loop is exercised through its WorkSource and FiringPlanner
// contracts without the scheduler's timers making the tests flaky.

// fbCartridge is the registry name of the mock cartridge these tests fire.
const fbCartridge = "fb-mock"

// fbBead is the bead every scenario runs on.
const fbBead = domain.BeadID("sc-fb1")

// The mock holds its own tool policy in-process, so it can say so.
func init() {
	err := agents.RegisterEnforcement(fbCartridge, func(name string, p cartridge.ToolPolicy) (agents.Enforcement, error) {
		return agents.Enforcement{
			Cartridge: name, Policy: p, Mode: agents.EnforceNative,
			Detail: "the mock refuses forbidden steps in-process",
		}, nil
	})
	if err != nil && !strings.Contains(err.Error(), "already registered") {
		panic(err)
	}
}

// fbEpoch is the fixed time the feedback tests run from.
var fbEpoch = time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)

// fbStore records the state transitions the loop persists. It is the
// StateStore the state machine writes through.
type fbStore struct {
	mu    sync.Mutex
	moves []string
	state domain.BeadState
}

func (s *fbStore) record(what string, to domain.BeadState) (beads.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.moves = append(s.moves, what)
	s.state = to
	return beads.Issue{ID: fbBead, Status: to.String()}, nil
}

func (s *fbStore) SetStatus(_ context.Context, _ domain.BeadID, st domain.BeadState) (beads.Issue, error) {
	return s.record("status:"+st.String(), st)
}

func (s *fbStore) ClaimFor(_ context.Context, _ domain.BeadID, a domain.AgentID) (beads.Issue, error) {
	return s.record("claim:"+a.String(), domain.BeadStateInProgress)
}

func (s *fbStore) Reclaim(_ context.Context, _ domain.BeadID, a domain.AgentID) (beads.Issue, error) {
	return s.record("reclaim:"+a.String(), domain.BeadStateInProgress)
}

func (s *fbStore) Release(_ context.Context, _ domain.BeadID) (beads.Issue, error) {
	return s.record("release", domain.BeadStateOpen)
}

func (s *fbStore) Close(_ context.Context, _ domain.BeadID, reason string) (beads.Issue, error) {
	return s.record("close:"+reason, domain.BeadStateClosed)
}

func (s *fbStore) Moves() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.moves...)
}

func (s *fbStore) State() domain.BeadState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// fbMessages records the structured comments the loop writes on the bead
// (D-06, D-25).
type fbMessages struct {
	mu   sync.Mutex
	sent []beads.Message
	// fail is the message type the store refuses to record, so a test can
	// see what the loop does when bd is unavailable.
	fail domain.MessageType
}

func (m *fbMessages) PostMessage(_ context.Context, id domain.BeadID, actor string, msg any) (beads.Message, error) {
	t, err := beads.MessageTypeOf(msg)
	if err != nil {
		return beads.Message{}, err
	}
	m.mu.Lock()
	refuse := m.fail != "" && m.fail == t
	m.mu.Unlock()
	if refuse {
		return beads.Message{}, errors.New("bd is not answering")
	}
	rec := beads.Message{Type: t, IssueID: id, Author: actor, Payload: msg}
	m.mu.Lock()
	m.sent = append(m.sent, rec)
	m.mu.Unlock()
	return rec, nil
}

// failOn makes the store refuse to record that message type.
func (m *fbMessages) failOn(t domain.MessageType) {
	m.mu.Lock()
	m.fail = t
	m.mu.Unlock()
}

func (m *fbMessages) Types() []domain.MessageType {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]domain.MessageType, 0, len(m.sent))
	for _, r := range m.sent {
		out = append(out, r.Type)
	}
	return out
}

func (m *fbMessages) All() []beads.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]beads.Message(nil), m.sent...)
}

// fbRig is one hermetic run with a feedback loop and a supervisor wired to
// a mock cartridge.
type fbRig struct {
	T        *testing.T
	Run      *Run
	Bus      *events.Bus
	Loop     *Feedback
	Sup      *Supervisor
	Adapter  *mock.Adapter
	Store    *fbStore
	Messages *fbMessages
	Worktree string
	Sub      *events.Subscription
	// Opts are the options Loop was built with, so a test can build the
	// second loop a resumed run would build ([Feedback.Restore]).
	Opts FeedbackOptions

	// Reverse fires each step's demands in the opposite order, which is
	// what one tester finishing before another looks like to the loop.
	Reverse bool

	mu       sync.Mutex
	diff     *gitx.Diff
	ratio    float64
	ratioErr error
	fired    []string
	seen     []events.Event
}

// fbRules and fbVibes are the configured prompt documents every firing must
// carry (P5-01: rules and vibes are pasted verbatim in all three roles).
var (
	fbRules = []prompt.Doc{{Name: "rules.md", Path: "/repo/rules.md",
		Content: "never widen a public API without a bead"}}
	fbVibes = []prompt.Doc{{Name: "vibes.md", Path: "/repo/vibes.md",
		Content: "short functions, boring names"}}
)

// fbSetup builds the rig. tweak may change the loop's options before it is
// built (the D-08 caps, the D-09 threshold, the lenses).
func fbSetup(t *testing.T, sc *mock.Scenario, tweak func(*FeedbackOptions)) *fbRig {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))

	repo := filepath.Join(home, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))

	bus := events.NewBus()
	sub := bus.Subscribe(context.Background(), 1024)
	t.Cleanup(func() { sub.Unsubscribe(); bus.Close() })

	cfg := config.Defaults()
	cfg.ActiveCartridge = fbCartridge
	cfg.Cartridges = map[string]config.Cartridge{fbCartridge: {}}
	adapter, err := mock.NewAdapter(fbCartridge, sc)
	require.NoError(t, err)
	eff := &config.Effective{Config: cfg, Sources: map[string]config.Source{}}
	rc, err := config.NewRunConfig(repo, eff, config.RunSettings{
		ID: "20260903-100000-fb00", Brief: "make the retry loop terminate", StartedAt: fbEpoch, Headless: true,
	})
	require.NoError(t, err)

	now := func() time.Time { return fbEpoch }
	sel, err := cartridge.NewSelector(cfg, map[string]cartridge.Cartridge{fbCartridge: adapter}, cartridge.WithClock(now))
	require.NoError(t, err)

	run, err := New(rc, Options{Bus: bus, Now: now, SkipLock: true})
	require.NoError(t, err)
	require.NoError(t, run.Start(context.Background()))
	t.Cleanup(func() { _ = run.Stop("test over"); _, _ = run.Wait() })

	wt := filepath.Join(run.Dirs().Worktrees, fbBead.String())
	require.NoError(t, os.MkdirAll(wt, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "retry.go"), []byte("package pkg\n"), 0o600))

	rig := &fbRig{T: t, Run: run, Bus: bus, Adapter: adapter, Store: &fbStore{state: domain.BeadStateOpen},
		Messages: &fbMessages{}, Worktree: wt, Sub: sub, diff: fbDiff("head1", 20)}

	states := NewStateMachine(rig.Store, WithStatePublisher(bus), WithStateRun(run.ID()), WithStateClock(now))
	opts := FeedbackOptions{
		Context:  rig.context,
		States:   states,
		Messages: rig.Messages,
		Bus:      bus,
		Run:      run.ID(),
		Now:      now,
		Limits:   config.Limits{MaxReviewRounds: 5, MaxPushbackRounds: 2},
		Ratio:    rig.changeRatio,
	}
	if tweak != nil {
		tweak(&opts)
	}
	rig.Opts = opts
	loop, err := NewFeedback(opts)
	require.NoError(t, err)
	rig.Loop = loop

	sup, err := NewSupervisor(SupervisorOptions{
		Cartridges: sel, Planner: loop, Bus: bus, Timeout: time.Minute, Now: now, NoShims: true,
	})
	require.NoError(t, err)
	sup.Attach(run)
	t.Cleanup(func() { _ = sup.Close() })
	rig.Sup = sup

	loop.Track(domain.Bead{ID: fbBead, Title: "make the retry loop terminate", State: domain.BeadStateOpen})
	return rig
}

// fbDiff is a hand-built change set: the loop never runs git in a test.
func fbDiff(head string, lines int) *gitx.Diff {
	return &gitx.Diff{
		Stats: gitx.DiffStats{
			Base: "base0", Head: head, Files: 1, Insertions: lines, Deletions: 0,
			Changed: []gitx.FileStat{{Path: "retry.go", Insertions: lines}},
		},
		Files: []gitx.DiffFile{{FileStat: gitx.FileStat{Path: "retry.go", Insertions: lines}, Status: gitx.DiffAdded}},
		Patch: "--- a/retry.go\n+++ b/retry.go\n@@\n+for {\n+  break\n+}\n",
	}
}

// context is the rig's FeedbackContextFunc.
func (r *fbRig) context(_ context.Context, bead domain.BeadID) (FeedbackBeadContext, error) {
	r.mu.Lock()
	diff := r.diff
	r.mu.Unlock()
	return FeedbackBeadContext{
		Bead: prompt.BeadContext{
			Bead: domain.Bead{
				ID: bead, Title: "make the retry loop terminate", Type: domain.BeadTypeTask,
				Description: "the retry loop spins forever", Acceptance: "the loop terminates",
				Priority: domain.PriorityDefault,
			},
			Worktree: r.Worktree,
			Branch:   "sc/" + bead.String(),
		},
		Repo:    prompt.RepoContext{Root: r.Worktree, DefaultBranch: "main", CheckCommand: "go test ./..."},
		Brief:   "make the retry loop terminate",
		Rules:   fbRules,
		Vibes:   fbVibes,
		Diff:    diff,
		Timeout: time.Minute,
	}, nil
}

// changeRatio is the rig's D-09 measurement; the test sets the ratio, or
// makes the measurement fail.
func (r *fbRig) changeRatio(_ context.Context, _ domain.BeadID, _, cur *gitx.Diff) (gitx.ChangeRatio, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ratioErr != nil {
		return gitx.ChangeRatio{}, r.ratioErr
	}
	return gitx.ChangeRatio{Current: cur.Head(), Total: cur.ChangedLines(), Ratio: r.ratio}, nil
}

// failRatio makes the D-09 measurement unavailable from now on.
func (r *fbRig) failRatio(err error) {
	r.mu.Lock()
	r.ratioErr = err
	r.mu.Unlock()
}

// setDiff replaces the change set the next round reviews and how far the
// round moved it.
func (r *fbRig) setDiff(head string, lines int, ratio float64) {
	r.mu.Lock()
	r.diff, r.ratio = fbDiff(head, lines), ratio
	r.mu.Unlock()
}

// step fires every demand the loop raises right now, exactly the way
// Scheduler.dispatch does — ask, order, fire — and reports how many agents
// it ran.
func (r *fbRig) step(t *testing.T) int {
	t.Helper()
	ctx := context.Background()
	ds, err := r.Loop.Demands(ctx)
	require.NoError(t, err)
	sortDemands(ds)
	if r.Reverse {
		for i, j := 0, len(ds)-1; i < j; i, j = i+1, j-1 {
			ds[i], ds[j] = ds[j], ds[i]
		}
	}
	for _, d := range ds {
		agent := d.Agent
		if agent == "" {
			agent = mintAgentID(d, 1)
		}
		_, err := r.Sup.Fire(ctx, Firing{
			Agent: domain.Agent{
				ID: agent, Role: d.Role(), Lens: d.Lens, Bead: d.Bead,
				State: domain.AgentStateRunning, StartedAt: fbEpoch,
			},
			Demand: d,
			Run:    r.Run,
		})
		require.NoErrorf(t, err, "firing %s for %s", agent, d.Kind)
		r.mu.Lock()
		r.fired = append(r.fired, agent.String())
		r.mu.Unlock()
	}
	return len(ds)
}

// pump steps until the loop wants nothing more, and fails when it never
// settles.
func (r *fbRig) pump(t *testing.T, steps int) {
	t.Helper()
	for i := 0; i < steps; i++ {
		if r.step(t) == 0 {
			return
		}
	}
	t.Fatalf("the feedback loop did not settle in %d steps (fired %v)", steps, r.Fired())
}

// fbSection returns the body of one "## <title>" section of a prompt.
func fbSection(t *testing.T, text, title string) string {
	t.Helper()
	head := "## " + title + "\n"
	i := strings.Index(text, head)
	require.GreaterOrEqualf(t, i, 0, "the prompt has no %q section", title)
	rest := text[i+len(head):]
	if j := strings.Index(rest, "\n## "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// Fired lists the agents the rig fired, in order.
func (r *fbRig) Fired() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.fired...)
}

// Thread is the loop's snapshot of the scenario's bead.
func (r *fbRig) Thread() FeedbackThread {
	th, ok := r.Loop.Thread(fbBead)
	require.True(r.T, ok, "the loop should be tracking %s", fbBead)
	return th
}

// Prompt returns the prompt the named agent was fired with.
func (r *fbRig) Prompt(agent string) string {
	for _, c := range r.Adapter.Calls() {
		if c.Spec.Labels.Get(cartridge.LabelAgent) == agent {
			return c.Spec.Prompt
		}
	}
	r.T.Fatalf("agent %q was never fired (fired %v)", agent, r.Fired())
	return ""
}

// Prompts returns every prompt the named agent was fired with, in order: a
// round that both judges a pushback and re-reviews a fix fires one lens
// twice, and a narrowed bead reviews its first round again.
func (r *fbRig) Prompts(agent string) []string {
	var out []string
	for _, c := range r.Adapter.Calls() {
		if c.Spec.Labels.Get(cartridge.LabelAgent) == agent {
			out = append(out, c.Spec.Prompt)
		}
	}
	if len(out) == 0 {
		r.T.Fatalf("agent %q was never fired (fired %v)", agent, r.Fired())
	}
	return out
}

// PromptFor returns the prompt of the named agent's firing for one task.
func (r *fbRig) PromptFor(agent string, task events.Task) string {
	for _, c := range r.Adapter.Calls() {
		if c.Spec.Labels.Get(cartridge.LabelAgent) == agent && c.Spec.Labels.Get(cartridge.LabelTask) == task.String() {
			return c.Spec.Prompt
		}
	}
	r.T.Fatalf("agent %q was never fired for %s (fired %v)", agent, task, r.Fired())
	return ""
}

// Events returns the published events of one kind. The bus delivers into a
// buffered channel, so draining it here (rather than in a goroutine) keeps
// the assertions deterministic.
func (r *fbRig) Events(kind events.Kind) []events.Event {
	for {
		select {
		case ev, ok := <-r.Sub.Events():
			if !ok {
				return r.eventsOfKind(kind)
			}
			r.mu.Lock()
			r.seen = append(r.seen, ev)
			r.mu.Unlock()
		default:
			return r.eventsOfKind(kind)
		}
	}
}

// eventsOfKind filters what has been drained so far.
func (r *fbRig) eventsOfKind(kind events.Kind) []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []events.Event
	for _, ev := range r.seen {
		if ev.Kind() == kind {
			out = append(out, ev)
		}
	}
	return out
}

// --- scripted replies -------------------------------------------------

// fbItemAt builds one feedback item.
func fbItemAt(id, title, file string, sev domain.Severity, status domain.ItemStatus) domain.FeedbackItem {
	return domain.FeedbackItem{
		ID: id, Severity: sev, Title: title, File: file,
		Detail: "the loop never leaves its first iteration", Status: status,
	}
}

// fbApprove is a clean approval.
func fbApprove(summary string) domain.Verdict {
	return domain.Verdict{Outcome: domain.VerdictApprove, Summary: summary}
}

// fbChanges is a verdict asking for changes.
func fbChanges(summary string, items ...domain.FeedbackItem) domain.Verdict {
	return domain.Verdict{Outcome: domain.VerdictChanges, Summary: summary, Items: items}
}

// fbSettled is a re-review or evaluation that settles its items.
func fbSettled(summary string, items ...domain.FeedbackItem) domain.Verdict {
	return domain.Verdict{Outcome: domain.VerdictApprove, Summary: summary, Items: items}
}

// fbReport is an engineer's implementation report.
func fbReport(summary string) domain.ImplReport {
	return domain.ImplReport{Summary: summary, FilesChanged: []string{"retry.go"}, HowToVerify: "go test ./..."}
}

// fbResponses wraps the engineer's answers the way the schema wants.
func fbResponses(rs ...domain.Response) agents.ResponseSet {
	return agents.ResponseSet{Responses: rs}
}

// fbScenario starts a scenario with a fallback that fails loudly, so an
// unscripted firing is a test failure and not a mystery.
func fbScenario(name string) *mock.Scenario {
	sc := mock.NewScenario(name)
	sc.Otherwise("unscripted").Say("no rule matched this firing").
		Fail(mock.FailCrash, "the feedback loop fired an agent the scenario does not script")
	return sc
}

// fbSay scripts one agent's reply.
func fbSay(sc *mock.Scenario, agent string, payload any) {
	sc.Rule(agent, mock.Match{Agent: agent}).Say("working on " + agent).Structured(payload)
}

// fbSayTask scripts one agent's reply to one task, for the round where a
// tester is fired twice: once to judge a pushback and once to re-review the
// fix (D-08). The rules are matched in the order they are added, so a
// task-specific rule must be added before any fbSay for the same agent.
func fbSayTask(sc *mock.Scenario, agent string, task events.Task, payload any) {
	sc.Rule(agent+"/"+task.String(), mock.Match{Agent: agent, Task: task.String()}).
		Say("working on " + agent + " (" + task.String() + ")").Structured(payload)
}

// --- (a) all three testers approve in round one -----------------------

func TestFeedbackAllApproveInRoundOne(t *testing.T) {
	sc := fbScenario("all-approve")
	fbSay(sc, "eng-sc-fb1-r1", fbReport("added the break"))
	for _, l := range domain.Lenses() {
		fbSay(sc, "tester-"+l.String()+"-sc-fb1-r1", fbApprove("nothing to add"))
	}
	rig := fbSetup(t, sc, nil)
	rig.pump(t, 8)

	th := rig.Thread()
	assert.Equal(t, FeedbackSettled, th.Phase)
	assert.Equal(t, domain.BeadStateApproved, th.State)
	assert.Equal(t, 1, th.Round)
	assert.Empty(t, th.Items)
	assert.Equal(t, domain.BeadStateApproved, rig.Store.State())
	assert.Equal(t, []string{
		"eng-sc-fb1-r1", "tester-ux-sc-fb1-r1", "tester-qa-sc-fb1-r1", "tester-test-sc-fb1-r1",
	}, rig.Fired())

	// D-06: the report and all three verdicts are on the bead.
	assert.Equal(t, []domain.MessageType{
		domain.MessageImplReport, domain.MessageVerdict, domain.MessageVerdict, domain.MessageVerdict,
	}, rig.Messages.Types())
	require.Len(t, rig.Events(events.KindRoundStarted), 1)
	started, ok := rig.Events(events.KindRoundStarted)[0].(events.RoundStarted)
	require.True(t, ok)
	assert.Equal(t, domain.Lenses(), started.Lenses)
	assert.Equal(t, "head1", started.Commit)
	assert.Len(t, rig.Events(events.KindVerdictReceived), 3)
}

// --- (b) one tester requests changes, the fix is approved in round two --

func TestFeedbackChangesThenApprovedInRoundTwo(t *testing.T) {
	item := fbItemAt("qa-r1-1", "retry loop never terminates", "retry.go", domain.SeverityMajor, domain.ItemOpen)
	sc := fbScenario("changes-then-approved")
	fbSay(sc, "eng-sc-fb1-r1", fbReport("added the retry"))
	fbSay(sc, "tester-ux-sc-fb1-r1", fbApprove("reads fine"))
	fbSay(sc, "tester-test-sc-fb1-r1", fbApprove("covered"))
	fbSay(sc, "tester-qa-sc-fb1-r1", fbChanges("it spins", item))
	fbSay(sc, "eng-sc-fb1-r2", fbResponses(domain.Response{ItemID: "qa-r1-1", Action: domain.ActionFixed, Rationale: "added the break"}))
	resolved := item
	resolved.Status = domain.ItemResolved
	fbSay(sc, "tester-qa-sc-fb1-r2", fbSettled("the break is there", resolved))

	rig := fbSetup(t, sc, nil)
	rig.setDiff("head2", 24, 0.1)
	rig.pump(t, 10)

	th := rig.Thread()
	assert.Equal(t, domain.BeadStateApproved, th.State)
	assert.Equal(t, 2, th.Round)
	require.Len(t, th.Items, 1)
	assert.Equal(t, domain.ItemResolved, th.Items[0].Status)

	// D-09: the two testers that approved in round 1 are not fired again.
	assert.Equal(t, []string{
		"eng-sc-fb1-r1", "tester-ux-sc-fb1-r1", "tester-qa-sc-fb1-r1", "tester-test-sc-fb1-r1",
		"eng-sc-fb1-r2", "tester-qa-sc-fb1-r2",
	}, rig.Fired())
	assert.Contains(t, rig.Store.Moves(), "status:changes_requested")
	assert.Contains(t, rig.Prompt("eng-sc-fb1-r2"), "qa-r1-1")
}

// --- (c) the engineer pushes back and the tester accepts ---------------

func TestFeedbackPushbackAccepted(t *testing.T) {
	item := fbItemAt("qa-r1-1", "retry loop never terminates", "retry.go", domain.SeverityMinor, domain.ItemOpen)
	sc := fbScenario("pushback-accepted")
	fbSay(sc, "eng-sc-fb1-r1", fbReport("added the retry"))
	fbSay(sc, "tester-ux-sc-fb1-r1", fbApprove("reads fine"))
	fbSay(sc, "tester-test-sc-fb1-r1", fbApprove("covered"))
	fbSay(sc, "tester-qa-sc-fb1-r1", fbChanges("it spins", item))
	fbSay(sc, "eng-sc-fb1-r2", fbResponses(domain.Response{
		ItemID: "qa-r1-1", Action: domain.ActionPushback, Rationale: "the caller bounds the retries already",
	}))
	accepted := item
	accepted.Status = domain.ItemAccepted
	fbSay(sc, "tester-qa-sc-fb1-r2", fbSettled("fair enough", accepted))

	rig := fbSetup(t, sc, nil)
	rig.pump(t, 10)

	th := rig.Thread()
	assert.Equal(t, domain.BeadStateApproved, th.State)
	require.Len(t, th.Items, 1)
	assert.Equal(t, domain.ItemAccepted, th.Items[0].Status)
	assert.Equal(t, domain.ActionPushback, th.Items[0].Response.Action)
	assert.Zero(t, th.Items[0].Holds)

	// The pushback went to the tester that raised it, in evaluate mode.
	assert.Contains(t, rig.Fired(), "tester-qa-sc-fb1-r2")
	assert.Contains(t, rig.Prompt("tester-qa-sc-fb1-r2"), "the caller bounds the retries already")
}

// --- (d) the tester holds, the product owner upholds the engineer ------

func TestFeedbackArbitrationUpholdsEngineer(t *testing.T) {
	item := fbItemAt("qa-r1-1", "retry loop never terminates", "retry.go", domain.SeverityMinor, domain.ItemOpen)
	sc := fbScenario("arbitration-upholds-engineer")
	fbSay(sc, "eng-sc-fb1-r1", fbReport("added the retry"))
	fbSay(sc, "tester-ux-sc-fb1-r1", fbApprove("reads fine"))
	fbSay(sc, "tester-test-sc-fb1-r1", fbApprove("covered"))
	fbSay(sc, "tester-qa-sc-fb1-r1", fbChanges("it spins", item))
	fbSay(sc, "eng-sc-fb1-r2", fbResponses(domain.Response{
		ItemID: "qa-r1-1", Action: domain.ActionPushback, Rationale: "the caller bounds the retries already",
	}))
	held := item
	held.Status = domain.ItemHeld
	held.Detail = "the caller is not the only entry point"
	fbSay(sc, "tester-qa-sc-fb1-r2", fbChanges("still not convinced", held))
	fbSay(sc, "po-sc-fb1-r2-qa-r1-1", domain.Arbitration{
		ItemID: "qa-r1-1", Ruling: domain.RulingUpholdEngineer, Rationale: "the bound is documented and tested",
	})

	rig := fbSetup(t, sc, func(o *FeedbackOptions) { o.Limits.MaxPushbackRounds = 1 })
	rig.pump(t, 12)

	th := rig.Thread()
	assert.Equal(t, domain.BeadStateApproved, th.State)
	require.Len(t, th.Items, 1)
	assert.Equal(t, domain.ItemOverruled, th.Items[0].Status)
	assert.Equal(t, 1, th.Items[0].Holds)
	require.NotNil(t, th.Items[0].Ruling)
	assert.Equal(t, domain.RulingUpholdEngineer, th.Items[0].Ruling.Ruling)
	assert.Contains(t, rig.Fired(), "po-sc-fb1-r2-qa-r1-1")
	// The arbitration firing carries the configured rules and vibes too
	// (P5-01), which is what makes a ruling that lets a rules violation
	// ship impossible for the product owner to give by accident.
	assert.Contains(t, rig.Prompt("po-sc-fb1-r2-qa-r1-1"), fbRules[0].Content)
	assert.Contains(t, rig.Prompt("po-sc-fb1-r2-qa-r1-1"), fbVibes[0].Content)
	require.Len(t, rig.Events(events.KindArbitrationReceived), 1)
	assert.Contains(t, rig.Messages.Types(), domain.MessageArbitration)
}

// --- (e) the product owner upholds the tester and the engineer fixes ---

func TestFeedbackArbitrationUpholdsTester(t *testing.T) {
	item := fbItemAt("qa-r1-1", "retry loop never terminates", "retry.go", domain.SeverityMajor, domain.ItemOpen)
	sc := fbScenario("arbitration-upholds-tester")
	fbSay(sc, "eng-sc-fb1-r1", fbReport("added the retry"))
	fbSay(sc, "tester-ux-sc-fb1-r1", fbApprove("reads fine"))
	fbSay(sc, "tester-test-sc-fb1-r1", fbApprove("covered"))
	fbSay(sc, "tester-qa-sc-fb1-r1", fbChanges("it spins", item))
	fbSay(sc, "eng-sc-fb1-r2", fbResponses(domain.Response{
		ItemID: "qa-r1-1", Action: domain.ActionPushback, Rationale: "the caller bounds the retries already",
	}))
	held := item
	held.Status = domain.ItemHeld
	held.Detail = "the caller is not the only entry point"
	fbSay(sc, "tester-qa-sc-fb1-r2", fbChanges("still not convinced", held))
	fbSay(sc, "po-sc-fb1-r2-qa-r1-1", domain.Arbitration{
		ItemID: "qa-r1-1", Ruling: domain.RulingUpholdTester, Instruction: "bound the loop where it is written",
	})
	fbSay(sc, "eng-sc-fb1-r3", fbResponses(domain.Response{
		ItemID: "qa-r1-1", Action: domain.ActionFixed, Rationale: "bounded the loop",
	}))
	resolved := item
	resolved.Status = domain.ItemResolved
	fbSay(sc, "tester-qa-sc-fb1-r3", fbSettled("bounded now", resolved))

	rig := fbSetup(t, sc, func(o *FeedbackOptions) { o.Limits.MaxPushbackRounds = 1 })
	rig.pump(t, 14)

	th := rig.Thread()
	assert.Equal(t, domain.BeadStateApproved, th.State)
	assert.Equal(t, 3, th.Round)
	require.Len(t, th.Items, 1)
	assert.Equal(t, domain.ItemResolved, th.Items[0].Status)
	// The ruling reaches the engineer with the item it decides.
	assert.Contains(t, rig.Prompt("eng-sc-fb1-r3"), "bound the loop where it is written")
}

// --- (f) the round cap escalates and the product owner blocks ----------

func TestFeedbackRoundCapEscalatesAndBlocks(t *testing.T) {
	item := fbItemAt("qa-r1-1", "retry loop never terminates", "retry.go", domain.SeverityBlocker, domain.ItemOpen)
	sc := fbScenario("round-cap")
	fbSay(sc, "eng-sc-fb1-r1", fbReport("added the retry"))
	fbSay(sc, "tester-ux-sc-fb1-r1", fbApprove("reads fine"))
	fbSay(sc, "tester-test-sc-fb1-r1", fbApprove("covered"))
	fbSay(sc, "tester-qa-sc-fb1-r1", fbChanges("it spins", item))
	fbSay(sc, "eng-sc-fb1-r2", fbResponses(domain.Response{
		ItemID: "qa-r1-1", Action: domain.ActionFixed, Rationale: "tried a bound",
	}))
	stillOpen := item
	stillOpen.Detail = "the bound is off by one, it still spins"
	fbSay(sc, "tester-qa-sc-fb1-r2", fbChanges("still spinning", stillOpen))
	fbSay(sc, "po-sc-fb1-r2-escalate", domain.Escalation{
		Outcome: domain.EscalationBlock, Summary: "two rounds could not bound the loop; a human should look",
	})

	rig := fbSetup(t, sc, func(o *FeedbackOptions) { o.Limits.MaxReviewRounds = 2 })
	rig.setDiff("head2", 22, 0.1)
	rig.pump(t, 12)

	th := rig.Thread()
	assert.Equal(t, FeedbackSettled, th.Phase)
	assert.Equal(t, domain.BeadStateNeedsHuman, th.State)
	require.NotNil(t, th.Escalation)
	assert.Equal(t, domain.EscalationBlock, th.Escalation.Outcome)
	assert.Contains(t, rig.Store.Moves(), "status:escalated")
	assert.Contains(t, rig.Store.Moves(), "status:needs_human")
	assert.Contains(t, rig.Messages.Types(), domain.MessageEscalation)
}

// fbCapScenario is the round-cap script of (f) with a configurable ruling.
func fbCapScenario(name string, ruling domain.Escalation) *mock.Scenario {
	item := fbItemAt("qa-r1-1", "retry loop never terminates", "retry.go", domain.SeverityMajor, domain.ItemOpen)
	sc := fbScenario(name)
	fbSay(sc, "eng-sc-fb1-r1", fbReport("added the retry"))
	fbSay(sc, "tester-ux-sc-fb1-r1", fbApprove("reads fine"))
	fbSay(sc, "tester-test-sc-fb1-r1", fbApprove("covered"))
	fbSay(sc, "tester-qa-sc-fb1-r1", fbChanges("it spins", item))
	fbSay(sc, "eng-sc-fb1-r2", fbResponses(domain.Response{
		ItemID: "qa-r1-1", Action: domain.ActionFixed, Rationale: "tried a bound",
	}))
	stillOpen := item
	stillOpen.Detail = "the bound is off by one, it still spins"
	fbSay(sc, "tester-qa-sc-fb1-r2", fbChanges("still spinning", stillOpen))
	fbSay(sc, "po-sc-fb1-r2-escalate", ruling)
	return sc
}

func TestFeedbackEscalationApprovesWithNotes(t *testing.T) {
	sc := fbCapScenario("escalation-approves", domain.Escalation{
		Outcome: domain.EscalationApproveWithNotes,
		Notes:   "the bound is imperfect but the loop terminates; follow-up bead filed",
	})
	rig := fbSetup(t, sc, func(o *FeedbackOptions) { o.Limits.MaxReviewRounds = 2 })
	rig.setDiff("head2", 22, 0.1)
	rig.pump(t, 12)

	th := rig.Thread()
	assert.Equal(t, FeedbackSettled, th.Phase)
	assert.Equal(t, domain.BeadStateApproved, th.State, "approve_with_notes ships the work (D-08)")
	require.Len(t, th.Items, 1)
	assert.Equal(t, domain.ItemOverruled, th.Items[0].Status)
}

func TestFeedbackEscalationNarrowsTheScope(t *testing.T) {
	sc := fbCapScenario("escalation-narrows", domain.Escalation{
		Outcome:       domain.EscalationNarrowScope,
		NewAcceptance: "the loop terminates for the error path only",
	})
	rig := fbSetup(t, sc, func(o *FeedbackOptions) { o.Limits.MaxReviewRounds = 2 })
	rig.setDiff("head2", 22, 0.1)
	// Four steps take the bead through both rounds and the ruling; the
	// narrowed bead then starts its review again, which this test does not
	// script.
	for i := 0; i < 5; i++ {
		rig.step(t)
	}

	th := rig.Thread()
	assert.Equal(t, FeedbackReviewing, th.Phase, "a narrowed bead is reviewed again (D-08)")
	assert.Equal(t, domain.BeadStateInReview, th.State)
	assert.Equal(t, 1, th.Round, "the narrowed bead gets its review rounds back")
	assert.Empty(t, th.Items, "the old items were raised against the old acceptance criteria")
	assert.Empty(t, th.Approved)
	require.NotNil(t, th.Escalation)
	assert.Equal(t, "the loop terminates for the error path only", th.Escalation.NewAcceptance)
}

// --- (g) a big round sends every tester back (D-09) --------------------

func TestFeedbackDiffRatioSendsEveryTesterBack(t *testing.T) {
	item := fbItemAt("qa-r1-1", "retry loop never terminates", "retry.go", domain.SeverityMajor, domain.ItemOpen)
	sc := fbScenario("full-re-review")
	fbSay(sc, "eng-sc-fb1-r1", fbReport("added the retry"))
	fbSay(sc, "tester-ux-sc-fb1-r1", fbApprove("reads fine"))
	fbSay(sc, "tester-test-sc-fb1-r1", fbApprove("covered"))
	fbSay(sc, "tester-qa-sc-fb1-r1", fbChanges("it spins", item))
	fbSay(sc, "eng-sc-fb1-r2", fbResponses(domain.Response{
		ItemID: "qa-r1-1", Action: domain.ActionFixed, Rationale: "rewrote the whole file",
	}))
	resolved := item
	resolved.Status = domain.ItemResolved
	fbSay(sc, "tester-qa-sc-fb1-r2", fbSettled("bounded now", resolved))
	fbSay(sc, "tester-ux-sc-fb1-r2", fbApprove("the rewrite still reads fine"))
	fbSay(sc, "tester-test-sc-fb1-r2", fbApprove("the rewrite is still covered"))

	rig := fbSetup(t, sc, nil)
	rig.setDiff("head2", 400, 0.9)
	rig.pump(t, 12)

	th := rig.Thread()
	assert.Equal(t, domain.BeadStateApproved, th.State)
	assert.True(t, th.FullReview, "the D-09 ratio should have forced a full re-review")
	for _, l := range domain.Lenses() {
		assert.Contains(t, rig.Fired(), "tester-"+l.String()+"-sc-fb1-r2")
	}
	rounds := rig.Events(events.KindRoundStarted)
	require.Len(t, rounds, 2)
	second, ok := rounds[1].(events.RoundStarted)
	require.True(t, ok)
	assert.Equal(t, domain.Lenses(), second.Lenses)
}

// --- P6-21: two testers, one item, one response ------------------------

func TestConsolidateItemsMergesTheSameIssue(t *testing.T) {
	ux := fbItemAt("ux-r1-1", "retry loop never terminates", "retry.go", domain.SeverityMinor, domain.ItemOpen)
	qa := fbItemAt("qa-r1-1", "retry loop never terminates on error", "retry.go", domain.SeverityBlocker, domain.ItemOpen)
	qa.Category = domain.CategoryRules
	other := fbItemAt("qa-r1-2", "the log line leaks the token", "log.go", domain.SeverityMajor, domain.ItemOpen)

	got := ConsolidateItems([]domain.Verdict{
		{Lens: domain.LensQA, Outcome: domain.VerdictChanges, Items: []domain.FeedbackItem{qa, other}},
		{Lens: domain.LensUX, Outcome: domain.VerdictChanges, Items: []domain.FeedbackItem{ux}},
	}, 0)

	require.Len(t, got, 2, "the two testers raised one shared issue and one of their own")
	assert.Equal(t, "ux-r1-1", got[0].Item.ID, "the first raiser in lens order names the merged item")
	assert.Equal(t, []domain.Lens{domain.LensUX, domain.LensQA}, got[0].Lenses)
	assert.Equal(t, domain.CategoryRules, got[0].Item.Category, "a rules violation survives the merge")
	assert.Equal(t, domain.SeverityBlocker, got[0].Item.Severity, "the strictest severity survives the merge")
	assert.Equal(t, []domain.Lens{domain.LensQA}, got[1].Lenses)

	// The one answer is rewritten for each tester's own id.
	r := domain.Response{ItemID: "ux-r1-1", Action: domain.ActionFixed}
	forQA, ok := got[0].ResponseFor(domain.LensQA, r)
	require.True(t, ok)
	assert.Equal(t, "qa-r1-1", forQA.ItemID)
	own, ok := got[0].ForLens(domain.LensQA)
	require.True(t, ok)
	assert.Equal(t, domain.SeverityBlocker, own.Severity, "each tester keeps its own wording")
}

func TestFeedbackOneRequestForTwoTestersOnTheSameIssue(t *testing.T) {
	ux := fbItemAt("ux-r1-1", "retry loop never terminates", "retry.go", domain.SeverityMajor, domain.ItemOpen)
	qa := fbItemAt("qa-r1-1", "retry loop never terminates on error", "retry.go", domain.SeverityMajor, domain.ItemOpen)

	sc := fbScenario("one-item-two-testers")
	fbSay(sc, "eng-sc-fb1-r1", fbReport("added the retry"))
	fbSay(sc, "tester-ux-sc-fb1-r1", fbChanges("it spins", ux))
	fbSay(sc, "tester-qa-sc-fb1-r1", fbChanges("it spins on error", qa))
	fbSay(sc, "tester-test-sc-fb1-r1", fbApprove("covered"))
	fbSay(sc, "eng-sc-fb1-r2", fbResponses(domain.Response{
		ItemID: "ux-r1-1", Action: domain.ActionFixed, Rationale: "bounded the loop once",
	}))
	uxResolved, qaResolved := ux, qa
	uxResolved.Status, qaResolved.Status = domain.ItemResolved, domain.ItemResolved
	fbSay(sc, "tester-ux-sc-fb1-r2", fbSettled("bounded", uxResolved))
	fbSay(sc, "tester-qa-sc-fb1-r2", fbSettled("bounded", qaResolved))

	rig := fbSetup(t, sc, nil)
	rig.setDiff("head2", 24, 0.1)
	rig.pump(t, 12)

	th := rig.Thread()
	require.Len(t, th.Items, 1, "the two testers raised one issue, so the engineer answers one item")
	assert.Equal(t, []domain.Lens{domain.LensUX, domain.LensQA}, th.Items[0].Lenses)
	assert.Equal(t, domain.ItemResolved, th.Items[0].Status)
	assert.Equal(t, domain.BeadStateApproved, th.State)

	// One engineer request, carrying one item id...
	answer := fbSection(t, rig.Prompt("eng-sc-fb1-r2"), "Feedback to answer")
	assert.Contains(t, answer, "ux-r1-1")
	assert.NotContains(t, answer, "qa-r1-1", "the engineer answers the merged item once (P6-21)")
	// ...and each tester carries its own occurrence of it, with the one
	// answer rewritten for its own id (P6-21).
	qaCarried := fbSection(t, rig.Prompt("tester-qa-sc-fb1-r2"), "The items you raised last round")
	assert.Contains(t, qaCarried, "qa-r1-1")
	assert.NotContains(t, qaCarried, "ux-r1-1")
	assert.Contains(t, qaCarried, "bounded the loop once")
	uxCarried := fbSection(t, rig.Prompt("tester-ux-sc-fb1-r2"), "The items you raised last round")
	assert.Contains(t, uxCarried, "ux-r1-1")
	assert.NotContains(t, uxCarried, "qa-r1-1")
	assert.Contains(t, uxCarried, "bounded the loop once")
}

// --- the demands the loop raises ---------------------------------------

func TestFeedbackDemandsCarryTheClaimAndTheRole(t *testing.T) {
	item := fbItemAt("qa-r1-1", "retry loop never terminates", "retry.go", domain.SeverityMajor, domain.ItemOpen)
	sc := fbScenario("demand-shapes")
	fbSay(sc, "eng-sc-fb1-r1", fbReport("added the retry"))
	fbSay(sc, "tester-ux-sc-fb1-r1", fbApprove("reads fine"))
	fbSay(sc, "tester-test-sc-fb1-r1", fbApprove("covered"))
	fbSay(sc, "tester-qa-sc-fb1-r1", fbChanges("it spins", item))
	rig := fbSetup(t, sc, nil)
	ctx := context.Background()

	ds, err := rig.Loop.Demands(ctx)
	require.NoError(t, err)
	require.Len(t, ds, 1)
	assert.Equal(t, DemandImplement, ds[0].Kind)
	assert.Equal(t, ClaimNew, ds[0].Claim, "a fresh bead is claimed atomically (P6-13)")
	assert.Equal(t, events.TaskImplement, ds[0].FiringTask())
	assert.Equal(t, domain.RoleEngineer, ds[0].Role())
	assert.True(t, rig.Loop.Handles(ds[0]))
	assert.False(t, rig.Loop.Handles(Demand{Kind: DemandPlan}))

	require.Equal(t, 1, rig.step(t), "the engineer implements first")
	require.Equal(t, 3, rig.step(t), "then one tester per lens reviews")

	// Round 1 is over; the engineer owes the answers.
	ds, err = rig.Loop.Demands(ctx)
	require.NoError(t, err)
	require.Len(t, ds, 1, "after the round the engineer owes the answers")
	assert.Equal(t, DemandImplement, ds[0].Kind)
	assert.Equal(t, ClaimRework, ds[0].Claim, "a bead coming back from review is reclaimed (P6-13)")
	assert.Equal(t, events.TaskAddressFeedback, ds[0].FiringTask())
	assert.Equal(t, 2, ds[0].Round, "the answer belongs to the new round")
	assert.Equal(t, 1, rig.Loop.Outstanding())
}

func TestFeedbackReviewDemandsAreOnePerLens(t *testing.T) {
	sc := fbScenario("review-demands")
	fbSay(sc, "eng-sc-fb1-r1", fbReport("added the retry"))
	rig := fbSetup(t, sc, nil)
	ctx := context.Background()

	require.Equal(t, 1, rig.step(t))

	ds, err := rig.Loop.Demands(ctx)
	require.NoError(t, err)
	require.Len(t, ds, 3)
	seen := map[domain.Lens]bool{}
	for _, d := range ds {
		assert.Equal(t, DemandReview, d.Kind)
		assert.Equal(t, ClaimNone, d.Claim, "a tester never owns the bead")
		assert.Equal(t, 1, d.Round)
		seen[d.Lens] = true
	}
	assert.Len(t, seen, 3)
	assert.Equal(t, domain.BeadStateInReview, rig.Thread().State)
}

// --- the delivery hooks the loop owns ----------------------------------

func TestFeedbackCheckFailedOpensAQARound(t *testing.T) {
	sc := fbScenario("check-failed")
	fbSay(sc, "eng-sc-fb1-r1", fbReport("added the retry"))
	for _, l := range domain.Lenses() {
		fbSay(sc, "tester-"+l.String()+"-sc-fb1-r1", fbApprove("fine"))
	}
	rig := fbSetup(t, sc, nil)
	rig.pump(t, 8)
	require.Equal(t, domain.BeadStateApproved, rig.Thread().State)
	before := len(rig.Store.Moves())

	// The delivery handoff has already moved the bead back and published
	// its RoundStarted (P6-06); the loop takes the items.
	failed := fbItemAt("check-r1-1", "the check command failed: go test ./...", "retry.go",
		domain.SeverityBlocker, domain.ItemOpen)
	failed.Category = DefaultCheckItemCategory
	require.NoError(t, rig.Loop.CheckFailed(context.Background(),
		domain.Bead{ID: fbBead, Title: "make the retry loop terminate"}, 1, []domain.FeedbackItem{failed}))

	th := rig.Thread()
	assert.Equal(t, FeedbackAddressing, th.Phase)
	assert.Equal(t, domain.BeadStateChangesRequested, th.State)
	assert.Equal(t, 2, th.Round)
	require.Len(t, th.Items, 1)
	assert.Equal(t, []domain.Lens{domain.LensQA}, th.Items[0].Lenses, "a failed check is a qa change request (P6-06)")
	assert.Len(t, rig.Store.Moves(), before, "the delivery handoff already moved the bead")

	ds, err := rig.Loop.Demands(context.Background())
	require.NoError(t, err)
	require.Len(t, ds, 1)
	assert.Equal(t, events.TaskAddressFeedback, ds[0].FiringTask())
	assert.Equal(t, ClaimRework, ds[0].Claim)
}

func TestFeedbackPlansTheDeliveryConflictRound(t *testing.T) {
	sc := fbScenario("conflict-round")
	fbSay(sc, "eng-sc-fb1-r1", fbReport("added the retry"))
	for _, l := range domain.Lenses() {
		fbSay(sc, "tester-"+l.String()+"-sc-fb1-r1", fbApprove("fine"))
	}
	fbSay(sc, "eng-sc-fb1-conflict", fbReport("rebased and kept both changes"))

	var resolved []domain.BeadID
	rig := fbSetup(t, sc, func(o *FeedbackOptions) {
		o.ConflictResolved = func(_ context.Context, b domain.BeadID) error {
			resolved = append(resolved, b)
			return nil
		}
	})
	rig.pump(t, 8)
	require.Equal(t, domain.BeadStateApproved, rig.Thread().State)
	before := len(rig.Store.Moves())

	// The demand the delivery handoff raises for a failed rebase (D-10).
	pending := PendingConflict{
		Bead:    domain.Bead{ID: fbBead, Title: "make the retry loop terminate", State: domain.BeadStateInProgress},
		Round:   1,
		Attempt: 1,
		Agent:   "eng-sc-fb1-conflict",
		Conflict: agents.EngineerConflict{
			Target: "main", Files: []string{"retry.go"}, Output: "CONFLICT (content): Merge conflict in retry.go",
		},
	}
	d := Demand{
		Kind: DemandImplement, Bead: fbBead, Round: 1, Task: events.TaskResolveConflict,
		Claim: ClaimRework, Agent: pending.Agent, Payload: pending,
	}
	assert.True(t, rig.Loop.Handles(d), "the loop plans the delivery's conflict firing (P6-06)")
	_, err := rig.Sup.Fire(context.Background(), Firing{
		Agent:  domain.Agent{ID: pending.Agent, Role: domain.RoleEngineer, Bead: fbBead},
		Demand: d, Run: rig.Run,
	})
	require.NoError(t, err)

	assert.Equal(t, []domain.BeadID{fbBead}, resolved, "the delivery decides what happens next")
	assert.Len(t, rig.Store.Moves(), before, "the loop moves no status in a delivery round")
	conflict := fbSection(t, rig.Prompt("eng-sc-fb1-conflict"), "The conflict")
	assert.Contains(t, conflict, "retry.go")
	assert.Contains(t, conflict, "Merge conflict")
	assert.Contains(t, rig.Messages.Types(), domain.MessageImplReport)
}

func TestFeedbackReviewAgainSendsEveryLensBack(t *testing.T) {
	sc := fbScenario("review-again")
	fbSay(sc, "eng-sc-fb1-r1", fbReport("added the retry"))
	for _, l := range domain.Lenses() {
		fbSay(sc, "tester-"+l.String()+"-sc-fb1-r1", fbApprove("fine"))
	}
	rig := fbSetup(t, sc, nil)
	rig.pump(t, 8)
	before := len(rig.Store.Moves())

	// A conflict resolution over the D-09 threshold: P6-06 leaves the bead
	// in review and asks the loop for a full round.
	rig.setDiff("head2", 400, 0.9)
	require.NoError(t, rig.Loop.ReviewAgain(context.Background(),
		domain.Bead{ID: fbBead}, 1, "the conflict resolution changed 90% of the diff"))

	th := rig.Thread()
	assert.Equal(t, FeedbackReviewing, th.Phase)
	assert.Equal(t, domain.BeadStateInReview, th.State)
	assert.Empty(t, th.Approved, "no approval survives a rewrite that big (D-09)")
	assert.Equal(t, domain.Lenses(), th.Reviewers)
	assert.Len(t, rig.Store.Moves(), before, "the delivery handoff already moved the bead")
}

func TestFeedbackObserveTracksSomebodyElsesMove(t *testing.T) {
	rig := fbSetup(t, fbScenario("observe"), nil)
	rig.Loop.Observe(fbBead, domain.BeadStateDelivering)
	assert.Equal(t, domain.BeadStateDelivering, rig.Thread().State)
	rig.Loop.Observe(fbBead, domain.BeadStateClosed)
	assert.Equal(t, FeedbackSettled, rig.Thread().Phase)
	assert.Zero(t, rig.Loop.Outstanding())
}

// --- resume ------------------------------------------------------------

func TestFeedbackRestoreRebuildsTheThreadFromTheBeadsComments(t *testing.T) {
	sc := fbScenario("restore")
	fbSay(sc, "tester-qa-sc-fb1-r1", fbApprove("unused"))
	rig := fbSetup(t, sc, nil)

	item := fbItemAt("qa-r1-1", "retry loop never terminates", "retry.go", domain.SeverityMajor, domain.ItemOpen)
	msgs := []beads.Message{
		{Type: domain.MessageImplReport, Payload: fbReport("added the retry")},
		{Type: domain.MessageVerdict, Payload: domain.Verdict{Lens: domain.LensUX, Round: 1, Outcome: domain.VerdictApprove}},
		{Type: domain.MessageVerdict, Payload: domain.Verdict{Lens: domain.LensTest, Round: 1, Outcome: domain.VerdictApprove}},
		{Type: domain.MessageVerdict, Payload: fbChanges("it spins", item)},
	}
	msgs[3].Payload = domain.Verdict{Lens: domain.LensQA, Round: 1, Outcome: domain.VerdictChanges, Items: []domain.FeedbackItem{item}}

	require.NoError(t, rig.Loop.Restore(context.Background(), domain.Bead{
		ID: fbBead, Title: "make the retry loop terminate", State: domain.BeadStateChangesRequested, Round: 1,
	}, msgs))

	th := rig.Thread()
	assert.Equal(t, FeedbackAddressing, th.Phase)
	require.Len(t, th.Items, 1)
	assert.Equal(t, domain.ItemOpen, th.Items[0].Status)
	assert.Equal(t, []domain.Lens{domain.LensUX, domain.LensTest}, th.Approved)
	assert.Empty(t, rig.Store.Moves(), "a restore writes nothing to bd")
	assert.Empty(t, rig.Messages.All(), "a restore writes no comments")
}

// A run killed between the last verdict comment (post writes it before the
// status) and the status write leaves bd holding an approved bead in
// in_review. The resumed run's replay concludes the approval, and it has to
// write it: nobody else will, the deliverer only ever sees approved beads,
// and Outstanding would report the run finished with the bead abandoned
// mid-review.
func TestFeedbackRestoreWritesTheApprovalTheKilledRunOwed(t *testing.T) {
	rig := fbSetup(t, fbScenario("restore-approved"), nil)
	resumed, err := NewFeedback(rig.Opts)
	require.NoError(t, err)

	msgs := []beads.Message{{Type: domain.MessageImplReport, Payload: fbReport("added the retry")}}
	for _, l := range domain.Lenses() {
		msgs = append(msgs, beads.Message{Type: domain.MessageVerdict, Payload: domain.Verdict{
			Lens: l, Round: 1, Outcome: domain.VerdictApprove, Summary: "fine",
		}})
	}
	require.NoError(t, resumed.Restore(context.Background(), domain.Bead{
		ID: fbBead, Title: "make the retry loop terminate", State: domain.BeadStateInReview, Round: 1,
	}, msgs))

	th, ok := resumed.Thread(fbBead)
	require.True(t, ok)
	assert.Equal(t, FeedbackSettled, th.Phase)
	assert.Equal(t, domain.BeadStateApproved, th.State)
	assert.Zero(t, resumed.Outstanding(), "the bead is finished with the loop, not abandoned in review")
	assert.Equal(t, []string{"status:approved"}, rig.Store.Moves(),
		"the one transition the killed run owed, and nothing else")
	assert.Empty(t, rig.Messages.All(), "a restore writes no comments")
	assert.Empty(t, rig.Events(events.KindRoundStarted), "a restore replays no rounds")
	assert.Len(t, rig.Events(events.KindBeadStateChanged), 1, "the resumed run is told where the bead went")
}

// --- the loop refuses what is not its work ------------------------------

func TestFeedbackPlanFiringRefusesForeignWork(t *testing.T) {
	rig := fbSetup(t, fbScenario("foreign"), nil)
	_, err := rig.Loop.PlanFiring(context.Background(), Firing{
		Agent:  domain.Agent{ID: "po-plan-1", Role: domain.RolePO},
		Demand: Demand{Kind: DemandPlan},
		Run:    rig.Run,
	})
	require.ErrorIs(t, err, ErrUnknownFeedbackWork)

	_, err = rig.Loop.PlanFiring(context.Background(), Firing{
		Agent:  domain.Agent{ID: "eng-other-r1", Role: domain.RoleEngineer},
		Demand: Demand{Kind: DemandImplement, Bead: "sc-other", Payload: FeedbackWork{Bead: "sc-other"}},
		Run:    rig.Run,
	})
	require.ErrorIs(t, err, ErrNoFeedbackThread)
}

func TestNewFeedbackNeedsAContextFunc(t *testing.T) {
	_, err := NewFeedback(FeedbackOptions{})
	require.ErrorIs(t, err, ErrNoFeedbackContext)
}

// --- D-08: a round that mixes a pushback and a fix is two firings ------

// A tester that pushed back on one item and was told another was fixed owes
// two different judgements: an argument to weigh, and a diff to look at.
// Sweeping the fixed item into the evaluate-pushback firing would let it be
// settled by a prompt that renders no diff at all, so the loop fires the
// evaluation first, with the disputed items alone, and then the re-review
// with the change set.
func TestFeedbackMixedRoundEvaluatesThePushbackAndReReviewsTheFix(t *testing.T) {
	spin := fbItemAt("qa-r1-1", "retry loop never terminates", "retry.go", domain.SeverityMinor, domain.ItemOpen)
	leak := fbItemAt("qa-r1-2", "the log line leaks the token", "log.go", domain.SeverityMajor, domain.ItemOpen)

	sc := fbScenario("mixed-round")
	fbSay(sc, "eng-sc-fb1-r1", fbReport("added the retry"))
	fbSay(sc, "tester-ux-sc-fb1-r1", fbApprove("reads fine"))
	fbSay(sc, "tester-test-sc-fb1-r1", fbApprove("covered"))
	fbSay(sc, "tester-qa-sc-fb1-r1", fbChanges("two problems", spin, leak))
	fbSay(sc, "eng-sc-fb1-r2", fbResponses(
		domain.Response{ItemID: "qa-r1-1", Action: domain.ActionPushback, Rationale: "the caller bounds the retries already"},
		domain.Response{ItemID: "qa-r1-2", Action: domain.ActionFixed, Rationale: "redacted the token"},
	))
	accepted := spin
	accepted.Status = domain.ItemAccepted
	fbSayTask(sc, "tester-qa-sc-fb1-r2", events.TaskEvaluatePushback, fbSettled("fair enough", accepted))
	resolved := leak
	resolved.Status = domain.ItemResolved
	fbSayTask(sc, "tester-qa-sc-fb1-r2", events.TaskReview, fbSettled("the token is redacted", resolved))

	rig := fbSetup(t, sc, nil)
	rig.setDiff("head2", 24, 0.1)
	rig.pump(t, 12)

	// The pushback firing judges the argument and nothing else.
	pushback := rig.PromptFor("tester-qa-sc-fb1-r2", events.TaskEvaluatePushback)
	disputed := fbSection(t, pushback, "The items the engineer pushed back on")
	assert.Contains(t, disputed, "qa-r1-1")
	assert.NotContains(t, disputed, "qa-r1-2",
		"an item the engineer fixed is not in dispute and must not be settled without the diff")
	assert.NotContains(t, pushback, "## The change under review",
		"evaluate_pushback renders no diff (agents.testerSections)")

	// The fix comes back as a re-review of the change set.
	review := rig.PromptFor("tester-qa-sc-fb1-r2", events.TaskReview)
	assert.Contains(t, review, "## The change under review")
	carried := fbSection(t, review, "The items you raised last round")
	assert.Contains(t, carried, "qa-r1-2")
	assert.NotContains(t, carried, "qa-r1-1", "the accepted pushback is settled and is not carried again")

	assert.Equal(t, []string{
		"eng-sc-fb1-r1", "tester-ux-sc-fb1-r1", "tester-qa-sc-fb1-r1", "tester-test-sc-fb1-r1",
		"eng-sc-fb1-r2", "tester-qa-sc-fb1-r2", "tester-qa-sc-fb1-r2",
	}, rig.Fired())

	th := rig.Thread()
	assert.Equal(t, domain.BeadStateApproved, th.State)
	assert.Equal(t, 2, th.Round)
	require.Len(t, th.Items, 2)
	assert.Equal(t, domain.ItemAccepted, th.Items[0].Status)
	assert.Equal(t, domain.ItemResolved, th.Items[1].Status)
}

// --- D-08: the pushback budget is spent per exchange, not per tester ----

func TestFeedbackMergedItemSpendsOnePushbackRoundPerExchange(t *testing.T) {
	ux := fbItemAt("ux-r1-1", "retry loop never terminates", "retry.go", domain.SeverityMajor, domain.ItemOpen)
	qa := fbItemAt("qa-r1-1", "retry loop never terminates on error", "retry.go", domain.SeverityMajor, domain.ItemOpen)
	uxHeld, qaHeld := ux, qa
	uxHeld.Status, qaHeld.Status = domain.ItemHeld, domain.ItemHeld
	uxHeld.Detail = "the other entry point is not bounded"
	qaHeld.Detail = "the error path is not bounded"

	sc := fbScenario("merged-pushback")
	fbSay(sc, "eng-sc-fb1-r1", fbReport("added the retry"))
	fbSay(sc, "tester-ux-sc-fb1-r1", fbChanges("it spins", ux))
	fbSay(sc, "tester-qa-sc-fb1-r1", fbChanges("it spins on error", qa))
	fbSay(sc, "tester-test-sc-fb1-r1", fbApprove("covered"))
	pushback := fbResponses(domain.Response{
		ItemID: "ux-r1-1", Action: domain.ActionPushback, Rationale: "the caller bounds the retries already",
	})
	fbSay(sc, "eng-sc-fb1-r2", pushback)
	fbSay(sc, "tester-ux-sc-fb1-r2", fbChanges("not convinced", uxHeld))
	fbSay(sc, "tester-qa-sc-fb1-r2", fbChanges("not convinced", qaHeld))
	fbSay(sc, "eng-sc-fb1-r3", pushback)
	fbSay(sc, "tester-ux-sc-fb1-r3", fbChanges("still not convinced", uxHeld))
	fbSay(sc, "tester-qa-sc-fb1-r3", fbChanges("still not convinced", qaHeld))
	fbSay(sc, "po-sc-fb1-r3-ux-r1-1", domain.Arbitration{
		ItemID: "ux-r1-1", Ruling: domain.RulingUpholdEngineer, Rationale: "the bound is documented and tested",
	})

	rig := fbSetup(t, sc, nil) // the default D-08 budget: max_pushback_rounds = 2
	require.Equal(t, 1, rig.step(t), "the engineer implements")
	require.Equal(t, 3, rig.step(t), "three lenses review")
	require.Equal(t, 1, rig.step(t), "the engineer pushes back")
	require.Equal(t, 2, rig.step(t), "both testers that raised the issue judge the pushback")

	th := rig.Thread()
	require.Len(t, th.Items, 1, "the two testers raised one consolidated issue (P6-21)")
	assert.Equal(t, 1, th.Items[0].Holds, "two lenses holding one item is one pushback exchange (D-08)")
	assert.Equal(t, FeedbackAddressing, th.Phase,
		"half the budget is left, so the item goes back to the engineer rather than to the product owner")

	require.Equal(t, 1, rig.step(t), "the engineer pushes back a second time")
	require.Equal(t, 2, rig.step(t), "both testers hold again")
	th = rig.Thread()
	assert.Equal(t, 2, th.Items[0].Holds, "the second exchange spends the second round of the budget")
	assert.Equal(t, FeedbackArbitrating, th.Phase, "the budget is spent, so the product owner rules (D-08)")

	rig.pump(t, 6)
	th = rig.Thread()
	assert.Equal(t, domain.BeadStateApproved, th.State)
	require.NotNil(t, th.Items[0].Ruling)
	assert.Contains(t, rig.Fired(), "po-sc-fb1-r3-ux-r1-1")
}

// --- the round history keeps each round's own report and ruling --------

func TestFeedbackRoundHistoryKeepsTheReportAndTheRulingToTheirOwnRound(t *testing.T) {
	rig := fbSetup(t, fbScenario("round-history"), nil)
	report := fbReport("the first round's work")
	esc := domain.Escalation{Outcome: domain.EscalationNarrowScope, NewAcceptance: "the error path only"}

	loop := rig.Loop
	loop.mu.Lock()
	th := loop.threads[fbBead]
	th.round, th.report, th.escalation = 1, &report, &esc
	loop.closeRound(th)
	th.round = 2 // a round in which the engineer only answered items
	loop.closeRound(th)
	history := append([]prompt.Round(nil), th.history...)
	loop.mu.Unlock()

	require.Len(t, history, 2)
	assert.NotNil(t, history[0].Report)
	assert.NotNil(t, history[0].Escalation)
	assert.Nil(t, history[1].Report, "a round with no new report must not file the previous round's")
	assert.Nil(t, history[1].Escalation, "a ruling belongs to the round that made it, not to every round after")
}

// --- D-08: a narrow_scope ruling's criteria are what the bead is judged by

func TestFeedbackNarrowedAcceptanceReachesEveryLaterPrompt(t *testing.T) {
	const narrowed = "the loop terminates for the error path only"
	sc := fbCapScenario("narrow-acceptance", domain.Escalation{
		Outcome: domain.EscalationNarrowScope, NewAcceptance: narrowed,
	})
	rig := fbSetup(t, sc, func(o *FeedbackOptions) { o.Limits.MaxReviewRounds = 2 })
	rig.setDiff("head2", 22, 0.1)
	for i := 0; i < 5; i++ {
		rig.step(t)
	}
	require.Equal(t, narrowed, rig.Thread().Escalation.NewAcceptance)

	// The next refresh from bd still carries the criteria the ruling
	// replaced (P6-24 has not written them back yet); it must not undo the
	// ruling.
	rig.Loop.Track(domain.Bead{
		ID: fbBead, Title: "make the retry loop terminate",
		Acceptance: "the loop terminates", State: domain.BeadStateInReview,
	})

	require.Equal(t, 3, rig.step(t), "the narrowed bead is reviewed again by every lens")
	prompts := rig.Prompts("tester-ux-sc-fb1-r1")
	require.Len(t, prompts, 2, "the ux lens reviewed the bead before and after the ruling")
	assert.Contains(t, fbAcceptance(t, prompts[1]), narrowed,
		"the tester judges the bead against the criteria the product owner narrowed it to")

	require.Equal(t, 1, rig.step(t), "the round the narrowed review opened goes to the engineer")
	eng := rig.Prompts("eng-sc-fb1-r2")
	require.Len(t, eng, 2)
	assert.Contains(t, fbAcceptance(t, eng[1]), narrowed,
		"the engineer works to the narrowed criteria too")
}

// fbAcceptance returns the bead's acceptance criteria as one prompt renders
// them.
func fbAcceptance(t *testing.T, text string) string {
	t.Helper()
	i := strings.Index(text, "Acceptance criteria")
	require.GreaterOrEqual(t, i, 0, "the prompt states no acceptance criteria")
	rest := text[i:]
	if j := strings.Index(rest, "\n## "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// --- the configured rules and vibes reach every role -------------------

func TestFeedbackSendsTheRulesAndVibesToEveryRole(t *testing.T) {
	sc := fbCapScenario("rules-and-vibes", domain.Escalation{
		Outcome: domain.EscalationBlock, Summary: "two rounds could not bound the loop",
	})
	rig := fbSetup(t, sc, func(o *FeedbackOptions) { o.Limits.MaxReviewRounds = 2 })
	rig.setDiff("head2", 22, 0.1)
	rig.pump(t, 12)

	for _, agent := range []string{
		"eng-sc-fb1-r1", "eng-sc-fb1-r2",
		"tester-ux-sc-fb1-r1", "tester-qa-sc-fb1-r1", "tester-test-sc-fb1-r1", "tester-qa-sc-fb1-r2",
		"po-sc-fb1-r2-escalate",
	} {
		p := rig.Prompt(agent)
		assert.Containsf(t, p, fbRules[0].Content, "%s was fired without the configured rules", agent)
		assert.Containsf(t, p, fbVibes[0].Content, "%s was fired without the configured vibes", agent)
	}
}

// --- what the loop does when bd or git will not answer -----------------

func TestFeedbackWarnsWhenItCannotRecordAMessage(t *testing.T) {
	sc := fbScenario("message-store-down")
	fbSay(sc, "eng-sc-fb1-r1", fbReport("added the retry"))
	for _, l := range domain.Lenses() {
		fbSay(sc, "tester-"+l.String()+"-sc-fb1-r1", fbApprove("fine"))
	}
	rig := fbSetup(t, sc, nil)
	rig.Messages.failOn(domain.MessageVerdict)
	rig.pump(t, 8)

	w := fbWarning(t, rig, "could not record the message")
	assert.Equal(t, SourceFeedback, w.Source)
	assert.Equal(t, fbBead, w.Bead)
	assert.Contains(t, w.Err, "bd is not answering")
	assert.Equal(t, domain.BeadStateApproved, rig.Thread().State,
		"a bead comment that will not write is a warning, not a stopped run")
}

func TestFeedbackUnmeasurableRoundSendsEveryTesterBack(t *testing.T) {
	item := fbItemAt("qa-r1-1", "retry loop never terminates", "retry.go", domain.SeverityMajor, domain.ItemOpen)
	sc := fbScenario("ratio-unmeasurable")
	fbSay(sc, "eng-sc-fb1-r1", fbReport("added the retry"))
	fbSay(sc, "tester-ux-sc-fb1-r1", fbApprove("reads fine"))
	fbSay(sc, "tester-test-sc-fb1-r1", fbApprove("covered"))
	fbSay(sc, "tester-qa-sc-fb1-r1", fbChanges("it spins", item))
	fbSay(sc, "eng-sc-fb1-r2", fbResponses(domain.Response{
		ItemID: "qa-r1-1", Action: domain.ActionFixed, Rationale: "bounded the loop",
	}))
	resolved := item
	resolved.Status = domain.ItemResolved
	fbSay(sc, "tester-qa-sc-fb1-r2", fbSettled("bounded now", resolved))
	fbSay(sc, "tester-ux-sc-fb1-r2", fbApprove("still reads fine"))
	fbSay(sc, "tester-test-sc-fb1-r2", fbApprove("still covered"))

	rig := fbSetup(t, sc, nil)
	rig.setDiff("head2", 24, 0.1)
	rig.failRatio(errors.New("git is not answering"))
	rig.pump(t, 12)

	w := fbWarning(t, rig, "could not measure how far the round moved")
	assert.Equal(t, SourceFeedback, w.Source)
	th := rig.Thread()
	assert.True(t, th.FullReview, "an unmeasurable round reads as everything changed (D-09)")
	for _, l := range domain.Lenses() {
		assert.Contains(t, rig.Fired(), "tester-"+l.String()+"-sc-fb1-r2")
	}
	assert.Equal(t, domain.BeadStateApproved, th.State)
}

// fbWarning is the first feedback-loop warning whose message contains want.
func fbWarning(t *testing.T, rig *fbRig, want string) events.Warning {
	t.Helper()
	for _, ev := range rig.Events(events.KindWarning) {
		w, ok := ev.(events.Warning)
		if ok && w.Source == SourceFeedback && strings.Contains(w.Message, want) {
			return w
		}
	}
	t.Fatalf("the loop published no warning about %q", want)
	return events.Warning{}
}

// --- the snapshot of every tracked bead --------------------------------

func TestFeedbackThreadsSnapshotsEveryTrackedBead(t *testing.T) {
	rig := fbSetup(t, fbScenario("threads"), nil)
	rig.Loop.Track(
		domain.Bead{ID: "sc-fb2", Title: "the second bead", State: domain.BeadStateInReview},
		domain.Bead{ID: "sc-fb3", Title: "already shipped", State: domain.BeadStateClosed},
	)

	ths := rig.Loop.Threads()
	require.Len(t, ths, 2, "a bead in a terminal state is not adopted")
	assert.Equal(t, fbBead, ths[0].Bead, "threads come back in the order the loop adopted them")
	assert.Equal(t, FeedbackImplementing, ths[0].Phase)
	assert.Equal(t, domain.BeadID("sc-fb2"), ths[1].Bead)
	assert.Equal(t, "the second bead", ths[1].Title)
	assert.Equal(t, FeedbackReviewing, ths[1].Phase)

	one, ok := rig.Loop.Thread("sc-fb2")
	require.True(t, ok)
	assert.Equal(t, ths[1], one, "Threads and Thread render the same snapshot")
	assert.Equal(t, 2, rig.Loop.Outstanding())
}

// --- D-09: a firing that was shown no diff has not reviewed the code ----

// A round the engineer spends arguing still moves the code: D-14 commits
// whatever the session left behind, so the head the next review must look at
// is not the one the round started from. The tester that judges the
// pushback is shown no diff at all (evaluate_pushback renders none), so it
// must not be credited with having reviewed that new commit — otherwise a
// round whose answers are all pushback approves a change set nobody looked
// at.
func TestFeedbackPushbackOnlyRoundStillReviewsTheNewCommit(t *testing.T) {
	item := fbItemAt("qa-r1-1", "retry loop never terminates", "retry.go", domain.SeverityMinor, domain.ItemOpen)
	sc := fbScenario("pushback-only-round")
	fbSay(sc, "eng-sc-fb1-r1", fbReport("added the retry"))
	fbSay(sc, "tester-ux-sc-fb1-r1", fbApprove("reads fine"))
	fbSay(sc, "tester-test-sc-fb1-r1", fbApprove("covered"))
	fbSay(sc, "tester-qa-sc-fb1-r1", fbChanges("it spins", item))
	fbSay(sc, "eng-sc-fb1-r2", fbResponses(domain.Response{
		ItemID: "qa-r1-1", Action: domain.ActionPushback, Rationale: "the caller bounds the retries already",
	}))
	accepted := item
	accepted.Status = domain.ItemAccepted
	fbSayTask(sc, "tester-qa-sc-fb1-r2", events.TaskEvaluatePushback, fbSettled("fair enough", accepted))
	fbSayTask(sc, "tester-qa-sc-fb1-r2", events.TaskReview, fbApprove("the code the round left behind is fine"))

	rig := fbSetup(t, sc, nil)
	require.Equal(t, 1, rig.step(t), "the engineer implements")
	require.Equal(t, 3, rig.step(t), "three lenses review head1")

	// The engineer only argues, but its session still committed (D-14).
	rig.setDiff("head2", 22, 0.1)
	require.Equal(t, 1, rig.step(t), "the engineer pushes back")
	assert.Equal(t, FeedbackEvaluating, rig.Thread().Phase)

	require.Equal(t, 1, rig.step(t), "qa judges the argument")
	th := rig.Thread()
	require.Len(t, th.Items, 1)
	assert.Equal(t, domain.ItemAccepted, th.Items[0].Status, "qa accepted the argument")
	assert.NotEqual(t, domain.BeadStateApproved, th.State,
		"an accepted argument is not a review of the code the round left behind (D-09)")
	assert.Equal(t, FeedbackReviewing, th.Phase)
	assert.Equal(t, []domain.Lens{domain.LensQA}, th.Reviewers,
		"qa did not approve round one and the code moved, so it reviews again; ux and test approved, and an approval stands")

	require.Equal(t, 1, rig.step(t), "qa re-reviews head2")
	assert.Equal(t, domain.BeadStateApproved, rig.Thread().State)

	assert.Equal(t, []string{
		"eng-sc-fb1-r1", "tester-ux-sc-fb1-r1", "tester-qa-sc-fb1-r1", "tester-test-sc-fb1-r1",
		"eng-sc-fb1-r2", "tester-qa-sc-fb1-r2", "tester-qa-sc-fb1-r2",
	}, rig.Fired())

	// The firing that settled the bead is the one that was shown head2.
	assert.NotContains(t, rig.PromptFor("tester-qa-sc-fb1-r2", events.TaskEvaluatePushback),
		"## The change under review")
	assert.Contains(t, rig.PromptFor("tester-qa-sc-fb1-r2", events.TaskReview), "## The change under review")
	rounds := rig.Events(events.KindRoundStarted)
	require.Len(t, rounds, 2, "the evaluation opens no review round; the re-review does")
	second, ok := rounds[1].(events.RoundStarted)
	require.True(t, ok)
	assert.Equal(t, 2, second.Round)
	assert.Equal(t, []domain.Lens{domain.LensQA}, second.Lenses)
	assert.Equal(t, "head2", second.Commit, "the round that settles the bead looks at the new commit")
}

// --- D-08: the product owner's ruling is binding ------------------------

// An engineer that argues with a ruling instead of carrying it out must not
// be able to erase it: that would restart the dispute the ruling closed —
// the same tester judging the same argument again, the same item arbitrated
// a second time with an empty ruling history — and burn a review round each
// time round.
func TestFeedbackBindingRulingCannotBeArguedWithAgain(t *testing.T) {
	item := fbItemAt("qa-r1-1", "retry loop never terminates", "retry.go", domain.SeverityMajor, domain.ItemOpen)
	sc := fbScenario("binding-ruling")
	fbSay(sc, "eng-sc-fb1-r1", fbReport("added the retry"))
	fbSay(sc, "tester-ux-sc-fb1-r1", fbApprove("reads fine"))
	fbSay(sc, "tester-test-sc-fb1-r1", fbApprove("covered"))
	fbSay(sc, "tester-qa-sc-fb1-r1", fbChanges("it spins", item))
	fbSay(sc, "eng-sc-fb1-r2", fbResponses(domain.Response{
		ItemID: "qa-r1-1", Action: domain.ActionPushback, Rationale: "the caller bounds the retries already",
	}))
	held := item
	held.Status = domain.ItemHeld
	held.Detail = "the caller is not the only entry point"
	fbSay(sc, "tester-qa-sc-fb1-r2", fbChanges("still not convinced", held))
	fbSay(sc, "po-sc-fb1-r2-qa-r1-1", domain.Arbitration{
		ItemID: "qa-r1-1", Ruling: domain.RulingUpholdTester, Instruction: "bound the loop where it is written",
	})
	fbSay(sc, "eng-sc-fb1-r3", fbResponses(domain.Response{
		ItemID: "qa-r1-1", Action: domain.ActionPushback, Rationale: "I still disagree with the product owner",
	}))
	fbSay(sc, "eng-sc-fb1-r4", fbResponses(domain.Response{
		ItemID: "qa-r1-1", Action: domain.ActionFixed, Rationale: "bounded the loop where it is written",
	}))
	resolved := item
	resolved.Status = domain.ItemResolved
	fbSay(sc, "tester-qa-sc-fb1-r4", fbSettled("bounded now", resolved))

	rig := fbSetup(t, sc, func(o *FeedbackOptions) { o.Limits.MaxPushbackRounds = 1 })
	rig.pump(t, 16)

	th := rig.Thread()
	assert.Equal(t, domain.BeadStateApproved, th.State)
	require.Len(t, th.Items, 1)
	require.NotNil(t, th.Items[0].Ruling, "the ruling survives the engineer arguing with it (D-08)")
	assert.Equal(t, domain.RulingUpholdTester, th.Items[0].Ruling.Ruling)
	assert.Equal(t, 1, th.Items[0].Holds, "arguing again does not spend another pushback exchange")

	// The refused round judged nothing and arbitrated nothing a second time.
	assert.Equal(t, []string{
		"eng-sc-fb1-r1", "tester-ux-sc-fb1-r1", "tester-qa-sc-fb1-r1", "tester-test-sc-fb1-r1",
		"eng-sc-fb1-r2", "tester-qa-sc-fb1-r2", "po-sc-fb1-r2-qa-r1-1",
		"eng-sc-fb1-r3", "eng-sc-fb1-r4", "tester-qa-sc-fb1-r4",
	}, rig.Fired())

	w := fbWarning(t, rig, "binding ruling")
	assert.Equal(t, fbBead, w.Bead)
	// The ruling is still in front of the engineer in every later round.
	assert.Contains(t, rig.Prompt("eng-sc-fb1-r4"), "bound the loop where it is written")
}

// --- D-08: a failed check at the round cap escalates --------------------

func TestFeedbackCheckFailedAtTheRoundCapEscalates(t *testing.T) {
	sc := fbScenario("check-failed-at-the-cap")
	fbSay(sc, "eng-sc-fb1-r1", fbReport("added the retry"))
	for _, l := range domain.Lenses() {
		fbSay(sc, "tester-"+l.String()+"-sc-fb1-r1", fbApprove("fine"))
	}
	fbSay(sc, "po-sc-fb1-r2-escalate", domain.Escalation{
		Outcome: domain.EscalationBlock, Summary: "the check has failed at the last round; a human should look",
	})
	rig := fbSetup(t, sc, func(o *FeedbackOptions) { o.Limits.MaxReviewRounds = 2 })
	rig.pump(t, 8)
	require.Equal(t, domain.BeadStateApproved, rig.Thread().State)

	failed := fbItemAt("check-r2-1", "the check command failed: go test ./...", "retry.go",
		domain.SeverityBlocker, domain.ItemOpen)
	failed.Category = DefaultCheckItemCategory
	// The delivery handoff reports the failure of the last round the cap
	// allows (DeliveryOutcome.Round is the bead's own sc_round).
	require.NoError(t, rig.Loop.CheckFailed(context.Background(),
		domain.Bead{ID: fbBead, Title: "make the retry loop terminate"}, 2, []domain.FeedbackItem{failed}))

	th := rig.Thread()
	assert.Equal(t, FeedbackEscalating, th.Phase, "there is no review round left to answer the failure (D-08)")
	assert.Equal(t, 2, th.Round, "a failed check at the cap buys no extra round")
	assert.Equal(t, domain.BeadStateEscalated, th.State)
	assert.Equal(t, domain.BeadStateEscalated, rig.Store.State())

	rig.pump(t, 4)
	assert.Equal(t, domain.BeadStateNeedsHuman, rig.Thread().State)
	assert.Contains(t, rig.Fired(), "po-sc-fb1-r2-escalate")
}

// --- the round a conflict review-again opens is the thread's own --------

func TestFeedbackReviewAgainPublishesTheThreadsRound(t *testing.T) {
	sc := fbScenario("review-again-no-round")
	fbSay(sc, "eng-sc-fb1-r1", fbReport("added the retry"))
	for _, l := range domain.Lenses() {
		fbSay(sc, "tester-"+l.String()+"-sc-fb1-r1", fbApprove("fine"))
	}
	rig := fbSetup(t, sc, nil)
	rig.pump(t, 8)
	before := len(rig.Events(events.KindRoundStarted))

	// A bead with no sc_round metadata reaches P6-06's review-again path
	// with no round of its own.
	require.NoError(t, rig.Loop.ReviewAgain(context.Background(),
		domain.Bead{ID: fbBead}, 0, "the conflict resolution changed 90% of the diff"))

	th := rig.Thread()
	assert.Equal(t, 1, th.Round, "no round was given, so the thread keeps the one it has")
	rounds := rig.Events(events.KindRoundStarted)
	require.Len(t, rounds, before+1)
	opened, ok := rounds[before].(events.RoundStarted)
	require.True(t, ok)
	assert.Equal(t, th.Round, opened.Round, "the event names the thread's round, not the caller's zero")
	assert.Equal(t, domain.Lenses(), opened.Lenses)
}

// --- P6-21: the live merge is the exported one --------------------------

// The scheduler fires the three lenses concurrently, so which tester's
// wording names a shared issue must not depend on which of them finishes
// first: the loop's live merge and ConsolidateItems have to agree.
func TestFeedbackConsolidationDoesNotDependOnTheTesterOrder(t *testing.T) {
	ux := fbItemAt("ux-r1-1", "retry loop never terminates", "retry.go", domain.SeverityMinor, domain.ItemOpen)
	qa := fbItemAt("qa-r1-1", "retry loop never terminates on error", "retry.go", domain.SeverityBlocker, domain.ItemOpen)
	build := func() *mock.Scenario {
		sc := fbScenario("consolidation-order")
		fbSay(sc, "eng-sc-fb1-r1", fbReport("added the retry"))
		fbSay(sc, "tester-ux-sc-fb1-r1", fbChanges("it spins", ux))
		fbSay(sc, "tester-qa-sc-fb1-r1", fbChanges("it spins on error", qa))
		fbSay(sc, "tester-test-sc-fb1-r1", fbApprove("covered"))
		return sc
	}
	want := ConsolidateItems([]domain.Verdict{
		{Lens: domain.LensQA, Outcome: domain.VerdictChanges, Items: []domain.FeedbackItem{qa}},
		{Lens: domain.LensUX, Outcome: domain.VerdictChanges, Items: []domain.FeedbackItem{ux}},
	}, 0)
	require.Len(t, want, 1)

	for _, reverse := range []bool{false, true} {
		name := "ux first"
		if reverse {
			name = "qa first"
		}
		t.Run(name, func(t *testing.T) {
			rig := fbSetup(t, build(), nil)
			rig.Reverse = reverse
			require.Equal(t, 1, rig.step(t), "the engineer implements")
			require.Equal(t, 3, rig.step(t), "three lenses review")

			th := rig.Thread()
			require.Len(t, th.Items, 1, "the two testers raised one issue (P6-21)")
			assert.Equal(t, want[0].Item.ID, th.Items[0].Item.ID,
				"the first raiser in lens order names the merged item, whoever replied first")
			assert.Equal(t, want[0].Item.Title, th.Items[0].Item.Title)
			assert.Equal(t, want[0].Item.File, th.Items[0].Item.File)
			assert.Equal(t, want[0].Item.Severity, th.Items[0].Item.Severity)
			assert.Equal(t, want[0].Lenses, th.Items[0].Lenses)
		})
	}
}
