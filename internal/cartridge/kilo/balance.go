package kilo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/proc"
)

// ErrBudget identifies a balance-floor refusal, profile failure, or exhausted
// in-session allowance. It also maps to cartridge.ErrCreditsExhausted so a
// Dandelion caller can reroute without Kilo.
var ErrBudget = errors.New("kilo: balance budget unavailable or exhausted")

func envLookup(cfgEnv, specEnv map[string]string, name string) (string, bool) {
	if value, ok := specEnv[name]; ok {
		return value, true
	}
	if value, ok := cfgEnv[name]; ok {
		return value, true
	}
	return os.LookupEnv(name)
}

func balanceFloor(cfgEnv, specEnv map[string]string) (*float64, error) {
	raw, ok := envLookup(cfgEnv, specEnv, minBalanceEnv)
	if !ok {
		return nil, nil
	}
	floor, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(floor) || math.IsInf(floor, 0) || floor < 0 {
		return nil, fmt.Errorf("%w: invalid %s", ErrBudget, minBalanceEnv)
	}
	return &floor, nil
}

// checkBalance runs immediately before a firing and returns its spend
// allowance. It deliberately discards all profile fields except balance and
// never includes profile output in an error.
func (a *Adapter) checkBalance(ctx context.Context, spec cartridge.RunSpec) (float64, error) {
	floor, err := balanceFloor(a.cfg.Env, spec.Env)
	if err != nil {
		return 0, cartridge.NewError(cartridge.KindCreditsExhausted, a.name, "invalid balance floor", err)
	}
	if floor == nil {
		return 0, nil
	}
	result, probeErr := proc.Run(ctx, proc.Spec{
		Command: a.Binary(), Args: []string{"profile", "--json"},
		Env: a.childEnv(spec.Env, ""), InheritEnv: InheritEnv(spec.EnvAllowlist()),
		Timeout: 20 * time.Second, ReapDescendants: true,
	})
	if probeErr != nil {
		return 0, budgetError(a.name, "balance profile failed")
	}
	var profile struct {
		Balance *float64 `json:"balance"`
	}
	if json.Unmarshal([]byte(result.Stdout), &profile) != nil || profile.Balance == nil ||
		math.IsNaN(*profile.Balance) || math.IsInf(*profile.Balance, 0) {
		return 0, budgetError(a.name, "balance profile did not contain a valid balance")
	}
	balance := *profile.Balance
	if balance <= *floor {
		return 0, budgetError(a.name, "balance is at or below the configured floor")
	}
	return balance - *floor, nil
}

func budgetError(name, message string) error {
	return cartridge.NewError(cartridge.KindCreditsExhausted, name, message, ErrBudget)
}
