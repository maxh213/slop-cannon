package beads

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"slop-cannon/internal/proc"
)

// Options configures a Client.
type Options struct {
	// Actor is the BEADS_ACTOR for every call ("sc:<orchestrator-id>",
	// D-21); empty means DefaultActor. WithActor derives per-agent clients.
	Actor string
	// Timeout bounds each bd call; zero means DefaultTimeout.
	Timeout time.Duration
	// Logger receives one Debug line per call; nil means slog.Default().
	Logger *slog.Logger
	// ReadOnly prepends `--readonly` to every call, so writes fail with
	// ErrReadOnly (the agent shim contract, P3-09).
	ReadOnly bool
}

// Client is the one writer of a repository's beads database (D-21). Every
// call runs `bd -C <repo> …` under the invocation contract (Spec) and is
// serialised through one mutex, so calls from any number of goroutines
// reach the embedded Dolt engine one at a time. Methods decode bd's JSON
// through Decode (preamble-tolerant) and turn failures into *Error values
// carrying the typed kinds in errors.go.
//
// Clients derived with WithActor share the mutex, so an agent's comment
// and the orchestrator's update never overlap either.
type Client struct {
	repo     string
	actor    string
	timeout  time.Duration
	logger   *slog.Logger
	readOnly bool

	mu   *sync.Mutex
	exec func(context.Context, proc.Spec) (proc.Result, error)
}

// New returns a Client for the beads database of repo (the repository
// root, or any directory inside it: bd resolves the workspace from -C).
func New(repo string, opts Options) *Client {
	c := &Client{
		repo:     repo,
		actor:    opts.Actor,
		timeout:  opts.Timeout,
		logger:   opts.Logger,
		readOnly: opts.ReadOnly,
		mu:       &sync.Mutex{},
		exec:     proc.Run,
	}
	if c.actor == "" {
		c.actor = DefaultActor
	}
	if c.timeout == 0 {
		c.timeout = DefaultTimeout
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	return c
}

// Repo returns the repository the client operates on.
func (c *Client) Repo() string { return c.repo }

// Actor returns the BEADS_ACTOR the client uses.
func (c *Client) Actor() string { return c.actor }

// WithActor returns a client identical to c but acting as actor (an
// agent id such as "sc:engineer-1"), sharing c's mutex so the two never
// run bd concurrently. An empty actor returns c itself.
func (c *Client) WithActor(actor string) *Client {
	if actor == "" || actor == c.actor {
		return c
	}
	d := *c
	d.actor = actor
	return &d
}

// options builds the CallOptions for one call.
func (c *Client) options(stdin io.Reader) CallOptions {
	return CallOptions{Repo: c.repo, Actor: c.actor, Timeout: c.timeout, Stdin: stdin, ReadOnly: c.readOnly}
}

// run executes `bd -C <repo> <args>` while holding the mutex and returns
// the result, with failures classified into *Error.
func (c *Client) run(ctx context.Context, args ...string) (proc.Result, error) {
	return c.runStdin(ctx, nil, args...)
}

// runStdin is run with standard input connected to stdin.
func (c *Client) runStdin(ctx context.Context, stdin io.Reader, args ...string) (proc.Result, error) {
	if err := ctx.Err(); err != nil {
		return proc.Result{}, err
	}
	spec := Spec(c.options(stdin), args...)
	c.mu.Lock()
	res, err := c.exec(ctx, spec)
	c.mu.Unlock()
	err = classify(args, res, err)
	attrs := []any{"command", commandName(args), "args", strings.Join(args, " "), "duration", res.Duration.Round(time.Millisecond)}
	if err != nil {
		attrs = append(attrs, "exit", res.ExitCode, "error", err.Error())
		c.logger.DebugContext(ctx, "bd call failed", attrs...)
		return res, err
	}
	c.logger.DebugContext(ctx, "bd call", attrs...)
	return res, nil
}

// call runs bd and decodes the JSON payload into v (see Decode).
func (c *Client) call(ctx context.Context, v any, args ...string) error {
	res, err := c.run(ctx, args...)
	if err != nil {
		return err
	}
	if v == nil {
		return nil
	}
	if err := Decode(res.Stdout, v); err != nil {
		return fmt.Errorf("bd %s: %w", commandName(args), err)
	}
	return nil
}

// Ping runs `bd ping --json`, the health check of the embedded database,
// through the client's queue.
func (c *Client) Ping(ctx context.Context) (PingResult, error) {
	var ping PingResult
	if err := c.call(ctx, &ping, "ping", "--json"); err != nil {
		return PingResult{}, err
	}
	if ping.Status != "ok" {
		return ping, &CheckError{Command: pingCommand, Reason: ReasonUnhealthy, Status: ping.Status}
	}
	return ping, nil
}

// Statuses runs `bd statuses --json`: the built-in statuses and the
// repository's custom ones.
func (c *Client) Statuses(ctx context.Context) (StatusList, error) {
	var list StatusList
	if err := c.call(ctx, &list, "statuses", "--json"); err != nil {
		return StatusList{}, err
	}
	return list, nil
}

// SetCustomStatuses writes statuses as the `status.custom` config value
// (`bd config set status.custom "name:category,…"`). It replaces the
// value, so callers union with the repository's existing statuses first
// (ConfigGet + ParseCustomStatuses + MergeCustomStatuses, D-07).
func (c *Client) SetCustomStatuses(ctx context.Context, statuses []CustomStatus) error {
	if len(statuses) == 0 {
		return fmt.Errorf("beads: no custom statuses to set")
	}
	for _, s := range statuses {
		if s.Name == "" || !s.Category.Valid() {
			return fmt.Errorf("beads: invalid custom status %q", s.String())
		}
	}
	return c.ConfigSet(ctx, StatusCustomKey, FormatCustomStatuses(statuses))
}

// ConfigSet runs `bd config set <key> <value> --json`.
func (c *Client) ConfigSet(ctx context.Context, key, value string) error {
	var got ConfigValue
	if err := c.call(ctx, &got, "config", "set", key, value, "--json"); err != nil {
		return err
	}
	if got.Key != "" && got.Key != key {
		return fmt.Errorf("bd config set: set %q, bd reported %q", key, got.Key)
	}
	return nil
}

// ConfigGet runs `bd config get <key> --json` and returns the value; an
// unset key yields "" and no error.
func (c *Client) ConfigGet(ctx context.Context, key string) (string, error) {
	var got ConfigValue
	if err := c.call(ctx, &got, "config", "get", key, "--json"); err != nil {
		return "", err
	}
	return got.Value, nil
}

// Export runs `bd export` and returns every issue of the JSONL snapshot
// (labels, dependencies in edge form and comments included), the
// whole-graph read P3-04 builds its model from. Records with a `_type`
// other than "issue" are skipped.
func (c *Client) Export(ctx context.Context) ([]Issue, error) {
	res, err := c.run(ctx, "export")
	if err != nil {
		return nil, err
	}
	return ParseExport(strings.NewReader(res.Stdout))
}

// ParseExport decodes `bd export` JSONL: one issue per line, non-issue
// records skipped, blank and non-JSON lines (a preamble) ignored.
func ParseExport(r io.Reader) ([]Issue, error) {
	var issues []Issue
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || text[0] != '{' {
			continue
		}
		var probe struct {
			Type string `json:"_type"`
		}
		if err := json.Unmarshal([]byte(text), &probe); err != nil {
			return nil, fmt.Errorf("bd export: line %d: %w", line, err)
		}
		if probe.Type != "" && probe.Type != "issue" {
			continue
		}
		var is Issue
		if err := json.Unmarshal([]byte(text), &is); err != nil {
			return nil, fmt.Errorf("bd export: line %d: %w", line, err)
		}
		issues = append(issues, is)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("bd export: %w", err)
	}
	return issues, nil
}

// errNoIssue is returned when bd answered a write with an empty array.
var errNoIssue = errors.New("bd returned no issue")
