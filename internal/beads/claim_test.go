package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/proc"
)

// claimFakeBead is one bead of the in-process fake bd.
type claimFakeBead struct {
	Issue
}

// claimFakeCall records one bd invocation seen by the fake.
type claimFakeCall struct {
	args  []string
	actor string
	meta  map[string]any
}

// claimFake is an in-process stand-in for bd that implements the claim
// semantics verified against bd 1.2.2 (see ErrNotClaimable): `update
// --claim` wins only on an open unassigned bead or for the current
// assignee of an in_progress bead, `--metadata @file` merges, `-s`,
// `--assignee` and `--unset-metadata` apply, `show` and `list -s/-l`
// read. It is plugged into a Client's exec hook, so every call still goes
// through the client's mutex, and it records the peak number of
// overlapping calls to prove they never do.
type claimFake struct {
	t     *testing.T
	mu    sync.Mutex
	beads map[domain.BeadID]*claimFakeBead
	order []domain.BeadID
	calls []claimFakeCall
	// beforeWrite runs (under the lock) before an update is applied, so a
	// test can change the bead between a Show and the write that follows.
	beforeWrite func(*claimFake)
	// failRelease makes `update -s open` of these beads fail.
	failRelease map[domain.BeadID]bool

	active atomic.Int32
	peak   atomic.Int32
}

// newClaimFake returns an empty fake and a client wired to it.
func newClaimFake(t *testing.T) (*claimFake, *Client) {
	t.Helper()
	f := &claimFake{t: t, beads: map[domain.BeadID]*claimFakeBead{}, failRelease: map[domain.BeadID]bool{}}
	c := New("/tmp/fixture-repo", Options{Actor: "sc:orch"})
	c.exec = f.exec
	return f, c
}

// add registers a bead.
func (f *claimFake) add(id domain.BeadID, status, assignee string, meta map[string]any, labels ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := Issue{ID: id, Title: "Bead " + string(id), Status: status, Assignee: assignee, Priority: domain.PriorityDefault,
		Type: "task", Labels: labels, CreatedAt: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)}
	is.UpdatedAt = is.CreatedAt
	if meta != nil {
		is.Metadata = Metadata{}
		for k, v := range meta {
			is.Metadata[k] = v
		}
	}
	f.beads[id] = &claimFakeBead{Issue: is}
	f.order = append(f.order, id)
}

// bead returns a copy of a bead.
func (f *claimFake) bead(id domain.BeadID) Issue {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.beads[id]
	require.True(f.t, ok, "fake has no bead %s", id)
	return b.Issue
}

// updates returns the recorded `update` calls.
func (f *claimFake) updates() []claimFakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []claimFakeCall
	for _, c := range f.calls {
		if len(c.args) > 0 && c.args[0] == "update" {
			out = append(out, c)
		}
	}
	return out
}

// commands returns the subcommand of every recorded call, in order.
func (f *claimFake) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for k, c := range f.calls {
		out[k] = c.args[0]
	}
	return out
}

func (f *claimFake) exec(_ context.Context, spec proc.Spec) (proc.Result, error) {
	n := f.active.Add(1)
	for {
		p := f.peak.Load()
		if n <= p || f.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(time.Millisecond)
	defer f.active.Add(-1)

	f.mu.Lock()
	defer f.mu.Unlock()
	args := spec.Args
	for len(args) > 0 && (args[0] == "--readonly" || args[0] == "-C") {
		if args[0] == "-C" {
			args = args[2:]
		} else {
			args = args[1:]
		}
	}
	require.NotEmpty(f.t, args)
	call := claimFakeCall{args: append([]string(nil), args...), actor: spec.Env["BEADS_ACTOR"]}
	fail := func(stderr string) (proc.Result, error) {
		res := proc.Result{ExitCode: 1, Stderr: stderr + "\n"}
		return res, &proc.ExitError{Command: "bd", Args: spec.Args, Result: res}
	}
	ok := func(v any) (proc.Result, error) {
		data, err := json.Marshal(map[string]any{"data": v, "schema_version": 1})
		require.NoError(f.t, err)
		return proc.Result{Stdout: string(data)}, nil
	}
	switch args[0] {
	case "update":
		id := domain.BeadID(args[1])
		var claim bool
		var status, assignee *string
		var unset []string
		for k := 2; k < len(args); k++ {
			switch args[k] {
			case "--claim":
				claim = true
			case "--metadata":
				k++
				path := strings.TrimPrefix(args[k], "@")
				raw, err := os.ReadFile(path)
				require.NoError(f.t, err, "metadata file must exist while bd runs")
				require.NoError(f.t, json.Unmarshal(raw, &call.meta))
			case "-s":
				k++
				status = &args[k]
			case "--assignee":
				k++
				assignee = &args[k]
			case "--unset-metadata":
				k++
				unset = append(unset, args[k])
			case "--json":
			default:
				f.t.Fatalf("fake bd: unexpected update flag %q in %q", args[k], args)
			}
		}
		f.calls = append(f.calls, call)
		if f.beforeWrite != nil {
			f.beforeWrite(f)
		}
		b, found := f.beads[id]
		if !found {
			return fail(fmt.Sprintf("Error resolving %s: no issue found matching %q", id, id))
		}
		if claim {
			switch {
			case b.Assignee != "" && b.Assignee != call.actor:
				return fail(fmt.Sprintf("Error claiming %s: issue already claimed by %s", id, b.Assignee))
			case b.Status == "open" && b.Assignee == "":
				b.Assignee = call.actor
				b.Status = "in_progress"
				b.StartedAt = time.Now().UTC()
			case b.Status == "in_progress" && b.Assignee == call.actor:
			default:
				return fail(fmt.Sprintf("Error claiming %s: issue not claimable: status %s", id, b.Status))
			}
		}
		if status != nil {
			if *status == "open" && f.failRelease[id] {
				return fail("Error: database is locked")
			}
			b.Status = *status
		}
		if assignee != nil {
			b.Assignee = *assignee
		}
		if len(call.meta) > 0 {
			if b.Metadata == nil {
				b.Metadata = Metadata{}
			}
			for k, v := range call.meta {
				b.Metadata[k] = v
			}
		}
		for _, k := range unset {
			delete(b.Metadata, k)
		}
		b.UpdatedAt = time.Now().UTC()
		return ok([]Issue{b.Issue})
	case "show":
		f.calls = append(f.calls, call)
		b, found := f.beads[domain.BeadID(args[1])]
		if !found {
			res := proc.Result{ExitCode: 1, Stdout: `{"data":{"error":"no issues found matching the provided IDs"},"schema_version":1}`}
			return res, &proc.ExitError{Command: "bd", Args: spec.Args, Result: res}
		}
		return ok([]Issue{b.Issue})
	case "list":
		f.calls = append(f.calls, call)
		var statuses, labels []string
		for k := 1; k < len(args); k++ {
			switch args[k] {
			case "-s":
				k++
				statuses = strings.Split(args[k], ",")
			case "-l":
				k++
				labels = strings.Split(args[k], ",")
			case "-n", "--limit":
				k++
			}
		}
		out := []Issue{}
		for _, id := range f.order {
			b := f.beads[id]
			if len(statuses) > 0 && !claimContains(statuses, b.Status) {
				continue
			}
			keep := true
			for _, l := range labels {
				keep = keep && b.HasLabel(l)
			}
			if keep {
				out = append(out, b.Issue)
			}
		}
		return ok(out)
	default:
		f.t.Fatalf("fake bd: unexpected command %q", args)
		return proc.Result{}, nil
	}
}

func claimContains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// claimLeaseTime parses a bead's sc_lease and asserts it is recent.
func claimLeaseTime(t *testing.T, is Issue) time.Time {
	t.Helper()
	at, ok := is.Lease()
	require.True(t, ok, "sc_lease must be set: %v", is.Metadata)
	assert.WithinDuration(t, time.Now(), at, time.Minute)
	return at
}

func TestClaimForIsOneAtomicCall(t *testing.T) {
	f, c := newClaimFake(t)
	f.add("fx-1", "open", "", map[string]any{MetaRun: "r1"})

	is, err := c.ClaimFor(context.Background(), "fx-1", "eng-1")
	require.NoError(t, err)
	assert.Equal(t, "in_progress", is.Status)
	assert.Equal(t, "sc:eng-1", is.Assignee)
	assert.Equal(t, domain.AgentID("eng-1"), is.Agent())
	assert.Equal(t, "r1", string(is.RunID()), "existing metadata survives the merge")
	first := claimLeaseTime(t, is)

	calls := f.updates()
	require.Len(t, calls, 1, "claim and lease go in one bd call")
	assert.Equal(t, "sc:eng-1", calls[0].actor, "the claim runs as the agent")
	assert.Equal(t, []string{"update", "fx-1", "--claim", "--metadata", calls[0].args[4], "--json"}, calls[0].args)
	assert.True(t, strings.HasPrefix(calls[0].args[4], "@"), "metadata goes through @file: %q", calls[0].args[4])
	_, statErr := os.Stat(strings.TrimPrefix(calls[0].args[4], "@"))
	assert.True(t, os.IsNotExist(statErr), "the lease temp file is removed")
	assert.Equal(t, map[string]any{MetaAgent: "eng-1", MetaLease: first.UTC().Format(time.RFC3339)}, calls[0].meta)

	// The same agent again: idempotent, and the lease is refreshed.
	time.Sleep(1100 * time.Millisecond)
	is, err = c.ClaimFor(context.Background(), "fx-1", "eng-1")
	require.NoError(t, err)
	assert.Equal(t, "sc:eng-1", is.Assignee)
	assert.True(t, claimLeaseTime(t, is).After(first), "repeated claim renews the lease")

	// Another agent: refused, nothing written.
	before := f.bead("fx-1")
	_, err = c.ClaimFor(context.Background(), "fx-1", "eng-2")
	assert.ErrorIs(t, err, ErrAlreadyClaimed)
	assert.Contains(t, err.Error(), "already claimed by sc:eng-1")
	assert.Equal(t, before, f.bead("fx-1"), "a refused claim changes nothing")
	assert.Equal(t, "sc:orch", c.Actor(), "the client's own actor is untouched")
}

func TestClaimForAgainstCapturedSamples(t *testing.T) {
	c, bd := clientFake(t, map[string]string{"update": "update-claim"})
	is, err := c.ClaimFor(context.Background(), "fx-so0", "engineer-1")
	require.NoError(t, err)
	assert.Equal(t, "in_progress", is.Status)
	assert.Equal(t, "sc:engineer-1", is.Assignee)
	args := clientLastArgs(t, bd)
	require.Len(t, args, 8)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "update", "fx-so0", "--claim", "--metadata"}, args[:6])
	assert.True(t, strings.HasPrefix(args[6], "@"))
	assert.Equal(t, "--json", args[7])

	c2, _ := clientFake(t, map[string]string{"update": "error-claim-taken"})
	_, err = c2.ClaimFor(context.Background(), "fx-so0", "engineer-2")
	assert.ErrorIs(t, err, ErrAlreadyClaimed)

	c3, _ := clientFake(t, map[string]string{"update": "error-update-not-found"})
	_, err = c3.ClaimFor(context.Background(), "nope-1", "engineer-2")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestClaimForErrors(t *testing.T) {
	f, c := newClaimFake(t)
	f.add("fx-closed", "closed", "", nil)
	f.add("fx-review", "in_review", "", nil)
	f.add("fx-cr", "changes_requested", "sc:eng-1", map[string]any{MetaAgent: "eng-1"})

	_, err := c.ClaimFor(context.Background(), "fx-closed", "")
	assert.ErrorIs(t, err, ErrInvalid)
	_, err = c.ClaimFor(context.Background(), "", "eng-1")
	assert.ErrorIs(t, err, ErrInvalid)
	assert.Empty(t, f.commands(), "invalid input never reaches bd")

	_, err = c.ClaimFor(context.Background(), "fx-closed", "eng-1")
	assert.ErrorIs(t, err, ErrNotClaimable)
	assert.Contains(t, err.Error(), "not claimable: status closed")
	var e *Error
	require.ErrorAs(t, err, &e)
	assert.Equal(t, ErrNotClaimable, e.Kind)

	_, err = c.ClaimFor(context.Background(), "fx-review", "eng-1")
	assert.ErrorIs(t, err, ErrNotClaimable)

	// The previous round's assignee wins over the status check in bd.
	_, err = c.ClaimFor(context.Background(), "fx-cr", "eng-2")
	assert.ErrorIs(t, err, ErrAlreadyClaimed)
	_, err = c.ClaimFor(context.Background(), "fx-cr", "eng-1")
	assert.ErrorIs(t, err, ErrNotClaimable)

	_, err = c.ClaimFor(context.Background(), "nope-1", "eng-1")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestClaimRaceHasExactlyOneWinner(t *testing.T) {
	f, c := newClaimFake(t)
	f.add("fx-1", "open", "", map[string]any{MetaRun: "r1"})

	const n = 10
	var wg sync.WaitGroup
	winners := make(chan domain.AgentID, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			agent := domain.AgentID(fmt.Sprintf("eng-%d", i))
			_, err := c.ClaimFor(context.Background(), "fx-1", agent)
			errs[i] = err
			if err == nil {
				winners <- agent
			}
		}(i)
	}
	wg.Wait()
	close(winners)

	var won []domain.AgentID
	for w := range winners {
		won = append(won, w)
	}
	require.Len(t, won, 1, "exactly one claim succeeds")
	lost := 0
	for _, err := range errs {
		if err != nil {
			assert.ErrorIs(t, err, ErrAlreadyClaimed)
			lost++
		}
	}
	assert.Equal(t, n-1, lost)
	assert.Len(t, f.updates(), n, "every goroutine reached bd")
	assert.Equal(t, int32(1), f.peak.Load(), "claims must be serialised by the client")

	is := f.bead("fx-1")
	assert.Equal(t, "in_progress", is.Status)
	assert.Equal(t, ActorFor(won[0]), is.Assignee)
	assert.Equal(t, won[0], is.Agent(), "the lease names the winner")
	claimLeaseTime(t, is)
}

func TestReclaim(t *testing.T) {
	f, c := newClaimFake(t)
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	f.add("fx-1", "changes_requested", "sc:eng-1", map[string]any{MetaRun: "r1", MetaAgent: "eng-1", MetaLease: old, MetaRound: 1})

	is, err := c.Reclaim(context.Background(), "fx-1", "eng-2")
	require.NoError(t, err)
	assert.Equal(t, "in_progress", is.Status)
	assert.Equal(t, "sc:eng-2", is.Assignee)
	assert.Equal(t, domain.AgentID("eng-2"), is.Agent())
	assert.Equal(t, 1, is.Round(), "other metadata is kept")
	assert.True(t, claimLeaseTime(t, is).After(time.Now().Add(-time.Minute)))

	calls := f.updates()
	require.Len(t, calls, 1)
	assert.Equal(t, []string{"update", "fx-1", "-s", "in_progress", "--assignee", "sc:eng-2", "--metadata", calls[0].args[7], "--json"}, calls[0].args)
	assert.Equal(t, "eng-2", calls[0].meta[MetaAgent])

	_, err = c.Reclaim(context.Background(), "fx-1", "")
	assert.ErrorIs(t, err, ErrInvalid)
	_, err = c.Reclaim(context.Background(), "nope-1", "eng-2")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestRenew(t *testing.T) {
	f, c := newClaimFake(t)
	old := time.Now().Add(-time.Hour).UTC()
	f.add("fx-1", "in_progress", "sc:eng-1", map[string]any{MetaAgent: "eng-1", MetaLease: old.Format(time.RFC3339)})

	is, err := c.Renew(context.Background(), "fx-1", "eng-1")
	require.NoError(t, err)
	assert.Equal(t, "in_progress", is.Status)
	assert.Equal(t, "sc:eng-1", is.Assignee)
	assert.Equal(t, domain.AgentID("eng-1"), is.Agent(), "sc_agent is kept")
	assert.True(t, claimLeaseTime(t, is).After(old))
	assert.Equal(t, []string{"show", "update"}, f.commands(), "a read, then one guarded write")
	calls := f.updates()
	assert.Equal(t, []string{"update", "fx-1", "--claim", "--metadata", calls[0].args[4], "--json"}, calls[0].args)
	assert.Equal(t, "sc:eng-1", calls[0].actor)
	assert.Equal(t, []string{MetaLease}, claimMapKeys(calls[0].meta), "renew only writes the lease")

	// Held by someone else: no write at all.
	f.calls = nil
	_, err = c.Renew(context.Background(), "fx-1", "eng-2")
	assert.ErrorIs(t, err, ErrLeaseLost)
	assert.Contains(t, err.Error(), `assignee "sc:eng-1"`)
	assert.Equal(t, []string{"show"}, f.commands())

	// Moved on to review: lost.
	f.add("fx-2", "in_review", "sc:eng-1", map[string]any{MetaAgent: "eng-1"})
	_, err = c.Renew(context.Background(), "fx-2", "eng-1")
	assert.ErrorIs(t, err, ErrLeaseLost)

	// Released meanwhile: lost, and never re-claimed.
	f.add("fx-3", "open", "", nil)
	_, err = c.Renew(context.Background(), "fx-3", "eng-1")
	assert.ErrorIs(t, err, ErrLeaseLost)
	assert.Equal(t, "open", f.bead("fx-3").Status)
	assert.Empty(t, f.bead("fx-3").Assignee)

	// Changed hands between the read and the write: bd's atomic check.
	f.add("fx-4", "in_progress", "sc:eng-1", map[string]any{MetaAgent: "eng-1"})
	f.beforeWrite = func(f *claimFake) {
		f.beads["fx-4"].Assignee = "sc:eng-9"
		f.beforeWrite = nil
	}
	_, err = c.Renew(context.Background(), "fx-4", "eng-1")
	assert.ErrorIs(t, err, ErrLeaseLost)
	assert.ErrorIs(t, err, ErrAlreadyClaimed)

	_, err = c.Renew(context.Background(), "nope-1", "eng-1")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = c.Renew(context.Background(), "fx-1", "")
	assert.ErrorIs(t, err, ErrInvalid)
}

func TestRelease(t *testing.T) {
	c, bd := clientFake(t, map[string]string{"update": "update-release"})
	is, err := c.Release(context.Background(), "fx-ctt")
	require.NoError(t, err)
	assert.Equal(t, "open", is.Status)
	assert.Empty(t, is.Assignee)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "update", "fx-ctt", "-s", "open", "--assignee", "",
		"--unset-metadata", MetaAgent, "--unset-metadata", MetaLease, "--json"}, clientLastArgs(t, bd))

	f, c2 := newClaimFake(t)
	f.add("fx-1", "in_progress", "sc:eng-1", map[string]any{MetaRun: "r1", MetaAgent: "eng-1", MetaLease: "2026-09-02T10:00:00Z"})
	is, err = c2.Release(context.Background(), "fx-1")
	require.NoError(t, err)
	assert.Equal(t, "open", is.Status)
	assert.Empty(t, is.Assignee)
	assert.Equal(t, Metadata{MetaRun: "r1"}, is.Metadata, "only the lease keys are removed")
	_, ok := LeaseHolder(is)
	assert.False(t, ok)

	_, err = c2.Release(context.Background(), "")
	assert.ErrorIs(t, err, ErrInvalid)
	_, err = c2.Release(context.Background(), "nope-1")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestLeaseHolderAndTime(t *testing.T) {
	lease := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	started := time.Date(2026, 9, 2, 11, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)

	agent, ok := LeaseHolder(Issue{Assignee: "sc:eng-1", Metadata: Metadata{MetaAgent: "eng-1"}})
	assert.True(t, ok)
	assert.Equal(t, domain.AgentID("eng-1"), agent)
	agent, ok = LeaseHolder(Issue{Assignee: "sc:eng-2"})
	assert.True(t, ok, "falls back to the assignee")
	assert.Equal(t, domain.AgentID("eng-2"), agent)
	_, ok = LeaseHolder(Issue{Assignee: "alice"})
	assert.False(t, ok, "a foreign assignee is not a holder")
	_, ok = LeaseHolder(Issue{Assignee: DefaultActor})
	assert.False(t, ok)
	_, ok = LeaseHolder(Issue{})
	assert.False(t, ok)

	at, ok := LeaseTime(Issue{Metadata: Metadata{MetaLease: lease.Format(time.RFC3339)}, StartedAt: started, UpdatedAt: updated})
	assert.True(t, ok)
	assert.Equal(t, lease, at)
	at, ok = LeaseTime(Issue{StartedAt: started, UpdatedAt: updated})
	assert.True(t, ok)
	assert.Equal(t, started, at)
	at, ok = LeaseTime(Issue{UpdatedAt: updated})
	assert.True(t, ok)
	assert.Equal(t, updated, at)
	at, ok = LeaseTime(Issue{Metadata: Metadata{MetaLease: "garbage"}})
	assert.False(t, ok)
	assert.True(t, at.IsZero())
}

func TestStaleLeases(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	at := func(age time.Duration) string { return now.Add(-age).Format(time.RFC3339) }
	issues := []Issue{
		{ID: "old-dead", Status: "in_progress", Assignee: "sc:eng-1", Metadata: Metadata{MetaAgent: "eng-1", MetaLease: at(time.Hour)}},
		{ID: "old-alive", Status: "in_progress", Assignee: "sc:eng-2", Metadata: Metadata{MetaAgent: "eng-2", MetaLease: at(time.Hour)}},
		{ID: "fresh-dead", Status: "in_progress", Assignee: "sc:eng-3", Metadata: Metadata{MetaAgent: "eng-3", MetaLease: at(time.Minute)}},
		{ID: "no-lease", Status: "in_progress", Assignee: "sc:eng-4", StartedAt: now.Add(-2 * time.Hour)},
		{ID: "no-time", Status: "in_progress", Assignee: "sc:eng-5"},
		{ID: "foreign", Status: "in_progress", Assignee: "alice", Metadata: Metadata{MetaLease: at(time.Hour)}},
		{ID: "reviewing", Status: "in_review", Assignee: "sc:eng-6", Metadata: Metadata{MetaAgent: "eng-6", MetaLease: at(time.Hour)}},
		{ID: "open", Status: "open"},
	}
	alive := func(a domain.AgentID) bool { return a == "eng-2" }
	ids := func(stale []StaleLease) []string {
		var out []string
		for _, s := range stale {
			out = append(out, string(s.ID))
		}
		return out
	}

	stale := StaleLeases(issues, RecoverOptions{MaxAge: 15 * time.Minute, Alive: alive, Now: func() time.Time { return now }})
	assert.Equal(t, []string{"old-dead", "no-lease", "no-time"}, ids(stale))
	assert.Equal(t, domain.AgentID("eng-1"), stale[0].Agent)
	assert.Equal(t, "sc:eng-1", stale[0].Assignee)
	assert.Equal(t, time.Hour, stale[0].Age)
	assert.Equal(t, now.Add(-time.Hour), stale[0].LeaseAt)
	assert.Equal(t, "old-dead held by eng-1 for 1h0m0s", stale[0].String())
	assert.Equal(t, 2*time.Hour, stale[1].Age, "started_at stands in for a missing lease")
	assert.True(t, stale[2].LeaseAt.IsZero())
	assert.Equal(t, "no-time held by eng-5 with no lease", stale[2].String())

	// Resume: nobody is alive and any age counts.
	stale = StaleLeases(issues, RecoverOptions{Now: func() time.Time { return now }})
	assert.Equal(t, []string{"old-dead", "old-alive", "fresh-dead", "no-lease", "no-time"}, ids(stale))

	// Everyone alive: nothing to do.
	assert.Empty(t, StaleLeases(issues, RecoverOptions{Alive: func(domain.AgentID) bool { return true }}))
	assert.Empty(t, StaleLeases(nil, RecoverOptions{}))
}

func TestRecoverStale(t *testing.T) {
	f, c := newClaimFake(t)
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	fresh := time.Now().UTC().Format(time.RFC3339)
	f.add("fx-dead", "in_progress", "sc:eng-1", map[string]any{MetaRun: "r1", MetaAgent: "eng-1", MetaLease: old}, "sc:run:r1")
	f.add("fx-alive", "in_progress", "sc:eng-2", map[string]any{MetaRun: "r1", MetaAgent: "eng-2", MetaLease: old}, "sc:run:r1")
	f.add("fx-fresh", "in_progress", "sc:eng-3", map[string]any{MetaRun: "r1", MetaAgent: "eng-3", MetaLease: fresh}, "sc:run:r1")
	f.add("fx-foreign", "in_progress", "alice", nil, "sc:run:r1")
	f.add("fx-open", "open", "", map[string]any{MetaRun: "r1"}, "sc:run:r1")
	f.add("fx-other", "in_progress", "sc:eng-4", map[string]any{MetaRun: "r2", MetaAgent: "eng-4", MetaLease: old}, "sc:run:r2")
	f.add("fx-nolease", "in_progress", "sc:eng-5", map[string]any{MetaRun: "r1"}, "sc:run:r1")
	alive := func(a domain.AgentID) bool { return a == "eng-2" }

	// Dry run: reports, changes nothing.
	res, err := c.RecoverStale(context.Background(), RecoverOptions{MaxAge: DefaultLeaseMaxAge, Alive: alive, DryRun: true})
	require.NoError(t, err)
	assert.True(t, res.DryRun)
	assert.Equal(t, 6, res.Checked, "every in_progress bead is examined")
	assert.Equal(t, []domain.BeadID{"fx-dead", "fx-other", "fx-nolease"}, claimStaleIDs(res.Stale))
	assert.Empty(t, res.Released)
	assert.Empty(t, res.IDs())
	assert.Equal(t, []string{"list"}, f.commands())
	assert.Equal(t, "in_progress", f.bead("fx-dead").Status)

	// For real, limited to run r1.
	f.calls = nil
	res, err = c.RecoverStale(context.Background(), RecoverOptions{MaxAge: DefaultLeaseMaxAge, Alive: alive, Run: "r1"})
	require.NoError(t, err)
	assert.Equal(t, 5, res.Checked)
	assert.Equal(t, []domain.BeadID{"fx-dead", "fx-nolease"}, res.IDs())
	assert.Equal(t, res.Stale, res.Released)
	assert.Empty(t, res.Failed)
	assert.Equal(t, []string{"list", "update", "update"}, f.commands())
	list := f.calls[0].args
	assert.Equal(t, []string{"list", "--json", "-n", "0", "--flat", "-s", "in_progress", "-l", "sc:run:r1"}, list)
	for _, id := range []domain.BeadID{"fx-dead", "fx-nolease"} {
		is := f.bead(id)
		assert.Equal(t, "open", is.Status, id)
		assert.Empty(t, is.Assignee, id)
		_, held := LeaseHolder(is)
		assert.False(t, held, id)
		assert.Equal(t, "r1", string(is.RunID()), "run metadata survives")
	}
	assert.Equal(t, "in_progress", f.bead("fx-alive").Status, "a live agent keeps its bead")
	assert.Equal(t, "in_progress", f.bead("fx-fresh").Status, "a fresh lease is kept")
	assert.Equal(t, "in_progress", f.bead("fx-foreign").Status, "foreign claims are never touched")
	assert.Equal(t, "in_progress", f.bead("fx-other").Status, "other runs are out of scope")

	// Resume across every run: nobody alive, any age; one release fails.
	f.calls = nil
	f.failRelease["fx-fresh"] = true
	res, err = c.RecoverStale(context.Background(), RecoverOptions{})
	require.NoError(t, err)
	assert.Equal(t, []domain.BeadID{"fx-alive", "fx-other"}, res.IDs())
	require.Len(t, res.Failed, 1)
	assert.Equal(t, domain.BeadID("fx-fresh"), res.Failed[0].ID)
	assert.ErrorIs(t, res.Failed[0].Err, ErrLocked)
	assert.Equal(t, []domain.BeadID{"fx-alive", "fx-fresh", "fx-other"}, claimStaleIDs(res.Stale))
	assert.Equal(t, "in_progress", f.bead("fx-fresh").Status)
	assert.Equal(t, "open", f.bead("fx-other").Status, "recovery continues past a failure")

	// The failed one is picked up next time; then nothing is left.
	delete(f.failRelease, "fx-fresh")
	res, err = c.RecoverStale(context.Background(), RecoverOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, res.Checked)
	assert.Equal(t, []domain.BeadID{"fx-fresh"}, res.IDs())
	res, err = c.RecoverStale(context.Background(), RecoverOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Checked, "only the foreign claim remains in progress")
	assert.Empty(t, res.Stale)
}

func TestRecoverStaleListFailure(t *testing.T) {
	c, _ := clientFake(t, map[string]string{"list": "error-no-project"})
	_, err := c.RecoverStale(context.Background(), RecoverOptions{})
	assert.ErrorIs(t, err, ErrNoProject)
}

func TestRecoverStaleCancelled(t *testing.T) {
	f, c := newClaimFake(t)
	f.add("fx-dead", "in_progress", "sc:eng-1", map[string]any{MetaAgent: "eng-1", MetaLease: "2026-09-02T10:00:00Z"})
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after the listing, before the release.
	f.beforeWrite = func(*claimFake) { cancel() }
	res, err := c.RecoverStale(ctx, RecoverOptions{})
	require.NoError(t, err, "the listing succeeded; the outcome is per bead")
	require.Len(t, res.Stale, 1)
	if len(res.Failed) == 1 {
		assert.ErrorIs(t, res.Failed[0].Err, context.Canceled)
	} else {
		assert.Equal(t, []domain.BeadID{"fx-dead"}, res.IDs(), "the write was already in flight")
	}
}

func TestClaimErrorRefinesNotClaimable(t *testing.T) {
	e := &Error{Command: "update", Message: "issue not claimable: status closed"}
	assert.ErrorIs(t, claimError(e), ErrNotClaimable)
	assert.Equal(t, ErrNotClaimable, e.Kind)

	taken := &Error{Command: "update", Kind: ErrAlreadyClaimed, Message: "issue already claimed by sc:eng-1"}
	assert.ErrorIs(t, claimError(taken), ErrAlreadyClaimed)
	assert.NotErrorIs(t, claimError(taken), ErrNotClaimable)

	assert.NoError(t, claimError(nil))
	plain := errors.New("boom")
	assert.Same(t, plain, claimError(plain))
}

func TestLeaseDefaults(t *testing.T) {
	assert.Less(t, DefaultRenewInterval, DefaultLeaseMaxAge/2, "several renewals fit inside one max age")
}

// staleIDs returns the ids of stale leases.
func claimStaleIDs(stale []StaleLease) []domain.BeadID {
	var out []domain.BeadID
	for _, s := range stale {
		out = append(out, s.ID)
	}
	return out
}

// mapKeys returns the keys of m sorted.
func claimMapKeys(m map[string]any) []string {
	return sortedKeys(m)
}
