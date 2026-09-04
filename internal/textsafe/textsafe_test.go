package textsafe_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/textsafe"
)

// TestScrubTable is the table P9-10 asks for: one case per shape of escape
// sequence and control character, under both policies, so the single scanner
// behind `sc status`, the headless renderer and the TUI is pinned in one
// place.
func TestScrubTable(t *testing.T) {
	cases := []struct {
		name         string
		in           string
		line, screen string
	}{
		{"clean text is untouched", "plain text", "plain text", "plain text"},
		{"csi colour", "\x1b[31mred\x1b[0m alert", "red alert", "red alert"},
		{"csi clear and home", "\x1b[2J\x1b[Hgone", "gone", "gone"},
		{"csi with parameters and intermediates", "\x1b[1;41m spoofed \x1b[0m", " spoofed ", " spoofed "},
		{"osc terminated by bel", "\x1b]0;window title\x07titled", "titled", "titled"},
		{"osc terminated by st", "\x1b]8;;https://example.test\x1b\\link", "link", "link"},
		{"truncated csi eats the rest", "keep\x1b[38;5", "keep", "keep"},
		{"truncated osc eats the rest", "keep\x1b]0;no terminator", "keep", "keep"},
		{"lone escape at the end", "keep\x1b", "keep", "keep"},
		{"two-character escape", "a\x1bMb", "ab", "ab"},
		{"two-character escape over a multibyte rune", "a\x1béb", "ab", "ab"},
		{"carriage return", "done\rrewritten", "donerewritten", "rewritten"},
		{"trailing carriage return wrote nothing", "progress 100%\r", "progress 100%", "progress 100%"},
		{"carriage return before a tab", "old\r\tnew", "old new", "    new"},
		{"repeated carriage returns", "a\rb\rc", "abc", "c"},
		{"tab", "tabs\tbetween\twords", "tabs between words", "tabs    between    words"},
		{"c0 controls", "\x07bell\x08back", "bellback", "bellback"},
		{"del", "de\x7fl", "del", "del"},
		{"c1 csi", "\u009b31mnot a colour", "31mnot a colour", "31mnot a colour"},
		{"c1 as a raw latin-1 byte", "\x9b31mnot a colour", "31mnot a colour", "31mnot a colour"},
		{"newline is a control", "one\ntwo", "onetwo", "onetwo"},
		{"printable unicode survives", "héllo ▶ 世界", "héllo ▶ 世界", "héllo ▶ 世界"},
		{"empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.line, textsafe.ScrubLine(tc.in), "line policy")
			require.Equal(t, tc.screen, textsafe.ScrubDisplay(tc.in), "display policy")
			for _, got := range []string{textsafe.ScrubLine(tc.in), textsafe.ScrubDisplay(tc.in)} {
				require.False(t, textsafe.ContainsControl(got), "a control character survived: %q", got)
				require.True(t, utf8.ValidString(got), "the result is not valid UTF-8: %q", got)
			}
		})
	}
}

// TestScrubIsIdempotent: a second scrub downstream (the TUI scrubbing what
// the publisher already scrubbed, say) must not change the text again.
func TestScrubIsIdempotent(t *testing.T) {
	for _, in := range []string{
		"\x1b[2J\x1b[H\x1b[31mBOOM\x1b[0m done\rrewritten",
		"tabs\tand\ta\x07bell",
		"plain",
	} {
		for _, p := range []textsafe.Policy{textsafe.LinePolicy(), textsafe.DisplayPolicy()} {
			once := textsafe.Scrub(in, p)
			require.Equal(t, once, textsafe.Scrub(once, p), "scrubbing %q twice differs", in)
		}
	}
}

// TestScrubDoesNotAllocateForCleanText: the guard is what keeps a benign
// line — every line of a well-behaved cartridge — down to one scan.
func TestScrubDoesNotAllocateForCleanText(t *testing.T) {
	in := strings.Repeat("clean text ", 50) + "héllo 世界"
	allocs := testing.AllocsPerRun(100, func() { _ = textsafe.ScrubLine(in) })
	require.Zero(t, allocs, "a clean string was rebuilt")
}

func TestScrubBlockKeepsLineStructure(t *testing.T) {
	require.Equal(t, "a\nb\n", textsafe.ScrubBlock("a\nb\n", textsafe.LinePolicy()), "clean text is unchanged")
	require.Equal(t, "sc-1  title\n  red\n",
		textsafe.ScrubBlock("sc-1  \x1b[2Jtitle\n  \x1b[31mred\x1b[0m\n", textsafe.LinePolicy()))
	require.Equal(t, "\n\n", textsafe.ScrubBlock("\r\n\x07\n", textsafe.LinePolicy()),
		"a line that was nothing but controls is emptied, not removed")
	require.Equal(t, "one\ntwo", textsafe.ScrubBlock("one\ntwo", textsafe.DisplayPolicy()))
}

func TestFlattenMakesOneLine(t *testing.T) {
	require.Equal(t, "one two", textsafe.Flatten("one\ntwo", textsafe.LinePolicy()))
	require.Equal(t, "one two", textsafe.Flatten("one\r\ntwo", textsafe.LinePolicy()),
		"a CRLF is one break, not a break and an erase")
	require.Equal(t, "carriage return", textsafe.Flatten("carriage\rreturn", textsafe.DisplayPolicy()),
		"a break becomes a space before the carriage return can overwrite the row")
	require.Equal(t, "plain", textsafe.Flatten("plain", textsafe.LinePolicy()))
}

func TestEscapeLen(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"not an escape", "abc", 0},
		{"empty", "", 0},
		{"lone escape", "\x1b", 1},
		{"csi", "\x1b[0m tail", 4},
		{"csi with parameters", "\x1b[1;41m", 7},
		{"truncated csi", "\x1b[38;5", 6},
		{"osc bel", "\x1b]0;title\x07tail", 10},
		{"osc st", "\x1b]0;title\x1b\\tail", 11},
		{"truncated osc", "\x1b]0;title", 9},
		{"two character", "\x1bMtail", 2},
		{"two character over a multibyte rune", "\x1bétail", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, textsafe.EscapeLen(tc.in))
		})
	}
}

func TestIsControl(t *testing.T) {
	for _, r := range []rune{0x00, '\a', '\b', '\t', '\n', '\r', 0x1b, 0x1f, 0x7f, 0x80, 0x9b, 0x9f} {
		require.True(t, textsafe.IsControl(r), "%U is a control character", r)
	}
	for _, r := range []rune{' ', 'a', '~', 0xa0, '·', '▶', '世'} {
		require.False(t, textsafe.IsControl(r), "%U is printable text", r)
	}
	require.True(t, textsafe.ContainsControl("a\x1bb"))
	require.False(t, textsafe.ContainsControl("ab"))
	require.False(t, textsafe.ContainsControlOutsideNewline("a\nb"))
	require.True(t, textsafe.ContainsControlOutsideNewline("a\n\x07b"))
}

// TestScrubDropsInvalidUTF8: a cartridge can cut a multibyte rune in half at
// a read boundary; the scrubbed line still has to be text of a known width.
func TestScrubDropsInvalidUTF8(t *testing.T) {
	got := textsafe.ScrubLine("a\xffb\x07")
	require.True(t, utf8.ValidString(got))
	require.Equal(t, "ab", got, "the half rune is dropped, not printed as a replacement character")
}
