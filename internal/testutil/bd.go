package testutil

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// bdSampleExts are the file suffixes that make up one bd sample, in lookup
// order for standard output.
var bdSampleExts = []string{".json", ".jsonl", ".txt"}

// bdSampleAllExts adds the stderr and exit-status companions.
var bdSampleAllExts = []string{".json", ".jsonl", ".txt", ".stderr", ".exit"}

// bdGroupCommands are bd commands whose first positional argument is a
// subcommand rather than an issue id, so the sample key includes it
// ("dep add" replays dep-add.json).
var bdGroupCommands = map[string]bool{
	"gate": true, "dep": true, "deps": true, "config": true, "comments": true,
	"label": true, "merge-slot": true, "swarm": true, "epic": true,
	"metrics": true, "worktree": true, "dolt": true,
}

// bdValueFlags are bd flags that consume the next argument, so that value is
// never mistaken for a positional when deriving the sample key.
var bdValueFlags = map[string]bool{
	"-C": true, "--directory": true, "--actor": true, "--db": true,
	"-n": true, "--limit": true, "-p": true, "--priority": true,
	"-t": true, "--type": true, "-s": true, "--status": true,
	"-l": true, "--labels": true, "-a": true, "--assignee": true,
	"-d": true, "--description": true, "--acceptance": true,
	"--reason": true, "--parent": true, "--metadata": true,
	"--set-metadata": true, "--add-label": true, "--remove-label": true,
	"--set-labels": true, "--metadata-field": true, "--title": true,
	"--await-id": true, "--blocks": true, "--graph": true, "-f": true,
	"--file": true, "--body-file": true, "--deps": true, "--id": true,
}

var bdWord = regexp.MustCompile(`^[a-z]+$`)

// BDKey returns the sample key the FakeBD shim derives from a bd argument
// list: the subcommand, plus the second word for group commands such as
// "dep add" or "gate check", with "create --graph" mapped to "create-graph"
// and "create-graph-dry-run". Flags and their values are ignored, so
// "bd -C repo ready -n 0 --json" and "bd ready --json" share the key "ready".
func BDKey(args []string) string {
	var key, sub string
	graph, dry := false, false
	skip := false
	for _, a := range args {
		if skip {
			skip = false
			continue
		}
		switch {
		case a == "--graph":
			graph, skip = true, true
		case strings.HasPrefix(a, "--graph="):
			graph = true
		case a == "--dry-run":
			dry = true
		case bdValueFlags[a]:
			skip = true
		case strings.HasPrefix(a, "-"):
		case key == "":
			key = a
		case sub == "":
			sub = a
		}
	}
	switch {
	case key == "create" && graph && dry:
		return "create-graph-dry-run"
	case key == "create" && graph:
		return "create-graph"
	case bdGroupCommands[key] && bdWord.MatchString(sub):
		return key + "-" + sub
	}
	return key
}

// BDSamplesDir returns the directory of envelope-shaped bd 1.2.2 outputs
// captured for the shim (internal/testutil/testdata/bd). See its README.md
// for how the samples were produced and what each key contains.
func BDSamplesDir(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("testutil: cannot locate the package source directory")
	}
	dir := filepath.Join(filepath.Dir(file), "testdata", "bd")
	if _, err := os.Stat(filepath.Join(dir, "ready.json")); err != nil {
		t.Fatalf("testutil: bd samples missing at %s: %v", dir, err)
	}
	return dir
}

// StageBDSamples copies the default samples into a temporary directory and
// then, for every key in overrides, installs the named sample under that key
// so the shim replays it. For example {"ready": "ready-after-close"} makes
// "bd ready" return the captured empty ready set, and {"close": "error-close-blocked"}
// makes "bd close" fail the way bd does when blockers are open. A key with
// a sequence suffix ("ready.2") applies to that call only. The directory is
// removed when the test ends.
func StageBDSamples(t testing.TB, overrides map[string]string) string {
	t.Helper()
	src := BDSamplesDir(t)
	dir := t.TempDir()
	if err := copyTree(src, dir); err != nil {
		t.Fatalf("testutil: copying bd samples: %v", err)
	}
	for key, sample := range overrides {
		found := false
		for _, ext := range bdSampleAllExts {
			data, err := os.ReadFile(filepath.Join(src, sample+ext))
			if err != nil {
				continue
			}
			found = true
			if err := os.WriteFile(filepath.Join(dir, key+ext), data, 0o644); err != nil {
				t.Fatalf("testutil: staging bd sample %s: %v", key+ext, err)
			}
		}
		if !found {
			t.Fatalf("testutil: no bd sample named %q in %s", sample, src)
		}
		// Drop stale companions from the default set so the override is the
		// only thing that decides stdout, stderr, and exit code.
		for _, ext := range bdSampleAllExts {
			if _, err := os.Stat(filepath.Join(src, sample+ext)); err != nil {
				_ = os.Remove(filepath.Join(dir, key+ext))
			}
		}
	}
	return dir
}

// FakeBD installs a "bd" shim (see FakeBin) that replays captured output
// from samplesDir instead of running beads. For each call it derives a key
// with BDKey and looks for, in order, "<key>.<n>" (n = the 1-based count of
// calls with that key so far) and then "<key>", each with the extensions
// .json/.jsonl/.txt for stdout, .stderr for stderr, and .exit for the exit
// status (default 0). On the very first call a "preamble.txt", if present,
// is written to stderr the way bd prints its one-time metrics notice.
//
// A call with no sample prints a "fakebd: no sample" line to stderr, exits
// with status 3, and is listed by MissingSamples so tests fail loudly
// instead of silently receiving an empty envelope. Standard input is
// captured (WithStdinCapture) so "bd comment --stdin" bodies are recorded.
func FakeBD(t testing.TB, samplesDir string) *Shim {
	t.Helper()
	abs, err := filepath.Abs(samplesDir)
	if err != nil {
		t.Fatalf("testutil: resolving bd samples dir: %v", err)
	}
	var b strings.Builder
	b.WriteString("samples=" + shellQuote(abs) + "\n")
	b.WriteString("key=''; sub=''; skip=0; graph=0; dry=0\n")
	b.WriteString("for a in \"$@\"; do\n")
	b.WriteString("  if [ \"$skip\" = 1 ]; then skip=0; continue; fi\n")
	b.WriteString("  case \"$a\" in\n")
	b.WriteString("    --graph) graph=1; skip=1 ;;\n")
	b.WriteString("    --graph=*) graph=1 ;;\n")
	b.WriteString("    --dry-run) dry=1 ;;\n")
	b.WriteString("    " + shellCasePattern(bdValueFlags) + ") skip=1 ;;\n")
	b.WriteString("    -*) ;;\n")
	b.WriteString("    *) if [ -z \"$key\" ]; then key=\"$a\"; elif [ -z \"$sub\" ]; then sub=\"$a\"; fi ;;\n")
	b.WriteString("  esac\n")
	b.WriteString("done\n")
	b.WriteString("if [ \"$key\" = create ] && [ \"$graph\" = 1 ]; then\n")
	b.WriteString("  if [ \"$dry\" = 1 ]; then key=create-graph-dry-run; else key=create-graph; fi\n")
	b.WriteString("else\n")
	b.WriteString("  case \"$key\" in\n")
	b.WriteString("    " + shellCasePattern(bdGroupCommands) + ")\n")
	b.WriteString("      case \"$sub\" in ''|*[!a-z]*) ;; *) key=\"$key-$sub\" ;; esac ;;\n")
	b.WriteString("  esac\n")
	b.WriteString("fi\n")
	b.WriteString(`if [ "$SC_FAKEBIN_SEQ" = 1 ] && [ -f "$samples/preamble.txt" ]; then cat "$samples/preamble.txt" >&2; fi
while ! mkdir "$SC_FAKEBIN_DIR/.keylock" 2>/dev/null; do sleep 0.01; done
n=$(cat "$SC_FAKEBIN_DIR/count.$key" 2>/dev/null || echo 0)
n=$((n + 1))
echo "$n" > "$SC_FAKEBIN_DIR/count.$key"
rmdir "$SC_FAKEBIN_DIR/.keylock"
stem=''
for candidate in "$samples/$key.$n" "$samples/$key"; do
  for ext in .json .jsonl .txt .stderr .exit; do
    if [ -f "$candidate$ext" ]; then stem="$candidate"; break; fi
  done
  [ -n "$stem" ] && break
done
if [ -z "$stem" ]; then
  echo "fakebd: no sample for key \"$key\" in $samples (bd $*)" >&2
  echo "$key" >> "$SC_FAKEBIN_DIR/missing.log"
  exit 3
fi
for ext in .json .jsonl .txt; do
  if [ -f "$stem$ext" ]; then cat "$stem$ext"; break; fi
done
if [ -f "$stem.stderr" ]; then cat "$stem.stderr" >&2; fi
if [ -f "$stem.exit" ]; then exit "$(tr -cd '0-9' < "$stem.exit")"; fi
exit 0
`)
	return FakeBin(t, "bd", b.String(), WithStdinCapture())
}

// MissingSamples returns the keys of every call the FakeBD shim could not
// answer, in order. Tests using FakeBD should assert it is empty.
func MissingSamples(t testing.TB, s *Shim) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.Dir, "missing.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("testutil: reading missing-sample log: %v", err)
	}
	return strings.Fields(string(data))
}

// shellCasePattern joins the keys of set into a "a|b|c" case pattern in a
// stable order.
func shellCasePattern(set map[string]bool) string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, "|")
}
