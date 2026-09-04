package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/orchestrator"
)

// TestStatusReportsTheBeadsTheRunLeftAlone is P6-28's reader half: a bead
// bd holds in a status this run never registered is passed over by every
// component, and the only trace is one Warning stamped with the
// foreign-status source. `sc status` folds it by that source — never by the
// wording — and names the bead, so a run over a repository whose beads sit
// in someone else's workflow does not just look idle.
func TestStatusReportsTheBeadsTheRunLeftAlone(t *testing.T) {
	_, repo := statusEnv(t)
	foreign := func(n int, bead domain.BeadID, status string) events.Warning {
		return events.Warning{
			Meta:    statusAt(n),
			Source:  orchestrator.SourceForeignStatus,
			Message: "bead " + string(bead) + " is in status " + status + ", which this run did not register",
			Bead:    bead,
		}
	}
	dir := statusRecord(t, repo, true,
		foreign(70, "sc-2", "triage"),
		// The same bead again, and a second bead: the count is of beads,
		// not of sightings.
		foreign(71, "sc-2", "triage"),
		foreign(72, "sc-3", "icebox"),
	)
	t.Cleanup(func() { statusUnlock(t, dir) })

	stdout, stderr, code := runExec(t, "status", "--repo", repo)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "2 beads in a status this run does not use: sc-2, sc-3")

	rep, err := RunStatus(repo, "", 5)
	require.NoError(t, err)
	require.NotNil(t, rep.Summary)
	assert.Equal(t, []domain.BeadID{"sc-2", "sc-3"}, rep.Summary.Foreign,
		"each bead is counted once, however many components met it")
}

// TestStatusReportsTheRunVitals is P6-29's exit criterion, read off disk
// the way `sc status` reads a run: the next planning time of an idle
// autopilot run, a gh outage with how long it has lasted, and a merge bd
// keeps refusing to close.
func TestStatusReportsTheRunVitals(t *testing.T) {
	_, repo := statusEnv(t)
	dir := statusRecord(t, repo, true)
	t.Cleanup(func() { statusUnlock(t, dir) })

	now := statusEpoch.Add(90 * time.Second)
	st, err := orchestrator.LoadState(dir)
	require.NoError(t, err)
	st.Vitals = &orchestrator.RunVitals{
		At: now,
		Planning: orchestrator.PlanningStatus{
			Mode: domain.RunModeAutopilot, Attempts: 2, Plans: 2, Beads: 3,
			NextAt: now.Add(90 * time.Second),
		},
		Gates: orchestrator.GatePollStats{
			Polls: 12, Skipped: 5, Pending: 1, Retries: 4,
			GHDown: true, GHDownSince: now.Add(-5 * time.Minute),
		},
	}
	require.NoError(t, orchestrator.WriteState(dir, st))

	stdout, stderr, code := runExec(t, "status", "--repo", repo)
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "vitals")
	assert.Contains(t, stdout, "next in 1m30s", "an idle autopilot run says when it plans again")
	assert.Contains(t, stdout, "gh unavailable", "a gh outage is visible without reading the log")
	assert.Contains(t, stdout, "5m0s so far")
	assert.Contains(t, stdout, "1 merge pending after 4 retries", "a stuck bd close shows as a pending merge")

	// And the same numbers survive --json, so a script need not parse the
	// text report.
	out, _, code := runExec(t, "status", "--repo", repo, "--json")
	require.Equal(t, 0, code)
	var rep StatusReport
	require.NoError(t, json.Unmarshal([]byte(out), &rep))
	require.NotNil(t, rep.State)
	require.NotNil(t, rep.State.Vitals)
	assert.True(t, rep.State.Vitals.Gates.GHDown)
	assert.Contains(t, out, `"next_at"`, "the planning half is snake_case like the rest of the document")
	assert.Equal(t, 1, rep.State.Vitals.Gates.Pending)
	assert.Contains(t, strings.Join(rep.State.Vitals.Lines(), "\n"), "gh unavailable")
}
