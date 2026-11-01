import AxeBuilder from '@axe-core/playwright';
import { manage, provisionGenerationRoute } from '../helpers/management';
import { signInGatewayOwner as signIn } from '../gateway/signIn';
import type { ProviderModel } from '../../src/lib/features/providers/api/models';
import { expect, test, type APIRequestContext, type Page } from '../playwright';
import { takeSecret, waitForRoutePublication } from '../journeys/fixtures';
import { verifyDraftSave } from '../journeys/draft-saving';
import type { RoutingDecision } from '../../src/lib/features/routes/api';
import { vertical } from '../journeys/fixtures';
import { verifyProviderRouting } from '../journeys/provider-routing';
import type { BillingImport } from '../../src/lib/features/providers/api/billing';

const upstream = {
  origin: 'http://127.0.0.1:4187',
  endpoint: 'http://127.0.0.1:4187/v1',
  model: 'compatible-e2e-model',
  credential: 'compatible-provider-secret',
  reply: 'Hello from the compatible upstream'
};
const route = 'compatible-chat';
const keyName = 'Compatible route key';

test('cost calculator distinguishes providers and credential slots', async ({
  page
}, info) => {
  await signIn(page);
  const slug = `calculator-${crypto.randomUUID()}`;
  const firstName = `Calculator first ${crypto.randomUUID()}`;
  const secondName = `Calculator second ${crypto.randomUUID()}`;
  const { provider } = await provisionGenerationRoute(page, {
    providerName: firstName,
    route: slug,
    endpoint: upstream.endpoint,
    model: upstream.model,
    credential: upstream.credential
  });
  const second = await manage<{ id: string }>(
    page,
    'POST',
    '/api/v1/providers',
    {
      idempotency: crypto.randomUUID(),
      body: {
        name: secondName,
        configuration: {
          kind: 'openai_compatible',
          endpoint: upstream.endpoint,
          auth_mode: 'api_key'
        },
        credential: upstream.credential
      }
    }
  );
  expect(second.status, second.source).toBe(201);
  const firstTarget = crypto.randomUUID();
  const decisions: RoutingDecision[] = [
    {
      target_id: firstTarget,
      provider_id: provider.id,
      upstream_model: upstream.model,
      credential_slot_id: crypto.randomUUID(),
      eligible: true,
      priority: 0,
      strategy: 'weighted',
      estimated_cost: { minimum: '0.10', maximum: '0.20', currency: 'USD' }
    },
    {
      target_id: firstTarget,
      provider_id: provider.id,
      upstream_model: upstream.model,
      credential_slot_id: crypto.randomUUID(),
      eligible: false,
      reason: 'Credential unavailable',
      priority: 0,
      strategy: 'weighted',
      estimated_cost: { minimum: '0.30', maximum: '0.40', currency: 'USD' }
    },
    {
      target_id: crypto.randomUUID(),
      provider_id: second.body.id,
      upstream_model: upstream.model,
      credential_slot_id: null,
      eligible: true,
      priority: 1,
      strategy: 'weighted',
      estimated_cost: null
    }
  ];
  await page.route('**/api/v1/routing/simulate', (request) =>
    request.fulfill({
      json: decisions,
      headers: { 'Cache-Control': 'no-store' }
    })
  );
  await page.goto('/usage');
  const calculator = page.getByRole('region', { name: 'Cost calculator' });
  await calculator.getByRole('tab', { name: 'Route', exact: true }).click();
  await calculator.getByLabel('Route', { exact: true }).selectOption(slug);
  await calculator
    .getByRole('button', { name: 'Simulate', exact: true })
    .click();
  const table = calculator.getByRole('table');
  await expect(
    calculator.getByRole('button', { name: 'Simulate', exact: true })
  ).toBeEnabled();
  await page.screenshot({
    path: info.outputPath('calculator-decisions.png'),
    fullPage: true
  });
  await expect(table.getByRole('row')).toHaveCount(decisions.length + 1);
  for (const [index, decision] of decisions.entries()) {
    const row = table.getByRole('row').nth(index + 1);
    await expect(row.getByRole('cell').nth(0)).toContainText(
      decision.provider_id === provider.id ? firstName : secondName
    );
    await expect(row.getByRole('cell').nth(0)).toContainText(upstream.model);
    await expect(row.getByRole('cell').nth(0)).toContainText(
      decision.credential_slot_id ?? 'Connection authentication'
    );
    await expect(row.getByRole('cell').nth(1)).toContainText(
      decision.eligible ? 'Yes' : (decision.reason ?? 'No')
    );
    if (decision.reason) await expect(row).toContainText(decision.reason);
    for (const [column, value] of [
      decision.estimated_cost?.minimum,
      decision.estimated_cost?.maximum
    ].entries()) {
      await expect(row.getByRole('cell').nth(column + 2)).toHaveText(
        value ? `${value} ${decision.estimated_cost?.currency}` : '—'
      );
    }
  }
  await calculator.screenshot({
    path: info.outputPath('calculator-results.png')
  });
});

async function gatewayStatus(page: Page, secret: string): Promise<number> {
  return page.evaluate(
    async ({ apiKey, slug }) => {
      const response = await fetch('/v1/chat/completions', {
        method: 'POST',
        headers: {
          Authorization: `Bearer ${apiKey}`,
          'Content-Type': 'application/json'
        },
        body: JSON.stringify({
          model: slug,
          messages: [{ role: 'user', content: 'Check route readiness.' }]
        })
      });
      await response.arrayBuffer();
      return response.status;
    },
    { apiKey: secret, slug: route }
  );
}

/// Streams one chat completion through the gateway and returns the assembled
/// text plus whether the stream terminated with [DONE].
function streamChat(page: Page, secret: string) {
  return page.evaluate(
    async ({ apiKey, slug }) => {
      const response = await fetch('/v1/chat/completions', {
        method: 'POST',
        headers: {
          Authorization: `Bearer ${apiKey}`,
          'Content-Type': 'application/json'
        },
        body: JSON.stringify({
          model: slug,
          stream: true,
          messages: [{ role: 'user', content: 'Stream a greeting.' }]
        })
      });
      const raw = await response.text();
      let text = '';
      let done = false;
      for (const line of raw.split('\n')) {
        if (!line.startsWith('data: ')) continue;
        const data = line.slice(6);
        if (data === '[DONE]') {
          done = true;
          continue;
        }
        const chunk = JSON.parse(data) as {
          choices?: Array<{ delta?: { content?: string } }>;
        };
        text += chunk.choices?.[0]?.delta?.content ?? '';
      }
      return {
        status: response.status,
        contentType: response.headers.get('content-type') ?? '',
        text,
        done
      };
    },
    { apiKey: secret, slug: route }
  );
}

async function resetUpstream(request: APIRequestContext, delayMs = 25) {
  expect(
    (await request.post(`${upstream.origin}/__test__/reset`)).status()
  ).toBe(204);
  expect(
    (
      await request.post(`${upstream.origin}/__test__/delay`, {
        data: { ms: delayMs }
      })
    ).status()
  ).toBe(204);
}

async function upstreamRequests(request: APIRequestContext) {
  const response = await request.get(`${upstream.origin}/__test__/requests`);
  expect(response.ok()).toBe(true);
  return (await response.json()) as {
    requests: Array<{
      path: string;
      headers: Record<string, string>;
      body: unknown;
    }>;
    unexpected: string[];
  };
}

async function assertUpstreamCredentials(
  request: APIRequestContext,
  secret: string
) {
  const observed = await upstreamRequests(request);
  expect(observed.unexpected).toEqual([]);
  expect(observed.requests.length).toBeGreaterThan(0);
  for (const call of observed.requests) {
    expect(call.headers.authorization).toBe(`Bearer ${upstream.credential}`);
  }
  expect(JSON.stringify(observed.requests)).not.toContain(secret);
}

test('a browser user configures an OpenAI-compatible route and reaches unary and streaming inference', async ({
  page,
  request
}, info) => {
  test.setTimeout(180_000);
  await signIn(page);
  await resetUpstream(request);

  // Provider wizard: connect, discover, certify, activate.
  await page.goto('/providers/new');
  await expect(
    page.getByRole('heading', { name: 'Connect an upstream provider.' })
  ).toBeVisible();
  await page.getByRole('radio', { name: /OpenAI-compatible/ }).check();
  await page.getByLabel('Provider name').fill('Compatible upstream');
  await page
    .getByLabel('Authentication', { exact: true })
    .selectOption('api_key');
  await page
    .getByRole('textbox', { name: 'Endpoint', exact: true })
    .fill(upstream.endpoint);
  await page.getByLabel('Seed model (optional)').fill(upstream.model);
  await page
    .getByLabel('Credential', { exact: true })
    .fill(upstream.credential);
  const created = page.waitForResponse(
    (response) =>
      response.request().method() === 'POST' &&
      new URL(response.url()).pathname === '/api/v1/providers'
  );
  await page.getByRole('button', { name: /Save and test connection/ }).click();
  const providerId = ((await (await created).json()) as { id: string }).id;
  await expect(page.getByText(upstream.credential)).toHaveCount(0);
  await expect(
    page.getByRole('heading', { name: 'Discover upstream models' })
  ).toBeVisible();
  await page.getByRole('button', { name: 'Discover upstream models' }).click();
  await expect(
    page.getByRole('heading', { name: 'Review model capabilities' })
  ).toBeVisible();
  await expect(
    page.getByText(upstream.model, { exact: true }).first()
  ).toBeVisible();
  // The seed model arrives with both supported tuples declared, so the review
  // confirms them; auxiliary operations remain available for an explicit review.
  await expect(page.getByLabel(/Operation \d+/)).toHaveCount(2);
  await expect(
    page.getByRole('button', { name: 'Add capability' })
  ).toBeEnabled();
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
  await expect(page.getByText('2/2 certified', { exact: true })).toBeVisible();
  await page.evaluate(() => window.scrollTo(0, 0));
  await page.screenshot({
    path: info.outputPath('provider-onboarding.png'),
    fullPage: true
  });
  await page.getByRole('button', { name: 'Continue to activation' }).click();
  await page.getByRole('button', { name: 'Test completed draft' }).click();
  await expect(page.getByText(/Final draft test passed/)).toBeVisible();
  await page.getByRole('button', { name: 'Activate provider' }).click();
  await expect(
    page.getByRole('heading', { name: 'Now build a stable route slug.' })
  ).toBeVisible();

  // Route draft: stale edits stay local, inactive providers block activation.
  await page.getByRole('link', { name: 'Build default route' }).click();
  await expect(
    page.getByRole('heading', { name: 'Build a route draft.' })
  ).toBeVisible();
  await page.getByLabel('Public model slug').fill(route);
  await page.getByRole('button', { name: 'Add target' }).click();
  await expect(
    page.getByLabel('Provider model').first().locator('option:checked')
  ).toContainText(upstream.model);
  await page.getByLabel('Maximum attempts').fill('2');
  await page.getByLabel('Maximum request body (bytes)').fill('65536');
  // This provider has no profile, so the route is declared transformed.
  await page.getByLabel('Fidelity mode').selectOption('transformed');
  await page.getByRole('button', { name: 'Create draft' }).click();
  await expect(page).toHaveURL(/\/routes\/[0-9a-f-]+$/);
  await verifyDraftSave(page, 'route', route);
  await expect(page.getByLabel('Maximum request body (bytes)')).toHaveValue(
    '65536'
  );
  await page
    .locator('fieldset')
    .filter({ has: page.getByLabel('Maximum request body (bytes)') })
    .screenshot({ path: info.outputPath('route-body-limit.png') });
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);

  const providerPath = `/api/v1/providers/${providerId}`;
  const disabled = await manage(page, 'POST', `${providerPath}/disable`, {
    match: providerPath,
    idempotency: `disable-${Date.now()}`
  });
  expect(disabled.status).toBe(200);
  await page
    .getByRole('button', { name: 'Validate draft', exact: true })
    .click();
  await expect(
    page.getByText(/belongs to a connection that is not active\./)
  ).toBeVisible();
  await page.screenshot({
    path: info.outputPath('route-activation-blocked.png'),
    fullPage: true
  });
  const reactivated = await manage(page, 'POST', `${providerPath}/activate`, {
    match: providerPath,
    idempotency: `activate-${Date.now()}`
  });
  expect(reactivated.status).toBe(200);
  await page.getByLabel('Dry-run operation').selectOption('generation');
  await page.getByLabel('Client surface').selectOption('openai');
  await page.getByLabel('Transport mode').selectOption('streaming');
  await page.getByRole('button', { name: 'Simulate order' }).click();
  await expect(
    page.getByRole('heading', { name: 'Attempt explanation' })
  ).toBeVisible();
  await page
    .getByRole('button', { name: 'Validate draft', exact: true })
    .click();
  await expect(page.getByText('Validation passed.')).toBeVisible();
  await page
    .getByRole('button', { name: 'Activate route', exact: true })
    .click();
  await expect(page.getByText('Revision 1 active')).toBeVisible();
  await page.screenshot({
    path: info.outputPath('route-active.png'),
    fullPage: true
  });

  // Key issuance with an explicit route allowlist, then real SDK-shaped traffic.
  await page.goto('/api-keys/new');
  await expect(
    page.getByRole('heading', { name: 'Create a proxy key.' })
  ).toBeVisible();
  await page.getByLabel('Key name').fill(keyName);
  await expect(
    page.getByRole('checkbox', { name: 'Inference requests' })
  ).toBeChecked();
  await page.getByRole('checkbox', { name: 'Model listing' }).check();
  await page
    .getByRole('group', { name: 'Allowed route slugs' })
    .getByRole('checkbox', { name: route })
    .check();
  await page.getByRole('button', { name: /Create and show key/ }).click();
  const secretDialog = page.getByRole('dialog', {
    name: 'Copy this secret now.'
  });
  await expect(secretDialog).toBeVisible();
  const secret = await takeSecret(secretDialog);
  expect(secret).toMatch(/^olp_/);
  await waitForRoutePublication(page, secret, route);
  await resetUpstream(request);
  await secretDialog.getByRole('tab', { name: 'OpenAI Python' }).click();
  const connectionTest = page.waitForResponse(
    (response) =>
      response.request().method() === 'POST' &&
      new URL(response.url()).pathname === '/v1/responses'
  );
  await secretDialog
    .getByRole('button', { name: 'Run connection test' })
    .click();
  expect((await connectionTest).status()).toBe(200);
  await expect(
    secretDialog.getByText(`OpenAI request succeeded through route ${route}.`)
  ).toBeVisible();
  await secretDialog.screenshot({
    path: info.outputPath('key-connection-test.png')
  });
  await secretDialog
    .getByRole('button', { name: 'I have saved the key' })
    .click();
  await expect(secretDialog).toBeHidden();

  await assertUpstreamCredentials(request, secret);

  // A stream admitted before a publication change keeps its pinned snapshot.
  await resetUpstream(request);
  expect(
    (await request.post(`${upstream.origin}/__test__/hold`)).status()
  ).toBe(204);
  const streaming = streamChat(page, secret);
  try {
    await expect
      .poll(async () => (await upstreamRequests(request)).requests.length, {
        timeout: 10_000
      })
      .toBeGreaterThan(0);
    expect(
      (
        await manage(page, 'POST', `${providerPath}/disable`, {
          match: providerPath,
          idempotency: `disable-inflight-${Date.now()}`
        })
      ).status
    ).toBe(200);
    // Wait for the gateway to install the disabled release, not just for
    // the management transaction to publish it.
    await expect
      .poll(() => gatewayStatus(page, secret), { timeout: 15_000 })
      .toBe(503);
  } finally {
    expect(
      (await request.post(`${upstream.origin}/__test__/resume`)).status()
    ).toBe(204);
    await streaming.catch(() => undefined);
  }
  const streamed = await streaming;
  expect(streamed.status).toBe(200);
  expect(streamed.contentType).toContain('text/event-stream');
  expect(streamed.text).toBe(upstream.reply);
  expect(streamed.done).toBe(true);
  expect(
    (
      await manage(page, 'POST', `${providerPath}/activate`, {
        match: providerPath,
        idempotency: `activate-inflight-${Date.now()}`
      })
    ).status
  ).toBe(200);
  await expect
    .poll(() => gatewayStatus(page, secret), { timeout: 15_000 })
    .toBe(200);

  await assertUpstreamCredentials(request, secret);

  // The console playground uses the same runtime through the session.
  await resetUpstream(request);
  await page.goto('/playground');
  await page.getByLabel('Route slug').fill(route);
  await page.getByLabel('Prompt').fill('Say hello.');
  await page.getByRole('button', { name: 'Run test' }).click();
  await expect(page.locator('.output pre').first()).toHaveText(upstream.reply);
  await page.screenshot({
    path: info.outputPath('playground.png'),
    fullPage: true
  });

  // Only the configured provider credential ever reached the upstream.
  await assertUpstreamCredentials(request, secret);

  // Revocation is authority state: the key stops working within the poll.
  await page.goto('/api-keys');
  const row = page.getByRole('row').filter({ hasText: keyName });
  await expect(row).toHaveCount(1);
  page.once('dialog', (dialog) => dialog.accept());
  await row.getByRole('button', { name: 'Revoke' }).click();
  await expect(row.getByText('revoked', { exact: true })).toBeVisible();
  await expect
    .poll(() => gatewayStatus(page, secret), {
      timeout: 15_000,
      intervals: [250, 500, 1_000]
    })
    .toBe(401);

  // A capability lookup failure is not evidence that history is disabled.
  const historyRequests: string[] = [];
  page.on('request', (outgoing) => {
    const path = new URL(outgoing.url()).pathname;
    if (path.startsWith('/api/v1/requests')) historyRequests.push(path);
  });
  const capabilitiesPattern = '**/api/v1/auth/capabilities';
  await page.route(capabilitiesPattern, (intercept) =>
    intercept.fulfill({
      status: 503,
      contentType: 'application/problem+json',
      headers: { 'cache-control': 'no-store' },
      body: JSON.stringify({ title: 'Unavailable', status: 503 })
    })
  );
  try {
    await page.goto('/requests');
    await expect(
      page.getByText('Request history capabilities are unavailable.')
    ).toBeVisible({ timeout: 15_000 });
    expect(historyRequests).toEqual([]);
  } finally {
    await page.unroute(capabilitiesPattern);
  }
  // Once the capability answer arrives, retention is enforced here, so the
  // explorer queries retained metadata instead of disclaiming it.
  const listed = page.waitForResponse(
    (response) =>
      response.request().method() === 'GET' &&
      new URL(response.url()).pathname === '/api/v1/requests'
  );
  await page.getByRole('button', { name: 'Try again', exact: true }).click();
  expect((await listed).status()).toBe(200);
  await expect(
    page.getByRole('button', { name: 'Apply filters' })
  ).toBeVisible();
  await expect(
    page.getByText(/Request history is not retained by this installation/)
  ).toHaveCount(0);
});

test('billing import preserves a cleared scope', async ({ page }, info) => {
  await signIn(page);
  const provider = await manage<{ id: string }>(
    page,
    'POST',
    '/api/v1/providers',
    {
      idempotency: crypto.randomUUID(),
      body: {
        name: `Billing scope fixture ${crypto.randomUUID()}`,
        configuration: {
          kind: 'openai',
          endpoint: upstream.endpoint,
          auth_mode: 'api_key'
        },
        credential: upstream.credential
      }
    }
  );
  expect(provider.status, provider.source).toBe(201);
  const providerPath = `/api/v1/providers/${provider.body.id}`;
  const importPath = `${providerPath}/billing-import`;
  const storedScope = { project_ids: ['proj_fixture'] };
  const created = await manage<BillingImport>(page, 'PUT', importPath, {
    match: providerPath,
    idempotency: crypto.randomUUID(),
    body: {
      enabled: false,
      scope: storedScope,
      credential: 'billing-admin-fixture-credential'
    }
  });
  expect(created.status, created.source).toBe(201);
  await page.goto(`/providers/${provider.body.id}`);
  const scope = page.locator('#billing-scope');
  await expect(scope).toHaveValue(storedScope.project_ids.join('\n'));
  await expect(page.locator('#billing-admin-key')).toHaveValue('');
  await scope.fill('');
  await scope.blur();
  const save = page.getByRole('button', { name: 'Save import', exact: true });
  await expect(save).toBeEnabled();
  await scope.screenshot({
    path: info.outputPath('billing-scope-cleared.png')
  });
  await expect(scope).toHaveValue('');
  const saved = page.waitForResponse(
    (response) =>
      response.request().method() === 'PUT' &&
      new URL(response.url()).pathname === importPath
  );
  await save.click();
  const response = await saved;
  expect(response.status(), await response.text()).toBe(200);
  const document = await manage<BillingImport>(page, 'GET', importPath);
  expect(document.body).toMatchObject({
    scope: {},
    enabled: false,
    credential_configured: true
  });
  await page.reload();
  await expect(
    page.getByRole('button', { name: 'Save import', exact: true })
  ).toBeVisible();
  await expect(scope).toHaveValue('');
  await page
    .locator('#billing-admin-key')
    .fill('replacement-billing-credential');
  await page.locator('#billing-admin-key').fill('');
  await expect(scope).toHaveValue('');
  await page.screenshot({
    path: info.outputPath('billing-scope-saved.png'),
    fullPage: true
  });
  const narrowed = await manage<BillingImport>(page, 'PUT', importPath, {
    match: importPath,
    idempotency: crypto.randomUUID(),
    body: { enabled: true, scope: { project_ids: ['proj_other'] } }
  });
  expect(narrowed.status, narrowed.source).toBe(200);
  await page.locator('#billing-admin-key').fill('unsaved-replacement-key');
  const conflicted = page.waitForResponse(
    (response) =>
      response.request().method() === 'PUT' &&
      new URL(response.url()).pathname === importPath
  );
  await save.click();
  expect((await conflicted).status()).toBe(412);
  const section = page.locator('.billing-import');
  await expect(
    section.getByText('This item changed elsewhere.', { exact: true })
  ).toBeVisible();
  const failureRoute = `**${importPath}`;
  await page.route(failureRoute, async (route) => {
    await route.fulfill({
      status: 503,
      json: { title: 'Reload unavailable', status: 503 },
      headers: { 'Cache-Control': 'no-store' }
    });
  });
  await section.getByRole('button', { name: 'Reload', exact: true }).click();
  await expect(
    section.getByText('Reload unavailable', { exact: true })
  ).toBeVisible();
  await expect(scope).toHaveValue('');
  await expect(page.locator('#billing-admin-key')).toHaveValue(
    'unsaved-replacement-key'
  );
  await expect(
    section.getByRole('checkbox', { name: 'Enabled', exact: true })
  ).not.toBeChecked();
  await page.unroute(failureRoute);
  await section.getByRole('button', { name: 'Reload', exact: true }).click();
  await expect(scope).toHaveValue('proj_other');
  await expect(
    section.getByRole('checkbox', { name: 'Enabled', exact: true })
  ).toBeChecked();
  await expect(page.locator('#billing-admin-key')).toHaveValue('');
  await expect(
    section.getByText('This item changed elsewhere.', { exact: true })
  ).toHaveCount(0);
  const resaved = page.waitForResponse(
    (response) =>
      response.request().method() === 'PUT' &&
      new URL(response.url()).pathname === importPath
  );
  await save.click();
  const resavedResponse = await resaved;
  expect(resavedResponse.status(), await resavedResponse.text()).toBe(200);
  expect(resavedResponse.request().postDataJSON()).toEqual({
    enabled: true,
    scope: { project_ids: ['proj_other'] }
  });
  const reloadedDocument = await manage<BillingImport>(page, 'GET', importPath);
  expect(reloadedDocument.body).toMatchObject({
    enabled: true,
    scope: { project_ids: ['proj_other'] },
    credential_configured: true
  });
});

test('cloud connections support bulk model review, credential pools and policy routing', async ({
  page
}, info) => {
  test.setTimeout(240_000);
  await signIn(page);
  await page.goto('/providers/new');
  await page.getByRole('radio', { name: /Azure OpenAI/ }).check();
  await page.getByLabel('Provider name').fill('Concurrent provider name');
  await page.getByLabel('Seed model (optional)').fill(vertical.deployment);
  await page.getByLabel('Azure resource endpoint').fill(vertical.endpoint);
  await page.getByLabel('API version').fill(vertical.apiVersion);
  await page.getByLabel('Cloud deployment').fill(vertical.deployment);
  await page
    .getByLabel('Credential', { exact: true })
    .fill(vertical.credential);
  await page.getByRole('button', { name: /Save and test connection/ }).click();
  const bulk = page.locator('section').filter({
    has: page.getByRole('heading', { name: 'Validate models in bulk' })
  });
  await bulk.getByRole('checkbox').check();
  await bulk.getByLabel('Capabilities to validate').selectOption('generation');
  await bulk
    .getByRole('button', { name: 'Validate 1 selected models' })
    .click();
  await expect(bulk.getByText(/Checked 1 models/)).toBeVisible();
  await page.getByRole('button', { name: 'Continue to activation' }).click();
  await page.getByRole('button', { name: 'Test completed draft' }).click();
  await expect(page.getByText(/Final draft test passed/)).toBeVisible();
  await page.getByRole('button', { name: 'Activate provider' }).click();
  await expect(
    page.getByRole('heading', { name: 'Now build a stable route slug.' })
  ).toBeVisible();
  await verifyProviderRouting(page, info);
});

test('manual model identifiers remain disabled until capability review', async ({
  page
}, info) => {
  await signIn(page);
  const created = await manage<{ id: string }>(
    page,
    'POST',
    '/api/v1/providers',
    {
      idempotency: crypto.randomUUID(),
      body: {
        name: `Manual model fixture ${crypto.randomUUID()}`,
        configuration: {
          kind: 'openai_compatible',
          endpoint: upstream.endpoint,
          auth_mode: 'api_key'
        },
        credential: upstream.credential
      }
    }
  );
  expect(created.status, created.source).toBe(201);
  const path = `/api/v1/providers/${created.body.id}`;
  await page.goto(`/providers/${created.body.id}`);
  const discovery = page.waitForResponse(
    (response) =>
      response.request().method() === 'POST' &&
      new URL(response.url()).pathname === `${path}/discovery`
  );
  await page.getByRole('button', { name: 'Run upstream discovery' }).click();
  expect((await discovery).status()).toBe(200);
  await expect(
    page.getByText(upstream.model, { exact: true }).first()
  ).toBeVisible();
  for (const names of [['manual-one'], ['manual-two', 'manual-three']]) {
    await page.getByText('Manual model identifiers', { exact: true }).click();
    await page.getByLabel('Upstream model identifiers').fill(names.join('\n'));
    const declared = page.waitForResponse(
      (response) =>
        response.request().method() === 'POST' &&
        new URL(response.url()).pathname === `${path}/discovery`
    );
    const submit = page.getByRole('button', {
      name: 'Add identifiers for review'
    });
    await submit.click();
    const response = await declared;
    await expect(
      page.getByRole('button', { name: 'Run upstream discovery' })
    ).toBeEnabled();
    await page.screenshot({
      path: info.outputPath(`manual-models-${names.length}.png`),
      fullPage: true
    });
    expect(response.status(), await response.text()).toBe(200);
    const inventory = await manage<{ items: ProviderModel[] }>(
      page,
      'GET',
      `${path}/models`
    );
    expect(inventory.status).toBe(200);
    for (const name of names) {
      expect(
        inventory.body.items.find((model) => model.upstream_model === name)
      ).toMatchObject({
        display_name: name,
        enabled: false,
        capabilities: []
      });
      await expect(page.getByText(name, { exact: true }).first()).toBeVisible();
    }
    await expect(page.getByLabel('Upstream model identifiers')).toHaveValue('');
  }
});
