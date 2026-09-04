package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
)

// The feedback loop (P6-04) is the D-08/D-09 review cycle, one thread per
// bead. It is a [WorkSource] the scheduler asks for work and a
// [FiringPlanner] the supervisor asks how to fire it, so the whole loop is
// driven by the ammo budget rather than by goroutines of its own:
//
//	round 1        an engineer implements the bead        (implement)
//	               the testers review the committed diff  (review)
//	round n > 1    the engineer answers the round's items (address_feedback)
//	               the testers judge any pushback         (evaluate_pushback)
//	               the product owner rules on held items  (arbitrate)
//	               the testers review the new diff        (re_review / review)
//
// The loop ends a bead's thread when every tester is satisfied and nothing
// is left unsettled (the bead moves to approved and the delivery handoff
// takes over, P6-06), when the product owner's escalation blocks it, or when
// the bead fails.
//
// The rules it enforces, all from the Decisions:
//
//   - D-08 review rounds are capped at limits.max_review_rounds; asking the
//     engineer for one more round past the cap escalates the bead to the
//     product owner instead, whose ruling (approve with notes, narrow the
//     scope, block) is binding.
//   - D-08 an item the engineer pushes back on goes to the tester that
//     raised it in evaluate-pushback mode, and an item the engineer says it
//     fixed goes back to that tester as a re-review of the diff. A round
//     that mixes the two is two firings, in that order: the evaluation
//     judges the argument alone, because its mode shows no diff and nothing
//     may be settled that nobody looked at. A tester that holds its ground
//     limits.max_pushback_rounds exchanges hands the item to the product
//     owner, whose ruling either overrules the item or sends it back to the
//     engineer, and that ruling is binding: an answer that argues with it
//     again is refused rather than applied, and an engineer that argues
//     [MaxRulingRefusals] times escalates the bead instead of spending
//     what is left of its review rounds one refused round at a time
//     (P6-38).
//   - D-08 each held item is its own ruling: the `arbitration` message
//     settles exactly one item, so every item the product owner owes a
//     ruling on is raised as its own demand with its own agent id (P6-25),
//     and two disputes on one bead need not queue behind one another.
//   - D-09 only testers that did not approve the previous round review
//     again, unless the engineer's round moved more than
//     [gitx.DefaultReReviewThreshold] of the change set, which sends every
//     lens back. A tester is credited with having reviewed the commit it
//     was actually shown, so a round the engineer spent arguing still
//     sends it back to the code the round left behind.
//   - D-06/D-25 every verdict, response, ruling and report is written to the
//     bead as a structured comment, so a resumed run can read the thread
//     back ([Feedback.Restore]).
//
// P6-21 lives here too: the items several testers raise about the same place
// in the same file are consolidated into one item for the engineer
// ([ConsolidateItems]), and the one answer is fanned back out to every
// tester that raised it, under that tester's own item id.
//
// Where the delivery handoff (P6-06) owns a round, the loop only supplies
// the agent and the bookkeeping: [Feedback.PlanFiring] builds the engineer
// firing of a conflict round from the delivery's own PendingConflict demand
// and reports back through [FeedbackOptions.ConflictResolved];
// [Feedback.CheckFailed] takes the qa items a failed check raised and opens
// the round that answers them; [Feedback.ReviewAgain] runs the full round a
// large conflict resolution owes (D-09); and [Feedback.Observe] keeps the
// loop's view of a bead honest when somebody else moved it. None of those
// writes a status — the delivery already did — bar the one escalation a
// check that failed at the D-08 round cap owes.
//
// The loop never leases a bead either: the scheduler claims it before the
// firing and renews it while the agent runs (P6-13), and [Supervisor.Fire]
// marks every firing NoLease, so nothing here has to.
//
// Nothing in here reads the wall clock or talks to git or bd directly: the
// clock is [FeedbackOptions.Now], the bead's context comes from
// [FeedbackContextFunc], the statuses go through the [StateMachine], the
// comments through a [FeedbackMessageStore] and the one field a ruling
// rewrites through a [BeadAcceptanceWriter] (D-08, P6-24), so the whole loop
// is deterministic under a fake clock and a mock cartridge.

// SourceFeedback is the [events.Warning] Source the feedback loop
// stamps on work it had to skip.
const SourceFeedback = "feedback"

// DefaultTitleSimilarity is how alike two feedback item titles about the
// same file must be before [ConsolidateItems] treats them as one issue: the
// share of words they have in common, from 0 (merge nothing but identical
// titles) to 1 (merge only identical word sets).
const DefaultTitleSimilarity = 0.6

// MaxRulingRefusals is how many times an engineer may argue with the same
// binding product owner ruling before the bead is escalated (D-08, P6-38).
//
// D-08 makes an arbitration binding, so an answer that argues with one is
// refused and the item stays owed ([Feedback.refuses]). A model that will
// not comply would otherwise be asked again every round until the review
// round cap escalated the bead, spending the whole budget on rounds that
// judge nothing; the second refusal of the same ruling escalates instead,
// so one round is spent rather than several. It is not configuration: the
// point is that the loop gives way after one more chance, whatever
// limits.max_review_rounds says.
const MaxRulingRefusals = 2

// Feedback loop errors, matched with errors.Is.
var (
	// ErrNoFeedbackThread means the bead is not one the loop is tracking.
	ErrNoFeedbackThread = errors.New("orchestrator: no feedback thread for this bead")
	// ErrUnknownFeedbackWork means a demand reached [Feedback.PlanFiring]
	// that the feedback loop did not raise.
	ErrUnknownFeedbackWork = errors.New("orchestrator: not a feedback demand")
	// ErrNoFeedbackContext means [FeedbackOptions.Context] could not
	// describe the bead, so no prompt can be built for it.
	ErrNoFeedbackContext = errors.New("orchestrator: no context for this bead")
)

// FeedbackPhase is what a bead's thread is waiting for.
type FeedbackPhase string

// The phases of one bead's thread.
const (
	// FeedbackImplementing waits for the first engineer to submit.
	FeedbackImplementing FeedbackPhase = "implementing"
	// FeedbackAddressing waits for the engineer to answer the round's items.
	FeedbackAddressing FeedbackPhase = "addressing"
	// FeedbackEvaluating waits for the testers to judge a pushback (D-08).
	FeedbackEvaluating FeedbackPhase = "evaluating"
	// FeedbackArbitrating waits for the product owner's ruling on a held
	// item (D-08).
	FeedbackArbitrating FeedbackPhase = "arbitrating"
	// FeedbackReviewing waits for this round's verdicts.
	FeedbackReviewing FeedbackPhase = "reviewing"
	// FeedbackEscalating waits for the product owner's ruling on a bead
	// that ran out of review rounds (D-08).
	FeedbackEscalating FeedbackPhase = "escalating"
	// FeedbackSettled means the loop is finished with the bead: it is
	// approved, blocked or failed and somebody else owns it now.
	FeedbackSettled FeedbackPhase = "settled"
)

// String returns the phase name.
func (p FeedbackPhase) String() string { return string(p) }

// FeedbackWork is the [Demand.Payload] the feedback loop attaches to every
// demand it raises. [Feedback.PlanFiring] reads it back; the scheduler never
// looks inside.
type FeedbackWork struct {
	// Bead and Round are the thread and the review round the work belongs to.
	Bead  domain.BeadID
	Round int
	// Phase is the thread phase that raised the demand.
	Phase FeedbackPhase
	// Engineer is the mode of an engineer firing; empty otherwise.
	Engineer agents.EngineerMode
	// Tester is the mode of a tester firing; empty otherwise.
	Tester agents.TesterMode
	// Lens is the tester's lens; empty outside a tester firing.
	Lens domain.Lens
	// Item is the disputed item an arbitration settles; empty otherwise.
	Item string
	// Escalate is true for the product owner firing that rules on a bead
	// which hit the review-round cap.
	Escalate bool
	// Delivery is true for a round the delivery handoff owns (a conflict
	// round, P6-06): the loop plans the firing and records the report, but
	// the delivery decides what happens to the bead afterwards.
	Delivery bool
}

// FeedbackBeadContext is everything outside the loop a prompt needs: the
// bead as beads sees it, the repository, the configured rules and vibes, and
// the diff under review. [FeedbackContextFunc] produces one per firing.
type FeedbackBeadContext struct {
	// Bead is the bead, its worktree (required: every firing runs in it)
	// and its branch.
	Bead prompt.BeadContext
	// Repo describes the repository (prompt.ScanRepo).
	Repo prompt.RepoContext
	// Brief is the human's -m text, empty in autopilot.
	Brief string
	// Rules and Vibes are the configured prompt documents (prompt.LoadDocs).
	Rules []prompt.Doc
	Vibes []prompt.Doc
	// Budget bounds the assembled prompt; the zero value means the defaults.
	Budget prompt.Budget
	// CheckCommand overrides the repository's check command for engineers.
	CheckCommand string
	// Diff is the change set under review. The loop keeps the diff of the
	// round it started, so every tester of a round reviews the same code;
	// this one is used when the loop has none yet.
	Diff *gitx.Diff
	// Model, MaxTurns and Timeout are passed to the cartridge; zero values
	// mean the adapter's defaults (and, for Timeout, the supervisor's).
	Model    string
	MaxTurns int
	Timeout  time.Duration
	// Env and InheritEnv are passed to the cartridge on top of the
	// read-only bd shim the supervisor installs (D-21, P6-17).
	Env        map[string]string
	InheritEnv []string
	// Notes are extra lines for the agent about this firing.
	Notes []string
}

// FeedbackContextFunc describes a bead for the prompt builders. It is called
// once per firing, on the goroutine of the agent being fired, so it may read
// git and bd; it must not block for long.
type FeedbackContextFunc func(ctx context.Context, bead domain.BeadID) (FeedbackBeadContext, error)

// FeedbackRatioFunc measures how far the engineer's round moved the change
// set, for the D-09 re-review rule ([gitx.Repo.ChangeRatio]). A nil func
// means the loop cannot measure movement and standing approvals stand.
type FeedbackRatioFunc func(ctx context.Context, bead domain.BeadID, previous, current *gitx.Diff) (gitx.ChangeRatio, error)

// FeedbackMessageStore records the loop's outcomes as bead comments
// (D-06, D-25). *beads.Client satisfies it.
type FeedbackMessageStore interface {
	PostMessage(ctx context.Context, id domain.BeadID, actor string, msg any) (beads.Message, error)
}

// *beads.Client is the production message store.
var _ FeedbackMessageStore = (*beads.Client)(nil)

// BeadAcceptanceWriter rewrites a bead's acceptance criteria in bd
// (`bd update --acceptance`), which is how a D-08 narrow_scope ruling
// becomes the criteria the bead is really held to rather than a note on the
// loop's own thread (P6-24). It is the narrowest slice of *beads.Client the
// loop needs, in the package convention host.go writes down; the planning
// loop asks for the same shape under its own name ([PlanPrioritiser]),
// because each component names the bd calls it makes.
type BeadAcceptanceWriter interface {
	Update(ctx context.Context, id domain.BeadID, patch beads.Patch) (beads.Issue, error)
}

// *beads.Client is the production acceptance writer.
var _ BeadAcceptanceWriter = (*beads.Client)(nil)

// FeedbackOptions configures [NewFeedback].
type FeedbackOptions struct {
	// Context describes a bead for the prompt builders. Required.
	Context FeedbackContextFunc
	// States moves the bead through the D-07 statuses. A nil machine keeps
	// the loop's own bookkeeping and writes nothing, which is what a dry
	// run wants.
	States *StateMachine
	// Messages records verdicts, responses and rulings on the bead. Nil
	// records nothing.
	Messages FeedbackMessageStore
	// Bus receives the loop's events. Nil publishes nothing.
	Bus Publisher
	// Run is the run id stamped on the events.
	Run domain.RunID
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Logger is where the loop logs; nil means slog.Default.
	Logger *slog.Logger
	// Limits are the D-08 caps; zero fields mean the config defaults.
	Limits config.Limits
	// ReReviewThreshold is the D-09 ratio; zero means
	// gitx.DefaultReReviewThreshold.
	ReReviewThreshold float64
	// Ratio measures the round's movement for D-09; nil keeps standing
	// approvals whatever the engineer did.
	Ratio FeedbackRatioFunc
	// Lenses are the tester lenses that review a bead; nil means
	// domain.Lenses(). The scheduler still decides how many of them run at
	// once (SlotCaps.TestersPerBead).
	Lenses []domain.Lens
	// TitleSimilarity is the P6-21 merge threshold; zero means
	// DefaultTitleSimilarity.
	TitleSimilarity float64
	// Acceptance writes a narrow_scope ruling's criteria back to the bead
	// (D-08, P6-24); pass the run's *beads.Client. Nil keeps them on the
	// thread only, which is what a dry run wants: the prompts still carry
	// the narrowed criteria, but bd does not, so a resumed run has only the
	// ruling comment to recover them from.
	Acceptance BeadAcceptanceWriter
	// ConflictResolved is called when the engineer of a delivery conflict
	// round has finished (P6-06 raises those demands; the loop plans the
	// firing because building an engineer request is the role layer's job).
	// The delivery handoff owns what happens next, so wire this to
	// Deliverer.Resolved and, when its outcome is DeliveryNeedsReview, to
	// [Feedback.ReviewAgain]. Nil leaves the bead where the delivery put it.
	ConflictResolved func(ctx context.Context, bead domain.BeadID) error
}

// ConsolidatedItem is one issue as the engineer sees it after the P6-21
// merge: the item to answer, and every tester that raised it under its own
// id and wording.
type ConsolidatedItem struct {
	// Item is what the engineer is asked about. Its ID is the id of the
	// first tester (in domain.Lenses order) that raised it, its Severity is
	// the most serious of the ones raised, and its Category is
	// domain.CategoryRules when any tester called it a rules violation.
	Item domain.FeedbackItem
	// Lenses are the testers that raised it, in domain.Lenses order.
	Lenses []domain.Lens
	// Raised is each tester's own version of the item, keyed by lens: the
	// id, severity and category that tester must see it carried back with
	// (agents.ValidateTesterVerdict refuses a rewritten one).
	Raised map[domain.Lens]domain.FeedbackItem
}

// ForLens returns the tester's own version of the item.
func (c ConsolidatedItem) ForLens(l domain.Lens) (domain.FeedbackItem, bool) {
	it, ok := c.Raised[l]
	return it, ok
}

// ResponseFor rewrites the engineer's answer so it names the tester's own
// item id, which is how one response settles the occurrence of every lens
// that raised the issue (P6-21).
func (c ConsolidatedItem) ResponseFor(l domain.Lens, r domain.Response) (domain.Response, bool) {
	it, ok := c.Raised[l]
	if !ok || r.Action == "" {
		return domain.Response{}, false
	}
	r.ItemID = it.ID
	return r, true
}

// ConsolidateItems merges the items of several testers' verdicts into one
// list for the engineer (P6-21). Two items are the same issue when they
// point at the same file and their titles share at least similarity of their
// words; items with no file are merged only on the title. Order is stable:
// issues come back in the order they were first raised, and the lenses of
// each are in domain.Lenses order.
//
// A similarity of zero means [DefaultTitleSimilarity]; one merges only
// identical word sets.
func ConsolidateItems(verdicts []domain.Verdict, similarity float64) []ConsolidatedItem {
	if similarity <= 0 {
		similarity = DefaultTitleSimilarity
	}
	var out []ConsolidatedItem
	for _, v := range fbSortVerdicts(verdicts) {
		for _, it := range v.Items {
			if strings.TrimSpace(it.ID) == "" {
				continue
			}
			if i := fbMatchItem(out, v.Lens, it, similarity); i >= 0 {
				out[i] = fbMergeItem(out[i], v.Lens, it)
				continue
			}
			out = append(out, ConsolidatedItem{
				Item:   it,
				Lenses: []domain.Lens{v.Lens},
				Raised: map[domain.Lens]domain.FeedbackItem{v.Lens: it},
			})
		}
	}
	return out
}

// fbSortVerdicts orders verdicts by lens so the canonical id of a merged
// item does not depend on the order the testers happened to finish in.
func fbSortVerdicts(vs []domain.Verdict) []domain.Verdict {
	out := append([]domain.Verdict(nil), vs...)
	sort.SliceStable(out, func(i, j int) bool { return lensRank(out[i].Lens) < lensRank(out[j].Lens) })
	return out
}

// fbMatchItem finds the consolidated issue an item belongs to, or -1. A
// lens never merges with itself: one tester raising two similar items has
// raised two items.
func fbMatchItem(items []ConsolidatedItem, lens domain.Lens, it domain.FeedbackItem, similarity float64) int {
	for i, c := range items {
		if _, dup := c.Raised[lens]; dup {
			continue
		}
		if fbSameIssue(c.Item, it, similarity) {
			return i
		}
	}
	return -1
}

// fbSameIssue reports whether two items are the same problem: the same file
// (or neither naming one) and titles that share enough words.
func fbSameIssue(a, b domain.FeedbackItem, similarity float64) bool {
	if fbNormalisePath(a.File) != fbNormalisePath(b.File) {
		return false
	}
	ta, tb := fbTitleWords(a.Title), fbTitleWords(b.Title)
	if len(ta) == 0 || len(tb) == 0 {
		return false
	}
	return fbOverlap(ta, tb) >= similarity
}

// fbMergeItem folds another tester's version of an issue into it.
func fbMergeItem(c ConsolidatedItem, lens domain.Lens, it domain.FeedbackItem) ConsolidatedItem {
	if c.Raised == nil {
		c.Raised = map[domain.Lens]domain.FeedbackItem{}
	}
	if _, seen := c.Raised[lens]; !seen {
		c.Lenses = append(c.Lenses, lens)
		sort.SliceStable(c.Lenses, func(i, j int) bool { return lensRank(c.Lenses[i]) < lensRank(c.Lenses[j]) })
	}
	c.Raised[lens] = it
	// The engineer answers the strictest reading of the issue.
	if it.Severity.Rank() >= 0 && (c.Item.Severity.Rank() < 0 || it.Severity.Rank() < c.Item.Severity.Rank()) {
		c.Item.Severity = it.Severity
	}
	if it.Category == domain.CategoryRules {
		c.Item.Category = domain.CategoryRules
		c.Item.Severity = domain.SeverityBlocker
	}
	if strings.TrimSpace(c.Item.Detail) == "" {
		c.Item.Detail = it.Detail
	} else if d := strings.TrimSpace(it.Detail); d != "" && !strings.Contains(c.Item.Detail, d) {
		c.Item.Detail += "\n" + d
	}
	if strings.TrimSpace(c.Item.Suggestion) == "" {
		c.Item.Suggestion = it.Suggestion
	}
	if c.Item.Line == 0 {
		c.Item.Line = it.Line
	}
	return c
}

// fbNormalisePath makes two spellings of one path compare equal.
func fbNormalisePath(p string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(p), "./"))
}

// fbTitleWords splits a title into comparable words.
func fbTitleWords(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		if len(w) > 2 && !fbStopWords[w] {
			out[w] = true
		}
	}
	return out
}

// fbStopWords are words too common to say two titles are the same issue.
var fbStopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "not": true,
	"has": true, "are": true, "this": true, "that": true, "was": true,
	"its": true, "into": true, "when": true, "from": true,
}

// fbOverlap is the share of words the two sets have in common (Jaccard).
func fbOverlap(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	common := 0
	for w := range a {
		if b[w] {
			common++
		}
	}
	union := len(a) + len(b) - common
	if union == 0 {
		return 0
	}
	return float64(common) / float64(union)
}

// fbItem is one consolidated issue as the loop tracks it across rounds.
type fbItem struct {
	con ConsolidatedItem
	// round is the review round the issue was first raised in.
	round int
	// per is each tester's latest status for its own occurrence.
	per map[domain.Lens]domain.ItemStatus
	// response is the engineer's latest answer to the consolidated item.
	response domain.Response
	// holds counts how many pushback exchanges a tester rejected (D-08).
	holds int
	// heldAt is the round the last hold was counted in, so two testers
	// holding one consolidated issue (P6-21) spend one exchange, not two.
	heldAt int
	// hold is the reason the tester gave the last time it held.
	hold string
	// ruling is the product owner's binding arbitration, once there is one.
	ruling *domain.Arbitration
	// refusals counts the answers refused because they argued with that
	// ruling instead of carrying it out (D-08, P6-38).
	refusals int
}

// status is the issue's status across every tester that raised it: open
// while anybody still wants it, held when somebody held it, and settled only
// when every occurrence is settled.
func (i *fbItem) status() domain.ItemStatus {
	if i.ruling != nil && i.ruling.Ruling == domain.RulingUpholdEngineer {
		return domain.ItemOverruled
	}
	var held, accepted, resolved bool
	for _, l := range i.con.Lenses {
		switch s := i.per[l]; s {
		case domain.ItemHeld:
			held = true
		case domain.ItemAccepted:
			accepted = true
		case domain.ItemResolved:
			resolved = true
		case domain.ItemOverruled:
			// Settled by a ruling; it neither blocks nor colours the rest.
		default:
			// Open, or a lens that has not answered yet.
			return domain.ItemOpen
		}
	}
	switch {
	case len(i.con.Lenses) == 0:
		return domain.ItemOpen
	case held:
		return domain.ItemHeld
	case resolved:
		return domain.ItemResolved
	case accepted:
		return domain.ItemAccepted
	default:
		return domain.ItemOverruled
	}
}

// settled reports whether nobody has to look at the issue again.
func (i *fbItem) settled() bool { return i.status().Settled() }

// awaitingEvaluation reports whether a tester still has to judge the
// engineer's pushback on the issue (D-08).
func (i *fbItem) awaitingEvaluation() bool {
	if i.settled() || i.status() == domain.ItemHeld {
		return false
	}
	switch i.response.Action {
	case domain.ActionPushback, domain.ActionWontfix:
		return true
	default:
		return false
	}
}

// evaluatedBy reports whether this lens still owes the issue a pushback
// evaluation: the engineer argued the point and the lens has neither
// settled it nor already held its ground on it (D-08). It is the one place
// the loop decides that, so the phase [Feedback.plan] chooses and the items
// [Feedback.planTester] sends cannot drift apart.
func (i *fbItem) evaluatedBy(l domain.Lens) bool {
	if !i.awaitingEvaluation() {
		return false
	}
	s := i.per[l]
	return s == "" || (!s.Settled() && s != domain.ItemHeld)
}

// awaitingArbitration reports whether the product owner owes a ruling: the
// tester held its ground the configured number of times (D-08).
func (i *fbItem) awaitingArbitration(cap int) bool {
	return i.ruling == nil && i.status() == domain.ItemHeld && i.holds >= cap
}

// defiant reports whether the engineer has argued with this item's binding
// ruling often enough that the bead should go to the product owner rather
// than buy another review round (D-08, P6-38).
func (i *fbItem) defiant() bool {
	return !i.settled() && i.ruling != nil && i.refusals >= MaxRulingRefusals
}

// awaitingEngineer reports whether the engineer owes the issue an answer.
func (i *fbItem) awaitingEngineer(cap int) bool {
	switch {
	case i.settled():
		return false
	case i.status() == domain.ItemHeld:
		return i.ruling != nil || i.holds < cap
	default:
		return i.response.Action == ""
	}
}

// dispute renders the issue as the ruling modes want it (P5-05).
func (i *fbItem) dispute() agents.ItemDispute {
	item := i.con.Item
	item.Status = i.status()
	d := agents.ItemDispute{
		Item:           item,
		Round:          i.round,
		Response:       i.response,
		HoldReason:     i.hold,
		PushbackRounds: i.holds,
	}
	if len(i.con.Lenses) > 0 {
		d.Lens = i.con.Lenses[0]
	}
	if i.ruling != nil {
		d.Rulings = []domain.Arbitration{*i.ruling}
	}
	return d
}

// fbThread is one bead's trip through the loop.
type fbThread struct {
	// id never changes, so it is the one field of a thread that is safe to
	// read without the loop's lock.
	id    domain.BeadID
	bead  domain.Bead
	state domain.BeadState
	phase FeedbackPhase
	round int
	kind  events.RoundKind

	items []*fbItem
	// verdicts are the current round's verdicts, by lens.
	verdicts map[domain.Lens]domain.Verdict
	// approved is each lens's latest outcome (D-09: an approval stands).
	approved map[domain.Lens]bool
	// looked is the round each lens last returned a verdict for, and
	// lookedAt the commit it saw then (D-09: a conflict round moves the
	// code without moving the round number).
	looked   map[domain.Lens]int
	lookedAt map[domain.Lens]string
	// reviewers are the lenses owed a verdict this round.
	reviewers []domain.Lens
	// evaluating are the lenses owed a pushback evaluation this round.
	evaluating []domain.Lens

	report     *domain.ImplReport
	diff       *gitx.Diff
	prevDiff   *gitx.Diff
	fullReview bool
	ratio      gitx.ChangeRatio

	conflict  agents.EngineerConflict
	history   []prompt.Round
	rulings   []domain.Arbitration
	responses []domain.Response
	// escalation is the product owner's ruling in the round now open. It is
	// filed into the history when the round closes and cleared with it, so a
	// narrowed bead's later rounds do not each repeat the ruling that
	// narrowed it.
	escalation *domain.Escalation
	// lastEscalation is the newest ruling whatever round it landed in, for
	// the thread snapshot.
	lastEscalation *domain.Escalation
	// acceptance is the acceptance criteria a narrow_scope ruling replaced
	// the bead's with (D-08). The thread's copy is what every prompt reads;
	// bd carries them too as soon as [FeedbackOptions.Acceptance] has
	// written them (P6-24), which is what makes them survive a resume.
	acceptance string
	// storedAcceptance is the narrowed criteria bd is known to hold, so the
	// write is made once and a replayed thread that finds them already
	// there repairs nothing (P6-24).
	storedAcceptance string
	notes            []string
	engineer         domain.AgentID
	// restoring means the thread is being rebuilt from the bead's comments
	// ([Feedback.Restore]): the replay publishes nothing, and the one
	// transition it concludes is written only when bd is not already there.
	restoring bool
}

// Feedback is the D-08/D-09 feedback loop engine (P6-04). It is safe for
// concurrent use: [Feedback.Demands] runs on the scheduler's loop goroutine
// while [Feedback.PlanFiring] and the result callbacks run on the
// goroutines of the agents being fired.
type Feedback struct {
	opts    FeedbackOptions
	lenses  []domain.Lens
	rounds  int
	pushbk  int
	thresh  float64
	similar float64
	now     func() time.Time
	log     *slog.Logger

	mu      sync.Mutex
	threads map[domain.BeadID]*fbThread
	order   []domain.BeadID
}

// NewFeedback builds the loop. It fails when there is no way to describe a
// bead ([FeedbackOptions.Context]), because no prompt could be built.
func NewFeedback(opts FeedbackOptions) (*Feedback, error) {
	if opts.Context == nil {
		return nil, fmt.Errorf("%w: the feedback loop needs a FeedbackContextFunc", ErrNoFeedbackContext)
	}
	f := &Feedback{
		opts:    opts,
		lenses:  append([]domain.Lens(nil), opts.Lenses...),
		rounds:  opts.Limits.MaxReviewRounds,
		pushbk:  opts.Limits.MaxPushbackRounds,
		thresh:  opts.ReReviewThreshold,
		similar: opts.TitleSimilarity,
		now:     opts.Now,
		log:     opts.Logger,
		threads: map[domain.BeadID]*fbThread{},
	}
	if len(f.lenses) == 0 {
		f.lenses = domain.Lenses()
	}
	if f.rounds <= 0 {
		f.rounds = config.DefaultMaxReviewRounds
	}
	if f.pushbk <= 0 {
		f.pushbk = config.DefaultMaxPushbackRounds
	}
	if f.thresh <= 0 {
		f.thresh = gitx.DefaultReReviewThreshold
	}
	if f.similar <= 0 {
		f.similar = DefaultTitleSimilarity
	}
	if f.now == nil {
		f.now = time.Now
	}
	if f.log == nil {
		f.log = slog.Default()
	}
	return f, nil
}

// FeedbackTrackedItem is one consolidated issue as a reader sees it.
type FeedbackTrackedItem struct {
	// Item is the issue the engineer answers.
	Item domain.FeedbackItem
	// Lenses are the testers that raised it.
	Lenses []domain.Lens
	// Round is the review round it was first raised in.
	Round int
	// Status is where it stands across every tester.
	Status domain.ItemStatus
	// Response is the engineer's latest answer, zero when there is none.
	Response domain.Response
	// Holds counts the times a tester rejected a pushback (D-08).
	Holds int
	// Ruling is the product owner's binding arbitration, nil until there is
	// one.
	Ruling *domain.Arbitration
	// Refusals counts the answers that argued with that ruling instead of
	// carrying it out; [MaxRulingRefusals] of them escalate the bead
	// (D-08, P6-38).
	Refusals int
}

// FeedbackThread is a read-only snapshot of one bead's loop, for `sc status`
// and the TUI.
type FeedbackThread struct {
	// Bead identifies the thread.
	Bead domain.BeadID
	// Title is the bead's title.
	Title string
	// State is the workflow state the loop believes the bead is in.
	State domain.BeadState
	// Phase is what the thread is waiting for.
	Phase FeedbackPhase
	// Round is the review round, 1-based; 0 before the first submission.
	Round int
	// Reviewers are the lenses owed a verdict right now.
	Reviewers []domain.Lens
	// Approved lists the lenses whose latest verdict was an approval.
	Approved []domain.Lens
	// Items are the consolidated issues, oldest first.
	Items []FeedbackTrackedItem
	// Escalation is the product owner's ruling on a bead that ran out of
	// rounds, nil until there is one.
	Escalation *domain.Escalation
	// FullReview is set when the D-09 ratio sent every lens back this round.
	FullReview bool
}

// Track adopts beads into the loop, or refreshes the ones it already holds.
// A bead in domain.BeadStateOpen is offered to an engineer; one already in
// review or later carries on from where its state says it is. Beads in a
// terminal state are dropped.
//
// Callers pass the beads this run owns and may implement — the ready set for
// fresh work (D-20: `bd ready` already excludes anything whose blockers are
// open) plus whatever the loop is already carrying.
func (f *Feedback) Track(bs ...domain.Bead) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range bs {
		if b.ID == "" {
			continue
		}
		t, ok := f.threads[b.ID]
		if !ok {
			if b.State.Terminal() {
				continue
			}
			t = f.newThread(b)
			f.threads[b.ID] = t
			f.order = append(f.order, b.ID)
			continue
		}
		t.bead = b
		if t.acceptance != "" {
			// A narrow_scope ruling replaced the criteria (D-08); the
			// refresh from bd must not put the old ones back — bd carries
			// the narrowed ones once the P6-24 write has landed, but a
			// listing that omits the field, or a write bd refused, would
			// otherwise lose the ruling.
			t.bead.Acceptance = t.acceptance
		}
		if b.State.Terminal() && t.state != b.State {
			t.state, t.phase = b.State, FeedbackSettled
		}
	}
}

// newThread starts a thread for a bead in whatever state bd holds it.
func (f *Feedback) newThread(b domain.Bead) *fbThread {
	t := &fbThread{
		id:       b.ID,
		bead:     b,
		state:    b.State,
		round:    b.Round,
		kind:     events.RoundReview,
		verdicts: map[domain.Lens]domain.Verdict{},
		approved: map[domain.Lens]bool{},
		looked:   map[domain.Lens]int{},
		lookedAt: map[domain.Lens]string{},
	}
	if t.state == "" {
		t.state = domain.BeadStateOpen
	}
	switch t.state {
	case domain.BeadStateOpen, domain.BeadStateInProgress:
		t.phase = FeedbackImplementing
	case domain.BeadStateChangesRequested:
		t.phase = FeedbackAddressing
	case domain.BeadStateInReview:
		t.phase = FeedbackReviewing
	case domain.BeadStateEscalated:
		t.phase = FeedbackEscalating
	default:
		t.phase = FeedbackSettled
	}
	if t.round < 1 && t.phase != FeedbackImplementing {
		t.round = 1
	}
	return t
}

// Thread returns a snapshot of one bead's loop.
func (f *Feedback) Thread(bead domain.BeadID) (FeedbackThread, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.threads[bead]
	if !ok {
		return FeedbackThread{}, false
	}
	return f.snapshot(t), true
}

// Threads returns a snapshot of every tracked bead, in the order the loop
// adopted them.
func (f *Feedback) Threads() []FeedbackThread {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FeedbackThread, 0, len(f.order))
	for _, id := range f.order {
		if t, ok := f.threads[id]; ok {
			out = append(out, f.snapshot(t))
		}
	}
	return out
}

// Outstanding counts the beads the loop is still working on. A run in brief
// mode is finished when it reaches zero (P6-05).
func (f *Feedback) Outstanding() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, t := range f.threads {
		if t.phase != FeedbackSettled {
			n++
		}
	}
	return n
}

// snapshot renders a thread; the caller holds the lock.
func (f *Feedback) snapshot(t *fbThread) FeedbackThread {
	out := FeedbackThread{
		Bead:       t.id,
		Title:      t.bead.Title,
		State:      t.state,
		Phase:      t.phase,
		Round:      t.round,
		Reviewers:  append([]domain.Lens(nil), t.reviewers...),
		Escalation: t.lastEscalation,
		FullReview: t.fullReview,
	}
	for _, l := range f.lenses {
		if t.approved[l] {
			out.Approved = append(out.Approved, l)
		}
	}
	for _, i := range t.items {
		out.Items = append(out.Items, FeedbackTrackedItem{
			Item:     i.con.Item,
			Lenses:   append([]domain.Lens(nil), i.con.Lenses...),
			Round:    i.round,
			Status:   i.status(),
			Response: i.response,
			Holds:    i.holds,
			Ruling:   i.ruling,
			Refusals: i.refusals,
		})
	}
	return out
}

// Demands implements [WorkSource]: it reports the firing every tracked bead
// wants right now, in the D-05 kinds. It is stateless in the sense the
// scheduler needs — the same demand comes back on every tick until the work
// behind it lands — so a firing that crashed is simply offered again.
func (f *Feedback) Demands(_ context.Context) ([]Demand, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Demand
	for i, id := range f.order {
		t, ok := f.threads[id]
		if !ok {
			continue
		}
		out = append(out, f.demandsFor(t, i)...)
	}
	return out, nil
}

// demandsFor is the per-thread half of Demands; the caller holds the lock.
func (f *Feedback) demandsFor(t *fbThread, order int) []Demand {
	base := Demand{Bead: t.id, BeadTitle: t.bead.Title, Round: t.round, Order: order, From: t.state}
	switch t.phase {
	case FeedbackImplementing:
		d := base
		d.Kind, d.Task, d.Claim = DemandImplement, events.TaskImplement, ClaimNew
		d.Round = max(t.round, 1)
		d.HeldBy = domain.AgentID(t.bead.Assignee)
		d.Payload = FeedbackWork{Bead: t.id, Round: d.Round, Phase: t.phase, Engineer: agents.EngineerImplement}
		return []Demand{d}

	case FeedbackAddressing:
		d := base
		d.Kind, d.Claim = DemandImplement, ClaimRework
		d.HeldBy = domain.AgentID(t.bead.Assignee)
		mode := agents.EngineerAddressFeedback
		d.Task = events.TaskAddressFeedback
		if !f.hasEngineerWork(t) {
			// A bead adopted in changes_requested whose feedback the loop
			// never saw (a resume with no comments left to read): there is
			// nothing to answer, so the engineer implements instead.
			mode, d.Task = agents.EngineerImplement, events.TaskImplement
		}
		d.Payload = FeedbackWork{Bead: t.id, Round: t.round, Phase: t.phase, Engineer: mode}
		return []Demand{d}

	case FeedbackEvaluating:
		var out []Demand
		for _, l := range t.evaluating {
			d := base
			d.Kind, d.Lens, d.Task = DemandReview, l, events.TaskEvaluatePushback
			d.Payload = FeedbackWork{
				Bead: t.id, Round: t.round, Phase: t.phase,
				Tester: agents.TesterModeEvaluatePushback, Lens: l,
			}
			out = append(out, d)
		}
		return out

	case FeedbackArbitrating:
		var out []Demand
		for _, item := range f.disputes(t) {
			d := base
			d.Kind, d.Task = DemandArbitrate, events.TaskArbitrate
			d.Agent = domain.AgentID(fmt.Sprintf("po-%s-r%d-%s", t.id, t.round, item.con.Item.ID))
			d.Payload = FeedbackWork{Bead: t.id, Round: t.round, Phase: t.phase, Item: item.con.Item.ID}
			out = append(out, d)
		}
		return out

	case FeedbackReviewing:
		if len(t.reviewers) == 0 {
			// A bead adopted straight into review (a resume) has not been
			// through plan yet, so work out who owes a verdict now.
			t.reviewers = f.reviewersFor(t)
		}
		var out []Demand
		for _, l := range t.reviewers {
			d := base
			d.Kind, d.Lens, d.Task = DemandReview, l, events.TaskReview
			mode := f.testerMode(t, l)
			d.Payload = FeedbackWork{
				Bead: t.id, Round: t.round, Phase: t.phase, Tester: mode, Lens: l,
			}
			out = append(out, d)
		}
		return out

	case FeedbackEscalating:
		d := base
		d.Kind, d.Task = DemandArbitrate, events.TaskEscalate
		d.Agent = domain.AgentID(fmt.Sprintf("po-%s-r%d-escalate", t.id, t.round))
		d.Payload = FeedbackWork{Bead: t.id, Round: t.round, Phase: t.phase, Escalate: true}
		return []Demand{d}
	}
	return nil
}

// testerMode is the mode a lens reviews in: a re-review when it carries
// items forward, a fresh review otherwise (a lens sent back by the D-09
// ratio has nothing of its own to carry).
func (f *Feedback) testerMode(t *fbThread, l domain.Lens) agents.TesterMode {
	if len(f.itemsFor(t, l)) > 0 {
		return agents.TesterModeReReview
	}
	return agents.TesterModeReview
}

// hasEngineerWork reports whether any issue is waiting on the engineer.
func (f *Feedback) hasEngineerWork(t *fbThread) bool {
	for _, i := range t.items {
		if i.awaitingEngineer(f.pushbk) {
			return true
		}
	}
	return false
}

// disputes are the items the product owner owes a ruling on, in the order
// they were raised (P6-25).
//
// One `arbitration` message settles exactly one item
// ([agents.ArbitrationRequest]), so two held items on one bead are two
// firings. Each is offered as its own demand, with the item in the payload
// and in the agent id, so they are independent jobs rather than a queue:
// nothing in the loop makes the second wait for the first, and a ruling
// that lands while another is still being made leaves the thread in
// [FeedbackArbitrating] until every dispute has one (see [Feedback.plan]).
//
// Note for whoever wires the scheduler: [Demand.Key] is
// kind|bead|lens|round, which two arbitrations of one round share, so the
// scheduler still runs them one after another until the item joins that
// key. Everything else — the agent ids, the payloads, the bookkeeping —
// is already per item, and no order or count changes when it does.
func (f *Feedback) disputes(t *fbThread) []*fbItem {
	var out []*fbItem
	for _, i := range t.items {
		if i.awaitingArbitration(f.pushbk) {
			out = append(out, i)
		}
	}
	return out
}

// defiant is the item whose binding ruling the engineer has argued with
// once too often, or nil (D-08, P6-38); the caller holds the lock.
func (f *Feedback) defiant(t *fbThread) *fbItem {
	for _, i := range t.items {
		if i.defiant() {
			return i
		}
	}
	return nil
}

// itemsFor returns the unsettled items a lens carries, in its own wording.
func (f *Feedback) itemsFor(t *fbThread, l domain.Lens) []domain.FeedbackItem {
	var out []domain.FeedbackItem
	for _, i := range t.items {
		if i.settled() {
			continue
		}
		if _, ok := i.con.ForLens(l); !ok {
			continue
		}
		if s := i.per[l]; s != "" && s.Settled() {
			continue
		}
		out = append(out, fbOwnItem(i, l))
	}
	return out
}

// disputedFor returns the items of one lens the engineer pushed back on and
// the answers it gave them, and nothing else: it is what an
// evaluate-pushback firing judges (D-08). An item the engineer fixed is not
// in dispute, so it is left for the re-review that reads the diff.
func (f *Feedback) disputedFor(t *fbThread, l domain.Lens) ([]domain.FeedbackItem, []domain.Response) {
	var (
		items []domain.FeedbackItem
		out   []domain.Response
	)
	for _, i := range t.items {
		if !i.evaluatedBy(l) {
			continue
		}
		if _, ok := i.con.ForLens(l); !ok {
			continue
		}
		r, ok := i.con.ResponseFor(l, i.response)
		if !ok {
			continue
		}
		items = append(items, fbOwnItem(i, l))
		out = append(out, r)
	}
	return items, out
}

// fbOwnItem is one lens's own wording of a consolidated issue, with the
// status that lens must see it carried back with (P6-21,
// agents.ValidateTesterVerdict refuses a rewritten one).
func fbOwnItem(i *fbItem, l domain.Lens) domain.FeedbackItem {
	own := i.con.Raised[l]
	own.Status = i.per[l]
	if own.Status == "" || own.Status == domain.ItemOverruled {
		own.Status = domain.ItemOpen
	}
	return own
}

// responsesFor rewrites the engineer's answers for one lens (P6-21).
func (f *Feedback) responsesFor(t *fbThread, l domain.Lens) []domain.Response {
	var out []domain.Response
	for _, i := range t.items {
		if i.settled() {
			continue
		}
		if r, ok := i.con.ResponseFor(l, i.response); ok {
			out = append(out, r)
		}
	}
	return out
}

// Handles reports whether the demand is one the feedback loop raised, so a
// run that composes several planners can route to the right one.
func (f *Feedback) Handles(d Demand) bool {
	if _, ok := d.Payload.(PendingConflict); ok {
		// The delivery handoff raises the conflict round; the loop is what
		// knows how to build an engineer firing for it (P6-06).
		return true
	}
	w, ok := d.Payload.(FeedbackWork)
	if !ok {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, tracked := f.threads[w.Bead]
	return tracked
}

// PlanFiring implements [FiringPlanner]: it turns one of the loop's demands
// into the agent session that serves it, with the result callback that folds
// the reply back into the thread.
func (f *Feedback) PlanFiring(ctx context.Context, fi Firing) (AgentFiring, error) {
	if p, ok := fi.Demand.Payload.(PendingConflict); ok {
		return f.planConflict(ctx, p, fi)
	}
	w, ok := fi.Demand.Payload.(FeedbackWork)
	if !ok {
		return AgentFiring{}, fmt.Errorf("%w: %s wants %s", ErrUnknownFeedbackWork, fi.Agent.ID, fi.Demand.Kind)
	}
	f.mu.Lock()
	t, tracked := f.threads[w.Bead]
	f.mu.Unlock()
	if !tracked {
		return AgentFiring{}, fmt.Errorf("%w: %s", ErrNoFeedbackThread, w.Bead)
	}

	bc, err := f.opts.Context(ctx, w.Bead)
	if err != nil {
		return AgentFiring{}, fmt.Errorf("orchestrator: feedback context for %s: %w", w.Bead, err)
	}
	if strings.TrimSpace(bc.Bead.Worktree) == "" {
		return AgentFiring{}, fmt.Errorf("%w: bead %s has no worktree", ErrNoFeedbackContext, w.Bead)
	}
	bc.Bead.Bead = f.mergeBead(t, bc.Bead.Bead)
	bc.Bead.Round = w.Round

	switch {
	case w.Escalate:
		return f.planEscalation(ctx, t, w, fi, bc)
	case w.Item != "":
		return f.planArbitration(ctx, t, w, fi, bc)
	case w.Tester != "":
		return f.planTester(ctx, t, w, fi, bc)
	case w.Engineer != "":
		return f.planEngineer(ctx, t, w, fi, bc)
	}
	return AgentFiring{}, fmt.Errorf("%w: %s carries no feedback work", ErrUnknownFeedbackWork, fi.Agent.ID)
}

// mergeBead fills the context's bead from the thread when the caller left it
// blank, so a prompt always names the bead it is about.
func (f *Feedback) mergeBead(t *fbThread, b domain.Bead) domain.Bead {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b.ID == "" {
		b = t.bead
	}
	if b.Title == "" {
		b.Title = t.bead.Title
	}
	if t.acceptance != "" {
		// D-08: the product owner narrowed the criteria this bead is judged
		// against. The context func reads them from bd, which still holds
		// the originals (P6-24), so the ruling wins here.
		b.Acceptance = t.acceptance
	}
	b.State = t.state
	b.Round = t.round
	return b
}

// planConflict builds the engineer firing of a delivery conflict round
// (D-10). The demand comes from the delivery handoff, which owns the round
// and its outcome (P6-06); the loop owns the firing, because assembling an
// [agents.EngineerRequest] is the role layer's job, and it records the
// report on the bead like any other round.
func (f *Feedback) planConflict(ctx context.Context, p PendingConflict, fi Firing) (AgentFiring, error) {
	f.Track(p.Bead)
	f.mu.Lock()
	t, ok := f.threads[p.Bead.ID]
	if ok {
		t.conflict = p.Conflict
		t.kind = events.RoundConflict
		t.state = domain.BeadStateInProgress
		if p.Round > 0 {
			t.round = p.Round
		}
	}
	f.mu.Unlock()
	if !ok {
		return AgentFiring{}, fmt.Errorf("%w: %s", ErrNoFeedbackThread, p.Bead.ID)
	}
	bc, err := f.opts.Context(ctx, p.Bead.ID)
	if err != nil {
		return AgentFiring{}, fmt.Errorf("orchestrator: feedback context for %s: %w", p.Bead.ID, err)
	}
	if strings.TrimSpace(bc.Bead.Worktree) == "" {
		return AgentFiring{}, fmt.Errorf("%w: bead %s has no worktree", ErrNoFeedbackContext, p.Bead.ID)
	}
	bc.Bead.Bead = f.mergeBead(t, bc.Bead.Bead)
	bc.Bead.Round = p.Round
	w := FeedbackWork{
		Bead: p.Bead.ID, Round: p.Round, Phase: FeedbackAddressing,
		Engineer: agents.EngineerResolveConflict, Delivery: true,
	}
	return f.planEngineer(ctx, t, w, fi, bc)
}

// planEngineer builds an implement, address-feedback or resolve-conflict
// firing.
func (f *Feedback) planEngineer(ctx context.Context, t *fbThread, w FeedbackWork, fi Firing, bc FeedbackBeadContext) (AgentFiring, error) {
	f.mu.Lock()
	req := agents.EngineerRequest{
		Mode:         w.Engineer,
		RunID:        f.opts.Run,
		AgentID:      fi.Agent.ID,
		Bead:         bc.Bead,
		Repo:         bc.Repo,
		Brief:        bc.Brief,
		Rules:        bc.Rules,
		Vibes:        bc.Vibes,
		History:      append([]prompt.Round(nil), t.history...),
		CheckCommand: bc.CheckCommand,
		Model:        bc.Model,
		MaxTurns:     bc.MaxTurns,
		Timeout:      bc.Timeout,
		InheritEnv:   bc.InheritEnv,
		Budget:       bc.Budget,
	}
	if w.Engineer == agents.EngineerResolveConflict {
		req.Conflict = t.conflict
	}
	if w.Engineer == agents.EngineerAddressFeedback {
		for _, i := range t.items {
			if !i.awaitingEngineer(f.pushbk) {
				continue
			}
			item := i.con.Item
			item.Status = i.status()
			req.Items = append(req.Items, item)
			if i.ruling != nil {
				req.Arbitrations = append(req.Arbitrations, *i.ruling)
			}
		}
	}
	t.engineer = fi.Agent.ID
	if t.state == domain.BeadStateOpen || t.state == domain.BeadStateChangesRequested {
		// The scheduler claimed the bead before firing (P6-13), so bd
		// already holds it in progress.
		t.state = domain.BeadStateInProgress
	}
	f.mu.Unlock()

	if len(req.Items) == 0 && w.Engineer == agents.EngineerAddressFeedback {
		return AgentFiring{}, fmt.Errorf("orchestrator: bead %s has nothing for the engineer to address", w.Bead)
	}
	af := AgentFiring{
		Agent: domain.Agent{
			ID: fi.Agent.ID, Role: domain.RoleEngineer, Bead: w.Bead,
			Model: bc.Model, State: domain.AgentStatePending,
		},
		Task:      fi.Demand.FiringTask(),
		Round:     w.Round,
		BeadTitle: bc.Bead.Bead.Title,
		Worktree:  bc.Bead.Worktree,
		Build: func(_ context.Context, fc FiringContext) (agents.AgentRun, error) {
			r := req
			r.Env = mergeEnv(bc.Env, fc.Env)
			r.Timeout = fc.Timeout
			firing, err := r.Firing()
			if err != nil && !errors.Is(err, agents.ErrPromptOverBudget) {
				return agents.AgentRun{}, err
			}
			if err != nil {
				f.warn(w.Bead, fmt.Sprintf("the %s prompt is over its token budget", r.Mode), err)
			}
			return agents.AgentRun{
				Spec: firing.Spec, Message: firing.Message,
				Check: agents.EngineerResultCheck(r), Now: f.now,
			}, nil
		},
		Result: func(payload any, _ agents.Transcript) error {
			return f.engineerReplied(context.WithoutCancel(ctx), w, fi.Agent.ID, payload)
		},
	}
	return af, nil
}

// planTester builds a review, re-review or evaluate-pushback firing.
func (f *Feedback) planTester(ctx context.Context, t *fbThread, w FeedbackWork, fi Firing, bc FeedbackBeadContext) (AgentFiring, error) {
	f.mu.Lock()
	diff := t.diff
	if diff == nil {
		diff = bc.Diff
	}
	items, responses := f.itemsFor(t, w.Lens), f.responsesFor(t, w.Lens)
	if w.Tester == agents.TesterModeEvaluatePushback {
		// D-08 fires two different things at a tester: an item the engineer
		// argued about goes to it in evaluate-pushback mode, and an item the
		// engineer says it fixed goes back for a re-review of the diff. This
		// firing is the first of those, so it carries the disputed items
		// alone: sweeping a fixed item in would let the tester settle it
		// without anyone having looked at the code (the mode renders no
		// diff). The fixed ones come back as the re-review this round plans
		// next.
		items, responses = f.disputedFor(t, w.Lens)
	}
	req := agents.TesterRequest{
		Mode:       w.Tester,
		Lens:       w.Lens,
		RunID:      f.opts.Run,
		AgentID:    fi.Agent.ID,
		Workdir:    bc.Bead.Worktree,
		Round:      w.Round,
		Bead:       bc.Bead,
		Repo:       bc.Repo,
		Brief:      bc.Brief,
		Rules:      bc.Rules,
		Vibes:      bc.Vibes,
		History:    append([]prompt.Round(nil), t.history...),
		Report:     t.report,
		Diff:       diff,
		Items:      items,
		Responses:  responses,
		Notes:      append(append([]string(nil), t.notes...), bc.Notes...),
		Budget:     bc.Budget,
		Model:      bc.Model,
		MaxTurns:   bc.MaxTurns,
		Timeout:    bc.Timeout,
		InheritEnv: bc.InheritEnv,
	}
	f.mu.Unlock()

	if req.Mode == agents.TesterModeEvaluatePushback {
		req.Diff = nil
	}
	if err := req.Validate(); err != nil {
		return AgentFiring{}, err
	}
	af := AgentFiring{
		Agent:     req.Agent(),
		Task:      fi.Demand.FiringTask(),
		Round:     w.Round,
		BeadTitle: bc.Bead.Bead.Title,
		Worktree:  bc.Bead.Worktree,
		Build: func(_ context.Context, fc FiringContext) (agents.AgentRun, error) {
			r := req
			r.Env = mergeEnv(bc.Env, fc.Env)
			r.Timeout = fc.Timeout
			spec, _, err := agents.TesterRunSpec(r)
			if err != nil && !errors.Is(err, agents.ErrPromptOverBudget) {
				return agents.AgentRun{}, err
			}
			if err != nil {
				f.warn(w.Bead, fmt.Sprintf("the %s %s prompt is over its token budget", r.Lens, r.Mode), err)
			}
			return agents.AgentRun{
				Spec: spec, Message: domain.MessageVerdict,
				Check: agents.TesterVerdictCheck(r), Now: f.now,
			}, nil
		},
		Result: func(payload any, _ agents.Transcript) error {
			return f.testerReplied(context.WithoutCancel(ctx), w, fi.Agent.ID, payload)
		},
	}
	return af, nil
}

// planArbitration builds the product owner's ruling on one held item (D-08).
func (f *Feedback) planArbitration(ctx context.Context, t *fbThread, w FeedbackWork, fi Firing, bc FeedbackBeadContext) (AgentFiring, error) {
	f.mu.Lock()
	item := fbFind(t, w.Item)
	var req agents.ArbitrationRequest
	if item != nil {
		req = agents.ArbitrationRequest{
			RulingContext: agents.RulingContext{
				RunID: f.opts.Run, AgentID: fi.Agent.ID, Brief: bc.Brief,
				Rules: bc.Rules, Vibes: bc.Vibes, Repo: bc.Repo, Bead: &bc.Bead,
				History: append([]prompt.Round(nil), t.history...), Budget: bc.Budget,
			},
			Dispute:           item.dispute(),
			MaxPushbackRounds: f.pushbk,
		}
	}
	f.mu.Unlock()
	if item == nil {
		return AgentFiring{}, fmt.Errorf("%w: item %q is not disputed on %s", ErrUnknownFeedbackWork, w.Item, w.Bead)
	}
	return AgentFiring{
		Agent: domain.Agent{
			ID: fi.Agent.ID, Role: domain.RolePO, Bead: w.Bead,
			Model: bc.Model, State: domain.AgentStatePending,
		},
		Task:      events.TaskArbitrate,
		Round:     w.Round,
		BeadTitle: bc.Bead.Bead.Title,
		Worktree:  bc.Bead.Worktree,
		Build: func(_ context.Context, fc FiringContext) (agents.AgentRun, error) {
			spec, err := fbPORunSpec(req.PromptRequest, events.TaskArbitrate, bc, fc)
			if err != nil {
				return agents.AgentRun{}, err
			}
			return agents.AgentRun{
				Spec: spec, Message: domain.MessageArbitration,
				Check: agents.ArbitrationCheck(req), Now: f.now,
			}, nil
		},
		Result: func(payload any, _ agents.Transcript) error {
			return f.poRuled(context.WithoutCancel(ctx), w, fi.Agent.ID, payload)
		},
	}, nil
}

// planEscalation builds the product owner's ruling on a bead that ran out of
// review rounds (D-08).
func (f *Feedback) planEscalation(ctx context.Context, t *fbThread, w FeedbackWork, fi Firing, bc FeedbackBeadContext) (AgentFiring, error) {
	f.mu.Lock()
	req := agents.EscalationRequest{
		RulingContext: agents.RulingContext{
			RunID: f.opts.Run, AgentID: fi.Agent.ID, Brief: bc.Brief,
			Rules: bc.Rules, Vibes: bc.Vibes, Repo: bc.Repo, Bead: &bc.Bead,
			History: append([]prompt.Round(nil), t.history...), Budget: bc.Budget,
		},
		Round:     t.round,
		MaxRounds: f.rounds,
		Notes:     append([]string(nil), t.notes...),
	}
	for _, i := range t.items {
		if !i.settled() {
			req.Unresolved = append(req.Unresolved, i.dispute())
		}
	}
	f.mu.Unlock()

	return AgentFiring{
		Agent: domain.Agent{
			ID: fi.Agent.ID, Role: domain.RolePO, Bead: w.Bead,
			Model: bc.Model, State: domain.AgentStatePending,
		},
		Task:      events.TaskEscalate,
		Round:     w.Round,
		BeadTitle: bc.Bead.Bead.Title,
		Worktree:  bc.Bead.Worktree,
		Build: func(_ context.Context, fc FiringContext) (agents.AgentRun, error) {
			spec, err := fbPORunSpec(req.PromptRequest, events.TaskEscalate, bc, fc)
			if err != nil {
				return agents.AgentRun{}, err
			}
			return agents.AgentRun{
				Spec: spec, Message: domain.MessageEscalation,
				Check: agents.EscalationCheck(req), Now: f.now,
			}, nil
		},
		Result: func(payload any, _ agents.Transcript) error {
			return f.poRuled(context.WithoutCancel(ctx), w, fi.Agent.ID, payload)
		},
	}, nil
}

// fbPORunSpec assembles the run spec of a product owner ruling firing: the
// prompt from the request, the tool policy from the task (P5-06), and the
// cartridge limits from the bead context.
func fbPORunSpec(build func() (prompt.Request, error), task events.Task, bc FeedbackBeadContext, fc FiringContext) (cartridge.RunSpec, error) {
	req, err := build()
	if err != nil {
		return cartridge.RunSpec{}, err
	}
	p, err := prompt.Build(req)
	if err != nil {
		return cartridge.RunSpec{}, err
	}
	spec := cartridge.RunSpec{
		Prompt:     p.Text,
		Workdir:    fc.Workdir,
		Model:      bc.Model,
		MaxTurns:   bc.MaxTurns,
		Timeout:    fc.Timeout,
		Env:        mergeEnv(bc.Env, fc.Env),
		InheritEnv: bc.InheritEnv,
	}
	if err := agents.ApplyPolicy(&spec, domain.RolePO, task); err != nil {
		return cartridge.RunSpec{}, err
	}
	return spec, nil
}

// fbFind returns the tracked issue with that consolidated id.
func fbFind(t *fbThread, id string) *fbItem {
	for _, i := range t.items {
		if i.con.Item.ID == id {
			return i
		}
	}
	return nil
}

// engineerReplied folds an engineer's reply into the thread: an impl report
// (implement, resolve conflict) submits the bead for review, a set of
// responses answers the round's items.
func (f *Feedback) engineerReplied(ctx context.Context, w FeedbackWork, agent domain.AgentID, payload any) error {
	f.mu.Lock()
	t, ok := f.threads[w.Bead]
	f.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoFeedbackThread, w.Bead)
	}
	switch v := payload.(type) {
	case domain.ImplReport:
		if w.Delivery {
			return f.conflictResolved(ctx, t, agent, v)
		}
		return f.submitted(ctx, t, agent, v)
	case []domain.Response:
		return f.answered(ctx, t, agent, v)
	default:
		return fmt.Errorf("orchestrator: engineer %s produced %T, not an impl_report or responses", agent, payload)
	}
}

// conflictResolved records the report of a delivery conflict round and
// hands the bead back to the delivery handoff, which decides whether the
// resolution is re-delivered straight away or reviewed again (D-09, D-10).
// The loop moves no status here: the delivery owns them for that round.
func (f *Feedback) conflictResolved(ctx context.Context, t *fbThread, agent domain.AgentID, report domain.ImplReport) error {
	f.mu.Lock()
	rep := report
	t.report = &rep
	round := t.round
	f.mu.Unlock()

	f.post(ctx, t.id, agent, rep)
	f.publish(events.ResponseReceived{
		Meta: f.meta(), Bead: t.id, Round: round, Agent: agent, Report: &rep,
	})
	if f.opts.ConflictResolved == nil {
		return nil
	}
	return f.opts.ConflictResolved(ctx, t.id)
}

// submitted records an implementation and opens the review round.
func (f *Feedback) submitted(ctx context.Context, t *fbThread, agent domain.AgentID, report domain.ImplReport) error {
	f.mu.Lock()
	t.report = &report
	if t.round < 1 {
		t.round = 1
	}
	rep := report
	round := t.round
	f.mu.Unlock()

	f.post(ctx, t.id, agent, rep)
	f.publish(events.ResponseReceived{
		Meta: f.meta(), Bead: t.id, Round: round, Agent: agent, Report: &rep,
	})
	if err := f.move(ctx, t, BeadEventSubmitted, agent, "the engineer submitted its work"); err != nil {
		return err
	}
	f.refreshDiff(ctx, t)
	return f.plan(ctx, t)
}

// refuses reports whether an engineer's answer must not be applied: D-08
// says the product owner's ruling is binding, so an item that carries one
// may only be fixed. Letting a second pushback through would drop the
// ruling, re-open the dispute it closed, fire the tester that raised the
// item to judge the same argument again and the product owner to rule on
// the same item a second time — with an empty Rulings list, so it would be
// asked as if it had never ruled — and burn a review round each time round.
// The refused item keeps its ruling and stays owed, so the next round asks
// the engineer again with the ruling still in front of it.
func (f *Feedback) refuses(i *fbItem, r domain.Response) bool {
	return i.ruling != nil && r.Action != domain.ActionFixed
}

// applyResponses folds a set of engineer answers into the thread's items
// and returns the ids it refused, newest note last; the caller holds the
// lock.
//
// It is the one place an answer changes an item, so the live path
// ([Feedback.answered]) and the replay ([Feedback.Restore]) cannot drift:
// a resumed thread reopens the same occurrences, refuses the same answers
// and carries the same refusal count, which is what lets the replay work
// out afterwards which verdicts were pushback evaluations (P6-26).
func (f *Feedback) applyResponses(t *fbThread, responses []domain.Response) []string {
	var refused []string
	for _, r := range responses {
		i := fbFind(t, r.ItemID)
		if i == nil {
			continue
		}
		if f.refuses(i, r) {
			i.refusals++
			refused = append(refused, r.ItemID)
			continue
		}
		i.response = r
		for _, l := range i.con.Lenses {
			i.per[l] = domain.ItemOpen
		}
	}
	if len(refused) > 0 {
		t.notes = append(t.notes, "the product owner has already ruled on "+strings.Join(refused, ", ")+
			": that ruling is binding (D-08), so carry it out rather than argue with it again")
	}
	t.responses = append(t.responses, responses...)
	return refused
}

// answered records the engineer's answers to a round of feedback.
//
// D-08 makes the product owner's ruling binding, so an answer that argues
// with one is refused rather than applied ([Feedback.refuses]): the item
// keeps its ruling and stays owed.
func (f *Feedback) answered(ctx context.Context, t *fbThread, agent domain.AgentID, responses []domain.Response) error {
	f.mu.Lock()
	refused := f.applyResponses(t, responses)
	round := t.round
	f.mu.Unlock()

	for _, id := range refused {
		f.warn(t.id, fmt.Sprintf("the engineer argued with the product owner's binding ruling on item %q, "+
			"so the answer was not applied and the item is still owed", id), nil)
	}
	f.post(ctx, t.id, agent, responses)
	f.publish(events.ResponseReceived{
		Meta: f.meta(), Bead: t.id, Round: round, Agent: agent, Responses: responses,
	})
	if err := f.move(ctx, t, BeadEventSubmitted, agent, "the engineer answered the feedback"); err != nil {
		return err
	}
	f.refreshDiff(ctx, t)
	return f.plan(ctx, t)
}

// testerReplied folds a verdict into the thread.
func (f *Feedback) testerReplied(ctx context.Context, w FeedbackWork, agent domain.AgentID, payload any) error {
	v, ok := payload.(domain.Verdict)
	if !ok {
		return fmt.Errorf("orchestrator: tester %s produced %T, not a verdict", agent, payload)
	}
	f.mu.Lock()
	t, tracked := f.threads[w.Bead]
	if !tracked {
		f.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNoFeedbackThread, w.Bead)
	}
	if v.Lens == "" {
		v.Lens = w.Lens
	}
	if v.Round == 0 {
		v.Round = w.Round
	}
	f.applyVerdict(t, w.Lens, w.Tester, v)
	f.mu.Unlock()

	f.post(ctx, t.id, agent, v)
	f.publish(events.VerdictReceived{
		Meta: f.meta(), Bead: t.id, Round: v.Round, Agent: agent, Verdict: v,
	})
	return f.plan(ctx, t)
}

// applyVerdict is the pure half of testerReplied; the caller holds the lock.
func (f *Feedback) applyVerdict(t *fbThread, lens domain.Lens, mode agents.TesterMode, v domain.Verdict) {
	if lens == "" {
		lens = v.Lens
	}
	t.verdicts[lens] = v
	// D-09 credit is for a firing that was shown the code, and an
	// evaluate-pushback firing is not: its prompt renders no diff
	// ([Feedback.planTester] clears req.Diff), so it judges an argument
	// alone. Stamping it as this lens's look at the current commit would
	// let a round whose answers were all pushback settle a change set
	// nobody reviewed — the engineer's session still moves the code in
	// such a round, because D-14 commits its leftovers — and a tester that
	// did not approve the previous round has to see that code.
	round := f.verdictRound(t, v)
	if mode != agents.TesterModeEvaluatePushback {
		t.approved[lens] = v.Outcome == domain.VerdictApprove
		t.looked[lens] = round
		t.lookedAt[lens] = fbHead(t.diff)
	}
	t.reviewers = fbWithout(t.reviewers, lens)
	t.evaluating = fbWithout(t.evaluating, lens)

	// Items the tester restated: update this lens's occurrence.
	for _, it := range v.Items {
		i := f.itemByOrigin(t, lens, it.ID)
		if i == nil {
			f.addItem(t, lens, it, round)
			continue
		}
		i.per[lens] = it.Status
		if it.Status == domain.ItemHeld {
			i.hold = fbFirstNonEmpty(it.Detail, i.hold)
		}
		own := i.con.Raised[lens]
		own.Status = it.Status
		own.Detail = fbFirstNonEmpty(it.Detail, own.Detail)
		i.con.Raised[lens] = own
		// A tester that looked at the new code and still calls the item
		// open has rejected the answer, so the answer is stale and the
		// item goes back to the engineer (D-08).
		if mode != agents.TesterModeEvaluatePushback && it.Status == domain.ItemOpen {
			i.response = domain.Response{}
		}
	}
	// A re-review that says nothing about an item it was given leaves it
	// where it was; the schema check already refuses that, so nothing to do.
	if mode == agents.TesterModeEvaluatePushback {
		f.countHolds(t, lens, round, v)
	}
}

// verdictRound is the review round a verdict belongs to; the caller holds
// the lock.
//
// Live it is the thread's own round, because the round is what the firing
// was raised for and a model may not renumber it. A replay
// ([Feedback.Restore]) folds a whole thread in at once, though, and the
// thread's round is by then the last one: crediting round one's verdicts to
// it would tell the resumed loop that every lens has already seen the code
// the last round left behind, and collapse two rounds of pushback into one
// exchange. There the comment's own round is the truth (P6-26).
func (f *Feedback) verdictRound(t *fbThread, v domain.Verdict) int {
	if t.restoring && v.Round > 0 {
		return v.Round
	}
	return t.round
}

// countHolds bumps the pushback counter of every item this lens held in the
// round it judged, once per round rather than once per lens. D-08 budgets
// pushback *exchanges* (max_pushback_rounds per item), and one exchange is
// one engineer answer judged by everybody that raised the issue: a
// consolidated item (P6-21) two testers both hold has still had one
// exchange, and spending two of the budget on it would arbitrate a round
// early.
func (f *Feedback) countHolds(t *fbThread, lens domain.Lens, round int, v domain.Verdict) {
	for _, it := range v.Items {
		if it.Status != domain.ItemHeld {
			continue
		}
		i := f.itemByOrigin(t, lens, it.ID)
		if i == nil || i.heldAt == round {
			continue
		}
		i.heldAt = round
		i.holds++
	}
}

// itemByOrigin finds the consolidated issue a lens's own item id belongs to.
func (f *Feedback) itemByOrigin(t *fbThread, lens domain.Lens, id string) *fbItem {
	for _, i := range t.items {
		if own, ok := i.con.Raised[lens]; ok && own.ID == id {
			return i
		}
		if i.con.Item.ID == id {
			return i
		}
	}
	return nil
}

// addItem records a newly raised item as raised in round, merging it into an
// existing issue when another tester already raised the same thing (P6-21).
func (f *Feedback) addItem(t *fbThread, lens domain.Lens, it domain.FeedbackItem, round int) {
	if strings.TrimSpace(it.ID) == "" {
		return
	}
	for _, i := range t.items {
		if _, dup := i.con.Raised[lens]; dup {
			continue
		}
		if i.settled() || !fbSameIssue(i.con.Item, it, f.similar) {
			continue
		}
		f.mergeInto(i, lens, it)
		i.per[lens] = fbOr(it.Status, domain.ItemOpen)
		return
	}
	t.items = append(t.items, &fbItem{
		con: ConsolidatedItem{
			Item:   it,
			Lenses: []domain.Lens{lens},
			Raised: map[domain.Lens]domain.FeedbackItem{lens: it},
		},
		round: round,
		per:   map[domain.Lens]domain.ItemStatus{lens: fbOr(it.Status, domain.ItemOpen)},
	})
}

// mergeInto folds another tester's version of an issue into one the loop is
// already tracking (P6-21).
//
// While nobody has answered the issue it goes through the exported
// [ConsolidateItems] over every version raised so far, so the item the
// engineer is asked about — its id, title and file — is the one
// ConsolidateItems would name however the testers' verdicts happen to be
// interleaved. The scheduler fires the three lenses concurrently, so the
// arrival order is not the loop's to choose, and the live merge must not
// disagree with the batch one about which tester's wording is canonical.
//
// Once there is an answer or a ruling the item's id is what they name, so
// the merge is pairwise from then on and cannot rename it.
func (f *Feedback) mergeInto(i *fbItem, lens domain.Lens, it domain.FeedbackItem) {
	if i.response.Action != "" || i.ruling != nil {
		i.con = fbMergeItem(i.con, lens, it)
		return
	}
	vs := make([]domain.Verdict, 0, len(i.con.Raised)+1)
	for l, raised := range i.con.Raised {
		vs = append(vs, domain.Verdict{Lens: l, Items: []domain.FeedbackItem{raised}})
	}
	vs = append(vs, domain.Verdict{Lens: lens, Items: []domain.FeedbackItem{it}})
	if merged := ConsolidateItems(vs, f.similar); len(merged) == 1 {
		i.con = merged[0]
		return
	}
	// The versions no longer read as one issue against the wording
	// ConsolidateItems would make canonical (a tester restated its item).
	// The match that got here has already decided they are one, so keep
	// the pairwise merge rather than splitting a tracked issue in two.
	i.con = fbMergeItem(i.con, lens, it)
}

// poRuled folds an arbitration or an escalation ruling into the thread.
func (f *Feedback) poRuled(ctx context.Context, w FeedbackWork, agent domain.AgentID, payload any) error {
	f.mu.Lock()
	t, ok := f.threads[w.Bead]
	f.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoFeedbackThread, w.Bead)
	}
	switch v := payload.(type) {
	case domain.Arbitration:
		return f.arbitrated(ctx, t, agent, v)
	case domain.Escalation:
		return f.escalated(ctx, t, agent, v)
	default:
		return fmt.Errorf("orchestrator: product owner %s produced %T, not a ruling", agent, payload)
	}
}

// arbitrated applies a binding per-item ruling (D-08).
func (f *Feedback) arbitrated(ctx context.Context, t *fbThread, agent domain.AgentID, a domain.Arbitration) error {
	status, err := agents.ArbitrationItemStatus(a.Ruling)
	if err != nil {
		f.warn(t.id, "ignored a ruling the loop cannot apply", err)
		return err
	}
	f.mu.Lock()
	i := fbFind(t, a.ItemID)
	if i == nil {
		f.mu.Unlock()
		return fmt.Errorf("%w: item %q is not on %s", ErrUnknownFeedbackWork, a.ItemID, t.id)
	}
	ruling := a
	i.ruling = &ruling
	i.hold = ""
	for _, l := range i.con.Lenses {
		i.per[l] = status
		own := i.con.Raised[l]
		own.Status = status
		i.con.Raised[l] = own
	}
	if status == domain.ItemOpen {
		// The engineer has to act on it again, so its answer is stale.
		i.response = domain.Response{}
	}
	t.rulings = append(t.rulings, ruling)
	round := t.round
	f.mu.Unlock()

	f.post(ctx, t.id, agent, a)
	f.publish(events.ArbitrationReceived{
		Meta: f.meta(), Bead: t.id, Round: round, Agent: agent, Rulings: []domain.Arbitration{a},
	})
	return f.plan(ctx, t)
}

// escalated applies the product owner's ruling on a bead that ran out of
// review rounds (D-08, agents.EscalationBeadState).
func (f *Feedback) escalated(ctx context.Context, t *fbThread, agent domain.AgentID, e domain.Escalation) error {
	to, err := agents.EscalationBeadState(e.Outcome)
	if err != nil {
		f.warn(t.id, "ignored an escalation ruling the loop cannot apply", err)
		return err
	}
	f.mu.Lock()
	esc := e
	t.escalation, t.lastEscalation = &esc, &esc
	round := t.round
	f.mu.Unlock()

	f.post(ctx, t.id, agent, e)
	f.publish(events.ArbitrationReceived{
		Meta: f.meta(), Bead: t.id, Round: round, Agent: agent, Escalation: &esc,
	})

	switch to {
	case domain.BeadStateApproved:
		f.mu.Lock()
		for _, i := range t.items {
			for _, l := range i.con.Lenses {
				i.per[l] = domain.ItemOverruled
			}
		}
		t.notes = append(t.notes, "the product owner approved this bead with notes: "+fbOneLine(e.Notes))
		f.mu.Unlock()
		if err := f.move(ctx, t, BeadEventApprovedWithNotes, agent, "approved with notes"); err != nil {
			return err
		}
		f.mu.Lock()
		t.phase = FeedbackSettled
		f.mu.Unlock()
		return nil

	case domain.BeadStateInReview:
		if err := f.move(ctx, t, BeadEventScopeNarrowed, agent, "the product owner narrowed the scope"); err != nil {
			return err
		}
		f.mu.Lock()
		f.closeRound(t)
		t.items = nil
		t.approved = map[domain.Lens]bool{}
		t.looked = map[domain.Lens]int{}
		t.lookedAt = map[domain.Lens]string{}
		t.verdicts = map[domain.Lens]domain.Verdict{}
		t.round = 1
		t.notes = append(t.notes,
			"the product owner narrowed this bead's acceptance criteria to: "+fbOneLine(e.NewAcceptance))
		// The ruling's criteria are what every later prompt judges the bead
		// against, so they replace the bead's own; the ruling itself stays
		// in the round that made it.
		t.acceptance = fbFirstNonEmpty(e.NewAcceptance, t.acceptance)
		t.bead.Acceptance = fbFirstNonEmpty(t.acceptance, t.bead.Acceptance)
		f.mu.Unlock()
		// And they go to bd, because bd is what the bead asks for (D-06):
		// without this the testers of the next round hold the engineer to
		// criteria bd still shows unchanged, and a resume loses the ruling
		// (P6-24).
		f.narrowScope(ctx, t, e.NewAcceptance)
		return f.plan(ctx, t)

	default:
		if err := f.move(ctx, t, BeadEventBlocked, agent, fbFirstNonEmpty(e.Summary, "the product owner blocked the bead")); err != nil {
			return err
		}
		f.mu.Lock()
		t.phase = FeedbackSettled
		f.mu.Unlock()
		return nil
	}
}

// narrowScope writes a narrow_scope ruling's acceptance criteria to the bead
// itself (`bd update --acceptance`), so every later reader — the next
// tester's prompt, `bd show`, a resumed run — is held to what the product
// owner narrowed the bead to (D-08, P6-24).
//
// The thread keeps its own copy whatever happens here: a loop with no
// [FeedbackOptions.Acceptance], or a write bd refuses, still runs the
// narrowed rounds, it just cannot survive a resume, and the failure is a
// warning rather than an error because the ruling itself has already been
// applied. The write is made once per set of criteria, so a replayed thread
// that finds bd already holding them repairs nothing.
func (f *Feedback) narrowScope(ctx context.Context, t *fbThread, criteria string) {
	if criteria == "" || f.opts.Acceptance == nil {
		return
	}
	f.mu.Lock()
	restoring, stored := t.restoring, t.storedAcceptance
	f.mu.Unlock()
	if stored == criteria {
		return
	}
	if _, err := f.opts.Acceptance.Update(ctx, t.id, beads.Patch{Acceptance: &criteria}); err != nil {
		f.warn(t.id, "could not write the narrowed acceptance criteria to the bead", err)
		return
	}
	f.mu.Lock()
	t.storedAcceptance = criteria
	bead := t.bead
	f.mu.Unlock()
	if restoring {
		// A replay finishes the write the killed run owed but announces
		// nothing: the thread is being re-read, not re-run.
		return
	}
	bead.Acceptance = criteria
	f.publish(events.BeadUpdated{Meta: f.meta(), Bead: bead, Fields: []string{"acceptance"}})
}

// plan works out what the thread wants next and moves the bead's state with
// it. It is the whole of the D-08/D-09 policy.
func (f *Feedback) plan(ctx context.Context, t *fbThread) error {
	f.mu.Lock()
	if t.phase == FeedbackSettled {
		f.mu.Unlock()
		return nil
	}
	// Nothing may be decided while an agent still owes the round an answer.
	if len(t.reviewers) > 0 || len(t.evaluating) > 0 {
		f.mu.Unlock()
		return nil
	}
	// D-08: an engineer that keeps arguing with a binding ruling is not
	// going to comply, and every refused round judges nothing while
	// spending one of the bead's review rounds. The product owner decides
	// what happens to the bead instead (P6-38).
	if i := f.defiant(t); i != nil {
		t.phase = FeedbackEscalating
		id, n, round, limit := i.con.Item.ID, i.refusals, t.round, f.rounds
		t.notes = append(t.notes, fmt.Sprintf(
			"the engineer has argued with your binding ruling on %s %d times instead of carrying it out, "+
				"so the bead was escalated with %d of its %d review rounds still unspent", id, n, limit-round, limit))
		f.mu.Unlock()
		f.log.Info("an engineer argued with a binding ruling once too often, so the bead was escalated",
			slog.String("bead", t.id.String()), slog.String("item", id),
			slog.Int("refusals", n), slog.Int("round", round), slog.Int("max_rounds", limit))
		return f.move(ctx, t, BeadEventEscalated, "",
			fmt.Sprintf("the engineer argued with the ruling on %s %d times (round %d of %d)", id, n, round, limit))
	}
	var (
		evaluate  []domain.Lens
		arbitrate bool
		engineer  bool
	)
	for _, i := range t.items {
		switch {
		case i.awaitingEvaluation():
			for _, l := range i.con.Lenses {
				if i.evaluatedBy(l) {
					evaluate = fbWith(evaluate, l)
				}
			}
		case i.awaitingArbitration(f.pushbk):
			arbitrate = true
		case i.awaitingEngineer(f.pushbk):
			engineer = true
		}
	}
	switch {
	case len(evaluate) > 0:
		t.phase, t.evaluating = FeedbackEvaluating, fbSortLenses(evaluate)
		f.mu.Unlock()
		return nil
	case arbitrate:
		t.phase = FeedbackArbitrating
		f.mu.Unlock()
		return nil
	case engineer:
		f.mu.Unlock()
		return f.nextRound(ctx, t)
	}
	reviewers := f.reviewersFor(t)
	if len(reviewers) > 0 {
		t.phase, t.reviewers = FeedbackReviewing, reviewers
		commit, kind, round, lenses := fbHead(t.diff), t.kind, t.round, append([]domain.Lens(nil), reviewers...)
		restoring := t.restoring
		f.mu.Unlock()
		if !restoring {
			f.publish(events.RoundStarted{
				Meta: f.meta(), Bead: t.id, Round: round,
				RoundKind: kind, Lenses: lenses, Commit: commit,
			})
		}
		return nil
	}
	// Everybody is satisfied and nothing is left unsettled.
	f.closeRound(t)
	t.phase = FeedbackSettled
	f.mu.Unlock()
	return f.move(ctx, t, BeadEventApproved, "", "every tester approved")
}

// reviewersFor applies D-09: a tester reviews again when it still carries an
// unsettled item, or when the code moved and it did not approve last round —
// and every tester reviews again when the round moved more of the change set
// than the re-review threshold. The caller holds the lock.
func (f *Feedback) reviewersFor(t *fbThread) []domain.Lens {
	head := fbHead(t.diff)
	var out []domain.Lens
	for _, l := range f.lenses {
		// "The code moved" is a change of commit wherever there is one to
		// compare: a lens that has already reviewed this very commit has
		// nothing new to look at whatever the round number now is, and a
		// lens that has never looked, or looked at another commit, has.
		// Only where there is no change set at all does the round number
		// stand in for it.
		moved := t.looked[l] < t.round
		if head != "" {
			moved = t.lookedAt[l] != head
		}
		switch {
		case len(f.itemsFor(t, l)) > 0:
			out = append(out, l)
		case t.fullReview && t.looked[l] < t.round:
			// D-09: a round that moved more of the change set than the
			// re-review threshold sends every lens back whether it
			// approved or not — once per round, so a lens that has already
			// answered this round is not asked again.
			out = append(out, l)
		case moved && !t.approved[l]:
			out = append(out, l)
		}
	}
	return out
}

// nextRound sends the bead back to the engineer, or to the product owner
// when that would be one round too many (D-08).
func (f *Feedback) nextRound(ctx context.Context, t *fbThread) error {
	f.mu.Lock()
	if t.round >= f.rounds {
		t.phase = FeedbackEscalating
		round, limit := t.round, f.rounds
		f.mu.Unlock()
		return f.move(ctx, t, BeadEventEscalated, "",
			fmt.Sprintf("round %d of %d ended without approval", round, limit))
	}
	f.closeRound(t)
	t.round++
	t.phase = FeedbackAddressing
	t.kind = events.RoundReview
	f.mu.Unlock()
	return f.move(ctx, t, BeadEventChangesRequested, "", "the testers asked for changes")
}

// closeRound files the round that just ended in the prompt history; the
// caller holds the lock.
func (f *Feedback) closeRound(t *fbThread) {
	if t.round < 1 {
		return
	}
	r := prompt.Round{Number: t.round, Report: t.report, Responses: t.responses, Arbitrations: t.rulings}
	for _, l := range f.lenses {
		if v, ok := t.verdicts[l]; ok {
			r.Verdicts = append(r.Verdicts, v)
		}
	}
	if t.escalation != nil {
		r.Escalation = t.escalation
	}
	if t.diff != nil {
		r.Diff = t.diff.Stat()
	}
	if len(t.notes) > 0 {
		r.Notes = append([]string(nil), t.notes...)
	}
	t.history = append(t.history, r)
	t.verdicts = map[domain.Lens]domain.Verdict{}
	t.responses = nil
	t.rulings = nil
	t.notes = nil
	// The report and the escalation belong to the round that just closed:
	// leaving them set would file the same implementation report against a
	// round in which the engineer only answered items, and repeat a
	// narrow_scope ruling in every round after the one that made it.
	t.report = nil
	t.escalation = nil
}

// refreshDiff reads the change set the next review looks at and works out
// how far the engineer's round moved it (D-09).
func (f *Feedback) refreshDiff(ctx context.Context, t *fbThread) {
	bc, err := f.opts.Context(ctx, t.id)
	if err != nil {
		f.warn(t.id, "could not read the diff for the next review round", err)
		return
	}
	f.mu.Lock()
	prev, cur := t.diff, bc.Diff
	t.prevDiff, t.diff = prev, cur
	t.fullReview = false
	f.mu.Unlock()
	if cur == nil || prev == nil || f.opts.Ratio == nil {
		return
	}
	ratio, err := f.opts.Ratio(ctx, t.id, prev, cur)
	if err != nil {
		// gitx says an unmeasurable round should be read as "everything
		// changed", which sends every tester back (D-09).
		f.warn(t.id, "could not measure how far the round moved the change set", err)
		f.mu.Lock()
		t.fullReview = true
		f.mu.Unlock()
		return
	}
	f.mu.Lock()
	t.ratio = ratio
	t.fullReview = ratio.Exceeds(f.thresh)
	f.mu.Unlock()
}

// move applies a state transition and remembers where the bead now is.
//
// A restored thread ([Feedback.Restore]) re-derives transitions the killed
// run already made, so it writes only the one bd is still behind on — the
// approval of a bead whose last verdict comment landed but whose status
// write did not — and drops a transition the D-07 table refuses or one that
// would stand still, rather than warning about it.
func (f *Feedback) move(ctx context.Context, t *fbThread, ev BeadEvent, agent domain.AgentID, reason string) error {
	f.mu.Lock()
	req := StateRequest{Bead: t.id, From: t.state, Event: ev, Agent: agent, Reason: reason, Round: t.round}
	restoring := t.restoring
	f.mu.Unlock()
	if restoring {
		if to, err := Transition(req.From, req.Event); err != nil || to == req.From {
			return nil
		}
	}
	if f.opts.States == nil {
		to, err := Transition(req.From, req.Event)
		if err != nil {
			f.warn(t.id, fmt.Sprintf("ignored %q on a bead in state %q", ev, req.From), err)
			return err
		}
		f.mu.Lock()
		t.state = to
		f.mu.Unlock()
		return nil
	}
	res, err := f.opts.States.Apply(ctx, req)
	if err != nil {
		return err
	}
	f.mu.Lock()
	t.state = res.To
	f.mu.Unlock()
	return nil
}

// Observe records where somebody else moved a bead the loop is tracking —
// the delivery handoff takes an approved bead to delivering and can push it
// back itself (P6-06) — so the loop's next transition starts from the truth.
func (f *Feedback) Observe(bead domain.BeadID, state domain.BeadState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.threads[bead]; ok && state != "" {
		t.state = state
		if state.Terminal() {
			t.phase = FeedbackSettled
		}
	}
}

// CheckFailed records the qa change request a failed check command raises at
// delivery time (P6-06) and opens the round that answers it. The delivery
// handoff has already moved the bead to changes_requested and published the
// RoundStarted, so the loop writes no status here: it takes the items
// (DeliveryOutcome.Items) and puts the bead back in front of an engineer.
//
// The one status it does write is the escalation a check that failed at the
// D-08 round cap owes: there is no round left for the engineer to answer
// in, so the product owner rules on the bead instead.
func (f *Feedback) CheckFailed(ctx context.Context, bead domain.Bead, round int, items []domain.FeedbackItem) error {
	if len(items) == 0 {
		return fmt.Errorf("orchestrator: a failed check on %s raised no items to answer", bead.ID)
	}
	f.Track(bead)
	f.mu.Lock()
	t, ok := f.threads[bead.ID]
	if !ok {
		f.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNoFeedbackThread, bead.ID)
	}
	if round > 0 {
		t.round = round
	}
	t.state = domain.BeadStateChangesRequested
	t.kind = events.RoundCheckFailed
	v := domain.Verdict{
		Lens: domain.LensQA, Round: t.round, Outcome: domain.VerdictChanges,
		Summary: "the repository's check command failed at delivery time", Items: items,
	}
	f.applyVerdict(t, domain.LensQA, agents.TesterModeReview, v)
	// D-08 caps the review rounds, and this is a round: a check that fails
	// at the cap has none left to answer it, so the product owner rules on
	// the bead instead of the engineer buying one more round. The round is
	// left open in that case, exactly as [Feedback.nextRound] leaves it, so
	// the escalation prompt still carries the verdict that failed.
	capped := t.round >= f.rounds
	if capped {
		t.phase = FeedbackEscalating
	} else {
		f.closeRound(t)
		t.round++
		t.phase = FeedbackAddressing
	}
	round, limit := t.round, f.rounds
	f.mu.Unlock()

	f.post(ctx, bead.ID, "", v)
	f.publish(events.VerdictReceived{Meta: f.meta(), Bead: bead.ID, Round: v.Round, Verdict: v})
	if capped {
		f.log.Info("a failed check at the review-round cap escalated the bead",
			slog.String("bead", bead.ID.String()), slog.Int("round", round), slog.Int("max_rounds", limit))
		return f.move(ctx, t, BeadEventEscalated, "",
			fmt.Sprintf("round %d of %d ended with a failing check", round, limit))
	}
	f.log.Info("a failed check reopened a bead as a qa change request",
		slog.String("bead", bead.ID.String()), slog.Int("round", round))
	return nil
}

// ReviewAgain sends every tester back to a bead the delivery handoff put
// back in review: a conflict resolution that moved more of the change set
// than the D-09 threshold (P6-06's DeliveryNeedsReview). The bead is already
// in_review, so the loop writes no status; it opens the round.
func (f *Feedback) ReviewAgain(_ context.Context, bead domain.Bead, round int, reason string) error {
	f.Track(bead)
	f.mu.Lock()
	t, ok := f.threads[bead.ID]
	if !ok {
		f.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNoFeedbackThread, bead.ID)
	}
	if round > 0 {
		t.round = round
	}
	t.state = domain.BeadStateInReview
	t.fullReview = true
	t.approved = map[domain.Lens]bool{}
	t.looked = map[domain.Lens]int{}
	t.lookedAt = map[domain.Lens]string{}
	if s := strings.TrimSpace(reason); s != "" {
		t.notes = append(t.notes, fbOneLine(s))
	}
	t.phase = FeedbackReviewing
	t.reviewers = f.reviewersFor(t)
	lenses, kind, at := append([]domain.Lens(nil), t.reviewers...), t.kind, fbHead(t.diff)
	// The round the event names is the thread's, not the caller's: a
	// conflict round whose bead carries no sc_round metadata reaches here
	// with round 0, and publishing that would put a round number in the
	// stream that no other event on the bead uses.
	round = t.round
	f.mu.Unlock()

	f.publish(events.RoundStarted{
		Meta: f.meta(), Bead: bead.ID, Round: round, RoundKind: kind, Lenses: lenses, Commit: at,
	})
	return nil
}

// Restore rebuilds a bead's thread from the structured comments the loop
// wrote in an earlier run (D-06, D-25), so a resumed run picks the feedback
// up where it stopped instead of starting the bead again. It publishes none
// of the replay and writes no comments: the thread is being re-read, not
// re-run.
//
// The replay is the live loop's own bookkeeping, not a summary of it: the
// answers go through [Feedback.applyResponses], so a refused answer is
// refused again and the D-08 refusal count survives (P6-38), and each
// verdict is folded in the mode it was fired in ([Feedback.restoredMode]),
// so the pushback exchanges a thread has already spent are the ones it
// resumes with and it arbitrates on the round it would have (P6-26).
//
// It does write the one status the killed run owed. A run killed between
// the last verdict comment and the status write leaves a bead bd still
// holds in in_review although every tester approved it; replaying that
// thread concludes the approval, and dropping it would strand the bead in
// review for ever while [Feedback.Outstanding] reported the run finished.
// Only a transition the D-07 table allows and that actually moves the bead
// is written, so a replay that merely re-derives where the bead already is
// stays silent (see [Feedback.move]).
func (f *Feedback) Restore(ctx context.Context, bead domain.Bead, msgs []beads.Message) error {
	f.Track(bead)
	f.mu.Lock()
	t, ok := f.threads[bead.ID]
	if !ok {
		f.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNoFeedbackThread, bead.ID)
	}
	t.restoring = true
	// The criteria a narrow_scope ruling replaced the bead's with, if the
	// replay finds one; bd is asked to carry them below (P6-24).
	var narrowed string
	for _, m := range msgs {
		if m.Err != nil {
			continue
		}
		switch payload := m.Payload.(type) {
		case domain.ImplReport:
			rep := payload
			t.report = &rep
			if t.round < 1 {
				t.round = 1
			}
		case domain.Verdict:
			lens := payload.Lens
			if payload.Round > t.round {
				t.round = payload.Round
			}
			f.applyVerdict(t, lens, f.restoredMode(t, lens, payload), payload)
		case []domain.Response:
			f.applyResponses(t, payload)
		case domain.Arbitration:
			if i := fbFind(t, payload.ItemID); i != nil {
				ruling := payload
				i.ruling = &ruling
				if s, err := agents.ArbitrationItemStatus(payload.Ruling); err == nil {
					for _, l := range i.con.Lenses {
						i.per[l] = s
					}
				}
			}
		case domain.Escalation:
			esc := payload
			t.escalation, t.lastEscalation = &esc, &esc
			if esc.Outcome == domain.EscalationNarrowScope {
				t.acceptance = fbFirstNonEmpty(esc.NewAcceptance, t.acceptance)
				t.bead.Acceptance = fbFirstNonEmpty(t.acceptance, t.bead.Acceptance)
				narrowed = t.acceptance
				if bead.Acceptance == narrowed {
					// bd already carries the ruling's criteria (P6-24), so
					// the resumed thread reads them from the bead itself
					// and there is nothing to repair.
					t.storedAcceptance = narrowed
				}
			}
		}
	}
	f.mu.Unlock()

	// A run killed between the ruling and the P6-24 write leaves bd holding
	// the criteria the ruling replaced, which is the one thing a replay
	// cannot re-derive later: finish that write, the way the replay writes
	// the status the killed run owed.
	f.narrowScope(ctx, t, narrowed)

	err := f.plan(ctx, t)
	f.mu.Lock()
	t.restoring = false
	f.mu.Unlock()
	return err
}

// restoredMode works out what a replayed verdict was fired as, which the
// comment does not say (P6-26). The caller holds the lock.
//
// The two modes are not interchangeable. An evaluate-pushback firing judges
// an argument with no diff in front of it, so it neither approves the
// commit nor counts as this lens's look at it (D-09), and it is the only
// firing that spends a pushback exchange (D-08). Replaying every verdict as
// a review credits a lens with code it never saw and restarts the hold
// count at zero, which buys the engineer an extra exchange before the
// arbitration the killed run was already owed.
//
// The inference is the state the replay is already in: an evaluation is
// fired for exactly the items that were waiting for one when it ran
// ([fbItem.evaluatedBy], the same rule the live loop plans from), and the
// loop always fires the evaluation of a round before its re-review, so by
// the time a re-review's verdict is replayed those items are held, settled
// or overruled and no longer await anything.
func (f *Feedback) restoredMode(t *fbThread, lens domain.Lens, v domain.Verdict) agents.TesterMode {
	if lens == "" {
		lens = v.Lens
	}
	for _, it := range v.Items {
		if i := f.itemByOrigin(t, lens, it.ID); i != nil && i.evaluatedBy(lens) {
			return agents.TesterModeEvaluatePushback
		}
	}
	return agents.TesterModeReview
}

// post writes one structured message onto the bead (D-06).
func (f *Feedback) post(ctx context.Context, bead domain.BeadID, agent domain.AgentID, msg any) {
	if f.opts.Messages == nil {
		return
	}
	actor := "sc:" + agent.String()
	if agent == "" {
		actor = "sc:feedback"
	}
	if _, err := f.opts.Messages.PostMessage(ctx, bead, actor, msg); err != nil {
		f.warn(bead, "could not record the message on the bead", err)
	}
}

// publish sends an event, unless the loop has no bus.
func (f *Feedback) publish(ev events.Event) {
	if f.opts.Bus != nil {
		f.opts.Bus.Publish(ev)
	}
}

// warn publishes an events.Warning and logs it.
func (f *Feedback) warn(bead domain.BeadID, message string, err error) {
	w := events.Warning{Meta: f.meta(), Source: SourceFeedback, Message: message, Bead: bead}
	if err != nil {
		w.Err = err.Error()
	}
	f.publish(w)
	f.log.Warn("feedback loop: "+message, slog.String("bead", bead.String()), slog.Any("err", err))
}

// meta stamps an event with the run and the loop's clock.
func (f *Feedback) meta() events.Meta { return events.MetaAt(f.opts.Run, f.now()) }

// fbWith adds a lens once.
func fbWith(ls []domain.Lens, l domain.Lens) []domain.Lens {
	for _, c := range ls {
		if c == l {
			return ls
		}
	}
	return append(ls, l)
}

// fbWithout removes a lens.
func fbWithout(ls []domain.Lens, l domain.Lens) []domain.Lens {
	out := ls[:0]
	for _, c := range ls {
		if c != l {
			out = append(out, c)
		}
	}
	return out
}

// fbSortLenses puts lenses in domain.Lenses order.
func fbSortLenses(ls []domain.Lens) []domain.Lens {
	out := append([]domain.Lens(nil), ls...)
	sort.SliceStable(out, func(i, j int) bool { return lensRank(out[i]) < lensRank(out[j]) })
	return out
}

// fbOr returns s, or fallback when s is empty.
func fbOr(s, fallback domain.ItemStatus) domain.ItemStatus {
	if s == "" {
		return fallback
	}
	return s
}

// fbFirstNonEmpty returns the first non-blank string.
func fbFirstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// fbOneLine collapses text onto one line for a note.
func fbOneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// fbHead is the commit a diff describes, or "".
func fbHead(d *gitx.Diff) string {
	if d == nil {
		return ""
	}
	return d.Head()
}
