package dandelion

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/claude"
	"slop-cannon/internal/proc"
)

// Router invocation.
const (
	routeSubcommand = "route"
	flagHigh        = "--high"
	flagMaxAge      = "--max-age"
	// disableEnv is the variable the router reads for providers to skip:
	// never probed, treated as unavailable.
	disableEnv = "DANDELION_DISABLE"
)

// Exit codes of "dandelion route".
const (
	exitNoRoute     = 1 // "none": nothing is routable
	exitRoutesFault = 2 // routes.json is unreadable or invalid; stderr says why
)

// routeKey is what a cached route is valid for: the mode and the account
// set the router was allowed to pick from.
type routeKey struct {
	mode    Mode
	allowed string
}

// cachedRoute is a route and when the router printed it.
type cachedRoute struct {
	route Route
	at    time.Time
}

// flight is one router call that concurrent firings with the same key share.
type flight struct {
	done  chan struct{}
	route Route
	err   error
}

// route returns the route for key: from the cache while it is younger than
// CacheTTL, else from the call already in flight for key, else from a new
// router call that later callers join. Only successes are cached. cached
// reports whether this caller did not start the call itself.
func (a *Adapter) route(ctx context.Context, key routeKey, disable string) (r Route, cached bool, err error) {
	a.mu.Lock()
	if c, ok := a.cache[key]; ok && a.now().Sub(c.at) < CacheTTL {
		a.mu.Unlock()
		return c.route, true, nil
	}
	f, joined := a.flights[key]
	if !joined {
		f = &flight{done: make(chan struct{})}
		a.flights[key] = f
		fresh := a.fresh[key]
		delete(a.fresh, key)
		// The call outlives a caller that gives up: others may be waiting
		// for it, and its answer is cached for the next firing.
		go a.fly(context.WithoutCancel(ctx), key, disable, fresh, f)
	}
	a.mu.Unlock()

	select {
	case <-f.done:
		return f.route, joined, f.err
	case <-ctx.Done():
		return Route{}, joined, cartridge.NewError(cartridge.KindKilled, a.name, "stopped while waiting for dandelion route", ctx.Err())
	}
}

// fly runs one router call for key and publishes its answer.
func (a *Adapter) fly(ctx context.Context, key routeKey, disable string, fresh bool, f *flight) {
	r, err := a.callRouter(ctx, key.mode, disable, fresh)
	a.mu.Lock()
	delete(a.flights, key)
	if err == nil {
		a.cache[key] = cachedRoute{route: r, at: a.now()}
	}
	f.route, f.err = r, err
	a.mu.Unlock()
	close(f.done)
}

// invalidate drops the cached route for key when it is still r, and makes
// the next call for key probe afresh instead of answering from dandelion's
// own snapshot: the account r named has just failed.
func (a *Adapter) invalidate(key routeKey, r Route) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.cache[key]; ok && c.route == r {
		delete(a.cache, key)
	}
	a.fresh[key] = true
}

// routerArgs builds "route [--high] [--max-age 120] [configured args…]".
// fresh leaves --max-age out so the router probes every provider again.
func (a *Adapter) routerArgs(mode Mode, fresh bool) []string {
	args := []string{routeSubcommand}
	if mode == ModeHigh {
		args = append(args, flagHigh)
	}
	if !fresh {
		args = append(args, flagMaxAge, strconv.Itoa(int(CacheTTL/time.Second)))
	}
	return append(args, a.cfg.Args...)
}

// callRouter runs the router once and classifies the outcome (see the
// package documentation for the error table).
func (a *Adapter) callRouter(ctx context.Context, mode Mode, disable string, fresh bool) (Route, error) {
	bin := a.Binary()
	if _, err := proc.LookPath(bin); err != nil {
		return Route{}, cartridge.NewError(cartridge.KindNotInstalled, a.name, bin+" not found on PATH", err)
	}
	env := make(map[string]string, len(a.cfg.Env)+1)
	for k, v := range a.cfg.Env {
		env[k] = v
	}
	env[disableEnv] = disable
	res, err := proc.Run(ctx, proc.Spec{
		Command:    bin,
		Args:       a.routerArgs(mode, fresh),
		Env:        env,
		InheritEnv: routerInheritEnv(),
		Timeout:    RouteTimeout,
	})
	if err != nil {
		var ee *proc.ExitError
		if !errors.As(err, &ee) {
			if proc.IsNotFound(err) {
				return Route{}, cartridge.NewError(cartridge.KindNotInstalled, a.name, bin+" not found on PATH", err)
			}
			return Route{}, cartridge.NewError(cartridge.KindCrash, a.name, "starting dandelion route", err)
		}
		if ee.Cause != nil {
			return Route{}, cartridge.AsError(a.name, err)
		}
		switch ee.Result.ExitCode {
		case exitNoRoute:
			return Route{}, a.noRoute("dandelion route found no account with quota left")
		case exitRoutesFault:
			// Not retryable and not the quota's fault: a human has to fix
			// routes.json. KindNotInstalled is the kind for "the CLI is
			// there but cannot be used as configured" that neither retries
			// nor retires the cartridge (see D-30).
			reason := firstLine(ee.Result.Stderr)
			if reason == "" {
				reason = "exit status 2"
			}
			return Route{}, cartridge.NewError(cartridge.KindNotInstalled, a.name, "dandelion cannot route: "+reason, nil)
		default:
			return Route{}, cartridge.NewError(cartridge.KindCrash, a.name, "dandelion route failed", err)
		}
	}
	line := strings.TrimSpace(res.Stdout)
	if line == NoRoute {
		return Route{}, a.noRoute("dandelion route found no account with quota left")
	}
	if strings.ContainsAny(line, "\r\n") {
		return Route{}, cartridge.NewError(cartridge.KindCrash, a.name, fmt.Sprintf("dandelion route printed more than one line: %q", line), nil)
	}
	r, perr := ParseRoute(line)
	if perr != nil {
		return Route{}, cartridge.NewError(cartridge.KindCrash, a.name, "dandelion route printed an unreadable line", perr)
	}
	return r, nil
}

// noRoute is the error for "nothing to fire on right now": a rate limit,
// so the supervisor pauses the cartridge for NoRouteRetryAfter and tries
// again rather than retiring it.
func (a *Adapter) noRoute(msg string) *cartridge.Error {
	e := cartridge.NewError(cartridge.KindRateLimited, a.name, msg, nil)
	e.RetryAfter = NoRouteRetryAfter
	return e
}

// routerInheritEnv is every variable in sc's environment except the
// markers a parent Claude Code session sets: the router probes the user's
// CLIs the way the user's shell would (each provider has its own token,
// home and endpoint variables), so it gets the user's environment rather
// than an agent's allowlist. DANDELION_DISABLE is set explicitly.
func routerInheritEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name == "" || name == disableEnv || claude.IsSessionMarker(name) {
			continue
		}
		out = append(out, name)
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
