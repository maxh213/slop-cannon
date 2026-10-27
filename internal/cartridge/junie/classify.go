package junie

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
	needle string
	kind   cartridge.Kind
}

var textRules = []textRule{
	{"insufficient credit", cartridge.KindCreditsExhausted},
	{"out of credits", cartridge.KindCreditsExhausted},
	{"credits exhausted", cartridge.KindCreditsExhausted},
	{"credit limit", cartridge.KindCreditsExhausted},
	{"quota exceeded", cartridge.KindCreditsExhausted},
	{"quota exhausted", cartridge.KindCreditsExhausted},
	{"insufficient balance", cartridge.KindCreditsExhausted},
	{"payment required", cartridge.KindCreditsExhausted},
	{"billing", cartridge.KindCreditsExhausted},

	{"authentication required", cartridge.KindAuth},
	{"authentication failed", cartridge.KindAuth},
	{"failed authentication", cartridge.KindAuth},
	{"not authenticated", cartridge.KindAuth},
	{"not logged in", cartridge.KindAuth},
	{"invalid api key", cartridge.KindAuth},
	{"incorrect api key", cartridge.KindAuth},
	{"api key is invalid", cartridge.KindAuth},
	{"invalid token", cartridge.KindAuth},
	{"token expired", cartridge.KindAuth},
	{"unauthorized", cartridge.KindAuth},
	{"unauthenticated", cartridge.KindAuth},
	{"response 401", cartridge.KindAuth},
	{"http 401", cartridge.KindAuth},

	{"rate limit", cartridge.KindRateLimited},
	{"rate-limit", cartridge.KindRateLimited},
	{"too many requests", cartridge.KindRateLimited},
	{"resource_exhausted", cartridge.KindRateLimited},
	{"response 429", cartridge.KindRateLimited},
	{"http 429", cartridge.KindRateLimited},
	{"overloaded", cartridge.KindRateLimited},
	{"usage limit", cartridge.KindRateLimited},
	{"try again later", cartridge.KindRateLimited},
}

// KindForText classifies Junie diagnostics, giving credits errors precedence.
func KindForText(text string) cartridge.Kind {
	text = strings.ToLower(text)
	for _, rule := range textRules {
		if strings.Contains(text, rule.needle) {
			return rule.kind
		}
	}
	return cartridge.KindUnknown
}

// Classify converts the process outcome and final JSON into a typed error.
func Classify(name string, p *Parser, procErr error, exitCode int) error {
	switch cartridge.KindOf(procErr) {
	case cartridge.KindTimeout:
		return cartridge.NewError(cartridge.KindTimeout, name, "session timed out", procErr)
	case cartridge.KindKilled:
		return cartridge.NewError(cartridge.KindKilled, name, "", procErr)
	}

	p.parse()
	if procErr == nil && p.parseErr == nil {
		return nil
	}

	kind := KindForText(p.diagnostics())
	if kind == cartridge.KindUnknown {
		kind = cartridge.KindCrash
	}
	if procErr != nil {
		msg := fmt.Sprintf("junie exited with status %d", exitCode)
		if tail := firstNonEmpty(p.diagnosticLines()); tail != "" {
			msg += ": " + truncate(tail, 300)
		}
		return cartridge.NewError(kind, name, msg, procErr)
	}
	msg := p.parseErr.Error()
	if tail := firstNonEmpty(p.diagnosticLines()); tail != "" {
		msg += ": " + truncate(tail, 300)
	}
	return cartridge.NewError(kind, name, msg, nil)
}

// ClassifyStart classifies a failure to start the Junie process.
func ClassifyStart(name string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return cartridge.NewError(cartridge.KindNotInstalled, name, "junie is not on PATH", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return cartridge.NewError(cartridge.KindKilled, name, "cancelled before start", err)
	}
	return cartridge.NewError(cartridge.KindCrash, name, "could not start junie", err)
}

// ClassifyProbe classifies a version-probe failure.
func ClassifyProbe(name, what string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return cartridge.NewError(cartridge.KindNotInstalled, name, "junie is not on PATH", err)
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

func firstNonEmpty(lines []string) string {
	for _, line := range lines {
		if s := strings.TrimSpace(line); s != "" {
			return s
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && s[cut]&0xc0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
