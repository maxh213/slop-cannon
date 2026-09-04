package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// The run-lock tests (P9-04). Nothing here reads the wall clock, spawns an
// agent or touches the real home: the clock, the hostname, the liveness
// probe and the process start stamp are all injectable, and the one place a
// real process is used (a child that has already exited) is deterministic
// because the child is waited for before its pid is written into a lock.

// lockEpoch is the instant every lock test stamps its records with.
var lockEpoch = time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)

// lockFixture is a repository whose state directory is a temp dir.
type lockFixture struct {
	Repo  string
	State string
}

// lockSetup points XDG at temp directories so nothing touches the real home
// (D-23) and returns the repository and its state directory.
func lockSetup(t *testing.T) lockFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))

	repo := filepath.Join(home, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	state := config.StateDir(repo)
	require.NoError(t, os.MkdirAll(state, 0o700))
	return lockFixture{Repo: repo, State: state}
}

// lockOpts is the deterministic base every test starts from: this test
// process is "sc-host", the clock is frozen, and neither the liveness probe
// nor the start stamp reaches the kernel unless the test says so.
func lockOpts() LockOptions {
	return LockOptions{
		Self:    4242,
		Host:    "sc-host",
		Now:     func() time.Time { return lockEpoch },
		Alive:   func(int) bool { return false },
		StartOf: func(int) string { return "" },
	}
}

// writeLock writes rec as the repository's run lock.
func writeLock(t *testing.T, state string, rec LockRecord) string {
	t.Helper()
	path := RepoLockPath(state)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	data, err := json.MarshalIndent(rec, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(data, '\n'), 0o600))
	return path
}

// liveRecord is a lock held by another live sc on this machine.
func liveRecord() LockRecord {
	return LockRecord{LockHolder: LockHolder{
		Run: "20260904-090000-0001", PID: 777, Host: "sc-host", TakenAt: lockEpoch.Add(-time.Hour),
	}}
}

// notices collects the lines a probe hands to LockOptions.Notice.
type notices struct{ lines []string }

func (n *notices) sink() func(string) { return func(s string) { n.lines = append(n.lines, s) } }

// ------------------------------------------------------------------- paths

// TestRepoLockPathIsTheOneRunLock: the preflight (which knows the
// repository) and P6-01 (which knows the run directory) have to name the
// same file, and it is never inside the user's repository (D-23).
func TestRepoLockPathIsTheOneRunLock(t *testing.T) {
	f := lockSetup(t)
	runDir := config.RunDir(f.Repo, "20260904-100000-abcd")
	require.Equal(t, LockPath(runDir), RepoLockPath(f.State))
	require.Equal(t, filepath.Join(f.State, LockFileName), RepoLockPath(f.State))
	require.NotContains(t, RepoLockPath(f.State), filepath.Join(f.Repo, ".slop-cannon"))
	require.Equal(t, RepoLockPath(f.State), RepoLockPath(f.State+string(filepath.Separator)),
		"a trailing separator names the same lock")
}

func TestLockStateString(t *testing.T) {
	require.Equal(t, "free", LockFree.String())
	require.Equal(t, "mine", LockMine.String())
	require.Equal(t, "live", LockLive.String())
	require.Equal(t, "stale", LockStale.String())
	require.Equal(t, "elsewhere", LockElsewhere.String())
	require.Equal(t, "corrupt", LockCorrupt.String())
	require.Equal(t, "unknown", LockState(99).String())
}

// TestLockMessagesSurviveAnEmptyRecord: a lock written by something that
// did not fill every field still produces a sentence, not a blank.
func TestLockMessagesSurviveAnEmptyRecord(t *testing.T) {
	f := lockSetup(t)
	writeLock(t, f.State, LockRecord{LockHolder: LockHolder{Run: "20260904-090000-0004", PID: 777}})
	opts := lockOpts()
	opts.Alive = func(int) bool { return true }

	_, err := ClaimRunLock(f.State, opts)
	require.ErrorIs(t, err, ErrLocked)
	require.ErrorContains(t, err, "since an unknown time")

	var n notices
	opts.Force, opts.Notice = true, n.sink()
	p, err := ClaimRunLock(f.State, opts)
	require.NoError(t, err)
	require.Contains(t, p.Notice, "an unnamed host")
	require.Equal(t, []string{p.Notice}, n.lines)
}

// ------------------------------------------------------------- the verdicts

func TestInspectLockFree(t *testing.T) {
	f := lockSetup(t)
	p := InspectLock(f.State, lockOpts())
	require.Equal(t, LockFree, p.State)
	require.False(t, p.Held())
	require.Zero(t, p.Age)
	require.Equal(t, RepoLockPath(f.State), p.Path)
}

func TestInspectLockLive(t *testing.T) {
	f := lockSetup(t)
	writeLock(t, f.State, liveRecord())
	opts := lockOpts()
	opts.Alive = func(pid int) bool { return pid == 777 }

	p := InspectLock(f.State, opts)
	require.Equal(t, LockLive, p.State)
	require.True(t, p.Held())
	require.Equal(t, domain.RunID("20260904-090000-0001"), p.Record.Run)
	require.Equal(t, time.Hour, p.Age)
	require.False(t, p.Removed)
}

func TestInspectLockMine(t *testing.T) {
	f := lockSetup(t)
	rec := liveRecord()
	rec.PID = 4242 // this process, resuming in place
	writeLock(t, f.State, rec)

	p := InspectLock(f.State, lockOpts())
	require.Equal(t, LockMine, p.State)
	require.True(t, p.Held(), "our own lock is held — by us")
}

func TestInspectLockStaleWhenTheHolderIsGone(t *testing.T) {
	f := lockSetup(t)
	writeLock(t, f.State, liveRecord())

	p := InspectLock(f.State, lockOpts()) // Alive says no
	require.Equal(t, LockStale, p.State)
	require.False(t, p.Held())
}

// TestInspectLockStaleWhenThePIDWasReused is the pid-reuse hardening: the
// pid the lock names is alive, but it is not the process that took the lock
// — the kernel handed 777 to something else after the run was killed.
func TestInspectLockStaleWhenThePIDWasReused(t *testing.T) {
	f := lockSetup(t)
	rec := liveRecord()
	rec.Started = "111111"
	writeLock(t, f.State, rec)

	opts := lockOpts()
	opts.Alive = func(int) bool { return true }
	opts.StartOf = func(int) string { return "999999" }
	require.Equal(t, LockStale, InspectLock(f.State, opts).State)

	// The same incarnation is live, not stale.
	opts.StartOf = func(int) string { return "111111" }
	require.Equal(t, LockLive, InspectLock(f.State, opts).State)

	// A platform that cannot tell falls back to plain liveness (P6-01's
	// behaviour), rather than calling a live run stale.
	opts.StartOf = func(int) string { return "" }
	require.Equal(t, LockLive, InspectLock(f.State, opts).State)
}

// TestInspectLockWithoutAStartStampIsJustLiveness: a lock written by an sc
// that never stamped itself (or by an older build) is judged on the pid
// alone.
func TestInspectLockWithoutAStartStampIsJustLiveness(t *testing.T) {
	f := lockSetup(t)
	writeLock(t, f.State, liveRecord())
	opts := lockOpts()
	opts.Alive = func(int) bool { return true }
	opts.StartOf = func(int) string { return "whatever" }
	require.Equal(t, LockLive, InspectLock(f.State, opts).State)
}

func TestInspectLockElsewhere(t *testing.T) {
	f := lockSetup(t)
	rec := liveRecord()
	rec.Host = "other-host"
	writeLock(t, f.State, rec)

	opts := lockOpts()
	opts.Alive = func(int) bool { panic("a pid on another machine must never be probed here") }
	p := InspectLock(f.State, opts)
	require.Equal(t, LockElsewhere, p.State)
	require.True(t, p.Held())

	// A lock with no host recorded, or a machine that cannot name itself,
	// is not evidence of anything: fall through to the pid.
	rec.Host = ""
	writeLock(t, f.State, rec)
	opts.Alive = func(int) bool { return true }
	require.Equal(t, LockLive, InspectLock(f.State, opts).State)

	// With no host in the options, the probe asks the machine its own name,
	// so a lock written here is judged locally and one written elsewhere is
	// not.
	host, err := os.Hostname()
	require.NoError(t, err)
	rec.Host = host
	writeLock(t, f.State, rec)
	opts.Host, opts.Self = "", -1 // -1 never matches a real pid
	require.Equal(t, LockLive, InspectLock(f.State, opts).State)

	rec.Host = host + "-elsewhere"
	writeLock(t, f.State, rec)
	require.Equal(t, LockElsewhere, InspectLock(f.State, opts).State)
}

func TestInspectLockCorrupt(t *testing.T) {
	f := lockSetup(t)
	path := RepoLockPath(f.State)
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	p := InspectLock(f.State, lockOpts())
	require.Equal(t, LockCorrupt, p.State)
	require.Error(t, p.Err)
	require.True(t, p.Held(), "an unread lock is respected until it is removed")

	// A record that parses but names no process is just as useless.
	writeLock(t, f.State, LockRecord{LockHolder: LockHolder{Run: "r", PID: 0, TakenAt: lockEpoch}})
	p = InspectLock(f.State, lockOpts())
	require.Equal(t, LockCorrupt, p.State)
	require.ErrorContains(t, p.Err, "names no process")
}

// TestInspectLockAgeFallsBackToTheFile: a corrupt lock has no TakenAt to
// read, so its age comes from the file itself — which is what the grace
// window is measured against.
func TestInspectLockAgeFallsBackToTheFile(t *testing.T) {
	f := lockSetup(t)
	path := RepoLockPath(f.State)
	require.NoError(t, os.WriteFile(path, []byte("rubbish"), 0o600))
	require.NoError(t, os.Chtimes(path, lockEpoch.Add(-2*time.Hour), lockEpoch.Add(-2*time.Hour)))

	p := InspectLock(f.State, lockOpts())
	require.Equal(t, LockCorrupt, p.State)
	require.Equal(t, 2*time.Hour, p.Age)
}

// ------------------------------------------------------------ the preflight

// TestClaimRunLockRefusesASecondLiveRun is the headline of P9-04: a second
// `sc <repo>` is refused, told which run is going, and pointed at
// `sc status`. The lock is left exactly as it was.
func TestClaimRunLockRefusesASecondLiveRun(t *testing.T) {
	f := lockSetup(t)
	path := writeLock(t, f.State, liveRecord())
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	var n notices
	opts := lockOpts()
	opts.Alive = func(int) bool { return true }
	opts.Notice = n.sink()

	p, err := ClaimRunLock(f.State, opts)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrLocked)
	require.ErrorContains(t, err, "20260904-090000-0001")
	require.ErrorContains(t, err, "pid 777")
	require.ErrorContains(t, err, "sc status")
	require.ErrorContains(t, err, "2026-09-04T09:00:00Z")
	require.Equal(t, LockLive, p.State)
	require.False(t, p.Removed)
	require.Empty(t, n.lines, "refusing is not a notice")

	after, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, before, after, "the live run's lock is untouched")

	var locked *LockedError
	require.ErrorAs(t, err, &locked)
	require.Equal(t, LockLive, locked.State)
	require.Equal(t, domain.RunID("20260904-090000-0001"), locked.Record.Run)
	require.Equal(t, path, locked.Path)
}

// TestClaimRunLockRemovesAStaleLockWithANotice: the crashed-run case. The
// lock goes, the user is told why, and the next run is free to start.
func TestClaimRunLockRemovesAStaleLockWithANotice(t *testing.T) {
	f := lockSetup(t)
	path := writeLock(t, f.State, liveRecord())

	var n notices
	opts := lockOpts() // Alive says the holder is gone
	opts.Notice = n.sink()

	p, err := ClaimRunLock(f.State, opts)
	require.NoError(t, err)
	require.True(t, p.Removed)
	require.NoFileExists(t, path)
	require.Equal(t, []string{p.Notice}, n.lines)
	require.Contains(t, p.Notice, "removed a stale run lock")
	require.Contains(t, p.Notice, "20260904-090000-0001")
	require.Contains(t, p.Notice, "pid 777")
	require.Contains(t, p.Notice, "killed or crashed")

	// And now the repository is free.
	p, err = ClaimRunLock(f.State, opts)
	require.NoError(t, err)
	require.Equal(t, LockFree, p.State)
	require.False(t, p.Removed)
	require.Len(t, n.lines, 1, "nothing to say about a repository nobody is using")
}

// TestClaimRunLockRemovesAReusedPIDWithItsOwnNotice: the same removal, but
// the reason a user would otherwise find baffling — the pid is alive — is
// spelled out.
func TestClaimRunLockRemovesAReusedPIDWithItsOwnNotice(t *testing.T) {
	f := lockSetup(t)
	rec := liveRecord()
	rec.Started = "111111"
	path := writeLock(t, f.State, rec)

	var n notices
	opts := lockOpts()
	opts.Alive = func(int) bool { return true }
	opts.StartOf = func(int) string { return "999999" }
	opts.Notice = n.sink()

	p, err := ClaimRunLock(f.State, opts)
	require.NoError(t, err)
	require.True(t, p.Removed)
	require.NoFileExists(t, path)
	require.Contains(t, p.Notice, "belongs to another program now")
	require.Contains(t, p.Notice, "20260904-090000-0001")
	require.Equal(t, []string{p.Notice}, n.lines)
}

// TestClaimRunLockCorruptWaitsOutTheGrace: an unreadable lock might be a
// run that is writing it right now, so it is respected for one grace period
// and reaped after that. Either way the message says what to do.
func TestClaimRunLockCorruptWaitsOutTheGrace(t *testing.T) {
	f := lockSetup(t)
	path := RepoLockPath(f.State)
	require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))
	require.NoError(t, os.Chtimes(path, lockEpoch.Add(-time.Second), lockEpoch.Add(-time.Second)))

	var n notices
	opts := lockOpts()
	opts.Grace = time.Minute
	opts.Notice = n.sink()

	_, err := ClaimRunLock(f.State, opts)
	require.ErrorIs(t, err, ErrLocked)
	require.ErrorContains(t, err, "cannot be read")
	require.ErrorContains(t, err, path)
	require.FileExists(t, path)
	require.Empty(t, n.lines)

	// One grace period later it is debris, not a lock.
	require.NoError(t, os.Chtimes(path, lockEpoch.Add(-time.Hour), lockEpoch.Add(-time.Hour)))
	p, err := ClaimRunLock(f.State, opts)
	require.NoError(t, err)
	require.True(t, p.Removed)
	require.NoFileExists(t, path)
	require.Contains(t, p.Notice, "removed an unreadable run lock")
	require.Equal(t, []string{p.Notice}, n.lines)
}

// TestClaimRunLockElsewhereIsRespected: a state directory on a shared file
// system can hold a lock this machine cannot probe. Guessing would be
// worse than refusing, so the user is told where to look.
func TestClaimRunLockElsewhereIsRespected(t *testing.T) {
	f := lockSetup(t)
	rec := liveRecord()
	rec.Host = "other-host"
	path := writeLock(t, f.State, rec)

	opts := lockOpts()
	_, err := ClaimRunLock(f.State, opts)
	require.ErrorIs(t, err, ErrLocked)
	require.ErrorContains(t, err, "other-host")
	require.ErrorContains(t, err, "20260904-090000-0001")
	require.ErrorContains(t, err, "--force")
	require.FileExists(t, path)
}

// TestClaimRunLockForceTakesOverAndSaysSo: `--force` is the user's
// override, and it never happens quietly.
func TestClaimRunLockForceTakesOverAndSaysSo(t *testing.T) {
	for _, tc := range []struct {
		name string
		host string
	}{
		{"live here", "sc-host"},
		{"live elsewhere", "other-host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := lockSetup(t)
			rec := liveRecord()
			rec.Host = tc.host
			path := writeLock(t, f.State, rec)

			var n notices
			opts := lockOpts()
			opts.Force = true
			opts.Alive = func(int) bool { return true }
			opts.Notice = n.sink()

			p, err := ClaimRunLock(f.State, opts)
			require.NoError(t, err)
			require.True(t, p.Removed)
			require.NoFileExists(t, path)
			require.Contains(t, p.Notice, "--force")
			require.Contains(t, p.Notice, "20260904-090000-0001")
			require.Contains(t, p.Notice, tc.host)
			require.Equal(t, []string{p.Notice}, n.lines)
		})
	}
}

// TestClaimRunLockLeavesOurOwnLockAlone: a run resuming in-process already
// holds the lock, and must not have it pulled out from under it.
func TestClaimRunLockLeavesOurOwnLockAlone(t *testing.T) {
	f := lockSetup(t)
	rec := liveRecord()
	rec.PID = 4242
	path := writeLock(t, f.State, rec)

	var n notices
	opts := lockOpts()
	opts.Notice = n.sink()
	p, err := ClaimRunLock(f.State, opts)
	require.NoError(t, err)
	require.Equal(t, LockMine, p.State)
	require.False(t, p.Removed)
	require.FileExists(t, path)
	require.Empty(t, n.lines)
}

// TestClaimRunLockReportsARemovalItCannotDo: a state directory the user
// cannot write to must fail loudly, not pretend the lock is gone.
func TestClaimRunLockCannotRemove(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f := lockSetup(t)
	writeLock(t, f.State, liveRecord())
	require.NoError(t, os.Chmod(f.State, 0o500))
	t.Cleanup(func() { _ = os.Chmod(f.State, 0o700) })

	_, err := ClaimRunLock(f.State, lockOpts())
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrLocked)
	require.ErrorContains(t, err, "remove the stale run lock")
}

// ---------------------------------------------------------------- the stamp

// TestStampRunLockRecordsThisIncarnation: the stamp is written next to the
// pid, and P6-01's own reader still understands the file.
func TestStampRunLockRecordsThisIncarnation(t *testing.T) {
	f := lockSetup(t)
	path := writeLock(t, f.State, LockRecord{LockHolder: LockHolder{
		Run: "20260904-100000-abcd", PID: os.Getpid(), Host: "sc-host", TakenAt: lockEpoch,
	}})

	require.NoError(t, StampRunLock(f.State, LockOptions{}))

	rec, err := readLockRecord(path)
	require.NoError(t, err)
	require.Equal(t, ProcessStart(os.Getpid()), rec.Started)
	if runtime.GOOS == "linux" {
		require.NotEmpty(t, rec.Started)
	}
	// P6-01's reader, and therefore `sc status`, is unaffected.
	holder, err := readLock(path)
	require.NoError(t, err)
	require.Equal(t, domain.RunID("20260904-100000-abcd"), holder.Run)
	require.Equal(t, os.Getpid(), holder.PID)
	require.Equal(t, lockEpoch, holder.TakenAt.UTC())

	// And a stamped lock is recognised as live by a probe that can see the
	// same process.
	require.Equal(t, LockLive, InspectLock(f.State, LockOptions{Self: -1, Host: "sc-host"}).State)

	// Stamping twice changes nothing.
	require.NoError(t, StampRunLock(f.State, LockOptions{}))
	again, err := readLockRecord(path)
	require.NoError(t, err)
	require.Equal(t, rec, again)
}

func TestStampRunLockIsBestEffort(t *testing.T) {
	f := lockSetup(t)
	// No lock at all: nothing to stamp, and nothing to complain about.
	require.NoError(t, StampRunLock(f.State, LockOptions{}))

	// Somebody else's lock is left exactly as it was.
	path := writeLock(t, f.State, liveRecord())
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, StampRunLock(f.State, LockOptions{Self: 4242, StartOf: func(int) string { return "1" }}))
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)

	// A platform with no start stamp writes nothing.
	require.NoError(t, StampRunLock(f.State, LockOptions{Self: 777, StartOf: func(int) string { return "" }}))
	after, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)

	// A lock that cannot be parsed is an error, not a silent overwrite.
	require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))
	require.Error(t, StampRunLock(f.State, LockOptions{Self: 777, StartOf: func(int) string { return "1" }}))
}

// readLockRecord is the test-side reader for the extended record.
func readLockRecord(path string) (LockRecord, error) {
	data, err := os.ReadFile(path) //nolint:gosec // a temp dir this test wrote.
	if err != nil {
		return LockRecord{}, err
	}
	var rec LockRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return LockRecord{}, err
	}
	return rec, nil
}

// --------------------------------------------------------------- the policy

func TestRunLockPolicyStale(t *testing.T) {
	base := lockOpts()
	base.Alive = func(pid int) bool { return pid == 777 }

	for _, tc := range []struct {
		name  string
		opts  func(LockOptions) LockOptions
		hold  LockHolder
		stale bool
	}{
		{"our own pid", nil, LockHolder{PID: 4242, Host: "sc-host"}, true},
		{"no pid at all", nil, LockHolder{PID: 0}, true},
		{"a live holder", nil, LockHolder{PID: 777, Host: "sc-host"}, false},
		{"a dead holder", nil, LockHolder{PID: 778, Host: "sc-host"}, true},
		{"another machine", nil, LockHolder{PID: 778, Host: "other-host"}, false},
		{"no host recorded", nil, LockHolder{PID: 777}, false},
		{"forced", func(o LockOptions) LockOptions { o.Force = true; return o },
			LockHolder{PID: 777, Host: "sc-host"}, true},
		{"forced across machines", func(o LockOptions) LockOptions { o.Force = true; return o },
			LockHolder{PID: 777, Host: "other-host"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := base
			if tc.opts != nil {
				opts = tc.opts(opts)
			}
			require.Equal(t, tc.stale, NewLockPolicy(opts).Stale(tc.hold))
		})
	}
}

// TestRunLockPolicyStaleAgreesWithItsPreflight: Stale() is handed a
// LockHolder with no start stamp, so on its own it would call a reused pid
// live. The Check it just ran remembers the answer.
func TestRunLockPolicyStaleAgreesWithItsPreflight(t *testing.T) {
	f := lockSetup(t)
	rec := liveRecord()
	rec.Started = "111111"
	writeLock(t, f.State, rec)

	opts := lockOpts()
	opts.Alive = func(int) bool { return true }
	opts.StartOf = func(int) string { return "999999" }
	p := NewLockPolicy(opts)

	require.False(t, p.Stale(rec.LockHolder), "with nothing remembered, a live pid is a live pid")
	require.Equal(t, LockFree, p.Last().State, "nothing has been checked yet")

	probe, err := p.Check(f.State)
	require.NoError(t, err)
	require.True(t, probe.Removed)
	require.Equal(t, LockStale, p.Last().State)
	require.True(t, p.Stale(rec.LockHolder), "the preflight established that pid 777 was recycled")

	// A different holder is judged afresh: the remembered verdict is about
	// one pid and one run, not about locks in general.
	require.False(t, p.Stale(LockHolder{Run: "20260904-090000-0002", PID: 777, Host: "sc-host"}))
	require.False(t, p.Stale(LockHolder{Run: rec.Run, PID: 778, Host: "sc-host"}),
		"a pid this policy never probed falls back to the live check")
}

// TestRunLockPolicyCheckPropagatesTheRefusal: the policy is also the object
// `sc run` asks for the preflight, so the error is the same one.
func TestRunLockPolicyCheckPropagatesTheRefusal(t *testing.T) {
	f := lockSetup(t)
	writeLock(t, f.State, liveRecord())
	opts := lockOpts()
	opts.Alive = func(int) bool { return true }

	p := NewLockPolicy(opts)
	require.Equal(t, opts.Force, p.Options().Force)
	probe, err := p.Check(f.State)
	require.ErrorIs(t, err, ErrLocked)
	require.Equal(t, LockLive, probe.State)
	require.Equal(t, LockLive, p.Last().State)
	require.False(t, p.Stale(liveRecord().LockHolder))
}

// ------------------------------------------------------- the process probes

// TestProcessStartTellsIncarnationsApart uses real processes: this one,
// which has a stamp, and one that has already exited, which has none.
func TestProcessStartTellsIncarnationsApart(t *testing.T) {
	if runtime.GOOS != "linux" {
		require.Empty(t, ProcessStart(os.Getpid()))
		t.Skip("the start stamp is read from /proc")
	}
	self := ProcessStart(os.Getpid())
	require.NotEmpty(t, self)
	require.Equal(t, self, ProcessStart(os.Getpid()), "the same process always reads the same")
	_, err := strconv.ParseUint(self, 10, 64)
	require.NoError(t, err, "the stamp is a plain tick count")

	require.NotEmpty(t, ProcessStart(1), "pid 1 always exists")

	require.Empty(t, ProcessStart(0))
	require.Empty(t, ProcessStart(-1))
	require.Empty(t, ProcessStart(1<<30), "no such process")
	require.Empty(t, ProcessStart(deadPID(t)))
}

func TestProcessAlive(t *testing.T) {
	require.True(t, ProcessAlive(os.Getpid()))
	require.False(t, ProcessAlive(1<<30))
	require.False(t, ProcessAlive(0))
	require.False(t, ProcessAlive(deadPID(t)))
}

// TestClaimRunLockAgainstARealDeadProcess is the crash case with no probes
// injected at all: a child that really ran, really exited, and left its pid
// in the lock.
func TestClaimRunLockAgainstARealDeadProcess(t *testing.T) {
	f := lockSetup(t)
	pid := deadPID(t)
	path := writeLock(t, f.State, LockRecord{LockHolder: LockHolder{
		Run: "20260904-090000-0003", PID: pid, TakenAt: lockEpoch,
	}})

	var n notices
	p, err := ClaimRunLock(f.State, LockOptions{Notice: n.sink()})
	require.NoError(t, err)
	require.True(t, p.Removed)
	require.NoFileExists(t, path)
	require.Contains(t, p.Notice, strconv.Itoa(pid))
	require.Len(t, n.lines, 1)
}

// deadPID runs a trivial child to completion and returns its pid, which is
// then a pid that certainly existed and certainly does not now. (The kernel
// could in principle recycle it, but not within one test process's lifetime
// on any system this runs on.)
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	require.NoError(t, cmd.Run())
	return cmd.Process.Pid
}

// ------------------------------------------------------------ the whole way

// TestRunRefusesToStartBesideALiveRun drives the interlock through the real
// lifecycle: one run holds the repository, a second one built with the same
// policy refuses to start and finishes failed, and once the first is done
// the second kind of run starts cleanly.
func TestRunRefusesToStartBesideALiveRun(t *testing.T) {
	f := lockSetup(t)
	bus := events.NewBus()
	t.Cleanup(bus.Close)

	policy := NewLockPolicy(LockOptions{}) // real pid, real host, real probes
	opts := Options{Bus: bus, Logger: lockLogger(), Now: func() time.Time { return lockEpoch },
		StopGrace: time.Second, LockPolicy: policy}

	first := lockRun(t, f.Repo, "20260904-100000-0001", opts)
	require.NoError(t, first.Start(context.Background()))
	require.FileExists(t, RepoLockPath(f.State))
	require.NoError(t, StampRunLock(f.State, LockOptions{}))

	// The preflight a second `sc <repo>` runs, before it builds anything.
	// Our own pid holds it, so the policy has to see a *different* process.
	var n notices
	other := LockOptions{Self: -1, Notice: n.sink(), Alive: func(int) bool { return true }}
	_, err := ClaimRunLock(f.State, other)
	require.ErrorIs(t, err, ErrLocked)
	require.ErrorContains(t, err, "20260904-100000-0001")
	require.ErrorContains(t, err, "sc status")
	require.Empty(t, n.lines)

	// And the run itself refuses, through the same policy.
	second := lockRun(t, f.Repo, "20260904-100001-0002", Options{
		Bus: bus, Logger: lockLogger(), Now: opts.Now, StopGrace: time.Second,
		LockPolicy: NewLockPolicy(LockOptions{Self: -1, Alive: func(int) bool { return true }}),
	})
	require.ErrorIs(t, second.Start(context.Background()), ErrLocked)
	out, waitErr := second.Wait()
	require.ErrorIs(t, waitErr, ErrLocked)
	require.Equal(t, events.FinishFailed, out.Reason)

	require.NoError(t, first.Stop("done"))
	_, err = first.Wait()
	require.NoError(t, err)
	require.NoFileExists(t, RepoLockPath(f.State), "a finished run releases the repository")

	third := lockRun(t, f.Repo, "20260904-100002-0003", opts)
	require.NoError(t, third.Start(context.Background()))
	require.NoError(t, third.Stop("done"))
	_, err = third.Wait()
	require.NoError(t, err)
}

// TestRunTakesOverAStaleLockThroughThePolicy: the crashed-run path, end to
// end — the lock a killed sc left behind does not stop the next run.
func TestRunTakesOverAStaleLockThroughThePolicy(t *testing.T) {
	f := lockSetup(t)
	pid := deadPID(t)
	writeLock(t, f.State, LockRecord{LockHolder: LockHolder{
		Run: "20260904-090000-0009", PID: pid, TakenAt: lockEpoch.Add(-time.Hour),
	}})

	var n notices
	policy := NewLockPolicy(LockOptions{Notice: n.sink()})
	probe, err := policy.Check(f.State)
	require.NoError(t, err)
	require.True(t, probe.Removed)
	require.Len(t, n.lines, 1)

	bus := events.NewBus()
	t.Cleanup(bus.Close)
	r := lockRun(t, f.Repo, "20260904-100000-000a", Options{
		Bus: bus, Logger: lockLogger(), Now: func() time.Time { return lockEpoch },
		StopGrace: time.Second, LockPolicy: policy,
	})
	require.NoError(t, r.Start(context.Background()))

	holder, err := readLock(RepoLockPath(f.State))
	require.NoError(t, err)
	require.Equal(t, domain.RunID("20260904-100000-000a"), holder.Run)
	require.Equal(t, os.Getpid(), holder.PID)

	require.NoError(t, r.Stop("done"))
	_, err = r.Wait()
	require.NoError(t, err)
}

// lockLogger keeps the run's own lines out of the test output.
func lockLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// lockRun builds a run for the fixture repository.
func lockRun(t *testing.T, repo string, id domain.RunID, opts Options) *Run {
	t.Helper()
	cfg := config.Defaults()
	cfg.ActiveCartridge = "claude"
	cfg.Cartridges = map[string]config.Cartridge{"claude": {Model: "opus"}}
	eff := &config.Effective{Config: cfg, Sources: map[string]config.Source{}}
	rc, err := config.NewRunConfig(repo, eff, config.RunSettings{ID: id, StartedAt: lockEpoch})
	require.NoError(t, err)
	r, err := New(rc, opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// ------------------------------------------- taking the lock (P9-04 round 4)

// The gaps a review found in the first round of P9-04, each pinned by a
// test: [acquireLock] used to bypass the policy whenever the existing lock
// could not be read (deleting it with no grace period and no notice, which
// is exactly what [DefaultLockGrace] exists to prevent), it published its
// own lock as a zero-byte file first and wrote the record afterwards
// (giving a second sc that very window to read), release() removed a lock
// it could not parse even when somebody else owned it, and the liveness
// probe read EPERM — "it exists, it is just not yours" — as "it is gone".

// lockRunDir is a run directory under the fixture's state directory; its
// LockPath is the fixture's one run lock.
func lockRunDir(t *testing.T, f lockFixture, id string) string {
	t.Helper()
	dir := config.RunDir(f.Repo, id)
	require.Equal(t, RepoLockPath(f.State), LockPath(dir))
	return dir
}

// TestAcquireLockRespectsAHalfWrittenLock is the race the zero-byte window
// used to open: a lock file that exists but cannot be read yet is another
// sc starting *right now*, so Run.Start refuses it for one grace period
// instead of deleting it and firing at the same repository.
func TestAcquireLockRespectsAHalfWrittenLock(t *testing.T) {
	f := lockSetup(t)
	path := RepoLockPath(f.State)
	require.NoError(t, os.WriteFile(path, nil, 0o600)) // 0 bytes: mid-write.

	policy := NewLockPolicy(lockOpts())
	_, err := acquireLock(lockRunDir(t, f, "20260904-100000-000b"), "20260904-100000-000b", policy, lockEpoch)
	require.ErrorIs(t, err, ErrLocked)
	require.ErrorContains(t, err, "cannot be read")
	require.ErrorContains(t, err, path)
	require.FileExists(t, path, "the other sc's lock is left where it is")

	data, readErr := os.ReadFile(path) //nolint:gosec // the test's own temp state directory.
	require.NoError(t, readErr)
	require.Empty(t, data, "nothing wrote over it either")
}

// TestAcquireLockClearsDebrisOnceItIsOld: the same unreadable file, older
// than the grace period, is debris — it must not wedge the repository.
func TestAcquireLockClearsDebrisOnceItIsOld(t *testing.T) {
	f := lockSetup(t)
	path := RepoLockPath(f.State)
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))
	require.NoError(t, os.Chtimes(path, lockEpoch.Add(-time.Hour), lockEpoch.Add(-time.Hour)))

	var n notices
	opts := lockOpts()
	opts.Notice = n.sink()
	lock, err := acquireLock(lockRunDir(t, f, "20260904-100000-000c"), "20260904-100000-000c", NewLockPolicy(opts), lockEpoch)
	require.NoError(t, err)
	require.NotNil(t, lock)
	require.Len(t, n.lines, 1)
	require.Contains(t, n.lines[0], "removed an unreadable run lock")

	holder, err := readLock(path)
	require.NoError(t, err)
	require.Equal(t, domain.RunID("20260904-100000-000c"), holder.Run)
	require.NoError(t, lock.release())
	require.NoFileExists(t, path)
}

// TestAcquireLockRefusesAPidZeroLock: a record that parses but names no
// process is as unusable as one that does not parse, and gets the same
// grace — a lock whose pid field has not been filled in yet is a run
// starting this instant.
func TestAcquireLockRefusesAPidZeroLock(t *testing.T) {
	f := lockSetup(t)
	path := writeLock(t, f.State, LockRecord{LockHolder: LockHolder{Run: "20260904-090000-000d", TakenAt: lockEpoch}})

	_, err := acquireLock(lockRunDir(t, f, "20260904-100000-000e"), "20260904-100000-000e", NewLockPolicy(lockOpts()), lockEpoch)
	require.ErrorIs(t, err, ErrLocked)
	require.ErrorContains(t, err, "names no process")
	require.FileExists(t, path)

	// --force is the way past it, as it is for every other verdict.
	forced := lockOpts()
	forced.Force = true
	lock, err := acquireLock(lockRunDir(t, f, "20260904-100000-000f"), "20260904-100000-000f", NewLockPolicy(forced), lockEpoch)
	require.NoError(t, err)
	require.NoError(t, lock.release())
}

// TestAcquireLockKeepsP6PolicyForUnreadableLocks: a LockPolicy with no
// opinion about unreadable locks (P6-01's [DefaultLockPolicy], and anything
// else that only implements Stale) keeps the old rule, so this hardening
// changes nothing for an embedder that has not asked for it.
func TestAcquireLockKeepsP6PolicyForUnreadableLocks(t *testing.T) {
	f := lockSetup(t)
	path := RepoLockPath(f.State)
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	lock, err := acquireLock(lockRunDir(t, f, "20260904-100000-0010"), "20260904-100000-0010", DefaultLockPolicy{}, lockEpoch)
	require.NoError(t, err)
	require.NoError(t, lock.release())
}

// TestAcquireLockPublishesTheWholeRecordAtOnce fires many runs at one
// repository at the same moment: exactly one may take the lock, and the
// file must never be readable in a half-written state, because a reader
// that saw a zero-byte lock used to delete it and start a second run.
func TestAcquireLockPublishesTheWholeRecordAtOnce(t *testing.T) {
	f := lockSetup(t)
	path := RepoLockPath(f.State)
	require.NoError(t, os.MkdirAll(f.State, 0o700))

	// Every contender sees the others as live processes it is not.
	opts := LockOptions{Self: -1, Alive: func(int) bool { return true }, StartOf: func(int) string { return "" }}

	const contenders = 8
	var (
		mu     sync.Mutex
		won    []*lockFile
		errs   []error
		stop   = make(chan struct{})
		start  = make(chan struct{})
		reader sync.WaitGroup
		wg     sync.WaitGroup
	)
	reader.Add(1)
	go func() {
		defer reader.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := os.ReadFile(path) //nolint:gosec // the test's own temp state directory.
			if err != nil {
				continue // not taken yet, or gone again: both are fine.
			}
			var h LockHolder
			if err := json.Unmarshal(data, &h); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("a reader saw a half-written lock (%d bytes): %w", len(data), err))
				mu.Unlock()
				return
			}
		}
	}()

	for i := 0; i < contenders; i++ {
		id := domain.RunID(fmt.Sprintf("20260904-1000%02d-00%02d", i, i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			lock, err := acquireLock(config.RunDir(f.Repo, string(id)), id, NewLockPolicy(opts), lockEpoch)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won = append(won, lock)
			case errors.Is(err, ErrLocked):
			default:
				errs = append(errs, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(stop)
	reader.Wait()

	require.Empty(t, errs)
	require.Len(t, won, 1, "exactly one run may hold the repository")
	holder, err := readLock(path)
	require.NoError(t, err)
	require.Equal(t, won[0].holder.Run, holder.Run)
	require.NoError(t, won[0].release())
	require.NoFileExists(t, path)
}

// TestReleaseLeavesALockItCannotReadAlone: a lock file that cannot be
// parsed cannot be proved to be ours, so releasing must not delete it —
// it may be the lock of an sc that is writing it this instant.
func TestReleaseLeavesALockItCannotReadAlone(t *testing.T) {
	f := lockSetup(t)
	path := RepoLockPath(f.State)
	lock, err := acquireLock(lockRunDir(t, f, "20260904-100000-0011"), "20260904-100000-0011", NewLockPolicy(lockOpts()), lockEpoch)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(path, nil, 0o600)) // somebody else's, mid-write.
	require.NoError(t, lock.release())
	require.FileExists(t, path, "an unreadable lock is not ours to remove")

	// A lock another run took over is left alone too (P6-01's rule).
	writeLock(t, f.State, liveRecord())
	require.NoError(t, lock.release())
	require.FileExists(t, path)
}

// ------------------------------------------------- the liveness probe again

// TestAliveFromSignal reads the kernel's answers the way the lock has to:
// only "no such process" means the holder is gone. EPERM means it exists
// and belongs to another user (`sudo -E sc /repo` in the other terminal),
// which used to be read as death — the one direction that lets two runs
// fire at one repository.
func TestAliveFromSignal(t *testing.T) {
	require.True(t, aliveFromSignal(nil), "the signal was accepted")
	require.True(t, aliveFromSignal(syscall.EPERM), "another user's live process")
	require.True(t, aliveFromSignal(&os.SyscallError{Syscall: "kill", Err: syscall.EPERM}), "wrapped the same way")
	require.False(t, aliveFromSignal(syscall.ESRCH), "no such process")
	require.False(t, aliveFromSignal(os.ErrProcessDone), "a child this process already waited for")
	require.True(t, aliveFromSignal(errors.New("something new")), "an answer we cannot read is not proof of death")
}

// TestProcessAliveOnAProcessWeMayNotSignal uses pid 1, which always exists
// and (unless the tests run as root) cannot be signalled by this user.
func TestProcessAliveOnAProcessWeMayNotSignal(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("pid 1 is only certainly there on Linux")
	}
	require.True(t, ProcessAlive(1), "pid 1 always exists")

	if os.Geteuid() == 0 {
		t.Skip("as root every signal is permitted, so EPERM cannot be shown here")
	}
	p, err := os.FindProcess(1)
	require.NoError(t, err)
	err = p.Signal(syscall.Signal(0))
	require.ErrorIs(t, err, syscall.EPERM, "the kernel says it exists but is not ours")
	require.True(t, aliveFromSignal(err))
}

// TestInspectLockRespectsAnotherUsersRun is the same thing through the
// verdicts: a lock naming a process this user may not signal is live, not
// stale, so the second sc is refused instead of reaping it.
func TestInspectLockRespectsAnotherUsersRun(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() == 0 {
		t.Skip("needs a live process this user may not signal")
	}
	f := lockSetup(t)
	writeLock(t, f.State, LockRecord{LockHolder: LockHolder{
		Run: "20260904-090000-0012", PID: 1, TakenAt: lockEpoch.Add(-time.Minute),
	}})

	// Real liveness probe; only "am I that process" is answered for us.
	p := InspectLock(f.State, LockOptions{Self: -1, Now: func() time.Time { return lockEpoch }})
	require.Equal(t, LockLive, p.State)
	require.True(t, p.Held())

	_, err := ClaimRunLock(f.State, LockOptions{Self: -1, Now: func() time.Time { return lockEpoch }})
	require.ErrorIs(t, err, ErrLocked)
	require.FileExists(t, RepoLockPath(f.State))
}

// TestLockedErrorOffersRemediesThatExist: the refusal used to say "stop
// it", and there is no `sc stop` (D-18 names one, but it is not built), so
// it now names the two things a user can actually do.
func TestLockedErrorOffersRemediesThatExist(t *testing.T) {
	err := &LockedError{State: LockLive, Record: liveRecord()}
	msg := err.Error()
	require.Contains(t, msg, "sc status")
	require.Contains(t, msg, "Ctrl-C")
	require.Contains(t, msg, "kill 777")
	require.NotContains(t, msg, "sc stop")
}

// TestRunRefusesToStartBesideAHalfWrittenLock drives the same rule through
// the lifecycle: Run.Start, not just the preflight, respects a lock it
// cannot read yet.
func TestRunRefusesToStartBesideAHalfWrittenLock(t *testing.T) {
	f := lockSetup(t)
	path := RepoLockPath(f.State)
	require.NoError(t, os.MkdirAll(f.State, 0o700))
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	bus := events.NewBus()
	t.Cleanup(bus.Close)
	r := lockRun(t, f.Repo, "20260904-100000-0013", Options{
		Bus: bus, Logger: lockLogger(), Now: func() time.Time { return lockEpoch },
		StopGrace: time.Second, LockPolicy: NewLockPolicy(LockOptions{}),
	})
	err := r.Start(context.Background())
	require.ErrorIs(t, err, ErrLocked)
	require.ErrorContains(t, err, "cannot be read")
	out, waitErr := r.Wait()
	require.ErrorIs(t, waitErr, ErrLocked)
	require.Equal(t, events.FinishFailed, out.Reason)
	require.FileExists(t, path, "the refused run leaves the other one's lock alone")
}
