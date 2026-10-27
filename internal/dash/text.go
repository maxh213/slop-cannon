package dash

import (
	"strings"
	"unicode/utf8"

	"slop-cannon/internal/textsafe"
)

const (
	maxIDText       = 256
	maxTitleText    = 500
	maxActivityText = 500
	maxURLText      = 2_048
	maxBranchText   = 500
	maxCommitText   = 128
	maxBeadStates   = 32
	maxBeadDeps     = 64
	maxReviewLens   = 32
	maxVerdicts     = 64
)

// boundedText flattens terminal controls and caps run-controlled text by rune
// so both the cached value and its JSON representation remain bounded.
func boundedText(s string, limit int) string {
	s = textsafe.Flatten(s, textsafe.LinePolicy())
	if limit <= 0 || utf8.RuneCountInString(s) <= limit {
		return s
	}
	r := []rune(s)
	if limit == 1 {
		return "…"
	}
	return strings.TrimSpace(string(r[:limit-1])) + "…"
}
