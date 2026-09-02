package claude

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/proc"
)

// Result subtypes of the stream-json result line that mean the run did not
// finish cleanly. error_max_turns was observed live (fixture
// error-max-turns.jsonl); the others are documented by the CLI.
const (
	subtypeErrorMaxTurns        = "error_max_turns"
	subtypeErrorMaxBudget       = "error_max_budget_usd"
	subtypeErrorDuringExecution = "error_during_execution"
)

// textRule maps a case-insensitive substring of CLI output to an error kind.
// The rules are checked in order: credits first (its messages mention
// limits too), then auth, then rate limits. See docs/cartridges.md for the
// table and where each phrase comes from.
type textRule struct {
	Needle string
	Kind   cartridge.Kind
}

// TextRules is the ordered substring table used by Classify. It is exported
// so the documentation test and P2-10's guide can render it; do not mutate
// it at runtime.
var TextRules = []textRule{
	// Credits, quota and billing exhaustion: the cartridge is dead (D-18).
	{"credit balance", cartridge.KindCreditsExhausted},
	{"insufficient credit", cartridge.KindCreditsExhausted},
	{"insufficient_quota", cartridge.KindCreditsExhausted},
	{"out of credits", cartridge.KindCreditsExhausted},
	{"purchase more", cartridge.KindCreditsExhausted},
	{"plans & billing", cartridge.KindCreditsExhausted},
	{"billing", cartridge.KindCreditsExhausted},
	{"payment required", cartridge.KindCreditsExhausted},
	{"spending limit", cartridge.KindCreditsExhausted},
	{"quota exceeded", cartridge.KindCreditsExhausted},
	{"extra usage", cartridge.KindCreditsExhausted},

	// Missing or rejected login.
	{"not logged in", cartridge.KindAuth},
	{"please run /login", cartridge.KindAuth},
	{"please login", cartridge.KindAuth},
	{"invalid api key", cartridge.KindAuth},
	{"invalid x-api-key", cartridge.KindAuth},
	{"authentication_error", cartridge.KindAuth},
	{"authentication failed", cartridge.KindAuth},
	{"invalid authentication", cartridge.KindAuth},
	{"oauth token", cartridge.KindAuth},
	{"token has expired", cartridge.KindAuth},
	{"token expired", cartridge.KindAuth},
	{"unauthorized", cartridge.KindAuth},
	{"permission_error", cartridge.KindAuth},

	// Temporary rate limits and overload: back off and retry.
	{"rate_limit_error", cartridge.KindRateLimited},
	{"rate limit", cartridge.KindRateLimited},
	{"rate-limit", cartridge.KindRateLimited},
	{"too many requests", cartridge.KindRateLimited},
	{"overloaded_error", cartridge.KindRateLimited},
	{"overloaded", cartridge.KindRateLimited},
	{"usage limit", cartridge.KindRateLimited},
	{"hit your limit", cartridge.KindRateLimited},
	{"limit reached", cartridge.KindRateLimited},
	{"try again later", cartridge.KindRateLimited},
}

// KindForStatus maps an HTTP status the CLI reported in api_error_status
// onto an error kind: 401/403 auth, 402 credits, 429 rate limited, 5xx
// treated as a temporary overload (rate limited), anything else a crash.
// Zero (unknown) returns KindUnknown.
func KindForStatus(status int) cartridge.Kind {
	switch status {
	case 0:
		return cartridge.KindUnknown
	case 401, 403:
		return cartridge.KindAuth
	case 402:
		return cartridge.KindCreditsExhausted
	case 429, 503, 529:
		return cartridge.KindRateLimited
	default:
		return cartridge.KindCrash
	}
}

// KindForText classifies free text (a result message, an error entry, a
// stderr line) with TextRules. It returns KindUnknown when nothing matches.
func KindForText(text string) cartridge.Kind {
	t := strings.ToLower(text)
	if t == "" {
		return cartridge.KindUnknown
	}
	for _, r := range TextRules {
		if strings.Contains(t, r.Needle) {
			return r.Kind
		}
	}
	return cartridge.KindUnknown
}

// Classify decides how a finished session ended. procErr is what
// proc.Handle.Wait returned; p is the parser that consumed the output. A nil
// error means success. The order of precedence is:
//
//  1. Timeout or deliberate kill (from the process), regardless of output.
//  2. A result line that reports an error: its HTTP status, then its text
//     (result, errors, last assistant text), then any rate_limit_event
//     that said "rejected", then the subtype (max turns and friends are
//     crashes: retryable, nothing wrong with the cartridge).
//  3. A non-zero exit without a result line: stderr and noise text, then
//     KindCrash.
//  4. A zero exit without a result line: KindCrash ("no result line").
func Classify(name string, p *Parser, procErr error, exitCode int) error {
	switch cartridge.KindOf(procErr) {
	case cartridge.KindTimeout:
		return cartridge.NewError(cartridge.KindTimeout, name, "session timed out", procErr)
	case cartridge.KindKilled:
		return cartridge.NewError(cartridge.KindKilled, name, "", procErr)
	}

	if r := p.result; r != nil {
		if !r.IsError && !isErrorSubtype(r.Subtype) {
			return nil
		}
		msg := resultMessageText(r)
		kind := cartridge.KindUnknown
		if r.APIErrorStatus != nil {
			kind = KindForStatus(*r.APIErrorStatus)
		}
		if kind == cartridge.KindUnknown || kind == cartridge.KindCrash {
			if k := KindForText(strings.Join([]string{msg, p.lastText, strings.Join(p.stderr, "\n")}, "\n")); k != cartridge.KindUnknown {
				kind = k
			}
		}
		if kind == cartridge.KindUnknown && p.rateLimit != nil {
			kind = cartridge.KindRateLimited
		}
		if kind == cartridge.KindUnknown {
			kind = cartridge.KindCrash
		}
		if msg == "" {
			msg = "claude reported " + describeSubtype(r)
		} else if r.Subtype != subtypeSuccess || r.APIErrorStatus != nil {
			msg = describeSubtype(r) + ": " + msg
		}
		e := cartridge.NewError(kind, name, msg, nil)
		e.RetryAfter = retryAfter(p.rateLimit, time.Now())
		return e
	}

	diag := strings.Join(append(append([]string{}, p.stderr...), p.noise...), "\n")
	if procErr != nil {
		kind := KindForText(diag)
		if kind == cartridge.KindUnknown && p.rateLimit != nil {
			kind = cartridge.KindRateLimited
		}
		if kind == cartridge.KindUnknown {
			kind = cartridge.KindCrash
		}
		msg := fmt.Sprintf("claude exited with status %d before printing a result", exitCode)
		if tail := lastNonEmpty(p.stderr); tail != "" {
			msg += ": " + tail
		}
		e := cartridge.NewError(kind, name, msg, procErr)
		e.RetryAfter = retryAfter(p.rateLimit, time.Now())
		return e
	}
	msg := fmt.Sprintf("claude exited 0 without a result line (%d stdout lines)", p.lines)
	if tail := lastNonEmpty(p.noise); tail != "" {
		msg += ": " + tail
	}
	return cartridge.NewError(cartridge.KindCrash, name, msg, nil)
}

// ClassifyStart wraps an error from starting the process: a missing binary
// is KindNotInstalled, anything else KindCrash.
func ClassifyStart(name string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return cartridge.NewError(cartridge.KindNotInstalled, name, "claude is not on PATH", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return cartridge.NewError(cartridge.KindKilled, name, "cancelled before start", err)
	}
	return cartridge.NewError(cartridge.KindCrash, name, "could not start claude", err)
}

// ClassifyProbe wraps a Detect probe failure: timeouts, missing binary,
// otherwise a crash carrying the last stderr line.
func ClassifyProbe(name, what string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return cartridge.NewError(cartridge.KindNotInstalled, name, "claude is not on PATH", err)
	}
	if k := cartridge.KindOf(err); k == cartridge.KindTimeout || k == cartridge.KindKilled {
		return cartridge.NewError(k, name, what+" did not finish", err)
	}
	var ee *proc.ExitError
	if errors.As(err, &ee) {
		if k := KindForText(ee.Result.Stderr + "\n" + ee.Result.Stdout); k != cartridge.KindUnknown {
			return cartridge.NewError(k, name, what+" failed", err)
		}
	}
	return cartridge.NewError(cartridge.KindCrash, name, what+" failed", err)
}

func isErrorSubtype(s string) bool {
	return s != "" && s != subtypeSuccess && strings.HasPrefix(s, "error")
}

func describeSubtype(r *resultMessage) string {
	switch r.Subtype {
	case subtypeErrorMaxTurns:
		return "max turns reached"
	case subtypeErrorMaxBudget:
		return "budget exhausted"
	case subtypeErrorDuringExecution:
		return "error during execution"
	case subtypeSuccess, "":
		if r.APIErrorStatus != nil {
			return fmt.Sprintf("api error (HTTP %d)", *r.APIErrorStatus)
		}
		if r.TerminalReason != "" {
			return r.TerminalReason
		}
		return "an error"
	default:
		return strings.ReplaceAll(strings.TrimPrefix(r.Subtype, "error_"), "_", " ")
	}
}

// resultMessageText is the text the result line carries about a failure:
// the errors array joined, else the result field.
func resultMessageText(r *resultMessage) string {
	var parts []string
	for _, e := range r.Errors {
		if s := strings.TrimSpace(e); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "; ")
	}
	return strings.TrimSpace(r.Result)
}

// retryAfter converts a rejected rate_limit_event into a back-off relative
// to now; zero when there is none or the reset is in the past.
func retryAfter(rl *rateLimitInfo, now time.Time) time.Duration {
	if rl == nil || rl.ResetsAt <= 0 {
		return 0
	}
	d := time.Unix(rl.ResetsAt, 0).Sub(now)
	if d < 0 {
		return 0
	}
	return d
}

func lastNonEmpty(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return ""
}
