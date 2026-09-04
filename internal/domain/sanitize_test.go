package domain_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
)

// clearScreen is the escape sequence P0-11 names: an agent that writes it
// into a bead title wipes the terminal of anything that renders the title.
const clearScreen = "\x1b[2J"

func TestCheckTextAllowsNewlinesAndTabs(t *testing.T) {
	t.Parallel()
	require.NoError(t, domain.CheckText("description", "line one\n\tline two\n"))
	require.NoError(t, domain.CheckText("description", ""))
	require.NoError(t, domain.CheckText("description", "émoji 🎯 and “quotes”"))
	assert.False(t, domain.HasControlText("plain prose"))
}

func TestCheckTextRejectsControls(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"escape", "before" + clearScreen + "after", "ESC at offset 6"},
		{"nul", "a\x00b", "NUL at offset 1"},
		{"bell", "ding\x07", "BEL at offset 4"},
		{"carriage return", "overwrite\rme", "CR at offset 9"},
		{"delete", "a\x7fb", "DEL at offset 1"},
		{"c1 next line", "a\u0085b", "U+0085 at offset 1"},
		{"backspace", "a\bb", "BS at offset 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := domain.CheckText("description", tc.in)
			require.Error(t, err)
			require.ErrorIs(t, err, domain.ErrControlCharacter)
			assert.Contains(t, err.Error(), "description must not contain")
			assert.Contains(t, err.Error(), tc.want)
			assert.True(t, domain.HasControlText(tc.in))
		})
	}
}

func TestCheckLineRejectsNewlinesAndTabs(t *testing.T) {
	t.Parallel()
	err := domain.CheckLine("title", "one\ntwo")
	require.ErrorIs(t, err, domain.ErrControlCharacter)
	assert.Contains(t, err.Error(), "title must not contain")
	assert.Contains(t, err.Error(), "LF at offset 3")
	require.ErrorIs(t, domain.CheckLine("title", "one\ttwo"), domain.ErrControlCharacter)
	require.NoError(t, domain.CheckLine("title", "one two"))
	assert.True(t, domain.HasControlLine("one\ntwo"))
	assert.False(t, domain.HasControlLine("one two"))
}

func TestSanitizeStripsControls(t *testing.T) {
	t.Parallel()
	// Only the control byte is dropped; the printable tail of the sequence
	// survives as harmless text.
	assert.Equal(t, "before[2Jafter", domain.SanitizeText("before"+clearScreen+"after"))
	assert.Equal(t, "keep\nthe\tlayout", domain.SanitizeText("keep\nthe\tlayout"))
	assert.Equal(t, "keepthelayout", domain.SanitizeLine("keep\nthe\tlayout"))
	assert.Equal(t, "ab", domain.SanitizeText("a\x00b"))
	assert.Equal(t, "ab", domain.SanitizeText("a\x80b"))

	// Invalid UTF-8 is dropped, and clean text is returned unchanged.
	assert.Equal(t, "ok", domain.SanitizeText("ok\xff"))
	clean := "already clean\n"
	assert.Equal(t, clean, domain.SanitizeText(clean))
}

func TestControlNameFallsBackToCodePoint(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "ESC", domain.ControlName(0x1b))
	assert.Equal(t, "U+0001", domain.ControlName(0x01))
	assert.Equal(t, "U+009B", domain.ControlName(0x9b))
	assert.True(t, domain.IsControl(0x1b))
	assert.False(t, domain.IsControl('a'))
}

// TestPlanValidateRejectsAnEscapeSequenceInATitle is the P0-11 acceptance
// criterion: a plan whose bead title carries "\x1b[2J" fails Plan.Validate,
// and the message names the offending field.
func TestPlanValidateRejectsAnEscapeSequenceInATitle(t *testing.T) {
	t.Parallel()
	p := domain.Plan{Beads: []domain.PlanBead{{
		Key:        "wipe",
		Title:      "Add a cache" + clearScreen,
		Acceptance: "The cache is covered by a test.",
		Priority:   domain.PriorityDefault,
	}}}
	err := p.Validate()
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrInvalidPlan)
	assert.ErrorIs(t, err, domain.ErrControlCharacter)
	assert.Contains(t, err.Error(), `bead "wipe": title must not contain`)
	assert.Contains(t, err.Error(), "ESC")
	// The escape itself must not be echoed back in the message.
	assert.NotContains(t, err.Error(), "\x1b")
}

func TestPlanValidateChecksEveryProseField(t *testing.T) {
	t.Parallel()
	p := domain.Plan{
		Brief: "Ship it\x07",
		Beads: []domain.PlanBead{{
			Key:         "b1",
			Title:       "Fine title",
			Description: "Sneaky\x1bdescription",
			Acceptance:  "Sneaky\x00acceptance",
			Priority:    domain.PriorityDefault,
		}},
	}
	err := p.Validate()
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "brief must not contain")
	assert.Contains(t, msg, `bead "b1": description must not contain`)
	assert.Contains(t, msg, `bead "b1": acceptance criteria must not contain`)
	assert.Equal(t, 3, strings.Count(msg, "must not contain"), msg)
}

// A bead whose key is unusable is still reported by position, so the message
// is never silently attached to the wrong bead.
func TestPlanValidateNamesABeadWithoutAKeyByPosition(t *testing.T) {
	t.Parallel()
	p := domain.Plan{Beads: []domain.PlanBead{{
		Title:      "Bad\x1btitle",
		Acceptance: "ok",
		Priority:   domain.PriorityDefault,
	}}}
	err := p.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bead 0: title must not contain")
}

func TestPlanValidateAcceptsCleanProse(t *testing.T) {
	t.Parallel()
	p := domain.Plan{
		Brief: "Make the retry path testable.",
		Beads: []domain.PlanBead{{
			Key:         "retry",
			Title:       "Add exponential backoff",
			Description: "Multi-line\nprose\twith a tab is fine.",
			Acceptance:  "A unit test proves the delays.",
			Priority:    domain.PriorityDefault,
		}},
	}
	require.NoError(t, p.Validate())
}

func TestPlanBeadValidateAndSanitizeText(t *testing.T) {
	t.Parallel()
	b := domain.PlanBead{Title: "Wipe" + clearScreen, Description: "a\x00b", Acceptance: "fine"}
	err := b.ValidateText()
	require.ErrorIs(t, err, domain.ErrControlCharacter)
	assert.Contains(t, err.Error(), "title must not contain")
	assert.Contains(t, err.Error(), "description must not contain")

	clean := b.Sanitized()
	assert.Equal(t, "Wipe[2J", clean.Title)
	assert.Equal(t, "ab", clean.Description)
	require.NoError(t, clean.ValidateText())
}

func TestBeadValidateAndSanitizeText(t *testing.T) {
	t.Parallel()
	b := domain.Bead{ID: "sc-7a1", Title: "Wipe" + clearScreen, Acceptance: "a\rb"}
	err := b.ValidateText()
	require.ErrorIs(t, err, domain.ErrControlCharacter)
	assert.Contains(t, err.Error(), "bead sc-7a1: title must not contain")
	assert.Contains(t, err.Error(), "bead sc-7a1: acceptance criteria must not contain")

	clean := b.Sanitized()
	require.NoError(t, clean.ValidateText())
	assert.Equal(t, "ab", clean.Acceptance)

	// No id: the message is still usable.
	anon := domain.Bead{Title: "x\x1by"}
	require.ErrorIs(t, anon.ValidateText(), domain.ErrControlCharacter)
	assert.Contains(t, anon.ValidateText().Error(), "title must not contain")

	require.NoError(t, domain.Bead{ID: "sc-1", Title: "Clean", Description: "line\nline"}.ValidateText())
}
