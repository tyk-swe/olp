import { mockOperationsHealth } from './operations-health-fixtures';
import AxeBuilder from '@axe-core/playwright';
import {
  expectUsageRequests,
  mockUsageKey,
  mockUsageReport
} from './usage-fixtures';
import {
  emulateTwoHundredPercentZoom,
  expect,
  mockSession,
  test,
  type Route
} from '../playwright';

import {
  generationId,
  incompleteUsageRequest,
  keyId,
  mockRequestExplorer,
  pricingRevisionId,
  providerId,
  requestId,
  twoChargeRequest
} from './request-fixtures';

test('request explorer loading state is accessible in forced colors at 200% zoom', async ({
  page
}) => {
  await mockSession(page);
  await page.emulateMedia({ forcedColors: 'active', reducedMotion: 'reduce' });
  await emulateTwoHundredPercentZoom(page);
  let pending: Route | undefined;
  await page.route('**/api/v1/requests*', (route) => {
    pending = route;
  });

  await page.goto('/requests');
  await expect(page.getByRole('status')).toHaveText(
    'Loading request metadata…'
  );
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await pending?.abort();
});

for (const state of ['empty', 'error'] as const) {
  test(`request explorer ${state} state is accessible in forced colors at 200% zoom`, async ({
    page
  }) => {
    await mockSession(page);
    await page.emulateMedia({
      forcedColors: 'active',
      reducedMotion: 'reduce'
    });
    await emulateTwoHundredPercentZoom(page);
    await page.route('**/api/v1/requests*', async (route) => {
      if (state === 'empty')
        await route.fulfill({ json: { items: [], next_cursor: null } });
      else
        await route.fulfill({
          status: 503,
          json: {
            status: 503,
            title: 'Service unavailable',
            detail: 'Request metadata storage is unavailable.'
          }
        });
    });

    await page.goto('/requests');
    // The server's own detail must survive to the operator, not a generic string.
    await expect(
      page.getByText(
        state === 'empty'
          ? 'No matching requests'
          : 'Request metadata storage is unavailable.'
      )
    ).toBeVisible();
    expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  });
}

test('request explorer filters metadata and opens an accessible attempt timeline', async ({
  page
}, testInfo) => {
  await mockSession(page);
  await mockRequestExplorer(page);

  await page.goto('/requests');
  await expect(
    page.getByRole('heading', { name: 'Request Explorer' })
  ).toBeVisible();
  const resultLabel =
    testInfo.project.name === 'mobile-chromium'
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
  await expect(
    page.getByRole('heading', { name: 'Request timeline' })
  ).toBeVisible();
  await page.reload();
  await expect(
    page.getByRole('heading', { name: 'Request timeline' })
  ).toBeVisible();
  await expect(
    page.getByRole('heading', { name: 'Attempt timeline' })
  ).toBeVisible();
  await expect(page.getByText('Response committed')).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});

test('attempt timeline explains failover costs with exact recorded usage and pricing', async ({
  page
}) => {
  await mockSession(page);
  await mockRequestExplorer(page, twoChargeRequest());
  await page.goto(`/requests/${requestId}`);
  const attempts = page.locator('.timeline > li');
  await expect(attempts).toHaveCount(2);
  await expect(
    page.getByRole('region', { name: 'Request summary' })
  ).toContainText('$0.000062');
  for (const [index, cost] of ['0.000042000000', '0.000020000000'].entries()) {
    const attempt = attempts.nth(index);
    await expect(attempt.getByText('Billable', { exact: true })).toBeVisible();
    await expect(attempt.getByText(cost, { exact: true })).toBeVisible();
    await expect(attempt.getByText('USD', { exact: true })).toBeVisible();
    await expect(
      attempt.getByText(pricingRevisionId, { exact: true })
    ).toBeVisible();
    await expect(
      attempt.getByText('Input tokens', { exact: true }).locator('..')
    ).toContainText('10');
  }
  await expect(
    attempts.nth(0).getByText('Cached input tokens').locator('..')
  ).toContainText('4');
  await expect(
    attempts.nth(1).getByText('Cached input tokens').locator('..')
  ).toContainText('0');
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});

test('attempt timeline preserves missing, nonbillable, unpriced and incomplete usage states', async ({
  page
}) => {
  await mockSession(page);
  await page.emulateMedia({ forcedColors: 'active', reducedMotion: 'reduce' });
  await emulateTwoHundredPercentZoom(page);
  await mockRequestExplorer(page, incompleteUsageRequest());
  await page.goto(`/requests/${requestId}`);
  const attempts = page.locator('.timeline > li');
  await expect(attempts).toHaveCount(6);
  await expect(attempts.nth(0)).toContainText(
    'No usage or pricing facts were recorded.'
  );
  await expect(
    attempts.nth(0).getByText('Not billable', { exact: true })
  ).toHaveCount(0);
  await expect(
    attempts.nth(1).getByText('Not billable', { exact: true })
  ).toBeVisible();
  await expect(
    attempts.nth(1).getByText('Estimated cost').locator('..')
  ).toContainText('Not applicable');
  await expect(attempts.nth(2)).toContainText('Billing uncertain');
  await expect(
    attempts.nth(2).getByText('Usage observed').locator('..')
  ).toContainText('No');
  await expect(
    attempts.nth(3).getByText('Unpriced', { exact: true })
  ).toBeVisible();
  await expect(
    attempts.nth(3).getByText('Media units').locator('..')
  ).toContainText('1.125000');
  await expect(attempts.nth(4)).toContainText('Unavailable (incomplete usage)');
  await expect(
    attempts.nth(4).getByText('Unpriced', { exact: true })
  ).toHaveCount(0);
  await expect(
    attempts.nth(4).getByText('Output tokens').locator('..')
  ).toContainText('—');
  await expect(
    attempts.nth(5).getByText('0.000000000000', { exact: true })
  ).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});

test('usage exposes pricing gaps and exact chart data accessibly', async ({
  page
}) => {
  await mockSession(page);
  await mockUsageReport(page);
  await mockUsageKey(page, keyId);

  await page.goto('/usage');
  await expect(
    page.getByRole('heading', { name: 'Usage', exact: true })
  ).toBeVisible();
  await expect(
    page.getByText('Request metadata worker heartbeat is stale')
  ).toBeVisible();
  const persistence = page.getByRole('region', {
    name: 'Request metadata persistence and usage range coverage'
  });
  await expect(persistence).toContainText('Stale');
  await expect(
    persistence.getByText('Pending acknowledgements').locator('..')
  ).toContainText('4');
  await expect(persistence.getByText('Stream lag').locator('..')).toContainText(
    '7'
  );
  await expect(persistence).toContainText(
    'Approximate totals · 1 partial retained-hour boundary excluded'
  );
  await expect(
    persistence.getByText('Priced requests').locator('..')
  ).toContainText('11');
  await expect(
    persistence.getByText('Gateway epoch uncertainty').locator('..')
  ).toContainText('1');
  // An approximate range says so instead of reading as an exact total.
  await expect(
    page.getByText(
      'Totals are approximate: 1 partial retained-hour boundary is excluded.'
    )
  ).toBeVisible();
  await expect(
    page.getByText('11 priced and 1 unpriced requests')
  ).toBeVisible();
  await expect(
    page
      .getByRole('region', { name: 'Usage summary' })
      .getByText('Cached input tokens')
      .locator('..')
  ).toContainText('96');
  await expect(
    page
      .getByRole('region', { name: 'Usage breakdown' })
      .getByRole('columnheader', { name: 'Cached input' })
  ).toBeVisible();
  await page.getByText('View chart data').click();
  const chartData = page.getByRole('table', {
    name: 'Exact usage values shown in the time-series chart'
  });
  await expect(chartData).toBeVisible();
  await expect(chartData).toContainText('96');
  await page.getByLabel('API key ID').fill(keyId);
  await page.getByRole('button', { name: 'Apply' }).click();
  const budget = page.getByRole('region', {
    name: 'Filtered API key budget'
  });
  await expect(budget).toContainText('production SDK');
  await expect(budget).toContainText('2.50 / 10.00');
  await expect(budget).toContainText('18.75 / 100.00');
  await expect(budget).toContainText('Unpriced attempts accrue 0.');
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});

test.describe('usage report URLs', () => {
  test.use({ timezoneId: 'America/New_York' });

  test('direct links preserve applied filters across edits, reload and browser history', async ({
    page
  }) => {
    await mockSession(page);
    const requests = await mockUsageReport(page);
    await mockUsageKey(page, keyId);
    const filters = {
      start: '2026-11-01T06:30:42.123Z',
      end: '2026-11-01T07:30:12.456Z',
      route: 'support-chat',
      model: 'model/one',
      provider_id: providerId,
      api_key_id: keyId,
      operation: 'generation',
      dimension: 'api_key',
      granularity: 'day'
    };
    const initial = `/usage?${new URLSearchParams(filters)}`;
    await page.goto(initial);
    const budget = page.getByRole('region', {
      name: 'Filtered API key budget'
    });
    await expect(budget).toContainText('production SDK');
    await expect(page.getByLabel('From', { exact: true })).toHaveValue(
      '2026-11-01T01:30'
    );
    await expect(page.getByLabel('To', { exact: true })).toHaveValue(
      '2026-11-01T02:30'
    );
    await expect(page.getByLabel('Provider ID')).toHaveValue(providerId);
    await expect(page.getByLabel('Model', { exact: true })).toHaveValue(
      'model/one'
    );
    await expect(page.getByLabel('Operation', { exact: true })).toHaveValue(
      'generation'
    );
    await expect(page.getByLabel('Break down by')).toHaveValue('api_key');
    await expect(page.getByLabel('Time buckets')).toHaveValue('day');
    const canonical = page.url();
    await expect.poll(() => requests.length).toBe(4);
    await page.getByLabel('Route', { exact: true }).fill('second-route');
    await page.getByRole('button', { name: 'Refresh', exact: true }).click();
    await expect.poll(() => requests.length).toBe(8);
    expectUsageRequests(requests.slice(-4), filters);
    expect(page.url()).toBe(canonical);
    await page.getByRole('button', { name: 'Apply', exact: true }).click();
    await expect
      .poll(() => new URL(page.url()).searchParams.get('route'))
      .toBe('second-route');
    await expect.poll(() => requests.length).toBe(12);
    expectUsageRequests(requests.slice(-4), {
      ...filters,
      route: 'second-route'
    });
    const appliedUrl = page.url();
    await page.reload();
    await expect(budget).toContainText('2.50 / 10.00');
    await expect(page.getByLabel('Route', { exact: true })).toHaveValue(
      'second-route'
    );
    expect(page.url()).toBe(appliedUrl);
    await page.goBack();
    await expect(page.getByLabel('Route', { exact: true })).toHaveValue(
      'support-chat'
    );
    await page.goForward();
    await expect(page.getByLabel('Route', { exact: true })).toHaveValue(
      'second-route'
    );
    await page.getByRole('button', { name: 'Clear', exact: true }).click();
    await expect(page.getByLabel('API key ID')).toHaveValue('');
    await expect(budget).toHaveCount(0);
    const cleared = new URL(page.url()).searchParams;
    expect([...cleared.keys()]).toEqual([
      'start',
      'end',
      'dimension',
      'granularity'
    ]);
    expect(cleared.get('dimension')).toBe('api_key');
    expect(cleared.get('granularity')).toBe('day');
    await page.goBack();
    await expect(budget).toContainText('production SDK');
    await expect(page.getByLabel('Route', { exact: true })).toHaveValue(
      'second-route'
    );
  });

  test('malformed parameters stay visible until corrected and unknown axes get defaults', async ({
    page
  }) => {
    await mockSession(page);
    const requests = await mockUsageReport(page);
    await page.goto(
      '/usage?start=invalid&end=2026-07-12T12%3A00%3A00Z&provider_id=invalid&dimension=unknown&granularity=minute'
    );
    await expect(page.getByRole('alert')).toHaveText(
      'Enter valid start and end times.'
    );
    expect(requests).toHaveLength(0);
    await expect(
      page.getByRole('button', { name: 'Refresh', exact: true })
    ).toBeDisabled();
    await expect(page.getByLabel('Break down by')).toHaveValue('route');
    await expect(page.getByLabel('Time buckets')).toHaveValue('hour');
    await page.getByLabel('From', { exact: true }).fill('2026-07-12T07:00');
    await page.getByRole('button', { name: 'Apply', exact: true }).click();
    await expect(
      page.getByText('Provider ID must be a UUID.', { exact: true })
    ).toBeVisible();
    expect(requests).toHaveLength(0);
    await page.getByLabel('Provider ID').fill('');
    await page.getByRole('button', { name: 'Apply', exact: true }).click();
    await expect(
      page.getByRole('region', { name: 'Usage summary' })
    ).toBeVisible();
    await expect(
      page.getByText('Enter valid start and end times.', { exact: true })
    ).toHaveCount(0);
    await expect(
      page.getByText('Provider ID must be a UUID.', { exact: true })
    ).toHaveCount(0);
    const query = new URL(page.url()).searchParams;
    expect(query.get('start')).toBe('2026-07-12T11:00:00.000Z');
    expect(query.get('end')).toBe('2026-07-12T12:00:00.000Z');
    expect(query.get('dimension')).toBe('route');
    expect(query.get('granularity')).toBe('hour');
    expect(query.has('provider_id')).toBe(false);
  });
});

test('health and audit remain usable with forced colors, reduced motion, and 200% zoom', async ({
  page
}) => {
  await mockSession(page);
  await page.emulateMedia({ forcedColors: 'active', reducedMotion: 'reduce' });
  await mockOperationsHealth(page);
  await emulateTwoHundredPercentZoom(page);
  await page.goto('/health');
  await expect(page.getByRole('heading', { name: 'Health' })).toBeVisible();
  await expect(page.getByText('Usage accounting is complete')).toBeVisible();

  const plane = page.getByRole('region', { name: 'Asynchronous plane' });
  await expect(
    plane.getByText('Checkpoints', { exact: true }).locator('..')
  ).toContainText('Current');
  await expect(
    plane.getByText('Queues', { exact: true }).locator('..')
  ).toContainText('Drained');
  await expect(
    plane.getByText('Stale task checkpoints').locator('..')
  ).toContainText('1');
  await expect(
    plane.getByText('Tasks that never reported').locator('..')
  ).toContainText('0');
  // Past the 20 second checkpoint threshold, the age carries the warning style.
  await expect(
    plane.locator('.warning-text').filter({ hasText: 'ago' })
  ).toBeVisible();

  const metadata = page.getByRole('region', { name: 'Persistence pipeline' });
  await expect(
    metadata.getByText('Worker checkpoint').locator('..')
  ).toContainText('1m 1s ago');
  await expect(
    metadata.getByText('Oldest pending event').locator('..')
  ).toContainText('41s ago');
  await expect(
    metadata.getByText('Pending acknowledgements').locator('..')
  ).toContainText('4');
  await expect(metadata.getByText('Stream lag').locator('..')).toContainText(
    '7'
  );
  // A counter the worker could not read stays unknown; it is never a reset.
  await expect(
    metadata.getByText('Duplicate persistence').locator('..')
  ).toContainText('—');
  await expect(
    metadata.getByText('Historical uncertain gaps').locator('..')
  ).toContainText('5');

  const outbox = page.getByRole('region', { name: 'Runtime outbox' });
  await expect(outbox.getByText('Pending rows').locator('..')).toContainText(
    '12'
  );
  await expect(outbox.getByText('Claimed rows').locator('..')).toContainText(
    '3'
  );
  await expect(outbox.getByText('Owner session').locator('..')).toContainText(
    'Active'
  );
  await expect(
    outbox.getByText('Ownership', { exact: true }).locator('..')
  ).toContainText('Held');
  await expect(outbox.getByText('Owner heartbeat').locator('..')).toContainText(
    '4s ago'
  );
  await expect(
    outbox.getByText('Publication retries').locator('..')
  ).toContainText('2');

  const media = page.getByRole('region', { name: 'Media reconciliation' });
  await expect(
    media.getByText('Pending', { exact: true }).locator('..')
  ).toContainText('2');
  await expect(media.getByText('Recorded gaps').locator('..')).toContainText(
    '0'
  );
  await expect(media.getByText('Media spool').locator('..')).toContainText(
    '512 MiB of 1.00 GiB (50.0%)'
  );

  await expect(
    page.getByRole('heading', { name: 'Unresolved gateway epochs' })
  ).toBeVisible();
  const epochRow = page.getByRole('row').filter({ hasText: 'gateway-a' });
  await expect(epochRow).toContainText('Not retrying');
  await expect(epochRow).toContainText('Writer open');
  await expect(epochRow).toContainText('Never closed gracefully');
  await expect(epochRow).toContainText('Not acknowledged');

  const providers = page.getByRole('region', { name: 'Providers' });
  await expect(providers).toContainText('Last live attempt:');
  await expect(providers).toContainText('Counted over the last 15 minutes.');
  await page.getByLabel('Window').selectOption({ label: '1 hour' });
  await expect(providers).toContainText('Counted over the last 60 minutes.');

  page.once('dialog', (dialog) => dialog.accept());
  await page.getByRole('button', { name: 'Acknowledge epoch' }).click();
  await expect(
    page.getByText('No unclean gateway epoch awaits acknowledgement.')
  ).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);

  await page.route('**/api/v1/audit*', async (route) =>
    route.fulfill({
      json: {
        items: [
          {
            id: requestId,
            actor_user_id: keyId,
            actor_email: 'owner@example.com',
            action: 'route.activate',
            resource_type: 'route',
            resource_id: 'support-chat',
            outcome: 'success',
            occurred_at: '2026-07-12T12:00:00Z',
            source_ip: null,
            user_agent_family: null
          }
        ],
        next_cursor: null
      }
    })
  );
  await page.goto('/audit');
  await expect(page.getByRole('table')).toContainText('route.activate');
  // An event the boundary could not attribute to an address reads as absent,
  // never as a blank cell that looks like a missing column.
  await expect(
    page.getByRole('row').filter({ hasText: 'route.activate' })
  ).toContainText('—');
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});

const auditEvent = (overrides: Record<string, unknown> = {}) => ({
  id: requestId,
  actor_user_id: keyId,
  actor_email: 'owner@example.com',
  action: 'route.activate',
  resource_type: 'route',
  resource_id: 'support-chat',
  outcome: 'success',
  occurred_at: '2026-07-12T12:00:00Z',
  source_ip: '203.0.113.7',
  user_agent_family: 'Firefox',
  ...overrides
});

test('audit filters narrow the page and the request origin columns render', async ({
  page
}, testInfo) => {
  await mockSession(page, { installationName: 'Acme Platform' });
  const seenFilters: URLSearchParams[] = [];
  await page.route('**/api/v1/audit*', async (route) => {
    const query = new URL(route.request().url()).searchParams;
    seenFilters.push(query);
    await route.fulfill({
      json: {
        items: [
          query.get('action') === 'provider.update'
            ? auditEvent({
                id: generationId,
                action: 'provider.update',
                resource_type: 'provider',
                resource_id: providerId,
                outcome: 'failure',
                source_ip: null,
                user_agent_family: null
              })
            : auditEvent()
        ],
        next_cursor: null
      }
    });
  });

  await page.goto('/audit');
  // The shell names the installation the session belongs to.
  const installation = page.getByText('Acme Platform');
  if (testInfo.project.name === 'mobile-chromium')
    await expect(installation).toBeAttached();
  else await expect(installation).toBeVisible();

  await expect(
    page.getByRole('columnheader', { name: 'Source IP' })
  ).toBeVisible();
  await expect(
    page.getByRole('columnheader', { name: 'User agent' })
  ).toBeVisible();
  const activation = page
    .getByRole('row')
    .filter({ hasText: 'route.activate' });
  await expect(activation).toContainText('203.0.113.7');
  await expect(activation).toContainText('Firefox');

  await page.getByLabel('Action').fill('provider.update');
  await page.getByLabel('Resource type').fill('provider');
  await page.getByLabel('Resource ID').fill(providerId);
  await page.getByLabel('Actor user ID').fill(keyId);
  await page.getByLabel('Outcome').selectOption('failure');
  await page.getByLabel('Occurred after').fill('2026-07-12T10:00');
  await page.getByLabel('Occurred before').fill('2026-07-12T14:00');
  await page.getByRole('button', { name: 'Apply filters' }).click();

  const update = page.getByRole('row').filter({ hasText: 'provider.update' });
  await expect(update).toBeVisible();
  // Neither address nor user-agent family was recorded for this event.
  await expect(update).toContainText('—');
  expect(seenFilters.map((filters) => Object.fromEntries(filters))).toEqual(
    expect.arrayContaining([
      expect.objectContaining({
        action: 'provider.update',
        resource_type: 'provider',
        resource_id: providerId,
        actor_user_id: keyId,
        outcome: 'failure',
        occurred_after: expect.any(String),
        occurred_before: expect.any(String)
      })
    ])
  );

  // An inverted window is refused in the browser with an inline message the
  // operator can act on, instead of a round trip that comes back a 422.
  const requested = seenFilters.length;
  await page.getByLabel('Occurred after').fill('2026-07-12T14:00');
  await page.getByLabel('Occurred before').fill('2026-07-12T10:00');
  await page.getByRole('button', { name: 'Apply filters' }).click();
  await expect(
    page.getByText('Occurred before must be later than occurred after.')
  ).toBeVisible();
  expect(seenFilters).toHaveLength(requested);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);

  await page.getByRole('button', { name: 'Clear' }).click();
  await expect(page.getByLabel('Action')).toHaveValue('');
  await expect(page.getByLabel('Occurred after')).toHaveValue('');
  await expect(
    page.getByText('Occurred before must be later than occurred after.')
  ).toHaveCount(0);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});

test.describe('audit investigation URLs', () => {
  test.use({ timezoneId: 'America/New_York' });

  test('direct links preserve exact applied filters across edits, reload and history', async ({
    page
  }) => {
    await mockSession(page);
    const requests: URLSearchParams[] = [];
    await page.route('**/api/v1/audit?*', async (route) => {
      requests.push(new URL(route.request().url()).searchParams);
      await route.fulfill({
        json: { items: [auditEvent()], next_cursor: null }
      });
    });
    const filters = {
      action: 'route.activate',
      resource_type: 'route',
      resource_id: 'support-chat',
      actor_user_id: keyId,
      outcome: 'success',
      occurred_after: '2026-11-01T05:50:00.123456Z',
      occurred_before: '2026-11-01T06:10:00.456789Z'
    };
    await page.goto(`/audit?${new URLSearchParams(filters)}`);
    await expect(page.getByRole('table')).toContainText('route.activate');
    await expect(page.getByLabel('Occurred after')).toHaveValue(
      '2026-11-01T01:50'
    );
    await expect(page.getByLabel('Occurred before')).toHaveValue(
      '2026-11-01T01:10'
    );
    const count = requests.length;
    await page.getByLabel('Action').fill('provider.update');
    expect(requests).toHaveLength(count);
    expect(new URL(page.url()).searchParams.get('action')).toBe(
      'route.activate'
    );
    await page.getByRole('button', { name: 'Refresh', exact: true }).click();
    await expect.poll(() => requests.length).toBe(count + 1);
    expect(Object.fromEntries(requests.at(-1)!)).toMatchObject(filters);
    await page.getByRole('button', { name: 'Apply filters' }).click();
    await expect
      .poll(() => new URL(page.url()).searchParams.get('action'))
      .toBe('provider.update');
    await expect(page.getByRole('alert')).toHaveCount(0);
    expect(new URL(page.url()).searchParams.get('occurred_after')).toBe(
      filters.occurred_after
    );
    expect(new URL(page.url()).searchParams.get('occurred_before')).toBe(
      filters.occurred_before
    );
    await page.reload();
    await expect(page.getByLabel('Action')).toHaveValue('provider.update');
    await page.goBack();
    await expect(page.getByLabel('Action')).toHaveValue('route.activate');
    await page.goForward();
    await expect(page.getByLabel('Action')).toHaveValue('provider.update');
    await page.getByRole('button', { name: 'Clear' }).click();
    await expect.poll(() => new URL(page.url()).search).toBe('');
    await expect(page.getByLabel('Action')).toHaveValue('');
    await expect.poll(() => requests.at(-1)?.has('action')).toBe(false);
  });

  for (const [parameter, value, label] of [
    ['actor_user_id', 'not-a-uuid', 'Actor user ID'],
    ['outcome', 'unknown-outcome', 'Outcome'],
    ['occurred_after', '2026-02-30T12:00:00Z', 'Occurred after']
  ]) {
    test(`invalid ${parameter} remains visible without fetching`, async ({
      page
    }) => {
      await mockSession(page);
      let requests = 0;
      await page.route('**/api/v1/audit?*', async (route) => {
        requests += 1;
        await route.fulfill({ json: { items: [], next_cursor: null } });
      });
      await page.goto(`/audit?${new URLSearchParams({ [parameter]: value })}`);
      await expect(page.getByRole('alert')).toContainText(value);
      await expect(page.getByLabel(label)).toHaveValue(value);
      await expect(
        page.getByRole('button', { name: 'Refresh', exact: true })
      ).toBeDisabled();
      expect(requests).toBe(0);
      if (parameter === 'occurred_after') {
        const corrected = '2026-02-28T12:00:00Z';
        await page.getByLabel(label).fill(corrected);
        await expect(page.getByLabel(label)).toHaveValue(corrected);
        await page.getByRole('button', { name: 'Apply filters' }).click();
        await expect.poll(() => requests).toBe(1);
        await expect(page.getByRole('alert')).toHaveCount(0);
        expect(new URL(page.url()).searchParams.get(parameter)).toBe(corrected);
        return;
      }
      await page.getByRole('button', { name: 'Clear' }).click();
      await expect(
        page.getByText('No audit events', { exact: true })
      ).toBeVisible();
      expect(requests).toBe(1);
    });
  }
});
