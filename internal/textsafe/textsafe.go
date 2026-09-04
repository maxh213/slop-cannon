// Package textsafe scrubs text another process wrote before sc shows it to
// anyone. A cartridge is an LLM CLI, bd and git are CLIs too, and a plan, a
// verdict or a bead title is written by a model: every one of them can put
// an ANSI escape sequence, a carriage return, a bell or a stray C1 byte into
// a string that sc then prints to a terminal, draws into a Bubble Tea frame
// or appends to a log line. An escape can clear the screen, repaint the
// frame, retitle the window or hide the cursor, and Lip Gloss counts every
// one of those bytes as zero width, so nothing downstream notices that a row
// no longer fits.
//
// This package holds the one scanner behind every such defence (P9-10): the
// table of characters a terminal reads as an instruction ([IsControl]) and
// the escape-sequence scanner ([EscapeLen]) that walks CSI sequences, OSC
// strings terminated by BEL or ST, and the two-character escapes. What the
// callers legitimately differ on is kept in [Policy]: `sc status` and the
// headless renderer want one flat line, so a tab becomes one space and a
// carriage return is dropped, while the TUI honours a carriage return the
// way a terminal does — a progress line that rewrites itself lands as its
// final state — and expands a tab to four columns.
//
// Scrubbing is idempotent and leaves text that carries no control character
// untouched, without allocating, so a benign line pays for one scan and a
// second scrub downstream changes nothing. The package imports only the
// standard library.
package textsafe

import (
	"strings"
	"unicode/utf8"
)

// The bytes the scanner names.
const (
	esc = 0x1b // ESC, the first byte of every escape sequence
	bel = 0x07 // BEL, one of the two OSC string terminators
	del = 0x7f // DEL
)

// Policy is the per-caller half of a scrub. Everything about which bytes are
// dangerous is shared; only these two questions have a legitimate answer per
// destination.
type Policy struct {
	// Tab is what one horizontal tab becomes. An empty string drops it.
	Tab string
	// ApplyCR honours a carriage return the way a terminal does: what was
	// written after the last one wins, and a carriage return with nothing
	// printable after it wrote nothing and is ignored. With ApplyCR false a
	// carriage return is dropped like any other control character, which is
	// what a single log line wants — the text before it is the line's own
	// prefix, not something the subprocess may erase.
	ApplyCR bool
}

// LinePolicy is the policy for text going into one line of a log or a
// report: `sc status`, the headless renderer, a run-log line. A tab becomes
// one space and a carriage return is dropped.
func LinePolicy() Policy { return Policy{Tab: " "} }

// DisplayPolicy is the policy for text drawn into a terminal frame: the
// TUI's transcript, log rows and detail panes. A tab becomes four spaces and
// a carriage return is honoured.
func DisplayPolicy() Policy { return Policy{Tab: "    ", ApplyCR: true} }

// IsControl reports whether r is read by a terminal as an instruction rather
// than as text: a C0 control (ESC, CR, LF and tab included), DEL, or a C1
// control (U+0080…U+009F, which includes the single-byte CSI).
func IsControl(r rune) bool {
	return r < 0x20 || r == del || (r >= 0x80 && r <= 0x9f)
}

// ContainsControl reports whether s holds at least one character [IsControl]
// names. It is the cheap guard [Scrub] uses before it walks a string.
func ContainsControl(s string) bool { return strings.ContainsFunc(s, IsControl) }

// ContainsControlOutsideNewline is [ContainsControl] for a multi-line block,
// whose own line breaks are structure rather than an instruction.
func ContainsControlOutsideNewline(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r != '\n' && IsControl(r) })
}

// Scrub returns s with every terminal instruction removed under p: escape
// sequences go whole, a tab becomes p.Tab, a carriage return is honoured or
// dropped as p says, and every other C0 control, DEL and C1 control is
// dropped. A line break is a control character like any other, so scrub a
// multi-line block with [ScrubBlock] — or replace the breaks first — rather
// than letting Scrub run two words together.
//
// A string with nothing to scrub is returned unchanged and unallocated.
// Bytes that are not valid UTF-8 — a cartridge can cut a multibyte rune in
// half at a read boundary, and a C1 control written as a raw Latin-1 byte is
// invalid too — are dropped, so the result is always printable text whose
// display width is known.
func Scrub(s string, p Policy) string {
	if !needsScrub(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	// overwrite records a carriage return whose overwrite has not happened
	// yet: it takes effect when something printable follows, so a trailing
	// carriage return (which wrote nothing after itself) keeps the line.
	overwrite := false
	write := func(text string) {
		if overwrite {
			b.Reset()
			overwrite = false
		}
		b.WriteString(text)
	}
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == esc:
			i += EscapeLen(s[i:])
		case c == '\t':
			if p.Tab != "" {
				write(p.Tab)
			}
			i++
		case c == '\r' && p.ApplyCR:
			overwrite = true
			i++
		case c < 0x20 || c == del:
			// A C0 control or DEL: an instruction, never text.
			i++
		case c < utf8.RuneSelf:
			write(s[i : i+1])
			i++
		default:
			r, n := utf8.DecodeRuneInString(s[i:])
			switch {
			case r == utf8.RuneError && n == 1:
				// Not valid UTF-8: a half rune, or a raw C1 byte.
			case r >= 0x80 && r <= 0x9f:
				// A C1 control: the single-byte CSI and friends.
			default:
				write(s[i : i+n])
			}
			i += n
		}
	}
	return b.String()
}

// needsScrub reports whether Scrub would change s: it is [ContainsControl]
// plus the bytes that are not valid UTF-8, which the rune-based test cannot
// see (they decode as U+FFFD, which is printable text). One byte scan, and
// non-ASCII text pays only for decoding its own runes.
func needsScrub(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == del {
			return true
		}
		if c >= utf8.RuneSelf {
			r, n := utf8.DecodeRuneInString(s[i:])
			if (r == utf8.RuneError && n == 1) || (r >= 0x80 && r <= 0x9f) {
				return true
			}
			i += n - 1
		}
	}
	return false
}

// ScrubLine scrubs s under [LinePolicy].
func ScrubLine(s string) string { return Scrub(s, LinePolicy()) }

// ScrubDisplay scrubs s under [DisplayPolicy].
func ScrubDisplay(s string) string { return Scrub(s, DisplayPolicy()) }

// ScrubBlock scrubs a multi-line string line by line, keeping the line
// structure its author wrote: every line is [Scrub] under p, and the
// newlines between them survive. It is what a report, a timeline or a
// description wants, where the paragraph breaks are meaning rather than an
// instruction to the terminal.
func ScrubBlock(s string, p Policy) string {
	if !ContainsControlOutsideNewline(s) && utf8.ValidString(s) {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = Scrub(line, p)
	}
	return strings.Join(lines, "\n")
}

// Flatten scrubs s into a single line under p: line breaks become spaces
// first — a row is one row, and a log line is one line — and the rest is
// [Scrub]. It is what a renderer with a one-event-per-line contract needs.
func Flatten(s string, p Policy) string {
	if !needsScrub(s) {
		return s
	}
	return Scrub(lineBreaks.Replace(s), p)
}

// lineBreaks turns the two line-break characters into spaces, bytewise, so
// [Flatten] leaves the rest of the string — invalid UTF-8 included — exactly
// as [Scrub] will see it.
// A CRLF is one break, so it is listed first: a Replacer prefers the pair
// that appears earliest among those matching at a position.
var lineBreaks = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")

// EscapeLen returns how many bytes the escape sequence at the start of s
// occupies: a CSI (ESC [) up to and including its final byte, an OSC (ESC ])
// up to its BEL or ST terminator, and otherwise the ESC and the whole rune
// after it. A truncated sequence consumes the rest of s, and a lone ESC at
// the end consumes itself, so a caller stepping by EscapeLen always makes
// progress. It returns 0 when s does not start with an ESC.
func EscapeLen(s string) int {
	if len(s) == 0 || s[0] != esc {
		return 0
	}
	if len(s) < 2 {
		return 1
	}
	switch s[1] {
	case '[': // CSI: parameter and intermediate bytes, then a final byte.
		for i := 2; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				return i + 1
			}
		}
		return len(s)
	case ']': // OSC: a string terminated by BEL or by ST (ESC \).
		for i := 2; i < len(s); i++ {
			if s[i] == bel {
				return i + 1
			}
			if s[i] == esc && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
		}
		return len(s)
	default: // A two-character escape: ESC and one whole rune.
		_, n := utf8.DecodeRuneInString(s[1:])
		return 1 + n
	}
}
