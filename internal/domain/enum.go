package domain

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnknownValue is wrapped by every enum parse failure in this package, so
// callers can test for it with errors.Is regardless of which enum failed.
var ErrUnknownValue = errors.New("unknown value")

// enum is the shared machinery behind every string enumeration in this
// package. Each enum type keeps one package-level instance listing its valid
// values in declaration order and delegates String, Parse, Valid and the text
// marshalling methods to it.
type enum[T ~string] struct {
	kind   string
	values []T
}

// normalize folds user input into the canonical spelling: trimmed,
// lower-cased, hyphens replaced by underscores.
func normalize(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "_")
}

// parse returns the canonical value matching s, or an error wrapping
// ErrUnknownValue that lists the accepted values. Empty input is an error.
func (e enum[T]) parse(s string) (T, error) {
	var zero T
	n := normalize(s)
	if n == "" {
		return zero, fmt.Errorf("%w: empty %s (want one of %s)", ErrUnknownValue, e.kind, e.list())
	}
	for _, v := range e.values {
		if string(v) == n {
			return v, nil
		}
	}
	return zero, fmt.Errorf("%w: %s %q (want one of %s)", ErrUnknownValue, e.kind, s, e.list())
}

// valid reports whether v is exactly one of the canonical values.
func (e enum[T]) valid(v T) bool {
	for _, x := range e.values {
		if x == v {
			return true
		}
	}
	return false
}

// marshal implements encoding.TextMarshaler semantics: the zero value
// marshals as the empty string, a valid value as itself, anything else fails.
func (e enum[T]) marshal(v T) ([]byte, error) {
	if v == "" {
		return []byte{}, nil
	}
	if !e.valid(v) {
		return nil, fmt.Errorf("%w: cannot marshal %s %q (want one of %s)", ErrUnknownValue, e.kind, string(v), e.list())
	}
	return []byte(v), nil
}

// unmarshal implements encoding.TextUnmarshaler semantics: empty text yields
// the zero value, anything else must parse.
func (e enum[T]) unmarshal(b []byte) (T, error) {
	var zero T
	if strings.TrimSpace(string(b)) == "" {
		return zero, nil
	}
	return e.parse(string(b))
}

// list renders the accepted values for error messages.
func (e enum[T]) list() string {
	parts := make([]string, len(e.values))
	for i, v := range e.values {
		parts[i] = string(v)
	}
	return strings.Join(parts, ", ")
}

// all returns a copy of the canonical values in declaration order.
func (e enum[T]) all() []T {
	out := make([]T, len(e.values))
	copy(out, e.values)
	return out
}
