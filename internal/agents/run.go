package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/structured"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// DefaultSchemaRetries is how many extra sessions [RunAgent] fires when a
// reply carries no valid structured result: one, as P5-08 specifies. A
// cartridge failure (crash, timeout, rate limit, …) is never retried here;
// that policy belongs to the supervisor (P6-07).
const DefaultSchemaRetries = structured.DefaultMaxRetries

// DefaultTranscriptLines is how many output lines a [Transcript] keeps when
// AgentRun.MaxLines is zero. Lines beyond it are still published on the bus;
// only the in-memory copy is capped, and [Transcript.Dropped] counts what it
// left out.
const DefaultTranscriptLines = 2000

// ErrInvalidAgentRun is wrapped by every [AgentRun.Validate] failure: the
// firing was refused before any subprocess started.
var ErrInvalidAgentRun = errors.New("agents: invalid agent run")

// AgentRun is one firing of one cartridge: what to run, which contract the
// reply must satisfy, and where the agent's output is published.
//
// Build the RunSpec with a role helper ([PlanRunSpec], [EngineerRequest.Firing],
// [TesterRunSpec], …) so the tool policy comes from [ApplyPolicy], then hand
// it here: [RunAgent] re-checks the policy with [EnforcementFor] and refuses
// to fire when the cartridge cannot hold it.
type AgentRun struct {
	// Cartridge is the adapter that runs the session. Required.
	Cartridge cartridge.Cartridge
	// Spec is the firing: prompt, workdir, tool policy, limits, labels.
	// Required and validated before anything starts.
	Spec cartridge.RunSpec
	// Message is the structured message the agent must produce. Required
	// unless Contract is set; it also selects the payload type
	// [RunMessage] decodes.
	Message domain.MessageType
	// Contract overrides the contract looked up from Message. Set it only
	// when the caller already holds the compiled contract (the role
	// builders do).
	Contract *structured.Contract
	// Agent identifies the firing on the event stream and on errors. ID is
	// required; Role, Lens and Bead are stamped on the transcript and on
	// [AgentError] when set.
	Agent domain.Agent
	// Task is what the agent was fired to do; it is recorded, not enforced.
	Task events.Task
	// Run is the run id stamped on published events.
	Run domain.RunID
	// Bus receives one [events.AgentOutput] per line the session produced.
	// Nil means nothing is published (the transcript is still collected).
	Bus *events.Bus
	// Observer, when set, is called for every session event of every
	// attempt, in order, before RunAgent returns.
	Observer func(cartridge.SessionEvent)
	// SchemaRetries caps the extra sessions fired after an unusable reply.
	// Zero means DefaultSchemaRetries; a negative value disables retries.
	SchemaRetries int
	// RetryPrompt builds the prompt of a retry attempt. Nil means
	// structured.RetryPrompt, which quotes structured.ParseError.Feedback.
	RetryPrompt structured.RetryPromptFunc
	// MaxLines caps how many output lines the transcript keeps: zero means
	// DefaultTranscriptLines, negative means keep every line.
	MaxLines int
	// Now is the clock used for durations; nil means time.Now. Orchestrator
	// code driven by a fake clock sets it.
	Now func() time.Time
}

// Validate reports the first problem that makes the firing impossible: no
// cartridge, an unusable spec, no agent id, an unknown role, lens or message
// type, or no way to resolve an output contract. Every error wraps
// [ErrInvalidAgentRun].
func (r AgentRun) Validate() error {
	if r.Cartridge == nil {
		return fmt.Errorf("%w: no cartridge", ErrInvalidAgentRun)
	}
	if err := r.Spec.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidAgentRun, err)
	}
	if strings.TrimSpace(r.Agent.ID.String()) == "" {
		return fmt.Errorf("%w: no agent id", ErrInvalidAgentRun)
	}
	if r.Agent.Role != "" && !r.Agent.Role.Valid() {
		return fmt.Errorf("%w: agent role %q is not a role", ErrInvalidAgentRun, r.Agent.Role.String())
	}
	if r.Agent.Lens != "" && !r.Agent.Lens.Valid() {
		return fmt.Errorf("%w: agent lens %q is not a lens", ErrInvalidAgentRun, r.Agent.Lens.String())
	}
	if _, err := r.OutputContract(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidAgentRun, err)
	}
	return nil
}

// OutputContract returns the contract the reply must satisfy: Contract when
// set, otherwise the contract for Message. An unusable Message yields an
// error wrapping [ErrUnknownMessageType].
func (r AgentRun) OutputContract() (*structured.Contract, error) {
	if r.Contract != nil {
		return r.Contract, nil
	}
	if r.Message == "" {
		return nil, errors.New("agents: agent run has neither a contract nor a message type")
	}
	return ContractFor(r.Message)
}

func (r AgentRun) retries() int {
	if r.SchemaRetries == 0 {
		return DefaultSchemaRetries
	}
	return max(r.SchemaRetries, 0)
}

func (r AgentRun) maxLines() int {
	if r.MaxLines == 0 {
		return DefaultTranscriptLines
	}
	return r.MaxLines
}

func (r AgentRun) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// OutputLine is one line of an agent's output as it was published on the
// bus.
type OutputLine struct {
	// At is when the adapter observed the event the line came from.
	At time.Time `json:"at"`
	// Stream is stdout for the model's own text and stderr for tool
	// activity and adapter errors.
	Stream events.Stream `json:"stream"`
	// Text is the line without its trailing newline.
	Text string `json:"line"`
}

// Attempt is one session [RunAgent] ran.
type Attempt struct {
	// Number is 1 for the first session and higher for schema retries.
	Number int `json:"number"`
	// Prompt is what the session was asked, contract suffix included.
	Prompt string `json:"prompt"`
	// Output is the final assistant text the session returned.
	Output string `json:"output,omitempty"`
	// Usage is what the session consumed, as far as the CLI reported it.
	Usage cartridge.Usage `json:"usage"`
	// Duration is the session's wall time, as the adapter reported it.
	Duration time.Duration `json:"duration,omitempty"`
	// SessionID is the CLI's own session id, when it reports one.
	SessionID string `json:"session_id,omitempty"`
	// ExitCode is the subprocess exit status, or -1 when killed or unknown.
	ExitCode int `json:"exit_code"`
	// Err is why the attempt failed: a *structured.ParseError for an
	// unusable reply, a *cartridge.Error for a failed session, nil for the
	// attempt that succeeded.
	Err error `json:"-"`
	// Failure classifies Err for the event stream; empty when Err is nil.
	Failure events.Failure `json:"failure,omitempty"`
}

// Transcript is the record of one [RunAgent] call: every attempt, every
// output line, and the totals. It is returned whether the firing succeeded
// or not, so a caller can log and store what happened either way.
type Transcript struct {
	// Run, Agent, Role, Lens, Bead and Task copy the firing's identity.
	Run   domain.RunID   `json:"run,omitempty"`
	Agent domain.AgentID `json:"agent"`
	Role  domain.Role    `json:"role,omitempty"`
	Lens  domain.Lens    `json:"lens,omitempty"`
	Bead  domain.BeadID  `json:"bead,omitempty"`
	Task  events.Task    `json:"task,omitempty"`
	// Cartridge is the adapter's registry name.
	Cartridge string `json:"cartridge"`
	// Contract is the name of the output contract the reply had to satisfy.
	Contract string `json:"contract"`
	// Enforcement is how the cartridge held the spec's tool policy.
	Enforcement Enforcement `json:"enforcement"`
	// Attempts lists every session, in order, failures included.
	Attempts []Attempt `json:"attempts"`
	// Lines holds the output lines that were published, capped by
	// AgentRun.MaxLines.
	Lines []OutputLine `json:"lines,omitempty"`
	// Dropped counts lines published on the bus but not kept in Lines.
	Dropped int `json:"dropped,omitempty"`
	// Output is the last attempt's final text (the successful one when the
	// firing succeeded).
	Output string `json:"output,omitempty"`
	// Parsed is the validated result block; zero when the firing failed.
	Parsed structured.Parsed `json:"parsed,omitempty"`
	// Usage is the sum over every attempt.
	Usage cartridge.Usage `json:"usage"`
	// Duration is wall time over every attempt, measured by RunAgent.
	Duration time.Duration `json:"duration"`
	// SessionID and ExitCode come from the last attempt.
	SessionID string `json:"session_id,omitempty"`
	ExitCode  int    `json:"exit_code"`
}

// Retries is how many extra sessions were fired after the first.
func (t Transcript) Retries() int { return max(len(t.Attempts)-1, 0) }

// Failed reports whether the last attempt ended with an error.
func (t Transcript) Failed() bool {
	if len(t.Attempts) == 0 {
		return true
	}
	return t.Attempts[len(t.Attempts)-1].Err != nil
}

// Text joins the kept output lines with newlines.
func (t Transcript) Text() string {
	parts := make([]string, len(t.Lines))
	for i, l := range t.Lines {
		parts[i] = l.Text
	}
	return strings.Join(parts, "\n")
}

// String renders the transcript on one line for a log.
func (t Transcript) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s/%s", t.Agent, t.Cartridge, t.Contract)
	if t.Task != "" {
		fmt.Fprintf(&b, " %s", t.Task)
	}
	fmt.Fprintf(&b, ": %d attempt(s), %d line(s), %d tokens, %s",
		len(t.Attempts), len(t.Lines)+t.Dropped, t.Usage.TotalTokens(), t.Duration.Round(time.Millisecond))
	if t.Failed() {
		b.WriteString(", failed")
	}
	return b.String()
}

// AgentError is what [RunAgent] returns when a firing did not yield a usable
// result. Failure is the class the orchestrator publishes in
// [events.AgentFailed]; errors.Is reaches the underlying sentinel, so
// errors.Is(err, cartridge.ErrTimeout), cartridge.ErrRateLimited,
// cartridge.ErrCreditsExhausted, cartridge.ErrCrash and
// structured.ErrSchema (and its siblings) all work.
type AgentError struct {
	// Agent, Role, Bead, Task and Cartridge identify the firing.
	Agent     domain.AgentID `json:"agent"`
	Role      domain.Role    `json:"role,omitempty"`
	Bead      domain.BeadID  `json:"bead,omitempty"`
	Task      events.Task    `json:"task,omitempty"`
	Cartridge string         `json:"cartridge"`
	// Contract is the output contract the reply had to satisfy.
	Contract string `json:"contract,omitempty"`
	// Failure classifies the failure for the event stream.
	Failure events.Failure `json:"failure"`
	// Kind is the cartridge failure kind; cartridge.KindUnknown for a
	// contract failure (Failure is then events.FailureBadOutput).
	Kind cartridge.Kind `json:"kind,omitempty"`
	// Attempts is how many sessions were fired.
	Attempts int `json:"attempts"`
	// RetryAfter is the back-off the CLI asked for, when it said.
	RetryAfter time.Duration `json:"retry_after,omitempty"`
	// ExitCode is the last attempt's exit status.
	ExitCode int `json:"exit_code"`
	// Duration is the wall time of the whole firing.
	Duration time.Duration `json:"duration,omitempty"`
	// Err is the underlying *cartridge.Error or *structured.ParseError.
	Err error `json:"-"`
}

// Error renders the failure with the agent, the cartridge and the cause.
func (e *AgentError) Error() string {
	var b strings.Builder
	b.WriteString("agents: ")
	if e.Agent != "" {
		b.WriteString(e.Agent.String())
		b.WriteString(" ")
	}
	if e.Task != "" {
		fmt.Fprintf(&b, "(%s) ", e.Task)
	}
	fmt.Fprintf(&b, "failed after %d attempt(s): %s", e.Attempts, e.Failure)
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap exposes the cause so errors.Is and errors.As reach the cartridge
// and contract sentinels.
func (e *AgentError) Unwrap() error { return e.Err }

// ParseError returns the contract failure when the reply was unusable.
func (e *AgentError) ParseError() (*structured.ParseError, bool) {
	return structured.AsParseError(e.Err)
}

// Feedback is the paragraph a retry prompt would show the model, for a
// contract failure; empty for a cartridge failure.
func (e *AgentError) Feedback() string {
	if pe, ok := e.ParseError(); ok {
		return pe.Feedback()
	}
	return ""
}

// Retryable reports whether firing the same job again can plausibly
// succeed: rate limits, crashes, timeouts and unclassified cartridge errors.
// Credits exhaustion, an auth failure and a deliberate kill are not. A
// contract failure is not retryable here either — the schema retry
// [RunAgent] owns is already spent — though the supervisor may still choose
// to re-fire the job from scratch.
func (e *AgentError) Retryable() bool {
	if e.Failure == events.FailureBadOutput {
		return false
	}
	return e.Kind.Retryable()
}

// FailureOf classifies any firing error the way the event stream does: a
// contract failure (a *structured.ParseError) is events.FailureBadOutput,
// anything else is the [events.Failure] of its cartridge kind. A nil error
// yields the empty Failure.
func FailureOf(err error) events.Failure {
	if err == nil {
		return ""
	}
	if _, ok := structured.AsParseError(err); ok {
		return events.FailureBadOutput
	}
	return cartridge.KindOf(err).Failure()
}

// RunAgent fires one cartridge session, streams the agent's output onto the
// event bus as [events.AgentOutput] events, and parses the reply into T.
//
// It is the single firing path every role shares:
//
//  1. the run is validated and the spec's tool policy is re-checked with
//     [EnforcementFor]; a policy the cartridge cannot hold refuses the
//     firing before any subprocess starts (P5-06);
//  2. the session runs through [structured.Fire], which appends the
//     contract to the prompt when the prompt builder has not already
//     (WithPromptSuffix is idempotent);
//  3. every session event is turned into output lines and published;
//  4. a reply with no valid result block is retried once (AgentRun.SchemaRetries),
//     with structured.ParseError.Feedback quoted back to the model;
//  5. the validated block is decoded into T.
//
// T is the payload type of the contract: domain.Plan, domain.ImplReport,
// domain.Verdict, [ResponseSet], domain.Arbitration or domain.Escalation.
// Use [RunMessage] to get the payload the bead comment writer stores
// (which unwraps ResponseSet into []domain.Response) without naming T.
//
// The Transcript is always returned, failure included. A firing refused
// before it started (an invalid run, an unknown message type, a tool policy
// the cartridge cannot hold) yields a plain error and an empty transcript;
// once a session has run the error is an *AgentError, whose Failure is the
// cartridge kind's (timeout, crash, rate limit, credits exhausted, killed)
// or events.FailureBadOutput for an unusable reply.
func RunAgent[T any](ctx context.Context, r AgentRun) (T, Transcript, error) {
	var zero T
	tr, err := fireAgent(ctx, r)
	if err != nil {
		return zero, tr, err
	}
	var v T
	if err := tr.Parsed.Decode(&v); err != nil {
		// The block validated, so a decode failure is a mismatch between
		// the schema and the Go type: report it as a contract failure, the
		// same shape every other unusable reply has.
		pe := &structured.ParseError{
			Reason:   structured.ReasonSchema,
			Contract: tr.Contract,
			Detail:   fmt.Sprintf("the result could not be decoded into %T", v),
			Cause:    err,
		}
		if n := len(tr.Attempts); n > 0 {
			tr.Attempts[n-1].Err, tr.Attempts[n-1].Failure = pe, events.FailureBadOutput
		}
		return zero, tr, r.agentError(tr, pe)
	}
	return v, tr, nil
}

// fireAgent runs the session and fills in the transcript; the callers decode
// the validated block into the shape they want.
func fireAgent(ctx context.Context, r AgentRun) (Transcript, error) {
	if err := r.Validate(); err != nil {
		return Transcript{}, err
	}
	contract, err := r.OutputContract()
	if err != nil {
		return Transcript{}, err
	}
	enf, err := EnforcementFor(r.Cartridge, r.Spec.ToolPolicy)
	if err != nil {
		return Transcript{}, err
	}
	if err := enf.Check(); err != nil {
		return Transcript{}, err
	}

	tr := Transcript{
		Run: r.Run, Agent: r.Agent.ID, Role: r.Agent.Role, Lens: r.Agent.Lens,
		Bead: r.Agent.Bead, Task: r.Task,
		Cartridge: r.Cartridge.Name(), Contract: contract.Name(), Enforcement: enf,
	}
	sink := &outputSink{run: r.Run, agent: r.Agent.ID, bus: r.Bus, max: r.maxLines(), now: r.now}
	observe := func(ev cartridge.SessionEvent) {
		sink.observe(ev)
		if r.Observer != nil {
			r.Observer(ev)
		}
	}
	retry := r.RetryPrompt
	if retry == nil {
		retry = structured.RetryPrompt
	}
	announce := func(rc structured.RetryContext) string {
		reason := "the previous reply had no usable result"
		if rc.Err != nil {
			reason = firstLine(rc.Err.Feedback())
		}
		sink.write(r.now(), events.StreamStderr, fmt.Sprintf("[retry %d] %s", rc.Attempt+1, reason))
		return retry(rc)
	}

	start := r.now()
	out, ferr := structured.Fire(ctx, r.Cartridge, r.Spec, contract,
		structured.WithObserver(observe),
		structured.WithMaxRetries(r.retries()),
		structured.WithRetryPrompt(announce),
	)
	sink.flush(r.now())
	tr.Duration = r.now().Sub(start)
	tr.Lines, tr.Dropped = sink.lines, sink.dropped
	tr.Usage = out.Usage
	tr.Attempts = make([]Attempt, 0, len(out.Attempts))
	for i, a := range out.Attempts {
		tr.Attempts = append(tr.Attempts, Attempt{
			Number: i + 1, Prompt: a.Prompt, Output: a.Result.Output,
			Usage: a.Result.Usage, Duration: a.Result.Duration,
			SessionID: a.Result.SessionID, ExitCode: a.Result.ExitCode,
			Err: a.Err, Failure: FailureOf(a.Err),
		})
	}
	if n := len(tr.Attempts); n > 0 {
		last := tr.Attempts[n-1]
		tr.Output, tr.SessionID, tr.ExitCode = last.Output, last.SessionID, last.ExitCode
	}
	if ferr != nil {
		return tr, r.agentError(tr, ferr)
	}
	tr.Parsed = out.Parsed
	tr.Output = out.Result.Output
	tr.SessionID, tr.ExitCode = out.Result.SessionID, out.Result.ExitCode
	return tr, nil
}

// RunMessage is [RunAgent] for a firing described by AgentRun.Message: it
// decodes the reply into the payload type the bead comment writer stores for
// that message (domain.Plan, domain.ImplReport, domain.Verdict,
// []domain.Response, domain.Arbitration or domain.Escalation), which is
// exactly what beads.Message.Payload holds. AgentRun.Contract is ignored
// here: the message type decides both the contract and the payload type.
func RunMessage(ctx context.Context, r AgentRun) (any, Transcript, error) {
	if r.Message == "" {
		return nil, Transcript{}, fmt.Errorf("%w: RunMessage needs a message type", ErrInvalidAgentRun)
	}
	// Keep the contract and the payload type in step: the message type
	// decides both.
	r.Contract = nil
	tr, err := fireAgent(ctx, r)
	if err != nil {
		return nil, tr, err
	}
	v, err := decodePayload(r.Message, tr.Parsed.JSON)
	if err != nil {
		return nil, tr, err
	}
	return v, tr, nil
}

// agentError wraps a firing failure with the identity of the agent.
func (r AgentRun) agentError(tr Transcript, err error) *AgentError {
	ae := &AgentError{
		Agent: r.Agent.ID, Role: r.Agent.Role, Bead: r.Agent.Bead, Task: r.Task,
		Cartridge: tr.Cartridge, Contract: tr.Contract,
		Failure: FailureOf(err), Attempts: len(tr.Attempts),
		ExitCode: tr.ExitCode, Duration: tr.Duration, Err: err,
	}
	if _, ok := structured.AsParseError(err); !ok {
		ae.Kind = cartridge.KindOf(err)
		var ce *cartridge.Error
		if errors.As(err, &ce) {
			ae.RetryAfter = ce.RetryAfter
		}
	}
	return ae
}

// outputSink turns session events into output lines: the model's text on
// stdout, tool activity and adapter errors on stderr. Lines are published as
// they complete and kept in memory up to max (negative keeps everything).
type outputSink struct {
	run     domain.RunID
	agent   domain.AgentID
	bus     *events.Bus
	max     int
	now     func() time.Time
	pending strings.Builder
	at      time.Time
	lines   []OutputLine
	dropped int
}

func (s *outputSink) observe(ev cartridge.SessionEvent) {
	switch ev.Kind {
	case cartridge.EventTextDelta:
		s.text(ev.At, ev.Text)
	case cartridge.EventToolUse, cartridge.EventToolResult, cartridge.EventError:
		s.flush(ev.At)
		s.write(ev.At, events.StreamStderr, cartridge.Describe(ev))
	case cartridge.EventDone:
		s.flush(ev.At)
	}
}

// text buffers a chunk of assistant text and publishes every complete line.
func (s *outputSink) text(at time.Time, chunk string) {
	if chunk == "" {
		return
	}
	if s.pending.Len() == 0 {
		s.at = at
	}
	s.pending.WriteString(chunk)
	buf := s.pending.String()
	i := strings.LastIndexByte(buf, '\n')
	if i < 0 {
		return
	}
	complete, rest := buf[:i], buf[i+1:]
	s.pending.Reset()
	s.pending.WriteString(rest)
	s.publishAll(s.at, events.StreamStdout, complete)
	s.at = at
}

// flush publishes whatever text was buffered without a trailing newline.
func (s *outputSink) flush(at time.Time) {
	if s.pending.Len() == 0 {
		return
	}
	rest := s.pending.String()
	s.pending.Reset()
	stamp := s.at
	if stamp.IsZero() {
		stamp = at
	}
	s.publishAll(stamp, events.StreamStdout, rest)
}

// write publishes text as one or more lines on stream.
func (s *outputSink) write(at time.Time, stream events.Stream, text string) {
	s.publishAll(at, stream, text)
}

func (s *outputSink) publishAll(at time.Time, stream events.Stream, text string) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		s.publish(at, stream, line)
	}
}

func (s *outputSink) publish(at time.Time, stream events.Stream, line string) {
	if at.IsZero() && s.now != nil {
		at = s.now()
	}
	if s.max < 0 || len(s.lines) < s.max {
		s.lines = append(s.lines, OutputLine{At: at, Stream: stream, Text: line})
	} else {
		s.dropped++
	}
	if s.bus == nil {
		return
	}
	s.bus.Publish(events.AgentOutput{
		Meta:   events.MetaAt(s.run, at),
		Agent:  s.agent,
		Stream: stream,
		Line:   line,
	})
}

// firstLine is the first non-empty line of s, for a one-line log message.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}
