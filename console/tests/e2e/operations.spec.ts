import AxeBuilder from '@axe-core/playwright';
import { expect, test, type Page } from '@playwright/test';

const requestId = '01980000-0000-7000-8000-000000000101';
const generationId = '01980000-0000-7000-8000-000000000102';
const keyId = '01980000-0000-7000-8000-000000000103';
const providerId = '01980000-0000-7000-8000-000000000104';

async function mockSession(page: Page) {
  await page.route('**/api/v1/sessions/current', async (route) => {
    await route.fulfill({
      json: {
        user: { id: '01980000-0000-7000-8000-000000000001', email: 'owner@example.com', display_name: 'Ada Owner', role: 'owner' },
        csrf_token: 'csrf-test-token'
      }
    });
  });
}

async function emulateTwoHundredPercentZoom(page: Page) {
  const viewport = page.viewportSize();
  if (!viewport || viewport.width <= 480) return;
  await page.setViewportSize({
    width: Math.max(320, Math.floor(viewport.width / 2)),
    height: Math.max(480, Math.floor(viewport.height / 2))
  });
}

test('request explorer filters metadata and opens an accessible attempt timeline', async ({ page }, testInfo) => {
  await mockSession(page);
  await page.route(/\/api\/v1\/requests(?:\/[^?]+)?(?:\?.*)?$/, async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === `/api/v1/requests/${requestId}`) {
      await route.fulfill({ json: {
        id: requestId, runtime_generation_id: generationId, api_key_id: keyId, route: 'support-chat', operation: 'generation', surface: 'openai', started_at: '2026-07-12T12:00:00Z', completed_at: '2026-07-12T12:00:00.245Z', status_code: 200, error_class: null, total_latency_ms: 245, first_byte_ms: 81, attempt_count: 1, input_tokens: 42, output_tokens: 18, cached_input_tokens: 0, estimated_cost: '0.00125', unpriced: false, usage_complete: true,
        attempts: [{ id: '01980000-0000-7000-8000-000000000105', provider_id: providerId, provider_name: 'Primary OpenAI', upstream_model: 'gpt-test', ordinal: 1, started_at: '2026-07-12T12:00:00Z', completed_at: '2026-07-12T12:00:00.245Z', status_code: 200, error_class: null, latency_ms: 245, first_byte_ms: 81, committed: true }]
      }});
      return;
    }
    await route.fulfill({ json: { data: [{ id: requestId, runtime_generation_id: generationId, api_key_id: keyId, route: 'support-chat', operation: 'generation', surface: 'openai', started_at: '2026-07-12T12:00:00Z', completed_at: '2026-07-12T12:00:00.245Z', status_code: 200, error_class: null, total_latency_ms: 245, first_byte_ms: 81, attempt_count: 1, input_tokens: 42, output_tokens: 18, cached_input_tokens: 0, estimated_cost: '0.00125', unpriced: false, usage_complete: true }], next_cursor: null } });
  });

  await page.goto('/requests');
  await expect(page.getByRole('heading', { name: 'Request Explorer' })).toBeVisible();
  const resultLabel = testInfo.project.name === 'mobile-chromium'
    ? page.locator('.mobile-results').getByText('support-chat')
    : page.locator('.desktop-results').getByText('support-chat');
  await expect(resultLabel).toBeVisible();
  await expect(page.getByText('secret prompt')).toHaveCount(0);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await expect(page).toHaveScreenshot('request-explorer.png', {
    fullPage: true,
    animations: 'disabled'
  });

  await page.getByRole('link', { name: `View request ${requestId}` }).click();
  await expect(page.getByRole('heading', { name: 'Request timeline' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Attempt timeline' })).toBeVisible();
  await expect(page.getByText('Response committed')).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});

test('usage exposes pricing gaps and exact chart data accessibly', async ({ page }) => {
  await mockSession(page);
  const point = { bucket: '2026-07-12T12:00:00Z', request_count: 12, input_tokens: '420', output_tokens: '180', estimated_cost: '0.45', unpriced_count: 1, incomplete_count: 0 };
  const coverage = { range_complete: false, approximate: true, excluded_partial_aggregate_boundaries: 1 };
  const request_metadata_consumer = { state: 'stale', pending_events: 4, lag_events: 7, oldest_pending_at: '2026-07-12T11:59:00Z', checked_at: '2026-07-12T12:00:00Z', heartbeat_age_seconds: 61 };
  await page.route('**/api/v1/usage/**', async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith('/summary')) await route.fulfill({ json: { request_count: 12, input_tokens: '420', output_tokens: '180', cached_input_tokens: '0', media_units: '0', estimated_cost: '0.45', unpriced_count: 1, incomplete_count: 0, request_metadata_gap_events: 0, uncertain_request_metadata_gap_count: 1, coverage, request_metadata_consumer, complete: false } });
    else if (path.endsWith('/time-series')) await route.fulfill({ json: { data: [point], coverage } });
    else if (path.endsWith('/breakdown')) await route.fulfill({ json: { data: [{ dimension: 'support-chat', request_count: 12, input_tokens: '420', output_tokens: '180', estimated_cost: '0.45', unpriced_count: 1, incomplete_count: 0 }], coverage } });
    else await route.fulfill({ json: { complete: false, request_count: 12, priced_count: 11, unpriced_count: 1, incomplete_count: 0, request_metadata_gap_events: 0, uncertain_request_metadata_gap_count: 1, estimated_cost: '0.45', coverage, request_metadata_consumer } });
  });

  await page.goto('/usage');
  await expect(page.getByRole('heading', { name: 'Usage', exact: true })).toBeVisible();
  await expect(page.getByText('Request metadata worker heartbeat is stale')).toBeVisible();
  const persistence = page.getByRole('region', { name: 'Request metadata persistence and usage range coverage' });
  await expect(persistence).toContainText('Stale');
  await expect(persistence.getByText('Pending acknowledgements').locator('..')).toContainText('4');
  await expect(persistence.getByText('Stream lag').locator('..')).toContainText('7');
  await expect(persistence).toContainText('1 partial retained-hour boundary excluded');
  await expect(persistence.getByText('Gateway epoch uncertainty').locator('..')).toContainText('1');
  await page.getByText('View chart data').click();
  await expect(page.getByRole('table', { name: 'Exact usage values shown in the time-series chart' })).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});

test('health and audit remain usable with forced colors, reduced motion, and 200% zoom', async ({ page }) => {
  await mockSession(page);
  let epochOpen = true;
  let epochAcknowledged = false;
  await page.emulateMedia({ forcedColors: 'active', reducedMotion: 'reduce' });
  await page.route('**/api/v1/health/ready', async (route) => route.fulfill({ json: { status: 'ok', generation: 8, database: 'ok', limits: 'ok', request_metadata_complete: true } }));
  await page.route('**/api/v1/provider-health*', async (route) => route.fulfill({ json: { window_minutes: 15, data: [{ provider_id: providerId, provider_name: 'Primary OpenAI', provider_kind: 'openai', provider_state: 'active', status: 'healthy', last_probe_at: '2026-07-12T12:00:00Z', last_probe_status: 'success', last_probe_detail: 'Authenticated', last_attempt_at: '2026-07-12T12:00:00Z', attempt_count: 10, success_count: 10, rate_limit_count: 0, server_error_count: 0, transport_error_count: 0, average_latency_ms: 98 }] } }));
  await page.route('**/api/v1/runtime-generations*', async (route) => route.fulfill({ json: { data: [{ id: generationId, sequence: 8, sha256: 'a'.repeat(64), created_by: keyId, created_by_email: 'owner@example.com', created_at: '2026-07-12T12:00:00Z' }], next_cursor: null } }));
  await page.route('**/api/v1/usage/completeness*', async (route) => route.fulfill({ json: { complete: true, request_count: 10, priced_count: 10, unpriced_count: 0, incomplete_count: 0, request_metadata_gap_events: 0, uncertain_request_metadata_gap_count: 0, request_metadata_consumer: { state: 'healthy', pending_events: 0, lag_events: 0 }, estimated_cost: '0.01' } }));
  await page.route('**/api/v1/request-metadata/gateway-epochs**', async (route) => {
    if (route.request().method() === 'POST') {
      epochAcknowledged = true;
      epochOpen = false;
      await route.fulfill({ json: { process_epoch: requestId, gateway_instance: 'gateway-a', acknowledged_by: keyId, acknowledged_at: '2026-07-12T12:05:00Z' } });
      return;
    }
    await route.fulfill({ json: {
      data: epochOpen ? [{
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
      }] : [],
      next_cursor: null
    } });
  });
  await emulateTwoHundredPercentZoom(page);
  await page.goto('/health');
  await expect(page.getByRole('heading', { name: 'Health' })).toBeVisible();
  await expect(page.getByText('Usage accounting is complete')).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Unresolved gateway epochs' })).toBeVisible();
  page.once('dialog', (dialog) => dialog.accept());
  await page.getByRole('button', { name: 'Acknowledge epoch' }).click();
  await expect(page.getByText('No unclean gateway epoch awaits acknowledgement.')).toBeVisible();
  expect(epochAcknowledged).toBe(true);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);

  await page.route('**/api/v1/audit*', async (route) => route.fulfill({ json: { data: [{ id: requestId, actor_user_id: keyId, actor_email: 'owner@example.com', action: 'route.activate', resource_type: 'route', resource_id: 'support-chat', outcome: 'success', occurred_at: '2026-07-12T12:00:00Z' }], next_cursor: null } }));
  await page.goto('/audit');
  await expect(page.getByRole('table')).toContainText('route.activate');
  await expect(page.getByRole('columnheader', { name: 'Origin' })).toHaveCount(0);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});

test('playground sends an ephemeral session-authorized structured-output request', async ({ page }) => {
  await mockSession(page);
  let headers: Record<string, string> = {};
  let payload: Record<string, unknown> = {};
  await page.route('**/api/v1/playground', async (route) => {
    headers = route.request().headers();
    payload = route.request().postDataJSON() as Record<string, unknown>;
    await route.fulfill({ json: { id: 'resp_test', model: 'support-chat', output_text: null, tool_calls: null, structured_output: { answer: 'Safe and ephemeral' }, usage: { input_tokens: 12, output_tokens: 4, total_tokens: 16 }, latency_ms: 142 } });
  });
  await page.goto('/playground');
  await page.getByRole('radio', { name: 'Text' }).focus();
  await page.keyboard.press('ArrowRight');
  await page.keyboard.press('ArrowRight');
  await expect(page.getByRole('radio', { name: 'Structured output' })).toHaveAttribute('data-state', 'checked');
  await page.getByLabel('Route slug').fill('support-chat');
  await page.getByLabel('Client surface').selectOption('anthropic');
  await page.getByLabel('Prompt').fill('Return a structured answer.');
  await page.getByRole('button', { name: 'Run test' }).click();
  await expect(page.getByText('Safe and ephemeral')).toBeVisible();
  expect(payload).toMatchObject({ model: 'support-chat', surface: 'anthropic', input: 'Return a structured answer.' });
  expect(payload).not.toHaveProperty('stream');
  expect(headers.authorization).toBeUndefined();
  // WebKit serializes Fetch's `cache: 'no-store'` mode as `no-cache` on the
  // wire; both values forbid reuse by the browser cache.
  expect(['no-store', 'no-cache']).toContain(headers['cache-control']);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});
