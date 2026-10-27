package events

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"

	"slop-cannon/internal/domain"
)

func TestKindsAreUniqueSnakeCaseAndRegistered(t *testing.T) {
	kinds := Kinds()
	if len(kinds) != 23 {
		t.Fatalf("expected 23 registered kinds, got %d: %v", len(kinds), kinds)
	}
	seen := map[Kind]bool{}
	for _, k := range kinds {
		if seen[k] {
			t.Errorf("kind %q listed twice", k)
		}
		seen[k] = true
		if !k.Valid() {
			t.Errorf("kind %q not valid", k)
		}
		if k.String() != string(k) {
			t.Errorf("String() mismatch for %q", k)
		}
		if strings.ToLower(string(k)) != string(k) || strings.ContainsAny(string(k), "- ") {
			t.Errorf("kind %q is not snake_case", k)
		}
		ev, ok := New(k)
		if !ok || ev == nil {
			t.Errorf("New(%q) failed", k)
			continue
		}
		if ev.Kind() != k {
			t.Errorf("New(%q).Kind() = %q", k, ev.Kind())
		}
		if reflect.TypeOf(ev).Kind() != reflect.Struct {
			t.Errorf("New(%q) returned a %s, want a struct value", k, reflect.TypeOf(ev).Kind())
		}
	}
	if _, ok := New("nope"); ok {
		t.Error("New of unknown kind succeeded")
	}
	if Kind("nope").Valid() {
		t.Error("unknown kind reported valid")
	}
}

func TestEveryEventKindMatchesItsTypeName(t *testing.T) {
	// The Kind constants are the snake_case form of the Go type names; this
	// keeps log lines and type switches in sync by construction.
	for _, p := range prototypes {
		name := reflect.TypeOf(p).Name()
		want := toSnake(name)
		if string(p.Kind()) != want {
			t.Errorf("%s.Kind() = %q, want %q", name, p.Kind(), want)
		}
	}
}

// toSnake turns a Go type name into its snake_case kind: "BeadStateChanged"
// → "bead_state_changed", "PROpened" → "pr_opened".
func toSnake(s string) string {
	var b strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		if unicode.IsUpper(r) && i > 0 {
			prevLower := unicode.IsLower(runes[i-1])
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if prevLower || nextLower {
				b.WriteByte('_')
			}
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

func TestParseKind(t *testing.T) {
	tests := []struct {
		in   string
		want Kind
		ok   bool
	}{
		{"run_started", KindRunStarted, true},
		{" Bead-State-Changed ", KindBeadStateChanged, true},
		{"PR_OPENED", KindPROpened, true},
		{"error", KindError, true},
		{"", "", false},
		{"   ", "", false},
		{"bead_exploded", "", false},
	}
	for _, tc := range tests {
		got, err := ParseKind(tc.in)
		if tc.ok {
			if err != nil || got != tc.want {
				t.Errorf("ParseKind(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("ParseKind(%q) succeeded with %q", tc.in, got)
		} else if !errors.Is(err, ErrUnknownKind) {
			t.Errorf("ParseKind(%q) error %v does not wrap ErrUnknownKind", tc.in, err)
		}
	}
}

func TestMeta(t *testing.T) {
	loc := time.FixedZone("X", 3*3600)
	local := time.Date(2026, 9, 2, 15, 0, 0, 0, loc)
	m := MetaAt("r1", local)
	if m.At().Location() != time.UTC {
		t.Errorf("MetaAt did not normalise to UTC: %v", m.At())
	}
	if !m.At().Equal(local) {
		t.Errorf("MetaAt changed the instant: %v vs %v", m.At(), local)
	}
	if m.RunID() != domain.RunID("r1") {
		t.Errorf("RunID() = %q", m.RunID())
	}

	before := time.Now()
	n := NewMeta("r2")
	if n.At().Before(before.Add(-time.Second)) || n.At().After(time.Now().Add(time.Second)) {
		t.Errorf("NewMeta time %v not close to now", n.At())
	}
	if n.At().Location() != time.UTC {
		t.Error("NewMeta not UTC")
	}

	// Every sample answers the interface through its embedded Meta.
	for _, ev := range sampleEvents() {
		if !ev.At().Equal(sampleTime) || ev.RunID() != sampleRun {
			t.Errorf("%s: At/RunID not taken from Meta", ev.Kind())
		}
	}
}

func TestSmallEnumsStringify(t *testing.T) {
	if FinishCreditsExhausted.String() != "credits_exhausted" {
		t.Error("FinishReason.String")
	}
	if FailureRateLimited.String() != "rate_limited" {
		t.Error("Failure.String")
	}
	if StreamStdout.String() != "stdout" {
		t.Error("Stream.String")
	}
	if RoundConflict.String() != "conflict" {
		t.Error("RoundKind.String")
	}
	if TaskEvaluatePushback.String() != "evaluate_pushback" {
		t.Error("Task.String")
	}
}
