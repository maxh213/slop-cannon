package beads

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/testutil"
)

// messagesPayloads is one well-formed payload per message type (D-25).
func messagesPayloads() map[domain.MessageType]any {
	return map[domain.MessageType]any{
		domain.MessagePlan: domain.Plan{Brief: "fix the two stats bugs", Beads: []domain.PlanBead{
			{Key: "mean", Title: "Fix Mean", Acceptance: "TestMean passes", Priority: 1, Size: domain.SizeS},
			{Key: "max", Title: "Fix Max", Acceptance: "TestMax passes", Priority: 2, Type: domain.BeadTypeBug, Deps: []string{"mean"}},
		}},
		domain.MessageImplReport: domain.ImplReport{Summary: "fixed Mean\nand tidied the loop", FilesChanged: []string{"stats/stats.go"}, HowToVerify: "go test ./...", Risks: []string{"none"}, Commit: "abc1234"},
		domain.MessageVerdict: domain.Verdict{Lens: domain.LensQA, Round: 2, Outcome: domain.VerdictChanges, Summary: "one edge case", Items: []domain.FeedbackItem{
			{ID: "qa-r2-1", Severity: domain.SeverityMajor, Title: "empty slice panics", File: "stats/stats.go", Line: 12, Status: domain.ItemOpen},
			{ID: "qa-r2-2", Severity: domain.SeverityNit, Category: "style", Title: "naming", Status: domain.ItemOpen},
		}},
		domain.MessageResponse: []domain.Response{
			{ItemID: "qa-r2-1", Action: domain.ActionFixed, Rationale: "guarded the empty case"},
			{ItemID: "qa-r2-2", Action: domain.ActionPushback, Rationale: "matches the file"},
		},
		domain.MessageArbitration: domain.Arbitration{ItemID: "qa-r2-2", Ruling: domain.RulingUpholdEngineer, Rationale: "taste"},
		domain.MessageEscalation:  domain.Escalation{Outcome: domain.EscalationNarrowScope, NewAcceptance: "TestMean passes", Summary: "drop the Max half"},
	}
}

// messagesPayload returns the sample payload for mt as T.
func messagesPayload[T any](t *testing.T, mt domain.MessageType) T {
	t.Helper()
	v, ok := messagesPayloads()[mt].(T)
	require.True(t, ok, "payload for %s is not a %T", mt, v)
	return v
}

func TestMessageRoundTripEveryType(t *testing.T) {
	for _, mt := range domain.MessageTypes() {
		payload, ok := messagesPayloads()[mt]
		require.True(t, ok, "no sample payload for %s", mt)
		t.Run(string(mt), func(t *testing.T) {
			got, err := MessageTypeOf(payload)
			require.NoError(t, err)
			assert.Equal(t, mt, got)

			body, err := EncodeMessage(payload)
			require.NoError(t, err)
			lines := strings.Split(body, "\n")
			assert.NotEmpty(t, lines[0], "a human headline comes first")
			assert.Equal(t, "", lines[1])
			assert.Equal(t, "```json", lines[2])
			assert.True(t, strings.HasSuffix(body, "\n```\n"), body)
			assert.Contains(t, lines[3], "{")
			assert.Contains(t, lines[4], `"type": "`+string(mt)+`"`, "type is the first key")

			m, err := ParseMessage(Comment{ID: "c1", IssueID: "fx-1", Author: "sc:agent-1", Text: body, CreatedAt: time.Unix(1, 0)})
			require.NoError(t, err)
			assert.Equal(t, mt, m.Type)
			assert.Equal(t, payload, m.Payload)
			assert.Equal(t, "c1", m.CommentID)
			assert.Equal(t, domain.BeadID("fx-1"), m.IssueID)
			assert.Equal(t, "sc:agent-1", m.Author)
			assert.Equal(t, domain.AgentID("agent-1"), m.Agent)
			assert.Equal(t, lines[0], m.Prose)
			assert.Nil(t, m.Err)
			assert.Equal(t, lines[0], m.Summary(), "the headline is stable across the round trip")
			assert.Equal(t, Headline(mt, payload), m.Summary())

			// The typed accessors agree with the payload.
			switch mt {
			case domain.MessagePlan:
				v, ok := m.Plan()
				assert.True(t, ok)
				assert.NoError(t, v.Validate())
			case domain.MessageImplReport:
				v, ok := m.ImplReport()
				assert.True(t, ok)
				assert.Equal(t, "abc1234", v.Commit)
			case domain.MessageVerdict:
				v, ok := m.Verdict()
				assert.True(t, ok)
				assert.Equal(t, domain.VerdictChanges, v.Outcome)
				assert.Len(t, v.Items, 2)
			case domain.MessageResponse:
				v, ok := m.Responses()
				assert.True(t, ok)
				assert.Len(t, v, 2)
			case domain.MessageArbitration:
				v, ok := m.Arbitration()
				assert.True(t, ok)
				assert.Equal(t, domain.RulingUpholdEngineer, v.Ruling)
			case domain.MessageEscalation:
				v, ok := m.Escalation()
				assert.True(t, ok)
				assert.Equal(t, domain.EscalationNarrowScope, v.Outcome)
			}

			// Decode into a caller's own struct sees the same fields.
			var generic map[string]any
			require.NoError(t, m.Decode(&generic))
			assert.Equal(t, string(mt), generic["type"])

			// Pointers and a Message are accepted as input too.
			body2, err := EncodeMessage(m)
			require.NoError(t, err)
			assert.Equal(t, body, body2)
		})
	}
}

func TestMessagePointerPayloads(t *testing.T) {
	plan := messagesPayload[domain.Plan](t, domain.MessagePlan)
	report := messagesPayload[domain.ImplReport](t, domain.MessageImplReport)
	verdict := messagesPayload[domain.Verdict](t, domain.MessageVerdict)
	responses := messagesPayload[[]domain.Response](t, domain.MessageResponse)
	arb := messagesPayload[domain.Arbitration](t, domain.MessageArbitration)
	esc := messagesPayload[domain.Escalation](t, domain.MessageEscalation)
	for in, want := range map[any]domain.MessageType{
		&plan: domain.MessagePlan, &report: domain.MessageImplReport, &verdict: domain.MessageVerdict,
		&responses: domain.MessageResponse, &arb: domain.MessageArbitration, &esc: domain.MessageEscalation,
	} {
		got, err := MessageTypeOf(in)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	got, err := MessageTypeOf(domain.Response{ItemID: "x", Action: domain.ActionFixed})
	require.NoError(t, err)
	assert.Equal(t, domain.MessageResponse, got, "a single response is wrapped in a list")

	for _, bad := range []any{"text", 42, nil, struct{}{}, Message{}, (*Message)(nil), map[string]any{"type": "verdict"}} {
		_, err := MessageTypeOf(bad)
		assert.ErrorIs(t, err, ErrUnsupportedPayload, "%T", bad)
		_, err = EncodeMessage(bad)
		assert.ErrorIs(t, err, ErrUnsupportedPayload, "%T", bad)
	}
	_, err = EncodeBlock("verdict", "not a verdict")
	assert.ErrorIs(t, err, ErrUnsupportedPayload)
	_, err = EncodeBlock("response", domain.Verdict{})
	assert.ErrorIs(t, err, ErrUnsupportedPayload)
	_, err = EncodeBlock("nope", domain.Verdict{})
	assert.ErrorIs(t, err, ErrUnsupportedPayload)
}

func TestEncodeBlockShapes(t *testing.T) {
	block, err := EncodeBlock(domain.MessageResponse, []domain.Response(nil))
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"response","responses":[]}`, string(block), "a nil list still encodes as an empty list")

	block, err = EncodeBlock(domain.MessageArbitration, domain.Arbitration{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"arbitration","item_id":"","ruling":""}`, string(block))

	block, err = EncodeBlock(domain.MessageEscalation, domain.Escalation{Outcome: domain.EscalationBlock, Summary: "needs a decision"})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(block), "{\n  \"type\": \"escalation\","), string(block))
	assert.JSONEq(t, `{"type":"escalation","outcome":"block","summary":"needs a decision"}`, string(block))
}

func TestHeadlines(t *testing.T) {
	p := messagesPayloads()
	assert.Equal(t, "Plan: 2 beads - fix the two stats bugs", Headline(domain.MessagePlan, p[domain.MessagePlan]))
	assert.Equal(t, "Plan: 1 bead", Headline(domain.MessagePlan, domain.Plan{Beads: []domain.PlanBead{{Key: "a"}}}))
	assert.Equal(t, "Implementation report: fixed Mean (1 file)", Headline(domain.MessageImplReport, p[domain.MessageImplReport]))
	assert.Equal(t, "Implementation report", Headline(domain.MessageImplReport, domain.ImplReport{}))
	assert.Equal(t, "Verdict (qa, round 2): changes requested, 2 items", Headline(domain.MessageVerdict, p[domain.MessageVerdict]))
	assert.Equal(t, "Verdict: approved", Headline(domain.MessageVerdict, domain.Verdict{Outcome: domain.VerdictApprove}))
	assert.Equal(t, "Verdict (ux): approved, 1 item", Headline(domain.MessageVerdict, domain.Verdict{Lens: domain.LensUX, Outcome: domain.VerdictApprove, Items: []domain.FeedbackItem{{ID: "x"}}}))
	assert.Equal(t, "Responses: 2 items (1 fixed, 1 pushback)", Headline(domain.MessageResponse, p[domain.MessageResponse]))
	assert.Equal(t, "Responses: 0 items", Headline(domain.MessageResponse, []domain.Response{}))
	assert.Equal(t, "Arbitration on qa-r2-2: uphold_engineer", Headline(domain.MessageArbitration, p[domain.MessageArbitration]))
	assert.Equal(t, "Arbitration: compromise", Headline(domain.MessageArbitration, domain.Arbitration{Ruling: domain.RulingCompromise}))
	assert.Equal(t, "Escalation: narrow_scope - drop the Max half", Headline(domain.MessageEscalation, p[domain.MessageEscalation]))
	assert.Equal(t, "verdict", Headline(domain.MessageVerdict, "something else"))
}

func TestExtractJSONBlock(t *testing.T) {
	block, prose, ok := ExtractJSONBlock("Heading\n\n```json\n{\"type\":\"verdict\",\"outcome\":\"approve\"}\n```\n\nTrailing prose.")
	require.True(t, ok)
	assert.JSONEq(t, `{"type":"verdict","outcome":"approve"}`, string(block))
	assert.Equal(t, "Heading\n\nTrailing prose.", prose)

	// An untagged fence, CRLF line ends and indentation inside the block.
	block, prose, ok = ExtractJSONBlock("```\r\n{\r\n  \"type\": \"plan\",\r\n  \"beads\": []\r\n}\r\n```\r\n")
	require.True(t, ok)
	assert.JSONEq(t, `{"type":"plan","beads":[]}`, string(block))
	assert.Empty(t, prose)

	// The first fenced block with a type wins; code blocks without one are
	// prose.
	text := "Look at this:\n\n```go\nfunc main() {}\n```\n\nand this:\n\n```json\n{\"outcome\":\"approve\"}\n```\n\n```json\n{\"type\":\"response\",\"responses\":[]}\n```\n\n```json\n{\"type\":\"verdict\"}\n```"
	block, prose, ok = ExtractJSONBlock(text)
	require.True(t, ok)
	assert.JSONEq(t, `{"type":"response","responses":[]}`, string(block))
	assert.Contains(t, prose, "func main() {}")
	assert.Contains(t, prose, `{"type":"verdict"}`, "later blocks stay in the prose")

	// Bare JSON without a fence, prose around it, braces in the prose.
	block, prose, ok = ExtractJSONBlock("I think {this} is fine: {\"type\": \"escalation\", \"outcome\": \"block\"} done")
	require.True(t, ok)
	assert.JSONEq(t, `{"type":"escalation","outcome":"block"}`, string(block))
	assert.Equal(t, "I think {this} is fine:\n\ndone", prose)

	for _, none := range []string{"", "plain prose", "{\"outcome\":\"approve\"}", "```json\n{\"outcome\":\"approve\"}\n```", "```json\n[1,2]\n```", "{ broken", "{\"type\": 3}", "{\"type\": \"\"}"} {
		_, _, ok := ExtractJSONBlock(none)
		assert.False(t, ok, "%q", none)
	}
}

func TestParseMessageErrors(t *testing.T) {
	_, err := ParseMessage(Comment{Text: "just prose"})
	assert.ErrorIs(t, err, ErrNoMessage)
	_, err = ParseMessage(Comment{Text: "```json\n{\"outcome\":\"approve\"}\n```"})
	assert.ErrorIs(t, err, ErrNoMessage, "a block without a type is not a message")

	m, err := ParseMessage(Comment{ID: "c9", Author: "sc:tester-ux-1", Text: "```json\n{\"type\":\"review\",\"outcome\":\"approve\"}\n```"})
	assert.ErrorIs(t, err, ErrBadMessage)
	assert.ErrorIs(t, err, domain.ErrUnknownValue)
	assert.Empty(t, m.Type)
	assert.Equal(t, "c9", m.CommentID)
	assert.Equal(t, domain.AgentID("tester-ux-1"), m.Agent)
	assert.NotEmpty(t, m.Raw, "the raw block is kept for diagnosis")
	assert.Equal(t, m.Err, err)
	assert.Contains(t, m.Summary(), "message: ")

	m, err = ParseMessage(Comment{Text: "```json\n{\"type\":\"verdict\",\"outcome\":\"maybe\"}\n```"})
	assert.ErrorIs(t, err, ErrBadMessage)
	assert.Equal(t, domain.MessageVerdict, m.Type, "the type parsed even though the payload did not")
	assert.Nil(t, m.Payload)
	assert.True(t, strings.HasPrefix(m.Summary(), "verdict: "), m.Summary())

	m, err = ParseMessage(Comment{Text: "```json\n{\"type\":\"response\"}\n```"})
	assert.ErrorIs(t, err, ErrBadMessage)
	assert.Contains(t, err.Error(), "no responses list")

	// Lenient type spelling and the alternative list key still parse.
	m, err = ParseMessage(Comment{Text: "```json\n{\"type\":\"Impl-Report\",\"summary\":\"x\"}\n```"})
	require.NoError(t, err)
	assert.Equal(t, domain.MessageImplReport, m.Type)
	m, err = ParseMessage(Comment{Text: "```json\n{\"type\":\"response\",\"items\":[{\"item_id\":\"a\",\"action\":\"wontfix\"}]}\n```"})
	require.NoError(t, err)
	rs, _ := m.Responses()
	assert.Len(t, rs, 1)

	assert.ErrorIs(t, Message{}.Decode(&struct{}{}), ErrNoMessage)
	assert.Equal(t, "verdict", Message{Type: domain.MessageVerdict}.Summary())
	assert.Equal(t, "Hello", Message{Prose: "\nHello\nworld"}.Summary())
}

func TestParseMessagesFromCapturedComments(t *testing.T) {
	var comments []Comment
	require.NoError(t, Decode(envSample(t, "comments-messages.json"), &comments))
	msgs := ParseMessages(comments)
	require.Len(t, msgs, 7, "the prose comment is skipped, the malformed one kept")
	var types []domain.MessageType
	for _, m := range msgs {
		types = append(types, m.Type)
	}
	assert.Equal(t, []domain.MessageType{
		domain.MessagePlan, domain.MessageImplReport, domain.MessageVerdict, domain.MessageResponse,
		domain.MessageArbitration, domain.MessageVerdict, domain.MessageEscalation,
	}, types)

	plan, ok := msgs[0].Plan()
	require.True(t, ok)
	assert.Equal(t, "fix the two stats bugs", plan.Brief)
	require.Len(t, plan.Beads, 2)
	assert.Equal(t, domain.PriorityDefault, plan.Beads[1].Priority, "a missing priority is the default")
	assert.NoError(t, plan.Validate())

	report, ok := msgs[1].ImplReport()
	require.True(t, ok)
	assert.Equal(t, []string{"stats/stats.go"}, report.FilesChanged)

	verdict, ok := msgs[2].Verdict()
	require.True(t, ok)
	assert.Equal(t, domain.LensQA, verdict.Lens)
	assert.Equal(t, 1, verdict.Round)
	assert.Equal(t, "Verdict (qa, round 1): changes requested, 1 item\n\nSome prose before the block.\n\nAnd prose after it.", msgs[2].Prose)
	assert.Equal(t, domain.AgentID("tester-qa-1"), msgs[2].Agent)

	assert.ErrorIs(t, msgs[5].Err, ErrBadMessage)
	assert.Nil(t, msgs[5].Payload)

	assert.Len(t, MessagesOfType(msgs, domain.MessageVerdict), 1, "the malformed verdict is not counted")
	last, ok := LastMessage(msgs, domain.MessageVerdict)
	assert.True(t, ok)
	assert.Equal(t, "c-verdict", last.CommentID)
	_, ok = LastMessage(msgs, domain.MessagePlan)
	assert.True(t, ok)
	_, ok = LastMessage(nil, domain.MessagePlan)
	assert.False(t, ok)
	assert.Empty(t, MessagesOfType(nil, domain.MessagePlan))

	// The P3-02 capture (a minimal impl_report) parses as well.
	var captured []Comment
	require.NoError(t, Decode(envSample(t, "comments.json"), &captured))
	got := ParseMessages(captured)
	require.Len(t, got, 2)
	assert.Equal(t, domain.MessageImplReport, got[0].Type)
	r, _ := got[0].ImplReport()
	assert.Equal(t, "fixed Mean", r.Summary)
	assert.Equal(t, "Implementation report", got[0].Prose)
	assert.Empty(t, got[1].Agent, "the plumbing actor is not an agent")
}

func TestPostMessage(t *testing.T) {
	c, bd := clientFake(t, nil)
	verdict := messagesPayload[domain.Verdict](t, domain.MessageVerdict)
	m, err := c.PostMessage(context.Background(), "fx-so0", ActorFor("tester-qa-1"), verdict)
	require.NoError(t, err)
	assert.Equal(t, domain.MessageVerdict, m.Type)
	assert.Equal(t, verdict, m.Payload)
	assert.Equal(t, "01a0636a-ad0d-7bd0-8fe1-9ddb12501edd", m.CommentID, "bd's id")
	assert.Equal(t, domain.BeadID("fx-so0"), m.IssueID)
	assert.Equal(t, "sc:engineer-1", m.Author, "the author bd reported")
	assert.Equal(t, domain.AgentID("engineer-1"), m.Agent)
	assert.False(t, m.CreatedAt.IsZero())
	assert.Equal(t, "Verdict (qa, round 2): changes requested, 2 items", m.Prose)
	assert.Equal(t, m.Prose, m.Summary())

	inv := bd.Invocations(t)
	require.Len(t, inv, 1)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "comment", "fx-so0", "--stdin", "--json"}, inv[0].Args)
	body, err := EncodeMessage(verdict)
	require.NoError(t, err)
	assert.Equal(t, body, inv[0].Stdin, "the body on stdin is exactly EncodeMessage")
	back, err := ParseMessage(Comment{Text: inv[0].Stdin})
	require.NoError(t, err)
	assert.Equal(t, verdict, back.Payload)

	_, err = c.PostMessage(context.Background(), "fx-so0", "", 42)
	assert.ErrorIs(t, err, ErrUnsupportedPayload)
	assert.Len(t, bd.ArgLists(t), 1)

	failing, _ := clientFake(t, map[string]string{"comment": "error-comment-not-found"})
	_, err = failing.PostMessage(context.Background(), "nope-1", "", verdict)
	assert.ErrorIs(t, err, ErrNotFound)
	assert.Empty(t, testutil.MissingSamples(t, bd))
}

func TestPostMessageActorReachesTheProcess(t *testing.T) {
	envIsolate(t)
	bd := testutil.FakeBin(t, "bd", `echo "$BEADS_ACTOR" > "$SC_FAKEBIN_DIR/actor"
echo '{"data":{"id":"c-1","issue_id":"fx-1","author":"'"$BEADS_ACTOR"'","text":"x"},"schema_version":1}'`, testutil.WithStdinCapture())
	c := New("/repo", Options{Actor: "sc:orch"})
	m, err := c.PostMessage(context.Background(), "fx-1", ActorFor("tester-qa-1"), domain.Arbitration{ItemID: "a", Ruling: domain.RulingCompromise, Instruction: "do both"})
	require.NoError(t, err)
	actor, err := os.ReadFile(filepath.Join(bd.Dir, "actor"))
	require.NoError(t, err)
	assert.Equal(t, "sc:tester-qa-1", strings.TrimSpace(string(actor)), "the message is authored by the agent")
	assert.Equal(t, "sc:tester-qa-1", m.Author)
	assert.Equal(t, domain.AgentID("tester-qa-1"), m.Agent)
	assert.Equal(t, "sc:orch", c.Actor(), "the client's own actor is untouched")

	// An empty actor posts as the client itself.
	m, err = c.PostMessage(context.Background(), "fx-1", "", domain.Escalation{Outcome: domain.EscalationBlock})
	require.NoError(t, err)
	assert.Equal(t, "sc:orch", m.Author)
}

func TestMessagesFromClient(t *testing.T) {
	c, bd := clientFake(t, map[string]string{"comments": "comments-messages"})
	msgs, err := c.Messages(context.Background(), "fx-so0")
	require.NoError(t, err)
	assert.Len(t, msgs, 7)
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "comments", "fx-so0", "--json"}, clientLastArgs(t, bd))

	empty, _ := clientFake(t, map[string]string{"comments": "comments-empty"})
	msgs, err = empty.Messages(context.Background(), "fx-ctt")
	require.NoError(t, err)
	assert.Empty(t, msgs)

	failing, _ := clientFake(t, map[string]string{"comments": "error-not-found"})
	_, err = failing.Messages(context.Background(), "nope-1")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestTimeline(t *testing.T) {
	var comments []Comment
	require.NoError(t, Decode(envSample(t, "comments-messages.json"), &comments))
	msgs := ParseMessages(comments)
	is := conventionsIssue(t, "list-all.json", "fx-so0")
	require.Equal(t, "closed", is.Status)

	entries := Timeline(is, msgs)
	require.NotEmpty(t, entries)
	assert.Equal(t, EventCreated, entries[0].Event)
	assert.Equal(t, "created (bug, p1)", entries[0].Summary)
	var claimed *TimelineEntry
	for k := range entries {
		if entries[k].Event == EventClaimed {
			claimed = &entries[k]
		}
	}
	require.NotNil(t, claimed)
	assert.Equal(t, "sc:engineer-1", claimed.Actor)
	assert.Equal(t, is.StartedAt, claimed.At)
	assert.Equal(t, EventClosed, entries[len(entries)-1].Event)
	assert.Equal(t, "closed: fixed in round 1", entries[len(entries)-1].Summary)
	for k := 1; k < len(entries); k++ {
		assert.False(t, entries[k].At.Before(entries[k-1].At), "entries are in time order")
	}
	var messages int
	for _, e := range entries {
		if e.Event == EventMessage {
			messages++
			require.NotNil(t, e.Message)
			assert.Equal(t, e.Message.Summary(), e.Summary)
			assert.Equal(t, e.Message.Author, e.Actor)
		}
	}
	assert.Equal(t, len(msgs), messages)

	out := FormatTimeline(is, msgs)
	t.Log("\n" + out)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	assert.Equal(t, "fx-so0  Fix Mean skipping the first value", lines[0])
	assert.Equal(t, "  status closed; run r1; assignee sc:engineer-1", lines[1])
	assert.Len(t, lines, 2+len(entries))
	assert.Regexp(t, `^  2026-09-02 18:38:\d\d  \S*\s+created \(bug, p1\)$`, lines[2])
	assert.Contains(t, out, "sc:tester-qa-1  Verdict (qa, round 1): changes requested, 1 item")
	assert.Contains(t, out, "Implementation report: fixed Mean (1 file)")
	assert.Contains(t, out, "Responses: 1 item (1 fixed)")
	assert.Contains(t, out, "Arbitration on qa-r1-1: uphold_tester")
	assert.Contains(t, out, "Escalation: approve_with_notes")
	assert.Contains(t, out, "verdict: malformed structured message", "a bad message is visible, not hidden")
	assert.Contains(t, out, "closed: fixed in round 1")
	for _, l := range lines[2:] {
		assert.True(t, strings.HasPrefix(l, "  2026-09-02 18:38:"), l)
	}

	// An open bead ends with its current status and bookkeeping.
	open := conventionsIssue(t, "list-filtered.json", "fx-ctt")
	entries = Timeline(open, nil)
	last := entries[len(entries)-1]
	assert.Equal(t, EventStatus, last.Event)
	assert.Equal(t, "now open (round 1, PR https://example.com/pr/7)", last.Summary)
	assert.Equal(t, open.UpdatedAt, last.At)
	out = FormatTimeline(open, nil)
	assert.Contains(t, out, "  status open; run r1; round 1; PR https://example.com/pr/7\n")

	// A bead with nothing but an id still renders.
	out = FormatTimeline(Issue{ID: "x", Title: "bare", Status: "in_review", Assignee: "sc:eng"}, nil)
	assert.Equal(t, "x  bare\n  status in_review; assignee sc:eng\n  0001-01-01 00:00:00    now in_review (assignee sc:eng)\n", out)
	assert.Empty(t, Timeline(Issue{}, nil))

	// A claimed bead without an assignee (released) names the lease agent.
	released := Issue{ID: "y", CreatedAt: time.Unix(10, 0), StartedAt: time.Unix(20, 0), UpdatedAt: time.Unix(30, 0), Metadata: Metadata{MetaAgent: "engineer-2"}, Status: "open"}
	entries = Timeline(released, nil)
	require.Len(t, entries, 3)
	assert.Equal(t, []string{EventCreated, EventClaimed, EventStatus}, []string{entries[0].Event, entries[1].Event, entries[2].Event})
	assert.Equal(t, "sc:engineer-2", entries[1].Actor)
	assert.Equal(t, "created (task, p0)", entries[0].Summary)
	assert.Equal(t, "now open", entries[2].Summary)
}

func TestMessageJSONFieldsMatchDomain(t *testing.T) {
	// The block's keys are the domain JSON tags, so P5-05's schemas and
	// the comment reader agree.
	block, err := EncodeBlock(domain.MessageVerdict, messagesPayloads()[domain.MessageVerdict])
	require.NoError(t, err)
	var keys map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(block, &keys))
	for _, k := range []string{"type", "lens", "round", "outcome", "summary", "items"} {
		assert.Contains(t, keys, k)
	}
}
