package export

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
)

const (
	captureQueueSize          = 64
	captureMaxRecord          = 1 << 20
	captureProjectMinuteBytes = 4 << 20
	captureMaxProjects        = 4096
	captureMaxCollectors      = 256
	captureFieldMax           = 65536
	captureCacheRefresh       = 5 * time.Second
	captureCacheTimeout       = 3 * time.Second
	captureCacheTTL           = 15 * time.Second
	captureSendTimeout        = 5 * time.Second
	captureDrainDeadline      = 10 * time.Second
	captureLoadLimit          = 1001
)

type Manager struct {
	Pool         *pgxpool.Pool
	Keys         *secrets.KeyRing
	Installation string
	Sender       *Sender
	Log          *slog.Logger

	mu         sync.Mutex
	policies   []CapturePolicy
	loadedAt   time.Time
	loaded     bool
	queue      chan queuedCapture
	collectors int
	minute     map[string]minuteCharge

	queued          atomic.Int64
	delivered       atomic.Int64
	dropped         atomic.Int64
	failed          atomic.Int64
	redactionFailed atomic.Int64
}

func (m *Manager) Metrics() (queued, delivered, dropped, failed, redactionFailed int64) {
	return m.queued.Load(), m.delivered.Load(), m.dropped.Load(), m.failed.Load(), m.redactionFailed.Load()
}

type minuteCharge struct {
	window time.Time
	bytes  int
}

type queuedCapture struct {
	policyID, etag string
	requestID      string
	projectID      string
	queuedAt       time.Time
	record         []byte
}

func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	if m.queue == nil {
		m.queue = make(chan queuedCapture, captureQueueSize)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	wg.Go(func() { m.reloadLoop(ctx) })
	wg.Go(func() { m.deliverLoop(ctx) })
	wg.Wait()
}

func (m *Manager) reloadLoop(ctx context.Context) {
	for {
		m.reload(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(captureCacheRefresh):
		}
	}
}

func (m *Manager) reload(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, captureCacheTimeout)
	defer cancel()
	rows, err := m.Pool.Query(ctx, loadCapturePoliciesSQL)
	if err != nil {
		m.mu.Lock()
		m.policies, m.loaded = nil, false
		m.mu.Unlock()
		m.Log.Warn("capture policy load failed", "code", "database")
		return
	}
	var policies []CapturePolicy
	truncated := false
	for rows.Next() {
		var data []byte
		var policy CapturePolicy
		if err := rows.Scan(&data); err != nil || json.Unmarshal(data, &policy) != nil {
			rows.Close()
			m.mu.Lock()
			m.policies, m.loaded = nil, false
			m.mu.Unlock()
			m.Log.Warn("capture policy decode failed")
			return
		}
		policies = append(policies, policy)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		m.mu.Lock()
		m.policies, m.loaded = nil, false
		m.mu.Unlock()
		return
	}
	if len(policies) > 1000 {
		truncated = true
		policies = policies[:1000]
	}
	var scoped, broad []CapturePolicy
	for _, p := range policies {
		if p.Route != nil {
			scoped = append(scoped, p)
		} else {
			broad = append(broad, p)
		}
	}
	m.mu.Lock()
	m.policies = append(scoped, broad...)
	m.loadedAt, m.loaded = time.Now(), true
	m.pruneMinuteLocked(time.Now())
	m.mu.Unlock()
	if truncated {
		m.Log.Warn("capture policies truncated at 1000")
	}
}

func (m *Manager) Resolve(project, route, key, endUser, requestID string) *Collector {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loaded || time.Since(m.loadedAt) > captureCacheTTL {
		return nil
	}
	var policy *CapturePolicy
	for i := range m.policies {
		if m.policies[i].Matches(project, route, key, endUser) {
			policy = &m.policies[i]
			break
		}
	}
	if policy == nil || !policy.Sample(requestID) {
		return nil
	}
	if m.collectors >= captureMaxCollectors {
		m.dropped.Add(1)
		return nil
	}
	m.collectors++
	return &Collector{manager: m, policyID: policy.ID, etag: policy.ETag, requestID: requestID, maxBytes: policy.MaxBytes, include: policy.Include}
}

func (m *Manager) release() {
	m.mu.Lock()
	if m.collectors > 0 {
		m.collectors--
	}
	m.mu.Unlock()
}

func (c *Collector) enqueue(project string, record []byte, queuedAt time.Time) bool {
	m := c.manager
	if len(record) > captureMaxRecord {
		m.dropped.Add(1)
		return false
	}
	m.mu.Lock()
	if m.queue == nil {
		m.queue = make(chan queuedCapture, captureQueueSize)
	}
	if m.minute == nil {
		m.minute = map[string]minuteCharge{}
	}
	if !m.minuteFitsLocked(project, len(record), queuedAt) {
		m.mu.Unlock()
		m.dropped.Add(1)
		return false
	}
	select {
	case m.queue <- queuedCapture{policyID: c.policyID, etag: c.etag, requestID: c.requestID, projectID: project, queuedAt: queuedAt, record: record}:
		m.minuteChargeLocked(project, len(record), queuedAt)
		m.mu.Unlock()
		m.queued.Add(1)
		return true
	default:
		m.mu.Unlock()
		m.dropped.Add(1)
		return false
	}
}

func (m *Manager) pruneMinuteLocked(now time.Time) {
	for project, charge := range m.minute {
		if now.Sub(charge.window) > time.Minute {
			delete(m.minute, project)
		}
	}
}

func (m *Manager) minuteFitsLocked(project string, bytes int, now time.Time) bool {
	charge, known := m.minute[project]
	if now.Sub(charge.window) > time.Minute {
		charge = minuteCharge{window: now}
	}
	if charge.bytes+bytes > captureProjectMinuteBytes {
		return false
	}
	if !known {
		if len(m.minute) >= captureMaxProjects {
			m.pruneMinuteLocked(now)
		}
		if _, ok := m.minute[project]; !ok && len(m.minute) >= captureMaxProjects {
			return false
		}
	}
	return true
}

func (m *Manager) minuteChargeLocked(project string, bytes int, now time.Time) {
	charge := m.minute[project]
	if now.Sub(charge.window) > time.Minute {
		charge = minuteCharge{window: now}
	}
	m.minute[project] = minuteCharge{window: charge.window, bytes: charge.bytes + bytes}
}

func (m *Manager) deliverLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			m.drain()
			return
		case c := <-m.queue:
			m.deliver(ctx, c)
		}
	}
}

func (m *Manager) drain() {
	deadline := time.Now().Add(captureDrainDeadline)
	for {
		select {
		case c := <-m.queue:
			if time.Now().After(deadline) {
				m.dropped.Add(1)
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), min(captureSendTimeout, time.Until(deadline)))
			m.deliver(ctx, c)
			cancel()
		default:
			return
		}
	}
}

func (m *Manager) deliver(ctx context.Context, c queuedCapture) {
	tx, err := m.Pool.Begin(ctx)
	if err != nil {
		m.failed.Add(1)
		return
	}
	defer tx.Rollback(ctx)
	var destination Destination
	var credentialID *string
	err = tx.QueryRow(ctx, readCaptureDestinationSQL, c.policyID, c.etag).Scan(&destination.Type, &destination.URL, &credentialID, &destination.Format)
	if err != nil {
		m.audit(ctx, tx, c.requestID, "capture.dropped", "success", c.projectID)
		_ = tx.Commit(ctx)
		m.dropped.Add(1)
		return
	}
	var credential []byte
	if credentialID != nil {
		if credential, err = access.SecretValueFor(ctx, tx, m.Keys, secrets.SinkCredential, m.Installation, *credentialID); err != nil {
			m.failed.Add(1)
			return
		}
	}
	if err = tx.Commit(ctx); err != nil {
		m.failed.Add(1)
		return
	}
	sendCtx, cancel := context.WithTimeout(ctx, captureSendTimeout)
	err = m.Sender.Send(sendCtx, destination, credential, Record{ID: c.requestID, Stream: "captures", At: c.queuedAt, Data: json.RawMessage(c.record)})
	cancel()
	atx, aerr := m.Pool.Begin(ctx)
	if aerr == nil {
		defer atx.Rollback(ctx)
		action, outcome := "capture.failed", "failure"
		if err == nil {
			action, outcome = "capture.delivered", "success"
		}
		m.audit(ctx, atx, c.requestID, action, outcome, c.projectID)
		_ = atx.Commit(ctx)
	}
	if err != nil {
		m.failed.Add(1)
		m.Log.Warn("capture delivery failed", "request_id", c.requestID, "code", DeliveryErrorCode(err))
		return
	}
	m.delivered.Add(1)
}

func (m *Manager) audit(ctx context.Context, tx pgx.Tx, requestID, action, outcome, projectID string) {
	var project any
	if projectID != "" {
		project = projectID
	}
	if _, err := tx.Exec(ctx, captureOutcomeAuditSQL, access.NewID(), action, requestID, outcome, project); err != nil {
		m.Log.Warn("capture audit failed", "request_id", requestID, "code", "database")
	}
}

type Collector struct {
	manager   *Manager
	policyID  string
	etag      string
	requestID string
	maxBytes  int
	include   []string

	mu         sync.Mutex
	released   bool
	finished   bool
	oversized  bool
	input      map[string]json.RawMessage
	output     strings.Builder
	outputSize int
	toolCalls  []json.RawMessage
	toolSize   int
}

func (c *Collector) includes(field string) bool {
	for _, i := range c.include {
		if i == field {
			return true
		}
	}
	return false
}

func (c *Collector) CaptureDocument(document map[string]json.RawMessage) {
	if c == nil || !c.includes("input") {
		return
	}
	for _, field := range captureInputFields {
		if len(document[field]) > captureFieldMax {
			c.mu.Lock()
			c.oversized = true
			c.mu.Unlock()
			return
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.released || c.finished {
		return
	}
	c.input = CaptureInput(document)
}

func (c *Collector) CaptureText(field, text string) {
	if c == nil || !c.includes(field) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.released || c.finished {
		return
	}
	if field != "output" || c.outputSize+len(text) > captureFieldMax {
		c.oversized = c.oversized || field == "output"
		return
	}
	c.output.WriteString(text)
	c.outputSize += len(text)
}

func (c *Collector) CaptureToolCall(raw json.RawMessage) {
	if c == nil || !c.includes("tool_calls") {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.released || c.finished {
		return
	}
	if len(raw) > captureFieldMax || c.toolSize+len(raw) > captureFieldMax {
		c.oversized = true
		return
	}
	c.toolCalls = append(c.toolCalls, slices.Clone(raw))
	c.toolSize += len(raw)
}

type CaptureIdentity struct {
	Route         string
	ProjectID     string
	KeyID         string
	EndUserDigest string
	TraceID       string
	SpanID        string
	Outcome       string
	Mode          string
}

func (c *Collector) Finish(identity CaptureIdentity, queuedAt time.Time) bool {
	if c == nil {
		return false
	}
	defer c.Close()
	c.mu.Lock()
	if c.finished {
		c.mu.Unlock()
		return false
	}
	c.finished = true
	oversized := c.oversized
	input, output, tools := c.input, c.output.String(), c.toolCalls
	c.mu.Unlock()
	if oversized {
		c.manager.dropped.Add(1)
		return false
	}
	data := map[string]any{"request_id": c.requestID}
	for key, value := range map[string]string{
		"route": identity.Route, "project_id": identity.ProjectID, "key_id": identity.KeyID,
		"end_user_digest": identity.EndUserDigest, "mode": identity.Mode,
	} {
		if value != "" {
			data[key] = value
		}
	}
	if !zeroHex(identity.TraceID) {
		data["trace_id"] = identity.TraceID
	}
	if !zeroHex(identity.SpanID) {
		data["span_id"] = identity.SpanID
	}
	hasContent := false
	if c.includes("input") && len(input) > 0 {
		data["input"] = input
		hasContent = true
	}
	if c.includes("output") && output != "" {
		data["output"] = output
		hasContent = true
	}
	if c.includes("tool_calls") && len(tools) > 0 {
		data["tool_calls"] = tools
		hasContent = true
	}
	if !hasContent {
		c.manager.dropped.Add(1)
		return false
	}
	payload, err := json.Marshal(data)
	if err != nil {
		c.manager.dropped.Add(1)
		return false
	}
	record, err := json.Marshal(map[string]any{
		"version": 1, "event_id": access.NewID(), "stream": "captures",
		"source_id": c.requestID, "project_id": identity.ProjectID,
		"route": identity.Route, "outcome": identity.Outcome,
		"occurred_at": queuedAt.UTC().Format(time.RFC3339Nano), "data": json.RawMessage(payload),
	})
	if err != nil || len(record) > c.maxBytes {
		c.manager.dropped.Add(1)
		return false
	}
	return c.enqueue(identity.ProjectID, record, queuedAt)
}

func (c *Collector) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	released := c.released
	c.released = true
	c.input = nil
	c.toolCalls = nil
	c.output.Reset()
	c.mu.Unlock()
	if !released {
		c.manager.release()
	}
}

func zeroHex(id string) bool {
	if len(id) != 16 && len(id) != 32 {
		return true
	}
	for _, c := range id {
		if c != '0' {
			return false
		}
	}
	return true
}
