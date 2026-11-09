package capture

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"math/big"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/export/destination"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/secrets"
)

const (
	queueSize          = 64
	projectMinuteBytes = 4 << 20
	maxProjects        = 4096
	maxCollectors      = 256
	cacheRefresh       = 5 * time.Second
	cacheTimeout       = 3 * time.Second
	cacheTTL           = 15 * time.Second
)

type Manager struct {
	pool         *pgxpool.Pool
	keys         *secrets.KeyRing
	installation string
	sender       *destination.Sender
	log          *slog.Logger

	cache      atomic.Pointer[snapshot]
	budget     budget
	queue      chan queued
	collectors atomic.Int64

	queued          atomic.Int64
	delivered       atomic.Int64
	dropped         atomic.Int64
	failed          atomic.Int64
	redactionFailed atomic.Int64
}

func NewManager(pool *pgxpool.Pool, keys *secrets.KeyRing, installation string, sender *destination.Sender, log *slog.Logger) *Manager {
	return &Manager{
		pool: pool, keys: keys, installation: installation, sender: sender, log: log,
		budget: budget{windows: map[string]window{}},
		queue:  make(chan queued, queueSize),
	}
}

type snapshot struct {
	policies []active
	at       time.Time
}

type active struct {
	Policy
	ratio     *big.Rat
	redaction redaction
}

type redaction interface {
	engineFor(project string) (*guardrails.Engine, bool)
}

type unredacted struct{}

type pinned struct {
	engine  *guardrails.Engine
	project string
}

type unavailable struct{}

func (unredacted) engineFor(string) (*guardrails.Engine, bool) { return nil, true }

func (p pinned) engineFor(project string) (*guardrails.Engine, bool) {
	return p.engine, p.project == project
}

func (unavailable) engineFor(string) (*guardrails.Engine, bool) { return nil, false }

func (m *Manager) Resolve(project, route, key, endUser, requestID string) *Collector {
	cached := m.cache.Load()
	if cached == nil || time.Since(cached.at) > cacheTTL {
		return nil
	}
	i := slices.IndexFunc(cached.policies, func(p active) bool { return p.matches(project, route, key, endUser) })
	if i < 0 || !sampled(cached.policies[i].ratio, cached.policies[i].ID, requestID) {
		return nil
	}
	policy := cached.policies[i]
	engine, ok := policy.redaction.engineFor(project)
	if !ok {
		m.drop(true)
		return nil
	}
	if m.collectors.Add(1) > maxCollectors {
		m.collectors.Add(-1)
		m.dropped.Add(1)
		return nil
	}
	return &Collector{manager: m, policyID: policy.ID, etag: policy.ETag, requestID: requestID, maxBytes: policy.MaxBytes, include: policy.Include, redactor: engine}
}

func (m *Manager) drop(redactionFailed bool) {
	m.dropped.Add(1)
	if redactionFailed {
		m.redactionFailed.Add(1)
	}
}

func (m *Manager) Run(ctx context.Context) {
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
		case <-time.After(cacheRefresh):
		}
	}
}

func (m *Manager) reload(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, cacheTimeout)
	defer cancel()
	policies, err := m.load(ctx)
	if err != nil {
		m.cache.Store(nil)
		m.log.Warn("capture policy load failed", "code", "database", "error", err)
		return
	}
	m.cache.Store(&snapshot{policies: policies, at: time.Now()})
	m.budget.prune(time.Now())
}

type loadedPolicy struct {
	policy  Policy
	project *string
}

func (m *Manager) load(ctx context.Context) ([]active, error) {
	rows, err := m.pool.Query(ctx, loadPoliciesSQL)
	if err != nil {
		return nil, err
	}
	loaded, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (loadedPolicy, error) {
		var raw []byte
		var l loadedPolicy
		if err := row.Scan(&raw, &l.project); err != nil {
			return l, err
		}
		return l, json.Unmarshal(raw, &l.policy)
	})
	if err != nil {
		return nil, err
	}
	definitions, definitionsErr := loadDefinitions(ctx, m.pool)
	policies := make([]active, len(loaded))
	for i, l := range loaded {
		ratio, ok := new(big.Rat).SetString(l.policy.SampleRatio)
		if !ok {
			return nil, fmt.Errorf("capture policy %s has an unreadable sample ratio", l.policy.ID)
		}
		policies[i] = active{Policy: l.policy, ratio: ratio, redaction: compiled(ctx, l.policy, valueOr(l.project, ""), definitions, definitionsErr)}
	}
	return policies, nil
}

func compiled(ctx context.Context, p Policy, project string, definitions map[string]guardrails.Revision, definitionsErr error) redaction {
	if ctx.Err() != nil || definitionsErr != nil && len(p.Redact) > 0 {
		return unavailable{}
	}
	engine, err := compile(project, p.Redact, p.RedactorRevisions, definitions)
	switch {
	case err != nil:
		return unavailable{}
	case engine == nil:
		return unredacted{}
	}
	return pinned{engine: engine, project: project}
}

type queued struct {
	policyID, etag string
	requestID      string
	projectID      string
	at             time.Time
	record         []byte
}

func (m *Manager) enqueue(item queued) bool {
	if len(item.record) > maxRecord || !m.budget.admit(item.projectID, len(item.record), item.at, func() bool {
		select {
		case m.queue <- item:
			return true
		default:
			return false
		}
	}) {
		m.dropped.Add(1)
		return false
	}
	m.queued.Add(1)
	return true
}

type window struct {
	start time.Time
	bytes int
}

type budget struct {
	mu      sync.Mutex
	windows map[string]window
}

func (b *budget) admit(project string, size int, now time.Time, push func() bool) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	current := b.current(project, now)
	if current.bytes+size > projectMinuteBytes || !b.tracks(project, now) || !push() {
		return false
	}
	b.windows[project] = window{start: current.start, bytes: current.bytes + size}
	return true
}

func (b *budget) current(project string, now time.Time) window {
	w := b.windows[project]
	if now.Sub(w.start) > time.Minute {
		return window{start: now}
	}
	return w
}

func (b *budget) tracks(project string, now time.Time) bool {
	if _, known := b.windows[project]; known || len(b.windows) < maxProjects {
		return true
	}
	b.pruneLocked(now)
	return len(b.windows) < maxProjects
}

func (b *budget) prune(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked(now)
}

func (b *budget) pruneLocked(now time.Time) {
	maps.DeleteFunc(b.windows, func(_ string, w window) bool { return now.Sub(w.start) > time.Minute })
}
