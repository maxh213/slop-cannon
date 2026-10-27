package dash

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"slop-cannon/internal/proc"
)

const (
	dandelionCommand          = "dandelion"
	dandelionProbeTimeout     = 5 * time.Minute
	dandelionMaxStdoutBytes   = 1 << 20
	dandelionMaxStderrBytes   = 16 << 10
	maxDandelionProviders     = 32
	maxDandelionWindows       = 16
	maxDandelionCurrencyText  = 16
	maxDandelionResponseBytes = 1 << 20
	dandelionResponseHeadroom = 4 << 10

	dandelionErrorNotInstalled    = "not_installed"
	dandelionErrorTimedOut        = "timed_out"
	dandelionErrorProbeFailed     = "probe_failed"
	dandelionErrorInvalidSnapshot = "invalid_snapshot"
)

var errDandelionInvalidSnapshot = errors.New("dash: invalid dandelion snapshot")

// ValidateDandelionRefresh checks the command-line collection interval.
func ValidateDandelionRefresh(refresh time.Duration) error {
	if refresh < MinDandelionRefresh {
		return fmt.Errorf("dash: dandelion refresh must be at least %s", MinDandelionRefresh)
	}
	return nil
}

type dandelionProbeResult struct {
	snapshot *DandelionSnapshot
	error    string
}

type dandelionRunFunc func(context.Context, proc.Spec) (proc.Result, error)

type dandelionEvent struct {
	started bool
	at      time.Time
	result  dandelionProbeResult
}

type dandelionRawSnapshot struct {
	GeneratedAt *string                 `json:"generatedAt"`
	Providers   *[]dandelionRawProvider `json:"providers"`
	Route       json.RawMessage         `json:"route"`
	RouteHigh   json.RawMessage         `json:"routeHigh"`
}

type dandelionRawProvider struct {
	ID          *string               `json:"id"`
	DisplayName *string               `json:"displayName"`
	Status      *string               `json:"status"`
	PlanLabel   *string               `json:"planLabel"`
	Eligible    json.RawMessage       `json:"eligible"`
	Reason      *string               `json:"reason"`
	FetchedAt   *string               `json:"fetchedAt"`
	Balance     json.RawMessage       `json:"balance"`
	Windows     *[]dandelionRawWindow `json:"windows"`
}

type dandelionRawBalance struct {
	Amount   *float64 `json:"amount"`
	Currency *string  `json:"currency"`
}

type dandelionRawWindow struct {
	Label           *string         `json:"label"`
	Kind            *string         `json:"kind"`
	UsedPct         *float64        `json:"usedPct"`
	ResetsAt        json.RawMessage `json:"resetsAt"`
	ProjectedFullAt json.RawMessage `json:"projectedFullAt"`
}

func probeDandelionSnapshot(ctx context.Context, refresh time.Duration) dandelionProbeResult {
	return runDandelionProbe(ctx, refresh, proc.Run)
}

func runDandelionProbe(ctx context.Context, refresh time.Duration, run dandelionRunFunc) dandelionProbeResult {
	seconds := int64(refresh / time.Second)
	result, err := run(ctx, proc.Spec{
		Command:         dandelionCommand,
		Args:            []string{"--json", "--max-age", strconv.FormatInt(seconds, 10)},
		InheritEnv:      dandelionInheritEnv(),
		Timeout:         dandelionProbeTimeout,
		ReapDescendants: true,
		StdoutTail:      dandelionMaxStdoutBytes + 1,
		StderrTail:      dandelionMaxStderrBytes,
	})
	if result.TimedOut || errors.Is(err, proc.ErrTimeout) {
		return dandelionProbeResult{error: dandelionErrorTimedOut}
	}
	if err != nil {
		if proc.IsNotFound(err) {
			return dandelionProbeResult{error: dandelionErrorNotInstalled}
		}
		return dandelionProbeResult{error: dandelionErrorProbeFailed}
	}
	snapshot, err := parseDandelionSnapshot(result.Stdout)
	if err != nil {
		return dandelionProbeResult{error: dandelionErrorInvalidSnapshot}
	}
	return dandelionProbeResult{snapshot: snapshot}
}

func dandelionInheritEnv() []string {
	names := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func parseDandelionSnapshot(output string) (*DandelionSnapshot, error) {
	if len(output) > dandelionMaxStdoutBytes {
		return nil, errDandelionInvalidSnapshot
	}
	decoder := json.NewDecoder(strings.NewReader(output))
	var raw dandelionRawSnapshot
	if err := decoder.Decode(&raw); err != nil {
		return nil, errDandelionInvalidSnapshot
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errDandelionInvalidSnapshot
	}

	generatedAt, err := requiredDandelionTime(raw.GeneratedAt)
	if err != nil || raw.Providers == nil {
		return nil, errDandelionInvalidSnapshot
	}
	route, err := dandelionRoute(raw.Route)
	if err != nil {
		return nil, errDandelionInvalidSnapshot
	}
	routeHigh, err := dandelionRoute(raw.RouteHigh)
	if err != nil {
		return nil, errDandelionInvalidSnapshot
	}

	providers := *raw.Providers
	count := min(len(providers), maxDandelionProviders)
	snapshot := &DandelionSnapshot{
		GeneratedAt: generatedAt, Route: route, RouteHigh: routeHigh,
		Providers:        make([]DandelionProviderSnapshot, 0, count),
		ProvidersOmitted: len(providers) - count,
	}
	for _, provider := range providers[:count] {
		projected, err := projectDandelionProvider(provider)
		if err != nil {
			return nil, errDandelionInvalidSnapshot
		}
		snapshot.Providers = append(snapshot.Providers, projected)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil || len(encoded) > maxDandelionResponseBytes-dandelionResponseHeadroom {
		return nil, errDandelionInvalidSnapshot
	}
	return snapshot, nil
}

func projectDandelionProvider(raw dandelionRawProvider) (DandelionProviderSnapshot, error) {
	if raw.ID == nil || raw.DisplayName == nil || raw.Status == nil || raw.FetchedAt == nil || raw.Windows == nil {
		return DandelionProviderSnapshot{}, errDandelionInvalidSnapshot
	}
	status := *raw.Status
	if status != "ok" && status != "unavailable" && status != "error" {
		return DandelionProviderSnapshot{}, errDandelionInvalidSnapshot
	}
	if status != "ok" && raw.Reason == nil {
		return DandelionProviderSnapshot{}, errDandelionInvalidSnapshot
	}
	fetchedAt, err := requiredDandelionTime(raw.FetchedAt)
	if err != nil {
		return DandelionProviderSnapshot{}, errDandelionInvalidSnapshot
	}
	eligible := true
	if len(raw.Eligible) != 0 {
		if dandelionNull(raw.Eligible) || json.Unmarshal(raw.Eligible, &eligible) != nil {
			return DandelionProviderSnapshot{}, errDandelionInvalidSnapshot
		}
	}
	planLabel, reason := "", ""
	if raw.PlanLabel != nil {
		planLabel = boundedText(*raw.PlanLabel, maxTitleText)
	}
	if raw.Reason != nil {
		reason = boundedText(*raw.Reason, maxActivityText)
	}

	windows := *raw.Windows
	count := min(len(windows), maxDandelionWindows)
	provider := DandelionProviderSnapshot{
		ID: boundedText(*raw.ID, maxIDText), DisplayName: boundedText(*raw.DisplayName, maxTitleText),
		Status: status, PlanLabel: planLabel, Eligible: eligible, Reason: reason,
		FetchedAt: *fetchedAt, Balance: projectDandelionBalance(raw.Balance),
		Windows: make([]DandelionWindowSnapshot, 0, count), WindowsOmitted: len(windows) - count,
	}
	for _, window := range windows[:count] {
		projected, err := projectDandelionWindow(window)
		if err != nil {
			return DandelionProviderSnapshot{}, errDandelionInvalidSnapshot
		}
		provider.Windows = append(provider.Windows, projected)
	}
	return provider, nil
}

func projectDandelionBalance(raw json.RawMessage) *DandelionBalanceSnapshot {
	if len(raw) == 0 || dandelionNull(raw) {
		return nil
	}
	var balance dandelionRawBalance
	if json.Unmarshal(raw, &balance) != nil || balance.Amount == nil || balance.Currency == nil {
		return nil
	}
	amount := *balance.Amount
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 {
		return nil
	}
	currency := cleanDandelionCurrency(*balance.Currency)
	if currency == "" {
		return nil
	}
	return &DandelionBalanceSnapshot{Amount: amount, Currency: currency}
}

func cleanDandelionCurrency(input string) string {
	currency := strings.TrimSpace(input)
	if currency == "$" {
		return "USD"
	}
	if currency == "" || len(currency) > maxDandelionCurrencyText {
		return ""
	}
	for _, r := range currency {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == ' ', r == '-', r == '_', r == '.':
		default:
			return ""
		}
	}
	return currency
}

func projectDandelionWindow(raw dandelionRawWindow) (DandelionWindowSnapshot, error) {
	if raw.Label == nil || raw.Kind == nil || raw.UsedPct == nil || math.IsNaN(*raw.UsedPct) || math.IsInf(*raw.UsedPct, 0) {
		return DandelionWindowSnapshot{}, errDandelionInvalidSnapshot
	}
	kind := *raw.Kind
	if kind != "rolling" && kind != "weekly" && kind != "other" {
		return DandelionWindowSnapshot{}, errDandelionInvalidSnapshot
	}
	resetsAt, err := optionalDandelionTime(raw.ResetsAt)
	if err != nil {
		return DandelionWindowSnapshot{}, errDandelionInvalidSnapshot
	}
	projectedFullAt, err := optionalDandelionTime(raw.ProjectedFullAt)
	if err != nil {
		return DandelionWindowSnapshot{}, errDandelionInvalidSnapshot
	}
	return DandelionWindowSnapshot{
		Label: boundedText(*raw.Label, maxTitleText), Kind: kind, UsedPct: *raw.UsedPct,
		ResetsAt: resetsAt, ProjectedFullAt: projectedFullAt,
	}, nil
}

func dandelionRoute(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", errDandelionInvalidSnapshot
	}
	if dandelionNull(raw) {
		return "", nil
	}
	var route string
	if err := json.Unmarshal(raw, &route); err != nil {
		return "", errDandelionInvalidSnapshot
	}
	return boundedText(route, maxActivityText), nil
}

func requiredDandelionTime(raw *string) (*time.Time, error) {
	if raw == nil {
		return nil, errDandelionInvalidSnapshot
	}
	parsed, err := time.Parse(time.RFC3339Nano, *raw)
	if err != nil {
		return nil, errDandelionInvalidSnapshot
	}
	parsed = parsed.UTC()
	return &parsed, nil
}

func optionalDandelionTime(raw json.RawMessage) (*time.Time, error) {
	if len(raw) == 0 || dandelionNull(raw) {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil, errDandelionInvalidSnapshot
	}
	return requiredDandelionTime(&text)
}

func dandelionNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func (o *Observer) startDandelionCollection(ctx context.Context) (<-chan dandelionEvent, <-chan struct{}) {
	events := make(chan dandelionEvent, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		o.collectDandelion(ctx, events)
	}()
	return events, done
}

func (o *Observer) collectDandelion(ctx context.Context, events chan<- dandelionEvent) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if !sendDandelionEvent(ctx, events, dandelionEvent{started: true, at: o.now().UTC()}) {
			return
		}
		result := o.probeDandelion(ctx, o.opts.DandelionRefresh)
		if !sendDandelionEvent(ctx, events, dandelionEvent{at: o.now().UTC(), result: result}) {
			return
		}
		timer.Reset(o.opts.DandelionRefresh)
	}
}

func sendDandelionEvent(ctx context.Context, events chan<- dandelionEvent, event dandelionEvent) bool {
	select {
	case events <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

func (o *Observer) applyDandelionEvent(event dandelionEvent) {
	if !o.opts.Dandelion {
		return
	}
	if event.started {
		at := event.at.UTC()
		o.dandelion.Probing = true
		o.dandelion.LastAttemptAt = &at
		return
	}
	o.dandelion.Probing = false
	if event.result.error != "" || event.result.snapshot == nil {
		o.dandelion.Error = safeDandelionError(event.result.error)
		return
	}
	at := event.at.UTC()
	lastAttempt := cloneDandelionTime(o.dandelion.LastAttemptAt)
	o.dandelion = cloneDandelionSnapshot(*event.result.snapshot)
	o.dandelion.LastAttemptAt = lastAttempt
	o.dandelion.LastSuccessAt = &at
}

func safeDandelionError(code string) string {
	switch code {
	case dandelionErrorNotInstalled, dandelionErrorTimedOut, dandelionErrorProbeFailed, dandelionErrorInvalidSnapshot:
		return code
	default:
		return dandelionErrorProbeFailed
	}
}

func (o *Observer) projectDandelion(now time.Time) *DandelionSnapshot {
	projected := cloneDandelionSnapshot(o.dandelion)
	if projected.Providers == nil {
		projected.Providers = make([]DandelionProviderSnapshot, 0)
	}
	projected.Stale = projected.LastSuccessAt == nil || projected.Error != "" ||
		now.Sub(*projected.LastSuccessAt) >= o.opts.DandelionRefresh
	return &projected
}

func cloneDandelionSnapshot(snapshot DandelionSnapshot) DandelionSnapshot {
	snapshot.LastAttemptAt = cloneDandelionTime(snapshot.LastAttemptAt)
	snapshot.LastSuccessAt = cloneDandelionTime(snapshot.LastSuccessAt)
	snapshot.GeneratedAt = cloneDandelionTime(snapshot.GeneratedAt)
	snapshot.Providers = cloneSlice(snapshot.Providers)
	for i := range snapshot.Providers {
		snapshot.Providers[i].Balance = cloneDandelionBalance(snapshot.Providers[i].Balance)
		snapshot.Providers[i].Windows = cloneSlice(snapshot.Providers[i].Windows)
		for j := range snapshot.Providers[i].Windows {
			snapshot.Providers[i].Windows[j].ResetsAt = cloneDandelionTime(snapshot.Providers[i].Windows[j].ResetsAt)
			snapshot.Providers[i].Windows[j].ProjectedFullAt = cloneDandelionTime(snapshot.Providers[i].Windows[j].ProjectedFullAt)
		}
	}
	return snapshot
}

func cloneDandelionBalance(value *DandelionBalanceSnapshot) *DandelionBalanceSnapshot {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneDandelionTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
