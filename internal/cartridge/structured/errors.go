package structured

import (
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Reason classifies why an output did not yield a structured result.
type Reason string

// Parse failure reasons, each with a sentinel for errors.Is.
const (
	// ReasonMissing: the output has no sc-result block and no JSON object the
	// fallbacks could use.
	ReasonMissing Reason = "missing"
	// ReasonTruncated: an sc-result block was opened but the output ended
	// before its JSON was complete (the model ran out of turns or tokens).
	ReasonTruncated Reason = "truncated"
	// ReasonInvalidJSON: the block closed but its body is not one valid JSON
	// value.
	ReasonInvalidJSON Reason = "invalid_json"
	// ReasonSchema: the JSON parsed but violates the contract's schema.
	ReasonSchema Reason = "schema"
)

// Sentinel errors, one per Reason. Every *ParseError matches the sentinel of
// its Reason.
var (
	// ErrMissing: no sc-result block in the output.
	ErrMissing = errors.New("structured: no sc-result block in output")
	// ErrTruncated: the block never closed and its JSON is incomplete.
	ErrTruncated = errors.New("structured: sc-result block is truncated")
	// ErrInvalidJSON: the block body is not valid JSON.
	ErrInvalidJSON = errors.New("structured: sc-result block is not valid JSON")
	// ErrSchema: the JSON does not match the contract's schema.
	ErrSchema = errors.New("structured: result does not match schema")
	// ErrUnknownContract: a Registry has no contract of that name.
	ErrUnknownContract = errors.New("structured: unknown contract")
)

var reasons = []Reason{ReasonMissing, ReasonTruncated, ReasonInvalidJSON, ReasonSchema}

// Reasons returns every parse failure reason.
func Reasons() []Reason { return append([]Reason(nil), reasons...) }

// String returns the canonical name.
func (r Reason) String() string { return string(r) }

// Valid reports whether r is a declared reason.
func (r Reason) Valid() bool {
	for _, x := range reasons {
		if x == r {
			return true
		}
	}
	return false
}

// Sentinel returns the errors.Is target for the reason (nil when unknown).
func (r Reason) Sentinel() error {
	switch r {
	case ReasonMissing:
		return ErrMissing
	case ReasonTruncated:
		return ErrTruncated
	case ReasonInvalidJSON:
		return ErrInvalidJSON
	case ReasonSchema:
		return ErrSchema
	default:
		return nil
	}
}

// ParseError says why an output did not yield a valid result. It is what
// Extract, Contract.Validate, Contract.Parse and Fire return for
// contract-level failures; Feedback renders it for the model on retry.
type ParseError struct {
	// Reason classifies the failure.
	Reason Reason
	// Contract is the schema name, when the failure happened at or after
	// validation; empty for extraction failures.
	Contract string
	// Detail is a one-line summary a human can read in a log.
	Detail string
	// Problems lists schema violations, one per entry, as "at '/path':
	// message". Empty unless Reason is ReasonSchema.
	Problems []string
	// Cause is the underlying decoder or validator error, may be nil. Its
	// words are never lost: [ParseError.Error] appends them whenever they
	// are not already in Detail or Problems (P9-31), so a wrapped sentinel
	// such as a caller's own rejection error is readable in a log line and
	// not only matchable with errors.Is.
	Cause error
}

// Error renders the sentence of the Reason's sentinel — the same words
// docs/troubleshooting.md is keyed by — followed by the contract, the
// detail and the problems, one per line: "structured: sc-result block is
// not valid JSON (impl_report): invalid character 'h'". A Reason with no
// sentinel falls back to "structured: <reason>".
//
// A Cause whose message is not already spelled out by Detail or Problems is
// appended last as a "cause:" line (P9-31); one that only repeats them is
// left out, so the common case — Detail built from the cause's own text —
// is not printed twice.
func (e *ParseError) Error() string {
	var b strings.Builder
	if s := e.Reason.Sentinel(); s != nil {
		b.WriteString(s.Error())
	} else {
		b.WriteString("structured: ")
		b.WriteString(e.Reason.String())
	}
	if e.Contract != "" {
		fmt.Fprintf(&b, " (%s)", e.Contract)
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	for _, p := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(p)
	}
	if cause := e.causeWords(b.String()); cause != "" {
		lines := strings.Split(cause, "\n")
		b.WriteString("\n  cause: ")
		b.WriteString(lines[0])
		for _, l := range lines[1:] {
			b.WriteString("\n    ")
			b.WriteString(l)
		}
	}
	return b.String()
}

// causeWords returns the Cause's message when it carries words rendered
// does not already contain, and "" when printing it would only repeat them.
// The validator's own error is always left out: Problems is exactly its
// leaves (see leafProblems), so repeating it would double every violation
// and add the internal schema URL to a user-facing line.
func (e *ParseError) causeWords(rendered string) string {
	if e.Cause == nil {
		return ""
	}
	var ve *jsonschema.ValidationError
	if errors.As(e.Cause, &ve) {
		return ""
	}
	msg := strings.TrimSpace(e.Cause.Error())
	if msg == "" {
		return ""
	}
	for _, line := range strings.Split(msg, "\n") {
		if l := strings.TrimSpace(line); l != "" && !strings.Contains(rendered, l) {
			return msg
		}
	}
	return ""
}

// Unwrap exposes the cause.
func (e *ParseError) Unwrap() error { return e.Cause }

// Is matches the sentinel of e's Reason.
func (e *ParseError) Is(target error) bool {
	s := e.Reason.Sentinel()
	return s != nil && target == s
}

// Feedback renders the failure as the paragraph a retry prompt shows the
// model: what was wrong, in its own terms, without Go error prefixes.
func (e *ParseError) Feedback() string {
	var b strings.Builder
	switch e.Reason {
	case ReasonMissing:
		b.WriteString("it contained no ```sc-result fenced block (and no JSON object that could stand in for one)")
	case ReasonTruncated:
		b.WriteString("its ```sc-result block was cut off before the JSON was complete")
	case ReasonInvalidJSON:
		b.WriteString("its ```sc-result block did not contain valid JSON")
	case ReasonSchema:
		if e.Contract != "" {
			fmt.Fprintf(&b, "its JSON did not match the %q schema", e.Contract)
		} else {
			b.WriteString("its JSON did not match the required schema")
		}
	default:
		b.WriteString("it did not contain a usable result")
	}
	if e.Detail != "" && e.Reason != ReasonSchema {
		b.WriteString(" (")
		b.WriteString(e.Detail)
		b.WriteString(")")
	}
	if len(e.Problems) > 0 {
		b.WriteString(":")
		for _, p := range e.Problems {
			b.WriteString("\n- ")
			b.WriteString(p)
		}
	}
	return b.String()
}

// AsParseError returns err as a *ParseError when it is or wraps one.
func AsParseError(err error) (*ParseError, bool) {
	var pe *ParseError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}
