package events_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/logging"
)

const (
	// redactRun is the run every event in this file belongs to.
	redactRun domain.RunID = "run-redact"
	// redactSecret is what a cartridge might echo out of the environment: a
	// token the default rules mask. It must not survive anywhere.
	redactSecret = "sk-ant-api03-abcdefghijklmnop"
)

func redactMeta() events.Meta {
	return events.MetaAt(redactRun, time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC))
}

// redactEvents is one event per kind [events.Redacted] touches, with the
// secret planted in every untrusted field of each. The table is the contract:
// if a new free-text field is added to an event, this test fails until
// Redacted covers it.
func redactEvents() []events.Event {
	bead := domain.Bead{
		ID: "sc-1", Title: redactSecret, Description: redactSecret, Acceptance: redactSecret,
		Assignee: "sc:eng-1 " + redactSecret, State: domain.BeadStateOpen, PRURL: "https://x.test/" + redactSecret,
	}
	return []events.Event{
		events.RunStarted{Meta: redactMeta(), Info: domain.Run{ID: redactRun, Brief: "ship it with " + redactSecret}},
		events.RunPaused{Meta: redactMeta(), Reason: redactSecret},
		events.RunResumed{Meta: redactMeta(), Reason: redactSecret},
		events.RunStopped{Meta: redactMeta(), Reason: redactSecret},
		events.RunFinished{Meta: redactMeta(), Reason: events.FinishFailed, Summary: redactSecret},
		events.PlanProduced{Meta: redactMeta(), Agent: "po-1", Plan: domain.Plan{
			Brief: redactSecret,
			Beads: []domain.PlanBead{{Key: "a", Title: redactSecret, Description: redactSecret, Acceptance: redactSecret}},
		}},
		events.BeadCreated{Meta: redactMeta(), Bead: bead},
		events.BeadUpdated{Meta: redactMeta(), Bead: bead, Fields: []string{"title"}},
		events.BeadStateChanged{Meta: redactMeta(), Bead: "sc-1", To: domain.BeadStateInReview, Reason: redactSecret},
		events.GraphChanged{Meta: redactMeta(), Beads: []domain.Bead{bead}},
		events.AgentSpawned{Meta: redactMeta(), Agent: domain.Agent{ID: "eng-1"}, Task: events.TaskImplement,
			BeadTitle: redactSecret},
		events.AgentOutput{Meta: redactMeta(), Agent: "eng-1", Stream: events.StreamStdout,
			Line: "exporting ANTHROPIC_API_KEY=" + redactSecret},
		events.AgentFailed{Meta: redactMeta(), Agent: "eng-1", Failure: events.FailureCrash,
			Err: "auth failed for " + redactSecret},
		events.VerdictReceived{Meta: redactMeta(), Bead: "sc-1", Round: 1, Agent: "qa-1", Verdict: domain.Verdict{
			Outcome: domain.VerdictChanges, Summary: redactSecret,
			Items: []domain.FeedbackItem{{
				ID: "qa-1", Severity: domain.SeverityBlocker, Category: redactSecret,
				Title: redactSecret, Detail: redactSecret, Suggestion: redactSecret, File: redactSecret,
			}},
		}},
		events.ResponseReceived{Meta: redactMeta(), Bead: "sc-1", Round: 1, Agent: "eng-1",
			Responses: []domain.Response{{ItemID: "qa-1", Action: domain.ActionFixed, Rationale: redactSecret}},
			Report: &domain.ImplReport{
				Summary: redactSecret, HowToVerify: redactSecret, FilesChanged: []string{redactSecret},
				Risks: []string{redactSecret}, Questions: []string{redactSecret},
			}},
		events.ArbitrationReceived{Meta: redactMeta(), Bead: "sc-1", Round: 1, Agent: "po-1",
			Rulings: []domain.Arbitration{{ItemID: "qa-1", Ruling: domain.RulingCompromise,
				Instruction: redactSecret, Rationale: redactSecret}},
			Escalation: &domain.Escalation{Outcome: domain.EscalationBlock,
				Notes: redactSecret, NewAcceptance: redactSecret, Summary: redactSecret}},
		events.PROpened{Meta: redactMeta(), Bead: "sc-1", Number: 7,
			URL: "https://x.test/pr?token=" + redactSecret, Branch: redactSecret, Base: redactSecret},
		events.Merged{Meta: redactMeta(), Bead: "sc-1", SHA: "abc1234", Branch: redactSecret},
		events.Warning{Meta: redactMeta(), Source: redactSecret, Message: redactSecret, Err: redactSecret},
		events.Error{Meta: redactMeta(), Source: redactSecret, Message: redactSecret, Err: redactSecret},
	}
}

// TestRedactedMasksEveryUntrustedField: nothing that came out of a model, a
// cartridge, bd, git or gh keeps a secret once it has been through the hook,
// and the JSON envelope — what the run log stores — carries none either.
func TestRedactedMasksEveryUntrustedField(t *testing.T) {
	for _, ev := range redactEvents() {
		t.Run(string(ev.Kind()), func(t *testing.T) {
			raw, err := events.Marshal(ev)
			require.NoError(t, err)
			require.Contains(t, string(raw), redactSecret, "the fixture has to plant the secret to be worth anything")

			clean := events.Redacted(ev, logging.Redact)
			require.Equal(t, ev.Kind(), clean.Kind(), "redaction must not change the kind")
			require.Equal(t, ev.RunID(), clean.RunID(), "nor the run id")
			require.Equal(t, ev.At(), clean.At(), "nor the timestamp")

			out, err := events.Marshal(clean)
			require.NoError(t, err)
			require.NotContains(t, string(out), redactSecret, "the secret survived in %s", out)
			require.Contains(t, string(out), logging.Redacted, "the masked text has to say so")
		})
	}
}

// TestRedactedReachesTheBusAndTheRunLog is P9-09's own acceptance: a token
// planted in agent output never reaches a subscriber or the JSONL file when
// the publisher redacts at construction, which is the only place it can be
// done — the bus itself never transforms what it carries.
func TestRedactedReachesTheBusAndTheRunLog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bus := events.NewBus()
	defer bus.Close()
	sub := bus.Subscribe(ctx, 8)
	defer sub.Unsubscribe()

	var file bytes.Buffer
	log := events.NewJSONLWriter(&file)

	dirty := events.AgentOutput{
		Meta: redactMeta(), Agent: "eng-1", Stream: events.StreamStdout,
		Line: "$ curl -H 'Authorization: Bearer " + redactSecret + "'",
	}
	// What the publisher does: redact once, then hand the same value to
	// everyone. Both consumers see the masked line, and they see the same one.
	clean := events.Redacted(dirty, logging.Redact)
	bus.Publish(clean)
	require.NoError(t, log.Write(clean))

	select {
	case got := <-sub.Events():
		out, ok := got.(events.AgentOutput)
		require.True(t, ok)
		require.NotContains(t, out.Line, redactSecret, "the subscriber saw the token")
		require.Contains(t, out.Line, logging.Redacted)
	case <-time.After(time.Second):
		t.Fatal("no event reached the subscriber")
	}
	require.NotContains(t, file.String(), redactSecret, "the token reached the events file")
	require.Contains(t, file.String(), logging.Redacted)

	// And the event the publisher built is untouched, so nothing depends on
	// Redacted mutating in place.
	require.Contains(t, dirty.Line, redactSecret)
}

// TestRedactedDoesNotWriteThroughTheCallersSlices: an event is a value other
// subscribers may still be reading, so the copies matter.
func TestRedactedDoesNotWriteThroughTheCallersSlices(t *testing.T) {
	beads := []domain.Bead{{ID: "sc-1", Title: "keep me"}}
	items := []domain.FeedbackItem{{ID: "qa-1", Title: "keep me"}}
	report := domain.ImplReport{Summary: "keep me", FilesChanged: []string{"keep me"}}

	events.Redacted(events.GraphChanged{Meta: redactMeta(), Beads: beads}, strings.ToUpper)
	events.Redacted(events.VerdictReceived{Meta: redactMeta(), Verdict: domain.Verdict{Items: items}}, strings.ToUpper)
	events.Redacted(events.ResponseReceived{Meta: redactMeta(), Report: &report}, strings.ToUpper)

	require.Equal(t, "keep me", beads[0].Title)
	require.Equal(t, "keep me", items[0].Title)
	require.Equal(t, "keep me", report.Summary)
	require.Equal(t, []string{"keep me"}, report.FilesChanged)
}

// TestRedactedLeavesSCsOwnFieldsAlone: ids, states, counts and enums are
// minted by sc and by bd, not by a model, and the views key off them.
func TestRedactedLeavesSCsOwnFieldsAlone(t *testing.T) {
	in := events.AgentFailed{
		Meta: redactMeta(), Agent: "eng-1", Role: domain.RoleEngineer, Bead: "sc-1",
		Failure: events.FailureTimeout, Err: "boom", Attempt: 2, ExitCode: 9, WillRetry: true,
	}
	out, ok := events.Redacted(in, func(string) string { return "x" }).(events.AgentFailed)
	require.True(t, ok)
	require.Equal(t, "x", out.Err, "the error text is the untrusted half")
	require.Equal(t, in.Agent, out.Agent)
	require.Equal(t, in.Role, out.Role)
	require.Equal(t, in.Bead, out.Bead)
	require.Equal(t, in.Failure, out.Failure)
	require.Equal(t, in.Attempt, out.Attempt)
	require.Equal(t, in.ExitCode, out.ExitCode)
	require.True(t, out.WillRetry)
}

func TestRedactedTolerantOfNils(t *testing.T) {
	require.Nil(t, events.Redacted(nil, logging.Redact))
	ev := events.AgentOutput{Meta: redactMeta(), Line: redactSecret}
	require.Equal(t, ev, events.Redacted(ev, nil), "a nil rewriter leaves the event alone")
	unchanged := events.RoundStarted{Meta: redactMeta(), Bead: "sc-1", Round: 1}
	require.Equal(t, unchanged, events.Redacted(unchanged, func(string) string { return "x" }),
		"an event with no untrusted text is returned as it came in")
}

func TestChain(t *testing.T) {
	require.Nil(t, events.Chain())
	require.Nil(t, events.Chain(nil, nil))
	upper := events.TextFunc(strings.ToUpper)
	require.Equal(t, "AB", events.Chain(nil, upper, nil)("ab"), "the nil links are skipped")
	both := events.Chain(upper, func(s string) string { return s + "!" })
	require.Equal(t, "AB!", both("ab"), "the links are applied in order")
}
