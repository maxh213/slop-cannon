package structured

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Fence is the info string that tags the result block: ```sc-result.
const Fence = "sc-result"

// Source says where Extract found the JSON.
type Source string

// Block sources, from the contract's own form to the fallbacks.
const (
	// SourceFence is a ```sc-result block, the form the prompt asks for.
	SourceFence Source = "fence"
	// SourceJSONFence is a ```json (or untagged) fenced block; used only when
	// the output has no sc-result block.
	SourceJSONFence Source = "json_fence"
	// SourceBare is a JSON object found in the prose with no fence at all.
	SourceBare Source = "bare"
)

// String returns the canonical name.
func (s Source) String() string { return string(s) }

// Block is the JSON Extract pulled out of an output.
type Block struct {
	// JSON is the exact text of the JSON value, trimmed. It is valid JSON.
	JSON json.RawMessage `json:"json"`
	// Source says which form the JSON was found in.
	Source Source `json:"source"`
	// Blocks is how many sc-result blocks the output contained. When it is
	// greater than one the last block won; callers may log that.
	Blocks int `json:"blocks"`
	// Unclosed is true when the block had no closing fence but its JSON was
	// complete (the model forgot the fence). The JSON is still trusted.
	Unclosed bool `json:"unclosed,omitempty"`
}

var (
	openFenceRe  = regexp.MustCompile("^\\s{0,3}(`{3,}|~{3,})\\s*([^`~\\s]*)\\s*(.*)$")
	closeFenceRe = regexp.MustCompile("^\\s{0,3}(`{3,}|~{3,})\\s*$")
)

type fencedBlock struct {
	info   string
	body   string
	closed bool
}

// splitFences returns every fenced block in output in order. A block opened
// by a fence of N characters closes at the next line that is only a fence
// of the same character and at least N long; a block still open at the end
// of the output is returned with closed=false.
func splitFences(output string) []fencedBlock {
	var blocks []fencedBlock
	lines := strings.Split(output, "\n")
	var cur *fencedBlock
	var fence string
	var body []string
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if cur == nil {
			m := openFenceRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			fence = m[1]
			cur = &fencedBlock{info: strings.ToLower(m[2])}
			body = body[:0]
			continue
		}
		if m := closeFenceRe.FindStringSubmatch(line); m != nil && m[1][0] == fence[0] && len(m[1]) >= len(fence) {
			cur.body = strings.Join(body, "\n")
			cur.closed = true
			blocks = append(blocks, *cur)
			cur = nil
			continue
		}
		body = append(body, line)
	}
	if cur != nil {
		cur.body = strings.Join(body, "\n")
		blocks = append(blocks, *cur)
	}
	return blocks
}

// decodeFirst decodes the first JSON value in s and returns its exact text
// and whether anything but whitespace follows it. The error is the
// decoder's, so its message names the offset and the problem.
func decodeFirst(s string) (text string, trailing bool, err error) {
	s = strings.TrimSpace(s)
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", false, err
	}
	end := dec.InputOffset()
	text = strings.TrimSpace(s[:end])
	rest := strings.TrimSpace(s[end:])
	return text, rest != "", nil
}

func isUnexpectedEOF(err error) bool {
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var se *json.SyntaxError
	return errors.As(err, &se) && strings.Contains(se.Error(), "unexpected end of JSON input")
}

// Extract finds the result block in a model's final text. Rules, in order:
//
//   - The last ```sc-result block wins. A closed block must hold exactly one
//     JSON value (ReasonInvalidJSON otherwise). An unclosed block, when the
//     output ended inside it, is accepted if its JSON is complete (the
//     model forgot the closing fence; Block.Unclosed is set) and reported
//     as ReasonTruncated when it is not.
//   - With no sc-result block, the last closed ```json or untagged fenced
//     block holding one JSON value is used (SourceJSONFence).
//   - Failing that, the last JSON object in the prose is used (SourceBare).
//   - Otherwise ReasonMissing.
//
// The fallbacks exist so a model that forgets the tag still gets its result
// validated instead of being fired again; the schema check that follows
// catches JSON that was never meant as the result.
func Extract(output string) (Block, error) {
	fences := splitFences(output)
	var scBlocks []fencedBlock
	for _, f := range fences {
		if f.info == Fence {
			scBlocks = append(scBlocks, f)
		}
	}
	if n := len(scBlocks); n > 0 {
		last := scBlocks[n-1]
		text, trailing, err := decodeFirst(last.body)
		switch {
		case !last.closed && err != nil:
			return Block{Blocks: n}, &ParseError{Reason: ReasonTruncated, Detail: "the output ended inside the block: " + err.Error(), Cause: err}
		case err != nil:
			reason := ReasonInvalidJSON
			if isUnexpectedEOF(err) {
				reason = ReasonTruncated
			}
			return Block{Blocks: n}, &ParseError{Reason: reason, Detail: err.Error(), Cause: err}
		case last.closed && trailing:
			return Block{Blocks: n}, &ParseError{Reason: ReasonInvalidJSON, Detail: "the block holds more than one JSON value; only one object is allowed"}
		}
		return Block{JSON: json.RawMessage(text), Source: SourceFence, Blocks: n, Unclosed: !last.closed}, nil
	}
	for i := len(fences) - 1; i >= 0; i-- {
		f := fences[i]
		if !f.closed || (f.info != "json" && f.info != "") {
			continue
		}
		text, trailing, err := decodeFirst(f.body)
		if err != nil || trailing {
			continue
		}
		return Block{JSON: json.RawMessage(text), Source: SourceJSONFence}, nil
	}
	if text, ok := lastBareObject(output); ok {
		return Block{JSON: json.RawMessage(text), Source: SourceBare}, nil
	}
	return Block{}, &ParseError{Reason: ReasonMissing, Detail: "no ```sc-result block and no JSON object in the output"}
}

// lastBareObject returns the last top-level JSON object in s: candidates
// are scanned left to right and each decoded object is skipped over, so a
// nested object never wins over the one that contains it.
func lastBareObject(s string) (string, bool) {
	var found string
	for i := strings.Index(s, "{"); i >= 0; {
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		dec.UseNumber()
		var v map[string]any
		if err := dec.Decode(&v); err != nil {
			next := strings.Index(s[i+1:], "{")
			if next < 0 {
				break
			}
			i += 1 + next
			continue
		}
		end := i + int(dec.InputOffset())
		found = strings.TrimSpace(s[i:end])
		next := strings.Index(s[end:], "{")
		if next < 0 {
			break
		}
		i = end + next
	}
	return found, found != ""
}

// Decode unmarshals the block's JSON into v.
func (b Block) Decode(v any) error {
	if len(b.JSON) == 0 {
		return &ParseError{Reason: ReasonMissing, Detail: "empty block"}
	}
	dec := json.NewDecoder(bytes.NewReader(b.JSON))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return &ParseError{Reason: ReasonInvalidJSON, Detail: fmt.Sprintf("cannot decode into %T: %v", v, err), Cause: err}
	}
	return nil
}
