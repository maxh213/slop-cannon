package cursor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/proc"
)

// textRule maps a case-insensitive substring of CLI output to an error kind.
type textRule struct {
	Needle string
	Kind   cartridge.Kind
}

// TextRules is the ordered substring table used by Classify: credits first
// (billing messages mention limits too), then auth, then rate limits. It is
// exported so the documentation test can render it; do not mutate it.
var TextRules = []textRule{
	// Credits, plan and billing exhaustion: the cartridge is dead (D-18).
	{"insufficient credit", cartridge.KindCreditsExhausted},
	{"out of credits", cartridge.KindCreditsExhausted},
	{"payment required", cartridge.KindCreditsExhausted},
	{"spending limit", cartridge.KindCreditsExhausted},
	{"quota exceeded", cartridge.KindCreditsExhausted},
	{"billing", cartridge.KindCreditsExhausted},

	// Missing or rejected login (fixture logged-out.stderr).
	{"authentication required", cartridge.KindAuth},
	{"not logged in", cartridge.KindAuth},
	{"agent login", cartridge.KindAuth},
	{"invalid api key", cartridge.KindAuth},
	{"unauthorized", cartridge.KindAuth},
	{"unauthenticated", cartridge.KindAuth},
	{"token expired", cartridge.KindAuth},

	// Temporary rate limits and overload: back off and retry.
	{"rate limit", cartridge.KindRateLimited},
	{"rate-limit", cartridge.KindRateLimited},
	{"resource_exhausted", cartridge.KindRateLimited},
	{"too many requests", cartridge.KindRateLimited},
	{"overloaded", cartridge.KindRateLimited},
	{"usage limit", cartridge.KindRateLimited},
	{"hit your limit", cartridge.KindRateLimited},
	{"try again later", cartridge.KindRateLimited},
}

// KindForText classifies free text with TextRules. It returns KindUnknown
// when nothing matches.
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
//  2. A result line: success unless is_error is set or the subtype is not
//     "success"; a failing one is classified by its text, then KindCrash.
//  3. A non-zero exit without a result line: stderr and noise text (an
//     unknown model and a missing login both end here, fixtures
//     unknown-model.stderr and logged-out.stderr), then KindCrash.
//  4. A zero exit without a result line: KindCrash ("no result line").
func Classify(name string, p *Parser, procErr error, exitCode int) error {
	switch cartridge.KindOf(procErr) {
	case cartridge.KindTimeout:
		return cartridge.NewError(cartridge.KindTimeout, name, "session timed out", procErr)
	case cartridge.KindKilled:
		return cartridge.NewError(cartridge.KindKilled, name, "", procErr)
	}

	diag := strings.Join(append(append([]string{}, p.stderr...), p.noise...), "\n")
	if r := p.result; r != nil {
		if !r.IsError && (r.Subtype == "" || r.Subtype == subtypeSuccess) {
			return nil
		}
		msg := resultText(r)
		kind := KindForText(msg + "\n" + diag)
		if kind == cartridge.KindUnknown {
			kind = cartridge.KindCrash
		}
		if msg == "" {
			msg = "cursor-agent reported " + strings.ReplaceAll(r.Subtype, "_", " ")
		}
		return cartridge.NewError(kind, name, msg, nil)
	}

	if procErr != nil {
		kind := KindForText(diag)
		if kind == cartridge.KindUnknown {
			kind = cartridge.KindCrash
		}
		msg := fmt.Sprintf("cursor-agent exited with status %d before printing a result", exitCode)
		if tail := lastNonEmpty(p.stderr); tail != "" {
			msg += ": " + truncate(tail, 300)
		}
		return cartridge.NewError(kind, name, msg, procErr)
	}
	msg := fmt.Sprintf("cursor-agent exited 0 without a result line (%d stdout lines)", p.lines)
	if tail := lastNonEmpty(p.noise); tail != "" {
		msg += ": " + truncate(tail, 300)
	}
	return cartridge.NewError(cartridge.KindCrash, name, msg, nil)
}

// resultText is what a failing result line says: its error fields, else its
// result text.
func resultText(r *resultMessage) string {
	var parts []string
	if s := strings.TrimSpace(r.Error); s != "" {
		parts = append(parts, s)
	}
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

// ClassifyStart wraps an error from starting the process: a missing binary
// is KindNotInstalled, anything else KindCrash.
func ClassifyStart(name string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return cartridge.NewError(cartridge.KindNotInstalled, name, "cursor-agent is not on PATH", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return cartridge.NewError(cartridge.KindKilled, name, "cancelled before start", err)
	}
	return cartridge.NewError(cartridge.KindCrash, name, "could not start cursor-agent", err)
}

// ClassifyProbe wraps a Detect probe failure: timeouts, missing binary,
// otherwise a crash (or the kind its output names).
func ClassifyProbe(name, what string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return cartridge.NewError(cartridge.KindNotInstalled, name, "cursor-agent is not on PATH", err)
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

func lastNonEmpty(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return ""
}
