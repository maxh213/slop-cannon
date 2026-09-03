package prompt

import (
	"fmt"
	"strconv"
)

// Budget defaults.
const (
	// DefaultMaxTokens is the prompt budget when a request sets none. It is
	// deliberately far below any cartridge's context window: the prompt is
	// the agent's briefing, and the rest of the window belongs to the work.
	DefaultMaxTokens = 24000
	// DefaultCharsPerToken is the rule of thumb the estimate uses. Nothing
	// here needs a real tokenizer: the budget decides how much context to
	// drop, and a four-characters-per-token estimate is conservative for
	// English prose, source paths and JSON alike.
	DefaultCharsPerToken = 4
)

// Budget bounds the assembled prompt. The zero value means the defaults.
type Budget struct {
	// MaxTokens is the estimated token ceiling; 0 means DefaultMaxTokens.
	MaxTokens int
	// CharsPerToken is the estimator's ratio; 0 means DefaultCharsPerToken.
	CharsPerToken int
}

// withDefaults resolves the zero values.
func (b Budget) withDefaults() Budget {
	if b.MaxTokens <= 0 {
		b.MaxTokens = DefaultMaxTokens
	}
	if b.CharsPerToken <= 0 {
		b.CharsPerToken = DefaultCharsPerToken
	}
	return b
}

// Estimate returns the estimated token count of s, rounded up.
func (b Budget) Estimate(s string) int {
	cpt := b.CharsPerToken
	if cpt <= 0 {
		cpt = DefaultCharsPerToken
	}
	return (len(s) + cpt - 1) / cpt
}

// EstimateTokens estimates the size of s with the default ratio.
func EstimateTokens(s string) int { return Budget{}.Estimate(s) }

// knobs are the sizes of the four droppable layers. A negative value means
// "everything there is"; zero means the layer is left out entirely.
type knobs struct {
	// tree is the number of repository tree entries shown.
	tree int
	// rounds is the number of most recent review rounds shown.
	rounds int
	// readme is the number of README characters shown.
	readme int
	// openBeads is the number of existing beads listed.
	openBeads int
}

// fullKnobs is the untruncated prompt.
func fullKnobs(_ Request) knobs { return knobs{tree: -1, rounds: -1, readme: -1, openBeads: -1} }

// treeSteps, roundSteps, readmeSteps and beadSteps are the degradation
// ladders, applied in that order: the repository tree goes first because it
// is the least specific context, then the older review rounds, then the
// README, and only then the list of existing beads.
var (
	treeSteps   = []int{200, 100, 50, 20, 0}
	roundSteps  = []int{3, 2, 1}
	readmeSteps = []int{1024, 0}
	beadSteps   = []int{20, 0}
)

// degradations returns the prompts to try, from the full one to the most
// degraded, each step dropping a little more. Steps that would change
// nothing for this request are skipped, so the sequence never repeats a
// prompt.
func degradations(req Request, full knobs) []knobs {
	out := []knobs{full}
	cur := full
	for _, n := range treeSteps {
		if n < len(req.Repo.Tree) {
			cur.tree = n
			out = append(out, cur)
		}
	}
	for _, n := range roundSteps {
		if n < len(req.History) {
			cur.rounds = n
			out = append(out, cur)
		}
	}
	for _, n := range readmeSteps {
		if n < len(req.Repo.ReadmeHead) {
			cur.readme = n
			out = append(out, cur)
		}
	}
	for _, n := range beadSteps {
		if n < len(req.OpenBeads) {
			cur.openBeads = n
			out = append(out, cur)
		}
	}
	return out
}

// describeTruncations reports what the chosen knobs left out, in the order
// the ladders are applied.
func describeTruncations(req Request, full, final knobs) []string {
	var out []string
	if final.tree != full.tree {
		if final.tree == 0 {
			out = append(out, "repository tree omitted ("+strconv.Itoa(len(req.Repo.Tree))+" entries)")
		} else {
			out = append(out, fmt.Sprintf("repository tree cut to %d of %d entries", final.tree, len(req.Repo.Tree)))
		}
	}
	if final.rounds != full.rounds {
		out = append(out, fmt.Sprintf("dropped the %d oldest review round(s), kept %d",
			len(req.History)-final.rounds, final.rounds))
	}
	if final.readme != full.readme {
		if final.readme == 0 {
			out = append(out, "README head omitted")
		} else {
			out = append(out, fmt.Sprintf("README head cut to %d of %d characters",
				final.readme, len(req.Repo.ReadmeHead)))
		}
	}
	if final.openBeads != full.openBeads {
		if final.openBeads == 0 {
			out = append(out, "open-bead list omitted ("+strconv.Itoa(len(req.OpenBeads))+" beads)")
		} else {
			out = append(out, fmt.Sprintf("open-bead list cut to %d of %d beads",
				final.openBeads, len(req.OpenBeads)))
		}
	}
	return out
}
