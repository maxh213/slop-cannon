package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"slop-cannon/internal/dash"
	"slop-cannon/internal/domain"
)

// Flag names of `sc dash`.
const (
	FlagDashRun              = "run"
	FlagDashHost             = "host"
	FlagDashPort             = "port"
	FlagDashBasePath         = "base-path"
	FlagDashAllowedHost      = "allowed-host"
	FlagDashDandelion        = "dandelion"
	FlagDashDandelionRefresh = "dandelion-refresh"
)

func init() { register(newDashCommand) }

const dashLong = `Serve a read-only browser dashboard for a run that is already firing.

PATH may be the repository root or any directory inside it; without it the
--repo directory (the current one by default) is used. The most recent run
is shown unless --run names another. The dashboard follows the recorded event
log and refreshes durable state and run-lock liveness; it never claims the
lock, writes run state, reconciles beads, or offers run controls.

The page shows bead flow, live and recent agents, review rounds, pull requests
and merges, notices, and lifecycle activity. Prompts, diffs, agent output,
review transcripts, and raw notice prose are not served; notices retain their
time, kind, source, and a small category. Historical and resumed incarnations
are followed in one stream. A quiet, finished, or crashed run remains available
for as long as the dashboard process is running.

Dandelion quota collection is off by default. --dandelion enables immediate,
sequential dandelion --json probes and adds the quota panel. The probes keep
only bounded, redacted last-good data in memory; quota failures do not change
observer health. --dandelion-refresh takes a Go duration, defaults to 10m, and
must be at least 1m.

By default the server listens only on loopback at 127.0.0.1:8790 and serves at
/. Use --host and --port to change the listener, or --base-path /sc to serve
the same three read-only endpoints under a URL prefix. HTTP Host headers are
accepted only for loopback names and IPs unless --allowed-host names an exact
hostname. Repeat that flag for each additional host, such as a reverse-proxied
dash.demo.otwarte.xyz; wildcard hosts are not accepted.`

const dashExamples = `sc dash
sc dash ./repo-path
sc dash --run 20260924-214444-42c0
sc dash --host 127.0.0.1 --port 8790 --base-path /sc
sc dash --allowed-host dash.demo.otwarte.xyz --base-path /sc
sc dash --dandelion --dandelion-refresh 15m`

// newDashCommand builds `sc dash [PATH]`.
func newDashCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "dash [PATH]",
		Short:   "Serve a read-only browser dashboard for a run",
		Long:    dashLong,
		Example: indent(dashExamples, "  "),
		Args:    cobra.MaximumNArgs(1),
		RunE:    runDash,
	}
	f := cmd.Flags()
	f.String(FlagDashRun, "", "show run `ID` instead of the most recent one")
	f.String(FlagDashHost, dash.DefaultHost, "listen on `HOST` or IP (loopback by default)")
	f.Int(FlagDashPort, dash.DefaultPort, "listen on `PORT` (0 chooses a free port)")
	f.String(FlagDashBasePath, dash.DefaultBasePath, "serve under URL `PATH` (for example /sc)")
	f.StringArray(FlagDashAllowedHost, nil, "allow exact `HOSTNAME` in the HTTP Host header (repeatable; loopback always allowed)")
	f.Bool(FlagDashDandelion, false, "collect and show Dandelion quota (disabled by default)")
	f.Duration(FlagDashDandelionRefresh, dash.DefaultDandelionRefresh, "probe Dandelion every `DURATION` (minimum 1m)")
	return cmd
}

// dashServe is a seam for command wiring tests; production serves the real
// read-only dashboard until its context ends.
var dashServe = dash.ListenAndServe

// runDash is the RunE of `sc dash`.
func runDash(cmd *cobra.Command, args []string) error {
	opts, err := GlobalOpts(cmd)
	if err != nil {
		return err
	}
	path := opts.Repo
	if len(args) == 1 {
		path = args[0]
	}
	repo, err := runRepoRoot(path)
	if err != nil {
		return err
	}
	f := cmd.Flags()
	runID, err := f.GetString(FlagDashRun)
	if err != nil {
		return err
	}
	host, err := f.GetString(FlagDashHost)
	if err != nil {
		return err
	}
	host = strings.TrimSpace(host)
	port, err := f.GetInt(FlagDashPort)
	if err != nil {
		return err
	}
	if err := dash.ValidateHostPort(host, port); err != nil {
		return err
	}
	basePath, err := f.GetString(FlagDashBasePath)
	if err != nil {
		return err
	}
	basePath, err = dash.NormalizeBasePath(basePath)
	if err != nil {
		return err
	}
	allowedHosts, err := f.GetStringArray(FlagDashAllowedHost)
	if err != nil {
		return err
	}
	allowedHosts, err = dash.NormalizeAllowedHosts(allowedHosts)
	if err != nil {
		return err
	}
	dandelionEnabled, err := f.GetBool(FlagDashDandelion)
	if err != nil {
		return err
	}
	dandelionRefresh, err := f.GetDuration(FlagDashDandelionRefresh)
	if err != nil {
		return err
	}
	if err := dash.ValidateDandelionRefresh(dandelionRefresh); err != nil {
		return err
	}
	id, dir, err := logsRunDir(repo, domain.RunID(runID))
	if err != nil {
		return err
	}
	return dashServe(cmd.Context(), dash.Options{
		Run: domain.Run{ID: id, RepoPath: repo}, RunDir: dir,
		Host: host, Port: port, BasePath: basePath, AllowedHosts: allowedHosts, Out: cmd.OutOrStdout(),
		Dandelion: dandelionEnabled, DandelionRefresh: dandelionRefresh,
	})
}
