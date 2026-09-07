import type { Page } from '../playwright';
import { requestId, generationId, keyId, providerId } from './request-fixtures';

const readinessSnapshot = () => ({
  status: 'ok',
  generation: 8,
  database: 'ok',
  limits: 'ok',
  asynchronous_plane: 'healthy',
  asynchronous_plane_current: true,
  asynchronous_plane_drained: true,
  // Older than the 20 second checkpoint threshold in docs/operations.md.
  asynchronous_plane_last_progress_at: new Date(
    Date.now() - 600_000
  ).toISOString(),
  worker_tasks_stale: 1,
  worker_tasks_unknown: 0,
  request_metadata_complete: true,
  request_metadata_consumer: 'healthy',
  request_metadata_consumer_pending_events: 4,
  request_metadata_consumer_lag_events: 7,
  request_metadata_consumer_oldest_pending_at: '2026-07-12T11:59:00Z',
  request_metadata_consumer_oldest_pending_age_seconds: 41,
  request_metadata_consumer_checked_at: '2026-07-12T12:00:00Z',
  request_metadata_consumer_heartbeat_age_seconds: 61,
  request_metadata_reclaimed_events_total: 3,
  request_metadata_recovered_events_total: 2,
  request_metadata_duplicate_persistence_total: null,
  request_metadata_gateway_open_epochs: 2,
  request_metadata_gateway_unresolved_epochs: 1,
  request_metadata_gateway_unresolved_event_lower_bound: 2,
  request_metadata_historical_uncertain_gaps: 5,
  runtime_outbox: 'healthy',
  runtime_outbox_pending_rows: 12,
  runtime_outbox_oldest_pending_at: '2026-07-12T11:58:00Z',
  runtime_outbox_oldest_pending_age_seconds: 12,
  runtime_outbox_owner_active: true,
  runtime_outbox_claimed_rows: 3,
  runtime_outbox_owner_abandoned: false,
  runtime_outbox_heartbeat_age_seconds: 4,
  runtime_outbox_publication_attempts_total: 120,
  runtime_outbox_publication_retries_total: 2,
  runtime_outbox_repeated_publication_attempts_total: 1,
  runtime_outbox_abandoned_ownership_total: 0,
  runtime_outbox_failed_takeovers_total: 0,
  media_reconciliation: 'healthy',
  media_reconciliation_pending: 2,
  media_reconciliation_stale: 0,
  media_reconciliation_failed: 0,
  media_reconciliation_unbound: 0,
  media_reconciliation_gaps_total: 0,
  media_spool_used_bytes: 536_870_912,
  media_spool_capacity_bytes: 1_073_741_824
});

const providerHealthItem = {
  provider_id: providerId,
  provider_name: 'Primary OpenAI',
  provider_kind: 'openai',
  provider_state: 'active',
  status: 'healthy',
  last_probe_at: '2026-07-12T12:00:00Z',
  last_probe_status: 'success',
  last_probe_detail: 'Authenticated',
  last_attempt_at: '2026-07-12T12:04:00Z',
  attempt_count: 10,
  success_count: 10,
  rate_limit_count: 0,
  server_error_count: 0,
  transport_error_count: 0,
  average_latency_ms: 98
};

export async function mockOperationsHealth(page: Page) {
  await page.route('**/api/v1/health/ready', async (route) =>
    route.fulfill({ json: readinessSnapshot() })
  );
  await page.route('**/api/v1/provider-health*', async (route) => {
    const window = Number(
      new URL(route.request().url()).searchParams.get('window_minutes') ?? 15
    );
    await route.fulfill({
      json: { window_minutes: window, items: [providerHealthItem] }
    });
  });
  await page.route('**/api/v1/runtime-generations*', async (route) =>
    route.fulfill({
      json: {
        items: [
          {
            id: generationId,
            sequence: 8,
            sha256: 'a'.repeat(64),
            created_by: keyId,
            created_by_email: 'owner@example.com',
            created_at: '2026-07-12T12:00:00Z'
          }
        ],
        next_cursor: null
      }
    })
  );
  await page.route('**/api/v1/usage/completeness*', async (route) =>
    route.fulfill({
      json: {
        complete: true,
        request_count: 10,
        priced_count: 10,
        unpriced_count: 0,
        incomplete_count: 0,
        request_metadata_gap_events: 0,
        uncertain_request_metadata_gap_count: 0,
        request_metadata_consumer: {
          state: 'healthy',
          pending_events: 0,
          lag_events: 0
        },
        estimated_cost: '0.01'
      }
    })
  );
  await mockGatewayEpochs(page);
}

async function mockGatewayEpochs(page: Page) {
  let epochOpen = true;
  await page.route(
    '**/api/v1/request-metadata/gateway-epochs**',
    async (route) => {
      if (route.request().method() === 'POST') {
        epochOpen = false;
        await route.fulfill({
          json: {
            process_epoch: requestId,
            gateway_instance: 'gateway-a',
            acknowledged_by: keyId,
            acknowledged_at: '2026-07-12T12:05:00Z'
          }
        });
        return;
      }
      await route.fulfill({
        json: {
          items: epochOpen
            ? [
                {
                  process_epoch: requestId,
                  gateway_instance: 'gateway-a',
                  state: 'unresolved',
                  accepted: 12,
                  persisted: 10,
                  dropped: 1,
                  abandoned: 0,
                  retrying: false,
                  writer_closed: false,
                  uncertain_event_lower_bound: 2,
                  started_at: '2026-07-12T11:55:00Z',
                  updated_at: '2026-07-12T12:00:00Z',
                  stale_detected_at: '2026-07-12T12:00:00Z',
                  gracefully_closed_at: null,
                  acknowledged_at: null,
                  acknowledged_by: null
                }
              ]
            : [],
          next_cursor: null
        }
      });
    }
  );
}
