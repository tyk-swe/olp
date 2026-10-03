//go:build bench

package bench_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// resourceUse is what the gateway process consumed while a run was applied,
// read from /proc and from its own metrics.
type resourceUse struct {
	// RSS figures are in mebibytes. Peak is the largest RSS the kernel
	// recorded during the run, which is VmHWM after it was reset at the start;
	// PeakSampled is the largest RSS seen by sampling, a cross-check that does
	// not depend on the reset being allowed.
	RSSBeforeMiB      float64 `json:"rss_before_mib"`
	RSSPeakMiB        float64 `json:"rss_peak_mib"`
	RSSPeakSampledMiB float64 `json:"rss_peak_sampled_mib"`
	RSSAfterMiB       float64 `json:"rss_after_mib"`
	// PeakResetOK is false when the kernel refused to reset VmHWM, in which
	// case RSSPeakMiB is the sampled peak instead, as VmHWM covers the
	// process's whole life.
	PeakResetOK bool `json:"peak_reset_ok"`
	// Threads is the peak operating-system thread count. The number of
	// goroutines is not observable from outside the process.
	ThreadsPeak int64 `json:"threads_peak"`
	// PeakInferenceAdmitted is the most inference requests the gateway held at
	// once, from its admission gauge, and Capacity what it was configured to
	// admit.
	PeakInferenceAdmitted float64 `json:"peak_inference_admitted"`
	AdmissionCapacity     float64 `json:"admission_capacity"`
	// PeakOpenCircuits is read from a metrics snapshot the gateway refreshes
	// every fifteen seconds, so a short circuit episode can be missed.
	PeakOpenCircuits float64 `json:"peak_open_circuits"`
	// Rejections is the number of requests the gateway's admission pool shed
	// during the run.
	Rejections float64 `json:"admission_rejections"`

	// WallSeconds is the span the CPU figures below cover, and
	// GatewayIdleCores what the gateway used with no traffic, measured just
	// before the run. It spends CPU on its own background work, which at a low
	// rate would otherwise be charged to the requests.
	WallSeconds      float64 `json:"wall_seconds"`
	GatewayIdleCores float64 `json:"gateway_idle_cores"`
	// CPU time of each process over the run, in seconds. Cores is the average
	// number of CPUs it kept busy.
	Gateway cpuUse `json:"gateway_cpu"`
	Mock    cpuUse `json:"mock_cpu"`
	// Loadgen is the test process, which runs the load generator.
	Loadgen cpuUse `json:"loadgen_cpu"`
}

type cpuUse struct {
	Seconds float64 `json:"seconds"`
	Cores   float64 `json:"cores"`
	// Allowed is how many CPUs the process could use, 0 when unknown.
	Allowed int `json:"allowed_cpus,omitempty"`
}

// cpuMark is a reading of the CPU time of the processes of a scenario, and of
// the gateway's heap allocations.
type cpuMark struct {
	at                  time.Time
	gateway, mock, self float64
	// allocs and allocBytes are the gateway's cumulative heap allocations, in
	// objects and bytes, from its metrics; allocsKnown is false when they could
	// not be read, because a scrape failed or the binary does not serve them.
	allocs, allocBytes float64
	allocsKnown        bool
}

// idleCores is the average CPUs the gateway used between two marks.
func idleCores(from, to cpuMark) float64 {
	if wall := to.at.Sub(from.at).Seconds(); wall > 0 {
		return (to.gateway - from.gateway) / wall
	}
	return 0
}

func markCPU(g *gatewayProcess, m *mockProcess) cpuMark {
	// The scrape comes first: it allocates a little itself, after the counters
	// are read, and a few dozen objects are nothing against a run's.
	scraped, err := g.tryScrape()
	gateway, _ := readCPUSeconds(g.pid)
	mock, _ := readCPUSeconds(m.pid)
	mark := cpuMark{at: time.Now(), gateway: gateway, mock: mock, self: selfCPUSeconds()}
	if err == nil {
		objects, haveObjects := scraped[seriesAllocObjects]
		bytes, haveBytes := scraped[seriesAllocBytes]
		mark.allocs, mark.allocBytes, mark.allocsKnown = objects, bytes, haveObjects && haveBytes
	}
	return mark
}

// sampler watches the gateway while a run is applied.
type sampler struct {
	g    *gatewayProcess
	stop chan struct{}
	done chan struct{}

	mu      sync.Mutex
	use     resourceUse
	before  procStatus
	metrics metrics
}

const (
	procInterval    = 200 * time.Millisecond
	metricsInterval = time.Second
)

// startSampler resets the gateway's peak RSS and begins sampling.
func startSampler(t testing.TB, g *gatewayProcess) *sampler {
	t.Helper()
	before, err := readProcStatus(g.pid)
	if err != nil {
		t.Fatalf("read the gateway's /proc status: %v", err)
	}
	s := &sampler{g: g, stop: make(chan struct{}), done: make(chan struct{}), before: before, metrics: g.scrape(t)}
	s.use.PeakResetOK = resetPeakRSS(g.pid)
	s.use.RSSBeforeMiB = mebibytes(before.RSSKB)
	s.use.PeakInferenceAdmitted = s.metrics[seriesInferenceAdmitted]
	s.use.AdmissionCapacity = s.metrics[seriesInferenceCapacity]
	go s.run()
	return s
}

func mebibytes(kb int64) float64 { return float64(kb) / 1024 }

// peakRSS is the gateway's peak resident memory over the run, in mebibytes: the
// kernel's high-water mark when it could be reset at the start of the run, and
// otherwise the largest sample, since a mark that was not reset covers the
// process's whole life, start-up and certification included, and can exceed
// what the run itself reached.
func peakRSS(resetOK bool, highWater, sampled float64) float64 {
	if resetOK {
		return highWater
	}
	return sampled
}

func (s *sampler) run() {
	defer close(s.done)
	procs, scrapes := time.NewTicker(procInterval), time.NewTicker(metricsInterval)
	defer procs.Stop()
	defer scrapes.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-procs.C:
			if st, err := readProcStatus(s.g.pid); err == nil {
				s.mu.Lock()
				s.use.RSSPeakSampledMiB = max(s.use.RSSPeakSampledMiB, mebibytes(st.RSSKB))
				s.use.ThreadsPeak = max(s.use.ThreadsPeak, st.Threads)
				s.mu.Unlock()
			}
		case <-scrapes.C:
			if m, err := s.g.tryScrape(); err == nil {
				s.mu.Lock()
				s.use.PeakInferenceAdmitted = max(s.use.PeakInferenceAdmitted, m[seriesInferenceAdmitted])
				s.use.PeakOpenCircuits = max(s.use.PeakOpenCircuits, m[seriesOpenCircuits])
				s.mu.Unlock()
			}
		}
	}
}

// finish stops sampling and returns what the run consumed.
func (s *sampler) finish(t testing.TB, start, end cpuMark, idle float64, mockCPUs, gatewayCPUs, loadgenCPUs int) resourceUse {
	t.Helper()
	close(s.stop)
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	use := s.use
	if st, err := readProcStatus(s.g.pid); err == nil {
		use.RSSAfterMiB = mebibytes(st.RSSKB)
		use.RSSPeakSampledMiB = max(use.RSSPeakSampledMiB, use.RSSBeforeMiB, use.RSSAfterMiB)
		use.RSSPeakMiB = peakRSS(use.PeakResetOK, mebibytes(st.PeakRSSKB), use.RSSPeakSampledMiB)
		use.ThreadsPeak = max(use.ThreadsPeak, st.Threads)
	}
	after := s.g.scrape(t)
	use.Rejections = after[seriesInferenceRejections] - s.metrics[seriesInferenceRejections]
	wall := end.at.Sub(start.at).Seconds()
	use.WallSeconds, use.GatewayIdleCores = wall, idle
	cpu := func(seconds float64, allowed int) cpuUse {
		u := cpuUse{Seconds: seconds, Allowed: allowed}
		if wall > 0 {
			u.Cores = seconds / wall
		}
		return u
	}
	use.Gateway = cpu(end.gateway-start.gateway, gatewayCPUs)
	use.Mock = cpu(end.mock-start.mock, mockCPUs)
	use.Loadgen = cpu(end.self-start.self, loadgenCPUs)
	return use
}

// dbHandle reads what the gateway's request metadata became in PostgreSQL.
type dbHandle struct{ conn *pgx.Conn }

func openDatabase(t *testing.T, in *installation) *dbHandle {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), in.DatabaseURL)
	if err != nil {
		t.Fatalf("connect to the scenario database: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return &dbHandle{conn: conn}
}

// delivered counts the requests of a key whose metadata reached PostgreSQL,
// whether or not any of them dispatched an attempt: a request the gateway
// refused after admitting it, a budget that could not be read, say, leaves a
// request row and no attempt.
func (d *dbHandle) delivered(ctx context.Context, keyID string) (int64, error) {
	var n int64
	err := d.conn.QueryRow(ctx, `SELECT count(DISTINCT id) FROM olp.requests WHERE api_key_id = $1`, keyID).Scan(&n)
	return n, err
}

// dispatched counts the requests of a key that made at least one attempt.
func (d *dbHandle) dispatched(ctx context.Context, keyID string) (int64, error) {
	var n int64
	err := d.conn.QueryRow(ctx, `SELECT count(DISTINCT request_id) FROM olp.attempt_usage_facts WHERE api_key_id = $1`, keyID).Scan(&n)
	return n, err
}

// attemptRow counts the recorded attempts of one upstream model at one
// position in a request's attempts.
type attemptRow struct {
	UpstreamModel string `json:"upstream_model"`
	Ordinal       int    `json:"attempt_ordinal"`
	Charge        string `json:"charge_status"`
	Attempts      int64  `json:"attempts"`
}

func (d *dbHandle) attempts(ctx context.Context, keyID string) ([]attemptRow, error) {
	rows, err := d.conn.Query(ctx, `SELECT upstream_model, attempt_ordinal, charge_status, count(*)
		FROM olp.attempt_usage_facts WHERE api_key_id = $1 GROUP BY 1, 2, 3 ORDER BY 1, 2, 3`, keyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []attemptRow
	for rows.Next() {
		var r attemptRow
		if err := rows.Scan(&r.UpstreamModel, &r.Ordinal, &r.Charge, &r.Attempts); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// providerShare counts the attempts a provider received.
type providerShare struct {
	Provider string `json:"provider_id"`
	Attempts int64  `json:"attempts"`
}

func (d *dbHandle) byProvider(ctx context.Context, keyID string) ([]providerShare, error) {
	rows, err := d.conn.Query(ctx, `SELECT provider_id::text, count(*) FROM olp.attempt_usage_facts
		WHERE api_key_id = $1 GROUP BY 1 ORDER BY 2 DESC, 1`, keyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []providerShare
	for rows.Next() {
		var share providerShare
		if err := rows.Scan(&share.Provider, &share.Attempts); err != nil {
			return nil, err
		}
		out = append(out, share)
	}
	return out, rows.Err()
}

// epochCounters are what a gateway process recorded about its own metadata
// events when it shut down.
type epochCounters struct {
	Accepted  int64 `json:"accepted"`
	Persisted int64 `json:"persisted"`
	Dropped   int64 `json:"dropped"`
	Abandoned int64 `json:"abandoned"`
	// ClosedGracefully is false when the process did not shut down cleanly.
	ClosedGracefully bool `json:"closed_gracefully"`
}

func (d *dbHandle) epoch(ctx context.Context, instance string) (*epochCounters, error) {
	var e epochCounters
	err := d.conn.QueryRow(ctx, `SELECT accepted, persisted, dropped, abandoned, gracefully_closed_at IS NOT NULL
		FROM olp.request_metadata_gateway_epochs WHERE gateway_instance = $1 ORDER BY started_at DESC LIMIT 1`, instance).
		Scan(&e.Accepted, &e.Persisted, &e.Dropped, &e.Abandoned, &e.ClosedGracefully)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &e, err
}

// metadataResult is request-metadata completeness: of the requests the gateway
// admitted, how many have their metadata in PostgreSQL once the pipeline has
// drained.
type metadataResult struct {
	// Sent is every request the generator put on the wire for the gateway
	// run, warmup included: warmup requests reach the gateway and leave
	// metadata like any other. It includes the diagnostic request sent after a
	// run that had failures.
	Sent               int64 `json:"requests_sent"`
	DiagnosticRequests int64 `json:"diagnostic_requests"`
	// RejectedBeforeAdmission is what the gateway's admission pool shed, which
	// never produces metadata.
	RejectedBeforeAdmission int64 `json:"rejected_before_admission"`
	Admitted                int64 `json:"requests_admitted"`
	// Delivered counts distinct requests with metadata in PostgreSQL for the
	// run's key. DeliveredByAPI is the usage report's request count, which
	// counts only requests that dispatched an attempt, and so equals Delivered
	// when none was refused first.
	Delivered      int64 `json:"events_delivered"`
	DeliveredByAPI int64 `json:"events_delivered_by_usage_api"`
	// DeliveredAfterShutdown is the count once the gateway has stopped and
	// flushed, which exceeds Delivered when the drain wait ended too soon.
	DeliveredAfterShutdown int64 `json:"events_delivered_after_shutdown"`
	// Missing is Admitted less Delivered, never negative; a Delivered above
	// Admitted shows as Surplus, which means a request the generator did not
	// see as sent (a client timeout, say) still reached the gateway.
	Missing      int64   `json:"events_missing"`
	Surplus      int64   `json:"events_surplus"`
	Completeness float64 `json:"completeness"`
	// ReportedGapEvents is the gap the usage report itself admits to: events
	// the gateway counted as lost, plus the in-flight lower bound of unclean
	// epochs.
	ReportedGapEvents int64 `json:"reported_gap_events"`
	// DroppedByGateway and AbandonedByGateway are the gateway's own counters.
	DroppedByGateway   float64        `json:"dropped_by_gateway"`
	AbandonedByGateway float64        `json:"abandoned_by_gateway"`
	FailOpen           float64        `json:"limiter_fail_open"`
	Epoch              *epochCounters `json:"gateway_epoch,omitempty"`
	Drain              drainResult    `json:"drain"`
	// Healthy says Valkey was reachable throughout: the limiter was available
	// and the metadata writer never retried. The roadmap's zero-loss target is
	// stated for a healthy Valkey.
	Healthy bool `json:"valkey_healthy"`
	// MetricsStale is true when the gateway's metrics did not refresh after the
	// run ended within the time allowed, so Healthy and the counters read from
	// them may predate its last seconds.
	MetricsStale bool `json:"metrics_stale,omitempty"`
}

// admittedRequests is how many of the requests sent the gateway took in: those
// its admission pool shed never produce metadata.
func admittedRequests(sent, rejected int64) int64 { return sent - rejected }

// account settles the result from its counts: the admitted requests, how many
// of them have no metadata and how many metadata rows have no request, and the
// share delivered.
func (m *metadataResult) account() {
	m.Admitted = admittedRequests(m.Sent, m.RejectedBeforeAdmission)
	m.Missing, m.Surplus, m.Completeness = 0, 0, 0
	if m.Admitted > m.Delivered {
		m.Missing = m.Admitted - m.Delivered
	} else {
		m.Surplus = m.Delivered - m.Admitted
	}
	if m.Admitted > 0 {
		m.Completeness = min(1, float64(m.Delivered)/float64(m.Admitted))
	}
}

type drainResult struct {
	// Settled is true when delivery finished: every admitted request arrived,
	// or the pipeline went quiet with nothing more arriving.
	Settled bool    `json:"settled"`
	Seconds float64 `json:"waited_seconds"`
	Reason  string  `json:"reason"`
}

// awaitDrain waits, with the process alive, for metadata to reach PostgreSQL.
// The gateway's pipeline gauges come from a snapshot refreshed every fifteen
// seconds, so a quiet gauge counts only when its snapshot was taken after the
// last delivery.
func awaitDrain(t *testing.T, g *gatewayProcess, db *dbHandle, keyID string, want int64, limit time.Duration) (drainResult, int64) {
	t.Helper()
	start := time.Now()
	deadline := start.Add(limit)
	var delivered int64
	lastChange := start
	for {
		n, err := db.delivered(t.Context(), keyID)
		if err != nil {
			t.Fatalf("count delivered request metadata: %v", err)
		}
		if n != delivered {
			delivered, lastChange = n, time.Now()
		}
		if delivered >= want {
			return drainResult{Settled: true, Seconds: time.Since(start).Seconds(), Reason: "every admitted request has metadata"}, delivered
		}
		if m, err := g.tryScrape(); err == nil {
			quiet := m[seriesEventsPending] == 0 && m[seriesConsumerLag] == 0 && m[seriesConsumerPending] == 0
			if quiet && snapshotAfter(m, time.Now(), lastChange) {
				return drainResult{Settled: true, Seconds: time.Since(start).Seconds(), Reason: "the metadata pipeline went quiet with requests still missing"}, delivered
			}
		}
		if time.Now().After(deadline) {
			return drainResult{Seconds: time.Since(start).Seconds(), Reason: fmt.Sprintf("still %d short after %s", want-delivered, limit)}, delivered
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// snapshotTaken is when the gateway took the snapshot that its pipeline and
// limiter metrics were read from, and known is false when it has taken none: a
// gateway in that state reports an age of 2^64-1 seconds, which is no age at
// all.
func snapshotTaken(m metrics, now time.Time) (taken time.Time, known bool) {
	age := m[seriesSnapshotAge]
	return now.Add(-time.Duration(age * float64(time.Second))), age < 3600
}

// snapshotAfter reports whether the gateway's metrics snapshot was certainly
// taken after t. Its age is served in whole seconds, truncated, so the instant
// it names can be up to a second later than the snapshot's own, and only one
// that lies a second past t is known to be after it.
func snapshotAfter(m metrics, now, t time.Time) bool {
	taken, known := snapshotTaken(m, now)
	return known && taken.After(t.Add(time.Second))
}

// metricsRefreshWait is how long to wait for the gateway to refresh its metrics
// snapshot, which it does every fifteen seconds.
const metricsRefreshWait = 45 * time.Second

// metricsAfter reads the gateway's metrics once its snapshot was taken after
// the given time, so that Valkey's health and the gateway's own loss counters
// cover the whole run and not an older instant. It returns the last metrics it
// read and fresh=false if no such snapshot appeared within the limit.
func metricsAfter(t testing.TB, g *gatewayProcess, after time.Time, limit time.Duration) (m metrics, fresh bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	var last error
	for {
		scraped, err := g.tryScrape()
		if err != nil {
			last = err
		} else {
			m = scraped
			if snapshotAfter(m, time.Now(), after) {
				return m, true
			}
		}
		if time.Now().After(deadline) {
			if m == nil {
				t.Fatalf("read the gateway's metrics: %v", last)
			}
			return m, false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// usageRequestCount asks the usage report for the requests of a key, a second
// path to the same number.
func usageRequestCount(c *console, keyID string, from time.Time) (requests, gap int64) {
	c.t.Helper()
	query := url.Values{"start": {from.Add(-time.Hour).UTC().Format(time.RFC3339)}, "end": {time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, "api_key_id": {keyID}}
	var summary struct {
		RequestCount int64 `json:"request_count"`
		Gap          int64 `json:"request_metadata_gap_events"`
	}
	status, raw, _ := c.do(http.MethodGet, "/api/v1/usage/summary?"+query.Encode(), nil, nil)
	if status != http.StatusOK || json.Unmarshal(raw, &summary) != nil {
		c.t.Fatalf("usage summary: status %d: %s", status, raw)
	}
	return summary.RequestCount, summary.Gap
}

// keyBudget reads what a key's cost budget accrued from the management API,
// which sums the cost of the attempts recorded for the key and so is complete
// only once the request metadata has arrived.
func keyBudget(c *console, keyID string) *budgetResult {
	c.t.Helper()
	status, raw, _ := c.do(http.MethodGet, "/api/v1/api-keys/"+keyID, nil, nil)
	if status != http.StatusOK {
		c.t.Fatalf("read the key: status %d: %s", status, raw)
	}
	budget, err := parseKeyBudget(raw)
	if err != nil {
		c.t.Fatalf("read the key's budget: %v: %s", err, raw)
	}
	return budget
}

// parseKeyBudget reads the budget of an API key's detail.
func parseKeyBudget(raw []byte) (*budgetResult, error) {
	var key struct {
		Budget *struct {
			Daily struct {
				Accrued string `json:"accrued"`
			} `json:"daily"`
			Monthly struct {
				Accrued string `json:"accrued"`
			} `json:"monthly"`
			// Unpriced is the attempts of the key that no price covered.
			Unpriced int64 `json:"unpriced_attempts"`
		} `json:"budget"`
	}
	if err := json.Unmarshal(raw, &key); err != nil {
		return nil, err
	}
	if key.Budget == nil {
		return nil, fmt.Errorf("the key has no budget")
	}
	b := key.Budget
	for name, amount := range map[string]string{"daily": b.Daily.Accrued, "monthly": b.Monthly.Accrued} {
		if _, ok := new(big.Rat).SetString(amount); !ok {
			return nil, fmt.Errorf("the %s accrual %q is not a decimal", name, amount)
		}
	}
	return &budgetResult{DailyAccrued: b.Daily.Accrued, MonthlyAccrued: b.Monthly.Accrued, UnpricedAttempts: b.Unpriced}, nil
}
