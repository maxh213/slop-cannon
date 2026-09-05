package agents_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge/structured"
	"slop-cannon/internal/domain"
)

// schemaDocsDir is the published copy of the embedded schemas, relative to
// this package.
const schemaDocsDir = "../../docs/schemas"

// schemaExamplesDir holds the valid/ and invalid/ example documents.
var schemaExamplesDir = filepath.Join(schemaDocsDir, "examples")

// schemaFileName is the file name convention every schema follows.
func schemaFileName(t domain.MessageType) string { return t.String() + ".schema.json" }

// schemaUpdateGolden reports whether the test run may rewrite the published
// docs copies and the .expected error goldens (UPDATE_GOLDEN=1, as set by
// `make update-golden`).
func schemaUpdateGolden() bool { return os.Getenv("UPDATE_GOLDEN") != "" }

// schemaDoc decodes one embedded schema into a generic document so tests can
// assert on its shape.
func schemaDoc(t *testing.T, mt domain.MessageType) map[string]any {
	t.Helper()
	raw, err := agents.SchemaJSON(mt)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(raw, &doc), "%s is not valid JSON", schemaFileName(mt))
	return doc
}

// schemaDig walks doc by object keys and fails the test when the path is
// absent.
func schemaDig(t *testing.T, doc map[string]any, path ...string) any {
	t.Helper()
	var cur any = doc
	for i, key := range path {
		obj, ok := cur.(map[string]any)
		require.Truef(t, ok, "%s is not an object", strings.Join(path[:i], "."))
		cur, ok = obj[key]
		require.Truef(t, ok, "no %s in schema", strings.Join(path[:i+1], "."))
	}
	return cur
}

// schemaEnum returns the string enum at path.
func schemaEnum(t *testing.T, doc map[string]any, path ...string) []string {
	t.Helper()
	raw, ok := schemaDig(t, doc, append(path, "enum")...).([]any)
	require.Truef(t, ok, "%s.enum is not an array", strings.Join(path, "."))
	out := make([]string, len(raw))
	for i, v := range raw {
		s, ok := v.(string)
		require.Truef(t, ok, "%s.enum[%d] is not a string", strings.Join(path, "."), i)
		out[i] = s
	}
	return out
}

// schemaExamples lists the *.json files in valid/ or invalid/.
func schemaExamples(t *testing.T, kind string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(schemaExamplesDir, kind, "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, paths, "no %s examples in %s", kind, schemaExamplesDir)
	sort.Strings(paths)
	return paths
}

// schemaTypeOfExample derives the message type from an example file name:
// everything before the first "-" (plan-brief-mode.json -> plan).
func schemaTypeOfExample(t *testing.T, path string) domain.MessageType {
	t.Helper()
	base := strings.TrimSuffix(filepath.Base(path), ".json")
	name, _, ok := strings.Cut(base, "-")
	require.Truef(t, ok, "example %s must be named <message type>-<label>.json", filepath.Base(path))
	mt, err := domain.ParseMessageType(name)
	require.NoErrorf(t, err, "example %s does not start with a message type", filepath.Base(path))
	return mt
}

// schemaReadFile reads an example or golden file.
func schemaReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test-local, repo-relative path
	require.NoError(t, err)
	return b
}

// schemaBlock wraps a JSON document in the sc-result fence an agent emits.
func schemaBlock(prose string, doc []byte) string {
	return prose + "\n\n```" + structured.Fence + "\n" + string(doc) + "\n```\n"
}

func TestSchemaRegistryCoversEveryMessageType(t *testing.T) {
	t.Parallel()
	reg, err := agents.Contracts()
	require.NoError(t, err)
	want := make([]string, 0, len(domain.MessageTypes()))
	for _, mt := range domain.MessageTypes() {
		want = append(want, mt.String())
	}
	sort.Strings(want)
	assert.Equal(t, want, reg.Names(), "one schema file per domain.MessageType (D-06, D-25)")
	for _, mt := range domain.MessageTypes() {
		c, err := agents.ContractFor(mt)
		require.NoError(t, err)
		assert.Equal(t, mt.String(), c.Name())
	}
}

func TestSchemaFilesFollowTheConventions(t *testing.T) {
	t.Parallel()
	for _, mt := range domain.MessageTypes() {
		t.Run(mt.String(), func(t *testing.T) {
			t.Parallel()
			doc := schemaDoc(t, mt)
			assert.Equal(t, "https://json-schema.org/draft/2020-12/schema", doc["$schema"])
			assert.Equal(t, structured.BaseURL+schemaFileName(mt), doc["$id"],
				"$id must match the file name so relative $refs resolve")
			assert.Equal(t, mt.String(), doc["title"])
			assert.Equal(t, "object", doc["type"])
			assert.Equal(t, false, doc["additionalProperties"], "unknown fields must be rejected")
			assert.NotEmpty(t, doc["description"], "the description is shown to the model")
			// The comment writer stamps `type` on the stored block, so every
			// schema accepts it and pins it to its own name.
			assert.Equal(t, mt.String(), schemaDig(t, doc, "properties", "type", "const"))
			assert.NotContains(t, string(mustSchemaJSON(t, mt)), "http://",
				"schemas must not reference the network")
		})
	}
}

// mustSchemaJSON is SchemaJSON without the error dance.
func mustSchemaJSON(t *testing.T, mt domain.MessageType) []byte {
	t.Helper()
	b, err := agents.SchemaJSON(mt)
	require.NoError(t, err)
	return b
}

func TestSchemaEnumsMatchTheDomain(t *testing.T) {
	t.Parallel()
	plan := schemaDoc(t, domain.MessagePlan)
	verdict := schemaDoc(t, domain.MessageVerdict)
	response := schemaDoc(t, domain.MessageResponse)
	arbitration := schemaDoc(t, domain.MessageArbitration)
	escalation := schemaDoc(t, domain.MessageEscalation)

	planBead := []string{"$defs", "plan_bead", "properties"}
	item := []string{"$defs", "feedback_item", "properties"}

	assert.Equal(t, stringsOf(domain.PlannableBeadTypes()), schemaEnum(t, plan, append(planBead, "type")...))
	assert.Equal(t, stringsOf(domain.Sizes()), schemaEnum(t, plan, append(planBead, "size")...))
	assert.Equal(t, stringsOf(domain.Lenses()), schemaEnum(t, verdict, "properties", "lens"))
	assert.Equal(t, stringsOf(domain.VerdictOutcomes()), schemaEnum(t, verdict, "properties", "outcome"))
	assert.Equal(t, stringsOf(domain.Severities()), schemaEnum(t, verdict, append(item, "severity")...))
	assert.Equal(t, stringsOf(domain.ItemStatuses()), schemaEnum(t, verdict, append(item, "status")...))
	assert.Equal(t, stringsOf(domain.ResponseActions()), schemaEnum(t, response, "$defs", "response_item", "properties", "action"))
	assert.Equal(t, stringsOf(domain.ArbitrationRulings()), schemaEnum(t, arbitration, "properties", "ruling"))
	assert.Equal(t, stringsOf(domain.EscalationOutcomes()), schemaEnum(t, escalation, "properties", "outcome"))

	// bd's priority scale, 0 highest to 4 lowest, default 2 (D-22).
	priority := append(append([]string(nil), planBead...), "priority")
	assert.Equal(t, float64(domain.PriorityHighest), schemaDig(t, plan, append(priority, "minimum")...))
	assert.Equal(t, float64(domain.PriorityLowest), schemaDig(t, plan, append(priority, "maximum")...))
	assert.Equal(t, float64(domain.PriorityDefault), schemaDig(t, plan, append(priority, "default")...))

	// The mandatory rules category is spelled the same way in both places.
	assert.Equal(t, domain.CategoryRules,
		schemaDig(t, verdict, "$defs", "feedback_item", "if", "properties", "category", "const"))
}

// stringsOf renders any enum slice whose element type has an underlying
// string type.
func stringsOf[T ~string](vs []T) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	return out
}

func TestSchemaValidExamplesValidateAndDecode(t *testing.T) {
	t.Parallel()
	seen := map[domain.MessageType]bool{}
	for _, path := range schemaExamples(t, "valid") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			t.Parallel()
			mt := schemaTypeOfExample(t, path)
			raw := schemaReadFile(t, path)
			require.NoError(t, agents.ValidateJSON(mt, raw), "example must satisfy %s", schemaFileName(mt))

			payload, err := agents.DecodeJSON(mt, raw)
			require.NoError(t, err)
			require.NotNil(t, payload)

			// The same document must also survive the fenced-block path an
			// agent's reply takes.
			fromBlock, _, err := agents.ParseMessage(mt, schemaBlock("Here is the result.", raw))
			require.NoError(t, err)
			assert.Equal(t, payload, fromBlock)

			if plan, ok := payload.(domain.Plan); ok {
				assert.NoError(t, plan.Validate(), "a schema-valid plan must also pass domain.Plan.Validate")
			}
		})
		seen[schemaTypeOfExample(t, path)] = true
	}
	for _, mt := range domain.MessageTypes() {
		assert.Truef(t, seen[mt], "no valid example for %s in docs/schemas/examples/valid", mt)
	}
}

func TestSchemaInvalidExamplesFailWithAReadableMessage(t *testing.T) {
	t.Parallel()
	seen := map[domain.MessageType]bool{}
	for _, path := range schemaExamples(t, "invalid") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			t.Parallel()
			mt := schemaTypeOfExample(t, path)
			err := agents.ValidateJSON(mt, schemaReadFile(t, path))
			require.Error(t, err, "example must be rejected by %s", schemaFileName(mt))
			require.True(t, errors.Is(err, structured.ErrSchema), "want a schema violation, got %v", err)

			pe, ok := structured.AsParseError(err)
			require.True(t, ok)
			assert.Equal(t, mt.String(), pe.Contract)
			assert.NotEmpty(t, pe.Problems, "the reader must be told which rule was broken")
			for _, p := range pe.Problems {
				assert.Contains(t, p, "at '", "each problem names the offending location: %q", p)
			}

			got := err.Error()
			golden := strings.TrimSuffix(path, ".json") + ".expected"
			if schemaUpdateGolden() {
				require.NoError(t, os.WriteFile(golden, []byte(got+"\n"), 0o644)) //nolint:gosec // published docs
			}
			want := strings.TrimRight(string(schemaReadFile(t, golden)), "\n")
			assert.Equal(t, want, got,
				"error message changed; run `make update-golden` if the new text reads better")
		})
		seen[schemaTypeOfExample(t, path)] = true
	}
	for _, mt := range domain.MessageTypes() {
		assert.Truef(t, seen[mt], "no invalid example for %s in docs/schemas/examples/invalid", mt)
	}
}

func TestSchemaPublishedDocsMatchTheEmbeddedFiles(t *testing.T) {
	t.Parallel()
	for _, mt := range domain.MessageTypes() {
		t.Run(mt.String(), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(schemaDocsDir, schemaFileName(mt))
			embedded := mustSchemaJSON(t, mt)
			if schemaUpdateGolden() {
				require.NoError(t, os.WriteFile(path, embedded, 0o644)) //nolint:gosec // published docs
			}
			assert.Equal(t, string(embedded), string(schemaReadFile(t, path)),
				"docs/schemas is stale; run `make update-golden`")
		})
	}
}

func TestSchemaDocsIndexListsEverything(t *testing.T) {
	t.Parallel()
	index := string(schemaReadFile(t, filepath.Join(schemaDocsDir, "README.md")))
	for _, mt := range domain.MessageTypes() {
		assert.Contains(t, index, schemaFileName(mt), "docs/schemas/README.md must list every schema")
	}
	for _, kind := range []string{"valid", "invalid"} {
		for _, path := range schemaExamples(t, kind) {
			assert.Contains(t, index, filepath.Base(path),
				"docs/schemas/README.md must list every %s example", kind)
		}
	}
}

func TestSchemaTypedParseHelpers(t *testing.T) {
	t.Parallel()
	plan, _, err := agents.ParsePlan(schemaBlock("Planned.",
		schemaReadFile(t, filepath.Join(schemaExamplesDir, "valid", "plan-brief-mode.json"))))
	require.NoError(t, err)
	require.Len(t, plan.Beads, 3)
	assert.Equal(t, "fix-mean", plan.Beads[0].Key)
	assert.Equal(t, domain.BeadTypeBug, plan.Beads[0].Type)
	assert.Equal(t, 3, plan.DependencyCount())

	report, _, err := agents.ParseImplReport(schemaBlock("Done.",
		schemaReadFile(t, filepath.Join(schemaExamplesDir, "valid", "impl_report-minimal.json"))))
	require.NoError(t, err)
	assert.NotEmpty(t, report.Summary)

	verdict, _, err := agents.ParseVerdict(schemaBlock("Reviewed.",
		schemaReadFile(t, filepath.Join(schemaExamplesDir, "valid", "verdict-changes.json"))))
	require.NoError(t, err)
	assert.Equal(t, domain.VerdictChanges, verdict.Outcome)
	assert.Equal(t, domain.CategoryRules, verdict.Items[0].Category)
	assert.Equal(t, domain.SeverityBlocker, verdict.Items[0].Severity)

	responses, _, err := agents.ParseResponses(schemaBlock("Addressed.",
		schemaReadFile(t, filepath.Join(schemaExamplesDir, "valid", "response-mixed.json"))))
	require.NoError(t, err)
	require.Len(t, responses, 3)
	assert.Equal(t, domain.ActionPushback, responses[1].Action)

	arb, _, err := agents.ParseArbitration(schemaBlock("Ruling.",
		schemaReadFile(t, filepath.Join(schemaExamplesDir, "valid", "arbitration-compromise.json"))))
	require.NoError(t, err)
	assert.Equal(t, domain.RulingCompromise, arb.Ruling)
	assert.NotEmpty(t, arb.Instruction)

	esc, _, err := agents.ParseEscalation(schemaBlock("Escalating.",
		schemaReadFile(t, filepath.Join(schemaExamplesDir, "valid", "escalation-block.json"))))
	require.NoError(t, err)
	assert.Equal(t, domain.EscalationBlock, esc.Outcome)
	assert.NotEmpty(t, esc.Summary)
}

func TestSchemaParseReportsAMissingBlock(t *testing.T) {
	t.Parallel()
	_, _, err := agents.ParsePlan("I thought about it and decided not to answer in the required format.")
	require.Error(t, err)
	assert.True(t, errors.Is(err, structured.ErrMissing), "want ErrMissing, got %v", err)

	_, _, err = agents.ParseVerdict(schemaBlock("Reviewed.", []byte(`{"outcome":"changes"}`)))
	require.Error(t, err)
	assert.True(t, errors.Is(err, structured.ErrSchema), "want ErrSchema, got %v", err)
	pe, ok := structured.AsParseError(err)
	require.True(t, ok)
	assert.Equal(t, "verdict", pe.Contract)
	assert.Contains(t, pe.Feedback(), "verdict", "the retry feedback names the contract")
}

// TestSchemaBeadCommentBlocksValidate closes the loop with P3-03: what the
// comment writer stores for each message type must satisfy that type's
// schema, `type` field and all.
func TestSchemaBeadCommentBlocksValidate(t *testing.T) {
	t.Parallel()
	payloads := map[domain.MessageType]any{
		domain.MessagePlan: domain.Plan{
			Brief: "Fix the statistics helpers.",
			Beads: []domain.PlanBead{{
				Key:        "fix-mean",
				Title:      "Fix Mean on an empty slice",
				Acceptance: "Mean(nil) returns (0, false).",
				Type:       domain.BeadTypeBug,
				Priority:   domain.PriorityDefault,
				Size:       domain.SizeS,
			}},
		},
		domain.MessageImplReport: domain.ImplReport{
			Summary:      "Guarded the empty case.",
			FilesChanged: []string{"stats/stats.go"},
			Commit:       "4f2b1c9",
		},
		domain.MessageVerdict: domain.Verdict{
			Lens:    domain.LensQA,
			Round:   1,
			Outcome: domain.VerdictChanges,
			Items: []domain.FeedbackItem{{
				ID:       "qa-r1-1",
				Severity: domain.SeverityMajor,
				Title:    "The empty case is untested",
				File:     "stats/stats_test.go",
				Line:     41,
				Status:   domain.ItemOpen,
			}},
		},
		domain.MessageResponse: []domain.Response{{
			ItemID:    "qa-r1-1",
			Action:    domain.ActionFixed,
			Rationale: "Added the empty row to the table test.",
		}},
		domain.MessageArbitration: domain.Arbitration{
			ItemID:    "qa-r1-1",
			Ruling:    domain.RulingUpholdTester,
			Rationale: "The rules file is not negotiable.",
		},
		domain.MessageEscalation: domain.Escalation{
			Outcome: domain.EscalationApproveWithNotes,
			Notes:   "Naming objection left open on purpose.",
		},
	}
	for _, mt := range domain.MessageTypes() {
		payload, ok := payloads[mt]
		require.Truef(t, ok, "no payload for %s", mt)
		block, err := beads.EncodeBlock(mt, payload)
		require.NoError(t, err)
		require.NoErrorf(t, agents.ValidateJSON(mt, json.RawMessage(block)),
			"the stored comment block for %s must satisfy its schema", mt)

		decoded, err := agents.DecodeJSON(mt, json.RawMessage(block))
		require.NoError(t, err)
		assert.Equal(t, payload, decoded, "%s round trip", mt)
	}
}

func TestSchemaContractForRejectsUnknownTypes(t *testing.T) {
	t.Parallel()
	_, err := agents.ContractFor(domain.MessageType("gossip"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, agents.ErrUnknownMessageType), "want ErrUnknownMessageType, got %v", err)
	assert.Contains(t, err.Error(), "plan", "the error lists the known types")

	assert.Error(t, agents.ValidateJSON(domain.MessageType(""), json.RawMessage(`{}`)))
	_, err = agents.SchemaJSON(domain.MessageType("gossip"))
	assert.Error(t, err)
	assert.Panics(t, func() { agents.MustContractFor(domain.MessageType("gossip")) })
}

func TestSchemaPromptSuffixCarriesTheWholeSchema(t *testing.T) {
	t.Parallel()
	for _, mt := range domain.MessageTypes() {
		c := agents.MustContractFor(mt)
		suffix := c.PromptSuffix()
		assert.Contains(t, suffix, "```"+structured.Fence)
		assert.Contains(t, suffix, fmt.Sprintf("JSON Schema `%s`", mt))
		// Self-contained: no $ref to another file can be seen by the model.
		assert.NotContains(t, suffix, ".schema.json#", "cross-file $refs are invisible in a prompt")
		for _, ref := range refsIn(t, mt) {
			assert.Truef(t, strings.HasPrefix(ref, "#/"), "%s: $ref %q must be local", mt, ref)
		}
	}
	assert.NotNil(t, agents.MustContracts())
}

// refsIn collects every "$ref" value in a schema document.
func refsIn(t *testing.T, mt domain.MessageType) []string {
	t.Helper()
	var walk func(v any, out []string) []string
	walk = func(v any, out []string) []string {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				if k == "$ref" {
					if s, ok := child.(string); ok {
						out = append(out, s)
					}
					continue
				}
				out = walk(child, out)
			}
		case []any:
			for _, child := range x {
				out = walk(child, out)
			}
		}
		return out
	}
	return walk(any(schemaDoc(t, mt)), nil)
}

func TestSchemaFSExposesTheFiles(t *testing.T) {
	t.Parallel()
	for _, mt := range domain.MessageTypes() {
		b, err := os.ReadFile(filepath.Join(schemaDocsDir, schemaFileName(mt)))
		require.NoError(t, err)
		assert.NotEmpty(t, b)
	}
	reg, err := structured.LoadFS(agents.SchemaFS())
	require.NoError(t, err, "the embedded FS must be loadable on its own")
	assert.Len(t, reg.Names(), len(domain.MessageTypes()))
}

// TestSchemaPropertiesMatchTheGoStructs checks that the two hand-written
// halves of the contract agree: every property named anywhere in a schema
// exists as a JSON field on the payload type, and every field of the payload
// type is described by the schema. A field added on one side only would
// otherwise be silently dropped in one direction.
func TestSchemaPropertiesMatchTheGoStructs(t *testing.T) {
	t.Parallel()
	payloads := map[domain.MessageType]any{
		domain.MessagePlan:        domain.Plan{},
		domain.MessageImplReport:  domain.ImplReport{},
		domain.MessageVerdict:     domain.Verdict{},
		domain.MessageResponse:    agents.ResponseSet{},
		domain.MessageArbitration: domain.Arbitration{},
		domain.MessageEscalation:  domain.Escalation{},
	}
	for _, mt := range domain.MessageTypes() {
		t.Run(mt.String(), func(t *testing.T) {
			t.Parallel()
			doc := schemaDoc(t, mt)
			// The top-level `type` is stamped by the comment writer, not a
			// payload field. A nested `type` (a plan bead's) is one.
			if props, ok := doc["properties"].(map[string]any); ok {
				delete(props, "type")
			}
			inSchema := schemaPropertyNames(doc)
			inGo := schemaJSONFields(reflect.TypeOf(payloads[mt]))
			assert.Equal(t, sortedKeys(inGo), sortedKeys(inSchema),
				"%s.schema.json and its Go payload type describe different fields", mt)
		})
	}
}

// schemaPropertyNames collects every key of every "properties" object in a
// schema document, at any depth.
func schemaPropertyNames(doc map[string]any) map[string]bool {
	out := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				if k == "properties" {
					if props, ok := child.(map[string]any); ok {
						for name := range props {
							out[name] = true
						}
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(doc)
	return out
}

// schemaJSONFields collects the JSON field names of a struct type and of
// every struct type reachable from it through fields, slices, arrays,
// pointers and maps.
func schemaJSONFields(rt reflect.Type) map[string]bool {
	out := map[string]bool{}
	seen := map[reflect.Type]bool{}
	var walk func(rt reflect.Type)
	walk = func(rt reflect.Type) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice ||
			rt.Kind() == reflect.Array || rt.Kind() == reflect.Map {
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct || seen[rt] {
			return
		}
		seen[rt] = true
		for i := range rt.NumField() {
			f := rt.Field(i)
			if !f.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			out[name] = true
			walk(f.Type)
		}
	}
	walk(rt)
	return out
}

// sortedKeys renders a set for a readable assertion failure.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestSchemaGenericHelpersReportFailures(t *testing.T) {
	t.Parallel()
	_, err := agents.DecodeJSON(domain.MessageVerdict, json.RawMessage(`{"outcome":"maybe"}`))
	require.Error(t, err)
	assert.True(t, errors.Is(err, structured.ErrSchema), "want ErrSchema, got %v", err)

	_, err = agents.DecodeJSON(domain.MessageType("gossip"), json.RawMessage(`{}`))
	assert.True(t, errors.Is(err, agents.ErrUnknownMessageType), "want ErrUnknownMessageType, got %v", err)

	_, _, err = agents.ParseMessage(domain.MessageType("gossip"), schemaBlock("x", []byte(`{}`)))
	assert.True(t, errors.Is(err, agents.ErrUnknownMessageType), "want ErrUnknownMessageType, got %v", err)

	_, parsed, err := agents.ParseMessage(domain.MessagePlan, "no block here at all")
	assert.True(t, errors.Is(err, structured.ErrMissing), "want ErrMissing, got %v", err)
	assert.Empty(t, parsed.JSON)

	payload, parsed, err := agents.ParseMessage(domain.MessageResponse,
		schemaBlock("Addressed.", []byte(`{"responses":[{"item_id":"qa-r1-1","action":"fixed"}]}`)))
	require.NoError(t, err)
	assert.NotEmpty(t, parsed.JSON)
	assert.Equal(t, []domain.Response{{ItemID: "qa-r1-1", Action: domain.ActionFixed}}, payload)
}
