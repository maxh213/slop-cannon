package beads

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// DefaultPollInterval is how often the Watcher lists the beads database
// when the caller sets no interval (`graph_poll_interval`, P3-07). One
// serialised `bd list` takes about 0.2 s (Appendix A), so two seconds keeps
// the client mostly free for writers.
const DefaultPollInterval = 2 * time.Second

// WatcherSource is the events.Warning.Source the Watcher uses.
const WatcherSource = "graph"

// Clock is the time source of a Watcher, so tests drive polling with a fake
// clock instead of sleeping.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// After returns a channel that receives the time once d has elapsed.
	After(d time.Duration) <-chan time.Time
}

// SystemClock returns the wall clock.
func SystemClock() Clock { return systemClock{} }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Lister is the one read the Watcher needs; *Client satisfies it.
type Lister interface {
	List(ctx context.Context, f ListFilter) ([]Issue, error)
}

// WatcherOptions configures a Watcher. The zero value polls everything
// every DefaultPollInterval with the wall clock and publishes nothing.
type WatcherOptions struct {
	// Interval is the polling period; zero or negative means
	// DefaultPollInterval.
	Interval time.Duration
	// Filter scopes the listing (typically Labels: {RunLabel(run)} so
	// foreign beads never reach the diff). All is always forced on: the
	// watcher must see closed beads to report them as changed.
	Filter ListFilter
	// Run stamps the published events; the run the watcher belongs to.
	Run domain.RunID
	// Epic is the run's epic bead. It is reported in GraphChanged.Epic and
	// excluded from GraphChanged.Beads (they are the work beads).
	Epic domain.BeadID
	// Bus receives one events.GraphChanged per non-empty change and an
	// events.Warning when a poll starts failing. Nil publishes nothing;
	// the Changes channel still works.
	Bus *events.Bus
	// Statuses converts foreign statuses safely (SafeBeads) for the
	// published snapshot. Nil still resolves every D-07 state.
	Statuses StatusMap
	// Clock is the time source; nil means SystemClock.
	Clock Clock
	// Logger receives one Debug line per poll and a Warn line when polling
	// starts failing; nil means slog.Default().
	Logger *slog.Logger
}

// Change is one observed difference between two consecutive listings.
// Ids are sorted and each appears in exactly one of Added, Changed or
// Removed. Bursts of writes between two polls, and changes the consumer
// has not read yet, are coalesced into one Change (see Watcher.Changes).
type Change struct {
	// At is the clock time of the poll that produced the change.
	At time.Time
	// Initial is true for the first successful poll, which reports every
	// bead as Added so consumers can build their initial view.
	Initial bool
	// Added lists beads absent from the previous listing.
	Added []domain.BeadID
	// Changed lists beads whose updated_at, status, assignee or blockers
	// differ from the previous listing.
	Changed []domain.BeadID
	// Removed lists beads present before and gone now (deleted, or moved
	// out of the filter).
	Removed []domain.BeadID
	// Issues is the full listing the change was computed against.
	Issues []Issue
}

// Empty reports whether nothing was added, changed or removed.
func (c Change) Empty() bool {
	return len(c.Added) == 0 && len(c.Changed) == 0 && len(c.Removed) == 0
}

// IDs returns every affected id, sorted.
func (c Change) IDs() []domain.BeadID {
	ids := make([]domain.BeadID, 0, len(c.Added)+len(c.Changed)+len(c.Removed))
	ids = append(ids, c.Added...)
	ids = append(ids, c.Changed...)
	ids = append(ids, c.Removed...)
	sortIDs(ids)
	return ids
}

// Graph builds the dependency graph of the listing (NewGraph).
func (c Change) Graph() *Graph { return NewGraph(c.Issues) }

// WatcherStats is a point-in-time snapshot of a Watcher's counters.
type WatcherStats struct {
	// Polls counts every listing attempt.
	Polls int
	// Failures counts listings that returned an error.
	Failures int
	// Changes counts non-empty changes observed.
	Changes int
	// Coalesced counts changes merged into one the consumer had not read
	// yet.
	Coalesced int
	// LastPoll is when the last listing attempt finished; zero before the
	// first.
	LastPoll time.Time
	// LastError is the error of the most recent poll, nil when it
	// succeeded.
	LastError error
}

// Watcher polls a beads listing and reports what changed (P3-07). bd 1.2.2
// has no events journal, so the watcher lists every bead of its filter at
// each tick and diffs by updated_at (plus status, assignee and blockers,
// which cover any write that leaves updated_at alone).
//
// Each non-empty change is delivered on Changes and, when a Bus is set,
// published as events.GraphChanged with the full snapshot. Only the
// orchestrator writes beads (D-21), so the watcher exists to notice the
// effects of its own writes and of humans using bd directly, and to feed
// the graph view (P7-02) without every writer having to publish.
type Watcher struct {
	lister   Lister
	interval time.Duration
	filter   ListFilter
	run      domain.RunID
	epic     domain.BeadID
	bus      *events.Bus
	statuses StatusMap
	clock    Clock
	logger   *slog.Logger

	kick    chan struct{}
	changes chan Change

	mu       sync.Mutex // guards everything below and the changes buffer
	snapshot []Issue
	primed   bool
	failing  bool
	stats    WatcherStats
}

// NewWatcher returns a stopped Watcher reading through lister; call Run to
// start polling. A nil lister panics.
func NewWatcher(lister Lister, opts WatcherOptions) *Watcher {
	if lister == nil {
		panic("beads: NewWatcher: nil lister")
	}
	w := &Watcher{
		lister:   lister,
		interval: opts.Interval,
		filter:   opts.Filter,
		run:      opts.Run,
		epic:     opts.Epic,
		bus:      opts.Bus,
		statuses: opts.Statuses,
		clock:    opts.Clock,
		logger:   opts.Logger,
		kick:     make(chan struct{}, 1),
		changes:  make(chan Change, 1),
	}
	w.filter.All = true
	if w.interval <= 0 {
		w.interval = DefaultPollInterval
	}
	if w.clock == nil {
		w.clock = SystemClock()
	}
	if w.logger == nil {
		w.logger = slog.Default()
	}
	return w
}

// Interval returns the polling period in force.
func (w *Watcher) Interval() time.Duration { return w.interval }

// Changes returns the channel non-empty changes arrive on. It holds one
// pending change: when a new change is observed before the consumer read
// the previous one, the two are merged (ids unioned, latest snapshot) and
// WatcherStats.Coalesced grows, so a slow consumer never blocks polling
// and never misses an id. The channel is closed when Run returns.
func (w *Watcher) Changes() <-chan Change { return w.changes }

// Kick requests a poll now rather than at the next tick, for a caller that
// just wrote to beads and wants the change noticed. Several kicks before
// the poll runs collapse into one; a kick before Run starts makes its
// first iteration poll twice, which is harmless.
func (w *Watcher) Kick() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// Run polls immediately and then on every tick or Kick until ctx is done,
// returning ctx.Err(). Listing failures are logged, counted and published
// as one events.Warning per failure streak; they never stop the loop. Run
// must not be called twice on the same Watcher.
func (w *Watcher) Run(ctx context.Context) error {
	defer close(w.changes)
	for {
		if _, err := w.Poll(ctx); err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.clock.After(w.interval):
		case <-w.kick:
		}
	}
}

// Poll lists once, records the snapshot, delivers the change on Changes
// and publishes it, and returns it. Callers that do not use Run (a
// one-shot `sc status` refresh, or a test) can drive the watcher with
// Poll alone. The first successful poll reports every bead as Added with
// Initial set; an empty listing on the first poll is still an initial
// (empty) change. A failed listing leaves the snapshot untouched.
func (w *Watcher) Poll(ctx context.Context) (Change, error) {
	issues, err := w.lister.List(ctx, w.filter)
	now := w.clock.Now()
	if err != nil {
		w.recordFailure(ctx, now, err)
		return Change{}, err
	}
	ch := w.recordSuccess(now, issues)
	if ch.Empty() && !ch.Initial {
		return ch, nil
	}
	w.deliver(ch)
	w.publish(ch)
	return ch, nil
}

// Issues returns a copy of the most recent successful listing.
func (w *Watcher) Issues() []Issue {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Issue(nil), w.snapshot...)
}

// Graph builds the dependency graph of the most recent successful listing.
func (w *Watcher) Graph() *Graph { return NewGraph(w.Issues()) }

// Stats returns the current counters.
func (w *Watcher) Stats() WatcherStats {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stats
}

// recordFailure counts a failed poll and warns once per failure streak.
func (w *Watcher) recordFailure(ctx context.Context, now time.Time, err error) {
	w.mu.Lock()
	w.stats.Polls++
	w.stats.Failures++
	w.stats.LastPoll = now
	w.stats.LastError = err
	first := !w.failing
	w.failing = true
	w.mu.Unlock()
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return
	}
	if first {
		w.logger.WarnContext(ctx, "graph poll failed", "error", err.Error())
		if w.bus != nil {
			w.bus.Publish(events.Warning{
				Meta:    events.MetaAt(w.run, now),
				Source:  WatcherSource,
				Message: "polling the beads graph failed; the graph view may be stale until it recovers",
				Err:     err.Error(),
			})
		}
		return
	}
	w.logger.DebugContext(ctx, "graph poll still failing", "error", err.Error())
}

// recordSuccess diffs the listing against the previous one, stores it, and
// returns the change.
func (w *Watcher) recordSuccess(now time.Time, issues []Issue) Change {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stats.Polls++
	w.stats.LastPoll = now
	w.stats.LastError = nil
	if w.failing {
		w.failing = false
		w.logger.Info("graph poll recovered", "beads", len(issues))
	}
	ch := DiffIssues(w.snapshot, issues)
	ch.At = now
	ch.Initial = !w.primed
	w.primed = true
	w.snapshot = issues
	if !ch.Empty() {
		w.stats.Changes++
	}
	w.logger.Debug("graph poll", "beads", len(issues), "added", len(ch.Added), "changed", len(ch.Changed), "removed", len(ch.Removed))
	return ch
}

// deliver puts ch on the Changes channel, merging it with a change the
// consumer has not read yet. The mutex makes the drain-then-send atomic
// with respect to other polls, so the one-slot buffer always has room.
func (w *Watcher) deliver(ch Change) {
	w.mu.Lock()
	defer w.mu.Unlock()
	select {
	case pending := <-w.changes:
		ch = MergeChanges(pending, ch)
		w.stats.Coalesced++
	default:
	}
	w.changes <- ch
}

// publish sends the GraphChanged event for ch.
func (w *Watcher) publish(ch Change) {
	if w.bus == nil {
		return
	}
	w.bus.Publish(events.GraphChanged{
		Meta:  events.MetaAt(w.run, ch.At),
		Epic:  w.epic,
		Beads: w.workBeads(ch.Issues),
	})
}

// workBeads converts the listing to domain beads, leaving out the epic
// (and any other epic bead: an epic is a container, not work).
func (w *Watcher) workBeads(issues []Issue) []domain.Bead {
	beads := make([]domain.Bead, 0, len(issues))
	for _, is := range issues {
		if is.ID == w.epic || is.IsEpic() {
			continue
		}
		b, _ := is.SafeBead(w.statuses)
		beads = append(beads, b)
	}
	return beads
}

// DiffIssues computes the Change from the prev listing to next: ids only
// in next are Added, ids only in prev are Removed, and ids in both whose
// updated_at, status, assignee or blockers differ are Changed. At and
// Initial are left zero; Issues is next.
func DiffIssues(prev, next []Issue) Change {
	ch := Change{Issues: next}
	old := indexIssues(prev)
	seen := make(map[domain.BeadID]bool, len(next))
	for _, is := range next {
		if seen[is.ID] {
			continue
		}
		seen[is.ID] = true
		before, ok := old[is.ID]
		switch {
		case !ok:
			ch.Added = append(ch.Added, is.ID)
		case issueChanged(before, is):
			ch.Changed = append(ch.Changed, is.ID)
		}
	}
	for _, is := range prev {
		if !seen[is.ID] {
			ch.Removed = append(ch.Removed, is.ID)
			seen[is.ID] = true
		}
	}
	sortIDs(ch.Added)
	sortIDs(ch.Changed)
	sortIDs(ch.Removed)
	return ch
}

// MergeChanges folds a later change into an earlier unread one: an id
// added then removed cancels out, an id removed then re-added is Changed,
// and everything else is unioned. Issues and At come from later; Initial
// is kept when either was initial.
func MergeChanges(earlier, later Change) Change {
	added := idSet(earlier.Added)
	changed := idSet(earlier.Changed)
	removed := idSet(earlier.Removed)
	for _, id := range later.Added {
		if removed[id] {
			delete(removed, id)
			changed[id] = true
			continue
		}
		added[id] = true
	}
	for _, id := range later.Changed {
		if !added[id] {
			changed[id] = true
		}
		delete(removed, id)
	}
	for _, id := range later.Removed {
		delete(changed, id)
		if added[id] {
			delete(added, id)
			continue
		}
		removed[id] = true
	}
	return Change{
		At:      later.At,
		Initial: earlier.Initial || later.Initial,
		Added:   setIDs(added),
		Changed: setIDs(changed),
		Removed: setIDs(removed),
		Issues:  later.Issues,
	}
}

// issueChanged reports whether the fields the watcher tracks differ.
func issueChanged(a, b Issue) bool {
	if !a.UpdatedAt.Equal(b.UpdatedAt) || a.Status != b.Status || a.Assignee != b.Assignee {
		return true
	}
	ba, bb := a.Blockers(), b.Blockers()
	if len(ba) != len(bb) {
		return true
	}
	sortIDs(ba)
	sortIDs(bb)
	for i := range ba {
		if ba[i] != bb[i] {
			return true
		}
	}
	return false
}

// indexIssues maps issues by id; the first occurrence of a duplicate wins.
func indexIssues(issues []Issue) map[domain.BeadID]Issue {
	m := make(map[domain.BeadID]Issue, len(issues))
	for _, is := range issues {
		if _, ok := m[is.ID]; !ok {
			m[is.ID] = is
		}
	}
	return m
}

func idSet(ids []domain.BeadID) map[domain.BeadID]bool {
	m := make(map[domain.BeadID]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func setIDs(m map[domain.BeadID]bool) []domain.BeadID {
	if len(m) == 0 {
		return nil
	}
	ids := make([]domain.BeadID, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sortIDs(ids)
	return ids
}

func sortIDs(ids []domain.BeadID) {
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
}
