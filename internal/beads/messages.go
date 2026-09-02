package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"slop-cannon/internal/domain"
)

// Errors of the message reader.
var (
	// ErrNoMessage means the comment is plain prose: it carries no fenced
	// JSON block with a `type` field.
	ErrNoMessage = errors.New("comment carries no structured message")
	// ErrBadMessage means the comment carries a typed block that could not
	// be decoded: an unknown type, or a payload the domain type rejects.
	ErrBadMessage = errors.New("malformed structured message")
	// ErrUnsupportedPayload means PostMessage was handed a value that is
	// not one of the six message payloads.
	ErrUnsupportedPayload = errors.New("unsupported message payload")
)

// Message is one structured agent message stored as a bead comment
// (D-06, D-25). The comment body is a one-line human headline, a blank
// line and one fenced JSON block whose object carries `type` next to the
// payload's own fields (a `response` message wraps its list as
// `{"type":"response","responses":[…]}`); see EncodeMessage.
type Message struct {
	// Type is which of the six payloads Payload holds.
	Type domain.MessageType
	// CommentID is bd's comment id.
	CommentID string
	// IssueID is the bead the message is on.
	IssueID domain.BeadID
	// Author is the bd actor that wrote it ("sc:eng-sc-68s-r2").
	Author string
	// Agent is the agent id behind Author, when it is an sc actor.
	Agent domain.AgentID
	// CreatedAt is when bd stored the comment.
	CreatedAt time.Time
	// Prose is the text around the JSON block (the headline), trimmed.
	Prose string
	// Raw is the JSON block as written.
	Raw json.RawMessage
	// Payload is the decoded message: domain.Plan, domain.ImplReport,
	// domain.Verdict, []domain.Response, domain.Arbitration or
	// domain.Escalation. It is nil when Err is set.
	Payload any
	// Err is set when the block's type is unknown or its payload failed to
	// decode (wrapping ErrBadMessage); Type and Raw are still filled in as
	// far as they could be.
	Err error
}

// Plan returns the payload of a plan message.
func (m Message) Plan() (domain.Plan, bool) { v, ok := m.Payload.(domain.Plan); return v, ok }

// ImplReport returns the payload of an impl_report message.
func (m Message) ImplReport() (domain.ImplReport, bool) {
	v, ok := m.Payload.(domain.ImplReport)
	return v, ok
}

// Verdict returns the payload of a verdict message.
func (m Message) Verdict() (domain.Verdict, bool) {
	v, ok := m.Payload.(domain.Verdict)
	return v, ok
}

// Responses returns the payload of a response message.
func (m Message) Responses() ([]domain.Response, bool) {
	v, ok := m.Payload.([]domain.Response)
	return v, ok
}

// Arbitration returns the payload of an arbitration message.
func (m Message) Arbitration() (domain.Arbitration, bool) {
	v, ok := m.Payload.(domain.Arbitration)
	return v, ok
}

// Escalation returns the payload of an escalation message.
func (m Message) Escalation() (domain.Escalation, bool) {
	v, ok := m.Payload.(domain.Escalation)
	return v, ok
}

// Decode unmarshals the raw block into v, for callers with their own
// struct (P5-05 schemas). The `type` key is ignored by encoding/json
// unless v declares it.
func (m Message) Decode(v any) error {
	if len(m.Raw) == 0 {
		return ErrNoMessage
	}
	return json.Unmarshal(m.Raw, v)
}

// Summary is the one-line description used by the timeline: the headline
// derived from the payload, else the decoding problem, else the prose.
func (m Message) Summary() string {
	if m.Payload != nil {
		return Headline(m.Type, m.Payload)
	}
	if m.Err != nil {
		name := string(m.Type)
		if name == "" {
			name = "message"
		}
		return name + ": " + m.Err.Error()
	}
	if line := firstLineOf(m.Prose); line != "" {
		return line
	}
	return string(m.Type)
}

// MessageTypeOf returns the message type for a payload value: a
// domain.Plan, ImplReport, Verdict, Arbitration or Escalation (value or
// pointer), a []domain.Response, or a Message. Anything else is
// ErrUnsupportedPayload.
func MessageTypeOf(msg any) (domain.MessageType, error) {
	t, _, err := normalisePayload(msg)
	return t, err
}

// normalisePayload maps every accepted input shape to (type, value).
func normalisePayload(msg any) (domain.MessageType, any, error) {
	switch v := msg.(type) {
	case Message:
		if v.Payload == nil {
			return "", nil, fmt.Errorf("%w: message has no payload", ErrUnsupportedPayload)
		}
		return normalisePayload(v.Payload)
	case *Message:
		if v == nil {
			return "", nil, fmt.Errorf("%w: nil message", ErrUnsupportedPayload)
		}
		return normalisePayload(*v)
	case domain.Plan:
		return domain.MessagePlan, v, nil
	case *domain.Plan:
		return domain.MessagePlan, *v, nil
	case domain.ImplReport:
		return domain.MessageImplReport, v, nil
	case *domain.ImplReport:
		return domain.MessageImplReport, *v, nil
	case domain.Verdict:
		return domain.MessageVerdict, v, nil
	case *domain.Verdict:
		return domain.MessageVerdict, *v, nil
	case []domain.Response:
		return domain.MessageResponse, v, nil
	case *[]domain.Response:
		return domain.MessageResponse, *v, nil
	case domain.Response:
		return domain.MessageResponse, []domain.Response{v}, nil
	case domain.Arbitration:
		return domain.MessageArbitration, v, nil
	case *domain.Arbitration:
		return domain.MessageArbitration, *v, nil
	case domain.Escalation:
		return domain.MessageEscalation, v, nil
	case *domain.Escalation:
		return domain.MessageEscalation, *v, nil
	default:
		return "", nil, fmt.Errorf("%w: %T", ErrUnsupportedPayload, msg)
	}
}

// EncodeMessage renders the comment body for a payload (see MessageTypeOf
// for the accepted values): the Headline, a blank line and the fenced
// JSON block with `type` as its first key.
func EncodeMessage(msg any) (string, error) {
	t, payload, err := normalisePayload(msg)
	if err != nil {
		return "", err
	}
	block, err := EncodeBlock(t, payload)
	if err != nil {
		return "", err
	}
	return Headline(t, payload) + "\n\n```json\n" + string(block) + "\n```\n", nil
}

// EncodeBlock renders the JSON object of a message: the payload's fields
// with `type` first, or `{"type":"response","responses":[…]}` for the
// list payload. The output is indented for humans reading `bd show`.
func EncodeBlock(t domain.MessageType, payload any) (json.RawMessage, error) {
	if !t.Valid() {
		return nil, fmt.Errorf("%w: message type %q", ErrUnsupportedPayload, string(t))
	}
	var raw []byte
	if t == domain.MessageResponse {
		list, ok := payload.([]domain.Response)
		if !ok {
			return nil, fmt.Errorf("%w: response message wants []domain.Response, got %T", ErrUnsupportedPayload, payload)
		}
		if list == nil {
			list = []domain.Response{}
		}
		data, err := json.Marshal(struct {
			Type      domain.MessageType `json:"type"`
			Responses []domain.Response  `json:"responses"`
		}{t, list})
		if err != nil {
			return nil, fmt.Errorf("beads: encoding %s message: %w", t, err)
		}
		raw = data
	} else {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("beads: encoding %s message: %w", t, err)
		}
		data = bytes.TrimSpace(data)
		if len(data) == 0 || data[0] != '{' {
			return nil, fmt.Errorf("%w: %s payload is not a JSON object", ErrUnsupportedPayload, t)
		}
		head := `{"type":` + strconv.Quote(string(t))
		if bytes.Equal(data, []byte("{}")) {
			raw = []byte(head + "}")
		} else {
			raw = append([]byte(head+","), data[1:]...)
		}
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return nil, fmt.Errorf("beads: encoding %s message: %w", t, err)
	}
	return out.Bytes(), nil
}

// Headline is the human one-liner written above the JSON block and shown
// in the timeline, for example "Verdict (qa, round 2): changes requested,
// 2 items" or "Implementation report: fixed Mean".
func Headline(t domain.MessageType, payload any) string {
	switch v := payload.(type) {
	case domain.Plan:
		s := fmt.Sprintf("Plan: %d bead%s", len(v.Beads), plural(len(v.Beads)))
		if line := firstLineOf(v.Brief); line != "" {
			s += " - " + line
		}
		return s
	case domain.ImplReport:
		s := "Implementation report"
		if line := firstLineOf(v.Summary); line != "" {
			s += ": " + line
		}
		if n := len(v.FilesChanged); n > 0 {
			s += fmt.Sprintf(" (%d file%s)", n, plural(n))
		}
		return s
	case domain.Verdict:
		s := "Verdict"
		var tags []string
		if v.Lens != "" {
			tags = append(tags, string(v.Lens))
		}
		if v.Round > 0 {
			tags = append(tags, "round "+strconv.Itoa(v.Round))
		}
		if len(tags) > 0 {
			s += " (" + strings.Join(tags, ", ") + ")"
		}
		switch v.Outcome {
		case domain.VerdictApprove:
			s += ": approved"
		case domain.VerdictChanges:
			s += ": changes requested"
		default:
			s += ": " + string(v.Outcome)
		}
		if n := len(v.Items); n > 0 {
			s += fmt.Sprintf(", %d item%s", n, plural(n))
		}
		return s
	case []domain.Response:
		counts := map[domain.ResponseAction]int{}
		for _, r := range v {
			counts[r.Action]++
		}
		s := fmt.Sprintf("Responses: %d item%s", len(v), plural(len(v)))
		var parts []string
		for _, a := range domain.ResponseActions() {
			if counts[a] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", counts[a], a))
			}
		}
		if len(parts) > 0 {
			s += " (" + strings.Join(parts, ", ") + ")"
		}
		return s
	case domain.Arbitration:
		s := "Arbitration"
		if v.ItemID != "" {
			s += " on " + v.ItemID
		}
		return s + ": " + string(v.Ruling)
	case domain.Escalation:
		s := "Escalation: " + string(v.Outcome)
		if line := firstLineOf(v.Summary); line != "" {
			s += " - " + line
		}
		return s
	default:
		return string(t)
	}
}

// plural returns "s" unless n is 1.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// fenceRE matches a fenced code block, with or without a language tag.
var fenceRE = regexp.MustCompile("(?s)```[A-Za-z]*[ \t]*\r?\n(.*?)\r?\n[ \t]*```")

// ExtractJSONBlock finds the structured message in a comment body: the
// first fenced code block whose content is a JSON object with a non-empty
// `type` string, tolerating any prose before, between and after the
// fences. Without fences, a bare JSON object with a `type` anywhere in the
// text is accepted too. It returns the block, the surrounding prose
// (trimmed) and whether one was found.
func ExtractJSONBlock(text string) (block json.RawMessage, prose string, ok bool) {
	for _, loc := range fenceRE.FindAllStringSubmatchIndex(text, -1) {
		candidate := strings.TrimSpace(text[loc[2]:loc[3]])
		if !hasType(candidate) {
			continue
		}
		return json.RawMessage(candidate), joinProse(text[:loc[0]], text[loc[1]:]), true
	}
	start := 0
	for {
		idx := strings.Index(text[start:], "{")
		if idx < 0 {
			return nil, "", false
		}
		idx += start
		dec := json.NewDecoder(strings.NewReader(text[idx:]))
		var raw json.RawMessage
		if err := dec.Decode(&raw); err == nil && hasType(string(raw)) {
			end := idx + int(dec.InputOffset())
			return raw, joinProse(text[:idx], text[end:]), true
		}
		start = idx + 1
	}
}

// joinProse trims the text before and after the block and joins the two
// with a blank line.
func joinProse(before, after string) string {
	before, after = strings.TrimSpace(before), strings.TrimSpace(after)
	switch {
	case before == "":
		return after
	case after == "":
		return before
	default:
		return before + "\n\n" + after
	}
}

// hasType reports whether s is a JSON object with a non-empty string
// `type`.
func hasType(s string) bool {
	if !strings.HasPrefix(s, "{") {
		return false
	}
	var probe struct {
		Type string `json:"type"`
	}
	return json.Unmarshal([]byte(s), &probe) == nil && probe.Type != ""
}

// ParseMessage reads the structured message out of a comment. Plain
// prose is ErrNoMessage. A typed block that does not decode returns the
// partially filled Message (Type when it parsed, Raw, Prose, Err) and an
// error wrapping ErrBadMessage.
func ParseMessage(c Comment) (Message, error) {
	block, prose, ok := ExtractJSONBlock(c.Text)
	if !ok {
		return Message{}, ErrNoMessage
	}
	m := Message{CommentID: c.ID, IssueID: c.IssueID, Author: c.Author, CreatedAt: c.CreatedAt, Prose: prose, Raw: block}
	m.Agent, _ = AgentOf(c.Author)
	var probe struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(block, &probe)
	t, err := domain.ParseMessageType(probe.Type)
	if err != nil {
		m.Err = fmt.Errorf("%w: %w", ErrBadMessage, err)
		return m, m.Err
	}
	m.Type = t
	payload, err := decodePayload(t, block)
	if err != nil {
		m.Err = fmt.Errorf("%w: %s: %w", ErrBadMessage, t, err)
		return m, m.Err
	}
	m.Payload = payload
	return m, nil
}

// decodePayload unmarshals the block into the domain type for t.
func decodePayload(t domain.MessageType, block json.RawMessage) (any, error) {
	switch t {
	case domain.MessagePlan:
		var v domain.Plan
		return v, json.Unmarshal(block, &v)
	case domain.MessageImplReport:
		var v domain.ImplReport
		return v, json.Unmarshal(block, &v)
	case domain.MessageVerdict:
		var v domain.Verdict
		return v, json.Unmarshal(block, &v)
	case domain.MessageResponse:
		var v struct {
			Responses []domain.Response `json:"responses"`
			Items     []domain.Response `json:"items"`
		}
		if err := json.Unmarshal(block, &v); err != nil {
			return nil, err
		}
		switch {
		case v.Responses != nil:
			return v.Responses, nil
		case v.Items != nil:
			return v.Items, nil
		default:
			return nil, errors.New("no responses list")
		}
	case domain.MessageArbitration:
		var v domain.Arbitration
		return v, json.Unmarshal(block, &v)
	case domain.MessageEscalation:
		var v domain.Escalation
		return v, json.Unmarshal(block, &v)
	default:
		return nil, fmt.Errorf("%w: message type %q", domain.ErrUnknownValue, string(t))
	}
}

// ParseMessages reads the structured messages out of comments, in order.
// Prose comments are skipped; a typed block that does not decode is kept
// with Err set so the reader can report it instead of losing history.
func ParseMessages(comments []Comment) []Message {
	var out []Message
	for _, c := range comments {
		m, err := ParseMessage(c)
		if errors.Is(err, ErrNoMessage) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// MessagesOfType returns the messages of type t, in order.
func MessagesOfType(msgs []Message, t domain.MessageType) []Message {
	var out []Message
	for _, m := range msgs {
		if m.Type == t && m.Err == nil {
			out = append(out, m)
		}
	}
	return out
}

// LastMessage returns the latest well-formed message of type t.
func LastMessage(msgs []Message, t domain.MessageType) (Message, bool) {
	for k := len(msgs) - 1; k >= 0; k-- {
		if msgs[k].Type == t && msgs[k].Err == nil {
			return msgs[k], true
		}
	}
	return Message{}, false
}

// PostMessage writes msg (see MessageTypeOf) as a comment on the bead,
// authored by actor (an agent's ActorFor, or "" for the client's own
// actor), and returns the stored Message. Agents never write to beads
// themselves (D-21); the orchestrator records their output through this.
func (c *Client) PostMessage(ctx context.Context, id domain.BeadID, actor string, msg any) (Message, error) {
	t, payload, err := normalisePayload(msg)
	if err != nil {
		return Message{}, err
	}
	block, err := EncodeBlock(t, payload)
	if err != nil {
		return Message{}, err
	}
	headline := Headline(t, payload)
	body := headline + "\n\n```json\n" + string(block) + "\n```\n"
	cm, err := c.WithActor(actor).Comment(ctx, id, body)
	if err != nil {
		return Message{}, err
	}
	m := Message{Type: t, CommentID: cm.ID, IssueID: cm.IssueID, Author: cm.Author, CreatedAt: cm.CreatedAt, Prose: headline, Raw: block, Payload: payload}
	if m.IssueID == "" {
		m.IssueID = id
	}
	if m.Author == "" {
		m.Author = c.WithActor(actor).Actor()
	}
	m.Agent, _ = AgentOf(m.Author)
	return m, nil
}

// Messages reads the bead's comments and returns the structured messages
// in creation order (see ParseMessages).
func (c *Client) Messages(ctx context.Context, id domain.BeadID) ([]Message, error) {
	comments, err := c.Comments(ctx, id)
	if err != nil {
		return nil, err
	}
	return ParseMessages(comments), nil
}

// Timeline events.
const (
	// EventCreated is the bead's creation.
	EventCreated = "created"
	// EventClaimed is the first claim (bd's started_at).
	EventClaimed = "claimed"
	// EventMessage is a structured message.
	EventMessage = "message"
	// EventClosed is the close.
	EventClosed = "closed"
	// EventStatus is the current status of a bead that is not closed.
	EventStatus = "status"
)

// TimelineEntry is one line of a bead's history.
type TimelineEntry struct {
	// At is when it happened (UTC).
	At time.Time
	// Actor is who did it (a bd actor), when known.
	Actor string
	// Event is one of the Event constants.
	Event string
	// Summary is the one-line description.
	Summary string
	// Message is set for EventMessage.
	Message *Message
}

// Timeline builds a bead's history from the issue's bookkeeping fields
// and its structured messages, in time order: created, claimed, every
// message, then closed or the current status.
func Timeline(issue Issue, msgs []Message) []TimelineEntry {
	var out []TimelineEntry
	if !issue.CreatedAt.IsZero() {
		typ := issue.Type
		if typ == "" {
			typ = string(domain.BeadTypeTask)
		}
		out = append(out, TimelineEntry{At: issue.CreatedAt, Actor: issue.CreatedBy, Event: EventCreated,
			Summary: fmt.Sprintf("created (%s, p%d)", typ, issue.Priority)})
	}
	if !issue.StartedAt.IsZero() {
		actor := issue.Assignee
		if actor == "" && issue.Agent() != "" {
			actor = ActorFor(issue.Agent())
		}
		out = append(out, TimelineEntry{At: issue.StartedAt, Actor: actor, Event: EventClaimed, Summary: "claimed"})
	}
	for k := range msgs {
		m := &msgs[k]
		out = append(out, TimelineEntry{At: m.CreatedAt, Actor: m.Author, Event: EventMessage, Summary: m.Summary(), Message: m})
	}
	switch {
	case !issue.ClosedAt.IsZero():
		s := "closed"
		if issue.CloseReason != "" {
			s += ": " + issue.CloseReason
		}
		out = append(out, TimelineEntry{At: issue.ClosedAt, Event: EventClosed, Summary: s})
	case issue.Status != "" && issue.Status != string(domain.BeadStateClosed):
		var parts []string
		if issue.Round() > 0 {
			parts = append(parts, "round "+strconv.Itoa(issue.Round()))
		}
		if issue.Assignee != "" {
			parts = append(parts, "assignee "+issue.Assignee)
		}
		if issue.PRURL() != "" {
			parts = append(parts, "PR "+issue.PRURL())
		}
		s := "now " + issue.Status
		if len(parts) > 0 {
			s += " (" + strings.Join(parts, ", ") + ")"
		}
		at := issue.UpdatedAt
		if at.IsZero() {
			at = issue.CreatedAt
		}
		out = append(out, TimelineEntry{At: at, Event: EventStatus, Summary: s})
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].At.Before(out[b].At) })
	return out
}

// FormatTimeline renders a bead's history for `sc status <bead-id>`: a
// header with the id, title and status, then one line per TimelineEntry
// with the UTC time, the actor and the summary.
func FormatTimeline(issue Issue, msgs []Message) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n", issue.ID, issue.Title)
	meta := []string{"status " + issue.Status}
	if run := issue.RunID(); run != "" {
		meta = append(meta, "run "+string(run))
	}
	if issue.Round() > 0 {
		meta = append(meta, "round "+strconv.Itoa(issue.Round()))
	}
	if issue.Assignee != "" {
		meta = append(meta, "assignee "+issue.Assignee)
	}
	if issue.PRURL() != "" {
		meta = append(meta, "PR "+issue.PRURL())
	}
	fmt.Fprintf(&b, "  %s\n", strings.Join(meta, "; "))
	entries := Timeline(issue, msgs)
	width := 0
	for _, e := range entries {
		if len(e.Actor) > width {
			width = len(e.Actor)
		}
	}
	for _, e := range entries {
		fmt.Fprintf(&b, "  %s  %-*s  %s\n", e.At.UTC().Format("2006-01-02 15:04:05"), width, e.Actor, e.Summary)
	}
	return b.String()
}

// firstLineOf returns the first non-empty line of s, trimmed.
func firstLineOf(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}
