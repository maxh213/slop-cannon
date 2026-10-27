package grok

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/proc"
)

type textRule struct {
	Needle string
	Kind   cartridge.Kind
}

// TextRules classifies CLI diagnostics in precedence order.
var TextRules = []textRule{
	{"insufficient credit", cartridge.KindCreditsExhausted},
	{"out of credits", cartridge.KindCreditsExhausted},
	{"credits exhausted", cartridge.KindCreditsExhausted},
	{"insufficient_quota", cartridge.KindCreditsExhausted},
	{"quota exceeded", cartridge.KindCreditsExhausted},
	{"payment required", cartridge.KindCreditsExhausted},
	{"http 402", cartridge.KindCreditsExhausted},
	{"billing", cartridge.KindCreditsExhausted},
	{"spending limit", cartridge.KindCreditsExhausted},

	{"not authenticated", cartridge.KindAuth},
	{"authentication failed", cartridge.KindAuth},
	{"authentication required", cartridge.KindAuth},
	{"unauthenticated", cartridge.KindAuth},
	{"not logged in", cartridge.KindAuth},
	{"please login", cartridge.KindAuth},
	{"please log in", cartridge.KindAuth},
	{"grok login", cartridge.KindAuth},
	{"invalid api key", cartridge.KindAuth},
	{"invalid xai_api_key", cartridge.KindAuth},
	{"unauthorized", cartridge.KindAuth},
	{"http 401", cartridge.KindAuth},
	{"http 403", cartridge.KindAuth},
	{"token expired", cartridge.KindAuth},

	{"rate limit", cartridge.KindRateLimited},
	{"rate-limit", cartridge.KindRateLimited},
	{"rate_limit", cartridge.KindRateLimited},
	{"too many requests", cartridge.KindRateLimited},
	{"http 429", cartridge.KindRateLimited},
	{"resource_exhausted", cartridge.KindRateLimited},
	{"overloaded", cartridge.KindRateLimited},
	{"try again later", cartridge.KindRateLimited},
}

// KindForText maps free-form Grok diagnostics to a cartridge error kind.
func KindForText(text string) cartridge.Kind {
	text = strings.ToLower(text)
	for _, rule := range TextRules {
		if strings.Contains(text, rule.Needle) {
			return rule.Kind
		}
	}
	return cartridge.KindUnknown
}

// Classify combines process status, terminal events, and diagnostics.
func Classify(name string, p *Parser, procErr error, exitCode int) error {
	switch cartridge.KindOf(procErr) {
	case cartridge.KindTimeout:
		return cartridge.NewError(cartridge.KindTimeout, name, "session timed out", procErr)
	case cartridge.KindKilled:
		return cartridge.NewError(cartridge.KindKilled, name, "", procErr)
	}

	diag := strings.Join(append(append([]string{}, p.stderr...), p.noise...), "\n")
	if p.errorMsg != "" || p.endError != "" {
		msg := firstNonEmpty(p.errorMsg, p.endError)
		kind := KindForText(msg + "\n" + diag)
		if kind == cartridge.KindUnknown {
			kind = cartridge.KindCrash
		}
		return cartridge.NewError(kind, name, msg, procErr)
	}
	if procErr != nil {
		kind := KindForText(diag)
		if kind == cartridge.KindUnknown {
			kind = cartridge.KindCrash
		}
		msg := fmt.Sprintf("grok exited with status %d", exitCode)
		if !p.ended {
			msg += " before printing an end event"
		}
		if tail := lastNonEmpty(p.stderr); tail != "" {
			msg += ": " + truncate(tail, 300)
		}
		return cartridge.NewError(kind, name, msg, procErr)
	}
	if !p.ended {
		msg := fmt.Sprintf("grok exited 0 without an end event (%d stdout lines)", p.lines)
		if tail := lastNonEmpty(p.noise); tail != "" {
			msg += ": " + truncate(tail, 300)
		}
		return cartridge.NewError(cartridge.KindCrash, name, msg, nil)
	}
	return nil
}

// ClassifyStart maps a process startup failure to a typed cartridge error.
func ClassifyStart(name string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return cartridge.NewError(cartridge.KindNotInstalled, name, "grok is not on PATH", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return cartridge.NewError(cartridge.KindKilled, name, "cancelled before start", err)
	}
	return cartridge.NewError(cartridge.KindCrash, name, "could not start grok", err)
}

// ClassifyProbe maps a version or lookup probe failure to a typed error.
func ClassifyProbe(name, what string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return cartridge.NewError(cartridge.KindNotInstalled, name, "grok is not on PATH", err)
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
