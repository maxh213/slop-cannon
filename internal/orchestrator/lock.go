package orchestrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"slop-cannon/internal/config"
)

// One run per repository (P9-04).
//
// A repository can only be fired at once: two runs would claim the same
// beads, fight over the same worktrees and deliver the same branches. The
// interlock is one file, <state-dir>/run.lock (D-23 — never inside the
// user's repository), holding the run id, the pid of the sc that took it,
// the machine it runs on and when it was taken.
//
// P6-01 writes and releases that file ([acquireLock], [Run.Close]) and
// leaves the *decision* to a [LockPolicy]. This file is that decision, and
// the preflight a second `sc <repo>` runs before it builds anything:
//
//   - a lock held by a process that is still running refuses the second run
//     with the live run's id and a pointer at `sc status` ([LockedError]);
//   - a lock left behind by a killed run — its pid is gone, or its pid has
//     since been handed to some other program — is removed with a notice,
//     so a crashed run never wedges the repository ([ClaimRunLock]);
//   - a lock written on another machine (a state directory on a shared file
//     system) is respected, because this machine cannot probe that pid;
//     `--force` is the way past it.
//
// Pid reuse is the reason for [LockRecord.Started]: a bare "does pid 4242
// exist" check says yes just as loudly when 4242 has been recycled into an
// unrelated program as when it is the sc that took the lock. [StampRunLock]
// records the holder's process start stamp next to the pid, and a probe
// that finds a live pid whose start stamp disagrees treats the lock as
// stale. The stamp is best-effort (Linux /proc today): with no stamp on
// either side the check degrades to plain liveness, which is what P6-01
// already did.

// DefaultLockGrace is how long an *unreadable* run lock is respected before
// it is removed as rubbish. A lock this sc cannot parse is either a
// half-written file from a run that is starting right now (a state
// directory can live on a shared file system, where a write is not atomic
// across hosts) or debris; waiting one grace period tells them apart
// without wedging the repository forever.
const DefaultLockGrace = 30 * time.Second

// LockState is what a probe of the repository's run lock concluded.
type LockState int

const (
	// LockFree means there is no lock file, so nothing is running.
	LockFree LockState = iota
	// LockMine means the lock names this very process, which is what a
	// run being resumed in-process finds.
	LockMine
	// LockLive means the lock is held by another process that still
	// exists on this machine. This is the case that refuses a second run.
	LockLive
	// LockStale means the holder is gone — its pid no longer exists, or
	// the pid has been reused by another program. Safe to take over.
	LockStale
	// LockElsewhere means the lock was written on another machine, whose
	// processes this one cannot probe. Respected unless forced.
	LockElsewhere
	// LockCorrupt means the lock file could not be read or parsed.
	LockCorrupt
)

// String names the state for a log line.
func (s LockState) String() string {
	switch s {
	case LockFree:
		return "free"
	case LockMine:
		return "mine"
	case LockLive:
		return "live"
	case LockStale:
		return "stale"
	case LockElsewhere:
		return "elsewhere"
	case LockCorrupt:
		return "corrupt"
	default:
		return "unknown"
	}
}

// LockRecord is what the run lock holds: the [LockHolder] fields P6-01
// writes, plus the holder process's start stamp (P9-04). The embedded
// struct is inlined by encoding/json, so a record written here reads back
// through P6-01's own reader — and an older lock, written without a stamp,
// reads back here with an empty [LockRecord.Started].
type LockRecord struct {
	LockHolder
	// Started is an opaque token identifying the *incarnation* of the
	// holder process (its start time in kernel ticks on Linux). Empty when
	// this platform cannot tell, or when the lock predates [StampRunLock].
	Started string `json:"started,omitempty"`
}

// LockOptions tunes a probe. The zero value is production: this process,
// this hostname, the real clock, the real liveness probe and
// [DefaultLockGrace], with notices discarded and nothing forced.
type LockOptions struct {
	// Self is the pid a lock has to name to count as ours; 0 means
	// os.Getpid().
	Self int
	// Host is this machine's name; "" means os.Hostname(). A lock naming a
	// different host is [LockElsewhere].
	Host string
	// Now is the clock, for the grace window; nil means time.Now.
	Now func() time.Time
	// Alive reports whether a process with that id exists on this machine;
	// nil means the real signal-0 probe.
	Alive func(pid int) bool
	// StartOf returns a process's start stamp, or "" when it cannot be
	// told; nil means the real one. Tests replace it to exercise pid reuse
	// without waiting for the kernel to recycle a pid.
	StartOf func(pid int) string
	// Grace is how long an unreadable lock is respected; 0 means
	// [DefaultLockGrace].
	Grace time.Duration
	// Force takes the lock even from a run that is still going (`--force`).
	// It is the user's override, never sc's own decision.
	Force bool
	// Notice receives one line whenever a lock is removed — a stale one, or
	// a live one under Force. nil discards them; `sc run` prints them.
	Notice func(string)
}

// self returns the pid a lock has to name to be ours.
func (o LockOptions) self() int {
	if o.Self != 0 {
		return o.Self
	}
	return os.Getpid()
}

// host returns this machine's name, or "" when it cannot be told (in which
// case no lock is ever [LockElsewhere]: an unknowable host is not evidence).
func (o LockOptions) host() string {
	if o.Host != "" {
		return o.Host
	}
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// now reads the clock.
func (o LockOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// alive probes a pid.
func (o LockOptions) alive(pid int) bool {
	if o.Alive != nil {
		return o.Alive(pid)
	}
	return processAlive(pid)
}

// startOf reads a process's start stamp.
func (o LockOptions) startOf(pid int) string {
	if o.StartOf != nil {
		return o.StartOf(pid)
	}
	return ProcessStart(pid)
}

// grace is the window an unreadable lock is respected for.
func (o LockOptions) grace() time.Duration {
	if o.Grace > 0 {
		return o.Grace
	}
	return DefaultLockGrace
}

// notify hands a line to the caller's notice sink.
func (o LockOptions) notify(line string) {
	if o.Notice != nil && line != "" {
		o.Notice(line)
	}
}

// LockProbe is the result of looking at the repository's run lock.
type LockProbe struct {
	// Path is the lock file that was looked at, whether or not it exists.
	Path string
	// State is what the probe concluded.
	State LockState
	// Record is what the file said; the zero value when State is
	// [LockFree] or [LockCorrupt].
	Record LockRecord
	// Age is how long ago the lock was taken (from the file's timestamp
	// when the record itself could not be parsed); 0 when there is no lock.
	Age time.Duration
	// Removed is true when [ClaimRunLock] deleted the lock file.
	Removed bool
	// Notice is the line explaining a removal, or "" when nothing was
	// removed. It is also handed to [LockOptions.Notice].
	Notice string
	// Err is the read or parse error behind a [LockCorrupt] verdict.
	Err error
}

// Held reports whether something that has to be respected holds the lock.
func (p LockProbe) Held() bool {
	switch p.State {
	case LockLive, LockElsewhere, LockMine:
		return true
	case LockCorrupt:
		return !p.Removed
	case LockFree, LockStale:
		return false
	default:
		return false
	}
}

// RepoLockPath returns the run lock of the repository whose *state*
// directory is stateDir: <state-dir>/run.lock (D-23). [LockPath] is the
// same file reached from a run directory; this is the form a preflight has,
// which knows the repository but not yet the run.
func RepoLockPath(stateDir string) string {
	return filepath.Join(filepath.Clean(stateDir), LockFileName)
}

// LockedError is why a second `sc <repo>` will not start: another run holds
// the repository's lock. It wraps [ErrLocked], so callers keep matching on
// that, and its message names the live run and points at `sc status`.
type LockedError struct {
	// Path is the lock file.
	Path string
	// State is the probe's verdict ([LockLive], [LockElsewhere] or
	// [LockCorrupt]).
	State LockState
	// Record is what the lock said; the zero value when it was unreadable.
	Record LockRecord
	// Age is how long ago the lock was taken.
	Age time.Duration

	// cause is the read or parse error behind a LockCorrupt verdict.
	cause error
}

// Error explains what is in the way and what to do about it.
func (e *LockedError) Error() string {
	switch e.State {
	case LockElsewhere:
		return fmt.Sprintf("%s: run %s is going on %s (pid %d, since %s), and this machine cannot tell whether it is still alive; "+
			"check it there, or start again with --force once you are sure it stopped",
			ErrLocked, e.Record.Run, e.Record.Host, e.Record.PID, lockStamp(e.Record.TakenAt))
	case LockCorrupt:
		return fmt.Sprintf("%s: the run lock %s cannot be read (%v); if no run is going, delete it and try again",
			ErrLocked, e.Path, e.Err())
	default:
		return fmt.Sprintf("%s: run %s is already going in this repository (pid %d, since %s); "+
			"run `sc status` to see it, then stop it or wait for it to finish",
			ErrLocked, e.Record.Run, e.Record.PID, lockStamp(e.Record.TakenAt))
	}
}

// Err returns the underlying read or parse error of a corrupt lock, or nil.
func (e *LockedError) Err() error { return e.cause }

// Unwrap makes errors.Is(err, ErrLocked) true.
func (e *LockedError) Unwrap() error { return ErrLocked }

// lockStamp renders a lock timestamp, or "an unknown time" for a zero one.
func lockStamp(t time.Time) string {
	if t.IsZero() {
		return "an unknown time"
	}
	return t.UTC().Format(time.RFC3339)
}

// InspectLock reads the repository's run lock and says what it means,
// changing nothing. It never returns an error: a missing lock is the normal
// case, and an unreadable one is a verdict ([LockCorrupt]), not a failure.
func InspectLock(stateDir string, opts LockOptions) LockProbe {
	path := RepoLockPath(stateDir)
	p := LockProbe{Path: path}

	data, err := os.ReadFile(path) //nolint:gosec // the path is sc's own state directory (D-23).
	if errors.Is(err, os.ErrNotExist) {
		return p
	}
	if err != nil {
		p.State, p.Err, p.Age = LockCorrupt, err, lockFileAge(path, opts)
		return p
	}
	var rec LockRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		p.State, p.Err, p.Age = LockCorrupt, err, lockFileAge(path, opts)
		return p
	}
	p.Record = rec
	if !rec.TakenAt.IsZero() {
		p.Age = opts.now().Sub(rec.TakenAt)
	} else {
		p.Age = lockFileAge(path, opts)
	}
	if rec.PID <= 0 {
		p.State = LockCorrupt
		p.Err = fmt.Errorf("the run lock names no process (pid %d)", rec.PID)
		return p
	}
	if h := opts.host(); h != "" && rec.Host != "" && rec.Host != h {
		p.State = LockElsewhere
		return p
	}
	if rec.PID == opts.self() {
		p.State = LockMine
		return p
	}
	if !opts.alive(rec.PID) {
		p.State = LockStale
		return p
	}
	// The pid exists — but is it the same *process*? A pid the kernel has
	// since handed to another program answers a bare liveness probe just as
	// loudly as the sc that took the lock.
	if rec.Started != "" {
		if now := opts.startOf(rec.PID); now != "" && now != rec.Started {
			p.State = LockStale
			return p
		}
	}
	p.State = LockLive
	return p
}

// lockFileAge falls back to the file's modification time when the record
// itself carries no usable timestamp.
func lockFileAge(path string, opts LockOptions) time.Duration {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return opts.now().Sub(info.ModTime())
}

// ClaimRunLock is the preflight `sc <repo>` runs before it builds a run: it
// clears a lock nothing is using any more and refuses when something is.
//
// The lock is *not* taken here — [Run.Start] takes it, under the same
// policy ([NewLockPolicy]), which is what closes the race between two sc
// processes that pass this preflight at the same moment. What this does is
// give the user the verdict, and the sentence explaining it, before a run
// directory, a bd client or an agent exists.
//
// A stale lock (its holder is gone, or its pid now belongs to another
// program) is removed and reported through [LockOptions.Notice]; so is an
// unreadable one older than [LockOptions.Grace]. A live lock returns a
// *[LockedError], which wraps [ErrLocked] and names the run to look at with
// `sc status`. [LockOptions.Force] takes over even a live one, with a
// notice saying so.
func ClaimRunLock(stateDir string, opts LockOptions) (LockProbe, error) {
	p := InspectLock(stateDir, opts)
	switch p.State {
	case LockFree, LockMine:
		return p, nil
	case LockStale:
		return lockRemove(p, opts, lockStaleNotice(p, opts))
	case LockCorrupt:
		if p.Age >= opts.grace() || opts.Force {
			return lockRemove(p, opts, fmt.Sprintf("removed an unreadable run lock (%s): %v", p.Path, p.Err))
		}
		return p, &LockedError{Path: p.Path, State: p.State, Age: p.Age, cause: p.Err}
	case LockElsewhere, LockLive:
		if opts.Force {
			return lockRemove(p, opts, fmt.Sprintf("--force: took the run lock from run %s (pid %d on %s), which may still be going",
				p.Record.Run, p.Record.PID, lockHostName(p.Record.Host)))
		}
		return p, &LockedError{Path: p.Path, State: p.State, Record: p.Record, Age: p.Age}
	default:
		return p, nil
	}
}

// lockStaleNotice explains why a lock was reaped: its holder is gone, or
// its pid has been recycled into an unrelated program.
func lockStaleNotice(p LockProbe, opts LockOptions) string {
	if p.Record.Started != "" && opts.alive(p.Record.PID) {
		return fmt.Sprintf("removed a stale run lock: pid %d belongs to another program now, so run %s is gone",
			p.Record.PID, p.Record.Run)
	}
	return fmt.Sprintf("removed a stale run lock: run %s (pid %d) is gone — it was killed or crashed",
		p.Record.Run, p.Record.PID)
}

// lockHostName renders a possibly-empty hostname.
func lockHostName(h string) string {
	if h == "" {
		return "an unnamed host"
	}
	return h
}

// lockRemove deletes the lock file and records the notice on the probe.
func lockRemove(p LockProbe, opts LockOptions, notice string) (LockProbe, error) {
	if err := os.Remove(p.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return p, fmt.Errorf("orchestrator: remove the stale run lock %s: %w", p.Path, err)
	}
	p.Removed, p.Notice = true, notice
	opts.notify(notice)
	return p, nil
}

// StampRunLock records this process's start stamp in the run lock it holds,
// so a later sc can tell the lock's owner from an unrelated program that
// has since been given the same pid. Call it once, after [Run.Start] has
// taken the lock.
//
// It is best-effort by design: a lock held by somebody else is left alone,
// a missing lock is not an error, and a platform that cannot tell a
// process's start time changes nothing.
func StampRunLock(stateDir string, opts LockOptions) error {
	path := RepoLockPath(stateDir)
	self := opts.self()
	stamp := opts.startOf(self)
	if stamp == "" {
		return nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // the path is sc's own state directory (D-23).
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("orchestrator: read the run lock %s: %w", path, err)
	}
	var rec LockRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return fmt.Errorf("orchestrator: parse the run lock %s: %w", path, err)
	}
	if rec.PID != self || rec.Started == stamp {
		return nil // somebody else's lock, or already stamped.
	}
	rec.Started = stamp
	out, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("orchestrator: encode the run lock: %w", err)
	}
	if err := config.WriteAtomic(path, append(out, '\n'), runFilePerm); err != nil {
		return fmt.Errorf("orchestrator: write the run lock %s: %w", path, err)
	}
	return nil
}

// RunLockPolicy is P9-04's [LockPolicy]: the hardened rule [Run.Start]
// applies when it finds the lock already taken, and the same rule
// [ClaimRunLock] applies in the preflight before it.
//
// It is deliberately the *same object* in both places: Check remembers what
// it concluded about the lock it saw, so the verdict Start reaches through
// the narrow [LockPolicy.Stale] hook (which is handed a [LockHolder] and no
// file path, and so cannot redo the start-stamp check itself) agrees with
// the sentence the user was already shown.
//
// The zero value is not usable; build one with [NewLockPolicy].
type RunLockPolicy struct {
	opts LockOptions

	mu   sync.Mutex
	last LockProbe
}

// NewLockPolicy builds the run-lock policy from opts (the zero value is
// production; see [LockOptions]).
func NewLockPolicy(opts LockOptions) *RunLockPolicy {
	return &RunLockPolicy{opts: opts}
}

// Options returns the policy's options, resolved defaults included for the
// fields a caller set explicitly.
func (p *RunLockPolicy) Options() LockOptions { return p.opts }

// Check runs the [ClaimRunLock] preflight for the repository whose state
// directory is stateDir and remembers the verdict for [RunLockPolicy.Stale].
func (p *RunLockPolicy) Check(stateDir string) (LockProbe, error) {
	probe, err := ClaimRunLock(stateDir, p.opts)
	p.mu.Lock()
	p.last = probe
	p.mu.Unlock()
	return probe, err
}

// Last returns the probe of the most recent [RunLockPolicy.Check]; the zero
// value when there has not been one.
func (p *RunLockPolicy) Last() LockProbe {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

// Stale implements [LockPolicy]. It is the race-closing half of the
// interlock: between the preflight and [Run.Start] another sc may have
// taken the lock, so the rule is re-applied against whatever is there now.
//
// A lock this very process holds, or one that names no process, is ours to
// take. A lock on another machine is respected, because the pid cannot be
// probed here. Otherwise the holder has to be gone — either its pid no
// longer exists, or the preflight already established that the pid has been
// recycled. `--force` takes over regardless.
func (p *RunLockPolicy) Stale(h LockHolder) bool {
	if p.opts.Force || h.PID <= 0 || h.PID == p.opts.self() {
		return true
	}
	if host := p.opts.host(); host != "" && h.Host != "" && h.Host != host {
		return false
	}
	if last := p.Last(); last.Record.PID == h.PID && last.Record.Run == h.Run {
		switch last.State {
		case LockStale:
			return true
		case LockLive, LockElsewhere:
			return false
		case LockFree, LockMine, LockCorrupt:
			// Nothing useful was remembered about this holder; fall
			// through to the live probe below.
		}
	}
	return !p.opts.alive(h.PID)
}

// *RunLockPolicy is the policy Run.Start applies (P6-01's seam).
var _ LockPolicy = (*RunLockPolicy)(nil)

// ProcessStart returns an opaque token identifying the incarnation of the
// process with that id: two calls for the same running process agree, and a
// pid the kernel has recycled into another program gets a different token.
// It returns "" when the process is gone or the platform cannot tell, in
// which case callers fall back to plain liveness.
//
// On Linux the token is field 22 of /proc/<pid>/stat, the process's start
// time in clock ticks since boot. Everything before it is skipped from the
// closing parenthesis of the comm field, which may itself contain spaces
// and parentheses.
func ProcessStart(pid int) string {
	if pid <= 0 || runtime.GOOS != "linux" {
		return ""
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat") //nolint:gosec // a fixed procfs path for a pid we were given.
	if err != nil {
		return ""
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return ""
	}
	fields := strings.Fields(string(data)[end+1:])
	// fields[0] is field 3 (state), so field 22 (starttime) is index 19.
	const startTimeIndex = 19
	if len(fields) <= startTimeIndex {
		return ""
	}
	if _, err := strconv.ParseUint(fields[startTimeIndex], 10, 64); err != nil {
		return ""
	}
	return fields[startTimeIndex]
}

// ProcessAlive reports whether a process with that id exists on this
// machine (signal 0). It cannot say the process is the *same* one that took
// a lock — that is what [ProcessStart] adds — and a process owned by
// another user answers "yes" without being probeable further.
func ProcessAlive(pid int) bool { return processAlive(pid) }
