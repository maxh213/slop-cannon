package dash

import (
	"io"
	"time"

	"slop-cannon/internal/domain"
)

const (
	// DefaultHost keeps the dashboard reachable only from this machine.
	DefaultHost = "127.0.0.1"
	// DefaultPort is the dashboard's default listening port.
	DefaultPort = 8790
	// DefaultBasePath serves the dashboard at the web server's root.
	DefaultBasePath = "/"
	// DefaultQuietAfter is how long a running run goes without an event before
	// the dashboard calls it quiet. Quiet is not a liveness or health verdict.
	DefaultQuietAfter = 2 * time.Minute
	// DefaultRefreshInterval is how often durable state, liveness and the
	// cached browser snapshot are refreshed.
	DefaultRefreshInterval = time.Second
	// DefaultDandelionRefresh is the default delay between Dandelion probes.
	DefaultDandelionRefresh = 10 * time.Minute
	// DefaultHostRefreshInterval is the host metric sampling interval.
	DefaultHostRefreshInterval = 2 * time.Second
	// MinDandelionRefresh is the shortest interval accepted by the CLI.
	MinDandelionRefresh = time.Minute
)

// maxLiveAgents is the fixed hard cap on returned live agents, independent of
// configurable history windows and of the run's configured ammo.
const maxLiveAgents = 128

// Limits bounds the configurable collection windows in a cached Snapshot.
// Live agents use the separate fixed maxLiveAgents cap.
type Limits struct {
	Beads        int
	RecentAgents int
	Reviews      int
	Deliveries   int
	Notices      int
	Activity     int
}

// DefaultLimits returns the production snapshot bounds.
func DefaultLimits() Limits {
	return Limits{
		Beads:        500,
		RecentAgents: 50,
		Reviews:      250,
		Deliveries:   250,
		Notices:      100,
		Activity:     150,
	}
}

// Options selects one existing run and configures its read-only observer and
// server. RunDir is resolved by the caller; HTTP requests cannot change it.
type Options struct {
	Run          domain.Run
	RunDir       string
	Host         string
	Port         int
	BasePath     string
	AllowedHosts []string
	Out          io.Writer

	RefreshInterval     time.Duration
	HostRefreshInterval time.Duration
	FollowPoll          time.Duration
	LaunchLogInterval   time.Duration
	QuietAfter          time.Duration
	Dandelion           bool
	DandelionRefresh    time.Duration
	Limits              Limits
	Now                 func() time.Time
}

// Health describes the dashboard observer, not the observed run. A quiet,
// finished or crashed run still has a healthy observer serving its snapshot.
type Health struct {
	Status      string    `json:"status"`
	Stream      string    `json:"stream"`
	Revision    uint64    `json:"revision"`
	LastEventAt time.Time `json:"last_event_at,omitempty"`
}

// LockSnapshot is the non-sensitive part of the run-lock and pid probes.
type LockSnapshot struct {
	State     string `json:"state"`
	Held      bool   `json:"held"`
	Mine      bool   `json:"mine"`
	Alive     bool   `json:"alive"`
	Running   bool   `json:"running"`
	PIDReused bool   `json:"pid_reused,omitempty"`
}

// RunSnapshot is the safe run metadata and lifecycle summary. It deliberately
// omits the brief, configuration documents, state directory and finish prose.
type RunSnapshot struct {
	ID              domain.RunID  `json:"id"`
	Mode            string        `json:"mode"`
	Epic            domain.BeadID `json:"epic,omitempty"`
	Phase           string        `json:"phase"`
	Status          string        `json:"status"`
	Resumed         bool          `json:"resumed"`
	Paused          bool          `json:"paused"`
	Stopping        bool          `json:"stopping"`
	Quiet           bool          `json:"quiet"`
	QuietForSeconds int64         `json:"quiet_for_seconds,omitempty"`
	Ammo            int           `json:"ammo"`
	Safety          bool          `json:"safety"`
	StartedAt       time.Time     `json:"started_at,omitempty"`
	FinishedAt      time.Time     `json:"finished_at,omitempty"`
	LastEventAt     time.Time     `json:"last_event_at,omitempty"`
	ElapsedMillis   int64         `json:"elapsed_ms"`
	FinishReason    string        `json:"finish_reason,omitempty"`
	Lock            LockSnapshot  `json:"lock"`
}

// Counts are totals from the full fold, not merely the returned windows.
type Counts struct {
	Events       int            `json:"events"`
	Beads        int            `json:"beads"`
	BeadsByState map[string]int `json:"beads_by_state"`
	Agents       int            `json:"agents"`
	LiveAgents   int            `json:"live_agents"`
	Reviews      int            `json:"reviews"`
	Deliveries   int            `json:"deliveries"`
	PullRequests int            `json:"pull_requests"`
	Merges       int            `json:"merges"`
	Notices      int            `json:"notices"`
	Activity     int            `json:"activity"`
}

// Omitted reports how many entries each bound left out of this snapshot.
type Omitted struct {
	Beads      int `json:"beads"`
	Agents     int `json:"agents"`
	Reviews    int `json:"reviews"`
	Deliveries int `json:"deliveries"`
	Notices    int `json:"notices"`
	Activity   int `json:"activity"`
}

// BeadSnapshot keeps identity and workflow metadata, not descriptions or
// acceptance text supplied to agent prompts.
type BeadSnapshot struct {
	ID          domain.BeadID   `json:"id"`
	Title       string          `json:"title"`
	State       string          `json:"state"`
	Type        string          `json:"type,omitempty"`
	Priority    domain.Priority `json:"priority"`
	Size        string          `json:"size,omitempty"`
	Round       int             `json:"round"`
	PRNumber    int             `json:"pr_number,omitempty"`
	PRURL       string          `json:"pr_url,omitempty"`
	Deps        []domain.BeadID `json:"deps"`
	DepsOmitted int             `json:"deps_omitted,omitempty"`
}

// AgentSnapshot contains lifecycle and exact-attempt route metadata only. Model
// remains requested configuration. Output lines, transcript state, raw launch
// records and agent failure prose never cross this boundary.
type AgentSnapshot struct {
	ID            domain.AgentID `json:"id"`
	Role          string         `json:"role"`
	Lens          string         `json:"lens,omitempty"`
	Cartridge     string         `json:"cartridge,omitempty"`
	Model         string         `json:"model,omitempty"`
	RouteStatus   string         `json:"route_status"`
	RoutedModel   string         `json:"routed_model,omitempty"`
	RoutedAccount string         `json:"routed_account,omitempty"`
	RoutedEffort  string         `json:"routed_effort,omitempty"`
	RoutedAt      *time.Time     `json:"routed_at,omitempty"`
	RouteAttempt  int            `json:"route_attempt,omitempty"`
	RouteOrdinal  int            `json:"route_ordinal,omitempty"`
	Bead          domain.BeadID  `json:"bead,omitempty"`
	BeadTitle     string         `json:"bead_title,omitempty"`
	Task          string         `json:"task,omitempty"`
	State         string         `json:"state"`
	Round         int            `json:"round,omitempty"`
	Attempt       int            `json:"attempt,omitempty"`
	StartedAt     time.Time      `json:"started_at,omitempty"`
	ElapsedMillis int64          `json:"elapsed_ms"`
	ExitCode      int            `json:"exit_code,omitempty"`
	Failure       string         `json:"failure,omitempty"`
	Produced      string         `json:"produced,omitempty"`
	Live          bool           `json:"live"`
	LastSeen      bool           `json:"last_seen,omitempty"`
}

// VerdictSnapshot summarises a verdict without its prose or feedback items.
type VerdictSnapshot struct {
	Lens      string `json:"lens,omitempty"`
	Outcome   string `json:"outcome"`
	ItemCount int    `json:"item_count"`
}

// ReviewSnapshot is one review round with counts instead of transcript data.
type ReviewSnapshot struct {
	Bead            domain.BeadID     `json:"bead"`
	Number          int               `json:"number"`
	Kind            string            `json:"kind"`
	Lenses          []string          `json:"lenses"`
	LensesOmitted   int               `json:"lenses_omitted,omitempty"`
	Commit          string            `json:"commit,omitempty"`
	Verdicts        []VerdictSnapshot `json:"verdicts"`
	VerdictsOmitted int               `json:"verdicts_omitted,omitempty"`
	ResponseCount   int               `json:"response_count"`
	RulingCount     int               `json:"ruling_count"`
	HasReport       bool              `json:"has_report"`
	Escalation      string            `json:"escalation,omitempty"`
}

// DeliverySnapshot is a pull request or direct-merge delivery.
type DeliverySnapshot struct {
	Bead     domain.BeadID `json:"bead"`
	URL      string        `json:"url,omitempty"`
	Number   int           `json:"number,omitempty"`
	Branch   string        `json:"branch,omitempty"`
	Base     string        `json:"base,omitempty"`
	External bool          `json:"external"`
	Merged   bool          `json:"merged"`
	SHA      string        `json:"sha,omitempty"`
	Pushed   bool          `json:"pushed"`
}

// NoticeSnapshot is the structured form of a run warning or error. Raw
// message and error prose never crosses the public snapshot boundary.
type NoticeSnapshot struct {
	At       time.Time `json:"at"`
	Kind     string    `json:"kind"`
	Source   string    `json:"source,omitempty"`
	Category string    `json:"category"`
}

// Activity is a safe lifecycle rendering. Text is built from whitelisted
// metadata rather than tui.LogEntry.Text, which can contain agent output.
type Activity struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	Text string    `json:"text"`
}

// DandelionSnapshot is the safe, bounded projection of the last good
// Dandelion response together with the independent probe lifecycle.
type DandelionSnapshot struct {
	Probing          bool                        `json:"probing"`
	LastAttemptAt    *time.Time                  `json:"last_attempt_at,omitempty"`
	LastSuccessAt    *time.Time                  `json:"last_success_at,omitempty"`
	Stale            bool                        `json:"stale"`
	Error            string                      `json:"error,omitempty"`
	GeneratedAt      *time.Time                  `json:"generated_at"`
	Route            string                      `json:"route"`
	RouteHigh        string                      `json:"route_high"`
	Providers        []DandelionProviderSnapshot `json:"providers"`
	ProvidersOmitted int                         `json:"providers_omitted"`
}

// DandelionBalanceSnapshot is an optional provider balance. It carries no
// usage window, reset time, percentage, or routing/binding interpretation.
type DandelionBalanceSnapshot struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

// DandelionProviderSnapshot exposes only provider fields used by the dashboard.
type DandelionProviderSnapshot struct {
	ID             string                    `json:"id"`
	DisplayName    string                    `json:"display_name"`
	Status         string                    `json:"status"`
	PlanLabel      string                    `json:"plan_label"`
	Eligible       bool                      `json:"eligible"`
	Reason         string                    `json:"reason"`
	FetchedAt      time.Time                 `json:"fetched_at"`
	Balance        *DandelionBalanceSnapshot `json:"balance,omitempty"`
	Windows        []DandelionWindowSnapshot `json:"windows"`
	WindowsOmitted int                       `json:"windows_omitted"`
}

// DandelionWindowSnapshot is one bounded quota usage window.
type DandelionWindowSnapshot struct {
	Label           string     `json:"label"`
	Kind            string     `json:"kind"`
	UsedPct         float64    `json:"used_pct"`
	ResetsAt        *time.Time `json:"resets_at"`
	ProjectedFullAt *time.Time `json:"projected_full_at"`
}

// HostSnapshot is the last complete host metric sample. CPU remains absent
// until two samples with matching topology yield valid counter deltas.
type HostSnapshot struct {
	Name        string             `json:"name"`
	CollectedAt time.Time          `json:"collected_at"`
	CPU         *HostCPUSnapshot   `json:"cpu,omitempty"`
	Memory      HostUsageSnapshot  `json:"memory"`
	Swap        *HostUsageSnapshot `json:"swap,omitempty"`
	Disk        HostUsageSnapshot  `json:"disk"`
}

// HostCPUSnapshot contains aggregate and bounded per-core usage percentages.
type HostCPUSnapshot struct {
	UsedPct      float64            `json:"used_pct"`
	Cores        []HostCoreSnapshot `json:"cores"`
	CoresOmitted int                `json:"cores_omitted"`
}

// HostCoreSnapshot is one logical CPU's usage since the preceding sample.
type HostCoreSnapshot struct {
	ID      int     `json:"id"`
	UsedPct float64 `json:"used_pct"`
}

// HostUsageSnapshot is a byte count pair for memory, swap or root disk usage.
type HostUsageSnapshot struct {
	Used  uint64 `json:"used"`
	Total uint64 `json:"total"`
}

// Snapshot is the immutable-by-convention value cached behind the observer's
// mutex. Snapshot returns a deep copy so callers cannot mutate the cache.
type Snapshot struct {
	Revision          uint64             `json:"revision"`
	AsOf              time.Time          `json:"as_of"`
	Observer          Health             `json:"observer"`
	Run               RunSnapshot        `json:"run"`
	Counts            Counts             `json:"counts"`
	Omitted           Omitted            `json:"omitted"`
	Beads             []BeadSnapshot     `json:"beads"`
	LiveAgents        []AgentSnapshot    `json:"live_agents"`
	LiveAgentsOmitted int                `json:"live_agents_omitted"`
	RecentAgents      []AgentSnapshot    `json:"recent_agents"`
	Reviews           []ReviewSnapshot   `json:"reviews"`
	Deliveries        []DeliverySnapshot `json:"deliveries"`
	Notices           []NoticeSnapshot   `json:"notices"`
	Activity          []Activity         `json:"activity"`
	Host              *HostSnapshot      `json:"host,omitempty"`
	Dandelion         *DandelionSnapshot `json:"dandelion,omitempty"`
}
