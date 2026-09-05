package logging

import (
	"regexp"
	"regexp/syntax"
	"strings"
)

// The redaction prefilter (P9-26).
//
// [Redactor.Redact] is run on every untrusted field of every event, and
// almost every one of them is clean: an agent's output line, a bead title, a
// verdict's prose. Asking each rule's regular expression whether it matches
// cost about 900 ns apiece, so the six rules a run carries — three built in
// ([DefaultPatterns]) and three the secret filter adds
// (agents.SecretPatterns) — made the publication hook the larger half of a
// publish at 4.3 µs per clean field, against 2.6 µs for the run log and the
// bus beneath it (docs/performance.md).
//
// A secret, though, always contains one of a handful of literal words:
// "sk-", "ghp_", "bearer", "TOKEN", "PRIVATE KEY-----", "AKIA". A single
// pass over the field looking for those, with a 256-entry table of their
// first bytes, answers "could any rule match here?" in about 80 ns and
// returns nothing to run on a clean line.
//
// The words are derived from the rules themselves rather than written out
// beside them, so a new rule — including one a future config key compiles —
// is filtered without anybody remembering to annotate it, and a rule whose
// words cannot be derived is simply always run. The derivation is
// deliberately conservative: it may say "this text could match" when the
// regexp will not, and it must never say "it cannot" when the regexp would.

// prefilterMaxSet bounds the literal sets the derivation builds, so a rule
// like `[a-z]{3}` cannot produce thousands of needles.
const prefilterMaxSet = 16

// prefilterMinLen is the shortest word worth scanning for. A one- or
// two-byte word — the digit a `[0-9]{4,}` rule requires, say — is in most
// text anyway, so scanning for it costs the pass and saves nothing; the
// rule is better simply always run. Every rule a run carries is well over
// it ("sk-", "ghp_", "bearer", "KEY", "AKIA", "PRIVATE KEY-----").
const prefilterMinLen = 3

// needle is one literal a rule cannot match without. fold means it is
// compared without regard to ASCII case (the literal came from a
// case-insensitive rule), in which case text is already lower-cased.
type needle struct {
	text string
	fold bool
	rule int
}

// prefilter answers "which of these rules could match this text?" in one
// pass. The zero value passes everything through, which is what a redactor
// with no derivable rule wants.
type prefilter struct {
	// byFirst indexes the needles by their first byte; a folded needle is
	// registered under both cases of its first letter.
	byFirst [256][]needle
	// always is the set of rules with no derivable needle: they are run on
	// every string.
	always uint64
	// all is every rule's bit, the point at which the scan can stop.
	all uint64
	// usable is false when there are more rules than the bitmask holds, in
	// which case every rule is always run.
	usable bool
}

// newPrefilter builds the filter for pats.
func newPrefilter(pats []Pattern) *prefilter {
	pf := &prefilter{}
	if len(pats) == 0 || len(pats) > 64 {
		return pf
	}
	pf.usable = true
	for i, p := range pats {
		pf.all |= 1 << uint(i)
		ns := requiredLiterals(p.Regexp)
		if len(ns) == 0 {
			pf.always |= 1 << uint(i)
			continue
		}
		for _, n := range ns {
			n.rule = i
			pf.byFirst[n.text[0]] = append(pf.byFirst[n.text[0]], n)
			if n.fold {
				if up := upperASCII(n.text[0]); up != n.text[0] {
					pf.byFirst[up] = append(pf.byFirst[up], n)
				}
			}
		}
	}
	return pf
}

// candidates returns the bitmask of rules that could match s. A zero mask
// means the string is clean and no regular expression need be run.
func (pf *prefilter) candidates(s string) uint64 {
	if pf == nil || !pf.usable {
		return ^uint64(0)
	}
	mask := pf.always
	if mask == pf.all || s == "" {
		return mask
	}
	for i := range len(s) {
		for _, n := range pf.byFirst[s[i]] {
			bit := uint64(1) << uint(n.rule)
			if mask&bit != 0 {
				continue
			}
			if matchAt(s, i, n) {
				mask |= bit
				if mask == pf.all {
					return mask
				}
			}
		}
	}
	return mask
}

// prefilterWants reports whether the rule at index i is one the scan
// flagged. A rule past the bitmask's width is always run, which only
// happens for a redactor the scan gave up on entirely.
func prefilterWants(mask uint64, i int) bool {
	return i >= 64 || mask&(uint64(1)<<uint(i)) != 0
}

// matchAt reports whether n's literal starts at s[i]. The first byte is
// already known to match.
func matchAt(s string, i int, n needle) bool {
	if len(s)-i < len(n.text) {
		return false
	}
	for j := 1; j < len(n.text); j++ {
		c := s[i+j]
		if n.fold {
			c = lowerASCII(c)
		}
		if c != n.text[j] {
			return false
		}
	}
	return true
}

func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

func upperASCII(c byte) byte {
	if c >= 'a' && c <= 'z' {
		return c - ('a' - 'A')
	}
	return c
}

// requiredLiterals returns literals of which every string re matches
// contains at least one, or nil when it cannot derive any. Deriving nothing
// is always safe: the rule is then run on every string, which is what
// happened before the prefilter existed.
func requiredLiterals(re *regexp.Regexp) []needle {
	if re == nil {
		return nil
	}
	parsed, err := syntax.Parse(re.String(), syntax.Perl)
	if err != nil {
		return nil
	}
	return required(parsed.Simplify())
}

// literals is a set of alternatives with a shared case-sensitivity: either
// the exact strings an expression matches (see exact) or the literals one
// of which every match contains (see required).
type literals struct {
	set  []string
	fold bool
}

// minLen is the shortest alternative, which is how selective the set is.
func (l literals) minLen() int {
	if len(l.set) == 0 {
		return 0
	}
	n := len(l.set[0])
	for _, s := range l.set[1:] {
		n = min(n, len(s))
	}
	return n
}

// required returns the most selective literal set it can prove every match
// of re contains, as needles.
func required(re *syntax.Regexp) []needle {
	best, ok := requiredSet(re)
	if !ok || best.minLen() < prefilterMinLen {
		return nil
	}
	out := make([]needle, 0, len(best.set))
	for _, s := range best.set {
		out = append(out, needle{text: s, fold: best.fold})
	}
	return out
}

// requiredSet is [required]'s recursion.
func requiredSet(re *syntax.Regexp) (literals, bool) {
	switch re.Op {
	case syntax.OpLiteral:
		return exact(re)

	case syntax.OpCharClass:
		return exact(re)

	case syntax.OpCapture:
		return requiredSet(re.Sub[0])

	case syntax.OpPlus:
		return requiredSet(re.Sub[0])

	case syntax.OpRepeat:
		if re.Min < 1 {
			return literals{}, false
		}
		return requiredSet(re.Sub[0])

	case syntax.OpConcat:
		return requiredConcat(re)

	case syntax.OpAlternate:
		// Every branch must contribute, or a match could avoid the lot.
		var out literals
		for _, sub := range re.Sub {
			l, ok := requiredSet(sub)
			if !ok || l.minLen() == 0 {
				return literals{}, false
			}
			merged, ok := union(out, l)
			if !ok {
				return literals{}, false
			}
			out = merged
		}
		return out, len(out.set) > 0

	default:
		return literals{}, false
	}
}

// requiredConcat picks the most selective requirement a concatenation
// carries: every run of adjacent children whose whole language is a small
// literal set is multiplied out (so `gh[pousr]_` yields "ghp_", "gho_" and
// the rest), and every child that carries a requirement of its own is a
// candidate too. Any one of them holds for the whole concatenation, because
// a match of the concatenation contains a match of each child.
func requiredConcat(re *syntax.Regexp) (literals, bool) {
	var best literals
	var run literals
	haveRun := false
	consider := func(l literals, ok bool) {
		if !ok || l.minLen() == 0 {
			return
		}
		if l.minLen() > best.minLen() {
			best = l
		}
	}
	for _, sub := range re.Sub {
		if l, ok := exact(sub); ok {
			if !haveRun {
				run, haveRun = l, true
				continue
			}
			if merged, ok := concatSets(run, l); ok {
				run = merged
				continue
			}
		}
		if haveRun {
			consider(run, true)
			run, haveRun = literals{}, false
		}
		consider(requiredSet(sub))
	}
	if haveRun {
		consider(run, true)
	}
	return best, best.minLen() > 0
}

// exact returns the complete set of strings re matches, when that set is
// small enough to enumerate. A zero-width assertion matches the empty
// string, which is what lets `\bsk-` be read as the literal "sk-".
func exact(re *syntax.Regexp) (literals, bool) {
	switch re.Op {
	case syntax.OpEmptyMatch, syntax.OpBeginLine, syntax.OpEndLine,
		syntax.OpBeginText, syntax.OpEndText, syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return literals{set: []string{""}}, true

	case syntax.OpLiteral:
		var b strings.Builder
		fold := re.Flags&syntax.FoldCase != 0
		for _, r := range re.Rune {
			if r > 127 {
				return literals{}, false // ASCII case folding is all this scan does
			}
			c := byte(r)
			if fold {
				c = lowerASCII(c)
			}
			b.WriteByte(c)
		}
		return literals{set: []string{b.String()}, fold: fold}, true

	case syntax.OpCharClass:
		var set []string
		fold := re.Flags&syntax.FoldCase != 0
		for i := 0; i+1 < len(re.Rune); i += 2 {
			lo, hi := re.Rune[i], re.Rune[i+1]
			if lo > 127 || hi > 127 {
				return literals{}, false
			}
			for r := lo; r <= hi; r++ {
				c := byte(r)
				if fold {
					c = lowerASCII(c)
				}
				set = append(set, string(c))
				if len(set) > prefilterMaxSet {
					return literals{}, false
				}
			}
		}
		if len(set) == 0 {
			return literals{}, false
		}
		return dedupe(literals{set: set, fold: fold}), true

	case syntax.OpCapture:
		return exact(re.Sub[0])

	case syntax.OpConcat:
		out := literals{set: []string{""}}
		for _, sub := range re.Sub {
			l, ok := exact(sub)
			if !ok {
				return literals{}, false
			}
			merged, ok := concatSets(out, l)
			if !ok {
				return literals{}, false
			}
			out = merged
		}
		return out, true

	case syntax.OpAlternate:
		var out literals
		for _, sub := range re.Sub {
			l, ok := exact(sub)
			if !ok {
				return literals{}, false
			}
			merged, ok := union(out, l)
			if !ok {
				return literals{}, false
			}
			out = merged
		}
		return out, len(out.set) > 0

	default:
		return literals{}, false
	}
}

// concatSets multiplies two literal sets out, refusing to grow past
// [prefilterMaxSet]. Two sets of different case sensitivity are folded
// together as the case-insensitive one, which is the safe direction: a
// case-insensitive needle matches everything its case-sensitive twin does.
func concatSets(a, b literals) (literals, bool) {
	if len(a.set) == 0 {
		return b, true
	}
	if len(b.set) == 0 {
		return a, true
	}
	if len(a.set)*len(b.set) > prefilterMaxSet {
		return literals{}, false
	}
	fold := a.fold || b.fold
	x, y := a, b
	if fold {
		x, y = foldSet(a), foldSet(b)
	}
	out := literals{fold: fold, set: make([]string, 0, len(x.set)*len(y.set))}
	for _, l := range x.set {
		for _, r := range y.set {
			out.set = append(out.set, l+r)
		}
	}
	return dedupe(out), true
}

// union merges two alternatives' literal sets.
func union(a, b literals) (literals, bool) {
	if len(a.set) == 0 {
		return b, true
	}
	if len(b.set) == 0 {
		return a, true
	}
	if len(a.set)+len(b.set) > prefilterMaxSet {
		return literals{}, false
	}
	fold := a.fold || b.fold
	x, y := a, b
	if fold {
		x, y = foldSet(a), foldSet(b)
	}
	return dedupe(literals{fold: fold, set: append(append([]string{}, x.set...), y.set...)}), true
}

// foldSet lower-cases a set so it can be compared case-insensitively.
func foldSet(l literals) literals {
	if l.fold {
		return l
	}
	out := literals{fold: true, set: make([]string, 0, len(l.set))}
	for _, s := range l.set {
		out.set = append(out.set, strings.Map(func(r rune) rune {
			if r < 128 {
				return rune(lowerASCII(byte(r)))
			}
			return r
		}, s))
	}
	return out
}

// dedupe drops repeated alternatives, which a cross product produces
// whenever two branches share a prefix.
func dedupe(l literals) literals {
	out := literals{fold: l.fold}
	for _, s := range l.set {
		dup := false
		for _, kept := range out.set {
			if kept == s {
				dup = true
				break
			}
		}
		if !dup {
			out.set = append(out.set, s)
		}
	}
	return out
}
