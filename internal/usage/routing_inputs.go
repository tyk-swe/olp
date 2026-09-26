package usage

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"time"

	"github.com/tyk-swe/olp/internal/access"
)

const RoutingRefreshInterval = 10 * time.Second
const PerformanceMaxAge = 60 * time.Second
const PerformanceMinSamples = 20

type RoutingPrice struct {
	Price
	RevisionID    string    `json:"revision_id"`
	Revision      int       `json:"revision"`
	EffectiveAt   time.Time `json:"effective_at"`
	ScopePriority int       `json:"scope_priority"`
}
type Performance struct {
	SampleCount int64     `json:"sample_count"`
	LatencyMS   float64   `json:"latency_ms"`
	Throughput  *float64  `json:"throughput"`
	ObservedAt  time.Time `json:"observed_at"`
}
type RoutingInputs struct {
	Prices      []RoutingPrice
	Performance map[string]Performance
	RefreshedAt time.Time
}

func PerformanceKey(provider, model, operation, mode string) string {
	return provider + "\x00" + model + "\x00" + operation + "\x00" + mode
}
func (i *RoutingInputs) Price(kind, provider, vendor, model, operation string, now time.Time) *RoutingPrice {
	if i == nil || now.Sub(i.RefreshedAt) > PerformanceMaxAge {
		return nil
	}
	var found *RoutingPrice
	for _, v := range i.Prices {
		if v.ProviderKind != kind || v.Model != model || v.Operation != operation || v.EffectiveAt.After(now) || v.ProviderID != nil && *v.ProviderID != provider || v.VendorID != nil && *v.VendorID != vendor {
			continue
		}
		p := v
		p.ScopePriority = 0
		if p.VendorID != nil {
			p.ScopePriority++
		}
		if p.ProviderID != nil {
			p.ScopePriority += 2
		}
		if found == nil || p.ScopePriority > found.ScopePriority || p.ScopePriority == found.ScopePriority && (p.EffectiveAt.After(found.EffectiveAt) || p.EffectiveAt.Equal(found.EffectiveAt) && p.Revision > found.Revision) {
			found = &p
		}
	}
	if found != nil {
		found.ProviderID = &provider
	}
	return found
}
func (p *RoutingPrice) Scalar(operation string) *big.Rat {
	if p == nil {
		return nil
	}
	amounts := []*string{p.UnitPrice}
	if operation == "generation" {
		amounts = []*string{p.InputPerMillion, p.OutputPerMillion}
	}
	if operation == "embeddings" {
		amounts = []*string{p.InputPerMillion}
	}
	total := new(big.Rat)
	for _, amount := range amounts {
		if amount == nil {
			return nil
		}
		v, ok := new(big.Rat).SetString(*amount)
		if !ok {
			return nil
		}
		total.Add(total, v)
	}
	return total
}
func (i *RoutingInputs) Metrics(provider, model, operation, mode string, now time.Time) *Performance {
	if i == nil || now.Sub(i.RefreshedAt) > PerformanceMaxAge {
		return nil
	}
	p, ok := i.Performance[PerformanceKey(provider, model, operation, mode)]
	if !ok || p.SampleCount < PerformanceMinSamples || now.Sub(p.ObservedAt) > 5*time.Minute {
		return nil
	}
	return &p
}

// LoadRoutingInputs reads the same immutable rates and persisted successful
// attempt measurements used by accounting. A failed refresh leaves its old
// snapshot in place; performance expires independently after sixty seconds.
func LoadRoutingInputs(ctx context.Context, q access.Queryer, now time.Time) (*RoutingInputs, error) {
	in := &RoutingInputs{RefreshedAt: now, Performance: map[string]Performance{}}
	rows, err := q.Query(ctx, `WITH ranked AS (
 SELECT p.*,r.id AS rid,r.revision,r.effective_at,row_number() OVER
 (PARTITION BY p.provider_kind,p.provider_id,p.vendor_id,p.model,p.operation,(r.effective_at>$1)
 ORDER BY r.effective_at DESC,r.revision DESC) AS rank
 FROM olp.prices p JOIN olp.pricing_revisions r ON r.id=p.pricing_revision_id)
 SELECT rid::text,revision,effective_at,provider_kind,provider_id::text,vendor_id,model,operation,
 input_per_million::text,output_per_million::text,cached_input_per_million::text,
 cache_write_input_per_million::text,cache_write_5m_input_per_million::text,cache_write_1h_input_per_million::text,
 unit_price::text,btrim(currency)
 FROM ranked WHERE rank=1 OR effective_at>$1 LIMIT 100001`, now)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p RoutingPrice
		if err = rows.Scan(&p.RevisionID, &p.Revision, &p.EffectiveAt, &p.ProviderKind, &p.ProviderID, &p.VendorID, &p.Model, &p.Operation, &p.InputPerMillion, &p.OutputPerMillion, &p.CachedInputPerMillion, &p.CacheWriteInputPerMillion, &p.CacheWrite5MInputPerMillion, &p.CacheWrite1HInputPerMillion, &p.UnitPrice, &p.Currency); err != nil {
			rows.Close()
			return nil, err
		}
		in.Prices = append(in.Prices, p)
	}
	rows.Close()
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	if len(in.Prices) > 100000 {
		return nil, errors.New("routing price catalogue exceeds bounded capacity")
	}
	rows, err = q.Query(ctx, `SELECT a.provider_id::text,a.upstream_model,r.operation,a.routing->>'mode',count(*),
 avg(CASE WHEN a.routing->>'mode'='streaming' THEN (a.routing->>'first_output_ms')::double precision ELSE a.latency_ms END),
 CASE WHEN count(*) FILTER(WHERE a.routing->>'mode'='streaming' AND a.routing->>'streamed_output_tokens' IS NOT NULL AND a.latency_ms>(a.routing->>'first_output_ms')::double precision)>=20 THEN
 avg(CASE WHEN a.routing->>'mode'='streaming' AND a.latency_ms>(a.routing->>'first_output_ms')::double precision
 THEN (a.routing->>'streamed_output_tokens')::double precision*1000/(a.latency_ms-(a.routing->>'first_output_ms')::double precision) END) END,max(a.completed_at)
 FROM olp.attempts a JOIN olp.requests r ON r.id=a.request_id AND r.started_at=a.request_started_at
 WHERE a.completed_at>=$1::timestamptz-interval '5 minutes' AND a.completed_at<=$1 AND a.error_class IS NULL
 AND a.status_code BETWEEN 200 AND 299 AND a.latency_ms IS NOT NULL AND a.routing->>'mode' IN ('unary','streaming')
 AND (a.routing->>'mode'<>'streaming' OR a.routing->>'first_output_ms' IS NOT NULL)
 GROUP BY a.provider_id,a.upstream_model,r.operation,a.routing->>'mode' HAVING count(*)>=20 LIMIT 100001`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var provider, model, op, mode string
		var p Performance
		if err = rows.Scan(&provider, &model, &op, &mode, &p.SampleCount, &p.LatencyMS, &p.Throughput, &p.ObservedAt); err != nil {
			return nil, err
		}
		in.Performance[PerformanceKey(provider, model, op, mode)] = p
	}
	if len(in.Performance) > 100000 {
		return nil, errors.New("routing performance catalogue exceeds bounded capacity")
	}
	return in, rows.Err()
}

func (p Performance) MarshalJSON() ([]byte, error) {
	var throughput *int64
	if p.Throughput != nil {
		v := int64(math.Floor(*p.Throughput))
		throughput = &v
	}
	return json.Marshal(struct {
		Latency    int64     `json:"latency_ms"`
		Samples    int64     `json:"samples"`
		Throughput *int64    `json:"output_tokens_per_second"`
		ObservedAt time.Time `json:"observed_at"`
	}{int64(math.Ceil(p.LatencyMS)), p.SampleCount, throughput, p.ObservedAt})
}
