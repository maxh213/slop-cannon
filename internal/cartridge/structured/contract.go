package structured

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// BaseURL is the synthetic base every schema is registered under so that
// relative `$ref`s between schema files ("verdict.json#/$defs/item") resolve
// inside the compiler without any loader or network access.
const BaseURL = "sc://schemas/"

// Contract is a named JSON Schema the model's result must satisfy.
type Contract struct {
	name   string
	schema *jsonschema.Schema
	raw    []byte
}

// NewContract compiles schema (a JSON Schema document; draft 2020-12 when it
// has no $schema) under name. Use a Registry when schemas reference each
// other.
func NewContract(name string, schema []byte) (*Contract, error) {
	r, err := load(map[string][]byte{name + ".json": schema})
	if err != nil {
		return nil, err
	}
	return r.contracts[name], nil
}

// MustContract is NewContract that panics on error, for package-level
// variables.
func MustContract(name string, schema []byte) *Contract {
	c, err := NewContract(name, schema)
	if err != nil {
		panic(err)
	}
	return c
}

// Name returns the schema name.
func (c *Contract) Name() string { return c.name }

// SchemaJSON returns a copy of the schema document as loaded.
func (c *Contract) SchemaJSON() []byte { return append([]byte(nil), c.raw...) }

// PromptSuffix is the section appended to an agent prompt: the rules for
// the ```sc-result block and the schema itself. It is deterministic, so a
// prompt builder can detect that it is already present with
// [HasPromptSuffix].
func (c *Contract) PromptSuffix() string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Output contract (%s)\n\n", c.name)
	b.WriteString("When you are completely finished, end your reply with exactly one fenced block tagged `" + Fence + "` containing a single JSON object that matches the `" + c.name + "` JSON Schema below. Rules:\n\n")
	b.WriteString("- The block is the last thing in your reply; nothing follows its closing fence.\n")
	b.WriteString("- Emit exactly one such block. Do not show drafts or examples of it earlier in the reply.\n")
	b.WriteString("- The JSON must be complete and valid: double-quoted keys and strings, no comments, no trailing commas, no placeholders or ellipses.\n")
	b.WriteString("- Put only the JSON object inside the block: no prose, no nested fences, no markdown.\n")
	b.WriteString("- Fill every required property; leave optional ones out rather than inventing values.\n\n")
	b.WriteString("```" + Fence + "\n{ ... }\n```\n\n")
	fmt.Fprintf(&b, "JSON Schema `%s`:\n\n```json\n%s\n```\n", c.name, c.schemaForPrompt())
	return b.String()
}

func (c *Contract) schemaForPrompt() string {
	var out bytes.Buffer
	if err := json.Indent(&out, c.raw, "", "  "); err != nil {
		return string(c.raw)
	}
	return out.String()
}

// HasPromptSuffix reports whether prompt contains c's suffix anywhere, so a
// caller can check the contract reached the agent at all.
func (c *Contract) HasPromptSuffix(prompt string) bool {
	return strings.Contains(prompt, c.PromptSuffix())
}

// EndsWithPromptSuffix reports whether prompt ends with c's suffix, which is
// where the contract has to be: its own first rule is that the result block
// is the last thing in the reply, so it must be the last thing in the prompt.
func (c *Contract) EndsWithPromptSuffix(prompt string) bool {
	return strings.HasSuffix(strings.TrimRight(prompt, "\n"), strings.TrimRight(c.PromptSuffix(), "\n"))
}

// WithPromptSuffix returns prompt with the contract's suffix appended once,
// at the end. The check is [EndsWithPromptSuffix] rather than
// [HasPromptSuffix] on purpose: an earlier layer that merely quotes the
// contract — a bead description, a rules file, an agent-authored summary of a
// previous round — must not suppress the real copy and leave the prompt
// ending in something else.
func (c *Contract) WithPromptSuffix(prompt string) string {
	if c.EndsWithPromptSuffix(prompt) {
		return prompt
	}
	return strings.TrimRight(prompt, "\n") + "\n\n" + c.PromptSuffix()
}

// Validate checks raw against the schema. It returns nil on success, a
// *ParseError with ReasonInvalidJSON when raw is not JSON, and one with
// ReasonSchema (Problems filled, one readable line per violation)
// otherwise.
func (c *Contract) Validate(raw json.RawMessage) error {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return &ParseError{Reason: ReasonInvalidJSON, Contract: c.name, Detail: err.Error(), Cause: err}
	}
	if err := c.schema.Validate(doc); err != nil {
		var ve *jsonschema.ValidationError
		if !errors.As(err, &ve) {
			return &ParseError{Reason: ReasonSchema, Contract: c.name, Detail: err.Error(), Cause: err}
		}
		problems := leafProblems(ve)
		return &ParseError{
			Reason:   ReasonSchema,
			Contract: c.name,
			Detail:   fmt.Sprintf("%d violation(s)", len(problems)),
			Problems: problems,
			Cause:    err,
		}
	}
	return nil
}

// leafProblems flattens the validation tree into one line per leaf cause,
// deduplicated and sorted.
//
// The order is deliberately not the validator's own (P2-12): jsonschema v6
// walks the schema's keyword maps, so two violations in one document come
// back in either order from run to run, and every golden, log diff or
// retry prompt built from Problems would be flaky. Sorting once here is
// the only place that has to know, and the lines start with the JSON
// pointer ("at '/a': …"), so lexical order is document order for anything
// nested under the same parent.
func leafProblems(ve *jsonschema.ValidationError) []string {
	out := appendLeaves(ve, nil)
	sort.Strings(out)
	return dedupeSorted(out)
}

// appendLeaves collects the message of every leaf of the validation tree.
func appendLeaves(ve *jsonschema.ValidationError, out []string) []string {
	if len(ve.Causes) == 0 {
		return append(out, ve.Error())
	}
	for _, cause := range ve.Causes {
		out = appendLeaves(cause, out)
	}
	return out
}

// dedupeSorted drops adjacent duplicates from a sorted slice, in place.
func dedupeSorted(in []string) []string {
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// Parsed is a validated result block.
type Parsed struct {
	Block
	// Contract is the schema the JSON satisfied.
	Contract string `json:"contract"`
}

// Parse extracts the block from output (see Extract) and validates it. The
// error is always a *ParseError.
func (c *Contract) Parse(output string) (Parsed, error) {
	b, err := Extract(output)
	if err != nil {
		var pe *ParseError
		if errors.As(err, &pe) {
			cp := *pe
			cp.Contract = c.name
			return Parsed{Block: b, Contract: c.name}, &cp
		}
		return Parsed{Block: b, Contract: c.name}, err
	}
	if err := c.Validate(b.JSON); err != nil {
		return Parsed{Block: b, Contract: c.name}, err
	}
	return Parsed{Block: b, Contract: c.name}, nil
}

// Decode parses output against c and unmarshals the result into T.
func Decode[T any](c *Contract, output string) (T, Parsed, error) {
	var v T
	p, err := c.Parse(output)
	if err != nil {
		return v, p, err
	}
	if err := p.Decode(&v); err != nil {
		return v, p, err
	}
	return v, p, nil
}

// Registry holds the contracts loaded from one schema directory.
type Registry struct {
	contracts map[string]*Contract
}

// LoadFS compiles every *.json file at the root of fsys into a Contract
// named after the file without its ".schema.json" or ".json" suffix. All
// files are registered under BaseURL first, so `$ref`s between them
// resolve. Subdirectories are ignored.
func LoadFS(fsys fs.FS) (*Registry, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("structured: read schema dir: %w", err)
	}
	files := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("structured: read schema %s: %w", e.Name(), err)
		}
		files[e.Name()] = b
	}
	return load(files)
}

// MustLoadFS is LoadFS that panics on error, for package-level variables.
func MustLoadFS(fsys fs.FS) *Registry {
	r, err := LoadFS(fsys)
	if err != nil {
		panic(err)
	}
	return r
}

func contractName(file string) string {
	name := path.Base(file)
	name = strings.TrimSuffix(name, ".json")
	return strings.TrimSuffix(name, ".schema")
}

func load(files map[string][]byte) (*Registry, error) {
	comp := jsonschema.NewCompiler()
	comp.DefaultDraft(jsonschema.Draft2020)
	comp.AssertFormat()
	names := make([]string, 0, len(files))
	for f := range files {
		names = append(names, f)
	}
	sort.Strings(names)
	r := &Registry{contracts: map[string]*Contract{}}
	for _, f := range names {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(files[f]))
		if err != nil {
			return nil, fmt.Errorf("structured: schema %s is not valid JSON: %w", f, err)
		}
		if err := comp.AddResource(BaseURL+f, doc); err != nil {
			return nil, fmt.Errorf("structured: register schema %s: %w", f, err)
		}
	}
	for _, f := range names {
		name := contractName(f)
		if _, dup := r.contracts[name]; dup {
			return nil, fmt.Errorf("structured: two schema files named %q", name)
		}
		sch, err := comp.Compile(BaseURL + f)
		if err != nil {
			return nil, fmt.Errorf("structured: compile schema %s: %w", f, err)
		}
		r.contracts[name] = &Contract{name: name, schema: sch, raw: append([]byte(nil), files[f]...)}
	}
	return r, nil
}

// Names returns the contract names in sorted order.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.contracts))
	for n := range r.contracts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Has reports whether a contract of that name is loaded.
func (r *Registry) Has(name string) bool {
	_, ok := r.contracts[name]
	return ok
}

// Get returns the named contract or an error wrapping ErrUnknownContract
// that lists the known names.
func (r *Registry) Get(name string) (*Contract, error) {
	c, ok := r.contracts[name]
	if !ok {
		return nil, fmt.Errorf("%w %q (known: %s)", ErrUnknownContract, name, strings.Join(r.Names(), ", "))
	}
	return c, nil
}

// MustGet is Get that panics on an unknown name.
func (r *Registry) MustGet(name string) *Contract {
	c, err := r.Get(name)
	if err != nil {
		panic(err)
	}
	return c
}
