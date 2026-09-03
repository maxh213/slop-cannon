package agents

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"

	"slop-cannon/internal/cartridge/structured"
	"slop-cannon/internal/domain"
)

// schemaFiles holds the JSON Schema (draft 2020-12) for every structured
// message an agent can produce. One file per domain.MessageType, named
// `<type>.schema.json`, so the contract name the schema registry derives
// from the file name is exactly the `type` the bead comment writer stamps
// on the message (D-06, D-25).
//
//go:embed schemas/*.schema.json
var schemaFiles embed.FS

// SchemasDir is the path of the embedded schema directory inside this
// package, and the directory whose contents are published under
// docs/schemas/.
const SchemasDir = "schemas"

// ErrUnknownMessageType is wrapped by every lookup for a message type that
// has no schema.
var ErrUnknownMessageType = errors.New("agents: unknown message type")

// SchemaFS returns the embedded schemas as a filesystem whose root holds
// the `<type>.schema.json` files, ready for structured.LoadFS.
func SchemaFS() fs.FS {
	sub, err := fs.Sub(schemaFiles, SchemasDir)
	if err != nil {
		// Impossible: SchemasDir is a literal directory in the embed pattern.
		panic(fmt.Errorf("agents: embedded schema dir: %w", err))
	}
	return sub
}

// contracts compiles the embedded schemas once. Compilation is
// deterministic and offline: every file is registered under
// structured.BaseURL before it is compiled, so relative `$ref`s resolve
// without a loader.
var contracts = sync.OnceValues(func() (*structured.Registry, error) {
	r, err := structured.LoadFS(SchemaFS())
	if err != nil {
		return nil, fmt.Errorf("agents: loading embedded schemas: %w", err)
	}
	for _, t := range domain.MessageTypes() {
		if !r.Has(t.String()) {
			return nil, fmt.Errorf("agents: embedded schemas are missing %s.schema.json", t)
		}
	}
	return r, nil
})

// Contracts returns the registry holding one compiled contract per
// domain.MessageType. The schemas are compiled on first use and reused
// afterwards; the registry is read-only and safe for concurrent use.
func Contracts() (*structured.Registry, error) { return contracts() }

// MustContracts is Contracts for package-level variables: it panics when
// the embedded schemas do not compile, which is a build-time bug the
// package's own tests catch.
func MustContracts() *structured.Registry {
	r, err := Contracts()
	if err != nil {
		panic(err)
	}
	return r
}

// ContractFor returns the output contract an agent producing t must
// satisfy. Use Contract.PromptSuffix to show it to the model and
// Contract.Parse to read the model's reply back. An unknown type yields an
// error wrapping ErrUnknownMessageType.
func ContractFor(t domain.MessageType) (*structured.Contract, error) {
	if !t.Valid() {
		return nil, fmt.Errorf("%w %q (known: %s)", ErrUnknownMessageType, string(t), joinMessageTypes(domain.MessageTypes()))
	}
	r, err := Contracts()
	if err != nil {
		return nil, err
	}
	return r.Get(t.String())
}

// MustContractFor is ContractFor that panics, for package-level variables.
func MustContractFor(t domain.MessageType) *structured.Contract {
	c, err := ContractFor(t)
	if err != nil {
		panic(err)
	}
	return c
}

// SchemaJSON returns a copy of the schema document for t, as published in
// docs/schemas/.
func SchemaJSON(t domain.MessageType) ([]byte, error) {
	c, err := ContractFor(t)
	if err != nil {
		return nil, err
	}
	return c.SchemaJSON(), nil
}

// ResponseSet is the payload of a `response` message: the engineer's answer
// to every feedback item it was given. The messages are a list, but the
// output contract is one JSON object, so the list is carried in a
// `responses` field — the same shape beads.EncodeBlock writes into a bead
// comment.
type ResponseSet struct {
	// Responses holds one answer per feedback item, in the order the items
	// were presented.
	Responses []domain.Response `json:"responses"`
}

// ValidateJSON checks raw against t's schema. It returns nil on success and
// a *structured.ParseError (errors.Is ~ structured.ErrSchema or
// structured.ErrInvalidJSON) otherwise, whose Error lists one readable line
// per violation. raw may be either an agent's bare result object or the
// object stored in a bead comment: every schema accepts an optional `type`
// property holding its own name.
func ValidateJSON(t domain.MessageType, raw json.RawMessage) error {
	c, err := ContractFor(t)
	if err != nil {
		return err
	}
	return c.Validate(raw)
}

// DecodeJSON validates raw against t's schema and decodes it into the
// payload type beads stores for that message: domain.Plan,
// domain.ImplReport, domain.Verdict, []domain.Response, domain.Arbitration
// or domain.Escalation.
func DecodeJSON(t domain.MessageType, raw json.RawMessage) (any, error) {
	if err := ValidateJSON(t, raw); err != nil {
		return nil, err
	}
	return decodePayload(t, raw)
}

// ParseMessage extracts the ```sc-result block from an agent's output,
// validates it against t's schema and decodes it into the payload type
// DecodeJSON documents. The returned Parsed carries the raw block even when
// the error is non-nil, and every contract-level error is a
// *structured.ParseError, so a caller can feed ParseError.Feedback back to
// the model on a retry (P5-08).
func ParseMessage(t domain.MessageType, output string) (any, structured.Parsed, error) {
	c, err := ContractFor(t)
	if err != nil {
		return nil, structured.Parsed{}, err
	}
	p, err := c.Parse(output)
	if err != nil {
		return nil, p, err
	}
	v, err := decodePayload(t, p.JSON)
	if err != nil {
		return nil, p, err
	}
	return v, p, nil
}

// ParsePlan reads a `plan` result out of an agent's output.
func ParsePlan(output string) (domain.Plan, structured.Parsed, error) {
	return parseInto[domain.Plan](domain.MessagePlan, output)
}

// ParseImplReport reads an `impl_report` result out of an agent's output.
func ParseImplReport(output string) (domain.ImplReport, structured.Parsed, error) {
	return parseInto[domain.ImplReport](domain.MessageImplReport, output)
}

// ParseVerdict reads a `verdict` result out of an agent's output.
func ParseVerdict(output string) (domain.Verdict, structured.Parsed, error) {
	return parseInto[domain.Verdict](domain.MessageVerdict, output)
}

// ParseResponses reads a `response` result out of an agent's output and
// returns its responses list.
func ParseResponses(output string) ([]domain.Response, structured.Parsed, error) {
	set, p, err := parseInto[ResponseSet](domain.MessageResponse, output)
	return set.Responses, p, err
}

// ParseArbitration reads an `arbitration` result out of an agent's output.
func ParseArbitration(output string) (domain.Arbitration, structured.Parsed, error) {
	return parseInto[domain.Arbitration](domain.MessageArbitration, output)
}

// ParseEscalation reads an `escalation` result out of an agent's output.
func ParseEscalation(output string) (domain.Escalation, structured.Parsed, error) {
	return parseInto[domain.Escalation](domain.MessageEscalation, output)
}

// parseInto is the shared body of the typed Parse helpers.
func parseInto[T any](t domain.MessageType, output string) (T, structured.Parsed, error) {
	var zero T
	c, err := ContractFor(t)
	if err != nil {
		return zero, structured.Parsed{}, err
	}
	return structured.Decode[T](c, output)
}

// decodePayload unmarshals already-validated JSON into the payload type for
// t. Validation has run, so a decode failure here is a mismatch between the
// schema and the domain type, which the package's tests forbid.
func decodePayload(t domain.MessageType, raw json.RawMessage) (any, error) {
	switch t {
	case domain.MessagePlan:
		return unmarshal[domain.Plan](t, raw)
	case domain.MessageImplReport:
		return unmarshal[domain.ImplReport](t, raw)
	case domain.MessageVerdict:
		return unmarshal[domain.Verdict](t, raw)
	case domain.MessageResponse:
		set, err := unmarshal[ResponseSet](t, raw)
		if err != nil {
			return nil, err
		}
		return set.Responses, nil
	case domain.MessageArbitration:
		return unmarshal[domain.Arbitration](t, raw)
	case domain.MessageEscalation:
		return unmarshal[domain.Escalation](t, raw)
	default:
		return nil, fmt.Errorf("%w %q", ErrUnknownMessageType, string(t))
	}
}

// unmarshal decodes raw into T, tagging the error with the message type.
func unmarshal[T any](t domain.MessageType, raw json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("agents: decoding %s message: %w", t, err)
	}
	return v, nil
}

// joinMessageTypes renders types for error messages.
func joinMessageTypes(ts []domain.MessageType) string {
	parts := make([]string, len(ts))
	for i, t := range ts {
		parts[i] = t.String()
	}
	return strings.Join(parts, ", ")
}
