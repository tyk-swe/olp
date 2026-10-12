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

	// rates is Price parsed for pricing a request without parsing it again. It
	// is set when the routing inputs are loaded and shared by every copy.
	rates *priceRates
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
	// Discounts are the multipliers in force now plus every scheduled one;
	// the most specific scope at the plan time is pinned on the attempt.
	Discounts []RoutingDiscount
	// RateCards are each owner's card with its in-force and scheduled
	// revisions; a request with an authority project resolves and pins one.
	RateCards            []RoutingRateCard
	ProjectOrganizations map[string]string
	RefreshedAt          time.Time
}

// RoutingDiscount is one multiplier the plan may pin on an attempt.
type RoutingDiscount struct {
	ID           string    `json:"id"`
	ProviderKind string    `json:"provider_kind"`
	VendorID     *string   `json:"vendor_id"`
	ProviderID   *string   `json:"provider_id"`
	Multiplier   string    `json:"multiplier"`
	EffectiveAt  time.Time `json:"effective_at"`
	CreatedAt    time.Time `json:"created_at"`

	multiplier *big.Int
}

// parsedMultiplier is the discount at rate scale, as olp.price_usage applies
// it inside the one rounding.
func (d *RoutingDiscount) parsedMultiplier() *big.Int {
	if d.multiplier != nil {
		return d.multiplier
	}
	units, _ := scaledDecimal(d.Multiplier, costDigits)
	return units
}

// RoutingRateCard is one owner's card with its revisions and their explicit
// rates, parsed like routing prices.
type RoutingRateCard struct {
	ID             string  `json:"id"`
	OrganizationID *string `json:"organization_id"`
	ProjectID      *string `json:"project_id"`
	Revisions      []RoutingCardRevision
}

// RoutingCardRevision is one card revision: a markup and explicit prices.
type RoutingCardRevision struct {
	ID          string         `json:"id"`
	Revision    int            `json:"revision"`
	Markup      string         `json:"markup"`
	EffectiveAt time.Time      `json:"effective_at"`
	Prices      []RoutingPrice `json:"prices"`

	markup *big.Int
}

// parsedMarkup is the markup at rate scale.
func (r *RoutingCardRevision) parsedMarkup() *big.Int {
	if r.markup != nil {
		return r.markup
	}
	units, _ := scaledDecimal(r.Markup, costDigits)
	return units
}

func PerformanceKey(provider, model, operation, mode string) string {
	return provider + "\x00" + model + "\x00" + operation + "\x00" + mode
}
func (i *RoutingInputs) Price(kind, provider, vendor, model, operation string, now time.Time) *RoutingPrice {
	if i == nil || now.Sub(i.RefreshedAt) > PerformanceMaxAge {
		return nil
	}
	var found *RoutingPrice
	for index := range i.Prices {
		v := &i.Prices[index]
		if v.ProviderKind != kind || v.Model != model || v.Operation != operation || v.EffectiveAt.After(now) || v.ProviderID != nil && *v.ProviderID != provider || v.VendorID != nil && *v.VendorID != vendor {
			continue
		}
		p := *v
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

// Discount is the multiplier in force for one attempt's scope at `now`: the
// most specific scope wins (provider over vendor over kind), then the newest
// effective time, then the newest created time, then the greatest id.
func (i *RoutingInputs) Discount(kind, provider, vendor string, now time.Time) *RoutingDiscount {
	if i == nil || now.Sub(i.RefreshedAt) > PerformanceMaxAge {
		return nil
	}
	var found *RoutingDiscount
	for index := range i.Discounts {
		d := &i.Discounts[index]
		if d.ProviderKind != kind || d.EffectiveAt.After(now) ||
			d.ProviderID != nil && *d.ProviderID != provider ||
			d.VendorID != nil && *d.VendorID != vendor {
			continue
		}
		candidate := *d
		priority := 0
		if candidate.VendorID != nil {
			priority++
		}
		if candidate.ProviderID != nil {
			priority += 2
		}
		if found == nil {
			found = &candidate
			continue
		}
		existing := 0
		if found.VendorID != nil {
			existing++
		}
		if found.ProviderID != nil {
			existing += 2
		}
		better := priority > existing ||
			priority == existing && (candidate.EffectiveAt.After(found.EffectiveAt) ||
				candidate.EffectiveAt.Equal(found.EffectiveAt) &&
					(candidate.CreatedAt.After(found.CreatedAt) ||
						candidate.CreatedAt.Equal(found.CreatedAt) && candidate.ID > found.ID))
		if better {
			found = &candidate
		}
	}
	return found
}

// RateCardRevision is the resolved card revision for a consumer at `now`,
// nil when no card applies.
func (i *RoutingInputs) RateCardRevision(projectID *string, now time.Time) *RoutingCardRevision {
	if i == nil || now.Sub(i.RefreshedAt) > PerformanceMaxAge {
		return nil
	}
	var organization string
	if projectID != nil {
		organization = i.ProjectOrganizations[*projectID]
	}
	var card *RoutingRateCard
	for index := range i.RateCards {
		candidate := &i.RateCards[index]
		switch {
		case projectID != nil && candidate.ProjectID != nil && *candidate.ProjectID == *projectID:
			card = candidate
		case card == nil && organization != "" && candidate.OrganizationID != nil && *candidate.OrganizationID == organization:
			card = candidate
		case card == nil && candidate.ProjectID == nil && candidate.OrganizationID == nil:
			card = candidate
		}
		if card != nil && card.ProjectID != nil {
			break
		}
	}
	if card == nil {
		return nil
	}
	var found *RoutingCardRevision
	for index := range card.Revisions {
		revision := &card.Revisions[index]
		if revision.EffectiveAt.After(now) {
			continue
		}
		if found == nil || revision.EffectiveAt.After(found.EffectiveAt) ||
			revision.EffectiveAt.Equal(found.EffectiveAt) && revision.Revision > found.Revision {
			found = revision
		}
	}
	return found
}

// Explicit is the card revision's rate for one attempt's dimensions, the
// vendor-scoped entry winning over the unscoped one.
func (r *RoutingCardRevision) Explicit(kind, vendor, model, operation string) *RoutingPrice {
	if r == nil {
		return nil
	}
	var found *RoutingPrice
	for index := range r.Prices {
		p := &r.Prices[index]
		if p.ProviderKind != kind || p.Model != model || p.Operation != operation ||
			p.VendorID != nil && *p.VendorID != vendor {
			continue
		}
		if found == nil || p.VendorID != nil {
			found = p
		}
	}
	return found
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
 audio_input_per_million::text,cached_audio_input_per_million::text,audio_output_per_million::text,
 image_input_per_million::text,cached_image_input_per_million::text,image_output_per_million::text,
 per_request::text,batch_multiplier::text,tool_calls::text,tiers::text,windows::text,
 unit_price::text,btrim(currency)
 FROM ranked WHERE rank=1 OR effective_at>$1 LIMIT 100001`, now)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p RoutingPrice
		var toolCalls, tiers, windows *string
		if err = rows.Scan(&p.RevisionID, &p.Revision, &p.EffectiveAt, &p.ProviderKind, &p.ProviderID, &p.VendorID, &p.Model, &p.Operation, &p.InputPerMillion, &p.OutputPerMillion, &p.CachedInputPerMillion, &p.CacheWriteInputPerMillion, &p.CacheWrite5MInputPerMillion, &p.CacheWrite1HInputPerMillion, &p.AudioInputPerMillion, &p.CachedAudioInputPerMillion, &p.AudioOutputPerMillion, &p.ImageInputPerMillion, &p.CachedImageInputPerMillion, &p.ImageOutputPerMillion, &p.PerRequest, &p.BatchMultiplier, &toolCalls, &tiers, &windows, &p.UnitPrice, &p.Currency); err != nil {
			rows.Close()
			return nil, err
		}
		if toolCalls != nil {
			if err = json.Unmarshal([]byte(*toolCalls), &p.ToolCalls); err != nil {
				rows.Close()
				return nil, err
			}
		}
		if tiers != nil {
			if err = json.Unmarshal([]byte(*tiers), &p.Tiers); err != nil {
				rows.Close()
				return nil, err
			}
		}
		if windows != nil {
			p.Windows = &PriceWindows{}
			if err = json.Unmarshal([]byte(*windows), p.Windows); err != nil {
				rows.Close()
				return nil, err
			}
		}
		p.rates = parseRates(&p.Price)
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
	if err = loadRoutingDiscounts(ctx, q, now, in); err != nil {
		return nil, err
	}
	if err = loadRoutingRateCards(ctx, q, now, in); err != nil {
		return nil, err
	}
	return in, rows.Err()
}

// loadRoutingDiscounts reads the multipliers in force plus every scheduled
// one; a bounded catalogue keeps each plan's pin lookup cheap.
func loadRoutingDiscounts(ctx context.Context, q access.Queryer, now time.Time, in *RoutingInputs) error {
	rows, err := q.Query(ctx, `SELECT id::text,provider_kind,vendor_id,provider_id::text,
 multiplier::text,effective_at,created_at
 FROM olp.pricing_discounts WHERE effective_at<=$1+interval '90 days' ORDER BY id LIMIT 100001`, now)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var d RoutingDiscount
		if err = rows.Scan(&d.ID, &d.ProviderKind, &d.VendorID, &d.ProviderID,
			&d.Multiplier, &d.EffectiveAt, &d.CreatedAt); err != nil {
			return err
		}
		if units, ok := scaledDecimal(d.Multiplier, costDigits); ok {
			d.multiplier = units
		}
		in.Discounts = append(in.Discounts, d)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if len(in.Discounts) > 100000 {
		return errors.New("routing discount catalogue exceeds bounded capacity")
	}
	return nil
}

// loadRoutingRateCards reads every card with its revisions and prices plus
// the organization of every project a card may resolve through.
func loadRoutingRateCards(ctx context.Context, q access.Queryer, _ time.Time, in *RoutingInputs) error {
	in.ProjectOrganizations = map[string]string{}
	rows, err := q.Query(ctx, `SELECT id::text,organization_id::text FROM olp.projects
 WHERE organization_id IS NOT NULL`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var project, organization string
		if err = rows.Scan(&project, &organization); err != nil {
			rows.Close()
			return err
		}
		in.ProjectOrganizations[project] = organization
	}
	rows.Close()

	cards := map[string]*RoutingRateCard{}
	order := []string{}
	rows, err = q.Query(ctx, `SELECT id::text,organization_id::text,project_id::text FROM olp.rate_cards`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var card RoutingRateCard
		if err = rows.Scan(&card.ID, &card.OrganizationID, &card.ProjectID); err != nil {
			rows.Close()
			return err
		}
		cards[card.ID] = &card
		order = append(order, card.ID)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if len(cards) == 0 {
		return nil
	}
	revisions := map[string]*RoutingCardRevision{}
	cardRevisions := map[string][]*RoutingCardRevision{}
	rows, err = q.Query(ctx, `SELECT id::text,rate_card_id::text,revision,markup::text,effective_at
 FROM olp.rate_card_revisions`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var cardID string
		revision := &RoutingCardRevision{}
		if err = rows.Scan(&revision.ID, &cardID, &revision.Revision, &revision.Markup, &revision.EffectiveAt); err != nil {
			rows.Close()
			return err
		}
		if units, ok := scaledDecimal(revision.Markup, costDigits); ok {
			revision.markup = units
		}
		revisions[revision.ID] = revision
		if _, ok := cards[cardID]; ok {
			cardRevisions[cardID] = append(cardRevisions[cardID], revision)
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if len(revisions) == 0 {
		for _, id := range order {
			in.RateCards = append(in.RateCards, *cards[id])
		}
		return nil
	}
	rows, err = q.Query(ctx, `SELECT rate_card_revision_id::text,provider_kind,vendor_id,model,operation,
 input_per_million::text,cached_input_per_million::text,cache_write_input_per_million::text,
 cache_write_5m_input_per_million::text,cache_write_1h_input_per_million::text,
 output_per_million::text,audio_input_per_million::text,cached_audio_input_per_million::text,
 audio_output_per_million::text,image_input_per_million::text,cached_image_input_per_million::text,
 image_output_per_million::text,unit_price::text,per_request::text,batch_multiplier::text,
 tool_calls::text,tiers::text,windows::text,btrim(currency)
 FROM olp.rate_card_prices`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var revisionID string
		var p RoutingPrice
		var toolCalls, tiers, windows *string
		if err = rows.Scan(&revisionID, &p.ProviderKind, &p.VendorID, &p.Model, &p.Operation,
			&p.InputPerMillion, &p.CachedInputPerMillion, &p.CacheWriteInputPerMillion,
			&p.CacheWrite5MInputPerMillion, &p.CacheWrite1HInputPerMillion,
			&p.OutputPerMillion, &p.AudioInputPerMillion, &p.CachedAudioInputPerMillion,
			&p.AudioOutputPerMillion, &p.ImageInputPerMillion, &p.CachedImageInputPerMillion,
			&p.ImageOutputPerMillion, &p.UnitPrice, &p.PerRequest, &p.BatchMultiplier,
			&toolCalls, &tiers, &windows, &p.Currency); err != nil {
			rows.Close()
			return err
		}
		if toolCalls != nil {
			if err = json.Unmarshal([]byte(*toolCalls), &p.ToolCalls); err != nil {
				rows.Close()
				return err
			}
		}
		if tiers != nil {
			if err = json.Unmarshal([]byte(*tiers), &p.Tiers); err != nil {
				rows.Close()
				return err
			}
		}
		if windows != nil {
			p.Windows = &PriceWindows{}
			if err = json.Unmarshal([]byte(*windows), p.Windows); err != nil {
				rows.Close()
				return err
			}
		}
		p.rates = parseRates(&p.Price)
		if revision, ok := revisions[revisionID]; ok {
			revision.Prices = append(revision.Prices, p)
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, id := range order {
		in.RateCards = append(in.RateCards, *cards[id])
	}
	return nil
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
