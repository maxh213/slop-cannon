package logging

import (
	"fmt"
	"log/slog"
	"regexp"
)

// Redacted is the text that replaces the secret part of a matched token.
const Redacted = "[REDACTED]"

// Pattern is one redaction rule. Regexp must have exactly one capturing
// group: the text before the group is kept as a hint, the group is replaced
// by [Redacted], and anything after it is dropped.
type Pattern struct {
	// Name identifies the rule in doc comments and tests.
	Name string
	// Regexp matches one token; see the type comment for the group contract.
	Regexp *regexp.Regexp
}

// DefaultPatterns returns the built-in rules, in the order they are applied:
//
//   - sk: OpenAI/Anthropic-style keys such as "sk-…" and "sk-ant-…"
//   - github: classic and fine-grained GitHub tokens ("ghp_", "gho_", "ghu_",
//     "ghs_", "ghr_", "github_pat_")
//   - bearer: "Bearer <token>" authorization values, any case
//
// Tokens shorter than eight characters after the prefix are left alone so a
// mention like "sk-" in prose survives. The slice is fresh on every call.
func DefaultPatterns() []Pattern {
	return []Pattern{
		{Name: "sk", Regexp: regexp.MustCompile(`\bsk-([A-Za-z0-9_\-]{8,})`)},
		{Name: "github", Regexp: regexp.MustCompile(`\b(?:gh[pousr]_|github_pat_)([A-Za-z0-9_]{8,})`)},
		{Name: "bearer", Regexp: regexp.MustCompile(`(?i)\bbearer\s+([^\s"'` + "`" + `,;]{8,})`)},
	}
}

// Redactor rewrites strings so that anything matching its patterns is
// masked. The zero value redacts nothing; use [NewRedactor].
type Redactor struct {
	patterns []Pattern
}

// NewRedactor returns a Redactor using [DefaultPatterns] followed by any
// extra rules, which must follow the [Pattern] group contract.
func NewRedactor(extra ...Pattern) *Redactor {
	return &Redactor{patterns: append(DefaultPatterns(), extra...)}
}

var defaultRedactor = NewRedactor()

// Redact masks every token in s using the default rules.
func Redact(s string) string { return defaultRedactor.Redact(s) }

// Redact masks every token in s. Strings without a match are returned
// unchanged, without allocation.
func (r *Redactor) Redact(s string) string {
	if r == nil {
		return s
	}
	for _, p := range r.patterns {
		if !p.Regexp.MatchString(s) {
			continue
		}
		s = p.Regexp.ReplaceAllStringFunc(s, func(m string) string {
			loc := p.Regexp.FindStringSubmatchIndex(m)
			if len(loc) < 4 || loc[2] < 0 {
				return Redacted
			}
			return m[:loc[2]] + Redacted
		})
	}
	return s
}

// ReplaceAttr is a [slog.HandlerOptions.ReplaceAttr] hook that redacts the
// message, every string attribute, errors, and any other value whose printed
// form contains a token. Group members are visited by slog itself, so nested
// attributes are covered too.
func (r *Redactor) ReplaceAttr(_ []string, a slog.Attr) slog.Attr {
	a.Value = r.redactValue(a.Value.Resolve())
	return a
}

func (r *Redactor) redactValue(v slog.Value) slog.Value {
	switch v.Kind() {
	case slog.KindString:
		s := v.String()
		if red := r.Redact(s); red != s {
			return slog.StringValue(red)
		}
		return v
	case slog.KindGroup:
		attrs := v.Group()
		out := make([]slog.Attr, len(attrs))
		for i, a := range attrs {
			a.Value = r.redactValue(a.Value.Resolve())
			out[i] = a
		}
		return slog.GroupValue(out...)
	case slog.KindAny:
		raw := v.Any()
		var s string
		switch x := raw.(type) {
		case nil:
			return v
		case error:
			s = x.Error()
		case fmt.Stringer:
			s = x.String()
		default:
			s = fmt.Sprint(x)
		}
		// Only flatten to a string when something had to be masked, so
		// structured values keep their shape in JSON logs otherwise.
		if red := r.Redact(s); red != s {
			return slog.StringValue(red)
		}
		return v
	default:
		return v
	}
}

// ContainsSecret reports whether s has at least one token the default rules
// would redact. It is a cheap guard for callers that want to refuse rather
// than mask, such as prompt builders.
func ContainsSecret(s string) bool {
	for _, p := range defaultRedactor.patterns {
		if p.Regexp.MatchString(s) {
			return true
		}
	}
	return false
}
