import AxeBuilder from '@axe-core/playwright';
import { execFile } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { promisify } from 'node:util';
import { readFileSync } from 'node:fs';
import {
  expect,
  test,
  type APIRequestContext,
  type Locator,
  type Page
} from '../playwright';
import { takeSecret, waitForRoutePublication } from '../journeys/fixtures';
import {
  expectFact,
  refreshUntilRequestCount
} from '../journeys/request-history';

// The owner created by tests/access/control.spec.ts; a run that starts on an
// empty installation performs the setup itself.
test.describe.configure({ mode: 'serial' });

const owner = {
  email: 'owner@example.com',
  password: 'a long browser test password'
};
const upstream = {
  origin: 'http://127.0.0.1:4187',
  endpoint: 'http://127.0.0.1:4187/v1',
  model: 'compatible-e2e-model',
  credential: 'compatible-provider-secret'
};
const provider = 'Accounting upstream';
const route = 'accounting-chat';
const keyName = 'Accounting budget key';
const dailyBudget = '5.00';
// The mock upstream reports 7 input and 5 output tokens for every completion,
// so 1000.00 and 2600.00 per million make each priced request cost exactly
// 0.007 + 0.013 = 0.02. Whole cents keep the assertions about accounting
// rather than about rounding.
const price = { input: '1000.00', output: '2600.00' };
const requestCost = '$0.02';
const accrued = '$0.04';

async function resetUpstream(request: APIRequestContext): Promise<void> {
  expect(
    (await request.post(`${upstream.origin}/__test__/reset`)).status()
  ).toBe(204);
}

async function signIn(page: Page): Promise<void> {
  await page.goto('/');
  await expect(page).toHaveURL(/\/(setup|login)(\?.*)?$/);
  if (/\/setup$/.test(page.url())) {
    await page.getByLabel('Display name').fill('Owner');
    await page.getByLabel('Work email').fill(owner.email);
    await page.getByLabel('Password', { exact: true }).fill(owner.password);
    await page.getByLabel('Confirm password').fill(owner.password);
    await page
      .getByLabel('Setup token')
      .fill(readFileSync(process.env.OLP_BOOTSTRAP_TOKEN_FILE!, 'utf8').trim());
    await page.getByRole('button', { name: 'Create owner account' }).click();
  } else {
    const deadline = Date.now() + 75_000;
    while (true) {
      await page.getByLabel('Email').fill(owner.email);
      await page.getByLabel('Password').fill(owner.password);
      const completed = page.waitForResponse(
        (response) =>
          response.request().method() === 'POST' &&
          new URL(response.url()).pathname === '/api/v1/sessions'
      );
      await page.getByRole('button', { name: 'Sign in' }).click();
      const response = await completed;
      if (response.status() !== 429 || Date.now() >= deadline) {
        expect(response.status()).toBe(201);
        break;
      }
      const seconds = Number(response.headers()['retry-after'] ?? '1');
      await new Promise((resolve) =>
        setTimeout(resolve, Math.min(60, Math.max(1, seconds)) * 1000)
      );
    }
  }
  await expect(page).toHaveURL(/\/$/);
}

/// Brings one OpenAI-compatible provider and one active route online through
/// the console, which is the ground truth the accounting assertions need.
async function provisionRoute(page: Page): Promise<void> {
  await page.goto('/providers/new');
  await expect(
    page.getByRole('heading', { name: 'Connect an upstream provider.' })
  ).toBeVisible();
  await page.getByRole('radio', { name: /OpenAI-compatible/ }).check();
  await page.getByLabel('Provider name').fill(provider);
  await page.getByLabel('Authentication').selectOption('api_key');
  await page
    .getByRole('textbox', { name: 'Endpoint', exact: true })
    .fill(upstream.endpoint);
  await page.getByLabel('Seed model (optional)').fill(upstream.model);
  await page
    .getByLabel('Credential', { exact: true })
    .fill(upstream.credential);
  await page.getByRole('button', { name: /Save and test connection/ }).click();
  await expect(
    page.getByRole('heading', { name: 'Discover upstream models' })
  ).toBeVisible();
  await page.getByRole('button', { name: 'Discover upstream models' }).click();
  await expect(
    page.getByRole('heading', { name: 'Review model capabilities' })
  ).toBeVisible();
  await page.getByLabel('Operation 1').selectOption('generation');
  await page.getByLabel('Client surface 1').selectOption('openai');
  await page.getByLabel('Mode 1').selectOption('unary');
  await page.getByLabel('Operation 2').selectOption('generation');
  await page.getByLabel('Client surface 2').selectOption('openai');
  await page.getByLabel('Mode 2').selectOption('streaming');
  await page.getByRole('checkbox', { name: 'Eligible for routes' }).check();
  await page.getByRole('button', { name: 'Save capability review' }).click();
  await expect(
    page.getByText('Capability review saved with declared provenance.')
  ).toBeVisible();
  await page
    .getByRole('button', { name: 'Server-certify capabilities' })
    .click();
  await expect(
    page.getByText(/reviewed tuples passed server certification/)
  ).toBeVisible();
  await page.getByRole('button', { name: 'Continue to activation' }).click();
  await page.getByRole('button', { name: 'Test completed draft' }).click();
  await expect(page.getByText(/Final draft test passed/)).toBeVisible();
  await page.getByRole('button', { name: 'Activate provider' }).click();
  await expect(
    page.getByRole('heading', { name: 'Now build a stable route slug.' })
  ).toBeVisible();

  await page.getByRole('link', { name: 'Build default route' }).click();
  await expect(
    page.getByRole('heading', { name: 'Build a route draft.' })
  ).toBeVisible();
  await page.getByLabel('Public model slug').fill(route);
  await page.getByRole('button', { name: 'Add target' }).click();
  await expect(
    page.getByLabel('Provider model').first().locator('option:checked')
  ).toContainText(upstream.model);
  // This provider has no profile, so the route is declared transformed.
  await page.getByLabel('Fidelity mode').selectOption('transformed');
  await page.getByRole('button', { name: 'Create draft' }).click();
  await expect(page).toHaveURL(/\/routes\/[0-9a-f-]+$/);
  await page
    .getByRole('button', { name: 'Validate draft', exact: true })
    .click();
  await expect(page.getByText('Validation passed.')).toBeVisible();
  await page
    .getByRole('button', { name: 'Activate route', exact: true })
    .click();
  await expect(page.getByText('Revision 1 active')).toBeVisible();
}

/// Issues the accounting key with a daily cost budget and returns its secret.
async function issueBudgetedKey(page: Page): Promise<string> {
  await page.goto('/api-keys/new');
  await expect(
    page.getByRole('heading', { name: 'Create a proxy key.' })
  ).toBeVisible();
  await page.getByLabel('Key name').fill(keyName);
  await page.getByRole('checkbox', { name: 'Model listing' }).check();
  await page
    .getByRole('group', { name: 'Allowed route slugs' })
    .getByRole('checkbox', { name: route })
    .check();
  await page.getByLabel('Daily cost budget (optional)').fill(dailyBudget);
  await page.getByRole('button', { name: /Create and show key/ }).click();
  const dialog = page.getByRole('dialog', { name: 'Copy this secret now.' });
  await expect(dialog).toBeVisible();
  const secret = await takeSecret(dialog);
  expect(secret).toMatch(/^olp_/);
  await dialog.getByRole('button', { name: 'I have saved the key' }).click();
  await expect(dialog).toBeHidden();
  return secret;
}

/// Sends one SDK-shaped chat completion through the public origin and reports
/// whether the upstream usage reached the caller.
function sendChat(page: Page, secret: string, streaming: boolean) {
  return page.evaluate(
    async ({ apiKey, slug, stream }) => {
      const response = await fetch('/v1/chat/completions', {
        method: 'POST',
        headers: {
          Authorization: `Bearer ${apiKey}`,
          'Content-Type': 'application/json'
        },
        body: JSON.stringify({
          model: slug,
          messages: [{ role: 'user', content: 'Account for this request.' }],
          ...(stream
            ? { stream: true, stream_options: { include_usage: true } }
            : {})
        })
      });
      const body = await response.text();
      return {
        status: response.status,
        contentType: response.headers.get('content-type') ?? '',
        usageReported: /"completion_tokens":\s*5/.test(body),
        code: /"code":\s*"([a-z_]+)"/.exec(body)?.[1] ?? null
      };
    },
    { apiKey: secret, slug: route, stream: streaming }
  );
}

/// Sends the journey's first chat once the key's cost budget can be enforced.
///
/// A key with a cost budget is refused with `distributed_limits_unavailable`
/// until the cost reconciliation leader publishes its budget windows into the
/// shared limiter, and that leader runs on a sixty-second cadence, so a key
/// issued mid-journey is briefly unusable by design. Waiting for the window is
/// the only honest way to observe it; the refusals reach the request ledger
/// like any other terminal request, which is why the explorer assertions below
/// select the traffic this journey actually served.
async function sendChatOnceBudgetIsLive(
  page: Page,
  secret: string
): Promise<Awaited<ReturnType<typeof sendChat>>> {
  let call = await sendChat(page, secret, false);
  await expect
    .poll(
      async () => {
        if (call.code !== 'distributed_limits_unavailable') return call.status;
        call = await sendChat(page, secret, false);
        return call.status;
      },
      {
        message:
          'cost reconciliation should publish the budget window that admits this key',
        timeout: 90_000,
        intervals: [1_000, 2_000, 5_000]
      }
    )
    .toBe(200);
  return call;
}

function usageMetric(page: Page, label: string): Locator {
  return page
    .locator('.usage-metrics article')
    .filter({ has: page.getByText(label, { exact: true }) })
    .locator('strong');
}

/// Opens the request explorer filtered to this journey's own traffic and waits
/// for asynchronous persistence to land exactly `count` generation requests.
async function listGenerationRequests(
  page: Page,
  count: number
): Promise<Locator> {
  await page.goto(
    `/requests?route=${route}&operation=generation&status_code=200`
  );
  await expect(
    page.getByRole('heading', { name: 'Request Explorer' })
  ).toBeVisible();
  await refreshUntilRequestCount(page, count);
  return page.locator('.desktop-results tbody tr');
}

async function openDetail(row: Locator): Promise<void> {
  const href = await row
    .getByRole('link', { name: /^View request/ })
    .getAttribute('href');
  expect(href).toBeTruthy();
  await row.page().goto(href!);
  await expect(
    row.page().getByRole('heading', { name: 'Request timeline' })
  ).toBeVisible();
}

test('a browser user prices gateway traffic and reads the accounting it produced', async ({
  page,
  request
}, info) => {
  test.setTimeout(240_000);
  await resetUpstream(request);
  await signIn(page);
  await provisionRoute(page);
  const secret = await issueBudgetedKey(page);
  await waitForRoutePublication(page, secret, route);

  // Traffic sent before any price exists must be accounted as unpriced rather
  // than as free: cost totals never treat missing pricing as zero.
  const unpricedCall = await sendChatOnceBudgetIsLive(page, secret);
  expect(unpricedCall.usageReported).toBe(true);
  const beforePricing = await listGenerationRequests(page, 1);
  await expect(beforePricing.first()).toContainText('Unpriced');
  await openDetail(beforePricing.first());
  await expect(
    page
      .locator('.metric-card')
      .filter({ has: page.getByText('Estimated cost', { exact: true }) })
  ).toContainText(
    'Unpriced — no published price covered part of this request.'
  );

  // A price published now covers only the traffic that follows it.
  await page.goto('/settings');
  await expect(
    page.getByRole('heading', { name: 'Pricing revisions' })
  ).toBeVisible();
  await page.getByLabel('Provider kind').selectOption('openai_compatible');
  await page.getByLabel('Upstream model').fill(upstream.model);
  await page
    .getByLabel('Operation', { exact: true })
    .selectOption('generation');
  await page.getByLabel('Input / million', { exact: true }).fill(price.input);
  await page.getByLabel('Output / million', { exact: true }).fill(price.output);
  await page.getByRole('button', { name: 'Create pricing revision' }).click();
  await expect(
    page.getByText(
      'Pricing revision created. New usage will use the effective revision.'
    )
  ).toBeVisible();
  await expect(page.getByText('Revision 1', { exact: true })).toBeVisible();
  await page.screenshot({
    path: info.outputPath('pricing-revision.png'),
    fullPage: true
  });

  // Execution pins the same refreshed price evidence used by preview. Wait
  // for that evidence instead of sending traffic against the preceding snapshot.
  await expect
    .poll(
      async () =>
        page.evaluate(async (routeSlug) => {
          const session = await (
            await fetch('/api/v1/sessions/current')
          ).json();
          const response = await fetch('/api/v1/routing/simulate', {
            method: 'POST',
            headers: {
              'Content-Type': 'application/json',
              'X-CSRF-Token': session.csrf_token
            },
            body: JSON.stringify({
              operation: { kind: 'generation', request: { route: routeSlug } },
              surface: 'openai',
              mode: 'unary',
              seed: 'pricing-refresh'
            })
          });
          if (!response.ok)
            throw new Error(`Routing preview returned ${response.status}`);
          const preview = await response.json();
          return preview.some(
            (decision: {
              eligible: boolean;
              price?: { revision: number } | null;
            }) => decision.eligible && decision.price?.revision === 1
          );
        }, route),
      {
        timeout: 30_000,
        message:
          'routing should observe pricing revision 1 before requests pin it'
      }
    )
    .toBe(true);

  const unaryCall = await sendChat(page, secret, false);
  expect(unaryCall.status).toBe(200);
  expect(unaryCall.usageReported).toBe(true);
  const streamedCall = await sendChat(page, secret, true);
  expect(streamedCall.status).toBe(200);
  expect(streamedCall.contentType).toContain('text/event-stream');
  expect(streamedCall.usageReported).toBe(true);

  // Both priced requests cost the same two cents, unary and streamed alike.
  const rows = await listGenerationRequests(page, 3);
  await expect(rows.filter({ hasText: 'Unpriced' })).toHaveCount(1);
  const priced = rows.filter({ hasText: requestCost });
  await expect(priced).toHaveCount(2);
  await openDetail(priced.first());
  const facts = page
    .locator('section')
    .filter({ has: page.getByRole('heading', { name: route, exact: true }) });
  await expectFact(facts, 'Operation', 'generation');
  await expectFact(facts, 'Client surface', 'openai');
  await expectFact(facts, 'Input tokens', '7');
  await expectFact(facts, 'Output tokens', '5');
  await expectFact(facts, 'Usage completeness', 'Complete');
  await expect(
    page
      .locator('.metric-card')
      .filter({ has: page.getByText('Estimated cost', { exact: true }) })
      .locator('strong')
  ).toHaveText(requestCost);
  await expect(
    page.locator('details', { hasText: 'Exact amount' })
  ).toContainText(/0\.02\d* USD/);
  const timeline = page.locator('section').filter({
    has: page.getByRole('heading', { name: 'Attempt timeline', exact: true })
  });
  await expectFact(timeline, 'Response committed', 'Yes — failover stopped');
  const charge = page.locator('dl[aria-label="Attempt usage and pricing"]');
  await expectFact(charge, 'Charge status', 'Billable');
  await expectFact(charge, 'Usage observed', 'Yes');
  await expectFact(charge, 'Usage completeness', 'Complete');
  await page.screenshot({
    path: info.outputPath('request-accounting.png'),
    fullPage: true
  });

  // The usage report for this key: three requests, two of them priced, and the
  // completeness banner naming the unpriced one instead of implying zero cost.
  await page.goto('/api-keys');
  const row = page.getByRole('row').filter({ hasText: keyName });
  await expect(row).toHaveCount(1);
  await row.getByRole('link', { name: `Usage for ${keyName}` }).click();
  // The usage page normalises its own query string on arrival, so the key
  // filter the link carried survives in a longer, reordered URL.
  await expect(page).toHaveURL(/\/usage\?.*api_key_id=/);
  await page.getByLabel('Operation', { exact: true }).fill('generation');
  await page.getByRole('button', { name: 'Apply', exact: true }).click();
  await expect(usageMetric(page, 'Requests')).toHaveText('3');
  await expect(usageMetric(page, 'Input / output tokens')).toHaveText(
    '21 / 15'
  );
  await expect(usageMetric(page, 'Estimated cost')).toHaveText(accrued);
  const completeness = page.locator('section.completeness');
  await expect(completeness).toContainText('2 priced and 1 unpriced requests.');
  await expect(
    page.getByText(/Usage accounting and pricing are complete/)
  ).toHaveCount(0);

  // Budget accrual is written by the same asynchronous accounting path, so the
  // filtered budget line is polled rather than assumed to be current.
  const budget = page.getByRole('region', {
    name: 'Filtered API key budget'
  });
  await expect
    .poll(
      async () => {
        const shown = await budget.innerText().catch(() => '');
        if (shown.includes(accrued)) return shown;
        await page.reload();
        await budget.waitFor({ state: 'visible', timeout: 15_000 });
        return await budget.innerText().catch(() => '');
      },
      {
        message: 'priced traffic should accrue against the daily cost budget',
        timeout: 30_000,
        intervals: [500, 1_000, 2_000]
      }
    )
    .toContain(`${accrued} / $${dailyBudget}`);
  await expect(budget).toContainText('Window ends');
  await expect(budget).toContainText('Unpriced attempts this UTC month');
  await page.screenshot({
    path: info.outputPath('usage-budget.png'),
    fullPage: true
  });

  // The key's own policy view reports the same accrual without a currency.
  await page.goto('/api-keys');
  await page
    .getByRole('row')
    .filter({ hasText: keyName })
    .getByRole('button', { name: 'Edit', exact: true })
    .click();
  const keyBudget = page.getByRole('region', { name: 'Current spend budget' });
  await expect(keyBudget).toContainText('0.04 / 5.00');
  await expect(keyBudget).toContainText('Window ends (local time)');
});

test('retained media records expose metadata, filters, and accessible details', async ({
  page,
  request
}, info) => {
  await signIn(page);
  // Seed terminal history against the preceding journey's actual published
  // provider, key, and generation. Creation/reconciliation/content are covered
  // by the gateway service suites; every read here uses the real management API.
  const database = new URL(process.env.OLP_DATABASE_URL!);
  database.pathname =
    '/' +
    (process.env.OLP_CONSOLE_E2E_DATABASE_PREFIX ?? '') +
    (info.project.name === 'packaged' ? 'olp_packaged' : 'olp_vite');
  const succeeded = randomUUID();
  const failed = randomUUID();
  const seed = await promisify(execFile)('psql', [
    database.toString(),
    '-XAtq',
    '-v',
    'ON_ERROR_STOP=1',
    '-c',
    `
    WITH refs AS (
      SELECT p.id AS provider_id, p.active_revision_id AS revision_id,
        (SELECT id FROM olp.api_keys WHERE name='Accounting budget key' LIMIT 1) AS key_id,
        (SELECT id FROM olp.runtime_releases ORDER BY sequence DESC LIMIT 1) AS generation_id,
        (SELECT id FROM olp.provider_slots WHERE provider_id=p.id AND is_default) AS slot_id
      FROM olp.providers p WHERE p.name='Accounting upstream'
    )
    INSERT INTO olp.media_jobs(id,upstream_job_id,api_key_id,provider_id,provider_model,
      route_slug,operation,state,lifecycle_state,progress_percent,completed_at,deleted_at,
      etag,runtime_generation_id,provider_revision_id,slot_id,strict_contract)
    SELECT v.id::uuid,'terminal-fixture-'||v.id,key_id,provider_id,'archived-video-model',
      v.route,'video_create',v.state,'deleted',100,now(),now(),gen_random_uuid(),generation_id,revision_id,
      slot_id,false
    FROM refs CROSS JOIN (VALUES
      ('${succeeded}','retained-video-success','succeeded'),
      ('${failed}','retained-video-failure','failed')
    ) AS v(id,route,state) RETURNING id
  `
  ]);
  expect(seed.stdout.trim().split('\n').sort()).toEqual(
    [succeeded, failed].sort()
  );
  await page.goto('/media-jobs?route=retained-video-success');
  await expect(
    page.getByRole('heading', { name: 'Media Jobs', exact: true })
  ).toBeVisible();
  await expect(page.locator('.job-table tbody tr')).toHaveCount(1);
  await expect(page.locator('.job-table')).toContainText('succeeded');
  await page.getByRole('link', { name: 'View', exact: true }).click();
  await expect(
    page.getByRole('heading', { name: 'Media job detail' })
  ).toBeVisible();
  await expect(page.locator('.job-detail')).toContainText(succeeded);
  await expect(page.locator('.job-detail')).toContainText('Deleted');
  await expect(page.locator('.job-detail')).toContainText('Not available');
  await expect(
    page.getByRole('heading', { name: 'Recorded media lifecycle' })
  ).toBeVisible();
  await expect(page.getByText('Terminal state recorded')).toBeVisible();
  await expect(
    page.locator('.job-detail audio, .job-detail video, .job-detail img')
  ).toHaveCount(0);
  const detail = await page.evaluate(async (id) => {
    const response = await fetch(`/api/v1/media-jobs/${id}`);
    if (!response.ok)
      throw new Error(`Media metadata failed: ${response.status}`);
    return response.json();
  }, succeeded);
  expect(detail.id).toBe(succeeded);
  for (const field of ['prompt', 'content', 'credential', 'raw_response'])
    expect(detail).not.toHaveProperty(field);
  expect(JSON.stringify(detail)).not.toContain(upstream.credential);
  expect((await request.get(`/api/v1/media-jobs/${succeeded}`)).status()).toBe(
    401
  );
  for (const width of [320, 1440]) {
    await page.setViewportSize({ width, height: 900 });
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth - innerWidth
      )
    ).toBeLessThanOrEqual(0);
    expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
    await page.screenshot({
      path: info.outputPath(`retained-media-${width}.png`),
      fullPage: true
    });
  }
  await page.getByRole('link', { name: 'All media jobs' }).click();
  await expect(page.getByLabel('Route', { exact: true })).toHaveValue(
    'retained-video-success'
  );
  await page.getByLabel('Route', { exact: true }).fill('');
  await page.getByRole('combobox', { name: /^State/ }).selectOption('failed');
  await page.getByRole('button', { name: 'Apply filters' }).click();
  await expect(page.locator('.job-table tbody tr')).toHaveCount(1);
  await expect(page.locator('.job-table')).toContainText(
    'retained-video-failure'
  );
  await page.reload();
  await expect(page.getByRole('combobox', { name: /^State/ })).toHaveValue(
    'failed'
  );
  await expect(page.locator('.job-table')).toContainText(
    'retained-video-failure'
  );
});
