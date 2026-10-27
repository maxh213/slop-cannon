package dash

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/orchestrator"
	"slop-cannon/internal/tui"
)

// Observer owns the event-log and launch-log followers and the folded tui.State.
// The run loop alone mutates that state; readers see only snapshots under mu.
type Observer struct {
	opts     Options
	runState *tui.State

	activity           []Activity
	activityTotal      int
	noticeTotal        int
	lastEventAt        time.Time
	seenEvent          bool
	stream             string
	durable            orchestrator.State
	hasDurable         bool
	lock               orchestrator.LockProbe
	liveness           orchestrator.Liveness
	dandelion          DandelionSnapshot
	host               *HostSnapshot
	launchLog          launchLogHistory
	launchIncarnation  uint64
	currentEventStart  launchEventStart
	previousEventStart launchEventStart
	spawnSerial        uint64
	agentLaunches      map[domain.AgentID]agentLaunch
	agentFailures      map[launchFailureKey]events.Failure

	mu       sync.RWMutex
	snapshot Snapshot
	revision uint64
	started  atomic.Bool

	follow           func(context.Context, string, orchestrator.FollowOptions) <-chan events.Event
	loadState        func(string) (orchestrator.State, error)
	inspectLock      func(string, orchestrator.LockOptions) orchestrator.LockProbe
	probeLiveness    func(string, domain.RunID) orchestrator.Liveness
	probeDandelion   func(context.Context, time.Duration) dandelionProbeResult
	collectLaunchLog launchLogCollectFunc
	sampleHost       hostSampleFunc
	now              func() time.Time
}

// NewObserver builds an observer without opening files or starting goroutines.
// Run starts both followers. Negative or zero limits select the defaults.
func NewObserver(opts Options) *Observer {
	if opts.RefreshInterval <= 0 {
		opts.RefreshInterval = DefaultRefreshInterval
	}
	if opts.HostRefreshInterval <= 0 {
		opts.HostRefreshInterval = DefaultHostRefreshInterval
	}
	if opts.FollowPoll <= 0 {
		opts.FollowPoll = orchestrator.DefaultFollowPoll
	}
	if opts.LaunchLogInterval <= 0 {
		opts.LaunchLogInterval = defaultLaunchLogFollow
	}
	if opts.QuietAfter <= 0 {
		opts.QuietAfter = DefaultQuietAfter
	}
	if opts.DandelionRefresh <= 0 {
		opts.DandelionRefresh = DefaultDandelionRefresh
	}
	defaults := DefaultLimits()
	if opts.Limits.Beads <= 0 {
		opts.Limits.Beads = defaults.Beads
	}
	if opts.Limits.RecentAgents <= 0 {
		opts.Limits.RecentAgents = defaults.RecentAgents
	}
	if opts.Limits.Reviews <= 0 {
		opts.Limits.Reviews = defaults.Reviews
	}
	if opts.Limits.Deliveries <= 0 {
		opts.Limits.Deliveries = defaults.Deliveries
	}
	if opts.Limits.Notices <= 0 {
		opts.Limits.Notices = defaults.Notices
	}
	if opts.Limits.Activity <= 0 {
		opts.Limits.Activity = defaults.Activity
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	state := tui.NewState(opts.Run)
	state.MaxLog = max(opts.Limits.Activity, opts.Limits.Notices)
	o := &Observer{
		opts: opts, runState: state, stream: "starting",
		activity:      make([]Activity, 0, opts.Limits.Activity),
		agentLaunches: make(map[domain.AgentID]agentLaunch),
		agentFailures: make(map[launchFailureKey]events.Failure),
		follow:        orchestrator.FollowEventLog, loadState: orchestrator.LoadState,
		inspectLock: orchestrator.InspectLock, probeLiveness: orchestrator.ProbeLiveness,
		probeDandelion: probeDandelionSnapshot, collectLaunchLog: collectLaunchLog,
		sampleHost: newHostSampler(), now: opts.Now,
	}
	if opts.Dandelion {
		o.dandelion.Providers = make([]DandelionProviderSnapshot, 0)
	}
	o.publish()
	return o
}

// Run follows both logs and refreshes durable state and liveness until ctx ends.
// A closed event stream does not stop the observer: completed
// and crashed runs remain available to HTTP readers for as long as the server.
func (o *Observer) Run(ctx context.Context) {
	if !o.started.CompareAndSwap(false, true) {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	o.refresh()
	o.stream = "following"
	o.publish()

	var dandelionEvents <-chan dandelionEvent
	var dandelionDone <-chan struct{}
	if o.opts.Dandelion {
		dandelionEvents, dandelionDone = o.startDandelionCollection(runCtx)
	}
	launchLogEvents, launchLogDone := o.startLaunchLogCollection(runCtx)
	hostEvents, hostDone := o.startHostCollection(runCtx)
	stream := o.follow(runCtx, o.opts.RunDir, orchestrator.FollowOptions{
		Poll: o.opts.FollowPoll, FollowResumed: true,
		// This is the same read-only writer-gone probe sc watch uses. The
		// richer InspectLock result is projected separately for the UI.
		Ended: func() bool { return !o.probeLiveness(o.opts.RunDir, o.opts.Run.ID).Running() },
	})
	ticker := time.NewTicker(o.opts.RefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case ev, ok := <-stream:
			if !ok {
				stream = nil
				if o.runState.Finish != nil {
					o.stream = "complete"
				} else {
					o.stream = "closed"
				}
				o.refresh()
				o.publish()
				continue
			}
			o.apply(ev)
		case event, ok := <-launchLogEvents:
			if !ok {
				launchLogEvents = nil
				continue
			}
			o.launchLog.apply(event)
			o.publish()
		case event := <-dandelionEvents:
			o.applyDandelionEvent(event)
			o.publish()
		case event := <-hostEvents:
			o.applyHostEvent(event)
			o.publish()
		case <-ticker.C:
			o.refresh()
			o.publish()
		case <-ctx.Done():
			cancel()
			if launchLogDone != nil {
				<-launchLogDone
			}
			if dandelionDone != nil {
				<-dandelionDone
			}
			<-hostDone
			if o.opts.Dandelion {
				o.dandelion.Probing = false
			}
			o.stream = "closed"
			o.publish()
			return
		}
	}
}

// apply folds each event exactly once. Agent output updates tui.State as it
// does for sc watch, but has no Activity representation and is never exposed.
func (o *Observer) apply(ev events.Event) {
	if ev == nil {
		return
	}
	o.seenEvent = true
	o.lastEventAt = ev.At()
	o.trackAgentFailure(ev)
	o.runState.Apply(ev)
	if ev.Kind() == events.KindWarning || ev.Kind() == events.KindError {
		o.noticeTotal++
	}
	a, ok := activityFor(ev)
	if !ok {
		return
	}
	o.activityTotal++
	o.activity = append(o.activity, a)
	if extra := len(o.activity) - o.opts.Limits.Activity; extra > 0 {
		copy(o.activity, o.activity[extra:])
		o.activity = o.activity[:o.opts.Limits.Activity]
	}
}

func (o *Observer) trackAgentFailure(ev events.Event) {
	switch event := ev.(type) {
	case events.RunStarted:
		if event.RunID() == o.opts.Run.ID {
			o.launchIncarnation++
			o.previousEventStart = o.currentEventStart
			o.currentEventStart = launchEventStart{serial: o.launchIncarnation, at: event.At().UTC()}
		}
	case events.AgentSpawned:
		o.spawnSerial++
		if o.agentLaunches == nil {
			o.agentLaunches = make(map[domain.AgentID]agentLaunch)
		}
		if _, exists := o.agentLaunches[event.Agent.ID]; exists || len(o.agentLaunches) < maxLaunchLogContexts {
			o.agentLaunches[event.Agent.ID] = agentLaunch{
				spawnAt: event.At().UTC(), current: o.currentEventStart, previous: o.previousEventStart,
				serial: o.spawnSerial,
			}
		}
		o.clearAgentFailures(event.Agent.ID)
	case events.AgentFinished:
		o.clearAgentFailures(event.Agent)
	case events.AgentFailed:
		launch, ok := o.agentLaunches[event.Agent]
		if !ok {
			return
		}
		key := launchFailureKey{agent: string(event.Agent), attempt: event.Attempt, spawn: launch.serial}
		delete(o.agentFailures, key)
		if event.Attempt <= 0 || len(o.agentFailures) >= maxLaunchLogContexts {
			return
		}
		if o.agentFailures == nil {
			o.agentFailures = make(map[launchFailureKey]events.Failure)
		}
		o.agentFailures[key] = event.Failure
	}
}

func (o *Observer) clearAgentFailures(agent domain.AgentID) {
	for key := range o.agentFailures {
		if key.agent == string(agent) {
			delete(o.agentFailures, key)
		}
	}
}

// refresh reads only the run's durable state and lock files. An unavailable
// or temporarily unreadable state file leaves the last good durable snapshot
// in place; the event fold and cached browser snapshot remain useful.
func (o *Observer) refresh() {
	st, err := o.loadState(o.opts.RunDir)
	if err == nil {
		o.durable, o.hasDurable = st, true
	} else if errors.Is(err, orchestrator.ErrNoState) && !o.seenEvent {
		o.hasDurable = false
	}
	o.liveness = o.probeLiveness(o.opts.RunDir, o.opts.Run.ID)
	stateDir := filepath.Dir(orchestrator.LockPath(o.opts.RunDir))
	o.lock = o.inspectLock(stateDir, orchestrator.LockOptions{})
}

// publish builds a new bounded value and swaps it atomically under the mutex.
// Revisions increase on every published refresh and never move backwards.
func (o *Observer) publish() {
	s := o.project(o.now())
	o.mu.Lock()
	o.revision++
	s.Revision = o.revision
	s.Observer.Revision = o.revision
	o.snapshot = s
	o.mu.Unlock()
}

// Snapshot returns a deep copy of the current cached snapshot. It performs no
// file I/O and never reads or replays either log.
func (o *Observer) Snapshot() Snapshot {
	o.mu.RLock()
	s := o.snapshot
	o.mu.RUnlock()
	return cloneSnapshot(s)
}

// Health returns observer health independently of run liveness and quiet.
func (o *Observer) Health() Health {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.snapshot.Observer
}
