package dash

import (
	"fmt"
	"strings"

	"slop-cannon/internal/events"
)

// activityFor renders lifecycle metadata only. In particular, it does not use
// tui.FormatEvent: that renderer intentionally includes output, error and
// report prose for a terminal transcript, none of which belongs in this API.
func activityFor(ev events.Event) (Activity, bool) {
	a := Activity{At: ev.At(), Kind: boundedText(ev.Kind().String(), maxIDText)}
	switch e := ev.(type) {
	case events.RunStarted:
		a.Text = "Run started"
		if e.Resumed {
			a.Text = "Run resumed"
		}
	case events.RunPaused:
		a.Text = "Run paused"
	case events.RunResumed:
		a.Text = "Run resumed"
	case events.RunStopped:
		a.Text = "Run stopping"
	case events.RunFinished:
		a.Text = "Run finished: " + e.Reason.String()
	case events.PlanProduced:
		a.Text = fmt.Sprintf("Plan produced: %d beads", len(e.Plan.Beads))
	case events.BeadCreated:
		a.Text = fmt.Sprintf("Bead %s created", e.Bead.ID)
	case events.BeadUpdated:
		a.Text = fmt.Sprintf("Bead %s updated", e.Bead.ID)
	case events.BeadStateChanged:
		a.Text = fmt.Sprintf("Bead %s: %s → %s", e.Bead, e.From, e.To)
	case events.GraphChanged:
		a.Text = fmt.Sprintf("Bead graph updated: %d beads", len(e.Beads))
	case events.AgentSpawned:
		a.Text = fmt.Sprintf("Agent %s started (%s)", e.Agent.ID, e.Agent.Role)
	case events.AgentFinished:
		a.Text = fmt.Sprintf("Agent %s finished", e.Agent)
	case events.AgentFailed:
		a.Text = strings.TrimSpace(fmt.Sprintf("Agent %s failed: %s", e.Agent, e.Failure))
	case events.RoundStarted:
		a.Text = fmt.Sprintf("Bead %s review round %d started (%s)", e.Bead, e.Round, e.RoundKind)
	case events.VerdictReceived:
		a.Text = fmt.Sprintf("Bead %s round %d: %s %s (%d items)", e.Bead, e.Round,
			e.Verdict.Lens, e.Verdict.Outcome, len(e.Verdict.Items))
	case events.ResponseReceived:
		a.Text = fmt.Sprintf("Bead %s round %d: %d responses", e.Bead, e.Round, len(e.Responses))
	case events.ArbitrationReceived:
		a.Text = fmt.Sprintf("Bead %s round %d: %d rulings", e.Bead, e.Round, len(e.Rulings))
	case events.PROpened:
		a.Text = fmt.Sprintf("Bead %s: pull request #%d opened", e.Bead, e.Number)
	case events.Merged:
		a.Text = fmt.Sprintf("Bead %s merged", e.Bead)
	case events.Warning:
		a.Text = "Warning"
		if e.Source != "" {
			a.Text += " from " + e.Source
		}
	case events.Error:
		a.Text = "Error"
		if e.Source != "" {
			a.Text += " from " + e.Source
		}
	case events.AgentOutput:
		return Activity{}, false
	default:
		a.Text = ev.Kind().String()
	}
	a.Text = boundedText(a.Text, maxActivityText)
	return a, true
}
