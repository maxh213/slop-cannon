package events

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"slop-cannon/internal/domain"
)

func TestMarshalUnmarshalRoundTripEveryKind(t *testing.T) {
	for _, ev := range sampleEvents() {
		t.Run(string(ev.Kind()), func(t *testing.T) {
			b, err := Marshal(ev)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if !strings.HasPrefix(string(b), `{"kind":"`+string(ev.Kind())+`"`) {
				t.Errorf("envelope does not lead with kind: %s", b)
			}
			if strings.Contains(string(b), "\n") {
				t.Errorf("Marshal output contains a newline: %q", b)
			}
			var generic map[string]any
			if err := json.Unmarshal(b, &generic); err != nil {
				t.Fatalf("output is not a JSON object: %v\n%s", err, b)
			}
			for _, key := range []string{"kind", "at", "run_id"} {
				if _, ok := generic[key]; !ok {
					t.Errorf("missing %q in %s", key, b)
				}
			}

			back, err := Unmarshal(b)
			if err != nil {
				t.Fatalf("Unmarshal: %v\n%s", err, b)
			}
			if reflect.TypeOf(back) != reflect.TypeOf(ev) {
				t.Fatalf("decoded %T, want %T", back, ev)
			}
			if !reflect.DeepEqual(back, ev) {
				t.Errorf("round trip changed the event\n got: %#v\nwant: %#v\njson: %s", back, ev, b)
			}

			// Encoding the decoded value again must be byte-identical.
			again, err := Marshal(back)
			if err != nil {
				t.Fatalf("Marshal again: %v", err)
			}
			if string(again) != string(b) {
				t.Errorf("second encoding differs\n first: %s\nsecond: %s", b, again)
			}
		})
	}
}

func TestMarshalZeroValues(t *testing.T) {
	// The zero value of every kind must encode and decode too: this is what
	// New returns and what a fake generator starts from.
	for _, k := range Kinds() {
		ev, _ := New(k)
		b, err := Marshal(ev)
		if err != nil {
			t.Errorf("%s: %v", k, err)
			continue
		}
		back, err := Unmarshal(b)
		if err != nil {
			t.Errorf("%s: unmarshal zero: %v (%s)", k, err, b)
			continue
		}
		if back.Kind() != k {
			t.Errorf("%s: decoded as %s", k, back.Kind())
		}
	}
}

func TestMarshalRejectsNilAndUnregistered(t *testing.T) {
	if _, err := Marshal(nil); !errors.Is(err, ErrNotEvent) {
		t.Errorf("Marshal(nil) = %v, want ErrNotEvent", err)
	}
	if _, err := Marshal(unregistered{}); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("Marshal(unregistered) = %v, want ErrUnknownKind", err)
	}
	if _, err := Marshal(unencodable{}); err == nil || errors.Is(err, ErrUnknownKind) {
		t.Errorf("Marshal(unencodable) = %v, want a json error", err)
	}
}

// unregistered implements Event with a kind the registry does not know.
type unregistered struct{ Meta }

func (unregistered) Kind() Kind { return "unregistered" }

// unencodable has a registered kind but a field json cannot encode.
type unencodable struct {
	Meta
	Bad func() `json:"bad"`
}

func (unencodable) Kind() Kind { return KindWarning }

func TestUnmarshalErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want error
	}{
		{"empty", "", ErrNotEvent},
		{"not json", "hello", ErrNotEvent},
		{"array", `[1,2]`, ErrNotEvent},
		{"no kind", `{"at":"2026-09-02T00:00:00Z"}`, ErrNotEvent},
		{"unknown kind", `{"kind":"bead_exploded"}`, ErrUnknownKind},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Unmarshal([]byte(tc.in))
			if !errors.Is(err, tc.want) {
				t.Errorf("Unmarshal(%q) = %v, want %v", tc.in, err, tc.want)
			}
		})
	}

	// A known kind with a field of the wrong type is a decode error that is
	// neither ErrNotEvent nor ErrUnknownKind.
	_, err := Unmarshal([]byte(`{"kind":"bead_state_changed","round":"one"}`))
	if err == nil || errors.Is(err, ErrNotEvent) || errors.Is(err, ErrUnknownKind) {
		t.Errorf("bad field type: %v", err)
	}
	// An invalid enum value inside a domain type surfaces domain's error.
	_, err = Unmarshal([]byte(`{"kind":"bead_state_changed","to":"exploded"}`))
	if !errors.Is(err, domain.ErrUnknownValue) {
		t.Errorf("bad enum: %v, want domain.ErrUnknownValue", err)
	}
}

func TestUnmarshalIgnoresUnknownFieldsAndWhitespace(t *testing.T) {
	line := "  {\"kind\":\"warning\",\"at\":\"2026-09-02T12:01:03.5Z\",\"run_id\":\"r1\",\"message\":\"hi\",\"future_field\":{\"x\":1}}\n"
	ev, err := Unmarshal([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	w, ok := ev.(Warning)
	if !ok {
		t.Fatalf("got %T", ev)
	}
	if w.Message != "hi" || w.RunID() != "r1" || !w.At().Equal(time.Date(2026, 9, 2, 12, 1, 3, 500_000_000, time.UTC)) {
		t.Errorf("decoded %#v", w)
	}
}

func TestMarshalIsCompact(t *testing.T) {
	b, err := Marshal(RunPaused{Meta: sampleMeta(), Reason: "user"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"kind":"run_paused","at":"2026-09-02T12:01:03.5Z","run_id":"run-20260902-1201","reason":"user"}`
	if string(b) != want {
		t.Errorf("\n got: %s\nwant: %s", b, want)
	}
}
