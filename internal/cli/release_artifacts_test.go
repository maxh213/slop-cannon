package cli

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"text/template"

	"github.com/stretchr/testify/require"
)

// The release archive name exists in two hand-written places: the
// `name_template` goreleaser renders when it builds the tarballs, and the
// string install.sh pastes together to download one. Both files carry a
// comment saying they change together; this test is what makes that true.
// P10-09 pinned the -X ldflags the same way; this is the artefact-name half.
// Repository-root files are read through repoFile (docs_index_test.go).

var (
	nameTemplateRe = regexp.MustCompile(`(?m)^\s*name_template:\s*"([^"]*)"\s*$`)
	formatsRe      = regexp.MustCompile(`(?m)^\s*formats:\n\s*-\s*(\S+)\s*$`)
	projectNameRe  = regexp.MustCompile(`(?m)^project_name:\s*(\S+)\s*$`)
	listItemRe     = regexp.MustCompile(`(?m)^\s*-\s*(\S+)\s*$`)
	shellVarRe     = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	archiveVarRe   = regexp.MustCompile(`(?m)^ARCHIVE="([^"]*)"\s*$`)
	shellAssignRe  = regexp.MustCompile(`(?m)^PROJECT=(\S+)\s*$`)
)

// indentOf counts the leading spaces of a line.
func indentOf(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }

// goreleaserYAMLBlock returns the lines nested under the first `key:` line,
// without the key line itself: the block ends at the next line indented no
// further than the key. Enough YAML for the few lists this test reads.
func goreleaserYAMLBlock(t *testing.T, yaml, key string) string {
	t.Helper()
	lines := strings.Split(yaml, "\n")
	start, keyIndent := -1, 0
	for i, line := range lines {
		if strings.TrimSpace(line) == key+":" || strings.HasPrefix(strings.TrimSpace(line), key+": ") {
			start, keyIndent = i+1, indentOf(line)
			break
		}
	}
	require.NotEqual(t, -1, start, ".goreleaser.yaml has no %q block", key)
	for i := start; i < len(lines); i++ {
		l := lines[i]
		if strings.TrimSpace(l) == "" {
			continue
		}
		if indentOf(l) <= keyIndent {
			return strings.Join(lines[start:i], "\n")
		}
	}
	return strings.Join(lines[start:], "\n")
}

// renderGoreleaserTemplate renders one goreleaser name template with the
// fields below. A template that reaches for anything else is an error, not a
// silent empty string: install.sh could not have built the same name.
func renderGoreleaserTemplate(t *testing.T, tmpl string, fields map[string]string) string {
	t.Helper()
	parsed, err := template.New("name").Option("missingkey=error").Parse(tmpl)
	require.NoError(t, err, "parsing name_template %q", tmpl)
	var sb strings.Builder
	require.NoError(t, parsed.Execute(&sb, fields),
		"name_template %q uses a field install.sh does not know about; keep the two in sync", tmpl)
	return sb.String()
}

// expandShell substitutes ${NAME} in a shell string, failing on any name the
// test does not model rather than expanding it to "".
func expandShell(t *testing.T, s string, vars map[string]string) string {
	t.Helper()
	return shellVarRe.ReplaceAllStringFunc(s, func(m string) string {
		name := shellVarRe.FindStringSubmatch(m)[1]
		v, ok := vars[name]
		require.True(t, ok, "install.sh builds the archive name from ${%s}, which .goreleaser.yaml does not set", name)
		return v
	})
}

// TestInstallScriptArchiveNameMatchesGoreleaser guards P10-03: install.sh
// downloads `<project>_<version>_<os>_<arch>.tar.gz` by hand, so changing
// archives.name_template, the archive format or project_name in
// .goreleaser.yaml without changing install.sh would ship a release whose
// installer 404s on every platform.
func TestInstallScriptArchiveNameMatchesGoreleaser(t *testing.T) {
	gr := repoFile(t, ".goreleaser.yaml")
	sh := repoFile(t, "install.sh")

	// The archive template is the templated name_template; the checksum file
	// is the literal one.
	var archiveTmpl, checksumName string
	for _, m := range nameTemplateRe.FindAllStringSubmatch(gr, -1) {
		if strings.Contains(m[1], "{{") {
			require.Empty(t, archiveTmpl, ".goreleaser.yaml has more than one templated name_template; this test assumes one archive")
			archiveTmpl = m[1]
		} else {
			require.Empty(t, checksumName, ".goreleaser.yaml has more than one literal name_template")
			checksumName = m[1]
		}
	}
	require.NotEmpty(t, archiveTmpl, ".goreleaser.yaml no longer has a templated archives.name_template")
	require.NotEmpty(t, checksumName, ".goreleaser.yaml no longer has a literal checksum.name_template")

	project := projectNameRe.FindStringSubmatch(gr)
	require.NotNil(t, project, ".goreleaser.yaml no longer sets project_name")

	formats := formatsRe.FindAllStringSubmatch(gr, -1)
	require.Len(t, formats, 1, ".goreleaser.yaml should build exactly one archive format")

	const version, goos, goarch = "1.2.3", "linux", "amd64"
	want := renderGoreleaserTemplate(t, archiveTmpl, map[string]string{
		"ProjectName": project[1],
		"Version":     version,
		"Os":          goos,
		"Arch":        goarch,
	}) + "." + formats[0][1]

	shProject := shellAssignRe.FindStringSubmatch(sh)
	require.NotNil(t, shProject, "install.sh no longer sets PROJECT=")
	archiveLine := archiveVarRe.FindStringSubmatch(sh)
	require.NotNil(t, archiveLine, `install.sh no longer builds the archive name with ARCHIVE="…"`)
	got := expandShell(t, archiveLine[1], map[string]string{
		"PROJECT": shProject[1],
		"NUM":     version,
		"OS":      goos,
		"ARCH":    goarch,
	})

	require.Equal(t, want, got,
		"install.sh downloads %q but .goreleaser.yaml publishes %q; the two files are changed together", got, want)

	// goreleaser strips the leading "v" from the tag for artefact names, so
	// install.sh must strip it too before it fills in ${NUM}.
	require.True(t, strings.Contains(sh, `NUM="${TAG#v}"`),
		"install.sh must strip the leading v from the tag: goreleaser's .Version does")

	// The checksum file install.sh verifies against is the one goreleaser
	// writes.
	require.True(t, strings.Contains(sh, checksumName),
		"install.sh does not fetch %q, the checksum file .goreleaser.yaml writes", checksumName)
}

// TestInstallScriptCoversReleaseMatrix guards P10-03: install.sh maps uname
// output onto the goos/goarch pairs .goreleaser.yaml builds, so adding a
// platform to the release matrix without teaching install.sh about it would
// ship archives no installer can ask for.
func TestInstallScriptCoversReleaseMatrix(t *testing.T) {
	gr := repoFile(t, ".goreleaser.yaml")
	sh := repoFile(t, "install.sh")

	builds := goreleaserYAMLBlock(t, gr, "builds")
	for _, key := range []string{"goos", "goarch"} {
		block := goreleaserYAMLBlock(t, builds, key)
		items := listItemRe.FindAllStringSubmatch(block, -1)
		require.NotEmpty(t, items, ".goreleaser.yaml has no %s values", key)
		for _, it := range items {
			require.True(t, strings.Contains(sh, fmt.Sprintf("echo %s ;;", it[1])),
				"install.sh does not resolve %s %q, which .goreleaser.yaml builds", key, it[1])
		}
	}
}

// TestReleaseArtefactsAdvertiseNoGoInstall guards P10-03: the module path is
// `slop-cannon`, not a URL, so `go install …/cmd/sc` cannot resolve (README.md,
// CHANGELOG.md and docs/releasing.md all say so). The release notes and the
// installer must not offer it as a fallback.
func TestReleaseArtefactsAdvertiseNoGoInstall(t *testing.T) {
	for _, name := range []string{".goreleaser.yaml", "install.sh"} {
		body := repoFile(t, name)
		for _, line := range strings.Split(body, "\n") {
			if strings.Contains(line, "go install") && strings.Contains(line, "cmd/sc") {
				require.Failf(t, "unusable install route advertised",
					"%s offers `go install …/cmd/sc`, which cannot work with the module path %q: %s",
					name, "slop-cannon", strings.TrimSpace(line))
			}
		}
	}
}

// TestReleaseWorkflowUsesItsInputs guards P10-03: a workflow_dispatch input
// that nothing reads is a switch in the GitHub UI that silently does nothing,
// which is how `dry_run` shipped declared-but-unwired.
func TestReleaseWorkflowUsesItsInputs(t *testing.T) {
	wf := repoFile(t, ".github/workflows/release.yml")
	inputs := goreleaserYAMLBlock(t, goreleaserYAMLBlock(t, wf, "workflow_dispatch"), "inputs")
	names := 0
	minIndent := -1
	for _, line := range strings.Split(inputs, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if minIndent == -1 || indentOf(line) < minIndent {
			minIndent = indentOf(line)
		}
	}
	for _, line := range strings.Split(inputs, "\n") {
		if strings.TrimSpace(line) == "" || indentOf(line) != minIndent {
			continue
		}
		name := strings.TrimSuffix(strings.TrimSpace(line), ":")
		require.NotContains(t, name, ":", "unexpected inputs entry %q", line)
		names++
		require.True(t, strings.Contains(wf, "inputs."+name),
			"release.yml declares the workflow_dispatch input %q but never reads inputs.%s, so the box does nothing", name, name)
	}
	require.Positive(t, names, "release.yml declares no workflow_dispatch inputs; this test is stale")
}

// bootstrapURLRe matches the raw.githubusercontent.com URL the install
// one-liner is pasted from: owner, repo, ref, path.
var bootstrapURLRe = regexp.MustCompile(`https://raw\.githubusercontent\.com/([^/\s]+)/([^/\s]+)/([^/\s]+)/([^\s"'` + "`" + `)]+)`)

// bootstrapRefIsSafe reports whether a raw.githubusercontent ref names
// something that exists whatever the default branch is called: HEAD, a version
// tag (v0.1.0, or the vX.Y.Z placeholder the docs use), or a release template.
func bootstrapRefIsSafe(ref string) bool {
	switch {
	case ref == "HEAD":
		return true
	case strings.Contains(ref, "{{"): // goreleaser renders {{ .Tag }}
		return true
	case regexp.MustCompile(`^v[0-9X]`).MatchString(ref):
		return true
	}
	return false
}

// TestBootstrapInstallURLNamesNoBranch guards P10-03: the README shipped the
// bootstrap one-liner as .../slop-cannon/main/install.sh while install.sh
// itself advertised .../HEAD/install.sh. This repository's default branch is
// master, so the README's URL 404s before it downloads anything — the Phase 10
// exit criterion "curl | sh works on a fresh machine" fails at step one. A raw
// URL must therefore name HEAD or a tag, never a branch, and the README must
// quote the same URL install.sh advertises for itself.
func TestBootstrapInstallURLNamesNoBranch(t *testing.T) {
	files := []string{"README.md", "install.sh", "CHANGELOG.md", ".goreleaser.yaml"}
	// Every page under docs/ as well, so a new one cannot smuggle a branch in.
	pages, err := filepath.Glob(filepath.Join("..", "..", "docs", "*.md"))
	require.NoError(t, err)
	for _, page := range pages {
		files = append(files, filepath.Join("docs", filepath.Base(page)))
	}

	seen := 0
	installers := map[string][]string{}
	for _, name := range files {
		body := repoFile(t, name)
		for _, m := range bootstrapURLRe.FindAllStringSubmatch(body, -1) {
			url, ref, path := m[0], m[3], m[4]
			seen++
			require.Truef(t, bootstrapRefIsSafe(ref),
				"%s fetches %s: the ref %q is a branch name, and this repository's default branch may not be called that "+
					"(it is master today, so /main/ 404s). Use HEAD, like install.sh, or a tag.", name, url, ref)
			if strings.HasSuffix(path, "install.sh") {
				installers[name] = append(installers[name], url)
			}
		}
	}
	require.Positive(t, seen, "no raw.githubusercontent.com URL found anywhere; this test is stale")

	require.NotEmpty(t, installers["install.sh"], "install.sh no longer advertises its own bootstrap URL")
	want := installers["install.sh"][0]
	for _, got := range installers["install.sh"] {
		require.Equal(t, want, got, "install.sh advertises two different bootstrap URLs")
	}
	require.NotEmpty(t, installers["README.md"], "README.md no longer quotes the install.sh one-liner")
	for _, got := range installers["README.md"] {
		require.Equal(t, want, got,
			"README.md tells the reader to curl %q but install.sh advertises %q; the command a user copies is the README's", got, want)
	}
}

var (
	changelogListItemRe = regexp.MustCompile(`(?m)^\s*-\s+(.+?)\s*$`)
	changelogRegexpRe   = regexp.MustCompile(`(?m)^\s*regexp:\s*(.+?)\s*$`)
)

// unquoteYAMLScalar strips one layer of matching quotes from a YAML scalar.
func unquoteYAMLScalar(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// gitSubjects returns every commit subject in the repository, newest first.
func gitSubjects(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "..")
	// A shallow clone is not this repository's history, it is the tip of it:
	// the CI integration job checks out at the default depth of 1, and a
	// changelog filter judged against a single commit says nothing. Skip
	// rather than fail there; the `make check` job fetches the full history.
	shallow := exec.Command("git", "rev-parse", "--is-shallow-repository")
	shallow.Dir = root
	if out, err := shallow.Output(); err == nil && strings.TrimSpace(string(out)) == "true" {
		t.Skip("shallow clone: the full commit history this test judges the changelog config against is not here")
	}
	cmd := exec.Command("git", "log", "--format=%s")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("git log is unavailable here (%v); this test reads the repository's own history", err)
	}
	var subjects []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			subjects = append(subjects, line)
		}
	}
	require.NotEmpty(t, subjects, "git log returned no commit subjects")
	return subjects
}

// TestChangelogConfigMatchesRealHistory guards P10-03: .goreleaser.yaml shipped
// conventional-commit groups (feat:/fix:) and filters (^docs:, ^chore:,
// "^claim ") that matched none of this repository's 48 commits, so every commit
// — bookkeeping included — landed in the release body under one "Others"
// heading. A changelog pattern that matches nothing is decoration; it must earn
// its place against the real history.
func TestChangelogConfigMatchesRealHistory(t *testing.T) {
	subjects := gitSubjects(t)
	changelog := goreleaserYAMLBlock(t, repoFile(t, ".goreleaser.yaml"), "changelog")

	matchesSomething := func(pattern string) (int, error) {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return 0, err
		}
		n := 0
		for _, s := range subjects {
			if re.MatchString(s) {
				n++
			}
		}
		return n, nil
	}

	// Patterns that guard against a history shape this repository cannot
	// produce yet: it has no merge commits because nothing merges into it.
	// Everything else must match a commit that exists.
	forwardLooking := map[string]bool{"^Merge ": true}

	checked := 0
	if strings.Contains(changelog, "exclude:") {
		for _, m := range changelogListItemRe.FindAllStringSubmatch(goreleaserYAMLBlock(t, changelog, "exclude"), -1) {
			pattern := unquoteYAMLScalar(m[1])
			n, err := matchesSomething(pattern)
			require.NoErrorf(t, err, "changelog filter %q is not a valid regexp, so goreleaser cannot use it", pattern)
			checked++
			if forwardLooking[pattern] {
				continue
			}
			require.Positivef(t, n,
				"changelog.filters.exclude has %q, which matches none of the %d commit subjects in this repository: "+
					"it excludes nothing. Delete it or replace it with a pattern the real history has.", pattern, len(subjects))
		}
	}
	for _, m := range changelogRegexpRe.FindAllStringSubmatch(changelog, -1) {
		pattern := unquoteYAMLScalar(m[1])
		n, err := matchesSomething(pattern)
		require.NoErrorf(t, err, "changelog group regexp %q is not a valid regexp", pattern)
		checked++
		require.Positivef(t, n,
			"changelog.groups has the regexp %q, which matches none of the %d commit subjects in this repository, "+
				"so the group is always empty and everything falls through to the catch-all.", pattern, len(subjects))
	}
	require.Positive(t, checked, "the changelog block declares no filters or groups; this test is stale")

	// The bookkeeping commits the filters exist to drop really are dropped.
	kept := 0
	excluded := regexp.MustCompile(`: record |IMPLEMENTATION-PROGRESS`)
	for _, s := range subjects {
		if !excluded.MatchString(s) {
			kept++
		}
	}
	require.Less(t, kept, len(subjects),
		"no commit subject is bookkeeping any more; the changelog filters may be stale")
}
