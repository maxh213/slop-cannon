package dash

import (
	"strings"

	"slop-cannon/internal/tui"
)

const (
	noticeRateLimited = "rate_limited"
	noticeConflict    = "conflict"
	noticeCheck       = "check"
	noticeReview      = "review"
	noticeGeneric     = "generic"
)

// projectNotice deliberately drops Message and Err. They can quote an agent
// failure or review finding, so only a fixed category leaves the process.
func projectNotice(n tui.Notice) NoticeSnapshot {
	return NoticeSnapshot{
		At: n.At, Kind: boundedText(n.Kind.String(), maxIDText),
		Source: boundedText(n.Source, maxIDText), Category: noticeCategory(n),
	}
}

func noticeCategory(n tui.Notice) string {
	text := strings.ToLower(n.Source + " " + n.Message + " " + n.Err)
	switch {
	case containsAny(text, "rate limit", "rate_limit", "rate-limited", "quota", "credits", "billing"):
		return noticeRateLimited
	case containsAny(text, "conflict", "rebase"):
		return noticeConflict
	case containsAny(text, "check_command", "check command", "check_failed", "check failed", "checks failed"):
		return noticeCheck
	case containsAny(text, "review", "verdict", "feedback", "tester", "lens"):
		return noticeReview
	default:
		return noticeGeneric
	}
}

func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}
