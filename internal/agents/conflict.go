package agents

import (
	"strings"
	"unicode/utf8"
)

// This file is the agents half of P5-12: the normalisation an
// [EngineerConflict] needs before it reaches a prompt, kept here so the
// orchestrator's adapter from gitx's delivery error
// (orchestrator.ConflictFrom) is a two-line mapping and every caller gets
// the same bounds and the same tidying.
//
// [EngineerConflict] deliberately does not import internal/gitx: an
// engineer firing describes a failed rebase, it does not depend on how the
// delivery discovered one.

// MaxConflictOutput bounds the git output a resolve-conflict prompt quotes.
// A rebase that stopped on conflicts prints a handful of lines; anything
// longer is a runaway hook or a huge conflict list, and the tail is the
// part that says what went wrong.
const MaxConflictOutput = 8 << 10

// ConflictTruncationNotice is prepended to the output when
// [EngineerConflict.Normalise] had to drop the start of it.
const ConflictTruncationNotice = "[earlier output truncated]"

// NewEngineerConflict builds the conflict an [EngineerResolveConflict]
// firing has to repair from the three things a delivery knows about it:
// the branch the work was being rebased onto (normally the repository's
// default branch), the conflicted paths, and what git printed. The result
// is normalised (see [EngineerConflict.Normalise]), so a caller can hand it
// straight to [EngineerRequest].
//
// The orchestrator calls it through its own adapter, which is the one place
// that reads a gitx delivery error (P5-12).
func NewEngineerConflict(target string, files []string, output string) EngineerConflict {
	return EngineerConflict{Target: target, Files: files, Output: output}.Normalise()
}

// Normalise returns the conflict with its target and continue command
// trimmed, its file list trimmed, de-duplicated and free of blanks, and its
// output trimmed and cut to the last [MaxConflictOutput] bytes (on a line
// and rune boundary, with [ConflictTruncationNotice] in front of what is
// left). It is idempotent.
func (c EngineerConflict) Normalise() EngineerConflict {
	out := EngineerConflict{
		Target:   strings.TrimSpace(c.Target),
		Files:    normaliseConflictFiles(c.Files),
		Output:   tailLines(strings.TrimSpace(c.Output), MaxConflictOutput),
		Continue: strings.TrimSpace(c.Continue),
	}
	return out
}

// normaliseConflictFiles trims every path, drops the blanks and keeps the
// first occurrence of each, preserving git's order.
func normaliseConflictFiles(files []string) []string {
	if len(files) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(files))
	out := make([]string, 0, len(files))
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// tailLines returns the last limit bytes of s, cut at a line boundary when
// there is one and at a rune boundary otherwise, with
// [ConflictTruncationNotice] in front. A string that already fits is
// returned unchanged.
func tailLines(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	tail := s[len(s)-limit:]
	if i := strings.IndexByte(tail, '\n'); i >= 0 && i+1 < len(tail) {
		tail = tail[i+1:]
	}
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return ConflictTruncationNotice + "\n" + tail
}
