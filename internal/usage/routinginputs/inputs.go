package routinginputs

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"math"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/usage/rates"
)

const RefreshInterval = 10 * time.Second

const PerformanceMaxAge = 60 * time.Second

const PerformanceMinSamples = 20

type Performance struct {
	SampleCount int64     `json:"sample_count"`
	LatencyMS   float64   `json:"latency_ms"`
	Throughput  *float64  `json:"throughput"`
	ObservedAt  time.Time `json:"observed_at"`
}

type Inputs struct {
	priceIndex  map[routingPriceKey][]*rates.RoutingPrice
	Prices      []rates.RoutingPrice
	Performance map[string]Performance
	// Discounts are the multipliers in force now plus every scheduled one;
	// the most specific scope at the plan time is pinned on the attempt.
	Discounts []rates.RoutingDiscount
	// RateCards are each owner's card with its in-force and scheduled
	// revisions; a request with an authority project resolves and pins one.
	RateCards            []RoutingRateCard
	ProjectOrganizations map[string]string
	RefreshedAt          time.Time
}

type routingPriceKey struct {
	kind, model, operation string
}

func indexRoutingPrices(prices []rates.RoutingPrice) map[routingPriceKey][]*rates.RoutingPrice {
	if len(prices) < 2 {
		return nil
	}
	index := make(map[routingPriceKey][]*rates.RoutingPrice)
	for n := range prices {
		price := &prices[n]
		key := routingPriceKey{price.ProviderKind, price.Model, price.Operation}
		index[key] = append(index[key], price)
	}
	return index
}

func (i *Inputs) pricesFor(key routingPriceKey) iter.Seq[*rates.RoutingPrice] {
	return func(yield func(*rates.RoutingPrice) bool) {
		if i.priceIndex != nil {
			for _, price := range i.priceIndex[key] {
				if !yield(price) {
					return
				}
			}
			return
		}
		for n := range i.Prices {
			if !yield(&i.Prices[n]) {
				return
			}
		}
	}
}

// RoutingRateCard is one owner's card with its revisions and their explicit
// rates, parsed like routing prices.
type RoutingRateCard struct {
	ID             string  `json:"id"`
	OrganizationID *string `json:"organization_id"`
	ProjectID      *string `json:"project_id"`
	Revisions      []rates.RoutingCardRevision
}

func PerformanceKey(provider, model, operation, mode string) string {
	return provider + "\x00" + model + "\x00" + operation + "\x00" + mode
}

func (i *Inputs) Price(kind, provider, vendor, model, operation string, now time.Time) *rates.RoutingPrice {
	if i == nil || now.Sub(i.RefreshedAt) > PerformanceMaxAge {
		return nil
	}
	var found *rates.RoutingPrice
	for v := range i.pricesFor(routingPriceKey{kind, model, operation}) {
		if v.ProviderKind != kind || v.Model != model || v.Operation != operation || v.EffectiveAt.After(now) || v.ProviderID != nil && *v.ProviderID != provider || v.VendorID != nil && *v.VendorID != vendor {
			continue
		}
		if found == nil || v.ScopePriority > found.ScopePriority || v.ScopePriority == found.ScopePriority && (v.EffectiveAt.After(found.EffectiveAt) || v.EffectiveAt.Equal(found.EffectiveAt) && v.Revision > found.Revision) {
			found = v
		}
	}
	if found == nil {
		return nil
	}
	pinned := *found
	pinned.ProviderID = &provider
	return &pinned
}

// Discount is the multiplier in force for one attempt's scope at `now`: the
// most specific scope wins (provider over vendor over kind), then the newest
// effective time, then the newest created time, then the greatest id.
func (i *Inputs) Discount(kind, provider, vendor string, now time.Time) *rates.RoutingDiscount {
	if i == nil || now.Sub(i.RefreshedAt) > PerformanceMaxAge {
		return nil
	}
	var found *rates.RoutingDiscount
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

// RateCardRevision is the resolved card revision for a consumer at `now`:
// the most specific card — project, then its organization, then the
// installation — that has a revision in force; a card whose revisions all
// start in the future falls through to the next scope. Known reports the
// inputs are fresh enough to decide; when they are not the caller must leave
// the attempt unpinned rather than pin "no card".
func (i *Inputs) RateCardRevision(projectID *string, now time.Time) (revision *rates.RoutingCardRevision, known bool) {
	if i == nil || now.Sub(i.RefreshedAt) > PerformanceMaxAge {
		return nil, false
	}
	var organization string
	if projectID != nil {
		organization = i.ProjectOrganizations[*projectID]
	}
	inForce := func(card *RoutingRateCard) *rates.RoutingCardRevision {
		if card == nil {
			return nil
		}
		var found *rates.RoutingCardRevision
		for index := range card.Revisions {
			candidate := &card.Revisions[index]
			if candidate.EffectiveAt.After(now) {
				continue
			}
			if found == nil || candidate.EffectiveAt.After(found.EffectiveAt) ||
				candidate.EffectiveAt.Equal(found.EffectiveAt) && candidate.Revision > found.Revision {
				found = candidate
			}
		}
		return found
	}
	var projectCard, organizationCard, installationCard *RoutingRateCard
	for index := range i.RateCards {
		candidate := &i.RateCards[index]
		switch {
		case projectID != nil && candidate.ProjectID != nil && *candidate.ProjectID == *projectID:
			projectCard = candidate
		case organization != "" && candidate.OrganizationID != nil && *candidate.OrganizationID == organization:
			organizationCard = candidate
		case candidate.ProjectID == nil && candidate.OrganizationID == nil:
			installationCard = candidate
		}
	}
	for _, card := range []*RoutingRateCard{projectCard, organizationCard, installationCard} {
		if found := inForce(card); found != nil {
			return found, true
		}
	}
	return nil, true
}

func (i *Inputs) Metrics(provider, model, operation, mode string, now time.Time) *Performance {
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
func Load(ctx context.Context, q access.Queryer, now time.Time) (*Inputs, error) {
	in := &Inputs{RefreshedAt: now, Performance: map[string]Performance{}}
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
 unit_price::text,btrim(currency), training_per_million::text, storage_per_gib_day::text, cache_storage_per_million_token_hour::text, container_per_session::text
 FROM ranked WHERE rank=1 OR effective_at>$1 LIMIT 100001`, now)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p rates.RoutingPrice
		var toolCalls, tiers, windows, containerSessions *string
		if err = rows.Scan(&p.RevisionID, &p.Revision, &p.EffectiveAt, &p.ProviderKind, &p.ProviderID, &p.VendorID, &p.Model, &p.Operation, &p.InputPerMillion, &p.OutputPerMillion, &p.CachedInputPerMillion, &p.CacheWriteInputPerMillion, &p.CacheWrite5MInputPerMillion, &p.CacheWrite1HInputPerMillion, &p.AudioInputPerMillion, &p.CachedAudioInputPerMillion, &p.AudioOutputPerMillion, &p.ImageInputPerMillion, &p.CachedImageInputPerMillion, &p.ImageOutputPerMillion, &p.PerRequest, &p.BatchMultiplier, &toolCalls, &tiers, &windows, &p.UnitPrice, &p.Currency, &p.TrainingPerMillion, &p.StoragePerGiBDay, &p.CacheStoragePerMillionTokenHour, &containerSessions); err != nil {
			rows.Close()
			return nil, err
		}
		if err = rates.DecodeContainerRates(&p.Price, containerSessions); err != nil {
			rows.Close()
			return nil, err
		}
		if err = rates.UnmarshalPriceComponents(&p.Price, toolCalls, tiers, windows, "routing"); err != nil {
			rows.Close()
			return nil, err
		}
		in.Prices = append(in.Prices, *rates.NewRoutingPrice(p))
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
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
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
	in.priceIndex = indexRoutingPrices(in.Prices)
	return in, nil
}

// loadRoutingDiscounts reads the multipliers in force plus those scheduled
// within 90 days; a bounded catalogue keeps each plan's pin lookup cheap.
func loadRoutingDiscounts(ctx context.Context, q access.Queryer, now time.Time, in *Inputs) error {
	rows, err := q.Query(ctx, `SELECT id::text,provider_kind,vendor_id,provider_id::text,
 multiplier::text,effective_at,created_at
 FROM (
  (SELECT DISTINCT ON (provider_kind,vendor_id,provider_id) *
   FROM olp.pricing_discounts WHERE effective_at<=$1
   ORDER BY provider_kind,vendor_id,provider_id,effective_at DESC,created_at DESC,id DESC)
  UNION ALL
  (SELECT * FROM olp.pricing_discounts
   WHERE effective_at>$1 AND effective_at<=$1::timestamptz+interval '90 days')
 ) discounts ORDER BY id LIMIT 100001`, now)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var d rates.RoutingDiscount
		if err = rows.Scan(&d.ID, &d.ProviderKind, &d.VendorID, &d.ProviderID,
			&d.Multiplier, &d.EffectiveAt, &d.CreatedAt); err != nil {
			return err
		}
		parsed, err := rates.NewRoutingDiscount(d)
		if err != nil {
			return err
		}
		in.Discounts = append(in.Discounts, *parsed)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if len(in.Discounts) > 100000 {
		return errors.New("routing discount catalogue exceeds bounded capacity")
	}
	return nil
}

// loadRoutingRateCards reads every card with its in-force and scheduled
// revisions and their prices, plus the organization of every project a card
// may resolve through.
func loadRoutingRateCards(ctx context.Context, q access.Queryer, now time.Time, in *Inputs) error {
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
	if err = rows.Err(); err != nil {
		return err
	}

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
	revisions := map[string]*rates.RoutingCardRevision{}
	cardRevisions := map[string][]*rates.RoutingCardRevision{}
	revisionIDs := []string{}
	rows, err = q.Query(ctx, `SELECT id::text,rate_card_id::text,revision,markup::text,effective_at
 FROM (
  (SELECT DISTINCT ON (rate_card_id) *
   FROM olp.rate_card_revisions WHERE effective_at<=$1
   ORDER BY rate_card_id,effective_at DESC,revision DESC)
  UNION ALL
  (SELECT * FROM olp.rate_card_revisions WHERE effective_at>$1)
 ) active ORDER BY rate_card_id,revision DESC LIMIT 100001`, now)
	if err != nil {
		return err
	}
	for rows.Next() {
		var cardID string
		var loaded rates.RoutingCardRevision
		if err = rows.Scan(&loaded.ID, &cardID, &loaded.Revision, &loaded.Markup, &loaded.EffectiveAt); err != nil {
			rows.Close()
			return err
		}
		revision, err := rates.NewRoutingCardRevision(loaded)
		if err != nil {
			rows.Close()
			return err
		}
		revisions[revision.ID] = revision
		revisionIDs = append(revisionIDs, revision.ID)
		if _, ok := cards[cardID]; ok {
			cardRevisions[cardID] = append(cardRevisions[cardID], revision)
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if len(revisionIDs) > 100000 {
		return errors.New("routing rate card revision catalogue exceeds bounded capacity")
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
 tool_calls::text,tiers::text,windows::text,btrim(currency), training_per_million::text, storage_per_gib_day::text, cache_storage_per_million_token_hour::text, container_per_session::text
 FROM olp.rate_card_prices WHERE rate_card_revision_id=ANY($1::uuid[]) LIMIT 100001`, revisionIDs)
	if err != nil {
		return err
	}
	priceCount := 0
	for rows.Next() {
		priceCount++
		var revisionID string
		var p rates.RoutingPrice
		var toolCalls, tiers, windows, containerSessions *string
		if err = rows.Scan(&revisionID, &p.ProviderKind, &p.VendorID, &p.Model, &p.Operation,
			&p.InputPerMillion, &p.CachedInputPerMillion, &p.CacheWriteInputPerMillion,
			&p.CacheWrite5MInputPerMillion, &p.CacheWrite1HInputPerMillion,
			&p.OutputPerMillion, &p.AudioInputPerMillion, &p.CachedAudioInputPerMillion,
			&p.AudioOutputPerMillion, &p.ImageInputPerMillion, &p.CachedImageInputPerMillion,
			&p.ImageOutputPerMillion, &p.UnitPrice, &p.PerRequest, &p.BatchMultiplier,
			&toolCalls, &tiers, &windows, &p.Currency, &p.TrainingPerMillion, &p.StoragePerGiBDay, &p.CacheStoragePerMillionTokenHour, &containerSessions); err != nil {
			rows.Close()
			return err
		}
		if err = rates.DecodeContainerRates(&p.Price, containerSessions); err != nil {
			rows.Close()
			return err
		}
		if err = rates.UnmarshalPriceComponents(&p.Price, toolCalls, tiers, windows, "routing rate card"); err != nil {
			rows.Close()
			return err
		}
		if revision, ok := revisions[revisionID]; ok {
			revision.Prices = append(revision.Prices, *rates.NewRoutingPrice(p))
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if priceCount > 100000 {
		return errors.New("routing rate card price catalogue exceeds bounded capacity")
	}
	for _, id := range order {
		for _, revision := range cardRevisions[id] {
			cards[id].Revisions = append(cards[id].Revisions, *revision)
		}
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
