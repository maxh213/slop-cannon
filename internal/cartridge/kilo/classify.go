package kilo

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"slop-cannon/internal/cartridge"
)

type textRule struct {
	needle string
	kind   cartridge.Kind
}

var textRules = []textRule{
	{"insufficient balance", cartridge.KindCreditsExhausted},
	{"insufficient credit", cartridge.KindCreditsExhausted},
	{"insufficient_quota", cartridge.KindCreditsExhausted},
	{"credit balance", cartridge.KindCreditsExhausted},
	{"out of credits", cartridge.KindCreditsExhausted},
	{"quota exhausted", cartridge.KindCreditsExhausted},
	{"quota exceeded", cartridge.KindCreditsExhausted},
	{"spending limit", cartridge.KindCreditsExhausted},
	{"payment required", cartridge.KindCreditsExhausted},
	{"billing", cartridge.KindCreditsExhausted},
	{"not authenticated", cartridge.KindAuth},
	{"not logged in", cartridge.KindAuth},
	{"please login", cartridge.KindAuth},
	{"authentication", cartridge.KindAuth},
	{"invalid api key", cartridge.KindAuth},
	{"unauthorized", cartridge.KindAuth},
	{"token expired", cartridge.KindAuth},
	{"rate limit", cartridge.KindRateLimited},
	{"rate_limit", cartridge.KindRateLimited},
	{"too many requests", cartridge.KindRateLimited},
	{"overloaded", cartridge.KindRateLimited},
	{"try again later", cartridge.KindRateLimited},
	{"429", cartridge.KindRateLimited},
}

// KindForText classifies Kilo diagnostics, with credits before generic 429s.
func KindForText(text string) cartridge.Kind {
	text = strings.ToLower(text)
	for _, rule := range textRules {
		if strings.Contains(text, rule.needle) {
			return rule.kind
		}
	}
	return cartridge.KindUnknown
}

// KindForStatus maps an explicit structured HTTP status to a cartridge kind.
func KindForStatus(status int) cartridge.Kind {
	switch status {
	case 401, 403:
		return cartridge.KindAuth
	case 402:
		return cartridge.KindCreditsExhausted
	case 429, 500, 502, 503, 504, 529:
		return cartridge.KindRateLimited
	case 0:
		return cartridge.KindUnknown
	default:
		return cartridge.KindCrash
	}
}

// Classify converts parser and process outcomes into a terminal error.
func Classify(name string, p *Parser, procErr error, exitCode int, budgetExhausted bool) error {
	if budgetExhausted {
		return budgetError(name, "in-session allowance exhausted")
	}
	switch cartridge.KindOf(procErr) {
	case cartridge.KindTimeout:
		return cartridge.NewError(cartridge.KindTimeout, name, "session timed out", procErr)
	case cartridge.KindKilled:
		return cartridge.NewError(cartridge.KindKilled, name, "", procErr)
	}
	diagnostic := strings.Join(append(append([]string{}, p.stderr...), p.noise...), "\n")
	if p.ErrorMessage() != "" {
		diagnostic += "\n" + p.ErrorMessage()
	}
	kind := KindForText(diagnostic)
	if kind == cartridge.KindUnknown && p.ErrorStatus() != 0 {
		kind = KindForStatus(p.ErrorStatus())
	}
	if p.ErrorMessage() != "" {
		if kind == cartridge.KindUnknown {
			kind = cartridge.KindCrash
		}
		return cartridge.NewError(kind, name, "kilo reported an error: "+p.ErrorMessage(), procErr)
	}
	if procErr != nil || exitCode != 0 {
		if kind == cartridge.KindUnknown {
			kind = cartridge.KindCrash
		}
		message := fmt.Sprintf("kilo exited with status %d", exitCode)
		if tail := lastNonEmpty(p.stderr); tail != "" {
			message += ": " + tail
		}
		return cartridge.NewError(kind, name, message, procErr)
	}
	if problem := p.ProtocolProblem(); problem != "" {
		return cartridge.NewError(cartridge.KindCrash, name, problem, nil)
	}
	return nil
}

// ClassifyStart wraps an error that prevented the CLI from starting.
func ClassifyStart(name string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return cartridge.NewError(cartridge.KindNotInstalled, name, "kilo is not on PATH", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return cartridge.NewError(cartridge.KindKilled, name, "cancelled before start", err)
	}
	return cartridge.NewError(cartridge.KindCrash, name, "could not start kilo", err)
}

// ClassifyProbe wraps a local version-probe failure.
func ClassifyProbe(name, what string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return cartridge.NewError(cartridge.KindNotInstalled, name, "kilo is not on PATH", err)
	}
	if kind := cartridge.KindOf(err); kind == cartridge.KindTimeout || kind == cartridge.KindKilled {
		return cartridge.NewError(kind, name, what+" did not finish", err)
	}
	return cartridge.NewError(cartridge.KindCrash, name, what+" failed", err)
}

func lastNonEmpty(lines []string) string {
	for index := len(lines) - 1; index >= 0; index-- {
		if text := strings.TrimSpace(lines[index]); text != "" {
			return text
		}
	}
	return ""
}
