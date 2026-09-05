package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
	"slop-cannon/internal/orchestrator"
)

// What `sc status` says about delivery (P8-34), about the pull requests the
// run did not open (P8-24), and about the beads somebody else moved
// (P8-19). All three read the same recorded run as status_test.go, so every
// helper here is prefixed "deliv" and none of them collide.

// delivReviewBead is a review bead of the recorded run: the shape D-11's
// watcher creates for a pull request the cannon did not open (label
// sc:external-pr, sc_kind external_pr, the pull request number in the
// metadata).
func delivReviewBead(id domain.BeadID, number int, status, closeReason string) beads.Issue {
	return beads.Issue{
		ID: id, Title: fmt.Sprintf("Review PR %d", number), Status: status,
		Type: string(domain.BeadTypeTask), Priority: domain.PriorityDefault,
		CloseReason: closeReason,
		Metadata: beads.Metadata{
			beads.MetaRun: statusRunID, beads.MetaKind: gitx.KindExternalPR,
			beads.MetaPRNumber: float64(number),
		},
		Labels: []string{beads.RunLabel(statusRunID), beads.LabelExternalPR},
	}
}

// delivReviewEvents are what a run reviewing two external pull requests
// publishes: one PROpened per adopted pull request, with External set, and
// the review beads' own transitions.
func delivReviewEvents() []events.Event {
	return []events.Event{
		events.PROpened{Meta: statusAt(64), Bead: "sc-8", URL: "https://example.test/pr/11",
			Number: 11, External: true},
		events.BeadStateChanged{Meta: statusAt(65), Bead: "sc-8",
			From: domain.BeadStateOpen, To: domain.BeadStateInReview, Round: 1},
		events.PROpened{Meta: statusAt(66), Bead: "sc-9", URL: "https://example.test/pr/12",
			Number: 12, External: true},
		events.BeadStateChanged{Meta: statusAt(67), Bead: "sc-9",
			From: domain.BeadStateOpen, To: domain.BeadStateInReview, Round: 1},
	}
}

// TestStatusReportsWhatTheRunDelivered is P8-34: the pull requests and
// merges were folded and persisted all along, and `sc status` printed
// neither, so a finished run read back later said nothing about what it had
// shipped.
func TestStatusReportsWhatTheRunDelivered(t *testing.T) {
	_, repo := statusEnv(t)
	statusRecord(t, repo, true)

	out, err := rootRun(t, "--repo", repo, "status", "--events", "0")
	require.NoError(t, err)
	require.Contains(t, out, "delivery   1 pull request, 1 merge")
	require.NotContains(t, out, "reviews    ", "this run reviewed nobody else's pull requests")

	// The same two numbers are in --json, where they always were: the
	// report carries the whole fold, so a machine consumer reads them off
	// the summary rather than off a second copy.
	stdout, stderr, code := runExec(t, "--json", "--repo", repo, "status")
	require.Equal(t, 0, code, stderr)
	var rep StatusReport
	require.NoError(t, json.Unmarshal([]byte(stdout), &rep), stdout)
	require.Equal(t, 1, rep.Summary.PRs)
	require.Equal(t, 1, rep.Summary.Merges)
	require.Nil(t, rep.Reviews)
	require.Contains(t, stdout, `"prs_opened": 1`)
	require.Contains(t, stdout, `"merged": 1`)
}

// TestStatusDeliveryRowIsPrintedWhenNothingShipped: a run that delivered
// nothing says so, exactly as the run's own final summary does. A missing
// row would read as a report that never asked.
func TestStatusDeliveryRowIsPrintedWhenNothingShipped(t *testing.T) {
	var b strings.Builder
	statusDeliverySection(&b, &StatusReport{Summary: &orchestrator.RunSummary{}})
	require.Equal(t, "\ndelivery   0 pull requests, 0 merges\n", b.String())

	// No run log, nothing to say.
	b.Reset()
	statusDeliverySection(&b, &StatusReport{})
	require.Empty(t, b.String())
}

// TestStatusReportsExternalReviewProgress is P8-24's Done-when: how many of
// somebody else's pull requests the run is reviewing, and how many reviews
// have gone out. The count of adopted pull requests is the run log's; what
// became of each is bd's, because nothing else records that a review was
// posted.
func TestStatusReportsExternalReviewProgress(t *testing.T) {
	_, repo := statusEnv(t)
	statusRecord(t, repo, true, delivReviewEvents()...)
	liveInstallBeads(t, repo, &liveFakeBeads{issues: append(liveIssues(),
		delivReviewBead("sc-8", 1, string(domain.BeadStateClosed),
			"reviewed pull request #11: REQUEST_CHANGES (slop-cannon never merges a pull request it did not open)"),
		delivReviewBead("sc-9", 2, string(domain.BeadStateInReview), ""),
	)})

	out, err := rootRun(t, "--repo", repo, "status", "--events", "0")
	require.NoError(t, err)
	require.Contains(t, out, "reviews    2 external pull requests, 1 review posted, 1 still in review")

	stdout, stderr, code := runExec(t, "--json", "--repo", repo, "status")
	require.Equal(t, 0, code, stderr)
	var rep StatusReport
	require.NoError(t, json.Unmarshal([]byte(stdout), &rep), stdout)
	require.NotNil(t, rep.Reviews)
	require.Equal(t, StatusReviews{PullRequests: 2, Beads: 2, Open: 1, Posted: 1}, *rep.Reviews)
	require.Equal(t, 2, rep.Summary.Reviewing)
}

// TestStatusReviewProgressWithoutBd: with no bd to ask, the run log still
// says how many external pull requests the run adopted — and says nothing
// it cannot know about what came of them.
func TestStatusReviewProgressWithoutBd(t *testing.T) {
	_, repo := statusEnv(t)
	statusRecord(t, repo, true, delivReviewEvents()...)

	out, err := rootRun(t, "--repo", repo, "status", "--events", "0", "--live=false")
	require.NoError(t, err)
	require.Contains(t, out, "reviews    2 external pull requests\n")
	require.NotContains(t, out, "review posted")
}

// TestStatusReviewProgressCountsEveryOutcome covers the counter itself over
// the four things that become of a review bead, and the two ways a closed
// one is told apart: a review that went out, and a bead settled because its
// pull request had gone (P8-11).
func TestStatusReviewProgressCountsEveryOutcome(t *testing.T) {
	issues := []beads.Issue{
		liveIssue("sc-1", "Add retry", string(domain.BeadStateInProgress)), // not a review bead
		delivReviewBead("sc-8", 1, string(domain.BeadStateClosed), statusReviewPosted+"11: APPROVE"),
		delivReviewBead("sc-9", 2, string(domain.BeadStateClosed), "pull request #12 is no longer open on GitHub"),
		delivReviewBead("sc-7", 3, string(domain.BeadStateInReview), ""),
		delivReviewBead("sc-6", 4, string(domain.BeadStateNeedsHuman), ""),
	}
	rv := statusReviewProgress(issues, nil)
	require.NotNil(t, rv)
	require.Equal(t, StatusReviews{Beads: 4, Open: 1, Posted: 1, Settled: 1, NeedsHuman: 1}, *rv)

	require.Nil(t, statusReviewProgress(issues[:1], nil), "a run with no review beads reports none")
}

// TestStatusReviewPostedMarkIsWhatTheReviewerWrites pins the one coupling
// the posted count rests on: the close reason orchestrator.PRReview writes
// when a consolidated review has gone out. No event carries "a review was
// posted", so bd's own record of it is all a later reader has; if that
// sentence is reworded, this fails here rather than leaving `sc status`
// quietly reporting "0 reviews posted" for ever.
func TestStatusReviewPostedMarkIsWhatTheReviewerWrites(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "orchestrator", "prreview.go"))
	require.NoError(t, err)
	require.Contains(t, string(src), `"`+statusReviewPosted+`%d: %s`,
		"orchestrator.PRReview no longer closes a reviewed bead with a reason starting %q, "+
			"so sc status cannot count the reviews it posted", statusReviewPosted)
}

// TestStatusLiveSaysWhatTheRunWillDoWithAMovedBead is P8-19: the two
// questions a reader has about a bead somebody else moved are answered by
// the D-07 machine itself (orchestrator.ResolveBeadStatus and
// orchestrator.TransitionStatus), not by a second reading of bd's
// vocabulary in this package.
func TestStatusLiveSaysWhatTheRunWillDoWithAMovedBead(t *testing.T) {
	_, repo := statusEnv(t)
	statusRecord(t, repo, true)
	issues := liveIssues()
	// A person reopened the bead the run closed…
	issues[1] = liveIssue("sc-1", "Add retry", string(domain.BeadStateOpen))
	// …and parked the one it was reviewing in a status bd owns and the
	// cannon never sets.
	issues[2] = liveIssue("sc-2", "Document retries", string(domain.BeadStateBlocked))
	liveInstallBeads(t, repo, &liveFakeBeads{issues: issues})

	out, err := rootRun(t, "--repo", repo, "status", "--events", "0")
	require.NoError(t, err)
	require.Contains(t, out, "sc-1  closed -> open (changed outside the run; the run can claim it again)")
	require.Contains(t, out, "sc-2  in_review -> blocked (changed outside the run; the run will pass over it)")

	stdout, stderr, code := runExec(t, "--json", "--repo", repo, "status")
	require.Equal(t, 0, code, stderr)
	var rep StatusReport
	require.NoError(t, json.Unmarshal([]byte(stdout), &rep), stdout)
	require.NotNil(t, rep.Live)
	byBead := map[domain.BeadID]StatusLiveChange{}
	for _, c := range rep.Live.Changed {
		byBead[c.Bead] = c
	}
	require.True(t, byBead["sc-1"].Claimable)
	require.False(t, byBead["sc-1"].OutOfReach)
	require.True(t, byBead["sc-2"].OutOfReach)
	require.False(t, byBead["sc-2"].Claimable)
}

// TestStatusResolveIsTheD07Guard: for every status a repository can hold a
// bead in, what `sc status` prints is what orchestrator.ResolveBeadStatus
// answers — the same guard the gate poller and the state machine apply — so
// the report cannot promise a bead the run would refuse to touch (P8-19,
// P6-12).
func TestStatusResolveIsTheD07Guard(t *testing.T) {
	statuses := beads.NewStatusMap(beads.StatusList{
		BuiltIn: []beads.StatusInfo{
			{Name: "open", Category: string(domain.CategoryActive)},
			{Name: "closed", Category: string(domain.CategoryDone)},
			{Name: "blocked", Category: string(domain.CategoryFrozen)},
		},
		Custom: []beads.StatusInfo{
			{Name: "in_review", Category: string(domain.CategoryWIP)},
			{Name: "in-qa", Category: string(domain.CategoryWIP)},
		},
	})
	for _, status := range []string{"open", "closed", "blocked", "in_review", "in-qa", "nonsense"} {
		t.Run(status, func(t *testing.T) {
			c := statusResolve(beads.Issue{ID: "sc-1", Status: status}, statuses)
			want, err := orchestrator.ResolveBeadStatus(status, statuses)
			require.Equal(t, want, c.To)
			require.Equal(t, err != nil, c.OutOfReach)
			_, claimErr := orchestrator.TransitionStatus(status, statuses, orchestrator.BeadEventClaimed)
			require.Equal(t, claimErr == nil, c.Claimable)
		})
	}
}

// TestStatusDeliveryAndReviewsGolden is the whole report of a run that both
// shipped its own work and reviewed somebody else's (D-11) — the two rows
// P8-34 and P8-24 add, in place.
func TestStatusDeliveryAndReviewsGolden(t *testing.T) {
	base, repo := statusEnv(t)
	statusRecord(t, repo, true, delivReviewEvents()...)
	liveInstallBeads(t, repo, &liveFakeBeads{issues: append(liveIssues(),
		delivReviewBead("sc-8", 1, string(domain.BeadStateClosed),
			"reviewed pull request #11: REQUEST_CHANGES (slop-cannon never merges a pull request it did not open)"),
		delivReviewBead("sc-9", 2, string(domain.BeadStateInReview), ""),
	)})

	out, err := rootRun(t, "--repo", repo, "status", "--events", "0")
	require.NoError(t, err)
	rootGolden(t, "status-reviews", statusNormalise(out, base, repo))
}
