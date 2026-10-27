package dash

import (
	"time"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/orchestrator"
	"slop-cannon/internal/tui"
)

func (o *Observer) project(now time.Time) Snapshot {
	s := Snapshot{
		AsOf: now.UTC(), Counts: Counts{BeadsByState: make(map[string]int)},
		Beads: make([]BeadSnapshot, 0), LiveAgents: make([]AgentSnapshot, 0),
		RecentAgents: make([]AgentSnapshot, 0), Reviews: make([]ReviewSnapshot, 0),
		Deliveries: make([]DeliverySnapshot, 0), Notices: make([]NoticeSnapshot, 0),
		Activity: make([]Activity, 0),
	}
	s.Run = o.projectRun(now)
	s.Observer = Health{Status: "ok", Stream: o.stream, LastEventAt: s.Run.LastEventAt}
	s.Counts.Events = o.runState.Events
	s.Counts.Beads = len(o.runState.Beads)
	for _, b := range o.runState.Beads {
		state := boundedText(b.State.String(), maxIDText)
		if state == "" {
			state = "unrecorded"
		}
		if _, ok := s.Counts.BeadsByState[state]; ok || len(s.Counts.BeadsByState) < maxBeadStates-1 || state == "other" {
			s.Counts.BeadsByState[state]++
		} else {
			s.Counts.BeadsByState["other"]++
		}
	}
	beads := lastWindow(o.runState.Beads, o.opts.Limits.Beads)
	s.Omitted.Beads = len(o.runState.Beads) - len(beads)
	for _, b := range beads {
		s.Beads = append(s.Beads, projectBead(b))
	}

	s.Counts.Agents = len(o.runState.Agents)
	agentsCanBeLive := s.Run.Status == "running" || s.Run.Status == "unknown"
	for _, a := range o.runState.Agents {
		if !a.Live() || !agentsCanBeLive {
			continue
		}
		s.Counts.LiveAgents++
		if len(s.LiveAgents) < maxLiveAgents {
			s.LiveAgents = append(s.LiveAgents, o.projectAgent(a, now, true))
		}
	}
	s.LiveAgentsOmitted = s.Counts.LiveAgents - len(s.LiveAgents)
	for i := len(o.runState.Agents) - 1; i >= 0 && len(s.RecentAgents) < o.opts.Limits.RecentAgents; i-- {
		a := o.runState.Agents[i]
		if a.Live() && agentsCanBeLive {
			continue
		}
		s.RecentAgents = append(s.RecentAgents, o.projectAgent(a, now, false))
	}
	s.Omitted.Agents = len(o.runState.Agents) - len(s.LiveAgents) - len(s.RecentAgents)

	s.Counts.Reviews = len(o.runState.Rounds)
	rounds := lastWindow(o.runState.Rounds, o.opts.Limits.Reviews)
	s.Omitted.Reviews = len(o.runState.Rounds) - len(rounds)
	for _, r := range rounds {
		s.Reviews = append(s.Reviews, projectReview(r))
	}

	s.Counts.Deliveries = len(o.runState.Deliveries)
	deliveries := lastWindow(o.runState.Deliveries, o.opts.Limits.Deliveries)
	s.Omitted.Deliveries = len(o.runState.Deliveries) - len(deliveries)
	for _, d := range deliveries {
		s.Deliveries = append(s.Deliveries, projectDelivery(d))
	}
	// Delivery totals count the full fold, including entries outside the window.
	for _, d := range o.runState.Deliveries {
		if d.Number > 0 {
			s.Counts.PullRequests++
		}
		if d.Merged {
			s.Counts.Merges++
		}
	}

	s.Counts.Notices = o.noticeTotal
	notices := lastWindow(o.runState.Notices, o.opts.Limits.Notices)
	s.Omitted.Notices = max(o.noticeTotal-len(notices), 0)
	for _, n := range notices {
		s.Notices = append(s.Notices, projectNotice(n))
	}
	s.Counts.Activity = o.activityTotal
	for _, a := range o.activity {
		a.Kind = boundedText(a.Kind, maxIDText)
		a.Text = boundedText(a.Text, maxActivityText)
		s.Activity = append(s.Activity, a)
	}
	s.Omitted.Activity = max(o.activityTotal-len(o.activity), 0)
	if o.host != nil {
		host := cloneHostSnapshot(*o.host)
		s.Host = &host
	}
	if o.opts.Dandelion {
		s.Dandelion = o.projectDandelion(now)
	}
	return s
}

func (o *Observer) projectRun(now time.Time) RunSnapshot {
	run := o.runState.Run
	if o.hasDurable && o.durable.Run.ID != "" {
		run = o.durable.Run
	}
	r := RunSnapshot{
		ID:   domain.RunID(boundedText(string(run.ID), maxIDText)),
		Epic: domain.BeadID(boundedText(string(o.runState.Epic), maxIDText)),
		Ammo: run.Config.Ammo, Safety: run.Config.Safety,
		Mode: boundedText(run.Mode.String(), maxIDText), Resumed: o.runState.Resumed,
		Paused: o.runState.Paused, Stopping: o.runState.Stopping, StartedAt: run.StartedAt,
	}
	if r.ID == "" {
		r.ID = domain.RunID(boundedText(string(o.opts.Run.ID), maxIDText))
	}
	if r.Mode == "" {
		r.Mode = domain.RunModeAutopilot.String()
	}
	if r.Epic == "" {
		r.Epic = domain.BeadID(boundedText(string(run.Epic), maxIDText))
	}
	finished := o.runState.Finish != nil
	resumedActive := o.runState.Resumed && o.runState.Live
	if o.hasDurable {
		st := o.durable
		r.Resumed = r.Resumed || st.Resumes > 0
		durableFinished := st.Phase == orchestrator.PhaseFinished || st.Finish != "" || !st.FinishedAt.IsZero()
		if st.Phase != "" && (!resumedActive || !durableFinished) {
			r.Phase = boundedText(st.Phase.String(), maxIDText)
		}
		finished = finished || (!resumedActive && durableFinished)
		if !resumedActive {
			r.FinishReason = boundedText(st.Finish.String(), maxIDText)
			r.FinishedAt = st.FinishedAt
		}
	}
	if o.runState.Finish != nil {
		r.FinishReason = boundedText(o.runState.Finish.Reason.String(), maxIDText)
		r.FinishedAt = o.runState.Finish.At()
	}
	if finished {
		r.Phase = orchestrator.PhaseFinished.String()
		r.Paused, r.Stopping = false, false
	} else if r.Phase == "" {
		switch {
		case o.runState.Stopping:
			r.Phase = orchestrator.PhaseStopping.String()
		case o.runState.Paused:
			r.Phase = orchestrator.PhasePaused.String()
		case o.seenEvent:
			r.Phase = orchestrator.PhaseRunning.String()
		default:
			r.Phase = orchestrator.PhaseNew.String()
		}
	}

	mine := o.lock.Record.PID > 0 && (run.ID == "" || o.lock.Record.Run == run.ID)
	running := mine && (o.lock.State == orchestrator.LockLive || o.lock.State == orchestrator.LockMine)
	r.Lock = LockSnapshot{
		State: boundedText(o.lock.State.String(), maxIDText), Held: o.lock.Record.PID > 0, Mine: mine,
		Alive: o.liveness.Alive, Running: running,
		PIDReused: mine && o.lock.State == orchestrator.LockStale && o.liveness.Alive,
	}
	active := o.seenEvent && o.runState.Live && !finished
	if o.hasDurable && o.durable.Phase != orchestrator.PhaseNew {
		active = o.durable.Phase.Live() && !finished
	}
	switch {
	case finished:
		r.Status = "finished"
	case active && running:
		r.Status = "running"
	case active && mine && o.lock.State == orchestrator.LockElsewhere:
		r.Status = "unknown"
	case active:
		r.Status = "crashed"
	case running:
		r.Status = "running"
	default:
		r.Status = "unknown"
	}

	r.LastEventAt = o.lastEventAt
	if r.LastEventAt.IsZero() && o.hasDurable {
		r.LastEventAt = o.durable.LastEventAt
	}
	quietSince := r.LastEventAt
	if quietSince.IsZero() {
		quietSince = r.StartedAt
	}
	if r.Status == "running" && o.opts.QuietAfter > 0 && !quietSince.IsZero() {
		quietFor := now.Sub(quietSince)
		r.Quiet = quietFor >= o.opts.QuietAfter
		if r.Quiet {
			r.QuietForSeconds = max(int64(quietFor/time.Second), 0)
		}
	}
	end := now
	if finished && !r.FinishedAt.IsZero() {
		end = r.FinishedAt
	}
	if !r.StartedAt.IsZero() {
		r.ElapsedMillis = max(end.Sub(r.StartedAt).Milliseconds(), 0)
	}
	return r
}

func projectBead(b domain.Bead) BeadSnapshot {
	deps := lastWindow(b.Deps, maxBeadDeps)
	out := BeadSnapshot{
		ID:    domain.BeadID(boundedText(string(b.ID), maxIDText)),
		Title: boundedText(b.Title, maxTitleText),
		State: boundedText(b.State.String(), maxIDText), Type: boundedText(b.Type.String(), maxIDText),
		Priority: b.Priority, Size: boundedText(b.Size.String(), maxIDText),
		Round: b.Round, PRNumber: b.PRNumber,
		PRURL: boundedText(b.PRURL, maxURLText), Deps: make([]domain.BeadID, 0, len(deps)),
		DepsOmitted: len(b.Deps) - len(deps),
	}
	for _, id := range deps {
		out.Deps = append(out.Deps, domain.BeadID(boundedText(string(id), maxIDText)))
	}
	return out
}

func projectAgent(a tui.AgentView, now time.Time, live bool) AgentSnapshot {
	elapsed := a.Duration
	if a.Live() && !a.StartedAt.IsZero() {
		elapsed = now.Sub(a.StartedAt)
	}
	return AgentSnapshot{
		ID:   domain.AgentID(boundedText(string(a.ID), maxIDText)),
		Role: boundedText(a.Role.String(), maxIDText), Lens: boundedText(a.Lens.String(), maxIDText),
		Cartridge: boundedText(a.Cartridge, maxIDText), Model: boundedText(a.Model, maxIDText),
		Bead:      domain.BeadID(boundedText(string(a.Bead), maxIDText)),
		BeadTitle: boundedText(a.BeadTitle, maxTitleText),
		Task:      boundedText(a.Task.String(), maxIDText), State: boundedText(a.State.String(), maxIDText),
		Round: a.Round, Attempt: a.Attempt, StartedAt: a.StartedAt,
		ElapsedMillis: max(elapsed.Milliseconds(), 0), ExitCode: a.ExitCode,
		Failure: boundedText(a.Failure.String(), maxIDText), Produced: boundedText(a.Produced.String(), maxIDText),
		Live: live, LastSeen: a.Live() && !live,
	}
}

func (o *Observer) projectAgent(a tui.AgentView, now time.Time, live bool) AgentSnapshot {
	projected := projectAgent(a, now, live)
	if a.Cartridge != "dandelion" {
		return projected
	}
	projected.RouteStatus = routeStatusUnknown
	launch, ok := o.agentLaunches[a.ID]
	if !ok {
		return projected
	}
	incarnation, ok := o.launchLog.resolveGeneration(launch)
	if !ok {
		return projected
	}
	key := launchAttemptKey{agent: string(a.ID), attempt: a.Attempt, incarnation: incarnation}
	attempt, ok := o.launchLog.attempt(key)
	if !ok {
		return projected
	}
	projected.RouteAttempt = key.attempt
	if attempt.ambiguous || attempt.cartridge != "dandelion" {
		return projected
	}
	if len(attempt.routes) > 0 {
		route := attempt.routes[len(attempt.routes)-1]
		at := route.at.UTC()
		projected.RouteStatus = routeStatusRouted
		projected.RoutedModel = boundedText(route.model, maxIDText)
		projected.RoutedAccount = boundedText(route.account, maxIDText)
		projected.RoutedEffort = boundedText(route.effort, maxIDText)
		projected.RoutedAt = &at
		projected.RouteOrdinal = route.ordinal
		return projected
	}
	if live && a.Live() {
		projected.RouteStatus = routeStatusPending
		return projected
	}
	failure, ok := o.agentFailures[launchFailureKey{agent: string(a.ID), attempt: a.Attempt, spawn: launch.serial}]
	if ok && a.State == domain.AgentStateFailed && failure == events.FailureRateLimited {
		projected.RouteStatus = routeStatusNoRoute
	}
	return projected
}

func projectReview(r tui.Round) ReviewSnapshot {
	lenses := lastWindow(r.Lenses, maxReviewLens)
	verdicts := lastWindow(r.Verdicts, maxVerdicts)
	out := ReviewSnapshot{
		Bead: domain.BeadID(boundedText(string(r.Bead), maxIDText)), Number: r.Number,
		Kind: boundedText(r.Kind.String(), maxIDText), Lenses: make([]string, 0, len(lenses)),
		LensesOmitted: len(r.Lenses) - len(lenses),
		Commit:        boundedText(r.Commit, maxCommitText), Verdicts: make([]VerdictSnapshot, 0, len(verdicts)),
		VerdictsOmitted: len(r.Verdicts) - len(verdicts), ResponseCount: len(r.Responses),
		RulingCount: len(r.Rulings), HasReport: r.Report != nil,
	}
	for _, lens := range lenses {
		out.Lenses = append(out.Lenses, boundedText(lens.String(), maxIDText))
	}
	if r.Escalation != nil {
		out.Escalation = boundedText(r.Escalation.Outcome.String(), maxIDText)
	}
	for _, v := range verdicts {
		out.Verdicts = append(out.Verdicts, VerdictSnapshot{
			Lens: boundedText(v.Lens.String(), maxIDText), Outcome: boundedText(v.Outcome.String(), maxIDText),
			ItemCount: len(v.Items),
		})
	}
	return out
}

func projectDelivery(d tui.Delivery) DeliverySnapshot {
	return DeliverySnapshot{
		Bead: domain.BeadID(boundedText(string(d.Bead), maxIDText)),
		URL:  boundedText(d.URL, maxURLText), Number: d.Number,
		Branch: boundedText(d.Branch, maxBranchText), Base: boundedText(d.Base, maxBranchText),
		External: d.External, Merged: d.Merged, SHA: boundedText(d.SHA, maxCommitText), Pushed: d.Pushed,
	}
}

func lastWindow[T any](values []T, limit int) []T {
	if limit <= 0 {
		return []T{}
	}
	if len(values) <= limit {
		return values
	}
	return values[len(values)-limit:]
}

func cloneSnapshot(s Snapshot) Snapshot {
	if s.Host != nil {
		host := cloneHostSnapshot(*s.Host)
		s.Host = &host
	}
	if s.Dandelion != nil {
		dandelion := cloneDandelionSnapshot(*s.Dandelion)
		s.Dandelion = &dandelion
	}
	s.Beads = cloneSlice(s.Beads)
	for i := range s.Beads {
		s.Beads[i].Deps = cloneSlice(s.Beads[i].Deps)
	}
	s.LiveAgents = cloneAgentSnapshots(s.LiveAgents)
	s.RecentAgents = cloneAgentSnapshots(s.RecentAgents)
	s.Reviews = cloneSlice(s.Reviews)
	for i := range s.Reviews {
		s.Reviews[i].Lenses = cloneSlice(s.Reviews[i].Lenses)
		s.Reviews[i].Verdicts = cloneSlice(s.Reviews[i].Verdicts)
	}
	s.Deliveries = cloneSlice(s.Deliveries)
	s.Notices = cloneSlice(s.Notices)
	s.Activity = cloneSlice(s.Activity)
	if s.Counts.BeadsByState != nil {
		counts := make(map[string]int, len(s.Counts.BeadsByState))
		for state, n := range s.Counts.BeadsByState {
			counts[state] = n
		}
		s.Counts.BeadsByState = counts
	}
	return s
}

func cloneAgentSnapshots(values []AgentSnapshot) []AgentSnapshot {
	out := cloneSlice(values)
	for i := range out {
		if out[i].RoutedAt != nil {
			at := *out[i].RoutedAt
			out[i].RoutedAt = &at
		}
	}
	return out
}

func cloneSlice[T any](values []T) []T {
	if values == nil {
		return nil
	}
	out := make([]T, len(values))
	copy(out, values)
	return out
}
