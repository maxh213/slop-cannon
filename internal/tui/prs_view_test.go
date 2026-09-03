package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/testutil"
)

// prsGoldenDir is where the PRs view's golden frames live.
const prsGoldenDir = "../../testdata/tui"

// prsRunID names the run every PRs-view fixture watches.
const prsRunID = domain.RunID("sc-prs-0001")

// prsAt returns a deterministic timestamp, n minutes after a fixed start.
func prsAt(n int) time.Time {
	return time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Minute)
}

// prsMeta stamps the nth event of the fixture run.
func prsMeta(n int) events.Meta { return events.MetaAt(prsRunID, prsAt(n)) }

// prsRunInfo builds the run the fixtures watch.
func prsRunInfo(safety bool) domain.Run {
	return domain.Run{
		ID:        prsRunID,
		RepoPath:  "/tmp/repo",
		Mode:      domain.RunModeBrief,
		Brief:     "fix the retry bugs",
		StartedAt: prsAt(0),
		Config:    domain.ConfigSnapshot{Ammo: 4, Safety: safety, Cartridge: "mock", ReviewPRs: true},
	}
}

// prsFixtureEvents is the scripted delivery history the golden frames are
// rendered from: one bead merged behind a pull request, one waiting for a
// human merge, one whose checks came back failing, one merged straight to
// the default branch, and one external pull request the cannon reviewed and
// approved.
func prsFixtureEvents() []events.Event {
	bead := func(id domain.BeadID, title string, state domain.BeadState, round int) domain.Bead {
		return domain.Bead{ID: id, Title: title, Type: domain.BeadTypeTask, Priority: domain.PriorityDefault, State: state, Round: round}
	}
	return []events.Event{
		events.RunStarted{Meta: prsMeta(0), Info: prsRunInfo(true)},

		events.BeadCreated{Meta: prsMeta(1), Bead: bead("sc-1a2b", "Retry failed HTTP requests", domain.BeadStateClosed, 2)},
		events.PROpened{Meta: prsMeta(2), Bead: "sc-1a2b", URL: "https://example.invalid/acme/app/pull/42", Number: 42, Branch: "sc/sc-1a2b-retry", Base: "main"},
		events.Merged{Meta: prsMeta(3), Bead: "sc-1a2b", SHA: "2b7d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f", Branch: "main", PRNumber: 42, Pushed: true},

		events.BeadCreated{Meta: prsMeta(4), Bead: bead("sc-3c4d", "Document the retry policy", domain.BeadStateDelivering, 1)},
		events.PROpened{Meta: prsMeta(5), Bead: "sc-3c4d", URL: "https://example.invalid/acme/app/pull/43", Number: 43, Branch: "sc/sc-3c4d-docs", Base: "main"},

		events.BeadCreated{Meta: prsMeta(6), Bead: bead("sc-5e6f", "Cache DNS lookups", domain.BeadStateChangesRequested, 2)},
		events.PROpened{Meta: prsMeta(7), Bead: "sc-5e6f", URL: "https://example.invalid/acme/app/pull/44", Number: 44, Branch: "sc/sc-5e6f-dns", Base: "main"},
		events.RoundStarted{Meta: prsMeta(8), Bead: "sc-5e6f", Round: 2, RoundKind: events.RoundCheckFailed, Lenses: []domain.Lens{domain.LensQA}, Commit: "9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c"},

		events.BeadCreated{Meta: prsMeta(9), Bead: bead("sc-7g8h", "Drop the dead flag", domain.BeadStateClosed, 1)},
		events.Merged{Meta: prsMeta(10), Bead: "sc-7g8h", SHA: "9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c", Branch: "main", Pushed: false},

		events.BeadCreated{Meta: prsMeta(11), Bead: bead("sc-9x0y", "Review PR #77: bump the client timeout", domain.BeadStateClosed, 1)},
		events.PROpened{Meta: prsMeta(12), Bead: "sc-9x0y", URL: "https://example.invalid/acme/app/pull/77", Number: 77, Branch: "contrib/timeout", Base: "main", External: true},
		events.RoundStarted{Meta: prsMeta(13), Bead: "sc-9x0y", Round: 1, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensQA, domain.LensTest}},
		events.VerdictReceived{Meta: prsMeta(14), Bead: "sc-9x0y", Round: 1, Agent: "qa-1", Verdict: domain.Verdict{Lens: domain.LensQA, Round: 1, Outcome: domain.VerdictApprove, Summary: "looks right"}},
		events.VerdictReceived{Meta: prsMeta(15), Bead: "sc-9x0y", Round: 1, Agent: "test-1", Verdict: domain.Verdict{Lens: domain.LensTest, Round: 1, Outcome: domain.VerdictApprove, Summary: "no new surface"}},
	}
}

// prsState folds evs into a fresh state for the run.
func prsState(safety bool, evs []events.Event) *State {
	st := NewState(prsRunInfo(safety))
	for _, ev := range evs {
		st.Apply(ev)
	}
	return st
}

// prsView builds the view sized to the main pane of a w×h terminal.
func prsView(w, h int, opts PRsOptions) PRsModel {
	opts.NoColor = true
	return NewPRsModel(opts).Resize(w, h-chromeHeight)
}

// prsFrame renders the whole terminal frame with the PRs view in the main
// pane: what [Model.View] produces once it dispatches [TabPRs] to a
// [PRsModel].
func prsFrame(t *testing.T, v PRsModel, st *State, w, h int) string {
	t.Helper()
	m := New(t.Context(), Options{Run: st.Run, NoColor: true})
	m.state = st
	m.tab = TabPRs
	m.width, m.height = w, h
	return strings.Join([]string{
		m.header(w),
		m.tabBar(w),
		m.styles.Rule.Render(strings.Repeat("─", max(w, 1))),
		v.View(st),
		m.footer(w),
	}, "\n")
}

// prsGolden compares got with testdata/tui/<name>.golden; UPDATE_GOLDEN=1
// (make update-golden) rewrites it instead.
func prsGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(prsGoldenDir, name+".golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "missing golden %s; run `make update-golden`", path)
	require.Equal(t, string(want), got, "output differs from %s; run `make update-golden` if the change is intended", path)
}

// TestPRsViewGolden is the P7-05 acceptance test: golden frames of the
// deliveries list, wide and narrow, with the cursor moved, empty, and with
// safety off.
func TestPRsViewGolden(t *testing.T) {
	t.Run("wide", func(t *testing.T) {
		st := prsState(true, prsFixtureEvents())
		prsGolden(t, "prs", prsFrame(t, prsView(100, 20, PRsOptions{}), st, 100, 20))
	})

	t.Run("narrow", func(t *testing.T) {
		st := prsState(true, prsFixtureEvents())
		prsGolden(t, "prs-narrow", prsFrame(t, prsView(64, 16, PRsOptions{}), st, 64, 16))
	})

	t.Run("cursor on the external pull request", func(t *testing.T) {
		st := prsState(true, prsFixtureEvents())
		v := prsView(100, 20, PRsOptions{})
		for range len(st.Deliveries) {
			v, _, _ = v.Update(prsKey("down"), st, DefaultKeyMap())
		}
		require.Equal(t, len(st.Deliveries)-1, v.Cursor())
		prsGolden(t, "prs-external", prsFrame(t, v, st, 100, 20))
	})

	t.Run("empty", func(t *testing.T) {
		st := prsState(false, []events.Event{events.RunStarted{Meta: prsMeta(0), Info: prsRunInfo(false)}})
		prsGolden(t, "prs-empty", prsFrame(t, prsView(100, 20, PRsOptions{}), st, 100, 20))
	})

	t.Run("safety off", func(t *testing.T) {
		st := prsState(false, []events.Event{
			events.RunStarted{Meta: prsMeta(0), Info: prsRunInfo(false)},
			events.BeadCreated{Meta: prsMeta(1), Bead: domain.Bead{ID: "sc-1a2b", Title: "Retry failed HTTP requests", State: domain.BeadStateClosed, Priority: domain.PriorityDefault}},
			events.Merged{Meta: prsMeta(2), Bead: "sc-1a2b", SHA: "2b7d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f", Branch: "main", Pushed: true},
		})
		prsGolden(t, "prs-safety-off", prsFrame(t, prsView(100, 20, PRsOptions{}), st, 100, 20))
	})
}

// TestPRsRowsDerivation checks the status, check and review columns the view
// derives from the deliveries, beads and rounds.
func TestPRsRowsDerivation(t *testing.T) {
	rows := PRRows(prsState(true, prsFixtureEvents()))
	require.Len(t, rows, 5)

	byBead := map[domain.BeadID]PRRow{}
	for _, r := range rows {
		byBead[r.Bead] = r
	}

	merged := byBead["sc-1a2b"]
	require.Equal(t, DeliveryMerged, merged.Status)
	require.Equal(t, ChecksPassed, merged.Checks)
	require.Equal(t, "#42", merged.PR())
	require.Equal(t, "https://example.invalid/acme/app/pull/42", merged.Ref())
	require.Equal(t, "Retry failed HTTP requests", merged.Title)

	waiting := byBead["sc-3c4d"]
	require.Equal(t, DeliveryAwaitingMerge, waiting.Status)
	require.Equal(t, ChecksPending, waiting.Checks)

	failing := byBead["sc-5e6f"]
	require.Equal(t, DeliveryAwaitingMerge, failing.Status)
	require.Equal(t, ChecksFailing, failing.Checks)
	require.Equal(t, "failing", failing.Note())

	direct := byBead["sc-7g8h"]
	require.Equal(t, DeliveryMerged, direct.Status)
	require.Equal(t, ChecksUnknown, direct.Checks)
	require.Equal(t, "-", direct.PR())
	require.Equal(t, "9f8e7d6", direct.Ref())

	external := byBead["sc-9x0y"]
	require.Equal(t, DeliveryReviewed, external.Status)
	require.Equal(t, domain.VerdictApprove, external.Review)
	require.Equal(t, "approve", external.Note())
	require.Equal(t, "ext #77", external.PR())
	require.Equal(t, ChecksUnknown, external.Checks, "an external PR has a review, not checks")
}

// TestPRsRowsEdgeCases covers the states the fixture does not reach.
func TestPRsRowsEdgeCases(t *testing.T) {
	t.Run("nil state", func(t *testing.T) {
		require.Nil(t, PRRows(nil))
	})

	t.Run("conflict, needs human, failed and an unreviewed external PR", func(t *testing.T) {
		st := prsState(true, []events.Event{
			events.RunStarted{Meta: prsMeta(0), Info: prsRunInfo(true)},
			events.BeadCreated{Meta: prsMeta(1), Bead: domain.Bead{ID: "sc-aaa", Title: "conflicted", State: domain.BeadStateDelivering, Priority: domain.PriorityDefault}},
			events.PROpened{Meta: prsMeta(2), Bead: "sc-aaa", URL: "https://example.invalid/acme/app/pull/1", Number: 1},
			events.RoundStarted{Meta: prsMeta(3), Bead: "sc-aaa", Round: 1, RoundKind: events.RoundConflict},
			events.BeadCreated{Meta: prsMeta(4), Bead: domain.Bead{ID: "sc-bbb", Title: "parked", State: domain.BeadStateNeedsHuman, Priority: domain.PriorityDefault}},
			events.PROpened{Meta: prsMeta(5), Bead: "sc-bbb", URL: "https://example.invalid/acme/app/pull/2", Number: 2},
			events.BeadCreated{Meta: prsMeta(6), Bead: domain.Bead{ID: "sc-ccc", Title: "abandoned", State: domain.BeadStateFailed, Priority: domain.PriorityDefault}},
			events.PROpened{Meta: prsMeta(7), Bead: "sc-ccc", URL: "https://example.invalid/acme/app/pull/3", Number: 3},
			events.BeadCreated{Meta: prsMeta(8), Bead: domain.Bead{ID: "sc-ddd", Title: "unreviewed external", State: domain.BeadStateInReview, Priority: domain.PriorityDefault}},
			events.PROpened{Meta: prsMeta(9), Bead: "sc-ddd", URL: "https://example.invalid/acme/app/pull/4", Number: 4, External: true},
		})
		rows := PRRows(st)
		require.Len(t, rows, 4)
		require.Equal(t, DeliveryAwaitingMerge, rows[0].Status)
		require.Equal(t, ChecksConflict, rows[0].Checks)
		require.Equal(t, DeliveryNeedsHuman, rows[1].Status)
		require.Equal(t, DeliveryFailed, rows[2].Status)
		require.Equal(t, DeliveryReviewing, rows[3].Status)
		require.Equal(t, "-", rows[3].Note())
	})

	t.Run("a delivery with no pull request and no merge is open", func(t *testing.T) {
		st := prsState(true, []events.Event{
			events.RunStarted{Meta: prsMeta(0), Info: prsRunInfo(true)},
			events.PROpened{Meta: prsMeta(1), Bead: "sc-fff"},
		})
		rows := PRRows(st)
		require.Len(t, rows, 1)
		require.Equal(t, DeliveryOpen, rows[0].Status)
		require.Equal(t, "-", rows[0].PR())
		require.Equal(t, "-", rows[0].Ref())
		require.Equal(t, UnknownGlyph, DeliveryStatus("nonsense").Glyph())
	})

	t.Run("external review with a change request", func(t *testing.T) {
		st := prsState(true, []events.Event{
			events.RunStarted{Meta: prsMeta(0), Info: prsRunInfo(true)},
			events.PROpened{Meta: prsMeta(1), Bead: "sc-eee", URL: "https://example.invalid/acme/app/pull/9", Number: 9, External: true},
			events.RoundStarted{Meta: prsMeta(2), Bead: "sc-eee", Round: 1, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensQA, domain.LensTest}},
			events.VerdictReceived{Meta: prsMeta(3), Bead: "sc-eee", Round: 1, Verdict: domain.Verdict{Lens: domain.LensQA, Round: 1, Outcome: domain.VerdictApprove}},
			events.VerdictReceived{Meta: prsMeta(4), Bead: "sc-eee", Round: 1, Verdict: domain.Verdict{Lens: domain.LensTest, Round: 1, Outcome: domain.VerdictChanges}},
		})
		rows := PRRows(st)
		require.Len(t, rows, 1)
		require.Equal(t, DeliveryReviewed, rows[0].Status)
		require.Equal(t, domain.VerdictChanges, rows[0].Review)
	})

	t.Run("a closed bead with no merge event", func(t *testing.T) {
		st := prsState(true, []events.Event{
			events.RunStarted{Meta: prsMeta(0), Info: prsRunInfo(true)},
			events.BeadCreated{Meta: prsMeta(1), Bead: domain.Bead{ID: "sc-ggg", Title: "landed before the resume", State: domain.BeadStateClosed, Priority: domain.PriorityDefault}},
			events.PROpened{Meta: prsMeta(2), Bead: "sc-ggg", URL: "https://example.invalid/acme/app/pull/5", Number: 5},
		})
		rows := PRRows(st)
		require.Len(t, rows, 1)
		require.Equal(t, DeliveryClosed, rows[0].Status)
	})
}

// prsExternalEvents scripts an external pull request under review by lenses,
// with the first n verdicts in. It is how the "reviewed" status is checked
// against a round that has not finished.
func prsExternalEvents(lenses []domain.Lens, verdicts []domain.Verdict) []events.Event {
	evs := []events.Event{
		events.RunStarted{Meta: prsMeta(0), Info: prsRunInfo(true)},
		events.BeadCreated{Meta: prsMeta(1), Bead: domain.Bead{ID: "sc-ext", Title: "Review PR #88", Type: domain.BeadTypeTask, Priority: domain.PriorityDefault, State: domain.BeadStateInReview, Round: 1}},
		events.PROpened{Meta: prsMeta(2), Bead: "sc-ext", URL: "https://example.invalid/acme/app/pull/88", Number: 88, External: true},
		events.RoundStarted{Meta: prsMeta(3), Bead: "sc-ext", Round: 1, RoundKind: events.RoundReview, Lenses: lenses},
	}
	for i, v := range verdicts {
		evs = append(evs, events.VerdictReceived{Meta: prsMeta(4 + i), Bead: "sc-ext", Round: 1, Verdict: v})
	}
	return evs
}

// TestPRsExternalReviewIsOnlyReportedOnceItIsPosted pins the rule the whole
// external-PR column rests on: the testers post one consolidated review when
// the round finishes (D-11, gitx.BuildReview), so a round that is still
// collecting verdicts must not be shown as reviewed — the word could still
// flip from "approve" to "changes" when the next tester reports.
func TestPRsExternalReviewIsOnlyReportedOnceItIsPosted(t *testing.T) {
	lenses := []domain.Lens{domain.LensUX, domain.LensQA, domain.LensTest}
	approve := func(l domain.Lens) domain.Verdict {
		return domain.Verdict{Lens: l, Round: 1, Outcome: domain.VerdictApprove}
	}

	t.Run("one verdict of three is still being reviewed", func(t *testing.T) {
		rows := PRRows(prsState(true, prsExternalEvents(lenses, []domain.Verdict{approve(domain.LensUX)})))
		require.Len(t, rows, 1)
		require.Equal(t, DeliveryReviewing, rows[0].Status)
		require.Empty(t, rows[0].Review, "no review has been posted yet")
		require.Equal(t, 1, rows[0].ReviewsIn)
		require.Equal(t, 3, rows[0].ReviewsExpected)
		require.Equal(t, "1/3", rows[0].Note(), "the column counts verdicts, it does not guess the outcome")
	})

	t.Run("the last verdict posts the review", func(t *testing.T) {
		rows := PRRows(prsState(true, prsExternalEvents(lenses, []domain.Verdict{
			approve(domain.LensUX), approve(domain.LensQA), approve(domain.LensTest),
		})))
		require.Equal(t, DeliveryReviewed, rows[0].Status)
		require.Equal(t, domain.VerdictApprove, rows[0].Review)
		require.Equal(t, "approve", rows[0].Note())
	})

	t.Run("one changes verdict makes the whole review a change request", func(t *testing.T) {
		rows := PRRows(prsState(true, prsExternalEvents(lenses, []domain.Verdict{
			approve(domain.LensUX),
			{Lens: domain.LensQA, Round: 1, Outcome: domain.VerdictChanges},
			approve(domain.LensTest),
		})))
		require.Equal(t, DeliveryReviewed, rows[0].Status)
		require.Equal(t, domain.VerdictChanges, rows[0].Review)
	})

	t.Run("a round that never announced its lenses cannot be known to be done", func(t *testing.T) {
		rows := PRRows(prsState(true, prsExternalEvents(nil, []domain.Verdict{approve(domain.LensQA)})))
		require.Equal(t, DeliveryReviewing, rows[0].Status)
		require.Empty(t, rows[0].Review)
		require.Equal(t, "1/?", rows[0].Note())
	})

	t.Run("a closed review bead is reviewed whatever the round said", func(t *testing.T) {
		evs := append(prsExternalEvents(nil, []domain.Verdict{approve(domain.LensQA)}),
			events.BeadStateChanged{Meta: prsMeta(9), Bead: "sc-ext", From: domain.BeadStateInReview, To: domain.BeadStateClosed})
		rows := PRRows(prsState(true, evs))
		require.Equal(t, DeliveryReviewed, rows[0].Status)
		require.Equal(t, domain.VerdictApprove, rows[0].Review)
	})

	t.Run("the view says reviewing, not reviewed", func(t *testing.T) {
		st := prsState(true, prsExternalEvents(lenses, []domain.Verdict{approve(domain.LensUX)}))
		out := prsView(100, 20, PRsOptions{}).View(st)
		require.Contains(t, out, DeliveryReviewing.Glyph()+" reviewing")
		require.NotContains(t, out, "reviewed")
		require.NotContains(t, out, "approve")
	})
}

// prsAbandonedEvents scripts one abandoned and one parked delivery, the two
// deliveries nobody is pushing any more.
func prsAbandonedEvents() []events.Event {
	return []events.Event{
		events.RunStarted{Meta: prsMeta(0), Info: prsRunInfo(true)},
		events.BeadCreated{Meta: prsMeta(1), Bead: domain.Bead{ID: "sc-dead", Title: "abandoned", Type: domain.BeadTypeTask, Priority: domain.PriorityDefault, State: domain.BeadStateFailed}},
		events.PROpened{Meta: prsMeta(2), Bead: "sc-dead", URL: "https://example.invalid/acme/app/pull/6", Number: 6},
		events.BeadCreated{Meta: prsMeta(3), Bead: domain.Bead{ID: "sc-park", Title: "parked", Type: domain.BeadTypeTask, Priority: domain.PriorityDefault, State: domain.BeadStateNeedsHuman}},
		events.PROpened{Meta: prsMeta(4), Bead: "sc-park", URL: "https://example.invalid/acme/app/pull/7", Number: 7},
	}
}

// TestPRsAbandonedWorkIsNotInFlight checks that work nobody is pushing any
// more is neither counted as in flight nor shown with checks still running.
func TestPRsAbandonedWorkIsNotInFlight(t *testing.T) {
	st := prsState(true, prsAbandonedEvents())
	rows := PRRows(st)
	require.Len(t, rows, 2)
	require.Equal(t, DeliveryFailed, rows[0].Status)
	require.Equal(t, ChecksUnknown, rows[0].Checks, "nothing is running on an abandoned delivery")
	require.Equal(t, "-", rows[0].Note())
	require.Equal(t, DeliveryNeedsHuman, rows[1].Status)
	require.Equal(t, ChecksUnknown, rows[1].Checks)

	out := prsView(100, 20, PRsOptions{}).View(st)
	require.NotContains(t, out, "in flight")
	require.Contains(t, out, "2 delivered, 1 needs a human, 1 abandoned")
	require.NotContains(t, out, "pending")

	t.Run("a failing check is still reported on a parked delivery", func(t *testing.T) {
		evs := append(prsAbandonedEvents(),
			events.RoundStarted{Meta: prsMeta(5), Bead: "sc-park", Round: 1, RoundKind: events.RoundCheckFailed, Lenses: []domain.Lens{domain.LensQA}})
		rows := PRRows(prsState(true, evs))
		require.Equal(t, ChecksFailing, rows[1].Checks, "the last thing known about the checks still stands")
	})
}

// TestPRsCanonicalTerminal checks the 80x24 terminal every other golden frame
// is rendered at: the pull request URL must be readable somewhere on screen,
// and the counts must survive.
func TestPRsCanonicalTerminal(t *testing.T) {
	const w, h = 80, 24
	st := prsState(true, prsFixtureEvents())
	out := prsView(w, h, PRsOptions{}).View(st)

	t.Run("the selected PR URL is on screen in full", func(t *testing.T) {
		require.Contains(t, out, "https://example.invalid/acme/app/pull/42")
	})

	t.Run("no line is wider than the terminal", func(t *testing.T) {
		for _, l := range strings.Split(out, "\n") {
			require.LessOrEqual(t, lipgloss.Width(l), w, "line wider than the terminal: %q", l)
		}
	})

	t.Run("a cut REF column is dropped rather than shown cut", func(t *testing.T) {
		require.NotContains(t, out, "REF")
		require.NotContains(t, out, "https://example.invalid/acme/app/pull/43", "only the selected row's URL is shown")
	})

	t.Run("the counts survive", func(t *testing.T) {
		require.Contains(t, out, "1 external")
		require.Contains(t, strings.Split(out, "\n")[0], "4 delivered, 2 merged, 2 in flight, 1 external")
	})

	t.Run("a wide pane keeps the REF column and the URL in it", func(t *testing.T) {
		wide := prsView(prsWideWidth, h, PRsOptions{}).View(st)
		require.Contains(t, wide, "REF")
		require.Contains(t, wide, "https://example.invalid/acme/app/pull/43")
	})

	t.Run("a URL too long for the REF column moves to the detail line", func(t *testing.T) {
		long := "https://example.invalid/acme/a-repository-with-a-very-long-name/pull/123456"
		lst := prsState(true, []events.Event{
			events.RunStarted{Meta: prsMeta(0), Info: prsRunInfo(true)},
			events.PROpened{Meta: prsMeta(1), Bead: "sc-long", URL: long, Number: 123456},
		})
		out := prsView(prsWideWidth, h, PRsOptions{}).View(lst)
		require.Greater(t, len(long), prsRefWidth(prsWideWidth))
		require.Contains(t, strings.Split(out, "\n")[h-chromeHeight-1], long, "the detail line carries what the REF column had to cut")
	})
}

// TestPRsModelSize checks the geometry the view reports back.
func TestPRsModelSize(t *testing.T) {
	w, h := prsView(100, 20, PRsOptions{}).Size()
	require.Equal(t, 100, w)
	require.Equal(t, 20-chromeHeight, h)

	t.Run("never smaller than one cell", func(t *testing.T) {
		w, h := NewPRsModel(PRsOptions{}).Resize(0, -3).Size()
		require.Equal(t, 1, w)
		require.Equal(t, 1, h)
	})
}

// TestPRsOpenThroughTheModel drives `o` through the real program's update
// loop — Model.Update, handleKey, updateView — rather than the view alone, so
// a regression that stopped propagating the command fails here.
func TestPRsOpenThroughTheModel(t *testing.T) {
	st := prsState(true, prsFixtureEvents())
	newModel := func(t *testing.T, open URLOpener) Model {
		t.Helper()
		m := New(t.Context(), Options{Run: st.Run, NoColor: true, Open: open, Now: func() time.Time { return prsAt(20) }})
		m.state = st
		m.tab = TabPRs
		m.width, m.height = 120, 24
		return m
	}

	t.Run("the key reaches the opener and the footer says so", func(t *testing.T) {
		got := make(chan string, 1)
		m := newModel(t, func(_ context.Context, u string) error {
			got <- u
			return nil
		})
		next, cmd := m.Update(prsKey("o"))
		require.NotNil(t, cmd, "`o` must produce the open command through the model")
		msg, ok := cmd().(URLOpenedMsg)
		require.True(t, ok)
		require.NoError(t, msg.Err)
		require.Equal(t, "https://example.invalid/acme/app/pull/42", <-got)

		after, _ := prsModelOf(t, next).Update(msg)
		require.Contains(t, prsFooter(t, after), "opened https://example.invalid/acme/app/pull/42")
	})

	t.Run("a failed open is reported, not swallowed", func(t *testing.T) {
		boom := errors.New("exec: xdg-open: executable file not found in $PATH")
		m := newModel(t, func(context.Context, string) error { return boom })
		next, cmd := m.Update(prsKey("o"))
		require.NotNil(t, cmd)
		after, _ := prsModelOf(t, next).Update(cmd())
		am := prsModelOf(t, after)
		require.Contains(t, prsFooter(t, am), "could not open https://example.invalid/acme/app/pull/42")
		require.Len(t, am.State().Notices, 1)
		require.Equal(t, events.KindError, am.State().Notices[0].Kind)
		require.Equal(t, prsAt(20), am.State().Notices[0].At)
		require.Contains(t, am.State().Notices[0].Err, "xdg-open")

		t.Run("and cleared by the next key press", func(t *testing.T) {
			cleared, _ := am.Update(prsKey("j"))
			require.NotContains(t, prsFooter(t, cleared), "could not open")
		})
	})

	t.Run("the model defaults to the real opener", func(t *testing.T) {
		m := New(t.Context(), Options{Run: st.Run, NoColor: true})
		require.NotNil(t, m.prs.open)
	})
}

// TestPRsViewLayout checks the frame's shape rather than its exact text.
func TestPRsViewLayout(t *testing.T) {
	st := prsState(true, prsFixtureEvents())

	t.Run("fills the pane exactly", func(t *testing.T) {
		lines := strings.Split(prsFrame(t, prsView(100, 20, PRsOptions{}), st, 100, 20), "\n")
		require.Len(t, lines, 20)
		for _, l := range lines {
			require.LessOrEqual(t, lipgloss.Width(l), 100, "line wider than the terminal: %q", l)
		}
	})

	t.Run("a tiny pane still renders", func(t *testing.T) {
		out := prsView(40, 6, PRsOptions{}).View(st)
		require.Len(t, strings.Split(out, "\n"), 2)
	})

	t.Run("scrolls the cursor into view", func(t *testing.T) {
		v := prsView(100, 9, PRsOptions{})
		for range len(st.Deliveries) {
			v, _, _ = v.Update(prsKey("down"), st, DefaultKeyMap())
		}
		out := v.View(st)
		require.Contains(t, out, "sc-9x0y")
		require.NotContains(t, out, "sc-1a2b  ")
	})

	t.Run("narrow drops the reference column", func(t *testing.T) {
		out := prsView(64, 16, PRsOptions{}).View(st)
		require.Contains(t, out, "CHECKS")
		require.NotContains(t, out, "REF")
		require.Contains(t, out, "https://example.invalid/acme/app/pull/42", "the detail line keeps the URL")
	})

	t.Run("status glyphs survive NO_COLOR", func(t *testing.T) {
		out := prsView(100, 20, PRsOptions{}).View(st)
		require.Contains(t, out, DeliveryMerged.Glyph()+" merged")
		require.Contains(t, out, DeliveryAwaitingMerge.Glyph()+" awaiting merge")
		require.Contains(t, out, DeliveryReviewed.Glyph()+" reviewed")
	})

	t.Run("the safety indicator shows even with no deliveries", func(t *testing.T) {
		out := prsView(100, 20, PRsOptions{}).View(prsState(false, nil))
		require.Contains(t, out, "safety off")
		require.Contains(t, out, emptyText[TabPRs])
	})

	t.Run("a long title is truncated, never wrapped", func(t *testing.T) {
		long := strings.Repeat("a very long bead title ", 10)
		lst := prsState(false, []events.Event{
			events.RunStarted{Meta: prsMeta(0), Info: prsRunInfo(false)},
			events.BeadCreated{Meta: prsMeta(1), Bead: domain.Bead{ID: "sc-hhh", Title: long, State: domain.BeadStateClosed, Priority: domain.PriorityDefault}},
			events.Merged{Meta: prsMeta(2), Bead: "sc-hhh", SHA: "abcdef1234567890", Branch: "main", Pushed: true},
		})
		out := prsView(100, 20, PRsOptions{}).View(lst)
		for _, l := range strings.Split(out, "\n") {
			require.LessOrEqual(t, lipgloss.Width(l), 100)
		}
		require.Contains(t, out, "…")
	})
}

// TestPRsSelected checks the row the cursor points at.
func TestPRsSelected(t *testing.T) {
	st := prsState(true, prsFixtureEvents())

	t.Run("none without deliveries", func(t *testing.T) {
		_, ok := prsView(100, 20, PRsOptions{}).Selected(prsState(true, nil))
		require.False(t, ok)
	})

	t.Run("follows the cursor", func(t *testing.T) {
		v := prsView(100, 20, PRsOptions{})
		v, _, handled := v.Update(prsKey("down"), st, DefaultKeyMap())
		require.True(t, handled)
		row, ok := v.Selected(st)
		require.True(t, ok)
		require.Equal(t, domain.BeadID("sc-3c4d"), row.Bead)
	})

	t.Run("clamped at both ends", func(t *testing.T) {
		v := prsView(100, 20, PRsOptions{})
		v, _, _ = v.Update(prsKey("up"), st, DefaultKeyMap())
		require.Equal(t, 0, v.Cursor())
		for range 20 {
			v, _, _ = v.Update(prsKey("down"), st, DefaultKeyMap())
		}
		require.Equal(t, len(st.Deliveries)-1, v.Cursor())
	})
}

// TestPRsOpenKey checks the `o` key: it opens the selected pull request
// through the injected opener and reports the result.
func TestPRsOpenKey(t *testing.T) {
	st := prsState(true, prsFixtureEvents())

	t.Run("opens the selected URL", func(t *testing.T) {
		var got string
		v := prsView(100, 20, PRsOptions{Open: func(_ context.Context, u string) error {
			got = u
			return nil
		}})
		v, cmd, handled := v.Update(prsKey("o"), st, DefaultKeyMap())
		require.True(t, handled)
		require.NotNil(t, cmd)
		require.Equal(t, 0, v.Cursor(), "opening must not move the cursor")
		msg, ok := cmd().(URLOpenedMsg)
		require.True(t, ok)
		require.NoError(t, msg.Err)
		require.Equal(t, "https://example.invalid/acme/app/pull/42", msg.URL)
		require.Equal(t, msg.URL, got)
	})

	t.Run("reports the opener's error", func(t *testing.T) {
		boom := errors.New("no browser")
		v := prsView(100, 20, PRsOptions{Open: func(context.Context, string) error { return boom }})
		cmd := v.OpenCmd(st)
		require.NotNil(t, cmd)
		msg, ok := cmd().(URLOpenedMsg)
		require.True(t, ok)
		require.ErrorIs(t, msg.Err, boom)
	})

	t.Run("does nothing without a URL", func(t *testing.T) {
		v := prsView(100, 20, PRsOptions{Open: func(context.Context, string) error {
			t.Error("the opener must not run for a delivery with no URL")
			return nil
		}})
		for range 3 { // sc-7g8h, merged straight to the default branch
			v, _, _ = v.Update(prsKey("down"), st, DefaultKeyMap())
		}
		row, ok := v.Selected(st)
		require.True(t, ok)
		require.Equal(t, domain.BeadID("sc-7g8h"), row.Bead)
		_, cmd, handled := v.Update(prsKey("o"), st, DefaultKeyMap())
		require.True(t, handled, "the key is still the view's, even with nothing to open")
		require.Nil(t, cmd)
	})

	t.Run("does nothing with no deliveries", func(t *testing.T) {
		require.Nil(t, prsView(100, 20, PRsOptions{}).OpenCmd(prsState(true, nil)))
	})

	t.Run("ignores other keys and other messages", func(t *testing.T) {
		v := prsView(100, 20, PRsOptions{})
		_, cmd, handled := v.Update(prsKey("x"), st, DefaultKeyMap())
		require.False(t, handled)
		require.Nil(t, cmd)
		_, cmd, handled = v.Update(tea.WindowSizeMsg{Width: 10, Height: 10}, st, DefaultKeyMap())
		require.False(t, handled)
		require.Nil(t, cmd)
	})

	t.Run("the binding is in the view's key map", func(t *testing.T) {
		keys := DefaultPRsKeyMap()
		require.Contains(t, keys.Open.Keys(), "o")
		require.Equal(t, []string{"o"}, prsKeysOf(keys.ShortHelp()))
		require.Equal(t, []string{"o"}, prsKeysOf(keys.FullHelp()[0]))
	})

	t.Run("a custom key map replaces the binding", func(t *testing.T) {
		custom := PRsKeyMap{Open: key.NewBinding(key.WithKeys("O"), key.WithHelp("O", "open PR"))}
		v := prsView(100, 20, PRsOptions{Keys: &custom, Open: func(context.Context, string) error { return nil }})
		require.Equal(t, custom, v.Keys())
		_, _, handled := v.Update(prsKey("o"), st, DefaultKeyMap())
		require.False(t, handled)
		_, cmd, handled := v.Update(prsKey("O"), st, DefaultKeyMap())
		require.True(t, handled)
		require.NotNil(t, cmd)
	})
}

// TestOpenURL exercises the real OS opener against a fake executable, so no
// test ever launches a browser.
func TestOpenURL(t *testing.T) {
	t.Run("runs the opener with the URL", func(t *testing.T) {
		shim := testutil.FakeBin(t, "sc-test-open", "exit 0")
		t.Setenv(OpenerEnv, "sc-test-open")
		require.NoError(t, OpenURL(t.Context(), "https://example.invalid/acme/app/pull/42"))
		require.Equal(t, [][]string{{"https://example.invalid/acme/app/pull/42"}}, shim.ArgLists(t))
	})

	t.Run("reports a non-zero exit", func(t *testing.T) {
		testutil.FakeBin(t, "sc-test-open", "exit 3")
		t.Setenv(OpenerEnv, "sc-test-open")
		err := OpenURL(t.Context(), "https://example.invalid/acme/app/pull/42")
		require.Error(t, err)
		require.Contains(t, err.Error(), "sc-test-open")
	})

	t.Run("reports a missing opener", func(t *testing.T) {
		t.Setenv(OpenerEnv, "sc-test-open-does-not-exist")
		require.Error(t, OpenURL(t.Context(), "https://example.invalid/acme/app/pull/42"))
	})

	t.Run("refuses anything but http and https", func(t *testing.T) {
		shim := testutil.FakeBin(t, "sc-test-open", "exit 0")
		t.Setenv(OpenerEnv, "sc-test-open")
		for _, bad := range []string{"file:///etc/passwd", "javascript:alert(1)", "", "not a url", "https://", "://x"} {
			require.Error(t, OpenURL(t.Context(), bad), "must refuse %q", bad)
		}
		require.Empty(t, shim.ArgLists(t), "a refused URL must never reach the opener")
	})

	t.Run("picks a platform opener when SC_OPENER is unset", func(t *testing.T) {
		t.Setenv(OpenerEnv, "")
		command, args := openerCommand("https://example.invalid/x")
		require.NotEmpty(t, command)
		require.Contains(t, args, "https://example.invalid/x")
	})
}

// prsModelOf asserts a [tea.Model] back to the concrete model the update
// loop returns.
func prsModelOf(t *testing.T, m tea.Model) Model {
	t.Helper()
	mm, ok := m.(Model)
	require.True(t, ok, "the update loop must return a tui.Model")
	return mm
}

// prsFooter renders the model's footer wide enough that nothing in it is
// clipped, which is what the assertions about it are interested in.
func prsFooter(t *testing.T, m tea.Model) string {
	t.Helper()
	return prsModelOf(t, m).footer(300)
}

// prsKeysOf lists the first key of each binding, for the help assertions.
func prsKeysOf(bs []key.Binding) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Keys()...)
	}
	return out
}

// prsKey builds a key press from its printable spelling.
func prsKey(s string) tea.KeyMsg {
	switch s {
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}
