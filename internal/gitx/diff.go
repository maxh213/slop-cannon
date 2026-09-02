package gitx

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"slop-cannon/internal/domain"
)

// DefaultMaxDiffBytes is the largest unified diff handed to reviewers in
// full (200 KiB). Bigger diffs come back as a file list with per-file
// statistics and the testers open the files themselves.
const DefaultMaxDiffBytes = 200 << 10

// DefaultReReviewThreshold is the D-09 default: an earlier approval stands
// unless the engineer's diff moved by more than 40% of its changed lines
// between rounds.
const DefaultReReviewThreshold = 0.4

// DiffStatus is git's one-letter classification of a changed file.
type DiffStatus string

// File statuses as printed by "git diff --name-status".
const (
	// DiffAdded is a new file.
	DiffAdded DiffStatus = "A"
	// DiffModified is a file changed in place.
	DiffModified DiffStatus = "M"
	// DiffDeleted is a removed file.
	DiffDeleted DiffStatus = "D"
	// DiffRenamed is a file that moved (rename detection is always on).
	DiffRenamed DiffStatus = "R"
	// DiffCopied is a file created as a copy of another.
	DiffCopied DiffStatus = "C"
	// DiffTypeChanged is a file whose type changed (file, symlink, submodule).
	DiffTypeChanged DiffStatus = "T"
	// DiffUnmerged is a file with an unresolved conflict.
	DiffUnmerged DiffStatus = "U"
)

// DiffFile is one changed file: its line statistics (see [FileStat]) plus
// the kind of change.
type DiffFile struct {
	FileStat
	// Status classifies the change.
	Status DiffStatus
	// Similarity is the rename or copy similarity in percent, 0 otherwise.
	Similarity int
}

// Diff is a worktree's committed change set against the merge-base with
// the branch it started from, as reviewers see it.
type Diff struct {
	// Stats carries the merge-base, the head, and the line counts of the
	// change set (the same numbers [Repo.DiffStats] returns).
	Stats DiffStats
	// Files lists every changed file with its status, in git's order (the
	// same order as Stats.Changed).
	Files []DiffFile
	// Bytes is the size of the unified diff, whether or not Patch holds it.
	Bytes int
	// MaxBytes is the cap that was applied to Patch.
	MaxBytes int
	// Patch is the unified diff with rename detection, or "" when
	// Truncated.
	Patch string
	// Truncated is set when the unified diff exceeded MaxBytes; Files and
	// Stats are still complete, so reviewers get [Diff.Stat] and open the
	// files themselves.
	Truncated bool
}

// Base returns the merge-base commit the change set is measured from.
func (d *Diff) Base() string { return d.Stats.Base }

// Head returns the commit at the tip of the reviewed branch.
func (d *Diff) Head() string { return d.Stats.Head }

// Empty reports whether nothing changed between Base and Head.
func (d *Diff) Empty() bool { return d.Stats.Empty() }

// ChangedLines returns insertions plus deletions, the D-09 denominator.
func (d *Diff) ChangedLines() int { return d.Stats.Lines() }

// Stat renders one line per file in the spirit of "git diff --stat":
// "<status> <path>  +<adds> -<dels>" (with "<old> -> <new>" for renames
// and copies, and "binary" for binary files), followed by the
// [DiffStats.String] totals line. It is what reviewers get instead of the
// patch when the diff is truncated.
func (d *Diff) Stat() string {
	var b strings.Builder
	for _, f := range d.Files {
		fmt.Fprintf(&b, "%s ", f.Status)
		if f.OldPath != "" {
			fmt.Fprintf(&b, "%s -> %s", f.OldPath, f.Path)
		} else {
			b.WriteString(f.Path)
		}
		if f.Binary {
			b.WriteString("  binary\n")
		} else {
			fmt.Fprintf(&b, "  +%d -%d\n", f.Insertions, f.Deletions)
		}
	}
	b.WriteString(d.Stats.String())
	return b.String()
}

// DiffOption customises [Repo.Diff] and [Manager.Diff].
type DiffOption func(*diffOptions)

type diffOptions struct {
	maxBytes int
}

// WithMaxDiffBytes caps the unified diff at n bytes (see
// [DefaultMaxDiffBytes]). Zero or negative keeps the default.
func WithMaxDiffBytes(n int) DiffOption {
	return func(o *diffOptions) {
		if n > 0 {
			o.maxBytes = n
		}
	}
}

// Diff returns the committed changes of HEAD since its merge-base with
// base (a branch name or any revision), with rename detection. Uncommitted
// changes are not included: reviewers see what [Manager.CommitLeftovers]
// committed (D-14). When the unified diff is larger than the cap the
// result carries only the file list and statistics with Truncated set. It
// fails with [ErrUnknownRevision] when base does not resolve,
// [ErrNoMergeBase] when the histories are unrelated, and [ErrNoCommits] on
// an unborn branch.
func (r *Repo) Diff(ctx context.Context, base string, opts ...DiffOption) (*Diff, error) {
	o := diffOptions{maxBytes: DefaultMaxDiffBytes}
	for _, opt := range opts {
		opt(&o)
	}
	stats, err := r.DiffStats(ctx, base)
	if err != nil {
		return nil, err
	}
	d := &Diff{Stats: *stats, MaxBytes: o.maxBytes}
	if stats.Empty() {
		return d, nil
	}

	statuses, err := r.diffStatuses(ctx, stats.Base, stats.Head)
	if err != nil {
		return nil, err
	}
	d.Files = make([]DiffFile, 0, len(stats.Changed))
	for _, f := range stats.Changed {
		st, ok := statuses[f.Path]
		if !ok {
			return nil, fmt.Errorf("gitx: diff: no status for %q", f.Path)
		}
		d.Files = append(d.Files, DiffFile{FileStat: f, Status: st.Status, Similarity: st.Similarity})
	}

	patch, err := r.Git(ctx, append(diffArgs("--patch"), stats.Base, stats.Head, "--")...)
	if err != nil {
		return nil, err
	}
	if patch != "" {
		patch += "\n"
	}
	d.Bytes = len(patch)
	if d.Bytes > o.maxBytes {
		d.Truncated = true
	} else {
		d.Patch = patch
	}
	return d, nil
}

// Diff returns the bead's committed changes against the merge-base with
// the branch its worktree started from (see [Repo.Diff]). It fails with
// [ErrWorktreeMissing] when the bead has no usable worktree.
func (m *Manager) Diff(ctx context.Context, id domain.BeadID, opts ...DiffOption) (*Diff, error) {
	repo, err := m.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	base, err := m.baseRevision(ctx)
	if err != nil {
		return nil, err
	}
	return repo.Diff(ctx, base, opts...)
}

// ChangeRatio measures how far an engineer's diff moved between two review
// rounds, for the D-09 rule on which testers must re-review.
type ChangeRatio struct {
	// Previous and Current are the branch tips of the two rounds.
	Previous string
	Current  string
	// Delta is the number of lines inserted or deleted between Previous
	// and Current, i.e. the size of what the engineer did in the round.
	Delta int
	// Total is the number of changed lines in the current round's diff
	// against the merge-base (the whole change set under review).
	Total int
	// Ratio is Delta / Total: 0 when nothing moved, 1 when the change set
	// was emptied, and possibly above 1 when the round rewrote more than
	// the surviving change set.
	Ratio float64
}

// Exceeds reports whether the ratio is above threshold, so every tester
// must re-review (D-09). A threshold of zero or less means
// [DefaultReReviewThreshold].
func (c ChangeRatio) Exceeds(threshold float64) bool {
	if threshold <= 0 {
		threshold = DefaultReReviewThreshold
	}
	return c.Ratio > threshold
}

// ChangeRatio computes the D-09 change ratio between the previous round's
// diff and the current one: the lines changed between their heads,
// relative to the current diff's changed lines. Both heads must still be
// reachable in the repository. When the branch was rebased between rounds
// the upstream commits count as movement too, which errs on the side of
// re-reviewing.
func (r *Repo) ChangeRatio(ctx context.Context, previous, current *Diff) (ChangeRatio, error) {
	if previous == nil || current == nil {
		return ChangeRatio{}, errors.New("gitx: ChangeRatio: both diffs are required")
	}
	c := ChangeRatio{Previous: previous.Head(), Current: current.Head(), Total: current.ChangedLines()}
	if c.Previous != c.Current {
		delta, err := r.diffStatsBetween(ctx, c.Previous, c.Current)
		if err != nil {
			return ChangeRatio{}, err
		}
		c.Delta = delta.Lines()
	}
	c.Ratio = RatioOf(c.Delta, c.Total)
	return c, nil
}

// RatioOf is the D-09 arithmetic on its own: delta lines over total lines.
// It is 0 when nothing moved and 1 when total is zero but something moved
// (the change set vanished), so a threshold comparison never divides by
// zero.
func RatioOf(delta, total int) float64 {
	switch {
	case delta <= 0:
		return 0
	case total <= 0:
		return 1
	default:
		return float64(delta) / float64(total)
	}
}

// diffArgs returns the common "git diff" flags with mode ("--patch" or
// "--name-status") added: rename detection on, and no colour, external
// diff drivers, or textconv filters so the output is stable and
// machine-readable.
func diffArgs(mode string) []string {
	return []string{"diff", mode, "--find-renames", "--no-color", "--no-ext-diff", "--no-textconv"}
}

// fileStatus is the classification of one file from "--name-status".
type fileStatus struct {
	Status     DiffStatus
	Similarity int
}

// diffStatuses classifies every file changed between two commits, keyed
// by its (new) path.
func (r *Repo) diffStatuses(ctx context.Context, from, to string) (map[string]fileStatus, error) {
	out, err := r.Git(ctx, append(diffArgs("--name-status"), "-z", from, to, "--")...)
	if err != nil {
		return nil, err
	}
	return parseNameStatus(out)
}

// parseNameStatus parses "git diff --name-status -z": a status token
// ("M", "A", "D", "T", "R<score>", "C<score>", …) followed by one
// NUL-terminated path, or two for renames and copies (old, then new). The
// result is keyed by the new path.
func parseNameStatus(out string) (map[string]fileStatus, error) {
	statuses := map[string]fileStatus{}
	if out == "" {
		return statuses, nil
	}
	fields := strings.Split(out, "\x00")
	if fields[len(fields)-1] == "" {
		fields = fields[:len(fields)-1]
	}
	for i := 0; i < len(fields); {
		token := fields[i]
		if token == "" {
			return nil, fmt.Errorf("gitx: diff: empty status at field %d", i)
		}
		st := fileStatus{Status: DiffStatus(token[:1])}
		if score := token[1:]; score != "" {
			n, err := strconv.Atoi(score)
			if err != nil {
				return nil, fmt.Errorf("gitx: diff: bad status %q: %w", token, err)
			}
			st.Similarity = n
		}
		var path string
		switch st.Status {
		case DiffRenamed, DiffCopied:
			if i+2 >= len(fields) {
				return nil, fmt.Errorf("gitx: diff: truncated rename entry %q", token)
			}
			path = fields[i+2]
			i += 3
		default:
			if i+1 >= len(fields) {
				return nil, fmt.Errorf("gitx: diff: truncated entry %q", token)
			}
			path = fields[i+1]
			i += 2
		}
		statuses[path] = st
	}
	return statuses, nil
}
