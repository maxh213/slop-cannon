package domain

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"slop-cannon/internal/textsafe"
)

// ErrControlCharacter is wrapped by every control-character rejection, so a
// caller can tell "the planner wrote an escape sequence" from the other
// reasons a plan or a bead is refused (P0-11).
//
// The text an agent writes travels a long way: into the beads database, into
// the next agent's prompt, into the TUI's frames and into the event log. The
// TUI scrubs at render time (P7-01…P7-03) and the prompt builder fences
// agent prose (P5-01), but those are defences at the far end of the pipe.
// This is the validation at the near end: text that carries a C0 control, a
// C1 control or DEL never enters the domain model in the first place.
var ErrControlCharacter = errors.New("control character")

// Text and line rules.
//
// A "text" field may span lines, so LF and TAB are allowed and every other
// C0 control (U+0000…U+001F), DEL (U+007F) and every C1 control
// (U+0080…U+009F) is rejected. CR is rejected with the rest: on its own it
// moves the cursor to the start of the line, which is exactly how an agent
// would overwrite what a terminal has already printed, and a CRLF line
// ending is a normalisation the writer should have done.
//
// A "line" field — a title — may not span lines at all, so LF and TAB are
// rejected too.
const (
	// AllowedTextControls are the C0 controls a multi-line field may carry.
	AllowedTextControls = "\n\t"
)

// IsControl reports whether r is a C0 control, DEL or a C1 control. The
// table is [textsafe.IsControl], the one the whole binary shares (P9-10);
// what differs here is the policy above it — this package refuses such a
// rune in text an agent wrote, where a renderer scrubs it.
func IsControl(r rune) bool { return textsafe.IsControl(r) }

// CheckText reports the first control character in a multi-line field,
// naming the field and the character. LF and TAB are allowed; every other
// C0 control, DEL and every C1 control is rejected. The error wraps
// ErrControlCharacter.
func CheckText(field, s string) error { return checkControls(field, s, AllowedTextControls) }

// CheckLine reports the first control character in a single-line field such
// as a title. No control character is allowed, not even LF or TAB. The error
// wraps ErrControlCharacter.
func CheckLine(field, s string) error { return checkControls(field, s, "") }

// checkControls returns an error for the first rune of s that is a control
// character and is not listed in allowed.
func checkControls(field, s, allowed string) error {
	for i, r := range s {
		if !IsControl(r) || strings.ContainsRune(allowed, r) {
			continue
		}
		return fmt.Errorf("%s must not contain a %w (%s at offset %d)", field, ErrControlCharacter, ControlName(r), i)
	}
	return nil
}

// HasControlText reports whether CheckText would reject s.
func HasControlText(s string) bool { return CheckText("", s) != nil }

// HasControlLine reports whether CheckLine would reject s.
func HasControlLine(s string) bool { return CheckLine("", s) != nil }

// ControlName names a control character the way an error message should:
// its common abbreviation when it has one ("ESC", "NUL", "DEL"), else its
// code point ("U+0085").
func ControlName(r rune) string {
	switch r {
	case 0x00:
		return "NUL"
	case 0x07:
		return "BEL"
	case 0x08:
		return "BS"
	case 0x09:
		return "TAB"
	case 0x0a:
		return "LF"
	case 0x0b:
		return "VT"
	case 0x0c:
		return "FF"
	case 0x0d:
		return "CR"
	case 0x1b:
		return "ESC"
	case 0x7f:
		return "DEL"
	}
	return fmt.Sprintf("U+%04X", r)
}

// SanitizeText returns s with every rejected control character removed and
// invalid UTF-8 dropped, keeping LF and TAB. It is the "normalise" half of
// P0-11, for the places that must not fail — text read back out of a
// repository slop-cannon did not create, for instance — while CheckText is
// the "reject" half for text an agent has just written. A string CheckText
// accepts is returned unchanged.
func SanitizeText(s string) string { return sanitize(s, AllowedTextControls) }

// SanitizeLine returns s with every control character removed, including LF
// and TAB, and invalid UTF-8 dropped. Use it for a title.
func SanitizeLine(s string) string { return sanitize(s, "") }

// sanitize drops every control character of s that is not listed in
// allowed, along with any invalid UTF-8.
func sanitize(s, allowed string) string {
	clean := true
	for _, r := range s {
		if (IsControl(r) && !strings.ContainsRune(allowed, r)) || r == utf8.RuneError {
			clean = false
			break
		}
	}
	if clean && utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToValidUTF8(s, "") {
		if IsControl(r) && !strings.ContainsRune(allowed, r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ValidateText reports every control character in the prose fields of the
// proposed bead: Title (single line), Description and Acceptance
// (multi-line). Each error names the field it came from. Plan.Validate calls
// it for every bead, so a plan whose bead title carries "\x1b[2J" is
// rejected before it reaches bd.
func (b PlanBead) ValidateText() error {
	return errors.Join(b.textProblems("")...)
}

// textProblems returns one error per prose field of b that carries a control
// character, each prefixed with where ("bead \"retry-docs\"") when it is not
// empty.
func (b PlanBead) textProblems(where string) []error {
	return textProblems(where, [][2]string{
		{"title", b.Title},
		{"description", b.Description},
		{"acceptance criteria", b.Acceptance},
	}, map[string]bool{"title": true})
}

// Sanitized returns a copy of b with the control characters stripped out of
// its prose fields rather than rejected.
func (b PlanBead) Sanitized() PlanBead {
	b.Title = SanitizeLine(b.Title)
	b.Description = SanitizeText(b.Description)
	b.Acceptance = SanitizeText(b.Acceptance)
	return b
}

// ValidateText reports every control character in the bead's prose fields:
// Title (single line), Description and Acceptance (multi-line). Each error
// names the field, prefixed with the bead id when it has one. It is the same
// rule Plan.Validate applies to a proposed bead, for the beads a run reads
// back out of bd or builds by hand.
func (b Bead) ValidateText() error {
	where := ""
	if id := strings.TrimSpace(string(b.ID)); id != "" {
		where = "bead " + id
	}
	return errors.Join(textProblems(where, [][2]string{
		{"title", b.Title},
		{"description", b.Description},
		{"acceptance criteria", b.Acceptance},
	}, map[string]bool{"title": true})...)
}

// Sanitized returns a copy of b with the control characters stripped out of
// its prose fields rather than rejected. Use it where refusing the bead
// would strand a run — a bead written by something other than slop-cannon,
// read back out of bd — and ValidateText where the text has just been
// invented by an agent.
func (b Bead) Sanitized() Bead {
	b.Title = SanitizeLine(b.Title)
	b.Description = SanitizeText(b.Description)
	b.Acceptance = SanitizeText(b.Acceptance)
	return b
}

// textProblems checks each (field, value) pair, using CheckLine for the
// fields named in lines and CheckText for the rest, and prefixes every
// message with where when it is not empty.
func textProblems(where string, fields [][2]string, lines map[string]bool) []error {
	var errs []error
	for _, f := range fields {
		name, value := f[0], f[1]
		check := CheckText
		if lines[name] {
			check = CheckLine
		}
		if err := check(name, value); err != nil {
			if where != "" {
				err = fmt.Errorf("%s: %w", where, err)
			}
			errs = append(errs, err)
		}
	}
	return errs
}

// planTextProblems returns one error per prose field of the plan that
// carries a control character: the brief, then every bead in plan order.
// Plan.Validate appends them to its own findings, so they wrap
// ErrInvalidPlan like every other structural problem and agents.ValidatePlan
// reports them as retryable planning rules (P0-11).
func planTextProblems(p Plan) []error {
	var errs []error
	if err := CheckText("brief", p.Brief); err != nil {
		errs = append(errs, err)
	}
	for i, b := range p.Beads {
		where := fmt.Sprintf("bead %d", i)
		if key := strings.TrimSpace(b.Key); key != "" && !strings.ContainsAny(key, " \t\n") {
			where = fmt.Sprintf("bead %q", b.Key)
		}
		errs = append(errs, b.textProblems(where)...)
	}
	return errs
}
