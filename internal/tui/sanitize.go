package tui

import (
	"strings"

	"slop-cannon/internal/textsafe"
)

// The TUI draws text nobody in this repository wrote: an agent's output line
// comes off a cartridge's stdout, a bead title and a verdict's prose come out
// of a model through bd, a pull request URL comes from gh. Every one of them
// can carry an ANSI escape sequence, a carriage return or another control
// character, and Lip Gloss counts all of those as zero width — so one such
// line would repaint or erase the frame, move the cursor out of its row, or
// shift a card's border, and nothing downstream would notice the row no
// longer fits.
//
// The scanner behind these helpers lives in internal/textsafe, which is the
// one implementation for the whole binary (P9-10): the TUI, `sc status` and
// the headless renderer differ only in the policy they pass it. Publishers
// scrub at construction time too (logging.SafeEvent, P7-17), so for events
// built by this build these calls are a second, idempotent pass; they stay
// because a view must be safe to point at any event, whoever built it.

// agentsSanitize makes one line of subprocess output safe to draw in a
// frame: escape sequences go, a carriage return is honoured the way a
// terminal honours it (what was written after the last one wins), tabs
// become four spaces and every remaining control character is dropped. A
// clean line is returned unchanged and unallocated.
func agentsSanitize(s string) string { return textsafe.ScrubDisplay(s) }

// rowText makes one piece of untrusted text safe to draw on a single row: a
// line break becomes a space, because a row is one row and a stray newline
// would silently take a second body row that is drawn without the view's
// gutter, and the rest is [agentsSanitize].
func rowText(s string) string { return textsafe.Flatten(s, textsafe.DisplayPolicy()) }

// blockText makes a multi-line field safe to draw while keeping the
// paragraph breaks its author wrote — a bead's description or acceptance
// criteria, which the detail panel wraps itself. Every line is as safe as
// [rowText] makes one; the newlines between them survive.
func blockText(s string) string { return textsafe.ScrubBlock(s, textsafe.DisplayPolicy()) }

// headlessText makes one piece of untrusted text safe to write to a terminal
// as part of a headless log line (P7-18). The renderer promises one event per
// line, so a newline becomes a space; a carriage return is dropped rather
// than honoured or spaced, because the text before it is the line's own clock
// stamp and glyph, which a cartridge may not erase, and because `sc status`
// drops it too — the same run log has to read the same either way; and a tab
// becomes one space, again as it does in `sc status`.
func headlessText(s string) string {
	return textsafe.ScrubLine(strings.ReplaceAll(s, "\n", " "))
}
