package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
	"slop-cannon/internal/orchestrator"
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
		// Long enough that the compact view has to cut it and the details
		// view has to wrap it, so both goldens show what the pane does with
		// text it cannot fit on one line.
		Suggestion: "guard the call and cover it with a table case that exercises the failure against the fixture server",
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

// fbApprove is a clean approval from one lens. A re-review carries the
// items it was given forward with their settled status, which is why it
// takes them too (P5-05 refuses a verdict that drops one).
func fbApprove(lens domain.Lens, round int, summary string, items ...domain.FeedbackItem) domain.Verdict {
	return domain.Verdict{Lens: lens, Round: round, Outcome: domain.VerdictApprove, Summary: summary, Items: items}
}

// fbChanges is a change request with items.
func fbChanges(lens domain.Lens, round int, summary string, items ...domain.FeedbackItem) domain.Verdict {
	return domain.Verdict{Lens: lens, Round: round, Outcome: domain.VerdictChanges, Summary: summary, Items: items}
}

// The seven scenarios below are the shape of a real feedback loop, not a
// sketch of one: each is pinned to a recording of P6-04's own loop running
// that scenario against the mock cartridge, and
// [TestFeedbackFixturesMatchTheRecordedLoop] fails when the two disagree
// (P7-14). Three things follow from that and are easy to get wrong by hand:
// the engineer's report or answers are published before the round they open,
// an item is raised `open` and only reaches its settled status when a later
// verdict carries it forward, and a round that only judges a pushback opens
// no [events.RoundStarted] of its own.

// fbScenarioA is P6-04 (a): every tester approves in round 1.
func fbScenarioA(c *fbClock) []events.Event {
	const bead domain.BeadID = "sc-a1b2"
	return []events.Event{
		events.BeadCreated{Meta: c.next(), Bead: fbBead(bead, "Retry failed HTTP requests", domain.BeadStateClosed, 1)},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "eng-1", Report: fbReport("retry 5xx with capped exponential backoff")},
		events.RoundStarted{Meta: c.next(), Bead: bead, Round: 1, RoundKind: events.RoundReview, Lenses: fbAllLenses, Commit: "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "ux-1", Verdict: fbApprove(domain.LensUX, 1, "the error message still reads well")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "qa-1", Verdict: fbApprove(domain.LensQA, 1, "reproduced the flake, now green")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "test-1", Verdict: fbApprove(domain.LensTest, 1, "table test covers the cap")},
	}
}

// fbScenarioB is P6-04 (b): one tester requests changes, the engineer fixes
// them, the tester approves in round 2.
func fbScenarioB(c *fbClock) []events.Event {
	const bead domain.BeadID = "sc-c3d4"
	item := fbItem("qa-r1-1", domain.SeverityBlocker, "Retry loops forever on a 500", domain.ItemOpen)
	resolved := item
	resolved.Status = domain.ItemResolved
	return []events.Event{
		events.BeadCreated{Meta: c.next(), Bead: fbBead(bead, "Cache DNS lookups", domain.BeadStateApproved, 2)},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "eng-2", Report: fbReport("cache lookups for the TTL the resolver reports")},
		events.RoundStarted{Meta: c.next(), Bead: bead, Round: 1, RoundKind: events.RoundReview, Lenses: fbAllLenses, Commit: "2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e"},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "ux-1", Verdict: fbApprove(domain.LensUX, 1, "no user-visible change")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "qa-1", Verdict: fbChanges(domain.LensQA, 1, "one blocker", item)},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "test-1", Verdict: fbApprove(domain.LensTest, 1, "coverage is fine")},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "eng-2", Responses: []domain.Response{
			{ItemID: item.ID, Action: domain.ActionFixed, Rationale: "capped the loop at five attempts"},
		}},
		events.RoundStarted{Meta: c.next(), Bead: bead, Round: 2, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensQA}, Commit: "3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f"},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "qa-1", Verdict: fbApprove(domain.LensQA, 2, "the cap holds", resolved)},
	}
}

// fbScenarioC is P6-04 (c): the engineer pushes back and the tester accepts.
// The evaluation opens no round of its own: it judges the answer to round
// one's items rather than a new commit.
func fbScenarioC(c *fbClock) []events.Event {
	const bead domain.BeadID = "sc-e5f6"
	item := fbItem("ux-r1-1", domain.SeverityMinor, "Spell the flag --no-cache", domain.ItemOpen)
	nit := fbItem("ux-r1-2", domain.SeverityNit, "Capitalise the help text", domain.ItemOpen)
	accepted, acceptedNit := item, nit
	accepted.Status, acceptedNit.Status = domain.ItemAccepted, domain.ItemAccepted
	return []events.Event{
		events.BeadCreated{Meta: c.next(), Bead: fbBead(bead, "Add a --cache flag", domain.BeadStateApproved, 2)},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "eng-3", Report: fbReport("add the flag and document it")},
		events.RoundStarted{Meta: c.next(), Bead: bead, Round: 1, RoundKind: events.RoundReview, Lenses: fbAllLenses, Commit: "4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f70"},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "ux-1", Verdict: fbChanges(domain.LensUX, 1, "naming", item, nit)},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "qa-1", Verdict: fbApprove(domain.LensQA, 1, "behaviour is right")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "test-1", Verdict: fbApprove(domain.LensTest, 1, "covered")},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "eng-3", Responses: []domain.Response{
			{ItemID: item.ID, Action: domain.ActionPushback, Rationale: "every other flag in the CLI is positive; --cache=false already works"},
			{ItemID: nit.ID, Action: domain.ActionWontfix, Rationale: "cobra lower-cases every other help string"},
		}},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "ux-1", Verdict: fbApprove(domain.LensUX, 2, "fair, the rationale holds", accepted, acceptedNit)},
	}
}

// fbScenarioD is P6-04 (d): the pushback is held, the product owner upholds
// the engineer and the bead is approved on the ruling — the loop opens no
// further round for a tester that has been overruled.
func fbScenarioD(c *fbClock) []events.Event {
	const bead domain.BeadID = "sc-g7h8"
	raised := fbItem("qa-r1-1", domain.SeverityMajor, "Validate the timeout before use", domain.ItemOpen)
	held := raised
	held.Status = domain.ItemHeld
	return []events.Event{
		events.BeadCreated{Meta: c.next(), Bead: fbBead(bead, "Bump the client timeout", domain.BeadStateApproved, 2)},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "eng-4", Report: fbReport("raise the timeout to 30s")},
		events.RoundStarted{Meta: c.next(), Bead: bead, Round: 1, RoundKind: events.RoundReview, Lenses: fbAllLenses, Commit: "6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192"},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "ux-1", Verdict: fbApprove(domain.LensUX, 1, "fine")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "qa-1", Verdict: fbChanges(domain.LensQA, 1, "one major", raised)},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "test-1", Verdict: fbApprove(domain.LensTest, 1, "fine")},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "eng-4", Responses: []domain.Response{
			{ItemID: raised.ID, Action: domain.ActionPushback, Rationale: "the config layer already rejects a negative timeout"},
		}},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "qa-1", Verdict: fbChanges(domain.LensQA, 2, "still want the guard", held)},
		events.ArbitrationReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "po-1", Rulings: []domain.Arbitration{
			{ItemID: raised.ID, Ruling: domain.RulingUpholdEngineer, Rationale: "config validation covers it; a second check is dead code"},
		}},
	}
}

// fbScenarioE is P6-04 (e): the product owner upholds the tester and the
// engineer fixes the item. A rules violation is always a blocker the
// engineer has to fix (P5-05 refuses any other answer), so the item the
// tester holds on to is a plain major beside it.
func fbScenarioE(c *fbClock) []events.Event {
	const bead domain.BeadID = "sc-i9j0"
	rules := fbItem("test-r1-1", domain.SeverityBlocker, "No test for the empty-body case", domain.ItemOpen)
	rules.Category = domain.CategoryRules
	disputed := fbItem("test-r1-2", domain.SeverityMajor, "Name the reader parameter", domain.ItemOpen)
	rulesFixed, held, fixed := rules, disputed, disputed
	rulesFixed.Status = domain.ItemResolved
	held.Status = domain.ItemHeld
	fixed.Status = domain.ItemResolved
	return []events.Event{
		events.BeadCreated{Meta: c.next(), Bead: fbBead(bead, "Stream the response body", domain.BeadStateInReview, 3)},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "eng-5", Report: fbReport("stream instead of buffering")},
		events.RoundStarted{Meta: c.next(), Bead: bead, Round: 1, RoundKind: events.RoundReview, Lenses: fbAllLenses, Commit: "92a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5"},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "ux-1", Verdict: fbApprove(domain.LensUX, 1, "no interface change")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "qa-1", Verdict: fbApprove(domain.LensQA, 1, "streams as advertised")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "test-1", Verdict: fbChanges(domain.LensTest, 1, "missing coverage", rules, disputed)},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "eng-5", Responses: []domain.Response{
			{ItemID: rules.ID, Action: domain.ActionFixed, Rationale: "added TestStreamEmptyBody"},
			{ItemID: disputed.ID, Action: domain.ActionPushback, Rationale: "the parameter is named after the interface it takes"},
		}},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "test-1", Verdict: fbChanges(domain.LensTest, 2, "rules say every parameter reads as prose", held)},
		events.ArbitrationReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "po-1", Rulings: []domain.Arbitration{
			{ItemID: disputed.ID, Ruling: domain.RulingUpholdTester, Instruction: "rename it to body", Rationale: "the reviewer reads the code more often than the author"},
		}},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 3, Agent: "eng-5", Responses: []domain.Response{
			{ItemID: disputed.ID, Action: domain.ActionFixed, Rationale: "renamed it to body"},
		}},
		events.RoundStarted{Meta: c.next(), Bead: bead, Round: 3, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensTest}, Commit: "b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7"},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 3, Agent: "test-1", Verdict: fbApprove(domain.LensTest, 3, "covered and it reads well now", rulesFixed, fixed)},
	}
}

// fbScenarioF is P6-04 (f): the round cap is reached and the product owner
// blocks the bead for a human. The item is raised once and carried forward
// with its own id every round; a re-review that renumbered it would be
// refused by the verdict schema (P5-05).
func fbScenarioF(c *fbClock) []events.Event {
	const bead domain.BeadID = "sc-k1l2"
	item := fbItem("qa-r1-1", domain.SeverityBlocker, "Pool still leaks a connection per retry", domain.ItemOpen)
	out := []events.Event{
		events.BeadCreated{Meta: c.next(), Bead: fbBead(bead, "Rewrite the connection pool", domain.BeadStateNeedsHuman, 5)},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "eng-6", Report: fbReport("attempt 1 at the pool")},
		events.RoundStarted{Meta: c.next(), Bead: bead, Round: 1, RoundKind: events.RoundReview, Lenses: fbAllLenses, Commit: strings.Repeat("a", 40)},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "ux-1", Verdict: fbApprove(domain.LensUX, 1, "nothing user facing")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "qa-1", Verdict: fbChanges(domain.LensQA, 1, "still leaking", item)},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "test-1", Verdict: fbApprove(domain.LensTest, 1, "tests cover what there is")},
	}
	for round := 2; round <= 5; round++ {
		out = append(out,
			events.ResponseReceived{Meta: c.next(), Bead: bead, Round: round, Agent: "eng-6", Responses: []domain.Response{
				{ItemID: item.ID, Action: domain.ActionFixed, Rationale: fmt.Sprintf("attempt %d at the pool", round)},
			}},
			events.RoundStarted{Meta: c.next(), Bead: bead, Round: round, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensQA}, Commit: strings.Repeat(string(rune('a'+round-1)), 40)},
			events.VerdictReceived{Meta: c.next(), Bead: bead, Round: round, Agent: "qa-1", Verdict: fbChanges(domain.LensQA, round, "still leaking", item)},
		)
	}
	return append(out, events.ArbitrationReceived{Meta: c.next(), Bead: bead, Round: 5, Agent: "po-1", Escalation: &domain.Escalation{
		Outcome: domain.EscalationBlock,
		Summary: "five rounds without a fix; the pool needs a design a human signs off",
	}})
}

// fbScenarioG is P6-04 (g): the diff moved enough that every tester
// re-reviews (D-09). The fixture stops one verdict short of the recording,
// so the frame it draws is a round still waiting on a tester.
func fbScenarioG(c *fbClock) []events.Event {
	const bead domain.BeadID = "sc-m3n4"
	item := fbItem("qa-r1-1", domain.SeverityMajor, "Rebase dropped the timeout guard", domain.ItemOpen)
	resolved := item
	resolved.Status = domain.ItemResolved
	return []events.Event{
		events.BeadCreated{Meta: c.next(), Bead: fbBead(bead, "Split the transport package", domain.BeadStateInReview, 2)},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "eng-7", Report: fbReport("move the transport into its own package")},
		events.RoundStarted{Meta: c.next(), Bead: bead, Round: 1, RoundKind: events.RoundReview, Lenses: fbAllLenses, Commit: "c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8"},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "ux-1", Verdict: fbApprove(domain.LensUX, 1, "nothing user facing")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "qa-1", Verdict: fbChanges(domain.LensQA, 1, "one major", item)},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 1, Agent: "test-1", Verdict: fbApprove(domain.LensTest, 1, "tests moved with it")},
		events.ResponseReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "eng-7", Responses: []domain.Response{
			{ItemID: item.ID, Action: domain.ActionFixed, Rationale: "restored the guard and moved it into the new package"},
		}},
		// The fix rewrote more than re_review_threshold of the diff, so all
		// three lenses look again (D-09).
		events.RoundStarted{Meta: c.next(), Bead: bead, Round: 2, RoundKind: events.RoundReview, Lenses: fbAllLenses, Commit: "d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f809"},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "ux-1", Verdict: fbApprove(domain.LensUX, 2, "still nothing user facing")},
		events.VerdictReceived{Meta: c.next(), Bead: bead, Round: 2, Agent: "qa-1", Verdict: fbApprove(domain.LensQA, 2, "guard is back", resolved)},
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
		require.Equal(t, domain.ItemResolved, item.Status, "the re-review carried it forward as fixed")
		require.Equal(t, 2, item.Round, "the latest occurrence says where it stands")
		require.Nil(t, item.Ruling)

		// The answer stays on the occurrence it answered: the item was
		// raised in round 1 and fixed in round 2.
		answered := fbOne(t, th.Answered(2, "qa-r1-1"))
		require.Equal(t, 1, answered.Round)
		require.Equal(t, domain.ActionFixed, answered.Response.Action)
		require.Equal(t, 2, answered.ResponseRound)
	})

	t.Run("c pushback accepted", func(t *testing.T) {
		th := byBead["sc-e5f6"]
		require.True(t, th.Approved)
		item, ok := th.Item("ux-r1-1")
		require.True(t, ok)
		require.Equal(t, domain.ItemAccepted, item.Status)
		require.False(t, item.Blocking())
		require.Equal(t, domain.ActionPushback, fbOne(t, th.Answered(2, "ux-r1-1")).Response.Action)
		nit, ok := th.Item("ux-r1-2")
		require.True(t, ok)
		require.Equal(t, domain.ItemAccepted, nit.Status)
		require.Equal(t, domain.SeverityNit, nit.Severity)
		require.Equal(t, domain.ActionWontfix, fbOne(t, th.Answered(2, "ux-r1-2")).Response.Action)
	})

	t.Run("d pushback held then engineer upheld", func(t *testing.T) {
		th := byBead["sc-g7h8"]
		require.Len(t, th.Rounds, 2, "the ruling settles the bead; no further round is opened")
		// The loop approves the bead on the ruling and asks qa nothing more,
		// so the last thing the lens said is still a change request: the
		// view reports the bead's own state as approved and the tester as
		// unsatisfied, which is what the run really looks like.
		require.Equal(t, domain.BeadStateApproved, th.BeadState)
		require.False(t, th.Approved, "qa was overruled, not satisfied")
		item, ok := th.Item("qa-r1-1")
		require.True(t, ok)
		require.Equal(t, domain.ItemHeld, item.Status, "the latest occurrence says where it stands")
		require.Equal(t, 2, item.Round)
		require.NotNil(t, item.Ruling, "the ruling is on the occurrence the tester held")
		require.Equal(t, domain.RulingUpholdEngineer, item.Ruling.Ruling)

		raised := fbOne(t, th.Answered(2, "qa-r1-1"))
		require.Equal(t, 1, raised.Round, "round 2's response answers the item round 1 raised")
		require.Equal(t, domain.ActionPushback, raised.Response.Action)
		require.Equal(t, 2, raised.ResponseRound)

		held := fbOne(t, th.Ruled(2, "qa-r1-1"))
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
		// The rules violation was fixed in round 2, as P5-05 requires; the
		// item beside it was pushed back, held, and ruled on.
		require.Equal(t, domain.ActionFixed, fbOne(t, th.Answered(2, "test-r1-1")).Response.Action)
		raised := fbOne(t, th.Answered(2, "test-r1-2"))
		require.Equal(t, 1, raised.Round)
		require.Equal(t, domain.ActionPushback, raised.Response.Action)
		require.Equal(t, "the parameter is named after the interface it takes", raised.Response.Rationale)
		require.Equal(t, 2, raised.ResponseRound)

		held := fbOne(t, th.Ruled(2, "test-r1-2"))
		require.Equal(t, 2, held.Round)
		require.Equal(t, domain.ItemHeld, held.Status)
		require.Equal(t, domain.RulingUpholdTester, held.Ruling.Ruling)
		require.Equal(t, 2, held.RulingRound)
		require.Equal(t, domain.ActionFixed, held.Response.Action, "round 3 answered the held occurrence")
		require.Equal(t, 3, held.ResponseRound)

		// Nothing was raised before round 1, so a response given in round 1
		// would fall back to the statement round 1 itself made.
		require.Equal(t, 1, fbOne(t, th.Answered(1, "test-r1-1")).Round)
		require.Empty(t, th.Answered(1, "no-such-item"))
		require.Empty(t, th.Ruled(2, "no-such-item"))
	})

	t.Run("f round cap escalates", func(t *testing.T) {
		th := byBead["sc-k1l2"]
		require.Len(t, th.Rounds, 5)
		require.False(t, th.Approved)
		require.Equal(t, 1, th.Open, "the one blocker is carried forward, never renumbered")
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

// fbOne asserts an answer matched exactly one occurrence and returns it.
func fbOne(t *testing.T, ivs []FeedbackItemView) FeedbackItemView {
	t.Helper()
	require.Len(t, ivs, 1)
	return ivs[0]
}

// fbLineOf returns the index of the first rendered line containing needle,
// or -1 when nothing on screen does. It is how the tests below pin one line
// above or below another.
func fbLineOf(out, needle string) int {
	for i, line := range strings.Split(out, "\n") {
		if strings.Contains(fbPlain(line), needle) {
			return i
		}
	}
	return -1
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
		require.Contains(t, out, "the parameter is named after the interface it takes",
			"the round-2 pushback renders")
		require.Equal(t, 2, fbRoundOf(out, "↩ pushback"), "it was given in round 2")
		require.Equal(t, 2, fbRoundOf(out, "⚖ uphold_tester"), "the ruling followed in round 2")
		require.Equal(t, 3, fbRoundOf(out, "renamed it to body"), "the fix came in round 3")
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
		require.Equal(t, 2, fbRoundOf(out, "[held]"), "the ruling settles the occurrence the tester held")
	})

	t.Run("the answer names the item it answers", func(t *testing.T) {
		v := fbAtCursor(t, fbView(110, 40, FeedbackOptions{Details: true}), st, "sc-i9j0")
		out := v.View(st)
		require.Contains(t, out, "→ fixed    test-r1-1")
		require.Contains(t, out, "↩ pushback test-r1-2")
		require.Contains(t, out, "re: round 1 · test · blocker · No test for the empty-body case",
			"details spell out the occurrence answered")
		require.Contains(t, out, "re: round 2 · test · major · Name the reader parameter")
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

// TestFeedbackSameRoundResponse is the regression test for an engineer's
// answer that arrives in the round of the verdict it answers: P6-04
// aggregates a round's verdicts, consolidates the items, sends them to the
// engineer and collects the responses without necessarily opening another
// round, so a response can share its round with the item it replies to. It
// must then be drawn under that verdict, not above it, and it must still be
// attached to the occurrence it answers.
func TestFeedbackSameRoundResponse(t *testing.T) {
	item := fbItem("qa-1", domain.SeverityBlocker, "Retry loops forever", domain.ItemResolved)
	st := fbState([]events.Event{
		events.RunStarted{Meta: fbMeta(0), Info: fbRunInfo()},
		events.RoundStarted{Meta: fbMeta(1), Bead: "sc-same", Round: 1, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensQA}},
		events.VerdictReceived{Meta: fbMeta(2), Bead: "sc-same", Round: 1, Agent: "qa-1", Verdict: fbChanges(domain.LensQA, 1, "one blocker", item)},
		events.ResponseReceived{Meta: fbMeta(3), Bead: "sc-same", Round: 1, Agent: "eng-1", Responses: []domain.Response{
			{ItemID: item.ID, Action: domain.ActionFixed, Rationale: "capped the loop"},
		}},
	})

	th, ok := fbView(110, 26, FeedbackOptions{}).Selected(st)
	require.True(t, ok)
	answered := fbOne(t, th.Answered(1, "qa-1"))
	require.Equal(t, 1, answered.Round, "the response answers the item this round raised")
	require.NotNil(t, answered.Response, "the occurrence is not left looking unanswered")
	require.Equal(t, domain.ActionFixed, answered.Response.Action)
	require.Equal(t, 1, answered.ResponseRound)

	out := fbView(110, 26, FeedbackOptions{}).View(st)
	verdict := fbLineOf(out, "✗ qa   changes")
	response := fbLineOf(out, "→ fixed")
	require.Positive(t, verdict)
	require.Positive(t, response)
	require.Greater(t, response, verdict, "the engineer answers after the tester spoke, not before it")
	require.Equal(t, 1, fbRoundOf(out, "→ fixed"), "and inside the round it was given in")

	details := fbView(110, 30, FeedbackOptions{Details: true}).View(st)
	require.Contains(t, details, "re: round 1 · qa · blocker · Retry loops forever",
		"details name the occurrence answered, same round or not")
}

// TestFeedbackSameItemFromTwoLenses covers two lenses raising the same item
// id in one round, which P6-04 produces when it dedupes items by file and
// title across the three lenses: each lens holds its own open item, and the
// single engineer response settles both of them.
func TestFeedbackSameItemFromTwoLenses(t *testing.T) {
	raised := fbItem("dup-1", domain.SeverityMajor, "The empty state has no copy", domain.ItemOpen)
	fixed := raised
	fixed.Status = domain.ItemResolved
	evs := []events.Event{
		events.RunStarted{Meta: fbMeta(0), Info: fbRunInfo()},
		events.RoundStarted{Meta: fbMeta(1), Bead: "sc-dup0", Round: 1, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensUX, domain.LensQA}},
		events.VerdictReceived{Meta: fbMeta(2), Bead: "sc-dup0", Round: 1, Agent: "ux-1", Verdict: fbChanges(domain.LensUX, 1, "one major", raised)},
		events.VerdictReceived{Meta: fbMeta(3), Bead: "sc-dup0", Round: 1, Agent: "qa-1", Verdict: fbChanges(domain.LensQA, 1, "same major", raised)},
	}
	st := fbState(evs)
	th, ok := fbView(110, 26, FeedbackOptions{}).Selected(st)
	require.True(t, ok)
	require.Equal(t, 2, th.Open, "each lens holds its own open item")

	st = fbState(append(evs,
		events.RoundStarted{Meta: fbMeta(4), Bead: "sc-dup0", Round: 2, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensUX, domain.LensQA}},
		events.ResponseReceived{Meta: fbMeta(5), Bead: "sc-dup0", Round: 2, Agent: "eng-1", Responses: []domain.Response{
			{ItemID: "dup-1", Action: domain.ActionFixed, Rationale: "wrote the empty state copy"},
		}},
		events.VerdictReceived{Meta: fbMeta(6), Bead: "sc-dup0", Round: 2, Agent: "ux-1", Verdict: domain.Verdict{
			Lens: domain.LensUX, Round: 2, Outcome: domain.VerdictApprove, Summary: "reads well", Items: []domain.FeedbackItem{fixed},
		}},
		events.VerdictReceived{Meta: fbMeta(7), Bead: "sc-dup0", Round: 2, Agent: "qa-1", Verdict: domain.Verdict{
			Lens: domain.LensQA, Round: 2, Outcome: domain.VerdictApprove, Summary: "copy is there", Items: []domain.FeedbackItem{fixed},
		}},
	))
	th, ok = fbView(110, 26, FeedbackOptions{}).Selected(st)
	require.True(t, ok)
	require.Zero(t, th.Open, "both lenses saw their item settled")
	require.True(t, th.Approved)

	answered := th.Answered(2, "dup-1")
	require.Len(t, answered, 2, "one response settles the occurrence of every lens that raised it")
	lenses := make([]domain.Lens, 0, 2)
	for _, iv := range answered {
		require.NotNil(t, iv.Response, "%s is not left unanswered", iv.Lens)
		require.Equal(t, 2, iv.ResponseRound)
		require.Equal(t, 1, iv.Round)
		lenses = append(lenses, iv.Lens)
	}
	require.ElementsMatch(t, []domain.Lens{domain.LensUX, domain.LensQA}, lenses)

	out := fbView(110, 40, FeedbackOptions{Details: true}).View(st)
	require.Contains(t, out, "re: round 1 · ux · major · The empty state has no copy")
	require.Contains(t, out, "re: round 1 · qa · major · The empty state has no copy")
}

// fbLongState is a bead whose item, report and answers are all wider than
// any pane, so the tests below can see what the view does with text it
// cannot fit.
func fbLongState() *State {
	item := domain.FeedbackItem{
		ID: "qa-1", Severity: domain.SeverityBlocker, Status: domain.ItemOpen,
		Title:      "Retry loops forever on a 500 because the attempt counter is reset inside the loop body",
		Detail:     "The counter is assigned before every request, so a server that keeps answering 500 keeps the client in the loop until the context deadline fires. Reproduced against the fixture server with a two minute deadline.",
		Suggestion: "hoist the counter out of the loop and cover it with a table case that answers 500 forever",
		File:       "internal/http/retry.go", Line: 42,
	}
	return fbState([]events.Event{
		events.RunStarted{Meta: fbMeta(0), Info: fbRunInfo()},
		events.RoundStarted{Meta: fbMeta(1), Bead: "sc-long", Round: 1, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensQA}},
		events.ResponseReceived{Meta: fbMeta(2), Bead: "sc-long", Round: 1, Agent: "eng-1", Report: &domain.ImplReport{
			Summary: "retry with exponential backoff, capped at three attempts, and give up on a context deadline instead of spinning",
		}},
		events.VerdictReceived{Meta: fbMeta(3), Bead: "sc-long", Round: 1, Agent: "qa-1", Verdict: fbChanges(domain.LensQA, 1,
			"one blocker in the retry path that a long running server reproduces every time", item)},
		events.RoundStarted{Meta: fbMeta(4), Bead: "sc-long", Round: 2, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensQA}},
		events.ResponseReceived{Meta: fbMeta(5), Bead: "sc-long", Round: 2, Agent: "eng-1", Responses: []domain.Response{
			{ItemID: "qa-1", Action: domain.ActionPushback, Rationale: "the deadline is the caller's to set and every other client in the repo behaves the same way"},
		}},
		events.ArbitrationReceived{Meta: fbMeta(6), Bead: "sc-long", Round: 2, Agent: "po-1", Rulings: []domain.Arbitration{
			{ItemID: "qa-1", Ruling: domain.RulingCompromise, Instruction: "cap the attempts and leave the deadline to the caller, then say so in the package doc"},
		}},
	})
}

// fbDetail renders the round history of the selected bead on its own, so a
// test can read it without the bead list column running through every line.
func fbDetail(t *testing.T, v FeedbackModel, st *State) []string {
	t.Helper()
	th, ok := v.Selected(st)
	require.True(t, ok)
	out := make([]string, 0, 32)
	for _, line := range v.detailLines(th, v.detailWidth()) {
		out = append(out, strings.TrimRight(fbPlain(line), " "))
	}
	return out
}

// fbFlat joins rendered lines into one, so a test can look for text the
// details view wrapped across several of them.
func fbFlat(lines []string) string {
	return strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
}

// fbCutIsMarked reports whether text was cut with an ellipsis rather than
// clipped silently: some line ends in "…" right after a prefix of it.
func fbCutIsMarked(lines []string, text string) bool {
	for _, line := range lines {
		frag, ok := strings.CutSuffix(line, "…")
		if !ok {
			continue
		}
		for n := len(text); n > 8; n-- {
			if strings.HasSuffix(frag, text[:n]) {
				return true
			}
		}
	}
	return false
}

// TestFeedbackLongTextIsMarkedOrWrapped is the regression test for text
// wider than the pane: the compact view never cuts anything silently, and
// the details view shows the whole of it, as docs/tui-feedback.md promises.
func TestFeedbackLongTextIsMarkedOrWrapped(t *testing.T) {
	st := fbLongState()
	cut := []string{
		"Retry loops forever on a 500 because the attempt counter is reset inside the loop body",
		"retry with exponential backoff, capped at three attempts, and give up on a context deadline instead of spinning",
		"the deadline is the caller's to set and every other client in the repo behaves the same way",
		"cap the attempts and leave the deadline to the caller, then say so in the package doc",
		"one blocker in the retry path that a long running server reproduces every time",
	}

	for _, w := range []int{110, 80, 64} {
		t.Run(fmt.Sprintf("compact at %d columns marks the cut", w), func(t *testing.T) {
			lines := fbDetail(t, fbView(w, 40, FeedbackOptions{}), st)
			flat := fbFlat(lines)
			for _, text := range cut {
				require.NotContains(t, flat, text, "the pane is too narrow to hold it whole")
				require.True(t, fbCutIsMarked(lines, text), "%q was cut with no ellipsis in:\n%s",
					text, strings.Join(lines, "\n"))
			}
		})

		t.Run(fmt.Sprintf("details at %d columns show the whole text", w), func(t *testing.T) {
			lines := fbDetail(t, fbView(w, 60, FeedbackOptions{Details: true}), st)
			flat := fbFlat(lines)
			for _, text := range cut {
				require.Contains(t, flat, text, "the details toggle shows the item's full text")
			}
			require.Contains(t, flat, "The counter is assigned before every request", "the detail wraps too")
			require.Contains(t, flat, "hoist the counter out of the loop", "and so does the suggestion")
			require.NotContains(t, flat, "…", "nothing needs an ellipsis once it wraps")
		})
	}

	t.Run("the status bracket survives a narrow pane", func(t *testing.T) {
		held := domain.FeedbackItem{
			ID: "qa-1", Severity: domain.SeverityBlocker, Status: domain.ItemHeld,
			Title: "Retry loops forever on a 500 because the attempt counter is reset inside the loop body",
		}
		narrow := fbState([]events.Event{
			events.RunStarted{Meta: fbMeta(0), Info: fbRunInfo()},
			events.RoundStarted{Meta: fbMeta(1), Bead: "sc-held", Round: 1, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensQA}},
			events.VerdictReceived{Meta: fbMeta(2), Bead: "sc-held", Round: 1, Agent: "qa-1", Verdict: fbChanges(domain.LensQA, 1, "one blocker", held)},
		})
		require.Contains(t, fbView(64, 20, FeedbackOptions{}).View(narrow), "[held]",
			"the mark is never the field the pane cuts")
	})

	t.Run("a word wider than the line is cut with an ellipsis", func(t *testing.T) {
		require.Equal(t, []string{"short", "aaaaaaaaa…"}, feedbackWrap("short "+strings.Repeat("a", 40), 10, 10))
		require.Equal(t, []string{"aaaa…", "b"}, feedbackWrap(strings.Repeat("a", 40)+" b", 5, 5))
		require.Empty(t, feedbackWrap("   ", 10, 10))

		// Text a cartridge filled with nothing but spaces leaves the head
		// alone rather than an empty continuation line.
		v := fbView(40, 12, FeedbackOptions{Details: true})
		require.Equal(t, []string{"  ⚙ report"}, v.flow("  ⚙ report ", "   ", ThemeFor(ProfileNoColor).Muted(), 6, 40))
	})

	t.Run("a pane with no room for the text at all still renders the line", func(t *testing.T) {
		for _, details := range []bool{false, true} {
			out := fbView(24, 12, FeedbackOptions{Details: details}).View(st)
			lines := strings.Split(out, "\n")
			require.Len(t, lines, 12-chromeHeight)
			for _, line := range lines {
				require.LessOrEqual(t, lipgloss.Width(line), 24)
			}
		}
	})
}

// TestFeedbackKeysAreInTheHelpOverlay checks the view's own bindings are
// discoverable from inside the TUI and not only from the documentation: the
// `?` overlay lists them under the tab's own heading (P7-13 will do the same
// for every other view).
func TestFeedbackKeysAreInTheHelpOverlay(t *testing.T) {
	st := fbFixtureState()
	m := New(t.Context(), Options{Run: st.Run, NoColor: true})
	m.state = st
	m.tab = TabFeedback
	m.width, m.height = 110, 40
	m.help.Width = 110

	overlay := fbPlain(m.helpOverlay(110, 40))
	require.Contains(t, overlay, "Feedback keys", "the overlay names the view whose keys it lists")
	for _, want := range []string{"details", "scroll down", "scroll up", "first round", "last round"} {
		require.Contains(t, overlay, want, "the ? overlay documents %q", want)
	}
	require.Len(t, m.viewKeys(), len(DefaultFeedbackKeyMap().FullHelp()))
}

// --- P7-14: the fixtures, pinned to a recording of the real loop --------
//
// The scenarios above are hand-written, because P6-04's feedback loop did
// not exist when P7-04 shipped. They are the frames the view is designed
// against — a long title, a wrapped suggestion, a thread that has to scroll
// — and they stay. What they lacked was any tie to what the loop actually
// publishes: a round it stopped opening, a lens it stopped sending back or
// an item status it stopped setting would have left the fixtures, and so the
// goldens, describing a run that no longer happens. It had already happened
// three times over by the time this was written (a round opened for a
// pushback evaluation, a fourth round after a ruling, an item renumbered on
// every re-review and a rules violation pushed back, none of which the loop
// or the P5-05 schemas allow).
//
// So the seven P6-04 scenarios are run here for real — the real Feedback
// loop, the real supervisor, the real prompts and the mock cartridge, the
// rig internal/orchestrator/feedback_test.go drives them with — recorded as
// JSONL under testdata/tui/recorded (the run-log shape `sc tui-demo
// --replay` reads, P7-12), and both the recording and the hand-written
// fixture are reduced to the same trace and required to agree.

// fbrCartridge is the mock cartridge the recordings are fired at. It has its
// own registry name so the enforcement registered for it cannot collide with
// another package's.
const fbrCartridge = "tui-fb-mock"

// fbrBead is the bead every recorded scenario runs on.
const fbrBead = domain.BeadID("sc-fb1")

// fbrEpoch is the fixed clock the recordings are stamped with, so a
// recording is byte-for-byte reproducible.
var fbrEpoch = time.Date(2026, 3, 5, 11, 0, 0, 0, time.UTC)

// fbrDir is where the recordings live, beside the golden frames.
const fbrDir = fbGoldenDir + "/recorded"

// The mock holds its tool policy in-process and says so, rather than making
// every tester firing fall back to a copy of the worktree (P6-20).
func init() {
	err := agents.RegisterEnforcement(fbrCartridge, func(name string, p cartridge.ToolPolicy) (agents.Enforcement, error) {
		return agents.Enforcement{
			Cartridge: name, Policy: p, Mode: agents.EnforceNative,
			Detail: "the mock refuses forbidden steps in-process",
		}, nil
	})
	if err != nil && !strings.Contains(err.Error(), "already registered") {
		panic(err)
	}
}

// fbrStore is the bd the state machine writes through: it keeps the status
// and hands it back, which is all the loop reads.
type fbrStore struct {
	mu    sync.Mutex
	state domain.BeadState
}

func (s *fbrStore) set(to domain.BeadState) (beads.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = to
	return beads.Issue{ID: fbrBead, Status: to.String()}, nil
}

func (s *fbrStore) SetStatus(_ context.Context, _ domain.BeadID, st domain.BeadState) (beads.Issue, error) {
	return s.set(st)
}

func (s *fbrStore) ClaimFor(_ context.Context, _ domain.BeadID, _ domain.AgentID) (beads.Issue, error) {
	return s.set(domain.BeadStateInProgress)
}

func (s *fbrStore) Reclaim(_ context.Context, _ domain.BeadID, _ domain.AgentID) (beads.Issue, error) {
	return s.set(domain.BeadStateInProgress)
}

func (s *fbrStore) Release(_ context.Context, _ domain.BeadID) (beads.Issue, error) {
	return s.set(domain.BeadStateOpen)
}

func (s *fbrStore) Close(_ context.Context, _ domain.BeadID, _ string) (beads.Issue, error) {
	return s.set(domain.BeadStateClosed)
}

// fbrMessages accepts the bead comments the loop writes (D-06) and keeps
// none of them: the recording is of the event stream, not of bd.
type fbrMessages struct{}

func (fbrMessages) PostMessage(_ context.Context, id domain.BeadID, actor string, msg any) (beads.Message, error) {
	t, err := beads.MessageTypeOf(msg)
	if err != nil {
		return beads.Message{}, err
	}
	return beads.Message{Type: t, IssueID: id, Author: actor, Payload: msg}, nil
}

// fbrRig is one hermetic run of the real feedback loop against the mock.
type fbrRig struct {
	loop *orchestrator.Feedback
	sup  *orchestrator.Supervisor
	run  *orchestrator.Run
	sub  *events.Subscription
	home string

	mu    sync.Mutex
	diff  *gitx.Diff
	ratio float64
	seen  []events.Event
}

// fbrSetup builds the rig: a run, the state machine, the feedback loop and a
// supervisor firing the mock cartridge. It is internal/orchestrator's own
// fbSetup with the assertions dropped and the events kept.
func fbrSetup(t *testing.T, sc *mock.Scenario, tweak func(*orchestrator.FeedbackOptions)) *fbrRig {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	repo := filepath.Join(home, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))

	bus := events.NewBus()
	sub := bus.Subscribe(context.Background(), 4096)
	t.Cleanup(func() { sub.Unsubscribe(); bus.Close() })

	cfg := config.Defaults()
	cfg.ActiveCartridge = fbrCartridge
	cfg.Cartridges = map[string]config.Cartridge{fbrCartridge: {}}
	adapter, err := mock.NewAdapter(fbrCartridge, sc)
	require.NoError(t, err)
	eff := &config.Effective{Config: cfg, Sources: map[string]config.Source{}}
	rc, err := config.NewRunConfig(repo, eff, config.RunSettings{
		ID: "20260305-110000-fb00", Brief: "fix the retry bugs", StartedAt: fbrEpoch, Headless: true,
	})
	require.NoError(t, err)

	now := func() time.Time { return fbrEpoch }
	sel, err := cartridge.NewSelector(cfg, map[string]cartridge.Cartridge{fbrCartridge: adapter}, cartridge.WithClock(now))
	require.NoError(t, err)

	run, err := orchestrator.New(rc, orchestrator.Options{Bus: bus, Now: now, SkipLock: true})
	require.NoError(t, err)
	require.NoError(t, run.Start(context.Background()))
	t.Cleanup(func() { _ = run.Stop("the recording is over"); _, _ = run.Wait() })

	wt := filepath.Join(run.Dirs().Worktrees, fbrBead.String())
	require.NoError(t, os.MkdirAll(wt, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "retry.go"), []byte("package http\n"), 0o600))

	rig := &fbrRig{run: run, sub: sub, home: home, diff: fbrDiff("1a2b3c4d", 20)}
	states := orchestrator.NewStateMachine(&fbrStore{state: domain.BeadStateOpen},
		orchestrator.WithStatePublisher(bus), orchestrator.WithStateRun(run.ID()), orchestrator.WithStateClock(now))
	opts := orchestrator.FeedbackOptions{
		Context:  rig.contextFor(wt),
		States:   states,
		Messages: fbrMessages{},
		Bus:      bus,
		Run:      run.ID(),
		Now:      now,
		Limits:   config.Limits{MaxReviewRounds: 5, MaxPushbackRounds: 2},
		Ratio:    rig.changeRatio,
	}
	if tweak != nil {
		tweak(&opts)
	}
	loop, err := orchestrator.NewFeedback(opts)
	require.NoError(t, err)
	rig.loop = loop

	sup, err := orchestrator.NewSupervisor(orchestrator.SupervisorOptions{
		Cartridges: sel, Planner: loop, Bus: bus, Timeout: time.Minute, Now: now, NoShims: true,
	})
	require.NoError(t, err)
	sup.Attach(run)
	t.Cleanup(func() { _ = sup.Close() })
	rig.sup = sup

	loop.Track(domain.Bead{
		ID: fbrBead, Title: "Retry failed HTTP requests", Type: domain.BeadTypeTask,
		Priority: domain.PriorityDefault, State: domain.BeadStateOpen,
	})
	return rig
}

// fbrDiff is a hand-built change set: no git runs in these recordings.
func fbrDiff(head string, lines int) *gitx.Diff {
	return &gitx.Diff{
		Stats: gitx.DiffStats{
			Base: "base0", Head: head, Files: 1, Insertions: lines, Deletions: 0,
			Changed: []gitx.FileStat{{Path: "internal/http/retry.go", Insertions: lines}},
		},
		Files: []gitx.DiffFile{{FileStat: gitx.FileStat{Path: "internal/http/retry.go", Insertions: lines}, Status: gitx.DiffModified}},
		Patch: "--- a/internal/http/retry.go\n+++ b/internal/http/retry.go\n@@\n+for {\n+  break\n+}\n",
	}
}

// contextFor is the loop's FeedbackContextFunc: the bead, the worktree and
// the diff every firing is given.
func (r *fbrRig) contextFor(worktree string) func(context.Context, domain.BeadID) (orchestrator.FeedbackBeadContext, error) {
	return func(_ context.Context, bead domain.BeadID) (orchestrator.FeedbackBeadContext, error) {
		r.mu.Lock()
		diff := r.diff
		r.mu.Unlock()
		return orchestrator.FeedbackBeadContext{
			Bead: prompt.BeadContext{
				Bead: domain.Bead{
					ID: bead, Title: "Retry failed HTTP requests", Type: domain.BeadTypeTask,
					Description: "the client gives up on the first 5xx",
					Acceptance:  "a 5xx is retried with capped exponential backoff",
					Priority:    domain.PriorityDefault,
				},
				Worktree: worktree,
				Branch:   "sc/" + bead.String(),
			},
			Repo:    prompt.RepoContext{Root: worktree, DefaultBranch: "main", CheckCommand: "go test ./..."},
			Brief:   "fix the retry bugs",
			Diff:    diff,
			Timeout: time.Minute,
		}, nil
	}
}

// changeRatio is the D-09 measurement the scenarios set.
func (r *fbrRig) changeRatio(_ context.Context, _ domain.BeadID, _, cur *gitx.Diff) (gitx.ChangeRatio, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return gitx.ChangeRatio{Current: cur.Head(), Total: cur.ChangedLines(), Ratio: r.ratio}, nil
}

// setDiff replaces the change set the next round reviews and how far the
// round moved it.
func (r *fbrRig) setDiff(head string, lines int, ratio float64) {
	r.mu.Lock()
	r.diff, r.ratio = fbrDiff(head, lines), ratio
	r.mu.Unlock()
}

// step fires every demand the loop raises right now — the calls
// Scheduler.dispatch makes, without the timers — and reports whether there
// was any.
func (r *fbrRig) step(t *testing.T) bool {
	t.Helper()
	ctx := context.Background()
	ds, err := r.loop.Demands(ctx)
	require.NoError(t, err)
	for _, d := range ds {
		agent := d.Agent
		if agent == "" {
			agent = fbrAgentID(d)
		}
		_, err := r.sup.Fire(ctx, orchestrator.Firing{
			Agent: domain.Agent{
				ID: agent, Role: d.Role(), Lens: d.Lens, Bead: d.Bead,
				Cartridge: fbrCartridge, State: domain.AgentStateRunning, StartedAt: fbrEpoch,
			},
			Demand: d,
			Run:    r.run,
		})
		require.NoErrorf(t, err, "firing %s for %s", agent, d.Kind)
	}
	return len(ds) > 0
}

// fbrAgentID names an agent the scheduler would have named itself. The mock
// is scripted by role, lens, round and task rather than by id, so no
// recording depends on the spelling.
func fbrAgentID(d orchestrator.Demand) domain.AgentID {
	id := string(d.Kind) + "-" + string(d.Bead)
	if d.Lens != "" {
		id = string(d.Kind) + "-" + string(d.Lens) + "-" + string(d.Bead)
	}
	if d.Round > 0 {
		id += "-r" + strconv.Itoa(d.Round)
	}
	return domain.AgentID(id + "-" + string(d.FiringTask()))
}

// events drains the subscription and returns everything the run published,
// in order.
func (r *fbrRig) events() []events.Event {
	for {
		select {
		case ev, ok := <-r.sub.Events():
			if !ok {
				return r.drained()
			}
			r.mu.Lock()
			r.seen = append(r.seen, ev)
			r.mu.Unlock()
		default:
			return r.drained()
		}
	}
}

func (r *fbrRig) drained() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]events.Event(nil), r.seen...)
}

// --- the seven P6-04 scenarios, scripted for the mock -------------------

// fbrScenario is one P6-04 scenario as this package records it: the mock
// script, the D-09 measurement it needs and the letter P6-04 gives it.
type fbrScenario struct {
	// letter is the scenario's name in P6-04's "Done when" list.
	letter string
	// what it exercises, for the failure message.
	what string
	// script builds the mock scenario.
	script func() *mock.Scenario
	// tweak changes the loop's limits (D-08) before it is built.
	tweak func(*orchestrator.FeedbackOptions)
	// rounds are the D-09 measurements to apply before each pump step,
	// indexed by step; a step past the end keeps the last one.
	diffs []fbrDiffStep
	// steps is the pump budget.
	steps int
	// fixture is the hand-written event sequence the recording is compared
	// with.
	fixture func(*fbClock) []events.Event
}

// fbrDiffStep is a change set and how far the round moved it (D-09).
type fbrDiffStep struct {
	head  string
	lines int
	ratio float64
}

// fbrItem is one feedback item of a script, spelled the way the fixtures
// spell it so the two traces can be compared item by item.
func fbrItem(id string, sev domain.Severity, title string, status domain.ItemStatus) domain.FeedbackItem {
	return domain.FeedbackItem{
		ID: id, Severity: sev, Title: title, Status: status,
		Detail: "seen while exercising " + title,
		File:   "internal/http/retry.go", Line: 42,
	}
}

// fbrScript starts a script whose unmatched firings fail loudly, so a
// scenario that fires an agent it did not plan for is a test failure rather
// than a mystery.
func fbrScript(name string) *mock.Scenario {
	sc := mock.NewScenario(name)
	sc.Otherwise("unscripted").Say("no rule matched this firing").
		Fail(mock.FailCrash, "the feedback loop fired an agent the scenario does not script")
	return sc
}

// fbrSay scripts one reply. The match is by role, lens, round and task
// rather than by agent id: the ids are the scheduler's to mint, and a
// recording must not depend on their spelling.
func fbrSay(sc *mock.Scenario, name string, m mock.Match, payload any) {
	sc.Rule(name, m).Say("working on " + name).Structured(payload)
}

// fbrReport is an engineer's implementation report.
func fbrReport(summary string) domain.ImplReport {
	return domain.ImplReport{
		Summary: summary, FilesChanged: []string{"internal/http/retry.go"},
		HowToVerify: "go test ./internal/http/...",
	}
}

// fbrApprove and fbrChanges are a tester's two verdicts.
func fbrApprove(summary string, items ...domain.FeedbackItem) domain.Verdict {
	return domain.Verdict{Outcome: domain.VerdictApprove, Summary: summary, Items: items}
}

func fbrChanges(summary string, items ...domain.FeedbackItem) domain.Verdict {
	return domain.Verdict{Outcome: domain.VerdictChanges, Summary: summary, Items: items}
}

// fbrEngineer scripts the engineer's reply for one round.
func fbrEngineer(sc *mock.Scenario, round int, payload any) {
	fbrSay(sc, fmt.Sprintf("eng-r%d", round), mock.Match{Role: "engineer", Round: round}, payload)
}

// fbrTester scripts one lens's reply for one round and task.
func fbrTester(sc *mock.Scenario, lens domain.Lens, round int, task events.Task, v domain.Verdict) {
	fbrSay(sc, fmt.Sprintf("%s-r%d-%s", lens, round, task),
		mock.Match{Role: "tester", Lens: lens.String(), Round: round, Task: task.String()}, v)
}

// fbrPO scripts the product owner's ruling.
func fbrPO(sc *mock.Scenario, name string, task events.Task, payload any) {
	fbrSay(sc, name, mock.Match{Role: "po", Task: task.String()}, payload)
}

// fbrScenarios is P6-04's "Done when" list, each one scripted for the mock
// and paired with the hand-written fixture above that stands for it.
func fbrScenarios() []fbrScenario {
	return []fbrScenario{{
		letter: "a",
		what:   "all three testers approve in round one",
		script: func() *mock.Scenario {
			sc := fbrScript("all-approve")
			fbrEngineer(sc, 1, fbrReport("retry 5xx with capped exponential backoff"))
			for _, l := range domain.Lenses() {
				fbrTester(sc, l, 1, events.TaskReview, fbrApprove(l.String()+" is happy"))
			}
			return sc
		},
		steps:   8,
		fixture: fbScenarioA,
	}, {
		letter: "b",
		what:   "one tester requests changes, the engineer fixes them, round two approves",
		script: func() *mock.Scenario {
			item := fbrItem("qa-r1-1", domain.SeverityBlocker, "Retry loops forever on a 500", domain.ItemOpen)
			resolved := item
			resolved.Status = domain.ItemResolved
			sc := fbrScript("changes-then-approved")
			fbrEngineer(sc, 1, fbrReport("cache lookups for the TTL the resolver reports"))
			fbrTester(sc, domain.LensUX, 1, events.TaskReview, fbrApprove("no user-visible change"))
			fbrTester(sc, domain.LensQA, 1, events.TaskReview, fbrChanges("one blocker", item))
			fbrTester(sc, domain.LensTest, 1, events.TaskReview, fbrApprove("coverage is fine"))
			fbrEngineer(sc, 2, agents.ResponseSet{Responses: []domain.Response{
				{ItemID: item.ID, Action: domain.ActionFixed, Rationale: "capped the loop at five attempts"},
			}})
			fbrTester(sc, domain.LensQA, 2, events.TaskReview, fbrApprove("the cap holds", resolved))
			return sc
		},
		diffs:   []fbrDiffStep{{}, {"2b3c4d5e", 24, 0.1}},
		steps:   10,
		fixture: fbScenarioB,
	}, {
		letter: "c",
		what:   "the engineer pushes back and the tester accepts",
		script: func() *mock.Scenario {
			item := fbrItem("ux-r1-1", domain.SeverityMinor, "Spell the flag --no-cache", domain.ItemOpen)
			nit := fbrItem("ux-r1-2", domain.SeverityNit, "Capitalise the help text", domain.ItemOpen)
			accepted, acceptedNit := item, nit
			accepted.Status, acceptedNit.Status = domain.ItemAccepted, domain.ItemAccepted
			sc := fbrScript("pushback-accepted")
			fbrEngineer(sc, 1, fbrReport("add the flag and document it"))
			fbrTester(sc, domain.LensUX, 1, events.TaskReview, fbrChanges("naming", item, nit))
			fbrTester(sc, domain.LensQA, 1, events.TaskReview, fbrApprove("behaviour is right"))
			fbrTester(sc, domain.LensTest, 1, events.TaskReview, fbrApprove("covered"))
			fbrEngineer(sc, 2, agents.ResponseSet{Responses: []domain.Response{
				{ItemID: item.ID, Action: domain.ActionPushback, Rationale: "every other flag in the CLI is positive; --cache=false already works"},
				{ItemID: nit.ID, Action: domain.ActionWontfix, Rationale: "cobra lower-cases every other help string"},
			}})
			fbrTester(sc, domain.LensUX, 2, events.TaskEvaluatePushback, fbrApprove("fair, the rationale holds", accepted, acceptedNit))
			return sc
		},
		steps:   10,
		fixture: fbScenarioC,
	}, {
		letter: "d",
		what:   "the tester holds, the product owner upholds the engineer",
		script: func() *mock.Scenario {
			item := fbrItem("qa-r1-1", domain.SeverityMajor, "Validate the timeout before use", domain.ItemOpen)
			held := item
			held.Status = domain.ItemHeld
			sc := fbrScript("arbitration-upholds-engineer")
			fbrEngineer(sc, 1, fbrReport("raise the timeout to 30s"))
			fbrTester(sc, domain.LensUX, 1, events.TaskReview, fbrApprove("fine"))
			fbrTester(sc, domain.LensQA, 1, events.TaskReview, fbrChanges("one major", item))
			fbrTester(sc, domain.LensTest, 1, events.TaskReview, fbrApprove("fine"))
			fbrEngineer(sc, 2, agents.ResponseSet{Responses: []domain.Response{
				{ItemID: item.ID, Action: domain.ActionPushback, Rationale: "the config layer already rejects a negative timeout"},
			}})
			fbrTester(sc, domain.LensQA, 2, events.TaskEvaluatePushback, fbrChanges("still want the guard", held))
			fbrPO(sc, "po-uphold-engineer", events.TaskArbitrate, domain.Arbitration{
				ItemID: item.ID, Ruling: domain.RulingUpholdEngineer,
				Rationale: "config validation covers it; a second check is dead code",
			})
			return sc
		},
		tweak:   func(o *orchestrator.FeedbackOptions) { o.Limits.MaxPushbackRounds = 1 },
		steps:   12,
		fixture: fbScenarioD,
	}, {
		letter: "e",
		what:   "the product owner upholds the tester and the engineer fixes the item",
		script: func() *mock.Scenario {
			// A rules violation is always a blocker the engineer has to fix
			// (P5-05 refuses any other answer), so the item the tester holds
			// on to and the product owner rules on is a plain major beside
			// it. Answering one item with a fix and the other with a
			// pushback is what makes round two fire the test lens twice:
			// once to judge the pushback, once to re-review the fix (P6-04).
			rules := fbrItem("test-r1-1", domain.SeverityBlocker, "No test for the empty-body case", domain.ItemOpen)
			rules.Category = domain.CategoryRules
			disputed := fbrItem("test-r1-2", domain.SeverityMajor, "Name the reader parameter", domain.ItemOpen)
			rulesFixed, held, fixed := rules, disputed, disputed
			rulesFixed.Status = domain.ItemResolved
			held.Status = domain.ItemHeld
			fixed.Status = domain.ItemResolved
			sc := fbrScript("arbitration-upholds-tester")
			fbrEngineer(sc, 1, fbrReport("stream instead of buffering"))
			fbrTester(sc, domain.LensUX, 1, events.TaskReview, fbrApprove("no interface change"))
			fbrTester(sc, domain.LensQA, 1, events.TaskReview, fbrApprove("streams as advertised"))
			fbrTester(sc, domain.LensTest, 1, events.TaskReview, fbrChanges("missing coverage", rules, disputed))
			fbrEngineer(sc, 2, agents.ResponseSet{Responses: []domain.Response{
				{ItemID: rules.ID, Action: domain.ActionFixed, Rationale: "added TestStreamEmptyBody"},
				{ItemID: disputed.ID, Action: domain.ActionPushback, Rationale: "the parameter is named after the interface it takes"},
			}})
			fbrTester(sc, domain.LensTest, 2, events.TaskEvaluatePushback, fbrChanges("rules say every parameter reads as prose", held))
			fbrTester(sc, domain.LensTest, 2, events.TaskReview, fbrApprove("the empty-body test is there", rulesFixed))
			fbrPO(sc, "po-uphold-tester", events.TaskArbitrate, domain.Arbitration{
				ItemID: disputed.ID, Ruling: domain.RulingUpholdTester,
				Instruction: "rename it to body", Rationale: "the reviewer reads the code more often than the author",
			})
			fbrEngineer(sc, 3, agents.ResponseSet{Responses: []domain.Response{
				{ItemID: disputed.ID, Action: domain.ActionFixed, Rationale: "renamed it to body"},
			}})
			fbrTester(sc, domain.LensTest, 3, events.TaskReview, fbrApprove("reads well now", rulesFixed, fixed))
			return sc
		},
		tweak:   func(o *orchestrator.FeedbackOptions) { o.Limits.MaxPushbackRounds = 1 },
		diffs:   []fbrDiffStep{{}, {}, {}, {"b4c5d6e7", 24, 0.1}},
		steps:   14,
		fixture: fbScenarioE,
	}, {
		letter: "f",
		what:   "the round cap escalates and the product owner blocks the bead",
		script: func() *mock.Scenario {
			sc := fbrScript("round-cap")
			// The item is raised once and carried forward with its own id
			// every round: a re-review that renumbered it would be refused
			// by the verdict schema (P5-05).
			item := fbrItem("qa-r1-1", domain.SeverityBlocker,
				"Pool still leaks a connection per retry", domain.ItemOpen)
			for round := 1; round <= 5; round++ {
				if round == 1 {
					fbrEngineer(sc, round, fbrReport("attempt 1 at the pool"))
				} else {
					fbrEngineer(sc, round, agents.ResponseSet{Responses: []domain.Response{
						{ItemID: item.ID, Action: domain.ActionFixed,
							Rationale: fmt.Sprintf("attempt %d at the pool", round)},
					}})
				}
				for _, l := range domain.Lenses() {
					v := fbrApprove(l.String() + " is happy")
					if l == domain.LensQA {
						v = fbrChanges("still leaking", item)
					}
					fbrTester(sc, l, round, events.TaskReview, v)
				}
			}
			fbrPO(sc, "po-escalate", events.TaskEscalate, domain.Escalation{
				Outcome: domain.EscalationBlock,
				Summary: "five rounds without a fix; the pool needs a design a human signs off",
			})
			return sc
		},
		diffs: []fbrDiffStep{
			{}, {"b2b2b2b2", 22, 0.1}, {"c3c3c3c3", 22, 0.1}, {"d4d4d4d4", 22, 0.1}, {"e5e5e5e5", 22, 0.1},
		},
		steps:   16,
		fixture: fbScenarioF,
	}, {
		letter: "g",
		what:   "the diff moved enough that every tester re-reviews (D-09)",
		script: func() *mock.Scenario {
			item := fbrItem("qa-r1-1", domain.SeverityMajor, "Rebase dropped the timeout guard", domain.ItemOpen)
			resolved := item
			resolved.Status = domain.ItemResolved
			sc := fbrScript("full-re-review")
			fbrEngineer(sc, 1, fbrReport("move the transport into its own package"))
			fbrTester(sc, domain.LensUX, 1, events.TaskReview, fbrApprove("nothing user facing"))
			fbrTester(sc, domain.LensQA, 1, events.TaskReview, fbrChanges("one major", item))
			fbrTester(sc, domain.LensTest, 1, events.TaskReview, fbrApprove("tests moved with it"))
			fbrEngineer(sc, 2, agents.ResponseSet{Responses: []domain.Response{
				{ItemID: item.ID, Action: domain.ActionFixed, Rationale: "restored the guard and moved it into the new package"},
			}})
			fbrTester(sc, domain.LensQA, 2, events.TaskReview, fbrApprove("guard is back", resolved))
			fbrTester(sc, domain.LensUX, 2, events.TaskReview, fbrApprove("still nothing user facing"))
			fbrTester(sc, domain.LensTest, 2, events.TaskReview, fbrApprove("the rewrite is still covered"))
			return sc
		},
		diffs:   []fbrDiffStep{{}, {"d6e7f809", 400, 0.9}},
		steps:   12,
		fixture: fbScenarioG,
	}}
}

// fbrRecord runs one scenario and returns the rig and everything the run
// published, in order.
func fbrRecord(t *testing.T, s fbrScenario) (*fbrRig, []events.Event) {
	t.Helper()
	rig := fbrSetup(t, s.script(), s.tweak)
	for step := range s.steps {
		if step < len(s.diffs) {
			if d := s.diffs[step]; d.head != "" {
				rig.setDiff(d.head, d.lines, d.ratio)
			}
		}
		if !rig.step(t) {
			return rig, rig.events()
		}
	}
	t.Fatalf("scenario (%s) did not settle in %d steps", s.letter, s.steps)
	return nil, nil
}

// --- the trace: what a scenario's events say, without the prose ---------

// fbrTrace reduces an event stream to the shape the Feedback view draws: the
// rounds that were opened and with which lenses, the reports and answers the
// engineer gave, the verdicts each lens returned with the id, severity and
// status of every item in them, and the product owner's rulings. Titles,
// summaries, rationales, commit shas, agent ids and timestamps are left out
// — they are the fixture's to choose, and the recording's to prove nothing
// about. What is left is exactly what would change if the loop stopped
// opening a round, stopped sending a lens back or stopped settling an item.
func fbrTrace(evs []events.Event) []string {
	var out []string
	for _, ev := range evs {
		switch e := ev.(type) {
		case events.RoundStarted:
			out = append(out, fmt.Sprintf("round %d %s lenses %s", e.Round, e.RoundKind, lensList(e.Lenses)))
		case events.ResponseReceived:
			if e.Report != nil {
				out = append(out, fmt.Sprintf("round %d report", e.Round))
			}
			for _, r := range e.Responses {
				out = append(out, fmt.Sprintf("round %d answer %s %s", e.Round, r.ItemID, r.Action))
			}
		case events.VerdictReceived:
			line := fmt.Sprintf("round %d verdict %s %s", e.Round, orDash(string(e.Verdict.Lens)), e.Verdict.Outcome)
			for _, it := range e.Verdict.Items {
				line += fmt.Sprintf(" [%s %s %s]", it.ID, it.Severity, orDash(string(it.Status)))
			}
			out = append(out, line)
		case events.ArbitrationReceived:
			for _, r := range e.Rulings {
				out = append(out, fmt.Sprintf("round %d ruling %s %s", e.Round, r.ItemID, r.Ruling))
			}
			if e.Escalation != nil {
				out = append(out, fmt.Sprintf("round %d escalation %s", e.Round, e.Escalation.Outcome))
			}
		}
	}
	return out
}

// --- the recording, and what it is compared with ------------------------

// fbrRecording renders a scenario's events as the JSONL of a run log.
//
// Two things in a real stream are not reproducible and are dealt with here.
// [events.AgentOutput] is stamped by the agents layer with the wall clock
// rather than with the run's, so the transcript lines are left out — the
// Feedback view does not read them, and a recording has to be byte-for-byte
// the same on every machine. And the run's own paths are temporary
// directories, so the home the run was recorded under and the hash bd
// derives from it are replaced with fixed text.
func fbrRecording(t *testing.T, home string, evs []events.Event) string {
	t.Helper()
	var b strings.Builder
	w := events.NewJSONLWriter(&b)
	for _, ev := range evs {
		if ev.Kind() == events.KindAgentOutput {
			continue
		}
		require.NoError(t, w.Write(ev))
	}
	require.NoError(t, w.Err())
	out := strings.ReplaceAll(b.String(), home, "/recorded")
	return fbrRepoHash.ReplaceAllString(out, "repo-000000000000")
}

// fbrRepoHash matches the repository id bd derives from a repository's path.
var fbrRepoHash = regexp.MustCompile(`repo-[0-9a-f]{12}`)

// fbrPath is where a scenario's recording lives. The name ends in
// `.golden.jsonl` so `make check-golden` polices it like every other
// generated fixture (its pathspec is `*.golden` `*.golden.*`).
func fbrPath(letter string) string {
	return filepath.Join(fbrDir, "feedback-"+letter+".golden.jsonl")
}

// fbrGolden compares a recording with the committed one; UPDATE_GOLDEN=1
// (make update-golden) rewrites it instead.
func fbrGolden(t *testing.T, letter, got string) {
	t.Helper()
	path := fbrPath(letter)
	if os.Getenv("UPDATE_GOLDEN") != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "missing recording %s; run `make update-golden`", path)
	require.Equal(t, string(want), got,
		"the feedback loop no longer publishes what %s recorded; run `make update-golden` if the change is intended", path)
}

// TestFeedbackFixturesMatchTheRecordedLoop is P7-14's acceptance test. Each
// P6-04 scenario is run against the real loop, its events are recorded, and
// the recording is required to be both what the committed one holds and the
// shape the hand-written fixture describes.
//
// A fixture may stop short of its recording — a frame is often drawn while a
// round is still waiting on a tester, which is exactly what (g) pins — so
// the rule is that the fixture's trace is a prefix of the recording's. It
// may never invent a round, a lens or an item status the loop does not
// produce.
func TestFeedbackFixturesMatchTheRecordedLoop(t *testing.T) {
	for _, s := range fbrScenarios() {
		t.Run(s.letter, func(t *testing.T) {
			rig, evs := fbrRecord(t, s)
			fbrGolden(t, s.letter, fbrRecording(t, rig.home, evs))

			got, want := fbrTrace(evs), fbrTrace(s.fixture(&fbClock{}))
			require.NotEmpty(t, want, "scenario (%s) has no fixture", s.letter)
			require.LessOrEqualf(t, len(want), len(got),
				"the (%s) fixture — %s — has more in it than the loop produced:\n%s\nrecorded:\n%s",
				s.letter, s.what, strings.Join(want, "\n"), strings.Join(got, "\n"))
			require.Equalf(t, want, got[:len(want)],
				"the (%s) fixture — %s — is not what the feedback loop publishes", s.letter, s.what)
		})
	}
}

// TestFeedbackGoldenFromTheRecordedLoop renders a frame from a recording
// rather than from a fixture: the file is read back with [ReplayFile], the
// stream is folded into a [State] and the view draws it. It is the (e)
// scenario — a rules violation fixed, a pushback held, a ruling and a
// re-review, which is every part of the pane at once.
func TestFeedbackGoldenFromTheRecordedLoop(t *testing.T) {
	script, err := ReplayFile(fbrPath("e"), -1)
	require.NoError(t, err, "run `make update-golden` to record the scenarios")
	st := NewState(ReplayRun(script))
	for _, ev := range ReplayEvents(script) {
		st.Apply(ev)
	}
	threads := FeedbackThreads(st)
	require.Len(t, threads, 1, "the recording is one bead's thread")
	require.Equal(t, fbrBead, threads[0].Bead)

	v := fbView(110, 26, FeedbackOptions{})
	fbGolden(t, "feedback-recorded", fbFrame(t, v, st, 110, 26))
}
