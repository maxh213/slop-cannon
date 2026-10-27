package dash

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"slop-cannon/internal/domain"
)

const (
	launchLogName         = "sc.log"
	maxLaunchLogLineBytes = 64 << 10
	maxLaunchLogContexts  = 4096
	maxLaunchLogSessions  = 64
	launchLogReadBuffer   = 32 << 10
	// maxLaunchLogBytes bounds each complete read. Two reads per consistency
	// attempt and at most two attempts bound each collection cycle. Larger or
	// inconsistent files publish an empty, unknown history rather than stale routes.
	maxLaunchLogBytes        = 8 << 20
	maxLaunchLogReadAttempts = 2
	defaultLaunchLogFollow   = 2 * time.Second
	launchLogRunStarted      = "run started"
	launchLogAgentFired      = "agent fired"
	launchLogDandelionRoute  = "dandelion routed a firing"

	routeStatusRouted  = "routed"
	routeStatusPending = "pending"
	routeStatusNoRoute = "no_route"
	routeStatusUnknown = "unknown"
)

type launchLogEventKind uint8

const (
	launchLogEventNone launchLogEventKind = iota
	launchLogEventFired
	launchLogEventRouted
	launchLogEventDamaged
	launchLogEventHistory
	launchLogEventRunStarted
)

type launchLogEvent struct {
	kind      launchLogEventKind
	record    launchLogEventKind
	agent     string
	attempt   int
	cartridge string
	route     launchRoute
	at        time.Time
	history   *launchLogHistory
}

type launchRoute struct {
	account string
	model   string
	effort  string
	at      time.Time
	ordinal int
}

type launchEventStart struct {
	serial uint64
	at     time.Time
}

type agentLaunch struct {
	spawnAt  time.Time
	current  launchEventStart
	previous launchEventStart
	serial   uint64
}

type launchFailureKey struct {
	agent   string
	attempt int
	spawn   uint64
}

type launchAttemptKey struct {
	agent       string
	attempt     int
	incarnation uint64
}

type launchAttempt struct {
	cartridge string
	ambiguous bool
	routes    []launchRoute
}

type launchLogStart struct {
	incarnation uint64
	at          time.Time
}

type launchLogHistory struct {
	attempts    map[launchAttemptKey]*launchAttempt
	current     map[string]launchAttemptKey
	starts      []launchLogStart
	incarnation uint64
}

func (h *launchLogHistory) apply(event launchLogEvent) {
	switch event.kind {
	case launchLogEventHistory:
		if event.history == nil {
			*h = launchLogHistory{}
		} else {
			*h = *event.history
		}
	case launchLogEventRunStarted:
		h.startIncarnation(event.at)
	case launchLogEventFired:
		h.fire(event)
	case launchLogEventRouted:
		h.routeEvent(event)
	case launchLogEventDamaged:
		h.damage(event)
	}
}

func (h *launchLogHistory) init() {
	if h.attempts == nil {
		h.attempts = make(map[launchAttemptKey]*launchAttempt)
	}
	if h.current == nil {
		h.current = make(map[string]launchAttemptKey)
	}
}

func (h *launchLogHistory) startIncarnation(at time.Time) {
	h.init()
	h.incarnation++
	if len(h.starts) < maxLaunchLogContexts {
		h.starts = append(h.starts, launchLogStart{incarnation: h.incarnation, at: at})
	}
	h.current = make(map[string]launchAttemptKey)
}

func (h *launchLogHistory) fire(event launchLogEvent) {
	h.init()
	key := launchAttemptKey{agent: event.agent, attempt: event.attempt, incarnation: h.incarnation}
	attempt, exists := h.attempts[key]
	if !exists && len(h.attempts) < maxLaunchLogContexts {
		attempt = &launchAttempt{cartridge: event.cartridge}
		h.attempts[key] = attempt
	}
	if exists {
		attempt.ambiguous = true
	}
	if !h.setCurrent(key) && attempt != nil {
		attempt.ambiguous = true
	}
}

func (h *launchLogHistory) routeEvent(event launchLogEvent) {
	h.init()
	key, ok := h.current[event.agent]
	if !ok || key.attempt <= 0 {
		return
	}
	attempt := h.attempts[key]
	if attempt == nil || attempt.ambiguous {
		return
	}
	if len(attempt.routes) >= maxLaunchLogSessions {
		attempt.ambiguous = true
		attempt.routes = nil
		return
	}
	route := event.route
	route.ordinal = len(attempt.routes) + 1
	attempt.routes = append(attempt.routes, route)
}

func (h *launchLogHistory) damage(event launchLogEvent) {
	h.init()
	if event.agent != "" {
		key := launchAttemptKey{agent: event.agent, attempt: event.attempt, incarnation: h.incarnation}
		if event.record == launchLogEventFired && key.attempt > 0 {
			h.markAmbiguous(key)
			h.setCurrent(key)
			return
		}
		if event.record == launchLogEventRouted {
			if current, ok := h.current[event.agent]; ok && current.attempt > 0 {
				h.markAmbiguous(current)
				return
			}
		}
		h.damageAgent(event.agent)
		return
	}
	for key := range h.attempts {
		if key.incarnation == h.incarnation {
			h.attempts[key].ambiguous = true
		}
	}
	h.current = make(map[string]launchAttemptKey)
}

func (h *launchLogHistory) damageAgent(agent string) {
	for key, attempt := range h.attempts {
		if key.agent == agent && key.incarnation == h.incarnation {
			attempt.ambiguous = true
		}
	}
	h.setCurrent(launchAttemptKey{agent: agent, attempt: -1, incarnation: h.incarnation})
}

func (h *launchLogHistory) markAmbiguous(key launchAttemptKey) {
	attempt := h.attempts[key]
	if attempt == nil {
		if len(h.attempts) >= maxLaunchLogContexts {
			return
		}
		attempt = &launchAttempt{}
		h.attempts[key] = attempt
	}
	attempt.ambiguous = true
}

func (h *launchLogHistory) setCurrent(key launchAttemptKey) bool {
	if _, ok := h.current[key.agent]; !ok && len(h.current) >= maxLaunchLogContexts {
		return false
	}
	h.current[key.agent] = key
	return true
}

func (h *launchLogHistory) attempt(key launchAttemptKey) (*launchAttempt, bool) {
	attempt, ok := h.attempts[key]
	return attempt, ok && attempt != nil
}

func (h *launchLogHistory) resolveGeneration(launch agentLaunch) (uint64, bool) {
	current, previous := launch.current, launch.previous
	if current.serial == 0 || current.at.IsZero() || launch.spawnAt.IsZero() || current.at.After(launch.spawnAt) {
		return 0, false
	}
	if previous.serial != 0 && (previous.serial == current.serial || previous.at.IsZero() || !previous.at.Before(current.at)) {
		return 0, false
	}

	var selected launchLogStart
	ambiguous := false
	for _, start := range h.starts {
		if start.incarnation == 0 || start.at.IsZero() || start.at.After(current.at) {
			continue
		}
		if previous.serial != 0 {
			if !start.at.After(previous.at) {
				continue
			}
			if selected.incarnation != 0 {
				return 0, false
			}
			selected = start
			continue
		}
		if selected.incarnation == 0 || start.at.After(selected.at) {
			selected = start
			ambiguous = false
		} else if start.at.Equal(selected.at) {
			ambiguous = true
		}
	}
	if selected.incarnation == 0 || ambiguous {
		return 0, false
	}
	return selected.incarnation, true
}

func parseLaunchLogLine(line []byte, run domain.RunID) launchLogEvent {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return launchLogEvent{}
	}
	record, err := decodeLaunchLogObject(line)
	if err != nil {
		return launchLogEvent{kind: launchLogEventDamaged}
	}
	message, ok := launchLogString(record, "msg", false)
	if !ok {
		return launchLogEvent{}
	}
	var kind launchLogEventKind
	switch message {
	case launchLogRunStarted:
		kind = launchLogEventRunStarted
	case launchLogAgentFired:
		kind = launchLogEventFired
	case launchLogDandelionRoute:
		kind = launchLogEventRouted
	default:
		return launchLogEvent{}
	}

	agent, agentOK := launchLogString(record, "agent", false)
	damaged := func() launchLogEvent {
		return launchLogEvent{kind: launchLogEventDamaged, record: kind, agent: agent}
	}
	recordRun, runOK := launchLogString(record, "run", false)
	if runOK && recordRun != string(run) {
		return launchLogEvent{}
	}
	if kind == launchLogEventRunStarted {
		at, timeOK := launchLogTime(record, "time")
		if !runOK || !timeOK || !validRunStartedFields(record) {
			return damaged()
		}
		return launchLogEvent{kind: launchLogEventRunStarted, at: at}
	}
	if !runOK || !agentOK {
		return damaged()
	}
	if _, ok := launchLogTime(record, "time"); !ok {
		return damaged()
	}

	switch kind {
	case launchLogEventFired:
		cartridge, cartridgeOK := launchLogString(record, "cartridge", false)
		attempt, attemptOK := launchLogInt(record, "attempt")
		if !cartridgeOK || !attemptOK || attempt <= 0 {
			return damaged()
		}
		return launchLogEvent{kind: kind, agent: agent, attempt: attempt, cartridge: cartridge}
	case launchLogEventRouted:
		account, accountOK := launchLogString(record, "account", false)
		model, modelOK := launchLogString(record, "model", false)
		effort, effortOK := launchLogString(record, "effort", true)
		at, timeOK := launchLogTime(record, "time")
		if !accountOK || !modelOK || !effortOK || !timeOK || !validOptionalLaunchFields(record) {
			return damaged()
		}
		return launchLogEvent{kind: kind, agent: agent, route: launchRoute{
			account: account, model: model, effort: effort, at: at,
		}}
	default:
		return launchLogEvent{}
	}
}

func validOptionalLaunchFields(record map[string]json.RawMessage) bool {
	if raw, ok := record["cached"]; ok {
		var cached bool
		if launchLogNull(raw) || json.Unmarshal(raw, &cached) != nil {
			return false
		}
	}
	for _, key := range []string{"mode", "policy"} {
		if _, present := record[key]; present {
			if _, ok := launchLogString(record, key, true); !ok {
				return false
			}
		}
	}
	return true
}

func validRunStartedFields(record map[string]json.RawMessage) bool {
	if raw, ok := record["resumed"]; ok {
		var resumed bool
		if launchLogNull(raw) || json.Unmarshal(raw, &resumed) != nil {
			return false
		}
	}
	if _, present := record["mode"]; present {
		if _, ok := launchLogString(record, "mode", true); !ok {
			return false
		}
	}
	return true
}

func decodeLaunchLogObject(line []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(line))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, errors.New("dash: launch log record is not an object")
	}
	record := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("dash: launch log key is not a string")
		}
		if _, duplicate := record[key]; duplicate {
			return nil, errors.New("dash: duplicate launch log key")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		record[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("dash: trailing launch log data")
	}
	return record, nil
}

func launchLogString(record map[string]json.RawMessage, key string, emptyOK bool) (string, bool) {
	raw, ok := record[key]
	if !ok || launchLogNull(raw) {
		return "", false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || !validLaunchLogText(value, emptyOK) {
		return "", false
	}
	return value, true
}

func launchLogNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func validLaunchLogText(value string, emptyOK bool) bool {
	if !utf8.ValidString(value) || (!emptyOK && value == "") || utf8.RuneCountInString(value) > maxIDText {
		return false
	}
	return !strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

func launchLogInt(record map[string]json.RawMessage, key string) (int, bool) {
	raw, ok := record[key]
	if !ok {
		return 0, false
	}
	var value int
	if json.Unmarshal(raw, &value) != nil {
		return 0, false
	}
	return value, true
}

func launchLogTime(record map[string]json.RawMessage, key string) (time.Time, bool) {
	raw, ok := record[key]
	if !ok {
		return time.Time{}, false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

type launchLogCollectFunc func(context.Context, string, domain.RunID, time.Duration, chan<- launchLogEvent)

func collectLaunchLog(ctx context.Context, path string, run domain.RunID, poll time.Duration, out chan<- launchLogEvent) {
	if poll <= 0 {
		poll = defaultLaunchLogFollow
	}
	collector := launchLogCollector{path: filepath.Clean(path), runID: run, poll: poll, out: out, read: readLaunchLogFile}
	collector.run(ctx)
}

type launchLogReadStatus uint8

const (
	launchLogReadOK launchLogReadStatus = iota
	launchLogReadUnsupported
	launchLogReadMissing
	launchLogReadInvalid
	launchLogReadInconsistent
	launchLogReadCanceled
)

type launchLogReadResult struct {
	data      []byte
	status    launchLogReadStatus
	readBytes int64
}

type launchLogReadFunc func(context.Context, string) launchLogReadResult

type launchLogCollector struct {
	path  string
	runID domain.RunID
	poll  time.Duration
	out   chan<- launchLogEvent
	read  launchLogReadFunc

	published      []byte
	publishedKnown bool
	publishedAny   bool
}

func (c *launchLogCollector) run(ctx context.Context) {
	for {
		if !c.refresh(ctx) {
			return
		}
		if !waitLaunchLogPoll(ctx, c.poll) {
			return
		}
	}
}

func (c *launchLogCollector) refresh(ctx context.Context) bool {
	candidate, status := c.consistentRead(ctx)
	if status == launchLogReadCanceled {
		return false
	}
	if status != launchLogReadOK {
		return c.publishUnknown(ctx)
	}
	return c.publishCandidate(ctx, candidate)
}

func (c *launchLogCollector) consistentRead(ctx context.Context) ([]byte, launchLogReadStatus) {
	read := c.read
	if read == nil {
		read = readLaunchLogFile
	}
	for attempt := 0; attempt < maxLaunchLogReadAttempts; attempt++ {
		first := read(ctx, c.path)
		if first.status != launchLogReadOK {
			return nil, first.status
		}
		second := read(ctx, c.path)
		if second.status != launchLogReadOK {
			return nil, second.status
		}
		if bytes.Equal(first.data, second.data) || bytes.HasPrefix(second.data, first.data) {
			return first.data, launchLogReadOK
		}
	}
	return nil, launchLogReadInconsistent
}

func (c *launchLogCollector) publishUnknown(ctx context.Context) bool {
	if c.publishedAny && !c.publishedKnown {
		return true
	}
	if !sendLaunchLogEvent(ctx, c.out, launchLogEvent{kind: launchLogEventHistory, history: &launchLogHistory{}}) {
		return false
	}
	c.published = nil
	c.publishedKnown = false
	c.publishedAny = true
	return true
}

func (c *launchLogCollector) publishCandidate(ctx context.Context, candidate []byte) bool {
	if c.publishedAny && c.publishedKnown && bytes.Equal(c.published, candidate) {
		return true
	}
	history := parseLaunchLogHistory(candidate, c.runID)
	if !sendLaunchLogEvent(ctx, c.out, launchLogEvent{kind: launchLogEventHistory, history: history}) {
		return false
	}
	c.published = candidate
	c.publishedKnown = true
	c.publishedAny = true
	return true
}

func readLaunchLogFile(ctx context.Context, path string) launchLogReadResult {
	if ctx.Err() != nil {
		return launchLogReadResult{status: launchLogReadCanceled}
	}
	file, err := os.Open(path) //nolint:gosec // the selected run directory supplies this fixed path.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return launchLogReadResult{status: launchLogReadMissing}
		}
		return launchLogReadResult{status: launchLogReadInvalid}
	}
	defer func() { _ = file.Close() }()
	if info, err := file.Stat(); err == nil && info.Size() > maxLaunchLogBytes {
		return launchLogReadResult{status: launchLogReadUnsupported}
	}
	return readLaunchLogContent(ctx, file)
}

func readLaunchLogContent(ctx context.Context, source io.Reader) launchLogReadResult {
	result := launchLogReadResult{status: launchLogReadOK}
	reader := &launchLogContextReader{ctx: ctx, reader: source}
	buffer := make([]byte, launchLogReadBuffer)
	for {
		n, err := reader.Read(buffer)
		if ctx.Err() != nil {
			return launchLogReadResult{status: launchLogReadCanceled, readBytes: result.readBytes}
		}
		if n > 0 {
			result.readBytes += int64(n)
			if len(result.data)+n > maxLaunchLogBytes {
				result.data = nil
				result.status = launchLogReadUnsupported
				return result
			}
			result.data = append(result.data, buffer[:n]...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return result
			}
			result.data = nil
			result.status = launchLogReadInvalid
			return result
		}
	}
}

type launchLogContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *launchLogContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func parseLaunchLogHistory(data []byte, run domain.RunID) *launchLogHistory {
	history := &launchLogHistory{}
	for len(data) > 0 {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			break
		}
		line := data[:newline]
		data = data[newline+1:]
		if len(line) > maxLaunchLogLineBytes {
			history.apply(launchLogEvent{kind: launchLogEventDamaged})
			continue
		}
		history.apply(parseLaunchLogLine(line, run))
	}
	return history
}

func sendLaunchLogEvent(ctx context.Context, out chan<- launchLogEvent, event launchLogEvent) bool {
	select {
	case out <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

func waitLaunchLogPoll(ctx context.Context, poll time.Duration) bool {
	timer := time.NewTimer(poll)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (o *Observer) startLaunchLogCollection(ctx context.Context) (<-chan launchLogEvent, <-chan struct{}) {
	events := make(chan launchLogEvent, 256)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(events)
		o.collectLaunchLog(ctx, filepath.Join(o.opts.RunDir, launchLogName), o.opts.Run.ID, o.opts.LaunchLogInterval, events)
	}()
	return events, done
}
