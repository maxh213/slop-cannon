package domain

import (
	"encoding/json"
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
			assert.ErrorIs(t, err, ErrUnknownValue, "*json.MarshalerError unwraps to the enum error")
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
		want: []string{"open", "in_progress", "in_review", "changes_requested", "approved", "delivering", "closed", "escalated", "needs_human", "failed", "blocked", "hooked", "deferred", "pinned"},
	})
	checkEnum[StateCategory, *StateCategory](t, enumCase[StateCategory]{
		name: "StateCategory", all: StateCategories, parse: ParseStateCategory,
		want: []string{"active", "wip", "done", "frozen"},
	})
	checkEnum[BeadType, *BeadType](t, enumCase[BeadType]{
		name: "BeadType", all: BeadTypes, parse: ParseBeadType,
		want: []string{"task", "bug", "feature", "chore", "epic", "decision", "spike", "story", "milestone", "gate", "event"},
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
		workflow bool
		custom   bool
		terminal bool
		category StateCategory
	}{
		{BeadStateOpen, true, false, false, CategoryActive},
		{BeadStateInProgress, true, false, false, CategoryWIP},
		{BeadStateInReview, true, true, false, CategoryWIP},
		{BeadStateChangesRequested, true, true, false, CategoryWIP},
		{BeadStateApproved, true, true, false, CategoryWIP},
		{BeadStateDelivering, true, true, false, CategoryWIP},
		{BeadStateClosed, true, false, true, CategoryDone},
		{BeadStateEscalated, true, true, false, CategoryFrozen},
		{BeadStateNeedsHuman, true, true, true, CategoryFrozen},
		{BeadStateFailed, true, true, true, CategoryFrozen},
		// bd built-ins the cannon reads but never sets (Appendix A).
		{BeadStateBlocked, false, false, false, CategoryWIP},
		{BeadStateHooked, false, false, false, CategoryWIP},
		{BeadStateDeferred, false, false, false, CategoryFrozen},
		{BeadStatePinned, false, false, false, CategoryFrozen},
		{BeadState("bogus"), false, false, false, ""},
		{BeadState(""), false, false, false, ""},
	}
	for _, tc := range tests {
		t.Run(string(tc.state), func(t *testing.T) {
			assert.Equal(t, tc.workflow, tc.state.Workflow(), "Workflow")
			assert.Equal(t, tc.custom, tc.state.Custom(), "Custom")
			assert.Equal(t, tc.terminal, tc.state.Terminal(), "Terminal")
			assert.Equal(t, tc.category, tc.state.Category(), "Category")
		})
	}

	// Every valid state has a category, and the two listings agree.
	for _, s := range BeadStates() {
		assert.True(t, s.Category().Valid(), "%s has a category", s)
		if s.Custom() {
			assert.True(t, s.Workflow(), "%s: custom statuses are workflow states", s)
		}
	}
	assert.Equal(t, BeadStates()[:len(WorkflowBeadStates())], WorkflowBeadStates(), "workflow states lead BeadStates in flow order")
	wf := WorkflowBeadStates()
	wf[0] = "clobbered"
	assert.Equal(t, BeadStateOpen, WorkflowBeadStates()[0], "WorkflowBeadStates returns a copy")

	// The bd 1.2.2 built-in status list, as captured in
	// internal/testutil/testdata/bd/statuses-builtin.json, all parse and have
	// bd's categories.
	builtin := map[string]StateCategory{
		"open": CategoryActive, "in_progress": CategoryWIP, "blocked": CategoryWIP, "hooked": CategoryWIP,
		"deferred": CategoryFrozen, "pinned": CategoryFrozen, "closed": CategoryDone,
	}
	for name, cat := range builtin {
		s, err := ParseBeadState(name)
		require.NoError(t, err, name)
		assert.Equal(t, cat, s.Category(), name)
		assert.False(t, s.Custom(), "%s is a bd built-in", name)
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

func TestBeadTypePlannable(t *testing.T) {
	want := map[BeadType]bool{
		BeadTypeTask: true, BeadTypeBug: true, BeadTypeFeature: true, BeadTypeChore: true,
		BeadTypeEpic: false, BeadTypeDecision: false, BeadTypeSpike: false, BeadTypeStory: false,
		BeadTypeMilestone: false, BeadTypeGate: false, BeadTypeEvent: false,
		BeadType(""): false, BeadType("bogus"): false,
	}
	for _, bt := range BeadTypes() {
		_, listed := want[bt]
		assert.True(t, listed, "test table misses %s", bt)
	}
	for bt, plannable := range want {
		assert.Equal(t, plannable, bt.Plannable(), string(bt))
	}
	assert.Equal(t, []BeadType{BeadTypeTask, BeadTypeBug, BeadTypeFeature, BeadTypeChore}, PlannableBeadTypes())
	assert.Equal(t, BeadTypes()[:4], PlannableBeadTypes(), "plannable types lead BeadTypes")
	pl := PlannableBeadTypes()
	pl[0] = "clobbered"
	assert.Equal(t, BeadTypeTask, PlannableBeadTypes()[0], "PlannableBeadTypes returns a copy")

	// Everything bd 1.2.2 mints (Appendix A, and the gate-list/export
	// fixtures) parses.
	for _, name := range []string{"task", "bug", "feature", "chore", "epic", "decision", "spike", "story", "milestone", "gate", "event"} {
		_, err := ParseBeadType(name)
		assert.NoError(t, err, name)
	}
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

func TestPriorityJSON(t *testing.T) {
	t.Run("marshals as a bare number", func(t *testing.T) {
		for p := PriorityHighest; p <= PriorityLowest; p++ {
			data, err := json.Marshal(p)
			require.NoError(t, err)
			assert.Equal(t, fmt.Sprint(int(p)), string(data))
		}
	})

	decode := []struct {
		in   string
		want Priority
	}{
		{"0", PriorityHighest},
		{"2", PriorityDefault},
		{"4", PriorityLowest},
		{" 3 ", 3},
		{"null", PriorityDefault},
		{`""`, PriorityDefault},
		{`"  "`, PriorityDefault},
		{`"1"`, 1},
		{`" 4 "`, 4},
		{"7", 7}, // out of range decodes; Plan.Validate rejects it
	}
	for _, tc := range decode {
		t.Run(tc.in, func(t *testing.T) {
			p := Priority(-99)
			require.NoError(t, json.Unmarshal([]byte(tc.in), &p))
			assert.Equal(t, tc.want, p)
		})
	}

	for _, bad := range []string{`"high"`, `"1.5"`, `1.5`, `true`, `[1]`, `{}`} {
		t.Run("invalid "+bad, func(t *testing.T) {
			var p Priority
			err := json.Unmarshal([]byte(bad), &p)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrUnknownValue)
		})
	}

	t.Run("malformed string called directly", func(t *testing.T) {
		// encoding/json validates syntax before calling UnmarshalJSON, but
		// other decoders may not; an unterminated string must still fail.
		var p Priority
		err := p.UnmarshalJSON([]byte(`"unterminated`))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrUnknownValue)
		assert.Equal(t, Priority(0), p, "receiver untouched on error")
	})
}

func TestIDStringers(t *testing.T) {
	assert.Equal(t, "sc-68s", BeadID("sc-68s").String())
	assert.Equal(t, "eng-1", AgentID("eng-1").String())
	assert.Equal(t, "r1", RunID("r1").String())
}
