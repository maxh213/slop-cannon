package events_test

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/events"
)

// The registry-driven half of P9-09's redaction contract.
//
// [redactEvents] is a hand-written table, and a hand-written table can only
// prove what somebody remembered to put in it: a new event kind, or a new
// free-text field on an existing one, would simply not appear in it and no
// test would fail — [events.Redacted] ends in `default: return ev`, so an
// unknown kind passes through whole. The tests here close both holes the way
// TestSamplesCoverEveryKind closes the sample table's: they enumerate
// [events.Kinds] and the struct fields themselves, so the source of truth is
// the type, not the fixture.
//
//   - [TestRedactionTableCoversEveryKind] — every registered kind is either
//     in the fixture or named in [redactNoText] with the reason it carries no
//     untrusted text.
//   - [TestRedactedRewritesEveryFreeTextField] — every `string` field of
//     every kind, reached through pointers, slices and both halves of a map,
//     is rewritten unless it is listed in [redactKeeps] as something sc, bd
//     or git minted.
//
// The walk covers maps as well as slices because a map is the shape a
// free-text field is likeliest to slip in through unnoticed: neither its
// keys nor its values are addressable, so a rewriter has to rebuild it on
// purpose, and until this walk planted a marker in both halves a
// `map[string]string` of annotations would have carried raw agent text into
// events.jsonl with every test still green (P9-09 review, round 4).
//
// Only fields of the unnamed type `string` are considered untrusted text.
// The named string types (domain.BeadID, domain.Lens, events.Kind, …) are
// sc's own vocabulary: they are ids, enums and states the views key off, and
// [TestRedactedLeavesSCsOwnFieldsAlone] pins that they survive.

// redactNoText are the kinds [events.Redacted] deliberately returns
// unchanged, each with the reason. A kind in this list must have no free
// text at all — the reflection test below proves it, so a new field on one
// of them fails the build rather than passing silently.
var redactNoText = map[events.Kind]string{
	events.KindRoundStarted:  "a round's bead, number, kind, lenses and commit are sc's own bookkeeping and git's",
	events.KindAgentFinished: "how a session ended: duration, exit code and the message type it produced",
}

// redactMarker is planted in every string field the reflection walk fills.
// It is not a credential: the point here is which fields the rewriter
// reaches, not what the rewriter does with them, which is
// TestRedactedMasksEveryUntrustedField's job.
const redactMarker = "UNTRUSTED-TEXT"

// redactKeeps are the `string` fields [events.Redacted] leaves alone,
// keyed by "<kind>.<field path>", each with the reason it is not untrusted
// text. Anything else that survives the rewriter fails the test.
var redactKeeps = map[string]string{
	"merged.SHA":                      "a commit sha git printed",
	"response_received.Report.Commit": "the sha of the commit the engineer's report names",
	"round_started.Commit":            "a commit sha git printed",

	// The run's own resolved configuration: the user's paths and the
	// names of the cartridge, model and documents sc was pointed at. None
	// of it came out of a subprocess, and `sc status` and the TUI header
	// read the paths back.
	"run_started.Info.RepoPath":                 "the repository sc was fired at",
	"run_started.Info.StateDir":                 "the run's own state directory (D-23)",
	"run_started.Info.Config.Cartridge":         "the cartridge name from the configuration",
	"run_started.Info.Config.Model":             "the model name from the configuration",
	"run_started.Info.Config.Rules[]":           "a rules document path the user configured",
	"run_started.Info.Config.Vibes[]":           "a vibes document path the user configured",
	"run_started.Info.Config.AutoMergeChecks[]": "a check name the user configured in auto_merge_checks (D-31)",
	"agent_spawned.Agent.Cartridge":             "the cartridge name from the configuration",
	"agent_spawned.Agent.Model":                 "the model name from the configuration",

	// Correlation keys. They are written by a model, but everything downstream
	// matches on them — a plan's Deps against its Keys, a response and a
	// ruling against the verdict item they answer — so rewriting one would
	// break the graph or orphan the reply rather than hide anything: they are
	// short identifiers, not prose, and the prose beside them is rewritten.
	"plan_produced.Plan.Beads[].Key":        "the plan's own key, which its Deps refer to",
	"plan_produced.Plan.Beads[].Deps[]":     "an edge of the plan graph, matched against those keys",
	"plan_produced.Plan.Beads[].Timeout":    "a Go duration Plan.Validate has parsed, not prose",
	"verdict_received.Verdict.Items[].ID":   "the feedback item id responses and rulings answer",
	"response_received.Responses[].ItemID":  "the verdict item this response answers",
	"arbitration_received.Rulings[].ItemID": "the verdict item this ruling settles",

	// bd's and sc's bookkeeping about a bead, not text about it.
	"bead_created.Bead.Labels[]":     "bd's labels, sc:run:<id> among them",
	"bead_updated.Bead.Labels[]":     "the same",
	"graph_changed.Beads[].Labels[]": "the same",
	"bead_updated.Fields[]":          "the names of the bead fields sc saw change",

	// The plan-key half of the map from plan key to minted bead id. It is
	// the same correlation key as plan_produced.Plan.Beads[].Key above, on
	// the other side of the arrow: `sc status`, the graph and the delivery
	// all match one against the other, and a map key cannot be rewritten in
	// place anyway. The prose the key names is rewritten in the plan.
	"plan_produced.Created{key}": "the plan key the created bead answers to",
}

// TestRedactionTableCoversEveryKind: the fixture must name every kind that
// carries untrusted text, and [redactNoText] must name the rest with a
// reason. A new event kind therefore fails here until somebody has decided
// which of the two it is.
func TestRedactionTableCoversEveryKind(t *testing.T) {
	inTable := map[events.Kind]bool{}
	for _, ev := range redactEvents() {
		require.False(t, inTable[ev.Kind()], "the redaction fixture has two %s events", ev.Kind())
		inTable[ev.Kind()] = true
	}
	for _, k := range events.Kinds() {
		_, exempt := redactNoText[k]
		switch {
		case inTable[k] && exempt:
			t.Errorf("%s is both in the redaction fixture and exempt from it: drop one", k)
		case inTable[k], exempt:
		default:
			t.Errorf("%s is in no redaction table: plant a secret in it in redactEvents(), "+
				"or add it to redactNoText with the reason it carries no untrusted text", k)
		}
	}
	for k, reason := range redactNoText {
		require.True(t, k.Valid(), "redactNoText names %s, which is not a registered kind", k)
		require.NotEmpty(t, reason, "%s needs the reason it carries no untrusted text", k)
	}
}

// TestRedactedRewritesEveryFreeTextField walks the event structs
// themselves: every kind is built from the registry, every `string` field in
// it (through pointers and one element of every slice) is filled with
// [redactMarker], and the event is put through [events.Redacted]. Whatever
// still holds the marker afterwards is a field the rewriter does not reach,
// and must be listed in [redactKeeps] with the reason it is sc's own text.
//
// This is what makes the claim on redactEvents() true: a free-text field
// added to an event fails here even if nobody adds it to the fixture.
func TestRedactedRewritesEveryFreeTextField(t *testing.T) {
	mask := func(string) string { return "[MASKED]" }
	used := map[string]bool{}
	for _, k := range events.Kinds() {
		t.Run(string(k), func(t *testing.T) {
			proto, ok := events.New(k)
			require.True(t, ok, "the registry has no %s", k)

			filled := reflect.New(reflect.TypeOf(proto)).Elem()
			planted := redactPlant(t, filled, "")
			ev, ok := filled.Interface().(events.Event)
			require.True(t, ok, "%s does not implement Event", k)
			out := events.Redacted(ev, mask)
			require.Equal(t, k, out.Kind())
			if reason, exempt := redactNoText[k]; exempt {
				// An exempt kind is returned whole, so every string it does
				// carry has to be one of sc's own.
				require.Equal(t, ev, out, "%s is exempt from redaction (%s), so Redacted must return it unchanged", k, reason)
				for _, path := range planted {
					key := string(k) + "." + path
					require.Contains(t, redactKeeps, key,
						"%s is exempt from redaction (%s) but carries %s, which nothing says is sc's own text: "+
							"rewrite it in events.Redacted and put the kind in the fixture, or name it in redactKeeps",
						k, reason, path)
					used[key] = true
				}
				return
			}
			require.NotEmpty(t, planted, "%s has no string field at all: it belongs in redactNoText", k)

			for _, path := range redactSurvivors(reflect.ValueOf(out), "") {
				key := string(k) + "." + path
				if _, kept := redactKeeps[key]; kept {
					used[key] = true
					continue
				}
				t.Errorf("Redacted leaves %s.%s as it came in: rewrite it in events.Redacted, "+
					"or add %q to redactKeeps with the reason it is not untrusted text", k, path, key)
			}
		})
	}
	for key, reason := range redactKeeps {
		require.NotEmpty(t, reason, "%s needs the reason it is not untrusted text", key)
		require.True(t, used[key], "redactKeeps names %s, which no longer survives redaction: drop the entry", key)
	}
}

// redactPlant fills every reachable `string` field of v with
// [redactMarker], allocating one element for every slice and one value for
// every pointer, and returns the paths it planted. Named string types are
// left at their zero value: they are ids and enums, not text.
func redactPlant(t *testing.T, v reflect.Value, path string) []string {
	t.Helper()
	var planted []string
	switch v.Kind() {
	case reflect.String:
		if v.Type() == reflect.TypeOf("") && v.CanSet() {
			v.SetString(redactMarker)
			planted = append(planted, path)
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			return nil
		}
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			name := f.Name
			if f.Anonymous {
				// An embedded struct's fields read as its own (Meta.RunID).
				name = f.Type.Name()
			}
			planted = append(planted, redactPlant(t, v.Field(i), redactPath(path, name))...)
		}
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		planted = append(planted, redactPlant(t, v.Elem(), path)...)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		planted = append(planted, redactPlant(t, v.Index(0), path+"[]")...)
	case reflect.Map:
		// A map carries text in both halves, and neither is addressable, so
		// each is built on the side and put back. A key is the awkward one:
		// events.Redacted cannot rewrite a key in place either, so a map
		// keyed by free text has to be rebuilt by the rewriter or named in
		// redactKeeps — which is exactly the decision this walk forces.
		m := reflect.MakeMap(v.Type())
		key := reflect.New(v.Type().Key()).Elem()
		planted = append(planted, redactPlant(t, key, path+"{key}")...)
		val := reflect.New(v.Type().Elem()).Elem()
		planted = append(planted, redactPlant(t, val, path+"{}")...)
		m.SetMapIndex(key, val)
		v.Set(m)
	default:
		// Numbers, booleans, times: nothing a rewriter would touch.
	}
	return planted
}

// redactSurvivors returns the paths of the `string` fields still holding
// [redactMarker] after a rewrite, sorted for a stable failure message.
func redactSurvivors(v reflect.Value, path string) []string {
	var left []string
	switch v.Kind() {
	case reflect.String:
		if v.Type() == reflect.TypeOf("") && v.String() == redactMarker {
			left = append(left, path)
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			return nil
		}
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			name := f.Name
			if f.Anonymous {
				name = f.Type.Name()
			}
			left = append(left, redactSurvivors(v.Field(i), redactPath(path, name))...)
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			left = append(left, redactSurvivors(v.Elem(), path)...)
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			left = append(left, redactSurvivors(v.Index(i), path+"[]")...)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			left = append(left, redactSurvivors(k, path+"{key}")...)
			left = append(left, redactSurvivors(v.MapIndex(k), path+"{}")...)
		}
	default:
	}
	sort.Strings(left)
	return left
}

// redactPath joins a field onto a path, skipping the embedded Meta, whose
// fields read better as their own names.
func redactPath(path, name string) string {
	if name == "Meta" {
		return path
	}
	if path == "" {
		return name
	}
	return strings.TrimPrefix(path+"."+name, ".")
}
