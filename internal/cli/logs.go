package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/logging"
)

// `sc logs` (P8-05). The run directory holds one log per agent beside the
// orchestrator's own (D-23, internal/logging): <run-dir>/sc.log and
// <run-dir>/agents/<agent-id>.log. This command finds the right one and
// tails it, so a user never has to know that layout — and, because a bead
// is what a user actually remembers, `sc logs <bead-id>` tails every agent
// that worked on that bead.
//
// It reads files and nothing else: no bd, no git, no orchestrator. That is
// deliberate, and it is the same reason `sc status` is safe while another
// `sc` holds the run lock — a reader of a run must never be able to
// disturb it.
//
// Every line is scrubbed before it is printed ([statusScrub]): an agent log
// is a verbatim copy of what a cartridge wrote, so it may contain anything
// a model chose to emit, escape sequences included.

// Flag names of `sc logs`.
const (
	FlagLogsRun    = "run"
	FlagLogsLines  = "lines"
	FlagLogsFollow = "follow"
)

// DefaultLogLines is how many lines of each log `sc logs` prints when
// --lines is not given. It is a screenful rather than tail(1)'s ten,
// because one agent line is one cartridge event and the interesting part is
// usually a few exchanges back.
const DefaultLogLines = 50

// logsPollInterval is how often --follow looks for new bytes and for agent
// logs that did not exist yet. It is a variable so tests do not have to
// wait for it.
var logsPollInterval = 200 * time.Millisecond

func init() { register(newLogsCommand) }

const logsLong = `Tail the logs of a run.

Without arguments it prints the orchestrator's own log (sc.log). With an
agent id it prints that agent's log; with a bead id it prints the log of
every agent that has worked on that bead, oldest agent first, each under a
header naming it.

With --follow it keeps printing as the logs grow, and waits for a log that
is not there yet: follow a bead the moment the run adopts it and the output
starts when its first agent spawns. Without --follow a bead that has no
agent log yet is an error, because a one-shot read has nothing to print.

Logs are read straight from the run directory, so this works on a finished
run, on a crashed one, and while another sc process is still firing —
nothing here writes anything. --run picks a run other than the most recent
one; sc status lists what a run has done.`

const logsExamples = `sc logs
sc logs eng-sc-1-r1
sc logs sc-1
sc logs --repo ./repo-path --lines 200
sc logs sc-1 --follow`

// newLogsCommand builds `sc logs [AGENT-ID|BEAD-ID]`.
func newLogsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "logs [AGENT-ID|BEAD-ID]",
		Short:   "Tail the log of a run, an agent, or a bead",
		Long:    logsLong,
		Example: indent(logsExamples, "  "),
		Args:    cobra.MaximumNArgs(1),
		RunE:    runLogs,
	}
	f := cmd.Flags()
	f.String(FlagLogsRun, "", "read run `ID` instead of the most recent one")
	f.IntP(FlagLogsLines, "n", DefaultLogLines, "print the last `N` lines of each log (0 for all of it)")
	f.BoolP(FlagLogsFollow, "f", false, "keep printing as the logs grow, waiting for a log that is not there yet (until interrupted)")
	return cmd
}

// LogsReport is the --json shape of `sc logs`.
type LogsReport struct {
	// Repo is the repository the run belongs to.
	Repo string `json:"repo"`
	// Run is the run whose logs were read.
	Run domain.RunID `json:"run"`
	// Dir is its run directory (D-23).
	Dir string `json:"run_dir"`
	// Logs are the files that matched, in print order.
	Logs []LogFile `json:"logs"`
}

// LogFile is one log in a [LogsReport].
type LogFile struct {
	// Name is how the log is announced: "sc.log" for the orchestrator's
	// own, otherwise the agent id taken from the file name.
	Name string `json:"name"`
	// Path is the file on disk.
	Path string `json:"path"`
	// Agent is the agent the log belongs to, empty for the run log.
	Agent domain.AgentID `json:"agent,omitempty"`
	// Lines are the tail that was printed, scrubbed, oldest first.
	Lines []string `json:"lines,omitempty"`
	// Total is how many lines the whole file holds.
	Total int `json:"total_lines"`
}

// logSource is one log `sc logs` is going to print.
type logSource struct {
	name  string
	path  string
	agent domain.AgentID
	// started is when the log's first record was written, which is what
	// orders a bead's agents ([logsByAge]). It is the zero time for the run
	// log, which is never one of several.
	started time.Time
	// offset is how far the tail has already been printed, for --follow.
	offset int64
}

// runLogs is the RunE of `sc logs`.
func runLogs(cmd *cobra.Command, args []string) error {
	opts, err := GlobalOpts(cmd)
	if err != nil {
		return err
	}
	repo, err := runRepoRoot(opts.Repo)
	if err != nil {
		return err
	}
	f := cmd.Flags()
	runID, err := f.GetString(FlagLogsRun)
	if err != nil {
		return err
	}
	lines, err := f.GetInt(FlagLogsLines)
	if err != nil {
		return err
	}
	follow, err := f.GetBool(FlagLogsFollow)
	if err != nil {
		return err
	}
	if follow && opts.JSON {
		return fmt.Errorf("--%s cannot be combined with --%s: a followed log has no end to encode",
			FlagLogsFollow, FlagJSON)
	}
	id, dir, err := logsRunDir(repo, domain.RunID(runID))
	if err != nil {
		return err
	}
	arg := ""
	if len(args) == 1 {
		arg = strings.TrimSpace(args[0])
	}
	srcs, err := logsResolve(dir, arg)
	if err != nil {
		if !follow {
			return err
		}
		// P8-38: --follow is a subscription, not a read. A bead whose first
		// agent has not spawned yet — the normal case when a user starts
		// following a bead the moment the run adopts it — has no log to
		// resolve, and failing there made the user retry by hand for
		// something the follow loop already handles for every later agent.
		// The one-shot form still fails fast: there is nothing to wait for
		// when the command is going to return immediately.
		fmt.Fprintln(cmd.ErrOrStderr(), logsWaitingNotice(arg, err))
		srcs = nil
	}

	out := cmd.OutOrStdout()
	if opts.JSON {
		rep := LogsReport{Repo: repo, Run: id, Dir: dir}
		for _, s := range srcs {
			tail, total, _, err := logsTail(s.path, lines)
			if err != nil {
				return err
			}
			rep.Logs = append(rep.Logs, LogFile{
				Name: s.name, Path: s.path, Agent: s.agent, Lines: tail, Total: total,
			})
		}
		return writeJSON(out, rep)
	}

	headers := len(srcs) > 1
	last := ""
	for i := range srcs {
		tail, _, off, err := logsTail(srcs[i].path, lines)
		if err != nil {
			return err
		}
		srcs[i].offset = off
		if err := logsWrite(out, &last, srcs[i].name, headers, tail); err != nil {
			return err
		}
	}
	if !follow {
		return nil
	}
	return logsFollow(cmd.Context(), out, dir, arg, srcs, &last)
}

// logsRunDir resolves which run to read: the named one, or the most recent
// run recorded for the repository. A repository with no runs is an error
// here — unlike `sc status`, which reports the absence — because there is
// nothing to tail.
func logsRunDir(repo string, runID domain.RunID) (domain.RunID, string, error) {
	id := runID
	if id == "" {
		latest, found, err := config.LatestRunID(repo)
		if err != nil {
			return "", "", err
		}
		if !found {
			return "", "", fmt.Errorf("no runs recorded for %s", repo)
		}
		id = latest
	}
	dir := config.RunDir(repo, string(id))
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", "", fmt.Errorf("run %s: no run directory at %s", id, dir)
	}
	return id, dir, nil
}

// logsResolve turns the command's argument into the logs to print:
//
//	""                 the orchestrator's own log, <run-dir>/sc.log
//	an agent id        that agent's log, <run-dir>/agents/<id>.log
//	a bead id          every agent log whose id mentions that bead
//
// The last form is what makes the command usable: an agent id is minted
// from the bead, the lens and the round (eng-sc-1-r1, tester-qa-sc-1-r2),
// so the beads a user remembers are exactly the substrings that select
// them. Matching is on whole '-'-separated segments, so sc-1 never selects
// sc-12's agents.
func logsResolve(runDir, arg string) ([]logSource, error) {
	paths := logging.PathsFor(runDir)
	if arg == "" {
		if _, err := os.Stat(paths.Log); err != nil {
			return nil, fmt.Errorf("no %s in %s (the run wrote no log yet)", logging.RunLogName, runDir)
		}
		return []logSource{{name: logging.RunLogName, path: paths.Log}}, nil
	}
	// An exact agent id first: it costs one stat and is unambiguous.
	exact := paths.AgentLog(domain.AgentID(arg))
	if _, err := os.Stat(exact); err == nil {
		return []logSource{{name: arg, path: exact, agent: domain.AgentID(arg)}}, nil
	}
	names, err := logsAgentNames(paths.AgentsDir)
	if err != nil {
		return nil, err
	}
	want := logging.SafeFileName(arg)
	var out []logSource
	for _, name := range names {
		if logsMentions(name, want) {
			path := filepath.Join(paths.AgentsDir, name+logging.AgentLogExt)
			out = append(out, logSource{
				name:    name,
				path:    path,
				agent:   domain.AgentID(name),
				started: logsStartedAt(path),
			})
		}
	}
	if len(out) == 0 {
		return nil, logsUnknown(runDir, arg, names)
	}
	logsByAge(out)
	return out, nil
}

// logsByAge orders a bead's agent logs oldest first, which is what `sc
// logs <bead-id>` promises: a bead is read as a story, and the agent that
// went first should print first.
//
// Sorting by name is not that story. The ids are minted from the bead, the
// lens and the round (eng-sc-1-r1, tester-qa-sc-1-r1), so name order groups
// the engineer's rounds together ahead of the testers that judged the first
// one — on a real run, eng-e2e-4-r2 printed before tester-qa-e2e-4-r1,
// which ran three seconds earlier (P8-05). The tie-break is the name, so
// two logs opened in the same instant still print in a fixed order.
func logsByAge(srcs []logSource) {
	sort.SliceStable(srcs, func(i, j int) bool {
		a, b := srcs[i], srcs[j]
		if !a.started.Equal(b.started) {
			return a.started.Before(b.started)
		}
		return a.name < b.name
	})
}

// logsStartedAt is when an agent's log was first written: the timestamp of
// its first record, which is the moment the agent was spawned.
//
// A log file is one JSON object per line ([logging.NewFileHandler]) with a
// "time" key, so the first line answers it exactly. Anything that does not
// parse — an empty file the agent has not written to yet, a truncated first
// line, a format that changes one day — falls back to the file's modification
// time, and then to the zero time, which sorts first: an unreadable log is
// never worth failing the command for.
func logsStartedAt(path string) time.Time {
	f, err := os.Open(path) //nolint:gosec // a path derived from the run directory
	if err == nil {
		defer func() { _ = f.Close() }()
		line, rerr := bufio.NewReader(f).ReadString('\n')
		if rerr == nil || errors.Is(rerr, io.EOF) {
			var rec struct {
				Time time.Time `json:"time"`
			}
			if json.Unmarshal([]byte(strings.TrimRight(line, "\r\n")), &rec) == nil && !rec.Time.IsZero() {
				return rec.Time
			}
		}
	}
	if info, serr := os.Stat(path); serr == nil {
		return info.ModTime()
	}
	return time.Time{}
}

// logsAgentNames lists the agent ids a run directory holds a log for, in
// name order — which is the order the "this run has:" listing wants
// ([logsUnknown]), the logs themselves being ordered by age ([logsByAge]).
// A run that has not spawned an agent yet has no directory at all, which is
// not an error: logsUnknown says so better than a stat failure would.
func logsAgentNames(agentsDir string) ([]string, error) {
	entries, err := os.ReadDir(agentsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), logging.AgentLogExt) {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), logging.AgentLogExt))
	}
	sort.Strings(names)
	return names, nil
}

// logsUnknown explains that nothing matched, listing what the run does have.
func logsUnknown(runDir, arg string, names []string) error {
	if len(names) == 0 {
		return fmt.Errorf("no log for %q in %s: the run has no agent logs yet", arg, runDir)
	}
	shown := names
	more := ""
	if len(shown) > 12 {
		more = fmt.Sprintf(" (and %d more)", len(shown)-12)
		shown = shown[:12]
	}
	return fmt.Errorf("no log for %q in %s; this run has: %s%s",
		arg, runDir, strings.Join(shown, ", "), more)
}

// logsWaitingNotice is the line `--follow` prints when there is nothing to
// tail yet (P8-38). It says what is being waited for and, through the
// resolver's own error, what the run does have — so a user who mistyped a
// bead id sees the list of agent ids rather than an empty screen that never
// fills. It goes to stderr, so `sc logs <bead> --follow | grep …` still
// pipes only log lines.
func logsWaitingNotice(arg string, err error) string {
	what := logging.RunLogName
	if arg != "" {
		what = "the first agent log of " + strconv.Quote(arg)
	}
	return fmt.Sprintf("waiting for %s: %v", what, err)
}

// logsMentions reports whether the agent id name refers to id: name is id,
// or contains it as a run of whole '-'-separated segments. It is how a bead
// id selects the agents that worked on it.
func logsMentions(name, id string) bool {
	if id == "" {
		return false
	}
	for i := 0; i+len(id) <= len(name); i++ {
		if name[i:i+len(id)] != id {
			continue
		}
		if i > 0 && name[i-1] != '-' {
			continue
		}
		if j := i + len(id); j < len(name) && name[j] != '-' {
			continue
		}
		return true
	}
	return false
}

// logsTail reads the last n lines of path (n <= 0 means all of them) and
// returns them scrubbed, how many lines the file holds in total, and the
// byte offset the read stopped at (what --follow carries on from).
//
// The file is streamed through a ring buffer rather than slurped: an agent
// log is one line per cartridge output line for the life of the agent
// (P5-08), and printing its last screenful must not cost its whole size in
// memory. A log that does not exist yet is not an error — an agent is
// spawned before it writes — it is simply empty.
func logsTail(path string, n int) (lines []string, total int, offset int64, err error) {
	f, err := os.Open(path) //nolint:gosec // a path derived from the run directory
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, 0, nil
	}
	if err != nil {
		return nil, 0, 0, err
	}
	defer func() { _ = f.Close() }()

	r := bufio.NewReader(f)
	var ring []string
	for {
		line, rerr := r.ReadString('\n')
		// Only whole lines count. A last line without its newline is one
		// the run is still writing (D-15 appends to a live log), so it is
		// left for the next read rather than printed in half.
		if strings.HasSuffix(line, "\n") {
			offset += int64(len(line))
			total++
			text := statusScrub(strings.TrimRight(line, "\r\n"))
			switch {
			case n <= 0, len(ring) < n:
				ring = append(ring, text)
			default:
				ring = append(ring[1:], text)
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return nil, 0, 0, fmt.Errorf("read %s: %w", path, rerr)
		}
	}
	return ring, total, offset, nil
}

// logsWrite prints one log's lines, preceded by a header naming it when
// more than one log is being printed and the previous lines came from a
// different one. last is the name the header was last written for, so a
// followed log does not repeat its header on every poll.
func logsWrite(w io.Writer, last *string, name string, headers bool, lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	if headers && *last != name {
		sep := "\n"
		if *last == "" {
			sep = ""
		}
		if _, err := fmt.Fprintf(w, "%s==> %s <==\n", sep, name); err != nil {
			return err
		}
	}
	*last = name
	for _, line := range lines {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

// logsFollow prints new lines as they are written, until the context is
// cancelled (Ctrl-C, or SIGTERM: see Execute). It re-resolves the argument
// on every poll, so an agent spawned for the bead after the command started
// joins the output rather than being missed — which is the whole point of
// following a bead rather than an agent.
func logsFollow(ctx context.Context, w io.Writer, runDir, arg string, srcs []logSource, last *string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	seen := make(map[string]bool, len(srcs))
	for _, s := range srcs {
		seen[s.path] = true
	}
	ticker := time.NewTicker(logsPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		if fresh, err := logsResolve(runDir, arg); err == nil {
			for _, s := range fresh {
				if !seen[s.path] {
					seen[s.path] = true
					srcs = append(srcs, s)
				}
			}
		}
		// Headers only make sense once there is more than one log; a
		// second agent appearing mid-follow starts announcing them.
		headers := len(srcs) > 1
		for i := range srcs {
			lines, off, err := logsSince(srcs[i].path, srcs[i].offset)
			if err != nil {
				return err
			}
			srcs[i].offset = off
			if err := logsWrite(w, last, srcs[i].name, headers, lines); err != nil {
				return err
			}
		}
	}
}

// logsSince reads the whole lines written to path after offset and returns
// them scrubbed with the new offset. A file that shrank was rotated or
// replaced, so reading starts again from its beginning.
func logsSince(path string, offset int64) ([]string, int64, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, offset, nil
	}
	if err != nil {
		return nil, offset, err
	}
	if info.Size() < offset {
		offset = 0
	}
	if info.Size() == offset {
		return nil, offset, nil
	}
	f, err := os.Open(path) //nolint:gosec // a path derived from the run directory
	if err != nil {
		return nil, offset, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	r := bufio.NewReader(f)
	var lines []string
	for {
		line, rerr := r.ReadString('\n')
		if strings.HasSuffix(line, "\n") {
			offset += int64(len(line))
			lines = append(lines, statusScrub(strings.TrimRight(line, "\r\n")))
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return lines, offset, nil
			}
			return lines, offset, fmt.Errorf("read %s: %w", path, rerr)
		}
	}
}
