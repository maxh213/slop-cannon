package agents

import (
	"strings"
	"testing"
)

// TestNewEngineerConflictNormalises covers the tidying every caller of the
// P5-12 adapter gets: trimmed fields, de-duplicated files and no blanks.
func TestNewEngineerConflictNormalises(t *testing.T) {
	t.Parallel()
	got := NewEngineerConflict("  main\n", []string{" internal/a.go ", "", "internal/a.go", "internal/b.go"},
		"  CONFLICT (content): Merge conflict in internal/a.go  ")
	if got.Target != "main" {
		t.Errorf("target = %q, want main", got.Target)
	}
	want := []string{"internal/a.go", "internal/b.go"}
	if len(got.Files) != len(want) {
		t.Fatalf("files = %v, want %v", got.Files, want)
	}
	for i := range want {
		if got.Files[i] != want[i] {
			t.Fatalf("files = %v, want %v", got.Files, want)
		}
	}
	if got.Output != "CONFLICT (content): Merge conflict in internal/a.go" {
		t.Errorf("output = %q, want it trimmed", got.Output)
	}
	if got.Empty() {
		t.Error("Empty() = true, want a conflict an engineer can act on")
	}
	if again := got.Normalise(); !sameConflict(again, got) {
		t.Errorf("Normalise is not idempotent: %+v then %+v", got, again)
	}
}

// TestNewEngineerConflictOfNothing: an empty delivery report stays empty, so
// [EngineerRequest.Validate] refuses the firing rather than sending an
// engineer to resolve nothing.
func TestNewEngineerConflictOfNothing(t *testing.T) {
	t.Parallel()
	got := NewEngineerConflict("   ", []string{"", "  "}, "\n\n")
	if !got.Empty() {
		t.Errorf("conflict = %+v, want empty", got)
	}
	if got.Files != nil {
		t.Errorf("files = %v, want nil", got.Files)
	}
}

// TestEngineerConflictOutputIsBounded keeps a runaway rebase log out of the
// prompt: the tail survives, cut at a line boundary and announced.
func TestEngineerConflictOutputIsBounded(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	for i := range 4000 {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", 20))
		if i == 3999 {
			b.WriteString(" LAST")
		}
		b.WriteByte('\n')
	}
	got := NewEngineerConflict("main", []string{"a.go"}, b.String())
	if len(got.Output) > MaxConflictOutput+len(ConflictTruncationNotice)+1 {
		t.Fatalf("output is %d bytes, want at most %d", len(got.Output), MaxConflictOutput)
	}
	if !strings.HasPrefix(got.Output, ConflictTruncationNotice+"\n") {
		t.Errorf("output does not announce the truncation: %q", got.Output[:40])
	}
	if !strings.Contains(got.Output, "LAST") {
		t.Error("the tail of the output was dropped; it is the part that says what went wrong")
	}
	for _, line := range strings.Split(got.Output, "\n") {
		if line != ConflictTruncationNotice && line != "" && !strings.HasPrefix(line, "line ") {
			t.Fatalf("output was cut mid-line: %q", line)
		}
	}
}

// TestEngineerConflictOutputCutsOnARuneBoundary: a truncated multi-byte rune
// would make the prompt invalid UTF-8.
func TestEngineerConflictOutputCutsOnARuneBoundary(t *testing.T) {
	t.Parallel()
	got := NewEngineerConflict("main", nil, strings.Repeat("é", MaxConflictOutput))
	if !strings.HasPrefix(got.Output, ConflictTruncationNotice+"\n") {
		t.Fatalf("output = %q…, want the truncation notice", got.Output[:20])
	}
	rest := strings.TrimPrefix(got.Output, ConflictTruncationNotice+"\n")
	if !strings.HasPrefix(rest, "é") {
		t.Errorf("output starts mid-rune: %q", rest[:4])
	}
}

// TestConflictPromptQuotesTheHelperOutput proves the adapter's result
// reaches the engineer: a resolve-conflict prompt built from it names the
// target, the files and the continue command.
func TestConflictPromptQuotesTheHelperOutput(t *testing.T) {
	t.Parallel()
	c := NewEngineerConflict("main", []string{"internal/a.go"}, "CONFLICT (content)")
	body := renderEngineerConflict(c)
	for _, want := range []string{"internal/a.go", "CONFLICT (content)"} {
		if !strings.Contains(body, want) {
			t.Errorf("the rendered conflict does not mention %q:\n%s", want, body)
		}
	}
	if c.continueCommand() != DefaultConflictContinue {
		t.Errorf("continue = %q, want %q", c.continueCommand(), DefaultConflictContinue)
	}
}

// sameConflict compares two conflicts field by field (Files is a slice, so
// == does not work).
func sameConflict(a, b EngineerConflict) bool {
	if a.Target != b.Target || a.Output != b.Output || a.Continue != b.Continue || len(a.Files) != len(b.Files) {
		return false
	}
	for i := range a.Files {
		if a.Files[i] != b.Files[i] {
			return false
		}
	}
	return true
}
