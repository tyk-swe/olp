package retention

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/usage/reports"
)

// Deleting and aggregating the same rows in one statement keeps a late stream
// event out of the delete set until a later pass, and makes repeated rollups
// additive for an hour that already carries retained totals.
const rollupSQL = `WITH candidates AS (
      SELECT ctid FROM olp.attempt_usage_facts
       WHERE observed_at < date_trunc('hour', $1::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
       LIMIT $2 FOR UPDATE SKIP LOCKED
    ), expired AS (
      DELETE FROM olp.attempt_usage_facts fact USING candidates
       WHERE fact.ctid = candidates.ctid
      RETURNING route_slug, provider_id, upstream_model, operation, surface, api_key_id,
                budget_group_id, observed_at, input_tokens, output_tokens, cached_input_tokens,
                cache_write_input_tokens, cache_write_5m_input_tokens, cache_write_1h_input_tokens,
                audio_input_tokens, cached_audio_input_tokens, audio_output_tokens,
                image_input_tokens, cached_image_input_tokens, image_output_tokens,
                media_units,
                estimated_cost, currency, charge_status, unpriced,
                request_counted, provider_request_counted, model_request_counted,
                target_request_counted, request_unpriced_counted, provider_unpriced_counted,
                model_unpriced_counted, target_unpriced_counted, request_incomplete_counted,
                provider_incomplete_counted, model_incomplete_counted, target_incomplete_counted,
                attribution, end_user_digest, budget_exempt, agent_principal_id, delegation_pair_digest, usage_observed, estimated_input_tokens, estimate_provenance, model_family,
                commitment_weight, commitment_amount, commitment_period, charge,
                list_cost, contracted_cost, cache_budget_cost, cache_budget_charge, response_cache,training_tokens,storage_byte_seconds,cache_token_seconds,container_sessions
    ), rolled AS (
      INSERT INTO olp.attempt_usage_hourly
        (bucket, budget_bucket, route_slug, provider_id, upstream_model, operation, surface, api_key_id,
         budget_group_id, attribution, end_user_digest, budget_exempt, agent_principal_id, delegation_pair_digest,
         request_count, provider_request_count, model_request_count, target_request_count,
         input_tokens, output_tokens, cached_input_tokens,
         cache_write_input_tokens, cache_write_5m_input_tokens, cache_write_1h_input_tokens,
         audio_input_tokens, cached_audio_input_tokens, audio_output_tokens,
         image_input_tokens, cached_image_input_tokens, image_output_tokens,
         media_units, estimated_cost,
         request_unpriced_count, provider_unpriced_count, model_unpriced_count,
         target_unpriced_count, unpriced_attempt_count, request_incomplete_count,
         provider_incomplete_count, model_incomplete_count, target_incomplete_count, currency,
         model_family, estimate_provenance, estimated_input_tokens,
         estimate_reported_input_tokens, estimate_attempt_count,
         commitment_weight, commitment_amount, commitment_period, charge, uncharged_attempt_count,
         list_cost, contracted_cost, cache_budget_cost, cache_budget_charge, provider_cache_hit_count,training_tokens,storage_byte_seconds,cache_token_seconds,container_sessions)
      SELECT date_trunc('hour', observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',olp.budget_bucket(observed_at),
             route_slug, provider_id, upstream_model, operation, surface, api_key_id,
             budget_group_id, attribution, end_user_digest, budget_exempt, agent_principal_id, COALESCE(delegation_pair_digest,''),
             COUNT(*) FILTER (WHERE request_counted),
             COUNT(*) FILTER (WHERE provider_request_counted),
             COUNT(*) FILTER (WHERE model_request_counted),
             COUNT(*) FILTER (WHERE target_request_counted),
             COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
             COALESCE(SUM(cached_input_tokens), 0),
             COALESCE(SUM(cache_write_input_tokens), 0),
             COALESCE(SUM(cache_write_5m_input_tokens), 0),
             COALESCE(SUM(cache_write_1h_input_tokens), 0),
             COALESCE(SUM(audio_input_tokens), 0),
             COALESCE(SUM(cached_audio_input_tokens), 0),
             COALESCE(SUM(audio_output_tokens), 0),
             COALESCE(SUM(image_input_tokens), 0),
             COALESCE(SUM(cached_image_input_tokens), 0),
             COALESCE(SUM(image_output_tokens), 0),
             COALESCE(SUM(media_units), 0),
             SUM(estimated_cost),
             COUNT(*) FILTER (WHERE request_unpriced_counted),
             COUNT(*) FILTER (WHERE provider_unpriced_counted),
             COUNT(*) FILTER (WHERE model_unpriced_counted),
             COUNT(*) FILTER (WHERE target_unpriced_counted),
             COUNT(*) FILTER (WHERE charge_status <> 'not_billable' AND unpriced),
             COUNT(*) FILTER (WHERE request_incomplete_counted),
             COUNT(*) FILTER (WHERE provider_incomplete_counted),
             COUNT(*) FILTER (WHERE model_incomplete_counted),
             COUNT(*) FILTER (WHERE target_incomplete_counted), MAX(currency),
             model_family, estimate_provenance,
             COALESCE(SUM(estimated_input_tokens) FILTER (WHERE ` + reports.EstimatePaired + `), 0),
             COALESCE(SUM(input_tokens) FILTER (WHERE ` + reports.EstimatePaired + `), 0),
             COUNT(*) FILTER (WHERE ` + reports.EstimatePaired + `),
             COALESCE(SUM(commitment_weight), 0), commitment_amount, commitment_period,
             SUM(charge), COUNT(*) FILTER (WHERE charge_status <> 'not_billable' AND charge IS NULL),
             SUM(COALESCE(list_cost, estimated_cost + COALESCE(commitment_weight, 0))),
             SUM(COALESCE(contracted_cost, estimated_cost + COALESCE(commitment_weight, 0))),
             SUM(cache_budget_cost), SUM(cache_budget_charge),
             COUNT(*) FILTER (WHERE response_cache AND provider_request_counted),
 COALESCE(SUM(training_tokens),0),COALESCE(SUM(storage_byte_seconds),0),COALESCE(SUM(cache_token_seconds),0),COALESCE(olp.container_session_totals(jsonb_agg(container_sessions)),'{}'::jsonb)
        FROM expired
       GROUP BY date_trunc('hour', observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',olp.budget_bucket(observed_at),
                route_slug, provider_id, upstream_model, operation, surface, api_key_id,
                budget_group_id, attribution, end_user_digest, budget_exempt, model_family, estimate_provenance,
                commitment_amount, commitment_period, agent_principal_id, COALESCE(delegation_pair_digest,'')
      ON CONFLICT ON CONSTRAINT attempt_usage_hourly_dimensions_key DO UPDATE SET
        request_count = attempt_usage_hourly.request_count + EXCLUDED.request_count,
        provider_request_count = attempt_usage_hourly.provider_request_count + EXCLUDED.provider_request_count,
        model_request_count = attempt_usage_hourly.model_request_count + EXCLUDED.model_request_count,
        target_request_count = attempt_usage_hourly.target_request_count + EXCLUDED.target_request_count,
        training_tokens=attempt_usage_hourly.training_tokens+EXCLUDED.training_tokens,
 storage_byte_seconds=attempt_usage_hourly.storage_byte_seconds+EXCLUDED.storage_byte_seconds,
 cache_token_seconds=attempt_usage_hourly.cache_token_seconds+EXCLUDED.cache_token_seconds,
 container_sessions=COALESCE(olp.container_session_totals(jsonb_build_array(attempt_usage_hourly.container_sessions,EXCLUDED.container_sessions)),'{}'::jsonb),
 input_tokens = attempt_usage_hourly.input_tokens + EXCLUDED.input_tokens,
        output_tokens = attempt_usage_hourly.output_tokens + EXCLUDED.output_tokens,
        cached_input_tokens = attempt_usage_hourly.cached_input_tokens + EXCLUDED.cached_input_tokens,
        cache_write_input_tokens = attempt_usage_hourly.cache_write_input_tokens + EXCLUDED.cache_write_input_tokens,
        cache_write_5m_input_tokens = attempt_usage_hourly.cache_write_5m_input_tokens + EXCLUDED.cache_write_5m_input_tokens,
        cache_write_1h_input_tokens = attempt_usage_hourly.cache_write_1h_input_tokens + EXCLUDED.cache_write_1h_input_tokens,
        audio_input_tokens = attempt_usage_hourly.audio_input_tokens + EXCLUDED.audio_input_tokens,
        cached_audio_input_tokens = attempt_usage_hourly.cached_audio_input_tokens + EXCLUDED.cached_audio_input_tokens,
        audio_output_tokens = attempt_usage_hourly.audio_output_tokens + EXCLUDED.audio_output_tokens,
        image_input_tokens = attempt_usage_hourly.image_input_tokens + EXCLUDED.image_input_tokens,
        cached_image_input_tokens = attempt_usage_hourly.cached_image_input_tokens + EXCLUDED.cached_image_input_tokens,
        image_output_tokens = attempt_usage_hourly.image_output_tokens + EXCLUDED.image_output_tokens,
        media_units = attempt_usage_hourly.media_units + EXCLUDED.media_units,
        estimated_cost = CASE
          WHEN attempt_usage_hourly.estimated_cost IS NULL AND EXCLUDED.estimated_cost IS NULL THEN NULL
          ELSE COALESCE(attempt_usage_hourly.estimated_cost, 0) + COALESCE(EXCLUDED.estimated_cost, 0) END,
        request_unpriced_count = attempt_usage_hourly.request_unpriced_count + EXCLUDED.request_unpriced_count,
        provider_unpriced_count = attempt_usage_hourly.provider_unpriced_count + EXCLUDED.provider_unpriced_count,
        model_unpriced_count = attempt_usage_hourly.model_unpriced_count + EXCLUDED.model_unpriced_count,
        target_unpriced_count = attempt_usage_hourly.target_unpriced_count + EXCLUDED.target_unpriced_count,
        unpriced_attempt_count = attempt_usage_hourly.unpriced_attempt_count + EXCLUDED.unpriced_attempt_count,
        request_incomplete_count = attempt_usage_hourly.request_incomplete_count + EXCLUDED.request_incomplete_count,
        provider_incomplete_count = attempt_usage_hourly.provider_incomplete_count + EXCLUDED.provider_incomplete_count,
        model_incomplete_count = attempt_usage_hourly.model_incomplete_count + EXCLUDED.model_incomplete_count,
        target_incomplete_count = attempt_usage_hourly.target_incomplete_count + EXCLUDED.target_incomplete_count,
        currency = COALESCE(attempt_usage_hourly.currency, EXCLUDED.currency),
        estimated_input_tokens = attempt_usage_hourly.estimated_input_tokens + EXCLUDED.estimated_input_tokens,
        estimate_reported_input_tokens = attempt_usage_hourly.estimate_reported_input_tokens + EXCLUDED.estimate_reported_input_tokens,
        estimate_attempt_count = attempt_usage_hourly.estimate_attempt_count + EXCLUDED.estimate_attempt_count,
        commitment_weight = attempt_usage_hourly.commitment_weight + EXCLUDED.commitment_weight,
        commitment_amount = COALESCE(attempt_usage_hourly.commitment_amount, EXCLUDED.commitment_amount),
        commitment_period = COALESCE(attempt_usage_hourly.commitment_period, EXCLUDED.commitment_period),
        charge = CASE
          WHEN attempt_usage_hourly.charge IS NULL AND EXCLUDED.charge IS NULL THEN NULL
          ELSE COALESCE(attempt_usage_hourly.charge, 0) + COALESCE(EXCLUDED.charge, 0) END,
        uncharged_attempt_count = attempt_usage_hourly.uncharged_attempt_count + EXCLUDED.uncharged_attempt_count,
        list_cost = CASE
          WHEN attempt_usage_hourly.list_cost IS NULL AND attempt_usage_hourly.estimated_cost IS NULL
               AND EXCLUDED.list_cost IS NULL THEN NULL
          ELSE COALESCE(attempt_usage_hourly.list_cost,
                        attempt_usage_hourly.estimated_cost + attempt_usage_hourly.commitment_weight, 0)
               + COALESCE(EXCLUDED.list_cost, 0) END,
        cache_budget_cost = attempt_usage_hourly.cache_budget_cost + EXCLUDED.cache_budget_cost,
        cache_budget_charge = attempt_usage_hourly.cache_budget_charge + EXCLUDED.cache_budget_charge,
        provider_cache_hit_count = attempt_usage_hourly.provider_cache_hit_count + EXCLUDED.provider_cache_hit_count,
        contracted_cost = CASE
          WHEN attempt_usage_hourly.contracted_cost IS NULL AND attempt_usage_hourly.estimated_cost IS NULL
               AND EXCLUDED.contracted_cost IS NULL THEN NULL
          ELSE COALESCE(attempt_usage_hourly.contracted_cost,
                        attempt_usage_hourly.estimated_cost + attempt_usage_hourly.commitment_weight, 0)
               + COALESCE(EXCLUDED.contracted_cost, 0) END
      RETURNING 1
    )
    SELECT (SELECT count(*) FROM rolled)::bigint, (SELECT count(*) FROM expired)::bigint`

// rollUpAttemptUsage folds whole expired hours of facts into the hourly table.
// Only hours that have completely passed the usage cutoff are rolled up, so a
// bucket is never half aggregated and half live.
func rollUpAttemptUsage(ctx context.Context, conn *pgx.Conn, cutoff time.Time) (int64, int64, error) {
	var rollups, facts int64
	for {
		if err := ctx.Err(); err != nil {
			return rollups, facts, err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return rollups, facts, fmt.Errorf("roll up attempt usage: %w", err)
		}
		var batchRollups, batchFacts int64
		if err = tx.QueryRow(ctx, rollupSQL, cutoff, int64(retentionBatch)).
			Scan(&batchRollups, &batchFacts); err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return rollups, facts, fmt.Errorf("roll up attempt usage: %w", err)
		}
		if err = tx.Commit(ctx); err != nil {
			return rollups, facts, fmt.Errorf("roll up attempt usage: %w", err)
		}
		if batchRollups < 0 || batchFacts < 0 {
			return rollups, facts, errors.New("maintenance returned an invalid rollup count")
		}
		rollups += batchRollups
		facts += batchFacts
		if batchFacts < retentionBatch {
			return rollups, facts, nil
		}
	}
}
