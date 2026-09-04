package agents

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
	"slop-cannon/internal/logging"
)

// Secrets hygiene (P9-05).
//
// Two different leaks have to be stopped, and they need two different tools.
//
// The first is a *path*: a prompt that lists ".env", "id_rsa" or
// "deploy.pem" in the repository tree is an invitation for an agent with a
// read tool to open them. [SecretFilter] drops those entries from the
// repository context, from a diff, and from any other path list a prompt is
// built from, using the built-in patterns below plus whatever the user's
// `exclude` config key adds.
//
// The second is a *value*: a token pasted into a bead description, printed
// by a failing check, or echoed by an agent. Those are masked rather than
// dropped, by the P0-07 redaction hook, so the surrounding text still makes
// sense. [SecretFilter.Text] is that hook; it extends
// [logging.DefaultPatterns] with the three rules below, which is why a log
// line, an event and a prompt all mask the same things.

// DefaultSecretPatterns are the path patterns every run excludes, whatever
// the configuration says: dotenv files, PEM-encoded keys, and the id_*
// private keys ssh-keygen writes. They are matched against a path's base
// name and against every one of its parent directories, so ".env" also
// excludes "config/.env" and "id_*" also excludes ".ssh/id_ed25519".
//
// The list is deliberately short. Anything else a repository must not show
// an agent goes in the `exclude` config key (config.Config.Exclude), which
// is unioned with this one.
func DefaultSecretPatterns() []string {
	return []string{".env", ".env.*", "*.env", "*.pem", "id_*"}
}

// SecretPatterns returns the redaction rules [SecretFilter.Text] adds to
// [logging.DefaultPatterns]: an environment-style assignment whose name
// looks like a credential, the body of a PEM private key, and a bare AWS
// access key id. Each follows the [logging.Pattern] contract — exactly one
// capturing group, holding the part to mask.
func SecretPatterns() []logging.Pattern {
	return []logging.Pattern{
		{
			Name: "assignment",
			// A dotenv, YAML or shell assignment whose name looks like a
			// credential: anchored to the start of a line (after the "+" or
			// "-" of a diff and an optional "export"), with an upper-case
			// name, because that is what an environment variable looks like
			// and it is what keeps the rule from firing inside an
			// expression — `apiKey := os.Getenv("API_KEY")` in a reviewed
			// diff must survive intact.
			Regexp: regexp.MustCompile(`(?m)^[+\- ]?\s*(?:export\s+)?[A-Z][A-Z0-9_]*(?:TOKEN|SECRET|PASSWD|PASSWORD|KEY|CREDENTIAL)[A-Z0-9_]*\s*[=:]\s*["']?([^\s"']{6,})`),
		},
		{
			Name:   "pem",
			Regexp: regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----(.+?)-----END`),
		},
		{
			Name:   "aws",
			Regexp: regexp.MustCompile(`\b(?:AKIA|ASIA)([0-9A-Z]{12,})`),
		},
	}
}

// SecretFilter keeps secrets out of the prompts, transcripts and events of
// one run. Build it with [NewSecretFilter] from the configured `exclude`
// list; the zero value and a nil pointer both behave like a filter with no
// configured patterns, so the built-in ones still apply.
//
// It is immutable once built and safe for concurrent use.
type SecretFilter struct {
	// patterns are the built-in patterns followed by the configured ones,
	// already trimmed, de-duplicated and validated.
	patterns []string
	// redactor masks secret values in free text.
	redactor *logging.Redactor
}

// defaultSecretFilter is what a nil or zero SecretFilter falls back to.
var defaultSecretFilter = mustSecretFilter()

// mustSecretFilter builds the built-ins-only filter; the built-in patterns
// are constants, so they cannot fail to compile.
func mustSecretFilter() *SecretFilter {
	f, err := NewSecretFilter()
	if err != nil {
		panic("agents: built-in secret patterns are invalid: " + err.Error())
	}
	return f
}

// NewSecretFilter returns a filter that excludes [DefaultSecretPatterns]
// plus exclude, which is the `exclude` config key (config.Config.Exclude).
// A pattern is a slash-separated glob in the syntax of [path.Match]; an
// empty or blank entry is ignored, and a malformed one is an error naming
// the pattern, because silently ignoring it would leave the user believing
// a file is hidden when it is not.
func NewSecretFilter(exclude ...string) (*SecretFilter, error) {
	f := &SecretFilter{redactor: logging.NewRedactor(SecretPatterns()...)}
	seen := map[string]bool{}
	add := func(p string, configured bool) error {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, "/")
		if p == "" || seen[p] {
			return nil
		}
		if _, err := path.Match(p, "probe"); err != nil {
			if configured {
				return fmt.Errorf("agents: exclude pattern %q is malformed: %w", p, err)
			}
			return fmt.Errorf("agents: built-in exclude pattern %q is malformed: %w", p, err)
		}
		seen[p] = true
		f.patterns = append(f.patterns, p)
		return nil
	}
	for _, p := range DefaultSecretPatterns() {
		if err := add(p, false); err != nil {
			return nil, err
		}
	}
	for _, p := range exclude {
		if err := add(p, true); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// resolve returns f, or the built-ins-only filter when f is nil or was
// built by hand.
func (f *SecretFilter) resolve() *SecretFilter {
	if f == nil || len(f.patterns) == 0 {
		return defaultSecretFilter
	}
	return f
}

// Patterns returns the effective exclude patterns, built-ins first, in the
// order they are matched. The slice is a copy.
func (f *SecretFilter) Patterns() []string {
	f = f.resolve()
	return append([]string(nil), f.patterns...)
}

// Match reports whether p — a repository-relative path, with "/" separators
// and an optional trailing "/" for a directory — is excluded, and by which
// pattern. A pattern is tried against the base name, against the whole
// relative path, and against every parent directory name, so "*.pem"
// excludes "certs/server.pem" and "id_*" excludes ".ssh/id_ed25519".
func (f *SecretFilter) Match(p string) (pattern string, excluded bool) {
	f = f.resolve()
	clean := strings.Trim(strings.TrimSpace(path.Clean("/"+strings.TrimSpace(p))), "/")
	if clean == "" || clean == "." {
		return "", false
	}
	segments := strings.Split(clean, "/")
	for _, pat := range f.patterns {
		if ok, _ := path.Match(pat, clean); ok {
			return pat, true
		}
		for _, seg := range segments {
			if ok, _ := path.Match(pat, seg); ok {
				return pat, true
			}
		}
	}
	return "", false
}

// Excluded reports whether p is excluded.
func (f *SecretFilter) Excluded(p string) bool {
	_, ok := f.Match(p)
	return ok
}

// Paths returns the entries of list that are not excluded, in order. The
// result is a fresh slice; nil in, nil out.
func (f *SecretFilter) Paths(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, p := range list {
		if !f.Excluded(p) {
			out = append(out, p)
		}
	}
	return out
}

// Text masks the secret values in s: everything [logging.Redact] masks plus
// the [SecretPatterns] rules. It is the redaction hook every transcript
// line, event field and quoted piece of agent prose passes through (P9-09).
// Text with nothing to mask is returned unchanged.
//
// It redacts and does not scrub: tabs and escape sequences are left alone,
// because this text is going into a prompt, where a tab is part of the code
// under review. Text bound for a terminal — a log line, a TUI row, the
// headless renderer — wants [logging.SafeText], which scrubs first and
// redacts second; both rules are idempotent, so applying this one as well
// costs a scan and changes nothing.
func (f *SecretFilter) Text(s string) string {
	return f.resolve().redactor.Redact(s)
}

// Redactor returns the redactor Text uses, for callers that need to install
// it somewhere else — a [logging.Options] for the run's log files, or the
// event constructors of P9-09.
func (f *SecretFilter) Redactor() *logging.Redactor { return f.resolve().redactor }

// Texts is [SecretFilter.Text] over a list, on a copy; nil in, nil out. It
// is what a prompt's free-form notes go through — the orchestrator's line
// about a failed check command quotes what the command printed.
func (f *SecretFilter) Texts(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	f = f.resolve()
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = f.Text(s)
	}
	return out
}

// Message returns a copy of one structured agent message — a plan, an
// implementation report, a verdict, responses, arbitrations, an escalation
// or a bead — with the prose a model wrote masked by [SecretFilter.Text].
// Anything else comes back unchanged. It is [events.RedactMessage] with
// this run's rules, so the walk is the one [events.Redacted] makes rather
// than a second copy of it.
//
// The two sinks that need it are the ones that are not events: the comment
// the feedback loop writes on the bead, and the pull request body the
// delivery hands to `gh`, which is the only one of the three that leaves the
// machine.
func (f *SecretFilter) Message(msg any) any {
	return events.RedactMessage(msg, f.resolve().Text)
}

// Report is [SecretFilter.Message] for an implementation report, keeping the
// pointer shape the delivery request uses. A nil report returns nil.
func (f *SecretFilter) Report(r *domain.ImplReport) *domain.ImplReport {
	if r == nil {
		return nil
	}
	out, ok := f.Message(*r).(domain.ImplReport)
	if !ok {
		return r
	}
	return &out
}

// Verdicts is [SecretFilter.Message] for a list of tester verdicts, on a
// copy; nil in, nil out.
func (f *SecretFilter) Verdicts(list []domain.Verdict) []domain.Verdict {
	if len(list) == 0 {
		return nil
	}
	out, ok := f.Message(list).([]domain.Verdict)
	if !ok {
		return list
	}
	return out
}

// Items is [SecretFilter.Message] for a list of feedback items, on a copy;
// nil in, nil out. The delivery raises one out of a failed check command's
// output (P6-06), which is where a credential in the environment surfaces.
func (f *SecretFilter) Items(list []domain.FeedbackItem) []domain.FeedbackItem {
	if len(list) == 0 {
		return nil
	}
	out, ok := f.Message(list).([]domain.FeedbackItem)
	if !ok {
		return list
	}
	return out
}

// Bead is [SecretFilter.Message] for a bead: the title, description and
// acceptance criteria a product owner or a human wrote, masked. The id,
// state and numbers are sc's own and are left alone.
func (f *SecretFilter) Bead(b domain.Bead) domain.Bead {
	out, ok := f.Message(b).(domain.Bead)
	if !ok {
		return b
	}
	return out
}

// Scan is [prompt.ScanRepo] with the excluded files removed: the repository
// context an agent may see. Use it instead of prompt.ScanRepo wherever a
// prompt is built.
//
// The entry cap in opts is applied by the scan, before the filter, so a
// repository whose first entries are all excluded shows fewer than
// ScanOptions.MaxEntries of them. That is the safe direction to be wrong in.
func (f *SecretFilter) Scan(dir string, opts prompt.ScanOptions) (prompt.RepoContext, error) {
	rc, err := prompt.ScanRepo(dir, opts)
	if err != nil {
		return rc, err
	}
	return f.RepoContext(rc), nil
}

// RepoContext returns rc with the excluded entries dropped from the tree
// and the README head redacted, so a README that pastes a token in its
// quickstart does not put it in every prompt of the run. Everything else is
// copied unchanged.
func (f *SecretFilter) RepoContext(rc prompt.RepoContext) prompt.RepoContext {
	rc.Tree = f.Paths(rc.Tree)
	rc.ReadmeHead = f.Text(rc.ReadmeHead)
	return rc
}

// History returns a copy of the feedback rounds a prompt quotes, with each
// round's diff filtered by [SecretFilter.Diff] and its notes redacted by
// [SecretFilter.Text]. Those are the two fields of a round that carry text
// straight from a subprocess: the patch git printed and the orchestrator's
// note about, say, a failed check command. The prose a model wrote — the
// report, the verdicts, the responses — is redacted where the event is
// built (P9-09) before it is ever stored, so it arrives here already safe.
//
// nil in, nil out; the input is never modified.
func (f *SecretFilter) History(rounds []prompt.Round) []prompt.Round {
	if len(rounds) == 0 {
		return nil
	}
	f = f.resolve()
	out := make([]prompt.Round, len(rounds))
	for i, r := range rounds {
		r.Diff = f.Diff(r.Diff)
		if len(r.Notes) > 0 {
			notes := make([]string, len(r.Notes))
			for j, n := range r.Notes {
				notes[j] = f.Text(n)
			}
			r.Notes = notes
		}
		out[i] = r
	}
	return out
}

// SecretPlaceholder is the line a dropped diff section is replaced with. It
// says a change exists and why it cannot be seen, so a reviewer knows the
// diff is partial, and it deliberately names neither the path nor the
// pattern: an agent that reads the worktree would only need the path to open
// the file the exclusion was protecting.
const SecretPlaceholder = "[a change to an excluded path was withheld: the path matches a secrets exclude pattern]"

// Diff returns a unified diff with the sections for excluded files replaced
// by a one-line placeholder, and the remaining text redacted. A change to
// ".env" is exactly the shape of leak that matters most — the file the tree
// summary already hides, arriving through the reviewer's diff instead.
//
// Both `git diff` output (sections opened by "diff --git a/x b/x") and a
// plain unified diff (sections opened by "--- x" followed by "+++ y") are
// understood; anything before the first section header is passed through.
func (f *SecretFilter) Diff(diff string) string {
	if strings.TrimSpace(diff) == "" {
		return diff
	}
	f = f.resolve()
	lines := strings.Split(diff, "\n")
	var b strings.Builder
	b.Grow(len(diff))
	dropping, git := false, strings.Contains(diff, "\ndiff --git ") || strings.HasPrefix(diff, "diff --git ")
	for i, line := range lines {
		if p, ok := diffSectionPath(lines, i, git); ok {
			dropping = f.Excluded(p)
			if dropping {
				b.WriteString(SecretPlaceholder + "\n")
				continue
			}
		}
		if dropping {
			continue
		}
		b.WriteString(line)
		if i < len(lines)-1 {
			b.WriteString("\n")
		}
	}
	return f.Text(b.String())
}

// diffSectionPath reports whether lines[i] opens a new file section of a
// unified diff and, if so, the path it is about. In a git diff only the
// "diff --git" header opens a section: the "--- a/x"/"+++ b/x" pair inside
// one would otherwise open a second section for the same file, and the
// section would be reported (and withheld) twice.
func diffSectionPath(lines []string, i int, git bool) (string, bool) {
	line := lines[i]
	if rest, ok := strings.CutPrefix(line, "diff --git "); ok {
		return gitDiffPath(rest), true
	}
	if git {
		return "", false
	}
	// A plain unified diff: "--- old" immediately followed by "+++ new".
	if strings.HasPrefix(line, "--- ") && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "+++ ") {
		if p := diffFilePath(lines[i+1][len("+++ "):]); p != "" && p != "/dev/null" {
			return p, true
		}
		return diffFilePath(line[len("--- "):]), true
	}
	return "", false
}

// gitDiffPath extracts the file from the tail of a "diff --git" header. It
// prefers the b/ side (the new name) and falls back to the a/ side, which
// is what a deletion leaves behind.
func gitDiffPath(rest string) string {
	// git writes the two sides as "a/<path> b/<path>" and does not escape a
	// space, so splitting on fields would cut a path in half; the " b/"
	// separator is what actually divides them.
	if i := strings.Index(rest, " b/"); i > 0 && !strings.HasPrefix(rest, `"`) {
		if p := diffFilePath(rest[i+1:]); p != "" {
			return p
		}
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	last := diffFilePath(fields[len(fields)-1])
	if last != "" && last != "/dev/null" {
		return last
	}
	return diffFilePath(fields[0])
}

// diffFilePath strips the a/ or b/ prefix, the quoting git applies to a
// path with unusual characters, and the trailing timestamp of a plain
// unified diff header.
func diffFilePath(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, `"`) {
		if unquoted, err := strconv.Unquote(s); err == nil {
			s = unquoted
		}
	}
	if tab := strings.IndexByte(s, '\t'); tab >= 0 {
		s = s[:tab]
	}
	for _, prefix := range []string{"a/", "b/", "i/", "w/", "c/", "o/"} {
		if rest, ok := strings.CutPrefix(s, prefix); ok {
			return rest
		}
	}
	return s
}

// GitDiff returns a copy of d with every excluded file removed: from the
// file list, from the stats and from the patch. It is what a reviewer is
// shown, so a change to ".env" is reported as withheld rather than quoted
// line by line.
//
// The copy's stats no longer count the withheld files, which keeps the
// "3 files changed" line honest about what the reviewer can see. That makes
// it the wrong value for the D-09 change ratio: measure that on the diff
// gitx returned, not on this one. A nil diff returns nil.
func (f *SecretFilter) GitDiff(d *gitx.Diff) *gitx.Diff {
	if d == nil {
		return nil
	}
	f = f.resolve()
	out := *d
	out.Files = make([]gitx.DiffFile, 0, len(d.Files))
	out.Stats.Changed = make([]gitx.FileStat, 0, len(d.Stats.Changed))
	out.Stats.Files, out.Stats.Insertions, out.Stats.Deletions = 0, 0, 0
	for _, file := range d.Files {
		if f.Excluded(file.Path) || (file.OldPath != "" && f.Excluded(file.OldPath)) {
			continue
		}
		out.Files = append(out.Files, file)
	}
	for _, stat := range d.Stats.Changed {
		if f.Excluded(stat.Path) || (stat.OldPath != "" && f.Excluded(stat.OldPath)) {
			continue
		}
		out.Stats.Changed = append(out.Stats.Changed, stat)
		out.Stats.Files++
		out.Stats.Insertions += stat.Insertions
		out.Stats.Deletions += stat.Deletions
	}
	out.Patch = f.Diff(d.Patch)
	return &out
}

// GitignoreEnvPatterns are the .gitignore lines that would keep a dotenv
// file out of the repository's history. Any one of them is enough.
var GitignoreEnvPatterns = []string{".env", ".env*", "*.env", ".env.*", "**/.env"}

// GitignoreHint is what to tell a user whose repository does not ignore
// .env: the exact line to add, and where.
const GitignoreHint = `add ".env" to the repository's .gitignore; agents commit what they find, and a committed .env is a leaked credential`

// GitignoreCoversEnv reports whether the repository at root ignores its
// dotenv files, by looking for one of [GitignoreEnvPatterns] in its
// .gitignore. A missing .gitignore is not an error: covered is false and err
// is nil, which is the answer that matters.
//
// It exists for the `sc doctor` row P9-05 asks for — "warn if the repo's
// .gitignore lacks .env" — because excluding a file from the prompts is only
// half the job: an engineer that commits a .env has leaked the credential
// whatever the prompt said. Comments, blank lines, directory patterns and
// negations ("!.env", which puts the file back) are all handled.
func GitignoreCoversEnv(root string) (covered bool, err error) {
	data, err := os.ReadFile(filepath.Join(root, ".gitignore")) //nolint:gosec // the target repository's own .gitignore
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("agents: reading .gitignore in %s: %w", root, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		case strings.HasPrefix(line, "!"):
			// A negation puts the file back; the safest reading of a file
			// that both ignores and un-ignores .env is "not covered".
			if gitignoreNames(line[1:], GitignoreEnvPatterns) {
				return false, nil
			}
			continue
		case strings.HasSuffix(line, "/"):
			continue // a directory pattern never matches the file itself
		}
		if gitignoreNames(line, GitignoreEnvPatterns) {
			covered = true
		}
	}
	return covered, nil
}

// gitignoreNames reports whether the .gitignore pattern line is one of want,
// ignoring a leading "/" (which only anchors it to the repository root).
func gitignoreNames(line string, want []string) bool {
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "/"))
	for _, w := range want {
		if line == w {
			return true
		}
	}
	return false
}
