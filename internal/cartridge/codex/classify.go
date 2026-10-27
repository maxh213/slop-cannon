package codex

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

// TextRules classifies Codex diagnostics, checking exhausted credits before
// authentication and temporary rate limits.
var TextRules = []textRule{
	{"insufficient_quota", cartridge.KindCreditsExhausted},
	{"insufficient credit", cartridge.KindCreditsExhausted},
	{"out of credits", cartridge.KindCreditsExhausted},
	{"credit balance", cartridge.KindCreditsExhausted},
	{"payment required", cartridge.KindCreditsExhausted},
	{"billing", cartridge.KindCreditsExhausted},
	{"quota exceeded", cartridge.KindCreditsExhausted},
	{"spending limit", cartridge.KindCreditsExhausted},

	{"not logged in", cartridge.KindAuth},
	{"not authenticated", cartridge.KindAuth},
	{"codex login", cartridge.KindAuth},
	{"invalid api key", cartridge.KindAuth},
	{"incorrect api key", cartridge.KindAuth},
	{"missing environment variable: openai_api_key", cartridge.KindAuth},
	{"authentication", cartridge.KindAuth},
	{"unauthorized", cartridge.KindAuth},
	{"unauthenticated", cartridge.KindAuth},
	{"token expired", cartridge.KindAuth},
	{"401", cartridge.KindAuth},

	{"rate_limit", cartridge.KindRateLimited},
	{"rate limit", cartridge.KindRateLimited},
	{"rate-limit", cartridge.KindRateLimited},
	{"too many requests", cartridge.KindRateLimited},
	{"resource_exhausted", cartridge.KindRateLimited},
	{"usage limit", cartridge.KindRateLimited},
	{"hit your limit", cartridge.KindRateLimited},
	{"overloaded", cartridge.KindRateLimited},
	{"try again later", cartridge.KindRateLimited},
	{"429", cartridge.KindRateLimited},
}

// KindForText classifies Codex diagnostics using TextRules.
func KindForText(text string) cartridge.Kind {
	low := strings.ToLower(text)
	for _, rule := range TextRules {
		if strings.Contains(low, rule.Needle) {
			return rule.Kind
		}
	}
	return cartridge.KindUnknown
}

// Classify requires both a successful process and a valid turn.completed.
// Explicit turn.failed and top-level error events are failures even on exit 0.
func Classify(name string, p *Parser, procErr error, exitCode int) error {
	switch cartridge.KindOf(procErr) {
	case cartridge.KindTimeout:
		return cartridge.NewError(cartridge.KindTimeout, name, "session timed out", procErr)
	case cartridge.KindKilled:
		return cartridge.NewError(cartridge.KindKilled, name, "", procErr)
	}

	diag := strings.Join(append(append([]string{}, p.stderr...), p.noise...), "\n")
	failure := p.failure
	if failure == "" {
		failure = p.streamErr
	}
	if failure != "" {
		return cartridge.NewError(kindOrCrash(failure+"\n"+diag), name, failure, procErr)
	}
	if procErr != nil {
		kind := kindOrCrash(diag)
		msg := fmt.Sprintf("codex exited with status %d", exitCode)
		if tail := lastNonEmpty(p.stderr); tail != "" {
			msg += ": " + truncate(tail, 300)
		}
		return cartridge.NewError(kind, name, msg, procErr)
	}
	if problem := p.terminalProblem(); problem != "" {
		msg := problem
		if tail := lastNonEmpty(p.noise); tail != "" {
			msg += ": " + truncate(tail, 300)
		}
		return cartridge.NewError(cartridge.KindCrash, name, msg, nil)
	}
	return nil
}

func kindOrCrash(text string) cartridge.Kind {
	if kind := KindForText(text); kind != cartridge.KindUnknown {
		return kind
	}
	return cartridge.KindCrash
}

// ClassifyStart converts a process-start failure into a cartridge error.
func ClassifyStart(name string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return cartridge.NewError(cartridge.KindNotInstalled, name, "codex is not on PATH", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return cartridge.NewError(cartridge.KindKilled, name, "cancelled before start", err)
	}
	return cartridge.NewError(cartridge.KindCrash, name, "could not start codex", err)
}

// ClassifyProbe converts a version or login probe failure into a cartridge error.
func ClassifyProbe(name, what string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return cartridge.NewError(cartridge.KindNotInstalled, name, "codex is not on PATH", err)
	}
	if k := cartridge.KindOf(err); k == cartridge.KindTimeout || k == cartridge.KindKilled {
		return cartridge.NewError(k, name, what+" did not finish", err)
	}
	var exitErr *proc.ExitError
	if errors.As(err, &exitErr) {
		if kind := KindForText(exitErr.Result.Stderr + "\n" + exitErr.Result.Stdout); kind != cartridge.KindUnknown {
			return cartridge.NewError(kind, name, what+" failed", err)
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
