package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stringEnum is the surface every enum in this package must offer.
type stringEnum interface {
	~string
	String() string
	Valid() bool
	MarshalText() ([]byte, error)
}

// enumCase describes one enum for the generic table test below.
type enumCase[T stringEnum] struct {
	name  string
	all   func() []T
	parse func(string) (T, error)
	want  []string // canonical spellings in declaration order
}

// checkEnum exercises String, Parse (canonical, lenient and invalid inputs),
// Valid, and JSON marshal/unmarshal for every value of one enum, plus the
// zero-value and invalid-value edge cases of the text codec.
func checkEnum[T stringEnum, PT interface {
	*T
	UnmarshalText([]byte) error
}](t *testing.T, c enumCase[T]) {
	t.Helper()
	t.Run(c.name, func(t *testing.T) {
		values := c.all()
		require.Len(t, values, len(c.want), "declared value count")

		for i, v := range values {
			want := c.want[i]
			t.Run(want, func(t *testing.T) {
				assert.Equal(t, want, v.String(), "String")
				assert.True(t, v.Valid(), "Valid")

				got, err := c.parse(want)
				require.NoError(t, err)
				assert.Equal(t, v, got, "Parse canonical")

				lenient := "  " + strings.ToUpper(strings.ReplaceAll(want, "_", "-")) + "\n"
				got, err = c.parse(lenient)
				require.NoError(t, err, "lenient input %q", lenient)
				assert.Equal(t, v, got, "Parse lenient")

				// JSON round trip as a bare value.
				data, err := json.Marshal(v)
				require.NoError(t, err)
				assert.Equal(t, fmt.Sprintf("%q", want), string(data), "JSON encoding")
				var back T
				require.NoError(t, json.Unmarshal(data, PT(&back)))
				assert.Equal(t, v, back, "JSON round trip")

				// Lenient spelling is also accepted when decoding.
				var lenientBack T
				require.NoError(t, json.Unmarshal([]byte(fmt.Sprintf("%q", lenient)), PT(&lenientBack)))
				assert.Equal(t, v, lenientBack, "JSON lenient decoding")

				// Enums work as JSON object keys too (TextMarshaler).
				keyed, err := json.Marshal(map[T]int{v: 1})
				require.NoError(t, err)
				var keyedBack map[T]int
				require.NoError(t, json.Unmarshal(keyed, &keyedBack))
				assert.Equal(t, 1, keyedBack[v], "map key round trip")
			})
		}

		t.Run("all returns a copy", func(t *testing.T) {
			a := c.all()
			a[0] = T("clobbered")
			assert.Equal(t, T(c.want[0]), c.all()[0])
		})

		t.Run("invalid", func(t *testing.T) {
			for _, bad := range []string{"", "   ", "bogus", c.want[0] + "x"} {
				_, err := c.parse(bad)
				require.Error(t, err, "Parse(%q)", bad)
				assert.ErrorIs(t, err, ErrUnknownValue)
				assert.Contains(t, err.Error(), c.want[0], "error lists the accepted values")
			}
			var zero T
			assert.False(t, zero.Valid(), "zero value is not Valid")
			assert.False(t, T("bogus").Valid())

			var back T
			err := json.Unmarshal([]byte(`"bogus"`), PT(&back))
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrUnknownValue)

			_, err = json.Marshal(T("bogus"))
			require.Error(t, err, "marshalling an invalid value fails")
			assert.True(t, errors.Is(err, ErrUnknownValue) || strings.Contains(err.Error(), "unknown value"))
		})

		t.Run("zero value round trips as empty string", func(t *testing.T) {
			var zero T
			data, err := json.Marshal(zero)
			require.NoError(t, err)
			assert.Equal(t, `""`, string(data))
			var back T
			require.NoError(t, json.Unmarshal([]byte(`""`), PT(&back)))
			assert.Equal(t, zero, back)
			require.NoError(t, json.Unmarshal([]byte(`"  "`), PT(&back)))
			assert.Equal(t, zero, back)
		})
	})
}

func TestEnums(t *testing.T) {
	checkEnum[Role, *Role](t, enumCase[Role]{
		name: "Role", all: Roles, parse: ParseRole,
		want: []string{"po", "engineer", "tester"},
	})
	checkEnum[Lens, *Lens](t, enumCase[Lens]{
		name: "Lens", all: Lenses, parse: ParseLens,
		want: []string{"ux", "qa", "test"},
	})
	checkEnum[BeadState, *BeadState](t, enumCase[BeadState]{
		name: "BeadState", all: BeadStates, parse: ParseBeadState,
		want: []string{"open", "in_progress", "in_review", "changes_requested", "approved", "delivering", "closed", "escalated", "needs_human", "failed"},
	})
	checkEnum[StateCategory, *StateCategory](t, enumCase[StateCategory]{
		name: "StateCategory", all: StateCategories, parse: ParseStateCategory,
		want: []string{"active", "wip", "done", "frozen"},
	})
	checkEnum[BeadType, *BeadType](t, enumCase[BeadType]{
		name: "BeadType", all: BeadTypes, parse: ParseBeadType,
		want: []string{"task", "bug", "feature", "chore", "epic"},
	})
	checkEnum[Size, *Size](t, enumCase[Size]{
		name: "Size", all: Sizes, parse: ParseSize,
		want: []string{"s", "m", "l"},
	})
	checkEnum[AgentState, *AgentState](t, enumCase[AgentState]{
		name: "AgentState", all: AgentStates, parse: ParseAgentState,
		want: []string{"pending", "running", "finished", "failed", "killed"},
	})
	checkEnum[RunMode, *RunMode](t, enumCase[RunMode]{
		name: "RunMode", all: RunModes, parse: ParseRunMode,
		want: []string{"brief", "autopilot"},
	})
	checkEnum[VerdictOutcome, *VerdictOutcome](t, enumCase[VerdictOutcome]{
		name: "VerdictOutcome", all: VerdictOutcomes, parse: ParseVerdictOutcome,
		want: []string{"approve", "changes"},
	})
	checkEnum[Severity, *Severity](t, enumCase[Severity]{
		name: "Severity", all: Severities, parse: ParseSeverity,
		want: []string{"blocker", "major", "minor", "nit"},
	})
	checkEnum[ItemStatus, *ItemStatus](t, enumCase[ItemStatus]{
		name: "ItemStatus", all: ItemStatuses, parse: ParseItemStatus,
		want: []string{"open", "resolved", "accepted", "held", "overruled"},
	})
	checkEnum[ResponseAction, *ResponseAction](t, enumCase[ResponseAction]{
		name: "ResponseAction", all: ResponseActions, parse: ParseResponseAction,
		want: []string{"fixed", "pushback", "wontfix"},
	})
	checkEnum[ArbitrationRuling, *ArbitrationRuling](t, enumCase[ArbitrationRuling]{
		name: "ArbitrationRuling", all: ArbitrationRulings, parse: ParseArbitrationRuling,
		want: []string{"uphold_tester", "uphold_engineer", "compromise"},
	})
	checkEnum[EscalationOutcome, *EscalationOutcome](t, enumCase[EscalationOutcome]{
		name: "EscalationOutcome", all: EscalationOutcomes, parse: ParseEscalationOutcome,
		want: []string{"approve_with_notes", "narrow_scope", "block"},
	})
	checkEnum[MessageType, *MessageType](t, enumCase[MessageType]{
		name: "MessageType", all: MessageTypes, parse: ParseMessageType,
		want: []string{"plan", "impl_report", "verdict", "response", "arbitration", "escalation"},
	})
}

func TestBeadStateHelpers(t *testing.T) {
	tests := []struct {
		state    BeadState
		custom   bool
		terminal bool
		category StateCategory
	}{
		{BeadStateOpen, false, false, CategoryActive},
		{BeadStateInProgress, false, false, CategoryWIP},
		{BeadStateInReview, true, false, CategoryWIP},
		{BeadStateChangesRequested, true, false, CategoryWIP},
		{BeadStateApproved, true, false, CategoryWIP},
		{BeadStateDelivering, true, false, CategoryWIP},
		{BeadStateClosed, false, true, CategoryDone},
		{BeadStateEscalated, true, false, CategoryFrozen},
		{BeadStateNeedsHuman, true, true, CategoryFrozen},
		{BeadStateFailed, true, true, CategoryFrozen},
		{BeadState("bogus"), false, false, ""},
		{BeadState(""), false, false, ""},
	}
	for _, tc := range tests {
		t.Run(string(tc.state), func(t *testing.T) {
			assert.Equal(t, tc.custom, tc.state.Custom(), "Custom")
			assert.Equal(t, tc.terminal, tc.state.Terminal(), "Terminal")
			assert.Equal(t, tc.category, tc.state.Category(), "Category")
		})
	}

	// D-07's registration string, reconstructed from the model.
	var parts []string
	for _, s := range CustomBeadStates() {
		parts = append(parts, s.String()+":"+s.Category().String())
	}
	assert.Equal(t,
		"in_review:wip,changes_requested:wip,approved:wip,delivering:wip,escalated:frozen,needs_human:frozen,failed:frozen",
		strings.Join(parts, ","))
}

func TestAgentStateTerminal(t *testing.T) {
	tests := map[AgentState]bool{
		AgentStatePending:   false,
		AgentStateRunning:   false,
		AgentStateFinished:  true,
		AgentStateFailed:    true,
		AgentStateKilled:    true,
		AgentState("bogus"): false,
	}
	for s, want := range tests {
		assert.Equal(t, want, s.Terminal(), s.String())
	}
}

func TestSeverityRank(t *testing.T) {
	assert.Equal(t, 0, SeverityBlocker.Rank())
	assert.Equal(t, 1, SeverityMajor.Rank())
	assert.Equal(t, 2, SeverityMinor.Rank())
	assert.Equal(t, 3, SeverityNit.Rank())
	assert.Equal(t, -1, Severity("bogus").Rank())
	assert.Less(t, SeverityBlocker.Rank(), SeverityNit.Rank())
}

func TestItemStatusSettled(t *testing.T) {
	tests := map[ItemStatus]bool{
		ItemOpen:       false,
		ItemHeld:       false,
		ItemResolved:   true,
		ItemAccepted:   true,
		ItemOverruled:  true,
		ItemStatus(""): false,
	}
	for s, want := range tests {
		assert.Equal(t, want, s.Settled(), s.String())
	}
}

func TestPriorityValid(t *testing.T) {
	for p := PriorityHighest; p <= PriorityLowest; p++ {
		assert.True(t, p.Valid(), "priority %d", p)
	}
	assert.False(t, Priority(-1).Valid())
	assert.False(t, Priority(5).Valid())
	assert.True(t, PriorityDefault.Valid())
}

func TestIDStringers(t *testing.T) {
	assert.Equal(t, "sc-68s", BeadID("sc-68s").String())
	assert.Equal(t, "eng-1", AgentID("eng-1").String())
	assert.Equal(t, "r1", RunID("r1").String())
}
