package logging

import (
	"regexp"
	"strings"
	"testing"
)

// The redaction prefilter (P9-26). The whole risk of the fast path is a
// false negative: a string the byte scan calls clean and a rule would have
// masked. Every test here is aimed at that, and the last of them fuzzes it.

// pfProductionPatterns is every rule a real run carries: the built-in ones
// plus the three agents.SecretPatterns adds (P9-05). They are repeated here
// rather than imported because internal/agents imports this package.
func pfProductionPatterns() []Pattern {
	return append(DefaultPatterns(),
		Pattern{
			Name:   "assignment",
			Regexp: regexp.MustCompile(`(?m)(?:^|[^A-Za-z0-9_])(?:export\s+)?[A-Z][A-Z0-9_]*(?:TOKEN|SECRET|PASSWD|PASSWORD|KEY|CREDENTIAL)[A-Z0-9_]*\s*[=:]\s*["']?([^\s"']{6,})`),
		},
		Pattern{Name: "pem", Regexp: regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----(.+?)-----END`)},
		Pattern{Name: "aws", Regexp: regexp.MustCompile(`\b(?:AKIA|ASIA)([0-9A-Z]{12,})`)},
	)
}

// TestPrefilterDerivesTheWordsEveryRuleNeeds pins what the derivation makes
// of the six rules production runs with. It is the reason no rule has to
// carry a hand-written list of its own words: a regexp that grows another
// alternative is filtered by the new one automatically.
func TestPrefilterDerivesTheWordsEveryRuleNeeds(t *testing.T) {
	t.Parallel()
	want := map[string]struct {
		words []string
		fold  bool
	}{
		"sk":         {words: []string{"sk-"}},
		"github":     {words: []string{"gho_", "ghp_", "ghr_", "ghs_", "ghu_", "github_pat_"}},
		"bearer":     {words: []string{"bearer"}, fold: true},
		"assignment": {words: []string{"CREDENTIAL", "KEY", "PASSWD", "PASSWORD", "SECRET", "TOKEN"}},
		"pem":        {words: []string{"PRIVATE KEY-----"}},
		"aws":        {words: []string{"AKIA", "ASIA"}},
	}
	for _, p := range pfProductionPatterns() {
		exp, ok := want[p.Name]
		if !ok {
			t.Fatalf("no expectation for rule %q", p.Name)
		}
		var got []string
		for _, n := range requiredLiterals(p.Regexp) {
			if n.fold != exp.fold {
				t.Errorf("%s: needle %q fold = %v, want %v", p.Name, n.text, n.fold, exp.fold)
			}
			got = append(got, n.text)
		}
		if strings.Join(sorted(got), ",") != strings.Join(sorted(exp.words), ",") {
			t.Errorf("%s: derived %v, want %v", p.Name, got, exp.words)
		}
	}
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// TestPrefilterSkipsNothingARuleWouldMask is the safety property, over the
// samples that matter: a rule that matches is always among the candidates
// the scan returns.
func TestPrefilterSkipsNothingARuleWouldMask(t *testing.T) {
	t.Parallel()
	pats := pfProductionPatterns()
	pf := newPrefilter(pats)
	for _, s := range pfCorpus() {
		mask := pf.candidates(s)
		for i, p := range pats {
			if p.Regexp.MatchString(s) && !prefilterWants(mask, i) {
				t.Errorf("the scan called %q clean, but the %s rule matches it", s, p.Name)
			}
		}
	}
}

// TestPrefilterCallsCleanTextClean is the other half: the fast path is only
// worth having if it really does answer no. These are lines a run publishes
// by the thousand, including the ones whose words come close.
func TestPrefilterCallsCleanTextClean(t *testing.T) {
	t.Parallel()
	pf := newPrefilter(pfProductionPatterns())
	for _, s := range []string{
		"",
		"ok   slop-cannon/internal/proc  0.42s",
		"--- FAIL: TestThing (0.03s)",
		"a thorough review of the highlighted section",   // "gh" twice, no rule
		"the monkey pressed every key on the keyboard",   // "key", but lower case
		"an assessment of bd-12, bd-13 and the sk of it", // no "sk-", no "ghp_"
		"bearing in mind the bear market",                // not "bearer"
		"applied 3 patches to internal/gitx/resume.go",   // "git"
		"the plan has 4 beads and 2 dependency edges",    //
	} {
		if got := pf.candidates(s); got != 0 {
			t.Errorf("candidates(%q) = %b, want no rule at all", s, got)
		}
	}
}

// TestRedactIsWhatTheUnfilteredRulesWouldDo compares the fast path with the
// same rules run one by one, over every sample in the corpus. The reference
// is a Redactor built without a prefilter, which is exactly the code that
// shipped before P9-26.
func TestRedactIsWhatTheUnfilteredRulesWouldDo(t *testing.T) {
	t.Parallel()
	pats := pfProductionPatterns()
	fast := &Redactor{patterns: pats, pre: newPrefilter(pats)}
	slow := &Redactor{patterns: pats} // a nil prefilter runs every rule
	for _, s := range pfCorpus() {
		if got, want := fast.Redact(s), slow.Redact(s); got != want {
			t.Errorf("Redact(%q) = %q with the scan, %q without it", s, got, want)
		}
		if got, want := fast.contains(s), slow.contains(s); got != want {
			t.Errorf("contains(%q) = %v with the scan, %v without it", s, got, want)
		}
	}
}

// TestPrefilterRunsARuleItCannotReadEveryTime is the conservative fallback:
// a rule the derivation makes nothing of must be run on every string, or it
// would silently stop masking.
func TestPrefilterRunsARuleItCannotReadEveryTime(t *testing.T) {
	t.Parallel()
	// The only thing this one requires is a digit, which is too short to
	// be worth a scan (and a "-", shorter still).
	shy := Pattern{Name: "shy", Regexp: regexp.MustCompile(`\b([0-9]{4,})-([0-9]{4,})`)}
	if got := requiredLiterals(shy.Regexp); got != nil {
		t.Fatalf("requiredLiterals derived %v from a rule with no literal", got)
	}
	r := NewRedactor(shy)
	if got := r.Redact("id 12345-67890 here"); !strings.Contains(got, Redacted) {
		t.Errorf("Redact = %q, want the rule with no derivable word still applied", got)
	}
	// And a repeat that may match nothing at all leaves no requirement.
	if got := requiredLiterals(regexp.MustCompile(`(sk-)*x?`)); got != nil {
		t.Errorf("requiredLiterals derived %v from an optional literal", got)
	}
}

// pfCorpus is every string the tests above check both ways: secrets in the
// shapes the rules are written for, near misses, and ordinary run output.
func pfCorpus() []string {
	return []string{
		"", "nothing to see here", "ok   slop-cannon/internal/proc  0.42s",
		"key=sk-abcdefghijklmnop123456",
		"sk-ant-api03-AbCdEfGhIjKlMnOp_-12345 in env",
		"SK-ABCDEFGHIJKLMNOP", // upper case: the rule is case-sensitive
		"token ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
		"gho_abcdefghijklmnop", "x=ghs_abcdefghijklmnop;", "ghu_abcdefghijklmnop",
		"ghr_abcdefghijklmnop", "github_pat_11ABCDEFG0123456789_abcdefghijklmnop",
		"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig",
		"authorization: bearer abcdefghijklmnop",
		"BEARER ABCDEFGHIJKLMNOP",
		`{"auth":"Bearer abcdefghijklmnop"}`,
		"sk-abcdefghijklmnop and ghp_abcdefghijklmnop",
		"use an sk- key or ghp_ token", "sk-short ghp_short Bearer short",
		"task-123 risk-assessment", "a thorough review of the highlight",
		"export MY_API_TOKEN=hunter2222",
		"2026-01-01 INFO  DATABASE_PASSWORD: s3cr3tvalue",
		"+ANTHROPIC_API_KEY=abcdefghij",
		"NOTATOKEN=short", "apiKey := os.Getenv(\"API_KEY\")",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\n-----END RSA PRIVATE KEY-----",
		"AKIAIOSFODNN7EXAMPLE", "ASIAIOSFODNN7EXAMPLE", "akiaiosfodnn7example",
		"\x1b[32mok\x1b[0m  exporting ANTHROPIC_API_KEY=sk-ant-api03-abcdefghijklmnopqrstuvwx",
	}
}

// FuzzRedactIsWhatTheUnfilteredRulesWouldDo is the property the corpus
// above can only sample: whatever the input, the fast path masks exactly
// what running every rule would. Under `go test` it replays the seeds; run
// it with -fuzz to hunt for a string the scan calls clean and a rule would
// have caught.
func FuzzRedactIsWhatTheUnfilteredRulesWouldDo(f *testing.F) {
	for _, s := range pfCorpus() {
		f.Add(s)
	}
	pats := pfProductionPatterns()
	fast := &Redactor{patterns: pats, pre: newPrefilter(pats)}
	slow := &Redactor{patterns: pats}
	f.Fuzz(func(t *testing.T, s string) {
		if got, want := fast.Redact(s), slow.Redact(s); got != want {
			t.Errorf("Redact(%q) = %q with the scan, %q without it", s, got, want)
		}
	})
}
