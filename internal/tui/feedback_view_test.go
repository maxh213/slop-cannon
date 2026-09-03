package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// fbGoldenDir is where the Feedback view's golden frames live.
const fbGoldenDir = "../../testdata/tui"

// fbRunID names the run every Feedback-view fixture watches.
const fbRunID = domain.RunID("sc-feedback-0001")

// fbAt returns a deterministic timestamp, n minutes after a fixed start.
func fbAt(n int) time.Time {
	return time.Date(2026, 3, 5, 11, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Minute)
}

// fbMeta stamps the nth event of the fixture run.
func fbMeta(n int) events.Meta { return events.MetaAt(fbRunID, fbAt(n)) }

// fbRunInfo builds the run the fixtures watch.
func fbRunInfo() domain.Run {
	return domain.Run{
		ID:        fbRunID,
		RepoPath:  "/tmp/repo",
		Mode:      domain.RunModeBrief,
		Brief:     "fix the retry bugs",
		StartedAt: fbAt(0),
		Config:    domain.ConfigSnapshot{Ammo: 4, Safety: true, Cartridge: "mock"},
	}
}

// fbClock hands out fixture timestamps in order, so the scenario builders
// below do not have to count minutes.
type fbClock struct{ n int }

func (c *fbClock) next() events.Meta {
	c.n++
	return fbMeta(c.n)
}

// fbBead builds a bead the way the graph reports it.
func fbBead(id domain.BeadID, title string, state domain.BeadState, round int) domain.Bead {
	return domain.Bead{
		ID: id, Title: title, Type: domain.BeadTypeTask,
		Priority: domain.PriorityDefault, State: state, Round: round,
	}
}

// fbItem builds one feedback item.
func fbItem(id string, sev domain.Severity, title string, status domain.ItemStatus) domain.FeedbackItem {
	return domain.FeedbackItem{
		ID: id, Severity: sev, Title: title, Status: status,
		Detail: "seen while exercising " + title,
		File:   "internal/http/retry.go", Line: 42,
		Suggestion: "guard the call and cover it with a table case",
	}
}

// fbReport is the engineer's implementation report for a round.
func fbReport(summary string) *domain.ImplReport {
	return &domain.ImplReport{
		Summary:      summary,
		FilesChanged: []string{"internal/http/retry.go", "internal/http/retry_test.go"},
		HowToVerify:  "go test ./internal/http/...",
		Risks:        []string{"the backoff constants are a guess"},
		Commit:       "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d",
	}
}

// fbAllLenses is the full tester roster of a first round.
var fbAllLenses = []domain.Lens{domain.LensUX, domain.LensQA, domain.LensTest}

// fbApprove is a clean approval from one lens.
func fbApprove(lens domain.Lens, round int, summary string) domain.Verdict {
	return domain.Verdict{Lens: lens, Round: round, Outcome: domain.VerdictApprove, Summary: summary}
}

// fbChanges is a change request with items.
func fbChanges(lens domain.Lens, round int, summary string, items ...domain.FeedbackItem) domain.Verdict {
	return domain.Verdict{Lens: lens, Round: round, Outcome: domain.VerdictChanges, Summary: summary, Items: items}
}

// fbScenarioA is P6-04 (a): every tester approves in round 1.
func fbScenarioA(c *fbClock) []events.Event {
	const bead domain.BeadID = "sc-a1b2"
	return []events.Event{
		events.BeadCreated{Meta: c.next(), Bead: fbBead(bead, "Retry failed HTTP requests", domain.BeadStateClosed, 1)},
		events.RoundStarted{Meta: c.next(), Bead: bead, Round: 1, RoundKind: events.RoundReview, Lenses: fbAllLenses, Commit: "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "eng-1", Report: fbReport("retry 5xx with capped exponential backoff")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "ux-1", Verdict: fbApprove(domain.LensUX, 1, "the error message still reads well")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "qa-1", Verdict: fbApprove(domain.LensQA, 1, "reproduced the flake, now green")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "test-1", Verdict: fbApprove(domain.LensTest, 1, "table test covers the cap")},
	}
}

// fbScenarioB is P6-04 (b): one tester requests changes, the engineer fixes
// them, the tester approves in round 2.
func fbScenarioB(c *fbClock) []events.Event {
	const bead domain.BeadID = "sc-c3d4"
	item := fbItem("qa-r1-1", domain.SeverityBlocker, "Retry loops forever on a 500", domain.ItemResolved)
	return []events.Event{
		events.BeadCreated{Meta: c.next(), Bead: fbBead(bead, "Cache DNS lookups", domain.BeadStateApproved, 2)},
		events.RoundStarted{Meta: c.next(), Bead: bead, Round: 1, RoundKind: events.RoundReview, Lenses: fbAllLenses, Commit: "2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e"},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "eng-2", Report: fbReport("cache lookups for the TTL the resolver reports")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "ux-1", Verdict: fbApprove(domain.LensUX, 1, "no user-visible change")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "qa-1", Verdict: fbChanges(domain.LensQA, 1, "one blocker", item)},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "test-1", Verdict: fbApprove(domain.LensTest, 1, "coverage is fine")},
		events.RoundStarted{Meta: c.next(), Bead: bead, Round: 2, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensQA}, Commit: "3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f"},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "eng-2", Responses: []domain.Response{
			{ItemID: item.ID, Action: domain.ActionFixed, Rationale: "capped the loop at five attempts"},
		}, Report: fbReport("cap the retry loop")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "qa-1", Verdict: fbApprove(domain.LensQA, 2, "the cap holds")},
	}
}

// fbScenarioC is P6-04 (c): the engineer pushes back and the tester accepts.
func fbScenarioC(c *fbClock) []events.Event {
	const bead domain.BeadID = "sc-e5f6"
	item := fbItem("ux-r1-1", domain.SeverityMinor, "Spell the flag --no-cache", domain.ItemAccepted)
	nit := fbItem("ux-r1-2", domain.SeverityNit, "Capitalise the help text", domain.ItemAccepted)
	return []events.Event{
		events.BeadCreated{Meta: c.next(), Bead: fbBead(bead, "Add a --cache flag", domain.BeadStateApproved, 2)},
		events.RoundStarted{Meta: c.next(), Bead: bead, Round: 1, RoundKind: events.RoundReview, Lenses: fbAllLenses, Commit: "4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f70"},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "eng-3", Report: fbReport("add the flag and document it")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "ux-1", Verdict: fbChanges(domain.LensUX, 1, "naming", item, nit)},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "qa-1", Verdict: fbApprove(domain.LensQA, 1, "behaviour is right")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "test-1", Verdict: fbApprove(domain.LensTest, 1, "covered")},
		events.RoundStarted{Meta: c.next(), Bead: bead, Round: 2, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensUX}, Commit: "5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f7081"},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "eng-3", Responses: []domain.Response{
			{ItemID: item.ID, Action: domain.ActionPushback, Rationale: "every other flag in the CLI is positive; --cache=false already works"},
			{ItemID: nit.ID, Action: domain.ActionWontfix, Rationale: "cobra lower-cases every other help string"},
		}},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "ux-1", Verdict: fbApprove(domain.LensUX, 2, "fair, the rationale holds")},
	}
}

// fbScenarioD is P6-04 (d): the pushback is held, the product owner upholds
// the engineer and the bead is approved.
func fbScenarioD(c *fbClock) []events.Event {
	const bead domain.BeadID = "sc-g7h8"
	raised := fbItem("qa-r1-1", domain.SeverityMajor, "Validate the timeout before use", domain.ItemOpen)
	held := raised
	held.Status = domain.ItemHeld
	settled := raised
	settled.Status = domain.ItemOverruled
	return []events.Event{
		events.BeadCreated{Meta: c.next(), Bead: fbBead(bead, "Bump the client timeout", domain.BeadStateApproved, 3)},
		events.RoundStarted{Meta: c.next(), Bead: bead, Round: 1, RoundKind: events.RoundReview, Lenses: fbAllLenses, Commit: "6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192"},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "eng-4", Report: fbReport("raise the timeout to 30s")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "qa-1", Verdict: fbChanges(domain.LensQA, 1, "one major", raised)},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "ux-1", Verdict: fbApprove(domain.LensUX, 1, "fine")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "test-1", Verdict: fbApprove(domain.LensTest, 1, "fine")},
		events.RoundStarted{Meta: c.next(), Bead: bead, Round: 2, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensQA}, Commit: "708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3"},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "eng-4", Responses: []domain.Response{
			{ItemID: raised.ID, Action: domain.ActionPushback, Rationale: "the config layer already rejects a negative timeout"},
		}},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "qa-1", Verdict: fbChanges(domain.LensQA, 2, "still want the guard", held)},
		events.ArbitrationReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "po-1", Rulings: []domain.Arbitration{
			{ItemID: raised.ID, Ruling: domain.RulingUpholdEngineer, Rationale: "config validation covers it; a second check is dead code"},
		}},
		events.RoundStarted{Meta: c.next(), Bead: bead, Round: 3, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensQA}, Commit: "8192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4"},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 3, Agent: "qa-1", Verdict: domain.Verdict{
			Lens: domain.LensQA, Round: 3, Outcome: domain.VerdictApprove, Summary: "ruling accepted", Items: []domain.FeedbackItem{settled},
		}},
	}
}

// fbScenarioE is P6-04 (e): the product owner upholds the tester and the
// engineer fixes the item.
func fbScenarioE(c *fbClock) []events.Event {
	const bead domain.BeadID = "sc-i9j0"
	raised := fbItem("test-r1-1", domain.SeverityBlocker, "No test for the empty-body case", domain.ItemOpen)
	raised.Category = domain.CategoryRules
	held := raised
	held.Status = domain.ItemHeld
	fixed := raised
	fixed.Status = domain.ItemResolved
	return []events.Event{
		events.BeadCreated{Meta: c.next(), Bead: fbBead(bead, "Stream the response body", domain.BeadStateInReview, 3)},
		events.RoundStarted{Meta: c.next(), Bead: bead, Round: 1, RoundKind: events.RoundReview, Lenses: fbAllLenses, Commit: "92a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5"},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "eng-5", Report: fbReport("stream instead of buffering")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "ux-1", Verdict: fbApprove(domain.LensUX, 1, "no interface change")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "qa-1", Verdict: fbApprove(domain.LensQA, 1, "streams as advertised")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "test-1", Verdict: fbChanges(domain.LensTest, 1, "missing coverage", raised)},
		events.RoundStarted{Meta: c.next(), Bead: bead, Round: 2, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensTest}, Commit: "a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6"},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "eng-5", Responses: []domain.Response{
			{ItemID: raised.ID, Action: domain.ActionWontfix, Rationale: "the empty body is unreachable through the public API"},
		}},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "test-1", Verdict: fbChanges(domain.LensTest, 2, "rules say every branch is tested", held)},
		events.ArbitrationReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "po-1", Rulings: []domain.Arbitration{
			{ItemID: raised.ID, Ruling: domain.RulingUpholdTester, Instruction: "add the test with a hand-built reader", Rationale: "a rules item is never overruled"},
		}},
		events.RoundStarted{Meta: c.next(), Bead: bead, Round: 3, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensTest}, Commit: "b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7"},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 3, Agent: "eng-5", Responses: []domain.Response{
			{ItemID: raised.ID, Action: domain.ActionFixed, Rationale: "added TestStreamEmptyBody"},
		}, Report: fbReport("add the empty-body test")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 3, Agent: "test-1", Verdict: domain.Verdict{
			Lens: domain.LensTest, Round: 3, Outcome: domain.VerdictApprove, Summary: "covered now", Items: []domain.FeedbackItem{fixed},
		}},
	}
}

// fbScenarioF is P6-04 (f): the round cap is reached and the product owner
// blocks the bead for a human.
func fbScenarioF(c *fbClock) []events.Event {
	const bead domain.BeadID = "sc-k1l2"
	out := []events.Event{
		events.BeadCreated{Meta: c.next(), Bead: fbBead(bead, "Rewrite the connection pool", domain.BeadStateNeedsHuman, 5)},
	}
	for round := 1; round <= 5; round++ {
		item := fbItem(
			"qa-r"+string(rune('0'+round))+"-1",
			domain.SeverityBlocker,
			"Pool still leaks a connection per retry",
			domain.ItemOpen,
		)
		out = append(out,
			events.RoundStarted{Meta: c.next(), Bead: bead, Round: round, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensQA}, Commit: strings.Repeat(string(rune('a'+round-1)), 40)},
			events.ResponseReceived{Meta: c.next(), Bead: bead, Round: round, Agent: "eng-6", Report: fbReport("attempt " + string(rune('0'+round)) + " at the pool")},
			events.VerdictReceived{Meta: c.next(), Bead: bead, Round: round, Agent: "qa-1", Verdict: fbChanges(domain.LensQA, round, "still leaking", item)},
		)
	}
	return append(out, events.ArbitrationReceived{Meta: c.next(), Bead: bead, Round: 5, Agent: "po-1", Escalation: &domain.Escalation{
		Outcome: domain.EscalationBlock,
		Summary: "five rounds without a fix; the pool needs a design a human signs off",
	}})
}

// fbScenarioG is P6-04 (g): the diff moved enough that every tester
// re-reviews (D-09), and one of them has not answered yet.
func fbScenarioG(c *fbClock) []events.Event {
	const bead domain.BeadID = "sc-m3n4"
	item := fbItem("qa-r1-1", domain.SeverityMajor, "Rebase dropped the timeout guard", domain.ItemResolved)
	return []events.Event{
		events.BeadCreated{Meta: c.next(), Bead: fbBead(bead, "Split the transport package", domain.BeadStateInReview, 2)},
		events.RoundStarted{Meta: c.next(), Bead: bead, Round: 1, RoundKind: events.RoundReview, Lenses: fbAllLenses, Commit: "c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8"},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "eng-7", Report: fbReport("move the transport into its own package")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "ux-1", Verdict: fbApprove(domain.LensUX, 1, "nothing user facing")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "qa-1", Verdict: fbChanges(domain.LensQA, 1, "one major", item)},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "test-1", Verdict: fbApprove(domain.LensTest, 1, "tests moved with it")},
		// The fix rewrote more than re_review_threshold of the diff, so all
		// three lenses look again (D-09).
		events.RoundStarted{Meta: c.next(), Bead: bead, Round: 2, RoundKind: events.RoundReview, Lenses: fbAllLenses, Commit: "d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f809"},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "eng-7", Responses: []domain.Response{
			{ItemID: item.ID, Action: domain.ActionFixed, Rationale: "restored the guard and moved it into the new package"},
		}, Report: fbReport("restore the guard")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "ux-1", Verdict: fbApprove(domain.LensUX, 2, "still nothing user facing")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "qa-1", Verdict: fbApprove(domain.LensQA, 2, "guard is back")},
	}
}

// fbFixtureEvents is the whole recorded history the golden frames render:
// one bead per P6-04 scenario, in scenario order.
func fbFixtureEvents() []events.Event {
	c := &fbClock{}
	out := []events.Event{events.RunStarted{Meta: c.next(), Info: fbRunInfo()}}
	for _, scenario := range []func(*fbClock) []events.Event{
		fbScenarioA, fbScenarioB, fbScenarioC, fbScenarioD, fbScenarioE, fbScenarioF, fbScenarioG,
	} {
		out = append(out, scenario(c)...)
	}
	return out
}

// fbState folds evs into a fresh state for the run.
func fbState(evs []events.Event) *State {
	st := NewState(fbRunInfo())
	for _, ev := range evs {
		st.Apply(ev)
	}
	return st
}

// fbFixtureState is the state every golden frame is rendered from.
func fbFixtureState() *State { return fbState(fbFixtureEvents()) }

// fbView builds the view sized to the main pane of a w×h terminal.
func fbView(w, h int, opts FeedbackOptions) FeedbackModel {
	opts.NoColor = true
	return NewFeedbackModel(opts).Resize(w, h-chromeHeight)
}

// fbAtCursor returns the view with its cursor on the bead with the given id.
func fbAtCursor(t *testing.T, v FeedbackModel, st *State, bead domain.BeadID) FeedbackModel {
	t.Helper()
	for range len(FeedbackThreads(st)) {
		if th, ok := v.Selected(st); ok && th.Bead == bead {
			return v
		}
		v, _ = v.Update(fbKey("down"), st, DefaultKeyMap())
	}
	th, ok := v.Selected(st)
	require.True(t, ok, "no threads to select")
	require.Equal(t, bead, th.Bead, "bead %s is not in the fixture", bead)
	return v
}

// fbFrame renders the whole terminal frame with the Feedback view in the
// main pane: what [Model.View] produces once it dispatches [TabFeedback] to
// a [FeedbackModel].
func fbFrame(t *testing.T, v FeedbackModel, st *State, w, h int) string {
	t.Helper()
	m := New(t.Context(), Options{Run: st.Run, NoColor: true})
	m.state = st
	m.tab = TabFeedback
	m.width, m.height = w, h
	return strings.Join([]string{
		m.header(w),
		m.tabBar(w),
		m.styles.Rule.Render(strings.Repeat("─", max(w, 1))),
		v.View(st),
		m.footer(w),
	}, "\n")
}

// fbGolden compares got with testdata/tui/<name>.golden; UPDATE_GOLDEN=1
// (make update-golden) rewrites it instead.
func fbGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(fbGoldenDir, name+".golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "missing golden %s; run `make update-golden`", path)
	require.Equal(t, string(want), got, "output differs from %s; run `make update-golden` if the change is intended", path)
}

// fbKey builds a key press.
func fbKey(s string) tea.KeyMsg {
	switch s {
	case "up", "down", "enter", "home", "end", "pgup", "pgdown":
		return tea.KeyMsg{Type: map[string]tea.KeyType{
			"up": tea.KeyUp, "down": tea.KeyDown, "enter": tea.KeyEnter,
			"home": tea.KeyHome, "end": tea.KeyEnd, "pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown,
		}[s]}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// TestFeedbackViewGolden is the P7-04 acceptance test: golden frames from
// the recorded events of every P6-04 scenario.
func TestFeedbackViewGolden(t *testing.T) {
	cases := []struct {
		name string
		bead domain.BeadID
	}{
		{"feedback", "sc-a1b2"},             // (a) all approve in round 1
		{"feedback-changes", "sc-c3d4"},     // (b) changes, fixed, approved
		{"feedback-pushback", "sc-e5f6"},    // (c) pushback accepted
		{"feedback-arbitration", "sc-g7h8"}, // (d) held → uphold engineer
		{"feedback-rules", "sc-i9j0"},       // (e) held → uphold tester → fixed
		{"feedback-rereview", "sc-m3n4"},    // (g) full re-review, one pending
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := fbFixtureState()
			v := fbAtCursor(t, fbView(110, 26, FeedbackOptions{}), st, tc.bead)
			fbGolden(t, tc.name, fbFrame(t, v, st, 110, 26))
		})
	}

	// (f) round cap → escalation → block, long enough to scroll.
	t.Run("escalation", func(t *testing.T) {
		st := fbFixtureState()
		v := fbAtCursor(t, fbView(110, 26, FeedbackOptions{}), st, "sc-k1l2")
		fbGolden(t, "feedback-escalation", fbFrame(t, v, st, 110, 26))
	})

	t.Run("scrolled to the escalation", func(t *testing.T) {
		st := fbFixtureState()
		v := fbAtCursor(t, fbView(110, 26, FeedbackOptions{}), st, "sc-k1l2")
		v, ok := v.Update(fbKey("end"), st, DefaultKeyMap())
		require.True(t, ok)
		require.Positive(t, v.Offset())
		fbGolden(t, "feedback-scrolled", fbFrame(t, v, st, 110, 26))
	})

	t.Run("details", func(t *testing.T) {
		st := fbFixtureState()
		v := fbAtCursor(t, fbView(110, 26, FeedbackOptions{}), st, "sc-i9j0")
		v, ok := v.Update(fbKey("enter"), st, DefaultKeyMap())
		require.True(t, ok)
		require.True(t, v.ShowingDetails())
		fbGolden(t, "feedback-details", fbFrame(t, v, st, 110, 26))
	})

	t.Run("narrow", func(t *testing.T) {
		st := fbFixtureState()
		v := fbAtCursor(t, fbView(64, 20, FeedbackOptions{}), st, "sc-c3d4")
		fbGolden(t, "feedback-narrow", fbFrame(t, v, st, 64, 20))
	})

	t.Run("empty", func(t *testing.T) {
		st := fbState([]events.Event{events.RunStarted{Meta: fbMeta(0), Info: fbRunInfo()}})
		fbGolden(t, "feedback-empty", fbFrame(t, fbView(110, 26, FeedbackOptions{}), st, 110, 26))
	})
}

// TestFeedbackThreadsScenarios checks what the view derives from each P6-04
// scenario: where every tester stands, how items were settled, and whether
// the bead came out approved or escalated.
func TestFeedbackThreadsScenarios(t *testing.T) {
	st := fbFixtureState()
	threads := FeedbackThreads(st)
	require.Len(t, threads, 7, "one thread per scenario, in scenario order")

	byBead := make(map[domain.BeadID]FeedbackThread, len(threads))
	order := make([]domain.BeadID, 0, len(threads))
	for _, th := range threads {
		byBead[th.Bead] = th
		order = append(order, th.Bead)
	}
	require.Equal(t, []domain.BeadID{"sc-a1b2", "sc-c3d4", "sc-e5f6", "sc-g7h8", "sc-i9j0", "sc-k1l2", "sc-m3n4"}, order)

	t.Run("a all approve in round one", func(t *testing.T) {
		th := byBead["sc-a1b2"]
		require.Len(t, th.Rounds, 1)
		require.True(t, th.Approved)
		require.Zero(t, th.Open)
		require.Empty(t, th.Items)
		require.Equal(t, []domain.Lens{domain.LensUX, domain.LensQA, domain.LensTest}, fbLenses(th))
		for _, tester := range th.Testers {
			require.True(t, tester.Satisfied(), "%s", tester.Lens)
			require.Equal(t, "✓", tester.Glyph())
		}
	})

	t.Run("b changes then approved in round two", func(t *testing.T) {
		th := byBead["sc-c3d4"]
		require.Len(t, th.Rounds, 2)
		require.True(t, th.Approved)
		require.Zero(t, th.Open)
		item, ok := th.Item("qa-r1-1")
		require.True(t, ok)
		require.Equal(t, domain.ItemResolved, item.Status)
		require.NotNil(t, item.Response)
		require.Equal(t, domain.ActionFixed, item.Response.Action)
		require.Nil(t, item.Ruling)
		require.Equal(t, 1, item.Round, "the item belongs to the round it was raised in")
	})

	t.Run("c pushback accepted", func(t *testing.T) {
		th := byBead["sc-e5f6"]
		require.True(t, th.Approved)
		item, ok := th.Item("ux-r1-1")
		require.True(t, ok)
		require.Equal(t, domain.ActionPushback, item.Response.Action)
		require.Equal(t, domain.ItemAccepted, item.Status)
		require.False(t, item.Blocking())
		nit, ok := th.Item("ux-r1-2")
		require.True(t, ok)
		require.Equal(t, domain.ActionWontfix, nit.Response.Action)
		require.Equal(t, domain.SeverityNit, nit.Severity)
	})

	t.Run("d pushback held then engineer upheld", func(t *testing.T) {
		th := byBead["sc-g7h8"]
		require.Len(t, th.Rounds, 3)
		require.True(t, th.Approved)
		require.Zero(t, th.Open, "the overruled item no longer counts")
		item, ok := th.Item("qa-r1-1")
		require.True(t, ok)
		require.Equal(t, domain.ItemOverruled, item.Status, "the latest occurrence says where it stands")
		require.Equal(t, 3, item.Round)
		require.Nil(t, item.Response, "nothing was said about the occurrence that closed the item")
		require.Nil(t, item.Ruling)

		raised, ok := th.Answered(2, "qa-r1-1")
		require.True(t, ok)
		require.Equal(t, 1, raised.Round, "round 2's response answers the item round 1 raised")
		require.Equal(t, domain.ActionPushback, raised.Response.Action)
		require.Equal(t, 2, raised.ResponseRound)

		held, ok := th.Ruled(2, "qa-r1-1")
		require.True(t, ok)
		require.Equal(t, 2, held.Round, "the ruling settles the occurrence the tester held")
		require.Equal(t, domain.ItemHeld, held.Status)
		require.Equal(t, domain.RulingUpholdEngineer, held.Ruling.Ruling)
		require.Equal(t, 2, held.RulingRound)
	})

	t.Run("e tester upheld then fixed", func(t *testing.T) {
		th := byBead["sc-i9j0"]
		require.Len(t, th.Rounds, 3)
		require.True(t, th.Approved)
		item, ok := th.Item("test-r1-1")
		require.True(t, ok)
		require.Equal(t, domain.CategoryRules, item.Category)
		require.Equal(t, domain.ItemResolved, item.Status)

		// The pushback that caused the arbitration was given in round 2 and
		// stays on the occurrence it answered: round 3's fix does not
		// overwrite it.
		raised, ok := th.Answered(2, "test-r1-1")
		require.True(t, ok)
		require.Equal(t, 1, raised.Round)
		require.Equal(t, domain.ActionWontfix, raised.Response.Action)
		require.Equal(t, "the empty body is unreachable through the public API", raised.Response.Rationale)
		require.Equal(t, 2, raised.ResponseRound)

		held, ok := th.Ruled(2, "test-r1-1")
		require.True(t, ok)
		require.Equal(t, 2, held.Round)
		require.Equal(t, domain.ItemHeld, held.Status)
		require.Equal(t, domain.RulingUpholdTester, held.Ruling.Ruling)
		require.Equal(t, 2, held.RulingRound)
		require.Equal(t, domain.ActionFixed, held.Response.Action, "round 3 answered the held occurrence")
		require.Equal(t, 3, held.ResponseRound)

		_, ok = th.Answered(1, "test-r1-1")
		require.False(t, ok, "nothing was raised before round 1")
		_, ok = th.Ruled(2, "no-such-item")
		require.False(t, ok)
	})

	t.Run("f round cap escalates", func(t *testing.T) {
		th := byBead["sc-k1l2"]
		require.Len(t, th.Rounds, 5)
		require.False(t, th.Approved)
		require.Equal(t, 5, th.Open, "one open blocker per round")
		require.NotNil(t, th.Escalation)
		require.Equal(t, domain.EscalationBlock, th.Escalation.Outcome)
		last, ok := th.Latest()
		require.True(t, ok)
		require.Equal(t, 5, last.Number)
	})

	t.Run("g full re-review with one tester still out", func(t *testing.T) {
		th := byBead["sc-m3n4"]
		require.Len(t, th.Rounds, 2)
		require.Equal(t, fbAllLenses, th.Rounds[1].Lenses, "D-09 forced everyone back")
		require.False(t, th.Approved, "test has not answered round 2")
		byLens := fbTesters(th)
		require.True(t, byLens[domain.LensUX].Satisfied())
		require.True(t, byLens[domain.LensQA].Satisfied())
		require.True(t, byLens[domain.LensTest].Pending)
		require.False(t, byLens[domain.LensTest].Satisfied())
		require.Equal(t, "◐", byLens[domain.LensTest].Glyph())
		require.Equal(t, "reviewing", byLens[domain.LensTest].String())
	})
}

// fbLenses lists a thread's testers in order.
func fbLenses(th FeedbackThread) []domain.Lens {
	out := make([]domain.Lens, 0, len(th.Testers))
	for _, t := range th.Testers {
		out = append(out, t.Lens)
	}
	return out
}

// fbTesters indexes a thread's testers by lens.
func fbTesters(th FeedbackThread) map[domain.Lens]TesterStatus {
	out := make(map[domain.Lens]TesterStatus, len(th.Testers))
	for _, t := range th.Testers {
		out[t.Lens] = t
	}
	return out
}

// TestFeedbackThreadsEdgeCases covers the states the scenarios do not reach:
// no state at all, a round nobody has answered, an unknown lens and a bead
// the graph never described.
func TestFeedbackThreadsEdgeCases(t *testing.T) {
	require.Nil(t, FeedbackThreads(nil))
	require.Empty(t, FeedbackThreads(NewState(fbRunInfo())))

	st := fbState([]events.Event{
		events.RunStarted{Meta: fbMeta(0), Info: fbRunInfo()},
		events.RoundStarted{Meta: fbMeta(1), Bead: "sc-zzzz", Round: 1, RoundKind: events.RoundConflict, Lenses: []domain.Lens{domain.LensQA, "security"}},
	})
	threads := FeedbackThreads(st)
	require.Len(t, threads, 1)
	th := threads[0]
	require.Empty(t, th.Title, "no BeadCreated was seen")
	require.Empty(t, th.BeadState)
	require.False(t, th.Approved)
	require.Equal(t, []domain.Lens{domain.LensQA, domain.Lens("security")}, fbLenses(th), "unknown lenses sort after the known ones")
	for _, tester := range th.Testers {
		require.True(t, tester.Pending)
		require.Equal(t, "◐", tester.Glyph())
	}

	// A verdict for a round nobody started still creates the round.
	st.Apply(events.VerdictReceived{Meta: fbMeta(2), Bead: "sc-yyyy", Round: 2, Agent: "qa-9", Verdict: fbApprove(domain.LensQA, 2, "fine")})
	threads = FeedbackThreads(st)
	require.Len(t, threads, 2)
	require.True(t, threads[1].Approved)
	require.Equal(t, "waiting", TesterStatus{Lens: domain.LensQA}.String())
	require.Equal(t, "·", TesterStatus{Lens: domain.LensQA}.Glyph())
	require.Equal(t, "changes", TesterStatus{Lens: domain.LensQA, Outcome: domain.VerdictChanges}.String())
	require.Equal(t, "✗", TesterStatus{Lens: domain.LensQA, Outcome: domain.VerdictChanges}.Glyph())

	_, ok := FeedbackThread{}.Latest()
	require.False(t, ok)
	_, ok = FeedbackThread{}.Item("nope")
	require.False(t, ok)
	require.True(t, FeedbackItemView{FeedbackItem: domain.FeedbackItem{Severity: domain.SeverityMajor, Status: domain.ItemOpen}}.Blocking())
	require.False(t, FeedbackItemView{FeedbackItem: domain.FeedbackItem{Severity: domain.SeverityNit, Status: domain.ItemOpen}}.Blocking())
	require.False(t, FeedbackItemView{FeedbackItem: domain.FeedbackItem{Severity: domain.SeverityBlocker, Status: domain.ItemResolved}}.Blocking())
}

// TestFeedbackViewLayout checks the pane is always exactly the size it was
// resized to, at every width, and that it survives a state with nothing in it.
func TestFeedbackViewLayout(t *testing.T) {
	st := fbFixtureState()
	for _, size := range []struct{ w, h int }{
		{110, 26}, {100, 24}, {80, 24}, {64, 20}, {40, 12}, {20, 6}, {1, 1},
	} {
		v := fbView(size.w, size.h, FeedbackOptions{})
		out := v.View(st)
		lines := strings.Split(out, "\n")
		wantW, wantH := v.Size()
		require.Len(t, lines, wantH, "height at %dx%d", size.w, size.h)
		for i, l := range lines {
			require.LessOrEqual(t, lipgloss.Width(l), wantW, "line %d too wide at %dx%d: %q", i, size.w, size.h, l)
		}
	}

	t.Run("empty and nil states", func(t *testing.T) {
		v := fbView(80, 24, FeedbackOptions{})
		empty := v.View(NewState(fbRunInfo()))
		require.Contains(t, empty, emptyText[TabFeedback])
		require.Len(t, strings.Split(empty, "\n"), 24-chromeHeight)
		require.Len(t, strings.Split(v.View(nil), "\n"), 24-chromeHeight)
		_, ok := v.Selected(nil)
		require.False(t, ok)
	})

	t.Run("wide shows the bead list, narrow the selector", func(t *testing.T) {
		wide := fbView(110, 26, FeedbackOptions{}).View(st)
		require.Contains(t, wide, "BEAD", "the list column has a header")
		require.Contains(t, wide, "U Q T", "one tick column per lens")
		require.Contains(t, wide, "sc-c3d4", "other beads are listed")

		narrow := fbView(60, 20, FeedbackOptions{}).View(st)
		require.NotContains(t, narrow, "U Q T")
		require.Contains(t, narrow, "1/7", "the selector says which bead is shown")
	})

	t.Run("content of the selected thread", func(t *testing.T) {
		v := fbAtCursor(t, fbView(110, 26, FeedbackOptions{}), st, "sc-c3d4")
		out := v.View(st)
		require.Contains(t, out, "round 1")
		require.Contains(t, out, "round 2")
		require.Contains(t, out, "blocker")
		require.Contains(t, out, "Retry loops forever on a 500")
		require.Contains(t, out, "→ fixed", "the engineer's response is inline")
		require.Contains(t, out, "[resolved]")
		require.Contains(t, out, "⚙ report")
	})

	t.Run("the newest round names the tester it waits for", func(t *testing.T) {
		v := fbAtCursor(t, fbView(110, 26, FeedbackOptions{}), st, "sc-m3n4")
		require.Contains(t, v.View(st), "◐ test reviewing")
	})

	t.Run("markers for pushback, arbitration and escalation", func(t *testing.T) {
		v := fbAtCursor(t, fbView(110, 26, FeedbackOptions{}), st, "sc-g7h8")
		out := v.View(st)
		require.Contains(t, out, "↩ pushback")
		require.Contains(t, out, "⚖ uphold_engineer")
		require.Contains(t, out, "[held]")

		v = fbAtCursor(t, fbView(110, 40, FeedbackOptions{}), st, "sc-e5f6")
		require.Contains(t, v.View(st), "⊘ wontfix")

		v = fbAtCursor(t, fbView(110, 40, FeedbackOptions{}), st, "sc-i9j0")
		out = v.View(st)
		require.Contains(t, out, "⚖ uphold_tester")
		require.Contains(t, out, "(rules)")

		v = fbAtCursor(t, fbView(110, 26, FeedbackOptions{}), st, "sc-k1l2")
		v, _ = v.Update(fbKey("end"), st, DefaultKeyMap())
		out = v.View(st)
		require.Contains(t, out, "▲ escalation block")
		require.Contains(t, out, "needs a design a human signs off")
	})

	t.Run("details add the file, detail and rationale", func(t *testing.T) {
		v := fbAtCursor(t, fbView(110, 40, FeedbackOptions{Details: true}), st, "sc-i9j0")
		require.True(t, v.ShowingDetails())
		out := v.View(st)
		require.Contains(t, out, "internal/http/retry.go:42")
		require.Contains(t, out, "suggestion:", "an item with a suggestion shows it")
		require.Contains(t, out, "files: internal/http/retry.go")
		require.Contains(t, out, "verify: go test ./internal/http/...")
	})
}

// TestFeedbackKeys checks the cursor, the scroll offset and the details
// toggle, and that unrelated keys are left to the global bindings.
func TestFeedbackKeys(t *testing.T) {
	st := fbFixtureState()
	keys := DefaultKeyMap()
	n := len(FeedbackThreads(st))

	t.Run("up and down move between beads", func(t *testing.T) {
		v := fbView(110, 26, FeedbackOptions{})
		v, ok := v.Update(fbKey("down"), st, keys)
		require.True(t, ok)
		require.Equal(t, 1, v.Cursor())
		th, ok := v.Selected(st)
		require.True(t, ok)
		require.Equal(t, domain.BeadID("sc-c3d4"), th.Bead)

		v, _ = v.Update(fbKey("up"), st, keys)
		require.Equal(t, 0, v.Cursor())
		v, _ = v.Update(fbKey("up"), st, keys)
		require.Equal(t, 0, v.Cursor(), "the cursor stops at the first bead")

		for range n + 3 {
			v, _ = v.Update(fbKey("down"), st, keys)
		}
		require.Equal(t, n-1, v.Cursor(), "and at the last")
	})

	t.Run("scrolling is clamped to the history", func(t *testing.T) {
		v := fbAtCursor(t, fbView(110, 26, FeedbackOptions{}), st, "sc-k1l2")
		require.Zero(t, v.Offset())

		v, ok := v.Update(fbKey("pgup"), st, keys)
		require.True(t, ok)
		require.Zero(t, v.Offset(), "cannot scroll above the first line")

		v, _ = v.Update(fbKey("pgdown"), st, keys)
		require.Positive(t, v.Offset())

		for range 20 {
			v, _ = v.Update(fbKey("pgdown"), st, keys)
		}
		bottom := v.Offset()

		v, _ = v.Update(fbKey("end"), st, keys)
		require.Equal(t, bottom, v.Offset(), "pgdown stops where end lands")

		th, ok := v.Selected(st)
		require.True(t, ok)
		lines := len(v.detailLines(th, v.detailWidth()))
		require.Equal(t, lines-(26-chromeHeight-1), bottom, "the last line of the history is the last line of the pane")

		v, _ = v.Update(fbKey("home"), st, keys)
		require.Zero(t, v.Offset())

		v, _ = v.Update(fbKey("pgdown"), st, keys)
		v, _ = v.Update(fbKey("down"), st, keys)
		require.Zero(t, v.Offset(), "moving to another bead starts at its first round")
	})

	t.Run("a short history does not scroll", func(t *testing.T) {
		v := fbAtCursor(t, fbView(110, 40, FeedbackOptions{}), st, "sc-a1b2")
		v, _ = v.Update(fbKey("end"), st, keys)
		require.Zero(t, v.Offset())
	})

	t.Run("details toggles", func(t *testing.T) {
		v := fbView(110, 26, FeedbackOptions{})
		require.False(t, v.ShowingDetails())
		v, ok := v.Update(fbKey("enter"), st, keys)
		require.True(t, ok)
		require.True(t, v.ShowingDetails())
		v, _ = v.Update(fbKey("d"), st, keys)
		require.False(t, v.ShowingDetails())
	})

	t.Run("everything else is left alone", func(t *testing.T) {
		v := fbView(110, 26, FeedbackOptions{})
		for _, k := range []string{"q", "?", "tab", "1"} {
			_, ok := v.Update(fbKey(k), st, keys)
			require.False(t, ok, "key %q must fall through to the global bindings", k)
		}
		_, ok := v.Update(tea.WindowSizeMsg{Width: 10, Height: 10}, st, keys)
		require.False(t, ok, "only key presses are consumed")
	})

	t.Run("keys work with no threads", func(t *testing.T) {
		empty := NewState(fbRunInfo())
		v := fbView(110, 26, FeedbackOptions{})
		v, ok := v.Update(fbKey("down"), empty, keys)
		require.True(t, ok)
		require.Zero(t, v.Cursor())
		v, _ = v.Update(fbKey("end"), empty, keys)
		require.Zero(t, v.Offset())
	})
}

// TestFeedbackKeyMap checks the view's own bindings and its help.
func TestFeedbackKeyMap(t *testing.T) {
	v := NewFeedbackModel(FeedbackOptions{NoColor: true})
	km := v.Keys()
	require.Equal(t, DefaultFeedbackKeyMap().Details.Keys(), km.Details.Keys())
	require.Equal(t, []string{"enter", "d"}, km.Details.Keys())
	require.Equal(t, []string{"pgup", "ctrl+b"}, km.PageUp.Keys())
	require.Equal(t, []string{"pgdown", "ctrl+f"}, km.PageDown.Keys())
	require.Equal(t, []string{"home", "g"}, km.Top.Keys())
	require.Equal(t, []string{"end", "G"}, km.Bottom.Keys())
	require.Len(t, km.ShortHelp(), 2)
	require.Len(t, km.FullHelp(), 3)
	for _, b := range km.ShortHelp() {
		require.NotEmpty(t, b.Help().Key)
		require.NotEmpty(t, b.Help().Desc)
	}

	custom := FeedbackKeyMap{Details: key.NewBinding(key.WithKeys("x"))}
	v = NewFeedbackModel(FeedbackOptions{NoColor: true, Keys: &custom})
	require.Equal(t, []string{"x"}, v.Keys().Details.Keys())
	st := fbFixtureState()
	v, ok := v.Update(fbKey("x"), st, DefaultKeyMap())
	require.True(t, ok)
	require.True(t, v.ShowingDetails())
}

// TestFeedbackNoColor checks that the NoColor path really is plain text and
// that a theme can be supplied instead.
func TestFeedbackNoColor(t *testing.T) {
	st := fbFixtureState()
	plain := NewFeedbackModel(FeedbackOptions{NoColor: true}).Resize(110, 22).View(st)
	for _, sgr := range []string{"\x1b[3", "\x1b[4", "\x1b[9"} {
		require.NotContains(t, plain, sgr, "NO_COLOR output carries no colour (bold is not a colour)")
	}

	themed := NewFeedbackModel(FeedbackOptions{Theme: ThemeFor(ProfileNoColor)}).Resize(110, 22).View(st)
	require.Equal(t, plain, themed, "an explicit no-colour theme renders the same frame")
}

// fbRoundHead matches a round section heading, wherever the bead list
// column leaves it on the line.
var fbRoundHead = regexp.MustCompile(`round ([0-9]+) · `)

// fbRoundOf reports which round section of a rendered thread the first line
// containing needle sits in, and 0 when the needle is not on screen. It is
// how the tests below pin an answer to the round it was given in.
func fbRoundOf(out, needle string) int {
	round := 0
	for _, line := range strings.Split(out, "\n") {
		plain := fbPlain(line)
		if m := fbRoundHead.FindStringSubmatch(plain); m != nil {
			n, err := strconv.Atoi(m[1])
			if err == nil {
				round = n
			}
		}
		if strings.Contains(plain, needle) {
			return round
		}
	}
	return 0
}

// fbPlain strips the SGR escapes a themed frame carries, so a test can look
// for text that a style split across attributes.
func fbPlain(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// TestFeedbackAnswersKeepTheirRound is the regression test for the round
// attribution of engineer responses and product-owner rulings: every answer
// is drawn in the round it was given, and an item answered again later
// keeps what was said about it the first time (P6-04 scenarios (c), (d) and
// (e)).
func TestFeedbackAnswersKeepTheirRound(t *testing.T) {
	st := fbFixtureState()

	t.Run("e the pushback that caused the arbitration is not overwritten", func(t *testing.T) {
		v := fbAtCursor(t, fbView(110, 40, FeedbackOptions{}), st, "sc-i9j0")
		out := v.View(st)
		require.Contains(t, out, "the empty body is unreachable through the public API",
			"the round-2 wontfix renders")
		require.Equal(t, 2, fbRoundOf(out, "⊘ wontfix"), "it was given in round 2")
		require.Equal(t, 2, fbRoundOf(out, "⚖ uphold_tester"), "the ruling followed in round 2")
		require.Equal(t, 3, fbRoundOf(out, "→ fixed"), "the fix came in round 3")
		require.Equal(t, 1, fbRoundOf(out, "No test for the empty-body case"),
			"the item itself is still raised in round 1")
	})

	t.Run("c the pushback is drawn in the round it was given", func(t *testing.T) {
		v := fbAtCursor(t, fbView(110, 40, FeedbackOptions{}), st, "sc-e5f6")
		out := v.View(st)
		require.Equal(t, 2, fbRoundOf(out, "↩ pushback"))
		require.Equal(t, 2, fbRoundOf(out, "⊘ wontfix"))
		require.Equal(t, 1, fbRoundOf(out, "Spell the flag --no-cache"))
	})

	t.Run("d the ruling is drawn in the round it was made", func(t *testing.T) {
		v := fbAtCursor(t, fbView(110, 40, FeedbackOptions{}), st, "sc-g7h8")
		out := v.View(st)
		require.Equal(t, 2, fbRoundOf(out, "↩ pushback"))
		require.Equal(t, 2, fbRoundOf(out, "⚖ uphold_engineer"))
		require.Equal(t, 3, fbRoundOf(out, "[overruled]"))
	})

	t.Run("the answer names the item it answers", func(t *testing.T) {
		v := fbAtCursor(t, fbView(110, 40, FeedbackOptions{Details: true}), st, "sc-i9j0")
		out := v.View(st)
		require.Contains(t, out, "⊘ wontfix  test-r1-1")
		require.Contains(t, out, "re: round 1 · test · blocker · No test for the empty-body case",
			"details spell out the occurrence answered")
		require.Contains(t, out, "re: round 2 · test · blocker · No test for the empty-body case")
	})

	t.Run("an answer to an item no verdict raised still renders", func(t *testing.T) {
		orphan := fbState([]events.Event{
			events.RunStarted{Meta: fbMeta(0), Info: fbRunInfo()},
			events.RoundStarted{Meta: fbMeta(1), Bead: "sc-orph", Round: 1, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensQA}},
			events.ResponseReceived{Meta: fbMeta(2), Bead: "sc-orph", Round: 1, Agent: "eng-9", Responses: []domain.Response{
				{ItemID: "ghost-1", Action: domain.ActionFixed, Rationale: "answered an item nobody raised"},
			}},
			events.ArbitrationReceived{Meta: fbMeta(3), Bead: "sc-orph", Round: 1, Agent: "po-9", Rulings: []domain.Arbitration{
				{ItemID: "ghost-1", Ruling: domain.RulingCompromise, Instruction: "split the difference"},
			}},
		})
		out := fbView(110, 40, FeedbackOptions{Details: true}).View(orphan)
		require.Contains(t, out, "→ fixed    ghost-1")
		require.Contains(t, out, "⚖ compromise")
		require.NotContains(t, out, "re: round", "there is no occurrence to point back at")
	})
}

// TestFeedbackEscalationOutcomes covers the two D-08 outcomes the seven
// scenarios do not reach: approve_with_notes, which has only notes, and
// narrow_scope, which replaces the acceptance criteria.
func TestFeedbackEscalationOutcomes(t *testing.T) {
	esc := func(e domain.Escalation) *State {
		return fbState([]events.Event{
			events.RunStarted{Meta: fbMeta(0), Info: fbRunInfo()},
			events.RoundStarted{Meta: fbMeta(1), Bead: "sc-esc0", Round: 1, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensQA}},
			events.VerdictReceived{Meta: fbMeta(2), Bead: "sc-esc0", Round: 1, Agent: "qa-1", Verdict: fbChanges(domain.LensQA, 1, "one major",
				fbItem("qa-r1-1", domain.SeverityMajor, "The retry budget is unbounded", domain.ItemOpen))},
			events.ArbitrationReceived{Meta: fbMeta(3), Bead: "sc-esc0", Round: 1, Agent: "po-1", Escalation: &e},
		})
	}

	t.Run("approve_with_notes falls back to the notes", func(t *testing.T) {
		st := esc(domain.Escalation{
			Outcome: domain.EscalationApproveWithNotes,
			Notes:   "fine at this traffic",
		})
		out := fbView(110, 30, FeedbackOptions{}).View(st)
		require.Contains(t, out, "▲ escalation approve_with_notes")
		require.Contains(t, out, "fine at this traffic", "the notes are the only text there is")
		th, ok := fbView(110, 30, FeedbackOptions{}).Selected(st)
		require.True(t, ok)
		require.Equal(t, domain.EscalationApproveWithNotes, th.Escalation.Outcome)
	})

	t.Run("narrow_scope falls back to the new acceptance", func(t *testing.T) {
		st := esc(domain.Escalation{
			Outcome:       domain.EscalationNarrowScope,
			NewAcceptance: "retry only idempotent verbs",
		})
		out := fbView(110, 30, FeedbackOptions{}).View(st)
		require.Contains(t, out, "▲ escalation narrow_scope")
		require.Contains(t, out, "retry only idempotent verbs")
		require.NotContains(t, out, "acceptance:", "there is nothing else to say")
	})

	t.Run("details spell the new acceptance out under a summary", func(t *testing.T) {
		st := esc(domain.Escalation{
			Outcome:       domain.EscalationNarrowScope,
			Summary:       "three rounds on a budget nobody agreed",
			NewAcceptance: "retry only idempotent verbs",
		})
		plain := fbView(110, 30, FeedbackOptions{}).View(st)
		require.Contains(t, plain, "three rounds on a budget nobody agreed")
		require.NotContains(t, plain, "acceptance:", "the long form is behind the details toggle")

		out := fbView(110, 30, FeedbackOptions{Details: true}).View(st)
		require.Contains(t, out, "acceptance: retry only idempotent verbs")
	})

	t.Run("the summary line counts an escalated bead however it ended", func(t *testing.T) {
		for _, outcome := range domain.EscalationOutcomes() {
			st := esc(domain.Escalation{Outcome: outcome, Summary: "ruled"})
			require.Contains(t, fbView(110, 30, FeedbackOptions{}).View(st), "1 escalated", "%s", outcome)
		}
	})
}

// TestFeedbackStatusStyle checks every item status is coloured, including a
// status this build does not know.
func TestFeedbackStatusStyle(t *testing.T) {
	th := ThemeFor(ProfileTrueColor)
	for _, s := range domain.ItemStatuses() {
		require.NotEmpty(t, feedbackStatusStyle(th, s).Render(s.String()), "%s", s)
	}
	require.Equal(t,
		feedbackStatusStyle(th, domain.ItemOpen).Render("x"),
		feedbackStatusStyle(th, domain.ItemStatus("from-a-newer-build")).Render("x"),
		"an unknown status reads as unsettled")
}

// TestFeedbackViewScrollsTheBeadList covers a run with more reviewed beads
// than the list column has rows: the cursor stays visible and the beads
// above it scroll off.
func TestFeedbackViewScrollsTheBeadList(t *testing.T) {
	evs := []events.Event{events.RunStarted{Meta: fbMeta(0), Info: fbRunInfo()}}
	c := &fbClock{}
	ids := make([]domain.BeadID, 0, 12)
	for i := range 12 {
		id := domain.BeadID(fmt.Sprintf("sc-l%03d", i))
		ids = append(ids, id)
		evs = append(evs,
			events.BeadCreated{Meta: c.next(), Bead: fbBead(id, fmt.Sprintf("Reviewed bead %d", i), domain.BeadStateClosed, 1)},
			events.RoundStarted{Meta: c.next(), Bead: id, Round: 1, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensQA}},
			events.VerdictReceived{Meta: c.next(), Bead: id, Round: 1, Agent: "qa-1", Verdict: fbApprove(domain.LensQA, 1, "fine")},
		)
	}
	st := fbState(evs)
	require.Len(t, FeedbackThreads(st), len(ids))

	v := fbView(110, 12, FeedbackOptions{})
	top := v.View(st)
	require.Contains(t, top, string(ids[0]))
	require.NotContains(t, top, string(ids[len(ids)-1]), "the last bead is below the fold")

	v = fbAtCursor(t, v, st, ids[len(ids)-1])
	bottom := v.View(st)
	require.Contains(t, bottom, string(ids[len(ids)-1]), "the cursor stays visible")
	require.NotContains(t, bottom, string(ids[0]), "the list scrolled past the first bead")
}

// TestFeedbackViewSparseRounds covers a round nobody has said anything in:
// no lenses, no report and no verdicts.
func TestFeedbackViewSparseRounds(t *testing.T) {
	st := fbState([]events.Event{
		events.RunStarted{Meta: fbMeta(0), Info: fbRunInfo()},
		events.RoundStarted{Meta: fbMeta(1), Bead: "sc-qui1", Round: 1, RoundKind: events.RoundReview},
	})
	out := fbView(110, 20, FeedbackOptions{}).View(st)
	require.Contains(t, out, "no testers yet", "nobody was asked to review")
	require.Contains(t, out, "waiting for verdicts", "and nothing was said in the round")

	narrow := fbView(50, 20, FeedbackOptions{}).View(st)
	require.Contains(t, narrow, "no testers yet", "the narrow selector says the same")
}

// TestFeedbackOccurrenceLookups covers the guards that keep a mismatched
// index or an unindexed verdict from breaking a frame.
func TestFeedbackOccurrenceLookups(t *testing.T) {
	st := fbFixtureState()
	th, ok := fbAtCursor(t, fbView(110, 26, FeedbackOptions{}), st, "sc-c3d4").Selected(st)
	require.True(t, ok)

	got, ok := th.occurrence(1, domain.LensQA, "qa-r1-1")
	require.True(t, ok)
	require.Equal(t, 1, got.Round)

	_, ok = th.occurrence(9, domain.LensQA, "qa-r1-1")
	require.False(t, ok, "no such round")

	broken := FeedbackThread{Items: th.Items, at: map[string]int{
		feedbackItemKey(1, domain.LensQA, "qa-r1-1"): len(th.Items) + 3,
	}}
	_, ok = broken.occurrence(1, domain.LensQA, "qa-r1-1")
	require.False(t, ok, "an index past the items is not an item")

	// A verdict filed under a round the thread does not index still renders
	// its items from the verdict itself.
	v := fbView(110, 26, FeedbackOptions{})
	lines := v.verdictLines(th, 7, fbChanges(domain.LensQA, 7, "unindexed",
		fbItem("qa-r7-1", domain.SeverityMinor, "Raised out of band", domain.ItemOpen)), 110)
	require.Len(t, lines, 2)
	require.Contains(t, lines[0], "unindexed")
	require.Contains(t, lines[1], "Raised out of band")
}
