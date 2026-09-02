package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// ErrNotEvent is returned by [Unmarshal] when the input is not a JSON object
// carrying a "kind" field.
var ErrNotEvent = errors.New("not an event envelope")

// prototypes lists one zero value of every concrete event, in the order the
// kinds are declared. It is the single place a new event must be added:
// the registry, [Kinds] and the exhaustive tests all derive from it.
var prototypes = []Event{
	RunStarted{}, RunPaused{}, RunResumed{}, RunStopped{}, RunFinished{},
	PlanProduced{},
	BeadCreated{}, BeadUpdated{}, BeadStateChanged{}, GraphChanged{},
	AgentSpawned{}, AgentOutput{}, AgentFinished{}, AgentFailed{},
	RoundStarted{}, VerdictReceived{}, ResponseReceived{}, ArbitrationReceived{},
	PROpened{}, Merged{},
	Warning{}, Error{},
}

// registry maps each kind to the concrete struct type that decodes it.
var registry = func() map[Kind]reflect.Type {
	m := make(map[Kind]reflect.Type, len(prototypes))
	for _, p := range prototypes {
		k := p.Kind()
		if _, dup := m[k]; dup {
			panic(fmt.Sprintf("events: kind %q registered twice", k))
		}
		m[k] = reflect.TypeOf(p)
	}
	return m
}()

// Kinds returns every registered kind in declaration order.
func Kinds() []Kind {
	out := make([]Kind, len(prototypes))
	for i, p := range prototypes {
		out[i] = p.Kind()
	}
	return out
}

// New returns the zero event of the given kind, or false when the kind is
// unknown. Fake event generators (P7-01) use it to enumerate shapes.
func New(k Kind) (Event, bool) {
	t, ok := registry[k]
	if !ok {
		return nil, false
	}
	ev, ok := reflect.New(t).Elem().Interface().(Event)
	return ev, ok
}

// envelope is the shape of the leading bytes of every encoded event.
type envelope struct {
	Kind Kind `json:"kind"`
}

// Marshal encodes an event as one JSON object: a leading "kind" field
// followed by the event's own fields (including "at" and "run_id" from
// [Meta]). The result has no trailing newline. A nil event is an error.
func Marshal(ev Event) ([]byte, error) {
	if ev == nil {
		return nil, fmt.Errorf("%w: nil event", ErrNotEvent)
	}
	k := ev.Kind()
	if !k.Valid() {
		return nil, fmt.Errorf("%w: %q is not registered", ErrUnknownKind, k)
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("events: marshal %s: %w", k, err)
	}
	// Every registered event is a struct, so body is "{...}". Splice the
	// discriminator in as the first member rather than re-encoding through
	// a map: this keeps field order stable and costs one allocation.
	if len(body) < 2 || body[0] != '{' || body[len(body)-1] != '}' {
		return nil, fmt.Errorf("events: marshal %s: unexpected encoding %q", k, body)
	}
	head, err := json.Marshal(envelope{Kind: k})
	if err != nil {
		return nil, fmt.Errorf("events: marshal %s: %w", k, err)
	}
	out := make([]byte, 0, len(head)+len(body))
	out = append(out, head[:len(head)-1]...) // {"kind":"..."
	if len(body) > 2 {
		out = append(out, ',')
		out = append(out, body[1:len(body)-1]...)
	}
	return append(out, '}'), nil
}

// Unmarshal decodes one JSON object produced by [Marshal] back into the
// concrete event value for its "kind". Unknown fields are ignored so a log
// written by a newer sc still reads; an unknown kind returns an error
// wrapping [ErrUnknownKind], and input that is not an object with a "kind"
// returns one wrapping [ErrNotEvent].
func Unmarshal(data []byte) (Event, error) {
	data = bytes.TrimSpace(data)
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotEvent, err)
	}
	if env.Kind == "" {
		return nil, fmt.Errorf("%w: missing kind", ErrNotEvent)
	}
	t, ok := registry[env.Kind]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKind, env.Kind)
	}
	ptr := reflect.New(t)
	if err := json.Unmarshal(data, ptr.Interface()); err != nil {
		return nil, fmt.Errorf("events: unmarshal %s: %w", env.Kind, err)
	}
	ev, ok := ptr.Elem().Interface().(Event)
	if !ok {
		return nil, fmt.Errorf("events: %s does not implement Event", t)
	}
	return ev, nil
}
